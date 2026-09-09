package dripsupply

import (
	"bytes"
	"context"
	"database/sql"
	"log"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// R1 — reseed(): the lane balance follows its dispatch contract
// -----------------------------------------------------------------------------
//
// Measured 2026-09-09: the nightly stepper raised the yahoo_family lane contract
// from 343,829 to 420,157, drip_lane_balance kept the superseded numbers, and
// `unfilled` drained to 218 on yahoo / 589 on aol while Reserve returned
// lane_demand with 0 committed. Commit rate fell ~12,600/h -> 3,168/h.

func TestReseed_EnforcesTheLaneInvariant(t *testing.T) {
	cases := []struct {
		name            string
		prev            LaneBalance
		contractDesired int
		wantDesired     int
		wantUnfilled    int
		wantDrift       int // laneDrift(prev): positive = the lane was SHORT
	}{
		// ---- the production table, 2026-09-09 13:03Z, by its real numbers ----
		// Five of six rows had drifted. `wantUnfilled` is the "should be"
		// column; wantDrift is the "short" column.
		{
			name:            "prod yahoo",
			prev:            LaneBalance{Desired: 106_399, Reserved: 231, Committed: 54_500, Unfilled: 3_324},
			contractDesired: 106_399,
			wantDesired:     106_399, wantUnfilled: 51_668, wantDrift: 48_344,
		},
		{
			name:            "prod att",
			prev:            LaneBalance{Desired: 123_291, Reserved: 835, Committed: 33_732, Unfilled: 12_201},
			contractDesired: 123_291,
			wantDesired:     123_291, wantUnfilled: 88_724, wantDrift: 76_523,
		},
		{
			name:            "prod sbcglobal",
			prev:            LaneBalance{Desired: 56_092, Reserved: 764, Committed: 15_718, Unfilled: 5_470},
			contractDesired: 56_092,
			wantDesired:     56_092, wantUnfilled: 39_610, wantDrift: 34_140,
		},
		{
			name:            "prod aol",
			prev:            LaneBalance{Desired: 83_134, Reserved: 74, Committed: 42_368, Unfilled: 4_769},
			contractDesired: 83_134,
			wantDesired:     83_134, wantUnfilled: 40_692, wantDrift: 35_923,
		},
		{
			name:            "prod cox",
			prev:            LaneBalance{Desired: 17_794, Reserved: 275, Committed: 4_301, Unfilled: 1_199},
			contractDesired: 17_794,
			wantDesired:     17_794, wantUnfilled: 13_218, wantDrift: 12_019,
		},
		{
			// The one clean row. It must come back byte-identical: a row that
			// already satisfies the invariant is a NO-OP, or every tick rewrites
			// the whole estate and "changed" stops meaning anything.
			name:            "prod comcast is already correct and must be untouched",
			prev:            LaneBalance{Desired: 33_447, Reserved: 518, Committed: 16_449, Unfilled: 16_480},
			contractDesired: 33_447,
			wantDesired:     33_447, wantUnfilled: 16_480, wantDrift: 0,
		},

		// ---- contract movement, with and without drift ------------------------
		{
			// A contract raise on a CLEAN row: 86,036 - 1,200 - 84,618 = 218, so
			// nothing had drifted; the raise alone moves the allowance.
			name:            "contract raised on a clean row",
			prev:            LaneBalance{Desired: 86_036, Reserved: 1_200, Committed: 84_618, Unfilled: 218},
			contractDesired: 106_399,
			wantDesired:     106_399, wantUnfilled: 20_581, wantDrift: 0,
		},
		{
			// BOTH in one pass: the contract stepped up 20,363 AND the row was
			// 48,344 short. One reconciliation must land on the contract's
			// number, not on either correction alone.
			name:            "a contract raise and a drift correction in the SAME pass",
			prev:            LaneBalance{Desired: 106_399, Reserved: 231, Committed: 54_500, Unfilled: 3_324},
			contractDesired: 126_762,
			wantDesired:     126_762, wantUnfilled: 72_031, wantDrift: 48_344,
		},
		{
			name:            "contract unchanged on a clean row is a no-op",
			prev:            LaneBalance{Desired: 86_036, Reserved: 1_200, Committed: 84_618, Unfilled: 218},
			contractDesired: 86_036,
			wantDesired:     86_036, wantUnfilled: 218, wantDrift: 0,
		},

		// ---- bounds -----------------------------------------------------------
		{
			// R3: reserved + committed exceeds desired (a contract lowered below
			// what is already spent). unfilled floors at 0 and NEVER goes
			// negative; the ledger keeps what it already spent.
			name:            "reserved + committed above desired floors at zero",
			prev:            LaneBalance{Desired: 86_036, Reserved: 1_200, Committed: 84_618, Unfilled: 218},
			contractDesired: 50_000,
			wantDesired:     50_000, wantUnfilled: 0, wantDrift: 0,
		},
		{
			name:            "a zero contract closes the lane",
			prev:            LaneBalance{Desired: 40_000, Reserved: 0, Committed: 28_000, Unfilled: 12_000},
			contractDesired: 0,
			wantDesired:     0, wantUnfilled: 0, wantDrift: 0,
		},
		{
			name:            "brand new day with nothing consumed takes the contract whole",
			prev:            LaneBalance{Desired: 40_000, Unfilled: 40_000},
			contractDesired: 55_000,
			wantDesired:     55_000, wantUnfilled: 55_000, wantDrift: 0,
		},
		{
			name:            "a negative contract is treated as zero, never as a negative ceiling",
			prev:            LaneBalance{Desired: 1_000, Unfilled: 1_000},
			contractDesired: -5,
			wantDesired:     0, wantUnfilled: 0, wantDrift: 0,
		},
		{
			// SUPERSEDES the earlier delta-form behaviour, stated explicitly.
			// The planner writes its award into `unfilled` (planner.go:2035);
			// the invariant now overwrites that with the contract's line. The
			// plan keeps its own line in drip_daily_plan, read as plan_share.
			name:            "a planner award in unfilled does not survive the invariant",
			prev:            LaneBalance{Desired: 86_036, AwardedFirm: 37_640, Unfilled: 37_640},
			contractDesired: 86_036,
			wantDesired:     86_036, wantUnfilled: 86_036, wantDrift: 48_396,
		},
		{
			// A corrupt negative counter must not INFLATE unfilled above the
			// contract — over-correction is as wrong as under-correction.
			name:            "a negative counter cannot inflate unfilled above the contract",
			prev:            LaneBalance{Desired: 10_000, Reserved: -5_000, Committed: 2_000, Unfilled: 8_000},
			contractDesired: 10_000,
			wantDesired:     10_000, wantUnfilled: 8_000, wantDrift: 5_000,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := c.prev
			got := reseed(c.prev, c.contractDesired)

			if c.prev != before {
				t.Fatalf("reseed mutated its argument:\n got %+v\nwant %+v", c.prev, before)
			}
			if d := laneDrift(before); d != c.wantDrift {
				t.Errorf("laneDrift = %d, want %d", d, c.wantDrift)
			}
			if got.Desired != c.wantDesired {
				t.Errorf("desired = %d, want %d", got.Desired, c.wantDesired)
			}
			if got.Unfilled != c.wantUnfilled {
				t.Errorf("unfilled = %d, want %d", got.Unfilled, c.wantUnfilled)
			}
			if got.Unfilled < 0 {
				t.Errorf("unfilled = %d — a negative unfilled reads as unbounded the moment anything sums it", got.Unfilled)
			}
			// THE INVARIANT, asserted directly on the output. Counters are read
			// clamped, exactly as reseed reads them, so the corrupt-row case
			// below is held to the same identity instead of being excused from
			// it. The identity is allowed to fall short ONLY when the floor
			// bound — a lane genuinely oversubscribed against a lowered
			// contract, where unfilled is 0 and the ledger keeps the overspend.
			spent := 0
			if got.Reserved > 0 {
				spent += got.Reserved
			}
			if got.Committed > 0 {
				spent += got.Committed
			}
			if got.Unfilled != 0 && spent+got.Unfilled != got.Desired {
				t.Errorf("reserved+committed+unfilled = %d, want desired %d", spent+got.Unfilled, got.Desired)
			}
			// Consumed volume and the planner's award are read, never written.
			if got.Reserved != before.Reserved || got.Committed != before.Committed ||
				got.AwardedFirm != before.AwardedFirm || got.AwardedProvisional != before.AwardedProvisional {
				t.Errorf("reseed touched consumption/award counters: got %+v, want them as in %+v", got, before)
			}
		})
	}
}

// TestReseed_IsIdempotent: the reconciliation runs on EVERY tick (~15 s in
// prod), so a second application must be a fixed point. It is by construction —
// reseed derives unfilled from reserved/committed, which it never writes — and
// this pins it, because the previous delta form was NOT a fixed point under a
// changing contract.
func TestReseed_IsIdempotent(t *testing.T) {
	for _, prev := range []LaneBalance{
		{Desired: 106_399, Reserved: 231, Committed: 54_500, Unfilled: 3_324},
		{Desired: 86_036, Reserved: 1_200, Committed: 84_618, Unfilled: 218},
		{Desired: 17_794, Reserved: 275, Committed: 4_301, Unfilled: 1_199},
		{Desired: 10, Reserved: 900, Committed: 900, Unfilled: 0},
	} {
		once := reseed(prev, 126_762)
		if d := laneDrift(once); d != 0 && once.Unfilled != 0 {
			t.Fatalf("reseed left %+v still drifting by %d", once, d)
		}
		for i := 0; i < 50; i++ {
			if again := reseed(once, 126_762); again != once {
				t.Fatalf("reseed is not a fixed point for %+v: pass %d = %+v, first = %+v", prev, i, again, once)
			}
		}
	}
}

// -----------------------------------------------------------------------------
// R1 end to end, under the row lock
// -----------------------------------------------------------------------------

func readLaneBalance(t *testing.T, db *sql.DB, day time.Time, lane, isp string) LaneBalance {
	t.Helper()
	var b LaneBalance
	if err := db.QueryRow(`
		SELECT desired, awarded_firm, awarded_provisional, reserved, committed, unfilled
		FROM drip_lane_balance WHERE day = $1::date AND lane = $2 AND isp = $3
	`, dayKey(day), lane, isp).Scan(&b.Desired, &b.AwardedFirm, &b.AwardedProvisional,
		&b.Reserved, &b.Committed, &b.Unfilled); err != nil {
		t.Fatalf("read lane balance: %v", err)
	}
	return b
}

// TestReconcileLaneBalances_FollowsARaisedContract is the prod defect. Before
// this, EnsureDayBalances' ON CONFLICT DO NOTHING (balance.go:253) meant a
// mid-day lane raise changed nothing at all, while a DOMAIN raise took effect on
// the next tick because RefillDomain rewrites contracted/effective every time.
func TestReconcileLaneBalances_FollowsARaisedContract(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	const lane, isp = "wcl_remail", "aol"

	dc, _ := seedDay(t, db, day, 500_000, 86_036)
	// Spend 85,818 of the day, the way a morning of waves would.
	if _, err := db.Exec(`
		UPDATE drip_lane_balance SET unfilled = 218, reserved = 1200, committed = 84618
		WHERE day = $1::date AND lane = $2 AND isp = $3
	`, dayKey(day), lane, isp); err != nil {
		t.Fatalf("spend the lane: %v", err)
	}

	// The nightly stepper supersedes the dispatch contract.
	raised := dispatchContract(lane, 2, map[string]int{isp: 106_399})
	svc := NewService(db, WithClock(midWindow(day)))

	res, err := svc.ReconcileLaneBalances(ctx, day, activeSet(day, []*DomainContract{dc}, []*DispatchContract{raised}))
	if err != nil {
		t.Fatalf("ReconcileLaneBalances: %v", err)
	}
	if res.Changed != 1 || res.Seen != 1 {
		t.Fatalf("reconcile = %+v, want 1 seen / 1 changed", res)
	}
	// The row satisfied its own invariant (86,036 - 1,200 - 84,618 = 218), so a
	// contract step must NOT be reported as drift — otherwise the drift signal
	// is permanent noise and nobody reads it.
	if res.Drifted != 0 {
		t.Fatalf("reconcile reported %d drifted row(s) for a clean contract step", res.Drifted)
	}
	got := readLaneBalance(t, db, day, lane, isp)
	if got.Desired != 106_399 || got.Unfilled != 20_581 {
		t.Fatalf("lane balance = desired %d unfilled %d, want 106399 / 20581", got.Desired, got.Unfilled)
	}
	if got.Reserved != 1_200 || got.Committed != 84_618 {
		t.Fatalf("reconcile moved consumption counters: reserved %d committed %d, want 1200 / 84618", got.Reserved, got.Committed)
	}

	// IDEMPOTENT: the next tick (and the other ECS instance) must be a no-op.
	res2, err := svc.ReconcileLaneBalances(ctx, day, activeSet(day, []*DomainContract{dc}, []*DispatchContract{raised}))
	if err != nil {
		t.Fatalf("second ReconcileLaneBalances: %v", err)
	}
	if res2.Changed != 0 {
		t.Fatalf("second reconcile changed %d row(s) — the lane drifts by one delta per tick", res2.Changed)
	}
	if again := readLaneBalance(t, db, day, lane, isp); again != got {
		t.Fatalf("second reconcile moved the row: %+v -> %+v", got, again)
	}

	// And the raise is REACHABLE: a reservation now draws on it. Before the fix
	// this returned 0 with binding_reason=lane_demand.
	setBalance(t, db, day, "em.historythinking.com", isp, 500_000, 500_000)
	rr := baseReq(day, "wave-after-raise", 20_000)
	rr.LaneContractDesired = 106_399
	got2, err := svc.Reserve(ctx, rr)
	if err != nil {
		t.Fatalf("Reserve after the raise: %v", err)
	}
	if got2.Granted != 20_000 || got2.BindingReason != ReasonRequested {
		t.Fatalf("Reserve after the raise = %+v, want 20000 / %s — the lane is still pinned to the superseded contract", got2, ReasonRequested)
	}
}

// TestReconcileLaneBalances_NeverInventsALane: a lane×ISP with no balance row is
// not open today (EnsureDayBalances skips desired<=0 and excluded ISPs), and the
// reconciler must not create one behind that gate.
func TestReconcileLaneBalances_NeverInventsALane(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	dc := domainContract("em.historythinking.com", 1, map[string]int{"aol": 1000})

	svc := NewService(db, WithClock(midWindow(day)))
	unseeded := dispatchContract("never_seeded", 1, map[string]int{"aol": 5000})
	if _, err := svc.ReconcileLaneBalances(ctx, day, activeSet(day, []*DomainContract{dc}, []*DispatchContract{unseeded})); err != nil {
		t.Fatalf("ReconcileLaneBalances: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM drip_lane_balance`).Scan(&n); err != nil {
		t.Fatalf("count lanes: %v", err)
	}
	if n != 0 {
		t.Fatalf("reconciliation created %d lane row(s) — EnsureDayBalances owns creation", n)
	}
}

// -----------------------------------------------------------------------------
// R2 — a frozen plan may not cap a lane below its own active contract
// -----------------------------------------------------------------------------

func TestPlanTerm_SkipsOnlyASupersededPlan(t *testing.T) {
	cases := []struct {
		name                                            string
		ceiling, consumed, laneAward, laneContractDesir int
		wantLimit                                       int
		wantBounded                                     bool
	}{
		{
			// THE PROD CASE. The 00:05 freeze awarded 37,640 across the lane;
			// the contract now asks 106,399. 77 of plan headroom must not be
			// the lane's day.
			name: "superseded plan does not bind", ceiling: 37_640, consumed: 37_563,
			laneAward: 37_640, laneContractDesir: 106_399, wantLimit: 0, wantBounded: false,
		},
		{
			name: "a plan that matches its contract binds normally", ceiling: 20_000, consumed: 19_923,
			laneAward: 106_399, laneContractDesir: 106_399, wantLimit: 77, wantBounded: true,
		},
		{
			name: "a plan that AWARDED MORE than the contract still binds", ceiling: 20_000, consumed: 5_000,
			laneAward: 120_000, laneContractDesir: 106_399, wantLimit: 15_000, wantBounded: true,
		},
		{
			// No contract number (the follow-up path, or any caller that does
			// not set the field): behave exactly as before the change.
			name: "unknown contract leaves the plan binding", ceiling: 20_000, consumed: 19_923,
			laneAward: 37_640, laneContractDesir: 0, wantLimit: 77, wantBounded: true,
		},
		{
			name: "an exhausted but current plan binds at zero, it is not bypassed", ceiling: 500, consumed: 900,
			laneAward: 106_399, laneContractDesir: 106_399, wantLimit: 0, wantBounded: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			limit, bounded := planTerm(c.ceiling, c.consumed, c.laneAward, c.laneContractDesir)
			if limit != c.wantLimit || bounded != c.wantBounded {
				t.Fatalf("planTerm = (%d, %v), want (%d, %v)", limit, bounded, c.wantLimit, c.wantBounded)
			}
		})
	}
}

// TestPlanShare_DoesNotCapARaisedLane is R2 end to end through Reserve. It is
// the second half of the prod stall: once the lane balance was repaired by hand
// the binding term moved straight to plan_share and the lane stayed at 3,168/h.
func TestPlanShare_DoesNotCapARaisedLane(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	const lane, isp = "wcl_remail", "aol"
	const mine, sibling = "em.historythinking.com", "em.discountblog.com"

	if _, err := db.Exec(DailyPlanDDL); err != nil {
		t.Fatalf("plan ddl: %v", err)
	}
	seedDay(t, db, day, 500_000, 106_399)
	setBalance(t, db, day, mine, isp, 500_000, 500_000)

	// The 00:05 freeze, against the SUPERSEDED contract: 37,640 across the lane,
	// of which this domain holds 77 of unused headroom.
	insertPlanRow(t, db, "drip_daily_plan", day, lane, isp, mine, 77, 0, 0)
	insertPlanRow(t, db, "drip_daily_plan", day, lane, isp, sibling, 37_563, 0, 0)

	svc := NewService(db, WithClock(midWindow(day)), WithPlanReader(PlanStore{}))

	// NEGATIVE CONTROL FIRST: with no contract number the stale plan still binds
	// at 77. If this ever grants more, the test below proves nothing.
	stale := baseReq(day, "wave-stale", 20_000)
	stale.LaneContractDesired = 0
	if res, err := svc.Reserve(ctx, stale); err != nil {
		t.Fatalf("Reserve (control): %v", err)
	} else if res.Granted != 77 || res.BindingReason != ReasonPlanShare {
		t.Fatalf("control Reserve = %+v, want 77 / %s — the plan term is not binding at all", res, ReasonPlanShare)
	}

	// SECOND CONTROL: a contract at or below what the plan awarded is NOT
	// superseded, so the plan must keep binding. This is the "do not silently
	// remove the plan term where it is doing real work" half.
	current := baseReq(day, "wave-current", 20_000)
	current.LaneContractDesired = 30_000 // <= the 37,640 the plan awarded
	if res, err := svc.Reserve(ctx, current); err != nil {
		t.Fatalf("Reserve (current contract): %v", err)
	} else if res.BindingReason != ReasonPlanShare {
		t.Fatalf("a NON-superseded plan stopped binding: %+v — plan_share was removed where it was doing real work", res)
	}

	// THE FIX: the contract now asks 106,399 for the lane, far above the 37,640
	// the frozen plan awarded. The plan is superseded and must not cap the lane.
	raised := baseReq(day, "wave-raised", 20_000)
	raised.LaneContractDesired = 106_399
	res, err := svc.Reserve(ctx, raised)
	if err != nil {
		t.Fatalf("Reserve (raised contract): %v", err)
	}
	if res.BindingReason == ReasonPlanShare {
		t.Fatalf("a superseded plan still bound the lane: %+v", res)
	}
	if res.Granted != 20_000 || res.BindingReason != ReasonRequested {
		t.Fatalf("Reserve after the raise = %+v, want 20000 / %s", res, ReasonRequested)
	}
}

// TestPlanShare_FollowupsIgnoreTheIntroContract: desired_daily_intros governs
// INTROS. A follow-up is an obligation the planner reserved separately, so the
// intro contract is not a yardstick for it and must never bypass its reserve.
func TestPlanShare_FollowupsIgnoreTheIntroContract(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	const lane, isp, dom = "wcl_remail", "aol", "em.historythinking.com"

	if _, err := db.Exec(DailyPlanDDL); err != nil {
		t.Fatalf("plan ddl: %v", err)
	}
	seedDay(t, db, day, 500_000, 106_399)
	setBalance(t, db, day, dom, isp, 500_000, 500_000)
	insertPlanRow(t, db, "drip_daily_plan", day, lane, isp, dom, 10, 0, 250)

	svc := NewService(db, WithClock(midWindow(day)), WithPlanReader(PlanStore{}))
	req := baseReq(day, "wave-followup", 20_000)
	req.TouchClass = "followup"
	req.LaneContractDesired = 106_399 // would bypass on the intro path
	res, err := svc.Reserve(ctx, req)
	if err != nil {
		t.Fatalf("Reserve (followup): %v", err)
	}
	if res.Granted != 250 || res.BindingReason != ReasonPlanShare {
		t.Fatalf("followup Reserve = %+v, want 250 / %s — the intro contract must not lift a follow-up reserve", res, ReasonPlanShare)
	}
}

// -----------------------------------------------------------------------------
// R1 / R2 — the invariant is enforced every tick, and a repair is REPORTED
// -----------------------------------------------------------------------------

// captureLogs redirects the standard logger for one test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	out, prefix, flags := log.Writer(), log.Prefix(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(out); log.SetPrefix(prefix); log.SetFlags(flags) })
	return &buf
}

// TestReconcileLaneBalances_RepairsDriftWithNoContractChange is the 2026-09-09
// defect. The contract did NOT move — every one of these rows carries its own
// contracted desired — so a reconciliation that only fires on a contract change
// repairs exactly none of them, which is why the lane was hand-patched four
// times in one night. yahoo below is the real row: 46,000 records ready and
// 3,324 of allowance to send them with.
func TestReconcileLaneBalances_RepairsDriftWithNoContractChange(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	const lane = "wcl_remail"

	// The production table, 2026-09-09 13:03Z.
	rows := []struct {
		isp                                    string
		desired, reserved, committed, unfilled int
		wantUnfilled                           int
		wantDrift                              bool
	}{
		{"yahoo", 106_399, 231, 54_500, 3_324, 51_668, true},
		{"att", 123_291, 835, 33_732, 12_201, 88_724, true},
		{"sbcglobal", 56_092, 764, 15_718, 5_470, 39_610, true},
		{"aol", 83_134, 74, 42_368, 4_769, 40_692, true},
		{"cox", 17_794, 275, 4_301, 1_199, 13_218, true},
		{"comcast", 33_447, 518, 16_449, 16_480, 16_480, false}, // already correct
	}

	desired := map[string]int{}
	for _, r := range rows {
		desired[r.isp] = r.desired
	}
	dc := domainContract("em.historythinking.com", 1, map[string]int{"aol": 1000})
	lc := dispatchContract(lane, 1, desired)
	if _, err := EnsureDayBalances(ctx, db, day, activeSet(day, []*DomainContract{dc}, []*DispatchContract{lc})); err != nil {
		t.Fatalf("EnsureDayBalances: %v", err)
	}
	for _, r := range rows {
		if _, err := db.Exec(`
			UPDATE drip_lane_balance SET reserved = $4, committed = $5, unfilled = $6
			WHERE day = $1::date AND lane = $2 AND isp = $3
		`, dayKey(day), lane, r.isp, r.reserved, r.committed, r.unfilled); err != nil {
			t.Fatalf("seed %s: %v", r.isp, err)
		}
	}

	buf := captureLogs(t)
	svc := NewService(db, WithClock(midWindow(day)))
	// The contract is UNCHANGED — same version, same desired as every row.
	res, err := svc.ReconcileLaneBalances(ctx, day, activeSet(day, []*DomainContract{dc}, []*DispatchContract{lc}))
	if err != nil {
		t.Fatalf("ReconcileLaneBalances: %v", err)
	}
	if res.Seen != 6 || res.Changed != 5 || res.Drifted != 5 {
		t.Fatalf("reconcile = %+v, want 6 seen / 5 changed / 5 drifted — a contract-triggered pass would report 0", res)
	}

	for _, r := range rows {
		got := readLaneBalance(t, db, day, lane, r.isp)
		if got.Desired != r.desired || got.Unfilled != r.wantUnfilled {
			t.Errorf("%s = desired %d unfilled %d, want %d / %d", r.isp, got.Desired, got.Unfilled, r.desired, r.wantUnfilled)
		}
		if sum := got.Reserved + got.Committed + got.Unfilled; sum != got.Desired {
			t.Errorf("%s violates reserved+committed+unfilled == desired: %d != %d", r.isp, sum, got.Desired)
		}
		if got.Reserved != r.reserved || got.Committed != r.committed {
			t.Errorf("%s: reconcile moved a consumption counter (reserved %d committed %d)", r.isp, got.Reserved, got.Committed)
		}
	}

	// R2: a repair is a DEFECT REPORT. Every drifted row is named in the log
	// with its old and new unfilled and the delta.
	logs := buf.String()
	for _, want := range []string{
		"DRIFT repaired wcl_remail/yahoo",
		"unfilled 3324 -> 51668 (delta +48344)",
		"DRIFT repaired wcl_remail/att",
		"unfilled 12201 -> 88724 (delta +76523)",
		"DRIFT repaired wcl_remail/sbcglobal",
		"DRIFT repaired wcl_remail/aol",
		"DRIFT repaired wcl_remail/cox",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log does not contain %q — a silent repair hides the Reserve/settle bug forever.\n%s", want, logs)
		}
	}
	// And the CLEAN row is not reported. A drift signal that fires on healthy
	// rows is one nobody reads.
	if strings.Contains(logs, "comcast") {
		t.Errorf("the already-correct comcast row was logged:\n%s", logs)
	}

	// Idempotent: the next tick repairs nothing and reports nothing.
	buf.Reset()
	res2, err := svc.ReconcileLaneBalances(ctx, day, activeSet(day, []*DomainContract{dc}, []*DispatchContract{lc}))
	if err != nil {
		t.Fatalf("second ReconcileLaneBalances: %v", err)
	}
	if res2.Changed != 0 || res2.Drifted != 0 {
		t.Fatalf("second pass = %+v, want 0 changed / 0 drifted", res2)
	}
	if strings.Contains(buf.String(), "DRIFT") {
		t.Errorf("the second pass reported drift on rows it had just repaired:\n%s", buf.String())
	}
}

// TestReconcileLaneBalances_OverspentLaneFloorsAtZero is R3 through the
// database: a contract lowered below what is already spent must floor unfilled
// at 0, never write a negative, and never claw back the ledger's spend.
func TestReconcileLaneBalances_OverspentLaneFloorsAtZero(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	const lane, isp = "wcl_remail", "aol"

	dc, _ := seedDay(t, db, day, 500_000, 86_036)
	if _, err := db.Exec(`
		UPDATE drip_lane_balance SET reserved = 1200, committed = 84618, unfilled = 218
		WHERE day = $1::date AND lane = $2 AND isp = $3
	`, dayKey(day), lane, isp); err != nil {
		t.Fatalf("spend the lane: %v", err)
	}
	lowered := dispatchContract(lane, 2, map[string]int{isp: 50_000})
	svc := NewService(db, WithClock(midWindow(day)))
	if _, err := svc.ReconcileLaneBalances(ctx, day, activeSet(day, []*DomainContract{dc}, []*DispatchContract{lowered})); err != nil {
		t.Fatalf("ReconcileLaneBalances: %v", err)
	}
	got := readLaneBalance(t, db, day, lane, isp)
	if got.Desired != 50_000 || got.Unfilled != 0 {
		t.Fatalf("lane = desired %d unfilled %d, want 50000 / 0", got.Desired, got.Unfilled)
	}
	if got.Reserved != 1_200 || got.Committed != 84_618 {
		t.Fatalf("a lowered contract clawed back the ledger's spend: reserved %d committed %d", got.Reserved, got.Committed)
	}
}
