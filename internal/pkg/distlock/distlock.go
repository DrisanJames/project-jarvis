package distlock

import (
	"context"
	"database/sql"
	"hash/fnv"
	"time"

	"github.com/redis/go-redis/v9"
)

// DistLock is the interface for distributed locking.
// Implementations must be safe for use from a single goroutine;
// concurrent use across goroutines requires separate lock instances.
type DistLock interface {
	// Acquire tries to acquire the lock. Returns true if successful.
	Acquire(ctx context.Context) (bool, error)
	// Release releases the lock if we still own it.
	Release(ctx context.Context) error
}

// NewLock creates a distributed lock using the best available backend.
// If redisClient is non-nil, uses Redis (preferred for cross-host locking).
// Otherwise falls back to PostgreSQL advisory locks.
func NewLock(redisClient *redis.Client, db *sql.DB, key string, ttl time.Duration) DistLock {
	if redisClient != nil {
		return NewRedisLock(redisClient, key, ttl)
	}
	return NewPGAdvisoryLock(db, key)
}

// =============================================================================
// PostgreSQL Advisory Lock (fallback when Redis is unavailable)
// =============================================================================
// Uses pg_try_advisory_lock / pg_advisory_unlock which are session-scoped.
// The lock is automatically released if the DB connection drops, providing
// crash-safety similar to Redis TTL expiration.

// PGAdvisoryLock implements DistLock using PostgreSQL advisory locks.
type PGAdvisoryLock struct {
	db     *sql.DB
	lockID int64
}

// NewPGAdvisoryLock creates a PG advisory lock with a deterministic lock ID
// derived from the given key string.
func NewPGAdvisoryLock(db *sql.DB, key string) *PGAdvisoryLock {
	h := fnv.New64a()
	h.Write([]byte(key))
	return &PGAdvisoryLock{
		db:     db,
		lockID: int64(h.Sum64()),
	}
}

// Acquire tries to acquire the advisory lock. Returns true if successful.
// Uses pg_try_advisory_lock which returns immediately (non-blocking).
func (l *PGAdvisoryLock) Acquire(ctx context.Context) (bool, error) {
	var acquired bool
	err := l.db.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", l.lockID).Scan(&acquired)
	return acquired, err
}

// Release releases the advisory lock.
func (l *PGAdvisoryLock) Release(ctx context.Context) error {
	_, err := l.db.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", l.lockID)
	return err
}
