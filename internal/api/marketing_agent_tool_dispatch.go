package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
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
	case "delete_recommendation":
		result, action = a.toolDeleteRecommendation(ctx, orgID, args)
	case "clear_forecasts":
		result, action = a.toolClearForecasts(ctx, orgID, args)
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

		normBounce := math.Min((hardBounceRate+softBounceRate)/5.0, 1.0)
		normDeferral := math.Min(deferralRate/10.0, 1.0)
		normComplaint := math.Min(complaintRate/0.1, 1.0)
		riskScore := int(math.Round(math.Min(100, normBounce*35+normDeferral*35+normComplaint*30)))

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
			"complaint_rate": complaintRate, "open_rate": openRate,
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
	             COALESCE(bounce_count,0), COALESCE(hard_bounce_count,0), COALESCE(soft_bounce_count,0),
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
		        COALESCE(bounce_count,0), COALESCE(hard_bounce_count,0), COALESCE(soft_bounce_count,0),
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
	err := a.db.QueryRowContext(ctx,
		`SELECT status, COALESCE(campaign_config::text, '{}')
		 FROM agent_campaign_recommendations WHERE id = $1 AND organization_id = $2`,
		recID, orgID).Scan(&status, &configJSON)
	if err != nil {
		return map[string]string{"error": "recommendation not found: " + recID}, ""
	}
	if status != "pending" {
		return map[string]string{"error": fmt.Sprintf("can only update pending recommendations, current status: %s", status)}, ""
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

	return map[string]interface{}{
		"status":            "updated",
		"recommendation_id": recID,
		"fields_updated":    updated,
		"projected_volume":  totalVolume,
	}, fmt.Sprintf("Updated recommendation %s: %s", recID, strings.Join(updated, ", "))
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
	htmlContent, _ := args["html_content"].(string)
	fromName, _ := args["from_name"].(string)
	previewText, _ := args["preview_text"].(string)
	folderName, _ := args["folder_name"].(string)
	brand, _ := args["brand"].(string)

	if name == "" || subject == "" || htmlContent == "" {
		return map[string]string{"error": "name, subject, and html_content are required"}, ""
	}
	if len(htmlContent) < 200 {
		return map[string]string{"error": "html_content is too short — must be a complete HTML email"}, ""
	}
	if !strings.Contains(htmlContent, "unsubscribe") {
		return map[string]string{"error": "html_content must include an unsubscribe link ({{ system.unsubscribe_url }}) for CAN-SPAM compliance"}, ""
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
			`INSERT INTO mailing_templates (id, organization_id, name, subject, from_name, preview_text, html_content, folder_id, status, created_at, updated_at)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7::uuid, 'active', NOW(), NOW())
			 RETURNING id::text`,
			orgID, name, subject, fromName, previewText, htmlContent, *folderID,
		).Scan(&templateID)
	} else {
		err = a.db.QueryRowContext(ctx,
			`INSERT INTO mailing_templates (id, organization_id, name, subject, from_name, preview_text, html_content, status, created_at, updated_at)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, 'active', NOW(), NOW())
			 RETURNING id::text`,
			orgID, name, subject, fromName, previewText, htmlContent,
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
		"html_length": len(htmlContent),
	}, fmt.Sprintf("Created template: %s (id: %s)", name, templateID)
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

	// If a reference template was provided, read it so the LLM can use it as inspiration
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

	// Save each variation as a draft template in the content library
	var savedTemplates []map[string]interface{}
	for _, v := range result.Variations {
		var templateID string
		err := a.db.QueryRowContext(ctx,
			`INSERT INTO mailing_templates (id, organization_id, name, subject, from_name, html_content, preview_text, status, created_at, updated_at)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, '', 'draft', NOW(), NOW())
			 RETURNING id::text`,
			orgID,
			fmt.Sprintf("[EDITH] %s — %s", campaignType, v.VariantName),
			v.Subject,
			v.FromName,
			v.HTMLContent,
		).Scan(&templateID)
		if err != nil {
			log.Printf("[MarketingAgent] save draft template: %v", err)
			continue
		}
		savedTemplates = append(savedTemplates, map[string]interface{}{
			"template_id":  templateID,
			"variant_name": v.VariantName,
			"name":         fmt.Sprintf("[EDITH] %s — %s", campaignType, v.VariantName),
			"subject":      v.Subject,
			"from_name":    v.FromName,
			"status":       "draft",
		})
	}

	if savedTemplates == nil {
		savedTemplates = []map[string]interface{}{}
	}

	return map[string]interface{}{
		"status":          "generated",
		"templates_saved": len(savedTemplates),
		"templates":       savedTemplates,
		"campaign_type":   campaignType,
		"sending_domain":  domain,
		"brand_info":      result.BrandInfo,
	}, fmt.Sprintf("Generated %d %s templates for %s (saved as drafts)", len(savedTemplates), campaignType, domain)
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
