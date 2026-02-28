package ongage

import (
	"strings"
	"time"
)

// processCampaignStats converts raw report rows to processed campaigns
// Note: The Ongage API returns columns with simple names (e.g., "sent") even when queried with sum()
func (c *Collector) processCampaignStats(rows []ReportRow, espMap map[string]ESPConnection) []ProcessedCampaign {
	campaigns := make([]ProcessedCampaign, 0, len(rows))

	for _, row := range rows {
		campaign := ProcessedCampaign{
			ID:              getStringValue(row, "mailing_id"),
			Name:            getStringValue(row, "mailing_name"),
			Subject:         getStringValue(row, "email_message_subject"),
			ESPConnectionID: getStringValue(row, "esp_connection_id"),
			Segments:        []string{getStringValue(row, "segment_name")},
			Targeted:        getInt64Value(row, "targeted"),
			Sent:            getInt64Value(row, "sent"),
			Delivered:       getInt64Value(row, "success"),
			Opens:           getInt64Value(row, "opens"),
			UniqueOpens:     getInt64Value(row, "unique_opens"),
			Clicks:          getInt64Value(row, "clicks"),
			UniqueClicks:    getInt64Value(row, "unique_clicks"),
			Unsubscribes:    getInt64Value(row, "unsubscribes"),
			Complaints:      getInt64Value(row, "complaints"),
			HardBounces:     getInt64Value(row, "hard_bounces"),
			SoftBounces:     getInt64Value(row, "soft_bounces"),
			Bounces:         getInt64Value(row, "hard_bounces") + getInt64Value(row, "soft_bounces"),
			Failed:          getInt64Value(row, "failed"),
		}

		// Parse schedule time
		if schedStr := getStringValue(row, "schedule_date"); schedStr != "" {
			if ts, err := ParseUnixTimestamp(schedStr); err == nil {
				campaign.ScheduleTime = ts
			}
		}

		// Get ESP name
		espName := getStringValue(row, "esp_name")
		if espName == "" {
			if esp, ok := espMap[campaign.ESPConnectionID]; ok {
				espName = GetESPName(esp.ESPID.String())
			}
		}
		campaign.ESP = espName

		// Calculate rates
		if campaign.Sent > 0 {
			campaign.DeliveryRate = float64(campaign.Delivered) / float64(campaign.Sent)
			campaign.OpenRate = float64(campaign.UniqueOpens) / float64(campaign.Sent)
			campaign.ClickRate = float64(campaign.UniqueClicks) / float64(campaign.Sent)
			campaign.UnsubscribeRate = float64(campaign.Unsubscribes) / float64(campaign.Sent)
			campaign.ComplaintRate = float64(campaign.Complaints) / float64(campaign.Sent)
			campaign.BounceRate = float64(campaign.Bounces) / float64(campaign.Sent)
		}
		if campaign.UniqueOpens > 0 {
			campaign.CTR = float64(campaign.UniqueClicks) / float64(campaign.UniqueOpens)
		}

		campaigns = append(campaigns, campaign)
	}

	return campaigns
}

// processESPStats converts raw report rows to ESP performance metrics
func (c *Collector) processESPStats(rows []ReportRow) []ESPPerformance {
	perfs := make([]ESPPerformance, 0, len(rows))

	for _, row := range rows {
		sent := getInt64Value(row, "sent")
		delivered := getInt64Value(row, "success")
		opens := getInt64Value(row, "unique_opens")
		clicks := getInt64Value(row, "unique_clicks")
		bounces := getInt64Value(row, "hard_bounces") + getInt64Value(row, "soft_bounces")
		complaints := getInt64Value(row, "complaints")

		perf := ESPPerformance{
			ESPName:         getStringValue(row, "esp_name"),
			ConnectionID:    getStringValue(row, "esp_connection_id"),
			ConnectionTitle: getStringValue(row, "esp_connection_title"),
			TotalSent:       sent,
			TotalDelivered:  delivered,
		}

		if sent > 0 {
			perf.DeliveryRate = float64(delivered) / float64(sent)
			perf.OpenRate = float64(opens) / float64(sent)
			perf.ClickRate = float64(clicks) / float64(sent)
			perf.BounceRate = float64(bounces) / float64(sent)
			perf.ComplaintRate = float64(complaints) / float64(sent)
		}

		perfs = append(perfs, perf)
	}

	return perfs
}

// processScheduleStats converts campaign data to schedule analysis by extracting hour/day from schedule_date
func (c *Collector) processScheduleStats(rows []ReportRow) []ScheduleAnalysis {
	dayNames := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

	// Aggregate stats by hour and day of week
	type hourDayKey struct {
		hour      int
		dayOfWeek int
	}
	aggregated := make(map[hourDayKey]struct {
		sent, delivered, opens, clicks int64
		count                          int
	})

	for _, row := range rows {
		schedStr := getStringValue(row, "schedule_date")
		ts, err := ParseUnixTimestamp(schedStr)
		if err != nil || ts.IsZero() {
			continue
		}

		hour := ts.Hour()
		dayOfWeek := int(ts.Weekday())
		key := hourDayKey{hour: hour, dayOfWeek: dayOfWeek}

		agg := aggregated[key]
		agg.sent += getInt64Value(row, "sent")
		agg.delivered += getInt64Value(row, "success")
		agg.opens += getInt64Value(row, "unique_opens")
		agg.clicks += getInt64Value(row, "unique_clicks")
		agg.count++
		aggregated[key] = agg
	}

	analyses := make([]ScheduleAnalysis, 0, len(aggregated))

	for key, agg := range aggregated {
		analysis := ScheduleAnalysis{
			Hour:          key.hour,
			DayOfWeek:     key.dayOfWeek + 1, // Convert to 1-indexed (Sunday=1)
			DayName:       dayNames[key.dayOfWeek],
			CampaignCount: agg.count,
			TotalSent:     agg.sent,
		}

		if agg.sent > 0 {
			analysis.AvgDeliveryRate = float64(agg.delivered) / float64(agg.sent)
			analysis.AvgOpenRate = float64(agg.opens) / float64(agg.sent)
			analysis.AvgClickRate = float64(agg.clicks) / float64(agg.sent)
		}

		// Determine performance rating
		if analysis.AvgOpenRate >= 0.25 {
			analysis.Performance = "optimal"
		} else if analysis.AvgOpenRate >= 0.18 {
			analysis.Performance = "good"
		} else if analysis.AvgOpenRate >= 0.12 {
			analysis.Performance = "average"
		} else {
			analysis.Performance = "poor"
		}

		analyses = append(analyses, analysis)
	}

	return analyses
}

// processAudienceStats converts raw report rows to audience analysis
func (c *Collector) processAudienceStats(rows []ReportRow) []AudienceAnalysis {
	analyses := make([]AudienceAnalysis, 0, len(rows))

	for _, row := range rows {
		targeted := getInt64Value(row, "targeted")
		sent := getInt64Value(row, "sent")
		opens := getInt64Value(row, "unique_opens")
		clicks := getInt64Value(row, "unique_clicks")
		bounces := getInt64Value(row, "hard_bounces") + getInt64Value(row, "soft_bounces")

		segmentID := getStringValue(row, "segment_id")
		segmentName := getStringValue(row, "segment_name")

		// Skip empty segments
		if segmentID == "" && segmentName == "" {
			continue
		}

		analysis := AudienceAnalysis{
			SegmentID:     segmentID,
			SegmentName:   segmentName,
			CampaignCount: 0, // Will be enriched later from campaign data
			TotalTargeted: targeted,
			TotalSent:     sent,
		}

		if sent > 0 {
			analysis.AvgOpenRate = float64(opens) / float64(sent)
			analysis.AvgClickRate = float64(clicks) / float64(sent)
			analysis.AvgBounceRate = float64(bounces) / float64(sent)
		}

		// Determine engagement level
		if analysis.AvgOpenRate >= 0.20 && analysis.AvgClickRate >= 0.03 {
			analysis.Engagement = "high"
		} else if analysis.AvgOpenRate >= 0.12 {
			analysis.Engagement = "medium"
		} else {
			analysis.Engagement = "low"
		}

		analyses = append(analyses, analysis)
	}

	return analyses
}

// enrichAudienceWithCampaignCounts enriches audience analysis with campaign counts from campaign data
func (c *Collector) enrichAudienceWithCampaignCounts(audienceAnalysis []AudienceAnalysis, campaigns []ProcessedCampaign) []AudienceAnalysis {
	// Count campaigns per segment
	segmentCampaignCounts := make(map[string]int)
	for _, camp := range campaigns {
		for _, segmentName := range camp.Segments {
			if segmentName != "" {
				segmentCampaignCounts[segmentName]++
			}
		}
	}

	// Update campaign counts
	for i := range audienceAnalysis {
		if count, ok := segmentCampaignCounts[audienceAnalysis[i].SegmentName]; ok {
			audienceAnalysis[i].CampaignCount = count
		}
	}

	return audienceAnalysis
}

// processDailyStats converts raw report rows to pipeline metrics
func (c *Collector) processDailyStats(rows []ReportRow) []PipelineMetrics {
	metrics := make([]PipelineMetrics, 0, len(rows))

	for _, row := range rows {
		sent := getInt64Value(row, "sent")
		delivered := getInt64Value(row, "success")
		opens := getInt64Value(row, "unique_opens")
		clicks := getInt64Value(row, "unique_clicks")
		targeted := getInt64Value(row, "targeted")

		// Handle stats_date which may come as timestamp or date string
		dateStr := getStringValue(row, "stats_date")
		if dateStr == "" {
			continue
		}

		// Try to parse as Unix timestamp first
		if timestamp := getInt64Value(row, "stats_date"); timestamp > 0 {
			dateStr = time.Unix(timestamp, 0).Format("2006-01-02")
		}

		m := PipelineMetrics{
			Date:           dateStr,
			CampaignsSent:  1, // Aggregated data per day
			TotalTargeted:  targeted,
			TotalSent:      sent,
			TotalDelivered: delivered,
			TotalOpens:     opens,
			TotalClicks:    clicks,
		}

		if sent > 0 {
			m.DeliveryRate = float64(delivered) / float64(sent)
			m.OpenRate = float64(opens) / float64(sent)
			m.ClickRate = float64(clicks) / float64(sent)
		}

		metrics = append(metrics, m)
	}

	return metrics
}

// analyzeSubjectLines analyzes subject line performance patterns
func (c *Collector) analyzeSubjectLines(campaigns []ProcessedCampaign) []SubjectLineAnalysis {
	// Group campaigns by subject line
	subjectMap := make(map[string][]ProcessedCampaign)
	for _, camp := range campaigns {
		if camp.Subject != "" {
			subjectMap[camp.Subject] = append(subjectMap[camp.Subject], camp)
		}
	}

	analyses := make([]SubjectLineAnalysis, 0, len(subjectMap))

	for subject, camps := range subjectMap {
		analysis := SubjectLineAnalysis{
			Subject:       subject,
			CampaignCount: len(camps),
			Length:        len(subject),
			HasEmoji:      containsEmoji(subject),
			HasNumber:     containsNumber(subject),
			HasQuestion:   strings.Contains(subject, "?"),
			HasUrgency:    containsUrgencyWords(subject),
		}

		var totalSent, totalOpens, totalClicks int64
		espSet := make(map[string]bool)

		for _, camp := range camps {
			totalSent += camp.Sent
			totalOpens += camp.UniqueOpens
			totalClicks += camp.UniqueClicks
			if camp.ESP != "" {
				espSet[camp.ESP] = true
			}
		}

		analysis.TotalSent = totalSent
		if totalSent > 0 {
			analysis.AvgOpenRate = float64(totalOpens) / float64(totalSent)
			analysis.AvgClickRate = float64(totalClicks) / float64(totalSent)
		}
		if totalOpens > 0 {
			analysis.AvgCTR = float64(totalClicks) / float64(totalOpens)
		}

		for esp := range espSet {
			analysis.ESPs = append(analysis.ESPs, esp)
		}

		// Determine performance
		if analysis.AvgOpenRate >= 0.22 {
			analysis.Performance = "high"
		} else if analysis.AvgOpenRate >= 0.15 {
			analysis.Performance = "medium"
		} else {
			analysis.Performance = "low"
		}

		analyses = append(analyses, analysis)
	}

	return analyses
}
