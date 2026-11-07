package thirdpartypromptaudit

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

// ActionFilter 是处置动作列表的筛选条件，对应 enforcement_actions 的真实列。
type ActionFilter struct {
	// ExecutionStatuses 可同时选多个执行状态，工作台“账号动作待核对”需要 failed 与 unknown 一起列出。
	ExecutionStatuses  []string   `json:"execution_status,omitempty"`
	NotificationStatus string     `json:"notification_status,omitempty"`
	ActionType         string     `json:"action_type,omitempty"`
	UserID             *int64     `json:"user_id,omitempty"`
	From               *time.Time `json:"from,omitempty"`
	To                 *time.Time `json:"to,omitempty"`
}

// actionRecordColumns 在动作字段后追加用户名，用户名来自动作的业务快照。
const actionRecordColumns = actionColumns + `,COALESCE(business_snapshot::jsonb->'user'->>'username','')`

// ActionRecord 是列表展示用的一条处置动作。
type ActionRecord struct {
	Action
	Username string `json:"username"`
}

func validateActionFilter(filter ActionFilter) error {
	for _, status := range filter.ExecutionStatuses {
		if !slices.Contains([]string{"pending", "processing", "succeeded", "failed", "unknown", "cancelled"}, status) {
			return fmt.Errorf("执行状态筛选无效")
		}
	}
	if filter.NotificationStatus != "" && !slices.Contains([]string{"pending", "processing", "retry", "sent", "failed", "not_required"}, filter.NotificationStatus) {
		return fmt.Errorf("通知状态筛选无效")
	}
	if filter.ActionType != "" && !slices.Contains([]string{"warning", "disable", "counter_reset", "blocking_notice", "breaker_notice", "node_auth_notice", "audit_failure_notice", "escalation", "escalation_clear"}, filter.ActionType) {
		return fmt.Errorf("动作类型筛选无效")
	}
	if filter.From != nil && filter.To != nil && !filter.From.Before(*filter.To) {
		return fmt.Errorf("开始时间必须早于结束时间")
	}
	return nil
}

func actionFilterSQL(filter ActionFilter) (string, []any) {
	clauses := []string{"1=1"}
	args := []any{}
	appendValue := func(column string, value any) {
		args = append(args, value)
		clauses = append(clauses, column+"=$"+itoa(len(args)))
	}
	if len(filter.ExecutionStatuses) > 0 {
		args = append(args, pq.Array(filter.ExecutionStatuses))
		clauses = append(clauses, "execution_status=ANY($"+itoa(len(args))+")")
	}
	if filter.NotificationStatus != "" {
		appendValue("notification_status", filter.NotificationStatus)
	}
	if filter.ActionType != "" {
		appendValue("action_type", filter.ActionType)
	}
	if filter.UserID != nil {
		appendValue("user_id", *filter.UserID)
	}
	if filter.From != nil {
		appendValue("requested_at", *filter.From)
		clauses[len(clauses)-1] = "requested_at>=$" + itoa(len(args))
	}
	if filter.To != nil {
		appendValue("requested_at", *filter.To)
		clauses[len(clauses)-1] = "requested_at<$" + itoa(len(args))
	}
	return strings.Join(clauses, " AND "), args
}

// ListActionRecords 返回按请求时间倒序的处置动作分页列表。
func (r *Repository) ListActionRecords(ctx context.Context, filter ActionFilter, page, pageSize int) (*Page[ActionRecord], error) {
	if err := validateActionFilter(filter); err != nil {
		return nil, err
	}
	where, args := actionFilterSQL(filter)
	result := &Page[ActionRecord]{Items: []ActionRecord{}, Page: page, PageSize: pageSize}
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions WHERE `+where, args...).Scan(&result.Total); err != nil {
		return nil, err
	}
	queryArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	rows, err := r.db.QueryContext(ctx, `SELECT `+actionRecordColumns+` FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions WHERE `+where+
		` ORDER BY requested_at DESC,id DESC LIMIT $`+itoa(len(args)+1)+` OFFSET $`+itoa(len(args)+2), queryArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var username string
		action, err := scanAction(rows, &username)
		if err != nil {
			return nil, err
		}
		result.Items = append(result.Items, ActionRecord{Action: *action, Username: username})
	}
	return result, rows.Err()
}

// InboxEntry 是待处理工作台中的一条事项；事项是否已处理完全由源数据状态决定。
type InboxEntry struct {
	Category   string `json:"category"`
	ID         int64  `json:"id,omitempty"`
	ActionType string `json:"action_type,omitempty"`
	UserID     int64  `json:"user_id,omitempty"`
	Username   string `json:"username,omitempty"`
	JobID      *int64 `json:"job_id,omitempty"`
	Status     string `json:"status,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Hash       string `json:"hash,omitempty"`
	// Name、Health、ErrorCode 只用于节点项：前端按 Health 本地化状态，并展示节点名与错误码。
	Name       string    `json:"name,omitempty"`
	Health     string    `json:"health,omitempty"`
	ErrorCode  string    `json:"error_code,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

// InboxCategory 是一类待处理事项的计数与最近若干条。
type InboxCategory struct {
	Key   string       `json:"key"`
	Count int64        `json:"count"`
	Items []InboxEntry `json:"items"`
}

// FailureRateSnapshot 是失败率检查的最近一次结果，供运行诊断与待处理使用。
type FailureRateSnapshot struct {
	WindowMinutes int     `json:"window_minutes"`
	Jobs          int64   `json:"jobs"`
	Failed        int64   `json:"failed"`
	Rate          float64 `json:"rate"`
	Threshold     float64 `json:"threshold"`
	MinJobs       int64   `json:"min_jobs"`
	Exceeded      bool    `json:"exceeded"`
}

// Inbox 是待处理工作台的只读聚合：不存状态，完整列表交给各自的分页接口。
type Inbox struct {
	Total              int64                `json:"total"`
	ActionFailed       int64                `json:"action_failed"`
	NotificationFailed int64                `json:"notification_failed"`
	BreakerPending     int64                `json:"breaker_pending"`
	NodeUnhealthy      int64                `json:"node_unhealthy"`
	FailureRate        *FailureRateSnapshot `json:"failure_rate,omitempty"`
	Categories         []InboxCategory      `json:"categories"`
	// NotificationWindowDays 是通知投递失败的统计窗口天数，前端下钻列表按同一窗口筛选。
	NotificationWindowDays int `json:"notification_window_days"`
}

// InboxCounts 是动作与熔断的待处理计数和最近条目。
type InboxCounts struct {
	ActionFailed       int64
	NotificationFailed int64
	BreakerPending     int64
	ActionItems        []InboxEntry
	BreakerItems       []InboxEntry
}

// inboxRecentLimit 每类事项在工作台展示的最近条数。
const inboxRecentLimit = 5

// inboxNotificationWindow 是工作台统计通知投递失败的时间窗口：更早的失败（如长期退信的邮箱）
// 不再常驻待处理，仍可在动作列表按时间查询。账号动作待核对不设窗口，直到源状态改变。
const inboxNotificationWindow = 7 * 24 * time.Hour

// LoadInboxCounts 汇总动作与熔断的待处理计数和最近条目。
// 两类动作条件分别由迁移 020 的部分索引 tppa_actions_attention_idx、tppa_actions_notification_failed_idx 支撑，
// 改动这里的谓词时必须同步索引，否则会退回全表扫描。
func (r *Repository) LoadInboxCounts(ctx context.Context) (InboxCounts, error) {
	result := InboxCounts{ActionItems: []InboxEntry{}, BreakerItems: []InboxEntry{}}
	if err := r.db.QueryRowContext(ctx, `SELECT
 count(*) FILTER (WHERE execution_status IN ('failed','unknown')),
 count(*) FILTER (WHERE notification_status='failed' AND requested_at>=clock_timestamp()-$1::interval)
 FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions
 WHERE execution_status IN ('failed','unknown') OR notification_status='failed'`, interval(inboxNotificationWindow)).Scan(&result.ActionFailed, &result.NotificationFailed); err != nil {
		return result, err
	}
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM sub2api_enhance.instruction_breakers WHERE status='tripped'`).Scan(&result.BreakerPending); err != nil {
		return result, err
	}
	actionRows, err := r.db.QueryContext(ctx, `SELECT id,action_type,user_id,job_id,execution_status,requested_at,COALESCE(business_snapshot::jsonb->'user'->>'username','')
 FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions WHERE execution_status IN ('failed','unknown') ORDER BY id DESC LIMIT $1`, inboxRecentLimit)
	if err != nil {
		return result, err
	}
	defer func() { _ = actionRows.Close() }()
	for actionRows.Next() {
		item := InboxEntry{Category: "action_failed"}
		if err := actionRows.Scan(&item.ID, &item.ActionType, &item.UserID, &item.JobID, &item.Status, &item.OccurredAt, &item.Username); err != nil {
			return result, err
		}
		result.ActionItems = append(result.ActionItems, item)
	}
	if err := actionRows.Err(); err != nil {
		return result, err
	}
	breakerRows, err := r.db.QueryContext(ctx, `SELECT b.hash,COALESCE(left(p.text,120),''),b.distinct_users,b.tripped_at
 FROM sub2api_enhance.instruction_breakers b LEFT JOIN sub2api_enhance.prompt_texts p ON p.hash=b.hash
 WHERE b.status='tripped' ORDER BY b.tripped_at DESC LIMIT $1`, inboxRecentLimit)
	if err != nil {
		return result, err
	}
	defer func() { _ = breakerRows.Close() }()
	for breakerRows.Next() {
		item := InboxEntry{Category: "breaker_pending"}
		var users int
		if err := breakerRows.Scan(&item.Hash, &item.Detail, &users, &item.OccurredAt); err != nil {
			return result, err
		}
		item.Status = fmt.Sprintf("%d 个用户", users)
		result.BreakerItems = append(result.BreakerItems, item)
	}
	return result, breakerRows.Err()
}

// NotificationFailedItems 返回统计窗口内投递失败的动作条目。
func (r *Repository) NotificationFailedItems(ctx context.Context, limit int) ([]InboxEntry, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,action_type,user_id,job_id,notification_status,requested_at,COALESCE(business_snapshot::jsonb->'user'->>'username','')
 FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions
 WHERE notification_status='failed' AND requested_at>=clock_timestamp()-$2::interval ORDER BY id DESC LIMIT $1`, limit, interval(inboxNotificationWindow))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := []InboxEntry{}
	for rows.Next() {
		item := InboxEntry{Category: "notification_failed"}
		if err := rows.Scan(&item.ID, &item.ActionType, &item.UserID, &item.JobID, &item.Status, &item.OccurredAt, &item.Username); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// RiskUserQuery 是用户风险榜的时间范围、关键字、排序与分页。
type RiskUserQuery struct {
	From     time.Time
	To       time.Time
	Keyword  string
	Sort     string
	Page     int
	PageSize int
}

// RiskUser 是用户风险榜的一行；统计范围受保留天数限制。
type RiskUser struct {
	UserID                int64      `json:"user_id"`
	Username              string     `json:"username"`
	Email                 string     `json:"email"`
	Role                  string     `json:"role"`
	Status                string     `json:"status"`
	Jobs                  int64      `json:"jobs"`
	Blocks                int64      `json:"blocks"`
	Reviews               int64      `json:"reviews"`
	Violations            int64      `json:"violations"`
	LastBlockAt           *time.Time `json:"last_block_at"`
	DisableViolationCount int64      `json:"disable_violation_count"`
	DisableResetAt        *time.Time `json:"disable_reset_at"`
	PendingActions        int64      `json:"pending_actions"`
	Escalated             bool       `json:"escalated"`
	Categories            []string   `json:"categories"`
}

// riskUserSortSQL 只允许三种排序键，SQL 片段全部来自本包常量。
var riskUserSortSQL = map[string]string{
	"blocks":        "a.blocks DESC, a.user_id",
	"violations":    "a.violations DESC, a.user_id",
	"last_block_at": "a.last_block_at DESC NULLS LAST, a.user_id",
}

// riskUserWhere 是风险榜的展示字段过滤条件：关键字匹配用户名、邮箱或用户 ID。
const riskUserWhere = `($3='' OR COALESCE(u.username,'') ILIKE '%'||$3||'%' OR COALESCE(u.email,'') ILIKE '%'||$3||'%' OR ($4>0 AND a.user_id=$4))`

// RiskUsers 返回聚合后的用户风险榜；按“内层聚合、外层连接展示字段”组织。
func (r *Repository) RiskUsers(ctx context.Context, query RiskUserQuery) (*Page[RiskUser], error) {
	orderBy, ok := riskUserSortSQL[query.Sort]
	if !ok {
		orderBy = riskUserSortSQL["blocks"]
	}
	keyword := strings.TrimSpace(query.Keyword)
	keywordID := int64(0)
	if keyword != "" {
		if parsed, err := strconv.ParseInt(keyword, 10, 64); err == nil && parsed > 0 {
			keywordID = parsed
		}
	}
	result := &Page[RiskUser]{Items: []RiskUser{}, Page: query.Page, PageSize: query.PageSize}
	args := []any{query.From, query.To, keyword, keywordID}
	countQuery := `WITH agg AS (
 SELECT j.user_id FROM sub2api_enhance.audit_jobs j
 WHERE j.created_at >= $1 AND j.created_at < $2
 GROUP BY j.user_id
)
SELECT count(*) FROM agg a
LEFT JOIN public.users u ON u.id = a.user_id AND u.deleted_at IS NULL
WHERE ` + riskUserWhere
	if err := r.db.QueryRowContext(ctx, countQuery, args...).Scan(&result.Total); err != nil {
		return nil, err
	}
	pageArgs := append(append([]any{}, args...), query.PageSize, (query.Page-1)*query.PageSize)
	rows, err := r.db.QueryContext(ctx, `WITH agg AS (
 SELECT j.user_id,
        count(*) AS jobs,
        count(*) FILTER (WHERE j.decision = 'block') AS blocks,
        count(*) FILTER (WHERE j.decision = 'review') AS reviews,
        count(*) FILTER (WHERE j.violation_counted) AS violations,
        max(j.created_at) FILTER (WHERE j.decision = 'block') AS last_block_at
 FROM sub2api_enhance.audit_jobs j
 WHERE j.created_at >= $1 AND j.created_at < $2
 GROUP BY j.user_id
)
SELECT a.user_id, COALESCE(u.username,''), COALESCE(u.email,''), COALESCE(u.role,''), COALESCE(u.status,''),
       a.jobs, a.blocks, a.reviews, a.violations, a.last_block_at,
       COALESCE(s.disable_violation_count,0), s.disable_reset_at,
       (SELECT count(*) FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions ea
         WHERE ea.user_id=a.user_id AND ea.execution_status IN ('pending','processing','unknown')),
       (s.escalated_at IS NOT NULL),
       COALESCE((SELECT j2.trigger_categories FROM sub2api_enhance.audit_jobs j2
         WHERE j2.user_id=a.user_id AND j2.decision='block' AND j2.trigger_categories IS NOT NULL
         ORDER BY j2.id DESC LIMIT 1),'{}')
FROM agg a
LEFT JOIN public.users u ON u.id = a.user_id AND u.deleted_at IS NULL
LEFT JOIN sub2api_enhance.third_party_prompt_audit_enforcement_states s ON s.user_id = a.user_id
WHERE `+riskUserWhere+`
ORDER BY `+orderBy+` LIMIT $5 OFFSET $6`, pageArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		item := RiskUser{}
		if err := rows.Scan(&item.UserID, &item.Username, &item.Email, &item.Role, &item.Status, &item.Jobs, &item.Blocks, &item.Reviews,
			&item.Violations, &item.LastBlockAt, &item.DisableViolationCount, &item.DisableResetAt, &item.PendingActions, &item.Escalated, pq.Array(&item.Categories)); err != nil {
			return nil, err
		}
		result.Items = append(result.Items, item)
	}
	return result, rows.Err()
}

// CostQuery 是审核成本看板的时间范围与时区、分桶。
type CostQuery struct {
	From     time.Time
	To       time.Time
	Bucket   string
	Timezone string
}

// CostBucket 是按分桶、节点、调用类型与阶段聚合的调用成本。
type CostBucket struct {
	Bucket  time.Time `json:"bucket"`
	ModelID string    `json:"model_id"`
	// NodeName 优先取当前配置的节点名称，配置缺失时回退到调用快照中的名称。
	NodeName     string   `json:"node_name"`
	CallKind     string   `json:"call_kind"`
	Stage        string   `json:"stage"`
	Calls        int64    `json:"calls"`
	Succeeded    int64    `json:"succeeded"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	UnknownUsage int64    `json:"unknown_usage"`
	P50LatencyMS *float64 `json:"p50_latency_ms"`
}

// CostReuse 是按目标统计的复用节省。
type CostReuse struct {
	Kind       string `json:"kind"`
	Resolution string `json:"resolution"`
	Targets    int64  `json:"targets"`
}

// CostReport 是审核成本看板的完整结果。
type CostReport struct {
	From     time.Time    `json:"from"`
	To       time.Time    `json:"to"`
	Bucket   string       `json:"bucket"`
	Timezone string       `json:"timezone"`
	Buckets  []CostBucket `json:"buckets"`
	Reuse    []CostReuse  `json:"reuse"`
}

// CostStats 以真实发出调用的时间为依据聚合调用量与 token；用量未知的调用单独计数。
func (r *Repository) CostStats(ctx context.Context, query CostQuery, nodeNames map[string]string) (*CostReport, error) {
	report := &CostReport{From: query.From, To: query.To, Bucket: query.Bucket, Timezone: query.Timezone,
		Buckets: []CostBucket{}, Reuse: []CostReuse{}}
	rows, err := r.db.QueryContext(ctx, `SELECT date_trunc($3, a.dispatch_started_at AT TIME ZONE $4) AS bucket,
       a.model_id, COALESCE(a.model_snapshot::jsonb->>'name','') AS node_name, a.call_kind, a.stage,
       count(*) AS calls,
       count(*) FILTER (WHERE a.status = 'succeeded') AS succeeded,
       COALESCE(sum(a.input_tokens),0) AS input_tokens,
       COALESCE(sum(a.output_tokens),0) AS output_tokens,
       count(*) FILTER (WHERE a.status = 'succeeded' AND (a.input_tokens IS NULL OR a.output_tokens IS NULL)) AS unknown_usage,
       percentile_cont(0.5) WITHIN GROUP (ORDER BY a.latency_ms) AS p50_latency_ms
FROM sub2api_enhance.model_attempts a
WHERE a.dispatch_started_at >= $1 AND a.dispatch_started_at < $2
GROUP BY 1, 2, 3, 4, 5
ORDER BY 1, 2, 4, 5`, query.From, query.To, query.Bucket, query.Timezone)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		item := CostBucket{}
		if err := rows.Scan(&item.Bucket, &item.ModelID, &item.NodeName, &item.CallKind, &item.Stage, &item.Calls, &item.Succeeded,
			&item.InputTokens, &item.OutputTokens, &item.UnknownUsage, &item.P50LatencyMS); err != nil {
			return nil, err
		}
		// 当前配置中的节点名称优先，配置已删除的节点回退到调用快照。
		if name := nodeNames[item.ModelID]; name != "" {
			item.NodeName = name
		}
		report.Buckets = append(report.Buckets, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	reuseRows, err := r.db.QueryContext(ctx, `SELECT t.kind, t.resolution, count(*) AS targets
FROM sub2api_enhance.audit_job_targets t
JOIN sub2api_enhance.audit_jobs j ON j.id = t.job_id
WHERE j.created_at >= $1 AND j.created_at < $2 AND t.resolution IS NOT NULL
GROUP BY 1, 2 ORDER BY 1, 2`, query.From, query.To)
	if err != nil {
		return nil, err
	}
	defer func() { _ = reuseRows.Close() }()
	for reuseRows.Next() {
		item := CostReuse{}
		if err := reuseRows.Scan(&item.Kind, &item.Resolution, &item.Targets); err != nil {
			return nil, err
		}
		report.Reuse = append(report.Reuse, item)
	}
	return report, reuseRows.Err()
}
