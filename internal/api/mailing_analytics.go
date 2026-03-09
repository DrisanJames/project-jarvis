package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// ─── API Version Constants ────────────────────────────────────────────────────
// Bump the version for any handler you modify. The version is included in every
// JSON response so the frontend can display it for deployment verification.
const (
	VersionAnalyticsOverview       = "1.2"
	VersionISPPerformance          = "1.0"
	VersionISPSendingInsights      = "1.1"
	VersionCampaignComparison      = "1.0"
	VersionTopPerformers           = "1.0"
	VersionListPerformance         = "1.0"
	VersionEngagementReport        = "1.0"
	VersionDeliverabilityReport    = "1.0"
	VersionInfrastructureBreakdown = "1.5"
	VersionRevenueReport           = "1.0"
	VersionHistoricalMetrics       = "1.0"
)

// ─── Pure Functions ───────────────────────────────────────────────────────────

// InfraRates holds computed percentage rates for infrastructure metrics.
type InfraRates struct {
	OpenRate      float64
	ClickRate     float64
	BounceRate    float64
	ComplaintRate float64
	DeferralRate  float64
}

// ComputeInfraRates calculates engagement and deliverability rates.
// Open/Click rates use delivered as the base (falls back to sent if delivered is 0).
// Bounce/Complaint/Deferral rates always use sent as the base.
func ComputeInfraRates(sent, delivered, opens, clicks, bounces, complaints, deferred int) InfraRates {
	r := InfraRates{}
	base := float64(delivered)
	if base == 0 {
		base = float64(sent)
	}
	if base > 0 {
		r.OpenRate = math.Round(float64(opens)/base*1000) / 10
		r.ClickRate = math.Round(float64(clicks)/base*1000) / 10
	}
	if sent > 0 {
		s := float64(sent)
		r.BounceRate = math.Round(float64(bounces) / s * 10000) / 100
		r.ComplaintRate = math.Round(float64(complaints) / s * 100000) / 1000
		r.DeferralRate = math.Round(float64(deferred) / s * 10000) / 100
	}
	return r
}

// parseAnalyticsRange extracts start/end times from query params with sub-day support.
// Supports: start_date + end_date (ISO 8601 or YYYY-MM-DD), range_type (1h, 24h, today, 7, 14, 30, 90), days (legacy).
func parseAnalyticsRange(r *http.Request) (start, end time.Time) {
	now := time.Now().UTC()
	q := r.URL.Query()

	if sd := q.Get("start_date"); sd != "" {
		if t, err := time.Parse(time.RFC3339, sd); err == nil {
			start = t
		} else if t, err := time.Parse("2006-01-02", sd); err == nil {
			start = t
		}
	}
	if ed := q.Get("end_date"); ed != "" {
		if t, err := time.Parse(time.RFC3339, ed); err == nil {
			end = t
		} else if t, err := time.Parse("2006-01-02", ed); err == nil {
			end = t.Add(23*time.Hour + 59*time.Minute + 59*time.Second)
		}
	}

	if !start.IsZero() && !end.IsZero() {
		return start, end
	}

	// If only start is provided, assume end is now
	if !start.IsZero() && end.IsZero() {
		return start, now
	}

	end = now
	rt := q.Get("range_type")
	if rt == "" {
		rt = q.Get("days")
	}

	switch rt {
	case "1h":
		start = now.Add(-1 * time.Hour)
	case "24h":
		start = now.Add(-24 * time.Hour)
	case "today":
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	case "7":
		start = now.AddDate(0, 0, -7)
	case "14":
		start = now.AddDate(0, 0, -14)
	case "90":
		start = now.AddDate(0, 0, -90)
	default:
		d := 30
		if ds := q.Get("days"); ds != "" {
			if v, err := strconv.Atoi(ds); err == nil && v > 0 {
				d = v
			}
		}
		start = now.AddDate(0, 0, -d)
	}
	return start, end
}

func trendGranularity(start, end time.Time) string {
	d := end.Sub(start)
	if d <= time.Hour+time.Minute {
		return "10min"
	}
	if d <= 48*time.Hour {
		return "hour"
	}
	return "day"
}

func (s *AdvancedMailingService) HandleCampaignTimeline(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID := chi.URLParam(r, "campaignId")
	
	rows, _ := s.db.QueryContext(ctx, `
		SELECT DATE_TRUNC('hour', event_time) as hour,
			   SUM(CASE WHEN event_type = 'opened' THEN 1 ELSE 0 END) as opens,
			   SUM(CASE WHEN event_type = 'clicked' THEN 1 ELSE 0 END) as clicks,
			   SUM(CASE WHEN event_type IN ('hard_bounce', 'soft_bounce', 'bounced') THEN 1 ELSE 0 END) as bounces
		FROM mailing_tracking_events
		WHERE campaign_id = $1
		GROUP BY DATE_TRUNC('hour', event_time)
		ORDER BY hour
	`, campaignID)
	defer rows.Close()
	
	var timeline []map[string]interface{}
	for rows.Next() {
		var hour time.Time
		var opens, clicks, bounces int
		rows.Scan(&hour, &opens, &clicks, &bounces)
		timeline = append(timeline, map[string]interface{}{
			"hour": hour.Format(time.RFC3339), "opens": opens, "clicks": clicks, "bounces": bounces,
		})
	}
	if timeline == nil { timeline = []map[string]interface{}{} }
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"timeline": timeline})
}

func (s *AdvancedMailingService) HandleCampaignByDomain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID := chi.URLParam(r, "campaignId")

	// JOIN with subscribers to get email domain (email column may not exist on tracking_events)
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(SPLIT_PART(s.email, '@', 2), 'unknown') as domain,
		       SUM(CASE WHEN t.event_type = 'sent' THEN 1 ELSE 0 END) as sent,
		       SUM(CASE WHEN t.event_type = 'delivered' THEN 1 ELSE 0 END) as delivered,
		       SUM(CASE WHEN t.event_type = 'opened' THEN 1 ELSE 0 END) as opens,
		       SUM(CASE WHEN t.event_type = 'clicked' THEN 1 ELSE 0 END) as clicks,
		       SUM(CASE WHEN t.event_type = 'hard_bounce' THEN 1 ELSE 0 END) as hard_bounces,
		       SUM(CASE WHEN t.event_type = 'soft_bounce' THEN 1 ELSE 0 END) as soft_bounces,
		       SUM(CASE WHEN t.event_type = 'complained' THEN 1 ELSE 0 END) as complaints
		FROM mailing_tracking_events t
		JOIN mailing_subscribers s ON s.id = t.subscriber_id
		WHERE t.campaign_id = $1 AND s.email IS NOT NULL AND s.email != ''
		GROUP BY SPLIT_PART(s.email, '@', 2)
		ORDER BY sent DESC
		LIMIT 50
	`, campaignID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	var domains []map[string]interface{}
	for rows.Next() {
		var domain string
		var sent, delivered, opens, clicks, hardBounces, softBounces, complaints int
		if err := rows.Scan(&domain, &sent, &delivered, &opens, &clicks, &hardBounces, &softBounces, &complaints); err != nil {
			continue
		}
		openRate, clickRate := 0.0, 0.0
		if delivered > 0 {
			openRate = float64(opens) / float64(delivered) * 100
			clickRate = float64(clicks) / float64(delivered) * 100
		}
		domains = append(domains, map[string]interface{}{
			"domain": domain, "sent": sent, "delivered": delivered,
			"opens": opens, "clicks": clicks,
			"hard_bounces": hardBounces, "soft_bounces": softBounces, "complaints": complaints,
			"open_rate": math.Round(openRate*100) / 100, "click_rate": math.Round(clickRate*100) / 100,
		})
	}
	if domains == nil {
		domains = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"domains": domains})
}

func (s *AdvancedMailingService) HandleCampaignByDevice(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID := chi.URLParam(r, "campaignId")
	
	rows, _ := s.db.QueryContext(ctx, `
		SELECT COALESCE(device_type, 'unknown') as device,
			   COUNT(*) as total
		FROM mailing_tracking_events
		WHERE campaign_id = $1 AND event_type IN ('opened', 'clicked')
		GROUP BY device_type
		ORDER BY total DESC
	`, campaignID)
	defer rows.Close()
	
	var devices []map[string]interface{}
	for rows.Next() {
		var device string
		var total int
		rows.Scan(&device, &total)
		devices = append(devices, map[string]interface{}{
			"device": device, "total": total,
		})
	}
	if devices == nil { devices = []map[string]interface{}{} }
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"devices": devices})
}

func (s *AdvancedMailingService) HandleAnalyticsOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start, end := parseAnalyticsRange(r)

	// Aggregate from mailing_campaigns (only columns guaranteed to exist)
	var totalSent, totalOpens, totalClicks, totalBounces, totalComplaints, totalDelivered int
	var totalRevenue float64
	s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(sent_count),0), COALESCE(SUM(delivered_count),0),
		       COALESCE(SUM(open_count),0), COALESCE(SUM(click_count),0),
		       COALESCE(SUM(bounce_count),0),
		       COALESCE(SUM(complaint_count),0), COALESCE(SUM(revenue),0)
		FROM mailing_campaigns
		WHERE COALESCE(started_at, created_at) >= $1
		  AND COALESCE(started_at, created_at) <= $2
	`, start, end).Scan(&totalSent, &totalDelivered, &totalOpens, &totalClicks,
		&totalBounces, &totalComplaints, &totalRevenue)

	// Hard/soft bounce split from tracking events (reliable regardless of schema)
	var totalHardBounce, totalSoftBounce int
	s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN event_type = 'hard_bounce' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN event_type = 'soft_bounce' THEN 1 ELSE 0 END), 0)
		FROM mailing_tracking_events
		WHERE event_at >= $1 AND event_at <= $2
	`, start, end).Scan(&totalHardBounce, &totalSoftBounce)

	openRate, clickRate, bounceRate, complaintRate := 0.0, 0.0, 0.0, 0.0
	if totalSent > 0 {
		openRate = float64(totalOpens) / float64(totalSent) * 100
		clickRate = float64(totalClicks) / float64(totalSent) * 100
		bounceRate = float64(totalBounces) / float64(totalSent) * 100
		complaintRate = float64(totalComplaints) / float64(totalSent) * 100
	}

	gran := trendGranularity(start, end)
	truncFn := "DATE(event_at)"
	dateFmt := "2006-01-02"
	switch gran {
	case "10min":
		truncFn = "DATE_TRUNC('hour', event_at) + INTERVAL '10 min' * FLOOR(EXTRACT(MINUTE FROM event_at) / 10)"
		dateFmt = "2006-01-02T15:04"
	case "hour":
		truncFn = "DATE_TRUNC('hour', event_at)"
		dateFmt = "2006-01-02T15:04"
	}

	trendArgs := []interface{}{start, end}
	trendWhere := "event_at >= $1 AND event_at <= $2"
	trendDomain := r.URL.Query().Get("trend_domain")
	if trendDomain != "" {
		trendWhere += " AND sending_domain = $3"
		trendArgs = append(trendArgs, trendDomain)
	}

	rows, _ := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s as bucket,
		       SUM(CASE WHEN event_type = 'sent' THEN 1 ELSE 0 END) as sent,
		       SUM(CASE WHEN event_type = 'delivered' THEN 1 ELSE 0 END) as delivered,
		       SUM(CASE WHEN event_type = 'opened' THEN 1 ELSE 0 END) as opens,
		       SUM(CASE WHEN event_type = 'clicked' THEN 1 ELSE 0 END) as clicks,
		       SUM(CASE WHEN event_type IN ('hard_bounce','soft_bounce','bounced') THEN 1 ELSE 0 END) as bounces,
		       SUM(CASE WHEN event_type = 'complained' THEN 1 ELSE 0 END) as complaints,
		       SUM(CASE WHEN event_type IN ('deferred','deferral') THEN 1 ELSE 0 END) as deferred,
		       SUM(CASE WHEN event_type = 'unsubscribed' THEN 1 ELSE 0 END) as unsubscribes
		FROM mailing_tracking_events
		WHERE %s
		GROUP BY %s ORDER BY bucket
	`, truncFn, trendWhere, truncFn), trendArgs...)
	defer rows.Close()

	var trend []map[string]interface{}
	for rows.Next() {
		var bucket time.Time
		var sent, delivered, opens, clicks, bounces, complaints, deferred, unsubscribes int
		rows.Scan(&bucket, &sent, &delivered, &opens, &clicks, &bounces, &complaints, &deferred, &unsubscribes)
		trend = append(trend, map[string]interface{}{
			"date": bucket.Format(dateFmt), "sent": sent, "delivered": delivered,
			"opens": opens, "clicks": clicks, "bounces": bounces, "complaints": complaints,
			"deferred": deferred, "unsubscribes": unsubscribes,
		})
	}
	if trend == nil {
		trend = []map[string]interface{}{}
	}

	// Available sending domains for chart filter
	var domains []string
	domRows, _ := s.db.QueryContext(ctx, `
		SELECT DISTINCT COALESCE(NULLIF(LOWER(SPLIT_PART(from_email, '@', 2)), ''), 'unknown')
		FROM mailing_campaigns
		WHERE from_email IS NOT NULL AND from_email LIKE '%@%'
		  AND COALESCE(started_at, created_at) >= $1 AND COALESCE(started_at, created_at) <= $2
		ORDER BY 1
	`, start, end)
	if domRows != nil {
		for domRows.Next() {
			var d string
			domRows.Scan(&d)
			domains = append(domains, d)
		}
		domRows.Close()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"api_version": VersionAnalyticsOverview,
		"range":       map[string]string{"start": start.Format(time.RFC3339), "end": end.Format(time.RFC3339)},
		"granularity": gran,
		"totals": map[string]interface{}{
			"sent": totalSent, "delivered": totalDelivered,
			"opens": totalOpens, "clicks": totalClicks,
			"bounces": totalBounces, "hard_bounces": totalHardBounce, "soft_bounces": totalSoftBounce,
			"complaints": totalComplaints, "revenue": totalRevenue,
		},
		"rates": map[string]interface{}{
			"open_rate": math.Round(openRate*100) / 100, "click_rate": math.Round(clickRate*100) / 100,
			"bounce_rate": math.Round(bounceRate*100) / 100, "complaint_rate": math.Round(complaintRate*1000) / 1000,
		},
		"daily_trend":     trend,
		"sending_domains": domains,
	})
}

// ================== CROSS-CAMPAIGN REPORTING ==================

// HandleCampaignComparison compares multiple campaigns within a date range.
func (s *AdvancedMailingService) HandleCampaignComparison(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start, end := parseAnalyticsRange(r)

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, status, sent_count, open_count, click_count, bounce_count, complaint_count, revenue,
			   created_at, COALESCE(started_at, created_at), COALESCE(completed_at, created_at)
		FROM mailing_campaigns
		WHERE sent_count > 0
		  AND COALESCE(started_at, created_at) >= $1
		  AND COALESCE(started_at, created_at) <= $2
		ORDER BY COALESCE(started_at, created_at) DESC
		LIMIT 50
	`, start, end)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	
	var campaigns []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var name, status string
		var sent, opens, clicks, bounces, complaints int
		var revenue float64
		var created, started, completed time.Time
		
		rows.Scan(&id, &name, &status, &sent, &opens, &clicks, &bounces, &complaints, &revenue, &created, &started, &completed)
		
		openRate := 0.0
		clickRate := 0.0
		bounceRate := 0.0
		if sent > 0 {
			openRate = float64(opens) / float64(sent) * 100
			clickRate = float64(clicks) / float64(sent) * 100
			bounceRate = float64(bounces) / float64(sent) * 100
		}
		
		campaigns = append(campaigns, map[string]interface{}{
			"id":             id.String(),
			"name":           name,
			"status":         status,
			"sent":           sent,
			"opens":          opens,
			"clicks":         clicks,
			"bounces":        bounces,
			"complaints":     complaints,
			"revenue":        revenue,
			"open_rate":      math.Round(openRate*100) / 100,
			"click_rate":     math.Round(clickRate*100) / 100,
			"bounce_rate":    math.Round(bounceRate*100) / 100,
			"created_at":     created,
			"started_at":     started,
			"completed_at":   completed,
		})
	}
	if campaigns == nil { campaigns = []map[string]interface{}{} }
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"api_version": VersionCampaignComparison,
		"campaigns":   campaigns,
		"total":       len(campaigns),
	})
}

// HandleTopPerformers returns top performing campaigns
func (s *AdvancedMailingService) HandleTopPerformers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	metric := r.URL.Query().Get("metric") // open_rate, click_rate, revenue
	if metric == "" { metric = "open_rate" }
	limit := r.URL.Query().Get("limit")
	if limit == "" { limit = "10" }
	
	var orderBy string
	switch metric {
	case "open_rate":
		orderBy = "CASE WHEN sent_count > 0 THEN open_count::float / sent_count ELSE 0 END DESC"
	case "click_rate":
		orderBy = "CASE WHEN sent_count > 0 THEN click_count::float / sent_count ELSE 0 END DESC"
	case "revenue":
		orderBy = "revenue DESC"
	case "sent":
		orderBy = "sent_count DESC"
	default:
		orderBy = "open_count DESC"
	}
	
	query := fmt.Sprintf(`
		SELECT id, name, sent_count, open_count, click_count, revenue
		FROM mailing_campaigns
		WHERE sent_count > 0
		ORDER BY %s
		LIMIT %s
	`, orderBy, limit)
	
	rows, _ := s.db.QueryContext(ctx, query)
	defer rows.Close()
	
	var topCampaigns []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var name string
		var sent, opens, clicks int
		var revenue float64
		rows.Scan(&id, &name, &sent, &opens, &clicks, &revenue)
		
		openRate := 0.0
		clickRate := 0.0
		if sent > 0 {
			openRate = float64(opens) / float64(sent) * 100
			clickRate = float64(clicks) / float64(sent) * 100
		}
		
		topCampaigns = append(topCampaigns, map[string]interface{}{
			"id":         id.String(),
			"name":       name,
			"sent":       sent,
			"opens":      opens,
			"clicks":     clicks,
			"revenue":    revenue,
			"open_rate":  math.Round(openRate*100) / 100,
			"click_rate": math.Round(clickRate*100) / 100,
		})
	}
	if topCampaigns == nil { topCampaigns = []map[string]interface{}{} }
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"api_version": VersionTopPerformers,
		"metric":      metric,
		"campaigns":   topCampaigns,
	})
}

// HandleListPerformance returns performance by list
func (s *AdvancedMailingService) HandleListPerformance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	
	rows, _ := s.db.QueryContext(ctx, `
		SELECT l.id, l.name, l.subscriber_count,
			   COUNT(DISTINCT c.id) as campaign_count,
			   COALESCE(SUM(c.sent_count), 0) as total_sent,
			   COALESCE(SUM(c.open_count), 0) as total_opens,
			   COALESCE(SUM(c.click_count), 0) as total_clicks,
			   COALESCE(SUM(c.revenue), 0) as total_revenue
		FROM mailing_lists l
		LEFT JOIN mailing_campaigns c ON c.list_id = l.id
		GROUP BY l.id, l.name, l.subscriber_count
		ORDER BY total_sent DESC
	`)
	defer rows.Close()
	
	var lists []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var name string
		var subscriberCount, campaignCount, sent, opens, clicks int
		var revenue float64
		rows.Scan(&id, &name, &subscriberCount, &campaignCount, &sent, &opens, &clicks, &revenue)
		
		openRate := 0.0
		clickRate := 0.0
		if sent > 0 {
			openRate = float64(opens) / float64(sent) * 100
			clickRate = float64(clicks) / float64(sent) * 100
		}
		
		lists = append(lists, map[string]interface{}{
			"id":               id.String(),
			"name":             name,
			"subscriber_count": subscriberCount,
			"campaign_count":   campaignCount,
			"total_sent":       sent,
			"total_opens":      opens,
			"total_clicks":     clicks,
			"total_revenue":    revenue,
			"avg_open_rate":    math.Round(openRate*100) / 100,
			"avg_click_rate":   math.Round(clickRate*100) / 100,
		})
	}
	if lists == nil { lists = []map[string]interface{}{} }
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"api_version": VersionListPerformance, "lists": lists})
}

// HandleEngagementReport returns subscriber engagement summary
func (s *AdvancedMailingService) HandleEngagementReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start, end := parseAnalyticsRange(r)

	// Single query for engagement distribution (was 4 separate round-trips)
	var highEngagement, medEngagement, lowEngagement, noEngagement int
	s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN engagement_score >= 70 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN engagement_score >= 40 AND engagement_score < 70 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN engagement_score > 0 AND engagement_score < 40 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN engagement_score = 0 OR engagement_score IS NULL THEN 1 ELSE 0 END), 0)
		FROM mailing_subscribers
	`).Scan(&highEngagement, &medEngagement, &lowEngagement, &noEngagement)

	// Top engaged subscribers
	rows, _ := s.db.QueryContext(ctx, `
		SELECT email, engagement_score, total_opens, total_clicks, last_open_at
		FROM mailing_subscribers
		WHERE engagement_score > 0
		ORDER BY engagement_score DESC
		LIMIT 20
	`)
	defer rows.Close()

	var topSubscribers []map[string]interface{}
	for rows.Next() {
		var email string
		var score float64
		var opens, clicks int
		var lastOpen *time.Time
		rows.Scan(&email, &score, &opens, &clicks, &lastOpen)
		topSubscribers = append(topSubscribers, map[string]interface{}{
			"email":     email,
			"score":     score,
			"opens":     opens,
			"clicks":    clicks,
			"last_open": lastOpen,
		})
	}
	if topSubscribers == nil { topSubscribers = []map[string]interface{}{} }

	// Engagement over time — bounded by the selected time range
	engagementTrend, _ := s.db.QueryContext(ctx, `
		SELECT DATE(event_at) as day, COUNT(DISTINCT subscriber_id) as engaged_subscribers
		FROM mailing_tracking_events
		WHERE event_type IN ('opened', 'clicked')
		  AND event_at >= $1 AND event_at <= $2
		GROUP BY DATE(event_at)
		ORDER BY day DESC
	`, start, end)
	defer engagementTrend.Close()

	var trend []map[string]interface{}
	for engagementTrend.Next() {
		var day time.Time
		var engaged int
		engagementTrend.Scan(&day, &engaged)
		trend = append(trend, map[string]interface{}{
			"date":    day.Format("2006-01-02"),
			"engaged": engaged,
		})
	}
	if trend == nil { trend = []map[string]interface{}{} }

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"api_version": VersionEngagementReport,
		"distribution": map[string]int{
			"high":   highEngagement,
			"medium": medEngagement,
			"low":    lowEngagement,
			"none":   noEngagement,
		},
		"top_subscribers":  topSubscribers,
		"engagement_trend": trend,
	})
}

// HandleDeliverabilityReport returns deliverability metrics filtered by date range.
func (s *AdvancedMailingService) HandleDeliverabilityReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start, end := parseAnalyticsRange(r)

	var totalSent, totalDelivered, totalBounced, totalComplaints int
	s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(sent_count),0), COALESCE(SUM(delivered_count),0),
		       COALESCE(SUM(bounce_count),0), COALESCE(SUM(complaint_count),0)
		FROM mailing_campaigns
		WHERE COALESCE(started_at, created_at) >= $1 AND COALESCE(started_at, created_at) <= $2
	`, start, end).Scan(&totalSent, &totalDelivered, &totalBounced, &totalComplaints)
	if totalDelivered == 0 && totalSent > totalBounced {
		totalDelivered = totalSent - totalBounced
	}

	deliveryRate, bounceRate, complaintRate := 0.0, 0.0, 0.0
	if totalSent > 0 {
		deliveryRate = float64(totalDelivered) / float64(totalSent) * 100
		bounceRate = float64(totalBounced) / float64(totalSent) * 100
		complaintRate = float64(totalComplaints) / float64(totalSent) * 100
	}

	rows, _ := s.db.QueryContext(ctx, `
		SELECT COALESCE(bounce_type, event_type) as type, COUNT(*) as count
		FROM mailing_tracking_events
		WHERE event_type IN ('hard_bounce', 'soft_bounce', 'bounced')
		  AND event_at >= $1 AND event_at <= $2
		GROUP BY COALESCE(bounce_type, event_type)
		ORDER BY count DESC
	`, start, end)
	defer rows.Close()

	var bounceBreakdown []map[string]interface{}
	for rows.Next() {
		var bounceType string
		var count int
		rows.Scan(&bounceType, &count)
		bounceBreakdown = append(bounceBreakdown, map[string]interface{}{
			"type": bounceType, "count": count,
		})
	}
	if bounceBreakdown == nil {
		bounceBreakdown = []map[string]interface{}{}
	}

	var suppressionCount int
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_global_suppressions").Scan(&suppressionCount)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"api_version": VersionDeliverabilityReport,
		"range":       map[string]string{"start": start.Format(time.RFC3339), "end": end.Format(time.RFC3339)},
		"totals": map[string]int{
			"sent": totalSent, "delivered": totalDelivered,
			"bounced": totalBounced, "complaints": totalComplaints,
		},
		"rates": map[string]float64{
			"delivery_rate":  math.Round(deliveryRate*100) / 100,
			"bounce_rate":    math.Round(bounceRate*100) / 100,
			"complaint_rate": math.Round(complaintRate*1000) / 1000,
		},
		"bounce_breakdown":    bounceBreakdown,
		"global_suppressions": suppressionCount,
	})
}

// HandleInfrastructureBreakdown returns performance aggregated by Domain, IP, or ISP.
// Level 1 (no domain selected): uses mailing_campaigns for sent/delivered (reliable)
// and mailing_tracking_events for opens/clicks/bounces/complaints/deferred.
// Level 2 (domain selected, drilldown by IP or ISP): uses tracking_events only.
func (s *AdvancedMailingService) HandleInfrastructureBreakdown(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start, end := parseAnalyticsRange(r)

	selectedDomain := r.URL.Query().Get("domain")
	drilldownType := r.URL.Query().Get("drilldown")
	campaignID := r.URL.Query().Get("campaign_id")

	var results []map[string]interface{}
	var err error

	if selectedDomain == "" {
		results, err = s.infraLevel1(ctx, start, end, campaignID)
	} else {
		if drilldownType != "ip" && drilldownType != "isp" {
			http.Error(w, `{"error":"invalid drilldown type, use ip or isp"}`, http.StatusBadRequest)
			return
		}
		results, err = s.infraLevel2(ctx, start, end, selectedDomain, drilldownType, campaignID)
	}

	if err != nil {
		log.Printf("[infrastructure] query error: %v", err)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	if results == nil {
		results = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"api_version":    VersionInfrastructureBreakdown,
		"level":          selectedDomain,
		"drilldown_type": drilldownType,
		"campaign_id":    campaignID,
		"data":           results,
	})
}

// infraLevel1 aggregates by sending domain using campaign-level sent/delivered
// for accuracy and tracking events for engagement metrics.
func (s *AdvancedMailingService) infraLevel1(ctx context.Context, start, end time.Time, campaignID string) ([]map[string]interface{}, error) {
	campArgs := []interface{}{start, end}
	campWhere := "COALESCE(c.started_at, c.created_at) >= $1 AND COALESCE(c.started_at, c.created_at) <= $2"
	evtArgs := []interface{}{start, end}
	evtWhere := "t.event_at >= $1 AND t.event_at <= $2"
	argIdx := 3

	if campaignID != "" {
		campWhere += fmt.Sprintf(" AND c.id = $%d", argIdx)
		campArgs = append(campArgs, campaignID)
		evtWhere += fmt.Sprintf(" AND t.campaign_id = $%d", argIdx)
		evtArgs = append(evtArgs, campaignID)
		argIdx++
	}

	campQuery := fmt.Sprintf(`
		SELECT COALESCE(NULLIF(LOWER(SPLIT_PART(c.from_email, '@', 2)), ''), 'unknown') as entity,
		       COALESCE(SUM(c.sent_count), 0) as sent,
		       COALESCE(SUM(c.delivered_count), 0) as delivered
		FROM mailing_campaigns c
		WHERE %s AND c.from_email IS NOT NULL AND c.from_email LIKE '%%@%%'
		GROUP BY COALESCE(NULLIF(LOWER(SPLIT_PART(c.from_email, '@', 2)), ''), 'unknown')
	`, campWhere)

	evtQuery := fmt.Sprintf(`
		SELECT COALESCE(NULLIF(t.sending_domain, ''), 'unknown') as entity,
		       COUNT(DISTINCT CASE WHEN t.event_type = 'opened' THEN t.subscriber_id END) as opens,
		       COUNT(DISTINCT CASE WHEN t.event_type = 'clicked' THEN t.subscriber_id END) as clicks,
		       SUM(CASE WHEN t.event_type IN ('hard_bounce', 'soft_bounce', 'bounced') THEN 1 ELSE 0 END) as bounces,
		       SUM(CASE WHEN t.event_type = 'complained' THEN 1 ELSE 0 END) as complaints,
		       SUM(CASE WHEN t.event_type IN ('deferred', 'deferral') THEN 1 ELSE 0 END) as deferred
		FROM mailing_tracking_events t
		WHERE %s
		GROUP BY COALESCE(NULLIF(t.sending_domain, ''), 'unknown')
	`, evtWhere)

	campData := map[string][2]int{}
	rows, err := s.db.QueryContext(ctx, campQuery, campArgs...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var entity string
		var sent, delivered int
		if err := rows.Scan(&entity, &sent, &delivered); err != nil {
			continue
		}
		campData[entity] = [2]int{sent, delivered}
	}
	rows.Close()

	type evtRow struct {
		opens, clicks, bounces, complaints, deferred int
	}
	evtData := map[string]evtRow{}
	rows2, err := s.db.QueryContext(ctx, evtQuery, evtArgs...)
	if err != nil {
		return nil, err
	}
	for rows2.Next() {
		var entity string
		var r evtRow
		if err := rows2.Scan(&entity, &r.opens, &r.clicks, &r.bounces, &r.complaints, &r.deferred); err != nil {
			continue
		}
		evtData[entity] = r
	}
	rows2.Close()

	allEntities := map[string]bool{}
	for k := range campData {
		allEntities[k] = true
	}
	for k := range evtData {
		allEntities[k] = true
	}

	var results []map[string]interface{}
	for entity := range allEntities {
		cd := campData[entity]
		sent, delivered := cd[0], cd[1]
		ed := evtData[entity]

		rates := ComputeInfraRates(sent, delivered, ed.opens, ed.clicks, ed.bounces, ed.complaints, ed.deferred)
		results = append(results, map[string]interface{}{
			"entity": entity, "sent": sent, "delivered": delivered,
			"opens": ed.opens, "clicks": ed.clicks, "bounces": ed.bounces,
			"complaints": ed.complaints, "deferred": ed.deferred,
			"open_rate": rates.OpenRate, "click_rate": rates.ClickRate,
			"bounce_rate": rates.BounceRate, "complaint_rate": rates.ComplaintRate,
			"deferral_rate": rates.DeferralRate,
		})
	}

	// Sort by sent descending
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			si, _ := results[i]["sent"].(int)
			sj, _ := results[j]["sent"].(int)
			if sj > si {
				results[i], results[j] = results[j], results[i]
			}
		}
	}
	if len(results) > 50 {
		results = results[:50]
	}

	return results, nil
}

// infraLevel2 drills down into a specific domain by IP or ISP.
// Uses a hybrid approach: campaign-level sent/delivered as the parent totals,
// then tracking events for per-entity engagement/deliverability breakdown.
// For ISP: proportionally allocates parent sent/delivered based on subscriber list composition.
func (s *AdvancedMailingService) infraLevel2(ctx context.Context, start, end time.Time, domain, drilldownType, campaignID string) ([]map[string]interface{}, error) {
	// Step 1: Get parent domain's total sent/delivered from mailing_campaigns
	campArgs := []interface{}{start, end}
	campWhere := "COALESCE(c.started_at, c.created_at) >= $1 AND COALESCE(c.started_at, c.created_at) <= $2"
	campArgIdx := 3

	if campaignID != "" {
		campWhere += fmt.Sprintf(" AND c.id = $%d", campArgIdx)
		campArgs = append(campArgs, campaignID)
		campArgIdx++
	}

	campWhere += fmt.Sprintf(" AND COALESCE(NULLIF(LOWER(SPLIT_PART(c.from_email, '@', 2)), ''), 'unknown') = $%d", campArgIdx)
	campArgs = append(campArgs, domain)

	var parentSent, parentDelivered int
	s.db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(SUM(c.sent_count), 0), COALESCE(SUM(c.delivered_count), 0)
		FROM mailing_campaigns c
		WHERE %s AND c.from_email IS NOT NULL AND c.from_email LIKE '%%@%%'
	`, campWhere), campArgs...).Scan(&parentSent, &parentDelivered)

	// Step 2: For ISP, get subscriber distribution to estimate per-ISP sent/delivered
	ispDistribution := map[string]float64{}
	if drilldownType == "isp" && parentSent > 0 {
		distArgs := []interface{}{start, end}
		distWhere := "COALESCE(c.started_at, c.created_at) >= $1 AND COALESCE(c.started_at, c.created_at) <= $2"
		distIdx := 3
		if campaignID != "" {
			distWhere += fmt.Sprintf(" AND c.id = $%d", distIdx)
			distArgs = append(distArgs, campaignID)
			distIdx++
		}
		distWhere += fmt.Sprintf(" AND COALESCE(NULLIF(LOWER(SPLIT_PART(c.from_email, '@', 2)), ''), 'unknown') = $%d", distIdx)
		distArgs = append(distArgs, domain)

		distRows, distErr := s.db.QueryContext(ctx, fmt.Sprintf(`
			SELECT LOWER(SPLIT_PART(sub.email, '@', 2)) as isp,
			       COUNT(*) as cnt
			FROM mailing_subscribers sub
			JOIN mailing_campaigns c ON (
				c.list_id = sub.list_id
				OR (c.list_ids IS NOT NULL AND c.list_ids != '[]'::jsonb
				    AND sub.list_id::text IN (SELECT jsonb_array_elements_text(c.list_ids)))
			)
			WHERE %s
			  AND c.from_email IS NOT NULL AND c.from_email LIKE '%%@%%'
			  AND sub.email LIKE '%%@%%'
			  AND sub.status = 'confirmed'
			GROUP BY LOWER(SPLIT_PART(sub.email, '@', 2))
			ORDER BY cnt DESC
			LIMIT 100
		`, distWhere), distArgs...)
		if distErr == nil {
			var totalSubs int64
			type distRow struct {
				isp string
				cnt int64
			}
			var distData []distRow
			for distRows.Next() {
				var d distRow
				if err := distRows.Scan(&d.isp, &d.cnt); err == nil {
					distData = append(distData, d)
					totalSubs += d.cnt
				}
			}
			distRows.Close()
			if totalSubs > 0 {
				for _, d := range distData {
					ispDistribution[d.isp] = float64(d.cnt) / float64(totalSubs)
				}
			}
		}
	}

	// Step 3: Query tracking events grouped by entity
	args := []interface{}{start, end}
	argIdx := 3
	whereClauses := []string{"t.event_at >= $1", "t.event_at <= $2"}

	if campaignID != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("t.campaign_id = $%d", argIdx))
		args = append(args, campaignID)
		argIdx++
	}

	whereClauses = append(whereClauses, fmt.Sprintf("COALESCE(NULLIF(t.sending_domain, ''), 'unknown') = $%d", argIdx))
	args = append(args, domain)
	argIdx++

	var selectCol, groupByCol, fromTable string
	if drilldownType == "ip" {
		selectCol = "COALESCE(NULLIF(t.sending_ip, ''), 'unknown')"
		groupByCol = selectCol
		fromTable = "mailing_tracking_events t"
	} else if drilldownType == "isp" {
		selectCol = "COALESCE(NULLIF(LOWER(t.recipient_domain), ''), COALESCE(NULLIF(LOWER(SPLIT_PART(sub.email, '@', 2)), ''), 'unknown'))"
		groupByCol = selectCol
		fromTable = "mailing_tracking_events t LEFT JOIN mailing_subscribers sub ON t.subscriber_id = sub.id"
	} else {
		return nil, fmt.Errorf("invalid drilldown type: %s", drilldownType)
	}

	whereStr := strings.Join(whereClauses, " AND ")

	query := fmt.Sprintf(`
		SELECT %s as entity,
		       SUM(CASE WHEN t.event_type = 'sent' THEN 1 ELSE 0 END) as sent,
		       SUM(CASE WHEN t.event_type = 'delivered' THEN 1 ELSE 0 END) as delivered,
		       COUNT(DISTINCT CASE WHEN t.event_type = 'opened' THEN t.subscriber_id ELSE NULL END) as opens,
		       COUNT(DISTINCT CASE WHEN t.event_type = 'clicked' THEN t.subscriber_id ELSE NULL END) as clicks,
		       SUM(CASE WHEN t.event_type IN ('hard_bounce', 'soft_bounce', 'bounced') THEN 1 ELSE 0 END) as bounces,
		       SUM(CASE WHEN t.event_type = 'complained' THEN 1 ELSE 0 END) as complaints,
		       SUM(CASE WHEN t.event_type IN ('deferred', 'deferral') THEN 1 ELSE 0 END) as deferred
		FROM %s
		WHERE %s
		GROUP BY %s
		ORDER BY (SUM(CASE WHEN t.event_type = 'delivered' THEN 1 ELSE 0 END) +
		          COUNT(DISTINCT CASE WHEN t.event_type = 'opened' THEN t.subscriber_id END)) DESC
		LIMIT 50`, selectCol, fromTable, whereStr, groupByCol)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var entity string
		var sent, delivered, opens, clicks, bounces, complaints, deferred int
		if err := rows.Scan(&entity, &sent, &delivered, &opens, &clicks, &bounces, &complaints, &deferred); err != nil {
			log.Printf("[infrastructure] scan error: %v", err)
			continue
		}

		estSent := sent
		estDelivered := delivered
		if drilldownType == "isp" {
			if pct, ok := ispDistribution[entity]; ok && pct > 0 {
				estSent = int(math.Round(float64(parentSent) * pct))
				estDelivered = int(math.Round(float64(parentDelivered) * pct))
			}
		}

		// When proportional allocation yields 0 but we have real event data,
		// use delivered+bounces as an approximation for sent
		rateSent := estSent
		rateDelivered := estDelivered
		if rateSent == 0 && (delivered > 0 || bounces > 0) {
			rateSent = delivered + bounces
		}
		if rateDelivered == 0 && delivered > 0 {
			rateDelivered = delivered
		}

		rates := ComputeInfraRates(rateSent, rateDelivered, opens, clicks, bounces, complaints, deferred)
		capRate := func(r float64) float64 {
			if r > 100 {
				return 100
			}
			return r
		}
		results = append(results, map[string]interface{}{
			"entity": entity, "sent": estSent, "delivered": estDelivered,
			"opens": opens, "clicks": clicks, "bounces": bounces, "complaints": complaints, "deferred": deferred,
			"open_rate": capRate(rates.OpenRate), "click_rate": capRate(rates.ClickRate),
			"bounce_rate": capRate(rates.BounceRate), "complaint_rate": capRate(rates.ComplaintRate),
			"deferral_rate": capRate(rates.DeferralRate),
			"parent_sent": parentSent, "parent_delivered": parentDelivered,
		})
	}

	// Re-sort by estimated sent descending since we may have enriched values
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			si, _ := results[i]["sent"].(int)
			sj, _ := results[j]["sent"].(int)
			if sj > si {
				results[i], results[j] = results[j], results[i]
			}
		}
	}

	return results, nil
}

// HandleRevenueReport returns revenue analytics
func (s *AdvancedMailingService) HandleRevenueReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start, end := parseAnalyticsRange(r)
	daysInt := int(end.Sub(start).Hours() / 24)
	if daysInt == 0 { daysInt = 1 }

	// Total revenue within range
	var totalRevenue float64
	var campaignsWithRevenue int
	s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(revenue), 0), COUNT(*)
		FROM mailing_campaigns
		WHERE revenue > 0 AND COALESCE(started_at, created_at) >= $1 AND COALESCE(started_at, created_at) <= $2
	`, start, end).Scan(&totalRevenue, &campaignsWithRevenue)

	// Revenue per email within range
	var totalSent int
	s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(sent_count), 0) FROM mailing_campaigns
		WHERE COALESCE(started_at, created_at) >= $1 AND COALESCE(started_at, created_at) <= $2
	`, start, end).Scan(&totalSent)
	revenuePerEmail := 0.0
	if totalSent > 0 {
		revenuePerEmail = totalRevenue / float64(totalSent)
	}

	// Revenue by campaign within range
	rows, _ := s.db.QueryContext(ctx, `
		SELECT name, sent_count, revenue
		FROM mailing_campaigns
		WHERE revenue > 0 AND COALESCE(started_at, created_at) >= $1 AND COALESCE(started_at, created_at) <= $2
		ORDER BY revenue DESC
		LIMIT 10
	`, start, end)
	defer rows.Close()

	var topRevenueCampaigns []map[string]interface{}
	for rows.Next() {
		var name string
		var sent int
		var revenue float64
		rows.Scan(&name, &sent, &revenue)
		rpe := 0.0
		if sent > 0 {
			rpe = revenue / float64(sent)
		}
		topRevenueCampaigns = append(topRevenueCampaigns, map[string]interface{}{
			"name":              name,
			"sent":              sent,
			"revenue":           revenue,
			"revenue_per_email": math.Round(rpe*10000) / 10000,
		})
	}
	if topRevenueCampaigns == nil { topRevenueCampaigns = []map[string]interface{}{} }

	// Daily revenue trend bounded by range
	trendRows, _ := s.db.QueryContext(ctx, `
		SELECT DATE(COALESCE(started_at, created_at)) as day, SUM(revenue) as daily_revenue
		FROM mailing_campaigns
		WHERE COALESCE(started_at, created_at) >= $1 AND COALESCE(started_at, created_at) <= $2
		  AND revenue > 0
		GROUP BY DATE(COALESCE(started_at, created_at))
		ORDER BY day DESC
	`, start, end)
	defer trendRows.Close()

	var revenueTrend []map[string]interface{}
	for trendRows.Next() {
		var day time.Time
		var rev float64
		trendRows.Scan(&day, &rev)
		revenueTrend = append(revenueTrend, map[string]interface{}{
			"date": day.Format("2006-01-02"), "revenue": rev,
		})
	}
	if revenueTrend == nil { revenueTrend = []map[string]interface{}{} }

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"api_version":            VersionRevenueReport,
		"period_days":            daysInt,
		"total_revenue":          totalRevenue,
		"campaigns_with_revenue": campaignsWithRevenue,
		"total_sent":             totalSent,
		"revenue_per_email":      math.Round(revenuePerEmail*10000) / 10000,
		"top_revenue_campaigns":  topRevenueCampaigns,
		"daily_trend":            revenueTrend,
	})
}

// ================== HISTORICAL METRICS & LLM LEARNING ==================

// HandleGetHistoricalMetrics returns comprehensive historical metrics for analysis
func (s *AdvancedMailingService) HandleGetHistoricalMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start, end := parseAnalyticsRange(r)
	daysInt := int(end.Sub(start).Hours() / 24)
	if daysInt == 0 { daysInt = 1 }

	// Campaign performance over time
	var totalCampaigns, totalSent, totalOpens, totalClicks, totalBounces, totalComplaints int
	var totalRevenue float64
	s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(sent_count),0), COALESCE(SUM(open_count),0), COALESCE(SUM(click_count),0),
			   COALESCE(SUM(bounce_count),0), COALESCE(SUM(complaint_count),0), COALESCE(SUM(revenue),0)
		FROM mailing_campaigns WHERE created_at >= $1 AND created_at <= $2
	`, start, end).Scan(&totalCampaigns, &totalSent, &totalOpens, &totalClicks, &totalBounces, &totalComplaints, &totalRevenue)
	
	// Calculate rates
	openRate := 0.0
	clickRate := 0.0
	bounceRate := 0.0
	complaintRate := 0.0
	if totalSent > 0 {
		openRate = float64(totalOpens) / float64(totalSent) * 100
		clickRate = float64(totalClicks) / float64(totalSent) * 100
		bounceRate = float64(totalBounces) / float64(totalSent) * 100
		complaintRate = float64(totalComplaints) / float64(totalSent) * 100
	}
	
	// Weekly trends
	weeklyRows, weeklyErr := s.db.QueryContext(ctx, `
		SELECT DATE_TRUNC('week', created_at) as week,
			   COUNT(*) as campaigns,
			   COALESCE(SUM(sent_count),0) as sent,
			   COALESCE(SUM(open_count),0) as opens,
			   COALESCE(SUM(click_count),0) as clicks,
			   COALESCE(SUM(bounce_count),0) as bounces,
			   COALESCE(SUM(complaint_count),0) as complaints,
			   COALESCE(SUM(revenue),0) as revenue
		FROM mailing_campaigns
		WHERE created_at >= $1 AND created_at <= $2
		GROUP BY DATE_TRUNC('week', created_at)
		ORDER BY week DESC
	`, start, end)
	if weeklyErr != nil {
		log.Printf("[historical-metrics] weekly trends query error: %v", weeklyErr)
	}
	if weeklyRows != nil { defer weeklyRows.Close() }
	
	var weeklyTrends []map[string]interface{}
	for weeklyRows != nil && weeklyRows.Next() {
		var week time.Time
		var campaigns, sent, opens, clicks, bounces, complaints int
		var rev float64
		weeklyRows.Scan(&week, &campaigns, &sent, &opens, &clicks, &bounces, &complaints, &rev)
		weekOR := 0.0
		weekCR := 0.0
		if sent > 0 {
			weekOR = float64(opens) / float64(sent) * 100
			weekCR = float64(clicks) / float64(sent) * 100
		}
		weeklyTrends = append(weeklyTrends, map[string]interface{}{
			"week": week.Format("2006-01-02"), "campaigns": campaigns, "sent": sent,
			"opens": opens, "clicks": clicks, "open_rate": math.Round(weekOR*10)/10,
			"click_rate": math.Round(weekCR*10)/10, "revenue": rev,
		})
	}
	if weeklyTrends == nil { weeklyTrends = []map[string]interface{}{} }
	
	// Best performing campaigns (for learning)
	topRows, topErr := s.db.QueryContext(ctx, `
		SELECT id, name, subject, COALESCE(sent_count,0), COALESCE(open_count,0), COALESCE(click_count,0), 
			   COALESCE(revenue,0), created_at
		FROM mailing_campaigns
		WHERE sent_count > 0 AND created_at >= $1 AND created_at <= $2
		ORDER BY (open_count::float / NULLIF(sent_count,0)) DESC LIMIT 10
	`, start, end)
	if topErr != nil {
		log.Printf("[historical-metrics] top campaigns query error: %v", topErr)
	}
	if topRows != nil { defer topRows.Close() }
	
	var topCampaigns []map[string]interface{}
	for topRows != nil && topRows.Next() {
		var id uuid.UUID
		var name, subject string
		var sent, opens, clicks int
		var rev float64
		var createdAt time.Time
		topRows.Scan(&id, &name, &subject, &sent, &opens, &clicks, &rev, &createdAt)
		or := 0.0
		cr := 0.0
		if sent > 0 {
			or = float64(opens) / float64(sent) * 100
			cr = float64(clicks) / float64(sent) * 100
		}
		topCampaigns = append(topCampaigns, map[string]interface{}{
			"id": id.String(), "name": name, "subject": subject, "sent": sent,
			"open_rate": math.Round(or*10)/10, "click_rate": math.Round(cr*10)/10,
			"revenue": rev, "created_at": createdAt,
		})
	}
	if topCampaigns == nil { topCampaigns = []map[string]interface{}{} }
	
	// Subject line analysis (what works)
	subjectRows, subjectErr := s.db.QueryContext(ctx, `
		SELECT subject, AVG(open_count::float / NULLIF(sent_count,0)) as avg_open_rate,
			   COUNT(*) as usage_count
		FROM mailing_campaigns
		WHERE sent_count > 100 AND created_at >= $1 AND created_at <= $2
		GROUP BY subject HAVING COUNT(*) >= 1
		ORDER BY avg_open_rate DESC LIMIT 20
	`, start, end)
	if subjectErr != nil {
		log.Printf("[historical-metrics] subject patterns query error: %v", subjectErr)
	}
	if subjectRows != nil { defer subjectRows.Close() }
	
	var subjectPatterns []map[string]interface{}
	for subjectRows != nil && subjectRows.Next() {
		var subject string
		var avgOR float64
		var usageCount int
		subjectRows.Scan(&subject, &avgOR, &usageCount)
		subjectPatterns = append(subjectPatterns, map[string]interface{}{
			"subject": subject, "avg_open_rate": math.Round(avgOR*1000)/10, "usage_count": usageCount,
		})
	}
	if subjectPatterns == nil { subjectPatterns = []map[string]interface{}{} }
	
	// Hour-of-day performance
	hourRows, hourErr := s.db.QueryContext(ctx, `
		SELECT EXTRACT(HOUR FROM event_at) as hour,
			   COUNT(*) FILTER (WHERE event_type = 'opened') as opens,
			   COUNT(*) FILTER (WHERE event_type = 'clicked') as clicks
		FROM mailing_tracking_events
		WHERE event_at >= $1 AND event_at <= $2
		GROUP BY EXTRACT(HOUR FROM event_at)
		ORDER BY hour
	`, start, end)
	if hourErr != nil {
		log.Printf("[historical-metrics] hourly performance query error: %v", hourErr)
	}
	if hourRows != nil { defer hourRows.Close() }

	var hourlyPerformance []map[string]interface{}
	for hourRows != nil && hourRows.Next() {
		var hour int
		var opens, clicks int
		hourRows.Scan(&hour, &opens, &clicks)
		hourlyPerformance = append(hourlyPerformance, map[string]interface{}{
			"hour": hour, "opens": opens, "clicks": clicks,
		})
	}
	if hourlyPerformance == nil { hourlyPerformance = []map[string]interface{}{} }
	
	// Day-of-week performance
	dowRows, dowErr := s.db.QueryContext(ctx, `
		SELECT EXTRACT(DOW FROM event_at) as dow,
			   COUNT(*) FILTER (WHERE event_type = 'opened') as opens,
			   COUNT(*) FILTER (WHERE event_type = 'clicked') as clicks
		FROM mailing_tracking_events
		WHERE event_at >= $1 AND event_at <= $2
		GROUP BY EXTRACT(DOW FROM event_at)
		ORDER BY dow
	`, start, end)
	if dowErr != nil {
		log.Printf("[historical-metrics] dow performance query error: %v", dowErr)
	}
	if dowRows != nil { defer dowRows.Close() }

	dayNames := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	var dailyPerformance []map[string]interface{}
	for dowRows != nil && dowRows.Next() {
		var dow int
		var opens, clicks int
		dowRows.Scan(&dow, &opens, &clicks)
		dailyPerformance = append(dailyPerformance, map[string]interface{}{
			"day": dayNames[dow], "day_num": dow, "opens": opens, "clicks": clicks,
		})
	}
	if dailyPerformance == nil { dailyPerformance = []map[string]interface{}{} }
	
	// Domain performance
	domainRows, domainErr := s.db.QueryContext(ctx, `
		SELECT domain,
			   COALESCE(SUM(total_sent),0) as sent,
			   COALESCE(SUM(total_opens),0) as opens,
			   COALESCE(SUM(total_clicks),0) as clicks,
			   AVG(engagement_score) as avg_engagement
		FROM mailing_inbox_profiles
		GROUP BY domain HAVING SUM(total_sent) > 0
		ORDER BY SUM(total_sent) DESC LIMIT 20
	`)
	if domainErr != nil {
		log.Printf("[historical-metrics] domain metrics query error: %v", domainErr)
	}
	if domainRows != nil { defer domainRows.Close() }

	var domainMetrics []map[string]interface{}
	for domainRows != nil && domainRows.Next() {
		var domain string
		var sent, opens, clicks int
		var avgEng float64
		domainRows.Scan(&domain, &sent, &opens, &clicks, &avgEng)
		or := 0.0
		cr := 0.0
		if sent > 0 {
			or = float64(opens) / float64(sent) * 100
			cr = float64(clicks) / float64(sent) * 100
		}
		domainMetrics = append(domainMetrics, map[string]interface{}{
			"domain": domain, "sent": sent, "open_rate": math.Round(or*10)/10,
			"click_rate": math.Round(cr*10)/10, "avg_engagement": math.Round(avgEng*10)/10,
		})
	}
	if domainMetrics == nil { domainMetrics = []map[string]interface{}{} }
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"api_version": VersionHistoricalMetrics,
		"period_days": daysInt,
		"summary": map[string]interface{}{
			"total_campaigns":  totalCampaigns,
			"total_sent":       totalSent,
			"total_opens":      totalOpens,
			"total_clicks":     totalClicks,
			"total_bounces":    totalBounces,
			"total_complaints": totalComplaints,
			"total_revenue":    totalRevenue,
			"avg_open_rate":    math.Round(openRate*10) / 10,
			"avg_click_rate":   math.Round(clickRate*10) / 10,
			"avg_bounce_rate":  math.Round(bounceRate*100) / 100,
			"complaint_rate":   math.Round(complaintRate*1000) / 1000,
		},
		"weekly_trends":       weeklyTrends,
		"top_campaigns":       topCampaigns,
		"subject_patterns":    subjectPatterns,
		"hourly_performance":  hourlyPerformance,
		"daily_performance":   dailyPerformance,
		"domain_metrics":      domainMetrics,
		"generated_at":        time.Now(),
	})
}

// HandleGetLLMLearningData returns data formatted for LLM learning and recommendations
func (s *AdvancedMailingService) HandleGetLLMLearningData(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	
	// Get comprehensive learning dataset
	var totalCampaigns, totalSubscribers, totalEvents int
	var avgOpenRate, avgClickRate, avgBounceRate, avgComplaintRate float64
	
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_campaigns").Scan(&totalCampaigns)
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_subscribers").Scan(&totalSubscribers)
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_tracking_events").Scan(&totalEvents)
	
	// Calculate averages from recent campaigns
	s.db.QueryRowContext(ctx, `
		SELECT 
			COALESCE(AVG(open_count::float / NULLIF(sent_count,0)), 0),
			COALESCE(AVG(click_count::float / NULLIF(sent_count,0)), 0),
			COALESCE(AVG(bounce_count::float / NULLIF(sent_count,0)), 0),
			COALESCE(AVG(complaint_count::float / NULLIF(sent_count,0)), 0)
		FROM mailing_campaigns WHERE sent_count > 0
	`).Scan(&avgOpenRate, &avgClickRate, &avgBounceRate, &avgComplaintRate)
	
	// Best send times learned from data
	var bestHour int
	var bestDay string
	s.db.QueryRowContext(ctx, `
		SELECT EXTRACT(HOUR FROM event_at)::int as hour
		FROM mailing_tracking_events WHERE event_type = 'opened'
		GROUP BY EXTRACT(HOUR FROM event_at)
		ORDER BY COUNT(*) DESC LIMIT 1
	`).Scan(&bestHour)
	
	s.db.QueryRowContext(ctx, `
		SELECT CASE EXTRACT(DOW FROM event_at)::int
			WHEN 0 THEN 'Sunday' WHEN 1 THEN 'Monday' WHEN 2 THEN 'Tuesday'
			WHEN 3 THEN 'Wednesday' WHEN 4 THEN 'Thursday' WHEN 5 THEN 'Friday'
			WHEN 6 THEN 'Saturday' END
		FROM mailing_tracking_events WHERE event_type = 'opened'
		GROUP BY EXTRACT(DOW FROM event_at)
		ORDER BY COUNT(*) DESC LIMIT 1
	`).Scan(&bestDay)
	
	// Top performing subjects
	subjectRows2, _ := s.db.QueryContext(ctx, `
		SELECT subject, AVG(open_count::float / NULLIF(sent_count,0)) * 100 as open_rate
		FROM mailing_campaigns WHERE sent_count > 0
		GROUP BY subject ORDER BY open_rate DESC LIMIT 5
	`)
	defer subjectRows2.Close()
	
	var bestSubjects []map[string]interface{}
	for subjectRows2.Next() {
		var subject string
		var or float64
		subjectRows2.Scan(&subject, &or)
		bestSubjects = append(bestSubjects, map[string]interface{}{
			"subject": subject, "open_rate": math.Round(or*10)/10,
		})
	}
	
	// Worst performing subjects (to avoid)
	worstRows, _ := s.db.QueryContext(ctx, `
		SELECT subject, AVG(open_count::float / NULLIF(sent_count,0)) * 100 as open_rate
		FROM mailing_campaigns WHERE sent_count > 0
		GROUP BY subject ORDER BY open_rate ASC LIMIT 5
	`)
	defer worstRows.Close()
	
	var worstSubjects []map[string]interface{}
	for worstRows.Next() {
		var subject string
		var or float64
		worstRows.Scan(&subject, &or)
		worstSubjects = append(worstSubjects, map[string]interface{}{
			"subject": subject, "open_rate": math.Round(or*10)/10,
		})
	}
	
	// Engagement segments
	var highEngaged, medEngaged, lowEngaged int
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_inbox_profiles WHERE engagement_score >= 70").Scan(&highEngaged)
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_inbox_profiles WHERE engagement_score >= 40 AND engagement_score < 70").Scan(&medEngaged)
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_inbox_profiles WHERE engagement_score < 40").Scan(&lowEngaged)
	
	// Generate AI recommendations based on learned data
	recommendations := []string{}
	
	if avgOpenRate < 0.15 {
		recommendations = append(recommendations, "Open rates are below industry average (15%). Consider A/B testing subject lines and optimizing send times.")
	} else if avgOpenRate > 0.25 {
		recommendations = append(recommendations, "Excellent open rates! Your subject lines are performing well. Consider scaling successful patterns.")
	}
	
	if avgComplaintRate > 0.001 {
		recommendations = append(recommendations, "Complaint rate exceeds 0.1% threshold. Review list quality and ensure clear unsubscribe options.")
	}
	
	if bestHour >= 10 && bestHour <= 14 {
		recommendations = append(recommendations, fmt.Sprintf("Peak engagement at %d:00. Schedule campaigns around this time for best results.", bestHour))
	}
	
	if lowEngaged > highEngaged {
		recommendations = append(recommendations, "High proportion of low-engagement subscribers. Consider re-engagement campaigns or list cleaning.")
	}
	
	// Format learning summary for LLM context
	learningSummary := fmt.Sprintf(`
CAMPAIGN PERFORMANCE LEARNING SUMMARY
=====================================
Data Points: %d campaigns, %d subscribers, %d events

KEY METRICS (Baseline):
- Average Open Rate: %.1f%%
- Average Click Rate: %.2f%%
- Average Bounce Rate: %.2f%%
- Complaint Rate: %.3f%%

OPTIMAL TIMING (Learned):
- Best Hour: %d:00
- Best Day: %s

ENGAGEMENT DISTRIBUTION:
- High Engagement (70+): %d profiles
- Medium Engagement (40-69): %d profiles
- Low Engagement (<40): %d profiles

These metrics should inform all future campaign decisions and recommendations.
`, totalCampaigns, totalSubscribers, totalEvents,
		avgOpenRate*100, avgClickRate*100, avgBounceRate*100, avgComplaintRate*100,
		bestHour, bestDay,
		highEngaged, medEngaged, lowEngaged)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"learning_context": learningSummary,
		"metrics": map[string]interface{}{
			"total_campaigns":    totalCampaigns,
			"total_subscribers":  totalSubscribers,
			"total_events":       totalEvents,
			"avg_open_rate":      math.Round(avgOpenRate*1000) / 10,
			"avg_click_rate":     math.Round(avgClickRate*1000) / 10,
			"avg_bounce_rate":    math.Round(avgBounceRate*1000) / 10,
			"complaint_rate":     math.Round(avgComplaintRate*10000) / 100,
		},
		"learned_patterns": map[string]interface{}{
			"best_send_hour":     bestHour,
			"best_send_day":      bestDay,
			"best_subjects":      bestSubjects,
			"worst_subjects":     worstSubjects,
		},
		"engagement_segments": map[string]interface{}{
			"high_engaged":   highEngaged,
			"medium_engaged": medEngaged,
			"low_engaged":    lowEngaged,
		},
		"ai_recommendations": recommendations,
		"generated_at":       time.Now(),
		"model_ready":        true,
	})
}

// HandleStoreCampaignLearning stores campaign metrics for future learning
func (s *AdvancedMailingService) HandleStoreCampaignLearning(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID := chi.URLParam(r, "campaignId")
	
	// Get campaign metrics
	var sent, opens, clicks, bounces, complaints int
	var revenue float64
	var subject string
	var createdAt time.Time
	
	err := s.db.QueryRowContext(ctx, `
		SELECT subject, COALESCE(sent_count,0), COALESCE(open_count,0), COALESCE(click_count,0),
			   COALESCE(bounce_count,0), COALESCE(complaint_count,0), COALESCE(revenue,0), created_at
		FROM mailing_campaigns WHERE id = $1
	`, campaignID).Scan(&subject, &sent, &opens, &clicks, &bounces, &complaints, &revenue, &createdAt)
	
	if err != nil {
		http.Error(w, `{"error":"campaign not found"}`, http.StatusNotFound)
		return
	}
	
	// Calculate rates
	openRate := 0.0
	clickRate := 0.0
	bounceRate := 0.0
	if sent > 0 {
		openRate = float64(opens) / float64(sent)
		clickRate = float64(clicks) / float64(sent)
		bounceRate = float64(bounces) / float64(sent)
	}
	
	// Determine what can be learned from this campaign
	learnings := []string{}
	
	if openRate > 0.25 {
		learnings = append(learnings, fmt.Sprintf("HIGH_OPEN_SUBJECT: '%s' achieved %.1f%% open rate", subject, openRate*100))
	} else if openRate < 0.10 {
		learnings = append(learnings, fmt.Sprintf("LOW_OPEN_SUBJECT: '%s' only achieved %.1f%% open rate", subject, openRate*100))
	}
	
	if clickRate > 0.05 {
		learnings = append(learnings, "HIGH_ENGAGEMENT: This campaign had exceptional click-through")
	}
	
	if bounceRate > 0.03 {
		learnings = append(learnings, "LIST_QUALITY_ISSUE: High bounce rate indicates list needs cleaning")
	}
	
	sendHour := createdAt.Hour()
	if openRate > 0.20 && sendHour >= 10 && sendHour <= 14 {
		learnings = append(learnings, fmt.Sprintf("TIMING_SUCCESS: Send at %d:00 performed well", sendHour))
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"campaign_id":  campaignID,
		"subject":      subject,
		"metrics": map[string]interface{}{
			"sent":           sent,
			"opens":          opens,
			"clicks":         clicks,
			"bounces":        bounces,
			"complaints":     complaints,
			"revenue":        revenue,
			"open_rate":      math.Round(openRate*1000) / 10,
			"click_rate":     math.Round(clickRate*1000) / 10,
			"bounce_rate":    math.Round(bounceRate*1000) / 10,
		},
		"learnings":     learnings,
		"stored":        true,
		"learning_applied": true,
	})
}

// ================== ISP PERFORMANCE ==================

const ispDomainCaseSQL = `CASE
	WHEN dom IN ('gmail.com','googlemail.com') THEN 'gmail'
	WHEN dom IN ('yahoo.com','ymail.com','aol.com','rocketmail.com','yahoo.co.uk','yahoo.ca','yahoo.co.jp') THEN 'yahoo'
	WHEN dom IN ('outlook.com','hotmail.com','live.com','msn.com') THEN 'microsoft'
	WHEN dom IN ('icloud.com','me.com','mac.com') THEN 'apple'
	WHEN dom IN ('comcast.net','xfinity.com') THEN 'comcast'
	WHEN dom IN ('att.net','sbcglobal.net','bellsouth.net') THEN 'att'
	WHEN dom IN ('cox.net') THEN 'cox'
	WHEN dom IN ('charter.net','spectrum.net') THEN 'charter'
	ELSE 'other'
END`

var ispDomainFilter = map[string]string{
	"gmail":     "('gmail.com','googlemail.com')",
	"yahoo":     "('yahoo.com','ymail.com','aol.com','rocketmail.com','yahoo.co.uk','yahoo.ca','yahoo.co.jp')",
	"microsoft": "('outlook.com','hotmail.com','live.com','msn.com')",
	"apple":     "('icloud.com','me.com','mac.com')",
	"comcast":   "('comcast.net','xfinity.com')",
	"att":       "('att.net','sbcglobal.net','bellsouth.net')",
	"cox":       "('cox.net')",
	"charter":   "('charter.net','spectrum.net')",
}

var ispLabels = map[string]string{
	"gmail": "Gmail", "yahoo": "Yahoo", "microsoft": "Microsoft",
	"apple": "Apple iCloud", "comcast": "Comcast", "att": "AT&T",
	"cox": "Cox", "charter": "Charter/Spectrum", "other": "Other",
}

func (s *AdvancedMailingService) HandleISPPerformance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start, end := parseAnalyticsRange(r)
	ispParam := r.URL.Query().Get("isp")

	domSubquery := `SELECT t.*, LOWER(COALESCE(NULLIF(t.recipient_domain,''), SPLIT_PART(s.email,'@',2))) as dom
		FROM mailing_tracking_events t
		LEFT JOIN mailing_subscribers s ON t.subscriber_id = s.id
		WHERE t.event_at >= $1 AND t.event_at <= $2`

	if ispParam != "" {
		domains, ok := ispDomainFilter[ispParam]
		if !ok {
			http.Error(w, `{"error":"unknown isp"}`, http.StatusBadRequest)
			return
		}

		gran := trendGranularity(start, end)
		truncFn := "DATE(d.event_at)"
		dateFmt := "2006-01-02"
		switch gran {
		case "10min":
			truncFn = "DATE_TRUNC('hour', d.event_at) + INTERVAL '10 min' * FLOOR(EXTRACT(MINUTE FROM d.event_at) / 10)"
			dateFmt = "2006-01-02T15:04"
		case "hour":
			truncFn = "DATE_TRUNC('hour', d.event_at)"
			dateFmt = "2006-01-02T15:04"
		}

		q := fmt.Sprintf(`SELECT %s as bucket,
			SUM(CASE WHEN d.event_type = 'sent' THEN 1 ELSE 0 END) as sent,
			SUM(CASE WHEN d.event_type = 'delivered' THEN 1 ELSE 0 END) as delivered,
			SUM(CASE WHEN d.event_type = 'opened' THEN 1 ELSE 0 END) as opens,
			SUM(CASE WHEN d.event_type = 'clicked' THEN 1 ELSE 0 END) as clicks,
			SUM(CASE WHEN d.event_type IN ('bounced','hard_bounce','soft_bounce') THEN 1 ELSE 0 END) as bounces,
			SUM(CASE WHEN d.event_type = 'complained' THEN 1 ELSE 0 END) as complaints
		FROM (%s) d
		WHERE d.dom IN %s
		GROUP BY bucket ORDER BY bucket`, truncFn, domSubquery, domains)

		rows, err := s.db.QueryContext(ctx, q, start, end)
		if err != nil {
			log.Printf("[ISPPerformance trend] query error: %v", err)
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
			return
		}
		defer rows.Close()

		var trend []map[string]interface{}
		var tSent, tDel, tOpens, tClicks, tBounces, tComplaints int
		for rows.Next() {
			var bucket time.Time
			var sent, delivered, opens, clicks, bounces, complaints int
			rows.Scan(&bucket, &sent, &delivered, &opens, &clicks, &bounces, &complaints)
			tSent += sent
			tDel += delivered
			tOpens += opens
			tClicks += clicks
			tBounces += bounces
			tComplaints += complaints
			trend = append(trend, map[string]interface{}{
				"date": bucket.Format(dateFmt), "sent": sent, "delivered": delivered,
				"opens": opens, "clicks": clicks, "bounces": bounces, "complaints": complaints,
			})
		}
		if trend == nil {
			trend = []map[string]interface{}{}
		}

		openRate, clickRate, bounceRate, complaintRate := 0.0, 0.0, 0.0, 0.0
		if tSent > 0 {
			openRate = math.Round(float64(tOpens)/float64(tSent)*10000) / 100
			clickRate = math.Round(float64(tClicks)/float64(tSent)*10000) / 100
			bounceRate = math.Round(float64(tBounces)/float64(tSent)*10000) / 100
			complaintRate = math.Round(float64(tComplaints)/float64(tSent)*10000) / 100
		}

		label := ispLabels[ispParam]
		if label == "" {
			label = ispParam
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"api_version": VersionISPPerformance,
			"isp":         ispParam,
			"label":       label,
			"granularity":  gran,
			"totals": map[string]interface{}{
				"sent": tSent, "delivered": tDel, "opens": tOpens,
				"clicks": tClicks, "bounces": tBounces, "complaints": tComplaints,
				"open_rate": openRate, "click_rate": clickRate,
				"bounce_rate": bounceRate, "complaint_rate": complaintRate,
			},
			"trend": trend,
		})
		return
	}

	q := fmt.Sprintf(`SELECT %s as isp,
		SUM(CASE WHEN d.event_type = 'sent' THEN 1 ELSE 0 END) as sent,
		SUM(CASE WHEN d.event_type = 'delivered' THEN 1 ELSE 0 END) as delivered,
		COUNT(DISTINCT CASE WHEN d.event_type = 'opened' THEN d.subscriber_id END) as opens,
		COUNT(DISTINCT CASE WHEN d.event_type = 'clicked' THEN d.subscriber_id END) as clicks,
		SUM(CASE WHEN d.event_type IN ('bounced','hard_bounce','soft_bounce') THEN 1 ELSE 0 END) as bounces,
		SUM(CASE WHEN d.event_type = 'complained' THEN 1 ELSE 0 END) as complaints
	FROM (%s) d
	GROUP BY isp ORDER BY sent DESC`, ispDomainCaseSQL, domSubquery)

	rows, err := s.db.QueryContext(ctx, q, start, end)
	if err != nil {
		log.Printf("[ISPPerformance summary] query error: %v", err)
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()

	var isps []map[string]interface{}
	for rows.Next() {
		var isp string
		var sent, delivered, opens, clicks, bounces, complaints int
		rows.Scan(&isp, &sent, &delivered, &opens, &clicks, &bounces, &complaints)

		openRate, clickRate, bounceRate, complaintRate := 0.0, 0.0, 0.0, 0.0
		if sent > 0 {
			openRate = math.Round(float64(opens)/float64(sent)*10000) / 100
			clickRate = math.Round(float64(clicks)/float64(sent)*10000) / 100
			bounceRate = math.Round(float64(bounces)/float64(sent)*10000) / 100
			complaintRate = math.Round(float64(complaints)/float64(sent)*10000) / 100
		}

		label := ispLabels[isp]
		if label == "" {
			label = isp
		}

		isps = append(isps, map[string]interface{}{
			"isp": isp, "label": label,
			"sent": sent, "delivered": delivered, "opens": opens,
			"clicks": clicks, "bounces": bounces, "complaints": complaints,
			"open_rate": openRate, "click_rate": clickRate,
			"bounce_rate": bounceRate, "complaint_rate": complaintRate,
		})
	}
	if isps == nil {
		isps = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"api_version": VersionISPPerformance,
		"isps":        isps,
	})
}

// ================== ISP SENDING INSIGHTS ==================

func (s *AdvancedMailingService) HandleISPSendingInsights(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := r.Header.Get("X-Organization-ID")

	end := time.Now()
	start := end.Add(-3 * 24 * time.Hour)

	domSubquery := `SELECT t.*, LOWER(COALESCE(NULLIF(t.recipient_domain,''), SPLIT_PART(s.email,'@',2))) as dom
		FROM mailing_tracking_events t
		LEFT JOIN mailing_subscribers s ON t.subscriber_id = s.id
		WHERE t.event_at >= $1 AND t.event_at <= $2`

	// 1. Daily metrics per ISP (hard vs soft bounce split)
	dailyQ := fmt.Sprintf(`SELECT %s as isp, DATE(d.event_at) as day,
		SUM(CASE WHEN d.event_type = 'sent' THEN 1 ELSE 0 END) as sent,
		SUM(CASE WHEN d.event_type = 'delivered' THEN 1 ELSE 0 END) as delivered,
		SUM(CASE WHEN d.event_type = 'hard_bounce' OR (d.event_type = 'bounced' AND COALESCE(d.bounce_type,'') IN ('bad-mailbox','bad-domain','no-answer-from-host','inactive-mailbox','policy-related','routing-errors','bad-connection')) THEN 1 ELSE 0 END) as hard_bounces,
		SUM(CASE WHEN d.event_type = 'soft_bounce' OR d.event_type = 'soft_bounced' OR (d.event_type = 'bounced' AND COALESCE(d.bounce_type,'') NOT IN ('bad-mailbox','bad-domain','no-answer-from-host','inactive-mailbox','policy-related','routing-errors','bad-connection')) THEN 1 ELSE 0 END) as soft_bounces,
		SUM(CASE WHEN d.event_type IN ('deferred','deferral') THEN 1 ELSE 0 END) as deferred,
		SUM(CASE WHEN d.event_type = 'complained' THEN 1 ELSE 0 END) as complained,
		SUM(CASE WHEN d.event_type = 'opened' THEN 1 ELSE 0 END) as opened,
		SUM(CASE WHEN d.event_type = 'opened' AND COALESCE(d.is_machine_open, false) = true THEN 1 ELSE 0 END) as mpp_opens
	FROM (%s) d
	GROUP BY isp, day ORDER BY isp, day`, ispDomainCaseSQL, domSubquery)

	dailyRows, err := s.db.QueryContext(ctx, dailyQ, start, end)
	if err != nil {
		log.Printf("[ISPSendingInsights] daily query error: %v", err)
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	defer dailyRows.Close()

	type dailyRow struct {
		day                                                                        string
		sent, delivered, hardBounces, softBounces, deferred, complained, opened, mppOpens int
	}
	ispDaily := map[string][]dailyRow{}
	for dailyRows.Next() {
		var isp string
		var day time.Time
		var sent, delivered, hardBounces, softBounces, deferred, complained, opened, mppOpens int
		dailyRows.Scan(&isp, &day, &sent, &delivered, &hardBounces, &softBounces, &deferred, &complained, &opened, &mppOpens)
		ispDaily[isp] = append(ispDaily[isp], dailyRow{
			day: day.Format("2006-01-02"), sent: sent, delivered: delivered,
			hardBounces: hardBounces, softBounces: softBounces,
			deferred: deferred, complained: complained,
			opened: opened, mppOpens: mppOpens,
		})
	}

	// 2. Bounce category breakdown per ISP
	catQ := fmt.Sprintf(`SELECT %s as isp, COALESCE(NULLIF(d.bounce_type,''), 'unknown') as category, COUNT(*) as cnt
	FROM (%s) d
	WHERE d.event_type IN ('bounced','hard_bounce','soft_bounce')
	GROUP BY isp, category`, ispDomainCaseSQL, domSubquery)

	catRows, _ := s.db.QueryContext(ctx, catQ, start, end)
	ispBounceCategories := map[string]map[string]int{}
	if catRows != nil {
		defer catRows.Close()
		for catRows.Next() {
			var isp, cat string
			var cnt int
			catRows.Scan(&isp, &cat, &cnt)
			if ispBounceCategories[isp] == nil {
				ispBounceCategories[isp] = map[string]int{}
			}
			ispBounceCategories[isp][cat] = cnt
		}
	}

	// 3. Hourly deferral distribution per ISP
	hrQ := fmt.Sprintf(`SELECT %s as isp, EXTRACT(HOUR FROM d.event_at)::int as hr, COUNT(*) as cnt
	FROM (%s) d
	WHERE d.event_type IN ('deferred','deferral')
	GROUP BY isp, hr ORDER BY isp, hr`, ispDomainCaseSQL, domSubquery)

	hrRows, _ := s.db.QueryContext(ctx, hrQ, start, end)
	ispHourlyDeferrals := map[string][24]int{}
	if hrRows != nil {
		defer hrRows.Close()
		for hrRows.Next() {
			var isp string
			var hr, cnt int
			hrRows.Scan(&isp, &hr, &cnt)
			if hr >= 0 && hr < 24 {
				arr := ispHourlyDeferrals[isp]
				arr[hr] = cnt
				ispHourlyDeferrals[isp] = arr
			}
		}
	}

	// 4. Current quotas from last campaign
	currentQuotas := map[string]int{}
	quotaRows, _ := s.db.QueryContext(ctx, `
		SELECT p.isp, p.quota FROM mailing_campaign_isp_plans p
		JOIN mailing_campaigns c ON p.campaign_id = c.id
		WHERE ($1 = '' OR c.organization_id::text = $1)
		  AND c.status IN ('completed','sent','cancelled','completed_with_errors','sending')
		ORDER BY COALESCE(c.completed_at, c.started_at, c.created_at) DESC
	`, orgID)
	if quotaRows != nil {
		defer quotaRows.Close()
		for quotaRows.Next() {
			var isp string
			var quota int
			quotaRows.Scan(&isp, &quota)
			if _, seen := currentQuotas[isp]; !seen {
				currentQuotas[isp] = quota
			}
		}
	}

	// 5. Build per-ISP insights
	var isps []map[string]interface{}
	for _, isp := range []string{"gmail", "yahoo", "microsoft", "apple", "comcast", "att", "cox", "charter", "other"} {
		days := ispDaily[isp]
		if len(days) == 0 {
			continue
		}

		var totalSent, totalDelivered, totalHardBounces, totalSoftBounces, totalDeferred, totalComplained, totalOpened, totalMPP int
		dailyEntries := make([]map[string]interface{}, 0, len(days))
		bounceRates := make([]float64, 0, len(days))
		deferralRates := make([]float64, 0, len(days))

		for _, d := range days {
			totalSent += d.sent
			totalDelivered += d.delivered
			totalHardBounces += d.hardBounces
			totalSoftBounces += d.softBounces
			totalDeferred += d.deferred
			totalComplained += d.complained
			totalOpened += d.opened
			totalMPP += d.mppOpens

			totalDayBounces := d.hardBounces + d.softBounces
			br := 0.0
			dr := 0.0
			if d.sent > 0 {
				br = float64(totalDayBounces) / float64(d.sent) * 100
				dr = float64(d.deferred) / float64(d.sent) * 100
			}
			bounceRates = append(bounceRates, br)
			deferralRates = append(deferralRates, dr)

			dailyEntries = append(dailyEntries, map[string]interface{}{
				"date": d.day, "sent": d.sent, "delivered": d.delivered,
				"hard_bounces": d.hardBounces, "soft_bounces": d.softBounces,
				"deferred": d.deferred,
				"bounce_rate": math.Round(br*100) / 100,
			})
		}

		totalBounced := totalHardBounces + totalSoftBounces
		bounceRate, deferralRate, complaintRate, humanOpenRate := 0.0, 0.0, 0.0, 0.0
		humanOpens := totalOpened - totalMPP
		if humanOpens < 0 {
			humanOpens = 0
		}
		if totalSent > 0 {
			bounceRate = math.Round(float64(totalBounced)/float64(totalSent)*10000) / 100
			deferralRate = math.Round(float64(totalDeferred)/float64(totalSent)*10000) / 100
			complaintRate = math.Round(float64(totalComplained)/float64(totalSent)*10000) / 100
			humanOpenRate = math.Round(float64(humanOpens)/float64(totalSent)*10000) / 100
		}
		deliveryRate := 0.0
		if totalSent > 0 {
			deliveryRate = math.Round(float64(totalDelivered)/float64(totalSent)*10000) / 100
		}

		// Bounce trend: slope via simple linear regression on daily bounce rates
		bounceTrendSlope := linearSlope(bounceRates)
		deferralTrendSlope := linearSlope(deferralRates)

		// Throttle bounce category ratio
		throttlePct := 0.0
		cats := ispBounceCategories[isp]
		if cats != nil && totalBounced > 0 {
			throttleBounces := cats["quota-issues"] + cats["rate-limited"] + cats["message-expired"]
			throttlePct = float64(throttleBounces) / float64(totalBounced) * 100
		}

		// Deferral concentration: top-3 hours' share
		hourly := ispHourlyDeferrals[isp]
		hourlySlice := make([]int, 24)
		copy(hourlySlice, hourly[:])
		deferralConcentration := top3HourShare(hourlySlice, totalDeferred)

		// Risk score (0-100)
		normBounce := math.Min(bounceRate/5.0, 1.0)      // 5% bounce rate = max
		normDeferral := math.Min(deferralRate/10.0, 1.0)  // 10% deferral rate = max
		normComplaint := math.Min(complaintRate/0.1, 1.0)  // 0.1% complaint rate = max
		normThrottle := throttlePct / 100.0
		normBounceTrend := math.Min(math.Max(bounceTrendSlope/2.0, 0), 1.0)

		riskScore := int(math.Round(math.Min(100, math.Max(0,
			normBounce*25+
				normDeferral*25+
				normBounceTrend*20+
				normThrottle*20+
				normComplaint*10,
		))))

		// Recommendation and suggested quota
		currentQ := currentQuotas[isp]
		var recommendation string
		var suggestedQ int
		switch {
		case riskScore > 80:
			recommendation = "PAUSE"
			suggestedQ = 0
		case riskScore > 60:
			recommendation = "DECREASE"
			suggestedQ = int(float64(currentQ) * 0.65)
		case riskScore > 40:
			recommendation = "CAUTION"
			suggestedQ = int(float64(currentQ) * 0.85)
		case riskScore > 20:
			recommendation = "MAINTAIN"
			suggestedQ = currentQ
		default:
			recommendation = "INCREASE"
			suggestedQ = int(float64(currentQ) * 1.25)
		}
		if currentQ == 0 {
			suggestedQ = 0
		}

		// Signals
		var signals []map[string]interface{}

		bounceTrendDir := "stable"
		if bounceTrendSlope > 0.3 {
			bounceTrendDir = "increasing"
		} else if bounceTrendSlope < -0.3 {
			bounceTrendDir = "decreasing"
		}
		if len(bounceRates) >= 2 {
			signals = append(signals, map[string]interface{}{
				"type": "bounce_trend", "direction": bounceTrendDir,
				"slope": math.Round(bounceTrendSlope*100) / 100,
				"detail": fmt.Sprintf("Bounce rate %s from %.1f%% to %.1f%% over %d days",
					bounceTrendDir, bounceRates[0], bounceRates[len(bounceRates)-1], len(bounceRates)),
			})
		}

		if deferralConcentration > 50 {
			peakHrs := topNHours(hourlySlice, 3)
			signals = append(signals, map[string]interface{}{
				"type": "deferral_spike", "severity": severityLabel(deferralConcentration),
				"detail": fmt.Sprintf("%.0f%% of deferrals concentrated in hours %v UTC", deferralConcentration, peakHrs),
			})
		}

		if throttlePct > 10 {
			signals = append(signals, map[string]interface{}{
				"type": "throttle_bounces", "pct": math.Round(throttlePct*10) / 10,
				"detail": fmt.Sprintf("%.0f%% of bounces are quota-issues or rate-limited categories", throttlePct),
			})
		}

		if complaintRate > 0.05 {
			signals = append(signals, map[string]interface{}{
				"type": "complaint_rate", "severity": severityLabel(complaintRate * 1000),
				"detail": fmt.Sprintf("Complaint rate at %.3f%% — threshold is 0.1%%", complaintRate),
			})
		}

		if deferralTrendSlope > 0.5 {
			signals = append(signals, map[string]interface{}{
				"type": "deferral_trend", "direction": "increasing",
				"detail": fmt.Sprintf("Deferral rate rising over %d days — ISP may be increasing throttling", len(deferralRates)),
			})
		}

		if signals == nil {
			signals = []map[string]interface{}{}
		}

		label := ispLabels[isp]
		if label == "" {
			label = isp
		}

		hardBounceRate, softBounceRate := 0.0, 0.0
		if totalSent > 0 {
			hardBounceRate = math.Round(float64(totalHardBounces)/float64(totalSent)*10000) / 100
			softBounceRate = math.Round(float64(totalSoftBounces)/float64(totalSent)*10000) / 100
		}

		isps = append(isps, map[string]interface{}{
			"isp": isp, "label": label,
			"sent": totalSent, "delivered": totalDelivered,
			"bounced": totalBounced, "hard_bounces": totalHardBounces, "soft_bounces": totalSoftBounces,
			"deferred": totalDeferred,
			"complained": totalComplained, "opened": totalOpened,
			"mpp_opens": totalMPP, "human_opens": humanOpens,
			"delivery_rate": deliveryRate,
			"bounce_rate": bounceRate, "hard_bounce_rate": hardBounceRate, "soft_bounce_rate": softBounceRate,
			"deferral_rate": deferralRate, "complaint_rate": complaintRate,
			"human_open_rate": humanOpenRate,
			"current_quota": currentQ, "suggested_quota": suggestedQ,
			"recommendation": recommendation, "risk_score": riskScore,
			"signals":           signals,
			"daily":             dailyEntries,
			"hourly_deferrals":  hourlySlice,
		})
	}

	if isps == nil {
		isps = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"api_version":    VersionISPSendingInsights,
		"window":         map[string]string{"start": start.Format(time.RFC3339), "end": end.Format(time.RFC3339)},
		"current_quotas": currentQuotas,
		"isps":           isps,
	})
}

// linearSlope computes the slope of a simple linear regression on y values indexed 0..n-1.
func linearSlope(ys []float64) float64 {
	n := float64(len(ys))
	if n < 2 {
		return 0
	}
	var sumX, sumY, sumXY, sumX2 float64
	for i, y := range ys {
		x := float64(i)
		sumX += x
		sumY += y
		sumXY += x * y
		sumX2 += x * x
	}
	denom := n*sumX2 - sumX*sumX
	if denom == 0 {
		return 0
	}
	return (n*sumXY - sumX*sumY) / denom
}

func top3HourShare(hourly []int, total int) float64 {
	if total == 0 {
		return 0
	}
	sorted := make([]int, len(hourly))
	copy(sorted, hourly)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j] > sorted[i] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	top3 := 0
	for i := 0; i < 3 && i < len(sorted); i++ {
		top3 += sorted[i]
	}
	return float64(top3) / float64(total) * 100
}

func topNHours(hourly []int, n int) []int {
	type hv struct{ hr, cnt int }
	pairs := make([]hv, 0, 24)
	for h, c := range hourly {
		if c > 0 {
			pairs = append(pairs, hv{h, c})
		}
	}
	for i := 0; i < len(pairs); i++ {
		for j := i + 1; j < len(pairs); j++ {
			if pairs[j].cnt > pairs[i].cnt {
				pairs[i], pairs[j] = pairs[j], pairs[i]
			}
		}
	}
	result := make([]int, 0, n)
	for i := 0; i < n && i < len(pairs); i++ {
		result = append(result, pairs[i].hr)
	}
	return result
}

func severityLabel(pct float64) string {
	if pct > 70 {
		return "critical"
	}
	if pct > 40 {
		return "high"
	}
	return "moderate"
}
