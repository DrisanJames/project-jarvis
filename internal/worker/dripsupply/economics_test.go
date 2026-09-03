package dripsupply

// REQ-118 WP8 tests (docs/DRIP_SUPPLY_CHAIN_DESIGN.md §4, §8 WP1 DoD row).
//
// The formula tests run against REAL Postgres, not sqlmock: what is under test
// is arithmetic Postgres performs (the NUMERIC share split, the fan-out an
// offer join would produce, ON CONFLICT, the reap's timestamp predicate). A
// mock returns canned rows and cannot tell a correct query from a wrong one.
//
// They use the LOCAL apex-postgres container in scratch database `req118_res`,
// each test in its own schema, dropped at the end. Nothing here can reach
// production: the DSN is hard-defaulted to localhost and every test SKIPS
// (never fails, never falls back) when it is unreachable.
//
// The harness is deliberately SELF-CONTAINED rather than reusing
// reservation_test.go's newTestDB: that file is owned by another work package
// and is being edited concurrently, and its schemaDDL() builds the capacity
// tables, not the economics ones. Same scratch database, distinct names.

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
	econAdminDSNEnv     = "DRIPSUPPLY_TEST_ADMIN_DSN"
	econDefaultAdminDSN = "postgres://apex_user:apex_password@localhost:5432/apex_db?sslmode=disable"
	econScratchDBName   = "req118_res"
)

func econAdminDSN() string {
	if v := strings.TrimSpace(os.Getenv(econAdminDSNEnv)); v != "" {
		return v
	}
	return econDefaultAdminDSN
}

func econScratchDSN(t *testing.T) string {
	t.Helper()
	dsn := econAdminDSN()
	i := strings.LastIndex(dsn, "/")
	if i < 0 {
		t.Skipf("cannot derive a scratch DSN from %q", dsn)
	}
	tail := dsn[i+1:]
	q := ""
	if j := strings.Index(tail, "?"); j >= 0 {
		q = tail[j:]
	}
	return dsn[:i+1] + econScratchDBName + q
}

func econEnsureScratchDB(t *testing.T) {
	t.Helper()
	admin, err := sql.Open("postgres", econAdminDSN())
	if err != nil {
		t.Skipf("cannot open admin DSN: %v", err)
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Skipf("local postgres unreachable (%v) — set %s to run the economics integration tests", err, econAdminDSNEnv)
	}
	var exists bool
	if err := admin.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, econScratchDBName).Scan(&exists); err != nil {
		t.Skipf("cannot list databases: %v", err)
	}
	if exists {
		return
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+econScratchDBName); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		t.Skipf("cannot create scratch database %s: %v", econScratchDBName, err)
	}
}

// econSchemaDDL is the production shape of every table ComputeLaneEconomics
// reads or writes.
//
// The three tables WP8 owns come from the package constants, so a WP1/WP8
// drift breaks these tests rather than production. The rest are faithful
// SUBSETS of the production tables — only the columns the queries touch, with
// the production types (verified against information_schema on 2026-09-03) —
// because building all 22 columns of mailing_campaign_isp_plans would add
// nothing the queries can observe.
func econSchemaDDL() []string {
	return []string{
		LaneEconomicsDDL,
		ManualRevenueDDL,
		CostRatesDDL,

		// WP1 owns drip_supply_ledger (req118_create_drip_supply_ledger);
		// this is the subset loadEconVerdicts reads, same types and CHECK.
		`CREATE TABLE IF NOT EXISTS drip_supply_ledger (
			entry_id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			occurred_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			lane         TEXT NOT NULL,
			source_slug  TEXT NOT NULL DEFAULT '',
			isp          TEXT NOT NULL,
			event        TEXT NOT NULL,
			quantity     INT NOT NULL
		)`,
		// WP1 owns drip_source_contracts; subset read by loadAcquisitionCosts.
		`CREATE TABLE IF NOT EXISTS drip_source_contracts (
			id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			source_slug           TEXT NOT NULL,
			status                TEXT NOT NULL,
			effective_at          TIMESTAMPTZ NOT NULL,
			unit_acquisition_cost NUMERIC NOT NULL DEFAULT 0
		)`,

		// Production tables (subsets).
		`CREATE TABLE IF NOT EXISTS mailing_campaigns (
			id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			name             VARCHAR NOT NULL,
			status           VARCHAR,
			sent_count       INT DEFAULT 0,
			total_recipients INT DEFAULT 0,
			scheduled_at     TIMESTAMPTZ,
			created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS mailing_campaign_isp_plans (
			id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			campaign_id              UUID NOT NULL,
			isp                      VARCHAR,
			audience_selected_count  INT DEFAULT 0,
			enqueued_count           INT DEFAULT 0,
			sent_count               INT DEFAULT 0,
			created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS mailing_everflow_conversions (
			id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			campaign_id        UUID,
			everflow_offer_id  TEXT,
			payout             NUMERIC,
			converted_at       TIMESTAMPTZ
		)`,
		`CREATE TABLE IF NOT EXISTS mailing_offers (
			id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			name              TEXT,
			everflow_offer_id TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS partner_datasets (
			id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			slug     TEXT NOT NULL,
			vertical TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS partner_clean_queue (
			id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			vertical     TEXT,
			isp_family   TEXT,
			validated_at TIMESTAMPTZ
		)`,
	}
}

func newEconTestDB(t *testing.T) *sql.DB {
	t.Helper()
	econEnsureScratchDB(t)
	schema := "e" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")

	bootstrap, err := sql.Open("postgres", econScratchDSN(t))
	if err != nil {
		t.Skipf("cannot open scratch DSN: %v", err)
	}
	defer bootstrap.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := bootstrap.PingContext(ctx); err != nil {
		t.Skipf("scratch database unreachable: %v", err)
	}
	if _, err := bootstrap.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}

	dsn := econScratchDSN(t)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("postgres", dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatalf("open scratch: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		clean, err := sql.Open("postgres", econScratchDSN(t))
		if err == nil {
			_, _ = clean.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
			clean.Close()
		}
	})
	for _, stmt := range econSchemaDDL() {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("ddl %.60s: %v", strings.ReplaceAll(stmt, "\n", " "), err)
		}
	}
	return db
}

func econLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		return time.UTC
	}
	return loc
}

// econDay is a fixed, non-DST-boundary Denver day so every hand-computed
// expectation below is reproducible whenever the suite runs.
func econDay(t *testing.T) time.Time {
	t.Helper()
	return time.Date(2026, 9, 10, 0, 0, 0, 0, econLoc(t))
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %.80s: %v", strings.ReplaceAll(q, "\n", " "), err)
	}
}

// seedCampaign inserts one drip campaign and its ISP plans, returning the id.
// planSel maps ISP → audience_selected_count. Every plan row is written with
// sent_count = 0, which is what production carries (105,870 of 105,870 plan
// rows over a week read 0) — so a test that passes here CANNOT be reading it.
func seedCampaign(t *testing.T, db *sql.DB, name string, sched time.Time, sent int, planSel map[string]int) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.QueryRow(`INSERT INTO mailing_campaigns (name, status, sent_count, scheduled_at)
		VALUES ($1, 'sent', $2, $3) RETURNING id`, name, sent, sched).Scan(&id); err != nil {
		t.Fatalf("seed campaign %q: %v", name, err)
	}
	for isp, sel := range planSel {
		mustExec(t, db, `INSERT INTO mailing_campaign_isp_plans
			(campaign_id, isp, audience_selected_count, enqueued_count, sent_count)
			VALUES ($1, $2, $3, $4, 0)`, id, isp, sel, sel)
	}
	return id
}

func seedRates(t *testing.T, db *sql.DB, infra *float64) {
	t.Helper()
	mustExec(t, db, `INSERT INTO drip_cost_rates (key, value, unit) VALUES
		('eo_per_verdict',      0.000244, 'usd_per_verdict'),
		('eo_list_per_verdict', 0.0006,   'usd_per_verdict'),
		('ses_per_message',     0.0001,   'usd_per_message'),
		('pmta_per_message',    0,        'usd_per_message')`)
	if infra == nil {
		mustExec(t, db, `INSERT INTO drip_cost_rates (key, value, unit) VALUES ('infra_monthly_usd', NULL, 'usd_per_month')`)
	} else {
		mustExec(t, db, `INSERT INTO drip_cost_rates (key, value, unit) VALUES ('infra_monthly_usd', $1::numeric, 'usd_per_month')`, *infra)
	}
}

// econRow reads one drip_lane_economics row.
type econRow struct {
	Messages, Intros, Followups, Verdicts, Conversions int
	RevenueEverflow, RevenueManual                     float64
	SendCost, EOCost, AcquisitionCost                  float64
	InfraShare                                         sql.NullFloat64
	GrossECPM, ContributionECPM                        float64
	CleaningValue, FullyLoadedECPM                     sql.NullFloat64
	Maturity                                           string
	SampleOK                                           bool
}

func readEconRow(t *testing.T, db *sql.DB, day time.Time, lane, isp string) econRow {
	t.Helper()
	var r econRow
	err := db.QueryRow(`SELECT messages, intros, followups, verdicts, conversions,
		revenue_everflow::float8, revenue_manual::float8, send_cost::float8, eo_cost::float8,
		acquisition_cost::float8, infra_share::float8, gross_ecpm::float8, contribution_ecpm::float8,
		cleaning_value::float8, fully_loaded_ecpm::float8, maturity, sample_ok
		FROM drip_lane_economics WHERE day = $1::date AND lane = $2 AND isp = $3`,
		day.Format("2006-01-02"), lane, isp).Scan(
		&r.Messages, &r.Intros, &r.Followups, &r.Verdicts, &r.Conversions,
		&r.RevenueEverflow, &r.RevenueManual, &r.SendCost, &r.EOCost,
		&r.AcquisitionCost, &r.InfraShare, &r.GrossECPM, &r.ContributionECPM,
		&r.CleaningValue, &r.FullyLoadedECPM, &r.Maturity, &r.SampleOK)
	if err != nil {
		t.Fatalf("read economics %s/%s/%s: %v", day.Format("2006-01-02"), lane, isp, err)
	}
	return r
}

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %.12f, want %.12f", what, got, want)
	}
}

// -----------------------------------------------------------------------------
// The fixture — one fixed day, hand-computed
// -----------------------------------------------------------------------------
//
// lane "alpha" (two ISPs, three campaigns) and lane "beta" (one ISP, one
// campaign that clears the conversion sample threshold).
//
//	C1 alpha intro    SES  sent=1000  plans aol 750 / yahoo 250  → 0.75 / 0.25
//	C2 alpha followup SES  sent= 400  plans aol 100 / yahoo 100  → 0.50 / 0.50
//	C3 alpha intro    PMTA sent= 200  plans aol 200              → 1.00
//	C4 beta  intro    SES  sent=2000  plans aol 2000             → 1.00
//	C5 alpha intro    SES  sent=   0  NO plan rows               → contributes nothing
//
//	alpha/aol   messages 750+200+200 = 1150  intros 950  followups 200  ses 950 pmta 200
//	alpha/yahoo messages 250+200     =  450  intros 250  followups 200  ses 450 pmta   0
//	beta/aol    messages             = 2000  intros 2000 followups   0  ses 2000
//
//	conversions: 4 on C1 @ $25 = $100 → aol 3 / $75.00, yahoo 1 / $25.00
//	             5 on C4 @ $10 =  $50 → aol 5 / $50.00
//	verdicts:    alpha aol 1000, alpha yahoo 500, beta aol 800
//	manual:      alpha $30 over 2026-09-09..2026-09-11 (3 days) → $10 for the day
//	             split by the day's message share 1150/1600 and 450/1600
//	acquisition: source contract for alpha only, $0.002/record
func seedEconFixture(t *testing.T, db *sql.DB, day time.Time) map[string]uuid.UUID {
	t.Helper()
	ids := map[string]uuid.UUID{}

	ids["C1"] = seedCampaign(t, db, "[partner-drip] alpha ht 20260910T010000 aaa bbb [ses:x1]",
		day.Add(1*time.Hour), 1000, map[string]int{"aol": 750, "yahoo": 250})
	ids["C2"] = seedCampaign(t, db, "[partner-drip] alpha ht 20260910T020000 aaa ccc [ses:x1] [t2]",
		day.Add(2*time.Hour), 400, map[string]int{"aol": 100, "yahoo": 100})
	ids["C3"] = seedCampaign(t, db, "[partner-drip] alpha db 20260910T030000 aaa ddd",
		day.Add(3*time.Hour), 200, map[string]int{"aol": 200})
	ids["C4"] = seedCampaign(t, db, "[partner-drip] beta ht 20260910T040000 aaa eee [ses:y1]",
		day.Add(4*time.Hour), 2000, map[string]int{"aol": 2000})
	ids["C5"] = seedCampaign(t, db, "[partner-drip] alpha ht 20260910T050000 aaa fff [ses:x1]",
		day.Add(5*time.Hour), 0, nil)

	// Three mailing_offers rows share EF 162 — the production fan-out trap.
	for i := 0; i < 3; i++ {
		mustExec(t, db, `INSERT INTO mailing_offers (name, everflow_offer_id) VALUES ($1, '162')`,
			fmt.Sprintf("heloc variant %d", i))
	}
	for i := 0; i < 4; i++ {
		mustExec(t, db, `INSERT INTO mailing_everflow_conversions (campaign_id, everflow_offer_id, payout, converted_at)
			VALUES ($1, '162', 25.00, $2)`, ids["C1"], day.Add(time.Duration(30+i)*time.Hour))
	}
	for i := 0; i < 5; i++ {
		mustExec(t, db, `INSERT INTO mailing_everflow_conversions (campaign_id, everflow_offer_id, payout, converted_at)
			VALUES ($1, '162', 10.00, $2)`, ids["C4"], day.Add(time.Duration(30+i)*time.Hour))
	}

	mustExec(t, db, `INSERT INTO drip_supply_ledger (lane, isp, event, quantity, occurred_at) VALUES
		('alpha', 'aol',   'VALIDATION_ORDERED', 1000, $1),
		('alpha', 'yahoo', 'VALIDATION_ORDERED',  500, $1),
		('beta',  'aol',   'VALIDATION_ORDERED',  800, $1)`, day.Add(6*time.Hour))

	mustExec(t, db, `INSERT INTO drip_manual_revenue
		(lane, revenue_date, attribution_start, attribution_end, amount, source, entered_by)
		VALUES ('alpha', '2026-09-09', '2026-09-09', '2026-09-11', 30.00, 'cpm deal', 'operator')`)

	mustExec(t, db, `INSERT INTO partner_datasets (slug, vertical) VALUES ('alpha_src', 'alpha')`)
	mustExec(t, db, `INSERT INTO drip_source_contracts (source_slug, status, effective_at, unit_acquisition_cost)
		VALUES ('alpha_src', 'active', $1, 0.002)`, day.AddDate(0, 0, -30))

	return ids
}

// -----------------------------------------------------------------------------
// 1. The formulas, on the fixed fixture
// -----------------------------------------------------------------------------

func TestComputeLaneEconomics_Formulas(t *testing.T) {
	db := newEconTestDB(t)
	day := econDay(t)
	seedRates(t, db, nil) // infra_monthly_usd NULL — the seeded production state
	seedEconFixture(t, db, day)

	// now = day + 3 days ⇒ younger than the 7-day attribution window.
	now := day.AddDate(0, 0, 3)
	if err := computeLaneEconomics(context.Background(), db, day, now); err != nil {
		t.Fatalf("compute: %v", err)
	}

	// ── alpha / aol ────────────────────────────────────────────────────
	// messages 1150 · intros 950 · followups 200 · ses 950 · pmta 200
	// revenue  $75.00 everflow + $7.1875 manual (= $10/day × 1150/1600)
	// send     950 × 0.0001 + 200 × 0     = 0.095
	// eo       1000 × 0.000244            = 0.244
	// acq      950 × 0.002                = 1.90
	// gross    82.1875 / 1150 × 1000      = 71.467391304347826…
	// contrib  (82.1875 − 0.095) / 1.15   = 71.384782608695652…
	aol := readEconRow(t, db, day, "alpha", "aol")
	if aol.Messages != 1150 || aol.Intros != 950 || aol.Followups != 200 {
		t.Errorf("alpha/aol messages/intros/followups = %d/%d/%d, want 1150/950/200",
			aol.Messages, aol.Intros, aol.Followups)
	}
	if aol.Verdicts != 1000 {
		t.Errorf("alpha/aol verdicts = %d, want 1000", aol.Verdicts)
	}
	if aol.Conversions != 3 {
		t.Errorf("alpha/aol conversions = %d, want 3 (4 conversions × the 0.75 message share)", aol.Conversions)
	}
	near(t, "alpha/aol revenue_everflow", aol.RevenueEverflow, 75.0)
	near(t, "alpha/aol revenue_manual", aol.RevenueManual, 10.0*1150.0/1600.0)
	near(t, "alpha/aol send_cost", aol.SendCost, 0.095)
	near(t, "alpha/aol eo_cost", aol.EOCost, 0.244)
	near(t, "alpha/aol acquisition_cost", aol.AcquisitionCost, 1.90)
	near(t, "alpha/aol gross_ecpm", aol.GrossECPM, (75.0+10.0*1150.0/1600.0)/1150.0*1000.0)
	near(t, "alpha/aol contribution_ecpm", aol.ContributionECPM, (75.0+10.0*1150.0/1600.0-0.095)/1150.0*1000.0)

	// infra_monthly_usd is NULL, so BOTH infra-derived numbers are NULL.
	// "unknown renders as unknown, never zero" (§6).
	if aol.InfraShare.Valid {
		t.Errorf("alpha/aol infra_share = %v, want NULL while infra_monthly_usd is unset", aol.InfraShare.Float64)
	}
	if aol.FullyLoadedECPM.Valid {
		t.Errorf("alpha/aol fully_loaded_ecpm = %v, want NULL while infra_share is NULL", aol.FullyLoadedECPM.Float64)
	}

	// ── alpha / yahoo ──────────────────────────────────────────────────
	yahoo := readEconRow(t, db, day, "alpha", "yahoo")
	if yahoo.Messages != 450 || yahoo.Intros != 250 || yahoo.Followups != 200 {
		t.Errorf("alpha/yahoo messages/intros/followups = %d/%d/%d, want 450/250/200",
			yahoo.Messages, yahoo.Intros, yahoo.Followups)
	}
	if yahoo.Verdicts != 500 || yahoo.Conversions != 1 {
		t.Errorf("alpha/yahoo verdicts/conversions = %d/%d, want 500/1", yahoo.Verdicts, yahoo.Conversions)
	}
	near(t, "alpha/yahoo revenue_everflow", yahoo.RevenueEverflow, 25.0)
	near(t, "alpha/yahoo revenue_manual", yahoo.RevenueManual, 10.0*450.0/1600.0)
	near(t, "alpha/yahoo send_cost", yahoo.SendCost, 0.045)
	near(t, "alpha/yahoo acquisition_cost", yahoo.AcquisitionCost, 0.50)
	near(t, "alpha/yahoo gross_ecpm", yahoo.GrossECPM, (25.0+10.0*450.0/1600.0)/450.0*1000.0)

	// The whole $10/day of manual revenue is attributed, none lost.
	near(t, "alpha manual revenue total", aol.RevenueManual+yahoo.RevenueManual, 10.0)

	// ── beta / aol ─────────────────────────────────────────────────────
	// No source contract ⇒ acquisition 0 (§1.1 default), and 5 conversions
	// clears the sample threshold even though 2000 messages does not.
	beta := readEconRow(t, db, day, "beta", "aol")
	if beta.Messages != 2000 || beta.Intros != 2000 || beta.Followups != 0 {
		t.Errorf("beta/aol messages/intros/followups = %d/%d/%d, want 2000/2000/0",
			beta.Messages, beta.Intros, beta.Followups)
	}
	near(t, "beta/aol acquisition_cost", beta.AcquisitionCost, 0)
	near(t, "beta/aol send_cost", beta.SendCost, 0.2)
	near(t, "beta/aol eo_cost", beta.EOCost, 800*0.000244)
	near(t, "beta/aol gross_ecpm", beta.GrossECPM, 25.0)
	near(t, "beta/aol contribution_ecpm", beta.ContributionECPM, 24.9)

	// cleaning_value = conv_rate × rev_per_conv + manual/intro
	//                  − acquisition − eo/yield − send over the ladder
	//                = 0.0025 × 10 + 0 − 0 − 0.000244 × (800/2000) − 0.2/2000
	//                = 0.025 − 0.0000976 − 0.0001 = 0.0248024
	if !beta.CleaningValue.Valid {
		t.Fatal("beta/aol cleaning_value is NULL — 5 conversions clears the §4 minimum sample")
	}
	near(t, "beta/aol cleaning_value", beta.CleaningValue.Float64, 0.0248024)

	// The plan-less campaign C5 contributes nothing, and nothing invented a
	// lane for it.
	var lanes int
	if err := db.QueryRow(`SELECT COUNT(DISTINCT lane) FROM drip_lane_economics WHERE day = $1::date`,
		day.Format("2006-01-02")).Scan(&lanes); err != nil {
		t.Fatal(err)
	}
	if lanes != 2 {
		t.Errorf("distinct lanes = %d, want 2 (alpha, beta)", lanes)
	}
}

// TestComputeLaneEconomics_InfraShareWhenRateIsSet is the positive half of the
// NULL-propagation rule: with infra_monthly_usd set, both infra_share and
// fully_loaded_ecpm become numbers, and the day's shares sum to
// infra_monthly_usd / 30.
func TestComputeLaneEconomics_InfraShareWhenRateIsSet(t *testing.T) {
	db := newEconTestDB(t)
	day := econDay(t)
	infra := 3000.0
	seedRates(t, db, &infra)
	seedEconFixture(t, db, day)

	if err := computeLaneEconomics(context.Background(), db, day, day.AddDate(0, 0, 3)); err != nil {
		t.Fatalf("compute: %v", err)
	}

	// estate messages for the day = 1150 + 450 + 2000 = 3600
	aol := readEconRow(t, db, day, "alpha", "aol")
	if !aol.InfraShare.Valid {
		t.Fatal("infra_share is NULL with infra_monthly_usd = 3000")
	}
	near(t, "alpha/aol infra_share", aol.InfraShare.Float64, 3000.0/30.0*(1150.0/3600.0))
	if !aol.FullyLoadedECPM.Valid {
		t.Fatal("fully_loaded_ecpm is NULL with infra_share set")
	}
	revenue := 75.0 + 10.0*1150.0/1600.0
	want := (revenue - 0.095 - 0.244 - 1.90 - 3000.0/30.0*(1150.0/3600.0)) / 1150.0 * 1000.0
	near(t, "alpha/aol fully_loaded_ecpm", aol.FullyLoadedECPM.Float64, want)

	var total float64
	if err := db.QueryRow(`SELECT COALESCE(SUM(infra_share),0)::float8 FROM drip_lane_economics WHERE day = $1::date`,
		day.Format("2006-01-02")).Scan(&total); err != nil {
		t.Fatal(err)
	}
	near(t, "estate infra_share total", total, 100.0) // 3000 / 30
}

// -----------------------------------------------------------------------------
// 2. The unjoined-conversion proof (the EF-162 ×3 trap)
// -----------------------------------------------------------------------------

// TestUnjoinedConversionCounting proves BOTH halves of the §8 WP8 DoD:
// joining mailing_offers fans EF-162 conversions ×3, and the query this
// package actually runs does not.
func TestUnjoinedConversionCounting(t *testing.T) {
	db := newEconTestDB(t)
	day := econDay(t)
	seedRates(t, db, nil)
	seedEconFixture(t, db, day)

	dayStart, dayEnd := dayWindow(day)

	// NEGATIVE CONTROL: the join that must never be written. If this stops
	// producing 12 (4 conversions × 3 offer rows on C1) the trap has moved
	// and the assertion below is no longer evidence of anything.
	const joinedSQL = `
		SELECT COUNT(*)
		  FROM mailing_everflow_conversions ec
		  JOIN mailing_campaigns c ON c.id = ec.campaign_id
		  JOIN mailing_offers o ON o.everflow_offer_id = ec.everflow_offer_id
		 WHERE c.name LIKE '[partner-drip] alpha %'
		   AND ec.converted_at >= $1 AND ec.converted_at < $2`
	var fanned int
	if err := db.QueryRow(joinedSQL, dayStart, dayEnd.AddDate(0, 0, EconAttributionDays)).Scan(&fanned); err != nil {
		t.Fatal(err)
	}
	if fanned != 12 {
		t.Fatalf("NEGATIVE CONTROL: the offer join produced %d rows, want 12 (4 conversions × 3 mailing_offers rows sharing EF 162) — the fan-out fixture is wrong, so the assertion below proves nothing", fanned)
	}

	// The real reader: unjoined, attributed by campaign_id.
	rev, err := loadEconEverflow(context.Background(), db, dayStart, dayEnd, EconAttributionDays)
	if err != nil {
		t.Fatalf("loadEconEverflow: %v", err)
	}
	var conversions, payout float64
	for k, r := range rev {
		if k.Lane == "alpha" {
			conversions += r.Conversions
			payout += r.Payout
		}
	}
	near(t, "alpha conversions (unjoined)", conversions, 4)
	near(t, "alpha payout (unjoined)", payout, 100.0)

	// And the same, through the full pipeline.
	if err := computeLaneEconomics(context.Background(), db, day, day.AddDate(0, 0, 3)); err != nil {
		t.Fatalf("compute: %v", err)
	}
	var storedConv int
	var storedPayout float64
	if err := db.QueryRow(`SELECT COALESCE(SUM(conversions),0), COALESCE(SUM(revenue_everflow),0)::float8
		FROM drip_lane_economics WHERE day = $1::date AND lane = 'alpha'`,
		day.Format("2006-01-02")).Scan(&storedConv, &storedPayout); err != nil {
		t.Fatal(err)
	}
	if storedConv != 4 {
		t.Errorf("stored alpha conversions = %d, want 4 (a join would have written 12)", storedConv)
	}
	near(t, "stored alpha revenue_everflow", storedPayout, 100.0)
}

// TestPlanSentCountIsNotTheSource pins the other production trap: the split
// comes from audience_selected_count and the LEVEL from
// mailing_campaigns.sent_count, because mailing_campaign_isp_plans.sent_count
// is 0 on every row in production.
func TestPlanSentCountIsNotTheSource(t *testing.T) {
	db := newEconTestDB(t)
	day := econDay(t)
	seedRates(t, db, nil)
	seedEconFixture(t, db, day)

	// NEGATIVE CONTROL: the column the design warns against reads 0, exactly
	// as it does in production. A pipeline sourcing it would report an estate
	// that sent nothing.
	var planSent int
	if err := db.QueryRow(`SELECT COALESCE(SUM(sent_count),0) FROM mailing_campaign_isp_plans`).Scan(&planSent); err != nil {
		t.Fatal(err)
	}
	if planSent != 0 {
		t.Fatalf("NEGATIVE CONTROL: fixture plan sent_count = %d, want 0 (production carries 0 on all 105,870 rows)", planSent)
	}

	if err := computeLaneEconomics(context.Background(), db, day, day.AddDate(0, 0, 3)); err != nil {
		t.Fatalf("compute: %v", err)
	}
	var messages int
	if err := db.QueryRow(`SELECT COALESCE(SUM(messages),0) FROM drip_lane_economics WHERE day = $1::date`,
		day.Format("2006-01-02")).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 3600 {
		t.Errorf("estate messages = %d, want 3600 (1000+400+200+2000 from mailing_campaigns.sent_count)", messages)
	}
}

// -----------------------------------------------------------------------------
// 3. Maturity and sample rules
// -----------------------------------------------------------------------------

func TestMaturityFlipsAtTheAttributionWindow(t *testing.T) {
	db := newEconTestDB(t)
	day := econDay(t)
	seedRates(t, db, nil)
	seedEconFixture(t, db, day)
	ctx := context.Background()

	for _, tc := range []struct {
		ageDays int
		want    string
	}{
		{0, MaturityIncomplete},
		{6, MaturityIncomplete}, // negative control: one day short is NOT mature
		{7, MaturityMature},
		{30, MaturityMature},
	} {
		if err := computeLaneEconomics(ctx, db, day, day.AddDate(0, 0, tc.ageDays)); err != nil {
			t.Fatalf("compute at age %d: %v", tc.ageDays, err)
		}
		got := readEconRow(t, db, day, "alpha", "aol").Maturity
		if got != tc.want {
			t.Errorf("age %d days: maturity = %q, want %q", tc.ageDays, got, tc.want)
		}
	}
}

func TestSampleOKAndCleaningValueGate(t *testing.T) {
	db := newEconTestDB(t)
	day := econDay(t)
	seedRates(t, db, nil)
	seedEconFixture(t, db, day)

	if err := computeLaneEconomics(context.Background(), db, day, day.AddDate(0, 0, 8)); err != nil {
		t.Fatalf("compute: %v", err)
	}

	// beta/aol: 5 conversions ⇒ sample_ok, cleaning_value present.
	beta := readEconRow(t, db, day, "beta", "aol")
	if !beta.SampleOK {
		t.Error("beta/aol sample_ok = false, want true (5 conversions ≥ the §4 minimum of 5)")
	}
	if !beta.CleaningValue.Valid {
		t.Error("beta/aol cleaning_value is NULL despite sample_ok")
	}

	// NEGATIVE CONTROL: alpha/aol has 1150 trailing messages (< 20,000) and 3
	// conversions (< 5), so it is below sample and cleaning_value MUST be
	// NULL — a cleaning value off this sample is noise the supply controller
	// would spend real EO dollars on.
	aol := readEconRow(t, db, day, "alpha", "aol")
	if aol.SampleOK {
		t.Error("alpha/aol sample_ok = true, want false (1150 messages, 3 conversions)")
	}
	if aol.CleaningValue.Valid {
		t.Errorf("alpha/aol cleaning_value = %v, want NULL below sample", aol.CleaningValue.Float64)
	}
}

// TestSampleOKMessagesThreshold exercises the OTHER limb of the §4 rule —
// 20,000 trailing messages with no conversions at all.
func TestSampleOKMessagesThreshold(t *testing.T) {
	db := newEconTestDB(t)
	day := econDay(t)
	seedRates(t, db, nil)

	seedCampaign(t, db, "[partner-drip] bulk ht 20260910T010000 a b [ses:z]",
		day.Add(time.Hour), 20000, map[string]int{"aol": 1})
	if err := computeLaneEconomics(context.Background(), db, day, day.AddDate(0, 0, 8)); err != nil {
		t.Fatalf("compute: %v", err)
	}
	if got := readEconRow(t, db, day, "bulk", "aol"); !got.SampleOK {
		t.Error("bulk/aol sample_ok = false, want true (20,000 messages hits the threshold with 0 conversions)")
	}

	// NEGATIVE CONTROL: one message fewer does not clear it.
	db2 := newEconTestDB(t)
	seedRates(t, db2, nil)
	seedCampaign(t, db2, "[partner-drip] bulk ht 20260910T010000 a b [ses:z]",
		day.Add(time.Hour), 19999, map[string]int{"aol": 1})
	if err := computeLaneEconomics(context.Background(), db2, day, day.AddDate(0, 0, 8)); err != nil {
		t.Fatalf("compute: %v", err)
	}
	if got := readEconRow(t, db2, day, "bulk", "aol"); got.SampleOK {
		t.Error("19,999 messages set sample_ok = true; the threshold is not being applied")
	}
}

// -----------------------------------------------------------------------------
// 4. Verdict source: ledger first, pcq fallback second
// -----------------------------------------------------------------------------

func TestVerdictsPreferTheSupplyLedgerAndFallBackToPCQ(t *testing.T) {
	db := newEconTestDB(t)
	day := econDay(t)
	dayStart, dayEnd := dayWindow(day)
	ctx := context.Background()

	// Ledger empty ⇒ the pcq fallback, and it says so.
	mustExec(t, db, `INSERT INTO partner_clean_queue (vertical, isp_family, validated_at)
		SELECT 'alpha', 'aol', $1 FROM generate_series(1, 7)`, day.Add(time.Hour))
	got, src, err := loadEconVerdicts(ctx, db, dayStart, dayEnd)
	if err != nil {
		t.Fatal(err)
	}
	if src != "pcq_fallback" {
		t.Errorf("verdict source = %q, want pcq_fallback while drip_supply_ledger is empty", src)
	}
	if got[econKey{"alpha", "aol"}] != 7 {
		t.Errorf("pcq fallback verdicts = %d, want 7", got[econKey{"alpha", "aol"}])
	}

	// NEGATIVE CONTROL: once the ledger carries VALIDATION_ORDERED it wins,
	// and the (different) pcq number must NOT appear.
	mustExec(t, db, `INSERT INTO drip_supply_ledger (lane, isp, event, quantity, occurred_at)
		VALUES ('alpha', 'aol', 'VALIDATION_ORDERED', 900, $1)`, day.Add(time.Hour))
	got, src, err = loadEconVerdicts(ctx, db, dayStart, dayEnd)
	if err != nil {
		t.Fatal(err)
	}
	if src != "supply_ledger" {
		t.Errorf("verdict source = %q, want supply_ledger once it has rows", src)
	}
	if got[econKey{"alpha", "aol"}] != 900 {
		t.Errorf("ledger verdicts = %d, want 900 (never the pcq 7)", got[econKey{"alpha", "aol"}])
	}

	// A non-VALIDATION_ORDERED event is not a verdict.
	mustExec(t, db, `INSERT INTO drip_supply_ledger (lane, isp, event, quantity, occurred_at)
		VALUES ('alpha', 'yahoo', 'MAILABLE', 5000, $1)`, day.Add(time.Hour))
	got, _, err = loadEconVerdicts(ctx, db, dayStart, dayEnd)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := got[econKey{"alpha", "yahoo"}]; ok {
		t.Errorf("MAILABLE counted as %d verdicts; only VALIDATION_ORDERED is a verdict", n)
	}
}

// -----------------------------------------------------------------------------
// 5. The upsert: idempotent, and it reaps
// -----------------------------------------------------------------------------

func TestUpsertIsIdempotentAndReapsStaleRows(t *testing.T) {
	db := newEconTestDB(t)
	day := econDay(t)
	ctx := context.Background()
	seedRates(t, db, nil)
	ids := seedEconFixture(t, db, day)

	// A row on a DIFFERENT day, which the reap must never touch.
	other := day.AddDate(0, 0, -1)
	mustExec(t, db, `INSERT INTO drip_lane_economics (day, lane, isp, messages, maturity, computed_at)
		VALUES ($1::date, 'ghost', 'aol', 42, 'mature', NOW() - INTERVAL '10 days')`,
		other.Format("2006-01-02"))

	if err := computeLaneEconomics(ctx, db, day, day.AddDate(0, 0, 3)); err != nil {
		t.Fatalf("compute 1: %v", err)
	}
	first := readEconRow(t, db, day, "beta", "aol")

	// Same inputs, second run: same numbers, no duplicate rows.
	if err := computeLaneEconomics(ctx, db, day, day.AddDate(0, 0, 3)); err != nil {
		t.Fatalf("compute 2: %v", err)
	}
	second := readEconRow(t, db, day, "beta", "aol")
	if first.Messages != second.Messages || first.GrossECPM != second.GrossECPM {
		t.Errorf("recompute changed the numbers: %+v vs %+v", first, second)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM drip_lane_economics WHERE day = $1::date`,
		day.Format("2006-01-02")).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("rows for the day = %d, want 3 (alpha/aol, alpha/yahoo, beta/aol)", n)
	}

	// Lane beta stops mailing: its stale row must go, or the planner keeps
	// ranking on an eCPM the lane no longer earns.
	mustExec(t, db, `DELETE FROM mailing_campaign_isp_plans WHERE campaign_id = $1`, ids["C4"])
	mustExec(t, db, `DELETE FROM mailing_everflow_conversions WHERE campaign_id = $1`, ids["C4"])
	mustExec(t, db, `DELETE FROM drip_supply_ledger WHERE lane = 'beta'`)
	mustExec(t, db, `DELETE FROM mailing_campaigns WHERE id = $1`, ids["C4"])
	if err := computeLaneEconomics(ctx, db, day, day.AddDate(0, 0, 3)); err != nil {
		t.Fatalf("compute 3: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM drip_lane_economics WHERE day = $1::date AND lane = 'beta'`,
		day.Format("2006-01-02")).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("beta rows after it stopped mailing = %d, want 0 (the reap did not fire)", n)
	}

	// NEGATIVE CONTROL: the reap is scoped to the day it recomputed.
	if err := db.QueryRow(`SELECT COUNT(*) FROM drip_lane_economics WHERE day = $1::date AND lane = 'ghost'`,
		other.Format("2006-01-02")).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the reap deleted a row on another day (%d ghost rows left, want 1)", n)
	}
}

// -----------------------------------------------------------------------------
// 6. Manual revenue: spread, revision, and validation
// -----------------------------------------------------------------------------

func TestManualRevenueSpreadsAcrossItsWindow(t *testing.T) {
	db := newEconTestDB(t)
	day := econDay(t)
	ctx := context.Background()
	seedRates(t, db, nil)

	// One lane, one ISP, so the whole per-day slice lands on one row.
	seedCampaign(t, db, "[partner-drip] solo ht 20260910T010000 a b [ses:z]",
		day.Add(time.Hour), 1000, map[string]int{"aol": 1000})
	// The prior day too, so a day OUTSIDE the window can be checked.
	seedCampaign(t, db, "[partner-drip] solo ht 20260908T010000 a b [ses:z]",
		day.AddDate(0, 0, -2).Add(time.Hour), 1000, map[string]int{"aol": 1000})

	// $40 over 2026-09-09..2026-09-12 = 4 days ⇒ $10/day.
	mustExec(t, db, `INSERT INTO drip_manual_revenue
		(lane, revenue_date, attribution_start, attribution_end, amount, entered_by)
		VALUES ('solo', '2026-09-09', '2026-09-09', '2026-09-12', 40.00, 'operator')`)

	if err := computeLaneEconomics(ctx, db, day, day.AddDate(0, 0, 10)); err != nil {
		t.Fatalf("compute in-window day: %v", err)
	}
	near(t, "solo/aol manual revenue on an in-window day",
		readEconRow(t, db, day, "solo", "aol").RevenueManual, 10.0)

	// NEGATIVE CONTROL: 2026-09-08 is one day before the window opens and
	// must receive nothing.
	outside := day.AddDate(0, 0, -2)
	if err := computeLaneEconomics(ctx, db, outside, day.AddDate(0, 0, 10)); err != nil {
		t.Fatalf("compute out-of-window day: %v", err)
	}
	near(t, "solo/aol manual revenue outside the window",
		readEconRow(t, db, outside, "solo", "aol").RevenueManual, 0)
}

func TestManualRevenueRevisionSupersedesTheOriginal(t *testing.T) {
	db := newEconTestDB(t)
	day := econDay(t)
	ctx := context.Background()
	seedRates(t, db, nil)
	seedCampaign(t, db, "[partner-drip] solo ht 20260910T010000 a b [ses:z]",
		day.Add(time.Hour), 1000, map[string]int{"aol": 1000})

	orig, err := InsertManualRevenue(ctx, db, ManualRevenueEntry{
		Lane:             "solo",
		RevenueDate:      day,
		AttributionStart: day,
		AttributionEnd:   day,
		Amount:           100,
		EnteredBy:        "operator",
	})
	if err != nil {
		t.Fatalf("insert original: %v", err)
	}

	// NEGATIVE CONTROL: before the revision exists, the original counts.
	if err := computeLaneEconomics(ctx, db, day, day.AddDate(0, 0, 10)); err != nil {
		t.Fatal(err)
	}
	near(t, "manual revenue before the revision",
		readEconRow(t, db, day, "solo", "aol").RevenueManual, 100)

	if _, err := InsertManualRevenue(ctx, db, ManualRevenueEntry{
		Lane:             "solo",
		RevenueDate:      day,
		AttributionStart: day,
		AttributionEnd:   day,
		Amount:           60,
		EnteredBy:        "operator",
		RevisionOf:       &orig,
	}); err != nil {
		t.Fatalf("insert revision: %v", err)
	}

	if err := computeLaneEconomics(ctx, db, day, day.AddDate(0, 0, 10)); err != nil {
		t.Fatal(err)
	}
	// $60, not $160: a superseded entry stops contributing entirely.
	near(t, "manual revenue after the revision",
		readEconRow(t, db, day, "solo", "aol").RevenueManual, 60)
}

func TestInsertManualRevenueValidation(t *testing.T) {
	db := newEconTestDB(t)
	day := econDay(t)
	ctx := context.Background()

	valid := ManualRevenueEntry{
		Lane:             "solo",
		RevenueDate:      day,
		AttributionStart: day,
		AttributionEnd:   day.AddDate(0, 0, 2),
		Amount:           25.50,
		Source:           "cpm deal",
		Reference:        "INV-1",
		EnteredBy:        "drisan",
	}

	// POSITIVE CONTROL first: the valid entry is accepted and stored as sent.
	id, err := InsertManualRevenue(ctx, db, valid)
	if err != nil {
		t.Fatalf("a valid entry was rejected: %v", err)
	}
	var lane, enteredBy string
	var amount float64
	var start, end time.Time
	if err := db.QueryRow(`SELECT lane, entered_by, amount::float8, attribution_start, attribution_end
		FROM drip_manual_revenue WHERE id = $1`, id).Scan(&lane, &enteredBy, &amount, &start, &end); err != nil {
		t.Fatal(err)
	}
	if lane != "solo" || enteredBy != "drisan" || amount != 25.50 {
		t.Errorf("stored row = %s/%s/%v, want solo/drisan/25.5", lane, enteredBy, amount)
	}

	missing := uuid.New()
	for _, tc := range []struct {
		name  string
		field string
		mut   func(*ManualRevenueEntry)
	}{
		{"empty lane", "lane", func(e *ManualRevenueEntry) { e.Lane = "  " }},
		{"empty entered_by", "entered_by", func(e *ManualRevenueEntry) { e.EnteredBy = "" }},
		{"zero amount", "amount", func(e *ManualRevenueEntry) { e.Amount = 0 }},
		{"negative amount", "amount", func(e *ManualRevenueEntry) { e.Amount = -5 }},
		{"inverted window", "attribution_end", func(e *ManualRevenueEntry) {
			e.AttributionStart = day.AddDate(0, 0, 3)
			e.AttributionEnd = day
		}},
		{"revision_of names nothing", "revision_of", func(e *ManualRevenueEntry) { e.RevisionOf = &missing }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := valid
			tc.mut(&e)
			if _, err := InsertManualRevenue(ctx, db, e); err == nil {
				t.Fatalf("%s was accepted; validation did not fire", tc.name)
			} else if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error %q does not name the offending field %q", err, tc.field)
			}
			// Nothing was written.
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM drip_manual_revenue`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Errorf("a rejected entry still wrote a row (%d rows, want 1)", n)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// 7. RankInputs
// -----------------------------------------------------------------------------

// seedRank writes one drip_lane_economics row directly, so the rank window can
// be built without simulating fourteen days of sends.
func seedRank(t *testing.T, db *sql.DB, day time.Time, lane, isp string, messages, conversions int, revenue, sendCost float64) {
	t.Helper()
	mustExec(t, db, `INSERT INTO drip_lane_economics
		(day, lane, isp, messages, intros, conversions, revenue_everflow, send_cost, maturity, sample_ok, computed_at)
		VALUES ($1::date, $2, $3, $4, $4, $5, $6::numeric, $7::numeric, 'mature', TRUE, NOW())`,
		day.Format("2006-01-02"), lane, isp, messages, conversions, revenue, sendCost)
}

func TestRankInputsUsesMatureCohortsAndFallsBackToTheEstateMedian(t *testing.T) {
	db := newEconTestDB(t)
	today := econDay(t)
	ctx := context.Background()

	// Mature window for `today`: [today-13, today-7].
	inWindow := today.AddDate(0, 0, -10)

	// Three sampled lanes, so the median is the middle one.
	//   high:   revenue 300, send 0, messages 30,000 → 10.00 eCPM
	//   mid:    revenue 150, send 0, messages 30,000 →  5.00 eCPM
	//   low:    revenue  30, send 0, messages 30,000 →  1.00 eCPM
	seedRank(t, db, inWindow, "high", "aol", 30000, 9, 300, 0)
	seedRank(t, db, inWindow, "mid", "aol", 30000, 9, 150, 0)
	seedRank(t, db, inWindow, "low", "aol", 30000, 9, 30, 0)
	// Below sample: 100 messages, 1 conversion.
	seedRank(t, db, inWindow, "tiny", "aol", 100, 1, 90, 0)

	// OUTSIDE the window on the young side — a cohort 6 days old whose
	// attribution has not closed. It must not reach the rank at all.
	seedRank(t, db, today.AddDate(0, 0, -6), "fresh", "aol", 500000, 500, 999999, 0)

	got, err := RankInputs(ctx, db, today)
	if err != nil {
		t.Fatalf("RankInputs: %v", err)
	}

	if _, ok := got["fresh"]; ok {
		t.Error("a 6-day-old cohort reached the rank; only cohorts ≥ 7 days old are mature (§4)")
	}

	for lane, want := range map[string]float64{"high": 10, "mid": 5, "low": 1} {
		ri, ok := got[lane]
		if !ok {
			t.Fatalf("lane %s missing from the rank", lane)
		}
		if !ri.SampleOK {
			t.Errorf("%s: sample_ok = false with 30,000 messages", lane)
		}
		if ri.Fallback {
			t.Errorf("%s: Fallback = true for a lane that cleared the sample threshold", lane)
		}
		near(t, lane+" contribution_ecpm", ri.ContributionECPM, want)
	}

	tiny, ok := got["tiny"]
	if !ok {
		t.Fatal("lane tiny missing from the rank")
	}
	if tiny.SampleOK {
		t.Error("tiny: sample_ok = true with 100 messages and 1 conversion")
	}
	if !tiny.Fallback {
		t.Error("tiny: Fallback = false; a below-sample lane must inherit the estate median")
	}
	near(t, "tiny inherited contribution_ecpm", tiny.ContributionECPM, 5.0) // median(1, 5, 10)
	// Its own measurement is preserved alongside the inherited number, so
	// the UI can say "inherited" rather than presenting it as measured.
	near(t, "tiny observed contribution_ecpm", tiny.Observed, 900.0)

	// Window bounds are reported.
	if !tiny.WindowStart.Equal(today.AddDate(0, 0, -13)) || !tiny.WindowEnd.Equal(today.AddDate(0, 0, -7)) {
		t.Errorf("rank window = [%s, %s], want [%s, %s]",
			tiny.WindowStart.Format("2006-01-02"), tiny.WindowEnd.Format("2006-01-02"),
			today.AddDate(0, 0, -13).Format("2006-01-02"), today.AddDate(0, 0, -7).Format("2006-01-02"))
	}
}

func TestRankInputsSubtractsSendCost(t *testing.T) {
	db := newEconTestDB(t)
	today := econDay(t)
	inWindow := today.AddDate(0, 0, -8)

	// revenue 300, send 60, messages 30,000 → (300-60)/30 = 8.00 eCPM.
	seedRank(t, db, inWindow, "solo", "aol", 30000, 9, 300, 60)
	got, err := RankInputs(context.Background(), db, today)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "solo contribution_ecpm", got["solo"].ContributionECPM, 8.0)

	// NEGATIVE CONTROL: the GROSS number (10.00) is what you get if send cost
	// is dropped — the two must not be equal.
	if math.Abs(got["solo"].ContributionECPM-10.0) < 1e-9 {
		t.Error("contribution eCPM equals the gross eCPM; send cost is not being subtracted")
	}
}

func TestRankInputsWithNoSampledLaneYieldsZeroMedian(t *testing.T) {
	db := newEconTestDB(t)
	today := econDay(t)
	seedRank(t, db, today.AddDate(0, 0, -9), "tiny", "aol", 10, 0, 5, 0)

	got, err := RankInputs(context.Background(), db, today)
	if err != nil {
		t.Fatal(err)
	}
	ri := got["tiny"]
	if !ri.Fallback || ri.ContributionECPM != 0 {
		t.Errorf("with no sampled lane the fallback must be 0, got fallback=%v ecpm=%v", ri.Fallback, ri.ContributionECPM)
	}
}

// -----------------------------------------------------------------------------
// 8. Pure helpers (no database)
// -----------------------------------------------------------------------------

func TestManualRevenuePerDay(t *testing.T) {
	loc := econLoc(t)
	d := func(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 0, 0, 0, 0, loc) }
	for _, tc := range []struct {
		name       string
		start, end time.Time
		amount     float64
		want       float64
	}{
		{"single day", d(2026, 9, 10), d(2026, 9, 10), 30, 30},
		{"three days", d(2026, 9, 9), d(2026, 9, 11), 30, 10},
		{"thirty days", d(2026, 9, 1), d(2026, 9, 30), 300, 10},
	} {
		got := manualRevenueRow{Amount: tc.amount, AttributionStart: tc.start, AttributionEnd: tc.end}.perDay()
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: perDay = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMedian(t *testing.T) {
	for _, tc := range []struct {
		in   []float64
		want float64
	}{
		{nil, 0},
		{[]float64{7}, 7},
		{[]float64{10, 1, 5}, 5},
		{[]float64{4, 2, 8, 6}, 5},
		{[]float64{-3, 3}, 0},
	} {
		if got := median(tc.in); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("median(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestEconDayAge(t *testing.T) {
	loc := econLoc(t)
	day := time.Date(2026, 9, 10, 0, 0, 0, 0, loc)
	for _, tc := range []struct {
		now  time.Time
		want int
	}{
		{time.Date(2026, 9, 10, 23, 59, 0, 0, loc), 0},
		{time.Date(2026, 9, 16, 0, 20, 0, 0, loc), 6},
		{time.Date(2026, 9, 17, 0, 20, 0, 0, loc), 7},
	} {
		if got := econDayAge(day, tc.now); got != tc.want {
			t.Errorf("econDayAge(%s, %s) = %d, want %d", day.Format("01-02"), tc.now.Format("01-02 15:04"), got, tc.want)
		}
	}
}

func TestCleaningValueFormula(t *testing.T) {
	// intros 2000, messages 2000 all SES; 5 conversions worth $50;
	// 800 verdicts; $0.0005/record acquisition.
	//   revenue per record = (5/2000) × (50/5) = 0.025
	//   eo per record      = 0.000244 × (800/2000) = 0.0000976
	//   send per record    = 2000 × 0.0001 / 2000 = 0.0001
	//   cleaning           = 0.025 − 0.0005 − 0.0000976 − 0.0001 = 0.0243024
	got := cleaningValue(
		econFacts{Messages: 2000, Intros: 2000, SESMessages: 2000},
		econRevenue{Conversions: 5, Payout: 50},
		0, 800, 0.0005, 0.0001, 0, 0.000244)
	if math.Abs(got-0.0243024) > 1e-12 {
		t.Errorf("cleaningValue = %.12f, want 0.024302400000", got)
	}

	// No intros ⇒ nothing is per-record; the caller gates on SampleOK, and
	// this must not divide by zero.
	if got := cleaningValue(econFacts{}, econRevenue{}, 0, 0, 0, 0, 0, 0); got != 0 {
		t.Errorf("cleaningValue with 0 intros = %v, want 0", got)
	}
}

func TestNumStrKeepsTheDecimalForm(t *testing.T) {
	for in, want := range map[float64]string{
		0.000244: "0.000244",
		25.5:     "25.5",
		0:        "0",
	} {
		if got := numStr(in); got != want {
			t.Errorf("numStr(%v) = %q, want %q", in, got, want)
		}
	}
	if got := numStr(math.NaN()); got != "0" {
		t.Errorf("numStr(NaN) = %q, want \"0\" — NaN must never reach a NUMERIC column", got)
	}
	if got := numStr(math.Inf(1)); got != "0" {
		t.Errorf("numStr(+Inf) = %q, want \"0\"", got)
	}
}

// TestEconomicsWorkerKillSwitch proves DRIP_ECONOMICS_DISABLED=1 actually
// disables the worker, and — the negative control — that it runs without it.
func TestEconomicsWorkerKillSwitch(t *testing.T) {
	t.Setenv("DRIP_ECONOMICS_DISABLED", "1")
	if w := NewEconomicsWorker(nil, nil); !w.disabled {
		t.Error("DRIP_ECONOMICS_DISABLED=1 did not disable the worker")
	}
	// Start must return immediately on a nil DB rather than dereferencing it.
	NewEconomicsWorker(nil, nil).Start(context.Background())

	t.Setenv("DRIP_ECONOMICS_DISABLED", "")
	if w := NewEconomicsWorker(nil, nil); w.disabled {
		t.Error("NEGATIVE CONTROL: the worker is disabled with the kill switch unset")
	}
}

// TestEconomicsWorkerRunOnceCoversYesterdayPlusSeven pins the day set the
// nightly pass recomputes: yesterday and the seven days before it, oldest
// first, and never today (whose day is not over).
func TestEconomicsWorkerRunOnceCoversYesterdayPlusSeven(t *testing.T) {
	db := newEconTestDB(t)
	seedRates(t, db, nil)
	loc := econLoc(t)
	today := time.Date(2026, 9, 20, 0, 0, 0, 0, loc)

	// One campaign on each of the eleven days around the window.
	for i := 1; i <= 11; i++ {
		d := today.AddDate(0, 0, -i)
		seedCampaign(t, db, fmt.Sprintf("[partner-drip] solo ht %s a b [ses:z]", d.Format("20060102")),
			d.Add(time.Hour), 100, map[string]int{"aol": 100})
	}
	// And one on today, which the pass must NOT compute.
	seedCampaign(t, db, "[partner-drip] solo ht 20260920 a b [ses:z]",
		today.Add(time.Hour), 100, map[string]int{"aol": 100})

	w := NewEconomicsWorker(db, nil)
	w.loc = loc
	if err := w.RunOnce(context.Background(), today); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	var days []string
	rows, err := db.Query(`SELECT day::text FROM drip_lane_economics ORDER BY day`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		days = append(days, d)
	}
	want := []string{
		"2026-09-12", "2026-09-13", "2026-09-14", "2026-09-15",
		"2026-09-16", "2026-09-17", "2026-09-18", "2026-09-19",
	}
	if strings.Join(days, ",") != strings.Join(want, ",") {
		t.Errorf("computed days = %v, want %v (yesterday + the 7 before it, never today)", days, want)
	}
}
