package quotafollow

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sub2api-enhance/internal/sub2api"
	"time"
)

func (s *Service) deliver(ctx context.Context) error {
	if err := s.repo.RecoverInflight(ctx); err != nil {
		return err
	}
	ids, err := s.repo.PendingBatches(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = s.deliverBatch(ctx, id); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Service) deliverBatch(ctx context.Context, id int64) error {
	b, err := s.repo.Batch(ctx, id)
	if err != nil {
		return err
	}
	if b.Status != "pending" {
		return nil
	}
	cfg, err := s.repo.Config(ctx)
	if err != nil {
		return err
	}
	// 自动事件受总开关约束：关闭时暂缓发送，而不丢弃已冻结的名单。
	if b.Trigger == "automatic" && !cfg.Enabled {
		return nil
	}
	reason := ""
	if b.Trigger == "automatic" && b.Epoch != cfg.Epoch {
		reason = "监控账号或启用周期已经改变"
	}
	if reason == "" && !s.api.Configured() {
		return errors.New("原版管理员 API Key 未配置")
	}
	// 发送前复核资格：失效或归属变化的订阅只跳过对应项，不重组已冻结的其余项。
	if reason == "" {
		ids := []int64{}
		for _, v := range b.Items {
			if v.Status == "pending" {
				ids = append(ids, v.SubscriptionID)
			}
		}
		current, readErr := s.source.SubscriptionSnapshots(ctx, ids)
		if readErr != nil {
			return readErr
		}
		for i := range b.Items {
			v := &b.Items[i]
			if v.Status != "pending" {
				continue
			}
			snapshot, ok := current[v.SubscriptionID]
			if !ok || snapshot.UserID != v.UserID || snapshot.GroupID != v.GroupID {
				v.Status = "skipped"
				v.Error = "订阅已失效、过期或不再属于目标用户及分组"
				continue
			}
			v.Before = &snapshot
		}
	} else {
		for i := range b.Items {
			if b.Items[i].Status == "pending" {
				b.Items[i].Status = "skipped"
				b.Items[i].Error = reason
			}
		}
	}
	sent, err := s.repo.ClaimBatch(ctx, b)
	if err != nil {
		return err
	}
	if !sent {
		return nil
	}
	log.Printf("开始批量重置订阅 event_id=%d batch_id=%d request_id=%s request=%s", b.EventID, b.ID, b.RequestID, b.Body)
	reply := s.api.ResetSubscriptions(ctx, []byte(b.Body), b.RequestID)
	applyReply(b, reply)
	// 先提交逐项结果：快照只是只读观测，不能因为读取慢或失败而丢失已确认的调用结果。
	finish, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancelFinish()
	if err = s.repo.FinishBatch(finish, b); err != nil {
		log.Printf("订阅批量重置结果保存失败 batch_id=%d status=%s error=%v", b.ID, b.Status, err)
		return err
	}
	log.Printf("订阅批量重置返回 batch_id=%d status=%s response=%s error=%v", b.ID, b.Status, b.Response, reply.Error)
	observe, cancelObserve := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelObserve()
	sentIDs := []int64{}
	for _, v := range b.Items {
		if v.Status != "skipped" {
			sentIDs = append(sentIDs, v.SubscriptionID)
		}
	}
	after, readErr := s.source.SubscriptionSnapshots(observe, sentIDs)
	if readErr != nil {
		log.Printf("订阅批量重置后快照读取失败 batch_id=%d error=%v", b.ID, readErr)
		return nil
	}
	for i := range b.Items {
		if snapshot, ok := after[b.Items[i].SubscriptionID]; ok {
			b.Items[i].After = &snapshot
		}
	}
	if err = s.repo.SaveAfterSnapshots(observe, b.Items); err != nil {
		log.Printf("订阅批量重置后快照保存失败 batch_id=%d error=%v", b.ID, err)
	}
	return nil
}

func applyReply(b *Batch, reply sub2api.SubscriptionResetReply) {
	b.Response = reply.Raw
	b.Error = ""
	if reply.HTTPStatus != 0 {
		status := reply.HTTPStatus
		b.HTTPStatus = &status
	}
	if reply.Error != nil {
		b.Error = reply.Error.Error()
		b.Status = "uncertain"
		// 只有明确拒绝整个请求的状态能判失败；5xx、冲突及不完整响应都可能发生在提交之后。
		if reply.HTTPStatus == 400 || reply.HTTPStatus == 401 || reply.HTTPStatus == 403 || reply.HTTPStatus == 404 || reply.HTTPStatus == 405 || reply.HTTPStatus == 422 {
			b.Status = "failed"
		}
		for i := range b.Items {
			if b.Items[i].Status == "inflight" {
				b.Items[i].Status = b.Status
				b.Items[i].Error = b.Error
			}
		}
		return
	}
	results := map[int64]sub2api.SubscriptionResetItem{}
	for _, v := range reply.Result.Items {
		results[v.ID] = v
	}
	b.Status = "succeeded"
	for i := range b.Items {
		v := &b.Items[i]
		if v.Status != "inflight" {
			continue
		}
		result, ok := results[v.SubscriptionID]
		if !ok || result.Success == nil {
			v.Status = "uncertain"
			v.Error = "原版未返回可确认的订阅结果"
			b.Status = "uncertain"
			continue
		}
		if *result.Success {
			v.Status = "succeeded"
		} else {
			v.Status = "failed"
			v.Error = result.Error
			if b.Status != "uncertain" {
				b.Status = "partial"
			}
		}
	}
}

// Reconcile 仅追加当前订阅观测，不以用量为零推断原调用成功，也不再次调用重置接口。
func (s *Service) Reconcile(ctx context.Context, id int64) (*Batch, error) {
	b, err := s.repo.Batch(ctx, id)
	if err != nil {
		return nil, err
	}
	if b.Status == "pending" || b.Status == "inflight" {
		return nil, errors.New("批次尚未结束，不能并发核对")
	}
	ids := []int64{}
	for _, v := range b.Items {
		ids = append(ids, v.SubscriptionID)
	}
	snapshots, err := s.source.SubscriptionSnapshots(ctx, ids)
	if err != nil {
		return nil, err
	}
	tx, err := s.repo.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for i := range b.Items {
		v := &b.Items[i]
		snapshot, ok := snapshots[v.SubscriptionID]
		var current *sub2api.SubscriptionSnapshot
		if ok {
			current = &snapshot
		}
		raw, _ := json.Marshal(map[string]any{"at": time.Now().UTC(), "snapshot": current, "note": "只读观测，不证明原重置结果或来源"})
		if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_reset_records SET evidence=jsonb_set(evidence,'{reconciliations}',COALESCE(evidence->'reconciliations','[]'::jsonb)||jsonb_build_array($2::jsonb)) WHERE subscription_item_id=$1`, v.ID, string(raw)); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return b, nil
}
