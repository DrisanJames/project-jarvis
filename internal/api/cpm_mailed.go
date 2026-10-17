package api

import (
	"database/sql"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// CPM MAILED TRACKER (operator 2026-10-17): "how much have I mailed to each
// CPM this month and how far am I from fulfilling it".
//
// Source = mailing_campaigns.sent_count / delivered_count, NOT
// mailing_tracking_events. The pacing view reads mailing_cpm_deal_day_rollup,
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

type cpmMailedDay struct {
	Day       string `json:"day"`
	Sent      int64  `json:"sent"`
	Delivered int64  `json:"delivered"`
	Campaigns int64  `json:"campaigns"`
}

type cpmMailedDeal struct {
	DealID          string  `json:"deal_id"`
	Name            string  `json:"name"`
	Status          string  `json:"status"`
	EverflowOfferID string  `json:"everflow_offer_id"`
	TargetVolume    int64   `json:"target_volume"`
	TargetBudget    float64 `json:"target_budget"`
	TargetSource    string  `json:"target_source"` // monthly | deal | none
	Ecpm            float64 `json:"ecpm"`

	Sent       int64   `json:"sent"`
	Delivered  int64   `json:"delivered"`
	Campaigns  int64   `json:"campaigns"`
	Booked     int64   `json:"booked"` // recipients on in-flight/scheduled campaigns not yet sent
	Remaining  int64   `json:"remaining"`
	PctSent    float64 `json:"pct_sent"`
	EarnedUSD  float64 `json:"earned_usd"` // sent ÷ 1000 × eCPM (billing basis is the advertiser's; sent is shown)
	SentToday  int64   `json:"sent_today"`
	Avg3d      float64 `json:"avg_3d"` // mean sent over the last 3 COMPLETE Denver days
	DaysLeft   int     `json:"days_left"`
	ReqDaily   float64 `json:"required_daily"`
	Projected  int64   `json:"projected"`
	ProjPct    float64 `json:"projected_pct"`
	OnPace     bool    `json:"on_pace"`
	LastSentOn string  `json:"last_sent_on"`

	cpmMailedModel

	Days []cpmMailedDay `json:"days"`
}

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

// HandleMailedTracker GET /cpm-planner/mailed?month=YYYY-MM (default: current
// Denver month). Per deal: month target, sent/delivered MTD from campaign
// counters, remaining, required/day, 3-day pace, projection, per-day series.
func (h *CpmPlannerHandlers) HandleMailedTracker(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)

	var today time.Time
	if err := h.db.QueryRow(`SELECT (NOW() AT TIME ZONE 'America/Denver')::date`).Scan(&today); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("current date: %v", err))
		return
	}
	curMonth := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	month := curMonth
	if v := strings.TrimSpace(r.URL.Query().Get("month")); v != "" {
		m, err := parseMonthArg(v)
		if err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		month = m
	}
	next := month.AddDate(0, 1, 0)
	daysInMonth := next.AddDate(0, 0, -1).Day()
	isCurrent := month.Equal(curMonth)
	dayOfMonth := daysInMonth // closed month: nothing left to pace
	if isCurrent {
		dayOfMonth = today.Day()
	} else if month.After(curMonth) {
		dayOfMonth = 1 // future month: every day remains
	}

	deals, _, err := h.loadDealsLite(orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("load deals: %v", err))
		return
	}

	type tgt struct {
		budget, ecpm, ecpa sql.NullFloat64
		volume             sql.NullInt64
	}
	targets := map[string]tgt{}
	if rows, err := h.db.Query(`
		SELECT deal_id::text, target_budget, target_volume, target_ecpm, target_ecpa
		FROM mailing_cpm_deal_monthly_targets
		WHERE organization_id = $1 AND month = $2`, orgID, month); err == nil {
		func() {
			defer rows.Close()
			for rows.Next() {
				var id string
				var t tgt
				if err := rows.Scan(&id, &t.budget, &t.volume, &t.ecpm, &t.ecpa); err == nil {
					targets[id] = t
				}
			}
		}()
	} else {
		log.Printf("[CpmPlanner] mailed targets: %v", err)
	}

	type agg struct {
		sent, delivered, campaigns, booked int64
		days                               map[string]*cpmMailedDay
	}
	actuals := map[string]*agg{}
	rows, err := h.db.Query(cpmMailedSQL(), orgID, month.Format("2006-01-02"), next.Format("2006-01-02"))
	if err != nil {
		// Fail loudly: a silent zero on this screen reads as "we mailed nothing".
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("mailed volume: %v", err))
		return
	}
	efConv := map[string]int64{}
	for rows.Next() {
		var kind, id, day string
		var sent, dlv, camps, booked int64
		if err := rows.Scan(&kind, &id, &day, &sent, &dlv, &camps, &booked); err != nil {
			rows.Close()
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("mailed scan: %v", err))
			return
		}
		if kind == "c" {
			efConv[id] = sent
			continue
		}
		a := actuals[id]
		if a == nil {
			a = &agg{days: map[string]*cpmMailedDay{}}
			actuals[id] = a
		}
		a.sent += sent
		a.delivered += dlv
		a.campaigns += camps
		a.booked += booked
		a.days[day] = &cpmMailedDay{Day: day, Sent: sent, Delivered: dlv, Campaigns: camps}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("mailed rows: %v", err))
		return
	}

	type manual struct {
		conv int64
		paid float64
	}
	manuals := map[string]manual{}
	mArgs := []interface{}{orgID, month.Format("2006-01-02"), next.Format("2006-01-02")}
	if mrow, err := h.db.Query(cpmMailedManualSQL, mArgs...); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("manual conversions: %v", err))
		return
	} else {
		for mrow.Next() {
			var id string
			var m manual
			if err := mrow.Scan(&id, &m.conv, &m.paid); err == nil {
				manuals[id] = m
			}
		}
		mrow.Close()
	}

	out := []cpmMailedDeal{}
	var pf struct {
		TargetVolume int64   `json:"target_volume"`
		Sent         int64   `json:"sent"`
		Delivered    int64   `json:"delivered"`
		Remaining    int64   `json:"remaining"`
		Booked       int64   `json:"booked"`
		EarnedUSD    float64 `json:"earned_usd"`
		TargetBudget float64 `json:"target_budget"`
		Revenue      float64 `json:"revenue"`
		ProjRevenue  float64 `json:"projected_revenue"`
		Projected    int64   `json:"projected"`
	}
	for i := range deals {
		d := &deals[i]
		a := actuals[d.ID]
		if a == nil {
			a = &agg{days: map[string]*cpmMailedDay{}}
		}

		// Target: the month's target row, else the deal's own plan (active
		// deals only) — the same precedence HandleCurrentMonthPacing applies.
		ecpm, ecpa := d.EcpmGoal, d.EcpaGoal
		targetVolume, targetBudget, source := int64(0), 0.0, "none"
		if t, ok := targets[d.ID]; ok {
			if t.ecpm.Valid && t.ecpm.Float64 > 0 {
				ecpm = t.ecpm.Float64
			}
			if t.ecpa.Valid && t.ecpa.Float64 > 0 {
				ecpa = t.ecpa.Float64
			}
			if t.budget.Valid {
				targetBudget = t.budget.Float64
			}
			switch {
			case t.volume.Valid && t.volume.Int64 > 0:
				targetVolume, source = t.volume.Int64, "monthly"
			case targetBudget > 0 && ecpm > 0:
				targetVolume, source = int64(math.Ceil(targetBudget/ecpm*1000)), "monthly"
			}
		}
		if source == "none" && d.Status == "active" {
			if planned, _, _ := cpmPlanNumbers(d.Budget, d.EcpmGoal, d.EcpaGoal, d.AvgCampaignSize); planned > 0 {
				targetVolume, targetBudget, source = planned, d.Budget, "deal"
			}
		}
		mn := manuals[d.ID]
		if a.sent == 0 && a.booked == 0 && efConv[d.ID] == 0 && mn.conv == 0 && mn.paid == 0 &&
			(d.Status != "active" || targetVolume == 0) {
			continue // not mailing and not owed anything this month
		}

		daily := map[string]int64{}
		days := make([]cpmMailedDay, 0, len(a.days))
		last := ""
		for k, v := range a.days {
			daily[k] = v.Sent
			days = append(days, *v)
			if v.Sent > 0 && k > last {
				last = k
			}
		}
		sort.Slice(days, func(i, j int) bool { return days[i].Day < days[j].Day })

		var avg3 float64
		var sentToday int64
		if isCurrent {
			avg3 = cpmMailedAvg3d(daily, today)
			sentToday = daily[today.Format("2006-01-02")]
		}
		reqDaily, projected, projPct, onPace := cpmPacingMath(targetVolume, a.sent, avg3, dayOfMonth, daysInMonth)
		if !isCurrent && !month.After(curMonth) {
			reqDaily = 0 // closed month: required/day is meaningless
		}
		remaining := targetVolume - a.sent
		if remaining < 0 {
			remaining = 0
		}
		var pct float64
		if targetVolume > 0 {
			pct = float64(a.sent) / float64(targetVolume)
		}
		daysLeft := daysInMonth - dayOfMonth + 1
		if !isCurrent {
			daysLeft = 0
			if month.After(curMonth) {
				daysLeft = daysInMonth
			}
		}

		row := cpmMailedDeal{
			DealID: d.ID, Name: d.Name, Status: d.Status, EverflowOfferID: d.EverflowOfferID,
			TargetVolume: targetVolume, TargetBudget: targetBudget, TargetSource: source, Ecpm: ecpm,
			Sent: a.sent, Delivered: a.delivered, Campaigns: a.campaigns, Booked: a.booked,
			Remaining: remaining, PctSent: pct, EarnedUSD: cpmMonthlyRevenue(a.sent, ecpm),
			SentToday: sentToday, Avg3d: avg3, DaysLeft: daysLeft, ReqDaily: reqDaily,
			Projected: projected, ProjPct: projPct, OnPace: onPace, LastSentOn: last,
			cpmMailedModel: cpmMailedModelFor(targetBudget, ecpa, a.sent, projected, efConv[d.ID], mn.conv, mn.paid),
			Days:           days,
		}
		out = append(out, row)
		pf.TargetVolume += targetVolume
		pf.TargetBudget += targetBudget
		pf.Sent += a.sent
		pf.Delivered += a.delivered
		pf.Remaining += remaining
		pf.Booked += a.booked
		pf.EarnedUSD += row.EarnedUSD
		pf.Revenue += row.Revenue
		pf.ProjRevenue += row.ProjRevenue
		pf.Projected += row.Projected
	}
	// Biggest commitment first — the operator reads the deal that matters most.
	sort.SliceStable(out, func(i, j int) bool { return out[i].TargetVolume > out[j].TargetVolume })

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"month":         month.Format("2006-01"),
		"is_current":    isCurrent,
		"today":         today.Format("2006-01-02"),
		"day_of_month":  dayOfMonth,
		"days_in_month": daysInMonth,
		"source":        "mailing_campaigns.sent_count (campaign counters; deal map = offer_id ∪ earmarks ∪ name pattern ∪ offer_key)",
		"deals":         out,
		"portfolio":     pf,
	})
}
