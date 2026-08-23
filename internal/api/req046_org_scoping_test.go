package api

// REQ-046 — segment surfaces are org-scoped. These tests pin the ownership
// gates added across the segment read/export/execute/delete surfaces and the
// deploy-path segment-ownership validation:
//
//   DoD-1: ExportSegmentMembersCSV / GetSegmentSubscribers / ExecuteSegment /
//          ExportSegmentsUnionCSV / GetSnapshot / GetSnapshotSubscribers
//          resolve org and 404 on a foreign-org UUID (no existence leak),
//          200 on a same-org one.
//   DoD-2: HandleDeleteSegment is org-scoped, checks its Exec error, cleans
//          member rows, and refuses (409) when a live campaign references
//          the segment.
//   DoD-3: deployFromInput validates inclusion/exclusion/send_priority
//          segment ownership → foreign segment payload = HTTP 400.
//   DoD-4: HandleRefreshAllSegments is closed behind X-Admin-Key;
//          HandleJourneySegments filters by organization_id.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// req046SegmentRow builds a full sqlmock row for segmentation.Store.GetSegment
// (22 columns — see internal/segmentation/store.go GetSegment).
func req046SegmentRow(segID, orgID uuid.UUID, name, conditions string) *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows([]string{
		"id", "organization_id", "list_id", "name", "description", "segment_type", "conditions",
		"category", "calculation_mode", "refresh_interval_minutes", "include_suppressed",
		"global_exclusion_rules", "subscriber_count", "last_calculated_at", "status",
		"is_system", "system_query", "created_by", "last_edited_by", "last_edited_at",
		"created_at", "updated_at",
	}).AddRow(
		segID.String(), orgID.String(), nil, name, "", "static", []byte(conditions),
		"uncategorized", "batch", 0, false,
		[]byte("[]"), 3, nil, "active",
		false, "", nil, nil, nil,
		now, now,
	)
}

// req046SnapshotRow builds a full sqlmock row for segmentation.Store.GetSnapshot.
func req046SnapshotRow(snapID, segID, orgID uuid.UUID) *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows([]string{
		"id", "segment_id", "organization_id", "name", "description", "conditions_snapshot",
		"subscriber_count", "subscriber_ids", "query_hash", "purpose", "campaign_id",
		"created_by", "snapshot_at", "expires_at", "created_at",
	}).AddRow(
		snapID.String(), segID.String(), orgID.String(), "snap", "", []byte("[]"),
		0, []byte("[]"), "", "manual", nil,
		nil, now, nil, now,
	)
}

func req046Router(t *testing.T) (sqlmock.Sqlmock, chi.Router, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	api := NewSegmentationAPI(db)
	router := chi.NewRouter()
	api.RegisterRoutes(router)
	return mock, router, func() { db.Close() }
}

// --- DoD-1: single-segment member export ------------------------------------

func TestExportSegmentMembersCSV_ForeignOrgIs404(t *testing.T) {
	mock, router, done := req046Router(t)
	defer done()

	segID := uuid.New()
	callerOrg := uuid.New()

	// Org-scoped GetSegment finds nothing for (segID, callerOrg).
	mock.ExpectQuery("SELECT ms.id, ms.organization_id, ms.list_id, ms.name,").
		WithArgs(segID, callerOrg).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	req := httptest.NewRequest(http.MethodGet, "/v2/segments/"+segID.String()+"/members.csv", nil)
	req.Header.Set("X-Organization-ID", callerOrg.String())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign-org export: expected 404, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "@") {
		t.Fatalf("foreign-org export leaked member data: %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestExportSegmentMembersCSV_SameOrgIs200(t *testing.T) {
	mock, router, done := req046Router(t)
	defer done()

	segID := uuid.New()
	orgID := uuid.New()

	mock.ExpectQuery("SELECT ms.id, ms.organization_id, ms.list_id, ms.name,").
		WithArgs(segID, orgID).
		WillReturnRows(req046SegmentRow(segID, orgID, "My Segment", "[]"))
	// Default dedupe=true path: DISTINCT LOWER(email) stream.
	mock.ExpectQuery("SELECT DISTINCT LOWER").
		WithArgs(segID).
		WillReturnRows(sqlmock.NewRows([]string{"email"}).
			AddRow("a@example.com").
			AddRow("b@example.com"))

	req := httptest.NewRequest(http.MethodGet, "/v2/segments/"+segID.String()+"/members.csv", nil)
	req.Header.Set("X-Organization-ID", orgID.String())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("same-org export: expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "a@example.com") || !strings.Contains(body, "b@example.com") {
		t.Fatalf("same-org export missing member rows: %s", body)
	}
	if got := rec.Header().Get("X-Segment-Name"); got != "My Segment" {
		t.Fatalf("expected X-Segment-Name from the org-scoped row, got %q", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

// --- DoD-1: subscribers + execute --------------------------------------------

func TestGetSegmentSubscribers_ForeignOrgIs404(t *testing.T) {
	mock, router, done := req046Router(t)
	defer done()

	segID := uuid.New()
	callerOrg := uuid.New()

	mock.ExpectQuery("SELECT ms.id, ms.organization_id, ms.list_id, ms.name,").
		WithArgs(segID, callerOrg).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	req := httptest.NewRequest(http.MethodGet, "/v2/segments/"+segID.String()+"/subscribers", nil)
	req.Header.Set("X-Organization-ID", callerOrg.String())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign-org subscribers: expected 404, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestGetSegmentSubscribers_SameOrgIs200_MaterializedPath(t *testing.T) {
	mock, router, done := req046Router(t)
	defer done()

	segID := uuid.New()
	orgID := uuid.New()
	lakeConditions := `{"lake_spec":{"event":"click","window_days":30,"scope":"global"}}`

	mock.ExpectQuery("SELECT ms.id, ms.organization_id, ms.list_id, ms.name,").
		WithArgs(segID, orgID).
		WillReturnRows(req046SegmentRow(segID, orgID, "Lake Segment", lakeConditions))
	// segmentIsLakeBuilt re-reads conditions, then the materialized read runs.
	mock.ExpectQuery("conditions::text").
		WithArgs(segID).
		WillReturnRows(sqlmock.NewRows([]string{"conditions"}).AddRow(lakeConditions))
	mock.ExpectQuery("SELECT COUNT").
		WithArgs(segID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("SELECT subscriber_id FROM mailing_segment_members").
		WithArgs(segID, 100, 0).
		WillReturnRows(sqlmock.NewRows([]string{"subscriber_id"}).AddRow(uuid.New().String()))

	req := httptest.NewRequest(http.MethodGet, "/v2/segments/"+segID.String()+"/subscribers", nil)
	req.Header.Set("X-Organization-ID", orgID.String())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("same-org subscribers: expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestExecuteSegment_ForeignOrgIs404(t *testing.T) {
	mock, router, done := req046Router(t)
	defer done()

	segID := uuid.New()
	callerOrg := uuid.New()

	mock.ExpectQuery("SELECT ms.id, ms.organization_id, ms.list_id, ms.name,").
		WithArgs(segID, callerOrg).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	req := httptest.NewRequest(http.MethodPost, "/v2/segments/"+segID.String()+"/execute", nil)
	req.Header.Set("X-Organization-ID", callerOrg.String())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign-org execute: expected 404, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

// --- DoD-1: union export ------------------------------------------------------

func TestExportSegmentsUnionCSV_ForeignOrgIs404(t *testing.T) {
	mock, router, done := req046Router(t)
	defer done()

	callerOrg := uuid.New()
	foreignSeg := uuid.New()

	// Ownership count: none of the requested ids belong to the caller's org.
	mock.ExpectQuery("FROM mailing_segments WHERE id = ANY").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	body, _ := json.Marshal(map[string]any{"segment_ids": []string{foreignSeg.String()}})
	req := httptest.NewRequest(http.MethodPost, "/v2/segments/export.csv", bytes.NewReader(body))
	req.Header.Set("X-Organization-ID", callerOrg.String())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign-org union export: expected 404, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestExportSegmentsUnionCSV_SameOrgIs200(t *testing.T) {
	mock, router, done := req046Router(t)
	defer done()

	orgID := uuid.New()
	segA := uuid.New()
	segB := uuid.New()

	mock.ExpectQuery("FROM mailing_segments WHERE id = ANY").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery("SELECT DISTINCT LOWER").
		WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow("a@example.com"))

	body, _ := json.Marshal(map[string]any{"segment_ids": []string{segA.String(), segB.String()}})
	req := httptest.NewRequest(http.MethodPost, "/v2/segments/export.csv", bytes.NewReader(body))
	req.Header.Set("X-Organization-ID", orgID.String())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("same-org union export: expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "a@example.com") {
		t.Fatalf("union export missing rows: %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

// --- DoD-1: snapshots ----------------------------------------------------------

func TestGetSnapshot_ForeignOrgIs404(t *testing.T) {
	mock, router, done := req046Router(t)
	defer done()

	snapID := uuid.New()
	snapOrg := uuid.New()
	callerOrg := uuid.New()

	mock.ExpectQuery("FROM mailing_segment_snapshots").
		WithArgs(snapID).
		WillReturnRows(req046SnapshotRow(snapID, uuid.New(), snapOrg))

	req := httptest.NewRequest(http.MethodGet, "/v2/snapshots/"+snapID.String()+"/", nil)
	req.Header.Set("X-Organization-ID", callerOrg.String())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign-org snapshot: expected 404, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestGetSnapshotSubscribers_ForeignOrgIs404(t *testing.T) {
	mock, router, done := req046Router(t)
	defer done()

	snapID := uuid.New()
	snapOrg := uuid.New()
	callerOrg := uuid.New()

	mock.ExpectQuery("FROM mailing_segment_snapshots").
		WithArgs(snapID).
		WillReturnRows(req046SnapshotRow(snapID, uuid.New(), snapOrg))

	req := httptest.NewRequest(http.MethodGet, "/v2/snapshots/"+snapID.String()+"/subscribers", nil)
	req.Header.Set("X-Organization-ID", callerOrg.String())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign-org snapshot subscribers: expected 404, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestGetSnapshot_SameOrgIs200(t *testing.T) {
	mock, router, done := req046Router(t)
	defer done()

	snapID := uuid.New()
	orgID := uuid.New()

	mock.ExpectQuery("FROM mailing_segment_snapshots").
		WithArgs(snapID).
		WillReturnRows(req046SnapshotRow(snapID, uuid.New(), orgID))

	req := httptest.NewRequest(http.MethodGet, "/v2/snapshots/"+snapID.String()+"/", nil)
	req.Header.Set("X-Organization-ID", orgID.String())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("same-org snapshot: expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

// --- DoD-2: HandleDeleteSegment --------------------------------------------------

func req046DeleteRouter(t *testing.T) (sqlmock.Sqlmock, chi.Router, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	svc := NewAdvancedMailingService(db)
	router := chi.NewRouter()
	router.Delete("/segments/{segmentId}", svc.HandleDeleteSegment)
	return mock, router, func() { db.Close() }
}

func TestHandleDeleteSegment_ForeignOrgIs404(t *testing.T) {
	mock, router, done := req046DeleteRouter(t)
	defer done()

	segID := uuid.New()
	callerOrg := uuid.New()

	// No live campaign in the caller's org references it…
	mock.ExpectQuery("FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	// …and the org-scoped DELETE touches zero rows (the segment belongs to
	// another org) → 404, and the member cleanup must NOT run.
	mock.ExpectExec("DELETE FROM mailing_segments WHERE id = ").
		WithArgs(segID.String(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	req := httptest.NewRequest(http.MethodDelete, "/segments/"+segID.String(), nil)
	req.Header.Set("X-Organization-ID", callerOrg.String())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign-org delete: expected 404, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations (member cleanup must not have run): %v", err)
	}
}

func TestHandleDeleteSegment_RefusesWhenReferencedByLiveCampaign(t *testing.T) {
	mock, router, done := req046DeleteRouter(t)
	defer done()

	segID := uuid.New()
	orgID := uuid.New()

	mock.ExpectQuery("FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	req := httptest.NewRequest(http.MethodDelete, "/segments/"+segID.String(), nil)
	req.Header.Set("X-Organization-ID", orgID.String())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("referenced delete: expected 409, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "referenced by 2 active campaign") {
		t.Fatalf("expected reference-count error, got %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations (DELETE must not have run): %v", err)
	}
}

func TestHandleDeleteSegment_SameOrgDeletesAndCleansMembers(t *testing.T) {
	mock, router, done := req046DeleteRouter(t)
	defer done()

	segID := uuid.New()
	orgID := uuid.New()

	mock.ExpectQuery("FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("DELETE FROM mailing_segments WHERE id = ").
		WithArgs(segID.String(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM mailing_segment_members WHERE segment_id = ").
		WithArgs(segID.String()).
		WillReturnResult(sqlmock.NewResult(0, 42))

	req := httptest.NewRequest(http.MethodDelete, "/segments/"+segID.String(), nil)
	req.Header.Set("X-Organization-ID", orgID.String())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("same-org delete: expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if payload["deleted"] != segID.String() {
		t.Fatalf("expected deleted=%s, got %#v", segID, payload["deleted"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

// --- DoD-3: deploy-path segment ownership -----------------------------------------

func req046DeployInput(name string, inclusionSegments []string) engine.PMTACampaignInput {
	scheduledAt := time.Now().UTC().Add(30 * time.Minute).Round(time.Minute)
	return engine.PMTACampaignInput{
		OfferID:       "0d0d0d0d-0d0d-40d0-80d0-0d0d0d0d0d0d",
		Name:          name,
		TargetISPs:    []engine.ISP{engine.ISPGmail},
		SendingDomain: "mail.example.com",
		Variants: []engine.ContentVariant{{
			VariantName: "A",
			Subject:     "Subject",
			HTMLContent: "<html></html>",
		}},
		SendMode:          "scheduled",
		ScheduledAt:       &scheduledAt,
		Timezone:          "UTC",
		InclusionSegments: inclusionSegments,
	}
}

func TestHandleDeployCampaign_ForeignSegmentIs400(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	foreignSeg := uuid.New().String()

	// Ownership check finds no row for (segment, org) → deploy refused
	// BEFORE any campaign row is reserved (no Begin/INSERT expectations).
	mock.ExpectQuery("SELECT id::text FROM mailing_segments WHERE id = ANY").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	body, _ := json.Marshal(req046DeployInput("Foreign Segment Deploy", []string{foreignSeg}))
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	service.HandleDeployCampaign(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("foreign-segment deploy: expected 400, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "segment not found: "+foreignSeg) {
		t.Fatalf("expected 'segment not found' error, got %s", rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations (no campaign row may be reserved): %v", err)
	}
}

func TestHandleDeployCampaign_OwnSegmentProceeds(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	ownSeg := uuid.New().String()

	// Ownership check passes, then the normal reservation flow runs.
	mock.ExpectQuery("SELECT id::text FROM mailing_segments WHERE id = ANY").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(ownSeg))
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT id::text, status\\s+FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}))
	mock.ExpectExec("INSERT INTO mailing_campaigns").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT id::text FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ok"))

	body, _ := json.Marshal(req046DeployInput("Own Segment Deploy", []string{ownSeg}))
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	service.HandleDeployCampaign(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("own-segment deploy: expected 202, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

// --- DoD-4: refresh-all admin gate + journey segments org scoping ------------------

func TestHandleRefreshAllSegments_ClosedWithoutAdminKey(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()
	svc := NewAdvancedMailingService(db)

	cases := []struct {
		name   string
		envKey string
		reqKey string
	}{
		{name: "no key configured (closed by default)", envKey: "", reqKey: "anything"},
		{name: "wrong key", envKey: "secret", reqKey: "wrong"},
		{name: "missing header", envKey: "secret", reqKey: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ADMIN_API_KEY", tc.envKey)
			req := httptest.NewRequest(http.MethodPost, "/segments/refresh-all", nil)
			if tc.reqKey != "" {
				req.Header.Set("X-Admin-Key", tc.reqKey)
			}
			rec := httptest.NewRecorder()
			svc.HandleRefreshAllSegments(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations (no query may run when gated): %v", err)
	}
}

func TestHandleRefreshAllSegments_RunsWithAdminKey(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()
	svc := NewAdvancedMailingService(db)

	t.Setenv("ADMIN_API_KEY", "secret")
	mock.ExpectQuery("FROM mailing_segments").
		WillReturnRows(sqlmock.NewRows([]string{"id", "list_id", "name", "conditions"}))

	req := httptest.NewRequest(http.MethodPost, "/segments/refresh-all", nil)
	req.Header.Set("X-Admin-Key", "secret")
	rec := httptest.NewRecorder()
	svc.HandleRefreshAllSegments(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid admin key, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestHandleJourneySegments_FiltersByOrg(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()
	jc := &JourneyCenter{db: db}

	orgID := uuid.New().String()
	mock.ExpectQuery("FROM mailing_segments").
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "subscriber_count", "last_calculated_at"}))

	req := httptest.NewRequest(http.MethodGet, "/journey-center/segments", nil)
	req.Header.Set("X-Organization-ID", orgID)
	rec := httptest.NewRecorder()
	jc.HandleJourneySegments(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations (query must carry the org arg): %v", err)
	}
}
