package thirdpartypromptaudit

import "time"

type EnforcementState struct {
	UserID                  int64      `json:"user_id"`
	WarningRuleHash         string     `json:"warning_rule_hash"`
	WarningWindowAfterJobID int64      `json:"warning_window_after_job_id"`
	WarningArmed            bool       `json:"warning_armed"`
	DisableViolationCount   int64      `json:"disable_violation_count"`
	DisableResetAt          *time.Time `json:"disable_reset_at"`
	LastJobID               *int64     `json:"last_job_id"`
	// EscalatedAt 非空表示该用户已自动加严为同步审核。
	EscalatedAt    *time.Time `json:"escalated_at"`
	EscalatedJobID *int64     `json:"escalated_job_id"`
}

type EnforcementUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	Status   string `json:"status"`
}

type DeliveryAttempt struct {
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Status     string     `json:"status"`
	Error      string     `json:"error"`
}

type Delivery struct {
	Recipient string            `json:"recipient"`
	Kind      string            `json:"kind"`
	Subject   string            `json:"subject"`
	Body      string            `json:"body"`
	Status    string            `json:"status"`
	Attempts  []DeliveryAttempt `json:"attempts"`
	LastError string            `json:"last_error"`
}

type Action struct {
	ExecutionStatus    string           `json:"execution_status"`
	AttemptHistory     []AccountAttempt `json:"attempt_history"`
	RequestedAt        time.Time        `json:"requested_at"`
	CompletedAt        *time.Time       `json:"completed_at"`
	ID                 int64            `json:"id"`
	UserID             int64            `json:"user_id"`
	JobID              *int64           `json:"job_id"`
	ActorUserID        *int64           `json:"actor_user_id"`
	ActionType         string           `json:"action_type"`
	RuleSnapshot       map[string]any   `json:"rule_snapshot"`
	BusinessSnapshot   map[string]any   `json:"business_snapshot"`
	AppliedAt          *time.Time       `json:"applied_at"`
	NotificationStatus string           `json:"notification_status"`
	Deliveries         []Delivery       `json:"deliveries"`
	AuthCacheStatus    string           `json:"auth_cache_status"`
	AuthCacheError     string           `json:"auth_cache_error"`
	NextAttemptAt      time.Time        `json:"next_attempt_at"`
	ClaimGeneration    int64            `json:"claim_generation"`
	LeaseUntil         *time.Time       `json:"lease_until"`
	UpdatedAt          time.Time        `json:"updated_at"`
}

// enforcementInput 汇总一次裁决所需的规则、用户状态与类别处置级别。
type enforcementInput struct {
	State        EnforcementState
	User         EnforcementUser
	Eligible     bool
	TriggerBlock bool
	// Mode 是本任务冻结的处理方式，用于判断加严是否从异步开始。
	Mode        string
	Config      Config
	Window      []Decision
	Disposition string
}

// decideEnforcement 同时返回提醒、加严和停用动作；原版账号停用仍只适用于普通用户。
func decideEnforcement(input enforcementInput) (EnforcementState, []string) {
	next := input.State
	if !input.Eligible {
		return next, nil
	}
	actions := make([]string, 0, 3)
	if input.Config.Warning.Enabled {
		violations := 0
		for _, decision := range input.Window {
			if decision == DecisionBlock {
				violations++
			}
		}
		if violations < input.Config.Warning.Limit {
			next.WarningArmed = true
		} else if next.WarningArmed && input.TriggerBlock {
			next.WarningArmed = false
			actions = append(actions, "warning")
			// 自动加严只对异步审核用户生效，且同一用户只加严一次。
			if input.Config.EscalateOnWarning && input.Mode == "async" && next.EscalatedAt == nil {
				actions = append(actions, "escalation")
			}
		}
	}
	disable := input.User.Role == "user" && input.Config.Disable.Enabled && input.User.Status == "active" && input.TriggerBlock &&
		next.DisableViolationCount >= input.Config.Disable.Limit
	if input.Disposition == CategoryDispositionImmediateDisable {
		// 标记为立即停用的类别不必等累计达到全局阈值。
		disable = input.User.Role == "user" && input.Config.Disable.Enabled && input.User.Status == "active" && input.TriggerBlock
	}
	if input.Disposition == CategoryDispositionNoticeOnly {
		disable = false
	}
	if disable {
		actions = append(actions, "disable")
	}
	return next, actions
}
