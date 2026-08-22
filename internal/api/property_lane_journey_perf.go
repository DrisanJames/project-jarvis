package api

// Journey canvas — per-touch 7-day performance (perf_7d on each touch node).
//
// SOURCE CHOICE: the drip observatory rollup (internal/worker/
// drip_observatory_rollup.go) DOES maintain touch-grained fact tables
// (partner_drip_send_cohort_daily / partner_drip_event_daily), but they are
// generation-swapped by run_id, refreshed 6-hourly, disable-able via
// DRIP_OBSERVATORY_ROLLUP_DISABLED, and have NO existing API-side reader —
// reading them correctly means resolving the latest published run first.
// A lane's wave campaigns over 7 days are a few hundred small rows on an
// indexed slice, so this reads mailing_campaigns directly instead: ONE query
// per (vertical, brand), short-TTL cached, riding the SAME access path as
// laneJourneyActivitySQL (idx_campaigns_org_status_created; the name regex is
// a filter on the already-narrow recent slice, never the driver). NEVER a
// tracking-events scan.
//
// COUNTING CAVEATS (stated per the metric contract):
//   - sent/delivered/opened/clicked come from the mailing_campaigns
//     DENORMALIZED counters (sent_count/delivered_count/open_count/
//     click_count). opens/clicks UNDERCOUNT SES-routed engagement — SES
//     opens/clicks land in the tracking/lake surfaces and are not reliably
//     folded back into the campaign counters; delivered similarly lags for
//     Microsoft (PG delivered ≈ ingestion rate). Treat perf_7d as a relative
//     per-touch shape, not delivery truth — the lake is delivery truth.
//   - the touch number is parsed from the wave campaign name: deployWaveGroups
//     appends " [tN]" for follow-up touches; a name with NO suffix is the
//     welcome pass = touch 1 (same parsing rule as the observatory rollup and
//     laneJourneyActivitySQL's is_followup regex).

import (
	"context"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// laneJourneyPerfDays is the trailing window the node stats cover.
const laneJourneyPerfDays = 7

// laneJourneyPerfCacheTTL bounds staleness; the canvas polls, and the
// underlying counters move slowly enough that 60s is honest.
const laneJourneyPerfCacheTTL = 60 * time.Second

// laneJourneyPerfSQL — one indexed slice of the lane's wave campaigns over
// the window. Access path copied from laneJourneyActivitySQL (the proven
// shape): driver is idx_campaigns_org_status_created; the name regex only
// filters. $1 org, $2 window start, $3 vertical, $4 brand.
const laneJourneyPerfSQL = `
	SELECT c.name,
	       COALESCE(c.sent_count, 0), COALESCE(c.delivered_count, 0),
	       COALESCE(c.open_count, 0), COALESCE(c.click_count, 0)
	FROM mailing_campaigns c
	WHERE c.organization_id = $1::uuid
	  AND c.status IN ('sending','sent','completed','completed_with_errors')
	  AND c.created_at >= $2
	  AND c.partner_drip_tag IS NOT NULL
	  AND c.name ~ ('^\[partner-drip\] ' || $3 || ' ' || $4 || ' ')`

// laneJourneyPerf is one touch's trailing-7d counters.
type laneJourneyPerf struct {
	Sent      int64 `json:"sent"`
	Delivered int64 `json:"delivered"`
	Opened    int64 `json:"opened"`
	Clicked   int64 `json:"clicked"`
}

var dripTouchSuffixRe = regexp.MustCompile(` \[t([0-9]+)\]$`)

// parseDripTouchFromName returns the touch number a partner-drip wave
// campaign name encodes: the " [tN]" suffix appended by deployWaveGroups for
// follow-up touches, or 1 (the welcome pass) when the suffix is absent.
func parseDripTouchFromName(name string) int {
	m := dripTouchSuffixRe.FindStringSubmatch(name)
	if len(m) != 2 {
		return 1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// laneJourneyPerfCache: per-(vertical|brand) TTL slots, mirroring the
// laneSupplyCacheSlot idiom (misses collapse on the slot mutex). Package
// level because PMTACampaignService's fields live in
// handlers_pmta_campaign.go and the service is a process singleton in prod.
var laneJourneyPerfCache sync.Map // key "vertical|brand" -> *laneJourneyPerfSlot

type laneJourneyPerfSlot struct {
	mu         sync.Mutex
	computedAt time.Time
	byTouch    map[int]laneJourneyPerf
}

// resetLaneJourneyPerfCacheForTest drops every cached slot.
func resetLaneJourneyPerfCacheForTest() {
	laneJourneyPerfCache.Range(func(k, _ interface{}) bool {
		laneJourneyPerfCache.Delete(k)
		return true
	})
}

// laneJourneyPerf7d returns the lane's per-touch trailing-7d counters,
// through the TTL cache. One query on a miss; error on the SINGLE query is
// returned so the caller can degrade (the canvas never fails on an aux read).
func (s *PMTACampaignService) laneJourneyPerf7d(ctx context.Context, orgID, vertical, brand string) (map[int]laneJourneyPerf, error) {
	key := vertical + "|" + brand
	slotI, _ := laneJourneyPerfCache.LoadOrStore(key, &laneJourneyPerfSlot{})
	slot := slotI.(*laneJourneyPerfSlot)
	slot.mu.Lock()
	defer slot.mu.Unlock()

	if slot.byTouch != nil && time.Since(slot.computedAt) < laneJourneyPerfCacheTTL {
		return slot.byTouch, nil
	}

	windowStart := time.Now().AddDate(0, 0, -laneJourneyPerfDays).UTC()
	byTouch := map[int]laneJourneyPerf{}
	err := func() error {
		rows, err := s.db.QueryContext(ctx, laneJourneyPerfSQL, orgID, windowStart, vertical, brand)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var sent, delivered, opened, clicked int64
			if err := rows.Scan(&name, &sent, &delivered, &opened, &clicked); err != nil {
				return err
			}
			t := parseDripTouchFromName(name)
			p := byTouch[t]
			p.Sent += sent
			p.Delivered += delivered
			p.Opened += opened
			p.Clicked += clicked
			byTouch[t] = p
		}
		return rows.Err()
	}()
	if err != nil {
		return nil, err
	}
	slot.byTouch = byTouch
	slot.computedAt = time.Now()
	return byTouch, nil
}

// laneJourneyAttachPerf fills each touch node's Perf7d. Degrades to a named
// gap: on a failed read every node keeps Perf7d nil and the returned error
// string is surfaced as perf_7d_error — a zero row is never substituted for
// a failed read (same rule as the send-activity strip).
func (s *PMTACampaignService) laneJourneyAttachPerf(ctx context.Context, orgID, vertical, brand string, touches []laneJourneyTouch) string {
	byTouch, err := s.laneJourneyPerf7d(ctx, orgID, vertical, brand)
	if err != nil {
		return "perf read failed"
	}
	for i := range touches {
		p := byTouch[touches[i].Touch]
		cp := p
		touches[i].Perf7d = &cp
	}
	return ""
}

const laneJourneyPerfNote = "perf_7d aggregates the lane's wave campaigns over the trailing 7 days from mailing_campaigns' denormalized counters (touch parsed from the ` [tN]` name suffix; no suffix = touch 1). opens/clicks UNDERCOUNT SES-routed engagement and delivered lags for Microsoft — relative per-touch shape, not delivery truth (the lake is delivery truth)."
