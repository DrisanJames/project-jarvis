package analytics

// Lake ACTION-CLICK reader (operator 2026-08-01, non-CPM performance view).
//
// Why this exists rather than another Breakdown() call:
//
//  1. Breakdown counts COUNT(DISTINCT event_uid) — click EVENTS. The operator's
//     funnel is volume → CLICKERS → conversions, which needs distinct PEOPLE.
//  2. Breakdown has no link_url predicate, and a raw click count is vendor
//     telemetry, not engagement: measured on the lake 2026-08-01 over dt >=
//     2026-07-25, of 435,861 click rows only 188,854 were navigational —
//     34,277 were stylesheet/font fetches (fonts.googleapis.com), 47,235 were
//     unsubscribe/preference/privacy links. Counting those as "clickers" is the
//     asset-fetch inflation documented in the click-cohort doctrine.
//
// PERSON KEY IS subscriber_id, NEVER email. Verified on the lake 2026-08-01:
// every source='app' click row (388,692 of 435,861 = 89%) carries an EMPTY
// email, so COUNT(DISTINCT email) collapses the whole app click stream to a
// single "person". subscriber_id is populated on 100% of click rows in both
// the 'app' and 'ses' streams.
//
// The ACTION predicate mirrors PG_CLICK_ASSET / PG_CLICK_NONNAV in
// agents/dbknowledge/_db.py, with ONE deliberate divergence: the PG doctrine
// drops every t.em.* URL as an "unresolved tracker self-reference", but in the
// lake t.em carries two distinct path families (verified 2026-08-01) —
// `/o/<brand>/<uuid>` is the SMART-LINK OFFER REDIRECT (60,677 events / 1,797
// people; the money click) and `/track/...` is the tracking-service record
// whose destination really is unproven (109,834 events). Dropping t.em
// wholesale would discard the majority of real offer engagement, so only the
// `/track/` family is excluded here. `/preferences?...` is already caught by
// the compliance term.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ActionClickRow is one ISP bucket of navigational click activity.
type ActionClickRow struct {
	ISP      string `json:"isp"`
	Clickers int64  `json:"clickers"` // COUNT(DISTINCT subscriber_id)
	Clicks   int64  `json:"clicks"`   // COUNT(DISTINCT event_uid)
}

// ActionClickFilter scopes an action-click read to a campaign set and a Denver
// day range. CampaignIDs is REQUIRED — this reader is never allowed to run
// estate-wide (an unscoped click scan is exactly the shape that produced the
// 2026-07 Athena S3 GET storm).
type ActionClickFilter struct {
	From        string   // YYYY-MM-DD, Denver day, required
	To          string   // YYYY-MM-DD, Denver day, required
	CampaignIDs []string // required, 1..maxBreakdownCampaignIDs, each a UUID
	Limit       int      // clamped like Breakdown; default 1000
}

// lakeActionClickPredicate is the navigational/commercial click filter. Kept as
// a single const so the handler, the tests, and any future consumer cannot
// drift from one another.
const lakeActionClickPredicate = `link_url IS NOT NULL AND link_url <> ''` +
	// asset extensions
	` AND NOT regexp_like(link_url, '(?i)\.(css|js|woff2?|ttf|otf|eot|png|jpe?g|gif|svg|ico|webp|map)([?#]|$)')` +
	// asset/CDN hosts
	` AND NOT regexp_like(link_url, '(?i)(fonts\.g|cdn\.|cloudfront|akamai|fastly|jsdelivr|unpkg|gstatic)')` +
	// compliance links (unsub / preference centre / privacy) — never engagement
	` AND NOT regexp_like(link_url, '(?i)unsub|optout|opt-out|preference|/privacy')` +
	// synthetic markers written by the everflow conversion importer
	` AND NOT regexp_like(link_url, '(?i)^everflow-import:')` +
	// t.em tracking-service records whose destination is unproven; the
	// /o/ offer-redirect family is deliberately NOT excluded (see file header)
	` AND NOT regexp_like(link_url, '(?i)^https?://t\.em\.[^/]+/track/')`

// buildActionClickSQL renders the validated query. Split out so it can be
// asserted in tests without an Athena client.
func buildActionClickSQL(f ActionClickFilter) (string, error) {
	if err := validateDt("from", f.From); err != nil {
		return "", err
	}
	if err := validateDt("to", f.To); err != nil {
		return "", err
	}
	if len(f.CampaignIDs) == 0 {
		return "", fmt.Errorf("action clicks: CampaignIDs is required")
	}
	if len(f.CampaignIDs) > maxBreakdownCampaignIDs {
		return "", fmt.Errorf("action clicks: %d campaign ids exceeds the %d cap",
			len(f.CampaignIDs), maxBreakdownCampaignIDs)
	}
	seen := make(map[string]bool, len(f.CampaignIDs))
	quoted := make([]string, 0, len(f.CampaignIDs))
	for _, id := range f.CampaignIDs {
		if err := validateUUID("campaign_id", id); err != nil {
			return "", err
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		quoted = append(quoted, sqlStr(id))
	}

	// The window is a DENVER day range, so widen the UTC dt partition bound by
	// ±1 day and apply the exact local-day predicate — same rule Breakdown
	// applies whenever a Denver-derived bucket is involved.
	var b strings.Builder
	b.WriteString("SELECT ")
	b.WriteString(ispExpr)
	b.WriteString(" isp, COUNT(DISTINCT subscriber_id) clickers, COUNT(DISTINCT event_uid) clicks FROM ")
	b.WriteString(lakeTable)
	b.WriteString(" WHERE dt BETWEEN ")
	b.WriteString(sqlStr(shiftDt(f.From, -1)))
	b.WriteString(" AND ")
	b.WriteString(sqlStr(shiftDt(f.To, 1)))
	b.WriteString(" AND ")
	b.WriteString(localDtExpr)
	b.WriteString(" BETWEEN ")
	b.WriteString(sqlStr(f.From))
	b.WriteString(" AND ")
	b.WriteString(sqlStr(f.To))
	b.WriteString(" AND event_type = 'click'")
	b.WriteString(" AND subscriber_id IS NOT NULL AND subscriber_id <> ''")
	b.WriteString(" AND campaign_id IN (")
	b.WriteString(strings.Join(quoted, ", "))
	b.WriteString(") AND ")
	b.WriteString(lakeActionClickPredicate)
	b.WriteString(" GROUP BY ")
	b.WriteString(ispExpr)
	b.WriteString(" ORDER BY clickers DESC LIMIT ")
	b.WriteString(strconv.Itoa(clampBreakdownLimit(f.Limit)))
	return b.String(), nil
}

// ActionClicks returns navigational clicks + distinct clickers per ISP for a
// campaign set. Callers with more than maxBreakdownCampaignIDs campaigns must
// chunk and sum; note that per-chunk DISTINCT clicker counts sum with
// double-counting when one subscriber clicks campaigns that land in different
// chunks (the same second-order caveat the Offer Alignment engagement fetch
// carries, handlers_offer_alignment.go).
func (r *Reader) ActionClicks(ctx context.Context, f ActionClickFilter) ([]ActionClickRow, error) {
	sql, err := buildActionClickSQL(f)
	if err != nil {
		return nil, err
	}
	_, rows, err := r.runQuery(ctx, sql)
	if err != nil {
		return nil, err
	}
	out := make([]ActionClickRow, 0, len(rows))
	for _, row := range rows {
		if len(row) < 3 {
			continue
		}
		clickers, _ := strconv.ParseInt(row[1], 10, 64)
		clicks, _ := strconv.ParseInt(row[2], 10, 64)
		out = append(out, ActionClickRow{ISP: row[0], Clickers: clickers, Clicks: clicks})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Clickers > out[j].Clickers })
	return out, nil
}

// ActionClicks is the package-level entry point (mirrors Breakdown/Summary):
// returns IsDisabledErr when the lake reader was never initialised.
func ActionClicks(ctx context.Context, f ActionClickFilter) ([]ActionClickRow, error) {
	rd := getReader()
	if rd == nil {
		return nil, errDisabled
	}
	return rd.ActionClicks(ctx, f)
}
