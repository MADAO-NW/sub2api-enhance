ALTER TABLE sub2api_enhance.third_party_prompt_audit_model_attempts
    DROP CONSTRAINT IF EXISTS third_party_prompt_audit_model_attempts_stage_check;

ALTER TABLE sub2api_enhance.third_party_prompt_audit_model_attempts
    ADD CONSTRAINT third_party_prompt_audit_model_attempts_stage_check
        CHECK (stage IN ('segment', 'joint', 'format_repair', 'probe',
                         'current_user', 'instruction_context', 'intent_binding', 'effective_behavior', 'health_probe'));

ALTER TABLE sub2api_enhance.third_party_prompt_audit_segment_results
    DROP CONSTRAINT IF EXISTS third_party_prompt_audit_segment_results_target_kind_check;

ALTER TABLE sub2api_enhance.third_party_prompt_audit_segment_results
    ADD CONSTRAINT tppa_segment_results_target_kind_check
        CHECK (target_kind IN ('legacy_segment', 'current_user', 'instruction_context', 'intent_binding', 'effective_behavior'));

ALTER TABLE sub2api_enhance.third_party_prompt_audit_outcome_segments
    DROP CONSTRAINT IF EXISTS third_party_prompt_audit_outcome_segments_target_kind_check;

ALTER TABLE sub2api_enhance.third_party_prompt_audit_outcome_segments
    ADD CONSTRAINT tppa_outcome_segments_target_kind_check
        CHECK (target_kind IN ('legacy_segment', 'current_user', 'instruction_context', 'intent_binding', 'effective_behavior'));
