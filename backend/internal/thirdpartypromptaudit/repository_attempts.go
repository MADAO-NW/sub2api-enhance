package thirdpartypromptaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/lib/pq"
)

type AttemptStore interface {
	PrepareAttempt(context.Context, *Job, *ModelAttempt) error
	StartAttempt(context.Context, *Job, *ModelAttempt) error
	FinishAttempt(context.Context, *Job, *ModelAttempt) error
}

type EvaluationStore interface {
	FindWholeResult(context.Context, *Job) (*Outcome, error)
	FindSegments(context.Context, int64, string, []string) (map[string]SegmentResult, error)
	SaveSegment(context.Context, *Job, *SegmentResult) error
}

func (r *Repository) PrepareAttempt(ctx context.Context, job *Job, attempt *ModelAttempt) error {
	model, err := json.Marshal(attempt.ModelSnapshot)
	if err != nil {
		return err
	}
	var generation int64
	if job != nil {
		generation = job.ClaimGeneration
	}
	err = r.db.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.third_party_prompt_audit_model_attempts
 (job_id,call_kind,evaluation_round,model_id,model_snapshot,stage,segment_order,repair_of_attempt_id,request_metadata)
 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9
 WHERE $1::bigint IS NULL OR EXISTS (SELECT 1 FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE id=$1 AND claim_generation=$10 AND status='processing' AND lease_until>clock_timestamp())
 RETURNING id,created_at`, attempt.JobID, attempt.CallKind, attempt.EvaluationRound, attempt.ModelID,
		string(model), attempt.Stage, attempt.SegmentOrder, attempt.RepairOfAttemptID, string(attempt.RequestMetadata), generation,
	).Scan(&attempt.ID, &attempt.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	return err
}

func (r *Repository) StartAttempt(ctx context.Context, job *Job, attempt *ModelAttempt) error {
	var generation int64
	if job != nil {
		generation = job.ClaimGeneration
	}
	err := r.db.QueryRowContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_model_attempts a SET status='started',dispatch_started_at=clock_timestamp()
 WHERE a.id=$1 AND a.status='prepared' AND (a.job_id IS NULL OR EXISTS
 (SELECT 1 FROM sub2api_enhance.third_party_prompt_audit_jobs j WHERE j.id=a.job_id AND j.claim_generation=$2 AND j.status='processing' AND j.lease_until>clock_timestamp()))
 AND (a.call_kind='probe' OR (SELECT value::json->>'mode' FROM sub2api_enhance.settings WHERE key='third_party_prompt_audit_config') IN ('async','blocking'))
 RETURNING dispatch_started_at`, attempt.ID, generation).Scan(&attempt.DispatchStartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		if job != nil {
			var valid bool
			lookupErr := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE id=$1 AND claim_generation=$2 AND status='processing' AND lease_until>clock_timestamp())`, job.ID, job.ClaimGeneration).Scan(&valid)
			if lookupErr != nil {
				return lookupErr
			}
			if valid {
				return ErrAuditPaused
			}
		}
		return ErrLeaseLost
	}
	return err
}

func (r *Repository) FinishAttempt(ctx context.Context, job *Job, attempt *ModelAttempt) error {
	var confidence, reason, raw any
	if attempt.Result != nil {
		confidence, reason = attempt.Result.Confidence, encodeStoredText(attempt.Result.Reason)
	}
	if attempt.RawResponse != nil {
		raw = encodeStoredText(*attempt.RawResponse)
	}
	var generation int64
	if job != nil {
		generation = job.ClaimGeneration
	}
	result, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_model_attempts a SET status=$2,http_status=$3,raw_response=$4,confidence=$5,reason=$6,
 input_tokens=$7,output_tokens=$8,latency_ms=$9,error_code=$10,error_message=$11,finished_at=clock_timestamp()
 WHERE a.id=$1 AND a.status IN ('prepared','started') AND (a.job_id IS NULL OR EXISTS
 (SELECT 1 FROM sub2api_enhance.third_party_prompt_audit_jobs j WHERE j.id=a.job_id AND j.claim_generation=$12 AND j.status='processing' AND j.lease_until>clock_timestamp()))`,
		attempt.ID, attempt.Status, attempt.HTTPStatus, raw, confidence, reason, attempt.InputTokens, attempt.OutputTokens,
		attempt.LatencyMS, attempt.ErrorCode, encodeStoredText(attempt.ErrorMessage), generation)
	return checkLeaseUpdate(result, err)
}

func (r *Repository) FindSegments(ctx context.Context, userID int64, modelID string, keys []string) (map[string]SegmentResult, error) {
	results := make(map[string]SegmentResult)
	if len(keys) == 0 {
		return results, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT ON (audit_key) id,user_id,model_id,audit_key,source_attempt_id,source_role,policy_role,turn_scope,content_hash,confidence,reason
 FROM sub2api_enhance.third_party_prompt_audit_segment_results WHERE user_id=$1 AND model_id=$2 AND audit_key=ANY($3)
 ORDER BY audit_key,id DESC`, userID, modelID, pq.Array(keys))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var item SegmentResult
		if err := rows.Scan(&item.ID, &item.UserID, &item.ModelID, &item.AuditKey, &item.SourceAttemptID, &item.SourceRole,
			&item.PolicyRole, &item.TurnScope, &item.ContentHash, &item.Confidence, &item.Reason); err != nil {
			return nil, err
		}
		item.Reason = decodeStoredText(item.Reason)
		results[item.AuditKey] = item
	}
	return results, rows.Err()
}

func (r *Repository) SaveSegment(ctx context.Context, job *Job, item *SegmentResult) error {
	err := r.db.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.third_party_prompt_audit_segment_results
 (user_id,model_id,audit_key,source_attempt_id,source_role,policy_role,turn_scope,content_hash,confidence,reason)
 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10
 WHERE EXISTS (SELECT 1 FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE id=$11 AND claim_generation=$12 AND status='processing' AND lease_until>clock_timestamp())
 ON CONFLICT (source_attempt_id) DO UPDATE SET source_attempt_id=EXCLUDED.source_attempt_id RETURNING id`,
		item.UserID, item.ModelID, item.AuditKey, item.SourceAttemptID, item.SourceRole, item.PolicyRole, item.TurnScope,
		item.ContentHash, item.Confidence, encodeStoredText(item.Reason), job.ID, job.ClaimGeneration).Scan(&item.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	return err
}

func (r *Repository) FindWholeResult(ctx context.Context, job *Job) (*Outcome, error) {
	return r.queryOutcome(ctx, `SELECT o.id,o.job_id,o.user_id,o.decision,o.partial_failure,o.enforcement_eligible,o.model_results,o.source_outcome_id,o.created_at
 FROM sub2api_enhance.third_party_prompt_audit_outcomes o JOIN sub2api_enhance.third_party_prompt_audit_jobs j ON j.id=o.job_id
 WHERE j.user_id=$1 AND j.evaluation_hash=$2 AND j.target_hash=$3 AND NOT o.partial_failure AND o.source_outcome_id IS NULL
 ORDER BY o.id DESC LIMIT 1`, job.UserID, job.EvaluationHash, job.TargetHash)
}

func (r *Repository) GetOutcome(ctx context.Context, jobID int64) (*Outcome, error) {
	return r.queryOutcome(ctx, `SELECT id,job_id,user_id,decision,partial_failure,enforcement_eligible,model_results,source_outcome_id,created_at FROM sub2api_enhance.third_party_prompt_audit_outcomes WHERE job_id=$1`, jobID)
}

func (r *Repository) queryOutcome(ctx context.Context, query string, args ...any) (*Outcome, error) {
	var outcome Outcome
	var models string
	err := r.db.QueryRowContext(ctx, query, args...).Scan(&outcome.ID, &outcome.JobID, &outcome.UserID, &outcome.Decision,
		&outcome.PartialFailure, &outcome.EnforcementEligible, &models, &outcome.SourceOutcomeID, &outcome.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(models), &outcome.Models); err != nil {
		return nil, err
	}
	return &outcome, nil
}

func (r *Repository) ListAttempts(ctx context.Context, jobID int64) ([]ModelAttempt, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,job_id,call_kind,evaluation_round,model_id,model_snapshot,stage,segment_order,repair_of_attempt_id,
 request_metadata,status,http_status,raw_response,confidence,reason,input_tokens,output_tokens,latency_ms,error_code,error_message,created_at,dispatch_started_at,finished_at
 FROM sub2api_enhance.third_party_prompt_audit_model_attempts WHERE job_id=$1
 OR id IN (SELECT s.source_attempt_id FROM sub2api_enhance.third_party_prompt_audit_outcomes o JOIN sub2api_enhance.third_party_prompt_audit_outcome_segments link ON link.outcome_id=o.id JOIN sub2api_enhance.third_party_prompt_audit_segment_results s ON s.id=link.segment_result_id WHERE o.job_id=$1)
 OR id IN (SELECT (m->>'joint_attempt_id')::bigint FROM sub2api_enhance.third_party_prompt_audit_outcomes o CROSS JOIN LATERAL json_array_elements(o.model_results::json) m WHERE o.job_id=$1)
 ORDER BY id`, jobID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]ModelAttempt, 0)
	for rows.Next() {
		var item ModelAttempt
		var model, metadata string
		var confidence *float64
		var reason, code, message *string
		if err := rows.Scan(&item.ID, &item.JobID, &item.CallKind, &item.EvaluationRound, &item.ModelID, &model, &item.Stage, &item.SegmentOrder, &item.RepairOfAttemptID,
			&metadata, &item.Status, &item.HTTPStatus, &item.RawResponse, &confidence, &reason, &item.InputTokens, &item.OutputTokens, &item.LatencyMS, &code, &message, &item.CreatedAt, &item.DispatchStartedAt, &item.FinishedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(model), &item.ModelSnapshot); err != nil {
			return nil, err
		}
		item.RequestMetadata = json.RawMessage(metadata)
		if confidence != nil && reason != nil {
			item.Result = &Score{Confidence: *confidence, Reason: decodeStoredText(*reason)}
		}
		if item.RawResponse != nil {
			raw := decodeStoredText(*item.RawResponse)
			item.RawResponse = &raw
		}
		if code != nil {
			item.ErrorCode = *code
		}
		if message != nil {
			item.ErrorMessage = decodeStoredText(*message)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
