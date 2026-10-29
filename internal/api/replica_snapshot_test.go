package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type snapTestPayload struct {
	N int `json:"n"`
}

func newSnapTest(t *testing.T) (*miniredis.Miniredis, *redis.Client, *replicaSnapshot) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return mr, rdb, &replicaSnapshot{rdb: rdb, name: "test-refresh", freshFor: time.Minute, ttl: 3 * time.Minute, lockTTL: time.Minute}
}

func seedSnapshot(t *testing.T, mr *miniredis.Miniredis, name string, at time.Time, v interface{}) {
	t.Helper()
	payload, _ := json.Marshal(v)
	body, _ := json.Marshal(replicaSnapshotEnvelope{At: at, Payload: payload})
	if err := mr.Set("snapshot:"+name, string(body)); err != nil {
		t.Fatal(err)
	}
}

// (a) the lock holder computes, publishes, and releases the lock.
func TestReplicaSnapshot_HolderComputesAndPublishes(t *testing.T) {
	mr, _, s := newSnapTest(t)
	computed, applied := 0, 0
	out := runReplicaSnapshot(context.Background(), s,
		func() (snapTestPayload, bool) { computed++; return snapTestPayload{N: 7}, true },
		func(snapTestPayload) { applied++ })
	if out != snapshotComputed || computed != 1 || applied != 0 {
		t.Fatalf("out=%s computed=%d applied=%d", out, computed, applied)
	}
	raw, err := mr.Get("snapshot:test-refresh")
	if err != nil {
		t.Fatalf("snapshot not published: %v", err)
	}
	var env replicaSnapshotEnvelope
	var p snapTestPayload
	if json.Unmarshal([]byte(raw), &env) != nil || json.Unmarshal(env.Payload, &p) != nil || p.N != 7 {
		t.Fatalf("published payload = %s", raw)
	}
	if ttl := mr.TTL("snapshot:test-refresh"); ttl <= 0 || ttl > s.ttl {
		t.Fatalf("snapshot ttl = %v", ttl)
	}
	if mr.Exists("lock:test-refresh") {
		t.Fatal("lock must be released after the compute")
	}
}

// A failed compute publishes nothing.
func TestReplicaSnapshot_FailedComputePublishesNothing(t *testing.T) {
	mr, _, s := newSnapTest(t)
	runReplicaSnapshot(context.Background(), s,
		func() (snapTestPayload, bool) { return snapTestPayload{}, false },
		func(snapTestPayload) {})
	if mr.Exists("snapshot:test-refresh") {
		t.Fatal("failed compute must not publish")
	}
}

// (b) a fresh published snapshot is applied with no compute (no PG query).
func TestReplicaSnapshot_NonHolderLoadsFreshSnapshot(t *testing.T) {
	mr, _, s := newSnapTest(t)
	seedSnapshot(t, mr, "test-refresh", time.Now().Add(-10*time.Second), snapTestPayload{N: 42})
	var got snapTestPayload
	out := runReplicaSnapshot(context.Background(), s,
		func() (snapTestPayload, bool) { t.Fatal("must not compute"); return snapTestPayload{}, false },
		func(v snapTestPayload) { got = v })
	if out != snapshotLoaded || got.N != 42 {
		t.Fatalf("out=%s got=%+v", out, got)
	}
}

// (b) lock held elsewhere: a stale snapshot is served, nothing is computed.
func TestReplicaSnapshot_LockHeldServesStaleSnapshot(t *testing.T) {
	mr, _, s := newSnapTest(t)
	seedSnapshot(t, mr, "test-refresh", time.Now().Add(-time.Hour), snapTestPayload{N: 3})
	_ = mr.Set("lock:test-refresh", "other-replica")
	var got snapTestPayload
	out := runReplicaSnapshot(context.Background(), s,
		func() (snapTestPayload, bool) { t.Fatal("must not compute"); return snapTestPayload{}, false },
		func(v snapTestPayload) { got = v })
	if out != snapshotLoaded || got.N != 3 {
		t.Fatalf("out=%s got=%+v", out, got)
	}
}

// Lock held, nothing published yet (holder's first compute): skip, no compute.
func TestReplicaSnapshot_LockHeldNoSnapshotSkips(t *testing.T) {
	mr, _, s := newSnapTest(t)
	_ = mr.Set("lock:test-refresh", "other-replica")
	out := runReplicaSnapshot(context.Background(), s,
		func() (snapTestPayload, bool) { t.Fatal("must not compute"); return snapTestPayload{}, false },
		func(snapTestPayload) { t.Fatal("nothing to apply") })
	if out != snapshotSkipped {
		t.Fatalf("out=%s", out)
	}
}

// (c) no Redis → compute locally, exactly as before.
func TestReplicaSnapshot_NoRedisComputesLocally(t *testing.T) {
	computed := 0
	out := runReplicaSnapshot(context.Background(), &replicaSnapshot{name: "test-refresh"},
		func() (snapTestPayload, bool) { computed++; return snapTestPayload{}, true },
		func(snapTestPayload) { t.Fatal("nothing to apply") })
	if out != snapshotLocal || computed != 1 {
		t.Fatalf("out=%s computed=%d", out, computed)
	}
}

// (c) Redis configured but down → compute locally.
func TestReplicaSnapshot_RedisDownComputesLocally(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	defer rdb.Close()
	mr.Close()
	computed := 0
	out := runReplicaSnapshot(context.Background(),
		&replicaSnapshot{rdb: rdb, name: "test-refresh", freshFor: time.Minute, ttl: time.Minute, lockTTL: time.Minute},
		func() (snapTestPayload, bool) { computed++; return snapTestPayload{}, true },
		func(snapTestPayload) { t.Fatal("nothing to apply") })
	if out != snapshotLocal || computed != 1 {
		t.Fatalf("out=%s computed=%d", out, computed)
	}
}

// Surface wiring: a non-holder replica fills each real cache from the snapshot
// with a NIL db — any PG query would panic, so passing proves none was issued.

func TestOutboxSummaryShared_LoadsSnapshotWithoutDB(t *testing.T) {
	mr, rdb, _ := newSnapTest(t)
	share := outboxSummarySnapshot(rdb, OutboxSummaryRefreshInterval)
	at := time.Now().Add(-5 * time.Second).UTC()
	seedSnapshot(t, mr, share.name, at, OutboxSummaryResponse{GeneratedAt: at, APIVersion: VersionOutboxSummary, DepthByStatus: map[string]int{"queued": 9}, Last5MinInserted: 11})
	cache := &OutboxSummaryCache{}
	refreshOutboxSummaryShared(context.Background(), nil, cache, share)
	snap, ok := cache.Snapshot()
	if !ok || snap.DepthByStatus["queued"] != 9 || snap.Last5MinInserted != 11 || !snap.GeneratedAt.Equal(at) {
		t.Fatalf("cache = %+v ok=%v", snap, ok)
	}
}

func TestEngineStatusShared_LoadsSnapshotWithoutDB(t *testing.T) {
	mr, rdb, _ := newSnapTest(t)
	share := engineStatusSnapshot(rdb, engineFastRefreshInterval)
	at := time.Now().Add(-2 * time.Second).UTC()
	seedSnapshot(t, mr, share.name, at, OutboxEngineStatusResponse{GeneratedAt: at, APIVersion: VersionOutboxEngineStatus, Queue: EngineQueueSection{OK: true, Queued: 5}})
	cache := &outboxEngineCache{}
	refreshOutboxEngineStatusShared(context.Background(), nil, cache, &engineBaseline{}, share)
	snap, ok := cache.snapshot()
	if !ok || !snap.Queue.OK || snap.Queue.Queued != 5 {
		t.Fatalf("cache = %+v ok=%v", snap.Queue, ok)
	}
}

func TestDataIngestShared_LoadsSnapshotWithoutDB(t *testing.T) {
	mr, rdb, _ := newSnapTest(t)
	s := &DataIngestService{rdb: rdb} // db nil: the scan would return early, the cache stays empty
	share := &replicaSnapshot{rdb: rdb, name: "data-ingest-queue-state-refresh", freshFor: dataIngestRefreshEvery, ttl: time.Hour, lockTTL: time.Minute}
	at := time.Now().Add(-time.Minute).UTC()
	seedSnapshot(t, mr, share.name, at, &queueStateSnapshot{GeneratedAt: at, ByStatus: map[string]int64{"staged": 4}, Total: 4, ObjectsMeasured: true, Objects: map[string]objectCounts{"o": {StaticObjects: 2}}})
	s.refreshQueueStateShared(context.Background(), share)
	if s.cached == nil || s.cached.Total != 4 || s.cached.ByStatus["staged"] != 4 || s.cached.Objects["o"].StaticObjects != 2 || !s.cachedAt.Equal(at) {
		t.Fatalf("cached = %+v", s.cached)
	}
}

func TestCpmEventCacheShared_LoadsSnapshotWithoutDB(t *testing.T) {
	mr, rdb, _ := newSnapTest(t)
	h := &CpmPlannerHandlers{
		evByOrg: map[string]map[string]*cpmDealEventCounts{}, evWarmed: map[string]time.Time{},
		evRunning: map[string]bool{}, gapByOrg: map[string]*cpmAttributionGap{},
	}
	h.SetRedisClient(rdb)
	org := "00000000-0000-0000-0000-000000000001"
	warmed := time.Now().Add(-time.Minute).UTC()
	seedSnapshot(t, mr, "cpm-event-cache-refresh:"+org, warmed, cpmEventCacheSnapshot{
		Counts: map[string][4]int64{"deal-1": {10, 9, 1, 2}}, Gap: &cpmAttributionGap{Days: 30, DealDelivered: 5}, WarmedAt: warmed,
	})
	h.refreshOrgEventCountsShared(org)
	got := h.cachedEventCounts(org)
	if got == nil || got["deal-1"] == nil || *got["deal-1"] != (cpmDealEventCounts{sent: 10, delivered: 9, hard: 1, soft: 2}) {
		t.Fatalf("event cache = %+v", got)
	}
	if g := h.gapByOrg[org]; g == nil || g.DealDelivered != 5 {
		t.Fatalf("gap = %+v", g)
	}
}
