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

func TestCarryoverPreservesCurrentUsageOrAddsPreviousUsagePrecisely(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	week := time.Date(2026, 9, 7, 0, 0, 0, 0, location)
	oldStart := week.AddDate(0, 0, -7)
	snapshot := sub2api.QuotaSnapshot{WeeklyUsage: "10.1234567890", WeeklyStart: &oldStart, ObservedAt: week.Add(-time.Minute)}
	tests := []struct {
		name, usage string
		start       *time.Time
		want        string
	}{
		{"原版尚未懒重置，保留含新增消费的当前值", "12.1234567891", &oldStart, "12.1234567891"},
		{"原版已经清零，加回旧值且保留新消费", "0.0000000001", &week, "10.1234567891"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := carryoverAmount(snapshot, sub2api.QuotaSnapshot{WeeklyUsage: test.usage, WeeklyStart: test.start}, week, week.Add(time.Second), 15*time.Minute)
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
	t.Run("拒绝人工归零后的窗口", func(t *testing.T) {
		later := week.Add(time.Second)
		_, err := carryoverAmount(snapshot, sub2api.QuotaSnapshot{WeeklyUsage: "2", WeeklyStart: &later}, week, week.Add(time.Minute), 15*time.Minute)
		require.ErrorContains(t, err, "其他重置")
	})
	t.Run("缺失或过旧快照与超时补偿均失败关闭", func(t *testing.T) {
		current := sub2api.QuotaSnapshot{WeeklyUsage: "2", WeeklyStart: &week}
		stale := snapshot
		stale.ObservedAt = week.Add(-16 * time.Minute)
		_, err := carryoverAmount(stale, current, week, week.Add(time.Second), 15*time.Minute)
		require.ErrorContains(t, err, "快照")
		_, err = carryoverAmount(snapshot, current, week, week.Add(15*time.Minute), 15*time.Minute)
		require.ErrorContains(t, err, "安全处理窗口")
	})
	t.Run("未更新多周的历史窗口不能结转", func(t *testing.T) {
		stale := snapshot
		old := week.AddDate(0, 0, -14)
		stale.WeeklyStart = &old
		_, err := carryoverAmount(stale, sub2api.QuotaSnapshot{}, week, week, 15*time.Minute)
		require.ErrorContains(t, err, "上一自然周")
	})
	require.True(t, weekStart(week.Add(2*time.Hour).UTC(), location).Equal(week))
}

func TestCarryoverRequiresFreshAlignedAccountsAndExplicitEnvironment(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Epoch = "epoch"
	now := time.Now()
	boundary := now.Add(time.Hour)
	r := Runtime{Epoch: "epoch", Accounts: []sub2api.QuotaAccount{{ID: 1}}, States: []AccountState{{ObservedAt: &now, NextResetAt: &boundary}}}
	require.NoError(t, carryoverHealth(cfg, r, now))
	require.Error(t, carryoverHealth(cfg, r, now.Add(16*time.Minute)))
	r.States[0].ObservedAt = &now
	r.States[0].BaselineRebased = true
	require.ErrorContains(t, carryoverHealth(cfg, r, now), "基线正在重建")
	r.States[0].BaselineRebased = false
	r.States[0].Error = "upstream error"
	require.Error(t, carryoverHealth(cfg, r, now))
	require.ErrorContains(t, (&Service{}).carryoverPrerequisite(), "Flusher")
	require.ErrorContains(t, (&Service{flusherDisabled: true}).carryoverPrerequisite(), "时区")
}

func TestCarryoverAndEvidenceCommitAtomicallyWithoutCacheOrAPIWrite(t *testing.T) {
	for _, commitFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "提交成功", true: "提交未知"}[commitFails], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			week := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
			oldStart := week.AddDate(0, 0, -7)
			now := week.Add(time.Second)
			cfg := DefaultConfig()
			cfg.Enabled = true
			cfg.ObserveOnly = false
			cfg.Epoch = "epoch"
			group := int64(2)
			cfg.GroupID = &group
			cfgRaw, _ := json.Marshal(cfg)
			snapshotRaw, _ := json.Marshal(sub2api.QuotaSnapshot{WeeklyUsage: "10.0000000001", WeeklyStart: &oldStart, ObservedAt: week.Add(-time.Minute)})
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT value .* FOR SHARE").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(string(cfgRaw)))
			mock.ExpectQuery("INSERT INTO .*weekly_carryovers").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
			mock.ExpectQuery("SELECT snapshot FROM .*weekly_snapshots").WillReturnRows(sqlmock.NewRows([]string{"snapshot"}).AddRow(string(snapshotRaw)))
			mock.ExpectQuery("SELECT u.id.*FOR UPDATE OF q").WillReturnRows(sqlmock.NewRows([]string{"id", "username", "daily", "weekly", "daily_start", "weekly_start", "now"}).AddRow(7, "User", "1.0", "0.0000000001", week, week, now))
			mock.ExpectQuery("SELECT EXISTS.*reset_deliveries").WillReturnRows(sqlmock.NewRows([]string{"conflict"}).AddRow(false))
			mock.ExpectQuery("SELECT request_body FROM public.audit_logs").WillReturnRows(sqlmock.NewRows([]string{"body"}))
			mock.ExpectQuery("SELECT clock_timestamp").WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(now))
			mock.ExpectQuery("UPDATE public.user_platform_quotas SET weekly_usage_usd").WithArgs(int64(7), "10.0000000002", week).WillReturnRows(sqlmock.NewRows([]string{"usage", "start", "now"}).AddRow("10.0000000002", week, now))
			mock.ExpectExec("UPDATE .*weekly_carryovers SET snapshot").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec("INSERT INTO .*quota_reset_records").WillReturnResult(sqlmock.NewResult(0, 1))
			if commitFails {
				mock.ExpectCommit().WillReturnError(errors.New("commit unknown"))
			} else {
				mock.ExpectCommit()
			}
			cache := &repairTestCache{}
			api := &fakeAPI{}
			s := &Service{repo: NewRepository(db), cache: cache, api: api, source: carryTestSource{}}
			err = s.applyCarryover(context.Background(), cfg, Runtime{AccountSetHash: accountSetHash([]sub2api.QuotaAccount{{ID: 1}})}, week, sub2api.QuotaUser{ID: 7, Username: "User"})
			if commitFails {
				require.ErrorContains(t, err, "commit unknown")
			} else {
				require.NoError(t, err)
			}
			require.Zero(t, cache.writes)
			require.Zero(t, api.resets)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestExistingWeekCarryoverNeverAddsAgain(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.ObserveOnly = false
	group := int64(2)
	cfg.GroupID = &group
	raw, _ := json.Marshal(cfg)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT value").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(string(raw)))
	mock.ExpectQuery("INSERT INTO .*weekly_carryovers").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectRollback()
	s := &Service{repo: NewRepository(db), source: carryTestSource{}}
	require.NoError(t, s.applyCarryover(context.Background(), cfg, Runtime{AccountSetHash: accountSetHash([]sub2api.QuotaAccount{{ID: 1}})}, time.Now(), sub2api.QuotaUser{ID: 7}))
	require.NoError(t, mock.ExpectationsWereMet())
}

type repairTestCache struct {
	writes  int
	failure error
}

func (c *repairTestCache) Read(context.Context, int64) (map[string]string, error) {
	return map[string]string{}, nil
}
func (c *repairTestCache) Status() string                          { return "测试缓存" }
func (c *repairTestCache) Invalidate(context.Context, int64) error { c.writes++; return c.failure }

func TestCommittedCacheRecoveryDoesNotRepeatAmountsOrResetAPI(t *testing.T) {
	for _, failure := range []error{nil, errors.New("DEL 未确认")} {
		name := "success"
		if failure != nil {
			name = "retry_cache_only"
		}
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectQuery("SELECT 'carryover'.*UNION ALL SELECT 'delivery'").WillReturnRows(sqlmock.NewRows([]string{"kind", "id", "user_id", "group_id", "window"}).AddRow("carryover", 3, 7, 2, "weekly"))
			mock.ExpectBegin()
			mock.ExpectExec("UPDATE .*weekly_carryovers SET cache_status").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec("UPDATE .*quota_reset_records SET evidence").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			api := &fakeAPI{}
			cache := &repairTestCache{failure: failure}
			s := &Service{repo: NewRepository(db), flusherDisabled: true, cache: cache, api: api, source: fakeSource{&sub2api.QuotaSnapshot{UserID: 7}}}
			err = s.repairPendingCaches(context.Background())
			if failure == nil {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "DEL 未确认")
			}
			require.Equal(t, 1, cache.writes)
			require.Zero(t, api.resets)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

type carryTestSource struct{ fakeSource }

func (carryTestSource) Accounts(context.Context, int64) ([]sub2api.QuotaAccount, error) {
	return []sub2api.QuotaAccount{{ID: 1}}, nil
}
