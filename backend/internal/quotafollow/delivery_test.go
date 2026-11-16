package quotafollow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"sub2api-enhance/internal/sub2api"
)

func boolPointer(b bool) *bool { return &b }

func TestBatchResponseKeepsPartialSuccessAndNeverReclassifiesSkippedItems(t *testing.T) {
	b := &Batch{Items: []BatchItem{{SubscriptionID: 1, Status: "inflight"}, {SubscriptionID: 2, Status: "inflight"}, {SubscriptionID: 3, Status: "skipped"}}}
	applyReply(b, sub2api.SubscriptionResetReply{HTTPStatus: 200, Result: sub2api.SubscriptionResetResult{Items: []sub2api.SubscriptionResetItem{{ID: 1, Success: boolPointer(true)}, {ID: 2, Success: boolPointer(false), Error: "missing"}}}})
	require.Equal(t, "partial", b.Status)
	require.Equal(t, "succeeded", b.Items[0].Status)
	require.Equal(t, "failed", b.Items[1].Status)
	require.Equal(t, "skipped", b.Items[2].Status)
}

func TestUncertainTransportOrInvalidResponseNeverBecomesRetryable(t *testing.T) {
	for _, status := range []int{0, 200, 409, 500, 503} {
		b := &Batch{Items: []BatchItem{{Status: "inflight"}}}
		applyReply(b, sub2api.SubscriptionResetReply{HTTPStatus: status, Error: errors.New("未确认")})
		require.Equal(t, "uncertain", b.Status)
		require.Equal(t, "uncertain", b.Items[0].Status)
	}
}

func TestMalformedPerItemResultStaysUncertain(t *testing.T) {
	b := &Batch{Items: []BatchItem{{SubscriptionID: 1, Status: "inflight"}}}
	applyReply(b, sub2api.SubscriptionResetReply{HTTPStatus: 200, Result: sub2api.SubscriptionResetResult{Items: []sub2api.SubscriptionResetItem{{ID: 1}}}})
	require.Equal(t, "uncertain", b.Status)
	require.Equal(t, "uncertain", b.Items[0].Status)
}

func expectGlobalClaim(mock sqlmock.Sqlmock) {
	mock.ExpectExec("INSERT INTO sub2api_enhance.quota_follow_monitor_runtime").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT snapshot FROM sub2api_enhance.quota_follow_monitor_runtime").WillReturnRows(sqlmock.NewRows([]string{"snapshot"}).AddRow([]byte(`{}`)))
}

func TestClaimPersistsFixedDailyWeeklyMonthlyBody(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	expectGlobalClaim(mock)
	mock.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(false))
	mock.ExpectQuery("SELECT status FROM sub2api_enhance.quota_follow_subscription_batches").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("pending"))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_follow_subscription_items").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_reset_records").WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectQuery("UPDATE sub2api_enhance.quota_follow_subscription_batches").WillReturnRows(sqlmock.NewRows([]string{"started_at"}).AddRow(nil))
	mock.ExpectCommit()
	b := &Batch{ID: 10, Trigger: "manual", Items: []BatchItem{{ID: 20, SubscriptionID: 99, UserID: 7, GroupID: 3, Status: "pending"}}}
	sent, err := NewRepository(db).ClaimBatch(context.Background(), b)
	require.NoError(t, err)
	require.True(t, sent)
	var body sub2api.SubscriptionResetRequest
	require.NoError(t, json.Unmarshal([]byte(b.Body), &body))
	require.Equal(t, []int64{99}, body.IDs)
	require.True(t, body.Daily)
	require.True(t, body.Weekly)
	require.True(t, body.Monthly)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAnyUnresolvedBatchBlocksTheWholeQueue(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	expectGlobalClaim(mock)
	mock.ExpectQuery("SELECT EXISTS").WithArgs(int64(99)).WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(true))
	mock.ExpectRollback()
	sent, err := NewRepository(db).ClaimBatch(context.Background(), &Batch{ID: 99, Trigger: "manual", Items: []BatchItem{{ID: 1, SubscriptionID: 1, Status: "pending"}}})
	require.NoError(t, err)
	require.False(t, sent)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAutomaticBatchWaitsWhileTheModuleIsDisabled(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	raw, _ := json.Marshal(DefaultConfig())
	mock.ExpectBegin()
	expectGlobalClaim(mock)
	mock.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(false))
	mock.ExpectQuery("SELECT status FROM sub2api_enhance.quota_follow_subscription_batches").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("pending"))
	mock.ExpectQuery("SELECT value FROM sub2api_enhance.settings").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
	mock.ExpectRollback()
	sent, err := NewRepository(db).ClaimBatch(context.Background(), &Batch{ID: 5, Trigger: "automatic", Epoch: "epoch", Items: []BatchItem{{ID: 1, SubscriptionID: 1, Status: "pending"}}})
	require.NoError(t, err)
	require.False(t, sent)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAutomaticBatchWithChangedEpochIsSkippedInsideTheClaim(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	cfg := testConfig()
	cfg.Epoch = "other"
	raw, _ := json.Marshal(cfg)
	mock.ExpectBegin()
	expectGlobalClaim(mock)
	mock.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(false))
	mock.ExpectQuery("SELECT status FROM sub2api_enhance.quota_follow_subscription_batches").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("pending"))
	mock.ExpectQuery("SELECT value FROM sub2api_enhance.settings").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_follow_subscription_items").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_reset_records").WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_follow_subscription_batches SET status='skipped'").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_follow_reset_events").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	b := &Batch{ID: 5, Trigger: "automatic", Epoch: "epoch", Items: []BatchItem{{ID: 1, SubscriptionID: 1, Status: "pending"}}}
	sent, err := NewRepository(db).ClaimBatch(context.Background(), b)
	require.NoError(t, err)
	require.False(t, sent)
	require.Equal(t, "skipped", b.Status)
	require.Equal(t, "skipped", b.Items[0].Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClaimCommitFailureDoesNotPermitOutboundCall(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	expectGlobalClaim(mock)
	mock.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(false))
	mock.ExpectQuery("SELECT status FROM sub2api_enhance.quota_follow_subscription_batches").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("pending"))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_follow_subscription_items").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_reset_records").WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectQuery("UPDATE sub2api_enhance.quota_follow_subscription_batches").WillReturnRows(sqlmock.NewRows([]string{"started_at"}).AddRow(nil))
	mock.ExpectCommit().WillReturnError(errors.New("commit result unknown"))
	sent, err := NewRepository(db).ClaimBatch(context.Background(), &Batch{ID: 1, Trigger: "manual", Items: []BatchItem{{SubscriptionID: 1, Status: "pending"}}})
	require.Error(t, err)
	require.False(t, sent)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestEpochSkipAlsoPersistsItemsMarkedSkippedBeforeTheClaim(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	cfg := testConfig()
	cfg.Epoch = "other"
	raw, _ := json.Marshal(cfg)
	mock.ExpectBegin()
	expectGlobalClaim(mock)
	mock.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(false))
	mock.ExpectQuery("SELECT status FROM sub2api_enhance.quota_follow_subscription_batches").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("pending"))
	mock.ExpectQuery("SELECT value FROM sub2api_enhance.settings").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_follow_subscription_items").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_reset_records").WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_follow_subscription_batches SET status='skipped'").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_follow_reset_events").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	b := &Batch{ID: 5, Trigger: "automatic", Epoch: "epoch", Items: []BatchItem{{ID: 1, SubscriptionID: 1, Status: "skipped", Error: "订阅已失效、过期或不再属于目标用户及分组"}}}
	sent, err := NewRepository(db).ClaimBatch(context.Background(), b)
	require.NoError(t, err)
	require.False(t, sent)
	require.Equal(t, "订阅已失效、过期或不再属于目标用户及分组", b.Items[0].Error)
	require.Equal(t, "skipped", b.Items[0].Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAfterSnapshotsOnlyWriteObservation(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	snapshot := sub2api.SubscriptionSnapshot{ID: 3}
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE sub2api_enhance.quota_follow_subscription_items SET after_snapshot=").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_reset_records SET after_snapshot=").WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectCommit()
	require.NoError(t, NewRepository(db).SaveAfterSnapshots(context.Background(), []BatchItem{{ID: 7, SubscriptionID: 3, Status: "succeeded", After: &snapshot}, {ID: 8, SubscriptionID: 4, Status: "skipped"}}))
	require.NoError(t, mock.ExpectationsWereMet())
}
