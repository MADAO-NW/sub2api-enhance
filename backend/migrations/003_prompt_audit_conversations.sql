ALTER TABLE sub2api_enhance.captures
    ADD COLUMN conversation_key TEXT;

ALTER TABLE sub2api_enhance.third_party_prompt_audit_jobs
    ADD COLUMN conversation_key TEXT,
    ADD COLUMN audit_round INTEGER NOT NULL DEFAULT 1 CHECK (audit_round > 0),
    ADD COLUMN current_run_kind TEXT NOT NULL DEFAULT 'request' CHECK (current_run_kind IN ('request', 'reaudit')),
    ADD COLUMN current_requested_by BIGINT;

UPDATE sub2api_enhance.third_party_prompt_audit_jobs
SET current_run_kind = run_kind,
    current_requested_by = requested_by;

ALTER TABLE sub2api_enhance.third_party_prompt_audit_jobs
    ALTER COLUMN reuse_metrics SET DEFAULT
    '{"whole_lookups":0,"whole_hits":0,"segment_lookups":0,"segment_hits":0,"within_job_hits":0,"inflight_hits":0,"short_circuited_nodes":0}';

DROP INDEX sub2api_enhance.tppa_jobs_evaluation_idx;
CREATE INDEX tppa_jobs_evaluation_idx
    ON sub2api_enhance.third_party_prompt_audit_jobs
       (user_id, conversation_key, evaluation_hash, target_hash, id DESC)
    WHERE conversation_key IS NOT NULL
      AND evaluation_hash IS NOT NULL
      AND target_hash IS NOT NULL;

ALTER TABLE sub2api_enhance.third_party_prompt_audit_outcome_segments
    DROP CONSTRAINT third_party_prompt_audit_outcome_segments_reuse_kind_check;
ALTER TABLE sub2api_enhance.third_party_prompt_audit_outcome_segments
    ADD CONSTRAINT third_party_prompt_audit_outcome_segments_reuse_kind_check
    CHECK (reuse_kind IN ('fresh', 'within_job', 'history', 'full_evaluation', 'inflight'));

ALTER TABLE sub2api_enhance.third_party_prompt_audit_model_attempts
    ADD COLUMN audit_round INTEGER NOT NULL DEFAULT 1 CHECK (audit_round > 0);

ALTER TABLE sub2api_enhance.third_party_prompt_audit_outcomes
    ADD COLUMN audit_round INTEGER NOT NULL DEFAULT 1 CHECK (audit_round > 0),
    ADD COLUMN run_kind TEXT NOT NULL DEFAULT 'request' CHECK (run_kind IN ('request', 'reaudit')),
    ADD COLUMN requested_by BIGINT,
    ADD COLUMN config_snapshot TEXT NOT NULL DEFAULT '{}',
    ADD COLUMN started_at TIMESTAMPTZ,
    ADD COLUMN finished_at TIMESTAMPTZ;

UPDATE sub2api_enhance.third_party_prompt_audit_outcomes o
SET run_kind = j.run_kind,
    requested_by = j.requested_by,
    config_snapshot = j.config_snapshot,
    started_at = j.started_at,
    finished_at = COALESCE(j.finished_at, o.created_at)
FROM sub2api_enhance.third_party_prompt_audit_jobs j
WHERE j.id = o.job_id;

ALTER TABLE sub2api_enhance.third_party_prompt_audit_outcomes
    DROP CONSTRAINT third_party_prompt_audit_outcomes_job_id_key;
ALTER TABLE sub2api_enhance.third_party_prompt_audit_outcomes
    ADD CONSTRAINT tppa_outcomes_job_round_uniq UNIQUE (job_id, audit_round);

ALTER TABLE sub2api_enhance.third_party_prompt_audit_events
    ADD COLUMN disable_counted BOOLEAN NOT NULL DEFAULT FALSE;

WITH candidates AS (
    SELECT event.id,
           row_number() OVER (PARTITION BY root.user_id ORDER BY original.id DESC) AS position,
           state.disable_violation_count
    FROM sub2api_enhance.third_party_prompt_audit_events event
    JOIN sub2api_enhance.third_party_prompt_audit_jobs root ON root.id = event.job_id
    JOIN sub2api_enhance.third_party_prompt_audit_outcomes original ON original.id = event.original_outcome_id
    JOIN sub2api_enhance.third_party_prompt_audit_outcomes latest ON latest.id = event.latest_outcome_id
    JOIN sub2api_enhance.third_party_prompt_audit_enforcement_states state ON state.user_id = root.user_id
    LEFT JOIN sub2api_enhance.captures capture ON capture.id = root.capture_id
    WHERE latest.decision = 'block'
      AND original.enforcement_eligible
      AND COALESCE(capture.created_at, root.created_at) > COALESCE(state.disable_reset_at, '-infinity'::timestamptz)
)
UPDATE sub2api_enhance.third_party_prompt_audit_events event
SET disable_counted = TRUE
FROM candidates
WHERE candidates.id = event.id
  AND candidates.position <= candidates.disable_violation_count;

ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_actions
    DROP CONSTRAINT third_party_prompt_audit_enforcement_actions_action_type_check;
ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_actions
    ADD CONSTRAINT third_party_prompt_audit_enforcement_actions_action_type_check
    CHECK (action_type IN ('warning', 'disable', 'counter_reset', 'blocking_notice'));

ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_actions
    DROP CONSTRAINT tppa_actions_source;
ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_actions
    ADD CONSTRAINT tppa_actions_source CHECK (
        (action_type = 'counter_reset' AND outcome_id IS NULL)
        OR (action_type IN ('warning', 'disable', 'blocking_notice') AND outcome_id IS NOT NULL)
    );
