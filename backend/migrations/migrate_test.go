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

func TestBatchMigrationUsesNullableTargetColumnsAndAtomicStatuses(t *testing.T) {
	raw, err := files.ReadFile("012_prompt_audit_batches.sql")
	require.NoError(t, err)
	sql := string(raw)
	require.Contains(t, sql, "batch_type IN ('pending_review','failed_recovery','reaudit_selected','reaudit_filter')")
	require.Contains(t, sql, "CHECK ((capture_id IS NOT NULL) <> (job_id IS NOT NULL))")
	require.Contains(t, sql, "CREATE UNIQUE INDEX third_party_prompt_audit_batch_items_capture_uniq")
	require.Contains(t, sql, "CREATE UNIQUE INDEX third_party_prompt_audit_batch_items_job_uniq")
	require.Contains(t, sql, "source_audit_round")
	require.Contains(t, sql, "claim_generation")
	require.Contains(t, sql, "request_hash")
	require.Contains(t, sql, "third_party_prompt_audit_batches_active_request_uniq")
	require.NotContains(t, sql, "PRIMARY KEY (batch_id, capture_id, job_id)")
	require.NotContains(t, strings.ToUpper(sql), "DROP TABLE")
	require.NotContains(t, strings.ToUpper(sql), "DELETE FROM")
}

// expectMigrationFile 期望一个迁移文件在单独事务中整体执行并登记台账。
func expectMigrationFile(mock sqlmock.Sqlmock, name, content string) {
	mock.ExpectQuery("SELECT checksum FROM sub2api_enhance.schema_migrations WHERE filename=$1").WithArgs(name).WillReturnRows(sqlmock.NewRows([]string{"checksum"}))
	mock.ExpectBegin()
	mock.ExpectExec(content).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO sub2api_enhance.schema_migrations(filename,checksum) VALUES($1,$2)").WithArgs(name, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

func embeddedMigrations(t *testing.T) ([]string, map[string]string) {
	t.Helper()
	entries, err := files.ReadDir(".")
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	contents := make(map[string]string, len(entries))
	for _, entry := range entries {
		raw, err := files.ReadFile(entry.Name())
		require.NoError(t, err)
		names = append(names, entry.Name())
		contents[entry.Name()] = strings.TrimSpace(string(raw))
	}
	return names, contents
}

// expectSchemaReady 期望连接锁与 schema 初始化，和迁移执行器的实际语句一致。
func expectSchemaReady(mock sqlmock.Sqlmock) {
	mock.ExpectQuery("SELECT pg_try_advisory_lock($1)").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectExec(`CREATE SCHEMA IF NOT EXISTS sub2api_enhance; CREATE TABLE IF NOT EXISTS sub2api_enhance.schema_migrations(filename TEXT PRIMARY KEY,checksum TEXT NOT NULL,applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`).WillReturnResult(sqlmock.NewResult(0, 0))
}

// 空库新安装：001–019 按文件名顺序逐个事务执行，审计迁移在订阅迁移之前。
func TestEmptyDatabaseRunsAuditThenSubscriptionMigrations(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	names, contents := embeddedMigrations(t)
	require.Equal(t, "013_prompt_audit_v3.sql", names[len(names)-7])
	require.Equal(t, "014_subscription_quota_follow.sql", names[len(names)-6])
	require.Equal(t, "015_prompt_audit_node_auth.sql", names[len(names)-5])
	require.Equal(t, "016_prompt_audit_optimize.sql", names[len(names)-4])
	require.Equal(t, "017_prompt_audit_metrics.sql", names[len(names)-3])
	require.Equal(t, "018_prompt_audit_reaudit_fresh.sql", names[len(names)-2])
	require.Equal(t, "019_prompt_audit_categories_samples.sql", names[len(names)-1])
	expectSchemaReady(mock)
	for _, name := range names {
		expectMigrationFile(mock, name, contents[name])
	}
	mock.ExpectQuery("SELECT pg_advisory_unlock($1)").WillReturnRows(sqlmock.NewRows([]string{"released"}).AddRow(true))
	require.NoError(t, Run(context.Background(), db))
	require.NoError(t, mock.ExpectationsWereMet())
}

// 上一版本已执行 001–017：升级只追加 018、019，按文件名顺序执行。
func TestUpgradeFromV026RunsAuditThenSubscriptionMigrations(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	names, contents := embeddedMigrations(t)
	applied, pending := names[:len(names)-2], names[len(names)-2:]
	require.Equal(t, []string{"018_prompt_audit_reaudit_fresh.sql", "019_prompt_audit_categories_samples.sql"}, pending)
	expectSchemaReady(mock)
	for _, name := range applied {
		checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(contents[name])))
		mock.ExpectQuery("SELECT checksum FROM sub2api_enhance.schema_migrations WHERE filename=$1").WithArgs(name).WillReturnRows(sqlmock.NewRows([]string{"checksum"}).AddRow(checksum))
	}
	for _, name := range pending {
		expectMigrationFile(mock, name, contents[name])
	}
	mock.ExpectQuery("SELECT pg_advisory_unlock($1)").WillReturnRows(sqlmock.NewRows([]string{"released"}).AddRow(true))
	require.NoError(t, Run(context.Background(), db))
	require.NoError(t, mock.ExpectationsWereMet())
}

// 013 前置检查失败时整个文件回滚且不登记台账，014 不会被执行。
func TestPromptAuditV3MigrationFailureRollsBackWholeFile(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	names, contents := embeddedMigrations(t)
	expectSchemaReady(mock)
	for _, name := range names[:len(names)-7] {
		checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(contents[name])))
		mock.ExpectQuery("SELECT checksum FROM sub2api_enhance.schema_migrations WHERE filename=$1").WithArgs(name).WillReturnRows(sqlmock.NewRows([]string{"checksum"}).AddRow(checksum))
	}
	last := names[len(names)-7]
	// 前置检查发现未完成处置动作时 RAISE EXCEPTION：整个文件回滚，不登记台账。
	mock.ExpectQuery("SELECT checksum FROM sub2api_enhance.schema_migrations WHERE filename=$1").WithArgs(last).WillReturnRows(sqlmock.NewRows([]string{"checksum"}))
	mock.ExpectBegin()
	mock.ExpectExec(contents[last]).WillReturnError(errors.New("提示词审计存在未完成的处置动作或通知"))
	mock.ExpectRollback()
	mock.ExpectQuery("SELECT pg_advisory_unlock($1)").WillReturnRows(sqlmock.NewRows([]string{"released"}).AddRow(true))
	err = Run(context.Background(), db)
	require.ErrorContains(t, err, "013_prompt_audit_v3.sql")
	require.ErrorContains(t, err, "未完成的处置动作")
	require.NoError(t, mock.ExpectationsWereMet())
}

// 013 内语句顺序：前置检查 → 解除外键 → DROP → 新建 → 外键重建 → 配置。
func TestPromptAuditV3MigrationOrdersPrecheckDropsAndRebuilds(t *testing.T) {
	raw, err := files.ReadFile("013_prompt_audit_v3.sql")
	require.NoError(t, err)
	sql := string(raw)
	order := []string{
		"RAISE EXCEPTION",
		"DROP COLUMN last_outcome_id",
		"RENAME COLUMN warning_window_after_outcome_id TO warning_window_after_job_id",
		"SET warning_window_after_job_id = 0",
		"DROP COLUMN outcome_id",
		"'breaker_notice'",
		"DROP TABLE",
		"CREATE TABLE sub2api_enhance.prompt_texts",
		"CREATE TABLE sub2api_enhance.audit_jobs",
		"CREATE TABLE sub2api_enhance.prompt_reviews",
		"CREATE TABLE sub2api_enhance.prompt_trust",
		"CREATE TABLE sub2api_enhance.audit_job_rounds",
		"CREATE TABLE sub2api_enhance.audit_job_targets",
		"CREATE TABLE sub2api_enhance.model_attempts",
		"CREATE TABLE sub2api_enhance.instruction_violation_marks",
		"CREATE TABLE sub2api_enhance.instruction_breakers",
		"ADD COLUMN last_job_id BIGINT REFERENCES sub2api_enhance.audit_jobs(id) ON DELETE SET NULL",
		"ADD COLUMN job_id BIGINT REFERENCES sub2api_enhance.audit_jobs(id) ON DELETE SET NULL",
		"UNIQUE (job_id, action_type)",
		"'retention_days', 30",
	}
	position := -1
	for _, fragment := range order {
		index := strings.Index(sql, fragment)
		require.Greater(t, index, position, fragment)
		position = index
	}
	for _, table := range []string{"captures", "third_party_prompt_audit_jobs", "third_party_prompt_audit_outcomes", "third_party_prompt_audit_segment_results",
		"third_party_prompt_audit_outcome_segments", "third_party_prompt_audit_targets", "third_party_prompt_audit_job_targets",
		"third_party_prompt_audit_batches", "third_party_prompt_audit_batch_items", "third_party_prompt_audit_model_attempts"} {
		require.True(t, strings.Contains(sql, "sub2api_enhance."+table+",") || strings.Contains(sql, "sub2api_enhance."+table+";"), table)
	}
	// 处置状态与处置动作只改造、不删除。
	require.NotContains(t, sql, "DROP TABLE sub2api_enhance.third_party_prompt_audit_enforcement")
	require.NotContains(t, strings.ToUpper(sql), "DELETE FROM")
	require.Contains(t, sql, "execution_status IN ('pending', 'processing', 'unknown')")
	require.Contains(t, sql, "notification_status IN ('pending', 'processing', 'retry')")
}

// 014 只新增订阅结构与列，不删除已发布业务历史。
func TestSubscriptionFollowMigrationKeepsPublishedHistory(t *testing.T) {
	raw, err := files.ReadFile("014_subscription_quota_follow.sql")
	require.NoError(t, err)
	sql := string(raw)
	for _, fragment := range []string{"quota_follow_group_runtime", "quota_follow_subscription_batches", "quota_follow_reset_events"} {
		require.Contains(t, sql, fragment)
	}
	require.NotContains(t, strings.ToUpper(sql), "DROP TABLE")
	require.NotContains(t, strings.ToUpper(sql), "DELETE FROM")
	require.NotContains(t, strings.ToUpper(sql), "DROP COLUMN")
}
