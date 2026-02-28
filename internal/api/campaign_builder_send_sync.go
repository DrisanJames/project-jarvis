package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
)

// HandleSendCampaign sends a campaign immediately using the selected profile
func (cb *CampaignBuilder) HandleSendCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	
	// Get campaign details
	var campaign struct {
		Subject       string
		FromName      string
		FromEmail     string
		HTMLContent   string
		TextContent   string
		ListID        sql.NullString
		SegmentID     sql.NullString
		ProfileID     sql.NullString
		ThrottleSpeed string
		MaxRecipients sql.NullInt64
		Status        string
	}
	
	err := cb.db.QueryRowContext(ctx, `
		SELECT subject, from_name, from_email, 
			   COALESCE(html_content, ''), COALESCE(plain_content, ''),
			   list_id, segment_id, sending_profile_id,
			   COALESCE(throttle_speed, 'gentle'), max_recipients, status
		FROM mailing_campaigns WHERE id = $1
	`, id).Scan(
		&campaign.Subject, &campaign.FromName, &campaign.FromEmail,
		&campaign.HTMLContent, &campaign.TextContent,
		&campaign.ListID, &campaign.SegmentID, &campaign.ProfileID,
		&campaign.ThrottleSpeed, &campaign.MaxRecipients, &campaign.Status,
	)
	
	if err != nil {
		http.Error(w, `{"error":"campaign not found"}`, http.StatusNotFound)
		return
	}
	
	if campaign.Status != "draft" && campaign.Status != "scheduled" {
		http.Error(w, fmt.Sprintf(`{"error":"cannot send campaign in %s status"}`, campaign.Status), http.StatusBadRequest)
		return
	}
	
	// Get sending profile details
	var profile struct {
		ID         string
		VendorType string
		APIKey     sql.NullString
	}
	
	if campaign.ProfileID.Valid {
		cb.db.QueryRowContext(ctx, `
			SELECT id, vendor_type, api_key FROM mailing_sending_profiles WHERE id = $1
		`, campaign.ProfileID.String).Scan(&profile.ID, &profile.VendorType, &profile.APIKey)
	}
	
	// Update status to sending
	cb.db.ExecContext(ctx, `
		UPDATE mailing_campaigns SET status = 'sending', started_at = NOW(), updated_at = NOW() WHERE id = $1
	`, id)
	
	// Get subscribers
	var listID, segmentID *string
	if campaign.ListID.Valid {
		listID = &campaign.ListID.String
	}
	if campaign.SegmentID.Valid {
		segmentID = &campaign.SegmentID.String
	}
	
	subscribers := cb.getSubscribers(ctx, listID, segmentID, campaign.MaxRecipients)
	
	// Apply throttle settings
	throttle := ThrottlePresets[campaign.ThrottleSpeed]
	if throttle.PerMinute == 0 {
		throttle = ThrottlePresets["gentle"]
	}
	
	// Send emails - get org ID from request context
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
		return
	}
	campUUID, _ := uuid.Parse(id)
	
	// ============================================
	// PERSONALIZATION ENGINE SETUP
	// ============================================
	// Initialize template service (with caching) and context builder
	templateSvc := mailing.NewTemplateService()
	contextBuilder := mailing.NewContextBuilder(cb.db, "https://track.ignite.media", "signing-key-placeholder")
	
	// Pre-validate template syntax ONCE before the loop
	// This prevents sending malformed templates to subscribers
	if err := templateSvc.Parse(campaign.HTMLContent); err != nil {
		log.Printf("Campaign %s has invalid HTML template syntax: %v", id, err)
		// Continue anyway - personalization will return original content on error
	}
	if err := templateSvc.Parse(campaign.Subject); err != nil {
		log.Printf("Campaign %s has invalid subject template syntax: %v", id, err)
	}
	
	// Build campaign object for context
	mailCampaign := &mailing.Campaign{
		ID:        campUUID,
		Subject:   campaign.Subject,
		FromName:  campaign.FromName,
		FromEmail: campaign.FromEmail,
	}
	
	// Cache key for this campaign's templates
	htmlCacheKey := fmt.Sprintf("campaign:%s:html", id)
	subjectCacheKey := fmt.Sprintf("campaign:%s:subject", id)
	textCacheKey := fmt.Sprintf("campaign:%s:text", id)
	
	var sent, failed, suppressed int
	sendDelay := time.Duration(60000/throttle.PerMinute) * time.Millisecond
	
	for _, sub := range subscribers {
		// Check legacy suppression table
		var isSuppressed bool
		cb.db.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM mailing_suppressions WHERE LOWER(email) = LOWER($1) AND active = true
			)
		`, sub.Email).Scan(&isSuppressed)
		
		if isSuppressed {
			suppressed++
			continue
		}

		// Check global suppression hub (single source of truth)
		if cb.globalHub != nil && cb.globalHub.IsSuppressed(sub.Email) {
			suppressed++
			continue
		}
		
		// ============================================
		// PERSONALIZATION: Build context for this subscriber
		// ============================================
		// Parse custom fields from raw bytes to map
		var customFieldsMap mailing.JSON
		if len(sub.CustomFields) > 0 {
			json.Unmarshal(sub.CustomFields, &customFieldsMap)
		}
		
		mailSub := &mailing.Subscriber{
			ID:                  sub.ID,
			Email:               sub.Email,
			FirstName:           sub.FirstName,
			LastName:            sub.LastName,
			CustomFields:        customFieldsMap,
			EngagementScore:     sub.EngagementScore,
			TotalEmailsReceived: sub.TotalEmailsReceived,
			TotalOpens:          sub.TotalOpens,
			TotalClicks:         sub.TotalClicks,
			LastOpenAt:          sub.LastOpenAt,
			LastClickAt:         sub.LastClickAt,
			LastEmailAt:         sub.LastEmailAt,
			SubscribedAt:        sub.SubscribedAt,
			Status:              sub.Status,
			Source:              sub.Source,
			Timezone:            sub.Timezone,
		}
		
		renderCtx, ctxErr := contextBuilder.BuildContext(ctx, mailSub, mailCampaign)
		if ctxErr != nil {
			log.Printf("Failed to build render context for %s: %v", logger.RedactEmail(sub.Email), ctxErr)
			// Fall back to original content
			renderCtx = make(mailing.RenderContext)
		}
		
		// Personalize subject line
		personalizedSubject, _ := templateSvc.Render(subjectCacheKey, campaign.Subject, renderCtx)
		
		// Personalize HTML content
		personalizedHTML, _ := templateSvc.Render(htmlCacheKey, campaign.HTMLContent, renderCtx)
		
		// Personalize plain text content (if exists)
		personalizedText := campaign.TextContent
		if campaign.TextContent != "" {
			personalizedText, _ = templateSvc.Render(textCacheKey, campaign.TextContent, renderCtx)
		}
		
		// Inject tracking (AFTER personalization)
		emailID := uuid.New()
		trackedHTML := cb.mailingSvc.injectTracking(personalizedHTML, orgID, campUUID, sub.ID, emailID)
		
		// Send via the appropriate profile/vendor
		var result map[string]interface{}
		var sendErr error
		
		switch profile.VendorType {
		case "ses":
			result, sendErr = cb.mailingSvc.sendViaSES(ctx, sub.Email, campaign.FromEmail, campaign.FromName, "", personalizedSubject, trackedHTML, personalizedText)
		case "mailgun":
			apiKey := ""
			if profile.APIKey.Valid {
				apiKey = profile.APIKey.String
			}
			domain := strings.Split(campaign.FromEmail, "@")[1]
			result, sendErr = cb.mailingSvc.sendViaMailgun(ctx, apiKey, domain, sub.Email, campaign.FromEmail, campaign.FromName, "", personalizedSubject, trackedHTML, personalizedText)
		default: // sparkpost or default
			result, sendErr = cb.mailingSvc.sendViaSparkPost(ctx, sub.Email, campaign.FromEmail, campaign.FromName, personalizedSubject, trackedHTML, personalizedText)
		}
		
		if sendErr == nil && result["success"] == true {
			sent++
			// Record sent event
			cb.db.ExecContext(ctx, `
				INSERT INTO mailing_tracking_events (id, campaign_id, subscriber_id, email, event_type, event_time, metadata)
				VALUES ($1, $2, $3, $4, 'sent', NOW(), $5)
			`, emailID, campUUID, sub.ID, sub.Email, fmt.Sprintf(`{"message_id": "%v", "vendor": "%s"}`, result["message_id"], profile.VendorType))
		} else {
			failed++
		}
		
		// Apply throttle delay
		if sendDelay > 0 {
			time.Sleep(sendDelay)
		}
	}
	
	// Update campaign completion
	finalStatus := "completed"
	if failed > 0 && sent == 0 {
		finalStatus = "failed"
	} else if failed > 0 {
		finalStatus = "completed_with_errors"
	}
	
	// Use background context for final update to avoid cancellation issues
	bgCtx := context.Background()
	
	_, updateErr := cb.db.ExecContext(bgCtx, `
		UPDATE mailing_campaigns 
		SET status = $1, sent_count = $2, completed_at = NOW(), updated_at = NOW()
		WHERE id = $3
	`, finalStatus, sent, id)
	
	if updateErr != nil {
		log.Printf("ERROR updating campaign %s to status %s: %v", id, finalStatus, updateErr)
	} else {
		log.Printf("Campaign %s completed: sent=%d, failed=%d, final_status=%s", id, sent, failed, finalStatus)
	}
	
	// Update profile usage
	if profile.ID != "" {
		cb.db.ExecContext(bgCtx, `
			INSERT INTO mailing_profile_usage (id, profile_id, campaign_id, sent_count, used_at)
			VALUES ($1, $2, $3, $4, NOW())
		`, uuid.New(), profile.ID, id, sent)
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"campaign_id":   id,
		"status":        finalStatus,
		"sent":          sent,
		"failed":        failed,
		"suppressed":    suppressed,
		"total_targeted": len(subscribers),
		"vendor":        profile.VendorType,
		"throttle_speed": campaign.ThrottleSpeed,
	})
}
