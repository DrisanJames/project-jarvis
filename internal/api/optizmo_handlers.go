package api

import (
	"bufio"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// OptizmoHandlers provides HTTP handlers for Optizmo list scrub management.
type OptizmoHandlers struct {
	db *sql.DB
}

type optizmoMatch struct {
	ID    string
	Email string
	Hash  string
}

// ---------------------------------------------------------------------------
// Response types
// ---------------------------------------------------------------------------

type ScrubJobResponse struct {
	ID              string     `json:"id"`
	FileCount       int        `json:"file_count"`
	AudienceCount   int        `json:"audience_count"`
	SuppressedCount int        `json:"suppressed_count"`
	Status          string     `json:"status"`
	ErrorMessage    string     `json:"error_message,omitempty"`
	RequestedAt     time.Time  `json:"requested_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

type ScrubStatusResponse struct {
	OptizmoStatus         *string            `json:"optizmo_status"`
	OptizmoLastScrubbedAt *time.Time         `json:"optizmo_last_scrubbed_at"`
	Jobs                  []ScrubJobResponse `json:"jobs"`
}

// ---------------------------------------------------------------------------
// HandleRequestScrub — POST /offer-center/offers/{id}/optizmo/request-scrub
//
// Downloads the Optizmo suppression list from the offer's optizmo_link,
// matches subscriber MD5 hashes against it, inserts suppressions, and
// marks the job complete — all inline (async goroutine for the heavy work).
// ---------------------------------------------------------------------------

func (h *OptizmoHandlers) HandleRequestScrub(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id is required")
		return
	}

	ctx := r.Context()

	var offerExists bool
	var optizmoLink sql.NullString
	var offerName string
	err := h.db.QueryRowContext(ctx,
		`SELECT TRUE, optizmo_link, COALESCE(name,'') FROM mailing_offers WHERE id = $1`, offerID,
	).Scan(&offerExists, &optizmoLink, &offerName)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "offer not found")
		return
	}
	if err != nil {
		log.Printf("[Optizmo] error checking offer %s: %v", offerID, err)
		respondError(w, http.StatusInternalServerError, "failed to verify offer")
		return
	}

	link := ""
	if optizmoLink.Valid {
		link = strings.TrimSpace(optizmoLink.String)
	}
	if link == "" {
		respondError(w, http.StatusBadRequest, "offer has no Optizmo link configured")
		return
	}

	jobID := uuid.New().String()
	now := time.Now()
	_, err = h.db.ExecContext(ctx,
		`INSERT INTO mailing_optizmo_scrub_jobs
			(id, offer_id, audience_file_path, audience_count, status, requested_at)
		 VALUES ($1, $2, '', 0, 'pending', $3)`,
		jobID, offerID, now)
	if err != nil {
		log.Printf("[Optizmo] error creating scrub job: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to create scrub job")
		return
	}

	_, _ = h.db.ExecContext(ctx,
		`UPDATE mailing_offers SET optizmo_status = 'scrub_pending', updated_at = NOW() WHERE id = $1`,
		offerID)

	log.Printf("[Optizmo] scrub requested for offer %s (%s) — job %s, downloading from %s", offerID, offerName, jobID, link)

	go h.runInlineScrub(jobID, offerID, offerName, link)

	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"job_id": jobID,
		"status": "pending",
	})
}

// runInlineScrub downloads the Optizmo suppression list, matches against
// subscribers, inserts suppressions, creates a named suppression list, and
// finalizes the job+offer status.
func (h *OptizmoHandlers) runInlineScrub(jobID, offerID, offerName, optizmoLink string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprintf("goroutine panic: %v", r)
			log.Printf("[Optizmo] scrub job %s PANICKED: %s", jobID, msg)
			h.db.ExecContext(ctx,
				`UPDATE mailing_optizmo_scrub_jobs
				 SET status = 'failed', error_message = $1, completed_at = NOW()
				 WHERE id = $2`, msg, jobID)
			h.db.ExecContext(ctx,
				`UPDATE mailing_offers SET optizmo_status = 'scrub_failed', updated_at = NOW()
				 WHERE id = $1`, offerID)
		}
	}()

	fail := func(msg string) {
		log.Printf("[Optizmo] scrub job %s FAILED: %s", jobID, msg)
		h.db.ExecContext(ctx,
			`UPDATE mailing_optizmo_scrub_jobs
			 SET status = 'failed', error_message = $1, completed_at = NOW()
			 WHERE id = $2`, msg, jobID)
		h.db.ExecContext(ctx,
			`UPDATE mailing_offers SET optizmo_status = 'scrub_failed', updated_at = NOW()
			 WHERE id = $1`, offerID)
	}

	_, err := h.db.ExecContext(ctx,
		`UPDATE mailing_optizmo_scrub_jobs SET status = 'processing', started_at = NOW() WHERE id = $1`, jobID)
	if err != nil {
		log.Printf("[Optizmo] error setting job %s to processing: %v", jobID, err)
	}

	client := &http.Client{Timeout: 3 * time.Minute}
	resp, err := client.Get(optizmoLink)
	if err != nil {
		fail(fmt.Sprintf("failed to download Optizmo list: %v", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodySnippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		fail(fmt.Sprintf("Optizmo returned HTTP %d: %s", resp.StatusCode, string(bodySnippet)))
		return
	}

	suppressedHashes := make(map[string]bool)
	var fileLineCount, validMD5Count, nonMD5Count int
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lower := strings.ToLower(line)
		fileLineCount++
		if isHexMD5(lower) {
			validMD5Count++
		} else {
			nonMD5Count++
		}
		suppressedHashes[lower] = true
	}
	if err := scanner.Err(); err != nil {
		fail(fmt.Sprintf("error reading Optizmo response: %v", err))
		return
	}

	log.Printf("[Optizmo] job %s: downloaded %d entries (%d valid MD5, %d non-MD5)",
		jobID, fileLineCount, validMD5Count, nonMD5Count)
	if nonMD5Count > 0 {
		log.Printf("[Optizmo] WARNING job %s: %d entries are NOT valid MD5 hashes — matching may fail for those entries. Ensure Optizmo download format is set to MD5.",
			jobID, nonMD5Count)
	}

	h.db.ExecContext(ctx,
		`UPDATE mailing_optizmo_scrub_jobs SET file_count = $1 WHERE id = $2`,
		fileLineCount, jobID)

	if len(suppressedHashes) == 0 {
		h.db.ExecContext(ctx,
			`UPDATE mailing_optizmo_scrub_jobs
			 SET status = 'completed', audience_count = 0, suppressed_count = 0, completed_at = NOW()
			 WHERE id = $1`, jobID)
		h.db.ExecContext(ctx,
			`UPDATE mailing_offers SET optizmo_status = 'scrubbed', optizmo_last_scrubbed_at = NOW(), updated_at = NOW()
			 WHERE id = $1`, offerID)
		log.Printf("[Optizmo] job %s: Optizmo list was empty — offer marked scrubbed (no suppressions needed)", jobID)
		return
	}

	subRows, err := h.db.QueryContext(ctx,
		`SELECT id, LOWER(TRIM(email)) FROM mailing_subscribers
		 WHERE organization_id = $1 AND status = 'confirmed'`, defaultOrgID)
	if err != nil {
		fail(fmt.Sprintf("failed to query subscribers: %v", err))
		return
	}
	defer subRows.Close()

	var matches []optizmoMatch
	var audienceCount int

	for subRows.Next() {
		var subID, email string
		if err := subRows.Scan(&subID, &email); err != nil {
			continue
		}
		audienceCount++
		emailHash := md5Hash(email)
		if suppressedHashes[emailHash] {
			matches = append(matches, optizmoMatch{ID: subID, Email: email, Hash: emailHash})
		}
	}

	h.db.ExecContext(ctx,
		`UPDATE mailing_optizmo_scrub_jobs SET audience_count = $1 WHERE id = $2`,
		audienceCount, jobID)

	suppressedCount := 0
	for _, m := range matches {
		_, err := h.db.ExecContext(ctx,
			`INSERT INTO mailing_offer_suppressions
				(organization_id, offer_id, subscriber_id, email_hash, reason, source, suppressed_at)
			 VALUES ($1, $2, $3, $4, 'optizmo', 'optizmo_scrub', NOW())
			 ON CONFLICT (offer_id, subscriber_id) DO NOTHING`,
			defaultOrgID, offerID, m.ID, m.Hash)
		if err != nil {
			log.Printf("[Optizmo] error inserting suppression for subscriber %s: %v", m.ID, err)
			continue
		}
		suppressedCount++
	}

	now := time.Now()
	h.db.ExecContext(ctx,
		`UPDATE mailing_optizmo_scrub_jobs
		 SET status = 'completed', suppressed_count = $1, completed_at = $2
		 WHERE id = $3`, suppressedCount, now, jobID)
	h.db.ExecContext(ctx,
		`UPDATE mailing_offers
		 SET optizmo_status = 'scrubbed', optizmo_last_scrubbed_at = $1, updated_at = NOW()
		 WHERE id = $2`, now, offerID)

	// Create a named suppression list so it can be selected as an exclusion
	h.createSuppressionListFromScrub(ctx, offerID, offerName, matches)

	log.Printf("[Optizmo] job %s COMPLETED: %d audience, %d matched, %d suppressed",
		jobID, audienceCount, len(matches), suppressedCount)
}

// createSuppressionListFromScrub creates/updates a named suppression list
// "[offer name] - suppression" populated with the scrub delta.
func (h *OptizmoHandlers) createSuppressionListFromScrub(ctx context.Context, offerID, offerName string, matches []optizmoMatch) {
	listName := offerName + " - suppression"
	listID := "optizmo-" + offerID

	_, err := h.db.ExecContext(ctx,
		`INSERT INTO mailing_suppression_lists (id, name, description, source, entry_count, created_at, updated_at)
		 VALUES ($1, $2, $3, 'optizmo', 0, NOW(), NOW())
		 ON CONFLICT (id) DO UPDATE SET name = $2, updated_at = NOW()`,
		listID, listName, fmt.Sprintf("Optizmo scrub delta for offer: %s", offerName))
	if err != nil {
		log.Printf("[Optizmo] error creating suppression list %q: %v", listName, err)
		return
	}

	inserted := 0
	for _, m := range matches {
		entryID := uuid.New().String()
		_, err := h.db.ExecContext(ctx,
			`INSERT INTO mailing_suppression_entries (id, list_id, email, md5_hash, reason, source, created_at)
			 VALUES ($1, $2, $3, $4, 'optizmo', 'optizmo_scrub', NOW())
			 ON CONFLICT (list_id, md5_hash) DO NOTHING`,
			entryID, listID, m.Email, m.Hash)
		if err != nil {
			log.Printf("[Optizmo] error inserting suppression entry: %v", err)
			continue
		}
		inserted++
	}

	h.db.ExecContext(ctx,
		`UPDATE mailing_suppression_lists
		 SET entry_count = (SELECT COUNT(*) FROM mailing_suppression_entries WHERE list_id = $1), updated_at = NOW()
		 WHERE id = $1`, listID)

	log.Printf("[Optizmo] suppression list %q created/updated with %d entries", listName, inserted)
}

// ---------------------------------------------------------------------------
// HandleCancelScrub — POST /offer-center/offers/{id}/optizmo/cancel-scrub
// Resets a stuck scrub_pending state back to not_scrubbed and marks pending
// jobs as cancelled.
// ---------------------------------------------------------------------------

func (h *OptizmoHandlers) HandleCancelScrub(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id is required")
		return
	}

	ctx := r.Context()

	var currentStatus sql.NullString
	err := h.db.QueryRowContext(ctx,
		`SELECT optizmo_status FROM mailing_offers WHERE id = $1`, offerID,
	).Scan(&currentStatus)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "offer not found")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to query offer")
		return
	}

	status := ""
	if currentStatus.Valid {
		status = currentStatus.String
	}
	if status != "scrub_pending" && status != "scrub_failed" {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("cannot cancel scrub in state '%s'", status))
		return
	}

	h.db.ExecContext(ctx,
		`UPDATE mailing_optizmo_scrub_jobs SET status = 'cancelled', completed_at = NOW()
		 WHERE offer_id = $1 AND status IN ('pending', 'processing')`, offerID)
	h.db.ExecContext(ctx,
		`UPDATE mailing_offers SET optizmo_status = 'not_scrubbed', updated_at = NOW() WHERE id = $1`, offerID)

	log.Printf("[Optizmo] scrub cancelled for offer %s", offerID)
	respondJSON(w, http.StatusOK, map[string]string{"status": "not_scrubbed"})
}

// ---------------------------------------------------------------------------
// HandleResetScrub — POST /offer-center/offers/{id}/optizmo/reset-scrub
// Resets the offer back to not_scrubbed from ANY state (including scrubbed)
// so the scrub can be re-run. Does NOT delete offer suppressions — those
// remain in mailing_offer_suppressions until the next scrub overwrites them.
// ---------------------------------------------------------------------------

func (h *OptizmoHandlers) HandleResetScrub(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id is required")
		return
	}

	ctx := r.Context()

	var exists bool
	err := h.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM mailing_offers WHERE id = $1)`, offerID).Scan(&exists)
	if err != nil || !exists {
		respondError(w, http.StatusNotFound, "offer not found")
		return
	}

	h.db.ExecContext(ctx,
		`UPDATE mailing_optizmo_scrub_jobs SET status = 'cancelled', completed_at = NOW()
		 WHERE offer_id = $1 AND status IN ('pending', 'processing')`, offerID)
	h.db.ExecContext(ctx,
		`UPDATE mailing_offers SET optizmo_status = 'not_scrubbed', optizmo_last_scrubbed_at = NULL, updated_at = NOW()
		 WHERE id = $1`, offerID)

	log.Printf("[Optizmo] scrub reset for offer %s — status returned to not_scrubbed", offerID)
	respondJSON(w, http.StatusOK, map[string]string{"status": "not_scrubbed"})
}

// ---------------------------------------------------------------------------
// HandleImportScrubResult — POST /offer-center/offers/{id}/optizmo/import-result
// ---------------------------------------------------------------------------

func (h *OptizmoHandlers) HandleImportScrubResult(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id is required")
		return
	}

	ctx := r.Context()

	var offerName string
	h.db.QueryRowContext(ctx, `SELECT COALESCE(name,'') FROM mailing_offers WHERE id = $1`, offerID).Scan(&offerName)

	if err := r.ParseMultipartForm(64 << 20); err != nil {
		respondError(w, http.StatusBadRequest, "failed to parse multipart form: "+err.Error())
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		respondError(w, http.StatusBadRequest, "file field is required")
		return
	}
	defer file.Close()

	suppressedHashes := make(map[string]bool)
	var fileLineCount int
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			fileLineCount++
			suppressedHashes[strings.ToLower(line)] = true
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[Optizmo] error reading suppression file: %v", err)
		respondError(w, http.StatusBadRequest, "error reading uploaded file")
		return
	}

	if len(suppressedHashes) == 0 {
		respondError(w, http.StatusBadRequest, "uploaded file contains no hashes")
		return
	}

	log.Printf("[Optizmo] import for offer %s: %d file entries (%d unique)",
		offerID, fileLineCount, len(suppressedHashes))

	subRows, err := h.db.QueryContext(ctx,
		`SELECT id, LOWER(TRIM(email)) FROM mailing_subscribers
		 WHERE organization_id = $1 AND status = 'confirmed'`, defaultOrgID)
	if err != nil {
		log.Printf("[Optizmo] error querying subscribers for matching: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to query subscribers")
		return
	}
	defer subRows.Close()

	var matches []optizmoMatch
	var audienceCount int

	for subRows.Next() {
		var subID, email string
		if err := subRows.Scan(&subID, &email); err != nil {
			continue
		}
		audienceCount++
		hash := md5Hash(email)
		if suppressedHashes[hash] {
			matches = append(matches, optizmoMatch{ID: subID, Email: email, Hash: hash})
		}
	}
	if err := subRows.Err(); err != nil {
		log.Printf("[Optizmo] row iteration error during matching: %v", err)
	}

	suppressedCount := 0
	for _, m := range matches {
		_, err := h.db.ExecContext(ctx,
			`INSERT INTO mailing_offer_suppressions
				(organization_id, offer_id, subscriber_id, email_hash, reason, source, suppressed_at)
			 VALUES ($1, $2, $3, $4, 'optizmo', 'optizmo_scrub', NOW())
			 ON CONFLICT (offer_id, subscriber_id) DO NOTHING`,
			defaultOrgID, offerID, m.ID, m.Hash)
		if err != nil {
			log.Printf("[Optizmo] error inserting suppression for subscriber %s: %v", m.ID, err)
			continue
		}
		suppressedCount++
	}

	now := time.Now()

	var jobID string
	err = h.db.QueryRowContext(ctx,
		`SELECT id FROM mailing_optizmo_scrub_jobs
		 WHERE offer_id = $1 AND status IN ('pending', 'processing')
		 ORDER BY requested_at DESC LIMIT 1`, offerID,
	).Scan(&jobID)

	if err == nil && jobID != "" {
		_, err = h.db.ExecContext(ctx,
			`UPDATE mailing_optizmo_scrub_jobs
			 SET status = 'completed', file_count = $1, audience_count = $2, suppressed_count = $3, completed_at = $4
			 WHERE id = $5`,
			fileLineCount, audienceCount, suppressedCount, now, jobID)
		if err != nil {
			log.Printf("[Optizmo] error updating scrub job %s: %v", jobID, err)
		}
	}

	_, err = h.db.ExecContext(ctx,
		`UPDATE mailing_offers
		 SET optizmo_status = 'scrubbed', optizmo_last_scrubbed_at = $1, updated_at = NOW()
		 WHERE id = $2`, now, offerID)
	if err != nil {
		log.Printf("[Optizmo] error updating offer optizmo status: %v", err)
	}

	h.createSuppressionListFromScrub(ctx, offerID, offerName, matches)

	log.Printf("[Optizmo] import complete for offer %s — %d file entries, %d audience, %d suppressed",
		offerID, fileLineCount, audienceCount, suppressedCount)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"offer_id":         offerID,
		"file_count":       fileLineCount,
		"audience_scanned": audienceCount,
		"matched":          len(matches),
		"suppressed_count": suppressedCount,
		"status":           "completed",
	})
}

// ---------------------------------------------------------------------------
// HandleGetScrubStatus — GET /offer-center/offers/{id}/optizmo/status
// ---------------------------------------------------------------------------

func (h *OptizmoHandlers) HandleGetScrubStatus(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id is required")
		return
	}

	ctx := r.Context()

	// Auto-recover stuck jobs: any job pending/processing for >10min is dead
	h.db.ExecContext(ctx,
		`UPDATE mailing_optizmo_scrub_jobs
		 SET status = 'failed', error_message = 'Timed out — goroutine likely crashed or server restarted', completed_at = NOW()
		 WHERE offer_id = $1 AND status IN ('pending','processing')
		   AND requested_at < NOW() - INTERVAL '10 minutes'`, offerID)
	// If all jobs for this offer are failed/cancelled and status is still scrub_pending, reset
	var stuckPending bool
	h.db.QueryRowContext(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM mailing_offers WHERE id = $1 AND optizmo_status = 'scrub_pending'
			AND NOT EXISTS(
				SELECT 1 FROM mailing_optizmo_scrub_jobs
				WHERE offer_id = $1 AND status IN ('pending','processing')
			)
		)`, offerID).Scan(&stuckPending)
	if stuckPending {
		h.db.ExecContext(ctx,
			`UPDATE mailing_offers SET optizmo_status = 'scrub_failed', updated_at = NOW() WHERE id = $1`, offerID)
		log.Printf("[Optizmo] auto-recovered stuck scrub_pending for offer %s", offerID)
	}

	var optizmoStatus sql.NullString
	var optizmoLastScrubbedAt sql.NullTime
	err := h.db.QueryRowContext(ctx,
		`SELECT optizmo_status, optizmo_last_scrubbed_at FROM mailing_offers WHERE id = $1`,
		offerID,
	).Scan(&optizmoStatus, &optizmoLastScrubbedAt)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "offer not found")
		return
	}
	if err != nil {
		log.Printf("[Optizmo] error querying offer status: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to query offer")
		return
	}

	resp := ScrubStatusResponse{}
	if optizmoStatus.Valid {
		resp.OptizmoStatus = &optizmoStatus.String
	}
	if optizmoLastScrubbedAt.Valid {
		resp.OptizmoLastScrubbedAt = &optizmoLastScrubbedAt.Time
	}

	rows, err := h.db.QueryContext(ctx,
		`SELECT id, COALESCE(file_count,0), audience_count, suppressed_count, status,
		        COALESCE(error_message,''), requested_at, started_at, completed_at
		 FROM mailing_optizmo_scrub_jobs
		 WHERE offer_id = $1
		 ORDER BY requested_at DESC
		 LIMIT 20`, offerID)
	if err != nil {
		log.Printf("[Optizmo] error querying scrub jobs: %v", err)
		resp.Jobs = []ScrubJobResponse{}
		respondJSON(w, http.StatusOK, resp)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var j ScrubJobResponse
		var startedAt, completedAt sql.NullTime
		if err := rows.Scan(&j.ID, &j.FileCount, &j.AudienceCount, &j.SuppressedCount, &j.Status,
			&j.ErrorMessage, &j.RequestedAt, &startedAt, &completedAt); err != nil {
			log.Printf("[Optizmo] error scanning scrub job row: %v", err)
			continue
		}
		if startedAt.Valid {
			j.StartedAt = &startedAt.Time
		}
		if completedAt.Valid {
			j.CompletedAt = &completedAt.Time
		}
		resp.Jobs = append(resp.Jobs, j)
	}
	if resp.Jobs == nil {
		resp.Jobs = []ScrubJobResponse{}
	}

	respondJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// CheckOfferCompliance — campaign deployment gate
// ---------------------------------------------------------------------------

func (h *OptizmoHandlers) CheckOfferCompliance(ctx context.Context, offerID uuid.UUID) (bool, string) {
	var status sql.NullString
	err := h.db.QueryRowContext(ctx,
		`SELECT optizmo_status FROM mailing_offers WHERE id = $1`, offerID,
	).Scan(&status)
	if err != nil {
		return true, ""
	}
	if !status.Valid || status.String == "" || status.String == "not_scrubbed" {
		return false, "Optizmo compliance scrub required before deployment"
	}
	if status.String == "scrub_pending" {
		return false, "Optizmo scrub is still in progress"
	}
	if status.String == "scrub_failed" {
		return false, "Optizmo scrub failed — retry or cancel before deploying"
	}
	return true, ""
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func md5Hash(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func isHexMD5(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
