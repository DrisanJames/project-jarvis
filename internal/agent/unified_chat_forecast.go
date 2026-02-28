package agent

import (
	"fmt"
	"math"
	"strings"
)

// handleUnifiedForecastQuery provides forecasting across ESPs
func (a *Agent) handleUnifiedForecastQuery(query string, eco EcosystemSummary, allISPs []UnifiedISP) ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("🔮 **Ecosystem Forecast**\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	if len(a.baselines) < 3 {
		sb.WriteString("⏳ **Building Forecasting Models**\n\n")
		sb.WriteString("I'm still learning patterns from your data.\n")
		sb.WriteString(fmt.Sprintf("Current baselines: %d\n", len(a.baselines)))
		sb.WriteString(fmt.Sprintf("Current correlations: %d\n\n", len(a.correlations)))
		sb.WriteString("I'll provide accurate forecasts once I have enough historical data.\n")
	} else {
		sb.WriteString("Based on learned patterns and current trends:\n\n")

		// Trend analysis for each provider
		for _, isp := range allISPs {
			if isp.Volume < 10000 {
				continue // Skip small volumes
			}

			key := fmt.Sprintf("%s:isp:%s", isp.Provider, isp.ISP)
			baseline, hasBaseline := a.baselines[key]

			if !hasBaseline {
				key = fmt.Sprintf("isp:%s", isp.ISP) // Try without provider prefix
				baseline, hasBaseline = a.baselines[key]
			}

			if hasBaseline {
				sb.WriteString(fmt.Sprintf("**%s (%s):**\n", isp.ISP, strings.ToUpper(isp.Provider)))

				// Complaint trend
				if mb, ok := baseline.Metrics["complaint_rate"]; ok && mb.StdDev > 0 {
					deviation := (isp.ComplaintRate - mb.Mean) / mb.StdDev
					trend := "stable →"
					if deviation > 1 {
						trend = "trending up ⬆️"
					} else if deviation < -1 {
						trend = "trending down ⬇️"
					}
					sb.WriteString(fmt.Sprintf("  • Complaints: %s (%.1fσ from baseline)\n", trend, deviation))
				}

				// Volume correlation risk
				for _, corr := range a.correlations {
					if corr.EntityName == isp.ISP && corr.TriggerMetric == "volume" {
						if float64(isp.Volume) > corr.TriggerThreshold*0.9 {
							sb.WriteString(fmt.Sprintf("  ⚠️ Volume approaching threshold (%.0f)\n", corr.TriggerThreshold))
						}
					}
				}
				sb.WriteString("\n")
			}
		}

		// Overall forecast
		sb.WriteString("**Overall Ecosystem Forecast:**\n")
		if eco.CriticalISPs > 0 {
			sb.WriteString("⚠️ Critical issues present - expect continued challenges without intervention\n")
		} else if eco.WarningISPs > eco.HealthyISPs {
			sb.WriteString("⚡ Multiple warnings - monitor closely, may need adjustments\n")
		} else {
			sb.WriteString("✅ Ecosystem healthy - expect stable performance if current practices maintained\n")
		}
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"What are the current concerns?",
			"Show learned patterns",
			"How is overall performance?",
		},
	}
}

// handleUnifiedLearningQuery shows what the agent has learned
func (a *Agent) handleUnifiedLearningQuery() ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("🧠 **Agent Learning Status**\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	sb.WriteString("**Baselines Learned:**\n")
	if len(a.baselines) == 0 {
		sb.WriteString("  No baselines established yet.\n")
	} else {
		// Group by provider
		providerBaselines := make(map[string]int)
		for key := range a.baselines {
			parts := strings.Split(key, ":")
			if len(parts) > 0 {
				providerBaselines[parts[0]]++
			}
		}
		for provider, count := range providerBaselines {
			sb.WriteString(fmt.Sprintf("  • %s: %d baselines\n", provider, count))
		}
	}

	sb.WriteString(fmt.Sprintf("\n**Total Baselines:** %d\n", len(a.baselines)))
	sb.WriteString(fmt.Sprintf("**Correlations Found:** %d\n", len(a.correlations)))
	sb.WriteString(fmt.Sprintf("**Rolling Stats Tracked:** %d metrics\n\n", len(a.rollingStats)))

	if len(a.correlations) > 0 {
		sb.WriteString("**Key Correlations:**\n")
		for i, corr := range a.correlations {
			if i >= 5 {
				break
			}
			sb.WriteString(fmt.Sprintf("  %d. %s: %s > %.0f → %s ↑ (%.0f%% confidence)\n",
				i+1, corr.EntityName, corr.TriggerMetric, corr.TriggerThreshold,
				corr.EffectMetric, corr.Confidence*100))
		}
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"Show ecosystem overview",
			"What are the forecasts?",
			"What concerns should I watch?",
		},
	}
}

// handleUnifiedGeneralQuery handles general queries
func (a *Agent) handleUnifiedGeneralQuery(eco EcosystemSummary) ChatResponse {
	var sb strings.Builder
	sb.WriteString("👋 **Email Ecosystem Agent**\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")
	sb.WriteString("I'm your intelligent email analytics agent. I monitor and learn from your entire email ecosystem across all providers.\n\n")

	sb.WriteString("**Currently Monitoring:**\n")
	sb.WriteString(fmt.Sprintf("• %d email providers\n", eco.ProviderCount))
	sb.WriteString(fmt.Sprintf("• %d ISPs\n", eco.ISPCount))
	sb.WriteString(fmt.Sprintf("• %s total volume\n\n", formatNumber(eco.TotalVolume)))

	sb.WriteString("**I can help with:**\n")
	sb.WriteString("• 📊 Ecosystem overview (\"How is overall performance?\")\n")
	sb.WriteString("• 📈 Provider comparison (\"Compare SparkPost vs SES\")\n")
	sb.WriteString("• ⚠️ Concern detection (\"What should I watch?\")\n")
	sb.WriteString("• 💡 Recommendations (\"What should I do?\")\n")
	sb.WriteString("• 🔮 Forecasting (\"What do you predict for tomorrow?\")\n")
	sb.WriteString("• 📧 ISP analysis (\"How is Gmail doing?\")\n\n")
	sb.WriteString("What would you like to know?\n")

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"Show ecosystem overview",
			"Compare all providers",
			"What concerns should I watch?",
			"Forecast for tomorrow",
		},
	}
}

// ForecastVolume predicts volume based on historical patterns
func (a *Agent) ForecastVolume(provider string, days int) ([]int64, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	key := fmt.Sprintf("%s:volume", provider)
	stats, exists := a.rollingStats[key]
	if !exists || len(stats.Values) < 7 {
		return nil, fmt.Errorf("insufficient data for forecasting")
	}

	// Simple moving average forecast
	windowSize := 7
	if len(stats.Values) < windowSize {
		windowSize = len(stats.Values)
	}

	recentValues := stats.Values[len(stats.Values)-windowSize:]
	var sum float64
	for _, v := range recentValues {
		sum += v
	}
	avg := sum / float64(windowSize)

	// Calculate trend
	trend := 0.0
	if len(recentValues) >= 2 {
		trend = (recentValues[len(recentValues)-1] - recentValues[0]) / float64(len(recentValues))
	}

	forecasts := make([]int64, days)
	for i := 0; i < days; i++ {
		forecasts[i] = int64(math.Max(0, avg+trend*float64(i+1)))
	}

	return forecasts, nil
}
