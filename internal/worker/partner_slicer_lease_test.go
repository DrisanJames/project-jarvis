package worker

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// Mutual-exclusion tests for concurrent batch claiming.
//
// These need REAL Postgres: the contract under test lives in claimNextBatch's
// WHERE clause and in row locking, and sqlmock returns canned rows without
// evaluating either — it cannot tell a correct guard from a missing one.
// Skips when SLICER_TEST_DATABASE_URL is unset so CI and `go test ./...` are
// unaffected.
//
// Background: claimNextBatch commits its short claim transaction BEFORE the S3
// fetch and insert work runs, so the row lock is gone while the batch is still
// being processed. Matching status='slicing' unconditionally therefore let a
// second worker claim a batch the first was actively processing. The lease
// closes that hole while preserving crash recovery.
// leaseTestDB returns a pool pinned to `schema` via a CONNECTION parameter, not
// a `SET search_path` statement. database/sql hands each goroutine an arbitrary
// pooled connection, so a SET applies to exactly one of them — the other workers
// would resolve unqualified table names in the wrong schema and silently claim
// nothing, which reads as "mutual exclusion passed" when nothing was tested.
func leaseTestDB(t *testing.T, schema string) *sql.DB {
	t.Helper()
	dsn := os.Getenv("SLICER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SLICER_TEST_DATABASE_URL not set — skipping slicer lease integration test")
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("postgres", dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("cannot reach test database: %v", err)
	}
	// Enough connections that every worker can be in flight at once; a smaller
	// pool would serialize them and mask a missing guard.
	db.SetMaxOpenConns(16)
	return db
}

// seedLeaseFixture builds the two tables claimNextBatch joins, in a scratch
// schema dropped at test end.
func seedLeaseFixture(t *testing.T, db *sql.DB, schema string, batches int) {
	t.Helper()
	ctx := context.Background()
	mustExec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec %.60s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.Exec(fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema))
	})

	mustExec(`CREATE TABLE partner_datasets (
		id UUID PRIMARY KEY, vertical TEXT, status TEXT,
		paused_emergency BOOLEAN DEFAULT false, express_dispatch BOOLEAN DEFAULT false)`)
	mustExec(`CREATE TABLE partner_inbound_batches (
		id UUID PRIMARY KEY, dataset_id UUID, partner_id UUID,
		s3_bucket TEXT, s3_key TEXT, record_count INT, next_record_offset INT DEFAULT 0,
		status TEXT, emergency_stopped BOOLEAN DEFAULT false,
		received_at TIMESTAMPTZ, slicing_started_at TIMESTAMPTZ)`)
	mustExec(`INSERT INTO partner_datasets (id, vertical, status)
		VALUES ('11111111-1111-1111-1111-111111111111','consumer','active')`)
	for i := 0; i < batches; i++ {
		mustExec(`INSERT INTO partner_inbound_batches
			(id, dataset_id, partner_id, s3_bucket, s3_key, record_count, status, received_at)
			VALUES (gen_random_uuid(),'11111111-1111-1111-1111-111111111111',
			        '22222222-2222-2222-2222-222222222222','b',$1,1,'received', NOW() - ($2 || ' seconds')::interval)`,
			fmt.Sprintf("key-%d", i), fmt.Sprintf("%d", batches-i))
	}
}

// A batch must be claimed by exactly ONE worker. Every worker races at the same
// barrier, so this is the scenario that produced duplicate processing before
// the lease existed.
func TestPartnerSlicer_ClaimIsMutuallyExclusive(t *testing.T) {
	const schema, batches, workers = "slicer_lease_excl", 25, 8
	bootstrapSchema(t, schema)
	db := leaseTestDB(t, schema)
	defer db.Close()
	seedLeaseFixture(t, db, schema, batches)

	ps := NewPartnerSlicer(db, nil, "b", nil, PartnerSlicerConfig{Concurrency: workers})

	var mu sync.Mutex
	claimedBy := map[string]int{}
	var barrier, done sync.WaitGroup
	barrier.Add(1)
	for w := 0; w < workers; w++ {
		done.Add(1)
		go func(worker int) {
			defer done.Done()
			barrier.Wait() // all workers claim simultaneously
			// Bounded: without the lease guard a batch is re-claimable
			// forever, so an unbounded loop HANGS rather than failing.
			for i := 0; i < batches*4; i++ {
				b, err := ps.claimNextBatch(context.Background())
				if err != nil || b == nil {
					return
				}
				mu.Lock()
				claimedBy[b.id]++
				mu.Unlock()
			}
		}(w)
	}
	barrier.Done()
	done.Wait()

	if len(claimedBy) != batches {
		t.Fatalf("claimed %d distinct batches, want %d", len(claimedBy), batches)
	}
	for id, n := range claimedBy {
		if n != 1 {
			t.Fatalf("batch %s claimed %d times — concurrent workers double-processed it", id, n)
		}
	}
}

// A batch already in flight must NOT be re-claimable while its lease is live,
// but MUST be recovered once the lease expires (crashed/hung worker).
func TestPartnerSlicer_LeaseBlocksLiveButRecoversAbandoned(t *testing.T) {
	const schema = "slicer_lease_ttl"
	bootstrapSchema(t, schema)
	db := leaseTestDB(t, schema)
	defer db.Close()
	seedLeaseFixture(t, db, schema, 1)

	ps := NewPartnerSlicer(db, nil, "b", nil, PartnerSlicerConfig{Concurrency: 2})
	ctx := context.Background()

	first, err := ps.claimNextBatch(ctx)
	if err != nil || first == nil {
		t.Fatalf("first claim failed: %v", err)
	}

	// Lease is live: the batch is being processed, so nobody else may take it.
	if again, err := ps.claimNextBatch(ctx); err != nil {
		t.Fatalf("second claim errored: %v", err)
	} else if again != nil {
		t.Fatalf("batch %s re-claimed while its lease was live — this is the double-processing bug", again.id)
	}

	// Simulate a worker that died without renewing: age the lease past its TTL.
	if _, err := db.ExecContext(ctx,
		`UPDATE partner_inbound_batches SET slicing_started_at = NOW() - INTERVAL '16 minutes' WHERE id = $1`,
		first.id); err != nil {
		t.Fatalf("age lease: %v", err)
	}
	recovered, err := ps.claimNextBatch(ctx)
	if err != nil {
		t.Fatalf("recovery claim errored: %v", err)
	}
	if recovered == nil || recovered.id != first.id {
		t.Fatalf("abandoned batch was not recovered after lease expiry — a crashed worker would strand it forever")
	}
}

// Express datasets must be claimed ahead of older bulk batches.
func TestPartnerSlicer_ExpressJumpsQueue(t *testing.T) {
	const schema = "slicer_lease_express"
	bootstrapSchema(t, schema)
	db := leaseTestDB(t, schema)
	defer db.Close()
	seedLeaseFixture(t, db, schema, 5)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO partner_datasets (id, vertical, status, express_dispatch)
		VALUES ('33333333-3333-3333-3333-333333333333','internal_auto_insurance','active',true)`); err != nil {
		t.Fatalf("seed express dataset: %v", err)
	}
	// Newest possible arrival — pure FIFO would put it dead last.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO partner_inbound_batches
		(id, dataset_id, partner_id, s3_bucket, s3_key, record_count, status, received_at)
		VALUES ('44444444-4444-4444-4444-444444444444','33333333-3333-3333-3333-333333333333',
		        '22222222-2222-2222-2222-222222222222','b','express-key',1,'received', NOW())`); err != nil {
		t.Fatalf("seed express batch: %v", err)
	}

	got, err := NewPartnerSlicer(db, nil, "b", nil, PartnerSlicerConfig{}).claimNextBatch(ctx)
	if err != nil || got == nil {
		t.Fatalf("claim failed: %v", err)
	}
	if got.id != "44444444-4444-4444-4444-444444444444" {
		t.Fatalf("claimed %s (key=%s) — express batch did not jump the queue", got.id, got.s3Key)
	}
}

var _ = time.Second

// bootstrapSchema creates the scratch schema on an unpinned connection, since
// the pinned pool cannot connect to a search_path that does not exist yet.
func bootstrapSchema(t *testing.T, schema string) {
	t.Helper()
	dsn := os.Getenv("SLICER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SLICER_TEST_DATABASE_URL not set — skipping slicer lease integration test")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("cannot reach test database: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("cannot reach test database: %v", err)
	}
	for _, q := range []string{
		fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema),
		fmt.Sprintf("CREATE SCHEMA %s", schema),
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("bootstrap %s: %v", q, err)
		}
	}
}
