package api

// offers_rollup.go — audience unification Phase 3.
//
// Live per-offer campaign rollup over the denormalized mailing_campaigns
// counters (same columns as campaign_tags_report.go — no new metric source,
// METRIC_CONTRACT-neutral). Replaces the structurally dead
// mailing_offer_deployments performance path (it stores template ids).
//
//	GET /api/mailing/offers/rollup?days=30
//	    one row per offer joined on c.offer_id, plus a synthetic
//	    '(unattributed)' row for offer_id IS NULL. conversions = rows in
//	    mailing_offer_suppressions with reason='converted' in the window
//	    (column is suppressed_at — verified cmd/server/main.go:4718).
//	GET /api/mailing/offers/list
//	    lean org-scoped offer catalog for pickers (Campaign Manager wizard).

import (
	"database/sql"
	"net/http"
	"strconv"
	"time"
)

// offerRollupRow is one per-offer aggregate.
type offerRollupRow struct {
	OfferID         string `json:"offer_id"` // "" for the unattributed row
	OfferKey        string `json:"offer_key"`
	OfferName       string `json:"offer_name"`
	Campaigns       int64  `json:"campaigns"`
	TotalRecipients int64  `json:"total_recipients"`
	SentCount       int64  `json:"sent_count"`
	DeliveredCount  int64  `json:"delivered_count"`
	UniqueOpenCount int64  `json:"unique_open_count"`
	UniqueClickCnt  int64  `json:"unique_click_count"`
	HardBounceCount int64  `json:"hard_bounce_count"`
	SoftBounceCount int64  `json:"soft_bounce_count"`
	Conversions     int64  `json:"conversions"`
}

// HandleOffersRollup GET /api/mailing/offers/rollup?days=30
func HandleOffersRollup(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID := getOrgID(r)

		days := 30
		if n, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && n >= 1 && n <= 365 {
			days = n
		}
		from := time.Now().UTC().AddDate(0, 0, -days)

		rows, err := db.QueryContext(r.Context(), `
			SELECT COALESCE(o.id::text, ''),
			       COALESCE(NULLIF(lower(o.landing_page_slug), ''), ''),
			       COALESCE(o.name, '(unattributed)'),
			       COUNT(*),
			       COALESCE(SUM(c.total_recipients), 0),
			       COALESCE(SUM(c.sent_count), 0),
			       COALESCE(SUM(c.delivered_count), 0),
			       COALESCE(SUM(c.unique_open_count), 0),
			       COALESCE(SUM(c.unique_click_count), 0),
			       COALESCE(SUM(c.hard_bounce_count), 0),
			       COALESCE(SUM(c.soft_bounce_count), 0)
			FROM mailing_campaigns c
			LEFT JOIN mailing_offers o ON o.id = c.offer_id
			WHERE c.organization_id = $1
			  AND c.created_at >= $2
			GROUP BY o.id, o.landing_page_slug, o.name
			ORDER BY 7 DESC`, orgID, from)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "offer rollup query failed: "+err.Error())
			return
		}
		defer rows.Close()

		out := []offerRollupRow{}
		byOfferID := map[string]int{}
		for rows.Next() {
			var a offerRollupRow
			if err := rows.Scan(&a.OfferID, &a.OfferKey, &a.OfferName, &a.Campaigns,
				&a.TotalRecipients, &a.SentCount, &a.DeliveredCount,
				&a.UniqueOpenCount, &a.UniqueClickCnt,
				&a.HardBounceCount, &a.SoftBounceCount); err != nil {
				respondError(w, http.StatusInternalServerError, "offer rollup scan failed: "+err.Error())
				return
			}
			if a.OfferID != "" {
				byOfferID[a.OfferID] = len(out)
			}
			out = append(out, a)
		}
		if err := rows.Err(); err != nil {
			respondError(w, http.StatusInternalServerError, "offer rollup rows failed: "+err.Error())
			return
		}

		// Conversions ledger: mailing_offer_suppressions reason='converted',
		// windowed on suppressed_at, merged by offer_id. The unattributed row
		// stays 0 (offer_id is NOT NULL on the ledger).
		convRows, err := db.QueryContext(r.Context(), `
			SELECT offer_id::text, COUNT(*)
			FROM mailing_offer_suppressions
			WHERE organization_id = $1
			  AND reason = 'converted'
			  AND suppressed_at >= $2
			GROUP BY offer_id`, orgID, from)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "offer conversions query failed: "+err.Error())
			return
		}
		defer convRows.Close()
		for convRows.Next() {
			var offerID string
			var n int64
			if err := convRows.Scan(&offerID, &n); err != nil {
				respondError(w, http.StatusInternalServerError, "offer conversions scan failed: "+err.Error())
				return
			}
			if idx, ok := byOfferID[offerID]; ok {
				out[idx].Conversions = n
			}
			// Converted offers with no campaigns in the window are omitted by
			// design — the rollup is a campaign report, not a ledger dump.
		}
		if err := convRows.Err(); err != nil {
			respondError(w, http.StatusInternalServerError, "offer conversions rows failed: "+err.Error())
			return
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"days": days,
			"from": from.Format("2006-01-02"),
			"rows": out,
		})
	}
}

// offerListRow is one picker entry.
type offerListRow struct {
	ID         string `json:"id"`
	Key        string `json:"key"`
	Name       string `json:"name"`
	EverflowID string `json:"everflow_id"`
	Status     string `json:"status"`
}

// HandleOffersList GET /api/mailing/offers/list — lean org-scoped catalog.
func HandleOffersList(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID := getOrgID(r)
		rows, err := db.QueryContext(r.Context(), `
			SELECT id::text, COALESCE(NULLIF(lower(landing_page_slug), ''), ''),
			       name, COALESCE(everflow_offer_id, ''), COALESCE(status, 'draft')
			FROM mailing_offers
			WHERE organization_id = $1
			ORDER BY name ASC`, orgID)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "offers list query failed: "+err.Error())
			return
		}
		defer rows.Close()

		out := []offerListRow{}
		for rows.Next() {
			var o offerListRow
			if err := rows.Scan(&o.ID, &o.Key, &o.Name, &o.EverflowID, &o.Status); err != nil {
				respondError(w, http.StatusInternalServerError, "offers list scan failed: "+err.Error())
				return
			}
			out = append(out, o)
		}
		if err := rows.Err(); err != nil {
			respondError(w, http.StatusInternalServerError, "offers list rows failed: "+err.Error())
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"offers": out})
	}
}
