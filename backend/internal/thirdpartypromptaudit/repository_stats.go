package thirdpartypromptaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type StatsQuery struct {
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Timezone string    `json:"timezone"`
	Mode     string    `json:"mode"`
	ModelID  string    `json:"model_id"`
	Stage    string    `json:"stage"`
}

type Distribution struct {
	Count int64    `json:"count"`
	P50MS *float64 `json:"p50_ms"`
	P95MS *float64 `json:"p95_ms"`
}

type CallStats struct {
	ModelID     string           `json:"model_id"`
	CallKind    string           `json:"call_kind"`
	Stage       string           `json:"stage"`
	Total       int64            `json:"total"`
	HTTPSuccess int64            `json:"http_success"`
	ValidResult int64            `json:"valid_result"`
	Failed      int64            `json:"failed"`
	Unknown     int64            `json:"unknown"`
	InFlight    int64            `json:"in_flight"`
	Errors      map[string]int64 `json:"errors"`
	Latency     Distribution     `json:"latency"`
}

type UserSegmentReuseView struct {
	UserID   int64    `json:"user_id"`
	Username string   `json:"username"`
	Email    string   `json:"email"`
	Reused   int      `json:"reused"`
	Total    int      `json:"total"`
	Rate     *float64 `json:"rate"`
}

type Stats struct {
	CaptureStock         map[string]int64 `json:"capture_stock"`
	ForwardingStock      map[string]int64 `json:"forwarding_stock"`
	ActionExecutionStock map[string]int64 `json:"action_execution_stock"`
	StatsQuery
	AsOf               time.Time              `json:"as_of"`
	Stock              map[string]int64       `json:"stock"`
	OldestWaitingAt    *time.Time             `json:"oldest_waiting_at"`
	WaitingForSlot     int64                  `json:"waiting_for_slot"`
	Cohort             map[string]int64       `json:"cohort"`
	Received           int64                  `json:"received"`
	ReauditsCreated    int64                  `json:"reaudits_created"`
	Formal             map[string]int64       `json:"formal"`
	Reaudit            map[string]int64       `json:"reaudit"`
	Failures           map[string]int64       `json:"failures"`
	CurrentDecisions   map[string]int64       `json:"current_decisions"`
	Gateway            map[string]int64       `json:"gateway"`
	GatewayLatency     Distribution           `json:"gateway_latency"`
	TaskLatency        Distribution           `json:"task_latency"`
	Calls              []CallStats            `json:"calls"`
	EvaluationRounds   int64                  `json:"evaluation_rounds"`
	Reuse              ReuseMetrics           `json:"reuse"`
	SegmentReuse       SegmentReuseView       `json:"segment_reuse"`
	SegmentReuseByUser []UserSegmentReuseView `json:"segment_reuse_by_user"`
	Actions            map[string]int64       `json:"actions"`
	NotificationStock  map[string]int64       `json:"notification_stock"`
	DeliveryStock      map[string]int64       `json:"delivery_stock"`
	AuthCacheStock     map[string]int64       `json:"auth_cache_stock"`
}

// Stats 在同一 PostgreSQL 只读快照内计算各口径，时段统一采用左闭右开区间。
func (r *Repository) Stats(ctx context.Context, q StatsQuery) (*Stats, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	result := &Stats{StatsQuery: q, Calls: []CallStats{}, SegmentReuseByUser: []UserSegmentReuseView{}}
	segmentReuse := struct {
		Overall SegmentReuseView       `json:"overall"`
		Users   []UserSegmentReuseView `json:"users"`
	}{Users: []UserSegmentReuseView{}}
	if err = tx.QueryRowContext(ctx, `SELECT transaction_timestamp()`).Scan(&result.AsOf); err != nil {
		return nil, err
	}
	args := []any{q.From, q.To, q.Mode}
	// 此处每项对应一个业务事实来源，不能把不同时间轴的分母合并。
	queries := []struct {
		sql    string
		args   []any
		target any
	}{
		{`SELECT COALESCE(json_object_agg(processing_status,n),'{}') FROM (SELECT processing_status,count(*) n FROM sub2api_enhance.captures GROUP BY processing_status) x`, nil, &result.CaptureStock},
		{`SELECT COALESCE(json_object_agg(forwarding_status,n),'{}') FROM (SELECT forwarding_status,count(*) n FROM sub2api_enhance.captures GROUP BY forwarding_status) x`, nil, &result.ForwardingStock},
		{`SELECT COALESCE(json_object_agg(execution_status,n),'{}') FROM (SELECT execution_status,count(*) n FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions GROUP BY execution_status) x`, nil, &result.ActionExecutionStock},
		{`SELECT COALESCE(json_object_agg(status,n),'{}') FROM (SELECT status,count(*) n FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE ($1='' OR execution_mode=$1) GROUP BY status) x`, []any{q.Mode}, &result.Stock},
		{`SELECT COALESCE(json_object_agg(status,n),'{}') FROM (SELECT status,count(*) n FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE created_at>=$1 AND created_at<$2 AND ($3='' OR execution_mode=$3) GROUP BY status) x`, args, &result.Cohort},
		{`WITH latest AS (SELECT DISTINCT ON (o.job_id) o.*,j.execution_mode FROM sub2api_enhance.third_party_prompt_audit_outcomes o JOIN sub2api_enhance.third_party_prompt_audit_jobs j ON j.id=o.job_id ORDER BY o.job_id,o.audit_round DESC,o.id DESC) SELECT COALESCE(json_object_agg(decision,n),'{}') FROM (SELECT decision,count(*) n FROM latest WHERE created_at>=$1 AND created_at<$2 AND ($3='' OR execution_mode=$3) GROUP BY decision UNION ALL SELECT 'partial_failure',count(*) FROM latest WHERE partial_failure AND created_at>=$1 AND created_at<$2 AND ($3='' OR execution_mode=$3)) x`, args, &result.Formal},
		{`SELECT COALESCE(json_object_agg(decision,n),'{}') FROM (SELECT o.decision,count(*) n FROM sub2api_enhance.third_party_prompt_audit_outcomes o JOIN sub2api_enhance.third_party_prompt_audit_jobs j ON j.id=o.job_id WHERE o.run_kind='reaudit' AND o.created_at>=$1 AND o.created_at<$2 AND ($3='' OR j.execution_mode=$3) GROUP BY o.decision UNION ALL SELECT 'changed',count(*) FROM sub2api_enhance.third_party_prompt_audit_outcomes o JOIN sub2api_enhance.third_party_prompt_audit_jobs j ON j.id=o.job_id JOIN LATERAL (SELECT po.decision FROM sub2api_enhance.third_party_prompt_audit_outcomes po WHERE po.job_id=o.job_id AND (po.audit_round<o.audit_round OR (po.audit_round=o.audit_round AND po.id<o.id)) ORDER BY po.audit_round DESC,po.id DESC LIMIT 1) prior ON true WHERE o.run_kind='reaudit' AND prior.decision<>o.decision AND o.created_at>=$1 AND o.created_at<$2 AND ($3='' OR j.execution_mode=$3)) x`, args, &result.Reaudit},
		{`SELECT COALESCE(json_object_agg(failure_stage,n),'{}') FROM (SELECT COALESCE(failure_stage,'unknown') failure_stage,count(*) n FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE status='failed' AND finished_at>=$1 AND finished_at<$2 AND ($3='' OR execution_mode=$3) GROUP BY failure_stage) x`, args, &result.Failures},
		{`WITH latest AS (SELECT DISTINCT ON (o.job_id) o.decision,o.job_id,j.created_at,j.execution_mode FROM sub2api_enhance.third_party_prompt_audit_outcomes o JOIN sub2api_enhance.third_party_prompt_audit_jobs j ON j.id=o.job_id ORDER BY o.job_id,o.audit_round DESC,o.id DESC) SELECT COALESCE(json_object_agg(decision,n),'{}') FROM (SELECT decision,count(*) n FROM latest WHERE created_at>=$1 AND created_at<$2 AND ($3='' OR execution_mode=$3) GROUP BY decision) x`, args, &result.CurrentDecisions},
		{`SELECT COALESCE(json_object_agg(gateway_result,n),'{}') FROM (SELECT gateway_result,count(*) n FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE execution_mode='blocking' AND created_at>=$1 AND created_at<$2 AND ($3='' OR execution_mode=$3) GROUP BY gateway_result) x`, args, &result.Gateway},
		{`SELECT row_to_json(x) FROM (SELECT count(gateway_duration_ms) count,percentile_cont(0.5) WITHIN GROUP(ORDER BY gateway_duration_ms) p50_ms,percentile_cont(0.95) WITHIN GROUP(ORDER BY gateway_duration_ms) p95_ms FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE execution_mode='blocking' AND created_at>=$1 AND created_at<$2 AND ($3='' OR execution_mode=$3)) x`, args, &result.GatewayLatency},
		{`SELECT row_to_json(x) FROM (SELECT count(*) count,percentile_cont(0.5) WITHIN GROUP(ORDER BY extract(epoch FROM finished_at-started_at)*1000) p50_ms,percentile_cont(0.95) WITHIN GROUP(ORDER BY extract(epoch FROM finished_at-started_at)*1000) p95_ms FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE started_at IS NOT NULL AND finished_at>=$1 AND finished_at<$2 AND ($3='' OR execution_mode=$3)) x`, args, &result.TaskLatency},
		{`SELECT row_to_json(x) FROM (SELECT count(*) FILTER(WHERE (reuse_metrics::json->>'whole_lookups')::bigint>0) whole_lookups,count(*) FILTER(WHERE (reuse_metrics::json->>'whole_hits')::bigint>0) whole_hits,COALESCE(sum((reuse_metrics::json->>'segment_lookups')::bigint),0) segment_lookups,COALESCE(sum((reuse_metrics::json->>'segment_hits')::bigint),0) segment_hits,COALESCE(sum((reuse_metrics::json->>'within_job_hits')::bigint),0) within_job_hits,COALESCE(sum(COALESCE((reuse_metrics::json->>'inflight_hits')::bigint,0)),0) inflight_hits,COALESCE(sum(COALESCE((reuse_metrics::json->>'short_circuited_nodes')::bigint,0)),0) short_circuited_nodes FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE created_at>=$1 AND created_at<$2 AND ($3='' OR execution_mode=$3)) x`, args, &result.Reuse},
		{`WITH latest AS MATERIALIZED (SELECT DISTINCT ON (o.job_id) o.id,o.user_id,j.identity_snapshot FROM sub2api_enhance.third_party_prompt_audit_outcomes o JOIN sub2api_enhance.third_party_prompt_audit_jobs j ON j.id=o.job_id WHERE j.created_at>=$1 AND j.created_at<$2 AND ($3='' OR j.execution_mode=$3) ORDER BY o.job_id,o.audit_round DESC,o.id DESC), user_counts AS (SELECT latest.user_id,max(NULLIF(latest.identity_snapshot::json->>'username','')) snapshot_username,max(NULLIF(latest.identity_snapshot::json->>'user_email','')) snapshot_email,count(s.outcome_id) FILTER(WHERE s.reuse_kind IN ('history','within_job','inflight','full_evaluation')) reused,count(s.outcome_id) total FROM latest LEFT JOIN sub2api_enhance.third_party_prompt_audit_outcome_segments s ON s.outcome_id=latest.id GROUP BY latest.user_id), overall AS (SELECT COALESCE(sum(reused),0) reused,COALESCE(sum(total),0) total FROM user_counts) SELECT json_build_object('overall',json_build_object('reused',overall.reused,'total',overall.total,'rate',CASE WHEN overall.total=0 THEN NULL ELSE overall.reused::double precision/overall.total END),'users',(SELECT COALESCE(json_agg(json_build_object('user_id',counts.user_id,'username',COALESCE(NULLIF(users.username,''),counts.snapshot_username,''),'email',COALESCE(NULLIF(users.email,''),counts.snapshot_email,''),'reused',counts.reused,'total',counts.total,'rate',CASE WHEN counts.total=0 THEN NULL ELSE counts.reused::double precision/counts.total END) ORDER BY counts.user_id),'[]'::json) FROM user_counts counts LEFT JOIN public.users users ON users.id=counts.user_id AND users.deleted_at IS NULL)) FROM overall`, args, &segmentReuse},
		{`SELECT COALESCE(json_object_agg(action_type,n),'{}') FROM (SELECT a.action_type,count(*) n FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions a LEFT JOIN sub2api_enhance.third_party_prompt_audit_outcomes o ON o.id=a.outcome_id LEFT JOIN sub2api_enhance.third_party_prompt_audit_jobs j ON j.id=o.job_id WHERE a.applied_at>=$1 AND a.applied_at<$2 AND ($3='' OR j.execution_mode=$3) GROUP BY a.action_type) x`, args, &result.Actions},
		{`SELECT COALESCE(json_object_agg(notification_status,n),'{}') FROM (SELECT notification_status,count(*) n FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions GROUP BY notification_status) x`, nil, &result.NotificationStock},
		{`SELECT COALESCE(json_object_agg(status,n),'{}') FROM (SELECT d->>'status' status,count(*) n FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions a CROSS JOIN LATERAL json_array_elements(a.deliveries::json) d GROUP BY d->>'status') x`, nil, &result.DeliveryStock},
		{`SELECT COALESCE(json_object_agg(auth_cache_status,n),'{}') FROM (SELECT auth_cache_status,count(*) n FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions GROUP BY auth_cache_status) x`, nil, &result.AuthCacheStock},
	}
	for _, query := range queries {
		var raw []byte
		if err = tx.QueryRowContext(ctx, query.sql, query.args...).Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, query.target); err != nil {
			return nil, err
		}
	}
	result.SegmentReuse = segmentReuse.Overall
	result.SegmentReuseByUser = segmentReuse.Users
	if err = tx.QueryRowContext(ctx, `SELECT min(created_at) FILTER(WHERE status IN ('queued','retry')),count(*) FILTER(WHERE status='processing' AND execution_mode='blocking' AND attempts=0) FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE ($1='' OR execution_mode=$1)`, q.Mode).Scan(&result.OldestWaitingAt, &result.WaitingForSlot); err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT
	 (SELECT count(*) FROM sub2api_enhance.third_party_prompt_audit_jobs j WHERE j.created_at>=$1 AND j.created_at<$2 AND ($3='' OR j.execution_mode=$3)),
 (SELECT count(*) FROM sub2api_enhance.third_party_prompt_audit_outcomes o JOIN sub2api_enhance.third_party_prompt_audit_jobs j ON j.id=o.job_id WHERE o.run_kind='reaudit' AND o.created_at>=$1 AND o.created_at<$2 AND ($3='' OR j.execution_mode=$3)),
 (SELECT count(DISTINCT (a.job_id,a.audit_round,a.evaluation_round)) FROM sub2api_enhance.third_party_prompt_audit_model_attempts a LEFT JOIN sub2api_enhance.third_party_prompt_audit_jobs j ON j.id=a.job_id WHERE a.job_id IS NOT NULL AND a.audit_round>0 AND a.evaluation_round IS NOT NULL AND a.created_at>=$1 AND a.created_at<$2 AND ($3='' OR j.execution_mode=$3))`, args...).Scan(&result.Received, &result.ReauditsCreated, &result.EvaluationRounds); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `WITH calls AS (
 SELECT a.* FROM sub2api_enhance.third_party_prompt_audit_model_attempts a LEFT JOIN sub2api_enhance.third_party_prompt_audit_jobs j ON j.id=a.job_id
 WHERE a.dispatch_started_at>=$1 AND a.dispatch_started_at<$2 AND ($3='' OR j.execution_mode=$3 OR a.call_kind IN ('probe','health_probe')) AND ($4='' OR a.model_id=$4) AND ($5='' OR a.stage=$5)
 ) SELECT row_to_json(x) FROM (
 SELECT model_id,call_kind,stage,count(*) total,
 count(*) FILTER(WHERE http_status BETWEEN 200 AND 299 AND (status='succeeded' OR error_code='invalid_response')) http_success,
 count(*) FILTER(WHERE status='succeeded') valid_result,count(*) FILTER(WHERE status='failed') failed,
 count(*) FILTER(WHERE status='unknown') unknown,count(*) FILTER(WHERE status='started') in_flight,
 json_build_object('count',count(latency_ms),'p50_ms',percentile_cont(0.5) WITHIN GROUP(ORDER BY latency_ms),'p95_ms',percentile_cont(0.95) WITHIN GROUP(ORDER BY latency_ms)) latency,
 (SELECT COALESCE(json_object_agg(error_code,n),'{}') FROM (SELECT c.error_code,count(*) n FROM calls c WHERE c.model_id=calls.model_id AND c.call_kind=calls.call_kind AND c.stage=calls.stage AND c.status='failed' GROUP BY c.error_code) errors) errors
 FROM calls GROUP BY model_id,call_kind,stage ORDER BY model_id,call_kind,stage) x`, q.From, q.To, q.Mode, q.ModelID, q.Stage)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return nil, err
		}
		var item CallStats
		if err = json.Unmarshal(raw, &item); err != nil {
			_ = rows.Close()
			return nil, err
		}
		result.Calls = append(result.Calls, item)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
