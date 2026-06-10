package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/lib/pq"
)

func TestSegmentWorkerOnline(t *testing.T) {
	cases := []struct {
		name     string
		secs     int64
		interval int
		want     bool
	}{
		{"fresh beat, long interval", 100, 3600, true},
		{"exactly 2x interval", 7200, 3600, true},
		{"just past 2x interval", 7201, 3600, false},
		{"short interval inside 5m grace", 250, 60, true},
		{"short interval past 5m grace", 301, 60, false},
		{"zero interval falls back to grace", 299, 0, true},
		{"zero interval past grace", 301, 0, false},
		{"negative staleness (clock skew) is fresh", -5, 3600, true},
	}
	for _, tc := range cases {
		if got := segmentWorkerOnline(tc.secs, tc.interval); got != tc.want {
			t.Errorf("%s: segmentWorkerOnline(%d, %d) = %v, want %v",
				tc.name, tc.secs, tc.interval, got, tc.want)
		}
	}
}

// Full handler pass: one heartbeat row present (segment_refresh), the other
// missing (segment_cleanup → online=false, null beat fields); runs table
// missing (42P01 → empty runs, no error); ledger aggregate present.
func TestGetSegmentWorkers_DegradedSourcesStillRespond(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	beat := time.Now().Add(-90 * time.Second)
	mock.ExpectQuery(`FROM mailing_worker_heartbeats`).
		WillReturnRows(sqlmock.NewRows([]string{
			"worker_name", "last_beat_at", "secs", "last_status", "last_error",
			"cycle_count", "expected_interval_seconds", "stalled",
		}).AddRow("segment_refresh", beat, int64(90), "ok", "", int64(42), 900, false))

	mock.ExpectQuery(`FROM mailing_worker_runs`).
		WillReturnError(&pq.Error{Code: "42P01"})

	lastStandard := time.Now().Add(-3 * time.Hour)
	mock.ExpectQuery(`FROM mailing_segment_build_ledger`).
		WillReturnRows(sqlmock.NewRows([]string{"max", "std", "eng", "lake", "failed", "blocked"}).
			AddRow(lastStandard, 6, 4, 2, 1, 0))

	api := NewSegmentationAPI(db)
	router := chi.NewRouter()
	api.RegisterRoutes(router)

	req := httptest.NewRequest(http.MethodGet, "/v2/segments/workers", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var resp segmentWorkersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.APIVersion != VersionSegmentWorkersAPI {
		t.Errorf("api_version = %q", resp.APIVersion)
	}
	if len(resp.Workers) != 2 {
		t.Fatalf("workers = %d, want exactly 2", len(resp.Workers))
	}

	refresh, cleanup := resp.Workers[0], resp.Workers[1]
	if refresh.Name != "segment_refresh" || cleanup.Name != "segment_cleanup" {
		t.Fatalf("worker order = %q, %q", refresh.Name, cleanup.Name)
	}
	if !refresh.Online {
		t.Errorf("segment_refresh should be online (90s beat, 900s interval)")
	}
	if refresh.CycleCount != 42 || refresh.LastStatus != "ok" {
		t.Errorf("segment_refresh heartbeat fields not mapped: %+v", refresh)
	}
	if refresh.LastRun != nil || len(refresh.RecentRuns) != 0 {
		t.Errorf("missing runs table must yield empty runs, got %+v", refresh.RecentRuns)
	}
	if cleanup.Online || cleanup.LastBeatAt != nil || cleanup.SecondsSinceBeat != nil {
		t.Errorf("missing heartbeat row must yield offline + nulls, got %+v", cleanup)
	}

	if resp.LakeChain.LastStandardBuiltAt == nil {
		t.Errorf("lake_chain.last_standard_built_at missing")
	}
	if resp.LakeChain.StandardBuilt24h != 6 || resp.LakeChain.EngagedBuilt24h != 4 ||
		resp.LakeChain.LakeBuilderBuilt24h != 2 || resp.LakeChain.FailedNow != 1 ||
		resp.LakeChain.BlockedNow != 0 {
		t.Errorf("lake_chain mismatch: %+v", resp.LakeChain)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Runs present: latest run becomes last_run, list is capped/ordered as given.
func TestGetSegmentWorkers_RunsMapped(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	beat := time.Now().Add(-30 * time.Second)
	mock.ExpectQuery(`FROM mailing_worker_heartbeats`).
		WillReturnRows(sqlmock.NewRows([]string{
			"worker_name", "last_beat_at", "secs", "last_status", "last_error",
			"cycle_count", "expected_interval_seconds", "stalled",
		}).
			AddRow("segment_refresh", beat, int64(30), "ok", "", int64(10), 900, false).
			AddRow("segment_cleanup", beat, int64(30), "ok", "", int64(5), 3600, false))

	now := time.Now()
	mock.ExpectQuery(`FROM mailing_worker_runs`).
		WillReturnRows(sqlmock.NewRows([]string{
			"worker_name", "started_at", "finished_at", "duration_ms", "status",
			"items_processed", "items_failed", "detail",
		}).
			AddRow("segment_refresh", now.Add(-time.Minute), now, 1234, "partial", 14, 2, "refreshed 14 dynamic segments (2 failed)").
			AddRow("segment_refresh", now.Add(-16*time.Minute), now.Add(-15*time.Minute), 900, "ok", 20, 0, "refreshed 20 dynamic segments (0 failed)").
			AddRow("segment_cleanup", now.Add(-2*time.Hour), now.Add(-2*time.Hour), 50, "ok", 4, 0, "warned 3, archived/deactivated 1, purged 0 static snapshots, deleted 0 expired archives"))

	mock.ExpectQuery(`FROM mailing_segment_build_ledger`).
		WillReturnRows(sqlmock.NewRows([]string{"max", "std", "eng", "lake", "failed", "blocked"}).
			AddRow(nil, 0, 0, 0, 0, 0))

	api := NewSegmentationAPI(db)
	router := chi.NewRouter()
	api.RegisterRoutes(router)

	req := httptest.NewRequest(http.MethodGet, "/v2/segments/workers", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", rec.Code, rec.Body.String())
	}
	var resp segmentWorkersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	refresh := resp.Workers[0]
	if refresh.LastRun == nil || refresh.LastRun.Status != "partial" ||
		refresh.LastRun.ItemsProcessed != 14 || refresh.LastRun.ItemsFailed != 2 {
		t.Errorf("last_run not the newest run: %+v", refresh.LastRun)
	}
	if len(refresh.RecentRuns) != 2 {
		t.Errorf("recent_runs = %d, want 2", len(refresh.RecentRuns))
	}
	cleanup := resp.Workers[1]
	if cleanup.LastRun == nil || cleanup.LastRun.ItemsProcessed != 4 {
		t.Errorf("cleanup last_run mismatch: %+v", cleanup.LastRun)
	}
	if resp.LakeChain.LastStandardBuiltAt != nil {
		t.Errorf("null max(last_built_at) must serialize as null")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
