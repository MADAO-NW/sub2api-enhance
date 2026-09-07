package thirdpartypromptaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// actionColumns 完整保留远端执行和邮件投递的独立状态。
const actionColumns = `id,user_id,outcome_id,actor_user_id,action_type,rule_snapshot,business_snapshot,applied_at,notification_status,deliveries,auth_cache_status,auth_cache_error,next_attempt_at,claim_generation,lease_until,updated_at,execution_status,attempt_history,requested_at,completed_at`

type rowScanner interface{ Scan(...any) error }

func scanAction(row rowScanner) (*Action, error) {
	var a Action
	var rule, business, deliveries, attempts string
	var cacheError *string
	err := row.Scan(&a.ID, &a.UserID, &a.OutcomeID, &a.ActorUserID, &a.ActionType, &rule, &business, &a.AppliedAt, &a.NotificationStatus, &deliveries, &a.AuthCacheStatus, &cacheError, &a.NextAttemptAt, &a.ClaimGeneration, &a.LeaseUntil, &a.UpdatedAt, &a.ExecutionStatus, &attempts, &a.RequestedAt, &a.CompletedAt)
	if err != nil {
		return nil, err
	}
	for _, v := range []struct {
		raw    string
		target any
	}{{rule, &a.RuleSnapshot}, {business, &a.BusinessSnapshot}, {deliveries, &a.Deliveries}, {attempts, &a.AttemptHistory}} {
		d := json.NewDecoder(strings.NewReader(v.raw))
		d.UseNumber()
		if err := d.Decode(v.target); err != nil {
			return nil, err
		}
	}
	if cacheError != nil {
		a.AuthCacheError = decodeStoredText(*cacheError)
	}
	return &a, nil
}
func (r *Repository) ClaimAction(ctx context.Context) (*Action, error) {
	a, err := scanAction(r.db.QueryRowContext(ctx, `WITH candidate AS (
 SELECT id FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions
 WHERE (execution_status IN ('pending','processing','unknown') OR (execution_status='succeeded' AND notification_status IN ('pending','processing','retry')))
 AND next_attempt_at<=clock_timestamp() AND (lease_until IS NULL OR lease_until<=clock_timestamp())
 ORDER BY next_attempt_at,id FOR UPDATE SKIP LOCKED LIMIT 1)
 UPDATE sub2api_enhance.third_party_prompt_audit_enforcement_actions SET claim_generation=claim_generation+1,lease_until=clock_timestamp()+$1::interval,
 execution_status=CASE WHEN execution_status='processing' THEN 'unknown' ELSE execution_status END,updated_at=clock_timestamp()
 WHERE id IN(SELECT id FROM candidate) RETURNING `+actionColumns, interval(leaseDuration)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}
func (r *Repository) RenewAction(ctx context.Context, a *Action) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_enforcement_actions SET lease_until=clock_timestamp()+$3::interval,updated_at=clock_timestamp() WHERE id=$1 AND claim_generation=$2 AND lease_until>clock_timestamp()`, a.ID, a.ClaimGeneration, interval(leaseDuration))
	return checkLeaseUpdate(result, err)
}
func (r *Repository) SaveAction(ctx context.Context, a *Action, release bool) error {
	deliveries, err := json.Marshal(a.Deliveries)
	if err != nil {
		return err
	}
	attempts, err := json.Marshal(a.AttemptHistory)
	if err != nil {
		return err
	}
	business, err := json.Marshal(a.BusinessSnapshot)
	if err != nil {
		return err
	}
	result, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_enforcement_actions SET notification_status=$3,deliveries=$4,auth_cache_status=$5,auth_cache_error=$6,next_attempt_at=$7,
 lease_until=CASE WHEN $8 THEN NULL ELSE lease_until END,execution_status=$9,attempt_history=$10,applied_at=$11,completed_at=$12,business_snapshot=$13,updated_at=clock_timestamp()
 WHERE id=$1 AND claim_generation=$2 AND lease_until>clock_timestamp() AND execution_status<>'cancelled'`, a.ID, a.ClaimGeneration, a.NotificationStatus, string(deliveries), a.AuthCacheStatus, encodeStoredText(a.AuthCacheError), a.NextAttemptAt, release, a.ExecutionStatus, string(attempts), a.AppliedAt, a.CompletedAt, string(business))
	return checkLeaseUpdate(result, err)
}
func (r *Repository) RetryAction(ctx context.Context, id int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, err := scanAction(tx.QueryRowContext(ctx, `SELECT `+actionColumns+` FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if a.LeaseUntil != nil && a.LeaseUntil.After(time.Now()) {
		return ErrLeaseLost
	}
	if a.ExecutionStatus == "cancelled" {
		return errors.New("已取消动作不能重新执行")
	}
	if a.ExecutionStatus == "failed" {
		if len(a.AttemptHistory) > 0 {
			a.ExecutionStatus = "unknown"
		} else {
			a.ExecutionStatus = "pending"
		}
	}
	for i := range a.Deliveries {
		if a.Deliveries[i].Status == "failed" {
			a.Deliveries[i].Status = "pending"
		}
	}
	if a.ExecutionStatus == "succeeded" && a.NotificationStatus == "failed" {
		a.NotificationStatus = "pending"
	}
	raw, _ := json.Marshal(a.Deliveries)
	_, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_enforcement_actions SET execution_status=$2,notification_status=$3,deliveries=$4,next_attempt_at=clock_timestamp(),lease_until=NULL,claim_generation=claim_generation+1,updated_at=clock_timestamp() WHERE id=$1`, id, a.ExecutionStatus, a.NotificationStatus, string(raw))
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (r *Repository) ListActions(ctx context.Context, userID, rootID int64) ([]Action, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+actionColumns+` FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions WHERE user_id=$1 AND
 (outcome_id IN(SELECT o.id FROM sub2api_enhance.third_party_prompt_audit_outcomes o JOIN sub2api_enhance.third_party_prompt_audit_jobs j ON j.id=o.job_id WHERE j.id=$2 OR j.source_job_id=$2) OR action_type='counter_reset') ORDER BY id`, userID, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Action{}
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}
