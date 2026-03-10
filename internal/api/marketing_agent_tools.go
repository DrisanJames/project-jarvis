package api

func getAgentTools() []agentToolDef {
	return []agentToolDef{
		// ── Read Tools ──────────────────────────────────────────────────

		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "get_isp_health",
				Description: "Get 3-day ISP sending health: bounce, deferral, complaint rates, risk scores, and quota recommendations per ISP. Optionally filter by sending domain.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"sending_domain": prop("string", "Filter by sending domain (e.g. em.quizfiesta.com). Leave empty for all domains."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "list_campaigns",
				Description: "List recent campaigns with status, sent count, open/click/bounce rates. Returns up to 30 campaigns.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"status_filter": prop("string", "Filter by status: scheduled, sending, completed, cancelled, paused, draft. Leave empty for all."),
						"limit":         prop("integer", "Max campaigns to return (default 20, max 50)."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "get_campaign_details",
				Description: "Get full details of a specific campaign including ISP plans, quotas, variants, lists/segments.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"campaign_id"},
					"properties": map[string]interface{}{
						"campaign_id": prop("string", "Campaign UUID (full or prefix)."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "list_lists",
				Description: "List all mailing lists with subscriber counts and engagement rates.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "list_segments",
				Description: "List all audience segments with subscriber counts, type, and conditions.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "list_templates",
				Description: "List templates in the content library. Optionally filter by folder or search by name.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"search":    prop("string", "Search templates by name (partial match)."),
						"folder_id": prop("string", "Filter by folder UUID."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "read_template",
				Description: "Get full template details INCLUDING complete HTML content (not truncated). Use this when you need to review, modify, or use a template's actual content.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"template_id"},
					"properties": map[string]interface{}{
						"template_id": prop("string", "Template UUID."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "get_sending_domains",
				Description: "List available sending domains with their profiles, from_email, and vendor type.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "get_last_quotas",
				Description: "Get ISP quotas from the most recent completed campaign.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "estimate_audience",
				Description: "Estimate total audience size for given inclusion lists and target ISPs, accounting for suppressions.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"inclusion_lists"},
					"properties": map[string]interface{}{
						"inclusion_lists": map[string]interface{}{"type": "array", "items": map[string]string{"type": "string"}, "description": "List UUIDs or names to include."},
						"target_isps":    map[string]interface{}{"type": "array", "items": map[string]string{"type": "string"}, "description": "ISPs to target (gmail, yahoo, etc.)."},
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "get_engagement_breakdown",
				Description: "Get subscriber counts by engagement tier for specified lists. Tiers: openers_7d, clickers_14d, engagers_30d, recent_subscribers, cold. Useful for warmup audience planning.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"list_ids"},
					"properties": map[string]interface{}{
						"list_ids": map[string]interface{}{"type": "array", "items": map[string]string{"type": "string"}, "description": "List UUIDs to analyze."},
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "get_domain_strategy",
				Description: "Get the current sending strategy (warmup/performance) for a domain, including params like volume increase %, audience priority, etc.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"sending_domain"},
					"properties": map[string]interface{}{
						"sending_domain": prop("string", "The sending domain to look up."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "get_recommendations",
				Description: "Get campaign recommendations for a date range, optionally filtered by status or sending domain. Returns full campaign_config including ISP quotas, lists, template, schedule, wave/throttle settings.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"start_date":     prop("string", "Start date (YYYY-MM-DD). Defaults to today."),
						"end_date":       prop("string", "End date (YYYY-MM-DD). Defaults to 30 days from now."),
						"status":         prop("string", "Filter by status: pending, approved, rejected, executed, failed."),
						"sending_domain": prop("string", "Filter by sending domain."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "get_recommendation_details",
				Description: "Get full details of a single campaign recommendation including campaign_config (ISP quotas, inclusion/exclusion lists, template, subject, preview_text, scheduled_time, wave_interval_minutes, throttle_per_wave, from_name, from_email, audience_priority). Use this to inspect a recommendation before modifying it.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"recommendation_id"},
					"properties": map[string]interface{}{
						"recommendation_id": prop("string", "The recommendation UUID."),
					},
				},
			},
		},

		// ── Write Tools ─────────────────────────────────────────────────

		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "save_domain_strategy",
				Description: "Save or update a sending strategy for a domain. Strategy types: 'warmup' (increasing volume, prioritize engaged audience) or 'performance' (monetization focus, high-engagement only).",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"sending_domain", "strategy"},
					"properties": map[string]interface{}{
						"sending_domain":           prop("string", "The sending domain (e.g. em.quizfiesta.com)."),
						"strategy":                 prop("string", "Strategy type: 'warmup' or 'performance'."),
						"daily_volume_increase_pct": prop("number", "For warmup: daily volume increase percentage (e.g. 10 for 10%)."),
						"max_daily_volume":          prop("integer", "Maximum daily send volume."),
						"audience_priority":         map[string]interface{}{"type": "array", "items": map[string]string{"type": "string"}, "description": "Audience tiers in priority order. Options: openers_7d, clickers_14d, engagers_30d, recent_subscribers, cold."},
						"content_rotation":          prop("boolean", "Whether to rotate templates across sends."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "create_recommendation",
				Description: "Create a campaign recommendation for a specific date. The recommendation will have status 'pending' and must be approved by the user before execution.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"sending_domain", "scheduled_date", "campaign_name", "isp_quotas", "inclusion_lists"},
					"properties": map[string]interface{}{
						"sending_domain": prop("string", "Sending domain for this campaign."),
						"scheduled_date": prop("string", "Target send date (YYYY-MM-DD)."),
						"scheduled_time": prop("string", "Target send time UTC (HH:MM). Defaults to 13:00 (6am MST)."),
						"campaign_name":  prop("string", "Proposed campaign name."),
						"isp_quotas":     map[string]interface{}{"type": "object", "description": "ISP quota map, e.g. {\"gmail\": 60000, \"yahoo\": 30000}."},
						"inclusion_lists": map[string]interface{}{"type": "array", "items": map[string]string{"type": "string"}, "description": "List UUIDs or names to include."},
						"exclusion_lists": map[string]interface{}{"type": "array", "items": map[string]string{"type": "string"}, "description": "List UUIDs or names to exclude."},
						"template_id":    prop("string", "Template UUID to use for content."),
						"subject":        prop("string", "Email subject line."),
						"preview_text":   prop("string", "Email preview/pre-header text."),
						"reasoning":      prop("string", "Your reasoning for this recommendation."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "generate_template",
				Description: "Generate new AI email templates based on campaign type and sending domain brand intelligence. Scrapes the domain for colors/logos, generates 5 HTML variations, and saves each as a draft in the Content Library. Use list_templates and read_template first to review existing templates for inspiration.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"campaign_type", "sending_domain"},
					"properties": map[string]interface{}{
						"campaign_type":         prop("string", "Template type: welcome, newsletter, promotional, winback, re-engagement, announcement, trivia."),
						"sending_domain":        prop("string", "Domain to scrape for brand intelligence (e.g. quizfiesta.com)."),
						"reference_template_id": prop("string", "Optional: existing template UUID to use as style reference."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "update_recommendation",
				Description: "Update fields of a pending campaign recommendation. Use this to modify scheduled_time, wave_interval_minutes, throttle_per_wave, ISP quotas, lists, subject, preview_text, from_name, etc. Only pending recommendations can be updated.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"recommendation_id"},
					"properties": map[string]interface{}{
						"recommendation_id":  prop("string", "The recommendation UUID to update."),
						"campaign_name":      prop("string", "Updated campaign name."),
						"scheduled_date":     prop("string", "Updated date (YYYY-MM-DD)."),
						"scheduled_time":     prop("string", "Updated time UTC (HH:MM)."),
						"subject":            prop("string", "Updated subject line."),
						"preview_text":       prop("string", "Updated preview/pre-header text."),
						"from_name":          prop("string", "Updated from name."),
						"from_email":         prop("string", "Updated from email."),
						"template_id":        prop("string", "Updated template UUID."),
						"wave_interval_minutes": prop("integer", "Minutes between waves (e.g. 15)."),
						"throttle_per_wave":  prop("integer", "Batch size per wave (0 = unlimited)."),
						"isp_quotas":         map[string]interface{}{"type": "object", "description": "Updated ISP quota map, e.g. {\"gmail\": 60000, \"yahoo\": 30000}."},
						"inclusion_lists":    map[string]interface{}{"type": "array", "items": map[string]string{"type": "string"}, "description": "Updated inclusion list UUIDs."},
						"exclusion_lists":    map[string]interface{}{"type": "array", "items": map[string]string{"type": "string"}, "description": "Updated exclusion list UUIDs."},
						"audience_priority":  map[string]interface{}{"type": "array", "items": map[string]string{"type": "string"}, "description": "Updated audience priority order."},
						"reasoning":          prop("string", "Updated reasoning text."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "deploy_approved_campaign",
				Description: "Deploy an approved campaign recommendation through the PMTA wave pipeline. Requires the recommendation to be in 'approved' status. ONLY call this after the user has explicitly approved.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"recommendation_id"},
					"properties": map[string]interface{}{
						"recommendation_id": prop("string", "The recommendation UUID to deploy."),
					},
				},
			},
		},
	}
}
