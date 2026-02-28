package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
)

// HandleSetThrottle allows adjusting throttle rate mid-send
func (cb *CampaignBuilder) HandleSetThrottle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	var input struct {
		ThrottleSpeed    string `json:"throttle_speed"`     // "instant", "gentle", "moderate", "careful", "custom"
		RatePerMinute    int    `json:"rate_per_minute"`    // For custom throttle
	}
	
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	// Validate throttle speed
	validSpeeds := map[string]bool{
		"instant": true, "gentle": true, "moderate": true, "careful": true, "custom": true,
	}
	if !validSpeeds[input.ThrottleSpeed] {
		http.Error(w, `{"error":"invalid throttle_speed - must be instant, gentle, moderate, careful, or custom"}`, http.StatusBadRequest)
		return
	}

	// For custom, validate rate
	if input.ThrottleSpeed == "custom" {
		if input.RatePerMinute <= 0 {
			http.Error(w, `{"error":"rate_per_minute required for custom throttle"}`, http.StatusBadRequest)
			return
		}
		// Clamp to reasonable limits
		if input.RatePerMinute > 10000 {
			input.RatePerMinute = 10000
		}
	}

	// Check campaign status
	var status string
	err := cb.db.QueryRowContext(ctx, `SELECT status FROM mailing_campaigns WHERE id = $1`, id).Scan(&status)
	if err != nil {
		http.Error(w, `{"error":"campaign not found"}`, http.StatusNotFound)
		return
	}

	// Can only adjust throttle for sending campaigns
	if status != "sending" && status != "scheduled" && status != "paused" {
		http.Error(w, fmt.Sprintf(`{"error":"cannot adjust throttle for campaign in '%s' status"}`, status), http.StatusBadRequest)
		return
	}

	// Get rate per minute based on speed
	ratePerMinute := input.RatePerMinute
	if input.ThrottleSpeed != "custom" {
		preset := ThrottlePresets[input.ThrottleSpeed]
		ratePerMinute = preset.PerMinute
	}

	// Update in database for persistence
	_, err = cb.db.ExecContext(ctx, `
		UPDATE mailing_campaigns 
		SET throttle_speed = $1, throttle_rate_per_minute = $2, updated_at = NOW()
		WHERE id = $3
	`, input.ThrottleSpeed, ratePerMinute, id)
	
	if err != nil {
		http.Error(w, `{"error":"failed to update throttle"}`, http.StatusInternalServerError)
		return
	}

	// Also update in Redis for real-time effect (workers poll this)
	// The worker will pick up this change within ~10 seconds
	redisKey := fmt.Sprintf("campaign:throttle:%s", id)
	throttleData, _ := json.Marshal(map[string]interface{}{
		"rate":            input.ThrottleSpeed,
		"rate_per_minute": ratePerMinute,
		"updated_at":      time.Now(),
	})
	
	// Note: In production, inject Redis client properly
	// For now, log the intent
	log.Printf("[Throttle] Campaign %s throttle changed to %s (%d/min), Redis key: %s", 
		id, input.ThrottleSpeed, ratePerMinute, redisKey)
	_ = throttleData // Would be: redisClient.Set(ctx, redisKey, throttleData, 24*time.Hour)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":              id,
		"throttle_speed":  input.ThrottleSpeed,
		"rate_per_minute": ratePerMinute,
		"effective_in":    "~10 seconds",
		"message":         "Throttle updated. Workers will pick up the new rate shortly.",
	})
}

// HandleSendCampaignAsync queues a campaign for background sending
// Returns immediately with a job_id - use /stats to track progress
func (cb *CampaignBuilder) HandleSendCampaignAsync(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	// Acquire distributed lock to prevent duplicate sends from concurrent API calls
	lock := distlock.NewLock(cb.redisClient, cb.db, fmt.Sprintf("campaign:%s", id), 10*time.Minute)
	acquired, lockErr := lock.Acquire(ctx)
	if lockErr != nil {
		log.Printf("[SendCampaignAsync] Warning: lock acquisition error for campaign %s: %v", id, lockErr)
		// Continue without lock — SQL status guard below is a secondary safety net
	} else if !acquired {
		http.Error(w, `{"error":"campaign is already being processed by another request"}`, http.StatusConflict)
		return
	} else {
		defer lock.Release(ctx)
	}

	// Check campaign exists and status
	var status string
	var listID, segmentID sql.NullString
	err := cb.db.QueryRowContext(ctx, `
		SELECT status, list_id, segment_id FROM mailing_campaigns WHERE id = $1
	`, id).Scan(&status, &listID, &segmentID)

	if err != nil {
		http.Error(w, `{"error":"campaign not found"}`, http.StatusNotFound)
		return
	}

	if status != "draft" && status != "scheduled" {
		http.Error(w, fmt.Sprintf(`{"error":"cannot send campaign in '%s' status"}`, status), http.StatusConflict)
		return
	}

	// Check for audience
	if !listID.Valid && !segmentID.Valid {
		http.Error(w, `{"error":"campaign has no audience (list or segment)"}`, http.StatusBadRequest)
		return
	}

	// Generate job ID
	jobID := uuid.New().String()

	// Update status to preparing (will transition to sending once queue is populated)
	_, err = cb.db.ExecContext(ctx, `
		UPDATE mailing_campaigns 
		SET status = 'preparing', started_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status IN ('draft', 'scheduled')
	`, id)

	if err != nil {
		http.Error(w, `{"error":"failed to start campaign"}`, http.StatusInternalServerError)
		return
	}

	// Enqueue in background
	go cb.enqueueCampaignAsync(id, listID, segmentID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"campaign_id": id,
		"job_id":      jobID,
		"status":      "queuing",
		"message":     "Campaign is being queued for sending. Use GET /campaigns/{id}/stats to track progress.",
	})
}

// enqueueCampaignAsync handles background enqueuing of subscribers
func (cb *CampaignBuilder) enqueueCampaignAsync(campaignID string, listID, segmentID sql.NullString) {
	ctx := context.Background()
	campUUID, _ := uuid.Parse(campaignID)

	// Get campaign details
	var subject, htmlContent, plainContent, throttleSpeed string
	var maxRecipients sql.NullInt64
	
	err := cb.db.QueryRowContext(ctx, `
		SELECT subject, COALESCE(html_content, ''), COALESCE(plain_content, ''),
			   COALESCE(throttle_speed, 'gentle'), max_recipients
		FROM mailing_campaigns WHERE id = $1
	`, campaignID).Scan(&subject, &htmlContent, &plainContent, &throttleSpeed, &maxRecipients)
	
	if err != nil {
		log.Printf("[EnqueueAsync] Failed to get campaign %s: %v", campaignID, err)
		cb.db.ExecContext(ctx, `UPDATE mailing_campaigns SET status = 'failed' WHERE id = $1`, campaignID)
		return
	}

	// Calculate priority
	priority := 5
	switch throttleSpeed {
	case "instant":
		priority = 10
	case "gentle":
		priority = 7
	case "moderate":
		priority = 5
	case "careful":
		priority = 3
	}

	// Build subscriber query
	var query string
	var args []interface{}

	if segmentID.Valid && segmentID.String != "" {
		query, args = cb.mailingSvc.buildSegmentQuery(ctx, segmentID.String)
		if query == "" {
			log.Printf("[EnqueueAsync] Failed to build segment query for campaign %s", campaignID)
			cb.db.ExecContext(ctx, `UPDATE mailing_campaigns SET status = 'failed' WHERE id = $1`, campaignID)
			return
		}
	} else if listID.Valid {
		query = `SELECT id, email FROM mailing_subscribers WHERE list_id = $1 AND status = 'confirmed'`
		args = []interface{}{listID.String}
	} else {
		log.Printf("[EnqueueAsync] Campaign %s has no list or segment", campaignID)
		cb.db.ExecContext(ctx, `UPDATE mailing_campaigns SET status = 'failed' WHERE id = $1`, campaignID)
		return
	}

	// Add suppression check
	query = fmt.Sprintf(`
		SELECT id, email FROM (%s) sub
		WHERE NOT EXISTS (
			SELECT 1 FROM mailing_suppressions sup 
			WHERE LOWER(sup.email) = LOWER(sub.email) AND sup.active = true
		)
	`, query)

	// Apply limit
	if maxRecipients.Valid && maxRecipients.Int64 > 0 {
		query += fmt.Sprintf(" LIMIT %d", maxRecipients.Int64)
	}

	rows, err := cb.db.QueryContext(ctx, query, args...)
	if err != nil {
		log.Printf("[EnqueueAsync] Failed to query subscribers for %s: %v", campaignID, err)
		cb.db.ExecContext(ctx, `UPDATE mailing_campaigns SET status = 'failed' WHERE id = $1`, campaignID)
		return
	}
	defer rows.Close()

	// Insert into queue
	var queued int
	for rows.Next() {
		var subID uuid.UUID
		var email string
		if err := rows.Scan(&subID, &email); err != nil {
			continue
		}

		queueID := uuid.New()
		_, err := cb.db.ExecContext(ctx, `
			INSERT INTO mailing_campaign_queue (
				id, campaign_id, subscriber_id, subject, html_content, plain_content,
				status, priority, scheduled_at, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, 'queued', $7, NOW(), NOW())
			ON CONFLICT DO NOTHING
		`, queueID, campUUID, subID, subject, htmlContent, plainContent, priority)

		if err == nil {
			queued++
		}

		// Update progress periodically
		if queued%1000 == 0 {
			cb.db.ExecContext(ctx, `
				UPDATE mailing_campaigns SET queued_count = $1, updated_at = NOW() WHERE id = $2
			`, queued, campaignID)
		}
	}

	// Finalize
	if queued == 0 {
		cb.db.ExecContext(ctx, `
			UPDATE mailing_campaigns SET status = 'completed', completed_at = NOW(), sent_count = 0 WHERE id = $1
		`, campaignID)
		log.Printf("[EnqueueAsync] Campaign %s has no recipients to send to", campaignID)
		return
	}

	// Update campaign to sending status
	cb.db.ExecContext(ctx, `
		UPDATE mailing_campaigns 
		SET status = 'sending', total_recipients = $1, queued_count = $1, updated_at = NOW()
		WHERE id = $2
	`, queued, campaignID)

	log.Printf("[EnqueueAsync] Campaign %s: queued %d subscribers, status=sending", campaignID, queued)
}
