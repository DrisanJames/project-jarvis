package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// SendGovernor (type FamilyGovernor, kept for its call sites) — per
// (LANE × sending domain × ISP) DAILY CEILING on the broadcast wave enqueue,
// sourced from the contract system (drip_dispatch_contracts, lane
// `broadcast-<lane>.<sending_domain>`, see laneKey), SHADOW-first.
//
// 2026-09-23 generalization (operator: "contracts should be the governors"):
// the 09-07 governor read ONE ceiling per domain (daily_ceiling) for the
// yahoo family only and counted every non-drip campaign of the domain in one
// pot, so it could not tell a cold cell from an engaged one. Now:
//
//	lane     = the campaign's `lane` (deploy payload key, persisted in
//	           pmta_config->'campaign_input'), or LaneOf(name) when untagged.
//	           Only lanes in SEND_GOVERNOR_LANES (default family,cold) are
//	           governed; every other lane (engaged, kumo, fresh) is untouched.
//	ceiling  = desired_daily_intros[isp] of the lane's active contract (per
//	           ISP), AND daily_ceiling as the domain total when present. An ISP
//	           absent from a non-empty desired_daily_intros is 0: the contract
//	           is the commitment, an unlisted ISP is not committed.
//	spent    = today's (Denver day) mailing_campaign_queue rows for the
//	           domain's NON-drip campaigns IN THE SAME LANE — per ISP for the
//	           per-ISP term, all ISPs for the domain-total term.
//	allowed  = max(0, min(requested, ceiling_isp − spent_isp, daily_ceiling − spent_domain)).
//
// One wave = one ISP plan = one ISP. The dispatcher asks once per due wave,
// AFTER it knows `remaining` and BEFORE it claims recipients:
//
//	SHADOW : compute + log + ledger row; `remaining` is NOT touched.
//	ON     : `remaining` becomes Allowed; 0 takes the wave straight to 'completed'.
//	OFF    : nothing runs — not one query.
//
// The planner (internal/api, planPMTAAudience) asks Headroom() for the same
// numbers at DEPLOY time so an audience-bound (volume 0) cell is BUILT at the
// contract instead of trimmed wave by wave and left with abandoned rows.
//
// FAIL OPEN, in both modes: any DB error returns Allowed=requested with
// Reason="error:<step>". The governor's reads and the ledger INSERT run on the
// *sql.DB handed to Decide, NOT inside the wave's FOR UPDATE transaction (a
// failed statement there would abort the wave). Keep it that way.
//
// Known bound (documented, not a bug): dispatch is parallel across waves, so
// ON mode can overshoot by at most (parallelism × wave size) at the boundary.

// SendGovernorModeEnv is read ONCE at construction; FamilyGovernorModeEnv is
// honoured as the fallback so the 09-07 task-definition keeps working.
const SendGovernorModeEnv = "SEND_GOVERNOR_MODE"
const FamilyGovernorModeEnv = "FAMILY_GOVERNOR_MODE"

// SendGovernorLanesEnv lists the governed lanes, comma-separated.
const SendGovernorLanesEnv = "SEND_GOVERNOR_LANES"
const sendGovernorDefaultLanes = "family,cold"

const (
	FamilyGovernorOff    = "off"
	FamilyGovernorShadow = "shadow"
	FamilyGovernorOn     = "on"
)

// Lane names. LaneEngaged is never governed (audience-bound by doctrine).
const (
	LaneFamily  = "family"
	LaneCold    = "cold"
	LaneEngaged = "engaged"
	LaneKumo    = "kumo"
	LaneFresh   = "fresh"
)

// governorLanePrefix + lane + "." + the plan's sending_domain is the contract
// lane. DOT, not colon (a colon is percent-encoded in the contracts API path).
// The domain is the plan's sending_domain VERBATIM (`m.<apex>` for the 16
// legacy brands), so the stepper files `broadcast-family.m.<apex>` and
// `broadcast-cold.m.<apex>`.
const governorLanePrefix = "broadcast-"
const familyGovernorLanePrefix = governorLanePrefix + LaneFamily + "."

// familyLane keeps the 09-07 derivation for the family lane.
func familyLane(sendingDomain string) string { return laneKey(LaneFamily, sendingDomain) }

// laneKey is THE contract-lane derivation — the stepper (Python) must produce
// the identical string. Lower-cased, trimmed; empty domain → "".
func laneKey(lane, sendingDomain string) string {
	d := strings.ToLower(strings.TrimSpace(sendingDomain))
	if d == "" {
		return ""
	}
	return governorLanePrefix + strings.ToLower(strings.TrimSpace(lane)) + "." + d
}

// LaneOf resolves a campaign's lane: the tagged lane wins; an untagged campaign
// falls back to its NAME. Order matters — the family cells are named
// `NL-YF-NEWSLETTER-D14-COLD`, so NL-YF must be tested before -COLD.
// LaneOfSQL is the SAME rule in SQL (used by the spend query); the test
// TestLaneOf_GoMatchesSQL pins the two equal over real prod names.
func LaneOf(name, lane string) string {
	if l := strings.ToLower(strings.TrimSpace(lane)); l != "" {
		return l
	}
	switch {
	case laneReNLYF.MatchString(name):
		return LaneFamily
	case laneReKumo.MatchString(name):
		return LaneKumo
	case laneReFresh.MatchString(name):
		return LaneFresh
	case laneReCold.MatchString(name):
		return LaneCold
	}
	return LaneEngaged
}

// KnownLanes is the closed set a deploy payload may tag; anything else is
// refused at the door (normalizePMTACampaignInput) so a typo cannot create an
// ungoverned lane.
var KnownLanes = map[string]bool{LaneEngaged: true, LaneCold: true, LaneFamily: true, LaneFresh: true, LaneKumo: true}

// IsKnownLane reports whether a payload lane tag (empty allowed) is in KnownLanes.
func IsKnownLane(lane string) bool {
	l := strings.ToLower(strings.TrimSpace(lane))
	return l == "" || KnownLanes[l]
}

var (
	laneReNLYF  = regexp.MustCompile(`NL-YF`)
	laneReKumo  = regexp.MustCompile(`KUMO-WARM`)
	laneReFresh = regexp.MustCompile(`FRESH`)
	laneReCold  = regexp.MustCompile(`-COLD|^[0-9]{8} - NX-`)
)

// LaneOfSQL mirrors LaneOf for the campaign row `c`.
const LaneOfSQL = `COALESCE(NULLIF(lower(btrim(c.pmta_config->'campaign_input'->>'lane', E' \t\r\n')), ''),
    CASE WHEN c.name ~ 'NL-YF' THEN 'family'
         WHEN c.name ~ 'KUMO-WARM' THEN 'kumo'
         WHEN c.name ~ 'FRESH' THEN 'fresh'
         WHEN c.name ~ '-COLD|^[0-9]{8} - NX-' THEN 'cold'
         ELSE 'engaged' END)`

// governorCacheTTL is how long a lane's ceiling (or its absence) and a
// campaign's lane are remembered before being re-read.
const familyGovernorCacheTTL = 60 * time.Second

// familyGovernorLaneCacheMax bounds the campaign→lane cache (expired entries
// are swept when it is reached; prod sees ~500 wave-bearing campaigns/day).
const familyGovernorLaneCacheMax = 4096

// familyGovernorQueryTimeout bounds the governor's own DB round-trips so a
// slow spend COUNT cannot eat the wave processor's budget (spend measured
// 54ms execution on prod 2026-09-06).
const familyGovernorQueryTimeout = 10 * time.Second

// familyGovernorISPs is the yahoo family (kept for callers; the governor no
// longer gates on it — every ISP a contract lists is governed).
var familyGovernorISPs = []string{isp.Yahoo, isp.Aol, isp.ATT, isp.Sbcglobal, isp.Cox}

// IsFamilyGovernedISP reports whether an isp_plans.isp value is in the yahoo family.
func IsFamilyGovernedISP(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, f := range familyGovernorISPs {
		if f == name {
			return true
		}
	}
	return false
}

// FamilyGovernorDecisionsDDL — the decision LEDGER, written in BOTH modes, one
// row per wave (PK wave_id; INSERT … ON CONFLICT DO NOTHING so a scheduler
// re-fire is idempotent). ONE statement — the migration runner classifies by
// leading keyword. The `lane` column is added to existing installs by
// FamilyGovernorDecisionsLaneDDL (5s slice; the table is PK-indexed only).
const FamilyGovernorDecisionsDDL = `
CREATE TABLE IF NOT EXISTS family_governor_decisions (
    wave_id        UUID PRIMARY KEY,
    day            DATE NOT NULL,
    sending_domain TEXT NOT NULL,
    isp            TEXT NOT NULL,
    mode           TEXT NOT NULL,
    requested      INTEGER NOT NULL,
    ceiling        INTEGER NOT NULL,
    spent          INTEGER NOT NULL,
    allowed        INTEGER NOT NULL,
    reason         TEXT NOT NULL,
    decided_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lane           TEXT NOT NULL DEFAULT ''
)`

const FamilyGovernorDecisionsLaneDDL = `ALTER TABLE family_governor_decisions ADD COLUMN IF NOT EXISTS lane TEXT NOT NULL DEFAULT ''`

// FamilyGovernorQueryer is what Decide needs from the DB: *sql.DB, *sql.Conn
// and *sql.Tx satisfy it (the dispatcher passes the DB, never its transaction).
type FamilyGovernorQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// FamilyGovernorDecision is the answer to one wave (or one Headroom call).
type FamilyGovernorDecision struct {
	Governed bool
	Lane     string
	Ceiling  int // the binding ceiling term (per-ISP when present, else the domain total)
	Spent    int // the spend of the binding term
	Allowed  int
	Mode     string
	Reason   string // ungoverned | ungoverned:<lane> | no_domain | no_contract | within | trim | deny | error:<step>
}

type governorCeiling struct {
	domainTotal sql.NullInt64 // daily_ceiling
	perISP      sql.NullInt64 // desired_daily_intros->>isp
	ddiEmpty    bool          // desired_daily_intros IS NULL or '{}'
	found       bool
	expires     time.Time
}

type governorLane struct {
	lane    string
	expires time.Time
}

// FamilyGovernor holds the mode (read once), the governed lanes and the caches.
type FamilyGovernor struct {
	db    *sql.DB
	mode  string
	lanes map[string]bool

	mu        sync.Mutex
	cache     map[string]governorCeiling // key: laneKey + "|" + isp
	laneCache map[string]governorLane    // key: campaign id

	now func() time.Time
}

// ParseFamilyGovernorMode maps the env value to a mode. ok=false means the
// value was not recognised (caller logs once and runs OFF).
func ParseFamilyGovernorMode(raw string) (mode string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", FamilyGovernorOff:
		return FamilyGovernorOff, true
	case FamilyGovernorShadow:
		return FamilyGovernorShadow, true
	case FamilyGovernorOn:
		return FamilyGovernorOn, true
	}
	return FamilyGovernorOff, false
}

// ParseGovernorLanes maps "family,cold" to a set; empty → the default set.
func ParseGovernorLanes(raw string) map[string]bool {
	if strings.TrimSpace(raw) == "" {
		raw = sendGovernorDefaultLanes
	}
	out := map[string]bool{}
	for _, l := range strings.Split(raw, ",") {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" && l != LaneEngaged {
			out[l] = true
		}
	}
	return out
}

// NewFamilyGovernor reads SEND_GOVERNOR_MODE (fallback FAMILY_GOVERNOR_MODE)
// and SEND_GOVERNOR_LANES once. Unknown mode values run OFF and log one line.
func NewFamilyGovernor(db *sql.DB) *FamilyGovernor {
	envName := SendGovernorModeEnv
	raw := os.Getenv(SendGovernorModeEnv)
	if strings.TrimSpace(raw) == "" {
		envName = FamilyGovernorModeEnv
		raw = os.Getenv(FamilyGovernorModeEnv)
	}
	mode, ok := ParseFamilyGovernorMode(raw)
	if !ok {
		log.Printf("[SendGovernor] %s=%q not recognised (off|shadow|on) — running OFF", envName, raw)
	}
	g := newFamilyGovernorWithMode(db, mode)
	g.lanes = ParseGovernorLanes(os.Getenv(SendGovernorLanesEnv))
	return g
}

func newFamilyGovernorWithMode(db *sql.DB, mode string) *FamilyGovernor {
	return &FamilyGovernor{
		db:        db,
		mode:      mode,
		lanes:     ParseGovernorLanes(""),
		cache:     make(map[string]governorCeiling),
		laneCache: make(map[string]governorLane),
		now:       time.Now,
	}
}

// Mode returns off|shadow|on. Safe on a nil receiver (→ off).
func (g *FamilyGovernor) Mode() string {
	if g == nil {
		return FamilyGovernorOff
	}
	return g.mode
}

// Enabled is the one gate the dispatcher checks before doing ANY governor work.
func (g *FamilyGovernor) Enabled() bool {
	return g != nil && g.mode != FamilyGovernorOff
}

// Lanes returns the governed lane set (copy).
func (g *FamilyGovernor) Lanes() []string {
	if g == nil {
		return nil
	}
	out := make([]string, 0, len(g.lanes))
	for l := range g.lanes {
		out = append(out, l)
	}
	return out
}

// Governs reports whether a lane is in the governed set.
func (g *FamilyGovernor) Governs(lane string) bool {
	return g != nil && g.lanes[strings.ToLower(strings.TrimSpace(lane))]
}

// familyGovernorDayStart is the Denver day start.
func familyGovernorDayStart(now time.Time) time.Time {
	loc, _ := time.LoadLocation("America/Denver")
	d := now.In(loc)
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
}

// familyGovernorDayEnd is the next Denver midnight (DST-safe via time.Date).
func familyGovernorDayEnd(dayStart time.Time) time.Time {
	return time.Date(dayStart.Year(), dayStart.Month(), dayStart.Day()+1, 0, 0, 0, 0, dayStart.Location())
}

// familyGovernorLaneSQL reads one campaign's lane (tag or name-derived).
const familyGovernorLaneSQL = `
SELECT ` + LaneOfSQL + `
FROM mailing_campaigns c
WHERE c.id = $1::uuid`

// familyGovernorContractSQL reads the lane's contract IN FORCE AT $3: the
// domain total, the per-ISP intro and whether the per-ISP map is empty. A
// `scheduled` version whose effective_at has been reached counts — the
// stepper files tomorrow's version at 23:10 MT as scheduled and the
// activator flips it at midnight, but the builders deploy tomorrow's cells at
// 22:31–23:45 MT and must plan against TOMORROW's ceiling (Headroom passes
// the cell's send-day start; the wave hook passes now).
const familyGovernorContractSQL = `
SELECT daily_ceiling,
       (desired_daily_intros->>$2)::int,
       (desired_daily_intros IS NULL OR desired_daily_intros = '{}'::jsonb)
FROM drip_dispatch_contracts
WHERE lane = $1::text AND status IN ('active','scheduled')
  AND effective_at <= $3
  AND superseded_at IS NULL
ORDER BY effective_at DESC, version DESC
LIMIT 1`

// familyGovernorSpendSQL counts today's queue rows for the domain's NON-drip
// campaigns IN THE LANE, in ONE pass: the per-ISP count (recipient_isp = $5,
// index idx_campaign_queue_recipient_isp) and the domain total. Keyed on
// isp_plans.sending_domain (the value the decision is keyed on). The campaign
// window opens one day early so a wave that crosses Denver midnight still
// counts its rows on the day they were enqueued (created_at). Measured on
// prod 2026-09-23 (EXPLAIN ANALYZE, m.discountblog.com): 1.4 s warm — the
// campaign-window scan dominates; idx_campaign_isp_plans_domain (concurrent
// builder) is filed to flip the join order.
//
// $1 dayStart, $2 dayEnd (timestamptz), $3 sending_domain, $4 lane, $5 isp.
const familyGovernorSpendSQL = `
SELECT COUNT(*) FILTER (WHERE q.recipient_isp = $5::text), COUNT(*)
FROM mailing_campaign_queue q
WHERE q.created_at >= $1 AND q.created_at < $2
  AND q.campaign_id IN (
    SELECT p.campaign_id
    FROM mailing_campaign_isp_plans p
    JOIN mailing_campaigns c ON c.id = p.campaign_id
    WHERE p.sending_domain = $3
      AND c.partner_drip_tag IS NULL AND c.journey_id IS NULL
      AND c.status NOT IN ('cancelled','deleted','failed','draft')
      AND c.scheduled_at >= $1 - INTERVAL '1 day' AND c.scheduled_at < $2
      AND ` + LaneOfSQL + ` = $4::text)`

// familyGovernorPlannedSQL is the DEPLOY-time spend: what the day's earlier
// deploys in the lane already COMMITTED (audience_selected_count) for the
// domain — per ISP and in total, one pass — whether or not their waves have
// fired yet. Headroom takes max(queued, planned) so two cells deployed the
// same night are sized against each other; the wave hook keeps counting
// queued rows. The cell being planned and its PreDispatch `~fresh` twin
// (same name sans suffix) are EXCLUDED, or a re-bind would count the campaign
// it replaces against itself and build the sibling short (QA 2026-09-23).
// $1 dayStart, $2 dayEnd, $3 sending_domain, $4 lane, $5 isp, $6 campaign name.
const familyGovernorPlannedSQL = `
SELECT COALESCE(SUM(p.audience_selected_count) FILTER (WHERE p.isp = $5::text), 0),
       COALESCE(SUM(p.audience_selected_count), 0)
FROM mailing_campaign_isp_plans p
JOIN mailing_campaigns c ON c.id = p.campaign_id
WHERE p.sending_domain = $3
  AND regexp_replace(c.name, ' ~fresh$', '') <> regexp_replace($6::text, ' ~fresh$', '')
  AND c.partner_drip_tag IS NULL AND c.journey_id IS NULL
  AND c.status NOT IN ('cancelled','deleted','failed','draft')
  AND c.scheduled_at >= $1 AND c.scheduled_at < $2
  AND ` + LaneOfSQL + ` = $4::text`

const familyGovernorLedgerSQL = `
INSERT INTO family_governor_decisions
    (wave_id, day, sending_domain, isp, mode, requested, ceiling, spent, allowed, reason, lane)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (wave_id) DO NOTHING`

// LaneFor resolves and caches a campaign's lane. Errors fail open to
// LaneEngaged (ungoverned) with the error returned for logging.
func (g *FamilyGovernor) LaneFor(ctx context.Context, q FamilyGovernorQueryer, campaignID string) (string, error) {
	campaignID = strings.TrimSpace(campaignID)
	now := g.now()
	g.mu.Lock()
	if e, ok := g.laneCache[campaignID]; ok && now.Before(e.expires) {
		g.mu.Unlock()
		return e.lane, nil
	}
	g.mu.Unlock()
	var lane string
	if err := q.QueryRowContext(ctx, familyGovernorLaneSQL, campaignID).Scan(&lane); err != nil {
		return LaneEngaged, err
	}
	lane = strings.ToLower(strings.TrimSpace(lane))
	g.mu.Lock()
	if len(g.laneCache) >= familyGovernorLaneCacheMax {
		for k, e := range g.laneCache { // sweep expired entries; ~500 campaigns/day, never unbounded
			if !now.Before(e.expires) {
				delete(g.laneCache, k)
			}
		}
	}
	g.laneCache[campaignID] = governorLane{lane: lane, expires: now.Add(familyGovernorCacheTTL)}
	g.mu.Unlock()
	return lane, nil
}

// ceilingFor returns the lane × ISP contract terms, from the 60s cache when
// fresh. A missing contract is cached as not-found so the contracts table is
// not re-read on every wave of an ungoverned domain.
func (g *FamilyGovernor) ceilingFor(ctx context.Context, q FamilyGovernorQueryer, lane, ispName string, asOf time.Time) (governorCeiling, error) {
	key := lane + "|" + ispName + "|" + familyGovernorDayStart(asOf).Format("2006-01-02")
	now := g.now()
	g.mu.Lock()
	if e, ok := g.cache[key]; ok && now.Before(e.expires) {
		g.mu.Unlock()
		return e, nil
	}
	g.mu.Unlock()

	var e governorCeiling
	err := q.QueryRowContext(ctx, familyGovernorContractSQL, lane, ispName, asOf).Scan(&e.domainTotal, &e.perISP, &e.ddiEmpty)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// no active contract → ungoverned
	case err != nil:
		return governorCeiling{}, err
	default:
		e.found = true
	}
	e.expires = now.Add(familyGovernorCacheTTL)
	g.mu.Lock()
	g.cache[key] = e
	g.mu.Unlock()
	return e, nil
}

// Decide answers one wave: resolves the campaign's lane, then DecideLane.
// It never returns Allowed < 0 and, on any error, returns Allowed == requested
// (fail open) alongside the error for logging.
func (g *FamilyGovernor) Decide(ctx context.Context, q FamilyGovernorQueryer, campaignID, sendingDomain, ispName string, day time.Time, waveID string, requested int) (FamilyGovernorDecision, error) {
	d := FamilyGovernorDecision{Mode: g.Mode(), Allowed: requested, Reason: "ungoverned"}
	if !g.Enabled() {
		return d, nil
	}
	qctx, cancel := context.WithTimeout(ctx, familyGovernorQueryTimeout)
	defer cancel()
	lane, err := g.LaneFor(qctx, q, campaignID)
	if err != nil {
		d.Reason = "error:lane"
		return d, fmt.Errorf("send governor lane %s: %w", campaignID, err)
	}
	return g.DecideLane(ctx, q, lane, sendingDomain, ispName, day, waveID, requested)
}

// DecideLane answers one wave for a known lane and writes the ledger row.
func (g *FamilyGovernor) DecideLane(ctx context.Context, q FamilyGovernorQueryer, lane, sendingDomain, ispName string, day time.Time, waveID string, requested int) (FamilyGovernorDecision, error) {
	d, err := g.evaluate(ctx, q, lane, sendingDomain, ispName, day, requested, false, "")
	if d.Governed || strings.HasPrefix(d.Reason, "error:") || strings.Contains(d.Reason, "no_isp_ceiling") {
		qctx, cancel := context.WithTimeout(ctx, familyGovernorQueryTimeout)
		defer cancel()
		g.record(qctx, q, familyGovernorDayStart(day), lane, strings.ToLower(strings.TrimSpace(sendingDomain)), strings.ToLower(strings.TrimSpace(ispName)), waveID, requested, d)
	}
	return d, err
}

// Headroom is the deploy-time question: how many rows may this lane × domain
// × ISP still send on `day` (the cell's send day, Denver)? Spend counts
// max(queued, planned) so earlier deploys for the same day bind. governed=false
// means "no contract / lane not governed / governor off" and the caller must
// not clamp. Errors fail open (governed=false) with the error for logging.
// No ledger row.
func (g *FamilyGovernor) Headroom(ctx context.Context, q FamilyGovernorQueryer, lane, sendingDomain, ispName, campaignName string, day time.Time) (int, bool, error) {
	d, err := g.evaluate(ctx, q, lane, sendingDomain, ispName, day, math.MaxInt32, true, campaignName)
	if err != nil || !d.Governed {
		return 0, false, err
	}
	return d.Allowed, true, nil
}

// evaluate is the arithmetic shared by Decide and Headroom.
func (g *FamilyGovernor) evaluate(ctx context.Context, q FamilyGovernorQueryer, lane, sendingDomain, ispName string, day time.Time, requested int, planTime bool, campaignName string) (FamilyGovernorDecision, error) {
	lane = strings.ToLower(strings.TrimSpace(lane))
	d := FamilyGovernorDecision{Mode: g.Mode(), Lane: lane, Allowed: requested, Reason: "ungoverned"}
	if !g.Enabled() {
		return d, nil
	}
	if !g.Governs(lane) {
		d.Reason = "ungoverned:" + lane
		return d, nil
	}
	sendingDomain = strings.ToLower(strings.TrimSpace(sendingDomain))
	ispName = strings.ToLower(strings.TrimSpace(ispName))
	if sendingDomain == "" {
		d.Reason = "no_domain"
		return d, nil
	}

	qctx, cancel := context.WithTimeout(ctx, familyGovernorQueryTimeout)
	defer cancel()

	key := laneKey(lane, sendingDomain)
	asOf := day
	if planTime {
		asOf = familyGovernorDayStart(day) // the version in force on the cell's send day
	}
	c, err := g.ceilingFor(qctx, q, key, ispName, asOf)
	if err != nil {
		d.Reason = "error:contract"
		return d, fmt.Errorf("send governor contract %s: %w", key, err)
	}
	if !c.found {
		d.Reason = "no_contract"
		return d, nil
	}
	d.Governed = true

	dayStart := familyGovernorDayStart(day)
	dayEnd := familyGovernorDayEnd(dayStart)
	// ONE spend pass per decision: (per-ISP, domain total) for queued rows,
	// plus the same pair for planned rows at deploy time.
	var spentISP, spentDomain int
	if err := q.QueryRowContext(qctx, familyGovernorSpendSQL, dayStart, dayEnd, sendingDomain, lane, ispName).Scan(&spentISP, &spentDomain); err != nil {
		d.Governed = true
		d.Reason = "error:spend"
		return d, fmt.Errorf("send governor spend %s %s %s: %w", lane, sendingDomain, ispName, err)
	}
	if planTime {
		var plannedISP, plannedDomain int
		if err := q.QueryRowContext(qctx, familyGovernorPlannedSQL, dayStart, dayEnd, sendingDomain, lane, ispName, campaignName).Scan(&plannedISP, &plannedDomain); err != nil {
			d.Governed = true
			d.Reason = "error:spend"
			return d, fmt.Errorf("send governor planned %s %s %s: %w", lane, sendingDomain, ispName, err)
		}
		if plannedISP > spentISP {
			spentISP = plannedISP
		}
		if plannedDomain > spentDomain {
			spentDomain = plannedDomain
		}
	}
	balance := math.MaxInt32
	bind := func(ceiling, spent int) {
		if b := ceiling - spent; b < balance {
			balance = b
			d.Ceiling, d.Spent = ceiling, spent
		}
	}

	// Per-ISP term: the listed intro. An ISP ABSENT from a non-empty map is a
	// contract GAP, not a zero: the per-ISP term is skipped, the domain total
	// still binds, and the decision is flagged `no_isp_ceiling` so the shadow
	// report surfaces it (QA 2026-09-23: every family contract lacked comcast;
	// zero would have denied comcast estate-wide the first night).
	noISP := false
	switch {
	case c.perISP.Valid:
		bind(int(c.perISP.Int64), spentISP)
	case !c.ddiEmpty:
		noISP = true
	}
	if c.domainTotal.Valid {
		bind(int(c.domainTotal.Int64), spentDomain)
	}
	if balance == math.MaxInt32 {
		// contract row without any binding term → nothing to enforce; a
		// per-ISP gap with no domain total is reported as such (ledgered).
		d.Governed = false
		d.Reason = "no_contract"
		if noISP {
			d.Reason = "no_isp_ceiling"
		}
		return d, nil
	}

	switch {
	case balance <= 0:
		d.Allowed = 0
		d.Reason = "deny"
	case requested > balance:
		d.Allowed = balance
		d.Reason = "trim"
	default:
		d.Allowed = requested
		d.Reason = "within"
	}
	if d.Allowed < 0 {
		d.Allowed = 0
	}
	if noISP {
		d.Reason += "|no_isp_ceiling"
	}
	return d, nil
}

// record writes the ledger row (both modes). Failures are logged, never returned.
func (g *FamilyGovernor) record(ctx context.Context, q FamilyGovernorQueryer, dayStart time.Time, lane, sendingDomain, ispName, waveID string, requested int, d FamilyGovernorDecision) {
	if _, err := q.ExecContext(ctx, familyGovernorLedgerSQL,
		waveID, dayStart.Format("2006-01-02"), sendingDomain, ispName, d.Mode,
		requested, d.Ceiling, d.Spent, d.Allowed, d.Reason, lane); err != nil {
		log.Printf("[SendGovernor] ledger write failed wave=%s lane=%s domain=%s isp=%s: %v", waveID, lane, sendingDomain, ispName, err)
	}
}
