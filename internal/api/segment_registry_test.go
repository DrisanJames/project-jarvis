package api

// REQ-C15 tests — segment-family ownership registry CRUD + freshness.
// sqlmock only (no DB); org scoping via X-Organization-ID header.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
)

const segRegTestOrg = "11111111-2222-3333-4444-555555555555"

func segRegRouter(db *SegmentRegistryService) *chi.Mux {
	r := chi.NewRouter()
	db.RegisterRoutes(r)
	return r
}

func segRegCols() []string {
	return []string{"id", "family_key", "family_pattern", "definition_source", "owner",
		"cadence", "sla_hours", "keep_policy", "heartbeat_worker", "notes", "active",
		"created_at", "updated_at"}
}

func TestSegmentRegistryList(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Now()
	mock.ExpectQuery(`FROM mailing_segment_registry`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(segRegCols()).
			AddRow("aaaaaaaa-0000-0000-0000-000000000001", "slot", "SLOT-%", "slot_ledger",
				"board program", "daily", 48, "protect", "", "", true, now, now))

	svc := NewSegmentRegistryService(db)
	req := httptest.NewRequest(http.MethodGet, "/segment-registry/", nil)
	req.Header.Set("X-Organization-ID", segRegTestOrg)
	rec := httptest.NewRecorder()
	segRegRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Families []SegmentRegistryRow `json:"families"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Families) != 1 || out.Families[0].FamilyPattern != "SLOT-%" {
		t.Fatalf("unexpected families: %+v", out.Families)
	}
	if out.Families[0].KeepPolicy != "protect" {
		t.Fatalf("keep_policy = %q", out.Families[0].KeepPolicy)
	}
}

func TestSegmentRegistryCreate_ValidationRejectsUnowned(t *testing.T) {
	// Negative path (AS-7.1): a write without an owner is refused — an unowned
	// family is the exact defect the registry exists to kill.
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	svc := NewSegmentRegistryService(db)
	body := `{"family_key":"newfam","family_pattern":"NEWFAM-%","keep_policy":"protect"}`
	req := httptest.NewRequest(http.MethodPost, "/segment-registry/", strings.NewReader(body))
	req.Header.Set("X-Organization-ID", segRegTestOrg)
	rec := httptest.NewRecorder()
	segRegRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "owner is required") {
		t.Fatalf("expected owner-required error, got %s", rec.Body.String())
	}
}

func TestSegmentRegistryCreate_RejectsBadKeepPolicy(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	svc := NewSegmentRegistryService(db)
	body := `{"family_key":"f","family_pattern":"F-%","owner":"me","keep_policy":"delete_everything"}`
	req := httptest.NewRequest(http.MethodPost, "/segment-registry/", strings.NewReader(body))
	req.Header.Set("X-Organization-ID", segRegTestOrg)
	rec := httptest.NewRecorder()
	segRegRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestSegmentRegistryCreate_OK(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Now()
	mock.ExpectQuery(`INSERT INTO mailing_segment_registry`).
		WillReturnRows(sqlmock.NewRows(segRegCols()).
			AddRow("aaaaaaaa-0000-0000-0000-000000000002", "newfam", "NEWFAM-%", "script",
				"agents.jobs newfam_refresh", "nightly", 24, "protect", "job:newfam_refresh", "", true, now, now))

	svc := NewSegmentRegistryService(db)
	body := `{"family_key":"newfam","family_pattern":"NEWFAM-%","owner":"agents.jobs newfam_refresh","cadence":"nightly","sla_hours":24,"heartbeat_worker":"job:newfam_refresh"}`
	req := httptest.NewRequest(http.MethodPost, "/segment-registry/", strings.NewReader(body))
	req.Header.Set("X-Organization-ID", segRegTestOrg)
	rec := httptest.NewRecorder()
	segRegRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestSegmentRegistryFreshness_SLAStates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	cols := []string{"id", "family_key", "family_pattern", "owner", "cadence",
		"sla_hours", "keep_policy", "active", "segments_count", "members_estimate",
		"newest_refresh_at", "oldest_refresh_at", "heartbeat_worker",
		"last_beat_at", "last_status", "stalled"}
	fresh := time.Now().Add(-2 * time.Hour)
	stale := time.Now().Add(-80 * time.Hour)
	beat := time.Now().Add(-30 * time.Minute)
	mock.ExpectQuery(`FROM mailing_segment_registry reg`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(cols).
			// in-SLA family with a live heartbeat
			AddRow("aaaaaaaa-0000-0000-0000-000000000001", "slot", "SLOT-%", "board", "daily",
				48, "protect", true, 2060, int64(6630000), fresh, stale, "verified_humans_ledger",
				beat, "ok", false).
			// SLA-breached family (newest older than sla_hours)
			AddRow("aaaaaaaa-0000-0000-0000-000000000002", "fresh", "FRESH-%", "hydrator", "daily",
				48, "protect", true, 40, int64(120000), stale, stale, "", nil, nil, nil).
			// NEVER-BUILT family (null newest) with an SLA → breached
			AddRow("aaaaaaaa-0000-0000-0000-000000000003", "welcome_saturated", "Welcome-Saturated%", "cron", "daily",
				48, "protect", true, 0, int64(0), nil, nil, "", nil, nil, nil).
			// no-SLA family
			AddRow("aaaaaaaa-0000-0000-0000-000000000004", "excl", "EXCL%", "registry", "",
				0, "protect", true, 12, int64(400000), fresh, fresh, "", nil, nil, nil))

	svc := NewSegmentRegistryService(db)
	req := httptest.NewRequest(http.MethodGet, "/segment-registry/freshness", nil)
	req.Header.Set("X-Organization-ID", segRegTestOrg)
	rec := httptest.NewRecorder()
	segRegRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Families []SegmentRegistryFreshnessRow `json:"families"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Families) != 4 {
		t.Fatalf("families = %d, want 4", len(out.Families))
	}
	wantStates := []string{"ok", "breached", "breached", "no_sla"}
	for i, want := range wantStates {
		if out.Families[i].SLAState != want {
			t.Errorf("family %d (%s) sla_state = %q, want %q",
				i, out.Families[i].FamilyKey, out.Families[i].SLAState, want)
		}
	}
	// Never-built must be distinguishable from built-empty: newest_refresh_at
	// is null AND segments_count is 0.
	if out.Families[2].NewestRefreshAt != nil {
		t.Errorf("never-built family must have null newest_refresh_at")
	}
	if out.Families[0].Heartbeat == nil || out.Families[0].Heartbeat.LastStatus != "ok" {
		t.Errorf("expected joined heartbeat on family 0: %+v", out.Families[0].Heartbeat)
	}
}
