-- v0.3.0 提示词审计重构：不再保存请求原文，改为按“稳定指令块 + 当前回合用户提示词”内容寻址审核。
-- 本迁移删除旧审计表（captures、旧任务、Outcome、片段、批次等），保留并改造处置状态与处置动作。

-- 0. 前置检查：存在未完成的账号动作或通知时中止，避免删除来源后无法确认外部副作用。
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions
        WHERE execution_status IN ('pending', 'processing', 'unknown')
           OR notification_status IN ('pending', 'processing', 'retry')
    ) THEN
        RAISE EXCEPTION '提示词审计存在未完成的处置动作或通知（execution_status 为 pending/processing/unknown，或 notification_status 为 pending/processing/retry），请先在管理页面处理完成后再升级到 v0.3.0';
    END IF;
END
$$;

-- 1. 解除处置表对旧表的外键：删除列会同时删除其外键、唯一约束和引用该列的检查约束。
ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_states
    DROP COLUMN last_outcome_id;
ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_states
    RENAME COLUMN warning_window_after_outcome_id TO warning_window_after_job_id;
-- 旧游标是 Outcome ID，新任务 ID 从 1 开始；置 0 避免提醒窗口长期失效。
UPDATE sub2api_enhance.third_party_prompt_audit_enforcement_states
SET warning_window_after_job_id = 0,
    updated_at = clock_timestamp();

ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_actions
    DROP CONSTRAINT tppa_actions_source;
ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_actions
    DROP COLUMN outcome_id;
ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_actions
    DROP CONSTRAINT third_party_prompt_audit_enforcement_actions_action_type_check;
ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_actions
    ADD CONSTRAINT third_party_prompt_audit_enforcement_actions_action_type_check
    CHECK (action_type IN ('warning', 'disable', 'counter_reset', 'blocking_notice', 'breaker_notice'));

-- 2. 删除旧审计表。
DROP TABLE
    sub2api_enhance.third_party_prompt_audit_batch_items,
    sub2api_enhance.third_party_prompt_audit_batches,
    sub2api_enhance.third_party_prompt_audit_job_targets,
    sub2api_enhance.third_party_prompt_audit_targets,
    sub2api_enhance.third_party_prompt_audit_outcome_segments,
    sub2api_enhance.third_party_prompt_audit_segment_results,
    sub2api_enhance.third_party_prompt_audit_outcomes,
    sub2api_enhance.third_party_prompt_audit_model_attempts,
    sub2api_enhance.third_party_prompt_audit_jobs,
    sub2api_enhance.captures;

-- 3. 新建内容寻址文本、全库复用审核结果与信任记录。
CREATE TABLE sub2api_enhance.prompt_texts (
    hash TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('instruction', 'user')),
    text TEXT NOT NULL,
    bytes INTEGER NOT NULL CHECK (bytes >= 0),
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX prompt_texts_first_seen_idx ON sub2api_enhance.prompt_texts (first_seen_at);

CREATE TABLE sub2api_enhance.audit_jobs (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL,
    api_key_id BIGINT NOT NULL,
    group_id BIGINT,
    identity_snapshot TEXT NOT NULL DEFAULT '{}',
    platform TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    protocol TEXT NOT NULL,
    client_fingerprint TEXT NOT NULL DEFAULT 'generic',
    mode TEXT NOT NULL CHECK (mode IN ('async', 'blocking')),
    turn_fingerprint TEXT NOT NULL,
    orphan_continuation BOOLEAN NOT NULL DEFAULT FALSE,
    status TEXT NOT NULL CHECK (status IN ('queued', 'processing', 'retry', 'done', 'failed', 'skipped')),
    decision TEXT CHECK (decision IN ('pass', 'review', 'block')),
    current_round INTEGER NOT NULL DEFAULT 1 CHECK (current_round > 0),
    current_run_kind TEXT NOT NULL DEFAULT 'request' CHECK (current_run_kind IN ('request', 'reaudit')),
    current_requested_by BIGINT,
    reuse_mode TEXT NOT NULL DEFAULT 'allow' CHECK (reuse_mode IN ('allow', 'force')),
    trigger_hash TEXT,
    trigger_kind TEXT CHECK (trigger_kind IN ('instruction', 'user')),
    enforcement_eligible BOOLEAN NOT NULL DEFAULT FALSE,
    violation_counted BOOLEAN NOT NULL DEFAULT FALSE,
    disable_counted BOOLEAN NOT NULL DEFAULT FALSE,
    config_revision BIGINT,
    config_snapshot TEXT,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts INTEGER NOT NULL DEFAULT 3 CHECK (max_attempts > 0),
    claim_generation BIGINT NOT NULL DEFAULT 0 CHECK (claim_generation >= 0),
    lease_until TIMESTAMPTZ,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    failure_stage TEXT,
    last_error_code TEXT,
    last_error_message TEXT,
    blocking_wait_ms BIGINT CHECK (blocking_wait_ms >= 0),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX audit_jobs_queue_idx ON sub2api_enhance.audit_jobs (next_attempt_at, id)
    WHERE status IN ('queued', 'retry');
CREATE INDEX audit_jobs_lease_idx ON sub2api_enhance.audit_jobs (lease_until, id)
    WHERE status = 'processing';
CREATE INDEX audit_jobs_created_idx ON sub2api_enhance.audit_jobs (created_at DESC, id DESC);
CREATE INDEX audit_jobs_user_idx ON sub2api_enhance.audit_jobs (user_id, id DESC);

CREATE TABLE sub2api_enhance.prompt_reviews (
    id BIGSERIAL PRIMARY KEY,
    hash TEXT NOT NULL REFERENCES sub2api_enhance.prompt_texts(hash) ON DELETE RESTRICT,
    eval_hash TEXT NOT NULL,
    run_kind TEXT NOT NULL CHECK (run_kind IN ('request', 'reaudit')),
    decision TEXT NOT NULL CHECK (decision IN ('pass', 'review', 'block')),
    score DOUBLE PRECISION CHECK (score >= 0 AND score <= 1),
    reason TEXT NOT NULL DEFAULT '',
    node_results JSONB NOT NULL,
    reusable BOOLEAN NOT NULL,
    requested_by BIGINT,
    origin_job_id BIGINT REFERENCES sub2api_enhance.audit_jobs(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX prompt_reviews_lookup_idx ON sub2api_enhance.prompt_reviews (hash, eval_hash, created_at DESC, id DESC);
CREATE INDEX prompt_reviews_origin_idx ON sub2api_enhance.prompt_reviews (origin_job_id) WHERE origin_job_id IS NOT NULL;
CREATE INDEX prompt_reviews_created_idx ON sub2api_enhance.prompt_reviews (created_at);

CREATE TABLE sub2api_enhance.prompt_trust (
    hash TEXT PRIMARY KEY REFERENCES sub2api_enhance.prompt_texts(hash) ON DELETE RESTRICT,
    trusted_by BIGINT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

-- 4. 审核轮次、轮次目标与真实模型调用。
CREATE TABLE sub2api_enhance.audit_job_rounds (
    job_id BIGINT NOT NULL REFERENCES sub2api_enhance.audit_jobs(id) ON DELETE CASCADE,
    round INTEGER NOT NULL CHECK (round > 0),
    run_kind TEXT NOT NULL CHECK (run_kind IN ('request', 'reaudit')),
    requested_by BIGINT,
    reuse_mode TEXT NOT NULL DEFAULT 'allow' CHECK (reuse_mode IN ('allow', 'force')),
    decision TEXT NOT NULL CHECK (decision IN ('pass', 'review', 'block')),
    trigger_hash TEXT,
    trigger_kind TEXT CHECK (trigger_kind IN ('instruction', 'user')),
    config_revision BIGINT,
    config_snapshot TEXT,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (job_id, round)
);
CREATE INDEX audit_job_rounds_finished_idx ON sub2api_enhance.audit_job_rounds (finished_at DESC);

CREATE TABLE sub2api_enhance.audit_job_targets (
    job_id BIGINT NOT NULL REFERENCES sub2api_enhance.audit_jobs(id) ON DELETE CASCADE,
    round INTEGER NOT NULL CHECK (round > 0),
    target_order INTEGER NOT NULL CHECK (target_order > 0),
    hash TEXT NOT NULL REFERENCES sub2api_enhance.prompt_texts(hash) ON DELETE RESTRICT,
    kind TEXT NOT NULL CHECK (kind IN ('instruction', 'user')),
    resolution TEXT CHECK (resolution IN ('model', 'cache', 'inflight', 'trust')),
    review_id BIGINT REFERENCES sub2api_enhance.prompt_reviews(id) ON DELETE RESTRICT,
    decision TEXT CHECK (decision IN ('pass', 'review', 'block')),
    error_code TEXT,
    error_message TEXT,
    resolved_at TIMESTAMPTZ,
    PRIMARY KEY (job_id, round, target_order),
    CONSTRAINT audit_job_targets_resolution CHECK (
        (resolution IS NULL AND decision IS NULL AND review_id IS NULL)
        OR (resolution = 'trust' AND decision = 'pass' AND review_id IS NULL)
        OR (resolution IN ('model', 'cache', 'inflight') AND decision IS NOT NULL AND review_id IS NOT NULL)
    )
);
CREATE INDEX audit_job_targets_hash_idx ON sub2api_enhance.audit_job_targets (hash);
CREATE INDEX audit_job_targets_review_idx ON sub2api_enhance.audit_job_targets (review_id) WHERE review_id IS NOT NULL;

CREATE TABLE sub2api_enhance.model_attempts (
    id BIGSERIAL PRIMARY KEY,
    job_id BIGINT REFERENCES sub2api_enhance.audit_jobs(id) ON DELETE CASCADE,
    audit_round INTEGER CHECK (audit_round > 0),
    target_hash TEXT,
    call_kind TEXT NOT NULL CHECK (call_kind IN ('audit', 'probe', 'health_probe')),
    evaluation_round INTEGER CHECK (evaluation_round > 0),
    model_id TEXT NOT NULL,
    model_snapshot TEXT NOT NULL,
    stage TEXT NOT NULL CHECK (stage IN ('instruction', 'user', 'format_repair', 'probe', 'health_probe')),
    repair_of_attempt_id BIGINT REFERENCES sub2api_enhance.model_attempts(id) ON DELETE CASCADE,
    request_metadata TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'prepared'
        CHECK (status IN ('prepared', 'started', 'succeeded', 'failed', 'unknown')),
    http_status INTEGER,
    raw_response TEXT,
    confidence DOUBLE PRECISION CHECK (confidence >= 0 AND confidence <= 1),
    reason TEXT,
    input_tokens BIGINT CHECK (input_tokens >= 0),
    output_tokens BIGINT CHECK (output_tokens >= 0),
    latency_ms BIGINT CHECK (latency_ms >= 0),
    error_code TEXT,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    dispatch_started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT model_attempts_owner CHECK (
        (call_kind = 'audit' AND job_id IS NOT NULL AND audit_round IS NOT NULL AND target_hash IS NOT NULL AND evaluation_round IS NOT NULL)
        OR (call_kind IN ('probe', 'health_probe') AND job_id IS NULL)
    ),
    CONSTRAINT model_attempts_valid_result CHECK (
        status <> 'succeeded' OR (confidence IS NOT NULL AND reason IS NOT NULL)
    )
);
CREATE INDEX model_attempts_job_idx ON sub2api_enhance.model_attempts (job_id, audit_round, target_hash, id);
CREATE INDEX model_attempts_dispatch_idx ON sub2api_enhance.model_attempts (dispatch_started_at DESC, id DESC)
    WHERE dispatch_started_at IS NOT NULL;
CREATE INDEX model_attempts_probe_created_idx ON sub2api_enhance.model_attempts (created_at)
    WHERE job_id IS NULL;

-- 5. 指令块“只计一次”与熔断；这两张表不随保留期清理。
CREATE TABLE sub2api_enhance.instruction_violation_marks (
    user_id BIGINT NOT NULL,
    hash TEXT NOT NULL,
    first_job_id BIGINT,
    suppressed BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (user_id, hash)
);
CREATE INDEX instruction_violation_marks_hash_idx ON sub2api_enhance.instruction_violation_marks (hash);

CREATE TABLE sub2api_enhance.instruction_breakers (
    hash TEXT PRIMARY KEY,
    status TEXT NOT NULL CHECK (status IN ('tripped', 'trusted', 'confirmed')),
    distinct_users INTEGER NOT NULL CHECK (distinct_users >= 0),
    tripped_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    tripped_job_id BIGINT,
    resolved_by BIGINT,
    resolved_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX instruction_breakers_status_idx ON sub2api_enhance.instruction_breakers (status, tripped_at DESC);

-- 6. 处置表来源改为新任务，任务被保留期清理后来源置空，处置记录本身保留。
ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_states
    ADD COLUMN last_job_id BIGINT REFERENCES sub2api_enhance.audit_jobs(id) ON DELETE SET NULL;
ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_actions
    ADD COLUMN job_id BIGINT REFERENCES sub2api_enhance.audit_jobs(id) ON DELETE SET NULL;
ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_actions
    ADD CONSTRAINT tppa_actions_job_action_uniq UNIQUE (job_id, action_type);
ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_actions
    ADD CONSTRAINT tppa_actions_source CHECK (action_type <> 'counter_reset' OR job_id IS NULL);

-- 7. 配置只保留仍然有效的字段，新增保留天数并升级配置修订号。
UPDATE sub2api_enhance.settings
SET value = (
        (value::jsonb - 'capture_when_audit_off' - 'audit_scope')
        || jsonb_build_object(
            'retention_days', 30,
            'revision', COALESCE((value::jsonb ->> 'revision')::bigint, 1) + 1
        )
    )::text,
    updated_at = clock_timestamp()
WHERE key = 'third_party_prompt_audit_config';
