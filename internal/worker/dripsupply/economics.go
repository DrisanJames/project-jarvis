package dripsupply

// REQ-118 WP8 — Economics (docs/DRIP_SUPPLY_CHAIN_DESIGN.md §4).
//
// Computes drip_lane_economics(day, lane, isp) nightly and exposes the two
// things the rest of the chain reads from it:
//
//   ComputeLaneEconomics — the nightly recompute for one Denver day.
//   RankInputs           — the per-lane mature contribution eCPM the WP6
//                          planner ranks discretionary intro demand by (§5.2
//                          rule 3), with the estate-median fallback for lanes
//                          below the §4 minimum sample.
//
// Plus InsertManualRevenue, the validated writer behind WP9's
// POST /api/mailing/supply/manual-revenue.
//
// ─────────────────────────────────────────────────────────────────────────
// WHERE THE NUMBERS COME FROM (every claim below was verified against the
// production database on 2026-09-03 through the read-only
// agents/dbknowledge layer; the evidence is quoted at each site)
// ─────────────────────────────────────────────────────────────────────────
//
// MESSAGES. The partner-drip executor names every campaign it deploys
//
//	[partner-drip] <lane> <brand> <ts> <hash> <hash> [ses:<profile>] [tN]
//
// so lane = split_part(name,' ',2), the transport is the presence of the
// `[ses:` marker, and the touch class is the `[tN]` suffix (absent = intro).
// Verified over the last 3 days: 12,198 of 12,201 drip campaigns carry
// `[ses:` (1,104,173 of 1,104,175 messages), and the touch suffix splits
// 4,535 intro campaigns / 7,666 follow-up campaigns.
//
// THE ISP SPLIT IS NOT `mailing_campaign_isp_plans.sent_count`. That column
// is DEAD — nothing maintains it:
//
//	SELECT COUNT(*), SUM(sent_count), MAX(sent_count),
//	       SUM(audience_selected_count), SUM(enqueued_count)
//	  FROM mailing_campaign_isp_plans
//	 WHERE created_at >= NOW() - INTERVAL '7 days';
//	→ 105,870 plans | sum=0 | max=0 | sel=32,338,493 | enq=13,625,169
//
// Every one of 105,870 plan rows in a week reads 0. Summing it would make
// every eCPM infinite (revenue ÷ 0) or, worse, silently zero. So the plan
// supplies the SHAPE (audience_selected_count as the split ratio) and
// mailing_campaigns.sent_count supplies the LEVEL — the "tracking = shape,
// counters = level" rule the wcl_remail cap work landed on.
//
// A drip campaign with no plan rows at all contributes nothing, and that
// loses nothing: over 3 days the 158 plan-less drip campaigns carry
// sent_count = 0 and total_recipients = 0 (verified). They are skipped and
// counted in the log line, never silently.
//
// REVENUE IS COUNTED UNJOINED. mailing_everflow_conversions.campaign_id is
// the attribution key and the query NEVER joins mailing_offers: three
// mailing_offers rows share everflow_offer_id '162' in production
//
//	SELECT everflow_offer_id, COUNT(*) FROM mailing_offers
//	 WHERE everflow_offer_id IS NOT NULL GROUP BY 1 HAVING COUNT(*) > 1;
//	→ '162' → 3   (also '5990' → 4, '329'/'9539'/'8738' → 2)
//
// so an offer join fans every EF-162 conversion ×3 and triples the lane's
// revenue. TestUnjoinedConversionCounting proves both halves of that.
//
// The conversion carries no ISP, so lane revenue is split across the lane's
// ISPs by the SAME message share that forms the denominator — which is what
// makes a per-ISP eCPM well defined at all.
//
// VERDICTS come from the Supply Ledger's VALIDATION_ORDERED events (§1.2).
// The ledger ships dark until WP7 writes to it, so while it is empty this
// falls back to partner_clean_queue.validated_at per vertical × isp_family
// (index idx_pcq_validated_at) and LOGS that it did — there is no column on
// drip_lane_economics saying which source was used, so the log line is the
// only record and it is unconditional.
//
// COSTS come from drip_cost_rates (§1.2 seeds). infra_monthly_usd seeds
// NULL (§9 decision 6), and NULL propagates: infra_share and
// fully_loaded_ecpm are written NULL rather than 0, because a fully-loaded
// eCPM that silently omits infrastructure is a number the operator would
// act on believing it complete.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
)

// -----------------------------------------------------------------------------
// DDL — the §4 shape, kept next to its readers
// -----------------------------------------------------------------------------
//
// VERBATIM copies of the WP1 statements in cmd/server/main.go
// (dripSupplyMigrations). WP1 owns the production copy; these exist so the
// integration tests build the PRODUCTION shape — CHECK constraints,
// nullability and defaults included — and so a WP1/WP8 drift surfaces as a
// failing test here rather than as a 3am constraint violation. Same contract
// as CapacityLedgerDDL in balance.go. Keep them byte-identical.

// LaneEconomicsDDL is `drip_lane_economics` (§4).
const LaneEconomicsDDL = `CREATE TABLE IF NOT EXISTS drip_lane_economics (
		day               DATE NOT NULL,                          -- Denver day of the campaign's scheduled_at
		lane              TEXT NOT NULL,                          -- split_part(campaign name, ' ', 2)
		isp               TEXT NOT NULL,
		messages          INT  NOT NULL DEFAULT 0,                -- intros + followups
		intros            INT  NOT NULL DEFAULT 0,
		followups         INT  NOT NULL DEFAULT 0,
		verdicts          INT  NOT NULL DEFAULT 0,                -- EO verdicts ordered for this lane x ISP that day
		conversions       INT  NOT NULL DEFAULT 0,
		revenue_everflow  NUMERIC NOT NULL DEFAULT 0,
		revenue_manual    NUMERIC NOT NULL DEFAULT 0,
		send_cost         NUMERIC NOT NULL DEFAULT 0,
		eo_cost           NUMERIC NOT NULL DEFAULT 0,
		acquisition_cost  NUMERIC NOT NULL DEFAULT 0,
		infra_share       NUMERIC,                                -- NULL until infra_monthly_usd is set (§9 decision 6)
		gross_ecpm        NUMERIC NOT NULL DEFAULT 0,             -- revenue / messages x 1000
		contribution_ecpm NUMERIC NOT NULL DEFAULT 0,             -- (revenue - send) / messages x 1000
		cleaning_value    NUMERIC,                                -- NULL when sample_ok is false
		fully_loaded_ecpm NUMERIC,                                -- NULL when infra_share is NULL
		maturity          TEXT NOT NULL DEFAULT 'incomplete'
			CHECK (maturity IN ('mature','incomplete')),
		sample_ok         BOOLEAN NOT NULL DEFAULT FALSE,
		computed_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (day, lane, isp)
	)`

// ManualRevenueDDL is `drip_manual_revenue` (§1.2) — audited operator entries
// for lanes whose revenue lives outside Everflow.
const ManualRevenueDDL = `CREATE TABLE IF NOT EXISTS drip_manual_revenue (
		id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		lane               TEXT NOT NULL,
		revenue_date       DATE NOT NULL,
		attribution_start  DATE NOT NULL,
		attribution_end    DATE NOT NULL,
		amount             NUMERIC NOT NULL,
		source             TEXT NOT NULL DEFAULT '',
		reference          TEXT NOT NULL DEFAULT '',
		entered_by         TEXT NOT NULL DEFAULT '',
		entered_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		revision_of        UUID
	)`

// CostRatesDDL is `drip_cost_rates` (§1.2).
const CostRatesDDL = `CREATE TABLE IF NOT EXISTS drip_cost_rates (
		key        TEXT PRIMARY KEY,
		value      NUMERIC,                                       -- NULL = not yet allocated (infra_monthly_usd)
		unit       TEXT NOT NULL DEFAULT '',
		updated_by TEXT NOT NULL DEFAULT '',
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`

// -----------------------------------------------------------------------------
// Constants
// -----------------------------------------------------------------------------

// Cost-rate keys (§1.2 seeds).
const (
	RateEOPerVerdict     = "eo_per_verdict"
	RateEOListPerVerdict = "eo_list_per_verdict"
	RateSESPerMessage    = "ses_per_message"
	RatePMTAPerMessage   = "pmta_per_message"
	RateInfraMonthlyUSD  = "infra_monthly_usd"
)

const (
	// EconAttributionDays is §4's attribution window, and therefore both the
	// maturity threshold (a day younger than this is `incomplete`) and the
	// upper bound on how long after a send a conversion still counts toward
	// that send's cohort.
	EconAttributionDays = 7

	// EconTrailingDays is the trailing window sample_ok and cleaning_value
	// are measured over, ending on (and including) the day being computed.
	EconTrailingDays = 7

	// §4 minimum sample for a rank: 20k messages OR 5 conversions.
	EconSampleMinMessages    = 20000
	EconSampleMinConversions = 5

	// econInfraDaysPerMonth is §4's literal divisor: infra_monthly_usd / 30.
	// A calendar-accurate divisor would make the same month's days carry
	// different infra shares for no analytic gain.
	econInfraDaysPerMonth = 30.0

	MaturityMature     = "mature"
	MaturityIncomplete = "incomplete"
)

// -----------------------------------------------------------------------------
// Types
// -----------------------------------------------------------------------------

// EconomicsDB is the database surface the economics layer needs. *sql.DB
// satisfies it; the upsert needs a transaction, which Queryer alone cannot
// open (same split as ContractQueryer / ContractTxBeginner in contracts.go).
type EconomicsDB interface {
	Queryer
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// econKey is one row's grain.
type econKey struct{ Lane, ISP string }

// econFacts is the message side of a lane×ISP over some window. Everything is
// float64 because the ISP split is fractional (a campaign's sent_count times
// that ISP's share of audience_selected_count); it is rounded exactly once,
// at write time.
type econFacts struct {
	Messages     float64
	Intros       float64
	Followups    float64
	SESMessages  float64
	PMTAMessages float64
}

// econRevenue is the Everflow side of a lane×ISP over some window.
type econRevenue struct {
	Conversions float64
	Payout      float64
}

// LaneEconomics is one drip_lane_economics row.
type LaneEconomics struct {
	Day       time.Time
	Lane      string
	ISP       string
	Messages  int
	Intros    int
	Followups int
	Verdicts  int

	Conversions     int
	RevenueEverflow float64
	RevenueManual   float64

	SendCost        float64
	EOCost          float64
	AcquisitionCost float64
	InfraShare      *float64 // NULL when infra_monthly_usd is not set

	GrossECPM        float64
	ContributionECPM float64
	CleaningValue    *float64 // NULL when SampleOK is false
	FullyLoadedECPM  *float64 // NULL when InfraShare is NULL

	Maturity string
	SampleOK bool
}

// Revenue is the §4 revenue term: Everflow payout plus audited manual entries.
func (e LaneEconomics) Revenue() float64 { return e.RevenueEverflow + e.RevenueManual }

// RankInput is one lane's planner input (§5.2 rule 3): the mature 7-day
// contribution eCPM, or the estate median when the lane is below §4's minimum
// sample.
type RankInput struct {
	Lane string

	// ContributionECPM is what the planner ranks on. When Fallback is true
	// this is the estate median, not the lane's own number.
	ContributionECPM float64

	// Observed is the lane's own measured contribution eCPM over the window,
	// whether or not it cleared the sample threshold. Kept separate so the UI
	// can show "inherited $7.60 (own sample: 2 conversions)" rather than
	// presenting a borrowed number as measured.
	Observed    float64
	Messages    int
	Conversions int
	SampleOK    bool
	Fallback    bool

	// WindowStart/WindowEnd are the inclusive Denver day bounds of the mature
	// cohort window this was measured over.
	WindowStart time.Time
	WindowEnd   time.Time
}

// ManualRevenueEntry is one audited operator revenue entry (§1.2).
type ManualRevenueEntry struct {
	Lane             string
	RevenueDate      time.Time
	AttributionStart time.Time
	AttributionEnd   time.Time
	Amount           float64
	Source           string
	Reference        string
	EnteredBy        string
	// RevisionOf, when set, must name an existing drip_manual_revenue row.
	// Entries are never edited in place; a correction is a new row pointing
	// at the one it supersedes, and the superseded row stops contributing.
	RevisionOf *uuid.UUID
}

// econManualRevenueKind is the ValidationError subject for manual-revenue
// entries. ValidationError is reused rather than duplicated so WP9's handler
// renders these with the same code path as a bad contract save; ContractKind
// is a bare string type, and nothing dispatches on unknown kinds.
const econManualRevenueKind = ContractKind("manual_revenue")

// -----------------------------------------------------------------------------
// ComputeLaneEconomics
// -----------------------------------------------------------------------------

// ComputeLaneEconomics recomputes drip_lane_economics for one Denver day.
//
// `day` is Denver-anchored, the package convention (bucket.go:23): every
// boundary is derived from day.Location(), so this never loads tzdata and
// never guesses which day the caller meant.
//
// Idempotent: re-running it for the same day rewrites that day's rows and
// reaps any lane×ISP that no longer has data (a lane that stopped mailing
// must not leave last week's eCPM sitting on the planner's input).
func ComputeLaneEconomics(ctx context.Context, db EconomicsDB, day time.Time) error {
	return computeLaneEconomics(ctx, db, day, time.Now())
}

// computeLaneEconomics is ComputeLaneEconomics with an injected clock, so the
// maturity rule can be tested without waiting seven days.
func computeLaneEconomics(ctx context.Context, db EconomicsDB, day, now time.Time) error {
	day = dayOf(day)
	dayStart, dayEnd := dayWindow(day)

	// The trailing window is EconTrailingDays Denver days ENDING ON `day`
	// inclusive, so a day is always inside its own sample.
	trailStart := day.AddDate(0, 0, -(EconTrailingDays - 1))
	trailStartTS, _ := dayWindow(trailStart)

	rates, err := loadCostRates(ctx, db)
	if err != nil {
		return fmt.Errorf("dripsupply: load cost rates: %w", err)
	}
	sesRate := rateOrZero(rates, RateSESPerMessage)
	pmtaRate := rateOrZero(rates, RatePMTAPerMessage)
	eoRate := rateOrZero(rates, RateEOPerVerdict)
	infraMonthly := rates[RateInfraMonthlyUSD] // *float64; nil ⇒ NULL propagates

	dayFacts, skipped, err := loadEconFacts(ctx, db, dayStart, dayEnd)
	if err != nil {
		return fmt.Errorf("dripsupply: load message facts for %s: %w", day.Format("2006-01-02"), err)
	}
	trailFacts, _, err := loadEconFacts(ctx, db, trailStartTS, dayEnd)
	if err != nil {
		return fmt.Errorf("dripsupply: load trailing message facts: %w", err)
	}

	dayRev, err := loadEconEverflow(ctx, db, dayStart, dayEnd, EconAttributionDays)
	if err != nil {
		return fmt.Errorf("dripsupply: load conversions for %s: %w", day.Format("2006-01-02"), err)
	}
	trailRev, err := loadEconEverflow(ctx, db, trailStartTS, dayEnd, EconAttributionDays)
	if err != nil {
		return fmt.Errorf("dripsupply: load trailing conversions: %w", err)
	}

	dayVerdicts, src, err := loadEconVerdicts(ctx, db, dayStart, dayEnd)
	if err != nil {
		return fmt.Errorf("dripsupply: load verdicts for %s: %w", day.Format("2006-01-02"), err)
	}
	trailVerdicts, _, err := loadEconVerdicts(ctx, db, trailStartTS, dayEnd)
	if err != nil {
		return fmt.Errorf("dripsupply: load trailing verdicts: %w", err)
	}

	acq, err := loadAcquisitionCosts(ctx, db, dayEnd)
	if err != nil {
		return fmt.Errorf("dripsupply: load acquisition costs: %w", err)
	}

	// Loaded over the WHOLE trailing window, not just the day: cleaning_value
	// needs the trailing manual revenue too, and an entry covering day-5 but
	// not `day` would otherwise be invisible to it.
	manual, err := loadManualRevenue(ctx, db, trailStart, day)
	if err != nil {
		return fmt.Errorf("dripsupply: load manual revenue for %s: %w", day.Format("2006-01-02"), err)
	}
	manualToday := coveringDay(manual, day)

	// Manual revenue is lane-grained; spread it across the lane's ISPs by the
	// day's message share, falling back to the trailing window's share when
	// the lane did not mail that day. A lane with neither is LOGGED, never
	// silently dropped — drip_manual_revenue stays the source of truth.
	manualByKey := spreadManualRevenue(manualToday, dayFacts, trailFacts)

	// Estate denominator for the infra share (§4). Rows sum to
	// infra_monthly_usd/30 across the estate for the day, by construction.
	var estateMessages float64
	for _, f := range dayFacts {
		estateMessages += f.Messages
	}

	maturity := MaturityIncomplete
	if econDayAge(day, now) >= EconAttributionDays {
		maturity = MaturityMature
	}

	// Union of every key that has ANY signal for the day. A lane×ISP that
	// spent EO and mailed nothing gets a row: that is exactly the cell the
	// supply controller is over-ordering into.
	keys := map[econKey]bool{}
	for k := range dayFacts {
		keys[k] = true
	}
	for k := range dayVerdicts {
		keys[k] = true
	}
	for k := range manualByKey {
		keys[k] = true
	}
	for k := range dayRev {
		keys[k] = true
	}

	rows := make([]LaneEconomics, 0, len(keys))
	for k := range keys {
		f := dayFacts[k]
		rev := dayRev[k]

		row := LaneEconomics{
			Day:             day,
			Lane:            k.Lane,
			ISP:             k.ISP,
			Messages:        roundInt(f.Messages),
			Intros:          roundInt(f.Intros),
			Followups:       roundInt(f.Followups),
			Verdicts:        dayVerdicts[k],
			Conversions:     roundInt(rev.Conversions),
			RevenueEverflow: rev.Payout,
			RevenueManual:   manualByKey[k],
			Maturity:        maturity,
		}

		// Costs. Send cost is per transport (§4): the campaign's `[ses:`
		// marker decides which rate each message is billed at.
		row.SendCost = f.SESMessages*sesRate + f.PMTAMessages*pmtaRate
		row.EOCost = float64(row.Verdicts) * eoRate
		row.AcquisitionCost = f.Intros * acq[k.Lane]

		if infraMonthly != nil && estateMessages > 0 {
			share := *infraMonthly / econInfraDaysPerMonth * (f.Messages / estateMessages)
			row.InfraShare = &share
		}

		// The three eCPMs (§4). Messages is the denominator for all of them;
		// with no messages there is no per-mille anything, and 0 is the
		// honest value (there is no revenue either).
		revenue := row.Revenue()
		if f.Messages > 0 {
			row.GrossECPM = revenue / f.Messages * 1000
			row.ContributionECPM = (revenue - row.SendCost) / f.Messages * 1000
			if row.InfraShare != nil {
				fl := (revenue - row.SendCost - row.EOCost - row.AcquisitionCost - *row.InfraShare) / f.Messages * 1000
				row.FullyLoadedECPM = &fl
			}
		}

		// Sample and cleaning value, both measured over the trailing window.
		tf := trailFacts[k]
		tr := trailRev[k]
		tm := trailingManualFor(k, manual, day, trailFacts)
		row.SampleOK = tf.Messages >= EconSampleMinMessages || tr.Conversions >= EconSampleMinConversions
		if row.SampleOK && tf.Intros > 0 {
			cv := cleaningValue(tf, tr, tm, trailVerdicts[k], acq[k.Lane], sesRate, pmtaRate, eoRate)
			row.CleaningValue = &cv
		}

		rows = append(rows, row)
	}

	// Deterministic write order: the upsert is one row at a time, and a
	// stable order keeps the integration test's diffs readable and any
	// deadlock impossible between the nightly job and a manual re-run.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Lane != rows[j].Lane {
			return rows[i].Lane < rows[j].Lane
		}
		return rows[i].ISP < rows[j].ISP
	})

	// The write stamp is the REAL wall clock, never the injected `now`: the
	// reap below deletes the day's rows this run did not rewrite, keyed on
	// computed_at < stamp, so the stamp must advance on every run. (`now` is
	// injected only to make the maturity rule testable, and a test that
	// re-runs with the same `now` would otherwise leave stale rows behind —
	// which is exactly how this was caught.) computed_at is also the
	// freshness the §6 UI labels every number with, and that must be wall
	// clock too.
	if err := upsertLaneEconomics(ctx, db, day, rows, time.Now()); err != nil {
		return err
	}

	log.Printf("[DripEconomics] day=%s rows=%d messages=%.0f verdict_source=%s manual_entries=%d plan_less_campaigns_skipped=%d maturity=%s",
		day.Format("2006-01-02"), len(rows), estateMessages, src, len(manual), skipped, maturity)
	return nil
}

// econDayAge is how many whole Denver days separate `day` from `now`.
func econDayAge(day, now time.Time) int {
	today := dayOf(now.In(day.Location()))
	return int(today.Sub(day).Hours() / 24)
}

// cleaningValue is §4's "expected revenue per raw record − acquisition −
// expected EO ÷ yield − expected send cost over the ladder", every term
// measured over the trailing window and expressed PER INTRO (an intro is one
// raw record entering the ladder).
//
//	expected revenue per record = conversion_rate × revenue_per_conversion
//	                              + manual revenue per record
//	expected EO ÷ yield         = eo_per_verdict × verdicts per record
//	                              (verdicts ÷ intros IS 1/yield, measured)
//	expected send over ladder   = trailing send cost ÷ intros
//	                              (= per-message rate × messages per intro)
func cleaningValue(tf econFacts, tr econRevenue, trailManual float64, trailVerdicts int,
	acqPerRecord, sesRate, pmtaRate, eoRate float64) float64 {

	if tf.Intros <= 0 {
		return 0
	}
	convRate := tr.Conversions / tf.Intros
	revPerConv := 0.0
	if tr.Conversions > 0 {
		revPerConv = tr.Payout / tr.Conversions
	}
	revPerRecord := convRate*revPerConv + trailManual/tf.Intros

	eoPerRecord := eoRate * (float64(trailVerdicts) / tf.Intros)
	sendPerRecord := (tf.SESMessages*sesRate + tf.PMTAMessages*pmtaRate) / tf.Intros

	return revPerRecord - acqPerRecord - eoPerRecord - sendPerRecord
}

// trailingManualFor spreads every manual entry whose attribution window
// overlaps the trailing window onto one lane×ISP key, using the trailing
// window's message share. Same rule as the day's spread, one window wider.
func trailingManualFor(k econKey, entries []manualRevenueRow, day time.Time, trail map[econKey]econFacts) float64 {
	trailStart := day.AddDate(0, 0, -(EconTrailingDays - 1))

	var laneMessages float64
	for kk, f := range trail {
		if kk.Lane == k.Lane {
			laneMessages += f.Messages
		}
	}
	if laneMessages <= 0 {
		return 0
	}
	share := trail[k].Messages / laneMessages

	var total float64
	for _, e := range entries {
		if e.Lane != k.Lane {
			continue
		}
		perDay := e.perDay()
		// Count one perDay slice for each trailing day the entry covers.
		for d := trailStart; !d.After(day); d = d.AddDate(0, 0, 1) {
			if !d.Before(e.AttributionStart) && !d.After(e.AttributionEnd) {
				total += perDay
			}
		}
	}
	return total * share
}

// spreadManualRevenue turns lane-grained manual entries into lane×ISP amounts
// for ONE day. Order of preference for the split: the day's own message share,
// then the trailing window's. A lane with neither is logged at ERROR — the
// money must never disappear quietly.
func spreadManualRevenue(entries []manualRevenueRow, dayFacts, trailFacts map[econKey]econFacts) map[econKey]float64 {
	out := map[econKey]float64{}
	if len(entries) == 0 {
		return out
	}

	shareFor := func(lane string) map[string]float64 {
		for _, src := range []map[econKey]econFacts{dayFacts, trailFacts} {
			total := 0.0
			per := map[string]float64{}
			for k, f := range src {
				if k.Lane == lane {
					per[k.ISP] += f.Messages
					total += f.Messages
				}
			}
			if total > 0 {
				for isp := range per {
					per[isp] /= total
				}
				return per
			}
		}
		return nil
	}

	for _, e := range entries {
		shares := shareFor(e.Lane)
		if shares == nil {
			log.Printf("[DripEconomics] ERROR manual revenue $%.2f for lane %q (entry %s) has no message share to attribute to — the lane mailed nothing in the day or the trailing window; the row stands in drip_manual_revenue and is NOT in drip_lane_economics",
				e.Amount, e.Lane, e.ID)
			continue
		}
		perDay := e.perDay()
		for isp, s := range shares {
			out[econKey{Lane: e.Lane, ISP: isp}] += perDay * s
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// Loaders
// -----------------------------------------------------------------------------

// econCampaignCTE is the shared campaign/plan projection. It is a string
// constant rather than a view so the whole query is visible at the call site
// and the two readers cannot drift.
//
// $1/$2 are the window's [start, end) instants (Denver day boundaries computed
// in Go — NEVER an `AT TIME ZONE` cast in the WHERE clause, which throws away
// the index on scheduled_at).
//
// The share expression is the ratio of the plan's audience_selected_count to
// the campaign's total. When that total is 0 (the plan rows exist but carry no
// selection) the campaign is split EVENLY across its plans rather than dropped:
// dropping would silently delete real messages from the denominator.
const econCampaignCTE = `
WITH camp AS (
    SELECT c.id,
           split_part(c.name, ' ', 2)                             AS lane,
           c.sent_count                                           AS sent,
           (substring(c.name from '\[t([0-9]+)\]$') IS NOT NULL)   AS is_followup,
           (c.name ~ '\[ses:')                                    AS is_ses
      FROM mailing_campaigns c
     WHERE c.name LIKE '[partner-drip] %'
       AND c.scheduled_at >= $1 AND c.scheduled_at < $2
       AND c.sent_count > 0
),
plan AS (
    SELECT p.campaign_id,
           lower(COALESCE(NULLIF(p.isp, ''), 'other'))            AS isp,
           GREATEST(p.audience_selected_count, 0)::numeric        AS sel
      FROM mailing_campaign_isp_plans p
      JOIN camp ON camp.id = p.campaign_id
),
tot AS (
    SELECT campaign_id, SUM(sel) AS s, COUNT(*) AS n FROM plan GROUP BY 1
)`

const econFactsSQL = econCampaignCTE + `
SELECT camp.lane,
       plan.isp,
       SUM(camp.sent * sh.share)::float8                                           AS messages,
       SUM(CASE WHEN camp.is_followup THEN 0 ELSE camp.sent * sh.share END)::float8 AS intros,
       SUM(CASE WHEN camp.is_followup THEN camp.sent * sh.share ELSE 0 END)::float8 AS followups,
       SUM(CASE WHEN camp.is_ses THEN camp.sent * sh.share ELSE 0 END)::float8      AS ses_messages,
       SUM(CASE WHEN camp.is_ses THEN 0 ELSE camp.sent * sh.share END)::float8      AS pmta_messages
  FROM camp
  JOIN plan ON plan.campaign_id = camp.id
  JOIN tot  ON tot.campaign_id  = camp.id
  CROSS JOIN LATERAL (
      SELECT CASE WHEN tot.s > 0 THEN plan.sel / tot.s ELSE 1.0 / tot.n END AS share
  ) sh
 GROUP BY 1, 2`

// econPlanlessSQL counts the drip campaigns in the window that have NO plan
// rows, so the log can say how many were skipped instead of the number being
// invisible. Verified on prod 2026-09-03: over 3 days all 158 such campaigns
// carry sent_count = 0 and total_recipients = 0, so skipping them loses no
// messages — but the count is reported every night in case that changes.
const econPlanlessSQL = `
SELECT COUNT(*)
  FROM mailing_campaigns c
 WHERE c.name LIKE '[partner-drip] %'
   AND c.scheduled_at >= $1 AND c.scheduled_at < $2
   AND NOT EXISTS (SELECT 1 FROM mailing_campaign_isp_plans p WHERE p.campaign_id = c.id)`

func loadEconFacts(ctx context.Context, db Queryer, from, to time.Time) (map[econKey]econFacts, int, error) {
	out := map[econKey]econFacts{}
	rows, err := db.QueryContext(ctx, econFactsSQL, from, to)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var k econKey
		var f econFacts
		if err := rows.Scan(&k.Lane, &k.ISP, &f.Messages, &f.Intros, &f.Followups, &f.SESMessages, &f.PMTAMessages); err != nil {
			return nil, 0, err
		}
		k.Lane = strings.TrimSpace(k.Lane)
		k.ISP = normISP(k.ISP)
		if k.Lane == "" {
			// A name that does not parse to a lane is a data defect, not a
			// lane called "". Never fold it into another lane's economics.
			log.Printf("[DripEconomics] WARNING dropped %.0f messages with an unparseable lane token", f.Messages)
			continue
		}
		out[k] = f
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	var planless int
	if err := db.QueryRowContext(ctx, econPlanlessSQL, from, to).Scan(&planless); err != nil {
		return nil, 0, err
	}
	return out, planless, nil
}

// econEverflowSQL attributes conversions by campaign_id and NEVER joins
// mailing_offers (the EF-162 ×3 fan-out — see the file header). The payout is
// split across the campaign's ISPs by the same share as its messages, because
// a conversion row carries no ISP of its own.
//
// $3 is the attribution window in days: a conversion counts toward the send
// day's cohort when it lands within [day, day + $3).
const econEverflowSQL = econCampaignCTE + `,
conv AS (
    SELECT ec.campaign_id,
           COUNT(*)::numeric                       AS n,
           COALESCE(SUM(ec.payout), 0)::numeric    AS payout
      FROM mailing_everflow_conversions ec
      JOIN camp ON camp.id = ec.campaign_id
     WHERE ec.converted_at >= $1
       AND ec.converted_at <  ($2::timestamptz + ($3::text || ' days')::interval)
     GROUP BY 1
)
SELECT camp.lane,
       plan.isp,
       SUM(conv.n * sh.share)::float8      AS conversions,
       SUM(conv.payout * sh.share)::float8 AS payout
  FROM camp
  JOIN plan ON plan.campaign_id = camp.id
  JOIN tot  ON tot.campaign_id  = camp.id
  JOIN conv ON conv.campaign_id = camp.id
  CROSS JOIN LATERAL (
      SELECT CASE WHEN tot.s > 0 THEN plan.sel / tot.s ELSE 1.0 / tot.n END AS share
  ) sh
 GROUP BY 1, 2`

func loadEconEverflow(ctx context.Context, db Queryer, from, to time.Time, attributionDays int) (map[econKey]econRevenue, error) {
	out := map[econKey]econRevenue{}
	rows, err := db.QueryContext(ctx, econEverflowSQL, from, to, strconv.Itoa(attributionDays))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k econKey
		var r econRevenue
		if err := rows.Scan(&k.Lane, &k.ISP, &r.Conversions, &r.Payout); err != nil {
			return nil, err
		}
		k.Lane = strings.TrimSpace(k.Lane)
		k.ISP = normISP(k.ISP)
		if k.Lane == "" {
			continue
		}
		out[k] = r
	}
	return out, rows.Err()
}

const econVerdictLedgerSQL = `
SELECT lane, lower(COALESCE(NULLIF(isp, ''), 'other')) AS isp, COALESCE(SUM(quantity), 0)
  FROM drip_supply_ledger
 WHERE event = 'VALIDATION_ORDERED'
   AND occurred_at >= $1 AND occurred_at < $2
 GROUP BY 1, 2`

// econVerdictPCQFallbackSQL is the fallback while the Supply Ledger is empty
// (WP7 has not started writing VALIDATION_ORDERED yet). partner_clean_queue's
// vertical IS the lane and isp_family IS the ISP, and idx_pcq_validated_at
// makes the range scan cheap.
const econVerdictPCQFallbackSQL = `
SELECT COALESCE(NULLIF(vertical, ''), '')                    AS lane,
       lower(COALESCE(NULLIF(isp_family, ''), 'other'))       AS isp,
       COUNT(*)
  FROM partner_clean_queue
 WHERE validated_at >= $1 AND validated_at < $2
 GROUP BY 1, 2`

// loadEconVerdicts returns verdicts per lane×ISP and the source it used
// ("supply_ledger" | "pcq_fallback"). drip_lane_economics has no column
// recording the source, so the returned value is logged unconditionally by the
// caller — an inferior source must never be invisible.
func loadEconVerdicts(ctx context.Context, db Queryer, from, to time.Time) (map[econKey]int, string, error) {
	read := func(query string) (map[econKey]int, int, error) {
		out := map[econKey]int{}
		total := 0
		rows, err := db.QueryContext(ctx, query, from, to)
		if err != nil {
			return nil, 0, err
		}
		defer rows.Close()
		for rows.Next() {
			var k econKey
			var n int
			if err := rows.Scan(&k.Lane, &k.ISP, &n); err != nil {
				return nil, 0, err
			}
			k.Lane = strings.TrimSpace(k.Lane)
			k.ISP = normISP(k.ISP)
			if k.Lane == "" {
				continue
			}
			out[k] += n
			total += n
		}
		return out, total, rows.Err()
	}

	ledger, total, err := read(econVerdictLedgerSQL)
	if err != nil {
		return nil, "", err
	}
	if total > 0 {
		return ledger, "supply_ledger", nil
	}

	pcq, _, err := read(econVerdictPCQFallbackSQL)
	if err != nil {
		return nil, "", err
	}
	return pcq, "pcq_fallback", nil
}

// econAcquisitionSQL resolves a lane's per-record acquisition cost from the
// active source contracts (§1.1 drip_source_contracts) via
// partner_datasets.slug → .vertical. A lane with several sources takes the
// mean, and a lane with no active source contract costs 0 — the §1.1 default
// and the honest value before §7 step 1 normalizes the contract set.
const econAcquisitionSQL = `
SELECT pd.vertical AS lane, AVG(sc.unit_acquisition_cost)::float8
  FROM drip_source_contracts sc
  JOIN partner_datasets pd ON pd.slug = sc.source_slug
 WHERE sc.status = 'active'
   AND sc.effective_at < $1
   AND COALESCE(NULLIF(pd.vertical, ''), '') <> ''
 GROUP BY 1`

func loadAcquisitionCosts(ctx context.Context, db Queryer, before time.Time) (map[string]float64, error) {
	out := map[string]float64{}
	rows, err := db.QueryContext(ctx, econAcquisitionSQL, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var lane string
		var cost float64
		if err := rows.Scan(&lane, &cost); err != nil {
			return nil, err
		}
		out[strings.TrimSpace(lane)] = cost
	}
	return out, rows.Err()
}

func loadCostRates(ctx context.Context, db Queryer) (map[string]*float64, error) {
	out := map[string]*float64{}
	rows, err := db.QueryContext(ctx, `SELECT key, value::float8 FROM drip_cost_rates`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var v sql.NullFloat64
		if err := rows.Scan(&key, &v); err != nil {
			return nil, err
		}
		if v.Valid {
			f := v.Float64
			out[key] = &f
		} else {
			out[key] = nil
		}
	}
	return out, rows.Err()
}

func rateOrZero(rates map[string]*float64, key string) float64 {
	if v, ok := rates[key]; ok && v != nil {
		return *v
	}
	return 0
}

// manualRevenueRow is one live (non-superseded) drip_manual_revenue entry.
type manualRevenueRow struct {
	ID               uuid.UUID
	Lane             string
	Amount           float64
	AttributionStart time.Time
	AttributionEnd   time.Time
}

// perDay is the entry's amount spread evenly across every day its attribution
// window covers, inclusive of both ends (§1.2: the window is a date range, so
// a one-day window is start == end and carries the whole amount).
func (m manualRevenueRow) perDay() float64 {
	days := int(m.AttributionEnd.Sub(m.AttributionStart).Hours()/24) + 1
	if days < 1 {
		days = 1
	}
	return m.Amount / float64(days)
}

// econManualRevenueSQL reads every manual entry whose attribution window
// OVERLAPS [$1, $2], excluding any entry that a later revision supersedes (a
// correction is a new row with revision_of pointing at the old one; counting
// both would double the revenue).
const econManualRevenueSQL = `
SELECT m.id, m.lane, m.amount::float8, m.attribution_start, m.attribution_end
  FROM drip_manual_revenue m
 WHERE m.attribution_start <= $2::date
   AND m.attribution_end   >= $1::date
   AND NOT EXISTS (SELECT 1 FROM drip_manual_revenue r WHERE r.revision_of = m.id)`

// loadManualRevenue reads the live entries overlapping the inclusive Denver
// day range [from, to].
func loadManualRevenue(ctx context.Context, db Queryer, from, to time.Time) ([]manualRevenueRow, error) {
	rows, err := db.QueryContext(ctx, econManualRevenueSQL, from.Format("2006-01-02"), to.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []manualRevenueRow
	for rows.Next() {
		var m manualRevenueRow
		if err := rows.Scan(&m.ID, &m.Lane, &m.Amount, &m.AttributionStart, &m.AttributionEnd); err != nil {
			return nil, err
		}
		m.Lane = strings.TrimSpace(m.Lane)
		// Re-anchor the DATE columns to the caller's Denver location so the
		// day-by-day walk in trailingManualFor compares like with like.
		m.AttributionStart = reanchor(m.AttributionStart, to.Location())
		m.AttributionEnd = reanchor(m.AttributionEnd, to.Location())
		out = append(out, m)
	}
	return out, rows.Err()
}

// coveringDay is the subset of entries whose attribution window covers `day`.
func coveringDay(entries []manualRevenueRow, day time.Time) []manualRevenueRow {
	var out []manualRevenueRow
	for _, e := range entries {
		if !day.Before(e.AttributionStart) && !day.After(e.AttributionEnd) {
			out = append(out, e)
		}
	}
	return out
}

// reanchor rebuilds a DATE-valued timestamp at midnight in loc. lib/pq hands
// DATE back as a UTC-midnight time.Time; comparing that to a Denver-anchored
// day would shift every boundary by the UTC offset.
func reanchor(t time.Time, loc *time.Location) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// -----------------------------------------------------------------------------
// Upsert
// -----------------------------------------------------------------------------

const econUpsertSQL = `
INSERT INTO drip_lane_economics (
    day, lane, isp, messages, intros, followups, verdicts, conversions,
    revenue_everflow, revenue_manual, send_cost, eo_cost, acquisition_cost,
    infra_share, gross_ecpm, contribution_ecpm, cleaning_value,
    fully_loaded_ecpm, maturity, sample_ok, computed_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8,
    $9::numeric, $10::numeric, $11::numeric, $12::numeric, $13::numeric,
    $14::numeric, $15::numeric, $16::numeric, $17::numeric,
    $18::numeric, $19, $20, $21
)
ON CONFLICT (day, lane, isp) DO UPDATE SET
    messages          = EXCLUDED.messages,
    intros            = EXCLUDED.intros,
    followups         = EXCLUDED.followups,
    verdicts          = EXCLUDED.verdicts,
    conversions       = EXCLUDED.conversions,
    revenue_everflow  = EXCLUDED.revenue_everflow,
    revenue_manual    = EXCLUDED.revenue_manual,
    send_cost         = EXCLUDED.send_cost,
    eo_cost           = EXCLUDED.eo_cost,
    acquisition_cost  = EXCLUDED.acquisition_cost,
    infra_share       = EXCLUDED.infra_share,
    gross_ecpm        = EXCLUDED.gross_ecpm,
    contribution_ecpm = EXCLUDED.contribution_ecpm,
    cleaning_value    = EXCLUDED.cleaning_value,
    fully_loaded_ecpm = EXCLUDED.fully_loaded_ecpm,
    maturity          = EXCLUDED.maturity,
    sample_ok         = EXCLUDED.sample_ok,
    computed_at       = EXCLUDED.computed_at`

// econReapSQL removes rows for the day that this run did not write. A lane
// that stopped mailing must not leave a stale eCPM on the planner's input, and
// a run stamp is a cheaper reaper than diffing key sets. Both statements run
// in ONE transaction, so a reader either sees the whole previous snapshot or
// the whole new one.
const econReapSQL = `DELETE FROM drip_lane_economics WHERE day = $1 AND computed_at < $2`

func upsertLaneEconomics(ctx context.Context, db EconomicsDB, day time.Time, rows []LaneEconomics, stamp time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dripsupply: begin economics upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	dayStr := day.Format("2006-01-02")
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, econUpsertSQL,
			dayStr, r.Lane, r.ISP, r.Messages, r.Intros, r.Followups, r.Verdicts, r.Conversions,
			numStr(r.RevenueEverflow), numStr(r.RevenueManual), numStr(r.SendCost), numStr(r.EOCost),
			numStr(r.AcquisitionCost), numPtr(r.InfraShare), numStr(r.GrossECPM), numStr(r.ContributionECPM),
			numPtr(r.CleaningValue), numPtr(r.FullyLoadedECPM), r.Maturity, r.SampleOK, stamp,
		); err != nil {
			return fmt.Errorf("dripsupply: upsert economics %s/%s/%s: %w", dayStr, r.Lane, r.ISP, err)
		}
	}
	if _, err := tx.ExecContext(ctx, econReapSQL, dayStr, stamp); err != nil {
		return fmt.Errorf("dripsupply: reap stale economics for %s: %w", dayStr, err)
	}
	return tx.Commit()
}

// numStr formats a float for a ::numeric parameter. Going through the decimal
// text form rather than binding a float64 keeps the value the database stores
// exactly the value computed here, with no float→numeric round trip.
func numStr(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "0"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func numPtr(v *float64) any {
	if v == nil {
		return nil
	}
	return numStr(*v)
}

func roundInt(v float64) int {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return int(math.Round(v))
}

// -----------------------------------------------------------------------------
// RankInputs — what the WP6 planner consumes
// -----------------------------------------------------------------------------

// econRankSQL aggregates the MATURE cohort window per lane.
//
// The window is the EconTrailingDays Denver days ending EconAttributionDays
// before `day` — i.e. only cohorts that are at least 7 days old, whose 7-day
// attribution window has closed (§4).
//
// It filters on the DAY RANGE, not on the stored `maturity` label. The label
// is derived from exactly this rule at compute time, so the range is the
// stronger test — and a row that aged past 7 days but has not been recomputed
// yet still carries `incomplete`, which would silently starve the lane of its
// own history. (The nightly job recomputes the 7 prior days precisely so the
// label catches up; the rank must not depend on that having happened.)
const econRankSQL = `
SELECT lane,
       COALESCE(SUM(messages), 0)                                   AS messages,
       COALESCE(SUM(conversions), 0)                                AS conversions,
       COALESCE(SUM(revenue_everflow + revenue_manual), 0)::float8   AS revenue,
       COALESCE(SUM(send_cost), 0)::float8                           AS send_cost
  FROM drip_lane_economics
 WHERE day >= $1::date AND day <= $2::date
 GROUP BY 1`

// RankInputs returns, per lane, the mature 7-day dispatch contribution eCPM
// the planner ranks on (§5.2 rule 3). Lanes below §4's minimum sample (20k
// messages or 5 conversions over the window) inherit the estate median rather
// than a number their own volume cannot support, and say so via Fallback.
//
// §4 specifies "the estate median for its record class". Record class lives on
// drip_source_contracts, which is empty until §7 step 1 normalizes the contract
// set; until then the fallback is the estate-wide median, which is what the
// planner ranks against today. Narrowing it to record class is a one-line
// change here once the contracts exist.
func RankInputs(ctx context.Context, db Queryer, day time.Time) (map[string]RankInput, error) {
	day = dayOf(day)
	windowEnd := day.AddDate(0, 0, -EconAttributionDays)
	windowStart := windowEnd.AddDate(0, 0, -(EconTrailingDays - 1))

	rows, err := db.QueryContext(ctx, econRankSQL,
		windowStart.Format("2006-01-02"), windowEnd.Format("2006-01-02"))
	if err != nil {
		return nil, fmt.Errorf("dripsupply: rank inputs: %w", err)
	}
	defer rows.Close()

	out := map[string]RankInput{}
	var sampled []float64
	for rows.Next() {
		var lane string
		var messages, conversions int
		var revenue, sendCost float64
		if err := rows.Scan(&lane, &messages, &conversions, &revenue, &sendCost); err != nil {
			return nil, err
		}
		lane = strings.TrimSpace(lane)
		if lane == "" {
			continue
		}
		ri := RankInput{
			Lane:        lane,
			Messages:    messages,
			Conversions: conversions,
			SampleOK:    messages >= EconSampleMinMessages || conversions >= EconSampleMinConversions,
			WindowStart: windowStart,
			WindowEnd:   windowEnd,
		}
		if messages > 0 {
			ri.Observed = (revenue - sendCost) / float64(messages) * 1000
		}
		ri.ContributionECPM = ri.Observed
		if ri.SampleOK {
			sampled = append(sampled, ri.Observed)
		}
		out[lane] = ri
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	med := median(sampled)
	for lane, ri := range out {
		if !ri.SampleOK {
			ri.ContributionECPM = med
			ri.Fallback = true
			out[lane] = ri
		}
	}
	return out, nil
}

// median of an unsorted slice; 0 for an empty one (no sampled lane means there
// is no estate signal to inherit, and 0 ranks a below-sample lane neither above
// nor below a break-even one).
func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// -----------------------------------------------------------------------------
// InsertManualRevenue — the validated writer behind WP9's POST handler
// -----------------------------------------------------------------------------

// InsertManualRevenue validates and inserts one audited manual revenue entry
// (§1.2 drip_manual_revenue, §3 POST /api/mailing/supply/manual-revenue) and
// returns the new row's id.
//
// Validation is here, not only in the handler, so the rule holds for every
// caller: an unvalidated row is money the economics layer will spread across
// days and lanes with no way to tell it was wrong.
func InsertManualRevenue(ctx context.Context, db Queryer, e ManualRevenueEntry) (uuid.UUID, error) {
	var errs ValidationErrors
	bad := func(field, msg string) {
		errs = append(errs, &ValidationError{
			Kind:    econManualRevenueKind,
			Subject: strings.TrimSpace(e.Lane),
			Field:   field,
			Msg:     msg,
		})
	}

	lane := strings.TrimSpace(e.Lane)
	if lane == "" {
		bad("lane", "required: an entry with no lane cannot be attributed to anything")
	}
	if strings.TrimSpace(e.EnteredBy) == "" {
		bad("entered_by", "required: manual revenue is an audited entry and must name who entered it")
	}
	if !(e.Amount > 0) {
		bad("amount", fmt.Sprintf("must be > 0, got %v; a correction is a new row with revision_of, never a negative amount", e.Amount))
	}
	if math.IsNaN(e.Amount) || math.IsInf(e.Amount, 0) {
		bad("amount", "must be a finite number")
	}
	start, end := dayOf(e.AttributionStart), dayOf(e.AttributionEnd)
	if e.AttributionStart.IsZero() || e.AttributionEnd.IsZero() {
		bad("attribution_start", "required: the window decides which days the amount is spread across")
	} else if end.Before(start) {
		bad("attribution_end", fmt.Sprintf("attribution_start (%s) must be on or before attribution_end (%s)",
			start.Format("2006-01-02"), end.Format("2006-01-02")))
	}
	if e.RevisionOf != nil {
		var exists bool
		if err := db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM drip_manual_revenue WHERE id = $1)`, *e.RevisionOf).Scan(&exists); err != nil {
			return uuid.Nil, fmt.Errorf("dripsupply: check revision_of: %w", err)
		}
		if !exists {
			bad("revision_of", fmt.Sprintf("no drip_manual_revenue row %s: a revision must supersede an entry that exists, or the superseded row keeps counting", *e.RevisionOf))
		}
	}
	if len(errs) > 0 {
		return uuid.Nil, errs
	}

	revenueDate := dayOf(e.RevenueDate)
	if e.RevenueDate.IsZero() {
		revenueDate = start
	}

	var revisionOf any
	if e.RevisionOf != nil {
		revisionOf = *e.RevisionOf
	}

	var id uuid.UUID
	err := db.QueryRowContext(ctx, `
		INSERT INTO drip_manual_revenue
			(lane, revenue_date, attribution_start, attribution_end, amount,
			 source, reference, entered_by, revision_of)
		VALUES ($1, $2::date, $3::date, $4::date, $5::numeric, $6, $7, $8, $9)
		RETURNING id`,
		lane,
		revenueDate.Format("2006-01-02"),
		start.Format("2006-01-02"),
		end.Format("2006-01-02"),
		numStr(e.Amount),
		strings.TrimSpace(e.Source),
		strings.TrimSpace(e.Reference),
		strings.TrimSpace(e.EnteredBy),
		revisionOf,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("dripsupply: insert manual revenue: %w", err)
	}
	return id, nil
}

// -----------------------------------------------------------------------------
// EconomicsWorker — the nightly job (started from cmd/server/main.go)
// -----------------------------------------------------------------------------

// EconomicsWorker recomputes drip_lane_economics every night at 00:20 America/
// Denver: yesterday, plus the seven days before it.
//
// The re-computation of the prior seven days is not belt-and-braces, it is the
// point. Two things move after a day is first computed:
//
//  1. MATURITY. A day is `incomplete` until it is EconAttributionDays old and
//     then becomes `mature` (§4). Nothing else flips that label, so without a
//     nightly re-pass every cohort would sit at `incomplete` forever and
//     RankInputs would rank against a window that never fills.
//  2. LATE REVENUE. Everflow postbacks land inside the same 7-day window
//     (mailing_everflow_conversions is also topped up by the 6h conversions
//     sync), so a day's revenue is not final on the night it is first written.
//
// 00:20 MT sits AFTER the day boundary and BEFORE the WP6 planner's 00:05
// next-day pass reads yesterday's numbers on the following night, and clear of
// the 23:50 sweep and the 01:01 clicker anchor.
//
// Kill switch: DRIP_ECONOMICS_DISABLED=1.
type EconomicsWorker struct {
	db    *sql.DB
	redis *redis.Client
	loc   *time.Location

	disabled bool
	interval time.Duration // poll cadence; the run is gated on the Denver clock
	runAfter time.Duration // how far past Denver midnight the run fires
	backfill int           // days BEFORE yesterday that are recomputed

	nowFn func() time.Time

	// lastRanDay is the Denver day ("2006-01-02") whose 00:20 boundary this
	// process has already served. Advanced only on success, so a failed pass
	// retries on the next tick and stays loud in the log rather than being
	// silently skipped until tomorrow.
	lastRanDay string
}

// economicsLockKey is the distributed lock every instance contends on. Two
// orchestrator instances run (desiredCount=2), and the upsert is transactional,
// but a lock keeps the two from doing the same eight days of work twice.
const economicsLockKey = "drip:economics:nightly"

// NewEconomicsWorker builds the nightly economics worker. redisClient may be
// nil; distlock then falls back to a PG advisory lock (session-scoped through
// the pool — the weaker of the two, which is why every sibling worker passes
// Redis when it has one).
func NewEconomicsWorker(db *sql.DB, redisClient *redis.Client) *EconomicsWorker {
	w := &EconomicsWorker{
		db:       db,
		redis:    redisClient,
		interval: 10 * time.Minute,
		runAfter: 20 * time.Minute,
		backfill: EconAttributionDays,
		nowFn:    time.Now,
	}
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		log.Printf("[DripEconomics] America/Denver unavailable (%v) — falling back to UTC day boundaries", err)
		loc = time.UTC
	}
	w.loc = loc
	if v := os.Getenv("DRIP_ECONOMICS_DISABLED"); v == "1" || strings.EqualFold(v, "true") {
		log.Println("[DripEconomics] DRIP_ECONOMICS_DISABLED set — nightly economics worker disabled")
		w.disabled = true
	}
	return w
}

// Start polls every `interval` and fires once per Denver day, at or after
// 00:20 MT. Single goroutine; honors ctx.Done().
func (w *EconomicsWorker) Start(ctx context.Context) {
	if w.disabled {
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

// tick fires the day's pass if the Denver clock is past the run time and this
// process has not already completed today's pass.
func (w *EconomicsWorker) tick(ctx context.Context) {
	now := w.nowFn().In(w.loc)
	today := dayOf(now)
	dayKey := today.Format("2006-01-02")
	if now.Before(today.Add(w.runAfter)) {
		return
	}
	if w.lastRanDay == dayKey {
		return
	}

	lock := distlock.NewLock(w.redis, w.db, economicsLockKey, 30*time.Minute)
	ok, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[DripEconomics] lock acquire failed: %v", err)
		return
	}
	if !ok {
		// The other instance is running this pass. Do NOT advance lastRanDay:
		// if that instance dies mid-pass this one retries on the next tick.
		return
	}
	defer func() {
		relCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := lock.Release(relCtx); err != nil {
			log.Printf("[DripEconomics] lock release failed: %v", err)
		}
	}()

	if err := w.RunOnce(ctx, today); err != nil {
		log.Printf("[DripEconomics] nightly pass for %s FAILED: %v (will retry in %s)", dayKey, err, w.interval)
		return
	}
	w.lastRanDay = dayKey
}

// RunOnce recomputes yesterday plus the `backfill` days before it, relative to
// the Denver day `today`. Oldest first, so a partial failure leaves the most
// recent days as the ones still to do — the days the planner reads first.
//
// Exported so an operator (and WP9's API) can force a recompute without waiting
// for midnight.
func (w *EconomicsWorker) RunOnce(ctx context.Context, today time.Time) error {
	today = dayOf(today.In(w.loc))
	var firstErr error
	for i := w.backfill; i >= 0; i-- {
		day := today.AddDate(0, 0, -(i + 1))
		if err := ComputeLaneEconomics(ctx, w.db, day); err != nil {
			log.Printf("[DripEconomics] day %s failed: %v", day.Format("2006-01-02"), err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
