package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

func (a *EmailMarketingAgent) executeAgentTool(ctx context.Context, orgID, name, arguments string) (string, string) {
	var args map[string]interface{}
	json.Unmarshal([]byte(arguments), &args)

	log.Printf("[MarketingAgent] tool=%s args=%s", name, arguments)

	var result interface{}
	var action string

	switch name {
	case "get_isp_health":
		result = a.toolGetISPHealth(ctx, orgID, args)
	case "list_campaigns":
		result = a.toolListCampaigns(ctx, orgID, args)
	case "get_campaign_details":
		result = a.toolGetCampaignDetails(ctx, orgID, args)
	case "list_lists":
		result = a.toolListLists(ctx, orgID)
	case "list_segments":
		result = a.toolListSegments(ctx, orgID)
	case "list_suppression_lists":
		result = a.toolListSuppressionLists(ctx, orgID)
	case "list_templates":
		result = a.toolListTemplates(ctx, orgID, args)
	case "read_template":
		result = a.toolReadTemplate(ctx, orgID, args)
	case "get_sending_domains":
		result = a.toolGetSendingDomains(ctx, orgID)
	case "get_last_quotas":
		result = a.toolGetLastQuotas(ctx, orgID)
	case "estimate_audience":
		result = a.toolEstimateAudience(ctx, orgID, args)
	case "get_engagement_breakdown":
		result = a.toolGetEngagementBreakdown(ctx, orgID, args)
	case "get_domain_strategy":
		result = a.toolGetDomainStrategy(ctx, orgID, args)
	case "get_recommendations":
		result = a.toolGetRecommendations(ctx, orgID, args)
	case "get_recommendation_details":
		result = a.toolGetRecommendationDetails(ctx, orgID, args)
	case "update_recommendation":
		result, action = a.toolUpdateRecommendation(ctx, orgID, args)
	case "save_domain_strategy":
		result, action = a.toolSaveDomainStrategy(ctx, orgID, args)
	case "create_recommendation":
		result, action = a.toolCreateRecommendation(ctx, orgID, args)
	case "create_template":
		result, action = a.toolCreateTemplate(ctx, orgID, args)
	case "generate_template":
		result, action = a.toolGenerateTemplate(ctx, orgID, args)
	case "deploy_approved_campaign":
		result, action = a.toolDeployApprovedCampaign(ctx, orgID, args)
	case "unapprove_recommendation":
		result, action = a.toolUnapproveRecommendation(ctx, orgID, args)
	case "delete_recommendation":
		result, action = a.toolDeleteRecommendation(ctx, orgID, args)
	case "clear_forecasts":
		result, action = a.toolClearForecasts(ctx, orgID, args)
	case "compute_isp_quotas":
		result = a.toolComputeISPQuotas(ctx, orgID, args)
	case "get_ip_pool_health":
		result = a.toolGetIPPoolHealth(ctx, orgID)
	case "get_preflight_status":
		result = a.toolGetPreflightStatus(ctx, orgID, args)
	case "get_pipeline_health":
		result = a.toolGetPipelineHealth(ctx, orgID)
	case "manage_ip_status":
		result, action = a.toolManageIPStatus(ctx, orgID, args)
	case "get_isp_sending_insights":
		result = a.toolGetISPSendingInsights(ctx, orgID, args)
	case "get_deliverability_report":
		result = a.toolGetDeliverabilityReport(ctx, orgID, args)
	case "get_injection_analytics":
		result = a.toolGetInjectionAnalytics(ctx, orgID, args)
	case "get_content_learnings":
		result = a.toolGetContentLearnings(ctx, orgID, args)
	case "get_wave_cache_status":
		result = a.toolGetWaveCacheStatus(ctx, orgID)
	case "refresh_wave_cache":
		result, action = a.toolRefreshWaveCache(ctx, orgID, args)
	case "get_subscriber_360":
		result = a.toolGetSubscriber360(ctx, orgID, args)
	case "get_segment_preview":
		result = a.toolGetSegmentPreview(ctx, orgID, args)
	case "approve_recommendation":
		result, action = a.toolApproveRecommendation(ctx, orgID, args)
	default:
		result = map[string]string{"error": "unknown tool: " + name}
	}

	out, _ := json.Marshal(result)
	return string(out), action
}

// ── Read Tools ──────────────────────────────────────────────────────────────

func (a *EmailMarketingAgent) toolGetISPHealth(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	domain, _ := args["sending_domain"].(string)

	end := time.Now()
	start := end.Add(-3 * 24 * time.Hour)

	domSubquery := `SELECT t.*, LOWER(COALESCE(NULLIF(t.recipient_domain,''), SPLIT_PART(s.email,'@',2))) as dom
		FROM mailing_tracking_events t
		LEFT JOIN mailing_subscribers s ON t.subscriber_id = s.id
		WHERE t.event_at >= $1 AND t.event_at <= $2`
	subArgs := []interface{}{start, end}
	if domain != "" {
		domSubquery += fmt.Sprintf(` AND LOWER(COALESCE(NULLIF(t.sending_domain,''),'unknown')) = LOWER($%d)`, len(subArgs)+1)
		subArgs = append(subArgs, domain)
	}

	dailyQ := fmt.Sprintf(`SELECT %s as isp,
		SUM(CASE WHEN d.event_type = 'sent' THEN 1 ELSE 0 END) as sent,
		SUM(CASE WHEN d.event_type = 'delivered' THEN 1 ELSE 0 END) as delivered,
		SUM(CASE WHEN d.event_type IN ('hard_bounce','bounced') THEN 1 ELSE 0 END) as hard_bounces,
		SUM(CASE WHEN d.event_type = 'soft_bounce' THEN 1 ELSE 0 END) as soft_bounces,
		SUM(CASE WHEN d.event_type IN ('deferred','deferral') THEN 1 ELSE 0 END) as deferred,
		SUM(CASE WHEN d.event_type = 'complained' THEN 1 ELSE 0 END) as complaints,
		SUM(CASE WHEN d.event_type = 'opened' THEN 1 ELSE 0 END) as opens
	FROM (%s) d
	GROUP BY isp`, ispDomainCaseSQL, domSubquery)

	rows, err := a.db.QueryContext(ctx, dailyQ, subArgs...)
	if err != nil {
		return map[string]string{"error": "query failed: " + err.Error()}
	}
	defer rows.Close()

	currentQuotas := map[string]int{}
	quotaRows, _ := a.db.QueryContext(ctx, `
		SELECT p.isp, p.quota FROM mailing_campaign_isp_plans p
		JOIN mailing_campaigns c ON p.campaign_id = c.id
		WHERE c.organization_id::text = $1
		  AND c.status IN ('completed','sent','cancelled','completed_with_errors','sending')
		ORDER BY COALESCE(c.completed_at, c.started_at, c.created_at) DESC
		LIMIT 100`, orgID)
	if quotaRows != nil {
		defer quotaRows.Close()
		for quotaRows.Next() {
			var isp string
			var quota int
			quotaRows.Scan(&isp, &quota)
			if _, seen := currentQuotas[isp]; !seen {
				currentQuotas[isp] = quota
			}
		}
	}

	var isps []map[string]interface{}
	for rows.Next() {
		var isp string
		var sent, delivered, hardBounces, softBounces, deferred, complaints, opens int
		rows.Scan(&isp, &sent, &delivered, &hardBounces, &softBounces, &deferred, &complaints, &opens)
		if isp == "other" || sent == 0 {
			continue
		}

		hardBounceRate := math.Round(float64(hardBounces)/float64(sent)*10000) / 100
		softBounceRate := math.Round(float64(softBounces)/float64(sent)*10000) / 100
		deferralRate := math.Round(float64(deferred)/float64(sent)*10000) / 100
		complaintRate := math.Round(float64(complaints)/float64(sent)*10000) / 100
		openRate := math.Round(float64(opens)/float64(sent)*10000) / 100

		deliveryRate := 0.0
		if delivered > 0 {
			deliveryRate = math.Round(float64(delivered)/float64(sent)*10000) / 100
		}
		normHardBounce := math.Min(hardBounceRate/3.0, 1.0)
		normSoftBounce := math.Min(softBounceRate/20.0, 1.0)
		normDeliveryFail := 1.0 - math.Min(deliveryRate/100, 1)
		normDeferral := math.Min(deferralRate/15.0, 1.0)
		normComplaint := math.Min(complaintRate/0.1, 1.0)
		rawRisk := normHardBounce*25 + normSoftBounce*10 + normDeliveryFail*20 + normDeferral*15 + normComplaint*30
		if deliveryRate > 50 && openRate > 10 {
			engagementDiscount := math.Min((openRate-10)/40.0, 0.15)
			rawRisk = rawRisk * (1.0 - engagementDiscount)
		}
		riskScore := int(math.Round(math.Min(100, rawRisk)))

		recommendation := "MAINTAIN"
		currentQ := currentQuotas[isp]
		suggestedQ := currentQ
		switch {
		case riskScore > 80:
			recommendation = "PAUSE"
			suggestedQ = 0
		case riskScore > 60:
			recommendation = "DECREASE"
			suggestedQ = int(float64(currentQ) * 0.65)
		case riskScore > 40:
			recommendation = "CAUTION"
			suggestedQ = int(float64(currentQ) * 0.85)
		case riskScore > 20:
			recommendation = "MAINTAIN"
		default:
			recommendation = "INCREASE"
			suggestedQ = int(float64(currentQ) * 1.25)
		}

		isps = append(isps, map[string]interface{}{
			"isp": isp, "label": ispLabels[isp],
			"sent": sent, "delivered": delivered, "hard_bounces": hardBounces, "soft_bounces": softBounces, "deferred": deferred,
			"complaints": complaints, "opens": opens,
			"hard_bounce_rate": hardBounceRate, "soft_bounce_rate": softBounceRate, "deferral_rate": deferralRate,
			"complaint_rate": complaintRate, "open_rate": openRate, "delivery_rate": deliveryRate,
			"risk_score": riskScore, "recommendation": recommendation,
			"current_quota": currentQ, "suggested_quota": suggestedQ,
		})
	}
	if isps == nil {
		isps = []map[string]interface{}{}
	}

	// Past EDITH campaign outcomes for learning context
	var pastOutcomes []map[string]interface{}
	pastRows, pastErr := a.db.QueryContext(ctx, `
		SELECT r.sending_domain, r.scheduled_date,
		       r.execution_metrics->>'sent' as sent,
		       r.execution_metrics->>'bounced' as bounced,
		       r.execution_metrics->>'opens' as opens,
		       r.execution_metrics->>'clicks' as clicks,
		       r.status
		FROM agent_campaign_recommendations r
		WHERE r.organization_id::text = $1
		  AND r.execution_metrics IS NOT NULL
		ORDER BY r.scheduled_date DESC
		LIMIT 10
	`, orgID)
	if pastErr == nil {
		defer pastRows.Close()
		for pastRows.Next() {
			var dom, status string
			var schedDate time.Time
			var sent, bounced, opens, clicks sql.NullString
			if pastRows.Scan(&dom, &schedDate, &sent, &bounced, &opens, &clicks, &status) == nil {
				pastOutcomes = append(pastOutcomes, map[string]interface{}{
					"domain":   dom,
					"date":     schedDate.Format("2006-01-02"),
					"sent":     sent.String,
					"bounced":  bounced.String,
					"opens":    opens.String,
					"clicks":   clicks.String,
					"status":   status,
				})
			}
		}
	}

	return map[string]interface{}{
		"isps":            isps,
		"window_days":     3,
		"domain_filter":   domain,
		"past_campaigns":  pastOutcomes,
	}
}

func (a *EmailMarketingAgent) toolListCampaigns(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	statusFilter, _ := args["status_filter"].(string)
	limit := 20
	if l, ok := args["limit"].(float64); ok && int(l) > 0 {
		limit = min(int(l), 50)
	}
	q := `SELECT id, name, status, COALESCE(from_email,''),
	             sent_count, COALESCE(open_count,0), COALESCE(click_count,0),
	             COALESCE(bounce_count,0),
	             COALESCE(hard_bounce_count,0),
	             COALESCE(soft_bounce_count,0),
	             COALESCE(complaint_count,0),
	             scheduled_at, created_at
	      FROM mailing_campaigns WHERE organization_id = $1`
	qArgs := []interface{}{orgID}
	if statusFilter != "" {
		q += ` AND status = $2`
		qArgs = append(qArgs, statusFilter)
	}
	q += ` ORDER BY created_at DESC LIMIT ` + fmt.Sprintf("%d", limit)

	rows, err := a.db.QueryContext(ctx, q, qArgs...)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()
	var campaigns []map[string]interface{}
	for rows.Next() {
		var id, name, status, fromEmail string
		var sent, opens, clicks, bounces, hardBounces, softBounces, complaints int
		var scheduledAt, createdAt sql.NullTime
		rows.Scan(&id, &name, &status, &fromEmail, &sent, &opens, &clicks, &bounces, &hardBounces, &softBounces, &complaints, &scheduledAt, &createdAt)
		c := map[string]interface{}{
			"id": id, "name": name, "status": status, "from_email": fromEmail,
			"sent_count": sent, "open_count": opens, "click_count": clicks,
			"bounce_count": bounces, "hard_bounce_count": hardBounces, "soft_bounce_count": softBounces,
			"complaint_count": complaints,
			"created_at": createdAt.Time.Format(time.RFC3339),
		}
		if sent > 0 {
			c["open_rate"] = fmt.Sprintf("%.1f%%", float64(opens)/float64(sent)*100)
			c["click_rate"] = fmt.Sprintf("%.1f%%", float64(clicks)/float64(sent)*100)
		}
		if scheduledAt.Valid {
			c["scheduled_at"] = scheduledAt.Time.Format(time.RFC3339)
		}
		campaigns = append(campaigns, c)
	}
	if campaigns == nil {
		campaigns = []map[string]interface{}{}
	}
	return map[string]interface{}{"campaigns": campaigns, "count": len(campaigns)}
}

func (a *EmailMarketingAgent) toolGetCampaignDetails(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	cID, _ := args["campaign_id"].(string)
	if cID == "" {
		return map[string]string{"error": "campaign_id required"}
	}
	var id, name, status, fromEmail string
	var sent, opens, clicks, bounces, hardBounces, softBounces, complaints int
	var pmtaConfig sql.NullString
	var scheduledAt, createdAt sql.NullTime
	err := a.db.QueryRowContext(ctx,
		`SELECT id, name, status, COALESCE(from_email,''), sent_count,
		        COALESCE(open_count,0), COALESCE(click_count,0),
		        COALESCE(bounce_count,0),
		        COALESCE(hard_bounce_count,0),
		        COALESCE(soft_bounce_count,0),
		        COALESCE(complaint_count,0),
		        pmta_config::text, scheduled_at, created_at
		 FROM mailing_campaigns WHERE id::text LIKE $1 AND organization_id = $2`,
		cID+"%", orgID).Scan(&id, &name, &status, &fromEmail, &sent, &opens, &clicks, &bounces, &hardBounces, &softBounces, &complaints, &pmtaConfig, &scheduledAt, &createdAt)
	if err != nil {
		return map[string]string{"error": "campaign not found"}
	}
	result := map[string]interface{}{
		"id": id, "name": name, "status": status, "from_email": fromEmail,
		"sent_count": sent, "open_count": opens, "click_count": clicks,
		"bounce_count": bounces, "hard_bounce_count": hardBounces, "soft_bounce_count": softBounces,
		"complaint_count": complaints,
		"created_at": createdAt.Time.Format(time.RFC3339),
	}
	if scheduledAt.Valid {
		result["scheduled_at"] = scheduledAt.Time.Format(time.RFC3339)
	}
	if pmtaConfig.Valid {
		var cfg map[string]interface{}
		if json.Unmarshal([]byte(pmtaConfig.String), &cfg) == nil {
			if ci, ok := cfg["campaign_input"].(map[string]interface{}); ok {
				result["campaign_input"] = ci
			}
		}
	}
	return result
}

func (a *EmailMarketingAgent) toolListLists(ctx context.Context, orgID string) interface{} {
	rows, err := a.db.QueryContext(ctx,
		`SELECT id, name, COALESCE(description,''), subscriber_count, COALESCE(active_count,0), status, created_at
		 FROM mailing_lists WHERE organization_id = $1 ORDER BY name`, orgID)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()
	var lists []map[string]interface{}
	for rows.Next() {
		var id, name, desc, status string
		var subCount, activeCount int
		var createdAt time.Time
		rows.Scan(&id, &name, &desc, &subCount, &activeCount, &status, &createdAt)
		lists = append(lists, map[string]interface{}{
			"id": id, "name": name, "description": desc,
			"subscriber_count": subCount, "active_count": activeCount,
			"status": status,
		})
	}
	if lists == nil {
		lists = []map[string]interface{}{}
	}
	return map[string]interface{}{"lists": lists, "count": len(lists)}
}

func (a *EmailMarketingAgent) toolListSegments(ctx context.Context, orgID string) interface{} {
	rows, err := a.db.QueryContext(ctx,
		`SELECT id, name, COALESCE(segment_type,'dynamic'), subscriber_count, status
		 FROM mailing_segments WHERE organization_id = $1 ORDER BY name`, orgID)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()
	var segments []map[string]interface{}
	for rows.Next() {
		var id, name, segType, status string
		var subCount int
		rows.Scan(&id, &name, &segType, &subCount, &status)
		segments = append(segments, map[string]interface{}{
			"id": id, "name": name, "type": segType,
			"subscriber_count": subCount, "status": status,
		})
	}
	if segments == nil {
		segments = []map[string]interface{}{}
	}
	return map[string]interface{}{"segments": segments, "count": len(segments)}
}

func (a *EmailMarketingAgent) toolListSuppressionLists(ctx context.Context, orgID string) interface{} {
	rows, err := a.db.QueryContext(ctx, `
		SELECT id, name, COALESCE(entry_count, 0) as entry_count
		FROM mailing_suppression_lists
		ORDER BY CASE WHEN id = 'global-suppression-list' THEN 0 ELSE 1 END, name
		LIMIT 50
	`)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()
	var lists []map[string]interface{}
	for rows.Next() {
		var id, name string
		var entryCount int
		if rows.Scan(&id, &name, &entryCount) != nil {
			continue
		}
		lists = append(lists, map[string]interface{}{
			"id":           id,
			"name":         name,
			"type":         "suppression_list",
			"entry_count":  entryCount,
		})
	}
	if lists == nil {
		lists = []map[string]interface{}{}
	}
	return map[string]interface{}{"suppression_lists": lists, "count": len(lists)}
}

func (a *EmailMarketingAgent) toolDeleteRecommendation(ctx context.Context, orgID string, args map[string]interface{}) (interface{}, string) {
	recID, _ := args["recommendation_id"].(string)
	if recID == "" {
		return map[string]string{"error": "recommendation_id is required"}, ""
	}
	result, err := a.db.ExecContext(ctx,
		`DELETE FROM agent_campaign_recommendations WHERE id = $1 AND organization_id = $2`, recID, orgID)
	if err != nil {
		return map[string]string{"error": err.Error()}, ""
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return map[string]string{"error": "recommendation not found or already deleted"}, ""
	}
	return map[string]interface{}{
		"status":  "deleted",
		"id":      recID,
		"message": "Recommendation deleted",
	}, "delete_recommendation"
}

func (a *EmailMarketingAgent) toolClearForecasts(ctx context.Context, orgID string, args map[string]interface{}) (interface{}, string) {
	domain, _ := args["sending_domain"].(string)
	statusFilter, _ := args["status"].(string)
	if statusFilter == "" {
		statusFilter = "pending"
	}

	var result sql.Result
	var err error
	if domain != "" && statusFilter != "all" {
		result, err = a.db.ExecContext(ctx,
			`DELETE FROM agent_campaign_recommendations WHERE organization_id = $1 AND sending_domain = $2 AND status = $3`,
			orgID, domain, statusFilter)
	} else if domain != "" && statusFilter == "all" {
		result, err = a.db.ExecContext(ctx,
			`DELETE FROM agent_campaign_recommendations WHERE organization_id = $1 AND sending_domain = $2`,
			orgID, domain)
	} else if statusFilter != "all" {
		result, err = a.db.ExecContext(ctx,
			`DELETE FROM agent_campaign_recommendations WHERE organization_id = $1 AND status = $2`,
			orgID, statusFilter)
	} else {
		result, err = a.db.ExecContext(ctx,
			`DELETE FROM agent_campaign_recommendations WHERE organization_id = $1`, orgID)
	}
	if err != nil {
		return map[string]string{"error": err.Error()}, ""
	}
	n, _ := result.RowsAffected()
	return map[string]interface{}{
		"status":   "cleared",
		"deleted":  n,
		"message":  fmt.Sprintf("Cleared %d forecast recommendations", n),
	}, "clear_forecasts"
}

func (a *EmailMarketingAgent) toolListTemplates(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	search, _ := args["search"].(string)
	folderID, _ := args["folder_id"].(string)

	q := `SELECT t.id, t.name, COALESCE(t.subject,''), COALESCE(t.from_name,''), COALESCE(t.preview_text,''),
	             COALESCE(f.name,''), t.status
	      FROM mailing_templates t
	      LEFT JOIN mailing_template_folders f ON t.folder_id = f.id
	      WHERE t.organization_id = $1`
	qArgs := []interface{}{orgID}
	idx := 2
	if search != "" {
		q += fmt.Sprintf(` AND LOWER(t.name) LIKE LOWER($%d)`, idx)
		qArgs = append(qArgs, "%"+search+"%")
		idx++
	}
	if folderID != "" {
		q += fmt.Sprintf(` AND t.folder_id = $%d`, idx)
		qArgs = append(qArgs, folderID)
	}
	q += ` ORDER BY t.updated_at DESC LIMIT 50`

	rows, err := a.db.QueryContext(ctx, q, qArgs...)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()
	var templates []map[string]interface{}
	for rows.Next() {
		var id, name, subject, fromName, preview, folder, status string
		rows.Scan(&id, &name, &subject, &fromName, &preview, &folder, &status)
		templates = append(templates, map[string]interface{}{
			"id": id, "name": name, "subject": subject,
			"from_name": fromName, "preview_text": preview,
			"folder": folder, "status": status,
		})
	}
	if templates == nil {
		templates = []map[string]interface{}{}
	}
	return map[string]interface{}{"templates": templates, "count": len(templates)}
}

func (a *EmailMarketingAgent) toolReadTemplate(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	tID, _ := args["template_id"].(string)
	if tID == "" {
		return map[string]string{"error": "template_id required"}
	}
	var id, name, subject, fromName, fromEmail, preview, html, status string
	err := a.db.QueryRowContext(ctx,
		`SELECT id, name, COALESCE(subject,''), COALESCE(from_name,''), COALESCE(from_email,''),
		        COALESCE(preview_text,''), COALESCE(html_content,''), COALESCE(status,'draft')
		 FROM mailing_templates WHERE id = $1 AND organization_id = $2`, tID, orgID,
	).Scan(&id, &name, &subject, &fromName, &fromEmail, &preview, &html, &status)
	if err != nil {
		return map[string]string{"error": "template not found"}
	}
	return map[string]interface{}{
		"id": id, "name": name, "subject": subject,
		"from_name": fromName, "from_email": fromEmail,
		"preview_text": preview, "html_content": html,
		"html_content_length": len(html), "status": status,
	}
}

func (a *EmailMarketingAgent) toolGetSendingDomains(ctx context.Context, orgID string) interface{} {
	rows, err := a.db.QueryContext(ctx,
		`SELECT id, COALESCE(name,''), COALESCE(sending_domain,''), COALESCE(from_email,''),
		        COALESCE(from_name,''), COALESCE(vendor_type,'')
		 FROM mailing_sending_profiles WHERE organization_id = $1 AND status = 'active' ORDER BY name`, orgID)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()
	var domains []map[string]interface{}
	for rows.Next() {
		var id, name, domain, fromEmail, fromName, vendor string
		rows.Scan(&id, &name, &domain, &fromEmail, &fromName, &vendor)
		domains = append(domains, map[string]interface{}{
			"id": id, "name": name, "sending_domain": domain,
			"from_email": fromEmail, "from_name": fromName, "vendor_type": vendor,
		})
	}
	if domains == nil {
		domains = []map[string]interface{}{}
	}
	return map[string]interface{}{"domains": domains}
}

func (a *EmailMarketingAgent) toolGetLastQuotas(ctx context.Context, orgID string) interface{} {
	rows, err := a.db.QueryContext(ctx, `
		SELECT p.isp, p.quota FROM mailing_campaign_isp_plans p
		JOIN mailing_campaigns c ON p.campaign_id = c.id
		WHERE c.organization_id::text = $1
		  AND c.status IN ('completed','sent','cancelled','completed_with_errors','sending')
		ORDER BY COALESCE(c.completed_at, c.started_at, c.created_at) DESC
		LIMIT 100`, orgID)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()
	quotas := map[string]int{}
	for rows.Next() {
		var isp string
		var quota int
		rows.Scan(&isp, &quota)
		if _, seen := quotas[isp]; !seen {
			quotas[isp] = quota
		}
	}
	return map[string]interface{}{"quotas": quotas}
}

func (a *EmailMarketingAgent) toolEstimateAudience(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	listsRaw, _ := args["inclusion_lists"].([]interface{})
	if len(listsRaw) == 0 {
		return map[string]string{"error": "inclusion_lists required"}
	}
	placeholders := make([]string, len(listsRaw))
	qArgs := []interface{}{orgID}
	for i, l := range listsRaw {
		qArgs = append(qArgs, fmt.Sprintf("%v", l))
		placeholders[i] = fmt.Sprintf("$%d", i+2)
	}
	q := fmt.Sprintf(`SELECT COUNT(*) FROM mailing_subscribers
		WHERE organization_id = $1 AND status = 'confirmed'
		AND (list_id::text IN (%s) OR list_id IN (SELECT id FROM mailing_lists WHERE organization_id = $1 AND name IN (%s)))`,
		strings.Join(placeholders, ","), strings.Join(placeholders, ","))
	var total int
	a.db.QueryRowContext(ctx, q, qArgs...).Scan(&total)
	return map[string]interface{}{"estimated_audience": total}
}

func (a *EmailMarketingAgent) toolGetEngagementBreakdown(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	listsRaw, _ := args["list_ids"].([]interface{})
	if len(listsRaw) == 0 {
		return map[string]string{"error": "list_ids required"}
	}
	placeholders := make([]string, len(listsRaw))
	qArgs := []interface{}{orgID}
	for i, l := range listsRaw {
		qArgs = append(qArgs, fmt.Sprintf("%v", l))
		placeholders[i] = fmt.Sprintf("$%d", i+2)
	}
	listFilter := strings.Join(placeholders, ",")
	baseWhere := fmt.Sprintf(`organization_id = $1 AND status = 'confirmed' AND list_id::text IN (%s)`, listFilter)

	type tier struct {
		name  string
		where string
	}
	tiers := []tier{
		{"openers_7d", baseWhere + ` AND last_open_at >= NOW() - INTERVAL '7 days'`},
		{"clickers_14d", baseWhere + ` AND last_click_at >= NOW() - INTERVAL '14 days'`},
		{"engagers_30d", baseWhere + ` AND (last_open_at >= NOW() - INTERVAL '30 days' OR last_click_at >= NOW() - INTERVAL '30 days')`},
		{"ghost_visitors", baseWhere + ` AND EXISTS (
			SELECT 1 FROM mailing_subscriber_tags t
			WHERE t.subscriber_id = mailing_subscribers.id AND t.tag = 'ghost_visitor'
		)`},
		{"recent_subscribers", baseWhere + ` AND subscribed_at >= NOW() - INTERVAL '30 days' AND COALESCE(total_opens,0) = 0`},
		{"total_confirmed", baseWhere},
	}

	result := map[string]interface{}{}
	for _, t := range tiers {
		var cnt int
		a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailing_subscribers WHERE `+t.where, qArgs...).Scan(&cnt)
		result[t.name] = cnt
	}
	return map[string]interface{}{"engagement_breakdown": result, "list_ids": listsRaw}
}

func (a *EmailMarketingAgent) toolGetDomainStrategy(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	domain, _ := args["sending_domain"].(string)
	if domain == "" {
		return map[string]string{"error": "sending_domain required"}
	}
	var id, strategy string
	var params sql.NullString
	var createdAt, updatedAt time.Time
	err := a.db.QueryRowContext(ctx,
		`SELECT id, strategy, params::text, created_at, updated_at
		 FROM agent_domain_strategies WHERE organization_id = $1 AND sending_domain = $2`,
		orgID, domain).Scan(&id, &strategy, &params, &createdAt, &updatedAt)
	if err != nil {
		return map[string]interface{}{"found": false, "sending_domain": domain}
	}
	result := map[string]interface{}{
		"found": true, "id": id, "sending_domain": domain,
		"strategy": strategy, "updated_at": updatedAt.Format(time.RFC3339),
	}
	if params.Valid {
		var p map[string]interface{}
		json.Unmarshal([]byte(params.String), &p)
		result["params"] = p
	}
	return result
}

func (a *EmailMarketingAgent) toolGetRecommendations(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	startDate, _ := args["start_date"].(string)
	endDate, _ := args["end_date"].(string)
	statusFilter, _ := args["status"].(string)
	domainFilter, _ := args["sending_domain"].(string)

	if startDate == "" {
		startDate = time.Now().Format("2006-01-02")
	}
	if endDate == "" {
		endDate = time.Now().Add(30 * 24 * time.Hour).Format("2006-01-02")
	}

	q := `SELECT id::text, sending_domain, scheduled_date,
	             COALESCE(TO_CHAR(scheduled_time, 'HH24:MI'), ''),
	             COALESCE(campaign_name,''), COALESCE(reasoning,''),
	             COALESCE(strategy,''), projected_volume, status,
	             COALESCE(campaign_config::text, '{}'), created_at
	      FROM agent_campaign_recommendations
	      WHERE organization_id = $1 AND scheduled_date >= $2 AND scheduled_date <= $3`
	qArgs := []interface{}{orgID, startDate, endDate}
	idx := 4
	if statusFilter != "" {
		q += fmt.Sprintf(` AND status = $%d`, idx)
		qArgs = append(qArgs, statusFilter)
		idx++
	}
	if domainFilter != "" {
		q += fmt.Sprintf(` AND sending_domain = $%d`, idx)
		qArgs = append(qArgs, domainFilter)
	}
	q += ` ORDER BY scheduled_date, scheduled_time`

	rows, err := a.db.QueryContext(ctx, q, qArgs...)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()
	var recs []map[string]interface{}
	for rows.Next() {
		var id, domain, name, reasoning, strategy, status, configJSON string
		var scheduledDate time.Time
		var scheduledTime string
		var volume int
		var createdAt time.Time
		rows.Scan(&id, &domain, &scheduledDate, &scheduledTime, &name, &reasoning, &strategy, &volume, &status, &configJSON, &createdAt)
		rec := map[string]interface{}{
			"id": id, "sending_domain": domain,
			"scheduled_date": scheduledDate.Format("2006-01-02"),
			"scheduled_time": scheduledTime,
			"campaign_name":  name, "reasoning": reasoning,
			"strategy": strategy, "projected_volume": volume,
			"status": status,
		}
		if configJSON != "" && configJSON != "{}" {
			var cfg map[string]interface{}
			if json.Unmarshal([]byte(configJSON), &cfg) == nil {
				rec["campaign_config"] = cfg
			}
		}
		recs = append(recs, rec)
	}
	if recs == nil {
		recs = []map[string]interface{}{}
	}
	return map[string]interface{}{"recommendations": recs, "count": len(recs)}
}

func (a *EmailMarketingAgent) toolGetRecommendationDetails(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	recID, _ := args["recommendation_id"].(string)
	if recID == "" {
		return map[string]string{"error": "recommendation_id required"}
	}
	var domain, name, reasoning, strategy, status, configJSON string
	var volume int
	var scheduledDate time.Time
	var scheduledTime string
	var approvedAt, executedCampaign, executionError sql.NullString
	var createdAt time.Time

	err := a.db.QueryRowContext(ctx,
		`SELECT sending_domain, scheduled_date, COALESCE(TO_CHAR(scheduled_time, 'HH24:MI'), ''),
		        COALESCE(campaign_name,''), COALESCE(reasoning,''), COALESCE(strategy,''),
		        projected_volume, status, COALESCE(campaign_config::text, '{}'),
		        approved_at::text, executed_campaign_id::text,
		        COALESCE(execution_error,''), created_at
		 FROM agent_campaign_recommendations WHERE id = $1 AND organization_id = $2`,
		recID, orgID).Scan(&domain, &scheduledDate, &scheduledTime, &name, &reasoning, &strategy,
		&volume, &status, &configJSON, &approvedAt, &executedCampaign, &executionError, &createdAt)
	if err != nil {
		return map[string]string{"error": "recommendation not found: " + recID}
	}

	result := map[string]interface{}{
		"id": recID, "sending_domain": domain,
		"scheduled_date":   scheduledDate.Format("2006-01-02"),
		"scheduled_time":   scheduledTime,
		"campaign_name":    name,
		"reasoning":        reasoning,
		"strategy":         strategy,
		"projected_volume": volume,
		"status":           status,
		"created_at":       createdAt.Format(time.RFC3339),
	}
	if configJSON != "" && configJSON != "{}" {
		var cfg map[string]interface{}
		if json.Unmarshal([]byte(configJSON), &cfg) == nil {
			result["campaign_config"] = cfg
		}
	}
	if approvedAt.Valid {
		result["approved_at"] = approvedAt.String
	}
	if executedCampaign.Valid {
		result["executed_campaign_id"] = executedCampaign.String
	}
	if executionError.Valid && executionError.String != "" {
		result["execution_error"] = executionError.String
	}
	return result
}

func (a *EmailMarketingAgent) toolUpdateRecommendation(ctx context.Context, orgID string, args map[string]interface{}) (interface{}, string) {
	recID, _ := args["recommendation_id"].(string)
	if recID == "" {
		return map[string]string{"error": "recommendation_id required"}, ""
	}

	var status, configJSON string
	var executedCampaignID sql.NullString
	err := a.db.QueryRowContext(ctx,
		`SELECT status, COALESCE(campaign_config::text, '{}'), executed_campaign_id::text
		 FROM agent_campaign_recommendations WHERE id = $1 AND organization_id = $2`,
		recID, orgID).Scan(&status, &configJSON, &executedCampaignID)
	if err != nil {
		return map[string]string{"error": "recommendation not found: " + recID}, ""
	}
	if status != "pending" && status != "approved" {
		return map[string]string{"error": fmt.Sprintf("can only update pending or approved recommendations, current status: %s", status)}, ""
	}

	var cfg map[string]interface{}
	json.Unmarshal([]byte(configJSON), &cfg)
	if cfg == nil {
		cfg = map[string]interface{}{}
	}

	updated := []string{}
	setStr := func(key, argKey string) {
		if v, ok := args[argKey].(string); ok && v != "" {
			cfg[key] = v
			updated = append(updated, key)
		}
	}
	setNum := func(key, argKey string) {
		if v, ok := args[argKey].(float64); ok {
			cfg[key] = int(v)
			updated = append(updated, key)
		}
	}

	setStr("campaign_name", "campaign_name")
	setStr("scheduled_date", "scheduled_date")
	setStr("scheduled_time", "scheduled_time")
	setStr("subject", "subject")
	setStr("preview_text", "preview_text")
	setStr("from_name", "from_name")
	setStr("from_email", "from_email")
	setStr("template_id", "template_id")
	setNum("wave_interval_minutes", "wave_interval_minutes")
	setNum("throttle_per_wave", "throttle_per_wave")
	if v, ok := args["isp_quotas"].(map[string]interface{}); ok {
		cfg["isp_quotas"] = v
		updated = append(updated, "isp_quotas")
	}
	if v, ok := args["inclusion_lists"].([]interface{}); ok {
		cfg["inclusion_lists"] = v
		updated = append(updated, "inclusion_lists")
	}
	if v, ok := args["exclusion_lists"].([]interface{}); ok {
		cfg["exclusion_lists"] = v
		updated = append(updated, "exclusion_lists")
	}
	if v, ok := args["audience_priority"].([]interface{}); ok {
		cfg["audience_priority"] = v
		updated = append(updated, "audience_priority")
	}
	if v, ok := args["reasoning"].(string); ok && v != "" {
		updated = append(updated, "reasoning")
	}

	newConfigJSON, _ := json.Marshal(cfg)

	// Recalculate projected volume from ISP quotas
	totalVolume := 0
	if quotas, ok := cfg["isp_quotas"].(map[string]interface{}); ok {
		for _, v := range quotas {
			switch n := v.(type) {
			case float64:
				totalVolume += int(n)
			case int:
				totalVolume += n
			}
		}
	}

	// Build UPDATE query
	updateQ := `UPDATE agent_campaign_recommendations SET campaign_config = $3, projected_volume = $4, updated_at = NOW()`
	updateArgs := []interface{}{recID, orgID, string(newConfigJSON), totalVolume}
	idx := 5
	if v, ok := args["campaign_name"].(string); ok && v != "" {
		updateQ += fmt.Sprintf(`, campaign_name = $%d`, idx)
		updateArgs = append(updateArgs, v)
		idx++
	}
	if v, ok := args["scheduled_date"].(string); ok && v != "" {
		updateQ += fmt.Sprintf(`, scheduled_date = $%d`, idx)
		updateArgs = append(updateArgs, v)
		idx++
	}
	if v, ok := args["scheduled_time"].(string); ok && v != "" {
		updateQ += fmt.Sprintf(`, scheduled_time = $%d`, idx)
		updateArgs = append(updateArgs, v)
		idx++
	}
	if v, ok := args["reasoning"].(string); ok && v != "" {
		updateQ += fmt.Sprintf(`, reasoning = $%d`, idx)
		updateArgs = append(updateArgs, v)
		idx++
	}
	updateQ += ` WHERE id = $1 AND organization_id = $2`

	_, err = a.db.ExecContext(ctx, updateQ, updateArgs...)
	if err != nil {
		return map[string]string{"error": "update failed: " + err.Error()}, ""
	}

	// If approved with a linked campaign, propagate content changes to the real campaign
	campaignUpdated := false
	if status == "approved" && executedCampaignID.Valid && executedCampaignID.String != "" {
		sets := []string{}
		cArgs := []interface{}{}
		ci := 1
		if v, ok := cfg["subject"].(string); ok && v != "" {
			sets = append(sets, fmt.Sprintf("subject = $%d", ci))
			cArgs = append(cArgs, v)
			ci++
		}
		if v, ok := cfg["preview_text"].(string); ok && v != "" {
			sets = append(sets, fmt.Sprintf("preview_text = $%d", ci))
			cArgs = append(cArgs, v)
			ci++
		}
		if v, ok := cfg["from_name"].(string); ok && v != "" {
			sets = append(sets, fmt.Sprintf("from_name = $%d", ci))
			cArgs = append(cArgs, v)
			ci++
		}
		if v, ok := cfg["from_email"].(string); ok && v != "" {
			sets = append(sets, fmt.Sprintf("from_email = $%d", ci))
			cArgs = append(cArgs, v)
			ci++
		}
		if len(sets) > 0 {
			cArgs = append(cArgs, executedCampaignID.String)
			q := fmt.Sprintf("UPDATE mailing_campaigns SET %s, updated_at = NOW() WHERE id = $%d::uuid", strings.Join(sets, ", "), ci)
			if _, e := a.db.ExecContext(ctx, q, cArgs...); e == nil {
				campaignUpdated = true
			}
		}
	}

	result := map[string]interface{}{
		"status":            "updated",
		"recommendation_id": recID,
		"fields_updated":    updated,
		"projected_volume":  totalVolume,
	}
	if campaignUpdated {
		result["campaign_updated"] = true
		result["campaign_id"] = executedCampaignID.String
	}
	return result, fmt.Sprintf("Updated recommendation %s: %s", recID, strings.Join(updated, ", "))
}

func (a *EmailMarketingAgent) toolUnapproveRecommendation(ctx context.Context, orgID string, args map[string]interface{}) (interface{}, string) {
	recID, _ := args["recommendation_id"].(string)
	if recID == "" {
		return map[string]string{"error": "recommendation_id required"}, ""
	}

	var status string
	var executedCampaignID sql.NullString
	err := a.db.QueryRowContext(ctx,
		`SELECT status, executed_campaign_id::text FROM agent_campaign_recommendations WHERE id = $1 AND organization_id = $2`,
		recID, orgID).Scan(&status, &executedCampaignID)
	if err != nil {
		return map[string]string{"error": "recommendation not found: " + recID}, ""
	}
	if status != "approved" {
		return map[string]string{"error": fmt.Sprintf("can only unapprove approved recommendations, current status: %s", status)}, ""
	}

	campaignCancelled := false
	if executedCampaignID.Valid && executedCampaignID.String != "" {
		var campStatus string
		if e := a.db.QueryRowContext(ctx, `SELECT status FROM mailing_campaigns WHERE id = $1::uuid`, executedCampaignID.String).Scan(&campStatus); e == nil {
			if campStatus == "sending" || campStatus == "sent" || campStatus == "completed" {
				return map[string]string{"error": fmt.Sprintf("cannot unapprove: linked campaign %s is already %s", executedCampaignID.String, campStatus)}, ""
			}
			a.db.ExecContext(ctx, `UPDATE mailing_campaigns SET status = 'cancelled', updated_at = NOW() WHERE id = $1::uuid`, executedCampaignID.String)
			campaignCancelled = true
		}
	}

	a.db.ExecContext(ctx,
		`UPDATE agent_campaign_recommendations SET status = 'pending', approved_at = NULL, executed_campaign_id = NULL, updated_at = NOW()
		 WHERE id = $1 AND organization_id = $2`, recID, orgID)

	return map[string]interface{}{
		"status":             "pending",
		"recommendation_id": recID,
		"campaign_cancelled": campaignCancelled,
	}, fmt.Sprintf("Unapproved recommendation %s, reverted to pending", recID)
}

// ── Write Tools ─────────────────────────────────────────────────────────────

func (a *EmailMarketingAgent) toolSaveDomainStrategy(ctx context.Context, orgID string, args map[string]interface{}) (interface{}, string) {
	domain, _ := args["sending_domain"].(string)
	strategy, _ := args["strategy"].(string)
	if domain == "" || (strategy != "warmup" && strategy != "performance") {
		return map[string]string{"error": "sending_domain required, strategy must be 'warmup' or 'performance'"}, ""
	}

	params := map[string]interface{}{}
	if v, ok := args["daily_volume_increase_pct"].(float64); ok {
		params["daily_volume_increase_pct"] = v
	}
	if v, ok := args["max_daily_volume"].(float64); ok {
		params["max_daily_volume"] = int(v)
	}
	if v, ok := args["audience_priority"].([]interface{}); ok {
		params["audience_priority"] = v
	}
	if v, ok := args["content_rotation"].(bool); ok {
		params["content_rotation"] = v
	}

	paramsJSON, _ := json.Marshal(params)
	var id string
	err := a.db.QueryRowContext(ctx,
		`INSERT INTO agent_domain_strategies (organization_id, sending_domain, strategy, params)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (organization_id, sending_domain) DO UPDATE SET strategy=EXCLUDED.strategy, params=EXCLUDED.params, updated_at=NOW()
		 RETURNING id::text`, orgID, domain, strategy, string(paramsJSON)).Scan(&id)
	if err != nil {
		return map[string]string{"error": err.Error()}, ""
	}
	return map[string]interface{}{
		"status": "saved", "id": id, "sending_domain": domain, "strategy": strategy, "params": params,
	}, fmt.Sprintf("Saved %s strategy for %s", strategy, domain)
}

func (a *EmailMarketingAgent) toolCreateRecommendation(ctx context.Context, orgID string, args map[string]interface{}) (interface{}, string) {
	domain, _ := args["sending_domain"].(string)
	dateStr, _ := args["scheduled_date"].(string)
	timeStr, _ := args["scheduled_time"].(string)
	name, _ := args["campaign_name"].(string)
	reasoning, _ := args["reasoning"].(string)

	if domain == "" || dateStr == "" || name == "" {
		return map[string]string{"error": "sending_domain, scheduled_date, and campaign_name are required"}, ""
	}
	if timeStr == "" {
		timeStr = "13:00"
	}

	campaignConfig := map[string]interface{}{
		"sending_domain": domain,
		"name":           name,
		"scheduled_date": dateStr,
		"scheduled_time": timeStr,
	}

	setIfPresent := func(key, argKey string) {
		if v, ok := args[argKey]; ok && v != nil {
			campaignConfig[key] = v
		}
	}
	setIfPresent("isp_quotas", "isp_quotas")
	setIfPresent("inclusion_lists", "inclusion_lists")
	setIfPresent("exclusion_lists", "exclusion_lists")
	setIfPresent("template_id", "template_id")
	setIfPresent("subject", "subject")
	setIfPresent("preview_text", "preview_text")
	setIfPresent("from_name", "from_name")
	setIfPresent("from_email", "from_email")
	setIfPresent("audience_priority", "audience_priority")
	if v, ok := args["wave_interval_minutes"].(float64); ok {
		campaignConfig["wave_interval_minutes"] = int(v)
	}
	if v, ok := args["throttle_per_wave"].(float64); ok {
		campaignConfig["throttle_per_wave"] = int(v)
	}

	configJSON, _ := json.Marshal(campaignConfig)

	totalVolume := 0
	if quotas, ok := args["isp_quotas"].(map[string]interface{}); ok {
		for _, v := range quotas {
			if n, ok := v.(float64); ok {
				totalVolume += int(n)
			}
		}
	}

	var strategy string
	a.db.QueryRowContext(ctx,
		`SELECT strategy FROM agent_domain_strategies WHERE organization_id = $1 AND sending_domain = $2`,
		orgID, domain).Scan(&strategy)

	var id string
	err := a.db.QueryRowContext(ctx,
		`INSERT INTO agent_campaign_recommendations
		 (organization_id, sending_domain, scheduled_date, scheduled_time, campaign_name,
		  campaign_config, reasoning, strategy, projected_volume, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'pending')
		 RETURNING id::text`,
		orgID, domain, dateStr, timeStr, name, string(configJSON), reasoning, strategy, totalVolume,
	).Scan(&id)
	if err != nil {
		return map[string]string{"error": err.Error()}, ""
	}
	return map[string]interface{}{
		"status": "created", "id": id, "campaign_name": name,
		"scheduled_date": dateStr, "scheduled_time": timeStr,
		"projected_volume": totalVolume, "approval_status": "pending",
	}, fmt.Sprintf("Created recommendation: %s for %s", name, dateStr)
}

func (a *EmailMarketingAgent) toolCreateTemplate(ctx context.Context, orgID string, args map[string]interface{}) (interface{}, string) {
	name, _ := args["name"].(string)
	subject, _ := args["subject"].(string)
	fromName, _ := args["from_name"].(string)
	previewText, _ := args["preview_text"].(string)
	folderName, _ := args["folder_name"].(string)
	brand, _ := args["brand"].(string)

	if _, hasHTML := args["html_content"]; hasHTML {
		return map[string]string{
			"error": "html_content is not allowed. Template HTML structure is managed through approved brand templates in the Content Library. " +
				"Use this tool only for metadata (name, subject, from_name, preview_text). " +
				"Content variations are generated automatically by the wave content pipeline using approved template slots.",
		}, ""
	}

	if name == "" || subject == "" {
		return map[string]string{"error": "name and subject are required"}, ""
	}

	var folderID *string
	if folderName != "" {
		var fid string
		err := a.db.QueryRowContext(ctx,
			`SELECT id::text FROM mailing_template_folders WHERE organization_id = $1 AND LOWER(name) = LOWER($2) LIMIT 1`,
			orgID, folderName).Scan(&fid)
		if err != nil {
			err = a.db.QueryRowContext(ctx,
				`INSERT INTO mailing_template_folders (organization_id, name, created_at, updated_at)
				 VALUES ($1, $2, NOW(), NOW()) RETURNING id::text`,
				orgID, folderName).Scan(&fid)
			if err != nil {
				log.Printf("[MarketingAgent] create folder %q: %v", folderName, err)
			}
		}
		if fid != "" {
			folderID = &fid
		}
	}

	if brand != "" && !strings.Contains(name, brand) {
		name = fmt.Sprintf("[%s] %s", brand, name)
	}

	var templateID string
	var err error
	if folderID != nil {
		err = a.db.QueryRowContext(ctx,
			`INSERT INTO mailing_templates (id, organization_id, name, subject, from_name, preview_text, folder_id, status, created_at, updated_at)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6::uuid, 'draft', NOW(), NOW())
			 RETURNING id::text`,
			orgID, name, subject, fromName, previewText, *folderID,
		).Scan(&templateID)
	} else {
		err = a.db.QueryRowContext(ctx,
			`INSERT INTO mailing_templates (id, organization_id, name, subject, from_name, preview_text, status, created_at, updated_at)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, 'draft', NOW(), NOW())
			 RETURNING id::text`,
			orgID, name, subject, fromName, previewText,
		).Scan(&templateID)
	}
	if err != nil {
		return map[string]string{"error": "failed to save template: " + err.Error()}, ""
	}

	return map[string]interface{}{
		"status":      "created",
		"template_id": templateID,
		"name":        name,
		"subject":     subject,
		"from_name":   fromName,
		"brand":       brand,
		"folder":      folderName,
		"note":        "Template saved as draft (metadata only). HTML content is managed through approved brand templates.",
	}, fmt.Sprintf("Created template metadata: %s (id: %s, draft)", name, templateID)
}

func (a *EmailMarketingAgent) toolGenerateTemplate(ctx context.Context, orgID string, args map[string]interface{}) (interface{}, string) {
	campaignType, _ := args["campaign_type"].(string)
	domain, _ := args["sending_domain"].(string)
	referenceTemplateID, _ := args["reference_template_id"].(string)
	if campaignType == "" || domain == "" {
		return map[string]string{"error": "campaign_type and sending_domain are required"}, ""
	}

	if a.aiContent == nil {
		return map[string]string{"error": "AI content service not configured (missing ANTHROPIC_API_KEY or OPENAI_API_KEY)"}, ""
	}

	var referenceHTML string
	if referenceTemplateID != "" {
		a.db.QueryRowContext(ctx,
			`SELECT COALESCE(html_content,'') FROM mailing_templates WHERE id = $1 AND organization_id = $2`,
			referenceTemplateID, orgID).Scan(&referenceHTML)
	}

	result, err := a.aiContent.GenerateEmailTemplates(ctx, mailing.TemplateGenerationRequest{
		CampaignType:  campaignType,
		SendingDomain: domain,
	})
	if err != nil {
		return map[string]string{"error": "template generation failed: " + err.Error()}, ""
	}

	// Save as pending_review — EDITH cannot activate templates with custom HTML
	// without human approval. This prevents structural template modifications
	// while still allowing EDITH to propose new designs for review.
	var savedTemplates []map[string]interface{}
	for _, v := range result.Variations {
		var templateID string
		err := a.db.QueryRowContext(ctx,
			`INSERT INTO mailing_templates (id, organization_id, name, subject, from_name, html_content, preview_text, status, created_at, updated_at)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, '', 'pending_review', NOW(), NOW())
			 RETURNING id::text`,
			orgID,
			fmt.Sprintf("[EDITH-review] %s — %s", campaignType, v.VariantName),
			v.Subject,
			v.FromName,
			v.HTMLContent,
		).Scan(&templateID)
		if err != nil {
			log.Printf("[MarketingAgent] save pending_review template: %v", err)
			continue
		}
		savedTemplates = append(savedTemplates, map[string]interface{}{
			"template_id":  templateID,
			"variant_name": v.VariantName,
			"name":         fmt.Sprintf("[EDITH-review] %s — %s", campaignType, v.VariantName),
			"subject":      v.Subject,
			"from_name":    v.FromName,
			"status":       "pending_review",
		})
	}

	if savedTemplates == nil {
		savedTemplates = []map[string]interface{}{}
	}

	return map[string]interface{}{
		"status":          "pending_review",
		"templates_saved": len(savedTemplates),
		"templates":       savedTemplates,
		"campaign_type":   campaignType,
		"sending_domain":  domain,
		"brand_info":      result.BrandInfo,
		"note":            "Templates saved as pending_review. A human must approve them before they can be used in campaigns. Content variations for active campaigns are generated automatically by the wave content pipeline.",
	}, fmt.Sprintf("Generated %d %s templates for %s (pending human review)", len(savedTemplates), campaignType, domain)
}

func (a *EmailMarketingAgent) toolDeployApprovedCampaign(ctx context.Context, orgID string, args map[string]interface{}) (interface{}, string) {
	recID, _ := args["recommendation_id"].(string)
	if recID == "" {
		return map[string]string{"error": "recommendation_id required"}, ""
	}

	var status string
	var configJSON sql.NullString
	err := a.db.QueryRowContext(ctx,
		`SELECT status, campaign_config::text FROM agent_campaign_recommendations
		 WHERE id = $1 AND organization_id = $2`, recID, orgID).Scan(&status, &configJSON)
	if err != nil {
		return map[string]string{"error": "recommendation not found"}, ""
	}
	if status != "approved" {
		return map[string]string{"error": fmt.Sprintf("recommendation status is '%s', must be 'approved' to deploy", status)}, ""
	}

	// Mark as executed (actual deployment through the PMTA pipeline would be done by the approval worker)
	_, err = a.db.ExecContext(ctx,
		`UPDATE agent_campaign_recommendations SET status = 'executed', updated_at = NOW() WHERE id = $1`, recID)
	if err != nil {
		return map[string]string{"error": err.Error()}, ""
	}

	return map[string]interface{}{
		"status":            "executed",
		"recommendation_id": recID,
		"message":           "Campaign has been queued for deployment through the PMTA wave pipeline.",
	}, fmt.Sprintf("Deployed recommendation %s", recID)
}

// ComputeISPQuotas computes risk-adjusted ISP quota distribution for a target volume.
// Reusable by both the EDITH tool and the REST endpoint.
func (a *EmailMarketingAgent) ComputeISPQuotas(ctx context.Context, orgID, domain string, targetVolume int) map[string]interface{} {
	end := time.Now()
	start := end.Add(-3 * 24 * time.Hour)

	domSubquery := `SELECT t.*, LOWER(COALESCE(NULLIF(t.recipient_domain,''), SPLIT_PART(s.email,'@',2))) as dom
		FROM mailing_tracking_events t
		LEFT JOIN mailing_subscribers s ON t.subscriber_id = s.id
		WHERE t.event_at >= $1 AND t.event_at <= $2`
	subArgs := []interface{}{start, end}
	if domain != "" {
		domSubquery += fmt.Sprintf(` AND LOWER(COALESCE(NULLIF(t.sending_domain,''),'unknown')) = LOWER($%d)`, len(subArgs)+1)
		subArgs = append(subArgs, domain)
	}

	q := fmt.Sprintf(`SELECT %s as isp,
		SUM(CASE WHEN d.event_type = 'sent' THEN 1 ELSE 0 END) as sent,
		SUM(CASE WHEN d.event_type = 'delivered' THEN 1 ELSE 0 END) as delivered,
		SUM(CASE WHEN d.event_type IN ('hard_bounce','bounced') THEN 1 ELSE 0 END) as hard_bounces,
		SUM(CASE WHEN d.event_type = 'soft_bounce' THEN 1 ELSE 0 END) as soft_bounces,
		SUM(CASE WHEN d.event_type IN ('deferred','deferral') THEN 1 ELSE 0 END) as deferred,
		SUM(CASE WHEN d.event_type = 'complained' THEN 1 ELSE 0 END) as complaints,
		SUM(CASE WHEN d.event_type = 'opened' THEN 1 ELSE 0 END) as opens
	FROM (%s) d
	GROUP BY isp`, ispDomainCaseSQL, domSubquery)

	rows, err := a.db.QueryContext(ctx, q, subArgs...)
	if err != nil {
		return map[string]interface{}{"error": "query failed: " + err.Error()}
	}
	defer rows.Close()

	type ispMetric struct {
		ISP          string
		Sent         int
		Delivered    int
		HardBounces  int
		SoftBounces  int
		Deferred     int
		Complaints   int
		Opens        int
		RiskScore      int
		Rec            string
		HardBounceRate float64
		SoftBounceRate float64
		BounceRate     float64
		DeferralRate   float64
		ComplaintRate  float64
		OpenRate       float64
		DeliveryRate   float64
	}
	var metrics []ispMetric
	for rows.Next() {
		var m ispMetric
		rows.Scan(&m.ISP, &m.Sent, &m.Delivered, &m.HardBounces, &m.SoftBounces, &m.Deferred, &m.Complaints, &m.Opens)
		if m.ISP == "other" || m.Sent == 0 {
			continue
		}
		m.HardBounceRate = math.Round(float64(m.HardBounces)/float64(m.Sent)*10000) / 100
		m.SoftBounceRate = math.Round(float64(m.SoftBounces)/float64(m.Sent)*10000) / 100
		m.BounceRate = m.HardBounceRate + m.SoftBounceRate
		m.DeferralRate = math.Round(float64(m.Deferred)/float64(m.Sent)*10000) / 100
		m.ComplaintRate = math.Round(float64(m.Complaints)/float64(m.Sent)*10000) / 100
		m.OpenRate = math.Round(float64(m.Opens)/float64(m.Sent)*10000) / 100
		if m.Delivered > 0 {
			m.DeliveryRate = math.Round(float64(m.Delivered)/float64(m.Sent)*10000) / 100
		}

		normHardBounce := math.Min(m.HardBounceRate/3.0, 1.0)
		normSoftBounce := math.Min(m.SoftBounceRate/20.0, 1.0)
		normDeliveryFail := 1.0 - math.Min(m.DeliveryRate/100, 1)
		normDeferral := math.Min(m.DeferralRate/15.0, 1.0)
		normComplaint := math.Min(m.ComplaintRate/0.1, 1.0)
		rawRisk := normHardBounce*25 + normSoftBounce*10 + normDeliveryFail*20 + normDeferral*15 + normComplaint*30
		if m.DeliveryRate > 50 && m.OpenRate > 10 {
			engagementDiscount := math.Min((m.OpenRate-10)/40.0, 0.15)
			rawRisk = rawRisk * (1.0 - engagementDiscount)
		}
		m.RiskScore = int(math.Round(math.Min(100, rawRisk)))

		switch {
		case m.RiskScore > 80:
			m.Rec = "PAUSE"
		case m.RiskScore > 60:
			m.Rec = "DECREASE"
		case m.RiskScore > 40:
			m.Rec = "CAUTION"
		case m.RiskScore > 20:
			m.Rec = "MAINTAIN"
		default:
			m.Rec = "INCREASE"
		}
		metrics = append(metrics, m)
	}

	// Base quotas: use actual delivered volume as proven capacity signal.
	baseQuotas := map[string]int{}
	for i := range metrics {
		if metrics[i].Delivered > 0 {
			baseQuotas[metrics[i].ISP] = metrics[i].Delivered
		}
	}
	// Fall back to last campaign plans only for ISPs with zero recent deliveries.
	quotaRows, _ := a.db.QueryContext(ctx, `
		SELECT p.isp, p.quota FROM mailing_campaign_isp_plans p
		JOIN mailing_campaigns c ON p.campaign_id = c.id
		WHERE c.organization_id::text = $1
		  AND c.status IN ('completed','sent','scheduled','sending')
		ORDER BY COALESCE(c.completed_at, c.started_at, c.created_at) DESC
		LIMIT 100`, orgID)
	if quotaRows != nil {
		defer quotaRows.Close()
		for quotaRows.Next() {
			var isp string
			var quota int
			quotaRows.Scan(&isp, &quota)
			if _, seen := baseQuotas[isp]; !seen {
				baseQuotas[isp] = quota
			}
		}
	}

	allISPs := []string{"gmail", "yahoo", "microsoft", "apple", "comcast", "att", "cox", "charter"}

	metricByISP := map[string]*ispMetric{}
	for i := range metrics {
		metricByISP[metrics[i].ISP] = &metrics[i]
	}

	rawWeights := map[string]float64{}
	var totalWeight float64
	var healthDetails []map[string]interface{}
	var adjustments []string

	for _, isp := range allISPs {
		base := float64(baseQuotas[isp])
		if base == 0 {
			base = 50
		}
		var multiplier float64
		rec := "MAINTAIN"
		var riskScore int
		var bounceRate, deferralRate, complaintRate, openRate float64

		if m, ok := metricByISP[isp]; ok {
			riskScore = m.RiskScore
			rec = m.Rec
			bounceRate = m.BounceRate
			deferralRate = m.DeferralRate
			complaintRate = m.ComplaintRate
			openRate = m.OpenRate
		}

		switch rec {
		case "PAUSE":
			multiplier = 0
		case "DECREASE":
			multiplier = 0.65
		case "CAUTION":
			multiplier = 0.85
		case "MAINTAIN":
			multiplier = 1.0
		case "INCREASE":
			multiplier = 1.25
		default:
			multiplier = 1.0
		}

		weight := base * multiplier
		rawWeights[isp] = weight
		totalWeight += weight

		hd := map[string]interface{}{
			"isp": isp, "label": ispLabels[isp],
			"risk_score": riskScore, "recommendation": rec,
			"bounce_rate": bounceRate, "deferral_rate": deferralRate,
			"complaint_rate": complaintRate, "open_rate": openRate,
			"base_quota": int(base), "multiplier": multiplier,
		}
		if m, ok := metricByISP[isp]; ok {
			hd["hard_bounce_rate"] = m.HardBounceRate
			hd["soft_bounce_rate"] = m.SoftBounceRate
			hd["delivery_rate"] = m.DeliveryRate
			hd["sent"] = m.Sent
			hd["delivered"] = m.Delivered
		}
		healthDetails = append(healthDetails, hd)

		if rec != "MAINTAIN" {
			adjustments = append(adjustments, fmt.Sprintf("%s: %s (multiplier %.2f)", isp, rec, multiplier))
		}
	}

	quotas := map[string]interface{}{}
	computedTotal := 0
	if totalWeight > 0 {
		for _, isp := range allISPs {
			w := rawWeights[isp]
			if w == 0 {
				quotas[isp] = 0
				continue
			}
			q := int(math.Round(float64(targetVolume) * w / totalWeight))
			if q < 25 {
				q = 25
			}
			quotas[isp] = q
			computedTotal += q
		}
	} else {
		share := targetVolume / len(allISPs)
		for _, isp := range allISPs {
			quotas[isp] = share
			computedTotal += share
		}
	}

	return map[string]interface{}{
		"sending_domain":      domain,
		"target_volume":       targetVolume,
		"computed_volume":     computedTotal,
		"isp_quotas":          quotas,
		"isp_health":          healthDetails,
		"adjustments_applied": adjustments,
		"data_window_days":    3,
	}
}

func (a *EmailMarketingAgent) toolComputeISPQuotas(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	domain, _ := args["sending_domain"].(string)
	targetVolume := 0
	if v, ok := args["target_volume"].(float64); ok {
		targetVolume = int(v)
	}
	if domain == "" || targetVolume <= 0 {
		return map[string]string{"error": "sending_domain and target_volume (>0) are required"}
	}
	return a.ComputeISPQuotas(ctx, orgID, domain, targetVolume)
}

// ── Infrastructure Tools ────────────────────────────────────────────────────

func (a *EmailMarketingAgent) toolGetIPPoolHealth(ctx context.Context, orgID string) interface{} {
	rows, err := a.db.QueryContext(ctx, `
		SELECT p.id::text, p.name, COALESCE(p.pool_type,''), COALESCE(p.status,''),
		       COUNT(i.id) AS total_ips,
		       COUNT(CASE WHEN i.status IN ('active','warmup') THEN 1 END) AS active_ips,
		       COUNT(CASE WHEN i.status = 'paused' THEN 1 END) AS paused_ips,
		       COUNT(CASE WHEN i.status = 'quarantined' THEN 1 END) AS quarantined_ips
		FROM mailing_ip_pools p
		LEFT JOIN mailing_ip_addresses i ON i.pool_id = p.id
		WHERE p.organization_id = $1
		GROUP BY p.id, p.name, p.pool_type, p.status
		ORDER BY p.name`, orgID)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()

	var pools []map[string]interface{}
	for rows.Next() {
		var id, name, poolType, status string
		var total, active, paused, quarantined int
		rows.Scan(&id, &name, &poolType, &status, &total, &active, &paused, &quarantined)
		pools = append(pools, map[string]interface{}{
			"id": id, "name": name, "pool_type": poolType, "status": status,
			"total_ips": total, "active_ips": active, "paused_ips": paused, "quarantined_ips": quarantined,
		})
	}

	ipRows, err := a.db.QueryContext(ctx, `
		SELECT i.id::text, i.ip_address::text, COALESCE(i.hostname,''), i.status,
		       p.name AS pool_name, COALESCE(i.warmup_stage,''), COALESCE(i.warmup_day,0),
		       COALESCE(i.reputation_score,0), i.total_sent, i.total_delivered, i.total_bounced
		FROM mailing_ip_addresses i
		LEFT JOIN mailing_ip_pools p ON i.pool_id = p.id
		WHERE i.organization_id = $1
		ORDER BY p.name, i.ip_address`, orgID)
	if err == nil {
		defer ipRows.Close()
		var ips []map[string]interface{}
		for ipRows.Next() {
			var id, addr, hostname, status, poolName, warmupStage string
			var warmupDay int
			var repScore float64
			var sent, delivered, bounced int64
			ipRows.Scan(&id, &addr, &hostname, &status, &poolName, &warmupStage, &warmupDay, &repScore, &sent, &delivered, &bounced)
			ips = append(ips, map[string]interface{}{
				"id": id, "ip_address": addr, "hostname": hostname, "status": status,
				"pool_name": poolName, "warmup_stage": warmupStage, "warmup_day": warmupDay,
				"reputation_score": repScore, "total_sent": sent, "total_delivered": delivered, "total_bounced": bounced,
			})
		}
		return map[string]interface{}{"pools": pools, "ips": ips}
	}
	return map[string]interface{}{"pools": pools}
}

func (a *EmailMarketingAgent) toolGetPreflightStatus(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	domain, _ := args["sending_domain"].(string)
	if domain == "" {
		return map[string]string{"error": "sending_domain required"}
	}
	pf := preflightDeployCheck(ctx, a.db, orgID, domain)
	var errors []map[string]interface{}
	for _, e := range pf.Errors {
		errors = append(errors, map[string]interface{}{
			"check": e.Check, "message": e.Message,
		})
	}
	var warnings []map[string]interface{}
	for _, w := range pf.Warnings {
		warnings = append(warnings, map[string]interface{}{
			"check": w.Check, "message": w.Message,
		})
	}
	return map[string]interface{}{
		"ok": pf.OK, "sending_domain": domain,
		"errors": errors, "warnings": warnings,
	}
}

func (a *EmailMarketingAgent) toolGetPipelineHealth(ctx context.Context, orgID string) interface{} {
	health := map[string]interface{}{}

	// Wave content cache inventory
	var cacheTotal, cacheAvailable int
	a.db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(CASE WHEN used_at IS NULL THEN 1 END) FROM mailing_wave_content_cache WHERE version = $1`, mailing.GeneratorVersion).Scan(&cacheTotal, &cacheAvailable)
	health["wave_cache"] = map[string]interface{}{"total": cacheTotal, "available": cacheAvailable}

	// PMTA reachability
	conn, err := net.DialTimeout("tcp", "15.204.101.125:587", 5*time.Second)
	pmtaUp := err == nil
	if conn != nil {
		conn.Close()
	}
	health["pmta_reachable"] = pmtaUp

	// Active campaigns
	var scheduled, sending int
	a.db.QueryRowContext(ctx, `SELECT COUNT(CASE WHEN status='scheduled' THEN 1 END), COUNT(CASE WHEN status='sending' THEN 1 END) FROM mailing_campaigns WHERE organization_id = $1`, orgID).Scan(&scheduled, &sending)
	health["campaigns"] = map[string]interface{}{"scheduled": scheduled, "sending": sending}

	// IP health summary
	var totalIPs, activeIPs int
	a.db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(CASE WHEN status IN ('active','warmup') THEN 1 END) FROM mailing_ip_addresses WHERE organization_id = $1`, orgID).Scan(&totalIPs, &activeIPs)
	health["ips"] = map[string]interface{}{"total": totalIPs, "active": activeIPs}

	// Pending recommendations
	var pendingRecs int
	a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_campaign_recommendations WHERE organization_id = $1 AND status = 'pending'`, orgID).Scan(&pendingRecs)
	health["pending_recommendations"] = pendingRecs

	return health
}

func (a *EmailMarketingAgent) toolManageIPStatus(ctx context.Context, orgID string, args map[string]interface{}) (interface{}, string) {
	ipID, _ := args["ip_id"].(string)
	newStatus, _ := args["status"].(string)
	if ipID == "" || newStatus == "" {
		return map[string]string{"error": "ip_id and status required"}, ""
	}
	allowed := map[string]bool{"active": true, "warmup": true, "paused": true}
	if !allowed[newStatus] {
		return map[string]string{"error": "status must be active, warmup, or paused"}, ""
	}
	res, err := a.db.ExecContext(ctx,
		`UPDATE mailing_ip_addresses SET status = $1, updated_at = NOW() WHERE id = $2 AND organization_id = $3`,
		newStatus, ipID, orgID)
	if err != nil {
		return map[string]string{"error": err.Error()}, ""
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return map[string]string{"error": "IP not found"}, ""
	}
	return map[string]interface{}{"status": "updated", "ip_id": ipID, "new_status": newStatus}, fmt.Sprintf("Updated IP %s to %s", ipID, newStatus)
}

// ── Analytics Tools ─────────────────────────────────────────────────────────

func (a *EmailMarketingAgent) toolGetISPSendingInsights(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	domain, _ := args["sending_domain"].(string)
	days := 7
	if d, ok := args["days"].(float64); ok && int(d) > 0 {
		days = min(int(d), 30)
	}

	end := time.Now()
	start := end.Add(-time.Duration(days) * 24 * time.Hour)

	q := fmt.Sprintf(`SELECT DATE(t.event_at) AS day,
		%s AS isp,
		SUM(CASE WHEN t.event_type = 'sent' THEN 1 ELSE 0 END) AS sent,
		SUM(CASE WHEN t.event_type = 'delivered' THEN 1 ELSE 0 END) AS delivered,
		SUM(CASE WHEN t.event_type IN ('hard_bounce','bounced') THEN 1 ELSE 0 END) AS hard_bounces,
		SUM(CASE WHEN t.event_type = 'soft_bounce' THEN 1 ELSE 0 END) AS soft_bounces,
		SUM(CASE WHEN t.event_type = 'opened' THEN 1 ELSE 0 END) AS opens,
		SUM(CASE WHEN t.event_type = 'clicked' THEN 1 ELSE 0 END) AS clicks,
		SUM(CASE WHEN t.event_type = 'complained' THEN 1 ELSE 0 END) AS complaints
	FROM mailing_tracking_events t
	LEFT JOIN mailing_subscribers s ON t.subscriber_id = s.id
	WHERE t.event_at >= $1 AND t.event_at <= $2 AND t.organization_id = $3`,
		ispDomainCaseSQL)

	qArgs := []interface{}{start, end, orgID}
	if domain != "" {
		q += fmt.Sprintf(` AND LOWER(COALESCE(NULLIF(t.sending_domain,''),'unknown')) = LOWER($%d)`, len(qArgs)+1)
		qArgs = append(qArgs, domain)
	}
	q += ` GROUP BY day, isp ORDER BY day, isp`

	rows, err := a.db.QueryContext(ctx, q, qArgs...)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()

	var insights []map[string]interface{}
	for rows.Next() {
		var day time.Time
		var isp string
		var sent, delivered, hardBounces, softBounces, opens, clicks, complaints int
		rows.Scan(&day, &isp, &sent, &delivered, &hardBounces, &softBounces, &opens, &clicks, &complaints)
		if isp == "other" || sent == 0 {
			continue
		}
		insights = append(insights, map[string]interface{}{
			"date": day.Format("2006-01-02"), "isp": isp,
			"sent": sent, "delivered": delivered,
			"hard_bounces": hardBounces, "soft_bounces": softBounces,
			"opens": opens, "clicks": clicks, "complaints": complaints,
			"delivery_rate": math.Round(float64(delivered)/float64(sent)*10000) / 100,
			"open_rate":     math.Round(float64(opens)/float64(sent)*10000) / 100,
		})
	}
	if insights == nil {
		insights = []map[string]interface{}{}
	}
	return map[string]interface{}{"insights": insights, "days": days, "domain_filter": domain}
}

func (a *EmailMarketingAgent) toolGetDeliverabilityReport(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	days := 7
	if d, ok := args["days"].(float64); ok && int(d) > 0 {
		days = min(int(d), 30)
	}

	end := time.Now()
	start := end.Add(-time.Duration(days) * 24 * time.Hour)

	rows, err := a.db.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(t.sending_domain,''),'unknown') AS domain,
		       SUM(CASE WHEN t.event_type = 'sent' THEN 1 ELSE 0 END) AS sent,
		       SUM(CASE WHEN t.event_type = 'delivered' THEN 1 ELSE 0 END) AS delivered,
		       SUM(CASE WHEN t.event_type IN ('hard_bounce','bounced') THEN 1 ELSE 0 END) AS hard_bounces,
		       SUM(CASE WHEN t.event_type = 'soft_bounce' THEN 1 ELSE 0 END) AS soft_bounces,
		       SUM(CASE WHEN t.event_type = 'opened' THEN 1 ELSE 0 END) AS opens,
		       SUM(CASE WHEN t.event_type = 'clicked' THEN 1 ELSE 0 END) AS clicks,
		       SUM(CASE WHEN t.event_type = 'complained' THEN 1 ELSE 0 END) AS complaints
		FROM mailing_tracking_events t
		WHERE t.event_at >= $1 AND t.event_at <= $2 AND t.organization_id = $3
		GROUP BY domain ORDER BY sent DESC`, start, end, orgID)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()

	var domains []map[string]interface{}
	for rows.Next() {
		var domain string
		var sent, delivered, hardBounces, softBounces, opens, clicks, complaints int
		rows.Scan(&domain, &sent, &delivered, &hardBounces, &softBounces, &opens, &clicks, &complaints)
		if sent == 0 {
			continue
		}
		domains = append(domains, map[string]interface{}{
			"sending_domain": domain, "sent": sent, "delivered": delivered,
			"hard_bounces": hardBounces, "soft_bounces": softBounces,
			"opens": opens, "clicks": clicks, "complaints": complaints,
			"delivery_rate":    math.Round(float64(delivered)/float64(sent)*10000) / 100,
			"hard_bounce_rate": math.Round(float64(hardBounces)/float64(sent)*10000) / 100,
			"complaint_rate":   math.Round(float64(complaints)/float64(sent)*10000) / 100,
			"open_rate":        math.Round(float64(opens)/float64(sent)*10000) / 100,
		})
	}
	if domains == nil {
		domains = []map[string]interface{}{}
	}
	return map[string]interface{}{"domains": domains, "window_days": days}
}

func (a *EmailMarketingAgent) toolGetInjectionAnalytics(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	campaignID, _ := args["campaign_id"].(string)
	limit := 10
	if l, ok := args["limit"].(float64); ok && int(l) > 0 {
		limit = min(int(l), 50)
	}

	q := `SELECT w.id::text, w.wave_number, w.isp_plan_id::text, w.status,
	             w.planned_recipients, COALESCE(w.enqueued_recipients,0),
	             w.batch_size, w.scheduled_at, w.started_at, w.completed_at,
	             COALESCE(p.isp,'')
	      FROM mailing_campaign_waves w
	      LEFT JOIN mailing_campaign_isp_plans p ON w.isp_plan_id = p.id`
	qArgs := []interface{}{}
	if campaignID != "" {
		q += ` WHERE w.campaign_id = $1::uuid`
		qArgs = append(qArgs, campaignID)
	} else {
		q += ` JOIN mailing_campaigns c ON w.campaign_id = c.id WHERE c.organization_id = $1`
		qArgs = append(qArgs, orgID)
	}
	q += fmt.Sprintf(` ORDER BY w.scheduled_at DESC LIMIT %d`, limit)

	rows, err := a.db.QueryContext(ctx, q, qArgs...)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()

	var waves []map[string]interface{}
	for rows.Next() {
		var id, planID, status, isp string
		var waveNum, planned, enqueued, batchSize int
		var scheduledAt time.Time
		var startedAt, completedAt sql.NullTime
		rows.Scan(&id, &waveNum, &planID, &status, &planned, &enqueued, &batchSize, &scheduledAt, &startedAt, &completedAt, &isp)
		w := map[string]interface{}{
			"id": id, "wave_number": waveNum, "isp": isp, "status": status,
			"planned_recipients": planned, "enqueued_recipients": enqueued,
			"batch_size": batchSize, "scheduled_at": scheduledAt.Format(time.RFC3339),
		}
		if startedAt.Valid {
			w["started_at"] = startedAt.Time.Format(time.RFC3339)
		}
		if completedAt.Valid {
			w["completed_at"] = completedAt.Time.Format(time.RFC3339)
		}
		waves = append(waves, w)
	}
	if waves == nil {
		waves = []map[string]interface{}{}
	}
	return map[string]interface{}{"waves": waves, "count": len(waves)}
}

func (a *EmailMarketingAgent) toolGetContentLearnings(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	domain, _ := args["sending_domain"].(string)
	limit := 20
	if l, ok := args["limit"].(float64); ok && int(l) > 0 {
		limit = min(int(l), 50)
	}

	q := `SELECT c.id::text, c.name, c.subject, COALESCE(c.from_name,''),
	             c.sent_count, COALESCE(c.open_count,0), COALESCE(c.click_count,0),
	             COALESCE(c.hard_bounce_count,0), COALESCE(c.soft_bounce_count,0),
	             COALESCE(c.complaint_count,0), c.created_at
	      FROM mailing_campaigns c
	      WHERE c.organization_id = $1 AND c.status IN ('completed','sent','completed_with_errors')
	        AND c.sent_count > 0`
	qArgs := []interface{}{orgID}
	if domain != "" {
		q += fmt.Sprintf(` AND LOWER(COALESCE(c.from_email,'')) LIKE '%%@' || LOWER($%d)`, len(qArgs)+1)
		qArgs = append(qArgs, domain)
	}
	q += fmt.Sprintf(` ORDER BY c.created_at DESC LIMIT %d`, limit)

	rows, err := a.db.QueryContext(ctx, q, qArgs...)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()

	var learnings []map[string]interface{}
	for rows.Next() {
		var id, name, subject, fromName string
		var sent, opens, clicks, hardBounces, softBounces, complaints int
		var createdAt time.Time
		rows.Scan(&id, &name, &subject, &fromName, &sent, &opens, &clicks, &hardBounces, &softBounces, &complaints, &createdAt)
		l := map[string]interface{}{
			"campaign_id": id, "name": name, "subject": subject, "from_name": fromName,
			"sent": sent, "opens": opens, "clicks": clicks,
			"hard_bounces": hardBounces, "soft_bounces": softBounces, "complaints": complaints,
			"date": createdAt.Format("2006-01-02"),
		}
		if sent > 0 {
			l["open_rate"] = math.Round(float64(opens)/float64(sent)*10000) / 100
			l["click_rate"] = math.Round(float64(clicks)/float64(sent)*10000) / 100
			l["hard_bounce_rate"] = math.Round(float64(hardBounces)/float64(sent)*10000) / 100
		}
		learnings = append(learnings, l)
	}
	if learnings == nil {
		learnings = []map[string]interface{}{}
	}
	return map[string]interface{}{"learnings": learnings, "count": len(learnings)}
}

// ── Content & Audience Tools ────────────────────────────────────────────────

func (a *EmailMarketingAgent) toolGetWaveCacheStatus(ctx context.Context, orgID string) interface{} {
	rows, err := a.db.QueryContext(ctx, `
		SELECT brand_key, COALESCE(campaign_type,'newsletter'),
		       COUNT(*) AS total,
		       COUNT(CASE WHEN used_at IS NULL THEN 1 END) AS available,
		       COUNT(CASE WHEN used_at IS NOT NULL THEN 1 END) AS used
		FROM mailing_wave_content_cache
		WHERE version = $1
		GROUP BY brand_key, campaign_type
		ORDER BY brand_key, campaign_type`, mailing.GeneratorVersion)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()

	var brands []map[string]interface{}
	for rows.Next() {
		var brand, campType string
		var total, available, used int
		rows.Scan(&brand, &campType, &total, &available, &used)
		brands = append(brands, map[string]interface{}{
			"brand_key": brand, "campaign_type": campType,
			"total": total, "available": available, "used": used,
		})
	}
	if brands == nil {
		brands = []map[string]interface{}{}
	}
	return map[string]interface{}{"cache_entries": brands, "version": mailing.GeneratorVersion}
}

func (a *EmailMarketingAgent) toolRefreshWaveCache(ctx context.Context, orgID string, args map[string]interface{}) (interface{}, string) {
	brand, _ := args["brand"].(string)
	if brand == "" {
		brand = "all"
	}
	waves := 5
	if w, ok := args["waves"].(float64); ok && int(w) > 0 {
		waves = min(int(w), 10)
	}

	brands := knownBrands()
	var refreshed []string

	for key, bc := range brands {
		if brand != "all" && key != brand && bc.BrandName != brand {
			continue
		}
		if a.aiContent == nil {
			continue
		}
		scraped := a.aiContent.ScrapeBrandIntelligence(ctx, bc.BlogDomain)
		if scraped == nil {
			continue
		}
		contentPool := scraped.BlogPosts
		if len(contentPool) < 3 && len(bc.FallbackContent) > 0 {
			contentPool = bc.FallbackContent
		}
		waveGen := mailing.NewWaveContentGenerator(a.aiContent)
		result, genErr := waveGen.GenerateFull(ctx, mailing.WaveContentRequest{
			SendingDomain: bc.SendingDomain,
			BrandName:     bc.BrandName,
			NumWaves:      waves,
			CampaignType:  bc.CampaignType,
			Voice:         bc.Voice,
			Audience:      bc.Audience,
			DesignSystem:  bc.DesignSystem,
			HTMLTemplate:  bc.HTMLTemplate,
			BrandInfo:     scraped,
			ContentPool:   contentPool,
		})
		if genErr != nil || result == nil {
			continue
		}
		for i, v := range result.Variations {
			var editJSON []byte
			if i < len(result.Editorial) {
				editJSON, _ = json.Marshal(result.Editorial[i])
			}
			a.db.ExecContext(ctx, `
				INSERT INTO mailing_wave_content_cache
				(brand_key, wave_index, subject, preview_text, from_name, html_content, diagnostics, generated_at, version, campaign_type, editorial_json)
				VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), $8, $9, $10)`,
				bc.Key, i, v.Subject, v.PreviewText, v.FromName, v.HTMLContent,
				"{}", mailing.GeneratorVersion, bc.CampaignType, string(editJSON))
		}
		refreshed = append(refreshed, key)
	}

	return map[string]interface{}{
		"status":   "refreshed",
		"brands":   refreshed,
		"waves":    waves,
	}, fmt.Sprintf("Refreshed wave cache for %d brands", len(refreshed))
}

func (a *EmailMarketingAgent) toolGetSubscriber360(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	email, _ := args["email"].(string)
	if email == "" {
		return map[string]string{"error": "email required"}
	}

	rows, err := a.db.QueryContext(ctx, `
		SELECT s.id::text, s.email, COALESCE(s.first_name,''), COALESCE(s.last_name,''),
		       s.status, l.name AS list_name,
		       COALESCE(s.total_opens,0), COALESCE(s.total_clicks,0),
		       s.last_open_at, s.last_click_at, s.last_email_at,
		       COALESCE(s.engagement_score,0), COALESCE(s.isp,''),
		       s.subscribed_at
		FROM mailing_subscribers s
		LEFT JOIN mailing_lists l ON s.list_id = l.id
		WHERE s.organization_id = $1 AND LOWER(s.email) = LOWER($2)
		ORDER BY s.subscribed_at DESC`, orgID, email)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()

	var subs []map[string]interface{}
	for rows.Next() {
		var id, em, first, last, status, listName, isp string
		var totalOpens, totalClicks int
		var lastOpen, lastClick, lastEmail sql.NullTime
		var score float64
		var subscribedAt time.Time
		rows.Scan(&id, &em, &first, &last, &status, &listName, &totalOpens, &totalClicks,
			&lastOpen, &lastClick, &lastEmail, &score, &isp, &subscribedAt)
		sub := map[string]interface{}{
			"id": id, "email": em, "first_name": first, "last_name": last,
			"status": status, "list": listName, "isp": isp,
			"total_opens": totalOpens, "total_clicks": totalClicks,
			"engagement_score": score, "subscribed_at": subscribedAt.Format(time.RFC3339),
		}
		if lastOpen.Valid {
			sub["last_open_at"] = lastOpen.Time.Format(time.RFC3339)
		}
		if lastClick.Valid {
			sub["last_click_at"] = lastClick.Time.Format(time.RFC3339)
		}
		if lastEmail.Valid {
			sub["last_email_at"] = lastEmail.Time.Format(time.RFC3339)
		}
		subs = append(subs, sub)
	}

	// Recent events
	var events []map[string]interface{}
	evRows, evErr := a.db.QueryContext(ctx, `
		SELECT t.event_type, t.event_at, COALESCE(c.name,'')
		FROM mailing_tracking_events t
		LEFT JOIN mailing_campaigns c ON t.campaign_id = c.id
		LEFT JOIN mailing_subscribers s ON t.subscriber_id = s.id
		WHERE t.organization_id = $1 AND LOWER(s.email) = LOWER($2)
		ORDER BY t.event_at DESC LIMIT 20`, orgID, email)
	if evErr == nil {
		defer evRows.Close()
		for evRows.Next() {
			var evType, campName string
			var evAt time.Time
			evRows.Scan(&evType, &evAt, &campName)
			events = append(events, map[string]interface{}{
				"event": evType, "at": evAt.Format(time.RFC3339), "campaign": campName,
			})
		}
	}

	return map[string]interface{}{
		"email":         email,
		"subscriptions": subs,
		"recent_events": events,
	}
}

func (a *EmailMarketingAgent) toolGetSegmentPreview(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	segmentID, _ := args["segment_id"].(string)
	if segmentID == "" {
		return map[string]string{"error": "segment_id required"}
	}

	var name, segType, conditions string
	var subCount int
	err := a.db.QueryRowContext(ctx,
		`SELECT name, COALESCE(segment_type,'dynamic'), subscriber_count, COALESCE(conditions::text,'{}')
		 FROM mailing_segments WHERE id = $1 AND organization_id = $2`,
		segmentID, orgID).Scan(&name, &segType, &subCount, &conditions)
	if err != nil {
		return map[string]string{"error": "segment not found"}
	}

	var conds interface{}
	json.Unmarshal([]byte(conditions), &conds)

	// Sample subscribers
	sampleRows, sErr := a.db.QueryContext(ctx, `
		SELECT s.email, COALESCE(s.isp,''), COALESCE(s.total_opens,0), s.last_open_at
		FROM mailing_segment_subscribers ss
		JOIN mailing_subscribers s ON ss.subscriber_id = s.id
		WHERE ss.segment_id = $1
		ORDER BY RANDOM() LIMIT 10`, segmentID)
	var samples []map[string]interface{}
	if sErr == nil {
		defer sampleRows.Close()
		for sampleRows.Next() {
			var em, isp string
			var opens int
			var lastOpen sql.NullTime
			sampleRows.Scan(&em, &isp, &opens, &lastOpen)
			s := map[string]interface{}{"email": em, "isp": isp, "total_opens": opens}
			if lastOpen.Valid {
				s["last_open_at"] = lastOpen.Time.Format(time.RFC3339)
			}
			samples = append(samples, s)
		}
	}

	return map[string]interface{}{
		"id": segmentID, "name": name, "type": segType,
		"subscriber_count": subCount, "conditions": conds,
		"sample_subscribers": samples,
	}
}

// ── Approve Recommendation (delegates to shared doApproveRecommendation) ────

func (a *EmailMarketingAgent) toolApproveRecommendation(ctx context.Context, orgID string, args map[string]interface{}) (interface{}, string) {
	recID, _ := args["recommendation_id"].(string)
	if recID == "" {
		return map[string]string{"error": "recommendation_id required"}, ""
	}

	res, err := a.doApproveRecommendation(ctx, orgID, recID)
	if err != nil {
		return map[string]string{"error": err.Error()}, ""
	}

	return map[string]interface{}{
		"status":         "approved",
		"recommendation": recID,
		"campaign_id":    res.CampaignID,
		"campaign_name":  res.CampaignName,
		"scheduled_at":   res.ScheduledAt.Format(time.RFC3339),
		"total_audience": res.TotalAudience,
		"target_isps":    res.TargetISPs,
		"isp_plans":      res.ISPPlanCount,
	}, fmt.Sprintf("Approved recommendation %s → campaign %s", recID, res.CampaignID)
}
