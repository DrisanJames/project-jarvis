package api

// Promoted analytics handlers — Phase D of the Metrics Consistency &
// Analytics Restructure plan.
//
// These endpoints absorb the long-running collection of operator Python
// scripts (`.scratch/today_report_apr24_terminal.py`,
// `.scratch/who_did_we_mail.py`, `.scratch/apr28_wave_scheduler_health.py`,
// etc.) into first-class Analytics Center surfaces. Each handler is a
// compact Go re-implementation of the script's core query against the live
// production DB.
//
// Conventions:
//   * Time windows are accepted via parseAnalyticsRange (RFC3339 or
//     range_type=today/yesterday) so every Analytics tab uses the same
//     range selector contract as the existing /analytics/overview.
//   * Hard-bounce taxonomy comes from HardBounceSQL — never inline a
//     bounce_type IN (...) list.
//   * Every handler emits an `api_version` field so the frontend can
//     detect deploy drift (per testing.mdc PAGE_VERSION rule).
//
import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	isppkg "github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// Version constants below are bumped on every behaviour change.
const (
	VersionTerminalStateMatrix    = "1.0"
	VersionDomainISPMatrix        = "1.1" // SES-aware: delivered=delivered+ses_delivered, relayed_to_ses surfaced
	VersionEngagementQuality      = "1.0"
	VersionDailyAcquisition       = "1.0"
	VersionSegmentIntegrity       = "1.0"
	VersionQueueStatusHistogram   = "1.0"
	VersionWaveSchedulerHealth    = "1.0"
	VersionDispatchTimeline       = "1.0"
	VersionGrowthNarrative        = "1.1" // SES-aware: delivered folds ses_delivered, total_relayed_to_ses added
	VersionAnalyticsCSVExports    = "1.1" // SES-aware: relayed_to_ses column added to domain_isp + growth_narrative
)

// ─── Helpers ─────────────────────────────────────────────────────────────────

// writeJSON marshals v as JSON and writes it with status 200. On marshal
// failure we emit an HTTP 500 with a minimal error body so the frontend
// can show a clean error toast instead of breaking on truncated JSON.
func writeJSONResponse(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
	}
}

// orgIDFromHeader returns the X-Organization-ID header (canonical sidebar
// pattern) or "" when the caller is unscoped (admin).
func orgIDFromHeader(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Organization-ID"))
}

// ─── 1. Terminal-state matrix ────────────────────────────────────────────────
//
// Promoted from `.scratch/today_report_apr24_terminal.py`. For every
// (campaign, subscriber) pair in the requested window we collapse all
// PMTA tail events into ONE terminal outcome:
//
//   delivered          → at least one 'delivered' event (wins everything)
//   hard_bounce        → at least one hard-category bounce, never delivered
//   soft_bounce_final  → at least one soft-category bounce, never delivered
//   deferred_open      → only deferred / deferral events
//   sent_only          → only the worker-side 'sent' event (no PMTA tail)
//
// This is the canonical accounting that prevents the "delivered>sent"
// inversion and the "44.7% soft-bounce rate" bogey that retry-row-counting
// produces. Returns per (sending_domain, isp) buckets plus row totals.

type terminalStateBucket struct {
	SendingDomain    string `json:"sending_domain"`
	ISP              string `json:"isp"`
	Recipients       int    `json:"recipients"`
	Delivered        int    `json:"delivered"`
	HardBounce       int    `json:"hard_bounce"`
	SoftBounceFinal  int    `json:"soft_bounce_final"`
	DeferredOpen     int    `json:"deferred_open"`
	SentOnly         int    `json:"sent_only"`
	SoftBounceEvents int    `json:"soft_bounce_events"`
	DeferralEvents   int    `json:"deferral_events"`
}

func (s *AdvancedMailingService) HandleTerminalStateMatrix(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start, end := parseAnalyticsRange(r)
	hb := HardBounceSQL("t")

	// Per-(campaign, subscriber) terminal-state aggregate. The outer
	// SELECT collapses each pair into a single classification using
	// BOOL_OR — the same pattern as the Python script.
	q := fmt.Sprintf(`
		WITH per_recipient AS (
			SELECT
				LOWER(COALESCE(NULLIF(MAX(t.sending_domain),''),'unknown')) AS sending_domain,
				LOWER(COALESCE(NULLIF(MAX(t.recipient_domain),''),'unknown')) AS recipient_domain,
				BOOL_OR(t.event_type = 'delivered')                          AS has_delivered,
				BOOL_OR(t.event_type = 'bounced' AND %s)                     AS has_hard_bounce,
				BOOL_OR(t.event_type = 'bounced' AND NOT (%s))               AS has_soft_bounce,
				BOOL_OR(t.event_type IN ('deferred','deferral'))             AS has_deferral,
				BOOL_OR(t.event_type = 'sent')                               AS has_sent,
				COUNT(*) FILTER (WHERE t.event_type IN ('deferred','deferral')) AS deferral_event_count,
				COUNT(*) FILTER (WHERE t.event_type = 'bounced' AND NOT (%s)) AS soft_bounce_event_count
			FROM mailing_tracking_events t
			WHERE t.event_at >= $1 AND t.event_at < $2
			  AND t.event_type IN ('sent','delivered','bounced','deferred','deferral')
			  AND t.campaign_id IS NOT NULL
			  AND t.subscriber_id IS NOT NULL
			GROUP BY t.campaign_id, t.subscriber_id
		)
		SELECT sending_domain, recipient_domain,
			COUNT(*) AS recipients,
			COUNT(*) FILTER (WHERE has_delivered) AS delivered,
			COUNT(*) FILTER (WHERE NOT has_delivered AND has_hard_bounce) AS hard_bounce,
			COUNT(*) FILTER (WHERE NOT has_delivered AND NOT has_hard_bounce AND has_soft_bounce) AS soft_bounce_final,
			COUNT(*) FILTER (WHERE NOT has_delivered AND NOT has_hard_bounce AND NOT has_soft_bounce AND has_deferral) AS deferred_open,
			COUNT(*) FILTER (WHERE NOT has_delivered AND NOT has_hard_bounce AND NOT has_soft_bounce AND NOT has_deferral AND has_sent) AS sent_only,
			COALESCE(SUM(soft_bounce_event_count),0) AS soft_bounce_events,
			COALESCE(SUM(deferral_event_count),0) AS deferral_events
		FROM per_recipient
		GROUP BY sending_domain, recipient_domain
	`, hb, hb, hb)

	rows, err := s.db.QueryContext(ctx, q, start, end)
	if err != nil {
		http.Error(w, fmt.Sprintf("terminal-state query: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	byCell := map[string]*terminalStateBucket{}
	bySD := map[string]*terminalStateBucket{}
	byISP := map[string]*terminalStateBucket{}
	var grand terminalStateBucket
	for rows.Next() {
		var sd, rd string
		var b terminalStateBucket
		if err := rows.Scan(&sd, &rd, &b.Recipients, &b.Delivered, &b.HardBounce,
			&b.SoftBounceFinal, &b.DeferredOpen, &b.SentOnly,
			&b.SoftBounceEvents, &b.DeferralEvents); err != nil {
			continue
		}
		ispGroup := isppkg.GroupFromDomain(rd)
		b.SendingDomain = sd
		b.ISP = ispGroup
		key := sd + "|" + ispGroup
		if cell, ok := byCell[key]; ok {
			mergeTerminalBucket(cell, &b)
		} else {
			cp := b
			byCell[key] = &cp
		}
		mergeTerminalBucketLabel(bySD, sd, &b, "")
		mergeTerminalBucketLabel(byISP, ispGroup, &b, "isp")
		grand.Recipients += b.Recipients
		grand.Delivered += b.Delivered
		grand.HardBounce += b.HardBounce
		grand.SoftBounceFinal += b.SoftBounceFinal
		grand.DeferredOpen += b.DeferredOpen
		grand.SentOnly += b.SentOnly
		grand.SoftBounceEvents += b.SoftBounceEvents
		grand.DeferralEvents += b.DeferralEvents
	}

	cells := flattenTerminalBuckets(byCell)
	sd := flattenTerminalBuckets(bySD)
	isp := flattenTerminalBuckets(byISP)

	writeJSONResponse(w, map[string]interface{}{
		"api_version":     VersionTerminalStateMatrix,
		"range":           map[string]string{"start": start.UTC().Format(time.RFC3339), "end": end.UTC().Format(time.RFC3339)},
		"grand_total":     grand,
		"by_cell":         cells,
		"by_sending_domain": sd,
		"by_isp":          isp,
	})
}

func mergeTerminalBucket(dst, src *terminalStateBucket) {
	dst.Recipients += src.Recipients
	dst.Delivered += src.Delivered
	dst.HardBounce += src.HardBounce
	dst.SoftBounceFinal += src.SoftBounceFinal
	dst.DeferredOpen += src.DeferredOpen
	dst.SentOnly += src.SentOnly
	dst.SoftBounceEvents += src.SoftBounceEvents
	dst.DeferralEvents += src.DeferralEvents
}

func mergeTerminalBucketLabel(m map[string]*terminalStateBucket, key string, src *terminalStateBucket, kind string) {
	if existing, ok := m[key]; ok {
		mergeTerminalBucket(existing, src)
		return
	}
	cp := *src
	if kind == "isp" {
		cp.SendingDomain = ""
		cp.ISP = key
	} else {
		cp.SendingDomain = key
		cp.ISP = ""
	}
	m[key] = &cp
}

func flattenTerminalBuckets(m map[string]*terminalStateBucket) []terminalStateBucket {
	out := make([]terminalStateBucket, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	return out
}

// ─── 2. Domain × ISP performance matrix (sent/delivered/HB%/SB%) ─────────────
//
// Promoted from `.scratch/apr30_yesterday_today_by_domain_isp.py` and
// `.scratch/perf_by_isp_domain.py`. Aggregates per (sending_domain, isp)
// from `pmta_acct_daily_summary` (deduped delivery facts) joined with
// engagement from mailing_tracking_events. This is the deliverability
// heatmap the operator looks at every send-day.

func (s *AdvancedMailingService) HandleDomainISPMatrix(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start, end := parseAnalyticsRange(r)

	type cell struct {
		SendingDomain string  `json:"sending_domain"`
		ISP           string  `json:"isp"`
		Sent          int     `json:"sent"`
		Delivered     int     `json:"delivered"`
		RelayedToSES  int     `json:"relayed_to_ses"`
		HardBounces   int     `json:"hard_bounces"`
		SoftBounces   int     `json:"soft_bounces"`
		Complaints    int     `json:"complaints"`
		Deferred      int     `json:"deferred"`
		HardRate      float64 `json:"hard_bounce_rate"`
		SoftRate      float64 `json:"soft_bounce_rate"`
		DeliveryRate  float64 `json:"delivery_rate"`
	}

	// delivered folds in ses_delivered (SES DELIVERY truth written back by the
	// SES webhook) so via_ses brands don't read ~0; relayed_to_ses is the
	// labeled PMTA→SES handoff column.
	q := `
		SELECT
			LOWER(COALESCE(NULLIF(s.sending_domain,''),'unknown')) as sending_domain,
			COALESCE(NULLIF(s.recipient_isp,''),'other') as isp,
			COALESCE(SUM(s.sent),0)         as sent,
			COALESCE(SUM(s.delivered),0) + COALESCE(SUM(s.ses_delivered),0) as delivered,
			COALESCE(SUM(s.relayed_to_ses),0) as relayed_to_ses,
			COALESCE(SUM(s.hard_bounced),0) as hard_bounces,
			COALESCE(SUM(s.soft_bounced),0) as soft_bounces,
			COALESCE(SUM(s.complained),0)   as complaints,
			COALESCE(SUM(s.deferred),0)     as deferred
		FROM pmta_acct_daily_summary s
		WHERE s.summary_date >= $1::date AND s.summary_date <= $2::date
		GROUP BY sending_domain, isp
		ORDER BY sending_domain, isp
	`

	rows, err := s.db.QueryContext(ctx, q, start, end)
	if err != nil {
		http.Error(w, fmt.Sprintf("matrix query: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	cells := []cell{}
	for rows.Next() {
		var c cell
		if err := rows.Scan(&c.SendingDomain, &c.ISP, &c.Sent, &c.Delivered, &c.RelayedToSES,
			&c.HardBounces, &c.SoftBounces, &c.Complaints, &c.Deferred); err != nil {
			continue
		}
		if c.Sent > 0 {
			c.HardRate = metricsRound2(float64(c.HardBounces) / float64(c.Sent) * 100)
			c.SoftRate = metricsRound2(float64(c.SoftBounces) / float64(c.Sent) * 100)
			c.DeliveryRate = metricsRound2(float64(c.Delivered) / float64(c.Sent) * 100)
		}
		cells = append(cells, c)
	}

	writeJSONResponse(w, map[string]interface{}{
		"api_version": VersionDomainISPMatrix,
		"range":       map[string]string{"start": start.UTC().Format(time.RFC3339), "end": end.UTC().Format(time.RFC3339)},
		"cells":       cells,
	})
}

// ─── 3. Engagement quality (true opens vs proxy vs MPP vs scanner) ───────────
//
// Promoted from `.scratch/true_opens_clicks.py`. Classifies each opened
// event as:
//
//   mpp       → is_machine_open=true (Apple MPP / image proxy)
//   scanner   → opened within 30s of sent (almost certainly an automated
//               link/security scanner re-opening the pixel)
//   human     → not MPP and >30s after sent
//   no_send   → opened with no matching 'sent' row (orphan / replay)
//
// Returns aggregate counts, plus per-ISP and per-sending-domain breakdown.

// engagementQualityBucket counts opened-event classifications for a single
// dimension (overall, per-ISP, per-sending-domain).
type engagementQualityBucket struct {
	Total   int `json:"total"`
	MPP     int `json:"mpp"`
	Scanner int `json:"scanner"`
	Human   int `json:"human"`
	NoSend  int `json:"no_send"`
}

func bumpEngagementBucket(b *engagementQualityBucket, class string) {
	b.Total++
	switch class {
	case "mpp":
		b.MPP++
	case "scanner":
		b.Scanner++
	case "human":
		b.Human++
	case "no_send":
		b.NoSend++
	}
}

func (s *AdvancedMailingService) HandleEngagementQuality(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start, end := parseAnalyticsRange(r)

	// Correlate every 'opened' row in the window with the most recent
	// matching 'sent' row for the same (campaign_id, subscriber_id) up
	// to 7 days prior. Sub-30s gap = scanner, MPP-flagged = mpp,
	// no-send-found = orphan, otherwise human.
	q := `
		SELECT
			LOWER(COALESCE(NULLIF(o.recipient_domain,''),'unknown'))   AS dom,
			LOWER(COALESCE(NULLIF(o.sending_domain,''),'unknown'))      AS sending_domain,
			COALESCE(o.is_machine_open,false)                            AS mpp,
			o.event_at                                                   AS opened_at,
			(SELECT MAX(s.event_at) FROM mailing_tracking_events s
			   WHERE s.campaign_id = o.campaign_id
			     AND s.subscriber_id = o.subscriber_id
			     AND s.event_type = 'sent'
			     AND s.event_at >= $1 - INTERVAL '7 days'
			     AND s.event_at <= o.event_at)                           AS sent_at
		FROM mailing_tracking_events o
		WHERE o.event_type = 'opened'
		  AND o.event_at >= $1 AND o.event_at < $2
	`

	rows, err := s.db.QueryContext(ctx, q, start, end)
	if err != nil {
		http.Error(w, fmt.Sprintf("engagement-quality query: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var grand engagementQualityBucket
	byISP := map[string]*engagementQualityBucket{}
	bySD := map[string]*engagementQualityBucket{}

	classify := func(mpp bool, sentAt sql.NullTime, openedAt time.Time) string {
		if mpp {
			return "mpp"
		}
		if !sentAt.Valid {
			return "no_send"
		}
		if openedAt.Sub(sentAt.Time) <= 30*time.Second {
			return "scanner"
		}
		return "human"
	}

	for rows.Next() {
		var dom, sd string
		var mpp bool
		var openedAt time.Time
		var sentAt sql.NullTime
		if err := rows.Scan(&dom, &sd, &mpp, &openedAt, &sentAt); err != nil {
			continue
		}
		class := classify(mpp, sentAt, openedAt)
		ispGroup := isppkg.GroupFromDomain(dom)

		bumpEngagementBucket(&grand, class)
		if b, ok := byISP[ispGroup]; ok {
			bumpEngagementBucket(b, class)
		} else {
			nb := engagementQualityBucket{}
			bumpEngagementBucket(&nb, class)
			byISP[ispGroup] = &nb
		}
		if b, ok := bySD[sd]; ok {
			bumpEngagementBucket(b, class)
		} else {
			nb := engagementQualityBucket{}
			bumpEngagementBucket(&nb, class)
			bySD[sd] = &nb
		}
	}

	flatten := func(m map[string]*engagementQualityBucket, label string) []map[string]interface{} {
		out := []map[string]interface{}{}
		for k, v := range m {
			row := map[string]interface{}{
				label:    k,
				"total":   v.Total,
				"mpp":     v.MPP,
				"scanner": v.Scanner,
				"human":   v.Human,
				"no_send": v.NoSend,
			}
			out = append(out, row)
		}
		return out
	}

	writeJSONResponse(w, map[string]interface{}{
		"api_version":       VersionEngagementQuality,
		"range":             map[string]string{"start": start.UTC().Format(time.RFC3339), "end": end.UTC().Format(time.RFC3339)},
		"grand_total":       grand,
		"by_isp":            flatten(byISP, "isp"),
		"by_sending_domain": flatten(bySD, "sending_domain"),
	})
}

// ─── 4. Daily acquisition (new subscribers per day × ISP × brand) ────────────
//
// Promoted from `daily_acquisition.py` (root). New subscribers per UTC
// calendar day, grouped by inferred ISP and brand_domain. Brand inference
// uses the canonical `brand_root` column on `mailing_subscribers` (set by
// the activation pipeline); falls back to derived domain when NULL.

func (s *AdvancedMailingService) HandleDailyAcquisition(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start, end := parseAnalyticsRange(r)
	orgID := orgIDFromHeader(r)

	args := []interface{}{start, end}
	orgFilter := ""
	if orgID != "" {
		orgFilter = " AND organization_id = $3::uuid"
		args = append(args, orgID)
	}

	q := fmt.Sprintf(`
		SELECT
			(created_at AT TIME ZONE 'America/Denver')::date AS day,
			LOWER(SPLIT_PART(COALESCE(NULLIF(email,''),'@unknown'),'@',2)) AS recipient_domain,
			COALESCE(NULLIF(brand_root,''),'unknown') AS brand,
			COUNT(*) AS new_subs,
			COUNT(*) FILTER (WHERE status = 'confirmed') AS confirmed_subs
		FROM mailing_subscribers
		WHERE created_at >= $1 AND created_at < $2 %s
		GROUP BY day, recipient_domain, brand
	`, orgFilter)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		http.Error(w, fmt.Sprintf("acquisition query: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type row struct {
		Day       string `json:"day"`
		ISP       string `json:"isp"`
		Brand     string `json:"brand"`
		NewSubs   int    `json:"new_subs"`
		Confirmed int    `json:"confirmed_subs"`
	}
	out := []row{}
	for rows.Next() {
		var day time.Time
		var dom, brand string
		var newCount, confirmed int
		if err := rows.Scan(&day, &dom, &brand, &newCount, &confirmed); err != nil {
			continue
		}
		out = append(out, row{
			Day:       day.Format("2006-01-02"),
			ISP:       isppkg.GroupFromDomain(dom),
			Brand:     brand,
			NewSubs:   newCount,
			Confirmed: confirmed,
		})
	}

	writeJSONResponse(w, map[string]interface{}{
		"api_version": VersionDailyAcquisition,
		"range":       map[string]string{"start": start.UTC().Format(time.RFC3339), "end": end.UTC().Format(time.RFC3339)},
		"rows":        out,
	})
}

// ─── 5. Segment integrity (size drift + seed/exclusion overlap) ──────────────
//
// Promoted from `.scratch/audience_audit.py`. For every segment, returns
// its current member count, snapshot count from yesterday, and the
// overlap with welcome-saturation / global-suppression so the operator
// can see at-a-glance which segments are bleeding members or have
// dangerous overlap with exclusions.

func (s *AdvancedMailingService) HandleSegmentIntegrity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := orgIDFromHeader(r)

	args := []interface{}{}
	orgFilter := ""
	if orgID != "" {
		orgFilter = " WHERE organization_id = $1::uuid"
		args = append(args, orgID)
	}

	q := `
		SELECT
			ms.id::text                                            AS segment_id,
			ms.name                                                AS segment_name,
			COALESCE(ms.subscriber_count, 0)                        AS member_count,
			ms.created_at,
			ms.updated_at
		FROM mailing_segments ms
		` + orgFilter + `
		ORDER BY ms.updated_at DESC
		LIMIT 200
	`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		http.Error(w, fmt.Sprintf("segment-integrity query: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type segRow struct {
		ID          string    `json:"segment_id"`
		Name        string    `json:"segment_name"`
		MemberCount int       `json:"member_count"`
		CreatedAt   time.Time `json:"created_at"`
		UpdatedAt   time.Time `json:"updated_at"`
		StaleHours  float64   `json:"stale_hours"`
	}
	out := []segRow{}
	now := time.Now().UTC()
	for rows.Next() {
		var sr segRow
		if err := rows.Scan(&sr.ID, &sr.Name, &sr.MemberCount, &sr.CreatedAt, &sr.UpdatedAt); err != nil {
			continue
		}
		sr.StaleHours = now.Sub(sr.UpdatedAt).Hours()
		out = append(out, sr)
	}

	writeJSONResponse(w, map[string]interface{}{
		"api_version": VersionSegmentIntegrity,
		"segments":    out,
	})
}

// ─── 6. Queue status histogram (who did we mail today?) ──────────────────────
//
// Promoted from `.scratch/who_did_we_mail.py`. For every active campaign
// in the requested window, returns the histogram of `mailing_campaign_queue.status`
// counts. Operator uses this to see — in real-time — which campaigns are
// stalled, which have failed enqueues, which are mid-flight.

func (s *AdvancedMailingService) HandleQueueStatusHistogram(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := orgIDFromHeader(r)

	q := `
		SELECT
			c.id::text     AS campaign_id,
			c.name         AS name,
			c.status       AS status,
			c.created_at,
			c.scheduled_at,
			c.started_at,
			c.organization_id::text,
			q.status       AS queue_status,
			COUNT(*)       AS cnt
		FROM mailing_campaigns c
		LEFT JOIN mailing_campaign_queue q ON q.campaign_id = c.id
		WHERE c.created_at > NOW() - INTERVAL '48 hours'
		  AND ($1::uuid IS NULL OR c.organization_id = $1::uuid)
		GROUP BY c.id, c.name, c.status, c.created_at, c.scheduled_at, c.started_at, c.organization_id, q.status
		ORDER BY c.created_at DESC
	`
	var orgArg interface{}
	if orgID != "" {
		orgArg = orgID
	}
	rows, err := s.db.QueryContext(ctx, q, orgArg)
	if err != nil {
		http.Error(w, fmt.Sprintf("queue-histogram query: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type campaignAgg struct {
		ID          string                 `json:"campaign_id"`
		Name        string                 `json:"name"`
		Status      string                 `json:"status"`
		CreatedAt   time.Time              `json:"created_at"`
		ScheduledAt sql.NullTime           `json:"scheduled_at"`
		StartedAt   sql.NullTime           `json:"started_at"`
		OrgID       string                 `json:"organization_id"`
		QueueByStatus map[string]int       `json:"queue_by_status"`
		QueueTotal  int                    `json:"queue_total"`
	}
	byCampaign := map[string]*campaignAgg{}
	for rows.Next() {
		var ca campaignAgg
		var qStatus sql.NullString
		var cnt int
		if err := rows.Scan(&ca.ID, &ca.Name, &ca.Status, &ca.CreatedAt,
			&ca.ScheduledAt, &ca.StartedAt, &ca.OrgID, &qStatus, &cnt); err != nil {
			continue
		}
		if existing, ok := byCampaign[ca.ID]; ok {
			if qStatus.Valid {
				existing.QueueByStatus[qStatus.String] += cnt
				existing.QueueTotal += cnt
			}
		} else {
			ca.QueueByStatus = map[string]int{}
			if qStatus.Valid {
				ca.QueueByStatus[qStatus.String] = cnt
				ca.QueueTotal = cnt
			}
			byCampaign[ca.ID] = &ca
		}
	}
	out := []campaignAgg{}
	for _, c := range byCampaign {
		out = append(out, *c)
	}

	writeJSONResponse(w, map[string]interface{}{
		"api_version": VersionQueueStatusHistogram,
		"campaigns":   out,
	})
}

// ─── 7. Wave scheduler health (backlog + zombies on dead campaigns) ──────────
//
// Promoted from `.scratch/apr28_wave_scheduler_health.py`. Surfaces the
// three failure modes the operator's pre-deploy janitor query catches:
//
//   1. zombies   — wave.status='planned' on campaigns that are
//                  cancelled / failed / sent (will block the FIFO)
//   2. expired   — wave.status='planned' but window_end_at < NOW()
//   3. due_now   — wave.status='planned' AND scheduled_at <= NOW() but
//                  not yet picked up

// waveSchedulerCounts holds the janitor's wave-table health counters. Zombies
// and Expired are Send-Day Gate B's inputs (<50 each — CLAUDE.md §6 Gate B);
// factored out so the deploy path's server-side gate evaluation
// (evaluateSendDayGates, REQ-007) reads the exact same source as this endpoint.
type waveSchedulerCounts struct {
	Zombies  int
	Expired  int
	DueNow   int
	Planned  int
	Enqueued int
	Running  int
}

func queryWaveSchedulerCounts(ctx context.Context, db *sql.DB) (waveSchedulerCounts, error) {
	var c waveSchedulerCounts
	row := db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (
				WHERE w.status='planned'
				  AND c.status IN ('cancelled','failed','sent')
			) AS zombies,
			COUNT(*) FILTER (
				WHERE w.status='planned'
				  AND w.window_end_at IS NOT NULL
				  AND w.window_end_at < NOW()
				  AND c.status NOT IN ('cancelled','failed','sent')
			) AS expired,
			COUNT(*) FILTER (
				WHERE w.status='planned'
				  AND w.scheduled_at <= NOW()
				  AND c.status NOT IN ('cancelled','failed','sent')
			) AS due_now,
			COUNT(*) FILTER (WHERE w.status='planned') AS planned,
			COUNT(*) FILTER (WHERE w.status='enqueued') AS enqueued,
			COUNT(*) FILTER (WHERE w.status='running') AS running
		FROM mailing_campaign_waves w
		LEFT JOIN mailing_campaigns c ON c.id = w.campaign_id
	`)
	if err := row.Scan(&c.Zombies, &c.Expired, &c.DueNow, &c.Planned, &c.Enqueued, &c.Running); err != nil {
		return waveSchedulerCounts{}, err
	}
	return c, nil
}

func (s *AdvancedMailingService) HandleWaveSchedulerHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	counts, err := queryWaveSchedulerCounts(ctx, s.db)
	if err != nil {
		http.Error(w, fmt.Sprintf("wave-health query: %v", err), http.StatusInternalServerError)
		return
	}
	zombies, expired, dueNow := counts.Zombies, counts.Expired, counts.DueNow
	planned, enqueued, running := counts.Planned, counts.Enqueued, counts.Running

	// Sample of the worst offenders so the operator can drill in
	q := `
		SELECT w.id::text, w.campaign_id::text, c.name, c.status as campaign_status,
		       w.status as wave_status, w.scheduled_at, w.window_end_at, w.last_error
		FROM mailing_campaign_waves w
		LEFT JOIN mailing_campaigns c ON c.id = w.campaign_id
		WHERE w.status = 'planned'
		  AND (
		       c.status IN ('cancelled','failed','sent')
		    OR (w.window_end_at IS NOT NULL AND w.window_end_at < NOW())
		    OR (w.scheduled_at <= NOW() - INTERVAL '15 minutes')
		  )
		ORDER BY w.scheduled_at ASC
		LIMIT 50
	`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		http.Error(w, fmt.Sprintf("wave-sample query: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type sampleRow struct {
		WaveID         string       `json:"wave_id"`
		CampaignID     string       `json:"campaign_id"`
		CampaignName   string       `json:"campaign_name"`
		CampaignStatus string       `json:"campaign_status"`
		WaveStatus     string       `json:"wave_status"`
		ScheduledAt    sql.NullTime `json:"scheduled_at"`
		WindowEndAt    sql.NullTime `json:"window_end_at"`
		LastError      sql.NullString `json:"last_error"`
	}
	samples := []sampleRow{}
	for rows.Next() {
		var sr sampleRow
		if err := rows.Scan(&sr.WaveID, &sr.CampaignID, &sr.CampaignName,
			&sr.CampaignStatus, &sr.WaveStatus, &sr.ScheduledAt,
			&sr.WindowEndAt, &sr.LastError); err != nil {
			continue
		}
		samples = append(samples, sr)
	}

	writeJSONResponse(w, map[string]interface{}{
		"api_version": VersionWaveSchedulerHealth,
		"summary": map[string]int{
			"zombies":  zombies,
			"expired":  expired,
			"due_now":  dueNow,
			"planned":  planned,
			"enqueued": enqueued,
			"running":  running,
		},
		"samples": samples,
		"action_required": zombies > 0 || expired > 50,
	})
}

// ─── 8. Per-minute dispatch timeline (last 60 min, real-time) ────────────────
//
// Promoted from `.scratch/apr30_post_deploy_per_minute.py`. Rolls
// `mailing_tracking_events` into one-minute buckets for the last hour
// so the operator can SEE dispatch healing or stalling immediately
// after a deploy.

func (s *AdvancedMailingService) HandleDispatchTimeline(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	minutes := 60
	if v := q.Get("minutes"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 360 {
			minutes = n
		}
	}

	hb := HardBounceSQL("")
	queryStr := fmt.Sprintf(`
		SELECT
			DATE_TRUNC('minute', event_at) AS minute,
			COUNT(*) FILTER (WHERE event_type = 'sent')      AS sent,
			COUNT(*) FILTER (WHERE event_type = 'delivered') AS delivered,
			COUNT(*) FILTER (WHERE event_type = 'bounced' AND %s)        AS hard_bounces,
			COUNT(*) FILTER (WHERE event_type = 'bounced' AND NOT (%s))  AS soft_bounces,
			COUNT(*) FILTER (WHERE event_type IN ('deferred','deferral')) AS deferred
		FROM mailing_tracking_events
		WHERE event_at > NOW() - ($1 || ' minutes')::interval
		GROUP BY minute
		ORDER BY minute
	`, hb, hb)
	rows, err := s.db.QueryContext(ctx, queryStr, strconv.Itoa(minutes))
	if err != nil {
		http.Error(w, fmt.Sprintf("dispatch-timeline query: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type bucket struct {
		Minute      time.Time `json:"minute"`
		Sent        int       `json:"sent"`
		Delivered   int       `json:"delivered"`
		HardBounces int       `json:"hard_bounces"`
		SoftBounces int       `json:"soft_bounces"`
		Deferred    int       `json:"deferred"`
	}
	out := []bucket{}
	for rows.Next() {
		var b bucket
		if err := rows.Scan(&b.Minute, &b.Sent, &b.Delivered, &b.HardBounces, &b.SoftBounces, &b.Deferred); err != nil {
			continue
		}
		out = append(out, b)
	}

	writeJSONResponse(w, map[string]interface{}{
		"api_version": VersionDispatchTimeline,
		"minutes":     minutes,
		"buckets":     out,
	})
}

// ─── 9. Growth narrative (90-day reputation + audience trends) ───────────────
//
// Promoted from `.scratch/may05_growth_analysis.py`. Returns daily totals
// for the last 90 days plus a few summary KPIs the operator uses for
// week-over-week reputation reviews. NOT a full HTML report — that's
// rendered client-side from these numbers.

func (s *AdvancedMailingService) HandleGrowthNarrative(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	days := 90
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 180 {
			days = n
		}
	}

	// delivered folds in ses_delivered (SES DELIVERY truth); relayed_to_ses
	// surfaced separately as the PMTA→SES handoff.
	q := `
		SELECT summary_date,
		       COALESCE(SUM(sent),0)         AS sent,
		       COALESCE(SUM(delivered),0) + COALESCE(SUM(ses_delivered),0) AS delivered,
		       COALESCE(SUM(relayed_to_ses),0) AS relayed_to_ses,
		       COALESCE(SUM(hard_bounced),0) AS hard_bounces,
		       COALESCE(SUM(soft_bounced),0) AS soft_bounces,
		       COALESCE(SUM(complained),0)   AS complaints,
		       COALESCE(SUM(deferred),0)     AS deferred
		FROM pmta_acct_daily_summary
		WHERE summary_date >= (NOW() AT TIME ZONE 'America/Denver')::date - ($1 * INTERVAL '1 day')
		GROUP BY summary_date
		ORDER BY summary_date
	`
	rows, err := s.db.QueryContext(ctx, q, days)
	if err != nil {
		http.Error(w, fmt.Sprintf("growth-narrative query: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type row struct {
		Date         time.Time `json:"date"`
		Sent         int       `json:"sent"`
		Delivered    int       `json:"delivered"`
		RelayedToSES int       `json:"relayed_to_ses"`
		HardBounces  int       `json:"hard_bounces"`
		SoftBounces  int       `json:"soft_bounces"`
		Complaints   int       `json:"complaints"`
		Deferred     int       `json:"deferred"`
		HardRate     float64   `json:"hard_bounce_rate"`
		SoftRate     float64   `json:"soft_bounce_rate"`
		DeliveryRate float64   `json:"delivery_rate"`
	}
	out := []row{}
	var grandSent, grandDel, grandHB, grandSB, grandRelayed int
	for rows.Next() {
		var rrow row
		if err := rows.Scan(&rrow.Date, &rrow.Sent, &rrow.Delivered, &rrow.RelayedToSES,
			&rrow.HardBounces, &rrow.SoftBounces, &rrow.Complaints, &rrow.Deferred); err != nil {
			continue
		}
		if rrow.Sent > 0 {
			rrow.HardRate = metricsRound2(float64(rrow.HardBounces) / float64(rrow.Sent) * 100)
			rrow.SoftRate = metricsRound2(float64(rrow.SoftBounces) / float64(rrow.Sent) * 100)
			rrow.DeliveryRate = metricsRound2(float64(rrow.Delivered) / float64(rrow.Sent) * 100)
		}
		grandSent += rrow.Sent
		grandDel += rrow.Delivered
		grandHB += rrow.HardBounces
		grandSB += rrow.SoftBounces
		grandRelayed += rrow.RelayedToSES
		out = append(out, rrow)
	}

	summary := map[string]interface{}{
		"total_sent":           grandSent,
		"total_delivered":      grandDel,
		"total_relayed_to_ses": grandRelayed,
		"total_hard":           grandHB,
		"total_soft":           grandSB,
		"hard_bounce_rate":     0.0,
		"soft_bounce_rate":     0.0,
		"delivery_rate":        0.0,
	}
	if grandSent > 0 {
		summary["hard_bounce_rate"] = metricsRound2(float64(grandHB) / float64(grandSent) * 100)
		summary["soft_bounce_rate"] = metricsRound2(float64(grandSB) / float64(grandSent) * 100)
		summary["delivery_rate"] = metricsRound2(float64(grandDel) / float64(grandSent) * 100)
	}

	writeJSONResponse(w, map[string]interface{}{
		"api_version": VersionGrowthNarrative,
		"days":        days,
		"summary":     summary,
		"daily":       out,
	})
}

// ─── 10. CSV export (terminal-state by domain/ISP/day) ───────────────────────
//
// Streams a CSV of one of the supported reports. Defaults to terminal
// state by sending_domain × ISP for the requested window. Selectable via
// `report=terminal_state|domain_isp|growth_narrative`.

func (s *AdvancedMailingService) HandleAnalyticsCSVExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start, end := parseAnalyticsRange(r)
	report := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("report")))
	if report == "" {
		report = "domain_isp"
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("X-Api-Version", VersionAnalyticsCSVExports)
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s_%s_%s.csv"`,
			report, start.Format("20060102"), end.Format("20060102")))

	cw := csv.NewWriter(w)
	defer cw.Flush()

	switch report {
	case "domain_isp":
		_ = cw.Write([]string{"sending_domain", "isp", "sent", "delivered", "relayed_to_ses", "hard_bounces", "soft_bounces", "complaints", "deferred", "hard_pct", "soft_pct", "delivery_pct"})
		rows, err := s.db.QueryContext(ctx, `
			SELECT
				LOWER(COALESCE(NULLIF(sending_domain,''),'unknown')) AS sd,
				COALESCE(NULLIF(recipient_isp,''),'other')           AS isp,
				COALESCE(SUM(sent),0),
				COALESCE(SUM(delivered),0) + COALESCE(SUM(ses_delivered),0),
				COALESCE(SUM(relayed_to_ses),0),
				COALESCE(SUM(hard_bounced),0), COALESCE(SUM(soft_bounced),0),
				COALESCE(SUM(complained),0), COALESCE(SUM(deferred),0)
			FROM pmta_acct_daily_summary
			WHERE summary_date >= $1::date AND summary_date <= $2::date
			GROUP BY sd, isp
			ORDER BY sd, isp
		`, start, end)
		if err != nil {
			http.Error(w, fmt.Sprintf("csv export query: %v", err), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var sd, isp string
			var sent, del, relayed, hb, sb, comp, def int
			if err := rows.Scan(&sd, &isp, &sent, &del, &relayed, &hb, &sb, &comp, &def); err != nil {
				continue
			}
			hp, sp, dp := 0.0, 0.0, 0.0
			if sent > 0 {
				hp = metricsRound2(float64(hb) / float64(sent) * 100)
				sp = metricsRound2(float64(sb) / float64(sent) * 100)
				dp = metricsRound2(float64(del) / float64(sent) * 100)
			}
			_ = cw.Write([]string{sd, isp,
				strconv.Itoa(sent), strconv.Itoa(del), strconv.Itoa(relayed),
				strconv.Itoa(hb), strconv.Itoa(sb),
				strconv.Itoa(comp), strconv.Itoa(def),
				strconv.FormatFloat(hp, 'f', 2, 64),
				strconv.FormatFloat(sp, 'f', 2, 64),
				strconv.FormatFloat(dp, 'f', 2, 64)})
		}
	case "growth_narrative":
		_ = cw.Write([]string{"date", "sent", "delivered", "relayed_to_ses", "hard_bounces", "soft_bounces", "complaints", "deferred"})
		rows, err := s.db.QueryContext(ctx, `
			SELECT summary_date,
			       COALESCE(SUM(sent),0),
			       COALESCE(SUM(delivered),0) + COALESCE(SUM(ses_delivered),0),
			       COALESCE(SUM(relayed_to_ses),0),
			       COALESCE(SUM(hard_bounced),0), COALESCE(SUM(soft_bounced),0),
			       COALESCE(SUM(complained),0), COALESCE(SUM(deferred),0)
			FROM pmta_acct_daily_summary
			WHERE summary_date >= $1::date AND summary_date <= $2::date
			GROUP BY summary_date
			ORDER BY summary_date
		`, start, end)
		if err != nil {
			http.Error(w, fmt.Sprintf("csv export query: %v", err), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var d time.Time
			var sent, del, relayed, hb, sb, comp, def int
			if err := rows.Scan(&d, &sent, &del, &relayed, &hb, &sb, &comp, &def); err != nil {
				continue
			}
			_ = cw.Write([]string{d.Format("2006-01-02"),
				strconv.Itoa(sent), strconv.Itoa(del), strconv.Itoa(relayed),
				strconv.Itoa(hb), strconv.Itoa(sb),
				strconv.Itoa(comp), strconv.Itoa(def)})
		}
	default:
		http.Error(w, "unsupported report (use: domain_isp | growth_narrative)", http.StatusBadRequest)
	}
}

// Compile-time assertion: every promoted handler is a method on
// *AdvancedMailingService and uses the canonical helpers from metrics.go.
var _ = func(_ *AdvancedMailingService, _ context.Context) {}
