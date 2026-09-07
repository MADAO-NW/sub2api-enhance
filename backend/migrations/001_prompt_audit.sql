CREATE SCHEMA IF NOT EXISTS sub2api_enhance;


CREATE TABLE sub2api_enhance.settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE sub2api_enhance.captures (
    id BIGSERIAL PRIMARY KEY,
    capture_key TEXT NOT NULL UNIQUE,
    transport TEXT NOT NULL CHECK (transport IN ('http', 'websocket')),
    connection_key TEXT,
    message_sequence BIGINT CHECK (message_sequence > 0),
    protocol TEXT NOT NULL,
    body_format TEXT NOT NULL CHECK (body_format IN ('entity_bytes', 'websocket_message', 'multipart_text_fields')),
    raw_body BYTEA NOT NULL,
    body_sha256 TEXT NOT NULL,
    body_bytes BIGINT NOT NULL CHECK (body_bytes >= 0),
    snapshot_status TEXT NOT NULL CHECK (snapshot_status IN ('complete', 'incomplete')),
    request_metadata TEXT NOT NULL,
    identity_snapshot TEXT,
    identity_resolved_at TIMESTAMPTZ,
    user_id BIGINT,
    api_key_id BIGINT,
    group_id BIGINT,
    eligibility_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (eligibility_status IN ('pending', 'passed', 'rejected', 'unknown')),
    processing_status TEXT NOT NULL DEFAULT 'queued'
        CHECK (processing_status IN ('queued', 'processing', 'retry', 'done', 'failed', 'skipped')),
    processing_attempts INTEGER NOT NULL DEFAULT 0 CHECK (processing_attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    claim_generation BIGINT NOT NULL DEFAULT 0 CHECK (claim_generation >= 0),
    lease_until TIMESTAMPTZ,
    last_error_code TEXT,
    last_error_message TEXT,
    forwarding_status TEXT NOT NULL DEFAULT 'not_forwarded'
        CHECK (forwarding_status IN ('not_forwarded', 'started', 'response_started', 'complete', 'failed', 'unknown', 'blocked')),
    forwarding_observations TEXT NOT NULL DEFAULT '[]',
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT enhance_capture_byte_count CHECK (body_bytes = octet_length(raw_body)),
    CONSTRAINT enhance_capture_ws_identity CHECK (
        (transport = 'http' AND connection_key IS NULL AND message_sequence IS NULL)
        OR (transport = 'websocket' AND connection_key IS NOT NULL AND message_sequence IS NOT NULL)
    ),
    UNIQUE (connection_key, message_sequence)
);

CREATE INDEX enhance_captures_created_idx ON sub2api_enhance.captures (created_at DESC, id DESC);
CREATE INDEX enhance_captures_pending_idx ON sub2api_enhance.captures (next_attempt_at, id)
    WHERE processing_status IN ('queued', 'retry');
CREATE INDEX enhance_captures_lease_idx ON sub2api_enhance.captures (lease_until, id)
    WHERE processing_status = 'processing';

CREATE TABLE sub2api_enhance.third_party_prompt_audit_jobs (
    id BIGSERIAL PRIMARY KEY,
    capture_key TEXT NOT NULL UNIQUE,
    run_kind TEXT NOT NULL CHECK (run_kind IN ('request', 'reaudit')),
    source_job_id BIGINT REFERENCES sub2api_enhance.third_party_prompt_audit_jobs(id) ON DELETE RESTRICT,
    capture_id BIGINT UNIQUE REFERENCES sub2api_enhance.captures(id) ON DELETE RESTRICT,
    requested_by BIGINT,
    user_id BIGINT NOT NULL,
    api_key_id BIGINT,
    group_id BIGINT,
    request_id TEXT NOT NULL,
    identity_snapshot TEXT NOT NULL,
    platform TEXT NOT NULL,
    protocol TEXT NOT NULL,
    ingress_stage TEXT NOT NULL,
    requested_model TEXT NOT NULL,
    execution_mode TEXT NOT NULL CHECK (execution_mode IN ('async', 'blocking')),
    config_revision BIGINT NOT NULL,
    config_snapshot TEXT NOT NULL,
    full_input_snapshot TEXT,
    snapshot_status TEXT NOT NULL CHECK (snapshot_status IN ('complete', 'incomplete')),
    input_manifest TEXT,
    input_hash TEXT,
    target_hash TEXT,
    evaluation_hash TEXT,
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'processing', 'retry', 'done', 'failed', 'skipped')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts INTEGER NOT NULL DEFAULT 3 CHECK (max_attempts > 0),
    claim_generation BIGINT NOT NULL DEFAULT 0 CHECK (claim_generation >= 0),
    lease_until TIMESTAMPTZ,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    result_checkpoint TEXT,
    reuse_metrics TEXT NOT NULL DEFAULT
        '{"whole_lookups":0,"whole_hits":0,"segment_lookups":0,"segment_hits":0,"within_job_hits":0}',
    failure_stage TEXT,
    last_error_code TEXT,
    last_error_message TEXT,
    gateway_result TEXT NOT NULL DEFAULT 'not_observed'
        CHECK (gateway_result IN ('continued', 'blocked_here', 'blocked_elsewhere', 'unavailable', 'not_observed')),
    gateway_completed_at TIMESTAMPTZ,
    gateway_duration_ms BIGINT CHECK (gateway_duration_ms >= 0),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT tppa_jobs_input_owner CHECK (
        (run_kind = 'request' AND source_job_id IS NULL AND full_input_snapshot IS NOT NULL)
        OR
        (run_kind = 'reaudit' AND source_job_id IS NOT NULL AND full_input_snapshot IS NULL)
    ),
    CONSTRAINT tppa_jobs_source_not_self CHECK (source_job_id IS NULL OR source_job_id <> id),
    CONSTRAINT tppa_jobs_capture_owner CHECK (run_kind = 'request' OR capture_id IS NULL)
);

CREATE INDEX tppa_jobs_queue_idx
    ON sub2api_enhance.third_party_prompt_audit_jobs (next_attempt_at, id)
    WHERE status IN ('queued', 'retry');
CREATE INDEX tppa_jobs_lease_idx
    ON sub2api_enhance.third_party_prompt_audit_jobs (lease_until, id)
    WHERE status = 'processing';
CREATE INDEX tppa_jobs_created_idx
    ON sub2api_enhance.third_party_prompt_audit_jobs (created_at DESC, id DESC);
CREATE INDEX tppa_jobs_user_created_idx
    ON sub2api_enhance.third_party_prompt_audit_jobs (user_id, created_at DESC, id DESC);
CREATE INDEX tppa_jobs_evaluation_idx
    ON sub2api_enhance.third_party_prompt_audit_jobs (user_id, evaluation_hash, target_hash, id DESC)
    WHERE evaluation_hash IS NOT NULL AND target_hash IS NOT NULL;
CREATE INDEX tppa_jobs_source_idx
    ON sub2api_enhance.third_party_prompt_audit_jobs (source_job_id, id DESC)
    WHERE source_job_id IS NOT NULL;
CREATE UNIQUE INDEX tppa_jobs_active_reaudit_uniq
    ON sub2api_enhance.third_party_prompt_audit_jobs (source_job_id)
    WHERE run_kind = 'reaudit' AND status IN ('queued', 'processing', 'retry');

CREATE TABLE sub2api_enhance.third_party_prompt_audit_model_attempts (
    id BIGSERIAL PRIMARY KEY,
    job_id BIGINT REFERENCES sub2api_enhance.third_party_prompt_audit_jobs(id) ON DELETE RESTRICT,
    call_kind TEXT NOT NULL CHECK (call_kind IN ('audit', 'probe')),
    evaluation_round INTEGER CHECK (evaluation_round > 0),
    model_id TEXT NOT NULL,
    model_snapshot TEXT NOT NULL,
    stage TEXT NOT NULL CHECK (stage IN ('segment', 'joint', 'format_repair', 'probe')),
    segment_order INTEGER CHECK (segment_order > 0),
    repair_of_attempt_id BIGINT REFERENCES sub2api_enhance.third_party_prompt_audit_model_attempts(id) ON DELETE RESTRICT,
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
    CONSTRAINT tppa_attempts_owner CHECK (
        (call_kind = 'audit' AND job_id IS NOT NULL AND evaluation_round IS NOT NULL)
        OR (call_kind = 'probe' AND job_id IS NULL)
    ),
    CONSTRAINT tppa_attempts_valid_result CHECK (
        status <> 'succeeded' OR (confidence IS NOT NULL AND reason IS NOT NULL)
    )
);

CREATE INDEX tppa_attempts_job_idx
    ON sub2api_enhance.third_party_prompt_audit_model_attempts (job_id, id);
CREATE INDEX tppa_attempts_dispatch_idx
    ON sub2api_enhance.third_party_prompt_audit_model_attempts (dispatch_started_at DESC, id DESC)
    WHERE dispatch_started_at IS NOT NULL;
CREATE INDEX tppa_attempts_model_dispatch_idx
    ON sub2api_enhance.third_party_prompt_audit_model_attempts (model_id, dispatch_started_at DESC, id DESC)
    WHERE dispatch_started_at IS NOT NULL;

CREATE TABLE sub2api_enhance.third_party_prompt_audit_segment_results (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL,
    model_id TEXT NOT NULL,
    audit_key TEXT NOT NULL,
    source_attempt_id BIGINT NOT NULL UNIQUE
        REFERENCES sub2api_enhance.third_party_prompt_audit_model_attempts(id) ON DELETE RESTRICT,
    source_role TEXT NOT NULL,
    policy_role TEXT NOT NULL,
    turn_scope TEXT NOT NULL CHECK (turn_scope IN ('active', 'current', 'historical')),
    content_hash TEXT NOT NULL,
    confidence DOUBLE PRECISION NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    reason TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX tppa_segments_lookup_idx
    ON sub2api_enhance.third_party_prompt_audit_segment_results (user_id, model_id, audit_key, id DESC);

CREATE TABLE sub2api_enhance.third_party_prompt_audit_outcomes (
    id BIGSERIAL PRIMARY KEY,
    job_id BIGINT NOT NULL UNIQUE
        REFERENCES sub2api_enhance.third_party_prompt_audit_jobs(id) ON DELETE RESTRICT,
    user_id BIGINT NOT NULL,
    decision TEXT NOT NULL CHECK (decision IN ('pass', 'review', 'block')),
    partial_failure BOOLEAN NOT NULL DEFAULT FALSE,
    enforcement_eligible BOOLEAN NOT NULL DEFAULT FALSE,
    model_results TEXT NOT NULL,
    source_outcome_id BIGINT REFERENCES sub2api_enhance.third_party_prompt_audit_outcomes(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT tppa_outcomes_source_not_self CHECK (source_outcome_id IS NULL OR source_outcome_id <> id)
);

CREATE INDEX tppa_outcomes_created_idx
    ON sub2api_enhance.third_party_prompt_audit_outcomes (created_at DESC, id DESC);
CREATE INDEX tppa_outcomes_user_idx
    ON sub2api_enhance.third_party_prompt_audit_outcomes (user_id, id DESC);

CREATE TABLE sub2api_enhance.third_party_prompt_audit_events (
    id BIGSERIAL PRIMARY KEY,
    job_id BIGINT NOT NULL UNIQUE
        REFERENCES sub2api_enhance.third_party_prompt_audit_jobs(id) ON DELETE RESTRICT,
    original_outcome_id BIGINT REFERENCES sub2api_enhance.third_party_prompt_audit_outcomes(id) ON DELETE RESTRICT,
    latest_outcome_id BIGINT NOT NULL REFERENCES sub2api_enhance.third_party_prompt_audit_outcomes(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX tppa_events_created_idx
    ON sub2api_enhance.third_party_prompt_audit_events (created_at DESC, id DESC);

CREATE TABLE sub2api_enhance.third_party_prompt_audit_outcome_segments (
    outcome_id BIGINT NOT NULL REFERENCES sub2api_enhance.third_party_prompt_audit_outcomes(id) ON DELETE RESTRICT,
    model_id TEXT NOT NULL,
    segment_order INTEGER NOT NULL CHECK (segment_order > 0),
    segment_result_id BIGINT NOT NULL REFERENCES sub2api_enhance.third_party_prompt_audit_segment_results(id) ON DELETE RESTRICT,
    reuse_kind TEXT NOT NULL CHECK (reuse_kind IN ('fresh', 'within_job', 'history', 'full_evaluation')),
    PRIMARY KEY (outcome_id, model_id, segment_order)
);

CREATE INDEX tppa_outcome_segments_source_idx
    ON sub2api_enhance.third_party_prompt_audit_outcome_segments (segment_result_id);

CREATE TABLE sub2api_enhance.third_party_prompt_audit_enforcement_states (
    user_id BIGINT PRIMARY KEY,
    warning_rule_hash TEXT NOT NULL DEFAULT '',
    warning_window_after_outcome_id BIGINT NOT NULL DEFAULT 0,
    warning_armed BOOLEAN NOT NULL DEFAULT TRUE,
    disable_violation_count BIGINT NOT NULL DEFAULT 0 CHECK (disable_violation_count >= 0),
    disable_reset_at TIMESTAMPTZ,
    last_outcome_id BIGINT REFERENCES sub2api_enhance.third_party_prompt_audit_outcomes(id) ON DELETE RESTRICT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE sub2api_enhance.third_party_prompt_audit_enforcement_actions (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL,
    outcome_id BIGINT REFERENCES sub2api_enhance.third_party_prompt_audit_outcomes(id) ON DELETE RESTRICT,
    actor_user_id BIGINT,
    action_type TEXT NOT NULL CHECK (action_type IN ('warning', 'disable', 'counter_reset')),
    rule_snapshot TEXT NOT NULL,
    business_snapshot TEXT NOT NULL,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    execution_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (execution_status IN ('pending', 'processing', 'succeeded', 'failed', 'unknown', 'cancelled')),
    attempt_history TEXT NOT NULL DEFAULT '[]',
    applied_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    notification_status TEXT NOT NULL DEFAULT 'not_required'
        CHECK (notification_status IN ('not_required', 'pending', 'processing', 'retry', 'sent', 'failed')),
    deliveries TEXT NOT NULL DEFAULT '[]',
    auth_cache_status TEXT NOT NULL DEFAULT 'not_required'
        CHECK (auth_cache_status IN ('not_required', 'not_observed', 'pending', 'done', 'retry', 'failed')),
    auth_cache_error TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    claim_generation BIGINT NOT NULL DEFAULT 0 CHECK (claim_generation >= 0),
    lease_until TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (outcome_id, action_type),
    CONSTRAINT tppa_actions_confirmed CHECK (execution_status <> 'succeeded' OR applied_at IS NOT NULL),
    CONSTRAINT tppa_actions_source CHECK (
        (action_type = 'counter_reset' AND outcome_id IS NULL)
        OR (action_type IN ('warning', 'disable') AND outcome_id IS NOT NULL)
    )
);

CREATE INDEX tppa_actions_user_idx
    ON sub2api_enhance.third_party_prompt_audit_enforcement_actions (user_id, applied_at DESC, id DESC);
CREATE INDEX tppa_actions_outbox_idx
    ON sub2api_enhance.third_party_prompt_audit_enforcement_actions (next_attempt_at, id)
    WHERE notification_status IN ('pending', 'processing', 'retry')
        OR execution_status IN ('pending', 'processing', 'unknown');
