package api

// =============================================================================
// EO CLEANING SERVICE — job intake + progress (/api/mailing/eo-clean/*)
// =============================================================================
// Operator ask (2026-07-26): "Somewhere in the platform I should be able to
// upload a list and have it cleaned. Or tell the platform to clean a
// particular list or segment from the segment command." This service is the
// intake half: it lands ad-hoc EmailOversight cleaning jobs in
// mailing_eo_clean_jobs + mailing_eo_clean_items; the drain half is
// internal/worker/eo_clean_jobs.go (EOCleanJobWorker), which validates via the
// SAME EO transport as the PartnerValidator and lands verdicts in
// mailing_eo_validation (the gate every fresh draw reads).
//
// Sources:
//   upload  — JSON emails[] (paste box today; multipart CSV is a noted
//             follow-up, not built here). Malformed entries REFUSE the whole
//             request (the eo_tranche_landing.py fail-closed idiom — a silent
//             skip would strand rows with no signal).
//   segment — source_ref = mailing_segments.id (org-verified). Emails resolve
//             server-side from mailing_segment_members.email (the members
//             table carries its own email column — never join subscribers for
//             this; see the ambiguous-email footgun).
//   list    — source_ref = mailing_lists.id (org-verified). Emails resolve via
//             mailing_list_subscribers → mailing_subscribers.
//
// NEVER PAY TWICE: enqueue skips emails already status='Verified' in
// mailing_eo_validation; they are counted as already_clean. Cleaning costs
// money per record — the UI quotes this before POSTing.
//
// Bounds: uploads ≤ eoCleanMaxUploadEmails per request; segment/list sources
// ≤ eoCleanMaxJobSize resolved emails (reject larger — split the source).
// The whole enqueue runs in ONE transaction under a SET LOCAL
// statement_timeout, so a too-slow resolve fails cleanly with no partial job.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// VersionEOCleanAPI is bumped on every response-shape change.
const VersionEOCleanAPI = "1.0"

const (
	eoCleanMaxUploadEmails = 200000  // per POST (JSON array)
	eoCleanMaxJobSize      = 500000  // resolved emails per segment/list job
	eoCleanUploadChunk     = 10000   // unnest batch size
	eoCleanEnqueueTimeout  = "25s"   // SET LOCAL statement_timeout for the enqueue tx
	eoCleanListLimit       = 100     // jobs returned by the list endpoint
	eoCleanFailSampleLimit = 20      // failed-item sample on GET /{id}
)

// eoCleanJobStatuses — job lifecycle (worker: queued|running → done; operator:
// pause/resume). 'failed' is reserved for future enqueue-side terminalization.
var eoCleanPausableStatuses = map[string]bool{"queued": true, "running": true}

// eoCleanPauseTransition validates a pause/resume request against the current
// status. Pure (unit-tested). Returns the next status and whether the
// transition is legal.
func eoCleanPauseTransition(current, action string) (string, bool) {
	switch action {
	case "pause":
		if eoCleanPausableStatuses[current] {
			return "paused", true
		}
	case "resume":
		if current == "paused" {
			return "queued", true
		}
	}
	return current, false
}

// normalizeEOCleanEmails lowercases/trims and dedupes (order-preserving) an
// upload emails array. Malformed entries (no interior '@') REFUSE the whole
// request — returned with their positions so the operator can fix the paste.
func normalizeEOCleanEmails(raw []string) (clean []string, bad []string) {
	seen := make(map[string]bool, len(raw))
	for i, r := range raw {
		e := strings.ToLower(strings.TrimSpace(r))
		if e == "" {
			continue // blank lines in a paste box are noise, not errors
		}
		if !strings.Contains(e, "@") || strings.HasPrefix(e, "@") || strings.HasSuffix(e, "@") {
			bad = append(bad, fmt.Sprintf("entry %d: %q", i+1, r))
			continue
		}
		if !seen[e] {
			seen[e] = true
			clean = append(clean, e)
		}
	}
	return clean, bad
}

// ── JSON shapes ─────────────────────────────────────────────────────────────

type eoCleanJobJSON struct {
	ID                 string  `json:"id"`
	SourceType         string  `json:"source_type"`
	SourceRef          string  `json:"source_ref"`
	Label              string  `json:"label"`
	Status             string  `json:"status"`
	TotalCount         int64   `json:"total_count"`
	AlreadyCleanCount  int64   `json:"already_clean_count"`
	QueuedCount        int64   `json:"queued_count"`
	ValidatedCount     int64   `json:"validated_count"`
	VerifiedCount      int64   `json:"verified_count"`
	ComplainerCount    int64   `json:"complainer_count"`
	UndeliverableCount int64   `json:"undeliverable_count"`
	OtherCount         int64   `json:"other_count"`
	FailedCount        int64   `json:"failed_count"`
	DailyCap           int     `json:"daily_cap"`
	CreatedBy          string  `json:"created_by"`
	CreatedAt          string  `json:"created_at"`
	FinishedAt         *string `json:"finished_at"`
}

type eoCleanFailedItemJSON struct {
	EmailLower string `json:"email_lower"`
	Result     string `json:"result"`
}

type eoCleanCreateRequest struct {
	SourceType string   `json:"source_type"` // upload | segment | list
	SourceRef  string   `json:"source_ref"`  // segment/list id (uuid)
	Emails     []string `json:"emails"`      // upload source
	Label      string   `json:"label"`
	DailyCap   int      `json:"daily_cap"` // 0 = uncapped (global budget still applies)
	CreatedBy  string   `json:"created_by"`
}

// ── Store ───────────────────────────────────────────────────────────────────

// EOCleanStore is the data-access layer (no raw SQL in handlers).
type EOCleanStore struct {
	db *sql.DB
}

func NewEOCleanStore(db *sql.DB) *EOCleanStore {
	return &EOCleanStore{db: db}
}

const eoCleanJobColumns = `id::text, source_type, source_ref, label, status,
	total_count, already_clean_count, queued_count, validated_count,
	verified_count, complainer_count, undeliverable_count, other_count,
	failed_count, daily_cap, created_by, created_at, finished_at`

func scanEOCleanJob(scan func(dest ...interface{}) error) (eoCleanJobJSON, error) {
	var j eoCleanJobJSON
	var createdAt time.Time
	var finishedAt sql.NullTime
	err := scan(&j.ID, &j.SourceType, &j.SourceRef, &j.Label, &j.Status,
		&j.TotalCount, &j.AlreadyCleanCount, &j.QueuedCount, &j.ValidatedCount,
		&j.VerifiedCount, &j.ComplainerCount, &j.UndeliverableCount, &j.OtherCount,
		&j.FailedCount, &j.DailyCap, &j.CreatedBy, &createdAt, &finishedAt)
	if err != nil {
		return j, err
	}
	j.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	if finishedAt.Valid {
		t := finishedAt.Time.UTC().Format(time.RFC3339)
		j.FinishedAt = &t
	}
	return j, nil
}

// CreateJob runs the whole enqueue in one tx: insert the job row, resolve +
// insert items (skipping already-Verified), stamp the counters. A job whose
// source resolves to 0 emails is rejected (tx rolls back — no empty jobs).
func (s *EOCleanStore) CreateJob(r *http.Request, orgID uuid.UUID, req eoCleanCreateRequest, uploadEmails []string) (eoCleanJobJSON, int, error) {
	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return eoCleanJobJSON{}, http.StatusInternalServerError, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = '"+eoCleanEnqueueTimeout+"'"); err != nil {
		return eoCleanJobJSON{}, http.StatusInternalServerError, err
	}

	// Source verification + label default happen before the job insert so a
	// bad source costs nothing.
	label := strings.TrimSpace(req.Label)
	switch req.SourceType {
	case "segment":
		var name string
		if err := tx.QueryRowContext(ctx, `
			SELECT name FROM mailing_segments WHERE id = $1::uuid AND organization_id = $2
		`, req.SourceRef, orgID).Scan(&name); err != nil {
			if err == sql.ErrNoRows {
				return eoCleanJobJSON{}, http.StatusNotFound, fmt.Errorf("segment %s not found in this organization", req.SourceRef)
			}
			return eoCleanJobJSON{}, http.StatusInternalServerError, err
		}
		if label == "" {
			label = name
		}
	case "list":
		var name string
		if err := tx.QueryRowContext(ctx, `
			SELECT name FROM mailing_lists WHERE id = $1::uuid AND organization_id = $2
		`, req.SourceRef, orgID).Scan(&name); err != nil {
			if err == sql.ErrNoRows {
				return eoCleanJobJSON{}, http.StatusNotFound, fmt.Errorf("list %s not found in this organization", req.SourceRef)
			}
			return eoCleanJobJSON{}, http.StatusInternalServerError, err
		}
		if label == "" {
			label = name
		}
	case "upload":
		if label == "" {
			label = fmt.Sprintf("upload %s", time.Now().UTC().Format("2006-01-02 15:04"))
		}
	}

	var jobID string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO mailing_eo_clean_jobs
			(organization_id, source_type, source_ref, label, daily_cap, created_by, status)
		VALUES ($1, $2, $3, $4, $5, $6, 'queued')
		RETURNING id::text
	`, orgID, req.SourceType, req.SourceRef, label, req.DailyCap, strings.TrimSpace(req.CreatedBy)).Scan(&jobID); err != nil {
		return eoCleanJobJSON{}, http.StatusInternalServerError, err
	}

	var total, queued int64
	switch req.SourceType {
	case "upload":
		total = int64(len(uploadEmails))
		for i := 0; i < len(uploadEmails); i += eoCleanUploadChunk {
			end := i + eoCleanUploadChunk
			if end > len(uploadEmails) {
				end = len(uploadEmails)
			}
			res, err := tx.ExecContext(ctx, `
				INSERT INTO mailing_eo_clean_items (job_id, email_lower)
				SELECT $1::uuid, e FROM unnest($2::text[]) AS e
				WHERE NOT EXISTS (
					SELECT 1 FROM mailing_eo_validation v
					WHERE v.email_lower = e AND v.status = 'Verified'
				)
				ON CONFLICT (job_id, email_lower) DO NOTHING
			`, jobID, pq.Array(uploadEmails[i:end]))
			if err != nil {
				return eoCleanJobJSON{}, http.StatusInternalServerError, err
			}
			n, _ := res.RowsAffected()
			queued += n
		}
	case "segment":
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(DISTINCT lower(m.email))
			FROM mailing_segment_members m
			WHERE m.segment_id = $1::uuid AND COALESCE(m.email, '') <> ''
		`, req.SourceRef).Scan(&total); err != nil {
			return eoCleanJobJSON{}, http.StatusInternalServerError, err
		}
		if total > eoCleanMaxJobSize {
			return eoCleanJobJSON{}, http.StatusBadRequest,
				fmt.Errorf("segment resolves to %d emails — over the %d per-job bound; split the source", total, eoCleanMaxJobSize)
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO mailing_eo_clean_items (job_id, email_lower)
			SELECT DISTINCT $1::uuid, lower(m.email)
			FROM mailing_segment_members m
			WHERE m.segment_id = $2::uuid AND COALESCE(m.email, '') <> ''
			  AND NOT EXISTS (
				SELECT 1 FROM mailing_eo_validation v
				WHERE v.email_lower = lower(m.email) AND v.status = 'Verified'
			  )
			ON CONFLICT (job_id, email_lower) DO NOTHING
		`, jobID, req.SourceRef)
		if err != nil {
			return eoCleanJobJSON{}, http.StatusInternalServerError, err
		}
		queued, _ = res.RowsAffected()
	case "list":
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(DISTINCT lower(s.email))
			FROM mailing_list_subscribers ls
			JOIN mailing_subscribers s ON s.id = ls.subscriber_id
			WHERE ls.list_id = $1::uuid AND COALESCE(s.email, '') <> ''
		`, req.SourceRef).Scan(&total); err != nil {
			return eoCleanJobJSON{}, http.StatusInternalServerError, err
		}
		if total > eoCleanMaxJobSize {
			return eoCleanJobJSON{}, http.StatusBadRequest,
				fmt.Errorf("list resolves to %d emails — over the %d per-job bound; split the source", total, eoCleanMaxJobSize)
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO mailing_eo_clean_items (job_id, email_lower)
			SELECT DISTINCT $1::uuid, lower(s.email)
			FROM mailing_list_subscribers ls
			JOIN mailing_subscribers s ON s.id = ls.subscriber_id
			WHERE ls.list_id = $2::uuid AND COALESCE(s.email, '') <> ''
			  AND NOT EXISTS (
				SELECT 1 FROM mailing_eo_validation v
				WHERE v.email_lower = lower(s.email) AND v.status = 'Verified'
			  )
			ON CONFLICT (job_id, email_lower) DO NOTHING
		`, jobID, req.SourceRef)
		if err != nil {
			return eoCleanJobJSON{}, http.StatusInternalServerError, err
		}
		queued, _ = res.RowsAffected()
	}

	if total == 0 {
		return eoCleanJobJSON{}, http.StatusBadRequest, fmt.Errorf("source resolved to 0 emails — nothing to clean")
	}
	alreadyClean := total - queued

	// A job where everything was already Verified completes at birth.
	status, finishedExpr := "queued", "NULL"
	if queued == 0 {
		status, finishedExpr = "done", "NOW()"
	}
	row := tx.QueryRowContext(ctx, `
		UPDATE mailing_eo_clean_jobs SET
			total_count = $2, already_clean_count = $3, queued_count = $4,
			status = $5, finished_at = `+finishedExpr+`
		WHERE id = $1::uuid
		RETURNING `+eoCleanJobColumns,
		jobID, total, alreadyClean, queued, status)
	job, err := scanEOCleanJob(row.Scan)
	if err != nil {
		return eoCleanJobJSON{}, http.StatusInternalServerError, err
	}
	if err := tx.Commit(); err != nil {
		return eoCleanJobJSON{}, http.StatusInternalServerError, err
	}
	return job, http.StatusOK, nil
}

// ListJobs returns the org's jobs, newest first.
func (s *EOCleanStore) ListJobs(r *http.Request, orgID uuid.UUID) ([]eoCleanJobJSON, error) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT `+eoCleanJobColumns+`
		FROM mailing_eo_clean_jobs
		WHERE organization_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, orgID, eoCleanListLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]eoCleanJobJSON, 0, 16)
	for rows.Next() {
		j, err := scanEOCleanJob(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// GetJob returns one org-scoped job (+ a sample of failed items).
func (s *EOCleanStore) GetJob(r *http.Request, orgID uuid.UUID, jobID string) (eoCleanJobJSON, []eoCleanFailedItemJSON, error) {
	row := s.db.QueryRowContext(r.Context(), `
		SELECT `+eoCleanJobColumns+`
		FROM mailing_eo_clean_jobs
		WHERE id = $1::uuid AND organization_id = $2
	`, jobID, orgID)
	job, err := scanEOCleanJob(row.Scan)
	if err != nil {
		return job, nil, err
	}
	fails := make([]eoCleanFailedItemJSON, 0, 4)
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT email_lower, COALESCE(result, '')
		FROM mailing_eo_clean_items
		WHERE job_id = $1::uuid AND status = 'failed'
		ORDER BY email_lower
		LIMIT $2
	`, jobID, eoCleanFailSampleLimit)
	if err != nil {
		return job, fails, nil // sample is best-effort; the job row is the truth
	}
	defer rows.Close()
	for rows.Next() {
		var f eoCleanFailedItemJSON
		if err := rows.Scan(&f.EmailLower, &f.Result); err != nil {
			break
		}
		fails = append(fails, f)
	}
	return job, fails, nil
}

// Transition applies a validated pause/resume. Returns the updated job.
// sql.ErrNoRows = job missing OR transition illegal for its current status —
// the handler disambiguates with an existence probe.
func (s *EOCleanStore) Transition(r *http.Request, orgID uuid.UUID, jobID, action string) (eoCleanJobJSON, error) {
	var fromStatuses []string
	var to string
	switch action {
	case "pause":
		fromStatuses, to = []string{"queued", "running"}, "paused"
	case "resume":
		fromStatuses, to = []string{"paused"}, "queued"
	}
	row := s.db.QueryRowContext(r.Context(), `
		UPDATE mailing_eo_clean_jobs SET status = $3
		WHERE id = $1::uuid AND organization_id = $2 AND status = ANY($4)
		RETURNING `+eoCleanJobColumns,
		jobID, orgID, to, pq.Array(fromStatuses))
	return scanEOCleanJob(row.Scan)
}

// JobExists probes existence for the 404-vs-409 disambiguation.
func (s *EOCleanStore) JobExists(r *http.Request, orgID uuid.UUID, jobID string) bool {
	var one int
	err := s.db.QueryRowContext(r.Context(), `
		SELECT 1 FROM mailing_eo_clean_jobs WHERE id = $1::uuid AND organization_id = $2
	`, jobID, orgID).Scan(&one)
	return err == nil
}

// ── Service / handlers ──────────────────────────────────────────────────────

// EOCleanService exposes the EO cleaning job intake over HTTP.
type EOCleanService struct {
	store *EOCleanStore
}

func NewEOCleanService(db *sql.DB) *EOCleanService {
	return &EOCleanService{store: NewEOCleanStore(db)}
}

// RegisterRoutes mounts the service under the /api/mailing group.
func (s *EOCleanService) RegisterRoutes(r chi.Router) {
	r.Post("/eo-clean/jobs", s.HandleCreateJob)
	r.Get("/eo-clean/jobs", s.HandleListJobs)
	r.Get("/eo-clean/jobs/{id}", s.HandleGetJob)
	r.Post("/eo-clean/jobs/{id}/pause", s.HandlePause)
	r.Post("/eo-clean/jobs/{id}/resume", s.HandleResume)
}

// HandleCreateJob handles POST /api/mailing/eo-clean/jobs.
func (s *EOCleanService) HandleCreateJob(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization not resolved")
		return
	}
	var req eoCleanCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	req.SourceType = strings.ToLower(strings.TrimSpace(req.SourceType))
	req.SourceRef = strings.TrimSpace(req.SourceRef)
	if req.DailyCap < 0 {
		respondError(w, http.StatusBadRequest, "daily_cap must be >= 0 (0 = uncapped; the global daily budget still applies)")
		return
	}

	var uploadEmails []string
	switch req.SourceType {
	case "upload":
		if len(req.Emails) == 0 {
			respondError(w, http.StatusBadRequest, "upload source requires a non-empty emails array (multipart CSV upload is a noted follow-up)")
			return
		}
		if len(req.Emails) > eoCleanMaxUploadEmails {
			respondError(w, http.StatusBadRequest,
				fmt.Sprintf("emails array has %d entries — over the %d per-request bound; split the upload", len(req.Emails), eoCleanMaxUploadEmails))
			return
		}
		clean, bad := normalizeEOCleanEmails(req.Emails)
		if len(bad) > 0 {
			shown := bad
			if len(shown) > 10 {
				shown = shown[:10]
			}
			respondError(w, http.StatusBadRequest,
				fmt.Sprintf("%d malformed email(s) — nothing enqueued (fix the paste): %s", len(bad), strings.Join(shown, "; ")))
			return
		}
		if len(clean) == 0 {
			respondError(w, http.StatusBadRequest, "emails array normalized to 0 addresses")
			return
		}
		uploadEmails = clean
	case "segment", "list":
		if _, err := uuid.Parse(req.SourceRef); err != nil {
			respondError(w, http.StatusBadRequest, "source_ref must be the "+req.SourceType+" id (uuid)")
			return
		}
	default:
		respondError(w, http.StatusBadRequest, "source_type must be one of: upload, segment, list")
		return
	}

	job, code, err := s.store.CreateJob(r, orgID, req, uploadEmails)
	if err != nil {
		respondError(w, code, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"api_version": VersionEOCleanAPI,
		"job":         job,
	})
}

// HandleListJobs handles GET /api/mailing/eo-clean/jobs.
func (s *EOCleanService) HandleListJobs(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization not resolved")
		return
	}
	jobs, err := s.store.ListJobs(r, orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "eo-clean jobs: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"api_version": VersionEOCleanAPI,
		"jobs":        jobs,
	})
}

// HandleGetJob handles GET /api/mailing/eo-clean/jobs/{id}.
func (s *EOCleanService) HandleGetJob(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization not resolved")
		return
	}
	jobID := chi.URLParam(r, "id")
	if _, err := uuid.Parse(jobID); err != nil {
		respondError(w, http.StatusBadRequest, "invalid job id")
		return
	}
	job, fails, err := s.store.GetJob(r, orgID, jobID)
	if err != nil {
		if err == sql.ErrNoRows {
			respondError(w, http.StatusNotFound, "job not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "eo-clean job: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"api_version":  VersionEOCleanAPI,
		"job":          job,
		"failed_items": fails,
	})
}

// HandlePause handles POST /api/mailing/eo-clean/jobs/{id}/pause.
func (s *EOCleanService) HandlePause(w http.ResponseWriter, r *http.Request) {
	s.handleTransition(w, r, "pause")
}

// HandleResume handles POST /api/mailing/eo-clean/jobs/{id}/resume.
func (s *EOCleanService) HandleResume(w http.ResponseWriter, r *http.Request) {
	s.handleTransition(w, r, "resume")
}

func (s *EOCleanService) handleTransition(w http.ResponseWriter, r *http.Request, action string) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization not resolved")
		return
	}
	jobID := chi.URLParam(r, "id")
	if _, err := uuid.Parse(jobID); err != nil {
		respondError(w, http.StatusBadRequest, "invalid job id")
		return
	}
	job, err := s.store.Transition(r, orgID, jobID, action)
	if err != nil {
		if err == sql.ErrNoRows {
			if s.store.JobExists(r, orgID, jobID) {
				respondError(w, http.StatusConflict, "job status does not allow "+action)
			} else {
				respondError(w, http.StatusNotFound, "job not found")
			}
			return
		}
		respondError(w, http.StatusInternalServerError, "eo-clean "+action+": "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"api_version": VersionEOCleanAPI,
		"job":         job,
	})
}
