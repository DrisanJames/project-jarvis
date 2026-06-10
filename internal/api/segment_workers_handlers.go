package api

// Segment-worker observability — GET /api/mailing/v2/segments/workers.
//
// One read-only endpoint that answers "are the segment workers alive, and
// what did their recent cycles actually do?" by combining three sources:
//
//   - mailing_worker_heartbeats  — liveness (EmitHeartbeat, end-of-cycle)
//   - mailing_worker_runs        — run-level summaries (RecordWorkerRun)
//   - mailing_segment_build_ledger — lake-chain build freshness/failures
//
// Resilience: each source degrades independently. A missing heartbeat row
// yields a worker entry with online=false and null timestamps; a missing
// mailing_worker_runs table (startup-migration ordering — the runner can
// exhaust its boot budget before end-of-slice entries) yields empty runs; a
// ledger query error yields a zeroed lake_chain. The endpoint itself never
// 500s on those paths. Safe to poll.

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"time"

	"github.com/lib/pq"
)

// VersionSegmentWorkersAPI is bumped on every change to this handler's
// response shape so API consumers can detect drift.
const VersionSegmentWorkersAPI = "1.0"

// segmentWorkerNames is the exact set (and order) of workers this endpoint
// reports on.
var segmentWorkerNames = []string{"segment_refresh", "segment_cleanup"}

// segmentWorkerOnlineGrace is the minimum staleness allowance regardless of
// how short a worker's declared interval is.
const segmentWorkerOnlineGrace = 5 * time.Minute

// segmentWorkerOnline reports whether a heartbeat is fresh enough to call
// the worker online: fresher than 2× its expected interval, with a minimum
// grace of segmentWorkerOnlineGrace. Pure so it is unit-testable.
func segmentWorkerOnline(secondsSinceBeat int64, expectedIntervalSeconds int) bool {
	threshold := int64(expectedIntervalSeconds) * 2
	if min := int64(segmentWorkerOnlineGrace.Seconds()); threshold < min {
		threshold = min
	}
	// Negative staleness (beat "in the future") only happens with clock skew
	// and is by definition fresh.
	return secondsSinceBeat <= threshold
}

// segmentWorkerRun mirrors one mailing_worker_runs row.
type segmentWorkerRun struct {
	StartedAt      string `json:"started_at"`
	FinishedAt     string `json:"finished_at"`
	DurationMs     int    `json:"duration_ms"`
	Status         string `json:"status"`
	ItemsProcessed int    `json:"items_processed"`
	ItemsFailed    int    `json:"items_failed"`
	Detail         string `json:"detail"`
}

type segmentWorkerStatus struct {
	Name             string             `json:"name"`
	Online           bool               `json:"online"`
	LastBeatAt       *string            `json:"last_beat_at"`
	SecondsSinceBeat *int64             `json:"seconds_since_beat"`
	ExpectedInterval *int               `json:"expected_interval_seconds"`
	CycleCount       int64              `json:"cycle_count"`
	Stalled          bool               `json:"stalled"`
	LastStatus       string             `json:"last_status"`
	LastError        string             `json:"last_error"`
	LastRun          *segmentWorkerRun  `json:"last_run"`
	RecentRuns       []segmentWorkerRun `json:"recent_runs"`
}

type segmentLakeChain struct {
	LastStandardBuiltAt *string `json:"last_standard_built_at"`
	StandardBuilt24h    int     `json:"standard_built_24h"`
	EngagedBuilt24h     int     `json:"engaged_built_24h"`
	LakeBuilderBuilt24h int     `json:"lake_builder_built_24h"`
	FailedNow           int     `json:"failed_now"`
	BlockedNow          int     `json:"blocked_now"`
}

type segmentWorkersResponse struct {
	APIVersion  string                `json:"api_version"`
	GeneratedAt string                `json:"generated_at"`
	Workers     []segmentWorkerStatus `json:"workers"`
	LakeChain   segmentLakeChain      `json:"lake_chain"`
}

// recentRunsPerWorker caps how many run rows are returned per worker.
const recentRunsPerWorker = 10

// GetSegmentWorkers handles GET /api/mailing/v2/segments/workers.
func (api *SegmentationAPI) GetSegmentWorkers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	resp := segmentWorkersResponse{
		APIVersion:  VersionSegmentWorkersAPI,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Workers:     make([]segmentWorkerStatus, 0, len(segmentWorkerNames)),
	}

	beats := api.loadSegmentWorkerHeartbeats(ctx)
	runs := api.loadSegmentWorkerRuns(ctx)

	for _, name := range segmentWorkerNames {
		ws := segmentWorkerStatus{
			Name:       name,
			RecentRuns: []segmentWorkerRun{},
		}
		if hb, ok := beats[name]; ok {
			lastBeat := hb.lastBeatAt.UTC().Format(time.RFC3339)
			secs := hb.secondsSinceBeat
			interval := hb.expectedIntervalSeconds
			ws.LastBeatAt = &lastBeat
			ws.SecondsSinceBeat = &secs
			ws.ExpectedInterval = &interval
			ws.CycleCount = hb.cycleCount
			ws.Stalled = hb.stalled
			ws.LastStatus = hb.lastStatus
			ws.LastError = hb.lastError
			ws.Online = segmentWorkerOnline(secs, interval)
		}
		if wr := runs[name]; len(wr) > 0 {
			ws.RecentRuns = wr
			ws.LastRun = &wr[0]
		}
		resp.Workers = append(resp.Workers, ws)
	}

	resp.LakeChain = api.loadSegmentLakeChain(ctx)

	respondJSON(w, http.StatusOK, resp)
}

type segmentWorkerBeat struct {
	lastBeatAt              time.Time
	secondsSinceBeat        int64
	lastStatus              string
	lastError               string
	cycleCount              int64
	expectedIntervalSeconds int
	stalled                 bool
}

func (api *SegmentationAPI) loadSegmentWorkerHeartbeats(ctx context.Context) map[string]segmentWorkerBeat {
	out := make(map[string]segmentWorkerBeat, len(segmentWorkerNames))
	if api.db == nil {
		return out
	}
	rows, err := api.db.QueryContext(ctx, `
		SELECT worker_name,
		       last_beat_at,
		       EXTRACT(EPOCH FROM (NOW() - last_beat_at))::bigint,
		       last_status,
		       COALESCE(last_error, ''),
		       cycle_count,
		       expected_interval_seconds,
		       stalled
		FROM mailing_worker_heartbeats
		WHERE worker_name = ANY($1)
	`, pq.Array(segmentWorkerNames))
	if err != nil {
		log.Printf("[segment-workers] heartbeat query: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var hb segmentWorkerBeat
		if err := rows.Scan(&name, &hb.lastBeatAt, &hb.secondsSinceBeat, &hb.lastStatus,
			&hb.lastError, &hb.cycleCount, &hb.expectedIntervalSeconds, &hb.stalled); err != nil {
			continue
		}
		out[name] = hb
	}
	return out
}

// loadSegmentWorkerRuns returns the latest recentRunsPerWorker runs for each
// segment worker, newest first. Any error (including a missing
// mailing_worker_runs table, pq 42P01, when startup migrations haven't
// created it yet) degrades to empty.
func (api *SegmentationAPI) loadSegmentWorkerRuns(ctx context.Context) map[string][]segmentWorkerRun {
	out := make(map[string][]segmentWorkerRun, len(segmentWorkerNames))
	if api.db == nil {
		return out
	}
	rows, err := api.db.QueryContext(ctx, `
		SELECT worker_name, started_at, finished_at, duration_ms, status,
		       items_processed, items_failed, COALESCE(detail, '')
		FROM (
			SELECT worker_name, started_at, finished_at, duration_ms, status,
			       items_processed, items_failed, detail,
			       ROW_NUMBER() OVER (PARTITION BY worker_name ORDER BY started_at DESC) AS rn
			FROM mailing_worker_runs
			WHERE worker_name = ANY($1)
		) t
		WHERE rn <= $2
		ORDER BY worker_name, started_at DESC
	`, pq.Array(segmentWorkerNames), recentRunsPerWorker)
	if err != nil {
		// Most likely 42P01 (table not yet created by startup migrations) —
		// either way the runs panel just shows empty, never an error.
		if pqErr, ok := err.(*pq.Error); !ok || pqErr.Code != "42P01" {
			log.Printf("[segment-workers] runs query: %v", err)
		}
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var run segmentWorkerRun
		var startedAt, finishedAt time.Time
		if err := rows.Scan(&name, &startedAt, &finishedAt, &run.DurationMs, &run.Status,
			&run.ItemsProcessed, &run.ItemsFailed, &run.Detail); err != nil {
			continue
		}
		run.StartedAt = startedAt.UTC().Format(time.RFC3339)
		run.FinishedAt = finishedAt.UTC().Format(time.RFC3339)
		out[name] = append(out[name], run)
	}
	return out
}

// loadSegmentLakeChain summarizes the lake build chain from the segment
// build ledger in a single aggregate query. Errors degrade to zeros.
func (api *SegmentationAPI) loadSegmentLakeChain(ctx context.Context) segmentLakeChain {
	var lc segmentLakeChain
	if api.db == nil {
		return lc
	}
	var lastStandard sql.NullTime
	err := api.db.QueryRowContext(ctx, `
		SELECT
			MAX(last_built_at) FILTER (WHERE build_source = 'lake-standard'),
			COUNT(*) FILTER (WHERE build_source = 'lake-standard' AND last_built_at > NOW() - INTERVAL '24 hours'),
			COUNT(*) FILTER (WHERE build_source = 'lake-engaged'  AND last_built_at > NOW() - INTERVAL '24 hours'),
			COUNT(*) FILTER (WHERE build_source = 'lake-builder'  AND last_built_at > NOW() - INTERVAL '24 hours'),
			COUNT(*) FILTER (WHERE last_build_status = 'failed'),
			COUNT(*) FILTER (WHERE last_build_status = 'blocked_delta')
		FROM mailing_segment_build_ledger
	`).Scan(&lastStandard, &lc.StandardBuilt24h, &lc.EngagedBuilt24h,
		&lc.LakeBuilderBuilt24h, &lc.FailedNow, &lc.BlockedNow)
	if err != nil {
		log.Printf("[segment-workers] lake-chain query: %v", err)
		return segmentLakeChain{}
	}
	if lastStandard.Valid {
		s := lastStandard.Time.UTC().Format(time.RFC3339)
		lc.LastStandardBuiltAt = &s
	}
	return lc
}
