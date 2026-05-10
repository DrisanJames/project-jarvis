package api

// Wave processor status handler — pure observability for the per-domain
// engagement engine wave-processing pipeline (SA-7, 2026-05-09).
//
// GET /api/wave-processor/status returns JSON with current pipeline metrics.
// Manual verification:
//
//	curl https://projectjarvis.io/api/wave-processor/status | jq
//
// The handler runs ONE consolidated SQL query (per-domain queue depth +
// overdue + max delay + last completed) and merges the result with the
// in-memory throughput snapshot pulled from SendWorkerPool. No new tables,
// no schema changes — purely a read view.
//
// Wired in cmd/server/main.go after sendWorkerPool is constructed; see
// Server.RegisterWaveProcessorStatusRoute.

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// WaveProcessorStatus is the JSON shape returned by GET /api/wave-processor/status.
// It blends DB-derived wave queue depth with in-memory send throughput so
// operators see one consistent snapshot of the entire pipeline per request.
type WaveProcessorStatus struct {
	Generated                   time.Time          `json:"generated"`
	WaveQueueDepthByDomain      map[string]int     `json:"wave_queue_depth_by_domain"`
	WaveOverdue5mByDomain       map[string]int     `json:"wave_overdue_5m_by_domain"`
	MaxWaveDelaySecondsByDomain map[string]float64 `json:"max_wave_delay_seconds_by_domain"`
	SendsLast60sByDomain        map[string]int     `json:"sends_last_60s_by_domain"`
	LastSendAtByDomain          map[string]string  `json:"last_send_at_by_domain"`
}

// ThroughputProvider exposes the in-memory send throughput. The
// SendWorkerPool implements this. Kept as an interface so the status
// handler can be unit-tested against a stub without standing up a real
// worker pool.
type ThroughputProvider interface {
	Throughput() map[string]int
}

// sqlExec narrows the *sql.DB surface that the handler needs. Lets tests
// pass a sqlmock-backed *sql.DB or a custom stub without exposing the
// full database/sql API.
type sqlExec interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
}

// WaveProcessorStatusHandler captures the dependencies the handler needs.
// DB is required; ThroughputProvider may be nil (degrades gracefully —
// the throughput map is reported as empty).
type WaveProcessorStatusHandler struct {
	DB                 sqlExec
	ThroughputProvider ThroughputProvider
}

// waveProcessorStatusQuery is the consolidated per-sending-domain wave
// pipeline query. Bounded to the last 7 days of waves to avoid scanning
// history that has no operational signal.
const waveProcessorStatusQuery = `
SELECT
    sp.sending_domain,
    COUNT(*) FILTER (WHERE w.status='planned' AND w.scheduled_at <= NOW())                                       AS waves_due,
    COUNT(*) FILTER (WHERE w.status='planned' AND w.scheduled_at < NOW() - INTERVAL '5 minutes')                 AS waves_overdue_5m,
    COALESCE(MAX(EXTRACT(EPOCH FROM (NOW() - w.scheduled_at)))
             FILTER (WHERE w.status='planned' AND w.scheduled_at <= NOW()), 0)                                   AS max_due_age_seconds,
    MAX(w.completed_at) FILTER (WHERE w.status='completed')                                                      AS last_completed_at
FROM mailing_campaign_waves w
JOIN mailing_campaigns c ON c.id = w.campaign_id
JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
WHERE w.scheduled_at > NOW() - INTERVAL '7 days'
GROUP BY sp.sending_domain
`

// ServeHTTP implements http.Handler. Only GET is supported; everything else
// gets 405. A DB error returns 500; an empty result returns 200 with empty
// maps so the JSON shape is stable for downstream consumers.
func (h *WaveProcessorStatusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	status := WaveProcessorStatus{
		Generated:                   time.Now().UTC(),
		WaveQueueDepthByDomain:      map[string]int{},
		WaveOverdue5mByDomain:       map[string]int{},
		MaxWaveDelaySecondsByDomain: map[string]float64{},
		SendsLast60sByDomain:        map[string]int{},
		LastSendAtByDomain:          map[string]string{},
	}

	if h.DB != nil {
		rows, err := h.DB.QueryContext(ctx, waveProcessorStatusQuery)
		if err != nil {
			log.Printf("[wave_processor_status] db query error: %v", err)
			http.Error(w, `{"error":"db query failed"}`, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var (
				domain         string
				wavesDue       int
				wavesOverdue   int
				maxDueAgeSecs  float64
				lastCompleted  sql.NullTime
			)
			if err := rows.Scan(&domain, &wavesDue, &wavesOverdue, &maxDueAgeSecs, &lastCompleted); err != nil {
				log.Printf("[wave_processor_status] row scan error: %v", err)
				http.Error(w, `{"error":"row scan failed"}`, http.StatusInternalServerError)
				return
			}
			status.WaveQueueDepthByDomain[domain] = wavesDue
			status.WaveOverdue5mByDomain[domain] = wavesOverdue
			status.MaxWaveDelaySecondsByDomain[domain] = maxDueAgeSecs
			if lastCompleted.Valid {
				status.LastSendAtByDomain[domain] = lastCompleted.Time.UTC().Format(time.RFC3339)
			}
		}
		if err := rows.Err(); err != nil {
			log.Printf("[wave_processor_status] rows iteration error: %v", err)
			http.Error(w, `{"error":"rows iteration failed"}`, http.StatusInternalServerError)
			return
		}
	}

	if h.ThroughputProvider != nil {
		for d, c := range h.ThroughputProvider.Throughput() {
			status.SendsLast60sByDomain[d] = c
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(status); err != nil {
		log.Printf("[wave_processor_status] response encode error: %v", err)
	}
}
