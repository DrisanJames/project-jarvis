package api

// =============================================================================
// FRESH BROADCAST RUNS API — /api/mailing/fresh-broadcast/*
// =============================================================================
// The HTTP surface over FreshBroadcastRunner (fresh_broadcast_runner.go):
//
//   POST /api/mailing/fresh-broadcast/runs        {date?, dry, trigger?} → run
//   GET  /api/mailing/fresh-broadcast/runs        ?limit=N → recent runs
//   GET  /api/mailing/fresh-broadcast/runs/{id}   → one run
//   GET  /api/mailing/fresh-broadcast/status      → auto_stage map + last runs
//   POST /api/mailing/fresh-broadcast/auto-stage  {stream_key, auto_stage}
//
// auto_stage lives here (not in the stream-broadcast config PUT) so the
// config service's optimistic-lock contract stays untouched: flipping the
// toggle is a single-column write with its own audit trail (updated_by).
// auto_stage=TRUE only lets the nightly worker run the LIVE stage step —
// campaigns still land as drafts; the Draft Board remains the approval gate.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// FreshBroadcastRunService owns the routes.
type FreshBroadcastRunService struct {
	db     *sql.DB
	runner *FreshBroadcastRunner
}

// NewFreshBroadcastRunService constructs the service (one runner per process).
func NewFreshBroadcastRunService(db *sql.DB) *FreshBroadcastRunService {
	return &FreshBroadcastRunService{db: db, runner: NewFreshBroadcastRunner(db)}
}

// Runner exposes the runner for boot wiring (worker injection in main.go).
func (s *FreshBroadcastRunService) Runner() *FreshBroadcastRunner { return s.runner }

// RegisterRoutes mounts the service under the /api/mailing group.
func (s *FreshBroadcastRunService) RegisterRoutes(r chi.Router) {
	r.Route("/fresh-broadcast", func(r chi.Router) {
		r.Post("/runs", s.HandleCreateRun)
		r.Get("/runs", s.HandleListRuns)
		r.Get("/runs/{id}", s.HandleGetRun)
		r.Get("/status", s.HandleStatus)
		r.Post("/auto-stage", s.HandleAutoStage)
	})
}

type freshRunRequest struct {
	Date    string `json:"date"`
	Dry     bool   `json:"dry"`
	Trigger string `json:"trigger"` // api (default) | screen
}

// HandleCreateRun handles POST /api/mailing/fresh-broadcast/runs. Runs are
// synchronous — the response carries the full per-stream results.
func (s *FreshBroadcastRunService) HandleCreateRun(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization not resolved")
		return
	}
	var in freshRunRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	trigger := strings.TrimSpace(in.Trigger)
	switch trigger {
	case "", "api":
		trigger = "api"
	case "screen":
	default:
		respondError(w, http.StatusBadRequest, "trigger must be 'api' or 'screen'")
		return
	}
	// The run itself detaches its write path from r.Context() (see
	// FreshBroadcastRunner.stage) — a client disconnect never abandons a
	// half-staged batch. The request context still bounds the read phase.
	res, err := s.runner.Run(r.Context(), orgID, FreshRunOptions{
		Date: in.Date, Dry: in.Dry, Trigger: trigger,
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "fresh broadcast run: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, res)
}

// freshRunRecord is one persisted run row.
type freshRunRecord struct {
	ID        string          `json:"id"`
	Date      string          `json:"date"`
	Dry       bool            `json:"dry"`
	Trigger   string          `json:"trigger"`
	Status    string          `json:"status"`
	Results   json.RawMessage `json:"results"`
	CreatedAt time.Time       `json:"created_at"`
}

func scanFreshRun(sc interface{ Scan(...any) error }) (freshRunRecord, error) {
	var rec freshRunRecord
	var results string
	var runDate time.Time
	if err := sc.Scan(&rec.ID, &runDate, &rec.Dry, &rec.Trigger, &rec.Status,
		&results, &rec.CreatedAt); err != nil {
		return rec, err
	}
	rec.Date = runDate.Format("2006-01-02")
	rec.Results = json.RawMessage(results)
	return rec, nil
}

const freshRunCols = `id::text, run_date, dry, trigger_source, status, results::text, created_at`

// HandleListRuns handles GET /api/mailing/fresh-broadcast/runs?limit=N.
func (s *FreshBroadcastRunService) HandleListRuns(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization not resolved")
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT `+freshRunCols+`
		FROM mailing_fresh_broadcast_runs
		WHERE organization_id = $1
		ORDER BY created_at DESC
		LIMIT $2`, orgID, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "list runs: "+err.Error())
		return
	}
	defer rows.Close()
	out := make([]freshRunRecord, 0, limit)
	for rows.Next() {
		rec, err := scanFreshRun(rows)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "scan run: "+err.Error())
			return
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "list runs: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"runs": out})
}

// HandleGetRun handles GET /api/mailing/fresh-broadcast/runs/{id}.
func (s *FreshBroadcastRunService) HandleGetRun(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization not resolved")
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		respondError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	rec, err := scanFreshRun(s.db.QueryRowContext(r.Context(), `
		SELECT `+freshRunCols+`
		FROM mailing_fresh_broadcast_runs
		WHERE organization_id = $1 AND id = $2`, orgID, id))
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "get run: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, rec)
}

// freshStreamStatus is the per-stream auto_stage flag for the screen.
type freshStreamStatus struct {
	StreamKey string `json:"stream_key"`
	Enabled   bool   `json:"enabled"`
	AutoStage bool   `json:"auto_stage"`
}

// HandleStatus handles GET /api/mailing/fresh-broadcast/status — the screen's
// last-run panel source: per-stream auto_stage + the most recent runs.
func (s *FreshBroadcastRunService) HandleStatus(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization not resolved")
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT stream_key, enabled, COALESCE(auto_stage, FALSE)
		FROM mailing_stream_broadcast_config
		WHERE organization_id = $1
		ORDER BY stream_key`, orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "stream status: "+err.Error())
		return
	}
	streams := make([]freshStreamStatus, 0, 8)
	for rows.Next() {
		var st freshStreamStatus
		if err := rows.Scan(&st.StreamKey, &st.Enabled, &st.AutoStage); err != nil {
			rows.Close()
			respondError(w, http.StatusInternalServerError, "scan stream status: "+err.Error())
			return
		}
		streams = append(streams, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "stream status: "+err.Error())
		return
	}

	runRows, err := s.db.QueryContext(r.Context(), `
		SELECT `+freshRunCols+`
		FROM mailing_fresh_broadcast_runs
		WHERE organization_id = $1
		ORDER BY created_at DESC
		LIMIT 5`, orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "recent runs: "+err.Error())
		return
	}
	defer runRows.Close()
	runs := make([]freshRunRecord, 0, 5)
	for runRows.Next() {
		rec, err := scanFreshRun(runRows)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "scan run: "+err.Error())
			return
		}
		runs = append(runs, rec)
	}
	if err := runRows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "recent runs: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"streams": streams, "runs": runs})
}

type freshAutoStageRequest struct {
	StreamKey string `json:"stream_key"`
	AutoStage bool   `json:"auto_stage"`
}

// HandleAutoStage handles POST /api/mailing/fresh-broadcast/auto-stage.
func (s *FreshBroadcastRunService) HandleAutoStage(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization not resolved")
		return
	}
	var in freshAutoStageRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	in.StreamKey = strings.TrimSpace(in.StreamKey)
	if in.StreamKey == "" {
		respondError(w, http.StatusBadRequest, "stream_key is required")
		return
	}
	updatedBy := strings.TrimSpace(r.Header.Get("X-User-Email"))
	if updatedBy == "" {
		updatedBy = "portal"
	}
	res, err := s.db.ExecContext(r.Context(), `
		UPDATE mailing_stream_broadcast_config
		SET auto_stage = $3, updated_at = NOW(), updated_by = $4
		WHERE organization_id = $1 AND stream_key = $2`,
		orgID, in.StreamKey, in.AutoStage, updatedBy)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "auto_stage update: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(w, http.StatusNotFound, "stream not found: "+in.StreamKey)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"stream_key": in.StreamKey, "auto_stage": in.AutoStage,
	})
}
