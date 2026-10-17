package api

import (
	"math"
	"time"
)

// CPM mailed-to-goal + eCPM model — the volume and revenue engine behind
// HandleCurrentMonthPacing (operator 2026-10-17: "how much have I mailed to
// each CPM this month, how far am I from fulfilling it, and what eCPM does the
// month land at on the current cadence").
//
// Source = mailing_campaigns.sent_count / delivered_count, NOT
// mailing_tracking_events. Pacing used to read mailing_cpm_deal_day_rollup,
// which counts PG 'delivered' webhook confirmations (lossy under burst, laggy
// at Microsoft) and computes each closed day ONCE — late confirmations never
// land, so a month undercounts and the gap differs month to month. Campaign
// counters are what the send worker itself wrote; summing them for one month
// is an indexed read over a few thousand rows, not a partitioned-events scan.
//
// Attribution = dealCampaignMapCTE (offer_id ∪ earmarks ∪ name pattern ∪
// stamped offer_key) — the same deal→campaign map every other CPM surface
// uses, so a campaign counts here iff it counts there. A campaign's volume
// belongs to the Denver day it STARTED (scheduled_at when not yet started).

// cpmMailedModel is the eCPM model for one deal-month (operator 2026-10-17:
// "what am I mailing at, and if the month finishes at the current cadence,
// what does the eCPM come out to").
//
// Revenue basis, in precedence:
//   - "paid":  the month's manual-conversion rows that carry revenue — the
//     advertiser's actual payment, entered by the operator. It REPLACES the
//     modeled figure (never added to it, or a paid month double counts).
//   - "model": conversions × eCPA, where conversions = Everflow postbacks on
//     the deal's campaigns + revenue-less manual rows.
//
// The forecast holds yield per send constant: projected revenue scales with
// projected volume, so the forecast EARNED eCPM equals the MTD one. The number
// that moves with cadence is BudgetEcpmAtProj — the eCPM the month lands at if
// the whole budget is paid against the volume actually mailed.
type cpmMailedModel struct {
	Ecpa             float64 `json:"ecpa"`
	ConvNeeded       int64   `json:"conversions_needed"`
	EfConversions    int64   `json:"ef_conversions"`
	Conversions      int64   `json:"conversions"`
	Revenue          float64 `json:"revenue"`
	RevenueBasis     string  `json:"revenue_basis"` // paid | model | none
	EarnedEcpm       float64 `json:"earned_ecpm"`
	ActualEcpa       float64 `json:"actual_ecpa"`
	ProjConversions  int64   `json:"projected_conversions"`
	ProjRevenue      float64 `json:"projected_revenue"`
	BudgetEcpmAtProj float64 `json:"budget_ecpm_at_projected"`
	RevenueGap       float64 `json:"revenue_gap"` // budget − projected revenue (negative = over)
}

func cpmMailedModelFor(budget, ecpa float64, sent, projected, efConv, manualConv int64, paid float64) cpmMailedModel {
	m := cpmMailedModel{Ecpa: ecpa, EfConversions: efConv, RevenueBasis: "none"}
	if budget > 0 && ecpa > 0 {
		m.ConvNeeded = int64(math.Ceil(budget / ecpa))
	}
	m.Conversions = efConv + manualConv
	switch {
	case paid > 0:
		m.Revenue, m.RevenueBasis = paid, "paid"
	case m.Conversions > 0 && ecpa > 0:
		m.Revenue, m.RevenueBasis = float64(m.Conversions)*ecpa, "model"
	}
	if sent > 0 {
		m.EarnedEcpm = m.Revenue / float64(sent) * 1000
		scale := float64(projected) / float64(sent)
		m.ProjRevenue = m.Revenue * scale
		m.ProjConversions = int64(math.Round(float64(m.Conversions) * scale))
	}
	if m.Conversions > 0 && m.Revenue > 0 {
		m.ActualEcpa = m.Revenue / float64(m.Conversions)
	}
	if projected > 0 && budget > 0 {
		m.BudgetEcpmAtProj = budget / float64(projected) * 1000
	}
	m.RevenueGap = budget - m.ProjRevenue
	return m
}

// cpmMailedSQL returns BOTH halves of the month in one pass over the
// deal→campaign map (each pass costs ~8s on prod; two would crowd the 30s
// pool timeout). Rows are tagged by kind:
//
//	'v' — volume per (deal, Denver day the campaign started): sent,
//	      delivered, campaigns, booked (recipients on in-flight campaigns not
//	      yet sent). started_at, else scheduled_at for not-yet-started rows.
//	'c' — Everflow postbacks CONVERTED in the month on the deal's campaigns
//	      (any send month) in the sent column. Campaign-less postings — e.g. a
//	      CPM budget allocation posted as one "conversion" (EF 338 $12k) —
//	      never attach to a campaign, so never count.
//
// $1 = org, $2 = month start (Denver date), $3 = next month start; bounds are
// converted to timestamptz once so the comparisons stay sargable.
func cpmMailedSQL() string {
	return dealCampaignMapCTE + `,
	vol AS (
		SELECT dm.deal_id,
		       to_char((COALESCE(c.started_at, c.scheduled_at) AT TIME ZONE 'America/Denver')::date, 'YYYY-MM-DD') AS day,
		       COALESCE(SUM(c.sent_count), 0) AS sent,
		       COALESCE(SUM(c.delivered_count), 0) AS delivered,
		       COUNT(DISTINCT c.id) AS campaigns,
		       COALESCE(SUM(CASE WHEN c.status IN ('scheduled','preparing','finalizing_audience','sending','paused')
		                         THEN GREATEST(COALESCE(c.total_recipients,0) - COALESCE(c.sent_count,0), 0)
		                         ELSE 0 END), 0) AS booked
		FROM dm
		JOIN mailing_campaigns c ON c.id = dm.campaign_id
		WHERE c.organization_id = $1
		  AND c.status NOT IN ('draft','cancelled','deleted','failed')
		  AND COALESCE(c.started_at, c.scheduled_at) >= ($2::date::timestamp AT TIME ZONE 'America/Denver')
		  AND COALESCE(c.started_at, c.scheduled_at) <  ($3::date::timestamp AT TIME ZONE 'America/Denver')
		GROUP BY 1, 2
	),
	conv AS (
		SELECT dm.deal_id, COUNT(*) AS conversions
		FROM mailing_everflow_conversions ec
		JOIN dm ON dm.campaign_id = ec.campaign_id
		WHERE ec.organization_id = $1
		  AND ec.converted_at >= ($2::date::timestamp AT TIME ZONE 'America/Denver')
		  AND ec.converted_at <  ($3::date::timestamp AT TIME ZONE 'America/Denver')
		GROUP BY 1
	)
	SELECT 'v' AS kind, deal_id::text, day, sent, delivered, campaigns, booked FROM vol
	UNION ALL
	SELECT 'c', deal_id::text, '', conversions, 0, 0, 0 FROM conv`
}

// cpmMailedManualSQL: manual rows by STORED (UTC) date, like every other CPM
// month read (bare-date quick-adds are timezone-naive).
const cpmMailedManualSQL = `
		SELECT deal_id::text,
		       COALESCE(SUM(CASE WHEN revenue <= 0 THEN count ELSE 0 END), 0) AS conv_no_revenue,
		       COALESCE(SUM(CASE WHEN revenue > 0 THEN revenue ELSE 0 END), 0) AS paid
		FROM mailing_cpm_manual_conversions
		WHERE organization_id = $1
		  AND (converted_at AT TIME ZONE 'UTC')::date >= $2::date
		  AND (converted_at AT TIME ZONE 'UTC')::date <  $3::date
		GROUP BY 1`

// cpmMailedAvg3d is the mean sent over the 3 complete Denver days before
// today (today's partial day would understate it). days is keyed YYYY-MM-DD.
func cpmMailedAvg3d(days map[string]int64, today time.Time) float64 {
	var s int64
	for i := 1; i <= 3; i++ {
		s += days[today.AddDate(0, 0, -i).Format("2006-01-02")]
	}
	return float64(s) / 3.0
}
