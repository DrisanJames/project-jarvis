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
				Name:        "list_suppression_lists",
				Description: "List all suppression lists (exclusion lists). Use for exclusion_lists in recommendations. ALWAYS include 'Global Suppression' (id: global-suppression-list) as the first exclusion. Returns id, name, type='suppression_list', entry_count.",
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
				Description: "Create a fully-configured campaign recommendation for a specific date. ALL fields (from_name, from_email, inclusion_lists as objects with id/name/type, exclusion_lists as objects, isp_quotas, wave settings) are persisted in one call. The recommendation is created as 'pending' and must be approved by the user.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"sending_domain", "scheduled_date", "campaign_name"},
					"properties": map[string]interface{}{
						"sending_domain":      prop("string", "Sending domain for this campaign (e.g. em.discountblog.com)."),
						"scheduled_date":      prop("string", "Target send date (YYYY-MM-DD)."),
						"scheduled_time":      prop("string", "Target send time UTC (HH:MM). Defaults to 13:00 (6am MST)."),
						"campaign_name":       prop("string", "Proposed campaign name."),
						"from_name":           prop("string", "Sender display name (e.g. 'Jamie @ Discount Blog')."),
						"from_email":          prop("string", "Sender email address (e.g. 'hello@em.discountblog.com')."),
						"isp_quotas":          map[string]interface{}{"type": "object", "description": "ISP quota map, e.g. {\"gmail\": 100, \"microsoft\": 950}. Keys: gmail, yahoo, microsoft, apple, comcast, att, cox, charter."},
						"inclusion_lists":     map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object"}, "description": "Inclusion lists/segments as objects: [{\"id\":\"uuid\",\"name\":\"...\",\"type\":\"list|segment\"}]. Use 'list' for mailing lists, 'segment' for dynamic segments."},
						"exclusion_lists":     map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object"}, "description": "Exclusion lists/segments as objects: [{\"id\":\"global-suppression-list\",\"name\":\"Global Suppression\",\"type\":\"suppression_list\"}]."},
						"template_id":         prop("string", "Template UUID to use for content."),
						"subject":             prop("string", "Email subject line. Supports Liquid: {{ first_name | default: \"Hey\" }}"),
						"preview_text":        prop("string", "Email preview/pre-header text."),
						"wave_interval_minutes": prop("integer", "Minutes between send waves (default 15)."),
						"throttle_per_wave":   prop("integer", "Max emails per wave (0 = unlimited)."),
						"audience_priority":   map[string]interface{}{"type": "array", "items": map[string]string{"type": "string"}, "description": "Audience tiers in priority order: clickers_14d, openers_7d, engagers_30d, recent_subscribers, cold."},
						"reasoning":           prop("string", "Your reasoning for this recommendation."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "create_template",
				Description: "Create template metadata (name, subject, from_name, preview_text) in the Content Library as a draft. HTML content is NOT accepted — template structure is managed through approved brand templates. Content variations are generated automatically by the wave content pipeline. Do NOT pass html_content.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"name", "subject"},
					"properties": map[string]interface{}{
						"name":         prop("string", "Template name (e.g. 'DB Newsletter — Wednesday Savings')."),
						"subject":      prop("string", "Default subject line for this template."),
						"from_name":    prop("string", "Default from name."),
						"preview_text": prop("string", "Default preview/pre-header text."),
						"folder_name":  prop("string", "Folder to organize the template in (e.g. 'newsletter', 'welcome-series'). Created if it doesn't exist."),
						"brand":        prop("string", "Brand name for organization (e.g. 'DiscountBlog', 'QuizFiesta', 'HistoryThinking', 'MyOwnHealth')."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "generate_template",
				Description: "Generate new AI email templates for review. Templates are saved with 'pending_review' status and require human approval before use. Content variations for active campaigns are generated automatically by the wave content pipeline — use this tool only when proposing new template designs.",
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
				Description: "Update fields of a pending OR approved campaign recommendation. For approved recommendations, content changes (subject, preview_text, from_name, from_email) are also propagated to the linked deployed campaign. Use this to modify subject, preview_text, from_name, ISP quotas, lists, etc.",
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
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "unapprove_recommendation",
				Description: "Revert an approved recommendation back to pending status. This cancels the linked deployed campaign (if it hasn't started sending) and clears the executed_campaign_id, allowing full re-editing and re-approval.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"recommendation_id"},
					"properties": map[string]interface{}{
						"recommendation_id": prop("string", "The recommendation UUID to unapprove."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "delete_recommendation",
				Description: "Delete a single campaign recommendation by ID. Use when the user wants to remove a specific recommendation without clearing the entire calendar.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"recommendation_id"},
					"properties": map[string]interface{}{
						"recommendation_id": prop("string", "The recommendation UUID to delete."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "clear_forecasts",
				Description: "Delete all pending campaign recommendations (forecasts) for the organization. Use when the user wants to clear the calendar and regenerate, or remove all generated forecasts.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"sending_domain": prop("string", "Optional: only clear forecasts for this sending domain. Omit to clear all."),
						"status":         prop("string", "Optional: only clear forecasts with this status (default: pending). Use 'all' to clear regardless of status."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "compute_isp_quotas",
				Description: "Compute a risk-adjusted ISP quota distribution for a target send volume. Analyzes the last 3 days of ISP-level bounce, deferral, and complaint data, then distributes volume proportionally with risk-based adjustments (PAUSE/DECREASE/CAUTION/MAINTAIN/INCREASE). Use this before create_recommendation to get data-driven ISP quotas.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"sending_domain", "target_volume"},
					"properties": map[string]interface{}{
						"sending_domain": prop("string", "The sending domain to compute quotas for (e.g. em.discountblog.com)."),
						"target_volume":  prop("integer", "The total target send volume to distribute across ISPs."),
					},
				},
			},
		},

		// ── Campaign Approval ───────────────────────────────────────────

		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "approve_recommendation",
				Description: "Approve a pending campaign recommendation and deploy it through the full PMTA wave pipeline. This is the final step — it runs preflight checks, generates multi-variant content, plans the audience, creates the campaign with ISP wave scheduling, and marks the recommendation as approved. The campaign will be scheduled for sending at the configured time. ONLY call this after the user has explicitly confirmed they want to approve.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"recommendation_id"},
					"properties": map[string]interface{}{
						"recommendation_id": prop("string", "The recommendation UUID to approve and deploy."),
					},
				},
			},
		},

		// ── Infrastructure Tools ────────────────────────────────────────

		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "get_ip_pool_health",
				Description: "List all IP pools and their IPs with statuses (active, warmup, paused, quarantined). Shows pool-level aggregates and per-IP details including reputation scores, warmup stage, and lifetime send/deliver/bounce counts.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "get_preflight_status",
				Description: "Run infrastructure preflight checks for a sending domain BEFORE approving campaigns. Validates: sending profile exists, IP pool has active IPs, DKIM/SPF DNS records resolve, PMTA is reachable. Returns pass/fail per check with error details.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"sending_domain"},
					"properties": map[string]interface{}{
						"sending_domain": prop("string", "The sending domain to validate (e.g. em.discountblog.com)."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "get_pipeline_health",
				Description: "Get a holistic health check of the entire sending pipeline: wave content cache inventory, PMTA reachability (port 587), active/scheduled campaign counts, IP availability, and pending recommendation count.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "manage_ip_status",
				Description: "Update the status of a sending IP address. Use to recover quarantined IPs (set to 'warmup'), pause problematic IPs, or activate IPs. Only affects IPs owned by the organization.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"ip_id", "status"},
					"properties": map[string]interface{}{
						"ip_id":  prop("string", "UUID of the IP address to update."),
						"status": prop("string", "New status: 'active', 'warmup', or 'paused'."),
					},
				},
			},
		},

		// ── Analytics Tools ─────────────────────────────────────────────

		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "get_isp_sending_insights",
				Description: "Get day-by-day per-ISP sending metrics: sent, delivered, hard/soft bounces, opens, clicks, complaints with delivery and open rates. Useful for trend analysis and identifying ISP-specific issues over time.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"sending_domain": prop("string", "Filter by sending domain (optional)."),
						"days":           prop("integer", "Number of days to look back (default 7, max 30)."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "get_deliverability_report",
				Description: "Get aggregate deliverability metrics grouped by sending domain: total sent, delivered, hard/soft bounces, opens, clicks, complaints with calculated rates. Covers the last N days.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"days": prop("integer", "Number of days to look back (default 7, max 30)."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "get_injection_analytics",
				Description: "Get wave-level injection details for campaigns: wave number, ISP, status, planned vs enqueued recipients, batch size, scheduling timestamps. Use to monitor active sends or audit completed campaigns.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"campaign_id": prop("string", "Campaign UUID to inspect waves for. If omitted, shows recent waves across all campaigns."),
						"limit":       prop("integer", "Max waves to return (default 10, max 50)."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "get_content_learnings",
				Description: "Get historical campaign performance data for learning: subject lines, from names, open/click/bounce rates from completed campaigns. Use to inform future subject line and content decisions.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"sending_domain": prop("string", "Filter by sending domain (optional)."),
						"limit":          prop("integer", "Max campaigns to return (default 20, max 50)."),
					},
				},
			},
		},

		// ── Content & Audience Tools ────────────────────────────────────

		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "get_wave_cache_status",
				Description: "Check the wave content cache inventory grouped by brand and campaign type. Shows total, available (unused), and used entries per brand. Use to verify content is ready before approving campaigns.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "refresh_wave_cache",
				Description: "Trigger AI content generation to replenish the wave content cache for a brand. Scrapes the brand's blog, generates multi-variant editorial content, and stores it for future campaign approvals. Use when get_wave_cache_status shows low available entries.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"brand": prop("string", "Brand key to refresh (e.g. 'discountblog', 'quizfiesta', 'historythinking', 'myownhealth'). Use '-welcome' suffix for welcome variants. Use 'all' for all brands."),
						"waves": prop("integer", "Number of content variations to generate (default 5, max 10)."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "get_subscriber_360",
				Description: "Look up a specific subscriber by email address. Returns all list memberships, engagement history (opens, clicks, scores), ISP, and recent tracking events. Use for debugging delivery issues or understanding subscriber behavior.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"email"},
					"properties": map[string]interface{}{
						"email": prop("string", "Subscriber email address to look up."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: agentToolFuncDef{
				Name:        "get_segment_preview",
				Description: "Preview a segment: see its conditions, subscriber count, and a random sample of 10 matching subscribers with their ISP, open counts, and last activity. Use to verify a segment targets the right audience before including it in a campaign.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"segment_id"},
					"properties": map[string]interface{}{
						"segment_id": prop("string", "UUID of the segment to preview."),
					},
				},
			},
		},
	}
}
