package quotafollow

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sub2api-enhance/internal/sub2api"
)

func number(v string) *json.Number { n := json.Number(v); return &n }

func weeklyQuota(fetched, reset time.Time, percent string) sub2api.AccountQuota {
	return sub2api.AccountQuota{AccountID: "acc-1", PlanType: "plus", FetchedAt: fetched.Unix(), Windows: &sub2api.QuotaWindows{Primary: &sub2api.QuotaWindow{UsedPercent: number(percent), Seconds: weeklyWindowSeconds, ResetAt: reset.Unix()}}}
}

func testConfig() SavedConfig {
	return SavedConfig{Config: Config{Enabled: true, MonitorAccountID: 7}, Epoch: "epoch", Revision: 3}
}

func testAccount() sub2api.MonitorAccount {
	return sub2api.MonitorAccount{ID: 7, Name: "monitor", Platform: "openai", Type: "oauth", Status: "active"}
}

func TestFirstTrustedSampleOnlyBuildsBaseline(t *testing.T) {
	now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	baseline := now.Add(48 * time.Hour)
	state, signal := evaluate(Runtime{}, testConfig(), testAccount(), weeklyQuota(now.Add(-time.Second), baseline, "42"), now)
	require.Nil(t, signal)
	require.Equal(t, baseline, *state.BaselineResetAt)
	require.Equal(t, "42", state.UsedPercent.String())
	require.Equal(t, "acc-1", state.UpstreamAccountID)
	require.Equal(t, "plus", state.PlanType)
	require.False(t, state.AwaitingWindow)
	require.Empty(t, state.LastError)
}

func TestSameWindowJitterOnlyFollowsLatestValue(t *testing.T) {
	now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	baseline := now.Add(24 * time.Hour)
	previous := Runtime{Epoch: "epoch", BaselineResetAt: &baseline, UsedPercent: number("30"), FetchedAt: timePointer(now.Add(-time.Minute))}
	shifted := baseline.Add(30 * time.Second)
	state, signal := evaluate(previous, testConfig(), testAccount(), weeklyQuota(now, shifted, "30"), now)
	require.Nil(t, signal)
	require.Equal(t, shifted, *state.BaselineResetAt)
}

func TestNaturalResetTriggersOnceWithOldWindowAsEventKey(t *testing.T) {
	now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	baseline := now.Add(-time.Hour)
	previous := Runtime{Epoch: "epoch", BaselineResetAt: &baseline, UsedPercent: number("78"), FetchedAt: timePointer(now.Add(-15 * time.Minute))}
	next := baseline.Add(weeklyPeriod)
	state, signal := evaluate(previous, testConfig(), testAccount(), weeklyQuota(now, next, "0"), now)
	require.NotNil(t, signal)
	require.Equal(t, baseline, signal.ResetAt)
	require.Equal(t, "automatic", signal.Trigger)
	require.Equal(t, automaticEventKey("epoch", baseline), signal.Key)
	require.Equal(t, next, *state.BaselineResetAt)
}

func TestDowntimeAcrossWeeksReportsOnlyTheLatestRound(t *testing.T) {
	now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	baseline := now.Add(-20 * 24 * time.Hour)
	previous := Runtime{Epoch: "epoch", BaselineResetAt: &baseline, UsedPercent: number("90"), FetchedAt: timePointer(now.Add(-30 * 24 * time.Hour))}
	next := now.Add(24 * time.Hour)
	_, signal := evaluate(previous, testConfig(), testAccount(), weeklyQuota(now, next, "0"), now)
	require.NotNil(t, signal)
	require.Equal(t, next.Add(-weeklyPeriod), signal.ResetAt)
	require.Equal(t, automaticEventKey("epoch", baseline), signal.Key)
}

func TestUnusedFloatingWindowNeverTriggersAndUsedWindowTriggersEarly(t *testing.T) {
	now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	baseline := now.Add(time.Hour)
	next := baseline.Add(weeklyPeriod)
	previous := Runtime{Epoch: "epoch", BaselineResetAt: &baseline, UsedPercent: number("0"), FetchedAt: timePointer(now.Add(-5 * time.Minute))}
	state, signal := evaluate(previous, testConfig(), testAccount(), weeklyQuota(now, next, "0"), now)
	require.Nil(t, signal)
	require.Equal(t, next, *state.BaselineResetAt)

	previous.UsedPercent = number("35")
	state, signal = evaluate(previous, testConfig(), testAccount(), weeklyQuota(now, next, "0"), now)
	require.NotNil(t, signal)
	require.Equal(t, now, signal.ResetAt)
	require.Equal(t, next, *state.BaselineResetAt)
}

func TestRegressedBoundaryRebuildsBaselineWithWarning(t *testing.T) {
	now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	baseline := now.Add(3 * time.Hour)
	earlier := baseline.Add(-2 * time.Hour)
	previous := Runtime{Epoch: "epoch", BaselineResetAt: &baseline, UsedPercent: number("20"), FetchedAt: timePointer(now.Add(-time.Minute))}
	state, signal := evaluate(previous, testConfig(), testAccount(), weeklyQuota(now, earlier, "20"), now)
	require.Nil(t, signal)
	require.Equal(t, earlier, *state.BaselineResetAt)
	require.Contains(t, state.LastError, "回退")
}

func TestUpstreamHasNotSwitchedWindowYet(t *testing.T) {
	now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	baseline := now.Add(time.Hour)
	previous := Runtime{Epoch: "epoch", BaselineResetAt: &baseline, UsedPercent: number("10"), FetchedAt: timePointer(now.Add(-time.Minute))}
	state, signal := evaluate(previous, testConfig(), testAccount(), weeklyQuota(now, now.Add(-time.Second), "10"), now)
	require.Nil(t, signal)
	require.Equal(t, baseline, *state.BaselineResetAt)
	require.Empty(t, state.LastError)
}

func TestMissingResetTimeFallsBackOnceAndThenWaitsForNewWindow(t *testing.T) {
	now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	baseline := now.Add(-time.Hour)
	previous := Runtime{Epoch: "epoch", BaselineResetAt: &baseline, UsedPercent: number("0"), FetchedAt: timePointer(now.Add(-time.Minute))}
	state, signal := evaluate(previous, testConfig(), testAccount(), weeklyQuota(now, time.Unix(0, 0), "0"), now)
	require.NotNil(t, signal)
	require.Equal(t, baseline, signal.ResetAt)
	require.True(t, state.AwaitingWindow)
	require.Equal(t, baseline, *state.BaselineResetAt)

	next := now.Add(36 * time.Hour)
	state, signal = evaluate(state, testConfig(), testAccount(), weeklyQuota(now.Add(time.Minute), next, "5"), now.Add(time.Minute))
	require.Nil(t, signal)
	require.False(t, state.AwaitingWindow)
	require.Equal(t, next, *state.BaselineResetAt)

	// 同一窗口不会再次触发兜底事件。
	state, signal = evaluate(state, testConfig(), testAccount(), weeklyQuota(now.Add(2*time.Minute), time.Unix(0, 0), "0"), now.Add(2*time.Minute))
	require.Nil(t, signal)
	require.Equal(t, next, *state.BaselineResetAt)
	require.Contains(t, state.LastError, "绝对重置时间")
}

func TestMissingResetTimeWithoutZeroUsageOnlyRecordsError(t *testing.T) {
	now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	baseline := now.Add(time.Hour)
	previous := Runtime{Epoch: "epoch", BaselineResetAt: &baseline, UsedPercent: number("12"), FetchedAt: timePointer(now.Add(-time.Minute))}
	state, signal := evaluate(previous, testConfig(), testAccount(), weeklyQuota(now, time.Unix(0, 0), "50"), now)
	require.Nil(t, signal)
	require.Equal(t, baseline, *state.BaselineResetAt)
	require.Contains(t, state.LastError, "绝对重置时间")
}

func TestUpstreamIdentityChangeOnlyRebuildsBaseline(t *testing.T) {
	now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	baseline := now.Add(time.Hour)
	previous := Runtime{Epoch: "epoch", UpstreamAccountID: "acc-1", PlanType: "plus", BaselineResetAt: &baseline, UsedPercent: number("88"), FetchedAt: timePointer(now.Add(-time.Minute))}
	quota := weeklyQuota(now, baseline.Add(weeklyPeriod), "0")
	quota.AccountID, quota.PlanType = "acc-2", "pro"
	state, signal := evaluate(previous, testConfig(), testAccount(), quota, now)
	require.Nil(t, signal)
	require.Equal(t, "acc-2", state.UpstreamAccountID)
	require.Equal(t, "pro", state.PlanType)
	require.Equal(t, baseline.Add(weeklyPeriod), *state.BaselineResetAt)
}

func TestEpochChangeDropsPreviousBaseline(t *testing.T) {
	now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	baseline := now.Add(-time.Hour)
	previous := Runtime{Epoch: "old", BaselineResetAt: &baseline, UsedPercent: number("90"), FetchedAt: timePointer(now.Add(-time.Minute))}
	next := now.Add(weeklyPeriod)
	state, signal := evaluate(previous, testConfig(), testAccount(), weeklyQuota(now, next, "0"), now)
	require.Nil(t, signal)
	require.Equal(t, next, *state.BaselineResetAt)
}

func TestUntrustedSamplesNeverBecomeEvidence(t *testing.T) {
	now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	baseline := now.Add(time.Hour)
	previous := Runtime{Epoch: "epoch", BaselineResetAt: &baseline, UsedPercent: number("60"), FetchedAt: timePointer(now.Add(-time.Minute))}
	bothWindows := weeklyQuota(now, baseline.Add(weeklyPeriod), "0")
	bothWindows.Windows.Secondary = bothWindows.Windows.Primary
	for name, quota := range map[string]sub2api.AccountQuota{
		"no weekly window":  {FetchedAt: now.Unix(), Windows: &sub2api.QuotaWindows{Primary: &sub2api.QuotaWindow{UsedPercent: number("0"), Seconds: 86400, ResetAt: now.Add(time.Hour).Unix()}}},
		"no windows":        {FetchedAt: now.Unix()},
		"stale fetch":       weeklyQuota(now.Add(-2*time.Minute), baseline.Add(weeklyPeriod), "0"),
		"future fetch":      weeklyQuota(now.Add(2*time.Minute), baseline.Add(weeklyPeriod), "0"),
		"duplicate windows": bothWindows,
	} {
		t.Run(name, func(t *testing.T) {
			state, signal := evaluate(previous, testConfig(), testAccount(), quota, now)
			require.Nil(t, signal)
			require.NotEmpty(t, state.LastError)
			require.Equal(t, baseline, *state.BaselineResetAt)
			require.Equal(t, "60", state.UsedPercent.String())
		})
	}
}

func TestSampleTimeMustAdvance(t *testing.T) {
	now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	baseline := now.Add(time.Hour)
	sampled := now.Add(-30 * time.Second)
	previous := Runtime{Epoch: "epoch", BaselineResetAt: &baseline, UsedPercent: number("60"), FetchedAt: &sampled}
	state, signal := evaluate(previous, testConfig(), testAccount(), weeklyQuota(sampled, baseline, "60"), now)
	require.Nil(t, signal)
	require.Contains(t, state.LastError, "没有前进")
}

func TestNextCheckPrefersWindowEdgeAndBacksOffFailures(t *testing.T) {
	now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	next := nextCheck(now, nil, false)
	require.False(t, next.Before(now.Add(detectionMinInterval)))
	require.False(t, next.After(now.Add(detectionMaxInterval)))
	boundary := now.Add(2 * time.Minute)
	require.Equal(t, boundary.Add(time.Second), nextCheck(now, &boundary, false))
	require.Equal(t, now.Add(detectionRetryInterval), nextCheck(now, &boundary, true))
}

func TestMonitorProblemsCoverEveryRequiredField(t *testing.T) {
	require.Empty(t, monitorProblems(testAccount()))
	parent := int64(9)
	for name, account := range map[string]sub2api.MonitorAccount{
		"platform":    {ID: 1, Platform: "anthropic", Type: "oauth", Status: "active"},
		"type":        {ID: 1, Platform: "openai", Type: "apikey", Status: "active"},
		"shadow":      {ID: 1, Platform: "openai", Type: "oauth", Status: "active", ParentID: &parent},
		"deleted":     {ID: 1, Platform: "openai", Type: "oauth", Status: "active", Deleted: true},
		"status":      {ID: 1, Platform: "openai", Type: "oauth", Status: "disabled"},
		"schedulable": {ID: 1, Platform: "openai", Type: "oauth", Status: "active", Schedulable: true},
		"grouped":     {ID: 1, Platform: "openai", Type: "oauth", Status: "active", GroupCount: 2},
	} {
		t.Run(name, func(t *testing.T) { require.NotEmpty(t, monitorProblems(account)) })
	}
}

func TestResumedDetectionClearsPausedRevision(t *testing.T) {
	now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	baseline := now.Add(24 * time.Hour)
	oldRevision := int64(2)
	previous := Runtime{Epoch: "epoch", BaselineResetAt: &baseline, UsedPercent: number("10"), FetchedAt: timePointer(now.Add(-time.Minute)), PausedRevision: &oldRevision, LastError: "监控账号授权失效"}
	state, signal := evaluate(previous, testConfig(), testAccount(), weeklyQuota(now, baseline, "10"), now)
	require.Nil(t, signal)
	require.Nil(t, state.PausedRevision)
	require.Empty(t, state.LastError)
	require.Equal(t, baseline, *state.BaselineResetAt)
}
