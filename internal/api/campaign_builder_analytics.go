package api

import (
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// HandleSendTestCampaign sends a test email for the campaign
func (cb *CampaignBuilder) HandleSendTestCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	
	var input struct {
		TestEmails []string `json:"test_emails"`
	}
	json.NewDecoder(r.Body).Decode(&input)
	
	if len(input.TestEmails) == 0 {
		http.Error(w, `{"error":"test_emails required"}`, http.StatusBadRequest)
		return
	}
	
	// Get campaign
	var subject, fromName, fromEmail, htmlContent, profileID string
	err := cb.db.QueryRowContext(ctx, `
		SELECT subject, from_name, from_email, COALESCE(html_content, ''), COALESCE(sending_profile_id::text, '')
		FROM mailing_campaigns WHERE id = $1
	`, id).Scan(&subject, &fromName, &fromEmail, &htmlContent, &profileID)
	
	if err != nil {
		http.Error(w, `{"error":"campaign not found"}`, http.StatusNotFound)
		return
	}
	
	results := []map[string]interface{}{}
	for _, email := range input.TestEmails {
		// Use the profile's vendor type to route correctly
		var vendorType, smtpHost, smtpUser, smtpPass string
		var smtpPort int
		if profileID != "" {
			cb.db.QueryRowContext(ctx, `
				SELECT COALESCE(vendor_type,''), COALESCE(smtp_host,''), COALESCE(smtp_port,25),
				       COALESCE(smtp_username,''), COALESCE(smtp_password,'')
				FROM mailing_sending_profiles WHERE id = $1
			`, profileID).Scan(&vendorType, &smtpHost, &smtpPort, &smtpUser, &smtpPass)
		}

		var result map[string]interface{}
		var sendErr error
		switch vendorType {
		case "pmta", "smtp":
			result, sendErr = cb.mailingSvc.sendViaSMTP(ctx, smtpHost, smtpPort, smtpUser, smtpPass, email, fromEmail, fromName, "", "[TEST] "+subject, htmlContent, "")
		default:
			result, sendErr = cb.mailingSvc.sendViaSparkPost(ctx, email, fromEmail, fromName, "[TEST] "+subject, htmlContent, "")
		}
		if sendErr != nil {
			result = map[string]interface{}{"success": false, "error": sendErr.Error()}
		}
		result["email"] = email
		results = append(results, result)
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"campaign_id": id,
		"results":     results,
	})
}

// HandlePreviewCampaign returns HTML preview
func (cb *CampaignBuilder) HandlePreviewCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	
	var htmlContent string
	err := cb.db.QueryRowContext(ctx, `SELECT COALESCE(html_content, '') FROM mailing_campaigns WHERE id = $1`, id).Scan(&htmlContent)
	if err != nil {
		http.Error(w, `{"error":"campaign not found"}`, http.StatusNotFound)
		return
	}
	
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(htmlContent))
}

// HandleEstimateAudience returns audience size estimate
func (cb *CampaignBuilder) HandleEstimateAudience(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	
	var listID, segmentID sql.NullString
	cb.db.QueryRowContext(ctx, `SELECT list_id, segment_id FROM mailing_campaigns WHERE id = $1`, id).Scan(&listID, &segmentID)
	
	var lid, sid *string
	if listID.Valid {
		lid = &listID.String
	}
	if segmentID.Valid {
		sid = &segmentID.String
	}
	
	total := cb.getAudienceCount(ctx, lid, sid)
	
	// Get suppression count
	var suppressedCount int
	cb.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailing_suppressions WHERE active = true`).Scan(&suppressedCount)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total_subscribers":   total,
		"estimated_delivered": total - (total * suppressedCount / max(1, total)),
		"suppression_rate":    calcRate(suppressedCount, total),
	})
}

// HandleCampaignStats returns campaign statistics with ISP breakdown and timeline.
func (cb *CampaignBuilder) HandleCampaignStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	var sent, delivered, opens, clicks, bounces, complaints, unsubscribes int
	cb.db.QueryRowContext(ctx, `
		SELECT COALESCE(sent_count,0), COALESCE(delivered_count,0),
		       COALESCE(open_count,0), COALESCE(click_count,0),
		       COALESCE(bounce_count,0), COALESCE(complaint_count,0), COALESCE(unsubscribe_count,0)
		FROM mailing_campaigns WHERE id = $1
	`, id).Scan(&sent, &delivered, &opens, &clicks, &bounces, &complaints, &unsubscribes)

	// Hard/soft bounce split from tracking events (resilient to missing columns)
	var hardBounces, softBounces int
	cb.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN event_type = 'hard_bounce' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN event_type = 'soft_bounce' THEN 1 ELSE 0 END), 0)
		FROM mailing_tracking_events WHERE campaign_id = $1
	`, id).Scan(&hardBounces, &softBounces)

	// ISP/domain breakdown via subscriber JOIN (email column may not exist on tracking_events)
	domainRows, _ := cb.db.QueryContext(ctx, `
		SELECT COALESCE(SPLIT_PART(s.email, '@', 2), 'unknown') as domain,
		       SUM(CASE WHEN t.event_type = 'sent' THEN 1 ELSE 0 END) as sent,
		       SUM(CASE WHEN t.event_type = 'delivered' THEN 1 ELSE 0 END) as delivered,
		       SUM(CASE WHEN t.event_type = 'opened' THEN 1 ELSE 0 END) as opens,
		       SUM(CASE WHEN t.event_type = 'clicked' THEN 1 ELSE 0 END) as clicks,
		       SUM(CASE WHEN t.event_type = 'hard_bounce' THEN 1 ELSE 0 END) as hard_bounces,
		       SUM(CASE WHEN t.event_type = 'soft_bounce' THEN 1 ELSE 0 END) as soft_bounces,
		       SUM(CASE WHEN t.event_type = 'complained' THEN 1 ELSE 0 END) as complaints
		FROM mailing_tracking_events t
		JOIN mailing_subscribers s ON s.id = t.subscriber_id
		WHERE t.campaign_id = $1 AND s.email IS NOT NULL AND s.email != ''
		GROUP BY SPLIT_PART(s.email, '@', 2)
		ORDER BY sent DESC
		LIMIT 50
	`, id)
	var domainBreakdown []map[string]interface{}
	if domainRows != nil {
		defer domainRows.Close()
		for domainRows.Next() {
			var domain string
			var ds, dd, do, dc, dhb, dsb, dcomp int
			if err := domainRows.Scan(&domain, &ds, &dd, &do, &dc, &dhb, &dsb, &dcomp); err != nil {
				continue
			}
			oRate, cRate := 0.0, 0.0
			if dd > 0 {
				oRate = float64(do) / float64(dd) * 100
				cRate = float64(dc) / float64(dd) * 100
			}
			domainBreakdown = append(domainBreakdown, map[string]interface{}{
				"domain": domain, "sent": ds, "delivered": dd,
				"opens": do, "clicks": dc,
				"hard_bounces": dhb, "soft_bounces": dsb, "complaints": dcomp,
				"open_rate": math.Round(oRate*100) / 100, "click_rate": math.Round(cRate*100) / 100,
			})
		}
	}
	if domainBreakdown == nil {
		domainBreakdown = []map[string]interface{}{}
	}

	// Hourly timeline
	timeRows, _ := cb.db.QueryContext(ctx, `
		SELECT DATE_TRUNC('hour', event_at) as hour,
		       SUM(CASE WHEN event_type = 'sent' THEN 1 ELSE 0 END) as sent,
		       SUM(CASE WHEN event_type = 'delivered' THEN 1 ELSE 0 END) as delivered,
		       SUM(CASE WHEN event_type = 'opened' THEN 1 ELSE 0 END) as opens,
		       SUM(CASE WHEN event_type = 'clicked' THEN 1 ELSE 0 END) as clicks,
		       SUM(CASE WHEN event_type IN ('hard_bounce','soft_bounce') THEN 1 ELSE 0 END) as bounces
		FROM mailing_tracking_events
		WHERE campaign_id = $1
		GROUP BY DATE_TRUNC('hour', event_at)
		ORDER BY hour
	`, id)
	var timeline []map[string]interface{}
	if timeRows != nil {
		defer timeRows.Close()
		for timeRows.Next() {
			var hour time.Time
			var ts, td, to, tc, tb int
			if err := timeRows.Scan(&hour, &ts, &td, &to, &tc, &tb); err != nil {
				continue
			}
			timeline = append(timeline, map[string]interface{}{
				"hour": hour.Format(time.RFC3339), "sent": ts, "delivered": td,
				"opens": to, "clicks": tc, "bounces": tb,
			})
		}
	}
	if timeline == nil {
		timeline = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sent":             sent,
		"delivered":        delivered,
		"opens":            opens,
		"clicks":           clicks,
		"bounces":          bounces,
		"hard_bounces":     hardBounces,
		"soft_bounces":     softBounces,
		"complaints":       complaints,
		"unsubscribes":     unsubscribes,
		"open_rate":        calcRate(opens, sent),
		"click_rate":       calcRate(clicks, sent),
		"bounce_rate":      calcRate(bounces, sent),
		"complaint_rate":   calcRate(complaints, sent),
		"unsubscribe_rate": calcRate(unsubscribes, sent),
		"domain_breakdown": domainBreakdown,
		"hourly_timeline":  timeline,
	})
}

// HandleCampaignTimeline returns sending timeline
func (cb *CampaignBuilder) HandleCampaignTimeline(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	
	rows, _ := cb.db.QueryContext(ctx, `
		SELECT DATE_TRUNC('hour', event_time) as hour,
			   event_type, COUNT(*) as count
		FROM mailing_tracking_events
		WHERE campaign_id = $1
		GROUP BY 1, 2
		ORDER BY 1
	`, id)
	defer rows.Close()
	
	timeline := []map[string]interface{}{}
	for rows.Next() {
		var hour time.Time
		var eventType string
		var count int
		rows.Scan(&hour, &eventType, &count)
		timeline = append(timeline, map[string]interface{}{
			"hour":       hour,
			"event_type": eventType,
			"count":      count,
		})
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"campaign_id": id,
		"timeline":    timeline,
	})
}
