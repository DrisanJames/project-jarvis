package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExpressFollowupBrands_OrdersByDueDesc pins the express follow-up path's
// brand selection (operator 2026-08-16 "this must drain"): brands that actually
// hold due records, heaviest first — replacing the blind 16-brand rotation that
// gave a db-only express lane ~1 productive wave/hour against a 32k backlog.
func TestExpressFollowupBrands_OrdersByDueDesc(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	po := &PartnerDripOrchestrator{db: db}

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).
		WithArgs("internal_auto_insurance", MaxTouchCount-1, 8).
		WillReturnRows(sqlmock.NewRows([]string{"brand", "due"}).
			AddRow("db", 32711).
			AddRow("QF ", 12). // mixed case + whitespace must normalize
			AddRow("", 3))     // blank brand must be dropped, never dispatched
	mock.ExpectCommit()

	brands, err := po.expressFollowupBrands(context.Background(), "internal_auto_insurance", 8)
	require.NoError(t, err)
	assert.Equal(t, []string{"db", "qf"}, brands,
		"loaded brands, heaviest first, normalized, blanks dropped")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestExpressFollowupBrands_ErrorPropagates: a lookup failure must surface an
// error (the caller falls back to the rotation — an express vertical is never
// worse off than a cold one, and never silently skipped).
func TestExpressFollowupBrands_ErrorPropagates(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	po := &PartnerDripOrchestrator{db: db}

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	_, err = po.expressFollowupBrands(context.Background(), "internal_auto_insurance", 8)
	assert.Error(t, err, "lookup failure must propagate so tickFollowups falls back to rotation")
}

// TestDatasetIsExpress_FailClosed pins the express gate's failure mode: a
// lookup error must return FALSE (keep the drain horizon + rotation), never
// grant express treatment on bad data.
func TestDatasetIsExpress_FailClosed(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	po := &PartnerDripOrchestrator{db: db}

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_datasets`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	assert.False(t, po.datasetIsExpress(context.Background(), "99137b10-969c-4c9b-84a6-28042b779a07"),
		"express lookup failure must fail CLOSED (no express treatment)")
}
