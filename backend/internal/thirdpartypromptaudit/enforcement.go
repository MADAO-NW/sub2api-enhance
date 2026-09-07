package thirdpartypromptaudit

import "time"

type EnforcementState struct {
	UserID                      int64      `json:"user_id"`
	WarningRuleHash             string     `json:"warning_rule_hash"`
	WarningWindowAfterOutcomeID int64      `json:"warning_window_after_outcome_id"`
	WarningArmed                bool       `json:"warning_armed"`
	DisableViolationCount       int64      `json:"disable_violation_count"`
	DisableResetAt              *time.Time `json:"disable_reset_at"`
	LastOutcomeID               *int64     `json:"last_outcome_id"`
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
	OutcomeID          *int64           `json:"outcome_id"`
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

// decideEnforcement 只计算当前正式事实带来的状态变化，外部调用在事务提交后执行。
func decideEnforcement(state EnforcementState, user EnforcementUser, job *Job, outcome *Outcome, config Config, window []Decision) (EnforcementState, string) {
	capturedAt := job.CapturedAt
	if capturedAt.IsZero() {
		capturedAt = job.CreatedAt
	}
	next := state
	if !outcome.EnforcementEligible || job.RunKind != "request" {
		return next, ""
	}
	if config.Disable.Enabled && user.Role == "user" && outcome.Decision == DecisionBlock &&
		(state.DisableResetAt == nil || capturedAt.After(*state.DisableResetAt)) {
		next.DisableViolationCount++
	}
	if config.Disable.Enabled && user.Role == "user" && user.Status == "active" && outcome.Decision == DecisionBlock && next.DisableViolationCount >= config.Disable.Limit {
		// 启用重置前捕获的迟到任务也不能仅凭旧累计触发新的停用。
		if state.DisableResetAt == nil || capturedAt.After(*state.DisableResetAt) {
			next.WarningArmed = false
			return next, "disable"
		}
	}
	if config.Warning.Enabled {
		violations := 0
		for _, decision := range window {
			if decision == DecisionBlock {
				violations++
			}
		}
		if violations < config.Warning.Limit {
			next.WarningArmed = true
		} else if next.WarningArmed {
			next.WarningArmed = false
			return next, "warning"
		}
	}
	return next, ""
}
