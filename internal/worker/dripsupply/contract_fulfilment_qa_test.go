package dripsupply

import (
	"context"
	"testing"
	"time"
)

// contract_fulfilment_qa_test.go — WP-E (QA) additions to the 2026-09-09
// contract-fulfilment suite. NEW FILE ONLY: nothing here rewrites a builder's
// test. Each block closes a negative path the builders' files leave open.
//
// The four gaps, and why each one matters:
//
//	1. The engaged pad now fills to the promise. Nothing proved it still stops
//	   at an inventory contract that has switched remail OFF — a fill-to-promise
//	   that overrides a prohibition is the rule-5 leak wearing rule-2's clothes.
//	2. nameContractedZero renames a zero grant. Nothing proved it leaves the
//	   ABSENCE reasons alone. `no_balance` / `no_lane_balance` are the outage
//	   strings; renaming one of them to `zero_contracted` would put the
//	   2026-09-05 dark-estate signal back under a healthy-looking word, which is
//	   the exact failure rule 1 exists to end.
//	3. ReconcileLaneBalances changed from "skip an excluded ISP" to "reconcile it
//	   to zero". The skip is what left YESTERDAY's desire standing on a row after
//	   an exclusion landed; no test covered the exclusion path at all.
//	4. Rule 3 is tested on a fresh INSERT and on a replan at unchanged desire.
//	   The prod incident was neither: a lane with spend already on it whose
//	   contract was RAISED mid-day (yahoo_family, 46,000 ready against 3,324 of
//	   allowance). That is the ON CONFLICT DO UPDATE arm with a moving desired,
//	   and it was untested.

// -----------------------------------------------------------------------------
// RULE 2 — filling to the promise is allowed; overriding a prohibition is not
// -----------------------------------------------------------------------------

// NEGATIVE CONTROL for TestRule2_EngagedPadFillsToThePromise.
//
// `remail_enabled = false` on the inventory contract is a PROHIBITION on
// re-mailing that lane's engaged records — not a volume preference. Rule 2 says
// the pad fills whatever remains of the award; it does not say the pad may
// appear where the contract forbids it. Without this test, "the pad fills the
// remainder" could have been implemented by ignoring RemailEnabled whenever the
// lane was short, and every existing rule-2 test (all of which run with remail
// ON) would still pass.
//
// The lane is left SHORT on purpose. A short mint is the honest outcome here and
// reportMintVerdict will say so; silently re-mailing against a contract that
// says not to would be the dishonest one.
func TestQA_Rule2_PadFillCannotOverrideARemailProhibition(t *testing.T) {
	in := blankInputs(t)
	in.Contracts = newSet(in.Day,
		[]*DomainContract{planDomain("db", gDB, map[string]int{"aol": 4000})},
		[]*DispatchContract{
			planDispatch(dispatchSpec{lane: "noremail_lane", tier: 2,
				desired: map[string]int{"aol": 5000}, allowed: []string{"db"}, introShare: 1.0}),
		},
		// remail_enabled = FALSE, with a generous share that must not matter.
		[]*InventoryContract{planInventory("noremail_lane", false, 0.90)})
	in.FreshMailable = map[LaneISP]int{{"noremail_lane", "aol"}: 1000}
	in.RemailEligible = map[LaneISP]int{{"noremail_lane", "aol"}: 50000}
	in.Ranks = map[string]LaneRank{"noremail_lane": {ContributionECPM: 5, Mature: true}}

	a := laneAward(t, assign(in), "noremail_lane", "aol")
	if a.AwardedFirm != 1000 {
		t.Errorf("firm = %d, want the 1000 of fresh — fresh is unaffected by the remail switch", a.AwardedFirm)
	}
	if a.AwardedProvisional != 0 {
		t.Errorf("pad = %d with remail_enabled=false and 50,000 eligible records — fill-to-promise must never override a prohibition", a.AwardedProvisional)
	}
	if a.AwardedFirm+a.AwardedProvisional >= a.AwardedCapacity {
		t.Errorf("the lane reached %d of its %d award: it should be SHORT, and short is the honest outcome when the contract forbids the only supply left",
			a.AwardedFirm+a.AwardedProvisional, a.AwardedCapacity)
	}
}

// The kill switch and the prohibition are independent: with the pad-fill switch
// ARMED and remail still enabled, the share cap binds; with remail DISABLED the
// pad is zero whatever the switch says. Neither one may resurrect the other.
func TestQA_Rule2_KillSwitchAndProhibitionAreIndependent(t *testing.T) {
	t.Setenv(PadFillDisabledEnv, "1")
	in := blankInputs(t)
	in.Contracts = newSet(in.Day,
		[]*DomainContract{planDomain("db", gDB, map[string]int{"aol": 4000})},
		[]*DispatchContract{
			planDispatch(dispatchSpec{lane: "noremail_lane", tier: 2,
				desired: map[string]int{"aol": 5000}, allowed: []string{"db"}, introShare: 1.0}),
		},
		[]*InventoryContract{planInventory("noremail_lane", false, 0.90)})
	in.FreshMailable = map[LaneISP]int{{"noremail_lane", "aol"}: 1000}
	in.RemailEligible = map[LaneISP]int{{"noremail_lane", "aol"}: 50000}
	in.Ranks = map[string]LaneRank{"noremail_lane": {ContributionECPM: 5, Mature: true}}

	if a := laneAward(t, assign(in), "noremail_lane", "aol"); a.AwardedProvisional != 0 {
		t.Errorf("pad = %d, want 0 — the switch changes the CEILING on the pad, it does not enable a pad the inventory contract forbids", a.AwardedProvisional)
	}
}

// -----------------------------------------------------------------------------
// RULE 1 — the rename sharpens the outage signal; it must not swallow it
// -----------------------------------------------------------------------------

// NEGATIVE CONTROL for nameContractedZero. `no_balance` and `no_lane_balance`
// mean a row is MISSING — an unseeded day, a contract that never loaded, an
// estate going dark. dark_alert.go classifies them as contract denials and they
// are the strings the 2026-09-05 11h42m outage hid behind.
//
// nameContractedZero fires on `Granted == 0`, and a domain whose balance row is
// absent also presents `Contracted == 0`, so the two conditions overlap
// exactly. Only the explicit switch on the binding reason keeps the absence
// reasons out of the rename — a `default:` that renamed anything would have
// deleted the outage signal, and every rule-1 test would still be green.
func TestQA_Rule1_AbsenceIsNeverRenamedToAContractedZero(t *testing.T) {
	// A zero-contracted domain balance is the shape most likely to over-trigger.
	zeroDomain := Balance{Contracted: 0, Effective: 0}
	fullLane := LaneBalance{Desired: 4000, Unfilled: 4000}

	for _, reason := range []string{
		ReasonNoBalance,
		ReasonNoLaneBalance,
		ReasonOutsideWindow,
		SkipNoContract,
		ReasonReserveTimeout,
	} {
		got := nameContractedZero(Decision{Granted: 0, BindingReason: reason}, zeroDomain, fullLane)
		if got.BindingReason != reason {
			t.Errorf("nameContractedZero renamed %q to %q — that string reports a MISSING row (an incident), not a promise of zero",
				reason, got.BindingReason)
		}
	}

	// And the lane-side rename is equally narrow: only `lane_demand` on a
	// desired=0 row becomes `zero_desired`.
	zeroLane := LaneBalance{Desired: 0, Unfilled: 0}
	fundedDomain := Balance{Contracted: 5000, Effective: 5000, Tokens: 5000}
	for _, reason := range []string{ReasonNoLaneBalance, ReasonPlanShare, ReasonSupply, ReasonDomainTokens} {
		got := nameContractedZero(Decision{Granted: 0, BindingReason: reason}, fundedDomain, zeroLane)
		if got.BindingReason != reason {
			t.Errorf("a desired=0 lane renamed %q to %q — only the LANE term binding is evidence of a contracted zero",
				reason, got.BindingReason)
		}
	}
	if got := nameContractedZero(Decision{Granted: 0, BindingReason: ReasonLaneDemand}, fundedDomain, zeroLane); got.BindingReason != ReasonZeroDesired {
		t.Errorf("the one case that SHOULD rename did not: got %q, want %q", got.BindingReason, ReasonZeroDesired)
	}
}

// FAILS ON PRE-CHANGE HEAD (behaviour). ReconcileLaneBalances used to SKIP an
// excluded ISP entirely, so a lane row seeded yesterday at 5,000 kept carrying
// 5,000 of desire and allowance after today's contract excluded that ISP — the
// lane went on spending against a number the contract had withdrawn. It now
// reconciles the row TO ZERO, which is the same statement EnsureDayBalances
// makes when it seeds an excluded ISP at 0.
//
// The exclusion path had no test of any kind before this one.
func TestQA_Rule1_ExclusionLandingMidDayReconcilesTheRowToZero(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)

	dc := domainContract("em.historythinking.com", 1, map[string]int{"aol": 7600, "gmail": 7600})
	// Yesterday's shape: gmail funded at 5,000.
	funded := dispatchContract("wcl_remail", 1, map[string]int{"aol": 4000, "gmail": 5000})
	if _, err := EnsureDayBalances(ctx, db, day, activeSet(day, []*DomainContract{dc}, []*DispatchContract{funded})); err != nil {
		t.Fatalf("EnsureDayBalances: %v", err)
	}
	if l := readLane(t, db, day, "wcl_remail", "gmail"); l.Desired != 5000 {
		t.Fatalf("fixture: gmail desired = %d, want 5000", l.Desired)
	}

	// Today's shape: the eight-brand gmail ban lands as an EXCLUSION.
	excluded := dispatchContract("wcl_remail", 2, map[string]int{"aol": 4000, "gmail": 5000}, "gmail")
	svc := NewService(db, WithClock(func() time.Time { return day.Add(10 * time.Hour) }))
	res, err := svc.ReconcileLaneBalances(ctx, day, activeSet(day, []*DomainContract{dc}, []*DispatchContract{excluded}))
	if err != nil {
		t.Fatalf("ReconcileLaneBalances: %v", err)
	}
	if res.Changed == 0 {
		t.Error("the exclusion changed nothing — the excluded ISP was skipped, which is the pre-change behaviour")
	}

	l := readLane(t, db, day, "wcl_remail", "gmail")
	if l.Desired != 0 || l.Unfilled != 0 {
		t.Fatalf("gmail lane after the exclusion = %+v, want desired=0 unfilled=0 — the lane kept spending against a withdrawn number", l)
	}
	if l.Reserved+l.Committed+l.Unfilled != l.Desired {
		t.Errorf("LaneInvariant broken after the exclusion: %d + %d + %d != %d", l.Reserved, l.Committed, l.Unfilled, l.Desired)
	}
	// NEGATIVE CONTROL: the funded sibling on the same contract is untouched.
	if a := readLane(t, db, day, "wcl_remail", "aol"); a.Desired != 4000 || a.Unfilled != 4000 {
		t.Errorf("aol lane = %+v, want desired=4000 unfilled=4000 — an exclusion on gmail is not an exclusion on aol", a)
	}
}

// -----------------------------------------------------------------------------
// RULE 3 — the allowance follows the CONTRACT, including when it moves up
// -----------------------------------------------------------------------------

// The prod incident's exact shape, which neither existing rule-3 test covers:
// a lane that has ALREADY SPENT part of its day, whose contract is then RAISED,
// with supply still lagging the new promise. That combination lands on
// Planner.store's ON CONFLICT DO UPDATE arm with a moving `desired` — the arm
// TestRule3_PlannerCannotDrainUnfilledBelowTheContract (a fresh INSERT) and
// TestRule3_ReplanPreservesSpendAndTheInvariant (unchanged desired) both miss.
//
// FAILS ON PRE-CHANGE HEAD: the old expression wrote
// GREATEST(award_firm + award_provisional - reserved - committed, 0), so with a
// 5,000-of-supply award against a 46,000 promise the lane would reopen to 1,676
// instead of 42,676 and would sit on inventory it was entitled to mail — which
// is what yahoo_family did all day on 2026-09-09 (46,000 ready, 3,324 allowed).
func TestQA_Rule3_ContractRaisedMidDayReopensTheAllowanceOverExistingSpend(t *testing.T) {
	db := newPlanTestDB(t)
	ctx := context.Background()

	lane := "ramping_lane"
	build := func(desired, fresh int) Inputs {
		in := blankInputs(t)
		in.Contracts = newSet(in.Day,
			[]*DomainContract{planDomain("db", gDB, map[string]int{"aol": 100000})},
			[]*DispatchContract{
				planDispatch(dispatchSpec{lane: lane, tier: 2,
					desired: map[string]int{"aol": desired}, allowed: []string{"db"}, introShare: 1.0}),
			},
			[]*InventoryContract{planInventory(lane, false, 0.25)})
		in.FreshMailable = map[LaneISP]int{{lane, "aol"}: fresh}
		in.Ranks = map[string]LaneRank{lane: {ContributionECPM: 5, Mature: true}}
		return in
	}

	// 00:05 — the day freezes at the small contract, fully supplied.
	morning := build(3324, 3324)
	p := NewPlanner(WithPlannerClock(func() time.Time { return morning.Now }))
	first := assign(morning)
	if err := p.store(ctx, db, morning.Contracts, &first); err != nil {
		t.Fatalf("store (morning): %v", err)
	}
	// The lane spends most of it.
	if _, err := db.Exec(`
		UPDATE drip_lane_balance SET reserved = 1000, committed = 2324
		WHERE day = $1::date AND lane = $2 AND isp = 'aol'
	`, dayKey(morning.Day), lane); err != nil {
		t.Fatal(err)
	}

	// Midday — the operator raises the contract to 46,000. Supply has NOT caught
	// up: only 5,000 is mailable right now, so the award is supply-bound.
	midday := build(46000, 5000)
	second := assign(midday)
	if a := laneAward(t, second, lane, "aol"); a.AwardedFirm+a.AwardedProvisional >= 46000 {
		t.Fatalf("fixture: the award is %d and must stay supply-bound below the 46,000 promise, or this test proves nothing",
			a.AwardedFirm+a.AwardedProvisional)
	}
	if err := p.store(ctx, db, midday.Contracts, &second); err != nil {
		t.Fatalf("store (midday): %v", err)
	}

	l := readLane(t, db, midday.Day, lane, "aol")
	if l.Desired != 46000 {
		t.Fatalf("desired = %d, want the raised contract's 46000", l.Desired)
	}
	if l.Reserved != 1000 || l.Committed != 2324 {
		t.Fatalf("the raise clobbered spend: reserved=%d committed=%d, want 1000/2324", l.Reserved, l.Committed)
	}
	if want := 46000 - 1000 - 2324; l.Unfilled != want {
		t.Fatalf("unfilled = %d, want %d — the lane must reopen to the CONTRACT minus its spend, not to the 00:05 supply forecast",
			l.Unfilled, want)
	}
	if l.Reserved+l.Committed+l.Unfilled != l.Desired {
		t.Errorf("LaneInvariant broken by the raise: %d + %d + %d != %d",
			l.Reserved, l.Committed, l.Unfilled, l.Desired)
	}
}
