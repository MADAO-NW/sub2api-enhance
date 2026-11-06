package thirdpartypromptaudit

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisKeyPrefix 隔离增强审计回合键与原版 Sub2API 业务键。
const redisKeyPrefix = "sub2api_enhance:tppa:v2:"

// turnKeyTTL 是回合键与 previous_response_id 映射的固定有效期，命中时刷新（决策 24）。
const turnKeyTTL = 30 * time.Minute

// RedisRuntime 是运行态页面展示的 Redis 可用性。
type RedisRuntime struct {
	OK          bool       `json:"ok"`
	Status      string     `json:"status"`
	Error       string     `json:"error"`
	TTLSeconds  int        `json:"ttl_seconds"`
	LastSuccess *time.Time `json:"last_success"`
}

// RedisStore 保存可丢失重建的回合键和响应映射，不承担业务事实持久化。
type RedisStore struct {
	client *redis.Client
	mu     sync.RWMutex
	state  RedisRuntime
}

// turnState 是回合键的值：对应任务、当前状态和裁决。
type turnState struct {
	JobID    int64  `json:"job_id"`
	Status   string `json:"status"`
	Decision string `json:"decision,omitempty"`
}

func NewRedisStore(ctx context.Context, rawURL string) (*RedisStore, error) {
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, errors.New("ENHANCE_REDIS_URL 无效")
	}
	options.Protocol = 2
	options.DisableIdentity = true
	options.MaxRetries = -1
	options.ContextTimeoutEnabled = true
	store := &RedisStore{client: redis.NewClient(options), state: RedisRuntime{Status: "connecting", TTLSeconds: int(turnKeyTTL.Seconds())}}
	if err := store.client.Ping(ctx).Err(); err != nil {
		_ = store.client.Close()
		return nil, errors.New("增强 Redis 连接不可用")
	}
	store.noteSuccess()
	return store, nil
}

func (s *RedisStore) Close() error { return s.client.Close() }

func (s *RedisStore) Runtime() RedisRuntime {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *RedisStore) noteSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	s.state.OK, s.state.Status, s.state.Error, s.state.LastSuccess = true, "ok", "", &now
}

func (s *RedisStore) noteError() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.OK, s.state.Status, s.state.Error = false, "degraded", "增强 Redis 暂时不可用"
}

// GetTurn 读取回合键并在命中时刷新有效期（滑动过期）。
func (s *RedisStore) GetTurn(ctx context.Context, key string) (*turnState, error) {
	raw, err := s.client.Get(ctx, redisKeyPrefix+"turn:"+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		s.noteError()
		return nil, err
	}
	var state turnState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	s.noteSuccess()
	if err := s.client.Expire(ctx, redisKeyPrefix+"turn:"+key, turnKeyTTL).Err(); err != nil {
		s.noteError()
	}
	return &state, nil
}

// SaveTurn 写入回合键，值为任务状态与裁决。
func (s *RedisStore) SaveTurn(ctx context.Context, key string, state turnState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := s.client.Set(ctx, redisKeyPrefix+"turn:"+key, raw, turnKeyTTL).Err(); err != nil {
		s.noteError()
		return err
	}
	s.noteSuccess()
	return nil
}

// GetResponseTurn 按 previous_response_id 查回合键，命中时刷新有效期。
func (s *RedisStore) GetResponseTurn(ctx context.Context, responseID string) (string, error) {
	key := redisKeyPrefix + "response:" + responseID
	value, err := s.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	if err != nil {
		s.noteError()
		return "", err
	}
	s.noteSuccess()
	if err := s.client.Expire(ctx, key, turnKeyTTL).Err(); err != nil {
		s.noteError()
	}
	return value, nil
}

// SaveResponseTurn 登记 previous_response_id 到回合键的映射。
func (s *RedisStore) SaveResponseTurn(ctx context.Context, responseID, turnKey string) error {
	if err := s.client.Set(ctx, redisKeyPrefix+"response:"+responseID, turnKey, turnKeyTTL).Err(); err != nil {
		s.noteError()
		return err
	}
	s.noteSuccess()
	return nil
}
