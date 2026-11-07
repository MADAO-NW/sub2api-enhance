package thirdpartypromptaudit

import (
	"context"
	"fmt"
	"time"
)

// StatsQuery 是统计页面的时间范围与筛选条件。
type StatsQuery struct {
	From     time.Time
	To       time.Time
	Timezone string
	Mode     string
}

// ClientMissRate 是某一客户端指纹的指令块缓存未命中率，只展示不告警（决策 24）。
type ClientMissRate struct {
	Client     string  `json:"client"`
	Kind       string  `json:"kind"`
	Targets    int64   `json:"targets"`
	ModelCalls int64   `json:"model_calls"`
	MissRate   float64 `json:"miss_rate"`
}

// Stats 只包含基于 audit_jobs 的指标，不再有 capture 与逐请求网关库存（决策 20）。
type Stats struct {
	From          time.Time        `json:"from"`
	To            time.Time        `json:"to"`
	Timezone      string           `json:"timezone"`
	Mode          string           `json:"mode"`
	Jobs          int64            `json:"jobs"`
	OrphanJobs    int64            `json:"orphan_jobs"`
	ReauditJobs   int64            `json:"reaudit_jobs"`
	FailedJobs    int64            `json:"failed_jobs"`
	Decisions     map[string]int64 `json:"decisions"`
	WaitingJobs   int64            `json:"waiting_jobs"`
	WaitingSince  *time.Time       `json:"waiting_since"`
	ActionPending int64            `json:"action_pending"`
	ActionByState map[string]int64 `json:"action_by_state"`
	BlockingJobs  int64            `json:"blocking_jobs"`
	BlockingAvgMS *float64         `json:"blocking_avg_ms"`
	BlockingMaxMS *int64           `json:"blocking_max_ms"`
	BlockingP50MS *float64         `json:"blocking_p50_ms"`
	BlockingP95MS *float64         `json:"blocking_p95_ms"`
	JobP50MS      *float64         `json:"job_p50_ms"`
	JobP95MS      *float64         `json:"job_p95_ms"`
	CacheMiss     []ClientMissRate `json:"cache_miss"`
}

// Stats 汇总时间范围内的任务、等待、处置库存、同步等待耗时和缓存未命中率。
func (r *Repository) Stats(ctx context.Context, query StatsQuery) (*Stats, error) {
	stats := &Stats{From: query.From, To: query.To, Timezone: query.Timezone, Mode: query.Mode,
		Decisions: map[string]int64{}, ActionByState: map[string]int64{}, CacheMiss: []ClientMissRate{}}
	modeArgs := []any{}
	modeClause := ""
	if query.Mode != "" {
		modeArgs = append(modeArgs, query.Mode)
		modeClause = fmt.Sprintf(" AND mode=$3")
	}
	var passCount, reviewCount, blockCount int64
	err := r.db.QueryRowContext(ctx, `SELECT count(*),
 count(*) FILTER (WHERE orphan_continuation),
 count(*) FILTER (WHERE current_run_kind='reaudit'),
 count(*) FILTER (WHERE status='failed'),
 count(*) FILTER (WHERE decision='pass'),count(*) FILTER (WHERE decision='review'),count(*) FILTER (WHERE decision='block')
 FROM sub2api_enhance.audit_jobs WHERE created_at>=$1 AND created_at<$2`+modeClause,
		append([]any{query.From, query.To}, modeArgs...)...).Scan(&stats.Jobs, &stats.OrphanJobs, &stats.ReauditJobs, &stats.FailedJobs,
		&passCount, &reviewCount, &blockCount)
	if err != nil {
		return nil, err
	}
	stats.Decisions["pass"], stats.Decisions["review"], stats.Decisions["block"] = passCount, reviewCount, blockCount
	if err := r.db.QueryRowContext(ctx, `SELECT count(*),min(next_attempt_at) FROM sub2api_enhance.audit_jobs WHERE status IN ('queued','retry')`).Scan(&stats.WaitingJobs, &stats.WaitingSince); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT execution_status,count(*) FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions
 WHERE execution_status IN ('pending','processing','unknown') OR notification_status IN ('pending','processing','retry') GROUP BY execution_status`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			_ = rows.Close()
			return nil, err
		}
		stats.ActionByState[state] = count
		stats.ActionPending += count
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := r.db.QueryRowContext(ctx, `SELECT count(*),avg(blocking_wait_ms),max(blocking_wait_ms),
 percentile_cont(0.5) WITHIN GROUP (ORDER BY blocking_wait_ms),percentile_cont(0.95) WITHIN GROUP (ORDER BY blocking_wait_ms)
 FROM sub2api_enhance.audit_jobs
 WHERE mode='blocking' AND blocking_wait_ms IS NOT NULL AND created_at>=$1 AND created_at<$2`, query.From, query.To).
		Scan(&stats.BlockingJobs, &stats.BlockingAvgMS, &stats.BlockingMaxMS, &stats.BlockingP50MS, &stats.BlockingP95MS); err != nil {
		return nil, err
	}
	if err := r.db.QueryRowContext(ctx, `SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (finished_at-created_at))*1000),
 percentile_cont(0.95) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (finished_at-created_at))*1000)
 FROM sub2api_enhance.audit_jobs WHERE finished_at IS NOT NULL AND created_at>=$1 AND created_at<$2`, query.From, query.To).
		Scan(&stats.JobP50MS, &stats.JobP95MS); err != nil {
		return nil, err
	}
	missRows, err := r.db.QueryContext(ctx, `SELECT j.client_fingerprint,t.kind,count(*),count(*) FILTER (WHERE t.resolution='model')
 FROM sub2api_enhance.audit_job_targets t JOIN sub2api_enhance.audit_jobs j ON j.id=t.job_id
 WHERE j.created_at>=$1 AND j.created_at<$2
 GROUP BY j.client_fingerprint,t.kind ORDER BY count(*) DESC`, query.From, query.To)
	if err != nil {
		return nil, err
	}
	defer func() { _ = missRows.Close() }()
	for missRows.Next() {
		item := ClientMissRate{}
		if err := missRows.Scan(&item.Client, &item.Kind, &item.Targets, &item.ModelCalls); err != nil {
			return nil, err
		}
		if item.Targets > 0 {
			item.MissRate = float64(item.ModelCalls) / float64(item.Targets)
		}
		stats.CacheMiss = append(stats.CacheMiss, item)
	}
	return stats, missRows.Err()
}
