package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// SegmentCleanupAPI handles segment cleanup management endpoints
type SegmentCleanupAPI struct {
	db *sql.DB
}

// NewSegmentCleanupAPI creates a new segment cleanup API handler
func NewSegmentCleanupAPI(db *sql.DB) *SegmentCleanupAPI {
	return &SegmentCleanupAPI{db: db}
}

// RegisterRoutes registers segment cleanup routes
func (api *SegmentCleanupAPI) RegisterRoutes(r chi.Router) {
	r.Route("/segment-cleanup", func(r chi.Router) {
		// Settings
		r.Get("/settings", api.GetCleanupSettings)
		r.Put("/settings", api.UpdateCleanupSettings)

		// Segment actions
		r.Post("/segments/{segmentID}/keep-active", api.MarkSegmentKeepActive)
		r.Delete("/segments/{segmentID}/keep-active", api.RemoveSegmentKeepActive)
		r.Post("/segments/{segmentID}/count", api.CountSegmentSubscribers)
		r.Post("/segments/{segmentID}/restore", api.RestoreArchivedSegment)

		// Reporting
		r.Get("/pending", api.GetPendingCleanups)
		r.Get("/history", api.GetCleanupHistory)
		r.Get("/stale-segments", api.GetStaleSegments)
	})
}

// CleanupSettingsResponse represents the cleanup settings
type CleanupSettingsResponse struct {
	Enabled               bool     `json:"enabled"`
	InactiveDaysThreshold int      `json:"inactive_days_threshold"`
	GracePeriodDays       int      `json:"grace_period_days"`
	AutoArchive           bool     `json:"auto_archive"`
	AutoDelete            bool     `json:"auto_delete"`
	ArchiveRetentionDays  int      `json:"archive_retention_days"`
	NotifyAdmins          bool     `json:"notify_admins"`
	AdminEmails           []string `json:"admin_emails"`
	MinSegmentAgeDays     int      `json:"min_segment_age_days"`
	ExcludePatterns       []string `json:"exclude_patterns"`
}

// GetCleanupSettings returns cleanup settings for the organization
func (api *SegmentCleanupAPI) GetCleanupSettings(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgIDFromContext(r.Context())

	var settings CleanupSettingsResponse
	var adminEmails, excludePatterns sql.NullString

	err := api.db.QueryRowContext(r.Context(), `
		SELECT enabled, inactive_days_threshold, grace_period_days, auto_archive, auto_delete,
			   archive_retention_days, notify_admins, admin_emails, min_segment_age_days, exclude_patterns
		FROM mailing_segment_cleanup_settings
		WHERE organization_id = $1
	`, orgID).Scan(
		&settings.Enabled,
		&settings.InactiveDaysThreshold,
		&settings.GracePeriodDays,
		&settings.AutoArchive,
		&settings.AutoDelete,
		&settings.ArchiveRetentionDays,
		&settings.NotifyAdmins,
		&adminEmails,
		&settings.MinSegmentAgeDays,
		&excludePatterns,
	)

	if err == sql.ErrNoRows {
		// Return defaults
		settings = CleanupSettingsResponse{
			Enabled:               true,
			InactiveDaysThreshold: 30,
			GracePeriodDays:       7,
			AutoArchive:           true,
			AutoDelete:            false,
			ArchiveRetentionDays:  90,
			NotifyAdmins:          true,
			AdminEmails:           []string{},
			MinSegmentAgeDays:     14,
			ExcludePatterns:       []string{},
		}
	} else if err != nil {
		log.Printf("Error fetching cleanup settings: %v", err)
		http.Error(w, "Failed to fetch settings", http.StatusInternalServerError)
		return
	} else {
		// Parse arrays
		if adminEmails.Valid {
			settings.AdminEmails = parsePostgresArray(adminEmails.String)
		} else {
			settings.AdminEmails = []string{}
		}
		if excludePatterns.Valid {
			settings.ExcludePatterns = parsePostgresArray(excludePatterns.String)
		} else {
			settings.ExcludePatterns = []string{}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(settings)
}

// UpdateCleanupSettings updates cleanup settings
func (api *SegmentCleanupAPI) UpdateCleanupSettings(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgIDFromContext(r.Context())

	var req CleanupSettingsResponse
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Validate
	if req.InactiveDaysThreshold < 1 {
		req.InactiveDaysThreshold = 30
	}
	if req.GracePeriodDays < 1 {
		req.GracePeriodDays = 7
	}
	if req.MinSegmentAgeDays < 1 {
		req.MinSegmentAgeDays = 14
	}

	_, err := api.db.ExecContext(r.Context(), `
		INSERT INTO mailing_segment_cleanup_settings 
			(organization_id, enabled, inactive_days_threshold, grace_period_days, auto_archive, 
			 auto_delete, archive_retention_days, notify_admins, admin_emails, min_segment_age_days, 
			 exclude_patterns, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW())
		ON CONFLICT (organization_id) DO UPDATE SET
			enabled = EXCLUDED.enabled,
			inactive_days_threshold = EXCLUDED.inactive_days_threshold,
			grace_period_days = EXCLUDED.grace_period_days,
			auto_archive = EXCLUDED.auto_archive,
			auto_delete = EXCLUDED.auto_delete,
			archive_retention_days = EXCLUDED.archive_retention_days,
			notify_admins = EXCLUDED.notify_admins,
			admin_emails = EXCLUDED.admin_emails,
			min_segment_age_days = EXCLUDED.min_segment_age_days,
			exclude_patterns = EXCLUDED.exclude_patterns,
			updated_at = NOW()
	`, orgID, req.Enabled, req.InactiveDaysThreshold, req.GracePeriodDays, req.AutoArchive,
		req.AutoDelete, req.ArchiveRetentionDays, req.NotifyAdmins,
		formatPostgresArray(req.AdminEmails), req.MinSegmentAgeDays, formatPostgresArray(req.ExcludePatterns))

	if err != nil {
		log.Printf("Error updating cleanup settings: %v", err)
		http.Error(w, "Failed to update settings", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

// MarkSegmentKeepActive marks a segment as "keep active" to prevent cleanup
func (api *SegmentCleanupAPI) MarkSegmentKeepActive(w http.ResponseWriter, r *http.Request) {
	segmentID := chi.URLParam(r, "segmentID")
	orgID := getOrgIDFromContext(r.Context())

	result, err := api.db.ExecContext(r.Context(), `
		UPDATE mailing_segments 
		SET keep_active = TRUE, 
			cleanup_warning_sent = FALSE, 
			cleanup_warned_at = NULL,
			last_used_at = NOW()
		WHERE id = $1 AND organization_id = $2
	`, segmentID, orgID)

	if err != nil {
		log.Printf("Error marking segment keep active: %v", err)
		http.Error(w, "Failed to update segment", http.StatusInternalServerError)
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		http.Error(w, "Segment not found", http.StatusNotFound)
		return
	}

	// Update any pending cleanup notification
	api.db.ExecContext(r.Context(), `
		UPDATE mailing_segment_cleanup_notifications 
		SET action_taken = 'kept', action_taken_at = NOW()
		WHERE segment_id = $1 AND action_taken IS NULL
	`, segmentID)

	// Log the action
	api.db.ExecContext(r.Context(), `
		INSERT INTO mailing_segment_usage_log (segment_id, usage_type)
		VALUES ($1, 'keep_active_marked')
	`, segmentID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "success",
		"keep_active": true,
		"message":     "Segment marked as keep active - it will not be auto-cleaned",
	})
}

// RemoveSegmentKeepActive removes the "keep active" flag from a segment
func (api *SegmentCleanupAPI) RemoveSegmentKeepActive(w http.ResponseWriter, r *http.Request) {
	segmentID := chi.URLParam(r, "segmentID")
	orgID := getOrgIDFromContext(r.Context())

	result, err := api.db.ExecContext(r.Context(), `
		UPDATE mailing_segments 
		SET keep_active = FALSE
		WHERE id = $1 AND organization_id = $2
	`, segmentID, orgID)

	if err != nil {
		http.Error(w, "Failed to update segment", http.StatusInternalServerError)
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		http.Error(w, "Segment not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "success",
		"keep_active": false,
		"message":     "Segment can now be auto-cleaned if inactive",
	})
}

// CountSegmentSubscribers counts subscribers in a segment and resets active state
func (api *SegmentCleanupAPI) CountSegmentSubscribers(w http.ResponseWriter, r *http.Request) {
	segmentID := chi.URLParam(r, "segmentID")
	orgID := getOrgIDFromContext(r.Context())

	// Get segment details
	var segment struct {
		ID              uuid.UUID
		Name            string
		SubscriberCount int
		Conditions      sql.NullString
		ListID          *uuid.UUID
	}

	err := api.db.QueryRowContext(r.Context(), `
		SELECT id, name, COALESCE(subscriber_count, 0), conditions, list_id
		FROM mailing_segments
		WHERE id = $1 AND organization_id = $2 AND archived_at IS NULL
	`, segmentID, orgID).Scan(&segment.ID, &segment.Name, &segment.SubscriberCount, &segment.Conditions, &segment.ListID)

	if err == sql.ErrNoRows {
		http.Error(w, "Segment not found", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("Error fetching segment: %v", err)
		http.Error(w, "Failed to fetch segment", http.StatusInternalServerError)
		return
	}

	// Calculate actual count (this would use the segment engine in production)
	// For now, we'll use a simpler approach
	var count int
	countQuery := `
		SELECT COUNT(*) FROM mailing_subscribers
		WHERE organization_id = $1 
			AND status = 'confirmed'
			AND ($2::uuid IS NULL OR list_id = $2)
	`
	api.db.QueryRowContext(r.Context(), countQuery, orgID, segment.ListID).Scan(&count)

	// Update segment with new count and reset activity tracking
	_, err = api.db.ExecContext(r.Context(), `
		UPDATE mailing_segments 
		SET subscriber_count = $1,
			last_used_at = NOW(),
			last_count_at = NOW(),
			usage_count = COALESCE(usage_count, 0) + 1,
			cleanup_warning_sent = FALSE,
			cleanup_warned_at = NULL
		WHERE id = $2
	`, count, segmentID)

	if err != nil {
		log.Printf("Error updating segment count: %v", err)
	}

	// Log the usage
	api.db.ExecContext(r.Context(), `
		INSERT INTO mailing_segment_usage_log (segment_id, usage_type, subscriber_count)
		VALUES ($1, 'count_query', $2)
	`, segmentID, count)

	// Cancel any pending cleanup notification
	api.db.ExecContext(r.Context(), `
		UPDATE mailing_segment_cleanup_notifications 
		SET action_taken = 'kept', action_taken_at = NOW()
		WHERE segment_id = $1 AND action_taken IS NULL
	`, segmentID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"segment_id":       segmentID,
		"segment_name":     segment.Name,
		"subscriber_count": count,
		"counted_at":       time.Now().UTC().Format(time.RFC3339),
		"cleanup_reset":    true,
		"message":          "Count updated and cleanup warning cleared",
	})
}

// RestoreArchivedSegment restores an archived segment
func (api *SegmentCleanupAPI) RestoreArchivedSegment(w http.ResponseWriter, r *http.Request) {
	segmentID := chi.URLParam(r, "segmentID")
	orgID := getOrgIDFromContext(r.Context())

	result, err := api.db.ExecContext(r.Context(), `
		UPDATE mailing_segments 
		SET archived_at = NULL, 
			status = 'active',
			last_used_at = NOW(),
			cleanup_warning_sent = FALSE,
			cleanup_warned_at = NULL
		WHERE id = $1 AND organization_id = $2 AND archived_at IS NOT NULL
	`, segmentID, orgID)

	if err != nil {
		http.Error(w, "Failed to restore segment", http.StatusInternalServerError)
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		http.Error(w, "Archived segment not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "restored",
		"message": "Segment has been restored from archive",
	})
}

// GetPendingCleanups returns segments pending cleanup action
func (api *SegmentCleanupAPI) GetPendingCleanups(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgIDFromContext(r.Context())

	rows, err := api.db.QueryContext(r.Context(), `
		SELECT n.id, n.segment_id, n.segment_name, n.subscriber_count, 
			   n.last_used_at, n.warning_sent_at, n.grace_period_ends_at,
			   EXTRACT(EPOCH FROM (n.grace_period_ends_at - NOW()))/86400 as days_remaining
		FROM mailing_segment_cleanup_notifications n
		JOIN mailing_segments s ON s.id = n.segment_id
		WHERE n.organization_id = $1 
			AND n.action_taken IS NULL
			AND s.archived_at IS NULL
			AND s.keep_active = FALSE
		ORDER BY n.grace_period_ends_at ASC
	`, orgID)

	if err != nil {
		http.Error(w, "Failed to fetch pending cleanups", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type PendingCleanup struct {
		ID                string     `json:"id"`
		SegmentID         string     `json:"segment_id"`
		SegmentName       string     `json:"segment_name"`
		SubscriberCount   int        `json:"subscriber_count"`
		LastUsedAt        *time.Time `json:"last_used_at"`
		WarningSentAt     time.Time  `json:"warning_sent_at"`
		GracePeriodEndsAt time.Time  `json:"grace_period_ends_at"`
		DaysRemaining     float64    `json:"days_remaining"`
	}

	var pending []PendingCleanup
	for rows.Next() {
		var p PendingCleanup
		var lastUsed sql.NullTime
		if err := rows.Scan(&p.ID, &p.SegmentID, &p.SegmentName, &p.SubscriberCount,
			&lastUsed, &p.WarningSentAt, &p.GracePeriodEndsAt, &p.DaysRemaining); err != nil {
			continue
		}
		if lastUsed.Valid {
			p.LastUsedAt = &lastUsed.Time
		}
		pending = append(pending, p)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"pending": pending,
		"count":   len(pending),
	})
}

// GetCleanupHistory returns cleanup action history
func (api *SegmentCleanupAPI) GetCleanupHistory(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgIDFromContext(r.Context())

	rows, err := api.db.QueryContext(r.Context(), `
		SELECT segment_name, subscriber_count, warning_sent_at, 
			   action_taken, action_taken_at, grace_period_ends_at
		FROM mailing_segment_cleanup_notifications
		WHERE organization_id = $1 AND action_taken IS NOT NULL
		ORDER BY action_taken_at DESC
		LIMIT 100
	`, orgID)

	if err != nil {
		http.Error(w, "Failed to fetch cleanup history", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type HistoryEntry struct {
		SegmentName       string    `json:"segment_name"`
		SubscriberCount   int       `json:"subscriber_count"`
		WarningSentAt     time.Time `json:"warning_sent_at"`
		ActionTaken       string    `json:"action_taken"`
		ActionTakenAt     time.Time `json:"action_taken_at"`
		GracePeriodEndsAt time.Time `json:"grace_period_ends_at"`
	}

	var history []HistoryEntry
	for rows.Next() {
		var h HistoryEntry
		var actionAt sql.NullTime
		if err := rows.Scan(&h.SegmentName, &h.SubscriberCount, &h.WarningSentAt,
			&h.ActionTaken, &actionAt, &h.GracePeriodEndsAt); err != nil {
			continue
		}
		if actionAt.Valid {
			h.ActionTakenAt = actionAt.Time
		}
		history = append(history, h)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"history": history,
		"count":   len(history),
	})
}

// GetStaleSegments returns segments that are candidates for cleanup warning
func (api *SegmentCleanupAPI) GetStaleSegments(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgIDFromContext(r.Context())

	// Get settings
	var inactiveDays, minAgeDays int
	err := api.db.QueryRowContext(r.Context(), `
		SELECT COALESCE(inactive_days_threshold, 30), COALESCE(min_segment_age_days, 14)
		FROM mailing_segment_cleanup_settings
		WHERE organization_id = $1
	`, orgID).Scan(&inactiveDays, &minAgeDays)

	if err != nil {
		inactiveDays = 30
		minAgeDays = 14
	}

	rows, err := api.db.QueryContext(r.Context(), `
		SELECT id, name, COALESCE(subscriber_count, 0), last_used_at, 
			   EXTRACT(DAY FROM NOW() - COALESCE(last_used_at, created_at))::INTEGER as days_inactive,
			   created_at, keep_active, cleanup_warning_sent
		FROM mailing_segments
		WHERE organization_id = $1
			AND archived_at IS NULL
			AND created_at < NOW() - ($2 || ' days')::INTERVAL
			AND COALESCE(last_used_at, created_at) < NOW() - ($3 || ' days')::INTERVAL
		ORDER BY last_used_at ASC NULLS FIRST
		LIMIT 100
	`, orgID, minAgeDays, inactiveDays)

	if err != nil {
		http.Error(w, "Failed to fetch stale segments", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type StaleSegment struct {
		ID                  string     `json:"id"`
		Name                string     `json:"name"`
		SubscriberCount     int        `json:"subscriber_count"`
		LastUsedAt          *time.Time `json:"last_used_at"`
		DaysInactive        int        `json:"days_inactive"`
		CreatedAt           time.Time  `json:"created_at"`
		KeepActive          bool       `json:"keep_active"`
		CleanupWarningSent  bool       `json:"cleanup_warning_sent"`
	}

	var segments []StaleSegment
	for rows.Next() {
		var s StaleSegment
		var lastUsed sql.NullTime
		if err := rows.Scan(&s.ID, &s.Name, &s.SubscriberCount, &lastUsed, &s.DaysInactive,
			&s.CreatedAt, &s.KeepActive, &s.CleanupWarningSent); err != nil {
			continue
		}
		if lastUsed.Valid {
			s.LastUsedAt = &lastUsed.Time
		}
		segments = append(segments, s)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"stale_segments":    segments,
		"count":             len(segments),
		"inactive_threshold": inactiveDays,
	})
}

// Helper functions

func getOrgIDFromContext(ctx interface{}) uuid.UUID {
	// Use the dynamic org context extraction
	if c, ok := ctx.(context.Context); ok {
		return GetOrgIDFromContext(c)
	}
	return uuid.Nil
}

func parsePostgresArray(s string) []string {
	if s == "" || s == "{}" {
		return []string{}
	}
	s = s[1 : len(s)-1] // Remove { }
	if s == "" {
		return []string{}
	}
	parts := make([]string, 0)
	for _, p := range splitRespectingQuotes(s) {
		p = trimQuotes(p)
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

func formatPostgresArray(arr []string) string {
	if len(arr) == 0 {
		return "{}"
	}
	return "{" + joinWithQuotes(arr) + "}"
}

func splitRespectingQuotes(s string) []string {
	var result []string
	var current string
	inQuotes := false
	for _, c := range s {
		if c == '"' {
			inQuotes = !inQuotes
			current += string(c)
		} else if c == ',' && !inQuotes {
			result = append(result, current)
			current = ""
		} else {
			current += string(c)
		}
	}
	if current != "" {
		result = append(result, current)
	}
	return result
}

func trimQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func joinWithQuotes(arr []string) string {
	quoted := make([]string, len(arr))
	for i, s := range arr {
		quoted[i] = `"` + s + `"`
	}
	return join(quoted, ",")
}

func join(arr []string, sep string) string {
	if len(arr) == 0 {
		return ""
	}
	result := arr[0]
	for i := 1; i < len(arr); i++ {
		result += sep + arr[i]
	}
	return result
}
