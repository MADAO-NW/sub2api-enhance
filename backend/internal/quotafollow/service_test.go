package quotafollow

import (
	"context"
	"encoding/json"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"sub2api-enhance/internal/sub2api"
	"sync"
	"testing"
	"time"
)

type fakeSource struct {
	discoveries map[int64]sub2api.SubscriptionDiscovery
	snapshots   map[int64]sub2api.SubscriptionSnapshot
}

func (s fakeSource) DiscoverSubscriptions(_ context.Context, id int64) (sub2api.SubscriptionDiscovery, error) {
	return s.discoveries[id], nil
}
func (s fakeSource) SubscriptionSnapshots(context.Context, int64, []int64) (map[int64]sub2api.SubscriptionSnapshot, error) {
	return s.snapshots, nil
}

type fakeAPI struct {
	mu         sync.Mutex
	calls      []int64
	resetCalls int
	usage      sub2api.AccountQuota
	reply      sub2api.SubscriptionResetReply
}

func (*fakeAPI) Configured() bool { return true }
func (a *fakeAPI) AccountQuota(_ context.Context, id int64) (sub2api.AccountQuota, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, id)
	return a.usage, nil
}
func (a *fakeAPI) ResetSubscriptions(context.Context, []byte, string) sub2api.SubscriptionResetReply {
	a.resetCalls++
	return a.reply
}
func TestSharedAccountSampledOnceAndNoResetOnFirstBaseline(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	now := time.Now().UTC().Truncate(time.Second)
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Groups = []GroupRule{{GroupID: 1, Enabled: true, ResetWeekly: true, Epoch: "a"}, {GroupID: 2, Enabled: true, ResetWeekly: true, Epoch: "b"}}
	raw, _ := json.Marshal(cfg)
	mock.ExpectQuery("SELECT value").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
	for range cfg.Groups {
		mock.ExpectQuery("SELECT snapshot").WillReturnRows(sqlmock.NewRows([]string{"snapshot"}))
	}
	for _, g := range cfg.Groups {
		mock.ExpectBegin()
		mock.ExpectQuery("SELECT value .* FOR SHARE").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
		mock.ExpectExec("INSERT INTO .*quota_follow_group_runtime").WillReturnResult(sqlmock.NewResult(0, 1))
		previous, _ := json.Marshal(Runtime{GroupID: g.GroupID, States: []AccountState{}})
		mock.ExpectQuery("SELECT snapshot .* FOR UPDATE").WillReturnRows(sqlmock.NewRows([]string{"snapshot"}).AddRow(previous))
		mock.ExpectExec("UPDATE .*quota_follow_group_runtime").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
	}
	api := &fakeAPI{usage: weeklyQuota(now, now.Add(time.Hour), "20")}
	account := sub2api.SubscriptionAccount{ID: 7, Type: "oauth"}
	service := &Service{repo: NewRepository(db), api: api, source: fakeSource{discoveries: map[int64]sub2api.SubscriptionDiscovery{1: {GroupID: 1, Accounts: []sub2api.SubscriptionAccount{account}}, 2: {GroupID: 2, Accounts: []sub2api.SubscriptionAccount{account}}}}}
	require.NoError(t, service.detect(context.Background()))
	require.Equal(t, []int64{7}, api.calls)
	require.Zero(t, api.resetCalls)
	require.NoError(t, mock.ExpectationsWereMet())
}
func TestDisabledModuleDoesNotQueryAccounts(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	raw, _ := json.Marshal(DefaultConfig())
	mock.ExpectQuery("SELECT value").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
	api := &fakeAPI{}
	s := &Service{repo: NewRepository(db), api: api}
	require.NoError(t, s.detect(context.Background()))
	require.Empty(t, api.calls)
	require.NoError(t, mock.ExpectationsWereMet())
}
