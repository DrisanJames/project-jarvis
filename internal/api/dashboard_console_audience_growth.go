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
// 1.1 (2026-07-02): SARGABLE rewrite. v1.0 wrapped the date columns in
//     (col AT TIME ZONE 'America/Denver')::date, which is not index-usable, so
//     each of the two counts was a full seq scan of ~13M rows (~25s total) and
//     the 8s handler timeout returned 500 "database query failed". Now the
//     Denver-day floor is computed in Go as a UTC timestamptz and compared
//     against the raw columns (created_at / unsubscribed_at), which the new
//     concurrent indexes (idx_subscribers_org_created / _org_unsubscribed) serve
//     as range scans. Acquisition keys on created_at (row-creation = acquired);
//     churn keys on terminal status + updated_at (unsubscribed_at is unpopulated
//     in this DB — it returns 0; the real "left" signal is a terminal status
//     whose row was last changed in-window), served by a partial index over the
//     small terminal-status subset (~21k rows).
const VersionDashboardAudienceGrowth = "1.1"

// HandleDashboardAudienceGrowth serves GET /api/mailing/dashboard/audience-growth.
func (s *Server) HandleDashboardAudienceGrowth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "organization context required")
		return
	}

	// Denver-day floor as a UTC timestamptz: midnight (America/Denver) of
	// (today - 6 days) = exactly 7 Denver days inclusive of today. Computed in
	// Go so the SQL predicate is a plain `col >= $2` range the indexes can serve.
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		loc = time.UTC
	}
	nowD := time.Now().In(loc)
	floor := time.Date(nowD.Year(), nowD.Month(), nowD.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -6)

	const query = `
		SELECT
			(SELECT COUNT(*) FROM mailing_subscribers
			  WHERE organization_id = $1 AND created_at >= $2) AS acquired,
			(SELECT COUNT(*) FROM mailing_subscribers
			  WHERE organization_id = $1
			    AND status IN ('bounced','complained','blacklisted','unsubscribed')
			    AND updated_at >= $2) AS churned`

	var acquired, churned int64
	if err := s.mailingDB.QueryRowContext(ctx, query, orgID, floor).Scan(&acquired, &churned); err != nil {
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
