package api

import (
	"context"
	"database/sql"
	"net/http"
	"time"
)

// WorkerHealthVersion is bumped on every change to this handler's response
// shape so the UI footer and API consumers can detect drift.
const WorkerHealthVersion = "1.0"

// workerHealthRow is the per-worker health record surfaced to the UI.
type workerHealthRow struct {
	WorkerName        string  `json:"worker_name"`
	LastBeatAt        string  `json:"last_beat_at"`
	SecondsSinceBeat  int64   `json:"seconds_since_beat"`
	LastStatus        string  `json:"last_status"`
	LastError         string  `json:"last_error,omitempty"`
	CycleCount        int64   `json:"cycle_count"`
	ExpectedInterval  int     `json:"expected_interval_seconds"`
	Stalled           bool    `json:"stalled"`
	StalledSince      *string `json:"stalled_since,omitempty"`
	LastAlertedAt     *string `json:"last_alerted_at,omitempty"`
}

type workerHealthResponse struct {
	APIVersion  string            `json:"api_version"`
	GeneratedAt string            `json:"generated_at"`
	StalledCount int              `json:"stalled_count"`
	Workers     []workerHealthRow `json:"workers"`
}

// HandleWorkerHealth returns the current heartbeat/stall state of every
// instrumented background worker. Read-only; safe to poll.
func HandleWorkerHealth(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		rows, err := db.QueryContext(ctx, `
			SELECT worker_name,
			       last_beat_at,
			       EXTRACT(EPOCH FROM (NOW() - last_beat_at))::bigint AS seconds_since_beat,
			       last_status,
			       COALESCE(last_error, ''),
			       cycle_count,
			       expected_interval_seconds,
			       stalled,
			       stalled_since,
			       last_alerted_at
			FROM mailing_worker_heartbeats
			ORDER BY stalled DESC, worker_name ASC
		`)
		if err != nil {
			respondJSON(w, http.StatusOK, workerHealthResponse{
				APIVersion:  WorkerHealthVersion,
				GeneratedAt: time.Now().UTC().Format(time.RFC3339),
				Workers:     []workerHealthRow{},
			})
			return
		}
		defer rows.Close()

		out := make([]workerHealthRow, 0, 16)
		stalled := 0
		for rows.Next() {
			var (
				rec          workerHealthRow
				lastBeat     time.Time
				stalledSince sql.NullTime
				lastAlerted  sql.NullTime
			)
			if err := rows.Scan(
				&rec.WorkerName, &lastBeat, &rec.SecondsSinceBeat, &rec.LastStatus,
				&rec.LastError, &rec.CycleCount, &rec.ExpectedInterval, &rec.Stalled,
				&stalledSince, &lastAlerted,
			); err != nil {
				continue
			}
			rec.LastBeatAt = lastBeat.UTC().Format(time.RFC3339)
			if stalledSince.Valid {
				s := stalledSince.Time.UTC().Format(time.RFC3339)
				rec.StalledSince = &s
			}
			if lastAlerted.Valid {
				s := lastAlerted.Time.UTC().Format(time.RFC3339)
				rec.LastAlertedAt = &s
			}
			if rec.Stalled {
				stalled++
			}
			out = append(out, rec)
		}

		respondJSON(w, http.StatusOK, workerHealthResponse{
			APIVersion:   WorkerHealthVersion,
			GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
			StalledCount: stalled,
			Workers:      out,
		})
	}
}
