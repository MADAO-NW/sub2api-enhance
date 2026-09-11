package thirdpartypromptaudit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRedisStoreRejectsMissingTTLBeforeConnecting(t *testing.T) {
	_, err := NewRedisStore(context.Background(), "redis://127.0.0.1:6379/14", 0)
	require.ErrorContains(t, err, "有效期")
}

func TestWholeCacheValueDoesNotContainSourceIdentity(t *testing.T) {
	value := wholeCacheValue{ID: 9, Models: []ModelResult{{ModelID: "node-a", Reason: "正常"}}}
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "user_id")
	require.NotContains(t, string(raw), "job_id")
	require.NotContains(t, string(raw), "config_snapshot")
}

func TestStatsCacheKeyCoversCompleteQuery(t *testing.T) {
	query := StatsQuery{From: time.Unix(1, 0).UTC(), To: time.Unix(2, 0).UTC(), Timezone: "Asia/Shanghai", Mode: "async", ModelID: "node-a", Stage: "current_user"}
	first, err := statsCacheKey(query)
	require.NoError(t, err)
	second, err := statsCacheKey(query)
	require.NoError(t, err)
	require.Equal(t, first, second)
	query.Stage = "intent_binding"
	third, err := statsCacheKey(query)
	require.NoError(t, err)
	require.NotEqual(t, first, third)
}
