package api

// Partner admin handlers — the operator UI talks to these endpoints to
// onboard new partners, mint API keys, hot-swap creatives, override ISP
// distribution, and trigger emergency stop. All routes are mounted inside
// the authenticated /api router so they inherit session / X-Admin-Key auth.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// PartnerAdminHandler holds the deps needed by the admin endpoints.
type PartnerAdminHandler struct {
	db *sql.DB
}

func NewPartnerAdminHandler(db *sql.DB) *PartnerAdminHandler {
	return &PartnerAdminHandler{db: db}
}

// ============ POST /api/mailing/data-partners ============

type createPartnerRequest struct {
	Name         string `json:"name"`
	Slug         string `json:"slug,omitempty"`
	ContactEmail string `json:"contact_email,omitempty"`
	Notes        string `json:"notes,omitempty"`
}

func (h *PartnerAdminHandler) HandleCreatePartner(w http.ResponseWriter, r *http.Request) {
	var req createPartnerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeJSONError(w, "name is required", http.StatusBadRequest)
		return
	}
	slug := strings.TrimSpace(req.Slug)
	if slug == "" {
		slug = slugifyForPartner(req.Name)
	} else {
		slug = sanitizeSlug(slug)
	}
	if slug == "" || slug == "unknown" {
		writeJSONError(w, "could not derive a valid slug from name", http.StatusBadRequest)
		return
	}

	id := uuid.New().String()
	_, err := h.db.ExecContext(r.Context(), `
		INSERT INTO data_partners (id, name, slug, contact_email, status, notes)
		VALUES ($1, $2, $3, NULLIF($4, ''), 'active', NULLIF($5, ''))
	`, id, req.Name, slug, req.ContactEmail, req.Notes)
	if err != nil {
		if isUniqueViolation(err) {
			writeJSONError(w, "partner with this name or slug already exists", http.StatusConflict)
			return
		}
		writeJSONError(w, "create_partner_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeAuditLog(r.Context(), h.db, actorFromRequest(r), "create_partner", "data_partner", id, nil, map[string]string{
		"name": req.Name, "slug": slug,
	})

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":            id,
		"name":          req.Name,
		"slug":          slug,
		"contact_email": req.ContactEmail,
		"status":        "active",
	})
}

// ============ GET /api/mailing/data-partners ============

func (h *PartnerAdminHandler) HandleListPartners(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.QueryContext(r.Context(), `
		SELECT p.id, p.name, p.slug, COALESCE(p.contact_email, ''), p.status,
		       COALESCE(p.notes, ''), p.created_at,
		       (SELECT COUNT(*) FROM partner_datasets d WHERE d.partner_id = p.id) AS dataset_count,
		       (SELECT COUNT(*) FROM partner_inbound_batches b WHERE b.partner_id = p.id) AS batch_count
		FROM data_partners p
		ORDER BY p.created_at DESC
	`)
	if err != nil {
		writeJSONError(w, "list_partners_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := make([]map[string]interface{}, 0)
	for rows.Next() {
		var (
			id, name, slug, contactEmail, status, notes string
			createdAt                                   time.Time
			datasetCount, batchCount                    int
		)
		if err := rows.Scan(&id, &name, &slug, &contactEmail, &status, &notes, &createdAt, &datasetCount, &batchCount); err != nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"id":            id,
			"name":          name,
			"slug":          slug,
			"contact_email": contactEmail,
			"status":        status,
			"notes":         notes,
			"created_at":    createdAt.Format(time.RFC3339),
			"dataset_count": datasetCount,
			"batch_count":   batchCount,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"partners": out})
}

// ============ POST /api/mailing/data-partners/{id}/datasets ============

type createDatasetRequest struct {
	Name             string `json:"name"`
	Slug             string `json:"slug,omitempty"`
	Vertical         string `json:"vertical"`
	FlushWindowHours int    `json:"flush_window_hours,omitempty"`
}

func (h *PartnerAdminHandler) HandleCreateDataset(w http.ResponseWriter, r *http.Request) {
	partnerID := chi.URLParam(r, "id")
	if !isValidUUID(partnerID) {
		writeJSONError(w, "invalid partner id", http.StatusBadRequest)
		return
	}
	var req createDatasetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Vertical = strings.TrimSpace(req.Vertical)
	if req.Name == "" {
		writeJSONError(w, "name is required", http.StatusBadRequest)
		return
	}
	if !isValidVertical(req.Vertical) {
		writeJSONError(w, "vertical must be one of: refi_heloc | personal_loans | tax_relief | remodel", http.StatusBadRequest)
		return
	}
	slug := strings.TrimSpace(req.Slug)
	if slug == "" {
		slug = slugifyForPartner(req.Name)
	} else {
		slug = sanitizeSlug(slug)
	}
	if slug == "" || slug == "unknown" {
		writeJSONError(w, "could not derive a valid slug from dataset name", http.StatusBadRequest)
		return
	}
	flushWindow := req.FlushWindowHours
	if flushWindow <= 0 {
		flushWindow = 24
	}
	if flushWindow > 168 {
		flushWindow = 168 // cap at 1 week
	}

	tx, err := h.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSONError(w, "tx begin failed", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	datasetID := uuid.New().String()
	_, err = tx.ExecContext(r.Context(), `
		INSERT INTO partner_datasets (id, partner_id, name, slug, vertical, flush_window_hours, status)
		VALUES ($1, $2, $3, $4, $5, $6, 'active')
	`, datasetID, partnerID, req.Name, slug, req.Vertical, flushWindow)
	if err != nil {
		if isUniqueViolation(err) {
			writeJSONError(w, "dataset slug already exists for this partner", http.StatusConflict)
			return
		}
		if isFKViolation(err) {
			writeJSONError(w, "partner does not exist", http.StatusNotFound)
			return
		}
		writeJSONError(w, "create_dataset_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	rawKey, prefix, hash, kerr := GeneratePartnerKey()
	if kerr != nil {
		writeJSONError(w, "key_generation_failed: "+kerr.Error(), http.StatusInternalServerError)
		return
	}
	keyID := uuid.New().String()
	_, err = tx.ExecContext(r.Context(), `
		INSERT INTO partner_api_keys (id, partner_id, dataset_id, key_hash, key_prefix, status)
		VALUES ($1, $2, $3, $4, $5, 'active')
	`, keyID, partnerID, datasetID, hash, prefix)
	if err != nil {
		writeJSONError(w, "insert_api_key_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		writeJSONError(w, "tx commit failed", http.StatusInternalServerError)
		return
	}

	writeAuditLog(r.Context(), h.db, actorFromRequest(r), "create_dataset", "partner_dataset", datasetID, nil, map[string]string{
		"partner_id": partnerID, "name": req.Name, "slug": slug, "vertical": req.Vertical, "key_prefix": prefix,
	})

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"dataset_id":          datasetID,
		"partner_id":          partnerID,
		"name":                req.Name,
		"slug":                slug,
		"vertical":            req.Vertical,
		"flush_window_hours":  flushWindow,
		"api_key":             rawKey,
		"api_key_prefix":      prefix,
		"api_key_warning":     "Show this key to the partner ONCE — it cannot be retrieved later. Only the prefix is stored.",
	})
}

// ============ GET /api/mailing/data-partners/datasets ============

func (h *PartnerAdminHandler) HandleListDatasets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	partnerFilter := strings.TrimSpace(q.Get("partner_id"))
	rows, err := h.db.QueryContext(r.Context(), `
		SELECT d.id, d.partner_id, d.name, d.slug, d.vertical, d.flush_window_hours,
		       d.paused_emergency, COALESCE(d.paused_reason, ''), d.status, d.created_at,
		       p.name AS partner_name, p.slug AS partner_slug,
		       (SELECT COUNT(*) FROM partner_inbound_batches b WHERE b.dataset_id = d.id) AS batch_count,
		       (SELECT COUNT(*) FROM partner_clean_queue q WHERE q.dataset_id = d.id AND q.status = 'ready') AS ready_count,
		       (SELECT COUNT(*) FROM partner_clean_queue q WHERE q.dataset_id = d.id AND q.status = 'mailed') AS mailed_count
		FROM partner_datasets d
		JOIN data_partners p ON p.id = d.partner_id
		WHERE ($1 = '' OR d.partner_id::text = $1)
		ORDER BY d.created_at DESC
	`, partnerFilter)
	if err != nil {
		writeJSONError(w, "list_datasets_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := make([]map[string]interface{}, 0)
	for rows.Next() {
		var (
			id, partnerID, name, slug, vertical, status   string
			pausedReason, partnerName, partnerSlug         string
			flushWindow                                    int
			pausedEmergency                                bool
			createdAt                                      time.Time
			batchCount, readyCount, mailedCount            int
		)
		if err := rows.Scan(&id, &partnerID, &name, &slug, &vertical, &flushWindow,
			&pausedEmergency, &pausedReason, &status, &createdAt,
			&partnerName, &partnerSlug, &batchCount, &readyCount, &mailedCount); err != nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"id":                  id,
			"partner_id":          partnerID,
			"partner_name":        partnerName,
			"partner_slug":        partnerSlug,
			"name":                name,
			"slug":                slug,
			"vertical":            vertical,
			"flush_window_hours":  flushWindow,
			"paused_emergency":    pausedEmergency,
			"paused_reason":       pausedReason,
			"status":              status,
			"created_at":          createdAt.Format(time.RFC3339),
			"batch_count":         batchCount,
			"ready_queue_count":   readyCount,
			"mailed_count":        mailedCount,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"datasets": out})
}

// ============ POST /api/mailing/data-partners/datasets/{id}/emergency-stop ============

type emergencyStopRequest struct {
	Reason string `json:"reason,omitempty"`
}

func (h *PartnerAdminHandler) HandleEmergencyStopDataset(w http.ResponseWriter, r *http.Request) {
	datasetID := chi.URLParam(r, "id")
	if !isValidUUID(datasetID) {
		writeJSONError(w, "invalid dataset id", http.StatusBadRequest)
		return
	}
	var req emergencyStopRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if strings.TrimSpace(req.Reason) == "" {
		req.Reason = "operator emergency stop"
	}
	res, err := h.db.ExecContext(r.Context(), `
		UPDATE partner_datasets
		SET paused_emergency = true,
		    paused_reason = $2,
		    paused_at = NOW(),
		    updated_at = NOW()
		WHERE id = $1
	`, datasetID, req.Reason)
	if err != nil {
		writeJSONError(w, "emergency_stop_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		writeJSONError(w, "dataset not found", http.StatusNotFound)
		return
	}
	writeAuditLog(r.Context(), h.db, actorFromRequest(r), "emergency_stop_dataset", "partner_dataset", datasetID, nil, map[string]string{
		"reason": req.Reason,
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"dataset_id": datasetID, "paused_emergency": true, "reason": req.Reason})
}

func (h *PartnerAdminHandler) HandleResumeDataset(w http.ResponseWriter, r *http.Request) {
	datasetID := chi.URLParam(r, "id")
	if !isValidUUID(datasetID) {
		writeJSONError(w, "invalid dataset id", http.StatusBadRequest)
		return
	}
	res, err := h.db.ExecContext(r.Context(), `
		UPDATE partner_datasets
		SET paused_emergency = false,
		    paused_reason = NULL,
		    paused_at = NULL,
		    updated_at = NOW()
		WHERE id = $1
	`, datasetID)
	if err != nil {
		writeJSONError(w, "resume_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		writeJSONError(w, "dataset not found", http.StatusNotFound)
		return
	}
	writeAuditLog(r.Context(), h.db, actorFromRequest(r), "resume_dataset", "partner_dataset", datasetID, nil, nil)
	writeJSON(w, http.StatusOK, map[string]interface{}{"dataset_id": datasetID, "paused_emergency": false})
}

// ============ GET /api/mailing/data-partners/datasets/{id}/throughput ============

// HandleGetDatasetThroughput returns the live ISP distribution of records in
// the ready queue plus a simple recommended-waves estimate based on the
// flush_window_hours configured for the dataset.
func (h *PartnerAdminHandler) HandleGetDatasetThroughput(w http.ResponseWriter, r *http.Request) {
	datasetID := chi.URLParam(r, "id")
	if !isValidUUID(datasetID) {
		writeJSONError(w, "invalid dataset id", http.StatusBadRequest)
		return
	}

	var (
		flushWindowHours int
		oldestIngest     sql.NullTime
		readyTotal       int
	)
	if err := h.db.QueryRowContext(r.Context(), `
		SELECT d.flush_window_hours,
		       (SELECT MIN(ingested_at) FROM partner_clean_queue q WHERE q.dataset_id = d.id AND q.status = 'ready'),
		       (SELECT COUNT(*) FROM partner_clean_queue q WHERE q.dataset_id = d.id AND q.status = 'ready')
		FROM partner_datasets d
		WHERE d.id = $1
	`, datasetID).Scan(&flushWindowHours, &oldestIngest, &readyTotal); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, "dataset not found", http.StatusNotFound)
			return
		}
		writeJSONError(w, "throughput_lookup_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	rows, err := h.db.QueryContext(r.Context(), `
		SELECT isp_family, COUNT(*) FROM partner_clean_queue
		WHERE dataset_id = $1 AND status = 'ready'
		GROUP BY isp_family
		ORDER BY 2 DESC
	`, datasetID)
	if err != nil {
		writeJSONError(w, "isp_breakdown_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	ispBreakdown := make(map[string]int)
	for rows.Next() {
		var isp string
		var n int
		if err := rows.Scan(&isp, &n); err == nil {
			ispBreakdown[isp] = n
		}
	}

	wavesRemaining := 0
	if oldestIngest.Valid {
		deadline := oldestIngest.Time.Add(time.Duration(flushWindowHours) * time.Hour)
		mins := int(time.Until(deadline).Minutes())
		if mins < 15 {
			mins = 15
		}
		wavesRemaining = mins / 15
	}
	recommendedWaveSize := 0
	if wavesRemaining > 0 {
		recommendedWaveSize = readyTotal / wavesRemaining
		if recommendedWaveSize < 25 && readyTotal > 0 {
			recommendedWaveSize = 25
		}
	}

	overrideRows, _ := h.db.QueryContext(r.Context(),
		`SELECT isp, pct_override, COALESCE(max_per_wave, 0) FROM partner_isp_distribution_overrides WHERE dataset_id = $1`, datasetID)
	overrides := make([]map[string]interface{}, 0)
	if overrideRows != nil {
		defer overrideRows.Close()
		for overrideRows.Next() {
			var isp string
			var pct float64
			var maxPerWave int
			if err := overrideRows.Scan(&isp, &pct, &maxPerWave); err == nil {
				overrides = append(overrides, map[string]interface{}{
					"isp":          isp,
					"pct_override": pct,
					"max_per_wave": maxPerWave,
				})
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"dataset_id":            datasetID,
		"flush_window_hours":    flushWindowHours,
		"ready_queue_total":     readyTotal,
		"isp_breakdown":         ispBreakdown,
		"oldest_ingest_at":      formatNullTime(oldestIngest),
		"waves_remaining":       wavesRemaining,
		"recommended_wave_size": recommendedWaveSize,
		"isp_overrides":         overrides,
	})
}

// ============ PUT /api/mailing/data-partners/datasets/{id}/isp-distribution ============

type ispDistributionRequest struct {
	Overrides []struct {
		ISP         string  `json:"isp"`
		PctOverride float64 `json:"pct_override,omitempty"`
		MaxPerWave  int     `json:"max_per_wave,omitempty"`
	} `json:"overrides"`
}

func (h *PartnerAdminHandler) HandleUpdateISPDistribution(w http.ResponseWriter, r *http.Request) {
	datasetID := chi.URLParam(r, "id")
	if !isValidUUID(datasetID) {
		writeJSONError(w, "invalid dataset id", http.StatusBadRequest)
		return
	}
	var req ispDistributionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	tx, err := h.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSONError(w, "tx begin failed", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(r.Context(),
		`DELETE FROM partner_isp_distribution_overrides WHERE dataset_id = $1`, datasetID); err != nil {
		writeJSONError(w, "delete_overrides_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for _, ov := range req.Overrides {
		isp := strings.ToLower(strings.TrimSpace(ov.ISP))
		if isp == "" {
			continue
		}
		if ov.PctOverride < 0 || ov.PctOverride > 1 {
			writeJSONError(w, "pct_override must be between 0 and 1", http.StatusBadRequest)
			return
		}
		var maxPtr interface{}
		if ov.MaxPerWave > 0 {
			maxPtr = ov.MaxPerWave
		}
		_, err := tx.ExecContext(r.Context(), `
			INSERT INTO partner_isp_distribution_overrides (dataset_id, isp, pct_override, max_per_wave, updated_by)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (dataset_id, isp) DO UPDATE
			SET pct_override = EXCLUDED.pct_override,
			    max_per_wave = EXCLUDED.max_per_wave,
			    updated_at = NOW(),
			    updated_by = EXCLUDED.updated_by
		`, datasetID, isp, ov.PctOverride, maxPtr, actorFromRequest(r))
		if err != nil {
			writeJSONError(w, "upsert_override_failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeJSONError(w, "tx commit failed", http.StatusInternalServerError)
		return
	}
	writeAuditLog(r.Context(), h.db, actorFromRequest(r), "update_isp_distribution", "partner_dataset", datasetID, nil, map[string]interface{}{
		"override_count": len(req.Overrides),
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"dataset_id": datasetID, "override_count": len(req.Overrides)})
}

// ============ PUT /api/mailing/data-partners/creatives/{vertical}/{brand} ============

type creativeUpdateRequest struct {
	CreativeFilename string `json:"creative_filename"`
	SubjectLine      string `json:"subject_line"`
	Preheader        string `json:"preheader"`
	FromName         string `json:"from_name"`
	Active           *bool  `json:"active,omitempty"`
}

func (h *PartnerAdminHandler) HandleUpdateCreative(w http.ResponseWriter, r *http.Request) {
	vertical := chi.URLParam(r, "vertical")
	brand := chi.URLParam(r, "brand")
	if !isValidVertical(vertical) {
		writeJSONError(w, "invalid vertical", http.StatusBadRequest)
		return
	}
	brand = strings.ToLower(strings.TrimSpace(brand))
	if !isValidBrand(brand) {
		writeJSONError(w, "brand must be one of: db | ht | mh | qf", http.StatusBadRequest)
		return
	}
	var req creativeUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.CreativeFilename = strings.TrimSpace(req.CreativeFilename)
	req.SubjectLine = strings.TrimSpace(req.SubjectLine)
	req.FromName = strings.TrimSpace(req.FromName)
	if req.CreativeFilename == "" || req.SubjectLine == "" || req.FromName == "" {
		writeJSONError(w, "creative_filename, subject_line, and from_name are required", http.StatusBadRequest)
		return
	}
	if strings.Contains(req.CreativeFilename, "/") || strings.Contains(req.CreativeFilename, "..") {
		writeJSONError(w, "creative_filename must be a bare basename inside docs/emails/", http.StatusBadRequest)
		return
	}

	// Capture before-state for audit log.
	var beforeFilename, beforeSubject, beforePreheader, beforeFrom string
	var beforeActive bool
	_ = h.db.QueryRowContext(r.Context(), `
		SELECT creative_filename, subject_line, preheader, from_name, active
		FROM partner_drip_creatives WHERE vertical = $1 AND brand = $2
	`, vertical, brand).Scan(&beforeFilename, &beforeSubject, &beforePreheader, &beforeFrom, &beforeActive)

	active := true
	if req.Active != nil {
		active = *req.Active
	}

	_, err := h.db.ExecContext(r.Context(), `
		INSERT INTO partner_drip_creatives (vertical, brand, creative_filename, subject_line, preheader, from_name, active, effective_from, updated_by)
		VALUES ($1, $2, $3, $4, COALESCE($5, ''), $6, $7, NOW(), $8)
		ON CONFLICT (vertical, brand) DO UPDATE SET
		    creative_filename = EXCLUDED.creative_filename,
		    subject_line = EXCLUDED.subject_line,
		    preheader = EXCLUDED.preheader,
		    from_name = EXCLUDED.from_name,
		    active = EXCLUDED.active,
		    effective_from = NOW(),
		    updated_at = NOW(),
		    updated_by = EXCLUDED.updated_by
	`, vertical, brand, req.CreativeFilename, req.SubjectLine, req.Preheader, req.FromName, active, actorFromRequest(r))
	if err != nil {
		writeJSONError(w, "creative_update_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeAuditLog(r.Context(), h.db, actorFromRequest(r), "update_creative", "partner_drip_creative",
		fmt.Sprintf("%s/%s", vertical, brand),
		map[string]interface{}{
			"creative_filename": beforeFilename,
			"subject_line":      beforeSubject,
			"preheader":         beforePreheader,
			"from_name":         beforeFrom,
			"active":            beforeActive,
		},
		map[string]interface{}{
			"creative_filename": req.CreativeFilename,
			"subject_line":      req.SubjectLine,
			"preheader":         req.Preheader,
			"from_name":         req.FromName,
			"active":            active,
		})
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"vertical": vertical, "brand": brand,
		"creative_filename": req.CreativeFilename,
		"effective":         "next_wave",
	})
}

// ============ GET /api/mailing/data-partners/creatives ============

func (h *PartnerAdminHandler) HandleListCreatives(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.QueryContext(r.Context(), `
		SELECT vertical, brand, creative_filename, subject_line, COALESCE(preheader, ''),
		       from_name, active, effective_from, COALESCE(updated_by, '')
		FROM partner_drip_creatives
		ORDER BY vertical, brand
	`)
	if err != nil {
		writeJSONError(w, "list_creatives_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := make([]map[string]interface{}, 0)
	for rows.Next() {
		var vertical, brand, filename, subject, preheader, fromName, updatedBy string
		var active bool
		var effectiveFrom time.Time
		if err := rows.Scan(&vertical, &brand, &filename, &subject, &preheader, &fromName, &active, &effectiveFrom, &updatedBy); err != nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"vertical":          vertical,
			"brand":             brand,
			"creative_filename": filename,
			"subject_line":      subject,
			"preheader":         preheader,
			"from_name":         fromName,
			"active":            active,
			"effective_from":    effectiveFrom.Format(time.RFC3339),
			"updated_by":        updatedBy,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"creatives": out})
}

// ============ GET /api/mailing/data-partners/dashboard ============

// HandleGetDashboard returns a single roll-up suitable for the operator
// landing page: per-vertical queue depth, last wave, drip-state, and the
// recent batch list.
func (h *PartnerAdminHandler) HandleGetDashboard(w http.ResponseWriter, r *http.Request) {
	verticalRows, err := h.db.QueryContext(r.Context(), `
		SELECT s.vertical, s.next_brand_index, s.last_wave_at, s.last_wave_brand, s.last_wave_size,
		       (SELECT COUNT(*) FROM partner_clean_queue q WHERE q.vertical = s.vertical AND q.status = 'ready') AS ready_q,
		       (SELECT COUNT(*) FROM partner_clean_queue q WHERE q.vertical = s.vertical AND q.status = 'pending_eo') AS pending_eo,
		       (SELECT COUNT(*) FROM partner_clean_queue q WHERE q.vertical = s.vertical AND q.status = 'mailed') AS mailed
		FROM partner_drip_state s
		ORDER BY s.vertical
	`)
	if err != nil {
		writeJSONError(w, "dashboard_query_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer verticalRows.Close()

	verticals := make([]map[string]interface{}, 0, 4)
	for verticalRows.Next() {
		var vertical string
		var brandIdx int
		var lastWave sql.NullTime
		var lastBrand sql.NullString
		var lastSize sql.NullInt64
		var readyQ, pendingEO, mailed int
		if err := verticalRows.Scan(&vertical, &brandIdx, &lastWave, &lastBrand, &lastSize, &readyQ, &pendingEO, &mailed); err != nil {
			continue
		}
		verticals = append(verticals, map[string]interface{}{
			"vertical":          vertical,
			"next_brand_index":  brandIdx,
			"last_wave_at":      formatNullTime(lastWave),
			"last_wave_brand":   nullStringValue(lastBrand),
			"last_wave_size":    nullIntValue(lastSize),
			"ready_queue":       readyQ,
			"pending_eo":        pendingEO,
			"mailed_total":      mailed,
		})
	}

	batchRows, _ := h.db.QueryContext(r.Context(), `
		SELECT b.id, b.dataset_id, b.partner_id, b.status, b.record_count,
		       b.received_at, b.completed_at, b.emergency_stopped,
		       d.name, d.vertical, p.name
		FROM partner_inbound_batches b
		JOIN partner_datasets d ON d.id = b.dataset_id
		JOIN data_partners p ON p.id = b.partner_id
		ORDER BY b.received_at DESC
		LIMIT 25
	`)
	batches := make([]map[string]interface{}, 0)
	if batchRows != nil {
		defer batchRows.Close()
		for batchRows.Next() {
			var id, datasetID, partnerID, status, datasetName, vertical, partnerName string
			var recordCount int
			var receivedAt time.Time
			var completedAt sql.NullTime
			var emergency bool
			if err := batchRows.Scan(&id, &datasetID, &partnerID, &status, &recordCount,
				&receivedAt, &completedAt, &emergency, &datasetName, &vertical, &partnerName); err != nil {
				continue
			}
			batches = append(batches, map[string]interface{}{
				"id":                id,
				"dataset_id":        datasetID,
				"dataset_name":      datasetName,
				"partner_id":        partnerID,
				"partner_name":      partnerName,
				"vertical":          vertical,
				"status":            status,
				"record_count":      recordCount,
				"received_at":       receivedAt.Format(time.RFC3339),
				"completed_at":      formatNullTime(completedAt),
				"emergency_stopped": emergency,
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"verticals":      verticals,
		"recent_batches": batches,
	})
}

// ============ GET /api/mailing/data-partners/audit-log ============

// HandleListAuditLog returns the most recent admin audit events. Optionally
// filtered by target_type / target_id / actor / action via query params.
func (h *PartnerAdminHandler) HandleListAuditLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	targetType := strings.TrimSpace(q.Get("target_type"))
	targetID := strings.TrimSpace(q.Get("target_id"))
	actor := strings.TrimSpace(q.Get("actor"))
	action := strings.TrimSpace(q.Get("action"))
	limit := 100
	if s := strings.TrimSpace(q.Get("limit")); s != "" {
		fmt.Sscanf(s, "%d", &limit)
		if limit <= 0 || limit > 1000 {
			limit = 100
		}
	}

	rows, err := h.db.QueryContext(r.Context(), `
		SELECT id, actor, action, target_type, COALESCE(target_id,''),
		       COALESCE(before_state::text, ''), COALESCE(after_state::text, ''),
		       created_at
		FROM partner_admin_audit_log
		WHERE ($1 = '' OR target_type = $1)
		  AND ($2 = '' OR target_id   = $2)
		  AND ($3 = '' OR actor       = $3)
		  AND ($4 = '' OR action      = $4)
		ORDER BY created_at DESC
		LIMIT $5
	`, targetType, targetID, actor, action, limit)
	if err != nil {
		writeJSONError(w, "audit_log_query_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := make([]map[string]interface{}, 0)
	for rows.Next() {
		var (
			id, evActor, evAction, evType, evTargetID, beforeStr, afterStr string
			createdAt                                                      time.Time
		)
		if err := rows.Scan(&id, &evActor, &evAction, &evType, &evTargetID, &beforeStr, &afterStr, &createdAt); err != nil {
			continue
		}
		var before, after interface{}
		if beforeStr != "" {
			_ = json.Unmarshal([]byte(beforeStr), &before)
		}
		if afterStr != "" {
			_ = json.Unmarshal([]byte(afterStr), &after)
		}
		out = append(out, map[string]interface{}{
			"id":           id,
			"actor":        evActor,
			"action":       evAction,
			"target_type":  evType,
			"target_id":    evTargetID,
			"before_state": before,
			"after_state":  after,
			"created_at":   createdAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"events": out})
}

// ============ helpers ============

func slugifyForPartner(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	out := make([]byte, 0, len(s))
	lastDash := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z',
			ch >= '0' && ch <= '9':
			out = append(out, ch)
			lastDash = false
		default:
			if !lastDash {
				out = append(out, '-')
				lastDash = true
			}
		}
	}
	return strings.Trim(string(out), "-")
}

func isValidVertical(v string) bool {
	switch v {
	case "refi_heloc", "personal_loans", "tax_relief", "remodel":
		return true
	}
	return false
}

func isValidBrand(b string) bool {
	switch b {
	case "db", "ht", "mh", "qf":
		return true
	}
	return false
}

func isValidUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

func nullIntValue(ni sql.NullInt64) interface{} {
	if !ni.Valid {
		return nil
	}
	return ni.Int64
}

func actorFromRequest(r *http.Request) string {
	// AuthManager attaches user info; if unavailable, fall back to admin-key
	// or remote-ip so audit log is never empty.
	if user := r.Header.Get("X-User-Email"); user != "" {
		return user
	}
	if r.Header.Get("X-Admin-Key") != "" {
		return "admin-key"
	}
	if ip := readClientIP(r); ip != "" {
		return "ip:" + ip
	}
	return "unknown"
}

func writeAuditLog(ctx context.Context, db *sql.DB, actor, action, targetType, targetID string, before, after interface{}) {
	if db == nil {
		return
	}
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	_, _ = db.ExecContext(ctx, `
		INSERT INTO partner_admin_audit_log (actor, action, target_type, target_id, before_state, after_state)
		VALUES ($1, $2, $3, $4, NULLIF($5, 'null')::jsonb, NULLIF($6, 'null')::jsonb)
	`, actor, action, targetType, targetID, string(beforeJSON), string(afterJSON))
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate key") || strings.Contains(msg, "unique constraint")
}

func isFKViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "foreign key")
}

// pathBaseForCreative is only used in tests / future creative validation.
func pathBaseForCreative(p string) string {
	if p == "" {
		return ""
	}
	if u, err := url.Parse(p); err == nil && u.Path != "" {
		return path.Base(u.Path)
	}
	return path.Base(p)
}
