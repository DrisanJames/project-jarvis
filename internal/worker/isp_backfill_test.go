package worker

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestISPBackfill_RunOnce_IteratesUntilZero verifies that RunOnce keeps
// running batches until one returns 0 affected rows, then stops.
func TestISPBackfill_RunOnce_IteratesUntilZero(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	// Three batches: 50000, 37123, 0 (stop after zero). Each batch
	// acquires a connection and first issues SET statement_timeout.
	for _, affected := range []int64{50000, 37123, 0} {
		mock.ExpectExec(`SET statement_timeout`).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(`UPDATE mailing_subscribers s\s+SET isp =`).
			WillReturnResult(sqlmock.NewResult(0, affected))
	}

	w := NewISPBackfillWorker(db)
	err = w.RunOnce(context.Background())
	assert.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestISPBackfill_RunOnce_NoUnclassifiedRows verifies the steady-state
// case — after initial backfill, each subsequent pass affects 0 rows and
// returns immediately after a single no-op UPDATE.
func TestISPBackfill_RunOnce_NoUnclassifiedRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectExec(`SET statement_timeout`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE mailing_subscribers s\s+SET isp =`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := NewISPBackfillWorker(db)
	err = w.RunOnce(context.Background())
	assert.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestISPBackfill_BatchQueryShape verifies that the SQL emitted uses the
// canonical isp.SQLCaseFromEmail expression (checks for the Gmail group's
// signature domains as a proxy — if the CASE is present, all groups are).
// The test pins the query shape so refactors that accidentally bypass the
// single-source-of-truth helper fail loudly.
func TestISPBackfill_BatchQueryShape(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	// Use a regex that asserts multiple invariants:
	//   - CTE scoped to empty/NULL isp with FOR UPDATE SKIP LOCKED
	//   - CASE references the gmail.com / googlemail.com pair
	//   - UPDATE guards against concurrent fills
	pattern := regexp.MustCompile(`(?s)WITH batch AS \(.*isp IS NULL OR isp = ''.*FOR UPDATE SKIP LOCKED.*\)` +
		`.*UPDATE mailing_subscribers s\s+SET isp =.*` +
		`'gmail\.com','googlemail\.com'.*` +
		`WHERE s\.id = batch\.id\s+AND \(s\.isp IS NULL OR s\.isp = ''\)`)

	mock.ExpectExec(`SET statement_timeout`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(pattern.String()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := NewISPBackfillWorker(db)
	_, err = w.runBatch(context.Background())
	assert.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestISPBackfill_RunOnce_StopsOnContextCancel verifies graceful shutdown
// mid-pass. If ctx is cancelled between batches, RunOnce returns the
// context error without running another batch.
func TestISPBackfill_RunOnce_StopsOnContextCancel(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())

	// One batch succeeds (returns > 0 rows so the loop would continue),
	// but context is cancelled before the next iteration.
	mock.ExpectExec(`SET statement_timeout`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE mailing_subscribers s\s+SET isp =`).
		WillReturnResult(sqlmock.NewResult(0, 50000)).
		WillDelayFor(0)

	w := NewISPBackfillWorker(db)

	// Cancel immediately after first batch by wrapping in a goroutine —
	// but simplest: run one batch manually, then cancel, then call RunOnce
	// which should see ctx.Err() and return before issuing a new batch.
	_, err = w.runBatch(ctx)
	require.NoError(t, err)
	cancel()

	err = w.RunOnce(ctx)
	assert.ErrorIs(t, err, context.Canceled)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestISPBackfill_RunOnce_PropagatesBatchError verifies errors from the
// UPDATE propagate out with context. Operational visibility matters when
// a backfill fails — we want the batch number in the log.
func TestISPBackfill_RunOnce_PropagatesBatchError(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectExec(`SET statement_timeout`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE mailing_subscribers s\s+SET isp =`).
		WillReturnError(assertableError{})

	w := NewISPBackfillWorker(db)
	err = w.RunOnce(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "isp backfill batch")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// assertableError is a small error type that renders to a known string
// so we can pattern-match in TestISPBackfill_RunOnce_PropagatesBatchError
// without pulling in a mock generator.
type assertableError struct{}

func (assertableError) Error() string { return "simulated db failure" }
