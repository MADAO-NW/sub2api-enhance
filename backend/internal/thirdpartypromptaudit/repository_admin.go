package thirdpartypromptaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lib/pq"
)

// JobDetail 是任务详情：任务本身、当前轮目标、历史轮次与相关处置动作。
type JobDetail struct {
	Job     *AuditJob   `json:"job"`
	Targets []JobTarget `json:"targets"`
	Rounds  []JobRound  `json:"rounds"`
	Actions []Action    `json:"actions"`
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
	actions, err := r.ListActions(ctx, job.UserID, id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return &JobDetail{Job: job, Targets: targets, Rounds: rounds, Actions: actions}, nil
}

type reauditCandidate struct {
	ID     int64
	UserID int64
	Ready  bool
	Reason string
}

func (r *Repository) reauditCandidates(ctx context.Context, request ReauditRequest) ([]reauditCandidate, error) {
	if request.ReuseMode != "" && request.ReuseMode != ReuseModeAllow && request.ReuseMode != ReuseModeForce {
		return nil, errors.New("复核复用方式无效")
	}
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
	result := &ReauditResult{Matched: int64(len(candidates)), Items: []ReauditItem{}}
	for _, candidate := range candidates {
		item := ReauditItem{JobID: candidate.ID, Status: "ready", userID: candidate.UserID}
		if !candidate.Ready {
			item.Status, item.Reason = "skipped", candidate.Reason
		} else {
			result.Ready++
		}
		result.Items = append(result.Items, item)
	}
	return result, nil
}

// CreateReaudits 在同一任务上追加新一轮：复制当前轮目标，重新审核本身不触发新的处置。
func (r *Repository) CreateReaudits(ctx context.Context, request ReauditRequest, snapshot ConfigSnapshot, actorID int64) (*ReauditResult, error) {
	if request.ReuseMode == "" {
		request.ReuseMode = ReuseModeAllow
	}
	result, err := r.PreviewReaudits(ctx, request)
	if err != nil {
		return nil, err
	}
	for i := range result.Items {
		item := &result.Items[i]
		if item.Status != "ready" {
			continue
		}
		effective := snapshot
		effective.Config = effectiveConfigForUser(snapshot.Config, item.userID)
		encoded, err := json.Marshal(effective)
		if err != nil {
			item.Status, item.Reason = "failed", err.Error()
			continue
		}
		err = r.appendRound(ctx, item.JobID, actorID, request.ReuseMode, snapshot.Revision, string(encoded))
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
func (r *Repository) appendRound(ctx context.Context, jobID, actorID int64, reuseMode string, revision int64, configSnapshot string) error {
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
 reuse_mode=$4,config_revision=$5,config_snapshot=$6,status='queued',attempts=0,max_attempts=$7,next_attempt_at=clock_timestamp(),
 lease_until=NULL,failure_stage=NULL,last_error_code=NULL,last_error_message=NULL,started_at=NULL,finished_at=NULL,updated_at=clock_timestamp()
 WHERE id=$1`, jobID, next, actorID, reuseMode, revision, configSnapshot, MaxEvaluationAttempts); err != nil {
		return err
	}
	return tx.Commit()
}

// jobFilterWhere 供统计与列表共用同一套过滤语义。
func jobFilterWhere(filter Filter) (string, []any) { return filterSQL(filter, true) }

var _ = fmt.Sprintf
