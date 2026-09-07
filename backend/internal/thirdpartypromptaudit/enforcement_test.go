package thirdpartypromptaudit

import (
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestReauditAndResultOnlyRecoveryNeverPunish(t *testing.T) {
	config := testConfig()
	config.Disable = DisableConfig{Enabled: true, Limit: 1}
	config.Warning = WarningConfig{Enabled: true, Window: 3, Limit: 1}
	state := EnforcementState{WarningArmed: true}
	user := EnforcementUser{Role: "user", Status: "active"}
	for _, job := range []*Job{{RunKind: "reaudit"}, {RunKind: "request", ExecutionMode: "blocking"}} {
		outcome := &Outcome{Evaluation: Evaluation{Decision: DecisionBlock, EnforcementEligible: false}}
		next, action := decideEnforcement(state, user, job, outcome, config, []Decision{DecisionBlock})
		require.Empty(t, action)
		require.Equal(t, state, next)
	}
}

func TestManualEnableResetPreventsLateJobFromDisablingAgain(t *testing.T) {
	now := time.Now()
	config := testConfig()
	config.Disable = DisableConfig{Enabled: true, Limit: 1}
	state := EnforcementState{DisableResetAt: &now, WarningArmed: true}
	outcome := &Outcome{Evaluation: Evaluation{Decision: DecisionBlock, EnforcementEligible: true}}
	user := EnforcementUser{Role: "user", Status: "active"}
	old := &Job{RunKind: "request", CreatedAt: now.Add(-time.Second)}
	next, action := decideEnforcement(state, user, old, outcome, config, nil)
	require.Zero(t, next.DisableViolationCount)
	require.Empty(t, action)
	fresh := &Job{RunKind: "request", CreatedAt: now.Add(time.Second)}
	next, action = decideEnforcement(state, user, fresh, outcome, config, nil)
	require.Equal(t, int64(1), next.DisableViolationCount)
	require.Equal(t, "disable", action)
}

func TestWarningRearmsOnlyAfterWindowFallsBelowLimit(t *testing.T) {
	config := testConfig()
	config.Warning = WarningConfig{Enabled: true, Window: 3, Limit: 2}
	state := EnforcementState{WarningArmed: true}
	user := EnforcementUser{Role: "user", Status: "active"}
	job := &Job{RunKind: "request"}
	outcome := &Outcome{Evaluation: Evaluation{Decision: DecisionBlock, EnforcementEligible: true}}
	state, action := decideEnforcement(state, user, job, outcome, config, []Decision{DecisionBlock, DecisionBlock})
	require.Equal(t, "warning", action)
	state, action = decideEnforcement(state, user, job, outcome, config, []Decision{DecisionBlock, DecisionBlock, DecisionBlock})
	require.Empty(t, action)
	state, action = decideEnforcement(state, user, job, outcome, config, []Decision{DecisionPass, DecisionPass, DecisionBlock})
	require.Empty(t, action)
	require.True(t, state.WarningArmed)
	_, action = decideEnforcement(state, user, job, outcome, config, []Decision{DecisionBlock, DecisionPass, DecisionBlock})
	require.Equal(t, "warning", action)
}

// TestDelayedCaptureCannotReenterResetCounter 防止原文早已接收、Job 延迟建成的旧请求进入新累计。
func TestDelayedCaptureCannotReenterResetCounter(t *testing.T) {
	reset := time.Now()
	state := EnforcementState{DisableViolationCount: 4, DisableResetAt: &reset}
	job := &Job{RunKind: "request", CapturedAt: reset.Add(-time.Minute), CreatedAt: reset.Add(time.Minute)}
	outcome := &Outcome{Evaluation: Evaluation{Decision: DecisionBlock, EnforcementEligible: true}}
	cfg := Config{Disable: DisableConfig{Enabled: true, Limit: 4}}
	next, action := decideEnforcement(state, EnforcementUser{Role: "user", Status: "active"}, job, outcome, cfg, nil)
	if next.DisableViolationCount != 4 || action != "" {
		t.Fatalf("旧采集不得修改新累计或触发停用：count=%d action=%s", next.DisableViolationCount, action)
	}
}
