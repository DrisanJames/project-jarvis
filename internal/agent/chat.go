package agent

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/sparkpost"
	"github.com/ignite/sparkpost-monitor/internal/storage"
)

// ChatMessage represents a message in the chat
type ChatMessage struct {
	Role      string    `json:"role"` // "user" or "agent"
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// ChatResponse represents the agent's response to a query
type ChatResponse struct {
	Message     string      `json:"message"`
	Data        interface{} `json:"data,omitempty"`
	Suggestions []string    `json:"suggestions,omitempty"`
}

// Chat handles a user query and returns a response
func (a *Agent) Chat(query string, currentMetrics *sparkpost.Summary, ispMetrics []sparkpost.ISPMetrics) ChatResponse {
	query = strings.ToLower(query)

	// Detect intent and respond accordingly - order matters for priority
	switch {
	case containsAny(query, []string{"correlation", "relationship", "cause"}):
		return a.handleCorrelationQuery(query)
	case containsAny(query, []string{"concern", "watch", "issue", "problem", "alert"}):
		return a.handleConcernsQuery(currentMetrics, ispMetrics)
	case containsAny(query, []string{"volume", "increase", "decrease", "throttle"}):
		return a.handleVolumeQuery(query, currentMetrics, ispMetrics)
	case containsAny(query, []string{"baseline", "learned", "pattern", "normal"}):
		return a.handleBaselineQuery(query)
	case containsAny(query, []string{"should i", "recommend", "suggest", "advice"}):
		return a.handleRecommendationQuery(query, currentMetrics, ispMetrics)
	case containsAny(query, []string{"forecast", "predict", "expect", "tomorrow"}):
		return a.handleForecastQuery(query, currentMetrics, ispMetrics)
	case containsAny(query, []string{"how is", "performance", "doing", "status"}):
		return a.handlePerformanceQuery(query, currentMetrics, ispMetrics)
	default:
		return a.handleGeneralQuery(query, currentMetrics, ispMetrics)
	}
}

// handlePerformanceQuery handles queries about performance
func (a *Agent) handlePerformanceQuery(query string, summary *sparkpost.Summary, ispMetrics []sparkpost.ISPMetrics) ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	// Check if asking about specific ISP
	for _, isp := range ispMetrics {
		if strings.Contains(query, strings.ToLower(isp.Provider)) {
			return a.formatISPPerformance(isp)
		}
	}

	// General performance summary
	if summary == nil {
		return ChatResponse{
			Message: "I don't have current metrics data available. Please wait for the next data fetch.",
		}
	}

	var sb strings.Builder
	sb.WriteString("📊 **Overall Performance (Last 24 Hours)**\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")
	
	sb.WriteString(fmt.Sprintf("**Volume:** %s\n", formatNumber(summary.TotalTargeted)))
	sb.WriteString(fmt.Sprintf("**Delivered:** %s (%.2f%%)\n", formatNumber(summary.TotalDelivered), summary.DeliveryRate*100))
	sb.WriteString(fmt.Sprintf("**Open Rate:** %.2f%%\n", summary.OpenRate*100))
	sb.WriteString(fmt.Sprintf("**Click Rate:** %.2f%%\n", summary.ClickRate*100))
	sb.WriteString(fmt.Sprintf("**Complaint Rate:** %.4f%%\n", summary.ComplaintRate*100))
	sb.WriteString(fmt.Sprintf("**Bounce Rate:** %.2f%%\n", summary.BounceRate*100))
	
	// Evaluate overall health
	status := "✅ Healthy"
	if summary.ComplaintRate > 0.0005 || summary.BounceRate > 0.05 {
		status = "❌ Critical Issues"
	} else if summary.ComplaintRate > 0.0003 || summary.BounceRate > 0.03 {
		status = "⚠️ Needs Attention"
	}
	sb.WriteString(fmt.Sprintf("\n**Status:** %s\n", status))

	// Add ISP breakdown
	if len(ispMetrics) > 0 {
		sb.WriteString("\n**Top ISPs:**\n")
		for i, isp := range ispMetrics {
			if i >= 5 {
				break
			}
			ispStatus := "✅"
			if isp.Status == "critical" {
				ispStatus = "❌"
			} else if isp.Status == "warning" {
				ispStatus = "⚠️"
			}
			sb.WriteString(fmt.Sprintf("  %s %s: %s delivered, %.2f%% open rate\n",
				ispStatus, isp.Provider, formatNumber(isp.Metrics.Delivered), isp.Metrics.OpenRate*100))
		}
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"How is Gmail performing?",
			"What should I be concerned about?",
			"Should I increase volume?",
		},
	}
}

// formatISPPerformance formats performance data for a specific ISP
func (a *Agent) formatISPPerformance(isp sparkpost.ISPMetrics) ChatResponse {
	var sb strings.Builder
	
	statusEmoji := "✅"
	if isp.Status == "critical" {
		statusEmoji = "❌"
	} else if isp.Status == "warning" {
		statusEmoji = "⚠️"
	}

	sb.WriteString(fmt.Sprintf("📊 **%s Performance**\n", isp.Provider))
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")
	
	sb.WriteString(fmt.Sprintf("**Volume:** %s\n", formatNumber(isp.Metrics.Targeted)))
	sb.WriteString(fmt.Sprintf("**Delivered:** %.2f%%\n", isp.Metrics.DeliveryRate*100))
	sb.WriteString(fmt.Sprintf("**Open Rate:** %.2f%%\n", isp.Metrics.OpenRate*100))
	sb.WriteString(fmt.Sprintf("**Click Rate:** %.2f%%\n", isp.Metrics.ClickRate*100))
	sb.WriteString(fmt.Sprintf("**Complaint Rate:** %.4f%%\n", isp.Metrics.ComplaintRate*100))
	sb.WriteString(fmt.Sprintf("**Bounce Rate:** %.2f%%\n", isp.Metrics.BounceRate*100))
	sb.WriteString(fmt.Sprintf("\n**Status:** %s %s\n", statusEmoji, strings.ToUpper(isp.Status)))
	
	if isp.StatusReason != "" {
		sb.WriteString(fmt.Sprintf("**Reason:** %s\n", isp.StatusReason))
	}

	// Check against baseline if available
	key := fmt.Sprintf("isp:%s", isp.Provider)
	if baseline, ok := a.baselines[key]; ok && baseline.DataPoints > 0 {
		sb.WriteString("\n**Compared to Baseline:**\n")
		if mb, ok := baseline.Metrics["complaint_rate"]; ok {
			deviation := (isp.Metrics.ComplaintRate - mb.Mean) / mb.StdDev
			trend := "stable"
			if deviation > 1 {
				trend = "above normal"
			} else if deviation < -1 {
				trend = "below normal (good)"
			}
			sb.WriteString(fmt.Sprintf("  Complaint rate: %s (%.1fσ from mean)\n", trend, deviation))
		}
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			fmt.Sprintf("Should I increase volume to %s?", isp.Provider),
			"What's the overall performance?",
			"What correlations have you learned?",
		},
	}
}

// handleRecommendationQuery handles queries asking for recommendations
func (a *Agent) handleRecommendationQuery(query string, summary *sparkpost.Summary, ispMetrics []sparkpost.ISPMetrics) ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var sb strings.Builder
	var recommendations []string

	// Check for ISP-specific recommendations
	for _, isp := range ispMetrics {
		if strings.Contains(query, strings.ToLower(isp.Provider)) {
			return a.getISPRecommendation(isp)
		}
	}

	sb.WriteString("💡 **Recommendations Based on Current Data**\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	// Check each ISP for recommendations
	for _, isp := range ispMetrics {
		if isp.Status == "critical" {
			recommendations = append(recommendations, fmt.Sprintf(
				"🔴 **%s:** Consider reducing volume or pausing sends. %s",
				isp.Provider, isp.StatusReason))
		} else if isp.Status == "warning" {
			recommendations = append(recommendations, fmt.Sprintf(
				"🟡 **%s:** Monitor closely. %s",
				isp.Provider, isp.StatusReason))
		}
	}

	// Check learned correlations for proactive recommendations
	for _, corr := range a.correlations {
		if corr.Confidence > 0.7 {
			recommendations = append(recommendations, fmt.Sprintf(
				"📈 **%s:** Based on learned patterns, when %s exceeds %.0f, %s tends to increase by %.0f%%. Monitor this threshold.",
				corr.EntityName, corr.TriggerMetric, corr.TriggerThreshold,
				corr.EffectMetric, corr.EffectChange*100))
		}
	}

	if len(recommendations) == 0 {
		sb.WriteString("✅ All metrics look healthy! No specific recommendations at this time.\n\n")
		sb.WriteString("**General Best Practices:**\n")
		sb.WriteString("- Continue monitoring complaint rates across ISPs\n")
		sb.WriteString("- Maintain consistent sending volumes\n")
		sb.WriteString("- Review bounce reasons weekly\n")
	} else {
		for i, rec := range recommendations {
			sb.WriteString(fmt.Sprintf("%d. %s\n\n", i+1, rec))
		}
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"What should I watch today?",
			"How is Gmail performing?",
			"Show me learned correlations",
		},
	}
}

// getISPRecommendation provides specific recommendations for an ISP
func (a *Agent) getISPRecommendation(isp sparkpost.ISPMetrics) ChatResponse {
	var sb strings.Builder
	
	sb.WriteString(fmt.Sprintf("💡 **Recommendations for %s**\n", isp.Provider))
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	key := fmt.Sprintf("isp:%s", isp.Provider)
	baseline, hasBaseline := a.baselines[key]

	// Analyze current state
	if isp.Status == "critical" {
		sb.WriteString("⚠️ **Current Status: CRITICAL**\n\n")
		sb.WriteString(fmt.Sprintf("Reason: %s\n\n", isp.StatusReason))
		sb.WriteString("**Recommended Actions:**\n")
		sb.WriteString("1. Consider reducing volume by 30-50%\n")
		sb.WriteString("2. Review content for spam triggers\n")
		sb.WriteString("3. Check if you're hitting any rate limits\n")
		sb.WriteString("4. Review recent changes to email templates\n")
	} else if isp.Status == "warning" {
		sb.WriteString("⚡ **Current Status: WARNING**\n\n")
		sb.WriteString(fmt.Sprintf("Reason: %s\n\n", isp.StatusReason))
		sb.WriteString("**Recommended Actions:**\n")
		sb.WriteString("1. Monitor closely for the next 2-4 hours\n")
		sb.WriteString("2. Avoid increasing volume\n")
		sb.WriteString("3. Review targeting for this ISP\n")
	} else {
		sb.WriteString("✅ **Current Status: HEALTHY**\n\n")
		
		if hasBaseline && baseline.DataPoints >= a.config.MinDataPoints {
			// Provide data-driven volume recommendation
			if mb, ok := baseline.Metrics["complaint_rate"]; ok {
				headroom := (mb.Mean + mb.StdDev*2) - isp.Metrics.ComplaintRate
				if headroom > mb.StdDev {
					sb.WriteString("**Volume Recommendation:** You have room to safely increase volume by 10-15%.\n")
				} else {
					sb.WriteString("**Volume Recommendation:** Maintain current volume. Limited headroom before reaching concern levels.\n")
				}
			}
		} else {
			sb.WriteString("**Note:** Still learning patterns for this ISP. Recommend maintaining current volume until baseline is established.\n")
		}
	}

	// Check for relevant correlations
	for _, corr := range a.correlations {
		if corr.EntityName == isp.Provider {
			sb.WriteString(fmt.Sprintf("\n📊 **Learned Pattern:** When %s exceeds %.0f, %s typically increases by %.0f%%.\n",
				corr.TriggerMetric, corr.TriggerThreshold, corr.EffectMetric, corr.EffectChange*100))
		}
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			fmt.Sprintf("How is %s performing?", isp.Provider),
			"What are the overall recommendations?",
			"Show me all alerts",
		},
	}
}

// handleConcernsQuery handles queries about concerns and issues
func (a *Agent) handleConcernsQuery(summary *sparkpost.Summary, ispMetrics []sparkpost.ISPMetrics) ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("⚠️ **Items Requiring Attention**\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	var concerns []string
	
	// Check ISP metrics
	for _, isp := range ispMetrics {
		if isp.Status == "critical" {
			concerns = append(concerns, fmt.Sprintf("🔴 **%s** - %s", isp.Provider, isp.StatusReason))
		} else if isp.Status == "warning" {
			concerns = append(concerns, fmt.Sprintf("🟡 **%s** - %s", isp.Provider, isp.StatusReason))
		}
	}

	// Check active alerts
	for _, alert := range a.alerts {
		if !alert.Acknowledged {
			emoji := "🟡"
			if alert.Severity == "critical" {
				emoji = "🔴"
			}
			concerns = append(concerns, fmt.Sprintf("%s **%s**: %s (%.1fσ deviation)",
				emoji, alert.EntityName, alert.MetricName, alert.Deviation))
		}
	}

	if len(concerns) == 0 {
		sb.WriteString("✅ **All Clear!** No significant concerns at this time.\n\n")
		sb.WriteString("All metrics are within normal ranges and no anomalies have been detected.\n")
	} else {
		for i, concern := range concerns {
			sb.WriteString(fmt.Sprintf("%d. %s\n\n", i+1, concern))
		}
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"What are your recommendations?",
			"Show me learned correlations",
			"How is overall performance?",
		},
	}
}

// handleVolumeQuery handles queries about volume changes
func (a *Agent) handleVolumeQuery(query string, summary *sparkpost.Summary, ispMetrics []sparkpost.ISPMetrics) ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	// Check for specific ISP
	for _, isp := range ispMetrics {
		if strings.Contains(query, strings.ToLower(isp.Provider)) {
			return a.getVolumeAdvice(isp)
		}
	}

	// General volume advice
	var sb strings.Builder
	sb.WriteString("📈 **Volume Analysis**\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	if summary != nil {
		sb.WriteString(fmt.Sprintf("**Current Volume:** %s emails/day\n\n", formatNumber(summary.TotalTargeted)))
	}

	// Check which ISPs can handle more volume
	sb.WriteString("**ISP Volume Assessment:**\n")
	for _, isp := range ispMetrics {
		if isp.Status == "healthy" {
			sb.WriteString(fmt.Sprintf("✅ %s: Safe to increase\n", isp.Provider))
		} else if isp.Status == "warning" {
			sb.WriteString(fmt.Sprintf("⚠️ %s: Maintain current levels\n", isp.Provider))
		} else {
			sb.WriteString(fmt.Sprintf("❌ %s: Consider reducing\n", isp.Provider))
		}
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"Should I increase volume to Gmail?",
			"What's the Yahoo volume threshold?",
			"Show me correlations",
		},
	}
}

// getVolumeAdvice provides volume advice for a specific ISP
func (a *Agent) getVolumeAdvice(isp sparkpost.ISPMetrics) ChatResponse {
	var sb strings.Builder
	
	sb.WriteString(fmt.Sprintf("📈 **Volume Advice for %s**\n", isp.Provider))
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	sb.WriteString(fmt.Sprintf("**Current Volume:** %s\n", formatNumber(isp.Metrics.Targeted)))
	sb.WriteString(fmt.Sprintf("**Current Status:** %s\n\n", isp.Status))

	// Check for volume correlations
	for _, corr := range a.correlations {
		if corr.EntityName == isp.Provider && corr.TriggerMetric == "volume" {
			sb.WriteString(fmt.Sprintf("⚠️ **Learned Threshold:** Based on historical data, when volume exceeds **%.0f**, ",
				corr.TriggerThreshold))
			sb.WriteString(fmt.Sprintf("%s increases by approximately %.0f%%.\n\n", corr.EffectMetric, corr.EffectChange*100))
			
			if float64(isp.Metrics.Targeted) < corr.TriggerThreshold*0.8 {
				sb.WriteString("✅ **Recommendation:** You're currently below the threshold. Safe to increase volume gradually.\n")
			} else if float64(isp.Metrics.Targeted) < corr.TriggerThreshold {
				sb.WriteString("⚠️ **Recommendation:** You're approaching the threshold. Proceed cautiously with any increases.\n")
			} else {
				sb.WriteString("❌ **Recommendation:** You've exceeded the threshold. Consider reducing volume.\n")
			}
			
			return ChatResponse{
				Message: sb.String(),
				Suggestions: []string{
					fmt.Sprintf("How is %s performing?", isp.Provider),
					"What are all the correlations?",
					"Show me concerns",
				},
			}
		}
	}

	// No learned correlation, give general advice based on status
	switch isp.Status {
	case "healthy":
		sb.WriteString("✅ **Recommendation:** Metrics look healthy. You can consider a 10-15% volume increase.\n")
		sb.WriteString("\n**Suggested Approach:**\n")
		sb.WriteString("1. Increase volume gradually (5% per day)\n")
		sb.WriteString("2. Monitor complaint rate closely\n")
		sb.WriteString("3. If complaints increase, pause and reassess\n")
	case "warning":
		sb.WriteString("⚠️ **Recommendation:** Not recommended to increase volume at this time.\n")
		sb.WriteString("Wait until metrics stabilize before considering increases.\n")
	case "critical":
		sb.WriteString("❌ **Recommendation:** Do NOT increase volume. Consider reducing by 30-50%.\n")
		sb.WriteString("Focus on resolving the underlying issues first.\n")
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"What should I be concerned about?",
			"Show me all ISP performance",
			"What patterns have you learned?",
		},
	}
}

// handleBaselineQuery handles queries about learned baselines
func (a *Agent) handleBaselineQuery(query string) ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("📊 **Learned Baselines**\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	if len(a.baselines) == 0 {
		sb.WriteString("No baselines learned yet. I need more data to establish patterns.\n")
		sb.WriteString(fmt.Sprintf("Minimum data points required: %d\n", a.config.MinDataPoints))
	} else {
		for key, baseline := range a.baselines {
			sb.WriteString(fmt.Sprintf("**%s** (%.0f data points)\n", key, float64(baseline.DataPoints)))
			for metricName, mb := range baseline.Metrics {
				sb.WriteString(fmt.Sprintf("  • %s: mean=%.6f, σ=%.6f\n", metricName, mb.Mean, mb.StdDev))
			}
			sb.WriteString("\n")
		}
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"What correlations have you learned?",
			"How is performance?",
			"What concerns should I watch?",
		},
	}
}

// handleCorrelationQuery handles queries about learned correlations
func (a *Agent) handleCorrelationQuery(query string) ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("🔗 **Learned Correlations**\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	if len(a.correlations) == 0 {
		sb.WriteString("No significant correlations learned yet.\n")
		sb.WriteString("I'm continuously analyzing data to find patterns.\n\n")
		sb.WriteString("**What I'm looking for:**\n")
		sb.WriteString("• Volume thresholds that affect complaint rates\n")
		sb.WriteString("• Time patterns (day of week, hour of day)\n")
		sb.WriteString("• Relationships between metrics\n")
	} else {
		// Sort by confidence
		sorted := make([]storage.Correlation, len(a.correlations))
		copy(sorted, a.correlations)
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].Confidence > sorted[j].Confidence
		})

		for i, corr := range sorted {
			sb.WriteString(fmt.Sprintf("%d. **%s**\n", i+1, corr.EntityName))
			sb.WriteString(fmt.Sprintf("   When %s > %.0f → %s increases %.0f%%\n",
				corr.TriggerMetric, corr.TriggerThreshold, corr.EffectMetric, corr.EffectChange*100))
			sb.WriteString(fmt.Sprintf("   Confidence: %.0f%% (observed %d times)\n\n",
				corr.Confidence*100, corr.Occurrences))
		}
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"What are the baselines?",
			"What should I watch?",
			"Give me volume advice",
		},
	}
}

// handleForecastQuery handles queries about forecasts and predictions
func (a *Agent) handleForecastQuery(query string, summary *sparkpost.Summary, ispMetrics []sparkpost.ISPMetrics) ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("🔮 **Forecast & Predictions**\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	if len(a.baselines) < 3 || len(a.rollingStats) < 5 {
		sb.WriteString("⏳ **Insufficient Data for Forecasting**\n\n")
		sb.WriteString("I need more historical data to make accurate predictions.\n")
		sb.WriteString("Continue operating normally and I'll provide forecasts once I have enough data.\n")
	} else {
		sb.WriteString("Based on learned patterns and current trends:\n\n")

		for _, isp := range ispMetrics {
			key := fmt.Sprintf("isp:%s", isp.Provider)
			baseline, hasBaseline := a.baselines[key]
			
			if !hasBaseline {
				continue
			}

			sb.WriteString(fmt.Sprintf("**%s:**\n", isp.Provider))
			
			// Simple trend analysis based on current vs baseline
			if mb, ok := baseline.Metrics["complaint_rate"]; ok {
				current := isp.Metrics.ComplaintRate
				trend := "stable"
				if current > mb.Mean+mb.StdDev {
					trend = "trending up ⬆️"
				} else if current < mb.Mean-mb.StdDev {
					trend = "trending down ⬇️"
				}
				sb.WriteString(fmt.Sprintf("  • Complaint rate: %s\n", trend))
			}

			// Check volume correlation risk
			for _, corr := range a.correlations {
				if corr.EntityName == isp.Provider && corr.TriggerMetric == "volume" {
					if float64(isp.Metrics.Targeted) > corr.TriggerThreshold*0.9 {
						sb.WriteString(fmt.Sprintf("  ⚠️ Volume approaching threshold (%.0f). Expect %s increase.\n",
							corr.TriggerThreshold, corr.EffectMetric))
					}
				}
			}
			sb.WriteString("\n")
		}
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"What are the current concerns?",
			"Show me learned patterns",
			"How is overall performance?",
		},
	}
}

// handleGeneralQuery handles general queries
func (a *Agent) handleGeneralQuery(query string, summary *sparkpost.Summary, ispMetrics []sparkpost.ISPMetrics) ChatResponse {
	var sb strings.Builder
	sb.WriteString("👋 **SparkPost Agent**\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")
	sb.WriteString("I'm your SparkPost analytics agent. I learn from your email metrics to provide insights and recommendations.\n\n")
	sb.WriteString("**I can help with:**\n")
	sb.WriteString("• Performance analysis (\"How is Gmail performing?\")\n")
	sb.WriteString("• Volume recommendations (\"Should I increase volume to Yahoo?\")\n")
	sb.WriteString("• Identifying concerns (\"What should I watch?\")\n")
	sb.WriteString("• Learned patterns (\"What correlations have you found?\")\n")
	sb.WriteString("• Forecasts (\"What do you expect tomorrow?\")\n\n")
	sb.WriteString("What would you like to know?\n")

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"How is overall performance?",
			"What should I be concerned about?",
			"What have you learned so far?",
		},
	}
}

// Helper functions

func containsAny(s string, substrs []string) bool {
	for _, substr := range substrs {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

func formatNumber(n int64) string {
	if n >= 1000000 {
		return fmt.Sprintf("%.1fM", float64(n)/1000000)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}
