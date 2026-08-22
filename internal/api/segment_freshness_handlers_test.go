package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Freshness contract: fresh <26h · aging 26-48h · stale >48h · unknown when
// there is NO verified measurement — and unknown is never fresh or zero.
// ---------------------------------------------------------------------------

func TestComputeSegmentFreshness(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	at := func(age time.Duration) *time.Time {
		v := now.Add(-age)
		return &v
	}
	cases := []struct {
		name string
		in   *time.Time
		want string
	}{
		{"no measurement", nil, "unknown"},
		{"just built", at(1 * time.Hour), "fresh"},
		{"25h59m", at(25*time.Hour + 59*time.Minute), "fresh"},
		{"exactly 26h", at(26 * time.Hour), "aging"},
		{"47h", at(47 * time.Hour), "aging"},
		{"exactly 48h", at(48 * time.Hour), "aging"},
		{"49h", at(49 * time.Hour), "stale"},
		{"a week", at(7 * 24 * time.Hour), "stale"},
	}
	for _, c := range cases {
		if got := computeSegmentFreshness(c.in, now); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Handler harness
// ---------------------------------------------------------------------------

func newFreshnessRouter(db *SegmentFreshnessService) *chi.Mux {
	r := chi.NewRouter()
	db.RegisterRoutes(r)
	return r
}

// TestHandleSegmentFreshness_UnknownWhenNeverMeasured: a grid segment with
// no ledger row (or a ledger row with no successful build) must render
// freshness 'unknown' with member_count null — never zero, never fresh.
func TestHandleSegmentFreshness_UnknownWhenNeverMeasured(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewSegmentFreshnessService(db)

	segID := uuid.New().String()
	mock.ExpectQuery(`FROM mailing_segments s`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "name", "status", "subscriber_count", "last_built_at",
			"build_source", "last_build_status", "last_error", "updated_at",
		}).AddRow(segID, "DB 30D Openers", "active", nil, nil, "", "", "", nil))
	// Member stamps: the bounded read FAILS (timeout on a strained primary)
	// → members_stamped_at must degrade to null, not to a guess.
	mock.ExpectQuery(`FROM mailing_segment_members`).
		WillReturnError(errContextDeadline{})
	mock.ExpectQuery(`FROM mailing_segment_refresh_requests`).
		WillReturnRows(sqlmock.NewRows([]string{"segment_id", "status"}))
	mock.ExpectQuery(`FROM mailing_worker_runs`).
		WillReturnRows(sqlmock.NewRows([]string{"started_at", "status"}))
	mock.ExpectQuery(`FROM mailing_worker_heartbeats`).
		WillReturnRows(sqlmock.NewRows([]string{"last_status"}))

	req := httptest.NewRequest(http.MethodGet, "/segments/freshness", nil)
	req.Header.Set("X-Organization-ID", uuid.New().String())
	rec := httptest.NewRecorder()
	newFreshnessRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp segmentFreshnessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(resp.Rows))
	}
	row := resp.Rows[0]
	if row.Freshness != "unknown" {
		t.Fatalf("never-measured segment must be 'unknown', got %q", row.Freshness)
	}
	if row.MemberCount != nil {
		t.Fatalf("never-measured segment must have null member_count, got %d", *row.MemberCount)
	}
	if row.MembersStampedAt != nil {
		t.Fatalf("failed stamp read must yield null members_stamped_at")
	}
	if row.Brand != "DB" || row.WindowDays != 30 || row.Kind != "openers" {
		t.Fatalf("name parse wrong: %+v", row)
	}
}

// errContextDeadline mimics a query timeout.
type errContextDeadline struct{}

func (errContextDeadline) Error() string { return "pq: canceling statement due to statement timeout" }

// TestHandleSegmentFreshness_VerifiedBuildIsMeasured pins the positive band.
func TestHandleSegmentFreshness_VerifiedBuildIsMeasured(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewSegmentFreshnessService(db)

	segID := uuid.New().String()
	builtAt := time.Now().UTC().Add(-3 * time.Hour)
	stampAt := time.Now().UTC().Add(-3 * time.Hour)
	mock.ExpectQuery(`FROM mailing_segments s`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "name", "status", "subscriber_count", "last_built_at",
			"build_source", "last_build_status", "last_error", "updated_at",
		}).AddRow(segID, "AAD 60D Clickers", "active", 15722, builtAt, "lake-grid", "ok", "", builtAt))
	mock.ExpectQuery(`FROM mailing_segment_members`).
		WillReturnRows(sqlmock.NewRows([]string{"segment_id", "max"}).AddRow(segID, stampAt))
	mock.ExpectQuery(`FROM mailing_segment_refresh_requests`).
		WillReturnRows(sqlmock.NewRows([]string{"segment_id", "status"}).AddRow(segID, "queued"))
	mock.ExpectQuery(`FROM mailing_worker_runs`).
		WillReturnRows(sqlmock.NewRows([]string{"started_at", "status"}).
			AddRow(time.Now().UTC().Add(-2*time.Hour), "ok"))
	mock.ExpectQuery(`FROM mailing_worker_heartbeats`).
		WillReturnRows(sqlmock.NewRows([]string{"last_status"}).AddRow("ok"))

	req := httptest.NewRequest(http.MethodGet, "/segments/freshness", nil)
	req.Header.Set("X-Organization-ID", uuid.New().String())
	rec := httptest.NewRecorder()
	newFreshnessRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp segmentFreshnessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	row := resp.Rows[0]
	if row.Freshness != "fresh" {
		t.Fatalf("3h-old verified build must be 'fresh', got %q", row.Freshness)
	}
	if row.MemberCount == nil || *row.MemberCount != 15722 {
		t.Fatalf("member_count must come from the ledger, got %v", row.MemberCount)
	}
	if row.MembersStampedAt == nil {
		t.Fatal("members_stamped_at must be populated from the real read")
	}
	if row.RefreshState == nil || *row.RefreshState != "queued" {
		t.Fatalf("refresh_state must surface the open request, got %v", row.RefreshState)
	}
	if row.Brand != "AAD" || row.WindowDays != 60 || row.Kind != "clickers" {
		t.Fatalf("name parse wrong: %+v", row)
	}
	if resp.Worker.LastPassAt == nil || resp.Worker.LastPassOutcome != "ok" {
		t.Fatalf("worker block must carry the cluster-wide last run: %+v", resp.Worker)
	}
}

// ---------------------------------------------------------------------------
// NEGATIVE PATH: a queued duplicate is reported, never double-inserted
// ---------------------------------------------------------------------------

func TestHandleSegmentRefreshRequest_DuplicateNotDoubleInserted(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewSegmentFreshnessService(db)

	segID := uuid.New().String()
	mock.ExpectQuery(`FROM mailing_segments`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status"}).
			AddRow(segID, "DB 30D Openers", "active"))
	// ON CONFLICT ... DO NOTHING: the open-slot unique index absorbs the
	// duplicate — 0 rows inserted.
	mock.ExpectExec(`INSERT INTO mailing_segment_refresh_requests`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	body := `{"segment_ids":["` + segID + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/segments/refresh", strings.NewReader(body))
	req.Header.Set("X-Organization-ID", uuid.New().String())
	rec := httptest.NewRecorder()
	newFreshnessRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp segmentRefreshResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(resp.Queued) != 0 {
		t.Fatalf("duplicate must not report queued, got %+v", resp.Queued)
	}
	if len(resp.AlreadyPending) != 1 || resp.AlreadyPending[0] != segID {
		t.Fatalf("duplicate must be reported as already_pending, got %+v", resp.AlreadyPending)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("exactly one insert attempt expected: %v", err)
	}
}

func TestHandleSegmentRefreshRequest_QueuesNewRequest(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewSegmentFreshnessService(db)

	segID := uuid.New().String()
	mock.ExpectQuery(`FROM mailing_segments`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status"}).
			AddRow(segID, "DB 30D Openers", "active"))
	mock.ExpectExec(`INSERT INTO mailing_segment_refresh_requests`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"segment_ids":["` + segID + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/segments/refresh", strings.NewReader(body))
	req.Header.Set("X-Organization-ID", uuid.New().String())
	rec := httptest.NewRecorder()
	newFreshnessRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp segmentRefreshResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(resp.Queued) != 1 || resp.Queued[0].SegmentID != segID || resp.Queued[0].RequestID == "" {
		t.Fatalf("expected one queued entry with a request id, got %+v", resp.Queued)
	}
}

func TestHandleSegmentRefreshRequest_RejectsEmptyBody(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewSegmentFreshnessService(db)

	req := httptest.NewRequest(http.MethodPost, "/segments/refresh", strings.NewReader(`{}`))
	req.Header.Set("X-Organization-ID", uuid.New().String())
	rec := httptest.NewRecorder()
	newFreshnessRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body must 400, got %d", rec.Code)
	}
}

// Org scoping: a segment outside the caller's org is reported as skipped
// (not found in this organization), never queued.
func TestHandleSegmentRefreshRequest_OrgScoped(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewSegmentFreshnessService(db)

	foreignSeg := uuid.New().String()
	// Lookup constrained by organization_id returns no rows.
	mock.ExpectQuery(`FROM mailing_segments`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status"}))

	body := `{"segment_ids":["` + foreignSeg + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/segments/refresh", strings.NewReader(body))
	req.Header.Set("X-Organization-ID", uuid.New().String())
	rec := httptest.NewRecorder()
	newFreshnessRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp segmentRefreshResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(resp.Queued) != 0 || len(resp.AlreadyPending) != 0 {
		t.Fatalf("foreign segment must not be queued: %+v", resp)
	}
	if len(resp.Skipped) != 1 || resp.Skipped[0].Reason != "not found in this organization" {
		t.Fatalf("foreign segment must be skipped with the org reason, got %+v", resp.Skipped)
	}
}
