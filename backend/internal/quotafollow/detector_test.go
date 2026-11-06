package quotafollow

import (
	"encoding/json"
	"errors"
	"github.com/stretchr/testify/require"
	"sub2api-enhance/internal/sub2api"
	"testing"
	"time"
)

func number(v string) *json.Number { n := json.Number(v); return &n }
func weeklyQuota(at, reset time.Time, percent string) sub2api.AccountQuota {
	return sub2api.AccountQuota{FetchedAt: at.Unix(), Windows: &sub2api.QuotaWindows{Primary: &sub2api.QuotaWindow{UsedPercent: number(percent), Seconds: 604800, ResetAt: reset.Unix()}}}
}
func TestBaselineDropAndTimeAloneNeverReset(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(time.Hour)
	a := sub2api.SubscriptionAccount{ID: 1, Type: "oauth"}
	state := observe(AccountState{}, a, weeklyQuota(now, reset, "90"), nil, now, now)
	require.Nil(t, state.CandidateResetAt)
	dropped := observe(state, a, weeklyQuota(now.Add(time.Minute), reset, "0"), nil, now.Add(time.Minute), now)
	require.True(t, dropped.SuspectedDrop)
	require.Nil(t, dropped.CandidateResetAt)
	expired := observe(dropped, a, weeklyQuota(reset, reset, "0"), nil, reset, now)
	require.Nil(t, expired.CandidateResetAt)
	fresh := observe(expired, a, weeklyQuota(reset.Add(time.Second), reset.Add(weeklyPeriod), "3"), nil, reset.Add(time.Second), now)
	require.Equal(t, reset, *fresh.CandidateResetAt)
}
func TestStaggeredAccountsConsumeOneRoundWithoutBankingExtraResets(t *testing.T) {
	now := time.Now().UTC()
	a := now
	b := now.Add(3 * 24 * time.Hour)
	latestA := now.Add(2 * 24 * time.Hour)
	states := []AccountState{{CandidateResetAt: &a, FetchedAt: &b}, {FetchedAt: &b}}
	boundary, err := consensus(states)
	require.NoError(t, err)
	require.Nil(t, boundary)
	states[0].CandidateResetAt = &latestA
	states[1].CandidateResetAt = &b
	boundary, err = consensus(states)
	require.NoError(t, err)
	require.Equal(t, b, *boundary)
	consume(states)
	boundary, err = consensus(states)
	require.NoError(t, err)
	require.Nil(t, boundary)
	require.Equal(t, latestA, *states[0].ConsumedResetAt)
	require.Nil(t, states[0].CandidateResetAt)
	states[1].CandidateResetAt = timePointer(b.Add(weeklyPeriod))
	boundary, err = consensus(states)
	require.NoError(t, err)
	require.Nil(t, boundary)
}
func TestBadEvidencePreservesTrustedStateAndBlocksGroup(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(time.Hour)
	a := sub2api.SubscriptionAccount{ID: 1, Type: "oauth"}
	old := observe(AccountState{}, a, weeklyQuota(now, reset, "85"), nil, now, now)
	for _, test := range []struct {
		name string
		q    sub2api.AccountQuota
		err  error
	}{{"upstream error", weeklyQuota(now.Add(time.Minute), reset, "0"), errors.New("失败")}, {"old sample", weeklyQuota(now.Add(-time.Minute), reset, "0"), nil}, {"no windows", sub2api.AccountQuota{FetchedAt: now.Add(time.Minute).Unix()}, nil}, {"missing absolute reset", weeklyQuota(now.Add(time.Minute), time.Unix(0, 0), "0"), nil}} {
		t.Run(test.name, func(t *testing.T) {
			state := observe(old, a, test.q, test.err, now.Add(time.Minute), now)
			require.NotEmpty(t, state.Error)
			require.Equal(t, old.NextResetAt, state.NextResetAt)
			require.Equal(t, "85", state.Utilization.String())
			_, err := consensus([]AccountState{state})
			require.Error(t, err)
		})
	}
}
func TestRegressedBoundaryRequiresStableNewBaseline(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	oldReset := now.Add(time.Hour)
	earlier := now.Add(time.Minute)
	a := sub2api.SubscriptionAccount{ID: 1, Type: "oauth"}
	old := observe(AccountState{}, a, weeklyQuota(now, oldReset, "20"), nil, now, now)
	regressed := observe(old, a, weeklyQuota(now.Add(time.Second), earlier, "20"), nil, now.Add(time.Second), now)
	require.True(t, regressed.BaselineRebased)
	stable := observe(regressed, a, weeklyQuota(now.Add(2*time.Second), earlier, "20"), nil, now.Add(2*time.Second), now)
	require.False(t, stable.BaselineRebased)
	require.Nil(t, stable.CandidateResetAt)
}
func TestShadowUsesFeatureWindowAndMissingFeatureIsNotZero(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	a := sub2api.SubscriptionAccount{ID: 2, Type: "oauth", IsShadow: true}
	q := weeklyQuota(now, now.Add(time.Hour), "80")
	bad := observe(AccountState{}, a, q, nil, now, now)
	require.NotEmpty(t, bad.Error)
	require.Nil(t, bad.Utilization)
	require.NoError(t, json.Unmarshal([]byte(`{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":604800,"reset_at":`+jsonNumber(now.Add(time.Hour).Unix())+`}},"additional_rate_limits":[{"metered_feature":"codex_bengalfox","rate_limit":{"secondary_window":{"used_percent":12,"limit_window_seconds":604800,"reset_at":`+jsonNumber(now.Add(time.Hour).Unix())+`}}}],"fetched_at":`+jsonNumber(now.Unix())+`}`), &q))
	good := observe(AccountState{}, a, q, nil, now, now)
	require.Empty(t, good.Error)
	require.Equal(t, "12", good.Utilization.String())
}
func jsonNumber(v int64) string { raw, _ := json.Marshal(v); return string(raw) }
func TestChangingOneRuleDoesNotRebuildAnotherGroup(t *testing.T) {
	a := GroupRule{GroupID: 1, Enabled: true, ResetWeekly: true}
	b := a
	require.False(t, newEpochRequired(a, b))
	b.ResetDaily = true
	require.True(t, newEpochRequired(a, b))
	cfg := DefaultConfig()
	cfg.Groups = []GroupRule{a, a}
	require.Error(t, cfg.Validate())
	cfg.Groups[1].GroupID = 2
	require.NoError(t, cfg.Validate())
}
func TestNextDetectionChecksBoundaryAndBacksOffFailures(t *testing.T) {
	now := time.Now()
	cfg := DefaultConfig().Config
	boundary := now.Add(time.Minute)
	next := nextDetection(now, cfg, []AccountState{{NextResetAt: &boundary}}, false)
	require.Equal(t, boundary.Add(time.Second), next)
	require.Equal(t, now.Add(15*time.Minute), nextDetection(now, cfg, nil, true))
}

func TestConfigRequiresAnArrayButAllowsNoGroupsWhenDisabled(t *testing.T) {
	cfg := DefaultConfig().Config
	require.NoError(t, cfg.Validate())
	cfg.Groups = nil
	require.ErrorContains(t, cfg.Validate(), "数组")
	cfg.Groups = []GroupRule{}
	cfg.Enabled = true
	require.ErrorContains(t, cfg.Validate(), "添加一个分组")
}
