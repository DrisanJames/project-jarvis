package worker

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWithDBTimeout_SetsLocalTimeoutAndCommits verifies the helper wraps fn in
// a transaction, raises statement_timeout via SET LOCAL to the orchestrator's
// 120s ceiling, and commits on success — so no pooled connection leaks the
// elevated timeout back to the app's default 30s pool.
func TestWithDBTimeout_SetsLocalTimeoutAndCommits(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout = '120000ms'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE partner_clean_queue`).
		WillReturnResult(sqlmock.NewResult(0, 7))
	mock.ExpectCommit()

	po := &PartnerDripOrchestrator{db: db}
	var affected int64
	err = po.withDBTimeout(context.Background(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(context.Background(), `UPDATE partner_clean_queue SET status='ready'`)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	require.NoError(t, err)
	assert.Equal(t, int64(7), affected)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestWithDBTimeout_RollsBackOnError verifies that an error returned by fn rolls
// the transaction back (so the elevated statement_timeout is discarded with it)
// and propagates the original error.
func TestWithDBTimeout_RollsBackOnError(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	sentinel := errors.New("boom")
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	po := &PartnerDripOrchestrator{db: db}
	gotErr := po.withDBTimeout(context.Background(), func(tx *sql.Tx) error {
		return sentinel
	})
	require.ErrorIs(t, gotErr, sentinel)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestActiveVerticalsWithBacklog_TwoPhase verifies the rewritten gateway runs
// the cheap aggregate (phase 1) then a per-vertical dominant-dataset lookup
// (phase 2), all inside the extended-timeout transaction, and assembles a
// populated verticalState. This is the query that was tripping the 30s pool
// timeout every tick and silently zeroing the drip.
func TestActiveVerticalsWithBacklog_TwoPhase(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Phase 1: cheap aggregate over the ready partial index.
	mock.ExpectQuery(`SELECT s.vertical, s.next_brand_index, agg.ready_total, agg.oldest_at`).
		WillReturnRows(sqlmock.NewRows([]string{"vertical", "next_brand_index", "ready_total", "oldest_at"}).
			AddRow("remodel", 3, 42879, nil))
	// Phase 2: dominant-dataset metadata for the one returned vertical.
	mock.ExpectQuery(`FROM \(\s*SELECT dataset_id`).
		WithArgs("remodel").
		WillReturnRows(sqlmock.NewRows([]string{"id", "slug", "pslug", "pname", "flush", "offer"}).
			AddRow("ds-1", "trugreen-remodel", "trugreen", "TruGreen", 24, ""))
	mock.ExpectCommit()

	po := &PartnerDripOrchestrator{db: db}
	out, err := po.activeVerticalsWithBacklog(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "remodel", out[0].vertical)
	assert.Equal(t, 3, out[0].brandIndex)
	assert.Equal(t, 42879, out[0].readyCount)
	assert.Equal(t, "ds-1", out[0].datasetID)
	assert.Equal(t, "trugreen", out[0].partnerSlug)
	assert.Equal(t, 24, out[0].flushHours)
	assert.NoError(t, mock.ExpectationsWereMet())
}
