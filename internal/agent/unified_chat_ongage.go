package agent

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ignite/sparkpost-monitor/internal/ongage"
)

// handleOngageSummaryQuery provides an overview of Ongage campaign data
func (a *Agent) handleOngageSummaryQuery() ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.collectors == nil || a.collectors.Ongage == nil {
		return ChatResponse{
			Message: "⚠️ Ongage integration is not configured. Enable it to access campaign data.",
		}
	}

	campaigns := a.collectors.Ongage.GetCampaigns()
	subjects := a.collectors.Ongage.GetSubjectAnalysis()
	scheduleData := a.collectors.Ongage.GetScheduleAnalysis()
	espPerf := a.collectors.Ongage.GetESPPerformance()

	var sb strings.Builder
	sb.WriteString("📊 **Ongage Campaign Platform Summary**\n\n")

	// Campaign overview
	sb.WriteString(fmt.Sprintf("**Campaigns:** %d total\n", len(campaigns)))
	
	var totalSent, totalOpens, totalClicks int64
	for _, c := range campaigns {
		totalSent += c.Sent
		totalOpens += c.UniqueOpens
		totalClicks += c.UniqueClicks
	}
	
	if totalSent > 0 {
		openRate := float64(totalOpens) / float64(totalSent) * 100
		clickRate := float64(totalClicks) / float64(totalSent) * 100
		sb.WriteString(fmt.Sprintf("**Total Sent:** %s\n", formatNumber(totalSent)))
		sb.WriteString(fmt.Sprintf("**Avg Open Rate:** %.2f%%\n", openRate))
		sb.WriteString(fmt.Sprintf("**Avg Click Rate:** %.2f%%\n\n", clickRate))
	}

	// Subject line insights
	if len(subjects) > 0 {
		highPerf := 0
		for _, s := range subjects {
			if s.Performance == "high" {
				highPerf++
			}
		}
		sb.WriteString(fmt.Sprintf("**Subject Lines:** %d analyzed, %d high-performing\n\n", len(subjects), highPerf))
	}

	// ESP breakdown
	if len(espPerf) > 0 {
		sb.WriteString("**ESP Performance:**\n")
		for _, esp := range espPerf {
			sb.WriteString(fmt.Sprintf("• %s: %.2f%% delivery, %.2f%% opens\n",
				esp.ESPName, esp.DeliveryRate*100, esp.OpenRate*100))
		}
		sb.WriteString("\n")
	}

	// Best send times
	if len(scheduleData) > 0 {
		var bestTime *ongage.ScheduleAnalysis
		for i, s := range scheduleData {
			if s.Performance == "optimal" && (bestTime == nil || s.AvgOpenRate > bestTime.AvgOpenRate) {
				bestTime = &scheduleData[i]
			}
		}
		if bestTime != nil {
			sb.WriteString(fmt.Sprintf("**Best Send Time:** %s at %d:00 (%.2f%% open rate)\n",
				bestTime.DayName, bestTime.Hour, bestTime.AvgOpenRate*100))
		}
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"Show campaign performance",
			"Analyze subject lines",
			"What are optimal send times?",
			"Show audience engagement",
		},
	}
}

// handleOngageCampaignQuery handles questions about campaigns
func (a *Agent) handleOngageCampaignQuery(query string) ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.collectors == nil || a.collectors.Ongage == nil {
		return ChatResponse{
			Message: "⚠️ Ongage integration is not configured.",
		}
	}

	campaigns := a.collectors.Ongage.GetCampaigns()
	if len(campaigns) == 0 {
		return ChatResponse{
			Message: "📊 No campaign data available yet. The system is still collecting data from Ongage.",
		}
	}

	var sb strings.Builder
	sb.WriteString("📧 **Campaign Performance Overview**\n\n")

	// Calculate totals
	var totalSent, totalDelivered, totalOpens, totalClicks int64
	espCounts := make(map[string]int)
	
	for _, c := range campaigns {
		totalSent += c.Sent
		totalDelivered += c.Delivered
		totalOpens += c.UniqueOpens
		totalClicks += c.UniqueClicks
		espCounts[c.ESP]++
	}

	sb.WriteString(fmt.Sprintf("**Total Campaigns:** %d\n", len(campaigns)))
	sb.WriteString(fmt.Sprintf("**Total Sent:** %s\n", formatNumber(totalSent)))
	
	if totalSent > 0 {
		sb.WriteString(fmt.Sprintf("**Delivery Rate:** %.2f%%\n", float64(totalDelivered)/float64(totalSent)*100))
		sb.WriteString(fmt.Sprintf("**Open Rate:** %.2f%%\n", float64(totalOpens)/float64(totalSent)*100))
		sb.WriteString(fmt.Sprintf("**Click Rate:** %.2f%%\n\n", float64(totalClicks)/float64(totalSent)*100))
	}

	// ESP breakdown
	sb.WriteString("**By ESP:**\n")
	for esp, count := range espCounts {
		sb.WriteString(fmt.Sprintf("• %s: %d campaigns\n", esp, count))
	}
	sb.WriteString("\n")

	// Top 5 campaigns by opens
	sortedCampaigns := make([]ongage.ProcessedCampaign, len(campaigns))
	copy(sortedCampaigns, campaigns)
	sort.Slice(sortedCampaigns, func(i, j int) bool {
		return sortedCampaigns[i].OpenRate > sortedCampaigns[j].OpenRate
	})

	sb.WriteString("**Top Performing Campaigns (by open rate):**\n")
	for i, c := range sortedCampaigns {
		if i >= 5 {
			break
		}
		subject := c.Subject
		if len(subject) > 40 {
			subject = subject[:37] + "..."
		}
		sb.WriteString(fmt.Sprintf("%d. %s - %.2f%% opens\n", i+1, subject, c.OpenRate*100))
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"What subject lines perform best?",
			"When should I send campaigns?",
			"Show ESP performance comparison",
		},
	}
}

// handleOngageSubjectQuery handles questions about subject lines
func (a *Agent) handleOngageSubjectQuery(query string) ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.collectors == nil || a.collectors.Ongage == nil {
		return ChatResponse{
			Message: "⚠️ Ongage integration is not configured.",
		}
	}

	subjects := a.collectors.Ongage.GetSubjectAnalysis()
	if len(subjects) == 0 {
		return ChatResponse{
			Message: "📝 No subject line data available yet.",
		}
	}

	var sb strings.Builder
	sb.WriteString("✍️ **Subject Line Analysis**\n\n")

	// Count by performance
	highCount, medCount, lowCount := 0, 0, 0
	var withEmoji, withNumber, withUrgency int
	var emojiOpenRate, noEmojiOpenRate float64
	var emojiCount, noEmojiCount int

	for _, s := range subjects {
		switch s.Performance {
		case "high":
			highCount++
		case "medium":
			medCount++
		case "low":
			lowCount++
		}
		if s.HasEmoji {
			withEmoji++
			emojiOpenRate += s.AvgOpenRate
			emojiCount++
		} else {
			noEmojiOpenRate += s.AvgOpenRate
			noEmojiCount++
		}
		if s.HasNumber {
			withNumber++
		}
		if s.HasUrgency {
			withUrgency++
		}
	}

	sb.WriteString(fmt.Sprintf("**Total Subject Lines Analyzed:** %d\n\n", len(subjects)))
	sb.WriteString(fmt.Sprintf("**Performance Breakdown:**\n"))
	sb.WriteString(fmt.Sprintf("• 🟢 High: %d (%.0f%%)\n", highCount, float64(highCount)/float64(len(subjects))*100))
	sb.WriteString(fmt.Sprintf("• 🟡 Medium: %d (%.0f%%)\n", medCount, float64(medCount)/float64(len(subjects))*100))
	sb.WriteString(fmt.Sprintf("• 🔴 Low: %d (%.0f%%)\n\n", lowCount, float64(lowCount)/float64(len(subjects))*100))

	// Feature analysis
	sb.WriteString("**Feature Analysis:**\n")
	sb.WriteString(fmt.Sprintf("• With emojis: %d (%.0f%%)\n", withEmoji, float64(withEmoji)/float64(len(subjects))*100))
	sb.WriteString(fmt.Sprintf("• With numbers: %d (%.0f%%)\n", withNumber, float64(withNumber)/float64(len(subjects))*100))
	sb.WriteString(fmt.Sprintf("• Urgency words: %d (%.0f%%)\n\n", withUrgency, float64(withUrgency)/float64(len(subjects))*100))

	// Emoji impact
	if emojiCount > 0 && noEmojiCount > 0 {
		avgEmoji := emojiOpenRate / float64(emojiCount) * 100
		avgNoEmoji := noEmojiOpenRate / float64(noEmojiCount) * 100
		diff := avgEmoji - avgNoEmoji
		if diff > 0 {
			sb.WriteString(fmt.Sprintf("💡 **Insight:** Subjects with emojis have %.1f%% higher open rates\n\n", diff))
		} else {
			sb.WriteString(fmt.Sprintf("💡 **Insight:** Subjects without emojis perform %.1f%% better\n\n", -diff))
		}
	}

	// Top subjects
	sortedSubjects := make([]ongage.SubjectLineAnalysis, len(subjects))
	copy(sortedSubjects, subjects)
	sort.Slice(sortedSubjects, func(i, j int) bool {
		return sortedSubjects[i].AvgOpenRate > sortedSubjects[j].AvgOpenRate
	})

	sb.WriteString("**Top Performing Subjects:**\n")
	for i, s := range sortedSubjects {
		if i >= 3 {
			break
		}
		subj := s.Subject
		if len(subj) > 50 {
			subj = subj[:47] + "..."
		}
		sb.WriteString(fmt.Sprintf("%d. \"%s\" (%.2f%% opens)\n", i+1, subj, s.AvgOpenRate*100))
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"What send times work best?",
			"Show campaign performance",
			"Which segments have highest engagement?",
		},
	}
}

// handleOngageScheduleQuery handles questions about send time optimization
func (a *Agent) handleOngageScheduleQuery() ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.collectors == nil || a.collectors.Ongage == nil {
		return ChatResponse{
			Message: "⚠️ Ongage integration is not configured.",
		}
	}

	scheduleData := a.collectors.Ongage.GetScheduleAnalysis()
	if len(scheduleData) == 0 {
		return ChatResponse{
			Message: "⏰ No schedule data available yet.",
		}
	}

	var sb strings.Builder
	sb.WriteString("⏰ **Send Time Optimization Analysis**\n\n")

	// Find optimal times
	var optimalTimes []ongage.ScheduleAnalysis
	dayPerformance := make(map[string]float64)
	dayCount := make(map[string]int)

	for _, s := range scheduleData {
		if s.Performance == "optimal" || s.Performance == "good" {
			optimalTimes = append(optimalTimes, s)
		}
		dayPerformance[s.DayName] += s.AvgOpenRate
		dayCount[s.DayName]++
	}

	// Sort optimal times by open rate
	sort.Slice(optimalTimes, func(i, j int) bool {
		return optimalTimes[i].AvgOpenRate > optimalTimes[j].AvgOpenRate
	})

	sb.WriteString("**🎯 Optimal Send Times:**\n")
	for i, t := range optimalTimes {
		if i >= 5 {
			break
		}
		sb.WriteString(fmt.Sprintf("%d. %s at %02d:00 - %.2f%% open rate (%d campaigns)\n",
			i+1, t.DayName, t.Hour, t.AvgOpenRate*100, t.CampaignCount))
	}
	sb.WriteString("\n")

	// Day performance ranking
	type dayRank struct {
		day  string
		rate float64
	}
	var dayRanks []dayRank
	for day, rate := range dayPerformance {
		if dayCount[day] > 0 {
			dayRanks = append(dayRanks, dayRank{day, rate / float64(dayCount[day])})
		}
	}
	sort.Slice(dayRanks, func(i, j int) bool {
		return dayRanks[i].rate > dayRanks[j].rate
	})

	sb.WriteString("**📅 Best Days to Send:**\n")
	for i, dr := range dayRanks {
		emoji := "🥇"
		if i == 1 {
			emoji = "🥈"
		} else if i == 2 {
			emoji = "🥉"
		} else {
			emoji = fmt.Sprintf("%d.", i+1)
		}
		sb.WriteString(fmt.Sprintf("%s %s - %.2f%% avg open rate\n", emoji, dr.day, dr.rate*100))
	}
	sb.WriteString("\n")

	// Recommendations
	sb.WriteString("**💡 Recommendations:**\n")
	if len(optimalTimes) > 0 {
		sb.WriteString(fmt.Sprintf("• Best time to send: **%s at %02d:00**\n", optimalTimes[0].DayName, optimalTimes[0].Hour))
	}
	if len(dayRanks) >= 2 {
		sb.WriteString(fmt.Sprintf("• Avoid: **%s** (lowest engagement)\n", dayRanks[len(dayRanks)-1].day))
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"Show campaign performance",
			"Analyze subject lines",
			"Which audiences are most engaged?",
		},
	}
}

// handleOngageAudienceQuery handles questions about audience/segment performance
func (a *Agent) handleOngageAudienceQuery() ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.collectors == nil || a.collectors.Ongage == nil {
		return ChatResponse{
			Message: "⚠️ Ongage integration is not configured.",
		}
	}

	audiences := a.collectors.Ongage.GetAudienceAnalysis()
	if len(audiences) == 0 {
		return ChatResponse{
			Message: "👥 No audience data available yet.",
		}
	}

	var sb strings.Builder
	sb.WriteString("👥 **Audience Engagement Analysis**\n\n")

	// Count by engagement level
	var high, medium, low int
	var totalTargeted, totalSent int64

	for _, a := range audiences {
		switch a.Engagement {
		case "high":
			high++
		case "medium":
			medium++
		case "low":
			low++
		}
		totalTargeted += a.TotalTargeted
		totalSent += a.TotalSent
	}

	sb.WriteString(fmt.Sprintf("**Total Segments:** %d\n", len(audiences)))
	sb.WriteString(fmt.Sprintf("**Total Audience:** %s contacts\n\n", formatNumber(totalTargeted)))

	sb.WriteString("**Engagement Distribution:**\n")
	sb.WriteString(fmt.Sprintf("• 🟢 High Engagement: %d segments\n", high))
	sb.WriteString(fmt.Sprintf("• 🟡 Medium Engagement: %d segments\n", medium))
	sb.WriteString(fmt.Sprintf("• 🔴 Low Engagement: %d segments\n\n", low))

	// Sort by engagement metrics
	sorted := make([]ongage.AudienceAnalysis, len(audiences))
	copy(sorted, audiences)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].AvgOpenRate > sorted[j].AvgOpenRate
	})

	sb.WriteString("**Top Engaged Segments:**\n")
	for i, a := range sorted {
		if i >= 5 {
			break
		}
		name := a.SegmentName
		if len(name) > 30 {
			name = name[:27] + "..."
		}
		sb.WriteString(fmt.Sprintf("%d. %s - %.2f%% opens, %.2f%% clicks\n",
			i+1, name, a.AvgOpenRate*100, a.AvgClickRate*100))
	}
	sb.WriteString("\n")

	// Low engagement segments
	if low > 0 {
		sb.WriteString("**⚠️ Segments Needing Attention:**\n")
		count := 0
		for i := len(sorted) - 1; i >= 0 && count < 3; i-- {
			if sorted[i].Engagement == "low" {
				name := sorted[i].SegmentName
				if len(name) > 30 {
					name = name[:27] + "..."
				}
				sb.WriteString(fmt.Sprintf("• %s (%.2f%% opens)\n", name, sorted[i].AvgOpenRate*100))
				count++
			}
		}
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"Show campaign performance",
			"What subject lines work best?",
			"What are the best send times?",
		},
	}
}
