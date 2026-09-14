package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"

	"github.com/ignite/sparkpost-monitor/internal/brain"
)

const brainTestOrg = "00000000-0000-0000-0000-000000000001"

func brainTestServer(t *testing.T) (*chi.Mux, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	svc := NewBrainService(brain.NewStore(db), nil)
	root := chi.NewRouter()
	root.Route("/api/mailing", func(r chi.Router) { svc.RegisterRoutes(r) })
	svc.RegisterAdminRoutes(root)
	return root, mock, func() { db.Close() }
}

// Negative path (platform-work Part 4 item 5): an agent cannot activate a
// policy claim — brain.RequiredVerifier says operator only.
func TestBrainVerifyPolicyByAgentIsForbidden(t *testing.T) {
	srv, mock, done := brainTestServer(t)
	defer done()
	mock.ExpectQuery(regexp.QuoteMeta("FROM jarvis_brain_claims c WHERE c.id=$1 AND c.org_id=$2")).
		WithArgs(int64(7), brainTestOrg).
		WillReturnRows(sqlmock.NewRows([]string{"id", "org_id", "claim_type", "authority", "title", "body", "scope", "status",
			"effective_from", "effective_until", "supersedes", "superseded_by", "source", "source_ref", "created_by",
			"created_at", "updated_at", "verified_at", "verified_by", "evidence_count"}).
			AddRow(7, brainTestOrg, "policy", "operator", "t", "b", "{}", "candidate",
				nil, nil, nil, nil, "session", nil, "claude", time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC), nil, nil, 0))
	body, _ := json.Marshal(map[string]any{"verifier_role": "agent", "verifier_name": "x",
		"evidence": map[string]any{"checker": "q", "result": "r", "environment": "repo"}})
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/brain/claims/7/verify", bytes.NewReader(body))
	req.Header.Set("X-Organization-ID", brainTestOrg)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Negative path: on the admin (console) path, verifier_role=operator without
// the operator key is refused BEFORE any DB access.
func TestBrainAdminOperatorVerifyNeedsOperatorKey(t *testing.T) {
	os.Setenv("ADMIN_API_KEY", "adm")
	os.Setenv("BRAIN_OPERATOR_KEY", "opk")
	defer os.Unsetenv("ADMIN_API_KEY")
	defer os.Unsetenv("BRAIN_OPERATOR_KEY")
	srv, _, done := brainTestServer(t)
	defer done()
	body, _ := json.Marshal(map[string]any{"verifier_role": "operator", "verifier_name": "drisan",
		"evidence": map[string]any{"checker": "q", "result": "r", "environment": "operator"}})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/brain/claims/7/verify", bytes.NewReader(body))
	req.Header.Set("X-Organization-ID", brainTestOrg)
	req.Header.Set("X-Admin-Key", "adm")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 without X-Brain-Operator-Key, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Admin path is closed by default: no ADMIN_API_KEY ⇒ 401 on every route.
func TestBrainAdminPathClosedWithoutKey(t *testing.T) {
	os.Unsetenv("ADMIN_API_KEY")
	srv, _, done := brainTestServer(t)
	defer done()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/brain/stats", nil)
	req.Header.Set("X-Organization-ID", brainTestOrg)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

// Evidence without a result is not evidence (store validation surfaces as 400).
func TestBrainEvidenceRequiresResult(t *testing.T) {
	srv, mock, done := brainTestServer(t)
	defer done()
	_ = mock
	body, _ := json.Marshal(map[string]any{"checker": "SELECT 1", "result": "", "environment": "prod-pg-us-west-2"})
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/brain/claims/7/evidence", bytes.NewReader(body))
	req.Header.Set("X-Organization-ID", brainTestOrg)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// An eval carrying a write can never be stored.
func TestBrainEvalRejectsWriteSQL(t *testing.T) {
	srv, _, done := brainTestServer(t)
	defer done()
	body, _ := json.Marshal(map[string]any{"name": "bad_eval", "question": "q", "checker": "pg_sql",
		"spec": map[string]any{"query": "DELETE FROM mailing_campaigns"}, "expected": []any{}})
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/brain/evals/", bytes.NewReader(body))
	req.Header.Set("X-Organization-ID", brainTestOrg)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

var _ = sql.ErrNoRows

// A source_ref that already names a DIFFERENT claim is refused (400), not
// silently overwritten — the 2026-09-13 #759 collision.
func TestBrainRecordRefusesSourceRefReuseForDifferentClaim(t *testing.T) {
	srv, mock, done := brainTestServer(t)
	defer done()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT title FROM jarvis_brain_claims WHERE org_id=$1 AND source=$2 AND source_ref=$3")).
		WithArgs(brainTestOrg, "session", "ref-1").
		WillReturnRows(sqlmock.NewRows([]string{"title"}).AddRow("an unrelated existing claim"))
	mock.ExpectRollback()
	body, _ := json.Marshal(map[string]any{"claim_type": "historical_finding", "title": "a new fact", "body": "b",
		"source": "session", "source_ref": "ref-1"})
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/brain/claims/", bytes.NewReader(body))
	req.Header.Set("X-Organization-ID", brainTestOrg)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "already names a different claim") {
		t.Fatalf("want 400 source_ref conflict, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Observations: a batch with one bad row is rejected before any write (no
// DB expectation set — sqlmock would fail the test on an unexpected call).
func TestBrainObservationsRejectBadRowBeforeWrite(t *testing.T) {
	os.Setenv("ADMIN_API_KEY", "adm")
	defer os.Unsetenv("ADMIN_API_KEY")
	srv, _, done := brainTestServer(t)
	defer done()
	body, _ := json.Marshal(map[string]any{"observations": []map[string]any{
		{"metric": "lake.delivered", "grain": "all", "day": "2026-09-13", "value": 1, "unit": "count", "source": "t"},
		{"metric": "BAD METRIC", "grain": "all", "day": "2026-09-13", "value": 1, "unit": "count", "source": "t"},
	}})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/brain/observations/", bytes.NewReader(body))
	req.Header.Set("X-Admin-Key", "adm")
	req.Header.Set("X-Organization-ID", brainTestOrg)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "row 1") {
		t.Fatalf("want 400 naming row 1, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Observations: a valid batch is written in one transaction with upsert
// semantics and the count comes back.
func TestBrainObservationsUpsertBatch(t *testing.T) {
	os.Setenv("ADMIN_API_KEY", "adm")
	defer os.Unsetenv("ADMIN_API_KEY")
	srv, mock, done := brainTestServer(t)
	defer done()
	mock.ExpectBegin()
	mock.ExpectPrepare(regexp.QuoteMeta("INSERT INTO jarvis_brain_observations")).
		ExpectExec().WithArgs(brainTestOrg, "lake.delivered", "isp:gmail", "2026-09-13", 12345.0, "count", "observer", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	body, _ := json.Marshal(map[string]any{"observations": []map[string]any{
		{"metric": "lake.delivered", "grain": "isp:gmail", "day": "2026-09-13", "value": 12345, "unit": "count", "source": "observer"},
	}})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/brain/observations/", bytes.NewReader(body))
	req.Header.Set("X-Admin-Key", "adm")
	req.Header.Set("X-Organization-ID", brainTestOrg)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"written":1`) {
		t.Fatalf("want 201 written=1, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
