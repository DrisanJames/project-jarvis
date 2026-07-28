package api

func getCopilotTools() []copilotToolDef {
	return []copilotToolDef{
		// ── Read Tools ──────────────────────────────────────────────────

		{
			Type: "function",
			Function: copilotToolFuncDef{
				Name:        "list_campaigns",
				Description: "List recent campaigns with their status, sent count, open/click/bounce rates, and sending domain. Returns up to 30 campaigns ordered by creation date descending.",
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
			Function: copilotToolFuncDef{
				Name:        "get_campaign_details",
				Description: "Get full details of a specific campaign including its pmta_config (ISP plans, quotas, time spans, variants, inclusion/exclusion lists and segments). Use this to inspect or clone a campaign.",
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
			Function: copilotToolFuncDef{
				Name:        "search_campaigns_by_name",
				Description: "Search campaigns by name (case-insensitive partial match). Returns matching campaigns with key metrics.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"query"},
					"properties": map[string]interface{}{
						"query": prop("string", "Search text to match against campaign names."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: copilotToolFuncDef{
				Name:        "list_lists",
				Description: "List all mailing lists with subscriber counts, mailed-to counts, and engagement percentages (open%, click%, complaint%).",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: "function",
			Function: copilotToolFuncDef{
				Name:        "list_segments",
				Description: "List all audience segments with their subscriber counts, type (dynamic/static), and conditions summary.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: "function",
			Function: copilotToolFuncDef{
				Name:        "list_templates",
				Description: "List templates in the content library. Optionally filter by folder or search by name. Returns template name, subject, from_name, folder path.",
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
			Function: copilotToolFuncDef{
				Name:        "get_template",
				Description: "Get full template details including subject, from_name, preview_text, and HTML content. Use this when the user wants to use a specific template in a campaign.",
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
			Function: copilotToolFuncDef{
				Name:        "get_isp_performance",
				Description: "Get ISP-level performance metrics (sent, delivered, opens, clicks, bounces, complaints) for a date range. If no ISP specified, returns summary for all ISPs.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"isp":        prop("string", "ISP name: gmail, yahoo, microsoft, apple, comcast, att, cox, charter. Leave empty for all."),
						"range_type": prop("string", "Date range: 24h, 7, 14, 30, 90. Default 7."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: copilotToolFuncDef{
				Name:        "get_sending_insights",
				Description: "Get 3-day ISP sending health analysis including bounce rates, deferral rates, hard/soft bounce split, quota recommendations, and risk scores. Essential before setting quotas.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: "function",
			Function: copilotToolFuncDef{
				Name:        "get_last_quotas",
				Description: "Get ISP quotas from the most recent successful campaign. Returns per-ISP volumes, source campaign name, and date.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: "function",
			Function: copilotToolFuncDef{
				Name:        "get_sending_domains",
				Description: "List available sending domains with their profiles, from_email, and status.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: "function",
			Function: copilotToolFuncDef{
				Name:        "get_cpm_budgets",
				Description: "List active CPM deals with budget, delivered volume, spend-to-date, pace vs goal, and the GAP still to fill (volume remaining + budget remaining + conversions gap). Use to answer 'which budgets still need volume' when planning a send. Reuses the CPM planner's own pacing/eCPA math. Delivered comes from a background cache; on a fresh boot it returns plan-level numbers flagged 'cache_warming' — re-run in a few minutes for delivered/pace.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"include_all_statuses": prop("boolean", "Include paused/completed deals too. Default false (active deals only)."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: copilotToolFuncDef{
				Name:        "get_offer_winners",
				Description: "Get the APPROVED creatives/subjects (the approved-copy pool) for an offer. NOTE: this currently returns copy in positional/approval order with ranking_available=false — real human-click winner ranking (Everflow) is NOT yet ingested into prod, so the tool never fabricates a performance ranking. Use it to see what copy is approved for an offer; do not present the order as best-performing.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"offer"},
					"properties": map[string]interface{}{
						"offer": prop("string", "Offer key / slug (e.g. sams-club, liberty-mutual, metal-roofing)."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: copilotToolFuncDef{
				Name:        "estimate_audience",
				Description: "Estimate total audience size for a given set of inclusion lists and ISPs. Returns per-ISP counts.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"inclusion_lists"},
					"properties": map[string]interface{}{
						"inclusion_lists": propArray("string", "Array of list UUIDs to include."),
						"target_isps":    propArray("string", "Array of ISP names to target. Default: all 8."),
					},
				},
			},
		},

		// ── Scheduler Bridge (local runner via mailing_scheduler_commands) ──

		{
			Type: "function",
			Function: copilotToolFuncDef{
				Name:        "queue_scheduler_command",
				Description: "Queue a scheduling command for the operator's LOCAL scheduler runner (the platform cannot run the Python pipeline itself). Allowed commands EXACTLY: build-send-day, fresh-bcast, promote, intake-forecast, write-directive. args is a JSON object passed through verbatim to the runner. Commands take ~1-15 minutes; poll get_scheduler_command with the returned id before summarizing results. NEVER queue promote without explicit operator approval in this chat.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"command"},
					"properties": map[string]interface{}{
						"command": prop("string", "One of: build-send-day, fresh-bcast, promote, intake-forecast, write-directive."),
						"args":    propObject("Command arguments passed through verbatim, e.g. {date, table} for write-directive or {date, base, confirm} for build-send-day."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: copilotToolFuncDef{
				Name:        "get_scheduler_command",
				Description: "Get one queued/running/completed scheduler-bridge command by id (full UUID or prefix): status, output_tail (last lines of runner output), exit_code, timestamps. Poll this after queueing — report the output_tail VERBATIM to the operator.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"id"},
					"properties": map[string]interface{}{
						"id": prop("string", "Command UUID (full or prefix)."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: copilotToolFuncDef{
				Name:        "list_scheduler_commands",
				Description: "List recent scheduler-bridge commands (newest first) with status, output_tail, and timestamps.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"limit": prop("integer", "Max commands to return (default 10, max 50)."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: copilotToolFuncDef{
				Name:        "get_board_state",
				Description: "Read-only board snapshot for a send date: campaigns named '<montok><dd> - %' (e.g. 'jul29 - …') grouped by sending domain, with name, status, total_recipients, and whether offer_id is set (has_offer=false means offer suppression never fires).",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"date"},
					"properties": map[string]interface{}{
						"date": prop("string", "Send date as YYYY-MM-DD (or a board token like jul29)."),
					},
				},
			},
		},

		// ── Write Tools (require confirmation) ──────────────────────────

		{
			Type: "function",
			Function: copilotToolFuncDef{
				Name:        "clone_campaign",
				Description: "Clone an existing campaign with optional overrides. IMPORTANT: Set confirmed=true only after the user explicitly confirms. Always present the plan first with confirmed=false.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"source_campaign_id"},
					"properties": map[string]interface{}{
						"source_campaign_id":    prop("string", "UUID of the campaign to clone."),
						"name_override":         prop("string", "New campaign name. If empty, appends ' (Clone)'."),
						"scheduled_at_utc":      prop("string", "New scheduled time in RFC3339 UTC (e.g. 2026-03-11T13:00:00Z). Leave empty to keep original."),
						"exclusion_segments":    propArray("string", "Segment UUIDs to add as exclusions."),
						"additional_exclusion_lists": propArray("string", "Additional exclusion list identifiers to add."),
						"quota_overrides":       propObject("ISP quota overrides as {isp: volume} map."),
						"confirmed":            prop("boolean", "Must be true for execution. Set false to get a preview."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: copilotToolFuncDef{
				Name:        "deploy_campaign",
				Description: "Deploy a fully constructed campaign. IMPORTANT: Set confirmed=true only after the user explicitly confirms. Always present the full campaign summary first with confirmed=false.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"name", "sending_domain", "variants", "inclusion_lists", "target_isps", "isp_quotas"},
					"properties": map[string]interface{}{
						"name":               prop("string", "Campaign name."),
						"sending_domain":     prop("string", "Sending domain (e.g. em.quizfiesta.com)."),
						"variants":           propArray("object", "Array of {subject, from_name, html_content, preview_text}."),
						"inclusion_lists":    propArray("string", "List UUIDs to send to."),
						"inclusion_segments": propArray("string", "Segment UUIDs to include."),
						"exclusion_lists":    propArray("string", "Exclusion list identifiers."),
						"exclusion_segments": propArray("string", "Exclusion segment UUIDs."),
						"target_isps":        propArray("string", "ISP names to target."),
						"isp_quotas":         propArray("object", "Array of {isp, volume}."),
						"send_mode":          prop("string", "immediate or scheduled."),
						"scheduled_at_utc":   prop("string", "RFC3339 UTC time for scheduled sends."),
						"timezone":           prop("string", "Timezone (e.g. America/Boise). Default: America/Boise."),
						"confirmed":          prop("boolean", "Must be true for execution. Set false to preview."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: copilotToolFuncDef{
				Name:        "save_draft",
				Description: "Save a campaign configuration as a draft so the user can review/edit in the Campaign Manager wizard.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"name", "sending_domain"},
					"properties": map[string]interface{}{
						"name":               prop("string", "Campaign name."),
						"sending_domain":     prop("string", "Sending domain."),
						"variants":           propArray("object", "Array of {subject, from_name, html_content}."),
						"inclusion_lists":    propArray("string", "List UUIDs."),
						"exclusion_lists":    propArray("string", "Exclusion identifiers."),
						"exclusion_segments": propArray("string", "Exclusion segment UUIDs."),
						"target_isps":        propArray("string", "ISP names."),
						"isp_quotas":         propArray("object", "Array of {isp, volume}."),
						"send_mode":          prop("string", "immediate or scheduled."),
						"timezone":           prop("string", "Timezone."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: copilotToolFuncDef{
				Name:        "create_segment",
				Description: "Create a dynamic audience segment. IMPORTANT: Set confirmed=true only after the user confirms. Present the segment definition first with confirmed=false.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"name", "conditions"},
					"properties": map[string]interface{}{
						"name":        prop("string", "Segment name."),
						"description": prop("string", "Segment description."),
						"conditions":  propObject("Segment conditions as a ConditionGroupBuilder JSON: {logic_operator, conditions: [{condition_type, field, operator, value, event_name, event_sending_domain}]}."),
						"confirmed":   prop("boolean", "Must be true for execution."),
					},
				},
			},
		},
		{
			Type: "function",
			Function: copilotToolFuncDef{
				Name:        "emergency_stop",
				Description: "Immediately stop a running campaign. Cancels all pending queue items and pauses PMTA queues. IMPORTANT: Requires explicit user confirmation.",
				Parameters: map[string]interface{}{
					"type":     "object",
					"required": []string{"campaign_id"},
					"properties": map[string]interface{}{
						"campaign_id": prop("string", "Campaign UUID to stop."),
						"confirmed":   prop("boolean", "Must be true to execute."),
					},
				},
			},
		},
	}
}

func prop(typ, desc string) map[string]interface{} {
	return map[string]interface{}{"type": typ, "description": desc}
}

func propArray(itemType, desc string) map[string]interface{} {
	return map[string]interface{}{
		"type":        "array",
		"description": desc,
		"items":       map[string]interface{}{"type": itemType},
	}
}

func propObject(desc string) map[string]interface{} {
	return map[string]interface{}{
		"type":        "object",
		"description": desc,
	}
}
