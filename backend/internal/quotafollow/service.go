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

// 后台检测、交付和审计采集各自使用增强锁，不占用提示词审核槽位。
const (
	detectLock    int64 = 748239516309712
	deliveryLock  int64 = 748239516309713
	collectorLock int64 = 748239516309714
)

// quotaQueryConcurrency 限制原版配额查询的同时调用数，跨分组账号在同一轮复用结果。
const quotaQueryConcurrency = 4

type QuotaAPI interface {
	Configured() bool
	AccountQuota(context.Context, int64) (sub2api.AccountQuota, error)
	ResetSubscriptions(context.Context, []byte, string) sub2api.SubscriptionResetReply
}
type QuotaSource interface {
	DiscoverSubscriptions(context.Context, int64) (sub2api.SubscriptionDiscovery, error)
	SubscriptionSnapshots(context.Context, int64, []int64) (map[int64]sub2api.SubscriptionSnapshot, error)
}
type Service struct {
	lockDB *sql.DB
	repo   *Repository
	source QuotaSource
	api    QuotaAPI
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
}

func NewService(repo *Repository, source *sub2api.QuotaData, api *sub2api.Client, lockDB *sql.DB) *Service {
	return &Service{lockDB: lockDB, repo: repo, source: source, api: api}
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
	}{{detectLock, time.Second, s.detect}, {deliveryLock, time.Second, s.deliver}, {collectorLock, time.Minute, s.collect}} {
		s.wg.Add(1)
		go func(lock int64, interval time.Duration, run func(context.Context) error) {
			defer s.wg.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				if err := s.locked(ctx, lock, run); err != nil && ctx.Err() == nil {
					log.Printf("订阅跟随后台处理失败 task=%d error=%v", lock, err)
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
	if err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var released bool
		if err := conn.QueryRowContext(cleanup, `SELECT pg_advisory_unlock($1)`, key).Scan(&released); err != nil || !released {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	return run(ctx)
}
func nextDetection(now time.Time, cfg Config, states []AccountState, failed bool) time.Time {
	if failed {
		return now.Add(time.Duration(cfg.MaxInterval) * time.Minute)
	}
	minimum := time.Duration(cfg.MinInterval) * time.Minute
	span := time.Duration(cfg.MaxInterval-cfg.MinInterval) * time.Minute
	next := now.Add(minimum + time.Duration(rand.Int64N(int64(span)+1)))
	for _, s := range states {
		if s.NextResetAt != nil && s.NextResetAt.After(now) {
			edge := s.NextResetAt.Add(time.Second)
			if edge.Before(next) {
				next = edge
			}
		}
	}
	return next
}

type quotaSample struct {
	value sub2api.AccountQuota
	err   error
	at    time.Time
}
type groupDetection struct {
	rule      GroupRule
	before    Runtime
	discovery sub2api.SubscriptionDiscovery
	err       error
}

func (s *Service) detect(ctx context.Context) error {
	cfg, err := s.repo.Config(ctx)
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		return nil
	}
	if err = cfg.Validate(); err != nil {
		return err
	}
	now := time.Now().UTC()
	due := []groupDetection{}
	ids := map[int64]bool{}
	for _, g := range cfg.Groups {
		if !g.Enabled {
			continue
		}
		previous, readErr := s.repo.Runtime(ctx, g.GroupID)
		if readErr != nil {
			return readErr
		}
		if previous.Epoch == g.Epoch && ((previous.PausedRevision == nil && previous.NextCheckAt != nil && previous.NextCheckAt.After(now)) || (previous.PausedRevision != nil && *previous.PausedRevision == cfg.Revision)) {
			continue
		}
		d, discoverErr := s.source.DiscoverSubscriptions(ctx, g.GroupID)
		due = append(due, groupDetection{g, previous, d, discoverErr})
		if discoverErr == nil {
			for _, a := range d.Accounts {
				if a.Type != "oauth" {
					continue
				}
				id := a.ID
				if a.IsShadow && a.ParentID != nil {
					id = *a.ParentID
				}
				ids[id] = true
			}
		}
	}
	if len(due) == 0 {
		return nil
	}
	samples := s.sampleAccounts(ctx, ids)
	var failures []error
	for _, d := range due {
		next := d.before
		next.GroupID = d.rule.GroupID
		next.GroupName = d.discovery.GroupName
		next.Epoch = d.rule.Epoch
		next.LastCheckedAt = &now
		next.LastError = ""
		next.PausedRevision = nil
		if d.err != nil {
			if d.before.Epoch != d.rule.Epoch {
				next.AccountSetHash = ""
				next.States = []AccountState{}
				next.LastEventAt = nil
				next.BaselineAt = &now
			}
			next.LastError = d.err.Error()
			next.NextCheckAt = timePointer(nextDetection(time.Now().UTC(), cfg.Config, nil, true))
			if err = s.repo.SaveObservation(ctx, cfg, d.rule, d.discovery, next, d.before, nil); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		next.SubscriptionCount = len(d.discovery.Subscriptions)
		hash := accountSetHash(d.discovery.Accounts)
		changed := d.before.Epoch != d.rule.Epoch || d.before.AccountSetHash != hash
		previous := map[int64]AccountState{}
		if changed {
			next.LastEventAt = nil
			next.BaselineAt = &now
		} else {
			for _, state := range d.before.States {
				previous[state.Account.ID] = state
			}
		}
		if next.BaselineAt == nil {
			next.BaselineAt = &now
		}
		next.AccountSetHash = hash
		next.States = []AccountState{}
		for _, a := range d.discovery.Accounts {
			id := a.ID
			if a.IsShadow && a.ParentID != nil {
				id = *a.ParentID
			}
			sample, ok := samples[id]
			if !ok {
				sample = quotaSample{err: errors.New("账号类型不支持官方周窗口查询"), at: time.Now().UTC()}
			}
			state := observe(previous[a.ID], a, sample.value, sample.err, sample.at, *next.BaselineAt)
			var apiErr *sub2api.APIError
			if errors.As(sample.err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 403) {
				revision := cfg.Revision
				next.PausedRevision = &revision
			}
			next.States = append(next.States, state)
		}
		if !changed {
			if err = s.repo.ApplyAccountResetSignals(ctx, next.States, previous, *next.BaselineAt); err != nil {
				next.LastError = "账号重置审计读取失败：" + err.Error()
			}
		}
		boundary, consensusErr := consensus(next.States)
		if consensusErr != nil {
			next.LastError = joinMessage(next.LastError, consensusErr.Error())
		}
		if changed || next.LastError != "" {
			boundary = nil
		}
		if boundary != nil {
			// 模型外配额查询可能耗时较长；事件成立时重新冻结订阅名单及账号关系。
			fresh, readErr := s.source.DiscoverSubscriptions(ctx, d.rule.GroupID)
			if readErr != nil {
				next.LastError = "事件目标读取失败：" + readErr.Error()
				boundary = nil
			} else if accountSetHash(fresh.Accounts) != hash {
				next.LastError = "检测期间账号集合发生变化，等待重建本组基线"
				boundary = nil
			} else {
				d.discovery = fresh
				next.SubscriptionCount = len(fresh.Subscriptions)
			}
		}
		next.NextCheckAt = timePointer(nextDetection(time.Now().UTC(), cfg.Config, next.States, next.LastError != ""))
		if err = s.repo.SaveObservation(ctx, cfg, d.rule, d.discovery, next, d.before, boundary); err != nil {
			failures = append(failures, err)
			continue
		}
		evidence, _ := json.Marshal(map[string]any{"before": d.before, "after": next, "reset_at": boundary})
		log.Printf("订阅跟随账号检测完成 group_id=%d evidence=%s", d.rule.GroupID, evidence)
	}
	return errors.Join(failures...)
}
func timePointer(t time.Time) *time.Time { return &t }

// sampleAccounts 使用固定数量查询协程，每个真实账号只查询一次，不调用强制模型探测。
func (s *Service) sampleAccounts(ctx context.Context, ids map[int64]bool) map[int64]quotaSample {
	results := map[int64]quotaSample{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	jobs := make(chan int64)
	for i := 0; i < min(quotaQueryConcurrency, len(ids)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				var q sub2api.AccountQuota
				var err error
				if !s.api.Configured() {
					err = errors.New("原版管理员 API Key 未配置")
				} else {
					q, err = s.api.AccountQuota(ctx, id)
				}
				sample := quotaSample{q, err, time.Now().UTC()}
				mu.Lock()
				results[id] = sample
				mu.Unlock()
			}
		}()
	}
	for id := range ids {
		jobs <- id
	}
	close(jobs)
	wg.Wait()
	return results
}
func (s *Service) Runtime(ctx context.Context) ([]Runtime, error) {
	cfg, err := s.repo.Config(ctx)
	if err != nil {
		return nil, err
	}
	out := []Runtime{}
	for _, g := range cfg.Groups {
		v, err := s.repo.Runtime(ctx, g.GroupID)
		if err != nil {
			return nil, err
		}
		if !cfg.Enabled || !g.Enabled || v.Epoch != g.Epoch || (v.PausedRevision != nil && *v.PausedRevision == cfg.Revision) {
			v.NextCheckAt = nil
		}
		out = append(out, v)
	}
	return out, nil
}
func joinMessage(a, b string) string {
	if a == "" {
		return b
	}
	return a + "；" + b
}
