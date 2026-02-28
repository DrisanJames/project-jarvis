// Global suppression list HTTP handlers.
package api

import (
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// HandleGetGlobalSuppression returns the global suppression list with statistics
func (s *SuppressionService) HandleGetGlobalSuppression(w http.ResponseWriter, r *http.Request) {
	result := map[string]interface{}{
		"id":          "global-suppression-list",
		"name":        "Global Suppression List",
		"description": "Industry-standard global suppression list applied to all campaigns",
		"categories":  GlobalSuppressionCategories,
		"stats":       map[string]interface{}{},
	}

	// Get total count
	var totalCount int
	s.db.QueryRow(`SELECT COUNT(*) FROM mailing_suppression_entries WHERE is_global = TRUE`).Scan(&totalCount)
	result["total_entries"] = totalCount

	// Get counts by category
	categoryStats := make(map[string]int)
	rows, err := s.db.Query(`
		SELECT category, COUNT(*) as count 
		FROM mailing_suppression_entries 
		WHERE is_global = TRUE 
		GROUP BY category
	`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var cat string
			var count int
			if rows.Scan(&cat, &count) == nil {
				categoryStats[cat] = count
			}
		}
	}
	result["by_category"] = categoryStats

	// Get recent additions (last 24h)
	var recentCount int
	s.db.QueryRow(`
		SELECT COUNT(*) FROM mailing_suppression_entries 
		WHERE is_global = TRUE AND created_at > NOW() - INTERVAL '24 hours'
	`).Scan(&recentCount)
	result["recent_24h"] = recentCount

	// Get entries by source
	sourceStats := make(map[string]int)
	sourceRows, err := s.db.Query(`
		SELECT source, COUNT(*) as count 
		FROM mailing_suppression_entries 
		WHERE is_global = TRUE 
		GROUP BY source
	`)
	if err == nil {
		defer sourceRows.Close()
		for sourceRows.Next() {
			var src string
			var count int
			if sourceRows.Scan(&src, &count) == nil {
				sourceStats[src] = count
			}
		}
	}
	result["by_source"] = sourceStats

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// HandleAddToGlobalSuppression adds an email to the global suppression list
func (s *SuppressionService) HandleAddToGlobalSuppression(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email    string `json:"email"`
		Category string `json:"category"`
		Source   string `json:"source"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	if input.Category == "" {
		input.Category = "manual"
	}
	if input.Source == "" {
		input.Source = "manual"
	}

	err := s.AddToGlobalSuppression(input.Email, input.Category, input.Source)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"email":    input.Email,
		"category": input.Category,
	})
}

// HandleBulkAddToGlobalSuppression adds multiple emails to the global suppression list
func (s *SuppressionService) HandleBulkAddToGlobalSuppression(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Emails   []string `json:"emails"`
		Category string   `json:"category"`
		Source   string   `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	if input.Category == "" {
		input.Category = "manual"
	}
	if input.Source == "" {
		input.Source = "bulk_import"
	}

	added := 0
	for _, email := range input.Emails {
		if err := s.AddToGlobalSuppression(email, input.Category, input.Source); err == nil {
			added++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"added":   added,
		"total":   len(input.Emails),
	})
}

// HandleRemoveFromGlobalSuppression removes an email from the global suppression list
func (s *SuppressionService) HandleRemoveFromGlobalSuppression(w http.ResponseWriter, r *http.Request) {
	email := chi.URLParam(r, "email")
	email = strings.ToLower(strings.TrimSpace(email))

	_, err := s.db.Exec(`
		DELETE FROM mailing_suppression_entries 
		WHERE is_global = TRUE AND (email = $1 OR md5_hash = $1)
	`, email)

	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	// Update entry count
	s.db.Exec(`
		UPDATE mailing_suppression_lists 
		SET entry_count = (SELECT COUNT(*) FROM mailing_suppression_entries WHERE list_id = 'global-suppression-list'),
		    updated_at = NOW()
		WHERE id = 'global-suppression-list'
	`)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "email": email})
}

// HandleGetGlobalSuppressionEntries returns entries from the global suppression list
func (s *SuppressionService) HandleGetGlobalSuppressionEntries(w http.ResponseWriter, r *http.Request) {
	category := r.URL.Query().Get("category")
	limit := r.URL.Query().Get("limit")
	if limit == "" {
		limit = "500"
	}

	query := `
		SELECT id, email, md5_hash, reason, source, category, created_at
		FROM mailing_suppression_entries
		WHERE is_global = TRUE
	`
	args := []interface{}{}

	if category != "" {
		query += " AND category = $1"
		args = append(args, category)
	}

	query += " ORDER BY created_at DESC LIMIT " + limit

	rows, err := s.db.Query(query, args...)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"entries": []interface{}{}})
		return
	}
	defer rows.Close()

	entries := []map[string]interface{}{}
	for rows.Next() {
		var id, source, cat string
		var email, md5Hash, reason sql.NullString
		var createdAt time.Time
		if err := rows.Scan(&id, &email, &md5Hash, &reason, &source, &cat, &createdAt); err != nil {
			continue
		}
		entries = append(entries, map[string]interface{}{
			"id":         id,
			"email":      email.String,
			"md5_hash":   md5Hash.String,
			"reason":     reason.String,
			"source":     source,
			"category":   cat,
			"created_at": createdAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"entries": entries})
}

// HandleCheckGlobalSuppression checks if an email is in the global suppression list
func (s *SuppressionService) HandleCheckGlobalSuppression(w http.ResponseWriter, r *http.Request) {
	email := chi.URLParam(r, "email")
	email = strings.ToLower(strings.TrimSpace(email))

	// Compute MD5
	hash := md5.Sum([]byte(email))
	md5Hash := hex.EncodeToString(hash[:])

	var category, reason, source string
	var createdAt time.Time
	err := s.db.QueryRow(`
		SELECT category, reason, source, created_at 
		FROM mailing_suppression_entries 
		WHERE is_global = TRUE AND (email = $1 OR md5_hash = $2)
		LIMIT 1
	`, email, md5Hash).Scan(&category, &reason, &source, &createdAt)

	suppressed := err == nil

	result := map[string]interface{}{
		"email":      email,
		"suppressed": suppressed,
	}

	if suppressed {
		result["category"] = category
		result["reason"] = reason
		result["source"] = source
		result["suppressed_at"] = createdAt
	}

	// Also check if it's a role-based address
	result["is_role_based"] = IsRoleBasedEmail(email)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// HandleProcessBounce processes a bounce event and adds to global suppression
func (s *SuppressionService) HandleProcessBounce(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email      string `json:"email"`
		BounceType string `json:"bounce_type"` // hard, soft
		ESP        string `json:"esp"`
		ErrorCode  string `json:"error_code"`
		Timestamp  string `json:"timestamp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	category := "hard_bounce"
	if input.BounceType == "soft" {
		// Soft bounces should be tracked separately and promoted after threshold
		category = "soft_bounce_promoted"
	}

	source := fmt.Sprintf("esp_%s_bounce", input.ESP)
	err := s.AddToGlobalSuppression(input.Email, category, source)

	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"email":       input.Email,
		"category":    category,
		"bounce_type": input.BounceType,
	})
}

// HandleProcessComplaint processes a spam complaint and adds to global suppression
func (s *SuppressionService) HandleProcessComplaint(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email     string `json:"email"`
		ESP       string `json:"esp"`
		ISP       string `json:"isp"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	source := fmt.Sprintf("fbl_%s", input.ISP)
	if input.ESP != "" {
		source = fmt.Sprintf("esp_%s_fbl", input.ESP)
	}

	err := s.AddToGlobalSuppression(input.Email, "spam_complaint", source)

	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"email":    input.Email,
		"category": "spam_complaint",
	})
}
