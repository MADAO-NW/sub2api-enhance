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

type Job struct {
	CapturedAt         time.Time       `json:"captured_at"`
	CaptureID          *int64          `json:"capture_id"`
	ID                 int64           `json:"id"`
	CaptureKey         string          `json:"-"`
	RunKind            string          `json:"run_kind"`
	CurrentRunKind     string          `json:"current_run_kind"`
	AuditRound         int             `json:"audit_round"`
	CurrentRequestedBy *int64          `json:"current_requested_by"`
	SourceJobID        *int64          `json:"source_job_id"`
	RequestedBy        *int64          `json:"requested_by"`
	UserID             int64           `json:"user_id"`
	APIKeyID           *int64          `json:"api_key_id"`
	GroupID            *int64          `json:"group_id"`
	RequestID          string          `json:"request_id"`
	ConversationKey    string          `json:"conversation_key,omitempty"`
	Identity           Identity        `json:"identity"`
	Platform           string          `json:"platform"`
	Protocol           string          `json:"protocol"`
	IngressStage       string          `json:"ingress_stage"`
	RequestedModel     string          `json:"requested_model"`
	ExecutionMode      string          `json:"execution_mode"`
	Config             ConfigSnapshot  `json:"config_snapshot,omitempty"`
	DecisionConfig     *DecisionConfig `json:"decision_config,omitempty"`
	FullInput          *InputSnapshot  `json:"full_input_snapshot,omitempty"`
	SnapshotStatus     string          `json:"snapshot_status"`
	Manifest           []SegmentMeta   `json:"input_manifest,omitempty"`
	InputHash          string          `json:"input_hash"`
	TargetHash         string          `json:"target_hash"`
	EvaluationHash     string          `json:"evaluation_hash"`
	Status             string          `json:"status"`
	Attempts           int             `json:"attempts"`
	MaxAttempts        int             `json:"max_attempts"`
	ClaimGeneration    int64           `json:"claim_generation"`
	LeaseUntil         *time.Time      `json:"lease_until"`
	NextAttemptAt      time.Time       `json:"next_attempt_at"`
	Checkpoint         *Evaluation     `json:"result_checkpoint,omitempty"`
	Reuse              ReuseMetrics    `json:"reuse_metrics"`
	FailureStage       string          `json:"failure_stage"`
	LastErrorCode      string          `json:"last_error_code"`
	LastErrorMessage   string          `json:"last_error_message"`
	GatewayResult      string          `json:"gateway_result"`
	GatewayCompleted   *time.Time      `json:"gateway_completed_at"`
	GatewayDurationMS  *int64          `json:"gateway_duration_ms"`
	StartedAt          *time.Time      `json:"started_at"`
	FinishedAt         *time.Time      `json:"finished_at"`
	DurationMS         *int64          `json:"duration_ms"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

func (j *Job) auditRunKind() string {
	if j.CurrentRunKind != "" {
		return j.CurrentRunKind
	}
	if j.RunKind == "reaudit" {
		return "reaudit"
	}
	return "request"
}

type ReuseMetrics struct {
	WholeLookups   int `json:"whole_lookups"`
	WholeHits      int `json:"whole_hits"`
	SegmentLookups int `json:"segment_lookups"`
	SegmentHits    int `json:"segment_hits"`
	WithinJobHits  int `json:"within_job_hits"`
	InflightHits   int `json:"inflight_hits"`
	ShortCircuited int `json:"short_circuited_nodes"`
}

type SegmentResult struct {
	ID              int64  `json:"id"`
	UserID          int64  `json:"user_id"`
	ModelID         string `json:"model_id"`
	AuditKey        string `json:"audit_key"`
	SourceAttemptID int64  `json:"source_attempt_id"`
	SourceRole      string `json:"source_role"`
	PolicyRole      string `json:"policy_role"`
	TurnScope       string `json:"turn_scope"`
	ContentHash     string `json:"content_hash"`
	Score
}

type SegmentUse struct {
	Order      int           `json:"order"`
	SourcePath string        `json:"source_path"`
	ReuseKind  string        `json:"reuse_kind"`
	Result     SegmentResult `json:"result"`
}

type ModelResult struct {
	Reused         bool         `json:"reused"`
	ModelID        string       `json:"model_id"`
	ModelName      string       `json:"model_name"`
	Decision       Decision     `json:"decision,omitempty"`
	Basis          string       `json:"basis"`
	Confidence     *float64     `json:"confidence"`
	Reason         string       `json:"reason"`
	JointAttemptID *int64       `json:"joint_attempt_id"`
	Segments       []SegmentUse `json:"segments"`
	Error          *AuditError  `json:"error,omitempty"`
	Skipped        bool         `json:"skipped,omitempty"`
	SkipReason     string       `json:"skip_reason,omitempty"`
}

type Evaluation struct {
	Decision            Decision      `json:"decision"`
	Models              []ModelResult `json:"models"`
	PartialFailure      bool          `json:"partial_failure"`
	SourceOutcomeID     *int64        `json:"source_outcome_id"`
	EnforcementEligible bool          `json:"enforcement_eligible"`
}

type Outcome struct {
	ID          int64          `json:"id"`
	JobID       int64          `json:"job_id"`
	UserID      int64          `json:"user_id"`
	AuditRound  int            `json:"audit_round"`
	RunKind     string         `json:"run_kind"`
	RequestedBy *int64         `json:"requested_by"`
	Config      ConfigSnapshot `json:"config_snapshot"`
	Evaluation
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	DurationMS *int64     `json:"duration_ms"`
	CreatedAt  time.Time  `json:"created_at"`
}

type Event struct {
	ReauditStatus     string       `json:"reaudit_status"`
	ID                int64        `json:"id"`
	JobID             int64        `json:"job_id"`
	OriginalOutcomeID *int64       `json:"original_outcome_id"`
	LatestOutcomeID   int64        `json:"latest_outcome_id"`
	Job               *Job         `json:"job,omitempty"`
	Latest            *OutcomeView `json:"latest,omitempty"`
	Original          *OutcomeView `json:"original,omitempty"`
	CreatedAt         time.Time    `json:"created_at"`
	UpdatedAt         time.Time    `json:"updated_at"`
}

type ModelAttempt struct {
	ID                int64           `json:"id"`
	JobID             *int64          `json:"job_id"`
	CallKind          string          `json:"call_kind"`
	EvaluationRound   *int            `json:"evaluation_round"`
	AuditRound        int             `json:"audit_round"`
	ModelID           string          `json:"model_id"`
	ModelSnapshot     ModelConfig     `json:"model_snapshot"`
	Stage             string          `json:"stage"`
	SegmentOrder      *int            `json:"segment_order"`
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
	Keyword   string     `json:"keyword,omitempty"`
	ModelID   string     `json:"model_id,omitempty"`
}

type Page[T any] struct {
	Items    []T   `json:"items"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
}

type ReauditRequest struct {
	Source string `json:"source" binding:"required,oneof=jobs events"`
	Filter Filter `json:"filter"`
}

type ReauditItem struct {
	SourceJobID int64  `json:"source_job_id"`
	JobID       *int64 `json:"job_id"`
	Status      string `json:"status"`
	Reason      string `json:"reason"`
}

type ReauditResult struct {
	Matched int64         `json:"matched"`
	Ready   int64         `json:"ready"`
	Items   []ReauditItem `json:"items"`
}
