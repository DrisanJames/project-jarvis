package dripsupply

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// EnsureDayResult reports what a seeding pass created.
type EnsureDayResult struct {
	DomainRowsCreated int
	LaneRowsCreated   int
	DomainRowsSeen    int
	LaneRowsSeen      int
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
//   - an ISP in isp_exclusions, or with desired <= 0, gets NO lane row at all —
//     "absent ISP = 0 (not wanted)" (§1.1), and a missing lane row fails Reserve
//     closed rather than granting from a zero-desire lane.
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
		excluded := make(map[string]struct{}, len(c.ISPExclusions))
		for _, e := range c.ISPExclusions {
			excluded[normISP(e)] = struct{}{}
		}
		for _, isp := range sortedKeys(c.DesiredDailyIntros) {
			if err := ctx.Err(); err != nil {
				return res, fmt.Errorf("dripsupply: EnsureDayBalances cancelled: %w", err)
			}
			n := normISP(isp)
			desired := c.DesiredDailyIntros[isp]
			if _, skip := excluded[n]; skip || desired <= 0 {
				continue
			}
			res.LaneRowsSeen++
			r, err := db.ExecContext(ctx, `
				INSERT INTO drip_lane_balance
					(day, lane, isp, desired, awarded_firm, awarded_provisional, reserved, committed, unfilled)
				VALUES ($1::date, $2, $3, $4, 0, 0, 0, 0, $4)
				ON CONFLICT (day, lane, isp) DO NOTHING
			`, key, c.Lane, n, desired)
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
