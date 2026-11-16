package quotafollow

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"log"
	"strings"
	infra "sub2api-enhance/internal/pkg/errors"
	"sub2api-enhance/internal/sub2api"
	"sync"
	"time"
)

// 检测与交付各自使用增强锁，不占用提示词审核槽位。
const (
	detectLock   int64 = 748239516309712
	deliveryLock int64 = 748239516309713
)

type QuotaAPI interface {
	Configured() bool
	AccountQuota(context.Context, int64) (sub2api.AccountQuota, error)
	ResetSubscriptions(context.Context, []byte, string) sub2api.SubscriptionResetReply
}

type QuotaSource interface {
	MonitorAccount(context.Context, int64) (sub2api.MonitorAccount, error)
	MonitorCandidates(context.Context) ([]sub2api.MonitorAccount, error)
	ValidSubscriptions(context.Context) ([]sub2api.SubscriptionSnapshot, error)
	ValidSubscriptionGroups(context.Context) ([]sub2api.SubscriptionGroupCount, error)
	SubscriptionSnapshots(context.Context, []int64) (map[int64]sub2api.SubscriptionSnapshot, error)
}

type Service struct {
	lockDB *sql.DB
	repo   *Repository
	source QuotaSource
	api    QuotaAPI
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex

	// backoffMu 保护检测失败后的进程内退避，避免无法提交时每秒调用上游额度接口。
	backoffMu sync.Mutex
	retryAt   time.Time
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
	}{{detectLock, time.Second, s.detectWithBackoff}, {deliveryLock, time.Second, s.deliver}} {
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

// detectWithBackoff 在检测无法提交时保留进程内退避，避免每秒重复调用上游额度接口。
func (s *Service) detectWithBackoff(ctx context.Context) error {
	s.backoffMu.Lock()
	waiting := time.Now().Before(s.retryAt)
	s.backoffMu.Unlock()
	if waiting {
		return nil
	}
	err := s.detect(ctx)
	s.backoffMu.Lock()
	if err != nil {
		s.retryAt = time.Now().Add(detectionRetryInterval)
	} else {
		s.retryAt = time.Time{}
	}
	s.backoffMu.Unlock()
	return err
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

// detect 每次只采样一个监控账号；判定成立时在提交事务内冻结全部有效订阅。
func (s *Service) detect(ctx context.Context) error {
	cfg, err := s.repo.Config(ctx)
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		return nil
	}
	if err = cfg.Config.Validate(); err != nil {
		return err
	}
	previous, err := s.repo.Runtime(ctx)
	if err != nil {
		return err
	}
	if previous.Epoch == cfg.Epoch {
		if previous.PausedRevision != nil && *previous.PausedRevision == cfg.Revision {
			return nil
		}
		if previous.NextCheckAt != nil && previous.NextCheckAt.After(time.Now().UTC()) {
			return nil
		}
	}
	// 监控账号校验只读原版数据库；不满足时立即显示原因，也不再消耗 OpenAI 请求。
	account, err := s.source.MonitorAccount(ctx, cfg.MonitorAccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return s.saveFailure(ctx, cfg, previous, sub2api.MonitorAccount{}, "监控账号不存在或已删除", false, time.Now().UTC())
	}
	if err != nil {
		return err
	}
	if problems := monitorProblems(account); len(problems) > 0 {
		return s.saveFailure(ctx, cfg, previous, account, "监控账号不可用："+strings.Join(problems, "；"), false, time.Now().UTC())
	}
	if !s.api.Configured() {
		return s.saveFailure(ctx, cfg, previous, account, "原版管理员 API Key 未配置", false, time.Now().UTC())
	}

	quota, queryErr := s.api.AccountQuota(ctx, cfg.MonitorAccountID)
	now := time.Now().UTC()
	if queryErr != nil {
		message := queryErr.Error()
		paused := false
		if pause := pauseMessage(queryErr); pause != "" {
			message, paused = pause, true
		}
		return s.saveFailure(ctx, cfg, previous, account, message, paused, now)
	}

	next, signal := evaluate(previous, cfg, account, quota, now)
	next.NextCheckAt = timePointer(nextCheck(now, next.BaselineResetAt, next.LastError != ""))
	if signal == nil {
		saved, saveErr := s.repo.SaveObservation(ctx, cfg, previous, next, nil)
		if saveErr == nil {
			s.logDetection(previous, saved, quota, false)
		}
		return saveErr
	}

	// 模型外查询可能耗时较长；事件成立时重新冻结全部有效订阅。
	subscriptions, readErr := s.source.ValidSubscriptions(ctx)
	if readErr != nil {
		next.LastError = "事件目标读取失败：" + readErr.Error()
		next.NextCheckAt = timePointer(nextCheck(now, previous.BaselineResetAt, true))
		// 名单读取失败时不推进基线，下一轮重新判定同一窗口。
		next.BaselineResetAt = previous.BaselineResetAt
		next.AwaitingWindow = previous.AwaitingWindow
		next.UsedPercent = previous.UsedPercent
		next.FetchedAt = previous.FetchedAt
		saved, saveErr := s.repo.SaveObservation(ctx, cfg, previous, next, nil)
		if saveErr == nil {
			s.logDetection(previous, saved, quota, false)
		}
		return saveErr
	}
	evidence, _ := json.Marshal(map[string]any{
		"trigger": signal.Trigger,
		"account": map[string]any{
			"id": account.ID, "name": account.Name, "upstream_account_id": quota.AccountID,
			"plan_type": quota.PlanType, "used_percent": quota.Windows, "fetched_at": quota.FetchedAt,
		},
		"baseline_reset_at": previous.BaselineResetAt,
	})
	event := &pendingEvent{Key: signal.Key, Trigger: signal.Trigger, ResetAt: signal.ResetAt, Evidence: evidence, Subscriptions: subscriptions}
	saved, saveErr := s.repo.SaveObservation(ctx, cfg, previous, next, event)
	if saveErr == nil {
		s.logDetection(previous, saved, quota, true)
	}
	return saveErr
}

// saveFailure 记录一次未产生事件的检测；暂停时保留基线并把修订号写进运行状态。
func (s *Service) saveFailure(ctx context.Context, cfg SavedConfig, previous Runtime, account sub2api.MonitorAccount, message string, paused bool, now time.Time) error {
	next := previous
	if previous.Epoch != cfg.Epoch {
		next = Runtime{LastResetAt: previous.LastResetAt, LastEventID: previous.LastEventID}
	}
	next.Epoch = cfg.Epoch
	next.MonitorAccountID = cfg.MonitorAccountID
	if account.Name != "" {
		next.AccountName = account.Name
	}
	next.LastCheckedAt = &now
	next.LastError = message
	next.NextCheckAt = nil
	if paused {
		revision := cfg.Revision
		next.PausedRevision = &revision
	} else {
		next.PausedRevision = nil
		next.NextCheckAt = timePointer(nextCheck(now, previous.BaselineResetAt, true))
	}
	saved, err := s.repo.SaveObservation(ctx, cfg, previous, next, nil)
	if err != nil {
		return err
	}
	before, _ := json.Marshal(previous)
	after, _ := json.Marshal(saved)
	log.Printf("订阅跟随检测未完成 paused=%t error=%s before=%s after=%s", paused, message, before, after)
	return nil
}

func (s *Service) logDetection(previous, next Runtime, quota sub2api.AccountQuota, triggered bool) {
	before, _ := json.Marshal(previous)
	after, _ := json.Marshal(next)
	sampled, _ := json.Marshal(map[string]any{"account_id": quota.AccountID, "plan_type": quota.PlanType, "fetched_at": quota.FetchedAt, "windows": quota.Windows})
	log.Printf("订阅跟随检测完成 triggered=%t before=%s after=%s sample=%s", triggered, before, after, sampled)
}

// pauseMessage 按响应原因区分监控账号授权失效与原版管理员凭据失效。
func pauseMessage(err error) string {
	var apiErr *sub2api.APIError
	if !errors.As(err, &apiErr) || (apiErr.Status != 401 && apiErr.Status != 403) {
		return ""
	}
	if apiErr.Reason() == "OPENAI_QUOTA_UPSTREAM_ERROR" {
		return "监控账号授权失效，请在 sub2api 重新授权后重新保存配置"
	}
	return "原版管理员 API Key 无效"
}

// Runtime 返回单行运行状态；不可查询时清空下次检测时间。
func (s *Service) Runtime(ctx context.Context) (Runtime, error) {
	cfg, err := s.repo.Config(ctx)
	if err != nil {
		return Runtime{}, err
	}
	v, err := s.repo.Runtime(ctx)
	if err != nil {
		return Runtime{}, err
	}
	if !cfg.Enabled || v.Epoch != cfg.Epoch || (v.PausedRevision != nil && *v.PausedRevision == cfg.Revision) {
		v.NextCheckAt = nil
	}
	return v, nil
}

// Discovery 提供监控账号候选与有效订阅分布，供页面在确认前只读预览。
func (s *Service) Discovery(ctx context.Context) (Discovery, error) {
	accounts, err := s.source.MonitorCandidates(ctx)
	if err != nil {
		return Discovery{}, err
	}
	groups, err := s.source.ValidSubscriptionGroups(ctx)
	if err != nil {
		return Discovery{}, err
	}
	out := Discovery{MonitorAccounts: []MonitorCandidate{}, GroupCounts: groups}
	for _, a := range accounts {
		problems := monitorProblems(a)
		out.MonitorAccounts = append(out.MonitorAccounts, MonitorCandidate{MonitorAccount: a, Problems: problems, Valid: len(problems) == 0})
	}
	for _, g := range groups {
		out.SubscriptionCount += g.Count
	}
	return out, nil
}

// ValidateMonitor 在启用前同步校验监控账号，避免保存后才发现不可用。
func (s *Service) ValidateMonitor(ctx context.Context, id int64) error {
	account, err := s.source.MonitorAccount(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return infra.BadRequest("quota_follow_monitor_missing", "监控账号不存在或已删除")
	}
	if err != nil {
		return err
	}
	if problems := monitorProblems(account); len(problems) > 0 {
		return infra.BadRequest("quota_follow_monitor_invalid", "监控账号不可用："+strings.Join(problems, "；"))
	}
	return nil
}
