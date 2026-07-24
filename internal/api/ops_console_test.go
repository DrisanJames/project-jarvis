package api

// Tests for GET /api/mailing/ops/job-runs (Coalition WS3, REQ-C19 slice).
// sqlmock only (no DB); org scoping via X-Organization-ID header.

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

const opsTestOrg = "3c9e6f9a-6a2b-4c58-9f2e-0d3a1b2c4d5e"

func opsTestRequest(t *testing.T, url string) (*httptest.ResponseRecorder, func(handler http.Handler)) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-Organization-ID", opsTestOrg)
	rec := httptest.NewRecorder()
	return rec, func(handler http.Handler) { handler.ServeHTTP(rec, req) }
}

func TestOpsJobRuns_ReturnsRowsNewestFirst(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	newer := time.Date(2026, 7, 24, 2, 41, 2, 0, time.UTC)
	older := time.Date(2026, 7, 23, 2, 13, 3, 0, time.UTC)
	mock.ExpectQuery(`FROM mailing_worker_runs`).
		WithArgs("job:send_day_invariants", 30).
		WillReturnRows(sqlmock.NewRows([]string{
			"started_at", "finished_at", "duration_ms", "status",
			"items_processed", "items_failed", "detail",
		}).
			AddRow(newer, newer.Add(40*time.Second), 40000, "ok", 8, 0, `{"verdict":"GREEN"}`).
			AddRow(older, older.Add(41*time.Second), 41000, "failed", 8, 1, `{"verdict":"RED"}`))

	svc := NewOpsConsoleService(db)
	router := chi.NewRouter()
	svc.RegisterRoutes(router)

	rec, serve := opsTestRequest(t, "/ops/job-runs?worker=job:send_day_invariants")
	serve(router)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp opsJobRunsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.TablePresent {
		t.Errorf("table_present = false, want true")
	}
	if len(resp.Runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(resp.Runs))
	}
	if resp.Runs[0].Status != "ok" || resp.Runs[1].Status != "failed" {
		t.Errorf("runs out of order: %+v", resp.Runs)
	}
	if resp.Runs[1].ItemsFailed != 1 || resp.Runs[1].Detail != `{"verdict":"RED"}` {
		t.Errorf("row fields not mapped: %+v", resp.Runs[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Negative path (AS-6.1): the runs table not yet migrated (42P01) is a designed
// state — 200 with table_present=false and empty runs, never a 500.
func TestOpsJobRuns_MissingTableIsDesignedState(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM mailing_worker_runs`).
		WillReturnError(&pq.Error{Code: "42P01"})

	svc := NewOpsConsoleService(db)
	router := chi.NewRouter()
	svc.RegisterRoutes(router)

	rec, serve := opsTestRequest(t, "/ops/job-runs?worker=job:cohort_growth")
	serve(router)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp opsJobRunsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.TablePresent {
		t.Errorf("table_present = true, want false")
	}
	if resp.Runs == nil || len(resp.Runs) != 0 {
		t.Errorf("runs = %v, want empty non-nil", resp.Runs)
	}
}

// Negative path: worker param absent or malformed → 400, no query issued.
func TestOpsJobRuns_RejectsBadWorkerParam(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	svc := NewOpsConsoleService(db)
	router := chi.NewRouter()
	svc.RegisterRoutes(router)

	for _, url := range []string{
		"/ops/job-runs",                       // missing
		"/ops/job-runs?worker=bad%20name",     // whitespace
		"/ops/job-runs?worker=job:x;drop",     // disallowed char
		"/ops/job-runs?worker=job:ok&limit=0", // limit below floor
		"/ops/job-runs?worker=job:ok&limit=121",
	} {
		rec, serve := opsTestRequest(t, url)
		serve(router)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", url, rec.Code)
		}
	}
}
