package worker

// Tests for PartnerHumanRollupWorker (REQ-035/038). These pin EXPECTED
// BEHAVIOR, not implementation trivia:
//
//   - the upsert is (dataset_id, day)-keyed ON CONFLICT DO UPDATE that never
//     clobbers the engaged_tier_members snapshot,
//   - the event counts are verdict-filtered (the whole point of the rollup),
//   - the query shape is campaign-first enumeration (mailing_campaigns by
//     partner_dataset_id + the [partner-drip] name family) — NEVER the naive
//     cohort JOIN mailing_message_log that timed out at 580s (G3, 2026-07-13),
//   - one dataset failing does not stop the pass (per-dataset isolation),
//   - covered historical chunks are skipped (the REQ-038 resume mechanism),
//   - the distributed lock gates the entire pass.
//
// Pattern follows storage_guard_test.go / partner_test.go: sqlmock with the
// regex matcher, no real DB.

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newRollupMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
		sqlmock.MonitorPingsOption(false),
	)
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

// expectRollupBookkeeping registers the best-effort trailing writes every
// pass performs. ORDER MATTERS: sqlmock is ordered by default and Go runs
// defers LIFO, so the heartbeat/run-summary defer (registered last) fires
// BEFORE the lock-release defer (registered first) — heartbeat, run summary,
// retention prune, THEN pg_advisory_unlock.
func expectRollupBookkeeping(mock sqlmock.Sqlmock) {
	mock.ExpectExec("INSERT INTO mailing_worker_heartbeats").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO mailing_worker_runs").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM mailing_worker_runs").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("pg_advisory_unlock").WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectChunk registers one successful dataset×range chunk transaction.
func expectChunk(mock sqlmock.Sqlmock, upsertPattern string) {
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(upsertPattern).WillReturnResult(sqlmock.NewResult(0, 7))
	mock.ExpectCommit()
}

// expectEngagedTier registers the yesterday snapshot transaction.
func expectEngagedTier(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("engaged_tier_members = EXCLUDED.engaged_tier_members")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

// TestPartnerHumanRollup_UpsertShape pins the load-bearing properties of the
// rollup statement: verdict-filtered counts, campaign-first enumeration,
// (dataset_id, day) upsert, and — critically — that the event upsert does NOT
// overwrite the engaged_tier_members snapshot.
func TestPartnerHumanRollup_UpsertShape(t *testing.T) {
	// Static assertions on the SQL text itself (the strongest, cheapest pin).
	sql := partnerHumanRollupUpsertSQL
	for _, want := range []string{
		"ignite_verdict_is_human(ignite_event_verdict(te.user_agent, te.ip_address))",                             // human opens
		"ignite_verdict_is_human(COALESCE(te.click_verdict, ignite_event_verdict(te.user_agent, te.ip_address)))", // human clicks
		"partner_dataset_id = $1",                 // campaign-first enumeration key
		"name LIKE '[partner-drip]%'",             // drip name family
		"ON CONFLICT (dataset_id, day) DO UPDATE", // re-run safe
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("rollup upsert SQL missing %q", want)
		}
	}
	if strings.Contains(sql, "mailing_message_log") {
		t.Fatalf("rollup upsert must NOT touch mailing_message_log (the 580s cohort-join anti-pattern)")
	}
	// The DO UPDATE column list must not include engaged_tier_members —
	// that column is owned by the snapshot statement.
	updateClause := sql[strings.Index(sql, "DO UPDATE"):]
	if strings.Contains(updateClause, "engaged_tier_members") {
		t.Fatalf("event upsert must not clobber engaged_tier_members: %s", updateClause)
	}

	// End-to-end pass over sqlmock: lock → datasets → chunk tx → snapshot tx.
	db, mock := newRollupMockDB(t)
	mock.ExpectQuery("pg_try_advisory_lock").
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	mock.ExpectQuery("SELECT id FROM partner_datasets").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("11111111-1111-1111-1111-111111111111"))
	expectChunk(mock, regexp.QuoteMeta("ON CONFLICT (dataset_id, day) DO UPDATE"))
	expectEngagedTier(mock)
	expectRollupBookkeeping(mock)

	w := NewPartnerHumanRollupWorker(db, nil).WithWindows(7, 7, 7)
	w.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestPartnerHumanRollup_PerDatasetIsolation proves one dataset's query
// failure does not stop the pass: dataset A's chunk errors, dataset B still
// gets its chunk AND its engaged-tier snapshot.
func TestPartnerHumanRollup_PerDatasetIsolation(t *testing.T) {
	db, mock := newRollupMockDB(t)
	dsA := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	dsB := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	mock.ExpectQuery("pg_try_advisory_lock").
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	mock.ExpectQuery("SELECT id FROM partner_datasets").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(dsA).AddRow(dsB))

	// Dataset A: chunk statement fails → tx rolls back, dataset abandoned.
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("ON CONFLICT (dataset_id, day) DO UPDATE")).
		WithArgs(dsA, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(errors.New("canceling statement due to statement timeout"))
	mock.ExpectRollback()

	// Dataset B: full success.
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("ON CONFLICT (dataset_id, day) DO UPDATE")).
		WithArgs(dsB, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 7))
	mock.ExpectCommit()
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("engaged_tier_members = EXCLUDED.engaged_tier_members")).
		WithArgs(dsB, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	expectRollupBookkeeping(mock)

	w := NewPartnerHumanRollupWorker(db, nil).WithWindows(7, 7, 7)
	w.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("dataset B must complete despite dataset A failing: %v", err)
	}
}

// TestPartnerHumanRollup_DistlockNotAcquired proves the pass is fully gated
// by the distributed lock: when another instance holds it, no dataset work,
// no writes, no bookkeeping happen.
func TestPartnerHumanRollup_DistlockNotAcquired(t *testing.T) {
	db, mock := newRollupMockDB(t)
	mock.ExpectQuery("pg_try_advisory_lock").
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(false))

	w := NewPartnerHumanRollupWorker(db, nil)
	w.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestPartnerHumanRollup_BackfillSkipsCoveredChunks proves the REQ-038 resume
// mechanism: a fully-covered historical chunk is skipped (coverage count ==
// chunk days), an uncovered one is computed, and trailing chunks recompute
// unconditionally.
func TestPartnerHumanRollup_BackfillSkipsCoveredChunks(t *testing.T) {
	db, mock := newRollupMockDB(t)
	ds := "11111111-1111-1111-1111-111111111111"

	mock.ExpectQuery("pg_try_advisory_lock").
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	mock.ExpectQuery("SELECT id FROM partner_datasets").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(ds))

	// backfill=21d, trailing=7d, chunk=7d → 3 chunks. Chunk 1 (oldest,
	// historical): covered → skipped. Chunk 2 (historical): 3/7 days present
	// → recomputed. Chunk 3 (trailing window): computed WITHOUT a coverage
	// check.
	mock.ExpectQuery(regexp.QuoteMeta("FROM partner_dataset_human_rollup")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(7))
	mock.ExpectQuery(regexp.QuoteMeta("FROM partner_dataset_human_rollup")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	expectChunk(mock, regexp.QuoteMeta("ON CONFLICT (dataset_id, day) DO UPDATE")) // chunk 2
	expectChunk(mock, regexp.QuoteMeta("ON CONFLICT (dataset_id, day) DO UPDATE")) // chunk 3
	expectEngagedTier(mock)
	expectRollupBookkeeping(mock)

	w := NewPartnerHumanRollupWorker(db, nil).WithWindows(21, 7, 7)
	w.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestPartnerHumanRollup_ContextCancellation proves the worker honors ctx:
// a cancelled context stops the pass without touching the DB.
func TestPartnerHumanRollup_ContextCancellation(t *testing.T) {
	db, mock := newRollupMockDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w := NewPartnerHumanRollupWorker(db, nil)
	w.RunOnce(ctx)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("cancelled ctx must issue no queries: %v", err)
	}
}

// TestPartnerHumanRollup_DailyAnchor sanity-checks the 03:10 UTC tick math.
func TestPartnerHumanRollup_DailyAnchor(t *testing.T) {
	w := NewPartnerHumanRollupWorker(nil, nil)
	before := time.Date(2026, 7, 13, 1, 0, 0, 0, time.UTC)
	if got := w.timeUntilNextTick(before); got != 2*time.Hour+10*time.Minute {
		t.Fatalf("before-anchor: want 2h10m, got %s", got)
	}
	after := time.Date(2026, 7, 13, 4, 0, 0, 0, time.UTC)
	if got := w.timeUntilNextTick(after); got != 23*time.Hour+10*time.Minute {
		t.Fatalf("after-anchor: want 23h10m, got %s", got)
	}
}
