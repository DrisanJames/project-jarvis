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
// when EVERY gate sample shows sustained IO-wait saturation at the purge's own
// (higher) threshold, the purge defers (no DELETE issued) and only logs stats.
func TestCleanupTerminalQueueItems_DefersWhenPrimaryBusy(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// purgeGateBusy: all samples at/above the purge threshold → busy → defer.
	for i := 0; i < purgeGateSamples; i++ {
		mock.ExpectQuery(`pg_stat_activity`).WillReturnRows(
			sqlmock.NewRows([]string{"count"}).AddRow(terminalPurgeMaxIOWaitBackends + 1),
		)
	}
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

// TestCleanupTerminalQueueItems_RunsAboveSlimThreshold locks the aug15 fix:
// the purge no longer borrows the slimmer's threshold (8). A baseline IO-wait
// count above the slim threshold but below the purge threshold must NOT defer
// the purge — this exact condition (steady-state 12-21 backends) starved the
// purge for ~43 of 45 hourly cycles while the backlog grew to 1.75M rows.
func TestCleanupTerminalQueueItems_RunsAboveSlimThreshold(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// First gate sample: above slim threshold, below purge threshold → run.
	mock.ExpectQuery(`pg_stat_activity`).WillReturnRows(
		sqlmock.NewRows([]string{"count"}).AddRow(slimMaxIOWaitBackends + 5),
	)
	mock.ExpectExec(`(?s)DELETE FROM mailing_campaign_queue`).
		WillReturnResult(sqlmock.NewResult(0, 5))
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

// TestCleanupTerminalQueueItems_GateOpensOnLaterSample: one saturated sample
// is a spike, not a storm — a later sample below the threshold opens the gate.
func TestCleanupTerminalQueueItems_GateOpensOnLaterSample(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`pg_stat_activity`).WillReturnRows(
		sqlmock.NewRows([]string{"count"}).AddRow(terminalPurgeMaxIOWaitBackends + 10),
	)
	mock.ExpectQuery(`pg_stat_activity`).WillReturnRows(
		sqlmock.NewRows([]string{"count"}).AddRow(terminalPurgeMaxIOWaitBackends - 1),
	)
	mock.ExpectExec(`(?s)DELETE FROM mailing_campaign_queue`).
		WillReturnResult(sqlmock.NewResult(0, 5))
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

// TestCleanupTerminalQueueItems_RetriesFailedBatch: a single failed DELETE
// batch (e.g. one 60s statement timeout) no longer forfeits the cycle — the
// batch is retried and the purge completes.
func TestCleanupTerminalQueueItems_RetriesFailedBatch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`pg_stat_activity`).WillReturnRows(
		sqlmock.NewRows([]string{"count"}).AddRow(0),
	)
	mock.ExpectExec(`(?s)DELETE FROM mailing_campaign_queue`).
		WillReturnError(context.DeadlineExceeded)
	mock.ExpectExec(`(?s)DELETE FROM mailing_campaign_queue`).
		WillReturnResult(sqlmock.NewResult(0, 5))
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

// TestCleanupTerminalQueueItems_GivesUpAfterConsecutiveBatchErrors: the retry
// budget is bounded — after terminalPurgeMaxBatchRetries consecutive failures
// the cycle stops (stats still logged) instead of looping forever.
func TestCleanupTerminalQueueItems_GivesUpAfterConsecutiveBatchErrors(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`pg_stat_activity`).WillReturnRows(
		sqlmock.NewRows([]string{"count"}).AddRow(0),
	)
	for i := 0; i < terminalPurgeMaxBatchRetries; i++ {
		mock.ExpectExec(`(?s)DELETE FROM mailing_campaign_queue`).
			WillReturnError(context.DeadlineExceeded)
	}
	// Give-up path logs stats once.
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
