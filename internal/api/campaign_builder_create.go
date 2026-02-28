package api

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// HandleCreateCampaign creates a new campaign with modern defaults
func (cb *CampaignBuilder) HandleCreateCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	
	var input CampaignInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	
	// Validation
	if input.Name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}
	if input.Subject == "" {
		http.Error(w, `{"error":"subject is required"}`, http.StatusBadRequest)
		return
	}
	if input.HTMLContent == "" {
		http.Error(w, `{"error":"html_content is required"}`, http.StatusBadRequest)
		return
	}
	
	// Support segment_ids as primary, with backward compatibility
	if len(input.SegmentIDs) == 0 && input.SegmentID != nil {
		input.SegmentIDs = []string{*input.SegmentID}
	}
	// Also support list_ids for backward compatibility
	if len(input.ListIDs) == 0 && input.ListID != nil {
		input.ListIDs = []string{*input.ListID}
	}
	// Require at least segments OR lists
	if len(input.SegmentIDs) == 0 && len(input.ListIDs) == 0 {
		http.Error(w, `{"error":"segment_ids or list_ids is required"}`, http.StatusBadRequest)
		return
	}
	
	// Defaults
	if input.SendType == "" {
		input.SendType = "instant"
	}
	if input.ThrottleSpeed == "" {
		input.ThrottleSpeed = "gentle" // Default to gentle for better deliverability
	}
	
	// Auto-generate text content if not provided
	if input.TextContent == "" {
		input.TextContent = stripHTML(input.HTMLContent)
	}
	
	// Get sending profile (from ESP quotas or default)
	var profileID, fromName, fromEmail, replyEmail string
	
	// First check ESP quotas
	if len(input.ESPQuotas) > 0 {
		// Use first ESP with quota > 0 as primary (must be configured)
		for _, quota := range input.ESPQuotas {
			if quota.Percentage > 0 {
				var hasAPIKey bool
				err := cb.db.QueryRowContext(ctx, `
					SELECT id, from_name, from_email, COALESCE(reply_email, ''),
					       (api_key IS NOT NULL AND api_key != '') as has_api_key
					FROM mailing_sending_profiles WHERE id = $1 AND status = 'active'
				`, quota.ProfileID).Scan(&profileID, &fromName, &fromEmail, &replyEmail, &hasAPIKey)
				if err == nil && hasAPIKey {
					break
				}
			}
		}
	}
	
	// Fallback to single profile or default
	if profileID == "" {
		if input.SendingProfileID != nil && *input.SendingProfileID != "" {
			// Check profile exists, is active, AND has API key configured
			var hasAPIKey bool
			err := cb.db.QueryRowContext(ctx, `
				SELECT id, from_name, from_email, COALESCE(reply_email, ''),
				       (api_key IS NOT NULL AND api_key != '') as has_api_key
				FROM mailing_sending_profiles WHERE id = $1 AND status = 'active'
			`, *input.SendingProfileID).Scan(&profileID, &fromName, &fromEmail, &replyEmail, &hasAPIKey)
			if err != nil {
				http.Error(w, `{"error":"sending profile not found or inactive"}`, http.StatusBadRequest)
				return
			}
			if !hasAPIKey {
				http.Error(w, `{"error":"sending profile is not configured - API credentials are missing"}`, http.StatusBadRequest)
				return
			}
		} else {
			// Use default profile (must be configured)
			err := cb.db.QueryRowContext(ctx, `
				SELECT id, from_name, from_email, COALESCE(reply_email, '')
				FROM mailing_sending_profiles 
				WHERE is_default = true AND status = 'active' 
				  AND api_key IS NOT NULL AND api_key != ''
				LIMIT 1
			`).Scan(&profileID, &fromName, &fromEmail, &replyEmail)
			if err != nil {
				// No default profile, use hardcoded defaults
				fromName = "Your Company"
				fromEmail = "noreply@example.com"
			}
		}
	}
	
	// Override from profile if specified in input
	if input.FromName != nil && *input.FromName != "" {
		fromName = *input.FromName
	}
	if input.FromEmail != nil && *input.FromEmail != "" {
		fromEmail = *input.FromEmail
	}
	if input.ReplyEmail != nil && *input.ReplyEmail != "" {
		replyEmail = *input.ReplyEmail
	}
	
	// Serialize JSON fields
	segmentIDsJSON, _ := json.Marshal(input.SegmentIDs)
	listIDsJSON, _ := json.Marshal(input.ListIDs)
	suppressionIDsJSON, _ := json.Marshal(input.SuppressionListIDs)
	suppressionSegmentIDsJSON, _ := json.Marshal(input.SuppressionSegmentIDs)
	espQuotasJSON, _ := json.Marshal(input.ESPQuotas)
	
	// Create campaign with extended fields
	id := uuid.New()
	orgID := getOrganizationID(r)
	
	// Use first segment_id or list_id for backward compatibility
	var primarySegmentID, primaryListID *string
	if len(input.SegmentIDs) > 0 {
		primarySegmentID = &input.SegmentIDs[0]
	}
	if len(input.ListIDs) > 0 {
		primaryListID = &input.ListIDs[0]
	}
	_ = segmentIDsJSON // Will be used when column is added
	
	// Determine campaign status based on scheduled_at
	// If scheduled_at is provided and in the future, set status to 'scheduled'
	// Otherwise, set status to 'draft'
	campaignStatus := "draft"
	if input.ScheduledAt != nil && !input.ScheduledAt.IsZero() && input.ScheduledAt.After(time.Now()) {
		campaignStatus = "scheduled"
		log.Printf("Campaign %s will be scheduled for %s", id, input.ScheduledAt.Format(time.RFC3339))
	}
	
	// Begin transaction: campaign INSERT + Everflow metadata must be atomic.
	// Without a transaction, a failure between these steps leaves the DB in a partial state.
	tx, err := cb.db.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("Error starting transaction for campaign creation: %v", err)
		http.Error(w, `{"error":"Failed to create campaign"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	
	_, err = tx.ExecContext(ctx, `
		INSERT INTO mailing_campaigns (
			id, organization_id, name, subject, preview_text,
			html_content, plain_content, list_id, segment_id,
			sending_profile_id, from_name, from_email, reply_to,
			send_type, scheduled_at, throttle_speed, 
			throttle_rate_per_minute, throttle_duration_hours,
			max_recipients, list_ids, suppression_list_ids, suppression_segment_ids, esp_quotas,
			status, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9,
			$10, $11, $12, $13,
			$14, $15, $16,
			$17, $18,
			$19, $20, $21, $22, $23,
			$24, NOW(), NOW()
		)
	`, id, orgID, input.Name, input.Subject, input.PreviewText,
		input.HTMLContent, input.TextContent, primaryListID, primarySegmentID,
		nullIfEmpty(profileID), fromName, fromEmail, nullIfEmpty(replyEmail),
		input.SendType, input.ScheduledAt, input.ThrottleSpeed,
		nullIfZero(input.ThrottleRatePerMinute), nullIfZero(input.ThrottleDurationHours),
		input.MaxRecipients, string(listIDsJSON), string(suppressionIDsJSON), string(suppressionSegmentIDsJSON), string(espQuotasJSON),
		campaignStatus)
	
	if err != nil {
		log.Printf("Error creating campaign: %v", err)
		// Rollback the failed transaction before running DDL migrations
		tx.Rollback()
		// Try to add missing columns (DDL runs outside any transaction)
		cb.ensureCampaignColumns(ctx)
		// Start a new transaction for the retry
		tx, err = cb.db.BeginTx(ctx, nil)
		if err != nil {
			log.Printf("Error starting retry transaction: %v", err)
			http.Error(w, `{"error":"Failed to create campaign"}`, http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()
		// Retry insert with simpler schema
		_, err = tx.ExecContext(ctx, `
			INSERT INTO mailing_campaigns (
				id, organization_id, name, subject, preview_text,
				html_content, plain_content, list_id, segment_id,
				sending_profile_id, from_name, from_email, reply_to,
				send_type, scheduled_at, throttle_speed, max_recipients,
				status, created_at, updated_at
			) VALUES (
				$1, $2, $3, $4, $5,
				$6, $7, $8, $9,
				$10, $11, $12, $13,
				$14, $15, $16, $17,
				$18, NOW(), NOW()
			)
		`, id, orgID, input.Name, input.Subject, input.PreviewText,
			input.HTMLContent, input.TextContent, primaryListID, input.SegmentID,
			nullIfEmpty(profileID), fromName, fromEmail, nullIfEmpty(replyEmail),
			input.SendType, input.ScheduledAt, input.ThrottleSpeed, input.MaxRecipients,
			campaignStatus)
		if err != nil {
			log.Printf("Error creating campaign (retry): %v", err)
			log.Printf("ERROR: failed to create campaign (retry): %v", err)
			http.Error(w, `{"error":"Failed to create campaign"}`, http.StatusInternalServerError)
			return
		}
	}
	
	// Save Everflow creative metadata if provided (within the same transaction)
	if input.EverflowCreativeID != nil || input.EverflowOfferID != nil || input.TrackingLinkTemplate != nil {
		_, efErr := tx.ExecContext(ctx, `
			UPDATE mailing_campaigns 
			SET everflow_creative_id = COALESCE($2, everflow_creative_id),
			    everflow_offer_id = COALESCE($3, everflow_offer_id),
			    tracking_link_template = COALESCE($4, tracking_link_template)
			WHERE id = $1
		`, id, input.EverflowCreativeID, input.EverflowOfferID, input.TrackingLinkTemplate)
		if efErr != nil {
			log.Printf("Error saving Everflow creative metadata, rolling back campaign: %v", efErr)
			http.Error(w, `{"error":"Failed to save campaign metadata"}`, http.StatusInternalServerError)
			return
		}
	}
	
	// Commit the transaction - campaign INSERT + Everflow metadata are now atomic
	if err := tx.Commit(); err != nil {
		log.Printf("Error committing campaign creation transaction: %v", err)
		http.Error(w, `{"error":"Failed to create campaign"}`, http.StatusInternalServerError)
		return
	}

	// Get audience count (sum of all lists)
	var audienceCount int
	for _, listID := range input.ListIDs {
		audienceCount += cb.getAudienceCount(ctx, &listID, nil)
	}
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":                           id.String(),
		"name":                         input.Name,
		"status":                       campaignStatus,
		"audience_count":               audienceCount,
		"list_count":                   len(input.ListIDs),
		"suppression_count":            len(input.SuppressionListIDs),
		"suppression_segment_count":    len(input.SuppressionSegmentIDs),
		"esp_count":                    len(input.ESPQuotas),
		"sending_profile":              profileID,
		"from_name":                    fromName,
		"from_email":                   fromEmail,
		"throttle_speed":               input.ThrottleSpeed,
		"throttle_rate":                input.ThrottleRatePerMinute,
		"scheduled_at":                 input.ScheduledAt,
		"message":                      "Campaign created successfully",
	})
}
