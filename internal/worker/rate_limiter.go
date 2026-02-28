package worker

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// RateLimiter provides atomic rate limiting using Redis Lua scripts
// Prevents race conditions that occur with GET → check → INCR patterns
type RateLimiter struct {
	redis *redis.Client

	// Pre-compiled Lua scripts for atomicity
	multiLimitScript *redis.Script
	domainLimitScript *redis.Script
}

// RateLimit defines limits for an ESP
type RateLimit struct {
	RequestsPerSecond int
	RequestsPerMinute int
	DailyLimit        int
}

// ESPLimits defines rate limits per ESP (production-tier plans for 50M/day capacity)
var ESPLimits = map[string]RateLimit{
	"sparkpost": {RequestsPerSecond: 100, RequestsPerMinute: 5000, DailyLimit: 15000000},  // 15M
	"ses":       {RequestsPerSecond: 500, RequestsPerMinute: 30000, DailyLimit: 25000000}, // 25M (production SES)
	"mailgun":   {RequestsPerSecond: 50, RequestsPerMinute: 3000, DailyLimit: 5000000},    // 5M
	"sendgrid":  {RequestsPerSecond: 50, RequestsPerMinute: 3000, DailyLimit: 5000000},    // 5M
}

// Lua script for atomic multi-key rate limit check
// This script atomically checks all limits and only increments if ALL pass
const multiLimitLuaScript = `
local secondKey = KEYS[1]
local minuteKey = KEYS[2]
local dailyKey = KEYS[3]
local increment = tonumber(ARGV[1])
local secondLimit = tonumber(ARGV[2])
local minuteLimit = tonumber(ARGV[3])
local dailyLimit = tonumber(ARGV[4])
local secondTTL = tonumber(ARGV[5])
local minuteTTL = tonumber(ARGV[6])
local dailyTTL = tonumber(ARGV[7])

-- Get current values
local secCurrent = tonumber(redis.call("GET", secondKey) or "0")
local minCurrent = tonumber(redis.call("GET", minuteKey) or "0")
local dayCurrent = tonumber(redis.call("GET", dailyKey) or "0")

-- Check all limits BEFORE incrementing
if secCurrent + increment > secondLimit then
    return {0, 1, secCurrent}  -- denied, reason=second limit
end
if minCurrent + increment > minuteLimit then
    return {0, 2, minCurrent}  -- denied, reason=minute limit
end
if dayCurrent + increment > dailyLimit then
    return {0, 3, dayCurrent}  -- denied, reason=daily limit
end

-- All checks passed - atomically increment all counters
local newSec = redis.call("INCRBY", secondKey, increment)
if newSec == increment then
    redis.call("EXPIRE", secondKey, secondTTL)
end

local newMin = redis.call("INCRBY", minuteKey, increment)
if newMin == increment then
    redis.call("EXPIRE", minuteKey, minuteTTL)
end

local newDay = redis.call("INCRBY", dailyKey, increment)
if newDay == increment then
    redis.call("EXPIRE", dailyKey, dailyTTL)
end

return {1, 0, newDay}  -- allowed, no denial reason
`

// Lua script for domain rate limiting
const domainLimitLuaScript = `
local key = KEYS[1]
local increment = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])

local current = tonumber(redis.call("GET", key) or "0")

if current + increment > limit then
    return {0, current}  -- denied
end

local newVal = redis.call("INCRBY", key, increment)
if newVal == increment then
    redis.call("EXPIRE", key, ttl)
end

return {1, newVal}  -- allowed
`

// NewRateLimiter creates a rate limiter with pre-compiled Lua scripts
func NewRateLimiter(redisClient *redis.Client) *RateLimiter {
	return &RateLimiter{
		redis:             redisClient,
		multiLimitScript:  redis.NewScript(multiLimitLuaScript),
		domainLimitScript: redis.NewScript(domainLimitLuaScript),
	}
}

// NewRateLimiterFromURL creates a rate limiter by connecting to Redis
func NewRateLimiterFromURL(redisURL string) (*RateLimiter, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("invalid redis URL: %w", err)
	}

	client := redis.NewClient(opts)

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis connection failed: %w", err)
	}

	log.Printf("[RateLimiter] Connected to Redis at %s", redisURL)

	return NewRateLimiter(client), nil
}

// CheckAndIncrement atomically checks and increments rate limit counters
func (r *RateLimiter) CheckAndIncrement(ctx context.Context, espType string, batchSize int) (allowed bool, waitTime time.Duration, err error) {
	limits, ok := ESPLimits[espType]
	if !ok {
		return false, 0, fmt.Errorf("unknown ESP type: %s", espType)
	}

	now := time.Now()

	// Keys with time-based bucketing
	secondKey := fmt.Sprintf("ratelimit:%s:sec:%d", espType, now.Unix())
	minuteKey := fmt.Sprintf("ratelimit:%s:min:%d", espType, now.Unix()/60)
	dailyKey := fmt.Sprintf("ratelimit:%s:day:%s", espType, now.Format("2006-01-02"))

	// Execute atomic Lua script
	result, err := r.multiLimitScript.Run(ctx, r.redis,
		[]string{secondKey, minuteKey, dailyKey},
		batchSize,
		limits.RequestsPerSecond,
		limits.RequestsPerMinute,
		limits.DailyLimit,
		2,     // second TTL
		120,   // minute TTL
		90000, // daily TTL (25 hours)
	).Slice()

	if err != nil {
		return false, 0, fmt.Errorf("rate limit check failed: %w", err)
	}

	allowedInt := result[0].(int64)
	denialReason := result[1].(int64)

	allowed = allowedInt == 1

	if !allowed {
		switch denialReason {
		case 1: // Second limit
			waitTime = time.Second
		case 2: // Minute limit
			waitTime = time.Duration(60-now.Second()) * time.Second
		case 3: // Daily limit
			return false, 0, fmt.Errorf("daily limit exceeded for %s", espType)
		}
	}

	return allowed, waitTime, nil
}

// CheckDomainLimit atomically checks domain-level rate limit
func (r *RateLimiter) CheckDomainLimit(ctx context.Context, domain string, count int) (allowed bool, waitTime time.Duration) {
	const domainLimitPerMinute = 1000

	now := time.Now()
	key := fmt.Sprintf("ratelimit:domain:%s:%d", domain, now.Unix()/60)

	result, err := r.domainLimitScript.Run(ctx, r.redis,
		[]string{key},
		count,
		domainLimitPerMinute,
		120, // 2 minute TTL
	).Slice()

	if err != nil {
		// On error, allow but log
		log.Printf("[RateLimiter] Domain limit check error: %v", err)
		return true, 0
	}

	allowedInt := result[0].(int64)
	if allowedInt == 0 {
		return false, time.Duration(60-now.Second()) * time.Second
	}

	return true, 0
}

// GetCurrentUsage returns current usage for an ESP
func (r *RateLimiter) GetCurrentUsage(ctx context.Context, espType string) (map[string]int64, error) {
	now := time.Now()

	secondKey := fmt.Sprintf("ratelimit:%s:sec:%d", espType, now.Unix())
	minuteKey := fmt.Sprintf("ratelimit:%s:min:%d", espType, now.Unix()/60)
	dailyKey := fmt.Sprintf("ratelimit:%s:day:%s", espType, now.Format("2006-01-02"))

	pipe := r.redis.Pipeline()
	secCmd := pipe.Get(ctx, secondKey)
	minCmd := pipe.Get(ctx, minuteKey)
	dayCmd := pipe.Get(ctx, dailyKey)
	pipe.Exec(ctx)

	sec, _ := secCmd.Int64()
	min, _ := minCmd.Int64()
	day, _ := dayCmd.Int64()

	limits := ESPLimits[espType]

	return map[string]int64{
		"second_current": sec,
		"second_limit":   int64(limits.RequestsPerSecond),
		"minute_current": min,
		"minute_limit":   int64(limits.RequestsPerMinute),
		"daily_current":  day,
		"daily_limit":    int64(limits.DailyLimit),
	}, nil
}

// Close closes the Redis connection
func (r *RateLimiter) Close() error {
	return r.redis.Close()
}
