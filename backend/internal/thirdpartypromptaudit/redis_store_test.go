package thirdpartypromptaudit

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// newUnreachableClient 指向必然拒绝连接的本地端口，用于验证 Redis 故障语义。
func newUnreachableClient(t *testing.T) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond,
		ReadTimeout: 50 * time.Millisecond, WriteTimeout: 50 * time.Millisecond, MaxRetries: -1, Protocol: 2})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// Redis 不可用时读取回合键必须返回错误，由服务决定 async 放行或 blocking 不可用。
func TestTurnKeyReadFailsWhenRedisUnavailable(t *testing.T) {
	store := &RedisStore{}
	client := newUnreachableClient(t)
	store.client = client
	_, err := store.GetTurn(context.Background(), "turn-key")
	require.Error(t, err)
	require.False(t, store.Runtime().OK)
	require.Equal(t, "degraded", store.Runtime().Status)
}

// 回合键 TTL 固定 30 分钟（决策 24）。
func TestTurnKeyTTLIsFixed(t *testing.T) {
	require.Equal(t, 30*time.Minute, turnKeyTTL)
}

// 回合键由 API key 与回合指纹组成，不同用户或不同回合不会互相命中。
func TestTurnKeyIncludesAPIKeyAndFingerprint(t *testing.T) {
	require.Equal(t, "9:abc", turnKeyFor(9, "abc"))
	require.NotEqual(t, turnKeyFor(9, "abc"), turnKeyFor(10, "abc"))
	require.NotEqual(t, turnKeyFor(9, "abc"), turnKeyFor(9, "abd"))
}
