package analytics

// Lane-snapshot READ layer — ONE Athena query covering the whole estate for
// ONE Denver day, consumed by worker.LaneSnapshotWorker.
//
// WHY THIS EXISTS: the previous accelerator for the Property Ledger lane
// scoreboard was a Postgres rollup (worker.LaneStatsRollupWorker). It ran on
// the SAME RDS instance as the request path and starved it — GET
// /property-ledger/stats began failing with "canceling statement due to user
// request" on every call — so it was killed (LANE_STATS_ROLLUP_DISABLED, commit
// fc7fec3). This query puts the scan on the LAKE instead, where it cannot
// contend with sending.
//
// ⚠️⚠️ THE LAKE USES PRESENT-TENSE 'open' / 'click'.
// Postgres mailing_tracking_events uses PAST-tense 'opened' / 'clicked'
// (internal/api/property_lane_stats.go laneStatsDaySQL). They are OPPOSITE
// conventions and using the wrong one returns a SILENT ZERO — no error, no
// warning, just an empty engagement column. Pinned by
// TestLaneSnapshotSQLUsesPresentTenseEventTypes (internal/worker).
//
// ⚠️ dt IS A PARTITION COLUMN (string 'YYYY-MM-DD', UTC). Every read must
// filter on it or the scan cost explodes across the whole table. A Denver day
// straddles exactly two UTC partitions (Denver day D = UTC [D 06:00, D+1
// 06:00)), so dt is pinned to {D, D+1} and localDtExpr narrows to the Denver
// day — the same ±1-day widening buildGrowthDeliverySQL uses. Pinned by
// TestLaneSnapshotSQLFiltersOnPartitionColumn.
//
// ⚠️ THE LAKE HAS NO `vertical` COLUMN. campaign_id → (vertical, brand) is
// resolved by the caller against mailing_campaigns; this layer never joins.
//
// READ ONLY. No Postgres, no writes.

import (
	"context"
	"strconv"
	"strings"
)

// LaneSnapshotLakeRow is one (campaign_id, isp_group, source, event_type)
// bucket for a single Denver day.
//
// Uniques is COUNT(DISTINCT subscriber_id) WITHIN THIS BUCKET — i.e. within
// one campaign. Rolling buckets up to a lane sums per-campaign uniques, so a
// subscriber who engaged with two campaigns in the same lane counts twice.
// That is inherent to grouping by campaign_id (which is the only way to reach
// vertical/brand at all) and is surfaced in the snapshot payload rather than
// hidden.
type LaneSnapshotLakeRow struct {
	CampaignID string `json:"campaign_id"`
	ISPGroup   string `json:"isp_group"`
	Source     string `json:"source"`
	EventType  string `json:"event_type"`
	Events     int64  `json:"events"`
	Uniques    int64  `json:"uniques"`
}

// LaneSnapshotEventTypes is the closed event-type set, PRESENT TENSE.
//
// Exported so the regression guard in internal/worker can assert on it without
// re-deriving the list: 'open'/'click' are the lake's spelling, and
// 'opened'/'clicked' (the Postgres spelling) must NEVER appear here.
var LaneSnapshotEventTypes = []string{"attempted", "delivered", "open", "click", "bounced"}

// BuildLaneSnapshotSQL renders the whole-estate aggregate for one Denver day.
//
// Exported (unlike the other builders in this package, which are unexported and
// tested in-package) because the silent-zero and partition-filter guards are
// worth running in the internal/worker suite the operator's verification recipe
// actually executes. denverDay must already be validated YYYY-MM-DD; it is
// rendered through sqlStr like every other literal here.
func BuildLaneSnapshotSQL(denverDay string) string {
	quoted := make([]string, 0, len(LaneSnapshotEventTypes))
	for _, et := range LaneSnapshotEventTypes {
		quoted = append(quoted, sqlStr(et))
	}
	var b strings.Builder
	b.WriteString("SELECT campaign_id, isp_group, source, event_type,")
	b.WriteString(" COUNT(*) n, COUNT(DISTINCT subscriber_id) uniq")
	b.WriteString(" FROM ")
	b.WriteString(lakeTable)
	// dt is the UTC partition column — never dropped. A Denver day spans two.
	b.WriteString(" WHERE dt IN (")
	b.WriteString(sqlStr(denverDay))
	b.WriteString(", ")
	b.WriteString(sqlStr(shiftDt(denverDay, 1)))
	b.WriteString(")")
	b.WriteString(" AND ")
	b.WriteString(localDtExpr)
	b.WriteString(" = ")
	b.WriteString(sqlStr(denverDay))
	b.WriteString(" AND event_type IN (")
	b.WriteString(strings.Join(quoted, ", "))
	b.WriteString(")")
	b.WriteString(" GROUP BY campaign_id, isp_group, source, event_type")
	return b.String()
}

// LaneSnapshot returns every (campaign, isp_group, source, event_type) bucket
// for the given Denver day.
//
// Measured on prod 2026-08-19 for a full Denver day: 9.6s, 52.9 MB scanned,
// 30,308 rows, 6,264 distinct campaigns.
func (r *Reader) LaneSnapshot(ctx context.Context, denverDay string) ([]LaneSnapshotLakeRow, error) {
	if err := validateDt("day", denverDay); err != nil {
		return nil, err
	}
	_, rows, err := r.runQuery(ctx, BuildLaneSnapshotSQL(denverDay))
	if err != nil {
		return nil, err
	}
	out := make([]LaneSnapshotLakeRow, 0, len(rows))
	for _, row := range rows {
		if len(row) < 6 {
			continue
		}
		n := func(i int) int64 { v, _ := strconv.ParseInt(row[i], 10, 64); return v }
		out = append(out, LaneSnapshotLakeRow{
			CampaignID: strings.TrimSpace(row[0]),
			ISPGroup:   strings.ToLower(strings.TrimSpace(row[1])),
			Source:     strings.ToLower(strings.TrimSpace(row[2])),
			EventType:  strings.ToLower(strings.TrimSpace(row[3])),
			Events:     n(4),
			Uniques:    n(5),
		})
	}
	return out, nil
}

// LaneSnapshot runs against the global reader. Returns errDisabled when the
// reader is not configured (IsDisabledErr reports it).
func LaneSnapshot(ctx context.Context, denverDay string) ([]LaneSnapshotLakeRow, error) {
	r := getReader()
	if r == nil {
		return nil, errDisabled
	}
	return r.LaneSnapshot(ctx, denverDay)
}
