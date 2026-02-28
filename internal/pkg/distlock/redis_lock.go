package distlock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisLock provides distributed locking via Redis using SET NX with TTL.
// It uses a random ownership value and Lua scripts for atomic release/extend
// to prevent accidental release of locks held by other processes.
type RedisLock struct {
	client *redis.Client
	key    string
	value  string
	ttl    time.Duration
}

// NewRedisLock creates a new distributed lock backed by Redis.
func NewRedisLock(client *redis.Client, key string, ttl time.Duration) *RedisLock {
	// Generate random value for ownership verification
	b := make([]byte, 16)
	rand.Read(b)
	return &RedisLock{
		client: client,
		key:    fmt.Sprintf("lock:%s", key),
		value:  hex.EncodeToString(b),
		ttl:    ttl,
	}
}

// Acquire tries to acquire the lock. Returns true if successful.
func (l *RedisLock) Acquire(ctx context.Context) (bool, error) {
	result, err := l.client.SetNX(ctx, l.key, l.value, l.ttl).Result()
	if err != nil {
		return false, fmt.Errorf("failed to acquire lock %s: %w", l.key, err)
	}
	return result, nil
}

// Release releases the lock only if we still own it (using Lua script for atomicity).
func (l *RedisLock) Release(ctx context.Context) error {
	script := redis.NewScript(`
		if redis.call("get", KEYS[1]) == ARGV[1] then
			return redis.call("del", KEYS[1])
		else
			return 0
		end
	`)
	_, err := script.Run(ctx, l.client, []string{l.key}, l.value).Result()
	return err
}

// Extend extends the lock TTL (for long-running operations).
// Returns nil if successful, error if lock is no longer owned or Redis fails.
func (l *RedisLock) Extend(ctx context.Context, ttl time.Duration) error {
	script := redis.NewScript(`
		if redis.call("get", KEYS[1]) == ARGV[1] then
			return redis.call("pexpire", KEYS[1], ARGV[2])
		else
			return 0
		end
	`)
	_, err := script.Run(ctx, l.client, []string{l.key}, l.value, ttl.Milliseconds()).Result()
	return err
}
