package quotafollow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"sort"
	"sub2api-enhance/internal/sub2api"
	"time"
)

// settingKey 延用既有额度配置入口，迁移负责单分组到多分组的转换。
const settingKey = "quota_follow_config"

// weeklyPeriod 是上游周窗口的已知时长，不将短窗口当作日窗口。
const weeklyPeriod = 7 * 24 * time.Hour

// quotaSampleClockTolerance 覆盖管理 API 查询耗时及秒级时钟误差，过旧或明显超前的采样不能作证据。
const quotaSampleClockTolerance = time.Minute

type GroupRule struct {
	GroupID     int64      `json:"group_id"`
	Enabled     bool       `json:"enabled"`
	ObserveOnly bool       `json:"observe_only"`
	ResetDaily  bool       `json:"reset_daily_enabled"`
	ResetWeekly bool       `json:"reset_weekly_enabled"`
	Epoch       string     `json:"epoch"`
	EnabledAt   *time.Time `json:"enabled_at"`
}
type Config struct {
	Enabled     bool        `json:"enabled"`
	MinInterval int         `json:"min_interval_minutes"`
	MaxInterval int         `json:"max_interval_minutes"`
	Groups      []GroupRule `json:"groups"`
}
type SavedConfig struct {
	Config
	Revision  int64     `json:"revision"`
	UpdatedBy int64     `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}
type ConfigUpdate struct {
	ExpectedRevision int64  `json:"expected_revision" binding:"required,min=1"`
	Config           Config `json:"config"`
}

func DefaultConfig() SavedConfig {
	return SavedConfig{Config: Config{MinInterval: 10, MaxInterval: 15, Groups: []GroupRule{}}, Revision: 1}
}
func (c Config) Validate() error {
	if c.Groups == nil {
		return errors.New("分组规则必须提供数组，可为空")
	}
	if c.MinInterval <= 0 || c.MaxInterval < c.MinInterval || int64(c.MaxInterval) > int64((1<<63-1)/time.Minute) {
		return errors.New("检测间隔必须为有效正分钟数，且上限不小于下限")
	}
	seen := map[int64]bool{}
	for _, g := range c.Groups {
		if g.GroupID <= 0 || seen[g.GroupID] {
			return errors.New("分组 ID 必须有效且不能重复")
		}
		seen[g.GroupID] = true
		if g.Enabled && !g.ResetDaily && !g.ResetWeekly {
			return errors.New("启用分组前至少选择一个订阅重置窗口")
		}
	}
	if c.Enabled && len(c.Groups) == 0 {
		return errors.New("启用前至少添加一个分组")
	}
	return nil
}
func (c Config) Group(id int64) (GroupRule, bool) {
	for _, g := range c.Groups {
		if g.GroupID == id {
			return g, true
		}
	}
	return GroupRule{}, false
}
func newEpochRequired(a, b GroupRule) bool {
	return a.GroupID != b.GroupID || a.Enabled != b.Enabled || a.ObserveOnly != b.ObserveOnly || a.ResetDaily != b.ResetDaily || a.ResetWeekly != b.ResetWeekly
}

type AccountState struct {
	Account            sub2api.SubscriptionAccount `json:"account"`
	Utilization        *json.Number                `json:"utilization"`
	NextResetAt        *time.Time                  `json:"next_reset_at"`
	CandidateResetAt   *time.Time                  `json:"candidate_reset_at"`
	ConsumedResetAt    *time.Time                  `json:"consumed_reset_at"`
	BaselineRebased    bool                        `json:"baseline_rebased"`
	ObservedAt         *time.Time                  `json:"observed_at"`
	FetchedAt          *time.Time                  `json:"fetched_at"`
	ConsumedObservedAt *time.Time                  `json:"consumed_observed_at"`
	LastAuditID        int64                       `json:"last_audit_id"`
	SuspectedDrop      bool                        `json:"suspected_drop"`
	Error              string                      `json:"error"`
}
type Runtime struct {
	GroupID           int64          `json:"group_id"`
	GroupName         string         `json:"group_name"`
	Epoch             string         `json:"epoch"`
	AccountSetHash    string         `json:"account_set_hash"`
	States            []AccountState `json:"account_states"`
	SubscriptionCount int            `json:"subscription_count"`
	LastEventAt       *time.Time     `json:"last_event_at"`
	LastCheckedAt     *time.Time     `json:"last_checked_at"`
	NextCheckAt       *time.Time     `json:"next_check_at"`
	LastError         string         `json:"last_error"`
	PausedRevision    *int64         `json:"paused_revision"`
	BaselineAt        *time.Time     `json:"baseline_at"`
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
	ID             int64       `json:"id"`
	EventID        int64       `json:"event_id"`
	GroupID        int64       `json:"group_id"`
	RequestID      string      `json:"request_id"`
	Body           string      `json:"request_body"`
	Status         string      `json:"status"`
	Response       string      `json:"response_body"`
	Error          string      `json:"error_message"`
	HTTPStatus     *int        `json:"http_status"`
	StartedAt      *time.Time  `json:"started_at"`
	CompletedAt    *time.Time  `json:"completed_at"`
	Rule           GroupRule   `json:"config_snapshot"`
	AccountSetHash string      `json:"account_set_hash"`
	Trigger        string      `json:"trigger_type"`
	Items          []BatchItem `json:"items"`
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

func accountSetHash(accounts []sub2api.SubscriptionAccount) string {
	identities := append([]sub2api.SubscriptionAccount{}, accounts...)
	sort.Slice(identities, func(i, j int) bool { return identities[i].ID < identities[j].ID })
	for i := range identities {
		identities[i].Name = ""
	}
	raw, _ := json.Marshal(identities)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

// observe 仅接受本轮真实配额采样；自然重置要求旧边界已跨过且新窗口确实进入未来。
func observe(previous AccountState, account sub2api.SubscriptionAccount, quota sub2api.AccountQuota, queryErr error, now, baseline time.Time) AccountState {
	next := previous
	next.Account = account
	next.Error = ""
	next.SuspectedDrop = false
	if queryErr != nil {
		next.Error = queryErr.Error()
		return next
	}
	if account.Type != "oauth" {
		next.Error = "该账号类型不支持官方周窗口查询"
		return next
	}
	windows := quota.Windows
	if account.IsShadow {
		windows = nil
		for _, v := range quota.Additional {
			if v.Feature == "codex_bengalfox" {
				windows = v.Windows
				break
			}
		}
	}
	if windows == nil {
		next.Error = "账号缺少对应的官方配额窗口"
		return next
	}
	var weekly *sub2api.QuotaWindow
	for _, w := range []*sub2api.QuotaWindow{windows.Primary, windows.Secondary} {
		if w != nil && w.Seconds == int64(weeklyPeriod/time.Second) {
			if weekly != nil {
				next.Error = "账号返回多个周窗口，不能确定监控目标"
				return next
			}
			weekly = w
		}
	}
	if weekly == nil || weekly.UsedPercent == nil || weekly.ResetAt <= 0 || quota.FetchedAt <= 0 {
		next.Error = "账号缺少完整周窗口或绝对重置时间"
		return next
	}
	value, ok := new(big.Rat).SetString(weekly.UsedPercent.String())
	if !ok || value.Sign() < 0 {
		next.Error = "账号周利用率无效"
		return next
	}
	sampled := time.Unix(quota.FetchedAt, 0).UTC()
	boundary := time.Unix(weekly.ResetAt, 0).UTC()
	if sampled.After(now.Add(quotaSampleClockTolerance)) || now.Sub(sampled) > quotaSampleClockTolerance || (previous.FetchedAt != nil && !sampled.After(*previous.FetchedAt)) {
		next.Error = "账号采样时间无效或没有更新"
		return next
	}
	if previous.Utilization != nil {
		if old, ok := new(big.Rat).SetString(previous.Utilization.String()); ok && value.Cmp(old) < 0 {
			next.SuspectedDrop = true
		}
	}
	if previous.NextResetAt != nil {
		if boundary.Before(*previous.NextResetAt) {
			next.CandidateResetAt = nil
			next.BaselineRebased = true
		} else if previous.BaselineRebased {
			next.CandidateResetAt = nil
			next.BaselineRebased = !boundary.After(sampled)
		} else if !sampled.Before(*previous.NextResetAt) && boundary.After(sampled) && boundary.Sub(*previous.NextResetAt) > weeklyPeriod/2 && !previous.NextResetAt.Before(baseline) {
			// 忽略秒级边界抖动；跨越多个周窗口仅记最新一轮，不补发停机期间额度。
			reset := *previous.NextResetAt
			if boundary.Sub(reset) > weeklyPeriod {
				reset = boundary.Add(-weeklyPeriod)
			}
			if previous.ConsumedResetAt == nil || reset.After(*previous.ConsumedResetAt) {
				next.CandidateResetAt = &reset
			}
		}
	}
	next.Utilization = weekly.UsedPercent
	next.NextResetAt = &boundary
	next.FetchedAt = &sampled
	next.ObservedAt = &now
	return next
}
func consensus(states []AccountState) (*time.Time, error) {
	if len(states) == 0 {
		return nil, errors.New("分组内没有有效 OpenAI 账号")
	}
	var last time.Time
	for _, s := range states {
		if s.Error != "" || s.BaselineRebased {
			return nil, errors.New("部分账号证据不可用，等待本组全部账号确认")
		}
		if s.CandidateResetAt == nil || (s.ConsumedResetAt != nil && !s.CandidateResetAt.After(*s.ConsumedResetAt)) {
			return nil, nil
		}
		if s.CandidateResetAt.After(last) {
			last = *s.CandidateResetAt
		}
	}
	return &last, nil
}
func consume(states []AccountState) {
	for i := range states {
		states[i].ConsumedResetAt = states[i].CandidateResetAt
		states[i].ConsumedObservedAt = states[i].FetchedAt
		states[i].CandidateResetAt = nil
	}
}
