package thirdpartypromptaudit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisKeyPrefix 隔离增强审计投影与原版 Sub2API 业务键。
const redisKeyPrefix = "sub2api_enhance:tppa:v1:"

type RedisRuntime struct {
	OK                   bool       `json:"ok"`
	Status               string     `json:"status"`
	Error                string     `json:"error"`
	ProjectionVersion    string     `json:"projection_version"`
	ProjectionAsOf       *time.Time `json:"projection_as_of"`
	ProjectionRebuilding bool       `json:"projection_rebuilding"`
}

// RedisStore 保存可丢失重建的统计投影和审核热缓存，不承担业务事实持久化。
type RedisStore struct {
	client *redis.Client
	ttl    time.Duration
	mu     sync.RWMutex
	state  RedisRuntime
}

type wholeCacheValue struct {
	ID     int64         `json:"id"`
	Models []ModelResult `json:"models"`
}

func NewRedisStore(ctx context.Context, rawURL string, ttl time.Duration) (*RedisStore, error) {
	if ttl <= 0 {
		return nil, errors.New("审核热缓存有效期无效")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, errors.New("ENHANCE_REDIS_URL 无效")
	}
	options.Protocol = 2
	options.DisableIdentity = true
	options.MaxRetries = -1
	options.ContextTimeoutEnabled = true
	store := &RedisStore{client: redis.NewClient(options), ttl: ttl,
		state: RedisRuntime{Status: "connecting", ProjectionVersion: "v1", ProjectionRebuilding: true}}
	if err := store.client.Ping(ctx).Err(); err != nil {
		_ = store.client.Close()
		return nil, errors.New("增强 Redis 连接不可用")
	}
	store.noteSuccess(nil, true)
	return store, nil
}

func (s *RedisStore) Close() error { return s.client.Close() }

func (s *RedisStore) Runtime() RedisRuntime {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *RedisStore) noteSuccess(asOf *time.Time, rebuilding bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.OK, s.state.Status, s.state.Error = true, "ok", ""
	s.state.ProjectionRebuilding = rebuilding
	if asOf != nil {
		value := asOf.UTC()
		s.state.ProjectionAsOf = &value
	}
}

func (s *RedisStore) noteError(_ error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.OK, s.state.Status = false, "degraded"
	s.state.Error = "增强 Redis 暂时不可用"
}

func (s *RedisStore) setProjectionRebuilding(rebuilding bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.ProjectionRebuilding = rebuilding
}

func (s *RedisStore) getJSON(ctx context.Context, key string, target any) (bool, error) {
	raw, err := s.client.Get(ctx, redisKeyPrefix+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		s.noteError(err)
		return false, err
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return false, err
	}
	s.noteSuccess(nil, s.Runtime().ProjectionRebuilding)
	return true, nil
}

func (s *RedisStore) setJSON(ctx context.Context, key string, value any, ttl time.Duration) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := s.client.Set(ctx, redisKeyPrefix+key, raw, ttl).Err(); err != nil {
		s.noteError(err)
		return err
	}
	s.noteSuccess(nil, s.Runtime().ProjectionRebuilding)
	return nil
}

func (s *RedisStore) FindSegments(ctx context.Context, keys []string) (map[string]SegmentResult, error) {
	result := make(map[string]SegmentResult)
	for _, key := range keys {
		var item SegmentResult
		ok, err := s.getJSON(ctx, "target:"+key, &item)
		if err != nil {
			return result, err
		}
		if ok {
			result[key] = item
		}
	}
	return result, nil
}

func (s *RedisStore) SaveSegment(ctx context.Context, item SegmentResult) error {
	return s.setJSON(ctx, "target:"+item.AuditKey, item, s.ttl)
}

func (s *RedisStore) FindWhole(ctx context.Context, evaluationHash, targetHash string) (*Outcome, error) {
	var cached wholeCacheValue
	ok, err := s.getJSON(ctx, "whole:"+evaluationHash+":"+targetHash, &cached)
	if err != nil || !ok {
		return nil, err
	}
	return &Outcome{ID: cached.ID, Evaluation: Evaluation{Models: cached.Models}}, nil
}

func (s *RedisStore) SaveWhole(ctx context.Context, outcome Outcome, evaluationHash, targetHash string) error {
	value := wholeCacheValue{ID: outcome.ID, Models: outcome.Models}
	return s.setJSON(ctx, "whole:"+evaluationHash+":"+targetHash, value, s.ttl)
}

func statsCacheKey(query StatsQuery) (string, error) {
	raw, err := json.Marshal(query)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return "stats:" + hex.EncodeToString(digest[:]), nil
}

func statsQueryFamilyKey(query StatsQuery) (string, error) {
	raw, err := json.Marshal(struct {
		Timezone, Mode, ModelID, Stage string
	}{query.Timezone, query.Mode, query.ModelID, query.Stage})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

type cachedStats struct {
	Revision string `json:"revision"`
	Stats    Stats  `json:"stats"`
}

func (s *RedisStore) FindStats(ctx context.Context, revision string, query StatsQuery) (*Stats, bool, error) {
	key, err := statsCacheKey(query)
	if err != nil {
		return nil, false, err
	}
	var cached cachedStats
	ok, err := s.getJSON(ctx, key, &cached)
	if err != nil || !ok {
		return nil, false, err
	}
	stats := cached.Stats
	stats.StatsSource = "redis"
	stats.ProjectionRebuilding = cached.Revision != revision
	s.noteSuccess(&stats.AsOf, false)
	if stats.ProjectionRebuilding {
		s.noteSuccess(&stats.AsOf, true)
	}
	return &stats, stats.ProjectionRebuilding, nil
}

func (s *RedisStore) SaveStats(ctx context.Context, revision string, stats *Stats) error {
	key, err := statsCacheKey(stats.StatsQuery)
	if err != nil {
		return err
	}
	copy := *stats
	copy.StatsSource = "redis"
	copy.ProjectionRebuilding = false
	queryRaw, err := json.Marshal(stats.StatsQuery)
	if err != nil {
		return err
	}
	family, err := statsQueryFamilyKey(stats.StatsQuery)
	if err != nil {
		return err
	}
	pipe := s.client.TxPipeline()
	value, err := json.Marshal(cachedStats{Revision: revision, Stats: copy})
	if err != nil {
		return err
	}
	pipe.Set(ctx, redisKeyPrefix+key, value, 0)
	pipe.HSet(ctx, redisKeyPrefix+"stats:queries", family, queryRaw)
	if _, err := pipe.Exec(ctx); err != nil {
		s.noteError(err)
		return err
	}
	s.noteSuccess(&stats.AsOf, s.Runtime().ProjectionRebuilding)
	return nil
}

func (s *RedisStore) StatsQueries(ctx context.Context) ([]StatsQuery, error) {
	values, err := s.client.HVals(ctx, redisKeyPrefix+"stats:queries").Result()
	if err != nil {
		s.noteError(err)
		return nil, err
	}
	queries := make([]StatsQuery, 0, len(values))
	for _, value := range values {
		var query StatsQuery
		if json.Unmarshal([]byte(value), &query) == nil && query.From.Before(query.To) {
			queries = append(queries, query)
		}
	}
	return queries, nil
}
