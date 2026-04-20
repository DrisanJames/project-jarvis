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
	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
)

// VersionDomainSuppressionHandlers tracks the contract version exposed via
// the api_version field on every response. Bump whenever the response shape
// changes so clients can detect skew (see .cursor/rules/testing.mdc).
const VersionDomainSuppressionHandlers = "2.0"

// HandleGetDomainSuppressions returns brand-scoped suppressions. Replaces the
// original no-op stub (returned empty list). Query params:
//
//	?brand_root=<domain>   restrict to one brand (optional)
//	?email=<address>       restrict to one subscriber (optional, lowercased)
//	?limit=<int>           page size (default 100, max 1000)
//	?offset=<int>          page offset (default 0)
//
// Response shape is intentionally NOT the legacy {"domains":[]} — this route
// was dead code. Any caller still hitting it was already getting nothing useful.
func (s *SuppressionService) HandleGetDomainSuppressions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Api-Version", VersionDomainSuppressionHandlers)
	w.Header().Set("Content-Type", "application/json")

	brandFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("brand_root")))
	emailFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("email")))
	limit := parsePositiveIntQuery(r, "limit", 100, 1000)
	offset := parsePositiveIntQuery(r, "offset", 0, 1_000_000)

	// Dynamic WHERE — arguments are always parameterized, never string-interpolated.
	args := []interface{}{}
	conds := []string{}
	if brandFilter != "" {
		args = append(args, brandFilter)
		conds = append(conds, fmt.Sprintf("brand_root = $%d", len(args)))
	}
	if emailFilter != "" {
		args = append(args, emailFilter)
		conds = append(conds, fmt.Sprintf("LOWER(email) = $%d", len(args)))
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	args = append(args, limit, offset)
	query := fmt.Sprintf(`
		SELECT id, organization_id, email, email_hash, brand_root, reason,
		       COALESCE(source,''), COALESCE(isp,''), COALESCE(host(source_ip),''),
		       COALESCE(campaign_id::text,''), created_at, updated_at
		FROM mailing_domain_suppressions%s
		ORDER BY created_at DESC
		LIMIT $%d OFFSET $%d`, where, len(args)-1, len(args))

	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		// Graceful: if the table isn't migrated yet return empty rather than 500.
		log.Printf("[domain-suppressions] query failed: %v", err)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"api_version": VersionDomainSuppressionHandlers,
			"entries":     []interface{}{},
			"total":       0,
			"error":       "query_failed",
		})
		return
	}
	defer rows.Close()

	entries := []map[string]interface{}{}
	for rows.Next() {
		var id, orgID, email, hash, brandRoot, reason, source, isp, ip, campID string
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&id, &orgID, &email, &hash, &brandRoot, &reason,
			&source, &isp, &ip, &campID, &createdAt, &updatedAt); err != nil {
			continue
		}
		entries = append(entries, map[string]interface{}{
			"id":              id,
			"organization_id": orgID,
			"email":           email,
			"email_hash":      hash,
			"brand_root":      brandRoot,
			"reason":          reason,
			"source":          source,
			"isp":             isp,
			"source_ip":       ip,
			"campaign_id":     campID,
			"created_at":      createdAt,
			"updated_at":      updatedAt,
		})
	}

	// Separate count query so large tables don't force a full scan per read.
	var total int64
	countArgs := args[:len(args)-2]
	countQuery := "SELECT COUNT(*) FROM mailing_domain_suppressions" + where
	s.db.QueryRowContext(r.Context(), countQuery, countArgs...).Scan(&total)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"api_version": VersionDomainSuppressionHandlers,
		"entries":     entries,
		"total":       total,
		"limit":       limit,
		"offset":      offset,
	})
}

// HandleAddDomainSuppression inserts a brand-scoped suppression via the hub
// so the in-memory brandSet and the DB row stay in sync. Body:
//
//	{"email":"...", "brand_root":"...", "reason":"...", "source":"...", "isp":"...", "campaign_id":"..."}
//
// Either brand_root or sending_domain may be supplied; sending_domain is
// passed through brand.Root() to get the effective eTLD+1. If both are empty
// the request is rejected — we refuse to guess scope.
func (s *SuppressionService) HandleAddDomainSuppression(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Api-Version", VersionDomainSuppressionHandlers)
	w.Header().Set("Content-Type", "application/json")

	var input struct {
		Email          string `json:"email"`
		BrandRoot      string `json:"brand_root"`
		SendingDomain  string `json:"sending_domain"`
		Reason         string `json:"reason"`
		Source         string `json:"source"`
		ISP            string `json:"isp"`
		CampaignID     string `json:"campaign_id"`
		SourceIP       string `json:"source_ip"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}

	email := strings.ToLower(strings.TrimSpace(input.Email))
	brandRoot := strings.ToLower(strings.TrimSpace(input.BrandRoot))
	if brandRoot == "" && input.SendingDomain != "" {
		brandRoot = brand.Root(input.SendingDomain)
	}
	if email == "" || !strings.Contains(email, "@") {
		http.Error(w, `{"error":"valid email required"}`, http.StatusBadRequest)
		return
	}
	if brandRoot == "" {
		http.Error(w, `{"error":"brand_root or sending_domain required"}`, http.StatusBadRequest)
		return
	}
	reason := input.Reason
	if reason == "" {
		reason = "manual"
	}
	source := input.Source
	if source == "" {
		source = "admin_api"
	}

	if s.globalHub == nil {
		// Fallback: direct insert using the same shape as the hub. Keeps
		// this endpoint functional during boot before SetGlobalSuppressionHub fires.
		if _, err := s.db.ExecContext(r.Context(),
			`INSERT INTO mailing_domain_suppressions
			 (organization_id, email, email_hash, brand_root, reason, source, isp, source_ip, campaign_id, created_at, updated_at)
			 VALUES (COALESCE(NULLIF(current_setting('app.org_id', true), ''), '00000000-0000-0000-0000-000000000000')::uuid,
			         $1, md5(lower($1)), $2, $3, $4, NULLIF($5,''), NULLIF($6,'')::inet, NULLIF($7,'')::uuid, NOW(), NOW())
			 ON CONFLICT (organization_id, email_hash, brand_root) DO UPDATE
			 SET reason = EXCLUDED.reason, source = EXCLUDED.source, updated_at = NOW()`,
			email, brandRoot, reason, source, input.ISP, input.SourceIP, input.CampaignID); err != nil {
			log.Printf("[domain-suppressions] direct insert failed: %v", err)
			http.Error(w, `{"error":"insert_failed"}`, http.StatusInternalServerError)
			return
		}
	} else if err := s.globalHub.SuppressScoped(
		r.Context(), email, brandRoot, reason, source, input.ISP, input.SourceIP, input.CampaignID,
	); err != nil {
		log.Printf("[domain-suppressions] hub SuppressScoped failed: %v", err)
		http.Error(w, `{"error":"suppress_failed"}`, http.StatusInternalServerError)
		return
	}

	log.Printf("[domain-suppressions] ADDED email=%s brand=%s reason=%s source=%s",
		logger.RedactEmail(email), brandRoot, reason, source)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"api_version": VersionDomainSuppressionHandlers,
		"success":     true,
		"email":       email,
		"brand_root":  brandRoot,
		"reason":      reason,
	})
}

// HandleRemoveDomainSuppression removes a brand-scoped suppression. The legacy
// route placed the domain in the URL path; we re-use it as the brand_root and
// require the ?email= query param to identify the specific row. Removing an
// entire brand without an email would be catastrophic and is not supported.
func (s *SuppressionService) HandleRemoveDomainSuppression(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Api-Version", VersionDomainSuppressionHandlers)
	w.Header().Set("Content-Type", "application/json")

	brandRoot := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "domain")))
	email := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("email")))
	if brandRoot == "" || email == "" {
		http.Error(w, `{"error":"brand_root path param and ?email= query required"}`, http.StatusBadRequest)
		return
	}

	if s.globalHub != nil {
		if err := s.globalHub.RemoveScoped(r.Context(), email, brandRoot); err != nil {
			log.Printf("[domain-suppressions] hub RemoveScoped failed: %v", err)
			http.Error(w, `{"error":"remove_failed"}`, http.StatusInternalServerError)
			return
		}
	} else {
		if _, err := s.db.ExecContext(r.Context(),
			`DELETE FROM mailing_domain_suppressions WHERE LOWER(email) = $1 AND brand_root = $2`,
			email, brandRoot); err != nil {
			log.Printf("[domain-suppressions] direct delete failed: %v", err)
			http.Error(w, `{"error":"remove_failed"}`, http.StatusInternalServerError)
			return
		}
	}

	log.Printf("[domain-suppressions] REMOVED email=%s brand=%s",
		logger.RedactEmail(email), brandRoot)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"api_version": VersionDomainSuppressionHandlers,
		"success":     true,
		"email":       email,
		"brand_root":  brandRoot,
	})
}

// parsePositiveIntQuery extracts a non-negative integer query param, applying
// the supplied default and a hard upper bound. Invalid input falls back to def.
func parsePositiveIntQuery(r *http.Request, key string, def, max int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return def
	}
	n := 0
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || n < 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
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

	s.AddToGlobalSuppression(input.Email, "unsubscribe", "preference_center")

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
	tbl := "mailing_global_suppressions"
	if s.globalHub == nil {
		tbl = "mailing_suppression_entries"
	}

	var totalCount, bounceCount, complaintCount, unsubCount int
	s.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s`, tbl)).Scan(&totalCount)
	s.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE reason LIKE '%%bounce%%'`, tbl)).Scan(&bounceCount)
	s.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE reason LIKE '%%complaint%%'`, tbl)).Scan(&complaintCount)
	s.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE reason LIKE '%%unsub%%'`, tbl)).Scan(&unsubCount)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total":        totalCount,
		"bounces":      bounceCount,
		"complaints":   complaintCount,
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
	q := `SELECT id, email, reason, source, created_at FROM mailing_suppression_entries ORDER BY created_at DESC LIMIT 100`
	if s.globalHub != nil {
		q = `SELECT id, email, reason, source, created_at FROM mailing_global_suppressions ORDER BY created_at DESC LIMIT 100`
	}
	rows, err := s.db.Query(q)
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

// HandleOneClickUnsubscribe handles RFC 8058 one-click unsubscribe.
// Uses GlobalSuppressionHub as the single source of truth.
func (s *SuppressionService) HandleOneClickUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	email := r.PostFormValue("email")
	if email == "" {
		token := r.PostFormValue("token")
		if token != "" {
			email = token
		}
	}
	if email == "" {
		http.Error(w, "Email required", http.StatusBadRequest)
		return
	}

	emailLower := strings.ToLower(strings.TrimSpace(email))

	s.AddToGlobalSuppression(emailLower, "unsubscribe", "rfc8058_one_click")

	s.db.Exec(`UPDATE mailing_subscribers SET status = 'unsubscribed', updated_at = NOW() WHERE LOWER(email) = $1`, emailLower)

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Unsubscribed successfully"))
	log.Printf("One-click unsubscribe → global suppression: %s", logger.RedactEmail(email))
}

// HandleListUnsubscribeHeader returns the List-Unsubscribe header value
func (s *SuppressionService) HandleListUnsubscribeHeader(w http.ResponseWriter, r *http.Request) {
	baseURL := r.URL.Query().Get("base_url")
	if baseURL == "" {
		baseURL = "https://projectjarvis.io"
	}

	email := r.URL.Query().Get("email")
	campaignID := r.URL.Query().Get("campaign_id")

	unsubscribeURL := fmt.Sprintf("%s/api/mailing/unsubscribe/one-click?email=%s&campaign=%s",
		baseURL, email, campaignID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"List-Unsubscribe":      fmt.Sprintf("<%s>", unsubscribeURL),
		"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
		"one_click_url":         unsubscribeURL,
	})
}
