package quotafollow

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"sub2api-enhance/internal/sub2api"
	"testing"
	"time"
)

type baselineSource struct {
	discovery sub2api.QuotaDiscovery
}

func (s baselineSource) Accounts(context.Context, int64) ([]sub2api.QuotaAccount, error) {
	return s.discovery.Accounts, nil
}
func (s baselineSource) Discover(context.Context, int64) (sub2api.QuotaDiscovery, error) {
	return s.discovery, nil
}
func (baselineSource) Snapshot(context.Context, int64, int64) (*sub2api.QuotaSnapshot, error) {
	return nil, nil
}

type baselineAPI struct {
	usage map[int64]sub2api.AccountUsage
}

func (baselineAPI) Configured() bool { return true }
func (a baselineAPI) AccountUsageBatch(context.Context, []int64) (map[int64]sub2api.AccountUsage, error) {
	return a.usage, nil
}
func (baselineAPI) ResetQuota(context.Context, int64, string, string) sub2api.QuotaResetReply {
	return sub2api.QuotaResetReply{}
}

func TestDetectDoesNotReplayAccountAuditOnFirstBaseline(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	now := time.Date(2026, 9, 13, 6, 0, 0, 0, time.UTC)
	nextReset := now.Add(24 * time.Hour)
	groupID := int64(2)
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.GroupID = &groupID
	cfg.Epoch = "epoch"
	cfg.EnabledAt = &now
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)

	mock.ExpectQuery("SELECT value FROM sub2api_enhance.settings").WithArgs(settingKey).WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(string(raw)))
	mock.ExpectQuery("SELECT epoch,account_set_hash,accounts").WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT value .* FOR SHARE").WithArgs(settingKey).WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(string(raw)))
	mock.ExpectQuery("SELECT last_checked_at .* FOR UPDATE").WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("INSERT INTO sub2api_enhance.quota_follow_runtime").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO sub2api_enhance.quota_follow_account_states").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	service := &Service{
		repo:   NewRepository(db),
		source: baselineSource{discovery: sub2api.QuotaDiscovery{GroupID: groupID, Accounts: []sub2api.QuotaAccount{{ID: 1, Type: "oauth"}}}},
		api:    baselineAPI{usage: map[int64]sub2api.AccountUsage{1: {Utilization: number("0"), ResetsAt: &nextReset, UpdatedAt: &now}}},
	}
	require.NoError(t, service.detect(context.Background()))
	require.NoError(t, mock.ExpectationsWereMet())
}
