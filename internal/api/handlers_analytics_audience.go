package api

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

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

	// No Glue snapshot writer exists yet, so the Athena partition is empty in
	// every real deployment. Whenever the reader is off or the table is absent,
	// serve the live Postgres fallback so the screen shows real data.
	if !analytics.ReaderEnabled() {
		s.respondAudienceStatusPG(r.Context(), w)
		return
	}
	dt, rows, err := analytics.AudienceLatestDt(r.Context())
	if err != nil {
		if audienceWantsPGFallback(err) {
			s.respondAudienceStatusPG(r.Context(), w)
			return
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Reader enabled but no partition has landed → fall back to Postgres rather
	// than reporting an empty snapshot the user can do nothing about.
	if dt == "" {
		s.respondAudienceStatusPG(r.Context(), w)
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

	q := r.URL.Query()

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

	filter := analytics.AudienceBreakdownFilter{
		GroupBy:      groupBy,
		Eq:           audienceEqFromQuery(q),
		AcquiredFrom: q.Get("acquired_from"),
		AcquiredTo:   q.Get("acquired_to"),
		ChurnedFrom:  q.Get("churned_from"),
		ChurnedTo:    q.Get("churned_to"),
		Limit:        limit,
	}

	// No Glue snapshot writer exists yet → serve the live Postgres breakdown
	// whenever the reader is off or the table/partition is absent.
	if !analytics.ReaderEnabled() {
		s.respondAudienceBreakdownPG(r.Context(), w, filter)
		return
	}

	dt, err := resolveAudienceDt(r, q.Get("dt"))
	if err != nil {
		if audienceWantsPGFallback(err) {
			s.respondAudienceBreakdownPG(r.Context(), w, filter)
			return
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if dt == "" {
		s.respondAudienceBreakdownPG(r.Context(), w, filter)
		return
	}

	filter.Dt = dt
	rows, err := analytics.AudienceBreakdown(r.Context(), filter)
	if err != nil {
		if audienceWantsPGFallback(err) {
			s.respondAudienceBreakdownPG(r.Context(), w, filter)
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

	limit := 0 // 0 -> analytics default (2000)
	if ls := q.Get("limit"); ls != "" {
		if n, err := strconv.Atoi(ls); err == nil {
			limit = n
		}
	}

	filter := analytics.SourcePerfFilter{
		From:  from,
		To:    to,
		Dim:   q.Get("dim"),
		Eq:    audienceEqFromQuery(q),
		Limit: limit,
	}

	// No Glue snapshot writer exists yet → serve PG whenever the reader is off
	// or the snapshot is absent.
	if !analytics.ReaderEnabled() {
		s.respondAudienceSourcePerfPG(r.Context(), w, filter)
		return
	}

	dt, err := resolveAudienceDt(r, q.Get("dt"))
	if err != nil {
		if audienceWantsPGFallback(err) {
			s.respondAudienceSourcePerfPG(r.Context(), w, filter)
			return
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if dt == "" {
		s.respondAudienceSourcePerfPG(r.Context(), w, filter)
		return
	}

	filter.AudienceDt = dt
	rows, err := analytics.AudienceSourcePerformance(r.Context(), filter)
	if err != nil {
		if audienceWantsPGFallback(err) {
			s.respondAudienceSourcePerfPG(r.Context(), w, filter)
			return
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"dim":         filter.Dim,
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

	// No Glue snapshot writer exists yet → serve the PG acquisition cohort.
	if !analytics.ReaderEnabled() {
		s.respondAudienceFirstTouchPG(r.Context(), w, from, to)
		return
	}

	rows, err := analytics.AudienceFirstTouch(r.Context(), from, to)
	if err != nil {
		if audienceWantsPGFallback(err) {
			s.respondAudienceFirstTouchPG(r.Context(), w, from, to)
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

	q := r.URL.Query()
	email := q.Get("email")
	// Subscriber-id lookup (operator 2026-07-27): resolve the id to its email
	// up front so every downstream path (lake + PG fallback) works unchanged.
	if email == "" {
		if sid := strings.TrimSpace(q.Get("subscriber_id")); sid != "" {
			if s.mailingDB == nil {
				respondJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "subscriber lookup unavailable (no mailing DB)"})
				return
			}
			if _, err := uuid.Parse(sid); err != nil {
				respondJSON(w, http.StatusBadRequest, map[string]string{"error": "subscriber_id must be a UUID"})
				return
			}
			var resolved string
			err := s.mailingDB.QueryRowContext(r.Context(),
				`SELECT lower(TRIM(BOTH FROM email)) FROM mailing_subscribers WHERE id = $1`, sid).Scan(&resolved)
			if err == sql.ErrNoRows {
				respondJSON(w, http.StatusNotFound, map[string]string{"error": "no subscriber with that id"})
				return
			}
			if err != nil {
				respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			email = resolved
		}
	}
	if email == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "email or subscriber_id is required"})
		return
	}

	// No Glue snapshot writer exists yet → serve member profiles from Postgres
	// (the PG fallback cannot reconstruct lake event history, so events stay
	// empty — the member tab discloses this).
	if !analytics.ReaderEnabled() {
		s.respondAudienceMemberPG(r.Context(), w, email)
		return
	}

	dt, err := resolveAudienceDt(r, q.Get("dt"))
	if err != nil {
		if audienceWantsPGFallback(err) {
			s.respondAudienceMemberPG(r.Context(), w, email)
			return
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if dt == "" {
		s.respondAudienceMemberPG(r.Context(), w, email)
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
		if audienceWantsPGFallback(err) {
			s.respondAudienceMemberPG(r.Context(), w, email)
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

// ═══════════════════════════════════════════════════════════════════════════
// POSTGRES FALLBACK
//
// The ignite_analytics.audience Glue snapshot is a *planned* daily 07:00 UTC
// full-replace export of mailing_subscribers, but NOTHING currently writes it:
// the audience-metrics Lambda only UPDATEs the single mailing_audience_metrics
// KPI row, and there is no CTAS / S3 export / Glue partition writer anywhere in
// the repo. Until that export lands, AudienceLatestDt() returns "" forever and
// every Athena-backed panel renders the "still seeding" empty screen.
//
// These helpers serve the SAME response shapes (analytics.AudienceRow,
// SourcePerfRow, FirstTouchRow, AudienceProfile) directly from the live
// mailing_subscribers table so the screen shows real numbers. Responses are
// tagged "source":"pg_fallback" so the UI can disclose the substitution. The
// reader-pure reader_audience.go is intentionally left untouched — this is the
// handler layer, where *sql.DB lives.
//
// COLUMN REALITY: mailing_subscribers physically has status, source,
// data_source, verification_status, engagement_score, total_opens/clicks/
// emails_received, subscribed_at/created_at/unsubscribed_at, is_bot. It does
// NOT have isp, source_system, hard_bounced_at, complained_at, churned_dt,
// engagement_band or churn_reason — those are DERIVED here (band from
// engagement_score; churn_reason/churned_dt from status + unsubscribed_at;
// email_domain from the address). Dimensions with no PG basis (isp,
// source_system, list_id) are unsupported and yield EMPTY rows rather than a
// fabricated value. The hard-rule is honored: status='bounced' surfaces as
// hard_bounce only; soft bounces are unknown in PG and are never invented or
// combined with hard.

// pgFallbackDt is the synthetic snapshot date reported for PG-fallback
// responses (today UTC) so the frontend's "has snapshot" gate opens.
func pgFallbackDt() string { return time.Now().UTC().Format("2006-01-02") }

// pgAudienceDimExpr maps an audience dimension to a safe SQL expression over
// mailing_subscribers, or (",", false) when the dimension has no PG basis. The
// returned strings are fixed literals (no caller input is interpolated), so
// they are injection-safe; caller VALUES always flow through bind parameters.
func pgAudienceDimExpr(dim string) (string, bool) {
	switch dim {
	case "status":
		return "COALESCE(status, '')", true
	case "data_source":
		return "COALESCE(data_source, '')", true
	case "source":
		return "COALESCE(source, '')", true
	case "verification_status":
		return "COALESCE(verification_status, '')", true
	case "email_domain":
		return "lower(split_part(email, '@', 2))", true
	case "engagement_band":
		// engagement_score is a 0–100 DECIMAL (default 50). Banded the same
		// hot/warm/cold way the lake snapshot bands it (best-effort: the exact
		// lake thresholds are not reproducible without the snapshot job).
		return "CASE WHEN engagement_score >= 66 THEN 'hot' WHEN engagement_score >= 33 THEN 'warm' ELSE 'cold' END", true
	case "acquired_dt":
		return "to_char(COALESCE(subscribed_at, created_at), 'YYYY-MM-DD')", true
	case "churned_dt":
		return "CASE WHEN status = 'unsubscribed' AND unsubscribed_at IS NOT NULL THEN to_char(unsubscribed_at, 'YYYY-MM-DD') ELSE '' END", true
	case "churn_reason":
		return "CASE status WHEN 'unsubscribed' THEN 'unsubscribed' WHEN 'bounced' THEN 'hard_bounced' WHEN 'blacklisted' THEN 'hard_bounced' WHEN 'complained' THEN 'complained' ELSE '' END", true
	default:
		// isp, source_system, list_id, organization_id, acquired_dt-on-events …
		return "", false
	}
}

// pgArgs accumulates positional bind parameters for a PG query, emitting
// $1,$2,… placeholders in call order.
type pgArgs struct{ vals []interface{} }

func (a *pgArgs) next(v interface{}) string {
	a.vals = append(a.vals, v)
	return "$" + strconv.Itoa(len(a.vals))
}

// pgAudienceEqWhere appends WHERE fragments for the supported equality filters,
// silently skipping dims with no PG basis (the frontend only ever sends
// status= as an eq filter, so this is lossless in practice).
func pgAudienceEqWhere(b *strings.Builder, args *pgArgs, eq map[string]string) {
	// Sorted for deterministic SQL (mirrors the Athena path).
	cols := make([]string, 0, len(eq))
	for c, v := range eq {
		if v == "" {
			continue
		}
		cols = append(cols, c)
	}
	for i := 0; i < len(cols); i++ { // tiny insertion sort, len<=12
		for j := i + 1; j < len(cols); j++ {
			if cols[j] < cols[i] {
				cols[i], cols[j] = cols[j], cols[i]
			}
		}
	}
	for _, c := range cols {
		expr, ok := pgAudienceDimExpr(c)
		if !ok {
			continue
		}
		b.WriteString(" AND ")
		b.WriteString(expr)
		b.WriteString(" = ")
		b.WriteString(args.next(eq[c]))
	}
}

// audienceSubscribersAvailable reports whether the PG fallback can run.
func (s *Server) audienceSubscribersAvailable() bool { return s.mailingDB != nil }

// pgSubscriberCount returns the total mailing_subscribers row count (the PG
// analogue of the snapshot row count). Errors collapse to 0.
func (s *Server) pgSubscriberCount(ctx context.Context) int64 {
	if s.mailingDB == nil {
		return 0
	}
	var n int64
	_ = s.mailingDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailing_subscribers`).Scan(&n)
	return n
}

// respondAudienceStatusPG answers /status from Postgres: a synthetic "today"
// snapshot whose row count is the live subscriber count.
func (s *Server) respondAudienceStatusPG(ctx context.Context, w http.ResponseWriter) {
	if !s.audienceSubscribersAvailable() {
		respondJSON(w, http.StatusOK, map[string]interface{}{"enabled_read": false, "latest_dt": "", "rows": 0})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"enabled_read": true,
		"latest_dt":    pgFallbackDt(),
		"rows":         s.pgSubscriberCount(ctx),
		"source":       "pg_fallback",
	})
}

// pgAudienceBreakdown computes a GROUP BY breakdown over mailing_subscribers
// returning the same []analytics.AudienceRow shape as the Athena path. Returns
// empty rows (no error) when any requested dimension has no PG basis, so the
// panel shows a clean empty state instead of a 400.
func (s *Server) pgAudienceBreakdown(ctx context.Context, f analytics.AudienceBreakdownFilter) ([]analytics.AudienceRow, error) {
	if s.mailingDB == nil {
		return []analytics.AudienceRow{}, nil
	}
	// Dedupe group_by preserving order, cap at 3 (matches the Athena contract).
	seen := map[string]bool{}
	dims := make([]string, 0, len(f.GroupBy))
	exprs := make([]string, 0, len(f.GroupBy))
	for _, d := range f.GroupBy {
		if seen[d] {
			continue
		}
		expr, ok := pgAudienceDimExpr(d)
		if !ok {
			return []analytics.AudienceRow{}, nil // unsupported dim → empty, not error
		}
		seen[d] = true
		dims = append(dims, d)
		exprs = append(exprs, expr)
	}
	if len(dims) == 0 || len(dims) > 3 {
		return []analytics.AudienceRow{}, nil
	}

	var b strings.Builder
	args := &pgArgs{}
	b.WriteString("SELECT ")
	for i, e := range exprs {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(e)
	}
	b.WriteString(", COUNT(*) c, ROUND(AVG(engagement_score)::numeric, 2) avg_eng FROM mailing_subscribers WHERE 1=1")
	pgAudienceEqWhere(&b, args, f.Eq)
	if f.AcquiredFrom != "" {
		b.WriteString(" AND COALESCE(subscribed_at, created_at) >= " + args.next(f.AcquiredFrom) + "::date")
	}
	if f.AcquiredTo != "" {
		b.WriteString(" AND COALESCE(subscribed_at, created_at) < (" + args.next(f.AcquiredTo) + "::date + INTERVAL '1 day')")
	}
	if f.ChurnedFrom != "" || f.ChurnedTo != "" {
		b.WriteString(" AND unsubscribed_at IS NOT NULL")
		if f.ChurnedFrom != "" {
			b.WriteString(" AND unsubscribed_at >= " + args.next(f.ChurnedFrom) + "::date")
		}
		if f.ChurnedTo != "" {
			b.WriteString(" AND unsubscribed_at < (" + args.next(f.ChurnedTo) + "::date + INTERVAL '1 day')")
		}
	}
	b.WriteString(" GROUP BY ")
	for i := range exprs {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Itoa(i + 1))
	}
	b.WriteString(" ORDER BY c DESC LIMIT ")
	b.WriteString(strconv.Itoa(analytics.ClampBreakdownLimit(f.Limit)))

	rows, err := s.mailingDB.QueryContext(ctx, b.String(), args.vals...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]analytics.AudienceRow, 0, 64)
	for rows.Next() {
		cells := make([]sql.NullString, len(dims)+2)
		dest := make([]interface{}, len(cells))
		for i := range cells {
			dest[i] = &cells[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		keys := make(map[string]string, len(dims))
		for i, d := range dims {
			keys[d] = cells[i].String
		}
		c, _ := strconv.ParseInt(cells[len(dims)].String, 10, 64)
		avg, _ := strconv.ParseFloat(cells[len(dims)+1].String, 64)
		out = append(out, analytics.AudienceRow{Keys: keys, Count: c, AvgEngagement: avg})
	}
	return out, rows.Err()
}

// respondAudienceBreakdownPG serves /breakdown from Postgres with the same
// payload shape as the Athena path, tagged source:"pg_fallback".
func (s *Server) respondAudienceBreakdownPG(ctx context.Context, w http.ResponseWriter, f analytics.AudienceBreakdownFilter) {
	rows, err := s.pgAudienceBreakdown(ctx, f)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"dt":        pgFallbackDt(),
		"group_by":  f.GroupBy,
		"rows":      rows,
		"truncated": len(rows) == analytics.ClampBreakdownLimit(f.Limit),
		"source":    "pg_fallback",
	})
}

// pgAudienceSourcePerformance builds the source-performance matrix from the
// per-subscriber engagement aggregates that the tracking handlers maintain
// (total_emails_received/opens/clicks) plus terminal status. It returns the
// same []analytics.SourcePerfRow {value,event_type,count} shape the Athena
// join returns, so the frontend pivot is unchanged. NOTE: this is a LIFETIME
// aggregate per dim value — the events from/to window does not apply in the PG
// fallback (the handler discloses this via source:"pg_fallback"). soft_bounce
// is unknown in PG and is never emitted or merged into hard.
func (s *Server) pgAudienceSourcePerformance(ctx context.Context, f analytics.SourcePerfFilter) ([]analytics.SourcePerfRow, error) {
	if s.mailingDB == nil {
		return []analytics.SourcePerfRow{}, nil
	}
	expr, ok := pgAudienceDimExpr(f.Dim)
	if !ok {
		return []analytics.SourcePerfRow{}, nil // isp / source_system → empty, honest
	}

	var b strings.Builder
	args := &pgArgs{}
	b.WriteString("SELECT ")
	b.WriteString(expr)
	b.WriteString(` v,
		COALESCE(SUM(total_emails_received), 0) delivered,
		COALESCE(SUM(total_opens), 0) opens,
		COALESCE(SUM(total_clicks), 0) clicks,
		COUNT(*) FILTER (WHERE status IN ('bounced','blacklisted')) hard,
		COUNT(*) FILTER (WHERE status = 'complained') complaints
		FROM mailing_subscribers WHERE 1=1`)
	pgAudienceEqWhere(&b, args, f.Eq)
	b.WriteString(" GROUP BY 1 ORDER BY delivered DESC LIMIT ")
	b.WriteString(strconv.Itoa(analytics.ClampSourcePerfLimit(f.Limit)))

	rows, err := s.mailingDB.QueryContext(ctx, b.String(), args.vals...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]analytics.SourcePerfRow, 0, 64)
	for rows.Next() {
		var v sql.NullString
		var delivered, opens, clicks, hard, complaints int64
		if err := rows.Scan(&v, &delivered, &opens, &clicks, &hard, &complaints); err != nil {
			return nil, err
		}
		val := v.String
		// Emit only non-zero buckets, mirroring the join's sparse output.
		add := func(et string, c int64) {
			if c > 0 {
				out = append(out, analytics.SourcePerfRow{Value: val, EventType: et, Count: c})
			}
		}
		add("delivered", delivered)
		add("open", opens)
		add("click", clicks)
		add("hard_bounce", hard)
		add("complaint", complaints)
	}
	return out, rows.Err()
}

// respondAudienceSourcePerfPG serves /source-performance from Postgres. The
// events from/to window is echoed back for UI continuity but does NOT filter
// the PG aggregate (which is lifetime-per-subscriber); source:"pg_fallback"
// signals the substitution.
func (s *Server) respondAudienceSourcePerfPG(ctx context.Context, w http.ResponseWriter, f analytics.SourcePerfFilter) {
	rows, err := s.pgAudienceSourcePerformance(ctx, f)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"dim":         f.Dim,
		"from":        f.From,
		"to":          f.To,
		"audience_dt": pgFallbackDt(),
		"rows":        rows,
		"truncated":   len(rows) == analytics.ClampSourcePerfLimit(f.Limit),
		"source":      "pg_fallback",
	})
}

// respondAudienceFirstTouchPG serves /first-touch from Postgres.
func (s *Server) respondAudienceFirstTouchPG(ctx context.Context, w http.ResponseWriter, from, to string) {
	rows, err := s.pgAudienceFirstTouch(ctx, from, to)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"from": from, "to": to, "rows": rows, "source": "pg_fallback",
	})
}

// respondAudienceMemberPG serves /member profiles from Postgres (events empty).
func (s *Server) respondAudienceMemberPG(ctx context.Context, w http.ResponseWriter, email string) {
	profiles, err := s.pgAudienceMemberProfiles(ctx, strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"email":       strings.ToLower(strings.TrimSpace(email)),
		"audience_dt": pgFallbackDt(),
		"profiles":    profiles,
		"events":      []interface{}{},
		"source":      "pg_fallback",
	})
}

// pgAudienceFirstTouch builds the Growth cohort from acquisition date
// (subscribed_at/created_at) over [from,to], with lifetime activation
// (total_opens/clicks > 0) and terminal churn (status) per cohort day. Same
// []analytics.FirstTouchRow shape as the lake first-touch. hard_bounced is
// status-derived; soft is never inferred.
func (s *Server) pgAudienceFirstTouch(ctx context.Context, from, to string) ([]analytics.FirstTouchRow, error) {
	if s.mailingDB == nil {
		return []analytics.FirstTouchRow{}, nil
	}
	args := &pgArgs{}
	fromP := args.next(from)
	toP := args.next(to)
	q := `SELECT to_char(COALESCE(subscribed_at, created_at), 'YYYY-MM-DD') dt,
		COUNT(*) c,
		COUNT(*) FILTER (WHERE total_opens > 0) opened,
		COUNT(*) FILTER (WHERE total_clicks > 0) clicked,
		COUNT(*) FILTER (WHERE status = 'unsubscribed') unsub,
		COUNT(*) FILTER (WHERE status IN ('bounced','blacklisted')) hardb,
		COUNT(*) FILTER (WHERE status = 'complained') compl
		FROM mailing_subscribers
		WHERE COALESCE(subscribed_at, created_at) >= ` + fromP + `::date
		  AND COALESCE(subscribed_at, created_at) < (` + toP + `::date + INTERVAL '1 day')
		GROUP BY 1 ORDER BY 1`

	rows, err := s.mailingDB.QueryContext(ctx, q, args.vals...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]analytics.FirstTouchRow, 0, 64)
	for rows.Next() {
		var dt sql.NullString
		var c, opened, clicked, unsub, hardb, compl int64
		if err := rows.Scan(&dt, &c, &opened, &clicked, &unsub, &hardb, &compl); err != nil {
			return nil, err
		}
		out = append(out, analytics.FirstTouchRow{
			Dt: dt.String, Count: c, Opened: opened, Clicked: clicked,
			Unsubscribed: unsub, HardBounced: hardb, Complained: compl,
		})
	}
	return out, rows.Err()
}

// pgAudienceMemberProfiles returns every mailing_subscribers row for an email
// as analytics.AudienceProfile, deriving the snapshot-only fields. The PG
// fallback cannot reconstruct lake event history, so the events list stays
// empty (the member tab discloses this).
func (s *Server) pgAudienceMemberProfiles(ctx context.Context, email string) ([]analytics.AudienceProfile, error) {
	if s.mailingDB == nil {
		return []analytics.AudienceProfile{}, nil
	}
	args := &pgArgs{}
	emP := args.next(email)
	q := `SELECT id::text, COALESCE(organization_id::text, ''), COALESCE(list_id::text, ''),
		email, lower(split_part(email, '@', 2)),
		COALESCE(status, ''), COALESCE(source, ''), COALESCE(data_source, ''),
		COALESCE(verification_status, ''),
		COALESCE(engagement_score, 0), COALESCE(total_opens, 0), COALESCE(total_clicks, 0),
		COALESCE(total_emails_received, 0),
		last_open_at, last_click_at, last_email_at,
		subscribed_at, created_at, unsubscribed_at,
		COALESCE(data_quality_score, 0), COALESCE(churn_risk_score, 0),
		COALESCE(is_bot, false),
		CASE WHEN engagement_score >= 66 THEN 'hot' WHEN engagement_score >= 33 THEN 'warm' ELSE 'cold' END,
		CASE status WHEN 'unsubscribed' THEN 'unsubscribed' WHEN 'bounced' THEN 'hard_bounced' WHEN 'blacklisted' THEN 'hard_bounced' WHEN 'complained' THEN 'complained' ELSE '' END
		FROM mailing_subscribers WHERE lower(email) = lower(` + emP + `) LIMIT 50`

	rows, err := s.mailingDB.QueryContext(ctx, q, args.vals...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ts := func(t sql.NullTime) string {
		if !t.Valid {
			return ""
		}
		return t.Time.UTC().Format(time.RFC3339)
	}
	dt := func(t sql.NullTime) string {
		if !t.Valid {
			return ""
		}
		return t.Time.UTC().Format("2006-01-02")
	}

	out := make([]analytics.AudienceProfile, 0, 4)
	for rows.Next() {
		var p analytics.AudienceProfile
		var engScore, dataQual, churnRisk float64
		var lastOpen, lastClick, lastEmail, subAt, createdAt, unsubAt sql.NullTime
		if err := rows.Scan(
			&p.ID, &p.OrganizationID, &p.ListID, &p.Email, &p.EmailDomain,
			&p.Status, &p.Source, &p.DataSource, &p.VerificationStatus,
			&engScore, &p.TotalOpens, &p.TotalClicks, &p.TotalEmailsReceived,
			&lastOpen, &lastClick, &lastEmail,
			&subAt, &createdAt, &unsubAt,
			&dataQual, &churnRisk, &p.IsBot,
			&p.EngagementBand, &p.ChurnReason,
		); err != nil {
			return nil, err
		}
		p.EngagementScore = engScore
		p.DataQualityScore = dataQual
		p.ChurnRiskScore = churnRisk
		p.LastOpenAt = ts(lastOpen)
		p.LastClickAt = ts(lastClick)
		p.LastEmailAt = ts(lastEmail)
		p.SubscribedAt = ts(subAt)
		p.CreatedAt = ts(createdAt)
		p.AcquiredAt = ts(subAt)
		p.AcquiredDt = dt(subAt)
		if !subAt.Valid {
			p.AcquiredDt = dt(createdAt)
		}
		p.UnsubscribedAt = ts(unsubAt)
		if p.Status == "unsubscribed" {
			p.ChurnedDt = dt(unsubAt)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// audienceWantsPGFallback reports whether an Athena error / state means the
// Glue snapshot is simply not available (disabled reader or absent table), in
// which case the handler should serve Postgres instead of an empty screen.
func audienceWantsPGFallback(err error) bool {
	return analytics.IsDisabledErr(err) || isAudienceAbsentErr(err)
}
