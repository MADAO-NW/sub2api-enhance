package thirdpartypromptaudit

import "time"

// RetentionDefaultDays 是审计记录默认保留天数；可配置范围见 validateConfig。
const RetentionDefaultDays = 30

// 保留天数的可配置范围（决策 24）。
const (
	retentionMinDays = 1
	retentionMaxDays = 365
)

// 目标结论来源：真实模型调用、全库缓存、同实例并发复用、管理员信任。
const (
	ResolutionModel    = "model"
	ResolutionCache    = "cache"
	ResolutionInflight = "inflight"
	ResolutionTrust    = "trust"
)

// 熔断状态：熔断中、已信任、已确认违规（确认后不再熔断）。
const (
	BreakerTripped   = "tripped"
	BreakerTrusted   = "trusted"
	BreakerConfirmed = "confirmed"
)

// instructionBreakerUserThreshold 是指令块熔断的不同用户数阈值（决策 10，固定常量，不设时间窗口）。
const instructionBreakerUserThreshold = 2

// AuditJob 是一次首次请求（含孤儿续跑）的审核任务；不保存请求原文。
type AuditJob struct {
	ID                  int64          `json:"id"`
	UserID              int64          `json:"user_id"`
	APIKeyID            int64          `json:"api_key_id"`
	GroupID             *int64         `json:"group_id"`
	Identity            Identity       `json:"identity"`
	DisplayUsername     string         `json:"display_username"`
	DisplayEmail        string         `json:"display_email"`
	Platform            string         `json:"platform"`
	RequestID           string         `json:"request_id"`
	Model               string         `json:"model"`
	Protocol            string         `json:"protocol"`
	ClientFingerprint   string         `json:"client_fingerprint"`
	Mode                string         `json:"mode"`
	TurnFingerprint     string         `json:"turn_fingerprint"`
	OrphanContinuation  bool           `json:"orphan_continuation"`
	Status              string         `json:"status"`
	Decision            Decision       `json:"decision,omitempty"`
	CurrentRound        int            `json:"current_round"`
	CurrentRunKind      string         `json:"current_run_kind"`
	CurrentRequestedBy  *int64         `json:"current_requested_by"`
	ReuseMode           string         `json:"reuse_mode"`
	TriggerHash         string         `json:"trigger_hash,omitempty"`
	TriggerKind         string         `json:"trigger_kind,omitempty"`
	EnforcementEligible bool           `json:"enforcement_eligible"`
	ViolationCounted    bool           `json:"violation_counted"`
	DisableCounted      bool           `json:"disable_counted"`
	ConfigRevision      *int64         `json:"config_revision"`
	Config              ConfigSnapshot `json:"-"`
	Attempts            int            `json:"attempts"`
	MaxAttempts         int            `json:"max_attempts"`
	ClaimGeneration     int64          `json:"-"`
	LeaseUntil          *time.Time     `json:"lease_until"`
	NextAttemptAt       time.Time      `json:"next_attempt_at"`
	FailureStage        string         `json:"failure_stage"`
	LastErrorCode       string         `json:"last_error_code"`
	LastErrorMessage    string         `json:"last_error_message"`
	BlockingWaitMS      *int64         `json:"blocking_wait_ms"`
	// DurationMS 是任务从创建到结束的总耗时，两种模式都有值（未结束为 0）。
	DurationMS  int64      `json:"duration_ms"`
	TargetCount int        `json:"target_count"`
	StartedAt   *time.Time `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	// Dispatched 表示本次执行已经发出真实模型调用，用于区分容量退回和普通失败。
	Dispatched bool `json:"-"`
}

func (j *AuditJob) runKind() string {
	if j.CurrentRunKind == "" {
		return "request"
	}
	return j.CurrentRunKind
}

// JobTarget 是某一轮审核中的一个目标；resolution 为空表示尚未得出结论（失败或已提前结束）。
type JobTarget struct {
	Round        int         `json:"round"`
	Order        int         `json:"order"`
	Hash         string      `json:"hash"`
	Kind         string      `json:"kind"`
	Text         string      `json:"text,omitempty"`
	Bytes        int         `json:"bytes"`
	Resolution   string      `json:"resolution,omitempty"`
	ReviewID     *int64      `json:"-"`
	Decision     Decision    `json:"decision,omitempty"`
	ErrorCode    string      `json:"error_code,omitempty"`
	ErrorMessage string      `json:"error_message,omitempty"`
	ResolvedAt   *time.Time  `json:"resolved_at,omitempty"`
	Trusted      bool        `json:"trusted"`
	Review       *ReviewView `json:"review,omitempty"`
}

// NodeResult 是一个审核节点对单个目标的投票结果。
type NodeResult struct {
	ModelID    string            `json:"model_id"`
	ModelName  string            `json:"model_name"`
	Confidence *float64          `json:"confidence"`
	Reason     string            `json:"reason"`
	AttemptID  *int64            `json:"attempt_id,omitempty"`
	Error      *AuditError       `json:"error,omitempty"`
	Skipped    bool              `json:"skipped,omitempty"`
	SkipReason string            `json:"skip_reason,omitempty"`
	Dispatch   *DispatchSnapshot `json:"dispatch,omitempty"`
}

// PromptReview 是与用户无关、全库复用的目标审核结果。
type PromptReview struct {
	ID          int64        `json:"id"`
	Hash        string       `json:"hash"`
	EvalHash    string       `json:"eval_hash"`
	RunKind     string       `json:"run_kind"`
	Decision    Decision     `json:"decision"`
	Score       *float64     `json:"score"`
	Reason      string       `json:"reason"`
	Nodes       []NodeResult `json:"nodes"`
	Reusable    bool         `json:"reusable"`
	RequestedBy *int64       `json:"requested_by"`
	// OriginJobID 只供内部排障，复用它的任务详情中不得展示。
	OriginJobID *int64    `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
}

// ReviewView 是任务详情中的目标结论视图；复用结论不展示来源任务和他人的模型调用。
type ReviewView struct {
	Decision Decision     `json:"decision"`
	Score    *float64     `json:"score"`
	Reason   string       `json:"reason"`
	Nodes    []NodeResult `json:"nodes"`
	RunKind  string       `json:"run_kind"`
}

// JobRound 是一次全部目标得出结论的成功审核轮次。
type JobRound struct {
	Round          int        `json:"round"`
	RunKind        string     `json:"run_kind"`
	RequestedBy    *int64     `json:"requested_by"`
	ReuseMode      string     `json:"reuse_mode"`
	Decision       Decision   `json:"decision"`
	TriggerHash    string     `json:"trigger_hash,omitempty"`
	TriggerKind    string     `json:"trigger_kind,omitempty"`
	ConfigRevision *int64     `json:"config_revision"`
	StartedAt      *time.Time `json:"started_at"`
	FinishedAt     time.Time  `json:"finished_at"`
}

// TrustEntry 是管理员信任的指令块。
type TrustEntry struct {
	Hash      string    `json:"hash"`
	Preview   string    `json:"preview"`
	Bytes     int       `json:"bytes"`
	TrustedBy int64     `json:"trusted_by"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// BreakerEntry 是指令块熔断记录及其当前处理状态。
type BreakerEntry struct {
	Hash          string     `json:"hash"`
	Preview       string     `json:"preview"`
	Bytes         int        `json:"bytes"`
	Status        string     `json:"status"`
	DistinctUsers int        `json:"distinct_users"`
	TrippedAt     time.Time  `json:"tripped_at"`
	TrippedJobID  *int64     `json:"tripped_job_id"`
	ResolvedBy    *int64     `json:"resolved_by"`
	ResolvedAt    *time.Time `json:"resolved_at"`
}

// RetentionResult 记录一次保留期清理删除的数量，用于运行日志。
type RetentionResult struct {
	Jobs          int64 `json:"jobs"`
	ProbeAttempts int64 `json:"probe_attempts"`
	Reviews       int64 `json:"reviews"`
	Texts         int64 `json:"texts"`
}
