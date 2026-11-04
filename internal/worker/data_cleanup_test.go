package worker

import (
	"bytes"
	"context"
	"errors"
	"log"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// busyProbe matches primaryBusy's pg_stat_activity probe.
var busyProbe = regexp.MustCompile(`FROM pg_stat_activity`)

// slimUpdate matches the slimmer's batched UPDATE.
var slimUpdate = regexp.MustCompile(`UPDATE mailing_campaign_queue q\s+SET html_content = NULL`)

func newCleanupMock(t *testing.T) (*DataCleanupWorker, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return &DataCleanupWorker{db: db, interval: DefaultCleanupInterval}, mock, func() { db.Close() }
}

// When the primary is under heavy IO load, the slimmer must defer without
// issuing any UPDATE.
func TestSlimAcceptedQueueHTML_DefersWhenPrimaryBusy(t *testing.T) {
	dc, mock, done := newCleanupMock(t)
	defer done()

	mock.ExpectQuery(busyProbe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(slimMaxIOWaitBackends))

	dc.slimAcceptedQueueHTML(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// A failing/slow load probe must fail closed (defer), never proceed to slim.
func TestSlimAcceptedQueueHTML_FailsClosedOnProbeError(t *testing.T) {
	dc, mock, done := newCleanupMock(t)
	defer done()

	mock.ExpectQuery(busyProbe.String()).WillReturnError(errors.New("probe timeout"))

	dc.slimAcceptedQueueHTML(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Happy path: idle primary -> slim drains batches until an UPDATE affects zero
// rows, then stops.
func TestSlimAcceptedQueueHTML_DrainsUntilEmpty(t *testing.T) {
	dc, mock, done := newCleanupMock(t)
	defer done()

	mock.ExpectQuery(busyProbe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	// First batch slims a full batch, second batch finds nothing -> loop ends.
	mock.ExpectExec(slimUpdate.String()).
		WithArgs(slimBatchSize).
		WillReturnResult(sqlmock.NewResult(0, slimBatchSize))
	mock.ExpectExec(slimUpdate.String()).
		WithArgs(slimBatchSize).
		WillReturnResult(sqlmock.NewResult(0, 0))

	dc.slimAcceptedQueueHTML(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// A missing table on the first batch is swallowed (worker safe before
// migrations) and does not panic or loop.
func TestSlimAcceptedQueueHTML_MissingTableIsSafe(t *testing.T) {
	dc, mock, done := newCleanupMock(t)
	defer done()

	mock.ExpectQuery(busyProbe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(slimUpdate.String()).
		WithArgs(slimBatchSize).
		WillReturnError(errors.New(`relation "mailing_campaign_queue" does not exist`))

	dc.slimAcceptedQueueHTML(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPrimaryBusy_Thresholds(t *testing.T) {
	tests := []struct {
		name     string
		ioWait   int
		wantTrue bool
	}{
		{"below threshold proceeds", slimMaxIOWaitBackends - 1, false},
		{"at threshold defers", slimMaxIOWaitBackends, true},
		{"above threshold defers", slimMaxIOWaitBackends + 5, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dc, mock, done := newCleanupMock(t)
			defer done()
			mock.ExpectQuery(busyProbe.String()).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(tt.ioWait))
			if got := dc.primaryBusy(context.Background()); got != tt.wantTrue {
				t.Fatalf("primaryBusy(ioWait=%d) = %v, want %v", tt.ioWait, got, tt.wantTrue)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("expectations: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// WP-C 2026-11-04: per-cycle leader lease + adaptive batchDelete LIMIT.
// ---------------------------------------------------------------------------

var (
	leaseAcquire = regexp.QuoteMeta("SELECT pg_try_advisory_lock($1)")
	leaseRelease = regexp.QuoteMeta("SELECT pg_advisory_unlock($1)")
	wpcDelete    = `DELETE FROM wpc_table`
)

// captureCleanupLog redirects the std logger for the test and returns the buffer.
func captureCleanupLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return &buf
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// (a) The other replica holds the lease -> the whole cycle is skipped: no
// query beyond the lock probe, exactly one log line, no heartbeat.
func TestDataCleanup_LeaseNotAcquired_SkipsCycle(t *testing.T) {
	t.Setenv("DATA_CLEANUP_LEASE_DISABLED", "")
	dc, mock, done := newCleanupMock(t)
	defer done()
	buf := captureCleanupLog(t)

	mock.ExpectQuery(leaseAcquire).WithArgs(dataCleanupLeaseID()).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(false))

	dc.cleanup(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	lines := nonEmptyLines(buf.String())
	if len(lines) != 1 || !strings.Contains(lines[0], "cycle lease held by the other instance — skipping cycle") {
		t.Fatalf("want exactly one skip line, got %d: %q", len(lines), lines)
	}
}

// Lease acquired -> the cycle runs, and the unlock is issued on the dedicated
// connection at the end.
func TestDataCleanup_LeaseAcquired_RunsCycleAndReleases(t *testing.T) {
	t.Setenv("DATA_CLEANUP_LEASE_DISABLED", "")
	dc, mock, done := newCleanupMock(t)
	defer done()
	buf := captureCleanupLog(t)

	mock.ExpectQuery(leaseAcquire).WithArgs(dataCleanupLeaseID()).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	mock.ExpectQuery(leaseRelease).WithArgs(dataCleanupLeaseID()).
		WillReturnRows(sqlmock.NewRows([]string{"pg_advisory_unlock"}).AddRow(true))

	dc.cleanup(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	if !strings.Contains(buf.String(), "Cleanup cycle completed") {
		t.Fatalf("cycle did not run:\n%s", buf.String())
	}
}

// (c) Kill switch -> no lease query at all; the cycle runs (old behavior: both
// replicas run).
func TestDataCleanup_LeaseKillSwitch_RunsWithoutLease(t *testing.T) {
	t.Setenv("DATA_CLEANUP_LEASE_DISABLED", "1")
	dc, mock, done := newCleanupMock(t)
	defer done()
	buf := captureCleanupLog(t)

	dc.cleanup(context.Background())

	out := buf.String()
	if strings.Contains(out, "pg_try_advisory_lock") || strings.Contains(out, "cycle lease") {
		t.Fatalf("kill switch set but lease was touched:\n%s", out)
	}
	if !strings.Contains(out, "Cleanup cycle completed") {
		t.Fatalf("cycle did not run under kill switch:\n%s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Lock-infra error -> fail OPEN: the cycle still runs.
func TestDataCleanup_LeaseInfraError_FailsOpen(t *testing.T) {
	t.Setenv("DATA_CLEANUP_LEASE_DISABLED", "")
	dc, mock, done := newCleanupMock(t)
	defer done()
	buf := captureCleanupLog(t)

	mock.ExpectQuery(leaseAcquire).WithArgs(dataCleanupLeaseID()).
		WillReturnError(errors.New("pq: connection reset by peer"))

	dc.cleanup(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "cycle lease acquire error (fail-open, running cycle anyway)") {
		t.Fatalf("missing fail-open line:\n%s", out)
	}
	if !strings.Contains(out, "Cleanup cycle completed") {
		t.Fatalf("cycle did not run on lock-infra error:\n%s", out)
	}
}

const wpcQuery = "DELETE FROM wpc_table WHERE id IN (SELECT id FROM wpc_table LIMIT $1)"

var errStmtTimeout = errors.New("pq: canceling statement due to statement timeout")

// (b) A statement timeout halves the LIMIT on the very next DELETE, and the
// following cycle starts at the reduced size — not back at 10,000.
func TestDataCleanup_BatchDelete_TimeoutHalvesLimitAndPersists(t *testing.T) {
	t.Setenv("DATA_CLEANUP_ADAPTIVE_BATCH_DISABLED", "")
	dc, mock, done := newCleanupMock(t)
	defer done()
	buf := captureCleanupLog(t)

	// Cycle 1.
	mock.ExpectExec(wpcDelete).WithArgs(cleanupBatchSize).WillReturnError(errStmtTimeout)
	mock.ExpectExec(wpcDelete).WithArgs(cleanupBatchSize / 2).WillReturnResult(sqlmock.NewResult(0, 0))
	// Cycle 2: starts at the reduced size.
	mock.ExpectExec(wpcDelete).WithArgs(cleanupBatchSize / 2).WillReturnResult(sqlmock.NewResult(0, 0))

	dc.resetCycleBatchState()
	dc.batchDelete(context.Background(), "wpc_table", wpcQuery)
	dc.resetCycleBatchState()
	dc.batchDelete(context.Background(), "wpc_table", wpcQuery)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	if got := dc.batchSizeFor("wpc_table"); got != cleanupBatchSize/2 {
		t.Fatalf("batch size after cycle 2 = %d, want %d", got, cleanupBatchSize/2)
	}
	changes := strings.Count(buf.String(), "[DataCleanup] batch size ")
	if changes != 1 || !strings.Contains(buf.String(), "batch size 10000 -> 5000 after statement timeout on wpc_table") {
		t.Fatalf("want one size-change line, got %d:\n%s", changes, buf.String())
	}
}

// Reviewer (a): a lookup-bound delete that times out at every size costs
// EXACTLY 2 timed-out statements in a cycle (10,000 then 5,000), then the
// table's pass ends; cycle 2 starts at 5,000 and may shrink once more.
func TestDataCleanup_BatchDelete_LookupBoundTimeout_ShrinksOncePerCycle(t *testing.T) {
	t.Setenv("DATA_CLEANUP_ADAPTIVE_BATCH_DISABLED", "")
	dc, mock, done := newCleanupMock(t)
	defer done()
	buf := captureCleanupLog(t)

	// Cycle 1: exactly two statements, both time out.
	mock.ExpectExec(wpcDelete).WithArgs(10000).WillReturnError(errStmtTimeout)
	mock.ExpectExec(wpcDelete).WithArgs(5000).WillReturnError(errStmtTimeout)
	dc.resetCycleBatchState()
	dc.batchDelete(context.Background(), "wpc_table", wpcQuery)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("cycle 1 expectations: %v", err)
	}
	out := buf.String()
	// A third statement would hit sqlmock's "not expected" error instead of a
	// timeout; the pass must end on the SECOND timeout.
	if strings.Count(out, "Error deleting from wpc_table") != 1 ||
		!strings.Contains(out, "Error deleting from wpc_table: pq: canceling statement due to statement timeout") ||
		strings.Contains(out, "was not expected") || strings.Contains(out, "5000 -> 2500") {
		t.Fatalf("cycle 1 must end after 2 timed-out statements:\n%s", out)
	}
	if got := dc.batchSizeFor("wpc_table"); got != 5000 {
		t.Fatalf("size at cycle 2 start = %d, want 5000", got)
	}

	// Cycle 2: starts at 5,000; one more shrink allowed.
	mock.ExpectExec(wpcDelete).WithArgs(5000).WillReturnError(errStmtTimeout)
	mock.ExpectExec(wpcDelete).WithArgs(2500).WillReturnResult(sqlmock.NewResult(0, 0))
	dc.resetCycleBatchState()
	dc.batchDelete(context.Background(), "wpc_table", wpcQuery)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("cycle 2 expectations: %v", err)
	}
}

// At the floor a timeout is not retried at all (one statement, pass ends).
func TestDataCleanup_BatchDelete_AtFloorNoRetry(t *testing.T) {
	t.Setenv("DATA_CLEANUP_ADAPTIVE_BATCH_DISABLED", "")
	dc, mock, done := newCleanupMock(t)
	defer done()
	buf := captureCleanupLog(t)
	dc.batchByTable = map[string]int{"wpc_table": cleanupBatchFloor}

	mock.ExpectExec(wpcDelete).WithArgs(cleanupBatchFloor).WillReturnError(errStmtTimeout)
	dc.resetCycleBatchState()
	dc.batchDelete(context.Background(), "wpc_table", wpcQuery)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	if strings.Contains(buf.String(), "was not expected") || dc.batchSizeFor("wpc_table") != cleanupBatchFloor {
		t.Fatalf("floor timeout must not retry:\n%s", buf.String())
	}
}

// Reviewer (b): zero-row successes — on other tables or on the failing table
// itself — never regrow the failing table's size; only deletes that removed
// rows on THAT table do (3 in a row -> x2).
func TestDataCleanup_BatchDelete_NoRegrowFromZeroRowOrOtherTables(t *testing.T) {
	t.Setenv("DATA_CLEANUP_ADAPTIVE_BATCH_DISABLED", "")
	dc, mock, done := newCleanupMock(t)
	defer done()
	captureCleanupLog(t)
	otherDelete := `DELETE FROM other_table`
	otherQuery := "DELETE FROM other_table WHERE id IN (SELECT id FROM other_table LIMIT $1)"

	dc.resetCycleBatchState()
	mock.ExpectExec(wpcDelete).WithArgs(10000).WillReturnError(errStmtTimeout)
	mock.ExpectExec(wpcDelete).WithArgs(5000).WillReturnResult(sqlmock.NewResult(0, 0))
	dc.batchDelete(context.Background(), "wpc_table", wpcQuery)

	for i := 0; i < 9; i++ {
		mock.ExpectExec(otherDelete).WithArgs(10000).WillReturnResult(sqlmock.NewResult(0, 0))
		dc.batchDelete(context.Background(), "other_table", otherQuery)
	}
	for i := 0; i < 3; i++ {
		mock.ExpectExec(wpcDelete).WithArgs(5000).WillReturnResult(sqlmock.NewResult(0, 0))
		dc.batchDelete(context.Background(), "wpc_table", wpcQuery)
	}
	if got := dc.batchSizeFor("wpc_table"); got != 5000 {
		t.Fatalf("zero-row successes regrew wpc_table to %d, want 5000", got)
	}

	// Three deletes that removed rows on wpc_table -> doubles to 10,000.
	for i := 0; i < cleanupBatchGrowAfter; i++ {
		mock.ExpectExec(wpcDelete).WithArgs(5000).WillReturnResult(sqlmock.NewResult(0, 5000))
	}
	mock.ExpectExec(wpcDelete).WithArgs(10000).WillReturnResult(sqlmock.NewResult(0, 0))
	dc.batchDelete(context.Background(), "wpc_table", wpcQuery)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	if got := dc.batchSizeFor("wpc_table"); got != cleanupBatchSize {
		t.Fatalf("size after 3 row-removing deletes = %d, want %d", got, cleanupBatchSize)
	}
	if got := dc.batchSizeFor("other_table"); got != cleanupBatchSize {
		t.Fatalf("other_table size = %d, want %d", got, cleanupBatchSize)
	}
}

// Reviewer (c): DATA_CLEANUP_ADAPTIVE_BATCH_DISABLED=1 -> every path uses the
// fixed 10,000, including after a timeout (old behavior).
func TestDataCleanup_AdaptiveBatchKillSwitch_FixedSizeEverywhere(t *testing.T) {
	t.Setenv("DATA_CLEANUP_ADAPTIVE_BATCH_DISABLED", " TRUE ")
	dc, mock, done := newCleanupMock(t)
	defer done()
	buf := captureCleanupLog(t)
	dc.resetCycleBatchState()

	// batchDelete: timeout ends the pass with no retry; next call still 10,000.
	mock.ExpectExec(wpcDelete).WithArgs(cleanupBatchSize).WillReturnError(errStmtTimeout)
	mock.ExpectExec(wpcDelete).WithArgs(cleanupBatchSize).WillReturnResult(sqlmock.NewResult(0, 0))
	dc.batchDelete(context.Background(), "wpc_table", wpcQuery)
	dc.batchDelete(context.Background(), "wpc_table", wpcQuery)

	// Terminal purge: timeout, retry still at 10,000.
	termDelete := `(?s)WITH doomed AS .*DELETE FROM mailing_campaign_queue q`
	mock.ExpectQuery(busyProbe.String()).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(termDelete).WithArgs(cleanupBatchSize).WillReturnError(errStmtTimeout)
	mock.ExpectExec(termDelete).WithArgs(cleanupBatchSize).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`(?s)COUNT\(\*\).*status IN \('accepted', 'cancelled'`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`(?s)COUNT\(\*\).*html_content IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	dc.cleanupTerminalQueueItems(context.Background())

	// plan_recipients fallback: timeout ends the pass at 10,000.
	prDelete := `(?s)WITH doomed AS .*DELETE FROM mailing_campaign_plan_recipients pr`
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT relkind = 'p' FROM pg_class`)).
		WillReturnRows(sqlmock.NewRows([]string{"p"}).AddRow(false))
	mock.ExpectQuery(`(?s)SELECT c.id\s+FROM mailing_campaigns c`).WithArgs(planRecipMaxCampaignsPerCycle).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("c1"))
	mock.ExpectExec(prDelete).WithArgs("c1", cleanupBatchSize).WillReturnError(errStmtTimeout)
	dc.cleanupPlanRecipients(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "[DataCleanup] batch size ") || strings.Contains(out, "was not expected") {
		t.Fatalf("kill switch set but the LIMIT adapted:\n%s", out)
	}
	for _, k := range []string{"wpc_table", terminalPurgeKey, planRecipientsKey} {
		if got := dc.batchSizeFor(k); got != cleanupBatchSize {
			t.Fatalf("%s size = %d, want %d", k, got, cleanupBatchSize)
		}
	}
}

// Reviewer (d): if pg_advisory_unlock errors, the dedicated connection is
// DISCARDED (driver conn closed via ErrBadConn), never returned to the pool
// with the lock possibly still held.
func TestDataCleanup_LeaseUnlockError_DiscardsConnection(t *testing.T) {
	t.Setenv("DATA_CLEANUP_LEASE_DISABLED", "")
	dc, mock, done := newCleanupMock(t)
	defer done()
	buf := captureCleanupLog(t)

	mock.ExpectQuery(leaseAcquire).WithArgs(dataCleanupLeaseID()).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	mock.ExpectQuery(leaseRelease).WithArgs(dataCleanupLeaseID()).
		WillReturnError(errors.New("pq: server closed the connection unexpectedly"))
	// The driver connection must be closed by the release itself — a conn
	// returned to the pool would only close at db.Close() in done(), after
	// ExpectationsWereMet below.
	mock.ExpectClose()

	release, lead := dc.acquireCycleLease(context.Background())
	if !lead {
		t.Fatal("expected to lead")
	}
	release()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("connection was not discarded: %v", err)
	}
	if st := dc.db.Stats(); st.Idle != 0 || st.OpenConnections != 0 {
		t.Fatalf("conn returned to pool: idle=%d open=%d", st.Idle, st.OpenConnections)
	}
	if !strings.Contains(buf.String(), "connection discarded so the lock dies with the session") {
		t.Fatalf("missing discard log line:\n%s", buf.String())
	}
}

// Terminal queue purge: a timeout halves the LIMIT on the very next DELETE,
// and the next cycle's purge starts at the reduced size. The retry budget
// (terminalPurgeMaxBatchRetries) is unchanged.
func TestDataCleanup_TerminalPurge_TimeoutHalvesLimit(t *testing.T) {
	dc, mock, done := newCleanupMock(t)
	defer done()
	buf := captureCleanupLog(t)
	termDelete := `(?s)WITH doomed AS .*DELETE FROM mailing_campaign_queue q`
	expectStats := func() {
		mock.ExpectQuery(`(?s)COUNT\(\*\).*status IN \('accepted', 'cancelled'`).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
		mock.ExpectQuery(`(?s)COUNT\(\*\).*html_content IS NOT NULL`).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	}

	// Cycle 1.
	mock.ExpectQuery(busyProbe.String()).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(termDelete).WithArgs(cleanupBatchSize).
		WillReturnError(errors.New("pq: canceling statement due to statement timeout"))
	mock.ExpectExec(termDelete).WithArgs(cleanupBatchSize / 2).WillReturnResult(sqlmock.NewResult(0, 0))
	expectStats()
	// Cycle 2: starts at the reduced size.
	mock.ExpectQuery(busyProbe.String()).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(termDelete).WithArgs(cleanupBatchSize / 2).WillReturnResult(sqlmock.NewResult(0, 0))
	expectStats()

	dc.cleanupTerminalQueueItems(context.Background())
	dc.cleanupTerminalQueueItems(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	if !strings.Contains(buf.String(), "batch size 10000 -> 5000 after statement timeout on mailing_campaign_queue (terminal purge)") {
		t.Fatalf("missing size-change line:\n%s", buf.String())
	}
}

// plan_recipients non-partitioned fallback: a client-deadline timeout
// (context.DeadlineExceeded) halves the LIMIT and the same campaign is retried
// at the smaller size.
func TestDataCleanup_PlanRecipientsFallback_TimeoutHalvesLimit(t *testing.T) {
	dc, mock, done := newCleanupMock(t)
	defer done()
	captureCleanupLog(t)
	prDelete := `(?s)WITH doomed AS .*DELETE FROM mailing_campaign_plan_recipients pr`

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT relkind = 'p' FROM pg_class`)).
		WillReturnRows(sqlmock.NewRows([]string{"p"}).AddRow(false))
	mock.ExpectQuery(`(?s)SELECT c.id\s+FROM mailing_campaigns c`).WithArgs(planRecipMaxCampaignsPerCycle).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("c1"))
	mock.ExpectExec(prDelete).WithArgs("c1", cleanupBatchSize).WillReturnError(context.DeadlineExceeded)
	mock.ExpectExec(prDelete).WithArgs("c1", cleanupBatchSize/2).WillReturnResult(sqlmock.NewResult(0, 0))

	dc.cleanupPlanRecipients(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	if got := dc.batchSizeFor(planRecipientsKey); got != cleanupBatchSize/2 {
		t.Fatalf("batch size = %d, want %d", got, cleanupBatchSize/2)
	}
}

func TestDataCleanup_IsBatchTimeoutError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{errors.New("pq: canceling statement due to statement timeout"), true},
		{context.DeadlineExceeded, true},
		{errors.New("read tcp: context deadline exceeded"), true},
		{context.Canceled, false},
		{errors.New(`relation "x" does not exist`), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isBatchTimeoutError(c.err); got != c.want {
			t.Errorf("isBatchTimeoutError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
