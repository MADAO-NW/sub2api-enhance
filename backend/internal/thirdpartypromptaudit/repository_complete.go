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

// Complete 原子提交分类、任务当前状态、计数与动作；自动来源首次恢复可建立处罚资格，人工补写不能。
func (r *Repository) Complete(ctx context.Context, job *Job, evaluation *Evaluation, foreground bool) (*Outcome, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	var generation int64
	var leaseValid bool
	var disableCounted bool
	err = tx.QueryRowContext(ctx, `SELECT status,claim_generation,COALESCE(lease_until>clock_timestamp(),false),disable_counted FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE id=$1 FOR UPDATE`, job.ID).Scan(&status, &generation, &leaseValid, &disableCounted)
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
		current.AuditScope = "current_user"
		if err := json.Unmarshal([]byte(configRaw), &current); err != nil {
			return nil, err
		}
	}
	if err := validateConfig(current.Config, current.Mode != "off"); err != nil {
		return nil, err
	}
	configuredRules := current.Config
	currentlyExcluded := slices.Contains(current.ExcludedUserIDs, job.UserID)
	current.Config = effectiveConfigForUser(current.Config, job.UserID)
	var priorDecision Decision
	priorErr := tx.QueryRowContext(ctx, `SELECT decision FROM sub2api_enhance.third_party_prompt_audit_outcomes WHERE job_id=$1 ORDER BY audit_round DESC,id DESC LIMIT 1`, job.ID).Scan(&priorDecision)
	if priorErr != nil && !errors.Is(priorErr, sql.ErrNoRows) {
		return nil, priorErr
	}
	var originalEligible bool
	originalErr := tx.QueryRowContext(ctx, `SELECT enforcement_eligible FROM sub2api_enhance.third_party_prompt_audit_outcomes WHERE job_id=$1 ORDER BY audit_round,id LIMIT 1`, job.ID).Scan(&originalEligible)
	if originalErr != nil && !errors.Is(originalErr, sql.ErrNoRows) {
		return nil, originalErr
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
		if err := tx.QueryRowContext(ctx, `SELECT eligibility_status='passed' AND snapshot_status='complete'
		 AND COALESCE(request_metadata::json->>'audit_required','false')='true'
		 AND COALESCE(request_metadata::json->>'manual_reprocess','false')<>'true'
		 FROM sub2api_enhance.captures WHERE id=$1 AND user_id=$2`, *job.CaptureID, job.UserID).Scan(&eligible); err != nil {
			return nil, err
		}
	}
	cloned, err := cloneEvaluation(evaluation)
	if err != nil {
		return nil, err
	}
	runKind := job.auditRunKind()
	firstRecoveryOutcome := runKind == "reaudit" && errors.Is(priorErr, sql.ErrNoRows)
	cloned.EnforcementEligible = job.IngressStage != "manual_capture_reprocess" && eligible &&
		((runKind == "request" && (job.ExecutionMode == "async" || foreground)) || firstRecoveryOutcome) && current.Mode != "off" && !currentlyExcluded
	outcome := &Outcome{JobID: job.ID, UserID: job.UserID, ReuseMode: job.ReuseMode, Evaluation: *cloned}
	models, err := json.Marshal(cloned.Models)
	if err != nil {
		return nil, err
	}
	configSnapshot, err := json.Marshal(job.Config)
	if err != nil {
		return nil, err
	}
	outcome.AuditRound, outcome.RunKind, outcome.RequestedBy, outcome.Config, outcome.StartedAt = jobAuditRound(job), runKind, job.CurrentRequestedBy, job.Config, job.StartedAt
	err = tx.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.third_party_prompt_audit_outcomes(job_id,user_id,decision,partial_failure,enforcement_eligible,model_results,source_outcome_id,audit_round,run_kind,requested_by,config_snapshot,started_at,finished_at,reuse_mode,target_hash,evaluation_hash)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,clock_timestamp(),$13,$14,$15) RETURNING id,created_at,finished_at`, job.ID, job.UserID, cloned.Decision, cloned.PartialFailure, cloned.EnforcementEligible, string(models), cloned.SourceOutcomeID,
		jobAuditRound(job), runKind, job.CurrentRequestedBy, string(configSnapshot), job.StartedAt, job.ReuseMode, job.TargetHash, job.EvaluationHash).Scan(&outcome.ID, &outcome.CreatedAt, &outcome.FinishedAt)
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
		segments := model.TargetUses
		if len(segments) == 0 {
			segments = model.Segments
		}
		for _, segment := range segments {
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.third_party_prompt_audit_outcome_segments(outcome_id,model_id,segment_order,segment_result_id,reuse_kind,target_kind)
		  SELECT $1,model_id,segment_order,segment_result_id,reuse_kind,
		         COALESCE((SELECT target_kind FROM sub2api_enhance.third_party_prompt_audit_segment_results WHERE id=segment_result_id),'legacy_segment')
		  FROM json_to_recordset($2::json)
		  AS entry(model_id text,segment_order integer,segment_result_id bigint,reuse_kind text)`, outcome.ID, string(raw)); err != nil {
			return nil, err
		}
	}

	before := state
	window := make([]Decision, 0)
	canonicalEligible := !currentlyExcluded && (outcome.EnforcementEligible || (runKind == "reaudit" && originalEligible))
	if canonicalEligible {
		ruleHash, err := warningRuleHash(configuredRules, current.WarningRuleRevision, job.UserID)
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
			rows, err := tx.QueryContext(ctx, `SELECT latest.decision FROM sub2api_enhance.third_party_prompt_audit_jobs root JOIN LATERAL (SELECT first.id,first.enforcement_eligible FROM sub2api_enhance.third_party_prompt_audit_outcomes first WHERE first.job_id=root.id ORDER BY first.audit_round,first.id LIMIT 1) original ON original.enforcement_eligible JOIN LATERAL (SELECT current.decision FROM sub2api_enhance.third_party_prompt_audit_outcomes current WHERE current.job_id=root.id ORDER BY current.audit_round DESC,current.id DESC LIMIT 1) latest ON true WHERE root.user_id=$1 AND original.id>$2 ORDER BY root.id DESC LIMIT $3`, job.UserID, state.WarningWindowAfterOutcomeID, current.Warning.Window)
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
	if canonicalEligible && user.Role == "user" && (current.Disable.Enabled || disableCounted) && (state.DisableResetAt == nil || capturedAt.After(*state.DisableResetAt)) {
		state.DisableViolationCount, disableCounted = adjustDisableContribution(state.DisableViolationCount, disableCounted, outcome.Decision == DecisionBlock)
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
	if outcome.EnforcementEligible {
		next.LastOutcomeID = &outcome.ID
	}
	_, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_enforcement_states SET warning_rule_hash=$2,warning_window_after_outcome_id=$3,warning_armed=$4,disable_violation_count=$5,last_outcome_id=$6,updated_at=clock_timestamp() WHERE user_id=$1`, job.UserID, next.WarningRuleHash, next.WarningWindowAfterOutcomeID, next.WarningArmed, next.DisableViolationCount, next.LastOutcomeID)
	if err != nil {
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
	result, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_jobs SET status='done',disable_counted=$3,finished_at=clock_timestamp(),lease_until=NULL,result_checkpoint=NULL,failure_stage=NULL,last_error_code=NULL,last_error_message=NULL,updated_at=clock_timestamp() WHERE id=$1 AND claim_generation=$2 AND status='processing'`, job.ID, job.ClaimGeneration, disableCounted)
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
	if r.redis != nil && !outcome.PartialFailure && outcome.SourceOutcomeID == nil && job.EvaluationHash != "" && job.TargetHash != "" {
		cacheCtx, cacheCancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
		_ = r.redis.SaveWhole(cacheCtx, *outcome, job.EvaluationHash, job.TargetHash)
		cacheCancel()
	}
	resultName := map[Decision]string{DecisionPass: "通过", DecisionReview: "待复核", DecisionBlock: "违规"}[outcome.Decision]
	logger.LegacyPrintf("third_party_prompt_audit", "审核分类已提交：%s job_id=%d outcome_id=%d decision=%s state_transition=%s", resultName, job.ID, outcome.ID, outcome.Decision, string(transition))
	return outcome, nil
}

// adjustDisableContribution 按任务当前结论精确修正一次累计违规贡献，并保证累计值不为负数。
func adjustDisableContribution(count int64, counted, shouldCount bool) (int64, bool) {
	if !counted && shouldCount {
		count++
	} else if counted && !shouldCount && count > 0 {
		count--
	}
	return count, shouldCount
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
