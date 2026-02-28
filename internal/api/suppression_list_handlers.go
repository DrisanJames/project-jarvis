// Suppression list management HTTP handlers.
package api

import (
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// HandleGetSuppressionLists returns all suppression lists
func (s *SuppressionService) HandleGetSuppressionLists(w http.ResponseWriter, r *http.Request) {
	// Get organization ID from header, middleware context, or dev fallback
	orgID := r.Header.Get("X-Organization-ID")
	if orgID == "" {
		var err error
		orgID, err = GetOrgIDStringFromRequest(r)
		if err != nil {
			// Dev fallback: use the single org present in the DB
			dbErr := s.db.QueryRow(`SELECT DISTINCT organization_id FROM mailing_suppression_lists LIMIT 1`).Scan(&orgID)
			if dbErr != nil || orgID == "" {
				respondJSON(w, http.StatusUnauthorized, map[string]string{"error": "organization context required"})
				return
			}
		}
	}
	
	// Use cached entry_count (snapshot) — no expensive per-list COUNT(*) subquery
	rows, err := s.db.Query(`
		SELECT id, name, description, source, optizmo_list_id, 
		       COALESCE(entry_count, 0) as entry_count, created_at, updated_at
		FROM mailing_suppression_lists
		WHERE organization_id = $1
		ORDER BY created_at DESC
	`, orgID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"lists": []interface{}{}})
		return
	}
	defer rows.Close()

	lists := []map[string]interface{}{}
	for rows.Next() {
		var id, name, source string
		var description, optizmoListID sql.NullString
		var entryCount int
		var createdAt, updatedAt time.Time

		if err := rows.Scan(&id, &name, &description, &source, &optizmoListID, &entryCount, &createdAt, &updatedAt); err != nil {
			continue
		}

		lists = append(lists, map[string]interface{}{
			"id":              id,
			"name":            name,
			"description":     description.String,
			"source":          source,
			"optizmo_list_id": optizmoListID.String,
			"entry_count":     entryCount,
			"created_at":      createdAt,
			"updated_at":      updatedAt,
			"status":          "active",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"lists": lists})
}

// HandleCreateSuppressionList creates a new suppression list
func (s *SuppressionService) HandleCreateSuppressionList(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name          string `json:"name"`
		Description   string `json:"description"`
		Source        string `json:"source"`
		OptizmoListID string `json:"optizmo_list_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	if input.Name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}

	// Generate a proper UUID for the ID
	id := uuid.New().String()

	if input.Source == "" {
		input.Source = "manual"
	}

	// Get organization ID from request context (header, middleware, or dev fallback)
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		// Fallback: look up the single org from the database (common in dev environments)
		var fallbackOrgID string
		dbErr := s.db.QueryRow(`SELECT DISTINCT organization_id FROM mailing_suppression_lists LIMIT 1`).Scan(&fallbackOrgID)
		if dbErr != nil {
			log.Printf("[Suppression] Create failed: no org context and no fallback: %v", err)
			respondJSON(w, http.StatusUnauthorized, map[string]string{"error": "organization context required"})
			return
		}
		orgID, _ = uuid.Parse(fallbackOrgID)
	}

	_, err = s.db.Exec(`
		INSERT INTO mailing_suppression_lists (id, organization_id, name, description, source, optizmo_list_id)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, id, orgID, input.Name, nullString(input.Description), input.Source, nullString(input.OptizmoListID))

	if err != nil {
		log.Printf("[Suppression] Create list error: %v", err)
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"id": id, "name": input.Name})
}

// HandleGetSuppressionList returns a single suppression list with entries
func (s *SuppressionService) HandleGetSuppressionList(w http.ResponseWriter, r *http.Request) {
	listID := chi.URLParam(r, "listId")

	var list map[string]interface{}
	var id, name, source string
	var description, optizmoListID sql.NullString
	var entryCount int
	var createdAt, updatedAt time.Time

	err := s.db.QueryRow(`
		SELECT id, name, description, source, optizmo_list_id, COALESCE(entry_count, 0), created_at, updated_at
		FROM mailing_suppression_lists WHERE id = $1
	`, listID).Scan(&id, &name, &description, &source, &optizmoListID, &entryCount, &createdAt, &updatedAt)

	if err != nil {
		http.Error(w, `{"error":"list not found"}`, http.StatusNotFound)
		return
	}

	list = map[string]interface{}{
		"id":              id,
		"name":            name,
		"description":     description.String,
		"source":          source,
		"optizmo_list_id": optizmoListID.String,
		"entry_count":     entryCount,
		"created_at":      createdAt,
		"updated_at":      updatedAt,
		"status":          "active",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

// HandleDeleteSuppressionList deletes a suppression list
func (s *SuppressionService) HandleDeleteSuppressionList(w http.ResponseWriter, r *http.Request) {
	listID := chi.URLParam(r, "listId")

	// Delete entries first
	s.db.Exec(`DELETE FROM mailing_suppression_entries WHERE list_id = $1`, listID)
	// Delete list
	_, err := s.db.Exec(`DELETE FROM mailing_suppression_lists WHERE id = $1`, listID)

	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// HandleUpdateSuppressionList updates a suppression list
func (s *SuppressionService) HandleUpdateSuppressionList(w http.ResponseWriter, r *http.Request) {
	listID := chi.URLParam(r, "listId")

	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Source      string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	_, err := s.db.Exec(`
		UPDATE mailing_suppression_lists 
		SET name = $1, description = $2, source = $3, updated_at = NOW()
		WHERE id = $4
	`, input.Name, input.Description, input.Source, listID)

	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// HandleGetSuppressionListEntries returns entries for a specific list
func (s *SuppressionService) HandleGetSuppressionListEntries(w http.ResponseWriter, r *http.Request) {
	listID := chi.URLParam(r, "listId")

	rows, err := s.db.Query(`
		SELECT id, email, md5_hash, reason, source, created_at
		FROM mailing_suppression_entries
		WHERE list_id = $1
		ORDER BY created_at DESC
		LIMIT 1000
	`, listID)

	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"entries": []interface{}{}})
		return
	}
	defer rows.Close()

	entries := []map[string]interface{}{}
	for rows.Next() {
		var id, source string
		var email, md5Hash, reason sql.NullString
		var createdAt time.Time
		if err := rows.Scan(&id, &email, &md5Hash, &reason, &source, &createdAt); err != nil {
			continue
		}
		entries = append(entries, map[string]interface{}{
			"id":         id,
			"email":      email.String,
			"md5_hash":   md5Hash.String,
			"reason":     reason.String,
			"source":     source,
			"list_id":    listID,
			"created_at": createdAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"entries": entries})
}

// HandleAddSuppressionListEntry adds an entry to a specific list
func (s *SuppressionService) HandleAddSuppressionListEntry(w http.ResponseWriter, r *http.Request) {
	listID := chi.URLParam(r, "listId")

	var input struct {
		Email  string `json:"email"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	id := fmt.Sprintf("entry-%d", time.Now().UnixNano())
	email := strings.ToLower(strings.TrimSpace(input.Email))
	
	// Compute MD5 hash
	hash := md5.Sum([]byte(email))
	md5Hash := hex.EncodeToString(hash[:])

	reason := input.Reason
	if reason == "" {
		reason = "Manual addition"
	}

	_, err := s.db.Exec(`
		INSERT INTO mailing_suppression_entries (id, list_id, email, md5_hash, reason, source)
		VALUES ($1, $2, $3, $4, $5, 'manual')
		ON CONFLICT (list_id, md5_hash) DO NOTHING
	`, id, listID, email, md5Hash, reason)

	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	// Update entry count
	s.db.Exec(`
		UPDATE mailing_suppression_lists 
		SET entry_count = (SELECT COUNT(*) FROM mailing_suppression_entries WHERE list_id = $1),
		    updated_at = NOW()
		WHERE id = $1
	`, listID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":       id,
		"email":    email,
		"md5_hash": md5Hash,
		"success":  true,
	})
}

// HandleRemoveSuppressionListEntry removes an entry from a specific list
func (s *SuppressionService) HandleRemoveSuppressionListEntry(w http.ResponseWriter, r *http.Request) {
	listID := chi.URLParam(r, "listId")
	entryID := chi.URLParam(r, "entryId")

	_, err := s.db.Exec(`
		DELETE FROM mailing_suppression_entries 
		WHERE id = $1 AND list_id = $2
	`, entryID, listID)

	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	// Update entry count
	s.db.Exec(`
		UPDATE mailing_suppression_lists 
		SET entry_count = (SELECT COUNT(*) FROM mailing_suppression_entries WHERE list_id = $1),
		    updated_at = NOW()
		WHERE id = $1
	`, listID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// HandleImportSuppressionListEntries imports entries into a specific list
func (s *SuppressionService) HandleImportSuppressionListEntries(w http.ResponseWriter, r *http.Request) {
	listID := chi.URLParam(r, "listId")

	// Parse multipart form
	if err := r.ParseMultipartForm(100 << 20); err != nil { // 100MB max
		http.Error(w, `{"error":"file too large"}`, http.StatusBadRequest)
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"no file provided"}`, http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Read file content
	buf := make([]byte, 0)
	chunk := make([]byte, 1024*1024)
	for {
		n, err := file.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if err != nil {
			break
		}
	}

	// Parse and import
	lines := strings.Split(string(buf), "\n")
	imported := 0
	duplicates := 0
	invalid := 0

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		var email, md5Hash string
		if strings.Contains(line, "@") {
			email = strings.ToLower(line)
			hash := md5.Sum([]byte(email))
			md5Hash = hex.EncodeToString(hash[:])
		} else if len(line) == 32 {
			md5Hash = strings.ToLower(line)
		} else {
			invalid++
			continue
		}

		id := fmt.Sprintf("entry-%d", time.Now().UnixNano())
		result, err := s.db.Exec(`
			INSERT INTO mailing_suppression_entries (id, list_id, email, md5_hash, reason, source)
			VALUES ($1, $2, $3, $4, 'Import', 'import')
			ON CONFLICT (list_id, md5_hash) DO NOTHING
		`, id, listID, nullString(email), md5Hash)

		if err == nil {
			rows, _ := result.RowsAffected()
			if rows > 0 {
				imported++
			} else {
				duplicates++
			}
		}
	}

	// Update entry count
	s.db.Exec(`
		UPDATE mailing_suppression_lists 
		SET entry_count = (SELECT COUNT(*) FROM mailing_suppression_entries WHERE list_id = $1),
		    updated_at = NOW()
		WHERE id = $1
	`, listID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"imported":   imported,
		"duplicates": duplicates,
		"invalid":    invalid,
		"total":      len(lines),
	})
}
