package migrations

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"testing/fstest"
)

func TestExistingMigrationMustHaveIdenticalChecksum(t *testing.T) {
	for _, matches := range []bool{true, false} {
		t.Run(fmt.Sprint(matches), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			db.SetMaxOpenConns(1)
			mock.ExpectQuery("SELECT pg_try_advisory_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
			mock.ExpectExec("CREATE SCHEMA").WillReturnResult(sqlmock.NewResult(0, 0))
			entries, err := files.ReadDir(".")
			require.NoError(t, err)
			for _, e := range entries {
				raw, err := files.ReadFile(e.Name())
				require.NoError(t, err)
				checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(string(raw)))))
				if !matches {
					checksum = "changed"
				}
				mock.ExpectQuery("SELECT checksum").WithArgs(e.Name()).WillReturnRows(sqlmock.NewRows([]string{"checksum"}).AddRow(checksum))
				if !matches {
					break
				}
			}
			mock.ExpectQuery("SELECT pg_advisory_unlock").WillReturnRows(sqlmock.NewRows([]string{"released"}).AddRow(true))
			err = Run(context.Background(), db)
			if matches {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "迁移内容发生变化")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
func TestLaterMigrationFailurePreservesEarlierCommitAndRollsBackOnlyFailedFile(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	mock.ExpectQuery("SELECT pg_try_advisory_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectExec("CREATE SCHEMA").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT checksum").WithArgs("001.sql").WillReturnRows(sqlmock.NewRows([]string{"checksum"}))
	mock.ExpectBegin()
	mock.ExpectExec("CREATE TABLE first").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO .*schema_migrations").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT checksum").WithArgs("002.sql").WillReturnRows(sqlmock.NewRows([]string{"checksum"}))
	mock.ExpectBegin()
	mock.ExpectExec("CREATE TABLE second").WillReturnError(errors.New("permission denied"))
	mock.ExpectRollback()
	mock.ExpectQuery("SELECT pg_advisory_unlock").WillReturnRows(sqlmock.NewRows([]string{"released"}).AddRow(true))
	err = runFS(context.Background(), db, fstest.MapFS{"001.sql": &fstest.MapFile{Data: []byte("CREATE TABLE first(id int);")}, "002.sql": &fstest.MapFile{Data: []byte("CREATE TABLE second(id int);")}})
	require.ErrorContains(t, err, "002.sql")
	require.NoError(t, mock.ExpectationsWereMet())
}
func TestNonTransactionalMigrationMustBeReplayableConcurrentIndexOnly(t *testing.T) {
	ok, err := validateMigrationExecutionMode("003_notx.sql", "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx ON sub2api_enhance.jobs(id);")
	require.NoError(t, err)
	require.True(t, ok)
	for _, sql := range []string{"BEGIN; CREATE INDEX CONCURRENTLY idx ON t(id);", "UPDATE t SET id=1;", "CREATE INDEX CONCURRENTLY idx ON t(id);"} {
		_, err := validateMigrationExecutionMode("003_notx.sql", sql)
		require.Error(t, err)
	}
	_, err = validateMigrationExecutionMode("003.sql", "CREATE INDEX CONCURRENTLY idx ON t(id);")
	require.Error(t, err)
}

func TestTaskProjectionMigrationTransfersStateBeforeRemovingLegacyRows(t *testing.T) {
	raw, err := files.ReadFile("004_prompt_audit_task_projection.sql")
	require.NoError(t, err)
	sql := string(raw)
	for _, required := range []string{
		"ADD COLUMN disable_counted",
		"ADD COLUMN target_hash",
		"SET disable_counted = event.disable_counted",
		"CREATE INDEX tppa_outcomes_reuse_idx",
		"ON sub2api_enhance.third_party_prompt_audit_segment_results (model_id, audit_key, id DESC)",
		"CREATE TEMP TABLE tppa_legacy_reaudit_jobs",
		"UPDATE sub2api_enhance.third_party_prompt_audit_enforcement_states",
		"SET repair_of_attempt_id = NULL",
		"DELETE FROM sub2api_enhance.third_party_prompt_audit_jobs",
		"DROP TABLE sub2api_enhance.third_party_prompt_audit_events",
		"DROP COLUMN source_job_id",
	} {
		require.Contains(t, sql, required)
	}
	require.Less(t, strings.Index(sql, "SET disable_counted = event.disable_counted"), strings.Index(sql, "DROP TABLE sub2api_enhance.third_party_prompt_audit_events"))
	require.Less(t, strings.Index(sql, "UPDATE sub2api_enhance.third_party_prompt_audit_enforcement_states"), strings.Index(sql, "DELETE FROM sub2api_enhance.third_party_prompt_audit_jobs"))
	require.Less(t, strings.Index(sql, "SET repair_of_attempt_id = NULL"), strings.Index(sql, "DELETE FROM sub2api_enhance.third_party_prompt_audit_model_attempts"))
}

func TestCaptureReviewMigrationOnlyExtendsTheProcessingState(t *testing.T) {
	raw, err := files.ReadFile("005_prompt_audit_capture_review.sql")
	require.NoError(t, err)
	sql := string(raw)
	require.Contains(t, sql, "DROP CONSTRAINT captures_processing_status_check")
	require.Contains(t, sql, "'awaiting_review'")
	require.NotContains(t, strings.ToUpper(sql), "DELETE FROM")
	require.NotContains(t, strings.ToUpper(sql), "UPDATE ")
}

func TestTargetHealthMigrationPreservesLegacyEvidenceAndAddsNewKinds(t *testing.T) {
	raw, err := files.ReadFile("006_prompt_audit_target_health.sql")
	require.NoError(t, err)
	sql := string(raw)
	for _, required := range []string{
		"'health_probe'",
		"'current_user'",
		"'instruction_context'",
		"'intent_binding'",
		"ADD COLUMN target_kind TEXT NOT NULL DEFAULT 'legacy_segment'",
	} {
		require.Contains(t, sql, required)
	}
	require.NotContains(t, strings.ToUpper(sql), "DELETE FROM")
	require.NotContains(t, strings.ToUpper(sql), "DROP COLUMN")
}

func TestEffectiveBehaviorMigrationExtendsPublishedTargetChecks(t *testing.T) {
	raw, err := files.ReadFile("008_prompt_audit_effective_behavior.sql")
	require.NoError(t, err)
	sql := string(raw)
	for _, required := range []string{
		"third_party_prompt_audit_model_attempts_stage_check",
		"third_party_prompt_audit_segment_results_target_kind_check",
		"third_party_prompt_audit_outcome_segments_target_kind_check",
		"'effective_behavior'",
	} {
		require.Contains(t, sql, required)
	}
	require.NotContains(t, strings.ToUpper(sql), "DELETE FROM")
	require.NotContains(t, strings.ToUpper(sql), "DROP COLUMN")
}

func TestReauditBatchMigrationAddsNullableBatchIdentity(t *testing.T) {
	raw, err := files.ReadFile("009_prompt_audit_reaudit_batch.sql")
	require.NoError(t, err)
	sql := string(raw)
	require.Contains(t, sql, "ALTER TABLE sub2api_enhance.third_party_prompt_audit_jobs")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS reaudit_batch_id TEXT")
	require.Contains(t, sql, "reuse_kind IN ('fresh', 'within_job', 'history', 'full_evaluation', 'inflight', 'force_batch')")
}

func TestRedisProjectionMigrationAddsOnlyIncrementalWatermarksAndIndexes(t *testing.T) {
	raw, err := files.ReadFile("007_prompt_audit_redis_projection.sql")
	require.NoError(t, err)
	sql := string(raw)
	for _, required := range []string{
		"ADD COLUMN updated_at TIMESTAMPTZ",
		"SET updated_at = COALESCE(finished_at, dispatch_started_at, created_at)",
		"enhance_captures_updated_idx",
		"tppa_jobs_updated_idx",
		"tppa_actions_updated_idx",
		"tppa_attempts_updated_idx",
	} {
		require.Contains(t, sql, required)
	}
	require.NotContains(t, strings.ToUpper(sql), "DELETE FROM")
	require.NotContains(t, strings.ToUpper(sql), "DROP COLUMN")
}
