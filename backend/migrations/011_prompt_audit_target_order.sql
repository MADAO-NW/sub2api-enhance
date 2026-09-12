ALTER TABLE sub2api_enhance.third_party_prompt_audit_targets
    ADD COLUMN target_order INTEGER NOT NULL DEFAULT 1;

DROP INDEX IF EXISTS sub2api_enhance.tppa_targets_kind_created_idx;
CREATE INDEX tppa_targets_kind_created_idx
    ON sub2api_enhance.third_party_prompt_audit_targets (target_kind, target_order, created_at DESC);

CREATE TABLE sub2api_enhance.third_party_prompt_audit_job_targets (
    job_id BIGINT NOT NULL REFERENCES sub2api_enhance.third_party_prompt_audit_jobs(id) ON DELETE CASCADE,
    target_hash TEXT NOT NULL REFERENCES sub2api_enhance.third_party_prompt_audit_targets(target_hash) ON DELETE RESTRICT,
    target_kind TEXT NOT NULL,
    target_order INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (job_id, target_kind, target_order)
);
