package quotafollow

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"sub2api-enhance/internal/sub2api"
	"testing"
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
func TestClaimUsesSubscriptionIDAndBothWindowsInOneFixedRequest(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	cfg := DefaultConfig()
	raw, _ := json.Marshal(cfg)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT value .* FOR SHARE").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
	expectGroupClaim(mock)
	mock.ExpectQuery("SELECT status .* FOR UPDATE").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("pending"))
	mock.ExpectExec("UPDATE .*quota_follow_subscription_items").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE .*quota_reset_records").WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectQuery("UPDATE .*quota_follow_subscription_batches").WillReturnRows(sqlmock.NewRows([]string{"started_at"}).AddRow(nil))
	mock.ExpectCommit()
	b := &Batch{ID: 10, Rule: GroupRule{ResetDaily: true, ResetWeekly: true}, Items: []BatchItem{{ID: 20, SubscriptionID: 99, UserID: 7, Status: "pending"}}}
	sent, err := NewRepository(db).ClaimBatch(context.Background(), b, cfg.Revision)
	require.NoError(t, err)
	require.True(t, sent)
	var body sub2api.SubscriptionResetRequest
	require.NoError(t, json.Unmarshal([]byte(b.Body), &body))
	require.Equal(t, []int64{99}, body.IDs)
	require.True(t, body.Daily)
	require.True(t, body.Weekly)
	require.False(t, body.Monthly)
	require.NoError(t, mock.ExpectationsWereMet())
}
func TestClaimCommitFailureDoesNotPermitOutboundCall(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	raw, _ := json.Marshal(DefaultConfig())
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT value").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
	expectGroupClaim(mock)
	mock.ExpectQuery("SELECT status").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("pending"))
	mock.ExpectExec("UPDATE .*subscription_items").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE .*quota_reset_records").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("UPDATE .*subscription_batches").WillReturnRows(sqlmock.NewRows([]string{"started_at"}).AddRow(nil))
	mock.ExpectCommit().WillReturnError(errors.New("commit result unknown"))
	sent, err := NewRepository(db).ClaimBatch(context.Background(), &Batch{ID: 1, Rule: GroupRule{ResetWeekly: true}, Items: []BatchItem{{SubscriptionID: 1, Status: "pending"}}}, 1)
	require.Error(t, err)
	require.False(t, sent)
	require.NoError(t, mock.ExpectationsWereMet())
}

func expectGroupClaim(mock sqlmock.Sqlmock) {
	mock.ExpectExec("INSERT INTO .*quota_follow_group_runtime").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT group_id .* FOR UPDATE").WillReturnRows(sqlmock.NewRows([]string{"group_id"}).AddRow(1))
	mock.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(false))
}

func TestPersistentGroupClaimBlocksAnotherInflightBatchAfterSessionLockLoss(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	raw, _ := json.Marshal(DefaultConfig())
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT value").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
	mock.ExpectExec("INSERT INTO .*quota_follow_group_runtime").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT group_id .* FOR UPDATE").WillReturnRows(sqlmock.NewRows([]string{"group_id"}).AddRow(7))
	mock.ExpectQuery("SELECT EXISTS").WithArgs(int64(7), int64(99)).WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(true))
	mock.ExpectRollback()
	sent, err := NewRepository(db).ClaimBatch(context.Background(), &Batch{ID: 99, GroupID: 7, Items: []BatchItem{{SubscriptionID: 1, Status: "pending"}}}, 1)
	require.NoError(t, err)
	require.False(t, sent)
	require.NoError(t, mock.ExpectationsWereMet())
}
