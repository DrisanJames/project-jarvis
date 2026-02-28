package agent

// GetTools returns all available tool definitions
func (o *OpenAIAgent) GetTools() []Tool {
	return []Tool{
		// ESP Performance Tools
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_ecosystem_summary",
				Description: "Get overall email ecosystem summary across all ESP providers (SparkPost, Mailgun, SES) including total volume, delivery rates, open rates, click rates, bounce rates, and complaint rates",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
					"required":   []string{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_isp_performance",
				Description: "Get performance metrics for specific ISPs (Gmail, Yahoo, Outlook, etc.) or all ISPs. Returns delivery rate, open rate, click rate, bounce rate, complaint rate, and health status.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"isp": map[string]interface{}{
							"type":        "string",
							"description": "ISP name to filter (e.g., 'gmail', 'yahoo', 'outlook'). Leave empty for all ISPs.",
						},
						"provider": map[string]interface{}{
							"type":        "string",
							"description": "ESP provider to filter (e.g., 'sparkpost', 'mailgun', 'ses'). Leave empty for all providers.",
						},
					},
					"required": []string{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_provider_comparison",
				Description: "Compare performance metrics across ESP providers (SparkPost, Mailgun, AWS SES)",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
					"required":   []string{},
				},
			},
		},
		// Everflow/Revenue Tools
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_revenue_summary",
				Description: "Get Everflow revenue summary including total revenue, conversions, clicks, EPC, and conversion rates",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"days": map[string]interface{}{
							"type":        "integer",
							"description": "Number of days to include (default: 30)",
						},
					},
					"required": []string{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_daily_revenue",
				Description: "Get daily revenue breakdown from Everflow",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"days": map[string]interface{}{
							"type":        "integer",
							"description": "Number of days to retrieve (default: 7)",
						},
					},
					"required": []string{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_offer_performance",
				Description: "Get Everflow offer performance including revenue, conversions, clicks, and EPC for each offer",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"top_n": map[string]interface{}{
							"type":        "integer",
							"description": "Number of top offers to return (default: 10)",
						},
						"sort_by": map[string]interface{}{
							"type":        "string",
							"description": "Sort by: 'revenue', 'conversions', 'epc', or 'clicks' (default: revenue)",
						},
					},
					"required": []string{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_property_performance",
				Description: "Get performance by property/sending domain from Everflow including revenue, conversions, and EPC",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"top_n": map[string]interface{}{
							"type":        "integer",
							"description": "Number of top properties to return (default: 10)",
						},
					},
					"required": []string{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_campaign_revenue",
				Description: "Get campaign-level revenue data from Everflow including revenue, clicks, conversions, and audience size from Ongage",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"top_n": map[string]interface{}{
							"type":        "integer",
							"description": "Number of top campaigns to return (default: 20)",
						},
						"min_revenue": map[string]interface{}{
							"type":        "number",
							"description": "Minimum revenue filter",
						},
					},
					"required": []string{},
				},
			},
		},
		// Ongage/Campaign Tools
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_ongage_campaigns",
				Description: "Get Ongage campaign data including sent, delivered, opens, clicks, and performance metrics",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"top_n": map[string]interface{}{
							"type":        "integer",
							"description": "Number of campaigns to return (default: 20)",
						},
						"esp_filter": map[string]interface{}{
							"type":        "string",
							"description": "Filter by ESP (e.g., 'sparkpost', 'mailgun', 'ses')",
						},
					},
					"required": []string{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_subject_line_analysis",
				Description: "Get subject line analysis from Ongage including performance ratings, feature analysis (emojis, numbers, urgency), and open rates",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"top_n": map[string]interface{}{
							"type":        "integer",
							"description": "Number of subjects to return (default: 10)",
						},
						"performance_filter": map[string]interface{}{
							"type":        "string",
							"description": "Filter by performance: 'high', 'medium', 'low'",
						},
					},
					"required": []string{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_send_time_analysis",
				Description: "Get optimal send time analysis from Ongage showing best days and hours to send emails",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
					"required":   []string{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_audience_segments",
				Description: "Get audience segment analysis from Ongage including engagement levels, open rates, and click rates",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"min_campaigns": map[string]interface{}{
							"type":        "integer",
							"description": "Minimum campaign count filter (default: 10)",
						},
					},
					"required": []string{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_pipeline_metrics",
				Description: "Get daily pipeline metrics from Ongage showing daily send volumes, delivery rates, and engagement",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"days": map[string]interface{}{
							"type":        "integer",
							"description": "Number of days (default: 7)",
						},
					},
					"required": []string{},
				},
			},
		},
		// Alert/Health Tools
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_active_alerts",
				Description: "Get current active alerts and concerns across the email ecosystem",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
					"required":   []string{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_learned_patterns",
				Description: "Get patterns and correlations the agent has learned from historical data",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
					"required":   []string{},
				},
			},
		},
		// Knowledge Base Tools
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_ecosystem_assessment",
				Description: "Get comprehensive ecosystem health assessment including health score, learned baselines, trends, and issues from the AI knowledge base",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
					"required":   []string{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_performance_benchmarks",
				Description: "Get performance benchmarks for ecosystem metrics including targets, industry averages, current values, and gaps",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"metric": map[string]interface{}{
							"type":        "string",
							"description": "Specific metric to get benchmark for (e.g., 'delivery_rate', 'bounce_rate', 'complaint_rate'). Leave empty for all.",
						},
					},
					"required": []string{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_isp_best_practices",
				Description: "Get best practices and guidelines for specific ISPs (Gmail, Yahoo, Outlook, Apple) including thresholds and recommendations",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"isp": map[string]interface{}{
							"type":        "string",
							"description": "ISP name (gmail, yahoo, outlook, apple). Leave empty for all ISPs.",
						},
					},
					"required": []string{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_compliance_requirements",
				Description: "Get email compliance requirements including CAN-SPAM, GDPR, CCPA, CASL rules and prohibited practices",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"regulation": map[string]interface{}{
							"type":        "string",
							"description": "Specific regulation (canspam, gdpr, ccpa, casl). Leave empty for all.",
						},
					},
					"required": []string{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_industry_best_practices",
				Description: "Get general email marketing best practices including deliverability targets, engagement benchmarks, list hygiene, authentication, and send time optimization",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"category": map[string]interface{}{
							"type":        "string",
							"description": "Category: 'deliverability', 'engagement', 'authentication', 'warmup', 'content', 'send_time'. Leave empty for all.",
						},
					},
					"required": []string{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_historical_insights",
				Description: "Get historical insights and analysis the agent has generated from analyzing past data",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"category": map[string]interface{}{
							"type":        "string",
							"description": "Category filter: 'revenue', 'deliverability', 'engagement', 'compliance'. Leave empty for all.",
						},
						"limit": map[string]interface{}{
							"type":        "integer",
							"description": "Number of insights to return (default: 10)",
						},
					},
					"required": []string{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_kanban_tasks",
				Description: "Get current Kanban tasks and their status - tasks the system has created for the team to address ecosystem issues",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"status": map[string]interface{}{
							"type":        "string",
							"description": "Filter by status: 'todo', 'in_progress', 'done'. Leave empty for all.",
						},
						"priority": map[string]interface{}{
							"type":        "string",
							"description": "Filter by priority: 'critical', 'high', 'medium', 'low'. Leave empty for all.",
						},
					},
					"required": []string{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_last_analysis",
				Description: "Get the most recent hourly analysis results including health score, issues found, and recommendations",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
					"required":   []string{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_knowledge_summary",
				Description: "Get a summary of what the AI has learned - total patterns, insights, learning cycles, and data points analyzed",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
					"required":   []string{},
				},
			},
		},
	}
}
