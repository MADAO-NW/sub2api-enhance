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

type DecisionConfig struct {
	Revision        int64    `json:"revision"`
	ReviewThreshold *float64 `json:"review_threshold"`
	BlockThreshold  *float64 `json:"block_threshold"`
}

type ModelResultView struct {
	ModelResult
	MaxSegmentConfidence *float64 `json:"max_segment_confidence"`
}

type OutcomeView struct {
	*Outcome
	Models         []ModelResultView `json:"models"`
	DecisionConfig *DecisionConfig   `json:"decision_config,omitempty"`
}

// outcomeView 派生解释字段，不向持久化结果复制片段最大值或当前配置。
func outcomeView(outcome *Outcome, config *DecisionConfig) *OutcomeView {
	if outcome == nil {
		return nil
	}
	if config == nil && outcome.Config.Revision > 0 {
		config = &DecisionConfig{Revision: outcome.Config.Revision, ReviewThreshold: outcome.Config.ReviewThreshold, BlockThreshold: outcome.Config.BlockThreshold}
	}
	view := &OutcomeView{Outcome: outcome, DecisionConfig: config, Models: make([]ModelResultView, 0, len(outcome.Models))}
	for _, model := range outcome.Models {
		item := ModelResultView{ModelResult: model}
		for _, segment := range model.Segments {
			if item.MaxSegmentConfidence == nil || segment.Result.Confidence > *item.MaxSegmentConfidence {
				score := segment.Result.Confidence
				item.MaxSegmentConfidence = &score
			}
		}
		view.Models = append(view.Models, item)
	}
	return view
}

func validateFilter(filter Filter) error {
	if filter.From != nil && filter.To != nil && !filter.From.Before(*filter.To) {
		return errors.New("开始时间必须早于结束时间")
	}
	for _, id := range filter.IDs {
		if id <= 0 {
			return errors.New("筛选 ID 必须大于 0")
		}
	}
	for _, id := range []*int64{filter.UserID, filter.APIKeyID, filter.GroupID} {
		if id != nil && *id <= 0 {
			return errors.New("身份筛选 ID 必须大于 0")
		}
	}
	if filter.Status != "" && !slices.Contains([]string{"queued", "processing", "retry", "done", "failed", "skipped"}, filter.Status) {
		return errors.New("任务状态无效")
	}
	if filter.Decision != "" && filter.Decision != DecisionPass && filter.Decision != DecisionReview && filter.Decision != DecisionBlock {
		return errors.New("分类筛选无效")
	}
	if filter.RunKind != "" && filter.RunKind != "request" && filter.RunKind != "reaudit" {
		return errors.New("任务来源无效")
	}
	if filter.Mode != "" && filter.Mode != "async" && filter.Mode != "blocking" {
		return errors.New("运行模式筛选无效")
	}
	return nil
}

// filterSQL 在列表、预览和提交中保持同一筛选语义，所有外部值只作为 SQL 参数。
func filterSQL(filter Filter, events bool, includeTime bool) (string, []any) {
	clauses := make([]string, 0)
	args := make([]any, 0)
	add := func(expression string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(expression, len(args)))
	}
	idColumn, timeColumn := "j.id", "j.created_at"
	if events {
		idColumn, timeColumn = "e.id", "e.created_at"
	}
	if len(filter.IDs) > 0 {
		add(idColumn+"=ANY($%d)", pq.Array(filter.IDs))
	}
	if includeTime {
		if filter.From != nil {
			add(timeColumn+">=$%d", *filter.From)
		}
		if filter.To != nil {
			add(timeColumn+"<$%d", *filter.To)
		}
	}
	if filter.UserID != nil {
		add("j.user_id=$%d", *filter.UserID)
	}
	if filter.APIKeyID != nil {
		add("j.api_key_id=$%d", *filter.APIKeyID)
	}
	if filter.GroupID != nil {
		add("j.group_id=$%d", *filter.GroupID)
	}
	if filter.Status != "" {
		add("j.status=$%d", filter.Status)
	}
	if filter.RunKind != "" {
		add("j.current_run_kind=$%d", filter.RunKind)
	}
	if filter.Mode != "" {
		add("j.execution_mode=$%d", filter.Mode)
	}
	if filter.Platform != "" {
		add("j.platform=$%d", filter.Platform)
	}
	if filter.RequestID != "" {
		add("j.request_id=$%d", filter.RequestID)
	}
	if filter.Keyword != "" {
		add("(j.identity_snapshot ILIKE '%%'||$%d||'%%')", filter.Keyword)
	}
	if filter.Decision != "" {
		if events {
			add("o.decision=$%d", filter.Decision)
		} else {
			add("EXISTS(SELECT 1 FROM sub2api_enhance.third_party_prompt_audit_outcomes result WHERE result.job_id=j.id AND result.decision=$%d)", filter.Decision)
		}
	}
	if filter.ModelID != "" {
		if events {
			add("EXISTS(SELECT 1 FROM json_array_elements(o.model_results::json) model WHERE model->>'model_id'=$%d)", filter.ModelID)
		} else {
			add("EXISTS(SELECT 1 FROM json_array_elements(j.config_snapshot::json->'models') model WHERE model->>'id'=$%d AND model->>'enabled'='true')", filter.ModelID)
		}
	}
	if len(clauses) == 0 {
		return "TRUE", args
	}
	return strings.Join(clauses, " AND "), args
}

func (r *Repository) ListJobs(ctx context.Context, filter Filter, page, pageSize int) (*Page[Job], error) {
	if err := validateFilter(filter); err != nil {
		return nil, err
	}
	where, args := filterSQL(filter, false, true)
	result := &Page[Job]{Items: []Job{}, Page: page, PageSize: pageSize}
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sub2api_enhance.third_party_prompt_audit_jobs j WHERE `+where, args...).Scan(&result.Total); err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(record) FROM(SELECT ` + jobProjection(false) + jobSource + ` WHERE ` + where + ` ORDER BY j.created_at DESC,j.id DESC`
	if pageSize > 0 {
		args = append(args, pageSize, (page-1)*pageSize)
		query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))
	}
	query += `) record`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		job, err := decodeJob(raw)
		if err != nil {
			return nil, err
		}
		result.Items = append(result.Items, *job)
	}
	return result, rows.Err()
}

func (r *Repository) ListEvents(ctx context.Context, filter Filter, page, pageSize int) (*Page[Event], error) {
	if err := validateFilter(filter); err != nil {
		return nil, err
	}
	where, args := filterSQL(filter, true, true)
	from := ` FROM sub2api_enhance.third_party_prompt_audit_events e JOIN sub2api_enhance.third_party_prompt_audit_jobs j ON j.id=e.job_id` + jobInputJoins + `JOIN sub2api_enhance.third_party_prompt_audit_outcomes o ON o.id=e.latest_outcome_id `
	result := &Page[Event]{Items: []Event{}, Page: page, PageSize: pageSize}
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*)`+from+`WHERE `+where, args...).Scan(&result.Total); err != nil {
		return nil, err
	}
	query := `SELECT e.id,e.job_id,e.original_outcome_id,e.latest_outcome_id,e.created_at,e.updated_at,
 (SELECT row_to_json(record) FROM(SELECT ` + jobProjection(false) + `) record),o.decision,o.partial_failure,o.created_at,
	 CASE WHEN j.current_run_kind='reaudit' AND j.status IN ('queued','processing','retry') THEN j.status ELSE COALESCE((SELECT status FROM sub2api_enhance.third_party_prompt_audit_jobs child WHERE child.source_job_id=e.job_id ORDER BY child.id DESC LIMIT 1),'') END,
 (SELECT COALESCE(json_agg(json_build_object('model_id',m->>'model_id','model_name',m->>'model_name','decision',m->>'decision','basis',m->>'basis','confidence',m->'confidence',
	 'reused',m->'reused','joint_attempt_id',m->'joint_attempt_id','error',m->'error','skipped',m->'skipped','skip_reason',m->'skip_reason',
 'max_segment_confidence',(SELECT MAX((s->'result'->>'confidence')::double precision) FROM json_array_elements(m->'segments') s))),'[]') FROM json_array_elements(o.model_results::json) m),
	 json_build_object('revision',(o.config_snapshot::json->>'revision')::bigint,'review_threshold',o.config_snapshot::json->'review_threshold','block_threshold',o.config_snapshot::json->'block_threshold'),o.audit_round,
	 CASE WHEN o.started_at IS NULL OR o.finished_at IS NULL THEN NULL ELSE GREATEST(0,extract(epoch FROM (o.finished_at-o.started_at))*1000)::bigint END,o.job_id` + from + `WHERE ` + where + ` ORDER BY e.created_at DESC,e.id DESC`
	if pageSize > 0 {
		args = append(args, pageSize, (page-1)*pageSize)
		query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var event Event
		var jobRaw, modelsRaw, configRaw []byte
		latest := &OutcomeView{Outcome: &Outcome{}}
		if err := rows.Scan(&event.ID, &event.JobID, &event.OriginalOutcomeID, &event.LatestOutcomeID, &event.CreatedAt, &event.UpdatedAt, &jobRaw, &latest.Decision, &latest.PartialFailure, &latest.CreatedAt, &event.ReauditStatus, &modelsRaw, &configRaw, &latest.AuditRound, &latest.DurationMS, &latest.JobID); err != nil {
			return nil, err
		}
		job, err := decodeJob(jobRaw)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(modelsRaw, &latest.Models); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(configRaw, &latest.DecisionConfig); err != nil {
			return nil, err
		}
		event.Job, event.Latest = job, latest
		latest.ID = event.LatestOutcomeID
		result.Items = append(result.Items, event)
	}
	return result, rows.Err()
}

type JobDetail struct {
	InputJSON   string                  `json:"input_json"`
	InputParts  []Segment               `json:"input_parts"`
	AuditTarget *auditTarget            `json:"audit_target,omitempty"`
	NonText     []NonTextInput          `json:"non_text"`
	Job         *Job                    `json:"job"`
	Outcome     *OutcomeView            `json:"outcome"`
	Rounds      []*OutcomeView          `json:"rounds"`
	Reaudits    []Job                   `json:"reaudits"`
	Attempts    []ModelAttempt          `json:"attempts"`
	Actions     []Action                `json:"actions"`
	Enforcement *EnforcementExplanation `json:"enforcement,omitempty"`
}

type EnforcementExplanation struct {
	Role                  string `json:"role"`
	UserStatus            string `json:"user_status"`
	WarningReason         string `json:"warning_reason"`
	DisableReason         string `json:"disable_reason"`
	WindowViolations      int    `json:"window_violations"`
	WindowSize            int    `json:"window_size"`
	DisableViolationCount int64  `json:"disable_violation_count"`
}

func (r *Repository) JobDetail(ctx context.Context, id int64) (*JobDetail, error) {
	job, err := r.GetJob(ctx, id, true)
	if err != nil {
		return nil, err
	}
	result := &JobDetail{Job: job, Reaudits: []Job{}, Rounds: []*OutcomeView{}, InputParts: []Segment{}, NonText: []NonTextInput{}}
	if job.FullInput != nil {
		raw, err := json.MarshalIndent(job.FullInput, "", "  ")
		if err != nil {
			return nil, err
		}
		result.InputJSON = string(raw)
		result.NonText = job.FullInput.NonText
		if parts, err := ExtractSegments(job.FullInput, job.Config.AuditScope); err == nil {
			result.InputParts = parts
		}
		if target, err := prepareTarget(job); err == nil {
			result.AuditTarget = &target
		}
	}
	outcome, err := r.GetOutcome(ctx, id)
	if err != nil {
		return nil, err
	}
	result.Outcome = outcomeView(outcome, job.DecisionConfig)
	roundRows, err := r.db.QueryContext(ctx, `SELECT id FROM sub2api_enhance.third_party_prompt_audit_outcomes WHERE job_id=$1 ORDER BY audit_round DESC,id DESC`, job.ID)
	if err != nil {
		return nil, err
	}
	roundIDs := make([]int64, 0)
	for roundRows.Next() {
		var outcomeID int64
		if err := roundRows.Scan(&outcomeID); err != nil {
			_ = roundRows.Close()
			return nil, err
		}
		roundIDs = append(roundIDs, outcomeID)
	}
	if err := roundRows.Err(); err != nil {
		_ = roundRows.Close()
		return nil, err
	}
	if err := roundRows.Close(); err != nil {
		return nil, err
	}
	for _, outcomeID := range roundIDs {
		round, err := r.queryOutcome(ctx, `SELECT id,job_id,user_id,decision,partial_failure,enforcement_eligible,model_results,source_outcome_id,created_at,audit_round,run_kind,requested_by,config_snapshot,started_at,finished_at FROM sub2api_enhance.third_party_prompt_audit_outcomes WHERE id=$1`, outcomeID)
		if err != nil {
			return nil, err
		}
		result.Rounds = append(result.Rounds, outcomeView(round, nil))
	}
	result.Attempts, err = r.ListAttempts(ctx, id)
	if err != nil {
		return nil, err
	}
	rootID := job.ID
	if job.SourceJobID != nil {
		rootID = *job.SourceJobID
	}
	result.Actions, err = r.ListActions(ctx, job.UserID, rootID)
	if err != nil {
		return nil, err
	}
	result.Enforcement, err = r.enforcementExplanation(ctx, result)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT row_to_json(record) FROM(SELECT `+jobProjection(false)+jobSource+`WHERE j.source_job_id=$1 ORDER BY j.id DESC) record`, rootID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		child, err := decodeJob(raw)
		if err != nil {
			return nil, err
		}
		result.Reaudits = append(result.Reaudits, *child)
	}
	return result, rows.Err()
}

// enforcementExplanation 使用当前处置规则和已持久化状态解释最新结论为何触发或未触发账号动作。
func (r *Repository) enforcementExplanation(ctx context.Context, detail *JobDetail) (*EnforcementExplanation, error) {
	view := &EnforcementExplanation{}
	if err := r.db.QueryRowContext(ctx, `SELECT role,status FROM public.users WHERE id=$1 AND deleted_at IS NULL`, detail.Job.UserID).Scan(&view.Role, &view.UserStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return view, nil
		}
		return nil, err
	}
	if view.Role != "user" {
		view.WarningReason, view.DisableReason = "role_excluded", "role_excluded"
		return view, nil
	}
	current := storedConfig{Config: DefaultConfig()}
	var raw string
	if err := r.db.QueryRowContext(ctx, `SELECT value FROM sub2api_enhance.settings WHERE key=$1`, SettingKey).Scan(&raw); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	} else if err == nil {
		current.AuditScope = "full_request"
		if err := json.Unmarshal([]byte(raw), &current); err != nil {
			return nil, err
		}
	}
	var warningArmed bool
	var windowAfter int64
	if err := r.db.QueryRowContext(ctx, `SELECT warning_window_after_outcome_id,warning_armed,disable_violation_count FROM sub2api_enhance.third_party_prompt_audit_enforcement_states WHERE user_id=$1`, detail.Job.UserID).Scan(&windowAfter, &warningArmed, &view.DisableViolationCount); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	eligible := false
	if len(detail.Rounds) > 0 {
		eligible = detail.Rounds[len(detail.Rounds)-1].AuditRound == 1 && detail.Rounds[len(detail.Rounds)-1].EnforcementEligible
		for _, round := range detail.Rounds {
			if round.AuditRound == 1 && round.EnforcementEligible {
				eligible = true
				break
			}
		}
	}
	triggered := func(kind string) bool {
		if detail.Outcome == nil {
			return false
		}
		for _, action := range detail.Actions {
			if action.ActionType == kind && action.OutcomeID != nil && *action.OutcomeID == detail.Outcome.ID {
				return true
			}
		}
		return false
	}
	if !eligible {
		view.WarningReason, view.DisableReason = "not_enforcement_eligible", "not_enforcement_eligible"
	} else if detail.Outcome == nil || detail.Outcome.Decision != DecisionBlock {
		view.WarningReason, view.DisableReason = "decision_not_block", "decision_not_block"
	} else {
		if !current.Warning.Enabled {
			view.WarningReason = "rule_disabled"
		} else if triggered("warning") {
			view.WarningReason = "triggered"
		} else {
			rows, err := r.db.QueryContext(ctx, `SELECT latest.decision FROM sub2api_enhance.third_party_prompt_audit_jobs root JOIN sub2api_enhance.third_party_prompt_audit_outcomes original ON original.job_id=root.id AND original.audit_round=1 LEFT JOIN sub2api_enhance.third_party_prompt_audit_events event ON event.job_id=root.id JOIN sub2api_enhance.third_party_prompt_audit_outcomes latest ON latest.id=COALESCE(event.latest_outcome_id,original.id) WHERE root.run_kind='request' AND root.user_id=$1 AND original.enforcement_eligible AND original.id>$2 ORDER BY root.id DESC LIMIT $3`, detail.Job.UserID, windowAfter, current.Warning.Window)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var decision Decision
				if err = rows.Scan(&decision); err != nil {
					_ = rows.Close()
					return nil, err
				}
				view.WindowSize++
				if decision == DecisionBlock {
					view.WindowViolations++
				}
			}
			if err = rows.Err(); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if err := rows.Close(); err != nil {
				return nil, err
			}
			if view.WindowViolations < current.Warning.Limit {
				view.WarningReason = "window_insufficient"
			} else if !warningArmed {
				view.WarningReason = "already_notified"
			} else {
				view.WarningReason = "no_new_block"
			}
		}
		if !current.Disable.Enabled {
			view.DisableReason = "rule_disabled"
		} else if triggered("disable") {
			view.DisableReason = "triggered"
		} else if view.UserStatus != "active" {
			view.DisableReason = "already_disabled"
		} else if view.DisableViolationCount < current.Disable.Limit {
			view.DisableReason = "cumulative_insufficient"
		} else {
			view.DisableReason = "action_already_pending"
		}
	}
	return view, nil
}

type EventDetail struct {
	Event  Event      `json:"event"`
	Detail *JobDetail `json:"detail"`
}

func (r *Repository) EventDetail(ctx context.Context, id int64) (*EventDetail, error) {
	var event Event
	err := r.db.QueryRowContext(ctx, `SELECT id,job_id,original_outcome_id,latest_outcome_id,created_at,updated_at FROM sub2api_enhance.third_party_prompt_audit_events WHERE id=$1`, id).Scan(&event.ID, &event.JobID, &event.OriginalOutcomeID, &event.LatestOutcomeID, &event.CreatedAt, &event.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	latest, err := r.queryOutcome(ctx, `SELECT id,job_id,user_id,decision,partial_failure,enforcement_eligible,model_results,source_outcome_id,created_at,audit_round,run_kind,requested_by,config_snapshot,started_at,finished_at FROM sub2api_enhance.third_party_prompt_audit_outcomes WHERE id=$1`, event.LatestOutcomeID)
	if err != nil {
		return nil, err
	}
	if event.OriginalOutcomeID != nil {
		original, err := r.queryOutcome(ctx, `SELECT id,job_id,user_id,decision,partial_failure,enforcement_eligible,model_results,source_outcome_id,created_at,audit_round,run_kind,requested_by,config_snapshot,started_at,finished_at FROM sub2api_enhance.third_party_prompt_audit_outcomes WHERE id=$1`, *event.OriginalOutcomeID)
		if err != nil {
			return nil, err
		}
		event.Original = outcomeView(original, nil)
	}
	detail, err := r.JobDetail(ctx, latest.JobID)
	if err != nil {
		return nil, err
	}
	event.Latest = outcomeView(latest, detail.Job.DecisionConfig)
	return &EventDetail{Event: event, Detail: detail}, nil
}

type reauditCandidate struct {
	ID       int64
	Ready    bool
	ActiveID *int64
	Reason   string
}

func (r *Repository) reauditCandidates(ctx context.Context, request ReauditRequest) ([]reauditCandidate, error) {
	if request.Source != "events" && request.Source != "jobs" {
		return nil, errors.New("复核来源无效")
	}
	if err := validateFilter(request.Filter); err != nil {
		return nil, err
	}
	where, args := filterSQL(request.Filter, request.Source == "events", true)
	from := ` FROM sub2api_enhance.third_party_prompt_audit_jobs j `
	if request.Source == "events" {
		from = ` FROM sub2api_enhance.third_party_prompt_audit_events e JOIN sub2api_enhance.third_party_prompt_audit_jobs j ON j.id=e.job_id JOIN sub2api_enhance.third_party_prompt_audit_outcomes o ON o.id=e.latest_outcome_id `
	}
	query := `SELECT root.id,root.snapshot_status='complete' AND root.full_input_snapshot IS NOT NULL AND COALESCE(root.last_error_code,'')<>'no_text',
	 COALESCE(CASE WHEN root.status IN ('queued','processing','retry') THEN root.id END,(SELECT child.id FROM sub2api_enhance.third_party_prompt_audit_jobs child WHERE child.source_job_id=root.id AND child.status IN ('queued','processing','retry') LIMIT 1))
 FROM sub2api_enhance.third_party_prompt_audit_jobs root WHERE root.run_kind='request' AND root.id IN
 (SELECT COALESCE(j.source_job_id,j.id)` + from + `WHERE ` + where + `) ORDER BY root.id`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]reauditCandidate, 0)
	for rows.Next() {
		var item reauditCandidate
		if err := rows.Scan(&item.ID, &item.Ready, &item.ActiveID); err != nil {
			return nil, err
		}
		if !item.Ready {
			item.Reason = "完整可审文本不可用，跳过重新审核"
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
		item := ReauditItem{SourceJobID: candidate.ID, Status: "ready"}
		if !candidate.Ready {
			item.Status, item.Reason = "skipped", candidate.Reason
		} else if candidate.ActiveID != nil {
			item.Status, item.JobID, item.Reason = "already_running", candidate.ActiveID, "该来源已有进行中的复核任务"
		} else {
			result.Ready++
		}
		result.Items = append(result.Items, item)
	}
	return result, nil
}

// CreateReaudits 逐项锁定原任务并开始新的审核轮次，不再创建复核子任务。
func (r *Repository) CreateReaudits(ctx context.Context, request ReauditRequest, snapshot ConfigSnapshot, actorID int64) (*ReauditResult, error) {
	result, err := r.PreviewReaudits(ctx, request)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	for i := range result.Items {
		item := &result.Items[i]
		if item.Status != "ready" {
			continue
		}
		var id int64
		err := r.db.QueryRowContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_jobs root SET
	 audit_round=audit_round+1,current_run_kind='reaudit',current_requested_by=$1,config_revision=$2,config_snapshot=$3,
	 status='queued',attempts=0,max_attempts=$4,claim_generation=claim_generation+1,lease_until=NULL,next_attempt_at=clock_timestamp(),
	 result_checkpoint=NULL,reuse_metrics='{"whole_lookups":0,"whole_hits":0,"segment_lookups":0,"segment_hits":0,"within_job_hits":0,"inflight_hits":0,"short_circuited_nodes":0}',
	 failure_stage=NULL,last_error_code=NULL,last_error_message=NULL,started_at=NULL,finished_at=NULL,input_manifest=NULL,input_hash=NULL,target_hash=NULL,evaluation_hash=NULL,updated_at=clock_timestamp()
	 WHERE root.id=$5 AND root.run_kind='request' AND root.status IN ('done','failed','skipped') AND root.snapshot_status='complete' AND root.full_input_snapshot IS NOT NULL AND COALESCE(root.last_error_code,'')<>'no_text'
	 AND NOT EXISTS(SELECT 1 FROM sub2api_enhance.third_party_prompt_audit_jobs child WHERE child.source_job_id=root.id AND child.status IN ('queued','processing','retry'))
	 RETURNING root.id`, actorID, snapshot.Revision, string(encoded), MaxEvaluationAttempts, item.SourceJobID).Scan(&id)
		if err == nil {
			item.Status, item.JobID = "requeued", &id
			continue
		}
		if errors.Is(err, sql.ErrNoRows) {
			var activeID int64
			lookupErr := r.db.QueryRowContext(ctx, `SELECT id FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE (id=$1 OR source_job_id=$1) AND status IN ('queued','processing','retry') ORDER BY id LIMIT 1`, item.SourceJobID).Scan(&activeID)
			if lookupErr == nil {
				item.Status, item.JobID, item.Reason = "already_running", &activeID, "该来源已有进行中的复核任务"
			} else if errors.Is(lookupErr, sql.ErrNoRows) {
				item.Status, item.Reason = "skipped", "完整原文已不可用"
			} else {
				item.Status, item.Reason = "failed", lookupErr.Error()
			}
		} else {
			item.Status, item.Reason = "failed", err.Error()
		}
	}
	return result, nil
}

func (r *Repository) SaveTarget(ctx context.Context, job *Job) error {
	manifest, err := json.Marshal(job.Manifest)
	if err != nil {
		return err
	}
	result, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_jobs SET input_manifest=$3,input_hash=$4,target_hash=$5,evaluation_hash=$6,updated_at=clock_timestamp() WHERE id=$1 AND claim_generation=$2 AND status='processing' AND lease_until>clock_timestamp()`, job.ID, job.ClaimGeneration, string(manifest), job.InputHash, job.TargetHash, job.EvaluationHash)
	return checkLeaseUpdate(result, err)
}

func (r *Repository) ResumeResult(ctx context.Context, id int64) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_jobs SET status='retry',next_attempt_at=clock_timestamp(),finished_at=NULL,claim_generation=claim_generation+1,updated_at=clock_timestamp()
 WHERE id=$1 AND status='failed' AND failure_stage='result_persist' AND result_checkpoint IS NOT NULL`, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("该任务不具备可恢复的结果检查点，或已在处理中")
	}
	return nil
}
