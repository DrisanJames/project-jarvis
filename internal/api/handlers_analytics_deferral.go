package api

import (
	"net/http"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/analytics"
)

// HandleDeferralFunnel exposes the per-ISP deferral funnel over the Athena event
// lake (internal/analytics/deferral_funnel.go). For every message
// (campaign_id, lower(email)) DEFERRED in the [from,to] Denver-day window, it
// reports how many later RECOVERED (delivered), are still PENDING, or BOUNCED.
//
// Query params: from, to (YYYY-MM-DD, default last 7 days inclusive UTC), brand
// (optional apex domain). Validation lives in analytics.DeferralFunnel — bad
// input comes back as 400 {"error":...}. READ ONLY; mirrors HandleLakeBreakdown's
// disabled handling and date defaulting exactly. When the reader is disabled it
// returns 200 with {"disabled":true,"rows":[],"total":{...}}.
func (s *Server) HandleDeferralFunnel(w http.ResponseWriter, r *http.Request) {
	_, _ = GetOrgIDFromRequest(r) // consistency with sibling handlers; lake not org-partitioned

	emptyTotal := map[string]int64{"deferred": 0, "recovered": 0, "pending": 0, "bounced": 0}

	if !analytics.ReaderEnabled() {
		respondJSON(w, http.StatusOK, map[string]interface{}{"disabled": true, "rows": []interface{}{}, "total": emptyTotal})
		return
	}

	q := r.URL.Query()
	from := q.Get("from")
	to := q.Get("to")
	if from == "" || to == "" {
		now := time.Now().UTC()
		if to == "" {
			to = now.Format("2006-01-02")
		}
		if from == "" {
			from = now.AddDate(0, 0, -6).Format("2006-01-02")
		}
	}
	brand := q.Get("brand")

	rows, err := analytics.DeferralFunnel(r.Context(), from, to, brand)
	if err != nil {
		if analytics.IsDisabledErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{"disabled": true, "rows": []interface{}{}, "total": emptyTotal})
			return
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	total := map[string]int64{"deferred": 0, "recovered": 0, "pending": 0, "bounced": 0}
	for _, row := range rows {
		total["deferred"] += row.Deferred
		total["recovered"] += row.Recovered
		total["pending"] += row.Pending
		total["bounced"] += row.Bounced
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"from":  from,
		"to":    to,
		"rows":  rows,
		"total": total,
	})
}
