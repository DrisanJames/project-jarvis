package dripsupply

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// Queryer is the database surface this package's helpers need: *sql.DB, *sql.Tx
// and *sql.Conn all satisfy it. contracts.go uses the Contract*-prefixed
// interfaces for the contract loaders; this is the capacity side's equivalent.
type Queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// -----------------------------------------------------------------------------
// DDL — the §1.2 shape, kept next to its readers
// -----------------------------------------------------------------------------
//
// These are VERBATIM copies of the WP1 statements in cmd/server/main.go
// (runStartupMigrations, the req118_create_drip_capacity_* / req118_idx_dcl_*
// entries). WP1 owns the production copy; these exist so the integration tests
// build the PRODUCTION shape — CHECK constraints, nullability and defaults
// included — and so a WP1/WP3 drift surfaces as a failing test here rather than
// as a 3am constraint violation. Keep them byte-identical.

// CapacityLedgerDDL is `drip_capacity_ledger` (§1.2) — append-only, MESSAGES,
// one row per wave-level allocation.
const CapacityLedgerDDL = `CREATE TABLE IF NOT EXISTS drip_capacity_ledger (
		allocation_id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		idempotency_key           TEXT NOT NULL UNIQUE,          -- domain|isp|lane|wave_key|domain_ver|dispatch_ver
		day                       DATE NOT NULL,                 -- Denver
		tick                      TIMESTAMPTZ NOT NULL,
		sending_domain            TEXT NOT NULL,
		isp                       TEXT NOT NULL,
		lane                      TEXT NOT NULL,
		touch_class               TEXT NOT NULL
			CHECK (touch_class IN ('intro','followup','remail')),
		domain_contract_version   INT  NOT NULL,
		dispatch_contract_version INT  NOT NULL,
		requested                 INT  NOT NULL,
		reserved                  INT  NOT NULL,
		committed                 INT  NOT NULL DEFAULT 0,
		released                  INT  NOT NULL DEFAULT 0,
		status                    TEXT NOT NULL
			CHECK (status IN ('reserved','committed','released','expired')),
		campaign_id               UUID,
		binding_reason            TEXT NOT NULL,                 -- domain_tokens|lane_demand|supply|governor:<name>|plan_share|requested|reserve_timeout|outside_window|no_balance|no_lane_balance
		release_reason            TEXT,                          -- set by Release()/ExpireStale(); binding_reason stays the grant's record
		domain_balance_after      INT  NOT NULL,
		lane_unfilled_after       INT  NOT NULL,
		created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at                TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`

// CapacityLedgerIndexDDL are the §1.2 indexes. The partial index is on
// created_at (not status) because ExpireStale filters AND orders by it.
var CapacityLedgerIndexDDL = []string{
	`CREATE INDEX IF NOT EXISTS idx_drip_capacity_ledger_day_domain_isp ON drip_capacity_ledger (day, sending_domain, isp)`,
	`CREATE INDEX IF NOT EXISTS idx_drip_capacity_ledger_day_lane_isp ON drip_capacity_ledger (day, lane, isp)`,
	`CREATE INDEX IF NOT EXISTS idx_drip_capacity_ledger_campaign ON drip_capacity_ledger (campaign_id)`,
	`CREATE INDEX IF NOT EXISTS idx_drip_capacity_ledger_reserved ON drip_capacity_ledger (created_at) WHERE status = 'reserved'`,
}

// CapacityBalanceDDL is `drip_capacity_balance` (§1.2) — the lockable running
// balance, one row per day×domain×ISP. It exists only so Reserve can lock ONE row.
const CapacityBalanceDDL = `CREATE TABLE IF NOT EXISTS drip_capacity_balance (
		day              DATE NOT NULL,
		sending_domain   TEXT NOT NULL,
		isp              TEXT NOT NULL,
		contracted       INT  NOT NULL DEFAULT 0,   -- from the active domain contract
		effective        INT  NOT NULL DEFAULT 0,   -- min(contracted, governors), recomputed each tick
		effective_reason TEXT NOT NULL DEFAULT '',  -- which governor bound effective (empty = none); persisted so every instance/API sees it
		tokens           NUMERIC NOT NULL DEFAULT 0,
		reserved         INT  NOT NULL DEFAULT 0,
		committed        INT  NOT NULL DEFAULT 0,
		released         INT  NOT NULL DEFAULT 0,
		last_refill_tick TIMESTAMPTZ,
		PRIMARY KEY (day, sending_domain, isp)
	)`

// LaneBalanceDDL is `drip_lane_balance` (§1.2) — one row per day×lane×ISP.
const LaneBalanceDDL = `CREATE TABLE IF NOT EXISTS drip_lane_balance (
		day                 DATE NOT NULL,
		lane                TEXT NOT NULL,
		isp                 TEXT NOT NULL,
		desired             INT NOT NULL DEFAULT 0,
		awarded_firm        INT NOT NULL DEFAULT 0,
		awarded_provisional INT NOT NULL DEFAULT 0,
		reserved            INT NOT NULL DEFAULT 0,
		committed           INT NOT NULL DEFAULT 0,
		unfilled            INT NOT NULL DEFAULT 0,
		PRIMARY KEY (day, lane, isp)
	)`

// -----------------------------------------------------------------------------
// Row types
// -----------------------------------------------------------------------------

// Balance mirrors one drip_capacity_balance row.
type Balance struct {
	Day           time.Time
	SendingDomain string
	ISP           string
	Contracted    int
	Effective     int
	// EffectiveReason is drip_capacity_balance.effective_reason: the governor
	// that bound `effective` below `contracted`, empty when the contract itself
	// was the ceiling. Persisted (not process-local) so every reader agrees.
	EffectiveReason string
	Tokens          float64
	Reserved        int
	Committed       int
	Released        int
	LastRefillTick  time.Time
}

// Headroom is the §2.2 term `effective - reserved - committed`, floored at 0.
func (b Balance) Headroom() int {
	h := b.Effective - b.Reserved - b.Committed
	if h < 0 {
		return 0
	}
	return h
}

// LaneBalance mirrors one drip_lane_balance row.
type LaneBalance struct {
	Day                time.Time
	Lane               string
	ISP                string
	Desired            int
	AwardedFirm        int
	AwardedProvisional int
	Reserved           int
	Committed          int
	Unfilled           int
}

// -----------------------------------------------------------------------------
// Lane reconciliation — the lane balance follows its contract, like the domain
// balance follows its own (R1)
// -----------------------------------------------------------------------------

// LaneInvariant is the identity every drip_lane_balance row must satisfy:
//
//	reserved + committed + unfilled == desired
//
// It is stated here because it was nowhere before, and that is exactly why it
// broke. THREE writers touch these columns with three different semantics and
// none of them owns the identity:
//
//	Reserve decrements unfilled          (reservation.go:398)
//	settle  adds the give-back back      (reservation.go:790)
//	the planner writes its award into it (planner.go:2035)
//
// So the drift is permanent and ONE-DIRECTIONAL — a lane loses allowance it is
// contractually owed and never gets it back. Measured in prod 2026-09-09
// 13:03Z, five of six yahoo_family rows had drifted; yahoo held 46,000 records
// ready against 3,324 of allowance and the lane stalled all day.
//
// laneDrift is the signed size of the violation, in messages, and it is
// deliberately the operator's number: POSITIVE means the lane is SHORT by that
// much. yahoo above reads 106,399 - (231 + 54,500 + 3,324) = 48,344 short.
// Zero means the row is clean.
func laneDrift(prev LaneBalance) int {
	return prev.Desired - (prev.Reserved + prev.Committed + prev.Unfilled)
}

// reseed reconciles ONE lane balance row: it follows the active dispatch
// contract AND enforces LaneInvariant, in that order, in one pass.
//
//	next.desired  = contractDesired
//	next.unfilled = max(contractDesired - reserved - committed, 0)
//
// PURE: `prev` is a value, is never written through, and a NEW LaneBalance
// comes back. No I/O, no clock. Idempotent by construction — it depends only on
// the contract and on reserved/committed, neither of which it writes — which
// matters because this now runs on EVERY tick (~15 s), not only when a contract
// moves.
//
// This SUPERSEDES the earlier delta form (`unfilled + (new - old desired)`).
// The delta form followed a contract step correctly but could only ever
// preserve an existing drift, and preserving the drift was the bug: it is what
// left yahoo with 3,324. Deriving `unfilled` from reserved + committed makes the
// identity true by construction rather than by everyone remembering to
// maintain it.
//
// Consumed volume is still preserved EXACTLY, and more directly than before:
// reserved and committed are the ledger's record of what the lane has spent,
// they are read and never written here, and the allowance is whatever the
// contract has left over them.
//
// Two bounds, because R3 says under- and over-correction are both wrong:
//
//   - reserved + committed > desired (a contract lowered below what is already
//     spent) floors `unfilled` at 0. It never goes negative — a negative
//     unfilled reads as unbounded the moment anything sums it — and it never
//     claws back capacity already reserved or committed. The ledger owns that.
//   - a NEGATIVE reserved or committed (only reachable on a corrupt row) is
//     read as 0, so a corrupt counter cannot INFLATE unfilled above the
//     contract. Fixing that row is the ledger rebuild's job, not this one's.
//
// One consequence, stated because it is a real behaviour change: the planner's
// award no longer survives inside `unfilled`. drip_lane_balance goes back to
// carrying the CONTRACT's line, and the plan keeps its own line where it
// belongs — drip_daily_plan, read as the plan_share term (planner.go
// PlanRemaining).
func reseed(prev LaneBalance, contractDesired int) LaneBalance {
	if contractDesired < 0 {
		contractDesired = 0
	}
	spent := 0
	if prev.Reserved > 0 {
		spent += prev.Reserved
	}
	if prev.Committed > 0 {
		spent += prev.Committed
	}
	next := prev
	next.Desired = contractDesired
	next.Unfilled = contractDesired - spent
	if next.Unfilled < 0 {
		next.Unfilled = 0
	}
	return next
}

// ReconcileResult reports what one reconciliation pass changed.
//
// Changed and Drifted are different questions and are counted separately on
// purpose. Changed = "this row was written", which includes the entirely
// healthy case of a contract that stepped up overnight. Drifted = "this row
// violated LaneInvariant against its OWN desired before we touched it", which
// is an accounting defect in Reserve or settle and must not be filed under
// routine maintenance. Counting a contract step as drift would make the drift
// signal permanent noise, and a permanently noisy signal is one nobody reads.
type ReconcileResult struct {
	Seen    int
	Changed int
	Drifted int
}

// ReconcileLaneBalances rewrites every lane balance row for `day` to follow its
// active dispatch contract and to satisfy LaneInvariant, applying reseed()
// under the row lock.
//
// It runs on EVERY tick, not only when a contract changed: the drift has three
// authors (see LaneInvariant) and none of them is the contract, so a
// contract-triggered pass would have repaired yahoo exactly never.
//
// The FOR UPDATE is not optional and is the same reasoning as refillOne's:
// Reserve decrements `unfilled` in its own transaction, so a read-modify-write
// here without the lock is a lost-update window that hands the decremented
// volume straight back.
//
// One short transaction per row, and rows are visited in sorted order, so two
// orchestrator instances reconciling the same day take the same rows in the
// same order and cannot deadlock. A row that does not exist is NOT created here
// — EnsureDayBalances owns creation, and reconciliation never invents a lane.
//
// A per-row failure is returned; the caller (TickStart) logs and carries on,
// because a lane that cannot be reconciled must degrade to "yesterday's shape",
// never to "no tick".
func (s *Service) ReconcileLaneBalances(ctx context.Context, day time.Time, contracts *ActiveSet) (ReconcileResult, error) {
	var res ReconcileResult
	if s == nil || s.db == nil {
		return res, errors.New("dripsupply: ReconcileLaneBalances called on a nil service")
	}
	if contracts == nil {
		return res, errors.New("dripsupply: ReconcileLaneBalances called with a nil contract set")
	}
	lanes := make([]string, 0, len(contracts.Dispatches))
	for l := range contracts.Dispatches {
		lanes = append(lanes, l)
	}
	sort.Strings(lanes)

	for _, name := range lanes {
		c := contracts.Dispatches[name]
		if c == nil {
			continue
		}
		// The SAME union EnsureDayBalances seeds (ContractedISPs), so the two
		// passes cannot disagree about which rows the day is supposed to have.
		// An excluded ISP is reconciled TO ZERO rather than skipped: skipping it
		// left a row that had been contracted yesterday still carrying
		// yesterday's desire after an exclusion landed, and the lane kept
		// spending against a number the contract had withdrawn.
		for _, isp := range ContractedISPs(c) {
			if err := ctx.Err(); err != nil {
				return res, fmt.Errorf("dripsupply: ReconcileLaneBalances cancelled after %d rows: %w", res.Seen, err)
			}
			n := isp
			changed, drift, err := s.reconcileLaneOne(ctx, day, c.Lane, n, ContractDesiredFor(c, n))
			if err != nil {
				return res, err
			}
			res.Seen++
			if changed {
				res.Changed++
			}
			if drift {
				res.Drifted++
			}
		}
	}
	return res, nil
}

func (s *Service) reconcileLaneOne(ctx context.Context, day time.Time, lane, isp string, contractDesired int) (changed, drift bool, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var prev LaneBalance
		err := tx.QueryRowContext(ctx, `
			SELECT desired, awarded_firm, awarded_provisional, reserved, committed, unfilled
			FROM drip_lane_balance
			WHERE day = $1::date AND lane = $2 AND isp = $3
			FOR UPDATE
		`, dayKey(day), lane, isp).Scan(&prev.Desired, &prev.AwardedFirm, &prev.AwardedProvisional,
			&prev.Reserved, &prev.Committed, &prev.Unfilled)
		if errors.Is(err, sql.ErrNoRows) {
			// No row = this lane×ISP is not open today. EnsureDayBalances owns
			// creation; a reconciliation never invents a lane.
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock lane balance %s/%s: %w", lane, isp, err)
		}
		next := reseed(prev, contractDesired)
		if next.Desired == prev.Desired && next.Unfilled == prev.Unfilled {
			// Already correct. No write, not counted — otherwise every tick
			// reports the whole estate as "changed" and the number means
			// nothing.
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE drip_lane_balance
			SET desired = $4, unfilled = $5
			WHERE day = $1::date AND lane = $2 AND isp = $3
		`, dayKey(day), lane, isp, next.Desired, next.Unfilled); err != nil {
			return fmt.Errorf("update lane balance %s/%s: %w", lane, isp, err)
		}
		changed = true
		if d := laneDrift(prev); d != 0 {
			// R2: a repaired invariant is a DEFECT REPORT, not maintenance.
			// Reserve or settle lost this allowance and will keep losing it;
			// silently fixing it every tick forever is how the cause survives.
			drift = true
			log.Printf("[DripSupply] lane balance DRIFT repaired %s/%s on %s: unfilled %d -> %d (delta %+d); the row was %d short of reserved+committed+unfilled == desired (%d + %d + %d != %d)",
				lane, isp, dayKey(day), prev.Unfilled, next.Unfilled, next.Unfilled-prev.Unfilled,
				d, prev.Reserved, prev.Committed, prev.Unfilled, prev.Desired)
		} else {
			log.Printf("[DripSupply] lane balance %s/%s on %s follows its contract: desired %d -> %d, unfilled %d -> %d (reserved %d + committed %d preserved)",
				lane, isp, dayKey(day), prev.Desired, next.Desired, prev.Unfilled, next.Unfilled, prev.Reserved, prev.Committed)
		}
		return nil
	})
	if err != nil {
		return false, false, fmt.Errorf("dripsupply: reconcile lane %s/%s on %s: %w", lane, isp, dayKey(day), err)
	}
	return changed, drift, nil
}

// -----------------------------------------------------------------------------
// What a dispatch contract SAYS about an ISP (contract-fulfilment rule 1)
// -----------------------------------------------------------------------------

// ContractedISPs is every ISP class a dispatch contract NAMES, normalised,
// deduplicated and sorted: the keys of desired_daily_intros UNION
// isp_exclusions.
//
// The union is the point. Both halves are the contract SPEAKING about an ISP —
// one says "this many", the other says "none" — and both must produce a lane
// balance row, because a row is what separates a contracted zero from an
// unseeded day (see EnsureDayBalances' header). Only an ISP in NEITHER list is
// genuinely absent, and absence is the one case that still fails Reserve closed.
//
// PURE: no I/O, no clock, and deterministic for a given contract — the seeding
// pass and the reconciliation pass call it so they cannot disagree about which
// rows the day is supposed to have.
func ContractedISPs(c *DispatchContract) []string {
	if c == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(c.DesiredDailyIntros)+len(c.ISPExclusions))
	out := make([]string, 0, len(c.DesiredDailyIntros)+len(c.ISPExclusions))
	add := func(raw string) {
		n := normISP(raw)
		if n == "" {
			return
		}
		if _, dup := seen[n]; dup {
			return
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	for _, k := range sortedKeys(c.DesiredDailyIntros) {
		add(k)
	}
	for _, e := range c.ISPExclusions {
		add(e)
	}
	sort.Strings(out)
	return out
}

// ContractDesiredFor is the contract's desired_daily_intros for ONE normalised
// ISP, with the two rules that make it a promise rather than a hint:
//
//   - an ISP in isp_exclusions is 0, whatever desired_daily_intros says. An
//     exclusion is the stronger statement; a contract carrying both is a
//     validation defect and the safe reading of a contradiction is "none".
//   - a negative is 0. `unfilled` is summed by every reader and a negative
//     reads as unbounded the moment anything adds it up.
//
// Keys are matched on their normalised form, so an operator-authored "Yahoo"
// and "yahoo" resolve to the same class. When two raw keys normalise together
// the sorted-first one wins, which is the same tie-break laneDesiredFor uses.
//
// PURE.
func ContractDesiredFor(c *DispatchContract, isp string) int {
	if c == nil {
		return 0
	}
	n := normISP(isp)
	for _, e := range c.ISPExclusions {
		if normISP(e) == n {
			return 0
		}
	}
	for _, k := range sortedKeys(c.DesiredDailyIntros) {
		if normISP(k) != n {
			continue
		}
		if v := c.DesiredDailyIntros[k]; v > 0 {
			return v
		}
		return 0
	}
	return 0
}

// EnsureDayResult reports what a seeding pass created.
type EnsureDayResult struct {
	DomainRowsCreated int
	LaneRowsCreated   int
	DomainRowsSeen    int
	LaneRowsSeen      int
	// LaneRowsZero counts the rows seeded with desired = 0 — an ISP the
	// dispatch contract names and deliberately wants nothing on (an explicit
	// zero, or an ISP in isp_exclusions). They exist so an explicit zero and an
	// absent key are DIFFERENT things at Reserve time; see the header.
	LaneRowsZero int
}

// EnsureDayBalances creates the day's drip_capacity_balance and
// drip_lane_balance rows from the active contracts. It is idempotent by
// ON CONFLICT DO NOTHING and is safe to re-run at any point in the day — a
// second call at 14:00 must NOT reset a domain's reserved/committed counters,
// which is exactly what a DO UPDATE would do and why this is DO NOTHING.
//
// Seeding rules:
//   - contracted = daily_max_by_isp[isp]; effective = contracted. Governors are
//     applied by the first RefillDomain of the day, which §2.8 runs before any
//     reservation.
//   - tokens = one interval of credit (effective/active_intervals) and
//     last_refill_tick = the window start, so the day opens with exactly one
//     interval available and never more.
//   - lane rows: unfilled = desired. The planner (WP6) overwrites awarded_* and
//     unfilled when it freezes the day; until it does, desired is the lane ceiling.
//   - EVERY ISP the dispatch contract NAMES gets a lane row, including the ones
//     it names with desired = 0 and the ones it excludes. Both seed
//     desired = 0 / unfilled = 0.
//
// That last rule is a 2026-09-09 REVERSAL of "an ISP with desired <= 0 gets NO
// lane row at all", and it is the whole of contract-fulfilment rule 1.
//
// The old rule made an explicit zero and an absent key the SAME thing at
// Reserve time: both missed the lane row, both took the sql.ErrNoRows branch,
// and both came back `no_lane_balance`. So an operator reading a zero grant
// could not tell "this lane wants nothing on gmail today, as contracted" from
// "the day was never seeded and the estate is dark" — and on 2026-09-05 the
// estate WAS dark for 11h42m behind exactly that string (44,658 denials, and
// drip_tick_outcomes never carried the word once).
//
// A contract is a commitment in both directions, so a contracted 0 is a
// PROMISE OF ZERO and has to be recorded as one: the row exists, the grant is
// 0, and the reason names `zero_desired` (reservation.go). An ABSENT key —
// an ISP no contract mentions — stays absent and still fails closed as
// `no_lane_balance`, which now means only one thing.
//
// Cost: one extra row per contracted-zero ISP per day (the estate's gmail ban
// is eight brands, so tens of rows, not thousands), and they are seeded by the
// same ON CONFLICT DO NOTHING insert as every other row.
func EnsureDayBalances(ctx context.Context, db Queryer, day time.Time, contracts *ActiveSet) (EnsureDayResult, error) {
	var res EnsureDayResult
	if db == nil {
		return res, errors.New("dripsupply: EnsureDayBalances called with a nil db")
	}
	if contracts == nil {
		return res, errors.New("dripsupply: EnsureDayBalances called with a nil contract set")
	}
	key := dayKey(day)

	domains := make([]string, 0, len(contracts.Domains))
	for d := range contracts.Domains {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	for _, name := range domains {
		c := contracts.Domains[name]
		if c == nil {
			continue
		}
		w, err := WindowOf(c)
		if err != nil {
			return res, err
		}
		start, _ := w.Bounds(day)
		intervals := w.ActiveIntervals()
		for _, isp := range sortedKeys(c.DailyMaxByISP) {
			if err := ctx.Err(); err != nil {
				return res, fmt.Errorf("dripsupply: EnsureDayBalances cancelled: %w", err)
			}
			contracted := c.DailyMaxByISP[isp]
			if contracted < 0 {
				return res, fmt.Errorf("dripsupply: domain %s isp %s has a negative daily max (%d)", c.SendingDomain, isp, contracted)
			}
			res.DomainRowsSeen++
			tokens := float64(contracted) / float64(intervals)
			r, err := db.ExecContext(ctx, `
				INSERT INTO drip_capacity_balance
					(day, sending_domain, isp, contracted, effective, effective_reason, tokens, reserved, committed, released, last_refill_tick)
				VALUES ($1::date, $2, $3, $4, $4, '', $5, 0, 0, 0, $6)
				ON CONFLICT (day, sending_domain, isp) DO NOTHING
			`, key, c.SendingDomain, normISP(isp), contracted, tokens, start)
			if err != nil {
				return res, fmt.Errorf("dripsupply: seed capacity balance %s/%s on %s: %w", c.SendingDomain, isp, key, err)
			}
			if n, err := r.RowsAffected(); err == nil && n > 0 {
				res.DomainRowsCreated += int(n)
			}
		}
	}

	lanes := make([]string, 0, len(contracts.Dispatches))
	for l := range contracts.Dispatches {
		lanes = append(lanes, l)
	}
	sort.Strings(lanes)

	for _, name := range lanes {
		c := contracts.Dispatches[name]
		if c == nil {
			continue
		}
		for _, isp := range ContractedISPs(c) {
			if err := ctx.Err(); err != nil {
				return res, fmt.Errorf("dripsupply: EnsureDayBalances cancelled: %w", err)
			}
			desired := ContractDesiredFor(c, isp)
			res.LaneRowsSeen++
			if desired == 0 {
				res.LaneRowsZero++
			}
			r, err := db.ExecContext(ctx, `
				INSERT INTO drip_lane_balance
					(day, lane, isp, desired, awarded_firm, awarded_provisional, reserved, committed, unfilled)
				VALUES ($1::date, $2, $3, $4, 0, 0, 0, 0, $4)
				ON CONFLICT (day, lane, isp) DO NOTHING
			`, key, c.Lane, isp, desired)
			if err != nil {
				return res, fmt.Errorf("dripsupply: seed lane balance %s/%s on %s: %w", c.Lane, isp, key, err)
			}
			if rows, err := r.RowsAffected(); err == nil && rows > 0 {
				res.LaneRowsCreated += int(rows)
			}
		}
	}
	return res, nil
}

// RebuildResult reports what a rebuild touched.
type RebuildResult struct {
	DomainRows int
	LaneRows   int
}

// RebuildFromLedger recomputes the reserved/committed/released counters on both
// balance tables for one day from drip_capacity_ledger, which is the append-only
// truth (§1.2: the balances exist "only so reserve() can SELECT … FOR UPDATE one
// row"). Run at midnight and on demand after any manual ledger surgery.
//
// It does NOT rebuild tokens (a pacing artifact with no ledger representation)
// beyond clamping them to effective, and it does not touch contracted/effective
// (those come from the contract + governors via RefillDomain).
//
// The whole rebuild is one transaction: a crash between the zeroing statement
// and the aggregate would otherwise leave every balance in the day reading zero
// reserved — which would let the next tick grant the day's capacity twice.
func RebuildFromLedger(ctx context.Context, db *sql.DB, day time.Time) (RebuildResult, error) {
	var res RebuildResult
	if db == nil {
		return res, errors.New("dripsupply: RebuildFromLedger called with a nil db")
	}
	key := dayKey(day)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("dripsupply: RebuildFromLedger begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '30s'`); err != nil {
		return res, fmt.Errorf("dripsupply: RebuildFromLedger set statement_timeout: %w", err)
	}

	// Zero every balance row for the day first: a row whose ledger entries were
	// deleted or never existed must read zero, and an UPDATE … FROM (aggregate)
	// cannot touch a row the aggregate has no group for.
	if _, err := tx.ExecContext(ctx, `
		UPDATE drip_capacity_balance
		SET reserved = 0, committed = 0, released = 0, tokens = LEAST(tokens, effective)
		WHERE day = $1::date
	`, key); err != nil {
		return res, fmt.Errorf("dripsupply: zero capacity balances for %s: %w", key, err)
	}

	dr, err := tx.ExecContext(ctx, `
		UPDATE drip_capacity_balance b
		SET reserved  = COALESCE(a.resv, 0),
		    committed = COALESCE(a.comm, 0),
		    released  = COALESCE(a.rel, 0)
		FROM (
			SELECT l.sending_domain, l.isp,
			       SUM(GREATEST(l.reserved - l.committed - l.released, 0)) FILTER (WHERE l.status = 'reserved') AS resv,
			       SUM(l.committed) AS comm,
			       SUM(l.released)  AS rel
			FROM drip_capacity_ledger l
			WHERE l.day = $1::date
			GROUP BY l.sending_domain, l.isp
		) a
		WHERE b.day = $1::date AND b.sending_domain = a.sending_domain AND b.isp = a.isp
	`, key)
	if err != nil {
		return res, fmt.Errorf("dripsupply: rebuild capacity balances for %s: %w", key, err)
	}
	if n, err := dr.RowsAffected(); err == nil {
		res.DomainRows = int(n)
	}

	// Lane side. unfilled rebuilds against the planner's award when there is one
	// and against desired when there is not (pre-plan seeding, §EnsureDayBalances).
	lr, err := tx.ExecContext(ctx, `
		UPDATE drip_lane_balance b
		SET reserved  = COALESCE(a.resv, 0),
		    committed = COALESCE(a.comm, 0),
		    unfilled  = GREATEST(
			CASE WHEN b.awarded_firm + b.awarded_provisional > 0
			     THEN b.awarded_firm + b.awarded_provisional
			     ELSE b.desired END
			- COALESCE(a.resv, 0) - COALESCE(a.comm, 0), 0)
		FROM (
			SELECT l.lane, l.isp,
			       SUM(GREATEST(l.reserved - l.committed - l.released, 0)) FILTER (WHERE l.status = 'reserved') AS resv,
			       SUM(l.committed) AS comm
			FROM drip_capacity_ledger l
			WHERE l.day = $1::date
			GROUP BY l.lane, l.isp
		) a
		WHERE b.day = $1::date AND b.lane = a.lane AND b.isp = a.isp
	`, key)
	if err != nil {
		return res, fmt.Errorf("dripsupply: rebuild lane balances for %s: %w", key, err)
	}
	if n, err := lr.RowsAffected(); err == nil {
		res.LaneRows = int(n)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE drip_lane_balance b
		SET reserved = 0, committed = 0,
		    unfilled = CASE WHEN b.awarded_firm + b.awarded_provisional > 0
		                    THEN b.awarded_firm + b.awarded_provisional
		                    ELSE b.desired END
		WHERE b.day = $1::date
		  AND NOT EXISTS (
			SELECT 1 FROM drip_capacity_ledger l
			WHERE l.day = b.day AND l.lane = b.lane AND l.isp = b.isp
		  )
	`, key); err != nil {
		return res, fmt.Errorf("dripsupply: zero orphan lane balances for %s: %w", key, err)
	}

	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("dripsupply: RebuildFromLedger commit: %w", err)
	}
	return res, nil
}

// normISP is the one place an ISP class is normalised. Balances, ledger rows and
// governor lookups must all agree or a reservation silently locks a different
// row than the one it decrements.
func normISP(isp string) string { return strings.ToLower(strings.TrimSpace(isp)) }
