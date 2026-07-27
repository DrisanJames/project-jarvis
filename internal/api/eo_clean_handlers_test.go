package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
)

const eoCleanTestOrg = "00000000-0000-0000-0000-000000000001"

var eoCleanTestNow = time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)

func newEOCleanTestRouter(t *testing.T) (*chi.Mux, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	svc := NewEOCleanService(db)
	r := chi.NewRouter()
	svc.RegisterRoutes(r)
	return r, mock, func() { db.Close() }
}

func eoCleanRequest(method, path string, body interface{}) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("X-Organization-ID", eoCleanTestOrg)
	return req.WithContext(context.Background())
}

// jobRowColumns mirrors eoCleanJobColumns for sqlmock rows.
var eoCleanTestJobCols = []string{
	"id", "source_type", "source_ref", "label", "status",
	"total_count", "already_clean_count", "queued_count", "validated_count",
	"verified_count", "complainer_count", "undeliverable_count", "other_count",
	"failed_count", "daily_cap", "created_by", "created_at", "finished_at",
}

// ── Enqueue dedup vs already-Verified ───────────────────────────────────────

// TestEOCleanCreateUploadSkipsAlreadyVerified: 3 uploaded emails, 1 already
// Verified in mailing_eo_validation → the anti-join inserts 2, and the job
// records total=3, queued=2, already_clean=1. NEVER PAY TWICE is the invariant.
func TestEOCleanCreateUploadSkipsAlreadyVerified(t *testing.T) {
	r, mock, closeFn := newEOCleanTestRouter(t)
	defer closeFn()

	jobID := "11111111-2222-3333-4444-555555555555"
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`INSERT INTO mailing_eo_clean_jobs`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(jobID))
	// The anti-join against mailing_eo_validation status='Verified' is the
	// dedup gate — 2 of 3 rows survive it.
	mock.ExpectExec(`INSERT INTO mailing_eo_clean_items[\s\S]*mailing_eo_validation[\s\S]*'Verified'`).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectQuery(`UPDATE mailing_eo_clean_jobs SET[\s\S]*total_count = \$2`).
		WithArgs(jobID, int64(3), int64(1), int64(2), "queued").
		WillReturnRows(sqlmock.NewRows(eoCleanTestJobCols).AddRow(
			jobID, "upload", "", "my upload", "queued",
			3, 1, 2, 0, 0, 0, 0, 0, 0, 0, "", eoCleanTestNow, nil))
	mock.ExpectCommit()

	req := eoCleanRequest("POST", "/eo-clean/jobs", map[string]interface{}{
		"source_type": "upload",
		"label":       "my upload",
		"emails":      []string{"A@Example.com", "b@example.com ", "c@example.com"},
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Job eoCleanJobJSON `json:"job"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Job.TotalCount != 3 || resp.Job.QueuedCount != 2 || resp.Job.AlreadyCleanCount != 1 {
		t.Fatalf("counts = total %d / queued %d / already_clean %d, want 3/2/1",
			resp.Job.TotalCount, resp.Job.QueuedCount, resp.Job.AlreadyCleanCount)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestEOCleanCreateAllAlreadyVerifiedCompletesAtBirth: every email already
// Verified → queued=0 → the job lands status='done' with finished_at set, and
// the worker never spends a cent on it.
func TestEOCleanCreateAllAlreadyVerifiedCompletesAtBirth(t *testing.T) {
	r, mock, closeFn := newEOCleanTestRouter(t)
	defer closeFn()

	jobID := "11111111-2222-3333-4444-666666666666"
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`INSERT INTO mailing_eo_clean_jobs`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(jobID))
	mock.ExpectExec(`INSERT INTO mailing_eo_clean_items`).
		WillReturnResult(sqlmock.NewResult(0, 0)) // all skipped
	mock.ExpectQuery(`UPDATE mailing_eo_clean_jobs SET[\s\S]*finished_at = NOW\(\)`).
		WithArgs(jobID, int64(2), int64(2), int64(0), "done").
		WillReturnRows(sqlmock.NewRows(eoCleanTestJobCols).AddRow(
			jobID, "upload", "", "u", "done",
			2, 2, 0, 0, 0, 0, 0, 0, 0, 0, "", eoCleanTestNow, eoCleanTestNow))
	mock.ExpectCommit()

	req := eoCleanRequest("POST", "/eo-clean/jobs", map[string]interface{}{
		"source_type": "upload", "label": "u",
		"emails": []string{"a@example.com", "b@example.com"},
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Job eoCleanJobJSON `json:"job"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Job.Status != "done" || resp.Job.FinishedAt == nil {
		t.Fatalf("job = %+v, want status done + finished_at set", resp.Job)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// ── Upload normalization (REFUSE on malformed — fail-closed) ────────────────

func TestNormalizeEOCleanEmails(t *testing.T) {
	clean, bad := normalizeEOCleanEmails([]string{
		" A@Example.COM ", "a@example.com", "", "b@x.co", "not-an-email", "@lead.com", "trail@",
	})
	if len(bad) != 3 {
		t.Fatalf("bad = %v, want 3 malformed entries", bad)
	}
	if len(clean) != 2 || clean[0] != "a@example.com" || clean[1] != "b@x.co" {
		t.Fatalf("clean = %v, want deduped lowercased [a@example.com b@x.co]", clean)
	}
}

// TestEOCleanCreateRefusesMalformedUpload: one bad entry refuses the WHOLE
// request (nothing enqueued) — the eo_tranche_landing fail-closed idiom.
func TestEOCleanCreateRefusesMalformedUpload(t *testing.T) {
	r, mock, closeFn := newEOCleanTestRouter(t)
	defer closeFn()

	req := eoCleanRequest("POST", "/eo-clean/jobs", map[string]interface{}{
		"source_type": "upload",
		"emails":      []string{"good@example.com", "not-an-email"},
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a refused request must never touch the DB: %v", err)
	}
}

// ── Pause semantics ─────────────────────────────────────────────────────────

func TestEOCleanPauseTransitionTable(t *testing.T) {
	cases := []struct {
		current, action, want string
		ok                    bool
	}{
		{"queued", "pause", "paused", true},
		{"running", "pause", "paused", true},
		{"paused", "pause", "paused", false},
		{"done", "pause", "done", false},
		{"paused", "resume", "queued", true},
		{"queued", "resume", "queued", false},
		{"done", "resume", "done", false},
	}
	for _, tc := range cases {
		got, ok := eoCleanPauseTransition(tc.current, tc.action)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("eoCleanPauseTransition(%q,%q) = (%q,%v), want (%q,%v)",
				tc.current, tc.action, got, ok, tc.want, tc.ok)
		}
	}
}

// TestEOCleanPauseEndpoint: pause flips a running job to paused; pausing a
// done job returns 409 (guarded UPDATE matched no row, but the job exists).
func TestEOCleanPauseEndpoint(t *testing.T) {
	r, mock, closeFn := newEOCleanTestRouter(t)
	defer closeFn()

	jobID := "11111111-2222-3333-4444-777777777777"
	mock.ExpectQuery(`UPDATE mailing_eo_clean_jobs SET status = \$3`).
		WillReturnRows(sqlmock.NewRows(eoCleanTestJobCols).AddRow(
			jobID, "segment", "seg-1", "L", "paused",
			10, 0, 5, 5, 3, 1, 1, 0, 0, 0, "", eoCleanTestNow, nil))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, eoCleanRequest("POST", "/eo-clean/jobs/"+jobID+"/pause", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("pause status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Job eoCleanJobJSON `json:"job"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Job.Status != "paused" {
		t.Fatalf("status = %q, want paused", resp.Job.Status)
	}

	// Done job: transition UPDATE matches nothing → existence probe → 409.
	mock.ExpectQuery(`UPDATE mailing_eo_clean_jobs SET status = \$3`).
		WillReturnRows(sqlmock.NewRows(eoCleanTestJobCols)) // no row
	mock.ExpectQuery(`SELECT 1 FROM mailing_eo_clean_jobs`).
		WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, eoCleanRequest("POST", "/eo-clean/jobs/"+jobID+"/pause", nil))
	if w2.Code != http.StatusConflict {
		t.Fatalf("pause-done status = %d, want 409; body = %s", w2.Code, w2.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestEOCleanCreateRejectsBadSource pins input validation: unknown
// source_type and non-uuid source_ref are 400s that never touch the DB.
func TestEOCleanCreateRejectsBadSource(t *testing.T) {
	r, mock, closeFn := newEOCleanTestRouter(t)
	defer closeFn()

	for _, body := range []map[string]interface{}{
		{"source_type": "csv", "emails": []string{"a@b.co"}},
		{"source_type": "segment", "source_ref": "not-a-uuid"},
		{"source_type": "upload", "emails": []string{}},
		{"source_type": "upload", "emails": []string{"a@b.co"}, "daily_cap": -5},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, eoCleanRequest("POST", "/eo-clean/jobs", body))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %v: status = %d, want 400 (%s)", body, w.Code, w.Body.String())
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("validation rejects must never touch the DB: %v", err)
	}
}
