package agent

import "strings"

func (o *OpenAIAgent) getEcosystemAssessment() map[string]interface{} {
	if o.knowledgeBase == nil {
		return map[string]interface{}{"error": "Knowledge base not initialized"}
	}
	
	o.knowledgeBase.mu.RLock()
	defer o.knowledgeBase.mu.RUnlock()
	
	eco := o.knowledgeBase.EcosystemState
	bp := o.knowledgeBase.BestPractices
	
	var gaps []map[string]interface{}
	if eco.BaselineDeliveryRate > 0 {
		if eco.BaselineDeliveryRate < bp.DeliveryRateTarget {
			gaps = append(gaps, map[string]interface{}{
				"metric":  "delivery_rate",
				"current": eco.BaselineDeliveryRate,
				"target":  bp.DeliveryRateTarget,
				"gap":     bp.DeliveryRateTarget - eco.BaselineDeliveryRate,
				"status":  "below_target",
			})
		}
	}
	if eco.BaselineBounceRate > bp.BounceRateThreshold {
		gaps = append(gaps, map[string]interface{}{
			"metric":  "bounce_rate",
			"current": eco.BaselineBounceRate,
			"target":  bp.BounceRateThreshold,
			"gap":     eco.BaselineBounceRate - bp.BounceRateThreshold,
			"status":  "above_threshold",
		})
	}
	if eco.BaselineComplaintRate > bp.ComplaintRateThreshold {
		gaps = append(gaps, map[string]interface{}{
			"metric":  "complaint_rate",
			"current": eco.BaselineComplaintRate,
			"target":  bp.ComplaintRateThreshold,
			"gap":     eco.BaselineComplaintRate - bp.ComplaintRateThreshold,
			"status":  "above_threshold",
		})
	}
	
	return map[string]interface{}{
		"overall_health":        eco.OverallHealth,
		"health_score":          eco.HealthScore,
		"last_assessment":       eco.LastAssessment,
		"daily_avg_volume":      eco.DailyAverageVolume,
		"daily_avg_revenue":     eco.DailyAverageRevenue,
		"baseline_delivery_rate": eco.BaselineDeliveryRate,
		"baseline_open_rate":    eco.BaselineOpenRate,
		"baseline_click_rate":   eco.BaselineClickRate,
		"baseline_bounce_rate":  eco.BaselineBounceRate,
		"baseline_complaint_rate": eco.BaselineComplaintRate,
		"esp_volume_share":      eco.ESPVolumeShare,
		"esp_health_status":     eco.ESPHealthStatus,
		"active_issues":         eco.ActiveIssues,
		"resolved_issues":       eco.ResolvedIssues,
		"performance_gaps":      gaps,
		"weekly_trend":          eco.WeeklyTrend,
		"monthly_trend":         eco.MonthlyTrend,
		"revenue_trend":         eco.RevenueTrend,
	}
}

func (o *OpenAIAgent) getPerformanceBenchmarks(metric string) []map[string]interface{} {
	if o.knowledgeBase == nil {
		return nil
	}
	
	o.knowledgeBase.mu.RLock()
	defer o.knowledgeBase.mu.RUnlock()
	
	var results []map[string]interface{}
	for key, benchmark := range o.knowledgeBase.PerformanceBenchmarks {
		if metric != "" && !strings.Contains(strings.ToLower(key), strings.ToLower(metric)) {
			continue
		}
		results = append(results, map[string]interface{}{
			"metric":           benchmark.MetricName,
			"entity_type":      benchmark.EntityType,
			"entity_name":      benchmark.EntityName,
			"current_value":    benchmark.CurrentValue,
			"target_value":     benchmark.TargetValue,
			"industry_average": benchmark.IndustryAverage,
			"best_in_class":    benchmark.BestInClass,
			"last_7_days":      benchmark.Last7Days,
			"last_30_days":     benchmark.Last30Days,
			"trend":            benchmark.Trend,
			"trend_percentage": benchmark.TrendPercentage,
			"status":           benchmark.Status,
			"gap":              benchmark.Gap,
		})
	}
	
	if len(results) == 0 {
		bp := o.knowledgeBase.BestPractices
		results = append(results, 
			map[string]interface{}{
				"metric":           "delivery_rate",
				"target_value":     bp.DeliveryRateTarget,
				"industry_average": 0.95,
				"description":      "Percentage of emails successfully delivered",
			},
			map[string]interface{}{
				"metric":           "bounce_rate",
				"target_value":     bp.BounceRateThreshold,
				"industry_average": 0.02,
				"description":      "Keep below 2% - higher indicates list hygiene issues",
			},
			map[string]interface{}{
				"metric":           "complaint_rate",
				"target_value":     bp.ComplaintRateThreshold,
				"industry_average": 0.001,
				"description":      "Keep below 0.1% - critical for ISP reputation",
			},
			map[string]interface{}{
				"metric":           "open_rate",
				"target_value":     bp.OpenRateHealthy,
				"industry_average": 0.18,
				"description":      "Industry average is 15-25% depending on industry",
			},
			map[string]interface{}{
				"metric":           "click_rate",
				"target_value":     bp.ClickRateHealthy,
				"industry_average": 0.025,
				"description":      "Industry average is 2-5%",
			},
		)
	}
	
	return results
}

func (o *OpenAIAgent) getISPBestPractices(isp string) map[string]interface{} {
	if o.knowledgeBase == nil {
		return map[string]interface{}{"error": "Knowledge base not initialized"}
	}
	
	o.knowledgeBase.mu.RLock()
	defer o.knowledgeBase.mu.RUnlock()
	
	bp := o.knowledgeBase.BestPractices
	
	if isp != "" {
		ispLower := strings.ToLower(isp)
		if profile, ok := o.knowledgeBase.ISPKnowledge[ispLower]; ok {
			var guidelines []string
			switch ispLower {
			case "gmail":
				guidelines = bp.GmailGuidelines
			case "yahoo":
				guidelines = bp.YahooGuidelines
			case "outlook", "microsoft":
				guidelines = bp.OutlookGuidelines
			case "apple":
				guidelines = bp.AppleGuidelines
			}
			
			return map[string]interface{}{
				"isp":                isp,
				"max_complaint_rate": profile.MaxComplaintRate,
				"max_bounce_rate":    profile.MaxBounceRate,
				"current_delivery":   profile.DeliveryRate,
				"current_bounce":     profile.BounceRate,
				"current_complaints": profile.ComplaintRate,
				"status":             profile.Status,
				"status_reason":      profile.StatusReason,
				"best_practices":     profile.BestPractices,
				"guidelines":         guidelines,
				"known_issues":       profile.KnownIssues,
				"recommendations":    profile.Recommendations,
			}
		}
	}
	
	return map[string]interface{}{
		"gmail":   bp.GmailGuidelines,
		"yahoo":   bp.YahooGuidelines,
		"outlook": bp.OutlookGuidelines,
		"apple":   bp.AppleGuidelines,
		"general_thresholds": map[string]interface{}{
			"max_complaint_rate": 0.001,
			"max_bounce_rate":    0.02,
		},
	}
}

func (o *OpenAIAgent) getComplianceRequirements(regulation string) map[string]interface{} {
	if o.knowledgeBase == nil {
		return map[string]interface{}{"error": "Knowledge base not initialized"}
	}
	
	compliance := o.knowledgeBase.GetComplianceChecklist()
	
	if regulation != "" {
		regLower := strings.ToLower(regulation)
		if rules, ok := compliance[regLower]; ok {
			return map[string]interface{}{
				"regulation": regulation,
				"rules":      rules,
			}
		}
	}
	
	return map[string]interface{}{
		"canspam":    compliance["canspam"],
		"gdpr":       compliance["gdpr"],
		"ccpa":       compliance["ccpa"],
		"casl":       compliance["casl"],
		"required":   compliance["required"],
		"prohibited": compliance["prohibited"],
	}
}

func (o *OpenAIAgent) getIndustryBestPractices(category string) map[string]interface{} {
	if o.knowledgeBase == nil {
		return map[string]interface{}{"error": "Knowledge base not initialized"}
	}
	
	o.knowledgeBase.mu.RLock()
	defer o.knowledgeBase.mu.RUnlock()
	
	bp := o.knowledgeBase.BestPractices
	
	result := make(map[string]interface{})
	
	catLower := strings.ToLower(category)
	
	if catLower == "" || catLower == "deliverability" {
		result["deliverability"] = map[string]interface{}{
			"delivery_rate_target":     bp.DeliveryRateTarget,
			"bounce_rate_threshold":    bp.BounceRateThreshold,
			"complaint_rate_threshold": bp.ComplaintRateThreshold,
		}
	}
	
	if catLower == "" || catLower == "engagement" {
		result["engagement"] = map[string]interface{}{
			"open_rate_healthy":      bp.OpenRateHealthy,
			"click_rate_healthy":     bp.ClickRateHealthy,
			"unsubscribe_rate_max":   bp.UnsubscribeRateMax,
			"list_cleaning_frequency": bp.ListCleaningFrequency,
			"inactive_subscriber_days": bp.InactiveSubscriberDays,
		}
	}
	
	if catLower == "" || catLower == "authentication" {
		result["authentication"] = bp.AuthenticationRequired
	}
	
	if catLower == "" || catLower == "warmup" {
		result["warmup"] = bp.WarmupGuidelines
	}
	
	if catLower == "" || catLower == "content" {
		result["content"] = bp.ContentGuidelines
	}
	
	if catLower == "" || catLower == "send_time" {
		result["send_time"] = bp.SendTimeOptimization
	}
	
	return result
}

func (o *OpenAIAgent) getHistoricalInsights(category string, limit int) []map[string]interface{} {
	if o.knowledgeBase == nil {
		return nil
	}
	
	o.knowledgeBase.mu.RLock()
	defer o.knowledgeBase.mu.RUnlock()
	
	var results []map[string]interface{}
	
	for _, insight := range o.knowledgeBase.HistoricalInsights {
		if category != "" && !strings.EqualFold(insight.Category, category) {
			continue
		}
		
		results = append(results, map[string]interface{}{
			"id":               insight.ID,
			"generated_at":     insight.GeneratedAt,
			"time_range":       insight.TimeRange,
			"category":         insight.Category,
			"title":            insight.Title,
			"summary":          insight.Summary,
			"key_findings":     insight.KeyFindings,
			"recommendations":  insight.Recommendations,
		})
		
		if len(results) >= limit {
			break
		}
	}
	
	return results
}

func (o *OpenAIAgent) getKanbanTasks(status, priority string) map[string]interface{} {
	if o.agent.collectors == nil || o.agent.collectors.Kanban == nil {
		return map[string]interface{}{
			"error": "Kanban service not available",
			"note":  "Tasks are managed through the Tasks tab",
		}
	}
	
	return map[string]interface{}{
		"note":           "Query the Kanban board through the Tasks tab for current task status",
		"active_filter":  status,
		"priority_filter": priority,
		"recommendation": "Review the Tasks board to see AI-generated tasks addressing ecosystem issues",
	}
}

func (o *OpenAIAgent) getLastAnalysis() map[string]interface{} {
	if o.knowledgeBase == nil || o.knowledgeBase.LastAnalysis == nil {
		return map[string]interface{}{"error": "No analysis data available yet"}
	}
	
	o.knowledgeBase.mu.RLock()
	defer o.knowledgeBase.mu.RUnlock()
	
	analysis := o.knowledgeBase.LastAnalysis
	
	return map[string]interface{}{
		"timestamp":           analysis.Timestamp,
		"analysis_duration":   analysis.AnalysisDuration,
		"ecosystem_health":    analysis.EcosystemHealth,
		"health_score":        analysis.HealthScore,
		"total_volume_24h":    analysis.TotalVolume24h,
		"total_revenue_24h":   analysis.TotalRevenue24h,
		"avg_delivery_rate":   analysis.AvgDeliveryRate,
		"avg_bounce_rate":     analysis.AvgBounceRate,
		"avg_complaint_rate":  analysis.AvgComplaintRate,
		"critical_issues":     analysis.CriticalIssues,
		"warning_issues":      analysis.WarningIssues,
		"immediate_actions":   analysis.ImmediateActions,
		"short_term_actions":  analysis.ShortTermActions,
		"long_term_actions":   analysis.LongTermActions,
		"new_patterns_found":  analysis.NewPatternsFound,
		"trend_changes":       analysis.TrendChanges,
		"active_kanban_tasks": analysis.ActiveKanbanTasks,
	}
}

func (o *OpenAIAgent) getKnowledgeSummary() map[string]interface{} {
	if o.knowledgeBase == nil {
		return map[string]interface{}{"error": "Knowledge base not initialized"}
	}
	
	return o.knowledgeBase.GetKnowledgeSummary()
}
