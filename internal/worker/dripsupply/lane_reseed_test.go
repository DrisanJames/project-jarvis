package dripsupply

import (
	"context"
	"database/sql"
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

func TestReseed_FollowsTheContractWithoutResettingTheDay(t *testing.T) {
	cases := []struct {
		name            string
		prev            LaneBalance
		contractDesired int
		wantDesired     int
		wantUnfilled    int
	}{
		{
			// THE PROD CASE. 85,818 already consumed against the old shape; the
			// contract steps up 20,363 and the lane must gain exactly that much.
			name:            "raised mid-day with volume already consumed (yahoo 2026-09-09)",
			prev:            LaneBalance{Desired: 86_036, Unfilled: 218, Reserved: 1_200, Committed: 84_618},
			contractDesired: 106_399,
			wantDesired:     106_399,
			wantUnfilled:    20_581, // 106,399 - (86,036 - 218)
		},
		{
			name:            "contract unchanged is a no-op",
			prev:            LaneBalance{Desired: 86_036, Unfilled: 218, Reserved: 1_200, Committed: 84_618},
			contractDesired: 86_036,
			wantDesired:     86_036,
			wantUnfilled:    218,
		},
		{
			// A contract that goes DOWN cannot claw back what is already spent:
			// the ledger owns that. unfilled floors at 0 and NEVER goes negative
			// — a negative unfilled would read as a huge number the moment
			// anything did unsigned arithmetic on it.
			name:            "lowered below what is already consumed floors at zero",
			prev:            LaneBalance{Desired: 86_036, Unfilled: 218, Reserved: 1_200, Committed: 84_618},
			contractDesired: 50_000,
			wantDesired:     50_000,
			wantUnfilled:    0,
		},
		{
			name:            "lowered but still above consumption keeps the remainder",
			prev:            LaneBalance{Desired: 86_036, Unfilled: 218, Committed: 85_818},
			contractDesired: 90_000,
			wantDesired:     90_000,
			wantUnfilled:    4_182, // 90,000 - 85,818
		},
		{
			name:            "brand new day with nothing consumed takes the contract whole",
			prev:            LaneBalance{Desired: 40_000, Unfilled: 40_000},
			contractDesired: 55_000,
			wantDesired:     55_000,
			wantUnfilled:    55_000,
		},
		{
			name:            "a zero contract closes the lane",
			prev:            LaneBalance{Desired: 40_000, Unfilled: 12_000, Committed: 28_000},
			contractDesired: 0,
			wantDesired:     0,
			wantUnfilled:    0,
		},
		{
			// A planner award writes straight into `unfilled` (planner.go:2035),
			// so unfilled can sit well below desired with nothing consumed. The
			// DELTA form is what keeps that award intact instead of erasing it.
			name:            "a planner award below desired keeps its shape and gains the delta",
			prev:            LaneBalance{Desired: 86_036, AwardedFirm: 37_640, Unfilled: 37_640},
			contractDesired: 106_399,
			wantDesired:     106_399,
			wantUnfilled:    58_003, // 37,640 + (106,399 - 86,036)
		},
		{
			name:            "a negative contract is treated as zero, never as a negative ceiling",
			prev:            LaneBalance{Desired: 1_000, Unfilled: 1_000},
			contractDesired: -5,
			wantDesired:     0,
			wantUnfilled:    0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := c.prev
			got := reseed(c.prev, c.contractDesired)

			if c.prev != before {
				t.Fatalf("reseed mutated its argument:\n got %+v\nwant %+v", c.prev, before)
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
			// Consumed volume is preserved EXACTLY: reserved/committed and the
			// planner's award are the ledger's and the planner's to change, not
			// this function's.
			if got.Reserved != before.Reserved || got.Committed != before.Committed ||
				got.AwardedFirm != before.AwardedFirm || got.AwardedProvisional != before.AwardedProvisional {
				t.Errorf("reseed touched consumption/award counters: got %+v, want them as in %+v", got, before)
			}
		})
	}
}

// TestReseed_IsIdempotent: the reconciliation runs on EVERY tick (~every 15 s in
// prod). If it were not a fixed point the lane would drift by one delta per tick.
func TestReseed_IsIdempotent(t *testing.T) {
	prev := LaneBalance{Desired: 86_036, Unfilled: 218, Committed: 85_818}
	once := reseed(prev, 106_399)
	for i := 0; i < 50; i++ {
		if again := reseed(once, 106_399); again != once {
			t.Fatalf("reseed is not a fixed point: pass %d = %+v, first = %+v", i, again, once)
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
