package analytics

// Deferral funnel READ query. For every message (campaign_id, lower(email)) that
// was DEFERRED (event_type='delivery_delay') in the [from,to] Denver-day window,
// classify its later fate: RECOVERED (a 'delivered' event exists), BOUNCED (a
// hard/soft bounce exists, no delivery), or PENDING (neither). Grouped by the RAW
// isp_group column so it joins the existing per-ISP tables 1:1.
//
// READ ONLY, SQL-injection safe (every caller value validated then rendered via
// sqlStr), and DISABLED-aware (returns errDisabled when the reader is off) —
// identical contract to Breakdown above.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// DeferralFunnelRow is one per-ISP bucket of the deferral funnel.
type DeferralFunnelRow struct {
	ISPGroup  string `json:"isp_group"`
	Deferred  int64  `json:"deferred"`
	Recovered int64  `json:"recovered"`
	Pending   int64  `json:"pending"`
	Bounced   int64  `json:"bounced"`
}

// buildDeferralFunnelSQL validates the inputs and renders the full Athena query.
// Factored out of DeferralFunnel so the SQL shape is testable without a client.
//
// The cohort is anchored on the DEFERRED event falling inside the Denver-day
// window (localDtExpr BETWEEN from AND to); delivery/bounce can land on any later
// dt, so the dt partition bound is widened ±1 day (shiftDt) exactly as
// buildBreakdownSQL widens for Denver-derived buckets.
func buildDeferralFunnelSQL(from, to, brand string) (string, error) {
	if from == "" || to == "" {
		return "", fmt.Errorf("from and to dates are required")
	}
	if err := validateDt("from", from); err != nil {
		return "", err
	}
	if err := validateDt("to", to); err != nil {
		return "", err
	}
	// Both dates are YYYY-MM-DD, so lexical order == chronological order; reject
	// an inverted range explicitly (mirrors validateBreakdownFilter).
	if from > to {
		return "", fmt.Errorf("invalid range: from %s is after to %s", from, to)
	}
	// brand is optional; when present it must match the same dotted-identifier
	// pattern Breakdown applies to its brand Eq column (brands are apex domains).
	if brand != "" && !dottedRe.MatchString(brand) {
		return "", fmt.Errorf("invalid value for brand")
	}

	// Cohort anchor (the deferral event) needs only the ±1-day Denver↔UTC skew,
	// but the FATE events (delivered/bounced/flushed) land on any later dt —
	// PMTA retries throttle-deferred mail for up to ~72h. Scanning only to+1
	// classified late recoveries as "pending" (delivered=0 in the scanned
	// window), overstating pending and understating recovered at the tail of
	// every historical range. +4 covers the retry horizon plus the skew day.
	dtFrom, dtTo := shiftDt(from, -1), shiftDt(to, 4)

	var b strings.Builder
	b.WriteString("WITH msg AS (")
	b.WriteString("SELECT isp_group, campaign_id, lower(email) AS recipient,")
	b.WriteString(" max(CASE WHEN event_type='delivery_delay' AND ")
	b.WriteString(localDtExpr)
	b.WriteString(" BETWEEN ")
	b.WriteString(sqlStr(from))
	b.WriteString(" AND ")
	b.WriteString(sqlStr(to))
	b.WriteString(" THEN 1 ELSE 0 END) AS deferred,")
	b.WriteString(" max(CASE WHEN event_type='delivered' THEN 1 ELSE 0 END) AS delivered,")
	b.WriteString(" max(CASE WHEN event_type IN ('hard_bounce','soft_bounce') THEN 1 ELSE 0 END) AS bounced,")
	// flushed: operator `pmta flush` cancellation (diagnostic 'deleted by
	// administrator'). These are administrative cancellations, not deliverability
	// outcomes, so flushed messages are dropped from the cohort entirely (WHERE
	// flushed=0 below) — a deferred-then-flushed message is neither recovered nor a
	// real bounce. Keeps the funnel consistent with the soft-bounce reclass.
	b.WriteString(" max(CASE WHEN lower(dsn_diag) LIKE '%deleted by administrator%' THEN 1 ELSE 0 END) AS flushed")
	b.WriteString(" FROM ")
	b.WriteString(lakeTable)
	b.WriteString(" WHERE dt BETWEEN ")
	b.WriteString(sqlStr(dtFrom))
	b.WriteString(" AND ")
	b.WriteString(sqlStr(dtTo))
	b.WriteString(" AND source='pmta' AND route_type='pmta_direct'")
	b.WriteString(" AND campaign_id <> '' AND email <> ''")
	if brand != "" {
		// brandExpr, not the raw column: pmta rows historically carry brand=''
		// (the vmta code is the only brand signal), so a raw brand= filter
		// silently emptied the funnel whenever a brand was selected.
		b.WriteString(" AND ")
		b.WriteString(brandExpr)
		b.WriteString(" = ")
		b.WriteString(sqlStr(brand))
	}
	b.WriteString(" GROUP BY isp_group, campaign_id, lower(email)")
	b.WriteString(") SELECT isp_group,")
	b.WriteString(" count(*) FILTER (WHERE deferred=1) AS deferred,")
	b.WriteString(" count(*) FILTER (WHERE deferred=1 AND delivered=1) AS recovered,")
	b.WriteString(" count(*) FILTER (WHERE deferred=1 AND delivered=0 AND bounced=0) AS pending,")
	b.WriteString(" count(*) FILTER (WHERE deferred=1 AND delivered=0 AND bounced=1) AS bounced")
	b.WriteString(" FROM msg WHERE deferred=1 AND flushed=0")
	b.WriteString(" GROUP BY isp_group ORDER BY deferred DESC")
	return b.String(), nil
}

// DeferralFunnel runs the per-ISP deferral funnel against the global reader.
// Returns errDisabled when the reader is not configured (mirrors Breakdown).
func DeferralFunnel(ctx context.Context, from, to, brand string) ([]DeferralFunnelRow, error) {
	r := getReader()
	if r == nil {
		return nil, errDisabled
	}
	sql, err := buildDeferralFunnelSQL(from, to, brand)
	if err != nil {
		return nil, err
	}
	_, rows, err := r.runQuery(ctx, sql)
	if err != nil {
		return nil, err
	}
	out := make([]DeferralFunnelRow, 0, len(rows))
	for _, row := range rows {
		if len(row) < 5 {
			continue
		}
		deferred, _ := strconv.ParseInt(row[1], 10, 64)
		recovered, _ := strconv.ParseInt(row[2], 10, 64)
		pending, _ := strconv.ParseInt(row[3], 10, 64)
		bounced, _ := strconv.ParseInt(row[4], 10, 64)
		out = append(out, DeferralFunnelRow{
			ISPGroup:  row[0],
			Deferred:  deferred,
			Recovered: recovered,
			Pending:   pending,
			Bounced:   bounced,
		})
	}
	return out, nil
}
