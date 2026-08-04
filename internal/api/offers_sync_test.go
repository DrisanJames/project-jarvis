package api

// offers_sync_test.go — audience unification Phase 3.
//
// Pins the sync endpoint's contract: upsert keyed by landing_page_slug,
// idempotent re-runs report unchanged, registry statuses map onto the
// values the Offer Center already stores, rows are never deleted.

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func syncBody(t *testing.T, offers string) *strings.Reader {
	t.Helper()
	return strings.NewReader(`{"offers":[` + offers + `]}`)
}

// TestHandleOffersSyncPost_CreatesThenUnchanged: first sync of a new key
// INSERTs (created); an identical second sync is a no-op (unchanged) — the
// upsert is idempotent and never duplicates rows.
func TestHandleOffersSyncPost_CreatesThenUnchanged(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	orgID := uuid.New().String()
	offerID := uuid.New().String()

	// Run 1: slug lookup misses, name-adoption lookup misses → INSERT.
	mock.ExpectQuery(`lower\(landing_page_slug\) = \$2`).
		WithArgs(orgID, "sams-club").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`COALESCE\(landing_page_slug,''\) = ''`).
		WithArgs(orgID, "Sam's Club Membership").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`INSERT INTO mailing_offers`).
		WithArgs(orgID, "Sam's Club Membership", "8241",
			"https://www.eos57ytf.com/K4C5ZLC/PS8241/", "sams-club", "active").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(offerID))

	item := `{"key":"sams-club","display":"Sam's Club Membership","everflow_id":"8241","money_url":"https://www.eos57ytf.com/K4C5ZLC/PS8241/","status":"active"}`

	req := httptest.NewRequest("POST", "/api/admin/offers/sync", syncBody(t, item))
	req.Header.Set("X-Organization-ID", orgID)
	rec := httptest.NewRecorder()
	HandleOffersSyncPost(db)(rec, req)

	if rec.Code != 200 {
		t.Fatalf("run1 status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out1 struct {
		Created, Updated, Unchanged int
		Results                     []offerSyncResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out1); err != nil {
		t.Fatalf("decode run1: %v", err)
	}
	if out1.Created != 1 || out1.Updated != 0 || out1.Unchanged != 0 {
		t.Fatalf("run1 counts = %+v, want created=1", out1)
	}
	if out1.Results[0].Action != "created" || out1.Results[0].OfferID != offerID {
		t.Fatalf("run1 result = %+v", out1.Results[0])
	}

	// Run 2: slug lookup hits with identical fields → unchanged, no writes.
	mock.ExpectQuery(`lower\(landing_page_slug\) = \$2`).
		WithArgs(orgID, "sams-club").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "ef", "url", "status"}).
			AddRow(offerID, "Sam's Club Membership", "8241",
				"https://www.eos57ytf.com/K4C5ZLC/PS8241/", "active"))

	req2 := httptest.NewRequest("POST", "/api/admin/offers/sync", syncBody(t, item))
	req2.Header.Set("X-Organization-ID", orgID)
	rec2 := httptest.NewRecorder()
	HandleOffersSyncPost(db)(rec2, req2)

	if rec2.Code != 200 {
		t.Fatalf("run2 status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	var out2 struct {
		Created, Updated, Unchanged int
		Results                     []offerSyncResult `json:"results"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &out2); err != nil {
		t.Fatalf("decode run2: %v", err)
	}
	if out2.Unchanged != 1 || out2.Created != 0 || out2.Updated != 0 {
		t.Fatalf("run2 counts = %+v, want unchanged=1", out2)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// TestHandleOffersSyncPost_UpdatesAndStatusMapping: a drifted row is UPDATEd,
// and the registry's 'sunset' maps onto the Offer Center's 'paused' (never a
// DELETE — offers are permanent).
func TestHandleOffersSyncPost_UpdatesAndStatusMapping(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	orgID := uuid.New().String()
	offerID := uuid.New().String()

	mock.ExpectQuery(`lower\(landing_page_slug\) = \$2`).
		WithArgs(orgID, "carshield").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "ef", "url", "status"}).
			AddRow(offerID, "CarShield", "5990", "https://old.example/", "active"))
	mock.ExpectExec(`UPDATE mailing_offers`).
		WithArgs("CarShield Auto Warranty", "5990", "https://www.eos57ytf.com/K4C5ZLC/LSX1XK/",
			"carshield", "paused", offerID, orgID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	item := `{"key":"carshield","display":"CarShield Auto Warranty","everflow_id":"5990","money_url":"https://www.eos57ytf.com/K4C5ZLC/LSX1XK/","status":"sunset"}`
	req := httptest.NewRequest("POST", "/api/admin/offers/sync", syncBody(t, item))
	req.Header.Set("X-Organization-ID", orgID)
	rec := httptest.NewRecorder()
	HandleOffersSyncPost(db)(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Updated int
		Results []offerSyncResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Updated != 1 || out.Results[0].Action != "updated" {
		t.Fatalf("out = %+v, want updated=1", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// TestHandleOffersSyncGet_KeyToIDMap: the GET side returns the org-scoped
// key→id map the Python drift report consumes.
func TestHandleOffersSyncGet_KeyToIDMap(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	orgID := uuid.New().String()
	id1, id2 := uuid.New().String(), uuid.New().String()

	mock.ExpectQuery(`FROM mailing_offers`).
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"key", "id", "name", "ef", "status"}).
			AddRow("carshield", id1, "CarShield Auto Warranty", "5990", "active").
			AddRow("sams-club", id2, "Sam's Club Membership", "8241", "active"))

	req := httptest.NewRequest("GET", "/api/admin/offers/sync", nil)
	req.Header.Set("X-Organization-ID", orgID)
	rec := httptest.NewRecorder()
	HandleOffersSyncGet(db)(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		KeyToID map[string]string `json:"key_to_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.KeyToID["carshield"] != id1 || out.KeyToID["sams-club"] != id2 {
		t.Fatalf("key_to_id = %v", out.KeyToID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}
