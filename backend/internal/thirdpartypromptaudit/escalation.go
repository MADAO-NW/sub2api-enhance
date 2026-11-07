package thirdpartypromptaudit

import (
	"context"
	"database/sql"
	"errors"
)

// ClearEscalation 解除自动加严并记录本地动作；解除不发通知。
// 用户不处于加严状态时返回 ErrNotFound。
func (r *Repository) ClearEscalation(ctx context.Context, userID, actorID int64) (int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var escalatedJob sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT escalated_job_id FROM sub2api_enhance.third_party_prompt_audit_enforcement_states
 WHERE user_id=$1 AND escalated_at IS NOT NULL FOR UPDATE`, userID).Scan(&escalatedJob)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_enforcement_states
 SET escalated_at=NULL,escalated_job_id=NULL,updated_at=clock_timestamp() WHERE user_id=$1`, userID); err != nil {
		return 0, err
	}
	action := Action{UserID: userID, ActorUserID: &actorID, ActionType: "escalation_clear",
		RuleSnapshot: map[string]any{}, ExecutionStatus: "succeeded", NotificationStatus: "not_required", AuthCacheStatus: "not_required",
		BusinessSnapshot: map[string]any{"actor_user_id": actorID, "escalated_job_id": escalatedJob.Int64}}
	if err := insertAction(ctx, tx, &action); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return action.ID, nil
}
