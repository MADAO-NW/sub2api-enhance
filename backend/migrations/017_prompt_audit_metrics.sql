-- 每小时指标快照：保留期之外仍可查看趋势，不受任务清理影响。
CREATE TABLE IF NOT EXISTS sub2api_enhance.audit_metrics_hourly (
    hour TIMESTAMPTZ PRIMARY KEY,
    jobs BIGINT NOT NULL DEFAULT 0,
    blocked BIGINT NOT NULL DEFAULT 0,
    failed BIGINT NOT NULL DEFAULT 0,
    avg_blocking_ms DOUBLE PRECISION,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

-- 失败率异常时通知管理员；新增动作类型。
ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_actions
    DROP CONSTRAINT third_party_prompt_audit_enforcement_actions_action_type_check;
ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_actions
    ADD CONSTRAINT third_party_prompt_audit_enforcement_actions_action_type_check
    CHECK (action_type IN ('warning', 'disable', 'counter_reset', 'blocking_notice', 'breaker_notice', 'node_auth_notice', 'audit_failure_notice'));
