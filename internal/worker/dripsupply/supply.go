package dripsupply

// supply.go — REQ-118 WP7: the hourly supply controller (§2.6) and the measured
// EO yield the WP6 planner injects (§2.5 step 6).
//
// What this file is for, in one line: the planner awards capacity it BELIEVES
// supply can back; this controller is the thing that makes that belief true, by
// promoting, resurrecting or buying records BEFORE the executor needs them.
//
// Three properties are load-bearing and every change here must preserve them:
//
//  1. IT NEVER BUYS FOR MAIL THAT CANNOT SHIP. A cap-0 or excluded ISP, a
//     paused/inactive/state-less lane, and a lane×ISP already governed to zero
//     get NO order — §2.6 and §5.5, and the reason a verdict bought for a lane
//     that cannot mail is pure burn (`eo-deadletter-unverdicted-burn`).
//  2. EVERY ACTION LEAVES A LEDGER ROW. A promotion writes MAILABLE, a
//     resurrection writes REMAIL_ELIGIBLE, an order writes VALIDATION_ORDERED
//     with its unit and total cost. A decision that moved nothing writes a
//     drip_tick_outcomes row instead, so "the controller ran and did nothing"
//     and "the controller did not run" are distinguishable from the outside.
//     That distinction is the SegmentMaterializer defect this must not repeat.
//  3. THE FILL ORDER IS FRESH → REMAIL → EO (§2.6). Cheapest first: a held row
//     with a live verdict costs nothing, a resurrection costs nothing but
//     frequency, and only what neither can cover is bought.
//
// Mode (`DRIP_SUPPLY_CHAIN_MODE`, shared with WP5's executor):
//
//	off     RunOnce returns immediately. Nothing is read, nothing is written.
//	shadow  Everything is computed and the ledger rows land in
//	        drip_supply_ledger_shadow. NO partner_clean_queue row is touched.
//	canary  Only the cells named in DRIP_SUPPLY_CANARY act on the queue; every
//	        other cell is shadowed.
//	on      Every cell acts.
//
// Kill switch: DRIP_SUPPLY_CONTROLLER_DISABLED=1 stops the worker outright,
// independently of the mode (the mode is the cutover dial; this is the stop).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	"github.com/ignite/sparkpost-monitor/internal/notify"
	"github.com/ignite/sparkpost-monitor/internal/pkg/contractmeta"
	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
)

// -----------------------------------------------------------------------------
// Supply ledger vocabulary (§1.2)
// -----------------------------------------------------------------------------

// The drip_supply_ledger.event CHECK, verbatim. RECEIVED / PRECHECK_PASSED /
// SUPPRESSED / INTERNAL_INVALID belong to the ingest path and are declared here
// so the one vocabulary lives in one place.
const (
	SupplyEventReceived            = "RECEIVED"
	SupplyEventPrecheckPassed      = "PRECHECK_PASSED"
	SupplyEventSuppressed          = "SUPPRESSED"
	SupplyEventInternalInvalid     = "INTERNAL_INVALID"
	SupplyEventValidationOrdered   = "VALIDATION_ORDERED"
	SupplyEventValidationValid     = "VALIDATION_VALID"
	SupplyEventValidationInvalid   = "VALIDATION_INVALID"
	SupplyEventValidationNoVerdict = "VALIDATION_NO_VERDICT"
	SupplyEventMailable            = "MAILABLE"
	SupplyEventRemailEligible      = "REMAIL_ELIGIBLE"
	SupplyEventReservedForIntro    = "RESERVED_FOR_INTRO"
	SupplyEventConsumed            = "CONSUMED"
	SupplyEventExpired             = "EXPIRED"
	SupplyEventReleased            = "RELEASED"
)

// SupplyLedgerTable / ShadowSupplyLedgerTable are the live table and its §7
// step-2 twin. Shadow mode writes the twin and never locks a queue row.
const (
	SupplyLedgerTable       = "drip_supply_ledger"
	ShadowSupplyLedgerTable = "drip_supply_ledger_shadow"
)

// PassSupply is the drip_tick_outcomes `pass` this controller writes under. The
// (tick, lane, pass) grain means the supply pass and the executor's welcome /
// followup passes never collide on the same row.
const PassSupply = "supply"

// -----------------------------------------------------------------------------
// Measured yield and EO turnaround (§2.6)
// -----------------------------------------------------------------------------

const (
	// YieldWindowDays is §2.6's rolling window for VALIDATION_VALID /
	// VALIDATION_ORDERED.
	YieldWindowDays = 14

	// YieldMinSample is the ordered-record count below which an ISP's measured
	// ratio is not trusted and SeedEOYield is used instead. 1,000 is the
	// operator-facing round number in the WP7 brief; below it a single bad
	// batch moves the ratio by more than the ratio's whole useful range.
	YieldMinSample = 1000

	// SeedEOTurnaroundHours is the seed for eo_turnaround_p90_hours until the
	// ledger carries an ORDERED→VALID pair (§2.6). EO returns most batches in
	// well under two hours; the seed is deliberately SHORT because a long seed
	// inflates safety_stock and buys verdicts nobody needs.
	SeedEOTurnaroundHours = 2.0

	// MaxEOYield caps the measured ratio at 1.0. VALID rows whose ORDERED row
	// fell outside the window can push the raw ratio above 1, and a yield > 1
	// would UNDER-order (order = shortfall / yield).
	MaxEOYield = 1.0
)

// MeasuredYield is §2.6's rolling EO yield, and the planner's YieldSource.
//
// It is a struct rather than a function so the per-day measurement is computed
// once and shared by the planner (00:05 MT) and by every hourly supply pass of
// the same day: both must size against the SAME yield or the planner's
// provisional award and the controller's order disagree by construction.
type MeasuredYield struct {
	// WindowDays defaults to YieldWindowDays; MinSample to YieldMinSample.
	WindowDays int
	MinSample  int

	mu         sync.Mutex
	cachedDay  string
	yields     map[string]float64
	turnaround map[string]float64
}

// NewMeasuredYield returns the production configuration.
func NewMeasuredYield() *MeasuredYield {
	return &MeasuredYield{WindowDays: YieldWindowDays, MinSample: YieldMinSample}
}

func (m *MeasuredYield) windowDays() int {
	if m.WindowDays > 0 {
		return m.WindowDays
	}
	return YieldWindowDays
}

func (m *MeasuredYield) minSample() int {
	if m.MinSample > 0 {
		return m.MinSample
	}
	return YieldMinSample
}

// yieldSQL is the rolling window, per ISP, from the Supply Ledger. Both events
// are summed in one pass: a two-query version could read the ORDERED total from
// before a concurrent insert and the VALID total from after it, and produce a
// yield above 1 out of nothing.
const yieldSQL = `
SELECT lower(COALESCE(NULLIF(isp, ''), 'other'))                              AS isp,
       COALESCE(SUM(quantity) FILTER (WHERE event = 'VALIDATION_ORDERED'), 0) AS ordered,
       COALESCE(SUM(quantity) FILTER (WHERE event = 'VALIDATION_VALID'), 0)   AS valid
  FROM drip_supply_ledger
 WHERE occurred_at >= $1 AND occurred_at < $2
   AND event IN ('VALIDATION_ORDERED', 'VALIDATION_VALID')
 GROUP BY 1`

// turnaroundSQL is the p90 of ORDERED→VALID per ISP. The lateral takes the
// FIRST valid row at or after each order for the same lane × source × ISP,
// which is the only pairing a batch-grained ledger supports. percentile_disc
// (not _cont) so the value is an observed measurement, never an interpolation
// between two batches that never happened.
const turnaroundSQL = `
WITH ord AS (
    SELECT lower(COALESCE(NULLIF(isp, ''), 'other')) AS isp, lane, source_slug, occurred_at
      FROM drip_supply_ledger
     WHERE event = 'VALIDATION_ORDERED'
       AND occurred_at >= $1 AND occurred_at < $2
)
SELECT o.isp,
       percentile_disc(0.9) WITHIN GROUP (ORDER BY nxt.hours)::float8 AS p90_hours
  FROM ord o
  CROSS JOIN LATERAL (
      SELECT EXTRACT(EPOCH FROM (v.occurred_at - o.occurred_at)) / 3600.0 AS hours
        FROM drip_supply_ledger v
       WHERE v.event = 'VALIDATION_VALID'
         AND v.lane = o.lane
         AND v.source_slug = o.source_slug
         AND lower(COALESCE(NULLIF(v.isp, ''), 'other')) = o.isp
         AND v.occurred_at >= o.occurred_at
       ORDER BY v.occurred_at
       LIMIT 1
  ) nxt
 GROUP BY 1`

// load populates both maps for `day` under one lock. Cached per Denver day: the
// window is 14 days, so an hourly recompute would move the number by noise and
// make two passes of the same day size differently.
func (m *MeasuredYield) load(ctx context.Context, db Queryer, day time.Time) error {
	key := dayKey(day)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cachedDay == key && m.yields != nil {
		return nil
	}
	to := dayOf(day).AddDate(0, 0, 1)
	from := to.AddDate(0, 0, -m.windowDays())

	yields := map[string]float64{}
	rows, err := db.QueryContext(ctx, yieldSQL, from, to)
	if err != nil {
		return fmt.Errorf("dripsupply: measured yield: %w", err)
	}
	for rows.Next() {
		var isp string
		var ordered, valid int64
		if err := rows.Scan(&isp, &ordered, &valid); err != nil {
			rows.Close()
			return fmt.Errorf("dripsupply: measured yield scan: %w", err)
		}
		yields[normISP(isp)] = clampYield(ordered, valid, int64(m.minSample()))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("dripsupply: measured yield rows: %w", err)
	}

	turn := map[string]float64{}
	trows, err := db.QueryContext(ctx, turnaroundSQL, from, to)
	if err != nil {
		return fmt.Errorf("dripsupply: eo turnaround: %w", err)
	}
	for trows.Next() {
		var isp string
		var p90 sql.NullFloat64
		if err := trows.Scan(&isp, &p90); err != nil {
			trows.Close()
			return fmt.Errorf("dripsupply: eo turnaround scan: %w", err)
		}
		if p90.Valid && p90.Float64 > 0 {
			turn[normISP(isp)] = p90.Float64
		}
	}
	trows.Close()
	if err := trows.Err(); err != nil {
		return fmt.Errorf("dripsupply: eo turnaround rows: %w", err)
	}

	m.cachedDay, m.yields, m.turnaround = key, yields, turn
	return nil
}

// clampYield applies §2.6's seed, floor and the >1 guard.
func clampYield(ordered, valid, minSample int64) float64 {
	if ordered < minSample || ordered <= 0 {
		return SeedEOYield
	}
	y := float64(valid) / float64(ordered)
	if y < MinEOYield {
		return MinEOYield
	}
	if y > MaxEOYield {
		return MaxEOYield
	}
	return y
}

// Yields implements YieldSource. Every ISP class is present in the returned
// map — an ISP the ledger has never seen takes SeedEOYield explicitly rather
// than being absent, so a caller that indexes the map directly cannot silently
// read 0 and order infinity.
func (m *MeasuredYield) Yields(ctx context.Context, db Queryer, day time.Time) (map[string]float64, error) {
	if err := m.load(ctx, db, day); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]float64, len(ispClasses)+len(m.yields))
	for _, isp := range ispClasses {
		out[isp] = SeedEOYield
	}
	for isp, y := range m.yields {
		out[isp] = y
	}
	return out, nil
}

// TurnaroundP90Hours is §2.6's eo_turnaround_hours_p90 per ISP, seeded at
// SeedEOTurnaroundHours where the ledger has no ORDERED→VALID pair.
func (m *MeasuredYield) TurnaroundP90Hours(ctx context.Context, db Queryer, day time.Time) (map[string]float64, error) {
	if err := m.load(ctx, db, day); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]float64, len(ispClasses)+len(m.turnaround))
	for _, isp := range ispClasses {
		out[isp] = SeedEOTurnaroundHours
	}
	for isp, h := range m.turnaround {
		out[isp] = h
	}
	return out, nil
}

var _ YieldSource = (*MeasuredYield)(nil)

// -----------------------------------------------------------------------------
// Decisions
// -----------------------------------------------------------------------------

// Skip reasons. Free text is allowed in drip_tick_outcomes.reason; these are the
// values the §6 panes and the acceptance tests key on.
const (
	SupplySkipZeroDesired    = "zero_desired"
	SupplySkipISPExcluded    = "isp_excluded"
	SupplySkipLanePaused     = "lane_paused"
	SupplySkipLaneInactive   = "lane_inactive"
	SupplySkipNoLaneState    = "no_lane_state"
	SupplySkipNoSource       = "no_accepted_source"
	SupplySkipNoContract     = "no_contract"
	SupplySkipGovernorZero   = "governor_zero"
	SupplySkipCovered        = "covered"
	SupplySkipMaxCoverage    = "max_coverage"
	SupplySkipEODisabled     = "eo_disabled"
	SupplySkipBelowMinOrder  = "below_min_eo_order"
	SupplySkipEOSpendCap     = "eo_spend_cap"
	SupplySkipNoOrderStock   = "no_orderable_stock"
	SupplySkipNothingToOrder = "no_shortfall"
)

// SupplyDecision is one lane × ISP cell's evaluation. Every field the §2.6
// formula reads is recorded so a decision can be re-derived from the row
// without re-running the queries.
type SupplyDecision struct {
	Lane string `json:"lane"`
	ISP  string `json:"isp"`

	// Skip is non-empty when the cell was not evaluated or produced no action.
	Skip string `json:"skip,omitempty"`
	// Enforced is false in shadow mode and for a non-canary cell under
	// MODE=canary: the numbers are real, the queue was not touched.
	Enforced bool `json:"enforced"`

	FreshMailable    int     `json:"fresh_mailable"`
	PendingEO        int     `json:"pending_eo"`
	RemailEligible   int     `json:"remail_eligible"`
	ExpectedArrivals int     `json:"expected_arrivals"`
	ReservedForIntro int     `json:"reserved_for_intro"`
	HeldValidated    int     `json:"held_validated"`
	HeldOrderable    int     `json:"held_orderable"`
	Yield            float64 `json:"yield"`
	TurnaroundHours  float64 `json:"turnaround_hours"`

	AwardFirm                 int     `json:"award_firm"`
	AwardProvisional          int     `json:"award_provisional"`
	ProvisionalThroughHorizon int     `json:"provisional_through_horizon"`
	SendRatePerHour           float64 `json:"send_rate_per_hour"`
	CoverageHours             float64 `json:"coverage_hours"`
	SafetyStock               int     `json:"safety_stock"`
	Projected                 int     `json:"projected"`
	Shortfall                 int     `json:"shortfall"`
	Need                      int     `json:"need"`

	Promoted     int     `json:"promoted"` // MAILABLE  (held, verdict live → ready)
	Remailed     int     `json:"remailed"` // REMAIL_ELIGIBLE (mailed non-engager → ready)
	Ordered      int     `json:"ordered"`  // VALIDATION_ORDERED (held → pending_eo)
	OrderCostUSD float64 `json:"order_cost_usd"`

	Reason string `json:"reason"`
}

// Acted reports whether the decision moved anything.
func (d SupplyDecision) Acted() bool { return d.Promoted > 0 || d.Remailed > 0 || d.Ordered > 0 }

// SupplyRun is one RunOnce pass.
type SupplyRun struct {
	Day       time.Time        `json:"day"`
	At        time.Time        `json:"at"`
	Mode      Mode             `json:"mode"`
	Decisions []SupplyDecision `json:"decisions"`

	Promoted     int     `json:"promoted"`
	Remailed     int     `json:"remailed"`
	Ordered      int     `json:"ordered"`
	OrderCostUSD float64 `json:"order_cost_usd"`
	Evaluated    int     `json:"evaluated"`
	Skipped      int     `json:"skipped"`
}

// -----------------------------------------------------------------------------
// Controller
// -----------------------------------------------------------------------------

const (
	// DefaultSupplyChunk is §2.6's chunk ceiling for a queue transition. 20k
	// rows is roughly a two-second UPDATE on the 13.5M-row queue; a single
	// unbounded UPDATE would hold row locks across a lane's whole held pool
	// while the drip executor is trying to claim from it.
	DefaultSupplyChunk = 20000

	// DefaultSupplyTimeout is the statement_timeout for the controller's reads.
	// The heaviest measured shape (the held aggregate on the largest lane) ran
	// 2.6 s on prod on 2026-09-03; 120 s leaves room for a loaded database
	// without letting a pathological plan run into the next hourly pass.
	DefaultSupplyTimeout = 120 * time.Second

	// DefaultArrivalWindowDays is the trailing window `expected_arrivals` is
	// measured over. Seven days spans a full weekly supplier cycle.
	DefaultArrivalWindowDays = 7
)

// SupplyControllerConfig is everything the controller needs that is not the
// database.
type SupplyControllerConfig struct {
	Mode   Mode
	Canary []CanaryCell

	// Yield is the measured EO yield / turnaround source. Nil = a fresh
	// MeasuredYield. Share ONE instance with the planner so both size against
	// the same numbers.
	Yield *MeasuredYield

	// Governors is the §5.5 input: a lane × ISP whose every allowed domain is
	// governed to 0 gets no order. Nil = the check is INERT and the controller
	// says so once, loudly — this is the one term that can buy verdicts for
	// mail that cannot ship.
	Governors GovernorReader

	// ContractKey is the §1.5 HMAC key. Nil = read CONTRACT_TOKEN_KEY once.
	ContractKey []byte
	// ContractSource overrides how contracts are loaded (tests, and a future
	// per-tick cache shared with the planner).
	ContractSource func(ctx context.Context, day time.Time) (*ActiveSet, error)
	// PlanSource overrides how the frozen plan is read (tests).
	PlanSource func(ctx context.Context, day time.Time) (*Plan, bool, error)

	// ShadowLedgerTable overrides ShadowSupplyLedgerTable (tests).
	ShadowLedgerTable string
	// LedgerTable overrides SupplyLedgerTable (tests).
	LedgerTable string

	ChunkSize        int
	Timeout          time.Duration
	ArrivalWindow    int
	OutcomesDisabled bool
	AlertsDisabled   bool
	Notifier         notify.Notifier
	Clock            func() time.Time
}

// SupplyController implements §2.6. One value per process; RunOnce is safe for
// concurrent use but is expected to be serialised by the worker's lock.
type SupplyController struct {
	db  *sql.DB
	cfg SupplyControllerConfig

	contractKey    []byte
	contractKeyErr error

	govWarnOnce sync.Once

	// alerted rate-limits the §6 alerts to one per key per Denver day. The
	// spend cap trips every hour once it trips at all; an unthrottled alert
	// would post 24 identical messages and train the operator to ignore it.
	alertMu sync.Mutex
	alerted map[string]string
}

// NewSupplyController builds the controller. The contract key is resolved ONCE:
// a key that vanishes mid-life must fail every pass loudly from one log line,
// not intermittently depending on which goroutine re-read the environment.
func NewSupplyController(db *sql.DB, cfg SupplyControllerConfig) *SupplyController {
	if cfg.Yield == nil {
		cfg.Yield = NewMeasuredYield()
	}
	if cfg.ChunkSize <= 0 || cfg.ChunkSize > DefaultSupplyChunk {
		cfg.ChunkSize = DefaultSupplyChunk
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultSupplyTimeout
	}
	if cfg.ArrivalWindow <= 0 {
		cfg.ArrivalWindow = DefaultArrivalWindowDays
	}
	if strings.TrimSpace(cfg.ShadowLedgerTable) == "" {
		cfg.ShadowLedgerTable = ShadowSupplyLedgerTable
	}
	if strings.TrimSpace(cfg.LedgerTable) == "" {
		cfg.LedgerTable = SupplyLedgerTable
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	c := &SupplyController{db: db, cfg: cfg, alerted: map[string]string{}}
	if len(cfg.ContractKey) > 0 {
		c.contractKey = cfg.ContractKey
	} else if cfg.ContractSource == nil {
		key, err := contractmeta.KeyFromEnv()
		if err != nil {
			c.contractKeyErr = fmt.Errorf("dripsupply: supply controller cannot verify contract tokens: %w — set %s (>= %d bytes)",
				err, contractmeta.KeyEnvVar, contractmeta.MinKeyLen)
		}
		c.contractKey = key
	}
	return c
}

// ledgerTable picks the live or shadow table for a cell.
func (c *SupplyController) ledgerTable(enforced bool) string {
	if enforced {
		return c.cfg.LedgerTable
	}
	return c.cfg.ShadowLedgerTable
}

// enforces reports whether this lane × ISP acts on partner_clean_queue.
// MODE=canary matches a cell when ANY of the lane's allowed sending domains is
// named for that ISP × lane, because supply is lane-grained and the canary
// vocabulary is domain-grained.
func (c *SupplyController) enforces(lane, isp string, domains []string) bool {
	switch c.cfg.Mode {
	case ModeOn:
		return true
	case ModeCanary:
		for _, d := range domains {
			if canaryMatch(c.cfg.Canary, strings.ToLower(d), isp, strings.ToLower(lane)) {
				return true
			}
		}
		return false
	default: // off, shadow
		return false
	}
}

// RunOnce is §2.6, for every lane × ISP with a provisional award or coverage
// below min_coverage_hours. `db` may be nil, in which case the controller's own
// pool is used (the parameter exists because the WP7 brief names it and because
// a caller holding a scoped pool should be able to pass it).
func (c *SupplyController) RunOnce(ctx context.Context, db *sql.DB, now time.Time) (SupplyRun, error) {
	if db == nil {
		db = c.db
	}
	run := SupplyRun{At: now, Mode: c.cfg.Mode}
	if c.cfg.Mode == ModeOff {
		return run, nil
	}
	if db == nil {
		return run, fmt.Errorf("dripsupply: supply controller has no database")
	}
	if c.contractKeyErr != nil {
		return run, c.contractKeyErr
	}

	day := DenverDay(now)
	run.Day = day

	// A dedicated connection so the elevated statement_timeout applies to this
	// pass and to nothing else: database/sql hands a pooled connection to an
	// arbitrary goroutine, so a bare SET would land on one connection and the
	// next read would run under prod's default.
	conn, err := db.Conn(ctx)
	if err != nil {
		return run, fmt.Errorf("dripsupply: supply connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("SET statement_timeout = %d", c.cfg.Timeout.Milliseconds())); err != nil {
		return run, fmt.Errorf("dripsupply: supply statement_timeout: %w", err)
	}

	contracts, err := c.loadContracts(ctx, conn, day)
	if err != nil {
		return run, err
	}
	lanes := sortedMapKeys(contracts.Dispatches)
	if len(lanes) == 0 {
		log.Printf("[DripSupply] %s: no active dispatch contracts — nothing to supply", dayKey(day))
		return run, nil
	}

	st, err := c.readState(ctx, conn, db, day, contracts, lanes)
	if err != nil {
		return run, err
	}

	byBrand := map[string][]string{}
	for _, name := range sortedMapKeys(contracts.Domains) {
		if d := contracts.Domains[name]; d != nil {
			byBrand[d.BrandCode] = append(byBrand[d.BrandCode], d.SendingDomain)
		}
	}

	for _, lane := range lanes {
		decisions := c.evaluateLane(ctx, conn, day, now, contracts, byBrand, st, lane)
		for _, d := range decisions {
			run.Decisions = append(run.Decisions, d)
			if d.Skip != "" && !d.Acted() {
				run.Skipped++
				continue
			}
			run.Evaluated++
			run.Promoted += d.Promoted
			run.Remailed += d.Remailed
			run.Ordered += d.Ordered
			run.OrderCostUSD += d.OrderCostUSD
		}
		c.writeOutcome(ctx, db, now, lane, decisions)
	}

	log.Printf("[DripSupply] %s mode=%s lanes=%d cells=%d evaluated=%d promoted=%d remailed=%d ordered=%d spend=$%.2f",
		dayKey(day), c.cfg.Mode, len(lanes), len(run.Decisions), run.Evaluated,
		run.Promoted, run.Remailed, run.Ordered, run.OrderCostUSD)
	return run, nil
}

func (c *SupplyController) loadContracts(ctx context.Context, conn *sql.Conn, day time.Time) (*ActiveSet, error) {
	if c.cfg.ContractSource != nil {
		set, err := c.cfg.ContractSource(ctx, day)
		if err != nil {
			return nil, fmt.Errorf("dripsupply: supply contracts: %w", err)
		}
		if set == nil {
			set = &ActiveSet{Day: day}
		}
		return set, nil
	}
	set, err := LoadActiveWithKey(ctx, conn, day, c.contractKey)
	if err != nil {
		return nil, fmt.Errorf("dripsupply: supply load contracts: %w", err)
	}
	return set, nil
}

// -----------------------------------------------------------------------------
// State reads
// -----------------------------------------------------------------------------

type supplyDataset struct {
	ID       string
	Slug     string
	Vertical string
	Active   bool
	Paused   bool
}

// supplyState is every number the evaluation reads, gathered up front. The
// evaluation itself performs no aggregate reads: a controller that mixes reads
// into its decision loop cannot be replayed after an incident, and its cost
// scales with the number of cells rather than the number of lanes.
type supplyState struct {
	// datasets accepted by each lane's inventory contract, active and unpaused.
	laneDatasets map[string][]supplyDataset
	// every dataset named by a lane's accepted_sources, whatever its status.
	laneAllSources map[string][]supplyDataset
	laneHasState   map[string]bool

	fresh          map[LaneISP]int
	pendingEO      map[LaneISP]int
	remailEligible map[LaneISP]int
	heldValidated  map[LaneISP]int
	heldOrderable  map[LaneISP]int
	arrivalsPerDay map[LaneISP]float64
	reservedIntro  map[LaneISP]int
	remailedToday  map[LaneISP]int
	eoSpentToday   map[string]float64

	yields     map[string]float64
	turnaround map[string]float64
	planRows   map[LaneISP]PlanRow
	havePlan   bool
	eoUnitCost float64
}

func (c *SupplyController) readState(ctx context.Context, conn *sql.Conn, db *sql.DB, day time.Time, contracts *ActiveSet, lanes []string) (*supplyState, error) {
	st := &supplyState{
		laneDatasets:   map[string][]supplyDataset{},
		laneAllSources: map[string][]supplyDataset{},
		laneHasState:   map[string]bool{},
		fresh:          map[LaneISP]int{},
		pendingEO:      map[LaneISP]int{},
		remailEligible: map[LaneISP]int{},
		heldValidated:  map[LaneISP]int{},
		heldOrderable:  map[LaneISP]int{},
		arrivalsPerDay: map[LaneISP]float64{},
		reservedIntro:  map[LaneISP]int{},
		remailedToday:  map[LaneISP]int{},
		eoSpentToday:   map[string]float64{},
		planRows:       map[LaneISP]PlanRow{},
	}

	if err := c.readDatasets(ctx, conn, contracts, lanes, st); err != nil {
		return nil, err
	}
	if err := c.readLaneState(ctx, conn, lanes, st); err != nil {
		return nil, err
	}

	// dsToLane maps a dataset id to the lane that accepts it. The inventory
	// contract, not partner_datasets.vertical, is the authority: a lane mails
	// exactly the sources its contract names.
	dsToLane := map[string]string{}
	byWindow := map[int][]string{} // verdict_valid_days -> dataset ids
	byRemail := map[int][]string{} // remail_after_days  -> dataset ids
	var allIDs []string
	for _, lane := range lanes {
		inv := contracts.Inventories[lane]
		days := 60
		if inv != nil && inv.VerdictValidDays > 0 {
			days = inv.VerdictValidDays
		}
		for _, d := range st.laneDatasets[lane] {
			dsToLane[d.ID] = lane
			allIDs = append(allIDs, d.ID)
			byWindow[days] = append(byWindow[days], d.ID)
			if inv != nil && inv.RemailEnabled {
				ra := inv.RemailAfterDays
				if ra < 0 {
					ra = 0
				}
				byRemail[ra] = append(byRemail[ra], d.ID)
			}
		}
	}
	if len(allIDs) == 0 {
		return st, nil
	}
	sort.Strings(allIDs)

	if err := c.readFreshAndHeld(ctx, conn, day, byWindow, dsToLane, st); err != nil {
		return nil, err
	}
	if err := c.readPendingEO(ctx, conn, allIDs, dsToLane, st); err != nil {
		return nil, err
	}
	if err := c.readRemailEligible(ctx, conn, day, byRemail, dsToLane, st); err != nil {
		return nil, err
	}
	if err := c.readArrivals(ctx, conn, day, allIDs, dsToLane, st); err != nil {
		return nil, err
	}
	if err := c.readReservedIntro(ctx, conn, day, lanes, st); err != nil {
		return nil, err
	}
	if err := c.readLedgerToday(ctx, conn, day, lanes, st); err != nil {
		return nil, err
	}

	y, err := c.cfg.Yield.Yields(ctx, conn, day)
	if err != nil {
		return nil, err
	}
	st.yields = y
	t, err := c.cfg.Yield.TurnaroundP90Hours(ctx, conn, day)
	if err != nil {
		return nil, err
	}
	st.turnaround = t

	plan, ok, err := c.loadPlan(ctx, conn, day)
	if err != nil {
		return nil, err
	}
	st.havePlan = ok
	if ok && plan != nil {
		for _, r := range plan.Rows {
			k := LaneISP{Lane: r.Lane, ISP: normISP(r.ISP)}
			agg := st.planRows[k]
			agg.Lane, agg.ISP = r.Lane, k.ISP
			agg.AwardFirm += r.AwardFirm
			agg.AwardProvisional += r.AwardProvisional
			st.planRows[k] = agg
		}
	}

	rates, err := loadCostRates(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("dripsupply: supply cost rates: %w", err)
	}
	st.eoUnitCost = rateOrZero(rates, RateEOPerVerdict)
	return st, nil
}

func (c *SupplyController) loadPlan(ctx context.Context, q Queryer, day time.Time) (*Plan, bool, error) {
	if c.cfg.PlanSource != nil {
		return c.cfg.PlanSource(ctx, day)
	}
	return LoadStoredPlan(ctx, q, day)
}

// readDatasets resolves each lane's accepted_sources to partner_datasets rows.
// A lane whose contract names a slug that does not exist gets an empty list and
// is skipped with `no_accepted_source` — never a silent fall-through to "every
// dataset of that vertical".
func (c *SupplyController) readDatasets(ctx context.Context, conn *sql.Conn, contracts *ActiveSet, lanes []string, st *supplyState) error {
	slugSet := map[string]struct{}{}
	for _, lane := range lanes {
		if inv := contracts.Inventories[lane]; inv != nil {
			for _, s := range inv.AcceptedSources {
				if s = strings.TrimSpace(s); s != "" {
					slugSet[s] = struct{}{}
				}
			}
		}
	}
	if len(slugSet) == 0 {
		return nil
	}
	slugs := make([]string, 0, len(slugSet))
	for s := range slugSet {
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)

	rows, err := conn.QueryContext(ctx, `
		SELECT id::text, slug, vertical, COALESCE(status, ''), COALESCE(paused_emergency, false)
		FROM partner_datasets
		WHERE slug = ANY($1)
	`, pq.Array(slugs))
	if err != nil {
		return fmt.Errorf("dripsupply: supply datasets: %w", err)
	}
	defer rows.Close()
	bySlug := map[string]supplyDataset{}
	for rows.Next() {
		var d supplyDataset
		var status string
		if err := rows.Scan(&d.ID, &d.Slug, &d.Vertical, &status, &d.Paused); err != nil {
			return fmt.Errorf("dripsupply: supply datasets scan: %w", err)
		}
		d.Active = status == "active"
		bySlug[d.Slug] = d
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("dripsupply: supply datasets rows: %w", err)
	}
	for _, lane := range lanes {
		inv := contracts.Inventories[lane]
		if inv == nil {
			continue
		}
		for _, s := range inv.AcceptedSources {
			d, ok := bySlug[strings.TrimSpace(s)]
			if !ok {
				continue
			}
			if d.Vertical != lane {
				// The inventory contract is the authority on which sources a
				// lane mails, but a source whose partner_datasets.vertical is
				// a DIFFERENT lane is almost always a contract typo: its
				// records are claimed by that other lane's executor, so
				// cleaning them here buys verdicts this lane never mails.
				log.Printf("[DripSupply] lane %s accepts source %s whose dataset vertical is %q — cleaning records another lane claims", lane, d.Slug, d.Vertical)
			}
			st.laneAllSources[lane] = append(st.laneAllSources[lane], d)
			if d.Active && !d.Paused {
				st.laneDatasets[lane] = append(st.laneDatasets[lane], d)
			}
		}
	}
	return nil
}

// readLaneState records which lanes the orchestrator can actually claim for.
// A lane with no partner_drip_state row is INVISIBLE to the tick (the executor
// returns before it plans anything), so cleaning for it buys verdicts nothing
// will ever mail.
func (c *SupplyController) readLaneState(ctx context.Context, conn *sql.Conn, lanes []string, st *supplyState) error {
	rows, err := conn.QueryContext(ctx, `SELECT vertical FROM partner_drip_state WHERE vertical = ANY($1)`, pq.Array(lanes))
	if err != nil {
		return fmt.Errorf("dripsupply: supply lane state: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return fmt.Errorf("dripsupply: supply lane state scan: %w", err)
		}
		st.laneHasState[v] = true
	}
	return rows.Err()
}

// freshHeldSQL reads the three inventory pools that share one verdict window in
// ONE pass. `status` is the leading predicate on idx_pcq_dataset_status_mailed /
// idx_pcq_dataset_status_ingested, so each FILTER arm is an index scan of the
// lane's own rows rather than a scan of the 13.5M-row queue.
//
//	fresh_mailable  ready, verdict live, never mailed, touch 0 — mailable NOW
//	held_validated  held, verdict live, never mailed — the free top-up (§2.6 leg 1)
//	held_orderable  held, verdict absent or expired, EO retries left — the EO stock
//
// warm_touches excludes Kumo pad rows (they are owned by the warm pad, not the
// drip) and globusa_flag excludes the GlobUSA own-data carve-out from paid
// validation; both are VERBATIM from agents/jobs/drip_supply.py:46-49, the job
// this controller supersedes.
const freshHeldSQL = `
SELECT dataset_id::text,
       lower(COALESCE(NULLIF(isp_family, ''), 'other')) AS isp,
       COUNT(*) FILTER (WHERE status = 'ready'
                          AND mailed_at IS NULL
                          AND COALESCE(touch_count, 0) = 0
                          AND validated_at >= $2)                                AS fresh_mailable,
       COUNT(*) FILTER (WHERE status = 'held'
                          AND mailed_at IS NULL
                          AND COALESCE(touch_count, 0) = 0
                          AND terminal_reason IS NULL
                          AND (extra_metadata->>'warm_touches') IS NULL
                          AND validated_at >= $2)                                AS held_validated,
       COUNT(*) FILTER (WHERE status = 'held'
                          AND mailed_at IS NULL
                          AND COALESCE(touch_count, 0) = 0
                          AND terminal_reason IS NULL
                          AND (extra_metadata->>'warm_touches') IS NULL
                          AND (extra_metadata->>'globusa_flag') IS NULL
                          AND COALESCE(eo_attempts, 0) < 3
                          AND (validated_at IS NULL OR validated_at < $2))       AS held_orderable
  FROM partner_clean_queue
 WHERE dataset_id = ANY($1)
   AND status IN ('ready', 'held')
 GROUP BY 1, 2`

func (c *SupplyController) readFreshAndHeld(ctx context.Context, conn *sql.Conn, day time.Time, byWindow map[int][]string, dsToLane map[string]string, st *supplyState) error {
	for _, days := range sortedIntKeys(byWindow) {
		ids := byWindow[days]
		sort.Strings(ids)
		cutoff := dayOf(day).AddDate(0, 0, -days)
		rows, err := conn.QueryContext(ctx, freshHeldSQL, pq.Array(ids), cutoff)
		if err != nil {
			return fmt.Errorf("dripsupply: supply inventory (%d day verdict window): %w", days, err)
		}
		for rows.Next() {
			var dsID, isp string
			var fresh, heldValid, heldOrder int
			if err := rows.Scan(&dsID, &isp, &fresh, &heldValid, &heldOrder); err != nil {
				rows.Close()
				return fmt.Errorf("dripsupply: supply inventory scan: %w", err)
			}
			lane, ok := dsToLane[dsID]
			if !ok {
				continue
			}
			k := LaneISP{Lane: lane, ISP: normISP(isp)}
			st.fresh[k] += fresh
			st.heldValidated[k] += heldValid
			st.heldOrderable[k] += heldOrder
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("dripsupply: supply inventory rows: %w", err)
		}
	}
	return nil
}

func (c *SupplyController) readPendingEO(ctx context.Context, conn *sql.Conn, ids []string, dsToLane map[string]string, st *supplyState) error {
	rows, err := conn.QueryContext(ctx, `
		SELECT dataset_id::text, lower(COALESCE(NULLIF(isp_family, ''), 'other')) AS isp, COUNT(*)
		FROM partner_clean_queue
		WHERE dataset_id = ANY($1)
		  AND status IN ('pending_eo', 'eo_in_flight')
		GROUP BY 1, 2
	`, pq.Array(ids))
	if err != nil {
		return fmt.Errorf("dripsupply: supply pending EO: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var dsID, isp string
		var n int
		if err := rows.Scan(&dsID, &isp, &n); err != nil {
			return fmt.Errorf("dripsupply: supply pending EO scan: %w", err)
		}
		if lane, ok := dsToLane[dsID]; ok {
			st.pendingEO[LaneISP{Lane: lane, ISP: normISP(isp)}] += n
		}
	}
	return rows.Err()
}

// remailEligibleSQL is the WCL "resurrection" population: a record this estate
// mailed, that never engaged, whose ladder is finished or expired, older than
// the contract's remail_after_days. `last_click_at IS NULL` and the
// clicked_exit guard are deliberate — a clicker EXITS the ladder by standing
// ruling and must never be resurrected into a cold intro.
const remailEligibleSQL = `
SELECT dataset_id::text,
       lower(COALESCE(NULLIF(isp_family, ''), 'other')) AS isp,
       COUNT(*)
  FROM partner_clean_queue
 WHERE dataset_id = ANY($1)
   AND status = 'mailed'
   AND mailed_at < $2
   AND engaged_at IS NULL
   AND last_click_at IS NULL
   AND (next_touch_at IS NULL OR terminal_reason IN ('completed', 'ladder_complete'))
   AND terminal_reason IS DISTINCT FROM 'clicked_exit'
   AND (extra_metadata->>'warm_touches') IS NULL
 GROUP BY 1, 2`

func (c *SupplyController) readRemailEligible(ctx context.Context, conn *sql.Conn, day time.Time, byRemail map[int][]string, dsToLane map[string]string, st *supplyState) error {
	for _, days := range sortedIntKeys(byRemail) {
		ids := byRemail[days]
		sort.Strings(ids)
		cutoff := dayOf(day).AddDate(0, 0, -days)
		rows, err := conn.QueryContext(ctx, remailEligibleSQL, pq.Array(ids), cutoff)
		if err != nil {
			return fmt.Errorf("dripsupply: supply remail eligible (%d days): %w", days, err)
		}
		for rows.Next() {
			var dsID, isp string
			var n int
			if err := rows.Scan(&dsID, &isp, &n); err != nil {
				rows.Close()
				return fmt.Errorf("dripsupply: supply remail eligible scan: %w", err)
			}
			if lane, ok := dsToLane[dsID]; ok {
				st.remailEligible[LaneISP{Lane: lane, ISP: normISP(isp)}] += n
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("dripsupply: supply remail eligible rows: %w", err)
		}
	}
	return nil
}

// readArrivals measures the trailing daily intake per lane × ISP. This is the
// only honest basis for `expected_arrivals_before_need`: the source contract's
// arrival_cadence says "continuous", not how many.
func (c *SupplyController) readArrivals(ctx context.Context, conn *sql.Conn, day time.Time, ids []string, dsToLane map[string]string, st *supplyState) error {
	from := dayOf(day).AddDate(0, 0, -c.cfg.ArrivalWindow)
	rows, err := conn.QueryContext(ctx, `
		SELECT dataset_id::text, lower(COALESCE(NULLIF(isp_family, ''), 'other')) AS isp, COUNT(*)
		FROM partner_clean_queue
		WHERE dataset_id = ANY($1)
		  AND ingested_at >= $2
		  AND ingested_at < $3
		GROUP BY 1, 2
	`, pq.Array(ids), from, dayOf(day).AddDate(0, 0, 1))
	if err != nil {
		return fmt.Errorf("dripsupply: supply arrivals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var dsID, isp string
		var n int
		if err := rows.Scan(&dsID, &isp, &n); err != nil {
			return fmt.Errorf("dripsupply: supply arrivals scan: %w", err)
		}
		if lane, ok := dsToLane[dsID]; ok {
			st.arrivalsPerDay[LaneISP{Lane: lane, ISP: normISP(isp)}] += float64(n) / float64(c.cfg.ArrivalWindow)
		}
	}
	return rows.Err()
}

// readReservedIntro reads today's live intro reservations from the CAPACITY
// ledger. Units reconcile: an intro is exactly one message against exactly one
// record, so a reserved intro is a record already spoken for. `reserved` is the
// original grant and is never decremented, so the live figure is
// SUM(reserved) − SUM(released) — the same arithmetic PlanRemaining uses.
func (c *SupplyController) readReservedIntro(ctx context.Context, conn *sql.Conn, day time.Time, lanes []string, st *supplyState) error {
	rows, err := conn.QueryContext(ctx, `
		SELECT lane, lower(COALESCE(NULLIF(isp, ''), 'other')) AS isp,
		       GREATEST(0, COALESCE(SUM(reserved), 0) - COALESCE(SUM(released), 0))
		FROM drip_capacity_ledger
		WHERE day = $1::date
		  AND lane = ANY($2)
		  AND touch_class = 'intro'
		GROUP BY 1, 2
	`, dayKey(day), pq.Array(lanes))
	if err != nil {
		return fmt.Errorf("dripsupply: supply reserved intros: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var lane, isp string
		var n int
		if err := rows.Scan(&lane, &isp, &n); err != nil {
			return fmt.Errorf("dripsupply: supply reserved intros scan: %w", err)
		}
		st.reservedIntro[LaneISP{Lane: lane, ISP: normISP(isp)}] += n
	}
	return rows.Err()
}

// readLedgerToday reads what THIS controller already did today: the EO spend
// per lane (the max_daily_eo_spend_usd budget) and the remails per lane × ISP
// (the max_remail_share budget). Both are read from the ledger rather than kept
// in memory so two instances, or a restart, cannot double-spend.
func (c *SupplyController) readLedgerToday(ctx context.Context, conn *sql.Conn, day time.Time, lanes []string, st *supplyState) error {
	start := dayOf(day)
	end := start.AddDate(0, 0, 1)
	rows, err := conn.QueryContext(ctx, `
		SELECT lane, lower(COALESCE(NULLIF(isp, ''), 'other')) AS isp, event,
		       COALESCE(SUM(quantity), 0), COALESCE(SUM(total_cost), 0)::float8
		FROM `+c.cfg.LedgerTable+`
		WHERE occurred_at >= $1 AND occurred_at < $2
		  AND lane = ANY($3)
		  AND event IN ('VALIDATION_ORDERED', 'REMAIL_ELIGIBLE')
		GROUP BY 1, 2, 3
	`, start, end, pq.Array(lanes))
	if err != nil {
		return fmt.Errorf("dripsupply: supply ledger today: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var lane, isp, event string
		var qty int
		var cost float64
		if err := rows.Scan(&lane, &isp, &event, &qty, &cost); err != nil {
			return fmt.Errorf("dripsupply: supply ledger today scan: %w", err)
		}
		switch event {
		case SupplyEventValidationOrdered:
			st.eoSpentToday[lane] += cost
		case SupplyEventRemailEligible:
			st.remailedToday[LaneISP{Lane: lane, ISP: normISP(isp)}] += qty
		}
	}
	return rows.Err()
}

// -----------------------------------------------------------------------------
// Evaluation (§2.6)
// -----------------------------------------------------------------------------

func (c *SupplyController) evaluateLane(
	ctx context.Context, conn *sql.Conn, day, now time.Time,
	contracts *ActiveSet, byBrand map[string][]string, st *supplyState, lane string,
) []SupplyDecision {
	disp := contracts.Dispatches[lane]
	inv := contracts.Inventories[lane]

	// One skip decision for the whole lane when the lane itself is ineligible.
	laneSkip := ""
	switch {
	case disp == nil || inv == nil:
		laneSkip = SupplySkipNoContract
	case !st.laneHasState[lane]:
		laneSkip = SupplySkipNoLaneState
	case len(st.laneDatasets[lane]) == 0:
		laneSkip = laneSourceSkip(st.laneAllSources[lane])
	}
	if laneSkip != "" {
		return []SupplyDecision{{Lane: lane, Skip: laneSkip, Reason: laneSkip}}
	}

	domains := resolveDomains(disp.AllowedDomains, byBrand, contracts.Domains)
	windowHours := laneWindowHours(domains, contracts.Domains)

	excluded := map[string]struct{}{}
	for _, e := range disp.ISPExclusions {
		excluded[normISP(e)] = struct{}{}
	}

	// The ISPs to consider: everything the dispatch contract names, plus every
	// ISP that already carries a plan award (a lane whose desired was edited to
	// 0 after the plan froze still has mail to serve today).
	ispSet := map[string]struct{}{}
	for isp := range disp.DesiredDailyIntros {
		ispSet[normISP(isp)] = struct{}{}
	}
	for k := range st.planRows {
		if k.Lane == lane {
			ispSet[k.ISP] = struct{}{}
		}
	}
	isps := make([]string, 0, len(ispSet))
	for isp := range ispSet {
		isps = append(isps, isp)
	}
	sort.Strings(isps)

	out := make([]SupplyDecision, 0, len(isps))
	for _, isp := range isps {
		out = append(out, c.evaluateCell(ctx, conn, day, now, contracts, st, lane, isp, disp, inv, domains, windowHours, excluded))
	}
	return out
}

// laneSourceSkip distinguishes "every accepted source is paused" from "every
// accepted source is inactive" from "the contract names no real source". The
// three have different remedies and must not collapse into one string.
func laneSourceSkip(all []supplyDataset) string {
	if len(all) == 0 {
		return SupplySkipNoSource
	}
	for _, d := range all {
		if d.Paused {
			return SupplySkipLanePaused
		}
	}
	return SupplySkipLaneInactive
}

// laneWindowHours is the widest active window among the lane's allowed sending
// domains — the hours the lane can actually mail in. WIDEST, not narrowest: the
// lane's day ends when its last domain closes, and a narrower figure would
// overstate the send rate and over-order.
func laneWindowHours(domains []string, byName map[string]*DomainContract) float64 {
	best := 0.0
	for _, name := range domains {
		d := byName[name]
		if d == nil {
			continue
		}
		w, err := WindowOf(d)
		if err != nil {
			continue
		}
		if h := w.Hours(); h > best {
			best = h
		}
	}
	if best <= 0 {
		best = DefaultWindow().Hours()
	}
	return best
}

func (c *SupplyController) evaluateCell(
	ctx context.Context, conn *sql.Conn, day, now time.Time,
	contracts *ActiveSet, st *supplyState,
	lane, isp string, disp *DispatchContract, inv *InventoryContract,
	domains []string, windowHours float64, excluded map[string]struct{},
) SupplyDecision {
	k := LaneISP{Lane: lane, ISP: isp}
	d := SupplyDecision{
		Lane: lane, ISP: isp,
		FreshMailable:  st.fresh[k],
		PendingEO:      st.pendingEO[k],
		RemailEligible: st.remailEligible[k],
		HeldValidated:  st.heldValidated[k],
		HeldOrderable:  st.heldOrderable[k],
		Yield:          yieldFor(st.yields, isp),
	}
	d.TurnaroundHours = turnaroundFor(st.turnaround, isp)

	// --- hard gates (§2.6, §5.1, §5.5) ---------------------------------------
	if _, bad := excluded[isp]; bad {
		return d.skip(SupplySkipISPExcluded)
	}
	plan := st.planRows[k]
	d.AwardFirm, d.AwardProvisional = plan.AwardFirm, plan.AwardProvisional
	// §2.6: never for an ISP with desired = 0. UNCONDITIONAL — a plan award on
	// a cap-0 ISP is a planner defect, and honouring it here would buy verdicts
	// for a lane the domain contract cannot mail (the gmail-ban shape, where
	// daily_max_by_isp.gmail = 0 on eight brands). Fail closed, and let the
	// decision row say why.
	desired := disp.DesiredDailyIntros[isp]
	if desired <= 0 {
		return d.skip(SupplySkipZeroDesired)
	}
	if c.governedToZero(ctx, day, domains, isp, contracts) {
		return d.skip(SupplySkipGovernorZero)
	}

	// --- coverage (§2.6 entry condition) -------------------------------------
	demand := d.AwardFirm + d.AwardProvisional
	if !st.havePlan {
		demand = desired
	}
	if demand > 0 && windowHours > 0 {
		d.SendRatePerHour = float64(demand) / windowHours
	}
	d.CoverageHours = math.Inf(1)
	if d.SendRatePerHour > 0 {
		d.CoverageHours = float64(d.FreshMailable) / d.SendRatePerHour
	}
	minCov := float64(inv.MinCoverageHours)
	if d.AwardProvisional <= 0 && d.CoverageHours >= minCov {
		return d.skip(SupplySkipCovered)
	}
	maxCov := float64(inv.MaxCoverageHours)
	if maxCov > 0 && d.CoverageHours >= maxCov {
		return d.skip(SupplySkipMaxCoverage)
	}

	// --- §2.6's arithmetic ---------------------------------------------------
	//
	//   projected = fresh + pending_eo x yield + arrivals + remail_credit - reserved
	//   shortfall = provisional_through_eo_horizon + safety_stock - projected
	//   order     = ceil(max(0, shortfall) / yield)
	horizon := d.TurnaroundHours
	d.ExpectedArrivals = int(math.Round(st.arrivalsPerDay[k] * horizon / 24.0))
	d.ReservedForIntro = st.reservedIntro[k]

	remailCredit := 0
	if inv.RemailEnabled {
		remailCredit = c.remailAllowance(inv, st, k, demand)
		if remailCredit > d.RemailEligible {
			remailCredit = d.RemailEligible
		}
	}

	d.Projected = d.FreshMailable +
		int(math.Floor(float64(d.PendingEO)*d.Yield)) +
		d.ExpectedArrivals + remailCredit - d.ReservedForIntro

	d.ProvisionalThroughHorizon = throughHorizon(d.AwardProvisional, horizon, windowHours, now, domains, contracts)
	d.SafetyStock = int(math.Ceil(d.SendRatePerHour * horizon))
	d.Shortfall = d.ProvisionalThroughHorizon + d.SafetyStock - d.Projected

	need := d.Shortfall
	if need <= 0 {
		return d.skip(SupplySkipNothingToOrder)
	}
	// "stop at max_coverage_hours" is a CAP on the fill, not only a gate on
	// entry: without it a lane whose provisional award is large would be filled
	// to a coverage the day cannot consume, and the surplus ages out.
	if maxCov > 0 && d.SendRatePerHour > 0 {
		room := int(math.Floor(maxCov*d.SendRatePerHour)) - d.Projected
		if room < need {
			need = room
		}
	}
	if need <= 0 {
		return d.skip(SupplySkipMaxCoverage)
	}
	d.Need = need

	d.Enforced = c.enforces(lane, isp, domains)
	sources := st.laneDatasets[lane]

	// --- leg 1: FRESH (held rows whose verdict is still live) ----------------
	if n := minInt(need, d.HeldValidated); n > 0 {
		moved, err := c.promoteFresh(ctx, conn, day, inv, sources, isp, n, d.Enforced)
		if err != nil {
			log.Printf("[DripSupply] %s/%s promote failed: %v", lane, isp, err)
		}
		if moved > 0 {
			c.writeLedgerGrouped(ctx, conn, now, SupplyEventMailable, lane, isp, moved, 0, inv, contracts,
				fmt.Sprintf("top_up need=%d held_validated=%d", need, d.HeldValidated), d.Enforced)
			d.Promoted = moved
			need -= moved
		}
	}

	// --- leg 2: REMAIL (the WCL resurrection), within max_remail_share -------
	if need > 0 && inv.RemailEnabled {
		allow := minInt(need, remailCredit)
		if n := minInt(allow, d.RemailEligible); n > 0 {
			moved, err := c.remail(ctx, conn, day, inv, sources, isp, n, d.Enforced)
			if err != nil {
				log.Printf("[DripSupply] %s/%s remail failed: %v", lane, isp, err)
			}
			if moved > 0 {
				c.writeLedgerGrouped(ctx, conn, now, SupplyEventRemailEligible, lane, isp, moved, 0, inv, contracts,
					fmt.Sprintf("resurrection need=%d share=%.2f after_days=%d", need, inv.MaxRemailShare, inv.RemailAfterDays), d.Enforced)
				d.Remailed = moved
				need -= moved
			}
		}
	}

	if need <= 0 {
		d.Reason = fmt.Sprintf("filled from stock promoted=%d remailed=%d", d.Promoted, d.Remailed)
		return d
	}

	// --- leg 3: EO -----------------------------------------------------------
	if !inv.EOEnabled {
		return d.stop(SupplySkipEODisabled, fmt.Sprintf("unfilled=%d (eo_enabled=false)", need))
	}
	order := int(math.Ceil(float64(need) / d.Yield))
	if order < inv.MinEOOrder {
		// min_eo_order is a FLOOR on a PLACED order, not a target to inflate to:
		// rounding a 200-record shortfall up to 1,000 would buy 800 verdicts the
		// day's plan has no capacity to mail.
		return d.stop(SupplySkipBelowMinOrder, fmt.Sprintf("order=%d min=%d need=%d", order, inv.MinEOOrder, need))
	}
	if st.eoUnitCost > 0 && inv.MaxDailyEOSpendUSD > 0 {
		remaining := inv.MaxDailyEOSpendUSD - st.eoSpentToday[lane]
		affordable := int(math.Floor(remaining / st.eoUnitCost))
		if affordable < order {
			order = affordable
		}
		if order < inv.MinEOOrder {
			c.alertOncePerDay(ctx, day, "eo_spend_cap:"+lane, notify.TierWarn,
				fmt.Sprintf("drip supply: %s hit its EO spend cap · $%.2f", lane, inv.MaxDailyEOSpendUSD),
				fmt.Sprintf("Lane: %s\nISP: %s\nSpent today: $%.2f of $%.2f\nUnserved shortfall: %d records\nAffordable order: %d (min %d)",
					lane, isp, st.eoSpentToday[lane], inv.MaxDailyEOSpendUSD, need, affordable, inv.MinEOOrder),
				"Decide: raise max_daily_eo_spend_usd on the "+lane+" inventory contract, or accept the unserved demand")
			return d.stop(SupplySkipEOSpendCap,
				fmt.Sprintf("spent=$%.2f cap=$%.2f affordable=%d min=%d", st.eoSpentToday[lane], inv.MaxDailyEOSpendUSD, affordable, inv.MinEOOrder))
		}
	}
	if order > d.HeldOrderable {
		order = d.HeldOrderable
	}
	if order < inv.MinEOOrder {
		return d.stop(SupplySkipNoOrderStock, fmt.Sprintf("orderable=%d min=%d", d.HeldOrderable, inv.MinEOOrder))
	}

	moved, err := c.orderEO(ctx, conn, day, inv, sources, isp, order, d.Enforced)
	if err != nil {
		log.Printf("[DripSupply] %s/%s EO order failed: %v", lane, isp, err)
		return d.stop("order_failed", err.Error())
	}
	if moved > 0 {
		d.Ordered = moved
		d.OrderCostUSD = float64(moved) * st.eoUnitCost
		st.eoSpentToday[lane] += d.OrderCostUSD
		c.writeLedgerGrouped(ctx, conn, now, SupplyEventValidationOrdered, lane, isp, moved, st.eoUnitCost, inv, contracts,
			fmt.Sprintf("shortfall=%d need=%d yield=%.3f safety=%d horizon=%.1fh", d.Shortfall, d.Need, d.Yield, d.SafetyStock, horizon), d.Enforced)
	}
	d.Reason = fmt.Sprintf("promoted=%d remailed=%d ordered=%d yield=%.3f coverage=%.1fh", d.Promoted, d.Remailed, d.Ordered, d.Yield, d.CoverageHours)
	return d
}

func (d SupplyDecision) skip(reason string) SupplyDecision {
	d.Skip, d.Reason = reason, reason
	return d
}

// stop records a reason the fill stopped SHORT while the cell was genuinely
// evaluated. It keeps whatever the earlier legs moved.
func (d SupplyDecision) stop(reason, detail string) SupplyDecision {
	d.Skip = reason
	d.Reason = reason
	if detail != "" {
		d.Reason = reason + ": " + detail
	}
	return d
}

// remailAllowance is max_remail_share of the cell's intro demand, less what
// today already resurrected. Share OF THE DEMAND, not of the eligible pool: the
// contract bounds how much of a day's mail may be re-mailed, and a pool-relative
// share would let a big backlog dominate a small day.
func (c *SupplyController) remailAllowance(inv *InventoryContract, st *supplyState, k LaneISP, demand int) int {
	if inv.MaxRemailShare <= 0 || demand <= 0 {
		return 0
	}
	allow := int(math.Floor(inv.MaxRemailShare * float64(demand)))
	allow -= st.remailedToday[k]
	if allow < 0 {
		return 0
	}
	return allow
}

// throughHorizon is the slice of today's provisional award that must be backed
// before an order placed NOW comes back. Outside the sending window (or with no
// window left today) the whole award counts: the next window opens tomorrow and
// the order has to land by then.
func throughHorizon(provisional int, horizon, windowHours float64, now time.Time, domains []string, contracts *ActiveSet) int {
	if provisional <= 0 {
		return 0
	}
	left := hoursLeftInWindow(now, domains, contracts)
	if left <= 0 || horizon >= left {
		return provisional
	}
	return int(math.Ceil(float64(provisional) * horizon / left))
}

// hoursLeftInWindow is the widest remaining sending window across the lane's
// domains, in hours.
func hoursLeftInWindow(now time.Time, domains []string, contracts *ActiveSet) float64 {
	best := 0.0
	day := DenverDay(now)
	for _, name := range domains {
		d := contracts.Domains[name]
		if d == nil {
			continue
		}
		w, err := WindowOf(d)
		if err != nil {
			continue
		}
		_, end := w.Bounds(day)
		if h := end.Sub(now).Hours(); h > best {
			best = h
		}
	}
	return best
}

// governedToZero implements §5.5: no cleaning for a cell every one of whose
// domains is governed to 0. With no GovernorReader wired the check is inert and
// says so ONCE — silence here is how verdicts get bought for a held lane.
func (c *SupplyController) governedToZero(ctx context.Context, day time.Time, domains []string, isp string, contracts *ActiveSet) bool {
	if c.cfg.Governors == nil {
		c.govWarnOnce.Do(func() {
			log.Printf("[DripSupply] no GovernorReader wired — the §5.5 governor-zero gate is INERT; a lane governed to 0 can still be cleaned")
		})
		return false
	}
	if len(domains) == 0 {
		return false
	}
	for _, name := range domains {
		dc := contracts.Domains[name]
		if dc == nil {
			continue
		}
		w, err := WindowOf(dc)
		if err != nil {
			// An unreadable window must not read as "governed to zero" (that
			// would silently starve the lane) nor as "wide open".
			return false
		}
		ceilings, err := c.cfg.Governors.Ceilings(ctx, day, dc.SendingDomain, isp, w)
		if err != nil {
			log.Printf("[DripSupply] governor %s/%s unreadable (%v) — not treating the cell as governed", dc.SendingDomain, isp, err)
			return false
		}
		eff, _ := ApplyGovernors(dc.DailyMaxByISP[isp], ceilings)
		if eff > 0 {
			return false
		}
	}
	return true
}

func turnaroundFor(m map[string]float64, isp string) float64 {
	if h, ok := m[normISP(isp)]; ok && h > 0 {
		return h
	}
	return SeedEOTurnaroundHours
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// -----------------------------------------------------------------------------
// Queue transitions (§2.6 fill legs)
// -----------------------------------------------------------------------------

// promoteFreshSQL is leg 1: a held record whose verdict is still inside the
// contract's window becomes mailable at no cost. NEWEST first — a verdict
// decays, so the freshest validated record is the most trustworthy one to mail
// (verbatim intent from agents/jobs/drip_supply_lever.py's TOP-UP ordering).
const promoteFreshSQL = `
WITH pick AS (
    SELECT id
      FROM partner_clean_queue
     WHERE dataset_id = ANY($1)
       AND status = 'held'
       AND lower(COALESCE(NULLIF(isp_family, ''), 'other')) = $2
       AND mailed_at IS NULL
       AND COALESCE(touch_count, 0) = 0
       AND terminal_reason IS NULL
       AND (extra_metadata->>'warm_touches') IS NULL
       AND validated_at >= $3
     ORDER BY ingested_at DESC
     LIMIT $4
     FOR UPDATE SKIP LOCKED
)
UPDATE partner_clean_queue q
   SET status = 'ready'
  FROM pick
 WHERE q.id = pick.id AND q.status = 'held'
RETURNING q.dataset_id::text`

// remailSQL is leg 2: the WCL resurrection. touch_count back to 0, mailed_at
// cleared, the ladder reset, and remail_cycles incremented so a record cannot
// be recycled forever invisibly.
const remailSQL = `
WITH pick AS (
    SELECT id
      FROM partner_clean_queue
     WHERE dataset_id = ANY($1)
       AND status = 'mailed'
       AND lower(COALESCE(NULLIF(isp_family, ''), 'other')) = $2
       AND mailed_at < $3
       AND engaged_at IS NULL
       AND last_click_at IS NULL
       AND (next_touch_at IS NULL OR terminal_reason IN ('completed', 'ladder_complete'))
       AND terminal_reason IS DISTINCT FROM 'clicked_exit'
       AND (extra_metadata->>'warm_touches') IS NULL
     ORDER BY last_open_at DESC NULLS LAST, mailed_at ASC
     LIMIT $4
     FOR UPDATE SKIP LOCKED
)
UPDATE partner_clean_queue q
   SET status = 'ready',
       mailed_at = NULL,
       mailed_campaign_id = NULL,
       touch_count = 0,
       next_touch_at = NULL,
       terminal_reason = NULL,
       claimed_at = NULL,
       extra_metadata = COALESCE(q.extra_metadata, '{}'::jsonb)
           || jsonb_build_object(
                'remail_cycles', COALESCE((q.extra_metadata->>'remail_cycles')::int, 0) + 1,
                'remail_at', now()::text,
                'prior_mailed_at', q.mailed_at::text)
  FROM pick
 WHERE q.id = pick.id AND q.status = 'mailed'
RETURNING q.dataset_id::text`

// orderEOSQL is leg 3: unvalidated (or verdict-expired) held stock goes out for
// validation. OLDEST first — the oldest record is the one closest to aging out
// of usefulness, so it is the one worth spending a verdict on.
const orderEOSQL = `
WITH pick AS (
    SELECT id
      FROM partner_clean_queue
     WHERE dataset_id = ANY($1)
       AND status = 'held'
       AND lower(COALESCE(NULLIF(isp_family, ''), 'other')) = $2
       AND mailed_at IS NULL
       AND COALESCE(touch_count, 0) = 0
       AND terminal_reason IS NULL
       AND (extra_metadata->>'warm_touches') IS NULL
       AND (extra_metadata->>'globusa_flag') IS NULL
       AND COALESCE(eo_attempts, 0) < 3
       AND (validated_at IS NULL OR validated_at < $3)
     ORDER BY ingested_at ASC
     LIMIT $4
     FOR UPDATE SKIP LOCKED
)
UPDATE partner_clean_queue q
   SET status = 'pending_eo'
  FROM pick
 WHERE q.id = pick.id AND q.status = 'held'
RETURNING q.dataset_id::text`

func (c *SupplyController) promoteFresh(ctx context.Context, conn *sql.Conn, day time.Time, inv *InventoryContract, sources []supplyDataset, isp string, n int, enforced bool) (int, error) {
	cutoff := dayOf(day).AddDate(0, 0, -verdictDays(inv))
	return c.moveChunked(ctx, conn, promoteFreshSQL, sources, isp, cutoff, n, enforced)
}

func (c *SupplyController) remail(ctx context.Context, conn *sql.Conn, day time.Time, inv *InventoryContract, sources []supplyDataset, isp string, n int, enforced bool) (int, error) {
	after := inv.RemailAfterDays
	if after < 0 {
		after = 0
	}
	cutoff := dayOf(day).AddDate(0, 0, -after)
	return c.moveChunked(ctx, conn, remailSQL, sources, isp, cutoff, n, enforced)
}

func (c *SupplyController) orderEO(ctx context.Context, conn *sql.Conn, day time.Time, inv *InventoryContract, sources []supplyDataset, isp string, n int, enforced bool) (int, error) {
	cutoff := dayOf(day).AddDate(0, 0, -verdictDays(inv))
	return c.moveChunked(ctx, conn, orderEOSQL, sources, isp, cutoff, n, enforced)
}

func verdictDays(inv *InventoryContract) int {
	if inv != nil && inv.VerdictValidDays > 0 {
		return inv.VerdictValidDays
	}
	return 60
}

// moveChunked runs a transition in chunks of at most ChunkSize and returns the
// number of rows moved, keyed per dataset so the ledger can attribute each
// source_slug. In shadow mode (enforced=false) it moves NOTHING and reports the
// requested size against the lane's first source, so the shadow ledger records
// what the controller WOULD have done.
func (c *SupplyController) moveChunked(ctx context.Context, conn *sql.Conn, query string, sources []supplyDataset, isp string, cutoff time.Time, n int, enforced bool) (int, error) {
	if n <= 0 || len(sources) == 0 {
		return 0, nil
	}
	if !enforced {
		return n, nil
	}
	ids := make([]string, 0, len(sources))
	for _, s := range sources {
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)

	moved := 0
	for moved < n {
		chunk := minInt(c.cfg.ChunkSize, n-moved)
		rows, err := conn.QueryContext(ctx, query, pq.Array(ids), isp, cutoff, chunk)
		if err != nil {
			return moved, err
		}
		got := 0
		for rows.Next() {
			var dsID string
			if err := rows.Scan(&dsID); err != nil {
				rows.Close()
				return moved, err
			}
			got++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return moved, err
		}
		moved += got
		// Fewer rows than asked for means the pool is exhausted (or every
		// remaining row is locked by a concurrent claim). Either way, looping
		// again would spin.
		if got < chunk {
			break
		}
	}
	return moved, nil
}

// -----------------------------------------------------------------------------
// Ledger and outcomes
// -----------------------------------------------------------------------------

const supplyLedgerInsertCols = `(occurred_at, lane, source_slug, isp, event, quantity, unit_cost, total_cost, reason, source_contract_version, inventory_contract_version)`

// writeLedgerGrouped writes ONE ledger row per source_slug for the action. A
// ledger write that fails must never unwind an action that already happened —
// the records ARE moved — so the failure is logged and the pass continues.
func (c *SupplyController) writeLedgerGrouped(
	ctx context.Context, conn *sql.Conn, at time.Time, event, lane, isp string, quantity int, unitCost float64,
	inv *InventoryContract, contracts *ActiveSet, reason string, enforced bool,
) {
	if quantity <= 0 {
		return
	}
	slugs := inv.AcceptedSources
	if len(slugs) == 0 {
		slugs = []string{""}
	}
	// The action is lane-grained; the ledger is source-grained. Split evenly
	// and give the remainder to the first source, so SUM(quantity) over the
	// ledger equals the records actually moved.
	per := quantity / len(slugs)
	rem := quantity % len(slugs)
	table := c.ledgerTable(enforced)
	invVer := 0
	if inv != nil {
		invVer = inv.Version
	}
	for i, slug := range slugs {
		q := per
		if i == 0 {
			q += rem
		}
		if q <= 0 {
			continue
		}
		var srcVer any
		if sc, err := contracts.Source(strings.TrimSpace(slug)); err == nil && sc != nil {
			srcVer = sc.Version
		}
		// occurred_at is the PASS's instant, not NOW(): the daily EO budget and
		// the remail share are read back with a Denver-day filter, and a pass
		// that straddles midnight must have its rows land in the day it decided
		// for. In production the two are the same instant.
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO `+table+` `+supplyLedgerInsertCols+`
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, at, lane, slug, isp, event, q, unitCost, unitCost*float64(q), reason, srcVer, invVer); err != nil {
			log.Printf("[DripSupply] ledger insert %s %s/%s/%s failed: %v", event, lane, slug, isp, err)
		}
	}
}

// writeOutcome is the "nothing is silent" surface: one drip_tick_outcomes row
// per lane per pass, in EVERY mode. A lane that was skipped, a lane that had
// nothing to do and a lane that never ran must be distinguishable from the
// outside — the SegmentMaterializer's invisibility is the anti-pattern.
func (c *SupplyController) writeOutcome(ctx context.Context, db *sql.DB, now time.Time, lane string, decisions []SupplyDecision) {
	if c.cfg.OutcomesDisabled || db == nil {
		return
	}
	outcome := OutcomeSkipped
	acted := 0
	caps := map[string]int{}
	reasons := make([]string, 0, len(decisions))
	for _, d := range decisions {
		acted += d.Promoted + d.Remailed + d.Ordered
		if d.ISP != "" {
			caps[d.ISP] = d.Promoted + d.Remailed + d.Ordered
		}
		if d.Reason != "" {
			label := d.ISP
			if label == "" {
				label = "lane"
			}
			reasons = append(reasons, label+"="+d.Reason)
		}
	}
	switch {
	case acted > 0:
		outcome = OutcomeFired
	case len(decisions) > 0:
		outcome = OutcomeZero
	}
	reason := strings.Join(reasons, "; ")
	if len(reason) > 900 {
		reason = reason[:900] + "…"
	}
	capsJSON, err := json.Marshal(caps)
	if err != nil {
		capsJSON = []byte("{}")
	}
	if _, err := db.ExecContext(ctx, upsertOutcomeSQL,
		now, lane, PassSupply, outcome, reason, string(capsJSON), acted, nil, outcomePriority(outcome)); err != nil {
		log.Printf("[DripSupply] tick outcome %s: %v", lane, err)
	}
}

// alertOncePerDay delivers a §6 alert at most once per key per Denver day.
func (c *SupplyController) alertOncePerDay(ctx context.Context, day time.Time, key string, tier notify.Tier, headline, body, action string) {
	if c == nil || c.cfg.AlertsDisabled {
		return
	}
	stamp := dayKey(day)
	c.alertMu.Lock()
	if c.alerted[key] == stamp {
		c.alertMu.Unlock()
		return
	}
	c.alerted[key] = stamp
	c.alertMu.Unlock()

	msg := notify.Message{
		Tier:     tier,
		Scope:    notify.ScopeDrip,
		Headline: headline,
		Body:     body,
		Action:   action,
	}
	if err := notify.Deliver(c.cfg.Notifier, msg); err != nil {
		log.Printf("[DripSupply] alert delivery failed: %v", err)
	}
	_ = ctx
}

// -----------------------------------------------------------------------------
// Worker
// -----------------------------------------------------------------------------

const supplyLockKey = "drip:supply:controller"

// SupplyWorker runs the controller hourly, and on demand when a Supply Ledger
// event moves a lane across its reorder threshold (§2.6 cadence).
type SupplyWorker struct {
	ctrl  *SupplyController
	db    *sql.DB
	redis *redis.Client

	disabled bool
	interval time.Duration
	// debounce is the minimum gap between TRIGGERED passes. The hourly pass is
	// never debounced. Without it a burst of validator batches would run the
	// whole estate's aggregates several times a minute.
	debounce time.Duration
	trigger  chan string
	nowFn    func() time.Time

	mu      sync.Mutex
	lastRun time.Time
}

// NewSupplyWorker builds the hourly worker. redisClient may be nil; distlock
// then falls back to a PG advisory lock.
//
// Kill switch: DRIP_SUPPLY_CONTROLLER_DISABLED=1. It is INDEPENDENT of
// DRIP_SUPPLY_CHAIN_MODE — the mode is the cutover dial and `off` is also the
// rollback, but an operator stopping this one worker must not have to move the
// whole subsystem's mode to do it.
func NewSupplyWorker(db *sql.DB, redisClient *redis.Client, cfg SupplyControllerConfig) *SupplyWorker {
	w := &SupplyWorker{
		ctrl:     NewSupplyController(db, cfg),
		db:       db,
		redis:    redisClient,
		interval: time.Hour,
		debounce: 5 * time.Minute,
		trigger:  make(chan string, 64),
		nowFn:    time.Now,
	}
	if v := os.Getenv("DRIP_SUPPLY_CONTROLLER_DISABLED"); v == "1" || strings.EqualFold(v, "true") {
		log.Println("[DripSupply] DRIP_SUPPLY_CONTROLLER_DISABLED set — hourly supply controller disabled")
		w.disabled = true
	}
	return w
}

// Mode reports the configured mode (for the boot log).
func (w *SupplyWorker) Mode() Mode {
	if w == nil {
		return ModeOff
	}
	return w.ctrl.cfg.Mode
}

// Disabled reports the kill switch.
func (w *SupplyWorker) Disabled() bool { return w == nil || w.disabled }

// Trigger asks for an out-of-band pass because `lane` crossed a reorder
// threshold. Non-blocking and lossy by design: the hourly pass is the
// guarantee, a trigger is only an accelerator, and a trigger that blocked would
// stall whatever ledger writer called it.
func (w *SupplyWorker) Trigger(lane string) {
	if w == nil || w.disabled {
		return
	}
	select {
	case w.trigger <- lane:
	default:
	}
}

// Start runs the hourly loop. Single goroutine; honors ctx.Done().
func (w *SupplyWorker) Start(ctx context.Context) {
	if w.disabled || w.ctrl.cfg.Mode == ModeOff {
		if !w.disabled {
			log.Println("[DripSupply] DRIP_SUPPLY_CHAIN_MODE=off — supply controller idle")
		}
		return
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.run(ctx, "boot")
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.run(ctx, "hourly")
		case lane := <-w.trigger:
			if w.debounced() {
				continue
			}
			w.run(ctx, "trigger:"+lane)
		}
	}
}

func (w *SupplyWorker) debounced() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.nowFn().Sub(w.lastRun) < w.debounce
}

func (w *SupplyWorker) run(ctx context.Context, why string) {
	lock := distlock.NewLock(w.redis, w.db, supplyLockKey, 30*time.Minute)
	ok, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[DripSupply] lock acquire failed: %v", err)
		return
	}
	if !ok {
		// The other instance is running this pass.
		return
	}
	defer func() {
		relCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := lock.Release(relCtx); err != nil {
			log.Printf("[DripSupply] lock release failed: %v", err)
		}
	}()

	now := w.nowFn()
	w.mu.Lock()
	w.lastRun = now
	w.mu.Unlock()

	run, err := w.ctrl.RunOnce(ctx, w.db, now)
	if err != nil {
		log.Printf("[DripSupply] pass (%s) FAILED: %v", why, err)
		w.ctrl.alertOncePerDay(ctx, DenverDay(now), "supply_pass_failed", notify.TierAlert,
			"drip supply controller pass failed",
			fmt.Sprintf("Trigger: %s\nMode: %s\nError: %v", why, w.ctrl.cfg.Mode, err),
			"Run: SELECT * FROM drip_tick_outcomes WHERE pass='supply' ORDER BY tick DESC LIMIT 20")
		return
	}
	log.Printf("[DripSupply] pass (%s) ok: cells=%d evaluated=%d promoted=%d remailed=%d ordered=%d spend=$%.2f",
		why, len(run.Decisions), run.Evaluated, run.Promoted, run.Remailed, run.Ordered, run.OrderCostUSD)
}
