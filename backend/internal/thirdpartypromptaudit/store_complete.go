package thirdpartypromptaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"slices"
	"strings"
	"time"

	"sub2api-enhance/internal/pkg/logger"
)

// CompletionResult 是一次成功审核轮次提交后的结果。
type CompletionResult struct {
	JobID            int64    `json:"job_id"`
	Round            int      `json:"round"`
	Decision         Decision `json:"decision"`
	TriggerHash      string   `json:"trigger_hash,omitempty"`
	TriggerKind      string   `json:"trigger_kind,omitempty"`
	ViolationCounted bool     `json:"violation_counted"`
	TrippedBreakers  []string `json:"tripped_breakers,omitempty"`
}

func decisionSeverity(decision Decision) int {
	switch decision {
	case DecisionBlock:
		return 3
	case DecisionReview:
		return 2
	case DecisionPass:
		return 1
	default:
		return 0
	}
}

// roundDecision 按 block > review > pass 取各目标中最严重的结论；已有 block 时即可确定，
// 否则必须全部目标得出结论。触发目标在同等严重程度下优先标记用户提示词。
func roundDecision(targets []JobTarget) (Decision, *JobTarget, bool) {
	var trigger *JobTarget
	unresolved := false
	for i := range targets {
		target := &targets[i]
		if target.Resolution == "" {
			unresolved = true
			continue
		}
		if trigger == nil || decisionSeverity(target.Decision) > decisionSeverity(trigger.Decision) ||
			(decisionSeverity(target.Decision) == decisionSeverity(trigger.Decision) && target.Kind == PromptKindUser && trigger.Kind != PromptKindUser) {
			trigger = target
		}
	}
	if trigger != nil && trigger.Decision == DecisionBlock {
		return DecisionBlock, trigger, true
	}
	if unresolved || trigger == nil {
		return "", nil, false
	}
	return trigger.Decision, trigger, true
}

// CompleteAuditJob 在一个事务内提交审核轮次、任务当前结论、累计与处置动作；账号 API 和邮件在事务后执行。
func (r *Repository) CompleteAuditJob(ctx context.Context, job *AuditJob, foreground bool) (*CompletionResult, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	var generation int64
	var leaseValid bool
	err = tx.QueryRowContext(ctx, `SELECT status,claim_generation,COALESCE(lease_until>clock_timestamp(),false) FROM sub2api_enhance.audit_jobs WHERE id=$1 FOR UPDATE`, job.ID).Scan(&status, &generation, &leaseValid)
	if err != nil {
		return nil, err
	}
	if status != "processing" || generation != job.ClaimGeneration || !leaseValid {
		return nil, ErrLeaseLost
	}
	targets, err := loadRoundTargetsTx(ctx, tx, job.ID, job.CurrentRound)
	if err != nil {
		return nil, err
	}
	decision, trigger, ready := roundDecision(targets)
	if !ready {
		return nil, errors.New("审核目标尚未全部得出结论")
	}
	current := storedConfig{Config: DefaultConfig()}
	var configRaw string
	err = tx.QueryRowContext(ctx, `SELECT value FROM sub2api_enhance.settings WHERE key=$1 FOR SHARE`, SettingKey).Scan(&configRaw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal([]byte(configRaw), &current); err != nil {
			return nil, err
		}
	}
	configuredRules := current.Config
	currentlyExcluded := slices.Contains(current.ExcludedUserIDs, job.UserID)
	current.Config = effectiveConfigForUser(current.Config, job.UserID)
	if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.third_party_prompt_audit_enforcement_states(user_id) VALUES($1) ON CONFLICT(user_id) DO NOTHING`, job.UserID); err != nil {
		return nil, err
	}
	state := EnforcementState{UserID: job.UserID}
	if err := tx.QueryRowContext(ctx, `SELECT warning_rule_hash,warning_window_after_job_id,warning_armed,disable_violation_count,disable_reset_at,last_job_id
 FROM sub2api_enhance.third_party_prompt_audit_enforcement_states WHERE user_id=$1 FOR UPDATE`, job.UserID).Scan(
		&state.WarningRuleHash, &state.WarningWindowAfterJobID, &state.WarningArmed, &state.DisableViolationCount, &state.DisableResetAt, &state.LastJobID); err != nil {
		return nil, err
	}
	user := EnforcementUser{ID: job.UserID, Username: job.Identity.Username, Email: job.Identity.UserEmail}
	if err := tx.QueryRowContext(ctx, `SELECT username,email,role,status FROM public.users WHERE id=$1 AND deleted_at IS NULL`, job.UserID).Scan(&user.Username, &user.Email, &user.Role, &user.Status); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var resetting bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions WHERE user_id=$1 AND action_type='counter_reset' AND execution_status IN ('pending','processing','unknown'))`, job.UserID).Scan(&resetting); err != nil {
		return nil, err
	}
	if resetting {
		return nil, errors.New("用户启用及累计重置尚未确认，审核结论等待收尾")
	}
	runKind := job.runKind()
	result := &CompletionResult{JobID: job.ID, Round: job.CurrentRound, Decision: decision, TriggerHash: trigger.Hash, TriggerKind: trigger.Kind}
	configSnapshot, err := json.Marshal(job.Config)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.audit_job_rounds(job_id,round,run_kind,requested_by,reuse_mode,decision,trigger_hash,trigger_kind,config_revision,config_snapshot,started_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, job.ID, job.CurrentRound, runKind, job.CurrentRequestedBy, job.ReuseMode, decision, trigger.Hash, trigger.Kind,
		job.Config.Revision, string(configSnapshot), job.StartedAt); err != nil {
		return nil, err
	}
	// 首次处置资格只由业务请求的首轮建立；重新审核沿用首轮资格，且本身不触发新处置。
	eligible := job.EnforcementEligible
	violation := job.ViolationCounted
	disableCounted := job.DisableCounted
	var instructions instructionOutcome
	if runKind == "request" {
		eligible = (job.Mode == "async" || foreground) && current.Mode != "off" && !currentlyExcluded
		violation = false
		if eligible && decision == DecisionBlock {
			blocked := make([]string, 0)
			userBlocked := false
			for _, target := range targets {
				if target.Decision != DecisionBlock {
					continue
				}
				if target.Kind == PromptKindUser {
					userBlocked = true
				} else {
					blocked = append(blocked, target.Hash)
				}
			}
			instructions, err = applyInstructionViolations(ctx, sqlInstructionLedger{tx: tx}, job.UserID, job.ID, blocked)
			if err != nil {
				return nil, err
			}
			// 计入条件：用户提示词违规，或包含本用户尚未计过且未被熔断暂停的违规指令块。
			violation = userBlocked || len(instructions.Countable) > 0
		}
	}
	result.ViolationCounted = violation
	result.TrippedBreakers = instructions.Tripped
	createdAt := job.CreatedAt
	if user.Role == "user" && (state.DisableResetAt == nil || createdAt.After(*state.DisableResetAt)) {
		if runKind == "request" && eligible && violation && current.Disable.Enabled && !disableCounted {
			state.DisableViolationCount, disableCounted = state.DisableViolationCount+1, true
		} else if runKind == "reaudit" && disableCounted && decision != DecisionBlock {
			// 重新审核只修正已有累计贡献（违规改判为非违规时撤回），不新增累计。
			state.DisableViolationCount, disableCounted = max(state.DisableViolationCount-1, 0), false
		}
	}
	update, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.audit_jobs SET status='done',decision=$3,trigger_hash=$4,trigger_kind=$5,enforcement_eligible=$6,
 violation_counted=$7,disable_counted=$8,finished_at=clock_timestamp(),lease_until=NULL,failure_stage=NULL,last_error_code=NULL,last_error_message=NULL,updated_at=clock_timestamp()
 WHERE id=$1 AND claim_generation=$2 AND status='processing'`, job.ID, job.ClaimGeneration, decision, trigger.Hash, trigger.Kind, eligible, violation, disableCounted)
	if err := checkLeaseUpdate(update, err); err != nil {
		return nil, err
	}
	before := state
	window := make([]Decision, 0)
	if eligible && !currentlyExcluded {
		ruleHash, err := warningRuleHash(configuredRules, current.WarningRuleRevision, job.UserID)
		if err != nil {
			return nil, err
		}
		if state.WarningRuleHash != ruleHash {
			state.WarningRuleHash = ruleHash
			state.WarningWindowAfterJobID = 0
			if state.LastJobID != nil {
				state.WarningWindowAfterJobID = *state.LastJobID
			}
			state.WarningArmed = true
		}
		if current.Warning.Enabled {
			// 提醒窗口统计首轮可处置任务的最新一轮裁决，且只算计入过的违规。
			rows, err := tx.QueryContext(ctx, `SELECT violation_counted AND decision='block' FROM sub2api_enhance.audit_jobs
 WHERE user_id=$1 AND enforcement_eligible AND status='done' AND id>$2 ORDER BY id DESC LIMIT $3`, job.UserID, state.WarningWindowAfterJobID, current.Warning.Window)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var counted bool
				if err := rows.Scan(&counted); err != nil {
					_ = rows.Close()
					return nil, err
				}
				if counted {
					window = append(window, DecisionBlock)
				} else {
					window = append(window, DecisionPass)
				}
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if err := rows.Close(); err != nil {
				return nil, err
			}
		}
	}
	triggerBlock := runKind == "request" && violation
	next, actionTypes := decideEnforcement(state, user, eligible && !currentlyExcluded, triggerBlock, current.Config, window)
	if slices.Contains(actionTypes, "disable") {
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions WHERE user_id=$1 AND action_type='disable' AND execution_status IN ('pending','processing','unknown'))`, job.UserID).Scan(&active); err != nil {
			return nil, err
		}
		if active {
			actionTypes = slices.DeleteFunc(actionTypes, func(value string) bool { return value == "disable" })
		}
	}
	if runKind == "request" && eligible {
		next.LastJobID = &job.ID
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_enforcement_states SET warning_rule_hash=$2,warning_window_after_job_id=$3,warning_armed=$4,
 disable_violation_count=$5,last_job_id=$6,updated_at=clock_timestamp() WHERE user_id=$1`, job.UserID, next.WarningRuleHash, next.WarningWindowAfterJobID, next.WarningArmed, next.DisableViolationCount, next.LastJobID); err != nil {
		return nil, err
	}
	for _, actionType := range actionTypes {
		now := time.Now().UTC()
		action := Action{UserID: job.UserID, JobID: &job.ID, ActionType: actionType, NotificationStatus: "pending", AuthCacheStatus: "not_required", ExecutionStatus: "succeeded",
			AppliedAt: &now, CompletedAt: &now,
			RuleSnapshot:     map[string]any{"audit_revision": job.Config.Revision, "enforcement_revision": current.Revision, "warning": current.Warning, "disable": current.Disable, "decision": decision},
			BusinessSnapshot: map[string]any{"user": user, "old_user_status": user.Status, "new_user_status": user.Status, "before_state": before, "after_state": next, "window": window, "trigger_hash": trigger.Hash, "trigger_kind": trigger.Kind}}
		if actionType == "disable" {
			action.ExecutionStatus, action.AppliedAt, action.CompletedAt = "pending", nil, nil
			action.AuthCacheStatus, action.NotificationStatus = "not_observed", "not_required"
			action.BusinessSnapshot["requested_user_status"] = "disabled"
		}
		action.Deliveries = jobActionDeliveries(action, user, current.AdminEmail)
		if err := insertAction(ctx, tx, &action); err != nil {
			return nil, err
		}
	}
	if runKind == "request" && foreground && job.Mode == "blocking" && eligible && decision == DecisionBlock {
		now := time.Now().UTC()
		action := Action{UserID: job.UserID, JobID: &job.ID, ActionType: "blocking_notice", ExecutionStatus: "succeeded", AppliedAt: &now, CompletedAt: &now,
			NotificationStatus: "pending", AuthCacheStatus: "not_required", RuleSnapshot: map[string]any{"audit_revision": job.Config.Revision, "decision": decision},
			BusinessSnapshot: map[string]any{"user": user, "request_id": job.RequestID, "trigger_hash": trigger.Hash, "trigger_kind": trigger.Kind}}
		action.Deliveries = jobActionDeliveries(action, user, current.AdminEmail)
		if len(action.Deliveries) == 0 {
			action.NotificationStatus = "not_required"
		}
		if err := insertAction(ctx, tx, &action); err != nil {
			return nil, err
		}
	}
	if len(instructions.Tripped) > 0 {
		// 熔断通知只发管理员邮箱；未配置时只在页面显示。
		now := time.Now().UTC()
		action := Action{UserID: job.UserID, JobID: &job.ID, ActionType: "breaker_notice", ExecutionStatus: "succeeded", AppliedAt: &now, CompletedAt: &now,
			NotificationStatus: "pending", AuthCacheStatus: "not_required", RuleSnapshot: map[string]any{"threshold": instructionBreakerUserThreshold},
			BusinessSnapshot: map[string]any{"user": user, "hashes": instructions.Tripped}}
		action.Deliveries = jobActionDeliveries(action, user, current.AdminEmail)
		if len(action.Deliveries) == 0 {
			action.NotificationStatus = "not_required"
		}
		if err := insertAction(ctx, tx, &action); err != nil {
			return nil, err
		}
	}
	transition, err := json.Marshal(struct {
		Before, After EnforcementState
		Instructions  instructionOutcome
	}{before, next, instructions})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	logger.LegacyPrintf("third_party_prompt_audit", "审核轮次已提交 job_id=%d round=%d run_kind=%s decision=%s trigger_kind=%s violation_counted=%t state_transition=%s",
		job.ID, job.CurrentRound, runKind, decision, trigger.Kind, violation, string(transition))
	return result, nil
}

func loadRoundTargetsTx(ctx context.Context, tx *sql.Tx, jobID int64, round int) ([]JobTarget, error) {
	rows, err := tx.QueryContext(ctx, `SELECT target_order,hash,kind,COALESCE(resolution,''),COALESCE(decision,'') FROM sub2api_enhance.audit_job_targets
 WHERE job_id=$1 AND round=$2 ORDER BY target_order`, jobID, round)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	targets := make([]JobTarget, 0)
	for rows.Next() {
		target := JobTarget{Round: round}
		var decision string
		if err := rows.Scan(&target.Order, &target.Hash, &target.Kind, &target.Resolution, &decision); err != nil {
			return nil, err
		}
		target.Decision = Decision(decision)
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

// jobActionDeliveries 生成动作通知；熔断通知只发管理员，其余动作同时通知被审核用户。
func jobActionDeliveries(action Action, user EnforcementUser, adminEmail string) []Delivery {
	subject := "第三方提示词审计：违规提醒"
	switch action.ActionType {
	case "blocking_notice":
		subject = "第三方提示词审计：请求已阻止"
	case "disable":
		subject = "第三方提示词审计：账号已停用"
	case "breaker_notice":
		subject = "第三方提示词审计：指令块熔断待确认"
	}
	jobID := int64(0)
	if action.JobID != nil {
		jobID = *action.JobID
	}
	body := fmt.Sprintf("<p>%s</p><p>用户：%s</p><p>审核任务：%d</p><p>请在管理后台的第三方模型提示词审计页面查看完整依据。</p>", html.EscapeString(subject), html.EscapeString(user.Username), jobID)
	deliveries := []Delivery{}
	if action.ActionType == "breaker_notice" {
		hashes, _ := action.BusinessSnapshot["hashes"].([]string)
		body = fmt.Sprintf("<p>%s</p><p>同一指令块已累计让 %d 个不同用户违规，已暂停该指令块的累计和停用，blocking 模式仍会阻断。</p><p>指令块哈希：%s</p><p>触发任务：%d</p><p>请在熔断列表中选择“信任”或“确认违规”。</p>",
			html.EscapeString(subject), instructionBreakerUserThreshold, html.EscapeString(strings.Join(hashes, "、")), jobID)
		if adminEmail != "" {
			deliveries = append(deliveries, Delivery{Recipient: adminEmail, Kind: "admin", Subject: subject, Body: body, Status: "pending", Attempts: []DeliveryAttempt{}})
		}
		return deliveries
	}
	if adminEmail != "" {
		deliveries = append(deliveries, Delivery{Recipient: adminEmail, Kind: "admin", Subject: subject, Body: body, Status: "pending", Attempts: []DeliveryAttempt{}})
	}
	if user.Email != "" && user.Email != adminEmail {
		deliveries = append(deliveries, Delivery{Recipient: user.Email, Kind: "user", Subject: subject, Body: body, Status: "pending", Attempts: []DeliveryAttempt{}})
	}
	return deliveries
}
