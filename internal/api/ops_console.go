package api

// =============================================================================
// OPS CONSOLE — job-run history read API (Platform Coalition WS3, REQ-C19 slice)
// =============================================================================
// GET /api/mailing/ops/job-runs?worker=<name>&limit=N
//
// One minimal read-only endpoint over mailing_worker_runs (the per-run detail
// mechanism ratified in tasks/eng-team/coalition/SCHEMA-CONTRACTS.md §3). It
// exists so operator-machine job artifacts stop being invisible to the portal:
// scaffold-compliant external jobs (worker_name prefix 'job:') record one run
// row per cycle with a JSON `detail`, and the Operations console renders that
// history — send-day invariant results (job:send_day_invariants) and the
// cohort-growth ledger snapshots (job:cohort_growth) are the first consumers.
// The Python-side write requirements are filed in
// tasks/eng-team/coalition/DECISIONS.md (WS4); until those jobs write, this
// endpoint truthfully returns an empty run list, which the UI must present as
// "no results recorded", never as a healthy empty (AS-6.1).
//
// Resilience: a missing mailing_worker_runs table (startup-migration ordering)
// yields 200 with table_present=false — "never migrated" is distinguishable
// from "no rows yet". The endpoint never 500s on that path; safe to poll.
//
// Scope note: mailing_worker_runs carries no organization_id — run history is
// platform-global observability, exactly like GET /api/worker-health. The
// handler still resolves the org context (auth-surface consistency with every
// other /api/mailing handler) but does not filter by it.

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/lib/pq"
)

// VersionOpsConsoleAPI is bumped on every change to this handler's response
// shape so API consumers can detect drift.
const VersionOpsConsoleAPI = "1.0"

const (
	opsJobRunsDefaultLimit = 30
	opsJobRunsMaxLimit     = 120
)

// opsWorkerNameRe bounds the worker filter to the naming convention actually
// in use (Go workers bare, external jobs 'job:'-prefixed — SCHEMA-CONTRACTS §3).
var opsWorkerNameRe = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,128}$`)

// opsJobRun mirrors one mailing_worker_runs row (same field names as the
// segment-workers surface so consumers share one shape).
type opsJobRun struct {
	StartedAt      string `json:"started_at"`
	FinishedAt     string `json:"finished_at"`
	DurationMs     int    `json:"duration_ms"`
	Status         string `json:"status"`
	ItemsProcessed int    `json:"items_processed"`
	ItemsFailed    int    `json:"items_failed"`
	Detail         string `json:"detail"`
}

type opsJobRunsResponse struct {
	APIVersion   string      `json:"api_version"`
	GeneratedAt  string      `json:"generated_at"`
	Worker       string      `json:"worker"`
	TablePresent bool        `json:"table_present"`
	Runs         []opsJobRun `json:"runs"`
}

// OpsJobRunsStore is the data-access layer (AS-5.2: no raw SQL in handlers).
type OpsJobRunsStore struct {
	db *sql.DB
}

func NewOpsJobRunsStore(db *sql.DB) *OpsJobRunsStore {
	return &OpsJobRunsStore{db: db}
}

// ListRuns returns up to limit runs for one worker, newest first. A missing
// table (undefined_table, 42P01) is reported via tablePresent=false with no
// error, so the read path can distinguish "never migrated" from "no rows".
func (s *OpsJobRunsStore) ListRuns(ctx context.Context, worker string, limit int) (runs []opsJobRun, tablePresent bool, err error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT started_at, finished_at, duration_ms, status,
		       items_processed, items_failed, COALESCE(detail, '')
		FROM mailing_worker_runs
		WHERE worker_name = $1
		ORDER BY started_at DESC
		LIMIT $2
	`, worker, limit)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "42P01" {
			return []opsJobRun{}, false, nil
		}
		return nil, true, err
	}
	defer rows.Close()

	out := make([]opsJobRun, 0, limit)
	for rows.Next() {
		var run opsJobRun
		var started, finished time.Time
		if err := rows.Scan(&started, &finished, &run.DurationMs, &run.Status,
			&run.ItemsProcessed, &run.ItemsFailed, &run.Detail); err != nil {
			return nil, true, err
		}
		run.StartedAt = started.UTC().Format(time.RFC3339)
		run.FinishedAt = finished.UTC().Format(time.RFC3339)
		out = append(out, run)
	}
	return out, true, rows.Err()
}

// OpsConsoleService exposes the ops-console read surfaces over HTTP.
type OpsConsoleService struct {
	store *OpsJobRunsStore
}

func NewOpsConsoleService(db *sql.DB) *OpsConsoleService {
	return &OpsConsoleService{store: NewOpsJobRunsStore(db)}
}

// RegisterRoutes mounts the service under the /api/mailing group.
func (s *OpsConsoleService) RegisterRoutes(r chi.Router) {
	r.Route("/ops", func(r chi.Router) {
		r.Get("/job-runs", s.HandleJobRuns)
	})
}

// HandleJobRuns handles GET /api/mailing/ops/job-runs.
func (s *OpsConsoleService) HandleJobRuns(w http.ResponseWriter, r *http.Request) {
	if _, err := GetOrgIDFromRequest(r); err != nil {
		respondError(w, http.StatusUnauthorized, "organization not resolved")
		return
	}

	worker := r.URL.Query().Get("worker")
	if !opsWorkerNameRe.MatchString(worker) {
		respondError(w, http.StatusBadRequest, "worker is required and must match ^[a-zA-Z0-9_.:-]{1,128}$")
		return
	}

	limit := opsJobRunsDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > opsJobRunsMaxLimit {
			respondError(w, http.StatusBadRequest, "limit must be an integer between 1 and 120")
			return
		}
		limit = n
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	runs, tablePresent, err := s.store.ListRuns(ctx, worker, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "list job runs: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, opsJobRunsResponse{
		APIVersion:   VersionOpsConsoleAPI,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Worker:       worker,
		TablePresent: tablePresent,
		Runs:         runs,
	})
}
