package dripsupply

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/contractmeta"
	"github.com/lib/pq"
)

// planner.go — WP6, the daily capacity planner (§2.5).
//
// Shape of this file, and why:
//
//	Plan()      does ALL reads first, into an Inputs struct, then calls assign()
//	            and writes the result in ONE transaction.
//	assign()    is pure: Inputs in, Plan out, no clock, no database, no map-order
//	            dependence. Every test that pins the algorithm exercises assign()
//	            with a fixture; the golden file is its output.
//
// The split exists because a planner that mixes reads into its assignment loop
// cannot be tested without a database and cannot be replayed after an incident.
//
// Re-run safety (§8 DoD, and the standing rule that every scheduler re-fires on
// every ECS bounce): the day's plan is FROZEN. Plan() on an already-frozen day
// reads the stored rows back and writes NOTHING. Only an explicit replan=true
// recomputes, and that recompute is a single transaction — delete, insert,
// upsert the lane balances — so a crash mid-write leaves the previous frozen
// plan intact rather than a half-planned day.

// -----------------------------------------------------------------------------
// Injected sources (WP7 yield, WP8 economics)
// -----------------------------------------------------------------------------

const (
	// SeedEOYield is §2.6's seed for VALIDATION_VALID / VALIDATION_ORDERED,
	// used until WP7 supplies the measured 14-day rolling value per ISP.
	SeedEOYield = 0.85
	// MinEOYield is §2.6's floor. A measured yield below it is nearly always a
	// broken verdict feed rather than genuinely bad data, and trusting it would
	// silently collapse every provisional award on the estate.
	MinEOYield = 0.5
)

// LaneRank is one lane's economics input to the §5.2 ranking. WP8 computes it
// (`economics.go`); the planner only consumes it.
//
// Mature=false means the lane has no cohort old enough for the §4 seven-day
// attribution window, or is under the minimum sample. An immature lane ranks as
// 0.0 — NEUTRAL, never negative — because §5.3's "negative contribution gets no
// discretionary intros" must not fire on absence of evidence.
type LaneRank struct {
	ContributionECPM float64 `json:"contribution_ecpm"`
	Mature           bool    `json:"mature"`
	Messages         int     `json:"messages,omitempty"`
	Conversions      int     `json:"conversions,omitempty"`
}

// Effective is the value the ranking sorts on: an immature lane contributes 0.
func (r LaneRank) Effective() float64 {
	if !r.Mature {
		return 0
	}
	return r.ContributionECPM
}

// RankSource supplies §6.1's mature dispatch contribution eCPM per lane.
//
// WP8 owns the computation and exposes `RankInputs(ctx, db, day)`; this
// interface is deliberately shaped to that signature so wiring it is one line
// (`WithRankSource(RankSourceFunc(economics.RankInputs))`) and WP6 never has to
// reach into WP8's file. Nothing in this package implements it against the
// database — that is WP8's job.
type RankSource interface {
	RankInputs(ctx context.Context, db Queryer, day time.Time) (map[string]LaneRank, error)
}

// RankSourceFunc adapts a bare function to RankSource.
type RankSourceFunc func(ctx context.Context, db Queryer, day time.Time) (map[string]LaneRank, error)

// RankInputs implements RankSource.
func (f RankSourceFunc) RankInputs(ctx context.Context, db Queryer, day time.Time) (map[string]LaneRank, error) {
	return f(ctx, db, day)
}

// StaticRankSource is a fixed table, for tests and for a deliberate operator
// override. It never touches the database.
type StaticRankSource map[string]LaneRank

// RankInputs implements RankSource.
func (s StaticRankSource) RankInputs(context.Context, Queryer, time.Time) (map[string]LaneRank, error) {
	return map[string]LaneRank(s), nil
}

// EconomicsRankSource adapts WP8's package-level RankInputs (economics.go) to
// RankSource. It is the wiring WP5 should inject:
//
//	NewPlanner(WithRankSource(EconomicsRankSource{}), ...)
//
// The one judgement call is Mature. §5.3 blocks a lane's discretionary intros on
// a NEGATIVE MATURE contribution, and §4 says a lane under the minimum sample
// "inherits the estate median". A lane ranking on a borrowed median has no
// measured contribution of its own, so Mature is SampleOK AND NOT Fallback:
// an inherited negative median must rank a lane low, never switch it off.
type EconomicsRankSource struct{}

// RankInputs implements RankSource on top of economics.go's RankInputs.
func (EconomicsRankSource) RankInputs(ctx context.Context, db Queryer, day time.Time) (map[string]LaneRank, error) {
	raw, err := RankInputs(ctx, db, day)
	if err != nil {
		return nil, err
	}
	out := make(map[string]LaneRank, len(raw))
	for lane, r := range raw {
		out[lane] = LaneRank{
			ContributionECPM: r.ContributionECPM,
			Mature:           r.SampleOK && !r.Fallback,
			Messages:         r.Messages,
			Conversions:      r.Conversions,
		}
	}
	return out, nil
}

var _ RankSource = EconomicsRankSource{}

// YieldSource supplies §2.6's per-ISP EO verdict yield. WP7 owns the measured
// rolling computation; until it is wired every ISP takes SeedEOYield.
type YieldSource interface {
	Yields(ctx context.Context, db Queryer, day time.Time) (map[string]float64, error)
}

// YieldSourceFunc adapts a bare function to YieldSource.
type YieldSourceFunc func(ctx context.Context, db Queryer, day time.Time) (map[string]float64, error)

// Yields implements YieldSource.
func (f YieldSourceFunc) Yields(ctx context.Context, db Queryer, day time.Time) (map[string]float64, error) {
	return f(ctx, db, day)
}

// StaticYieldSource is a fixed per-ISP yield table, for tests.
type StaticYieldSource map[string]float64

// Yields implements YieldSource.
func (s StaticYieldSource) Yields(context.Context, Queryer, time.Time) (map[string]float64, error) {
	return map[string]float64(s), nil
}

// -----------------------------------------------------------------------------
// Keys
// -----------------------------------------------------------------------------

// LaneISP keys everything the planner reads per lane × ISP.
type LaneISP struct {
	Lane string `json:"lane"`
	ISP  string `json:"isp"`
}

// DomainISP keys the capacity side: one sending domain × ISP.
type DomainISP struct {
	Domain string `json:"domain"`
	ISP    string `json:"isp"`
}

// MarshalText lets a map keyed by LaneISP serialize as "lane|isp".
// encoding/json refuses a struct map key outright, and these maps are what the
// golden fixture, the §3 API and any incident replay are made of.
func (k LaneISP) MarshalText() ([]byte, error) { return []byte(k.Lane + "|" + k.ISP), nil }

// UnmarshalText parses "lane|isp".
func (k *LaneISP) UnmarshalText(b []byte) error {
	lane, isp, ok := strings.Cut(string(b), "|")
	if !ok {
		return fmt.Errorf("dripsupply: bad LaneISP key %q, want lane|isp", b)
	}
	k.Lane, k.ISP = lane, isp
	return nil
}

// MarshalText lets a map keyed by DomainISP serialize as "domain|isp".
func (k DomainISP) MarshalText() ([]byte, error) { return []byte(k.Domain + "|" + k.ISP), nil }

// UnmarshalText parses "domain|isp".
func (k *DomainISP) UnmarshalText(b []byte) error {
	domain, isp, ok := strings.Cut(string(b), "|")
	if !ok {
		return fmt.Errorf("dripsupply: bad DomainISP key %q, want domain|isp", b)
	}
	k.Domain, k.ISP = domain, isp
	return nil
}

// -----------------------------------------------------------------------------
// Unserved / binding reasons (§2.5 step 7)
// -----------------------------------------------------------------------------

// Reasons a lane × ISP did not get everything it asked for. These are the
// operator-facing "binding constraint" strings in §6's Pane 1.
const (
	UnservedNone                 = ""
	UnservedZeroDesired          = "zero_desired"
	UnservedISPExcluded          = "isp_excluded"
	UnservedNoAllowedDomain      = "no_allowed_domain"
	UnservedNegativeContribution = "negative_contribution"
	UnservedMaxIntroShare        = "max_intro_share"
	UnservedScarcityReserved     = "scarcity_reserved"
	UnservedFollowupReserve      = "followup_reserve"
	UnservedDomainCapacity       = "domain_capacity"
	UnservedExplorationCap       = "exploration_cap"
	UnservedSupply               = "supply"
	UnservedDailyCeiling         = "daily_ceiling"
)

// -----------------------------------------------------------------------------
// Inputs — everything assign() is allowed to look at
// -----------------------------------------------------------------------------

// Inputs is the complete, immutable input to assign(). If a number is not here,
// assign() cannot see it: that is the property that makes the planner replayable
// from a fixture and the golden test meaningful.
type Inputs struct {
	// Day is the Denver day being planned (midnight, America/Denver).
	Day time.Time `json:"day"`
	// Now is the freeze instant (§2.5 step 8). Passed in so assign() has no clock.
	Now time.Time `json:"now"`

	// Contracts is the active set for Day. Lanes and domains without an active
	// contract are absent, and absence fails closed (§2.1).
	Contracts *ActiveSet `json:"-"`

	// FreshMailable is pcq status='ready', validated within the lane's inventory
	// contract verdict_valid_days, never mailed, touch_count=0 — the records that
	// can be introduced TODAY. It bounds the firm award (§2.5 step 6).
	FreshMailable map[LaneISP]int `json:"fresh_mailable"`

	// RemailEligible is populated only for lanes whose inventory contract has
	// remail_enabled, and bounds the remail half of the provisional award.
	RemailEligible map[LaneISP]int `json:"remail_eligible"`

	// PendingEO is records out for validation. PendingEO × yield is the other
	// half of the provisional award.
	PendingEO map[LaneISP]int `json:"pending_eo"`

	// FollowupsDue is the day's obligation: pcq status='mailed',
	// next_touch_at < day_end, engaged_at IS NULL, terminal_reason IS NULL.
	FollowupsDue map[LaneISP]int `json:"followups_due"`

	// FollowupsDueByHour is the same population split by the Denver hour of
	// next_touch_at. §5.2 rule 1 ranks due follow-ups BY DEADLINE, and this is
	// the only input that carries a deadline.
	FollowupsDueByHour map[LaneISP][24]int `json:"followups_due_by_hour"`

	// GovernorEffective / GovernorReason are the plan-time governor snapshot.
	// §2.5 step 2 is explicit that governors apply at TICK time, not plan time —
	// the plan records both and awards against `contracted`. This snapshot is
	// therefore diagnostic here, and is the input §5.5 needs so the supply
	// controller does not order cleaning for a cell that is governed to 0.
	GovernorEffective map[DomainISP]int    `json:"governor_effective,omitempty"`
	GovernorReason    map[DomainISP]string `json:"governor_reason,omitempty"`

	// Ranks is WP8's economics, per lane.
	Ranks map[string]LaneRank `json:"ranks"`
	// RankSourceWired is false when no RankSource was injected; the ranking then
	// degrades to operator tier only and says so in every rank_reason.
	RankSourceWired bool `json:"rank_source_wired"`

	// Yields is the per-ISP EO yield. A missing ISP takes SeedEOYield.
	Yields map[string]float64 `json:"yields"`

	// OldestIngest is the oldest ingested_at among a lane's mailable fresh
	// inventory — §5.2 rule 5's tie-break. A zero time sorts LAST (unknown age
	// never jumps the queue ahead of a lane with proven-old stock).
	OldestIngest map[string]time.Time `json:"oldest_ingest,omitempty"`
}

// -----------------------------------------------------------------------------
// Plan — what assign() produces and Plan() stores
// -----------------------------------------------------------------------------

// PlanRow is one drip_daily_plan row: day × lane × ISP × sending_domain.
type PlanRow struct {
	Day               time.Time `json:"day"`
	Lane              string    `json:"lane"`
	ISP               string    `json:"isp"`
	SendingDomain     string    `json:"sending_domain"`
	AwardFirm         int       `json:"award_firm"`
	AwardProvisional  int       `json:"award_provisional"`
	FollowupsReserved int       `json:"followups_reserved"`
	PlanShare         float64   `json:"plan_share"`
	Rank              int       `json:"rank"`
	RankReason        string    `json:"rank_reason"`
}

// LaneAward is the lane × ISP roll-up: what was wanted, what was awarded, what
// was not, and what bound it (§2.5 step 7).
type LaneAward struct {
	Lane               string `json:"lane"`
	ISP                string `json:"isp"`
	Desired            int    `json:"desired"`
	AwardedCapacity    int    `json:"awarded_capacity"` // step 5 result, before the supply check
	AwardedFirm        int    `json:"awarded_firm"`
	AwardedProvisional int    `json:"awarded_provisional"`
	FollowupsDue       int    `json:"followups_due"`
	FollowupsReserved  int    `json:"followups_reserved"`
	Unserved           int    `json:"unserved"`
	UnservedReason     string `json:"unserved_reason"`
	Rank               int    `json:"rank"`
}

// RankedLane is one lane's frozen rank and the reason for it (§2.5 step 8).
type RankedLane struct {
	Lane             string  `json:"lane"`
	Rank             int     `json:"rank"`
	Tier             int     `json:"tier"`
	ContributionECPM float64 `json:"contribution_ecpm"`
	Mature           bool    `json:"mature"`
	Exploration      bool    `json:"exploration"`
	Scarcity         int     `json:"scarcity"` // allowed domains carrying capacity today
	Reason           string  `json:"reason"`
	Blocked          string  `json:"blocked,omitempty"` // non-empty: no discretionary intros
}

// Plan is the frozen day.
type Plan struct {
	Day       time.Time    `json:"day"`
	FrozenAt  time.Time    `json:"frozen_at"`
	Rows      []PlanRow    `json:"rows"`
	Lanes     []LaneAward  `json:"lanes"`
	Ranked    []RankedLane `json:"ranked"`
	Replanned bool         `json:"replanned"`
	// FromStore is true when Plan() returned an already-frozen day untouched.
	FromStore bool `json:"from_store"`

	// GovernorZero lists the domain × ISP cells a governor had at 0 when the
	// plan froze, with the governor's name. §5.5: the supply controller must not
	// order cleaning for these unless the award is marked executable tomorrow.
	GovernorZero map[DomainISP]string `json:"governor_zero,omitempty"`

	// SupplyReleased is capacity awarded in step 5 that step 6's supply check
	// could not back. It is reported, not re-offered — see the note on assign().
	SupplyReleased map[DomainISP]int `json:"supply_released,omitempty"`
}

// TotalFirm / TotalProvisional / TotalUnserved are the estate roll-ups §6's
// Pane 1 header shows.
func (p *Plan) TotalFirm() int {
	n := 0
	for _, r := range p.Rows {
		n += r.AwardFirm
	}
	return n
}

// TotalProvisional sums every row's provisional award.
func (p *Plan) TotalProvisional() int {
	n := 0
	for _, r := range p.Rows {
		n += r.AwardProvisional
	}
	return n
}

// TotalFollowupsReserved sums every row's follow-up reservation.
func (p *Plan) TotalFollowupsReserved() int {
	n := 0
	for _, r := range p.Rows {
		n += r.FollowupsReserved
	}
	return n
}

// TotalUnserved sums the lane roll-ups.
func (p *Plan) TotalUnserved() int {
	n := 0
	for _, l := range p.Lanes {
		n += l.Unserved
	}
	return n
}

// -----------------------------------------------------------------------------
// assign — §2.5 steps 1-8, pure
// -----------------------------------------------------------------------------

// working state for one lane × ISP through steps 3-6.
type cell struct {
	lane, isp   string
	desired     int
	domains     []string // resolved sending domains, sorted, with an active contract
	awards      map[string]int
	followups   map[string]int
	firm        map[string]int
	provisional map[string]int

	tier             int
	maxIntroShare    float64
	explorationShare float64

	followupsDue      int
	followupsReserved int
	earliestDueHour   int

	awardedCapacity int
	unservedReason  string
	blocked         string
}

// KNOWN LIMITATION, reported to the lead rather than silently patched:
// §2.5 runs step 5 (assignment) before step 6 (the firm/provisional supply
// check). A lane can therefore be awarded domain capacity that supply cannot
// back, and the doc specifies no second pass to re-offer it. assign() follows
// the doc literally and records the shortfall in Plan.SupplyReleased so the
// operator can see idle capacity next to an unserved lane, instead of the
// planner quietly inventing a redistribution rule the design does not have.
func assign(in Inputs) Plan {
	out := Plan{
		Day:            dayOf(in.Day),
		FrozenAt:       in.Now,
		GovernorZero:   map[DomainISP]string{},
		SupplyReleased: map[DomainISP]int{},
	}
	if in.Contracts == nil {
		return out
	}

	// ---- step 2: capacity = contracted (governors apply at tick time) --------
	capacity := map[DomainISP]int{}
	remaining := map[DomainISP]int{}
	byBrand := map[string][]string{} // brand_code -> sending domains
	domainNames := make([]string, 0, len(in.Contracts.Domains))
	for name := range in.Contracts.Domains {
		domainNames = append(domainNames, name)
	}
	sort.Strings(domainNames)
	for _, name := range domainNames {
		d := in.Contracts.Domains[name]
		if d == nil {
			continue
		}
		byBrand[d.BrandCode] = append(byBrand[d.BrandCode], d.SendingDomain)
		for _, isp := range sortedKeys(d.DailyMaxByISP) {
			k := DomainISP{Domain: d.SendingDomain, ISP: normISP(isp)}
			v := d.DailyMaxByISP[isp]
			if v < 0 {
				v = 0
			}
			capacity[k] = v
			remaining[k] = v
			if eff, ok := in.GovernorEffective[k]; ok && eff <= 0 {
				name := in.GovernorReason[k]
				if strings.TrimSpace(name) == "" {
					name = "unnamed"
				}
				out.GovernorZero[k] = name
			}
		}
	}

	// ---- build one cell per lane x ISP --------------------------------------
	laneNames := make([]string, 0, len(in.Contracts.Dispatches))
	for l := range in.Contracts.Dispatches {
		laneNames = append(laneNames, l)
	}
	sort.Strings(laneNames)

	cells := map[LaneISP]*cell{}
	cellsByLane := map[string][]*cell{}
	for _, laneName := range laneNames {
		disp := in.Contracts.Dispatches[laneName]
		if disp == nil {
			continue
		}
		excluded := map[string]struct{}{}
		for _, e := range disp.ISPExclusions {
			excluded[normISP(e)] = struct{}{}
		}
		doms := resolveDomains(disp.AllowedDomains, byBrand, in.Contracts.Domains)
		for _, ispRaw := range sortedKeys(disp.DesiredDailyIntros) {
			isp := normISP(ispRaw)
			key := LaneISP{Lane: laneName, ISP: isp}
			c := &cell{
				lane:             laneName,
				isp:              isp,
				desired:          disp.DesiredDailyIntros[ispRaw],
				tier:             disp.OperatorPriorityTier,
				maxIntroShare:    disp.MaxIntroShare,
				explorationShare: disp.ExplorationShare,
				awards:           map[string]int{},
				followups:        map[string]int{},
				firm:             map[string]int{},
				provisional:      map[string]int{},
				followupsDue:     in.FollowupsDue[key],
				earliestDueHour:  earliestDueHour(in.FollowupsDueByHour[key]),
			}
			if c.desired < 0 {
				c.desired = 0
			}
			// The lane's domains for THIS isp: an allowed domain with no
			// capacity for the isp cannot serve it.
			for _, d := range doms {
				if capacity[DomainISP{Domain: d, ISP: isp}] > 0 {
					c.domains = append(c.domains, d)
				}
			}
			switch {
			case func() bool { _, ok := excluded[isp]; return ok }():
				c.blocked = UnservedISPExcluded
			case c.desired <= 0 && c.followupsDue <= 0:
				c.blocked = UnservedZeroDesired
			case len(c.domains) == 0:
				c.blocked = UnservedNoAllowedDomain
			}
			cells[key] = c
			cellsByLane[laneName] = append(cellsByLane[laneName], c)
		}
	}

	// ---- step 1 + 3: follow-ups are obligations, reserved FIRST --------------
	// Ordered by deadline (§5.2 rule 1): earliest due hour first, lane name as
	// the deterministic tie-break. A lane whose follow-up contract is off
	// (followups_committed=false) does not pre-empt capacity.
	fuOrder := make([]*cell, 0, len(cells))
	for _, laneName := range laneNames {
		disp := in.Contracts.Dispatches[laneName]
		if disp == nil || !disp.FollowupsCommitted {
			continue
		}
		for _, c := range cellsByLane[laneName] {
			if c.followupsDue > 0 && c.blocked != UnservedISPExcluded && len(c.domains) > 0 {
				fuOrder = append(fuOrder, c)
			}
		}
	}
	sort.SliceStable(fuOrder, func(i, j int) bool {
		a, b := fuOrder[i], fuOrder[j]
		if a.earliestDueHour != b.earliestDueHour {
			return a.earliestDueHour < b.earliestDueHour
		}
		if a.lane != b.lane {
			return a.lane < b.lane
		}
		return a.isp < b.isp
	})
	for _, c := range fuOrder {
		weights := make([]int, len(c.domains))
		caps := make([]int, len(c.domains))
		for i, d := range c.domains {
			k := DomainISP{Domain: d, ISP: c.isp}
			weights[i] = capacity[k] // proportional to CONTRACTED capacity (§2.5 step 3)
			caps[i] = remaining[k]
		}
		got := apportion(c.followupsDue, weights, caps)
		for i, d := range c.domains {
			if got[i] <= 0 {
				continue
			}
			c.followups[d] += got[i]
			c.followupsReserved += got[i]
			remaining[DomainISP{Domain: d, ISP: c.isp}] -= got[i]
		}
	}

	// ---- step 4: rank ------------------------------------------------------
	ranked := rankLanes(in, laneNames, cellsByLane)
	rankOf := map[string]int{}
	blockedLane := map[string]string{}
	reasonOf := map[string]string{}
	for _, r := range ranked {
		rankOf[r.Lane] = r.Rank
		reasonOf[r.Lane] = r.Reason
		if r.Blocked != "" {
			blockedLane[r.Lane] = r.Blocked
		}
	}
	out.Ranked = ranked

	// ---- step 5: global water-filling assignment ---------------------------
	// Contention is computed ONCE, from gated demand against post-follow-up
	// capacity, so the ordering does not churn as awards land — a planner whose
	// domain order changes mid-pass is not reproducible.
	demand := map[DomainISP]int{}
	for _, r := range ranked {
		if r.Exploration || r.Blocked != "" {
			continue
		}
		for _, c := range cellsByLane[r.Lane] {
			if c.blocked != "" || c.desired <= 0 {
				continue
			}
			for _, d := range c.domains {
				demand[DomainISP{Domain: d, ISP: c.isp}] += c.desired
			}
		}
	}
	contention := map[DomainISP]float64{}
	for k, v := range demand {
		den := remaining[k]
		if den < 1 {
			den = 1
		}
		contention[k] = float64(v) / float64(den)
	}

	// "Scarce domains preserved for inflexible lanes" (§2.5 step 5): a lane whose
	// ONLY capable domain for an ISP is d gets that much of d held back from the
	// flexible lanes, regardless of rank. Without this a tier-1 lane spread over
	// 14 domains legitimately drains the one domain a single-domain lane can use.
	soleReserve := map[DomainISP]int{}
	for _, r := range ranked {
		if r.Exploration || r.Blocked != "" {
			continue
		}
		for _, c := range cellsByLane[r.Lane] {
			if c.blocked != "" || c.desired <= 0 || len(c.domains) != 1 {
				continue
			}
			k := DomainISP{Domain: c.domains[0], ISP: c.isp}
			soleReserve[k] += min(c.desired, introShareCap(c.maxIntroShare, capacity[k]))
		}
	}
	for k, v := range soleReserve {
		if v > remaining[k] {
			soleReserve[k] = remaining[k]
		}
	}

	introUsed := map[DomainISP]map[string]int{}
	explorationUsed := map[DomainISP]int{}
	explorationCapAt := map[DomainISP]int{}
	for _, r := range ranked {
		if r.Exploration {
			for _, c := range cellsByLane[r.Lane] {
				for _, d := range c.domains {
					k := DomainISP{Domain: d, ISP: c.isp}
					if v := introShareCap(c.explorationShare, capacity[k]); v > explorationCapAt[k] {
						explorationCapAt[k] = v
					}
				}
			}
		}
	}

	// Main pass, then the exploration pass. Tier-9 lanes never take from the
	// main pool and never from the follow-up reserve (§5.3).
	for _, phase := range []bool{false, true} {
		for _, r := range ranked {
			if r.Exploration != phase {
				continue
			}
			if b := blockedLane[r.Lane]; b != "" {
				for _, c := range cellsByLane[r.Lane] {
					if c.blocked == "" && c.desired > 0 {
						c.blocked = b
					}
				}
				continue
			}
			disp := in.Contracts.Dispatches[r.Lane]
			laneBudget := math.MaxInt
			if disp != nil && disp.DemandMode == DemandModeConsumeAvailable && disp.DailyCeiling != nil {
				laneBudget = max(*disp.DailyCeiling, 0)
			}
			for _, c := range cellsByLane[r.Lane] {
				if c.blocked != "" || c.desired <= 0 {
					continue
				}
				need := c.desired
				if need > laneBudget {
					need = laneBudget
					c.unservedReason = UnservedDailyCeiling
				}
				flexible := len(c.domains) > 1
				order := append([]string(nil), c.domains...)
				sort.SliceStable(order, func(i, j int) bool {
					a := DomainISP{Domain: order[i], ISP: c.isp}
					b := DomainISP{Domain: order[j], ISP: c.isp}
					if contention[a] != contention[b] {
						return contention[a] < contention[b]
					}
					return order[i] < order[j]
				})

				var blockedByShare, blockedBySole, blockedByFollowup, blockedByExploration bool
				for _, d := range order {
					if need <= 0 {
						break
					}
					k := DomainISP{Domain: d, ISP: c.isp}
					avail := remaining[k]
					if flexible && soleReserve[k] > 0 {
						if avail-soleReserve[k] < avail {
							blockedBySole = blockedBySole || avail-soleReserve[k] <= 0
						}
						avail -= soleReserve[k]
					}
					if avail < 0 {
						avail = 0
					}
					if phase {
						// exploration: bounded by this lane's share of the
						// domain's intro tokens AND by the aggregate tier-9 cap.
						laneCap := introShareCap(c.explorationShare, capacity[k]) - explorationUsed[k]
						aggCap := explorationCapAt[k] - explorationUsed[k]
						expAvail := max(min(laneCap, aggCap), 0)
						// Only blame exploration_share when it ACTUALLY bound.
						// A tier-9 lane starved because the estate is full was
						// bound by capacity, and saying otherwise sends the
						// operator to raise a share that was never the problem.
						if expAvail < avail {
							blockedByExploration = true
						}
						avail = min(avail, expAvail)
					}
					if introUsed[k] == nil {
						introUsed[k] = map[string]int{}
					}
					room := introShareCap(c.maxIntroShare, capacity[k]) - introUsed[k][c.lane]
					if room < 0 {
						room = 0
					}
					if avail > 0 && room < avail && room < need {
						blockedByShare = true
					}
					take := min(need, min(avail, room))
					if take <= 0 {
						if remaining[k] <= 0 && c.followups[d] > 0 {
							blockedByFollowup = true
						}
						continue
					}
					c.awards[d] += take
					c.awardedCapacity += take
					remaining[k] -= take
					introUsed[k][c.lane] += take
					if phase {
						explorationUsed[k] += take
					}
					if !flexible && soleReserve[k] > 0 {
						soleReserve[k] = max(soleReserve[k]-take, 0)
					}
					need -= take
					if laneBudget != math.MaxInt {
						laneBudget -= take
					}
					// This lane's OWN follow-up obligation displaced its own
					// intros on this domain. Another lane's follow-ups
					// exhausting a shared domain is plain domain_capacity —
					// blaming §5.4 for that would point the operator at the
					// wrong lane's ladder.
					if remaining[k] <= 0 && c.followups[d] > 0 {
						blockedByFollowup = true
					}
				}
				if need > 0 && c.unservedReason == "" {
					switch {
					case blockedByExploration:
						c.unservedReason = UnservedExplorationCap
					case blockedByShare:
						c.unservedReason = UnservedMaxIntroShare
					case blockedBySole:
						c.unservedReason = UnservedScarcityReserved
					case blockedByFollowup:
						c.unservedReason = UnservedFollowupReserve
					default:
						c.unservedReason = UnservedDomainCapacity
					}
				}
			}
		}
	}

	// ---- step 6: firm / provisional split ----------------------------------
	for _, laneName := range laneNames {
		inv := in.Contracts.Inventories[laneName]
		for _, c := range cellsByLane[laneName] {
			if c.awardedCapacity <= 0 {
				continue
			}
			key := LaneISP{Lane: c.lane, ISP: c.isp}
			y := yieldFor(in.Yields, c.isp)
			firmCap := max(in.FreshMailable[key], 0)
			remailCredit := 0
			if inv != nil && inv.RemailEnabled {
				remailCredit = min(max(in.RemailEligible[key], 0),
					introShareCap(inv.MaxRemailShare, c.awardedCapacity))
			}
			provCap := int(math.Floor(float64(max(in.PendingEO[key], 0))*y)) + remailCredit

			firmTotal := min(c.awardedCapacity, firmCap)
			provTotal := min(c.awardedCapacity-firmTotal, max(provCap, 0))

			// Split both halves across the domains proportionally to each
			// domain's award, largest remainder, capped by the award itself.
			doms := make([]string, 0, len(c.awards))
			for d := range c.awards {
				doms = append(doms, d)
			}
			sort.Strings(doms)
			weights := make([]int, len(doms))
			for i, d := range doms {
				weights[i] = c.awards[d]
			}
			firmParts := apportion(firmTotal, weights, weights)
			rest := make([]int, len(doms))
			for i := range doms {
				rest[i] = weights[i] - firmParts[i]
			}
			provParts := apportion(provTotal, rest, rest)
			for i, d := range doms {
				c.firm[d] = firmParts[i]
				c.provisional[d] = provParts[i]
				if short := weights[i] - firmParts[i] - provParts[i]; short > 0 {
					out.SupplyReleased[DomainISP{Domain: d, ISP: c.isp}] += short
				}
			}
			if c.awardedCapacity-firmTotal-provTotal > 0 {
				// Supply bound the award. It overrides a capacity reason only
				// when it is the larger shortfall — the operator needs the
				// constraint that actually cost the most mail.
				capShort := c.desired - c.awardedCapacity
				if c.unservedReason == "" || c.awardedCapacity-firmTotal-provTotal >= capShort {
					c.unservedReason = UnservedSupply
				}
			}
		}
	}

	// ---- step 7 + 8: rows, roll-ups, freeze --------------------------------
	for _, laneName := range laneNames {
		for _, c := range cellsByLane[laneName] {
			firm, prov := 0, 0
			for _, v := range c.firm {
				firm += v
			}
			for _, v := range c.provisional {
				prov += v
			}
			unserved := c.desired - firm - prov
			if unserved < 0 {
				unserved = 0
			}
			reason := c.unservedReason
			if unserved > 0 && reason == "" {
				if c.blocked != "" {
					reason = c.blocked
				} else {
					reason = UnservedDomainCapacity
				}
			}
			if unserved == 0 {
				reason = UnservedNone
			}
			award := LaneAward{
				Lane:               c.lane,
				ISP:                c.isp,
				Desired:            c.desired,
				AwardedCapacity:    c.awardedCapacity,
				AwardedFirm:        firm,
				AwardedProvisional: prov,
				FollowupsDue:       c.followupsDue,
				FollowupsReserved:  c.followupsReserved,
				Unserved:           unserved,
				UnservedReason:     reason,
				Rank:               rankOf[c.lane],
			}
			if award.Desired == 0 && award.FollowupsDue == 0 && award.FollowupsReserved == 0 {
				continue
			}
			out.Lanes = append(out.Lanes, award)

			// One row per domain that carries anything.
			doms := map[string]struct{}{}
			for d := range c.firm {
				doms[d] = struct{}{}
			}
			for d := range c.provisional {
				doms[d] = struct{}{}
			}
			for d := range c.followups {
				doms[d] = struct{}{}
			}
			names := make([]string, 0, len(doms))
			for d := range doms {
				names = append(names, d)
			}
			sort.Strings(names)
			total := firm + prov
			for _, d := range names {
				rf, rp, rfu := c.firm[d], c.provisional[d], c.followups[d]
				if rf == 0 && rp == 0 && rfu == 0 {
					continue
				}
				share := 0.0
				if total > 0 {
					share = round4(float64(rf+rp) / float64(total))
				}
				out.Rows = append(out.Rows, PlanRow{
					Day:               out.Day,
					Lane:              c.lane,
					ISP:               c.isp,
					SendingDomain:     d,
					AwardFirm:         rf,
					AwardProvisional:  rp,
					FollowupsReserved: rfu,
					PlanShare:         share,
					Rank:              rankOf[c.lane],
					RankReason:        composeRankReason(reasonOf[c.lane], unserved, reason),
				})
			}
		}
	}
	sort.SliceStable(out.Lanes, func(i, j int) bool {
		if out.Lanes[i].Lane != out.Lanes[j].Lane {
			return out.Lanes[i].Lane < out.Lanes[j].Lane
		}
		return out.Lanes[i].ISP < out.Lanes[j].ISP
	})
	sort.SliceStable(out.Rows, func(i, j int) bool {
		a, b := out.Rows[i], out.Rows[j]
		if a.Lane != b.Lane {
			return a.Lane < b.Lane
		}
		if a.ISP != b.ISP {
			return a.ISP < b.ISP
		}
		return a.SendingDomain < b.SendingDomain
	})
	return out
}

// rankLanes implements §5.2: hard gates, then operator tier 1->3, then mature
// contribution descending, then domain scarcity, then inventory age. Tier-9
// (exploration) lanes are ranked AFTER every other lane and only ever draw from
// exploration_share (§5.3).
func rankLanes(in Inputs, laneNames []string, cellsByLane map[string][]*cell) []RankedLane {
	entries := make([]RankedLane, 0, len(laneNames))
	for _, laneName := range laneNames {
		disp := in.Contracts.Dispatches[laneName]
		if disp == nil {
			continue
		}
		open := false
		scarcity := 0
		seen := map[string]struct{}{}
		for _, c := range cellsByLane[laneName] {
			if c.blocked == "" && c.desired > 0 {
				open = true
			}
			for _, d := range c.domains {
				seen[d] = struct{}{}
			}
		}
		scarcity = len(seen)
		if !open {
			continue
		}
		r := in.Ranks[laneName]
		e := RankedLane{
			Lane:             laneName,
			Tier:             disp.OperatorPriorityTier,
			ContributionECPM: r.ContributionECPM,
			Mature:           r.Mature,
			Exploration:      disp.OperatorPriorityTier == ExplorationTier,
			Scarcity:         scarcity,
		}
		// §5.3: a lane with a MEASURED negative contribution and tier != 9 gets
		// no discretionary intros. Its follow-ups still run — they were reserved
		// in step 3, before ranking, exactly so this rule cannot strand them.
		if !e.Exploration && r.Mature && r.ContributionECPM < 0 {
			e.Blocked = UnservedNegativeContribution
		}
		if e.Exploration && disp.ExplorationShare <= 0 {
			e.Blocked = UnservedExplorationCap
		}
		entries = append(entries, e)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Exploration != b.Exploration {
			return !a.Exploration // exploration lanes last
		}
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		if a.Effective() != b.Effective() {
			return a.Effective() > b.Effective()
		}
		if a.Scarcity != b.Scarcity {
			return a.Scarcity < b.Scarcity // fewer allowed domains first
		}
		ai, bi := in.OldestIngest[a.Lane], in.OldestIngest[b.Lane]
		if !ai.Equal(bi) {
			// A zero time means "age unknown" and must sort LAST, not first.
			switch {
			case ai.IsZero():
				return false
			case bi.IsZero():
				return true
			}
			return ai.Before(bi)
		}
		return a.Lane < b.Lane
	})
	for i := range entries {
		entries[i].Rank = i + 1
		entries[i].Reason = rankReason(entries[i], in)
	}
	return entries
}

// Effective mirrors LaneRank.Effective for a ranked entry.
func (r RankedLane) Effective() float64 {
	if !r.Mature {
		return 0
	}
	return r.ContributionECPM
}

func rankReason(e RankedLane, in Inputs) string {
	contrib := fmt.Sprintf("$%.2f/1k", e.ContributionECPM)
	switch {
	case !in.RankSourceWired:
		contrib = "unranked(no_rank_source)"
	case !e.Mature:
		contrib = contrib + "(incomplete)"
	default:
		contrib = contrib + "(mature)"
	}
	parts := []string{
		fmt.Sprintf("tier=%d", e.Tier),
		"contribution=" + contrib,
		fmt.Sprintf("domains=%d", e.Scarcity),
	}
	if t, ok := in.OldestIngest[e.Lane]; ok && !t.IsZero() {
		parts = append(parts, "oldest_ingest="+t.Format("2006-01-02"))
	}
	if e.Exploration {
		parts = append(parts, "exploration")
	}
	if e.Blocked != "" {
		parts = append(parts, "blocked="+e.Blocked)
	}
	return strings.Join(parts, " ")
}

// composeRankReason folds the §2.5 step-7 unserved record into rank_reason.
//
// SCHEMA GAP, reported to the lead: drip_daily_plan (WP1) has no `unserved` /
// `unserved_reason` column, and WP6 is not permitted to edit main.go. The
// binding reason step 7 requires therefore rides in rank_reason, which is the
// only TEXT column on the row. It should become two real columns in a WP1
// addendum; until then this is the durable record.
func composeRankReason(base string, unserved int, reason string) string {
	if unserved <= 0 || reason == "" {
		return base
	}
	suffix := fmt.Sprintf("unserved=%d reason=%s", unserved, reason)
	if base == "" {
		return suffix
	}
	return base + " | " + suffix
}

// resolveDomains maps a dispatch contract's allowed_domains onto sending
// domains that have an ACTIVE domain contract.
//
// §1.1 says allowed_domains holds brand codes, so brand_code is matched first.
// A sending domain is also accepted, because the normalization job (WP11) writes
// this column and a mixed vocabulary must degrade to "resolves correctly", not
// to a silently empty domain list that starves the lane with no error.
func resolveDomains(allowed []string, byBrand map[string][]string, domains map[string]*DomainContract) []string {
	seen := map[string]struct{}{}
	for _, a := range allowed {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if ds, ok := byBrand[a]; ok {
			for _, d := range ds {
				seen[d] = struct{}{}
			}
			continue
		}
		if _, ok := domains[a]; ok {
			seen[a] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// introShareCap is floor(share x capacity), floored at 0. A share of 0 yields 0
// and a share >= 1 yields the whole capacity.
func introShareCap(share float64, capacity int) int {
	if capacity <= 0 || share <= 0 {
		return 0
	}
	if share >= 1 {
		return capacity
	}
	v := int(math.Floor(share * float64(capacity)))
	if v < 0 {
		return 0
	}
	return v
}

func yieldFor(yields map[string]float64, isp string) float64 {
	y, ok := yields[normISP(isp)]
	if !ok || y <= 0 {
		return SeedEOYield
	}
	if y < MinEOYield {
		return MinEOYield
	}
	if y > 1 {
		return 1
	}
	return y
}

func earliestDueHour(hours [24]int) int {
	for h, n := range hours {
		if n > 0 {
			return h
		}
	}
	return 24 // no hourly detail: sorts after every lane that has one
}

func round4(f float64) float64 { return math.Round(f*10000) / 10000 }

// apportion distributes `total` across `weights` by the largest-remainder
// method, clamps each share to `caps`, and redistributes the clamped surplus to
// entries that still have headroom (lowest index first). Deterministic: ties on
// the remainder resolve to the lower index, never to map order.
func apportion(total int, weights, caps []int) []int {
	n := len(weights)
	out := make([]int, n)
	if total <= 0 || n == 0 {
		return out
	}
	room := 0
	c := make([]int, n)
	for i := 0; i < n; i++ {
		c[i] = max(caps[i], 0)
		room += c[i]
	}
	if total > room {
		total = room
	}
	if total <= 0 {
		return out
	}
	basis := make([]int, n)
	sum := 0
	for i := 0; i < n; i++ {
		basis[i] = max(weights[i], 0)
		sum += basis[i]
	}
	if sum == 0 {
		// Nothing to weight by (every award is 0): fall back to capacity, so a
		// zero-weight call still places the total instead of returning zeros.
		copy(basis, c)
		sum = room
	}
	if sum == 0 {
		return out
	}
	type rem struct {
		i    int
		frac float64
	}
	rems := make([]rem, 0, n)
	placed := 0
	for i := 0; i < n; i++ {
		exact := float64(total) * float64(basis[i]) / float64(sum)
		fl := int(math.Floor(exact))
		out[i] = fl
		placed += fl
		rems = append(rems, rem{i: i, frac: exact - float64(fl)})
	}
	sort.SliceStable(rems, func(a, b int) bool {
		if rems[a].frac != rems[b].frac {
			return rems[a].frac > rems[b].frac
		}
		return rems[a].i < rems[b].i
	})
	for k := 0; placed < total && k < len(rems); k++ {
		out[rems[k].i]++
		placed++
	}
	// Clamp and redistribute. Bounded by n passes: each pass either places the
	// whole surplus or fills at least one entry to its cap.
	for pass := 0; pass <= n; pass++ {
		surplus := 0
		for i := 0; i < n; i++ {
			if out[i] > c[i] {
				surplus += out[i] - c[i]
				out[i] = c[i]
			}
		}
		if surplus == 0 {
			break
		}
		for i := 0; i < n && surplus > 0; i++ {
			h := c[i] - out[i]
			if h <= 0 {
				continue
			}
			give := min(h, surplus)
			out[i] += give
			surplus -= give
		}
		if surplus > 0 {
			break // no headroom anywhere; total was already clamped to room
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// Planner — reads, assign, write
// -----------------------------------------------------------------------------

// DailyPlanDDL is `drip_daily_plan` (§1.2). VERBATIM copy of the WP1 statement
// in cmd/server/main.go (runStartupMigrations, req118_create_drip_daily_plan),
// kept next to its readers for the same reason balance.go keeps its copies: the
// integration tests build the PRODUCTION shape, so a WP1/WP6 drift surfaces as a
// failing test here rather than as a 3am constraint violation. Keep byte-identical.
const DailyPlanDDL = `CREATE TABLE IF NOT EXISTS drip_daily_plan (
		day                 DATE NOT NULL,
		lane                TEXT NOT NULL,
		isp                 TEXT NOT NULL,
		sending_domain      TEXT NOT NULL,
		award_firm          INT NOT NULL DEFAULT 0,
		award_provisional   INT NOT NULL DEFAULT 0,
		followups_reserved  INT NOT NULL DEFAULT 0,
		plan_share          NUMERIC NOT NULL DEFAULT 0,
		rank                INT NOT NULL DEFAULT 0,
		rank_reason         TEXT NOT NULL DEFAULT '',
		frozen_at           TIMESTAMPTZ,
		PRIMARY KEY (day, lane, isp, sending_domain)
	)`

// maxPlanRows bounds one day's plan. The estate is ~30 lanes x 12 ISPs x ~29
// domains; anything past this is a contract-set defect (a lane allowed on every
// domain for every ISP) and must fail loudly rather than write 10^6 rows into a
// 5 s statement budget.
const maxPlanRows = 50000

// planInsertChunk keeps one multi-VALUES insert under Postgres' 65535-parameter
// ceiling (11 params per row).
const planInsertChunk = 200

// DefaultPlannerTimeout is the per-query budget for the read phase. The planner
// runs at 00:05 MT off the hot path, and its pcq aggregates are measurably
// slower than prod's 30 s statement_timeout allows: the fresh-inventory rollup
// measured 4.1 s on 2026-09-03 against the live 13.5M-row table, and degrades
// under load. The reads therefore run on a DEDICATED connection with this
// timeout set, so a slow night surfaces as a planner error and not as a
// silently-absent plan.
const DefaultPlannerTimeout = 120 * time.Second

// Planner is the daily capacity planner.
type Planner struct {
	ranks   RankSource
	yields  YieldSource
	gov     GovernorReader
	clock   func() time.Time
	timeout time.Duration

	// tokenKey is the HMAC key LoadActiveWithKey verifies every active contract
	// with (WP2 addendum 10bcb0f). It is NEVER logged.
	tokenKey []byte

	rankWarnOnce sync.Once
	keyWarnOnce  sync.Once
}

// PlannerOption configures a Planner.
type PlannerOption func(*Planner)

// WithRankSource injects WP8's economics. Without it the ranking degrades to
// operator tier only, and says so in every rank_reason.
func WithRankSource(r RankSource) PlannerOption { return func(p *Planner) { p.ranks = r } }

// WithYieldSource injects WP7's measured EO yield. Without it every ISP takes
// SeedEOYield.
func WithYieldSource(y YieldSource) PlannerOption { return func(p *Planner) { p.yields = y } }

// WithPlannerGovernors injects the WP3 governor stack for the plan-time snapshot.
func WithPlannerGovernors(g GovernorReader) PlannerOption { return func(p *Planner) { p.gov = g } }

// WithPlannerClock injects a clock (tests, deterministic replay).
func WithPlannerClock(f func() time.Time) PlannerOption { return func(p *Planner) { p.clock = f } }

// WithContractTokenKey injects the HMAC key the contract loader verifies active
// contracts with (WP2 addendum). WP5 resolves it once at boot with
// contractmeta.KeyFromEnv() and passes it here, rather than every subsystem
// re-reading the environment.
//
// Without it the planner falls back to the environment and, if that is unset or
// short, FAILS CLOSED: planning against contracts whose integrity token cannot
// be verified would let an out-of-band edit to a contract row govern live
// sending, which is the whole reason the token exists.
func WithContractTokenKey(k []byte) PlannerOption {
	return func(p *Planner) { p.tokenKey = append([]byte(nil), k...) }
}

// WithPlannerTimeout overrides the read-phase per-query budget.
func WithPlannerTimeout(d time.Duration) PlannerOption {
	return func(p *Planner) { p.timeout = d }
}

// NewPlanner builds a planner.
func NewPlanner(opts ...PlannerOption) *Planner {
	p := &Planner{clock: time.Now, timeout: DefaultPlannerTimeout}
	for _, o := range opts {
		if o != nil {
			o(p)
		}
	}
	return p
}

func (p *Planner) now() time.Time {
	if p.clock != nil {
		return p.clock()
	}
	return time.Now()
}

// contractKey resolves the contract integrity key: the injected one, else the
// environment. The error path is fail-closed on purpose — a planner that
// shrugged and loaded unverified contracts would hand an out-of-band row edit
// authority over the whole day's sending. The key itself is never logged.
func (p *Planner) contractKey() ([]byte, error) {
	if len(p.tokenKey) > 0 {
		return p.tokenKey, nil
	}
	key, err := contractmeta.KeyFromEnv()
	if err != nil {
		return nil, fmt.Errorf("dripsupply: planner cannot verify contract tokens: %w — inject the key with WithContractTokenKey or set %s (>= %d bytes); the planner refuses to plan against unverified contracts",
			err, contractmeta.KeyEnvVar, contractmeta.MinKeyLen)
	}
	p.keyWarnOnce.Do(func() {
		log.Printf("[DripPlanner] contract token key read from %s — WP5 should resolve it once at boot and inject it with WithContractTokenKey", contractmeta.KeyEnvVar)
	})
	return key, nil
}

// Plan computes and freezes the day's plan (§2.5).
//
// Idempotency: with replan=false a day that already carries frozen rows is READ
// BACK and returned unchanged — no recompute, no writes. This is what makes the
// 00:05 job safe against a double fire, an ECS bounce and an on-demand call in
// the same minute.
//
// With replan=true the day is recomputed and rewritten in ONE transaction
// (delete plan rows, insert plan rows, upsert lane balances), so a crash between
// any two statements rolls back to the previous frozen plan. drip_lane_balance
// keeps its reserved/committed counters across a replan — an intraday replan
// must not un-spend capacity the executor has already handed out.
func (p *Planner) Plan(ctx context.Context, db *sql.DB, day time.Time, replan bool) (*Plan, error) {
	if p == nil {
		return nil, errors.New("dripsupply: Plan called on a nil planner")
	}
	if db == nil {
		return nil, errors.New("dripsupply: Plan called with a nil db")
	}
	day = dayOf(day)

	if !replan {
		stored, found, err := LoadStoredPlan(ctx, db, day)
		if err != nil {
			return nil, err
		}
		if found {
			log.Printf("[DripPlanner] %s already frozen at %s (%d rows) — returning the stored plan; pass replan to recompute",
				dayKey(day), stored.FrozenAt.Format(time.RFC3339), len(stored.Rows))
			return stored, nil
		}
	}

	in, err := p.ReadInputs(ctx, db, day)
	if err != nil {
		return nil, err
	}
	plan := assign(in)
	plan.Replanned = replan
	if len(plan.Rows) > maxPlanRows {
		return nil, fmt.Errorf("dripsupply: planner produced %d rows for %s, above the %d bound — check allowed_domains on the dispatch contracts",
			len(plan.Rows), dayKey(day), maxPlanRows)
	}
	if err := p.store(ctx, db, in.Contracts, &plan); err != nil {
		return nil, err
	}
	if len(plan.Rows) == 0 {
		// A day with no rows cannot be told apart from a day that was never
		// planned (LoadStoredPlan keys off the presence of rows), so the next
		// call recomputes. That is safe — an empty plan means nothing is
		// awarded — but it must not be SILENT: it is also the shape of a
		// contract set that resolves to nothing.
		log.Printf("[DripPlanner] %s produced ZERO plan rows (%d lane cells seen) — no lane was awarded capacity; check allowed_domains, desired_daily_intros and the domain contracts. The day is NOT frozen and the next call will recompute.",
			dayKey(day), len(plan.Lanes))
	}
	log.Printf("[DripPlanner] %s frozen: %d rows, %d lane cells, firm=%d provisional=%d followups=%d unserved=%d replan=%t",
		dayKey(day), len(plan.Rows), len(plan.Lanes), plan.TotalFirm(), plan.TotalProvisional(),
		plan.TotalFollowupsReserved(), plan.TotalUnserved(), replan)
	return &plan, nil
}

// ReadInputs performs the whole read phase. It is exported so WP5 and the §3 API
// can render a dry-run plan ("what would 00:05 decide right now") without
// writing anything.
func (p *Planner) ReadInputs(ctx context.Context, db *sql.DB, day time.Time) (Inputs, error) {
	day = dayOf(day)
	in := Inputs{
		Day:                day,
		Now:                p.now(),
		FreshMailable:      map[LaneISP]int{},
		RemailEligible:     map[LaneISP]int{},
		PendingEO:          map[LaneISP]int{},
		FollowupsDue:       map[LaneISP]int{},
		FollowupsDueByHour: map[LaneISP][24]int{},
		GovernorEffective:  map[DomainISP]int{},
		GovernorReason:     map[DomainISP]string{},
		Ranks:              map[string]LaneRank{},
		Yields:             map[string]float64{},
		OldestIngest:       map[string]time.Time{},
	}

	// A dedicated connection so the elevated statement_timeout applies to every
	// read below and to nothing else. database/sql hands a pooled connection to
	// an arbitrary goroutine, so a bare `SET statement_timeout` would land on one
	// connection and the next query would run under prod's 30 s default.
	conn, err := db.Conn(ctx)
	if err != nil {
		return in, fmt.Errorf("dripsupply: planner connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("SET statement_timeout = %d", p.timeout.Milliseconds())); err != nil {
		return in, fmt.Errorf("dripsupply: planner statement_timeout: %w", err)
	}

	key, err := p.contractKey()
	if err != nil {
		return in, err
	}
	contracts, err := LoadActiveWithKey(ctx, conn, day, key)
	if err != nil {
		return in, fmt.Errorf("dripsupply: planner load contracts: %w", err)
	}
	in.Contracts = contracts

	lanes := make([]string, 0, len(contracts.Dispatches))
	for l := range contracts.Dispatches {
		lanes = append(lanes, l)
	}
	sort.Strings(lanes)
	if len(lanes) == 0 {
		log.Printf("[DripPlanner] %s: no active dispatch contracts — nothing to plan", dayKey(day))
		return in, nil
	}

	if err := p.readFresh(ctx, conn, day, contracts, lanes, &in); err != nil {
		return in, err
	}
	if err := p.readRemail(ctx, conn, day, contracts, lanes, &in); err != nil {
		return in, err
	}
	if err := p.readPendingEO(ctx, conn, lanes, &in); err != nil {
		return in, err
	}
	if err := p.readFollowups(ctx, conn, day, lanes, &in); err != nil {
		return in, err
	}
	if err := p.readGovernors(ctx, day, contracts, &in); err != nil {
		return in, err
	}

	if p.ranks != nil {
		r, err := p.ranks.RankInputs(ctx, conn, day)
		if err != nil {
			return in, fmt.Errorf("dripsupply: planner rank inputs: %w", err)
		}
		in.Ranks = r
		in.RankSourceWired = true
	} else {
		p.rankWarnOnce.Do(func() {
			log.Printf("[DripPlanner] no RankSource wired — ranking by operator tier ONLY; mature contribution, negative-lane suppression and the economics tie-break are all inert until WP8 is injected")
		})
	}
	if p.yields != nil {
		y, err := p.yields.Yields(ctx, conn, day)
		if err != nil {
			return in, fmt.Errorf("dripsupply: planner yields: %w", err)
		}
		in.Yields = y
	}
	return in, nil
}

// readFresh fills FreshMailable and OldestIngest.
//
// Query shape is deliberate. The correlated form
// (`validated_at >= now() - (l.valid_days||' days')::interval` joined to a VALUES
// list) measured 8.9 s on prod because the planner built a BitmapAnd against
// idx_pcq_validated_at (5.8M rows). Grouping lanes by their contract's
// verdict_valid_days and issuing one scalar-cutoff query per distinct value
// measured 4.1 s for the same lane set. Verified 2026-09-03 with EXPLAIN ANALYZE
// on the live database (read-only).
func (p *Planner) readFresh(ctx context.Context, conn *sql.Conn, day time.Time, c *ActiveSet, lanes []string, in *Inputs) error {
	byDays := map[int][]string{}
	for _, l := range lanes {
		days := 60
		if inv := c.Inventories[l]; inv != nil && inv.VerdictValidDays > 0 {
			days = inv.VerdictValidDays
		}
		byDays[days] = append(byDays[days], l)
	}
	for _, days := range sortedIntKeys(byDays) {
		group := byDays[days]
		sort.Strings(group)
		cutoff := dayOf(day).AddDate(0, 0, -days)
		rows, err := conn.QueryContext(ctx, `
			SELECT vertical, isp_family, COUNT(*) AS n, MIN(ingested_at) AS oldest
			FROM partner_clean_queue
			WHERE status = 'ready'
			  AND vertical = ANY($1)
			  AND mailed_at IS NULL
			  AND touch_count = 0
			  AND validated_at >= $2
			GROUP BY 1, 2
		`, pq.Array(group), cutoff)
		if err != nil {
			return fmt.Errorf("dripsupply: planner fresh inventory (%d day window): %w", days, err)
		}
		if err := scanLaneISPCounts(rows, func(lane, isp string, n int, oldest sql.NullTime) {
			in.FreshMailable[LaneISP{Lane: lane, ISP: normISP(isp)}] += n
			if oldest.Valid {
				cur, ok := in.OldestIngest[lane]
				if !ok || oldest.Time.Before(cur) {
					in.OldestIngest[lane] = oldest.Time
				}
			}
		}); err != nil {
			return fmt.Errorf("dripsupply: planner fresh inventory scan: %w", err)
		}
	}
	return nil
}

// readRemail fills RemailEligible for the lanes whose inventory contract enables
// remail. Lanes without it are not queried at all — an inventory contract that
// says remail_enabled=false must not produce remail credit by accident.
func (p *Planner) readRemail(ctx context.Context, conn *sql.Conn, day time.Time, c *ActiveSet, lanes []string, in *Inputs) error {
	byDays := map[int][]string{}
	for _, l := range lanes {
		inv := c.Inventories[l]
		if inv == nil || !inv.RemailEnabled {
			continue
		}
		d := inv.RemailAfterDays
		if d < 0 {
			d = 0
		}
		byDays[d] = append(byDays[d], l)
	}
	for _, days := range sortedIntKeys(byDays) {
		group := byDays[days]
		sort.Strings(group)
		cutoff := dayOf(day).AddDate(0, 0, -days)
		rows, err := conn.QueryContext(ctx, `
			SELECT vertical, isp_family, COUNT(*) AS n, NULL::timestamptz AS oldest
			FROM partner_clean_queue
			WHERE status = 'mailed'
			  AND vertical = ANY($1)
			  AND mailed_at < $2
			  AND engaged_at IS NULL
			  AND terminal_reason IS NULL
			GROUP BY 1, 2
		`, pq.Array(group), cutoff)
		if err != nil {
			return fmt.Errorf("dripsupply: planner remail inventory (%d day window): %w", days, err)
		}
		if err := scanLaneISPCounts(rows, func(lane, isp string, n int, _ sql.NullTime) {
			in.RemailEligible[LaneISP{Lane: lane, ISP: normISP(isp)}] += n
		}); err != nil {
			return fmt.Errorf("dripsupply: planner remail inventory scan: %w", err)
		}
	}
	return nil
}

func (p *Planner) readPendingEO(ctx context.Context, conn *sql.Conn, lanes []string, in *Inputs) error {
	rows, err := conn.QueryContext(ctx, `
		SELECT vertical, isp_family, COUNT(*) AS n, NULL::timestamptz AS oldest
		FROM partner_clean_queue
		WHERE status IN ('pending_eo', 'eo_in_flight')
		  AND vertical = ANY($1)
		GROUP BY 1, 2
	`, pq.Array(lanes))
	if err != nil {
		return fmt.Errorf("dripsupply: planner pending EO: %w", err)
	}
	return scanLaneISPCounts(rows, func(lane, isp string, n int, _ sql.NullTime) {
		in.PendingEO[LaneISP{Lane: lane, ISP: normISP(isp)}] += n
	})
}

// readFollowups fills FollowupsDue and FollowupsDueByHour. `next_touch_at <
// day_end` deliberately includes OVERDUE touches: a follow-up whose deadline
// passed yesterday is more due, not less, and dropping it is how a ladder
// silently stalls.
func (p *Planner) readFollowups(ctx context.Context, conn *sql.Conn, day time.Time, lanes []string, in *Inputs) error {
	dayEnd := dayOf(day).AddDate(0, 0, 1)
	rows, err := conn.QueryContext(ctx, `
		SELECT vertical,
		       isp_family,
		       GREATEST(0, LEAST(23, EXTRACT(HOUR FROM next_touch_at AT TIME ZONE 'America/Denver')::int)) AS hr,
		       COUNT(*) AS n
		FROM partner_clean_queue
		WHERE status = 'mailed'
		  AND vertical = ANY($1)
		  AND next_touch_at < $2
		  AND engaged_at IS NULL
		  AND terminal_reason IS NULL
		GROUP BY 1, 2, 3
	`, pq.Array(lanes), dayEnd)
	if err != nil {
		return fmt.Errorf("dripsupply: planner follow-ups due: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var lane, isp string
		var hr, n int
		if err := rows.Scan(&lane, &isp, &hr, &n); err != nil {
			return fmt.Errorf("dripsupply: planner follow-ups scan: %w", err)
		}
		k := LaneISP{Lane: lane, ISP: normISP(isp)}
		in.FollowupsDue[k] += n
		h := in.FollowupsDueByHour[k]
		if hr >= 0 && hr < 24 {
			h[hr] += n
		}
		in.FollowupsDueByHour[k] = h
	}
	return rows.Err()
}

// readGovernors takes the plan-time governor snapshot. It never changes an
// award (§2.5 step 2) — it is recorded so §5.5 can keep the supply controller
// from cleaning into a cell that is governed to 0.
func (p *Planner) readGovernors(ctx context.Context, day time.Time, c *ActiveSet, in *Inputs) error {
	if p.gov == nil {
		return nil
	}
	names := make([]string, 0, len(c.Domains))
	for n := range c.Domains {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		d := c.Domains[n]
		if d == nil {
			continue
		}
		w, err := WindowOf(d)
		if err != nil {
			return fmt.Errorf("dripsupply: planner window for %s: %w", n, err)
		}
		for _, isp := range sortedKeys(d.DailyMaxByISP) {
			ceilings, err := p.gov.Ceilings(ctx, day, d.SendingDomain, normISP(isp), w)
			if err != nil {
				// A governor that cannot be read must not silently read as "no
				// ceiling": that is the failure mode that lets a held lane mail.
				return fmt.Errorf("dripsupply: planner governor %s/%s: %w", d.SendingDomain, isp, err)
			}
			eff, boundBy := ApplyGovernors(d.DailyMaxByISP[isp], ceilings)
			k := DomainISP{Domain: d.SendingDomain, ISP: normISP(isp)}
			in.GovernorEffective[k] = eff
			in.GovernorReason[k] = boundBy
		}
	}
	return nil
}

func scanLaneISPCounts(rows *sql.Rows, fn func(lane, isp string, n int, oldest sql.NullTime)) error {
	defer rows.Close()
	for rows.Next() {
		var lane, isp string
		var n int
		var oldest sql.NullTime
		if err := rows.Scan(&lane, &isp, &n, &oldest); err != nil {
			return err
		}
		fn(lane, isp, n, oldest)
	}
	return rows.Err()
}

// store writes the plan in ONE transaction (§2.5 step 8 + the crash-window rule).
func (p *Planner) store(ctx context.Context, db *sql.DB, contracts *ActiveSet, plan *Plan) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dripsupply: planner begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The planner's writes touch drip_lane_balance rows that Reserve locks on
	// the send path. lock_timeout means an intraday replan FAILS FAST and
	// retries instead of parking behind a live wave's row lock and stalling
	// every reservation queued behind it. Lock order is EnsureDayBalances
	// (domain rows) then the lane upserts — the same domain->lane order §2.2
	// uses, so the two cannot deadlock.
	if _, err := tx.ExecContext(ctx, "SET LOCAL lock_timeout = '5s'"); err != nil {
		return fmt.Errorf("dripsupply: planner lock_timeout: %w", err)
	}

	// The balance rows must exist before the lane upsert, and EnsureDayBalances
	// is ON CONFLICT DO NOTHING — it never resets an intraday counter.
	if contracts != nil {
		if _, err := EnsureDayBalances(ctx, tx, plan.Day, contracts); err != nil {
			return fmt.Errorf("dripsupply: planner ensure balances: %w", err)
		}
	}

	key := dayKey(plan.Day)
	if _, err := tx.ExecContext(ctx, `DELETE FROM drip_daily_plan WHERE day = $1::date`, key); err != nil {
		return fmt.Errorf("dripsupply: planner clear %s: %w", key, err)
	}
	for start := 0; start < len(plan.Rows); start += planInsertChunk {
		end := min(start+planInsertChunk, len(plan.Rows))
		chunk := plan.Rows[start:end]
		var sb strings.Builder
		sb.WriteString(`INSERT INTO drip_daily_plan
			(day, lane, isp, sending_domain, award_firm, award_provisional, followups_reserved, plan_share, rank, rank_reason, frozen_at) VALUES `)
		args := make([]any, 0, len(chunk)*11)
		for i, r := range chunk {
			if i > 0 {
				sb.WriteString(", ")
			}
			b := i * 11
			fmt.Fprintf(&sb, "($%d::date,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
				b+1, b+2, b+3, b+4, b+5, b+6, b+7, b+8, b+9, b+10, b+11)
			args = append(args, key, r.Lane, r.ISP, r.SendingDomain, r.AwardFirm, r.AwardProvisional,
				r.FollowupsReserved, r.PlanShare, r.Rank, r.RankReason, plan.FrozenAt)
		}
		if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("dripsupply: planner insert rows %d-%d: %w", start, end, err)
		}
	}

	for _, l := range plan.Lanes {
		// reserved/committed are NEVER touched: an intraday replan must not
		// un-spend capacity the executor has already handed out. unfilled is
		// recomputed from the NEW award minus what is already out.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO drip_lane_balance
				(day, lane, isp, desired, awarded_firm, awarded_provisional, reserved, committed, unfilled)
			VALUES ($1::date, $2, $3, $4::int, $5::int, $6::int, 0, 0, $5::int + $6::int)
			ON CONFLICT (day, lane, isp) DO UPDATE SET
				desired             = EXCLUDED.desired,
				awarded_firm        = EXCLUDED.awarded_firm,
				awarded_provisional = EXCLUDED.awarded_provisional,
				unfilled            = GREATEST(EXCLUDED.awarded_firm + EXCLUDED.awarded_provisional
				                               - drip_lane_balance.reserved - drip_lane_balance.committed, 0)
		`, key, l.Lane, l.ISP, l.Desired, l.AwardedFirm, l.AwardedProvisional); err != nil {
			return fmt.Errorf("dripsupply: planner lane balance %s/%s: %w", l.Lane, l.ISP, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("dripsupply: planner commit %s: %w", key, err)
	}
	return nil
}

// LoadStoredPlan reads back a frozen day. found=false when the day has no frozen
// rows, which is what makes Plan() recompute exactly once.
func LoadStoredPlan(ctx context.Context, q Queryer, day time.Time) (*Plan, bool, error) {
	day = dayOf(day)
	rows, err := q.QueryContext(ctx, `
		SELECT lane, isp, sending_domain, award_firm, award_provisional, followups_reserved,
		       plan_share, rank, rank_reason, frozen_at
		FROM drip_daily_plan
		WHERE day = $1::date AND frozen_at IS NOT NULL
		ORDER BY lane, isp, sending_domain
	`, dayKey(day))
	if err != nil {
		return nil, false, fmt.Errorf("dripsupply: load stored plan %s: %w", dayKey(day), err)
	}
	defer rows.Close()
	plan := &Plan{Day: day, FromStore: true}
	byCell := map[LaneISP]*LaneAward{}
	order := []LaneISP{}
	for rows.Next() {
		var r PlanRow
		var frozen sql.NullTime
		if err := rows.Scan(&r.Lane, &r.ISP, &r.SendingDomain, &r.AwardFirm, &r.AwardProvisional,
			&r.FollowupsReserved, &r.PlanShare, &r.Rank, &r.RankReason, &frozen); err != nil {
			return nil, false, fmt.Errorf("dripsupply: load stored plan scan: %w", err)
		}
		r.Day = day
		if frozen.Valid && frozen.Time.After(plan.FrozenAt) {
			plan.FrozenAt = frozen.Time
		}
		plan.Rows = append(plan.Rows, r)
		k := LaneISP{Lane: r.Lane, ISP: r.ISP}
		a, ok := byCell[k]
		if !ok {
			a = &LaneAward{Lane: r.Lane, ISP: r.ISP, Rank: r.Rank}
			byCell[k] = a
			order = append(order, k)
		}
		a.AwardedFirm += r.AwardFirm
		a.AwardedProvisional += r.AwardProvisional
		a.FollowupsReserved += r.FollowupsReserved
		a.AwardedCapacity += r.AwardFirm + r.AwardProvisional
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("dripsupply: load stored plan rows: %w", err)
	}
	if len(plan.Rows) == 0 {
		return nil, false, nil
	}
	for _, k := range order {
		plan.Lanes = append(plan.Lanes, *byCell[k])
	}
	return plan, true, nil
}

// -----------------------------------------------------------------------------
// PlanReader — the plan_remaining term WP3's Reserve consults
// -----------------------------------------------------------------------------

// PlanStore implements WP3's PlanReader against drip_daily_plan.
//
// TWO DELIBERATE DEPARTURES from the one-line brief, both reported to the lead:
//
//  1. The brief's arithmetic is "award_firm + award_provisional - reserved -
//     committed". In this schema that DOUBLE-COUNTS: drip_capacity_ledger.reserved
//     is the ORIGINAL grant and is never decremented — settle() only increments
//     `committed` and `released` (reservation.go settle, the UPDATE at the top of
//     the function). A 1,000 grant committed at 600 leaves reserved=1000,
//     committed=600, released=400; "reserved + committed" reads 1,600 against a
//     plan that has actually consumed 600. The correct consumption is
//     SUM(reserved) - SUM(released), which is what this implements.
//
//  2. It is TOUCH-CLASS AWARE. drip_daily_plan carries two separate ceilings for
//     the same cell: award_firm+award_provisional are the INTRO award, and
//     followups_reserved is the follow-up ceiling — §2.7: "the planner's
//     followups_reserved is its ceiling". A touch-class-blind reader would let
//     intros consume the follow-up reservation (and vice versa), which is exactly
//     the obligation §5.4 exists to protect.
type PlanStore struct{}

// PlanRemaining implements PlanReader.
func (PlanStore) PlanRemaining(ctx context.Context, q Queryer, req ReserveReq) (int, bool, error) {
	if q == nil {
		return 0, false, errors.New("dripsupply: PlanRemaining called with a nil queryer")
	}
	followup := strings.TrimSpace(req.TouchClass) == "followup"
	var ceiling, consumed int
	err := q.QueryRowContext(ctx, `
		SELECT CASE WHEN $5::bool
		            THEN p.followups_reserved
		            ELSE p.award_firm + p.award_provisional END AS ceiling,
		       COALESCE(l.consumed, 0) AS consumed
		FROM drip_daily_plan p
		LEFT JOIN LATERAL (
			SELECT COALESCE(SUM(c.reserved) - SUM(c.released), 0) AS consumed
			FROM drip_capacity_ledger c
			WHERE c.day = p.day
			  AND c.lane = p.lane
			  AND c.isp = p.isp
			  AND c.sending_domain = p.sending_domain
			  AND ((c.touch_class = 'followup') = $5::bool)
		) l ON TRUE
		WHERE p.day = $1::date AND p.lane = $2 AND p.isp = $3 AND p.sending_domain = $4
	`, dayKey(req.Day), req.Lane, normISP(req.ISP), req.Domain, followup).Scan(&ceiling, &consumed)
	if errors.Is(err, sql.ErrNoRows) {
		// No plan row for the cell: the plan does not constrain it. Failing OPEN
		// here is deliberate and matches WP3's contract (bounded=false) — the
		// domain balance, lane balance and supply terms still bind, and a planner
		// outage must not stop the estate mailing.
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("dripsupply: plan_remaining %s/%s/%s: %w", req.Domain, req.ISP, req.Lane, err)
	}
	rem := ceiling - consumed
	if rem < 0 {
		rem = 0
	}
	return rem, true, nil
}

var _ PlanReader = PlanStore{}

// -----------------------------------------------------------------------------
// Wiring stub (WP5 owns the actual scheduling; this file must not edit main.go)
// -----------------------------------------------------------------------------

// RunDailyPlanner is the entry point WP5 calls at 00:05 MT and on demand.
//
// It is safe to call repeatedly: the day is planned once and every later call in
// the same Denver day reads the frozen plan back. That is the property that
// matters when an ECS bounce restarts the scheduler at 00:05:30.
//
// This is a STUB in one respect only: it injects no dependencies. It resolves
// the contract token key from the environment (failing closed if unset), wires
// no YieldSource (so every ISP takes SeedEOYield) and no governor stack. WP5
// should build the planner once at boot instead:
//
//	NewPlanner(
//	    WithContractTokenKey(key),          // contractmeta.KeyFromEnv(), resolved at boot
//	    WithRankSource(EconomicsRankSource{}),
//	    WithYieldSource(...),               // WP7
//	    WithPlannerGovernors(governors),
//	)
func RunDailyPlanner(ctx context.Context, db *sql.DB, now time.Time) (*Plan, error) {
	return NewPlanner().Plan(ctx, db, DenverDay(now), false)
}

// DenverDay truncates an instant to the Denver day the send-day operates on.
// Falls back to the instant's own location if the tzdata is unavailable, which
// is loud in the log rather than silently planning a UTC day.
func DenverDay(t time.Time) time.Time {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		log.Printf("[DripPlanner] America/Denver tzdata unavailable (%v) — planning the day in %s", err, t.Location())
		return dayOf(t)
	}
	return dayOf(t.In(loc))
}

func sortedIntKeys[V any](m map[int]V) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
