// ENGAGEMENT-GRID BUCKET QUERY (read side). One Athena query per
// (event, window) returning the DISTINCT (subscriber_id, brand) pairs with
// that engagement in the window — the exact shape the nightly Fargate job's
// unload_window() UNLOADs (.scratch/analytics_lake/
// refresh_engagement_segments_from_lake.py), consumed in-platform by
// worker.SegmentGridWorker, which buckets the pairs per brand and rebuilds
// the per-sending-domain '<CODE> <N>D (Openers|Clickers)' grid from them.
//
// Why a bucket query and not per-segment BuildSegmentMembers: the grid is
// 27 brands × 8 cells; per-segment queries would rescan the same window
// partitions ~27×. One query per (event, window) — at most 8 per pass —
// keeps Athena spend identical to the Fargate job it replaces (this
// platform got burned on Athena/NAT cost).
//
// Python-parity notes (the sidecar script is the spec):
//   - source IN ('app','ses') — PMTA accounting rows carry no opens/clicks.
//   - dt-pruned AND event_at-pruned, both to [now-window, now] UTC.
//   - subscriber_id <> '' — identity is the subscriber id; email resolution
//     happens against Postgres in the worker (the Python job's DB resolve
//     path), NOT against the lake audience snapshot, because the grid's
//     exclude_never_clickers predicate needs mailing_subscribers columns the
//     snapshot does not carry.
//
// Same contract as the rest of this package: DISABLED BY DEFAULT
// (errDisabled when InitReader was never configured), READ ONLY, and every
// interpolated value is whitelist/format validated before rendering via
// sqlStr.
package analytics

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// gridBucketMaxPairs caps how many (subscriber_id, brand) pairs one bucket
// read may page back through GetQueryResults. The daily union measures
// ~200-500k pairs; beyond 2M something upstream is wrong (e.g. a brand
// column regression fanning pairs out) and the caller must fail the pass
// rather than grind pagination forever.
const gridBucketMaxPairs = 2_000_000

// GridPair is one distinct (subscriber, brand-apex) engagement pair.
type GridPair struct {
	SubscriberID string
	BrandApex    string
}

// buildGridBucketSQL renders the bucket query for one (event, window).
// fromDt/toDt bound the dt partition prune; fromEpochMs is the precise
// timestamp prune within the edge partition (event_epoch_ms is the
// canonical numeric time column the other readers use — format-proof where
// a string compare on event_at is not). All three are computed by
// BuildEngagementGridBucket and re-validated here so the SQL shape is
// unit-testable without an Athena client or a clock.
func buildGridBucketSQL(event, fromDt, toDt string, fromEpochMs int64) (string, error) {
	switch event {
	case SegmentEventOpen, SegmentEventClick:
	default:
		return "", fmt.Errorf("invalid event %q: must be %q or %q", event, SegmentEventOpen, SegmentEventClick)
	}
	for _, p := range []struct{ name, v string }{{"from", fromDt}, {"to", toDt}} {
		if !dtRe.MatchString(p.v) {
			return "", fmt.Errorf("invalid %s dt: must be YYYY-MM-DD", p.name)
		}
	}
	if fromDt > toDt {
		return "", fmt.Errorf("invalid range: from %s is after to %s", fromDt, toDt)
	}
	if fromEpochMs <= 0 {
		return "", fmt.Errorf("invalid fromEpochMs %d", fromEpochMs)
	}
	return "SELECT subscriber_id, brand FROM " + lakeTable +
		" WHERE event_type = " + sqlStr(event) +
		" AND source IN ('app','ses')" +
		" AND dt BETWEEN " + sqlStr(fromDt) + " AND " + sqlStr(toDt) +
		" AND event_epoch_ms >= " + strconv.FormatInt(fromEpochMs, 10) +
		" AND subscriber_id <> ''" +
		" GROUP BY subscriber_id, brand" +
		" LIMIT " + strconv.Itoa(gridBucketMaxPairs+1), nil
}

// BuildEngagementGridBucket returns the distinct (subscriber_id, brand)
// pairs for one engagement event over the trailing window. WindowDays is
// clamped to [1,120] like every other window in this package.
func (r *Reader) BuildEngagementGridBucket(ctx context.Context, event string, windowDays int) ([]GridPair, error) {
	windowDays = ClampSegmentWindowDays(windowDays)
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -windowDays)
	toDt := now.Format("2006-01-02")
	fromDt := from.Format("2006-01-02")

	sql, err := buildGridBucketSQL(event, fromDt, toDt, from.UnixMilli())
	if err != nil {
		return nil, err
	}
	_, rows, err := r.runQuery(ctx, sql)
	if err != nil {
		return nil, err
	}
	if len(rows) > gridBucketMaxPairs {
		return nil, fmt.Errorf("grid bucket %s/%dd exceeds %d pairs — refusing to page an unbounded result",
			event, windowDays, gridBucketMaxPairs)
	}
	out := make([]GridPair, 0, len(rows))
	for _, row := range rows {
		if len(row) < 2 || row[0] == "" {
			continue
		}
		out = append(out, GridPair{SubscriberID: row[0], BrandApex: row[1]})
	}
	return out, nil
}

// BuildEngagementGridBucket runs against the global reader. Returns
// errDisabled when the reader is not configured.
func BuildEngagementGridBucket(ctx context.Context, event string, windowDays int) ([]GridPair, error) {
	r := getReader()
	if r == nil {
		return nil, errDisabled
	}
	return r.BuildEngagementGridBucket(ctx, event, windowDays)
}
