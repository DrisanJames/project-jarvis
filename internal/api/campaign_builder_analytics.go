package api

import (
	"database/sql"
	"encoding/json"
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
		result, _ := cb.mailingSvc.sendViaSparkPost(ctx, email, fromEmail, fromName, "[TEST] "+subject, htmlContent, "")
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

// HandleCampaignStats returns campaign statistics
func (cb *CampaignBuilder) HandleCampaignStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	
	var stats struct {
		Sent         int
		Opens        int
		Clicks       int
		Bounces      int
		Complaints   int
		Unsubscribes int
	}
	
	cb.db.QueryRowContext(ctx, `
		SELECT COALESCE(sent_count, 0), COALESCE(open_count, 0), COALESCE(click_count, 0),
			   COALESCE(bounce_count, 0), COALESCE(complaint_count, 0), COALESCE(unsubscribe_count, 0)
		FROM mailing_campaigns WHERE id = $1
	`, id).Scan(&stats.Sent, &stats.Opens, &stats.Clicks, &stats.Bounces, &stats.Complaints, &stats.Unsubscribes)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sent":            stats.Sent,
		"opens":           stats.Opens,
		"clicks":          stats.Clicks,
		"bounces":         stats.Bounces,
		"complaints":      stats.Complaints,
		"unsubscribes":    stats.Unsubscribes,
		"open_rate":       calcRate(stats.Opens, stats.Sent),
		"click_rate":      calcRate(stats.Clicks, stats.Sent),
		"bounce_rate":     calcRate(stats.Bounces, stats.Sent),
		"complaint_rate":  calcRate(stats.Complaints, stats.Sent),
		"unsubscribe_rate": calcRate(stats.Unsubscribes, stats.Sent),
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
