package api

// dashboard_console_audience_growth.go — Dashboard Console: audience-growth card.
//
// GET /api/mailing/dashboard/audience-growth
//
// Read-only. Returns acquisition vs churn over the last 7 America/Denver days
// (today + the prior 6), computed DIRECTLY over PG mailing_subscribers.
//
// Why this exists (BUG-10, 2026-07-02): the dashboard previously derived growth
// from the Athena "audience breakdown" lake table, which is stale (last refresh
// 2026-06-09) and reported a green "+0 net" while PG showed ~371,724 acquired in
// the trailing 7 days. Delivery/engagement outcomes belong to the lake
// (METRIC_CONTRACT §1), but audience *membership* (who was acquired / who
// churned) is authoritative in PG mailing_subscribers — so this card reads PG.
//
// Windows are America/Denver calendar days (METRIC_CONTRACT §4): the floor is
// (now() AT TIME ZONE 'America/Denver')::date - 6, i.e. exactly 7 Denver days
// inclusive (fixes the "7d" label, BUG-11). Org-scoped via GetOrgIDFromRequest.
//
//   acquired_7d = subscribers whose COALESCE(subscribed_at, created_at) Denver
//                 day is >= the floor.
//   churned_7d  = subscribers who left in the window — prefer unsubscribed_at
//                 when set (cleanest signal); otherwise a terminal status
//                 (bounced/complained/blacklisted/unsubscribed) whose updated_at
//                 Denver day is >= the floor.

import (
	"context"
	"net/http"
	"time"
)

// VersionDashboardAudienceGrowth tracks this endpoint's response contract.
//
// 1.0 (2026-07-02): initial — PG mailing_subscribers acquisition vs churn over
// the last 7 Denver days, org-scoped (replaces the stale Athena audience table).
const VersionDashboardAudienceGrowth = "1.0"

// HandleDashboardAudienceGrowth serves GET /api/mailing/dashboard/audience-growth.
func (s *Server) HandleDashboardAudienceGrowth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "organization context required")
		return
	}

	// Single round-trip: two scalar subqueries over the same Denver-day floor.
	// bounds computes the floor once so both counts share exactly one 7-day
	// window (today + the prior 6 Denver days).
	const query = `
		WITH bounds AS (
			SELECT ((NOW() AT TIME ZONE 'America/Denver')::date - 6) AS floor_date
		)
		SELECT
			(SELECT COUNT(*)
			   FROM mailing_subscribers s, bounds b
			  WHERE s.organization_id = $1
			    AND (COALESCE(s.subscribed_at, s.created_at) AT TIME ZONE 'America/Denver')::date >= b.floor_date
			) AS acquired,
			(SELECT COUNT(*)
			   FROM mailing_subscribers s, bounds b
			  WHERE s.organization_id = $1
			    AND (
			        (s.unsubscribed_at IS NOT NULL
			         AND (s.unsubscribed_at AT TIME ZONE 'America/Denver')::date >= b.floor_date)
			     OR (s.unsubscribed_at IS NULL
			         AND s.status IN ('bounced','complained','blacklisted','unsubscribed')
			         AND (s.updated_at AT TIME ZONE 'America/Denver')::date >= b.floor_date)
			    )
			) AS churned`

	var acquired, churned int64
	if err := s.mailingDB.QueryRowContext(ctx, query, orgID).Scan(&acquired, &churned); err != nil {
		respondError(w, http.StatusInternalServerError, "database query failed")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"acquired_7d":  acquired,
		"churned_7d":   churned,
		"net":          acquired - churned,
		"window_days":  7,
		"api_version":  VersionDashboardAudienceGrowth,
		"generated_at": time.Now().UTC(),
	})
}
