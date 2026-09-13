package quotafollow

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"log"
	"math/rand/v2"
	"sub2api-enhance/internal/sub2api"
	"sync"
	"time"
)

// 各独立调度使用不同的增强专用锁，不占用提示词审计任务槽位。
const (
	detectLock    int64 = 748239516309712
	deliveryLock  int64 = 748239516309713
	collectorLock int64 = 748239516309714
)

// collectorInterval 只用于原版记录的后台增量采集，与账号检测业务间隔分离。
const collectorInterval = time.Minute

type QuotaAPI interface {
	Configured() bool
	AccountUsageBatch(context.Context, []int64) (map[int64]sub2api.AccountUsage, error)
	ResetQuota(context.Context, int64, string, string) sub2api.QuotaResetReply
}
type QuotaSource interface {
	Accounts(context.Context, int64) ([]sub2api.QuotaAccount, error)
	Discover(context.Context, int64) (sub2api.QuotaDiscovery, error)
	Snapshot(context.Context, int64, int64) (*sub2api.QuotaSnapshot, error)
}
type CacheReader interface {
	Read(context.Context, int64) (map[string]string, error)
	Status() string
}
type Service struct {
	lockDB          *sql.DB
	repo            *Repository
	source          QuotaSource
	api             QuotaAPI
	cache           CacheReader
	location        *time.Location
	flusherDisabled bool
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	mu              sync.Mutex
}

func NewService(repo *Repository, source *sub2api.QuotaData, api *sub2api.Client, cache CacheReader, location *time.Location, lockDB *sql.DB, flusherState string) *Service {
	return &Service{lockDB: lockDB, repo: repo, source: source, api: api, cache: cache, location: location, flusherDisabled: flusherState == "false"}
}
func (s *Service) Start(parent context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	for _, task := range []struct {
		lock     int64
		interval time.Duration
		run      func(context.Context) error
	}{{detectLock, time.Second, s.detect}, {deliveryLock, time.Second, s.deliver}, {collectorLock, collectorInterval, s.collect}} {
		s.wg.Add(1)
		go func(lock int64, interval time.Duration, run func(context.Context) error) {
			defer s.wg.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				if err := s.locked(ctx, lock, run); err != nil && ctx.Err() == nil {
					log.Printf("额度跟随后台处理失败 task=%d error=%v", lock, err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}(task.lock, task.interval, task.run)
	}
}
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// locked 将单个调度归属绑定到独占数据库连接，归还连接前可靠释放会话锁。
func (s *Service) locked(ctx context.Context, key int64, run func(context.Context) error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	conn, err := s.lockDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var acquired bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var released bool
		err := conn.QueryRowContext(cleanup, `SELECT pg_advisory_unlock($1)`, key).Scan(&released)
		if err != nil || !released {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	return run(ctx)
}
func nextDetection(now time.Time, cfg Config) time.Time {
	minimum := time.Duration(cfg.MinInterval) * time.Minute
	span := time.Duration(cfg.MaxInterval-cfg.MinInterval) * time.Minute
	return now.Add(minimum + time.Duration(rand.Int64N(int64(span)+1)))
}
func (s *Service) detect(ctx context.Context) error {
	cfg, err := s.repo.Config(ctx)
	if err != nil {
		return err
	}
	if !cfg.Enabled || cfg.GroupID == nil || (!cfg.ResetDaily && !cfg.ResetWeekly) {
		return nil
	}
	if err := cfg.Config.Validate(); err != nil {
		return err
	}
	runtime, err := s.repo.Runtime(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if (runtime.NextCheckAt != nil && runtime.NextCheckAt.After(now)) || (runtime.PausedRevision != nil && *runtime.PausedRevision == cfg.Revision) {
		return nil
	}
	next := nextDetection(now, cfg.Config)
	fail := func(err error) error {
		var paused *int64
		var apiErr *sub2api.APIError
		if errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 403) {
			paused = &cfg.Revision
		}
		if saveErr := s.repo.Schedule(ctx, next, err.Error(), paused); saveErr != nil {
			return saveErr
		}
		return err
	}
	if !s.api.Configured() {
		return fail(errors.New("原版管理员 API Key 未配置"))
	}
	log.Printf("开始检测 OpenAI 跟随窗口 group_id=%d epoch=%s", *cfg.GroupID, cfg.Epoch)
	discovery, err := s.source.Discover(ctx, *cfg.GroupID)
	if err != nil {
		return fail(err)
	}
	hash := accountSetHash(discovery.Accounts)
	changed := runtime.Epoch != cfg.Epoch || runtime.AccountSetHash != hash
	previous := map[int64]AccountState{}
	lastEvent := runtime.LastEventAt
	if changed {
		lastEvent = nil
	} else {
		for _, state := range runtime.States {
			previous[state.Account.ID] = state
		}
	}
	ids := make([]int64, 0, len(discovery.Accounts))
	for _, a := range discovery.Accounts {
		ids = append(ids, a.ID)
	}
	usages := map[int64]sub2api.AccountUsage{}
	if len(ids) > 0 {
		usages, err = s.api.AccountUsageBatch(ctx, ids)
		if err != nil {
			return fail(err)
		}
	}
	enabledAt := now
	if cfg.EnabledAt != nil {
		enabledAt = *cfg.EnabledAt
	}
	accountResetSignals := map[int64]time.Time{}
	if !changed && runtime.LastCheckedAt != nil {
		// 只消费上次成功检测之后的新账号审计，首次基线不回放历史记录。
		accountResetSignals, err = s.repo.AccountResetSignals(ctx, ids, *runtime.LastCheckedAt)
		if err != nil {
			return fail(err)
		}
	}
	states := make([]AccountState, 0, len(ids))
	for _, account := range discovery.Accounts {
		usage, ok := usages[account.ID]
		if !ok {
			usage.Error = "批量接口遗漏该账号"
		}
		old := previous[account.ID]
		if old.CandidateResetAt != nil && lastEvent != nil && !old.CandidateResetAt.After(*lastEvent) {
			old.CandidateResetAt = nil
		}
		state := observe(old, account, usage, now, enabledAt, lastEvent)
		if signal, ok := accountResetSignals[account.ID]; ok {
			state = applyAccountResetSignal(state, signal, enabledAt, lastEvent)
		}
		states = append(states, state)
	}
	boundary, consensusErr := consensus(states)
	message := ""
	if consensusErr != nil {
		message = consensusErr.Error()
	}
	if changed {
		boundary = nil
	}
	if err := s.repo.SaveObservation(ctx, cfg, discovery, hash, states, boundary, next, message, now, runtime.LastCheckedAt); err != nil {
		return err
	}
	transition, _ := json.Marshal(map[string]any{"before_accounts": runtime.Accounts, "after_accounts": discovery.Accounts, "before_states": runtime.States, "after_states": states})
	result := "等待账号一致重置"
	rebased := false
	for _, state := range states {
		if state.BaselineRebased {
			rebased = true
			break
		}
	}
	if changed {
		result = "已建立新账号基线"
	} else if rebased {
		result = "账号重置边界已调整，正在重新建立基线"
	} else if boundary != nil {
		result = "已确认账号一致重置"
	}
	if message != "" {
		result = message
	}
	log.Printf("OpenAI 跟随窗口检测结束：%s group_id=%d reset_at=%v transition=%s", result, *cfg.GroupID, boundary, transition)
	return nil
}
func (s *Service) Runtime(ctx context.Context) (Runtime, error) {
	r, err := s.repo.Runtime(ctx)
	if err != nil {
		return r, err
	}
	cfg, err := s.repo.Config(ctx)
	if err != nil {
		return r, err
	}
	if !cfg.Enabled || (r.PausedRevision != nil && *r.PausedRevision == cfg.Revision) {
		r.NextCheckAt = nil
	}
	r.RedisStatus = "未配置额度 Redis"
	if s.cache != nil {
		r.RedisStatus = s.cache.Status()
	}
	if s.location != nil {
		r.OriginalTimezone = s.location.String()
	}
	var message string
	err = s.repo.db.QueryRowContext(ctx, `SELECT last_error FROM sub2api_enhance.quota_follow_collector_state WHERE singleton=true`).Scan(&message)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return r, err
	}
	r.CollectorError = message
	if err := s.repo.db.QueryRowContext(ctx, `SELECT carryover_error FROM sub2api_enhance.quota_follow_runtime WHERE singleton=true`).Scan(&r.CarryoverError); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return r, err
	}
	if err := s.carryoverPrerequisite(); err != nil {
		r.CarryoverError = err.Error()
	}
	return r, nil
}
