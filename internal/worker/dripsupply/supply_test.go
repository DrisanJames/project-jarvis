package dripsupply

// REQ-118 WP7 tests (docs/DRIP_SUPPLY_CHAIN_DESIGN.md §2.6, §5.5, §8.2 test 9).
//
// These run against REAL Postgres, not sqlmock. What is under test is a set of
// SQL predicates — "held with a live verdict", "mailed non-engager whose ladder
// finished", "unvalidated and EO-retryable" — and a mock that returns canned
// rows cannot tell a correct predicate from one that selects the whole table.
// The §8.2 test 9 assertion in particular ("no cap-0 or excluded ISP receives an
// EO order") is only meaningful if the rows for those ISPs really are sitting in
// the queue next to the ones that DO get ordered.
//
// Local apex-postgres container, scratch database `req118_res`, one schema per
// test dropped at the end. Nothing here can reach production: the DSN is
// hard-defaulted to localhost and every test SKIPS (never fails, never falls
// back) when it is unreachable.
//
// The harness is self-contained rather than reusing reservation_test.go's:
// those files are owned by other work packages and are being edited
// concurrently. Same scratch database, distinct names, `sup` prefix throughout.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// -----------------------------------------------------------------------------
// Harness
// -----------------------------------------------------------------------------

const (
	supAdminDSNEnv     = "DRIPSUPPLY_TEST_ADMIN_DSN"
	supDefaultAdminDSN = "postgres://apex_user:apex_password@localhost:5432/apex_db?sslmode=disable"
	supScratchDBName   = "req118_res"
)

func supAdminDSN() string {
	if v := strings.TrimSpace(os.Getenv(supAdminDSNEnv)); v != "" {
		return v
	}
	return supDefaultAdminDSN
}

func supScratchDSN(t *testing.T) string {
	t.Helper()
	dsn := supAdminDSN()
	i := strings.LastIndex(dsn, "/")
	if i < 0 {
		t.Skipf("cannot derive a scratch DSN from %q", dsn)
	}
	tail := dsn[i+1:]
	q := ""
	if j := strings.Index(tail, "?"); j >= 0 {
		q = tail[j:]
	}
	return dsn[:i+1] + supScratchDBName + q
}

func supEnsureScratchDB(t *testing.T) {
	t.Helper()
	admin, err := sql.Open("postgres", supAdminDSN())
	if err != nil {
		t.Skipf("cannot open admin DSN: %v", err)
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Skipf("local postgres unreachable (%v) — set %s to run the WP7 integration tests", err, supAdminDSNEnv)
	}
	var exists bool
	if err := admin.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, supScratchDBName).Scan(&exists); err != nil {
		t.Skipf("cannot list databases: %v", err)
	}
	if exists {
		return
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+supScratchDBName); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		t.Skipf("cannot create scratch database %s: %v", supScratchDBName, err)
	}
}

// supSupplyLedgerDDL is a VERBATIM copy of the WP1 statement in
// cmd/server/main.go (req118_create_drip_supply_ledger), CHECK constraint
// included. A WP1/WP7 drift therefore breaks these tests rather than
// production: an event name the CHECK rejects fails here first.
const supSupplyLedgerDDL = `CREATE TABLE IF NOT EXISTS drip_supply_ledger (
		entry_id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		occurred_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		lane                       TEXT NOT NULL,
		source_slug                TEXT NOT NULL,
		isp                        TEXT NOT NULL,
		event                      TEXT NOT NULL CHECK (event IN (
			'RECEIVED','PRECHECK_PASSED','SUPPRESSED','INTERNAL_INVALID',
			'VALIDATION_ORDERED','VALIDATION_VALID','VALIDATION_INVALID','VALIDATION_NO_VERDICT',
			'MAILABLE','REMAIL_ELIGIBLE','RESERVED_FOR_INTRO','CONSUMED','EXPIRED','RELEASED')),
		quantity                   INT NOT NULL,
		unit_cost                  NUMERIC NOT NULL DEFAULT 0,
		total_cost                 NUMERIC NOT NULL DEFAULT 0,
		batch_id                   UUID,
		reservation_id             UUID,
		reason                     TEXT,
		source_contract_version    INT,
		inventory_contract_version INT
	)`

// supTickOutcomesDDL is the WP1 shape of drip_tick_outcomes.
const supTickOutcomesDDL = `CREATE TABLE IF NOT EXISTS drip_tick_outcomes (
		tick        TIMESTAMPTZ NOT NULL,
		lane        TEXT NOT NULL,
		pass        TEXT NOT NULL,
		outcome     TEXT NOT NULL CHECK (outcome IN ('fired','skipped','zero','failed')),
		reason      TEXT NOT NULL DEFAULT '',
		caps_seen   JSONB NOT NULL DEFAULT '{}'::jsonb,
		claimed     INT NOT NULL DEFAULT 0,
		campaign_id UUID,
		created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (tick, lane, pass)
	)`

// supPCQDDL is a faithful SUBSET of partner_clean_queue: every column the
// controller's predicates touch, with the production types verified against
// information_schema on 2026-09-03. Columns no query here reads are omitted
// because they add nothing a test can observe.
const supPCQDDL = `CREATE TABLE IF NOT EXISTS partner_clean_queue (
		id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		batch_id           UUID NOT NULL DEFAULT gen_random_uuid(),
		dataset_id         UUID NOT NULL,
		partner_id         UUID NOT NULL DEFAULT gen_random_uuid(),
		vertical           TEXT NOT NULL,
		email              TEXT NOT NULL,
		email_md5          VARCHAR NOT NULL,
		isp_family         TEXT NOT NULL,
		status             TEXT NOT NULL,
		eo_result_code     INT,
		eo_result          TEXT,
		eo_attempts        INT NOT NULL DEFAULT 0,
		mailed_campaign_id UUID,
		mailed_brand       TEXT,
		ingested_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		validated_at       TIMESTAMPTZ,
		claimed_at         TIMESTAMPTZ,
		mailed_at          TIMESTAMPTZ,
		extra_metadata     JSONB NOT NULL DEFAULT '{}'::jsonb,
		touch_count        INT NOT NULL DEFAULT 0,
		next_touch_at      TIMESTAMPTZ,
		subscriber_id      UUID,
		engaged_at         TIMESTAMPTZ,
		terminal_reason    TEXT,
		last_open_at       TIMESTAMPTZ,
		last_click_at      TIMESTAMPTZ
	)`

const supDatasetsDDL = `CREATE TABLE IF NOT EXISTS partner_datasets (
		id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		partner_id       UUID NOT NULL DEFAULT gen_random_uuid(),
		name             TEXT NOT NULL DEFAULT '',
		slug             TEXT NOT NULL,
		vertical         TEXT NOT NULL,
		paused_emergency BOOLEAN NOT NULL DEFAULT FALSE,
		status           TEXT NOT NULL DEFAULT 'active'
	)`

const supDripStateDDL = `CREATE TABLE IF NOT EXISTS partner_drip_state (
		vertical         TEXT PRIMARY KEY,
		next_brand_index INT NOT NULL DEFAULT 0
	)`

func supSchemaDDL() []string {
	out := []string{
		supPCQDDL, supDatasetsDDL, supDripStateDDL,
		supSupplyLedgerDDL,
		`CREATE TABLE IF NOT EXISTS drip_supply_ledger_shadow (LIKE drip_supply_ledger INCLUDING ALL)`,
		supTickOutcomesDDL,
		CapacityLedgerDDL,
		CostRatesDDL,
		`INSERT INTO drip_cost_rates (key, value, unit) VALUES ('eo_per_verdict', 0.000244, 'usd_per_verdict')
			ON CONFLICT (key) DO NOTHING`,
	}
	return out
}

func supNewDB(t *testing.T) *sql.DB {
	t.Helper()
	supEnsureScratchDB(t)
	schema := "w7" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")

	boot, err := sql.Open("postgres", supScratchDSN(t))
	if err != nil {
		t.Skipf("cannot open scratch DSN: %v", err)
	}
	defer boot.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := boot.PingContext(ctx); err != nil {
		t.Skipf("scratch database unreachable: %v", err)
	}
	if _, err := boot.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}

	dsn := supScratchDSN(t)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("postgres", dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatalf("open scratch: %v", err)
	}
	db.SetMaxOpenConns(8)
	t.Cleanup(func() {
		db.Close()
		if clean, err := sql.Open("postgres", supScratchDSN(t)); err == nil {
			_, _ = clean.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
			clean.Close()
		}
	})
	for _, stmt := range supSchemaDDL() {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("ddl %.70s: %v", strings.ReplaceAll(stmt, "\n", " "), err)
		}
	}
	return db
}

func supExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %.70s: %v", strings.ReplaceAll(q, "\n", " "), err)
	}
}

func supCount(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count %.70s: %v", strings.ReplaceAll(q, "\n", " "), err)
	}
	return n
}

// -----------------------------------------------------------------------------
// Fixture
// -----------------------------------------------------------------------------

const (
	supLane   = "wcl_test"
	supSlug   = "src_a"
	supDomain = "em.test.com"
	supBrand  = "tb"
)

// supFixture is one lane, one source, one sending domain, a frozen plan and
// whatever queue rows the test seeded.
type supFixture struct {
	db        *sql.DB
	datasetID string
	contracts *ActiveSet
	plan      *Plan
	now       time.Time
	day       time.Time
	inv       *InventoryContract
	disp      *DispatchContract
}

func supDenver(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		return time.UTC
	}
	return loc
}

// supNewFixture builds the canonical cell: 19h window (01:00–20:00), a lane
// wanting 9,500 aol intros, a plan awarding 2,000 firm + 7,000 provisional, and
// the clock at 10:00 MT so exactly 10 hours of window remain.
func supNewFixture(t *testing.T) *supFixture {
	t.Helper()
	db := supNewDB(t)
	loc := supDenver(t)
	now := time.Date(2026, 9, 10, 10, 0, 0, 0, loc)
	day := DenverDay(now)

	dsID := uuid.New().String()
	supExec(t, db, `INSERT INTO partner_datasets (id, slug, vertical, status, paused_emergency)
		VALUES ($1::uuid, $2, $3, 'active', false)`, dsID, supSlug, supLane)
	supExec(t, db, `INSERT INTO partner_drip_state (vertical) VALUES ($1)`, supLane)

	dom := &DomainContract{
		Meta:              Meta{Version: 3},
		SendingDomain:     supDomain,
		BrandCode:         supBrand,
		DailyMaxByISP:     map[string]int{"aol": 10000, "gmail": 0, "microsoft": 8000},
		ActiveWindowStart: "01:00",
		ActiveWindowEnd:   "20:00",
		IntervalMinutes:   15,
		MaxBurstIntervals: 2,
	}
	disp := &DispatchContract{
		Meta:                 Meta{Version: 5},
		Lane:                 supLane,
		OperatorPriorityTier: 1,
		DesiredDailyIntros:   map[string]int{"aol": 9500, "gmail": 0, "microsoft": 4000},
		DemandMode:           DemandModeTarget,
		AllowedDomains:       []string{supBrand},
		ISPExclusions:        []string{"microsoft"},
		LadderTouches:        5,
		LadderGapHours:       24,
		MaxIntroShare:        0.40,
	}
	inv := &InventoryContract{
		Meta:                Meta{Version: 7},
		Lane:                supLane,
		AcceptedSources:     []string{supSlug},
		VerdictValidDays:    60,
		EOEnabled:           true,
		MaxDailyEOSpendUSD:  50,
		MinEOOrder:          1000,
		MinCoverageHours:    8,
		TargetCoverageHours: 16,
		MaxCoverageHours:    36,
		RemailEnabled:       false,
		RemailAfterDays:     7,
		RemailMode:          RemailModeFullLadder,
		MaxRemailShare:      0.25,
	}
	src := &SourceContract{
		Meta:         Meta{Version: 2},
		SourceSlug:   supSlug,
		RecordClass:  "mortgage",
		EligibleISPs: []string{"aol", "gmail", "microsoft"},
	}

	f := &supFixture{
		db: db, datasetID: dsID, now: now, day: day, inv: inv, disp: disp,
		contracts: &ActiveSet{
			Day:           day,
			Domains:       map[string]*DomainContract{supDomain: dom},
			Dispatches:    map[string]*DispatchContract{supLane: disp},
			Inventories:   map[string]*InventoryContract{supLane: inv},
			SourcesBySlug: map[string]*SourceContract{supSlug: src},
		},
		plan: &Plan{
			Day:      day,
			FrozenAt: day,
			Rows: []PlanRow{{
				Day: day, Lane: supLane, ISP: "aol", SendingDomain: supDomain,
				AwardFirm: 2000, AwardProvisional: 7000,
			}},
		},
	}
	return f
}

// controller builds a controller wired to the fixture's injected contracts and
// plan. Governors default to nil (the §5.5 gate inert) unless a test passes one.
func (f *supFixture) controller(t *testing.T, mode Mode, opts ...func(*SupplyControllerConfig)) *SupplyController {
	t.Helper()
	cfg := SupplyControllerConfig{
		Mode:           mode,
		ContractSource: func(context.Context, time.Time) (*ActiveSet, error) { return f.contracts, nil },
		PlanSource:     func(context.Context, time.Time) (*Plan, bool, error) { return f.plan, f.plan != nil, nil },
		Yield:          NewMeasuredYield(),
		AlertsDisabled: true,
		Clock:          func() time.Time { return f.now },
	}
	for _, o := range opts {
		o(&cfg)
	}
	return NewSupplyController(f.db, cfg)
}

func (f *supFixture) run(t *testing.T, c *SupplyController) SupplyRun {
	t.Helper()
	run, err := c.RunOnce(context.Background(), f.db, f.now)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	return run
}

// decision returns the run's decision for one ISP.
func supDecision(t *testing.T, run SupplyRun, isp string) SupplyDecision {
	t.Helper()
	for _, d := range run.Decisions {
		if d.ISP == isp {
			return d
		}
	}
	t.Fatalf("no decision for isp %q (decisions: %+v)", isp, run.Decisions)
	return SupplyDecision{}
}

// supSeedHeld inserts `n` held rows for one ISP. validatedSQL is the
// validated_at expression: `NULL` makes them EO-orderable stock, a recent
// timestamp makes them free top-up stock.
func (f *supFixture) supSeedHeld(t *testing.T, isp string, n int, validatedSQL, prefix string) {
	t.Helper()
	supExec(t, f.db, fmt.Sprintf(`
		INSERT INTO partner_clean_queue (dataset_id, vertical, email, email_md5, isp_family, status, validated_at, ingested_at)
		SELECT $1::uuid, $2, $3 || g || '@x.test', md5($3 || g), $4, 'held', %s, $6::timestamptz - interval '40 days'
		FROM generate_series(1, $5) g`, validatedSQL),
		f.datasetID, supLane, prefix, isp, n, f.now)
}

// supSeedReady inserts `n` ready + freshly validated rows: the fresh_mailable
// pool.
func (f *supFixture) supSeedReady(t *testing.T, isp string, n int, prefix string) {
	t.Helper()
	supExec(t, f.db, `
		INSERT INTO partner_clean_queue (dataset_id, vertical, email, email_md5, isp_family, status, validated_at, ingested_at)
		SELECT $1::uuid, $2, $3 || g || '@x.test', md5($3 || g), $4, 'ready',
		       $6::timestamptz - interval '2 days', $6::timestamptz - interval '40 days'
		FROM generate_series(1, $5) g`,
		f.datasetID, supLane, prefix, isp, n, f.now)
}

// supSeedRemailable inserts `n` mailed non-engagers whose ladder has finished —
// the WCL resurrection population.
func (f *supFixture) supSeedRemailable(t *testing.T, isp string, n int, prefix string) {
	t.Helper()
	supExec(t, f.db, `
		INSERT INTO partner_clean_queue (dataset_id, vertical, email, email_md5, isp_family, status,
		                                 validated_at, ingested_at, mailed_at, touch_count)
		SELECT $1::uuid, $2, $3 || g || '@x.test', md5($3 || g), $4, 'mailed',
		       $6::timestamptz - interval '40 days', $6::timestamptz - interval '60 days',
		       $6::timestamptz - interval '30 days', 5
		FROM generate_series(1, $5) g`,
		f.datasetID, supLane, prefix, isp, n, f.now)
}

// supSeedYield writes the ledger history MeasuredYield reads. The VALID rows
// are stamped EARLIER than the ORDERED rows so the turnaround lateral finds no
// ORDERED→VALID pair and the p90 stays on its 2h seed — the yield under test is
// then the only measured input.
func (f *supFixture) supSeedYield(t *testing.T, isp string, ordered, valid int) {
	t.Helper()
	if ordered > 0 {
		supExec(t, f.db, `INSERT INTO drip_supply_ledger (occurred_at, lane, source_slug, isp, event, quantity)
			VALUES ($5, $1, $2, $3, 'VALIDATION_ORDERED', $4)`, supLane, supSlug, isp, ordered, f.now.Add(-time.Hour))
	}
	if valid > 0 {
		supExec(t, f.db, `INSERT INTO drip_supply_ledger (occurred_at, lane, source_slug, isp, event, quantity)
			VALUES ($5, $1, $2, $3, 'VALIDATION_VALID', $4)`, supLane, supSlug, isp, valid, f.now.Add(-3*time.Hour))
	}
}

// supStaticGovernor is a GovernorReader with a fixed ceiling, for §5.5.
type supStaticGovernor struct {
	ceiling int
	name    string
}

func (g supStaticGovernor) Ceilings(context.Context, time.Time, string, string, Window) ([]GovernorCeiling, error) {
	return []GovernorCeiling{{Name: g.name, Limit: g.ceiling}}, nil
}

// -----------------------------------------------------------------------------
// §8.2 test 9 — no cap-0 or excluded ISP receives an EO order, WITH the
// positive control in the same run.
// -----------------------------------------------------------------------------

func TestSupplyNoOrderForCapZeroOrExcludedISP(t *testing.T) {
	f := supNewFixture(t)
	// Identical stock on all three ISPs, so the ONLY thing that can separate
	// them is the contract.
	for _, isp := range []string{"aol", "gmail", "microsoft"} {
		f.supSeedHeld(t, isp, 5000, "NULL", "eo-"+isp+"-")
		f.supSeedReady(t, isp, 348, "rd-"+isp+"-")
	}
	f.supSeedYield(t, "aol", 10000, 7000)
	// gmail and microsoft also carry a plan award: a cell that is cap-0 or
	// excluded must be refused on the CONTRACT, not merely for want of demand.
	f.plan.Rows = append(f.plan.Rows,
		PlanRow{Day: f.day, Lane: supLane, ISP: "gmail", SendingDomain: supDomain, AwardProvisional: 7000},
		PlanRow{Day: f.day, Lane: supLane, ISP: "microsoft", SendingDomain: supDomain, AwardProvisional: 7000},
	)

	run := f.run(t, f.controller(t, ModeOn))

	// --- positive control: aol IS ordered ---------------------------------
	aol := supDecision(t, run, "aol")
	if aol.Ordered <= 0 {
		t.Fatalf("aol (desired=9500) got NO order — the negative results below would then prove nothing. decision=%+v", aol)
	}
	if aol.Skip != "" {
		t.Fatalf("aol skipped with %q, want an order", aol.Skip)
	}

	// --- the two negatives -------------------------------------------------
	gmail := supDecision(t, run, "gmail")
	if gmail.Ordered != 0 || gmail.Skip != SupplySkipZeroDesired {
		t.Errorf("gmail (desired=0): ordered=%d skip=%q, want 0 / %s", gmail.Ordered, gmail.Skip, SupplySkipZeroDesired)
	}
	ms := supDecision(t, run, "microsoft")
	if ms.Ordered != 0 || ms.Skip != SupplySkipISPExcluded {
		t.Errorf("microsoft (excluded): ordered=%d skip=%q, want 0 / %s", ms.Ordered, ms.Skip, SupplySkipISPExcluded)
	}

	// The queue itself, not just the decision struct: nothing moved off `held`
	// for the two refused ISPs.
	for _, isp := range []string{"gmail", "microsoft"} {
		if n := supCount(t, f.db, `SELECT COUNT(*) FROM partner_clean_queue WHERE isp_family=$1 AND status='pending_eo'`, isp); n != 0 {
			t.Errorf("%s: %d rows moved to pending_eo, want 0", isp, n)
		}
	}
	if n := supCount(t, f.db, `SELECT COUNT(*) FROM partner_clean_queue WHERE isp_family='aol' AND status='pending_eo'`); n != aol.Ordered {
		t.Errorf("aol pending_eo rows = %d, want %d (the order)", n, aol.Ordered)
	}
	// And the ledger carries an order for aol only.
	if n := supCount(t, f.db, `SELECT COUNT(*) FROM drip_supply_ledger WHERE event='VALIDATION_ORDERED' AND isp <> 'aol'`); n != 0 {
		t.Errorf("%d VALIDATION_ORDERED ledger rows for a non-aol ISP, want 0", n)
	}
}

// -----------------------------------------------------------------------------
// Lane-level hard gates (§2.6: never for a paused / inactive / state-less lane)
// -----------------------------------------------------------------------------

func TestSupplyNoOrderForIneligibleLane(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(t *testing.T, f *supFixture)
		wantWhy string
	}{
		{
			name: "paused_dataset",
			mutate: func(t *testing.T, f *supFixture) {
				supExec(t, f.db, `UPDATE partner_datasets SET paused_emergency = true WHERE id = $1::uuid`, f.datasetID)
			},
			wantWhy: SupplySkipLanePaused,
		},
		{
			name: "inactive_dataset",
			mutate: func(t *testing.T, f *supFixture) {
				supExec(t, f.db, `UPDATE partner_datasets SET status = 'archived' WHERE id = $1::uuid`, f.datasetID)
			},
			wantWhy: SupplySkipLaneInactive,
		},
		{
			name: "no_partner_drip_state_row",
			mutate: func(t *testing.T, f *supFixture) {
				supExec(t, f.db, `DELETE FROM partner_drip_state WHERE vertical = $1`, supLane)
			},
			wantWhy: SupplySkipNoLaneState,
		},
		{
			name: "contract_names_a_source_that_does_not_exist",
			mutate: func(t *testing.T, f *supFixture) {
				f.inv.AcceptedSources = []string{"src_does_not_exist"}
			},
			wantWhy: SupplySkipNoSource,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := supNewFixture(t)
			f.supSeedHeld(t, "aol", 5000, "NULL", "eo-")
			f.supSeedReady(t, "aol", 348, "rd-")
			f.supSeedYield(t, "aol", 10000, 7000)
			tc.mutate(t, f)

			run := f.run(t, f.controller(t, ModeOn))
			if run.Ordered != 0 {
				t.Fatalf("ordered %d records for an ineligible lane, want 0", run.Ordered)
			}
			if len(run.Decisions) != 1 || run.Decisions[0].Skip != tc.wantWhy {
				t.Fatalf("decisions = %+v, want one skip with reason %q", run.Decisions, tc.wantWhy)
			}
			if n := supCount(t, f.db, `SELECT COUNT(*) FROM partner_clean_queue WHERE status='pending_eo'`); n != 0 {
				t.Errorf("%d rows moved to pending_eo, want 0", n)
			}
		})
	}

	// NEGATIVE CONTROL: the same fixture with none of those mutations DOES
	// order — otherwise the four assertions above would pass on a controller
	// that never orders anything.
	t.Run("negative_control_eligible_lane_orders", func(t *testing.T) {
		f := supNewFixture(t)
		f.supSeedHeld(t, "aol", 5000, "NULL", "eo-")
		f.supSeedReady(t, "aol", 348, "rd-")
		f.supSeedYield(t, "aol", 10000, 7000)
		run := f.run(t, f.controller(t, ModeOn))
		if run.Ordered <= 0 {
			t.Fatalf("eligible lane ordered %d, want > 0", run.Ordered)
		}
	})
}

// -----------------------------------------------------------------------------
// §5.5 — a lane × ISP governed to zero is never cleaned
// -----------------------------------------------------------------------------

func TestSupplyGovernorZeroBlocksOrder(t *testing.T) {
	seed := func(t *testing.T, f *supFixture) {
		f.supSeedHeld(t, "aol", 5000, "NULL", "eo-")
		f.supSeedReady(t, "aol", 348, "rd-")
		f.supSeedYield(t, "aol", 10000, 7000)
	}

	t.Run("governed_to_zero", func(t *testing.T) {
		f := supNewFixture(t)
		seed(t, f)
		run := f.run(t, f.controller(t, ModeOn, func(c *SupplyControllerConfig) {
			c.Governors = supStaticGovernor{ceiling: 0, name: "throttle"}
		}))
		d := supDecision(t, run, "aol")
		if d.Ordered != 0 || d.Skip != SupplySkipGovernorZero {
			t.Fatalf("ordered=%d skip=%q, want 0 / %s", d.Ordered, d.Skip, SupplySkipGovernorZero)
		}
	})

	// NEGATIVE CONTROL: the same governor with a positive ceiling must NOT
	// block, or the test above would pass on any wiring that always skips.
	t.Run("governor_open_orders", func(t *testing.T) {
		f := supNewFixture(t)
		seed(t, f)
		run := f.run(t, f.controller(t, ModeOn, func(c *SupplyControllerConfig) {
			c.Governors = supStaticGovernor{ceiling: 9000, name: "throttle"}
		}))
		d := supDecision(t, run, "aol")
		if d.Ordered <= 0 {
			t.Fatalf("ordered=%d skip=%q, want an order with an open governor", d.Ordered, d.Skip)
		}
	})
}

// -----------------------------------------------------------------------------
// Order sizing: measured yield, seeded yield, min order, spend cap
// -----------------------------------------------------------------------------

// supExpectedOrder recomputes §2.6 independently of the controller, so the
// assertion pins the FORMULA rather than echoing the implementation.
func supExpectedOrder(fresh, provisional int, demand int, windowHours, hoursLeft, horizon, yield float64) (need, order int) {
	rate := float64(demand) / windowHours
	safety := int(math.Ceil(rate * horizon))
	through := int(math.Ceil(float64(provisional) * horizon / hoursLeft))
	need = through + safety - fresh
	order = int(math.Ceil(float64(need) / yield))
	return need, order
}

func TestSupplyOrderSizeUsesMeasuredYield(t *testing.T) {
	f := supNewFixture(t)
	f.supSeedHeld(t, "aol", 20000, "NULL", "eo-")
	f.supSeedReady(t, "aol", 348, "rd-")
	// 7,000 valid out of 10,000 ordered = 0.70, well over the 1,000 sample floor.
	f.supSeedYield(t, "aol", 10000, 7000)

	run := f.run(t, f.controller(t, ModeOn))
	d := supDecision(t, run, "aol")

	if math.Abs(d.Yield-0.70) > 1e-9 {
		t.Fatalf("yield = %v, want the measured 0.70 (7000/10000)", d.Yield)
	}
	if d.TurnaroundHours != SeedEOTurnaroundHours {
		t.Fatalf("turnaround = %v, want the %v seed (no ORDERED→VALID pair in the fixture)", d.TurnaroundHours, SeedEOTurnaroundHours)
	}
	wantNeed, wantOrder := supExpectedOrder(348, 7000, 9000, 19, 10, SeedEOTurnaroundHours, 0.70)
	if d.Need != wantNeed {
		t.Errorf("need = %d, want %d", d.Need, wantNeed)
	}
	if d.Ordered != wantOrder {
		t.Errorf("ordered = %d, want ceil(%d/0.70) = %d", d.Ordered, wantNeed, wantOrder)
	}
	if n := supCount(t, f.db, `SELECT COUNT(*) FROM partner_clean_queue WHERE status='pending_eo'`); n != wantOrder {
		t.Errorf("pending_eo rows = %d, want %d", n, wantOrder)
	}
	var qty int
	var unit, total float64
	if err := f.db.QueryRow(`SELECT quantity, unit_cost::float8, total_cost::float8
		FROM drip_supply_ledger WHERE event='VALIDATION_ORDERED' AND reason LIKE 'shortfall=%'`).Scan(&qty, &unit, &total); err != nil {
		t.Fatalf("read order ledger row: %v", err)
	}
	if qty != wantOrder {
		t.Errorf("ledger quantity = %d, want %d", qty, wantOrder)
	}
	if math.Abs(unit-0.000244) > 1e-12 {
		t.Errorf("unit_cost = %v, want the drip_cost_rates eo_per_verdict 0.000244", unit)
	}
	if math.Abs(total-unit*float64(wantOrder)) > 1e-9 {
		t.Errorf("total_cost = %v, want %v", total, unit*float64(wantOrder))
	}
	// NEGATIVE CONTROL on the yield itself: a DIFFERENT measured yield must
	// produce a different order from the same shortfall.
	if wantOrderSeed := int(math.Ceil(float64(wantNeed) / SeedEOYield)); wantOrder == wantOrderSeed {
		t.Fatalf("the 0.70 and %v yields size the same order (%d) — this fixture cannot distinguish them", SeedEOYield, wantOrder)
	}
}

func TestSupplyOrderSizeUsesSeedYieldBelowSample(t *testing.T) {
	f := supNewFixture(t)
	f.supSeedHeld(t, "aol", 20000, "NULL", "eo-")
	f.supSeedReady(t, "aol", 348, "rd-")
	// 500 ordered is BELOW YieldMinSample (1000): the measured 0.20 ratio must
	// be ignored in favour of the seed, not floored to MinEOYield.
	f.supSeedYield(t, "aol", 500, 100)

	run := f.run(t, f.controller(t, ModeOn))
	d := supDecision(t, run, "aol")
	if d.Yield != SeedEOYield {
		t.Fatalf("yield = %v, want the %v seed (only 500 ordered < %d sample)", d.Yield, SeedEOYield, YieldMinSample)
	}
	wantNeed, wantOrder := supExpectedOrder(348, 7000, 9000, 19, 10, SeedEOTurnaroundHours, SeedEOYield)
	if d.Need != wantNeed || d.Ordered != wantOrder {
		t.Errorf("need=%d ordered=%d, want %d / %d", d.Need, d.Ordered, wantNeed, wantOrder)
	}
}

// TestSupplyYieldFloorAndSample pins clampYield directly, including the floor
// that stops a broken verdict feed from collapsing every award.
func TestSupplyYieldFloorAndSample(t *testing.T) {
	cases := []struct {
		name           string
		ordered, valid int64
		want           float64
	}{
		{"below_sample_takes_seed", 999, 0, SeedEOYield},
		{"measured", 10000, 7000, 0.70},
		{"floor_applies", 10000, 100, MinEOYield},
		{"cap_at_one", 10000, 20000, MaxEOYield},
		{"zero_ordered_takes_seed", 0, 5000, SeedEOYield},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampYield(tc.ordered, tc.valid, YieldMinSample); math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("clampYield(%d,%d) = %v, want %v", tc.ordered, tc.valid, got, tc.want)
			}
		})
	}
}

func TestSupplyMinEOOrderRespected(t *testing.T) {
	f := supNewFixture(t)
	f.supSeedHeld(t, "aol", 20000, "NULL", "eo-")
	f.supSeedReady(t, "aol", 348, "rd-")
	f.supSeedYield(t, "aol", 10000, 7000)
	_, wantOrder := supExpectedOrder(348, 7000, 9000, 19, 10, SeedEOTurnaroundHours, 0.70)
	// One record above what this cell would order: the floor must refuse.
	f.inv.MinEOOrder = wantOrder + 1

	run := f.run(t, f.controller(t, ModeOn))
	d := supDecision(t, run, "aol")
	if d.Ordered != 0 {
		t.Fatalf("ordered %d with min_eo_order=%d above the computed order %d, want 0", d.Ordered, f.inv.MinEOOrder, wantOrder)
	}
	if d.Skip != SupplySkipBelowMinOrder {
		t.Errorf("skip = %q, want %s", d.Skip, SupplySkipBelowMinOrder)
	}
	if n := supCount(t, f.db, `SELECT COUNT(*) FROM partner_clean_queue WHERE status='pending_eo'`); n != 0 {
		t.Errorf("%d rows moved to pending_eo, want 0", n)
	}
	// NEGATIVE CONTROL: one record LOWER and the same cell orders.
	f2 := supNewFixture(t)
	f2.supSeedHeld(t, "aol", 20000, "NULL", "eo-")
	f2.supSeedReady(t, "aol", 348, "rd-")
	f2.supSeedYield(t, "aol", 10000, 7000)
	f2.inv.MinEOOrder = wantOrder
	if d2 := supDecision(t, f2.run(t, f2.controller(t, ModeOn)), "aol"); d2.Ordered != wantOrder {
		t.Fatalf("with min_eo_order=%d the cell ordered %d, want %d", wantOrder, d2.Ordered, wantOrder)
	}
}

func TestSupplyDailySpendCapRespected(t *testing.T) {
	f := supNewFixture(t)
	f.supSeedHeld(t, "aol", 20000, "NULL", "eo-")
	f.supSeedReady(t, "aol", 348, "rd-")
	f.supSeedYield(t, "aol", 10000, 7000)
	_, uncapped := supExpectedOrder(348, 7000, 9000, 19, 10, SeedEOTurnaroundHours, 0.70)

	// $0.50 at $0.000244/verdict affords 2,049 — fewer than the uncapped order,
	// more than min_eo_order, so the cap must TRIM rather than refuse.
	f.inv.MaxDailyEOSpendUSD = 0.50
	affordable := int(math.Floor(0.50 / 0.000244))
	if affordable >= uncapped || affordable < f.inv.MinEOOrder {
		t.Fatalf("fixture broken: affordable=%d uncapped=%d min=%d", affordable, uncapped, f.inv.MinEOOrder)
	}

	d := supDecision(t, f.run(t, f.controller(t, ModeOn)), "aol")
	if d.Ordered != affordable {
		t.Fatalf("ordered = %d, want the affordable %d (cap $%.2f trims the uncapped %d)", d.Ordered, affordable, f.inv.MaxDailyEOSpendUSD, uncapped)
	}
	if d.OrderCostUSD > f.inv.MaxDailyEOSpendUSD {
		t.Errorf("spent $%.4f over the $%.2f cap", d.OrderCostUSD, f.inv.MaxDailyEOSpendUSD)
	}

	// A SECOND pass in the same day must find the budget already spent (read
	// back from the ledger, not from memory) and refuse.
	f.supSeedYield(t, "aol", 0, 0)
	run2 := f.run(t, f.controller(t, ModeOn))
	d2 := supDecision(t, run2, "aol")
	if d2.Ordered != 0 || d2.Skip != SupplySkipEOSpendCap {
		t.Errorf("second pass: ordered=%d skip=%q, want 0 / %s (budget already spent today)", d2.Ordered, d2.Skip, SupplySkipEOSpendCap)
	}
}

// -----------------------------------------------------------------------------
// Fill order: fresh → remail → EO (§2.6)
// -----------------------------------------------------------------------------

func TestSupplyFillOrderFreshThenRemailThenEO(t *testing.T) {
	f := supNewFixture(t)
	f.inv.RemailEnabled = true
	f.inv.MaxRemailShare = 0.25 // 0.25 x 9,000 demand = 2,250 allowance

	f.supSeedReady(t, "aol", 348, "rd-")                                // fresh_mailable
	f.supSeedHeld(t, "aol", 500, "now() - interval '2 days'", "topup-") // free top-up
	f.supSeedRemailable(t, "aol", 300, "rm-")                           // resurrection pool
	f.supSeedHeld(t, "aol", 20000, "NULL", "eo-")                       // EO stock
	f.supSeedYield(t, "aol", 10000, 7000)

	// projected includes the remail CREDIT, so recompute need with it.
	rate := 9000.0 / 19.0
	safety := int(math.Ceil(rate * SeedEOTurnaroundHours))
	through := int(math.Ceil(7000 * SeedEOTurnaroundHours / 10.0))
	wantNeed := through + safety - (348 + 300) // fresh + remail credit (300 eligible < 2,250 allowance)

	d := supDecision(t, f.run(t, f.controller(t, ModeOn)), "aol")
	if d.Need != wantNeed {
		t.Fatalf("need = %d, want %d (projected carries the remail credit)", d.Need, wantNeed)
	}
	if d.Promoted != 500 {
		t.Errorf("promoted = %d, want the whole 500-row validated held pool first", d.Promoted)
	}
	if d.Remailed != 300 {
		t.Errorf("remailed = %d, want the whole 300-row eligible pool second", d.Remailed)
	}
	wantOrder := int(math.Ceil(float64(wantNeed-500-300) / 0.70))
	if d.Ordered != wantOrder {
		t.Errorf("ordered = %d, want ceil((%d-500-300)/0.70) = %d — EO must cover only the remainder", d.Ordered, wantNeed, wantOrder)
	}

	// The queue proves each leg actually ran:
	if n := supCount(t, f.db, `SELECT COUNT(*) FROM partner_clean_queue WHERE status='ready' AND email LIKE 'topup-%'`); n != 500 {
		t.Errorf("top-up rows now ready = %d, want 500", n)
	}
	if n := supCount(t, f.db, `SELECT COUNT(*) FROM partner_clean_queue
		WHERE email LIKE 'rm-%' AND status='ready' AND mailed_at IS NULL AND touch_count = 0
		  AND (extra_metadata->>'remail_cycles')::int = 1`); n != 300 {
		t.Errorf("resurrected rows = %d, want 300 at touch 0 with remail_cycles=1", n)
	}
	if n := supCount(t, f.db, `SELECT COUNT(*) FROM partner_clean_queue WHERE status='pending_eo'`); n != wantOrder {
		t.Errorf("pending_eo = %d, want %d", n, wantOrder)
	}

	// Ledger: one row per leg, with the §1.2 event vocabulary.
	for event, want := range map[string]int{
		SupplyEventMailable:          500,
		SupplyEventRemailEligible:    300,
		SupplyEventValidationOrdered: wantOrder,
	} {
		var got int
		if err := f.db.QueryRow(`SELECT COALESCE(SUM(quantity),0) FROM drip_supply_ledger
			WHERE event=$1 AND reason IS NOT NULL AND reason <> ''`, event).Scan(&got); err != nil {
			t.Fatalf("ledger sum %s: %v", event, err)
		}
		if got != want {
			t.Errorf("ledger %s quantity = %d, want %d", event, got, want)
		}
	}
	// Contract versions are recorded so a decision is re-derivable.
	var invVer, srcVer sql.NullInt64
	if err := f.db.QueryRow(`SELECT inventory_contract_version, source_contract_version
		FROM drip_supply_ledger WHERE event='VALIDATION_ORDERED' AND reason LIKE 'shortfall=%'`).Scan(&invVer, &srcVer); err != nil {
		t.Fatalf("read versions: %v", err)
	}
	if invVer.Int64 != 7 || srcVer.Int64 != 2 {
		t.Errorf("versions = inv %v / src %v, want 7 / 2", invVer, srcVer)
	}
}

// TestSupplyRemailNeverResurrectsAClicker is the negative control on the
// resurrection predicate: an ever-clicker EXITS the ladder by standing ruling
// and must never come back as a cold intro.
func TestSupplyRemailNeverResurrectsAClicker(t *testing.T) {
	f := supNewFixture(t)
	f.inv.RemailEnabled = true
	f.supSeedReady(t, "aol", 348, "rd-")
	f.supSeedRemailable(t, "aol", 200, "rm-")
	f.supSeedHeld(t, "aol", 20000, "NULL", "eo-")
	f.supSeedYield(t, "aol", 10000, 7000)
	// Half the pool clicked; half exited on `clicked_exit`.
	supExec(t, f.db, `UPDATE partner_clean_queue SET last_click_at = now() - interval '20 days'
		WHERE email LIKE 'rm-%' AND email < 'rm-6'`)
	supExec(t, f.db, `UPDATE partner_clean_queue SET terminal_reason = 'clicked_exit'
		WHERE email LIKE 'rm-%' AND last_click_at IS NULL AND email < 'rm-8'`)

	before := supCount(t, f.db, `SELECT COUNT(*) FROM partner_clean_queue
		WHERE email LIKE 'rm-%' AND (last_click_at IS NOT NULL OR terminal_reason='clicked_exit')`)
	if before == 0 {
		t.Fatal("fixture broken: no clicker rows to protect")
	}
	f.run(t, f.controller(t, ModeOn))
	after := supCount(t, f.db, `SELECT COUNT(*) FROM partner_clean_queue
		WHERE email LIKE 'rm-%' AND (last_click_at IS NOT NULL OR terminal_reason='clicked_exit') AND status='mailed'`)
	if after != before {
		t.Errorf("%d of %d clicker rows were resurrected, want 0", before-after, before)
	}
}

// -----------------------------------------------------------------------------
// Modes
// -----------------------------------------------------------------------------

func TestSupplyShadowModeWritesOnlyTheShadowLedger(t *testing.T) {
	f := supNewFixture(t)
	f.supSeedHeld(t, "aol", 20000, "NULL", "eo-")
	f.supSeedHeld(t, "aol", 500, "now() - interval '2 days'", "topup-")
	f.supSeedReady(t, "aol", 348, "rd-")
	f.supSeedYield(t, "aol", 10000, 7000)
	heldBefore := supCount(t, f.db, `SELECT COUNT(*) FROM partner_clean_queue WHERE status='held'`)

	run := f.run(t, f.controller(t, ModeShadow))
	d := supDecision(t, run, "aol")
	if d.Enforced {
		t.Fatal("shadow decision reported Enforced=true")
	}
	if d.Ordered <= 0 {
		t.Fatalf("shadow computed no order (%+v) — a shadow that computes nothing reconciles nothing", d)
	}

	// The queue is untouched.
	if n := supCount(t, f.db, `SELECT COUNT(*) FROM partner_clean_queue WHERE status='held'`); n != heldBefore {
		t.Errorf("held rows = %d, want %d — shadow mode moved records", n, heldBefore)
	}
	if n := supCount(t, f.db, `SELECT COUNT(*) FROM partner_clean_queue WHERE status='pending_eo'`); n != 0 {
		t.Errorf("%d rows in pending_eo, want 0 in shadow mode", n)
	}
	// The live ledger carries only the yield fixture; the shadow carries the pass.
	if n := supCount(t, f.db, `SELECT COUNT(*) FROM drip_supply_ledger WHERE reason IS NOT NULL AND reason <> ''`); n != 0 {
		t.Errorf("%d controller rows in the LIVE supply ledger, want 0 in shadow mode", n)
	}
	if n := supCount(t, f.db, `SELECT COUNT(*) FROM drip_supply_ledger_shadow WHERE event='VALIDATION_ORDERED'`); n != 1 {
		t.Errorf("%d VALIDATION_ORDERED rows in the shadow ledger, want 1", n)
	}
	// Tick outcomes are written in EVERY mode: that is the heartbeat.
	if n := supCount(t, f.db, `SELECT COUNT(*) FROM drip_tick_outcomes WHERE pass='supply' AND lane=$1`, supLane); n != 1 {
		t.Errorf("%d supply tick outcomes, want 1 even in shadow mode", n)
	}
}

func TestSupplyModeOffTouchesNothing(t *testing.T) {
	f := supNewFixture(t)
	f.supSeedHeld(t, "aol", 5000, "NULL", "eo-")
	f.supSeedReady(t, "aol", 348, "rd-")

	run, err := f.controller(t, ModeOff).RunOnce(context.Background(), f.db, f.now)
	if err != nil {
		t.Fatalf("RunOnce(off): %v", err)
	}
	if len(run.Decisions) != 0 || run.Ordered != 0 {
		t.Fatalf("mode=off produced %d decisions / %d orders, want 0 / 0", len(run.Decisions), run.Ordered)
	}
	for _, tbl := range []string{"drip_supply_ledger", "drip_supply_ledger_shadow", "drip_tick_outcomes"} {
		if n := supCount(t, f.db, `SELECT COUNT(*) FROM `+tbl); n != 0 {
			t.Errorf("%s has %d rows after a mode=off pass, want 0", tbl, n)
		}
	}
}

func TestSupplyCanaryEnforcesOnlyTheNamedCell(t *testing.T) {
	f := supNewFixture(t)
	f.supSeedHeld(t, "aol", 20000, "NULL", "eo-aol-")
	f.supSeedReady(t, "aol", 348, "rd-aol-")
	f.supSeedYield(t, "aol", 10000, 7000)

	// A canary naming a DIFFERENT lane leaves this cell shadowed.
	run := f.run(t, f.controller(t, ModeCanary, func(c *SupplyControllerConfig) {
		c.Canary = []CanaryCell{{Domain: "*", ISP: "aol", Lane: "some_other_lane"}}
	}))
	if d := supDecision(t, run, "aol"); d.Enforced {
		t.Fatal("cell enforced under a canary that does not name it")
	}
	if n := supCount(t, f.db, `SELECT COUNT(*) FROM partner_clean_queue WHERE status='pending_eo'`); n != 0 {
		t.Fatalf("%d rows moved for an unnamed canary cell, want 0", n)
	}

	// NEGATIVE CONTROL: naming the cell enforces it.
	f2 := supNewFixture(t)
	f2.supSeedHeld(t, "aol", 20000, "NULL", "eo-aol-")
	f2.supSeedReady(t, "aol", 348, "rd-aol-")
	f2.supSeedYield(t, "aol", 10000, 7000)
	run2 := f2.run(t, f2.controller(t, ModeCanary, func(c *SupplyControllerConfig) {
		c.Canary = []CanaryCell{{Domain: supDomain, ISP: "aol", Lane: supLane}}
	}))
	d2 := supDecision(t, run2, "aol")
	if !d2.Enforced || d2.Ordered <= 0 {
		t.Fatalf("named canary cell: enforced=%v ordered=%d, want true / > 0", d2.Enforced, d2.Ordered)
	}
}

// -----------------------------------------------------------------------------
// Coverage gates
// -----------------------------------------------------------------------------

func TestSupplyCoverageGates(t *testing.T) {
	// A cell with no provisional award and coverage at or above
	// min_coverage_hours is not evaluated at all (§2.6's entry condition).
	t.Run("covered_cell_is_not_evaluated", func(t *testing.T) {
		f := supNewFixture(t)
		f.plan.Rows = []PlanRow{{Day: f.day, Lane: supLane, ISP: "aol", SendingDomain: supDomain, AwardFirm: 9000}}
		// rate = 9000/19 = 473.68/h; 8h of coverage needs 3,790 fresh records.
		f.supSeedReady(t, "aol", 5000, "rd-")
		f.supSeedHeld(t, "aol", 20000, "NULL", "eo-")
		f.supSeedYield(t, "aol", 10000, 7000)

		d := supDecision(t, f.run(t, f.controller(t, ModeOn)), "aol")
		if d.Ordered != 0 || d.Skip != SupplySkipCovered {
			t.Fatalf("ordered=%d skip=%q coverage=%.1fh, want 0 / %s", d.Ordered, d.Skip, d.CoverageHours, SupplySkipCovered)
		}
	})

	// NEGATIVE CONTROL: the SAME cell below min_coverage_hours is evaluated
	// and ordered, with no provisional award involved.
	t.Run("thin_coverage_orders_without_a_provisional_award", func(t *testing.T) {
		f := supNewFixture(t)
		f.plan.Rows = []PlanRow{{Day: f.day, Lane: supLane, ISP: "aol", SendingDomain: supDomain, AwardFirm: 9000}}
		f.supSeedReady(t, "aol", 100, "rd-")
		f.supSeedHeld(t, "aol", 20000, "NULL", "eo-")
		f.supSeedYield(t, "aol", 10000, 7000)

		d := supDecision(t, f.run(t, f.controller(t, ModeOn)), "aol")
		if d.Ordered <= 0 {
			t.Fatalf("ordered=%d skip=%q coverage=%.1fh, want an order below min_coverage_hours=%d",
				d.Ordered, d.Skip, d.CoverageHours, f.inv.MinCoverageHours)
		}
		if d.ProvisionalThroughHorizon != 0 {
			t.Errorf("provisional_through_horizon = %d, want 0 (no provisional award)", d.ProvisionalThroughHorizon)
		}
	})

	// max_coverage_hours caps the FILL, not only the entry.
	t.Run("max_coverage_caps_the_order", func(t *testing.T) {
		f := supNewFixture(t)
		f.inv.MaxCoverageHours = 4 // below the 8h min: entry is on the provisional award
		f.supSeedReady(t, "aol", 100, "rd-")
		f.supSeedHeld(t, "aol", 40000, "NULL", "eo-")
		f.supSeedYield(t, "aol", 10000, 7000)

		d := supDecision(t, f.run(t, f.controller(t, ModeOn)), "aol")
		rate := 9000.0 / 19.0
		room := int(math.Floor(4*rate)) - 100
		if room >= d.Shortfall {
			t.Fatalf("fixture broken: room %d >= shortfall %d, so max_coverage cannot be the binding term", room, d.Shortfall)
		}
		if d.Need != room {
			t.Fatalf("need = %d, want the max-coverage room %d (shortfall was %d)", d.Need, room, d.Shortfall)
		}
		if want := int(math.Ceil(float64(room) / 0.70)); d.Ordered != want {
			t.Errorf("ordered = %d, want %d", d.Ordered, want)
		}
	})
}

// -----------------------------------------------------------------------------
// EO disabled
// -----------------------------------------------------------------------------

func TestSupplyEODisabledStillPromotesButNeverOrders(t *testing.T) {
	f := supNewFixture(t)
	f.inv.EOEnabled = false
	f.supSeedReady(t, "aol", 348, "rd-")
	f.supSeedHeld(t, "aol", 500, "now() - interval '2 days'", "topup-")
	f.supSeedHeld(t, "aol", 20000, "NULL", "eo-")
	f.supSeedYield(t, "aol", 10000, 7000)

	d := supDecision(t, f.run(t, f.controller(t, ModeOn)), "aol")
	if d.Promoted != 500 {
		t.Errorf("promoted = %d, want 500 — a free top-up does not need eo_enabled", d.Promoted)
	}
	if d.Ordered != 0 || d.Skip != SupplySkipEODisabled {
		t.Errorf("ordered=%d skip=%q, want 0 / %s", d.Ordered, d.Skip, SupplySkipEODisabled)
	}
}

// -----------------------------------------------------------------------------
// Worker plumbing
// -----------------------------------------------------------------------------

func TestSupplyWorkerKillSwitchAndTrigger(t *testing.T) {
	t.Setenv("DRIP_SUPPLY_CONTROLLER_DISABLED", "1")
	w := NewSupplyWorker(nil, nil, SupplyControllerConfig{Mode: ModeOn, ContractSource: func(context.Context, time.Time) (*ActiveSet, error) { return nil, nil }})
	if !w.Disabled() {
		t.Fatal("DRIP_SUPPLY_CONTROLLER_DISABLED=1 did not disable the worker")
	}
	// Start must return immediately rather than block on a ticker.
	done := make(chan struct{})
	go func() { w.Start(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return with the kill switch set")
	}
	// Trigger on a disabled worker must not block or panic.
	for i := 0; i < 200; i++ {
		w.Trigger("lane")
	}
}

func TestSupplyWorkerTriggerIsLossyNotBlocking(t *testing.T) {
	w := NewSupplyWorker(nil, nil, SupplyControllerConfig{Mode: ModeShadow, ContractSource: func(context.Context, time.Time) (*ActiveSet, error) { return nil, nil }})
	done := make(chan struct{})
	go func() {
		// Far more triggers than the channel holds: a blocking Trigger would
		// stall whatever ledger writer called it.
		for i := 0; i < 10000; i++ {
			w.Trigger("lane")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Trigger blocked when its channel was full")
	}
}
