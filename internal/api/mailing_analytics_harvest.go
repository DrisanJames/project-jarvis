package api

// Harvest Stream analytics.
//
// This handler powers the HarvestStreamDashboard in AnalyticsCenter. It is
// READ ONLY — no writes, no side effects.
//
// Data sources:
//   * mailing_tracking_events  — sent, delivered, opens, clicks, bounces,
//                                complaints, unsubs (post-send lifecycle)
//   * mailing_subscribers      — recipient identity + acquisition metrics
//
// Parallelism model: each sub-query runs on its own pooled connection with
// statement_timeout = 60s. Wall-clock is bounded by max(query) instead of
// sum(query).
//
// Aggregation rules (the *contract* the dashboard relies on):
//   * Sent / Delivered / Hard Bounce / Soft Bounce / Complaint / Unsub
//     counts are dedup'd via COUNT(DISTINCT recipient) so duplicate
//     webhook callbacks (PMTA + SES + retries) cannot push delivered above
//     sent. The "recipient" is COALESCE(email_id, id) — email_id is the
//     canonical per-message identifier; id falls back per-row when
//     email_id is null.
//   * Opens / Clicks are reported in two flavors:
//       - opens / clicks (gross)        — every event, useful for raw load
//       - unique_opens / unique_clicks  — distinct recipients
//   * Per-ISP bucketing reads recipient_domain from the tracking event,
//     falling back to the subscriber's email domain. Without this fallback
//     opens and clicks (which historically wrote a NULL recipient_domain)
//     were ALL collapsed into the "other" ISP bucket.
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
//
// 1.2 — fixed `column t.email does not exist` regression by switching the
//       eventSubquery's recipient projection to `email_id::text`.
// 1.3 — accuracy overhaul:
//        a) per-recipient dedup via COUNT(DISTINCT recipient) for sent /
//           delivered / hard / soft / complaints / unsubs (fixes the
//           "delivery > sent" inversion caused by duplicate webhook rows).
//        b) JOIN mailing_subscribers and fall back to subscriber email
//           domain when t.recipient_domain is NULL/empty (fixes the
//           "all opens land in 'other'" bug for opens/clicks rows).
//        c) acquisition metrics (newly created subscribers) per-ISP and
//           per time-bucket within the same window.
const VersionHarvestPerformance = "1.3"

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
// keeping the heatmap readable. The frontend offers presets (1h, 3h, 5h,
// 24h, 72h, 120h, 168h, 720h) but ANY integer in range is accepted so a
// custom-day workflow doesn't need a backend release.
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
	Sent           int     `json:"sent"`
	Delivered      int     `json:"delivered"`
	HardBounces    int     `json:"hard_bounces"`
	SoftBounces    int     `json:"soft_bounces"`
	Opens          int     `json:"opens"`
	UniqueOpens    int     `json:"unique_opens"`
	Clicks         int     `json:"clicks"`
	UniqueClicks   int     `json:"unique_clicks"`
	Complaints     int     `json:"complaints"`
	Unsubs         int     `json:"unsubs"`
	Deferred       int     `json:"deferred"`
	MPPOpens       int     `json:"mpp_opens"`
	OpenRate       float64 `json:"open_rate"`
	ClickRate      float64 `json:"click_rate"`
	HardBounceRate float64 `json:"hard_bounce_rate"`
	SoftBounceRate float64 `json:"soft_bounce_rate"`
	ComplaintRate  float64 `json:"complaint_rate"`
	DeliveryRate   float64 `json:"delivery_rate"`
}

// computeHarvestRates fills the Rate fields based on Sent/Delivered. Open
// and click rates use unique_opens/unique_clicks over delivered per the
// mailing-saas "bounce-metrics" and engagement conventions.
//
// Defensive clamp: with the v1.3 dedup we no longer expect delivered to
// exceed sent, but if upstream data still produces that anomaly we cap
// the delivery rate at 100% so the UI never shows a >100% impossible
// figure — better to under-report than to mislead the operator.
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
		dr := metricsRound2(float64(m.Delivered) / s * 100)
		if dr > 100 {
			dr = 100
		}
		m.DeliveryRate = dr
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
	// campaign so we can filter by campaign_prefix, AND to mailing_subscribers
	// so we can recover the recipient's email domain when the event row
	// itself doesn't carry recipient_domain (legacy/track-pixel rows).
	//
	// The `dom` projection is the canonical fallback used elsewhere in
	// mailing_analytics.go — keeping the same expression here guarantees
	// the harvest dashboard buckets ISPs the same way as every other
	// per-ISP report on the platform.
	//
	// `recipient` is the per-row identity used downstream by the
	//   COUNT(DISTINCT CASE ... THEN d.recipient END)
	// expressions for dedup'd counts and unique opens/clicks. Each
	// logical message gets a stable `recipient` value via email_id; the
	// per-row id fallback applies only to legacy rows where email_id was
	// never populated, in which case DISTINCT degrades to row-distinct
	// (still safe — never *more* than the row count).
	eventSubquery := `SELECT t.event_type, t.event_at, t.bounce_type, t.is_machine_open,
		t.sending_domain, t.campaign_id,
		COALESCE(t.email_id::text, t.id::text) AS recipient,
		t.subscriber_id,
		LOWER(COALESCE(NULLIF(t.recipient_domain,''), SPLIT_PART(s.email,'@',2), 'unknown')) as dom,
		mc.name as campaign_name
		FROM mailing_tracking_events t
		LEFT JOIN mailing_campaigns mc ON mc.id = t.campaign_id
		LEFT JOIN mailing_subscribers s ON s.id = t.subscriber_id
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
		// bucketing elsewhere in this handler. We re-use the same dom
		// expression (recipient_domain → fallback to subscriber email
		// domain) so a request like ?isp=gmail catches gmail-domained
		// recipients regardless of which webhook wrote the row.
		domExpr := "LOWER(COALESCE(NULLIF(t.recipient_domain,''), SPLIT_PART(s.email,'@',2), 'unknown'))"
		eventSubquery += fmt.Sprintf(" AND (%s) = $%d",
			strings.ReplaceAll(ispDomainCaseSQL, "dom", domExpr),
			len(args)+1)
		args = append(args, ispFilter)
	}

	// Shared metric SELECT-list. Every aggregation query (by_isp,
	// by_domain, time_series, time_series_by_isp, hour_of_day,
	// by_campaign) emits the same 12-column metric block in the same
	// order, so the scan signatures are identical.
	//
	// Note: opens/clicks are gross row counts (SUM CASE) because every
	// open event represents a real beacon hit — duplicates ARE the data
	// point we want for "how many opens did this subscriber generate".
	// unique_opens/unique_clicks dedup to per-recipient.
	//
	// Sent / Delivered / Hard / Soft / Complaints / Unsubs are dedup'd
	// to per-recipient via DISTINCT recipient. Without this dedup,
	// duplicate PMTA delivery callbacks pushed delivered above sent.
	hb := HardBounceSQL("d")
	metricSelect := fmt.Sprintf(`
		COUNT(DISTINCT CASE WHEN d.event_type = 'sent' THEN d.recipient END) as sent,
		COUNT(DISTINCT CASE WHEN d.event_type = 'delivered' THEN d.recipient END) as delivered,
		COUNT(DISTINCT CASE WHEN d.event_type = 'bounced' AND %s THEN d.recipient END) as hard_bounces,
		COUNT(DISTINCT CASE WHEN d.event_type = 'bounced' AND NOT (%s) THEN d.recipient END) as soft_bounces,
		SUM(CASE WHEN d.event_type = 'opened' THEN 1 ELSE 0 END) as opens,
		COUNT(DISTINCT CASE WHEN d.event_type = 'opened' THEN COALESCE(d.subscriber_id::text, d.recipient) END) as unique_opens,
		SUM(CASE WHEN d.event_type = 'clicked' THEN 1 ELSE 0 END) as clicks,
		COUNT(DISTINCT CASE WHEN d.event_type = 'clicked' THEN COALESCE(d.subscriber_id::text, d.recipient) END) as unique_clicks,
		COUNT(DISTINCT CASE WHEN d.event_type = 'complained' THEN d.recipient END) as complaints,
		COUNT(DISTINCT CASE WHEN d.event_type = 'unsubscribed' THEN d.recipient END) as unsubs,
		SUM(CASE WHEN d.event_type IN ('deferred','deferral') THEN 1 ELSE 0 END) as deferred,
		COUNT(DISTINCT CASE WHEN d.event_type = 'opened' AND COALESCE(d.is_machine_open,false) = true THEN COALESCE(d.subscriber_id::text, d.recipient) END) as mpp_opens
		`, hb, hb)

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
	type acqIspRow struct {
		isp           string
		newCount      int
		confirmedNew  int
	}
	type acqBucketRow struct {
		tsUTC        time.Time
		newCount     int
		confirmedNew int
	}

	var (
		byISP       []ispRow
		byDomain    []domainRow
		byBucket    []bucketRow
		byBucketISP []bucketRow
		byHour      []hourRow
		byCampaign  []campaignRow
		acqByISP    []acqIspRow
		acqTS       []acqBucketRow
		errs        = make(chan error, 16)
		wg          sync.WaitGroup
	)

	// Q1: by-ISP rollup.
	wg.Add(1)
	go func() {
		defer wg.Done()
		qsql := fmt.Sprintf(`SELECT %s as isp, %s
			FROM (%s) d GROUP BY isp ORDER BY isp`, ispDomainCaseSQL, metricSelect, eventSubquery)
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
		qsql := fmt.Sprintf(`SELECT LOWER(COALESCE(NULLIF(d.sending_domain,''),'unknown')) as sd, %s
			FROM (%s) d GROUP BY sd ORDER BY sd`, metricSelect, eventSubquery)
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
			to_timestamp(floor(extract(epoch from d.event_at) / %d) * %d) as ts, %s
			FROM (%s) d GROUP BY ts ORDER BY ts`, bucketWidthSec, bucketWidthSec, metricSelect, eventSubquery)
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
			%s as isp, %s
			FROM (%s) d GROUP BY ts, isp ORDER BY ts, isp`, bucketWidthSec, bucketWidthSec, ispDomainCaseSQL, metricSelect, eventSubquery)
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
			%s as isp, %s
			FROM (%s) d GROUP BY hr, isp ORDER BY hr, isp`, ispDomainCaseSQL, metricSelect, eventSubquery)
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
			%s
		FROM (%s) d
		WHERE d.campaign_id IS NOT NULL
		GROUP BY d.campaign_id, d.campaign_name
		ORDER BY sent DESC
		LIMIT 50`, metricSelect, eventSubquery)
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

	// Q7: acquisition by ISP.
	//
	// "Newly introduced audience members" within the same window. Groups
	// by the same ISP CASE used everywhere else so a row like
	// "12 new Gmail subscribers in the last 24h" lines up exactly with
	// the engagement table above it.
	//
	// We do NOT apply the ISP/sending_domain/campaign_prefix filters here
	// because acquisition is a list-level event — it can't be attributed
	// to a specific campaign. The frontend treats this as cross-campaign
	// context regardless of the rest of the filter selection.
	wg.Add(1)
	go func() {
		defer wg.Done()
		qsql := fmt.Sprintf(`SELECT %s as isp,
			COUNT(*) as new_count,
			COUNT(*) FILTER (WHERE LOWER(COALESCE(s.status,'')) IN ('active','confirmed')) as confirmed_new
			FROM (
				SELECT s.id, s.status,
				       LOWER(SPLIT_PART(s.email, '@', 2)) as dom
				FROM mailing_subscribers s
				WHERE s.created_at >= $1 AND s.created_at <= $2
				  AND s.email LIKE '%%@%%'
			) s
			GROUP BY isp ORDER BY isp`, ispDomainCaseSQL)
		if err := runQ("acq_by_isp", func(c *sql.Conn) error {
			rows, err := c.QueryContext(ctx, qsql, start, end)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var r acqIspRow
				if err := rows.Scan(&r.isp, &r.newCount, &r.confirmedNew); err != nil {
					continue
				}
				acqByISP = append(acqByISP, r)
			}
			return nil
		}); err != nil {
			errs <- err
		}
	}()

	// Q8: acquisition time-series (overall).
	//
	// Same bucketing as the engagement time-series so the dashboard can
	// overlay an "audience growth" line on the existing chart.
	wg.Add(1)
	go func() {
		defer wg.Done()
		qsql := fmt.Sprintf(`SELECT
			to_timestamp(floor(extract(epoch from s.created_at) / %d) * %d) as ts,
			COUNT(*) as new_count,
			COUNT(*) FILTER (WHERE LOWER(COALESCE(s.status,'')) IN ('active','confirmed')) as confirmed_new
			FROM mailing_subscribers s
			WHERE s.created_at >= $1 AND s.created_at <= $2
			GROUP BY ts ORDER BY ts`, bucketWidthSec, bucketWidthSec)
		if err := runQ("acq_time_series", func(c *sql.Conn) error {
			rows, err := c.QueryContext(ctx, qsql, start, end)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var r acqBucketRow
				if err := rows.Scan(&r.tsUTC, &r.newCount, &r.confirmedNew); err != nil {
					continue
				}
				acqTS = append(acqTS, r)
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

	// Compute overall from byISP (cheaper than running another query,
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

	// Shape the response.
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

	// Acquisition response shape: a per-ISP card row, a time-series, and
	// a single overall total. The frontend renders a small "+N new
	// subscribers" KPI block plus an additional line on the chart.
	respAcqByISP := make([]map[string]interface{}, 0, len(acqByISP))
	var acqTotalNew, acqTotalConfirmed int
	for _, r := range acqByISP {
		label, ok := ispLabels[r.isp]
		if !ok {
			label = r.isp
		}
		respAcqByISP = append(respAcqByISP, map[string]interface{}{
			"isp":            r.isp,
			"display_name":   label,
			"new_count":      r.newCount,
			"confirmed_new":  r.confirmedNew,
		})
		acqTotalNew += r.newCount
		acqTotalConfirmed += r.confirmedNew
	}

	respAcqTS := make([]map[string]interface{}, 0, len(acqTS))
	for _, b := range acqTS {
		respAcqTS = append(respAcqTS, map[string]interface{}{
			"ts_utc":        b.tsUTC.UTC().Format(time.RFC3339),
			"ts_mst":        b.tsUTC.In(mstLoc).Format(time.RFC3339),
			"new_count":     b.newCount,
			"confirmed_new": b.confirmedNew,
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
			"start_utc":      start.Format(time.RFC3339),
			"end_utc":        end.Format(time.RFC3339),
			"start_mst":      start.In(mstLoc).Format(time.RFC3339),
			"end_mst":        end.In(mstLoc).Format(time.RFC3339),
			"hours":          hours,
			"bucket":         bucketKey,
			"bucket_seconds": bucketWidthSec,
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
		"acquisition": map[string]interface{}{
			"total_new":       acqTotalNew,
			"total_confirmed": acqTotalConfirmed,
			"by_isp":          respAcqByISP,
			"time_series":     respAcqTS,
		},
		"engagement_vs_damage": map[string]interface{}{
			"engagement":    engagement,
			"damage":        damage,
			"ratio":         ratio,
			"hard_bounces":  overall.HardBounces,
			"complaints":    overall.Complaints,
			"unique_opens":  overall.UniqueOpens,
			"unique_clicks": overall.UniqueClicks,
		},
	}

	// Context: bucket precedence for sanity checks. The frontend can use
	// this to show "backend rounded your 4h request to 1h" etc.
	_ = context.TODO()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
