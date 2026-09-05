package dripsupply

// mediator_e2e_test.go — REQ-118, the COMPOSED path at DRIP_SUPPLY_CHAIN_MODE=on.
//
// Every layer of this subsystem is unit-tested. Nothing tested the SEQUENCE:
//
//	real contract rows -> TickStart (AnyDue -> ActivateScheduled -> LoadActiveWithKey
//	-> EnsureDayBalances -> ExpireStale) -> Grant (RefillDomain + governors + Reserve)
//	-> ClaimByISPCaps against real partner_clean_queue rows -> Commit/Release
//	-> drip_capacity_ledger + drip_capacity_balance + drip_tick_outcomes
//
// Four individually-correct units in sequence is not the same as a correct
// sequence, and mode=on went live in production (taskdef 1091) with that gap open.
//
// HARNESS: reservation_test.go's, unchanged — newTestDB (scratch schema in the
// local apex-postgres `req118_res`, dropped at the end), seedDay's contract
// builders, readBalance/readLedger/setBalance — plus executor_test.go's
// executorSchemaDDL (drip_tick_outcomes + the shadow ledger) and
// transition_test.go's pcqSchemaDDL/mkDataset. Nothing here can reach
// production: the DSN is hard-defaulted to localhost by adminDSN().
//
// These tests DO NOT skip. If Postgres is unreachable they fail, because a
// silently-skipped integration test on the send path is how a green suite hides
// an untested cutover. That is a deliberate departure from the sibling files,
// and requireLocalPG below is where it is enforced.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// -----------------------------------------------------------------------------
// Harness
// -----------------------------------------------------------------------------

// e2eContractKey is the §1.5 HMAC key these tests issue and verify tokens with.
// contractmeta requires >= 32 bytes.
var e2eContractKey = []byte("req118-e2e-contract-token-key-0123456789")

// e2eIssuedAt is the fixed token issue time, so a token is reproducible.
var e2eIssuedAt = time.Date(2026, 9, 3, 6, 0, 0, 0, time.UTC)

// requireLocalPG fails (never skips) when the local scratch database cannot be
// reached. The whole point of this file is that the composed path RUNS.
func requireLocalPG(t *testing.T) {
	t.Helper()
	db, err := sql.Open("postgres", adminDSN())
	if err != nil {
		t.Fatalf("REQ-118 e2e needs local Postgres and cannot open %s: %v", testAdminDSNEnv, err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("REQ-118 e2e needs local Postgres (apex-postgres on localhost:5432) and it is unreachable: %v\n"+
			"These tests must not skip: a skipped composed-path test is how mode=on shipped untested. "+
			"Start apex-postgres or set %s.", err, testAdminDSNEnv)
	}
}

// The four contract tables. VERBATIM from cmd/server/main.go's
// req118_create_drip_*_contracts entries (main.go:3116 onward), the same source
// contracts_test.go's copy mirrors. main.go is `package main` and cannot be
// imported; contracts_test.go is in the EXTERNAL test package and its constants
// are not visible here. A WP1 drift therefore surfaces as a failure of these
// tests rather than as a 3am NOT NULL violation.
//
// All four are needed even though only two kinds are seeded: AnyDue and
// ActivateScheduled walk every kind in one statement/transaction.
func e2eContractTableDDL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS drip_domain_contracts (
			id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			sending_domain      TEXT NOT NULL,
			brand_code          TEXT NOT NULL DEFAULT '',
			version             INT  NOT NULL DEFAULT 1,
			status              TEXT NOT NULL DEFAULT 'draft'
				CHECK (status IN ('draft','approved','scheduled','active','superseded')),
			effective_at        TIMESTAMPTZ NOT NULL,
			superseded_at       TIMESTAMPTZ,
			created_by          TEXT NOT NULL DEFAULT '',
			created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			approved_by         TEXT,
			approved_at         TIMESTAMPTZ,
			change_ledger_id    TEXT NOT NULL DEFAULT '',
			notes               TEXT NOT NULL DEFAULT '',
			metadata            JSONB NOT NULL DEFAULT '{}'::jsonb,
			token               TEXT  NOT NULL DEFAULT '',
			daily_max_by_isp    JSONB NOT NULL,
			active_window_start TIME NOT NULL DEFAULT '01:00',
			active_window_end   TIME NOT NULL DEFAULT '20:00',
			interval_minutes    INT  NOT NULL DEFAULT 15,
			max_burst_intervals INT  NOT NULL DEFAULT 2,
			ramp_source         TEXT,
			health_band         TEXT NOT NULL DEFAULT 'green'
				CHECK (health_band IN ('green','amber','red')),
			ramp_stage          TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS drip_dispatch_contracts (
			id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			lane                   TEXT NOT NULL,
			version                INT  NOT NULL DEFAULT 1,
			status                 TEXT NOT NULL DEFAULT 'draft'
				CHECK (status IN ('draft','approved','scheduled','active','superseded')),
			effective_at           TIMESTAMPTZ NOT NULL,
			superseded_at          TIMESTAMPTZ,
			created_by             TEXT NOT NULL DEFAULT '',
			created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			approved_by            TEXT,
			approved_at            TIMESTAMPTZ,
			change_ledger_id       TEXT NOT NULL DEFAULT '',
			notes                  TEXT NOT NULL DEFAULT '',
			metadata               JSONB NOT NULL DEFAULT '{}'::jsonb,
			token                  TEXT  NOT NULL DEFAULT '',
			operator_priority_tier INT  NOT NULL DEFAULT 2,
			desired_daily_intros   JSONB NOT NULL,
			demand_mode            TEXT NOT NULL DEFAULT 'target'
				CHECK (demand_mode IN ('target','consume_available')),
			daily_ceiling          INT,
			allowed_domains        TEXT[] NOT NULL,
			isp_exclusions         TEXT[] NOT NULL DEFAULT '{}',
			ladder_touches         INT  NOT NULL DEFAULT 5,
			ladder_gap_hours       INT  NOT NULL DEFAULT 24,
			followups_committed    BOOLEAN NOT NULL DEFAULT TRUE,
			max_intro_share        NUMERIC NOT NULL DEFAULT 0.40,
			exploration_share      NUMERIC NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS drip_inventory_contracts (
			id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			lane                   TEXT NOT NULL,
			version                INT  NOT NULL DEFAULT 1,
			status                 TEXT NOT NULL DEFAULT 'draft'
				CHECK (status IN ('draft','approved','scheduled','active','superseded')),
			effective_at           TIMESTAMPTZ NOT NULL,
			superseded_at          TIMESTAMPTZ,
			created_by             TEXT NOT NULL DEFAULT '',
			created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			approved_by            TEXT,
			approved_at            TIMESTAMPTZ,
			change_ledger_id       TEXT NOT NULL DEFAULT '',
			notes                  TEXT NOT NULL DEFAULT '',
			metadata               JSONB NOT NULL DEFAULT '{}'::jsonb,
			token                  TEXT  NOT NULL DEFAULT '',
			accepted_sources       TEXT[] NOT NULL,
			verdict_valid_days     INT  NOT NULL DEFAULT 60,
			eo_enabled             BOOLEAN NOT NULL DEFAULT TRUE,
			max_daily_eo_spend_usd NUMERIC NOT NULL DEFAULT 50,
			min_eo_order           INT  NOT NULL DEFAULT 1000,
			min_coverage_hours     INT  NOT NULL DEFAULT 8,
			target_coverage_hours  INT  NOT NULL DEFAULT 16,
			max_coverage_hours     INT  NOT NULL DEFAULT 36,
			remail_enabled         BOOLEAN NOT NULL DEFAULT FALSE,
			remail_after_days      INT  NOT NULL DEFAULT 7,
			remail_mode            TEXT NOT NULL DEFAULT 'full_ladder'
				CHECK (remail_mode IN ('full_ladder','single_touch')),
			max_remail_share       NUMERIC NOT NULL DEFAULT 0.25
		)`,
		`CREATE TABLE IF NOT EXISTS drip_source_contracts (
			id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			source_slug           TEXT NOT NULL,
			version               INT  NOT NULL DEFAULT 1,
			status                TEXT NOT NULL DEFAULT 'draft'
				CHECK (status IN ('draft','approved','scheduled','active','superseded')),
			effective_at          TIMESTAMPTZ NOT NULL,
			superseded_at         TIMESTAMPTZ,
			created_by            TEXT NOT NULL DEFAULT '',
			created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			approved_by           TEXT,
			approved_at           TIMESTAMPTZ,
			change_ledger_id      TEXT NOT NULL DEFAULT '',
			notes                 TEXT NOT NULL DEFAULT '',
			metadata              JSONB NOT NULL DEFAULT '{}'::jsonb,
			token                 TEXT  NOT NULL DEFAULT '',
			record_class          TEXT NOT NULL,
			eligible_isps         TEXT[] NOT NULL,
			max_daily_intake      INT,
			arrival_cadence       TEXT NOT NULL DEFAULT 'continuous',
			validated_on_arrival  BOOLEAN NOT NULL DEFAULT FALSE,
			record_max_age_days   INT,
			unit_acquisition_cost NUMERIC NOT NULL DEFAULT 0
		)`,
	}
}

// newE2EDB is one scratch schema carrying EVERY table the composed path touches:
// the WP3 balances/ledger (newTestDB), WP5's outcome + shadow tables, the four
// WP2 contract tables with their partial unique indexes, and partner_clean_queue
// with the REQ-118 CHECK at a PAST fence so the constraint is actually enforced.
func newE2EDB(t *testing.T) *sql.DB {
	t.Helper()
	requireLocalPG(t)
	db := newTestDB(t)

	stmts := executorSchemaDDL()
	stmts = append(stmts, e2eContractTableDDL()...)
	for _, kind := range AllKinds() {
		ddl, err := ActiveIndexDDL(kind)
		if err != nil {
			t.Fatalf("ActiveIndexDDL(%s): %v", kind, err)
		}
		stmts = append(stmts, ddl)
	}
	stmts = append(stmts, pcqSchemaDDL(pastFence)...)
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("e2e ddl %.70s: %v", strings.ReplaceAll(stmt, "\n", " "), err)
		}
	}
	return db
}

// e2eISPMap is a full, valid daily_max_by_isp: every canonical class present
// (Validate requires it), everything 0 except the named ISP, gmail held at 0 by
// the standing ruling so no `notes` waiver is needed.
func e2eISPMap(isp string, max int) map[string]int {
	m := map[string]int{}
	for _, c := range ISPClasses() {
		m[c] = 0
	}
	m[normISP(isp)] = max
	m["gmail"] = 0
	return m
}

// e2eDomain builds a DomainContract that passes Validate, with the window knobs
// each scenario needs to pin one term of the min().
func e2eDomain(domain, brand, isp string, max int, winStart, winEnd string, intervalMin, burst int) *DomainContract {
	return &DomainContract{
		SendingDomain:     domain,
		BrandCode:         brand,
		DailyMaxByISP:     e2eISPMap(isp, max),
		ActiveWindowStart: winStart,
		ActiveWindowEnd:   winEnd,
		IntervalMinutes:   intervalMin,
		MaxBurstIntervals: burst,
		RampSource:        RampSourceCards,
		HealthBand:        HealthBandGreen,
		RampStage:         "mature",
		Meta:              Meta{CreatedBy: "req118-e2e", ChangeLedgerID: "chg-e2e"},
	}
}

func e2eDispatch(lane, isp string, desired int, allowedBrands ...string) *DispatchContract {
	if len(allowedBrands) == 0 {
		allowedBrands = []string{"ht"}
	}
	return &DispatchContract{
		Lane:                 lane,
		OperatorPriorityTier: 2,
		DesiredDailyIntros:   map[string]int{normISP(isp): desired},
		DemandMode:           DemandModeTarget,
		AllowedDomains:       allowedBrands,
		ISPExclusions:        []string{"gmail"},
		LadderTouches:        5,
		LadderGapHours:       24,
		FollowupsCommitted:   true,
		MaxIntroShare:        0.40,
		ExplorationShare:     0,
		Meta:                 Meta{CreatedBy: "req118-e2e", ChangeLedgerID: "chg-e2e"},
	}
}

// e2eStage runs the REAL operator lifecycle for one contract:
// InsertDraft -> approved -> Schedule (issues the §1.5 token). It deliberately
// STOPS at `scheduled`: TickStart's AnyDue/ActivateScheduled is the step under
// test, so nothing here activates.
func e2eStage(t *testing.T, db *sql.DB, c Contract, effective time.Time) {
	t.Helper()
	ctx := context.Background()
	switch v := c.(type) {
	case *DomainContract:
		v.EffectiveAt = effective
	case *DispatchContract:
		v.EffectiveAt = effective
	default:
		t.Fatalf("e2eStage does not know how to stamp %T", c)
	}
	id, version, err := InsertDraft(ctx, db, c)
	if err != nil {
		t.Fatalf("InsertDraft(%s/%s): %v", c.Kind(), c.Subject(), err)
	}
	table, err := TableFor(c.Kind())
	if err != nil {
		t.Fatalf("TableFor(%s): %v", c.Kind(), err)
	}
	if _, err := db.Exec(
		`UPDATE `+table+` SET status='approved', approved_by='operator', approved_at=NOW() WHERE id=$1`, id); err != nil {
		t.Fatalf("approve %s/%s: %v", c.Kind(), c.Subject(), err)
	}
	if _, err := Schedule(ctx, db, c.Kind(), c.Subject(), version, e2eContractKey, e2eIssuedAt); err != nil {
		t.Fatalf("Schedule(%s/%s): %v", c.Kind(), c.Subject(), err)
	}
}

// e2eFixture is the composed system: real contract rows, a Service, and a
// Mediator whose ContractSource is NIL — so every tick really does run
// AnyDue -> ActivateScheduled -> LoadActiveWithKey against the tables.
type e2eFixture struct {
	db      *sql.DB
	svc     *Service
	med     *Mediator
	day     time.Time
	clock   time.Time
	dataset uuid.UUID
	lane    string
	domain  string
	brand   string
}

type e2eOpts struct {
	Mode      Mode
	Governors GovernorReader
	Domains   []*DomainContract
	Dispatch  *DispatchContract
	// ClockAt is the offset into the Denver day the first tick runs at.
	ClockAt time.Duration
}

func newE2EFixture(t *testing.T, o e2eOpts) *e2eFixture {
	t.Helper()
	db := newE2EDB(t)
	day := testDay(t)
	if len(o.Domains) == 0 {
		t.Fatal("newE2EFixture needs at least one domain contract")
	}
	if o.Dispatch == nil {
		t.Fatal("newE2EFixture needs a dispatch contract")
	}

	f := &e2eFixture{
		db:     db,
		day:    day,
		clock:  day.Add(o.ClockAt),
		lane:   o.Dispatch.Lane,
		domain: o.Domains[0].SendingDomain,
		brand:  o.Domains[0].BrandCode,
	}
	// Effective at the Denver day's midnight: due by every tick in the day,
	// and inside LoadActive's `effective_at < dayEnd` window.
	effective := dayOf(day)
	for _, dc := range o.Domains {
		e2eStage(t, db, dc, effective)
	}
	e2eStage(t, db, o.Dispatch, effective)

	opts := []Option{WithClock(func() time.Time { return f.clock })}
	if o.Governors != nil {
		opts = append(opts, WithGovernors(o.Governors))
	}
	f.svc = NewService(db, opts...)
	f.med = NewMediator(db, f.svc, MediatorConfig{
		Mode:           o.Mode,
		Clock:          func() time.Time { return f.clock },
		AlertsDisabled: true,
		// The plan_share term is planner_test.go's subject. Leaving the planner
		// wired here would only log a missing-table error and bind nothing.
		PlannerDisabled: true,
		ContractKey:     e2eContractKey,
		// ContractSource stays NIL on purpose: that is what makes this the
		// composed path rather than executor_test.go's injected ActiveSet.
	})
	f.dataset = mkDataset(t, db, false)
	return f
}

// tick advances the clock and runs the real tick preamble.
func (f *e2eFixture) tick(t *testing.T, at time.Duration) {
	t.Helper()
	f.clock = f.day.Add(at)
	f.med.TickStart(context.Background(), f.clock)
}

// grant mirrors PartnerDripOrchestrator.grantWaveCapacity's call shape
// (internal/worker/partner_drip_orchestrator.go:5867).
func (f *e2eFixture) grant(t *testing.T, waveKey, touchClass string, requested int, isps ...string) *Allocation {
	t.Helper()
	a, err := f.med.Grant(context.Background(), GrantReq{
		Day:        f.day,
		Lane:       f.lane,
		Brand:      f.brand,
		Domain:     f.domain,
		TouchClass: touchClass,
		Pass:       PassWelcome,
		WaveKey:    waveKey,
		ISPs:       isps,
		Requested:  requested,
	})
	if err != nil {
		t.Fatalf("Grant(%s): %v", waveKey, err)
	}
	return a
}

// claim mirrors claimWaveByCaps (partner_drip_orchestrator.go:5959): the
// reservation's caps ARE the claim caps, and the allocation id is stamped on
// every row it moves.
func (f *e2eFixture) claim(t *testing.T, a *Allocation, hardCap int) []ClaimedRecord {
	t.Helper()
	got, err := NewTransitions().ClaimByISPCaps(context.Background(), f.db,
		f.lane, f.brand, a.EnforcedCaps(), hardCap, a.AllocationID())
	if err != nil {
		t.Fatalf("ClaimByISPCaps: %v", err)
	}
	return got
}

// seedReady bulk-inserts `n` mailable partner_clean_queue rows for one ISP,
// oldest first, so the claim's ORDER BY ingested_at is meaningful.
func (f *e2eFixture) seedReady(t *testing.T, isp string, n int) {
	t.Helper()
	if _, err := f.db.Exec(`
		INSERT INTO partner_clean_queue
			(id, batch_id, dataset_id, partner_id, vertical, email, email_md5, isp_family, status, ingested_at)
		SELECT gen_random_uuid(), $1, $2, $3, $4,
		       'e2e-' || g || '@example.com', md5('e2e-' || g), $5, 'ready',
		       NOW() - make_interval(secs => ($6 - g)::double precision)
		FROM generate_series(1, $6) g
	`, uuid.New(), f.dataset, uuid.New(), f.lane, normISP(isp), n); err != nil {
		t.Fatalf("seed %d ready rows: %v", n, err)
	}
}

func (f *e2eFixture) countPCQ(t *testing.T, status string) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM partner_clean_queue WHERE status = $1`, status).Scan(&n); err != nil {
		t.Fatalf("count pcq %s: %v", status, err)
	}
	return n
}

// outcome mirrors PartnerDripOrchestrator.tickOutcome (:5647).
func (f *e2eFixture) outcome(t *testing.T, out, reason string, caps map[string]int, claimed int) {
	t.Helper()
	f.med.Outcome(context.Background(), OutcomeRow{
		Lane: f.lane, Pass: PassWelcome, Outcome: out, Reason: reason,
		CapsSeen: caps, Claimed: claimed, Brand: f.brand,
	})
}

type outcomeRowRead struct {
	Outcome, Reason string
	Claimed         int
}

func (f *e2eFixture) readOutcomes(t *testing.T) []outcomeRowRead {
	t.Helper()
	rows, err := f.db.Query(
		`SELECT outcome, reason, claimed FROM drip_tick_outcomes WHERE lane = $1 ORDER BY tick`, f.lane)
	if err != nil {
		t.Fatalf("read tick outcomes: %v", err)
	}
	defer rows.Close()
	var out []outcomeRowRead
	for rows.Next() {
		var r outcomeRowRead
		if err := rows.Scan(&r.Outcome, &r.Reason, &r.Claimed); err != nil {
			t.Fatalf("scan tick outcome: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// assertConserved is the invariant the whole subsystem exists to hold: for a
// day×domain×ISP, every unit the ledger granted is accounted for exactly once
// on the balance, and the balance never carries a unit the ledger did not grant.
// No capacity lost, none conjured.
func assertConserved(t *testing.T, db *sql.DB, day time.Time, domain, isp string) {
	t.Helper()
	var lReserved, lCommitted, lReleased int
	if err := db.QueryRow(`
		SELECT COALESCE(SUM(reserved),0), COALESCE(SUM(committed),0), COALESCE(SUM(released),0)
		FROM drip_capacity_ledger
		WHERE day = $1::date AND sending_domain = $2 AND isp = $3
	`, dayKey(day), domain, isp).Scan(&lReserved, &lCommitted, &lReleased); err != nil {
		t.Fatalf("sum ledger: %v", err)
	}
	b := readBalance(t, db, day, domain, isp)
	outstanding := lReserved - lCommitted - lReleased
	if outstanding < 0 {
		t.Fatalf("ledger conjured capacity: reserved=%d < committed=%d + released=%d", lReserved, lCommitted, lReleased)
	}
	if b.Reserved != outstanding {
		t.Fatalf("balance.reserved=%d but the ledger's outstanding is %d (reserved=%d committed=%d released=%d) — capacity was lost or double-counted",
			b.Reserved, outstanding, lReserved, lCommitted, lReleased)
	}
	if b.Committed != lCommitted {
		t.Fatalf("balance.committed=%d, ledger committed=%d", b.Committed, lCommitted)
	}
	if b.Released != lReleased {
		t.Fatalf("balance.released=%d, ledger released=%d", b.Released, lReleased)
	}
}

// standardDomain: 01:00-20:00, 15-minute intervals (76 in the window), burst 2.
// aol 7,600/day => 100 tokens per interval, a 200-token burst ceiling.
func standardDomain(max int) *DomainContract {
	return e2eDomain("em.historythinking.com", "ht", "aol", max, "01:00", "20:00", 15, 2)
}

// e2eStaticGovernor is a GovernorReader with a fixed ceiling for one ISP.
type e2eStaticGovernor struct {
	isp     string
	name    string
	ceiling int
}

func (g e2eStaticGovernor) Ceilings(_ context.Context, _ time.Time, _, isp string, _ Window) ([]GovernorCeiling, error) {
	if normISP(isp) != normISP(g.isp) {
		return nil, nil
	}
	return []GovernorCeiling{{Name: g.name, Limit: g.ceiling}}, nil
}

// -----------------------------------------------------------------------------
// Scenario 1 — the happy path, composed
// -----------------------------------------------------------------------------

// Real contract rows -> TickStart activates and verifies them -> Grant ->
// ClaimByISPCaps -> Commit. The ledger and the balance must agree at the end.
func TestE2E_HappyPath_ContractsToCommitConserveCapacity(t *testing.T) {
	f := newE2EFixture(t, e2eOpts{
		Mode:     ModeOn,
		Domains:  []*DomainContract{standardDomain(7600)},
		Dispatch: e2eDispatch("wcl_remail", "aol", 100000),
		ClockAt:  10 * time.Hour,
	})
	f.seedReady(t, "aol", 200)

	// Contracts are `scheduled` until the tick activates them. Prove that is
	// the state the composed path starts from, not a pre-activated fixture.
	var status string
	if err := f.db.QueryRow(`SELECT status FROM drip_domain_contracts WHERE sending_domain = $1`, f.domain).Scan(&status); err != nil {
		t.Fatalf("read contract status: %v", err)
	}
	if status != string(StatusScheduled) {
		t.Fatalf("pre-tick contract status = %q, want scheduled — the fixture skipped the activation step under test", status)
	}

	f.tick(t, 10*time.Hour)

	if err := f.db.QueryRow(`SELECT status FROM drip_domain_contracts WHERE sending_domain = $1`, f.domain).Scan(&status); err != nil {
		t.Fatalf("read contract status: %v", err)
	}
	if status != string(StatusActive) {
		t.Fatalf("post-tick contract status = %q, want active — TickStart's AnyDue/ActivateScheduled did not run", status)
	}

	// TickStart seeded the day's balances from the VERIFIED contract.
	bal := readBalance(t, f.db, f.day, f.domain, "aol")
	if bal.Contracted != 7600 || bal.Effective != 7600 {
		t.Fatalf("seeded balance contracted=%d effective=%d, want 7600/7600", bal.Contracted, bal.Effective)
	}

	alloc := f.grant(t, "wave-1", TouchClassIntro, 150, "aol")
	if !alloc.Enforced {
		t.Fatalf("mode=on did not enforce: %+v", alloc)
	}
	if got := alloc.Caps["aol"]; got != 150 {
		t.Fatalf("granted %d, want 150 (200-token burst ceiling, 7,600 headroom, 100k lane demand)", got)
	}
	if alloc.Reason != ReasonRequested {
		t.Fatalf("binding reason = %q, want %q — nothing should have constrained a 150 ask", alloc.Reason, ReasonRequested)
	}

	claimed := f.claim(t, alloc, 150)
	if len(claimed) != 150 {
		t.Fatalf("claimed %d rows, want 150 — the grant is the claim cap", len(claimed))
	}
	for _, r := range claimed {
		if r.ISPFamily != "aol" {
			t.Fatalf("claimed a %s row under an aol-only grant", r.ISPFamily)
		}
	}
	if n := f.countPCQ(t, "claimed"); n != 150 {
		t.Fatalf("%d pcq rows claimed, want 150", n)
	}
	// Every claimed row carries the allocation — the pcq_claim_requires_allocation
	// CHECK is live at a PAST fence in this schema, so a claim without it would
	// have raised instead of reaching here.
	var unstamped int
	if err := f.db.QueryRow(
		`SELECT count(*) FROM partner_clean_queue WHERE status='claimed' AND capacity_allocation_id IS NULL`).Scan(&unstamped); err != nil {
		t.Fatalf("count unstamped: %v", err)
	}
	if unstamped != 0 {
		t.Fatalf("%d claimed rows carry no capacity_allocation_id", unstamped)
	}

	campaign := uuid.New()
	tally := map[string]int{"aol": len(claimed)}
	if err := alloc.Commit(context.Background(), SplitSubmitted(tally, 150), campaign); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	f.outcome(t, OutcomeFired, "", alloc.Caps, 150)

	row := readLedger(t, f.db, alloc.ID)
	if row.Reserved != 150 || row.Committed != 150 || row.Released != 0 {
		t.Fatalf("ledger reserved=%d committed=%d released=%d, want 150/150/0", row.Reserved, row.Committed, row.Released)
	}
	if row.Status != StatusCommitted {
		t.Fatalf("ledger status = %q, want %q", row.Status, StatusCommitted)
	}
	if row.CampaignID == nil || *row.CampaignID != campaign {
		t.Fatalf("ledger campaign_id = %v, want %s", row.CampaignID, campaign)
	}

	after := readBalance(t, f.db, f.day, f.domain, "aol")
	if after.Reserved != 0 || after.Committed != 150 || after.Released != 0 {
		t.Fatalf("balance reserved=%d committed=%d released=%d, want 0/150/0", after.Reserved, after.Committed, after.Released)
	}
	if after.Tokens != 50 {
		t.Fatalf("tokens = %v, want 50 (200 minted - 150 spent, nothing handed back)", after.Tokens)
	}
	assertConserved(t, f.db, f.day, f.domain, "aol")

	outs := f.readOutcomes(t)
	if len(outs) != 1 || outs[0].Outcome != OutcomeFired || outs[0].Claimed != 150 {
		t.Fatalf("tick outcomes = %+v, want one 'fired' row claiming 150", outs)
	}
}

// NEGATIVE CONTROL for scenario 1: the LoadActive step is load-bearing, not
// decoration. A wave for a domain with no ACTIVE contract must be skipped
// outright at mode=on — never granted from a default.
func TestE2E_NegativeControl_UnactivatedContractSkipsTheWave(t *testing.T) {
	f := newE2EFixture(t, e2eOpts{
		Mode:     ModeOn,
		Domains:  []*DomainContract{standardDomain(7600)},
		Dispatch: e2eDispatch("wcl_remail", "aol", 100000),
		ClockAt:  10 * time.Hour,
	})
	f.seedReady(t, "aol", 200)
	f.tick(t, 10*time.Hour)

	// A brand whose sending domain has no contract at all.
	a, err := f.med.Grant(context.Background(), GrantReq{
		Day: f.day, Lane: f.lane, Brand: "zz", Domain: "em.nosuchdomain.example",
		TouchClass: TouchClassIntro, Pass: PassWelcome, WaveKey: "wave-1",
		ISPs: []string{"aol"}, Requested: 150,
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if !a.ShouldSkip() {
		t.Fatal("a wave with no active domain contract was NOT skipped at mode=on — the composed path grants from thin air")
	}
	if a.SkipReason() != SkipNoContract {
		t.Fatalf("skip reason = %q, want %q", a.SkipReason(), SkipNoContract)
	}
	if a.EnforcedCaps() != nil {
		t.Fatalf("a skipped wave returned caps %v", a.EnforcedCaps())
	}
	if n := countRows(t, f.db, "drip_capacity_ledger"); n != 0 {
		t.Fatalf("%d ledger rows written for an uncontracted wave, want 0", n)
	}
	if n := f.countPCQ(t, "claimed"); n != 0 {
		t.Fatalf("%d pcq rows claimed for an uncontracted wave, want 0", n)
	}
}

// -----------------------------------------------------------------------------
// Scenario 2 — partial claim downstream of a real Grant
// -----------------------------------------------------------------------------

// A wave reserves N, the queue only yields N-k, and the k remainder must be
// RELEASED back to the day in the same pass — not stranded until ExpireStale.
func TestE2E_PartialClaim_RemainderReturnsToTheDay(t *testing.T) {
	f := newE2EFixture(t, e2eOpts{
		Mode:     ModeOn,
		Domains:  []*DomainContract{standardDomain(7600)},
		Dispatch: e2eDispatch("wcl_remail", "aol", 100000),
		ClockAt:  10 * time.Hour,
	})
	// Only 90 mailable records exist for a grant of 150: k = 60.
	f.seedReady(t, "aol", 90)
	f.tick(t, 10*time.Hour)

	alloc := f.grant(t, "wave-1", TouchClassIntro, 150, "aol")
	if alloc.Caps["aol"] != 150 {
		t.Fatalf("granted %d, want 150", alloc.Caps["aol"])
	}
	claimed := f.claim(t, alloc, 150)
	if len(claimed) != 90 {
		t.Fatalf("claimed %d rows, want 90 (that is all the supply there is)", len(claimed))
	}
	if err := alloc.Commit(context.Background(), SplitSubmitted(map[string]int{"aol": 90}, 90), uuid.New()); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	row := readLedger(t, f.db, alloc.ID)
	if row.Reserved != 150 || row.Committed != 90 || row.Released != 60 {
		t.Fatalf("ledger reserved=%d committed=%d released=%d, want 150/90/60", row.Reserved, row.Committed, row.Released)
	}
	if !row.ReleaseReason.Valid || row.ReleaseReason.String != "partial_commit" {
		t.Fatalf("release_reason = %v, want partial_commit", row.ReleaseReason)
	}
	if row.Reason != ReasonRequested {
		t.Fatalf("binding_reason = %q — a partial commit must not overwrite the record of why the grant was its size", row.Reason)
	}

	bal := readBalance(t, f.db, f.day, f.domain, "aol")
	if bal.Reserved != 0 || bal.Committed != 90 || bal.Released != 60 {
		t.Fatalf("balance reserved=%d committed=%d released=%d, want 0/90/60", bal.Reserved, bal.Committed, bal.Released)
	}
	// The day is whole: 60 units of the 150 went back as spendable tokens
	// (200 minted - 150 reserved + 60 returned = 110) and the day's headroom
	// only lost the 90 that actually shipped.
	if bal.Tokens != 110 {
		t.Fatalf("tokens = %v, want 110 — the k remainder did not return to the bucket", bal.Tokens)
	}
	if h := (Balance{Effective: bal.Effective, Reserved: bal.Reserved, Committed: bal.Committed}).Headroom(); h != 7510 {
		t.Fatalf("headroom = %d, want 7510 (7,600 - the 90 that shipped)", h)
	}
	assertConserved(t, f.db, f.day, f.domain, "aol")

	lane := readLane(t, f.db, f.day, f.lane, "aol")
	if lane.Committed != 90 {
		t.Fatalf("lane committed = %d, want 90", lane.Committed)
	}
	if lane.Unfilled != 100000-90 {
		t.Fatalf("lane unfilled = %d, want %d — the released 60 must return to lane demand too", lane.Unfilled, 100000-90)
	}
}

// NEGATIVE CONTROL for scenario 2: the 60 came from the SHORTFALL, not from an
// unconditional release. A wave that claims and ships everything releases zero.
func TestE2E_NegativeControl_FullClaimReleasesNothing(t *testing.T) {
	f := newE2EFixture(t, e2eOpts{
		Mode:     ModeOn,
		Domains:  []*DomainContract{standardDomain(7600)},
		Dispatch: e2eDispatch("wcl_remail", "aol", 100000),
		ClockAt:  10 * time.Hour,
	})
	f.seedReady(t, "aol", 150)
	f.tick(t, 10*time.Hour)

	alloc := f.grant(t, "wave-1", TouchClassIntro, 150, "aol")
	claimed := f.claim(t, alloc, 150)
	if len(claimed) != 150 {
		t.Fatalf("claimed %d, want 150", len(claimed))
	}
	if err := alloc.Commit(context.Background(), SplitSubmitted(map[string]int{"aol": 150}, 150), uuid.New()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	row := readLedger(t, f.db, alloc.ID)
	if row.Released != 0 || row.Committed != 150 {
		t.Fatalf("ledger committed=%d released=%d, want 150/0", row.Committed, row.Released)
	}
	if row.ReleaseReason.Valid && row.ReleaseReason.String != "" {
		t.Fatalf("release_reason = %q on a wave that released nothing", row.ReleaseReason.String)
	}
	bal := readBalance(t, f.db, f.day, f.domain, "aol")
	if bal.Tokens != 50 {
		t.Fatalf("tokens = %v, want 50 — nothing should have been handed back", bal.Tokens)
	}
	assertConserved(t, f.db, f.day, f.domain, "aol")
}

// -----------------------------------------------------------------------------
// Scenario 3 — a governor reduces mid-sequence
// -----------------------------------------------------------------------------

// The governor is read by RefillDomain INSIDE Grant, so its ceiling has to
// survive all the way to what the claim takes. Non-negotiable #4: it may only
// REDUCE.
func TestE2E_GovernorReducesTheEffectiveCapAndTheClaim(t *testing.T) {
	// ThrottleGovernor: 100 msgs/hour x 19h window = 1,900 — below the 7,600
	// contract, so it binds. effective 1,900 / 76 intervals = 25 per interval,
	// burst 2 => 50 tokens instead of 200.
	f := newE2EFixture(t, e2eOpts{
		Mode:      ModeOn,
		Governors: Governors{ThrottleGovernor{DB: nil}},
		Domains:   []*DomainContract{standardDomain(7600)},
		Dispatch:  e2eDispatch("wcl_remail", "aol", 100000),
		ClockAt:   10 * time.Hour,
	})
	// The real governor reads the real table; wire it now that the db exists.
	f.svc = NewService(f.db,
		WithGovernors(Governors{ThrottleGovernor{DB: f.db}}),
		WithClock(func() time.Time { return f.clock }))
	f.med = NewMediator(f.db, f.svc, MediatorConfig{
		Mode: ModeOn, Clock: func() time.Time { return f.clock },
		AlertsDisabled: true, PlannerDisabled: true, ContractKey: e2eContractKey,
	})
	if _, err := f.db.Exec(`INSERT INTO mailing_isp_throttle_state (isp, msgs_per_hour) VALUES ('aol', 100)`); err != nil {
		t.Fatalf("seed throttle: %v", err)
	}
	f.seedReady(t, "aol", 200)
	f.tick(t, 10*time.Hour)

	alloc := f.grant(t, "wave-1", TouchClassIntro, 150, "aol")

	bal := readBalance(t, f.db, f.day, f.domain, "aol")
	if bal.Effective != 1900 {
		t.Fatalf("effective = %d, want 1900 (100/hr x 19h)", bal.Effective)
	}
	if bal.Contracted != 7600 {
		t.Fatalf("contracted = %d — a governor MUTATED the contract", bal.Contracted)
	}
	if got := readEffectiveReason(t, f.db, f.day, f.domain, "aol"); got != "throttle" {
		t.Fatalf("effective_reason = %q, want %q — every reader must see the same governor label", got, "throttle")
	}
	// The min() is what gets claimed against: 50, not the 150 asked for and
	// not the 200 the ungoverned bucket would have held.
	if got := alloc.Caps["aol"]; got != 50 {
		t.Fatalf("granted %d, want 50 — the governor's ceiling did not reach the cap", got)
	}
	claimed := f.claim(t, alloc, 150)
	if len(claimed) != 50 {
		t.Fatalf("claimed %d rows against a 50 grant with 200 ready, want 50", len(claimed))
	}
	if n := f.countPCQ(t, "ready"); n != 150 {
		t.Fatalf("%d rows left ready, want 150 — the claim ignored the governed cap", n)
	}
	if err := alloc.Commit(context.Background(), SplitSubmitted(map[string]int{"aol": 50}, 50), uuid.New()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	assertConserved(t, f.db, f.day, f.domain, "aol")
}

// NEGATIVE CONTROL for scenario 3, and non-negotiable #4 in its own right: a
// governor whose ceiling is ABOVE the contract must be ignored entirely. It can
// never RAISE the cap.
func TestE2E_NegativeControl_GovernorAboveContractNeverRaises(t *testing.T) {
	f := newE2EFixture(t, e2eOpts{
		Mode:     ModeOn,
		Domains:  []*DomainContract{standardDomain(7600)},
		Dispatch: e2eDispatch("wcl_remail", "aol", 100000),
		ClockAt:  10 * time.Hour,
	})
	// A static governor 10x the contract, and the throttle table wide open.
	f.svc = NewService(f.db,
		WithGovernors(Governors{e2eStaticGovernor{isp: "aol", name: "generous", ceiling: 76000}}),
		WithClock(func() time.Time { return f.clock }))
	f.med = NewMediator(f.db, f.svc, MediatorConfig{
		Mode: ModeOn, Clock: func() time.Time { return f.clock },
		AlertsDisabled: true, PlannerDisabled: true, ContractKey: e2eContractKey,
	})
	f.seedReady(t, "aol", 500)
	f.tick(t, 10*time.Hour)

	alloc := f.grant(t, "wave-1", TouchClassIntro, 100000, "aol")

	bal := readBalance(t, f.db, f.day, f.domain, "aol")
	if bal.Effective != 7600 {
		t.Fatalf("effective = %d, want 7600 — a governor above the contract RAISED the ceiling", bal.Effective)
	}
	if r := readEffectiveReason(t, f.db, f.day, f.domain, "aol"); r != "" {
		t.Fatalf("effective_reason = %q, want empty — a non-binding governor must not claim the row", r)
	}
	if got := alloc.Caps["aol"]; got != 200 {
		t.Fatalf("granted %d, want 200 — the contract's own token bucket is the ceiling, never the governor", got)
	}
	claimed := f.claim(t, alloc, 100000)
	if len(claimed) != 200 {
		t.Fatalf("claimed %d, want 200", len(claimed))
	}
}

// -----------------------------------------------------------------------------
// Scenario 4 — the contract is exhausted
// -----------------------------------------------------------------------------

// Drain the day's contracted capacity through real grants+commits, then ask
// again. The answer must be a clean zero with a truthful reason and a visible
// drip_tick_outcomes row — not a partial grant, not a panic.
func TestE2E_ExhaustedContractGrantsZeroWithAVisibleReason(t *testing.T) {
	// burst 76 = the whole window, so ONE wave can take the day's 760 and the
	// exhaustion is reached through the real path rather than by hand.
	dc := e2eDomain("em.historythinking.com", "ht", "aol", 760, "01:00", "20:00", 15, 76)
	f := newE2EFixture(t, e2eOpts{
		Mode:     ModeOn,
		Domains:  []*DomainContract{dc},
		Dispatch: e2eDispatch("wcl_remail", "aol", 100000),
		ClockAt:  19*time.Hour + 45*time.Minute,
	})
	f.tick(t, 19*time.Hour+45*time.Minute)

	// POSITIVE / NEGATIVE CONTROL, same mediator, same day: before exhaustion
	// this exact wave grants the full day.
	first := f.grant(t, "wave-drain", TouchClassIntro, 760, "aol")
	if first.Caps["aol"] != 760 {
		t.Fatalf("drain grant = %d, want 760 — the fixture never filled the day, so 'exhausted' would prove nothing", first.Caps["aol"])
	}
	if err := first.Commit(context.Background(), map[string]int{"aol": 760}, uuid.New()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	f.outcome(t, OutcomeFired, "", first.Caps, 760)

	drained := readBalance(t, f.db, f.day, f.domain, "aol")
	if drained.Committed != 760 {
		t.Fatalf("committed = %d, want 760", drained.Committed)
	}
	// Mint the next interval's tokens WITHOUT restoring headroom — the shape a
	// tick after a fully-spent day actually has. Now only the contract, never
	// pacing, can be the thing that says no.
	setBalance(t, f.db, f.day, f.domain, "aol", 760, 50)

	f.tick(t, 19*time.Hour+50*time.Minute)
	second := f.grant(t, "wave-after-exhaustion", TouchClassIntro, 500, "aol")

	if !second.Enforced {
		t.Fatal("an exhausted cell stopped enforcing — the old chain would then mail unmetered")
	}
	if got := second.Caps["aol"]; got != 0 {
		t.Fatalf("granted %d against an exhausted contract, want a clean 0 (a partial grant here overmails the day)", got)
	}
	if second.Reason != ReasonDomainTokens {
		t.Fatalf("binding reason = %q, want %q", second.Reason, ReasonDomainTokens)
	}
	if second.AllocationID() != uuid.Nil {
		t.Fatalf("a zero grant handed back allocation id %s — it would be stamped on claimed rows", second.AllocationID())
	}

	// The zero is RECORDED, not silent.
	var reserved, requested, domainAfter int
	var status, reason string
	if err := f.db.QueryRow(`
		SELECT reserved, requested, domain_balance_after, status, binding_reason
		FROM drip_capacity_ledger
		WHERE day = $1::date AND sending_domain = $2 AND isp = 'aol' AND requested = 500
	`, dayKey(f.day), f.domain).Scan(&reserved, &requested, &domainAfter, &status, &reason); err != nil {
		t.Fatalf("read the zero-grant ledger row: %v", err)
	}
	if reserved != 0 || status != StatusReleased || reason != ReasonDomainTokens || domainAfter != 0 {
		t.Fatalf("zero row reserved=%d status=%q reason=%q domain_after=%d", reserved, status, reason, domainAfter)
	}

	// Nothing was consumed by the refusal.
	after := readBalance(t, f.db, f.day, f.domain, "aol")
	if after.Committed != 760 || after.Reserved != 0 || after.Tokens != 50 {
		t.Fatalf("an exhausted-cell refusal moved the balance: %+v", after)
	}

	// The claim refuses on the same footing rather than panicking, and the
	// orchestrator's outcome row carries the truth.
	_, err := NewTransitions().ClaimByISPCaps(context.Background(), f.db,
		f.lane, f.brand, second.EnforcedCaps(), 500, second.AllocationID())
	if !errors.Is(err, ErrNoAllocation) && !errors.Is(err, ErrNoPositiveGrant) {
		t.Fatalf("claim on a zero grant returned %v, want ErrNoAllocation or ErrNoPositiveGrant", err)
	}
	f.outcome(t, OutcomeZero, SkipNoPositiveGrant, second.EnforcedCaps(), 0)

	outs := f.readOutcomes(t)
	if len(outs) != 2 {
		t.Fatalf("tick outcomes = %+v, want 2 (the fired drain and the zero)", outs)
	}
	last := outs[len(outs)-1]
	if last.Outcome != OutcomeZero {
		t.Fatalf("last outcome = %q, want %q", last.Outcome, OutcomeZero)
	}
	if !strings.Contains(last.Reason, SkipNoPositiveGrant) {
		t.Fatalf("last outcome reason = %q, want it to name %q", last.Reason, SkipNoPositiveGrant)
	}
	if n := f.countPCQ(t, "claimed"); n != 0 {
		t.Fatalf("%d pcq rows claimed off an exhausted contract, want 0", n)
	}
	assertConserved(t, f.db, f.day, f.domain, "aol")
}

// -----------------------------------------------------------------------------
// Scenario 5 — the token bucket's burst ceiling
// -----------------------------------------------------------------------------

// A wave cannot take more than max_burst_intervals worth of tokens no matter
// how large the contracted daily figure is, and no matter how late in the
// window the tick lands.
func TestE2E_BurstCeilingBoundsAWaveBelowTheDailyContract(t *testing.T) {
	// 76,000/day over 76 intervals = 1,000 per interval; burst 2 => 2,000.
	dc := e2eDomain("em.historythinking.com", "ht", "aol", 76000, "01:00", "20:00", 15, 2)
	f := newE2EFixture(t, e2eOpts{
		Mode:     ModeOn,
		Domains:  []*DomainContract{dc},
		Dispatch: e2eDispatch("wcl_remail", "aol", 1000000),
		// 19:45: 75 whole intervals past the window open, so an uncapped
		// bucket would hold the entire 76,000.
		ClockAt: 19*time.Hour + 45*time.Minute,
	})
	f.tick(t, 19*time.Hour+45*time.Minute)

	alloc := f.grant(t, "wave-burst", TouchClassIntro, 60000, "aol")
	if got := alloc.Caps["aol"]; got != 2000 {
		t.Fatalf("granted %d, want 2000 = max_burst_intervals(2) x 1,000/interval — the burst ceiling did not bind", got)
	}
	if alloc.Reason != ReasonDomainTokens {
		t.Fatalf("binding reason = %q, want %q (the bucket, not headroom)", alloc.Reason, ReasonDomainTokens)
	}
	bal := readBalance(t, f.db, f.day, f.domain, "aol")
	if h := (Balance{Effective: bal.Effective, Reserved: bal.Reserved, Committed: bal.Committed}).Headroom(); h < 60000 {
		t.Fatalf("headroom = %d — this test must be bound by the BURST, not by the day's remaining capacity", h)
	}
	if bal.Tokens != 0 {
		t.Fatalf("tokens after the wave = %v, want 0", bal.Tokens)
	}
}

// NEGATIVE CONTROL for scenario 5: the 2,000 came from max_burst_intervals=2,
// not from a constant. The same contract at burst 8 yields exactly 8,000.
func TestE2E_NegativeControl_BurstIntervalsIsWhatSetsTheCeiling(t *testing.T) {
	dc := e2eDomain("em.historythinking.com", "ht", "aol", 76000, "01:00", "20:00", 15, 8)
	f := newE2EFixture(t, e2eOpts{
		Mode:     ModeOn,
		Domains:  []*DomainContract{dc},
		Dispatch: e2eDispatch("wcl_remail", "aol", 1000000),
		ClockAt:  19*time.Hour + 45*time.Minute,
	})
	f.tick(t, 19*time.Hour+45*time.Minute)

	alloc := f.grant(t, "wave-burst-8", TouchClassIntro, 60000, "aol")
	if got := alloc.Caps["aol"]; got != 8000 {
		t.Fatalf("granted %d, want 8000 = max_burst_intervals(8) x 1,000/interval", got)
	}
}

// -----------------------------------------------------------------------------
// Scenario 6 — a crash between Reserve and Commit
// -----------------------------------------------------------------------------

// The wave reserves, claims its records, and dies. ExpireStale — which runs
// inside the NEXT TickStart, not as a separate janitor call — must hand the
// capacity back and stamp why.
func TestE2E_CrashBetweenReserveAndCommitIsRecoveredByTheNextTick(t *testing.T) {
	f := newE2EFixture(t, e2eOpts{
		Mode:     ModeOn,
		Domains:  []*DomainContract{standardDomain(7600)},
		Dispatch: e2eDispatch("wcl_remail", "aol", 100000),
		ClockAt:  10 * time.Hour,
	})
	f.seedReady(t, "aol", 200)
	f.tick(t, 10*time.Hour)

	alloc := f.grant(t, "wave-doomed", TouchClassIntro, 150, "aol")
	if alloc.Caps["aol"] != 150 {
		t.Fatalf("granted %d, want 150", alloc.Caps["aol"])
	}
	if len(f.claim(t, alloc, 150)) != 150 {
		t.Fatal("claim did not take the grant")
	}
	// ... and the process dies here. No Commit, no Release.

	held := readBalance(t, f.db, f.day, f.domain, "aol")
	if held.Reserved != 150 || held.Tokens != 50 {
		t.Fatalf("pre-crash balance reserved=%d tokens=%v, want 150/50", held.Reserved, held.Tokens)
	}

	// The reservation ages past the 45-minute cutoff. created_at is stamped by
	// Postgres, so the age is moved there too.
	if _, err := f.db.Exec(
		`UPDATE drip_capacity_ledger SET created_at = NOW() - INTERVAL '60 minutes' WHERE allocation_id = $1`,
		alloc.ID); err != nil {
		t.Fatalf("age the reservation: %v", err)
	}

	f.tick(t, 10*time.Hour+30*time.Minute)

	row := readLedger(t, f.db, alloc.ID)
	if row.Status != StatusExpired {
		t.Fatalf("ledger status = %q, want %q — the leak backstop did not fire from TickStart", row.Status, StatusExpired)
	}
	if row.Released != 150 || row.Committed != 0 {
		t.Fatalf("ledger released=%d committed=%d, want 150/0", row.Released, row.Committed)
	}
	if !row.ReleaseReason.Valid || row.ReleaseReason.String != "expire_stale" {
		t.Fatalf("release_reason = %v, want expire_stale", row.ReleaseReason)
	}
	bal := readBalance(t, f.db, f.day, f.domain, "aol")
	if bal.Reserved != 0 || bal.Released != 150 {
		t.Fatalf("balance reserved=%d released=%d, want 0/150", bal.Reserved, bal.Released)
	}
	if bal.Tokens != 200 {
		t.Fatalf("tokens = %v, want 200 — the crashed wave's capacity did not come back", bal.Tokens)
	}
	assertConserved(t, f.db, f.day, f.domain, "aol")

	// The CAPACITY comes back; the partner_clean_queue rows do NOT. The orphan
	// reap is operator-gated (DRIP_SUPPLY_REAP_ENABLED, REQ-117 §2.4) and this
	// mediator has it off, so the 150 records stay 'claimed'. Pinned so a future
	// change that silently arms the reap is visible here.
	if n := f.countPCQ(t, "claimed"); n != 150 {
		t.Fatalf("%d pcq rows still claimed, want 150 — the reap is supposed to be operator-gated", n)
	}
}

// NEGATIVE CONTROL for scenario 6: ExpireStale is age-driven. A reservation
// younger than the cutoff must survive the tick untouched — otherwise the
// "recovery" above would just be a mediator that releases everything.
func TestE2E_NegativeControl_FreshReservationSurvivesTheTick(t *testing.T) {
	f := newE2EFixture(t, e2eOpts{
		Mode:     ModeOn,
		Domains:  []*DomainContract{standardDomain(7600)},
		Dispatch: e2eDispatch("wcl_remail", "aol", 100000),
		ClockAt:  10 * time.Hour,
	})
	f.seedReady(t, "aol", 200)
	f.tick(t, 10*time.Hour)

	alloc := f.grant(t, "wave-alive", TouchClassIntro, 150, "aol")
	f.claim(t, alloc, 150)

	f.tick(t, 10*time.Hour+30*time.Minute)

	row := readLedger(t, f.db, alloc.ID)
	if row.Status != StatusReserved {
		t.Fatalf("ledger status = %q, want %q — a live in-flight wave was expired out from under itself", row.Status, StatusReserved)
	}
	if row.Released != 0 {
		t.Fatalf("released %d from a fresh reservation", row.Released)
	}
	bal := readBalance(t, f.db, f.day, f.domain, "aol")
	if bal.Reserved != 150 {
		t.Fatalf("balance reserved = %d, want 150 — the capacity was handed back while the wave still holds it", bal.Reserved)
	}
}

// -----------------------------------------------------------------------------
// Scenario 7 — one bad token fails the WHOLE set
// -----------------------------------------------------------------------------

// §1.5 rule 2, composed: a hand-edited contract must not merely disappear. It
// must take the entire estate's tick with it, so the operator sees an outage
// instead of a quietly smaller estate. This is the property that stood between
// the mode=on cutover and a total drip outage.
func TestE2E_OneTamperedContractFailsEveryLaneClosed(t *testing.T) {
	clean := e2eDomain("em.discountblog.com", "db", "aol", 7600, "01:00", "20:00", 15, 2)
	tampered := standardDomain(7600) // em.historythinking.com
	f := newE2EFixture(t, e2eOpts{
		Mode:     ModeOn,
		Domains:  []*DomainContract{clean, tampered},
		Dispatch: e2eDispatch("wcl_remail", "aol", 100000, "ht", "db"),
		ClockAt:  10 * time.Hour,
	})
	f.seedReady(t, "aol", 400)
	f.tick(t, 10*time.Hour)

	// POSITIVE CONTROL: with every token intact, the CLEAN domain mails.
	before, err := f.med.Grant(context.Background(), GrantReq{
		Day: f.day, Lane: f.lane, Brand: "db", Domain: clean.SendingDomain,
		TouchClass: TouchClassIntro, Pass: PassWelcome, WaveKey: "wave-pre",
		ISPs: []string{"aol"}, Requested: 150,
	})
	if err != nil {
		t.Fatalf("Grant (pre-tamper): %v", err)
	}
	if !before.Enforced || before.Caps["aol"] != 150 {
		t.Fatalf("pre-tamper grant %+v, want an enforced 150 — the tamper below would then prove nothing", before)
	}

	// The hand edit: someone widens a cap with SQL and does not re-issue.
	if _, err := f.db.Exec(`
		UPDATE drip_domain_contracts
		SET daily_max_by_isp = jsonb_set(daily_max_by_isp, '{aol}', '99000')
		WHERE sending_domain = $1 AND status = 'active'
	`, tampered.SendingDomain); err != nil {
		t.Fatalf("hand edit: %v", err)
	}

	// The load refuses EVERYTHING, not just the edited row.
	set, lerr := LoadActiveWithKey(context.Background(), f.db, f.day, e2eContractKey)
	if set != nil {
		t.Fatal("LoadActiveWithKey returned a partial set after a hand edit — the clean domains would mail under an unverified estate")
	}
	var mm *ErrTokenMismatch
	if !errors.As(lerr, &mm) {
		t.Fatalf("load error = %T (%v), want *ErrTokenMismatch", lerr, lerr)
	}

	// Composed: the next tick therefore holds NO contracts, and the CLEAN
	// domain's wave — which has nothing wrong with it — is skipped too.
	f.tick(t, 10*time.Hour+15*time.Minute)
	after, err := f.med.Grant(context.Background(), GrantReq{
		Day: f.day, Lane: f.lane, Brand: "db", Domain: clean.SendingDomain,
		TouchClass: TouchClassIntro, Pass: PassWelcome, WaveKey: "wave-post",
		ISPs: []string{"aol"}, Requested: 150,
	})
	if err != nil {
		t.Fatalf("Grant (post-tamper): %v", err)
	}
	if !after.ShouldSkip() {
		t.Fatalf("the untampered domain still mailed after another contract was hand-edited: %+v", after)
	}
	if after.SkipReason() != SkipNoContract {
		t.Fatalf("skip reason = %q, want %q", after.SkipReason(), SkipNoContract)
	}
	if after.EnforcedCaps() != nil {
		t.Fatalf("a failed-closed wave returned caps %v", after.EnforcedCaps())
	}
	// And it consumed nothing: only the pre-tamper wave's row exists.
	if n := countRows(t, f.db, "drip_capacity_ledger"); n != 1 {
		t.Fatalf("%d ledger rows, want 1 (only the pre-tamper grant)", n)
	}
}

// NEGATIVE CONTROL for scenario 7: the refusal is caused by the TOKEN, not by
// having two domains or by the second tick. Re-issuing the token for the edited
// row restores the whole set.
func TestE2E_NegativeControl_ReissuedTokenRestoresTheWholeSet(t *testing.T) {
	clean := e2eDomain("em.discountblog.com", "db", "aol", 7600, "01:00", "20:00", 15, 2)
	other := standardDomain(7600)
	f := newE2EFixture(t, e2eOpts{
		Mode:     ModeOn,
		Domains:  []*DomainContract{clean, other},
		Dispatch: e2eDispatch("wcl_remail", "aol", 100000, "ht", "db"),
		ClockAt:  10 * time.Hour,
	})
	f.seedReady(t, "aol", 400)
	f.tick(t, 10*time.Hour)
	f.tick(t, 10*time.Hour+15*time.Minute) // the same second tick, no tamper

	a, err := f.med.Grant(context.Background(), GrantReq{
		Day: f.day, Lane: f.lane, Brand: "db", Domain: clean.SendingDomain,
		TouchClass: TouchClassIntro, Pass: PassWelcome, WaveKey: "wave-post",
		ISPs: []string{"aol"}, Requested: 150,
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if a.ShouldSkip() {
		t.Fatalf("a two-domain estate with intact tokens was skipped on the second tick: %+v", a)
	}
	if a.Caps["aol"] != 150 {
		t.Fatalf("granted %d, want 150", a.Caps["aol"])
	}
}

// -----------------------------------------------------------------------------
// Scenario 8 — mode parity
// -----------------------------------------------------------------------------

// off and shadow must decrement NO balance and move NO partner_clean_queue row,
// while still writing the always-on drip_tick_outcomes row. mode=on in the same
// table is the control that proves the fixture would otherwise consume.
func TestE2E_ModeParity_OffAndShadowConsumeNothing(t *testing.T) {
	cases := []struct {
		mode Mode
		// wantAlloc: shadow returns a non-nil, non-enforcing Allocation so the
		// outcome row has something to say; off returns nil outright.
		wantAlloc      bool
		wantShadowRows int
		wantLedgerRows int
		wantClaimed    int
		// wantTokens is the ONE place the two non-enforcing modes differ, and
		// it is not a decrement:
		//
		//	off    100 — Grant returns before touching the database at all.
		//	shadow 200 — Grant refills the domain's bucket (executor.go:735,
		//	             ahead of the enforce branch), so shadow DOES write the
		//	             live balance's PACING columns (tokens, effective,
		//	             effective_reason, last_refill_tick). It still consumes
		//	             nothing: reserved/committed/released stay 0 below.
		//	             That accrual is what keeps the shadow arithmetic equal
		//	             to live and stops a shadow->on flip from starting on a
		//	             cold bucket. Pinned so it stays deliberate.
		wantTokens float64
	}{
		{ModeOff, false, 0, 0, 0, 100},
		{ModeShadow, true, 1, 0, 0, 200},
	}

	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			f := newE2EFixture(t, e2eOpts{
				Mode:     tc.mode,
				Domains:  []*DomainContract{standardDomain(7600)},
				Dispatch: e2eDispatch("wcl_remail", "aol", 100000),
				ClockAt:  10 * time.Hour,
			})
			f.seedReady(t, "aol", 200)

			// MODE=off returns from TickStart before EnsureDayBalances, so the
			// day's rows are seeded here from the same verified contracts the
			// tick would have used. That gives both modes a real balance to
			// leave untouched.
			set, err := LoadActiveWithKey(context.Background(), f.db, f.day, e2eContractKey)
			if err == nil {
				if _, err := EnsureDayBalances(context.Background(), f.db, f.day, set); err != nil {
					t.Fatalf("EnsureDayBalances: %v", err)
				}
			}
			// Contracts are still `scheduled` at this point in off mode, so
			// activate them the way the operator's tick would, then seed.
			if _, err := ActivateScheduled(context.Background(), f.db, f.clock); err != nil {
				t.Fatalf("ActivateScheduled: %v", err)
			}
			set, err = LoadActiveWithKey(context.Background(), f.db, f.day, e2eContractKey)
			if err != nil {
				t.Fatalf("LoadActiveWithKey: %v", err)
			}
			if _, err := EnsureDayBalances(context.Background(), f.db, f.day, set); err != nil {
				t.Fatalf("EnsureDayBalances: %v", err)
			}
			seeded := readBalance(t, f.db, f.day, f.domain, "aol")

			f.tick(t, 10*time.Hour)
			alloc := f.grant(t, "wave-1", TouchClassIntro, 150, "aol")

			if tc.wantAlloc && alloc == nil {
				t.Fatal("shadow mode returned no allocation — the outcome row would have nothing to say")
			}
			if !tc.wantAlloc && alloc != nil {
				t.Fatalf("mode=off returned an allocation %+v — the zero-cost path is gone", alloc)
			}
			if alloc.EnforcedCaps() != nil {
				t.Fatalf("mode=%s returned caps %v — the old chain must decide", tc.mode, alloc.EnforcedCaps())
			}
			if alloc.ShouldSkip() {
				t.Fatalf("mode=%s skipped a wave — a non-enforcing mode must never stop the old chain", tc.mode)
			}

			// Nothing consumed: no live ledger row, no balance movement.
			if n := countRows(t, f.db, "drip_capacity_ledger"); n != tc.wantLedgerRows {
				t.Fatalf("mode=%s wrote %d live ledger rows, want %d", tc.mode, n, tc.wantLedgerRows)
			}
			if n := countRows(t, f.db, "drip_capacity_ledger_shadow"); n != tc.wantShadowRows {
				t.Fatalf("mode=%s wrote %d shadow ledger rows, want %d", tc.mode, n, tc.wantShadowRows)
			}
			bal := readBalance(t, f.db, f.day, f.domain, "aol")
			if bal.Reserved != 0 || bal.Committed != 0 || bal.Released != 0 {
				t.Fatalf("mode=%s CONSUMED capacity: reserved=%d committed=%d released=%d, want 0/0/0",
					tc.mode, bal.Reserved, bal.Committed, bal.Released)
			}
			if bal.Tokens < seeded.Tokens {
				t.Fatalf("mode=%s spent tokens %v -> %v; a non-enforcing mode may accrue but must never consume",
					tc.mode, seeded.Tokens, bal.Tokens)
			}
			if bal.Tokens != tc.wantTokens {
				t.Fatalf("mode=%s tokens = %v, want %v (see the wantTokens comment: off touches nothing, shadow refills)",
					tc.mode, bal.Tokens, tc.wantTokens)
			}
			// No row was locked or moved in partner_clean_queue.
			if n := f.countPCQ(t, "claimed"); n != tc.wantClaimed {
				t.Fatalf("mode=%s claimed %d pcq rows, want %d", tc.mode, n, tc.wantClaimed)
			}
			if n := f.countPCQ(t, "ready"); n != 200 {
				t.Fatalf("mode=%s left %d ready rows, want all 200", tc.mode, n)
			}

			// ...but the always-on visibility surface is still written.
			f.outcome(t, OutcomeFired, "", nil, 0)
			outs := f.readOutcomes(t)
			if len(outs) != 1 {
				t.Fatalf("mode=%s wrote %d tick outcome rows, want 1 — outcomes are written in EVERY mode", tc.mode, len(outs))
			}
			if outs[0].Outcome != OutcomeFired {
				t.Fatalf("mode=%s outcome = %q", tc.mode, outs[0].Outcome)
			}
		})
	}
}

// NEGATIVE CONTROL for scenario 8: the same seeded scenario at mode=on DOES
// consume — balance decremented, live ledger written, pcq rows claimed. Without
// this the parity assertions above would pass on a fixture that could never
// consume anything.
func TestE2E_NegativeControl_ModeOnConsumesTheSameScenario(t *testing.T) {
	f := newE2EFixture(t, e2eOpts{
		Mode:     ModeOn,
		Domains:  []*DomainContract{standardDomain(7600)},
		Dispatch: e2eDispatch("wcl_remail", "aol", 100000),
		ClockAt:  10 * time.Hour,
	})
	f.seedReady(t, "aol", 200)
	f.tick(t, 10*time.Hour)

	alloc := f.grant(t, "wave-1", TouchClassIntro, 150, "aol")
	if alloc.EnforcedCaps() == nil {
		t.Fatal("mode=on returned no caps")
	}
	f.claim(t, alloc, 150)

	if n := countRows(t, f.db, "drip_capacity_ledger"); n != 1 {
		t.Fatalf("%d live ledger rows, want 1", n)
	}
	if n := countRows(t, f.db, "drip_capacity_ledger_shadow"); n != 0 {
		t.Fatalf("%d shadow rows at mode=on, want 0", n)
	}
	bal := readBalance(t, f.db, f.day, f.domain, "aol")
	if bal.Reserved != 150 {
		t.Fatalf("balance reserved = %d, want 150 — mode=on must consume", bal.Reserved)
	}
	if n := f.countPCQ(t, "claimed"); n != 150 {
		t.Fatalf("%d pcq rows claimed, want 150", n)
	}
	if n := f.countPCQ(t, "ready"); n != 50 {
		t.Fatalf("%d ready rows left, want 50", n)
	}
}
