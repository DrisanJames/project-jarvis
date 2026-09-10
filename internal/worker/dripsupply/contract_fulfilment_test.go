package dripsupply

import (
	"context"
	"testing"
	"time"
)

// contract_fulfilment_test.go — the 2026-09-09 rules, tested through symbols
// that ALREADY EXISTED before the change.
//
// That constraint is deliberate and it is the whole value of this file: every
// test here COMPILES on pre-change HEAD and FAILS there on behaviour, not on a
// missing identifier. A test that only fails to build proves the file is new;
// these prove the behaviour was wrong.
//
// The rules whose surface is genuinely new (window expiry, the mint verdict)
// live in contract_fulfilment_new_test.go and cannot do this.
//
// THE CONTRACT, in one line: minted rows for a domain×ISP×day equal the
// promise within ±5%. Under and over are both broken promises, and any place
// the system prevents that is a bug.

// -----------------------------------------------------------------------------
// RULE 1 — a balance row exists for EVERY contracted domain×ISP, desired=0
// included. An explicit zero grants 0 with a NAMED reason; an absent key is
// impossible.
// -----------------------------------------------------------------------------

// FAILS ON PRE-CHANGE HEAD. EnsureDayBalances skipped `desired <= 0`, so gmail
// got no lane row at all, Reserve took the sql.ErrNoRows branch, and the grant
// came back `no_lane_balance` — the SAME string an unseeded day, a failed
// contract load and a dark estate produce. The healthy case and the outage case
// were indistinguishable in SQL, which is how 2026-09-05 stayed invisible for
// 11h42m behind 44,658 denials.
func TestRule1_ContractedZeroGrantsZeroWithItsOwnReason(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	now := day.Add(10 * time.Hour)

	dc := domainContract("em.historythinking.com", 1, map[string]int{"aol": 7600, "gmail": 5000})
	// gmail: named by the contract, wanted at ZERO. This is the eight-brand
	// gmail ban's shape (memory: gmail-ban-eight-brands).
	lc := dispatchContract("wcl_remail", 1, map[string]int{"aol": 5000, "gmail": 0})
	if _, err := EnsureDayBalances(ctx, db, day, activeSet(day, []*DomainContract{dc}, []*DispatchContract{lc})); err != nil {
		t.Fatalf("EnsureDayBalances: %v", err)
	}

	// The row EXISTS. readLane fatals when it does not, which is the first half
	// of the assertion.
	if l := readLane(t, db, day, "wcl_remail", "gmail"); l.Desired != 0 || l.Unfilled != 0 {
		t.Fatalf("gmail lane row = %+v, want desired=0 unfilled=0", l)
	}

	svc := NewService(db, WithClock(func() time.Time { return now }))
	w, err := WindowOf(dc)
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Reserve(ctx, ReserveReq{
		Day: day, Domain: dc.SendingDomain, ISP: "gmail", Lane: lc.Lane,
		TouchClass: TouchClassIntro, WaveKey: "w1", Requested: 500,
		MailableSupply: -1, DomainVersion: 1, DispatchVersion: 1, Win: w,
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if res.Granted != 0 {
		t.Fatalf("granted %d on a contracted-zero ISP, want 0 — a promise of zero is still a promise", res.Granted)
	}
	// The literal, not the constant: this file must compile on pre-change HEAD.
	if res.BindingReason != "zero_desired" {
		t.Fatalf("binding reason = %q, want %q", res.BindingReason, "zero_desired")
	}
	if res.BindingReason == ReasonNoLaneBalance {
		t.Fatal("a contracted zero must NEVER report no_lane_balance: that string means the row is MISSING, and conflating the two is what hid the 2026-09-05 outage")
	}
	// And the decision is on the record, not merely returned.
	if got := readLedger(t, db, res.AllocationID); got.Reason != "zero_desired" || got.Reserved != 0 {
		t.Fatalf("ledger row = %+v, want reserved=0 binding_reason=zero_desired", got)
	}
}

// NEGATIVE CONTROL. An ISP no contract mentions is genuinely ABSENT and must
// still fail closed as `no_lane_balance`. Without this, "name the zero" could
// have been implemented by renaming every missing row, which would delete the
// outage signal instead of sharpening it.
func TestRule1_AbsentISPIsStillNoLaneBalance(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	now := day.Add(10 * time.Hour)

	dc := domainContract("em.historythinking.com", 1, map[string]int{"aol": 7600, "yahoo": 5000})
	lc := dispatchContract("wcl_remail", 1, map[string]int{"aol": 5000}) // yahoo: never mentioned
	if _, err := EnsureDayBalances(ctx, db, day, activeSet(day, []*DomainContract{dc}, []*DispatchContract{lc})); err != nil {
		t.Fatalf("EnsureDayBalances: %v", err)
	}

	svc := NewService(db, WithClock(func() time.Time { return now }))
	w, _ := WindowOf(dc)
	res, err := svc.Reserve(ctx, ReserveReq{
		Day: day, Domain: dc.SendingDomain, ISP: "yahoo", Lane: lc.Lane,
		TouchClass: TouchClassIntro, WaveKey: "w1", Requested: 500,
		MailableSupply: -1, DomainVersion: 1, DispatchVersion: 1, Win: w,
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if res.Granted != 0 || res.BindingReason != ReasonNoLaneBalance {
		t.Fatalf("absent ISP granted %d reason %q, want 0/%s — absence must stay loud",
			res.Granted, res.BindingReason, ReasonNoLaneBalance)
	}
}

// FAILS ON PRE-CHANGE HEAD. An excluded ISP is the contract SPEAKING about that
// ISP ("none"), not silence about it, so it seeds a row at 0 for exactly the
// same reason an explicit zero does. Before this it was skipped and reported
// `no_lane_balance`.
func TestRule1_ExcludedISPIsAContractedZeroNotAnAbsence(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	now := day.Add(10 * time.Hour)

	dc := domainContract("em.historythinking.com", 1, map[string]int{"aol": 7600, "gmail": 5000})
	// gmail carries a POSITIVE desire AND an exclusion. The exclusion is the
	// stronger statement and the row must land at 0.
	lc := dispatchContract("wcl_remail", 1, map[string]int{"aol": 5000, "gmail": 4000}, "gmail")
	if _, err := EnsureDayBalances(ctx, db, day, activeSet(day, []*DomainContract{dc}, []*DispatchContract{lc})); err != nil {
		t.Fatalf("EnsureDayBalances: %v", err)
	}
	if l := readLane(t, db, day, "wcl_remail", "gmail"); l.Desired != 0 || l.Unfilled != 0 {
		t.Fatalf("excluded gmail lane row = %+v, want desired=0 unfilled=0 (the exclusion beats the 4000)", l)
	}

	svc := NewService(db, WithClock(func() time.Time { return now }))
	w, _ := WindowOf(dc)
	res, err := svc.Reserve(ctx, ReserveReq{
		Day: day, Domain: dc.SendingDomain, ISP: "gmail", Lane: lc.Lane,
		TouchClass: TouchClassIntro, WaveKey: "w1", Requested: 500,
		MailableSupply: -1, DomainVersion: 1, DispatchVersion: 1, Win: w,
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if res.Granted != 0 || res.BindingReason != "zero_desired" {
		t.Fatalf("excluded ISP granted %d reason %q, want 0/zero_desired", res.Granted, res.BindingReason)
	}
}

// A lane that has SPENT its allowance is a different fact from a lane that was
// promised nothing, and the two must not share a reason either. This is the
// other side of the rename and it guards against over-applying it.
func TestRule1_ExhaustedLaneStillReportsLaneDemand(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	now := day.Add(10 * time.Hour)

	dc, lc := seedDay(t, db, day, 7600, 5000)
	setLaneUnfilled(t, db, day, lc.Lane, "aol", 0) // spent, not unwanted

	svc := NewService(db, WithClock(func() time.Time { return now }))
	w, _ := WindowOf(dc)
	res, err := svc.Reserve(ctx, ReserveReq{
		Day: day, Domain: dc.SendingDomain, ISP: "aol", Lane: lc.Lane,
		TouchClass: TouchClassIntro, WaveKey: "w1", Requested: 500,
		MailableSupply: -1, DomainVersion: 1, DispatchVersion: 1, Win: w,
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if res.Granted != 0 || res.BindingReason != ReasonLaneDemand {
		t.Fatalf("exhausted lane granted %d reason %q, want 0/%s — desired>0 means the lane WANTED more",
			res.Granted, res.BindingReason, ReasonLaneDemand)
	}
}

// -----------------------------------------------------------------------------
// RULE 2 — the day's mint targets the promise: fresh first, then the engaged
// pad fills the remainder.
// -----------------------------------------------------------------------------

// FAILS ON PRE-CHANGE HEAD. The pad was capped at
// introShareCap(max_remail_share, award) and could only ever be a fixed SHARE
// of the day, never the REMAINDER of it. A cell with 1,000 fresh against a
// 4,800 award and 50,000 engaged records planned 2,200 and filed the other
// 2,600 as `unserved_reason=supply` — a supply reason on a cell that was not
// short of supply.
//
// Operator ruling 5 (2026-09-09): filling from engaged records to reach the
// promise is ALWAYS allowed. Under-delivery is a broken promise exactly like
// over-delivery.
func TestRule2_EngagedPadFillsToThePromise(t *testing.T) {
	in := padInputs(t, 1000, 50000)
	p := assign(in)
	a := laneAward(t, p, "pad_lane", "aol")

	if a.AwardedCapacity != 4000 {
		t.Fatalf("fixture: awarded capacity = %d, want 4000", a.AwardedCapacity)
	}
	if a.AwardedFirm != 1000 {
		t.Errorf("firm = %d, want 1000 — FRESH IS SPENT FIRST", a.AwardedFirm)
	}
	if got := a.AwardedFirm + a.AwardedProvisional; got != a.AwardedCapacity {
		t.Fatalf("backed %d of a %d award (firm=%d pad=%d) — the pad must fill the REMAINDER, not a share of it",
			got, a.AwardedCapacity, a.AwardedFirm, a.AwardedProvisional)
	}
	if a.Released != 0 {
		t.Errorf("released %d with 50,000 engaged records available — nothing should be handed back", a.Released)
	}
	if a.UnservedReason == UnservedSupply {
		t.Errorf("unserved_reason=%q: a cell with 50,000 engaged records is not supply-bound", a.UnservedReason)
	}
}

// The pad closes a shortfall; it must never create an overshoot. Over-delivery
// is the other half of the broken promise and the ±5% band is two-sided.
func TestRule2_PadNeverMintsPastThePromise(t *testing.T) {
	for _, engaged := range []int{0, 1, 3999, 4000, 4001, 1_000_000} {
		p := assign(padInputs(t, 1000, engaged))
		a := laneAward(t, p, "pad_lane", "aol")
		if got := a.AwardedFirm + a.AwardedProvisional; got > a.AwardedCapacity {
			t.Errorf("engaged=%d: backed %d of a %d award — the pad overshot the promise",
				engaged, got, a.AwardedCapacity)
		}
	}
}

// Fresh is spent before the pad, whatever the pad holds. A blend would burn
// engaged records the lane did not need and leave fresh inventory ageing.
func TestRule2_FreshIsAlwaysSpentBeforeThePad(t *testing.T) {
	for _, fresh := range []int{0, 500, 3999, 4000, 9000} {
		p := assign(padInputs(t, fresh, 50000))
		a := laneAward(t, p, "pad_lane", "aol")
		wantFirm := fresh
		if wantFirm > a.AwardedCapacity {
			wantFirm = a.AwardedCapacity
		}
		if a.AwardedFirm != wantFirm {
			t.Errorf("fresh=%d: firm = %d, want %d (min(award, fresh))", fresh, a.AwardedFirm, wantFirm)
		}
	}
}

// The kill switch restores max_remail_share as a HARD ceiling — the pre-change
// behaviour, exactly — with no deploy. A new behaviour on the send path with no
// way back is not shippable.
func TestRule2_PadFillKillSwitchRestoresTheShareCap(t *testing.T) {
	t.Setenv("DRIP_SUPPLY_PAD_FILL_DISABLED", "1")
	p := assign(padInputs(t, 1000, 50000))
	a := laneAward(t, p, "pad_lane", "aol")
	// 0.25 x 4000 = 1000 of pad, on top of 1000 fresh.
	if a.AwardedProvisional != 1000 {
		t.Errorf("with the switch armed, pad = %d, want the share cap 1000 (0.25 x 4000)", a.AwardedProvisional)
	}
	if a.Released != 2000 {
		t.Errorf("released = %d, want 2000 — the share cap is back and the shortfall with it", a.Released)
	}
}

// Since the pad fills to the promise, splitCellSupply backs exactly
// min(award, supplyCeilingCap): the over-estimate that used to produce a
// SECOND-ORDER release in step 6b no longer exists in the default
// configuration. TestAssign_SupplyReclaimIsBoundedToOnePass keeps guarding the
// one-pass bound through the kill switch, for the day a future term re-opens a
// gap.
func TestRule2_PadFillLeavesNoSecondOrderRelease(t *testing.T) {
	in := padInputs(t, 0, 50000)
	p := assign(in)
	a := laneAward(t, p, "pad_lane", "aol")
	if a.Released != 0 {
		t.Errorf("released = %d, want 0 — with the pad filling to the promise there is nothing left over to re-offer", a.Released)
	}
	if got := a.AwardedFirm + a.AwardedProvisional; got != a.AwardedCapacity {
		t.Errorf("backed %d of %d", got, a.AwardedCapacity)
	}
}

// padInputs is one lane on one domain: 5,000 of desire, 4,000 of domain
// capacity (so the award is capacity-bound at 4,000 and the promise is
// unambiguous), `fresh` mailable now and `engaged` remail-eligible records.
// remail_enabled with max_remail_share 0.25, which is what the old share cap
// bound against.
func padInputs(t *testing.T, fresh, engaged int) Inputs {
	t.Helper()
	in := blankInputs(t)
	in.Contracts = newSet(in.Day,
		[]*DomainContract{planDomain("db", gDB, map[string]int{"aol": 4000})},
		[]*DispatchContract{
			planDispatch(dispatchSpec{lane: "pad_lane", tier: 2,
				desired: map[string]int{"aol": 5000}, allowed: []string{"db"}, introShare: 1.0}),
		},
		[]*InventoryContract{planInventory("pad_lane", true, 0.25)})
	in.FreshMailable = map[LaneISP]int{{"pad_lane", "aol"}: fresh}
	in.RemailEligible = map[LaneISP]int{{"pad_lane", "aol"}: engaged}
	in.Ranks = map[string]LaneRank{"pad_lane": {ContributionECPM: 5, Mature: true}}
	return in
}

// -----------------------------------------------------------------------------
// RULE 3 — the planner may not drain `unfilled` below the contract.
// -----------------------------------------------------------------------------

// FAILS ON PRE-CHANGE HEAD. Planner.store wrote
//
//	unfilled = GREATEST(award_firm + award_provisional - reserved - committed, 0)
//
// and award_firm+award_provisional is `desired` MINUS whatever supply the
// planner could not back at 00:05. So a 00:05 supply forecast became a hard
// ceiling on the lane's whole day, and when supply arrived later the ceiling did
// not move. Prod 2026-09-09: capped lanes ran at 65% of their mint
// (cl-20260909-122414-0c02, cl-20260909-130354-af16); yahoo_family held 46,000
// records ready against 3,324 of allowance and stalled all day.
func TestRule3_PlannerCannotDrainUnfilledBelowTheContract(t *testing.T) {
	db := newPlanTestDB(t)
	ctx := context.Background()
	in := blankInputs(t)
	in.Contracts = newSet(in.Day,
		[]*DomainContract{planDomain("db", gDB, map[string]int{"aol": 10000})},
		[]*DispatchContract{
			planDispatch(dispatchSpec{lane: "short_lane", tier: 2,
				desired: map[string]int{"aol": 10000}, allowed: []string{"db"}, introShare: 1.0}),
		},
		[]*InventoryContract{planInventory("short_lane", false, 0.25)})
	// Deliberately supply-starved: fresh 400, no EO, no remail. The award can
	// only be 400 of a 10,000 promise.
	in.FreshMailable = map[LaneISP]int{{"short_lane", "aol"}: 400}
	in.Ranks = map[string]LaneRank{"short_lane": {ContributionECPM: 5, Mature: true}}

	plan := assign(in)
	p := NewPlanner(WithPlannerClock(func() time.Time { return in.Now }))
	if err := p.store(ctx, db, in.Contracts, &plan); err != nil {
		t.Fatalf("store: %v", err)
	}

	l := readLane(t, db, in.Day, "short_lane", "aol")
	if l.Desired != 10000 {
		t.Fatalf("desired = %d, want the contract's 10000", l.Desired)
	}
	if l.Unfilled != 10000 {
		t.Fatalf("unfilled = %d after the plan froze, want 10000 — the planner drained the lane to its 00:05 supply forecast (award was %d)",
			l.Unfilled, plan.Lanes[0].AwardedFirm+plan.Lanes[0].AwardedProvisional)
	}
	if l.Reserved+l.Committed+l.Unfilled != l.Desired {
		t.Errorf("LaneInvariant broken by the planner: %d + %d + %d != %d",
			l.Reserved, l.Committed, l.Unfilled, l.Desired)
	}
}

// The invariant holds on the SECOND store too — an intraday replan is the
// scheduler re-firing, and re-run safety is the property, not a nice-to-have.
// Spent volume is preserved exactly and the allowance is whatever the contract
// has left over it.
func TestRule3_ReplanPreservesSpendAndTheInvariant(t *testing.T) {
	db := newPlanTestDB(t)
	ctx := context.Background()
	p, in, plan := storeGolden(t, db)

	if _, err := db.Exec(`
		UPDATE drip_lane_balance SET reserved = 900, committed = 2100
		WHERE day = $1::date AND lane = 'refi_heloc' AND isp = 'aol'
	`, dayKey(in.Day)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := p.store(ctx, db, in.Contracts, &plan); err != nil {
			t.Fatalf("store %d: %v", i, err)
		}
		l := readLane(t, db, in.Day, "refi_heloc", "aol")
		if l.Reserved != 900 || l.Committed != 2100 {
			t.Fatalf("store %d clobbered spend: reserved=%d committed=%d", i, l.Reserved, l.Committed)
		}
		if l.Reserved+l.Committed+l.Unfilled != l.Desired {
			t.Fatalf("store %d broke LaneInvariant: %d + %d + %d != %d",
				i, l.Reserved, l.Committed, l.Unfilled, l.Desired)
		}
	}
}

// A contract lowered BELOW what the lane has already spent floors `unfilled` at
// 0. It never goes negative (a negative reads as unbounded the moment anything
// sums it) and it never claws back capacity already reserved or committed —
// the ledger owns that.
func TestRule3_ContractBelowSpendFloorsAtZeroAndClawsNothingBack(t *testing.T) {
	db := newPlanTestDB(t)
	ctx := context.Background()
	in := blankInputs(t)
	in.Contracts = newSet(in.Day,
		[]*DomainContract{planDomain("db", gDB, map[string]int{"aol": 10000})},
		[]*DispatchContract{
			planDispatch(dispatchSpec{lane: "tiny_lane", tier: 2,
				desired: map[string]int{"aol": 500}, allowed: []string{"db"}, introShare: 1.0}),
		},
		[]*InventoryContract{planInventory("tiny_lane", false, 0.25)})
	in.FreshMailable = map[LaneISP]int{{"tiny_lane", "aol"}: 500}
	in.Ranks = map[string]LaneRank{"tiny_lane": {ContributionECPM: 5, Mature: true}}
	plan := assign(in)
	p := NewPlanner(WithPlannerClock(func() time.Time { return in.Now }))
	if err := p.store(ctx, db, in.Contracts, &plan); err != nil {
		t.Fatalf("store: %v", err)
	}
	// The lane has already spent MORE than the (now lowered) contract.
	if _, err := db.Exec(`
		UPDATE drip_lane_balance SET reserved = 300, committed = 900
		WHERE day = $1::date AND lane = 'tiny_lane' AND isp = 'aol'
	`, dayKey(in.Day)); err != nil {
		t.Fatal(err)
	}
	if err := p.store(ctx, db, in.Contracts, &plan); err != nil {
		t.Fatalf("re-store: %v", err)
	}
	l := readLane(t, db, in.Day, "tiny_lane", "aol")
	if l.Unfilled != 0 {
		t.Errorf("unfilled = %d, want 0 — floored, never negative", l.Unfilled)
	}
	if l.Reserved != 300 || l.Committed != 900 {
		t.Errorf("spend was clawed back: reserved=%d committed=%d, want 300/900", l.Reserved, l.Committed)
	}
}

// -----------------------------------------------------------------------------
// RULE 5 — every producer that spends a domain×ISP×day draws from the SAME
// balance, so the sum CANNOT exceed the promise. By construction, not by
// convention.
// -----------------------------------------------------------------------------

// Three producers (T1 intro, follow-up, and the AOL rotated companion wave) all
// reserve against ONE domain balance. The domain is contracted at 1,000 and the
// three between them ask for 3,000: the third gets exactly the remainder and
// the sum lands on the promise, never past it.
//
// This is a REGRESSION GUARD, not a bug fix — Reserve already bounds each grant
// by `effective - reserved - committed` and increments `reserved` inside the
// same transaction, so the property holds. It is pinned here because rule 5 is
// the one rule with no diff behind it, and an untested invariant is one refactor
// away from being untrue.
func TestRule5_ThreeProducersCannotExceedThePromise(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	now := day.Add(19 * time.Hour) // late in the window: the whole day's tokens have accrued

	dc, lc := seedDay(t, db, day, 1000, 100000)
	// The token bucket is a PACING term, not the contract, and it is not what
	// this test is about: give the cell the whole day's tokens so the only thing
	// that can bind is `effective - reserved - committed`, which is the promise.
	setBalance(t, db, day, dc.SendingDomain, "aol", 1000, 1000)
	svc := NewService(db, WithClock(func() time.Time { return now }))
	w, _ := WindowOf(dc)

	total := 0
	for _, p := range []struct {
		wave  string
		touch string
	}{
		{"t1-intro", TouchClassIntro},
		{"followup", TouchClassFollowup},
		{"aol-rotate", TouchClassIntro},
	} {
		res, err := svc.Reserve(ctx, ReserveReq{
			Day: day, Domain: dc.SendingDomain, ISP: "aol", Lane: lc.Lane,
			TouchClass: p.touch, WaveKey: p.wave, Requested: 1000,
			MailableSupply: -1, DomainVersion: 1, DispatchVersion: 1, Win: w,
		})
		if err != nil {
			t.Fatalf("Reserve(%s): %v", p.wave, err)
		}
		total += res.Granted
	}
	if total != 1000 {
		t.Fatalf("three producers were granted %d against a 1,000 promise", total)
	}
	b := readBalance(t, db, day, dc.SendingDomain, "aol")
	if b.Reserved+b.Committed > b.Effective {
		t.Fatalf("balance over-spent: reserved=%d + committed=%d > effective=%d",
			b.Reserved, b.Committed, b.Effective)
	}
}

// The same three producers replayed — an ECS bounce mid-tick, a retry, the
// second orchestrator instance on the same tick — consume NOTHING extra. The
// wave key is derived from the tick, so the replay resolves to the same
// idempotency key and gets the first allocation back.
func TestRule5_ProducerReplayCannotDoubleSpendThePromise(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	now := day.Add(19 * time.Hour)

	dc, lc := seedDay(t, db, day, 1000, 100000)
	setBalance(t, db, day, dc.SendingDomain, "aol", 1000, 1000)
	svc := NewService(db, WithClock(func() time.Time { return now }))
	w, _ := WindowOf(dc)
	req := ReserveReq{
		Day: day, Domain: dc.SendingDomain, ISP: "aol", Lane: lc.Lane,
		TouchClass: TouchClassIntro, WaveKey: "t1-intro", Requested: 600,
		MailableSupply: -1, DomainVersion: 1, DispatchVersion: 1, Win: w,
	}
	first, err := svc.Reserve(ctx, req)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := svc.Reserve(ctx, req)
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if again.AllocationID != first.AllocationID || again.Granted != first.Granted {
			t.Fatalf("replay %d produced a NEW grant %s/%d (first %s/%d)",
				i, again.AllocationID, again.Granted, first.AllocationID, first.Granted)
		}
	}
	b := readBalance(t, db, day, dc.SendingDomain, "aol")
	if b.Reserved != 600 {
		t.Fatalf("balance reserved = %d after 6 identical reservations, want 600", b.Reserved)
	}
}
