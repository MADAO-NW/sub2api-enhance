package quotafollow

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestManualResetReplaysSameEventWithoutCallingOriginalAgain(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT id,status,COALESCE").WillReturnRows(sqlmock.NewRows([]string{"id", "status", "revision"}).AddRow(99, "completed", 3))
	api := &fakeAPI{}
	s := &Service{repo: NewRepository(db), api: api}
	v, err := s.ImmediateReset(context.Background(), ImmediateResetRequest{ExpectedRevision: 3, RequestID: "b85ee0b8-dd18-423c-bd6b-a38679fb4414"})
	require.NoError(t, err)
	require.Equal(t, int64(99), v.EventID)
	require.Zero(t, api.resetCalls)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestManualResetRejectsReusedKeyForDifferentRevision(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT id,status,COALESCE").WillReturnRows(sqlmock.NewRows([]string{"id", "status", "revision"}).AddRow(99, "completed", 2))
	s := &Service{repo: NewRepository(db)}
	_, err = s.ImmediateReset(context.Background(), ImmediateResetRequest{ExpectedRevision: 3, RequestID: "b85ee0b8-dd18-423c-bd6b-a38679fb4414"})
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestManualResetRejectsInvalidRequestBeforeAnySideEffect(t *testing.T) {
	s := &Service{}
	_, err := s.ImmediateReset(context.Background(), ImmediateResetRequest{ExpectedRevision: 1, RequestID: "bad"})
	require.Error(t, err)
	_, err = s.ImmediateReset(context.Background(), ImmediateResetRequest{RequestID: "b85ee0b8-dd18-423c-bd6b-a38679fb4414"})
	require.Error(t, err)
}

func TestManualResetRejectsChangedConfiguration(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	cfg := DefaultConfig()
	raw, _ := json.Marshal(cfg)
	mock.ExpectQuery("SELECT id,status,COALESCE").WillReturnRows(sqlmock.NewRows([]string{"id", "status", "revision"}))
	mock.ExpectQuery("SELECT value FROM sub2api_enhance.settings").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
	s := &Service{repo: NewRepository(db)}
	_, err = s.ImmediateReset(context.Background(), ImmediateResetRequest{ExpectedRevision: 4, RequestID: "b85ee0b8-dd18-423c-bd6b-a38679fb4414"})
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAcknowledgementUnblocksWithoutChangingUncertainSubscriptionOutcomes(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT status FROM sub2api_enhance.quota_follow_subscription_batches").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("uncertain"))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_reset_records SET evidence=").WithArgs(int64(4), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_follow_subscription_batches SET status='reviewed'").WithArgs(int64(4)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	api := &fakeAPI{}
	err = (&Service{repo: NewRepository(db), api: api}).AcknowledgeUncertain(context.Background(), 4, 7, "已核对原版记录，允许后续轮次继续")
	require.NoError(t, err)
	require.Zero(t, api.resetCalls)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAcknowledgementRequiresNoteAndUncertainBatch(t *testing.T) {
	require.Error(t, (&Service{}).AcknowledgeUncertain(context.Background(), 4, 7, ""))
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT status FROM sub2api_enhance.quota_follow_subscription_batches").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("succeeded"))
	mock.ExpectRollback()
	require.Error(t, (&Service{repo: NewRepository(db)}).AcknowledgeUncertain(context.Background(), 4, 7, "已核对"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRetryReturnsExistingSupplementaryEventWithoutResending(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT id,status FROM sub2api_enhance.quota_follow_reset_events").WithArgs("subscription-retry:7").WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(99, "completed"))
	api := &fakeAPI{}
	out, err := (&Service{repo: NewRepository(db), api: api}).RetryFailed(context.Background(), 7)
	require.NoError(t, err)
	require.Equal(t, int64(99), out.EventID)
	require.Zero(t, api.resetCalls)
	require.NoError(t, mock.ExpectationsWereMet())
}
