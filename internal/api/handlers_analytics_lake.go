package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/analytics"
)

// Analytics event-lake READ API. These handlers expose the Athena-backed read
// layer (internal/analytics/reader.go) over the s3://ignite-analytics-lake
// event lake. They are READ ONLY (no Postgres, no send path) and degrade
// gracefully when the reader is disabled (ANALYTICS_ATHENA_OUTPUT unset): the
// status endpoint always works, and summary/events return a {"disabled":true}
// body with HTTP 200 rather than an error.
//
// Routes (wired in server_routes_mailing.go under /api/mailing/analytics/lake):
//
//	GET /status   — write+read enablement and emitter counters
//	GET /summary  — event_type counts over [from,to] (dt, default last 7 days)
//	GET /events   — most-recent events with optional filters
//
// Org resolution mirrors sibling /api/mailing analytics handlers via
// GetOrgIDFromRequest for consistency, even though the lake is not currently
// org-partitioned.

// HandleLakeStatus reports write/read enablement plus the emitter counters.
// Always returns 200 — works even when both sides are disabled.
func (s *Server) HandleLakeStatus(w http.ResponseWriter, r *http.Request) {
	_, _ = GetOrgIDFromRequest(r) // consistency with sibling handlers; lake not org-partitioned

	sent, failed, dropped, enabledWrite := analytics.Stats()
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"enabled_write": enabledWrite,
		"enabled_read":  analytics.ReaderEnabled(),
		"sent":          sent,
		"failed":        failed,
		"dropped":       dropped,
	})
}

// HandleLakeSummary returns event_type counts over a date range. Query params:
// from, to (YYYY-MM-DD). Defaults to the last 7 days (inclusive). When the
// reader is disabled it returns 200 with {"disabled":true,"rows":[]}.
func (s *Server) HandleLakeSummary(w http.ResponseWriter, r *http.Request) {
	_, _ = GetOrgIDFromRequest(r)

	if !analytics.ReaderEnabled() {
		respondJSON(w, http.StatusOK, map[string]interface{}{"disabled": true, "rows": []interface{}{}})
		return
	}

	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	if from == "" || to == "" {
		now := time.Now().UTC()
		if to == "" {
			to = now.Format("2006-01-02")
		}
		if from == "" {
			from = now.AddDate(0, 0, -6).Format("2006-01-02")
		}
	}

	rows, err := analytics.Summary(r.Context(), from, to)
	if err != nil {
		if analytics.IsDisabledErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{"disabled": true, "rows": []interface{}{}})
			return
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"rows": rows})
}

// HandleLakeEvents returns the most-recent events matching optional filters.
// Query params: dt (YYYY-MM-DD), campaign_id (UUID), isp_group, event_type,
// limit (1..1000, default 100). When the reader is disabled it returns 200 with
// {"disabled":true,"events":[]}.
func (s *Server) HandleLakeEvents(w http.ResponseWriter, r *http.Request) {
	_, _ = GetOrgIDFromRequest(r)

	if !analytics.ReaderEnabled() {
		respondJSON(w, http.StatusOK, map[string]interface{}{"disabled": true, "events": []interface{}{}})
		return
	}

	q := r.URL.Query()
	limit := 100
	if ls := q.Get("limit"); ls != "" {
		if n, err := strconv.Atoi(ls); err == nil {
			limit = n
		}
	}
	f := analytics.EventFilter{
		Dt:         q.Get("dt"),
		CampaignID: q.Get("campaign_id"),
		ISPGroup:   q.Get("isp_group"),
		EventType:  q.Get("event_type"),
		Limit:      limit,
	}

	events, err := analytics.RecentEvents(r.Context(), f)
	if err != nil {
		if analytics.IsDisabledErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{"disabled": true, "events": []interface{}{}})
			return
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"events": events})
}
