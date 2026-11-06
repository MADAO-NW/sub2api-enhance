package quotafollow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	infra "sub2api-enhance/internal/pkg/errors"
)

type ImmediateResetRequest struct {
	GroupID          int64  `json:"group_id" binding:"required,min=1"`
	ExpectedRevision int64  `json:"expected_revision" binding:"required,min=1"`
	RequestID        string `json:"request_id" binding:"required"`
}
type ImmediateResetResult struct {
	EventID int64  `json:"event_id"`
	GroupID int64  `json:"group_id"`
	Status  string `json:"status"`
}

// ImmediateReset 只提交持久化任务；页面超时后使用原请求键可取回同一事件。
func (s *Service) ImmediateReset(ctx context.Context, input ImmediateResetRequest) (ImmediateResetResult, error) {
	out := ImmediateResetResult{GroupID: input.GroupID}
	if input.GroupID <= 0 || input.ExpectedRevision < 1 {
		return out, infra.BadRequest("invalid_reset", "重置参数无效")
	}
	if _, err := uuid.Parse(input.RequestID); err != nil {
		return out, infra.BadRequest("invalid_reset_key", "请求 ID 必须是 UUID")
	}
	// 同一请求键绑定分组及已确认的配置修订，不允许改换范围后重用。
	eventKey := "manual:" + input.RequestID
	var existingGroup, existingRevision int64
	err := s.repo.db.QueryRowContext(ctx, `SELECT id,group_id,status,(config_snapshot->>'revision')::bigint FROM sub2api_enhance.quota_follow_reset_events WHERE event_key=$1`, eventKey).Scan(&out.EventID, &existingGroup, &out.Status, &existingRevision)
	if err == nil {
		if existingGroup != input.GroupID || existingRevision != input.ExpectedRevision {
			return out, infra.Conflict("reset_key_conflict", "请求 ID 已用于其他分组或配置修订")
		}
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	cfg, err := s.repo.Config(ctx)
	if err != nil {
		return out, err
	}
	if cfg.Revision != input.ExpectedRevision {
		return out, infra.Conflict("quota_follow_config_conflict", "配置已变化，请重新核对重置范围")
	}
	g, ok := cfg.Group(input.GroupID)
	if !ok || (!g.ResetDaily && !g.ResetWeekly) {
		return out, infra.BadRequest("invalid_reset_group", "分组未配置或没有选择重置窗口")
	}
	if !s.api.Configured() {
		return out, errors.New("原版管理员 API Key 未配置")
	}
	d, err := s.source.DiscoverSubscriptions(ctx, input.GroupID)
	if err != nil {
		return out, err
	}
	tx, err := s.repo.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	// 与配置保存共用事务锁，同时串行化同一请求的首次落库。
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, configLock); err != nil {
		return out, err
	}
	err = tx.QueryRowContext(ctx, `SELECT id,group_id,status,(config_snapshot->>'revision')::bigint FROM sub2api_enhance.quota_follow_reset_events WHERE event_key=$1`, eventKey).Scan(&out.EventID, &existingGroup, &out.Status, &existingRevision)
	if err == nil {
		if existingGroup != input.GroupID || existingRevision != input.ExpectedRevision {
			return out, infra.Conflict("reset_key_conflict", "请求 ID 已用于其他分组或配置修订")
		}
		return out, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if err = checkConfig(ctx, tx, input.ExpectedRevision); err != nil {
		return out, err
	}
	evidence, _ := json.Marshal(d.Accounts)
	out.EventID, err = s.repo.createEvent(ctx, tx, eventKey, "manual", g, accountSetHash(d.Accounts), d, evidence, time.Now().UTC(), cfg.Revision)
	if err != nil {
		return out, err
	}
	out.Status = "pending"
	if len(d.Subscriptions) == 0 {
		out.Status = "completed"
	}
	return out, tx.Commit()
}

// RetryFailed 仅为明确失败订阅创建一次补充事件，原批次保留历史结果，成功和不确定项不会重发。
func (s *Service) RetryFailed(ctx context.Context, id int64) (ImmediateResetResult, error) {
	out := ImmediateResetResult{}
	key := fmt.Sprintf("subscription-retry:%d", id)
	err := s.repo.db.QueryRowContext(ctx, `SELECT id,group_id,status FROM sub2api_enhance.quota_follow_reset_events WHERE event_key=$1`, key).Scan(&out.EventID, &out.GroupID, &out.Status)
	if err == nil {
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	b, err := s.repo.Batch(ctx, id)
	if err != nil {
		return out, err
	}
	if b.Status != "failed" && b.Status != "partial" {
		return out, infra.BadRequest("batch_not_retryable", "只有明确失败的订阅可重试")
	}
	cfg, err := s.repo.Config(ctx)
	if err != nil {
		return out, err
	}
	rule, ok := cfg.Group(b.GroupID)
	if !ok || rule.Epoch != b.Rule.Epoch {
		return out, infra.Conflict("batch_rule_changed", "原分组规则已变化，不能重试旧批次")
	}
	d, err := s.source.DiscoverSubscriptions(ctx, b.GroupID)
	if err != nil {
		return out, err
	}
	failed := map[int64]bool{}
	for _, item := range b.Items {
		if item.Status == "failed" {
			failed[item.SubscriptionID] = true
		}
	}
	targets := d.Subscriptions[:0]
	for _, v := range d.Subscriptions {
		if failed[v.ID] {
			targets = append(targets, v)
		}
	}
	d.Subscriptions = targets
	if len(targets) == 0 {
		return out, infra.BadRequest("batch_nothing_to_retry", "没有仍然有效的失败订阅")
	}
	tx, err := s.repo.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, configLock); err != nil {
		return out, err
	}
	err = tx.QueryRowContext(ctx, `SELECT id,group_id,status FROM sub2api_enhance.quota_follow_reset_events WHERE event_key=$1`, key).Scan(&out.EventID, &out.GroupID, &out.Status)
	if err == nil {
		return out, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if err = checkConfig(ctx, tx, cfg.Revision); err != nil {
		return out, err
	}
	evidence, _ := json.Marshal(d.Accounts)
	out.EventID, err = s.repo.createEvent(ctx, tx, key, "retry", rule, accountSetHash(d.Accounts), d, evidence, time.Now().UTC(), cfg.Revision)
	if err != nil {
		return out, err
	}
	out.GroupID = b.GroupID
	out.Status = "pending"
	return out, tx.Commit()
}

// AcknowledgeUncertain 记录管理员已完成核对，解除同组后续批次阻塞；原逐项未知结果保持不变。
func (s *Service) AcknowledgeUncertain(ctx context.Context, id, actor int64, note string) error {
	if actor <= 0 || note == "" {
		return infra.BadRequest("review_note_required", "请填写人工核对说明")
	}
	tx, err := s.repo.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM sub2api_enhance.quota_follow_subscription_batches WHERE id=$1 FOR UPDATE`, id).Scan(&status); err != nil {
		return err
	}
	if status == "reviewed" {
		return nil
	}
	if status != "uncertain" {
		return infra.Conflict("batch_not_uncertain", "批次不是待人工核对状态")
	}
	evidence, _ := json.Marshal(map[string]any{"actor_user_id": actor, "at": time.Now().UTC(), "note": note, "result": "已人工核对并解除后续批次阻塞，原重置结果仍不确定"})
	if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_reset_records SET evidence=jsonb_set(evidence,'{manual_reviews}',COALESCE(evidence->'manual_reviews','[]'::jsonb)||jsonb_build_array($2::jsonb)) WHERE batch_id=$1`, id, string(evidence)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_subscription_batches SET status='reviewed' WHERE id=$1`, id); err != nil {
		return err
	}
	return tx.Commit()
}
