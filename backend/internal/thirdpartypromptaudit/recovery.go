package thirdpartypromptaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
)

type recoveryCandidate struct {
	CaptureID  int64
	JobID      *int64
	UserID     int64
	Checkpoint bool
}

func (r *Repository) recoveryCandidates(ctx context.Context, awaitingReview bool) ([]recoveryCandidate, error) {
	condition := "((j.id IS NULL AND c.processing_status='failed') OR j.status='failed')"
	eligibility := " AND c.eligibility_status='passed' AND COALESCE(c.request_metadata::json->>'audit_required','false')='true'"
	userID := "c.user_id"
	if awaitingReview {
		condition = "j.id IS NULL AND c.processing_status='awaiting_review'"
		eligibility = ""
		userID = "COALESCE(c.user_id,0)"
	}
	query := `SELECT c.id,j.id,` + userID + `,
		 COALESCE(j.failure_stage='result_persist' AND j.result_checkpoint IS NOT NULL,false)
		 FROM sub2api_enhance.captures c
		 LEFT JOIN sub2api_enhance.third_party_prompt_audit_jobs j ON j.capture_id=c.id
		 WHERE c.snapshot_status='complete'` + eligibility + `
		 AND COALESCE(c.request_metadata::json->>'manual_reprocess','false')<>'true'
		 AND ` + condition + ` ORDER BY c.id`
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]recoveryCandidate, 0)
	for rows.Next() {
		var item recoveryCandidate
		if err := rows.Scan(&item.CaptureID, &item.JobID, &item.UserID, &item.Checkpoint); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Service) recoveryPlan(ctx context.Context, awaitingReview bool, validateInput bool) (*RecoveryResult, error) {
	candidates, err := s.repo.recoveryCandidates(ctx, awaitingReview)
	if err != nil {
		return nil, err
	}
	snapshot, err := s.config.Active()
	if err != nil {
		return nil, err
	}
	store := NewCaptureStore(s.repo.db)
	result := &RecoveryResult{Matched: int64(len(candidates)), Items: make([]RecoveryItem, 0, len(candidates))}
	for _, candidate := range candidates {
		item := RecoveryItem{CaptureID: candidate.CaptureID, JobID: candidate.JobID, userID: candidate.UserID, checkpoint: candidate.Checkpoint, Status: "ready"}
		if candidate.Checkpoint {
			item.Action = "resume_checkpoint"
		} else if candidate.JobID == nil {
			item.Action = "create_job"
		} else {
			item.Action = "requeue_job"
		}
		switch {
		case slices.Contains(snapshot.ExcludedUserIDs, candidate.UserID):
			item.Status, item.Reason = "skipped", "用户当前已设置为不审核"
		case candidate.Checkpoint:
		default:
			if !validateInput {
				result.Ready++
				result.Items = append(result.Items, item)
				continue
			}
			capture, readErr := store.Get(ctx, candidate.CaptureID)
			if readErr != nil {
				item.Status, item.Reason = "skipped", readErr.Error()
				break
			}
			protocol, body, parseErr := captureAuditBody(capture)
			if parseErr == nil {
				var input *InputSnapshot
				input, parseErr = CaptureInput(protocol, body)
				if parseErr == nil {
					config := snapshot
					config.Config = effectiveConfigForUser(snapshot.Config, candidate.UserID)
					job := &Job{Config: config, Protocol: protocol, FullInput: input}
					_, parseErr = prepareTarget(job)
				}
			}
			if parseErr != nil {
				item.Status, item.Reason = "skipped", parseErr.Error()
			}
		}
		if item.Status == "ready" {
			result.Ready++
		}
		result.Items = append(result.Items, item)
	}
	return result, nil
}

func (s *Service) PreviewRecoveries(ctx context.Context) (*RecoveryResult, error) {
	return s.recoveryPlan(ctx, false, false)
}
func (s *Service) PreviewAwaitingReviews(ctx context.Context) (*RecoveryResult, error) {
	return s.recoveryPlan(ctx, true, false)
}

func (s *Service) CreateRecoveries(ctx context.Context, actorID int64) (*RecoveryResult, error) {
	return s.createRecoveries(ctx, actorID, false)
}
func (s *Service) CreateAwaitingReviews(ctx context.Context, actorID int64) (*RecoveryResult, error) {
	batch, err := s.repo.CreateBatch(ctx, BatchRequest{Type: BatchPendingReview}, actorID)
	if err != nil {
		return nil, err
	}
	s.notify()
	return &RecoveryResult{Matched: batch.Matched, Ready: batch.Ready, Items: []RecoveryItem{}}, nil
}
func (s *Service) createRecoveries(ctx context.Context, actorID int64, awaitingReview bool) (*RecoveryResult, error) {
	result, err := s.recoveryPlan(ctx, awaitingReview, true)
	if err != nil {
		return nil, err
	}
	snapshot, err := s.config.Active()
	if err != nil {
		return nil, err
	}
	store := NewCaptureStore(s.repo.db)
	for i := range result.Items {
		item := &result.Items[i]
		if item.Status != "ready" {
			continue
		}
		if item.Action == "resume_checkpoint" {
			if item.JobID == nil {
				item.Status, item.Reason = "skipped", "结果检查点未关联任务"
				continue
			}
			if err := s.repo.ResumeResult(ctx, *item.JobID); err != nil {
				item.Status, item.Reason = "failed", err.Error()
			} else {
				item.Status = "resumed"
				s.notify()
			}
			continue
		}
		capture, err := store.Get(ctx, item.CaptureID)
		if err != nil {
			item.Status, item.Reason = "failed", err.Error()
			continue
		}
		protocol, body, err := captureAuditBody(capture)
		if err != nil {
			item.Status, item.Reason = "failed", err.Error()
			continue
		}
		input, err := CaptureInput(protocol, body)
		if err != nil {
			item.Status, item.Reason = "failed", err.Error()
			continue
		}
		effective := snapshot
		effective.Config = effectiveConfigForUser(snapshot.Config, item.userID)
		effective.Mode = "async"
		if item.JobID != nil {
			encodedInput, marshalErr := json.Marshal(input)
			encodedConfig, configErr := json.Marshal(effective)
			if marshalErr != nil {
				err = marshalErr
			} else if configErr != nil {
				err = configErr
			} else {
				var id int64
				err = s.repo.db.QueryRowContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_jobs SET
				 audit_round=audit_round+1,current_run_kind='reaudit',current_requested_by=$1,config_revision=$2,config_snapshot=$3,
				 full_input_snapshot=$4,protocol=$5,snapshot_status='complete',reuse_mode='allow',status='queued',attempts=0,max_attempts=$6,
				 claim_generation=claim_generation+1,lease_until=NULL,next_attempt_at=clock_timestamp(),result_checkpoint=NULL,
				 reuse_metrics='{"whole_lookups":0,"whole_hits":0,"segment_lookups":0,"segment_hits":0,"within_job_hits":0,"inflight_hits":0,"short_circuited_nodes":0}',
				 failure_stage=NULL,last_error_code=NULL,last_error_message=NULL,started_at=NULL,finished_at=NULL,input_manifest=NULL,input_hash=NULL,target_hash=NULL,evaluation_hash=NULL,updated_at=clock_timestamp()
				 WHERE id=$7 AND capture_id=$8 AND status='failed' RETURNING id`, actorID, effective.Revision, string(encodedConfig), string(encodedInput), protocol, MaxEvaluationAttempts, *item.JobID, item.CaptureID).Scan(&id)
				if errors.Is(err, sql.ErrNoRows) {
					item.Status, item.Reason = "already_running", "任务已恢复或正在处理中"
					continue
				}
			}
			if err != nil {
				item.Status, item.Reason = "failed", err.Error()
			} else {
				item.Status = "requeued"
				s.notify()
			}
			continue
		}
		request := IntakeRequest{CapturedAt: capture.CreatedAt, CaptureKey: capture.Key, CaptureID: &capture.ID, RequestID: capture.Key,
			ConversationKey: capture.ConversationKey, UserID: capture.Identity.UserID, APIKeyID: capture.Identity.APIKeyID, GroupID: capture.Identity.GroupID,
			Username: capture.Identity.Username, UserEmail: capture.Identity.UserEmail, APIKeyName: capture.Identity.APIKeyName, GroupName: capture.Identity.GroupName,
			Provider: capture.Identity.Platform, Endpoint: capture.Metadata["path"], Protocol: protocol, Stage: "automatic_failure_recovery", Body: body, Background: true, ActorUserID: actorID}
		var model struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &model) == nil {
			request.Model = model.Model
		}
		job := &Job{CapturedAt: request.CapturedAt, CaptureID: request.CaptureID, CaptureKey: request.CaptureKey, CurrentRunKind: "request", AuditRound: 1,
			CurrentRequestedBy: &actorID, ReuseMode: ReuseModeAllow, UserID: request.UserID, APIKeyID: &request.APIKeyID, GroupID: request.GroupID,
			RequestID: request.RequestID, ConversationKey: request.ConversationKey, Identity: Identity{Username: request.Username, UserEmail: request.UserEmail, APIKeyName: request.APIKeyName, GroupName: request.GroupName, Endpoint: request.Endpoint},
			Platform: request.Provider, Protocol: request.Protocol, IngressStage: request.Stage, RequestedModel: request.Model, ExecutionMode: "async", Config: effective,
			FullInput: input, SnapshotStatus: "complete", Status: "queued", MaxAttempts: MaxEvaluationAttempts}
		var stored *Job
		var created bool
		if _, err = prepareTarget(job); err == nil {
			stored, created, err = s.repo.CreateJob(ctx, job)
		}
		if err != nil {
			item.Status, item.Reason = "failed", err.Error()
			continue
		}
		item.JobID = &stored.ID
		if created {
			item.Status = "created"
		} else {
			item.Status, item.Reason = "already_running", "采集已关联任务"
		}
		_, _ = s.repo.db.ExecContext(ctx, `UPDATE sub2api_enhance.captures SET processing_status='done',last_error_message=NULL,updated_at=clock_timestamp() WHERE id=$1 AND processing_status=$2`, item.CaptureID, map[bool]string{true: "awaiting_review", false: "failed"}[awaitingReview])
		s.notify()
	}
	return result, nil
}

// processRecoveryItem 只处理批次快照中的单条失败采集，保持检查点、原任务和采集唯一性语义。
func (s *Service) processRecoveryItem(ctx context.Context, item BatchItem, actorID int64) (string, string, *int64) {
	if item.CaptureID == nil {
		return "skipped", "批次缺少采集记录", nil
	}
	capture, err := NewCaptureStore(s.repo.db).Get(ctx, *item.CaptureID)
	if err != nil {
		return "failed", err.Error(), nil
	}
	if item.JobID != nil {
		var stage string
		var checkpoint bool
		if err := s.repo.db.QueryRowContext(ctx, `SELECT failure_stage,result_checkpoint IS NOT NULL FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE id=$1 AND capture_id=$2`, *item.JobID, *item.CaptureID).Scan(&stage, &checkpoint); err != nil {
			return "failed", err.Error(), nil
		}
		if checkpoint && stage == "result_persist" {
			if err := s.repo.ResumeResult(ctx, *item.JobID); err != nil {
				return "failed", err.Error(), nil
			}
			id := *item.JobID
			return "resumed", "", &id
		}
		protocol, body, e := captureAuditBody(capture)
		if e != nil {
			return "failed", e.Error(), nil
		}
		input, e := CaptureInput(protocol, body)
		if e != nil {
			return "failed", e.Error(), nil
		}
		snap, e := s.config.Active()
		if e != nil {
			return "failed", e.Error(), nil
		}
		cfg := snap
		cfg.Config = effectiveConfigForUser(snap.Config, capture.Identity.UserID)
		encodedInput, _ := json.Marshal(input)
		encodedCfg, _ := json.Marshal(cfg)
		var id int64
		e = s.repo.db.QueryRowContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_jobs SET audit_round=audit_round+1,current_run_kind='reaudit',current_requested_by=$1,config_revision=$2,config_snapshot=$3,full_input_snapshot=$4,protocol=$5,snapshot_status='complete',reuse_mode='allow',status='queued',attempts=0,max_attempts=$6,claim_generation=claim_generation+1,lease_until=NULL,next_attempt_at=clock_timestamp(),result_checkpoint=NULL,failure_stage=NULL,last_error_code=NULL,last_error_message=NULL,started_at=NULL,finished_at=NULL,input_manifest=NULL,input_hash=NULL,target_hash=NULL,evaluation_hash=NULL,updated_at=clock_timestamp() WHERE id=$7 AND capture_id=$8 AND status='failed' AND ($9::int IS NULL OR audit_round=$9) RETURNING id`, actorID, cfg.Revision, string(encodedCfg), string(encodedInput), protocol, MaxEvaluationAttempts, *item.JobID, *item.CaptureID, item.SourceAuditRound).Scan(&id)
		if errors.Is(e, sql.ErrNoRows) {
			return "already_running", "任务状态已改变", nil
		}
		if e != nil {
			return "failed", e.Error(), nil
		}
		return "requeued", "", &id
	}
	capture.Metadata["background"] = "true"
	d, e := s.AuditCapture(ctx, capture)
	if e != nil {
		return "failed", e.Error(), nil
	}
	if d == nil || d.JobID <= 0 {
		return "skipped", "无法创建审核任务", nil
	}
	return "created", "", &d.JobID
}
