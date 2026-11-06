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

func TestObservationModeConsumesGroupRoundWithoutCreatingDeliveries(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	cfg := DefaultConfig()
	cfg.Enabled = true
	g := GroupRule{GroupID: 7, Enabled: true, ObserveOnly: true, ResetWeekly: true, Epoch: "epoch"}
	cfg.Groups = []GroupRule{g}
	raw, _ := json.Marshal(cfg)
	boundary := time.Now().UTC()
	previous := Runtime{GroupID: 7}
	previousRaw, _ := json.Marshal(previous)
	next := Runtime{GroupID: 7, Epoch: g.Epoch, LastCheckedAt: &boundary, States: []AccountState{{CandidateResetAt: &boundary, FetchedAt: &boundary}}}
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT value .* FOR SHARE").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
	mock.ExpectExec("INSERT INTO .*quota_follow_group_runtime").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT snapshot .* FOR UPDATE").WillReturnRows(sqlmock.NewRows([]string{"snapshot"}).AddRow(previousRaw))
	mock.ExpectQuery("INSERT INTO .*quota_follow_reset_events").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectExec("UPDATE .*quota_follow_group_runtime").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	require.NoError(t, NewRepository(db).SaveObservation(context.Background(), cfg, g, sub2api.SubscriptionDiscovery{Subscriptions: []sub2api.SubscriptionSnapshot{{ID: 9}}}, next, previous, &boundary))
	require.Nil(t, next.States[0].CandidateResetAt)
	require.Equal(t, boundary, *next.States[0].ConsumedResetAt)
	require.NoError(t, mock.ExpectationsWereMet())
}
func TestStaleDetectorCannotOverwriteNewlyCommittedEvidence(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	cfg := DefaultConfig()
	raw, _ := json.Marshal(cfg)
	actual, _ := json.Marshal(Runtime{LastCheckedAt: timePointer(time.Now())})
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT value").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
	mock.ExpectExec("INSERT INTO .*quota_follow_group_runtime").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT snapshot").WillReturnRows(sqlmock.NewRows([]string{"snapshot"}).AddRow(actual))
	mock.ExpectRollback()
	err = NewRepository(db).SaveObservation(context.Background(), cfg, GroupRule{GroupID: 1}, sub2api.SubscriptionDiscovery{}, Runtime{}, Runtime{}, nil)
	require.ErrorContains(t, err, "旧检测")
	require.NoError(t, mock.ExpectationsWereMet())
}
func TestMoreThanHundredSubscriptionsCreateIndependentBatches(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)
	mock.ExpectQuery("INSERT INTO .*quota_follow_reset_events").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(7))
	targets := []sub2api.SubscriptionSnapshot{}
	for i := 0; i < 101; i++ {
		targets = append(targets, sub2api.SubscriptionSnapshot{ID: int64(i + 1), UserID: int64(i + 1000)})
	}
	mock.ExpectExec("UPDATE .*quota_follow_subscription_batches").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("UPDATE .*quota_follow_subscription_items").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("UPDATE .*quota_reset_records").WillReturnResult(sqlmock.NewResult(0, 0))
	for batch, start := range []int{0, 100} {
		mock.ExpectQuery("INSERT INTO .*quota_follow_subscription_batches").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(batch + 1))
		for i := start; i < min(start+100, 101); i++ {
			mock.ExpectQuery("INSERT INTO .*quota_follow_subscription_items").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(i + 1))
			mock.ExpectExec("INSERT INTO .*quota_reset_records").WillReturnResult(sqlmock.NewResult(0, 1))
		}
	}
	_, err = NewRepository(db).createEvent(context.Background(), tx, "key", "automatic", GroupRule{GroupID: 1, ResetWeekly: true}, "hash", sub2api.SubscriptionDiscovery{Subscriptions: targets}, []byte(`[]`), time.Now(), 1)
	require.NoError(t, err)
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}
func TestRecordFilterIncludesGroupAndSubscriptionSearch(t *testing.T) {
	id := int64(7)
	where, args := recordFilter(Filter{GroupID: &id, Keyword: "99", Window: "weekly"})
	require.Contains(t, where, "group_id =")
	require.Contains(t, where, "subscription_id::text")
	require.Len(t, args, 3)
}
func TestAuditDoesNotTurnBatchHTTP200IntoPerSubscriptionSuccess(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE .*quota_reset_records").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO .*quota_reset_records").WithArgs(sqlmock.AnyArg(), int64(2), "user", "weekly", sqlmock.AnyArg(), "uncertain", "request", sqlmock.AnyArg(), int64(5), int64(3), int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	err = NewRepository(db).importAudit(context.Background(), sourceAudit{ID: 5, CreatedAt: time.Now(), RequestID: "request", Body: `{"subscription_ids":[1],"action":"reset_quota","weekly":true}`, Path: "/api/v1/admin/subscriptions/bulk-action", HTTPStatus: 200}, map[int64]sub2api.SubscriptionSnapshot{1: {ID: 1, UserID: 2, GroupID: 3, Username: "user"}})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestResetCardAuditNeedsActualWeeklyDropAndCannotReplayConsumedSamples(t *testing.T) {
	for _, test := range []struct {
		name                 string
		drop, consumed, want bool
	}{{"audit alone", false, false, false}, {"fresh weekly drop", true, false, true}, {"already consumed", true, true, false}} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			now := time.Now().UTC()
			before := now.Add(-time.Minute)
			at := now.Add(-10 * time.Second)
			next := now.Add(time.Hour)
			state := AccountState{Account: sub2api.SubscriptionAccount{ID: 1, Type: "oauth"}, FetchedAt: &now, NextResetAt: &next, SuspectedDrop: test.drop}
			if test.consumed {
				state.ConsumedObservedAt = &now
			}
			states := []AccountState{state}
			mock.ExpectQuery("SELECT DISTINCT ON").WillReturnRows(sqlmock.NewRows([]string{"id", "account_id", "created_at", "latency_ms"}).AddRow(10, 1, at, 1000))
			err = NewRepository(db).ApplyAccountResetSignals(context.Background(), states, map[int64]AccountState{1: {FetchedAt: &before}}, before.Add(-time.Hour))
			require.NoError(t, err)
			require.Equal(t, test.want, states[0].CandidateResetAt != nil)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
