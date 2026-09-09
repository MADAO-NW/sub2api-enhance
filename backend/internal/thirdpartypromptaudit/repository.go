package thirdpartypromptaudit

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sub2api-enhance/internal/pkg/logger"
	"time"
	"unicode/utf8"
)

// ErrLeaseLost 防止过期执行者继续写入分类或处置。
var ErrLeaseLost = errors.New("审核任务租约已失效")

// ErrAuditPaused 表示当前配置已撤销尚未发起的审核调用许可。
var ErrAuditPaused = errors.New("审核已暂停，等待重新启用")

// ErrNotFound 表示请求的本模块业务记录不存在。
var ErrNotFound = errors.New("审核记录不存在")

// leaseDuration 为可续租任务提供进程故障恢复窗口。
const leaseDuration = 30 * time.Second

type Repository struct{ db *sql.DB }

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// jobColumns 仅列举列表需要的字段，全文通过单独投影读取，避免列表展开大字段。
const jobColumns = `COALESCE(capture.created_at,j.created_at) AS captured_at,j.capture_id,j.id,j.capture_key,j.user_id,j.api_key_id,j.group_id,
j.request_id,j.conversation_key,j.identity_snapshot,COALESCE(NULLIF(display_user.username,''),j.identity_snapshot::json->>'username','') AS display_username,
COALESCE(NULLIF(display_user.email,''),j.identity_snapshot::json->>'user_email','') AS display_email,j.platform,j.protocol,j.ingress_stage,j.requested_model,j.execution_mode,j.audit_round,j.current_run_kind,j.current_requested_by,
j.reuse_mode,j.disable_counted,j.config_revision,j.snapshot_status,j.input_hash,j.target_hash,j.evaluation_hash,j.status,j.attempts,j.max_attempts,
j.claim_generation,j.lease_until,j.next_attempt_at,j.reuse_metrics,j.failure_stage,j.last_error_code,j.last_error_message,
j.gateway_result,j.gateway_completed_at,j.gateway_duration_ms,j.started_at,j.finished_at,
CASE WHEN j.started_at IS NULL THEN NULL ELSE GREATEST(0,extract(epoch FROM (COALESCE(j.finished_at,clock_timestamp())-j.started_at))*1000)::bigint END AS duration_ms,j.created_at,j.updated_at`

// jobInputJoins 为任务投影补齐唯一采集记录和当前用户展示信息。
const jobInputJoins = ` LEFT JOIN sub2api_enhance.captures capture ON capture.id=j.capture_id LEFT JOIN public.users display_user ON display_user.id=j.user_id AND display_user.deleted_at IS NULL `

// jobSource 是所有任务查询共享的根任务来源。
const jobSource = ` FROM sub2api_enhance.third_party_prompt_audit_jobs j` + jobInputJoins

func jobProjection(full bool) string {
	if full {
		return jobColumns + `,j.config_snapshot,j.full_input_snapshot,j.input_manifest,j.result_checkpoint`
	}
	return jobColumns + `,json_build_object('revision',j.config_revision,'review_threshold',j.config_snapshot::json->'review_threshold','block_threshold',j.config_snapshot::json->'block_threshold') AS decision_config`
}

func decodeJob(raw []byte) (*Job, error) {
	job := &Job{}
	record := struct {
		*Job
		IdentityRaw   string  `json:"identity_snapshot"`
		ConfigRaw     string  `json:"config_snapshot"`
		InputRaw      *string `json:"full_input_snapshot"`
		ManifestRaw   *string `json:"input_manifest"`
		CheckpointRaw *string `json:"result_checkpoint"`
		ReuseRaw      string  `json:"reuse_metrics"`
	}{Job: job}
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	for _, field := range []struct {
		raw    string
		target any
	}{
		{record.IdentityRaw, &job.Identity}, {record.ConfigRaw, &job.Config}, {record.ReuseRaw, &job.Reuse},
	} {
		if field.raw != "" {
			if err := json.Unmarshal([]byte(field.raw), field.target); err != nil {
				return nil, err
			}
		}
	}
	if record.InputRaw != nil {
		if err := json.Unmarshal([]byte(*record.InputRaw), &job.FullInput); err != nil {
			return nil, err
		}
	}
	if record.ManifestRaw != nil {
		if err := json.Unmarshal([]byte(*record.ManifestRaw), &job.Manifest); err != nil {
			return nil, err
		}
	}
	if record.CheckpointRaw != nil {
		if err := json.Unmarshal([]byte(*record.CheckpointRaw), &job.Checkpoint); err != nil {
			return nil, err
		}
	}
	job.LastErrorMessage = decodeStoredText(job.LastErrorMessage)
	job.RequestedModel = decodeStoredText(job.RequestedModel)
	if record.ConfigRaw != "" {
		job.DecisionConfig = &DecisionConfig{Revision: job.Config.Revision, ReviewThreshold: job.Config.ReviewThreshold, BlockThreshold: job.Config.BlockThreshold}
	}
	return job, nil
}

func (r *Repository) GetJob(ctx context.Context, id int64, full bool) (*Job, error) {
	var raw []byte
	err := r.db.QueryRowContext(ctx, `SELECT row_to_json(record) FROM (SELECT `+jobProjection(full)+jobSource+`WHERE j.id=$1) record`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	job, err := decodeJob(raw)
	if err != nil {
		return nil, err
	}
	return job, nil
}

// CreateJob 以一次 INSERT 原子保存输入和执行快照；后台无需依赖另一份队列载荷。
func (r *Repository) CreateJob(ctx context.Context, job *Job) (*Job, bool, error) {
	identity, err := json.Marshal(job.Identity)
	if err != nil {
		return nil, false, err
	}
	config, err := json.Marshal(job.Config)
	if err != nil {
		return nil, false, err
	}
	if job.FullInput == nil {
		return nil, false, errors.New("审核任务缺少输入快照")
	}
	raw, err := json.Marshal(job.FullInput)
	if err != nil {
		return nil, false, err
	}
	input := string(raw)
	manifest, err := json.Marshal(job.Manifest)
	if err != nil {
		return nil, false, err
	}
	if job.Status == "" {
		job.Status = "queued"
	}
	if job.MaxAttempts == 0 {
		job.MaxAttempts = MaxEvaluationAttempts
	}
	if job.ReuseMode == "" {
		job.ReuseMode = ReuseModeAllow
	}
	if job.Status == "processing" {
		job.Attempts = 0
		job.ClaimGeneration = 1
	}
	err = r.db.QueryRowContext(ctx, `
INSERT INTO sub2api_enhance.third_party_prompt_audit_jobs
(capture_key,user_id,api_key_id,group_id,request_id,conversation_key,identity_snapshot,
 platform,protocol,ingress_stage,requested_model,execution_mode,config_revision,config_snapshot,full_input_snapshot,
 snapshot_status,input_manifest,input_hash,target_hash,evaluation_hash,status,attempts,max_attempts,claim_generation,reuse_mode,
 lease_until,started_at,finished_at,failure_stage,last_error_code,last_error_message,current_requested_by,capture_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,
 CASE WHEN $21='processing' THEN clock_timestamp()+$26::interval END,
 CASE WHEN $21='processing' THEN clock_timestamp() END,
 CASE WHEN $21 IN ('failed','skipped') THEN clock_timestamp() END,$27,$28,$29,$30,$31)
ON CONFLICT (capture_key) DO NOTHING
RETURNING id,created_at,updated_at,lease_until,started_at,finished_at,next_attempt_at`,
		job.CaptureKey, job.UserID, job.APIKeyID, job.GroupID, job.RequestID, nullIfEmpty(job.ConversationKey), string(identity),
		job.Platform, job.Protocol, job.IngressStage, encodeStoredText(job.RequestedModel),
		job.ExecutionMode, job.Config.Revision, string(config), input, job.SnapshotStatus, string(manifest),
		job.InputHash, job.TargetHash, job.EvaluationHash, job.Status, job.Attempts, job.MaxAttempts,
		job.ClaimGeneration, job.ReuseMode, interval(leaseDuration), job.FailureStage, job.LastErrorCode, encodeStoredText(job.LastErrorMessage), job.CurrentRequestedBy, job.CaptureID,
	).Scan(&job.ID, &job.CreatedAt, &job.UpdatedAt, &job.LeaseUntil, &job.StartedAt, &job.FinishedAt, &job.NextAttemptAt)
	if errors.Is(err, sql.ErrNoRows) {
		var id int64
		if err := r.db.QueryRowContext(ctx, `SELECT id FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE capture_key=$1 AND user_id=$2`, job.CaptureKey, job.UserID).Scan(&id); err != nil {
			return nil, false, err
		}
		existing, err := r.GetJob(ctx, id, true)
		return existing, false, err
	}
	if err != nil {
		return nil, false, err
	}
	job.GatewayResult = "not_observed"
	return job, true, nil
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (r *Repository) Claim(ctx context.Context, allowEvaluation bool) (*Job, error) {
	var id int64
	err := r.db.QueryRowContext(ctx, `
WITH candidate AS (
 SELECT id FROM sub2api_enhance.third_party_prompt_audit_jobs
 WHERE status IN ('queued','retry') AND next_attempt_at<=clock_timestamp()
	 AND (result_checkpoint IS NOT NULL OR ($1 AND (execution_mode='async' OR current_run_kind='reaudit') AND (attempts<max_attempts OR last_error_code IN ('audit_paused','worker_paused'))))
 ORDER BY next_attempt_at,id FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE sub2api_enhance.third_party_prompt_audit_jobs j SET status='processing',claim_generation=claim_generation+1,
 attempts=attempts+CASE WHEN result_checkpoint IS NULL AND COALESCE(last_error_code,'') NOT IN ('audit_paused','worker_paused') THEN 1 ELSE 0 END,
 lease_until=clock_timestamp()+$2::interval,started_at=COALESCE(started_at,clock_timestamp()),updated_at=clock_timestamp()
FROM candidate WHERE j.id=candidate.id RETURNING j.id`, allowEvaluation, interval(leaseDuration)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r.GetJob(ctx, id, true)
}

func (r *Repository) Renew(ctx context.Context, job *Job) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_jobs SET lease_until=clock_timestamp()+$3::interval,updated_at=clock_timestamp() WHERE id=$1 AND claim_generation=$2 AND status='processing' AND lease_until>clock_timestamp()`, job.ID, job.ClaimGeneration, interval(leaseDuration))
	return checkLeaseUpdate(result, err)
}

func (r *Repository) RecoverExpired(ctx context.Context) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var jobID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE status='processing' AND lease_until<=clock_timestamp() ORDER BY lease_until,id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// 回收租约时一并标记未确认的调用，未知响应不伪装为模型失败。
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_model_attempts SET status=CASE WHEN dispatch_started_at IS NULL THEN 'failed' ELSE 'unknown' END,error_code=CASE WHEN dispatch_started_at IS NULL THEN 'not_dispatched' ELSE 'worker_lost' END,error_message=$1,finished_at=clock_timestamp() WHERE job_id=$2 AND status IN ('prepared','started')`, encodeStoredText("执行进程失联，调用结果未知"), jobID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_jobs SET
 status=CASE WHEN result_checkpoint IS NOT NULL OR (execution_mode='async' AND attempts<max_attempts) THEN 'retry' ELSE 'failed' END,
 finished_at=CASE WHEN result_checkpoint IS NULL AND (execution_mode='blocking' OR attempts>=max_attempts) THEN clock_timestamp() END,
 claim_generation=claim_generation+1,lease_until=NULL,next_attempt_at=clock_timestamp(),updated_at=clock_timestamp(),
 failure_stage='worker',last_error_code='worker_lost',last_error_message=$1
 WHERE id=$2`, encodeStoredText("执行租约过期，已回收任务"), jobID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repository) SaveCheckpoint(ctx context.Context, job *Job, evaluation *Evaluation) error {
	raw, err := json.Marshal(evaluation)
	if err != nil {
		return err
	}
	reuse, err := json.Marshal(job.Reuse)
	if err != nil {
		return err
	}
	result, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_jobs SET result_checkpoint=$3,reuse_metrics=$4,updated_at=clock_timestamp() WHERE id=$1 AND claim_generation=$2 AND status='processing' AND lease_until>clock_timestamp()`, job.ID, job.ClaimGeneration, string(raw), string(reuse))
	if err := checkLeaseUpdate(result, err); err != nil {
		return err
	}
	job.Checkpoint = evaluation
	return nil
}

func (r *Repository) Fail(ctx context.Context, job *Job, failure *AuditError, retry bool) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var owned bool
	err = tx.QueryRowContext(ctx, `SELECT claim_generation=$2 AND status='processing' AND lease_until>clock_timestamp() FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE id=$1 FOR UPDATE`, job.ID, job.ClaimGeneration).Scan(&owned)
	if err != nil {
		return err
	}
	if !owned {
		return ErrLeaseLost
	}
	status := "failed"
	if retry {
		status = "retry"
	}
	wait := failure.RetryAfter
	if wait <= 0 {
		wait = time.Duration(max(job.Attempts, 1)*max(job.Attempts, 1)) * time.Second
	}
	reuse, err := json.Marshal(job.Reuse)
	if err != nil {
		return err
	}
	// 退出本轮时终结未确认调用，后续重试不能留下永久的 started。
	_, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_model_attempts SET
      status=CASE WHEN dispatch_started_at IS NULL THEN 'failed' ELSE 'unknown' END,
      error_code=CASE WHEN dispatch_started_at IS NULL THEN 'not_dispatched' ELSE 'result_unconfirmed' END,
      error_message=$4,finished_at=clock_timestamp() WHERE job_id=$1 AND evaluation_round=$2 AND audit_round=$3 AND status IN ('prepared','started')`, job.ID, job.Attempts, jobAuditRound(job), encodeStoredText(failure.Message))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_jobs SET status=$2,failure_stage=$3,last_error_code=$4,last_error_message=$5,reuse_metrics=$6,
      next_attempt_at=clock_timestamp()+$7::interval,finished_at=CASE WHEN $2='failed' THEN clock_timestamp() END,lease_until=NULL,updated_at=clock_timestamp() WHERE id=$1`,
		job.ID, status, failure.Stage, failure.Code, encodeStoredText(failure.Message), string(reuse), interval(wait))
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	logger.LegacyPrintf("third_party_prompt_audit", "审核任务本轮处理结束 job_id=%d status=%s failure_stage=%s error_code=%s error=%s", job.ID, status, failure.Stage, failure.Code, failure.Message)
	return nil
}

func (r *Repository) BeginForegroundEvaluation(ctx context.Context, job *Job) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_jobs SET attempts=1,updated_at=clock_timestamp() WHERE id=$1 AND claim_generation=$2 AND attempts=0 AND status='processing' AND lease_until>clock_timestamp()`, job.ID, job.ClaimGeneration)
	if err := checkLeaseUpdate(result, err); err != nil {
		return err
	}
	job.Attempts = 1
	return nil
}

// BindEvaluationConfig 在模型调用前以当前租约冻结本次评估配置，原文快照保持不变。
func (r *Repository) BindEvaluationConfig(ctx context.Context, job *Job, snapshot ConfigSnapshot) error {
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	result, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_jobs SET config_revision=$3,config_snapshot=$4,
 input_manifest=NULL,input_hash=NULL,target_hash=NULL,evaluation_hash=NULL,updated_at=clock_timestamp()
 WHERE id=$1 AND claim_generation=$2 AND status='processing' AND lease_until>clock_timestamp()`, job.ID, job.ClaimGeneration, snapshot.Revision, string(raw))
	if err := checkLeaseUpdate(result, err); err != nil {
		return err
	}
	job.Config = snapshot
	job.Manifest = nil
	job.InputHash, job.TargetHash, job.EvaluationHash = "", "", ""
	return nil
}

func (r *Repository) RecordGateway(ctx context.Context, jobID int64, result string, duration time.Duration) error {
	_, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_jobs SET gateway_result=$2,gateway_completed_at=clock_timestamp(),gateway_duration_ms=$3,updated_at=clock_timestamp() WHERE id=$1 AND gateway_result='not_observed'`, jobID, result, duration.Milliseconds())
	return err
}

func checkLeaseUpdate(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrLeaseLost
	}
	return nil
}

func interval(duration time.Duration) string { return fmt.Sprintf("%.6f seconds", duration.Seconds()) }

// encodeStoredText 可逆保存 NUL 和非 UTF-8 外部响应，避免数据库拒绝或静默替换字符。
func encodeStoredText(value string) string {
	encoding, data := "utf8", value
	if !utf8.ValidString(value) {
		encoding, data = "base64", base64.StdEncoding.EncodeToString([]byte(value))
	}
	raw, _ := json.Marshal(struct {
		Encoding string `json:"encoding"`
		Data     string `json:"data"`
	}{encoding, data})
	return string(raw)
}

func decodeStoredText(value string) string {
	var encoded struct {
		Encoding string `json:"encoding"`
		Data     string `json:"data"`
	}
	if err := json.Unmarshal([]byte(value), &encoded); err != nil {
		return value
	}
	if encoded.Encoding == "utf8" {
		return encoded.Data
	}
	if encoded.Encoding == "base64" {
		if raw, err := base64.StdEncoding.DecodeString(encoded.Data); err == nil {
			return string(raw)
		}
	}
	return value
}
