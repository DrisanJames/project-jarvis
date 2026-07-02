package api

// dashboard_console_trending_offers.go — Dashboard Console: trending-offers card.
//
// GET /api/mailing/dashboard/trending-offers?hours=24
//
// Read-only. Ranks offers by CONVERSIONS recorded in the requested window.
//
// Source of truth: mailing_offer_suppressions rows with reason='converted' are
// the platform's per-(offer,subscriber) conversion ledger — the same ledger the
// click-drip enroller consults to skip already-converted users
// (journey_event_enroller.go) and that the Everflow conversion import writes.
// Each such row is one conversion for (offer_id, subscriber_id); suppressed_at
// is when it landed. Joining mailing_offers gives the display name.
//
// Org-scoped via GetOrgIDFromRequest → s.organization_id (mailing_offer_suppressions
// carries organization_id NOT NULL, matching sibling offer-suppression handlers).
//
// Conversions are SPARSE by design (CPM broadcasts are $0 CPA; only CPA/CPL
// offers write 'converted' rows, and attribution is offer-tagged) — an empty
// rows array is the normal, expected result, not an error.
import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// VersionDashboardTrendingOffers tracks this endpoint's response contract.
//
// 1.0 (2026-07-02): initial — offers ranked by reason='converted' suppressions
// in the window, org-scoped.
const VersionDashboardTrendingOffers = "1.0"

type trendingOfferRow struct {
	OfferID     string `json:"offer_id"`
	Name        string `json:"name"`
	Conversions int    `json:"conversions"`
}

// HandleDashboardTrendingOffers serves GET /api/mailing/dashboard/trending-offers.
func (s *Server) HandleDashboardTrendingOffers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "organization context required")
		return
	}

	hours := 24
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 168 {
			hours = n
		}
	}
	interval := strconv.Itoa(hours) + " hours"

	query := `
		SELECT s.offer_id, o.name, COUNT(*) AS conversions
		FROM mailing_offer_suppressions s
		JOIN mailing_offers o ON o.id = s.offer_id
		WHERE s.reason = 'converted'
		  AND s.suppressed_at > NOW() - $1::interval
		  AND s.organization_id = $2
		GROUP BY 1, 2
		ORDER BY 3 DESC
		LIMIT 10`

	rows, err := s.mailingDB.QueryContext(ctx, query, interval, orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "database query failed")
		return
	}
	defer rows.Close()

	out := make([]trendingOfferRow, 0, 10)
	total := 0
	for rows.Next() {
		var row trendingOfferRow
		if err := rows.Scan(&row.OfferID, &row.Name, &row.Conversions); err != nil {
			continue
		}
		total += row.Conversions
		out = append(out, row)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"hours":             hours,
		"rows":              out,
		"total_conversions": total,
		"api_version":       VersionDashboardTrendingOffers,
		"generated_at":      time.Now().UTC(),
	})
}
