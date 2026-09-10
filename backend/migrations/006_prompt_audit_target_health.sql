ALTER TABLE sub2api_enhance.third_party_prompt_audit_model_attempts
    DROP CONSTRAINT IF EXISTS third_party_prompt_audit_model_attempts_call_kind_check,
    DROP CONSTRAINT IF EXISTS third_party_prompt_audit_model_attempts_stage_check,
    DROP CONSTRAINT IF EXISTS tppa_attempts_owner;

ALTER TABLE sub2api_enhance.third_party_prompt_audit_model_attempts
    ADD CONSTRAINT third_party_prompt_audit_model_attempts_call_kind_check
        CHECK (call_kind IN ('audit', 'probe', 'health_probe')),
    ADD CONSTRAINT third_party_prompt_audit_model_attempts_stage_check
        CHECK (stage IN ('segment', 'joint', 'format_repair', 'probe',
                         'current_user', 'instruction_context', 'intent_binding', 'health_probe')),
    ADD CONSTRAINT tppa_attempts_owner CHECK (
        (call_kind = 'audit' AND job_id IS NOT NULL AND evaluation_round IS NOT NULL)
        OR (call_kind IN ('probe', 'health_probe') AND job_id IS NULL)
    );

ALTER TABLE sub2api_enhance.third_party_prompt_audit_segment_results
    ADD COLUMN target_kind TEXT NOT NULL DEFAULT 'legacy_segment'
        CHECK (target_kind IN ('legacy_segment', 'current_user', 'instruction_context', 'intent_binding'));

ALTER TABLE sub2api_enhance.third_party_prompt_audit_outcome_segments
    ADD COLUMN target_kind TEXT NOT NULL DEFAULT 'legacy_segment'
        CHECK (target_kind IN ('legacy_segment', 'current_user', 'instruction_context', 'intent_binding'));
