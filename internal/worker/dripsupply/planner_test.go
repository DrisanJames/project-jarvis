package dripsupply

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/pkg/contractmeta"
)

// planner_test.go — WP6.
//
// The algorithm tests are PURE: they build an Inputs fixture in Go, call
// assign(), and check the result. No database, no clock, no map-order luck. The
// golden file (testdata/golden_plan.json) is the hand-computed expected output
// of the fixture in goldenInputs(); every number in it was worked out on paper
// from §2.5 before the code was run, which is the only way a golden file is
// evidence rather than a screenshot of whatever the code happened to do.
//
// The database tests reuse reservation_test.go's local-Postgres harness
// (scratch schema per test, skip when unreachable) and cover exactly the two
// things assign() cannot: freeze/replan idempotency, and PlanRemaining.

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/golden_plan.json from the current assign() output")

// -----------------------------------------------------------------------------
// Fixture helpers
// -----------------------------------------------------------------------------

func planDomain(brand, domain string, maxByISP map[string]int) *DomainContract {
	return &DomainContract{
		Meta:              Meta{Version: 1, Status: StatusActive},
		SendingDomain:     domain,
		BrandCode:         brand,
		DailyMaxByISP:     maxByISP,
		ActiveWindowStart: "01:00",
		ActiveWindowEnd:   "20:00",
		IntervalMinutes:   15,
		MaxBurstIntervals: 2,
	}
}

type dispatchSpec struct {
	lane        string
	tier        int
	desired     map[string]int
	allowed     []string
	exclusions  []string
	introShare  float64
	exploration float64
	followups   bool
	demandMode  string
	ceiling     *int
}

func planDispatch(s dispatchSpec) *DispatchContract {
	mode := s.demandMode
	if mode == "" {
		mode = DemandModeTarget
	}
	share := s.introShare
	if share == 0 {
		share = 0.40
	}
	return &DispatchContract{
		Meta:                 Meta{Version: 1, Status: StatusActive},
		Lane:                 s.lane,
		OperatorPriorityTier: s.tier,
		DesiredDailyIntros:   s.desired,
		DemandMode:           mode,
		DailyCeiling:         s.ceiling,
		AllowedDomains:       s.allowed,
		ISPExclusions:        s.exclusions,
		LadderTouches:        5,
		LadderGapHours:       24,
		FollowupsCommitted:   s.followups,
		MaxIntroShare:        share,
		ExplorationShare:     s.exploration,
	}
}

func planInventory(lane string, remail bool, remailShare float64) *InventoryContract {
	return &InventoryContract{
		Meta:                Meta{Version: 1, Status: StatusActive},
		Lane:                lane,
		AcceptedSources:     []string{lane + "_src"},
		VerdictValidDays:    60,
		EOEnabled:           true,
		MaxDailyEOSpendUSD:  50,
		MinEOOrder:          1000,
		MinCoverageHours:    8,
		TargetCoverageHours: 16,
		MaxCoverageHours:    36,
		RemailEnabled:       remail,
		RemailAfterDays:     7,
		RemailMode:          RemailModeFullLadder,
		MaxRemailShare:      remailShare,
	}
}

func newSet(day time.Time, doms []*DomainContract, disp []*DispatchContract, inv []*InventoryContract) *ActiveSet {
	set := &ActiveSet{
		Day:           day,
		Domains:       map[string]*DomainContract{},
		Dispatches:    map[string]*DispatchContract{},
		Inventories:   map[string]*InventoryContract{},
		SourcesBySlug: map[string]*SourceContract{},
	}
	for _, d := range doms {
		set.Domains[d.SendingDomain] = d
	}
	for _, d := range disp {
		set.Dispatches[d.Lane] = d
	}
	for _, i := range inv {
		set.Inventories[i.Lane] = i
	}
	return set
}

func hours(pairs map[int]int) [24]int {
	var out [24]int
	for h, n := range pairs {
		out[h] = n
	}
	return out
}

// blankInputs is the empty shell every focused test fills in.
func blankInputs(t *testing.T) Inputs {
	t.Helper()
	loc := testLoc(t)
	return Inputs{
		Day:                time.Date(2026, 9, 10, 0, 0, 0, 0, loc),
		Now:                time.Date(2026, 9, 10, 0, 5, 0, 0, loc),
		FreshMailable:      map[LaneISP]int{},
		RemailEligible:     map[LaneISP]int{},
		PendingEO:          map[LaneISP]int{},
		FollowupsDue:       map[LaneISP]int{},
		FollowupsDueByHour: map[LaneISP][24]int{},
		Ranks:              map[string]LaneRank{},
		Yields:             map[string]float64{},
		OldestIngest:       map[string]time.Time{},
		RankSourceWired:    true,
	}
}

func laneAward(t *testing.T, p Plan, lane, isp string) LaneAward {
	t.Helper()
	for _, l := range p.Lanes {
		if l.Lane == lane && l.ISP == isp {
			return l
		}
	}
	t.Fatalf("no lane award for %s/%s (have %+v)", lane, isp, p.Lanes)
	return LaneAward{}
}

func rowFor(t *testing.T, p Plan, lane, isp, domain string) PlanRow {
	t.Helper()
	for _, r := range p.Rows {
		if r.Lane == lane && r.ISP == isp && r.SendingDomain == domain {
			return r
		}
	}
	t.Fatalf("no plan row for %s/%s/%s", lane, isp, domain)
	return PlanRow{}
}

// -----------------------------------------------------------------------------
// The golden fixture
// -----------------------------------------------------------------------------

const (
	gDB = "em.discountblog.com"
	gHT = "em.historythinking.com"
	gMH = "em.myownhealth.net"
)

// goldenInputs is the hand-computed fixture. Its arithmetic, worked from §2.5:
//
//	capacity aol   : db 12000, ht 6000, mh 6000   (24000)
//	capacity yahoo : db  6000, ht 4000, mh    0
//
//	step 3 follow-ups (deadline order: refi_heloc h=2, then consumer h=5),
//	       proportional to CONTRACTED capacity 12000:6000:6000 = 2:1:1
//	  refi_heloc/aol 3000 -> db 1500, ht 750, mh 750
//	  consumer/aol   1000 -> db  500, ht 250, mh 250
//	  remaining aol  -> db 10000, ht 5000, mh 5000
//
//	step 4 rank: refi_heloc(t1,$9.40) 1, consumer(t2,$6.20) 2,
//	             wcl_remail(t2,$4.10) 3, dead_lane(t2,-$1.20, blocked) 4,
//	             probe_v9(t9) 5
//
//	sole reserve: wcl_remail is the only lane on db alone ->
//	              min(6000, 0.40x12000=4800) = 4800 held on (db,aol)
//	contention aol: db 18000/10000=1.80, ht 12000/5000=2.40, mh 2.40
//
//	step 5:
//	  refi_heloc/aol  8000 -> db min(10000-4800, 4800)=4800, ht 2400, mh 800  = 8000
//	  refi_heloc/yah  5000 -> db 0.40x6000=2400, ht 0.40x4000=1600           = 4000 (max_intro_share)
//	  consumer/aol    4000 -> db 5200-4800=400, ht 2400, mh 1200             = 4000
//	  wcl_remail/aol  6000 -> db 4800 (its reserve survived ranks 1 and 2)   = 4800
//	  dead_lane/aol   2000 -> blocked, 0
//	  probe_v9/aol    3000 -> only 200 left on ht                            =  200
//
//	step 6 (yield 0.85 seed):
//	  refi_heloc/aol  firm min(8000, fresh 5000)=5000; prov min(3000, 2000x0.85=1700)=1700
//	  refi_heloc/yah  firm min(4000, fresh 6000)=4000; prov 0
//	  consumer/aol    firm min(4000, fresh  900)= 900; prov 0
//	  wcl_remail/aol  firm min(4800, fresh 1000)=1000; prov min(3800, remail credit
//	                  min(50000, 0.25x4800=1200)=1200)=1200
//	  probe_v9/aol    firm min(200, fresh 400)=200
func goldenInputs(t *testing.T) Inputs {
	t.Helper()
	in := blankInputs(t)
	in.Contracts = newSet(in.Day,
		[]*DomainContract{
			planDomain("db", gDB, map[string]int{"aol": 12000, "yahoo": 6000}),
			planDomain("ht", gHT, map[string]int{"aol": 6000, "yahoo": 4000}),
			planDomain("mh", gMH, map[string]int{"aol": 6000, "yahoo": 0}),
		},
		[]*DispatchContract{
			planDispatch(dispatchSpec{lane: "refi_heloc", tier: 1,
				desired: map[string]int{"aol": 8000, "yahoo": 5000},
				allowed: []string{"db", "ht", "mh"}, followups: true}),
			planDispatch(dispatchSpec{lane: "consumer", tier: 2,
				desired: map[string]int{"aol": 4000},
				allowed: []string{"db", "ht", "mh"}, followups: true}),
			planDispatch(dispatchSpec{lane: "wcl_remail", tier: 2,
				desired: map[string]int{"aol": 6000},
				allowed: []string{"db"}, followups: true}),
			planDispatch(dispatchSpec{lane: "dead_lane", tier: 2,
				desired: map[string]int{"aol": 2000},
				allowed: []string{"ht"}, followups: true}),
			planDispatch(dispatchSpec{lane: "probe_v9", tier: ExplorationTier,
				desired: map[string]int{"aol": 3000},
				allowed: []string{"db", "ht"}, exploration: 0.05, followups: true}),
		},
		[]*InventoryContract{
			planInventory("refi_heloc", false, 0.25),
			planInventory("consumer", false, 0.25),
			planInventory("wcl_remail", true, 0.25),
			planInventory("dead_lane", false, 0.25),
			planInventory("probe_v9", false, 0.25),
		})

	in.FreshMailable = map[LaneISP]int{
		{"refi_heloc", "aol"}:   5000,
		{"refi_heloc", "yahoo"}: 6000,
		{"consumer", "aol"}:     900,
		{"wcl_remail", "aol"}:   1000,
		{"dead_lane", "aol"}:    5000,
		{"probe_v9", "aol"}:     400,
	}
	in.PendingEO = map[LaneISP]int{{"refi_heloc", "aol"}: 2000}
	in.RemailEligible = map[LaneISP]int{{"wcl_remail", "aol"}: 50000}
	in.FollowupsDue = map[LaneISP]int{
		{"refi_heloc", "aol"}: 3000,
		{"consumer", "aol"}:   1000,
	}
	in.FollowupsDueByHour = map[LaneISP][24]int{
		{"refi_heloc", "aol"}: hours(map[int]int{2: 3000}),
		{"consumer", "aol"}:   hours(map[int]int{5: 1000}),
	}
	in.Ranks = map[string]LaneRank{
		"refi_heloc": {ContributionECPM: 9.40, Mature: true},
		"consumer":   {ContributionECPM: 6.20, Mature: true},
		"wcl_remail": {ContributionECPM: 4.10, Mature: true},
		"dead_lane":  {ContributionECPM: -1.20, Mature: true},
		"probe_v9":   {ContributionECPM: 0, Mature: false},
	}
	return in
}

func goldenPath() string { return filepath.Join("testdata", "golden_plan.json") }

func TestAssign_GoldenPlan(t *testing.T) {
	// The golden encodes Denver-local timestamps. Without tzdata testLoc falls
	// back to UTC and every timestamp in the file shifts — skip rather than
	// report a false failure (or, worse, let -update-golden bake UTC in).
	if testLoc(t).String() != "America/Denver" {
		t.Skip("America/Denver tzdata unavailable — the golden's timestamps cannot be reproduced")
	}
	got := assign(goldenInputs(t))
	buf, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	buf = append(buf, '\n')
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath(), buf, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("golden rewritten — re-run without -update-golden and REVIEW the diff by hand")
	}
	want, err := os.ReadFile(goldenPath())
	if err != nil {
		t.Fatalf("read golden: %v (run with -update-golden only after checking the numbers by hand)", err)
	}
	if string(buf) != string(want) {
		t.Errorf("assign() output does not match the hand-computed golden.\n--- got ---\n%s\n--- want ---\n%s", buf, want)
	}
}

// TestAssign_GoldenPlanCanFail is the golden file's negative control. A golden
// test that cannot fail proves nothing; this perturbs ONE contract value and
// asserts the golden rejects it.
func TestAssign_GoldenPlanCanFail(t *testing.T) {
	in := goldenInputs(t)
	in.Contracts.Dispatches["refi_heloc"].MaxIntroShare = 1.0
	buf, err := json.MarshalIndent(assign(in), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(goldenPath())
	if err != nil {
		t.Skipf("golden missing: %v", err)
	}
	if string(append(buf, '\n')) == string(want) {
		t.Fatal("perturbing refi_heloc.max_intro_share still matched the golden — the golden file is not pinning anything")
	}
}

// TestAssign_Deterministic runs the same fixture repeatedly. Go randomizes map
// iteration order on every range, so a planner that leaks map order produces a
// different plan on some run — which is exactly the defect that makes an
// incident un-replayable.
func TestAssign_Deterministic(t *testing.T) {
	first, err := json.Marshal(assign(goldenInputs(t)))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		got, err := json.Marshal(assign(goldenInputs(t)))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(first) {
			t.Fatalf("run %d differs from run 0 — assign() depends on map iteration order", i)
		}
	}
}

// -----------------------------------------------------------------------------
// Scarce domain (§2.5 step 5, the WP6 DoD case)
// -----------------------------------------------------------------------------

func scarceInputs(t *testing.T, bOnAllDomains bool) Inputs {
	t.Helper()
	in := blankInputs(t)
	var doms []*DomainContract
	var brands []string
	doms = append(doms, planDomain("db", gDB, map[string]int{"aol": 1000}))
	brands = append(brands, "db")
	for i := 1; i <= 13; i++ {
		b := fmt.Sprintf("x%02d", i)
		doms = append(doms, planDomain(b, fmt.Sprintf("em.x%02d.com", i), map[string]int{"aol": 1000}))
		brands = append(brands, b)
	}
	bAllowed := []string{"db"}
	if bOnAllDomains {
		bAllowed = append([]string(nil), brands...)
	}
	in.Contracts = newSet(in.Day, doms,
		[]*DispatchContract{
			// A is flexible: 14 domains, ranks FIRST on contribution.
			planDispatch(dispatchSpec{lane: "lane_a", tier: 1,
				desired: map[string]int{"aol": 14000}, allowed: brands, introShare: 1.0}),
			// B is inflexible: db only, ranks SECOND. Also profitable.
			planDispatch(dispatchSpec{lane: "lane_b", tier: 1,
				desired: map[string]int{"aol": 1000}, allowed: bAllowed, introShare: 1.0}),
		},
		[]*InventoryContract{planInventory("lane_a", false, 0.25), planInventory("lane_b", false, 0.25)})
	in.FreshMailable = map[LaneISP]int{{"lane_a", "aol"}: 14000, {"lane_b", "aol"}: 1000}
	in.Ranks = map[string]LaneRank{
		"lane_a": {ContributionECPM: 9.0, Mature: true},
		"lane_b": {ContributionECPM: 4.0, Mature: true},
	}
	return in
}

func TestAssign_ScarceDomainNotStarved(t *testing.T) {
	p := assign(scarceInputs(t, false))

	a := laneAward(t, p, "lane_a", "aol")
	b := laneAward(t, p, "lane_b", "aol")
	if a.Rank >= b.Rank {
		t.Fatalf("fixture broken: lane_a should outrank lane_b, got a=%d b=%d", a.Rank, b.Rank)
	}
	if b.AwardedFirm != 1000 {
		t.Errorf("lane_b (single-domain, profitable) was starved on %s: awarded_firm=%d, want 1000", gDB, b.AwardedFirm)
	}
	if b.Unserved != 0 {
		t.Errorf("lane_b unserved=%d (%s), want 0", b.Unserved, b.UnservedReason)
	}
	// A takes the other 13 domains and is short exactly the reserved 1000.
	if a.AwardedFirm != 13000 {
		t.Errorf("lane_a awarded_firm=%d, want 13000 (14 domains x 1000 minus the 1000 held for lane_b)", a.AwardedFirm)
	}
	if a.UnservedReason != UnservedScarcityReserved {
		t.Errorf("lane_a unserved reason=%q, want %q", a.UnservedReason, UnservedScarcityReserved)
	}
	if r := rowFor(t, p, "lane_b", "aol", gDB); r.AwardFirm != 1000 {
		t.Errorf("lane_b plan row on %s = %d, want 1000", gDB, r.AwardFirm)
	}
	for _, r := range p.Rows {
		if r.Lane == "lane_a" && r.SendingDomain == gDB && r.AwardFirm+r.AwardProvisional > 0 {
			t.Errorf("lane_a took %d from the scarce domain %s", r.AwardFirm+r.AwardProvisional, gDB)
		}
	}
}

// Negative control: when lane_b is ALSO allowed on all 14 domains it is no
// longer inflexible, nothing is reserved for it, and the higher-ranked lane_a
// legitimately drains the estate. If this test fails, the preservation rule is
// firing unconditionally rather than only for scarce cells.
func TestAssign_ScarceDomain_NegativeControl_FlexibleLaneIsNotPreserved(t *testing.T) {
	p := assign(scarceInputs(t, true))
	a := laneAward(t, p, "lane_a", "aol")
	b := laneAward(t, p, "lane_b", "aol")
	if a.AwardedFirm != 14000 {
		t.Errorf("lane_a awarded_firm=%d, want 14000 (nothing should be reserved when lane_b is flexible)", a.AwardedFirm)
	}
	if b.AwardedFirm != 0 {
		t.Errorf("lane_b awarded_firm=%d, want 0 — a flexible lane gets no scarcity preservation", b.AwardedFirm)
	}
}

// -----------------------------------------------------------------------------
// Follow-ups reserved first (§2.5 step 3)
// -----------------------------------------------------------------------------

func followupInputs(t *testing.T, committed bool) Inputs {
	in := blankInputs(t)
	in.Contracts = newSet(in.Day,
		[]*DomainContract{planDomain("db", gDB, map[string]int{"aol": 1000})},
		[]*DispatchContract{planDispatch(dispatchSpec{lane: "lane_a", tier: 1,
			desired: map[string]int{"aol": 1000}, allowed: []string{"db"},
			introShare: 1.0, followups: committed})},
		[]*InventoryContract{planInventory("lane_a", false, 0.25)})
	in.FreshMailable = map[LaneISP]int{{"lane_a", "aol"}: 1000}
	in.FollowupsDue = map[LaneISP]int{{"lane_a", "aol"}: 400}
	in.FollowupsDueByHour = map[LaneISP][24]int{{"lane_a", "aol"}: hours(map[int]int{3: 400})}
	in.Ranks = map[string]LaneRank{"lane_a": {ContributionECPM: 5, Mature: true}}
	return in
}

func TestAssign_FollowupsReservedBeforeIntros(t *testing.T) {
	p := assign(followupInputs(t, true))
	a := laneAward(t, p, "lane_a", "aol")
	if a.FollowupsReserved != 400 {
		t.Errorf("followups_reserved=%d, want 400", a.FollowupsReserved)
	}
	if a.AwardedFirm != 600 {
		t.Errorf("awarded_firm=%d, want 600 (1000 capacity minus the 400 follow-up obligation)", a.AwardedFirm)
	}
	if a.Unserved != 400 || a.UnservedReason != UnservedFollowupReserve {
		t.Errorf("unserved=%d reason=%q, want 400 / %q", a.Unserved, a.UnservedReason, UnservedFollowupReserve)
	}
	if r := rowFor(t, p, "lane_a", "aol", gDB); r.FollowupsReserved != 400 {
		t.Errorf("plan row followups_reserved=%d, want 400", r.FollowupsReserved)
	}
}

// Negative control: followups_committed=false means the lane's due follow-ups
// are NOT an obligation, so they reserve nothing and intros take the whole day.
func TestAssign_FollowupsNegativeControl_UncommittedReserveNothing(t *testing.T) {
	p := assign(followupInputs(t, false))
	a := laneAward(t, p, "lane_a", "aol")
	if a.FollowupsReserved != 0 {
		t.Errorf("followups_reserved=%d, want 0 when followups_committed=false", a.FollowupsReserved)
	}
	if a.AwardedFirm != 1000 || a.Unserved != 0 {
		t.Errorf("awarded_firm=%d unserved=%d, want 1000/0", a.AwardedFirm, a.Unserved)
	}
}

// -----------------------------------------------------------------------------
// max_intro_share (§5.4)
// -----------------------------------------------------------------------------

func introShareInputs(t *testing.T, share float64) Inputs {
	in := blankInputs(t)
	in.Contracts = newSet(in.Day,
		[]*DomainContract{planDomain("db", gDB, map[string]int{"aol": 1000})},
		[]*DispatchContract{planDispatch(dispatchSpec{lane: "lane_a", tier: 1,
			desired: map[string]int{"aol": 1000}, allowed: []string{"db"}, introShare: share})},
		[]*InventoryContract{planInventory("lane_a", false, 0.25)})
	in.FreshMailable = map[LaneISP]int{{"lane_a", "aol"}: 1000}
	in.Ranks = map[string]LaneRank{"lane_a": {ContributionECPM: 5, Mature: true}}
	return in
}

func TestAssign_MaxIntroShareCapsOneLaneOnOneDomain(t *testing.T) {
	a := laneAward(t, assign(introShareInputs(t, 0.40)), "lane_a", "aol")
	if a.AwardedFirm != 400 {
		t.Errorf("awarded_firm=%d, want 400 (0.40 x 1000)", a.AwardedFirm)
	}
	if a.Unserved != 600 || a.UnservedReason != UnservedMaxIntroShare {
		t.Errorf("unserved=%d reason=%q, want 600 / %q", a.Unserved, a.UnservedReason, UnservedMaxIntroShare)
	}
}

// Negative control: share 1.0 takes the whole domain. If this fails, the cap is
// being applied from somewhere other than the contract.
func TestAssign_MaxIntroShareNegativeControl_FullShareTakesEverything(t *testing.T) {
	a := laneAward(t, assign(introShareInputs(t, 1.0)), "lane_a", "aol")
	if a.AwardedFirm != 1000 || a.Unserved != 0 {
		t.Errorf("awarded_firm=%d unserved=%d, want 1000/0 at max_intro_share=1.0", a.AwardedFirm, a.Unserved)
	}
}

// -----------------------------------------------------------------------------
// Exploration (§5.3)
// -----------------------------------------------------------------------------

func explorationInputs(t *testing.T, tier int, share float64) Inputs {
	in := blankInputs(t)
	in.Contracts = newSet(in.Day,
		[]*DomainContract{planDomain("db", gDB, map[string]int{"aol": 10000})},
		[]*DispatchContract{planDispatch(dispatchSpec{lane: "probe_v9", tier: tier,
			desired: map[string]int{"aol": 3000}, allowed: []string{"db"},
			introShare: 1.0, exploration: share})},
		[]*InventoryContract{planInventory("probe_v9", false, 0.25)})
	in.FreshMailable = map[LaneISP]int{{"probe_v9", "aol"}: 3000}
	in.Ranks = map[string]LaneRank{"probe_v9": {}}
	return in
}

func TestAssign_Tier9CappedByExplorationShare(t *testing.T) {
	p := assign(explorationInputs(t, ExplorationTier, 0.05))
	a := laneAward(t, p, "probe_v9", "aol")
	if a.AwardedFirm != 500 {
		t.Errorf("awarded_firm=%d, want 500 (0.05 x 10000 intro tokens)", a.AwardedFirm)
	}
	if a.Unserved != 2500 || a.UnservedReason != UnservedExplorationCap {
		t.Errorf("unserved=%d reason=%q, want 2500 / %q", a.Unserved, a.UnservedReason, UnservedExplorationCap)
	}
	if len(p.Ranked) != 1 || !p.Ranked[0].Exploration {
		t.Fatalf("tier-9 lane not marked as exploration: %+v", p.Ranked)
	}
}

// Negative control: the SAME lane at tier 2 with the same exploration_share is
// not an exploration lane, so the cap does not apply and it takes its demand.
func TestAssign_ExplorationNegativeControl_NonTier9IgnoresTheShare(t *testing.T) {
	a := laneAward(t, assign(explorationInputs(t, 2, 0.05)), "probe_v9", "aol")
	if a.AwardedFirm != 3000 || a.Unserved != 0 {
		t.Errorf("awarded_firm=%d unserved=%d, want 3000/0 — exploration_share must only bind tier 9", a.AwardedFirm, a.Unserved)
	}
}

// -----------------------------------------------------------------------------
// Negative contribution (§5.3)
// -----------------------------------------------------------------------------

func negativeLaneInputs(t *testing.T, mature bool) Inputs {
	in := blankInputs(t)
	in.Contracts = newSet(in.Day,
		[]*DomainContract{planDomain("db", gDB, map[string]int{"aol": 5000})},
		[]*DispatchContract{planDispatch(dispatchSpec{lane: "dead_lane", tier: 2,
			desired: map[string]int{"aol": 1000}, allowed: []string{"db"},
			introShare: 1.0, followups: true})},
		[]*InventoryContract{planInventory("dead_lane", false, 0.25)})
	in.FreshMailable = map[LaneISP]int{{"dead_lane", "aol"}: 1000}
	in.FollowupsDue = map[LaneISP]int{{"dead_lane", "aol"}: 500}
	in.FollowupsDueByHour = map[LaneISP][24]int{{"dead_lane", "aol"}: hours(map[int]int{1: 500})}
	in.Ranks = map[string]LaneRank{"dead_lane": {ContributionECPM: -1.5, Mature: mature}}
	return in
}

func TestAssign_NegativeContributionGetsNoDiscretionaryIntros(t *testing.T) {
	p := assign(negativeLaneInputs(t, true))
	a := laneAward(t, p, "dead_lane", "aol")
	if a.AwardedCapacity != 0 || a.AwardedFirm != 0 || a.AwardedProvisional != 0 {
		t.Errorf("negative lane was awarded intros: capacity=%d firm=%d prov=%d",
			a.AwardedCapacity, a.AwardedFirm, a.AwardedProvisional)
	}
	if a.UnservedReason != UnservedNegativeContribution {
		t.Errorf("unserved reason=%q, want %q", a.UnservedReason, UnservedNegativeContribution)
	}
	// §5.3: "their follow-ups still run unless paused". Follow-ups are reserved
	// in step 3, BEFORE ranking, precisely so this rule cannot strand a ladder.
	if a.FollowupsReserved != 500 {
		t.Errorf("followups_reserved=%d, want 500 — a blocked lane still owes its ladder", a.FollowupsReserved)
	}
	if r := rowFor(t, p, "dead_lane", "aol", gDB); r.AwardFirm != 0 || r.FollowupsReserved != 500 {
		t.Errorf("plan row = firm %d / followups %d, want 0 / 500", r.AwardFirm, r.FollowupsReserved)
	}
}

// Negative control: an IMMATURE negative number is absence of evidence, not
// evidence of loss, and must not block the lane.
func TestAssign_NegativeContributionNegativeControl_ImmatureIsNeutral(t *testing.T) {
	a := laneAward(t, assign(negativeLaneInputs(t, false)), "dead_lane", "aol")
	if a.AwardedFirm != 1000 {
		t.Errorf("awarded_firm=%d, want 1000 — an immature lane must rank as neutral, not negative", a.AwardedFirm)
	}
	if a.UnservedReason == UnservedNegativeContribution {
		t.Error("an immature lane was blocked for negative contribution")
	}
}

// -----------------------------------------------------------------------------
// Unserved reasons (§2.5 step 7)
// -----------------------------------------------------------------------------

func TestAssign_UnservedRecordedWithBindingReason(t *testing.T) {
	base := func(mut func(*Inputs)) Plan {
		in := blankInputs(t)
		in.Contracts = newSet(in.Day,
			[]*DomainContract{planDomain("db", gDB, map[string]int{"aol": 1000, "yahoo": 1000})},
			[]*DispatchContract{planDispatch(dispatchSpec{lane: "lane_a", tier: 1,
				desired: map[string]int{"aol": 5000}, allowed: []string{"db"}, introShare: 1.0})},
			[]*InventoryContract{planInventory("lane_a", false, 0.25)})
		in.FreshMailable = map[LaneISP]int{{"lane_a", "aol"}: 5000}
		in.Ranks = map[string]LaneRank{"lane_a": {ContributionECPM: 5, Mature: true}}
		mut(&in)
		return assign(in)
	}

	t.Run("domain_capacity", func(t *testing.T) {
		a := laneAward(t, base(func(*Inputs) {}), "lane_a", "aol")
		if a.Unserved != 4000 || a.UnservedReason != UnservedDomainCapacity {
			t.Errorf("got %d/%q want 4000/%q", a.Unserved, a.UnservedReason, UnservedDomainCapacity)
		}
	})

	t.Run("supply", func(t *testing.T) {
		// Capacity is enough for everything the lane asked for; mailable stock is not.
		p := base(func(in *Inputs) {
			in.Contracts.Domains[gDB].DailyMaxByISP["aol"] = 5000
			in.FreshMailable[LaneISP{"lane_a", "aol"}] = 1200
		})
		a := laneAward(t, p, "lane_a", "aol")
		if a.AwardedCapacity != 5000 {
			t.Fatalf("fixture: awarded_capacity=%d, want 5000", a.AwardedCapacity)
		}
		if a.AwardedFirm != 1200 || a.Unserved != 3800 || a.UnservedReason != UnservedSupply {
			t.Errorf("got firm=%d unserved=%d/%q want 1200/3800/%q",
				a.AwardedFirm, a.Unserved, a.UnservedReason, UnservedSupply)
		}
		if got := p.SupplyReleased[DomainISP{Domain: gDB, ISP: "aol"}]; got != 3800 {
			t.Errorf("supply_released=%d, want 3800 — capacity the supply check could not back must be visible", got)
		}
	})

	t.Run("isp_excluded", func(t *testing.T) {
		p := base(func(in *Inputs) {
			in.Contracts.Dispatches["lane_a"].ISPExclusions = []string{"aol"}
		})
		a := laneAward(t, p, "lane_a", "aol")
		if a.AwardedCapacity != 0 || a.UnservedReason != UnservedISPExcluded {
			t.Errorf("got capacity=%d reason=%q want 0/%q", a.AwardedCapacity, a.UnservedReason, UnservedISPExcluded)
		}
	})

	t.Run("no_allowed_domain", func(t *testing.T) {
		p := base(func(in *Inputs) {
			in.Contracts.Dispatches["lane_a"].AllowedDomains = []string{"nope"}
		})
		a := laneAward(t, p, "lane_a", "aol")
		if a.AwardedCapacity != 0 || a.UnservedReason != UnservedNoAllowedDomain {
			t.Errorf("got capacity=%d reason=%q want 0/%q", a.AwardedCapacity, a.UnservedReason, UnservedNoAllowedDomain)
		}
	})

	t.Run("daily_ceiling", func(t *testing.T) {
		c := 700
		p := base(func(in *Inputs) {
			in.Contracts.Domains[gDB].DailyMaxByISP["aol"] = 5000
			in.Contracts.Dispatches["lane_a"].DemandMode = DemandModeConsumeAvailable
			in.Contracts.Dispatches["lane_a"].DailyCeiling = &c
		})
		a := laneAward(t, p, "lane_a", "aol")
		if a.AwardedCapacity != 700 || a.UnservedReason != UnservedDailyCeiling {
			t.Errorf("got capacity=%d reason=%q want 700/%q", a.AwardedCapacity, a.UnservedReason, UnservedDailyCeiling)
		}
	})

	// Negative control for the whole table: a lane that gets everything it asked
	// for records NO reason. A reason vocabulary that always fires is noise.
	t.Run("negative_control_served_lane_has_no_reason", func(t *testing.T) {
		p := base(func(in *Inputs) {
			in.Contracts.Domains[gDB].DailyMaxByISP["aol"] = 9000
		})
		a := laneAward(t, p, "lane_a", "aol")
		if a.Unserved != 0 || a.UnservedReason != UnservedNone {
			t.Errorf("fully served lane reported unserved=%d reason=%q", a.Unserved, a.UnservedReason)
		}
	})
}

// -----------------------------------------------------------------------------
// Rank source fallback
// -----------------------------------------------------------------------------

func TestAssign_NoRankSourceFallsBackToOperatorTier(t *testing.T) {
	in := blankInputs(t)
	in.RankSourceWired = false
	in.Contracts = newSet(in.Day,
		[]*DomainContract{planDomain("db", gDB, map[string]int{"aol": 10000})},
		[]*DispatchContract{
			planDispatch(dispatchSpec{lane: "b_tier1", tier: 1, desired: map[string]int{"aol": 1000}, allowed: []string{"db"}, introShare: 1.0}),
			planDispatch(dispatchSpec{lane: "a_tier3", tier: 3, desired: map[string]int{"aol": 1000}, allowed: []string{"db"}, introShare: 1.0}),
		},
		[]*InventoryContract{planInventory("b_tier1", false, 0.25), planInventory("a_tier3", false, 0.25)})
	in.FreshMailable = map[LaneISP]int{{"b_tier1", "aol"}: 1000, {"a_tier3", "aol"}: 1000}

	p := assign(in)
	if len(p.Ranked) != 2 {
		t.Fatalf("want 2 ranked lanes, got %d", len(p.Ranked))
	}
	if p.Ranked[0].Lane != "b_tier1" {
		t.Errorf("rank 1 = %s, want b_tier1 — tier must still order lanes without economics", p.Ranked[0].Lane)
	}
	for _, r := range p.Ranked {
		if !strings.Contains(r.Reason, "unranked(no_rank_source)") {
			t.Errorf("rank reason %q does not disclose the missing rank source", r.Reason)
		}
	}
}

// Negative control: with a rank source wired, the reason must NOT claim the
// source is missing, and contribution must reorder equal tiers.
func TestAssign_RankSourceNegativeControl_WiredSourceReorders(t *testing.T) {
	in := blankInputs(t)
	in.Contracts = newSet(in.Day,
		[]*DomainContract{planDomain("db", gDB, map[string]int{"aol": 10000})},
		[]*DispatchContract{
			planDispatch(dispatchSpec{lane: "a_lane", tier: 2, desired: map[string]int{"aol": 1000}, allowed: []string{"db"}, introShare: 1.0}),
			planDispatch(dispatchSpec{lane: "z_lane", tier: 2, desired: map[string]int{"aol": 1000}, allowed: []string{"db"}, introShare: 1.0}),
		},
		[]*InventoryContract{planInventory("a_lane", false, 0.25), planInventory("z_lane", false, 0.25)})
	in.FreshMailable = map[LaneISP]int{{"a_lane", "aol"}: 1000, {"z_lane", "aol"}: 1000}
	in.Ranks = map[string]LaneRank{
		"a_lane": {ContributionECPM: 1.0, Mature: true},
		"z_lane": {ContributionECPM: 9.0, Mature: true},
	}
	p := assign(in)
	if p.Ranked[0].Lane != "z_lane" {
		t.Errorf("rank 1 = %s, want z_lane — mature contribution must beat lane name", p.Ranked[0].Lane)
	}
	if strings.Contains(p.Ranked[0].Reason, "no_rank_source") {
		t.Errorf("reason %q claims no rank source while one is wired", p.Ranked[0].Reason)
	}
}

// -----------------------------------------------------------------------------
// apportion
// -----------------------------------------------------------------------------

func TestApportion(t *testing.T) {
	cases := []struct {
		name    string
		total   int
		weights []int
		caps    []int
		want    []int
	}{
		{"exact split", 3000, []int{10000, 5000, 5000}, []int{10000, 5000, 5000}, []int{1500, 750, 750}},
		{"largest remainder, ties to lower index", 10, []int{1, 1, 1}, []int{10, 10, 10}, []int{4, 3, 3}},
		{"clamped share spills to the next", 100, []int{50, 50}, []int{10, 200}, []int{10, 90}},
		{"total above total room is clamped", 500, []int{1, 1}, []int{10, 10}, []int{10, 10}},
		{"zero weights fall back to capacity", 60, []int{0, 0}, []int{40, 80}, []int{20, 40}},
		{"nothing to give", 0, []int{5, 5}, []int{5, 5}, []int{0, 0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := apportion(c.total, append([]int(nil), c.weights...), append([]int(nil), c.caps...))
			if len(got) != len(c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
			sum := 0
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v want %v", got, c.want)
				}
				if got[i] > c.caps[i] {
					t.Fatalf("entry %d = %d exceeds its cap %d", i, got[i], c.caps[i])
				}
				sum += got[i]
			}
			room := 0
			for _, v := range c.caps {
				room += v
			}
			if want := min(c.total, room); sum != want {
				t.Fatalf("placed %d, want %d", sum, want)
			}
		})
	}
}

// Negative control: apportion must never invent capacity.
func TestApportion_NegativeControl_NeverExceedsRoom(t *testing.T) {
	got := apportion(1_000_000, []int{1, 2, 3}, []int{1, 2, 3})
	sum := 0
	for _, v := range got {
		sum += v
	}
	if sum != 6 {
		t.Fatalf("apportion placed %d against 6 units of room: %v", sum, got)
	}
}

// -----------------------------------------------------------------------------
// Database tests: freeze / replan idempotency and PlanRemaining
// -----------------------------------------------------------------------------

// newPlanTestDB is newTestDB plus drip_daily_plan.
func newPlanTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := newTestDB(t)
	if _, err := db.Exec(DailyPlanDDL); err != nil {
		t.Fatalf("drip_daily_plan ddl: %v", err)
	}
	return db
}

func storeGolden(t *testing.T, db *sql.DB) (*Planner, Inputs, Plan) {
	t.Helper()
	in := goldenInputs(t)
	p := NewPlanner(WithPlannerClock(func() time.Time { return in.Now }))
	plan := assign(in)
	if err := p.store(context.Background(), db, in.Contracts, &plan); err != nil {
		t.Fatalf("store: %v", err)
	}
	return p, in, plan
}

func TestPlanner_FrozenDayIsReadBackAndNotRecomputed(t *testing.T) {
	db := newPlanTestDB(t)
	_, in, plan := storeGolden(t, db)
	// No contract token key: reading a frozen plan must not need one. The whole
	// point of the freeze is that the day's decision is already made.
	t.Setenv(contractmeta.KeyEnvVar, "")

	// The contract tables do not exist in this scratch schema. If Plan() were to
	// recompute it would have to call LoadActive and would fail — so a clean
	// return here is proof the frozen day short-circuited BEFORE the read phase.
	p := NewPlanner(WithPlannerClock(func() time.Time { return in.Now.Add(time.Hour) }))
	got, err := p.Plan(context.Background(), db, in.Day, false)
	if err != nil {
		t.Fatalf("Plan on a frozen day: %v", err)
	}
	if !got.FromStore {
		t.Error("Plan did not report FromStore on a frozen day")
	}
	if len(got.Rows) != len(plan.Rows) {
		t.Errorf("read back %d rows, stored %d", len(got.Rows), len(plan.Rows))
	}
	if !got.FrozenAt.Equal(plan.FrozenAt.Truncate(time.Microsecond)) &&
		got.FrozenAt.Sub(plan.FrozenAt).Abs() > time.Millisecond {
		t.Errorf("frozen_at moved: %s -> %s", plan.FrozenAt, got.FrozenAt)
	}
	// Nothing was written: a second call must not have bumped any frozen_at.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM drip_daily_plan WHERE day = $1::date AND frozen_at <> $2`,
		dayKey(in.Day), plan.FrozenAt).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d rows had their frozen_at rewritten by a no-op Plan call", n)
	}
}

// Negative control: replan=true MUST recompute. In this schema that means it
// reaches the read phase and fails on the absent contract tables. A silent
// success would mean the replan flag is inert.
func TestPlanner_ReplanNegativeControl_ActuallyRecomputes(t *testing.T) {
	db := newPlanTestDB(t)
	_, in, _ := storeGolden(t, db)
	p := NewPlanner(
		WithPlannerClock(func() time.Time { return in.Now }),
		WithContractTokenKey(testTokenKey()),
	)
	_, err := p.Plan(context.Background(), db, in.Day, true)
	if err == nil {
		t.Fatal("replan=true returned the stored plan instead of recomputing")
	}
	if errors.Is(err, contractmeta.ErrNoKey) {
		t.Fatalf("test is asserting the wrong failure — it stopped at the token gate, not the recompute: %v", err)
	}
}

func testTokenKey() []byte { return []byte(strings.Repeat("k", contractmeta.MinKeyLen)) }

// TestPlanner_FailsClosedWithoutAContractTokenKey pins the WP2 addendum
// (10bcb0f) at the planner boundary: with no key the planner refuses to plan
// rather than governing a day from contracts whose integrity token it cannot
// verify. An out-of-band UPDATE to a contract row must not be able to set the
// estate's volume.
func TestPlanner_FailsClosedWithoutAContractTokenKey(t *testing.T) {
	db := newPlanTestDB(t)
	_, in, _ := storeGolden(t, db)
	t.Setenv(contractmeta.KeyEnvVar, "")

	p := NewPlanner(WithPlannerClock(func() time.Time { return in.Now }))
	_, err := p.Plan(context.Background(), db, in.Day, true)
	if err == nil {
		t.Fatal("planner planned a day with no contract token key")
	}
	if !errors.Is(err, contractmeta.ErrNoKey) {
		t.Errorf("error does not unwrap to contractmeta.ErrNoKey: %v", err)
	}
	if !strings.Contains(err.Error(), contractmeta.KeyEnvVar) {
		t.Errorf("error does not name %s, so an operator cannot act on it: %v", contractmeta.KeyEnvVar, err)
	}
}

// Negative control: an INJECTED key gets past the token gate and the run fails
// on the next thing instead (the absent contract tables in this scratch
// schema). Without this, a planner that refused everything unconditionally
// would pass the test above.
func TestPlanner_ContractKeyNegativeControl_InjectedKeyPassesTheGate(t *testing.T) {
	db := newPlanTestDB(t)
	_, in, _ := storeGolden(t, db)
	t.Setenv(contractmeta.KeyEnvVar, "")

	p := NewPlanner(
		WithPlannerClock(func() time.Time { return in.Now }),
		WithContractTokenKey(testTokenKey()),
	)
	_, err := p.Plan(context.Background(), db, in.Day, true)
	if err == nil {
		t.Fatal("fixture: the recompute should still fail on the absent contract tables")
	}
	if errors.Is(err, contractmeta.ErrNoKey) {
		t.Fatalf("the injected key was ignored and the env was consulted anyway: %v", err)
	}
	if !strings.Contains(err.Error(), "load contracts") {
		t.Errorf("expected the failure to be the contract READ, got: %v", err)
	}
}

// A short key is a misconfiguration, not a weak-but-usable key.
func TestPlanner_ShortEnvKeyIsRefused(t *testing.T) {
	db := newPlanTestDB(t)
	_, in, _ := storeGolden(t, db)
	t.Setenv(contractmeta.KeyEnvVar, "tooshort")
	p := NewPlanner(WithPlannerClock(func() time.Time { return in.Now }))
	if _, err := p.Plan(context.Background(), db, in.Day, true); !errors.Is(err, contractmeta.ErrNoKey) {
		t.Errorf("a %d-byte key was accepted (or failed for another reason): %v", len("tooshort"), err)
	}
}

func TestPlanner_StoreIsIdempotentAndPreservesLedgerCounters(t *testing.T) {
	db := newPlanTestDB(t)
	p, in, plan := storeGolden(t, db)

	// The executor has already spent some of refi_heloc/aol's award.
	if _, err := db.Exec(`
		UPDATE drip_lane_balance SET reserved = 400, committed = 1100
		WHERE day = $1::date AND lane = 'refi_heloc' AND isp = 'aol'
	`, dayKey(in.Day)); err != nil {
		t.Fatal(err)
	}

	if err := p.store(context.Background(), db, in.Contracts, &plan); err != nil {
		t.Fatalf("second store: %v", err)
	}

	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM drip_daily_plan WHERE day = $1::date`, dayKey(in.Day)).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != len(plan.Rows) {
		t.Errorf("re-store produced %d rows, want %d — the day must be replaced, not appended to", rows, len(plan.Rows))
	}

	var reserved, committed, unfilled, firm, prov int
	if err := db.QueryRow(`
		SELECT reserved, committed, unfilled, awarded_firm, awarded_provisional
		FROM drip_lane_balance WHERE day = $1::date AND lane = 'refi_heloc' AND isp = 'aol'
	`, dayKey(in.Day)).Scan(&reserved, &committed, &unfilled, &firm, &prov); err != nil {
		t.Fatal(err)
	}
	if reserved != 400 || committed != 1100 {
		t.Errorf("replan clobbered the ledger counters: reserved=%d committed=%d, want 400/1100", reserved, committed)
	}
	if firm != 5000 || prov != 1700 {
		t.Errorf("awarded_firm/provisional = %d/%d, want 5000/1700", firm, prov)
	}
	if want := firm + prov - reserved - committed; unfilled != want {
		t.Errorf("unfilled=%d, want %d (award minus what is already out)", unfilled, want)
	}
}

func TestPlanRemaining(t *testing.T) {
	db := newPlanTestDB(t)
	_, in, _ := storeGolden(t, db)
	ctx := context.Background()
	store := PlanStore{}

	req := func(domain, isp, lane, touch string) ReserveReq {
		return ReserveReq{Day: in.Day, Domain: domain, ISP: isp, Lane: lane,
			TouchClass: touch, WaveKey: "w1", Requested: 1}
	}

	// refi_heloc / aol / db: firm 3000 + provisional 1020 = 4020, nothing spent.
	got, bounded, err := store.PlanRemaining(ctx, db, req(gDB, "aol", "refi_heloc", "intro"))
	if err != nil {
		t.Fatal(err)
	}
	if !bounded || got != 4020 {
		t.Errorf("intro remaining = %d bounded=%t, want 4020/true", got, bounded)
	}

	// A grant of 1000, committed at 600 (so released 400): the plan has consumed
	// 600, NOT 1600. This is the arithmetic the one-line brief got wrong.
	insertLedger(t, db, in.Day, gDB, "aol", "refi_heloc", "intro", "k-intro", 1000, 600, 400, StatusCommitted)
	got, _, err = store.PlanRemaining(ctx, db, req(gDB, "aol", "refi_heloc", "intro"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 3420 {
		t.Errorf("intro remaining after a 1000 grant committed at 600 = %d, want 3420 (4020-600)", got)
	}

	// Follow-ups read the follow-up ceiling, not the intro award, and the intro
	// ledger row must not touch it.
	got, bounded, err = store.PlanRemaining(ctx, db, req(gDB, "aol", "refi_heloc", "followup"))
	if err != nil {
		t.Fatal(err)
	}
	if !bounded || got != 1500 {
		t.Errorf("followup remaining = %d bounded=%t, want 1500/true (followups_reserved on that row)", got, bounded)
	}
	insertLedger(t, db, in.Day, gDB, "aol", "refi_heloc", "followup", "k-fu", 500, 0, 0, StatusReserved)
	got, _, err = store.PlanRemaining(ctx, db, req(gDB, "aol", "refi_heloc", "followup"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 1000 {
		t.Errorf("followup remaining after a 500 reservation = %d, want 1000", got)
	}
	// ...and the intro side is unchanged by the follow-up reservation.
	got, _, _ = store.PlanRemaining(ctx, db, req(gDB, "aol", "refi_heloc", "intro"))
	if got != 3420 {
		t.Errorf("a follow-up reservation moved the intro remaining to %d, want 3420", got)
	}

	// Over-consumption floors at 0 rather than going negative.
	insertLedger(t, db, in.Day, gDB, "aol", "refi_heloc", "intro", "k-big", 99999, 0, 0, StatusReserved)
	got, _, _ = store.PlanRemaining(ctx, db, req(gDB, "aol", "refi_heloc", "intro"))
	if got != 0 {
		t.Errorf("over-consumed cell returned %d, want 0", got)
	}
}

// Negative control: a cell with NO plan row is unbounded. WP3's Reserve drops
// the plan term entirely on bounded=false, which is what keeps a planner outage
// from stopping the estate; if this ever returned bounded=true with 0 the whole
// estate would go to zero silently.
func TestPlanRemaining_NegativeControl_NoRowIsUnbounded(t *testing.T) {
	db := newPlanTestDB(t)
	_, in, _ := storeGolden(t, db)
	got, bounded, err := PlanStore{}.PlanRemaining(context.Background(), db, ReserveReq{
		Day: in.Day, Domain: "em.nosuchdomain.com", ISP: "aol", Lane: "refi_heloc",
		TouchClass: "intro", WaveKey: "w1", Requested: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if bounded {
		t.Errorf("a cell with no plan row reported bounded=true (limit %d) — the plan must fail OPEN", got)
	}
	// And a lane that IS planned but on a domain it was not awarded is likewise
	// unbounded, not zero: the domain and lane balances are what stop it.
	unplanned := ReserveReq{
		Day: in.Day, Domain: gMH, ISP: "yahoo", Lane: "refi_heloc",
		TouchClass: "intro", WaveKey: "w1", Requested: 1,
	}
	if _, b, _ := (PlanStore{}).PlanRemaining(context.Background(), db, unplanned); b {
		t.Error("an unplanned domain x ISP cell reported bounded=true")
	}
}

func insertLedger(t *testing.T, db *sql.DB, day time.Time, domain, isp, lane, touch, key string, reserved, committed, released int, status string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO drip_capacity_ledger
			(allocation_id, idempotency_key, day, tick, sending_domain, isp, lane, touch_class,
			 domain_contract_version, dispatch_contract_version, requested, reserved, committed, released,
			 status, binding_reason, domain_balance_after, lane_unfilled_after)
		VALUES ($1, $2, $3::date, NOW(), $4, $5, $6, $7, 1, 1, $8, $8, $9, $10, $11, 'requested', 0, 0)
	`, uuid.New(), key, dayKey(day), domain, isp, lane, touch, reserved, committed, released, status); err != nil {
		t.Fatalf("insert ledger row: %v", err)
	}
}

// TestRunDailyPlanner_UsesTheDenverDay pins the wiring stub's day arithmetic:
// WP5 calls it at 00:05 MT and it must plan THAT Denver day, not a UTC one
// (00:05 MT is already 06:05 UTC on the same date, but 23:00 MT is the NEXT UTC
// date — planning the wrong day there would silently split a send-day in two).
func TestRunDailyPlanner_UsesTheDenverDay(t *testing.T) {
	loc := testLoc(t)
	late := time.Date(2026, 9, 10, 23, 30, 0, 0, loc)
	if got := DenverDay(late); !got.Equal(time.Date(2026, 9, 10, 0, 0, 0, 0, loc)) {
		t.Errorf("DenverDay(%s) = %s, want the 2026-09-10 Denver midnight", late, got)
	}
	if got := DenverDay(late.UTC()); !got.Equal(time.Date(2026, 9, 10, 0, 0, 0, 0, loc)) {
		t.Errorf("DenverDay of the same instant expressed in UTC = %s, want the 2026-09-10 Denver midnight", got)
	}
	// Negative control: the day AFTER the boundary is a different day.
	next := time.Date(2026, 9, 11, 0, 5, 0, 0, loc)
	if DenverDay(next).Equal(DenverDay(late)) {
		t.Error("DenverDay collapsed two different Denver days onto one")
	}
}

// -----------------------------------------------------------------------------
// WP8 adapter
// -----------------------------------------------------------------------------

// TestEconomicsRankSource_MaturityMapping pins the one judgement call in the
// WP8 adapter: a lane ranking on the INHERITED estate median has no measured
// contribution of its own, so a negative median must rank it low without
// tripping §5.3's "no discretionary intros" switch.
func TestEconomicsRankSource_MaturityMapping(t *testing.T) {
	cases := []struct {
		name       string
		in         RankInput
		wantMature bool
	}{
		{"own number, sample cleared", RankInput{ContributionECPM: 7.6, SampleOK: true}, true},
		{"own number, sample too small", RankInput{ContributionECPM: 7.6, SampleOK: false}, false},
		{"inherited estate median", RankInput{ContributionECPM: -2.0, SampleOK: false, Fallback: true}, false},
		{"inherited but flagged sample-ok", RankInput{ContributionECPM: -2.0, SampleOK: true, Fallback: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := LaneRank{
				ContributionECPM: c.in.ContributionECPM,
				Mature:           c.in.SampleOK && !c.in.Fallback,
				Messages:         c.in.Messages,
				Conversions:      c.in.Conversions,
			}
			if got.Mature != c.wantMature {
				t.Fatalf("Mature=%t, want %t", got.Mature, c.wantMature)
			}
			// The behavioural consequence, not just the flag.
			in := negativeLaneInputs(t, got.Mature)
			in.Ranks = map[string]LaneRank{"dead_lane": got}
			a := laneAward(t, assign(in), "dead_lane", "aol")
			blocked := a.UnservedReason == UnservedNegativeContribution
			wantBlocked := c.wantMature && c.in.ContributionECPM < 0
			if blocked != wantBlocked {
				t.Errorf("blocked=%t, want %t (ecpm %.2f, mature %t)", blocked, wantBlocked, c.in.ContributionECPM, got.Mature)
			}
		})
	}
}

// TestAssign_FollowupsApportionByContractedCapacity pins the BASIS of §2.5
// step 3's split: "proportionally to each domain's capacity" means the
// CONTRACTED capacity, not whatever is left after an earlier lane's follow-ups.
// The golden fixture cannot tell the two apart (its domains are 2:1:1 either
// way), so this fixture makes them differ: lane_a is db-only and takes 400 of
// db's 1000 first, leaving db at 600 against ht's 1000.
//
//	contracted basis (correct): 1000:1000 -> lane_b gets db 200, ht 200
//	remaining  basis (wrong)  :  600:1000 -> lane_b gets db 150, ht 250
func TestAssign_FollowupsApportionByContractedCapacity(t *testing.T) {
	in := blankInputs(t)
	in.Contracts = newSet(in.Day,
		[]*DomainContract{
			planDomain("db", gDB, map[string]int{"aol": 1000}),
			planDomain("ht", gHT, map[string]int{"aol": 1000}),
		},
		[]*DispatchContract{
			planDispatch(dispatchSpec{lane: "lane_a", tier: 1, desired: map[string]int{"aol": 1},
				allowed: []string{"db"}, introShare: 1.0, followups: true}),
			planDispatch(dispatchSpec{lane: "lane_b", tier: 1, desired: map[string]int{"aol": 1},
				allowed: []string{"db", "ht"}, introShare: 1.0, followups: true}),
		},
		[]*InventoryContract{planInventory("lane_a", false, 0.25), planInventory("lane_b", false, 0.25)})
	in.FollowupsDue = map[LaneISP]int{{"lane_a", "aol"}: 400, {"lane_b", "aol"}: 400}
	in.FollowupsDueByHour = map[LaneISP][24]int{
		{"lane_a", "aol"}: hours(map[int]int{1: 400}), // earlier deadline: runs first
		{"lane_b", "aol"}: hours(map[int]int{5: 400}),
	}
	in.Ranks = map[string]LaneRank{
		"lane_a": {ContributionECPM: 5, Mature: true},
		"lane_b": {ContributionECPM: 5, Mature: true},
	}

	p := assign(in)
	if got := rowFor(t, p, "lane_a", "aol", gDB).FollowupsReserved; got != 400 {
		t.Fatalf("fixture: lane_a should take all 400 on %s, got %d", gDB, got)
	}
	gotDB := rowFor(t, p, "lane_b", "aol", gDB).FollowupsReserved
	gotHT := rowFor(t, p, "lane_b", "aol", gHT).FollowupsReserved
	if gotDB != 200 || gotHT != 200 {
		t.Errorf("lane_b follow-ups split %d/%d across %s/%s, want 200/200 — the basis must be CONTRACTED capacity (1000:1000), not remaining (600:1000, which gives 150/250)",
			gotDB, gotHT, gDB, gHT)
	}
}

// Negative control for the same rule: the split must still RESPECT remaining
// capacity as a cap. A domain that an earlier lane drained to 100 cannot take
// its full contracted share, and the surplus spills to the domain that can.
func TestAssign_FollowupsNegativeControl_RemainingIsStillACap(t *testing.T) {
	in := blankInputs(t)
	in.Contracts = newSet(in.Day,
		[]*DomainContract{
			planDomain("db", gDB, map[string]int{"aol": 1000}),
			planDomain("ht", gHT, map[string]int{"aol": 1000}),
		},
		[]*DispatchContract{
			planDispatch(dispatchSpec{lane: "lane_a", tier: 1, desired: map[string]int{"aol": 1},
				allowed: []string{"db"}, introShare: 1.0, followups: true}),
			planDispatch(dispatchSpec{lane: "lane_b", tier: 1, desired: map[string]int{"aol": 1},
				allowed: []string{"db", "ht"}, introShare: 1.0, followups: true}),
		},
		[]*InventoryContract{planInventory("lane_a", false, 0.25), planInventory("lane_b", false, 0.25)})
	in.FollowupsDue = map[LaneISP]int{{"lane_a", "aol"}: 900, {"lane_b", "aol"}: 400}
	in.FollowupsDueByHour = map[LaneISP][24]int{
		{"lane_a", "aol"}: hours(map[int]int{1: 900}),
		{"lane_b", "aol"}: hours(map[int]int{5: 400}),
	}
	in.Ranks = map[string]LaneRank{
		"lane_a": {ContributionECPM: 5, Mature: true},
		"lane_b": {ContributionECPM: 5, Mature: true},
	}
	p := assign(in)
	gotDB := rowFor(t, p, "lane_b", "aol", gDB).FollowupsReserved
	gotHT := rowFor(t, p, "lane_b", "aol", gHT).FollowupsReserved
	if gotDB != 100 || gotHT != 300 {
		t.Errorf("lane_b follow-ups split %d/%d, want 100/300 — the contracted share (200) must clamp to the 100 left on %s and spill the rest",
			gotDB, gotHT, gDB)
	}
	if gotDB+gotHT != 400 {
		t.Errorf("lane_b placed %d of its 400 due follow-ups", gotDB+gotHT)
	}
}
