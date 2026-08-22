package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/analytics"
)

func resetGridState() {
	gridStateMu.Lock()
	gridState = SegmentGridWorkerState{}
	gridStateMu.Unlock()
}

// ---------------------------------------------------------------------------
// parseGridConditions — fail-closed contract (332b3bfe)
// ---------------------------------------------------------------------------

func TestParseGridConditions_60DOpenersWithNeverClickers(t *testing.T) {
	// The live shape carried by all 16 '<CODE> 60D Openers'.
	raw := `[{"field":"email_opened","operator":"in_last_days","value":"60"},
	         {"field":"sending_domain","operator":"equals","value":"em.discountblog.com"},
	         {"field":"exclude_never_clickers","operator":"gte","value":"15"}]`
	spec, err := parseGridConditions("DB 60D Openers", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec.Event != "open" || spec.WindowDays != 60 || spec.BrandApex != "discountblog.com" || spec.NeverClickersGte != 15 {
		t.Fatalf("bad spec: %+v", spec)
	}
}

func TestParseGridConditions_V2ObjectFormAndSeeds(t *testing.T) {
	raw := `{"conditions":[
		{"field":"email_clicked","operator":"within_last","value":30},
		{"field":"sending_domain","operator":"is","value":"em.quizfiesta.com"},
		{"field":"exclude_list_pattern","operator":"contains","value":"seed"}]}`
	spec, err := parseGridConditions("QF 30D Clickers", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec.Event != "click" || spec.WindowDays != 30 || spec.BrandApex != "quizfiesta.com" || !spec.ExcludeSeeds {
		t.Fatalf("bad spec: %+v", spec)
	}
}

// TestParseGridConditions_FailClosed pins the rule that an inexpressible
// condition refuses the WHOLE segment — building a broader audience than the
// segment's own definition is the pre-332b3bfe defect.
func TestParseGridConditions_FailClosed(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"unknown field", `[{"field":"email_opened","operator":"in_last_days","value":"30"},
			{"field":"sending_domain","operator":"equals","value":"em.x.com"},
			{"field":"engagement_score","operator":"gt","value":"50"}]`},
		{"known field unknown operator", `[{"field":"email_opened","operator":"equals","value":"30"},
			{"field":"sending_domain","operator":"equals","value":"em.x.com"}]`},
		{"no sending_domain", `[{"field":"email_opened","operator":"in_last_days","value":"30"}]`},
		{"no event", `[{"field":"sending_domain","operator":"equals","value":"em.x.com"}]`},
		{"lake spec blob", `{"lake_spec":{"event":"open","window_days":30,"scope":"brand","brand_apex":"x.com"}}`},
		{"empty", `[]`},
		{"isp email condition", `[{"field":"email_opened","operator":"in_last_days","value":"30"},
			{"field":"sending_domain","operator":"equals","value":"em.x.com"},
			{"field":"email","operator":"contains","value":"@gmail.com"}]`},
	}
	for _, c := range cases {
		if _, err := parseGridConditions("X 30D Openers", c.raw); err == nil {
			t.Errorf("%s: expected fail-closed error, got none", c.name)
		}
	}
}

// ---------------------------------------------------------------------------
// Directional delta guard — pinned to the real 2026-08-20 numbers (d8c4067c)
// ---------------------------------------------------------------------------

func TestGridDeltaGuard_GrowthCorrectionIsNeverSkipped(t *testing.T) {
	// DB 30D Clickers was frozen at 1,534 vs a true 2,846 (+85.5%) by the old
	// SYMMETRIC guard. The directional guard must let growth through.
	if gridDeltaGuardTrips(1534, 2846, segmentGridMaxDropPct, segmentGridMaxGrowthPct, segmentGridMinGuardSize) {
		t.Fatal("growth correction 1534→2846 must NOT trip the guard (the 08-20 freeze)")
	}
}

func TestGridDeltaGuard_BigDropTrips(t *testing.T) {
	// A drop past 50% is the genuine lake-gap signal.
	if !gridDeltaGuardTrips(46484, 10000, segmentGridMaxDropPct, segmentGridMaxGrowthPct, segmentGridMinGuardSize) {
		t.Fatal("drop 46484→10000 (-78%) must trip the guard")
	}
}

func TestGridDeltaGuard_GrossInflationTrips(t *testing.T) {
	if !gridDeltaGuardTrips(1534, 12000, segmentGridMaxDropPct, segmentGridMaxGrowthPct, segmentGridMinGuardSize) {
		t.Fatal("growth 1534→12000 (+682%) must trip the growth ceiling")
	}
}

func TestGridDeltaGuard_TinySegmentsExempt(t *testing.T) {
	// 8 → 146 is +1725% and means nothing on a warmup brand.
	if gridDeltaGuardTrips(8, 146, segmentGridMaxDropPct, segmentGridMaxGrowthPct, segmentGridMinGuardSize) {
		t.Fatal("segments under the min guard size must be exempt")
	}
	if gridDeltaGuardTrips(0, 5000, segmentGridMaxDropPct, segmentGridMaxGrowthPct, segmentGridMinGuardSize) {
		t.Fatal("prior 0 must never trip")
	}
}

func TestGridDeltaGuard_ModerateDropAllowed(t *testing.T) {
	if gridDeltaGuardTrips(10000, 7000, segmentGridMaxDropPct, segmentGridMaxGrowthPct, segmentGridMinGuardSize) {
		t.Fatal("-30% is inside the drop limit and must not trip")
	}
}

// ---------------------------------------------------------------------------
// Daily scheduling — Denver-aware once-per-day gating
// ---------------------------------------------------------------------------

func TestDailyDue(t *testing.T) {
	w := &SegmentGridWorker{}
	day := func(h, m int) time.Time {
		return time.Date(2026, 8, 21, h, m, 0, 0, time.UTC)
	}

	w.nowFn = func() time.Time { return day(5, 0) } // before 05:30 target
	if w.dailyDue() {
		t.Fatal("must not be due before the daily target")
	}

	w.nowFn = func() time.Time { return day(5, 31) }
	if !w.dailyDue() {
		t.Fatal("must be due after the daily target")
	}

	w.lastDailyDay = "2026-08-21" // completed today
	if w.dailyDue() {
		t.Fatal("must not re-run after completing today's pass")
	}

	w.lastDailyDay = "2026-08-20" // yesterday's completion, failed attempt 10m ago
	w.lastDailyAttempt = day(5, 25)
	w.nowFn = func() time.Time { return day(5, 35) }
	if w.dailyDue() {
		t.Fatal("retry inside the cooldown must be suppressed")
	}
	w.nowFn = func() time.Time { return day(6, 10) } // past cooldown
	if !w.dailyDue() {
		t.Fatal("retry past the cooldown must be allowed")
	}
}

// ---------------------------------------------------------------------------
// NEGATIVE PATH: a non-leader instance runs NOTHING
// ---------------------------------------------------------------------------

func TestTick_NonLeaderRunsNothing(t *testing.T) {
	resetGridState()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// redis nil → PG advisory fallback; the OTHER instance holds the lock.
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(false))

	w := NewSegmentGridWorker(db, nil)
	// Force the daily pass to be DUE so a leadership regression would run it.
	w.nowFn = func() time.Time { return time.Date(2026, 8, 21, 7, 0, 0, 0, time.UTC) }
	w.bucketFn = func(ctx context.Context, event string, windowDays int) ([]analytics.GridPair, error) {
		t.Fatal("non-leader must never query the lake")
		return nil, nil
	}

	w.tick(context.Background())

	st := SegmentGridState()
	if st.Leader {
		t.Fatal("instance must not report leader when the lock is held elsewhere")
	}
	if !st.LastPassAt.IsZero() {
		t.Fatal("non-leader must not run the daily pass (LastPassAt was stamped)")
	}
	// Only the advisory-lock probe may have hit the DB.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("non-leader issued unexpected DB work: %v", err)
	}
}

// ---------------------------------------------------------------------------
// NEGATIVE PATH: the circuit opens after N consecutive failures and the pass
// heartbeats 'degraded' instead of grinding on
// ---------------------------------------------------------------------------

func TestRunDailyPass_CircuitOpensAfterConsecutiveFailures(t *testing.T) {
	resetGridState()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// Five grid segments, all one (open, 30d) bucket, no ledger rows (never
	// built → all need building, prior 0 so no guard interference).
	ids := make([]string, 5)
	segRows := sqlmock.NewRows([]string{"id", "name", "conditions", "organization_id"})
	for i := range ids {
		ids[i] = uuid.New().String()
		conds := fmt.Sprintf(`[{"field":"email_opened","operator":"in_last_days","value":"30"},{"field":"sending_domain","operator":"equals","value":"em.brand%d.com"}]`, i)
		segRows.AddRow(ids[i], fmt.Sprintf("B%c 30D Openers", 'A'+i), conds, uuid.New().String())
	}
	mock.ExpectQuery(`FROM mailing_segments`).WillReturnRows(segRows)
	mock.ExpectQuery(`FROM mailing_segment_build_ledger`).
		WillReturnRows(sqlmock.NewRows([]string{"segment_id", "count", "built", "status", "updated"}))

	// Per failed segment: mark-running then the 'error' terminal upsert.
	// Exactly segmentGridCircuitLimit (3) pairs — the 4th/5th segments must
	// never be attempted.
	for i := 0; i < segmentGridCircuitLimit; i++ {
		mock.ExpectExec(`VALUES \(\$1::uuid, \$2, 'running'`).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(`CASE WHEN \$4::text = 'ok'`).
			WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), segmentGridLedgerSource, "error",
				sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}

	// Terminal bookkeeping: heartbeat MUST be 'degraded', run row 'failed'.
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs("segment_grid", "degraded", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_worker_runs`).
		WithArgs("segment_grid", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"failed", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`DELETE FROM mailing_worker_runs`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := NewSegmentGridWorker(db, nil)
	w.readerEnabledFn = func() bool { return true }
	w.bucketFn = func(ctx context.Context, event string, windowDays int) ([]analytics.GridPair, error) {
		pairs := make([]analytics.GridPair, 0, 5)
		for i := 0; i < 5; i++ {
			pairs = append(pairs, analytics.GridPair{
				SubscriberID: uuid.New().String(),
				BrandApex:    fmt.Sprintf("brand%d.com", i),
			})
		}
		return pairs, nil
	}
	w.resolveFn = func(ctx context.Context, resolveIDs []string, seedListIDs []string) (map[string]gridSubscriber, error) {
		out := make(map[string]gridSubscriber, len(resolveIDs))
		for _, id := range resolveIDs {
			out[id] = gridSubscriber{Email: id + "@example.com"}
		}
		return out, nil
	}
	swapCalls := 0
	w.swapFn = func(ctx context.Context, segmentID string, members []gridMember) error {
		swapCalls++
		return errors.New("pq: canceling statement due to statement timeout")
	}

	w.runDailyPass(context.Background())

	if swapCalls != segmentGridCircuitLimit {
		t.Fatalf("circuit must stop the pass after %d consecutive failures; %d builds were attempted",
			segmentGridCircuitLimit, swapCalls)
	}
	st := SegmentGridState()
	if !st.Degraded {
		t.Fatal("worker state must report degraded after the circuit opens")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected DB flow: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Delta-guard skip writes a LEDGER ROW (the no-row skip is the documented
// footgun) and does NOT touch members
// ---------------------------------------------------------------------------

func TestRunDailyPass_DeltaSkipWritesLedgerRowAndNoSwap(t *testing.T) {
	resetGridState()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	segID := uuid.New().String()
	segRows := sqlmock.NewRows([]string{"id", "name", "conditions", "organization_id"}).
		AddRow(segID, "DB 30D Openers",
			`[{"field":"email_opened","operator":"in_last_days","value":"30"},{"field":"sending_domain","operator":"equals","value":"em.discountblog.com"}]`,
			uuid.New().String())
	mock.ExpectQuery(`FROM mailing_segments`).WillReturnRows(segRows)

	// Prior verified build: 46,484 members, 30h ago (stale enough to rebuild).
	mock.ExpectQuery(`FROM mailing_segment_build_ledger`).
		WillReturnRows(sqlmock.NewRows([]string{"segment_id", "count", "built", "status", "updated"}).
			AddRow(segID, 46484, time.Now().UTC().Add(-30*time.Hour), "ok", time.Now().UTC().Add(-30*time.Hour)))

	mock.ExpectExec(`VALUES \(\$1::uuid, \$2, 'running'`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Terminal upsert MUST be 'skipped_delta' — with a row, not silence.
	mock.ExpectExec(`CASE WHEN \$4::text = 'ok'`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), segmentGridLedgerSource, "skipped_delta",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs("segment_grid", "ok", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_worker_runs`).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`DELETE FROM mailing_worker_runs`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := NewSegmentGridWorker(db, nil)
	w.readerEnabledFn = func() bool { return true }
	// New build resolves only 10k members — a -78% drop, the lake-gap shape.
	subIDs := make([]string, 10000)
	for i := range subIDs {
		subIDs[i] = uuid.New().String()
	}
	w.bucketFn = func(ctx context.Context, event string, windowDays int) ([]analytics.GridPair, error) {
		pairs := make([]analytics.GridPair, len(subIDs))
		for i, id := range subIDs {
			pairs[i] = analytics.GridPair{SubscriberID: id, BrandApex: "discountblog.com"}
		}
		return pairs, nil
	}
	w.resolveFn = func(ctx context.Context, resolveIDs []string, seedListIDs []string) (map[string]gridSubscriber, error) {
		out := make(map[string]gridSubscriber, len(resolveIDs))
		for _, id := range resolveIDs {
			out[id] = gridSubscriber{Email: id + "@example.com"}
		}
		return out, nil
	}
	w.swapFn = func(ctx context.Context, segmentID string, members []gridMember) error {
		t.Fatal("delta-guard skip must never swap members")
		return nil
	}

	w.runDailyPass(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("delta skip must write its ledger row: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Resume: segments built ok within the fresh window are skipped (re-fire /
// deploy-bounce safety — no duplicate Athena scans, no member churn)
// ---------------------------------------------------------------------------

func TestRunDailyPass_FreshSegmentsAreNotRebuilt(t *testing.T) {
	resetGridState()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	segID := uuid.New().String()
	mock.ExpectQuery(`FROM mailing_segments`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "conditions", "organization_id"}).
			AddRow(segID, "DB 30D Openers",
				`[{"field":"email_opened","operator":"in_last_days","value":"30"},{"field":"sending_domain","operator":"equals","value":"em.discountblog.com"}]`,
				uuid.New().String()))
	// Built ok 2h ago (e.g. by the pre-bounce pass or the Fargate job).
	mock.ExpectQuery(`FROM mailing_segment_build_ledger`).
		WillReturnRows(sqlmock.NewRows([]string{"segment_id", "count", "built", "status", "updated"}).
			AddRow(segID, 46484, time.Now().UTC().Add(-2*time.Hour), "ok", time.Now().UTC().Add(-2*time.Hour)))

	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs("segment_grid", "ok", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_worker_runs`).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`DELETE FROM mailing_worker_runs`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := NewSegmentGridWorker(db, nil)
	w.readerEnabledFn = func() bool { return true }
	w.bucketFn = func(ctx context.Context, event string, windowDays int) ([]analytics.GridPair, error) {
		t.Fatal("a fresh segment must not trigger a lake scan")
		return nil, nil
	}
	w.swapFn = func(ctx context.Context, segmentID string, members []gridMember) error {
		t.Fatal("a fresh segment must not be swapped")
		return nil
	}

	w.runDailyPass(context.Background())

	if w.lastDailyDay == "" {
		t.Fatal("an all-fresh pass must count as today's completed pass")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected DB flow: %v", err)
	}
}
