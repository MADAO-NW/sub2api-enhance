package thirdpartypromptaudit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// throttleRow 是节流查询的返回结果。
type throttleRow struct {
	throttled  bool
	suppressed int
}

// completeFixture 装配一次完整的阻断完成事务，返回仓库与 sqlmock。
func completeFixture(t *testing.T, config Config, throttle throttleRow, warningMerged bool) (*Repository, sqlmock.Sqlmock, *AuditJob, *time.Time) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	configJSON, err := json.Marshal(storedConfig{Config: config, Revision: 3})
	require.NoError(t, err)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status,claim_generation`).WillReturnRows(sqlmock.NewRows([]string{"status", "claim_generation", "lease_valid"}).AddRow("processing", 1, true))
	mock.ExpectQuery(`SELECT target_order,hash,kind`).WillReturnRows(sqlmock.NewRows([]string{"target_order", "hash", "kind", "resolution", "decision", "categories"}).
		AddRow(1, "hash-1", "user", "model", "block", nil))
	mock.ExpectQuery(`SELECT value FROM sub2api_enhance.settings`).WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(string(configJSON)))
	mock.ExpectExec(`INSERT INTO sub2api_enhance.third_party_prompt_audit_enforcement_states`).WillReturnResult(sqlmock.NewResult(0, 1))
	stateRows := sqlmock.NewRows([]string{"hash", "after_job", "armed", "count", "reset_at", "last_job", "escalated_at", "escalated_job"}).
		AddRow("", 0, true, 0, nil, nil, nil, nil)
	mock.ExpectQuery(`SELECT warning_rule_hash`).WillReturnRows(stateRows)
	mock.ExpectQuery(`SELECT username,email,role,status FROM public.users`).WillReturnRows(sqlmock.NewRows([]string{"username", "email", "role", "status"}).AddRow("tester", "tester@example.invalid", "user", "active"))
	mock.ExpectQuery(`counter_reset`).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec(`INSERT INTO sub2api_enhance.audit_job_rounds`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE sub2api_enhance.audit_jobs SET status='done'`).WillReturnResult(sqlmock.NewResult(0, 1))
	if config.Warning.Enabled {
		// 提醒窗口查询在更新处置状态之前执行。
		mock.ExpectQuery(`SELECT violation_counted AND decision='block'`).WillReturnRows(sqlmock.NewRows([]string{"counted"}).AddRow(true))
	}
	mock.ExpectExec(`UPDATE sub2api_enhance.third_party_prompt_audit_enforcement_states SET warning_rule_hash`).WillReturnResult(sqlmock.NewResult(0, 1))
	if config.Warning.Enabled {
		mock.ExpectQuery(`INSERT INTO sub2api_enhance.third_party_prompt_audit_enforcement_actions`).WithArgs(
			int64(7), int64(11), nil, "warning", sqlmock.AnyArg(), sqlmock.AnyArg(), "pending", sqlmock.AnyArg(), "not_required", "succeeded", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnRows(sqlmock.NewRows([]string{"id", "applied_at"}).AddRow(1, now))
	}
	if !warningMerged && !config.Warning.Enabled {
		mock.ExpectQuery(`AS throttled`).WillReturnRows(sqlmock.NewRows([]string{"throttled", "suppressed"}).AddRow(throttle.throttled, throttle.suppressed))
	}
	job := &AuditJob{ID: 11, UserID: 7, APIKeyID: 2, RequestID: "enh-ref", Mode: "blocking", Status: "processing",
		CurrentRound: 1, ReuseMode: ReuseModeAllow, CreatedAt: now, StartedAt: &now, EnforcementEligible: true, ClaimGeneration: 1,
		Config:      ConfigSnapshot{Config: config, Revision: 3, ContractVersion: ContractVersion, FixedContract: OutputContract},
		MaxAttempts: MaxEvaluationAttempts}
	return NewRepository(db), mock, job, &now
}

// 首次发信：窗口内没有已发通知，正常创建投递并记录 suppressed_since_last=0。
func TestCompleteAuditJobSendsFirstBlockingNotice(t *testing.T) {
	config := testConfig()
	repo, mock, job, now := completeFixture(t, config, throttleRow{}, false)
	mock.ExpectQuery(`INSERT INTO sub2api_enhance.third_party_prompt_audit_enforcement_actions`).WithArgs(
		int64(7), int64(11), nil, "blocking_notice", sqlmock.AnyArg(), sqlmock.AnyArg(), "pending", sqlmock.AnyArg(), "not_required", "succeeded", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "applied_at"}).AddRow(2, now))
	mock.ExpectCommit()
	result, err := repo.CompleteAuditJob(context.Background(), job, true, "https://enhance.example.invalid")
	require.NoError(t, err)
	require.Equal(t, DecisionBlock, result.Decision)
	require.Equal(t, []string(nil), result.TriggerCategories)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 窗口内再次阻断：仍插入证据行，但通知状态为 not_required 且不带投递对象。
func TestCompleteAuditJobThrottlesBlockingNoticeWithinWindow(t *testing.T) {
	config := testConfig()
	repo, mock, job, now := completeFixture(t, config, throttleRow{throttled: true, suppressed: 1}, false)
	mock.ExpectQuery(`INSERT INTO sub2api_enhance.third_party_prompt_audit_enforcement_actions`).WithArgs(
		int64(7), int64(11), nil, "blocking_notice", sqlmock.AnyArg(), sqlmock.AnyArg(), "not_required", sqlmock.AnyArg(), "not_required", "succeeded", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "applied_at"}).AddRow(2, now))
	mock.ExpectCommit()
	_, err := repo.CompleteAuditJob(context.Background(), job, true, "")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 窗口后再次阻断：正常发信，但用户正文带上被节流的次数。
func TestCompleteAuditJobResumesBlockingNoticeAfterWindow(t *testing.T) {
	config := testConfig()
	repo, mock, job, now := completeFixture(t, config, throttleRow{throttled: false, suppressed: 2}, false)
	mock.ExpectQuery(`INSERT INTO sub2api_enhance.third_party_prompt_audit_enforcement_actions`).WithArgs(
		int64(7), int64(11), nil, "blocking_notice", sqlmock.AnyArg(), sqlmock.AnyArg(), "pending", sqlmock.AnyArg(), "not_required", "succeeded", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "applied_at"}).AddRow(2, now))
	mock.ExpectCommit()
	_, err := repo.CompleteAuditJob(context.Background(), job, true, "")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// warning 合并仍生效：同一次违规已有提醒动作时阻断证据不再发信。
func TestCompleteAuditJobMergesBlockingNoticeIntoWarning(t *testing.T) {
	config := testConfig()
	config.Warning = WarningConfig{Enabled: true, Window: 3, Limit: 1}
	repo, mock, job, now := completeFixture(t, config, throttleRow{}, true)
	mock.ExpectQuery(`INSERT INTO sub2api_enhance.third_party_prompt_audit_enforcement_actions`).WithArgs(
		int64(7), int64(11), nil, "blocking_notice", sqlmock.AnyArg(), sqlmock.AnyArg(), "not_required", sqlmock.AnyArg(), "not_required", "succeeded", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "applied_at"}).AddRow(2, now))
	mock.ExpectCommit()
	_, err := repo.CompleteAuditJob(context.Background(), job, true, "")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
