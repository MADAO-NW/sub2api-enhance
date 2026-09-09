package thirdpartypromptaudit

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestIneligibleAndAdministratorResultsDoNotCreateAccountActions(t *testing.T) {
	config := testConfig()
	config.Disable = DisableConfig{Enabled: true, Limit: 1}
	config.Warning = WarningConfig{Enabled: true, Window: 3, Limit: 1}
	state := EnforcementState{WarningArmed: true, DisableViolationCount: 1}
	for _, input := range []struct {
		eligible bool
		role     string
	}{{false, "user"}, {true, "admin"}} {
		next, actions := decideEnforcement(state, EnforcementUser{Role: input.role, Status: "active"}, input.eligible, true, config, []Decision{DecisionBlock})
		require.Empty(t, actions)
		require.Equal(t, state, next)
	}
}

func TestOneViolationCanCreateWarningAndDisableActions(t *testing.T) {
	config := testConfig()
	config.Disable = DisableConfig{Enabled: true, Limit: 1}
	config.Warning = WarningConfig{Enabled: true, Window: 3, Limit: 1}
	state := EnforcementState{WarningArmed: true, DisableViolationCount: 1}
	next, actions := decideEnforcement(state, EnforcementUser{Role: "user", Status: "active"}, true, true, config, []Decision{DecisionBlock})
	require.Equal(t, []string{"warning", "disable"}, actions)
	require.False(t, next.WarningArmed)
}

func TestWarningRearmsOnlyAfterWindowFallsBelowLimit(t *testing.T) {
	config := testConfig()
	config.Warning = WarningConfig{Enabled: true, Window: 3, Limit: 2}
	state := EnforcementState{WarningArmed: true}
	user := EnforcementUser{Role: "user", Status: "active"}
	state, actions := decideEnforcement(state, user, true, true, config, []Decision{DecisionBlock, DecisionBlock})
	require.Equal(t, []string{"warning"}, actions)
	state, actions = decideEnforcement(state, user, true, true, config, []Decision{DecisionBlock, DecisionBlock, DecisionBlock})
	require.Empty(t, actions)
	state, actions = decideEnforcement(state, user, true, false, config, []Decision{DecisionPass, DecisionPass, DecisionBlock})
	require.Empty(t, actions)
	require.True(t, state.WarningArmed)
	_, actions = decideEnforcement(state, user, true, true, config, []Decision{DecisionBlock, DecisionPass, DecisionBlock})
	require.Equal(t, []string{"warning"}, actions)
}

func TestBlockingNoticeRecipientsAreIndependentAndDeduplicated(t *testing.T) {
	outcomeID := int64(9)
	action := Action{ActionType: "blocking_notice", OutcomeID: &outcomeID}
	user := EnforcementUser{Username: "user", Email: "user@example.invalid"}
	deliveries := actionDeliveries(action, user, "admin@example.invalid")
	require.Len(t, deliveries, 2)
	require.Equal(t, "admin", deliveries[0].Kind)
	require.Equal(t, "user", deliveries[1].Kind)
	require.Contains(t, deliveries[0].Subject, "请求已阻止")
	require.Len(t, actionDeliveries(action, user, "user@example.invalid"), 1)
	require.Len(t, actionDeliveries(action, user, ""), 1)
}
