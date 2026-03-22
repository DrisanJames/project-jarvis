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
	s3Client     *SuppressionS3Client
	suppMgr      *OfferSuppressionManager
}

// NewOptizmoHandlers creates handlers with the Optizmo Mailer API token.
func NewOptizmoHandlers(db *sql.DB) *OptizmoHandlers {
	token := os.Getenv("OPTIZMO_API_TOKEN")
	if token == "" {
		token = "nOC0do1yMRfevcVXdikjTQOhOpyGPlx5"
	}
	return &OptizmoHandlers{db: db, optizmoToken: token}
}

// SetS3Client sets the S3 client for suppression file storage.
func (h *OptizmoHandlers) SetS3Client(s3Client *SuppressionS3Client) {
	h.s3Client = s3Client
}

// SetSuppressionManager sets the offer suppression manager for Bloom updates.
func (h *OptizmoHandlers) SetSuppressionManager(mgr *OfferSuppressionManager) {
	h.suppMgr = mgr
}

// updateProgress writes a progress percentage and message to the scrub job.
func (h *OptizmoHandlers) updateProgress(jobID string, pct int, msg string) {
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dbCancel()
	h.db.ExecContext(dbCtx,
		`UPDATE mailing_optizmo_scrub_jobs SET progress_pct = $1, progress_message = $2 WHERE id = $3`,
		pct, msg, jobID)
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
	ProgressPct     int        `json:"progress_pct"`
	ProgressMessage string     `json:"progress_message,omitempty"`
	S3HashKey       string     `json:"s3_hash_key,omitempty"`
	S3BloomKey      string     `json:"s3_bloom_key,omitempty"`
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// markFailed uses a fresh context so it succeeds even when the main ctx
	// has expired (the exact scenario that was causing silent "Timed out" failures).
	markFailed := func(msg string) {
		log.Printf("[Optizmo] scrub job %s FAILED: %s", jobID, msg)
		dbCtx, dbCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer dbCancel()
		h.db.ExecContext(dbCtx,
			`UPDATE mailing_optizmo_scrub_jobs
			 SET status = 'failed', error_message = $1, completed_at = NOW()
			 WHERE id = $2`, msg, jobID)
		h.db.ExecContext(dbCtx,
			`UPDATE mailing_offers SET optizmo_status = 'scrub_failed', updated_at = NOW()
			 WHERE id = $1`, offerID)
	}

	defer func() {
		if r := recover(); r != nil {
			markFailed(fmt.Sprintf("goroutine panic: %v", r))
		}
	}()

	fail := markFailed

	_, err := h.db.ExecContext(ctx,
		`UPDATE mailing_optizmo_scrub_jobs SET status = 'processing', started_at = NOW() WHERE id = $1`, jobID)
	if err != nil {
		log.Printf("[Optizmo] error setting job %s to processing: %v", jobID, err)
	}

	h.updateProgress(jobID, 5, "Downloading suppression file from Optizmo…")

	dlResult, dlErr := h.downloadOptizmoHashes(ctx, jobID, optizmoLink)
	if dlErr != nil {
		fail(fmt.Sprintf("download failed: %v", dlErr))
		return
	}
	defer os.Remove(dlResult.HashFilePath)

	h.updateProgress(jobID, 30, fmt.Sprintf("Downloaded %d entries, uploading to S3…", dlResult.FileLineCount))

	h.db.ExecContext(ctx,
		`UPDATE mailing_optizmo_scrub_jobs SET file_count = $1, valid_md5_count = $2, non_md5_count = $3 WHERE id = $4`,
		dlResult.FileLineCount, dlResult.ValidMD5Count, dlResult.NonMD5Count, jobID)

	// Upload hashes to S3 for persistent storage + Bloom rebuild
	var s3HashKey string
	if h.s3Client != nil {
		hashFile, openErr := os.Open(dlResult.HashFilePath)
		if openErr == nil {
			key, _, uploadErr := h.s3Client.UploadHashFile(ctx, offerID, hashFile)
			hashFile.Close()
			if uploadErr != nil {
				log.Printf("[Optizmo] job %s: S3 hash upload failed (non-fatal): %v", jobID, uploadErr)
			} else {
				s3HashKey = key
				h.db.ExecContext(ctx,
					`UPDATE mailing_optizmo_scrub_jobs SET s3_hash_key = $1 WHERE id = $2`, s3HashKey, jobID)
			}
		}
	}

	if dlResult.FileLineCount == 0 {
		h.db.ExecContext(ctx,
			`UPDATE mailing_optizmo_scrub_jobs
			 SET status = 'completed', audience_count = 0, suppressed_count = 0, completed_at = NOW(), progress_pct = 100, progress_message = 'Complete — empty list'
			 WHERE id = $1`, jobID)
		h.db.ExecContext(ctx,
			`UPDATE mailing_offers SET optizmo_status = 'scrubbed', optizmo_last_scrubbed_at = NOW(), updated_at = NOW()
			 WHERE id = $1`, offerID)
		log.Printf("[Optizmo] job %s: Optizmo list was empty — offer marked scrubbed (no suppressions needed)", jobID)
		return
	}

	h.updateProgress(jobID, 50, "Matching subscribers against suppression hashes…")

	audienceCount, suppressedCount, matches, matchErr := matchAndSuppressFromHashFile(ctx, h.db, offerID, dlResult.HashFilePath)
	if matchErr != nil {
		fail(fmt.Sprintf("subscriber matching failed: %v", matchErr))
		return
	}

	h.updateProgress(jobID, 80, fmt.Sprintf("Matched %d suppressions, building Bloom filter…", suppressedCount))

	h.db.ExecContext(ctx,
		`UPDATE mailing_optizmo_scrub_jobs SET audience_count = $1 WHERE id = $2`,
		audienceCount, jobID)

	// Rebuild Bloom filter — prefer local file (avoids S3 round-trip), fall back to S3 hashes
	var s3BloomKey string
	if h.suppMgr != nil {
		var bloomErr error
		bloomErr = h.suppMgr.RebuildBloomFromLocalFile(ctx, offerID, dlResult.HashFilePath)
		if bloomErr != nil && s3HashKey != "" {
			log.Printf("[Optizmo] job %s: local Bloom build failed, trying S3: %v", jobID, bloomErr)
			bloomErr = h.suppMgr.RebuildBloomFromS3Hashes(ctx, offerID)
		}
		if bloomErr != nil {
			log.Printf("[Optizmo] job %s: Bloom rebuild failed (non-fatal): %v", jobID, bloomErr)
		} else {
			if h.s3Client != nil {
				s3BloomKey = h.s3Client.bloomKey(offerID)
			}
			h.db.ExecContext(ctx,
				`UPDATE mailing_optizmo_scrub_jobs SET s3_bloom_key = $1 WHERE id = $2`, s3BloomKey, jobID)
		}
	}

	now := time.Now()
	h.db.ExecContext(ctx,
		`UPDATE mailing_optizmo_scrub_jobs
		 SET status = 'completed', suppressed_count = $1, completed_at = $2,
		     progress_pct = 100, progress_message = 'Complete'
		 WHERE id = $3`, suppressedCount, now, jobID)
	h.db.ExecContext(ctx,
		`UPDATE mailing_offers
		 SET optizmo_status = 'scrubbed', optizmo_last_scrubbed_at = $1, updated_at = NOW()
		 WHERE id = $2`, now, offerID)

	h.createSuppressionListFromScrub(ctx, offerID, offerName, matches)

	log.Printf("[Optizmo] job %s COMPLETED: %d audience, %d suppressed, s3_hash=%s, s3_bloom=%s",
		jobID, audienceCount, suppressedCount, s3HashKey, s3BloomKey)
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
		        COALESCE(progress_pct,0), COALESCE(progress_message,''),
		        COALESCE(s3_hash_key,''), COALESCE(s3_bloom_key,''),
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
			&j.ProgressPct, &j.ProgressMessage,
			&j.S3HashKey, &j.S3BloomKey,
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

// optizmoDownloadResult holds the results of downloading and parsing an
// Optizmo suppression file. Hashes are written to a temp file on disk
// (one MD5 per line) to avoid loading 10M+ entries into Go memory.
type optizmoDownloadResult struct {
	HashFilePath string // temp file with one MD5 hash per line
	FileLineCount int
	ValidMD5Count int
	NonMD5Count   int
}

// downloadOptizmoHashesPkg uses the Optizmo Mailer API (2-step flow) to download
// suppression hashes. Extracts the MAK, calls prepare-download, downloads the
// ZIP, extracts, and writes MD5 hashes to a temp file on disk. The caller is
// responsible for removing the temp file via os.Remove(result.HashFilePath).
// Peak Go memory: ~32 KB (scanner buffer + batch buffer) regardless of file size.
func downloadOptizmoHashesPkg(ctx context.Context, token, logPrefix, optizmoLink string) (*optizmoDownloadResult, error) {
	mak := extractScrubMAK(optizmoLink)
	if mak == "" {
		return nil, fmt.Errorf("could not extract MAK from Optizmo link: %s", optizmoLink)
	}

	if token == "" {
		return nil, fmt.Errorf("no Optizmo API token configured")
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
		return nil, fmt.Errorf("prepare request: %w", err)
	}
	prepReq.Header.Set("User-Agent", "IgnitePlatform/1.0 OptizmoScrub")
	prepReq.Header.Set("Accept", "application/json")

	prepResp, err := client.Do(prepReq)
	if err != nil {
		return nil, fmt.Errorf("prepare-download request: %w", err)
	}
	defer prepResp.Body.Close()

	prepBody, err := io.ReadAll(prepResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read prepare response: %w", err)
	}

	if prepResp.StatusCode != http.StatusOK && prepResp.StatusCode != http.StatusAccepted {
		return nil, fmt.Errorf("prepare-download HTTP %d: %s", prepResp.StatusCode, string(prepBody[:min(512, len(prepBody))]))
	}

	var prepResult struct {
		Result       string `json:"result"`
		DownloadLink string `json:"download_link"`
		CampaignName string `json:"campaign_name"`
		Error        string `json:"error"`
	}
	if err := json.Unmarshal(prepBody, &prepResult); err != nil {
		return nil, fmt.Errorf("parse prepare response: %w (body: %s)", err, string(prepBody[:min(256, len(prepBody))]))
	}

	if prepResult.Result == "error" {
		return nil, fmt.Errorf("Optizmo API error: %s", prepResult.Error)
	}
	if prepResult.DownloadLink == "" {
		return nil, fmt.Errorf("prepare-download returned empty download_link")
	}

	log.Printf("[Optizmo] %s: prepare OK [%s], downloading ZIP...", logPrefix, prepResult.CampaignName)

	tmpFile, err := os.CreateTemp("", "optizmo-scrub-*.zip")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
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
				return nil, fmt.Errorf("context cancelled during download polling: %w", ctx.Err())
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
			return nil, fmt.Errorf("download HTTP %d: %s", dlResp.StatusCode, string(body))
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
		return nil, fmt.Errorf("download failed after %d attempts", maxAttempts)
	}

	zipReader, err := zip.OpenReader(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	defer zipReader.Close()

	if len(zipReader.File) == 0 {
		return nil, fmt.Errorf("zip archive is empty")
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
		return nil, fmt.Errorf("open zip entry %s: %w", suppressionFile.Name, err)
	}
	defer rc.Close()

	// Write hashes to a temp file on disk instead of a Go map.
	// For a 10M-entry file this uses ~330 MB on disk but only ~32 KB in RAM.
	hashFile, err := os.CreateTemp("", "optizmo-hashes-*.txt")
	if err != nil {
		return nil, fmt.Errorf("create hash temp file: %w", err)
	}
	hashWriter := bufio.NewWriterSize(hashFile, 256*1024)

	result := &optizmoDownloadResult{HashFilePath: hashFile.Name()}
	var rawSamples []string

	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lower := strings.ToLower(line)
		result.FileLineCount++

		var hash string
		if isHexMD5(lower) {
			result.ValidMD5Count++
			hash = lower
		} else {
			result.NonMD5Count++
			hash = md5Hash(extractEmail(lower))
		}
		hashWriter.WriteString(hash)
		hashWriter.WriteByte('\n')

		if len(rawSamples) < 3 {
			rawSamples = append(rawSamples, fmt.Sprintf("%q", line))
		}
	}

	if err := hashWriter.Flush(); err != nil {
		hashFile.Close()
		os.Remove(hashFile.Name())
		return nil, fmt.Errorf("flush hash file: %w", err)
	}
	hashFile.Close()

	log.Printf("[Optizmo] %s: parsed %d entries (%d valid MD5, %d non-MD5) → %s",
		logPrefix, result.FileLineCount, result.ValidMD5Count, result.NonMD5Count, result.HashFilePath)
	log.Printf("[Optizmo] %s: RAW first entries: %s", logPrefix, strings.Join(rawSamples, " | "))

	return result, nil
}

// downloadOptizmoHashes delegates to the package-level function.
func (h *OptizmoHandlers) downloadOptizmoHashes(ctx context.Context, jobID, optizmoLink string) (*optizmoDownloadResult, error) {
	return downloadOptizmoHashesPkg(ctx, h.optizmoToken, "job "+jobID, optizmoLink)
}

// matchAndSuppressSubscribers loads suppression hashes into a Postgres temp
// table, then uses a single INSERT ... SELECT ... JOIN to match subscribers
// by MD5(email) and insert suppressions. This pushes all heavy lifting
// (hashing 600K+ emails, set intersection, bulk insert) to the database engine
// instead of pulling everything into Go memory.
//
// Returns (audienceCount, suppressedCount, matches, error).
func matchAndSuppressSubscribers(ctx context.Context, db *sql.DB, offerID string, hashes map[string]bool) (int, int, []optizmoMatch, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Step 1: Create an unlogged temp table for the suppression hashes.
	// ON COMMIT DROP ensures cleanup even if we forget.
	_, err = tx.ExecContext(ctx,
		`CREATE TEMP TABLE _optizmo_hashes (hash VARCHAR(32) NOT NULL) ON COMMIT DROP`)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("create temp table: %w", err)
	}

	// Step 2: Bulk-load hashes using batched INSERT VALUES.
	// ~500K hashes at 1000 per batch = ~500 round-trips (vs 600K+ before).
	const batchSize = 1000
	batch := make([]string, 0, batchSize)
	flushBatch := func() error {
		if len(batch) == 0 {
			return nil
		}
		query := "INSERT INTO _optizmo_hashes (hash) VALUES "
		args := make([]interface{}, len(batch))
		for i, h := range batch {
			if i > 0 {
				query += ","
			}
			query += fmt.Sprintf("($%d)", i+1)
			args[i] = h
		}
		_, err := tx.ExecContext(ctx, query, args...)
		batch = batch[:0]
		return err
	}

	for h := range hashes {
		batch = append(batch, h)
		if len(batch) >= batchSize {
			if err := flushBatch(); err != nil {
				return 0, 0, nil, fmt.Errorf("bulk insert hashes: %w", err)
			}
		}
	}
	if err := flushBatch(); err != nil {
		return 0, 0, nil, fmt.Errorf("bulk insert hashes (final): %w", err)
	}

	// Index the temp table for the join.
	_, err = tx.ExecContext(ctx, `CREATE INDEX ON _optizmo_hashes (hash)`)
	if err != nil {
		log.Printf("[Optizmo] warning: could not index temp table: %v", err)
	}

	// Step 3: Get audience count (fast COUNT).
	var audienceCount int
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_subscribers
		 WHERE organization_id = $1 AND status = 'confirmed'`, defaultOrgID,
	).Scan(&audienceCount)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("count audience: %w", err)
	}

	// Step 4: Single INSERT ... SELECT to match and suppress in one pass.
	// Postgres computes MD5(LOWER(TRIM(email))) for each subscriber and JOINs
	// against the temp table — no data leaves the database.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO mailing_offer_suppressions
			(organization_id, offer_id, subscriber_id, email_hash, reason, source, suppressed_at)
		 SELECT $1, $2, s.id, MD5(LOWER(TRIM(s.email))), 'optizmo', 'optizmo_scrub', NOW()
		 FROM mailing_subscribers s
		 INNER JOIN _optizmo_hashes h ON h.hash = MD5(LOWER(TRIM(s.email)))
		 WHERE s.organization_id = $1 AND s.status = 'confirmed'
		 ON CONFLICT (offer_id, subscriber_id) DO NOTHING`,
		defaultOrgID, offerID)
	if err != nil {
		return audienceCount, 0, nil, fmt.Errorf("bulk suppress: %w", err)
	}
	suppressedCount, _ := res.RowsAffected()

	// Step 5: Retrieve the match list for named suppression lists.
	var matches []optizmoMatch
	matchRows, err := tx.QueryContext(ctx,
		`SELECT s.id, LOWER(TRIM(s.email)), MD5(LOWER(TRIM(s.email)))
		 FROM mailing_subscribers s
		 INNER JOIN _optizmo_hashes h ON h.hash = MD5(LOWER(TRIM(s.email)))
		 WHERE s.organization_id = $1 AND s.status = 'confirmed'`,
		defaultOrgID)
	if err != nil {
		log.Printf("[Optizmo] warning: could not retrieve match details: %v", err)
	} else {
		defer matchRows.Close()
		for matchRows.Next() {
			var m optizmoMatch
			if err := matchRows.Scan(&m.ID, &m.Email, &m.Hash); err != nil {
				continue
			}
			matches = append(matches, m)
		}
	}

	if err := tx.Commit(); err != nil {
		return audienceCount, 0, nil, fmt.Errorf("commit tx: %w", err)
	}

	log.Printf("[Optizmo] DB-side matching: %d audience, %d suppressed (temp table with %d hashes)",
		audienceCount, suppressedCount, len(hashes))

	return audienceCount, int(suppressedCount), matches, nil
}

// matchAndSuppressFromHashFile is the streaming variant of matchAndSuppressSubscribers.
// Instead of accepting a Go map, it reads hashes from a file on disk (one MD5
// per line) and streams them into a Postgres temp table in batches. Peak Go
// memory: ~32 KB regardless of file size. This is critical for large Optizmo
// files (10M+ entries, 327 MB) that would OOM a 1 GB container if loaded into
// a Go map.
func matchAndSuppressFromHashFile(ctx context.Context, db *sql.DB, offerID, hashFilePath string) (int, int, []optizmoMatch, error) {
	f, err := os.Open(hashFilePath)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("open hash file: %w", err)
	}
	defer f.Close()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		`CREATE TEMP TABLE _optizmo_hashes (hash VARCHAR(32) NOT NULL) ON COMMIT DROP`)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("create temp table: %w", err)
	}

	// Stream from file into temp table, 1000 hashes per batch INSERT.
	const batchSize = 1000
	batch := make([]string, 0, batchSize)
	var hashCount int

	flushBatch := func() error {
		if len(batch) == 0 {
			return nil
		}
		query := "INSERT INTO _optizmo_hashes (hash) VALUES "
		args := make([]interface{}, len(batch))
		for i, h := range batch {
			if i > 0 {
				query += ","
			}
			query += fmt.Sprintf("($%d)", i+1)
			args[i] = h
		}
		_, err := tx.ExecContext(ctx, query, args...)
		batch = batch[:0]
		return err
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		h := strings.TrimSpace(scanner.Text())
		if h == "" {
			continue
		}
		batch = append(batch, h)
		hashCount++
		if len(batch) >= batchSize {
			if err := flushBatch(); err != nil {
				return 0, 0, nil, fmt.Errorf("bulk insert hashes: %w", err)
			}
		}
	}
	if err := flushBatch(); err != nil {
		return 0, 0, nil, fmt.Errorf("bulk insert hashes (final): %w", err)
	}

	log.Printf("[Optizmo] streamed %d hashes into temp table from %s", hashCount, hashFilePath)

	_, err = tx.ExecContext(ctx, `CREATE INDEX ON _optizmo_hashes (hash)`)
	if err != nil {
		log.Printf("[Optizmo] warning: could not index temp table: %v", err)
	}

	var audienceCount int
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_subscribers
		 WHERE organization_id = $1 AND status = 'confirmed'`, defaultOrgID,
	).Scan(&audienceCount)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("count audience: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO mailing_offer_suppressions
			(organization_id, offer_id, subscriber_id, email_hash, reason, source, suppressed_at)
		 SELECT $1, $2, s.id, MD5(LOWER(TRIM(s.email))), 'optizmo', 'optizmo_scrub', NOW()
		 FROM mailing_subscribers s
		 INNER JOIN _optizmo_hashes h ON h.hash = MD5(LOWER(TRIM(s.email)))
		 WHERE s.organization_id = $1 AND s.status = 'confirmed'
		 ON CONFLICT (offer_id, subscriber_id) DO NOTHING`,
		defaultOrgID, offerID)
	if err != nil {
		return audienceCount, 0, nil, fmt.Errorf("bulk suppress: %w", err)
	}
	suppressedCount, _ := res.RowsAffected()

	var matches []optizmoMatch
	matchRows, err := tx.QueryContext(ctx,
		`SELECT s.id, LOWER(TRIM(s.email)), MD5(LOWER(TRIM(s.email)))
		 FROM mailing_subscribers s
		 INNER JOIN _optizmo_hashes h ON h.hash = MD5(LOWER(TRIM(s.email)))
		 WHERE s.organization_id = $1 AND s.status = 'confirmed'`,
		defaultOrgID)
	if err != nil {
		log.Printf("[Optizmo] warning: could not retrieve match details: %v", err)
	} else {
		defer matchRows.Close()
		for matchRows.Next() {
			var m optizmoMatch
			if err := matchRows.Scan(&m.ID, &m.Email, &m.Hash); err != nil {
				continue
			}
			matches = append(matches, m)
		}
	}

	if err := tx.Commit(); err != nil {
		return audienceCount, 0, nil, fmt.Errorf("commit tx: %w", err)
	}

	log.Printf("[Optizmo] DB-side matching (streamed): %d audience, %d suppressed (%d hashes from file)",
		audienceCount, suppressedCount, hashCount)

	return audienceCount, int(suppressedCount), matches, nil
}

// extractScrubMAK extracts the MAK from various Optizmo URL formats:
//   - https://app.optizmo.com/access/campaigns?mak=m-xxx-yyy-zzz
//   - https://www.affiliateaccesskey.com/m-xxx-yyy-zzz
//   - https://www.affiliateaccesskey.com/sm-xxx-yyy (sm- prefix variant)
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
		if strings.HasPrefix(segment, "m-") || strings.HasPrefix(segment, "sm-") {
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
