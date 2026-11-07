package thirdpartypromptaudit

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// 加严集合只影响后续请求的处理方式：异步提升为同步，全局 off 与排除用户仍然放行。
func TestEscalationPromotesAsyncUserToBlocking(t *testing.T) {
	config := testConfig()
	config.Mode = "async"
	manager := &ConfigManager{active: &activeConfig{Stored: storedConfig{Config: config}}}
	require.Equal(t, "async", manager.EffectiveModeForUser(7))
	manager.SetEscalation(7, true)
	require.Equal(t, "blocking", manager.EffectiveModeForUser(7))
	require.Equal(t, "async", manager.EffectiveModeForUser(8))
	manager.SetEscalation(7, false)
	require.Equal(t, "async", manager.EffectiveModeForUser(7))
	manager.SetEscalation(7, true)
	manager.active.Stored.Mode = "off"
	require.Equal(t, "off", manager.EffectiveModeForUser(7))
	manager.active.Stored.Mode = "async"
	manager.active.Stored.ExcludedUserIDs = []int64{7}
	require.Equal(t, "off", manager.EffectiveModeForUser(7))
}

// 审核入口同样读取加严集合：加严的异步用户按同步审核，后台请求仍降回异步，解除后恢复异步。
func TestCheckUsesEscalationForAsyncUser(t *testing.T) {
	config := testConfig()
	config.Mode = "async"
	manager := &ConfigManager{active: &activeConfig{Stored: storedConfig{Config: config}}}
	service := &Service{config: manager, closing: true, metrics: NewRuntimeMetrics()}
	parsed := &ParsedRequest{TurnFingerprint: "turn"}
	request := IntakeRequest{Parsed: parsed, UserID: 7, Provider: "openai"}
	require.Equal(t, "async", service.Check(context.Background(), request).Mode)
	manager.SetEscalation(7, true)
	require.Equal(t, "blocking", service.Check(context.Background(), request).Mode)
	require.Equal(t, "async", service.Check(context.Background(), IntakeRequest{Parsed: parsed, UserID: 8, Provider: "openai"}).Mode)
	background := request
	background.Background = true
	require.Equal(t, "async", service.Check(context.Background(), background).Mode)
	manager.SetEscalation(7, false)
	require.Equal(t, "async", service.Check(context.Background(), request).Mode)
}

// 解除加严清空状态并记录本地动作，不发通知；未加严时返回 ErrNotFound。
func TestClearEscalationRecordsAction(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT escalated_job_id`).WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"escalated_job_id"}).AddRow(int64(11)))
	mock.ExpectExec(`SET escalated_at=NULL,escalated_job_id=NULL`).WithArgs(int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO sub2api_enhance.third_party_prompt_audit_enforcement_actions`).WithArgs(
		int64(7), nil, int64(99), "escalation_clear", sqlmock.AnyArg(), sqlmock.AnyArg(), "not_required", sqlmock.AnyArg(), "not_required", "succeeded", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "applied_at"}).AddRow(5, nil))
	mock.ExpectCommit()
	actionID, err := NewRepository(db).ClearEscalation(context.Background(), 7, 99)
	require.NoError(t, err)
	require.Equal(t, int64(5), actionID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClearEscalationRejectsUserWithoutEscalation(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT escalated_job_id`).WithArgs(int64(7)).WillReturnRows(sqlmock.NewRows([]string{"escalated_job_id"}))
	mock.ExpectRollback()
	_, err = NewRepository(db).ClearEscalation(context.Background(), 7, 99)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 回放创建：同一时间只允许一个运行，样本为空表示全部样本。
func TestCreateReplayRejectsConcurrentRun(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM sub2api_enhance.policy_replays WHERE status='running'`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	_, err = NewRepository(db).CreateReplay(context.Background(), testConfig(), []int64{1}, 99)
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSampleIDsForReplayReturnsAllWhenUnselected(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`SELECT id FROM sub2api_enhance.policy_samples`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1).AddRow(2))
	ids, err := NewRepository(db).sampleIDsForReplay(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2}, ids)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Equal(t, []int64{3, 4, 4}, mustSorted(t))
}

// mustSorted 校验显式样本选择会先排序。
func mustSorted(t *testing.T) []int64 {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ids, err := NewRepository(db).sampleIDsForReplay(context.Background(), []int64{4, 3, 4})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	return ids
}
