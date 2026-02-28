// Core suppression CRUD and dashboard HTTP handlers.
package api

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// HandleGetSuppressions returns all suppressions with pagination
func (s *SuppressionService) HandleGetSuppressions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`
		SELECT e.id, e.email, e.md5_hash, e.reason, e.source, e.created_at, l.name as list_name
		FROM mailing_suppression_entries e
		LEFT JOIN mailing_suppression_lists l ON e.list_id = l.id
		ORDER BY e.created_at DESC
		LIMIT 1000
	`)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"suppressions": []interface{}{}})
		return
	}
	defer rows.Close()

	suppressions := []map[string]interface{}{}
	for rows.Next() {
		var id, reason, source string
		var email, md5Hash, listName sql.NullString
		var createdAt time.Time
		if err := rows.Scan(&id, &email, &md5Hash, &reason, &source, &createdAt, &listName); err != nil {
			continue
		}
		suppressions = append(suppressions, map[string]interface{}{
			"id":         id,
			"email":      email.String,
			"md5_hash":   md5Hash.String,
			"reason":     reason,
			"source":     source,
			"created_at": createdAt,
			"list_name":  listName.String,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"suppressions": suppressions})
}

// HandleAddSuppression adds a new suppression
func (s *SuppressionService) HandleAddSuppression(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email   string `json:"email"`
		MD5Hash string `json:"md5_hash"`
		Reason  string `json:"reason"`
		Source  string `json:"source"`
		ListID  string `json:"list_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	id := fmt.Sprintf("sup-%d", time.Now().UnixNano())
	_, err := s.db.Exec(`
		INSERT INTO mailing_suppression_entries (id, list_id, email, md5_hash, reason, source)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, id, nullString(input.ListID), nullString(input.Email), nullString(input.MD5Hash), input.Reason, input.Source)

	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"id": id, "success": true})
}

// HandleBulkAddSuppressions adds multiple suppressions at once
func (s *SuppressionService) HandleBulkAddSuppressions(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Entries []struct {
			Email   string `json:"email"`
			MD5Hash string `json:"md5_hash"`
			Reason  string `json:"reason"`
		} `json:"entries"`
		ListID string `json:"list_id"`
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	added := 0
	for _, entry := range input.Entries {
		id := fmt.Sprintf("sup-%d", time.Now().UnixNano())
		_, err := s.db.Exec(`
			INSERT INTO mailing_suppression_entries (id, list_id, email, md5_hash, reason, source)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (list_id, md5_hash) DO NOTHING
		`, id, nullString(input.ListID), nullString(entry.Email), nullString(entry.MD5Hash), entry.Reason, input.Source)
		if err == nil {
			added++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"added":   added,
		"total":   len(input.Entries),
	})
}

// HandleRemoveSuppression removes a suppression
func (s *SuppressionService) HandleRemoveSuppression(w http.ResponseWriter, r *http.Request) {
	email := chi.URLParam(r, "email")
	_, err := s.db.Exec(`DELETE FROM mailing_suppression_entries WHERE email = $1 OR md5_hash = $1`, email)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// HandleSuppressionDashboard returns dashboard statistics using snapshot/cached data.
// Avoids expensive COUNT(*) full-table scans on mailing_suppression_entries.
// Instead uses: SUM(entry_count) from lists table, PostgreSQL FILTER aggregates,
// and a single consolidated entries query for time-filtered metrics.
func (s *SuppressionService) HandleSuppressionDashboard(w http.ResponseWriter, r *http.Request) {
	dashboard := map[string]interface{}{
		"total_suppressed":            0,
		"total_lists":                 0,
		"avg_suppressed_per_campaign": 0,
		"recent_additions":            0,
		"optizmo_synced_today":        0,
		"last_delta_update":           time.Now().Format(time.RFC3339),
		"lists_updated_24h":           0,
		"new_lists_7d":                0,
		"suppression_rate":            0.0,
		"by_source":                   []map[string]interface{}{},
		"by_reason":                   []map[string]interface{}{},
		"recent_activity":             []map[string]interface{}{},
		"global_suppression": map[string]interface{}{
			"total":           0,
			"hard_bounces":    0,
			"spam_complaints": 0,
			"unsubscribes":    0,
			"spam_traps":      0,
			"role_based":      0,
			"disposable":      0,
			"known_litigator": 0,
			"invalid":         0,
			"manual":          0,
			"recent_24h":      0,
			"by_category":     map[string]int{},
		},
	}

	// BATCH 1: Lists-table snapshot (instant — no entries table scan)
	var totalEntries, totalLists, listsUpdated24h, newLists7d int
	s.db.QueryRow(`
		SELECT 
			COALESCE(SUM(entry_count), 0)::int,
			COUNT(*),
			COUNT(*) FILTER (WHERE updated_at > NOW() - INTERVAL '24 hours'),
			COUNT(*) FILTER (WHERE created_at > NOW() - INTERVAL '7 days')
		FROM mailing_suppression_lists
	`).Scan(&totalEntries, &totalLists, &listsUpdated24h, &newLists7d)
	dashboard["total_suppressed"] = totalEntries
	dashboard["total_lists"] = totalLists
	dashboard["lists_updated_24h"] = listsUpdated24h
	dashboard["new_lists_7d"] = newLists7d

	// BATCH 2: Source breakdown from lists table (instant — uses cached counts)
	sourceRows, err := s.db.Query(`
		SELECT source, COUNT(*) as list_count, COALESCE(SUM(entry_count), 0)::int as entry_count
		FROM mailing_suppression_lists
		GROUP BY source
		ORDER BY entry_count DESC
		LIMIT 10
	`)
	if err == nil {
		defer sourceRows.Close()
		bySource := []map[string]interface{}{}
		for sourceRows.Next() {
			var source string
			var listCount, entryCount int
			if sourceRows.Scan(&source, &listCount, &entryCount) == nil {
				bySource = append(bySource, map[string]interface{}{
					"source": source,
					"count":  entryCount,
					"lists":  listCount,
				})
			}
		}
		dashboard["by_source"] = bySource
	}

	// BATCH 3: Estimated recent additions (from lists updated recently)
	var recentEntryEstimate int
	s.db.QueryRow(`
		SELECT COALESCE(SUM(entry_count), 0)::int
		FROM mailing_suppression_lists
		WHERE updated_at > NOW() - INTERVAL '24 hours'
	`).Scan(&recentEntryEstimate)
	dashboard["recent_additions"] = recentEntryEstimate

	// Optizmo synced: count entries from optizmo-sourced lists updated today
	var optizmoSynced int
	s.db.QueryRow(`
		SELECT COALESCE(SUM(entry_count), 0)::int
		FROM mailing_suppression_lists
		WHERE source = 'optizmo' AND updated_at > CURRENT_DATE
	`).Scan(&optizmoSynced)
	dashboard["optizmo_synced_today"] = optizmoSynced

	// BATCH 4: Avg suppressed per campaign (small table, fast)
	var avgSuppressed float64
	s.db.QueryRow(`
		SELECT COALESCE(AVG(suppressed_count), 0) FROM mailing_campaigns 
		WHERE created_at > NOW() - INTERVAL '30 days'
	`).Scan(&avgSuppressed)
	dashboard["avg_suppressed_per_campaign"] = int(avgSuppressed)

	// BATCH 5: Global suppression stats from lists table (no entries scan)
	globalStats := map[string]interface{}{
		"total":           0,
		"hard_bounces":    0,
		"spam_complaints": 0,
		"unsubscribes":    0,
		"spam_traps":      0,
		"role_based":      0,
		"disposable":      0,
		"known_litigator": 0,
		"invalid":         0,
		"manual":          0,
		"recent_24h":      0,
		"by_category":     map[string]int{},
	}

	// Fast estimated total from PostgreSQL statistics (no table scan)
	var estimatedTotal float64
	s.db.QueryRow(`
		SELECT COALESCE(reltuples, 0)
		FROM pg_class
		WHERE relname = 'mailing_suppression_entries'
	`).Scan(&estimatedTotal)
	globalStats["total"] = int(estimatedTotal)

	// Recent global: estimate from lists updated in 24h with global-like sources
	var globalRecent int
	s.db.QueryRow(`
		SELECT COALESCE(SUM(entry_count), 0)::int
		FROM mailing_suppression_lists
		WHERE updated_at > NOW() - INTERVAL '24 hours'
		  AND source IN ('global', 'hard_bounce', 'spam_complaint', 'system', 'optizmo')
	`).Scan(&globalRecent)
	globalStats["recent_24h"] = globalRecent

	// Category breakdown from list sources (fast — lists table only)
	catRows, catErr := s.db.Query(`
		SELECT source, COUNT(*) as list_count, COALESCE(SUM(entry_count), 0)::int as entries
		FROM mailing_suppression_lists
		GROUP BY source
	`)
	if catErr == nil {
		defer catRows.Close()
		byCategory := make(map[string]int)
		for catRows.Next() {
			var src string
			var listCount, entries int
			if catRows.Scan(&src, &listCount, &entries) == nil {
				byCategory[src] = entries
				switch src {
				case "hard_bounce":
					globalStats["hard_bounces"] = entries
				case "spam_complaint":
					globalStats["spam_complaints"] = entries
				case "unsubscribe":
					globalStats["unsubscribes"] = entries
				case "spam_trap":
					globalStats["spam_traps"] = entries
				case "role_based":
					globalStats["role_based"] = entries
				case "disposable":
					globalStats["disposable"] = entries
				case "known_litigator":
					globalStats["known_litigator"] = entries
				case "invalid":
					globalStats["invalid"] = entries
				case "manual":
					globalStats["manual"] = entries
				}
			}
		}
		globalStats["by_category"] = byCategory
	}

	dashboard["global_suppression"] = globalStats

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dashboard)
}

// HandleCheckSuppression checks if an email is suppressed
func (s *SuppressionService) HandleCheckSuppression(w http.ResponseWriter, r *http.Request) {
	email := chi.URLParam(r, "email")

	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM mailing_suppression_entries 
		WHERE email = $1 OR md5_hash = $1
	`, strings.ToLower(email)).Scan(&count)

	suppressed := false
	if err == nil && count > 0 {
		suppressed = true
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"email":      email,
		"suppressed": suppressed,
	})
}

// HandleExportSuppressions exports all suppressions
func (s *SuppressionService) HandleExportSuppressions(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}

	rows, err := s.db.Query(`SELECT email, md5_hash, reason, created_at FROM mailing_suppression_entries`)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", "attachment; filename=suppressions.csv")
		w.Write([]byte("email,md5_hash,reason,created_at\n"))
		for rows.Next() {
			var email, md5Hash, reason sql.NullString
			var createdAt time.Time
			if err := rows.Scan(&email, &md5Hash, &reason, &createdAt); err == nil {
				w.Write([]byte(fmt.Sprintf("%s,%s,%s,%s\n", email.String, md5Hash.String, reason.String, createdAt.Format(time.RFC3339))))
			}
		}
		return
	}

	suppressions := []map[string]interface{}{}
	for rows.Next() {
		var email, md5Hash, reason sql.NullString
		var createdAt time.Time
		if err := rows.Scan(&email, &md5Hash, &reason, &createdAt); err == nil {
			suppressions = append(suppressions, map[string]interface{}{
				"email":      email.String,
				"md5_hash":   md5Hash.String,
				"reason":     reason.String,
				"created_at": createdAt,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"suppressions": suppressions})
}

// HandleImportSuppressions imports suppressions from file
func (s *SuppressionService) HandleImportSuppressions(w http.ResponseWriter, r *http.Request) {
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

	listID := r.FormValue("list_id")
	source := r.FormValue("source")
	if source == "" {
		source = "import"
	}

	// Stream file line-by-line to avoid loading entire file into memory
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024) // Allow up to 10MB lines

	added := 0
	totalLines := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		totalLines++

		// Determine if it's email or MD5
		var email, md5Hash string
		if strings.Contains(line, "@") {
			email = strings.ToLower(line)
		} else if len(line) == 32 {
			md5Hash = strings.ToLower(line)
		} else {
			continue
		}

		id := fmt.Sprintf("sup-%d", time.Now().UnixNano())
		_, err := s.db.Exec(`
			INSERT INTO mailing_suppression_entries (id, list_id, email, md5_hash, source)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (list_id, md5_hash) DO NOTHING
		`, id, nullString(listID), nullString(email), nullString(md5Hash), source)
		if err == nil {
			added++
		}
	}
	if err := scanner.Err(); err != nil {
		http.Error(w, `{"error":"failed to read file"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"added":   added,
		"total":   totalLines,
	})
}
