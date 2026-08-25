package analytics

// Click-funnel READ layer — ONE Athena query covering every click-drip shadow
// campaign at DAY GRAIN, consumed by worker.ClickFunnelSnapshotWorker.
//
// WHY DAY GRAIN: the screen lets an operator pick a window (7/14/30d). Querying
// per window means a live Athena round trip per interaction (measured 3.0s and
// 0.31 GB for ONE lane, x2 passes). Storing per-day rows once lets any window be
// a re-aggregation of cached rows with ZERO request-time Athena.
//
// WHY ONE PASS: the previous screen ran a delivery pass and an engagement pass
// per lane because Breakdown cannot GROUP BY four dims. This builder emits
// source alongside the dims, so the CALLER applies the METRIC_CONTRACT §1
// app-stream rule (delivery from pmta/ses/kumo, engagement from app) instead of
// paying for two scans. Measured 2026-08-25 over all 42 node-scoped click-drip
// campaigns, 30 days: 4.3s / 3.10 GB. The 3-day incremental the worker actually
// runs: 3.0s / 0.25 GB.
//
// ⚠️ dt IS A PARTITION COLUMN (string 'YYYY-MM-DD', UTC). Every read filters on
// it or the scan cost explodes. Unlike LaneSnapshot this builder does NOT narrow
// to a Denver day — click-funnel windows are multi-day and the caller buckets by
// the UTC dt it stores, so no localDtExpr correction is applied or needed.
//
// ⚠️ THE LAKE USES PRESENT-TENSE 'open' / 'click' (METRIC_CONTRACT §10). The
// past-tense Postgres spelling returns a SILENT ZERO.
//
// ⚠️ is_machine_click IS INERT (METRIC_CONTRACT §1, re-verified 2026-08-25:
// zero `true` rows estate-wide over 7 days). This builder therefore reports
// classified/machine counts SEPARATELY so the caller can publish classification
// COVERAGE rather than silently presenting raw clicks as human ones.
//
// READ ONLY. No Postgres, no writes.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// ClickFunnelLakeRow is one (dt, campaign_id, source, event_type) bucket.
//
// The three count domains are all present because different metrics need
// different ones and re-deriving them costs another scan:
//
//	Events     — COUNT(DISTINCT event_uid); the PMTA bridge redelivers events.
//	Recipients — COUNT(DISTINCT subscriber_id); the UNIQUE open/click metric.
//	             email is NULL on ~87% of app-source rows, so it cannot be the key.
//	Mailboxes  — COUNT(DISTINCT email); the deferral metric — delay notices are
//	             per-retry and event_uid-counting them overstates held mail ~2.6x.
//
// Classified/Machine are subsets of Recipients for click rows only.
type ClickFunnelLakeRow struct {
	Dt         string `json:"dt"`
	CampaignID string `json:"campaign_id"`
	Source     string `json:"source"`
	EventType  string `json:"event_type"`
	Events     int64  `json:"events"`
	Recipients int64  `json:"recipients"`
	Mailboxes  int64  `json:"mailboxes"`
	Classified int64  `json:"classified"`
	Machine    int64  `json:"machine"`
}

// ClickFunnelEventTypes is the closed event-type set this layer reads.
// PRESENT TENSE for engagement — see the silent-zero warning above.
var ClickFunnelEventTypes = []string{
	"delivered", "relayed_to_ses", "hard_bounce", "soft_bounce", "delivery_delay",
	"open", "click", "unsubscribe", "complaint",
}

// maxClickFunnelCampaignIDs bounds the IN-list. Same ceiling and reasoning as
// maxBreakdownCampaignIDs: Athena query strings cap at 256KB and 2000 quoted
// UUIDs is ~78KB.
const maxClickFunnelCampaignIDs = 2000

// BuildClickFunnelSQL renders the day-grain aggregate for a campaign set over
// [fromDt, toDt]. Exported so the regression guards in internal/worker can
// assert on partition filtering and event-type tense without a live reader.
//
// campaignIDs must already be UUID-validated by the caller (ClickFunnelDaily
// does it); every value is rendered through sqlStr regardless.
func BuildClickFunnelSQL(fromDt, toDt string, campaignIDs []string) string {
	quotedTypes := make([]string, 0, len(ClickFunnelEventTypes))
	for _, et := range ClickFunnelEventTypes {
		quotedTypes = append(quotedTypes, sqlStr(et))
	}
	quotedIDs := make([]string, 0, len(campaignIDs))
	for _, id := range campaignIDs {
		quotedIDs = append(quotedIDs, sqlStr(id))
	}

	var b strings.Builder
	b.WriteString("SELECT dt, campaign_id, source, event_type,")
	b.WriteString(" COUNT(DISTINCT event_uid) events,")
	b.WriteString(" COUNT(DISTINCT subscriber_id) recipients,")
	b.WriteString(" COUNT(DISTINCT email) mailboxes,")
	// Classification coverage: how many of this bucket's recipients carry ANY
	// verdict at all. Publishing coverage is what keeps an inert column from
	// masquerading as a human signal.
	b.WriteString(" COUNT(DISTINCT CASE WHEN is_machine_click IS NOT NULL THEN subscriber_id END) classified,")
	b.WriteString(" COUNT(DISTINCT CASE WHEN is_machine_click = true THEN subscriber_id END) machine")
	b.WriteString(" FROM ")
	b.WriteString(lakeTable)
	// dt is the partition column — never dropped.
	b.WriteString(" WHERE dt BETWEEN ")
	b.WriteString(sqlStr(fromDt))
	b.WriteString(" AND ")
	b.WriteString(sqlStr(toDt))
	b.WriteString(" AND campaign_id IN (")
	b.WriteString(strings.Join(quotedIDs, ", "))
	b.WriteString(")")
	b.WriteString(" AND event_type IN (")
	b.WriteString(strings.Join(quotedTypes, ", "))
	b.WriteString(")")
	b.WriteString(" GROUP BY dt, campaign_id, source, event_type")
	return b.String()
}

// ClickFunnelDaily returns day-grain buckets for the given campaign set.
func (r *Reader) ClickFunnelDaily(ctx context.Context, fromDt, toDt string, campaignIDs []string) ([]ClickFunnelLakeRow, error) {
	if err := validateDt("from", fromDt); err != nil {
		return nil, err
	}
	if err := validateDt("to", toDt); err != nil {
		return nil, err
	}
	if len(campaignIDs) == 0 {
		return nil, nil
	}
	if len(campaignIDs) > maxClickFunnelCampaignIDs {
		return nil, fmt.Errorf("click funnel: %d campaign ids exceeds cap %d", len(campaignIDs), maxClickFunnelCampaignIDs)
	}
	clean := make([]string, 0, len(campaignIDs))
	for _, id := range campaignIDs {
		id = strings.TrimSpace(id)
		// Caller text never reaches a column position: anything that is not a
		// UUID is dropped, not escaped-and-passed.
		if !uuidRe.MatchString(id) {
			return nil, fmt.Errorf("click funnel: %q is not a campaign uuid", id)
		}
		clean = append(clean, id)
	}

	_, rows, err := r.runQuery(ctx, BuildClickFunnelSQL(fromDt, toDt, clean))
	if err != nil {
		return nil, err
	}
	out := make([]ClickFunnelLakeRow, 0, len(rows))
	for _, row := range rows {
		if len(row) < 9 {
			continue
		}
		n := func(i int) int64 { v, _ := strconv.ParseInt(strings.TrimSpace(row[i]), 10, 64); return v }
		out = append(out, ClickFunnelLakeRow{
			Dt:         strings.TrimSpace(row[0]),
			CampaignID: strings.TrimSpace(row[1]),
			Source:     strings.ToLower(strings.TrimSpace(row[2])),
			EventType:  strings.ToLower(strings.TrimSpace(row[3])),
			Events:     n(4),
			Recipients: n(5),
			Mailboxes:  n(6),
			Classified: n(7),
			Machine:    n(8),
		})
	}
	return out, nil
}

// ClickFunnelDaily runs against the global reader. Returns errDisabled when the
// reader is not configured (IsDisabledErr reports it).
func ClickFunnelDaily(ctx context.Context, fromDt, toDt string, campaignIDs []string) ([]ClickFunnelLakeRow, error) {
	r := getReader()
	if r == nil {
		return nil, errDisabled
	}
	return r.ClickFunnelDaily(ctx, fromDt, toDt, campaignIDs)
}
