package thirdpartypromptaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"slices"
	"time"

	"sub2api-enhance/internal/pkg/logger"
)

// Complete 原子提交分类、事件、计数与动作；同步补写和复核不能获得处罚资格。
func (r *Repository) Complete(ctx context.Context, job *Job, evaluation *Evaluation, foreground bool) (*Outcome, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	var generation int64
	var leaseValid bool
	err = tx.QueryRowContext(ctx, `SELECT status,claim_generation,COALESCE(lease_until>clock_timestamp(),false) FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE id=$1 FOR UPDATE`, job.ID).Scan(&status, &generation, &leaseValid)
	if err != nil {
		return nil, err
	}
	if status == "done" {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return r.GetOutcome(ctx, job.ID)
	}
	if status != "processing" || generation != job.ClaimGeneration || !leaseValid {
		return nil, ErrLeaseLost
	}
	current := storedConfig{Config: DefaultConfig()}
	var configRaw string
	err = tx.QueryRowContext(ctx, `SELECT value FROM sub2api_enhance.settings WHERE key=$1 FOR SHARE`, SettingKey).Scan(&configRaw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		current.AuditScope = "full_request"
		if err := json.Unmarshal([]byte(configRaw), &current); err != nil {
			return nil, err
		}
	}
	if err := validateConfig(current.Config, current.Mode != "off"); err != nil {
		return nil, err
	}
	rootID := job.ID
	if job.SourceJobID != nil {
		rootID = *job.SourceJobID
		var validSource bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE id=$1 AND run_kind='request' AND user_id=$2)`, rootID, job.UserID).Scan(&validSource); err != nil {
			return nil, err
		}
		if !validSource {
			return nil, errors.New("复核来源与任务用户不一致")
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.third_party_prompt_audit_enforcement_states(user_id) VALUES($1) ON CONFLICT(user_id) DO NOTHING`, job.UserID); err != nil {
		return nil, err
	}
	state := EnforcementState{UserID: job.UserID}
	err = tx.QueryRowContext(ctx, `SELECT warning_rule_hash,warning_window_after_outcome_id,warning_armed,disable_violation_count,disable_reset_at,last_outcome_id FROM sub2api_enhance.third_party_prompt_audit_enforcement_states WHERE user_id=$1 FOR UPDATE`, job.UserID).Scan(
		&state.WarningRuleHash, &state.WarningWindowAfterOutcomeID, &state.WarningArmed, &state.DisableViolationCount, &state.DisableResetAt, &state.LastOutcomeID)
	if err != nil {
		return nil, err
	}
	user := EnforcementUser{ID: job.UserID, Username: job.Identity.Username, Email: job.Identity.UserEmail}
	err = tx.QueryRowContext(ctx, `SELECT username,email,role,status FROM public.users WHERE id=$1 AND deleted_at IS NULL`, job.UserID).Scan(&user.Username, &user.Email, &user.Role, &user.Status)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var resetting bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions WHERE user_id=$1 AND action_type='counter_reset' AND execution_status IN ('pending','processing','unknown'))`, job.UserID).Scan(&resetting); err != nil {
		return nil, err
	}
	if resetting {
		return nil, errors.New("用户启用及累计重置尚未确认，分类检查点等待收尾")
	}
	eligible := false
	if job.CaptureID != nil {
		if err := tx.QueryRowContext(ctx, `SELECT eligibility_status='passed' AND snapshot_status='complete' FROM sub2api_enhance.captures WHERE id=$1 AND user_id=$2`, *job.CaptureID, job.UserID).Scan(&eligible); err != nil {
			return nil, err
		}
	}
	cloned, err := cloneEvaluation(evaluation)
	if err != nil {
		return nil, err
	}
	runKind := job.auditRunKind()
	cloned.EnforcementEligible = job.IngressStage != "manual_capture_reprocess" && eligible && job.RunKind == "request" && runKind == "request" && (job.ExecutionMode == "async" || foreground) && current.Mode != "off"
	outcome := &Outcome{JobID: job.ID, UserID: job.UserID, Evaluation: *cloned}
	models, err := json.Marshal(cloned.Models)
	if err != nil {
		return nil, err
	}
	configSnapshot, err := json.Marshal(job.Config)
	if err != nil {
		return nil, err
	}
	outcome.AuditRound, outcome.RunKind, outcome.RequestedBy, outcome.Config, outcome.StartedAt = jobAuditRound(job), runKind, job.CurrentRequestedBy, job.Config, job.StartedAt
	err = tx.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.third_party_prompt_audit_outcomes(job_id,user_id,decision,partial_failure,enforcement_eligible,model_results,source_outcome_id,audit_round,run_kind,requested_by,config_snapshot,started_at,finished_at)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,clock_timestamp()) RETURNING id,created_at,finished_at`, job.ID, job.UserID, cloned.Decision, cloned.PartialFailure, cloned.EnforcementEligible, string(models), cloned.SourceOutcomeID,
		jobAuditRound(job), runKind, job.CurrentRequestedBy, string(configSnapshot), job.StartedAt).Scan(&outcome.ID, &outcome.CreatedAt, &outcome.FinishedAt)
	if err != nil {
		return nil, err
	}
	uses := make([]struct {
		ModelID   string `json:"model_id"`
		Order     int    `json:"segment_order"`
		ResultID  int64  `json:"segment_result_id"`
		ReuseKind string `json:"reuse_kind"`
	}, 0)
	for _, model := range outcome.Models {
		for _, segment := range model.Segments {
			uses = append(uses, struct {
				ModelID   string `json:"model_id"`
				Order     int    `json:"segment_order"`
				ResultID  int64  `json:"segment_result_id"`
				ReuseKind string `json:"reuse_kind"`
			}{model.ModelID, segment.Order, segment.Result.ID, segment.ReuseKind})
		}
	}
	if len(uses) > 0 {
		raw, err := json.Marshal(uses)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.third_party_prompt_audit_outcome_segments(outcome_id,model_id,segment_order,segment_result_id,reuse_kind)
          SELECT $1,model_id,segment_order,segment_result_id,reuse_kind FROM json_to_recordset($2::json)
          AS entry(model_id text,segment_order integer,segment_result_id bigint,reuse_kind text)`, outcome.ID, string(raw)); err != nil {
			return nil, err
		}
	}

	var originalOutcomeID *int64
	var priorDecision Decision
	var originalEligible bool
	var disableCounted bool
	priorErr := tx.QueryRowContext(ctx, `SELECT latest.decision,original.enforcement_eligible,event.disable_counted FROM sub2api_enhance.third_party_prompt_audit_events event JOIN sub2api_enhance.third_party_prompt_audit_outcomes latest ON latest.id=event.latest_outcome_id LEFT JOIN sub2api_enhance.third_party_prompt_audit_outcomes original ON original.id=event.original_outcome_id WHERE event.job_id=$1`, rootID).Scan(&priorDecision, &originalEligible, &disableCounted)
	if priorErr != nil && !errors.Is(priorErr, sql.ErrNoRows) {
		return nil, priorErr
	}
	err = tx.QueryRowContext(ctx, `SELECT id,enforcement_eligible FROM sub2api_enhance.third_party_prompt_audit_outcomes WHERE job_id=$1 ORDER BY audit_round,id LIMIT 1`, rootID).Scan(&originalOutcomeID, &originalEligible)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if runKind == "reaudit" || job.Config.StorePassEvents || outcome.Decision != DecisionPass {
		_, err = tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.third_party_prompt_audit_events AS event(job_id,original_outcome_id,latest_outcome_id) VALUES($1,$2,$3) ON CONFLICT(job_id) DO UPDATE SET original_outcome_id=COALESCE(event.original_outcome_id,EXCLUDED.original_outcome_id),latest_outcome_id=CASE WHEN EXISTS(SELECT 1 FROM sub2api_enhance.third_party_prompt_audit_outcomes current WHERE current.id=event.latest_outcome_id AND (current.audit_round,current.id)<($4,$3)) THEN EXCLUDED.latest_outcome_id ELSE event.latest_outcome_id END,updated_at=clock_timestamp()`, rootID, originalOutcomeID, outcome.ID, outcome.AuditRound)
		if err != nil {
			return nil, err
		}
	}
	before := state
	window := make([]Decision, 0)
	canonicalEligible := outcome.EnforcementEligible || (runKind == "reaudit" && originalEligible)
	if canonicalEligible && user.Role == "user" {
		ruleHash, err := fingerprint(struct {
			Rule     WarningConfig
			Revision int64
		}{current.Warning, current.WarningRuleRevision})
		if err != nil {
			return nil, err
		}
		if state.WarningRuleHash != ruleHash {
			state.WarningRuleHash = ruleHash
			state.WarningWindowAfterOutcomeID = 0
			if state.LastOutcomeID != nil {
				state.WarningWindowAfterOutcomeID = *state.LastOutcomeID
			}
			state.WarningArmed = true
		}
		if current.Warning.Enabled {
			rows, err := tx.QueryContext(ctx, `SELECT latest.decision FROM sub2api_enhance.third_party_prompt_audit_jobs root JOIN sub2api_enhance.third_party_prompt_audit_outcomes original ON original.job_id=root.id AND original.audit_round=1 LEFT JOIN sub2api_enhance.third_party_prompt_audit_events event ON event.job_id=root.id JOIN sub2api_enhance.third_party_prompt_audit_outcomes latest ON latest.id=COALESCE(event.latest_outcome_id,original.id) WHERE root.run_kind='request' AND root.user_id=$1 AND original.enforcement_eligible AND original.id>$2 ORDER BY root.id DESC LIMIT $3`, job.UserID, state.WarningWindowAfterOutcomeID, current.Warning.Window)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var decision Decision
				if err := rows.Scan(&decision); err != nil {
					_ = rows.Close()
					return nil, err
				}
				window = append(window, decision)
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
	capturedAt := job.CapturedAt
	if capturedAt.IsZero() {
		capturedAt = job.CreatedAt
	}
	if canonicalEligible && user.Role == "user" && current.Disable.Enabled && (state.DisableResetAt == nil || capturedAt.After(*state.DisableResetAt)) {
		shouldCount := outcome.Decision == DecisionBlock
		if !disableCounted && shouldCount {
			state.DisableViolationCount++
		} else if disableCounted && !shouldCount && state.DisableViolationCount > 0 {
			state.DisableViolationCount--
		}
		disableCounted = shouldCount
	}
	triggerBlock := outcome.Decision == DecisionBlock && (runKind == "request" || priorDecision != DecisionBlock)
	next, actionTypes := decideEnforcement(state, user, canonicalEligible, triggerBlock, current.Config, window)
	if slices.Contains(actionTypes, "disable") {
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions WHERE user_id=$1 AND action_type='disable' AND execution_status IN ('pending','processing','unknown'))`, job.UserID).Scan(&active); err != nil {
			return nil, err
		}
		if active {
			actionTypes = slices.DeleteFunc(actionTypes, func(value string) bool { return value == "disable" })
		}
	}
	oldStatus := user.Status
	if runKind == "request" && outcome.EnforcementEligible && user.Role == "user" {
		next.LastOutcomeID = &outcome.ID
	}
	_, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_enforcement_states SET warning_rule_hash=$2,warning_window_after_outcome_id=$3,warning_armed=$4,disable_violation_count=$5,last_outcome_id=$6,updated_at=clock_timestamp() WHERE user_id=$1`, job.UserID, next.WarningRuleHash, next.WarningWindowAfterOutcomeID, next.WarningArmed, next.DisableViolationCount, next.LastOutcomeID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_events SET disable_counted=$2 WHERE job_id=$1`, rootID, disableCounted); err != nil {
		return nil, err
	}
	for _, actionType := range actionTypes {
		action := Action{UserID: job.UserID, OutcomeID: &outcome.ID, ActionType: actionType, NotificationStatus: "pending", AuthCacheStatus: "not_required", RuleSnapshot: map[string]any{"audit_revision": job.Config.Revision, "enforcement_revision": current.Revision, "warning": current.Warning, "disable": current.Disable, "decision": outcome.Decision}, BusinessSnapshot: map[string]any{"user": user, "old_user_status": oldStatus, "new_user_status": user.Status, "before_state": before, "after_state": next, "window": window}}
		action.ExecutionStatus = "succeeded"
		now := time.Now().UTC()
		action.AppliedAt, action.CompletedAt = &now, &now
		if actionType == "disable" {
			action.ExecutionStatus, action.AppliedAt, action.CompletedAt = "pending", nil, nil
			action.AuthCacheStatus, action.NotificationStatus = "not_observed", "not_required"
			action.BusinessSnapshot["requested_user_status"] = "disabled"
		}
		action.Deliveries = actionDeliveries(action, user, current.AdminEmail)
		if err := insertAction(ctx, tx, &action); err != nil {
			return nil, err
		}
	}
	if foreground && job.ExecutionMode == "blocking" && outcome.EnforcementEligible && outcome.Decision == DecisionBlock {
		now := time.Now().UTC()
		action := Action{UserID: job.UserID, OutcomeID: &outcome.ID, ActionType: "blocking_notice", ExecutionStatus: "succeeded", AppliedAt: &now, CompletedAt: &now,
			NotificationStatus: "pending", AuthCacheStatus: "not_required", RuleSnapshot: map[string]any{"audit_revision": job.Config.Revision, "decision": outcome.Decision},
			BusinessSnapshot: map[string]any{"user": user, "request_id": job.RequestID, "conversation_key": job.ConversationKey}}
		action.Deliveries = actionDeliveries(action, user, current.AdminEmail)
		if len(action.Deliveries) == 0 {
			action.NotificationStatus = "not_required"
		}
		if err := insertAction(ctx, tx, &action); err != nil {
			return nil, err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_jobs SET status='done',finished_at=clock_timestamp(),lease_until=NULL,result_checkpoint=NULL,failure_stage=NULL,last_error_code=NULL,last_error_message=NULL,updated_at=clock_timestamp() WHERE id=$1 AND claim_generation=$2 AND status='processing'`, job.ID, job.ClaimGeneration)
	if err := checkLeaseUpdate(result, err); err != nil {
		return nil, err
	}
	transition, err := json.Marshal(struct{ Before, After EnforcementState }{before, next})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	resultName := map[Decision]string{DecisionPass: "通过", DecisionReview: "待复核", DecisionBlock: "违规"}[outcome.Decision]
	logger.LegacyPrintf("third_party_prompt_audit", "审核分类已提交：%s job_id=%d outcome_id=%d decision=%s state_transition=%s", resultName, job.ID, outcome.ID, outcome.Decision, string(transition))
	return outcome, nil
}

func actionDeliveries(action Action, user EnforcementUser, adminEmail string) []Delivery {
	subject := "第三方提示词审计风险提醒"
	if action.ActionType == "blocking_notice" {
		subject = "第三方提示词审计：请求已阻止"
	} else if action.ActionType == "disable" {
		subject = "第三方提示词审计：账号已停用"
	}
	body := fmt.Sprintf("<p>%s</p><p>用户：%s</p><p>分类记录：%d</p><p>请在管理后台的第三方模型提示词审计页面查看完整依据。</p>", html.EscapeString(subject), html.EscapeString(user.Username), *action.OutcomeID)
	deliveries := []Delivery{}
	if adminEmail != "" {
		deliveries = append(deliveries, Delivery{Recipient: adminEmail, Kind: "admin", Subject: subject, Body: body, Status: "pending", Attempts: []DeliveryAttempt{}})
	}
	if user.Email != "" && user.Email != adminEmail {
		deliveries = append(deliveries, Delivery{Recipient: user.Email, Kind: "user", Subject: subject, Body: body, Status: "pending", Attempts: []DeliveryAttempt{}})
	}
	return deliveries
}

func insertAction(ctx context.Context, tx *sql.Tx, action *Action) error {
	rule, err := json.Marshal(action.RuleSnapshot)
	if err != nil {
		return err
	}
	business, err := json.Marshal(action.BusinessSnapshot)
	if err != nil {
		return err
	}
	deliveries, err := json.Marshal(action.Deliveries)
	if err != nil {
		return err
	}
	return tx.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.third_party_prompt_audit_enforcement_actions(user_id,outcome_id,actor_user_id,action_type,rule_snapshot,business_snapshot,notification_status,deliveries,auth_cache_status,execution_status,applied_at,completed_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id,applied_at`, action.UserID, action.OutcomeID, action.ActorUserID, action.ActionType, string(rule), string(business), action.NotificationStatus, string(deliveries), action.AuthCacheStatus, action.ExecutionStatus, action.AppliedAt, action.CompletedAt).Scan(&action.ID, &action.AppliedAt)
}
