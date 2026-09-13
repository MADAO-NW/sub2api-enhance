package thirdpartypromptaudit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
)

func (r *Repository) CreateBatch(ctx context.Context, req BatchRequest, actor int64) (*Batch, error) {
	if req.Type != BatchPendingReview && req.Type != BatchFailedRecovery && req.Type != BatchReauditSelected && req.Type != BatchReauditFilter {
		return nil, errors.New("批次类型无效")
	}
	if len(req.IDs) > 0 {
		if len(req.Filter.IDs) == 0 {
			req.Filter.IDs = append([]int64(nil), req.IDs...)
		} else {
			allowed := make(map[int64]struct{}, len(req.IDs))
			for _, id := range req.IDs {
				allowed[id] = struct{}{}
			}
			ids := req.Filter.IDs[:0]
			for _, id := range req.Filter.IDs {
				if _, ok := allowed[id]; ok {
					ids = append(ids, id)
				}
			}
			req.Filter.IDs = ids
		}
	}
	if len(req.Filter.IDs) > 0 {
		seen := make(map[int64]struct{}, len(req.Filter.IDs))
		ids := req.Filter.IDs[:0]
		for _, id := range req.Filter.IDs {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		req.Filter.IDs = ids
	}
	if req.ReuseMode == "" {
		req.ReuseMode = ReuseModeAllow
	}
	if req.ReuseMode != ReuseModeAllow && req.ReuseMode != ReuseModeForce {
		return nil, errors.New("复核复用方式无效")
	}
	if err := validateFilter(req.Filter); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hashBytes := sha256.Sum256(raw)
	requestHash := hex.EncodeToString(hashBytes[:])
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var b Batch
	err = tx.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.third_party_prompt_audit_batches(batch_type,requested_by,request_hash,request_snapshot) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING RETURNING id,batch_type,requested_by,status,matched_count,ready_count,created_count,requeued_count,resumed_count,skipped_count,failed_count,created_at,updated_at`, req.Type, actor, requestHash, string(raw)).Scan(&b.ID, &b.Type, &b.RequestedBy, &b.Status, &b.Matched, &b.Ready, &b.Created, &b.Requeued, &b.Resumed, &b.Skipped, &b.Failed, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		if err = tx.QueryRowContext(ctx, `SELECT id,batch_type,requested_by,status,matched_count,ready_count,created_count,requeued_count,resumed_count,skipped_count,failed_count,created_at,updated_at FROM sub2api_enhance.third_party_prompt_audit_batches WHERE requested_by=$1 AND batch_type=$2 AND request_hash=$3 AND status IN ('queued','processing') ORDER BY id DESC LIMIT 1`, actor, req.Type, requestHash).Scan(&b.ID, &b.Type, &b.RequestedBy, &b.Status, &b.Matched, &b.Ready, &b.Created, &b.Requeued, &b.Resumed, &b.Skipped, &b.Failed, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return &b, nil
	}
	if err != nil {
		return nil, err
	}
	var matched, ready int64
	insert := func(captureID, jobID any, round *int, status, reason string) error {
		_, e := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.third_party_prompt_audit_batch_items(batch_id,capture_id,job_id,source_audit_round,status,reason) VALUES($1,$2,$3,$4,$5,$6)`, b.ID, captureID, jobID, round, status, reason)
		return e
	}
	switch req.Type {
	case BatchPendingReview:
		rows, e := tx.QueryContext(ctx, `SELECT id FROM sub2api_enhance.captures WHERE processing_status='awaiting_review' AND snapshot_status='complete' ORDER BY id`)
		if e != nil {
			return nil, e
		}
		for rows.Next() {
			var id int64
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return nil, e
			}
			if e = insert(id, nil, nil, "queued", ""); e != nil {
				rows.Close()
				return nil, e
			}
			matched++
			ready++
		}
		if e = rows.Err(); e != nil {
			rows.Close()
			return nil, e
		}
		rows.Close()
	case BatchFailedRecovery:
		rows, e := tx.QueryContext(ctx, `SELECT c.id,j.id,j.audit_round,CASE WHEN j.id IS NULL OR j.status='failed' THEN 'queued' ELSE 'skipped' END,CASE WHEN j.id IS NOT NULL AND j.status<>'failed' THEN '任务当前不是失败状态' ELSE '' END FROM sub2api_enhance.captures c LEFT JOIN sub2api_enhance.third_party_prompt_audit_jobs j ON j.capture_id=c.id WHERE c.snapshot_status='complete' AND COALESCE(c.request_metadata::json->>'manual_reprocess','false')<>'true' AND (((j.id IS NULL AND c.processing_status='failed') OR j.status='failed') AND c.eligibility_status='passed' AND COALESCE(c.request_metadata::json->>'audit_required','false')='true') ORDER BY c.id`)
		if e != nil {
			return nil, e
		}
		for rows.Next() {
			var cid int64
			var jid, round sql.NullInt64
			var status, reason string
			if e = rows.Scan(&cid, &jid, &round, &status, &reason); e != nil {
				rows.Close()
				return nil, e
			}
			var j any = nil
			if jid.Valid {
				j = jid.Int64
			}
			var rd *int
			if round.Valid {
				x := int(round.Int64)
				rd = &x
			}
			if e = insert(cid, j, rd, status, reason); e != nil {
				rows.Close()
				return nil, e
			}
			matched++
			if status == "queued" {
				ready++
			}
		}
		if e = rows.Err(); e != nil {
			rows.Close()
			return nil, e
		}
		rows.Close()
	default:
		where, args := filterSQL(req.Filter, true)
		q := `SELECT j.id,j.audit_round,(j.snapshot_status='complete' AND j.full_input_snapshot IS NOT NULL AND COALESCE(j.last_error_code,'')<>'no_text'),CASE WHEN j.status IN ('queued','processing','retry') THEN true ELSE false END` + jobSource + latestOutcomeJoin + ` WHERE ` + where + ` ORDER BY j.id`
		rows, e := tx.QueryContext(ctx, q, args...)
		if e != nil {
			return nil, e
		}
		for rows.Next() {
			var jid int64
			var round int
			var isReady, active bool
			if e = rows.Scan(&jid, &round, &isReady, &active); e != nil {
				rows.Close()
				return nil, e
			}
			status, reason := "queued", ""
			if !isReady {
				status, reason = "skipped", "完整可审文本不可用，跳过重新审核"
			} else if active {
				status, reason = "already_running", "该任务已在处理中"
			}
			if e = insert(nil, jid, &round, status, reason); e != nil {
				rows.Close()
				return nil, e
			}
			matched++
			if status == "queued" {
				ready++
			}
		}
		if e = rows.Err(); e != nil {
			rows.Close()
			return nil, e
		}
		rows.Close()
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_batches SET matched_count=$2,ready_count=$3,updated_at=clock_timestamp() WHERE id=$1`, b.ID, matched, ready); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	b.Matched, b.Ready = matched, ready
	return &b, nil
}
func (r *Repository) GetBatch(ctx context.Context, id int64) (*Batch, error) {
	var b Batch
	err := r.db.QueryRowContext(ctx, `SELECT id,batch_type,requested_by,status,matched_count,ready_count,processed_count,created_count,requeued_count,resumed_count,skipped_count,failed_count,COALESCE(last_error,''),started_at,finished_at,created_at,updated_at FROM sub2api_enhance.third_party_prompt_audit_batches WHERE id=$1`, id).Scan(&b.ID, &b.Type, &b.RequestedBy, &b.Status, &b.Matched, &b.Ready, &b.Processed, &b.Created, &b.Requeued, &b.Resumed, &b.Skipped, &b.Failed, &b.LastError, &b.StartedAt, &b.FinishedAt, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &b, err
}
func (r *Repository) GetBatchItems(ctx context.Context, id int64, limit int, afterID int64) ([]BatchItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id,batch_id,capture_id,job_id,source_audit_round,status,COALESCE(reason,''),result_job_id,processed_at FROM sub2api_enhance.third_party_prompt_audit_batch_items WHERE batch_id=$1 AND id>$2 ORDER BY id LIMIT $3`, id, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BatchItem{}
	for rows.Next() {
		var x BatchItem
		if err := rows.Scan(&x.ID, &x.BatchID, &x.CaptureID, &x.JobID, &x.SourceAuditRound, &x.Status, &x.Reason, &x.ResultJobID, &x.ProcessedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (r *Repository) claimBatch(ctx context.Context) (*batchExecution, error) {
	var x batchExecution
	err := r.db.QueryRowContext(ctx, `WITH candidate AS (SELECT id FROM sub2api_enhance.third_party_prompt_audit_batches WHERE status='queued' OR (status='processing' AND lease_until<clock_timestamp()) ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1) UPDATE sub2api_enhance.third_party_prompt_audit_batches b SET status='processing',claim_generation=claim_generation+1,lease_until=clock_timestamp()+interval '30 seconds',started_at=COALESCE(started_at,clock_timestamp()),updated_at=clock_timestamp() FROM candidate WHERE b.id=candidate.id RETURNING b.id,b.batch_type,b.requested_by,b.status,b.request_snapshot,b.cursor,b.claim_generation`).Scan(&x.ID, &x.Type, &x.RequestedBy, &x.Status, new(string), &x.Cursor, &x.ClaimGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var raw string
	err = r.db.QueryRowContext(ctx, `SELECT request_snapshot::text FROM sub2api_enhance.third_party_prompt_audit_batches WHERE id=$1`, x.ID).Scan(&raw)
	if err != nil {
		return nil, err
	}
	_, _ = r.db.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_batch_items SET status='queued' WHERE batch_id=$1 AND status='processing'`, x.ID)
	if err = json.Unmarshal([]byte(raw), &x.Snapshot); err != nil {
		return nil, err
	}
	return &x, nil
}
func (r *Repository) RecoverBatches(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_batches SET status='queued',lease_until=NULL,updated_at=clock_timestamp() WHERE status='processing' AND lease_until<clock_timestamp()`)
	return err
}
