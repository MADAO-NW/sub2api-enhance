package thirdpartypromptaudit

import (
	"context"
	"time"
)

// FailureRate 计算最近一小时的失败率，供运行诊断与待处理工作台使用。
func (r *Repository) FailureRate(ctx context.Context) (*FailureRateSnapshot, error) {
	window := int(failureAlertWindow / time.Minute)
	var jobs, failed int64
	if err := r.db.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER (WHERE status='failed')
 FROM sub2api_enhance.audit_jobs WHERE created_at>=clock_timestamp()-make_interval(secs=>$1)`, int(failureAlertWindow.Seconds())).Scan(&jobs, &failed); err != nil {
		return nil, err
	}
	snapshot := &FailureRateSnapshot{WindowMinutes: window, Jobs: jobs, Failed: failed, Threshold: failureAlertThreshold, MinJobs: failureAlertMinJobs}
	if jobs > 0 {
		snapshot.Rate = float64(failed) / float64(jobs)
	}
	snapshot.Exceeded = jobs >= failureAlertMinJobs && snapshot.Rate >= failureAlertThreshold
	return snapshot, nil
}

// Inbox 汇总待处理工作台：动作、熔断、节点健康与失败率，全部来自现有源数据。
func (s *Service) Inbox(ctx context.Context) (*Inbox, error) {
	counts, err := s.repo.LoadInboxCounts(ctx)
	if err != nil {
		return nil, err
	}
	notificationItems, err := s.repo.NotificationFailedItems(ctx, inboxRecentLimit)
	if err != nil {
		return nil, err
	}
	failure, err := s.repo.FailureRate(ctx)
	if err != nil {
		return nil, err
	}
	nodeItems, err := s.unhealthyNodes()
	if err != nil {
		return nil, err
	}
	categories := []InboxCategory{
		{Key: "action_failed", Count: counts.ActionFailed, Items: counts.ActionItems},
		{Key: "notification_failed", Count: counts.NotificationFailed, Items: notificationItems},
		{Key: "breaker_pending", Count: counts.BreakerPending, Items: counts.BreakerItems},
		{Key: "node_unhealthy", Count: int64(len(nodeItems)), Items: nodeItems},
	}
	inbox := &Inbox{ActionFailed: counts.ActionFailed, NotificationFailed: counts.NotificationFailed,
		BreakerPending: counts.BreakerPending, NodeUnhealthy: int64(len(nodeItems)),
		Categories: categories, NotificationWindowDays: int(inboxNotificationWindow / (24 * time.Hour))}
	if failure.Exceeded {
		// 失败率异常只在超过阈值时作为待处理事项出现。
		inbox.FailureRate = failure
		inbox.Categories = append(inbox.Categories, InboxCategory{Key: "failure_rate", Count: 1, Items: []InboxEntry{}})
	}
	for _, category := range inbox.Categories {
		inbox.Total += category.Count
	}
	return inbox, nil
}

// unhealthyNodes 读取当前节点健康，把非正常状态（含凭据失效）作为待处理事项。
func (s *Service) unhealthyNodes() ([]InboxEntry, error) {
	snapshot, err := s.config.Active()
	if err != nil {
		return nil, err
	}
	items := []InboxEntry{}
	now := time.Now().UTC()
	for _, node := range s.evaluator.nodeScheduler().runtime(snapshot.Models).Nodes {
		if node.Health == nodeHealthHealthy || node.Health == nodeHealthDisabled {
			continue
		}
		items = append(items, InboxEntry{Category: "node_unhealthy", ActionType: "node", Detail: node.ModelID,
			Status: node.Health + " · " + node.LastErrorCode, OccurredAt: now})
	}
	return items, nil
}
