package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// HandleScheduleCampaign schedules a campaign for later
func (cb *CampaignBuilder) HandleScheduleCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	
	var input struct {
		ScheduledAt time.Time `json:"scheduled_at"`
		Timezone    string    `json:"timezone"`
	}
	json.NewDecoder(r.Body).Decode(&input)
	
	// Validate minimum preparation time (5 minutes from now)
	minScheduleTime := time.Now().Add(time.Duration(MinPreparationMinutes) * time.Minute)
	if input.ScheduledAt.Before(minScheduleTime) {
		http.Error(w, fmt.Sprintf(
			`{"error":"scheduled_at must be at least %d minutes in the future to allow preparation time (minimum: %s)","min_schedule_time":"%s"}`,
			MinPreparationMinutes,
			minScheduleTime.Format(time.RFC3339),
			minScheduleTime.Format(time.RFC3339),
		), http.StatusBadRequest)
		return
	}
	
	// Calculate edit lock time (when the campaign can no longer be edited)
	editLockTime := input.ScheduledAt.Add(-time.Duration(MinPreparationMinutes) * time.Minute)
	
	_, err := cb.db.ExecContext(ctx, `
		UPDATE mailing_campaigns 
		SET status = 'scheduled', scheduled_at = $1, send_type = 'scheduled', updated_at = NOW()
		WHERE id = $2 AND status IN ('draft', 'scheduled')
	`, input.ScheduledAt, id)
	
	if err != nil {
		http.Error(w, `{"error":"failed to schedule campaign"}`, http.StatusInternalServerError)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":                   id,
		"status":               "scheduled",
		"scheduled_at":         input.ScheduledAt,
		"edit_lock_at":         editLockTime,
		"preparation_minutes":  MinPreparationMinutes,
		"message":              fmt.Sprintf("Campaign scheduled for %s. You can edit until %s.", input.ScheduledAt.Format("Jan 2, 2006 3:04 PM"), editLockTime.Format("Jan 2, 2006 3:04 PM")),
	})
}

// HandlePauseCampaign pauses a scheduled, preparing, or sending campaign
func (cb *CampaignBuilder) HandlePauseCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	
	// First check current status
	var currentStatus string
	err := cb.db.QueryRowContext(ctx, `SELECT status FROM mailing_campaigns WHERE id = $1`, id).Scan(&currentStatus)
	if err != nil {
		http.Error(w, `{"error":"campaign not found"}`, http.StatusNotFound)
		return
	}
	
	// Can pause: scheduled, preparing, sending
	allowedStatuses := map[string]bool{"scheduled": true, "preparing": true, "sending": true}
	if !allowedStatuses[currentStatus] {
		http.Error(w, fmt.Sprintf(`{"error":"cannot pause campaign in '%s' status. Can only pause scheduled, preparing, or sending campaigns."}`, currentStatus), http.StatusBadRequest)
		return
	}
	
	// Pause the campaign
	_, err = cb.db.ExecContext(ctx, `
		UPDATE mailing_campaigns SET status = 'paused', updated_at = NOW()
		WHERE id = $1 AND status IN ('scheduled', 'preparing', 'sending')
	`, id)
	
	if err != nil {
		http.Error(w, `{"error":"failed to pause campaign"}`, http.StatusInternalServerError)
		return
	}
	
	// Also pause any queued items for this campaign
	cb.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue SET status = 'paused', updated_at = NOW()
		WHERE campaign_id = $1 AND status = 'queued'
	`, id)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":              id,
		"previous_status": currentStatus,
		"status":          "paused",
		"message":         "Campaign paused. Use resume to continue.",
	})
}

// HandleResumeCampaign resumes a paused campaign
func (cb *CampaignBuilder) HandleResumeCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	
	_, err := cb.db.ExecContext(ctx, `
		UPDATE mailing_campaigns SET status = 'sending', updated_at = NOW()
		WHERE id = $1 AND status = 'paused'
	`, id)
	
	if err != nil {
		http.Error(w, `{"error":"failed to resume campaign"}`, http.StatusInternalServerError)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":      id,
		"status":  "sending",
		"message": "Campaign resumed",
	})
}

// HandleCancelCampaign cancels a campaign (can cancel at any time except when completed)
func (cb *CampaignBuilder) HandleCancelCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	
	// First check current status
	var currentStatus string
	var sentCount int
	err := cb.db.QueryRowContext(ctx, `
		SELECT status, COALESCE(sent_count, 0) FROM mailing_campaigns WHERE id = $1
	`, id).Scan(&currentStatus, &sentCount)
	
	if err != nil {
		http.Error(w, `{"error":"campaign not found"}`, http.StatusNotFound)
		return
	}
	
	// Cannot cancel already completed or cancelled campaigns
	notAllowed := map[string]bool{"completed": true, "completed_with_errors": true, "cancelled": true, "failed": true}
	if notAllowed[currentStatus] {
		http.Error(w, fmt.Sprintf(`{"error":"cannot cancel campaign in '%s' status"}`, currentStatus), http.StatusBadRequest)
		return
	}
	
	// Cancel the campaign
	result, err := cb.db.ExecContext(ctx, `
		UPDATE mailing_campaigns 
		SET status = 'cancelled', completed_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status NOT IN ('completed', 'completed_with_errors', 'cancelled', 'failed')
	`, id)
	
	if err != nil {
		http.Error(w, `{"error":"failed to cancel campaign"}`, http.StatusInternalServerError)
		return
	}
	
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		http.Error(w, `{"error":"campaign already completed or cancelled"}`, http.StatusBadRequest)
		return
	}
	
	// Cancel any pending queue items for this campaign
	cancelledItems, _ := cb.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue 
		SET status = 'cancelled', updated_at = NOW()
		WHERE campaign_id = $1 AND status IN ('queued', 'paused')
	`, id)
	
	queueCancelled, _ := cancelledItems.RowsAffected()
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":                   id,
		"previous_status":      currentStatus,
		"status":               "cancelled",
		"sent_before_cancel":   sentCount,
		"queue_items_cancelled": queueCancelled,
		"message":              fmt.Sprintf("Campaign cancelled. %d emails were sent before cancellation, %d queued items cancelled.", sentCount, queueCancelled),
	})
}

// HandleGetScheduledCampaigns returns all campaigns in scheduled/preparing/sending status.
// GET /api/mailing/campaigns/scheduled
func (cb *CampaignBuilder) HandleGetScheduledCampaigns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := cb.db.QueryContext(ctx, `
		SELECT id, name, status, COALESCE(scheduled_at, send_at) as scheduled_at,
		       COALESCE(total_recipients, 0), COALESCE(sent_count, 0)
		FROM mailing_campaigns
		WHERE status IN ('scheduled', 'preparing', 'sending')
		ORDER BY COALESCE(scheduled_at, send_at) ASC
	`)
	if err != nil {
		http.Error(w, `{"error":"failed to query campaigns"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type scheduledCampaign struct {
		ID              string  `json:"id"`
		Name            string  `json:"name"`
		Status          string  `json:"status"`
		ScheduledAt     *string `json:"scheduled_at"`
		TotalRecipients int     `json:"total_recipients"`
		SentCount       int     `json:"sent_count"`
	}

	var campaigns []scheduledCampaign
	for rows.Next() {
		var c scheduledCampaign
		var scheduledAt sql.NullTime
		if err := rows.Scan(&c.ID, &c.Name, &c.Status, &scheduledAt, &c.TotalRecipients, &c.SentCount); err != nil {
			continue
		}
		if scheduledAt.Valid {
			t := scheduledAt.Time.Format(time.RFC3339)
			c.ScheduledAt = &t
		}
		campaigns = append(campaigns, c)
	}

	if campaigns == nil {
		campaigns = []scheduledCampaign{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"count":     len(campaigns),
		"campaigns": campaigns,
	})
}

// HandleCancelAllScheduled cancels all campaigns in scheduled/preparing status.
// Does NOT cancel campaigns already in 'sending' status (those are actively sending).
// POST /api/mailing/campaigns/cancel-all-scheduled
func (cb *CampaignBuilder) HandleCancelAllScheduled(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Cancel all scheduled/preparing campaigns
	result, err := cb.db.ExecContext(ctx, `
		UPDATE mailing_campaigns
		SET status = 'cancelled', completed_at = NOW(), updated_at = NOW()
		WHERE status IN ('scheduled', 'preparing')
	`)
	if err != nil {
		http.Error(w, `{"error":"failed to cancel scheduled campaigns"}`, http.StatusInternalServerError)
		return
	}

	cancelled, _ := result.RowsAffected()

	// Also cancel queued items for those campaigns
	queueResult, _ := cb.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue
		SET status = 'cancelled', updated_at = NOW()
		WHERE campaign_id IN (
			SELECT id FROM mailing_campaigns WHERE status = 'cancelled' AND completed_at > NOW() - INTERVAL '1 minute'
		)
		AND status IN ('queued', 'paused')
	`)
	queueCancelled, _ := queueResult.RowsAffected()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"cancelled_campaigns":   cancelled,
		"cancelled_queue_items": queueCancelled,
		"message":               fmt.Sprintf("Cancelled %d scheduled campaigns and %d queue items.", cancelled, queueCancelled),
	})
}

// HandleResetCampaign resets a stuck or failed campaign back to draft status
func (cb *CampaignBuilder) HandleResetCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	
	// Reset campaign to draft - only allowed for failed, cancelled, or stuck sending (sent_count = 0)
	result, err := cb.db.ExecContext(ctx, `
		UPDATE mailing_campaigns 
		SET status = 'draft', 
			started_at = NULL, 
			completed_at = NULL,
			sent_count = 0,
			updated_at = NOW()
		WHERE id = $1 AND (
			status IN ('failed', 'cancelled', 'completed_with_errors')
			OR (status = 'sending' AND sent_count = 0)
		)
	`, id)
	
	if err != nil {
		http.Error(w, `{"error":"failed to reset campaign"}`, http.StatusInternalServerError)
		return
	}
	
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		http.Error(w, `{"error":"cannot reset campaign - must be failed, cancelled, or stuck"}`, http.StatusBadRequest)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":      id,
		"status":  "draft",
		"message": "Campaign reset to draft",
	})
}

// HandleDuplicateCampaign creates a copy of an existing campaign
func (cb *CampaignBuilder) HandleDuplicateCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	
	newID := uuid.New()
	
	_, err := cb.db.ExecContext(ctx, `
		INSERT INTO mailing_campaigns (
			id, organization_id, name, subject, preview_text,
			html_content, plain_content, list_id, segment_id,
			sending_profile_id, from_name, from_email, reply_email,
			send_type, throttle_speed, max_recipients,
			status, created_at, updated_at
		)
		SELECT 
			$1, organization_id, name || ' (Copy)', subject, preview_text,
			html_content, plain_content, list_id, segment_id,
			sending_profile_id, from_name, from_email, reply_email,
			'instant', throttle_speed, max_recipients,
			'draft', NOW(), NOW()
		FROM mailing_campaigns WHERE id = $2
	`, newID, id)
	
	if err != nil {
		http.Error(w, `{"error":"failed to duplicate campaign"}`, http.StatusInternalServerError)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":          newID.String(),
		"original_id": id,
		"status":      "draft",
		"message":     "Campaign duplicated",
	})
}
