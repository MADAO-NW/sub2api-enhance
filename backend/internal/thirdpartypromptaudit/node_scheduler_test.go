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
