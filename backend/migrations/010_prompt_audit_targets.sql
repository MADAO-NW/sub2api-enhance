CREATE TABLE sub2api_enhance.third_party_prompt_audit_targets (
    target_hash TEXT PRIMARY KEY,
    target_kind TEXT NOT NULL CHECK (target_kind IN ('current_user', 'instruction_context', 'effective_behavior')),
    protocol TEXT NOT NULL,
    contract_version TEXT NOT NULL,
    target_body TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX tppa_targets_kind_created_idx
    ON sub2api_enhance.third_party_prompt_audit_targets (target_kind, created_at DESC);
