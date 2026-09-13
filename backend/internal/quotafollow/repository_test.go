package quotafollow

import (
	"context"
	"encoding/json"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"sub2api-enhance/internal/sub2api"
	"testing"
	"time"
)

func TestObservationModeStoresEventWithoutCreatingDeliveries(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	cfg := DefaultConfig()
	cfg.Epoch = "epoch"
	cfg.Enabled = true
	configRaw, _ := json.Marshal(cfg)
	boundary := time.Now().UTC()
	states := []AccountState{{Account: sub2api.QuotaAccount{ID: 7}, CandidateResetAt: &boundary}}
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT value .* FOR SHARE").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(string(configRaw)))
	mock.ExpectQuery("SELECT last_checked_at .* FOR UPDATE").WillReturnRows(sqlmock.NewRows([]string{"last_checked_at"}).AddRow(nil))
	mock.ExpectExec("INSERT INTO sub2api_enhance.quota_follow_runtime").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO sub2api_enhance.quota_follow_account_states").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("INSERT INTO sub2api_enhance.quota_follow_reset_events").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectCommit()
	require.NoError(t, NewRepository(db).SaveObservation(context.Background(), cfg, sub2api.QuotaDiscovery{Accounts: []sub2api.QuotaAccount{{ID: 7}}, Users: []sub2api.QuotaUser{{ID: 9}}}, "hash", states, &boundary, boundary.Add(time.Hour), "", boundary, nil))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountResetSignalsReadsLatestSuccessfulAuditPerAccount(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	since := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	occurred := since.Add(time.Minute)
	mock.ExpectQuery("SELECT DISTINCT ON .*openai/accounts/:id/reset-quota.*created_at > \\$1").WillReturnRows(sqlmock.NewRows([]string{"account_id", "created_at"}).AddRow(int64(7), occurred))
	signals, err := NewRepository(db).AccountResetSignals(context.Background(), []int64{7, 8}, since)
	require.NoError(t, err)
	require.Equal(t, occurred, signals[7])
	_, ok := signals[8]
	require.False(t, ok)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestScheduleDoesNotAdvanceSuccessfulCheckWatermark(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	next := time.Date(2026, 9, 13, 7, 0, 0, 0, time.UTC)
	mock.ExpectExec("INSERT INTO sub2api_enhance.quota_follow_runtime\\(singleton,next_check_at,last_error,paused_revision\\)").WithArgs(next, "检测失败", nil).WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, NewRepository(db).Schedule(context.Background(), next, "检测失败", nil))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRecordFilterPreservesAllCallerConditions(t *testing.T) {
	now := time.Now()
	where, args := recordFilter(Filter{From: &now, Source: "enhance", Window: "weekly", Status: "uncertain", Keyword: "user"})
	require.Contains(t, where, `"window" =`)
	require.Contains(t, where, "merged_into IS NULL")
	require.Len(t, args, 5)
	require.Equal(t, "weekly", args[2])
	require.Equal(t, "%user%", args[4])
}

func TestStaleDetectorCannotOverwriteNewlyCommittedEvidence(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	cfg := DefaultConfig()
	raw, _ := json.Marshal(cfg)
	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT value .* FOR SHARE").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(string(raw)))
	mock.ExpectQuery("SELECT last_checked_at .* FOR UPDATE").WillReturnRows(sqlmock.NewRows([]string{"last_checked_at"}).AddRow(now))
	mock.ExpectRollback()
	err = NewRepository(db).SaveObservation(context.Background(), cfg, sub2api.QuotaDiscovery{}, "hash", nil, nil, now, "", now, nil)
	require.ErrorContains(t, err, "已有更新的检测结果")
	require.NoError(t, mock.ExpectationsWereMet())
}
func TestOriginalAuditEnrichesEnhancementRecordWithoutDuplicateOrigin(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id,event_id FROM sub2api_enhance.quota_follow_reset_deliveries").WithArgs("correlation", int64(7), "weekly").WillReturnRows(sqlmock.NewRows([]string{"id", "event_id"}).AddRow(11, 13))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_follow_reset_deliveries SET audit_log_id").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_reset_records").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_follow_reset_events").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_follow_collector_state").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	err = NewRepository(db).importAudit(context.Background(), sourceAudit{ID: 19, CreatedAt: now, RequestID: "correlation", HTTPStatus: 200}, 7, "User", "weekly")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
