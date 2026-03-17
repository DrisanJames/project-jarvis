package api

import (
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
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const optizmoScrubDir = "/tmp/optizmo-scrubs"

func optizmoWorkerURL() string {
	if v := os.Getenv("OPTIZMO_WORKER_URL"); v != "" {
		return v
	}
	return "http://optizmo-worker.ignite.local:8090"
}

// OptizmoHandlers provides HTTP handlers for Optizmo list scrub management.
type OptizmoHandlers struct {
	db *sql.DB
}

// ---------------------------------------------------------------------------
// Response types
// ---------------------------------------------------------------------------

type ScrubJobResponse struct {
	ID              string     `json:"id"`
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
	err := h.db.QueryRowContext(ctx,
		`SELECT TRUE, optizmo_link FROM mailing_offers WHERE id = $1`, offerID,
	).Scan(&offerExists, &optizmoLink)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "offer not found")
		return
	}
	if err != nil {
		log.Printf("[Optizmo] error checking offer %s: %v", offerID, err)
		respondError(w, http.StatusInternalServerError, "failed to verify offer")
		return
	}

	rows, err := h.db.QueryContext(ctx,
		`SELECT LOWER(TRIM(email)) FROM mailing_subscribers
		 WHERE organization_id = $1 AND status = 'confirmed'`, defaultOrgID)
	if err != nil {
		log.Printf("[Optizmo] error querying subscribers: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to query subscribers")
		return
	}
	defer rows.Close()

	if err := os.MkdirAll(optizmoScrubDir, 0755); err != nil {
		log.Printf("[Optizmo] error creating scrub directory: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to create scrub directory")
		return
	}

	jobID := uuid.New().String()
	filePath := fmt.Sprintf("%s/%s.csv", optizmoScrubDir, jobID)

	f, err := os.Create(filePath)
	if err != nil {
		log.Printf("[Optizmo] error creating audience file %s: %v", filePath, err)
		respondError(w, http.StatusInternalServerError, "failed to create audience file")
		return
	}
	defer f.Close()

	writer := bufio.NewWriter(f)
	var audienceCount int

	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			log.Printf("[Optizmo] error scanning subscriber row: %v", err)
			continue
		}
		hash := md5Hash(email)
		fmt.Fprintln(writer, hash)
		audienceCount++
	}
	if err := rows.Err(); err != nil {
		log.Printf("[Optizmo] row iteration error: %v", err)
	}
	if err := writer.Flush(); err != nil {
		log.Printf("[Optizmo] error flushing audience file: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to write audience file")
		return
	}

	now := time.Now()
	_, err = h.db.ExecContext(ctx,
		`INSERT INTO mailing_optizmo_scrub_jobs
			(id, offer_id, audience_file_path, audience_count, status, requested_at)
		 VALUES ($1, $2, $3, $4, 'pending', $5)`,
		jobID, offerID, filePath, audienceCount, now)
	if err != nil {
		log.Printf("[Optizmo] error creating scrub job: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to create scrub job")
		return
	}

	_, err = h.db.ExecContext(ctx,
		`UPDATE mailing_offers SET optizmo_status = 'scrub_pending', updated_at = NOW() WHERE id = $1`,
		offerID)
	if err != nil {
		log.Printf("[Optizmo] error updating offer status: %v", err)
	}

	link := ""
	if optizmoLink.Valid {
		link = optizmoLink.String
	}
	go fireSidecarTrigger(jobID, offerID, filePath, link)

	log.Printf("[Optizmo] scrub requested for offer %s — job %s, %d subscribers", offerID, jobID, audienceCount)

	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"job_id":         jobID,
		"audience_count": audienceCount,
		"audience_file":  filePath,
		"status":         "pending",
	})
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
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
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

	subRows, err := h.db.QueryContext(ctx,
		`SELECT id, LOWER(TRIM(email)) FROM mailing_subscribers
		 WHERE organization_id = $1 AND status = 'confirmed'`, defaultOrgID)
	if err != nil {
		log.Printf("[Optizmo] error querying subscribers for matching: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to query subscribers")
		return
	}
	defer subRows.Close()

	type matchedSub struct {
		ID    string
		Email string
	}
	var matches []matchedSub

	for subRows.Next() {
		var subID, email string
		if err := subRows.Scan(&subID, &email); err != nil {
			continue
		}
		hash := md5Hash(email)
		if suppressedHashes[hash] {
			matches = append(matches, matchedSub{ID: subID, Email: email})
		}
	}
	if err := subRows.Err(); err != nil {
		log.Printf("[Optizmo] row iteration error during matching: %v", err)
	}

	suppressedCount := 0
	for _, m := range matches {
		emailHash := md5Hash(m.Email)
		_, err := h.db.ExecContext(ctx,
			`INSERT INTO mailing_offer_suppressions
				(organization_id, offer_id, subscriber_id, email_hash, reason, source, suppressed_at)
			 VALUES ($1, $2, $3, $4, 'optizmo', 'optizmo_scrub', NOW())
			 ON CONFLICT (offer_id, subscriber_id) DO NOTHING`,
			defaultOrgID, offerID, m.ID, emailHash)
		if err != nil {
			log.Printf("[Optizmo] error inserting suppression for subscriber %s: %v", m.ID, err)
			continue
		}
		suppressedCount++
	}

	now := time.Now()

	// Find the most recent pending/processing job for this offer to update
	var jobID string
	err = h.db.QueryRowContext(ctx,
		`SELECT id FROM mailing_optizmo_scrub_jobs
		 WHERE offer_id = $1 AND status IN ('pending', 'processing')
		 ORDER BY requested_at DESC LIMIT 1`, offerID,
	).Scan(&jobID)

	if err == nil && jobID != "" {
		_, err = h.db.ExecContext(ctx,
			`UPDATE mailing_optizmo_scrub_jobs
			 SET status = 'completed', suppressed_count = $1, completed_at = $2
			 WHERE id = $3`,
			suppressedCount, now, jobID)
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

	log.Printf("[Optizmo] import complete for offer %s — %d suppressed out of %d hashes",
		offerID, suppressedCount, len(suppressedHashes))

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"offer_id":         offerID,
		"hashes_uploaded":  len(suppressedHashes),
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
		`SELECT id, audience_count, suppressed_count, status, COALESCE(error_message,''),
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
		if err := rows.Scan(&j.ID, &j.AudienceCount, &j.SuppressedCount, &j.Status,
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
	return true, ""
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func md5Hash(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func fireSidecarTrigger(jobID, offerID, audienceFile, optizmoLink string) {
	payload := map[string]string{
		"job_id":        jobID,
		"offer_id":      offerID,
		"audience_file": audienceFile,
		"optizmo_link":  optizmoLink,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[Optizmo] error marshaling sidecar payload: %v", err)
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(optizmoWorkerURL()+"/scrub", "application/json",
		strings.NewReader(string(body)))
	if err != nil {
		log.Printf("[Optizmo] sidecar trigger failed (non-blocking): %v", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	log.Printf("[Optizmo] sidecar trigger sent for job %s — status %d", jobID, resp.StatusCode)
}
