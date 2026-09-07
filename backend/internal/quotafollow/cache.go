package quotafollow

import (
	"context"
	"errors"
	"fmt"
	"github.com/redis/go-redis/v9"
	"log"
	"sync"
)

type RedisCache struct {
	client *redis.Client
	mu     sync.Mutex
	status string
}

func NewRedisCache(rawURL string) (*RedisCache, error) {
	if rawURL == "" {
		return nil, nil
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		return &RedisCache{status: "额度只读 Redis 连接配置无效"}, errors.New("额度只读 Redis 连接配置无效")
	}
	options.Protocol = 2
	options.DisableIdentity = true
	options.MaxRetries = -1
	options.ContextTimeoutEnabled = true
	return &RedisCache{client: redis.NewClient(options), status: "已配置，等待首次只读复核"}, nil
}
func (c *RedisCache) Read(ctx context.Context, userID int64) (map[string]string, error) {
	if c.client == nil {
		return nil, errors.New(c.Status())
	}
	raw, err := c.client.HGetAll(ctx, fmt.Sprintf("billing:user_platform_quota:%d:openai", userID)).Result()
	if err == nil {
		err = cacheCompatible(raw)
	} else {
		err = errors.New("Redis 只读访问失败，请检查受控连接与 ACL 配置")
	}
	c.mu.Lock()
	if err != nil {
		c.status = "Redis 只读访问不可用或配额格式不兼容"
	} else if len(raw) == 0 {
		c.status = "Redis 可读取，目标键不存在，尚未观测 Hash 版本"
	} else {
		c.status = "Redis 只读复核正常"
	}
	c.mu.Unlock()
	return raw, err
}
func (c *RedisCache) Status() string { c.mu.Lock(); defer c.mu.Unlock(); return c.status }
func (c *RedisCache) Close() error {
	if c.client == nil {
		return nil
	}
	return c.client.Close()
}

// Invalidate 仅清除该用户的 OpenAI 配额缓存及对应脏成员；不重新归零或写入金额。
func (c *RedisCache) Invalidate(ctx context.Context, userID int64) error {
	if c.client == nil {
		return errors.New(c.Status())
	}
	key := fmt.Sprintf("billing:user_platform_quota:%d:openai", userID)
	if err := c.client.Del(ctx, key).Err(); err != nil {
		return errors.New("额度缓存删除未确认，下次仅重试缓存清理")
	}
	member := fmt.Sprintf("%d:openai", userID)
	removed, err := c.client.SRem(ctx, "billing:upq:dirty", member).Result()
	if err != nil {
		return errors.New("额度缓存已删除，脏成员清理未确认")
	}
	log.Printf("额度脏成员清理完成 member=%s before_present=%t after_present=false", member, removed > 0)
	return nil
}
