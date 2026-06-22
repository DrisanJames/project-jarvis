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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/lib/pq"
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

// ============ GET /api/mailing/data-partners/drip-performance ============

// HandleGetDripPerformance backs the Data Partners "drip performance" panel:
// a live view of how the partner drip is actually performing, not just queue
// depth. Two sections, individually selectable via ?include= so the UI can
// poll them at different cadences:
//
//   - funnel (?include=funnel): one grouped scan over partner_clean_queue per
//     (vertical, isp_family) — lifecycle counts (pending_eo → ready → mailed),
//     the multi-touch state machine (T1..T4 distribution, engaged, completed,
//     follow-ups due now), and sent-in-24h. Same cost class as the digest
//     worker's queries; intended for ~30s polling.
//
//   - waves (?include=waves): the most recent wave campaigns (stamped with
//     partner_drip_tag by the orchestrator, served by the partial index
//     idx_mc_partner_drip_tag) with per-wave delivery stats aggregated LIVE
//     from mailing_tracking_events. Campaign counter columns are NOT used —
//     they are known-stale (dead since 2026-05-29); tracking events arrive
//     continuously from the PMTA ingestor, so re-aggregating on each poll is
//     what makes the panel "stats as they are received". Bounces are always
//     split hard vs soft per bounce-metrics doctrine — never combined.
//     Intended for ~10s polling (bounded: <=200 small campaigns, indexed).
//
// Default (no ?include=) returns both.
func (h *PartnerAdminHandler) HandleGetDripPerformance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	hours := 48
	if v, err := strconv.Atoi(r.URL.Query().Get("hours")); err == nil && v > 0 && v <= 168 {
		hours = v
	}
	limit := 40
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 200 {
		limit = v
	}
	wantFunnel, wantWaves, wantRollup, wantTotals := true, true, true, true
	if inc := strings.TrimSpace(r.URL.Query().Get("include")); inc != "" {
		wantFunnel, wantWaves, wantRollup, wantTotals = false, false, false, false
		for _, tok := range strings.Split(inc, ",") {
			switch strings.TrimSpace(strings.ToLower(tok)) {
			case "funnel":
				wantFunnel = true
			case "waves":
				wantWaves = true
			case "rollup":
				wantRollup = true
			case "totals":
				wantTotals = true
			}
		}
	}
	filterVertical := strings.TrimSpace(r.URL.Query().Get("vertical"))
	filterBrand := strings.TrimSpace(r.URL.Query().Get("brand"))

	resp := map[string]interface{}{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	}

	if wantFunnel {
		verticals, isps, err := h.dripFunnel(ctx)
		if err != nil {
			// Serve the waves section even if the funnel scan fails (e.g.
			// statement timeout under IO pressure) — partial data beats a 500
			// on an operator dashboard.
			resp["funnel_error"] = err.Error()
		} else {
			resp["funnel"] = verticals
			resp["funnel_isp"] = isps
		}
		// Inbound-flow roll-up: partners post leads in real time (often one
		// record per API call), so raw batch rows read as "you mailed 1
		// contact". This per-dataset 24h aggregate is what the Overview
		// renders instead; the Batches tab keeps per-post detail.
		if ingest, err := h.dripIngest24h(ctx); err == nil {
			resp["ingest_24h"] = ingest
		}
	}

	if wantWaves {
		waves, err := h.dripWaves(ctx, hours, limit, filterVertical, filterBrand)
		if err != nil {
			resp["waves_error"] = err.Error()
		} else {
			resp["waves"] = waves
		}
	}

	if wantRollup {
		rollup, err := h.dripRollup(ctx, hours)
		if err != nil {
			resp["rollup_error"] = err.Error()
		} else {
			resp["rollup"] = rollup
		}
		if series, err := h.dripSeries(ctx, hours); err == nil {
			resp["series"] = series
		}
	}

	// Overall 24h send performance across ALL partner-drip waves — one
	// indexed aggregate, cheap enough to ride along with the fast poll so
	// the headline numbers move as accounting events are ingested.
	if wantTotals || wantWaves {
		if totals, err := h.dripTotals24h(ctx); err == nil {
			resp["totals_24h"] = totals
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// dripRollup aggregates wave campaigns per (vertical, brand) over the window:
// wave counts + recipients from the campaign side, delivery stats from the
// events side (two queries merged in Go — a single LEFT JOIN would multiply
// campaign rows per event and corrupt COUNT/SUM).
func (h *PartnerAdminHandler) dripRollup(ctx context.Context, hours int) ([]map[string]interface{}, error) {
	type group struct {
		vertical, brand, partnerSlug                          string
		waves, recipients, active                             int
		lastWaveAt                                            sql.NullTime
		sent, delivered, opens, clicks, hard, soft, deferred  int
	}
	groups := map[string]*group{}
	var order []string

	campRows, err := h.db.QueryContext(ctx, `
		SELECT COALESCE(partner_drip_tag, ''), split_part(name, ' ', 3),
		       COUNT(*), COALESCE(SUM(COALESCE(total_recipients, 0)), 0),
		       COUNT(*) FILTER (WHERE status IN ('scheduled','preparing','finalizing_audience','sending')),
		       MAX(scheduled_at)
		FROM mailing_campaigns
		WHERE partner_drip_tag IS NOT NULL
		  AND scheduled_at > NOW() - ($1 * INTERVAL '1 hour')
		GROUP BY 1, 2
	`, hours)
	if err != nil {
		return nil, err
	}
	defer campRows.Close()
	for campRows.Next() {
		var tag, brand string
		var g group
		if err := campRows.Scan(&tag, &brand, &g.waves, &g.recipients, &g.active, &g.lastWaveAt); err != nil {
			continue
		}
		g.vertical, g.partnerSlug = parsePartnerDripTag(tag)
		g.brand = brand
		key := g.vertical + "|" + brand
		groups[key] = &g
		order = append(order, key)
	}
	if err := campRows.Err(); err != nil {
		return nil, err
	}

	hb := HardBounceSQL("t")
	evRows, err := h.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(c.partner_drip_tag, ''), split_part(c.name, ' ', 3),
		       COALESCE(SUM(CASE WHEN t.event_type = 'sent' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.event_type = 'delivered' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.event_type = 'opened' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.event_type = 'clicked' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.event_type = 'bounced' AND %s THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.event_type = 'bounced' AND NOT (%s) THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.event_type IN ('deferred','deferral') THEN 1 ELSE 0 END), 0)
		FROM mailing_tracking_events t
		JOIN mailing_campaigns c ON c.id = t.campaign_id
		WHERE c.partner_drip_tag IS NOT NULL
		  AND c.scheduled_at > NOW() - ($1 * INTERVAL '1 hour')
		GROUP BY 1, 2
	`, hb, hb), hours)
	if err != nil {
		return nil, err
	}
	defer evRows.Close()
	for evRows.Next() {
		var tag, brand string
		var sent, delivered, opens, clicks, hard, soft, deferred int
		if err := evRows.Scan(&tag, &brand, &sent, &delivered, &opens, &clicks, &hard, &soft, &deferred); err != nil {
			continue
		}
		vertical, _ := parsePartnerDripTag(tag)
		if g := groups[vertical+"|"+brand]; g != nil {
			g.sent += sent
			g.delivered += delivered
			g.opens += opens
			g.clicks += clicks
			g.hard += hard
			g.soft += soft
			g.deferred += deferred
		}
	}

	out := make([]map[string]interface{}, 0, len(order))
	for _, key := range order {
		g := groups[key]
		lastWave := ""
		if g.lastWaveAt.Valid {
			lastWave = g.lastWaveAt.Time.Format(time.RFC3339)
		}
		out = append(out, map[string]interface{}{
			"vertical":     g.vertical,
			"brand":        g.brand,
			"partner_slug": g.partnerSlug,
			"waves":        g.waves,
			"recipients":   g.recipients,
			"active":       g.active,
			"last_wave_at": lastWave,
			"sent":         g.sent,
			"delivered":    g.delivered,
			"opens":        g.opens,
			"clicks":       g.clicks,
			"hard_bounces": g.hard,
			"soft_bounces": g.soft,
			"deferred":     g.deferred,
		})
	}
	return out, nil
}

// dripSeries returns hourly delivery/performance buckets per (vertical, brand)
// over the window — the data behind the expandable groups' charts.
func (h *PartnerAdminHandler) dripSeries(ctx context.Context, hours int) ([]map[string]interface{}, error) {
	hb := HardBounceSQL("t")
	rows, err := h.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT date_trunc('hour', t.event_at),
		       COALESCE(c.partner_drip_tag, ''), split_part(c.name, ' ', 3),
		       COALESCE(SUM(CASE WHEN t.event_type = 'sent' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.event_type = 'delivered' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.event_type = 'opened' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.event_type = 'clicked' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.event_type = 'bounced' AND %s THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.event_type = 'bounced' AND NOT (%s) THEN 1 ELSE 0 END), 0)
		FROM mailing_tracking_events t
		JOIN mailing_campaigns c ON c.id = t.campaign_id
		WHERE c.partner_drip_tag IS NOT NULL
		  AND c.scheduled_at > NOW() - ($1 * INTERVAL '1 hour')
		  AND t.event_at > NOW() - ($1 * INTERVAL '1 hour')
		GROUP BY 1, 2, 3
		ORDER BY 1
	`, hb, hb), hours)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]map[string]interface{}, 0, 256)
	for rows.Next() {
		var hr time.Time
		var tag, brand string
		var sent, delivered, opens, clicks, hard, soft int
		if err := rows.Scan(&hr, &tag, &brand, &sent, &delivered, &opens, &clicks, &hard, &soft); err != nil {
			continue
		}
		vertical, _ := parsePartnerDripTag(tag)
		out = append(out, map[string]interface{}{
			"hour":         hr.Format(time.RFC3339),
			"vertical":     vertical,
			"brand":        brand,
			"sent":         sent,
			"delivered":    delivered,
			"opens":        opens,
			"clicks":       clicks,
			"hard_bounces": hard,
			"soft_bounces": soft,
		})
	}
	return out, rows.Err()
}

// dripTotals24h aggregates delivery performance across every partner-drip
// wave scheduled in the last 24h, straight from tracking events.
func (h *PartnerAdminHandler) dripTotals24h(ctx context.Context) (map[string]interface{}, error) {
	var waveCount, recipients int
	if err := h.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(COALESCE(total_recipients, 0)), 0)
		FROM mailing_campaigns
		WHERE partner_drip_tag IS NOT NULL
		  AND scheduled_at > NOW() - INTERVAL '24 hours'
	`).Scan(&waveCount, &recipients); err != nil {
		return nil, err
	}

	hb := HardBounceSQL("mailing_tracking_events")
	var sent, delivered, opens, clicks, hard, soft, deferred int
	if err := h.db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(SUM(CASE WHEN event_type = 'sent' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN event_type = 'delivered' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN event_type = 'opened' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN event_type = 'clicked' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN event_type = 'bounced' AND %s THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN event_type = 'bounced' AND NOT (%s) THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN event_type IN ('deferred','deferral') THEN 1 ELSE 0 END), 0)
		FROM mailing_tracking_events
		WHERE campaign_id IN (
		    SELECT id FROM mailing_campaigns
		    WHERE partner_drip_tag IS NOT NULL
		      AND scheduled_at > NOW() - INTERVAL '24 hours'
		)
	`, hb, hb)).Scan(&sent, &delivered, &opens, &clicks, &hard, &soft, &deferred); err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"waves":        waveCount,
		"recipients":   recipients,
		"sent":         sent,
		"delivered":    delivered,
		"opens":        opens,
		"clicks":       clicks,
		"hard_bounces": hard,
		"soft_bounces": soft,
		"deferred":     deferred,
	}, nil
}

// dripIngest24h rolls inbound batches up per dataset for the last 24h.
func (h *PartnerAdminHandler) dripIngest24h(ctx context.Context) ([]map[string]interface{}, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT d.id::text, d.name, p.name, d.vertical,
		       COUNT(*)                                   AS posts,
		       COALESCE(SUM(b.record_count), 0)           AS records,
		       MAX(b.received_at)                         AS last_received,
		       COUNT(*) FILTER (WHERE b.status NOT IN ('slicing_complete', 'completed')) AS in_flight,
		       COUNT(*) FILTER (WHERE b.emergency_stopped) AS stopped
		FROM partner_inbound_batches b
		JOIN partner_datasets d ON d.id = b.dataset_id
		JOIN data_partners p ON p.id = b.partner_id
		WHERE b.received_at > NOW() - INTERVAL '24 hours'
		GROUP BY 1, 2, 3, 4
		ORDER BY records DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]map[string]interface{}, 0, 8)
	for rows.Next() {
		var datasetID, datasetName, partnerName, vertical string
		var posts, records, inFlight, stopped int
		var lastReceived sql.NullTime
		if err := rows.Scan(&datasetID, &datasetName, &partnerName, &vertical,
			&posts, &records, &lastReceived, &inFlight, &stopped); err != nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"dataset_id":    datasetID,
			"dataset_name":  datasetName,
			"partner_name":  partnerName,
			"vertical":      vertical,
			"posts":         posts,
			"records":       records,
			"last_received": formatNullTime(lastReceived),
			"in_flight":     inFlight,
			"stopped":       stopped,
		})
	}
	return out, rows.Err()
}

// dripFunnel runs the single grouped queue scan and returns the per-vertical
// roll-up plus the per-(vertical, isp) slices.
func (h *PartnerAdminHandler) dripFunnel(ctx context.Context) ([]map[string]interface{}, []map[string]interface{}, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT vertical,
		       COALESCE(NULLIF(isp_family, ''), 'other')                         AS isp,
		       COUNT(*) FILTER (WHERE status = 'pending_eo')                     AS pending_eo,
		       COUNT(*) FILTER (WHERE status = 'hold')                           AS hold,
		       COUNT(*) FILTER (WHERE status = 'ready')                          AS ready,
		       COUNT(*) FILTER (WHERE status = 'claimed')                        AS claimed,
		       COUNT(*) FILTER (WHERE status = 'mailed')                         AS mailed,
		       COUNT(*) FILTER (WHERE status = 'mailed'
		                          AND mailed_at > NOW() - INTERVAL '24 hours')   AS sent_24h,
		       COUNT(*) FILTER (WHERE COALESCE(touch_count, 0) = 1)              AS touch_1,
		       COUNT(*) FILTER (WHERE COALESCE(touch_count, 0) = 2)              AS touch_2,
		       COUNT(*) FILTER (WHERE COALESCE(touch_count, 0) = 3)              AS touch_3,
		       COUNT(*) FILTER (WHERE COALESCE(touch_count, 0) >= 4)             AS touch_4,
		       COUNT(*) FILTER (WHERE engaged_at IS NOT NULL)                    AS engaged,
		       COUNT(*) FILTER (WHERE terminal_reason = 'completed')             AS completed,
		       COUNT(*) FILTER (WHERE status = 'mailed'
		                          AND engaged_at IS NULL
		                          AND terminal_reason IS NULL
		                          AND next_touch_at IS NOT NULL
		                          AND next_touch_at <= NOW())                    AS followups_due,
		       COALESCE(SUM(COALESCE(eo_attempts, 0)), 0)                       AS eo_credits_total,
		       COUNT(*) FILTER (WHERE validated_at > NOW() - INTERVAL '24 hours') AS eo_validated_24h,
		       -- Three mutually-exclusive outcome buckets over the mailed-into-drip
		       -- population (engaged / in_progress / churned). They partition it and
		       -- sum to it, so the UI rates total 100%. Denominator is the bucket
		       -- SUM, NOT touch_count (touch_count can be 0 on mailed rows — e.g.
		       -- personal_loans — and would break the rates).
		       COUNT(*) FILTER (WHERE engaged_at IS NULL AND terminal_reason IS NULL
		                          AND mailed_campaign_id IS NOT NULL)            AS in_progress,
		       COUNT(*) FILTER (WHERE engaged_at IS NULL
		                          AND terminal_reason = 'completed')            AS churned
		FROM partner_clean_queue
		GROUP BY 1, 2
		ORDER BY 1, 7 DESC
	`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	type vAgg struct {
		pendingEO, hold, ready, claimed, mailed, sent24h int
		t1, t2, t3, t4, engaged, completed, followupsDue int
		inProgress, churned                              int
		// eoCredits = SUM(eo_attempts): EmailOversight bills per validation
		// call, and partner_validator.go increments eo_attempts exactly once
		// per call — so this is consumed EO credits. eoValidated24h counts
		// rows whose last validation landed in the last 24h.
		eoCredits      int64
		eoValidated24h int
	}
	vTotals := map[string]*vAgg{}
	var vOrder []string
	isps := make([]map[string]interface{}, 0, 32)

	for rows.Next() {
		var vertical, isp string
		var pendingEO, hold, ready, claimed, mailed, sent24h int
		var t1, t2, t3, t4, engaged, completed, followupsDue int
		var eoCredits int64
		var eoValidated24h, inProgress, churned int
		if err := rows.Scan(&vertical, &isp, &pendingEO, &hold, &ready, &claimed, &mailed, &sent24h,
			&t1, &t2, &t3, &t4, &engaged, &completed, &followupsDue, &eoCredits, &eoValidated24h, &inProgress, &churned); err != nil {
			continue
		}
		agg, ok := vTotals[vertical]
		if !ok {
			agg = &vAgg{}
			vTotals[vertical] = agg
			vOrder = append(vOrder, vertical)
		}
		agg.pendingEO += pendingEO
		agg.hold += hold
		agg.ready += ready
		agg.claimed += claimed
		agg.mailed += mailed
		agg.sent24h += sent24h
		agg.t1 += t1
		agg.t2 += t2
		agg.t3 += t3
		agg.t4 += t4
		agg.engaged += engaged
		agg.completed += completed
		agg.followupsDue += followupsDue
		agg.eoCredits += eoCredits
		agg.eoValidated24h += eoValidated24h
		agg.inProgress += inProgress
		agg.churned += churned

		isps = append(isps, map[string]interface{}{
			"vertical": vertical,
			"isp":      isp,
			"ready":    ready,
			"mailed":   mailed,
			"sent_24h": sent24h,
		})
	}

	// Conversions per vertical (distinct conversion tied to the vertical's
	// subscribers; sub1 = subscriber_id). Separate cheap query merged in — same
	// semantics as Previous Activations so the two screens agree.
	convByVertical := map[string]int{}
	if crows, cerr := h.db.QueryContext(ctx, `
		SELECT vertical, COUNT(*) FROM (
			SELECT DISTINCT q.vertical, mc.conversion_id
			FROM mailing_cpm_manual_conversions mc
			JOIN (SELECT DISTINCT vertical, subscriber_id FROM partner_clean_queue WHERE subscriber_id IS NOT NULL) q
			  ON q.subscriber_id::text = mc.sub1
		) z GROUP BY vertical
	`); cerr == nil {
		for crows.Next() {
			var v string
			var n int
			if crows.Scan(&v, &n) == nil {
				convByVertical[v] = n
			}
		}
		crows.Close()
	}

	verticals := make([]map[string]interface{}, 0, len(vOrder))
	for _, v := range vOrder {
		a := vTotals[v]
		// drip_total = the mailed-into-drip population = the three buckets summed
		// (NOT touch_count, which can be 0 on mailed rows). The UI divides the
		// three rates by this so they total 100%.
		dripTotal := a.engaged + a.inProgress + a.churned
		verticals = append(verticals, map[string]interface{}{
			"vertical":      v,
			"pending_eo":    a.pendingEO,
			"hold":          a.hold,
			"ready":         a.ready,
			"claimed":       a.claimed,
			"mailed":        a.mailed,
			"sent_24h":      a.sent24h,
			"touch_1":       a.t1,
			"touch_2":       a.t2,
			"touch_3":       a.t3,
			"touch_4":       a.t4,
			"engaged":          a.engaged,
			"completed":        a.completed,
			"in_progress":      a.inProgress,
			"churned":          a.churned,
			"drip_total":       dripTotal,
			"conversions":      convByVertical[v],
			"followups_due":    a.followupsDue,
			"eo_credits_total": a.eoCredits,
			"eo_validated_24h": a.eoValidated24h,
		})
	}
	return verticals, isps, rows.Err()
}

// dripWaves returns the most recent partner-drip wave campaigns with live
// event-derived stats. vertical/brand (both optional) narrow to one rollup
// group — the expandable-group detail view.
func (h *PartnerAdminHandler) dripWaves(ctx context.Context, hours, limit int, vertical, brand string) ([]map[string]interface{}, error) {
	campRows, err := h.db.QueryContext(ctx, `
		SELECT c.id::text, c.name, COALESCE(c.partner_drip_tag, ''),
		       COALESCE(c.partner_dataset_id::text, ''), c.status,
		       c.scheduled_at, COALESCE(c.total_recipients, 0),
		       COALESCE(d.name, ''), COALESCE(p.name, '')
		FROM mailing_campaigns c
		LEFT JOIN partner_datasets d ON d.id = c.partner_dataset_id
		LEFT JOIN data_partners p ON p.id = d.partner_id
		WHERE c.partner_drip_tag IS NOT NULL
		  AND c.scheduled_at > NOW() - ($1 * INTERVAL '1 hour')
		  AND ($3 = '' OR c.partner_drip_tag LIKE '%/' || $3)
		  AND ($4 = '' OR split_part(c.name, ' ', 3) = $4)
		ORDER BY c.scheduled_at DESC
		LIMIT $2
	`, hours, limit, vertical, brand)
	if err != nil {
		return nil, err
	}
	defer campRows.Close()

	type waveRow struct {
		id, name, tag, datasetID, status, datasetName, partnerName string
		scheduledAt                                                sql.NullTime
		totalRecipients                                            int
	}
	var ordered []waveRow
	var ids []string
	for campRows.Next() {
		var wr waveRow
		if err := campRows.Scan(&wr.id, &wr.name, &wr.tag, &wr.datasetID, &wr.status,
			&wr.scheduledAt, &wr.totalRecipients, &wr.datasetName, &wr.partnerName); err != nil {
			continue
		}
		ordered = append(ordered, wr)
		ids = append(ids, wr.id)
	}
	if err := campRows.Err(); err != nil {
		return nil, err
	}

	type evAgg struct {
		sent, delivered, opens, clicks, hard, soft, deferred int
	}
	events := map[string]*evAgg{}
	if len(ids) > 0 {
		hb := HardBounceSQL("mailing_tracking_events")
		evRows, err := h.db.QueryContext(ctx, fmt.Sprintf(`
			SELECT campaign_id::text,
			       COALESCE(SUM(CASE WHEN event_type = 'sent' THEN 1 ELSE 0 END), 0),
			       COALESCE(SUM(CASE WHEN event_type = 'delivered' THEN 1 ELSE 0 END), 0),
			       COALESCE(SUM(CASE WHEN event_type = 'opened' THEN 1 ELSE 0 END), 0),
			       COALESCE(SUM(CASE WHEN event_type = 'clicked' THEN 1 ELSE 0 END), 0),
			       COALESCE(SUM(CASE WHEN event_type = 'bounced' AND %s THEN 1 ELSE 0 END), 0),
			       COALESCE(SUM(CASE WHEN event_type = 'bounced' AND NOT (%s) THEN 1 ELSE 0 END), 0),
			       COALESCE(SUM(CASE WHEN event_type IN ('deferred','deferral') THEN 1 ELSE 0 END), 0)
			FROM mailing_tracking_events
			WHERE campaign_id = ANY($1::uuid[])
			GROUP BY campaign_id
		`, hb, hb), pq.Array(ids))
		if err != nil {
			return nil, err
		}
		defer evRows.Close()
		for evRows.Next() {
			var cid string
			var a evAgg
			if err := evRows.Scan(&cid, &a.sent, &a.delivered, &a.opens, &a.clicks, &a.hard, &a.soft, &a.deferred); err != nil {
				continue
			}
			events[cid] = &a
		}
	}

	waves := make([]map[string]interface{}, 0, len(ordered))
	for _, wr := range ordered {
		vertical, partnerSlug := parsePartnerDripTag(wr.tag)
		brand := parsePartnerDripBrand(wr.name)
		a := events[wr.id]
		if a == nil {
			a = &evAgg{}
		}
		scheduled := ""
		if wr.scheduledAt.Valid {
			scheduled = wr.scheduledAt.Time.Format(time.RFC3339)
		}
		waves = append(waves, map[string]interface{}{
			"campaign_id":      wr.id,
			"name":             wr.name,
			"vertical":         vertical,
			"brand":            brand,
			"partner_slug":     partnerSlug,
			"partner_name":     wr.partnerName,
			"dataset_id":       wr.datasetID,
			"dataset_name":     wr.datasetName,
			"status":           wr.status,
			"scheduled_at":     scheduled,
			"total_recipients": wr.totalRecipients,
			"sent":             a.sent,
			"delivered":        a.delivered,
			"opens":            a.opens,
			"clicks":           a.clicks,
			"hard_bounces":     a.hard,
			"soft_bounces":     a.soft,
			"deferred":         a.deferred,
		})
	}
	return waves, nil
}

// parsePartnerDripTag splits the orchestrator's attribution tag
// ("data_partner:{slug}/{vertical}") into (vertical, partnerSlug). Either may
// come back empty on a malformed tag — callers render the campaign anyway.
func parsePartnerDripTag(tag string) (vertical, partnerSlug string) {
	rest, ok := strings.CutPrefix(tag, "data_partner:")
	if !ok {
		return "", ""
	}
	slug, vert, ok := strings.Cut(rest, "/")
	if !ok {
		return "", rest
	}
	return vert, slug
}

// parsePartnerDripBrand extracts the brand token from the orchestrator's wave
// campaign name ("[partner-drip] {vertical} {brand} {YYYYMMDDTHHmm} {sha4}").
func parsePartnerDripBrand(name string) string {
	fields := strings.Fields(name)
	if len(fields) >= 3 && fields[0] == "[partner-drip]" {
		return fields[2]
	}
	return ""
}

// ============ GET /api/mailing/data-partners/datasets/{id}/quality-report ============

// HandleDatasetQualityReport returns the partner-reportable quality funnel for
// one dataset over a window: what they sent us → what survived intake
// (slicer-level suppression/dedup) → EmailOversight verdicts (with named
// rejection reasons) → mailed → engaged. This is the view the operator hands
// back to the partner ("X% of your leads were invalid mailboxes"), replacing
// the useless one-row-per-POST batch list.
func (h *PartnerAdminHandler) HandleDatasetQualityReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	datasetID := chi.URLParam(r, "id")
	if _, err := uuid.Parse(datasetID); err != nil {
		writeJSONError(w, "invalid dataset id", http.StatusBadRequest)
		return
	}
	days := 14
	if v, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && v > 0 && v <= 90 {
		days = v
	}

	// Dataset identity.
	var dsName, partnerName, vertical string
	if err := h.db.QueryRowContext(ctx, `
		SELECT d.name, p.name, d.vertical
		FROM partner_datasets d JOIN data_partners p ON p.id = d.partner_id
		WHERE d.id = $1
	`, datasetID).Scan(&dsName, &partnerName, &vertical); err != nil {
		writeJSONError(w, "dataset not found", http.StatusNotFound)
		return
	}

	// Intake side: posts + records the partner actually sent us.
	var posts, recordsReceived int
	_ = h.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(record_count), 0)
		FROM partner_inbound_batches
		WHERE dataset_id = $1 AND received_at > NOW() - ($2 * INTERVAL '1 day')
	`, datasetID, days).Scan(&posts, &recordsReceived)

	// Queue side: lifecycle + EO verdicts + engagement.
	var queued, eoPending, eoPassed, eoRejected, deadLetter, ready, mailed, engaged, completed int
	_ = h.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE status = 'pending_eo'),
		       COUNT(*) FILTER (WHERE eo_result_code IN (1, 7)),
		       COUNT(*) FILTER (WHERE status = 'suppressed_eo'),
		       COUNT(*) FILTER (WHERE status = 'dead_letter'),
		       COUNT(*) FILTER (WHERE status = 'ready'),
		       COUNT(*) FILTER (WHERE status = 'mailed'),
		       COUNT(*) FILTER (WHERE engaged_at IS NOT NULL),
		       COUNT(*) FILTER (WHERE terminal_reason = 'completed')
		FROM partner_clean_queue
		WHERE dataset_id = $1 AND ingested_at > NOW() - ($2 * INTERVAL '1 day')
	`, datasetID, days).Scan(&queued, &eoPending, &eoPassed, &eoRejected, &deadLetter, &ready, &mailed, &engaged, &completed)

	intakeDropped := recordsReceived - queued
	if intakeDropped < 0 {
		intakeDropped = 0 // queue rows can outlive the batch window edge
	}

	// Named rejection reasons — the partner-reportable part.
	reasons := []map[string]interface{}{}
	if rows, err := h.db.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(eo_result, ''), 'unknown'), COALESCE(eo_result_code, -1), COUNT(*)
		FROM partner_clean_queue
		WHERE dataset_id = $1 AND ingested_at > NOW() - ($2 * INTERVAL '1 day')
		  AND status IN ('suppressed_eo', 'dead_letter')
		GROUP BY 1, 2 ORDER BY 3 DESC LIMIT 15
	`, datasetID, days); err == nil {
		defer rows.Close()
		for rows.Next() {
			var reason string
			var code, count int
			if err := rows.Scan(&reason, &code, &count); err == nil {
				reasons = append(reasons, map[string]interface{}{"reason": reason, "code": code, "count": count})
			}
		}
	}

	// ISP mix of what survived.
	ispMix := []map[string]interface{}{}
	if rows, err := h.db.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(isp_family, ''), 'other'),
		       COUNT(*),
		       COUNT(*) FILTER (WHERE status = 'ready'),
		       COUNT(*) FILTER (WHERE status = 'mailed'),
		       COUNT(*) FILTER (WHERE engaged_at IS NOT NULL)
		FROM partner_clean_queue
		WHERE dataset_id = $1 AND ingested_at > NOW() - ($2 * INTERVAL '1 day')
		GROUP BY 1 ORDER BY 2 DESC
	`, datasetID, days); err == nil {
		defer rows.Close()
		for rows.Next() {
			var isp string
			var total, rdy, mld, eng int
			if err := rows.Scan(&isp, &total, &rdy, &mld, &eng); err == nil {
				ispMix = append(ispMix, map[string]interface{}{
					"isp": isp, "queued": total, "ready": rdy, "mailed": mld, "engaged": eng,
				})
			}
		}
	}

	// Daily trend: received (batch side) vs passed/rejected (queue side, by
	// ingest day) vs mailed (by mailed day). Merged on day in Go.
	type dayAgg struct {
		posts, records, passed, rejected, mailedN, engagedN int
	}
	daysMap := map[string]*dayAgg{}
	getDay := func(k string) *dayAgg {
		if d, ok := daysMap[k]; ok {
			return d
		}
		d := &dayAgg{}
		daysMap[k] = d
		return d
	}
	if rows, err := h.db.QueryContext(ctx, `
		SELECT date_trunc('day', received_at)::date::text, COUNT(*), COALESCE(SUM(record_count), 0)
		FROM partner_inbound_batches
		WHERE dataset_id = $1 AND received_at > NOW() - ($2 * INTERVAL '1 day')
		GROUP BY 1
	`, datasetID, days); err == nil {
		defer rows.Close()
		for rows.Next() {
			var day string
			var p, rec int
			if err := rows.Scan(&day, &p, &rec); err == nil {
				d := getDay(day)
				d.posts, d.records = p, rec
			}
		}
	}
	if rows, err := h.db.QueryContext(ctx, `
		SELECT date_trunc('day', ingested_at)::date::text,
		       COUNT(*) FILTER (WHERE eo_result_code IN (1, 7)),
		       COUNT(*) FILTER (WHERE status IN ('suppressed_eo', 'dead_letter'))
		FROM partner_clean_queue
		WHERE dataset_id = $1 AND ingested_at > NOW() - ($2 * INTERVAL '1 day')
		GROUP BY 1
	`, datasetID, days); err == nil {
		defer rows.Close()
		for rows.Next() {
			var day string
			var passed, rejected int
			if err := rows.Scan(&day, &passed, &rejected); err == nil {
				d := getDay(day)
				d.passed, d.rejected = passed, rejected
			}
		}
	}
	if rows, err := h.db.QueryContext(ctx, `
		SELECT date_trunc('day', mailed_at)::date::text,
		       COUNT(*),
		       COUNT(*) FILTER (WHERE engaged_at IS NOT NULL)
		FROM partner_clean_queue
		WHERE dataset_id = $1 AND mailed_at IS NOT NULL
		  AND mailed_at > NOW() - ($2 * INTERVAL '1 day')
		GROUP BY 1
	`, datasetID, days); err == nil {
		defer rows.Close()
		for rows.Next() {
			var day string
			var m, e int
			if err := rows.Scan(&day, &m, &e); err == nil {
				d := getDay(day)
				d.mailedN, d.engagedN = m, e
			}
		}
	}
	dayKeys := make([]string, 0, len(daysMap))
	for k := range daysMap {
		dayKeys = append(dayKeys, k)
	}
	sort.Strings(dayKeys)
	daily := make([]map[string]interface{}, 0, len(dayKeys))
	for _, k := range dayKeys {
		d := daysMap[k]
		daily = append(daily, map[string]interface{}{
			"day": k, "posts": d.posts, "records": d.records,
			"eo_passed": d.passed, "eo_rejected": d.rejected,
			"mailed": d.mailedN, "engaged": d.engagedN,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"dataset_id":   datasetID,
		"dataset_name": dsName,
		"partner_name": partnerName,
		"vertical":     vertical,
		"days":         days,
		"totals": map[string]interface{}{
			"posts":            posts,
			"records_received": recordsReceived,
			"queued":           queued,
			"intake_dropped":   intakeDropped,
			"eo_pending":       eoPending,
			"eo_passed":        eoPassed,
			"eo_rejected":      eoRejected,
			"dead_letter":      deadLetter,
			"ready":            ready,
			"mailed":           mailed,
			"engaged":          engaged,
			"completed":        completed,
		},
		"rejection_reasons": reasons,
		"isp_mix":           ispMix,
		"daily":             daily,
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

// ============ GET /api/mailing/data-partners/warmup-progress ============

// warmupDatasetID is the partner dataset whose ISP overrides hold the per-wave
// ramp caps for the KumoMTA new-domain warm-up (mpf / pmd / trb).
const warmupDatasetID = "c4400fab-64dd-41ed-aed3-aa3f7d35c3da"

// warmupBrands restricts the warm-up panel to the three brand-new sending
// domains being ramped on KumoMTA. The samsclub_internal drip campaign-name
// pattern also matches the 16 mature brands, so we filter to these.
var warmupBrands = []string{"mpf", "pmd", "trb"}

// warmupISPExpr derives an ISP family from recipient_domain in SQL, matching
// the partner slicer's families (yahoo annex / aol / microsoft / apple /
// comcast / gmail / other). Kept in one place so the per-domain, by-ISP, and
// recovery-curve queries agree.
const warmupISPExpr = `
	CASE
	    WHEN t.recipient_domain ~* '(^|\.)(yahoo|ymail|rocketmail)\.'  THEN 'yahoo'
	    WHEN t.recipient_domain ~* '(^|\.)aol\.'                       THEN 'aol'
	    WHEN t.recipient_domain ~* '(^|\.)(outlook|hotmail|live|msn)\.' THEN 'microsoft'
	    WHEN t.recipient_domain ~* '(^|\.)(icloud|me|mac)\.'           THEN 'apple'
	    WHEN t.recipient_domain ~* '(^|\.)(comcast|xfinity)\.'         THEN 'comcast'
	    WHEN t.recipient_domain ~* '(^|\.)gmail\.'                     THEN 'gmail'
	    ELSE 'other'
	END`

// HandleGetWarmupProgress backs the read-only "Warm-Up Progress" panel that
// tracks the KumoMTA new-domain warm-up (em.mypersonalfinancial.com /
// em.paymydebit.com / em.theretirementblog.com). Everything is re-aggregated
// LIVE from mailing_tracking_events on each poll — campaign counter columns are
// known-stale and never used. Four sections:
//
//   - domains[]:        per (brand, sending_domain) window roll-up. Bounces are
//                       split hard vs soft per bounce-metrics doctrine.
//   - by_isp[]:         per (brand, derived ISP) window roll-up. The warm-up is
//                       Yahoo/AOL only, so most volume lands in those families.
//   - recovery_curve[]: per UTC hour bucket, overall + per-brand, the
//                       delivered/sent ramp (the "recovery" the operator watches).
//   - caps[]:           current per-wave ISP caps from the dataset's overrides.
//
// Default window 72h; ?hours=N (1..168) overrides. Read-only: no writes, no
// org scoping (the partner-admin group is admin-gated at the router and the
// warm-up campaigns are global by campaign name — matching the sibling
// drip-performance handlers).
func (h *PartnerAdminHandler) HandleGetWarmupProgress(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	hours := 72
	if v, err := strconv.Atoi(r.URL.Query().Get("hours")); err == nil && v > 0 && v <= 168 {
		hours = v
	}

	resp := map[string]interface{}{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"window_hours": hours,
		"dataset_id":   warmupDatasetID,
		"brands":       warmupBrands,
		// bounce_type on mailing_tracking_events distinguishes hard from soft,
		// so the panel splits them rather than reporting a combined "bounced".
		"bounce_split":    true,
		"engagement_note": "opens/clicks are pending tracking wiring for these new domains — currently 0",
	}

	if domains, err := h.warmupDomains(ctx, hours); err != nil {
		resp["domains_error"] = err.Error()
	} else {
		resp["domains"] = domains
	}

	if byISP, err := h.warmupByISP(ctx, hours); err != nil {
		resp["by_isp_error"] = err.Error()
	} else {
		resp["by_isp"] = byISP
	}

	if curve, err := h.warmupRecoveryCurve(ctx, hours); err != nil {
		resp["recovery_curve_error"] = err.Error()
	} else {
		resp["recovery_curve"] = curve
	}

	if caps, err := h.warmupCaps(ctx); err != nil {
		resp["caps_error"] = err.Error()
	} else {
		resp["caps"] = caps
	}

	writeJSON(w, http.StatusOK, resp)
}

// warmupDomains rolls every event up per (brand, sending_domain) over the
// window, straight from tracking events. hard vs soft split per doctrine.
func (h *PartnerAdminHandler) warmupDomains(ctx context.Context, hours int) ([]map[string]interface{}, error) {
	hb := HardBounceSQL("t")
	rows, err := h.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT split_part(c.name, ' ', 3) AS brand,
		       t.sending_domain,
		       COALESCE(SUM(CASE WHEN t.event_type = 'sent' THEN 1 ELSE 0 END), 0)      AS sent,
		       COALESCE(SUM(CASE WHEN t.event_type = 'delivered' THEN 1 ELSE 0 END), 0) AS delivered,
		       COALESCE(SUM(CASE WHEN t.event_type IN ('deferred','deferral') THEN 1 ELSE 0 END), 0) AS deferred,
		       COALESCE(SUM(CASE WHEN t.event_type = 'bounced' AND %s THEN 1 ELSE 0 END), 0)         AS hard,
		       COALESCE(SUM(CASE WHEN t.event_type = 'bounced' AND NOT (%s) THEN 1 ELSE 0 END), 0)   AS soft,
		       COALESCE(SUM(CASE WHEN t.event_type = 'opened' THEN 1 ELSE 0 END), 0)    AS opened,
		       COALESCE(SUM(CASE WHEN t.event_type = 'clicked' THEN 1 ELSE 0 END), 0)   AS clicked
		FROM mailing_tracking_events t
		JOIN mailing_campaigns c ON c.id = t.campaign_id
		WHERE c.name ILIKE '%%partner-drip%%samsclub_internal%%'
		  AND split_part(c.name, ' ', 3) = ANY($2)
		  AND t.event_at > NOW() - ($1 * INTERVAL '1 hour')
		GROUP BY 1, 2
		ORDER BY sent DESC
	`, hb, hb), hours, pq.Array(warmupBrands))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]map[string]interface{}, 0, len(warmupBrands))
	for rows.Next() {
		var brand, domain string
		var sent, delivered, deferred, hard, soft, opened, clicked int
		if err := rows.Scan(&brand, &domain, &sent, &delivered, &deferred, &hard, &soft, &opened, &clicked); err != nil {
			continue
		}
		deliveredPct := 0.0
		if sent > 0 {
			deliveredPct = float64(delivered) / float64(sent) * 100.0
		}
		out = append(out, map[string]interface{}{
			"brand":         brand,
			"domain":        domain,
			"sent":          sent,
			"delivered":     delivered,
			"deferred":      deferred,
			"hard_bounce":   hard,
			"soft_bounce":   soft,
			"opened":        opened,
			"clicked":       clicked,
			"delivered_pct": deliveredPct,
		})
	}
	return out, rows.Err()
}

// warmupByISP rolls events up per (brand, derived ISP). Bounces are combined
// here (the per-domain section carries the hard/soft split) to keep the ISP
// matrix compact.
func (h *PartnerAdminHandler) warmupByISP(ctx context.Context, hours int) ([]map[string]interface{}, error) {
	rows, err := h.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT split_part(c.name, ' ', 3) AS brand,
		       %s AS isp,
		       COALESCE(SUM(CASE WHEN t.event_type = 'sent' THEN 1 ELSE 0 END), 0)      AS sent,
		       COALESCE(SUM(CASE WHEN t.event_type = 'delivered' THEN 1 ELSE 0 END), 0) AS delivered,
		       COALESCE(SUM(CASE WHEN t.event_type IN ('deferred','deferral') THEN 1 ELSE 0 END), 0) AS deferred,
		       COALESCE(SUM(CASE WHEN t.event_type = 'bounced' THEN 1 ELSE 0 END), 0)   AS bounced,
		       COALESCE(SUM(CASE WHEN t.event_type = 'opened' THEN 1 ELSE 0 END), 0)    AS opened,
		       COALESCE(SUM(CASE WHEN t.event_type = 'clicked' THEN 1 ELSE 0 END), 0)   AS clicked
		FROM mailing_tracking_events t
		JOIN mailing_campaigns c ON c.id = t.campaign_id
		WHERE c.name ILIKE '%%partner-drip%%samsclub_internal%%'
		  AND split_part(c.name, ' ', 3) = ANY($2)
		  AND t.event_at > NOW() - ($1 * INTERVAL '1 hour')
		GROUP BY 1, 2
		ORDER BY 1, sent DESC
	`, warmupISPExpr), hours, pq.Array(warmupBrands))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]map[string]interface{}, 0, 16)
	for rows.Next() {
		var brand, isp string
		var sent, delivered, deferred, bounced, opened, clicked int
		if err := rows.Scan(&brand, &isp, &sent, &delivered, &deferred, &bounced, &opened, &clicked); err != nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"brand":     brand,
			"isp":       isp,
			"sent":      sent,
			"delivered": delivered,
			"deferred":  deferred,
			"bounced":   bounced,
			"opened":    opened,
			"clicked":   clicked,
		})
	}
	return out, rows.Err()
}

// warmupRecoveryCurve buckets sent/delivered per UTC hour — once overall
// (brand="") and once per brand — so the UI can plot the delivered/sent ramp
// as the warm-up recovers.
func (h *PartnerAdminHandler) warmupRecoveryCurve(ctx context.Context, hours int) ([]map[string]interface{}, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT date_trunc('hour', t.event_at) AS hour_utc,
		       grp.brand,
		       COALESCE(SUM(CASE WHEN t.event_type = 'sent' THEN 1 ELSE 0 END), 0)      AS sent,
		       COALESCE(SUM(CASE WHEN t.event_type = 'delivered' THEN 1 ELSE 0 END), 0) AS delivered
		FROM mailing_tracking_events t
		JOIN mailing_campaigns c ON c.id = t.campaign_id
		-- grp emits each event twice: once under its brand, once under the
		-- overall bucket (brand=''), so one scan yields both granularities.
		CROSS JOIN LATERAL (VALUES (split_part(c.name, ' ', 3)), ('')) AS grp(brand)
		WHERE c.name ILIKE '%partner-drip%samsclub_internal%'
		  AND split_part(c.name, ' ', 3) = ANY($2)
		  AND t.event_at > NOW() - ($1 * INTERVAL '1 hour')
		  AND t.event_type IN ('sent','delivered')
		GROUP BY 1, 2
		ORDER BY 1, 2
	`, hours, pq.Array(warmupBrands))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]map[string]interface{}, 0, 256)
	for rows.Next() {
		var hr time.Time
		var brand string
		var sent, delivered int
		if err := rows.Scan(&hr, &brand, &sent, &delivered); err != nil {
			continue
		}
		deliveredPct := 0.0
		if sent > 0 {
			deliveredPct = float64(delivered) / float64(sent) * 100.0
		}
		out = append(out, map[string]interface{}{
			"hour_utc":      hr.UTC().Format(time.RFC3339),
			"brand":         brand, // "" = overall across all warm-up brands
			"sent":          sent,
			"delivered":     delivered,
			"delivered_pct": deliveredPct,
		})
	}
	return out, rows.Err()
}

// warmupCaps returns the current per-wave ISP caps for the warm-up dataset.
func (h *PartnerAdminHandler) warmupCaps(ctx context.Context) ([]map[string]interface{}, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT isp, COALESCE(max_per_wave, 0)
		FROM partner_isp_distribution_overrides
		WHERE dataset_id = $1
		ORDER BY max_per_wave DESC, isp
	`, warmupDatasetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]map[string]interface{}, 0, 16)
	for rows.Next() {
		var isp string
		var maxPerWave int
		if err := rows.Scan(&isp, &maxPerWave); err != nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"isp":          isp,
			"max_per_wave": maxPerWave,
		})
	}
	return out, rows.Err()
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


// ============ GET /api/mailing/data-partners/previous-activations ============

// HandleGetPreviousActivations reports, per previous data activation (dataset),
// the LIFETIME engagement footprint of the dataset's records on the partner's
// OWN mail — so a data partner can be judged on the whole activation, not a
// rolling slice.
//
// Accuracy notes (v3, 2026-06-22, after the operator flagged the v2 screen as
// internally inconsistent — a 7-day "Mailed" next to all-time conversions read
// as broken):
//   - ALL primary metrics are LIFETIME and share one timeframe. v2 mixed a 7-day
//     window (mailed/opens/clicks/bounce) with all-time (total_records,
//     conv_lifetime); a dormant-but-historically-huge dataset (Spicy: 114,650
//     lifetime mailed) showed "Mailed 200", which destroyed trust.
//   - opens/clicks/bounces are scoped to the dataset's OWN campaigns via
//     mailing_campaigns.partner_dataset_id (the same scoping the engaged_at
//     marker uses), joined on (subscriber_id, campaign_id). v2 joined events on
//     subscriber_id ALONE against a global campaign set, so a subscriber who
//     engaged with ANOTHER dataset's campaign leaked in (verified over-count).
//     partner_dataset_id scoping also captures EVERY touch — per-row
//     mailed_campaign_id only holds the last touch and undercounted Spicy ~3-4x.
//   - bounce% is lifetime and surfaced prominently: it's the list-quality tell
//     the 7-day window hid (recent survivors show ~0%, lifetime shows 30-47%).
//   - recent_mailed / recent_clicks (last `days`) are a small freshness signal,
//     clearly separate from the lifetime headline — never mixed into a rate.
//   - conv = all conversions ever tied to the dataset's records (distinct
//     conversion). revenue is $0 by design (CPM pays per-send); rank by clicks +
//     conv, not revenue. opens are ~90% Apple-MPP/machine — weak signal.
//   - cost/CPM per record is NOT in the system (needs partner deal terms).
//
// Computed live as 3 cheap queries merged in Go (~10s total; each well under the
// 30s mailingDB statement_timeout) — no background rollup, so numbers are always
// fresh on load with no staleness/worker-bug surface.
func (h *PartnerAdminHandler) HandleGetPreviousActivations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	days := 7 // recency window for the freshness columns only; headline is lifetime
	if v, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && v > 0 && v <= 90 {
		days = v
	}

	type activation struct {
		DatasetID    string  `json:"dataset_id"`
		DatasetName  string  `json:"dataset_name"`
		PartnerName  string  `json:"partner_name"`
		Vertical     string  `json:"vertical"`
		TotalRecords int     `json:"total_records"`
		Mailed       int     `json:"mailed"`
		Opens        int     `json:"opens"`
		OpensPct     float64 `json:"opens_pct"`
		Clicks       int     `json:"clicks"`
		ClicksPct    float64 `json:"clicks_pct"`
		Bounced      int     `json:"bounced"`
		BouncePct    float64 `json:"bounce_pct"`
		ConvLifetime int     `json:"conv_lifetime"`
		RecentMailed int     `json:"recent_mailed"`
		RecentClicks int     `json:"recent_clicks"`
		RecentDays   int     `json:"recent_days"`
		Trend        string  `json:"trend"`
		LowSample    bool    `json:"low_sample"`
	}

	// Q1 — per-dataset volume + recency (partner_clean_queue only; cheap ~1s).
	byID := map[string]*activation{}
	order := []string{}
	prevClicks := map[string]int{}
	q1, err := h.db.QueryContext(ctx, `
		SELECT d.id, d.name, p.name, COALESCE(d.vertical, ''),
		       (SELECT COUNT(*) FROM partner_clean_queue q2 WHERE q2.dataset_id = d.id) AS total_records,
		       q.mailed, q.recent_mailed, q.recent_clicks, q.prev_clicks
		FROM partner_datasets d
		JOIN data_partners p ON p.id = d.partner_id
		JOIN (
			SELECT dataset_id,
			       COUNT(DISTINCT subscriber_id) FILTER (WHERE mailed_campaign_id IS NOT NULL) AS mailed,
			       COUNT(DISTINCT subscriber_id) FILTER (WHERE mailed_at > NOW() - make_interval(days => $1)) AS recent_mailed,
			       COUNT(DISTINCT subscriber_id) FILTER (WHERE engaged_at > NOW() - make_interval(days => $1)) AS recent_clicks,
			       COUNT(DISTINCT subscriber_id) FILTER (WHERE engaged_at > NOW() - make_interval(days => $1 * 2) AND engaged_at <= NOW() - make_interval(days => $1)) AS prev_clicks
			FROM partner_clean_queue
			WHERE subscriber_id IS NOT NULL
			GROUP BY dataset_id
		) q ON q.dataset_id = d.id
		WHERE q.mailed > 0
	`, days)
	if err != nil {
		writeJSONError(w, "previous_activations_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for q1.Next() {
		a := &activation{RecentDays: days}
		var prev int
		if err := q1.Scan(&a.DatasetID, &a.DatasetName, &a.PartnerName, &a.Vertical, &a.TotalRecords,
			&a.Mailed, &a.RecentMailed, &a.RecentClicks, &prev); err != nil {
			q1.Close()
			writeJSONError(w, "previous_activations_scan_failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		byID[a.DatasetID] = a
		prevClicks[a.DatasetID] = prev
		order = append(order, a.DatasetID)
	}
	q1.Close()
	if err := q1.Err(); err != nil {
		writeJSONError(w, "previous_activations_iter_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Q2 — lifetime opens/clicks/bounces on the dataset's OWN campaigns
	// (partner_dataset_id scoping = all touches; joined on subscriber+campaign so
	// no cross-dataset leak). Heaviest query (~7s).
	q2, err := h.db.QueryContext(ctx, `
		WITH subs AS (
			SELECT DISTINCT dataset_id, subscriber_id
			FROM partner_clean_queue
			WHERE subscriber_id IS NOT NULL AND mailed_campaign_id IS NOT NULL
		),
		camps AS (SELECT id, partner_dataset_id AS ds FROM mailing_campaigns WHERE partner_dataset_id IS NOT NULL)
		SELECT s.dataset_id,
		       COUNT(DISTINCT s.subscriber_id) FILTER (WHERE te.event_type = 'opened')  AS opens,
		       COUNT(DISTINCT s.subscriber_id) FILTER (WHERE te.event_type = 'clicked') AS clicks,
		       COUNT(DISTINCT s.subscriber_id) FILTER (WHERE te.event_type = 'bounced') AS bounced
		FROM subs s
		JOIN camps cp ON cp.ds = s.dataset_id
		JOIN mailing_tracking_events te
		  ON te.subscriber_id = s.subscriber_id AND te.campaign_id = cp.id
		 AND te.event_type IN ('opened','clicked','bounced')
		GROUP BY s.dataset_id
	`)
	if err != nil {
		writeJSONError(w, "previous_activations_events_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for q2.Next() {
		var id string
		var o, k, b int
		if err := q2.Scan(&id, &o, &k, &b); err != nil {
			q2.Close()
			writeJSONError(w, "previous_activations_events_scan_failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if a := byID[id]; a != nil {
			a.Opens, a.Clicks, a.Bounced = o, k, b
		}
	}
	q2.Close()
	if err := q2.Err(); err != nil {
		writeJSONError(w, "previous_activations_events_iter_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Q3 — lifetime conversions: distinct conversion tied to the dataset's
	// subscribers (a subscriber in N datasets attributes to each, by design;
	// distinct conversion_id avoids duplicate-row multiplication). ~2s.
	q3, err := h.db.QueryContext(ctx, `
		SELECT dataset_id, COUNT(*) AS c FROM (
			SELECT DISTINCT p.dataset_id, mc.conversion_id
			FROM mailing_cpm_manual_conversions mc
			JOIN (SELECT DISTINCT dataset_id, subscriber_id FROM partner_clean_queue WHERE subscriber_id IS NOT NULL) p
			  ON p.subscriber_id::text = mc.sub1
		) z GROUP BY dataset_id
	`)
	if err != nil {
		writeJSONError(w, "previous_activations_conv_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for q3.Next() {
		var id string
		var c int
		if err := q3.Scan(&id, &c); err != nil {
			q3.Close()
			writeJSONError(w, "previous_activations_conv_scan_failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if a := byID[id]; a != nil {
			a.ConvLifetime = c
		}
	}
	q3.Close()
	if err := q3.Err(); err != nil {
		writeJSONError(w, "previous_activations_conv_iter_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	pct := func(n, d int) float64 {
		if d == 0 {
			return 0
		}
		return float64(int(float64(n)/float64(d)*10000+0.5)) / 100
	}

	out := make([]activation, 0, len(order))
	var tMailed, tOpens, tClicks, tBounced, tConvL int
	for _, id := range order {
		a := byID[id]
		a.OpensPct = pct(a.Opens, a.Mailed)
		a.ClicksPct = pct(a.Clicks, a.Mailed)
		a.BouncePct = pct(a.Bounced, a.Mailed)
		switch {
		case a.RecentClicks > prevClicks[id]:
			a.Trend = "up"
		case a.RecentClicks < prevClicks[id]:
			a.Trend = "down"
		default:
			a.Trend = "flat"
		}
		a.LowSample = a.Mailed < 1000
		tMailed += a.Mailed
		tOpens += a.Opens
		tClicks += a.Clicks
		tBounced += a.Bounced
		tConvL += a.ConvLifetime
		out = append(out, *a)
	}
	// Rank by lifetime clicks (the human signal), then conversions.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Clicks != out[j].Clicks {
			return out[i].Clicks > out[j].Clicks
		}
		return out[i].ConvLifetime > out[j].ConvLifetime
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"days":        days,
		"scope":       "lifetime",
		"activations": out,
		"totals": map[string]interface{}{
			"mailed":        tMailed,
			"opens":         tOpens,
			"opens_pct":     pct(tOpens, tMailed),
			"clicks":        tClicks,
			"clicks_pct":    pct(tClicks, tMailed),
			"bounced":       tBounced,
			"bounce_pct":    pct(tBounced, tMailed),
			"conv_lifetime": tConvL,
		},
		// Honest caveats surfaced to the UI so the numbers aren't over-read.
		"notes": map[string]interface{}{
			"scope":            "all headline metrics are LIFETIME (the whole activation) on the dataset's own campaigns; recent_mailed/recent_clicks are a separate last-N-day freshness signal",
			"engagement_scope": "opens/clicks/bounces scoped to the dataset's own campaigns via partner_dataset_id (all touches), joined on subscriber+campaign — no cross-dataset leak",
			"bounce_caveat":    "bounce% is lifetime list-health — the single best partner-quality tell; dirty lists run 30-47%",
			"opens_caveat":     "opens are ~90% Apple-MPP/machine industry-wide; rank by clicks + conversions",
			"revenue":          "CPM deals pay per-send; conversion revenue is $0 by design — judge by conversion count",
			"cost_missing":     "cost/CPM per record is not in the system; needs partner deal terms for true ROI",
		},
	})
}
