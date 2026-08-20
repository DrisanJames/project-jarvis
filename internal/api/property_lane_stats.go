package api

// Property Lane Stats — the drip lane's open/click performance ledger, IN THE
// PRODUCT.
//
// This is a straight port of the two Python reports the operator already
// trusts; it exists so nobody has to ask an agent to run a report to see lane
// performance:
//
//   agents/reporting/lane_performance_ledger.py   — lane × ISP × day, rates on
//       a delivered denominator, day-chunked, campaign ids resolved with
//       created_at LEADING (series()).
//   agents/reporting/drip_lane_isp_report.py      — all_lanes_isp_day(), the
//       canonical per-lane-per-ISP-per-day aggregate whose shape is mirrored
//       verbatim below.
//   agents/reporting/partner_lane_report.py       — pins the counting rules
//       (uniques as well as events; backfill artifacts excluded).
//
// Deliberate parity notes (deviating here makes the product disagree with the
// reports, which is worse than not shipping):
//
//   * event_type is PAST TENSE — 'sent' / 'delivered' / 'opened' / 'clicked' /
//     'bounced'. 'open'/'click' return a SILENT ZERO. Pinned by
//     TestLaneStatsSQLPinsPastTenseAndEventAtBound.
//   * mailing_tracking_events is RANGE-partitioned on event_at; every read
//     carries an event_at range bound or it touches every partition. The
//     bounds are Denver-day instants computed in Go and param-injected — the
//     SQL never tz-casts (`(col AT TIME ZONE 'America/Denver')::date = …` is
//     non-sargable and was measured at 120s on a bare COUNT(*)).
//   * campaigns are resolved with created_at LEADING and the name regex as a
//     RESIDUAL filter (lane_performance_ledger.series() comment: leading with
//     the name regex "cannot use an index and timed the socket out at 852s").
//   * every rate is UNIQUE SUBSCRIBERS ÷ delivered, per the ledger. Raw event
//     counts ride along as open_events/click_events so the scanner shape stays
//     visible (partner_lane_report's "events AND uniques ALWAYS" rule) — they
//     are never the rate numerator.
//   * delivered here is a PG confirmation-INGESTION count, which undercuts
//     Microsoft by ~30% versus the Athena lake. The field is therefore named
//     delivered_pg and the payload carries source + delivered_note; it must
//     never be read as per-ISP delivery truth.
//   * a day whose query fails is reported in missing_days, never as zeros —
//     "a lost day is visible as a gap, never a silent zero" (series()).
//
// Indexes this rides (verified against prod pg_indexes 2026-08-19):
//   * mailing_campaigns   idx_campaigns_org_status_created
//                         (organization_id, status, created_at DESC)
//     EXPLAIN ANALYZE on the live table: Index Scan, Index Cond on
//     (organization_id, created_at), name regex as Filter — 139 ms for an
//     11-day resolve returning 1,272 campaigns.
//   * mailing_tracking_events  idx_tracking_campaign (campaign_id, event_at DESC)
//     — per-partition child index (…_2026_08_campaign_id_event_at_idx). The
//     unnest JOIN (NOT `= ANY`) is what keeps the planner on per-campaign
//     index lookups instead of seq-scanning the month partition.
//   * partner_clean_queue  idx_pcq_subscriber_id (subscriber_id)
//     WHERE subscriber_id IS NOT NULL — the ISP resolution CTE.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

// ── tuning ──────────────────────────────────────────────────────────────────

const (
	laneStatsDefaultDays = 7
	laneStatsMinDays     = 1
	laneStatsMaxDays     = 30

	// laneStatsCampaignCushionDays mirrors lane_performance_ledger.series():
	// `since = end_date - (days + 4)`. The reminder ladder is ~72h, so a
	// campaign created a few days before the window can still emit events
	// inside it; four days is the ledger's cushion and changing it changes
	// the numbers.
	laneStatsCampaignCushionDays = 4

	// laneStatsConcurrency bounds the day-chunk fan-out. The days are
	// independent queries; 4 in flight keeps wall-clock near one round-trip
	// per ceil(days/4) without stacking scans on a loaded RDS.
	laneStatsConcurrency = 4

	// laneStatsBudget is the whole-request wall-clock ceiling. This is a UI
	// endpoint: it returns what it has when the budget expires and names the
	// days it could not compute in missing_days, rather than hanging. It sits
	// under the prod 30s statement_timeout on purpose.
	laneStatsBudget = 25 * time.Second

	// TTLs: today's Denver day is still accumulating events; a closed day only
	// changes through late-arriving event_at rows, which are rare.
	laneStatsTodayTTL  = 5 * time.Minute
	laneStatsClosedTTL = 60 * time.Minute

	// laneStatsCacheMaxAge / laneStatsSweepEvery bound the cache: day keys
	// accumulate forever otherwise.
	laneStatsCacheMaxAge = 2 * time.Hour
	laneStatsSweepEvery  = 10 * time.Minute

	// The campaign set for a window changes only as new campaigns are created;
	// a few minutes of drift moves a rate in the 4th decimal. Kept short enough
	// that a lane deployed mid-session appears without a restart.
	laneStatsCampaignTTL = 10 * time.Minute

	laneStatsSource = "pg_tracking_events"
	laneStatsNote   = "delivered_pg is a PG confirmation-INGESTION count and undercounts Microsoft by ~30%; per-ISP delivery TRUTH is the Athena lake. Rates are unique subscribers / delivered_pg (lane_performance_ledger)."
)

// ── SQL ─────────────────────────────────────────────────────────────────────

// laneStatsCampaignSQL resolves the lane's campaign ids.
//
// created_at LEADS (idx_campaigns_org_status_created); the anchored name regex
// is a residual Filter. The anchor + trailing space make the vertical match
// EXACT, which is drip_lane_isp_report.lane_campaigns' semantics — its module
// docstring calls out that an unanchored pattern also matches `_v3` siblings.
//
// $1 organization_id · $2 created_at floor · $3 the anchored name regex.
const laneStatsCampaignSQL = `
	SELECT id::text, created_at
	FROM mailing_campaigns
	WHERE organization_id = $1::uuid
	  AND created_at >= $2
	  AND name ~ $3
	ORDER BY created_at`

// laneStatsDaySQL is drip_lane_isp_report.all_lanes_isp_day() for ONE lane and
// ONE Denver day, mirrored statement-for-statement.
//
// Two-level aggregation is load-bearing: the per-subscriber level uses
// bool_or/COUNT (hash-aggregable), the outer level counts plain booleans.
// COUNT(DISTINCT …) at the outer level forces a sort-based aggregate over the
// planner's (badly overestimated) join cardinality, and that spilling sort is
// what blew the statement budget in the Python before this shape.
//
// The NOT(… LIKE '%(undefined status)%' AND created_at > event_at + 1 hour)
// clause is partner_lane_report._artifact_pred — it drops backfilled
// placeholder bounce rows. (The Python has to write `%%` there because psycopg2
// treats `%` as a placeholder; lib/pq does not, so the literal is single-`%`.)
//
// $1 campaign ids (uuid[]) · $2 window start (UTC) · $3 window end (UTC).
const laneStatsDaySQL = `
	WITH cmap AS (
	  SELECT unnest($1::uuid[]) AS cid
	),
	per_sub AS (
	  SELECT m.subscriber_id,
	         bool_or(m.event_type = 'sent')      AS sent,
	         bool_or(m.event_type = 'delivered') AS delivered,
	         bool_or(m.event_type = 'opened')    AS opened,
	         bool_or(m.event_type = 'clicked')   AS clicked,
	         bool_or(m.event_type = 'clicked' AND m.click_verdict = 'human') AS human,
	         COUNT(*) FILTER (WHERE m.event_type = 'opened')  AS n_open,
	         COUNT(*) FILTER (WHERE m.event_type = 'clicked') AS n_click
	  FROM cmap c
	  JOIN mailing_tracking_events m ON m.campaign_id = c.cid
	  WHERE m.event_at >= $2 AND m.event_at < $3
	    AND m.subscriber_id IS NOT NULL
	    AND NOT (COALESCE(m.bounce_reason, '') LIKE '%(undefined status)%'
	             AND m.created_at > m.event_at + INTERVAL '1 hour')
	  GROUP BY 1
	),
	q AS (
	  SELECT DISTINCT ON (subscriber_id) subscriber_id, isp_family
	  FROM partner_clean_queue
	  WHERE subscriber_id IS NOT NULL
	    AND subscriber_id IN (SELECT DISTINCT subscriber_id FROM per_sub)
	)
	SELECT COALESCE(NULLIF(q.isp_family, ''), 'other')  AS isp,
	       COUNT(*) FILTER (WHERE p.sent)               AS sent,
	       COUNT(*) FILTER (WHERE p.delivered)          AS delivered_pg,
	       COUNT(*) FILTER (WHERE p.opened)             AS openers,
	       COUNT(*) FILTER (WHERE p.clicked)            AS clickers,
	       COUNT(*) FILTER (WHERE p.human)              AS human_clickers,
	       COALESCE(SUM(p.n_open), 0)::bigint           AS open_events,
	       COALESCE(SUM(p.n_click), 0)::bigint          AS click_events
	FROM per_sub p
	LEFT JOIN q ON q.subscriber_id = p.subscriber_id
	GROUP BY 1
	ORDER BY 2 DESC`

// laneStatsRollupReadSQL is the READ-THROUGH: the precomputed cells for this
// lane's window, written by worker.LaneStatsRollupWorker
// (internal/worker/lane_stats_rollup.go) using a COPY of laneStatsDaySQL —
// this file stays the source of truth for the counting rules.
//
// WHY: a cold 7-day live pass measured ~230-255s on prod with ~10x run-to-run
// variance, so a poller converged over three 25s polls instead of answering.
// This is a PK-prefix range scan on
// (organization_id, vertical, brand, day, isp) — the whole window in one cheap
// read.
//
// SAFETY POSTURE — this is a pure ACCELERATOR, never a source of truth:
//   - every day the table does not cover is computed by the untouched live
//     path, so an empty table (or a worker that never ran) is behaviourally
//     today's endpoint;
//   - a READ FAILURE here (missing table on a binary that shipped ahead of its
//     DDL, timeout, anything) is logged and treated as "no rollup days" — it
//     can never fail the request or zero a number;
//   - freshness is enforced HERE, not trusted from the writer:
//     laneStatsRollupUsable rejects a closed-day row computed before the day
//     closed (it would be partial) and a today row older than the endpoint's
//     own today TTL. A stale rollup degrades to the live path.
//
// $1 organization_id · $2 vertical · $3 brand · $4 first day · $5 last day.
const laneStatsRollupReadSQL = `
	SELECT to_char(day, 'YYYY-MM-DD'), isp, sent, delivered_pg, openers,
	       clickers, human_clickers, open_events, click_events, computed_at
	FROM mailing_lane_stats_daily
	WHERE organization_id = $1::uuid
	  AND vertical = $2 AND brand = $3
	  AND day >= $4::date AND day <= $5::date`

// laneStatsRollupEmptyISP is the writer's "computed, no cells" sentinel — it
// is how the table distinguishes an empty day from a never-computed one. It is
// filtered out on read and NEVER reaches the payload. Kept identical to
// worker.LaneStatsRollupEmptyISP.
const laneStatsRollupEmptyISP = "__none__"

// ── payload ─────────────────────────────────────────────────────────────────

// laneStatsCell is one ISP's counts for one Denver day.
type laneStatsCell struct {
	ISP           string `json:"isp"`
	Sent          int64  `json:"sent"`
	DeliveredPG   int64  `json:"delivered_pg"`
	Openers       int64  `json:"opens"`
	Clickers      int64  `json:"clicks"`
	HumanClickers int64  `json:"human_clicks"`
	OpenEvents    int64  `json:"open_events"`
	ClickEvents   int64  `json:"click_events"`
}

// laneStatsRow is a cell stamped with its day and its derived rates — one row
// per (day, ISP), which is what the UI diffs for day-over-day trend. Trend
// ARROWS are deliberately not precomputed: the UI owns that presentation.
type laneStatsRow struct {
	Day string `json:"day"`
	laneStatsCell
	OpenRate  float64 `json:"open_rate"`
	ClickRate float64 `json:"click_rate"`
}

type laneStatsTotals struct {
	Sent          int64   `json:"sent"`
	DeliveredPG   int64   `json:"delivered_pg"`
	Openers       int64   `json:"opens"`
	Clickers      int64   `json:"clicks"`
	HumanClickers int64   `json:"human_clicks"`
	OpenEvents    int64   `json:"open_events"`
	ClickEvents   int64   `json:"click_events"`
	OpenRate      float64 `json:"open_rate"`
	ClickRate     float64 `json:"click_rate"`
}

type laneStatsResponse struct {
	Vertical    string          `json:"vertical"`
	Brand       string          `json:"brand,omitempty"`
	Days        int             `json:"days"`
	DayList     []string        `json:"day_list"`
	Rows        []laneStatsRow  `json:"rows"`
	Totals      laneStatsTotals `json:"totals"`
	MissingDays []string        `json:"missing_days"`
	Partial     bool            `json:"partial"`
	// RollupDays / LiveDays name the PROVENANCE of every resolved day, so a
	// stale or half-filled rollup is visible on the payload instead of silently
	// changing what the operator is looking at. Invariant:
	// rollup_days ∪ live_days ∪ missing_days == day_list, disjoint.
	// With an empty mailing_lane_stats_daily, rollup_days is [] and live_days
	// is the whole window — i.e. today's behaviour, labelled.
	RollupDays []string `json:"rollup_days"`
	LiveDays   []string `json:"live_days"`
	// RollupOldestComputedAt is the oldest computed_at among the rollup-served
	// days (RFC3339, empty when none) — the one number that says "how stale is
	// the fast path".
	RollupOldestComputedAt string `json:"rollup_oldest_computed_at,omitempty"`
	Campaigns              int    `json:"campaigns"`
	// CampaignsStale: the campaign resolve failed and a PREVIOUS list was served
	// rather than 500-ing. Rates are computed over a slightly older campaign set.
	CampaignsStale bool   `json:"campaigns_stale,omitempty"`
	Source         string `json:"source"`
	DeliveredNote  string `json:"delivered_note"`
	GeneratedAt    string `json:"generated_at"`
}

// ── helpers ─────────────────────────────────────────────────────────────────

// laneStatsRate is the ONLY place a rate is computed. A zero denominator
// yields 0.0 — never NaN, never +Inf, never a panic.
func laneStatsRate(num, den int64) float64 {
	if den <= 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// laneStatsClampDays: absent/garbage → 7; below 1 → 1; above 30 → 30.
func laneStatsClampDays(raw string) int {
	if strings.TrimSpace(raw) == "" {
		return laneStatsDefaultDays
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return laneStatsDefaultDays
	}
	if n < laneStatsMinDays {
		return laneStatsMinDays
	}
	if n > laneStatsMaxDays {
		return laneStatsMaxDays
	}
	return n
}

// laneStatsSlug validates a vertical/brand token. Both are fed into a Postgres
// regex, so anything outside [a-z0-9_-] is rejected rather than escaped — no
// regex injection surface, and every real vertical/brand in
// partner_drip_vertical_roster already matches.
var laneStatsSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func laneStatsSlug(raw string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return "", false
	}
	return v, laneStatsSlugRe.MatchString(v)
}

// laneStatsNamePattern builds the anchored campaign-name regex. Campaign names
// are `[partner-drip] <vertical> <brand> <stamp> …` — anchoring plus the
// trailing space is what makes the vertical match EXACT.
func laneStatsNamePattern(vertical, brand string) string {
	if brand != "" {
		return `^\[partner-drip\] ` + vertical + ` ` + brand + ` `
	}
	return `^\[partner-drip\] ` + vertical + ` `
}

// laneStatsDayBoundsUTC returns [start, end) of a Denver calendar day as UTC
// instants. DST-correct by construction (23h/24h/25h days) because it adds a
// calendar day in the Denver location, never 24 hours.
func laneStatsDayBoundsUTC(day time.Time) (time.Time, time.Time) {
	l := day.In(propertyLedgerLoc)
	start := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, propertyLedgerLoc)
	return start.UTC(), start.AddDate(0, 0, 1).UTC()
}

// laneStatsDayList returns the Denver days ending today, oldest first.
func laneStatsDayList(now time.Time, days int) []string {
	l := now.In(propertyLedgerLoc)
	end := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, propertyLedgerLoc)
	out := make([]string, 0, days)
	for i := days - 1; i >= 0; i-- {
		out = append(out, end.AddDate(0, 0, -i).Format("2006-01-02"))
	}
	return out
}

// ── per-day cache ───────────────────────────────────────────────────────────
//
// Same posture as laneSupplyCache (property_lane_supply.go): a per-key mutex so
// concurrent pollers for the same day collapse onto one scan. It lives at
// package scope rather than on PMTACampaignService because the service struct
// is owned by handlers_pmta_campaign.go; the org id is part of every key, so
// the cache is org-safe.

type laneStatsCacheSlot struct {
	mu         sync.Mutex
	computedAt time.Time
	cells      []laneStatsCell
}

var (
	laneStatsCache     sync.Map // key -> *laneStatsCacheSlot
	laneStatsSweepMu   sync.Mutex
	laneStatsSweptAt   time.Time
	laneStatsCacheKeyF = "%s|%s|%s|%s" // org | vertical | brand | day
)

func laneStatsCacheKey(org, vertical, brand, day string) string {
	return fmt.Sprintf(laneStatsCacheKeyF, org, vertical, brand, day)
}

// laneStatsSweepCache drops slots older than laneStatsCacheMaxAge. Day keys
// accumulate forever otherwise (a new key every Denver midnight, per lane).
func laneStatsSweepCache(now time.Time) {
	laneStatsSweepMu.Lock()
	if !laneStatsSweptAt.IsZero() && now.Sub(laneStatsSweptAt) < laneStatsSweepEvery {
		laneStatsSweepMu.Unlock()
		return
	}
	laneStatsSweptAt = now
	laneStatsSweepMu.Unlock()

	laneStatsCache.Range(func(k, v interface{}) bool {
		slot, ok := v.(*laneStatsCacheSlot)
		if !ok {
			laneStatsCache.Delete(k)
			return true
		}
		slot.mu.Lock()
		stale := !slot.computedAt.IsZero() && now.Sub(slot.computedAt) > laneStatsCacheMaxAge
		slot.mu.Unlock()
		if stale {
			laneStatsCache.Delete(k)
		}
		return true
	})
}

func laneStatsTTLFor(day, today string) time.Duration {
	if day == today {
		return laneStatsTodayTTL
	}
	return laneStatsClosedTTL
}

// ── campaign-resolution cache ───────────────────────────────────────────────
//
// WHY: campaign resolution is a SINGLE query that must finish before any day
// can be scanned, and it was originally uncached — so it competed with the day
// scans for the same 25s budget. Measured on prod 2026-08-19 immediately after
// deploy: the first call resolved 922 campaigns and returned, then three
// consecutive calls ALL failed with
//
//	campaign resolution failed: pq: canceling statement due to user request
//
// i.e. the resolve alone exhausted the budget under RDS I/O contention. An
// endpoint whose whole purpose is to end manual stat-pulling cannot 500 on most
// calls.
//
// Two changes: memoize the resolve, and — when the refresh fails but a previous
// answer exists — SERVE THE STALE LIST and say so, rather than 500. A campaign
// list minutes out of date changes a rate in the 4th decimal; a 500 gives the
// operator nothing. Staleness is surfaced as campaigns_stale so the screen can
// label it; it is never silently passed off as fresh.
type laneStatsCampaign struct {
	id      string
	created time.Time
}

type laneStatsCampaignSlot struct {
	mu         sync.Mutex
	computedAt time.Time
	campaigns  []laneStatsCampaign
}

var laneStatsCampaignCache sync.Map // key -> *laneStatsCampaignSlot

// laneStatsCampaigns returns the resolved campaign list, whether it was served
// stale, and any error. It only errors when there is NO usable prior answer.
func (s *PMTACampaignService) laneStatsCampaigns(ctx context.Context, key string,
	orgID string, floor time.Time, pattern string) ([]laneStatsCampaign, bool, error) {

	slotI, _ := laneStatsCampaignCache.LoadOrStore(key, &laneStatsCampaignSlot{})
	slot := slotI.(*laneStatsCampaignSlot)
	slot.mu.Lock()
	defer slot.mu.Unlock()

	if !slot.computedAt.IsZero() && time.Since(slot.computedAt) < laneStatsCampaignTTL {
		return append([]laneStatsCampaign{}, slot.campaigns...), false, nil
	}

	out := []laneStatsCampaign{}
	err := func() error {
		rows, qerr := s.db.QueryContext(ctx, laneStatsCampaignSQL, orgID, floor, pattern)
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			var c laneStatsCampaign
			if serr := rows.Scan(&c.id, &c.created); serr != nil {
				return serr
			}
			out = append(out, c)
		}
		return rows.Err()
	}()
	if err != nil {
		// Stale-serve: a previous answer beats a 500. Only fail when we have none.
		if !slot.computedAt.IsZero() {
			return append([]laneStatsCampaign{}, slot.campaigns...), true, nil
		}
		return nil, false, err
	}
	slot.campaigns = out
	slot.computedAt = time.Now()
	return append([]laneStatsCampaign{}, out...), false, nil
}

// ── query ───────────────────────────────────────────────────────────────────

// laneStatsQueryDay runs laneStatsDaySQL for one day. Callers hand it only the
// campaign ids that existed before the day ended — a campaign cannot emit an
// event before it is created, so that prune is exactly equivalent and keeps the
// oldest days in the window cheap.
func (s *PMTACampaignService) laneStatsQueryDay(ctx context.Context, cids []string, start, end time.Time) ([]laneStatsCell, error) {
	if len(cids) == 0 {
		return []laneStatsCell{}, nil
	}
	rows, err := s.db.QueryContext(ctx, laneStatsDaySQL, pq.Array(cids), start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []laneStatsCell{}
	for rows.Next() {
		var c laneStatsCell
		if err := rows.Scan(&c.ISP, &c.Sent, &c.DeliveredPG, &c.Openers, &c.Clickers,
			&c.HumanClickers, &c.OpenEvents, &c.ClickEvents); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// laneStatsRollupDay carries one precomputed day out of the table.
type laneStatsRollupDay struct {
	cells      []laneStatsCell
	computedAt time.Time
	campaigns  int
}

// laneStatsReadRollup loads whatever the rollup table holds for this lane's
// window. It NEVER returns an error to the caller: a failure (table absent,
// timeout, scan error) is logged and answers "nothing precomputed", which puts
// every day back on the live path — the degradation property.
func (s *PMTACampaignService) laneStatsReadRollup(ctx context.Context,
	orgID, vertical, brand string, dayList []string) map[string]laneStatsRollupDay {

	out := map[string]laneStatsRollupDay{}
	if len(dayList) == 0 {
		return out
	}
	rows, err := s.db.QueryContext(ctx, laneStatsRollupReadSQL,
		orgID, vertical, brand, dayList[0], dayList[len(dayList)-1])
	if err != nil {
		log.Printf("[LaneStats] rollup read unavailable (%v) — every day falls back to the live path", err)
		return map[string]laneStatsRollupDay{}
	}
	defer rows.Close()
	for rows.Next() {
		var day string
		var c laneStatsCell
		var at time.Time
		if serr := rows.Scan(&day, &c.ISP, &c.Sent, &c.DeliveredPG, &c.Openers,
			&c.Clickers, &c.HumanClickers, &c.OpenEvents, &c.ClickEvents, &at); serr != nil {
			log.Printf("[LaneStats] rollup scan failed (%v) — falling back to the live path", serr)
			return map[string]laneStatsRollupDay{}
		}
		d := out[day]
		// The writer's freshness for a day is the OLDEST row in it: a newer
		// ISP appearing later must not make an older cell look fresh.
		if d.computedAt.IsZero() || at.Before(d.computedAt) {
			d.computedAt = at
		}
		// The sentinel marks "computed, no cells" — it establishes the day, it
		// is never a cell.
		if c.ISP != laneStatsRollupEmptyISP {
			d.cells = append(d.cells, c)
		}
		out[day] = d
	}
	if err := rows.Err(); err != nil {
		log.Printf("[LaneStats] rollup read incomplete (%v) — falling back to the live path", err)
		return map[string]laneStatsRollupDay{}
	}
	return out
}

// laneStatsRollupUsable decides whether a precomputed day may be served.
// Freshness is enforced on READ so a wedged or half-finished writer degrades to
// the live path instead of quietly changing the numbers.
//
//   - TODAY is usable only while it is younger than the endpoint's own today
//     TTL — the same freshness contract the live path already offers.
//   - a CLOSED day is usable only if it was computed AT OR AFTER that day's
//     Denver end instant. A row written while the day was still open is a
//     PARTIAL day and must not be served as a complete one.
func laneStatsRollupUsable(d laneStatsRollupDay, isToday bool, dayEnd, now time.Time) bool {
	if d.computedAt.IsZero() {
		return false
	}
	if isToday {
		return now.Sub(d.computedAt) < laneStatsTodayTTL
	}
	return !d.computedAt.Before(dayEnd)
}

// laneStatsDay serves one day through the TTL cache, scanning only on a
// cold/expired slot (the scan runs while HOLDING the slot mutex so concurrent
// pollers for the same day collapse to one scan per TTL).
func (s *PMTACampaignService) laneStatsDay(ctx context.Context, key string, ttl time.Duration,
	cids []string, start, end time.Time) ([]laneStatsCell, error) {

	slotI, _ := laneStatsCache.LoadOrStore(key, &laneStatsCacheSlot{})
	slot := slotI.(*laneStatsCacheSlot)
	slot.mu.Lock()
	defer slot.mu.Unlock()

	if !slot.computedAt.IsZero() && time.Since(slot.computedAt) < ttl {
		return append([]laneStatsCell{}, slot.cells...), nil
	}
	cells, err := s.laneStatsQueryDay(ctx, cids, start, end)
	if err != nil {
		// Never cache a failure — a lost day must be reported as a GAP and
		// retried, not frozen into the cache as zeros.
		return nil, err
	}
	slot.cells = cells
	slot.computedAt = time.Now()
	return append([]laneStatsCell{}, cells...), nil
}

// ── handler ─────────────────────────────────────────────────────────────────

// HandleLaneStats GET …/property-ledger/lane-stats?vertical=<v>[&brand=<b>][&days=7]
//
// Route registration is the tech lead's (RegisterRoutes in
// handlers_pmta_campaign.go); this file only supplies the handler.
func (s *PMTACampaignService) HandleLaneStats(w http.ResponseWriter, r *http.Request) {
	vertical, ok := laneStatsSlug(r.URL.Query().Get("vertical"))
	if !ok {
		respondError(w, http.StatusBadRequest,
			"vertical is required (lowercase [a-z0-9_-], e.g. internal_auto_insurance)")
		return
	}
	brand := ""
	if raw := strings.TrimSpace(r.URL.Query().Get("brand")); raw != "" {
		b, bok := laneStatsSlug(raw)
		if !bok {
			respondError(w, http.StatusBadRequest, "brand must be a lowercase [a-z0-9_-] brand code")
			return
		}
		brand = b
	}
	days := laneStatsClampDays(r.URL.Query().Get("days"))
	orgID := getOrgID(r)

	now := time.Now()
	laneStatsSweepCache(now)

	ctx, cancel := context.WithTimeout(r.Context(), laneStatsBudget)
	defer cancel()

	dayList := laneStatsDayList(now, days)
	today := now.In(propertyLedgerLoc).Format("2006-01-02")

	// Campaign resolution: created_at floor = oldest day in the window minus
	// the ledger's 4-day ladder cushion (series()).
	oldestStart, _ := laneStatsDayBoundsUTC(now.In(propertyLedgerLoc).AddDate(0, 0, -(days - 1)))
	floor := oldestStart.AddDate(0, 0, -laneStatsCampaignCushionDays)

	campaigns, campaignsStale, err := s.laneStatsCampaigns(ctx,
		fmt.Sprintf("%s|%s|%s|%s|%d", orgID, vertical, brand, floor.Format("2006-01-02"), days),
		orgID, floor, laneStatsNamePattern(vertical, brand))
	if err != nil {
		respondError(w, http.StatusInternalServerError, "campaign resolution failed: "+err.Error())
		return
	}

	// READ-THROUGH: whatever the daily rollup already holds for this window.
	// Never fatal — a failure here yields an empty map and every day goes back
	// on the live path below, which is exactly today's behaviour.
	rollup := s.laneStatsReadRollup(ctx, orgID, vertical, brand, dayList)

	// Per-day fan-out, bounded. Each day gets only the campaigns that existed
	// before it ended (exact: no events precede their campaign row).
	// A day the rollup can serve is filled in place and NEVER queried — that is
	// the whole latency win, and it is what the tests assert via unmet/
	// unexpected sqlmock expectations.
	type dayResult struct {
		idx    int
		cells  []laneStatsCell
		err    error
		rolled bool
	}
	results := make([]dayResult, len(dayList))
	rollupOldest := time.Time{}
	sem := make(chan struct{}, laneStatsConcurrency)
	var wg sync.WaitGroup

	for i, day := range dayList {
		d, perr := time.ParseInLocation("2006-01-02", day, propertyLedgerLoc)
		if perr != nil {
			results[i] = dayResult{idx: i, err: perr}
			continue
		}
		start, end := laneStatsDayBoundsUTC(d)

		if rd, ok := rollup[day]; ok && laneStatsRollupUsable(rd, day == today, end, now) {
			results[i] = dayResult{idx: i, cells: rd.cells, rolled: true}
			if rollupOldest.IsZero() || rd.computedAt.Before(rollupOldest) {
				rollupOldest = rd.computedAt
			}
			continue
		}

		cids := make([]string, 0, len(campaigns))
		for _, c := range campaigns {
			if c.created.Before(end) {
				cids = append(cids, c.id)
			}
		}
		key := laneStatsCacheKey(orgID, vertical, brand, day)
		ttl := laneStatsTTLFor(day, today)

		wg.Add(1)
		go func(i int, key string, ttl time.Duration, cids []string, start, end time.Time) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				results[i] = dayResult{idx: i, err: ctx.Err()}
				return
			}
			cells, derr := s.laneStatsDay(ctx, key, ttl, cids, start, end)
			results[i] = dayResult{idx: i, cells: cells, err: derr}
		}(i, key, ttl, cids, start, end)
	}
	wg.Wait()

	// Assemble. A failed day is a GAP (named in missing_days), never zeros.
	byDay := map[string][]laneStatsCell{}
	missing := []string{}
	rollupDays := []string{}
	liveDays := []string{}
	ispTotalSent := map[string]int64{}
	ispSeen := map[string]bool{}
	for i, day := range dayList {
		if results[i].err != nil {
			missing = append(missing, day)
			continue
		}
		if results[i].rolled {
			rollupDays = append(rollupDays, day)
		} else {
			liveDays = append(liveDays, day)
		}
		byDay[day] = results[i].cells
		for _, c := range results[i].cells {
			ispSeen[c.ISP] = true
			ispTotalSent[c.ISP] += c.Sent
		}
	}

	isps := make([]string, 0, len(ispSeen))
	for k := range ispSeen {
		isps = append(isps, k)
	}
	// Busiest ISP first (lane_performance_ledger.build sorts by window volume);
	// name breaks ties so the order is deterministic.
	sort.Slice(isps, func(a, b int) bool {
		if ispTotalSent[isps[a]] != ispTotalSent[isps[b]] {
			return ispTotalSent[isps[a]] > ispTotalSent[isps[b]]
		}
		return isps[a] < isps[b]
	})

	// Dense grid over the days that RESOLVED: every ISP gets a cell on every
	// resolved day, so the UI can diff consecutive days without gap-filling.
	rowsOut := []laneStatsRow{}
	var t laneStatsTotals
	for _, isp := range isps {
		for _, day := range dayList {
			cells, resolved := byDay[day]
			if !resolved {
				continue
			}
			cell := laneStatsCell{ISP: isp}
			for _, c := range cells {
				if c.ISP == isp {
					cell = c
					break
				}
			}
			rowsOut = append(rowsOut, laneStatsRow{
				Day:           day,
				laneStatsCell: cell,
				OpenRate:      laneStatsRate(cell.Openers, cell.DeliveredPG),
				ClickRate:     laneStatsRate(cell.Clickers, cell.DeliveredPG),
			})
			t.Sent += cell.Sent
			t.DeliveredPG += cell.DeliveredPG
			t.Openers += cell.Openers
			t.Clickers += cell.Clickers
			t.HumanClickers += cell.HumanClickers
			t.OpenEvents += cell.OpenEvents
			t.ClickEvents += cell.ClickEvents
		}
	}
	t.OpenRate = laneStatsRate(t.Openers, t.DeliveredPG)
	t.ClickRate = laneStatsRate(t.Clickers, t.DeliveredPG)

	respondJSON(w, http.StatusOK, laneStatsResponse{
		Vertical:    vertical,
		Brand:       brand,
		Days:        days,
		DayList:     dayList,
		Rows:        rowsOut,
		Totals:      t,
		MissingDays: missing,
		Partial:     len(missing) > 0,
		RollupDays:  rollupDays,
		LiveDays:    liveDays,
		RollupOldestComputedAt: func() string {
			if rollupOldest.IsZero() {
				return ""
			}
			return rollupOldest.UTC().Format(time.RFC3339)
		}(),
		Campaigns:      len(campaigns),
		CampaignsStale: campaignsStale,
		Source:         laneStatsSource,
		DeliveredNote:  laneStatsNote,
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
	})
}
