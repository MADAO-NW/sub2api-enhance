package thirdpartypromptaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/lib/pq"
	"sub2api-enhance/internal/pkg/logger"
)

// auditJobColumns 是任务列表与详情共用的投影；展示用户名优先读取当前用户资料。
const auditJobColumns = `j.id,j.user_id,j.api_key_id,j.group_id,j.identity_snapshot,j.platform,j.request_id,j.model,j.protocol,j.client_fingerprint,j.mode,
j.turn_fingerprint,j.orphan_continuation,j.status,COALESCE(j.decision,'') AS decision,j.current_round,j.current_run_kind,j.current_requested_by,j.reuse_mode,
COALESCE(j.trigger_hash,'') AS trigger_hash,COALESCE(j.trigger_kind,'') AS trigger_kind,COALESCE(j.trigger_categories,'{}') AS trigger_categories,j.reuse_after,j.enforcement_eligible,j.violation_counted,j.disable_counted,
j.config_revision,j.config_snapshot,j.attempts,j.max_attempts,j.claim_generation,j.lease_until,j.next_attempt_at,COALESCE(j.failure_stage,'') AS failure_stage,
COALESCE(j.last_error_code,'') AS last_error_code,COALESCE(j.last_error_message,'') AS last_error_message,j.blocking_wait_ms,
COALESCE((EXTRACT(EPOCH FROM (j.finished_at-j.created_at))*1000)::bigint,0) AS duration_ms,
COALESCE((EXTRACT(EPOCH FROM (j.finished_at-j.started_at))*1000)::bigint,0) AS processing_ms,j.started_at,j.finished_at,j.created_at,j.updated_at,
EXISTS(SELECT 1 FROM sub2api_enhance.audit_job_targets st JOIN sub2api_enhance.policy_samples ss ON ss.hash=st.hash
 WHERE st.job_id=j.id AND st.round=j.current_round) AS sampled,
COALESCE(NULLIF(display_user.username,''),j.identity_snapshot::json->>'username','') AS display_username,
COALESCE(NULLIF(display_user.email,''),j.identity_snapshot::json->>'user_email','') AS display_email,
(SELECT count(*) FROM sub2api_enhance.audit_job_targets t WHERE t.job_id=j.id AND t.round=j.current_round) AS target_count`

// auditJobSource 是任务查询共享的来源；用户表只读，用于展示当前用户名。
const auditJobSource = ` FROM sub2api_enhance.audit_jobs j LEFT JOIN public.users display_user ON display_user.id=j.user_id AND display_user.deleted_at IS NULL `

// leaseValidSQL 校验当前执行者仍持有任务租约，$1 为任务 ID，$2 为领取代次。
const leaseValidSQL = `EXISTS (SELECT 1 FROM sub2api_enhance.audit_jobs lj WHERE lj.id=$1 AND lj.claim_generation=$2 AND lj.status='processing' AND lj.lease_until>clock_timestamp())`

// readyToCompleteSQL 表示本轮目标已全部得出结论，或已有目标判定违规（可提前结束）。
const readyToCompleteSQL = `(NOT EXISTS (SELECT 1 FROM sub2api_enhance.audit_job_targets rt WHERE rt.job_id=j.id AND rt.round=j.current_round AND rt.resolution IS NULL)
 OR EXISTS (SELECT 1 FROM sub2api_enhance.audit_job_targets rt WHERE rt.job_id=j.id AND rt.round=j.current_round AND rt.decision='block'))`

func decodeAuditJob(raw []byte) (*AuditJob, error) {
	job := &AuditJob{}
	record := struct {
		*AuditJob
		IdentityRaw     string  `json:"identity_snapshot"`
		ConfigRaw       *string `json:"config_snapshot"`
		ClaimGeneration int64   `json:"claim_generation"`
	}{AuditJob: job}
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	job.ClaimGeneration = record.ClaimGeneration
	if record.IdentityRaw != "" {
		if err := json.Unmarshal([]byte(record.IdentityRaw), &job.Identity); err != nil {
			return nil, err
		}
	}
	if record.ConfigRaw != nil && *record.ConfigRaw != "" {
		if err := json.Unmarshal([]byte(*record.ConfigRaw), &job.Config); err != nil {
			return nil, err
		}
	}
	job.LastErrorMessage = decodeStoredText(job.LastErrorMessage)
	return job, nil
}

// GetAuditJob 读取一个任务的完整状态，不包含任何请求原文。
func (r *Repository) GetAuditJob(ctx context.Context, id int64) (*AuditJob, error) {
	var raw []byte
	err := r.db.QueryRowContext(ctx, `SELECT row_to_json(record) FROM (SELECT `+auditJobColumns+auditJobSource+`WHERE j.id=$1) record`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return decodeAuditJob(raw)
}

// sanitizeStoredText 去掉 PostgreSQL TEXT 无法保存的 NUL 与非法 UTF-8，仅用于展示类字段。
func sanitizeStoredText(value string) string {
	return strings.ToValidUTF8(strings.ReplaceAll(value, "\x00", "\uFFFD"), "\uFFFD")
}

// CreateAuditJob 在一个事务内保存去重文本、根任务和第 1 轮目标；同步任务直接以处理中状态创建并持有租约。
func (r *Repository) CreateAuditJob(ctx context.Context, job *AuditJob, targets []PromptText) error {
	if len(targets) == 0 {
		return errors.New("审核任务至少需要一个目标")
	}
	identity, err := json.Marshal(job.Identity)
	if err != nil {
		return err
	}
	if job.MaxAttempts == 0 {
		job.MaxAttempts = MaxEvaluationAttempts
	}
	if job.CurrentRound == 0 {
		job.CurrentRound = 1
	}
	if job.CurrentRunKind == "" {
		job.CurrentRunKind = "request"
	}
	if job.ReuseMode == "" {
		job.ReuseMode = ReuseModeAllow
	}
	if job.Status == "" {
		job.Status = "queued"
	}
	if job.Status == "processing" {
		job.Attempts, job.ClaimGeneration = 0, 1
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	hashes, kinds, texts, sizes := make([]string, 0, len(targets)), make([]string, 0, len(targets)), make([]string, 0, len(targets)), make([]int64, 0, len(targets))
	simhashes := make([]int64, 0, len(targets))
	for _, target := range targets {
		hashes, kinds, texts, sizes = append(hashes, target.Hash), append(kinds, target.Kind), append(texts, target.Text), append(sizes, int64(len(target.Text)))
		simhashes = append(simhashes, target.SimHash)
	}
	// 内容寻址文本只插入、不更新，避免共享指令块形成热点行。
	if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.prompt_texts(hash,kind,text,bytes,simhash)
 SELECT * FROM unnest($1::text[],$2::text[],$3::text[],$4::int[],$5::bigint[]) ON CONFLICT (hash) DO NOTHING`, pq.Array(hashes), pq.Array(kinds), pq.Array(texts), pq.Array(sizes), pq.Array(simhashes)); err != nil {
		return err
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.audit_jobs
 (user_id,api_key_id,group_id,identity_snapshot,platform,request_id,model,protocol,client_fingerprint,mode,turn_fingerprint,orphan_continuation,
  status,current_round,current_run_kind,reuse_mode,attempts,max_attempts,claim_generation,lease_until,started_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,
  CASE WHEN $13='processing' THEN clock_timestamp()+$20::interval END,CASE WHEN $13='processing' THEN clock_timestamp() END)
 RETURNING id,created_at,updated_at,lease_until,started_at,next_attempt_at`,
		job.UserID, job.APIKeyID, job.GroupID, string(identity), job.Platform, job.RequestID, sanitizeStoredText(job.Model), job.Protocol, job.ClientFingerprint,
		job.Mode, job.TurnFingerprint, job.OrphanContinuation, job.Status, job.CurrentRound, job.CurrentRunKind, job.ReuseMode, job.Attempts, job.MaxAttempts,
		job.ClaimGeneration, interval(leaseDuration)).Scan(&job.ID, &job.CreatedAt, &job.UpdatedAt, &job.LeaseUntil, &job.StartedAt, &job.NextAttemptAt)
	if err != nil {
		return err
	}
	orders := make([]int64, 0, len(targets))
	for i := range targets {
		orders = append(orders, int64(i+1))
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.audit_job_targets(job_id,round,target_order,hash,kind)
 SELECT $1,$2,target_order,hash,kind FROM unnest($3::int[],$4::text[],$5::text[]) AS item(target_order,hash,kind)`,
		job.ID, job.CurrentRound, pq.Array(orders), pq.Array(hashes), pq.Array(kinds)); err != nil {
		return err
	}
	job.TargetCount = len(targets)
	return tx.Commit()
}

// ClaimAuditJob 以 SKIP LOCKED 领取一个可执行任务；只有仍有未完成目标时才消耗评估预算。
func (r *Repository) ClaimAuditJob(ctx context.Context, allowEvaluation bool) (*AuditJob, error) {
	var id int64
	err := r.db.QueryRowContext(ctx, `
WITH candidate AS (
 SELECT j.id FROM sub2api_enhance.audit_jobs j
 WHERE j.status IN ('queued','retry') AND j.next_attempt_at<=clock_timestamp()
  AND (`+readyToCompleteSQL+` OR ($1 AND (j.mode='async' OR j.current_run_kind='reaudit') AND (j.attempts<j.max_attempts OR j.last_error_code IN ('audit_paused','worker_paused'))))
 ORDER BY j.next_attempt_at,j.id FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE sub2api_enhance.audit_jobs j SET status='processing',claim_generation=j.claim_generation+1,
 attempts=j.attempts+CASE WHEN NOT `+readyToCompleteSQL+` AND COALESCE(j.last_error_code,'') NOT IN ('audit_paused','worker_paused') THEN 1 ELSE 0 END,
 lease_until=clock_timestamp()+$2::interval,started_at=COALESCE(j.started_at,clock_timestamp()),updated_at=clock_timestamp()
FROM candidate WHERE j.id=candidate.id RETURNING j.id`, allowEvaluation, interval(leaseDuration)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r.GetAuditJob(ctx, id)
}

// RenewAuditJob 续租当前执行；租约失效时返回 ErrLeaseLost，过期执行者不能再写入结果。
func (r *Repository) RenewAuditJob(ctx context.Context, job *AuditJob) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.audit_jobs SET lease_until=clock_timestamp()+$3::interval,updated_at=clock_timestamp()
 WHERE id=$1 AND claim_generation=$2 AND status='processing' AND lease_until>clock_timestamp()`, job.ID, job.ClaimGeneration, interval(leaseDuration))
	return checkLeaseUpdate(result, err)
}

// RecoverExpiredAuditJobs 回收一个租约过期的任务，并把未确认的模型调用标为未知。
func (r *Repository) RecoverExpiredAuditJobs(ctx context.Context) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var jobID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM sub2api_enhance.audit_jobs WHERE status='processing' AND lease_until<=clock_timestamp() ORDER BY lease_until,id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.model_attempts SET status=CASE WHEN dispatch_started_at IS NULL THEN 'failed' ELSE 'unknown' END,
 error_code=CASE WHEN dispatch_started_at IS NULL THEN 'not_dispatched' ELSE 'worker_lost' END,error_message=$1,finished_at=clock_timestamp(),updated_at=clock_timestamp()
 WHERE job_id=$2 AND status IN ('prepared','started')`, encodeStoredText("执行进程失联，调用结果未知"), jobID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.audit_jobs j SET
 status=CASE WHEN `+readyToCompleteSQL+` OR ((j.mode='async' OR j.current_run_kind='reaudit') AND j.attempts<j.max_attempts) THEN 'retry' ELSE 'failed' END,
 finished_at=CASE WHEN `+readyToCompleteSQL+` OR ((j.mode='async' OR j.current_run_kind='reaudit') AND j.attempts<j.max_attempts) THEN NULL ELSE clock_timestamp() END,
 claim_generation=j.claim_generation+1,lease_until=NULL,next_attempt_at=clock_timestamp(),updated_at=clock_timestamp(),
 failure_stage='worker',last_error_code='worker_lost',last_error_message=$1 WHERE j.id=$2`, encodeStoredText("执行租约过期，已回收任务"), jobID); err != nil {
		return err
	}
	return tx.Commit()
}

// FailAuditJob 结束本次执行；已得出结论的目标保留，重试时只补未完成的目标。
func (r *Repository) FailAuditJob(ctx context.Context, job *AuditJob, failure *AuditError, retry bool) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var owned bool
	err = tx.QueryRowContext(ctx, `SELECT claim_generation=$2 AND status='processing' AND lease_until>clock_timestamp() FROM sub2api_enhance.audit_jobs WHERE id=$1 FOR UPDATE`, job.ID, job.ClaimGeneration).Scan(&owned)
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
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.model_attempts SET
 status=CASE WHEN dispatch_started_at IS NULL THEN 'failed' ELSE 'unknown' END,
 error_code=CASE WHEN dispatch_started_at IS NULL THEN 'not_dispatched' ELSE 'result_unconfirmed' END,
 error_message=$3,finished_at=clock_timestamp(),updated_at=clock_timestamp() WHERE job_id=$1 AND audit_round=$2 AND status IN ('prepared','started')`,
		job.ID, job.CurrentRound, encodeStoredText(failure.Message)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.audit_jobs SET status=$2,failure_stage=$3,last_error_code=$4,last_error_message=$5,
 next_attempt_at=clock_timestamp()+$6::interval,finished_at=CASE WHEN $2='failed' THEN clock_timestamp() END,lease_until=NULL,updated_at=clock_timestamp() WHERE id=$1`,
		job.ID, status, failure.Stage, failure.Code, encodeStoredText(failure.Message), interval(wait)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	logger.LegacyPrintf("third_party_prompt_audit", "审核任务本轮处理结束 job_id=%d status=%s failure_stage=%s error_code=%s error=%s", job.ID, status, failure.Stage, failure.Code, failure.Message)
	return nil
}

// DeferAuditJob 在首个模型请求前遇到容量满载或冷却时退回队列，并撤销本次执行计数。
func (r *Repository) DeferAuditJob(ctx context.Context, job *AuditJob, failure *AuditError) error {
	wait := failure.RetryAfter
	if wait <= 0 {
		wait = time.Second
	}
	result, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.audit_jobs SET
 status='retry',attempts=GREATEST(0,attempts-1),failure_stage=$3,last_error_code=$4,last_error_message=$5,
 next_attempt_at=clock_timestamp()+$6::interval,lease_until=NULL,updated_at=clock_timestamp()
 WHERE id=$1 AND claim_generation=$2 AND status='processing' AND lease_until>clock_timestamp()`,
		job.ID, job.ClaimGeneration, failure.Stage, failure.Code, encodeStoredText(failure.Message), interval(wait))
	return checkLeaseUpdate(result, err)
}

// SkipExcludedAuditJob 在尚未发出模型调用时终结被排除用户的任务，不产生处置。
func (r *Repository) SkipExcludedAuditJob(ctx context.Context, job *AuditJob) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.model_attempts SET status='failed',error_code='user_excluded',error_message=$2,finished_at=clock_timestamp(),updated_at=clock_timestamp()
 WHERE job_id=$1 AND status='prepared'`, job.ID, encodeStoredText(ErrUserExcluded.Error())); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.audit_jobs SET
 status='skipped',attempts=GREATEST(0,attempts-1),failure_stage=NULL,last_error_code='user_excluded',last_error_message=$3,
 finished_at=clock_timestamp(),lease_until=NULL,updated_at=clock_timestamp()
 WHERE id=$1 AND claim_generation=$2 AND status='processing' AND lease_until>clock_timestamp()`, job.ID, job.ClaimGeneration, encodeStoredText(ErrUserExcluded.Error()))
	if err := checkLeaseUpdate(result, err); err != nil {
		return err
	}
	return tx.Commit()
}

// BeginForegroundAuditJob 记录同步审核取得 Worker 槽位后的第一次执行。
func (r *Repository) BeginForegroundAuditJob(ctx context.Context, job *AuditJob) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.audit_jobs SET attempts=1,updated_at=clock_timestamp()
 WHERE id=$1 AND claim_generation=$2 AND attempts=0 AND status='processing' AND lease_until>clock_timestamp()`, job.ID, job.ClaimGeneration)
	if err := checkLeaseUpdate(result, err); err != nil {
		return err
	}
	job.Attempts = 1
	return nil
}

// BindAuditJobConfig 在首个模型调用前以当前租约冻结本次执行的配置快照。
func (r *Repository) BindAuditJobConfig(ctx context.Context, job *AuditJob, snapshot ConfigSnapshot) error {
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	result, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.audit_jobs SET config_revision=$3,config_snapshot=$4,updated_at=clock_timestamp()
 WHERE id=$1 AND claim_generation=$2 AND status='processing' AND lease_until>clock_timestamp()`, job.ID, job.ClaimGeneration, snapshot.Revision, string(raw))
	if err := checkLeaseUpdate(result, err); err != nil {
		return err
	}
	job.Config = snapshot
	revision := snapshot.Revision
	job.ConfigRevision = &revision
	return nil
}

// LoadRoundTargets 读取某一轮的全部目标及正文、信任状态，按目标顺序返回。
func (r *Repository) LoadRoundTargets(ctx context.Context, jobID int64, round int) ([]JobTarget, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT t.round,t.target_order,t.hash,t.kind,COALESCE(p.text,''),COALESCE(p.bytes,0),COALESCE(t.resolution,''),t.review_id,
 COALESCE(t.decision,''),COALESCE(t.error_code,''),COALESCE(t.error_message,''),t.resolved_at,EXISTS(SELECT 1 FROM sub2api_enhance.prompt_trust pt WHERE pt.hash=t.hash)
 FROM sub2api_enhance.audit_job_targets t LEFT JOIN sub2api_enhance.prompt_texts p ON p.hash=t.hash
 WHERE t.job_id=$1 AND t.round=$2 ORDER BY t.target_order`, jobID, round)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]JobTarget, 0)
	for rows.Next() {
		var item JobTarget
		var decision string
		if err := rows.Scan(&item.Round, &item.Order, &item.Hash, &item.Kind, &item.Text, &item.Bytes, &item.Resolution, &item.ReviewID,
			&decision, &item.ErrorCode, &item.ErrorMessage, &item.ResolvedAt, &item.Trusted); err != nil {
			return nil, err
		}
		item.Decision = Decision(decision)
		item.ErrorMessage = decodeStoredText(item.ErrorMessage)
		items = append(items, item)
	}
	return items, rows.Err()
}

// TrustedHashes 返回给定哈希中已被管理员信任的集合。
func (r *Repository) TrustedHashes(ctx context.Context, hashes []string) (map[string]bool, error) {
	result := make(map[string]bool)
	if len(hashes) == 0 {
		return result, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT hash FROM sub2api_enhance.prompt_trust WHERE hash=ANY($1)`, pq.Array(hashes))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		result[hash] = true
	}
	return result, rows.Err()
}

const reviewColumns = `id,hash,eval_hash,run_kind,decision,score,reason,node_results,reusable,requested_by,origin_job_id,created_at`

func scanReview(row rowScanner) (*PromptReview, error) {
	var review PromptReview
	var nodes []byte
	var decision string
	if err := row.Scan(&review.ID, &review.Hash, &review.EvalHash, &review.RunKind, &decision, &review.Score, &review.Reason, &nodes,
		&review.Reusable, &review.RequestedBy, &review.OriginJobID, &review.CreatedAt); err != nil {
		return nil, err
	}
	review.Decision = Decision(decision)
	review.Reason = decodeStoredText(review.Reason)
	if err := json.Unmarshal(nodes, &review.Nodes); err != nil {
		return nil, err
	}
	return &review, nil
}

// FindReusableReview 读取同一内容、同一评估语义下最新的可复用审核结果。
// FindReusableReview 读取同一内容、同一评估语义下最新的可复用审核结果。
// createdAfter 非空时只返回该时间之后产生的结论，供重新审核使用。
func (r *Repository) FindReusableReview(ctx context.Context, hash, evalHash string, createdAfter *time.Time) (*PromptReview, error) {
	query := `SELECT ` + reviewColumns + ` FROM sub2api_enhance.prompt_reviews
 WHERE hash=$1 AND eval_hash=$2 AND reusable`
	args := []any{hash, evalHash}
	if createdAfter != nil {
		args = append(args, *createdAfter)
		query += ` AND created_at>=$3`
	}
	review, err := scanReview(r.db.QueryRowContext(ctx, query+` ORDER BY created_at DESC,id DESC LIMIT 1`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return review, err
}

// GetReviews 按 ID 读取审核结果，供任务详情展示目标结论。
func (r *Repository) GetReviews(ctx context.Context, ids []int64) (map[int64]*PromptReview, error) {
	result := make(map[int64]*PromptReview)
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+reviewColumns+` FROM sub2api_enhance.prompt_reviews WHERE id=ANY($1)`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		review, err := scanReview(rows)
		if err != nil {
			return nil, err
		}
		result[review.ID] = review
	}
	return result, rows.Err()
}

// SaveReview 以当前租约追加一条全库复用的审核结果；强制重审也只追加新版本。
func (r *Repository) SaveReview(ctx context.Context, job *AuditJob, review *PromptReview) error {
	nodes, err := json.Marshal(review.Nodes)
	if err != nil {
		return err
	}
	err = r.db.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.prompt_reviews(hash,eval_hash,run_kind,decision,score,reason,node_results,reusable,requested_by,origin_job_id)
 SELECT $3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$1 WHERE `+leaseValidSQL+` RETURNING id,created_at`,
		job.ID, job.ClaimGeneration, review.Hash, review.EvalHash, review.RunKind, review.Decision, review.Score, encodeStoredText(review.Reason), string(nodes),
		review.Reusable, review.RequestedBy).Scan(&review.ID, &review.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	if err == nil {
		origin := job.ID
		review.OriginJobID = &origin
	}
	return err
}

// ResolveTarget 记录目标结论；成功目标即使任务最终失败也不会被撤销。
// ResolveTarget 记录目标结论与类别；成功目标即使任务最终失败也不会被撤销。
func (r *Repository) ResolveTarget(ctx context.Context, job *AuditJob, target *JobTarget) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.audit_job_targets SET resolution=$5,review_id=$6,decision=$7,categories=$8,error_code=NULL,error_message=NULL,resolved_at=clock_timestamp()
 WHERE job_id=$1 AND round=$3 AND target_order=$4 AND `+leaseValidSQL,
		job.ID, job.ClaimGeneration, target.Round, target.Order, target.Resolution, target.ReviewID, target.Decision, pq.Array(target.Categories))
	return checkLeaseUpdate(result, err)
}

// FailTarget 记录未完成目标的最近一次错误，便于详情展示和只重试失败目标。
func (r *Repository) FailTarget(ctx context.Context, job *AuditJob, target *JobTarget, failure *AuditError) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.audit_job_targets SET error_code=$5,error_message=$6
 WHERE job_id=$1 AND round=$3 AND target_order=$4 AND resolution IS NULL AND `+leaseValidSQL,
		job.ID, job.ClaimGeneration, target.Round, target.Order, failure.Code, encodeStoredText(failure.Message))
	return checkLeaseUpdate(result, err)
}

// ListRounds 返回任务的全部成功审核轮次。
func (r *Repository) ListRounds(ctx context.Context, jobID int64) ([]JobRound, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT round,run_kind,requested_by,reuse_mode,decision,COALESCE(trigger_hash,''),COALESCE(trigger_kind,''),COALESCE(trigger_categories,'{}'),config_revision,started_at,finished_at
 FROM sub2api_enhance.audit_job_rounds WHERE job_id=$1 ORDER BY round`, jobID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]JobRound, 0)
	for rows.Next() {
		var item JobRound
		var decision string
		if err := rows.Scan(&item.Round, &item.RunKind, &item.RequestedBy, &item.ReuseMode, &decision, &item.TriggerHash, &item.TriggerKind, pq.Array(&item.TriggerCategories), &item.ConfigRevision, &item.StartedAt, &item.FinishedAt); err != nil {
			return nil, err
		}
		item.Decision = Decision(decision)
		items = append(items, item)
	}
	return items, rows.Err()
}

// PrepareModelAttempt 在真实调用发出前持久化调用流水；正式任务必须仍持有租约。
func (r *Repository) PrepareModelAttempt(ctx context.Context, job *AuditJob, attempt *ModelAttempt) error {
	model, err := json.Marshal(attempt.ModelSnapshot)
	if err != nil {
		return err
	}
	var jobID, generation any = nil, int64(0)
	var round, evaluation, targetHash any
	if job != nil {
		jobID, generation = job.ID, job.ClaimGeneration
		round, evaluation, targetHash = job.CurrentRound, max(job.Attempts, 1), attempt.TargetHash
	}
	err = r.db.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.model_attempts
 (job_id,audit_round,target_hash,call_kind,evaluation_round,model_id,model_snapshot,stage,repair_of_attempt_id,request_metadata)
 SELECT $1,$3,$4,$5,$6,$7,$8,$9,$10,$11
 WHERE $1::bigint IS NULL OR `+leaseValidSQL+` RETURNING id,created_at`,
		jobID, generation, round, targetHash, attempt.CallKind, evaluation, attempt.ModelID, string(model), attempt.Stage, attempt.RepairOfAttemptID, string(attempt.RequestMetadata),
	).Scan(&attempt.ID, &attempt.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	return err
}

// StartModelAttempt 在发出 HTTP 前再次确认租约、全局开关和用户排除状态。
func (r *Repository) StartModelAttempt(ctx context.Context, job *AuditJob, attempt *ModelAttempt) error {
	var generation int64
	allowExcluded := true
	userID := int64(0)
	if job != nil {
		generation = job.ClaimGeneration
		userID = job.UserID
		allowExcluded = job.CurrentRequestedBy != nil || job.Dispatched
	}
	err := r.db.QueryRowContext(ctx, `UPDATE sub2api_enhance.model_attempts a SET status='started',dispatch_started_at=clock_timestamp(),updated_at=clock_timestamp()
 WHERE a.id=$1 AND a.status='prepared' AND (a.job_id IS NULL OR EXISTS
 (SELECT 1 FROM sub2api_enhance.audit_jobs j WHERE j.id=a.job_id AND j.claim_generation=$2 AND j.status='processing' AND j.lease_until>clock_timestamp()))
 AND (a.call_kind='probe' OR ((SELECT value::json->>'mode' FROM sub2api_enhance.settings WHERE key='third_party_prompt_audit_config') IN ('async','blocking')
 AND ($3 OR NOT COALESCE((SELECT (value::jsonb->'excluded_user_ids') @> to_jsonb(ARRAY[$4::bigint]) FROM sub2api_enhance.settings WHERE key='third_party_prompt_audit_config'),false))))
 RETURNING dispatch_started_at`, attempt.ID, generation, allowExcluded, userID).Scan(&attempt.DispatchStartedAt)
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if job == nil {
		return ErrAuditPaused
	}
	var valid bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sub2api_enhance.audit_jobs WHERE id=$1 AND claim_generation=$2 AND status='processing' AND lease_until>clock_timestamp())`, job.ID, job.ClaimGeneration).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrLeaseLost
	}
	if !allowExcluded {
		var excluded bool
		if err := r.db.QueryRowContext(ctx, `SELECT COALESCE((value::jsonb->'excluded_user_ids') @> to_jsonb(ARRAY[$1::bigint]),false) FROM sub2api_enhance.settings WHERE key=$2`, userID, SettingKey).Scan(&excluded); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if excluded {
			return ErrUserExcluded
		}
	}
	return ErrAuditPaused
}

// FinishModelAttempt 写入调用终态；收尾脱离请求取消，但不能借此再次调用模型。
func (r *Repository) FinishModelAttempt(ctx context.Context, job *AuditJob, attempt *ModelAttempt) error {
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
	result, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.model_attempts a SET status=$2,http_status=$3,raw_response=$4,confidence=$5,reason=$6,
 input_tokens=$7,output_tokens=$8,latency_ms=$9,error_code=$10,error_message=$11,finished_at=clock_timestamp(),updated_at=clock_timestamp()
 WHERE a.id=$1 AND a.status IN ('prepared','started') AND (a.job_id IS NULL OR EXISTS
 (SELECT 1 FROM sub2api_enhance.audit_jobs j WHERE j.id=a.job_id AND j.claim_generation=$12 AND j.status='processing' AND j.lease_until>clock_timestamp()))`,
		attempt.ID, attempt.Status, attempt.HTTPStatus, raw, confidence, reason, attempt.InputTokens, attempt.OutputTokens,
		attempt.LatencyMS, attempt.ErrorCode, encodeStoredText(attempt.ErrorMessage), generation)
	return checkLeaseUpdate(result, err)
}

// ListModelAttempts 读取模型调用流水；where 只由本包常量拼接。
func (r *Repository) ListModelAttempts(ctx context.Context, where string, args ...any) ([]ModelAttempt, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,job_id,call_kind,evaluation_round,COALESCE(audit_round,0),COALESCE(target_hash,''),model_id,model_snapshot,stage,repair_of_attempt_id,
 request_metadata,status,http_status,raw_response,confidence,reason,input_tokens,output_tokens,latency_ms,error_code,error_message,created_at,dispatch_started_at,finished_at
 FROM sub2api_enhance.model_attempts WHERE `+where+` ORDER BY id`, args...)
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
		if err := rows.Scan(&item.ID, &item.JobID, &item.CallKind, &item.EvaluationRound, &item.AuditRound, &item.TargetHash, &item.ModelID, &model, &item.Stage, &item.RepairOfAttemptID,
			&metadata, &item.Status, &item.HTTPStatus, &item.RawResponse, &confidence, &reason, &item.InputTokens, &item.OutputTokens, &item.LatencyMS, &code, &message,
			&item.CreatedAt, &item.DispatchStartedAt, &item.FinishedAt); err != nil {
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
			text := decodeStoredText(*item.RawResponse)
			item.RawResponse = &text
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
