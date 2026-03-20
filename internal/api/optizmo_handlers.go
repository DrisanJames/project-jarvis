package api

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// OptizmoHandlers provides HTTP handlers for Optizmo list scrub management.
type OptizmoHandlers struct {
	db           *sql.DB
	optizmoToken string
}

// NewOptizmoHandlers creates handlers with the Optizmo Mailer API token.
func NewOptizmoHandlers(db *sql.DB) *OptizmoHandlers {
	token := os.Getenv("OPTIZMO_API_TOKEN")
	if token == "" {
		token = "nOC0do1yMRfevcVXdikjTQOhOpyGPlx5"
	}
	return &OptizmoHandlers{db: db, optizmoToken: token}
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
	ValidMD5Count   int        `json:"valid_md5_count"`
	NonMD5Count     int        `json:"non_md5_count"`
	AudienceCount   int        `json:"audience_count"`
	SuppressedCount int        `json:"suppressed_count"`
	Status          string     `json:"status"`
	ErrorMessage    string     `json:"error_message,omitempty"`
	ScrubType       string     `json:"scrub_type"`
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

	hashes, fileLineCount, validMD5Count, nonMD5Count, dlErr := h.downloadOptizmoHashes(ctx, jobID, optizmoLink)
	if dlErr != nil {
		fail(fmt.Sprintf("download failed: %v", dlErr))
		return
	}
	suppressedHashes := hashes

	h.db.ExecContext(ctx,
		`UPDATE mailing_optizmo_scrub_jobs SET file_count = $1, valid_md5_count = $2, non_md5_count = $3 WHERE id = $4`,
		fileLineCount, validMD5Count, nonMD5Count, jobID)

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

	audienceCount, suppressedCount, matches, matchErr := matchAndSuppressSubscribers(ctx, h.db, offerID, suppressedHashes)
	if matchErr != nil {
		fail(fmt.Sprintf("subscriber matching failed: %v", matchErr))
		return
	}

	h.db.ExecContext(ctx,
		`UPDATE mailing_optizmo_scrub_jobs SET audience_count = $1 WHERE id = $2`,
		audienceCount, jobID)

	now := time.Now()
	h.db.ExecContext(ctx,
		`UPDATE mailing_optizmo_scrub_jobs
		 SET status = 'completed', suppressed_count = $1, completed_at = $2
		 WHERE id = $3`, suppressedCount, now, jobID)
	h.db.ExecContext(ctx,
		`UPDATE mailing_offers
		 SET optizmo_status = 'scrubbed', optizmo_last_scrubbed_at = $1, updated_at = NOW()
		 WHERE id = $2`, now, offerID)

	h.createSuppressionListFromScrub(ctx, offerID, offerName, matches)

	log.Printf("[Optizmo] job %s COMPLETED: %d audience, %d suppressed",
		jobID, audienceCount, suppressedCount)
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
	var fileLineCount, validMD5Count, nonMD5Count int
	var rawSamples []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		fileLineCount++
		if isHexMD5(lower) {
			validMD5Count++
			suppressedHashes[lower] = true
		} else {
			nonMD5Count++
			cleaned := extractEmail(lower)
			suppressedHashes[md5Hash(cleaned)] = true
		}
		if len(rawSamples) < 3 {
			rawSamples = append(rawSamples, fmt.Sprintf("%q", line))
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

	log.Printf("[Optizmo] import for offer %s: %d file entries (%d unique hashes, %d valid MD5, %d non-MD5 — plaintext hashed to MD5)",
		offerID, fileLineCount, len(suppressedHashes), validMD5Count, nonMD5Count)
	log.Printf("[Optizmo] import RAW first entries: %s", strings.Join(rawSamples, " | "))

	audienceCount, suppressedCount, _, matchErr := matchAndSuppressSubscribers(ctx, h.db, offerID, suppressedHashes)
	if matchErr != nil {
		log.Printf("[Optizmo] error during subscriber matching: %v", matchErr)
		respondError(w, http.StatusInternalServerError, "failed to match subscribers")
		return
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
			 SET status = 'completed', file_count = $1, audience_count = $2, suppressed_count = $3,
			     valid_md5_count = $4, non_md5_count = $5, completed_at = $6
			 WHERE id = $7`,
			fileLineCount, audienceCount, suppressedCount, validMD5Count, nonMD5Count, now, jobID)
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

	log.Printf("[Optizmo] import complete for offer %s — %d file entries, %d audience, %d suppressed",
		offerID, fileLineCount, audienceCount, suppressedCount)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"offer_id":         offerID,
		"file_count":       fileLineCount,
		"audience_scanned": audienceCount,
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
		`SELECT id, COALESCE(file_count,0), COALESCE(valid_md5_count,0), COALESCE(non_md5_count,0),
		        audience_count, suppressed_count, status,
		        COALESCE(error_message,''), COALESCE(scrub_type,'manual'),
		        requested_at, started_at, completed_at
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
		if err := rows.Scan(&j.ID, &j.FileCount, &j.ValidMD5Count, &j.NonMD5Count,
			&j.AudienceCount, &j.SuppressedCount, &j.Status,
			&j.ErrorMessage, &j.ScrubType,
			&j.RequestedAt, &startedAt, &completedAt); err != nil {
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
// Optizmo Mailer API Download (package-level, shared by handlers and workers)
// ---------------------------------------------------------------------------

// downloadOptizmoHashesPkg uses the Optizmo Mailer API (2-step flow) to download
// suppression hashes. Extracts the MAK from the offer's optizmo_link, calls
// the prepare-download endpoint to get a ZIP download link, downloads and
// extracts the ZIP, then returns MD5 hashes.
// This is a package-level function shared by OptizmoHandlers and OptizmoDeltaSyncWorker.
func downloadOptizmoHashesPkg(ctx context.Context, token, logPrefix, optizmoLink string) (map[string]bool, int, int, int, error) {
	mak := extractScrubMAK(optizmoLink)
	if mak == "" {
		return nil, 0, 0, 0, fmt.Errorf("could not extract MAK from Optizmo link: %s", optizmoLink)
	}

	if token == "" {
		return nil, 0, 0, 0, fmt.Errorf("no Optizmo API token configured")
	}

	log.Printf("[Optizmo] %s: using Mailer API flow (mak=%s...)", logPrefix, mak[:min(12, len(mak))])

	prepareURL := fmt.Sprintf("%s/accesskey/download/%s?token=%s&format=md5",
		optizmoMailerAPIBase,
		url.PathEscape(mak),
		url.QueryEscape(token),
	)

	client := &http.Client{Timeout: 3 * time.Minute}
	prepReq, err := http.NewRequestWithContext(ctx, http.MethodGet, prepareURL, nil)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("prepare request: %w", err)
	}
	prepReq.Header.Set("User-Agent", "IgnitePlatform/1.0 OptizmoScrub")
	prepReq.Header.Set("Accept", "application/json")

	prepResp, err := client.Do(prepReq)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("prepare-download request: %w", err)
	}
	defer prepResp.Body.Close()

	prepBody, err := io.ReadAll(prepResp.Body)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("read prepare response: %w", err)
	}

	if prepResp.StatusCode != http.StatusOK && prepResp.StatusCode != http.StatusAccepted {
		return nil, 0, 0, 0, fmt.Errorf("prepare-download HTTP %d: %s", prepResp.StatusCode, string(prepBody[:min(512, len(prepBody))]))
	}

	var prepResult struct {
		Result       string `json:"result"`
		DownloadLink string `json:"download_link"`
		CampaignName string `json:"campaign_name"`
		Error        string `json:"error"`
	}
	if err := json.Unmarshal(prepBody, &prepResult); err != nil {
		return nil, 0, 0, 0, fmt.Errorf("parse prepare response: %w (body: %s)", err, string(prepBody[:min(256, len(prepBody))]))
	}

	if prepResult.Result == "error" {
		return nil, 0, 0, 0, fmt.Errorf("Optizmo API error: %s", prepResult.Error)
	}
	if prepResult.DownloadLink == "" {
		return nil, 0, 0, 0, fmt.Errorf("prepare-download returned empty download_link")
	}

	log.Printf("[Optizmo] %s: prepare OK [%s], downloading ZIP...", logPrefix, prepResult.CampaignName)

	tmpFile, err := os.CreateTemp("", "optizmo-scrub-*.zip")
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	var downloadSuccess bool
	maxAttempts := 20
	pollInterval := 5 * time.Second

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-time.After(pollInterval):
			case <-ctx.Done():
				tmpFile.Close()
				return nil, 0, 0, 0, fmt.Errorf("context cancelled during download polling: %w", ctx.Err())
			}
			if pollInterval < 30*time.Second {
				pollInterval += 5 * time.Second
			}
		}

		dlClient := &http.Client{Timeout: 15 * time.Minute}
		dlReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, prepResult.DownloadLink, nil)
		dlReq.Header.Set("User-Agent", "IgnitePlatform/1.0 OptizmoScrub")

		dlResp, dlErr := dlClient.Do(dlReq)
		if dlErr != nil {
			log.Printf("[Optizmo] %s: download attempt %d/%d failed: %v", logPrefix, attempt, maxAttempts, dlErr)
			continue
		}

		if dlResp.StatusCode == http.StatusNotFound {
			dlResp.Body.Close()
			log.Printf("[Optizmo] %s: download attempt %d/%d: 404 (file not ready)", logPrefix, attempt, maxAttempts)
			continue
		}

		if dlResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(dlResp.Body, 512))
			dlResp.Body.Close()
			return nil, 0, 0, 0, fmt.Errorf("download HTTP %d: %s", dlResp.StatusCode, string(body))
		}

		tmpFile.Seek(0, 0)
		tmpFile.Truncate(0)
		written, copyErr := io.Copy(tmpFile, dlResp.Body)
		dlResp.Body.Close()
		if copyErr != nil {
			log.Printf("[Optizmo] %s: download stream error: %v (got %d bytes)", logPrefix, copyErr, written)
			continue
		}

		log.Printf("[Optizmo] %s: downloaded %d bytes (%.1f MB)", logPrefix, written, float64(written)/1024/1024)
		downloadSuccess = true
		break
	}

	tmpFile.Close()

	if !downloadSuccess {
		return nil, 0, 0, 0, fmt.Errorf("download failed after %d attempts", maxAttempts)
	}

	zipReader, err := zip.OpenReader(tmpPath)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("open zip: %w", err)
	}
	defer zipReader.Close()

	if len(zipReader.File) == 0 {
		return nil, 0, 0, 0, fmt.Errorf("zip archive is empty")
	}

	var suppressionFile *zip.File
	for _, f := range zipReader.File {
		lower := strings.ToLower(f.Name)
		if strings.Contains(lower, "suppression_list") || strings.Contains(lower, "optout") {
			suppressionFile = f
			break
		}
	}
	if suppressionFile == nil {
		suppressionFile = zipReader.File[0]
		for _, f := range zipReader.File[1:] {
			if f.UncompressedSize64 > suppressionFile.UncompressedSize64 {
				suppressionFile = f
			}
		}
	}

	log.Printf("[Optizmo] %s: extracting %s (%d MB uncompressed)", logPrefix, suppressionFile.Name, suppressionFile.UncompressedSize64/1024/1024)

	rc, err := suppressionFile.Open()
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("open zip entry %s: %w", suppressionFile.Name, err)
	}
	defer rc.Close()

	hashes := make(map[string]bool)
	var fileLineCount, validMD5Count, nonMD5Count int
	var rawSamples []string

	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lower := strings.ToLower(line)
		fileLineCount++
		if isHexMD5(lower) {
			validMD5Count++
			hashes[lower] = true
		} else {
			nonMD5Count++
			cleaned := extractEmail(lower)
			hashes[md5Hash(cleaned)] = true
		}
		if len(rawSamples) < 3 {
			rawSamples = append(rawSamples, fmt.Sprintf("%q", line))
		}
	}

	log.Printf("[Optizmo] %s: parsed %d entries (%d valid MD5, %d non-MD5)", logPrefix, fileLineCount, validMD5Count, nonMD5Count)
	log.Printf("[Optizmo] %s: RAW first entries: %s", logPrefix, strings.Join(rawSamples, " | "))

	return hashes, fileLineCount, validMD5Count, nonMD5Count, nil
}

// downloadOptizmoHashes delegates to the package-level function.
func (h *OptizmoHandlers) downloadOptizmoHashes(ctx context.Context, jobID, optizmoLink string) (map[string]bool, int, int, int, error) {
	return downloadOptizmoHashesPkg(ctx, h.optizmoToken, "job "+jobID, optizmoLink)
}

// matchAndSuppressSubscribers scans all confirmed subscribers, matches their
// email MD5 against the provided hash set, and inserts offer-level suppressions.
// Returns (audienceCount, suppressedCount, matches, error).
func matchAndSuppressSubscribers(ctx context.Context, db *sql.DB, offerID string, hashes map[string]bool) (int, int, []optizmoMatch, error) {
	subRows, err := db.QueryContext(ctx,
		`SELECT id, LOWER(TRIM(email)) FROM mailing_subscribers
		 WHERE organization_id = $1 AND status = 'confirmed'`, defaultOrgID)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("failed to query subscribers: %w", err)
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
		if hashes[emailHash] {
			matches = append(matches, optizmoMatch{ID: subID, Email: email, Hash: emailHash})
		}
	}

	suppressedCount := 0
	for _, m := range matches {
		_, err := db.ExecContext(ctx,
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

	return audienceCount, suppressedCount, matches, nil
}

// extractScrubMAK extracts the MAK from various Optizmo URL formats:
//   - https://app.optizmo.com/access/campaigns?mak=m-xxx-yyy-zzz
//   - https://www.affiliateaccesskey.com/m-xxx-yyy-zzz
func extractScrubMAK(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	if mak := parsed.Query().Get("mak"); mak != "" {
		return mak
	}
	if idx := strings.Index(rawURL, "mak="); idx >= 0 {
		rest := rawURL[idx+4:]
		if amp := strings.IndexByte(rest, '&'); amp >= 0 {
			return rest[:amp]
		}
		return rest
	}
	path := strings.TrimRight(parsed.Path, "/")
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		segment := path[idx+1:]
		if strings.HasPrefix(segment, "m-") {
			return segment
		}
	}
	return ""
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

// extractEmail extracts the email address from a line that may contain
// CSV columns, quotes, or other formatting. Handles common Optizmo formats:
//   - plain email: user@example.com
//   - quoted: "user@example.com"
//   - CSV with email as first column: user@example.com,other,data
//   - CSV with header-style: email,user@example.com
//   - tab-delimited: user@example.com\tother
func extractEmail(line string) string {
	line = strings.Trim(line, "\"'")

	for _, sep := range []string{",", "\t", "|", ";"} {
		parts := strings.Split(line, sep)
		for _, p := range parts {
			p = strings.TrimSpace(strings.Trim(p, "\"'"))
			if strings.Contains(p, "@") && strings.Contains(p, ".") {
				return strings.ToLower(p)
			}
		}
	}

	return strings.ToLower(strings.TrimSpace(line))
}
