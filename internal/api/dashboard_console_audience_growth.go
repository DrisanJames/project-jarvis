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
	"database/sql"
	"log"
	"net/http"
	"time"
)

// VersionDashboardAudienceGrowth tracks this endpoint's response contract.
//
// 1.0 (2026-07-02): initial — PG mailing_subscribers acquisition vs churn over
// the last 7 Denver days, org-scoped (replaces the stale Athena audience table).
// 1.1 (2026-07-02): SARGABLE rewrite. v1.0 wrapped the date columns in
//
//	(col AT TIME ZONE 'America/Denver')::date, which is not index-usable, so
//	each of the two counts was a full seq scan of ~13M rows (~25s total) and
//	the 8s handler timeout returned 500 "database query failed". Now the
//	Denver-day floor is computed in Go as a UTC timestamptz and compared
//	against the raw columns (created_at / unsubscribed_at), which the new
//	concurrent indexes (idx_subscribers_org_created / _org_unsubscribed) serve
//	as range scans. Acquisition keys on created_at (row-creation = acquired);
//	churn keys on terminal status + updated_at (unsubscribed_at is unpopulated
//	in this DB — it returns 0; the real "left" signal is a terminal status
//	whose row was last changed in-window), served by a partial index over the
//	small terminal-status subset (~21k rows).
//
// 1.2 (2026-07-02): add the "Activated" pool (operator request) — early-funnel
//
//	subscribers we have mailed ≤4 times who have opened or clicked
//	(total_emails_received BETWEEN 1 AND 4 AND (total_opens>0 OR total_clicks>0)).
//	This is a STATE/pool count (all-time state, like "available pool"), NOT a
//	7-day flow like acquired/churned. Served by the partial index
//	idx_subscribers_activated over the ~19k matching subset; degrades to a
//	~2.3s parallel seq scan before the concurrent index lands (well within the
//	8s budget).
const VersionDashboardAudienceGrowth = "1.3"

type audienceGrowthISP struct {
	ISP      string `json:"isp"`
	Acquired int64  `json:"acquired"`
	Churned  int64  `json:"churned"`
	Net      int64  `json:"net"`
}

// HandleDashboardAudienceGrowth serves GET /api/mailing/dashboard/audience-growth.
func (s *Server) HandleDashboardAudienceGrowth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "organization context required")
		return
	}

	// (today - 6 days) = exactly 7 Denver days inclusive of today.
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		loc = time.UTC
	}
	floor := time.Now().In(loc).AddDate(0, 0, -6).Format("2006-01-02")

	rows, err := s.mailingDB.QueryContext(ctx, `
		SELECT isp, COALESCE(SUM(acquired),0), COALESCE(SUM(churned),0), MAX(computed_at)
		  FROM mailing_audience_growth_daily
		 WHERE organization_id = $1 AND day >= $2::date
		 GROUP BY isp
		 ORDER BY SUM(acquired) DESC, isp`, orgID, floor)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "database query failed")
		return
	}
	defer rows.Close()

	byISP := []audienceGrowthISP{}
	var acquired, churned int64
	var computedAt sql.NullTime
	for rows.Next() {
		var row audienceGrowthISP
		var rowComputed sql.NullTime
		if err := rows.Scan(&row.ISP, &row.Acquired, &row.Churned, &rowComputed); err != nil {
			respondError(w, http.StatusInternalServerError, "database query failed")
			return
		}
		row.Net = row.Acquired - row.Churned
		acquired += row.Acquired
		churned += row.Churned
		if rowComputed.Valid && (!computedAt.Valid || rowComputed.Time.After(computedAt.Time)) {
			computedAt = rowComputed
		}
		byISP = append(byISP, row)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "database query failed")
		return
	}

	// State/pool count (all-time), not a 7-day flow; served by
	// idx_subscribers_activated.
	var activated sql.NullInt64
	if err := s.mailingDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_subscribers
		 WHERE organization_id = $1
		   AND total_emails_received BETWEEN 1 AND 4
		   AND (total_opens > 0 OR total_clicks > 0)`, orgID).Scan(&activated); err != nil {
		log.Printf("[audience-growth] activated org=%s: %v", orgID, err)
		activated = sql.NullInt64{}
	}

	resp := map[string]interface{}{
		"ready":        computedAt.Valid,
		"acquired_7d":  acquired,
		"churned_7d":   churned,
		"activated":    nil,
		"net":          acquired - churned,
		"by_isp":       byISP,
		"computed_at":  nil,
		"window_days":  7,
		"api_version":  VersionDashboardAudienceGrowth,
		"generated_at": time.Now().UTC(),
	}
	if activated.Valid {
		resp["activated"] = activated.Int64
	}
	if computedAt.Valid {
		resp["computed_at"] = computedAt.Time.UTC()
	}
	respondJSON(w, http.StatusOK, resp)
}
