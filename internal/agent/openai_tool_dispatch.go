package agent

import "encoding/json"

// executeTool executes a tool and returns the result as JSON
func (o *OpenAIAgent) executeTool(name, arguments string) string {
	o.agent.mu.RLock()
	defer o.agent.mu.RUnlock()

	var args map[string]interface{}
	json.Unmarshal([]byte(arguments), &args)

	var result interface{}

	switch name {
	case "get_ecosystem_summary":
		result = o.getEcosystemSummary()
	case "get_isp_performance":
		isp, _ := args["isp"].(string)
		provider, _ := args["provider"].(string)
		result = o.getISPPerformance(isp, provider)
	case "get_provider_comparison":
		result = o.getProviderComparison()
	case "get_revenue_summary":
		days := 30
		if d, ok := args["days"].(float64); ok {
			days = int(d)
		}
		result = o.getRevenueSummary(days)
	case "get_daily_revenue":
		days := 7
		if d, ok := args["days"].(float64); ok {
			days = int(d)
		}
		result = o.getDailyRevenue(days)
	case "get_offer_performance":
		topN := 10
		sortBy := "revenue"
		if n, ok := args["top_n"].(float64); ok {
			topN = int(n)
		}
		if s, ok := args["sort_by"].(string); ok {
			sortBy = s
		}
		result = o.getOfferPerformance(topN, sortBy)
	case "get_property_performance":
		topN := 10
		if n, ok := args["top_n"].(float64); ok {
			topN = int(n)
		}
		result = o.getPropertyPerformance(topN)
	case "get_campaign_revenue":
		topN := 20
		minRevenue := 0.0
		if n, ok := args["top_n"].(float64); ok {
			topN = int(n)
		}
		if m, ok := args["min_revenue"].(float64); ok {
			minRevenue = m
		}
		result = o.getCampaignRevenue(topN, minRevenue)
	case "get_ongage_campaigns":
		topN := 20
		espFilter := ""
		if n, ok := args["top_n"].(float64); ok {
			topN = int(n)
		}
		if e, ok := args["esp_filter"].(string); ok {
			espFilter = e
		}
		result = o.getOngageCampaigns(topN, espFilter)
	case "get_subject_line_analysis":
		topN := 10
		perfFilter := ""
		if n, ok := args["top_n"].(float64); ok {
			topN = int(n)
		}
		if p, ok := args["performance_filter"].(string); ok {
			perfFilter = p
		}
		result = o.getSubjectLineAnalysis(topN, perfFilter)
	case "get_send_time_analysis":
		result = o.getSendTimeAnalysis()
	case "get_audience_segments":
		minCampaigns := 10
		if m, ok := args["min_campaigns"].(float64); ok {
			minCampaigns = int(m)
		}
		result = o.getAudienceSegments(minCampaigns)
	case "get_pipeline_metrics":
		days := 7
		if d, ok := args["days"].(float64); ok {
			days = int(d)
		}
		result = o.getPipelineMetrics(days)
	case "get_active_alerts":
		result = o.getActiveAlerts()
	case "get_learned_patterns":
		result = o.getLearnedPatterns()
	// Knowledge Base tools
	case "get_ecosystem_assessment":
		result = o.getEcosystemAssessment()
	case "get_performance_benchmarks":
		metric, _ := args["metric"].(string)
		result = o.getPerformanceBenchmarks(metric)
	case "get_isp_best_practices":
		isp, _ := args["isp"].(string)
		result = o.getISPBestPractices(isp)
	case "get_compliance_requirements":
		regulation, _ := args["regulation"].(string)
		result = o.getComplianceRequirements(regulation)
	case "get_industry_best_practices":
		category, _ := args["category"].(string)
		result = o.getIndustryBestPractices(category)
	case "get_historical_insights":
		category, _ := args["category"].(string)
		limit := 10
		if l, ok := args["limit"].(float64); ok {
			limit = int(l)
		}
		result = o.getHistoricalInsights(category, limit)
	case "get_kanban_tasks":
		status, _ := args["status"].(string)
		priority, _ := args["priority"].(string)
		result = o.getKanbanTasks(status, priority)
	case "get_last_analysis":
		result = o.getLastAnalysis()
	case "get_knowledge_summary":
		result = o.getKnowledgeSummary()
	default:
		result = map[string]string{"error": "Unknown tool: " + name}
	}

	jsonResult, _ := json.Marshal(result)
	return string(jsonResult)
}
