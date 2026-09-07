-- 迁移仅创建增强自有表；运行时归零走 API，周一结转在受控事务中更新原周用量与窗口。
CREATE TABLE sub2api_enhance.quota_follow_runtime (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(singleton),
    epoch TEXT NOT NULL DEFAULT '',
    account_set_hash TEXT NOT NULL DEFAULT '',
    accounts JSONB NOT NULL DEFAULT '[]',
    last_event_at TIMESTAMPTZ,
    last_checked_at TIMESTAMPTZ,
    next_check_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    last_error TEXT NOT NULL DEFAULT '',
    paused_revision BIGINT,
    carryover_snapshot_at TIMESTAMPTZ,
    carryover_checked_week TIMESTAMPTZ,
    carryover_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE sub2api_enhance.quota_follow_account_states (
    account_id BIGINT PRIMARY KEY,
    epoch TEXT NOT NULL,
    account_set_hash TEXT NOT NULL,
    snapshot JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE sub2api_enhance.quota_follow_reset_events (
    id BIGSERIAL PRIMARY KEY,
    event_key TEXT NOT NULL UNIQUE,
    epoch TEXT NOT NULL,
    account_set_hash TEXT NOT NULL,
    reset_at TIMESTAMPTZ NOT NULL,
    accounts JSONB NOT NULL,
    users JSONB NOT NULL,
    config_snapshot JSONB NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('observed','pending','completed','partial','uncertain')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    completed_at TIMESTAMPTZ
);
CREATE TABLE sub2api_enhance.quota_follow_reset_deliveries (
    id BIGSERIAL PRIMARY KEY,
    event_id BIGINT NOT NULL REFERENCES sub2api_enhance.quota_follow_reset_events(id),
    user_id BIGINT NOT NULL,
    "window" TEXT NOT NULL CHECK("window" IN ('daily','weekly')),
    request_id TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','inflight','succeeded','failed','skipped','uncertain')),
    request_body TEXT NOT NULL,
    response_body TEXT,
    before_snapshot JSONB,
    after_snapshot JSONB,
    cache_snapshot JSONB,
    before_cache_snapshot JSONB,
    database_status TEXT NOT NULL DEFAULT 'not_observed',
    cache_status TEXT NOT NULL DEFAULT 'not_observed',
    cache_retry_at TIMESTAMPTZ,
    http_status INTEGER,
    error_message TEXT NOT NULL DEFAULT '',
    audit_log_id BIGINT,
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE(event_id,user_id,"window")
);
CREATE INDEX quota_follow_pending_idx ON sub2api_enhance.quota_follow_reset_deliveries(id) WHERE status='pending';
CREATE INDEX quota_follow_uncertain_idx ON sub2api_enhance.quota_follow_reset_deliveries(id) WHERE status IN ('inflight','uncertain');
-- 按即将到来的周一保存最后一次真实观测，不使用过期历史窗口或 Redis 金额猜测补偿。
CREATE TABLE sub2api_enhance.quota_follow_weekly_snapshots (
    user_id BIGINT NOT NULL,
    week_start TIMESTAMPTZ NOT NULL,
    epoch TEXT NOT NULL,
    account_set_hash TEXT NOT NULL,
    snapshot JSONB NOT NULL,
    cache_snapshot JSONB NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(user_id,week_start)
);
CREATE TABLE sub2api_enhance.quota_follow_weekly_carryovers (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL,
    week_start TIMESTAMPTZ NOT NULL,
    epoch TEXT NOT NULL,
    account_set_hash TEXT NOT NULL,
    config_snapshot JSONB NOT NULL,
    snapshot JSONB,
    before_snapshot JSONB,
    after_snapshot JSONB,
    cache_before JSONB,
    cache_after JSONB,
    status TEXT NOT NULL CHECK(status IN ('pending','succeeded','skipped')),
    cache_status TEXT NOT NULL DEFAULT 'pending',
    cache_retry_at TIMESTAMPTZ,
    error_message TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    applied_at TIMESTAMPTZ,
    UNIQUE(user_id,week_start)
);
CREATE INDEX quota_follow_carryover_cache_idx ON sub2api_enhance.quota_follow_weekly_carryovers(id) WHERE status='succeeded' AND cache_status<>'repaired';
CREATE TABLE sub2api_enhance.quota_reset_records (
    id BIGSERIAL PRIMARY KEY,
    record_key TEXT NOT NULL UNIQUE,
    source TEXT NOT NULL CHECK(source IN ('sub2api','enhance')),
    source_detail TEXT NOT NULL,
    evidence_type TEXT NOT NULL CHECK(evidence_type IN ('confirmed','inferred')),
    action_type TEXT NOT NULL DEFAULT 'reset' CHECK(action_type IN ('reset','carryover')),
    user_id BIGINT NOT NULL,
    username TEXT NOT NULL DEFAULT '',
    "window" TEXT CHECK("window" IN ('daily','weekly')),
    occurred_at TIMESTAMPTZ NOT NULL,
    detected_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    status TEXT NOT NULL,
    before_snapshot JSONB,
    after_snapshot JSONB,
    evidence JSONB NOT NULL DEFAULT '{}',
    event_id BIGINT REFERENCES sub2api_enhance.quota_follow_reset_events(id),
    carryover_id BIGINT UNIQUE REFERENCES sub2api_enhance.quota_follow_weekly_carryovers(id),
    delivery_id BIGINT UNIQUE REFERENCES sub2api_enhance.quota_follow_reset_deliveries(id),
    audit_log_id BIGINT UNIQUE,
    request_id TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    merged_into BIGINT REFERENCES sub2api_enhance.quota_reset_records(id),
    CHECK(merged_into IS NULL OR merged_into<>id)
);
CREATE INDEX quota_reset_records_time_idx ON sub2api_enhance.quota_reset_records(occurred_at DESC,id DESC) WHERE merged_into IS NULL;
CREATE INDEX quota_reset_records_user_idx ON sub2api_enhance.quota_reset_records(user_id,"window",occurred_at DESC);
CREATE TABLE sub2api_enhance.quota_follow_user_snapshots (
    user_id BIGINT PRIMARY KEY,
    snapshot JSONB NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE sub2api_enhance.quota_follow_collector_state (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(singleton),
    started_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    audit_watermark_at TIMESTAMPTZ,
    audit_watermark_id BIGINT NOT NULL DEFAULT 0,
    last_completed_at TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT ''
);
