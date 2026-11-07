package thirdpartypromptaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/lib/pq"
)

// JobDetail 是任务详情：任务本身、当前轮目标、历史轮次与相关处置动作。
type JobDetail struct {
	Job      *AuditJob      `json:"job"`
	Targets  []JobTarget    `json:"targets"`
	Rounds   []JobRound     `json:"rounds"`
	Attempts []ModelAttempt `json:"attempts"`
	Actions  []Action       `json:"actions"`
}

func validateFilter(filter Filter) error {
	if filter.Decision != "" && filter.Decision != DecisionPass && filter.Decision != DecisionReview && filter.Decision != DecisionBlock {
		return errors.New("裁决筛选无效")
	}
	if filter.Status != "" && !slices.Contains([]string{"queued", "processing", "retry", "done", "failed", "skipped"}, filter.Status) {
		return errors.New("状态筛选无效")
	}
	if filter.Mode != "" && filter.Mode != "async" && filter.Mode != "blocking" {
		return errors.New("运行模式筛选无效")
	}
	if filter.RunKind != "" && filter.RunKind != RunKindRequest && filter.RunKind != RunKindReaudit {
		return errors.New("轮次类型筛选无效")
	}
	if filter.From != nil && filter.To != nil && !filter.From.Before(*filter.To) {
		return errors.New("开始时间必须早于结束时间")
	}
	if filter.Category != "" && !validCategoryCode(filter.Category) {
		return errors.New("类别筛选无效，必须形如 R01")
	}
	return nil
}

// filterSQL 只用本包常量拼接列名，值全部走参数。
func filterSQL(filter Filter, includeTime bool) (string, []any) {
	clauses := []string{"1=1"}
	args := []any{}
	appendValue := func(column string, value any) {
		args = append(args, value)
		clauses = append(clauses, column+"=$"+itoa(len(args)))
	}
	if len(filter.IDs) > 0 {
		appendValue("j.id", pq.Array(filter.IDs))
		clauses[len(clauses)-1] = "j.id=ANY($" + itoa(len(args)) + ")"
	}
	if filter.UserID != nil {
		appendValue("j.user_id", *filter.UserID)
	}
	if filter.APIKeyID != nil {
		appendValue("j.api_key_id", *filter.APIKeyID)
	}
	if filter.GroupID != nil {
		appendValue("j.group_id", *filter.GroupID)
	}
	if filter.Status != "" {
		appendValue("j.status", filter.Status)
	}
	if filter.Decision != "" {
		appendValue("j.decision", string(filter.Decision))
	}
	if filter.RunKind != "" {
		appendValue("j.current_run_kind", filter.RunKind)
	}
	if filter.Mode != "" {
		appendValue("j.mode", filter.Mode)
	}
	if filter.Platform != "" {
		appendValue("j.platform", filter.Platform)
	}
	if filter.RequestID != "" {
		appendValue("j.request_id", filter.RequestID)
	}
	if filter.Model != "" {
		appendValue("j.model", filter.Model)
	}
	if filter.Category != "" {
		args = append(args, filter.Category)
		clauses = append(clauses, "j.trigger_categories && ARRAY[$"+itoa(len(args))+"]::text[]")
	}
	if includeTime {
		if filter.From != nil {
			appendValue("j.created_at", *filter.From)
			clauses[len(clauses)-1] = "j.created_at>=$" + itoa(len(args))
		}
		if filter.To != nil {
			appendValue("j.created_at", *filter.To)
			clauses[len(clauses)-1] = "j.created_at<$" + itoa(len(args))
		}
	}
	return strings.Join(clauses, " AND "), args
}

// ListJobs 返回按创建时间倒序的审核任务列表。
func (r *Repository) ListJobs(ctx context.Context, filter Filter, page, pageSize int) (*Page[AuditJob], error) {
	if err := validateFilter(filter); err != nil {
		return nil, err
	}
	result := &Page[AuditJob]{Items: []AuditJob{}, Page: page, PageSize: pageSize}
	where, args := filterSQL(filter, true)
	countQuery := `SELECT count(*)` + auditJobSource + `WHERE ` + where
	if err := r.db.QueryRowContext(ctx, countQuery, args...).Scan(&result.Total); err != nil {
		return nil, err
	}
	pageArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	rows, err := r.db.QueryContext(ctx, `SELECT row_to_json(record) FROM (SELECT `+auditJobColumns+auditJobSource+`WHERE `+where+
		` ORDER BY j.created_at DESC,j.id DESC LIMIT $`+itoa(len(args)+1)+` OFFSET $`+itoa(len(args)+2)+`) record`, pageArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		job, err := decodeAuditJob(raw)
		if err != nil {
			return nil, err
		}
		result.Items = append(result.Items, *job)
	}
	return result, rows.Err()
}

// JobDetail 读取任务详情；复用结论只展示结论本身，不暴露来源任务。
func (r *Repository) JobDetail(ctx context.Context, id int64) (*JobDetail, error) {
	job, err := r.GetAuditJob(ctx, id)
	if err != nil {
		return nil, err
	}
	targets, err := r.LoadRoundTargets(ctx, job.ID, job.CurrentRound)
	if err != nil {
		return nil, err
	}
	reviewIDs := make([]int64, 0, len(targets))
	for _, target := range targets {
		if target.ReviewID != nil {
			reviewIDs = append(reviewIDs, *target.ReviewID)
		}
	}
	reviews, err := r.GetReviews(ctx, reviewIDs)
	if err != nil {
		return nil, err
	}
	for i := range targets {
		target := &targets[i]
		if target.ReviewID == nil {
			continue
		}
		review := reviews[*target.ReviewID]
		if review == nil {
			continue
		}
		target.Review = &ReviewView{Decision: review.Decision, Score: review.Score, Reason: review.Reason, Nodes: review.Nodes, RunKind: review.RunKind}
	}
	rounds, err := r.ListRounds(ctx, id)
	if err != nil {
		return nil, err
	}
	attempts, err := r.ListModelAttempts(ctx, "job_id=$1", id)
	if err != nil {
		return nil, err
	}
	actions, err := r.ListActions(ctx, job.UserID, id)
	if err != nil {
		return nil, err
	}
	return &JobDetail{Job: job, Targets: targets, Rounds: rounds, Attempts: attempts, Actions: actions}, nil
}

// JobPrompt 是任务列表弹窗展示的单个审核目标正文。
type JobPrompt struct {
	Order      int      `json:"order"`
	Kind       string   `json:"kind"`
	Hash       string   `json:"hash"`
	Text       string   `json:"text"`
	Bytes      int      `json:"bytes"`
	Resolution string   `json:"resolution,omitempty"`
	Decision   string   `json:"decision,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	Score      *float64 `json:"score"`
	Trusted    bool     `json:"trusted"`
	Sampled    bool     `json:"sampled"`
}

// JobPrompts 只读取当前轮的审核目标正文，不加载调用流水与处置动作。
func (r *Repository) JobPrompts(ctx context.Context, id int64) ([]JobPrompt, error) {
	job, err := r.GetAuditJob(ctx, id)
	if err != nil {
		return nil, err
	}
	targets, err := r.LoadRoundTargets(ctx, job.ID, job.CurrentRound)
	if err != nil {
		return nil, err
	}
	reviewIDs := make([]int64, 0, len(targets))
	for _, target := range targets {
		if target.ReviewID != nil {
			reviewIDs = append(reviewIDs, *target.ReviewID)
		}
	}
	reviews, err := r.GetReviews(ctx, reviewIDs)
	if err != nil {
		return nil, err
	}
	hashes := make([]string, 0, len(targets))
	for _, target := range targets {
		hashes = append(hashes, target.Hash)
	}
	sampled := make(map[string]bool, len(hashes))
	if len(hashes) > 0 {
		sampleRows, err := r.db.QueryContext(ctx, `SELECT hash FROM sub2api_enhance.policy_samples WHERE hash=ANY($1)`, pq.Array(hashes))
		if err != nil {
			return nil, err
		}
		for sampleRows.Next() {
			var hash string
			if err := sampleRows.Scan(&hash); err != nil {
				_ = sampleRows.Close()
				return nil, err
			}
			sampled[hash] = true
		}
		if err := sampleRows.Err(); err != nil {
			_ = sampleRows.Close()
			return nil, err
		}
		if err := sampleRows.Close(); err != nil {
			return nil, err
		}
	}
	items := make([]JobPrompt, 0, len(targets))
	for _, target := range targets {
		item := JobPrompt{Order: target.Order, Kind: target.Kind, Hash: target.Hash, Text: target.Text, Bytes: target.Bytes,
			Resolution: target.Resolution, Decision: string(target.Decision), Trusted: target.Trusted, Sampled: sampled[target.Hash]}
		if target.ReviewID != nil {
			if review := reviews[*target.ReviewID]; review != nil {
				item.Reason, item.Score = review.Reason, review.Score
			}
		}
		items = append(items, item)
	}
	return items, nil
}

// reauditBatchLimit 限制单次批量重审的任务数。
const reauditBatchLimit = 200

type reauditCandidate struct {
	ID     int64
	UserID int64
	Ready  bool
	Reason string
}

func (r *Repository) reauditCandidates(ctx context.Context, request ReauditRequest) ([]reauditCandidate, error) {
	if err := validateFilter(request.Filter); err != nil {
		return nil, err
	}
	where, args := filterSQL(request.Filter, true)
	rows, err := r.db.QueryContext(ctx, `SELECT j.id,j.user_id,j.status IN ('done','failed','skipped')`+auditJobSource+`WHERE `+where+` ORDER BY j.id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]reauditCandidate, 0)
	for rows.Next() {
		var item reauditCandidate
		if err := rows.Scan(&item.ID, &item.UserID, &item.Ready); err != nil {
			return nil, err
		}
		if !item.Ready {
			item.Reason = "该任务仍在处理中"
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *Repository) PreviewReaudits(ctx context.Context, request ReauditRequest) (*ReauditResult, error) {
	candidates, err := r.reauditCandidates(ctx, request)
	if err != nil {
		return nil, err
	}
	return r.previewWithCandidates(ctx, candidates)
}

// previewWithCandidates 计算预览结果与预计真实调用数。
func (r *Repository) previewWithCandidates(ctx context.Context, candidates []reauditCandidate) (*ReauditResult, error) {
	result := &ReauditResult{Matched: int64(len(candidates)), Items: []ReauditItem{}}
	readyIDs := make([]int64, 0, len(candidates))
	for _, candidate := range candidates {
		item := ReauditItem{JobID: candidate.ID, Status: "ready", userID: candidate.UserID}
		if !candidate.Ready {
			item.Status, item.Reason = "skipped", candidate.Reason
		} else {
			result.Ready++
			readyIDs = append(readyIDs, candidate.ID)
		}
		result.Items = append(result.Items, item)
	}
	if len(readyIDs) > 0 {
		// 估算真实调用量：按目标内容去重，并排除已信任的指令块。
		if err := r.db.QueryRowContext(ctx, `SELECT count(DISTINCT t.hash) FROM sub2api_enhance.audit_job_targets t
 JOIN sub2api_enhance.audit_jobs j ON j.id=t.job_id AND t.round=j.current_round
 WHERE t.job_id=ANY($1) AND NOT EXISTS (SELECT 1 FROM sub2api_enhance.prompt_trust pt WHERE pt.hash=t.hash)`, pq.Array(readyIDs)).Scan(&result.EstimatedTargets); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// CreateReaudits 在同一任务上追加新一轮：复制当前轮目标，重新审核本身不触发新的处置。
func (r *Repository) CreateReaudits(ctx context.Context, request ReauditRequest, snapshot ConfigSnapshot, actorID int64) (*ReauditResult, error) {
	result, err := r.PreviewReaudits(ctx, request)
	if err != nil {
		return nil, err
	}
	if result.Ready == 0 {
		return result, nil
	}
	// 整批共用一个发起时间：逐个追加轮次期间，先被 Worker 审完的任务产生的结论晚于该时间，
	// 同批其余任务都能复用，保证批量重审同一目标只真实调用一次。
	var requestedAt time.Time
	if err := r.db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&requestedAt); err != nil {
		return nil, err
	}
	requeued := 0
	for i := range result.Items {
		item := &result.Items[i]
		if item.Status != "ready" {
			continue
		}
		// 单次批量重审设上限，避免一次请求灌满队列；其余条目明确标记跳过。
		if requeued >= reauditBatchLimit {
			item.Status, item.Reason = "skipped", "超过单次批量上限，请分批提交"
			continue
		}
		requeued++
		effective := snapshot
		effective.Config = effectiveConfigForUser(snapshot.Config, item.userID)
		encoded, err := json.Marshal(effective)
		if err != nil {
			item.Status, item.Reason = "failed", err.Error()
			continue
		}
		err = r.appendRound(ctx, item.JobID, actorID, snapshot.Revision, string(encoded), requestedAt)
		switch {
		case err == nil:
			item.Status = "requeued"
		case errors.Is(err, ErrNotFound):
			item.Status, item.Reason = "skipped", "该任务已不可重新审核"
		case errors.Is(err, ErrLeaseLost):
			item.Status, item.Reason = "already_running", "该任务已在处理中"
		default:
			item.Status, item.Reason = "failed", err.Error()
		}
	}
	return result, nil
}

// appendRound 追加一轮目标并让任务重新排队；任务状态不是已结束时报 ErrLeaseLost。
// 重新审核统一使用 fresh 语义，reuse_after 记录本批重审的发起时间。
func (r *Repository) appendRound(ctx context.Context, jobID, actorID int64, revision int64, configSnapshot string, reuseAfter time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	var round int
	err = tx.QueryRowContext(ctx, `SELECT status,current_round FROM sub2api_enhance.audit_jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&status, &round)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !slices.Contains([]string{"done", "failed", "skipped"}, status) {
		return ErrLeaseLost
	}
	next := round + 1
	if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.audit_job_targets(job_id,round,target_order,hash,kind)
 SELECT job_id,$3,target_order,hash,kind FROM sub2api_enhance.audit_job_targets WHERE job_id=$1 AND round=$2`,
		jobID, round, next); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.audit_jobs SET current_round=$2,current_run_kind='reaudit',current_requested_by=$3,
 reuse_mode='fresh',reuse_after=$7,config_revision=$4,config_snapshot=$5,status='queued',attempts=0,max_attempts=$6,next_attempt_at=clock_timestamp(),
 lease_until=NULL,failure_stage=NULL,last_error_code=NULL,last_error_message=NULL,started_at=NULL,finished_at=NULL,updated_at=clock_timestamp()
 WHERE id=$1`, jobID, next, actorID, revision, configSnapshot, MaxEvaluationAttempts, reuseAfter); err != nil {
		return err
	}
	return tx.Commit()
}

// ExportRow 是任务导出的一行：任务字段 + 当前轮目标正文与结论。
type ExportRow struct {
	JobID          int64
	CreatedAt      time.Time
	UserID         int64
	APIKeyID       int64
	Platform       string
	Protocol       string
	Mode           string
	Status         string
	Decision       string
	Trigger        string
	RequestID      string
	Order          *int
	Kind           string
	Resolution     string
	TargetDecision string
	Score          *float64
	Reason         string
	Bytes          int
	Text           string
}

// ExportJobs 按过滤条件导出任务及其当前轮目标，供合规取证。
func (r *Repository) ExportJobs(ctx context.Context, filter Filter, limit, offset int) ([]ExportRow, error) {
	if err := validateFilter(filter); err != nil {
		return nil, err
	}
	where, args := filterSQL(filter, true)
	pageArg := len(args) + 1
	query := `SELECT j.id,j.created_at,j.user_id,j.api_key_id,j.platform,j.protocol,j.mode,j.status,COALESCE(j.decision,''),COALESCE(j.trigger_kind,''),j.request_id,
 t.target_order,t.kind,COALESCE(t.resolution,''),COALESCE(t.decision,''),r.score,COALESCE(r.reason,''),COALESCE(p.bytes,0),COALESCE(p.text,'')
 FROM sub2api_enhance.audit_jobs j
 LEFT JOIN sub2api_enhance.audit_job_targets t ON t.job_id=j.id AND t.round=j.current_round
 LEFT JOIN sub2api_enhance.prompt_texts p ON p.hash=t.hash
 LEFT JOIN sub2api_enhance.prompt_reviews r ON r.id=t.review_id
 WHERE ` + where + ` ORDER BY j.id DESC,t.target_order LIMIT $` + itoa(pageArg) + ` OFFSET $` + itoa(pageArg+1)
	args = append(args, limit, offset)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]ExportRow, 0)
	for rows.Next() {
		var item ExportRow
		if err := rows.Scan(&item.JobID, &item.CreatedAt, &item.UserID, &item.APIKeyID, &item.Platform, &item.Protocol, &item.Mode,
			&item.Status, &item.Decision, &item.Trigger, &item.RequestID, &item.Order, &item.Kind, &item.Resolution,
			&item.TargetDecision, &item.Score, &item.Reason, &item.Bytes, &item.Text); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// DeleteUserData 删除某用户的审计任务与不再被引用的文本/结果，保留处置、信任与熔断记录。
func (r *Repository) DeleteUserData(ctx context.Context, userID int64) (int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	deleted, err := tx.ExecContext(ctx, `DELETE FROM sub2api_enhance.audit_jobs WHERE user_id=$1`, userID)
	if err != nil {
		return 0, err
	}
	count, _ := deleted.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM sub2api_enhance.prompt_reviews r
 WHERE NOT EXISTS (SELECT 1 FROM sub2api_enhance.audit_job_targets t WHERE t.review_id=r.id)
   AND NOT EXISTS (SELECT 1 FROM sub2api_enhance.audit_jobs j JOIN sub2api_enhance.audit_job_targets t ON t.job_id=j.id WHERE t.review_id=r.id)`); err != nil {
		return count, err
	}
	// 样本会引用文本，按用户删除时先删该用户的样本，再清理不再被引用的文本。
	if err := r.DeleteSamplesForUser(ctx, tx, userID); err != nil {
		return count, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sub2api_enhance.prompt_texts p
 WHERE NOT EXISTS (SELECT 1 FROM sub2api_enhance.audit_job_targets t WHERE t.hash=p.hash)
   AND NOT EXISTS (SELECT 1 FROM sub2api_enhance.prompt_reviews r WHERE r.hash=p.hash)
   AND NOT EXISTS (SELECT 1 FROM sub2api_enhance.prompt_trust pt WHERE pt.hash=p.hash)
   AND NOT EXISTS (SELECT 1 FROM sub2api_enhance.instruction_breakers b WHERE b.hash=p.hash)
   AND NOT EXISTS (SELECT 1 FROM sub2api_enhance.policy_samples s WHERE s.hash=p.hash)`); err != nil {
		return count, err
	}
	if err := tx.Commit(); err != nil {
		return count, err
	}
	return count, nil
}

// jobFilterWhere 供统计与列表共用同一套过滤语义。
func jobFilterWhere(filter Filter) (string, []any) { return filterSQL(filter, true) }
