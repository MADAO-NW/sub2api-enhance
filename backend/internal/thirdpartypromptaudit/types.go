package thirdpartypromptaudit

import (
	"encoding/json"
	"fmt"
	"time"
)

// SettingKey 是本模块独立配置的持久化键。
const SettingKey = "third_party_prompt_audit_config"

// MaxEvaluationAttempts 是每个新任务的模型评估预算，结果补写不消耗该预算。
const MaxEvaluationAttempts = 3

// 审核轮次的复用方式：允许复用历史结论、只复用本次重审发起之后产生的结论。
// force 只保留给升级前的历史轮次展示。
const (
	ReuseModeAllow = "allow"
	ReuseModeForce = "force"
	ReuseModeFresh = "fresh"
)

// 审核轮次类型：原始请求与管理员发起的重新审核。
const (
	RunKindRequest = "request"
	RunKindReaudit = "reaudit"
)

type Decision string

const (
	DecisionPass   Decision = "pass"
	DecisionReview Decision = "review"
	DecisionBlock  Decision = "block"
)

type Identity struct {
	Username   string `json:"username"`
	UserEmail  string `json:"user_email"`
	APIKeyName string `json:"api_key_name"`
	GroupName  string `json:"group_name"`
	Endpoint   string `json:"endpoint"`
}

// DispatchSnapshot 记录一次真实调用前的节点调度状态，用于任务详情排障。
type DispatchSnapshot struct {
	Order          int      `json:"order"`
	Health         string   `json:"health"`
	Active         int      `json:"active"`
	MaxConcurrency int      `json:"max_concurrency"`
	Waiting        int      `json:"waiting"`
	LoadRatio      float64  `json:"load_ratio"`
	LatencyEWMAMS  *float64 `json:"latency_ewma_ms,omitempty"`
	Reason         string   `json:"reason"`
}

// ModelAttempt 是一次真实模型调用流水；只在真实发出 HTTP 请求时写入。
type ModelAttempt struct {
	ID                int64           `json:"id"`
	JobID             *int64          `json:"job_id"`
	CallKind          string          `json:"call_kind"`
	EvaluationRound   *int            `json:"evaluation_round"`
	AuditRound        int             `json:"audit_round"`
	TargetHash        string          `json:"target_hash,omitempty"`
	ModelID           string          `json:"model_id"`
	ModelSnapshot     ModelConfig     `json:"model_snapshot"`
	Stage             string          `json:"stage"`
	RepairOfAttemptID *int64          `json:"repair_of_attempt_id"`
	RequestMetadata   json.RawMessage `json:"request_metadata"`
	Status            string          `json:"status"`
	HTTPStatus        *int            `json:"http_status"`
	RawResponse       *string         `json:"raw_response"`
	Result            *Score          `json:"result"`
	InputTokens       *int64          `json:"input_tokens"`
	OutputTokens      *int64          `json:"output_tokens"`
	LatencyMS         *int64          `json:"latency_ms"`
	ErrorCode         string          `json:"error_code"`
	ErrorMessage      string          `json:"error_message"`
	CreatedAt         time.Time       `json:"created_at"`
	DispatchStartedAt *time.Time      `json:"dispatch_started_at"`
	FinishedAt        *time.Time      `json:"finished_at"`
}

type AuditError struct {
	Code       string        `json:"code"`
	Message    string        `json:"message"`
	Stage      string        `json:"stage"`
	Retryable  bool          `json:"retryable"`
	RetryAfter time.Duration `json:"-"`
}

func (e *AuditError) Error() string { return fmt.Sprintf("%s · %s", e.Message, e.Code) }

// Filter 是任务列表与重新审核共用的过滤条件；字段都对应 audit_jobs 的真实列。
type Filter struct {
	IDs       []int64    `json:"ids,omitempty"`
	From      *time.Time `json:"from,omitempty"`
	To        *time.Time `json:"to,omitempty"`
	UserID    *int64     `json:"user_id,omitempty"`
	APIKeyID  *int64     `json:"api_key_id,omitempty"`
	GroupID   *int64     `json:"group_id,omitempty"`
	Status    string     `json:"status,omitempty"`
	Decision  Decision   `json:"decision,omitempty"`
	RunKind   string     `json:"run_kind,omitempty"`
	Mode      string     `json:"mode,omitempty"`
	Platform  string     `json:"platform,omitempty"`
	RequestID string     `json:"request_id,omitempty"`
	Model     string     `json:"model,omitempty"`
	// Category 按触发目标的违规类别筛选，如 R07。
	Category string `json:"category,omitempty"`
}

type Page[T any] struct {
	Items    []T   `json:"items"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
}

type ReauditRequest struct {
	Filter Filter `json:"filter"`
}

type ReauditItem struct {
	JobID  int64  `json:"job_id"`
	Status string `json:"status"`
	Reason string `json:"reason"`
	userID int64
}

type ReauditResult struct {
	Matched int64 `json:"matched"`
	Ready   int64 `json:"ready"`
	// EstimatedTargets 是就绪任务按哈希去重、排除已信任指令块后的目标数，用于估算真实调用量。
	EstimatedTargets int64         `json:"estimated_targets"`
	Items            []ReauditItem `json:"items"`
}
