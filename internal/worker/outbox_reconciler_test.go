package worker

// Tests for OutboxReconciler.
//
// The reconciler runs two UPDATEs per pass:
//   (1) submitting -> accepted for rows where message_id IS NOT NULL
//       (crash-window recovery)
//   (2) submitting -> failed_retryable for rows where message_id IS NULL
//       (stranded row requeue)
//
// Both UPDATEs carry explicit WHERE-status guards so the reconciler can
// never double-transition or race with a live worker. Tests assert the
// expected SQL is executed in order and the handler gracefully handles
// transient query errors without crashing the loop.

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestOutboxReconciler_ReconcileOnce_CommitsAndRequeues(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	// Commit-window UPDATE: message_id IS NOT NULL -> 'accepted'.
	mock.ExpectExec(regexp.QuoteMeta("SET status = 'accepted'")).
		WithArgs("10m0s").
		WillReturnResult(sqlmock.NewResult(0, 3))

	// Stranded UPDATE: message_id IS NULL -> 'failed_retryable'.
	mock.ExpectExec(regexp.QuoteMeta("SET status = 'failed_retryable'")).
		WithArgs("10m0s").
		WillReturnResult(sqlmock.NewResult(0, 2))

	r := NewOutboxReconciler(db)
	r.reconcileOnce(context.Background())

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOutboxReconciler_ReconcileOnce_CommitError_StillRunsRequeue(t *testing.T) {
	// The reconciler must be resilient: a transient error on the commit-window
	// UPDATE must not stop the requeue UPDATE from firing. Otherwise a single
	// transient failure could block the requeue path and leave rows stranded.
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("SET status = 'accepted'")).
		WithArgs("10m0s").
		WillReturnError(assertSQLBlip())

	mock.ExpectExec(regexp.QuoteMeta("SET status = 'failed_retryable'")).
		WithArgs("10m0s").
		WillReturnResult(sqlmock.NewResult(0, 1))

	r := NewOutboxReconciler(db)
	r.reconcileOnce(context.Background())

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOutboxReconciler_ReconcileOnce_RequeueError_DoesNotPanic(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("SET status = 'accepted'")).
		WithArgs("10m0s").
		WillReturnResult(sqlmock.NewResult(0, 0))

	mock.ExpectExec(regexp.QuoteMeta("SET status = 'failed_retryable'")).
		WithArgs("10m0s").
		WillReturnError(assertSQLBlip())

	r := NewOutboxReconciler(db)
	r.reconcileOnce(context.Background())

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOutboxReconciler_NoRows_NoActionTaken(t *testing.T) {
	// With zero stuck rows both UPDATEs still execute; the important
	// invariant is that RowsAffected=0 is handled silently without log
	// noise that would wake an operator at 3am for a non-issue.
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("SET status = 'accepted'")).
		WithArgs("10m0s").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("SET status = 'failed_retryable'")).
		WithArgs("10m0s").
		WillReturnResult(sqlmock.NewResult(0, 0))

	r := NewOutboxReconciler(db)
	r.reconcileOnce(context.Background())

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNewOutboxReconcilerWithConfig_DefaultsForNonPositive(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	r := NewOutboxReconcilerWithConfig(db, 0, 0)
	require.Equal(t, DefaultReconcilerInterval, r.interval)
	require.Equal(t, DefaultReconcilerGrace, r.grace)

	r2 := NewOutboxReconcilerWithConfig(db, -1*time.Second, -1*time.Second)
	require.Equal(t, DefaultReconcilerInterval, r2.interval)
	require.Equal(t, DefaultReconcilerGrace, r2.grace)

	r3 := NewOutboxReconcilerWithConfig(db, 30*time.Second, 5*time.Minute)
	require.Equal(t, 30*time.Second, r3.interval)
	require.Equal(t, 5*time.Minute, r3.grace)
}

// assertSQLBlip is a tiny helper type so tests can assert behavior in the
// presence of a transient SQL error without importing half of database/sql.
type reconcilerTestErr struct{}

func (reconcilerTestErr) Error() string { return "simulated transient sql error" }

func assertSQLBlip() error { return reconcilerTestErr{} }
