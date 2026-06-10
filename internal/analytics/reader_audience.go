// AUDIENCE LAKE read layer. The Glue table ignite_analytics.audience is a
// daily full-replace snapshot of mailing_subscribers (gzip-JSON, partition
// projection on dt = snapshot date YYYY-MM-DD). This file adds Athena-backed
// read queries over that table, following the exact contract of reader.go:
//
//   - DISABLED BY DEFAULT — every package-level wrapper returns errDisabled
//     when InitReader was never given an output location.
//   - READ ONLY — never writes to the lake, never touches Postgres.
//   - SQL-injection safe — every caller-supplied value is regex-validated
//     before it is rendered as a quoted literal via sqlStr. We NEVER use
//     Athena ExecutionParameters (see the sqlStr comment in reader.go for the
//     date-shaped TYPE_MISMATCH reason).
//
// JOIN-KEY REALITY when joining ignite_analytics.email_events: PMTA-sourced
// events carry subscriber_id = '' (email only, lowercase); SES events carry
// subscriber_id; app-tracking events carry subscriber_id but email = ''. The
// canonical event-side key is therefore
// CASE WHEN e.subscriber_id <> '' THEN e.subscriber_id ELSE lower(e.email) END,
// and the audience side must offer BOTH keys (id and email), deduped by key so
// the same email appearing in multiple lists cannot fan events out.
package analytics

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const audienceTable = "audience"

// Validation patterns specific to the audience table.
var (
	// freeTokenRe admits the raw import provenance values (source like
	// "warmup-data/2026/3M_Free_090123.csv", data_source like
	// "attribits:finance:attribits_finance_001") — spaces, dots, slashes and
	// colons are legal. The charset still excludes quotes, semicolons and every
	// other SQL special, so sqlStr interpolation stays injection-safe.
	freeTokenRe = regexp.MustCompile(`^[a-zA-Z0-9 ._/:\-]{1,300}$`)
	// emailRe validates member-lookup addresses. No quotes/specials can pass,
	// so a validated email is safe for sqlStr. Length is capped separately at
	// 320 (RFC max) in normalizeEmail.
	emailRe = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+$`)
)

// audienceDims is the closed set of audience columns that may be grouped or
// equality-filtered on, each paired with the validation pattern its values
// must satisfy. Anything outside this map is rejected by name before SQL
// construction — caller text never reaches a column position.
var audienceDims = map[string]*regexp.Regexp{
	"status":              tokenRe,
	"isp":                 tokenRe,
	"email_domain":        dottedRe, // recipient domains contain dots
	"verification_status": tokenRe,
	"engagement_band":     tokenRe, // hot | warm | cold
	"churn_reason":        tokenRe, // '' | unsubscribed | hard_bounced | complained
	"source":              freeTokenRe,
	"data_source":         freeTokenRe,
	"source_system":       freeTokenRe,
	"acquired_dt":         dtRe,
	"churned_dt":          dtRe,
	"list_id":             uuidRe,
	"organization_id":     uuidRe,
}

// audienceColumns is the full 33-column SELECT list for member lookup. Order
// MUST match scanAudienceProfile.
const audienceColumns = "id, organization_id, list_id, email, email_domain, isp, status, " +
	"source, data_source, source_system, source_detail, acquired_at, acquired_dt, " +
	"imported_at, created_at, subscribed_at, unsubscribed_at, hard_bounced_at, " +
	"complained_at, churned_dt, churn_reason, engagement_score, engagement_band, " +
	"total_opens, total_clicks, total_emails_received, last_open_at, last_click_at, " +
	"last_email_at, data_quality_score, verification_status, is_bot, churn_risk_score"

// AudienceProfile is one row of the audience snapshot (one subscriber-list
// row). JSON tags match the Glue column names exactly. Timestamps are
// ISO-8601 strings; '' means never. churned_dt/acquired_dt are YYYY-MM-DD.
type AudienceProfile struct {
	ID                  string  `json:"id"`
	OrganizationID      string  `json:"organization_id"`
	ListID              string  `json:"list_id"`
	Email               string  `json:"email"`
	EmailDomain         string  `json:"email_domain"`
	ISP                 string  `json:"isp"`
	Status              string  `json:"status"`
	Source              string  `json:"source"`
	DataSource          string  `json:"data_source"`
	SourceSystem        string  `json:"source_system"`
	SourceDetail        string  `json:"source_detail"`
	AcquiredAt          string  `json:"acquired_at"`
	AcquiredDt          string  `json:"acquired_dt"`
	ImportedAt          string  `json:"imported_at"`
	CreatedAt           string  `json:"created_at"`
	SubscribedAt        string  `json:"subscribed_at"`
	UnsubscribedAt      string  `json:"unsubscribed_at"`
	HardBouncedAt       string  `json:"hard_bounced_at"`
	ComplainedAt        string  `json:"complained_at"`
	ChurnedDt           string  `json:"churned_dt"`
	ChurnReason         string  `json:"churn_reason"`
	EngagementScore     float64 `json:"engagement_score"`
	EngagementBand      string  `json:"engagement_band"`
	TotalOpens          int64   `json:"total_opens"`
	TotalClicks         int64   `json:"total_clicks"`
	TotalEmailsReceived int64   `json:"total_emails_received"`
	LastOpenAt          string  `json:"last_open_at"`
	LastClickAt         string  `json:"last_click_at"`
	LastEmailAt         string  `json:"last_email_at"`
	DataQualityScore    float64 `json:"data_quality_score"`
	VerificationStatus  string  `json:"verification_status"`
	IsBot               bool    `json:"is_bot"`
	ChurnRiskScore      float64 `json:"churn_risk_score"`
}

// AudienceBreakdownFilter is the (validated) input for AudienceBreakdown.
type AudienceBreakdownFilter struct {
	Dt           string            // snapshot partition, required (handler resolves default)
	GroupBy      []string          // 1..3 dims from audienceDims
	Eq           map[string]string // optional equality predicates, audienceDims keys
	AcquiredFrom string            // optional YYYY-MM-DD, predicate on acquired_dt
	AcquiredTo   string            // optional YYYY-MM-DD, predicate on acquired_dt
	ChurnedFrom  string            // optional YYYY-MM-DD, predicate on churned_dt (implies churned_dt <> '')
	ChurnedTo    string            // optional YYYY-MM-DD, predicate on churned_dt (implies churned_dt <> '')
	Limit        int               // clamped to [1,5000], default 1000 if <=0
}

// AudienceRow is one GROUP BY bucket from AudienceBreakdown.
type AudienceRow struct {
	Keys          map[string]string `json:"keys"`
	Count         int64             `json:"count"`
	AvgEngagement float64           `json:"avg_engagement"`
}

// SourcePerfFilter is the (validated) input for AudienceSourcePerformance.
type SourcePerfFilter struct {
	AudienceDt string            // snapshot partition, required (handler resolves default)
	From       string            // email_events dt window start, YYYY-MM-DD
	To         string            // email_events dt window end, YYYY-MM-DD
	Dim        string            // one of sourcePerfDims
	Eq         map[string]string // optional audience-side equality filters (audienceDims)
	Limit      int               // clamped to [1,5000], default 2000 if <=0
}

// SourcePerfRow is one (dim value, event_type) bucket from
// AudienceSourcePerformance.
type SourcePerfRow struct {
	Value     string `json:"value"`
	EventType string `json:"event_type"`
	Count     int64  `json:"count"`
}

// FirstTouchRow is one dt bucket from AudienceFirstTouch: how many distinct
// recipients saw their first lake event on that day, and how many of that
// cohort have since ACTIVATED — i.e. produced at least one open (and click)
// event anywhere in lake history. Opened/Count is the cohort activation rate
// the operator uses to judge acquisition quality thresholds.
type FirstTouchRow struct {
	Dt      string `json:"dt"`
	Count   int64  `json:"count"`
	Opened  int64  `json:"opened"`
	Clicked int64  `json:"clicked"`
}

// sourcePerfDims is the closed set of audience dimensions
// AudienceSourcePerformance may slice by.
var sourcePerfDims = map[string]bool{
	"source":              true,
	"data_source":         true,
	"source_system":       true,
	"isp":                 true,
	"verification_status": true,
	"engagement_band":     true,
	"acquired_dt":         true,
	"status":              true,
}

// clampSourcePerfLimit clamps a source-performance LIMIT to [1,5000],
// defaulting to 2000 when the caller passes <=0.
func clampSourcePerfLimit(n int) int {
	if n <= 0 {
		return 2000
	}
	if n > 5000 {
		return 5000
	}
	return n
}

// ClampSourcePerfLimit is the exported form of clampSourcePerfLimit so
// handlers can mirror the effective LIMIT for their "truncated" flag.
func ClampSourcePerfLimit(n int) int { return clampSourcePerfLimit(n) }

// clampMemberEventsLimit clamps a member-events LIMIT to [1,500], defaulting
// to 200 when the caller passes <=0.
func clampMemberEventsLimit(n int) int {
	if n <= 0 {
		return 200
	}
	if n > 500 {
		return 500
	}
	return n
}

// normalizeEmail lowercases and validates a member-lookup address. The
// returned value is safe for sqlStr interpolation (emailRe admits no quotes
// or SQL specials).
func normalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return "", fmt.Errorf("email is required")
	}
	if len(email) > 320 {
		return "", fmt.Errorf("invalid email: too long")
	}
	if !emailRe.MatchString(email) {
		return "", fmt.Errorf("invalid email")
	}
	return email, nil
}

// validateAudienceEq checks every Eq key against the audienceDims whitelist
// and every non-empty value against its column's pattern, returning the
// predicate columns in sorted order (map iteration is random; tests assert
// exact SQL). Empty values are skipped (not predicates), matching the
// BreakdownFilter convention.
func validateAudienceEq(eq map[string]string) ([]string, error) {
	cols := make([]string, 0, len(eq))
	for col, val := range eq {
		re, ok := audienceDims[col]
		if !ok {
			return nil, fmt.Errorf("invalid filter column %q", col)
		}
		if val == "" {
			continue
		}
		if !re.MatchString(val) {
			return nil, fmt.Errorf("invalid value for %s", col)
		}
		cols = append(cols, col)
	}
	sort.Strings(cols)
	return cols, nil
}

// buildAudienceLatestDtSQL renders the partition-discovery query. minDt is
// computed in Go (today-4d UTC) and validated here for defense in depth.
//
//	SELECT dt, COUNT(*) c FROM audience WHERE dt >= '<minDt>'
//	GROUP BY dt ORDER BY dt DESC
func buildAudienceLatestDtSQL(minDt string) (string, error) {
	if minDt == "" || !dtRe.MatchString(minDt) {
		return "", fmt.Errorf("invalid min dt: must be YYYY-MM-DD")
	}
	return "SELECT dt, COUNT(*) c FROM " + audienceTable +
		" WHERE dt >= " + sqlStr(minDt) +
		" GROUP BY dt ORDER BY dt DESC", nil
}

// AudienceLatestDt returns the newest available snapshot partition (looking
// back 4 days from today UTC) and its row count. Returns ("", 0, nil) when no
// partition exists in that window.
func (r *Reader) AudienceLatestDt(ctx context.Context) (string, int64, error) {
	minDt := time.Now().UTC().AddDate(0, 0, -4).Format("2006-01-02")
	sql, err := buildAudienceLatestDtSQL(minDt)
	if err != nil {
		return "", 0, err
	}
	_, rows, err := r.runQuery(ctx, sql)
	if err != nil {
		return "", 0, err
	}
	if len(rows) == 0 || len(rows[0]) < 2 {
		return "", 0, nil
	}
	// The daily export clears its dt prefix and then streams parts for several
	// minutes, so the newest partition can be visibly PARTIAL mid-write. Skip a
	// newer partition whose row count is under half the window's max — readers
	// then keep using yesterday's complete snapshot until today's finishes
	// landing, rather than silently reporting an undersized audience.
	var maxC int64
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		if c, _ := strconv.ParseInt(row[1], 10, 64); c > maxC {
			maxC = c
		}
	}
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		c, _ := strconv.ParseInt(row[1], 10, 64)
		if c*2 >= maxC {
			return row[0], c, nil
		}
	}
	c, _ := strconv.ParseInt(rows[0][1], 10, 64)
	return rows[0][0], c, nil
}

// validateAudienceBreakdownFilter checks every caller-supplied piece of an
// AudienceBreakdownFilter and returns the deduplicated GroupBy list (original
// order preserved). Nothing reaches SQL construction until this passes.
func validateAudienceBreakdownFilter(f AudienceBreakdownFilter) ([]string, error) {
	if f.Dt == "" {
		return nil, fmt.Errorf("dt is required")
	}
	if err := validateDt("dt", f.Dt); err != nil {
		return nil, err
	}

	// GroupBy: whitelist every dim, dedupe preserving order, require 1..3.
	seen := make(map[string]bool, len(f.GroupBy))
	dims := make([]string, 0, len(f.GroupBy))
	for _, d := range f.GroupBy {
		if _, ok := audienceDims[d]; !ok {
			return nil, fmt.Errorf("invalid group_by dimension %q", d)
		}
		if seen[d] {
			continue
		}
		seen[d] = true
		dims = append(dims, d)
	}
	if len(dims) == 0 {
		return nil, fmt.Errorf("group_by requires at least one dimension")
	}
	if len(dims) > 3 {
		return nil, fmt.Errorf("group_by allows at most 3 dimensions, got %d", len(dims))
	}

	if _, err := validateAudienceEq(f.Eq); err != nil {
		return nil, err
	}

	// Optional date ranges on acquired_dt / churned_dt. Lexical order ==
	// chronological order for YYYY-MM-DD, so an inverted range is rejected
	// explicitly rather than silently matching zero rows.
	for _, p := range []struct{ name, from, to string }{
		{"acquired", f.AcquiredFrom, f.AcquiredTo},
		{"churned", f.ChurnedFrom, f.ChurnedTo},
	} {
		if err := validateDt(p.name+"_from", p.from); err != nil {
			return nil, err
		}
		if err := validateDt(p.name+"_to", p.to); err != nil {
			return nil, err
		}
		if p.from != "" && p.to != "" && p.from > p.to {
			return nil, fmt.Errorf("invalid %s range: from %s is after to %s", p.name, p.from, p.to)
		}
	}
	return dims, nil
}

// buildAudienceBreakdownSQL validates f and renders the full Athena query.
// Factored out so the SQL shape is unit-testable without an Athena client.
//
// SQL shape (validated literals only, via sqlStr):
//
//	SELECT <dims...>, COUNT(*) c, ROUND(AVG(engagement_score), 2) FROM audience
//	WHERE dt = '<dt>' [AND <col> = '<val>'...]
//	[AND acquired_dt >= '..'] [AND acquired_dt <= '..']
//	[AND churned_dt <> '' AND churned_dt >= '..' AND churned_dt <= '..']
//	GROUP BY <dims> ORDER BY c DESC LIMIT <n>
//
// COUNT(*) — NOT COUNT(DISTINCT ...) — is deliberate: the snapshot is a daily
// full replace with exactly one row per subscriber-list row by construction,
// so there are no duplicates to collapse (unlike email_events, where bridge
// redelivery forces COUNT(DISTINCT event_uid)). A churned range implies
// churned_dt <> '' so never-churned rows ('' sentinel) can't lexically slip
// under a lower bound. Eq predicates render in sorted column order so the
// output is deterministic. LIMIT renders from the clamped int via strconv
// (Athena cannot bind a parameter in the LIMIT position).
func buildAudienceBreakdownSQL(f AudienceBreakdownFilter) (string, error) {
	dims, err := validateAudienceBreakdownFilter(f)
	if err != nil {
		return "", err
	}
	eqCols, err := validateAudienceEq(f.Eq)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("SELECT ")
	b.WriteString(strings.Join(dims, ", "))
	b.WriteString(", COUNT(*) c, ROUND(AVG(engagement_score), 2) FROM ")
	b.WriteString(audienceTable)
	b.WriteString(" WHERE dt = ")
	b.WriteString(sqlStr(f.Dt))
	for _, col := range eqCols {
		b.WriteString(" AND ")
		b.WriteString(col)
		b.WriteString(" = ")
		b.WriteString(sqlStr(f.Eq[col]))
	}
	if f.AcquiredFrom != "" {
		b.WriteString(" AND acquired_dt >= ")
		b.WriteString(sqlStr(f.AcquiredFrom))
	}
	if f.AcquiredTo != "" {
		b.WriteString(" AND acquired_dt <= ")
		b.WriteString(sqlStr(f.AcquiredTo))
	}
	if f.ChurnedFrom != "" || f.ChurnedTo != "" {
		b.WriteString(" AND churned_dt <> ''")
		if f.ChurnedFrom != "" {
			b.WriteString(" AND churned_dt >= ")
			b.WriteString(sqlStr(f.ChurnedFrom))
		}
		if f.ChurnedTo != "" {
			b.WriteString(" AND churned_dt <= ")
			b.WriteString(sqlStr(f.ChurnedTo))
		}
	}
	b.WriteString(" GROUP BY ")
	b.WriteString(strings.Join(dims, ", "))
	b.WriteString(" ORDER BY c DESC LIMIT ")
	b.WriteString(strconv.Itoa(clampBreakdownLimit(f.Limit)))
	return b.String(), nil
}

// AudienceBreakdown runs a GROUP BY aggregation over one audience snapshot
// partition and returns one row per dimension-value combination, highest
// count first, with the bucket's average engagement_score.
func (r *Reader) AudienceBreakdown(ctx context.Context, f AudienceBreakdownFilter) ([]AudienceRow, error) {
	dims, err := validateAudienceBreakdownFilter(f)
	if err != nil {
		return nil, err
	}
	sql, err := buildAudienceBreakdownSQL(f)
	if err != nil {
		return nil, err
	}
	_, rows, err := r.runQuery(ctx, sql)
	if err != nil {
		return nil, err
	}
	out := make([]AudienceRow, 0, len(rows))
	for _, row := range rows {
		if len(row) < len(dims)+2 {
			continue
		}
		keys := make(map[string]string, len(dims))
		for i, d := range dims {
			keys[d] = row[i]
		}
		c, _ := strconv.ParseInt(row[len(dims)], 10, 64)
		avg, _ := strconv.ParseFloat(row[len(dims)+1], 64)
		out = append(out, AudienceRow{Keys: keys, Count: c, AvgEngagement: avg})
	}
	return out, nil
}

// validateSourcePerfFilter checks every caller-supplied piece of a
// SourcePerfFilter before SQL construction.
func validateSourcePerfFilter(f SourcePerfFilter) error {
	if f.AudienceDt == "" {
		return fmt.Errorf("audience dt is required")
	}
	if err := validateDt("dt", f.AudienceDt); err != nil {
		return err
	}
	if f.From == "" || f.To == "" {
		return fmt.Errorf("from and to dates are required")
	}
	if err := validateDt("from", f.From); err != nil {
		return err
	}
	if err := validateDt("to", f.To); err != nil {
		return err
	}
	if f.From > f.To {
		return fmt.Errorf("invalid range: from %s is after to %s", f.From, f.To)
	}
	if !sourcePerfDims[f.Dim] {
		return fmt.Errorf("invalid dim %q", f.Dim)
	}
	if _, err := validateAudienceEq(f.Eq); err != nil {
		return err
	}
	return nil
}

// buildAudienceSourcePerfSQL validates f and renders the audience x
// email_events join. Factored out for unit tests.
//
// SQL shape (validated literals only, via sqlStr):
//
//	WITH aud AS (
//	  SELECT id, email, <dim> AS dimv FROM audience
//	  WHERE dt = '<audienceDt>' [AND <eq preds>]
//	), keyed AS (
//	  SELECT join_key, max(dimv) AS dimv FROM (
//	    SELECT id AS join_key, dimv FROM aud
//	    UNION ALL
//	    SELECT email AS join_key, dimv FROM aud
//	  ) GROUP BY join_key
//	)
//	SELECT k.dimv, e.event_type, COUNT(DISTINCT e.event_uid) c
//	FROM email_events e
//	JOIN keyed k ON (CASE WHEN e.subscriber_id <> '' THEN e.subscriber_id
//	                      ELSE lower(e.email) END) = k.join_key
//	WHERE e.dt BETWEEN '<from>' AND '<to>'
//	GROUP BY 1, 2 ORDER BY c DESC LIMIT <n>
//
// The audience side offers BOTH join keys because of the lake's split
// identity: PMTA events have subscriber_id = '' (email only), SES events have
// subscriber_id, app-tracking events have subscriber_id but email = ''.
//
// The keyed GROUP BY join_key is MANDATORY: the same email exists in multiple
// lists (one snapshot row per subscriber-list row), so without deduping by
// join key a single event would join to N audience rows and inflate counts by
// the list-membership factor. max(dimv) picks one dimension value per key
// deterministically. COUNT(DISTINCT e.event_uid) additionally collapses PMTA
// bridge redeliveries, same as Breakdown in reader.go.
func buildAudienceSourcePerfSQL(f SourcePerfFilter) (string, error) {
	if err := validateSourcePerfFilter(f); err != nil {
		return "", err
	}
	eqCols, err := validateAudienceEq(f.Eq)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("WITH aud AS (SELECT id, email, ")
	b.WriteString(f.Dim)
	b.WriteString(" AS dimv FROM ")
	b.WriteString(audienceTable)
	b.WriteString(" WHERE dt = ")
	b.WriteString(sqlStr(f.AudienceDt))
	for _, col := range eqCols {
		b.WriteString(" AND ")
		b.WriteString(col)
		b.WriteString(" = ")
		b.WriteString(sqlStr(f.Eq[col]))
	}
	b.WriteString("), keyed AS (SELECT join_key, max(dimv) AS dimv FROM (")
	b.WriteString("SELECT id AS join_key, dimv FROM aud")
	b.WriteString(" UNION ALL ")
	b.WriteString("SELECT email AS join_key, dimv FROM aud")
	b.WriteString(") GROUP BY join_key)")
	b.WriteString(" SELECT k.dimv, e.event_type, COUNT(DISTINCT e.event_uid) c FROM ")
	b.WriteString(lakeTable)
	b.WriteString(" e JOIN keyed k ON (CASE WHEN e.subscriber_id <> '' THEN e.subscriber_id ELSE lower(e.email) END) = k.join_key")
	b.WriteString(" WHERE e.dt BETWEEN ")
	b.WriteString(sqlStr(f.From))
	b.WriteString(" AND ")
	b.WriteString(sqlStr(f.To))
	b.WriteString(" GROUP BY 1, 2 ORDER BY c DESC LIMIT ")
	b.WriteString(strconv.Itoa(clampSourcePerfLimit(f.Limit)))
	return b.String(), nil
}

// AudienceSourcePerformance joins one audience snapshot against the
// email_events window [From,To] and returns event counts per (dim value,
// event_type), highest count first.
func (r *Reader) AudienceSourcePerformance(ctx context.Context, f SourcePerfFilter) ([]SourcePerfRow, error) {
	sql, err := buildAudienceSourcePerfSQL(f)
	if err != nil {
		return nil, err
	}
	_, rows, err := r.runQuery(ctx, sql)
	if err != nil {
		return nil, err
	}
	out := make([]SourcePerfRow, 0, len(rows))
	for _, row := range rows {
		if len(row) < 3 {
			continue
		}
		c, _ := strconv.ParseInt(row[2], 10, 64)
		out = append(out, SourcePerfRow{Value: row[0], EventType: row[1], Count: c})
	}
	return out, nil
}

// buildAudienceFirstTouchSQL validates the range and renders the first-touch
// query. Factored out for unit tests.
//
// SQL shape (validated literals only, via sqlStr). One pass over the events
// table computes, per recipient key, BOTH the first send-ish day and whether
// the recipient ever opened/clicked; the outer query buckets by cohort day:
//
//	SELECT first_dt, COUNT(*) c, SUM(opened) o, SUM(clicked) k FROM (
//	  SELECT (CASE WHEN subscriber_id <> '' THEN subscriber_id
//	               ELSE lower(email) END) rk,
//	         MIN(CASE WHEN event_type IN ('attempted','delivered','relayed_to_ses')
//	                  THEN dt END) AS first_dt,
//	         MAX(CASE WHEN event_type = 'open' THEN 1 ELSE 0 END) AS opened,
//	         MAX(CASE WHEN event_type = 'click' THEN 1 ELSE 0 END) AS clicked
//	  FROM email_events
//	  GROUP BY 1
//	) WHERE first_dt IS NOT NULL AND first_dt BETWEEN '<from>' AND '<to>'
//	GROUP BY 1 ORDER BY 1
//
// SEMANTICS: this is "first touch within lake history" — the lake only goes
// back to the ~2026-03 backfill, so a recipient first mailed before that
// reads as first-touched on their earliest in-lake event, NOT their lifetime
// first send. opened/clicked are LIFETIME-in-lake activation flags for the
// cohort (any open/click ever, not just after first_dt — opens can only
// follow a send, so the distinction is moot in practice). Open/click events
// reach the lake only for SES-routed mail + the app-tracking backfill, so
// activation is directional for PMTA-heavy cohorts. The inner GROUP BY uses
// the canonical per-recipient key (see the file header for the
// split-identity reality).
func buildAudienceFirstTouchSQL(from, to string) (string, error) {
	if from == "" || to == "" {
		return "", fmt.Errorf("from and to dates are required")
	}
	if err := validateDt("from", from); err != nil {
		return "", err
	}
	if err := validateDt("to", to); err != nil {
		return "", err
	}
	if from > to {
		return "", fmt.Errorf("invalid range: from %s is after to %s", from, to)
	}
	return "SELECT first_dt, COUNT(*) c, SUM(opened) o, SUM(clicked) k FROM (" +
		"SELECT (CASE WHEN subscriber_id <> '' THEN subscriber_id ELSE lower(email) END) rk, " +
		"MIN(CASE WHEN event_type IN ('attempted','delivered','relayed_to_ses') THEN dt END) AS first_dt, " +
		"MAX(CASE WHEN event_type = 'open' THEN 1 ELSE 0 END) AS opened, " +
		"MAX(CASE WHEN event_type = 'click' THEN 1 ELSE 0 END) AS clicked FROM " +
		lakeTable +
		" GROUP BY 1" +
		") WHERE first_dt IS NOT NULL AND first_dt BETWEEN " + sqlStr(from) + " AND " + sqlStr(to) +
		" GROUP BY 1 ORDER BY 1", nil
}

// AudienceFirstTouch returns, per day in [from,to], how many distinct
// recipients saw their first-ever in-lake send event on that day, plus how
// many of each day's cohort have an open/click anywhere in lake history
// (cohort activation). See the builder comment for the "lake history, not
// lifetime" caveat.
func (r *Reader) AudienceFirstTouch(ctx context.Context, from, to string) ([]FirstTouchRow, error) {
	sql, err := buildAudienceFirstTouchSQL(from, to)
	if err != nil {
		return nil, err
	}
	_, rows, err := r.runQuery(ctx, sql)
	if err != nil {
		return nil, err
	}
	out := make([]FirstTouchRow, 0, len(rows))
	for _, row := range rows {
		if len(row) < 4 {
			continue
		}
		c, _ := strconv.ParseInt(row[1], 10, 64)
		o, _ := strconv.ParseInt(row[2], 10, 64)
		k, _ := strconv.ParseInt(row[3], 10, 64)
		out = append(out, FirstTouchRow{Dt: row[0], Count: c, Opened: o, Clicked: k})
	}
	return out, nil
}

// buildAudienceMemberProfileSQL renders the snapshot lookup for one address.
// LIMIT 20 caps pathological multi-list memberships.
//
//	SELECT <33 audience columns> FROM audience
//	WHERE dt = '<dt>' AND email = '<email>' LIMIT 20
func buildAudienceMemberProfileSQL(dt, email string) (string, error) {
	if dt == "" {
		return "", fmt.Errorf("dt is required")
	}
	if err := validateDt("dt", dt); err != nil {
		return "", err
	}
	em, err := normalizeEmail(email)
	if err != nil {
		return "", err
	}
	return "SELECT " + audienceColumns + " FROM " + audienceTable +
		" WHERE dt = " + sqlStr(dt) + " AND email = " + sqlStr(em) + " LIMIT 20", nil
}

// buildAudienceMemberEventsSQL renders the 90-day event history for one
// member. The SELECT list (and column order) is identical to RecentEvents so
// scanEvent can be reused. ids come from the member's own snapshot rows; each
// is re-validated against uuidRe before interpolation (defense in depth —
// they are lake data, not caller input; anything malformed is silently
// dropped rather than failing the whole lookup).
//
//	SELECT <24 event columns> FROM email_events
//	WHERE (lower(email) = '<email>' [OR subscriber_id IN ('<id>',...)])
//	AND dt >= '<minDt>' ORDER BY event_epoch_ms DESC LIMIT <n>
func buildAudienceMemberEventsSQL(email string, ids []string, minDt string, limit int) (string, error) {
	em, err := normalizeEmail(email)
	if err != nil {
		return "", err
	}
	if minDt == "" || !dtRe.MatchString(minDt) {
		return "", fmt.Errorf("invalid min dt: must be YYYY-MM-DD")
	}

	var b strings.Builder
	b.WriteString("SELECT event_uid, recipient_send_id, campaign_id, subscriber_id, email, ")
	b.WriteString("email_domain, brand, isp_group, route_type, event_type, suppression_reason, ")
	b.WriteString("vmta, pool, bounce_cat, dsn_code, dsn_diag, link_url, source_ip, variant, ")
	b.WriteString("event_at, event_epoch_ms, ingested_at, source, dt FROM ")
	b.WriteString(lakeTable)
	b.WriteString(" WHERE (lower(email) = ")
	b.WriteString(sqlStr(em))

	// Dedupe + re-validate the snapshot ids; only well-formed UUIDs reach SQL.
	seen := make(map[string]bool, len(ids))
	valid := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] || !uuidRe.MatchString(id) {
			continue
		}
		seen[id] = true
		valid = append(valid, sqlStr(id))
	}
	if len(valid) > 0 {
		b.WriteString(" OR subscriber_id IN (")
		b.WriteString(strings.Join(valid, ", "))
		b.WriteString(")")
	}
	b.WriteString(") AND dt >= ")
	b.WriteString(sqlStr(minDt))
	b.WriteString(" ORDER BY event_epoch_ms DESC LIMIT ")
	b.WriteString(strconv.Itoa(clampMemberEventsLimit(limit)))
	return b.String(), nil
}

// AudienceMember looks up one address in the dt snapshot (every list row it
// appears in) plus its last-90-day event history from email_events, matched
// by both email and any subscriber ids found in the snapshot rows.
func (r *Reader) AudienceMember(ctx context.Context, dt, email string, eventsLimit int) ([]AudienceProfile, []Event, error) {
	sqlA, err := buildAudienceMemberProfileSQL(dt, email)
	if err != nil {
		return nil, nil, err
	}
	_, rows, err := r.runQuery(ctx, sqlA)
	if err != nil {
		return nil, nil, err
	}
	profiles := make([]AudienceProfile, 0, len(rows))
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		p := scanAudienceProfile(row)
		profiles = append(profiles, p)
		ids = append(ids, p.ID)
	}

	minDt := time.Now().UTC().AddDate(0, 0, -90).Format("2006-01-02")
	sqlB, err := buildAudienceMemberEventsSQL(email, ids, minDt, eventsLimit)
	if err != nil {
		return nil, nil, err
	}
	_, erows, err := r.runQuery(ctx, sqlB)
	if err != nil {
		return nil, nil, err
	}
	events := make([]Event, 0, len(erows))
	for _, row := range erows {
		events = append(events, scanEvent(row))
	}
	return profiles, events, nil
}

// scanAudienceProfile maps a result row (string cells) into an
// AudienceProfile. Column order MUST match audienceColumns.
func scanAudienceProfile(row []string) AudienceProfile {
	get := func(i int) string {
		if i < len(row) {
			return row[i]
		}
		return ""
	}
	f64 := func(i int) float64 { v, _ := strconv.ParseFloat(get(i), 64); return v }
	i64 := func(i int) int64 { v, _ := strconv.ParseInt(get(i), 10, 64); return v }
	b, _ := strconv.ParseBool(get(31))
	return AudienceProfile{
		ID:                  get(0),
		OrganizationID:      get(1),
		ListID:              get(2),
		Email:               get(3),
		EmailDomain:         get(4),
		ISP:                 get(5),
		Status:              get(6),
		Source:              get(7),
		DataSource:          get(8),
		SourceSystem:        get(9),
		SourceDetail:        get(10),
		AcquiredAt:          get(11),
		AcquiredDt:          get(12),
		ImportedAt:          get(13),
		CreatedAt:           get(14),
		SubscribedAt:        get(15),
		UnsubscribedAt:      get(16),
		HardBouncedAt:       get(17),
		ComplainedAt:        get(18),
		ChurnedDt:           get(19),
		ChurnReason:         get(20),
		EngagementScore:     f64(21),
		EngagementBand:      get(22),
		TotalOpens:          i64(23),
		TotalClicks:         i64(24),
		TotalEmailsReceived: i64(25),
		LastOpenAt:          get(26),
		LastClickAt:         get(27),
		LastEmailAt:         get(28),
		DataQualityScore:    f64(29),
		VerificationStatus:  get(30),
		IsBot:               b,
		ChurnRiskScore:      f64(32),
	}
}

// --- package-level convenience wrappers (used by HTTP handlers) ---

// AudienceLatestDt runs against the global reader. Returns errDisabled when
// the reader is not configured.
func AudienceLatestDt(ctx context.Context) (string, int64, error) {
	r := getReader()
	if r == nil {
		return "", 0, errDisabled
	}
	return r.AudienceLatestDt(ctx)
}

// AudienceBreakdown runs against the global reader. Returns errDisabled when
// the reader is not configured.
func AudienceBreakdown(ctx context.Context, f AudienceBreakdownFilter) ([]AudienceRow, error) {
	r := getReader()
	if r == nil {
		return nil, errDisabled
	}
	return r.AudienceBreakdown(ctx, f)
}

// AudienceSourcePerformance runs against the global reader. Returns
// errDisabled when the reader is not configured.
func AudienceSourcePerformance(ctx context.Context, f SourcePerfFilter) ([]SourcePerfRow, error) {
	r := getReader()
	if r == nil {
		return nil, errDisabled
	}
	return r.AudienceSourcePerformance(ctx, f)
}

// AudienceFirstTouch runs against the global reader. Returns errDisabled when
// the reader is not configured.
func AudienceFirstTouch(ctx context.Context, from, to string) ([]FirstTouchRow, error) {
	r := getReader()
	if r == nil {
		return nil, errDisabled
	}
	return r.AudienceFirstTouch(ctx, from, to)
}

// AudienceMember runs against the global reader. Returns errDisabled when the
// reader is not configured.
func AudienceMember(ctx context.Context, dt, email string, eventsLimit int) ([]AudienceProfile, []Event, error) {
	r := getReader()
	if r == nil {
		return nil, nil, errDisabled
	}
	return r.AudienceMember(ctx, dt, email, eventsLimit)
}
