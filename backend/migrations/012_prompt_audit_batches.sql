CREATE TABLE sub2api_enhance.third_party_prompt_audit_batches (
    id BIGSERIAL PRIMARY KEY,
    batch_type TEXT NOT NULL CHECK (batch_type IN ('pending_review','failed_recovery','reaudit_selected','reaudit_filter')),
    requested_by BIGINT NOT NULL,
    request_hash TEXT NOT NULL,
    request_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,
    status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','processing','completed','failed')),
    cursor BIGINT NOT NULL DEFAULT 0,
    matched_count BIGINT NOT NULL DEFAULT 0,
    ready_count BIGINT NOT NULL DEFAULT 0,
    processed_count BIGINT NOT NULL DEFAULT 0,
    created_count BIGINT NOT NULL DEFAULT 0,
    requeued_count BIGINT NOT NULL DEFAULT 0,
    resumed_count BIGINT NOT NULL DEFAULT 0,
    skipped_count BIGINT NOT NULL DEFAULT 0,
    failed_count BIGINT NOT NULL DEFAULT 0,
    last_error TEXT,
    lease_until TIMESTAMPTZ,
    claim_generation BIGINT NOT NULL DEFAULT 0,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE UNIQUE INDEX third_party_prompt_audit_batches_active_request_uniq
    ON sub2api_enhance.third_party_prompt_audit_batches(requested_by, batch_type, request_hash)
    WHERE status IN ('queued','processing');
CREATE INDEX third_party_prompt_audit_batches_queue_idx
    ON sub2api_enhance.third_party_prompt_audit_batches(status, id);
CREATE INDEX third_party_prompt_audit_batches_lease_idx
    ON sub2api_enhance.third_party_prompt_audit_batches(status, lease_until, id);

CREATE TABLE sub2api_enhance.third_party_prompt_audit_batch_items (
    id BIGSERIAL PRIMARY KEY,
    batch_id BIGINT NOT NULL REFERENCES sub2api_enhance.third_party_prompt_audit_batches(id) ON DELETE CASCADE,
    capture_id BIGINT,
    job_id BIGINT,
    source_audit_round INTEGER,
    status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','ready','processing','created','requeued','resumed','skipped','failed','already_running')),
    reason TEXT,
    result_job_id BIGINT,
    processed_at TIMESTAMPTZ,
    CHECK ((capture_id IS NOT NULL) <> (job_id IS NOT NULL))
);
CREATE UNIQUE INDEX third_party_prompt_audit_batch_items_capture_uniq
    ON sub2api_enhance.third_party_prompt_audit_batch_items(batch_id, capture_id)
    WHERE capture_id IS NOT NULL;
CREATE UNIQUE INDEX third_party_prompt_audit_batch_items_job_uniq
    ON sub2api_enhance.third_party_prompt_audit_batch_items(batch_id, job_id)
    WHERE job_id IS NOT NULL;
CREATE INDEX third_party_prompt_audit_batch_items_queue_idx
    ON sub2api_enhance.third_party_prompt_audit_batch_items(batch_id, status, id);
