package worker

// Tests for OutboxSelfCheck.
//
// Each runOnce now runs five DB operations, every one wrapped by
// runWithTimeout in its own read-committed transaction with a raised
// statement_timeout (Begin -> SET LOCAL -> work -> Commit/Rollback). The
// expectation helpers below register that exact sequence so the tests assert
// both the invariant logic AND that the storm-resilient timeout wrapper is in
// place. Tests drive runOnce directly (not Start) so the ticker is out of scope.
//
// runOnce order is fixed: janitor, submitting, dead-letter, backlog, oldest.

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

type capturingAlerter struct {
	mu   sync.Mutex
	sent []capturedSMS
}

type capturedSMS struct {
	To   string
	Body string
}

func (a *capturingAlerter) SendSMS(_ context.Context, to, body string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent = append(a.sent, capturedSMS{To: to, Body: body})
	return "sid_test", nil
}

func (a *capturingAlerter) drain() []capturedSMS {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]capturedSMS(nil), a.sent...)
	a.sent = nil
	return out
}

func newSelfCheckMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
		sqlmock.MonitorPingsOption(false),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

// expectJanitorTx registers the Begin/SET LOCAL/UPDATE/Commit sequence for the
// terminal-campaign/wave janitor that runs first on every tick.
func expectJanitorTx(mock sqlmock.Sqlmock, cancelled int64) {
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout = '120000ms'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`WITH victims`).
		WillReturnResult(sqlmock.NewResult(0, cancelled))
	mock.ExpectCommit()
}

// expectCheckTx registers the Begin/SET LOCAL/query/Commit sequence for one of
// the four aggregate invariant checks.
func expectCheckTx(mock sqlmock.Sqlmock, queryRe string, rows *sqlmock.Rows) {
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(queryRe).WillReturnRows(rows)
	mock.ExpectCommit()
}

// expectCheckTxErr registers a check whose query errors — runWithTimeout must
// roll the transaction back rather than commit it.
func expectCheckTxErr(mock sqlmock.Sqlmock, queryRe string) {
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(queryRe).WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()
}

const (
	reSubmitting = `status = 'submitting'`
	reDeadLetter = `dead_letter`
	reBacklog    = `COUNT\(\*\)::bigint FROM mailing_campaign_queue q`
	reOldest     = `MIN\(scheduled_at\)`
)

// expectHealthyTick registers a full healthy runOnce: janitor + four checks,
// each returning a benign value. Callers override individual checks by passing
// their own rows; this helper is for the common all-clear scaffold.
func expectHealthyTick(mock sqlmock.Sqlmock) {
	expectJanitorTx(mock, 0)
	expectCheckTx(mock, reSubmitting, sqlmock.NewRows([]string{"count", "oldest"}).AddRow(int64(0), int64(0)))
	expectCheckTx(mock, reDeadLetter, sqlmock.NewRows([]string{"c"}).AddRow(int64(5)))
	expectCheckTx(mock, reBacklog, sqlmock.NewRows([]string{"c"}).AddRow(int64(1000)))
	expectCheckTx(mock, reOldest, sqlmock.NewRows([]string{"age"}).AddRow(int64(30)))
}

// TestSelfCheck_AllInvariantsHealthy — no SMS should fire when every check
// comes back clean. This verifies the baseline (no false positives).
func TestSelfCheck_AllInvariantsHealthy(t *testing.T) {
	db, mock := newSelfCheckMockDB(t)
	expectHealthyTick(mock)

	alerter := &capturingAlerter{}
	sc := NewOutboxSelfCheck(db)
	sc.SetAlerter(alerter, []string{"+15555550100"})

	sc.runOnce(context.Background())
	require.Empty(t, alerter.drain(), "healthy system must not alert")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSelfCheck_SubmittingStuckFiresSMS — rows held in 'submitting' past the
// grace window must fire the paging path.
func TestSelfCheck_SubmittingStuckFiresSMS(t *testing.T) {
	db, mock := newSelfCheckMockDB(t)
	expectJanitorTx(mock, 0)
	expectCheckTx(mock, reSubmitting, sqlmock.NewRows([]string{"count", "oldest"}).AddRow(int64(42), int64(900)))
	expectCheckTx(mock, reDeadLetter, sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	expectCheckTx(mock, reBacklog, sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	expectCheckTx(mock, reOldest, sqlmock.NewRows([]string{"age"}).AddRow(int64(0)))

	alerter := &capturingAlerter{}
	sc := NewOutboxSelfCheck(db)
	sc.SetAlerter(alerter, []string{"+15555550100", "+15555550101"})

	sc.runOnce(context.Background())
	got := alerter.drain()
	require.Len(t, got, 2, "each recipient must get one SMS")
	require.Contains(t, got[0].Body, "stuck in submitting")
	require.Contains(t, got[0].Body, "42 row")
}

// TestSelfCheck_DeadLetterSpikeFiresSMS — permanent-failure rate over the
// threshold fires the paging path.
func TestSelfCheck_DeadLetterSpikeFiresSMS(t *testing.T) {
	db, mock := newSelfCheckMockDB(t)
	expectJanitorTx(mock, 0)
	expectCheckTx(mock, reSubmitting, sqlmock.NewRows([]string{"count", "oldest"}).AddRow(int64(0), int64(0)))
	expectCheckTx(mock, reDeadLetter, sqlmock.NewRows([]string{"c"}).AddRow(int64(1500)))
	expectCheckTx(mock, reBacklog, sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	expectCheckTx(mock, reOldest, sqlmock.NewRows([]string{"age"}).AddRow(int64(0)))

	alerter := &capturingAlerter{}
	sc := NewOutboxSelfCheck(db)
	sc.SetAlerter(alerter, []string{"+15555550100"})

	sc.runOnce(context.Background())
	got := alerter.drain()
	require.Len(t, got, 1)
	require.Contains(t, got[0].Body, "1500 permanent failures")
}

// TestSelfCheck_ReAlertSuppression — the same invariant should not fire twice
// within the re-alert window. This is the noise-control contract.
func TestSelfCheck_ReAlertSuppression(t *testing.T) {
	db, mock := newSelfCheckMockDB(t)

	// Two consecutive runOnce — the same conditions on every query.
	for i := 0; i < 2; i++ {
		expectJanitorTx(mock, 0)
		expectCheckTx(mock, reSubmitting, sqlmock.NewRows([]string{"count", "oldest"}).AddRow(int64(10), int64(900)))
		expectCheckTx(mock, reDeadLetter, sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
		expectCheckTx(mock, reBacklog, sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
		expectCheckTx(mock, reOldest, sqlmock.NewRows([]string{"age"}).AddRow(int64(0)))
	}

	alerter := &capturingAlerter{}
	sc := NewOutboxSelfCheckWithConfig(db, 5*time.Minute, 30*time.Minute)
	sc.SetAlerter(alerter, []string{"+15555550100"})

	sc.runOnce(context.Background())
	require.Len(t, alerter.drain(), 1, "first breach fires")

	sc.runOnce(context.Background())
	require.Empty(t, alerter.drain(), "second breach within re-alert window must not fire")
}

// TestSelfCheck_AlertingDisabled — SMS disabled means no send attempts even on
// invariant breach. The log line still fires (verified indirectly via no
// panic) and all DB expectations are still consumed.
func TestSelfCheck_AlertingDisabled(t *testing.T) {
	db, mock := newSelfCheckMockDB(t)
	expectJanitorTx(mock, 0)
	expectCheckTx(mock, reSubmitting, sqlmock.NewRows([]string{"count", "oldest"}).AddRow(int64(10), int64(900)))
	expectCheckTx(mock, reDeadLetter, sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	expectCheckTx(mock, reBacklog, sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	expectCheckTx(mock, reOldest, sqlmock.NewRows([]string{"age"}).AddRow(int64(0)))

	sc := NewOutboxSelfCheck(db)
	// No SetAlerter call — alerting remains disabled.
	sc.runOnce(context.Background())
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSelfCheck_ContinuesAfterQueryError — a single failed query must not
// short-circuit the other invariants, and its transaction must roll back.
func TestSelfCheck_ContinuesAfterQueryError(t *testing.T) {
	db, mock := newSelfCheckMockDB(t)
	expectJanitorTx(mock, 0)
	expectCheckTxErr(mock, reSubmitting)
	expectCheckTx(mock, reDeadLetter, sqlmock.NewRows([]string{"c"}).AddRow(int64(1500)))
	expectCheckTx(mock, reBacklog, sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	expectCheckTx(mock, reOldest, sqlmock.NewRows([]string{"age"}).AddRow(int64(0)))

	alerter := &capturingAlerter{}
	sc := NewOutboxSelfCheck(db)
	sc.SetAlerter(alerter, []string{"+15555550100"})
	sc.runOnce(context.Background())

	got := alerter.drain()
	require.Len(t, got, 1, "dead-letter alert must fire even though submitting query failed")
	require.Contains(t, got[0].Body, "1500 permanent failures")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSelfCheck_QueuedInvariantsExcludeTerminalParents guards the 2026-06-07
// regression: a 'queued' row left behind by a long-completed campaign — or one
// stranded on a wave whose window closed — tripped the oldest-queued (and
// nearly the backlog) invariant forever because the queries never excluded
// terminal-parent/terminal-wave rows. Both queued aggregates must now carry the
// live-row EXISTS/NOT-EXISTS filter, the janitor must sweep both zombie classes
// each tick, and every query must run under the raised-timeout transaction.
// sqlmock's regexp matcher fails the test if any of those is dropped.
func TestSelfCheck_QueuedInvariantsExcludeTerminalParents(t *testing.T) {
	db, mock := newSelfCheckMockDB(t)

	// Janitor sweep covers terminal campaign OR terminal wave; here it reports
	// 7 zombies cancelled.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout = '120000ms'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`WITH victims[\s\S]*c\.status IN \('completed','cancelled','failed','sent'\)[\s\S]*mailing_campaign_waves[\s\S]*w\.status IN \('completed','cancelled','sent'\)`).
		WillReturnResult(sqlmock.NewResult(0, 7))
	mock.ExpectCommit()

	expectCheckTx(mock, reSubmitting, sqlmock.NewRows([]string{"count", "oldest"}).AddRow(int64(0), int64(0)))
	expectCheckTx(mock, reDeadLetter, sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	// Backlog query MUST restrict to live campaign AND live wave.
	expectCheckTx(mock, `COUNT\(\*\)::bigint FROM mailing_campaign_queue q[\s\S]*EXISTS[\s\S]*mailing_campaigns[\s\S]*NOT EXISTS[\s\S]*mailing_campaign_waves`,
		sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	// Oldest-queued query MUST restrict to live campaign AND live wave.
	expectCheckTx(mock, `MIN\(scheduled_at\)[\s\S]*EXISTS[\s\S]*mailing_campaigns[\s\S]*NOT EXISTS[\s\S]*mailing_campaign_waves`,
		sqlmock.NewRows([]string{"age"}).AddRow(int64(0)))

	alerter := &capturingAlerter{}
	sc := NewOutboxSelfCheck(db)
	sc.SetAlerter(alerter, []string{"+15555550100"})

	sc.runOnce(context.Background())
	require.NoError(t, mock.ExpectationsWereMet())
	require.Empty(t, alerter.drain(), "zombie-only backlog must not page once filtered")
}
