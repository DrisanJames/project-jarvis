package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/segmentation"
)

func TestParseSegmentListFilter(t *testing.T) {
	tests := []struct {
		name              string
		query             string
		wantFilter        segmentation.SegmentListFilter
		wantIncludeCounts bool
		wantErr           bool
	}{
		{
			name:       "no params yields zero filter (legacy behavior)",
			query:      "",
			wantFilter: segmentation.SegmentListFilter{},
		},
		{
			name:       "q is trimmed and passed through raw (escaping happens in SQL builder)",
			query:      "q=%20Welcome%2050%25_off%20",
			wantFilter: segmentation.SegmentListFilter{NameQuery: "Welcome 50%_off"},
		},
		{
			name:       "status active",
			query:      "status=active",
			wantFilter: segmentation.SegmentListFilter{Status: "active"},
		},
		{
			name:       "status is lowercased",
			query:      "status=ARCHIVED",
			wantFilter: segmentation.SegmentListFilter{Status: "archived"},
		},
		{
			name:       "status all accepted",
			query:      "status=all",
			wantFilter: segmentation.SegmentListFilter{Status: "all"},
		},
		{
			name:    "unknown status rejected",
			query:   "status=deleted",
			wantErr: true,
		},
		{
			name:  "categories comma list, trimmed, empties dropped",
			query: "categories=partner_wave_static,%20engaged-model%20,,data-partner",
			wantFilter: segmentation.SegmentListFilter{
				Categories: []string{"partner_wave_static", "engaged-model", "data-partner"},
			},
		},
		{
			name:  "exclude_categories parsed",
			query: "exclude_categories=partner_wave_static,legacy_snapshot",
			wantFilter: segmentation.SegmentListFilter{
				ExcludeCategories: []string{"partner_wave_static", "legacy_snapshot"},
			},
		},
		{
			name:    "category with inner space rejected",
			query:   "categories=partner%20wave",
			wantErr: true,
		},
		{
			name:    "category with quote rejected",
			query:   "categories=funnel%27--",
			wantErr: true,
		},
		{
			name:    "exclude category with percent rejected",
			query:   "exclude_categories=fun%25nel",
			wantErr: true,
		},
		{
			name:       "limit in range",
			query:      "limit=250",
			wantFilter: segmentation.SegmentListFilter{Limit: 250},
		},
		{
			name:       "limit clamped to max 2000",
			query:      "limit=99999",
			wantFilter: segmentation.SegmentListFilter{Limit: 2000},
		},
		{
			name:       "limit clamped to min 1",
			query:      "limit=0",
			wantFilter: segmentation.SegmentListFilter{Limit: 1},
		},
		{
			name:       "negative limit clamped to min 1",
			query:      "limit=-5",
			wantFilter: segmentation.SegmentListFilter{Limit: 1},
		},
		{
			name:    "non-numeric limit rejected",
			query:   "limit=abc",
			wantErr: true,
		},
		{
			name:       "offset parsed",
			query:      "offset=400",
			wantFilter: segmentation.SegmentListFilter{Offset: 400},
		},
		{
			name:       "negative offset clamped to 0",
			query:      "offset=-1",
			wantFilter: segmentation.SegmentListFilter{Offset: 0},
		},
		{
			name:    "non-numeric offset rejected",
			query:   "offset=x",
			wantErr: true,
		},
		{
			name:              "include_counts=1",
			query:             "include_counts=1",
			wantIncludeCounts: true,
		},
		{
			name:              "include_counts=true",
			query:             "include_counts=true",
			wantIncludeCounts: true,
		},
		{
			name:  "include_counts=0 is off",
			query: "include_counts=0",
		},
		{
			name:  "combined params",
			query: "q=warby&status=active&categories=funnel,framework&exclude_categories=legacy_snapshot&limit=100&offset=200&include_counts=1",
			wantFilter: segmentation.SegmentListFilter{
				NameQuery:         "warby",
				Status:            "active",
				Categories:        []string{"funnel", "framework"},
				ExcludeCategories: []string{"legacy_snapshot"},
				Limit:             100,
				Offset:            200,
			},
			wantIncludeCounts: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatalf("url.ParseQuery(%q) error = %v", tt.query, err)
			}

			filter, includeCounts, errMsg := parseSegmentListFilter(values)
			if tt.wantErr {
				if errMsg == "" {
					t.Fatalf("expected validation error, got filter %+v", filter)
				}
				return
			}
			if errMsg != "" {
				t.Fatalf("unexpected validation error: %s", errMsg)
			}
			if !reflect.DeepEqual(filter, tt.wantFilter) {
				t.Fatalf("filter = %+v, want %+v", filter, tt.wantFilter)
			}
			if includeCounts != tt.wantIncludeCounts {
				t.Fatalf("includeCounts = %v, want %v", includeCounts, tt.wantIncludeCounts)
			}
		})
	}
}

func TestListSegmentsRejectsInvalidParamsWith400(t *testing.T) {
	// No DB needed: validation fails before any query runs.
	api := NewSegmentationAPI(nil)
	router := chi.NewRouter()
	api.RegisterRoutes(router)

	for _, q := range []string{
		"categories=bad%20token",
		"exclude_categories=%27%3BDROP",
		"status=bogus",
		"limit=abc",
		"offset=zz",
	} {
		t.Run(q, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v2/segments/?"+q, nil)
			req.Header.Set("X-Organization-ID", uuid.NewString())
			rec := httptest.NewRecorder()

			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected status %d, got %d (body %s)", http.StatusBadRequest, rec.Code, rec.Body.String())
			}
			var payload map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if payload["error"] != "invalid_filter" {
				t.Fatalf("expected invalid_filter error, got %#v", payload["error"])
			}
		})
	}
}

// segmentListColumns mirrors the SELECT list of ListSegmentsFiltered.
func segmentListSQLMockRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "organization_id", "list_id", "name", "description", "segment_type",
		"category", "subscriber_count", "last_calculated_at", "status",
		"is_system", "system_query", "created_at", "updated_at",
	})
}

func TestListSegmentsNoParamsKeepsLegacySQLAndBareArray(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	api := NewSegmentationAPI(db)
	router := chi.NewRouter()
	api.RegisterRoutes(router)

	orgID := uuid.New()
	// Exactly one arg (org id), no ILIKE / category / LIMIT / OFFSET clauses.
	mock.ExpectQuery(`SELECT ms\.id, ms\.organization_id[\s\S]*WHERE ms\.organization_id = \$1 AND ms\.status != 'deleted'\s*ORDER BY \(ss\.segment_id IS NOT NULL\) DESC, ms\.name$`).
		WithArgs(orgID).
		WillReturnRows(segmentListSQLMockRows())

	req := httptest.NewRequest(http.MethodGet, "/v2/segments/", nil)
	req.Header.Set("X-Organization-ID", orgID.String())
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d (body %s)", http.StatusOK, rec.Code, rec.Body.String())
	}
	// Legacy payload is a bare JSON array, NOT an envelope.
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("expected bare JSON array, got %q (err %v)", rec.Body.String(), err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations were not met: %v", err)
	}
}

func TestListSegmentsFilteredWithCountsEnvelope(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	api := NewSegmentationAPI(db)
	router := chi.NewRouter()
	api.RegisterRoutes(router)

	orgID := uuid.New()

	// List query: ILIKE + status + categories + exclusion + LIMIT/OFFSET.
	mock.ExpectQuery(`ms\.name ILIKE \$2 AND ms\.status = \$3 AND COALESCE\(ms\.category, 'uncategorized'\) = ANY\(\$4::text\[\]\) AND COALESCE\(ms\.category, 'uncategorized'\) <> ALL\(\$5::text\[\]\)[\s\S]*ORDER BY[\s\S]*LIMIT \$6 OFFSET \$7`).
		WithArgs(orgID, `%warby \%deal%`, "active", `{"funnel","framework"}`, `{"legacy_snapshot"}`, 50, 100).
		WillReturnRows(segmentListSQLMockRows())

	// total: all filters, no LIMIT/OFFSET.
	mock.ExpectQuery(`SELECT COUNT\(\*\)[\s\S]*ms\.name ILIKE \$2 AND ms\.status = \$3 AND COALESCE\(ms\.category, 'uncategorized'\) = ANY\(\$4::text\[\]\) AND COALESCE\(ms\.category, 'uncategorized'\) <> ALL\(\$5::text\[\]\)$`).
		WithArgs(orgID, `%warby \%deal%`, "active", `{"funnel","framework"}`, `{"legacy_snapshot"}`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(7)))

	// category_counts: q/status filters only — NO category predicates.
	mock.ExpectQuery(`SELECT COALESCE\(ms\.category, 'uncategorized'\) AS category, COUNT\(\*\)[\s\S]*ms\.name ILIKE \$2 AND ms\.status = \$3\s*GROUP BY 1$`).
		WithArgs(orgID, `%warby \%deal%`, "active").
		WillReturnRows(sqlmock.NewRows([]string{"category", "count"}).
			AddRow("funnel", int64(5)).
			AddRow("partner_wave_static", int64(30977)))

	target := "/v2/segments/?q=" + url.QueryEscape("warby %deal") +
		"&status=active&categories=funnel,framework&exclude_categories=legacy_snapshot&limit=50&offset=100&include_counts=1"
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("X-Organization-ID", orgID.String())
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d (body %s)", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Segments       []map[string]any `json:"segments"`
		Total          int64            `json:"total"`
		CategoryCounts map[string]int64 `json:"category_counts"`
		APIVersion     string           `json:"api_version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v (body %s)", err, rec.Body.String())
	}
	if payload.Total != 7 {
		t.Fatalf("total = %d, want 7", payload.Total)
	}
	if payload.CategoryCounts["partner_wave_static"] != 30977 {
		t.Fatalf("category_counts = %#v, want partner_wave_static=30977", payload.CategoryCounts)
	}
	if payload.APIVersion != VersionSegmentationAPI {
		t.Fatalf("api_version = %q, want %q", payload.APIVersion, VersionSegmentationAPI)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations were not met: %v", err)
	}
}
