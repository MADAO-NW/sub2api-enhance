-- 违规类别（由 reason 前缀按任务计算）。
ALTER TABLE sub2api_enhance.audit_job_targets ADD COLUMN IF NOT EXISTS categories TEXT[];
ALTER TABLE sub2api_enhance.audit_jobs ADD COLUMN IF NOT EXISTS trigger_categories TEXT[];
ALTER TABLE sub2api_enhance.audit_job_rounds ADD COLUMN IF NOT EXISTS trigger_categories TEXT[];

-- 自动加严状态；任务被清理后来源置空。
ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_states
    ADD COLUMN IF NOT EXISTS escalated_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS escalated_job_id BIGINT REFERENCES sub2api_enhance.audit_jobs(id) ON DELETE SET NULL;

-- 新增加严与解除加严动作类型。
ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_actions
    DROP CONSTRAINT IF EXISTS third_party_prompt_audit_enforcement_actions_action_type_check;
ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_actions
    ADD CONSTRAINT third_party_prompt_audit_enforcement_actions_action_type_check
    CHECK (action_type IN ('warning', 'disable', 'counter_reset', 'blocking_notice', 'breaker_notice',
                           'node_auth_notice', 'audit_failure_notice', 'escalation', 'escalation_clear'));

-- 误判样本集：只引用 prompt_texts 已有文本，不新增正文来源。
CREATE TABLE IF NOT EXISTS sub2api_enhance.policy_samples (
    id BIGSERIAL PRIMARY KEY,
    hash TEXT NOT NULL UNIQUE REFERENCES sub2api_enhance.prompt_texts(hash),
    kind TEXT NOT NULL CHECK (kind IN ('instruction', 'user')),
    expected TEXT NOT NULL CHECK (expected IN ('pass', 'block')),
    note TEXT NOT NULL DEFAULT '',
    source_job_id BIGINT REFERENCES sub2api_enhance.audit_jobs(id) ON DELETE SET NULL,
    source_user_id BIGINT,
    created_by BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

-- 政策回放：记录草稿配置、样本选择与逐样本结论。
CREATE TABLE IF NOT EXISTS sub2api_enhance.policy_replays (
    id BIGSERIAL PRIMARY KEY,
    status TEXT NOT NULL CHECK (status IN ('running', 'done', 'failed', 'interrupted')),
    config_snapshot TEXT NOT NULL,
    sample_ids BIGINT[] NOT NULL,
    results TEXT NOT NULL DEFAULT '[]',
    error_message TEXT,
    created_by BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    finished_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS policy_replays_created_idx ON sub2api_enhance.policy_replays (created_at DESC, id DESC);
