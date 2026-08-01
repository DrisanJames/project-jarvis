package api

// NON-CPM PERFORMANCE (operator 2026-08-01).
//
// The CPM Planner answers "are the paid deals pacing to budget". This answers
// the other half: everything we mail that ISN'T on a CPM deal, as the operator's
// funnel — VOLUME → CLICKERS → CONVERSIONS.
//
// Sources, and why they differ (state this on the screen, never blur it):
//
//	volume      ATHENA lake (mailing delivery truth; email_events delivered)
//	clickers    ATHENA lake (analytics.ActionClicks — DISTINCT subscriber_id on
//	            navigational links; asset fetches and compliance links removed)
//	conversions POSTGRES. The lake carries NO conversion events — verified
//	            2026-08-01, ignite_analytics.email_events event_type ∈
//	            {attempted, delivered, open, click, bounce family, delivery_delay,
//	            unsubscribe, complaint}. Conversions are Everflow postbacks
//	            landing in mailing_offer_suppressions(reason='converted'), so
//	            they cannot come from Athena no matter how the query is written.
//
// Both lake metrics are read from mailing_offer_alignment_snapshot, which the
// offer-alignment worker rebuilds every 3h. This endpoint does NOT call Athena
// itself, deliberately: a per-request lake fan-out over every offer is the exact
// shape that produced the 2026-07 S3 GET storm (~830M GETs/day, ~$330/day).
//
// CPM vs non-CPM is decided by dealCampaignMapCTE — the single source of truth
// for deal attribution — NOT by a name/slug dictionary. A dictionary was tried
// first and misread four live offer keys (kqckq7, ctcdkm2, 2j2crs, fidelity)
// whose deals carry no slug-map row. Deciding by real campaign overlap also
// yields the PARTIAL attribution signal below for free.

import (
	"database/sql"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// nonCpmAttributionFullPct — at or above this share of an offer key's campaigns
// sitting inside one deal's attribution map, the offer counts as that deal's
// (CPM) and is reported as fully attributed. Below it the offer still belongs to
// the deal but is flagged PARTIAL: real volume is being mailed under the offer
// that the deal is not billing. Below nonCpmAttributionNonePct it is non-CPM.
const (
	nonCpmAttributionFullPct = 0.90
	nonCpmAttributionNonePct = 0.01
)

// nonCpmOfferRow is one offer's funnel for the window.
type nonCpmOfferRow struct {
	OfferKey  string `json:"offer_key"`
	OfferName string `json:"offer_name"`

	Delivered int64 `json:"delivered"` // lake
	Clickers  int64 `json:"clickers"`  // lake, DISTINCT subscriber_id
	Clicks    int64 `json:"clicks"`    // lake
	// Conversions is PG (Everflow postback ledger) — see the file header.
	Conversions int64 `json:"conversions"`

	// Rates, derived once here so the UI never re-derives them differently.
	ClickerRate float64 `json:"clicker_rate"` // clickers / delivered
	ConvRate    float64 `json:"conv_rate"`    // conversions / clickers

	IsCPM    bool   `json:"is_cpm"`
	DealID   string `json:"deal_id"`
	DealName string `json:"deal_name"`

	// Attribution: "full" | "partial" | "none". PARTIAL is the actionable one —
	// the offer maps to a CPM deal but most of its campaigns fall outside the
	// deal's attribution, so that volume is being mailed and NOT billed.
	Attribution     string `json:"attribution"`
	Campaigns       int64  `json:"campaigns"`
	CampaignsInDeal int64  `json:"campaigns_in_deal"`
}

type nonCpmTotals struct {
	Offers      int     `json:"offers"`
	Delivered   int64   `json:"delivered"`
	Clickers    int64   `json:"clickers"`
	Clicks      int64   `json:"clicks"`
	Conversions int64   `json:"conversions"`
	ClickerRate float64 `json:"clicker_rate"`
	ConvRate    float64 `json:"conv_rate"`
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

// nonCpmOfferDealMap returns, per lowercased offer_key, the campaign totals and
// the dominant CPM deal (most campaigns attributed). Campaign floor is the
// window, matching the snapshot the funnel numbers come from.
type nonCpmOfferDeal struct {
	dealID, dealName string
	campaigns        int64
	inDeal           int64
}

func (h *CpmPlannerHandlers) nonCpmOfferDealMap(orgID string, windowDays int) (map[string]*nonCpmOfferDeal, error) {
	q := dealCampaignMapCTE + `,
		c AS (
			SELECT id, lower(offer_key) AS okey
			FROM mailing_campaigns
			WHERE organization_id = $1
			  AND COALESCE(offer_key,'') <> ''
			  AND created_at >= (CURRENT_DATE - $2::int)
		)
		SELECT c.okey,
		       COALESCE(dm.deal_id::text, ''),
		       COALESCE(d.name, ''),
		       COUNT(*)
		FROM c
		LEFT JOIN dm ON dm.campaign_id = c.id
		LEFT JOIN mailing_cpm_deals d ON d.id = dm.deal_id
		GROUP BY 1, 2, 3`
	rows, err := h.db.Query(q, orgID, windowDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*nonCpmOfferDeal{}
	// best[okey] = campaigns attributed to the winning deal so far.
	best := map[string]int64{}
	for rows.Next() {
		var okey, dealID, dealName string
		var n int64
		if err := rows.Scan(&okey, &dealID, &dealName, &n); err != nil {
			return nil, err
		}
		e := out[okey]
		if e == nil {
			e = &nonCpmOfferDeal{}
			out[okey] = e
		}
		e.campaigns += n
		if dealID != "" {
			e.inDeal += n
			if n > best[okey] {
				best[okey] = n
				e.dealID, e.dealName = dealID, dealName
			}
		}
	}
	return out, rows.Err()
}

// HandleNonCpmPerformance GET /cpm-planner/non-cpm?window=7|30
//
// Month-scoped it is NOT: the lake snapshot is built on trailing 7/30-day
// windows, and reporting a "August 1-31" number off a trailing-30d snapshot
// would be a lie for most of the month. The CPM deals' calendar-month pacing
// lives on /pacing; this surface is the trailing-window performance read.
//
// Served on demand (mount / explicit refresh), never on a poll timer — the
// attribution join is a full pass over the window's campaigns (~8s measured on
// prod, 2026-08-01). Same posture as HandleCurrentMonthPacing.
func (h *CpmPlannerHandlers) HandleNonCpmPerformance(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)

	windowDays := 30
	if v := r.URL.Query().Get("window"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || (n != 7 && n != 30) {
			respondError(w, http.StatusBadRequest, "window must be 7 or 30")
			return
		}
		windowDays = n
	}

	// Funnel numbers straight off the lake-backed snapshot, folded ISP → offer.
	type snapAgg struct {
		name                                     string
		delivered, clickers, clicks, conversions int64
	}
	agg := map[string]*snapAgg{}
	var refreshedAt sql.NullTime
	rows, err := h.db.Query(`
		SELECT offer_key, MAX(offer_name),
		       SUM(delivered), SUM(lake_clickers), SUM(lake_clicks), SUM(conversions),
		       MAX(refreshed_at)
		FROM mailing_offer_alignment_snapshot
		WHERE organization_id = $1 AND window_days = $2 AND offer_key <> $3
		GROUP BY offer_key`, orgID, windowDays, offerAlignmentMetaKey)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("read offer snapshot: %v", err))
		return
	}
	func() {
		defer rows.Close()
		for rows.Next() {
			var key, name string
			var delivered, clickers, clicks, conversions int64
			var refreshed sql.NullTime
			if err := rows.Scan(&key, &name, &delivered, &clickers, &clicks, &conversions, &refreshed); err != nil {
				log.Printf("[CpmPlanner] non-cpm snapshot scan: %v", err)
				continue
			}
			agg[key] = &snapAgg{name: name, delivered: delivered, clickers: clickers, clicks: clicks, conversions: conversions}
			if refreshed.Valid && (!refreshedAt.Valid || refreshed.Time.After(refreshedAt.Time)) {
				refreshedAt = refreshed
			}
		}
	}()
	if len(agg) == 0 {
		// Never built (or the worker has not run since deploy) — say so rather
		// than rendering a convincing all-zero table.
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"window_days": windowDays, "offers": []nonCpmOfferRow{},
			"building": true,
			"note":     "offer-alignment snapshot has not been built for this window yet (rebuilds every 3h; POST /api/mailing/offer-alignment/refresh to force)",
		})
		return
	}

	dealMap, err := h.nonCpmOfferDealMap(orgID, windowDays)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("offer→deal attribution: %v", err))
		return
	}

	out := make([]nonCpmOfferRow, 0, len(agg))
	var cpm, nonCpm nonCpmTotals
	for key, a := range agg {
		row := nonCpmOfferRow{
			OfferKey: key, OfferName: a.name,
			Delivered: a.delivered, Clickers: a.clickers, Clicks: a.clicks,
			Conversions: a.conversions,
			Attribution: "none",
		}
		if dm := dealMap[key]; dm != nil {
			row.Campaigns, row.CampaignsInDeal = dm.campaigns, dm.inDeal
			row.DealID, row.DealName = dm.dealID, dm.dealName
			share := nonCpmDiv(float64(dm.inDeal), float64(dm.campaigns))
			switch {
			case share >= nonCpmAttributionFullPct:
				row.IsCPM, row.Attribution = true, "full"
			case share >= nonCpmAttributionNonePct:
				row.IsCPM, row.Attribution = true, "partial"
			}
		}
		row.ClickerRate = nonCpmDiv(float64(row.Clickers), float64(row.Delivered))
		row.ConvRate = nonCpmDiv(float64(row.Conversions), float64(row.Clickers))

		t := &nonCpm
		if row.IsCPM {
			t = &cpm
		}
		t.Offers++
		t.Delivered += row.Delivered
		t.Clickers += row.Clickers
		t.Clicks += row.Clicks
		t.Conversions += row.Conversions
		out = append(out, row)
	}
	for _, t := range []*nonCpmTotals{&cpm, &nonCpm} {
		t.ClickerRate = nonCpmDiv(float64(t.Clickers), float64(t.Delivered))
		t.ConvRate = nonCpmDiv(float64(t.Conversions), float64(t.Clickers))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Delivered > out[j].Delivered })

	resp := map[string]interface{}{
		"window_days": windowDays,
		"offers":      out,
		"totals":      map[string]interface{}{"cpm": cpm, "non_cpm": nonCpm},
		// Sources are part of the payload so the screen can label each column —
		// mixing a lake number and a PG number in one row without saying so is
		// how the two-source confusion starts (METRIC_CONTRACT).
		"sources": map[string]string{
			"delivered":   "athena:ignite_analytics.email_events",
			"clickers":    "athena:ignite_analytics.email_events (navigational links, DISTINCT subscriber_id)",
			"clicks":      "athena:ignite_analytics.email_events (navigational links)",
			"conversions": "postgres:mailing_offer_suppressions (everflow postbacks) — the lake has no conversion events",
		},
		// The snapshot enumerates at most offerAlignmentMaxOffers offers per org,
		// ranked by stamped-campaign count then click volume. Reported rather
		// than silently truncated.
		"offer_cap":     offerAlignmentMaxOffers,
		"offer_cap_hit": len(agg) >= offerAlignmentMaxOffers,
	}
	if refreshedAt.Valid {
		resp["refreshed_at"] = refreshedAt.Time.Format(time.RFC3339)
		resp["stale"] = time.Since(refreshedAt.Time) > offerAlignmentStalenessThreshold
	}
	respondJSON(w, http.StatusOK, resp)
}
