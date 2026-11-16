-- 订阅跟随改为单一监控账号、重置全部有效订阅；旧分组运行状态与观测表保留只读，不再写入。
CREATE TABLE sub2api_enhance.quota_follow_monitor_runtime (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(singleton),
    snapshot JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

-- 全局批次横跨多个订阅分组；订阅项与重置记录继续保存各自所属分组。
ALTER TABLE sub2api_enhance.quota_follow_subscription_batches ALTER COLUMN group_id DROP NOT NULL;
DROP INDEX sub2api_enhance.quota_follow_subscription_batches_group_open_idx;
-- 全局互斥与事件收尾只扫描未结束批次。
CREATE INDEX quota_follow_subscription_batches_open_idx ON sub2api_enhance.quota_follow_subscription_batches(id) WHERE status IN ('pending','inflight','uncertain');

-- 跟随固定重置日、周、月三个窗口。
ALTER TABLE sub2api_enhance.quota_reset_records DROP CONSTRAINT quota_reset_records_window_check;
ALTER TABLE sub2api_enhance.quota_reset_records ADD CONSTRAINT quota_reset_records_window_check CHECK("window" IN ('daily','weekly','monthly'));

-- 旧分组事件不再发送：未发出的批次跳过；已发出未确认的转为不确定，保留人工核对，禁止换键重发。
UPDATE sub2api_enhance.quota_follow_subscription_batches SET status='skipped',error_message='订阅跟随改为全局监控：旧分组待发送批次已停止',completed_at=clock_timestamp() WHERE status='pending';
UPDATE sub2api_enhance.quota_follow_subscription_batches SET status='uncertain',error_message='订阅跟随升级时调用结果未确认，请只读核对；禁止换键重发',completed_at=clock_timestamp() WHERE status='inflight';
UPDATE sub2api_enhance.quota_follow_subscription_items i SET status=CASE WHEN b.status='skipped' THEN 'skipped' ELSE 'uncertain' END,error_message=b.error_message FROM sub2api_enhance.quota_follow_subscription_batches b WHERE i.batch_id=b.id AND ((b.status='skipped' AND i.status='pending') OR (b.status='uncertain' AND i.status='inflight'));
UPDATE sub2api_enhance.quota_reset_records r SET status=i.status,error_message=i.error_message FROM sub2api_enhance.quota_follow_subscription_items i WHERE r.subscription_item_id=i.id AND r.status IN ('pending','inflight') AND i.status IN ('skipped','uncertain');
UPDATE sub2api_enhance.quota_follow_reset_events e SET status=CASE WHEN EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_subscription_batches b WHERE b.event_id=e.id AND b.status IN ('uncertain','reviewed')) THEN 'uncertain' WHEN EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_subscription_batches b WHERE b.event_id=e.id AND b.status IN ('partial','failed')) THEN 'partial' ELSE 'completed' END,completed_at=clock_timestamp() WHERE e.trigger_type<>'legacy' AND e.status='pending' AND NOT EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_subscription_batches b WHERE b.event_id=e.id AND b.status IN ('pending','inflight'));
