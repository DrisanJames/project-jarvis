package agent

import (
	"fmt"
	"sort"
	"strings"
)

// handleEcosystemQuery handles queries about the entire ecosystem
func (a *Agent) handleEcosystemQuery(eco EcosystemSummary, allISPs []UnifiedISP) ChatResponse {
	var sb strings.Builder
	sb.WriteString("🌐 **Email Ecosystem Overview**\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	// Overall health status
	status := "✅ Healthy"
	if eco.CriticalISPs > 0 {
		status = "❌ Critical Issues Detected"
	} else if eco.WarningISPs > 0 {
		status = "⚠️ Needs Attention"
	}
	sb.WriteString(fmt.Sprintf("**Status:** %s\n\n", status))

	sb.WriteString("**Aggregate Metrics (All Providers):**\n")
	sb.WriteString(fmt.Sprintf("• Total Volume: %s emails\n", formatNumber(eco.TotalVolume)))
	sb.WriteString(fmt.Sprintf("• Delivered: %s (%.2f%%)\n", formatNumber(eco.TotalDelivered), eco.DeliveryRate*100))
	sb.WriteString(fmt.Sprintf("• Open Rate: %.2f%%\n", eco.OpenRate*100))
	sb.WriteString(fmt.Sprintf("• Click Rate: %.2f%%\n", eco.ClickRate*100))
	sb.WriteString(fmt.Sprintf("• Bounce Rate: %.2f%%\n", eco.BounceRate*100))
	sb.WriteString(fmt.Sprintf("• Complaint Rate: %.4f%%\n\n", eco.ComplaintRate*100))

	sb.WriteString("**Provider Breakdown:**\n")
	providerStats := make(map[string]struct {
		volume    int64
		delivered int64
		isps      int
	})
	for _, isp := range allISPs {
		stats := providerStats[isp.Provider]
		stats.volume += isp.Volume
		stats.delivered += isp.Delivered
		stats.isps++
		providerStats[isp.Provider] = stats
	}
	for provider, stats := range providerStats {
		delRate := float64(stats.delivered) / float64(stats.volume) * 100
		sb.WriteString(fmt.Sprintf("• **%s**: %s sent, %.2f%% delivered, %d ISPs\n",
			strings.ToUpper(provider), formatNumber(stats.volume), delRate, stats.isps))
	}

	sb.WriteString(fmt.Sprintf("\n**ISP Health:** ✅ %d healthy | ⚠️ %d warning | ❌ %d critical\n",
		eco.HealthyISPs, eco.WarningISPs, eco.CriticalISPs))

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"Compare providers",
			"What concerns should I watch?",
			"Forecast for tomorrow",
			"How is Gmail performing across providers?",
		},
	}
}

// handleComparisonQuery compares providers
func (a *Agent) handleComparisonQuery(query string, allISPs []UnifiedISP) ChatResponse {
	var sb strings.Builder
	sb.WriteString("📊 **Provider Comparison**\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	// Aggregate by provider
	type ProviderStats struct {
		Volume        int64
		Delivered     int64
		Opens         int64
		Clicks        int64
		Bounces       int64
		Complaints    int64
		ISPCount      int
		HealthyCount  int
		WarningCount  int
		CriticalCount int
	}
	stats := make(map[string]*ProviderStats)

	for _, isp := range allISPs {
		if stats[isp.Provider] == nil {
			stats[isp.Provider] = &ProviderStats{}
		}
		s := stats[isp.Provider]
		s.Volume += isp.Volume
		s.Delivered += isp.Delivered
		s.Opens += int64(float64(isp.Delivered) * isp.OpenRate)
		s.Clicks += int64(float64(isp.Delivered) * isp.ClickRate)
		s.Bounces += int64(float64(isp.Volume) * isp.BounceRate)
		s.Complaints += int64(float64(isp.Delivered) * isp.ComplaintRate)
		s.ISPCount++
		switch isp.Status {
		case "healthy":
			s.HealthyCount++
		case "warning":
			s.WarningCount++
		case "critical":
			s.CriticalCount++
		}
	}

	// Sort providers by volume
	providers := make([]string, 0, len(stats))
	for p := range stats {
		providers = append(providers, p)
	}
	sort.Slice(providers, func(i, j int) bool {
		return stats[providers[i]].Volume > stats[providers[j]].Volume
	})

	sb.WriteString("| Provider | Volume | Delivery | Opens | Complaints | Health |\n")
	sb.WriteString("|----------|--------|----------|-------|------------|--------|\n")

	for _, p := range providers {
		s := stats[p]
		delRate := float64(s.Delivered) / float64(s.Volume) * 100
		openRate := float64(s.Opens) / float64(s.Delivered) * 100
		compRate := float64(s.Complaints) / float64(s.Delivered) * 100

		healthEmoji := "✅"
		if s.CriticalCount > 0 {
			healthEmoji = "❌"
		} else if s.WarningCount > 0 {
			healthEmoji = "⚠️"
		}

		sb.WriteString(fmt.Sprintf("| %s | %s | %.1f%% | %.1f%% | %.4f%% | %s |\n",
			strings.ToUpper(p), formatNumber(s.Volume), delRate, openRate, compRate, healthEmoji))
	}

	// Best performer analysis
	sb.WriteString("\n**Analysis:**\n")
	if len(providers) > 1 {
		bestDelivery := providers[0]
		bestOpen := providers[0]
		lowestComplaint := providers[0]

		for _, p := range providers {
			s := stats[p]
			if float64(s.Delivered)/float64(s.Volume) > float64(stats[bestDelivery].Delivered)/float64(stats[bestDelivery].Volume) {
				bestDelivery = p
			}
			if float64(s.Opens)/float64(s.Delivered) > float64(stats[bestOpen].Opens)/float64(stats[bestOpen].Delivered) {
				bestOpen = p
			}
			if float64(s.Complaints)/float64(s.Delivered) < float64(stats[lowestComplaint].Complaints)/float64(stats[lowestComplaint].Delivered) {
				lowestComplaint = p
			}
		}

		sb.WriteString(fmt.Sprintf("• Best delivery rate: **%s**\n", strings.ToUpper(bestDelivery)))
		sb.WriteString(fmt.Sprintf("• Best open rate: **%s**\n", strings.ToUpper(bestOpen)))
		sb.WriteString(fmt.Sprintf("• Lowest complaints: **%s**\n", strings.ToUpper(lowestComplaint)))
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"Show ecosystem overview",
			"How is SparkPost doing?",
			"What are the concerns?",
		},
	}
}

// handleProviderQuery handles queries about a specific provider
func (a *Agent) handleProviderQuery(provider string, allISPs []UnifiedISP) ChatResponse {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📧 **%s Performance**\n", strings.ToUpper(provider)))
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	var providerISPs []UnifiedISP
	var totalVolume, totalDelivered int64
	var healthy, warning, critical int

	for _, isp := range allISPs {
		if isp.Provider == provider {
			providerISPs = append(providerISPs, isp)
			totalVolume += isp.Volume
			totalDelivered += isp.Delivered
			switch isp.Status {
			case "healthy":
				healthy++
			case "warning":
				warning++
			case "critical":
				critical++
			}
		}
	}

	if len(providerISPs) == 0 {
		sb.WriteString("No data available for this provider.\n")
		return ChatResponse{Message: sb.String()}
	}

	// Sort by volume
	sort.Slice(providerISPs, func(i, j int) bool {
		return providerISPs[i].Volume > providerISPs[j].Volume
	})

	delRate := float64(totalDelivered) / float64(totalVolume) * 100
	sb.WriteString(fmt.Sprintf("**Total Volume:** %s\n", formatNumber(totalVolume)))
	sb.WriteString(fmt.Sprintf("**Delivery Rate:** %.2f%%\n", delRate))
	sb.WriteString(fmt.Sprintf("**ISP Health:** ✅ %d | ⚠️ %d | ❌ %d\n\n", healthy, warning, critical))

	sb.WriteString("**Top ISPs:**\n")
	for i, isp := range providerISPs {
		if i >= 5 {
			break
		}
		statusEmoji := "✅"
		if isp.Status == "critical" {
			statusEmoji = "❌"
		} else if isp.Status == "warning" {
			statusEmoji = "⚠️"
		}
		sb.WriteString(fmt.Sprintf("%s **%s**: %s sent, %.2f%% delivered, %.2f%% opens\n",
			statusEmoji, isp.ISP, formatNumber(isp.Volume), isp.DeliveryRate*100, isp.OpenRate*100))
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"Compare all providers",
			fmt.Sprintf("Should I increase %s volume?", provider),
			"Show ecosystem overview",
		},
	}
}

// handleISPQuery handles queries about specific ISPs
func (a *Agent) handleISPQuery(query string, allISPs []UnifiedISP) ChatResponse {
	ispNames := []string{"gmail", "yahoo", "outlook", "hotmail", "aol", "att", "icloud", "comcast"}
	var targetISP string
	for _, name := range ispNames {
		if strings.Contains(query, name) {
			targetISP = name
			break
		}
	}

	var sb strings.Builder
	if targetISP != "" {
		sb.WriteString(fmt.Sprintf("📬 **%s Performance Across Providers**\n", strings.Title(targetISP)))
	} else {
		sb.WriteString("📬 **ISP Performance Summary**\n")
	}
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	var matchingISPs []UnifiedISP
	for _, isp := range allISPs {
		if targetISP == "" || strings.Contains(strings.ToLower(isp.ISP), targetISP) {
			matchingISPs = append(matchingISPs, isp)
		}
	}

	if len(matchingISPs) == 0 {
		sb.WriteString("No matching ISP data found.\n")
		return ChatResponse{Message: sb.String()}
	}

	// Sort by volume
	sort.Slice(matchingISPs, func(i, j int) bool {
		return matchingISPs[i].Volume > matchingISPs[j].Volume
	})

	for _, isp := range matchingISPs {
		if len(matchingISPs) > 10 && isp.Volume < 1000 {
			continue // Skip very small ISPs
		}
		statusEmoji := "✅"
		if isp.Status == "critical" {
			statusEmoji = "❌"
		} else if isp.Status == "warning" {
			statusEmoji = "⚠️"
		}
		sb.WriteString(fmt.Sprintf("%s **%s** (%s)\n", statusEmoji, isp.ISP, strings.ToUpper(isp.Provider)))
		sb.WriteString(fmt.Sprintf("   Volume: %s | Delivery: %.2f%% | Opens: %.2f%% | Complaints: %.4f%%\n\n",
			formatNumber(isp.Volume), isp.DeliveryRate*100, isp.OpenRate*100, isp.ComplaintRate*100))
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"Compare providers",
			"What concerns should I watch?",
			"Show ecosystem overview",
		},
	}
}

// handleUnifiedConcernsQuery handles concerns across all ESPs
func (a *Agent) handleUnifiedConcernsQuery(eco EcosystemSummary, allISPs []UnifiedISP) ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("⚠️ **Ecosystem Concerns**\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	var concerns []string

	// Check ISP status
	for _, isp := range allISPs {
		if isp.Status == "critical" {
			concerns = append(concerns, fmt.Sprintf("🔴 **%s** (%s): %s",
				isp.ISP, strings.ToUpper(isp.Provider), isp.StatusReason))
		} else if isp.Status == "warning" {
			concerns = append(concerns, fmt.Sprintf("🟡 **%s** (%s): %s",
				isp.ISP, strings.ToUpper(isp.Provider), isp.StatusReason))
		}
	}

	// Check alerts
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

	// Check ecosystem-level concerns
	if eco.ComplaintRate > 0.001 {
		concerns = append(concerns, fmt.Sprintf("🔴 **Ecosystem**: Overall complaint rate (%.4f%%) is elevated", eco.ComplaintRate*100))
	}
	if eco.BounceRate > 0.05 {
		concerns = append(concerns, fmt.Sprintf("🟡 **Ecosystem**: Overall bounce rate (%.2f%%) is high", eco.BounceRate*100))
	}

	if len(concerns) == 0 {
		sb.WriteString("✅ **All Clear!**\n\n")
		sb.WriteString("No significant concerns across the ecosystem.\n")
		sb.WriteString(fmt.Sprintf("• %d providers active\n", eco.ProviderCount))
		sb.WriteString(fmt.Sprintf("• %d ISPs monitored\n", eco.ISPCount))
		sb.WriteString(fmt.Sprintf("• Overall delivery: %.2f%%\n", eco.DeliveryRate*100))
	} else {
		sb.WriteString(fmt.Sprintf("Found **%d** items requiring attention:\n\n", len(concerns)))
		for i, concern := range concerns {
			if i >= 10 {
				sb.WriteString(fmt.Sprintf("\n... and %d more concerns\n", len(concerns)-10))
				break
			}
			sb.WriteString(fmt.Sprintf("%d. %s\n\n", i+1, concern))
		}
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"What are your recommendations?",
			"Show ecosystem overview",
			"Compare providers",
		},
	}
}

// handleUnifiedRecommendationQuery provides recommendations across all ESPs
func (a *Agent) handleUnifiedRecommendationQuery(query string, eco EcosystemSummary, allISPs []UnifiedISP) ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("💡 **Ecosystem Recommendations**\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	var recommendations []string

	// Critical issues first
	for _, isp := range allISPs {
		if isp.Status == "critical" {
			recommendations = append(recommendations, fmt.Sprintf(
				"🔴 **%s (%s)**: Reduce volume or pause sends. %s",
				isp.ISP, strings.ToUpper(isp.Provider), isp.StatusReason))
		}
	}

	// Warnings
	for _, isp := range allISPs {
		if isp.Status == "warning" {
			recommendations = append(recommendations, fmt.Sprintf(
				"🟡 **%s (%s)**: Monitor closely. %s",
				isp.ISP, strings.ToUpper(isp.Provider), isp.StatusReason))
		}
	}

	// Provider-level recommendations
	providerHealthy := make(map[string]int)
	providerTotal := make(map[string]int)
	for _, isp := range allISPs {
		providerTotal[isp.Provider]++
		if isp.Status == "healthy" {
			providerHealthy[isp.Provider]++
		}
	}
	for provider, total := range providerTotal {
		healthy := providerHealthy[provider]
		if healthy == total && total > 0 {
			recommendations = append(recommendations, fmt.Sprintf(
				"✅ **%s**: All ISPs healthy. Safe to gradually increase volume.",
				strings.ToUpper(provider)))
		}
	}

	// Correlation-based recommendations
	for _, corr := range a.correlations {
		if corr.Confidence > 0.7 {
			recommendations = append(recommendations, fmt.Sprintf(
				"📈 **%s**: When %s > %.0f, %s increases ~%.0f%%. Watch this threshold.",
				corr.EntityName, corr.TriggerMetric, corr.TriggerThreshold,
				corr.EffectMetric, corr.EffectChange*100))
		}
	}

	if len(recommendations) == 0 {
		sb.WriteString("✅ **All systems healthy!**\n\n")
		sb.WriteString("No specific recommendations. Continue current practices.\n")
	} else {
		for i, rec := range recommendations {
			if i >= 8 {
				break
			}
			sb.WriteString(fmt.Sprintf("%d. %s\n\n", i+1, rec))
		}
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"What concerns should I watch?",
			"Forecast for tomorrow",
			"Compare providers",
		},
	}
}

// handleUnifiedVolumeQuery handles volume queries across ESPs
func (a *Agent) handleUnifiedVolumeQuery(query string, eco EcosystemSummary, allISPs []UnifiedISP) ChatResponse {
	var sb strings.Builder
	sb.WriteString("📈 **Volume Analysis**\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	sb.WriteString(fmt.Sprintf("**Total Ecosystem Volume:** %s\n\n", formatNumber(eco.TotalVolume)))

	// Group by provider
	providerVolumes := make(map[string]int64)
	for _, isp := range allISPs {
		providerVolumes[isp.Provider] += isp.Volume
	}

	sb.WriteString("**By Provider:**\n")
	for provider, volume := range providerVolumes {
		pct := float64(volume) / float64(eco.TotalVolume) * 100
		sb.WriteString(fmt.Sprintf("• %s: %s (%.1f%%)\n", strings.ToUpper(provider), formatNumber(volume), pct))
	}

	sb.WriteString("\n**Volume Recommendations:**\n")
	for _, isp := range allISPs {
		if isp.Volume > 100000 { // Only show significant ISPs
			if isp.Status == "healthy" {
				sb.WriteString(fmt.Sprintf("✅ %s (%s): Safe to increase\n", isp.ISP, strings.ToUpper(isp.Provider)))
			} else if isp.Status == "warning" {
				sb.WriteString(fmt.Sprintf("⚠️ %s (%s): Maintain current level\n", isp.ISP, strings.ToUpper(isp.Provider)))
			} else {
				sb.WriteString(fmt.Sprintf("❌ %s (%s): Consider reducing\n", isp.ISP, strings.ToUpper(isp.Provider)))
			}
		}
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"Show ecosystem overview",
			"What are the concerns?",
			"Compare providers",
		},
	}
}
