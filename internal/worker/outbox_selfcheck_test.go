package worker

// Tests for OutboxSelfCheck.
//
// The self-check issues four aggregate queries per runOnce and fires SMS
// alerts when thresholds are crossed. Tests drive runOnce directly (not
// Start) so the ticker is out of scope.

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
	mock.MatchExpectationsInOrder(false)
	return db, mock
}

// TestSelfCheck_AllInvariantsHealthy — no SMS should fire when every check
// comes back clean. This verifies the baseline (no false positives).
func TestSelfCheck_AllInvariantsHealthy(t *testing.T) {
	db, mock := newSelfCheckMockDB(t)

	mock.ExpectQuery(`status = 'submitting'`).
		WillReturnRows(sqlmock.NewRows([]string{"count", "oldest"}).AddRow(int64(0), int64(0)))
	mock.ExpectQuery(`dead_letter`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(5)))
	mock.ExpectQuery(`status = 'queued'\s*$|status = 'queued'\)`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(1000)))
	mock.ExpectQuery(`scheduled_at IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"age"}).AddRow(int64(30)))

	alerter := &capturingAlerter{}
	sc := NewOutboxSelfCheck(db)
	sc.SetAlerter(alerter, []string{"+15555550100"})

	sc.runOnce(context.Background())
	require.Empty(t, alerter.drain(), "healthy system must not alert")
}

// TestSelfCheck_SubmittingStuckFiresSMS — rows held in 'submitting' past the
// grace window must fire the paging path.
func TestSelfCheck_SubmittingStuckFiresSMS(t *testing.T) {
	db, mock := newSelfCheckMockDB(t)

	mock.ExpectQuery(`status = 'submitting'`).
		WillReturnRows(sqlmock.NewRows([]string{"count", "oldest"}).AddRow(int64(42), int64(900)))
	mock.ExpectQuery(`dead_letter`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	mock.ExpectQuery(`status = 'queued'\s*$|status = 'queued'\)`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	mock.ExpectQuery(`scheduled_at IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"age"}).AddRow(int64(0)))

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

	mock.ExpectQuery(`status = 'submitting'`).
		WillReturnRows(sqlmock.NewRows([]string{"count", "oldest"}).AddRow(int64(0), int64(0)))
	mock.ExpectQuery(`dead_letter`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(1500)))
	mock.ExpectQuery(`status = 'queued'\s*$|status = 'queued'\)`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	mock.ExpectQuery(`scheduled_at IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"age"}).AddRow(int64(0)))

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

	// Two consecutive runOnce — the same conditions on every query. Every
	// query must be expected per run.
	for i := 0; i < 2; i++ {
		mock.ExpectQuery(`status = 'submitting'`).
			WillReturnRows(sqlmock.NewRows([]string{"count", "oldest"}).AddRow(int64(10), int64(900)))
		mock.ExpectQuery(`dead_letter`).
			WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
		mock.ExpectQuery(`status = 'queued'\s*$|status = 'queued'\)`).
			WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
		mock.ExpectQuery(`scheduled_at IS NOT NULL`).
			WillReturnRows(sqlmock.NewRows([]string{"age"}).AddRow(int64(0)))
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
// panic).
func TestSelfCheck_AlertingDisabled(t *testing.T) {
	db, mock := newSelfCheckMockDB(t)

	mock.ExpectQuery(`status = 'submitting'`).
		WillReturnRows(sqlmock.NewRows([]string{"count", "oldest"}).AddRow(int64(10), int64(900)))
	mock.ExpectQuery(`dead_letter`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	mock.ExpectQuery(`status = 'queued'\s*$|status = 'queued'\)`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	mock.ExpectQuery(`scheduled_at IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"age"}).AddRow(int64(0)))

	sc := NewOutboxSelfCheck(db)
	// No SetAlerter call — alerting remains disabled.
	sc.runOnce(context.Background())
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSelfCheck_ContinuesAfterQueryError — a single failed query must not
// short-circuit the other invariants.
func TestSelfCheck_ContinuesAfterQueryError(t *testing.T) {
	db, mock := newSelfCheckMockDB(t)

	mock.ExpectQuery(`status = 'submitting'`).WillReturnError(sql.ErrConnDone)
	mock.ExpectQuery(`dead_letter`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(1500)))
	mock.ExpectQuery(`status = 'queued'\s*$|status = 'queued'\)`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	mock.ExpectQuery(`scheduled_at IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"age"}).AddRow(int64(0)))

	alerter := &capturingAlerter{}
	sc := NewOutboxSelfCheck(db)
	sc.SetAlerter(alerter, []string{"+15555550100"})
	sc.runOnce(context.Background())

	got := alerter.drain()
	require.Len(t, got, 1, "dead-letter alert must fire even though submitting query failed")
	require.Contains(t, got[0].Body, "1500 permanent failures")
}
