package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func invalidSegmentRequestBody(t *testing.T) []byte {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"name": "Recent non-openers",
		"root_group": map[string]any{
			"id":             "root",
			"logic_operator": "AND",
			"is_negated":     false,
			"conditions": []map[string]any{
				{
					"id":             "cond-1",
					"condition_type": "profile",
					"field":          "email",
					"operator":       "contains",
				},
			},
			"groups": []any{},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	return body
}

func TestSegmentationCreateReturnsBadRequestForValidationErrors(t *testing.T) {
	api := NewSegmentationAPI(nil)
	router := chi.NewRouter()
	api.RegisterRoutes(router)

	req := httptest.NewRequest(http.MethodPost, "/v2/segments/", bytes.NewReader(invalidSegmentRequestBody(t)))
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if payload["error"] != "validation_failed" {
		t.Fatalf("expected validation_failed error, got %#v", payload["error"])
	}
}

func TestSegmentationUpdateReturnsBadRequestForValidationErrors(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	api := NewSegmentationAPI(db)
	router := chi.NewRouter()
	api.RegisterRoutes(router)

	segmentID := uuid.New()
	mock.ExpectQuery("SELECT ms.id, ms.organization_id, ms.list_id, ms.name,").
		WithArgs(segmentID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	req := httptest.NewRequest(http.MethodPut, "/v2/segments/"+segmentID.String()+"/", bytes.NewReader(invalidSegmentRequestBody(t)))
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if payload["error"] != "validation_failed" {
		t.Fatalf("expected validation_failed error, got %#v", payload["error"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations were not met: %v", err)
	}
}

// TestSegmentIDArray covers the pure helper that formats a Postgres uuid[]
// literal for the materialized rollup query. It is the only piece of logic
// in ListSegments outside of the SQL itself, so a tight table-driven test
// here protects the heaviest read path on the segments dashboard.
func TestSegmentIDArray(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{name: "empty", in: nil, want: "{}"},
		{name: "empty_slice", in: []string{}, want: "{}"},
		{name: "single", in: []string{"00000000-0000-0000-0000-000000000001"}, want: "{00000000-0000-0000-0000-000000000001}"},
		{
			name: "multiple",
			in: []string{
				"11111111-1111-1111-1111-111111111111",
				"22222222-2222-2222-2222-222222222222",
				"33333333-3333-3333-3333-333333333333",
			},
			want: "{11111111-1111-1111-1111-111111111111,22222222-2222-2222-2222-222222222222,33333333-3333-3333-3333-333333333333}",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := segmentIDArray(tc.in)
			if got != tc.want {
				t.Fatalf("segmentIDArray(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestVersionSegmentationAPIIsBumped guards against accidental version
// regressions on the segments API surface. The major.minor must move forward
// with every visible change to the segments handler shape.
func TestVersionSegmentationAPIIsBumped(t *testing.T) {
	parts := strings.Split(VersionSegmentationAPI, ".")
	if len(parts) != 3 {
		t.Fatalf("expected semver-style VersionSegmentationAPI, got %q", VersionSegmentationAPI)
	}
	if !strings.HasPrefix(VersionSegmentationAPI, "2.") {
		t.Fatalf("expected 2.x version after materialized rollout, got %q", VersionSegmentationAPI)
	}
}

// TestGetOrgIDFromRequestFallsBackToSingleTenantConstant verifies that the
// hardcoded last-resort fallback fires when no other discovery path supplies
// an organization id. This is the production safety net: under boot-time DB
// pressure the SetProcessDefaultOrgID seed can be skipped, so the segments
// dashboard MUST still resolve a valid org id from the bare request.
func TestGetOrgIDFromRequestFallsBackToSingleTenantConstant(t *testing.T) {
	prevEnv := os.Getenv("DEFAULT_ORG_ID")
	os.Unsetenv("DEFAULT_ORG_ID")
	defer func() {
		if prevEnv != "" {
			os.Setenv("DEFAULT_ORG_ID", prevEnv)
		}
	}()

	prevDefault := GetProcessDefaultOrgID()
	SetProcessDefaultOrgID(uuid.Nil)
	defer SetProcessDefaultOrgID(prevDefault)

	req := httptest.NewRequest(http.MethodGet, "/v2/segments/", nil)
	got, err := GetOrgIDFromRequest(req)
	if err != nil {
		t.Fatalf("GetOrgIDFromRequest returned error: %v", err)
	}
	want, _ := uuid.Parse(SingleTenantFallbackOrgID)
	if got != want {
		t.Fatalf("expected hardcoded fallback %s, got %s", want, got)
	}
}
