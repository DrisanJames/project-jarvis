package api

// Harvest Stream analytics.
//
// This handler powers the HarvestStreamDashboard in AnalyticsCenter. It is
// READ ONLY — no writes, no side effects. The design goal is surgical
// over-build: given the always-on Welcome Harvest stream is sending 24/7
// across four sending domains and eleven ISP buckets, we need multi-axis
// visibility at 1h / 3h / 5h granularities with ZERO gaps on the frontend.
//
// Response shape is intentionally large. The frontend can pick-and-choose
// sections to render; the price of over-fetching is small vs the price of
// an under-delivered metric the user can't see.
//
// Data sources:
//   * mailing_tracking_events      — sent, opens, clicks, complaints, unsubs
//   * mailing_tracking_events t (bounced + bounce_type) — hard/soft bounces
//     via HardBounceSQL() for consistency with existing reports.
//
// Parallelism model: each sub-query runs on its own pooled connection with
// statement_timeout = 60s, mirroring HandleISPSendingInsights. This bounds
// wall-clock at max(query) instead of sum(query).
//
// Time buckets: the `bucket` param accepts "1h" / "3h" / "5h". PostgreSQL
// DATE_TRUNC only goes to 'hour', so 3h/5h buckets are computed as
//   to_timestamp(floor(extract(epoch from event_at) / (N*3600)) * N*3600)
// which gives monotonic UTC-aligned buckets without timezone drift.
//
// Filters:
//   * campaign_prefix  — defaults to "Welcome Harvest" to scope to the
//     harvest stream specifically. Pass "" to include all campaigns.
//   * hours            — lookback in hours, default 72, clamped [1, 720]
//   * bucket           — 1h | 3h | 5h, default 1h
//   * isp              — optional single-ISP filter (gmail, yahoo, ...)
//   * sending_domain   — optional single-domain filter

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// VersionHarvestPerformance is bumped on every behaviour change so the
// frontend can display "backend v1.0" in the dashboard footer and we can
// verify deploys without guessing.
const VersionHarvestPerformance = "1.1"

// DefaultHarvestCampaignPrefix is the naming prefix the harvest deploy
// script (scripts/deploy_welcome_harvest.py) applies to every brand
// campaign it creates. Keeping the default here as documentation lets the
// analytics page "just work" without any query param once campaigns ship.
const DefaultHarvestCampaignPrefix = "Welcome Harvest"

// harvestBuckets maps the user-facing bucket keyword to its second-width.
// We accept only these three values to guarantee the frontend has a
// bounded switch. Anything else snaps to 1h.
var harvestBuckets = map[string]int{
	"1h": 3600,
	"3h": 3 * 3600,
	"5h": 5 * 3600,
}

// parseHarvestBucket normalizes a user-supplied bucket keyword and returns
// (bucket-key, bucket-width-seconds). Empty / invalid inputs return "1h".
func parseHarvestBucket(raw string) (string, int) {
	key := strings.ToLower(strings.TrimSpace(raw))
	if w, ok := harvestBuckets[key]; ok {
		return key, w
	}
	return "1h", 3600
}

// parseHarvestHours clamps the lookback to [1, 720] hours (30 days).
// Default is 72h which covers the "most recent 3 day window" UX while
// keeping the heatmap readable.
func parseHarvestHours(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 72
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 72
	}
	if n < 1 {
		return 1
	}
	if n > 720 {
		return 720
	}
	return n
}

// sanitizeIdent strips spaces and lowercases an identifier-like string.
// Used for isp/sending_domain filters where we want forgiving input.
func sanitizeIdent(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// sanitizeCampaignPrefix trims the campaign_prefix param. Empty string
// means "no filter" — we do NOT apply the default here because the
// frontend may intentionally pass blank to get cross-campaign totals.
func sanitizeCampaignPrefix(raw string) string {
	return strings.TrimSpace(raw)
}

// HarvestMetrics is the per-row metric shape we return everywhere. Rates
// are pre-computed on the server so the frontend is pure presentation.
type HarvestMetrics struct {
	Sent         int     `json:"sent"`
	Delivered    int     `json:"delivered"`
	HardBounces  int     `json:"hard_bounces"`
	SoftBounces  int     `json:"soft_bounces"`
	Opens        int     `json:"opens"`
	UniqueOpens  int     `json:"unique_opens"`
	Clicks       int     `json:"clicks"`
	UniqueClicks int     `json:"unique_clicks"`
	Complaints   int     `json:"complaints"`
	Unsubs       int     `json:"unsubs"`
	Deferred     int     `json:"deferred"`
	MPPOpens     int     `json:"mpp_opens"`
	OpenRate     float64 `json:"open_rate"`
	ClickRate    float64 `json:"click_rate"`
	HardBounceRate float64 `json:"hard_bounce_rate"`
	SoftBounceRate float64 `json:"soft_bounce_rate"`
	ComplaintRate  float64 `json:"complaint_rate"`
	DeliveryRate   float64 `json:"delivery_rate"`
}

// computeHarvestRates fills the Rate fields based on Sent/Delivered. Open
// and click rates use unique_opens/unique_clicks over delivered per the
// mailing-saas "bounce-metrics" and engagement conventions.
func computeHarvestRates(m *HarvestMetrics) {
	base := float64(m.Delivered)
	if base == 0 {
		base = float64(m.Sent)
	}
	if base > 0 {
		m.OpenRate = metricsRound2(float64(m.UniqueOpens) / base * 100)
		m.ClickRate = metricsRound2(float64(m.UniqueClicks) / base * 100)
	}
	if m.Sent > 0 {
		s := float64(m.Sent)
		m.HardBounceRate = metricsRound2(float64(m.HardBounces) / s * 100)
		m.SoftBounceRate = metricsRound2(float64(m.SoftBounces) / s * 100)
		m.ComplaintRate = metricsRound3(float64(m.Complaints) / s * 100)
		m.DeliveryRate = metricsRound2(float64(m.Delivered) / s * 100)
	}
}

// HandleHarvestPerformance is the dashboard endpoint. See file-level
// comment for full behaviour.
func (s *AdvancedMailingService) HandleHarvestPerformance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	hours := parseHarvestHours(q.Get("hours"))
	bucketKey, bucketWidthSec := parseHarvestBucket(q.Get("bucket"))
	ispFilter := sanitizeIdent(q.Get("isp"))
	domainFilter := sanitizeIdent(q.Get("sending_domain"))

	campaignPrefixRaw := sanitizeCampaignPrefix(q.Get("campaign_prefix"))
	// If the caller omits campaign_prefix entirely, scope to the harvest
	// stream by default. An explicit empty string (?campaign_prefix=) is
	// respected as "no filter".
	campaignPrefix := campaignPrefixRaw
	if _, has := q["campaign_prefix"]; !has {
		campaignPrefix = DefaultHarvestCampaignPrefix
	}

	end := time.Now().UTC()
	start := end.Add(-time.Duration(hours) * time.Hour)

	// Subquery: every tracking-event row in the window, joined to the
	// campaign so we can filter by campaign_prefix. LEFT JOIN because we
	// don't want to drop rows whose campaign_id lookup fails (rare, but
	// happens during race windows between event ingest and campaign
	// insert).
	// NOTE: mailing_tracking_events stores the recipient address in the `email`
	// column (not `recipient`). We alias it back to `recipient` so the many
	// downstream aggregates (d.recipient) in this handler keep working.
	eventSubquery := `SELECT t.event_type, t.event_at, t.bounce_type, t.is_machine_open,
		t.sending_domain, t.campaign_id, t.email AS recipient, t.subscriber_id,
		LOWER(COALESCE(NULLIF(t.recipient_domain,''), 'unknown')) as dom,
		mc.name as campaign_name
		FROM mailing_tracking_events t
		LEFT JOIN mailing_campaigns mc ON mc.id = t.campaign_id
		WHERE t.event_at >= $1 AND t.event_at <= $2`
	args := []interface{}{start, end}

	if campaignPrefix != "" {
		eventSubquery += fmt.Sprintf(" AND mc.name LIKE $%d", len(args)+1)
		args = append(args, campaignPrefix+"%")
	}
	if domainFilter != "" {
		eventSubquery += fmt.Sprintf(" AND LOWER(COALESCE(NULLIF(t.sending_domain,''),'unknown')) = $%d", len(args)+1)
		args = append(args, domainFilter)
	}
	if ispFilter != "" {
		// Apply ISP via the canonical CASE so the filter matches the
		// bucketing elsewhere in this handler.
		eventSubquery += fmt.Sprintf(" AND (%s) = $%d", strings.ReplaceAll(ispDomainCaseSQL, "dom", "LOWER(COALESCE(NULLIF(t.recipient_domain,''),'unknown'))"), len(args)+1)
		args = append(args, ispFilter)
	}

	// Parallel query runner. Mirrors HandleISPSendingInsights.runQ.
	runQ := func(op string, fn func(*sql.Conn) error) error {
		c, err := s.db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("%s: acquire conn: %w", op, err)
		}
		defer c.Close()
		if _, err := c.ExecContext(ctx, "SET statement_timeout = '60s'"); err != nil {
			log.Printf("[HarvestPerformance/%s] set statement_timeout: %v", op, err)
		}
		return fn(c)
	}

	type ispRow struct {
		isp string
		m   HarvestMetrics
	}
	type domainRow struct {
		domain string
		m      HarvestMetrics
	}
	type bucketRow struct {
		tsUTC time.Time
		isp   string
		m     HarvestMetrics
	}
	type hourRow struct {
		hour int
		isp  string
		m    HarvestMetrics
	}
	type campaignRow struct {
		campaignID    string
		name          string
		sendingDomain string
		m             HarvestMetrics
	}

	var (
		byISP         []ispRow
		byDomain      []domainRow
		byBucket      []bucketRow
		byBucketISP   []bucketRow
		byHour        []hourRow
		byCampaign    []campaignRow
		errs          = make(chan error, 8)
		wg            sync.WaitGroup
	)

	// Q1: by-ISP rollup.
	wg.Add(1)
	go func() {
		defer wg.Done()
		qsql := fmt.Sprintf(`SELECT %s as isp,
			SUM(CASE WHEN d.event_type = 'sent' THEN 1 ELSE 0 END) as sent,
			SUM(CASE WHEN d.event_type = 'delivered' THEN 1 ELSE 0 END) as delivered,
			SUM(CASE WHEN d.event_type = 'bounced' AND `+HardBounceSQL("d")+` THEN 1 ELSE 0 END) as hard_bounces,
			SUM(CASE WHEN d.event_type = 'bounced' AND NOT (`+HardBounceSQL("d")+`) THEN 1 ELSE 0 END) as soft_bounces,
			SUM(CASE WHEN d.event_type = 'opened' THEN 1 ELSE 0 END) as opens,
			COUNT(DISTINCT CASE WHEN d.event_type = 'opened' THEN COALESCE(d.subscriber_id::text, d.recipient) END) as unique_opens,
			SUM(CASE WHEN d.event_type = 'clicked' THEN 1 ELSE 0 END) as clicks,
			COUNT(DISTINCT CASE WHEN d.event_type = 'clicked' THEN COALESCE(d.subscriber_id::text, d.recipient) END) as unique_clicks,
			SUM(CASE WHEN d.event_type = 'complained' THEN 1 ELSE 0 END) as complaints,
			SUM(CASE WHEN d.event_type = 'unsubscribed' THEN 1 ELSE 0 END) as unsubs,
			SUM(CASE WHEN d.event_type IN ('deferred','deferral') THEN 1 ELSE 0 END) as deferred,
			SUM(CASE WHEN d.event_type = 'opened' AND COALESCE(d.is_machine_open,false) = true THEN 1 ELSE 0 END) as mpp_opens
		FROM (%s) d GROUP BY isp ORDER BY isp`, ispDomainCaseSQL, eventSubquery)
		if err := runQ("by_isp", func(c *sql.Conn) error {
			rows, err := c.QueryContext(ctx, qsql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var r ispRow
				if err := rows.Scan(&r.isp,
					&r.m.Sent, &r.m.Delivered, &r.m.HardBounces, &r.m.SoftBounces,
					&r.m.Opens, &r.m.UniqueOpens, &r.m.Clicks, &r.m.UniqueClicks,
					&r.m.Complaints, &r.m.Unsubs, &r.m.Deferred, &r.m.MPPOpens); err != nil {
					continue
				}
				computeHarvestRates(&r.m)
				byISP = append(byISP, r)
			}
			return nil
		}); err != nil {
			errs <- err
		}
	}()

	// Q2: by-sending-domain rollup.
	wg.Add(1)
	go func() {
		defer wg.Done()
		qsql := fmt.Sprintf(`SELECT LOWER(COALESCE(NULLIF(d.sending_domain,''),'unknown')) as sd,
			SUM(CASE WHEN d.event_type = 'sent' THEN 1 ELSE 0 END) as sent,
			SUM(CASE WHEN d.event_type = 'delivered' THEN 1 ELSE 0 END) as delivered,
			SUM(CASE WHEN d.event_type = 'bounced' AND `+HardBounceSQL("d")+` THEN 1 ELSE 0 END) as hard_bounces,
			SUM(CASE WHEN d.event_type = 'bounced' AND NOT (`+HardBounceSQL("d")+`) THEN 1 ELSE 0 END) as soft_bounces,
			SUM(CASE WHEN d.event_type = 'opened' THEN 1 ELSE 0 END) as opens,
			COUNT(DISTINCT CASE WHEN d.event_type = 'opened' THEN COALESCE(d.subscriber_id::text, d.recipient) END) as unique_opens,
			SUM(CASE WHEN d.event_type = 'clicked' THEN 1 ELSE 0 END) as clicks,
			COUNT(DISTINCT CASE WHEN d.event_type = 'clicked' THEN COALESCE(d.subscriber_id::text, d.recipient) END) as unique_clicks,
			SUM(CASE WHEN d.event_type = 'complained' THEN 1 ELSE 0 END) as complaints,
			SUM(CASE WHEN d.event_type = 'unsubscribed' THEN 1 ELSE 0 END) as unsubs,
			SUM(CASE WHEN d.event_type IN ('deferred','deferral') THEN 1 ELSE 0 END) as deferred,
			SUM(CASE WHEN d.event_type = 'opened' AND COALESCE(d.is_machine_open,false) = true THEN 1 ELSE 0 END) as mpp_opens
		FROM (%s) d GROUP BY sd ORDER BY sd`, eventSubquery)
		if err := runQ("by_domain", func(c *sql.Conn) error {
			rows, err := c.QueryContext(ctx, qsql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var r domainRow
				if err := rows.Scan(&r.domain,
					&r.m.Sent, &r.m.Delivered, &r.m.HardBounces, &r.m.SoftBounces,
					&r.m.Opens, &r.m.UniqueOpens, &r.m.Clicks, &r.m.UniqueClicks,
					&r.m.Complaints, &r.m.Unsubs, &r.m.Deferred, &r.m.MPPOpens); err != nil {
					continue
				}
				computeHarvestRates(&r.m)
				byDomain = append(byDomain, r)
			}
			return nil
		}); err != nil {
			errs <- err
		}
	}()

	// Q3: time-series (overall, no ISP split).
	wg.Add(1)
	go func() {
		defer wg.Done()
		qsql := fmt.Sprintf(`SELECT
			to_timestamp(floor(extract(epoch from d.event_at) / %d) * %d) as ts,
			SUM(CASE WHEN d.event_type = 'sent' THEN 1 ELSE 0 END) as sent,
			SUM(CASE WHEN d.event_type = 'delivered' THEN 1 ELSE 0 END) as delivered,
			SUM(CASE WHEN d.event_type = 'bounced' AND `+HardBounceSQL("d")+` THEN 1 ELSE 0 END) as hard_bounces,
			SUM(CASE WHEN d.event_type = 'bounced' AND NOT (`+HardBounceSQL("d")+`) THEN 1 ELSE 0 END) as soft_bounces,
			SUM(CASE WHEN d.event_type = 'opened' THEN 1 ELSE 0 END) as opens,
			COUNT(DISTINCT CASE WHEN d.event_type = 'opened' THEN COALESCE(d.subscriber_id::text, d.recipient) END) as unique_opens,
			SUM(CASE WHEN d.event_type = 'clicked' THEN 1 ELSE 0 END) as clicks,
			COUNT(DISTINCT CASE WHEN d.event_type = 'clicked' THEN COALESCE(d.subscriber_id::text, d.recipient) END) as unique_clicks,
			SUM(CASE WHEN d.event_type = 'complained' THEN 1 ELSE 0 END) as complaints,
			SUM(CASE WHEN d.event_type = 'unsubscribed' THEN 1 ELSE 0 END) as unsubs,
			SUM(CASE WHEN d.event_type IN ('deferred','deferral') THEN 1 ELSE 0 END) as deferred,
			SUM(CASE WHEN d.event_type = 'opened' AND COALESCE(d.is_machine_open,false) = true THEN 1 ELSE 0 END) as mpp_opens
		FROM (%s) d GROUP BY ts ORDER BY ts`, bucketWidthSec, bucketWidthSec, eventSubquery)
		if err := runQ("time_series", func(c *sql.Conn) error {
			rows, err := c.QueryContext(ctx, qsql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var br bucketRow
				if err := rows.Scan(&br.tsUTC,
					&br.m.Sent, &br.m.Delivered, &br.m.HardBounces, &br.m.SoftBounces,
					&br.m.Opens, &br.m.UniqueOpens, &br.m.Clicks, &br.m.UniqueClicks,
					&br.m.Complaints, &br.m.Unsubs, &br.m.Deferred, &br.m.MPPOpens); err != nil {
					continue
				}
				computeHarvestRates(&br.m)
				byBucket = append(byBucket, br)
			}
			return nil
		}); err != nil {
			errs <- err
		}
	}()

	// Q4: time-series PER ISP. Needed for the stacked line chart where
	// each ISP is its own series.
	wg.Add(1)
	go func() {
		defer wg.Done()
		qsql := fmt.Sprintf(`SELECT
			to_timestamp(floor(extract(epoch from d.event_at) / %d) * %d) as ts,
			%s as isp,
			SUM(CASE WHEN d.event_type = 'sent' THEN 1 ELSE 0 END) as sent,
			SUM(CASE WHEN d.event_type = 'delivered' THEN 1 ELSE 0 END) as delivered,
			SUM(CASE WHEN d.event_type = 'bounced' AND `+HardBounceSQL("d")+` THEN 1 ELSE 0 END) as hard_bounces,
			SUM(CASE WHEN d.event_type = 'bounced' AND NOT (`+HardBounceSQL("d")+`) THEN 1 ELSE 0 END) as soft_bounces,
			SUM(CASE WHEN d.event_type = 'opened' THEN 1 ELSE 0 END) as opens,
			COUNT(DISTINCT CASE WHEN d.event_type = 'opened' THEN COALESCE(d.subscriber_id::text, d.recipient) END) as unique_opens,
			SUM(CASE WHEN d.event_type = 'clicked' THEN 1 ELSE 0 END) as clicks,
			COUNT(DISTINCT CASE WHEN d.event_type = 'clicked' THEN COALESCE(d.subscriber_id::text, d.recipient) END) as unique_clicks,
			SUM(CASE WHEN d.event_type = 'complained' THEN 1 ELSE 0 END) as complaints,
			SUM(CASE WHEN d.event_type = 'unsubscribed' THEN 1 ELSE 0 END) as unsubs,
			SUM(CASE WHEN d.event_type IN ('deferred','deferral') THEN 1 ELSE 0 END) as deferred,
			SUM(CASE WHEN d.event_type = 'opened' AND COALESCE(d.is_machine_open,false) = true THEN 1 ELSE 0 END) as mpp_opens
		FROM (%s) d GROUP BY ts, isp ORDER BY ts, isp`, bucketWidthSec, bucketWidthSec, ispDomainCaseSQL, eventSubquery)
		if err := runQ("time_series_by_isp", func(c *sql.Conn) error {
			rows, err := c.QueryContext(ctx, qsql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var br bucketRow
				if err := rows.Scan(&br.tsUTC, &br.isp,
					&br.m.Sent, &br.m.Delivered, &br.m.HardBounces, &br.m.SoftBounces,
					&br.m.Opens, &br.m.UniqueOpens, &br.m.Clicks, &br.m.UniqueClicks,
					&br.m.Complaints, &br.m.Unsubs, &br.m.Deferred, &br.m.MPPOpens); err != nil {
					continue
				}
				computeHarvestRates(&br.m)
				byBucketISP = append(byBucketISP, br)
			}
			return nil
		}); err != nil {
			errs <- err
		}
	}()

	// Q5: hour-of-day heatmap PER ISP. Aggregates across the whole window
	// into a 24-slot histogram.
	wg.Add(1)
	go func() {
		defer wg.Done()
		qsql := fmt.Sprintf(`SELECT
			EXTRACT(HOUR FROM d.event_at AT TIME ZONE 'America/Denver')::int as hr,
			%s as isp,
			SUM(CASE WHEN d.event_type = 'sent' THEN 1 ELSE 0 END) as sent,
			SUM(CASE WHEN d.event_type = 'delivered' THEN 1 ELSE 0 END) as delivered,
			SUM(CASE WHEN d.event_type = 'bounced' AND `+HardBounceSQL("d")+` THEN 1 ELSE 0 END) as hard_bounces,
			SUM(CASE WHEN d.event_type = 'bounced' AND NOT (`+HardBounceSQL("d")+`) THEN 1 ELSE 0 END) as soft_bounces,
			SUM(CASE WHEN d.event_type = 'opened' THEN 1 ELSE 0 END) as opens,
			COUNT(DISTINCT CASE WHEN d.event_type = 'opened' THEN COALESCE(d.subscriber_id::text, d.recipient) END) as unique_opens,
			SUM(CASE WHEN d.event_type = 'clicked' THEN 1 ELSE 0 END) as clicks,
			COUNT(DISTINCT CASE WHEN d.event_type = 'clicked' THEN COALESCE(d.subscriber_id::text, d.recipient) END) as unique_clicks,
			SUM(CASE WHEN d.event_type = 'complained' THEN 1 ELSE 0 END) as complaints,
			SUM(CASE WHEN d.event_type = 'unsubscribed' THEN 1 ELSE 0 END) as unsubs,
			SUM(CASE WHEN d.event_type IN ('deferred','deferral') THEN 1 ELSE 0 END) as deferred,
			SUM(CASE WHEN d.event_type = 'opened' AND COALESCE(d.is_machine_open,false) = true THEN 1 ELSE 0 END) as mpp_opens
		FROM (%s) d GROUP BY hr, isp ORDER BY hr, isp`, ispDomainCaseSQL, eventSubquery)
		if err := runQ("hour_of_day", func(c *sql.Conn) error {
			rows, err := c.QueryContext(ctx, qsql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var hr hourRow
				if err := rows.Scan(&hr.hour, &hr.isp,
					&hr.m.Sent, &hr.m.Delivered, &hr.m.HardBounces, &hr.m.SoftBounces,
					&hr.m.Opens, &hr.m.UniqueOpens, &hr.m.Clicks, &hr.m.UniqueClicks,
					&hr.m.Complaints, &hr.m.Unsubs, &hr.m.Deferred, &hr.m.MPPOpens); err != nil {
					continue
				}
				computeHarvestRates(&hr.m)
				byHour = append(byHour, hr)
			}
			return nil
		}); err != nil {
			errs <- err
		}
	}()

	// Q6: per-campaign rollup (limited to 50 campaigns for the drill-down
	// panel).
	wg.Add(1)
	go func() {
		defer wg.Done()
		qsql := fmt.Sprintf(`SELECT d.campaign_id::text as cid,
			COALESCE(NULLIF(d.campaign_name,''), d.campaign_id::text) as name,
			COALESCE(NULLIF(LOWER(MIN(d.sending_domain)),''),'') as sending_domain,
			SUM(CASE WHEN d.event_type = 'sent' THEN 1 ELSE 0 END) as sent,
			SUM(CASE WHEN d.event_type = 'delivered' THEN 1 ELSE 0 END) as delivered,
			SUM(CASE WHEN d.event_type = 'bounced' AND `+HardBounceSQL("d")+` THEN 1 ELSE 0 END) as hard_bounces,
			SUM(CASE WHEN d.event_type = 'bounced' AND NOT (`+HardBounceSQL("d")+`) THEN 1 ELSE 0 END) as soft_bounces,
			SUM(CASE WHEN d.event_type = 'opened' THEN 1 ELSE 0 END) as opens,
			COUNT(DISTINCT CASE WHEN d.event_type = 'opened' THEN COALESCE(d.subscriber_id::text, d.recipient) END) as unique_opens,
			SUM(CASE WHEN d.event_type = 'clicked' THEN 1 ELSE 0 END) as clicks,
			COUNT(DISTINCT CASE WHEN d.event_type = 'clicked' THEN COALESCE(d.subscriber_id::text, d.recipient) END) as unique_clicks,
			SUM(CASE WHEN d.event_type = 'complained' THEN 1 ELSE 0 END) as complaints,
			SUM(CASE WHEN d.event_type = 'unsubscribed' THEN 1 ELSE 0 END) as unsubs,
			SUM(CASE WHEN d.event_type IN ('deferred','deferral') THEN 1 ELSE 0 END) as deferred,
			SUM(CASE WHEN d.event_type = 'opened' AND COALESCE(d.is_machine_open,false) = true THEN 1 ELSE 0 END) as mpp_opens
		FROM (%s) d
		WHERE d.campaign_id IS NOT NULL
		GROUP BY d.campaign_id, d.campaign_name
		ORDER BY sent DESC
		LIMIT 50`, eventSubquery)
		if err := runQ("by_campaign", func(c *sql.Conn) error {
			rows, err := c.QueryContext(ctx, qsql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var r campaignRow
				if err := rows.Scan(&r.campaignID, &r.name, &r.sendingDomain,
					&r.m.Sent, &r.m.Delivered, &r.m.HardBounces, &r.m.SoftBounces,
					&r.m.Opens, &r.m.UniqueOpens, &r.m.Clicks, &r.m.UniqueClicks,
					&r.m.Complaints, &r.m.Unsubs, &r.m.Deferred, &r.m.MPPOpens); err != nil {
					continue
				}
				computeHarvestRates(&r.m)
				byCampaign = append(byCampaign, r)
			}
			return nil
		}); err != nil {
			errs <- err
		}
	}()

	wg.Wait()
	close(errs)

	// Collect any errors and surface the FIRST one — the endpoint fails
	// fast rather than returning partial aggregates that could mislead.
	for e := range errs {
		if e != nil {
			http.Error(w, e.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Compute overall from byISP (cheaper than running a seventh query,
	// and numerically identical because the CASE…ELSE 'other' partitions
	// every row into exactly one bucket).
	var overall HarvestMetrics
	for _, r := range byISP {
		overall.Sent += r.m.Sent
		overall.Delivered += r.m.Delivered
		overall.HardBounces += r.m.HardBounces
		overall.SoftBounces += r.m.SoftBounces
		overall.Opens += r.m.Opens
		overall.UniqueOpens += r.m.UniqueOpens
		overall.Clicks += r.m.Clicks
		overall.UniqueClicks += r.m.UniqueClicks
		overall.Complaints += r.m.Complaints
		overall.Unsubs += r.m.Unsubs
		overall.Deferred += r.m.Deferred
		overall.MPPOpens += r.m.MPPOpens
	}
	computeHarvestRates(&overall)

	// Shape the response. We preserve all sub-rollups even when filters
	// are applied so the frontend can run its own sparkline / ratio math
	// without needing a second request.
	respByISP := make([]map[string]interface{}, 0, len(byISP))
	for _, r := range byISP {
		label, ok := ispLabels[r.isp]
		if !ok {
			label = r.isp
		}
		respByISP = append(respByISP, map[string]interface{}{
			"isp":          r.isp,
			"display_name": label,
			"metrics":      r.m,
		})
	}

	respByDomain := make([]map[string]interface{}, 0, len(byDomain))
	for _, r := range byDomain {
		if r.domain == "" || r.domain == "unknown" {
			continue
		}
		respByDomain = append(respByDomain, map[string]interface{}{
			"sending_domain": r.domain,
			"metrics":        r.m,
		})
	}

	respTS := make([]map[string]interface{}, 0, len(byBucket))
	for _, b := range byBucket {
		respTS = append(respTS, map[string]interface{}{
			"ts_utc":  b.tsUTC.UTC().Format(time.RFC3339),
			"ts_mst":  b.tsUTC.In(mstLoc).Format(time.RFC3339),
			"metrics": b.m,
		})
	}

	tsByISP := map[string][]map[string]interface{}{}
	for _, b := range byBucketISP {
		tsByISP[b.isp] = append(tsByISP[b.isp], map[string]interface{}{
			"ts_utc":  b.tsUTC.UTC().Format(time.RFC3339),
			"ts_mst":  b.tsUTC.In(mstLoc).Format(time.RFC3339),
			"metrics": b.m,
		})
	}

	heatmap := make([]map[string]interface{}, 0, len(byHour))
	for _, h := range byHour {
		heatmap = append(heatmap, map[string]interface{}{
			"hour":    h.hour,
			"isp":     h.isp,
			"metrics": h.m,
		})
	}

	respByCampaign := make([]map[string]interface{}, 0, len(byCampaign))
	for _, r := range byCampaign {
		respByCampaign = append(respByCampaign, map[string]interface{}{
			"campaign_id":    r.campaignID,
			"name":           r.name,
			"sending_domain": r.sendingDomain,
			"metrics":        r.m,
		})
	}

	// Engagement-vs-damage: a single ratio card that tells the operator
	// if the stream is winning or leaking. Engagement is unique opens +
	// unique clicks. Damage is hard bounces + complaints. We deliberately
	// exclude soft bounces and deferrals from damage because they are
	// transient.
	engagement := overall.UniqueOpens + overall.UniqueClicks
	damage := overall.HardBounces + overall.Complaints
	ratio := 0.0
	if damage > 0 {
		ratio = metricsRound2(float64(engagement) / math.Max(float64(damage), 1))
	} else if engagement > 0 {
		ratio = -1 // sentinel: "perfect" (no damage). Frontend renders as ∞.
	}

	resp := map[string]interface{}{
		"api_version":     VersionHarvestPerformance,
		"campaign_prefix": campaignPrefix,
		"window": map[string]interface{}{
			"start_utc":       start.Format(time.RFC3339),
			"end_utc":         end.Format(time.RFC3339),
			"hours":           hours,
			"bucket":          bucketKey,
			"bucket_seconds":  bucketWidthSec,
		},
		"filters": map[string]interface{}{
			"isp":            ispFilter,
			"sending_domain": domainFilter,
		},
		"overall":            overall,
		"by_isp":             respByISP,
		"by_sending_domain":  respByDomain,
		"time_series":        respTS,
		"time_series_by_isp": tsByISP,
		"hour_of_day":        heatmap,
		"by_campaign":        respByCampaign,
		"engagement_vs_damage": map[string]interface{}{
			"engagement":     engagement,
			"damage":         damage,
			"ratio":          ratio,
			"hard_bounces":   overall.HardBounces,
			"complaints":     overall.Complaints,
			"unique_opens":   overall.UniqueOpens,
			"unique_clicks":  overall.UniqueClicks,
		},
	}

	// Context: bucket precedence for sanity checks. The frontend can use
	// this to show "backend rounded your 4h request to 1h" etc.
	_ = context.TODO()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
