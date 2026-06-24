package mailing

// Cross-brand daily cap.
//
// A subscriber should never receive more than N emails per calendar day
// across ALL brands combined. Under the legacy list architecture this
// was physically impossible to enforce (each brand had its own list, no
// cross-brand view). Under master-list selection the cap lives here.
//
// Fast path (Redis): key `cap:{subID}:{YYYYMMDD}`, value = integer count,
// TTL = 26h so the key self-expires a couple of hours past midnight UTC.
// Reserve is atomic — INCR then read; if the post-increment value is
// over the cap we immediately DECR and deny.
//
// Slow path (Postgres): if Redis is unavailable we count 'sent' rows in
// `mailing_tracking_events` for today. This is a best-effort fallback;
// it is NOT race-safe under high concurrency, so Redis should be on in
// production.
//
// The cap value is pulled per-org from `organizations.settings->>'cross_brand_daily_cap'`.
// Falls back to the struct default (seeded at 2, ceiling at 4).

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// CapChecker enforces the cross-brand daily cap.
//
// Thread-safe. All methods are safe to call concurrently from the send
// worker pool. The Redis client may be nil — in that case the struct
// falls back to Postgres-only counting (reconciliation mode).
type CapChecker struct {
	db         *sql.DB
	rdb        *redis.Client
	defaultCap int

	// cacheMu guards concurrent reads/writes on orgCapCache and
	// orgCapCachedAt. Reserve() is invoked from the dispatch loop for
	// every item in a claimed batch, and multiple campaigns can dispatch
	// concurrently, so a native map without a lock would race under -race
	// and can corrupt the map under load.
	cacheMu        sync.RWMutex
	orgCapCache    map[string]int
	orgCapCachedAt time.Time
}

// NewCapChecker constructs a checker with the given default cap (used
// when org settings do not specify one). Pass nil for rdb to run without
// the Redis fast path.
func NewCapChecker(db *sql.DB, rdb *redis.Client, defaultCap int) *CapChecker {
	if defaultCap <= 0 {
		defaultCap = 2
	}
	return &CapChecker{
		db:          db,
		rdb:         rdb,
		defaultCap:  defaultCap,
		orgCapCache: map[string]int{},
	}
}

// capForOrg reads `settings->>'cross_brand_daily_cap'` from the
// organizations table. Cached for 60s to keep the hot path cheap.
//
// Concurrency: reads are guarded by an RLock and writes take the full
// Lock. The 60s TTL is shared across all orgs because refresh is per-org
// (on miss we fetch just that org's value and update the shared timestamp).
// Under normal load this is fine — the cost of the read-through query is
// one row lookup on an indexed pk.
func (c *CapChecker) capForOrg(ctx context.Context, orgID string) int {
	if orgID == "" {
		return c.defaultCap
	}
	c.cacheMu.RLock()
	fresh := time.Since(c.orgCapCachedAt) < 60*time.Second
	v, have := c.orgCapCache[orgID]
	c.cacheMu.RUnlock()
	if fresh && have {
		return v
	}
	var raw sql.NullString
	err := c.db.QueryRowContext(ctx,
		`SELECT settings->>'cross_brand_daily_cap' FROM organizations WHERE id = $1`, orgID).
		Scan(&raw)
	var resolved int
	if err != nil || !raw.Valid || raw.String == "" {
		resolved = c.defaultCap
	} else if _, scanErr := fmt.Sscanf(raw.String, "%d", &resolved); scanErr != nil || resolved <= 0 {
		resolved = c.defaultCap
	}
	c.cacheMu.Lock()
	c.orgCapCache[orgID] = resolved
	c.orgCapCachedAt = time.Now()
	c.cacheMu.Unlock()
	return resolved
}

// todayKey returns the cap key for the current UTC date. UTC keeps the
// key stable regardless of operator timezone and matches the 24h billing
// window that ISPs use internally.
func todayKey(subscriberID string) string {
	return fmt.Sprintf("cap:%s:%s", subscriberID, time.Now().UTC().Format("20060102"))
}

// Reserve atomically reserves one send for the given subscriber. Returns
// (true, newCount) if the subscriber is still under cap after this
// reservation; (false, currentCount) if over cap. When over cap the
// Redis counter is decremented back so the reservation does not stick.
//
// Callers MUST call Release(subscriberID) if the downstream send fails,
// so the cap slot is freed for a future attempt.
func (c *CapChecker) Reserve(ctx context.Context, orgID, subscriberID string) (bool, int, error) {
	cap := c.capForOrg(ctx, orgID)
	if cap <= 0 {
		return true, 0, nil
	}
	if c.rdb == nil {
		return c.reservePostgres(ctx, subscriberID, cap)
	}

	key := todayKey(subscriberID)
	pipe := c.rdb.TxPipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, 26*time.Hour)
	if _, err := pipe.Exec(ctx); err != nil {
		// Redis hiccup — fall back to Postgres so a transient outage
		// does not stop the send pool entirely.
		log.Printf("[cap] Redis pipeline error: %v — falling back to Postgres", err)
		return c.reservePostgres(ctx, subscriberID, cap)
	}
	n := int(incr.Val())
	if n > cap {
		if err := c.rdb.Decr(ctx, key).Err(); err != nil {
			log.Printf("[cap] DECR failed after over-cap reserve: %v", err)
		}
		return false, n - 1, nil
	}
	return true, n, nil
}

// Peek reads the current cap count for a subscriber WITHOUT incrementing
// the Redis counter. Returns (overCap, count, err).
//
// Designed for the wave dispatcher's pre-claim path so it can substitute
// a capped subscriber with the next reserve before any slot is committed
// to the queue. The send worker still calls Reserve(), which remains the
// authoritative gate — Peek is advisory only.
//
// Behavior:
//   - cap <= 0 (cap disabled for org): returns (false, 0, nil)
//   - Redis disabled / rdb == nil: returns (false, 0, nil); the dispatcher
//     treats this as "let it through, worker will catch it". This is the
//     correct degradation: if Redis is down we have no faster signal than
//     the worker's Postgres fallback.
//   - Redis key missing (no sends today for this subscriber): (false, 0, nil)
//   - Redis error: returns (false, 0, err); dispatcher should treat the
//     error as advisory miss and let the row through.
//
// Crucially: NO INCR, NO EXPIRE, NO DECR. Peek must not mutate any
// shared counter — that invariant is exercised in cross_brand_cap_test.go.
func (c *CapChecker) Peek(ctx context.Context, orgID, subscriberID string) (bool, int, error) {
	cap := c.capForOrg(ctx, orgID)
	if cap <= 0 {
		return false, 0, nil
	}
	if c.rdb == nil {
		return false, 0, nil
	}
	key := todayKey(subscriberID)
	v, err := c.rdb.Get(ctx, key).Int()
	if err == redis.Nil {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	return v >= cap, v, nil
}

// Release returns a previously reserved cap slot. Call on send failure.
func (c *CapChecker) Release(ctx context.Context, subscriberID string) {
	if c.rdb == nil {
		return
	}
	key := todayKey(subscriberID)
	if err := c.rdb.Decr(ctx, key).Err(); err != nil {
		log.Printf("[cap] Release DECR failed: %v", err)
	}
}

// waveSlotKey returns the per-wave cap key: `wave1t:{subID}:{slotBucket}`,
// where slotBucket identifies a single wave slot (e.g. the campaign's
// scheduled_at truncated to the hour). Distinct from the daily todayKey so
// the two caps never collide on the same Redis key.
func waveSlotKey(subscriberID, slotBucket string) string {
	return fmt.Sprintf("wave1t:%s:%s", subscriberID, slotBucket)
}

// ClaimWaveSlot claims a subscriber's SINGLE slot within a wave for the
// given ownerToken (the claiming wave's ID), enforcing "one email per wave"
// across ALL campaigns sharing slotBucket. Returns true if this owner holds
// the slot (claimed it now, or already owns it).
//
// Why an owner-token SET-NX instead of a counter: the claim is written to
// Redis from inside the enqueue's Postgres transaction, and Redis is not
// transactional with Postgres. A plain INCR would NOT roll back if the
// enqueue TX fails, so a transient error + wave re-dispatch would re-INCR
// every recipient over the limit and permanently suppress the whole wave.
// SET key=ownerToken NX makes the claim IDEMPOTENT for the owning wave: a
// retry of the SAME wave reads back its own token and is allowed, while a
// DIFFERENT campaign's wave in the same slot reads a foreign token and is
// denied. Cross-campaign dedup holds; retries are safe.
//
// Redis-only and FAIL-OPEN: with rdb == nil, an empty subscriber/bucket/
// owner, or any Redis error it returns (true, nil) — the per-wave cap
// degrades to OFF rather than blocking the send pool (same safe direction
// as the daily cap). The key self-expires (26h TTL); a slot is never reused.
func (c *CapChecker) ClaimWaveSlot(ctx context.Context, subscriberID, slotBucket, ownerToken string) (bool, error) {
	if c.rdb == nil || subscriberID == "" || slotBucket == "" || ownerToken == "" {
		return true, nil
	}
	key := waveSlotKey(subscriberID, slotBucket)
	ok, err := c.rdb.SetNX(ctx, key, ownerToken, 26*time.Hour).Result()
	if err != nil {
		log.Printf("[cap] per-wave SetNX error: %v — allowing (cap degrades off)", err)
		return true, nil
	}
	if ok {
		return true, nil // freshly claimed by this wave
	}
	// Slot already owned — allowed only if WE own it (an idempotent retry of
	// the same wave); a foreign owner means another campaign already took it.
	cur, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		return true, nil // fail-open on read error
	}
	return cur == ownerToken, nil
}

// reservePostgres is the non-Redis fallback. It counts today's 'sent'
// events from mailing_tracking_events and denies if already at cap.
// NOT race-safe — concurrent workers can each see the same count and
// both proceed, breaching the cap by up to (nWorkers-1).
func (c *CapChecker) reservePostgres(ctx context.Context, subscriberID string, cap int) (bool, int, error) {
	var count int
	err := c.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_tracking_events
		WHERE subscriber_id = $1
		  AND event_type = 'sent'
		  AND event_at >= date_trunc('day', NOW() AT TIME ZONE 'UTC')`,
		subscriberID,
	).Scan(&count)
	if err != nil {
		return true, 0, err
	}
	if count >= cap {
		return false, count, nil
	}
	return true, count + 1, nil
}

// ReservationResult captures one subscriber's cap decision. Used when
// filtering a batch of queue items at dispatch time.
type ReservationResult struct {
	SubscriberID string
	Allowed      bool
	Count        int
}

// ReserveBatch calls Reserve for every subscriber ID. Returns the same
// list in the same order with per-item decisions. Errors on individual
// items are logged but not returned — an over-cap check failing open is
// less bad than stopping the whole batch.
func (c *CapChecker) ReserveBatch(ctx context.Context, orgID string, subscriberIDs []string) []ReservationResult {
	out := make([]ReservationResult, 0, len(subscriberIDs))
	for _, sid := range subscriberIDs {
		allowed, n, err := c.Reserve(ctx, orgID, sid)
		if err != nil {
			log.Printf("[cap] Reserve error for sub=%s: %v — allowing", sid, err)
			allowed = true
		}
		out = append(out, ReservationResult{SubscriberID: sid, Allowed: allowed, Count: n})
	}
	return out
}
