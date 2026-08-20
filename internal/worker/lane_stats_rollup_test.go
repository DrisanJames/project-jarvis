package worker

// LaneStatsRollupWorker fixtures. Permanent fixtures:
//
//   - ADDITIVE ONLY: no statement this worker can issue is a DELETE / TRUNCATE
//     / DROP, and the day statement is an INSERT … ON CONFLICT DO UPDATE. This
//     is the operator's standing constraint on the whole body of work, so it is
//     asserted against the SQL text and against the whole FILE, not just the
//     one const.
//   - SQL PARITY with internal/api/property_lane_stats.go: past-tense event
//     types ('opened'/'clicked' — the present tense is a SILENT ZERO), a
//     param-injected event_at bound (mailing_tracking_events is
//     RANGE-partitioned), no tz-cast anywhere (non-sargable), the unnest JOIN,
//     and the backfill-artifact exclusion.
//   - RE-RUN SAFETY: this worker re-fires on every ECS bounce. A closed day
//     already computed AFTER it closed is not recomputed; today is not
//     recomputed inside its min-age. Proven by an sqlmock that declares NO
//     upsert — an unexpected exec would fail the pass and be visible.
//   - a closed day whose stored row predates the day's close is PARTIAL and IS
//     recomputed.
//   - A FAILED DAY IS A GAP: the tx rolls back, nothing is written, and the
//     heartbeat says 'error'. There is no zero row and no half-written day.
//   - kill switch: LANE_STATS_ROLLUP_DISABLED ⇒ one heartbeat, nothing else.

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newLaneStatsRollupWorkerWithMock(t *testing.T) (*LaneStatsRollupWorker, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	w := NewLaneStatsRollupWorker(db, nil)
	return w, mock
}

// ── 4. UPSERT ONLY — the additive-only constraint ───────────────────────────

func TestLaneStatsRollupUpsertIssuesNoDelete(t *testing.T) {
	// The day statement is an upsert, full stop.
	if !strings.Contains(laneStatsRollupDaySQL, "INSERT INTO mailing_lane_stats_daily") {
		t.Fatal("the day statement must INSERT into mailing_lane_stats_daily")
	}
	if !strings.Contains(laneStatsRollupDaySQL,
		"ON CONFLICT (organization_id, vertical, brand, day, isp) DO UPDATE SET") {
		t.Fatal("the day statement must UPSERT on the full PK — never insert-or-fail, never delete-then-insert")
	}
	for _, bad := range []string{"DELETE", "TRUNCATE", "DROP ", "DROP\n"} {
		if strings.Contains(strings.ToUpper(laneStatsRollupDaySQL), bad) {
			t.Fatalf("day statement contains %q — this worker is ADDITIVE ONLY", bad)
		}
	}

	// And the whole file: no destructive statement can hide in a sibling const
	// or an inline query. Comments are stripped first so prose about "no
	// DELETE" does not trip the guard.
	src, err := os.ReadFile("lane_stats_rollup.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	code := stripGoComments(string(src))
	for _, bad := range []string{"DELETE FROM", "TRUNCATE ", "DROP TABLE", "DROP INDEX"} {
		if strings.Contains(strings.ToUpper(code), bad) {
			t.Fatalf("lane_stats_rollup.go contains %q outside comments — ADDITIVE ONLY, no deletes", bad)
		}
	}

	// The DDL is additive too, and small enough for the 5s startup-migration
	// budget: CREATE TABLE only, no backfill, no data movement.
	if !strings.Contains(LaneStatsRollupDDL, "CREATE TABLE IF NOT EXISTS mailing_lane_stats_daily") {
		t.Fatal("LaneStatsRollupDDL must be an idempotent CREATE TABLE")
	}
	if !strings.Contains(LaneStatsRollupDDL,
		"PRIMARY KEY (organization_id, vertical, brand, day, isp)") {
		t.Fatal("the PK must be (organization_id, vertical, brand, day, isp) — org-scoped by construction")
	}
	for _, bad := range []string{"INSERT", "UPDATE", "DELETE", "SELECT"} {
		if strings.Contains(strings.ToUpper(LaneStatsRollupDDL), bad) {
			t.Fatalf("LaneStatsRollupDDL must carry no data statement (%q) — the 5s budget is DDL only", bad)
		}
	}
	// The index is a SEPARATE statement: cmd/server/migration_skip.go classifies
	// a statement by its leading keywords, so a combined
	// `CREATE TABLE …; CREATE INDEX …` string is probed as CREATE TABLE and
	// skipped WHOLESALE once the table exists — the index would never land.
	if strings.Contains(LaneStatsRollupDDL, "CREATE INDEX") {
		t.Fatal("the index must NOT ride inside LaneStatsRollupDDL — migrationSkipProbe would skip it forever")
	}
	if !strings.Contains(LaneStatsRollupIndexDDL, "CREATE INDEX IF NOT EXISTS") {
		t.Fatal("LaneStatsRollupIndexDDL must be idempotent")
	}
	if stmts := LaneStatsRollupDDLStatements(); len(stmts) != 2 {
		t.Fatalf("the wiring helper must hand the boot path TWO entries, got %d", len(stmts))
	}
	for _, s := range LaneStatsRollupDDLStatements() {
		if strings.Contains(s.SQL, ";") {
			t.Fatalf("%s: one statement per migration entry (migrationSkipProbe reads the leading keywords)", s.Name)
		}
	}
}

var reGoLineComment = regexp.MustCompile(`(?m)^\s*//.*$`)

func stripGoComments(src string) string { return reGoLineComment.ReplaceAllString(src, "") }

// ── SQL parity with the endpoint ────────────────────────────────────────────

func TestLaneStatsRollupSQLMirrorsTheEndpoint(t *testing.T) {
	for _, frag := range []string{"'opened'", "'clicked'", "'sent'", "'delivered'"} {
		if !strings.Contains(laneStatsRollupDaySQL, frag) {
			t.Fatalf("must use the past-tense event type %s (present tense is a SILENT ZERO)", frag)
		}
	}
	for _, bad := range []string{"event_type = 'open'", "event_type='open'",
		"event_type = 'click'", "event_type='click'"} {
		if strings.Contains(laneStatsRollupDaySQL, bad) {
			t.Fatalf("present-tense %q is a SILENT ZERO", bad)
		}
	}
	if !strings.Contains(laneStatsRollupDaySQL, "m.event_at >= $2 AND m.event_at < $3") {
		t.Fatal("mailing_tracking_events is RANGE-partitioned on event_at — the bound is mandatory")
	}
	if !strings.Contains(laneStatsRollupDaySQL, "unnest($1::uuid[]) AS cid") ||
		!strings.Contains(laneStatsRollupDaySQL, "JOIN mailing_tracking_events m ON m.campaign_id = c.cid") {
		t.Fatal("keep the unnest JOIN shape — `= ANY` seq-scans the month partition")
	}
	if !strings.Contains(laneStatsRollupDaySQL, "(undefined status)") {
		t.Fatal("keep the backfill-artifact exclusion (partner_lane_report._artifact_pred)")
	}
	for name, q := range map[string]string{
		"day":       laneStatsRollupDaySQL,
		"campaign":  laneStatsRollupCampaignSQL,
		"freshness": laneStatsRollupFreshnessSQL,
	} {
		if strings.Contains(q, "AT TIME ZONE") {
			t.Fatalf("%s SQL must not tz-cast — non-sargable; Denver bounds are Go-computed", name)
		}
	}
	// Org scope on every write path key.
	if !strings.Contains(laneStatsRollupCampaignSQL, "organization_id = $1::uuid") ||
		!strings.Contains(laneStatsRollupFreshnessSQL, "organization_id = $1::uuid") {
		t.Fatal("every rollup read must be org-scoped")
	}
	// The lane source is the LIVE binding table, active rows only.
	if !strings.Contains(laneStatsRollupLanesSQL, "FROM partner_drip_vertical_roster") ||
		!strings.Contains(laneStatsRollupLanesSQL, "active = TRUE") {
		t.Fatal("lanes come from the ACTIVE rows of partner_drip_vertical_roster")
	}
	// The anchored-exact name pattern must match the endpoint's byte for byte.
	if got := laneStatsRollupNamePattern("internal_auto_insurance", ""); got != `^\[partner-drip\] internal_auto_insurance ` {
		t.Fatalf("vertical pattern drifted from the endpoint: %q", got)
	}
	if got := laneStatsRollupNamePattern("internal_auto_insurance", "db"); got != `^\[partner-drip\] internal_auto_insurance db ` {
		t.Fatalf("brand pattern drifted from the endpoint: %q", got)
	}
}

// ── kill switch ─────────────────────────────────────────────────────────────

func TestLaneStatsRollupDisabledHeartbeatOnly(t *testing.T) {
	t.Setenv("LANE_STATS_ROLLUP_DISABLED", "1")
	w, mock := newLaneStatsRollupWorkerWithMock(t)

	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(laneStatsRollupWorkerName, "disabled", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.tick(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a disabled tick must be heartbeat-only: %v", err)
	}
}

// ── 5. RE-RUN SAFETY ────────────────────────────────────────────────────────

// laneStatsRollupExpectPlan queues the three planning reads for one org × one
// lane over a 2-day horizon.
func laneStatsRollupExpectPlan(mock sqlmock.Sqlmock, fresh *sqlmock.Rows) {
	mock.ExpectQuery(`FROM organizations`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("00000000-0000-0000-0000-000000000001"))
	mock.ExpectQuery(`FROM partner_drip_vertical_roster`).
		WillReturnRows(sqlmock.NewRows([]string{"vertical", "brand"}).
			AddRow("internal_auto_insurance", "db"))
	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
			AddRow("11111111-1111-1111-1111-111111111111", time.Now().Add(-240*time.Hour)))
	mock.ExpectQuery(`FROM mailing_lane_stats_daily`).WillReturnRows(fresh)
}

func TestLaneStatsRollupSecondRunRecomputesNothing(t *testing.T) {
	w, mock := newLaneStatsRollupWorkerWithMock(t)
	w.WithHorizonDays(2)

	now := time.Now()
	today := denverDate(now, w.loc)
	yesterday := today.AddDate(0, 0, -1)
	_, yesterdayEnd := denverDayWindowUTC(yesterday, w.loc)

	// The state a first pass leaves behind: the closed day computed AFTER it
	// closed, today computed a moment ago.
	laneStatsRollupExpectPlan(mock, sqlmock.NewRows([]string{"day", "computed_at"}).
		AddRow(yesterday.Format("2006-01-02"), yesterdayEnd.Add(time.Minute)).
		AddRow(today.Format("2006-01-02"), now.Add(-time.Minute)))
	// NO ExpectBegin / no upsert exec: a re-fire must recompute NOTHING. An
	// unexpected Begin here fails the pass and shows up as a plan/day failure.
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(laneStatsRollupWorkerName, "ok", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a re-run over already-computed days must issue no work: %v", err)
	}
}

// The inverse control: a closed day whose row predates the day's close is
// PARTIAL, and IS recomputed — so the skip rule is a freshness rule, not a
// blanket "any row means done".
func TestLaneStatsRollupRecomputesPartialClosedDay(t *testing.T) {
	w, mock := newLaneStatsRollupWorkerWithMock(t)
	w.WithHorizonDays(2)

	now := time.Now()
	today := denverDate(now, w.loc)
	yesterday := today.AddDate(0, 0, -1)
	yStart, yEnd := denverDayWindowUTC(yesterday, w.loc)

	laneStatsRollupExpectPlan(mock, sqlmock.NewRows([]string{"day", "computed_at"}).
		// written an hour BEFORE the day closed → partial
		AddRow(yesterday.Format("2006-01-02"), yEnd.Add(-time.Hour)).
		AddRow(today.Format("2006-01-02"), now.Add(-time.Minute)))

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO mailing_lane_stats_daily`).
		WithArgs(sqlmock.AnyArg(), yStart, yEnd,
			"00000000-0000-0000-0000-000000000001", "internal_auto_insurance", "db",
			yesterday.Format("2006-01-02"), 1).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(laneStatsRollupWorkerName, "ok", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the partial closed day must be recomputed (and only it): %v", err)
	}
}

// ── 6. A FAILED DAY IS A GAP, NOT A ZERO ROW ────────────────────────────────

func TestLaneStatsRollupFailedDayWritesNothing(t *testing.T) {
	w, mock := newLaneStatsRollupWorkerWithMock(t)
	w.WithHorizonDays(1)

	today := denverDate(time.Now(), w.loc)

	laneStatsRollupExpectPlan(mock, sqlmock.NewRows([]string{"day", "computed_at"}))

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO mailing_lane_stats_daily`).
		WillReturnError(errors.New("pq: canceling statement due to statement timeout"))
	// The ONLY thing that follows is the rollback. No compensating INSERT, no
	// zero row, no partial commit: the day is simply absent and the endpoint
	// falls back to the live path.
	mock.ExpectRollback()
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(laneStatsRollupWorkerName, "error", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a failed day must roll back and write nothing: %v", err)
	}
	_ = today
}

// A day that produces no cells still records ONE sentinel row, so the day is
// distinguishable from "never computed" and is not rescanned forever. The
// sentinel is written by the SAME upsert, inside the same tx.
func TestLaneStatsRollupWritesSentinelForEmptyDay(t *testing.T) {
	if !strings.Contains(laneStatsRollupDaySQL, "'"+LaneStatsRollupEmptyISP+"'") {
		t.Fatalf("the day statement must emit the %q sentinel for an empty day", LaneStatsRollupEmptyISP)
	}
	if !strings.Contains(laneStatsRollupDaySQL, "WHERE NOT EXISTS (SELECT 1 FROM agg)") {
		t.Fatal("the sentinel must be guarded so it NEVER lands beside real cells")
	}
	// Freshness treats any row, sentinel included, as "this day is computed".
	if !strings.Contains(laneStatsRollupFreshnessSQL, "MAX(computed_at)") {
		t.Fatal("freshness must read MAX(computed_at) per day")
	}
}

// ── context cancellation ────────────────────────────────────────────────────

func TestLaneStatsRollupHonorsContextCancellation(t *testing.T) {
	w, mock := newLaneStatsRollupWorkerWithMock(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// A cancelled context must not even attempt a lock or a heartbeat.
	w.tick(ctx)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a cancelled tick must do nothing: %v", err)
	}
}

// ── bounds ──────────────────────────────────────────────────────────────────

func TestLaneStatsRollupBounds(t *testing.T) {
	w, _ := newLaneStatsRollupWorkerWithMock(t)
	if w.conc < 1 || w.conc > 3 {
		t.Fatalf("day-scan concurrency must stay in [1,3] — mailing_tracking_events is the bottleneck, got %d", w.conc)
	}
	if w.horizon != laneStatsRollupHorizonDays {
		t.Fatalf("horizon must default to the endpoint's max window, got %d", w.horizon)
	}
	if w.budget >= w.interval {
		t.Fatalf("the tick budget (%s) must be inside the lease/interval (%s) so ticks never overlap",
			w.budget, w.interval)
	}
	// The horizon override is clamped: nothing may ask for more than the
	// endpoint can request.
	if got := w.WithHorizonDays(999).horizon; got != laneStatsRollupHorizonDays {
		t.Fatalf("horizon override must clamp to %d, got %d", laneStatsRollupHorizonDays, got)
	}
}
