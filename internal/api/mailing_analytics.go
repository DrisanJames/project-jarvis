package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func (s *AdvancedMailingService) HandleCampaignTimeline(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID := chi.URLParam(r, "campaignId")
	
	rows, _ := s.db.QueryContext(ctx, `
		SELECT DATE_TRUNC('hour', event_time) as hour,
			   SUM(CASE WHEN event_type = 'opened' THEN 1 ELSE 0 END) as opens,
			   SUM(CASE WHEN event_type = 'clicked' THEN 1 ELSE 0 END) as clicks,
			   SUM(CASE WHEN event_type = 'bounced' THEN 1 ELSE 0 END) as bounces
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
	
	rows, _ := s.db.QueryContext(ctx, `
		SELECT SPLIT_PART(email, '@', 2) as domain,
			   COUNT(*) as total,
			   SUM(CASE WHEN event_type = 'opened' THEN 1 ELSE 0 END) as opens,
			   SUM(CASE WHEN event_type = 'clicked' THEN 1 ELSE 0 END) as clicks
		FROM mailing_tracking_events
		WHERE campaign_id = $1
		GROUP BY SPLIT_PART(email, '@', 2)
		ORDER BY total DESC LIMIT 10
	`, campaignID)
	defer rows.Close()
	
	var domains []map[string]interface{}
	for rows.Next() {
		var domain string
		var total, opens, clicks int
		rows.Scan(&domain, &total, &opens, &clicks)
		domains = append(domains, map[string]interface{}{
			"domain": domain, "total": total, "opens": opens, "clicks": clicks,
		})
	}
	if domains == nil { domains = []map[string]interface{}{} }
	
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
	days := r.URL.Query().Get("days")
	if days == "" { days = "30" }
	daysInt, _ := strconv.Atoi(days)
	
	var totalSent, totalOpens, totalClicks, totalBounces, totalComplaints int
	var totalRevenue float64
	
	// Get totals from all campaigns (not just those with started_at)
	s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(sent_count),0), COALESCE(SUM(open_count),0), COALESCE(SUM(click_count),0),
			   COALESCE(SUM(bounce_count),0), COALESCE(SUM(complaint_count),0), COALESCE(SUM(revenue),0)
		FROM mailing_campaigns WHERE created_at >= NOW() - INTERVAL '1 day' * $1
	`, daysInt).Scan(&totalSent, &totalOpens, &totalClicks, &totalBounces, &totalComplaints, &totalRevenue)
	
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
	
	// Daily trend from tracking events (more accurate)
	rows, _ := s.db.QueryContext(ctx, `
		SELECT DATE(event_at) as day, 
			   SUM(CASE WHEN event_type = 'sent' THEN 1 ELSE 0 END) as sent,
			   SUM(CASE WHEN event_type = 'opened' THEN 1 ELSE 0 END) as opens,
			   SUM(CASE WHEN event_type = 'clicked' THEN 1 ELSE 0 END) as clicks,
			   SUM(CASE WHEN event_type = 'bounced' THEN 1 ELSE 0 END) as bounces
		FROM mailing_tracking_events
		WHERE event_at >= NOW() - INTERVAL '1 day' * $1
		GROUP BY DATE(event_at)
		ORDER BY day DESC
	`, daysInt)
	defer rows.Close()
	
	var dailyTrend []map[string]interface{}
	for rows.Next() {
		var day time.Time
		var sent, opens, clicks, bounces int
		rows.Scan(&day, &sent, &opens, &clicks, &bounces)
		dailyTrend = append(dailyTrend, map[string]interface{}{
			"date": day.Format("2006-01-02"), "sent": sent, "opens": opens, "clicks": clicks, "bounces": bounces,
		})
	}
	if dailyTrend == nil { dailyTrend = []map[string]interface{}{} }
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"period_days": daysInt,
		"totals": map[string]interface{}{
			"sent": totalSent, "opens": totalOpens, "clicks": totalClicks,
			"bounces": totalBounces, "complaints": totalComplaints, "revenue": totalRevenue,
		},
		"rates": map[string]interface{}{
			"open_rate": openRate, "click_rate": clickRate,
			"bounce_rate": bounceRate, "complaint_rate": complaintRate,
		},
		"daily_trend": dailyTrend,
	})
}

// ================== CROSS-CAMPAIGN REPORTING ==================

// HandleCampaignComparison compares multiple campaigns
func (s *AdvancedMailingService) HandleCampaignComparison(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	
	// Get all campaigns with metrics
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, status, sent_count, open_count, click_count, bounce_count, complaint_count, revenue,
			   created_at, COALESCE(started_at, created_at), COALESCE(completed_at, created_at)
		FROM mailing_campaigns
		WHERE sent_count > 0
		ORDER BY created_at DESC
		LIMIT 50
	`)
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
		"campaigns": campaigns,
		"total":     len(campaigns),
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
		"metric":    metric,
		"campaigns": topCampaigns,
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
	json.NewEncoder(w).Encode(map[string]interface{}{"lists": lists})
}

// HandleEngagementReport returns subscriber engagement summary
func (s *AdvancedMailingService) HandleEngagementReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	
	// Engagement distribution
	var highEngagement, medEngagement, lowEngagement, noEngagement int
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_subscribers WHERE engagement_score >= 70").Scan(&highEngagement)
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_subscribers WHERE engagement_score >= 40 AND engagement_score < 70").Scan(&medEngagement)
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_subscribers WHERE engagement_score > 0 AND engagement_score < 40").Scan(&lowEngagement)
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_subscribers WHERE engagement_score = 0 OR engagement_score IS NULL").Scan(&noEngagement)
	
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
			"email":       email,
			"score":       score,
			"opens":       opens,
			"clicks":      clicks,
			"last_open":   lastOpen,
		})
	}
	if topSubscribers == nil { topSubscribers = []map[string]interface{}{} }
	
	// Engagement over time
	engagementTrend, _ := s.db.QueryContext(ctx, `
		SELECT DATE(event_at) as day, COUNT(DISTINCT subscriber_id) as engaged_subscribers
		FROM mailing_tracking_events
		WHERE event_type IN ('opened', 'clicked') AND event_at >= NOW() - INTERVAL '30 days'
		GROUP BY DATE(event_at)
		ORDER BY day DESC
	`)
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
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
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

// HandleDeliverabilityReport returns deliverability metrics
func (s *AdvancedMailingService) HandleDeliverabilityReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	
	// Overall deliverability
	var totalSent, totalDelivered, totalBounced, totalComplaints int
	s.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(sent_count), 0) FROM mailing_campaigns").Scan(&totalSent)
	s.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(bounce_count), 0) FROM mailing_campaigns").Scan(&totalBounced)
	s.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(complaint_count), 0) FROM mailing_campaigns").Scan(&totalComplaints)
	totalDelivered = totalSent - totalBounced
	
	deliveryRate := 0.0
	bounceRate := 0.0
	complaintRate := 0.0
	if totalSent > 0 {
		deliveryRate = float64(totalDelivered) / float64(totalSent) * 100
		bounceRate = float64(totalBounced) / float64(totalSent) * 100
		complaintRate = float64(totalComplaints) / float64(totalSent) * 100
	}
	
	// Bounce breakdown
	rows, _ := s.db.QueryContext(ctx, `
		SELECT COALESCE(bounce_type, 'unknown') as type, COUNT(*) as count
		FROM mailing_tracking_events
		WHERE event_type = 'bounced'
		GROUP BY bounce_type
		ORDER BY count DESC
	`)
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
	
	// Suppressions
	var suppressionCount, bounceSuppressions, complaintSuppressions int
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_suppressions WHERE active = true").Scan(&suppressionCount)
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_suppressions WHERE active = true AND source = 'bounce'").Scan(&bounceSuppressions)
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_suppressions WHERE active = true AND source IN ('complaint', 'spam_complaint')").Scan(&complaintSuppressions)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"totals": map[string]int{
			"sent":       totalSent,
			"delivered":  totalDelivered,
			"bounced":    totalBounced,
			"complaints": totalComplaints,
		},
		"rates": map[string]float64{
			"delivery_rate":  math.Round(deliveryRate*100) / 100,
			"bounce_rate":    math.Round(bounceRate*100) / 100,
			"complaint_rate": math.Round(complaintRate*1000) / 1000,
		},
		"bounce_breakdown": bounceBreakdown,
		"suppressions": map[string]int{
			"total":      suppressionCount,
			"bounces":    bounceSuppressions,
			"complaints": complaintSuppressions,
		},
	})
}

// HandleRevenueReport returns revenue analytics
func (s *AdvancedMailingService) HandleRevenueReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	days := r.URL.Query().Get("days")
	if days == "" { days = "30" }
	daysInt, _ := strconv.Atoi(days)
	
	// Total revenue
	var totalRevenue float64
	var campaignsWithRevenue int
	s.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(revenue), 0), COUNT(*) FROM mailing_campaigns WHERE revenue > 0").Scan(&totalRevenue, &campaignsWithRevenue)
	
	// Revenue per email
	var totalSent int
	s.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(sent_count), 0) FROM mailing_campaigns").Scan(&totalSent)
	revenuePerEmail := 0.0
	if totalSent > 0 {
		revenuePerEmail = totalRevenue / float64(totalSent)
	}
	
	// Revenue by campaign
	rows, _ := s.db.QueryContext(ctx, `
		SELECT name, sent_count, revenue
		FROM mailing_campaigns
		WHERE revenue > 0
		ORDER BY revenue DESC
		LIMIT 10
	`)
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
	
	// Daily revenue trend
	trendRows, _ := s.db.QueryContext(ctx, `
		SELECT DATE(COALESCE(started_at, created_at)) as day, SUM(revenue) as daily_revenue
		FROM mailing_campaigns
		WHERE created_at >= NOW() - INTERVAL '1 day' * $1 AND revenue > 0
		GROUP BY DATE(COALESCE(started_at, created_at))
		ORDER BY day DESC
	`, daysInt)
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
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"period_days":             daysInt,
		"total_revenue":           totalRevenue,
		"campaigns_with_revenue":  campaignsWithRevenue,
		"total_sent":              totalSent,
		"revenue_per_email":       math.Round(revenuePerEmail*10000) / 10000,
		"top_revenue_campaigns":   topRevenueCampaigns,
		"daily_trend":             revenueTrend,
	})
}

// ================== HISTORICAL METRICS & LLM LEARNING ==================

// HandleGetHistoricalMetrics returns comprehensive historical metrics for analysis
func (s *AdvancedMailingService) HandleGetHistoricalMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	days := r.URL.Query().Get("days")
	if days == "" { days = "90" }
	daysInt, _ := strconv.Atoi(days)
	if daysInt <= 0 { daysInt = 90 }
	
	// Campaign performance over time
	var totalCampaigns, totalSent, totalOpens, totalClicks, totalBounces, totalComplaints int
	var totalRevenue float64
	s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(sent_count),0), COALESCE(SUM(open_count),0), COALESCE(SUM(click_count),0),
			   COALESCE(SUM(bounce_count),0), COALESCE(SUM(complaint_count),0), COALESCE(SUM(revenue),0)
		FROM mailing_campaigns WHERE created_at >= NOW() - INTERVAL '1 day' * $1
	`, daysInt).Scan(&totalCampaigns, &totalSent, &totalOpens, &totalClicks, &totalBounces, &totalComplaints, &totalRevenue)
	
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
	weeklyRows, _ := s.db.QueryContext(ctx, `
		SELECT DATE_TRUNC('week', created_at) as week,
			   COUNT(*) as campaigns,
			   COALESCE(SUM(sent_count),0) as sent,
			   COALESCE(SUM(open_count),0) as opens,
			   COALESCE(SUM(click_count),0) as clicks,
			   COALESCE(SUM(bounce_count),0) as bounces,
			   COALESCE(SUM(complaint_count),0) as complaints,
			   COALESCE(SUM(revenue),0) as revenue
		FROM mailing_campaigns
		WHERE created_at >= NOW() - INTERVAL '1 day' * $1
		GROUP BY DATE_TRUNC('week', created_at)
		ORDER BY week DESC
	`, daysInt)
	defer weeklyRows.Close()
	
	var weeklyTrends []map[string]interface{}
	for weeklyRows.Next() {
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
	topRows, _ := s.db.QueryContext(ctx, `
		SELECT id, name, subject, COALESCE(sent_count,0), COALESCE(open_count,0), COALESCE(click_count,0), 
			   COALESCE(revenue,0), created_at
		FROM mailing_campaigns
		WHERE sent_count > 0 AND created_at >= NOW() - INTERVAL '1 day' * $1
		ORDER BY (open_count::float / NULLIF(sent_count,0)) DESC LIMIT 10
	`, daysInt)
	defer topRows.Close()
	
	var topCampaigns []map[string]interface{}
	for topRows.Next() {
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
	subjectRows, _ := s.db.QueryContext(ctx, `
		SELECT subject, AVG(open_count::float / NULLIF(sent_count,0)) as avg_open_rate,
			   COUNT(*) as usage_count
		FROM mailing_campaigns
		WHERE sent_count > 100 AND created_at >= NOW() - INTERVAL '1 day' * $1
		GROUP BY subject HAVING COUNT(*) >= 1
		ORDER BY avg_open_rate DESC LIMIT 20
	`, daysInt)
	defer subjectRows.Close()
	
	var subjectPatterns []map[string]interface{}
	for subjectRows.Next() {
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
	hourRows, _ := s.db.QueryContext(ctx, `
		SELECT EXTRACT(HOUR FROM event_at) as hour,
			   COUNT(*) FILTER (WHERE event_type = 'open') as opens,
			   COUNT(*) FILTER (WHERE event_type = 'click') as clicks
		FROM mailing_tracking_events
		WHERE event_at >= NOW() - INTERVAL '1 day' * $1
		GROUP BY EXTRACT(HOUR FROM event_at)
		ORDER BY hour
	`, daysInt)
	defer hourRows.Close()
	
	var hourlyPerformance []map[string]interface{}
	for hourRows.Next() {
		var hour int
		var opens, clicks int
		hourRows.Scan(&hour, &opens, &clicks)
		hourlyPerformance = append(hourlyPerformance, map[string]interface{}{
			"hour": hour, "opens": opens, "clicks": clicks,
		})
	}
	if hourlyPerformance == nil { hourlyPerformance = []map[string]interface{}{} }
	
	// Day-of-week performance
	dowRows, _ := s.db.QueryContext(ctx, `
		SELECT EXTRACT(DOW FROM event_at) as dow,
			   COUNT(*) FILTER (WHERE event_type = 'open') as opens,
			   COUNT(*) FILTER (WHERE event_type = 'click') as clicks
		FROM mailing_tracking_events
		WHERE event_at >= NOW() - INTERVAL '1 day' * $1
		GROUP BY EXTRACT(DOW FROM event_at)
		ORDER BY dow
	`, daysInt)
	defer dowRows.Close()
	
	dayNames := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	var dailyPerformance []map[string]interface{}
	for dowRows.Next() {
		var dow int
		var opens, clicks int
		dowRows.Scan(&dow, &opens, &clicks)
		dailyPerformance = append(dailyPerformance, map[string]interface{}{
			"day": dayNames[dow], "day_num": dow, "opens": opens, "clicks": clicks,
		})
	}
	if dailyPerformance == nil { dailyPerformance = []map[string]interface{}{} }
	
	// Domain performance
	domainRows, _ := s.db.QueryContext(ctx, `
		SELECT domain,
			   COALESCE(SUM(total_sent),0) as sent,
			   COALESCE(SUM(total_opens),0) as opens,
			   COALESCE(SUM(total_clicks),0) as clicks,
			   AVG(engagement_score) as avg_engagement
		FROM mailing_inbox_profiles
		GROUP BY domain HAVING SUM(total_sent) > 0
		ORDER BY SUM(total_sent) DESC LIMIT 20
	`)
	defer domainRows.Close()
	
	var domainMetrics []map[string]interface{}
	for domainRows.Next() {
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
		FROM mailing_tracking_events WHERE event_type = 'open'
		GROUP BY EXTRACT(HOUR FROM event_at)
		ORDER BY COUNT(*) DESC LIMIT 1
	`).Scan(&bestHour)
	
	s.db.QueryRowContext(ctx, `
		SELECT CASE EXTRACT(DOW FROM event_at)::int
			WHEN 0 THEN 'Sunday' WHEN 1 THEN 'Monday' WHEN 2 THEN 'Tuesday'
			WHEN 3 THEN 'Wednesday' WHEN 4 THEN 'Thursday' WHEN 5 THEN 'Friday'
			WHEN 6 THEN 'Saturday' END
		FROM mailing_tracking_events WHERE event_type = 'open'
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
