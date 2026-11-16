package quotafollow

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"math/rand/v2"
	"sub2api-enhance/internal/sub2api"
	"time"
)

// settingKey 是新的单监控账号配置键；旧 quota_follow_config 保留在库中但不再读取。
const settingKey = "quota_follow_monitor_config"

// 固定规则用常量表达，不进入配置；只有业务范围本身才值得让管理员调整。
const (
	// weeklyWindowSeconds 是上游周窗口的固定时长，用于在不假设窗口位置时识别它。
	weeklyWindowSeconds = 604800
	// resetAdvanceThreshold 远大于同一窗口内约 27 秒的重置时间抖动。
	resetAdvanceThreshold = time.Hour
	// sampleClockTolerance 覆盖额度查询耗时，过旧或明显超前的采样不能作证据。
	sampleClockTolerance = time.Minute
	// 检测间隔 10–15 分钟随机；失败后固定 15 分钟重试。
	detectionMinInterval   = 10 * time.Minute
	detectionMaxInterval   = 15 * time.Minute
	detectionRetryInterval = 15 * time.Minute
	// weeklyPeriod 是上游周窗口长度，用于自然重置的事件时间。
	weeklyPeriod = 7 * 24 * time.Hour
)

// resetWindows 是跟随固定重置的三个窗口，正文顺序固定为日、周、月。
var resetWindows = []string{"daily", "weekly", "monthly"}

type Config struct {
	Enabled          bool  `json:"enabled"`
	MonitorAccountID int64 `json:"monitor_account_id"`
}

// SavedConfig 中的 epoch、enabled_at、revision 由服务端维护，客户端不能指定。
type SavedConfig struct {
	Config
	Epoch     string     `json:"epoch"`
	EnabledAt *time.Time `json:"enabled_at"`
	Revision  int64      `json:"revision"`
	UpdatedBy int64      `json:"updated_by"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// ConfigUpdate 使用 expected_revision 防止覆盖其他管理员的保存。
type ConfigUpdate struct {
	Enabled          bool  `json:"enabled"`
	MonitorAccountID int64 `json:"monitor_account_id"`
	ExpectedRevision int64 `json:"expected_revision" binding:"required,min=1"`
}

// DefaultConfig 表示从未保存过的配置：关闭、修订号 1、没有启用周期。
func DefaultConfig() SavedConfig { return SavedConfig{Revision: 1} }

func (c Config) Validate() error {
	if c.MonitorAccountID < 0 {
		return errors.New("监控账号 ID 无效")
	}
	if c.Enabled && c.MonitorAccountID <= 0 {
		return errors.New("启用前必须选择监控账号")
	}
	return nil
}

// Runtime 是单行运行状态；baseline_reset_at 即判定规则中的 B。
type Runtime struct {
	Epoch             string       `json:"epoch"`
	MonitorAccountID  int64        `json:"monitor_account_id"`
	AccountName       string       `json:"account_name"`
	UpstreamAccountID string       `json:"upstream_account_id"`
	PlanType          string       `json:"plan_type"`
	BaselineResetAt   *time.Time   `json:"baseline_reset_at"`
	AwaitingWindow    bool         `json:"awaiting_window"`
	UsedPercent       *json.Number `json:"used_percent"`
	FetchedAt         *time.Time   `json:"fetched_at"`
	LastCheckedAt     *time.Time   `json:"last_checked_at"`
	NextCheckAt       *time.Time   `json:"next_check_at"`
	LastResetAt       *time.Time   `json:"last_reset_at"`
	LastEventID       *int64       `json:"last_event_id"`
	LastError         string       `json:"last_error"`
	PausedRevision    *int64       `json:"paused_revision"`
}

// monitorProblems 按原版账号表字段判断监控账号是否可用，不消耗 OpenAI 请求。
func monitorProblems(a sub2api.MonitorAccount) []string {
	problems := []string{}
	if a.Platform != "openai" {
		problems = append(problems, "账号平台不是 openai")
	}
	if a.Type != "oauth" {
		problems = append(problems, "账号类型不是 oauth")
	}
	if a.ParentID != nil {
		problems = append(problems, "影子账号不能作为监控账号")
	}
	if a.Deleted {
		problems = append(problems, "账号已删除")
	}
	if a.Status != "active" {
		problems = append(problems, "账号状态不是 active")
	}
	if a.Schedulable {
		problems = append(problems, "账号仍在调度中，请先在原版关闭调度")
	}
	if a.GroupCount > 0 {
		problems = append(problems, "账号已绑定分组，请先解绑")
	}
	return problems
}

type MonitorCandidate struct {
	sub2api.MonitorAccount
	Problems []string `json:"problems"`
	Valid    bool     `json:"valid"`
}

// Discovery 是页面确认前的只读预览：监控账号候选与有效订阅分布。
type Discovery struct {
	MonitorAccounts   []MonitorCandidate               `json:"monitor_accounts"`
	SubscriptionCount int                              `json:"subscription_count"`
	GroupCounts       []sub2api.SubscriptionGroupCount `json:"group_counts"`
}

type Filter struct {
	From, To                                *time.Time
	Source, Detail, Window, Status, Keyword string
	GroupID                                 *int64
	Page, PageSize                          int
}
type Page[T any] struct {
	Items    []T   `json:"items"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
}
type Batch struct {
	ID          int64       `json:"id"`
	EventID     int64       `json:"event_id"`
	RequestID   string      `json:"request_id"`
	Body        string      `json:"request_body"`
	Status      string      `json:"status"`
	Response    string      `json:"response_body"`
	Error       string      `json:"error_message"`
	HTTPStatus  *int        `json:"http_status"`
	StartedAt   *time.Time  `json:"started_at"`
	CompletedAt *time.Time  `json:"completed_at"`
	Trigger     string      `json:"trigger_type"`
	Epoch       string      `json:"epoch"`
	Items       []BatchItem `json:"items"`
}
type BatchItem struct {
	ID             int64                         `json:"id"`
	SubscriptionID int64                         `json:"subscription_id"`
	UserID         int64                         `json:"user_id"`
	GroupID        int64                         `json:"group_id"`
	Username       string                        `json:"username"`
	Status         string                        `json:"status"`
	Error          string                        `json:"error_message"`
	Before         *sub2api.SubscriptionSnapshot `json:"before_snapshot"`
	After          *sub2api.SubscriptionSnapshot `json:"after_snapshot"`
}

// sample 是一次通过时效校验的采样：R 保留原始秒值，0 表示上游未返回绝对重置时间。
type sample struct {
	ResetAtUnix int64
	FetchedAt   time.Time
	UsedPercent json.Number
}

// eventSignal 表示本轮判定成立、需要在同一事务内冻结名单并建批次。
type eventSignal struct {
	Key     string
	ResetAt time.Time
	Trigger string
}

// pendingEvent 是检测与人工触发共用的建事件输入。
type pendingEvent struct {
	Key           string
	Trigger       string
	ResetAt       time.Time
	Evidence      json.RawMessage
	Subscriptions []sub2api.SubscriptionSnapshot
}

// evaluate 实现技术方案第 5.3 节的判定顺序；返回的 Runtime 带下次检测前的完整状态。
func evaluate(previous Runtime, cfg SavedConfig, account sub2api.MonitorAccount, quota sub2api.AccountQuota, now time.Time) (Runtime, *eventSignal) {
	next := previous
	if previous.Epoch != cfg.Epoch {
		// 启用周期变化时丢弃旧周期基线，只保留展示用的账号名与最近事件。
		next = Runtime{LastResetAt: previous.LastResetAt, LastEventID: previous.LastEventID}
	}
	next.Epoch = cfg.Epoch
	next.MonitorAccountID = cfg.MonitorAccountID
	next.AccountName = account.Name
	next.LastCheckedAt = &now
	next.LastError = ""
	// 能进入评估说明本轮未被暂停；恢复后必须清掉旧修订号，页面才不会继续显示暂停。
	next.PausedRevision = nil

	weekly, err := weeklyWindow(quota)
	if err != nil {
		next.LastError = err.Error()
		return next, nil
	}
	current, err := trustedSample(weekly, quota, next.FetchedAt, now)
	if err != nil {
		next.LastError = err.Error()
		return next, nil
	}

	identityChanged := (quota.AccountID != "" && next.UpstreamAccountID != "" && quota.AccountID != next.UpstreamAccountID) ||
		(quota.PlanType != "" && next.PlanType != "" && quota.PlanType != next.PlanType)
	if quota.AccountID != "" {
		next.UpstreamAccountID = quota.AccountID
	}
	if quota.PlanType != "" {
		next.PlanType = quota.PlanType
	}
	// 规则 2：本周期还没有基线、或上游账号与套餐发生变化时只重建基线。
	rebase := identityChanged || next.BaselineResetAt == nil
	if rebase {
		next.BaselineResetAt = nil
		next.AwaitingWindow = false
	}

	var signal *eventSignal
	baseline := next.BaselineResetAt
	switch {
	case current.ResetAtUnix > now.Unix():
		resetAt := time.Unix(current.ResetAtUnix, 0).UTC()
		switch {
		case rebase:
			next.BaselineResetAt = timePointer(resetAt)
		case next.AwaitingWindow:
			// 规则 3：兜底触发后第一次拿到有效 R 只建立基线。
			next.BaselineResetAt = timePointer(resetAt)
			next.AwaitingWindow = false
		case resetAt.Sub(*baseline) > resetAdvanceThreshold && !now.Before(*baseline):
			// 规则 4：已跨过旧边界且新窗口确实进入未来，属于自然重置。
			eventAt := *baseline
			if edge := resetAt.Add(-weeklyPeriod); edge.After(eventAt) {
				eventAt = edge
			}
			signal = &eventSignal{Key: automaticEventKey(cfg.Epoch, *baseline), ResetAt: eventAt, Trigger: "automatic"}
			next.BaselineResetAt = timePointer(resetAt)
		case resetAt.Sub(*baseline) > resetAdvanceThreshold && usedPercentAboveZero(next.UsedPercent):
			// 规则 5：尚未到旧边界但重置时间前进一周以上且本窗口用过，属于提前重置。
			signal = &eventSignal{Key: automaticEventKey(cfg.Epoch, *baseline), ResetAt: current.FetchedAt, Trigger: "automatic"}
			next.BaselineResetAt = timePointer(resetAt)
		case resetAt.Sub(*baseline) > resetAdvanceThreshold:
			// 规则 6：本窗口从未使用（上次用量为 0）时的浮动漂移不是重置。
			next.BaselineResetAt = timePointer(resetAt)
		case resetAt.Before(baseline.Add(-resetAdvanceThreshold)):
			// 规则 8：重置时间回退说明证据不可信，重建基线但不触发。
			next.BaselineResetAt = timePointer(resetAt)
			next.LastError = "监控账号周窗口重置时间回退，已重建基线"
		default:
			// 规则 7：同一窗口内的秒级抖动，只跟随最新值。
			next.BaselineResetAt = timePointer(resetAt)
		}
	case current.ResetAtUnix > 0:
		// 规则 9：上游还没切到新窗口，不触发也不改动基线。
		if rebase {
			next.LastError = "监控账号周窗口重置时间不在未来，等待有效采样"
		}
	case rebase:
		// 规则 2 的无效分支：没有基线时不能凭缺失的重置时间建立基线。
		next.LastError = "监控账号未返回绝对重置时间"
	case !next.AwaitingWindow && !now.Before(*baseline) && isZeroPercent(current.UsedPercent):
		// 规则 10：旧边界已过、上游不返回重置时间且用量为 0，旧窗口已结束而新窗口还没开始。
		signal = &eventSignal{Key: automaticEventKey(cfg.Epoch, *baseline), ResetAt: *baseline, Trigger: "automatic"}
		next.AwaitingWindow = true
	default:
		// 规则 11：缺少绝对重置时间，基线不变。
		next.LastError = "监控账号未返回绝对重置时间"
	}

	used := current.UsedPercent
	next.UsedPercent = &used
	next.FetchedAt = &current.FetchedAt
	return next, signal
}

// weeklyWindow 按 limit_window_seconds 识别唯一的周窗口，不假设它在 primary 还是 secondary。
func weeklyWindow(quota sub2api.AccountQuota) (*sub2api.QuotaWindow, error) {
	if quota.Windows == nil {
		return nil, errors.New("监控账号未返回额度窗口")
	}
	var found *sub2api.QuotaWindow
	for _, w := range []*sub2api.QuotaWindow{quota.Windows.Primary, quota.Windows.Secondary} {
		if w == nil || w.Seconds != weeklyWindowSeconds {
			continue
		}
		if found != nil {
			return nil, errors.New("监控账号返回多个周窗口，不能确定监控目标")
		}
		found = w
	}
	if found == nil {
		return nil, errors.New("监控账号未返回周窗口")
	}
	return found, nil
}

// trustedSample 校验采样时效与单调性；上游不返回 used_percent 时按 0 处理。
func trustedSample(weekly *sub2api.QuotaWindow, quota sub2api.AccountQuota, previous *time.Time, now time.Time) (sample, error) {
	used := json.Number("0")
	if weekly.UsedPercent != nil {
		used = *weekly.UsedPercent
	}
	if value, ok := new(big.Rat).SetString(used.String()); !ok || value.Sign() < 0 {
		return sample{}, errors.New("监控账号周利用率无效")
	}
	if quota.FetchedAt <= 0 {
		return sample{}, errors.New("监控账号未返回采样时间")
	}
	fetched := time.Unix(quota.FetchedAt, 0).UTC()
	if fetched.After(now.Add(sampleClockTolerance)) || now.Sub(fetched) > sampleClockTolerance {
		return sample{}, errors.New("采样时间与本次查询相差超过 1 分钟")
	}
	if previous != nil && !fetched.After(*previous) {
		return sample{}, errors.New("采样时间没有前进")
	}
	return sample{ResetAtUnix: weekly.ResetAt, FetchedAt: fetched, UsedPercent: used}, nil
}

func usedPercentAboveZero(value *json.Number) bool {
	if value == nil {
		return false
	}
	rat, ok := new(big.Rat).SetString(value.String())
	return ok && rat.Sign() > 0
}

func isZeroPercent(value json.Number) bool {
	rat, ok := new(big.Rat).SetString(value.String())
	return ok && rat.Sign() == 0
}

// automaticEventKey 固定为“被重置掉的旧窗口”，同一窗口只会触发一次。
func automaticEventKey(epoch string, baseline time.Time) string {
	return fmt.Sprintf("automatic:%s:%d", epoch, baseline.Unix())
}

// nextCheck 取“当前时间 + 10–15 分钟随机值”，但边界后 1 秒更早就优先边界。
func nextCheck(now time.Time, baseline *time.Time, failed bool) time.Time {
	if failed {
		return now.Add(detectionRetryInterval)
	}
	next := now.Add(detectionMinInterval + time.Duration(rand.Int64N(int64(detectionMaxInterval-detectionMinInterval)+1)))
	if baseline != nil && baseline.After(now) {
		if edge := baseline.Add(time.Second); edge.Before(next) {
			next = edge
		}
	}
	return next
}

func timePointer(t time.Time) *time.Time { return &t }
