package api

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// =============================================================================
// SUPPRESSION REFRESH API — HTTP handlers for the suppression refresh system
// =============================================================================

// SuppressionRefreshAPI groups all HTTP handlers for managing suppression
// refresh sources, cycles, logs, and groups.
type SuppressionRefreshAPI struct {
	db     *sql.DB
	engine *SuppressionRefreshEngine
}

// NewSuppressionRefreshAPI creates a new SuppressionRefreshAPI with the given
// database connection and refresh engine.
func NewSuppressionRefreshAPI(db *sql.DB, engine *SuppressionRefreshEngine) *SuppressionRefreshAPI {
	return &SuppressionRefreshAPI{
		db:     db,
		engine: engine,
	}
}

// RegisterRoutes mounts all suppression-refresh routes onto the provided router.
// The parent is expected to mount this at /api/mailing/suppression-refresh.
func (api *SuppressionRefreshAPI) RegisterRoutes(r chi.Router) {
	// Sources CRUD
	r.Get("/sources", api.HandleListSources)
	r.Post("/sources", api.HandleCreateSource)
	r.Post("/sources/bulk-import", api.HandleBulkImportSources)
	r.Get("/sources/{id}", api.HandleGetSource)
	r.Put("/sources/{id}", api.HandleUpdateSource)
	r.Delete("/sources/{id}", api.HandleDeleteSource)
	r.Post("/sources/{id}/test", api.HandleTestSource)
	r.Post("/sources/bulk-update", api.HandleBulkUpdateSources)

	// Engine control
	r.Get("/status", api.HandleGetStatus)
	r.Post("/trigger", api.HandleTriggerCycle)
	r.Post("/stop", api.HandleStopCycle)

	// Cycles
	r.Get("/cycles", api.HandleListCycles)
	r.Get("/cycles/{id}", api.HandleGetCycle)
	r.Get("/cycles/{id}/logs", api.HandleGetCycleLogs)

	// Groups
	r.Get("/groups", api.HandleListGroups)
	r.Post("/groups", api.HandleCreateGroup)
	r.Delete("/groups/{name}", api.HandleDeleteGroup)
}

// =============================================================================
// JSON RESPONSE HELPERS
// =============================================================================

func srWriteJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func srWriteError(w http.ResponseWriter, status int, msg string) {
	srWriteJSON(w, status, map[string]string{"error": msg})
}

// detectProvider is defined in suppression_refresh_engine.go

// =============================================================================
// SOURCE HANDLERS
// =============================================================================

// HandleListSources returns a paginated, filterable list of suppression refresh sources.
// GET /sources?page=1&limit=50&active=true&group=february&provider=optizmo&search=carshield&sort=priority
func (api *SuppressionRefreshAPI) HandleListSources(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := (page - 1) * limit

	// Build WHERE clauses
	var conditions []string
	var args []interface{}
	argIdx := 1

	if active := q.Get("active"); active != "" {
		if active == "true" {
			conditions = append(conditions, fmt.Sprintf("is_active = $%d", argIdx))
			args = append(args, true)
		} else if active == "false" {
			conditions = append(conditions, fmt.Sprintf("is_active = $%d", argIdx))
			args = append(args, false)
		}
		argIdx++
	}
	if group := q.Get("group"); group != "" {
		conditions = append(conditions, fmt.Sprintf("refresh_group = $%d", argIdx))
		args = append(args, group)
		argIdx++
	}
	if provider := q.Get("provider"); provider != "" {
		conditions = append(conditions, fmt.Sprintf("source_provider = $%d", argIdx))
		args = append(args, provider)
		argIdx++
	}
	if search := q.Get("search"); search != "" {
		conditions = append(conditions, fmt.Sprintf("(campaign_name ILIKE $%d OR offer_id ILIKE $%d)", argIdx, argIdx+1))
		like := "%" + search + "%"
		args = append(args, like, like)
		argIdx += 2
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	// Sorting
	sortCol := "priority"
	sortDir := "ASC"
	if s := q.Get("sort"); s != "" {
		switch s {
		case "priority":
			sortCol = "priority"
		case "campaign_name":
			sortCol = "campaign_name"
		case "last_refreshed_at":
			sortCol = "last_refreshed_at"
		case "last_entry_count":
			sortCol = "last_entry_count"
		case "-priority":
			sortCol = "priority"
			sortDir = "DESC"
		case "-campaign_name":
			sortCol = "campaign_name"
			sortDir = "DESC"
		case "-last_refreshed_at":
			sortCol = "last_refreshed_at"
			sortDir = "DESC"
		case "-last_entry_count":
			sortCol = "last_entry_count"
			sortDir = "DESC"
		}
	}
	orderClause := fmt.Sprintf("ORDER BY %s %s, campaign_name ASC", sortCol, sortDir)

	// Count total
	countQuery := "SELECT COUNT(*) FROM suppression_refresh_sources " + whereClause
	var total int64
	if err := api.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		log.Printf("[suppression-refresh] list sources count error: %v", err)
		srWriteError(w, http.StatusInternalServerError, "failed to count sources")
		return
	}

	// Fetch page
	dataQuery := fmt.Sprintf(`SELECT id, offer_id, campaign_name, suppression_url, source_provider,
		ga_suppression_id, internal_list_id, refresh_group, priority, is_active,
		last_refreshed_at, last_entry_count, last_error, notes, created_at, updated_at
		FROM suppression_refresh_sources %s %s LIMIT $%d OFFSET $%d`,
		whereClause, orderClause, argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := api.db.Query(dataQuery, args...)
	if err != nil {
		log.Printf("[suppression-refresh] list sources query error: %v", err)
		srWriteError(w, http.StatusInternalServerError, "failed to query sources")
		return
	}
	defer rows.Close()

	sources := make([]map[string]interface{}, 0)
	for rows.Next() {
		var (
			id, offerID, campaignName, suppressionURL, sourceProvider string
			gaSuppID, internalListID, refreshGroup, lastError, notes sql.NullString
			priority                                                 int
			isActive                                                 bool
			lastRefreshedAt                                          sql.NullTime
			lastEntryCount                                           sql.NullInt64
			createdAt, updatedAt                                     time.Time
		)
		if err := rows.Scan(&id, &offerID, &campaignName, &suppressionURL, &sourceProvider,
			&gaSuppID, &internalListID, &refreshGroup, &priority, &isActive,
			&lastRefreshedAt, &lastEntryCount, &lastError, &notes, &createdAt, &updatedAt); err != nil {
			log.Printf("[suppression-refresh] scan source error: %v", err)
			continue
		}
		source := map[string]interface{}{
			"id":                id,
			"offer_id":         offerID,
			"campaign_name":    campaignName,
			"suppression_url":  suppressionURL,
			"source_provider":  sourceProvider,
			"priority":         priority,
			"is_active":        isActive,
			"created_at":       createdAt.Format(time.RFC3339),
			"updated_at":       updatedAt.Format(time.RFC3339),
		}
		if gaSuppID.Valid {
			source["ga_suppression_id"] = gaSuppID.String
		}
		if internalListID.Valid {
			source["internal_list_id"] = internalListID.String
		}
		if refreshGroup.Valid {
			source["refresh_group"] = refreshGroup.String
		}
		if lastRefreshedAt.Valid {
			source["last_refreshed_at"] = lastRefreshedAt.Time.Format(time.RFC3339)
		}
		if lastEntryCount.Valid {
			source["last_entry_count"] = lastEntryCount.Int64
		}
		if lastError.Valid {
			source["last_error"] = lastError.String
		}
		if notes.Valid {
			source["notes"] = notes.String
		}
		sources = append(sources, source)
	}

	totalPages := int((total + int64(limit) - 1) / int64(limit))
	if totalPages < 1 {
		totalPages = 1
	}

	srWriteJSON(w, http.StatusOK, map[string]interface{}{
		"data":        sources,
		"total":       total,
		"page":        page,
		"limit":       limit,
		"total_pages": totalPages,
	})
}

// HandleCreateSource creates a new suppression refresh source.
// POST /sources
func (api *SuppressionRefreshAPI) HandleCreateSource(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OfferID        string `json:"offer_id"`
		CampaignName   string `json:"campaign_name"`
		SuppressionURL string `json:"suppression_url"`
		GASuppressionID string `json:"ga_suppression_id"`
		InternalListID string `json:"internal_list_id"`
		RefreshGroup   string `json:"refresh_group"`
		Priority       int    `json:"priority"`
		Notes          string `json:"notes"`
		IsActive       *bool  `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		srWriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.SuppressionURL == "" {
		srWriteError(w, http.StatusBadRequest, "suppression_url is required")
		return
	}

	id := uuid.New().String()
	provider := detectProvider(body.SuppressionURL)
	isActive := true
	if body.IsActive != nil {
		isActive = *body.IsActive
	}

	now := time.Now()
	_, err := api.db.Exec(`INSERT INTO suppression_refresh_sources
		(id, offer_id, campaign_name, suppression_url, source_provider,
		 ga_suppression_id, internal_list_id, refresh_group, priority, is_active,
		 notes, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		id, body.OfferID, body.CampaignName, body.SuppressionURL, provider,
		srNullStr(body.GASuppressionID), srNullStr(body.InternalListID), srNullStr(body.RefreshGroup),
		body.Priority, isActive, srNullStr(body.Notes), now, now)
	if err != nil {
		log.Printf("[suppression-refresh] create source error: %v", err)
		srWriteError(w, http.StatusInternalServerError, "failed to create source")
		return
	}

	srWriteJSON(w, http.StatusCreated, map[string]interface{}{
		"id":               id,
		"offer_id":         body.OfferID,
		"campaign_name":    body.CampaignName,
		"suppression_url":  body.SuppressionURL,
		"source_provider":  provider,
		"ga_suppression_id": body.GASuppressionID,
		"internal_list_id": body.InternalListID,
		"refresh_group":    body.RefreshGroup,
		"priority":         body.Priority,
		"is_active":        isActive,
		"notes":            body.Notes,
		"created_at":       now.Format(time.RFC3339),
		"updated_at":       now.Format(time.RFC3339),
	})
}

// HandleBulkImportSources imports sources from a CSV file upload.
// POST /sources/bulk-import  (multipart form with "file" field)
func (api *SuppressionRefreshAPI) HandleBulkImportSources(w http.ResponseWriter, r *http.Request) {
	// Limit to 10 MB
	r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		srWriteError(w, http.StatusBadRequest, "failed to parse multipart form (max 10MB)")
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		srWriteError(w, http.StatusBadRequest, "no file provided")
		return
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.LazyQuotes = true
	reader.TrimLeadingSpace = true

	// Read header row
	header, err := reader.Read()
	if err != nil {
		srWriteError(w, http.StatusBadRequest, "failed to read CSV header")
		return
	}

	// Map header columns (case-insensitive)
	colMap := make(map[string]int)
	for i, h := range header {
		colMap[strings.ToLower(strings.TrimSpace(h))] = i
	}

	offerIDCol, hasOfferID := colMap["offer id"]
	campaignCol, hasCampaign := colMap["campaign name"]
	urlCol, hasURL := colMap["advertiser suppression links"]
	gaIDCol, hasGAID := colMap["ga suppression id"]

	if !hasURL {
		srWriteError(w, http.StatusBadRequest, "CSV must contain 'Advertiser Suppression Links' column")
		return
	}

	var imported, skipped, updated int
	var errors []map[string]interface{}
	rowNum := 1 // header was row 0

	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		rowNum++
		if err != nil {
			errors = append(errors, map[string]interface{}{"row": rowNum, "error": fmt.Sprintf("parse error: %v", err)})
			continue
		}

		// Extract fields
		suppressionURL := ""
		if hasURL && urlCol < len(record) {
			suppressionURL = strings.TrimSpace(record[urlCol])
		}
		if suppressionURL == "" {
			skipped++
			continue
		}

		offerID := ""
		if hasOfferID && offerIDCol < len(record) {
			offerID = strings.TrimSpace(record[offerIDCol])
		}
		campaignName := ""
		if hasCampaign && campaignCol < len(record) {
			campaignName = strings.TrimSpace(record[campaignCol])
		}
		gaSuppressionID := ""
		if hasGAID && gaIDCol < len(record) {
			gaSuppressionID = strings.TrimSpace(record[gaIDCol])
		}

		provider := detectProvider(suppressionURL)

		// Try to match ga_suppression_id to an existing mailing_suppression_lists entry
		var internalListID sql.NullString
		if gaSuppressionID != "" {
			var matchedID string
			matchErr := api.db.QueryRow(`SELECT id FROM mailing_suppression_lists
				WHERE id = $1 OR name ILIKE '%' || $2 || '%' LIMIT 1`,
				gaSuppressionID, gaSuppressionID).Scan(&matchedID)
			if matchErr == nil {
				internalListID = sql.NullString{String: matchedID, Valid: true}
			}
		}

		// Check if source already exists by suppression_url (or offer_id + suppression_url)
		now := time.Now()
		var existingID string
		err = api.db.QueryRow(
			`SELECT id FROM suppression_refresh_sources WHERE suppression_url = $1 LIMIT 1`,
			suppressionURL).Scan(&existingID)

		if err == sql.ErrNoRows {
			// INSERT new source
			newID := uuid.New().String()
			_, insertErr := api.db.Exec(`INSERT INTO suppression_refresh_sources
				(id, offer_id, campaign_name, suppression_url, source_provider,
				 ga_suppression_id, internal_list_id, is_active, priority, created_at, updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,true,100,$8,$9)`,
				newID, offerID, campaignName, suppressionURL, provider,
				srNullStr(gaSuppressionID), internalListID,
				now, now)
			if insertErr != nil {
				errors = append(errors, map[string]interface{}{"row": rowNum, "error": insertErr.Error()})
				continue
			}
			imported++
		} else if err == nil {
			// UPDATE existing source (merge non-empty fields)
			_, updateErr := api.db.Exec(`UPDATE suppression_refresh_sources SET
				offer_id = COALESCE(NULLIF($2,''), offer_id),
				campaign_name = COALESCE(NULLIF($3,''), campaign_name),
				source_provider = $4,
				ga_suppression_id = COALESCE($5, ga_suppression_id),
				internal_list_id = COALESCE($6, internal_list_id),
				updated_at = $7
				WHERE id = $1`,
				existingID, offerID, campaignName, provider,
				srNullStr(gaSuppressionID), internalListID, now)
			if updateErr != nil {
				errors = append(errors, map[string]interface{}{"row": rowNum, "error": updateErr.Error()})
				continue
			}
			updated++
		} else {
			errors = append(errors, map[string]interface{}{"row": rowNum, "error": err.Error()})
			continue
		}
	}

	srWriteJSON(w, http.StatusOK, map[string]interface{}{
		"imported": imported,
		"skipped":  skipped,
		"updated":  updated,
		"errors":   errors,
	})
}

// HandleGetSource returns a single source with its recent refresh logs.
// GET /sources/{id}
func (api *SuppressionRefreshAPI) HandleGetSource(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var (
		offerID, campaignName, suppressionURL, sourceProvider string
		gaSuppID, internalListID, refreshGroup, lastError, notes sql.NullString
		priority                                                  int
		isActive                                                  bool
		lastRefreshedAt                                           sql.NullTime
		lastEntryCount                                            sql.NullInt64
		createdAt, updatedAt                                      time.Time
	)
	err := api.db.QueryRow(`SELECT id, offer_id, campaign_name, suppression_url, source_provider,
		ga_suppression_id, internal_list_id, refresh_group, priority, is_active,
		last_refreshed_at, last_entry_count, last_error, notes, created_at, updated_at
		FROM suppression_refresh_sources WHERE id = $1`, id).Scan(
		&id, &offerID, &campaignName, &suppressionURL, &sourceProvider,
		&gaSuppID, &internalListID, &refreshGroup, &priority, &isActive,
		&lastRefreshedAt, &lastEntryCount, &lastError, &notes, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		srWriteError(w, http.StatusNotFound, "source not found")
		return
	}
	if err != nil {
		log.Printf("[suppression-refresh] get source error: %v", err)
		srWriteError(w, http.StatusInternalServerError, "failed to get source")
		return
	}

	source := map[string]interface{}{
		"id":              id,
		"offer_id":        offerID,
		"campaign_name":   campaignName,
		"suppression_url": suppressionURL,
		"source_provider": sourceProvider,
		"priority":        priority,
		"is_active":       isActive,
		"created_at":      createdAt.Format(time.RFC3339),
		"updated_at":      updatedAt.Format(time.RFC3339),
	}
	if gaSuppID.Valid {
		source["ga_suppression_id"] = gaSuppID.String
	}
	if internalListID.Valid {
		source["internal_list_id"] = internalListID.String
	}
	if refreshGroup.Valid {
		source["refresh_group"] = refreshGroup.String
	}
	if lastRefreshedAt.Valid {
		source["last_refreshed_at"] = lastRefreshedAt.Time.Format(time.RFC3339)
	}
	if lastEntryCount.Valid {
		source["last_entry_count"] = lastEntryCount.Int64
	}
	if lastError.Valid {
		source["last_error"] = lastError.String
	}
	if notes.Valid {
		source["notes"] = notes.String
	}

	// Recent refresh logs
	logRows, err := api.db.Query(`SELECT id, cycle_id, source_id, status, entries_downloaded,
		entries_new, download_ms, processing_ms, error_message,
		started_at, completed_at
		FROM suppression_refresh_logs WHERE source_id = $1
		ORDER BY started_at DESC LIMIT 10`, id)
	if err != nil {
		log.Printf("[suppression-refresh] get source logs error: %v", err)
		source["recent_logs"] = []interface{}{}
	} else {
		defer logRows.Close()
		logs := make([]map[string]interface{}, 0)
		for logRows.Next() {
			l := scanRefreshLog(logRows)
			if l != nil {
				logs = append(logs, l)
			}
		}
		source["recent_logs"] = logs
	}

	srWriteJSON(w, http.StatusOK, source)
}

// HandleUpdateSource performs a partial update of a source.
// PUT /sources/{id}
func (api *SuppressionRefreshAPI) HandleUpdateSource(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	// Decode into a map to detect which fields are present
	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		srWriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	var setClauses []string
	var args []interface{}
	argIdx := 1

	if v, ok := body["campaign_name"]; ok {
		setClauses = append(setClauses, fmt.Sprintf("campaign_name = $%d", argIdx))
		args = append(args, v)
		argIdx++
	}
	if v, ok := body["suppression_url"]; ok {
		url := fmt.Sprintf("%v", v)
		setClauses = append(setClauses, fmt.Sprintf("suppression_url = $%d", argIdx))
		args = append(args, url)
		argIdx++
		// Re-detect provider when URL changes
		setClauses = append(setClauses, fmt.Sprintf("source_provider = $%d", argIdx))
		args = append(args, detectProvider(url))
		argIdx++
	}
	if v, ok := body["is_active"]; ok {
		setClauses = append(setClauses, fmt.Sprintf("is_active = $%d", argIdx))
		args = append(args, v)
		argIdx++
	}
	if v, ok := body["refresh_group"]; ok {
		setClauses = append(setClauses, fmt.Sprintf("refresh_group = $%d", argIdx))
		args = append(args, nullStrFromInterface(v))
		argIdx++
	}
	if v, ok := body["priority"]; ok {
		setClauses = append(setClauses, fmt.Sprintf("priority = $%d", argIdx))
		args = append(args, v)
		argIdx++
	}
	if v, ok := body["notes"]; ok {
		setClauses = append(setClauses, fmt.Sprintf("notes = $%d", argIdx))
		args = append(args, nullStrFromInterface(v))
		argIdx++
	}
	if v, ok := body["internal_list_id"]; ok {
		setClauses = append(setClauses, fmt.Sprintf("internal_list_id = $%d", argIdx))
		args = append(args, nullStrFromInterface(v))
		argIdx++
	}

	if len(setClauses) == 0 {
		srWriteError(w, http.StatusBadRequest, "no fields to update")
		return
	}

	setClauses = append(setClauses, fmt.Sprintf("updated_at = $%d", argIdx))
	args = append(args, time.Now())
	argIdx++

	args = append(args, id)
	query := fmt.Sprintf("UPDATE suppression_refresh_sources SET %s WHERE id = $%d",
		strings.Join(setClauses, ", "), argIdx)

	res, err := api.db.Exec(query, args...)
	if err != nil {
		log.Printf("[suppression-refresh] update source error: %v", err)
		srWriteError(w, http.StatusInternalServerError, "failed to update source")
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		srWriteError(w, http.StatusNotFound, "source not found")
		return
	}

	srWriteJSON(w, http.StatusOK, map[string]interface{}{
		"id":      id,
		"updated": true,
	})
}

// HandleDeleteSource removes a source by id.
// DELETE /sources/{id}
func (api *SuppressionRefreshAPI) HandleDeleteSource(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	res, err := api.db.Exec("DELETE FROM suppression_refresh_sources WHERE id = $1", id)
	if err != nil {
		log.Printf("[suppression-refresh] delete source error: %v", err)
		srWriteError(w, http.StatusInternalServerError, "failed to delete source")
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		srWriteError(w, http.StatusNotFound, "source not found")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleTestSource downloads a source's suppression URL and returns a preview.
// POST /sources/{id}/test
func (api *SuppressionRefreshAPI) HandleTestSource(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var suppressionURL string
	err := api.db.QueryRow("SELECT suppression_url FROM suppression_refresh_sources WHERE id = $1", id).Scan(&suppressionURL)
	if err == sql.ErrNoRows {
		srWriteError(w, http.StatusNotFound, "source not found")
		return
	}
	if err != nil {
		log.Printf("[suppression-refresh] test source query error: %v", err)
		srWriteError(w, http.StatusInternalServerError, "failed to read source")
		return
	}

	client := &http.Client{Timeout: 30 * time.Second}
	start := time.Now()
	resp, err := client.Get(suppressionURL)
	downloadMs := time.Since(start).Milliseconds()

	if err != nil {
		srWriteJSON(w, http.StatusOK, map[string]interface{}{
			"status":      "failed",
			"error":       err.Error(),
			"download_ms": downloadMs,
		})
		return
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")

	// Read up to 1MB for preview
	limited := io.LimitReader(resp.Body, 1*1024*1024)
	bodyBytes, err := io.ReadAll(limited)
	if err != nil {
		srWriteJSON(w, http.StatusOK, map[string]interface{}{
			"status":       "failed",
			"http_status":  resp.StatusCode,
			"content_type": contentType,
			"error":        fmt.Sprintf("read error: %v", err),
			"download_ms":  downloadMs,
		})
		return
	}

	// Parse lines for preview
	lines := strings.Split(string(bodyBytes), "\n")
	previewLines := lines
	if len(previewLines) > 20 {
		previewLines = previewLines[:20]
	}
	// Trim empty trailing lines from preview
	for len(previewLines) > 0 && strings.TrimSpace(previewLines[len(previewLines)-1]) == "" {
		previewLines = previewLines[:len(previewLines)-1]
	}

	// Estimate total entries (non-empty lines)
	estimatedEntries := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			estimatedEntries++
		}
	}

	status := "success"
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status = "failed"
	}

	srWriteJSON(w, http.StatusOK, map[string]interface{}{
		"status":            status,
		"http_status":       resp.StatusCode,
		"content_type":      contentType,
		"preview_lines":     previewLines,
		"estimated_entries": estimatedEntries,
		"download_ms":       downloadMs,
	})
}

// HandleBulkUpdateSources applies a bulk action to multiple sources.
// POST /sources/bulk-update
func (api *SuppressionRefreshAPI) HandleBulkUpdateSources(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SourceIDs []string `json:"source_ids"`
		Action    string   `json:"action"`
		Group     string   `json:"group"`
		Priority  *int     `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		srWriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(body.SourceIDs) == 0 {
		srWriteError(w, http.StatusBadRequest, "source_ids is required")
		return
	}

	// Build placeholder list for IN clause
	placeholders := make([]string, len(body.SourceIDs))
	args := make([]interface{}, len(body.SourceIDs))
	for i, sid := range body.SourceIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+2)
		args[i] = sid
	}
	inClause := strings.Join(placeholders, ",")

	var query string
	now := time.Now()

	switch body.Action {
	case "activate":
		query = fmt.Sprintf("UPDATE suppression_refresh_sources SET is_active = true, updated_at = $1 WHERE id IN (%s)", inClause)
	case "deactivate":
		query = fmt.Sprintf("UPDATE suppression_refresh_sources SET is_active = false, updated_at = $1 WHERE id IN (%s)", inClause)
	case "set_group":
		query = fmt.Sprintf("UPDATE suppression_refresh_sources SET refresh_group = $%d, updated_at = $1 WHERE id IN (%s)",
			len(body.SourceIDs)+2, inClause)
		args = append(args, srNullStr(body.Group))
	case "set_priority":
		if body.Priority == nil {
			srWriteError(w, http.StatusBadRequest, "priority is required for set_priority action")
			return
		}
		query = fmt.Sprintf("UPDATE suppression_refresh_sources SET priority = $%d, updated_at = $1 WHERE id IN (%s)",
			len(body.SourceIDs)+2, inClause)
		args = append(args, *body.Priority)
	default:
		srWriteError(w, http.StatusBadRequest, "action must be one of: activate, deactivate, set_group, set_priority")
		return
	}

	// Prepend the timestamp as $1
	allArgs := append([]interface{}{now}, args...)

	res, err := api.db.Exec(query, allArgs...)
	if err != nil {
		log.Printf("[suppression-refresh] bulk update error: %v", err)
		srWriteError(w, http.StatusInternalServerError, "failed to bulk update sources")
		return
	}
	affected, _ := res.RowsAffected()

	srWriteJSON(w, http.StatusOK, map[string]interface{}{
		"updated": affected,
	})
}

// =============================================================================
// ENGINE CONTROL HANDLERS
// =============================================================================

// HandleGetStatus returns the current engine status.
// GET /status
func (api *SuppressionRefreshAPI) HandleGetStatus(w http.ResponseWriter, r *http.Request) {
	if api.engine == nil {
		srWriteError(w, http.StatusServiceUnavailable, "refresh engine not initialized")
		return
	}
	status := api.engine.GetStatus()
	srWriteJSON(w, http.StatusOK, status)
}

// HandleTriggerCycle starts a manual refresh cycle.
// POST /trigger
func (api *SuppressionRefreshAPI) HandleTriggerCycle(w http.ResponseWriter, r *http.Request) {
	if api.engine == nil {
		srWriteError(w, http.StatusServiceUnavailable, "refresh engine not initialized")
		return
	}

	cycleID, err := api.engine.ManualTrigger()
	if err != nil {
		log.Printf("[suppression-refresh] manual trigger error: %v", err)
		srWriteError(w, http.StatusInternalServerError, fmt.Sprintf("failed to trigger cycle: %v", err))
		return
	}

	srWriteJSON(w, http.StatusOK, map[string]interface{}{
		"cycle_id": cycleID,
		"message":  "Refresh cycle started",
	})
}

// HandleStopCycle requests the engine to stop the current cycle.
// POST /stop
func (api *SuppressionRefreshAPI) HandleStopCycle(w http.ResponseWriter, r *http.Request) {
	if api.engine == nil {
		srWriteError(w, http.StatusServiceUnavailable, "refresh engine not initialized")
		return
	}

	api.engine.Stop()

	// Cancel any running cycle in the DB
	_, err := api.db.Exec(`UPDATE suppression_refresh_cycles SET status = 'cancelled', completed_at = NOW()
		WHERE status = 'running'`)
	if err != nil {
		log.Printf("[suppression-refresh] stop cycle db update error: %v", err)
	}

	srWriteJSON(w, http.StatusOK, map[string]interface{}{
		"message": "Cycle stop requested",
	})
}

// =============================================================================
// CYCLE HANDLERS
// =============================================================================

// HandleListCycles returns paginated cycle history.
// GET /cycles?page=1&limit=20
func (api *SuppressionRefreshAPI) HandleListCycles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit < 1 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	offset := (page - 1) * limit

	var total int64
	if err := api.db.QueryRow("SELECT COUNT(*) FROM suppression_refresh_cycles").Scan(&total); err != nil {
		log.Printf("[suppression-refresh] list cycles count error: %v", err)
		srWriteError(w, http.StatusInternalServerError, "failed to count cycles")
		return
	}

	rows, err := api.db.Query(`SELECT id, status, triggered_by, total_sources, completed_sources,
		failed_sources, total_entries_downloaded, total_new_entries, started_at, completed_at
		FROM suppression_refresh_cycles ORDER BY started_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		log.Printf("[suppression-refresh] list cycles query error: %v", err)
		srWriteError(w, http.StatusInternalServerError, "failed to query cycles")
		return
	}
	defer rows.Close()

	cycles := make([]map[string]interface{}, 0)
	for rows.Next() {
		var (
			cycleID, status, triggeredBy string
			sourcesTotal, sourcesDone, sourcesFailed,
			totalEntriesDownloaded, totalNewEntries sql.NullInt64
			startedAt   time.Time
			completedAt sql.NullTime
		)
		if err := rows.Scan(&cycleID, &status, &triggeredBy, &sourcesTotal, &sourcesDone,
			&sourcesFailed, &totalEntriesDownloaded, &totalNewEntries, &startedAt, &completedAt); err != nil {
			log.Printf("[suppression-refresh] scan cycle error: %v", err)
			continue
		}
		cycle := map[string]interface{}{
			"id":           cycleID,
			"status":       status,
			"triggered_by": triggeredBy,
			"started_at":   startedAt.Format(time.RFC3339),
		}
		if sourcesTotal.Valid {
			cycle["total_sources"] = sourcesTotal.Int64
		}
		if sourcesDone.Valid {
			cycle["completed_sources"] = sourcesDone.Int64
		}
		if sourcesFailed.Valid {
			cycle["failed_sources"] = sourcesFailed.Int64
		}
		if totalEntriesDownloaded.Valid {
			cycle["total_entries_downloaded"] = totalEntriesDownloaded.Int64
		}
		if totalNewEntries.Valid {
			cycle["total_new_entries"] = totalNewEntries.Int64
		}
		if completedAt.Valid {
			cycle["completed_at"] = completedAt.Time.Format(time.RFC3339)
		}
		cycles = append(cycles, cycle)
	}

	totalPages := int((total + int64(limit) - 1) / int64(limit))
	if totalPages < 1 {
		totalPages = 1
	}

	srWriteJSON(w, http.StatusOK, map[string]interface{}{
		"data":        cycles,
		"total":       total,
		"page":        page,
		"limit":       limit,
		"total_pages": totalPages,
	})
}

// HandleGetCycle returns a single cycle with summary statistics.
// GET /cycles/{id}
func (api *SuppressionRefreshAPI) HandleGetCycle(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var (
		cycleID, status, triggeredBy string
		sourcesTotal, sourcesDone, sourcesFailed,
		totalEntriesDownloaded, totalNewEntries sql.NullInt64
		startedAt   time.Time
		completedAt sql.NullTime
	)
	err := api.db.QueryRow(`SELECT id, status, triggered_by, total_sources, completed_sources,
		failed_sources, total_entries_downloaded, total_new_entries, started_at, completed_at
		FROM suppression_refresh_cycles WHERE id = $1`, id).Scan(
		&cycleID, &status, &triggeredBy, &sourcesTotal, &sourcesDone,
		&sourcesFailed, &totalEntriesDownloaded, &totalNewEntries, &startedAt, &completedAt)
	if err == sql.ErrNoRows {
		srWriteError(w, http.StatusNotFound, "cycle not found")
		return
	}
	if err != nil {
		log.Printf("[suppression-refresh] get cycle error: %v", err)
		srWriteError(w, http.StatusInternalServerError, "failed to get cycle")
		return
	}

	cycle := map[string]interface{}{
		"id":           cycleID,
		"status":       status,
		"triggered_by": triggeredBy,
		"started_at":   startedAt.Format(time.RFC3339),
	}
	if sourcesTotal.Valid {
		cycle["total_sources"] = sourcesTotal.Int64
	}
	if sourcesDone.Valid {
		cycle["completed_sources"] = sourcesDone.Int64
	}
	if sourcesFailed.Valid {
		cycle["failed_sources"] = sourcesFailed.Int64
	}
	if totalEntriesDownloaded.Valid {
		cycle["total_entries_downloaded"] = totalEntriesDownloaded.Int64
	}
	if totalNewEntries.Valid {
		cycle["total_new_entries"] = totalNewEntries.Int64
	}
	if completedAt.Valid {
		cycle["completed_at"] = completedAt.Time.Format(time.RFC3339)
		// Include duration
		dur := completedAt.Time.Sub(startedAt)
		cycle["duration_seconds"] = int(dur.Seconds())
	}

	srWriteJSON(w, http.StatusOK, cycle)
}

// HandleGetCycleLogs returns logs for a specific cycle with optional status filter.
// GET /cycles/{id}/logs?status=failed&page=1&limit=50
func (api *SuppressionRefreshAPI) HandleGetCycleLogs(w http.ResponseWriter, r *http.Request) {
	cycleID := chi.URLParam(r, "id")
	q := r.URL.Query()

	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := (page - 1) * limit

	var conditions []string
	var args []interface{}
	argIdx := 1

	conditions = append(conditions, fmt.Sprintf("l.cycle_id = $%d", argIdx))
	args = append(args, cycleID)
	argIdx++

	if status := q.Get("status"); status != "" {
		conditions = append(conditions, fmt.Sprintf("l.status = $%d", argIdx))
		args = append(args, status)
		argIdx++
	}

	whereClause := "WHERE " + strings.Join(conditions, " AND ")

	// Count
	var total int64
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM suppression_refresh_logs l %s", whereClause)
	if err := api.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		log.Printf("[suppression-refresh] cycle logs count error: %v", err)
		srWriteError(w, http.StatusInternalServerError, "failed to count logs")
		return
	}

	// Fetch with joined source data
	dataQuery := fmt.Sprintf(`SELECT l.id, l.cycle_id, l.source_id, l.status, l.entries_downloaded,
		l.entries_new, l.download_ms, l.processing_ms, l.error_message,
		l.started_at, l.completed_at,
		COALESCE(s.campaign_name, '') as campaign_name,
		COALESCE(s.offer_id, '') as offer_id
		FROM suppression_refresh_logs l
		LEFT JOIN suppression_refresh_sources s ON s.id = l.source_id
		%s ORDER BY l.started_at DESC LIMIT $%d OFFSET $%d`,
		whereClause, argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := api.db.Query(dataQuery, args...)
	if err != nil {
		log.Printf("[suppression-refresh] cycle logs query error: %v", err)
		srWriteError(w, http.StatusInternalServerError, "failed to query logs")
		return
	}
	defer rows.Close()

	logs := make([]map[string]interface{}, 0)
	for rows.Next() {
		var (
			logID, logCycleID, sourceID, logStatus string
			entriesDownloaded, entriesNew,
			downloadMs, processingMs sql.NullInt64
			errorMsg               sql.NullString
			logStartedAt           time.Time
			logCompletedAt         sql.NullTime
			campaignName, offerID  string
		)
		if err := rows.Scan(&logID, &logCycleID, &sourceID, &logStatus,
			&entriesDownloaded, &entriesNew, &downloadMs, &processingMs, &errorMsg,
			&logStartedAt, &logCompletedAt, &campaignName, &offerID); err != nil {
			log.Printf("[suppression-refresh] scan cycle log error: %v", err)
			continue
		}
		entry := map[string]interface{}{
			"id":            logID,
			"cycle_id":      logCycleID,
			"source_id":     sourceID,
			"status":        logStatus,
			"started_at":    logStartedAt.Format(time.RFC3339),
			"campaign_name": campaignName,
			"offer_id":      offerID,
		}
		if entriesDownloaded.Valid {
			entry["entries_downloaded"] = entriesDownloaded.Int64
		}
		if entriesNew.Valid {
			entry["entries_new"] = entriesNew.Int64
		}
		if downloadMs.Valid {
			entry["download_ms"] = downloadMs.Int64
		}
		if processingMs.Valid {
			entry["processing_ms"] = processingMs.Int64
		}
		if errorMsg.Valid {
			entry["error_message"] = errorMsg.String
		}
		if logCompletedAt.Valid {
			entry["completed_at"] = logCompletedAt.Time.Format(time.RFC3339)
		}
		logs = append(logs, entry)
	}

	totalPages := int((total + int64(limit) - 1) / int64(limit))
	if totalPages < 1 {
		totalPages = 1
	}

	srWriteJSON(w, http.StatusOK, map[string]interface{}{
		"data":        logs,
		"total":       total,
		"page":        page,
		"limit":       limit,
		"total_pages": totalPages,
	})
}

// =============================================================================
// GROUP HANDLERS
// =============================================================================

// HandleListGroups returns all groups with active source counts.
// GET /groups
func (api *SuppressionRefreshAPI) HandleListGroups(w http.ResponseWriter, r *http.Request) {
	// Fetch groups from table
	groupRows, err := api.db.Query(`SELECT name, description, created_at FROM suppression_refresh_groups ORDER BY name`)
	if err != nil {
		log.Printf("[suppression-refresh] list groups error: %v", err)
		srWriteError(w, http.StatusInternalServerError, "failed to query groups")
		return
	}
	defer groupRows.Close()

	type groupInfo struct {
		Name         string `json:"name"`
		Description  string `json:"description,omitempty"`
		CreatedAt    string `json:"created_at"`
		ActiveSources int   `json:"active_sources"`
	}
	groupMap := make(map[string]*groupInfo)
	groups := make([]*groupInfo, 0)

	for groupRows.Next() {
		var name string
		var description sql.NullString
		var createdAt time.Time
		if err := groupRows.Scan(&name, &description, &createdAt); err != nil {
			log.Printf("[suppression-refresh] scan group error: %v", err)
			continue
		}
		g := &groupInfo{
			Name:      name,
			CreatedAt: createdAt.Format(time.RFC3339),
		}
		if description.Valid {
			g.Description = description.String
		}
		groupMap[name] = g
		groups = append(groups, g)
	}

	// Count active sources per group
	countRows, err := api.db.Query(`SELECT refresh_group, COUNT(*) as count
		FROM suppression_refresh_sources
		WHERE refresh_group IS NOT NULL AND is_active = true
		GROUP BY refresh_group`)
	if err != nil {
		log.Printf("[suppression-refresh] group source counts error: %v", err)
	} else {
		defer countRows.Close()
		for countRows.Next() {
			var groupName string
			var count int
			if err := countRows.Scan(&groupName, &count); err != nil {
				continue
			}
			if g, ok := groupMap[groupName]; ok {
				g.ActiveSources = count
			} else {
				// Group exists in sources but not in groups table — include it
				groups = append(groups, &groupInfo{
					Name:          groupName,
					ActiveSources: count,
				})
			}
		}
	}

	srWriteJSON(w, http.StatusOK, groups)
}

// HandleCreateGroup creates a new group.
// POST /groups
func (api *SuppressionRefreshAPI) HandleCreateGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		srWriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Name == "" {
		srWriteError(w, http.StatusBadRequest, "name is required")
		return
	}

	now := time.Now()
	_, err := api.db.Exec(`INSERT INTO suppression_refresh_groups (name, description, created_at)
		VALUES ($1, $2, $3)`, body.Name, srNullStr(body.Description), now)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
			srWriteError(w, http.StatusConflict, "group already exists")
			return
		}
		log.Printf("[suppression-refresh] create group error: %v", err)
		srWriteError(w, http.StatusInternalServerError, "failed to create group")
		return
	}

	srWriteJSON(w, http.StatusCreated, map[string]interface{}{
		"name":        body.Name,
		"description": body.Description,
		"created_at":  now.Format(time.RFC3339),
	})
}

// HandleDeleteGroup deletes a group and un-assigns sources from it.
// DELETE /groups/{name}
func (api *SuppressionRefreshAPI) HandleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	// Unset group on all sources
	_, err := api.db.Exec(`UPDATE suppression_refresh_sources SET refresh_group = NULL, updated_at = NOW()
		WHERE refresh_group = $1`, name)
	if err != nil {
		log.Printf("[suppression-refresh] unset group sources error: %v", err)
	}

	res, err := api.db.Exec("DELETE FROM suppression_refresh_groups WHERE name = $1", name)
	if err != nil {
		log.Printf("[suppression-refresh] delete group error: %v", err)
		srWriteError(w, http.StatusInternalServerError, "failed to delete group")
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		srWriteError(w, http.StatusNotFound, "group not found")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// =============================================================================
// HELPER FUNCTIONS
// =============================================================================

// srNullStr converts a Go string to sql.NullString (NULL if empty).
func srNullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// nullStrFromInterface converts an interface{} to sql.NullString.
func nullStrFromInterface(v interface{}) sql.NullString {
	if v == nil {
		return sql.NullString{}
	}
	s := fmt.Sprintf("%v", v)
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// scanRefreshLog scans a single refresh log row from the provided *sql.Rows.
// Expected SELECT order: id, cycle_id, source_id, status, entries_downloaded,
// entries_new, download_ms, processing_ms, error_message, started_at, completed_at
func scanRefreshLog(rows *sql.Rows) map[string]interface{} {
	var (
		logID, cycleID, sourceID, status string
		entriesDownloaded, entriesNew,
		downloadMs, processingMs sql.NullInt64
		errorMsg    sql.NullString
		startedAt   time.Time
		completedAt sql.NullTime
	)
	if err := rows.Scan(&logID, &cycleID, &sourceID, &status,
		&entriesDownloaded, &entriesNew, &downloadMs, &processingMs, &errorMsg,
		&startedAt, &completedAt); err != nil {
		log.Printf("[suppression-refresh] scan log error: %v", err)
		return nil
	}

	entry := map[string]interface{}{
		"id":         logID,
		"cycle_id":   cycleID,
		"source_id":  sourceID,
		"status":     status,
		"started_at": startedAt.Format(time.RFC3339),
	}
	if entriesDownloaded.Valid {
		entry["entries_downloaded"] = entriesDownloaded.Int64
	}
	if entriesNew.Valid {
		entry["entries_new"] = entriesNew.Int64
	}
	if downloadMs.Valid {
		entry["download_ms"] = downloadMs.Int64
	}
	if processingMs.Valid {
		entry["processing_ms"] = processingMs.Int64
	}
	if errorMsg.Valid {
		entry["error_message"] = errorMsg.String
	}
	if completedAt.Valid {
		entry["completed_at"] = completedAt.Time.Format(time.RFC3339)
	}
	return entry
}
