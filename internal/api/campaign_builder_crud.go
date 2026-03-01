package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// HandleListCampaigns lists all campaigns with filtering and pagination
func (cb *CampaignBuilder) HandleListCampaigns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pag := ParsePagination(r, 50, 200)

	orgID := getOrganizationID(r)
	if orgID == "" {
		// Log the issue for debugging
		log.Printf("[CampaignBuilder] Warning: Empty org ID from request")
	}
	status := r.URL.Query().Get("status")

	// Build WHERE clause (shared between count and data queries)
	whereClause := " WHERE c.organization_id = $1"
	args := []interface{}{orgID}

	if status != "" {
		whereClause += fmt.Sprintf(" AND c.status = $%d", len(args)+1)
		args = append(args, status)
	}

	// Get total count with same filters
	var total int64
	countQuery := `SELECT COUNT(*) FROM mailing_campaigns c` + whereClause
	if err := cb.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		log.Printf("[CampaignBuilder] Count error: %v", err)
		http.Error(w, `{"error":"failed to count campaigns"}`, http.StatusInternalServerError)
		return
	}

	// Build data query
	query := `
		SELECT c.id, c.name, c.subject, c.status, 
			   c.sent_count, c.open_count, c.click_count,
			   c.created_at, c.scheduled_at, c.completed_at,
			   COALESCE(p.name, '') as profile_name,
			   COALESCE(p.vendor_type, '') as vendor_type,
			   COALESCE(l.name, '') as list_name
		FROM mailing_campaigns c
		LEFT JOIN mailing_sending_profiles p ON c.sending_profile_id = p.id
		LEFT JOIN mailing_lists l ON c.list_id = l.id
	` + whereClause

	query += " ORDER BY c.created_at DESC"
	query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
	args = append(args, pag.Limit, pag.Offset)

	log.Printf("[CampaignBuilder] Query: %s with args: %v", query, args)
	rows, err := cb.db.QueryContext(ctx, query, args...)
	if err != nil {
		log.Printf("[CampaignBuilder] Query error: %v", err)
		http.Error(w, `{"error":"failed to fetch campaigns"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	campaigns := []map[string]interface{}{}
	for rows.Next() {
		var id, name, subject, status, profileName, vendorType, listName string
		var sentCount, openCount, clickCount int
		var createdAt time.Time
		var scheduledAt, completedAt sql.NullTime

		rows.Scan(&id, &name, &subject, &status,
			&sentCount, &openCount, &clickCount,
			&createdAt, &scheduledAt, &completedAt,
			&profileName, &vendorType, &listName)

		campaign := map[string]interface{}{
			"id":           id,
			"name":         name,
			"subject":      subject,
			"status":       status,
			"sent_count":   sentCount,
			"open_count":   openCount,
			"click_count":  clickCount,
			"open_rate":    calcRate(openCount, sentCount),
			"click_rate":   calcRate(clickCount, sentCount),
			"created_at":   createdAt,
			"profile_name": profileName,
			"vendor":       vendorType,
			"list_name":    listName,
		}

		if scheduledAt.Valid {
			campaign["scheduled_at"] = scheduledAt.Time
		}
		if completedAt.Valid {
			campaign["completed_at"] = completedAt.Time
		}

		campaigns = append(campaigns, campaign)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(NewPaginatedResponse(campaigns, pag, total))
}

// HandleGetCampaign returns a single campaign with full details
func (cb *CampaignBuilder) HandleGetCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	orgID := getOrganizationID(r)
	
	var campaign Campaign
	var listID, segmentID, profileID, replyEmail sql.NullString
	var scheduledAt, startedAt, completedAt sql.NullTime
	var maxRecipients sql.NullInt64
	var listIDsJSON, suppressionListIDsJSON, suppressionSegmentIDsJSON, espQuotasJSON sql.NullString
	
	err := cb.db.QueryRowContext(ctx, `
		SELECT c.id, c.name, c.subject, COALESCE(c.preview_text, ''),
			   COALESCE(c.html_content, ''), COALESCE(c.plain_content, ''),
			   c.list_id, c.segment_id, c.sending_profile_id,
			   c.from_name, c.from_email, c.reply_to,
			   COALESCE(c.send_type, 'instant'), c.scheduled_at,
			   COALESCE(c.throttle_speed, 'gentle'), c.max_recipients,
			   c.status, COALESCE(c.sent_count, 0), COALESCE(c.open_count, 0),
			   COALESCE(c.click_count, 0), COALESCE(c.bounce_count, 0),
			   COALESCE(c.complaint_count, 0), COALESCE(c.unsubscribe_count, 0),
			   c.created_at, c.updated_at, c.started_at, c.completed_at,
			   COALESCE(p.name, ''), COALESCE(p.vendor_type, ''),
			   COALESCE(l.name, ''), COALESCE(s.name, ''),
			   COALESCE(c.list_ids::text, '[]'),
			   COALESCE(c.suppression_list_ids::text, '[]'),
			   COALESCE(c.suppression_segment_ids::text, '[]'),
			   COALESCE(c.esp_quotas::text, '[]'),
			   COALESCE(c.throttle_rate_per_minute, 0),
			   COALESCE(c.throttle_duration_hours, 0)
		FROM mailing_campaigns c
		LEFT JOIN mailing_sending_profiles p ON c.sending_profile_id = p.id
		LEFT JOIN mailing_lists l ON c.list_id = l.id
		LEFT JOIN mailing_segments s ON c.segment_id = s.id
		WHERE c.id = $1 AND c.organization_id = $2
	`, id, orgID).Scan(
		&campaign.ID, &campaign.Name, &campaign.Subject, &campaign.PreviewText,
		&campaign.HTMLContent, &campaign.TextContent,
		&listID, &segmentID, &profileID,
		&campaign.FromName, &campaign.FromEmail, &replyEmail,
		&campaign.SendType, &scheduledAt,
		&campaign.ThrottleSpeed, &maxRecipients,
		&campaign.Status, &campaign.SentCount, &campaign.OpenCount,
		&campaign.ClickCount, &campaign.BounceCount,
		&campaign.ComplaintCount, &campaign.UnsubscribeCount,
		&campaign.CreatedAt, &campaign.UpdatedAt, &startedAt, &completedAt,
		&campaign.ProfileName, &campaign.ProfileVendor,
		&campaign.ListName, &campaign.SegmentName,
		&listIDsJSON, &suppressionListIDsJSON, &suppressionSegmentIDsJSON, &espQuotasJSON,
		&campaign.ThrottleRatePerMinute, &campaign.ThrottleDurationHours,
	)
	
	if err != nil {
		log.Printf("Error fetching campaign %s: %v", id, err)
		http.Error(w, `{"error":"campaign not found"}`, http.StatusNotFound)
		return
	}
	
	if listID.Valid {
		campaign.ListID = &listID.String
	}
	if segmentID.Valid {
		campaign.SegmentID = &segmentID.String
	}
	if profileID.Valid {
		campaign.SendingProfileID = &profileID.String
	}
	if replyEmail.Valid {
		campaign.ReplyEmail = replyEmail.String
	}
	if scheduledAt.Valid {
		campaign.ScheduledAt = &scheduledAt.Time
	}
	if startedAt.Valid {
		campaign.StartedAt = &startedAt.Time
	}
	if completedAt.Valid {
		campaign.CompletedAt = &completedAt.Time
	}
	if maxRecipients.Valid {
		max := int(maxRecipients.Int64)
		campaign.MaxRecipients = &max
	}
	
	// Parse JSONB array fields
	if listIDsJSON.Valid && listIDsJSON.String != "" && listIDsJSON.String != "[]" {
		json.Unmarshal([]byte(listIDsJSON.String), &campaign.ListIDs)
	}
	if suppressionListIDsJSON.Valid && suppressionListIDsJSON.String != "" && suppressionListIDsJSON.String != "[]" {
		json.Unmarshal([]byte(suppressionListIDsJSON.String), &campaign.SuppressionListIDs)
	}
	if suppressionSegmentIDsJSON.Valid && suppressionSegmentIDsJSON.String != "" && suppressionSegmentIDsJSON.String != "[]" {
		json.Unmarshal([]byte(suppressionSegmentIDsJSON.String), &campaign.SuppressionSegmentIDs)
	}
	if espQuotasJSON.Valid && espQuotasJSON.String != "" && espQuotasJSON.String != "[]" {
		json.Unmarshal([]byte(espQuotasJSON.String), &campaign.ESPQuotas)
	}
	
	// Build segment_ids array from segment_id for backward compatibility
	// The frontend expects segment_ids array
	if campaign.SegmentID != nil && *campaign.SegmentID != "" {
		campaign.SegmentIDs = []string{*campaign.SegmentID}
	}
	
	// Get total recipients
	campaign.TotalRecipients = cb.getAudienceCount(ctx, campaign.ListID, campaign.SegmentID)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(campaign)
}

// HandleUpdateCampaign updates a campaign (only if in draft or scheduled status, and not in edit lock window)
func (cb *CampaignBuilder) HandleUpdateCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	
	// Check status and scheduled time
	var currentStatus string
	var scheduledAt sql.NullTime
	err := cb.db.QueryRowContext(ctx, `
		SELECT status, scheduled_at FROM mailing_campaigns WHERE id = $1
	`, id).Scan(&currentStatus, &scheduledAt)
	
	if err != nil {
		http.Error(w, `{"error":"campaign not found"}`, http.StatusNotFound)
		return
	}
	
	// Can only edit draft or scheduled campaigns
	if currentStatus != "draft" && currentStatus != "scheduled" {
		http.Error(w, fmt.Sprintf(`{"error":"cannot update campaign in '%s' status"}`, currentStatus), http.StatusBadRequest)
		return
	}
	
	// For scheduled campaigns, check if we're in the edit lock window
	if currentStatus == "scheduled" && scheduledAt.Valid {
		editLockTime := scheduledAt.Time.Add(-time.Duration(MinPreparationMinutes) * time.Minute)
		if time.Now().After(editLockTime) {
			http.Error(w, fmt.Sprintf(
				`{"error":"campaign is locked for sending preparation","scheduled_at":"%s","edit_locked_at":"%s","message":"Cannot edit within %d minutes of scheduled send time. You can still cancel or pause the campaign."}`,
				scheduledAt.Time.Format(time.RFC3339),
				editLockTime.Format(time.RFC3339),
				MinPreparationMinutes,
			), http.StatusConflict)
			return
		}
	}
	
	var input CampaignInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	
	// Validate sending profile if being changed
	if input.SendingProfileID != nil && *input.SendingProfileID != "" {
		var hasCredentials bool
		err := cb.db.QueryRowContext(ctx, `
			SELECT ((api_key IS NOT NULL AND api_key != '') OR (smtp_host IS NOT NULL AND smtp_host != '')) as has_credentials
			FROM mailing_sending_profiles WHERE id = $1 AND status = 'active'
		`, *input.SendingProfileID).Scan(&hasCredentials)
		if err != nil {
			http.Error(w, `{"error":"sending profile not found or inactive"}`, http.StatusBadRequest)
			return
		}
		if !hasCredentials {
			http.Error(w, `{"error":"sending profile is not configured - credentials are missing (need API key or SMTP host)"}`, http.StatusBadRequest)
			return
		}
	}
	
	// Build update query dynamically
	updates := []string{}
	args := []interface{}{}
	argIdx := 1
	
	if input.Name != "" {
		updates = append(updates, fmt.Sprintf("name = $%d", argIdx))
		args = append(args, input.Name)
		argIdx++
	}
	if input.Subject != "" {
		updates = append(updates, fmt.Sprintf("subject = $%d", argIdx))
		args = append(args, input.Subject)
		argIdx++
	}
	if input.HTMLContent != "" {
		updates = append(updates, fmt.Sprintf("html_content = $%d", argIdx))
		args = append(args, input.HTMLContent)
		argIdx++
		if input.TextContent == "" {
			input.TextContent = stripHTML(input.HTMLContent)
		}
		updates = append(updates, fmt.Sprintf("plain_content = $%d", argIdx))
		args = append(args, input.TextContent)
		argIdx++
	}
	if input.SendingProfileID != nil {
		updates = append(updates, fmt.Sprintf("sending_profile_id = $%d", argIdx))
		args = append(args, *input.SendingProfileID)
		argIdx++
	}
	if input.ListID != nil {
		updates = append(updates, fmt.Sprintf("list_id = $%d", argIdx))
		args = append(args, *input.ListID)
		argIdx++
	}
	if input.SegmentID != nil {
		updates = append(updates, fmt.Sprintf("segment_id = $%d", argIdx))
		args = append(args, *input.SegmentID)
		argIdx++
	}
	// Handle segment_ids array - use first segment as primary segment_id
	if len(input.SegmentIDs) > 0 {
		updates = append(updates, fmt.Sprintf("segment_id = $%d", argIdx))
		args = append(args, input.SegmentIDs[0])
		argIdx++
	}
	// Handle list_ids array
	if len(input.ListIDs) > 0 {
		listIDsJSON, _ := json.Marshal(input.ListIDs)
		updates = append(updates, fmt.Sprintf("list_ids = $%d", argIdx))
		args = append(args, string(listIDsJSON))
		argIdx++
		// Also set primary list_id
		updates = append(updates, fmt.Sprintf("list_id = $%d", argIdx))
		args = append(args, input.ListIDs[0])
		argIdx++
	}
	// Handle suppression_list_ids array
	if len(input.SuppressionListIDs) > 0 {
		suppressionIDsJSON, _ := json.Marshal(input.SuppressionListIDs)
		updates = append(updates, fmt.Sprintf("suppression_list_ids = $%d", argIdx))
		args = append(args, string(suppressionIDsJSON))
		argIdx++
	}
	// Handle suppression_segment_ids array
	if len(input.SuppressionSegmentIDs) > 0 {
		suppressionSegmentIDsJSON, _ := json.Marshal(input.SuppressionSegmentIDs)
		updates = append(updates, fmt.Sprintf("suppression_segment_ids = $%d", argIdx))
		args = append(args, string(suppressionSegmentIDsJSON))
		argIdx++
	}
	if input.ThrottleSpeed != "" {
		updates = append(updates, fmt.Sprintf("throttle_speed = $%d", argIdx))
		args = append(args, input.ThrottleSpeed)
		argIdx++
	}
	if input.SendType != "" {
		updates = append(updates, fmt.Sprintf("send_type = $%d", argIdx))
		args = append(args, input.SendType)
		argIdx++
	}
	
	// Handle scheduled_at - this is the key fix for scheduling campaigns
	// When scheduled_at is provided and in the future, update status to 'scheduled'
	// When scheduled_at is cleared (nil or zero), revert to 'draft'
	newStatus := ""
	if input.ScheduledAt != nil {
		if !input.ScheduledAt.IsZero() && input.ScheduledAt.After(time.Now()) {
			// Schedule is set and in the future
			updates = append(updates, fmt.Sprintf("scheduled_at = $%d", argIdx))
			args = append(args, *input.ScheduledAt)
			argIdx++
			newStatus = "scheduled"
			log.Printf("Campaign %s scheduled for %s", id, input.ScheduledAt.Format(time.RFC3339))
		} else if input.ScheduledAt.IsZero() {
			// Schedule is being cleared - set to NULL and revert to draft
			updates = append(updates, fmt.Sprintf("scheduled_at = $%d", argIdx))
			args = append(args, nil)
			argIdx++
			newStatus = "draft"
			log.Printf("Campaign %s schedule cleared, reverting to draft", id)
		}
	}
	
	// Update status if schedule changed
	if newStatus != "" {
		updates = append(updates, fmt.Sprintf("status = $%d", argIdx))
		args = append(args, newStatus)
		argIdx++
	}
	
	// Handle preview_text/preheader
	if input.PreviewText != "" {
		updates = append(updates, fmt.Sprintf("preview_text = $%d", argIdx))
		args = append(args, input.PreviewText)
		argIdx++
	}
	
	// Handle from_name and from_email overrides
	if input.FromName != nil && *input.FromName != "" {
		updates = append(updates, fmt.Sprintf("from_name = $%d", argIdx))
		args = append(args, *input.FromName)
		argIdx++
	}
	if input.FromEmail != nil && *input.FromEmail != "" {
		updates = append(updates, fmt.Sprintf("from_email = $%d", argIdx))
		args = append(args, *input.FromEmail)
		argIdx++
	}
	
	updates = append(updates, "updated_at = NOW()")
	
	query := fmt.Sprintf("UPDATE mailing_campaigns SET %s WHERE id = $%d",
		strings.Join(updates, ", "), argIdx)
	args = append(args, id)
	
	_, execErr := cb.db.ExecContext(ctx, query, args...)
	if execErr != nil {
		log.Printf("Error updating campaign %s: %v", id, execErr)
		http.Error(w, `{"error":"failed to update campaign"}`, http.StatusInternalServerError)
		return
	}
	
	// Determine final status for response
	responseStatus := currentStatus
	if newStatus != "" {
		responseStatus = newStatus
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":      id,
		"status":  responseStatus,
		"message": "Campaign updated successfully",
	})
}

// HandleDeleteCampaign soft-deletes a campaign
func (cb *CampaignBuilder) HandleDeleteCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	
	_, err := cb.db.ExecContext(ctx, `
		UPDATE mailing_campaigns SET status = 'deleted', updated_at = NOW() WHERE id = $1
	`, id)
	
	if err != nil {
		http.Error(w, `{"error":"failed to delete campaign"}`, http.StatusInternalServerError)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":      id,
		"message": "Campaign deleted",
	})
}
