package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	mock.ExpectQuery("SELECT ms.id, ms.organization_id, ms.list_id, ms.name, ms.description, ms.segment_type, ms.conditions,").
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
