package worker

// Tests for OutboxSelfCheck.
//
// Every DB operation in a tick is wrapped by runWithTimeout in its own
// read-committed transaction with a raised statement_timeout (Begin -> SET LOCAL
// -> work -> Commit/Rollback). The expectation helpers below register that exact
// sequence so the tests assert both the invariant logic AND that the
// storm-resilient timeout wrapper is in place. Tests drive runOnce directly (not
// Start) so the ticker is out of scope.
//
// runOnce order is fixed and the sqlmock expectations depend on it:
//
//	terminal-parent janitor  (3 statements: parents, produced/landed, cancel)
//	zombie-scheduled janitor (1 statement)
//	submitting / dead-letter / backlog / oldest   (the four queue aggregates)
//	wave_unlanded / campaign_no_send / failed_burst / scheduled_dead  (REQ-087)
//	send-throughput sample   (publishes /health send_liveness)

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

// expectJanitorTx registers the full write half of a tick: the three-statement
// terminal-parent janitor (parents -> produced/landed -> bounded UPDATE) and the
// zombie 'scheduled' campaign janitor that follows it. The produced/landed row
// is returned SAFE (produced <= landed) so the default scaffold still cancels;
// TestSelfCheck_JanitorSkipsInFlight drives the unsafe case explicitly.
func expectJanitorTx(mock sqlmock.Sqlmock, cancelled int64) {
	expectJanitorParents(mock, []string{janitorTestCampaign})
	expectJanitorProducedLanded(mock, janitorTestCampaign, 100, 100)
	expectJanitorCancel(mock, cancelled)
	expectZombieTx(mock, 0)
}

const janitorTestCampaign = "11111111-1111-1111-1111-111111111111"

// expectJanitorParents — step A: which campaigns hold 'queued' rows.
func expectJanitorParents(mock sqlmock.Sqlmock, ids []string) {
	rows := sqlmock.NewRows([]string{"campaign_id", "n"})
	for _, id := range ids {
		rows.AddRow(id, int64(10))
	}
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout = '120000ms'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`GROUP BY q\.campaign_id`).WillReturnRows(rows)
	mock.ExpectCommit()
}

// expectJanitorProducedLanded — step B: the produced-vs-landed safety rule.
func expectJanitorProducedLanded(mock sqlmock.Sqlmock, id string, produced, landed int64) {
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout = '120000ms'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`AS produced`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "produced", "landed"}).AddRow(id, produced, landed))
	mock.ExpectCommit()
}

// expectJanitorCancel — step C: the bounded UPDATE.
func expectJanitorCancel(mock sqlmock.Sqlmock, cancelled int64) {
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout = '120000ms'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`WITH victims[\s\S]*UPDATE mailing_campaign_queue`).
		WillReturnResult(sqlmock.NewResult(0, cancelled))
	mock.ExpectCommit()
}

// expectZombieTx — the 'scheduled' >7d with no live wave campaign janitor.
func expectZombieTx(mock sqlmock.Sqlmock, cancelled int64) {
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout = '120000ms'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`WITH victims[\s\S]*UPDATE mailing_campaigns`).
		WillReturnResult(sqlmock.NewResult(0, cancelled))
	mock.ExpectCommit()
}

// expectTailChecks registers the four send-liveness invariants plus the
// throughput sample that close every tick, all returning benign values.
func expectTailChecks(mock sqlmock.Sqlmock) {
	expectCheckTx(mock, reWaveUnlanded, sqlmock.NewRows([]string{"waves", "recips"}).AddRow(int64(0), int64(0)))
	expectCheckTx(mock, reCampaignNoSend, sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	expectCheckTx(mock, reFailedBurst, sqlmock.NewRows([]string{"cur", "med"}).AddRow(int64(0), float64(0)))
	expectCheckTx(mock, reScheduledDead, sqlmock.NewRows([]string{"z", "n"}).AddRow(int64(0), int64(0)))
	expectCheckTx(mock, reThroughput, sqlmock.NewRows([]string{"n", "last"}).AddRow(int64(1000), time.Now()))
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

	// The four send-liveness invariants (REQ-087) + the throughput sample.
	reWaveUnlanded   = `WITH cand`
	reCampaignNoSend = `COALESCE\(c\.queued_count, 0\) > 0`
	reFailedBurst    = `WITH hourly`
	reScheduledDead  = `FILTER \(WHERE COALESCE\(c\.total_recipients`
	reThroughput     = `FROM mailing_message_log`
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
	expectTailChecks(mock)
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
	expectTailChecks(mock)

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
	expectTailChecks(mock)

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
		expectTailChecks(mock)
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
	expectTailChecks(mock)

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
	expectTailChecks(mock)

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
// regression: a 'queued' row left behind by a long-completed campaign tripped
// the oldest-queued (and nearly the backlog) invariant forever because the
// queries never excluded terminal-parent rows. Both queued aggregates must carry
// the live-CAMPAIGN EXISTS filter, and every query must run under the
// raised-timeout transaction.
//
// It ALSO guards the opposite regression, added 2026-09-01 (REQ-087 DoD 2): the
// wave half of that filter must be GONE. `NOT EXISTS (wave IN completed/...)`
// excluded every row of every board and sidecar campaign — the dispatcher marks
// a wave 'completed' at enqueue — which is why both monitors stayed silent
// through the 90-minute SK-4 transport wedge. The negative assertions below fail
// the test if the wave clause is ever restored to either aggregate.
func TestSelfCheck_QueuedInvariantsExcludeTerminalParents(t *testing.T) {
	db, mock := newSelfCheckMockDB(t)

	// Janitor sweep covers ONLY terminal CAMPAIGN (the wave-terminal branch was
	// removed 2026-06-18 — it cancelled live rows under enqueued-but-still-
	// draining waves), and only after the produced/landed rule clears the
	// parent. Here it reports 7 zombies cancelled.
	expectJanitorParents(mock, []string{janitorTestCampaign})
	expectJanitorProducedLanded(mock, janitorTestCampaign, 100, 100)
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout = '120000ms'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`WITH victims[\s\S]*q\.status = 'queued'[\s\S]*UPDATE mailing_campaign_queue`).
		WillReturnResult(sqlmock.NewResult(0, 7))
	mock.ExpectCommit()
	expectZombieTx(mock, 0)

	expectCheckTx(mock, reSubmitting, sqlmock.NewRows([]string{"count", "oldest"}).AddRow(int64(0), int64(0)))
	expectCheckTx(mock, reDeadLetter, sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	// Backlog query MUST restrict to a live campaign.
	expectCheckTx(mock, `COUNT\(\*\)::bigint FROM mailing_campaign_queue q[\s\S]*EXISTS[\s\S]*mailing_campaigns`,
		sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	// Oldest-queued query MUST restrict to a live campaign.
	expectCheckTx(mock, `MIN\(scheduled_at\)[\s\S]*EXISTS[\s\S]*mailing_campaigns`,
		sqlmock.NewRows([]string{"age"}).AddRow(int64(0)))
	expectTailChecks(mock)

	alerter := &capturingAlerter{}
	sc := NewOutboxSelfCheck(db)
	sc.SetAlerter(alerter, []string{"+15555550100"})

	sc.runOnce(context.Background())
	require.NoError(t, mock.ExpectationsWereMet())
	require.Empty(t, alerter.drain(), "zombie-only backlog must not page once filtered")

	// REQ-087 DoD 2 — the wave exclusion must not come back.
	require.NotContains(t, liveQueuedClause, "mailing_campaign_waves",
		"liveQueuedClause must not exclude rows under terminal waves — that blinded both monitors to the entire wave path")
	require.Contains(t, liveQueuedClause, "mailing_campaigns",
		"liveQueuedClause must still exclude rows under terminal campaigns")
}

// =============================================================================
// REQ-082 — the janitor must never cancel a recipient that is still in flight
// =============================================================================

// TestSelfCheck_JanitorSkipsInFlight is the direct regression guard for the
// 2026-09-01 loss: 80,514 rows cancelled under 'sent' campaigns as they landed
// out of Kafka. produced (SUM of wave enqueued_recipients) greater than landed
// (queue rows present) means recipients exist that have not reached the table
// yet, so nothing under that campaign may be touched no matter what its status
// says. Both branches are asserted — a guard that never lets anything through is
// as broken as one that lets everything through.
func TestSelfCheck_JanitorSkipsInFlight(t *testing.T) {
	t.Run("produced_gt_landed_cancels_nothing", func(t *testing.T) {
		db, mock := newSelfCheckMockDB(t)

		expectJanitorParents(mock, []string{janitorTestCampaign})
		// 41,437 produced vs 34,851 landed — the RRU-GLFE shape from the incident.
		expectJanitorProducedLanded(mock, janitorTestCampaign, 41437, 34851)
		// NO cancel transaction is registered: if the janitor issues one anyway,
		// sqlmock fails on the unexpected call.
		expectZombieTx(mock, 0)

		sc := NewOutboxSelfCheck(db)
		sc.cancelTerminalParentQueued(context.Background())
		sc.cancelZombieScheduledCampaigns(context.Background())
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("produced_eq_landed_cancels", func(t *testing.T) {
		db, mock := newSelfCheckMockDB(t)

		expectJanitorParents(mock, []string{janitorTestCampaign})
		expectJanitorProducedLanded(mock, janitorTestCampaign, 34851, 34851)
		expectJanitorCancel(mock, 12)

		sc := NewOutboxSelfCheck(db)
		sc.cancelTerminalParentQueued(context.Background())
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("no_terminal_parents_cancels_nothing", func(t *testing.T) {
		db, mock := newSelfCheckMockDB(t)

		expectJanitorParents(mock, []string{janitorTestCampaign})
		// Step B returns no rows: the campaign is still live, not terminal.
		mock.ExpectBegin()
		mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery(`AS produced`).
			WillReturnRows(sqlmock.NewRows([]string{"id", "produced", "landed"}))
		mock.ExpectCommit()

		sc := NewOutboxSelfCheck(db)
		sc.cancelTerminalParentQueued(context.Background())
		require.NoError(t, mock.ExpectationsWereMet())
	})
}

// TestSelfCheck_JanitorKillSwitch proves OUTBOX_SELFCHECK_JANITOR_DISABLED
// actually fires: with it set, NEITHER janitor issues a single statement, while
// the read-only invariants below it keep running. A documented kill switch that
// no-ops is worse than none (Gate F precedent).
func TestSelfCheck_JanitorKillSwitch(t *testing.T) {
	t.Setenv("OUTBOX_SELFCHECK_JANITOR_DISABLED", "true")

	db, mock := newSelfCheckMockDB(t)
	// No janitor or zombie transactions registered at all — any statement the
	// janitors issue fails the test.
	expectCheckTx(mock, reSubmitting, sqlmock.NewRows([]string{"count", "oldest"}).AddRow(int64(0), int64(0)))
	expectCheckTx(mock, reDeadLetter, sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	expectCheckTx(mock, reBacklog, sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	expectCheckTx(mock, reOldest, sqlmock.NewRows([]string{"age"}).AddRow(int64(0)))
	expectTailChecks(mock)

	sc := NewOutboxSelfCheck(db)
	sc.runOnce(context.Background())
	require.NoError(t, mock.ExpectationsWereMet())
}

// =============================================================================
// REQ-087 — the four send-liveness invariants, each proven on its FIRING path
// =============================================================================

// selfCheckTickWith runs one tick where the four legacy checks are clean and the
// caller supplies the rows for the four liveness invariants + the throughput
// sample, so each test below exercises exactly one firing path.
func selfCheckTickWith(t *testing.T, wave, noSend, burst, dead, thru *sqlmock.Rows) []capturedSMS {
	t.Helper()
	db, mock := newSelfCheckMockDB(t)
	expectJanitorTx(mock, 0)
	expectCheckTx(mock, reSubmitting, sqlmock.NewRows([]string{"count", "oldest"}).AddRow(int64(0), int64(0)))
	expectCheckTx(mock, reDeadLetter, sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	expectCheckTx(mock, reBacklog, sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	expectCheckTx(mock, reOldest, sqlmock.NewRows([]string{"age"}).AddRow(int64(0)))
	expectCheckTx(mock, reWaveUnlanded, wave)
	expectCheckTx(mock, reCampaignNoSend, noSend)
	expectCheckTx(mock, reFailedBurst, burst)
	expectCheckTx(mock, reScheduledDead, dead)
	expectCheckTx(mock, reThroughput, thru)

	alerter := &capturingAlerter{}
	sc := NewOutboxSelfCheck(db)
	sc.SetAlerter(alerter, []string{"+15555550100"})
	sc.runOnce(context.Background())
	require.NoError(t, mock.ExpectationsWereMet())
	return alerter.drain()
}

func cleanWave() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"waves", "recips"}).AddRow(int64(0), int64(0))
}
func cleanNoSend() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"c"}).AddRow(int64(0))
}
func cleanBurst() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"cur", "med"}).AddRow(int64(0), float64(0))
}
func cleanDead() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"z", "n"}).AddRow(int64(0), int64(0))
}
func cleanThru() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"n", "last"}).AddRow(int64(1000), time.Now())
}

// TestSelfCheckInvariantWaveUnlanded — the SK-4 signature: waves completed with
// recipients, zero queue rows landed. This is the alert that would have paged at
// T+5min on 2026-09-01 instead of a human noticing at T+90min.
func TestSelfCheckInvariantWaveUnlanded(t *testing.T) {
	got := selfCheckTickWith(t,
		sqlmock.NewRows([]string{"waves", "recips"}).AddRow(int64(16296), int64(219237)),
		cleanNoSend(), cleanBurst(), cleanDead(), cleanThru())
	require.Len(t, got, 1, "unlanded waves must page")
	require.Contains(t, got[0].Body, "16296 wave")
	require.Contains(t, got[0].Body, "219237 recipients")
}

// TestSelfCheckInvariantCampaignNoSend — a wedge BELOW the queue: rows landed,
// nothing sent. Catches PMTA/SES/Kumo being down, which the wave invariant
// cannot see because the rows are present and correct.
func TestSelfCheckInvariantCampaignNoSend(t *testing.T) {
	got := selfCheckTickWith(t, cleanWave(),
		sqlmock.NewRows([]string{"c"}).AddRow(int64(7)),
		cleanBurst(), cleanDead(), cleanThru())
	require.Len(t, got, 1, "sending campaigns with zero sends must page")
	require.Contains(t, got[0].Body, "7 campaign")
}

// TestSelfCheckInvariantFailedBurst — relative threshold, both directions.
func TestSelfCheckInvariantFailedBurst(t *testing.T) {
	t.Run("fires_above_multiple_and_floor", func(t *testing.T) {
		got := selfCheckTickWith(t, cleanWave(), cleanNoSend(),
			sqlmock.NewRows([]string{"cur", "med"}).AddRow(int64(385), float64(3)),
			cleanDead(), cleanThru())
		require.Len(t, got, 1, "385/h against a median of 3 must page")
		require.Contains(t, got[0].Body, "385 campaign")
	})

	t.Run("silent_at_normal_rate", func(t *testing.T) {
		// 9 an hour against a median of 4 is under BOTH the 3x multiple and the
		// absolute floor — the drip lanes fail campaigns at this rate every day.
		got := selfCheckTickWith(t, cleanWave(), cleanNoSend(),
			sqlmock.NewRows([]string{"cur", "med"}).AddRow(int64(9), float64(4)),
			cleanDead(), cleanThru())
		require.Empty(t, got, "baseline failure rate must not page")
	})

	t.Run("floor_beats_a_zero_median", func(t *testing.T) {
		// Median 0 makes any multiple test true; the floor is what stops a single
		// unrelated failure from paging.
		got := selfCheckTickWith(t, cleanWave(), cleanNoSend(),
			sqlmock.NewRows([]string{"cur", "med"}).AddRow(int64(2), float64(0)),
			cleanDead(), cleanThru())
		require.Empty(t, got, "2 failures against a zero median must not page")
	})
}

// TestSelfCheckInvariantScheduledDead — the deploy-side silent failure: a
// campaign that reached 'scheduled', is past its send time, and either planned
// zero recipients or produced no wave.
func TestSelfCheckInvariantScheduledDead(t *testing.T) {
	t.Run("zero_recipients_fires", func(t *testing.T) {
		got := selfCheckTickWith(t, cleanWave(), cleanNoSend(), cleanBurst(),
			sqlmock.NewRows([]string{"z", "n"}).AddRow(int64(105), int64(0)), cleanThru())
		require.Len(t, got, 1)
		require.Contains(t, got[0].Body, "105 scheduled campaign")
	})

	t.Run("no_wave_fires_on_its_own", func(t *testing.T) {
		got := selfCheckTickWith(t, cleanWave(), cleanNoSend(), cleanBurst(),
			sqlmock.NewRows([]string{"z", "n"}).AddRow(int64(0), int64(29)), cleanThru())
		require.Len(t, got, 1, "no-wave alone must page even when recipients look planned")
		require.Contains(t, got[0].Body, "29 with no wave")
	})
}

// TestSelfCheckSendLivenessSnapshot — the /health gauge. The tick must publish
// what it measured, and a probe that FAILED must be reported as an error rather
// than as a zero (a silent 0 in sent_last_15m reads as an outage).
func TestSelfCheckSendLivenessSnapshot(t *testing.T) {
	t.Run("publishes_measured_values", func(t *testing.T) {
		last := time.Now().Add(-30 * time.Second).UTC().Truncate(time.Second)
		db, mock := newSelfCheckMockDB(t)
		expectJanitorTx(mock, 0)
		expectCheckTx(mock, reSubmitting, sqlmock.NewRows([]string{"count", "oldest"}).AddRow(int64(0), int64(0)))
		expectCheckTx(mock, reDeadLetter, sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
		expectCheckTx(mock, reBacklog, sqlmock.NewRows([]string{"c"}).AddRow(int64(140639)))
		expectCheckTx(mock, reOldest, sqlmock.NewRows([]string{"age"}).AddRow(int64(0)))
		expectCheckTx(mock, reWaveUnlanded, sqlmock.NewRows([]string{"waves", "recips"}).AddRow(int64(3), int64(4200)))
		expectCheckTx(mock, reCampaignNoSend, cleanNoSend())
		expectCheckTx(mock, reFailedBurst, cleanBurst())
		expectCheckTx(mock, reScheduledDead, cleanDead())
		expectCheckTx(mock, reThroughput, sqlmock.NewRows([]string{"n", "last"}).AddRow(int64(51234), last))

		sc := NewOutboxSelfCheck(db)
		sc.runOnce(context.Background())

		snap := CurrentSendLiveness()
		require.Equal(t, int64(3), snap.UnlandedWaves)
		require.Equal(t, int64(4200), snap.UnlandedRecipients)
		require.Equal(t, int64(51234), snap.SentLast15m)
		require.Equal(t, int64(140639), snap.QueueReadyRows)
		require.NotNil(t, snap.LastSentAt)
		require.Equal(t, last.Unix(), snap.LastSentAt.Unix())
		require.NotNil(t, snap.CheckedAt, "checked_at must be stamped so a stale tick is visible")
		require.Empty(t, snap.Errors)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("failed_probe_is_an_error_not_a_zero", func(t *testing.T) {
		db, mock := newSelfCheckMockDB(t)
		expectJanitorTx(mock, 0)
		expectCheckTx(mock, reSubmitting, sqlmock.NewRows([]string{"count", "oldest"}).AddRow(int64(0), int64(0)))
		expectCheckTx(mock, reDeadLetter, sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
		expectCheckTx(mock, reBacklog, sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
		expectCheckTx(mock, reOldest, sqlmock.NewRows([]string{"age"}).AddRow(int64(0)))
		expectCheckTx(mock, reWaveUnlanded, cleanWave())
		expectCheckTx(mock, reCampaignNoSend, cleanNoSend())
		expectCheckTx(mock, reFailedBurst, cleanBurst())
		expectCheckTx(mock, reScheduledDead, cleanDead())
		expectCheckTxErr(mock, reThroughput)

		sc := NewOutboxSelfCheck(db)
		sc.runOnce(context.Background())

		snap := CurrentSendLiveness()
		require.Equal(t, int64(0), snap.SentLast15m)
		require.Contains(t, snap.Errors, "sent_last_15m",
			"a failed throughput probe must be labelled, never published as a healthy-looking zero")
		require.NoError(t, mock.ExpectationsWereMet())
	})
}
