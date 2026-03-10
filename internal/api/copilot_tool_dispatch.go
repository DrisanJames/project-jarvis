package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/segmentation"
)

func (c *CampaignCopilot) executeCopilotTool(ctx context.Context, orgID, name, arguments string) (string, string) {
	var args map[string]interface{}
	json.Unmarshal([]byte(arguments), &args)

	log.Printf("[CampaignCopilot] tool=%s args=%s", name, arguments)

	var result interface{}
	var action string

	switch name {
	case "list_campaigns":
		result = c.toolListCampaigns(ctx, orgID, args)
	case "get_campaign_details":
		result = c.toolGetCampaignDetails(ctx, orgID, args)
	case "search_campaigns_by_name":
		result = c.toolSearchCampaigns(ctx, orgID, args)
	case "list_lists":
		result = c.toolListLists(ctx, orgID)
	case "list_segments":
		result = c.toolListSegments(ctx, orgID)
	case "list_templates":
		result = c.toolListTemplates(ctx, orgID, args)
	case "get_template":
		result = c.toolGetTemplate(ctx, orgID, args)
	case "get_isp_performance":
		result = c.toolGetISPPerformance(ctx, orgID, args)
	case "get_sending_insights":
		result = c.toolGetSendingInsights(ctx, orgID)
	case "get_last_quotas":
		result = c.toolGetLastQuotas(ctx, orgID)
	case "get_sending_domains":
		result = c.toolGetSendingDomains(ctx, orgID)
	case "estimate_audience":
		result = c.toolEstimateAudience(ctx, orgID, args)
	case "clone_campaign":
		result, action = c.toolCloneCampaign(ctx, orgID, args)
	case "deploy_campaign":
		result, action = c.toolDeployCampaign(ctx, orgID, args)
	case "save_draft":
		result, action = c.toolSaveDraft(ctx, orgID, args)
	case "create_segment":
		result, action = c.toolCreateSegment(ctx, orgID, args)
	case "emergency_stop":
		result, action = c.toolEmergencyStop(ctx, orgID, args)
	default:
		result = map[string]string{"error": "unknown tool: " + name}
	}

	out, _ := json.Marshal(result)
	return string(out), action
}

// ── Read Tools ──────────────────────────────────────────────────────────

func (c *CampaignCopilot) toolListCampaigns(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	statusFilter, _ := args["status_filter"].(string)
	limit := 20
	if l, ok := args["limit"].(float64); ok && int(l) > 0 {
		limit = min(int(l), 50)
	}

	q := `SELECT id, name, status, COALESCE(from_email,''), COALESCE(sending_profile_id::text,''),
	             sent_count, COALESCE(open_count,0), COALESCE(click_count,0),
	             COALESCE(bounce_count,0), COALESCE(complaint_count,0),
	             scheduled_at, started_at, completed_at, created_at
	      FROM mailing_campaigns
	      WHERE organization_id = $1`
	qArgs := []interface{}{orgID}
	if statusFilter != "" {
		q += ` AND status = $2`
		qArgs = append(qArgs, statusFilter)
	}
	q += ` ORDER BY created_at DESC LIMIT ` + fmt.Sprintf("%d", limit)

	rows, err := c.db.QueryContext(ctx, q, qArgs...)
	if err != nil {
		log.Printf("[CampaignCopilot] toolListCampaigns: query error: %v", err)
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()

	var campaigns []map[string]interface{}
	for rows.Next() {
		var id, name, status, fromEmail, profileID string
		var sent, opens, clicks, bounces, complaints int
		var scheduledAt, startedAt, completedAt, createdAt sql.NullTime
		if err := rows.Scan(&id, &name, &status, &fromEmail, &profileID,
			&sent, &opens, &clicks, &bounces, &complaints,
			&scheduledAt, &startedAt, &completedAt, &createdAt); err != nil {
			continue
		}
		c := map[string]interface{}{
			"id": id, "name": name, "status": status, "from_email": fromEmail,
			"sent_count": sent, "open_count": opens, "click_count": clicks,
			"bounce_count": bounces, "complaint_count": complaints,
			"created_at": createdAt.Time.Format(time.RFC3339),
		}
		if sent > 0 {
			c["open_rate"] = fmt.Sprintf("%.1f%%", float64(opens)/float64(sent)*100)
			c["click_rate"] = fmt.Sprintf("%.1f%%", float64(clicks)/float64(sent)*100)
		}
		if scheduledAt.Valid {
			c["scheduled_at"] = scheduledAt.Time.Format(time.RFC3339)
		}
		if startedAt.Valid {
			c["started_at"] = startedAt.Time.Format(time.RFC3339)
		}
		campaigns = append(campaigns, c)
	}
	if campaigns == nil {
		campaigns = []map[string]interface{}{}
	}
	return map[string]interface{}{"campaigns": campaigns, "count": len(campaigns)}
}

func (c *CampaignCopilot) toolGetCampaignDetails(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	cID, _ := args["campaign_id"].(string)
	if cID == "" {
		return map[string]string{"error": "campaign_id required"}
	}

	var id, name, status, fromEmail string
	var sent, opens, clicks, bounces, complaints int
	var pmtaConfig sql.NullString
	var scheduledAt, createdAt sql.NullTime

	q := `SELECT id, name, status, COALESCE(from_email,''), sent_count,
	             COALESCE(open_count,0), COALESCE(click_count,0),
	             COALESCE(bounce_count,0), COALESCE(complaint_count,0),
	             pmta_config::text, scheduled_at, created_at
	      FROM mailing_campaigns WHERE id::text LIKE $1 AND organization_id = $2`
	pattern := cID + "%"
	err := c.db.QueryRowContext(ctx, q, pattern, orgID).Scan(
		&id, &name, &status, &fromEmail, &sent,
		&opens, &clicks, &bounces, &complaints,
		&pmtaConfig, &scheduledAt, &createdAt)
	if err != nil {
		return map[string]string{"error": "campaign not found: " + err.Error()}
	}

	result := map[string]interface{}{
		"id": id, "name": name, "status": status, "from_email": fromEmail,
		"sent_count": sent, "open_count": opens, "click_count": clicks,
		"bounce_count": bounces, "complaint_count": complaints,
		"created_at": createdAt.Time.Format(time.RFC3339),
	}
	if scheduledAt.Valid {
		result["scheduled_at"] = scheduledAt.Time.Format(time.RFC3339)
	}
	if pmtaConfig.Valid {
		var cfg map[string]interface{}
		if json.Unmarshal([]byte(pmtaConfig.String), &cfg) == nil {
			if ci, ok := cfg["campaign_input"].(map[string]interface{}); ok {
				// Strip HTML to keep token usage down
				if variants, ok := ci["variants"].([]interface{}); ok {
					for _, v := range variants {
						if vm, ok := v.(map[string]interface{}); ok {
							if html, ok := vm["html_content"].(string); ok && len(html) > 200 {
								vm["html_content"] = html[:200] + "... [truncated, " + fmt.Sprintf("%d", len(html)) + " chars total]"
							}
						}
					}
				}
				result["campaign_input"] = ci
			}
		}
	}
	return result
}

func (c *CampaignCopilot) toolSearchCampaigns(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	query, _ := args["query"].(string)
	if query == "" {
		return map[string]string{"error": "query required"}
	}

	rows, err := c.db.QueryContext(ctx,
		`SELECT id, name, status, COALESCE(from_email,''), sent_count, scheduled_at, created_at
		 FROM mailing_campaigns
		 WHERE organization_id = $1 AND LOWER(name) LIKE $2
		 ORDER BY created_at DESC LIMIT 20`,
		orgID, "%"+strings.ToLower(query)+"%")
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var id, name, status, fromEmail string
		var sent int
		var scheduledAt, createdAt sql.NullTime
		if rows.Scan(&id, &name, &status, &fromEmail, &sent, &scheduledAt, &createdAt) != nil {
			continue
		}
		r := map[string]interface{}{"id": id, "name": name, "status": status, "from_email": fromEmail, "sent_count": sent}
		if scheduledAt.Valid {
			r["scheduled_at"] = scheduledAt.Time.Format(time.RFC3339)
		}
		results = append(results, r)
	}
	if results == nil {
		results = []map[string]interface{}{}
	}
	return map[string]interface{}{"campaigns": results, "count": len(results)}
}

func (c *CampaignCopilot) toolListLists(ctx context.Context, orgID string) interface{} {
	rows, err := c.db.QueryContext(ctx,
		`SELECT l.id, l.name, l.status, l.subscriber_count,
		        (SELECT COUNT(DISTINCT e.subscriber_id) FROM mailing_tracking_events e
		         JOIN mailing_subscribers s2 ON s2.id = e.subscriber_id
		         WHERE s2.list_id = l.id AND e.event_type = 'sent') as mailed_to
		 FROM mailing_lists l WHERE l.organization_id = $1 AND l.status = 'active'
		 ORDER BY l.subscriber_count DESC`, orgID)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()

	var lists []map[string]interface{}
	for rows.Next() {
		var id, name, status string
		var subCount, mailedTo int
		if rows.Scan(&id, &name, &status, &subCount, &mailedTo) != nil {
			continue
		}
		lists = append(lists, map[string]interface{}{
			"id": id, "name": name, "subscriber_count": subCount, "mailed_to": mailedTo,
		})
	}
	if lists == nil {
		lists = []map[string]interface{}{}
	}
	return map[string]interface{}{"lists": lists, "count": len(lists)}
}

func (c *CampaignCopilot) toolListSegments(ctx context.Context, orgID string) interface{} {
	rows, err := c.db.QueryContext(ctx,
		`SELECT id, name, COALESCE(description,''), segment_type, subscriber_count, status
		 FROM mailing_segments WHERE organization_id = $1 AND status = 'active'
		 ORDER BY name`, orgID)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()

	var segs []map[string]interface{}
	for rows.Next() {
		var id, name, desc, segType, status string
		var count int
		if rows.Scan(&id, &name, &desc, &segType, &count, &status) != nil {
			continue
		}
		segs = append(segs, map[string]interface{}{
			"id": id, "name": name, "description": desc,
			"type": segType, "subscriber_count": count,
		})
	}
	if segs == nil {
		segs = []map[string]interface{}{}
	}
	return map[string]interface{}{"segments": segs, "count": len(segs)}
}

func (c *CampaignCopilot) toolListTemplates(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	search, _ := args["search"].(string)
	folderID, _ := args["folder_id"].(string)

	q := `SELECT t.id, t.name, COALESCE(t.subject,''), COALESCE(t.from_name,''),
	             COALESCE(t.preview_text,''), COALESCE(f.name,'Uncategorized') as folder_name
	      FROM mailing_templates t
	      LEFT JOIN mailing_template_folders f ON f.id = t.folder_id
	      WHERE t.organization_id = $1`
	qArgs := []interface{}{orgID}
	argN := 2

	if folderID != "" {
		q += fmt.Sprintf(" AND t.folder_id = $%d", argN)
		qArgs = append(qArgs, folderID)
		argN++
	}
	if search != "" {
		q += fmt.Sprintf(" AND LOWER(t.name) LIKE $%d", argN)
		qArgs = append(qArgs, "%"+strings.ToLower(search)+"%")
		argN++
	}
	q += " ORDER BY f.name, t.name LIMIT 50"

	rows, err := c.db.QueryContext(ctx, q, qArgs...)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()

	var templates []map[string]interface{}
	for rows.Next() {
		var id, name, subject, fromName, preview, folder string
		if rows.Scan(&id, &name, &subject, &fromName, &preview, &folder) != nil {
			continue
		}
		templates = append(templates, map[string]interface{}{
			"id": id, "name": name, "subject": subject,
			"from_name": fromName, "preview_text": preview, "folder": folder,
		})
	}
	if templates == nil {
		templates = []map[string]interface{}{}
	}
	return map[string]interface{}{"templates": templates, "count": len(templates)}
}

func (c *CampaignCopilot) toolGetTemplate(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	tID, _ := args["template_id"].(string)
	if tID == "" {
		return map[string]string{"error": "template_id required"}
	}

	var id, name string
	var subject, fromName, fromEmail, preview, html sql.NullString
	err := c.db.QueryRowContext(ctx,
		`SELECT id, name, subject, from_name, from_email, preview_text, html_content
		 FROM mailing_templates WHERE id = $1 AND organization_id = $2`, tID, orgID).
		Scan(&id, &name, &subject, &fromName, &fromEmail, &preview, &html)
	if err != nil {
		return map[string]string{"error": "template not found"}
	}

	htmlStr := html.String
	if len(htmlStr) > 500 {
		htmlStr = htmlStr[:500] + fmt.Sprintf("... [truncated, %d chars total]", len(html.String))
	}
	return map[string]interface{}{
		"id": id, "name": name, "subject": subject.String,
		"from_name": fromName.String, "from_email": fromEmail.String,
		"preview_text": preview.String, "html_content_preview": htmlStr,
		"html_content_length": len(html.String),
	}
}

func (c *CampaignCopilot) toolGetISPPerformance(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	rangeType, _ := args["range_type"].(string)
	if rangeType == "" {
		rangeType = "7"
	}
	isp, _ := args["isp"].(string)

	days := 7
	switch rangeType {
	case "24h":
		days = 1
	case "14":
		days = 14
	case "30":
		days = 30
	case "90":
		days = 90
	}

	q := `SELECT recipient_domain, event_type, COUNT(*)
	      FROM mailing_tracking_events
	      WHERE organization_id = $1 AND event_at >= NOW() - $2::interval
	      GROUP BY recipient_domain, event_type`
	interval := fmt.Sprintf("%d days", days)

	rows, err := c.db.QueryContext(ctx, q, orgID, interval)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()

	ispMap := map[string]string{
		"gmail.com": "gmail", "googlemail.com": "gmail",
		"yahoo.com": "yahoo", "ymail.com": "yahoo", "aol.com": "yahoo",
		"outlook.com": "microsoft", "hotmail.com": "microsoft", "live.com": "microsoft",
		"icloud.com": "apple", "me.com": "apple", "mac.com": "apple",
		"comcast.net": "comcast", "att.net": "att", "sbcglobal.net": "att",
		"cox.net": "cox", "charter.net": "charter", "spectrum.net": "charter",
	}

	type ispMetrics struct {
		Sent, Delivered, Opens, Clicks, Bounces, Complaints int
	}
	data := map[string]*ispMetrics{}

	for rows.Next() {
		var domain, eventType string
		var cnt int
		if rows.Scan(&domain, &eventType, &cnt) != nil {
			continue
		}
		ispName := ispMap[strings.ToLower(domain)]
		if ispName == "" {
			ispName = "other"
		}
		if isp != "" && ispName != isp {
			continue
		}
		if data[ispName] == nil {
			data[ispName] = &ispMetrics{}
		}
		switch eventType {
		case "sent":
			data[ispName].Sent += cnt
		case "delivered":
			data[ispName].Delivered += cnt
		case "opened":
			data[ispName].Opens += cnt
		case "clicked":
			data[ispName].Clicks += cnt
		case "bounced", "hard_bounce", "soft_bounce":
			data[ispName].Bounces += cnt
		case "complained":
			data[ispName].Complaints += cnt
		}
	}

	var isps []map[string]interface{}
	for name, m := range data {
		entry := map[string]interface{}{
			"isp": name, "sent": m.Sent, "delivered": m.Delivered,
			"opens": m.Opens, "clicks": m.Clicks,
			"bounces": m.Bounces, "complaints": m.Complaints,
		}
		if m.Sent > 0 {
			entry["open_rate"] = fmt.Sprintf("%.1f%%", float64(m.Opens)/float64(m.Sent)*100)
			entry["click_rate"] = fmt.Sprintf("%.1f%%", float64(m.Clicks)/float64(m.Sent)*100)
			entry["bounce_rate"] = fmt.Sprintf("%.1f%%", float64(m.Bounces)/float64(m.Sent)*100)
			entry["complaint_rate"] = fmt.Sprintf("%.3f%%", float64(m.Complaints)/float64(m.Sent)*100)
		}
		isps = append(isps, entry)
	}
	return map[string]interface{}{"isps": isps, "range_days": days}
}

func (c *CampaignCopilot) toolGetSendingInsights(ctx context.Context, orgID string) interface{} {
	rows, err := c.db.QueryContext(ctx,
		`SELECT recipient_domain, event_type, COUNT(*), DATE(event_at) as day
		 FROM mailing_tracking_events
		 WHERE organization_id = $1 AND event_at >= NOW() - INTERVAL '3 days'
		 GROUP BY recipient_domain, event_type, DATE(event_at)
		 ORDER BY day`, orgID)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()

	type dayMetrics struct {
		Sent, Delivered, Bounces, HardBounces, SoftBounces, Deferred, Complaints, Opens int
	}
	ispDaily := map[string]map[string]*dayMetrics{}

	ispMap := map[string]string{
		"gmail.com": "gmail", "googlemail.com": "gmail",
		"yahoo.com": "yahoo", "ymail.com": "yahoo",
		"outlook.com": "microsoft", "hotmail.com": "microsoft",
		"icloud.com": "apple", "comcast.net": "comcast",
		"att.net": "att", "cox.net": "cox", "charter.net": "charter",
	}

	for rows.Next() {
		var domain, eventType string
		var cnt int
		var day time.Time
		if rows.Scan(&domain, &eventType, &cnt, &day) != nil {
			continue
		}
		ispName := ispMap[strings.ToLower(domain)]
		if ispName == "" {
			continue
		}
		dayStr := day.Format("2006-01-02")
		if ispDaily[ispName] == nil {
			ispDaily[ispName] = map[string]*dayMetrics{}
		}
		if ispDaily[ispName][dayStr] == nil {
			ispDaily[ispName][dayStr] = &dayMetrics{}
		}
		m := ispDaily[ispName][dayStr]
		switch eventType {
		case "sent":
			m.Sent += cnt
		case "delivered":
			m.Delivered += cnt
		case "bounced":
			m.Bounces += cnt
		case "hard_bounce":
			m.HardBounces += cnt
		case "soft_bounce":
			m.SoftBounces += cnt
		case "deferred":
			m.Deferred += cnt
		case "complained":
			m.Complaints += cnt
		case "opened":
			m.Opens += cnt
		}
	}

	var isps []map[string]interface{}
	for ispName, days := range ispDaily {
		var totalSent, totalBounces, totalHard, totalSoft, totalDeferred, totalComplaints int
		var daily []map[string]interface{}
		for day, m := range days {
			totalSent += m.Sent
			totalBounces += m.Bounces
			totalHard += m.HardBounces
			totalSoft += m.SoftBounces
			totalDeferred += m.Deferred
			totalComplaints += m.Complaints
			daily = append(daily, map[string]interface{}{
				"date": day, "sent": m.Sent, "bounces": m.Bounces,
				"hard_bounces": m.HardBounces, "soft_bounces": m.SoftBounces,
				"deferred": m.Deferred, "complaints": m.Complaints, "opens": m.Opens,
			})
		}
		entry := map[string]interface{}{
			"isp": ispName, "total_sent": totalSent,
			"total_bounces": totalBounces, "hard_bounces": totalHard, "soft_bounces": totalSoft,
			"total_deferred": totalDeferred, "total_complaints": totalComplaints,
			"daily": daily,
		}
		if totalSent > 0 {
			entry["bounce_rate"] = fmt.Sprintf("%.2f%%", float64(totalBounces)/float64(totalSent)*100)
			entry["complaint_rate"] = fmt.Sprintf("%.3f%%", float64(totalComplaints)/float64(totalSent)*100)
		}
		isps = append(isps, entry)
	}
	return map[string]interface{}{"isps": isps, "window": "3 days"}
}

func (c *CampaignCopilot) toolGetLastQuotas(ctx context.Context, orgID string) interface{} {
	var configJSON sql.NullString
	var campaignName string
	var createdAt time.Time
	err := c.db.QueryRowContext(ctx,
		`SELECT name, pmta_config::text, created_at FROM mailing_campaigns
		 WHERE organization_id = $1
		   AND status IN ('completed','sent','cancelled','completed_with_errors')
		   AND COALESCE(sent_count,0) > 0 AND pmta_config IS NOT NULL
		 ORDER BY created_at DESC LIMIT 1`, orgID).Scan(&campaignName, &configJSON, &createdAt)
	if err != nil {
		return map[string]interface{}{"quotas": nil, "message": "no previous campaigns found"}
	}

	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(configJSON.String), &cfg); err != nil {
		return map[string]string{"error": "invalid config"}
	}
	ci, _ := cfg["campaign_input"].(map[string]interface{})
	quotas, _ := ci["isp_quotas"].([]interface{})

	return map[string]interface{}{
		"quotas":          quotas,
		"source_campaign": campaignName,
		"source_date":     createdAt.Format(time.RFC3339),
	}
}

func (c *CampaignCopilot) toolGetSendingDomains(ctx context.Context, orgID string) interface{} {
	rows, err := c.db.QueryContext(ctx,
		`SELECT id, COALESCE(name,''), COALESCE(sending_domain,''), COALESCE(from_email,''),
		        COALESCE(from_name,''), COALESCE(vendor_type,'')
		 FROM mailing_sending_profiles WHERE organization_id = $1 AND status = 'active'
		 ORDER BY name`, orgID)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()

	var domains []map[string]interface{}
	for rows.Next() {
		var id, name, domain, fromEmail, fromName, vendor string
		if rows.Scan(&id, &name, &domain, &fromEmail, &fromName, &vendor) != nil {
			continue
		}
		domains = append(domains, map[string]interface{}{
			"profile_id": id, "name": name, "sending_domain": domain,
			"from_email": fromEmail, "from_name": fromName, "vendor": vendor,
		})
	}
	if domains == nil {
		domains = []map[string]interface{}{}
	}
	return map[string]interface{}{"domains": domains}
}

func (c *CampaignCopilot) toolEstimateAudience(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	listIDs := extractStringArray(args, "inclusion_lists")
	if len(listIDs) == 0 {
		return map[string]string{"error": "inclusion_lists required"}
	}

	allISPs := []string{"gmail", "yahoo", "microsoft", "apple", "comcast", "att", "cox", "charter"}
	targetISPs := extractStringArray(args, "target_isps")
	if len(targetISPs) == 0 {
		targetISPs = allISPs
	}

	placeholders := make([]string, len(listIDs))
	qArgs := []interface{}{orgID}
	for i, id := range listIDs {
		qArgs = append(qArgs, id)
		placeholders[i] = fmt.Sprintf("$%d", i+2)
	}

	var total int
	err := c.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_subscribers
		 WHERE organization_id = $1 AND list_id IN (`+strings.Join(placeholders, ",")+`) AND status = 'confirmed'`,
		qArgs...).Scan(&total)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}

	return map[string]interface{}{
		"total_confirmed":  total,
		"inclusion_lists":  listIDs,
		"target_isps":      targetISPs,
		"note":             "Per-ISP breakdown requires domain analysis of subscribers.",
	}
}

// ── Write Tools ─────────────────────────────────────────────────────────

func (c *CampaignCopilot) toolCloneCampaign(ctx context.Context, orgID string, args map[string]interface{}) (interface{}, string) {
	confirmed, _ := args["confirmed"].(bool)
	sourceID, _ := args["source_campaign_id"].(string)
	if sourceID == "" {
		return map[string]string{"error": "source_campaign_id required"}, ""
	}

	// Load source config
	var configJSON sql.NullString
	var sourceName string
	err := c.db.QueryRowContext(ctx,
		`SELECT name, pmta_config::text FROM mailing_campaigns
		 WHERE id::text LIKE $1 AND organization_id = $2`,
		sourceID+"%", orgID).Scan(&sourceName, &configJSON)
	if err != nil || !configJSON.Valid {
		return map[string]string{"error": "source campaign not found or has no config"}, ""
	}

	var wrapper map[string]interface{}
	json.Unmarshal([]byte(configJSON.String), &wrapper)
	ci, _ := wrapper["campaign_input"].(map[string]interface{})
	if ci == nil {
		return map[string]string{"error": "invalid campaign config"}, ""
	}

	// Apply overrides
	if nameOverride, ok := args["name_override"].(string); ok && nameOverride != "" {
		ci["name"] = nameOverride
	} else {
		ci["name"] = sourceName + " (Clone)"
	}

	if scheduledAt, ok := args["scheduled_at_utc"].(string); ok && scheduledAt != "" {
		t, err := time.Parse(time.RFC3339, scheduledAt)
		if err != nil {
			return map[string]string{"error": "invalid scheduled_at_utc: " + err.Error()}, ""
		}
		// Calculate duration from existing span
		dur := 8 * time.Hour
		endAt := t.Add(dur)
		if plans, ok := ci["isp_plans"].([]interface{}); ok {
			for _, p := range plans {
				if pm, ok := p.(map[string]interface{}); ok {
					if spans, ok := pm["time_spans"].([]interface{}); ok {
						for _, s := range spans {
							if sm, ok := s.(map[string]interface{}); ok {
								sm["start_at"] = t.Format(time.RFC3339)
								sm["end_at"] = endAt.Format(time.RFC3339)
							}
						}
					}
				}
			}
		}
	}

	if exclSegs := extractStringArray(args, "exclusion_segments"); len(exclSegs) > 0 {
		existing, _ := ci["exclusion_segments"].([]interface{})
		for _, s := range exclSegs {
			existing = append(existing, s)
		}
		ci["exclusion_segments"] = existing
	}

	if exclLists := extractStringArray(args, "additional_exclusion_lists"); len(exclLists) > 0 {
		existing, _ := ci["exclusion_lists"].([]interface{})
		for _, l := range exclLists {
			existing = append(existing, l)
		}
		ci["exclusion_lists"] = existing
	}

	delete(ci, "campaign_id")

	if !confirmed {
		summary := map[string]interface{}{
			"status":              "preview",
			"message":             "Campaign clone ready. Say 'confirm' or 'yes' to deploy.",
			"source_campaign":     sourceName,
			"cloned_name":         ci["name"],
			"sending_domain":      ci["sending_domain"],
			"target_isps":         ci["target_isps"],
			"exclusion_segments":  ci["exclusion_segments"],
			"exclusion_lists":     ci["exclusion_lists"],
			"isp_quotas":          ci["isp_quotas"],
		}
		if plans, ok := ci["isp_plans"].([]interface{}); ok && len(plans) > 0 {
			if pm, ok := plans[0].(map[string]interface{}); ok {
				if spans, ok := pm["time_spans"].([]interface{}); ok && len(spans) > 0 {
					if sm, ok := spans[0].(map[string]interface{}); ok {
						summary["scheduled_start"] = sm["start_at"]
						summary["scheduled_end"] = sm["end_at"]
					}
				}
			}
		}
		return summary, ""
	}

	// Deploy
	inputBytes, _ := json.Marshal(ci)
	var input engine.PMTACampaignInput
	if err := json.Unmarshal(inputBytes, &input); err != nil {
		return map[string]string{"error": "failed to build input: " + err.Error()}, ""
	}

	normalized, err := normalizePMTACampaignInput(input)
	if err != nil {
		return map[string]string{"error": "normalization error: " + err.Error()}, ""
	}

	audience, err := planPMTAAudience(ctx, c.db, orgID, input, normalized, c.pmtaSvc.suppMatcher)
	if err != nil {
		return map[string]string{"error": "audience error: " + err.Error()}, ""
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return map[string]string{"error": err.Error()}, ""
	}
	defer tx.Rollback()

	result, err := createPMTAWaveCampaign(ctx, tx, c.db, orgID, input, normalized, audience, c.pmtaSvc.colCache)
	if err != nil {
		return map[string]string{"error": "deploy error: " + err.Error()}, ""
	}
	if err := tx.Commit(); err != nil {
		return map[string]string{"error": err.Error()}, ""
	}

	return map[string]interface{}{
		"status":          "deployed",
		"campaign_id":     result.CampaignID,
		"name":            result.Name,
		"campaign_status": result.Status,
		"send_mode":       result.SendMode,
		"sends_at":        result.SendsAt,
		"total_audience":  result.TotalAudience,
	}, fmt.Sprintf("Deployed campaign '%s' (ID: %s)", result.Name, result.CampaignID)
}

func (c *CampaignCopilot) toolDeployCampaign(ctx context.Context, orgID string, args map[string]interface{}) (interface{}, string) {
	confirmed, _ := args["confirmed"].(bool)

	name, _ := args["name"].(string)
	domain, _ := args["sending_domain"].(string)
	sendMode, _ := args["send_mode"].(string)
	if sendMode == "" {
		sendMode = "immediate"
	}
	tz, _ := args["timezone"].(string)
	if tz == "" {
		tz = "America/Boise"
	}

	if !confirmed {
		return map[string]interface{}{
			"status":           "preview",
			"message":          "Campaign ready to deploy. Say 'confirm' or 'yes' to proceed.",
			"name":             name,
			"sending_domain":   domain,
			"send_mode":        sendMode,
			"target_isps":      args["target_isps"],
			"isp_quotas":       args["isp_quotas"],
			"inclusion_lists":  args["inclusion_lists"],
			"exclusion_lists":  args["exclusion_lists"],
			"exclusion_segments": args["exclusion_segments"],
		}, ""
	}

	inputBytes, _ := json.Marshal(args)
	var input engine.PMTACampaignInput
	if err := json.Unmarshal(inputBytes, &input); err != nil {
		return map[string]string{"error": "invalid input: " + err.Error()}, ""
	}
	input.Timezone = tz
	if input.SendMode == "" {
		input.SendMode = sendMode
	}

	if scheduledStr, ok := args["scheduled_at_utc"].(string); ok && scheduledStr != "" {
		t, err := time.Parse(time.RFC3339, scheduledStr)
		if err != nil {
			return map[string]string{"error": "invalid scheduled_at_utc"}, ""
		}
		input.ScheduledAt = &t
		input.SendMode = "scheduled"
	}

	normalized, err := normalizePMTACampaignInput(input)
	if err != nil {
		return map[string]string{"error": err.Error()}, ""
	}

	audience, err := planPMTAAudience(ctx, c.db, orgID, input, normalized, c.pmtaSvc.suppMatcher)
	if err != nil {
		return map[string]string{"error": err.Error()}, ""
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return map[string]string{"error": err.Error()}, ""
	}
	defer tx.Rollback()

	result, err := createPMTAWaveCampaign(ctx, tx, c.db, orgID, input, normalized, audience, c.pmtaSvc.colCache)
	if err != nil {
		return map[string]string{"error": err.Error()}, ""
	}
	if err := tx.Commit(); err != nil {
		return map[string]string{"error": err.Error()}, ""
	}

	return map[string]interface{}{
		"status":          "deployed",
		"campaign_id":     result.CampaignID,
		"name":            result.Name,
		"campaign_status": result.Status,
		"total_audience":  result.TotalAudience,
	}, fmt.Sprintf("Deployed campaign '%s' (ID: %s)", result.Name, result.CampaignID)
}

func (c *CampaignCopilot) toolSaveDraft(ctx context.Context, orgID string, args map[string]interface{}) (interface{}, string) {
	inputBytes, _ := json.Marshal(args)
	var input engine.PMTACampaignInput
	json.Unmarshal(inputBytes, &input)

	if input.Timezone == "" {
		input.Timezone = "America/Boise"
	}

	draftInput := engine.PMTACampaignDraftInput{
		CampaignInput: input,
		ScheduleMode:  "per-isp",
	}
	draftJSON, _ := json.Marshal(draftInput)

	var draftID string
	err := c.db.QueryRowContext(ctx,
		`INSERT INTO mailing_campaigns (id, organization_id, name, status, execution_mode, send_type, pmta_config, created_at, updated_at)
		 VALUES (gen_random_uuid(), $1, $2, 'draft', 'pmta_isp_wave', 'blast', $3, NOW(), NOW())
		 ON CONFLICT (organization_id, status) WHERE status = 'draft'
		 DO UPDATE SET name = EXCLUDED.name, pmta_config = EXCLUDED.pmta_config, updated_at = NOW()
		 RETURNING id`,
		orgID, input.Name, string(draftJSON)).Scan(&draftID)
	if err != nil {
		return map[string]string{"error": err.Error()}, ""
	}

	return map[string]interface{}{
		"status":      "draft_saved",
		"campaign_id": draftID,
		"name":        input.Name,
		"message":     "Draft saved. Open Campaign Manager to review and deploy.",
	}, fmt.Sprintf("Saved draft '%s'", input.Name)
}

func (c *CampaignCopilot) toolCreateSegment(ctx context.Context, orgID string, args map[string]interface{}) (interface{}, string) {
	confirmed, _ := args["confirmed"].(bool)
	name, _ := args["name"].(string)
	desc, _ := args["description"].(string)
	if name == "" {
		return map[string]string{"error": "name required"}, ""
	}

	conditionsRaw, _ := args["conditions"]
	condBytes, _ := json.Marshal(conditionsRaw)
	var rootGroup segmentation.ConditionGroupBuilder
	if err := json.Unmarshal(condBytes, &rootGroup); err != nil {
		return map[string]string{"error": "invalid conditions: " + err.Error()}, ""
	}

	if !confirmed {
		return map[string]interface{}{
			"status":     "preview",
			"message":    "Segment ready to create. Say 'confirm' or 'yes' to proceed.",
			"name":       name,
			"description": desc,
			"conditions": conditionsRaw,
		}, ""
	}

	parsedOrgID, _ := uuid.Parse(orgID)
	segment := &segmentation.Segment{
		OrganizationID: parsedOrgID,
		Name:           name,
		Description:    desc,
	}

	if err := c.segAPI.engine.Store().CreateSegment(ctx, segment, &rootGroup); err != nil {
		return map[string]string{"error": err.Error()}, ""
	}

	// Count
	qb := c.segAPI.engine.NewQueryBuilder(ctx)
	qb.SetOrganizationID(orgID)
	cq, cqArgs, err := qb.BuildCountQuery(rootGroup, nil)
	if err == nil {
		var count int
		if c.db.QueryRowContext(ctx, cq, cqArgs...).Scan(&count) == nil {
			c.segAPI.engine.Store().UpdateSegmentCount(ctx, segment.ID, count)
			segment.SubscriberCount = count
		}
	}

	return map[string]interface{}{
		"status":           "created",
		"segment_id":       segment.ID.String(),
		"name":             name,
		"subscriber_count": segment.SubscriberCount,
	}, fmt.Sprintf("Created segment '%s' (%d subscribers)", name, segment.SubscriberCount)
}

func (c *CampaignCopilot) toolEmergencyStop(ctx context.Context, orgID string, args map[string]interface{}) (interface{}, string) {
	confirmed, _ := args["confirmed"].(bool)
	campaignID, _ := args["campaign_id"].(string)
	if campaignID == "" {
		return map[string]string{"error": "campaign_id required"}, ""
	}

	var name, status string
	err := c.db.QueryRowContext(ctx,
		`SELECT name, status FROM mailing_campaigns WHERE id::text LIKE $1 AND organization_id = $2`,
		campaignID+"%", orgID).Scan(&name, &status)
	if err != nil {
		return map[string]string{"error": "campaign not found"}, ""
	}

	if !confirmed {
		return map[string]interface{}{
			"status":          "preview",
			"message":         "WARNING: This will immediately stop the campaign and cancel all pending sends. Say 'confirm' to proceed.",
			"campaign_id":     campaignID,
			"campaign_name":   name,
			"current_status":  status,
		}, ""
	}

	c.db.ExecContext(ctx,
		`UPDATE mailing_campaigns SET status='cancelled', completed_at=NOW(), updated_at=NOW() WHERE id::text LIKE $1`,
		campaignID+"%")

	result, _ := c.db.ExecContext(ctx,
		`UPDATE mailing_campaign_queue SET status='skipped', error_message='emergency stop via copilot'
		 WHERE campaign_id::text LIKE $1 AND status IN ('queued','sending','claimed','pending')`,
		campaignID+"%")
	cancelled, _ := result.RowsAffected()

	return map[string]interface{}{
		"status":         "stopped",
		"campaign_name":  name,
		"items_cancelled": cancelled,
	}, fmt.Sprintf("Emergency stopped campaign '%s', cancelled %d queue items", name, cancelled)
}

// ── Helpers ─────────────────────────────────────────────────────────────

func extractStringArray(args map[string]interface{}, key string) []string {
	raw, ok := args[key]
	if !ok {
		return nil
	}
	arr, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	result := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			result = append(result, s)
		}
	}
	return result
}
