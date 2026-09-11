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

// consensusTolerance 是已确认的多账号重置边界最大差值。
const consensusTolerance = 5 * time.Minute

// settingKey 将额度配置与审计配置分离。
const settingKey = "quota_follow_config"

type Config struct {
	Enabled     bool   `json:"enabled"`
	GroupID     *int64 `json:"group_id"`
	ResetWeekly bool   `json:"reset_weekly_enabled"`
	ResetDaily  bool   `json:"reset_daily_enabled"`
	MinInterval int    `json:"min_interval_minutes"`
	MaxInterval int    `json:"max_interval_minutes"`
	ObserveOnly bool   `json:"observe_only"`
}
type SavedConfig struct {
	Config
	Revision  int64      `json:"revision"`
	Epoch     string     `json:"epoch"`
	EnabledAt *time.Time `json:"enabled_at"`
	UpdatedBy int64      `json:"updated_by"`
	UpdatedAt time.Time  `json:"updated_at"`
}
type ConfigUpdate struct {
	ExpectedRevision int64  `json:"expected_revision" binding:"required,min=1"`
	Config           Config `json:"config"`
}

func DefaultConfig() SavedConfig {
	return SavedConfig{Config: Config{ResetWeekly: true, MinInterval: 10, MaxInterval: 15, ObserveOnly: true}, Revision: 1}
}
func (c Config) Validate() error {
	if c.GroupID != nil && *c.GroupID <= 0 {
		return errors.New("目标分组 ID 必须大于 0")
	}
	if c.Enabled && c.GroupID == nil {
		return errors.New("启用前必须选择目标分组")
	}
	if c.MinInterval <= 0 || c.MaxInterval < c.MinInterval || int64(c.MaxInterval) > int64((1<<63-1)/time.Minute) {
		return errors.New("检测间隔必须为有效正分钟数，且上限不小于下限")
	}
	return nil
}
func newEpochRequired(a, b Config) bool {
	return (!a.Enabled && b.Enabled) || !sameID(a.GroupID, b.GroupID) || a.ResetDaily != b.ResetDaily || a.ResetWeekly != b.ResetWeekly || a.ObserveOnly != b.ObserveOnly
}
func sameID(a, b *int64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

type AccountState struct {
	Account           sub2api.QuotaAccount `json:"account"`
	Utilization       *json.Number         `json:"utilization"`
	NextResetAt       *time.Time           `json:"next_reset_at"`
	CandidateResetAt  *time.Time           `json:"candidate_reset_at"`
	BaselineRebased   bool                 `json:"baseline_rebased"`
	ObservedAt        *time.Time           `json:"observed_at"`
	UpstreamUpdatedAt *time.Time           `json:"upstream_updated_at"`
	SuspectedDrop     bool                 `json:"suspected_drop"`
	Error             string               `json:"error"`
}
type Runtime struct {
	CarryoverError   string                 `json:"carryover_error"`
	Epoch            string                 `json:"epoch"`
	AccountSetHash   string                 `json:"account_set_hash"`
	Accounts         []sub2api.QuotaAccount `json:"accounts"`
	States           []AccountState         `json:"account_states"`
	LastEventAt      *time.Time             `json:"last_event_at"`
	LastCheckedAt    *time.Time             `json:"last_checked_at"`
	NextCheckAt      *time.Time             `json:"next_check_at"`
	LastError        string                 `json:"last_error"`
	PausedRevision   *int64                 `json:"paused_revision"`
	RedisStatus      string                 `json:"redis_status"`
	CollectorError   string                 `json:"collector_error"`
	OriginalTimezone string                 `json:"original_timezone"`
}
type Delivery struct {
	CacheBefore    map[string]string      `json:"before_cache_snapshot"`
	ID             int64                  `json:"id"`
	EventID        int64                  `json:"event_id"`
	UserID         int64                  `json:"user_id"`
	Window         string                 `json:"window"`
	RequestID      string                 `json:"request_id"`
	Status         string                 `json:"status"`
	Epoch          string                 `json:"epoch"`
	Config         SavedConfig            `json:"config_snapshot"`
	Before         *sub2api.QuotaSnapshot `json:"before_snapshot"`
	After          *sub2api.QuotaSnapshot `json:"after_snapshot"`
	Cache          map[string]string      `json:"cache_snapshot"`
	DatabaseStatus string                 `json:"database_status"`
	CacheStatus    string                 `json:"cache_status"`
	HTTPStatus     *int                   `json:"http_status"`
	Response       string                 `json:"response_body"`
	Error          string                 `json:"error_message"`
	StartedAt      *time.Time             `json:"started_at"`
	CompletedAt    *time.Time             `json:"completed_at"`
	AuditLogID     *int64                 `json:"audit_log_id"`
}
type Record struct {
	ActionType   string                 `json:"action_type"`
	BeforeUsage  *string                `json:"before_usage_usd"`
	AfterUsage   *string                `json:"after_usage_usd"`
	Accounts     []sub2api.QuotaAccount `json:"accounts"`
	ID           int64                  `json:"id"`
	Source       string                 `json:"source"`
	SourceDetail string                 `json:"source_detail"`
	EvidenceType string                 `json:"evidence_type"`
	UserID       int64                  `json:"user_id"`
	Username     string                 `json:"username"`
	Window       *string                `json:"window"`
	OccurredAt   time.Time              `json:"occurred_at"`
	DetectedAt   time.Time              `json:"detected_at"`
	Status       string                 `json:"status"`
	Before       json.RawMessage        `json:"before_snapshot"`
	After        json.RawMessage        `json:"after_snapshot"`
	Evidence     json.RawMessage        `json:"evidence"`
	EventID      *int64                 `json:"event_id"`
	DeliveryID   *int64                 `json:"delivery_id"`
	AuditLogID   *int64                 `json:"audit_log_id"`
	RequestID    string                 `json:"request_id"`
	Error        string                 `json:"error_message"`
}
type Filter struct {
	From, To                                *time.Time
	Source, Detail, Window, Status, Keyword string
	Page, PageSize                          int
}
type Page[T any] struct {
	Items    []T   `json:"items"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
}

func accountSetHash(accounts []sub2api.QuotaAccount) string {
	identities := make([]struct {
		ID   int64
		Type string
	}, 0, len(accounts))
	for _, a := range accounts {
		identities = append(identities, struct {
			ID   int64
			Type string
		}{a.ID, a.Type})
	}
	sort.Slice(identities, func(i, j int) bool { return identities[i].ID < identities[j].ID })
	raw, _ := json.Marshal(identities)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}
func observe(previous AccountState, account sub2api.QuotaAccount, usage sub2api.AccountUsage, now time.Time, enabledAt time.Time, lastEvent *time.Time) AccountState {
	next := previous
	next.Account = account
	next.Error = usage.Error
	next.SuspectedDrop = false
	if usage.Error != "" {
		return next
	}
	if usage.ResetsAt == nil || usage.ResetsAt.IsZero() || usage.Utilization == nil {
		next.Error = "账号缺少有效 seven_day 窗口"
		return next
	}
	value, ok := new(big.Rat).SetString(usage.Utilization.String())
	if !ok || value.Sign() < 0 {
		next.Error = "账号利用率格式无效"
		return next
	}
	if previous.UpstreamUpdatedAt != nil && usage.UpdatedAt != nil && usage.UpdatedAt.Before(*previous.UpstreamUpdatedAt) {
		next.Error = "账号用量快照早于上次观测来源"
		return next
	}
	if previous.Utilization != nil {
		if before, ok := new(big.Rat).SetString(previous.Utilization.String()); ok && value.Cmp(before) < 0 {
			next.SuspectedDrop = true
		}
	}
	if previous.NextResetAt != nil {
		if usage.ResetsAt.Before(*previous.NextResetAt) {
			// 上游窗口可能因账号窗口修正而提前，接受新边界但要求下一次稳定观测后再确认重置。
			next.NextResetAt = usage.ResetsAt
			next.CandidateResetAt = nil
			next.BaselineRebased = true
		} else if previous.BaselineRebased {
			next.CandidateResetAt = nil
			// 回退到过去的边界时，必须等到新的边界重新落在未来，避免回升被误判为重置。
			next.BaselineRebased = usage.ResetsAt.Before(now)
		} else if !now.Before(*previous.NextResetAt) && usage.ResetsAt.After(*previous.NextResetAt) && !previous.NextResetAt.Before(enabledAt) && (lastEvent == nil || previous.NextResetAt.After(*lastEvent)) {
			boundary := *previous.NextResetAt
			next.CandidateResetAt = &boundary
		}
	}
	next.Utilization = usage.Utilization
	next.NextResetAt = usage.ResetsAt
	next.ObservedAt = &now
	next.UpstreamUpdatedAt = usage.UpdatedAt
	return next
}
func consensus(states []AccountState) (*time.Time, error) {
	if len(states) == 0 {
		return nil, errors.New("分组内没有有效 OpenAI 账号")
	}
	var first, last time.Time
	for _, s := range states {
		if s.Error != "" {
			return nil, errors.New("部分账号用量不可用，本轮不形成一致事件")
		}
		if s.CandidateResetAt == nil {
			return nil, nil
		}
		if first.IsZero() || s.CandidateResetAt.Before(first) {
			first = *s.CandidateResetAt
		}
		if last.IsZero() || s.CandidateResetAt.After(last) {
			last = *s.CandidateResetAt
		}
	}
	if last.Sub(first) > consensusTolerance {
		return nil, errors.New("账号重置边界差值超过五分钟")
	}
	return &last, nil
}
