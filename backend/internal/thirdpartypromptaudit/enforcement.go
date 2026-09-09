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

// decideEnforcement 同时返回提醒和停用动作；管理员只保留请求级审核及阻断通知。
func decideEnforcement(state EnforcementState, user EnforcementUser, eligible, triggerBlock bool, config Config, window []Decision) (EnforcementState, []string) {
	next := state
	if !eligible || user.Role != "user" {
		return next, nil
	}
	actions := make([]string, 0, 2)
	if config.Warning.Enabled {
		violations := 0
		for _, decision := range window {
			if decision == DecisionBlock {
				violations++
			}
		}
		if violations < config.Warning.Limit {
			next.WarningArmed = true
		} else if next.WarningArmed && triggerBlock {
			next.WarningArmed = false
			actions = append(actions, "warning")
		}
	}
	if config.Disable.Enabled && user.Status == "active" && triggerBlock && next.DisableViolationCount >= config.Disable.Limit {
		actions = append(actions, "disable")
	}
	return next, actions
}
