package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/analytics"
)

// AUDIENCE LAKE read API. These handlers expose the Athena-backed audience
// snapshot read layer (internal/analytics/reader_audience.go) over the
// ignite_analytics.audience Glue table — a daily full-replace, dt-partitioned
// snapshot of mailing_subscribers. They are READ ONLY (no Postgres, no send
// path) and degrade gracefully when the reader is disabled
// (ANALYTICS_ATHENA_OUTPUT unset): status always returns 200, the rest return
// a {"disabled":true} body with HTTP 200 rather than an error. Validation
// errors come back as 400 {"error":...}.
//
// Routes (wired in server_routes_mailing.go next to the event-lake routes,
// under /api/mailing/analytics/lake/audience):
//
//	GET /status             — read enablement + latest snapshot partition and row count
//	GET /breakdown          — GROUP BY counts + avg engagement over one snapshot
//	GET /source-performance — event counts per audience-dim value x event_type
//	GET /first-touch        — distinct recipients first seen in the lake per day
//	GET /member             — one address: all snapshot rows + 90-day event history
//
// Org resolution mirrors sibling /api/mailing analytics handlers via
// GetOrgIDFromRequest for consistency, even though the lake is not currently
// org-partitioned.

// audienceEqParams is the closed set of query params forwarded as equality
// filters; values are validated per-column inside the analytics package.
var audienceEqParams = []string{
	"status", "isp", "email_domain", "verification_status", "engagement_band",
	"churn_reason", "source", "data_source", "source_system", "acquired_dt",
	"churned_dt", "list_id",
}

// isAudienceAbsentErr reports whether an Athena error means the audience Glue
// table / partition simply isn't present in this deployment, or that a column
// has the wrong physical type (the "three engagement scales" footgun, where
// engagement_score lands as a STRING). Both are environment/data-shape
// conditions, not bad user input, so handlers degrade them to a friendly empty
// state (HTTP 200) instead of a hard 400 that reads as a broken screen.
// Genuinely unexpected errors fall through and still surface as 400.
func isAudienceAbsentErr(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	switch {
	case strings.Contains(m, "table_not_found"),
		strings.Contains(m, "schema_not_found"),
		strings.Contains(m, "entity_not_found"),
		strings.Contains(m, "does not exist"),
		strings.Contains(m, "not found"),
		strings.Contains(m, "no such table"),
		// Presto/Athena type errors from a string-typed numeric column.
		strings.Contains(m, "type_mismatch"),
		strings.Contains(m, "cannot be cast"),
		strings.Contains(m, "cannot cast"):
		return true
	}
	return false
}

// audienceEqFromQuery collects the non-empty equality-filter params.
func audienceEqFromQuery(q map[string][]string) map[string]string {
	eq := map[string]string{}
	for _, col := range audienceEqParams {
		if vs, ok := q[col]; ok && len(vs) > 0 && vs[0] != "" {
			eq[col] = vs[0]
		}
	}
	return eq
}

// HandleAudienceLakeStatus reports read enablement plus the newest available
// audience snapshot partition. Always 200; when the reader is disabled the
// body is simply {"enabled_read":false,"latest_dt":"","rows":0}.
func (s *Server) HandleAudienceLakeStatus(w http.ResponseWriter, r *http.Request) {
	_, _ = GetOrgIDFromRequest(r) // consistency with sibling handlers; lake not org-partitioned

	if !analytics.ReaderEnabled() {
		respondJSON(w, http.StatusOK, map[string]interface{}{"enabled_read": false, "latest_dt": "", "rows": 0})
		return
	}
	dt, rows, err := analytics.AudienceLatestDt(r.Context())
	if err != nil {
		if analytics.IsDisabledErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{"enabled_read": false, "latest_dt": "", "rows": 0})
			return
		}
		// Audience table not present yet → report "no snapshot" (enabled but
		// empty) rather than 400, so the screen renders its empty state.
		if isAudienceAbsentErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{"enabled_read": true, "latest_dt": "", "rows": 0})
			return
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"enabled_read": true, "latest_dt": dt, "rows": rows})
}

// resolveAudienceDt returns dt unchanged when set; otherwise it resolves the
// newest snapshot partition. ("", nil) means no partition exists — handlers
// answer that with a 200 {"empty":true} body, not an error.
func resolveAudienceDt(r *http.Request, dt string) (string, error) {
	if dt != "" {
		return dt, nil
	}
	latest, _, err := analytics.AudienceLatestDt(r.Context())
	if err != nil {
		return "", err
	}
	return latest, nil
}

// HandleAudienceLakeBreakdown runs a GROUP BY count + avg engagement over one
// audience snapshot. Query params: dt (YYYY-MM-DD, default = latest
// partition), group_by (comma-separated audience dims, 1..3, default
// "status"), limit (1..5000, default 1000), acquired_from/acquired_to and
// churned_from/churned_to (YYYY-MM-DD range predicates; a churned range
// implies churned_dt <> ''), plus the audienceEqParams equality filters.
// Disabled reader → 200 {"disabled":true,"rows":[]}; no snapshot partition →
// 200 {"dt":"","rows":[],"empty":true}; bad input → 400 {"error":...}.
func (s *Server) HandleAudienceLakeBreakdown(w http.ResponseWriter, r *http.Request) {
	_, _ = GetOrgIDFromRequest(r)

	if !analytics.ReaderEnabled() {
		respondJSON(w, http.StatusOK, map[string]interface{}{"disabled": true, "rows": []interface{}{}})
		return
	}

	q := r.URL.Query()
	dt, err := resolveAudienceDt(r, q.Get("dt"))
	if err != nil {
		if analytics.IsDisabledErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{"disabled": true, "rows": []interface{}{}})
			return
		}
		// Missing audience table/partition (or a string-typed column) → degrade
		// to a friendly empty state rather than a 400 that breaks the screen.
		if isAudienceAbsentErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{"dt": "", "rows": []interface{}{}, "empty": true})
			return
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if dt == "" {
		respondJSON(w, http.StatusOK, map[string]interface{}{"dt": "", "rows": []interface{}{}, "empty": true})
		return
	}

	groupBy := []string{"status"}
	if gb := q.Get("group_by"); gb != "" {
		groupBy = groupBy[:0]
		for _, d := range strings.Split(gb, ",") {
			if d = strings.TrimSpace(d); d != "" {
				groupBy = append(groupBy, d)
			}
		}
	}

	limit := 0 // 0 -> analytics default (1000)
	if ls := q.Get("limit"); ls != "" {
		if n, err := strconv.Atoi(ls); err == nil {
			limit = n
		}
	}

	rows, err := analytics.AudienceBreakdown(r.Context(), analytics.AudienceBreakdownFilter{
		Dt:           dt,
		GroupBy:      groupBy,
		Eq:           audienceEqFromQuery(q),
		AcquiredFrom: q.Get("acquired_from"),
		AcquiredTo:   q.Get("acquired_to"),
		ChurnedFrom:  q.Get("churned_from"),
		ChurnedTo:    q.Get("churned_to"),
		Limit:        limit,
	})
	if err != nil {
		if analytics.IsDisabledErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{"disabled": true, "rows": []interface{}{}})
			return
		}
		// Table-absent or column-type Athena errors degrade to an empty payload
		// (HTTP 200) so the Overview tab shows a clean empty state, not an error.
		if isAudienceAbsentErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{"dt": dt, "rows": []interface{}{}, "empty": true})
			return
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"dt":       dt,
		"group_by": groupBy,
		"rows":     rows,
		// truncated: the query hit its LIMIT, so more buckets likely exist.
		"truncated": len(rows) == analytics.ClampBreakdownLimit(limit),
	})
}

// HandleAudienceLakeSourcePerformance joins one audience snapshot against the
// email_events window and counts events per (dim value, event_type). Query
// params: dim (source|data_source|source_system|isp|verification_status|
// engagement_band|acquired_dt|status), from/to (YYYY-MM-DD events window,
// default last 7 days inclusive UTC), dt (snapshot, default = latest
// partition), limit (1..5000, default 2000), plus the audienceEqParams
// equality filters on the audience side. Disabled reader → 200
// {"disabled":true,"rows":[]}; no snapshot → 200 {"empty":true}.
func (s *Server) HandleAudienceLakeSourcePerformance(w http.ResponseWriter, r *http.Request) {
	_, _ = GetOrgIDFromRequest(r)

	if !analytics.ReaderEnabled() {
		respondJSON(w, http.StatusOK, map[string]interface{}{"disabled": true, "rows": []interface{}{}})
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

	dt, err := resolveAudienceDt(r, q.Get("dt"))
	if err != nil {
		if analytics.IsDisabledErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{"disabled": true, "rows": []interface{}{}})
			return
		}
		if isAudienceAbsentErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{"audience_dt": "", "rows": []interface{}{}, "empty": true})
			return
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if dt == "" {
		respondJSON(w, http.StatusOK, map[string]interface{}{"audience_dt": "", "rows": []interface{}{}, "empty": true})
		return
	}

	limit := 0 // 0 -> analytics default (2000)
	if ls := q.Get("limit"); ls != "" {
		if n, err := strconv.Atoi(ls); err == nil {
			limit = n
		}
	}

	dim := q.Get("dim")
	rows, err := analytics.AudienceSourcePerformance(r.Context(), analytics.SourcePerfFilter{
		AudienceDt: dt,
		From:       from,
		To:         to,
		Dim:        dim,
		Eq:         audienceEqFromQuery(q),
		Limit:      limit,
	})
	if err != nil {
		if analytics.IsDisabledErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{"disabled": true, "rows": []interface{}{}})
			return
		}
		if isAudienceAbsentErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{"audience_dt": dt, "rows": []interface{}{}, "empty": true})
			return
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"dim":         dim,
		"from":        from,
		"to":          to,
		"audience_dt": dt,
		"rows":        rows,
		// truncated: the query hit its LIMIT, so more buckets likely exist.
		"truncated": len(rows) == analytics.ClampSourcePerfLimit(limit),
	})
}

// HandleAudienceLakeFirstTouch returns, per day in [from,to], how many
// distinct recipients saw their first in-lake send event that day (lake
// history starts ~2026-03 — this is NOT lifetime-first). Query params:
// from/to (YYYY-MM-DD, default last 14 days inclusive UTC). Disabled reader →
// 200 {"disabled":true,"rows":[]}.
func (s *Server) HandleAudienceLakeFirstTouch(w http.ResponseWriter, r *http.Request) {
	_, _ = GetOrgIDFromRequest(r)

	if !analytics.ReaderEnabled() {
		respondJSON(w, http.StatusOK, map[string]interface{}{"disabled": true, "rows": []interface{}{}})
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
			from = now.AddDate(0, 0, -13).Format("2006-01-02")
		}
	}

	rows, err := analytics.AudienceFirstTouch(r.Context(), from, to)
	if err != nil {
		if analytics.IsDisabledErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{"disabled": true, "rows": []interface{}{}})
			return
		}
		if isAudienceAbsentErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{"from": from, "to": to, "rows": []interface{}{}, "empty": true})
			return
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"from": from, "to": to, "rows": rows})
}

// HandleAudienceLakeMember returns every snapshot row for one address plus
// its last-90-day event history (matched by email AND any subscriber ids
// found in the snapshot — see the join-key reality in reader_audience.go).
// Query params: email (required), dt (snapshot, default = latest partition),
// events_limit (1..500, default 200). Invalid email → 400. Disabled reader →
// 200 {"disabled":true,...}; no snapshot partition → 200 with empty arrays.
func (s *Server) HandleAudienceLakeMember(w http.ResponseWriter, r *http.Request) {
	_, _ = GetOrgIDFromRequest(r)

	if !analytics.ReaderEnabled() {
		respondJSON(w, http.StatusOK, map[string]interface{}{"disabled": true, "profiles": []interface{}{}, "events": []interface{}{}})
		return
	}

	q := r.URL.Query()
	email := q.Get("email")
	if email == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "email is required"})
		return
	}

	dt, err := resolveAudienceDt(r, q.Get("dt"))
	if err != nil {
		if analytics.IsDisabledErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{"disabled": true, "profiles": []interface{}{}, "events": []interface{}{}})
			return
		}
		if isAudienceAbsentErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{
				"email": strings.ToLower(strings.TrimSpace(email)), "audience_dt": "",
				"profiles": []interface{}{}, "events": []interface{}{}, "empty": true,
			})
			return
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if dt == "" {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"email": strings.ToLower(strings.TrimSpace(email)), "audience_dt": "",
			"profiles": []interface{}{}, "events": []interface{}{}, "empty": true,
		})
		return
	}

	eventsLimit := 0 // 0 -> analytics default (200)
	if ls := q.Get("events_limit"); ls != "" {
		if n, err := strconv.Atoi(ls); err == nil {
			eventsLimit = n
		}
	}

	profiles, events, err := analytics.AudienceMember(r.Context(), dt, email, eventsLimit)
	if err != nil {
		if analytics.IsDisabledErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{"disabled": true, "profiles": []interface{}{}, "events": []interface{}{}})
			return
		}
		if isAudienceAbsentErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{
				"email": strings.ToLower(strings.TrimSpace(email)), "audience_dt": dt,
				"profiles": []interface{}{}, "events": []interface{}{}, "empty": true,
			})
			return
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"email":       strings.ToLower(strings.TrimSpace(email)),
		"audience_dt": dt,
		"profiles":    profiles,
		"events":      events,
	})
}
