package quotafollow

import (
	"encoding/json"
	"errors"
	"github.com/stretchr/testify/require"
	"sub2api-enhance/internal/sub2api"
	"testing"
	"time"
)

func number(value string) *json.Number { v := json.Number(value); return &v }
func TestBaselineAndPercentageDropDoNotReset(t *testing.T) {
	now := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	next := now.Add(time.Hour)
	account := sub2api.QuotaAccount{ID: 7, Type: "oauth"}
	baseline := observe(AccountState{}, account, sub2api.AccountUsage{Utilization: number("80"), ResetsAt: &next}, now, now.Add(-time.Hour), nil)
	require.Nil(t, baseline.CandidateResetAt)
	dropped := observe(baseline, account, sub2api.AccountUsage{Utilization: number("0.123456789"), ResetsAt: &next}, now.Add(time.Minute), now.Add(-time.Hour), nil)
	require.True(t, dropped.SuspectedDrop)
	require.Nil(t, dropped.CandidateResetAt)
	require.Equal(t, "0.123456789", dropped.Utilization.String())
}
func TestStrongResetRequiresPassedBoundaryAndForwardProgress(t *testing.T) {
	boundary := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	later := boundary.Add(7 * 24 * time.Hour)
	a := sub2api.QuotaAccount{ID: 7}
	previous := AccountState{Account: a, NextResetAt: &boundary, Utilization: number("90")}
	for _, test := range []struct {
		name               string
		now, enabled, next time.Time
		last               *time.Time
		expect             bool
	}{{"到达并推进", boundary, boundary.Add(-time.Hour), later, nil, true}, {"尚未到达", boundary.Add(-time.Second), boundary.Add(-time.Hour), later, nil, false}, {"边界未推进", boundary, boundary.Add(-time.Hour), boundary, nil, false}, {"早于启用周期", boundary.Add(time.Hour), boundary.Add(time.Minute), later, nil, false}, {"已消费事件", boundary, boundary.Add(-time.Hour), later, &boundary, false}} {
		t.Run(test.name, func(t *testing.T) {
			state := observe(previous, a, sub2api.AccountUsage{Utilization: number("0"), ResetsAt: &test.next}, test.now, test.enabled, test.last)
			require.Equal(t, test.expect, state.CandidateResetAt != nil)
		})
	}
}
func TestConsensusRequiresEveryAccountWithinFiveMinutes(t *testing.T) {
	first := time.Now().UTC()
	edge := first.Add(consensusTolerance)
	tooLate := edge.Add(time.Nanosecond)
	boundary, err := consensus([]AccountState{{CandidateResetAt: &first}, {CandidateResetAt: &edge}})
	require.NoError(t, err)
	require.Equal(t, edge, *boundary)
	_, err = consensus([]AccountState{{CandidateResetAt: &first}, {CandidateResetAt: &tooLate}})
	require.Error(t, err)
	boundary, err = consensus([]AccountState{{CandidateResetAt: &first}, {}})
	require.NoError(t, err)
	require.Nil(t, boundary)
	_, err = consensus([]AccountState{{CandidateResetAt: &first}, {CandidateResetAt: &edge, Error: "查询失败"}})
	require.Error(t, err)
}
func TestConfigAndAccountChangesRebuildOnlyRequiredBaselines(t *testing.T) {
	original := DefaultConfig().Config
	group := int64(7)
	original.GroupID = &group
	original.Enabled = true
	next := original
	next.MinInterval = 11
	require.False(t, newEpochRequired(original, next))
	next.ObserveOnly = false
	require.True(t, newEpochRequired(original, next))
	accounts := []sub2api.QuotaAccount{{ID: 1, Type: "oauth"}, {ID: 2, Type: "oauth"}}
	require.Equal(t, accountSetHash(accounts), accountSetHash([]sub2api.QuotaAccount{accounts[1], accounts[0]}))
	changed := append([]sub2api.QuotaAccount{}, accounts...)
	changed[1].Type = "api_key"
	require.NotEqual(t, accountSetHash(accounts), accountSetHash(changed))
	require.False(t, DefaultConfig().Enabled)
	require.True(t, DefaultConfig().ObserveOnly)
}
func TestDeliveryClassificationNeverTreatsUncertainResultAsRetryable(t *testing.T) {
	for _, test := range []struct {
		status   int
		err      error
		expected string
	}{{200, nil, "succeeded"}, {200, errors.New("incomplete body"), "uncertain"}, {404, errors.New("missing"), "skipped"}, {403, errors.New("forbidden"), "failed"}, {500, errors.New("server error"), "uncertain"}, {0, errors.New("timeout"), "uncertain"}} {
		require.Equal(t, test.expected, deliveryStatus(sub2api.QuotaResetReply{HTTPStatus: test.status, Error: test.err}))
	}
}
func TestNaturalBoundaryUsesOriginalTimezoneAndMonday(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	observed := time.Date(2026, 9, 9, 10, 0, 0, 0, location)
	monday := time.Date(2026, 9, 7, 0, 0, 0, 0, location)
	require.True(t, naturalBoundary(monday, observed, "weekly", location))
	require.False(t, naturalBoundary(monday.Add(time.Hour), observed, "weekly", location))
	require.False(t, naturalBoundary(monday, observed, "weekly", nil))
}

func TestRegressedResetBoundaryRebuildsBaselineBeforeNextStableObservation(t *testing.T) {
	now := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	known := now.Add(time.Hour)
	regressed := now.Add(time.Minute)
	account := sub2api.QuotaAccount{ID: 7}
	state := observe(AccountState{NextResetAt: &known}, account, sub2api.AccountUsage{Utilization: number("0"), ResetsAt: &regressed}, now, now.Add(-time.Hour), nil)
	require.Empty(t, state.Error)
	require.Equal(t, regressed, *state.NextResetAt)
	require.True(t, state.BaselineRebased)
	require.Nil(t, state.CandidateResetAt)

	stable := observe(state, account, sub2api.AccountUsage{Utilization: number("0"), ResetsAt: &regressed}, now.Add(time.Minute), now.Add(-time.Hour), nil)
	require.Empty(t, stable.Error)
	require.False(t, stable.BaselineRebased)
	require.Nil(t, stable.CandidateResetAt)
}

func TestRegressedPastBoundaryDoesNotCreateCandidateWhenBaselineRecovers(t *testing.T) {
	now := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	known := now.Add(-time.Hour)
	regressed := now.Add(-2 * time.Hour)
	account := sub2api.QuotaAccount{ID: 7}
	rebased := observe(AccountState{NextResetAt: &known}, account, sub2api.AccountUsage{Utilization: number("0"), ResetsAt: &regressed}, now, now.Add(-3*time.Hour), nil)
	require.True(t, rebased.BaselineRebased)

	pastStable := observe(rebased, account, sub2api.AccountUsage{Utilization: number("0"), ResetsAt: &regressed}, now.Add(time.Minute), now.Add(-3*time.Hour), nil)
	require.True(t, pastStable.BaselineRebased)

	stableBoundary := now.Add(time.Hour)
	stable := observe(pastStable, account, sub2api.AccountUsage{Utilization: number("0"), ResetsAt: &stableBoundary}, now.Add(2*time.Minute), now.Add(-3*time.Hour), nil)
	require.False(t, stable.BaselineRebased)
	require.Nil(t, stable.CandidateResetAt)

	later := stableBoundary.Add(7 * 24 * time.Hour)
	next := observe(stable, account, sub2api.AccountUsage{Utilization: number("0"), ResetsAt: &later}, stableBoundary.Add(time.Minute), now.Add(-3*time.Hour), nil)
	require.Equal(t, stableBoundary, *next.CandidateResetAt)
}
