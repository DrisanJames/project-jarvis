package worker

// Tests for VerifiedHumansLedgerWorker (docs/JAOS/core.md §14). These pin
// EXPECTED BEHAVIOR, not implementation trivia:
//
//   - the member insert is ACCRETIVE: ON CONFLICT (segment_id, subscriber_id)
//     DO NOTHING and NO DELETE anywhere (a proven human is never removed),
//   - membership is verdict-HUMAN-filtered on BOTH opens and clicks (the whole
//     point of the ledger), clicks use the materialized click_verdict first and
//     EXCLUDE unsub-link clicks,
//   - coverage resumes from the per-segment high-water mark (scanned_through),
//     starting at today-backfill when unset, and is bounded per pass,
//   - one segment failing does not stop the pass (per-segment isolation),
//   - the distributed lock gates the entire pass,
//   - a cancelled context issues no queries.
//
// Pattern follows partner_human_rollup_test.go: sqlmock with the regex matcher,
// no real DB.

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

func newLedgerMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
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

// expectLedgerBookkeeping registers the trailing best-effort writes every pass
// performs. Defers run LIFO (lock-release registered first fires LAST), so the
// order is: heartbeat, run summary, run-retention prune, THEN pg_advisory_unlock.
func expectLedgerBookkeeping(mock sqlmock.Sqlmock) {
	mock.ExpectExec("INSERT INTO mailing_worker_heartbeats").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO mailing_worker_runs").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM mailing_worker_runs").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("pg_advisory_unlock").WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectLedgerChunk registers one successful segment×range chunk transaction
// followed by its progress upsert + subscriber_count refresh.
func expectLedgerChunk(mock sqlmock.Sqlmock, segmentID, domain string, added int64) {
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("ON CONFLICT (segment_id, subscriber_id) DO NOTHING")).
		WithArgs(segmentID, sqlmock.AnyArg(), sqlmock.AnyArg(), domain).
		WillReturnResult(sqlmock.NewResult(0, added))
	mock.ExpectCommit()
	mock.ExpectExec(regexp.QuoteMeta("INTO mailing_verified_humans_ledger_progress")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE mailing_segments").
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func ledgerSegmentRows(scannedThrough interface{}) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "name", "sending_domain", "scanned_through"}).
		AddRow("11111111-1111-1111-1111-111111111111", "BWP Verified Humans", "businessweeklypro.com", scannedThrough)
}

// TestVerifiedHumansLedger_InsertShape is the strongest, cheapest pin: the
// classification predicate and the ADD-ONLY guarantee live in the SQL text.
func TestVerifiedHumansLedger_InsertShape(t *testing.T) {
	sql := verifiedHumansMemberInsertSQL
	for _, want := range []string{
		"ignite_verdict_is_human(ignite_event_verdict(te.user_agent, te.ip_address))",                             // human opens
		"ignite_verdict_is_human(COALESCE(te.click_verdict, ignite_event_verdict(te.user_agent, te.ip_address)))", // human clicks (click_verdict first)
		"NOT ILIKE '%/track/unsubscribe%'",                     // unsub-click exclusion
		"ON CONFLICT (segment_id, subscriber_id) DO NOTHING",   // accretive / re-run safe
		"JOIN mailing_subscribers s ON s.id = q.subscriber_id", // email comes from subscribers (te has none)
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("member insert SQL missing %q", want)
		}
	}
	// The ledger must NEVER remove a human: no DELETE, no TRUNCATE in the
	// maintenance statement. This is the load-bearing invariant (a DELETE+INSERT
	// rebuild is exactly the SegmentMaterializer anti-pattern this worker avoids).
	if strings.Contains(strings.ToUpper(sql), "DELETE") || strings.Contains(strings.ToUpper(sql), "TRUNCATE") {
		t.Fatalf("member insert must be ADD-ONLY — found a DELETE/TRUNCATE: %s", sql)
	}
	if !strings.Contains(verifiedHumansProgressUpsertSQL, "ON CONFLICT (segment_id) DO UPDATE") {
		t.Fatalf("progress upsert must be re-run safe (ON CONFLICT (segment_id) DO UPDATE)")
	}
}

// TestVerifiedHumansLedger_Pass drives a full pass over sqlmock: lock →
// listSegments (scanned_through NULL → cold start) → one chunk tx → progress +
// count refresh → bookkeeping. backfill=chunk=7 so exactly one chunk fires.
func TestVerifiedHumansLedger_Pass(t *testing.T) {
	db, mock := newLedgerMockDB(t)
	seg := "11111111-1111-1111-1111-111111111111"
	domain := "businessweeklypro.com"

	mock.ExpectQuery("pg_try_advisory_lock").
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_segments s")).
		WillReturnRows(ledgerSegmentRows(nil))
	expectLedgerChunk(mock, seg, domain, 5)
	expectLedgerBookkeeping(mock)

	w := NewVerifiedHumansLedgerWorker(db, nil).WithWindows(7, 10, 7, 10)
	w.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestVerifiedHumansLedger_ResumeFromHighWaterMark proves the coverage cursor:
// with scanned_through set to ~4 days ago and trailing=10d, the scan starts at
// (scanned_through-10d) and, with chunk=7d, advances in the expected number of
// bounded chunks (2) — NOT a full backfill.
func TestVerifiedHumansLedger_ResumeFromHighWaterMark(t *testing.T) {
	db, mock := newLedgerMockDB(t)
	seg := "11111111-1111-1111-1111-111111111111"
	domain := "businessweeklypro.com"
	scanned := time.Now().UTC().Add(-4 * 24 * time.Hour) // window = [scanned-10d, today) ≈ 14d → 2 weekly chunks

	mock.ExpectQuery("pg_try_advisory_lock").
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_segments s")).
		WillReturnRows(ledgerSegmentRows(scanned))
	expectLedgerChunk(mock, seg, domain, 2) // chunk 1
	expectLedgerChunk(mock, seg, domain, 1) // chunk 2
	expectLedgerBookkeeping(mock)

	w := NewVerifiedHumansLedgerWorker(db, nil).WithWindows(240, 10, 7, 10)
	w.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("resume window must be bounded to 2 chunks: %v", err)
	}
}

// TestVerifiedHumansLedger_MaxChunksCap proves the per-pass cap: a cold start
// (scanned_through NULL) with backfill=21d and chunk=7d would be 3 chunks, but
// maxChunksPerPass=2 stops at 2 — the rest resumes next night.
func TestVerifiedHumansLedger_MaxChunksCap(t *testing.T) {
	db, mock := newLedgerMockDB(t)
	seg := "11111111-1111-1111-1111-111111111111"
	domain := "businessweeklypro.com"

	mock.ExpectQuery("pg_try_advisory_lock").
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_segments s")).
		WillReturnRows(ledgerSegmentRows(nil))
	expectLedgerChunk(mock, seg, domain, 3) // chunk 1
	expectLedgerChunk(mock, seg, domain, 4) // chunk 2 (cap) — chunk 3 NOT run
	expectLedgerBookkeeping(mock)

	w := NewVerifiedHumansLedgerWorker(db, nil).WithWindows(21, 10, 7, 2)
	w.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("max-chunks cap must stop at 2 chunks: %v", err)
	}
}

// TestVerifiedHumansLedger_PerSegmentIsolation proves one brand's chunk failure
// does not stop the pass: segment A's insert errors (tx rolls back, no progress
// write), segment B still gets its chunk + progress.
func TestVerifiedHumansLedger_PerSegmentIsolation(t *testing.T) {
	db, mock := newLedgerMockDB(t)
	segA := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	segB := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	mock.ExpectQuery("pg_try_advisory_lock").
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_segments s")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "sending_domain", "scanned_through"}).
			AddRow(segA, "AAA Verified Humans", "aaa.com", nil).
			AddRow(segB, "BBB Verified Humans", "bbb.com", nil))

	// Segment A: insert fails → rollback → segment abandoned (NO progress write).
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("ON CONFLICT (segment_id, subscriber_id) DO NOTHING")).
		WithArgs(segA, sqlmock.AnyArg(), sqlmock.AnyArg(), "aaa.com").
		WillReturnError(errors.New("canceling statement due to statement timeout"))
	mock.ExpectRollback()

	// Segment B: full success.
	expectLedgerChunk(mock, segB, "bbb.com", 9)
	expectLedgerBookkeeping(mock)

	w := NewVerifiedHumansLedgerWorker(db, nil).WithWindows(7, 10, 7, 10)
	w.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("segment B must complete despite segment A failing: %v", err)
	}
}

// TestVerifiedHumansLedger_DistlockNotAcquired proves the pass is fully gated by
// the lock: when another instance holds it, no segment work, no writes happen.
func TestVerifiedHumansLedger_DistlockNotAcquired(t *testing.T) {
	db, mock := newLedgerMockDB(t)
	mock.ExpectQuery("pg_try_advisory_lock").
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(false))

	w := NewVerifiedHumansLedgerWorker(db, nil)
	w.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestVerifiedHumansLedger_ContextCancellation proves a cancelled context stops
// the pass without touching the DB.
func TestVerifiedHumansLedger_ContextCancellation(t *testing.T) {
	db, mock := newLedgerMockDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w := NewVerifiedHumansLedgerWorker(db, nil)
	w.RunOnce(ctx)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("cancelled ctx must issue no queries: %v", err)
	}
}

// TestVerifiedHumansLedger_DailyAnchor sanity-checks the 03:40 UTC tick math.
func TestVerifiedHumansLedger_DailyAnchor(t *testing.T) {
	w := NewVerifiedHumansLedgerWorker(nil, nil)
	before := time.Date(2026, 7, 16, 1, 0, 0, 0, time.UTC)
	if got := w.timeUntilNextTick(before); got != 2*time.Hour+40*time.Minute {
		t.Fatalf("before-anchor: want 2h40m, got %s", got)
	}
	after := time.Date(2026, 7, 16, 4, 0, 0, 0, time.UTC)
	if got := w.timeUntilNextTick(after); got != 23*time.Hour+40*time.Minute {
		t.Fatalf("after-anchor: want 23h40m, got %s", got)
	}
}
