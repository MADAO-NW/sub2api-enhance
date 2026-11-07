package thirdpartypromptaudit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"sub2api-enhance/internal/pkg/logger"
)

// MetricsHour 是每小时指标快照的一行。
type MetricsHour struct {
	Hour          time.Time `json:"hour"`
	Jobs          int64     `json:"jobs"`
	Blocked       int64     `json:"blocked"`
	Failed        int64     `json:"failed"`
	AvgBlockingMS *float64  `json:"avg_blocking_ms"`
}

// SnapshotMetrics 把最近两小时的任务聚合写入快照表；同一小时重复执行只覆盖不累加。
func (r *Repository) SnapshotMetrics(ctx context.Context) error {
	for _, offset := range []int{0, 1} {
		if _, err := r.db.ExecContext(ctx, `
INSERT INTO sub2api_enhance.audit_metrics_hourly(hour,jobs,blocked,failed,avg_blocking_ms)
SELECT date_trunc('hour',clock_timestamp())-make_interval(hours=>$1),count(*),
 count(*) FILTER (WHERE decision='block'),count(*) FILTER (WHERE status='failed'),avg(blocking_wait_ms)
FROM sub2api_enhance.audit_jobs
WHERE created_at>=date_trunc('hour',clock_timestamp())-make_interval(hours=>$1)
  AND created_at<date_trunc('hour',clock_timestamp())-make_interval(hours=>$1)+interval '1 hour'
ON CONFLICT (hour) DO UPDATE SET jobs=EXCLUDED.jobs,blocked=EXCLUDED.blocked,failed=EXCLUDED.failed,
 avg_blocking_ms=EXCLUDED.avg_blocking_ms,updated_at=clock_timestamp()`, offset); err != nil {
			return err
		}
	}
	return nil
}

// MetricsHistory 返回最近 N 天的小时快照，按时间升序。
func (r *Repository) MetricsHistory(ctx context.Context, days int) ([]MetricsHour, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT hour,jobs,blocked,failed,avg_blocking_ms
 FROM sub2api_enhance.audit_metrics_hourly WHERE hour>=date_trunc('hour',clock_timestamp())-make_interval(days=>$1)
 ORDER BY hour`, days)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]MetricsHour, 0)
	for rows.Next() {
		var item MetricsHour
		if err := rows.Scan(&item.Hour, &item.Jobs, &item.Blocked, &item.Failed, &item.AvgBlockingMS); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// failureAlertWindow 与 failureAlertThreshold 定义失败率告警的判定窗口与阈值。
const (
	failureAlertWindow    = time.Hour
	failureAlertThreshold = 0.2
	failureAlertMinJobs   = 20
)

// CheckFailureRate 在最近一小时失败率过高时通知管理员；同一小时只通知一次。
func (r *Repository) CheckFailureRate(ctx context.Context, adminEmail string) error {
	var jobs, failed int64
	if err := r.db.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER (WHERE status='failed')
 FROM sub2api_enhance.audit_jobs WHERE created_at>=clock_timestamp()-make_interval(secs=>$1)`, int(failureAlertWindow.Seconds())).
		Scan(&jobs, &failed); err != nil {
		return err
	}
	if jobs < failureAlertMinJobs || float64(failed)/float64(jobs) < failureAlertThreshold {
		return nil
	}
	var exists bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions
 WHERE action_type='audit_failure_notice' AND requested_at>=date_trunc('hour',clock_timestamp()))`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	now := time.Now().UTC()
	action := Action{ActionType: "audit_failure_notice", NotificationStatus: "pending", AuthCacheStatus: "not_required",
		ExecutionStatus: "succeeded", AppliedAt: &now, CompletedAt: &now,
		RuleSnapshot:     map[string]any{"window": failureAlertWindow.String(), "threshold": failureAlertThreshold, "min_jobs": failureAlertMinJobs},
		BusinessSnapshot: map[string]any{"jobs": jobs, "failed": failed, "rate": float64(failed) / float64(jobs)}}
	if adminEmail != "" {
		action.Deliveries = []Delivery{{Recipient: adminEmail, Kind: "admin", Subject: "第三方提示词审计：失败率异常",
			Body:   fmt.Sprintf("<p>最近一小时审核任务失败率 %.0f%%（%d/%d），请检查节点与网络。</p>", float64(failed)/float64(jobs)*100, failed, jobs),
			Status: "pending", Attempts: []DeliveryAttempt{}}}
	} else {
		action.NotificationStatus = "not_required"
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertAction(ctx, tx, &action); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	logger.LegacyPrintf("third_party_prompt_audit", "审核失败率告警已生成 jobs=%d failed=%d threshold=%.2f", jobs, failed, failureAlertThreshold)
	return nil
}

var _ = errors.Is
