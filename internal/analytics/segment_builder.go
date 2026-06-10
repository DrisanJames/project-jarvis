// LAKE SEGMENT BUILDER (read side). Builds standard recency-segment member
// sets — "opened/clicked in the last N days", globally / per brand / per ISP —
// from the Athena event lake instead of the hot-DB
// mailing_subscribers × mailing_tracking_events scan that MaterializeSegment
// runs. Mirrors the standard nightly job
// .scratch/analytics_lake/refresh_engagement_segments_from_lake.py:
//
//   - engagement events are taken from email_events with
//     source IN ('app','ses') (PMTA accounting rows carry no opens/clicks),
//   - the recency window is dt-pruned ([today-window, today]) so a build NEVER
//     runs an unpruned scan (this platform got burned on Athena/NAT cost),
//   - identity is resolved against the latest complete `audience` snapshot
//     partition (AudienceLatestDt) using the keyed-UNION dedup join from
//     reader_audience.go AudienceSourcePerformance — the event side keys on
//     CASE WHEN subscriber_id <> ” THEN subscriber_id ELSE lower(email) END
//     and the audience side offers BOTH keys, GROUPed by join_key so an email
//     present in multiple lists cannot fan a single event out,
//   - seed-list exclusion mirrors the Python job's mechanism: the caller
//     resolves the seed mailing_lists ids (name ILIKE '%seed%') from Postgres
//     and passes them in SeedListIDs; audience rows whose list_id is in that
//     set are dropped before the join.
//
// Same contract as the rest of this package: DISABLED BY DEFAULT (errDisabled
// when InitReader was never configured), READ ONLY against the lake, and
// SQL-injection safe — every caller-supplied value is whitelist/regex
// validated before being rendered as a quoted literal via sqlStr. NEVER
// Athena ExecutionParameters (see the sqlStr comment in reader.go).
//
// SCALE CEILING: results are read back through the paginated
// GetQueryResults path (runQuery), capped at maxSegmentMembers (1,000,000)
// rows. Segments larger than that must go through the nightly Fargate
// refresh; the future in-platform path for >1M members is UNLOAD-to-S3 (write
// gzip JSON to s3://ignite-analytics-lake/_seg/... and stream the parts), the
// same shape the Python job already uses.
package analytics

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Segment spec whitelists. Events map 1:1 onto lake email_events.event_type
// values; scopes select which predicate set buildSegmentMembersSQL renders.
const (
	SegmentEventOpen  = "open"
	SegmentEventClick = "click"

	SegmentScopeGlobal = "global"
	SegmentScopeBrand  = "brand"
	SegmentScopeISP    = "isp"
)

// maxSegmentMembers caps how many member rows an in-platform build may pull
// back through paginated GetQueryResults. Beyond this the operator must use
// the nightly Fargate path (UNLOAD-to-S3 is the future >1M in-platform path).
const maxSegmentMembers = 1_000_000

// minSegmentWindowDays / maxSegmentWindowDays bound the recency window. The
// window is clamped (not rejected) so a defensive caller can never widen the
// dt prune past 120 days.
const (
	minSegmentWindowDays = 1
	maxSegmentWindowDays = 120
)

// SegmentSpec describes one standard recency segment to build from the lake.
type SegmentSpec struct {
	Event        string   // SegmentEventOpen | SegmentEventClick
	WindowDays   int      // recency window, clamped to [1,120]
	Scope        string   // SegmentScopeGlobal | SegmentScopeBrand | SegmentScopeISP
	BrandApex    string   // required when Scope==brand; apex domain as stored in email_events.brand (dottedRe)
	ISP          string   // required when Scope==isp; matches audience.isp (tokenRe, lowercase)
	ExcludeSeeds bool     // when true, audience rows whose list_id is in SeedListIDs are excluded
	SeedListIDs  []string // seed mailing_lists ids (uuidRe each); resolved by the caller from Postgres
}

// SegmentMember is one resolved member row: the audience snapshot's
// subscriber id plus its (lowercase) email.
type SegmentMember struct {
	SubscriberID string
	Email        string
}

// ClampSegmentWindowDays clamps a recency window into
// [minSegmentWindowDays, maxSegmentWindowDays].
func ClampSegmentWindowDays(n int) int {
	if n < minSegmentWindowDays {
		return minSegmentWindowDays
	}
	if n > maxSegmentWindowDays {
		return maxSegmentWindowDays
	}
	return n
}

// ValidateSegmentSpec strictly validates every caller-supplied piece of a
// SegmentSpec against the whitelists/regexes above. Nothing reaches SQL
// construction until this passes. WindowDays is NOT validated here — it is
// clamped by ClampSegmentWindowDays wherever a window is consumed.
func ValidateSegmentSpec(spec SegmentSpec) error {
	switch spec.Event {
	case SegmentEventOpen, SegmentEventClick:
	default:
		return fmt.Errorf("invalid event %q: must be %q or %q", spec.Event, SegmentEventOpen, SegmentEventClick)
	}

	switch spec.Scope {
	case SegmentScopeGlobal, SegmentScopeBrand, SegmentScopeISP:
	default:
		return fmt.Errorf("invalid scope %q: must be %q, %q or %q", spec.Scope, SegmentScopeGlobal, SegmentScopeBrand, SegmentScopeISP)
	}

	if spec.Scope == SegmentScopeBrand {
		if spec.BrandApex == "" {
			return fmt.Errorf("brand_apex is required when scope is %q", SegmentScopeBrand)
		}
		if !dottedRe.MatchString(spec.BrandApex) {
			return fmt.Errorf("invalid brand_apex: must match ^[a-zA-Z0-9_.-]{1,255}$")
		}
	}

	if spec.Scope == SegmentScopeISP {
		if spec.ISP == "" {
			return fmt.Errorf("isp is required when scope is %q", SegmentScopeISP)
		}
		if !tokenRe.MatchString(spec.ISP) {
			return fmt.Errorf("invalid isp: must match ^[a-zA-Z0-9_-]+$")
		}
	}

	for _, id := range spec.SeedListIDs {
		if !uuidRe.MatchString(id) {
			return fmt.Errorf("invalid seed list id %q: must be a UUID", id)
		}
	}
	return nil
}

// buildSegmentMembersSQL validates spec and renders the full Athena query.
// audienceDt / fromDt / toDt are computed by the caller (BuildSegmentMembers)
// and re-validated here for defense in depth; factoring the dates out keeps
// the SQL shape exactly unit-testable without an Athena client or a clock.
//
// SQL shape (validated literals only, via sqlStr) — single pass, dt-pruned on
// BOTH tables, keyed-UNION dedup join copied from buildAudienceSourcePerfSQL
// (Athena/Presto cannot OR-join `e.key IN (a.id, a.email)` efficiently, and
// without the GROUP BY join_key a multi-list email would fan events out):
//
//	WITH aud AS (
//	  SELECT id, email FROM audience
//	  WHERE dt = '<audienceDt>' AND status = 'confirmed'
//	    [AND isp = '<isp>']                       -- scope=isp (audience-side column)
//	    [AND list_id NOT IN ('<uuid>', ...)]      -- ExcludeSeeds + SeedListIDs
//	), keyed AS (
//	  SELECT join_key, max(id) AS sub_id, max(email) AS email FROM (
//	    SELECT id AS join_key, id, email FROM aud
//	    UNION ALL
//	    SELECT email AS join_key, id, email FROM aud
//	  ) GROUP BY join_key
//	)
//	SELECT DISTINCT k.sub_id, k.email
//	FROM email_events e
//	JOIN keyed k ON (CASE WHEN e.subscriber_id <> '' THEN e.subscriber_id
//	                      ELSE lower(e.email) END) = k.join_key
//	WHERE e.dt BETWEEN '<today-window>' AND '<today>'
//	  AND e.event_type = '<open|click>'
//	  AND e.source IN ('app','ses')               -- PMTA rows carry no opens/clicks (Python-job parity)
//	  [AND e.brand = '<brand_apex>']              -- scope=brand
//	LIMIT <maxSegmentMembers+1>
//
// The LIMIT is maxSegmentMembers+1 so the reader can detect (and reject with
// a clear error) a segment that exceeds the in-platform cap without paging an
// unbounded result set back. Seed ids render in sorted order so the output is
// deterministic (tests assert exact SQL).
func buildSegmentMembersSQL(spec SegmentSpec, audienceDt, fromDt, toDt string) (string, error) {
	if err := ValidateSegmentSpec(spec); err != nil {
		return "", err
	}
	for _, p := range []struct{ name, dt string }{
		{"audience dt", audienceDt}, {"from", fromDt}, {"to", toDt},
	} {
		if p.dt == "" || !dtRe.MatchString(p.dt) {
			return "", fmt.Errorf("invalid %s: must be YYYY-MM-DD", p.name)
		}
	}
	if fromDt > toDt {
		return "", fmt.Errorf("invalid range: from %s is after to %s", fromDt, toDt)
	}

	var b strings.Builder
	b.WriteString("WITH aud AS (SELECT id, email FROM ")
	b.WriteString(audienceTable)
	b.WriteString(" WHERE dt = ")
	b.WriteString(sqlStr(audienceDt))
	b.WriteString(" AND status = 'confirmed'")
	if spec.Scope == SegmentScopeISP {
		b.WriteString(" AND isp = ")
		b.WriteString(sqlStr(spec.ISP))
	}
	if spec.ExcludeSeeds && len(spec.SeedListIDs) > 0 {
		ids := append([]string(nil), spec.SeedListIDs...)
		sort.Strings(ids)
		quoted := make([]string, 0, len(ids))
		for _, id := range ids {
			quoted = append(quoted, sqlStr(id))
		}
		b.WriteString(" AND list_id NOT IN (")
		b.WriteString(strings.Join(quoted, ", "))
		b.WriteString(")")
	}
	b.WriteString("), keyed AS (SELECT join_key, max(id) AS sub_id, max(email) AS email FROM (")
	b.WriteString("SELECT id AS join_key, id, email FROM aud")
	b.WriteString(" UNION ALL ")
	b.WriteString("SELECT email AS join_key, id, email FROM aud")
	b.WriteString(") GROUP BY join_key)")
	b.WriteString(" SELECT DISTINCT k.sub_id, k.email FROM ")
	b.WriteString(lakeTable)
	b.WriteString(" e JOIN keyed k ON (CASE WHEN e.subscriber_id <> '' THEN e.subscriber_id ELSE lower(e.email) END) = k.join_key")
	b.WriteString(" WHERE e.dt BETWEEN ")
	b.WriteString(sqlStr(fromDt))
	b.WriteString(" AND ")
	b.WriteString(sqlStr(toDt))
	b.WriteString(" AND e.event_type = ")
	b.WriteString(sqlStr(spec.Event))
	b.WriteString(" AND e.source IN ('app','ses')")
	if spec.Scope == SegmentScopeBrand {
		b.WriteString(" AND e.brand = ")
		b.WriteString(sqlStr(spec.BrandApex))
	}
	b.WriteString(" LIMIT ")
	b.WriteString(strconv.Itoa(maxSegmentMembers + 1))
	return b.String(), nil
}

// BuildSegmentMembers resolves the latest complete audience snapshot
// partition, renders the recency query for spec, and returns the distinct
// (subscriber_id, email) member set. Returns a clear error when the result
// exceeds maxSegmentMembers — that segment belongs on the nightly Fargate
// path (future in-platform answer: UNLOAD-to-S3).
func (r *Reader) BuildSegmentMembers(ctx context.Context, spec SegmentSpec) ([]SegmentMember, error) {
	spec.WindowDays = ClampSegmentWindowDays(spec.WindowDays)
	if err := ValidateSegmentSpec(spec); err != nil {
		return nil, err
	}

	audienceDt, _, err := r.AudienceLatestDt(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve audience snapshot: %w", err)
	}
	if audienceDt == "" {
		return nil, fmt.Errorf("no audience snapshot partition available (last 4 days)")
	}

	now := time.Now().UTC()
	toDt := now.Format("2006-01-02")
	fromDt := now.AddDate(0, 0, -spec.WindowDays).Format("2006-01-02")

	sql, err := buildSegmentMembersSQL(spec, audienceDt, fromDt, toDt)
	if err != nil {
		return nil, err
	}

	_, rows, err := r.runQuery(ctx, sql)
	if err != nil {
		return nil, err
	}
	if len(rows) > maxSegmentMembers {
		return nil, fmt.Errorf(
			"segment exceeds the in-platform build cap (%d+ members > %d): build it on the nightly Fargate refresh instead (UNLOAD-to-S3 is the future in-platform path for >1M)",
			len(rows), maxSegmentMembers)
	}

	out := make([]SegmentMember, 0, len(rows))
	for _, row := range rows {
		if len(row) < 2 || row[0] == "" || row[1] == "" {
			continue
		}
		out = append(out, SegmentMember{SubscriberID: row[0], Email: row[1]})
	}
	return out, nil
}

// BuildSegmentMembers runs against the global reader. Returns errDisabled
// when the reader is not configured.
func BuildSegmentMembers(ctx context.Context, spec SegmentSpec) ([]SegmentMember, error) {
	r := getReader()
	if r == nil {
		return nil, errDisabled
	}
	return r.BuildSegmentMembers(ctx, spec)
}
