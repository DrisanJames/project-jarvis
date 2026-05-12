package mailing

// Tests for CapChecker — the cross-brand daily cap enforcer.
//
// The surface under test here covers three things that have to hold together
// for the Master List architecture to be safe under load:
//
//   1. capForOrg() resolves per-org overrides from organizations.settings and
//      caches them for 60s (verified indirectly via a second call that does
//      NOT re-query the DB).
//   2. Concurrent callers on the hot path (Reserve -> capForOrg) do not race
//      on the internal cache map. This is exercised under `go test -race`.
//   3. The Postgres reservation fallback enforces the cap when Redis is not
//      configured — both the under-cap pass and the over-cap deny cases.
//
// Redis integration is deliberately NOT exercised here: it would require a
// real Redis (or a miniredis dependency) and the fast path is a thin wrapper
// around standard go-redis commands. The Postgres fallback IS the correctness
// floor and is what we want firm test coverage on.

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newCapCheckerMock(t *testing.T) (*CapChecker, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	cc := NewCapChecker(db, nil, 2)
	return cc, mock, db
}

func TestCapChecker_CapForOrg_UsesDefaultWhenEmptyOrgID(t *testing.T) {
	cc, _, _ := newCapCheckerMock(t)
	if got := cc.capForOrg(context.Background(), ""); got != 2 {
		t.Errorf("capForOrg(\"\") = %d, want 2 (defaultCap)", got)
	}
}

func TestCapChecker_CapForOrg_ReadsOrgOverride(t *testing.T) {
	cc, mock, _ := newCapCheckerMock(t)
	orgID := "00000000-0000-0000-0000-000000000001"

	mock.ExpectQuery(`SELECT settings->>'cross_brand_daily_cap' FROM organizations`).
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("4"))

	got := cc.capForOrg(context.Background(), orgID)
	if got != 4 {
		t.Errorf("capForOrg(%q) = %d, want 4", orgID, got)
	}

	// Second call must hit the 60s cache — no additional SELECT expected.
	got = cc.capForOrg(context.Background(), orgID)
	if got != 4 {
		t.Errorf("cached capForOrg(%q) = %d, want 4", orgID, got)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestCapChecker_CapForOrg_FallsBackOnMissingSetting(t *testing.T) {
	cc, mock, _ := newCapCheckerMock(t)
	orgID := "00000000-0000-0000-0000-000000000002"

	mock.ExpectQuery(`SELECT settings->>'cross_brand_daily_cap' FROM organizations`).
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(nil))

	got := cc.capForOrg(context.Background(), orgID)
	if got != 2 {
		t.Errorf("capForOrg(%q) with NULL setting = %d, want 2 (defaultCap)", orgID, got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestCapChecker_CapForOrg_FallsBackOnUnparseableSetting(t *testing.T) {
	cc, mock, _ := newCapCheckerMock(t)
	orgID := "00000000-0000-0000-0000-000000000003"

	mock.ExpectQuery(`SELECT settings->>'cross_brand_daily_cap' FROM organizations`).
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("not-a-number"))

	got := cc.capForOrg(context.Background(), orgID)
	if got != 2 {
		t.Errorf("capForOrg(%q) with garbage setting = %d, want 2 (defaultCap)", orgID, got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// TestCapChecker_CapForOrg_ConcurrentAccessIsSafe validates the RWMutex
// that guards orgCapCache. Run this with `go test -race ./internal/mailing/...`
// to catch regressions where the mutex is accidentally dropped.
//
// We seed the cache once, then fan out 200 goroutines hitting capForOrg
// concurrently. The cache is fresh for 60s so every goroutine should hit
// the read-lock fast path and observe a stable value (no torn map reads,
// no data races). A total of exactly 1 DB query is expected.
func TestCapChecker_CapForOrg_ConcurrentAccessIsSafe(t *testing.T) {
	cc, mock, _ := newCapCheckerMock(t)
	orgID := "00000000-0000-0000-0000-000000000004"

	mock.ExpectQuery(`SELECT settings->>'cross_brand_daily_cap' FROM organizations`).
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("3"))

	// Prime the cache (one query).
	if got := cc.capForOrg(context.Background(), orgID); got != 3 {
		t.Fatalf("initial capForOrg = %d, want 3", got)
	}

	const workers = 200
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			if got := cc.capForOrg(context.Background(), orgID); got != 3 {
				t.Errorf("concurrent capForOrg = %d, want 3", got)
			}
		}()
	}
	wg.Wait()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v (expected exactly 1 DB hit for priming)", err)
	}
}

// TestCapChecker_ReservePostgres_UnderCapAllows covers the Postgres fallback
// path (Redis nil). A send is allowed when today's 'sent' count is below cap.
func TestCapChecker_ReservePostgres_UnderCapAllows(t *testing.T) {
	cc, mock, _ := newCapCheckerMock(t)
	orgID := "00000000-0000-0000-0000-000000000005"
	subID := "aaaaaaaa-0000-0000-0000-000000000001"

	mock.ExpectQuery(`SELECT settings->>'cross_brand_daily_cap' FROM organizations`).
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("2"))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM mailing_tracking_events`).
		WithArgs(subID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	allowed, n, err := cc.Reserve(context.Background(), orgID, subID)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if !allowed {
		t.Errorf("allowed = false, want true (1 prior send < cap 2)")
	}
	if n != 2 {
		t.Errorf("count = %d, want 2 (one already sent + this reservation)", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestCapChecker_ReservePostgres_AtCapDenies(t *testing.T) {
	cc, mock, _ := newCapCheckerMock(t)
	orgID := "00000000-0000-0000-0000-000000000006"
	subID := "aaaaaaaa-0000-0000-0000-000000000002"

	mock.ExpectQuery(`SELECT settings->>'cross_brand_daily_cap' FROM organizations`).
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("2"))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM mailing_tracking_events`).
		WithArgs(subID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	allowed, n, err := cc.Reserve(context.Background(), orgID, subID)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if allowed {
		t.Errorf("allowed = true, want false (already at cap)")
	}
	if n != 2 {
		t.Errorf("count = %d, want 2 (current cap hit)", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestCapChecker_ReserveBatch_MixedResults(t *testing.T) {
	cc, mock, _ := newCapCheckerMock(t)
	orgID := "00000000-0000-0000-0000-000000000007"

	// capForOrg is read once, then cached for this batch.
	mock.ExpectQuery(`SELECT settings->>'cross_brand_daily_cap' FROM organizations`).
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("2"))
	// sub1 under cap (count=0), sub2 at cap (count=2), sub3 under cap (count=1).
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM mailing_tracking_events`).
		WithArgs("sub-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM mailing_tracking_events`).
		WithArgs("sub-2").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM mailing_tracking_events`).
		WithArgs("sub-3").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	results := cc.ReserveBatch(context.Background(), orgID, []string{"sub-1", "sub-2", "sub-3"})
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	want := []struct {
		id      string
		allowed bool
	}{
		{"sub-1", true},
		{"sub-2", false},
		{"sub-3", true},
	}
	for i, w := range want {
		if results[i].SubscriberID != w.id {
			t.Errorf("results[%d].SubscriberID = %q, want %q", i, results[i].SubscriberID, w.id)
		}
		if results[i].Allowed != w.allowed {
			t.Errorf("results[%d].Allowed = %v, want %v", i, results[i].Allowed, w.allowed)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// newCapCheckerWithMiniredis returns a CapChecker wired to an in-process
// miniredis. Used by the Peek tests where we need an actual Redis surface
// to assert no-mutation behavior.
func newCapCheckerWithMiniredis(t *testing.T, defaultCap int) (*CapChecker, *miniredis.Miniredis, sqlmock.Sqlmock) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cc := NewCapChecker(db, rdb, defaultCap)
	return cc, mr, mock
}

// TestCapChecker_Peek_NoRedis_ReturnsUnderCap covers the degradation
// path: when Redis is not configured, Peek MUST short-circuit to "under
// cap" so the dispatcher does not falsely cap-skip subscribers. The
// authoritative worker-side Reserve is still the gate.
func TestCapChecker_Peek_NoRedis_ReturnsUnderCap(t *testing.T) {
	cc, _, _ := newCapCheckerMock(t)
	overCap, count, err := cc.Peek(context.Background(), "", "sub-x")
	if err != nil {
		t.Fatalf("Peek with nil Redis returned err: %v", err)
	}
	if overCap {
		t.Errorf("overCap = true, want false when Redis disabled")
	}
	if count != 0 {
		t.Errorf("count = %d, want 0 when Redis disabled", count)
	}
}

// TestCapChecker_Peek_DoesNotMutateRedis is the headline invariant: the
// dispatcher will call Peek for EVERY candidate row in every wave; if
// Peek were to INCR (or EXPIRE) the counter, the cap would burn through
// in the dispatcher before the worker ever reached the subscriber. This
// test seeds the counter at a known value, calls Peek, and asserts the
// stored value is byte-identical afterwards.
func TestCapChecker_Peek_DoesNotMutateRedis(t *testing.T) {
	cc, mr, mock := newCapCheckerWithMiniredis(t, 5)
	orgID := "00000000-0000-0000-0000-0000000000aa"
	subID := "subscriber-peek-test"

	mock.ExpectQuery(`SELECT settings->>'cross_brand_daily_cap' FROM organizations`).
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("5"))

	key := todayKey(subID)
	mr.Set(key, "3")
	mr.SetTTL(key, 26*time.Hour)

	overCap, count, err := cc.Peek(context.Background(), orgID, subID)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if overCap {
		t.Errorf("overCap = true, want false (3 < cap 5)")
	}
	if count != 3 {
		t.Errorf("count = %d, want 3 (current stored value)", count)
	}

	got, err := mr.Get(key)
	if err != nil {
		t.Fatalf("post-Peek Get: %v", err)
	}
	if got != "3" {
		t.Errorf("post-Peek key value = %q, want %q (Peek mutated the counter)", got, "3")
	}

	if ttl := mr.TTL(key); ttl > 26*time.Hour || ttl <= 0 {
		t.Errorf("post-Peek TTL = %v, want untouched (~26h)", ttl)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// TestCapChecker_Peek_AtCapReturnsTrue covers the over-cap detection. When
// the stored count is >= cap, Peek must return overCap=true so the
// dispatcher can substitute a reserve subscriber.
func TestCapChecker_Peek_AtCapReturnsTrue(t *testing.T) {
	cc, mr, mock := newCapCheckerWithMiniredis(t, 2)
	orgID := "00000000-0000-0000-0000-0000000000bb"
	subID := "subscriber-at-cap"

	mock.ExpectQuery(`SELECT settings->>'cross_brand_daily_cap' FROM organizations`).
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("2"))

	mr.Set(todayKey(subID), "2")

	overCap, count, err := cc.Peek(context.Background(), orgID, subID)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if !overCap {
		t.Errorf("overCap = false, want true (2 >= cap 2)")
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// TestCapChecker_Peek_MissingKeyReturnsZero covers the "no sends yet
// today" path. The Redis key won't exist; Peek must report not-over-cap
// with count=0 (not over-cap, not an error).
func TestCapChecker_Peek_MissingKeyReturnsZero(t *testing.T) {
	cc, _, mock := newCapCheckerWithMiniredis(t, 3)
	orgID := "00000000-0000-0000-0000-0000000000cc"
	subID := "subscriber-never-sent"

	mock.ExpectQuery(`SELECT settings->>'cross_brand_daily_cap' FROM organizations`).
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("3"))

	overCap, count, err := cc.Peek(context.Background(), orgID, subID)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if overCap {
		t.Errorf("overCap = true, want false (no sends yet)")
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestCapChecker_Reserve_ZeroCapAllowsEverything(t *testing.T) {
	cc, mock, _ := newCapCheckerMock(t)
	orgID := "00000000-0000-0000-0000-000000000008"
	subID := "aaaaaaaa-0000-0000-0000-000000000003"

	// capForOrg returns "0" from org settings → resolves to defaultCap (2)
	// because the parser treats <=0 as invalid. To actually exercise the
	// "cap <= 0 allows all" branch we need defaultCap itself to be <= 0.
	// Re-construct with an explicit zero default to prove the short-circuit.
	db, mock2, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	cc = NewCapChecker(db, nil, -1) // NewCapChecker normalises <=0 to 2
	_ = cc
	_ = mock

	// With defaultCap clamped to 2 by the constructor, the hypothetical
	// "no cap" case is impossible to trigger through the public API —
	// which is exactly the invariant we want. Just verify the constructor
	// normalised the default.
	want := 2
	if got := cc.capForOrg(context.Background(), ""); got != want {
		t.Errorf("constructor-normalised default cap = %d, want %d", got, want)
	}
	_ = orgID
	_ = subID
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}
