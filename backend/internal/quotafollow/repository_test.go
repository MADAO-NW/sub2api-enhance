package quotafollow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"sub2api-enhance/internal/sub2api"
)

func TestMoreThanHundredSubscriptionsCreateIndependentBatches(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)
	mock.ExpectQuery("INSERT INTO sub2api_enhance.quota_follow_reset_events").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(7))
	subscriptions := []sub2api.SubscriptionSnapshot{}
	for i := 0; i < 101; i++ {
		subscriptions = append(subscriptions, sub2api.SubscriptionSnapshot{ID: int64(i + 1), UserID: int64(i + 1000), GroupID: int64(i%3 + 1)})
	}
	for batch := 0; batch < 2; batch++ {
		mock.ExpectQuery("INSERT INTO sub2api_enhance.quota_follow_subscription_batches").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(batch + 1))
		for i := batch * 100; i < min((batch+1)*100, 101); i++ {
			mock.ExpectQuery("INSERT INTO sub2api_enhance.quota_follow_subscription_items").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(i + 1))
			for range resetWindows {
				mock.ExpectExec("INSERT INTO sub2api_enhance.quota_reset_records").WillReturnResult(sqlmock.NewResult(0, 1))
			}
		}
	}
	created, ok, err := NewRepository(db).createEvent(context.Background(), tx, DefaultConfig(), &pendingEvent{Key: "manual:test", Trigger: "manual", ResetAt: time.Now(), Subscriptions: subscriptions})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(7), created)
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNewAutomaticEventSkipsEarlierPendingBatches(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)
	mock.ExpectQuery("INSERT INTO sub2api_enhance.quota_follow_reset_events").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(9))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_follow_subscription_batches b").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_follow_subscription_items i").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_reset_records r").WillReturnResult(sqlmock.NewResult(0, 3))
	_, created, err := NewRepository(db).createEvent(context.Background(), tx, DefaultConfig(), &pendingEvent{Key: "automatic:e:1", Trigger: "automatic", ResetAt: time.Now()})
	require.NoError(t, err)
	require.True(t, created)
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDuplicateEventKeyIsAlreadyHandled(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)
	mock.ExpectQuery("INSERT INTO sub2api_enhance.quota_follow_reset_events").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	id, created, err := NewRepository(db).createEvent(context.Background(), tx, DefaultConfig(), &pendingEvent{Key: "automatic:e:1", Trigger: "automatic", ResetAt: time.Now()})
	require.NoError(t, err)
	require.False(t, created)
	require.Zero(t, id)
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestStaleDetectorCannotOverwriteNewlyCommittedState(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	cfg := DefaultConfig()
	raw, _ := json.Marshal(cfg)
	actual, _ := json.Marshal(Runtime{Epoch: "epoch", LastCheckedAt: timePointer(time.Now())})
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT value FROM sub2api_enhance.settings").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
	mock.ExpectExec("INSERT INTO sub2api_enhance.quota_follow_monitor_runtime").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT snapshot FROM sub2api_enhance.quota_follow_monitor_runtime").WillReturnRows(sqlmock.NewRows([]string{"snapshot"}).AddRow(actual))
	mock.ExpectRollback()
	_, err = NewRepository(db).SaveObservation(context.Background(), cfg, Runtime{Epoch: "epoch"}, Runtime{Epoch: "epoch"}, nil)
	require.ErrorContains(t, err, "旧检测")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSaveObservationReturnsEventIDAndResetTime(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	cfg := testConfig()
	raw, _ := json.Marshal(cfg)
	checkedAt := time.Now().UTC()
	previous := Runtime{Epoch: "epoch", LastCheckedAt: &checkedAt}
	initial, _ := json.Marshal(previous)
	next := Runtime{Epoch: "epoch", LastCheckedAt: &checkedAt}
	eventAt := checkedAt.Add(-time.Hour)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT value FROM sub2api_enhance.settings").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
	mock.ExpectExec("INSERT INTO sub2api_enhance.quota_follow_monitor_runtime").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT snapshot FROM sub2api_enhance.quota_follow_monitor_runtime").WillReturnRows(sqlmock.NewRows([]string{"snapshot"}).AddRow(initial))
	mock.ExpectQuery("INSERT INTO sub2api_enhance.quota_follow_reset_events").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(12))
	mock.ExpectQuery("INSERT INTO sub2api_enhance.quota_follow_subscription_batches").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(4))
	mock.ExpectQuery("INSERT INTO sub2api_enhance.quota_follow_subscription_items").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(21))
	mock.ExpectExec("INSERT INTO sub2api_enhance.quota_reset_records").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO sub2api_enhance.quota_reset_records").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO sub2api_enhance.quota_reset_records").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_follow_monitor_runtime").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	saved, err := NewRepository(db).SaveObservation(context.Background(), cfg, previous, next, &pendingEvent{Key: "manual:test", Trigger: "manual", ResetAt: eventAt, Subscriptions: []sub2api.SubscriptionSnapshot{{ID: 3, UserID: 4, GroupID: 5}}})
	require.NoError(t, err)
	require.Equal(t, eventAt, *saved.LastResetAt)
	require.Equal(t, int64(12), *saved.LastEventID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRecordFilterIncludesGroupAndSubscriptionSearch(t *testing.T) {
	id := int64(7)
	where, args := recordFilter(Filter{GroupID: &id, Keyword: "99", Window: "monthly"})
	require.Contains(t, where, "group_id =")
	require.Contains(t, where, "subscription_id::text")
	require.Len(t, args, 3)
}

func TestConfigDefaultsToRevisionOneWithoutRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT value FROM sub2api_enhance.settings").WillReturnRows(sqlmock.NewRows([]string{"value"}))
	cfg, err := NewRepository(db).Config(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(1), cfg.Revision)
	require.False(t, cfg.Enabled)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSavingConfigRebuildsEpochOnlyForRealScopeChanges(t *testing.T) {
	for _, test := range []struct {
		name         string
		old          SavedConfig
		enabled      bool
		accountID    int64
		wantNewEpoch bool
	}{
		{"unpausing without scope change", SavedConfig{Config: Config{Enabled: true, MonitorAccountID: 7}, Epoch: "e", Revision: 2}, true, 7, false},
		{"enabling generates epoch", SavedConfig{Config: Config{MonitorAccountID: 7}, Epoch: "e", Revision: 2}, true, 7, true},
		{"changing account generates epoch", SavedConfig{Config: Config{Enabled: true, MonitorAccountID: 7}, Epoch: "e", Revision: 2}, true, 8, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			raw, _ := json.Marshal(test.old)
			mock.ExpectBegin()
			mock.ExpectExec("SELECT pg_advisory_xact_lock").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery("SELECT value FROM sub2api_enhance.settings").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
			mock.ExpectExec("INSERT INTO sub2api_enhance.settings").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			saved, err := NewRepository(db).SaveConfig(context.Background(), ConfigUpdate{Enabled: test.enabled, MonitorAccountID: test.accountID, ExpectedRevision: test.old.Revision}, 9)
			require.NoError(t, err)
			require.Equal(t, test.old.Revision+1, saved.Revision)
			if test.wantNewEpoch {
				require.NotEqual(t, test.old.Epoch, saved.Epoch)
				require.NotNil(t, saved.EnabledAt)
			} else {
				require.Equal(t, test.old.Epoch, saved.Epoch)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
