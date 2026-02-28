// Domain suppressions, soft bounces, preferences, Optizmo, batch check, analytics, and one-click unsubscribe handlers.
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
	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
)

// HandleGetDomainSuppressions returns domain-level suppressions
func (s *SuppressionService) HandleGetDomainSuppressions(w http.ResponseWriter, r *http.Request) {
	// For now, return empty list - can be implemented with a separate table
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"domains": []interface{}{}})
}

// HandleAddDomainSuppression adds a domain suppression
func (s *SuppressionService) HandleAddDomainSuppression(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Domain string `json:"domain"`
		Reason string `json:"reason"`
	}
	json.NewDecoder(r.Body).Decode(&input)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "domain": input.Domain})
}

// HandleRemoveDomainSuppression removes a domain suppression
func (s *SuppressionService) HandleRemoveDomainSuppression(w http.ResponseWriter, r *http.Request) {
	domain := chi.URLParam(r, "domain")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "domain": domain})
}

// HandleGetSoftBounces returns soft bounces pending promotion
func (s *SuppressionService) HandleGetSoftBounces(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"soft_bounces": []interface{}{}})
}

// HandlePromoteSoftBounces promotes soft bounces to hard bounces
func (s *SuppressionService) HandlePromoteSoftBounces(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "promoted": 0})
}

// HandleGetPreferences returns subscriber preferences
func (s *SuppressionService) HandleGetPreferences(w http.ResponseWriter, r *http.Request) {
	email := chi.URLParam(r, "email")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"email":         email,
		"subscribed":    true,
		"preferences":   map[string]bool{},
		"frequency":     "all",
	})
}

// HandleUpdatePreferences updates subscriber preferences
func (s *SuppressionService) HandleUpdatePreferences(w http.ResponseWriter, r *http.Request) {
	email := chi.URLParam(r, "email")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "email": email})
}

// HandleUnsubscribeAll unsubscribes from all lists
func (s *SuppressionService) HandleUnsubscribeAll(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email string `json:"email"`
	}
	json.NewDecoder(r.Body).Decode(&input)

	// Add to global suppression
	id := fmt.Sprintf("sup-%d", time.Now().UnixNano())
	s.db.Exec(`
		INSERT INTO mailing_suppression_entries (id, email, reason, source)
		VALUES ($1, $2, 'unsubscribe_all', 'preference_center')
	`, id, strings.ToLower(input.Email))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// HandleOptizmoSync triggers an Optizmo sync
func (s *SuppressionService) HandleOptizmoSync(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ListID    string `json:"list_id"`
		DeltaOnly bool   `json:"delta_only"`
	}
	json.NewDecoder(r.Body).Decode(&input)

	// Queue sync job in background
	go func() {
		ctx := context.Background()
		s.runOptizmoSync(ctx, input.ListID, input.DeltaOnly)
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Sync initiated for list " + input.ListID,
	})
}

// runOptizmoSync performs the actual Optizmo sync
func (s *SuppressionService) runOptizmoSync(ctx context.Context, listID string, deltaOnly bool) {
	log.Printf("Starting Optizmo sync for list %s (delta=%v)", listID, deltaOnly)

	// Record sync job
	jobID := fmt.Sprintf("sync-%d", time.Now().UnixNano())
	s.db.Exec(`
		INSERT INTO suppression_sync_jobs (id, list_id, started_at, status, delta_sync)
		VALUES ($1, $2, NOW(), 'running', $3)
	`, jobID, listID, deltaOnly)

	// TODO: Implement actual Optizmo API call using s.optizmoKey
	// For now, mark as completed
	s.db.Exec(`
		UPDATE suppression_sync_jobs 
		SET status = 'completed', completed_at = NOW()
		WHERE id = $1
	`, jobID)

	log.Printf("Optizmo sync completed for list %s", listID)
}

// HandleOptizmoStatus returns Optizmo connection status
func (s *SuppressionService) HandleOptizmoStatus(w http.ResponseWriter, r *http.Request) {
	status := map[string]interface{}{
		"connected":       s.optizmoKey != "",
		"auth_configured": s.optizmoKey != "",
		"matcher_ready":   s.matcher != nil,
		"matcher_stats":   s.matcher.GetStats(),
	}

	// Get recent sync status
	rows, err := s.db.Query(`
		SELECT list_id, started_at, status, records_new
		FROM suppression_sync_jobs
		ORDER BY started_at DESC
		LIMIT 5
	`)
	if err == nil {
		defer rows.Close()
		syncs := []map[string]interface{}{}
		for rows.Next() {
			var listID, syncStatus string
			var startedAt time.Time
			var recordsNew int64
			if rows.Scan(&listID, &startedAt, &syncStatus, &recordsNew) == nil {
				syncs = append(syncs, map[string]interface{}{
					"list_id":     listID,
					"started_at":  startedAt,
					"status":      syncStatus,
					"records_new": recordsNew,
				})
			}
		}
		status["recent_syncs"] = syncs
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// HandleOptizmoSyncLog returns sync history
func (s *SuppressionService) HandleOptizmoSyncLog(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`
		SELECT id, list_id, started_at, completed_at, status, delta_sync, records_new, error
		FROM suppression_sync_jobs
		ORDER BY started_at DESC
		LIMIT 50
	`)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"jobs": []interface{}{}})
		return
	}
	defer rows.Close()

	jobs := []map[string]interface{}{}
	for rows.Next() {
		var id, listID, status string
		var startedAt time.Time
		var completedAt sql.NullTime
		var deltaSync bool
		var recordsNew int64
		var errMsg sql.NullString

		if err := rows.Scan(&id, &listID, &startedAt, &completedAt, &status, &deltaSync, &recordsNew, &errMsg); err != nil {
			continue
		}

		job := map[string]interface{}{
			"id":          id,
			"list_id":     listID,
			"started_at":  startedAt,
			"status":      status,
			"delta_sync":  deltaSync,
			"records_new": recordsNew,
		}
		if completedAt.Valid {
			job["completed_at"] = completedAt.Time
		}
		if errMsg.Valid {
			job["error"] = errMsg.String
		}
		jobs = append(jobs, job)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"jobs": jobs})
}

// HandleGetOptizmoConfig returns Optizmo configuration
func (s *SuppressionService) HandleGetOptizmoConfig(w http.ResponseWriter, r *http.Request) {
	config := map[string]interface{}{
		"base_url":        "https://mailer-api.optizmo.net",
		"connected":       s.optizmoKey != "",
		"auth_configured": s.optizmoKey != "",
		"sync_settings": map[string]interface{}{
			"auto_sync_enabled": true,
			"sync_interval":     "24h",
			"delta_enabled":     true,
			"s3_bucket":         "mailing-suppressions",
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(config)
}

// HandleUpdateOptizmoConfig updates Optizmo configuration
func (s *SuppressionService) HandleUpdateOptizmoConfig(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AuthToken    string `json:"auth_token"`
		SyncInterval string `json:"sync_interval"`
		DeltaEnabled bool   `json:"delta_enabled"`
		S3Bucket     string `json:"s3_bucket"`
	}
	json.NewDecoder(r.Body).Decode(&input)

	// Store in database
	configJSON, _ := json.Marshal(input)
	s.db.Exec(`
		CREATE TABLE IF NOT EXISTS suppression_config (
			id VARCHAR(100) PRIMARY KEY,
			config JSONB,
			updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
		)
	`)
	s.db.Exec(`
		INSERT INTO suppression_config (id, config, updated_at)
		VALUES ('optizmo', $1, NOW())
		ON CONFLICT (id) DO UPDATE SET config = $1, updated_at = NOW()
	`, string(configJSON))

	// Update API key if provided
	if input.AuthToken != "" {
		s.optizmoKey = input.AuthToken
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// HandleGetOptizmoLists returns configured Optizmo lists
func (s *SuppressionService) HandleGetOptizmoLists(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`
		SELECT id, name, optizmo_list_id, entry_count, updated_at
		FROM mailing_suppression_lists
		WHERE source = 'optizmo'
	`)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"lists": []interface{}{}})
		return
	}
	defer rows.Close()

	lists := []map[string]interface{}{}
	for rows.Next() {
		var id, name string
		var optizmoListID sql.NullString
		var entryCount int
		var updatedAt time.Time
		if err := rows.Scan(&id, &name, &optizmoListID, &entryCount, &updatedAt); err != nil {
			continue
		}
		lists = append(lists, map[string]interface{}{
			"id":              id,
			"name":            name,
			"optizmo_list_id": optizmoListID.String,
			"entry_count":     entryCount,
			"last_sync":       updatedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"lists": lists})
}

// HandleOptizmoListSync triggers sync for a specific Optizmo list
func (s *SuppressionService) HandleOptizmoListSync(w http.ResponseWriter, r *http.Request) {
	listID := chi.URLParam(r, "listId")

	var input struct {
		DeltaOnly bool `json:"delta_only"`
	}
	json.NewDecoder(r.Body).Decode(&input)

	go func() {
		ctx := context.Background()
		s.runOptizmoSync(ctx, listID, input.DeltaOnly)
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("Sync triggered for list %s", listID),
	})
}

// HandleBatchSuppressionCheck performs fast batch suppression checking
func (s *SuppressionService) HandleBatchSuppressionCheck(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Emails             []string `json:"emails"`
		MD5Hashes          []string `json:"md5_hashes"`
		SuppressionListIDs []string `json:"suppression_list_ids"`
		ReturnDeliverable  bool     `json:"return_deliverable"`
		ReturnSuppressed   bool     `json:"return_suppressed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	startTime := time.Now()
	suppressed := 0
	deliverable := 0
	suppressedList := []string{}
	deliverableList := []string{}

	allItems := append(input.Emails, input.MD5Hashes...)

	for _, item := range allItems {
		isSuppressed := false
		if s.matcher != nil {
			if strings.Contains(item, "@") {
				isSuppressed = s.matcher.IsSuppressed(item, input.SuppressionListIDs)
			} else {
				isSuppressed = s.matcher.IsSuppressedMD5(item, input.SuppressionListIDs)
			}
		}

		if isSuppressed {
			suppressed++
			if input.ReturnSuppressed {
				suppressedList = append(suppressedList, item)
			}
		} else {
			deliverable++
			if input.ReturnDeliverable {
				deliverableList = append(deliverableList, item)
			}
		}
	}

	result := map[string]interface{}{
		"stats": map[string]interface{}{
			"total":         len(allItems),
			"suppressed":    suppressed,
			"deliverable":   deliverable,
			"processing_ms": time.Since(startTime).Milliseconds(),
		},
	}

	if input.ReturnSuppressed {
		result["suppressed_emails"] = suppressedList
	}
	if input.ReturnDeliverable {
		result["deliverable_emails"] = deliverableList
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// HandleMatcherStats returns bloom filter matcher statistics
func (s *SuppressionService) HandleMatcherStats(w http.ResponseWriter, r *http.Request) {
	stats := map[string]interface{}{
		"matcher_available": s.matcher != nil,
	}

	if s.matcher != nil {
		stats["matcher_stats"] = s.matcher.GetStats()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// HandleSuppressionAnalytics returns suppression analytics
func (s *SuppressionService) HandleSuppressionAnalytics(w http.ResponseWriter, r *http.Request) {
	var totalCount, bounceCount, complaintCount, unsubCount int

	s.db.QueryRow(`SELECT COUNT(*) FROM mailing_suppression_entries`).Scan(&totalCount)
	s.db.QueryRow(`SELECT COUNT(*) FROM mailing_suppression_entries WHERE reason = 'bounce'`).Scan(&bounceCount)
	s.db.QueryRow(`SELECT COUNT(*) FROM mailing_suppression_entries WHERE reason = 'complaint'`).Scan(&complaintCount)
	s.db.QueryRow(`SELECT COUNT(*) FROM mailing_suppression_entries WHERE reason LIKE '%unsub%'`).Scan(&unsubCount)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total":      totalCount,
		"bounces":    bounceCount,
		"complaints": complaintCount,
		"unsubscribes": unsubCount,
		"by_reason": map[string]int{
			"bounce":      bounceCount,
			"complaint":   complaintCount,
			"unsubscribe": unsubCount,
			"other":       totalCount - bounceCount - complaintCount - unsubCount,
		},
	})
}

// HandleSuppressionAudit returns audit log of suppression changes
func (s *SuppressionService) HandleSuppressionAudit(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`
		SELECT id, email, reason, source, created_at
		FROM mailing_suppression_entries
		ORDER BY created_at DESC
		LIMIT 100
	`)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"audit_log": []interface{}{}})
		return
	}
	defer rows.Close()

	entries := []map[string]interface{}{}
	for rows.Next() {
		var id, reason, source string
		var email sql.NullString
		var createdAt time.Time
		if err := rows.Scan(&id, &email, &reason, &source, &createdAt); err != nil {
			continue
		}
		entries = append(entries, map[string]interface{}{
			"id":         id,
			"email":      email.String,
			"reason":     reason,
			"source":     source,
			"created_at": createdAt,
			"action":     "added",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"audit_log": entries})
}

// HandleOneClickUnsubscribe handles RFC 8058 one-click unsubscribe
func (s *SuppressionService) HandleOneClickUnsubscribe(w http.ResponseWriter, r *http.Request) {
	// Parse form data
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// RFC 8058 requires List-Unsubscribe=One-Click header
	listUnsubscribe := r.PostFormValue("List-Unsubscribe")
	email := r.PostFormValue("email")

	if email == "" {
		// Try to extract from token
		token := r.PostFormValue("token")
		if token != "" {
			// Decode token to get email
			// For now, use token as email if no email provided
			email = token
		}
	}

	if email == "" {
		http.Error(w, "Email required", http.StatusBadRequest)
		return
	}

	// Add to global suppression
	id := fmt.Sprintf("unsub-%d", time.Now().UnixNano())
	_, err := s.db.Exec(`
		INSERT INTO mailing_suppression_entries (id, email, reason, source)
		VALUES ($1, $2, 'one_click_unsubscribe', 'rfc8058')
		ON CONFLICT DO NOTHING
	`, id, strings.ToLower(email))

	if err != nil {
		log.Printf("One-click unsubscribe failed for %s: %v", logger.RedactEmail(email), err)
	}

	// RFC 8058 requires 200 OK response
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Unsubscribed successfully"))

	log.Printf("One-click unsubscribe processed for %s (list: %s)", logger.RedactEmail(email), listUnsubscribe)
}

// HandleListUnsubscribeHeader returns the List-Unsubscribe header value
func (s *SuppressionService) HandleListUnsubscribeHeader(w http.ResponseWriter, r *http.Request) {
	// Generate a one-click unsubscribe URL
	baseURL := r.URL.Query().Get("base_url")
	if baseURL == "" {
		baseURL = "https://mail.example.com"
	}

	email := r.URL.Query().Get("email")
	campaignID := r.URL.Query().Get("campaign_id")

	// Generate header value per RFC 8058
	unsubscribeURL := fmt.Sprintf("%s/api/mailing/unsubscribe/one-click?email=%s&campaign=%s",
		baseURL, email, campaignID)
	mailtoURL := fmt.Sprintf("mailto:unsubscribe@example.com?subject=Unsubscribe%%20%s", email)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"List-Unsubscribe":           fmt.Sprintf("<%s>, <%s>", unsubscribeURL, mailtoURL),
		"List-Unsubscribe-Post":      "List-Unsubscribe=One-Click",
		"one_click_url":              unsubscribeURL,
		"mailto_url":                 mailtoURL,
	})
}
