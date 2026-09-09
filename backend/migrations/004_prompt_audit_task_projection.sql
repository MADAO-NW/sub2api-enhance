ALTER TABLE sub2api_enhance.third_party_prompt_audit_jobs
    ADD COLUMN disable_counted BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN reuse_mode TEXT NOT NULL DEFAULT 'allow'
        CHECK (reuse_mode IN ('allow', 'force'));

ALTER TABLE sub2api_enhance.third_party_prompt_audit_outcomes
    ADD COLUMN reuse_mode TEXT NOT NULL DEFAULT 'allow'
        CHECK (reuse_mode IN ('allow', 'force')),
    ADD COLUMN target_hash TEXT,
    ADD COLUMN evaluation_hash TEXT;

UPDATE sub2api_enhance.third_party_prompt_audit_outcomes outcome
SET target_hash = job.target_hash,
    evaluation_hash = job.evaluation_hash
FROM sub2api_enhance.third_party_prompt_audit_jobs job
WHERE job.id = outcome.job_id
  AND job.status = 'done'
  AND outcome.audit_round = job.audit_round
  AND outcome.id = (
      SELECT latest.id
      FROM sub2api_enhance.third_party_prompt_audit_outcomes latest
      WHERE latest.job_id = job.id
      ORDER BY latest.audit_round DESC, latest.id DESC
      LIMIT 1
  );

DROP INDEX IF EXISTS sub2api_enhance.tppa_jobs_evaluation_idx;
CREATE INDEX tppa_outcomes_reuse_idx
    ON sub2api_enhance.third_party_prompt_audit_outcomes (evaluation_hash, target_hash, id DESC)
    WHERE evaluation_hash IS NOT NULL
      AND target_hash IS NOT NULL
      AND NOT partial_failure;

DROP INDEX sub2api_enhance.tppa_segments_lookup_idx;
CREATE INDEX tppa_segments_lookup_idx
    ON sub2api_enhance.third_party_prompt_audit_segment_results (model_id, audit_key, id DESC);

UPDATE sub2api_enhance.third_party_prompt_audit_jobs root
SET disable_counted = event.disable_counted
FROM sub2api_enhance.third_party_prompt_audit_events event
WHERE event.job_id = root.id;

CREATE TEMP TABLE tppa_legacy_reaudit_jobs ON COMMIT DROP AS
SELECT id
FROM sub2api_enhance.third_party_prompt_audit_jobs
WHERE run_kind = 'reaudit' OR source_job_id IS NOT NULL;

CREATE TEMP TABLE tppa_legacy_reaudit_outcomes ON COMMIT DROP AS
SELECT outcome.id
FROM sub2api_enhance.third_party_prompt_audit_outcomes outcome
JOIN tppa_legacy_reaudit_jobs legacy ON legacy.id = outcome.job_id;

CREATE TEMP TABLE tppa_legacy_reaudit_attempts ON COMMIT DROP AS
SELECT attempt.id
FROM sub2api_enhance.third_party_prompt_audit_model_attempts attempt
JOIN tppa_legacy_reaudit_jobs legacy ON legacy.id = attempt.job_id;

CREATE TEMP TABLE tppa_legacy_reaudit_segments ON COMMIT DROP AS
SELECT segment.id
FROM sub2api_enhance.third_party_prompt_audit_segment_results segment
JOIN tppa_legacy_reaudit_attempts attempt ON attempt.id = segment.source_attempt_id;

ALTER TABLE sub2api_enhance.third_party_prompt_audit_jobs
    DROP CONSTRAINT IF EXISTS third_party_prompt_audit_jobs_source_job_id_fkey,
    DROP CONSTRAINT tppa_jobs_input_owner,
    DROP CONSTRAINT tppa_jobs_source_not_self,
    DROP CONSTRAINT tppa_jobs_capture_owner;

DROP TABLE sub2api_enhance.third_party_prompt_audit_events;

UPDATE sub2api_enhance.third_party_prompt_audit_enforcement_states state
SET last_outcome_id = (
    SELECT MAX(outcome.id)
    FROM sub2api_enhance.third_party_prompt_audit_outcomes outcome
    JOIN sub2api_enhance.third_party_prompt_audit_jobs job ON job.id = outcome.job_id
    WHERE outcome.user_id = state.user_id
      AND NOT EXISTS (SELECT 1 FROM tppa_legacy_reaudit_jobs legacy WHERE legacy.id = job.id)
)
WHERE state.last_outcome_id IN (SELECT id FROM tppa_legacy_reaudit_outcomes);

DELETE FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions
WHERE outcome_id IN (SELECT id FROM tppa_legacy_reaudit_outcomes);

UPDATE sub2api_enhance.third_party_prompt_audit_outcomes
SET source_outcome_id = NULL
WHERE source_outcome_id IN (SELECT id FROM tppa_legacy_reaudit_outcomes);

UPDATE sub2api_enhance.third_party_prompt_audit_model_attempts
SET repair_of_attempt_id = NULL
WHERE repair_of_attempt_id IN (SELECT id FROM tppa_legacy_reaudit_attempts);

DELETE FROM sub2api_enhance.third_party_prompt_audit_outcome_segments
WHERE outcome_id IN (SELECT id FROM tppa_legacy_reaudit_outcomes)
   OR segment_result_id IN (SELECT id FROM tppa_legacy_reaudit_segments);

DELETE FROM sub2api_enhance.third_party_prompt_audit_segment_results
WHERE id IN (SELECT id FROM tppa_legacy_reaudit_segments);

DELETE FROM sub2api_enhance.third_party_prompt_audit_outcomes
WHERE id IN (SELECT id FROM tppa_legacy_reaudit_outcomes);

DELETE FROM sub2api_enhance.third_party_prompt_audit_model_attempts
WHERE id IN (SELECT id FROM tppa_legacy_reaudit_attempts);

DELETE FROM sub2api_enhance.third_party_prompt_audit_jobs
WHERE id IN (SELECT id FROM tppa_legacy_reaudit_jobs);

DROP INDEX IF EXISTS sub2api_enhance.tppa_jobs_source_idx;
DROP INDEX IF EXISTS sub2api_enhance.tppa_jobs_active_reaudit_uniq;

ALTER TABLE sub2api_enhance.third_party_prompt_audit_jobs
    DROP COLUMN run_kind,
    DROP COLUMN source_job_id,
    DROP COLUMN requested_by,
    ALTER COLUMN full_input_snapshot SET NOT NULL;
