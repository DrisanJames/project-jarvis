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
)

// VersionCampaignBuilder is bumped on every schema/behavior change to the
// /api/mailing/campaigns endpoints so clients can detect drift. Per
// workspace rule testing.mdc.
//
// History:
//   1.0 (2026-05-08) — Phase B remediation:
//     * Open/click rates now use `delivered` denominator only (was max(delivered, sent))
//     * Removed combined `bounce_rate` and `bounces` from /stats; hard/soft only
//       per .cursor/rules/bounce-metrics.mdc
//     * Replaced per-row N+1 list/segment name lookup with two batched queries
//     * /stats accepts ?include=domain,hourly to make the heavy aggregations opt-in
//     * /stats returns queue_status histogram (mailing_campaign_queue) so the
//       UI can show "what we're actually mailing" without a separate request
const VersionCampaignBuilder = "1.0"

// HandleListCampaigns lists all campaigns with filtering and pagination
func (cb *CampaignBuilder) HandleListCampaigns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pag := ParsePagination(r, 50, 200)

	orgID := getOrganizationID(r)
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

	// Build data query — return all fields the frontend Campaign interface expects
	query := `
		SELECT c.id, c.name, COALESCE(c.subject,''), c.status,
			   COALESCE(c.total_recipients,0), COALESCE(c.sent_count,0),
			   COALESCE(c.delivered_count,0),
			   COALESCE(c.open_count,0), COALESCE(c.click_count,0),
			   COALESCE(c.bounce_count,0),
			   COALESCE(c.hard_bounce_count,0),
			   COALESCE(c.soft_bounce_count,0),
			   COALESCE(c.complaint_count,0),
			   COALESCE(c.unsubscribe_count,0), COALESCE(c.revenue,0),
			   COALESCE(c.from_name,''), COALESCE(c.from_email,''),
			   COALESCE(c.throttle_speed,''),
			   c.created_at, c.scheduled_at, c.started_at, c.completed_at,
			   COALESCE(p.name, '') as profile_name,
			   COALESCE(p.vendor_type, '') as vendor_type,
			   COALESCE(l.name, '') as list_name,
			   COALESCE(c.list_ids::text, '[]'),
			   COALESCE(c.html_content,''),
			   COALESCE(c.preview_text, ''),
			   COALESCE(seg.name, '') as segment_name,
			   COALESCE(c.pmta_config->'campaign_input'->>'inclusion_segments', '[]')
		FROM mailing_campaigns c
		LEFT JOIN mailing_sending_profiles p ON c.sending_profile_id = p.id
		LEFT JOIN mailing_lists l ON c.list_id = l.id
		LEFT JOIN mailing_segments seg ON c.segment_id = seg.id
	` + whereClause

	query += " ORDER BY c.created_at DESC"
	query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
	args = append(args, pag.Limit, pag.Offset)

	rows, err := cb.db.QueryContext(ctx, query, args...)
	if err != nil {
		log.Printf("[CampaignBuilder] Query error: %v", err)
		http.Error(w, `{"error":"failed to fetch campaigns"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type campaignRow struct {
		entry         map[string]interface{}
		listIDsJSON   string
		segIDsJSON    string
		baseListName  string
		baseSegName   string
	}
	var collected []campaignRow
	listIDSet := map[string]struct{}{}
	segIDSet := map[string]struct{}{}

	for rows.Next() {
		var id, name, subject, status string
		var totalRecipients, sentCount, deliveredCount, openCount, clickCount int
		var bounceCount, hardBounceCount, softBounceCount, complaintCount, unsubscribeCount int
		var revenue float64
		var fromName, fromEmail, throttleSpeed string
		var profileName, vendorType, listName string
		var listIDsJSON, htmlPreview string
		var previewText, segmentName, inclusionSegmentsJSON string
		var createdAt time.Time
		var scheduledAt, startedAt, completedAt sql.NullTime

		if scanErr := rows.Scan(&id, &name, &subject, &status,
			&totalRecipients, &sentCount, &deliveredCount,
			&openCount, &clickCount,
			&bounceCount, &hardBounceCount, &softBounceCount, &complaintCount,
			&unsubscribeCount, &revenue,
			&fromName, &fromEmail,
			&throttleSpeed,
			&createdAt, &scheduledAt, &startedAt, &completedAt,
			&profileName, &vendorType, &listName,
			&listIDsJSON, &htmlPreview,
			&previewText, &segmentName, &inclusionSegmentsJSON); scanErr != nil {
			log.Printf("[CampaignBuilder] List scan error: %v", scanErr)
			continue
		}

		// Phase B: open/click rates use the Deliverability convention —
		// denominator = delivered. Falling back to sent_count silently when
		// delivered=0 caused per-screen drift (Dashboard vs Campaign Center
		// vs Analytics could each show a different number for the same
		// campaign).
		entry := map[string]interface{}{
			"id":                id,
			"name":              name,
			"subject":           subject,
			"status":            status,
			"total_recipients":  totalRecipients,
			"sent_count":        sentCount,
			"delivered_count":   deliveredCount,
			"open_count":        openCount,
			"click_count":       clickCount,
			"bounce_count":      bounceCount,
			"hard_bounce_count": hardBounceCount,
			"soft_bounce_count": softBounceCount,
			"complaint_count":   complaintCount,
			"unsubscribe_count": unsubscribeCount,
			"revenue":           revenue,
			"from_name":         fromName,
			"from_email":        fromEmail,
			"throttle_speed":    throttleSpeed,
			"open_rate":         calcRate(openCount, deliveredCount),
			"click_rate":        calcRate(clickCount, deliveredCount),
			"hard_bounce_rate":  calcRate(hardBounceCount, sentCount),
			"soft_bounce_rate":  calcRate(softBounceCount, sentCount),
			"created_at":        createdAt,
			"profile_name":      profileName,
			"vendor":            vendorType,
			"list_name":         listName,
			"html_preview":      htmlPreview,
			"preview_text":      previewText,
		}

		if scheduledAt.Valid {
			entry["scheduled_at"] = scheduledAt.Time
		}
		if startedAt.Valid {
			entry["started_at"] = startedAt.Time
		}
		if completedAt.Valid {
			entry["completed_at"] = completedAt.Time
		}

		// Collect list & segment IDs for batched name lookup below.
		// The previous implementation issued N queries per campaign × M IDs
		// — at 50 campaigns with ~3 ids each that was ~150 round-trips per
		// page load. The new approach is exactly 2 queries regardless of N.
		var listIDs []string
		if listIDsJSON != "" && listIDsJSON != "[]" && listIDsJSON != "null" {
			_ = json.Unmarshal([]byte(listIDsJSON), &listIDs)
			for _, lid := range listIDs {
				if lid != "" {
					listIDSet[lid] = struct{}{}
				}
			}
		}
		var segIDs []string
		if inclusionSegmentsJSON != "" && inclusionSegmentsJSON != "[]" && inclusionSegmentsJSON != "null" {
			_ = json.Unmarshal([]byte(inclusionSegmentsJSON), &segIDs)
			for _, sid := range segIDs {
				if sid != "" {
					segIDSet[sid] = struct{}{}
				}
			}
		}

		collected = append(collected, campaignRow{
			entry:        entry,
			listIDsJSON:  listIDsJSON,
			segIDsJSON:   inclusionSegmentsJSON,
			baseListName: listName,
			baseSegName:  segmentName,
		})
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		log.Printf("[CampaignBuilder] List iter error: %v", rowsErr)
	}

	listNameMap := batchFetchNames(ctx, cb.db, "mailing_lists", listIDSet)
	segNameMap := batchFetchNames(ctx, cb.db, "mailing_segments", segIDSet)

	campaigns := make([]map[string]interface{}, 0, len(collected))
	for _, cr := range collected {
		var listNames []string
		if cr.baseListName != "" {
			listNames = append(listNames, cr.baseListName)
		}
		if cr.listIDsJSON != "" && cr.listIDsJSON != "[]" && cr.listIDsJSON != "null" {
			var ids []string
			if err := json.Unmarshal([]byte(cr.listIDsJSON), &ids); err == nil {
				for _, id := range ids {
					if n, ok := listNameMap[id]; ok && n != "" {
						listNames = append(listNames, n)
					}
				}
			}
		}
		var segmentNames []string
		if cr.baseSegName != "" {
			segmentNames = append(segmentNames, cr.baseSegName)
		}
		if cr.segIDsJSON != "" && cr.segIDsJSON != "[]" && cr.segIDsJSON != "null" {
			var ids []string
			if err := json.Unmarshal([]byte(cr.segIDsJSON), &ids); err == nil {
				for _, id := range ids {
					if n, ok := segNameMap[id]; ok && n != "" {
						segmentNames = append(segmentNames, n)
					}
				}
			}
		}
		cr.entry["list_names"] = listNames
		cr.entry["segment_names"] = segmentNames
		campaigns = append(campaigns, cr.entry)
	}

	resp := NewPaginatedResponse(campaigns, pag, total)
	envelope := map[string]interface{}{
		"data":        resp.Data,
		"pagination":  resp.Pagination,
		"api_version": VersionCampaignBuilder,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(envelope)
}

// batchFetchNames fetches `id -> name` for the given table using a single
// `WHERE id = ANY($1::uuid[])` query. Empty input returns an empty map.
// The caller decides what to do when an ID is missing.
func batchFetchNames(ctx context.Context, db *sql.DB, table string, ids map[string]struct{}) map[string]string {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out
	}
	idList := make([]string, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	// table is a fixed string from a small known set — not user input. Safe to
	// interpolate. (Validated by the literal call sites.)
	q := fmt.Sprintf(`SELECT id::text, name FROM %s WHERE id = ANY($1::uuid[])`, table)
	rows, err := db.QueryContext(ctx, q, "{"+strings.Join(idList, ",")+"}")
	if err != nil {
		log.Printf("[batchFetchNames] %s query error: %v", table, err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err == nil {
			out[id] = name
		}
	}
	return out
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
	var executionMode, ispQuotasJSON sql.NullString
	
	err := cb.db.QueryRowContext(ctx, `
		SELECT c.id, c.name, c.subject, COALESCE(c.preview_text, ''),
			   COALESCE(c.html_content, ''), COALESCE(c.plain_content, ''),
			   c.list_id, c.segment_id, c.sending_profile_id,
			   c.from_name, c.from_email, c.reply_to,
			   COALESCE(c.send_type, 'instant'), c.scheduled_at,
			   COALESCE(c.throttle_speed, 'gentle'), c.max_recipients,
			   c.status, COALESCE(c.sent_count, 0), COALESCE(c.open_count, 0),
			   COALESCE(c.click_count, 0), COALESCE(c.bounce_count, 0),
			   CASE WHEN COALESCE(c.hard_bounce_count,0)+COALESCE(c.soft_bounce_count,0)>0 THEN COALESCE(c.hard_bounce_count,0) ELSE COALESCE(c.bounce_count,0) END,
			   CASE WHEN COALESCE(c.hard_bounce_count,0)+COALESCE(c.soft_bounce_count,0)>0 THEN COALESCE(c.soft_bounce_count,0) ELSE 0 END,
			   COALESCE(c.complaint_count, 0), COALESCE(c.unsubscribe_count, 0),
			   c.created_at, c.updated_at, c.started_at, c.completed_at,
			   COALESCE(p.name, ''), COALESCE(p.vendor_type, ''),
			   COALESCE(l.name, ''), COALESCE(s.name, ''),
			   COALESCE(c.list_ids::text, '[]'),
			   COALESCE(c.suppression_list_ids::text, '[]'),
			   COALESCE(c.suppression_segment_ids::text, '[]'),
			   COALESCE(c.esp_quotas::text, '[]'),
			   COALESCE(c.throttle_rate_per_minute, 0),
			   COALESCE(c.throttle_duration_hours, 0),
			   c.execution_mode,
			   c.esp_quotas::text,
			   COALESCE(c.content_locked, FALSE)
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
		&campaign.HardBounceCount, &campaign.SoftBounceCount,
		&campaign.ComplaintCount, &campaign.UnsubscribeCount,
		&campaign.CreatedAt, &campaign.UpdatedAt, &startedAt, &completedAt,
		&campaign.ProfileName, &campaign.ProfileVendor,
		&campaign.ListName, &campaign.SegmentName,
		&listIDsJSON, &suppressionListIDsJSON, &suppressionSegmentIDsJSON, &espQuotasJSON,
		&campaign.ThrottleRatePerMinute, &campaign.ThrottleDurationHours,
		&executionMode, &ispQuotasJSON,
		&campaign.ContentLocked,
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
	if executionMode.Valid {
		campaign.ExecutionMode = executionMode.String
	}
	if ispQuotasJSON.Valid && ispQuotasJSON.String != "" && ispQuotasJSON.String != "{}" {
		campaign.ISPQuotas = json.RawMessage(ispQuotasJSON.String)
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
	if len(input.ISPQuotas) > 0 {
		updates = append(updates, fmt.Sprintf("esp_quotas = $%d", argIdx))
		args = append(args, string(input.ISPQuotas))
		argIdx++
	}
	if input.ExecutionMode != "" {
		updates = append(updates, fmt.Sprintf("execution_mode = $%d", argIdx))
		args = append(args, input.ExecutionMode)
		argIdx++
	}
	if input.ContentLocked != nil {
		updates = append(updates, fmt.Sprintf("content_locked = $%d", argIdx))
		args = append(args, *input.ContentLocked)
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

// HandleGetCampaignVariants returns A/B content variants for a campaign.
func (cb *CampaignBuilder) HandleGetCampaignVariants(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	rows, err := cb.db.QueryContext(ctx, `
		SELECT v.id::text, v.variant_name, COALESCE(v.subject, ''),
		       COALESCE(v.from_name, ''), COALESCE(v.html_content, ''),
		       COALESCE(v.split_percent, 0),
		       COALESCE(v.sent_count, 0), COALESCE(v.open_count, 0),
		       COALESCE(v.click_count, 0), COALESCE(v.is_winner, false)
		FROM mailing_ab_variants v
		JOIN mailing_ab_tests t ON t.id = v.test_id
		WHERE t.campaign_id = $1
		ORDER BY v.variant_name
	`, id)
	if err != nil {
		log.Printf("Error fetching variants for campaign %s: %v", id, err)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]interface{}{})
		return
	}
	defer rows.Close()

	type variantRow struct {
		ID           string  `json:"id"`
		VariantName  string  `json:"variant_name"`
		Subject      string  `json:"subject"`
		FromName     string  `json:"from_name"`
		HTMLContent  string  `json:"html_content"`
		SplitPercent float64 `json:"split_percent"`
		SentCount    int     `json:"sent_count"`
		OpenCount    int     `json:"open_count"`
		ClickCount   int     `json:"click_count"`
		IsWinner     bool    `json:"is_winner"`
		OpenRate     float64 `json:"open_rate"`
		ClickRate    float64 `json:"click_rate"`
	}

	variants := make([]variantRow, 0)
	for rows.Next() {
		var v variantRow
		if err := rows.Scan(&v.ID, &v.VariantName, &v.Subject,
			&v.FromName, &v.HTMLContent, &v.SplitPercent,
			&v.SentCount, &v.OpenCount, &v.ClickCount, &v.IsWinner); err != nil {
			log.Printf("Error scanning variant row: %v", err)
			continue
		}
		if v.SentCount > 0 {
			v.OpenRate = float64(v.OpenCount) / float64(v.SentCount) * 100
			v.ClickRate = float64(v.ClickCount) / float64(v.SentCount) * 100
		}
		variants = append(variants, v)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(variants)
}

// HandleUpdateVariant patches a single A/B variant's content.
// PATCH /campaigns/{id}/variants/{variantId}
func (cb *CampaignBuilder) HandleUpdateVariant(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	variantID := chi.URLParam(r, "variantId")

	var input struct {
		Subject     *string  `json:"subject"`
		FromName    *string  `json:"from_name"`
		HTMLContent *string  `json:"html_content"`
		SplitPercent *float64 `json:"split_percent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}

	var sets []string
	var args []interface{}
	idx := 1
	if input.Subject != nil {
		sets = append(sets, fmt.Sprintf("subject = $%d", idx))
		args = append(args, *input.Subject)
		idx++
	}
	if input.FromName != nil {
		sets = append(sets, fmt.Sprintf("from_name = $%d", idx))
		args = append(args, *input.FromName)
		idx++
	}
	if input.HTMLContent != nil {
		sets = append(sets, fmt.Sprintf("html_content = $%d", idx))
		args = append(args, *input.HTMLContent)
		idx++
	}
	if input.SplitPercent != nil {
		sets = append(sets, fmt.Sprintf("split_percent = $%d", idx))
		args = append(args, *input.SplitPercent)
		idx++
	}
	if len(sets) == 0 {
		http.Error(w, `{"error":"no fields to update"}`, http.StatusBadRequest)
		return
	}

	query := fmt.Sprintf("UPDATE mailing_ab_variants SET %s WHERE id = $%d::uuid",
		strings.Join(sets, ", "), idx)
	args = append(args, variantID)

	result, err := cb.db.ExecContext(ctx, query, args...)
	if err != nil {
		log.Printf("Error updating variant %s: %v", variantID, err)
		http.Error(w, `{"error":"failed to update variant"}`, http.StatusInternalServerError)
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		http.Error(w, `{"error":"variant not found"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":      variantID,
		"updated": len(sets),
		"message": "Variant updated",
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
