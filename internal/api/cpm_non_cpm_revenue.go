package api

// NON-CPM REVENUE PLAN — revenue earned vs budget for CPL / CPA / CPC offer
// groups, the non-CPM counterpart of the CPM mailed-to-goal model
// (operator 2026-10-17: "provide CPL eCPMs as well ... revenue earned vs
// budget. the eHealth offer is a CPC which is at a 1.80 payout until today
// where it is at a $2.00 payout").
//
// Configured per offer GROUP (the advertiser row of the non-CPM table):
//   mailing_offer_revenue_config  — per (group, month): budget, payout model,
//                                   paid-to-date override, notes
//   mailing_offer_payout_rates    — per (group, effective_from): the payout
//                                   rate in effect from that Denver day on.
//                                   Not month-scoped — a rate runs until the
//                                   next row replaces it.
//
// Revenue, in precedence:
//   "paid"     — paid_revenue > 0: what the advertiser actually paid MTD,
//                entered by the operator. Replaces the model, never added.
//   "rate"     — CPC: Σ_day billable clicks(day) × rate in effect that day.
//                CPL/CPA: conversions × the rate in effect on the last day.
//   "everflow" — the supplied per-conversion payout (no rate configured).
//
// CPC billable clicks are OUR tracked clicks, not Everflow's: PG holds no
// Everflow click table (mailing_everflow_conversions is postbacks only). A
// billable click = one DISTINCT (subscriber, campaign) per Denver day on a
// navigational link — repeat clicks, asset fetches and compliance links are
// excluded, the scanner verdict is NOT applied (doctrine: never gate on it).
// It is an ESTIMATE of Everflow's count; paid_revenue overrides it.

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

func nonCpmRevenueDDL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS mailing_offer_revenue_config (
			organization_id UUID NOT NULL,
			group_name      TEXT NOT NULL,
			month           DATE NOT NULL,
			budget          NUMERIC(12,2) NOT NULL DEFAULT 0,
			payout_model    TEXT NOT NULL DEFAULT 'cpl',
			paid_revenue    NUMERIC(12,2) NOT NULL DEFAULT 0,
			notes           TEXT NOT NULL DEFAULT '',
			source          TEXT NOT NULL DEFAULT 'operator',
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (organization_id, group_name, month)
		)`,
		`CREATE TABLE IF NOT EXISTS mailing_offer_payout_rates (
			organization_id UUID NOT NULL,
			group_name      TEXT NOT NULL,
			effective_from  DATE NOT NULL,
			rate            NUMERIC(10,4) NOT NULL,
			source          TEXT NOT NULL DEFAULT 'operator',
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (organization_id, group_name, effective_from)
		)`,
	}
}

// nonCpmSeedRates: eHealth CPC $1.80 from onboarding, $2.00 from 2026-10-17
// (operator 2026-10-17). Seeded only while the group has NO rates at all, so an
// operator edit or deletion is never undone by a boot.
var nonCpmSeedRates = map[string][]nonCpmRate{
	"eHealth CPC": {{EffectiveFrom: "2026-10-01", Rate: 1.80}, {EffectiveFrom: "2026-10-17", Rate: 2.00}},
}

// nonCpmSeedModels: payout model for the seeded groups' first month.
var nonCpmSeedModels = map[string][2]string{"eHealth CPC": {"2026-10-01", "cpc"}}

func (h *CpmPlannerHandlers) seedNonCpmRevenue() {
	for g, rates := range nonCpmSeedRates {
		for _, r := range rates {
			if _, err := h.db.Exec(`
				INSERT INTO mailing_offer_payout_rates (organization_id, group_name, effective_from, rate, source)
				SELECT o.id, $1, $2::date, $3, 'seed' FROM organizations o
				WHERE NOT EXISTS (SELECT 1 FROM mailing_offer_payout_rates x
				                  WHERE x.organization_id = o.id AND x.group_name = $1 AND x.source <> 'seed')
				  AND NOT EXISTS (SELECT 1 FROM mailing_offer_revenue_config c
				                  WHERE c.organization_id = o.id AND c.group_name = $1 AND c.source = 'operator')
				ON CONFLICT (organization_id, group_name, effective_from) DO NOTHING`, g, r.EffectiveFrom, r.Rate); err != nil {
				log.Printf("[CpmPlanner] seed rate %s %s: %v", g, r.EffectiveFrom, err)
				return
			}
		}
	}
	for g, mm := range nonCpmSeedModels {
		if _, err := h.db.Exec(`
			INSERT INTO mailing_offer_revenue_config (organization_id, group_name, month, payout_model, source)
			SELECT id, $1, $2::date, $3, 'seed' FROM organizations
			ON CONFLICT (organization_id, group_name, month) DO NOTHING`, g, mm[0], mm[1]); err != nil {
			log.Printf("[CpmPlanner] seed model %s: %v", g, err)
		}
	}
}

// ─── Types ──────────────────────────────────────────────────────────────────

type nonCpmRate struct {
	EffectiveFrom string  `json:"effective_from"` // YYYY-MM-DD, Denver day
	Rate          float64 `json:"rate"`
}

type nonCpmRevenueConfig struct {
	Budget      float64 `json:"budget"`
	PayoutModel string  `json:"payout_model"` // cpl | cpa | cpc
	PaidRevenue float64 `json:"paid_revenue"`
	Notes       string  `json:"notes"`
}

type nonCpmPlanDay struct {
	Day     string  `json:"day"`
	Sent    int64   `json:"sent"`
	Units   int64   `json:"units"` // billable clicks (cpc)
	Rate    float64 `json:"rate"`
	Revenue float64 `json:"revenue"`
}

// nonCpmPlan is the revenue-vs-budget model for one offer group-month.
type nonCpmPlan struct {
	Configured  bool         `json:"configured"`
	PayoutModel string       `json:"payout_model"`
	Budget      float64      `json:"budget"`
	PaidRevenue float64      `json:"paid_revenue"`
	Notes       string       `json:"notes"`
	Rates       []nonCpmRate `json:"rates"`
	CurrentRate float64      `json:"current_rate"`

	Sent           int64   `json:"sent"`
	Units          int64   `json:"units"` // billable clicks (cpc) or conversions (cpl/cpa)
	UnitLabel      string  `json:"unit_label"`
	ModeledRevenue float64 `json:"modeled_revenue"` // rate × units, before any paid override
	Revenue        float64 `json:"revenue"`
	RevenueBasis   string  `json:"revenue_basis"` // paid | rate | everflow | unavailable | none
	EarnedEcpm     float64 `json:"earned_ecpm"`   // revenue / sent × 1000

	BudgetPct float64 `json:"budget_pct"` // revenue / budget
	BudgetGap float64 `json:"budget_gap"` // budget − revenue (negative = over)

	Avg3dSent      float64 `json:"avg3d_sent"`
	Avg3dUnits     float64 `json:"avg3d_units"`
	ProjSent       int64   `json:"projected_sent"`
	ProjRevenue    float64 `json:"projected_revenue"`
	ProjEcpm       float64 `json:"projected_ecpm"`
	ProjBudgetPct  float64 `json:"projected_budget_pct"`
	ProjGap        float64 `json:"projected_gap"`
	RequiredPerDay float64 `json:"required_revenue_per_day"`

	Days     []nonCpmPlanDay `json:"days,omitempty"`
	Warnings []string        `json:"warnings,omitempty"`
}

// nonCpmRateOn returns the rate in effect on day (rates sorted ascending).
func nonCpmRateOn(rates []nonCpmRate, day string) (float64, bool) {
	rate, ok := 0.0, false
	for _, r := range rates {
		if r.EffectiveFrom <= day {
			rate, ok = r.Rate, true
		}
	}
	return rate, ok
}

// nonCpmPlanFor is the pure model. sentDays / unitDays are keyed YYYY-MM-DD;
// unitsOK=false means the click pass failed — a CPC group then reports
// "unavailable" rather than pricing zero clicks as fact. today is the last day
// of the window (Denver today for the current month).
func nonCpmPlanFor(cfg *nonCpmRevenueConfig, rates []nonCpmRate, sentDays, unitDays map[string]int64,
	conversions int64, everflowRevenue float64, unitsOK bool, today time.Time, isCurrent bool) nonCpmPlan {
	p := nonCpmPlan{RevenueBasis: "none", UnitLabel: "conversions", Rates: rates}
	if p.Rates == nil {
		p.Rates = []nonCpmRate{}
	}
	if cfg != nil {
		p.Configured = true
		p.PayoutModel, p.Budget, p.PaidRevenue, p.Notes = cfg.PayoutModel, cfg.Budget, cfg.PaidRevenue, cfg.Notes
	}
	todayS := today.Format("2006-01-02")
	p.CurrentRate, _ = nonCpmRateOn(rates, todayS)

	for _, n := range sentDays {
		p.Sent += n
	}
	cpc := p.PayoutModel == "cpc"

	switch {
	case cpc && !unitsOK:
		p.UnitLabel = "billable clicks"
		p.RevenueBasis = "unavailable"
		p.Warnings = append(p.Warnings, "click pass failed — CPC revenue NOT computed (not zero)")
	case cpc:
		p.UnitLabel = "billable clicks"
		days := map[string]bool{}
		for d := range sentDays {
			days[d] = true
		}
		for d := range unitDays {
			days[d] = true
		}
		keys := make([]string, 0, len(days))
		for d := range days {
			keys = append(keys, d)
		}
		sort.Strings(keys)
		var unpriced int64
		for _, d := range keys {
			u := unitDays[d]
			rate, ok := nonCpmRateOn(rates, d)
			if !ok && u > 0 {
				unpriced += u
			}
			rev := float64(u) * rate
			p.Units += u
			p.ModeledRevenue += rev
			p.Days = append(p.Days, nonCpmPlanDay{Day: d, Sent: sentDays[d], Units: u, Rate: rate, Revenue: rev})
		}
		if unpriced > 0 {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%d billable clicks fall before the first rate — priced at $0", unpriced))
		}
		if len(rates) == 0 {
			p.Warnings = append(p.Warnings, "CPC model with no payout rate configured")
		} else {
			p.RevenueBasis = "rate"
		}
	default:
		p.Units = conversions
		if lastRate, ok := nonCpmRateOn(rates, todayS); ok && p.Configured {
			p.ModeledRevenue = float64(conversions) * lastRate
			p.RevenueBasis = "rate"
		} else if everflowRevenue > 0 {
			p.ModeledRevenue = everflowRevenue
			p.RevenueBasis = "everflow"
		}
	}
	p.Revenue = p.ModeledRevenue
	if p.PaidRevenue > 0 {
		p.Revenue, p.RevenueBasis = p.PaidRevenue, "paid"
	}
	p.EarnedEcpm = nonCpmDiv(p.Revenue*1000, float64(p.Sent))
	if p.Budget > 0 {
		p.BudgetPct = p.Revenue / p.Budget
	}
	p.BudgetGap = p.Budget - p.Revenue

	// Forecast — the CPM pacing math: MTD + 3-complete-day mean × days after
	// today. A closed month's forecast IS its actuals.
	p.ProjSent, p.ProjRevenue = p.Sent, p.Revenue
	if isCurrent && p.RevenueBasis != "unavailable" {
		daysInMonth := time.Date(today.Year(), today.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
		p.Avg3dSent = cpmMailedAvg3d(sentDays, today)
		_, p.ProjSent, _, _ = cpmPacingMath(0, p.Sent, p.Avg3dSent, today.Day(), daysInMonth)
		after := float64(daysInMonth - today.Day())
		if cpc {
			// Clicks priced at TODAY's rate going forward — a rate change moves
			// the forecast immediately, which constant yield per send would hide.
			p.Avg3dUnits = cpmMailedAvg3d(unitDays, today)
			p.ProjRevenue = p.Revenue + p.Avg3dUnits*after*p.CurrentRate
		} else if p.Sent > 0 {
			p.ProjRevenue = p.Revenue * float64(p.ProjSent) / float64(p.Sent)
		}
		if p.Budget > p.Revenue {
			p.RequiredPerDay = (p.Budget - p.Revenue) / (after + 1)
		}
	}
	p.ProjRevenue = math.Round(p.ProjRevenue*100) / 100
	p.ProjEcpm = nonCpmDiv(p.ProjRevenue*1000, float64(p.ProjSent))
	if p.Budget > 0 {
		p.ProjBudgetPct = p.ProjRevenue / p.Budget
	}
	p.ProjGap = p.Budget - p.ProjRevenue
	return p
}

// ─── Loaders ────────────────────────────────────────────────────────────────

func (h *CpmPlannerHandlers) nonCpmRevenueConfigs(orgID string, month time.Time) (map[string]*nonCpmRevenueConfig, map[string][]nonCpmRate, error) {
	cfgs := map[string]*nonCpmRevenueConfig{}
	rates := map[string][]nonCpmRate{}
	rows, err := h.db.Query(`
		SELECT group_name, budget, payout_model, paid_revenue, notes
		FROM mailing_offer_revenue_config
		WHERE organization_id = $1 AND month = $2::date`, orgID, month.Format("2006-01-02"))
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var g string
		c := &nonCpmRevenueConfig{}
		if err := rows.Scan(&g, &c.Budget, &c.PayoutModel, &c.PaidRevenue, &c.Notes); err != nil {
			rows.Close()
			return nil, nil, err
		}
		cfgs[g] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rrows, err := h.db.Query(`
		SELECT group_name, to_char(effective_from, 'YYYY-MM-DD'), rate
		FROM mailing_offer_payout_rates
		WHERE organization_id = $1
		ORDER BY group_name, effective_from`, orgID)
	if err != nil {
		return nil, nil, err
	}
	defer rrows.Close()
	for rrows.Next() {
		var g string
		var r nonCpmRate
		if err := rrows.Scan(&g, &r.EffectiveFrom, &r.Rate); err != nil {
			return nil, nil, err
		}
		rates[g] = append(rates[g], r)
	}
	return cfgs, rates, rrows.Err()
}

// ─── Handler ────────────────────────────────────────────────────────────────

// HandleUpsertRevenueConfig PUT /cpm-planner/revenue-config
//
//	{"group_name":"eHealth CPC","month":"2026-10","budget":5000,
//	 "payout_model":"cpc","paid_revenue":0,"notes":"",
//	 "rates":[{"effective_from":"2026-10-01","rate":1.8},{"effective_from":"2026-10-17","rate":2}]}
//
// rates, when present, REPLACES the group's whole rate schedule (rates are not
// month-scoped); omit it to leave the schedule untouched.
func (h *CpmPlannerHandlers) HandleUpsertRevenueConfig(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	var in struct {
		GroupName   string        `json:"group_name"`
		Month       string        `json:"month"`
		Budget      float64       `json:"budget"`
		PayoutModel string        `json:"payout_model"`
		PaidRevenue float64       `json:"paid_revenue"`
		Notes       string        `json:"notes"`
		Rates       *[]nonCpmRate `json:"rates"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	group := strings.TrimSpace(in.GroupName)
	if group == "" {
		respondError(w, http.StatusBadRequest, "group_name required")
		return
	}
	month, err := parseMonthArg(in.Month)
	if err != nil {
		respondError(w, http.StatusBadRequest, "month must be YYYY-MM")
		return
	}
	model := strings.ToLower(strings.TrimSpace(in.PayoutModel))
	if model != "cpl" && model != "cpa" && model != "cpc" {
		respondError(w, http.StatusBadRequest, "payout_model must be cpl, cpa or cpc")
		return
	}
	if in.Budget < 0 || in.PaidRevenue < 0 {
		respondError(w, http.StatusBadRequest, "budget and paid_revenue must be >= 0")
		return
	}
	if in.Rates != nil {
		seen := map[string]bool{}
		for _, rt := range *in.Rates {
			if _, err := time.Parse("2006-01-02", rt.EffectiveFrom); err != nil {
				respondError(w, http.StatusBadRequest, fmt.Sprintf("rate effective_from %q must be YYYY-MM-DD", rt.EffectiveFrom))
				return
			}
			if rt.Rate <= 0 || seen[rt.EffectiveFrom] {
				respondError(w, http.StatusBadRequest, fmt.Sprintf("rate for %s must be > 0 and dated once", rt.EffectiveFrom))
				return
			}
			seen[rt.EffectiveFrom] = true
		}
	}

	tx, err := h.db.Begin()
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("begin: %v", err))
		return
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(`
		INSERT INTO mailing_offer_revenue_config
			(organization_id, group_name, month, budget, payout_model, paid_revenue, notes, source, updated_at)
		VALUES ($1, $2, $3::date, $4, $5, $6, $7, 'operator', NOW())
		ON CONFLICT (organization_id, group_name, month) DO UPDATE
		SET budget = EXCLUDED.budget, payout_model = EXCLUDED.payout_model,
		    paid_revenue = EXCLUDED.paid_revenue, notes = EXCLUDED.notes,
		    source = 'operator', updated_at = NOW()`,
		orgID, group, month.Format("2006-01-02"), in.Budget, model, in.PaidRevenue, strings.TrimSpace(in.Notes)); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("save config: %v", err))
		return
	}
	if in.Rates != nil {
		if _, err := tx.Exec(`DELETE FROM mailing_offer_payout_rates WHERE organization_id = $1 AND group_name = $2`,
			orgID, group); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("clear rates: %v", err))
			return
		}
		for _, rt := range *in.Rates {
			if _, err := tx.Exec(`
				INSERT INTO mailing_offer_payout_rates (organization_id, group_name, effective_from, rate, source, updated_at)
				VALUES ($1, $2, $3::date, $4, 'operator', NOW())`, orgID, group, rt.EffectiveFrom, rt.Rate); err != nil {
				respondError(w, http.StatusInternalServerError, fmt.Sprintf("save rate %s: %v", rt.EffectiveFrom, err))
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("commit: %v", err))
		return
	}
	// A config change must show on the next load, not after the response TTL.
	h.evMu.Lock()
	for k := range h.nonCpmCache {
		if strings.HasPrefix(k, orgID+"|") {
			delete(h.nonCpmCache, k)
		}
	}
	h.evMu.Unlock()
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"group_name": group, "month": month.Format("2006-01"), "status": "saved",
	})
}
