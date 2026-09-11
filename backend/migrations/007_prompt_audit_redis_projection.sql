ALTER TABLE sub2api_enhance.third_party_prompt_audit_model_attempts
    ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp();

UPDATE sub2api_enhance.third_party_prompt_audit_model_attempts
SET updated_at = COALESCE(finished_at, dispatch_started_at, created_at);

CREATE INDEX enhance_captures_updated_idx
    ON sub2api_enhance.captures (updated_at, id);

CREATE INDEX tppa_jobs_updated_idx
    ON sub2api_enhance.third_party_prompt_audit_jobs (updated_at, id);

CREATE INDEX tppa_actions_updated_idx
    ON sub2api_enhance.third_party_prompt_audit_enforcement_actions (updated_at, id);

CREATE INDEX tppa_attempts_updated_idx
    ON sub2api_enhance.third_party_prompt_audit_model_attempts (updated_at, id);
