package dripsupply

// executor.go — REQ-118 WP5: the Mediator the partner-drip orchestrator calls
// once per wave, plus the "nothing is silent" tick-outcome surface and the §6
// alerts.
//
// The whole file exists to make ONE decision reversible: whether a wave's
// per-ISP caps come from the old typed cap chain or from a capacity
// reservation. `DRIP_SUPPLY_CHAIN_MODE` picks:
//
//	off     Grant returns nil before touching the database. The orchestrator's
//	        cap chain runs byte-identical to HEAD. This is the rollback.
//	shadow  Identical computation, written to the SHADOW ledger. No balance is
//	        decremented, no partner_clean_queue row is locked, caps are nil, so
//	        the old chain still decides what ships (§7 step 2).
//	canary  Enforce only the cells named in DRIP_SUPPLY_CANARY; every other
//	        cell is shadowed (§7 step 3).
//	on      Enforce everywhere.
//
// Tick outcomes are written in EVERY mode, `off` included: they are the
// operator's answer to "is this lane wedged or just out of demand", they cost
// one upsert per lane per pass per tick, and the pre-REQ-118 orchestrator's
// silence on a zero-claim wave is the defect REQ-116 was filed for.
//
// Nil-safety is deliberate and load-bearing: every exported method on
// *Mediator and *Allocation tolerates a nil receiver, so an orchestrator with
// no mediator wired (every existing test, and any boot where the flag is
// unset) runs the old path with no extra branches at the call sites.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/ignite/sparkpost-monitor/internal/notify"
	"github.com/ignite/sparkpost-monitor/internal/pkg/contractmeta"
	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
)

// -----------------------------------------------------------------------------
// Mode
// -----------------------------------------------------------------------------

// Mode is DRIP_SUPPLY_CHAIN_MODE (§7).
type Mode string

const (
	ModeOff    Mode = "off"
	ModeShadow Mode = "shadow"
	ModeCanary Mode = "canary"
	ModeOn     Mode = "on"
)

// ParseMode reads the env value. An EMPTY value is `off` (the shipped default);
// an UNRECOGNISED value is an error and the caller must fail the wiring rather
// than guess — a typo'd "canery" that silently resolved to `on` would enforce
// contracts estate-wide on a deploy nobody reviewed for that.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return ModeOff, nil
	case ModeOff, ModeShadow, ModeCanary, ModeOn:
		return m, nil
	default:
		return ModeOff, fmt.Errorf("dripsupply: unknown DRIP_SUPPLY_CHAIN_MODE %q (want off|shadow|canary|on)", s)
	}
}

// Enforces reports whether the mode can enforce any cell at all.
func (m Mode) Enforces() bool { return m == ModeCanary || m == ModeOn }

// -----------------------------------------------------------------------------
// Canary cells
// -----------------------------------------------------------------------------

// CanaryCell is one `<domain>:<isp>:<lane>` entry of DRIP_SUPPLY_CANARY.
// `*` in any position is a wildcard, so a whole domain or a whole lane can be
// promoted without listing 12 ISPs.
type CanaryCell struct{ Domain, ISP, Lane string }

// ParseCanary parses the comma-separated cell list. An entry that is not three
// colon-separated parts is an error: a half-parsed cell would silently enforce
// nothing (or, worse, everything) and the operator would have no way to tell
// from the outside which it was.
func ParseCanary(spec string) ([]CanaryCell, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var out []CanaryCell
	for _, raw := range strings.Split(spec, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		parts := strings.Split(raw, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("dripsupply: bad DRIP_SUPPLY_CANARY entry %q (want <domain>:<isp>:<lane>)", raw)
		}
		c := CanaryCell{
			Domain: strings.ToLower(strings.TrimSpace(parts[0])),
			ISP:    strings.ToLower(strings.TrimSpace(parts[1])),
			Lane:   strings.ToLower(strings.TrimSpace(parts[2])),
		}
		if c.Domain == "" || c.ISP == "" || c.Lane == "" {
			return nil, fmt.Errorf("dripsupply: bad DRIP_SUPPLY_CANARY entry %q (empty part)", raw)
		}
		out = append(out, c)
	}
	return out, nil
}

func canaryPartMatches(pattern, v string) bool {
	return pattern == "*" || pattern == strings.ToLower(strings.TrimSpace(v))
}

// Matches reports whether this cell selects (domain, isp, lane).
func (c CanaryCell) Matches(domain, isp, lane string) bool {
	return canaryPartMatches(c.Domain, domain) &&
		canaryPartMatches(c.ISP, isp) &&
		canaryPartMatches(c.Lane, lane)
}

func canaryMatch(cells []CanaryCell, domain, isp, lane string) bool {
	for _, c := range cells {
		if c.Matches(domain, isp, lane) {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// Tick outcomes (§1.2 drip_tick_outcomes)
// -----------------------------------------------------------------------------

// Outcome values — the CHECK on drip_tick_outcomes.outcome.
const (
	OutcomeFired   = "fired"
	OutcomeSkipped = "skipped"
	OutcomeZero    = "zero"
	OutcomeFailed  = "failed"
)

// Touch classes — the drip_capacity_ledger.touch_class CHECK (§1.2).
const (
	TouchClassIntro    = "intro"
	TouchClassFollowup = "followup"
	TouchClassRemail   = "remail"
)

// Pass names — the `pass` half of the (tick, lane, pass) grain.
const (
	PassWelcome   = "welcome"
	PassFollowup  = "followup"
	PassGoverned  = "governed"
	PassAOLRotate = "aol_rotate"
)

// Skip / zero reasons the executor writes. Free text is allowed (the column has
// no CHECK); these are the ones the §6 alerts and the portal key on.
const (
	SkipNoContract = "no_contract"
	// SkipNoContractKey is CONTRACT_TOKEN_KEY unset or too short (§1.5). Every
	// active contract carries an HMAC token that LoadActive verifies, so
	// without the key NOTHING can be loaded and every lane fails closed — a
	// distinct reason from "this subject has no contract", because the remedy
	// is a task-def env var, not a contract row.
	SkipNoContractKey    = "no_contract_key"
	SkipPaused           = "paused"
	SkipBudgetExhausted  = "budget_exhausted"
	SkipOutsideWindow    = "outside_window"
	SkipReserveTimeout   = "reserve_timeout"
	SkipNoPositiveGrant  = "no_positive_grant"
	SkipNoWaveSize       = "no_wave_size"
	ZeroNoRecordsClaimed = "no_records_claimed"
	ZeroAllDeferred      = "all_records_deferred"
)

// outcomePriority ranks the four outcomes so a lane that fired one brand's wave
// and zeroed another's in the SAME tick reads as `fired`, and a failure always
// wins. The (tick, lane, pass) primary key means several brands collapse onto
// one row; without a priority the last brand processed would decide the row and
// a lane that shipped would look dead.
func outcomePriority(o string) int {
	switch o {
	case OutcomeFailed:
		return 3
	case OutcomeFired:
		return 2
	case OutcomeZero:
		return 1
	default: // skipped
		return 0
	}
}

// OutcomeRow is one drip_tick_outcomes write.
type OutcomeRow struct {
	Lane       string
	Pass       string
	Outcome    string
	Reason     string
	CapsSeen   map[string]int
	Claimed    int
	CampaignID string
	// Brand is not a column; it is folded into `reason` so the operator can
	// tell WHICH brand of the lane produced this tick's outcome.
	Brand string
}

// upsertOutcomeSQL keeps the highest-priority outcome of the tick and SUMS the
// claimed counts across the brands of one lane×pass.
//
// $9 carries the NEW row's priority, computed in Go; the old row's priority is
// derived inline so the comparison happens inside the statement and two ECS
// instances writing the same grain cannot lose to each other's read-modify-write.
const upsertOutcomeSQL = `
	INSERT INTO drip_tick_outcomes (tick, lane, pass, outcome, reason, caps_seen, claimed, campaign_id)
	VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8)
	ON CONFLICT (tick, lane, pass) DO UPDATE SET
		outcome = CASE WHEN $9 >= (CASE drip_tick_outcomes.outcome
		                              WHEN 'failed' THEN 3 WHEN 'fired' THEN 2
		                              WHEN 'zero'   THEN 1 ELSE 0 END)
		          THEN EXCLUDED.outcome ELSE drip_tick_outcomes.outcome END,
		reason = CASE WHEN $9 >= (CASE drip_tick_outcomes.outcome
		                              WHEN 'failed' THEN 3 WHEN 'fired' THEN 2
		                              WHEN 'zero'   THEN 1 ELSE 0 END)
		          THEN EXCLUDED.reason ELSE drip_tick_outcomes.reason END,
		caps_seen = CASE WHEN $9 >= (CASE drip_tick_outcomes.outcome
		                              WHEN 'failed' THEN 3 WHEN 'fired' THEN 2
		                              WHEN 'zero'   THEN 1 ELSE 0 END)
		          THEN EXCLUDED.caps_seen ELSE drip_tick_outcomes.caps_seen END,
		claimed = drip_tick_outcomes.claimed + EXCLUDED.claimed,
		campaign_id = COALESCE(EXCLUDED.campaign_id, drip_tick_outcomes.campaign_id)`

// -----------------------------------------------------------------------------
// Mediator
// -----------------------------------------------------------------------------

// ShadowLedgerTable is the §7 step-2 twin of drip_capacity_ledger. It is a
// MediatorConfig knob rather than a Service option because the WP3 Service
// hard-codes its table names inside the reservation transaction and WP5 does
// not edit reservation.go: shadow mode therefore runs its OWN read-only
// computation (shadowReserve) that writes here, and never locks a live balance
// row. reservation_shadow_parity is the test that keeps the two arithmetics
// identical.
const ShadowLedgerTable = "drip_capacity_ledger_shadow"

// ShadowPlanTable is the §7 step-2 twin of drip_daily_plan. Same reason the
// ledger has one: WP6's Planner.store hard-codes `drip_daily_plan` AND upserts
// drip_lane_balance, which the LIVE executor reserves against. Pointing WP6 at
// another table is not enough — a shadow plan must write no lane balances at
// all, so shadow planning runs WP6's read + assign phases and this file's own
// writer. planShadow is that writer; planner.go is untouched.
const ShadowPlanTable = "drip_daily_plan_shadow"

// Defaults for the tick janitors (§2.8).
const (
	DefaultExpireAfter = 45 * time.Minute
	DefaultAlertEvery  = time.Hour
)

// MediatorConfig is everything main.go reads from the environment.
type MediatorConfig struct {
	Mode   Mode
	Canary []CanaryCell

	// ExpireAfter is §2.2's stale-reservation cutoff (reserved, never committed).
	ExpireAfter time.Duration
	// ReapAge / ReapBatch bound the §2.4 orphan-claim reaper.
	ReapAge   time.Duration
	ReapBatch int

	// OutcomesDisabled is DRIP_TICK_OUTCOMES_DISABLED=1. The outcome rows are
	// the only always-on visibility this subsystem has, so this is an opt-OUT.
	OutcomesDisabled bool
	// AlertsDisabled is DRIP_SUPPLY_ALERTS_DISABLED=1.
	AlertsDisabled bool

	// ShadowLedgerTable overrides ShadowLedgerTable (tests use a scratch name).
	ShadowLedgerTable string
	// ShadowPlanTable overrides ShadowPlanTable (tests use a scratch name).
	ShadowPlanTable string

	// Planner is WP6's daily planner, built once at boot with its rank source,
	// yield source, governors and contract key. Nil = the tick does not plan
	// (and says so once), because an un-injected planner would silently fall
	// back to seed yields and tier-only ranking on the live estate.
	Planner *Planner
	// PlanFunc overrides how a day is planned. Nil = Planner.Plan for the live
	// table, planShadow for the shadow one. It exists so the scheduling and
	// idempotency around the plan can be tested without standing up WP6's four
	// contract tables and their HMAC tokens (planner_test.go owns those), and
	// so a future caller can substitute a plan source.
	PlanFunc func(ctx context.Context, day time.Time) (*Plan, error)
	// PlannerDisabled is DRIP_PLANNER_DISABLED=1: the tick never plans and the
	// PlannerWorker never starts. The plan_share term then does not bind
	// (PlanRemaining fails OPEN on a missing row), so this degrades to the
	// pre-WP6 behaviour rather than stopping the estate.
	PlannerDisabled bool

	// ContractKey is the §1.5 HMAC key LoadActive verifies contract tokens
	// with. Nil = resolve it once from CONTRACT_TOKEN_KEY at construction.
	ContractKey []byte

	// ContractSource overrides how the tick loads its active contract set.
	// Nil = LoadActiveWithKey against the database, which is what production
	// uses. It exists so a caller that already holds a verified set (a future
	// per-tick cache shared with the WP6 planner, and the tests here) can hand
	// it in without re-reading and re-verifying four tables every 15 minutes.
	ContractSource func(ctx context.Context, day time.Time) (*ActiveSet, error)

	// Notifier delivers the §6 alerts. Nil = log only.
	Notifier notify.Notifier
	// AlertEvery rate-limits alerts to one per lane per window (§6: one hour).
	AlertEvery time.Duration

	Clock func() time.Time
}

// Mediator is the single entry point the orchestrator calls. One value per
// process; every method is safe on a nil receiver and safe for concurrent use.
type Mediator struct {
	db  *sql.DB
	svc *Service
	tr  *Transitions
	cfg MediatorConfig

	mu sync.Mutex
	// Per-tick state, reset by TickStart.
	tick      time.Time
	day       time.Time
	contracts *ActiveSet
	refilled  map[string]bool // sending domain -> RefillDomain already ran this tick

	// contractKey / contractKeyErr are resolved ONCE at construction (§1.5).
	// Re-reading the env every tick would let a key that vanished mid-life
	// silently start failing every lane closed with no single log line saying
	// when it changed.
	contractKey    []byte
	contractKeyErr error

	// Cross-tick state.
	plannedDay   string         // Denver dayKey this process has confirmed a frozen plan for
	activatedDay string         // dayKey of the last ActivateScheduled
	zeroStreak   map[string]int // "lane|pass" -> consecutive zero/failed outcomes
	lastAlert    map[string]time.Time

	plannerWarnOnce sync.Once
}

// NewMediator builds the mediator. `svc` may be nil only when mode is off.
func NewMediator(db *sql.DB, svc *Service, cfg MediatorConfig) *Mediator {
	if cfg.Mode == "" {
		cfg.Mode = ModeOff
	}
	if cfg.ExpireAfter <= 0 {
		cfg.ExpireAfter = DefaultExpireAfter
	}
	if cfg.ReapAge <= 0 {
		cfg.ReapAge = DefaultReapAge
	}
	if cfg.ReapBatch <= 0 {
		cfg.ReapBatch = DefaultReapBatch
	}
	if strings.TrimSpace(cfg.ShadowLedgerTable) == "" {
		cfg.ShadowLedgerTable = ShadowLedgerTable
	}
	if strings.TrimSpace(cfg.ShadowPlanTable) == "" {
		cfg.ShadowPlanTable = ShadowPlanTable
	}
	if cfg.AlertEvery <= 0 {
		cfg.AlertEvery = DefaultAlertEvery
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	m := &Mediator{
		db:         db,
		svc:        svc,
		tr:         NewTransitions(),
		cfg:        cfg,
		refilled:   map[string]bool{},
		zeroStreak: map[string]int{},
		lastAlert:  map[string]time.Time{},
	}
	if len(cfg.ContractKey) > 0 {
		m.contractKey = cfg.ContractKey
	} else {
		key, err := contractmeta.KeyFromEnv()
		if err != nil {
			m.contractKeyErr = err
			// Only worth saying out loud when the flag is on: with MODE=off no
			// contract is ever loaded and the key is irrelevant.
			if cfg.Mode != ModeOff {
				log.Printf("[DripSupply] %s: %v — every lane fails closed (%s) while the mode is %s",
					contractmeta.KeyEnvVar, err, SkipNoContractKey, cfg.Mode)
			}
		} else {
			m.contractKey = key
		}
	}
	return m
}

// Mode reports the configured mode; a nil mediator is off.
func (m *Mediator) Mode() Mode {
	if m == nil {
		return ModeOff
	}
	return m.cfg.Mode
}

// Enforcing reports whether ANY cell can be enforced this boot.
func (m *Mediator) Enforcing() bool { return m.Mode().Enforces() }

func (m *Mediator) now() time.Time {
	if m == nil || m.cfg.Clock == nil {
		return time.Now()
	}
	return m.cfg.Clock()
}

// TickStart is §2.8's tick preamble: activate contracts if the day rolled,
// ensure the day's balances, refill nothing yet (that happens per domain in
// Grant), expire stale reservations and reap orphan claims.
//
// It ALWAYS sets the tick timestamp — even with mode off — because the outcome
// rows are keyed on it and they are written in every mode. Everything below the
// timestamp is skipped when off, so `off` costs one map reset per tick.
//
// Errors are logged and swallowed on purpose: a contract-load failure must not
// stop the drip, it must fail the reservations closed (Grant then finds no
// contract and, in `on`, skips the wave with a visible outcome + alert).
func (m *Mediator) TickStart(ctx context.Context, now time.Time) {
	if m == nil {
		return
	}
	if now.IsZero() {
		now = m.now()
	}
	m.mu.Lock()
	m.tick = now.UTC().Truncate(time.Second)
	m.day = dayOf(now)
	m.contracts = nil
	m.refilled = map[string]bool{}
	mode := m.cfg.Mode
	m.mu.Unlock()

	if mode == ModeOff || m.svc == nil || m.db == nil {
		return
	}
	// §1.5: no key, no contracts. Say so once an hour and stop — activating or
	// loading with no key cannot succeed, and hammering it every tick would
	// bury the one line that names the missing env var.
	if m.contractKeyErr != nil && m.cfg.ContractSource == nil {
		m.alertOnce(ctx, "contract_key", notify.TierAlert,
			fmt.Sprintf("%s is unusable · every drip lane fails closed", contractmeta.KeyEnvVar),
			"Error: "+m.contractKeyErr.Error()+"\nMode: "+string(mode)+"\nEffect: no contract is loaded, no wave reserves",
			"Run: set "+contractmeta.KeyEnvVar+" (min 32 bytes) in the task definition")
		return
	}

	// (1) Day boundary: scheduled -> active, previous active -> superseded.
	// Guarded by the day key so a 15-min tick does not run the activation
	// transaction 96 times a day; an ECS bounce re-runs it once, which
	// ActivateScheduled is idempotent under.
	key := dayKey(now)
	m.mu.Lock()
	rollover := m.activatedDay != key
	m.mu.Unlock()
	if rollover {
		if res, err := ActivateScheduled(ctx, m.db, now); err != nil {
			log.Printf("[DripSupply] activate scheduled contracts: %v", err)
			m.alert(ctx, "activation", notify.TierAlert,
				fmt.Sprintf("contract activation failed · day %s", key),
				"Error: "+err.Error(), "Run: check drip_*_contracts status rows for "+key)
		} else {
			m.mu.Lock()
			m.activatedDay = key
			m.mu.Unlock()
			if res.Total() > 0 {
				log.Printf("[DripSupply] contracts activated for %s: %+v", key, res.Activated)
			}
		}
	}

	// (2) Contracts + balances for the day.
	load := m.cfg.ContractSource
	if load == nil {
		load = func(ctx context.Context, day time.Time) (*ActiveSet, error) {
			return LoadActiveWithKey(ctx, m.db, day, m.contractKey)
		}
	}
	set, err := load(ctx, now)
	if err != nil {
		log.Printf("[DripSupply] load active contracts for %s: %v — this tick reserves nothing", key, err)
		m.alert(ctx, "contracts", notify.TierAlert,
			fmt.Sprintf("contract load failed · day %s", key),
			"Error: "+err.Error(), "Run: GET /api/mailing/supply/contracts")
		return
	}
	m.mu.Lock()
	m.contracts = set
	m.mu.Unlock()

	if _, err := EnsureDayBalances(ctx, m.db, now, set); err != nil {
		log.Printf("[DripSupply] ensure day balances for %s: %v", key, err)
	}

	// (2b) The day's plan. The 00:05 MT PlannerWorker is the scheduled owner;
	// this is the SAFETY NET for the day it cannot serve — a deploy at 09:00,
	// a first boot, an ECS bounce that killed the 00:05 holder mid-pass. It is
	// gated on "no frozen rows for this Denver day", so on a normal day it
	// costs one EXISTS the first tick and nothing afterwards.
	m.ensureDailyPlan(ctx, now)

	// (3) Stale reservations. A reservation with no commit after 45 min means a
	// wave claimed capacity and then died between Reserve and Commit — the
	// capacity is handed back here, and the operator is told, because the mail
	// for it did NOT ship.
	if n, err := m.svc.ExpireStale(ctx, m.cfg.ExpireAfter); err != nil {
		log.Printf("[DripSupply] expire stale reservations: %v", err)
	} else if n > 0 {
		log.Printf("[DripSupply] expired %d reservations with no commit after %s", n, m.cfg.ExpireAfter)
		m.alert(ctx, "stale_reservations", notify.TierWarn,
			fmt.Sprintf("reservations expired with no commit · %d", n),
			fmt.Sprintf("Age: over %s\nEffect: capacity returned, the waves did not ship", m.cfg.ExpireAfter),
			"Run: SELECT * FROM drip_capacity_ledger WHERE status='expired' ORDER BY updated_at DESC LIMIT 20")
	}

	// (4) Orphan claims (§2.4). Covers the shape releaseStaleClaims cannot:
	// a claimed row that got as far as having a subscriber hydrated.
	if n, err := m.tr.Reap(ctx, m.db, m.cfg.ReapAge, m.cfg.ReapBatch); err != nil {
		log.Printf("[DripSupply] reap orphan claims: %v", err)
	} else if n > 0 {
		log.Printf("[DripSupply] reaped %d orphan claims older than %s", n, m.cfg.ReapAge)
	}
}

// Tick returns the timestamp every outcome row of this tick is keyed on.
func (m *Mediator) Tick() time.Time {
	if m == nil {
		return time.Time{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tick.IsZero() {
		return time.Now().UTC().Truncate(time.Second)
	}
	return m.tick
}

// -----------------------------------------------------------------------------
// Grant
// -----------------------------------------------------------------------------

// GrantReq is one wave's ask: a lane, the brand's sending domain, the touch
// class, and the ISPs the cap chain still wants after the layers that survive
// enforcement (§2.7: brand allow-list, apple-banned verticals, throughput
// safety, governors).
type GrantReq struct {
	Day        time.Time // zero = the tick's day
	Lane       string
	Brand      string
	Domain     string // sending domain, e.g. em.discountblog.com
	TouchClass string // intro | followup | remail
	Pass       string // welcome | followup | governed | aol_rotate
	WaveKey    string // stable per wave attempt; part of the idempotency key
	ISPs       []string
	// Requested is the per-ISP ask. The token bucket (§2.3) is the pacing
	// authority, so this is the wave's hard ceiling, not a per-ISP quota.
	Requested int
	// MailableSupply per ISP; a missing key (or a negative value) means
	// "unknown / not supply-bound" and the supply term does not participate.
	MailableSupply map[string]int
}

// Allocation is the mediator's answer. A nil *Allocation, or one whose
// EnforcedCaps() is nil, means "the old chain decides" — that is the contract
// that makes MODE=off byte-identical.
type Allocation struct {
	Mode     Mode
	Enforced bool   // Caps must REPLACE the chain for the ISPs in Caps
	Skip     bool   // fail closed: this wave must not run at all
	Reason   string // outcome reason when Skip, or the binding reason otherwise
	ID       uuid.UUID
	Caps     map[string]int

	m       *Mediator
	perISP  map[string]uuid.UUID
	grants  map[string]int
	settled bool
	mu      sync.Mutex
}

// EnforcedCaps returns the caps that replace the chain, or nil when the old
// chain must decide. Nil-receiver safe.
func (a *Allocation) EnforcedCaps() map[string]int {
	if a == nil || !a.Enforced {
		return nil
	}
	return a.Caps
}

// AllocationID is the id stamped on partner_clean_queue.capacity_allocation_id.
// Nil-receiver safe; uuid.Nil when nothing was granted.
func (a *Allocation) AllocationID() uuid.UUID {
	if a == nil {
		return uuid.Nil
	}
	return a.ID
}

// ShouldSkip reports a fail-closed wave (no contract, outside window, timeout).
func (a *Allocation) ShouldSkip() bool { return a != nil && a.Skip }

// SkipReason is the outcome reason for a skipped wave.
func (a *Allocation) SkipReason() string {
	if a == nil {
		return ""
	}
	return a.Reason
}

// Grant is §2.8's per-wave decision. It activates nothing (TickStart did),
// refills the domain's buckets once per tick, and issues ONE Reserve per ISP
// under the SAME wave key.
//
// Returns (nil, nil) instantly when mode is off — that is the zero-cost path.
func (m *Mediator) Grant(ctx context.Context, req GrantReq) (*Allocation, error) {
	if m == nil || m.cfg.Mode == ModeOff || m.svc == nil || m.db == nil {
		return nil, nil
	}
	if len(req.ISPs) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(req.WaveKey) == "" {
		return nil, errors.New("dripsupply: Grant requires a WaveKey")
	}
	if strings.TrimSpace(req.TouchClass) == "" {
		req.TouchClass = "intro"
	}

	day := req.Day
	m.mu.Lock()
	if day.IsZero() {
		day = m.day
	}
	set := m.contracts
	mode := m.cfg.Mode
	canary := m.cfg.Canary
	m.mu.Unlock()

	if day.IsZero() {
		day = dayOf(m.now())
	}
	if m.contractKeyErr != nil && m.cfg.ContractSource == nil {
		// §1.5 fail-closed: without CONTRACT_TOKEN_KEY no contract can be
		// verified, so no cell may be enforced and none may be shadowed either.
		return m.failClosed(ctx, mode, canary, req, SkipNoContractKey), nil
	}
	if set == nil {
		// TickStart could not load the contracts. Fail CLOSED when we would
		// otherwise enforce; stay out of the way when we would only shadow.
		return m.failClosed(ctx, mode, canary, req, SkipNoContract), nil
	}

	dc, derr := set.Domain(req.Domain)
	if derr != nil {
		m.alertOnce(ctx, "contract:"+req.Domain, notify.TierAlert,
			fmt.Sprintf("no active domain contract · %s", req.Domain),
			"Lane: "+req.Lane+"\nEffect: the wave is skipped while the mode enforces this cell",
			"Run: POST /api/mailing/supply/contracts/domain/"+req.Domain)
		return m.failClosed(ctx, mode, canary, req, SkipNoContract), nil
	}
	pc, perr := set.Dispatch(req.Lane)
	if perr != nil {
		m.alertOnce(ctx, "contract:"+req.Lane, notify.TierAlert,
			fmt.Sprintf("no active dispatch contract · %s", req.Lane),
			"Domain: "+req.Domain+"\nEffect: the wave is skipped while the mode enforces this cell",
			"Run: POST /api/mailing/supply/contracts/dispatch/"+req.Lane)
		return m.failClosed(ctx, mode, canary, req, SkipNoContract), nil
	}

	win, werr := WindowOf(dc)
	if werr != nil {
		return m.failClosed(ctx, mode, canary, req, SkipNoContract), nil
	}

	// Refill this domain's buckets ONCE per tick. Two waves for the same brand
	// in one tick must not each mint an interval of tokens.
	m.mu.Lock()
	needRefill := !m.refilled[dc.SendingDomain]
	if needRefill {
		m.refilled[dc.SendingDomain] = true
	}
	m.mu.Unlock()
	if needRefill {
		if _, err := m.svc.RefillDomain(ctx, day, dc); err != nil {
			log.Printf("[DripSupply] refill %s: %v", dc.SendingDomain, err)
		}
	}

	alloc := &Allocation{
		Mode:   mode,
		m:      m,
		perISP: map[string]uuid.UUID{},
		grants: map[string]int{},
		Caps:   map[string]int{},
	}

	isps := append([]string(nil), req.ISPs...)
	sort.Strings(isps)
	for _, raw := range isps {
		isp := normISP(raw)
		if isp == "" {
			continue
		}
		supply := -1
		if req.MailableSupply != nil {
			if v, ok := req.MailableSupply[isp]; ok {
				supply = v
			}
		}
		rr := ReserveReq{
			Day:             day,
			Domain:          dc.SendingDomain,
			ISP:             isp,
			Lane:            req.Lane,
			TouchClass:      req.TouchClass,
			WaveKey:         req.WaveKey,
			Requested:       req.Requested,
			MailableSupply:  supply,
			DomainVersion:   dc.Version,
			DispatchVersion: pc.Version,
			Win:             win,
		}

		enforce := mode == ModeOn || (mode == ModeCanary && canaryMatch(canary, dc.SendingDomain, isp, req.Lane))
		if !enforce {
			// SHADOW: identical arithmetic, read-only on the live balances,
			// row written to the shadow ledger. Never touches partner_clean_queue.
			if _, _, err := m.shadowReserve(ctx, rr); err != nil {
				log.Printf("[DripSupply] shadow reserve %s/%s/%s: %v", rr.Domain, rr.ISP, rr.Lane, err)
			}
			continue
		}

		res, err := m.svc.Reserve(ctx, rr)
		if err != nil {
			return nil, fmt.Errorf("dripsupply: reserve %s/%s/%s: %w", rr.Domain, rr.ISP, rr.Lane, err)
		}
		alloc.Enforced = true
		alloc.Caps[isp] = res.Granted
		alloc.grants[isp] = res.Granted
		if res.Granted > 0 {
			alloc.perISP[isp] = res.AllocationID
		}
		if res.BindingReason == ReasonReserveTimeout {
			// §2.2: no ledger row was written, so this is invisible unless the
			// executor says so.
			alloc.Reason = SkipReserveTimeout
			m.alertOnce(ctx, "timeout:"+req.Lane, notify.TierAlert,
				fmt.Sprintf("reserve timed out · %s/%s", rr.Domain, rr.ISP),
				"Lane: "+req.Lane+"\nEffect: 0 granted for this ISP, the wave key stays retryable",
				"Run: check drip_capacity_balance row contention for "+rr.Domain)
		} else if alloc.Reason == "" {
			alloc.Reason = res.BindingReason
		}
	}

	if !alloc.Enforced {
		// Shadow-only wave: caps stay nil so the old chain runs unchanged.
		alloc.Caps = nil
		alloc.Reason = string(mode)
		return alloc, nil
	}

	// The id stamped on the claimed rows: the largest grant, ties broken by ISP
	// name so two instances replaying the same wave pick the same one.
	best, bestISP := 0, ""
	for _, isp := range sortedKeys(alloc.grants) {
		if g := alloc.grants[isp]; g > best {
			best, bestISP = g, isp
		}
	}
	if bestISP != "" {
		alloc.ID = alloc.perISP[bestISP]
	}
	return alloc, nil
}

// failClosed builds the Allocation for a wave whose contracts could not be
// resolved. When the mode would ENFORCE this cell the wave is skipped; when it
// would only shadow it, the old chain runs and the outcome row records why the
// shadow produced nothing.
func (m *Mediator) failClosed(ctx context.Context, mode Mode, canary []CanaryCell, req GrantReq, reason string) *Allocation {
	_ = ctx
	enforced := mode == ModeOn
	if mode == ModeCanary {
		for _, isp := range req.ISPs {
			if canaryMatch(canary, req.Domain, normISP(isp), req.Lane) {
				enforced = true
				break
			}
		}
	}
	return &Allocation{Mode: mode, Skip: enforced, Reason: reason, m: m}
}

// -----------------------------------------------------------------------------
// Commit / Release
// -----------------------------------------------------------------------------

// Commit settles the wave. `perISPSubmitted` is the exact per-ISP count that
// entered a campaign; anything reserved and not submitted is released in the
// same pass, so a partially-deployed wave hands its remainder straight back
// instead of waiting 45 minutes for ExpireStale.
//
// Nil-receiver and not-enforced safe.
func (a *Allocation) Commit(ctx context.Context, perISPSubmitted map[string]int, campaignID uuid.UUID) error {
	if a == nil || !a.Enforced || a.m == nil || a.m.svc == nil {
		return nil
	}
	a.mu.Lock()
	if a.settled {
		a.mu.Unlock()
		return nil
	}
	a.settled = true
	a.mu.Unlock()

	var firstErr error
	for _, isp := range sortedKeys(a.grants) {
		id, ok := a.perISP[isp]
		if !ok || id == uuid.Nil {
			continue
		}
		granted := a.grants[isp]
		submitted := perISPSubmitted[isp]
		if submitted < 0 {
			submitted = 0
		}
		if submitted > granted {
			submitted = granted
		}
		if submitted > 0 {
			if err := a.m.svc.Commit(ctx, id, submitted, campaignID); err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		// Nothing shipped on this ISP: hand the whole grant back now.
		if err := a.m.svc.Release(ctx, id, granted, "not_submitted"); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Release hands the entire outstanding reservation back. Used on every failure
// path the orchestrator already releases records on (deferral, promote failure,
// creative miss, deploy failure).
func (a *Allocation) Release(ctx context.Context, reason string) error {
	if a == nil || !a.Enforced || a.m == nil || a.m.svc == nil {
		return nil
	}
	a.mu.Lock()
	if a.settled {
		a.mu.Unlock()
		return nil
	}
	a.settled = true
	a.mu.Unlock()

	var firstErr error
	for _, isp := range sortedKeys(a.grants) {
		id, ok := a.perISP[isp]
		if !ok || id == uuid.Nil {
			continue
		}
		if err := a.m.svc.Release(ctx, id, a.grants[isp], reason); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// SplitSubmitted distributes a wave's TOTAL submitted count across its ISPs
// using the claimed-record tally, largest-remainder. When every claimed record
// shipped (the overwhelmingly common case) the result is exact; it approximates
// only when a wave GROUP failed to deploy and the orchestrator knows the total
// but not which group's records were released.
func SplitSubmitted(tally map[string]int, submitted int) map[string]int {
	out := make(map[string]int, len(tally))
	total := 0
	for _, n := range tally {
		if n > 0 {
			total += n
		}
	}
	if total <= 0 || submitted <= 0 {
		return out
	}
	if submitted >= total {
		for isp, n := range tally {
			if n > 0 {
				out[normISP(isp)] = n
			}
		}
		return out
	}
	type rem struct {
		isp  string
		frac float64
	}
	assigned := 0
	rems := make([]rem, 0, len(tally))
	for _, isp := range sortedKeys(tally) {
		n := tally[isp]
		if n <= 0 {
			continue
		}
		exact := float64(n) * float64(submitted) / float64(total)
		base := int(exact)
		out[normISP(isp)] = base
		assigned += base
		rems = append(rems, rem{normISP(isp), exact - float64(base)})
	}
	sort.Slice(rems, func(i, j int) bool {
		if rems[i].frac != rems[j].frac {
			return rems[i].frac > rems[j].frac
		}
		return rems[i].isp < rems[j].isp
	})
	for i := 0; assigned < submitted && i < len(rems); i++ {
		out[rems[i].isp]++
		assigned++
	}
	return out
}

// -----------------------------------------------------------------------------
// Shadow reserve
// -----------------------------------------------------------------------------

// shadowReserve is the §7 step-2 computation: EXACTLY the min() of
// Service.Reserve (reservation.go, step 3), evaluated against the live balance
// rows WITHOUT locking or decrementing them, with the resulting row written to
// the shadow ledger.
//
// Why it is a separate implementation and not Service.Reserve pointed at
// another table: Reserve's transaction takes `FOR UPDATE` on the LIVE
// drip_capacity_balance and drip_lane_balance rows and decrements them. There
// is no shadow twin of those tables, so re-pointing only the ledger would leave
// shadow mode consuming real capacity — the one thing §7 step 2 forbids
// ("not the live ledger, no locks on pcq"). The arithmetic below is therefore
// duplicated on purpose and pinned by TestShadowReserveMatchesReserve.
func (m *Mediator) shadowReserve(ctx context.Context, req ReserveReq) (int, string, error) {
	if m == nil || m.db == nil {
		return 0, "", nil
	}
	if err := req.validate(); err != nil {
		return 0, "", err
	}
	req.ISP = normISP(req.ISP)
	key := req.IdempotencyKey()

	if (req.Win != Window{}) && !req.Win.Contains(req.Day, m.now()) {
		return 0, ReasonOutsideWindow, m.writeShadowRow(ctx, req, key, 0, StatusReleased, ReasonOutsideWindow, 0, 0)
	}

	var bal Balance
	err := m.db.QueryRowContext(ctx, `
		SELECT contracted, effective, effective_reason, tokens, reserved, committed
		FROM drip_capacity_balance
		WHERE day = $1::date AND sending_domain = $2 AND isp = $3
	`, dayKey(req.Day), req.Domain, req.ISP).
		Scan(&bal.Contracted, &bal.Effective, &bal.EffectiveReason, &bal.Tokens, &bal.Reserved, &bal.Committed)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ReasonNoBalance, m.writeShadowRow(ctx, req, key, 0, StatusReleased, ReasonNoBalance, 0, 0)
	}
	if err != nil {
		return 0, "", fmt.Errorf("shadow read domain balance %s/%s: %w", req.Domain, req.ISP, err)
	}

	var lane LaneBalance
	err = m.db.QueryRowContext(ctx, `
		SELECT desired, unfilled, reserved, committed
		FROM drip_lane_balance
		WHERE day = $1::date AND lane = $2 AND isp = $3
	`, dayKey(req.Day), req.Lane, req.ISP).
		Scan(&lane.Desired, &lane.Unfilled, &lane.Reserved, &lane.Committed)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ReasonNoLaneBalance, m.writeShadowRow(ctx, req, key, 0, StatusReleased, ReasonNoLaneBalance, bal.Headroom(), 0)
	}
	if err != nil {
		return 0, "", fmt.Errorf("shadow read lane balance %s/%s: %w", req.Lane, req.ISP, err)
	}

	granted, reason := shadowTerms(bal, lane, req)
	status := StatusReserved
	if granted <= 0 {
		status = StatusReleased
	}
	return granted, reason, m.writeShadowRow(ctx, req, key, granted, status, reason, bal.Headroom()-granted, lane.Unfilled-granted)
}

// shadowTerms mirrors reservation.go's step (3) term list, INCLUDING its
// ordering (which is the binding-reason tie-break) and its governor-label rule.
// The plan term is omitted: WP6's PlanReader is called inside Reserve's
// transaction and shadow mode has none.
func shadowTerms(bal Balance, lane LaneBalance, req ReserveReq) (int, string) {
	domainReason := ReasonDomainTokens
	if bal.Effective < bal.Contracted {
		name := strings.TrimSpace(bal.EffectiveReason)
		if name == "" {
			name = "reduced"
		}
		domainReason = ReasonGovernor + ":" + name
	}
	terms := []term{
		{ReasonRequested, req.Requested},
		{domainReason, bal.Headroom()},
		{ReasonDomainTokens, int(math.Floor(bal.Tokens))},
		{ReasonLaneDemand, lane.Unfilled},
	}
	if req.MailableSupply >= 0 {
		terms = append(terms, term{ReasonSupply, req.MailableSupply})
	}
	return bindingMin(terms)
}

func (m *Mediator) writeShadowRow(ctx context.Context, req ReserveReq, key string, reserved int, status, reason string, domainAfter, laneAfter int) error {
	if domainAfter < 0 {
		domainAfter = 0
	}
	if laneAfter < 0 {
		laneAfter = 0
	}
	// The table name is a package constant (or a test override), never caller
	// input — there is no interpolation of any request field here.
	q := `
		INSERT INTO ` + m.cfg.ShadowLedgerTable + ` (
			allocation_id, idempotency_key, day, tick, sending_domain, isp, lane, touch_class,
			domain_contract_version, dispatch_contract_version,
			requested, reserved, committed, released, status, campaign_id, binding_reason,
			domain_balance_after, lane_unfilled_after, created_at, updated_at
		) VALUES ($1, $2, $3::date, $4, $5, $6, $7, $8, $9, $10, $11, $12, 0, 0, $13, NULL, $14, $15, $16, NOW(), NOW())
		ON CONFLICT (idempotency_key) DO NOTHING`
	_, err := m.db.ExecContext(ctx, q,
		uuid.New(), key, dayKey(req.Day), m.Tick(), req.Domain, normISP(req.ISP), req.Lane, req.TouchClass,
		req.DomainVersion, req.DispatchVersion, req.Requested, reserved, status, reason,
		domainAfter, laneAfter,
	)
	if err != nil {
		return fmt.Errorf("shadow ledger insert (key=%s): %w", key, err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Outcomes
// -----------------------------------------------------------------------------

// Outcome writes one drip_tick_outcomes row. It runs in EVERY mode, `off`
// included, and is the only always-on evidence that a lane was considered.
// Never returns an error to the caller: a failed outcome write must not abort a
// wave that is otherwise fine.
func (m *Mediator) Outcome(ctx context.Context, r OutcomeRow) {
	if m == nil || m.db == nil || m.cfg.OutcomesDisabled {
		return
	}
	lane := strings.TrimSpace(r.Lane)
	if lane == "" {
		return
	}
	pass := strings.TrimSpace(r.Pass)
	if pass == "" {
		pass = PassWelcome
	}
	switch r.Outcome {
	case OutcomeFired, OutcomeSkipped, OutcomeZero, OutcomeFailed:
	default:
		r.Outcome = OutcomeSkipped
	}
	reason := strings.TrimSpace(r.Reason)
	if b := strings.TrimSpace(r.Brand); b != "" {
		if reason == "" {
			reason = "brand=" + b
		} else {
			reason = reason + " brand=" + b
		}
	}
	caps := "{}"
	if len(r.CapsSeen) > 0 {
		if b, err := json.Marshal(r.CapsSeen); err == nil {
			caps = string(b)
		}
	}
	var campaign any
	if id := strings.TrimSpace(r.CampaignID); id != "" {
		if u, err := uuid.Parse(id); err == nil {
			campaign = u
		}
	}
	if _, err := m.db.ExecContext(ctx, upsertOutcomeSQL,
		m.Tick(), lane, pass, r.Outcome, reason, caps, r.Claimed, campaign, outcomePriority(r.Outcome),
	); err != nil {
		log.Printf("[DripSupply] tick outcome %s/%s=%s: %v", lane, pass, r.Outcome, err)
		return
	}
	m.trackStreak(ctx, lane, pass, r.Outcome, reason)
}

// trackStreak implements the §6 "two consecutive zero/failed ticks with demand"
// alert. The streak is per (lane, pass) and resets on any fired outcome.
func (m *Mediator) trackStreak(ctx context.Context, lane, pass, outcome, reason string) {
	key := lane + "|" + pass
	m.mu.Lock()
	switch outcome {
	case OutcomeZero, OutcomeFailed:
		m.zeroStreak[key]++
	default:
		delete(m.zeroStreak, key)
	}
	n := m.zeroStreak[key]
	m.mu.Unlock()
	if n < 2 {
		return
	}
	tier := notify.TierWarn
	if outcome == OutcomeFailed {
		tier = notify.TierAlert
	}
	m.alertOnce(ctx, "lane:"+lane, tier,
		fmt.Sprintf("%s produced nothing for %d ticks · %s", lane, n, pass),
		"Reason: "+reason+"\nPass: "+pass,
		"Run: SELECT * FROM drip_tick_outcomes WHERE lane='"+lane+"' ORDER BY tick DESC LIMIT 10")
}

// -----------------------------------------------------------------------------
// Alerts (§6)
// -----------------------------------------------------------------------------

// alertOnce rate-limits to one alert per key per AlertEvery window (§6: one per
// lane per hour). The window is in-process: a restart re-arms it, which is the
// correct bias for an alert that says a lane is not shipping.
func (m *Mediator) alertOnce(ctx context.Context, key string, tier notify.Tier, headline, body, action string) {
	if m == nil || m.cfg.AlertsDisabled {
		return
	}
	now := m.now()
	m.mu.Lock()
	last, seen := m.lastAlert[key]
	if seen && now.Sub(last) < m.cfg.AlertEvery {
		m.mu.Unlock()
		return
	}
	m.lastAlert[key] = now
	m.mu.Unlock()
	m.deliver(ctx, tier, headline, body, action)
}

// alert is the un-rate-limited form, used for the tick-level janitor findings
// that already run at most once per tick.
func (m *Mediator) alert(ctx context.Context, key string, tier notify.Tier, headline, body, action string) {
	m.alertOnce(ctx, key, tier, headline, body, action)
}

func (m *Mediator) deliver(ctx context.Context, tier notify.Tier, headline, body, action string) {
	_ = ctx
	msg := notify.Message{
		Tier:     tier,
		Scope:    notify.ScopeDrip,
		Headline: headline,
		Body:     body,
		Action:   action,
	}
	if m.cfg.Notifier == nil {
		log.Printf("[DripSupply] ALERT %s: %s | %s", tier, headline, strings.ReplaceAll(body, "\n", " · "))
		return
	}
	if err := notify.Deliver(m.cfg.Notifier, msg); err != nil {
		log.Printf("[DripSupply] alert delivery failed (%s): %v", headline, err)
	}
}

// -----------------------------------------------------------------------------
// The daily plan (§2.5) — scheduling and the shadow twin
// -----------------------------------------------------------------------------

// plannerLockKey is the distributed lock the 00:05 pass contends on, in the
// same shape as economicsLockKey / supplyLockKey. Two orchestrator instances
// run (desiredCount=2); the plan write is transactional and Plan(replan=false)
// is idempotent, so the lock is about not paying for the read phase twice, not
// about correctness.
const plannerLockKey = "drip:planner:daily"

// DefaultPlannerRunAfter is §2.5's 00:05 MT.
const DefaultPlannerRunAfter = 5 * time.Minute

// planTable is the table this mediator's plan lands in: the live one when a
// cell can be enforced, the shadow twin in shadow mode (§7 step 2).
func (m *Mediator) planTable() string {
	if m.cfg.Mode == ModeShadow {
		return m.cfg.ShadowPlanTable
	}
	return "drip_daily_plan"
}

// planFrozen reports whether `table` already carries frozen rows for the day.
// EXISTS, not a row count and not LoadStoredPlan: the check runs on every tick
// until it is true, and loading a 30-lane × 12-ISP × 29-domain plan to answer
// "has this day been planned" would read thousands of rows every 15 minutes.
func planFrozen(ctx context.Context, q Queryer, table string, day time.Time) (bool, error) {
	var ok bool
	err := q.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM `+table+` WHERE day = $1::date AND frozen_at IS NOT NULL)`,
		dayKey(day)).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("dripsupply: plan frozen check on %s for %s: %w", table, dayKey(day), err)
	}
	return ok, nil
}

// ensureDailyPlan plans the Denver day if nothing has yet. Idempotent three
// times over: an in-process day cache, an EXISTS on the plan table, and
// Planner.Plan's own frozen-day short circuit.
//
// Failures are logged and alerted, never fatal: with no plan row PlanRemaining
// fails OPEN (bounded=false), so the domain balance, lane balance and supply
// terms still bind and a planner outage cannot stop the estate mailing.
func (m *Mediator) ensureDailyPlan(ctx context.Context, now time.Time) {
	if m == nil || m.db == nil || m.cfg.PlannerDisabled {
		return
	}
	day := DenverDay(now)
	key := dayKey(day)

	m.mu.Lock()
	already := m.plannedDay == key
	m.mu.Unlock()
	if already {
		return
	}

	frozen, err := planFrozen(ctx, m.db, m.planTable(), day)
	if err != nil {
		log.Printf("[DripSupply] %v", err)
		return
	}
	if frozen {
		m.mu.Lock()
		m.plannedDay = key
		m.mu.Unlock()
		return
	}

	if m.cfg.Planner == nil && m.cfg.PlanFunc == nil {
		m.plannerWarnOnce.Do(func() {
			log.Printf("[DripSupply] no Planner injected — %s will not be planned by the tick; the plan_share term does not bind", key)
		})
		return
	}

	plan, perr := m.runPlan(ctx, day)
	if perr != nil {
		log.Printf("[DripSupply] daily plan for %s FAILED: %v — the plan does not bind this tick", key, perr)
		m.alertOnce(ctx, "planner", notify.TierAlert,
			fmt.Sprintf("daily plan failed · %s", key),
			"Error: "+perr.Error()+"\nEffect: plan_share does not bind; balances and supply still do",
			"Run: GET /api/mailing/supply/plan?day="+key)
		return
	}
	m.mu.Lock()
	m.plannedDay = key
	m.mu.Unlock()
	log.Printf("[DripSupply] daily plan for %s ready in %s: %d rows, firm=%d provisional=%d",
		key, m.planTable(), len(plan.Rows), plan.TotalFirm(), plan.TotalProvisional())
}

// runPlan dispatches to the live planner or the shadow writer.
func (m *Mediator) runPlan(ctx context.Context, day time.Time) (*Plan, error) {
	if m.cfg.PlanFunc != nil {
		return m.cfg.PlanFunc(ctx, day)
	}
	if m.cfg.Mode == ModeShadow {
		return m.planShadow(ctx, day)
	}
	return m.cfg.Planner.Plan(ctx, m.db, day, false)
}

// planShadow is the §7 step-2 plan: WP6's read + assign phases verbatim, with
// the rows written to the shadow table and NOTHING else touched.
//
// It is a separate writer rather than a table option on WP6's Planner for the
// same reason shadowReserve is separate from Reserve: Planner.store also calls
// EnsureDayBalances and upserts drip_lane_balance — the rows the LIVE executor
// reserves against. A shadow plan that rewrote `desired`, `awarded_*` and
// `unfilled` would move the live chain's ceilings, which is precisely what
// shadow mode must not do.
func (m *Mediator) planShadow(ctx context.Context, day time.Time) (*Plan, error) {
	in, err := m.cfg.Planner.ReadInputs(ctx, m.db, day)
	if err != nil {
		return nil, err
	}
	plan := assign(in)
	if len(plan.Rows) > maxPlanRows {
		return nil, fmt.Errorf("dripsupply: shadow plan produced %d rows for %s, above the %d bound",
			len(plan.Rows), dayKey(day), maxPlanRows)
	}
	if err := m.storeShadowPlan(ctx, &plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

// storeShadowPlan writes one day into the shadow plan table in ONE transaction
// (delete the day, insert the rows), mirroring Planner.store's crash-window
// rule minus every live-table write.
func (m *Mediator) storeShadowPlan(ctx context.Context, plan *Plan) error {
	table := m.cfg.ShadowPlanTable
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dripsupply: shadow plan begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	key := dayKey(plan.Day)
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE day = $1::date`, key); err != nil {
		return fmt.Errorf("dripsupply: shadow plan clear %s: %w", key, err)
	}
	for start := 0; start < len(plan.Rows); start += planInsertChunk {
		end := min(start+planInsertChunk, len(plan.Rows))
		chunk := plan.Rows[start:end]
		var sb strings.Builder
		sb.WriteString(`INSERT INTO ` + table + `
			(day, lane, isp, sending_domain, award_firm, award_provisional, followups_reserved, plan_share,
			 rank, rank_reason, unserved, unserved_reason, supply_released, frozen_at) VALUES `)
		args := make([]any, 0, len(chunk)*planInsertCols)
		for i, r := range chunk {
			if i > 0 {
				sb.WriteString(", ")
			}
			b := i * planInsertCols
			fmt.Fprintf(&sb, "($%d::date,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
				b+1, b+2, b+3, b+4, b+5, b+6, b+7, b+8, b+9, b+10, b+11, b+12, b+13, b+14)
			args = append(args, key, r.Lane, r.ISP, r.SendingDomain, r.AwardFirm, r.AwardProvisional,
				r.FollowupsReserved, r.PlanShare, r.Rank, r.RankReason,
				r.Unserved, r.UnservedReason, r.SupplyReleased, plan.FrozenAt)
		}
		if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("dripsupply: shadow plan insert rows %d-%d: %w", start, end, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("dripsupply: shadow plan commit %s: %w", key, err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// PlannerWorker — the 00:05 MT scheduled pass
// -----------------------------------------------------------------------------

// PlannerWorkerConfig is what main.go supplies.
type PlannerWorkerConfig struct {
	Mediator *Mediator     // the plan is run THROUGH the mediator, so mode + shadow table are shared
	Interval time.Duration // poll cadence; the run is gated on the Denver clock
	RunAfter time.Duration // how far past Denver midnight the pass fires (default 00:05)
	NowFn    func() time.Time
}

// PlannerWorker fires the daily plan at 00:05 MT, once per Denver day, under the
// distributed lock. It is the SCHEDULED owner of the plan; Mediator.TickStart is
// the safety net for the day this worker cannot serve (first boot, a deploy at
// 09:00, an instance killed mid-pass).
type PlannerWorker struct {
	med   *Mediator
	db    *sql.DB
	redis *redis.Client
	loc   *time.Location

	disabled bool
	interval time.Duration
	runAfter time.Duration
	nowFn    func() time.Time

	// lastRanDay is advanced only on SUCCESS, so a failed pass retries on the
	// next poll instead of being silently skipped until tomorrow.
	lastRanDay string
}

// NewPlannerWorker builds the 00:05 MT pass. Kill switch: DRIP_PLANNER_DISABLED=1.
func NewPlannerWorker(db *sql.DB, redisClient *redis.Client, cfg PlannerWorkerConfig) *PlannerWorker {
	w := &PlannerWorker{
		med:      cfg.Mediator,
		db:       db,
		redis:    redisClient,
		interval: cfg.Interval,
		runAfter: cfg.RunAfter,
		nowFn:    cfg.NowFn,
	}
	if w.interval <= 0 {
		w.interval = 10 * time.Minute
	}
	if w.runAfter <= 0 {
		w.runAfter = DefaultPlannerRunAfter
	}
	if w.nowFn == nil {
		w.nowFn = time.Now
	}
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		log.Printf("[DripPlanner] America/Denver unavailable (%v) — falling back to UTC day boundaries", err)
		loc = time.UTC
	}
	w.loc = loc
	if v := os.Getenv("DRIP_PLANNER_DISABLED"); v == "1" || strings.EqualFold(v, "true") {
		log.Println("[DripPlanner] DRIP_PLANNER_DISABLED set — the daily planner pass is disabled")
		w.disabled = true
	}
	// A worker with no mediator, no planner, or MODE=off would poll every ten
	// minutes to do nothing. MODE=off is the rollback: nothing reads the plan,
	// so nothing plans.
	if w.med == nil || w.med.cfg.PlannerDisabled || w.med.Mode() == ModeOff ||
		(w.med.cfg.Planner == nil && w.med.cfg.PlanFunc == nil) {
		w.disabled = true
	}
	return w
}

// Disabled reports whether this worker will do anything. Exported so the boot
// log states it rather than leaving a dark worker looking alive.
func (w *PlannerWorker) Disabled() bool { return w == nil || w.disabled }

// Start polls every `interval` and fires once per Denver day at or after
// `runAfter`. Single goroutine; honors ctx.Done().
func (w *PlannerWorker) Start(ctx context.Context) {
	if w.Disabled() {
		return
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

func (w *PlannerWorker) tick(ctx context.Context) {
	now := w.nowFn().In(w.loc)
	today := dayOf(now)
	key := today.Format("2006-01-02")
	if now.Before(today.Add(w.runAfter)) {
		return
	}
	if w.lastRanDay == key {
		return
	}

	lock := distlock.NewLock(w.redis, w.db, plannerLockKey, 30*time.Minute)
	ok, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[DripPlanner] lock acquire failed: %v", err)
		return
	}
	if !ok {
		// The other instance is running this pass. Do NOT advance lastRanDay:
		// if that instance dies mid-pass this one retries on the next poll.
		return
	}
	defer func() {
		relCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := lock.Release(relCtx); err != nil {
			log.Printf("[DripPlanner] lock release failed: %v", err)
		}
	}()

	if err := w.RunOnce(ctx, today); err != nil {
		log.Printf("[DripPlanner] daily pass for %s FAILED: %v (will retry in %s)", key, err, w.interval)
		return
	}
	w.lastRanDay = key
}

// RunOnce plans one Denver day, unless it is already frozen. Exported so an
// operator (and WP9's API) can force the pass without waiting for midnight.
func (w *PlannerWorker) RunOnce(ctx context.Context, day time.Time) error {
	if w == nil || w.med == nil {
		return errors.New("dripsupply: PlannerWorker.RunOnce with no mediator")
	}
	day = dayOf(day.In(w.loc))
	frozen, err := planFrozen(ctx, w.db, w.med.planTable(), day)
	if err != nil {
		return err
	}
	if frozen {
		w.med.mu.Lock()
		w.med.plannedDay = dayKey(day)
		w.med.mu.Unlock()
		return nil
	}
	plan, err := w.med.runPlan(ctx, day)
	if err != nil {
		return err
	}
	w.med.mu.Lock()
	w.med.plannedDay = dayKey(day)
	w.med.mu.Unlock()
	log.Printf("[DripPlanner] %s planned into %s: %d rows, firm=%d provisional=%d followups=%d",
		dayKey(day), w.med.planTable(), len(plan.Rows), plan.TotalFirm(), plan.TotalProvisional(), plan.TotalFollowupsReserved())
	return nil
}
