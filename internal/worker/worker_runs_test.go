package worker

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/DATA-DOG/go-sqlmock"
)

var (
	runInsert = regexp.MustCompile(`INSERT INTO mailing_worker_runs`)
	runPrune  = regexp.MustCompile(`DELETE FROM mailing_worker_runs`)
)

// Happy path: one INSERT with the computed duration, then the opportunistic
// 30-day prune for the same worker_name.
func TestRecordWorkerRun_InsertsAndPrunes(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(runInsert.String()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(runPrune.String()).
		WillReturnResult(sqlmock.NewResult(0, 3))

	RecordWorkerRun(context.Background(), db, "segment_refresh",
		time.Now().Add(-2*time.Second), "ok", 14, 2, "refreshed 14 dynamic segments (2 failed)")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Best-effort: an INSERT failure must be swallowed (no panic, no propagation)
// and must skip the prune rather than hammer a broken table.
func TestRecordWorkerRun_InsertErrorIsSwallowed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(runInsert.String()).
		WillReturnError(errors.New("relation does not exist"))

	RecordWorkerRun(context.Background(), db, "segment_cleanup",
		time.Now(), "failed", 0, 1, "cycle error: boom")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// A prune failure is ignored — the run row already landed.
func TestRecordWorkerRun_PruneErrorIsIgnored(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(runInsert.String()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(runPrune.String()).
		WillReturnError(errors.New("deadlock detected"))

	RecordWorkerRun(context.Background(), db, "segment_refresh",
		time.Now(), "partial", 5, 1, "refreshed 5 dynamic segments (1 failed)")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// nil DB / empty worker name are no-ops (mirrors EmitHeartbeat's guard).
func TestRecordWorkerRun_NilGuards(t *testing.T) {
	RecordWorkerRun(context.Background(), nil, "segment_refresh", time.Now(), "ok", 0, 0, "")

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	RecordWorkerRun(context.Background(), db, "", time.Now(), "ok", 0, 0, "")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// A worker that is shutting down (cancelled ctx) must still land its terminal
// run row — RecordWorkerRun detaches to a background context.
func TestRecordWorkerRun_DetachesFromCancelledContext(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(runInsert.String()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(runPrune.String()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	RecordWorkerRun(ctx, db, "segment_cleanup", time.Now(), "ok", 1, 0, "warned 1, archived/deactivated 0")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestTruncateRunDetail(t *testing.T) {
	if got := truncateRunDetail("short"); got != "short" {
		t.Fatalf("short detail mutated: %q", got)
	}

	long := strings.Repeat("a", runDetailMaxLen+50)
	if got := truncateRunDetail(long); len(got) != runDetailMaxLen {
		t.Fatalf("expected %d bytes, got %d", runDetailMaxLen, len(got))
	}

	// Multibyte runes: never cut mid-rune.
	multibyte := strings.Repeat("é", runDetailMaxLen) // 2 bytes each
	got := truncateRunDetail(multibyte)
	if len(got) > runDetailMaxLen {
		t.Fatalf("truncated detail too long: %d bytes", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated detail is not valid UTF-8")
	}
}
