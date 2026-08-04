package worker

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestCleanupTerminalQueueItems_RowAgeDriven locks the 2026-06-28 fix: the
// terminal-queue purge must age by the QUEUE ROW's own clock (terminal status +
// COALESCE(updated_at, created_at) < 14d), matching StorageGuard — NOT by the
// owning campaign's updated_at (which engagement counters keep bumping, so
// aged terminal rows on still-active campaigns never got purged). The DELETE
// regex below requires the terminal-status list AND the row-age predicate; the
// old campaign-driven shape (WHERE campaign_id = $1, no status/age filter,
// joined to mailing_campaigns) would fail this expectation.
func TestCleanupTerminalQueueItems_RowAgeDriven(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// primaryBusy probe → 0 backends in IO wait → not busy, purge proceeds.
	mock.ExpectQuery(`pg_stat_activity`).WillReturnRows(
		sqlmock.NewRows([]string{"count"}).AddRow(0),
	)

	// The purge DELETE: terminal-status list → row-age predicate → DELETE on the
	// queue. 5 rows affected (< batch size) breaks the loop after one batch.
	mock.ExpectExec(`(?s)status IN \('accepted','cancelled','failed','dead_letter','dead_letter_strict'\).*COALESCE\(updated_at, created_at\) < NOW\(\) - INTERVAL '14 days'.*ORDER BY status, COALESCE\(updated_at, created_at\).*DELETE FROM mailing_campaign_queue`).
		WillReturnResult(sqlmock.NewResult(0, 5))

	// logTerminalQueueStats: two COUNT(*) probes (aged terminal + accepted-html).
	mock.ExpectQuery(`(?s)COUNT\(\*\).*status IN \('accepted', 'cancelled', 'failed', 'dead_letter', 'dead_letter_strict'\)`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`(?s)COUNT\(\*\).*status = 'accepted' AND html_content IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	dc := &DataCleanupWorker{db: db, interval: time.Hour}
	dc.cleanupTerminalQueueItems(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestCleanupTerminalQueueItems_DefersWhenPrimaryBusy verifies the IO-load gate:
// when many backends are in IO wait, the purge defers (no DELETE issued) and
// only logs stats, so it never piles WAL onto an active IO storm.
func TestCleanupTerminalQueueItems_DefersWhenPrimaryBusy(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// primaryBusy probe → many backends in IO wait → busy → defer.
	mock.ExpectQuery(`pg_stat_activity`).WillReturnRows(
		sqlmock.NewRows([]string{"count"}).AddRow(slimMaxIOWaitBackends + 1),
	)
	// No ExpectExec for a DELETE — purge must NOT run. Only stats are logged.
	mock.ExpectQuery(`(?s)COUNT\(\*\).*status IN \('accepted', 'cancelled', 'failed', 'dead_letter', 'dead_letter_strict'\)`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`(?s)COUNT\(\*\).*html_content IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	dc := &DataCleanupWorker{db: db, interval: time.Hour}
	dc.cleanupTerminalQueueItems(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}
