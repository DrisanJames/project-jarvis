package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// FamilyGovernor — per-(sending domain × yahoo-family) DAILY CEILING on the
// broadcast wave enqueue, sourced from the contract system (drip_dispatch_contracts,
// lane `broadcast-family.<sending_domain>`, see familyLane), SHADOW-first.
//
// One wave = one ISP plan = one ISP (mailing_campaign_waves → isp_plans). The
// dispatcher (EnqueuePMTAWave) asks Decide() once per due wave, AFTER it knows
// `remaining` (planned − enqueued) and BEFORE it claims recipients:
//
//	SHADOW : compute + log + write the decision row; `remaining` is NOT touched.
//	ON     : `remaining` becomes Allowed; 0 takes the wave straight to 'completed'.
//	OFF    : nothing runs — not one query (the negative-control test pins this).
//
// Ceiling  = daily_ceiling of the lane's single `active` contract (cached 60s).
// Spent    = today's (Denver day) mailing_campaign_queue rows for the domain's
//            NON-drip campaigns whose recipient_isp is in the family — the same
//            non-drip filter the domain governor uses (partner_drip_domain_governor.go
//            domainGovernorSpendToday): partner_drip_tag IS NULL AND journey_id IS NULL.
// Allowed  = max(0, min(requested, Ceiling − Spent)).
//
// FAIL OPEN, in both modes: any DB error returns Allowed=requested with
// Reason="error:<step>" — a governor outage must never stop the board; the
// operator flips FAMILY_GOVERNOR_MODE instead. Deliberately NOT
// dripsupply.Service.Reserve / Mediator.Grant: their token bucket and the
// planner-overwritten lane demand would starve broadcast (research verdict,
// FAMILY_GOVERNOR_SPEC.md).
//
// The governor's reads and the ledger INSERT run on the *sql.DB handed to
// Decide (a separate connection), NOT inside the wave's FOR UPDATE transaction:
// a failed statement inside that transaction would abort it ("current
// transaction is aborted") and turn a fail-open decision into a failed wave
// that the scheduler re-fires every 15s. Keep it that way.
//
// Known bound (not a bug, documented): dispatch is parallel across waves
// (dispatch_parallelism_test.go), and Spent is read before any sibling wave
// commits, so ON mode can overshoot the ceiling by at most (parallelism × wave
// size) at the boundary. Tightening needs a per-domain lock; out of scope.

// FamilyGovernorModeEnv is the env var read ONCE at construction.
const FamilyGovernorModeEnv = "FAMILY_GOVERNOR_MODE"

const (
	FamilyGovernorOff    = "off"
	FamilyGovernorShadow = "shadow"
	FamilyGovernorOn     = "on"
)

// familyGovernorLanePrefix + the plan's sending_domain is the contract lane.
// DOT, not colon: a colon is percent-encoded in the contracts API path and
// stored verbatim, so lanes never carry one. NOTE the domain is the plan's
// sending_domain VERBATIM: the 16 legacy brands mail the board from `m.<apex>`
// (prod isp_plans 2026-09-05: m.discountblog.com, m.historythinking.com, …),
// so the stepper must create `broadcast-family.m.<apex>` lanes, not em.<apex>.
const familyGovernorLanePrefix = "broadcast-family."

// familyLane is THE lane derivation — the stepper (Python) must produce the
// identical string. Lower-cased, trimmed; empty domain → "".
func familyLane(sendingDomain string) string {
	d := strings.ToLower(strings.TrimSpace(sendingDomain))
	if d == "" {
		return ""
	}
	return familyGovernorLanePrefix + d
}

// familyGovernorCacheTTL is how long a lane's ceiling (or its absence) is
// remembered before drip_dispatch_contracts is re-read.
const familyGovernorCacheTTL = 60 * time.Second

// familyGovernorQueryTimeout bounds the governor's own DB round-trips so a
// slow spend COUNT cannot eat the wave processor's budget. The spend query
// measured 54ms execution / <4s wall on prod (2026-09-06, m.discountblog.com).
const familyGovernorQueryTimeout = 10 * time.Second

// familyGovernorISPs is the yahoo family. Gmail is never here.
var familyGovernorISPs = []string{isp.Yahoo, isp.Aol, isp.ATT, isp.Sbcglobal, isp.Cox}

// IsFamilyGovernedISP reports whether an isp_plans.isp value is governed.
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
// re-fire is idempotent). Bare CREATE TABLE, empty at creation, PK index only:
// O(1), inside the 5s startup-migration budget. ONE statement — the migration
// runner classifies by leading keyword (cmd/server/migration_skip.go).
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
    decided_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`

// FamilyGovernorQueryer is what Decide needs from the DB: *sql.DB satisfies it
// (so does *sql.Tx — but see the package comment on why the dispatcher passes
// the DB, not its transaction).
type FamilyGovernorQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// FamilyGovernorDecision is the answer to one wave.
type FamilyGovernorDecision struct {
	Governed bool
	Ceiling  int
	Spent    int
	Allowed  int
	Mode     string
	Reason   string // ungoverned | no_domain | no_contract | within | trim | deny | error:<step>
}

type familyCeilingEntry struct {
	ceiling int
	found   bool
	expires time.Time
}

// FamilyGovernor holds the mode (read once) and the per-lane ceiling cache.
type FamilyGovernor struct {
	db   *sql.DB
	mode string

	mu    sync.Mutex
	cache map[string]familyCeilingEntry

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

// NewFamilyGovernor reads FAMILY_GOVERNOR_MODE once. Unknown values run OFF
// and log one line.
func NewFamilyGovernor(db *sql.DB) *FamilyGovernor {
	raw := os.Getenv(FamilyGovernorModeEnv)
	mode, ok := ParseFamilyGovernorMode(raw)
	if !ok {
		log.Printf("[FamilyGovernor] %s=%q not recognised (off|shadow|on) — running OFF", FamilyGovernorModeEnv, raw)
	}
	return newFamilyGovernorWithMode(db, mode)
}

func newFamilyGovernorWithMode(db *sql.DB, mode string) *FamilyGovernor {
	return &FamilyGovernor{
		db:    db,
		mode:  mode,
		cache: make(map[string]familyCeilingEntry),
		now:   time.Now,
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

// familyGovernorDayStart mirrors domainGovernorDayStart: the Denver day start.
func familyGovernorDayStart(now time.Time) time.Time {
	loc, _ := time.LoadLocation("America/Denver")
	d := now.In(loc)
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
}

// familyGovernorDayEnd is the next Denver midnight (DST-safe via time.Date).
func familyGovernorDayEnd(dayStart time.Time) time.Time {
	return time.Date(dayStart.Year(), dayStart.Month(), dayStart.Day()+1, 0, 0, 0, 0, dayStart.Location())
}

// familyGovernorContractSQL reads the lane's single active ceiling. Direct
// SELECT by design (see package comment). NULL daily_ceiling scans as invalid.
const familyGovernorContractSQL = `
SELECT daily_ceiling
FROM drip_dispatch_contracts
WHERE lane = $1 AND status = 'active'
ORDER BY version DESC
LIMIT 1`

// familyGovernorSpendSQL counts today's family queue rows for the domain's
// NON-drip campaigns. Keyed on isp_plans.sending_domain — the same value the
// decision is keyed on — NOT mailing_sending_profiles.sending_domain (the
// board's profiles carry `m.<apex>` and the two need not agree).
//
// Plan verified on prod 2026-09-06 (EXPLAIN ANALYZE, m.discountblog.com,
// 2026-09-05 Denver day): idx_campaigns_org_sched → idx_campaign_isp_plans_campaign
// → idx_campaign_queue_recipient_isp; 54ms execution, 37k shared-hit buffers,
// spent=10,346. No new index needed.
//
// $1 dayStart, $2 dayEnd (timestamptz), $3 family ISPs, $4 sending_domain.
// The campaign window opens one day early so a wave that crosses Denver
// midnight still counts its rows on the day they were enqueued (created_at).
const familyGovernorSpendSQL = `
SELECT COUNT(*)
FROM mailing_campaign_queue q
WHERE q.recipient_isp = ANY($3)
  AND q.created_at >= $1 AND q.created_at < $2
  AND q.campaign_id IN (
    SELECT p.campaign_id
    FROM mailing_campaign_isp_plans p
    JOIN mailing_campaigns c ON c.id = p.campaign_id
    WHERE p.sending_domain = $4
      AND c.partner_drip_tag IS NULL AND c.journey_id IS NULL
      AND c.status NOT IN ('cancelled','deleted','failed','draft')
      AND c.scheduled_at >= $1 - INTERVAL '1 day' AND c.scheduled_at < $2)`

const familyGovernorLedgerSQL = `
INSERT INTO family_governor_decisions
    (wave_id, day, sending_domain, isp, mode, requested, ceiling, spent, allowed, reason)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (wave_id) DO NOTHING`

// ceilingFor returns (ceiling, found, err) for a lane, from the 60s cache when
// fresh. A missing contract or a NULL ceiling is cached as not-found so the
// contracts table is not re-read on every wave of an ungoverned domain.
func (g *FamilyGovernor) ceilingFor(ctx context.Context, q FamilyGovernorQueryer, lane string) (int, bool, error) {
	now := g.now()
	g.mu.Lock()
	if e, ok := g.cache[lane]; ok && now.Before(e.expires) {
		g.mu.Unlock()
		return e.ceiling, e.found, nil
	}
	g.mu.Unlock()

	var ceiling sql.NullInt64
	err := q.QueryRowContext(ctx, familyGovernorContractSQL, lane).Scan(&ceiling)
	found := false
	var val int
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// no active contract → ungoverned
	case err != nil:
		return 0, false, err
	case ceiling.Valid:
		found = true
		val = int(ceiling.Int64)
	}
	g.mu.Lock()
	g.cache[lane] = familyCeilingEntry{ceiling: val, found: found, expires: now.Add(familyGovernorCacheTTL)}
	g.mu.Unlock()
	return val, found, nil
}

// Decide answers one wave. It never returns Allowed < 0 and, on any error,
// returns Allowed == requested (fail open) alongside the error for logging.
func (g *FamilyGovernor) Decide(ctx context.Context, q FamilyGovernorQueryer, sendingDomain, ispName string, day time.Time, waveID string, requested int) (FamilyGovernorDecision, error) {
	d := FamilyGovernorDecision{Mode: g.Mode(), Allowed: requested, Reason: "ungoverned"}
	if !g.Enabled() || !IsFamilyGovernedISP(ispName) {
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

	lane := familyLane(sendingDomain)
	ceiling, found, err := g.ceilingFor(qctx, q, lane)
	if err != nil {
		d.Reason = "error:contract"
		return d, fmt.Errorf("family governor contract %s: %w", lane, err)
	}
	if !found {
		d.Reason = "no_contract"
		return d, nil
	}
	d.Governed = true
	d.Ceiling = ceiling

	dayStart := familyGovernorDayStart(day)
	dayEnd := familyGovernorDayEnd(dayStart)
	var spent int
	if err := q.QueryRowContext(qctx, familyGovernorSpendSQL, dayStart, dayEnd, pq.Array(familyGovernorISPs), sendingDomain).Scan(&spent); err != nil {
		d.Reason = "error:spend"
		g.record(qctx, q, dayStart, sendingDomain, ispName, waveID, requested, d)
		return d, fmt.Errorf("family governor spend %s: %w", sendingDomain, err)
	}
	d.Spent = spent

	balance := ceiling - spent
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
	g.record(qctx, q, dayStart, sendingDomain, ispName, waveID, requested, d)
	return d, nil
}

// record writes the ledger row (both modes). Failures are logged, never returned.
func (g *FamilyGovernor) record(ctx context.Context, q FamilyGovernorQueryer, dayStart time.Time, sendingDomain, ispName, waveID string, requested int, d FamilyGovernorDecision) {
	if _, err := q.ExecContext(ctx, familyGovernorLedgerSQL,
		waveID, dayStart.Format("2006-01-02"), sendingDomain, ispName, d.Mode,
		requested, d.Ceiling, d.Spent, d.Allowed, d.Reason); err != nil {
		log.Printf("[FamilyGovernor] ledger write failed wave=%s domain=%s isp=%s: %v", waveID, sendingDomain, ispName, err)
	}
}
