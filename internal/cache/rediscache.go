package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// keyPrefix 是 Agent 运行态缓存在 Redis 中的 key 前缀。
const keyPrefix = "agenteam:runtime:agent:"

// RedisAgentCache 是 AgentCache 的 Redis 实现。
type RedisAgentCache struct {
	rdb *redis.Client
}

// NewRedisAgentCache 使用连接串（如 redis://[:password@]host:port/db）创建
// RedisAgentCache，并做一次连通性检查。
func NewRedisAgentCache(ctx context.Context, dsn string) (*RedisAgentCache, error) {
	opts, err := redis.ParseURL(dsn)
	if err != nil {
		return nil, fmt.Errorf("cache: parse redis dsn: %w", err)
	}
	rdb := redis.NewClient(opts)
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("cache: ping redis: %w", err)
	}
	return &RedisAgentCache{rdb: rdb}, nil
}

// Close 关闭 Redis 连接。
func (c *RedisAgentCache) Close() error {
	return c.rdb.Close()
}

func key(agentID string) string {
	return keyPrefix + agentID
}

// Set 写入/覆盖指定 Agent 的运行态快照，永久保存（无 TTL）：
// 该快照代表“当前生效配置”，其生命周期跟随 Agent 本身，而非临时缓存。
func (c *RedisAgentCache) Set(ctx context.Context, snapshot *AgentSnapshot) error {
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("cache: marshal snapshot: %w", err)
	}
	if err := c.rdb.Set(ctx, key(snapshot.ID), data, 0).Err(); err != nil {
		return fmt.Errorf("cache: set agent snapshot: %w", err)
	}
	return nil
}

// Get 读取指定 Agent 的运行态快照。
func (c *RedisAgentCache) Get(ctx context.Context, agentID string) (*AgentSnapshot, bool, error) {
	data, err := c.rdb.Get(ctx, key(agentID)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("cache: get agent snapshot: %w", err)
	}
	var snapshot AgentSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, false, fmt.Errorf("cache: unmarshal snapshot: %w", err)
	}
	return &snapshot, true, nil
}

// Delete 删除指定 Agent 的运行态快照。
func (c *RedisAgentCache) Delete(ctx context.Context, agentID string) error {
	if err := c.rdb.Del(ctx, key(agentID)).Err(); err != nil {
		return fmt.Errorf("cache: delete agent snapshot: %w", err)
	}
	return nil
}
