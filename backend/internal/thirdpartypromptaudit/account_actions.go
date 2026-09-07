package thirdpartypromptaudit

import (
	"context"
	"encoding/json"
	"errors"
	"sub2api-enhance/internal/pkg/logger"
	"sub2api-enhance/internal/sub2api"
	"time"
)

type AccountAttempt struct {
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at"`
	RequestedStatus string     `json:"requested_status"`
	Response        string     `json:"response"`
	Error           string     `json:"error"`
	Status          string     `json:"status"`
}

// SetAccountClient 注入原版账号边界，模型与通知不持有该管理员凭据。
func (s *Service) SetAccountClient(c *sub2api.Client) { s.accounts = c }
func (s *Service) executeAccountAction(ctx context.Context, a *Action) bool {
	finish := func(status, reason string) {
		a.ExecutionStatus = status
		a.BusinessSnapshot["execution_reason"] = reason
		a.NextAttemptAt = time.Now().Add(30 * time.Second)
		if status == "cancelled" || status == "failed" {
			now := time.Now().UTC()
			a.CompletedAt = &now
		}
		if err := s.repo.SaveAction(ctx, a, true); err != nil {
			s.noteError("account_action_persist_failed", err)
		}
	}
	if s.accounts == nil || !s.accounts.Configured() {
		finish("failed", "尚未配置原版账号 API")
		return false
	}
	target := "disabled"
	if a.ActionType == "counter_reset" {
		target = "active"
	}
	user, err := s.accounts.GetUser(ctx, a.UserID)
	if err != nil {
		finish(a.ExecutionStatus, err.Error())
		return false
	}
	if user.Role != "user" {
		finish("cancelled", "仅允许处理普通用户")
		return false
	}
	if a.ExecutionStatus == "pending" {
		if a.ActionType == "disable" {
			config, err := s.config.Active()
			if err != nil || s.config.EffectiveMode() == "off" || !config.Disable.Enabled {
				finish("cancelled", "当前配置不允许自动停用")
				return false
			}
			var obsolete bool
			if err := s.repo.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sub2api_enhance.third_party_prompt_audit_enforcement_states WHERE user_id=$1 AND disable_reset_at>=$2) OR EXISTS(SELECT 1 FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions WHERE user_id=$1 AND action_type='counter_reset' AND execution_status IN ('pending','processing','unknown'))`, a.UserID, a.RequestedAt).Scan(&obsolete); err != nil {
				finish("pending", err.Error())
				return false
			}
			if obsolete || user.Status != "active" {
				finish("cancelled", "用户已重置累计或状态发生变化")
				return false
			}
		}
		a.BusinessSnapshot["before_user"] = user
		a.ExecutionStatus = "processing"
		a.AttemptHistory = append(a.AttemptHistory, AccountAttempt{StartedAt: time.Now().UTC(), RequestedStatus: target, Status: "started"})
		if err := s.repo.SaveAction(ctx, a, false); err != nil {
			s.noteError("account_attempt_persist_failed", err)
			return false
		}
		logger.LegacyPrintf("third_party_prompt_audit", "开始执行账号状态操作 action_id=%d user_id=%d target_status=%s", a.ID, a.UserID, target)
		raw, callErr := s.accounts.SetUserStatus(ctx, a.UserID, target)
		now := time.Now().UTC()
		attempt := &a.AttemptHistory[len(a.AttemptHistory)-1]
		attempt.FinishedAt = &now
		attempt.Response = raw
		if callErr != nil {
			attempt.Error = callErr.Error()
			attempt.Status = "unknown"
			var apiErr *sub2api.APIError
			if errors.As(callErr, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 {
				attempt.Status = "failed"
				finish("failed", callErr.Error())
			} else {
				finish("unknown", callErr.Error())
			}
			return false
		}
		attempt.Status = "succeeded"
		a.BusinessSnapshot["confirmation_basis"] = "api_success"
	} else {
		// 已发送但结果未知的动作只读确认，不能再次发起状态写入。
		if user.Status != target {
			finish("unknown", "当前状态与目标不一致，请核对原版操作记录；未重复调用账号更新")
			return false
		}
		a.BusinessSnapshot["confirmation_basis"] = "readback_state_matches"
	}
	if a.ActionType == "counter_reset" {
		if err := s.repo.finishCounterReset(ctx, a); err != nil {
			s.noteError("counter_reset_finish_failed", err)
			return false
		}
		return true
	}
	now := time.Now().UTC()
	a.ExecutionStatus = "succeeded"
	a.AppliedAt = &now
	a.CompletedAt = &now
	a.AuthCacheStatus = "not_observed"
	a.NotificationStatus = "pending"
	a.BusinessSnapshot["confirmed_user_status"] = target
	if err := s.repo.SaveAction(ctx, a, false); err != nil {
		s.noteError("account_confirmation_persist_failed", err)
		return false
	}
	logger.LegacyPrintf("third_party_prompt_audit", "账号停用已确认 action_id=%d user_id=%d confirmation_basis=%v", a.ID, a.UserID, a.BusinessSnapshot["confirmation_basis"])
	return true
}
func (r *Repository) CreateCounterReset(ctx context.Context, userID, actorID int64) (int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.third_party_prompt_audit_enforcement_states(user_id) VALUES($1) ON CONFLICT DO NOTHING`, userID); err != nil {
		return 0, err
	}
	var count int64
	if err := tx.QueryRowContext(ctx, `SELECT disable_violation_count FROM sub2api_enhance.third_party_prompt_audit_enforcement_states WHERE user_id=$1 FOR UPDATE`, userID).Scan(&count); err != nil {
		return 0, err
	}
	// 锁住该用户的活跃动作，避免“检查未发送”与取消之间被其他 Worker 发起调用。
	rows, err := tx.QueryContext(ctx, `SELECT action_type,execution_status FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions WHERE user_id=$1 AND execution_status IN ('pending','processing','unknown') FOR UPDATE`, userID)
	if err != nil {
		return 0, err
	}
	busy := false
	for rows.Next() {
		var kind, status string
		if err := rows.Scan(&kind, &status); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if status == "processing" || status == "unknown" || kind == "counter_reset" {
			busy = true
		}
	}
	readErr := rows.Err()
	closeErr := rows.Close()
	if readErr != nil {
		return 0, readErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	if busy {
		return 0, errors.New("该用户有尚未确认的账号动作，请先处理原动作")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_enforcement_actions SET execution_status='cancelled',claim_generation=claim_generation+1,lease_until=NULL,completed_at=clock_timestamp(),updated_at=clock_timestamp() WHERE user_id=$1 AND action_type='disable' AND execution_status='pending'`, userID); err != nil {
		return 0, err
	}
	a := Action{UserID: userID, ActorUserID: &actorID, ActionType: "counter_reset", RuleSnapshot: map[string]any{}, BusinessSnapshot: map[string]any{"requested_user_status": "active", "actor_user_id": actorID}, ExecutionStatus: "pending", NotificationStatus: "not_required", AuthCacheStatus: "not_observed", Deliveries: []Delivery{}}
	if err := insertAction(ctx, tx, &a); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return a.ID, nil
}
func (r *Repository) finishCounterReset(ctx context.Context, a *Action) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int64
	var before *time.Time
	if err := tx.QueryRowContext(ctx, `SELECT disable_violation_count,disable_reset_at FROM sub2api_enhance.third_party_prompt_audit_enforcement_states WHERE user_id=$1 FOR UPDATE`, a.UserID).Scan(&count, &before); err != nil {
		return err
	}
	var valid bool
	if err := tx.QueryRowContext(ctx, `SELECT execution_status IN ('processing','unknown') AND claim_generation=$2 AND lease_until>clock_timestamp() FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions WHERE id=$1 FOR UPDATE`, a.ID, a.ClaimGeneration).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrLeaseLost
	}
	var at time.Time
	if err := tx.QueryRowContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_enforcement_states SET disable_violation_count=0,disable_reset_at=clock_timestamp(),updated_at=clock_timestamp() WHERE user_id=$1 RETURNING disable_reset_at`, a.UserID).Scan(&at); err != nil {
		return err
	}
	a.BusinessSnapshot["before_state"] = map[string]any{"disable_violation_count": count, "disable_reset_at": before}
	a.BusinessSnapshot["after_state"] = map[string]any{"disable_violation_count": 0, "disable_reset_at": at}
	business, _ := json.Marshal(a.BusinessSnapshot)
	attempts, _ := json.Marshal(a.AttemptHistory)
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_enforcement_actions SET execution_status='succeeded',applied_at=$2,completed_at=$2,business_snapshot=$3,attempt_history=$4,lease_until=NULL,updated_at=clock_timestamp() WHERE id=$1`, a.ID, at, string(business), string(attempts)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.ExecutionStatus = "succeeded"
	a.AppliedAt = &at
	a.CompletedAt = &at
	logger.LegacyPrintf("third_party_prompt_audit", "用户启用及累计重置已确认 action_id=%d user_id=%d transition=%s", a.ID, a.UserID, business)
	return nil
}
