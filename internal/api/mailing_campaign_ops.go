package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func (s *AdvancedMailingService) HandleUpdateCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID := chi.URLParam(r, "campaignId")
	
	var input struct {
		Name        string `json:"name"`
		Subject     string `json:"subject"`
		FromName    string `json:"from_name"`
		FromEmail   string `json:"from_email"`
		HTMLContent string `json:"html_content"`
		ListID      string `json:"list_id"`
		SegmentID   string `json:"segment_id"`
	}
	json.NewDecoder(r.Body).Decode(&input)
	
	_, err := s.db.ExecContext(ctx, `
		UPDATE mailing_campaigns SET 
			name = COALESCE(NULLIF($2, ''), name),
			subject = COALESCE(NULLIF($3, ''), subject),
			from_name = COALESCE(NULLIF($4, ''), from_name),
			from_email = COALESCE(NULLIF($5, ''), from_email),
			html_content = COALESCE(NULLIF($6, ''), html_content),
			list_id = CASE WHEN $7 = '' THEN list_id ELSE $7::uuid END,
			segment_id = CASE WHEN $8 = '' THEN segment_id ELSE $8::uuid END,
			updated_at = NOW()
		WHERE id = $1
	`, campaignID, input.Name, input.Subject, input.FromName, input.FromEmail, input.HTMLContent, input.ListID, input.SegmentID)
	
	if err != nil {
		http.Error(w, `{"error":"failed to update campaign"}`, http.StatusInternalServerError)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"id": campaignID, "updated": true})
}

func (s *AdvancedMailingService) HandleCloneCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID := chi.URLParam(r, "campaignId")
	
	newID := uuid.New()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mailing_campaigns (id, organization_id, list_id, template_id, segment_id, name, campaign_type,
			subject, from_name, from_email, reply_to, html_content, plain_content, preview_text,
			ai_send_time_optimization, ai_content_optimization, ai_audience_optimization, status, created_at, updated_at)
		SELECT $2, organization_id, list_id, template_id, segment_id, name || ' (Copy)', campaign_type,
			subject, from_name, from_email, reply_to, html_content, plain_content, preview_text,
			ai_send_time_optimization, ai_content_optimization, ai_audience_optimization, 'draft', NOW(), NOW()
		FROM mailing_campaigns WHERE id = $1
	`, campaignID, newID)
	
	if err != nil {
		http.Error(w, `{"error":"failed to clone campaign"}`, http.StatusInternalServerError)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{"id": newID.String(), "cloned_from": campaignID})
}

// Minimum preparation time before a campaign can be sent (in minutes)
const AdvMinPreparationMinutes = 5

func (s *AdvancedMailingService) HandleScheduleCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID := chi.URLParam(r, "campaignId")
	
	var input struct {
		SendAt      string `json:"send_at"`       // ISO 8601 (alias for scheduled_at)
		ScheduledAt string `json:"scheduled_at"`  // ISO 8601 (preferred)
		Timezone    string `json:"timezone"`
	}
	json.NewDecoder(r.Body).Decode(&input)
	
	// Support both send_at and scheduled_at for backward compatibility
	scheduledStr := input.ScheduledAt
	if scheduledStr == "" {
		scheduledStr = input.SendAt
	}
	
	scheduledAt, err := time.Parse(time.RFC3339, scheduledStr)
	if err != nil {
		http.Error(w, `{"error":"invalid scheduled_at/send_at format - use ISO 8601 (e.g., 2026-01-02T15:04:05Z)"}`, http.StatusBadRequest)
		return
	}
	
	// Validate minimum preparation time (5 minutes from now)
	minScheduleTime := time.Now().Add(time.Duration(AdvMinPreparationMinutes) * time.Minute)
	if scheduledAt.Before(minScheduleTime) {
		http.Error(w, fmt.Sprintf(
			`{"error":"scheduled_at must be at least %d minutes in the future to allow preparation time","min_schedule_time":"%s"}`,
			AdvMinPreparationMinutes,
			minScheduleTime.Format(time.RFC3339),
		), http.StatusBadRequest)
		return
	}
	
	if input.Timezone == "" { input.Timezone = "America/Denver" }
	
	// Calculate edit lock time
	editLockTime := scheduledAt.Add(-time.Duration(AdvMinPreparationMinutes) * time.Minute)
	
	// Update using scheduled_at column (consistent with campaign_builder.go)
	_, err = s.db.ExecContext(ctx, `
		UPDATE mailing_campaigns 
		SET status = 'scheduled', scheduled_at = $2, send_type = 'scheduled', timezone = $3, updated_at = NOW()
		WHERE id = $1 AND status IN ('draft', 'scheduled')
	`, campaignID, scheduledAt, input.Timezone)
	
	if err != nil {
		log.Printf("Error scheduling campaign: %v", err)
		http.Error(w, `{"error":"failed to schedule campaign"}`, http.StatusInternalServerError)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":                  campaignID, 
		"status":              "scheduled", 
		"scheduled_at":        scheduledAt, 
		"edit_lock_at":        editLockTime,
		"preparation_minutes": AdvMinPreparationMinutes,
		"timezone":            input.Timezone,
		"message":             fmt.Sprintf("Campaign scheduled for %s. You can edit until %s.", scheduledAt.Format("Jan 2, 2006 3:04 PM"), editLockTime.Format("Jan 2, 2006 3:04 PM")),
	})
}

func (s *AdvancedMailingService) HandleDeleteCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID := chi.URLParam(r, "campaignId")
	
	s.db.ExecContext(ctx, `DELETE FROM mailing_campaigns WHERE id = $1 AND status IN ('draft', 'queued')`, campaignID)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"deleted": campaignID})
}
