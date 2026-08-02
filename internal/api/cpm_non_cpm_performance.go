package api

// NON-CPM PERFORMANCE — calendar-month funnel: VOLUME → CLICKERS → CONVERSIONS
// → PAYOUT → eCPM, for every offer, split by whether a CPM deal bills it.
// (operator 2026-08-01, rebuilt from the trailing-window first cut.)
//
// ─── Why this is Postgres and calendar-scoped (operator ruling 2026-08-01) ───
// The first cut read the trailing 7d/30d offer-alignment lake snapshot. Three
// things were wrong with that for this question:
//   1. The operator budgets and reviews by CALENDAR MONTH; a trailing-30d
//      number is not "August", and the CPM rows directly above this table on
//      the same screen are PG + calendar month. Two clocks on one screen is
//      how the two-source confusion starts (METRIC_CONTRACT).
//   2. The snapshot lags up to 3h, which is useless for checking MTD during a
//      send day.
//   3. Conversions were structurally unreachable — see the next section.
//
// ─── Identity: ONE cascade, used by every metric here ───────────────────────
// Grouping on mailing_campaigns.offer_key alone loses most of the estate:
// measured 2026-08-01 over the trailing 31 days, only 63,843 of 88,640
// campaigns (72%) carry an offer_key, and Liberty Mutual — the single largest
// non-CPM offer — was MISSING from the volume table entirely because its
// August campaigns carry offer_id but no offer_key. nonCpmIdentityExpr
// cascades offer_key → the offer's name → '(unattributed)', which recovered
// Liberty (168 clickers, the top row). Every query in this file MUST use it,
// or the volume / click / conversion rows will not line up on the same key.
//
// ─── Conversions: why the old path always returned 0 ────────────────────────
// The trailing-window version counted conversions via
// offer_key → mailing_offer_slug_map → everflow_offer_id → suppressions. That
// chain breaks twice in live data (verified 2026-08-01):
//   * No slug-map row at all for iwchelocv1..4, liberty-mutual, metal-roofing,
//     tahiti-village, sams-club, newsletter, fidelity — efids came back empty
//     and fetchAlignmentConversionsByISP short-circuits to zero.
//   * Where a row DOES exist the id disagrees with the ledger: offer_key
//     kqckq7 maps to everflow id 1090, but Liberty's conversions are recorded
//     under 338.
// Conversions are now attributed the same way the deal cards do it — by the
// CONVERSION'S OWN campaign/offer linkage, resolved through the same identity
// cascade — so no dictionary can silently drop them.
//
// ─── Money ──────────────────────────────────────────────────────────────────
// mailing_offers.payout is 0.00 on 27 of 34 offers and cannot be the revenue
// basis. The supplied payout lives per-conversion in
// mailing_everflow_conversions.payout (August: $100 across 2 of 3 conversions).
// So:
//     revenue = SUM(everflow payout)      -- only conversions that carry one
//     eCPM    = revenue / delivered * 1000
// conversions is the LEDGER COUNT (mailing_offer_suppressions reason
// 'converted'), which is the same count of record the CPM deal cards use and
// is more complete than the payout ledger (July: 45 vs 19). Because those two
// differ, PayoutCoverage is reported alongside — how many of the counted
// conversions actually carried a payout — so a low eCPM reads as "payout not
// supplied yet", never as "this offer earned nothing".
//
// ─── Cost ───────────────────────────────────────────────────────────────────
// Measured on prod 2026-08-01: the delivered aggregate costs ~55s for a SINGLE
// day and grows linearly across a month — far past the 30s request budget. It
// is therefore rolled up per Denver day into mailing_offer_day_rollup, computed
// once per day and never recomputed for closed days; a month is a cheap SUM.
// Clicks/clickers (~6.9s for the month) and conversions (tiny) stay LIVE and
// exact — a month's distinct clickers cannot be summed from daily distincts
// without double-counting anyone who clicked on two days.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// nonCpmIdentityExpr is THE offer identity for this surface. Requires the
// query to alias mailing_campaigns as c and LEFT JOIN mailing_offers as o.
const nonCpmIdentityExpr = `lower(COALESCE(NULLIF(c.offer_key,''), NULLIF(o.name,''), '(unattributed)'))`

// nonCpmUnattributed is the identity for delivery we cannot tie to any offer.
// Kept as a visible row rather than filtered out: it is ~28% of campaigns and
// hiding it would make the funnel look complete when it is not.
const nonCpmUnattributed = "(unattributed)"

const (
	// nonCpmRollupInterval — how often the open (today/yesterday) days are
	// recomputed. Closed days are immutable and computed once.
	nonCpmRollupInterval = 20 * time.Minute
	// nonCpmRollupDayTimeout — one day's delivered aggregate needs well over
	// the pooled 30s statement_timeout (~55s measured); scoped per day via
	// SET LOCAL on a dedicated connection, same trick as loadAllDealEventCounts.
	nonCpmRollupDayTimeout = 300
	// nonCpmRollupBackfillDays bounds how far back the worker will fill missing
	// days, so a cold table cannot run an unbounded backfill.
	nonCpmRollupBackfillDays = 75
	// nonCpmDealMapTTL — how long the identity→deal map is cached in memory.
	// It is a ~16.5s campaign-wide pass and only moves when deals or offer
	// wiring change, so a short TTL keeps the page responsive without staleness
	// that matters.
	nonCpmDealMapTTL = 5 * time.Minute
)

// ─── Schema ─────────────────────────────────────────────────────────────────

// nonCpmTableDDL is applied from ensureTables (synchronously, before the routes
// are registered) — runStartupMigrations runs in a background goroutine and
// cannot be relied on to land before the handler serves.
func nonCpmTableDDL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS mailing_offer_day_rollup (
			organization_id UUID NOT NULL,
			day             DATE NOT NULL,
			offer_identity  TEXT NOT NULL,
			delivered       BIGINT NOT NULL DEFAULT 0,
			refreshed_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (organization_id, day, offer_identity)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_offer_day_rollup_org_day
			ON mailing_offer_day_rollup(organization_id, day)`,
		// Per (Denver day, CPM deal) delivered — see cpmDealRollupInsertSQL for
		// why the live month scan had to go.
		`CREATE TABLE IF NOT EXISTS mailing_cpm_deal_day_rollup (
			organization_id UUID NOT NULL,
			day             DATE NOT NULL,
			deal_id         UUID NOT NULL,
			delivered       BIGINT NOT NULL DEFAULT 0,
			refreshed_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (organization_id, day, deal_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_cpm_deal_day_rollup_org_day
			ON mailing_cpm_deal_day_rollup(organization_id, day)`,
		// Operator-editable grouping — the "tag" that collapses creative
		// variants and duplicate keys onto one advertiser row (iwchelocv1..4 =
		// West Capital; liberty-mutual + kqckq7 + the offer-name form = Liberty
		// Mutual). source='seed' rows may be re-seeded on boot; source='operator'
		// rows are NEVER overwritten.
		`CREATE TABLE IF NOT EXISTS mailing_offer_groups (
			organization_id UUID NOT NULL,
			offer_identity  TEXT NOT NULL,
			group_name      TEXT NOT NULL,
			source          TEXT NOT NULL DEFAULT 'seed',
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (organization_id, offer_identity)
		)`,
		// CARRY-OVER (operator 2026-08-01, West Capital HELOC): some advertisers
		// run a single running conversion tally rather than resetting each
		// calendar month. For those, CONVERSIONS and REVENUE accumulate from the
		// offer's first conversion through the end of the selected month, while
		// VOLUME and CLICKERS stay strictly month-scoped (they are pacing
		// numbers, not settlement numbers). The row is flagged in the API so the
		// screen can show that the conversion column has a different time basis.
		`ALTER TABLE mailing_offer_groups ADD COLUMN IF NOT EXISTS carry_over BOOLEAN NOT NULL DEFAULT FALSE`,
	}
}

// nonCpmSeedGroups maps the identities observed on prod 2026-08-01 onto their
// advertiser. Applied with ON CONFLICT DO NOTHING so an operator edit always
// wins and re-running is a no-op. Unseeded identities group under themselves.
var nonCpmSeedGroups = map[string]string{
	"iwchelocv1": "West Capital HELOC", "iwchelocv2": "West Capital HELOC",
	"iwchelocv3": "West Capital HELOC", "iwchelocv4": "West Capital HELOC",
	"west capital heloc (iwchelocv1)": "West Capital HELOC",

	"liberty-mutual": "Liberty Mutual", "kqckq7": "Liberty Mutual",
	"liberty mutual insurance": "Liberty Mutual",

	"ps8241": "Sam's Club", "sams-club": "Sam's Club",
	"sam's club membership":                       "Sam's Club",
	"sam's club membership - partner drip (4989)": "Sam's Club",

	"3lks16": "Get Metal Roofing", "s6wff5": "Get Metal Roofing",
	"metal-roofing": "Get Metal Roofing", "get metal roofing": "Get Metal Roofing",
	"get metal roofing (ef 9539)": "Get Metal Roofing",

	"ctcdkm2": "Tahiti Village Resort", "tahiti-village": "Tahiti Village Resort",
	"tahiti village resort": "Tahiti Village Resort",

	"2j2crs": "Fidelity Life", "fidelity": "Fidelity Life",
	"fidelity life rapidecision": "Fidelity Life",
	"fidelity life insurance":    "Fidelity Life",

	"3mznpr": "Empire Today Flooring", "xf1sr2cs": "Empire Today Flooring",
	"empire today flooring": "Empire Today Flooring", "empire-flooring": "Empire Today Flooring",

	"3qqg71": "A Place for Mom", "a place for mom": "A Place for Mom",
	"k9tm4q": "3 Day Blinds", "3 day blinds": "3 Day Blinds",
	"cl38pfr": "CarShield", "carshield auto warranty": "CarShield",
	"j345ssd": "Optima Tax Relief", "optima tax relief": "Optima Tax Relief",
	"2hh43pb": "National Debt Relief", "national debt relief": "National Debt Relief",
	"newsletter": "Newsletter (no offer)",
}

// nonCpmCarryOverGroups are the advertisers whose conversion tally carries
// across months. Applied on every boot (idempotent) because it is a commercial
// term of the deal, not an operator preference to be preserved.
var nonCpmCarryOverGroups = []string{"West Capital HELOC"}

func (h *CpmPlannerHandlers) seedNonCpmGroups() {
	for identity, group := range nonCpmSeedGroups {
		if _, err := h.db.Exec(`
			INSERT INTO mailing_offer_groups (organization_id, offer_identity, group_name, source)
			SELECT id, $1, $2, 'seed' FROM organizations
			ON CONFLICT (organization_id, offer_identity) DO NOTHING`, identity, group); err != nil {
			// organizations may not exist in every deployment shape; fall back
			// to the single-tenant org rather than failing the boot path.
			if _, err2 := h.db.Exec(`
				INSERT INTO mailing_offer_groups (organization_id, offer_identity, group_name, source)
				VALUES ($1::uuid, $2, $3, 'seed')
				ON CONFLICT (organization_id, offer_identity) DO NOTHING`,
				SingleTenantFallbackOrgID, identity, group); err2 != nil {
				log.Printf("[CpmPlanner] seed offer group %q: %v / %v", identity, err, err2)
				return // the table is unusable; one log line, not one per key
			}
		}
	}
	for _, g := range nonCpmCarryOverGroups {
		if _, err := h.db.Exec(
			`UPDATE mailing_offer_groups SET carry_over = TRUE WHERE group_name = $1 AND carry_over = FALSE`, g); err != nil {
			log.Printf("[CpmPlanner] seed carry-over %q: %v", g, err)
		}
	}
}

// ─── Daily delivered rollup ─────────────────────────────────────────────────

// refreshNonCpmDay recomputes ONE Denver day's delivered-by-identity rollup.
// DELETE+INSERT in a single tx so a re-run is idempotent and an identity that
// stopped appearing does not linger. Runs on a dedicated connection with a
// per-transaction statement_timeout (the aggregate needs ~55s; the pool default
// is 30s).
func (h *CpmPlannerHandlers) refreshNonCpmDay(ctx context.Context, orgID string, day time.Time) error {
	conn, err := h.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("rollup conn: %w", err)
	}
	defer conn.Close()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rollup begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = '%ds'`, nonCpmRollupDayTimeout)); err != nil {
		return fmt.Errorf("rollup timeout override: %w", err)
	}
	d := day.Format("2006-01-02")
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM mailing_offer_day_rollup WHERE organization_id = $1 AND day = $2::date`, orgID, d); err != nil {
		return fmt.Errorf("rollup delete %s: %w", d, err)
	}
	// The bare event_at bounds are the partition-pruning predicate (a Denver
	// day straddles two UTC days); the AT TIME ZONE equality is the exact edge.
	if _, err := tx.ExecContext(ctx, nonCpmRollupInsertSQL(), orgID, d); err != nil {
		return fmt.Errorf("rollup insert %s: %w", d, err)
	}
	// Same day, same transaction: the per-DEAL rollup that feeds CPM pacing.
	// Kept together so the two surfaces can never disagree about which days
	// are rolled up.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM mailing_cpm_deal_day_rollup WHERE organization_id = $1 AND day = $2::date`, orgID, d); err != nil {
		return fmt.Errorf("deal rollup delete %s: %w", d, err)
	}
	if _, err := tx.ExecContext(ctx, cpmDealRollupInsertSQL(), orgID, d); err != nil {
		return fmt.Errorf("deal rollup insert %s: %w", d, err)
	}
	return tx.Commit()
}

// nonCpmRollupLoop keeps the rollup current: the two OPEN days (today and
// yesterday — late-arriving events still land against them) are recomputed
// every cycle, and missing closed days are backfilled oldest-first, ONE per
// cycle so a cold start cannot monopolise the DB.
//
// Process-lifetime goroutine, same posture as eventCacheLoop. Re-run safe: each
// day is an independent DELETE+INSERT keyed by (org, day).
func (h *CpmPlannerHandlers) nonCpmRollupLoop() {
	for {
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()

			orgs := []string{}
			rows, err := h.db.QueryContext(ctx, `SELECT DISTINCT organization_id::text FROM mailing_cpm_deals`)
			if err != nil {
				log.Printf("[CpmPlanner] non-cpm rollup org scan: %v", err)
				return
			}
			for rows.Next() {
				var o string
				if err := rows.Scan(&o); err == nil {
					orgs = append(orgs, o)
				}
			}
			rows.Close()

			for _, org := range orgs {
				var today time.Time
				if err := h.db.QueryRowContext(ctx,
					`SELECT (NOW() AT TIME ZONE 'America/Denver')::date`).Scan(&today); err != nil {
					log.Printf("[CpmPlanner] non-cpm rollup today: %v", err)
					return
				}
				// Open days first — these are what the operator is watching.
				for _, off := range []int{0, -1} {
					day := today.AddDate(0, 0, off)
					started := time.Now()
					if err := h.refreshNonCpmDay(ctx, org, day); err != nil {
						log.Printf("[CpmPlanner] non-cpm rollup %s %s: %v", org, day.Format("2006-01-02"), err)
						continue
					}
					log.Printf("[CpmPlanner] non-cpm rollup %s in %s", day.Format("2006-01-02"), time.Since(started).Round(time.Second))
				}
				// One missing closed day per cycle, oldest first.
				var missing sql.NullTime
				if err := h.db.QueryRowContext(ctx, `
					SELECT d::date FROM generate_series(
						(NOW() AT TIME ZONE 'America/Denver')::date - $2::int,
						(NOW() AT TIME ZONE 'America/Denver')::date - 2, '1 day') d
					WHERE NOT EXISTS (
						SELECT 1 FROM mailing_offer_day_rollup r
						WHERE r.organization_id = $1 AND r.day = d::date)
					ORDER BY d LIMIT 1`, org, nonCpmRollupBackfillDays).Scan(&missing); err == nil && missing.Valid {
					started := time.Now()
					if err := h.refreshNonCpmDay(ctx, org, missing.Time); err != nil {
						log.Printf("[CpmPlanner] non-cpm backfill %s: %v", missing.Time.Format("2006-01-02"), err)
					} else {
						log.Printf("[CpmPlanner] non-cpm backfilled %s in %s", missing.Time.Format("2006-01-02"), time.Since(started).Round(time.Second))
					}
				}
			}
		}()
		time.Sleep(nonCpmRollupInterval)
	}
}

// ─── SQL, one definition site each ──────────────────────────────────────────
// Extracted so the regression tests can assert the SHAPE of each query without
// a database — these are the exact strings the handlers execute.

// cpmDealRollupInsertSQL rolls DELIVERED up per (Denver day, CPM deal) — the
// fix for "Mailed MTD is not updating" (operator 2026-08-01).
//
// loadAllDealMonthlyActuals scanned a whole month of mailing_tracking_events
// live on every /pacing and /months request. Past the 30s statement_timeout
// that scan is cancelled, and its scan helper LOGS AND RETURNS — leaving the
// actuals map empty, so the handler reported mtd_delivered = 0 as though it
// were fact. Observed on prod repeatedly:
//
//	[CpmPlanner] monthly delivered: pq: canceling statement due to statement timeout
//	[CpmPlanner] pacing 3d rate:    pq: canceling statement due to statement timeout
//
// A silent zero on a budget-pacing screen is worse than an error: it reads as
// "we mailed nothing" and invites over-sending to "catch up". Delivered is now
// rolled up per closed day (computed once, immutable) so a month is a cheap
// SUM that cannot time out, and the handler can tell "no rows" from "no data".
//
// $1 = org, $2 = Denver day.
func cpmDealRollupInsertSQL() string {
	return dealCampaignMapCTE + `
		INSERT INTO mailing_cpm_deal_day_rollup (organization_id, day, deal_id, delivered, refreshed_at)
		SELECT $1::uuid, $2::date, dm.deal_id, COUNT(*), NOW()
		FROM mailing_tracking_events te
		JOIN dm ON dm.campaign_id = te.campaign_id
		WHERE te.organization_id = $1
		  AND te.event_type = 'delivered'
		  AND te.event_at >= ($2::date - INTERVAL '1 day')
		  AND te.event_at <  ($2::date + INTERVAL '2 days')
		  AND (te.event_at AT TIME ZONE 'America/Denver')::date = $2::date
		GROUP BY dm.deal_id`
}

// nonCpmRollupInsertSQL: $1 = org, $2 = Denver day.
func nonCpmRollupInsertSQL() string {
	return `
		INSERT INTO mailing_offer_day_rollup (organization_id, day, offer_identity, delivered, refreshed_at)
		SELECT $1::uuid, $2::date, ` + nonCpmIdentityExpr + `, COUNT(*), NOW()
		FROM mailing_tracking_events te
		JOIN mailing_campaigns c ON c.id = te.campaign_id
		LEFT JOIN mailing_offers o ON o.id = c.offer_id
		WHERE te.organization_id = $1
		  AND te.event_type = 'delivered'
		  AND te.event_at >= ($2::date - INTERVAL '1 day')
		  AND te.event_at <  ($2::date + INTERVAL '2 days')
		  AND (te.event_at AT TIME ZONE 'America/Denver')::date = $2::date
		GROUP BY 3`
}

// nonCpmClicksSQL: $1 = org, $2 = from date, $3 = to date (Denver days).
func nonCpmClicksSQL() string {
	return `
		SELECT ` + nonCpmIdentityExpr + ` AS identity,
		       COUNT(*), COUNT(DISTINCT te.subscriber_id)
		FROM mailing_tracking_events te
		JOIN mailing_campaigns c ON c.id = te.campaign_id
		LEFT JOIN mailing_offers o ON o.id = c.offer_id
		WHERE te.organization_id = $1
		  AND te.event_type = 'clicked'
		  AND te.event_at >= ($2::date - INTERVAL '1 day')
		  AND te.event_at <  ($3::date + INTERVAL '2 days')
		  AND (te.event_at AT TIME ZONE 'America/Denver')::date >= $2::date
		  AND (te.event_at AT TIME ZONE 'America/Denver')::date <= $3::date
		  AND COALESCE(te.link_url,'') <> ''
		  AND te.link_url !~* '\.(css|js|woff2?|ttf|otf|eot|png|jpe?g|gif|svg|ico|webp|map)([?#]|$)'
		  AND te.link_url !~* '(fonts\.g|cdn\.|cloudfront|akamai|fastly|jsdelivr|unpkg|gstatic)'
		  AND te.link_url !~* 'unsub|optout|opt-out|preference|/privacy'
		  AND te.link_url !~* '^everflow-import:'
		  AND te.link_url !~* '^https?://t\.em\.[^/]+/track/'
		GROUP BY 1`
}

// nonCpmConversionCountSQL: the LEDGER COUNT. $1 = org, $2 = from, $3 = to.
func nonCpmConversionCountSQL() string {
	return `
		SELECT lower(COALESCE(NULLIF(o.name,''), '(unattributed)')), COUNT(*)
		FROM mailing_offer_suppressions s
		LEFT JOIN mailing_offers o ON o.id = s.offer_id
		WHERE s.organization_id = $1 AND s.reason = 'converted'
		  AND (s.suppressed_at AT TIME ZONE 'America/Denver')::date >= $2::date
		  AND (s.suppressed_at AT TIME ZONE 'America/Denver')::date <= $3::date
		GROUP BY 1`
}

// nonCpmConversionPayoutSQL: the SUPPLIED PAYOUT. $1 = org, $2 = from, $3 = to.
func nonCpmConversionPayoutSQL() string {
	return `
		SELECT lower(COALESCE(
		           NULLIF(c.offer_key,''), NULLIF(o.name,''),
		           -- SCALAR subquery, never a join: everflow ids are NOT unique
		           -- in mailing_offers (id 162 = West Capital carries FOUR rows,
		           -- one per creative variant). Joining on it fanned 2 August
		           -- conversions into 8 and 100 USD of payout into 400 USD (4x
		           -- overstatement), caught pre-ship 2026-08-01. MIN()
		           -- makes the pick deterministic; the offer-group layer folds
		           -- the variants onto one advertiser row anyway.
		           NULLIF((SELECT MIN(eo.name) FROM mailing_offers eo
		                    WHERE eo.everflow_offer_id = ec.everflow_offer_id
		                      AND eo.organization_id = ec.organization_id), ''),
		           '(unattributed)')),
		       COUNT(*) FILTER (WHERE ec.payout > 0),
		       COALESCE(SUM(ec.payout), 0),
		       COUNT(*)
		FROM mailing_everflow_conversions ec
		LEFT JOIN mailing_campaigns c ON c.id = ec.campaign_id
		LEFT JOIN mailing_offers o ON o.id = c.offer_id
		WHERE ec.organization_id = $1
		  AND (ec.converted_at AT TIME ZONE 'America/Denver')::date >= $2::date
		  AND (ec.converted_at AT TIME ZONE 'America/Denver')::date <= $3::date
		GROUP BY 1`
}

// nonCpmDealMapSQL: identity -> CPM deal. $1 = org. Deliberately unwindowed.
func nonCpmDealMapSQL() string {
	return dealCampaignMapCTE + `,
		ident AS (
			SELECT ` + nonCpmIdentityExpr + ` AS identity, c.id
			FROM mailing_campaigns c
			LEFT JOIN mailing_offers o ON o.id = c.offer_id
			WHERE c.organization_id = $1
		)
		SELECT ident.identity, COALESCE(d.id::text,''), COALESCE(d.name,''), COUNT(*)
		FROM ident
		JOIN dm ON dm.campaign_id = ident.id
		JOIN mailing_cpm_deals d ON d.id = dm.deal_id
		GROUP BY 1, 2, 3`
}

// ─── Response types ─────────────────────────────────────────────────────────

type nonCpmOfferRow struct {
	OfferKey  string `json:"offer_key"` // the resolved identity
	GroupName string `json:"group_name"`

	Delivered   int64 `json:"delivered"`
	Clickers    int64 `json:"clickers"`
	Clicks      int64 `json:"clicks"`
	Conversions int64 `json:"conversions"`
	// PayoutCoverage: how many of Conversions carried a supplied payout. When
	// this is below Conversions the revenue/eCPM are FLOORS, not the truth.
	PayoutCoverage int64   `json:"payout_coverage"`
	Revenue        float64 `json:"revenue"`
	AvgPayout      float64 `json:"avg_payout"`

	ClickerRate float64 `json:"clicker_rate"` // clickers / delivered
	ConvRate    float64 `json:"conv_rate"`    // conversions / clickers
	Ecpm        float64 `json:"ecpm"`         // revenue / delivered * 1000

	IsCPM    bool   `json:"is_cpm"`
	DealID   string `json:"deal_id"`
	DealName string `json:"deal_name"`
	// CarryOver: conversions/revenue on this row are a RUNNING TOTAL to the end
	// of the selected month, not that month alone. Volume and clickers on the
	// same row are still month-scoped -- the screen must say so.
	CarryOver bool `json:"carry_over"`
}

type nonCpmGroupRow struct {
	GroupName string           `json:"group_name"`
	Offers    []nonCpmOfferRow `json:"offers"`
	nonCpmOfferRow
}

type nonCpmTotals struct {
	Offers         int     `json:"offers"`
	Delivered      int64   `json:"delivered"`
	Clickers       int64   `json:"clickers"`
	Clicks         int64   `json:"clicks"`
	Conversions    int64   `json:"conversions"`
	PayoutCoverage int64   `json:"payout_coverage"`
	Revenue        float64 `json:"revenue"`
	ClickerRate    float64 `json:"clicker_rate"`
	ConvRate       float64 `json:"conv_rate"`
	Ecpm           float64 `json:"ecpm"`
}

func nonCpmDiv(a, b float64) float64 {
	if b <= 0 {
		return 0
	}
	v := a / b
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return 0
	}
	return v
}

func (r *nonCpmOfferRow) deriveRates() {
	r.ClickerRate = nonCpmDiv(float64(r.Clickers), float64(r.Delivered))
	r.ConvRate = nonCpmDiv(float64(r.Conversions), float64(r.Clickers))
	r.Ecpm = nonCpmDiv(r.Revenue*1000, float64(r.Delivered))
	r.AvgPayout = nonCpmDiv(r.Revenue, float64(r.PayoutCoverage))
}

// ─── Per-metric loaders (all keyed on nonCpmIdentityExpr) ───────────────────

// nonCpmVolume sums the daily rollup across the month. Second return is the
// number of days actually present, so the handler can say when a month is only
// partially rolled up instead of reporting a short number as fact.
func (h *CpmPlannerHandlers) nonCpmVolume(orgID string, from, to time.Time) (map[string]int64, int, time.Time, error) {
	out := map[string]int64{}
	var days int
	var refreshed sql.NullTime
	rows, err := h.db.Query(`
		SELECT offer_identity, SUM(delivered), COUNT(DISTINCT day), MAX(refreshed_at)
		FROM mailing_offer_day_rollup
		WHERE organization_id = $1 AND day >= $2::date AND day <= $3::date
		GROUP BY 1`, orgID, from.Format("2006-01-02"), to.Format("2006-01-02"))
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int64
		var d int
		var r sql.NullTime
		if err := rows.Scan(&id, &n, &d, &r); err != nil {
			return nil, 0, time.Time{}, err
		}
		out[id] = n
		if d > days {
			days = d
		}
		if r.Valid && (!refreshed.Valid || r.Time.After(refreshed.Time)) {
			refreshed = r
		}
	}
	return out, days, refreshed.Time, rows.Err()
}

// nonCpmClicks is LIVE and exact for the month — distinct clickers cannot be
// summed from per-day distincts. The link_url predicate is the navigational
// (action) click definition: asset fetches, compliance links, the everflow
// import marker and unresolved t.em /track/ records are all excluded, while the
// t.em /o/ smart-link OFFER REDIRECT is kept (it is the money click).
func (h *CpmPlannerHandlers) nonCpmClicks(orgID string, from, to time.Time) (clicks, clickers map[string]int64, err error) {
	clicks, clickers = map[string]int64{}, map[string]int64{}
	rows, qerr := h.db.Query(nonCpmClicksSQL(), orgID, from.Format("2006-01-02"), to.Format("2006-01-02"))
	if qerr != nil {
		return nil, nil, qerr
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var ck, cr int64
		if err := rows.Scan(&id, &ck, &cr); err != nil {
			return nil, nil, err
		}
		clicks[id], clickers[id] = ck, cr
	}
	return clicks, clickers, rows.Err()
}

// nonCpmConversions returns the ledger conversion count per identity, plus the
// supplied payout. Two independent ledgers, deliberately reported separately:
//
//	count   mailing_offer_suppressions reason='converted' — the count of record,
//	        the same source the CPM deal cards use.
//	payout  mailing_everflow_conversions.payout — the only place a supplied
//	        payout exists (mailing_offers.payout is 0.00 on 27 of 34 offers).
//
// Both resolve identity by the conversion's OWN campaign linkage first, falling
// back to its everflow offer id → offer name. No slug-map dictionary is
// consulted: that indirection is exactly what silently zeroed this metric.
func (h *CpmPlannerHandlers) nonCpmConversions(orgID string, from, to time.Time) (counts, withPayout map[string]int64, revenue map[string]float64, err error) {
	counts, withPayout, revenue = map[string]int64{}, map[string]int64{}, map[string]float64{}
	ledger := map[string]int64{}

	// Ledger count — suppressions carry offer_id but no campaign; resolve the
	// identity through the offer the same way a campaign would.
	rows, qerr := h.db.Query(nonCpmConversionCountSQL(), orgID, from.Format("2006-01-02"), to.Format("2006-01-02"))
	if qerr != nil {
		return nil, nil, nil, qerr
	}
	func() {
		defer rows.Close()
		for rows.Next() {
			var id string
			var n int64
			if err := rows.Scan(&id, &n); err == nil {
				counts[id] += n
			}
		}
	}()

	// Supplied payout — prefer the conversion's campaign identity (exact),
	// else its everflow offer id resolved to an offer name.
	prows, perr := h.db.Query(nonCpmConversionPayoutSQL(), orgID, from.Format("2006-01-02"), to.Format("2006-01-02"))
	if perr != nil {
		return counts, withPayout, revenue, nil // count still usable
	}
	defer prows.Close()
	for prows.Next() {
		var id string
		var n, total int64
		var rev float64
		if err := prows.Scan(&id, &n, &rev, &total); err == nil {
			withPayout[id] += n
			revenue[id] += rev
			ledger[id] += total
		}
	}
	// COUNT OF RECORD = the LARGER of the two ledgers, per identity.
	//
	// Neither is complete on its own. mailing_offer_suppressions drops
	// conversions with a blank sub1 and carries no payout;
	// mailing_everflow_conversions was built later (REQ-037) precisely because
	// of that, but only holds what has landed since. Taking the max means a
	// conversion recorded in either place is counted once and never lost --
	// e.g. Sam's Club reads from the suppression ledger (39 in July vs 6), and
	// West Capital reads from the everflow ledger (10 vs 5) after the operator
	// supplied the conversions the postback path never captured.
	for id, n := range ledger {
		if n > counts[id] {
			counts[id] = n
		}
	}
	return counts, withPayout, revenue, prows.Err()
}

// nonCpmDealForIdentity maps each identity to the CPM deal billing it, via
// dealCampaignMapCTE — the single source of truth for deal attribution.
//
// NOT windowed by campaign created_at. The first cut scoped this to campaigns
// created inside the display window and mislabelled six live offers as "No CPM
// deal" in the 7-day view: kqckq7, sams-club, metal-roofing, tahiti-village,
// s6wff5 and liberty-mutual had ZERO campaigns created in 7 days while
// liberty-mutual alone delivered 1.5M in that window — campaigns created weeks
// earlier keep sending. Deal membership is a property of the OFFER, not of the
// reporting window.
func (h *CpmPlannerHandlers) nonCpmDealForIdentity(orgID string) (map[string][2]string, error) {
	q := nonCpmDealMapSQL()
	// Cached: the map is a ~16.5s full pass over the org's campaigns (measured
	// on prod 2026-08-01) and changes only when deals or offer wiring change,
	// so re-running it on every page view would dominate the request for no
	// freshness gain.
	h.evMu.Lock()
	if h.dealIdentity != nil && time.Since(h.dealIdentityAt) < nonCpmDealMapTTL {
		cached := h.dealIdentity
		h.evMu.Unlock()
		return cached, nil
	}
	h.evMu.Unlock()

	rows, err := h.db.Query(q, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]string{}
	best := map[string]int64{}
	for rows.Next() {
		var identity, dealID, dealName string
		var n int64
		if err := rows.Scan(&identity, &dealID, &dealName, &n); err != nil {
			return nil, err
		}
		// '(unattributed)' is a MIXED bucket — campaigns with neither an
		// offer_key nor a named offer, spanning many advertisers (measured:
		// 220 land in the Liberty deal, 194 in Tahiti). Stamping it with one
		// deal name would assert an attribution that does not exist, so it is
		// never classified as CPM.
		if identity == nonCpmUnattributed {
			continue
		}
		if dealID != "" && n > best[identity] {
			best[identity] = n
			out[identity] = [2]string{dealID, dealName}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	h.evMu.Lock()
	h.dealIdentity, h.dealIdentityAt = out, time.Now()
	h.evMu.Unlock()
	return out, nil
}

func (h *CpmPlannerHandlers) nonCpmGroups(orgID string) (map[string]string, map[string]bool, error) {
	rows, err := h.db.Query(
		`SELECT offer_identity, group_name, COALESCE(carry_over, FALSE)
		 FROM mailing_offer_groups WHERE organization_id = $1`, orgID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	carry := map[string]bool{}
	for rows.Next() {
		var id, g string
		var co bool
		if err := rows.Scan(&id, &g, &co); err == nil {
			out[id] = g
			if co {
				carry[g] = true
			}
		}
	}
	return out, carry, rows.Err()
}

// ─── Handlers ───────────────────────────────────────────────────────────────

// nonCpmMonthBounds resolves ?month=YYYY-MM to [first, last] Denver dates.
// The current month is capped at TODAY so an MTD number is never divided by
// days that have not happened.
func (h *CpmPlannerHandlers) nonCpmMonthBounds(arg string) (from, to time.Time, ym string, isCurrent bool, err error) {
	var today time.Time
	if e := h.db.QueryRow(`SELECT (NOW() AT TIME ZONE 'America/Denver')::date`).Scan(&today); e != nil {
		return from, to, "", false, e
	}
	curFirst := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	from = curFirst
	if strings.TrimSpace(arg) != "" {
		m, e := parseMonthArg(arg)
		if e != nil {
			return from, to, "", false, e
		}
		from = m
	}
	isCurrent = from.Equal(curFirst)
	to = from.AddDate(0, 1, -1)
	if isCurrent {
		to = today
	}
	return from, to, from.Format("2006-01"), isCurrent, nil
}

// HandleNonCpmPerformance GET /cpm-planner/non-cpm?month=YYYY-MM
//
// Served on demand (mount / period change / explicit refresh), never on a poll
// timer: the click pass is a live month-scoped scan (~7s measured).
func (h *CpmPlannerHandlers) HandleNonCpmPerformance(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)

	from, to, ym, isCurrent, err := h.nonCpmMonthBounds(r.URL.Query().Get("month"))
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	volume, daysRolled, refreshedAt, err := h.nonCpmVolume(orgID, from, to)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("volume rollup: %v", err))
		return
	}
	clicks, clickers, err := h.nonCpmClicks(orgID, from, to)
	if err != nil {
		log.Printf("[CpmPlanner] non-cpm clicks: %v", err)
		clicks, clickers = map[string]int64{}, map[string]int64{}
	}
	convCounts, convPaid, convRevenue, err := h.nonCpmConversions(orgID, from, to)
	if err != nil {
		log.Printf("[CpmPlanner] non-cpm conversions: %v", err)
	}
	deals, err := h.nonCpmDealForIdentity(orgID)
	if err != nil {
		log.Printf("[CpmPlanner] non-cpm deal map: %v", err)
		deals = map[string][2]string{}
	}
	groups, carryOver, err := h.nonCpmGroups(orgID)
	if err != nil {
		log.Printf("[CpmPlanner] non-cpm groups: %v", err)
		groups, carryOver = map[string]string{}, map[string]bool{}
	}

	// Carry-over advertisers settle on a running tally, so their conversions and
	// revenue are read cumulatively up to the end of the selected month instead
	// of month-only. Cheap to do as a second pass: the conversion ledgers are
	// tiny next to the delivery tables. Volume and clickers are deliberately NOT
	// re-read -- those stay month-scoped pacing numbers.
	var cumCounts, cumPaid map[string]int64
	var cumRevenue map[string]float64
	if len(carryOver) > 0 {
		epoch := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		if cc, cp, cr, cerr := h.nonCpmConversions(orgID, epoch, to); cerr != nil {
			log.Printf("[CpmPlanner] non-cpm carry-over conversions: %v", cerr)
		} else {
			cumCounts, cumPaid, cumRevenue = cc, cp, cr
		}
	}

	// Union every identity seen by ANY metric — an offer with conversions but
	// no rolled-up volume yet must still appear, or money goes missing.
	seen := map[string]bool{}
	for _, m := range []map[string]int64{volume, clicks, clickers, convCounts, convPaid} {
		for k := range m {
			seen[k] = true
		}
	}
	for k := range convRevenue {
		seen[k] = true
	}

	byGroup := map[string]*nonCpmGroupRow{}
	var cpmT, nonCpmT nonCpmTotals
	for identity := range seen {
		row := nonCpmOfferRow{
			OfferKey:    identity,
			Delivered:   volume[identity],
			Clicks:      clicks[identity],
			Clickers:    clickers[identity],
			Conversions: convCounts[identity],
			// PayoutCoverage can exceed Conversions when the payout ledger
			// resolves an identity the suppression ledger files elsewhere;
			// both are reported raw rather than reconciled behind the scenes.
			PayoutCoverage: convPaid[identity],
			Revenue:        convRevenue[identity],
		}
		if d, ok := deals[identity]; ok {
			row.IsCPM, row.DealID, row.DealName = true, d[0], d[1]
		}
		row.deriveRates()

		g := groups[identity]
		if g == "" {
			g = identity
		}
		row.GroupName = g
		if carryOver[g] && cumCounts != nil {
			row.Conversions = cumCounts[identity]
			row.PayoutCoverage = cumPaid[identity]
			row.Revenue = cumRevenue[identity]
			row.CarryOver = true
			row.deriveRates()
		}

		gr := byGroup[g]
		if gr == nil {
			gr = &nonCpmGroupRow{GroupName: g}
			gr.nonCpmOfferRow.GroupName = g
			byGroup[g] = gr
		}
		gr.Offers = append(gr.Offers, row)
		gr.Delivered += row.Delivered
		gr.Clicks += row.Clicks
		gr.Clickers += row.Clickers
		gr.Conversions += row.Conversions
		gr.PayoutCoverage += row.PayoutCoverage
		gr.Revenue += row.Revenue
		// A group is CPM if ANY of its identities is billed by a deal.
		if row.IsCPM && !gr.IsCPM {
			gr.IsCPM, gr.DealID, gr.DealName = true, row.DealID, row.DealName
		}
		if row.CarryOver {
			gr.CarryOver = true
		}

		t := &nonCpmT
		if row.IsCPM {
			t = &cpmT
		}
		t.Offers++
		t.Delivered += row.Delivered
		t.Clicks += row.Clicks
		t.Clickers += row.Clickers
		t.Conversions += row.Conversions
		t.PayoutCoverage += row.PayoutCoverage
		t.Revenue += row.Revenue
	}

	out := make([]nonCpmGroupRow, 0, len(byGroup))
	for _, gr := range byGroup {
		gr.nonCpmOfferRow.OfferKey = gr.GroupName
		gr.deriveRates()
		sort.Slice(gr.Offers, func(i, j int) bool { return gr.Offers[i].Delivered > gr.Offers[j].Delivered })
		out = append(out, *gr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Delivered > out[j].Delivered })

	for _, t := range []*nonCpmTotals{&cpmT, &nonCpmT} {
		t.ClickerRate = nonCpmDiv(float64(t.Clickers), float64(t.Delivered))
		t.ConvRate = nonCpmDiv(float64(t.Conversions), float64(t.Clickers))
		t.Ecpm = nonCpmDiv(t.Revenue*1000, float64(t.Delivered))
	}

	expectedDays := int(to.Sub(from).Hours()/24) + 1
	resp := map[string]interface{}{
		"month":         ym,
		"from":          from.Format("2006-01-02"),
		"to":            to.Format("2006-01-02"),
		"is_current":    isCurrent,
		"groups":        out,
		"totals":        map[string]interface{}{"cpm": cpmT, "non_cpm": nonCpmT},
		"days_rolled":   daysRolled,
		"days_expected": expectedDays,
		// volume_partial is the honest flag: the delivered rollup backfills one
		// closed day per cycle, so an older month can legitimately be short.
		// Clicks and conversions are always live/complete for the range.
		"volume_partial": daysRolled < expectedDays,
		"sources": map[string]string{
			"delivered":   "postgres:mailing_tracking_events (rolled up per Denver day)",
			"clickers":    "postgres:mailing_tracking_events, navigational links only, DISTINCT subscriber_id (live)",
			"conversions": "postgres:mailing_offer_suppressions reason='converted' (ledger count of record)",
			"revenue":     "postgres:mailing_everflow_conversions.payout (supplied payout; mailing_offers.payout is 0.00 on 27 of 34 offers)",
			"ecpm":        "revenue / delivered * 1000",
		},
	}
	if !refreshedAt.IsZero() {
		resp["refreshed_at"] = refreshedAt.Format(time.RFC3339)
	}
	respondJSON(w, http.StatusOK, resp)
}

// HandleUpsertOfferGroup PUT /cpm-planner/offer-groups/{identity}
// Body: {"group_name": "West Capital HELOC"}. Marks the row source='operator'
// so the boot seed can never overwrite it. An empty group_name clears the
// override and lets the identity group under itself.
func (h *CpmPlannerHandlers) HandleUpsertOfferGroup(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	identity := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "identity")))
	if identity == "" {
		respondError(w, http.StatusBadRequest, "offer identity required")
		return
	}
	var in struct {
		GroupName string `json:"group_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	name := strings.TrimSpace(in.GroupName)
	if name == "" {
		if _, err := h.db.Exec(
			`DELETE FROM mailing_offer_groups WHERE organization_id = $1 AND offer_identity = $2`,
			orgID, identity); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("clear group: %v", err))
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"offer_identity": identity, "status": "cleared"})
		return
	}
	if _, err := h.db.Exec(`
		INSERT INTO mailing_offer_groups (organization_id, offer_identity, group_name, source, updated_at)
		VALUES ($1, $2, $3, 'operator', NOW())
		ON CONFLICT (organization_id, offer_identity) DO UPDATE
		SET group_name = EXCLUDED.group_name, source = 'operator', updated_at = NOW()`,
		orgID, identity, name); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("save group: %v", err))
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"offer_identity": identity, "group_name": name, "status": "saved",
	})
}
