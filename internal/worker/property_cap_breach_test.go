package worker

// PERMANENT NEGATIVE FIXTURES (I-11) for the cap-breach detector + automatic
// shutoff. These exist because this repo has shipped gates that no-oped (Gate F,
// the is_machine_* columns): a gate is only real if a test proves it FIRES on
// the bad input and STAYS SILENT on the good one.
//
// The whole decision lives in evalCapBreach, a pure integer function, so these
// are proofs of the production decision path — not of a mirrored predicate.

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func intp(v int) *int { return &v }

// ── 1. The threshold itself ────────────────────────────────────────────────

func TestEvalCapBreachThresholdBoundaries(t *testing.T) {
	const pct, minExcess = defaultCapBreachPct, 0 // floor off: isolate the RATIO

	cases := []struct {
		name     string
		intended int
		actual   int
		want     bool
	}{
		{"1.4x does NOT fire", 1000, 1400, false},
		{"1.49x does NOT fire", 1000, 1490, false},
		{"exactly 1.5x does NOT fire (must be MORE than 50% over)", 1000, 1500, false},
		{"1.5x + 1 record FIRES", 1000, 1501, true},
		{"1.6x FIRES", 1000, 1600, true},
		{"3x FIRES", 1000, 3000, true},
		{"exactly at cap does NOT fire", 1000, 1000, false},
		{"under cap does NOT fire", 1000, 999, false},
		{"small cell: exactly 1.5x does NOT fire", 200, 300, false},
		{"small cell: 1.5x + 1 FIRES", 200, 301, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := evalCapBreach(capBreachCandidate{
				Brand: "db", ISP: "yahoo",
				Intended: intp(tc.intended), Actual: intp(tc.actual),
			}, pct, minExcess)
			if v.Breach != tc.want {
				t.Fatalf("intended=%d actual=%d: breach=%v want %v (%s)",
					tc.intended, tc.actual, v.Breach, tc.want, v.Reason)
			}
		})
	}
}

// TestEvalCapBreachRatioIsIntegerExact guards against a float reimplementation
// reintroducing an off-by-epsilon at the boundary for every plausible budget.
func TestEvalCapBreachRatioIsIntegerExact(t *testing.T) {
	for intended := 2; intended <= 4000; intended += 7 {
		exact := intended * 3 / 2 // floor(1.5 * intended)
		for _, actual := range []int{exact, exact + 1} {
			v := evalCapBreach(capBreachCandidate{
				Intended: intp(intended), Actual: intp(actual),
			}, defaultCapBreachPct, 0)
			// actual*100 > intended*150  ⇔  actual*2 > intended*3
			want := actual*2 > intended*3
			if v.Breach != want {
				t.Fatalf("intended=%d actual=%d: breach=%v want %v", intended, actual, v.Breach, want)
			}
		}
	}
}

// ── 2. The gate that must NEVER fire: an UNGOVERNED lane ────────────────────

// TestEvalCapBreachNeverTripsUngovernedLane is the headline negative fixture.
// A (brand × ISP) with NO ledger row has NO intention. Reading a missing row as
// an intention of zero would trip a shutoff on every ungoverned lane in the
// estate (docs/JAOS/drip-lanes.md §2.1: "A dataset with no row is not capped at
// 0"). No volume, however large, may trip it.
func TestEvalCapBreachNeverTripsUngovernedLane(t *testing.T) {
	for _, actual := range []int{0, 1, 100, 10_000, 5_000_000} {
		v := evalCapBreach(capBreachCandidate{
			Brand: "tot", ISP: "gmail",
			Intended: nil, // NO ledger row
			Actual:   intp(actual),
		}, defaultCapBreachPct, defaultCapBreachMinExcess)
		if v.Breach {
			t.Fatalf("ungoverned lane tripped at actual=%d — a missing row was read as an intention: %s", actual, v.Reason)
		}
		if !strings.Contains(v.Reason, "ungoverned") {
			t.Fatalf("ungoverned skip must say so, got %q", v.Reason)
		}
	}
}

// TestCapBreachSQLCannotManufactureAGovernedCell pins the STRUCTURAL half of
// the same guarantee: the ledger drives the query and the counters are LEFT
// JOINed onto it, so no counter row can put an ungoverned cell in the result
// set. An INNER JOIN, or counters as the driving table, breaks this.
func TestCapBreachSQLCannotManufactureAGovernedCell(t *testing.T) {
	if !regexp.MustCompile(`FROM\s+partner_drip_brand_budgets\s+b`).MatchString(capBreachCandidatesSQL) {
		t.Fatal("the LEDGER must drive the candidate query")
	}
	if !regexp.MustCompile(`LEFT\s+JOIN\s+property_intro_counters`).MatchString(capBreachCandidatesSQL) {
		t.Fatal("counters must be LEFT JOINed onto the ledger, never the driving table")
	}
	if regexp.MustCompile(`(?i)\bINNER\s+JOIN\b|FROM\s+property_intro_counters`).MatchString(capBreachCandidatesSQL) {
		t.Fatal("counters must not drive or inner-join the candidate query")
	}
	// partner_clean_queue is ~11.2M rows and the primary is IO-starved: the
	// detector must never query it.
	if strings.Contains(capBreachCandidatesSQL, "partner_clean_queue") {
		t.Fatal("cap-breach detector must not touch partner_clean_queue — it rides property_intro_counters")
	}
	// The same-day dedup key must be in the load.
	for _, frag := range []string{"property_hold_intervals", "changed_by = $2", "held_from >= $3"} {
		if !strings.Contains(capBreachCandidatesSQL, frag) {
			t.Fatalf("candidate SQL missing same-day dedup fragment %q", frag)
		}
	}
}

// ── 3. The other never-fire gates ──────────────────────────────────────────

func TestEvalCapBreachSkipGates(t *testing.T) {
	cases := []struct {
		name   string
		cand   capBreachCandidate
		reason string
	}{
		{
			// 1.5 × 0 = 0 would make one stray record a "breach", and a zero
			// cell is already hard-suppressed by applyBrandIntroBudgets.
			"daily_budget 0 is not an intention to measure a ratio against",
			capBreachCandidate{Intended: intp(0), Actual: intp(9999)},
			"daily_budget <= 0",
		},
		{
			// The rollup writes explicit zeros for the whole grid, so a missing
			// counter is ABSENT, never 0 — and absent input is never judged.
			"absent counter cell",
			capBreachCandidate{Intended: intp(100), Actual: nil},
			"ABSENT, not zero",
		},
		{
			"already held",
			capBreachCandidate{Intended: intp(100), Actual: intp(9999), Hold: true},
			"already held",
		},
		{
			// Not fighting a human who un-holds a still-over-cap cell.
			"already auto-tripped this Denver day",
			capBreachCandidate{Intended: intp(100), Actual: intp(9999), AutoTrippedToday: true},
			"already auto-tripped",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := evalCapBreach(tc.cand, defaultCapBreachPct, defaultCapBreachMinExcess)
			if v.Breach {
				t.Fatalf("must not breach: %s", v.Reason)
			}
			if !strings.Contains(v.Reason, tc.reason) {
				t.Fatalf("reason %q must contain %q", v.Reason, tc.reason)
			}
		})
	}
}

// ── 4. Normal per-wave overshoot vs a genuine runaway ──────────────────────

// applyBrandIntroBudgets clamps each wave to (budget − spend-so-far), so a
// single wave cannot exceed the budget; real overshoot comes from concurrent
// waves reading the same spend and from mailed_at stamp lag — bounded by about
// one per-wave per-ISP cap (PerISPCapPerWave tops out at 100). The absolute
// floor is what keeps that from tripping a shutoff on a tiny cell.
func TestEvalCapBreachAbsoluteFloorAbsorbsPerWaveOvershoot(t *testing.T) {
	const pct, minExcess = defaultCapBreachPct, defaultCapBreachMinExcess // 150, 100

	// 10 over a budget of 10 is 2.0× by ratio — but it is 10 records, i.e.
	// ordinary per-wave granularity. Not a runaway.
	if v := evalCapBreach(capBreachCandidate{Intended: intp(10), Actual: intp(20)}, pct, minExcess); v.Breach {
		t.Fatalf("tiny-cell per-wave overshoot must not trip: %s", v.Reason)
	}
	// Exactly one max per-wave cap over: still tolerated (strictly greater).
	if v := evalCapBreach(capBreachCandidate{Intended: intp(100), Actual: intp(200)}, pct, minExcess); v.Breach {
		t.Fatalf("excess == min_excess must not trip: %s", v.Reason)
	}
	// One record past the tolerance AND past the ratio: runaway.
	if v := evalCapBreach(capBreachCandidate{Intended: intp(100), Actual: intp(201)}, pct, minExcess); !v.Breach {
		t.Fatalf("excess > min_excess and > 1.5x must trip: %s", v.Reason)
	}
	// Large budget: the ratio binds, not the floor. 1.4× of 5000 is 2000 over
	// the floor in absolute terms, and must still NOT trip.
	if v := evalCapBreach(capBreachCandidate{Intended: intp(5000), Actual: intp(7000)}, pct, minExcess); v.Breach {
		t.Fatalf("1.4x of a large budget must not trip however big the absolute excess: %s", v.Reason)
	}
	if v := evalCapBreach(capBreachCandidate{Intended: intp(5000), Actual: intp(7501)}, pct, minExcess); !v.Breach {
		t.Fatalf("1.5x+1 of a large budget must trip: %s", v.Reason)
	}
}

// ── 5. Configuration ───────────────────────────────────────────────────────

func TestCapBreachThresholdCannotBeTunedBelowTheCap(t *testing.T) {
	t.Setenv("PROPERTY_CAP_BREACH_PCT", "80")
	if got := CapBreachThresholdPct(); got != defaultCapBreachPct {
		t.Fatalf("a sub-100%% threshold must be refused (it would trip lanes at or under intention), got %d", got)
	}
	t.Setenv("PROPERTY_CAP_BREACH_PCT", "200")
	if got := CapBreachThresholdPct(); got != 200 {
		t.Fatalf("valid override ignored, got %d", got)
	}
	t.Setenv("PROPERTY_CAP_BREACH_MIN_EXCESS", "-5")
	if got := CapBreachMinExcess(); got != defaultCapBreachMinExcess {
		t.Fatalf("negative min_excess must be refused, got %d", got)
	}
}

func TestCapBreachDefaultsMatchTheOperatorRule(t *testing.T) {
	if defaultCapBreachPct != 150 {
		t.Fatalf("operator rule is >50%% over intention: default must be 150, got %d", defaultCapBreachPct)
	}
	if CapBreachDisabled() {
		t.Fatal("detector must default to ARMED (kill switch is fail-OPEN, opt-in)")
	}
	if CapBreachDetectOnly() {
		t.Fatal("detect-only must default OFF")
	}
}

// ── 6. Pass-level behaviour (sqlmock) ──────────────────────────────────────

func capBreachRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"brand", "isp", "daily_budget", "hold", "lock_version", "introduced", "auto_tripped_today"})
}

func TestCapBreachDisabledWritesNothing(t *testing.T) {
	t.Setenv("PROPERTY_CAP_BREACH_SHUTOFF_DISABLED", "1")
	w, mock := newRollupWorkerWithMock(t)

	// The ONLY statement allowed is the heartbeat saying it is off.
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(capBreachWorkerName, "disabled", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.RunCapBreachDetector(context.Background(), denverDate(time.Now(), w.loc))

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("disabled detector must be heartbeat-only: %v", err)
	}
}

func TestCapBreachDetectOnlyHoldsNothing(t *testing.T) {
	t.Setenv("PROPERTY_CAP_BREACH_DETECT_ONLY", "1")
	w, mock := newRollupWorkerWithMock(t)

	mock.ExpectQuery(`FROM partner_drip_brand_budgets b`).
		WillReturnRows(capBreachRows().AddRow("db", "yahoo", 400, false, int64(7), 900, false))
	// No BEGIN, no UPDATE — only the heartbeat.
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(capBreachWorkerName, "detect-only", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.RunCapBreachDetector(context.Background(), denverDate(time.Now(), w.loc))

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("detect-only must write nothing but a heartbeat: %v", err)
	}
}

func TestCapBreachEmptyLedgerReportsGovernsNothing(t *testing.T) {
	w, mock := newRollupWorkerWithMock(t)

	mock.ExpectQuery(`FROM partner_drip_brand_budgets b`).WillReturnRows(capBreachRows())
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(capBreachWorkerName, "ok", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.RunCapBreachDetector(context.Background(), denverDate(time.Now(), w.loc))

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("empty ledger pass: %v", err)
	}
}

// TestCapBreachTripsAndOnlyTouchesHold pins the shutoff transaction shape:
// FOR UPDATE → same-day guard → audit interval → hold=TRUE with lock_version
// CAS. daily_budget must NEVER appear in the UPDATE's SET list.
func TestCapBreachTripsAndOnlyTouchesHold(t *testing.T) {
	w, mock := newRollupWorkerWithMock(t)

	mock.ExpectQuery(`FROM partner_drip_brand_budgets b`).
		WillReturnRows(capBreachRows().
			// governed, 1.4x → must NOT be touched
			AddRow("ht", "aol", 1000, false, int64(2), 1400, false).
			// governed, 2.25x and excess 500 → MUST be held
			AddRow("db", "yahoo", 400, false, int64(7), 900, false))

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL lock_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT daily_budget, hold, lock_version\s+FROM partner_drip_brand_budgets\s+WHERE brand = \$1 AND isp = \$2 FOR UPDATE`).
		WithArgs("db", "yahoo").
		WillReturnRows(sqlmock.NewRows([]string{"daily_budget", "hold", "lock_version"}).AddRow(400, false, int64(7)))
	mock.ExpectQuery(`SELECT EXISTS \(\s+SELECT 1 FROM property_hold_intervals`).
		WithArgs("db", "yahoo", CapBreachActor, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec(`INSERT INTO property_hold_intervals`).
		WithArgs("db", "yahoo", sqlmock.AnyArg(), CapBreachActor).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE partner_drip_brand_budgets\s+SET hold = TRUE`).
		WithArgs("db", "yahoo", CapBreachActor, int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(capBreachWorkerName, "ok", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.RunCapBreachDetector(context.Background(), denverDate(time.Now(), w.loc))

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("trip pass shape (exactly ONE cell held, 1.4x untouched): %v", err)
	}
}

// TestCapBreachHoldUpdateNeverWritesBudget is a source-level guard: the one
// autonomous UPDATE must not be able to destroy the operator's intended value.
func TestCapBreachHoldUpdateNeverWritesBudget(t *testing.T) {
	src := mustReadSource(t, "property_cap_breach.go")
	i := strings.Index(src, "UPDATE partner_drip_brand_budgets\n\t\tSET hold = TRUE")
	if i < 0 {
		t.Fatal("the hold UPDATE moved — re-pin this guard")
	}
	stmt := src[i:]
	if j := strings.Index(stmt, "`"); j > 0 {
		stmt = stmt[:j]
	}
	if strings.Contains(stmt, "daily_budget") {
		t.Fatalf("the autonomous shutoff must never write daily_budget:\n%s", stmt)
	}
}

// TestCapBreachSecondPassIsANoOp is the double-fire / restart-safety proof: the
// scheduler re-fires every 10 minutes and on every ECS bounce. The same-day
// interval written by the first trip makes every later pass inert — including
// after a human un-holds (hold=false, auto_tripped_today=true), which is the
// "do not fight the operator" case.
func TestCapBreachSecondPassIsANoOp(t *testing.T) {
	for _, tc := range []struct {
		name string
		hold bool
	}{
		{"still held by the detector", true},
		{"human un-held it, still over cap", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := newRollupWorkerWithMock(t)

			mock.ExpectQuery(`FROM partner_drip_brand_budgets b`).
				WillReturnRows(capBreachRows().
					AddRow("db", "yahoo", 400, tc.hold, int64(8), 900, true)) // auto_tripped_today
			// No BEGIN. No UPDATE. Just the beat.
			mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
				WithArgs(capBreachWorkerName, "ok", sqlmock.AnyArg(), sqlmock.AnyArg()).
				WillReturnResult(sqlmock.NewResult(0, 1))

			w.RunCapBreachDetector(context.Background(), denverDate(time.Now(), w.loc))

			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("re-fire must be inert: %v", err)
			}
		})
	}
}

// TestCapBreachRefusesMassTrip: many cells breaching at once is a systemic
// reading (counter regression, budget wipe), not a lane runaway. Holding the
// estate on a bad reading is exactly what JAOS core §1.8 forbids.
func TestCapBreachRefusesMassTrip(t *testing.T) {
	w, mock := newRollupWorkerWithMock(t)

	rows := capBreachRows()
	for i := 0; i <= capBreachMaxTripsPerPass; i++ {
		rows = rows.AddRow("db", "isp"+string(rune('a'+i)), 400, false, int64(1), 9000, false)
	}
	mock.ExpectQuery(`FROM partner_drip_brand_budgets b`).WillReturnRows(rows)
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(capBreachWorkerName, "error", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.RunCapBreachDetector(context.Background(), denverDate(time.Now(), w.loc))

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mass breach must hold NOTHING and report error: %v", err)
	}
}

// TestCapBreachRefusesToHoldWithoutAnAuditRow: if the interval INSERT affects
// no row (an open interval already exists), the whole trip rolls back rather
// than holding a lane with no record of why.
func TestCapBreachRefusesToHoldWithoutAnAuditRow(t *testing.T) {
	w, mock := newRollupWorkerWithMock(t)

	mock.ExpectQuery(`FROM partner_drip_brand_budgets b`).
		WillReturnRows(capBreachRows().AddRow("db", "yahoo", 400, false, int64(7), 900, false))
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL lock_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FOR UPDATE`).WithArgs("db", "yahoo").
		WillReturnRows(sqlmock.NewRows([]string{"daily_budget", "hold", "lock_version"}).AddRow(400, false, int64(7)))
	mock.ExpectQuery(`SELECT EXISTS`).WithArgs("db", "yahoo", CapBreachActor, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec(`INSERT INTO property_hold_intervals`).
		WillReturnResult(sqlmock.NewResult(0, 0)) // no audit row written
	mock.ExpectRollback()
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(capBreachWorkerName, "error", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.RunCapBreachDetector(context.Background(), denverDate(time.Now(), w.loc))

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no audit row must mean no hold: %v", err)
	}
}

// TestCapBreachBlockedNeverPresentsAsNoBreaches: the counters are the ONLY
// input. If they were not produced, the detector must say BLOCKED — the
// "building forever" / silent-zero failure mode this repo keeps re-learning.
func TestCapBreachBlockedNeverPresentsAsNoBreaches(t *testing.T) {
	w, mock := newRollupWorkerWithMock(t)

	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(capBreachWorkerName, "blocked", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.capBreachReportBlocked(context.Background(), "counter pass blocked: idx_pcq_intro_rollup missing or invalid")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("blocked report: %v", err)
	}
}
