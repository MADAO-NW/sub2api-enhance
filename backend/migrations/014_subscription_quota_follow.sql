-- 已发布的用户配额记录继续只读保留；新调度仅处理订阅，不转换历史交付对象。
CREATE TABLE sub2api_enhance.quota_follow_group_runtime (
    group_id BIGINT PRIMARY KEY,
    snapshot JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

ALTER TABLE sub2api_enhance.quota_follow_reset_events
    ADD COLUMN group_id BIGINT,
    ADD COLUMN trigger_type TEXT NOT NULL DEFAULT 'legacy',
    ADD COLUMN subscriptions JSONB NOT NULL DEFAULT '[]';

CREATE TABLE sub2api_enhance.quota_follow_subscription_batches (
    id BIGSERIAL PRIMARY KEY,
    event_id BIGINT NOT NULL REFERENCES sub2api_enhance.quota_follow_reset_events(id),
    group_id BIGINT NOT NULL,
    request_id TEXT NOT NULL UNIQUE,
    request_body TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','inflight','succeeded','partial','failed','skipped','uncertain','reviewed')),
    response_body TEXT NOT NULL DEFAULT '',
    http_status INTEGER,
    error_message TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX quota_follow_subscription_batches_pending_idx ON sub2api_enhance.quota_follow_subscription_batches(id) WHERE status='pending';
-- 分组互斥与事件收尾不扫描已完成批次的全部历史。
CREATE INDEX quota_follow_subscription_batches_group_open_idx ON sub2api_enhance.quota_follow_subscription_batches(group_id,id) WHERE status IN ('pending','inflight','uncertain');
CREATE INDEX quota_follow_subscription_batches_event_idx ON sub2api_enhance.quota_follow_subscription_batches(event_id);
CREATE TABLE sub2api_enhance.quota_follow_subscription_items (
    id BIGSERIAL PRIMARY KEY,
    event_id BIGINT NOT NULL REFERENCES sub2api_enhance.quota_follow_reset_events(id),
    batch_id BIGINT NOT NULL REFERENCES sub2api_enhance.quota_follow_subscription_batches(id),
    subscription_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL,
    group_id BIGINT NOT NULL,
    username TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','inflight','succeeded','failed','skipped','uncertain')),
    before_snapshot JSONB,
    after_snapshot JSONB,
    error_message TEXT NOT NULL DEFAULT '',
    UNIQUE(event_id,subscription_id)
);
CREATE INDEX quota_follow_subscription_items_batch_idx ON sub2api_enhance.quota_follow_subscription_items(batch_id,id);
ALTER TABLE sub2api_enhance.quota_reset_records
    ADD COLUMN group_id BIGINT,
    ADD COLUMN subscription_id BIGINT,
    ADD COLUMN subscription_item_id BIGINT REFERENCES sub2api_enhance.quota_follow_subscription_items(id),
    ADD COLUMN batch_id BIGINT REFERENCES sub2api_enhance.quota_follow_subscription_batches(id);
ALTER TABLE sub2api_enhance.quota_reset_records DROP CONSTRAINT quota_reset_records_audit_log_id_key;
CREATE UNIQUE INDEX quota_reset_records_legacy_audit_uniq ON sub2api_enhance.quota_reset_records(audit_log_id) WHERE subscription_id IS NULL;
CREATE UNIQUE INDEX quota_reset_records_subscription_audit_uniq ON sub2api_enhance.quota_reset_records(audit_log_id,subscription_id,"window") WHERE subscription_id IS NOT NULL;
CREATE INDEX quota_reset_records_subscription_item_idx ON sub2api_enhance.quota_reset_records(subscription_item_id) WHERE subscription_item_id IS NOT NULL;
CREATE INDEX quota_reset_records_subscription_window_idx ON sub2api_enhance.quota_reset_records(subscription_id,"window",occurred_at) WHERE subscription_id IS NOT NULL;
CREATE INDEX quota_reset_records_group_idx ON sub2api_enhance.quota_reset_records(group_id,occurred_at DESC,id DESC);
CREATE TABLE sub2api_enhance.quota_follow_subscription_snapshots (
    subscription_id BIGINT PRIMARY KEY,
    snapshot JSONB NOT NULL
);

-- 旧任务不能落到新订阅上。已发送但未确认的动作保留不确定状态，禁止重新发送。
UPDATE sub2api_enhance.quota_follow_reset_deliveries SET status=CASE WHEN status='pending' THEN 'skipped' ELSE 'uncertain' END,error_message='订阅跟随升级：旧用户配额交付已停止',completed_at=clock_timestamp() WHERE status IN ('pending','inflight');
UPDATE sub2api_enhance.quota_reset_records SET status=d.status,error_message=d.error_message FROM sub2api_enhance.quota_follow_reset_deliveries d WHERE delivery_id=d.id AND quota_reset_records.status IN ('pending','inflight');
UPDATE sub2api_enhance.quota_follow_reset_events SET status='uncertain',completed_at=clock_timestamp() WHERE status='pending';

-- 原配置保留分组及勾选项，但新语义必须重新确认后启用；已有配置修订递增。
UPDATE sub2api_enhance.settings SET value=jsonb_build_object(
    'enabled',false,'revision',COALESCE((value->>'revision')::bigint,1)+1,
    'min_interval_minutes',COALESCE((value->>'min_interval_minutes')::integer,10),
    'max_interval_minutes',COALESCE((value->>'max_interval_minutes')::integer,15),
    'updated_by',COALESCE((value->>'updated_by')::bigint,0),'updated_at',clock_timestamp(),
    'groups',CASE WHEN value->>'group_id' IS NULL THEN '[]'::jsonb ELSE jsonb_build_array(jsonb_build_object(
        'group_id',(value->>'group_id')::bigint,'enabled',false,'observe_only',true,
        'reset_daily_enabled',COALESCE((value->>'reset_daily_enabled')::boolean,false),
        'reset_weekly_enabled',COALESCE((value->>'reset_weekly_enabled')::boolean,true),
        'epoch','','enabled_at',NULL)) END),updated_at=clock_timestamp()
WHERE key='quota_follow_config' AND NOT (value ? 'groups');
