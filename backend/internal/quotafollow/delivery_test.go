package quotafollow

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"sub2api-enhance/internal/sub2api"
	"testing"
	"time"
)

type fakeSource struct{ after *sub2api.QuotaSnapshot }

func (f fakeSource) Discover(context.Context, int64) (sub2api.QuotaDiscovery, error) {
	return sub2api.QuotaDiscovery{}, nil
}
func (f fakeSource) Accounts(context.Context, int64) ([]sub2api.QuotaAccount, error) { return nil, nil }
func (f fakeSource) Snapshot(context.Context, int64, int64) (*sub2api.QuotaSnapshot, error) {
	return f.after, nil
}

type fakeAPI struct{ resets int }

func (*fakeAPI) Configured() bool { return true }
func (*fakeAPI) AccountUsageBatch(context.Context, []int64) (map[int64]sub2api.AccountUsage, error) {
	return nil, nil
}
func (f *fakeAPI) ResetQuota(context.Context, int64, string, string) sub2api.QuotaResetReply {
	f.resets++
	return sub2api.QuotaResetReply{Error: errors.New("timeout")}
}

type fakeCache struct{ raw map[string]string }

func (f fakeCache) Read(context.Context, int64) (map[string]string, error) { return f.raw, nil }
func (fakeCache) Status() string                                           { return "测试只读缓存" }

func TestRecoveryUpdatesEvidenceAndEventEvenWhenDisabledWithoutResending(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectExec("UPDATE sub2api_enhance.quota_follow_reset_deliveries SET status='uncertain'").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_reset_records r SET status=d.status").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_follow_reset_events e SET status=CASE").WillReturnResult(sqlmock.NewResult(0, 1))
	raw, _ := json.Marshal(DefaultConfig())
	mock.ExpectQuery("SELECT value FROM sub2api_enhance.settings").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(string(raw)))
	api := &fakeAPI{}
	s := &Service{repo: NewRepository(db), api: api}
	require.NoError(t, s.deliver(context.Background()))
	require.Zero(t, api.resets)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCommitUncertaintyPreventsSendingReset(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.ObserveOnly = false
	cfg.Epoch = "epoch"
	raw, _ := json.Marshal(cfg)
	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT value .* FOR SHARE").WithArgs(settingKey).WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(string(raw)))
	mock.ExpectQuery("UPDATE .* SET status='inflight'").WillReturnRows(sqlmock.NewRows([]string{"started_at"}).AddRow(now))
	mock.ExpectExec("UPDATE .* SET status='inflight'").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit().WillReturnError(errors.New("commit outcome unknown"))
	sent, err := NewRepository(db).MarkInflight(context.Background(), &Delivery{ID: 1}, cfg)
	require.Error(t, err)
	require.False(t, sent)
	require.NoError(t, mock.ExpectationsWereMet())
}
func TestReadbackOfStaleCacheDoesNotResetAgain(t *testing.T) {
	now := time.Now().UTC()
	after := now.Add(time.Second)
	api := &fakeAPI{}
	service := &Service{api: api, source: fakeSource{&sub2api.QuotaSnapshot{UserID: 7, WeeklyUsage: "0.0000000001", WeeklyStart: &after}}, cache: fakeCache{map[string]string{"weekly_window_start": "1", "weekly_usage": "99.0000000001", "daily_usage": "0"}}}
	delivery := &Delivery{UserID: 7, Window: "weekly", StartedAt: &now, Status: "uncertain"}
	service.verifyDelivery(context.Background(), delivery, 1)
	require.Equal(t, "window_advanced", delivery.DatabaseStatus)
	require.Equal(t, "stale", delivery.CacheStatus)
	require.Equal(t, "uncertain", delivery.Status)
	require.Zero(t, api.resets)
}
func TestCacheValidationKeepsAmountPrecisionAndRejectsMalformedWindows(t *testing.T) {
	require.NoError(t, cacheCompatible(map[string]string{"schema_version": "1", "daily_usage": "0.0000000001", "weekly_usage": "9999999999.1234567890", "weekly_window_start": "1720000000"}))
	require.Error(t, cacheCompatible(map[string]string{"schema_version": "1", "daily_usage": "bad", "weekly_usage": "1"}))
	require.Error(t, cacheCompatible(map[string]string{"schema_version": "1", "daily_usage": "0", "weekly_usage": "1", "daily_window_start": "today"}))
}
