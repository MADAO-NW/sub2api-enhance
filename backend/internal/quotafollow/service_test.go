package quotafollow

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"sub2api-enhance/internal/sub2api"
)

type fakeSource struct {
	mu             sync.Mutex
	monitorCalls   int
	subscriptions  []sub2api.SubscriptionSnapshot
	snapshots      map[int64]sub2api.SubscriptionSnapshot
	monitorAccount sub2api.MonitorAccount
	monitorErr     error
}

func (s *fakeSource) MonitorAccount(context.Context, int64) (sub2api.MonitorAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.monitorCalls++
	return s.monitorAccount, s.monitorErr
}
func (s *fakeSource) MonitorCandidates(context.Context) ([]sub2api.MonitorAccount, error) {
	return []sub2api.MonitorAccount{s.monitorAccount}, nil
}
func (s *fakeSource) ValidSubscriptions(context.Context) ([]sub2api.SubscriptionSnapshot, error) {
	return s.subscriptions, nil
}
func (s *fakeSource) ValidSubscriptionGroups(context.Context) ([]sub2api.SubscriptionGroupCount, error) {
	return []sub2api.SubscriptionGroupCount{{GroupID: 1, GroupName: "A", Count: len(s.subscriptions)}}, nil
}
func (s *fakeSource) SubscriptionSnapshots(context.Context, []int64) (map[int64]sub2api.SubscriptionSnapshot, error) {
	return s.snapshots, nil
}

type fakeAPI struct {
	mu         sync.Mutex
	quotaCalls int
	usage      sub2api.AccountQuota
	err        error
	resetCalls int
	reply      sub2api.SubscriptionResetReply
}

func (*fakeAPI) Configured() bool { return true }
func (a *fakeAPI) AccountQuota(context.Context, int64) (sub2api.AccountQuota, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.quotaCalls++
	return a.usage, a.err
}
func (a *fakeAPI) ResetSubscriptions(context.Context, []byte, string) sub2api.SubscriptionResetReply {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resetCalls++
	return a.reply
}

func expectFailureSave(mock sqlmock.Sqlmock, cfg SavedConfig) {
	raw, _ := json.Marshal(cfg)
	initial, _ := json.Marshal(Runtime{})
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT value FROM sub2api_enhance.settings").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
	mock.ExpectExec("INSERT INTO sub2api_enhance.quota_follow_monitor_runtime").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT snapshot FROM sub2api_enhance.quota_follow_monitor_runtime").WillReturnRows(sqlmock.NewRows([]string{"snapshot"}).AddRow(initial))
	mock.ExpectExec("UPDATE sub2api_enhance.quota_follow_monitor_runtime").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

func enabledConfig() SavedConfig {
	return SavedConfig{Config: Config{Enabled: true, MonitorAccountID: 7}, Epoch: "epoch", Revision: 3}
}

func TestDisabledModuleNeverTouchesMonitorAccountOrUpstream(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	raw, _ := json.Marshal(DefaultConfig())
	mock.ExpectQuery("SELECT value FROM sub2api_enhance.settings").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
	api := &fakeAPI{}
	source := &fakeSource{}
	require.NoError(t, (&Service{repo: NewRepository(db), api: api, source: source}).detect(context.Background()))
	require.Zero(t, source.monitorCalls)
	require.Zero(t, api.quotaCalls)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestInvalidMonitorAccountStopsBeforeQuotaRequest(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	cfg := enabledConfig()
	raw, _ := json.Marshal(cfg)
	mock.ExpectQuery("SELECT value FROM sub2api_enhance.settings").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
	mock.ExpectQuery("SELECT snapshot FROM sub2api_enhance.quota_follow_monitor_runtime").WillReturnRows(sqlmock.NewRows([]string{"snapshot"}))
	expectFailureSave(mock, cfg)
	api := &fakeAPI{}
	source := &fakeSource{monitorAccount: sub2api.MonitorAccount{ID: 7, Platform: "openai", Type: "oauth", Status: "active", Schedulable: true}}
	require.NoError(t, (&Service{repo: NewRepository(db), api: api, source: source}).detect(context.Background()))
	require.Zero(t, api.quotaCalls)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpstreamAuthorizationFailurePausesDetection(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	cfg := enabledConfig()
	raw, _ := json.Marshal(cfg)
	mock.ExpectQuery("SELECT value FROM sub2api_enhance.settings").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
	mock.ExpectQuery("SELECT snapshot FROM sub2api_enhance.quota_follow_monitor_runtime").WillReturnRows(sqlmock.NewRows([]string{"snapshot"}))
	expectFailureSave(mock, cfg)
	api := &fakeAPI{err: &sub2api.APIError{Status: 401, Body: `{"code":401,"message":"expired","reason":"OPENAI_QUOTA_UPSTREAM_ERROR"}`}}
	source := &fakeSource{monitorAccount: testAccount()}
	require.NoError(t, (&Service{repo: NewRepository(db), api: api, source: source}).detect(context.Background()))
	require.Equal(t, 1, api.quotaCalls)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPausedRevisionSkipsFurtherQueries(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	cfg := enabledConfig()
	raw, _ := json.Marshal(cfg)
	revision := cfg.Revision
	paused, _ := json.Marshal(Runtime{Epoch: cfg.Epoch, PausedRevision: &revision, LastError: "监控账号授权失效"})
	mock.ExpectQuery("SELECT value FROM sub2api_enhance.settings").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
	mock.ExpectQuery("SELECT snapshot FROM sub2api_enhance.quota_follow_monitor_runtime").WillReturnRows(sqlmock.NewRows([]string{"snapshot"}).AddRow(paused))
	api := &fakeAPI{}
	source := &fakeSource{monitorAccount: testAccount()}
	require.NoError(t, (&Service{repo: NewRepository(db), api: api, source: source}).detect(context.Background()))
	require.Zero(t, source.monitorCalls)
	require.Zero(t, api.quotaCalls)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDetectionBeforeNextCheckDoesNothing(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	cfg := enabledConfig()
	raw, _ := json.Marshal(cfg)
	next := time.Now().Add(time.Minute)
	waiting, _ := json.Marshal(Runtime{Epoch: cfg.Epoch, NextCheckAt: &next})
	mock.ExpectQuery("SELECT value FROM sub2api_enhance.settings").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
	mock.ExpectQuery("SELECT snapshot FROM sub2api_enhance.quota_follow_monitor_runtime").WillReturnRows(sqlmock.NewRows([]string{"snapshot"}).AddRow(waiting))
	api := &fakeAPI{}
	source := &fakeSource{monitorAccount: testAccount()}
	require.NoError(t, (&Service{repo: NewRepository(db), api: api, source: source}).detect(context.Background()))
	require.Zero(t, api.quotaCalls)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDiscoverySeparatesCandidatesFromSubscriptionTotals(t *testing.T) {
	source := &fakeSource{monitorAccount: testAccount(), subscriptions: []sub2api.SubscriptionSnapshot{{ID: 1}, {ID: 2}, {ID: 3}}}
	out, err := (&Service{source: source}).Discovery(context.Background())
	require.NoError(t, err)
	require.Len(t, out.MonitorAccounts, 1)
	require.True(t, out.MonitorAccounts[0].Valid)
	require.Equal(t, 3, out.SubscriptionCount)
	require.Equal(t, 1, len(out.GroupCounts))
}

func TestValidateMonitorRejectsIneligibleAccount(t *testing.T) {
	source := &fakeSource{monitorAccount: sub2api.MonitorAccount{ID: 7, Platform: "openai", Type: "oauth", Status: "active", GroupCount: 1}}
	require.Error(t, (&Service{source: source}).ValidateMonitor(context.Background(), 7))
	source.monitorAccount.GroupCount = 0
	require.NoError(t, (&Service{source: source}).ValidateMonitor(context.Background(), 7))
}

func TestQuotaFailureMessagesDistinguishCredentialProblems(t *testing.T) {
	message, paused := quotaQueryFailure(&sub2api.APIError{Status: 403, Body: `{"reason":"OPENAI_QUOTA_UPSTREAM_ERROR"}`}, 2)
	require.True(t, paused)
	require.Equal(t, "监控账号授权失效，请在 sub2api 重新授权后重新保存配置", message)

	message, paused = quotaQueryFailure(&sub2api.APIError{Status: 401, Body: `{"reason":"UNAUTHORIZED","message":"Invalid admin API key"}`}, 2)
	require.True(t, paused)
	require.Equal(t, "原版管理员 API Key 无效", message)

	// 账号已通过数据库校验时，原版 404 只可能来自管理密钥的账号范围。
	message, paused = quotaQueryFailure(&sub2api.APIError{Status: 404, Body: `{"code":404,"message":"账户不存在"}`, Method: "GET", Path: "/api/v1/admin/openai/accounts/2/quota"}, 2)
	require.False(t, paused)
	require.Contains(t, message, "#2")
	require.Contains(t, message, "管理员 API Key")
	require.Contains(t, message, "账户不存在")

	message, paused = quotaQueryFailure(&sub2api.APIError{Status: 502, Body: `{"reason":"OPENAI_QUOTA_UPSTREAM_ERROR"}`}, 2)
	require.False(t, paused)
	require.Contains(t, message, "HTTP 502")

	message, paused = quotaQueryFailure(context.Canceled, 2)
	require.False(t, paused)
	require.Equal(t, "context canceled", message)
}

func TestDetectionBacksOffWhenItCannotPersist(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT value FROM sub2api_enhance.settings").WillReturnError(errors.New("boom"))
	s := &Service{repo: NewRepository(db)}
	require.Error(t, s.detectWithBackoff(context.Background()))
	// 退避期内不再访问数据库，也不再消耗上游额度接口。
	require.NoError(t, s.detectWithBackoff(context.Background()))
	require.NoError(t, mock.ExpectationsWereMet())
}
