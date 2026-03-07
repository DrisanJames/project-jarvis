// Global suppression list HTTP handlers.
// All global suppression operations delegate to GlobalSuppressionHub (single source of truth).
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// HandleGetGlobalSuppression returns the global suppression list with statistics
func (s *SuppressionService) HandleGetGlobalSuppression(w http.ResponseWriter, r *http.Request) {
	result := map[string]interface{}{
		"id":          "global-suppression-list",
		"name":        "Global Suppression List",
		"description": "Industry-standard global suppression list applied to all campaigns",
		"categories":  GlobalSuppressionCategories,
	}

	if s.globalHub != nil {
		stats, err := s.globalHub.GetStats(r.Context())
		if err == nil {
			result["stats"] = stats
		}
		result["total_entries"] = s.globalHub.Count()
	} else {
		var totalCount int
		s.db.QueryRow(`SELECT COUNT(*) FROM mailing_suppression_entries WHERE is_global = TRUE`).Scan(&totalCount)
		result["total_entries"] = totalCount
	}

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

	if s.globalHub != nil {
		if err := s.globalHub.Remove(r.Context(), email); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
			return
		}
	} else {
		s.db.Exec(`DELETE FROM mailing_suppression_entries WHERE is_global = TRUE AND (email = $1 OR md5_hash = $1)`, email)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "email": email})
}

// HandleGetGlobalSuppressionEntries returns entries from the global suppression list
func (s *SuppressionService) HandleGetGlobalSuppressionEntries(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	limitStr := r.URL.Query().Get("limit")
	limit := 500
	if limitStr != "" {
		fmt.Sscanf(limitStr, "%d", &limit)
	}
	if limit <= 0 || limit > 1000 {
		limit = 500
	}

	if s.globalHub != nil {
		entries, total, err := s.globalHub.Search(r.Context(), query, limit, 0)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"entries": []interface{}{}, "total": 0})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"entries": entries, "total": total})
		return
	}

	// Fallback to legacy table
	dbQuery := `SELECT id, email, md5_hash, reason, source, category, created_at
		FROM mailing_suppression_entries WHERE is_global = TRUE ORDER BY created_at DESC LIMIT $1`
	rows, err := s.db.Query(dbQuery, limit)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"entries": []interface{}{}})
		return
	}
	defer rows.Close()

	entries := []map[string]interface{}{}
	for rows.Next() {
		var id, source, cat string
		var email, md5Hash, reason interface{}
		var createdAt time.Time
		if rows.Scan(&id, &email, &md5Hash, &reason, &source, &cat, &createdAt) == nil {
			entries = append(entries, map[string]interface{}{
				"id": id, "email": email, "md5_hash": md5Hash,
				"reason": reason, "source": source, "category": cat, "created_at": createdAt,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"entries": entries})
}

// HandleCheckGlobalSuppression checks if an email is in the global suppression list.
// Delegates to GlobalSuppressionHub for O(1) in-memory lookup.
func (s *SuppressionService) HandleCheckGlobalSuppression(w http.ResponseWriter, r *http.Request) {
	email := chi.URLParam(r, "email")
	email = strings.ToLower(strings.TrimSpace(email))

	result := map[string]interface{}{
		"email":        email,
		"is_role_based": IsRoleBasedEmail(email),
	}

	if s.globalHub != nil {
		suppressed := s.globalHub.IsSuppressed(email)
		result["suppressed"] = suppressed
		result["md5_hash"] = engine.MD5Hash(email)
	} else {
		md5Hash := engine.MD5Hash(email)
		var exists bool
		s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM mailing_suppression_entries WHERE is_global = TRUE AND (email = $1 OR md5_hash = $2))`, email, md5Hash).Scan(&exists)
		result["suppressed"] = exists
		result["md5_hash"] = md5Hash
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// HandleProcessBounce processes a bounce event and adds to global suppression
func (s *SuppressionService) HandleProcessBounce(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email      string `json:"email"`
		BounceType string `json:"bounce_type"`
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
		"success": true, "email": input.Email, "category": category,
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
		"success": true, "email": input.Email, "category": "spam_complaint",
	})
}
