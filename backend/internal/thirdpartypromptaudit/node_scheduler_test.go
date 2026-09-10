package thirdpartypromptaudit

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNodeSchedulerSelectsTheLeastLoadedUnusedCandidate(t *testing.T) {
	scheduler := newNodeScheduler()
	models := []ModelConfig{{ID: "a", Enabled: true, MaxConcurrency: 2}, {ID: "b", Enabled: true, MaxConcurrency: 2}}
	scheduler.updateLimits(models)
	first, releaseFirst, err := scheduler.acquire(context.Background(), models[:1])
	require.NoError(t, err)
	require.Equal(t, "a", first.ID)
	second, releaseSecond, err := scheduler.acquire(context.Background(), models)
	require.NoError(t, err)
	require.Equal(t, "b", second.ID)
	releaseSecond()
	releaseFirst()
}

func TestNodeSchedulerWaitsForCapacityAndHonorsLiveDownsizing(t *testing.T) {
	scheduler := newNodeScheduler()
	model := ModelConfig{ID: "a", Enabled: true, MaxConcurrency: 2}
	scheduler.updateLimits([]ModelConfig{model})
	_, releaseFirst, err := scheduler.acquire(context.Background(), []ModelConfig{model})
	require.NoError(t, err)
	_, releaseSecond, err := scheduler.acquire(context.Background(), []ModelConfig{model})
	require.NoError(t, err)
	model.MaxConcurrency = 1
	scheduler.updateLimits([]ModelConfig{model})
	type acquireResult struct {
		release func()
		err     error
	}
	acquired := make(chan acquireResult, 1)
	go func() {
		_, release, acquireErr := scheduler.acquire(context.Background(), []ModelConfig{model})
		acquired <- acquireResult{release: release, err: acquireErr}
	}()
	releaseFirst()
	select {
	case <-acquired:
		t.Fatal("缩容后仍有一个旧占用时不应取得新并发位")
	case <-time.After(20 * time.Millisecond):
	}
	releaseSecond()
	select {
	case result := <-acquired:
		require.NoError(t, result.err)
		result.release()
	case <-time.After(time.Second):
		t.Fatal("节点容量释放后没有唤醒等待任务")
	}
}

func TestNodeSchedulerKeepsDeletedNodeUntilItsBoundEvaluationReleasesIt(t *testing.T) {
	scheduler := newNodeScheduler()
	model := ModelConfig{ID: "removed", Enabled: true, MaxConcurrency: 1}
	scheduler.updateLimits([]ModelConfig{model})
	_, release, err := scheduler.acquire(context.Background(), []ModelConfig{model})
	require.NoError(t, err)
	scheduler.updateLimits(nil)
	require.Contains(t, scheduler.states, model.ID)
	release()
	require.NotContains(t, scheduler.states, model.ID)
}

func TestNodeSchedulerPrefersHealthyNodeOverDegradedNode(t *testing.T) {
	scheduler := newNodeScheduler()
	models := []ModelConfig{{ID: "degraded", Enabled: true, MaxConcurrency: 1}, {ID: "healthy", Enabled: true, MaxConcurrency: 1}}
	scheduler.updateLimits(models)
	scheduler.observe("degraded", &AuditError{Code: "timeout", Stage: "model_request", Retryable: true}, time.Second)
	scheduler.observe("healthy", nil, 2*time.Second)

	chosen, release, _, err := scheduler.acquireWithPolicy(context.Background(), models, false)
	require.NoError(t, err)
	require.Equal(t, "healthy", chosen.ID)
	release()
}

func TestNodeSchedulerOpensCooldownAfterTwoTransientFailures(t *testing.T) {
	scheduler := newNodeScheduler()
	model := ModelConfig{ID: "node", Enabled: true, MaxConcurrency: 1}
	scheduler.updateLimits([]ModelConfig{model})
	failure := &AuditError{Code: "timeout", Stage: "model_request", Retryable: true}

	scheduler.observe(model.ID, failure, 100*time.Millisecond)
	require.Equal(t, nodeHealthDegraded, scheduler.states[model.ID].health)
	scheduler.observe(model.ID, failure, 200*time.Millisecond)
	state := scheduler.states[model.ID]
	require.Equal(t, nodeHealthCooldown, state.health)
	require.Equal(t, 2, state.consecutiveFailures)
	require.WithinDuration(t, time.Now().Add(15*time.Second), state.cooldownUntil, time.Second)
	require.InDelta(t, 120, *state.latencyEWMAMS, 0.1)

	_, _, _, err := scheduler.acquireWithPolicy(context.Background(), []ModelConfig{model}, false)
	var scheduleErr *nodeScheduleError
	require.ErrorAs(t, err, &scheduleErr)
	require.Equal(t, "temporarily_unhealthy", scheduleErr.Code)
}

func TestNodeSchedulerRateLimitUsesRetryAfterAndProbeRecovers(t *testing.T) {
	scheduler := newNodeScheduler()
	model := ModelConfig{ID: "node", Enabled: true, MaxConcurrency: 2}
	scheduler.updateLimits([]ModelConfig{model})
	scheduler.observe(model.ID, &AuditError{Code: "rate_limited", Stage: "model_request", RetryAfter: 40 * time.Millisecond}, time.Millisecond)
	require.Equal(t, nodeHealthCooldown, scheduler.states[model.ID].health)

	time.Sleep(50 * time.Millisecond)
	selected, release, ok := scheduler.dueProbe([]ModelConfig{model})
	require.True(t, ok)
	require.Equal(t, model.ID, selected.ID)
	require.Equal(t, nodeHealthHalfOpen, scheduler.states[model.ID].health)
	require.Equal(t, 1, scheduler.states[model.ID].active)
	_, _, _, err := scheduler.acquireWithPolicy(context.Background(), []ModelConfig{model}, false)
	var scheduleErr *nodeScheduleError
	require.ErrorAs(t, err, &scheduleErr)
	require.Equal(t, "temporarily_unhealthy", scheduleErr.Code)

	scheduler.observe(model.ID, nil, 10*time.Millisecond)
	release()
	require.Equal(t, nodeHealthHealthy, scheduler.states[model.ID].health)
	require.Equal(t, 0, scheduler.states[model.ID].active)
}

func TestNodeSchedulerMarksStableConfigurationFailureAsMisconfigured(t *testing.T) {
	scheduler := newNodeScheduler()
	model := ModelConfig{ID: "node", Enabled: true, MaxConcurrency: 1}
	scheduler.updateLimits([]ModelConfig{model})
	scheduler.observe(model.ID, &AuditError{Code: "upstream_http_error", Stage: "model_request", Message: "审核节点返回 HTTP 422"}, time.Millisecond)
	require.Equal(t, nodeHealthMisconfigured, scheduler.states[model.ID].health)

	_, _, _, err := scheduler.acquireWithPolicy(context.Background(), []ModelConfig{model}, false)
	var scheduleErr *nodeScheduleError
	require.ErrorAs(t, err, &scheduleErr)
	require.Equal(t, "no_healthy_nodes", scheduleErr.Code)
}

func TestNodeSchedulerIgnoresPersistenceAndPauseFailures(t *testing.T) {
	scheduler := newNodeScheduler()
	model := ModelConfig{ID: "node", Enabled: true, MaxConcurrency: 1}
	scheduler.updateLimits([]ModelConfig{model})
	for _, failure := range []*AuditError{{Code: "audit_paused", Stage: "result_persist"}, {Code: "attempt_result_persist_failed", Stage: "result_persist"}} {
		scheduler.observe(model.ID, failure, time.Second)
	}
	require.Equal(t, nodeHealthUnknown, scheduler.states[model.ID].health)
	require.Zero(t, scheduler.states[model.ID].consecutiveFailures)
}

func TestNodeSchedulerAsyncBackpressureDoesNotWait(t *testing.T) {
	scheduler := newNodeScheduler()
	model := ModelConfig{ID: "node", Enabled: true, MaxConcurrency: 1}
	scheduler.updateLimits([]ModelConfig{model})
	_, release, _, err := scheduler.acquireWithPolicy(context.Background(), []ModelConfig{model}, false)
	require.NoError(t, err)

	started := time.Now()
	_, _, _, err = scheduler.acquireWithPolicy(context.Background(), []ModelConfig{model}, false)
	var scheduleErr *nodeScheduleError
	require.ErrorAs(t, err, &scheduleErr)
	require.Equal(t, "capacity_saturated", scheduleErr.Code)
	require.Less(t, time.Since(started), 100*time.Millisecond)
	require.Equal(t, uint64(1), scheduler.runtime([]ModelConfig{model}).BackpressuredTotal)
	release()
}
