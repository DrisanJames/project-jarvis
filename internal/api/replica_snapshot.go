package api

// replicaSnapshot — ONE replica computes a per-process portal cache, the
// others read the result from Redis.
//
// ignite-service runs more than one ECS task, and every task starts the same
// in-memory cache refreshers (outbox summary, outbox engine status, partner
// queue state, CPM event counts). Each task therefore ran the same aggregate
// scans on its own cadence and the primary did the work once per replica —
// measured 2026-10-28: the five heaviest refresh queries running 100% of a
// 60s pg_stat_activity window on BOTH replicas while RDS sat at DiskQueueDepth
// ~65.
//
// The pattern is the existing pieces only: distlock (the Redis lock) elects the
// computing replica per cycle, and the result is published as JSON with a TTL
// (the esp_distributor / agent_preprocessor posture). One tick:
//
//  1. no Redis                 → compute locally (pre-change behaviour)
//  2. a snapshot younger than freshFor is in Redis → apply it, no PG query
//  3. lock acquired            → compute, publish, release
//  4. lock held elsewhere      → apply the (older) published snapshot if any;
//     otherwise skip — the holder is computing and will publish
//  5. Redis error on the lock  → compute locally (never worse than before)
//
// The PG advisory fallback of distlock is deliberately NOT used here: without
// Redis there is nowhere to publish the snapshot, so a lock would only make
// the losing replica serve nothing.

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
)

// replicaSnapshotOutcome names what one tick did (logged by callers, pinned
// by tests).
type replicaSnapshotOutcome string

const (
	snapshotLocal    replicaSnapshotOutcome = "local"    // computed, no Redis (or Redis lock error)
	snapshotComputed replicaSnapshotOutcome = "computed" // lock holder: computed + published
	snapshotLoaded   replicaSnapshotOutcome = "loaded"   // served another replica's snapshot
	snapshotSkipped  replicaSnapshotOutcome = "skipped"  // holder busy, nothing published yet
)

type replicaSnapshot struct {
	rdb      *redis.Client
	name     string        // distinct per surface; lock key = name, data key = "snapshot:"+name
	freshFor time.Duration // a published snapshot younger than this is served instead of recomputing
	ttl      time.Duration // Redis TTL of the published snapshot (~3x the refresh interval)
	lockTTL  time.Duration // lease covering one compute (released when it finishes)
}

type replicaSnapshotEnvelope struct {
	At      time.Time       `json:"at"`
	Payload json.RawMessage `json:"payload"`
}

func (s *replicaSnapshot) dataKey() string { return "snapshot:" + s.name }

// load returns the published snapshot (nil when absent or undecodable).
func (s *replicaSnapshot) load(ctx context.Context) *replicaSnapshotEnvelope {
	raw, err := s.rdb.Get(ctx, s.dataKey()).Bytes()
	if err != nil {
		if err != redis.Nil {
			log.Printf("[ReplicaSnapshot] %s read: %v", s.name, err)
		}
		return nil
	}
	var env replicaSnapshotEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Payload) == 0 {
		log.Printf("[ReplicaSnapshot] %s decode: %v", s.name, err)
		return nil
	}
	return &env
}

func (s *replicaSnapshot) publish(ctx context.Context, at time.Time, v interface{}) {
	payload, err := json.Marshal(v)
	if err != nil {
		log.Printf("[ReplicaSnapshot] %s encode: %v", s.name, err)
		return
	}
	body, err := json.Marshal(replicaSnapshotEnvelope{At: at, Payload: payload})
	if err != nil {
		log.Printf("[ReplicaSnapshot] %s encode: %v", s.name, err)
		return
	}
	if err := s.rdb.Set(ctx, s.dataKey(), body, s.ttl).Err(); err != nil {
		log.Printf("[ReplicaSnapshot] %s publish: %v", s.name, err)
	}
}

// runReplicaSnapshot runs one refresh tick. compute stores into the caller's
// local cache itself (exactly as before this change) and returns the value to
// publish; ok=false publishes nothing (the caller keeps its previous cache).
// apply installs a snapshot computed by another replica into the local cache.
func runReplicaSnapshot[T any](ctx context.Context, s *replicaSnapshot, compute func() (T, bool), apply func(T)) replicaSnapshotOutcome {
	if s == nil || s.rdb == nil {
		compute()
		return snapshotLocal
	}
	applyEnv := func(env *replicaSnapshotEnvelope) bool {
		var v T
		if err := json.Unmarshal(env.Payload, &v); err != nil {
			log.Printf("[ReplicaSnapshot] %s payload decode: %v", s.name, err)
			return false
		}
		apply(v)
		return true
	}

	env := s.load(ctx)
	if env != nil && time.Since(env.At) < s.freshFor && applyEnv(env) {
		return snapshotLoaded
	}

	lock := distlock.NewLock(s.rdb, nil, s.name, s.lockTTL)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[ReplicaSnapshot] %s lock unavailable, computing locally: %v", s.name, err)
		compute()
		return snapshotLocal
	}
	if !acquired {
		if env != nil && applyEnv(env) {
			return snapshotLoaded
		}
		return snapshotSkipped
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("[ReplicaSnapshot] %s lock release: %v", s.name, err)
		}
	}()

	started := time.Now()
	v, ok := compute()
	if ok {
		s.publish(ctx, started, v)
	}
	return snapshotComputed
}
