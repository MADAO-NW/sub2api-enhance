package thirdpartypromptaudit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *Service) StartBatchWorker() {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				s.processBatches(s.ctx)
			case <-s.wake:
				s.processBatches(s.ctx)
			}
		}
	}()
}
func (s *Service) processBatches(ctx context.Context) {
	exec, err := s.repo.claimBatch(ctx)
	if err != nil || exec == nil {
		return
	}
	items, err := s.repo.GetBatchItems(ctx, exec.ID, 100, exec.Cursor)
	if err != nil {
		return
	}
	nextCursor := exec.Cursor
	for _, item := range items {
		if item.ID > nextCursor {
			nextCursor = item.ID
		}
		if item.Status != "queued" {
			continue
		}
		_, _ = s.repo.db.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_batches SET lease_until=clock_timestamp()+interval '30 seconds',updated_at=clock_timestamp() WHERE id=$1 AND claim_generation=$2 AND status='processing'`, exec.ID, exec.ClaimGeneration)
		_, _ = s.repo.db.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_batch_items SET status='processing' WHERE id=$1 AND status='queued'`, item.ID)
		status, reason, resultID := s.processBatchItem(ctx, exec, item)
		_, _ = s.repo.db.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_batch_items SET status=$2,reason=$3,result_job_id=$4,processed_at=clock_timestamp() WHERE id=$1 AND status='processing'`, item.ID, status, reason, resultID)
		_, _ = s.repo.db.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_batches SET processed_count=processed_count+1,created_count=created_count+CASE WHEN $2='created' THEN 1 ELSE 0 END,requeued_count=requeued_count+CASE WHEN $2='requeued' THEN 1 ELSE 0 END,resumed_count=resumed_count+CASE WHEN $2='resumed' THEN 1 ELSE 0 END,skipped_count=skipped_count+CASE WHEN $2 IN ('skipped','already_running') THEN 1 ELSE 0 END,failed_count=failed_count+CASE WHEN $2='failed' THEN 1 ELSE 0 END,updated_at=clock_timestamp() WHERE id=$1`, exec.ID, status)
	}
	_, _ = s.repo.db.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_batches SET cursor=$2,updated_at=clock_timestamp() WHERE id=$1 AND claim_generation=$3`, exec.ID, nextCursor, exec.ClaimGeneration)
	var pending bool
	_ = s.repo.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sub2api_enhance.third_party_prompt_audit_batch_items WHERE batch_id=$1 AND status IN ('queued','ready','processing'))`, exec.ID).Scan(&pending)
	if !pending {
		_, _ = s.repo.db.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_batches SET status='completed',finished_at=clock_timestamp(),lease_until=NULL,updated_at=clock_timestamp() WHERE id=$1 AND status='processing'`, exec.ID)
	} else {
		_, _ = s.repo.db.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_batches SET status='queued',lease_until=NULL,updated_at=clock_timestamp() WHERE id=$1 AND status='processing'`, exec.ID)
	}
}
func (s *Service) processBatchItem(ctx context.Context, exec *batchExecution, item BatchItem) (string, string, *int64) {
	if item.Status != "queued" {
		return item.Status, item.Reason, item.ResultJobID
	}
	if exec.Type == BatchPendingReview && item.CaptureID != nil {
		c, err := NewCaptureStore(s.repo.db).Get(ctx, *item.CaptureID)
		if err != nil {
			return "failed", err.Error(), nil
		}
		d, err := s.ReviewCapture(ctx, c, exec.RequestedBy)
		if err != nil {
			if strings.Contains(err.Error(), "身份") || strings.Contains(err.Error(), "输入不完整") || errors.Is(err, ErrNoText) {
				return "skipped", err.Error(), nil
			}
			return "failed", err.Error(), nil
		}
		if d == nil || d.JobID <= 0 {
			return "skipped", "无法创建人工审核任务", nil
		}
		return "created", "", &d.JobID
	}
	if (exec.Type == BatchReauditSelected || exec.Type == BatchReauditFilter) && item.JobID != nil {
		res, err := s.CreateReaudits(ctx, ReauditRequest{ReuseMode: exec.Snapshot.ReuseMode, BatchID: fmt.Sprintf("%d", exec.ID), Filter: Filter{IDs: []int64{*item.JobID}}}, exec.RequestedBy)
		if err != nil {
			return "failed", err.Error(), nil
		}
		if len(res.Items) == 0 {
			return "skipped", "任务不存在", nil
		}
		x := res.Items[0]
		if x.Status == "requeued" {
			return x.Status, x.Reason, &x.JobID
		}
		return x.Status, x.Reason, nil
	}
	if exec.Type == BatchFailedRecovery && item.CaptureID != nil {
		return s.processRecoveryItem(ctx, item, exec.RequestedBy)
	}
	return "skipped", "批次目标不完整", nil
}

type BatchPreview struct {
	Matched int64 `json:"matched"`
	Ready   int64 `json:"ready"`
}

func (r *Repository) PreviewBatch(ctx context.Context, req BatchRequest) (*BatchPreview, error) {
	if len(req.IDs) > 0 {
		if len(req.Filter.IDs) == 0 {
			req.Filter.IDs = append([]int64(nil), req.IDs...)
		} else {
			allowed := map[int64]struct{}{}
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
	if err := validateFilter(req.Filter); err != nil {
		return nil, err
	}
	p := &BatchPreview{}
	switch req.Type {
	case BatchPendingReview:
		err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM sub2api_enhance.captures WHERE processing_status='awaiting_review' AND snapshot_status='complete'`).Scan(&p.Matched)
		p.Ready = p.Matched
		return p, err
	case BatchFailedRecovery:
		err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM sub2api_enhance.captures c LEFT JOIN sub2api_enhance.third_party_prompt_audit_jobs j ON j.capture_id=c.id WHERE c.snapshot_status='complete' AND c.eligibility_status='passed' AND COALESCE(c.request_metadata::json->>'audit_required','false')='true' AND COALESCE(c.request_metadata::json->>'manual_reprocess','false')<>'true' AND ((j.id IS NULL AND c.processing_status='failed') OR j.status='failed')`).Scan(&p.Matched)
		p.Ready = p.Matched
		return p, err
	case BatchReauditSelected, BatchReauditFilter:
		result, err := r.PreviewReaudits(ctx, ReauditRequest{ReuseMode: req.ReuseMode, Filter: req.Filter})
		if err != nil {
			return nil, err
		}
		return &BatchPreview{Matched: result.Matched, Ready: result.Ready}, nil
	default:
		return nil, errors.New("批次类型无效")
	}
}
func (r *Repository) ListBatches(ctx context.Context, actor int64, limit int) ([]Batch, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id,batch_type,requested_by,status,matched_count,ready_count,processed_count,created_count,requeued_count,resumed_count,skipped_count,failed_count,COALESCE(last_error,''),started_at,finished_at,created_at,updated_at FROM sub2api_enhance.third_party_prompt_audit_batches WHERE requested_by=$1 ORDER BY id DESC LIMIT $2`, actor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Batch{}
	for rows.Next() {
		var b Batch
		if err := rows.Scan(&b.ID, &b.Type, &b.RequestedBy, &b.Status, &b.Matched, &b.Ready, &b.Processed, &b.Created, &b.Requeued, &b.Resumed, &b.Skipped, &b.Failed, &b.LastError, &b.StartedAt, &b.FinishedAt, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
