ALTER TABLE sub2api_enhance.third_party_prompt_audit_jobs
    ADD COLUMN IF NOT EXISTS reaudit_batch_id TEXT;

ALTER TABLE sub2api_enhance.third_party_prompt_audit_outcome_segments
    DROP CONSTRAINT IF EXISTS third_party_prompt_audit_outcome_segments_reuse_kind_check;

ALTER TABLE sub2api_enhance.third_party_prompt_audit_outcome_segments
    ADD CONSTRAINT third_party_prompt_audit_outcome_segments_reuse_kind_check
        CHECK (reuse_kind IN ('fresh', 'within_job', 'history', 'full_evaluation', 'inflight', 'force_batch'));
