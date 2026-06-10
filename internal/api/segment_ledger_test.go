package api

// Tests for the segment build ledger (mailing_segment_build_ledger):
// pure-logic helpers (stale-running coercion, error truncation, delta
// percentage) plus sqlmock coverage of the upsert/mark-running SQL and the
// nil-DB / cancelled-context best-effort guarantees.

import (
	"context"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// ---------- coerceLedgerStatus ----------

func TestCoerceLedgerStatusStaleRunningBecomesFailed(t *testing.T) {
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-ledgerRunningStaleAfter - time.Minute)
	if got := coerceLedgerStatus("running", stale, now); got != "failed" {
		t.Fatalf("stale running: got %q, want failed", got)
	}
}

func TestCoerceLedgerStatusFreshRunningStaysRunning(t *testing.T) {
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-5 * time.Minute)
	if got := coerceLedgerStatus("running", fresh, now); got != "running" {
		t.Fatalf("fresh running: got %q, want running", got)
	}
}

func TestCoerceLedgerStatusExactBoundaryStaysRunning(t *testing.T) {
	// Coercion requires STRICTLY older than the threshold.
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	boundary := now.Add(-ledgerRunningStaleAfter)
	if got := coerceLedgerStatus("running", boundary, now); got != "running" {
		t.Fatalf("boundary running: got %q, want running", got)
	}
}

func TestCoerceLedgerStatusNonRunningNeverCoerced(t *testing.T) {
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	ancient := now.Add(-48 * time.Hour)
	for _, s := range []string{"ok", "failed", "blocked_delta", "unknown"} {
		if got := coerceLedgerStatus(s, ancient, now); got != s {
			t.Fatalf("status %q: got %q, want unchanged", s, got)
		}
	}
}

// ---------- truncateLedgerError ----------

func TestTruncateLedgerErrorShortUnchanged(t *testing.T) {
	if got := truncateLedgerError("boom"); got != "boom" {
		t.Fatalf("got %q", got)
	}
	if got := truncateLedgerError(""); got != "" {
		t.Fatalf("empty: got %q", got)
	}
}

func TestTruncateLedgerErrorLongCappedAt500(t *testing.T) {
	long := strings.Repeat("x", 2000)
	got := truncateLedgerError(long)
	if len(got) != ledgerErrorMaxLen {
		t.Fatalf("len=%d, want %d", len(got), ledgerErrorMaxLen)
	}
	if got != long[:ledgerErrorMaxLen] {
		t.Fatalf("content mismatch after truncation")
	}
}

func TestTruncateLedgerErrorBacksOffToValidUTF8(t *testing.T) {
	// Place a multi-byte rune straddling the 500-byte cut so naive byte
	// slicing would produce invalid UTF-8 (which Postgres TEXT rejects).
	msg := strings.Repeat("a", ledgerErrorMaxLen-1) + "€" /* 3 bytes */ + strings.Repeat("b", 100)
	got := truncateLedgerError(msg)
	if len(got) > ledgerErrorMaxLen {
		t.Fatalf("len=%d exceeds cap", len(got))
	}
	if !strings.HasPrefix(got, strings.Repeat("a", ledgerErrorMaxLen-1)) {
		t.Fatalf("unexpected prefix after truncation")
	}
	for _, r := range got {
		if r == '�' {
			t.Fatalf("truncated string contains replacement rune — invalid UTF-8 slice")
		}
	}
}

// ---------- computeLedgerDeltaPct ----------

func TestComputeLedgerDeltaPct(t *testing.T) {
	cases := []struct {
		prev, cur int64
		want      float64
	}{
		{0, 1000, 0},    // no prior count → 0 by contract
		{0, 0, 0},       // nothing → nothing
		{-5, 100, 0},    // defensive: nonsense prior → 0
		{1000, 1000, 0}, // unchanged
		{1000, 1200, 20},
		{1000, 900, -10},
		{200, 0, -100}, // emptied out
	}
	for _, c := range cases {
		if got := computeLedgerDeltaPct(c.prev, c.cur); got != c.want {
			t.Fatalf("delta(%d → %d) = %v, want %v", c.prev, c.cur, got, c.want)
		}
	}
}

// ---------- nil-DB safety (best-effort contract) ----------

func TestLedgerWritesNilDBAreNoOps(t *testing.T) {
	ctx := context.Background()
	if err := UpsertSegmentLedger(ctx, nil, "00000000-0000-0000-0000-000000000001", 10, "materializer", "ok", 5, 0, ""); err != nil {
		t.Fatalf("UpsertSegmentLedger nil db: %v", err)
	}
	if err := MarkSegmentBuildRunning(ctx, nil, "00000000-0000-0000-0000-000000000001", "materializer"); err != nil {
		t.Fatalf("MarkSegmentBuildRunning nil db: %v", err)
	}
	if n, ok := ledgerPriorCount(ctx, nil, "00000000-0000-0000-0000-000000000001"); n != 0 || ok {
		t.Fatalf("ledgerPriorCount nil db: got (%d,%v), want (0,false)", n, ok)
	}
}

// ---------- SQL shape via sqlmock ----------

const testLedgerSegID = "11111111-2222-3333-4444-555555555555"

func TestUpsertSegmentLedgerSQL(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO mailing_segment_build_ledger`).
		WithArgs(testLedgerSegID, int64(17234), "recalculate", "ok", 4821, 12.5, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := UpsertSegmentLedger(context.Background(), db, testLedgerSegID, 17234, "recalculate", "ok", 4821, 12.5, ""); err != nil {
		t.Fatalf("UpsertSegmentLedger: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// last_built_at and subscriber_count must only advance on status='ok': a
// failed/blocked build keeps the last good freshness timestamp and audience
// count. The statement enforces this in SQL (CASE on the bound status / on
// EXCLUDED.last_build_status), so we pin the guards' presence here —
// sqlmock's default regexp matcher treats the expectation as a pattern over
// the executed SQL.
func TestUpsertSegmentLedgerOnlyAdvancesBuiltAtAndCountOnOK(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// INSERT branch: last_built_at is NULL unless the bound status is 'ok'.
	// ON CONFLICT branch: both fields keep the stored value unless
	// EXCLUDED.last_build_status = 'ok'.
	pattern := `(?s)CASE WHEN \$4::text = 'ok' THEN NOW\(\) END.*` +
		`subscriber_count\s+= CASE WHEN EXCLUDED\.last_build_status = 'ok'.*` +
		`ELSE mailing_segment_build_ledger\.subscriber_count END.*` +
		`last_built_at\s+= CASE WHEN EXCLUDED\.last_build_status = 'ok'.*` +
		`ELSE mailing_segment_build_ledger\.last_built_at END`
	mock.ExpectExec(pattern).
		WithArgs(testLedgerSegID, int64(9001), "lake-builder", "blocked_delta", 1200, -100.0, "delta guard tripped").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := UpsertSegmentLedger(context.Background(), db, testLedgerSegID, 9001, "lake-builder", "blocked_delta", 1200, -100.0, "delta guard tripped"); err != nil {
		t.Fatalf("UpsertSegmentLedger: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestUpsertSegmentLedgerTruncatesErrMsgBeforeBind(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	long := strings.Repeat("e", 2000)
	mock.ExpectExec(`INSERT INTO mailing_segment_build_ledger`).
		WithArgs(testLedgerSegID, int64(0), "materializer", "failed", 100, 0.0, long[:ledgerErrorMaxLen]).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := UpsertSegmentLedger(context.Background(), db, testLedgerSegID, 0, "materializer", "failed", 100, 0, long); err != nil {
		t.Fatalf("UpsertSegmentLedger: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestMarkSegmentBuildRunningSQL(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO mailing_segment_build_ledger`).
		WithArgs(testLedgerSegID, "materializer").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := MarkSegmentBuildRunning(context.Background(), db, testLedgerSegID, "materializer"); err != nil {
		t.Fatalf("MarkSegmentBuildRunning: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Terminal ledger writes must survive a caller context that has already been
// cancelled (e.g. a build that hit its 10-minute timeout recording 'failed'):
// ledgerWriteCtx detaches to a background context in that case.
func TestUpsertSegmentLedgerSurvivesCancelledContext(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO mailing_segment_build_ledger`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead before the write

	if err := UpsertSegmentLedger(ctx, db, testLedgerSegID, 5, "materializer", "failed", 9, 0, "timed out"); err != nil {
		t.Fatalf("UpsertSegmentLedger with cancelled ctx: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ---------- ledgerPriorCount semantics ----------

func TestLedgerPriorCountRequiresCompletedBuild(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// Row exists but was only ever marked 'running' (last_built_at NULL):
	// must report no usable prior count.
	mock.ExpectQuery(`SELECT subscriber_count, last_built_at`).
		WithArgs(testLedgerSegID).
		WillReturnRows(sqlmock.NewRows([]string{"subscriber_count", "last_built_at"}).AddRow(int64(0), nil))

	if n, ok := ledgerPriorCount(context.Background(), db, testLedgerSegID); ok || n != 0 {
		t.Fatalf("running-only row: got (%d,%v), want (0,false)", n, ok)
	}

	// Completed build: count is usable.
	built := time.Date(2026, 6, 8, 4, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT subscriber_count, last_built_at`).
		WithArgs(testLedgerSegID).
		WillReturnRows(sqlmock.NewRows([]string{"subscriber_count", "last_built_at"}).AddRow(int64(9001), built))

	if n, ok := ledgerPriorCount(context.Background(), db, testLedgerSegID); !ok || n != 9001 {
		t.Fatalf("completed row: got (%d,%v), want (9001,true)", n, ok)
	}

	// No row at all: degrade to (0,false), never error.
	mock.ExpectQuery(`SELECT subscriber_count, last_built_at`).
		WithArgs(testLedgerSegID).
		WillReturnError(context.DeadlineExceeded)

	if n, ok := ledgerPriorCount(context.Background(), db, testLedgerSegID); ok || n != 0 {
		t.Fatalf("query error: got (%d,%v), want (0,false)", n, ok)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
