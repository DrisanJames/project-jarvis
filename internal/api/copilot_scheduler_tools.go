package api

// Scheduler read tools for the Campaign Copilot (repurposed into a Scheduler).
//
// Two tools live here:
//
//   get_cpm_budgets   — per active CPM deal: budget, delivered/spend, pace vs
//                       goal, and the GAP still to fill (volume + $). It REUSES
//                       the CPM planner's own computation (cpm_planner_handlers.go
//                       loadDeals → loadProgress: planned_volume, delivered,
//                       eCPA/pace math) rather than reinventing any of it. The
//                       heavy per-deal delivered scan is served from the planner's
//                       background event-count cache; when that cache is cold
//                       (fresh boot) the tool returns plan-level numbers and flags
//                       delivered as "warming" instead of blocking the chat path
//                       on a multi-minute synchronous scan.
//
//   get_offer_winners — per offer: the APPROVED creatives/subjects. Intended to
//                       be ranked by REAL human Everflow clicks, but the winner
//                       accumulator (agents/scheduling/data/everflow_clicks.jsonl)
//                       is a repo-root file NOT present in the deployed container,
//                       and no everflow_clicks prod table exists yet. So this ships
//                       as an HONEST STUB: it returns the approved-copy pool in
//                       positional/approval order with ranking_available=false and
//                       an explicit flag — it never fabricates a click ranking.

import (
	"context"
	"log"
	"math"
	"strings"
	"sync"
)

// ─── get_cpm_budgets ─────────────────────────────────────────────────────────

// The Copilot has only *sql.DB, not the server's live *CpmPlannerHandlers (which
// owns the warm event-count cache). To reuse the planner's real computation
// without changing the copilot struct or server wiring, we lazily build ONE
// shared planner instance (process-lifetime, sync.Once) whose background warmer
// keeps the per-deal delivered aggregate hot. Tradeoff (flagged in the report):
// this is a SECOND cache warmer duplicating the server's; the cleaner fix is to
// pass the existing *CpmPlannerHandlers into the copilot, which is out of this
// change's file scope.
var (
	copilotCpmOnce    sync.Once
	copilotCpmPlanner *CpmPlannerHandlers
)

func (c *CampaignCopilot) cpmPlanner() *CpmPlannerHandlers {
	copilotCpmOnce.Do(func() {
		copilotCpmPlanner = NewCpmPlannerHandlers(c.db)
	})
	return copilotCpmPlanner
}

func (c *CampaignCopilot) toolGetCpmBudgets(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	includeAll, _ := args["include_all_statuses"].(bool)

	planner := c.cpmPlanner()

	// Non-blocking cache probe. When cold (fresh boot, before the first warm
	// cycle finishes) we DELIBERATELY avoid loadDeals — its cold path runs the
	// multi-minute synchronous delivered scan, which would hang the 120s chat
	// request. Instead we return plan-level numbers and flag delivered warming.
	warm := planner.cachedEventCounts(orgID) != nil

	if !warm {
		return c.cpmBudgetsColdPlanOnly(orgID, includeAll)
	}

	deals, err := planner.loadDeals(orgID, "")
	if err != nil {
		log.Printf("[CampaignCopilot] toolGetCpmBudgets loadDeals: %v", err)
		return map[string]string{"error": err.Error()}
	}

	out := []map[string]interface{}{}
	for i := range deals {
		d := &deals[i]
		if !includeAll && d.Status != "active" {
			continue
		}
		p := d.Progress

		// CPM is billed on DELIVERED (planner doctrine, operator 2026-06-18):
		// spend = delivered/1000 * eCPM goal.
		spend := float64(p.Delivered) / 1000.0 * d.EcpmGoal
		budgetRemaining := d.Budget - spend
		if budgetRemaining < 0 {
			budgetRemaining = 0
		}
		volGap := d.PlannedVolume - p.Delivered
		if volGap < 0 {
			volGap = 0
		}
		convGap := d.ConversionsNeeded - p.Conversions
		if convGap < 0 {
			convGap = 0
		}

		row := map[string]interface{}{
			"deal_id":              d.ID,
			"name":                 d.Name,
			"offer_name":           d.OfferName,
			"status":               d.Status,
			"budget":               round2(d.Budget),
			"ecpm_goal":            d.EcpmGoal,
			"ecpa_goal":            d.EcpaGoal,
			"planned_volume":       d.PlannedVolume,
			"delivered":            p.Delivered,
			"pct_volume_delivered": round4(p.PctDelivered),
			"spend_so_far":         round2(spend),
			"budget_remaining":     round2(budgetRemaining),
			"volume_gap_remaining": volGap,
			"conversions_so_far":   p.Conversions,
			"conversions_needed":   d.ConversionsNeeded,
			"conversions_gap":      convGap,
			"actual_ecpa":          round2(p.ActualEcpa),
			"required_daily":       round0(p.RequiredDaily),
			"actual_daily":         round0(p.ActualDaily),
			"on_pace":              p.OnPace,
			"days_elapsed":         p.DaysElapsed,
			"days_to_finish":       d.DaysToFinish,
		}
		if d.EndDate != "" {
			row["end_date"] = d.EndDate
			if p.DaysToDeadline != nil {
				row["days_to_deadline"] = *p.DaysToDeadline
			}
			if p.RequiredDailyToDeadline != nil {
				row["required_daily_to_deadline"] = round0(*p.RequiredDailyToDeadline)
			}
		}
		out = append(out, row)
	}

	return map[string]interface{}{
		"deals":            out,
		"count":            len(out),
		"delivered_source": "warm_cache",
		"note":             "delivered/spend/pace from the CPM planner's live computation; CPM billed on delivered. volume_gap_remaining + conversions_gap show which budgets still need volume.",
	}
}

// cpmBudgetsColdPlanOnly returns plan-level deal numbers (budget, planned volume,
// conversions needed, required daily) with delivered/spend/pace flagged warming.
// Cheap: one indexed deal query, no events scan. Used only while the planner's
// delivered cache is cold so the chat path never blocks on the heavy scan.
func (c *CampaignCopilot) cpmBudgetsColdPlanOnly(orgID string, includeAll bool) interface{} {
	q := cpmDealSelect + " ORDER BY d.created_at DESC"
	rows, err := c.db.Query(q, orgID)
	if err != nil {
		log.Printf("[CampaignCopilot] cpmBudgetsColdPlanOnly query: %v", err)
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()

	out := []map[string]interface{}{}
	for rows.Next() {
		d, _, err := scanCpmDeal(rows)
		if err != nil {
			continue
		}
		if !includeAll && d.Status != "active" {
			continue
		}
		var requiredDaily float64
		if d.DaysToFinish > 0 {
			requiredDaily = float64(d.PlannedVolume) / float64(d.DaysToFinish)
		}
		out = append(out, map[string]interface{}{
			"deal_id":            d.ID,
			"name":               d.Name,
			"offer_name":         d.OfferName,
			"status":             d.Status,
			"budget":             round2(d.Budget),
			"ecpm_goal":          d.EcpmGoal,
			"ecpa_goal":          d.EcpaGoal,
			"planned_volume":     d.PlannedVolume,
			"conversions_needed": d.ConversionsNeeded,
			"required_daily":     round0(requiredDaily),
			"days_to_finish":     d.DaysToFinish,
			"delivered":          nil,
			"spend_so_far":       nil,
		})
	}

	return map[string]interface{}{
		"deals":            out,
		"count":            len(out),
		"delivered_source": "cache_warming",
		"note":             "Planner delivered cache is warming (fresh boot). Plan-level numbers are exact; delivered/spend/pace/gap will populate within a few minutes — re-run then.",
	}
}

// ─── get_offer_winners (honest stub) ─────────────────────────────────────────

func (c *CampaignCopilot) toolGetOfferWinners(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	offer, _ := args["offer"].(string)
	offer = strings.TrimSpace(offer)
	if offer == "" {
		return map[string]string{"error": "offer required (the offer_key / slug, e.g. sams-club, liberty-mutual)"}
	}

	// Approved-copy pool = approved, active offer proofs matching the offer_key.
	// Exact match first; fall back to a contains match so a loose slug still
	// resolves. This is the pool the ranking WOULD reorder — returned here in
	// positional/approval order, NOT click-ranked.
	rows, err := c.db.QueryContext(ctx,
		`SELECT id::text, name, COALESCE(offer_key,''), variants, from_names, approved_domains, approved_isps
		 FROM mailing_offer_proofs
		 WHERE organization_id = $1 AND approval_status = 'approved' AND is_active = TRUE
		   AND (lower(offer_key) = lower($2) OR lower(offer_key) LIKE '%' || lower($2) || '%')
		 ORDER BY updated_at DESC
		 LIMIT 100`, orgID, offer)
	if err != nil {
		log.Printf("[CampaignCopilot] toolGetOfferWinners query: %v", err)
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()

	approved := []map[string]interface{}{}
	allSubjects := []string{}
	for rows.Next() {
		var id, name, offerKey string
		var variantsB, fromNamesB, domainsB, ispsB []byte
		if rows.Scan(&id, &name, &offerKey, &variantsB, &fromNamesB, &domainsB, &ispsB) != nil {
			continue
		}
		variants := unmarshalVariants(variantsB)
		subjects := []string{}
		for _, v := range variants {
			if s := strings.TrimSpace(v.Subject); s != "" {
				subjects = append(subjects, s)
			}
		}
		subjects = cleanStrings(subjects)
		allSubjects = append(allSubjects, subjects...)
		approved = append(approved, map[string]interface{}{
			"proof_id":         id,
			"name":             name,
			"offer_key":        offerKey,
			"subjects":         subjects,
			"from_names":       cleanStrings(unmarshalStrArray(fromNamesB)),
			"approved_domains": unmarshalStrArray(domainsB),
			"approved_isps":    unmarshalStrArray(ispsB),
		})
	}

	return map[string]interface{}{
		"offer":             offer,
		"approved_copy":     approved,
		"count":             len(approved),
		"subjects":          cleanStrings(allSubjects),
		"ranking_available": false,
		"ranking_status":    "positional",
		"ranking_flag":      "WINNER-RANKING PENDING EVERFLOW PROD INGEST — copy is returned in approval/positional order, NOT ranked by human clicks. Do not present this order as performance-ranked.",
		"ranking_reason":    "Real click signal lives in the Python winner engine (agents/scheduling/everflow.py) backed by a repo-root accumulator (agents/scheduling/data/everflow_clicks.jsonl, human-verdict-filtered — ~98% of Everflow clicks are Microsoft/Azure scanners). That JSONL is NOT in the deployed upside-down container and there is no everflow_clicks prod table, so this Go tool cannot rank from it. To enable ranking: create an everflow_clicks prod table + ingest (mirror everflow.py's dedup-by-transaction-id, human_isp filter), then rank within this approved pool.",
	}
}

// round0 rounds to a whole number (round2/round4 already exist package-wide and
// are reused above). Kept local to keep the daily-pace fields as clean integers.
func round0(f float64) float64 { return math.Round(f) }
