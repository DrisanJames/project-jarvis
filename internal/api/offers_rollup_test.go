package api

// offers_rollup_test.go — audience unification Phase 3.
//
// Pins the rollup contract: org-scoped aggregates over the denormalized
// campaign counters, the synthetic '(unattributed)' row for offer_id IS NULL,
// and conversions merged from the mailing_offer_suppressions converted ledger.

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// TestHandleOffersRollup_AggregatesAndUnattributed: per-offer sums come back
// keyed by offer, the NULL-offer_id bucket renders as '(unattributed)' with
// zero conversions, and conversion counts merge onto the right offer row.
func TestHandleOffersRollup_AggregatesAndUnattributed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	orgID := uuid.New().String()
	offerID := uuid.New().String()

	mock.ExpectQuery(`LEFT JOIN mailing_offers`).
		WithArgs(orgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"offer_id", "offer_key", "offer_name", "campaigns", "total_recipients",
			"sent_count", "delivered_count", "unique_open_count", "unique_click_count",
			"hard_bounce_count", "soft_bounce_count",
		}).
			AddRow(offerID, "sams-club", "Sam's Club Membership", 4, 40000, 39000, 38000, 1200, 150, 60, 70).
			AddRow("", "", "(unattributed)", 2, 20000, 19000, 18500, 500, 60, 30, 40))

	mock.ExpectQuery(`reason = 'converted'`).
		WithArgs(orgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"offer_id", "count"}).
			AddRow(offerID, 17))

	req := httptest.NewRequest("GET", "/api/mailing/offers/rollup?days=30", nil)
	req.Header.Set("X-Organization-ID", orgID)
	rec := httptest.NewRecorder()
	HandleOffersRollup(db)(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Days int              `json:"days"`
		Rows []offerRollupRow `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Days != 30 || len(body.Rows) != 2 {
		t.Fatalf("days=%d rows=%d, want 30/2", body.Days, len(body.Rows))
	}
	attributed := body.Rows[0]
	if attributed.OfferID != offerID || attributed.OfferKey != "sams-club" ||
		attributed.Campaigns != 4 || attributed.DeliveredCount != 38000 ||
		attributed.UniqueOpenCount != 1200 || attributed.UniqueClickCnt != 150 ||
		attributed.HardBounceCount != 60 || attributed.SoftBounceCount != 70 {
		t.Fatalf("unexpected attributed row: %+v", attributed)
	}
	if attributed.Conversions != 17 {
		t.Fatalf("conversions = %d, want 17 (merged from suppression ledger)", attributed.Conversions)
	}
	unattributed := body.Rows[1]
	if unattributed.OfferID != "" || unattributed.OfferName != "(unattributed)" ||
		unattributed.Campaigns != 2 || unattributed.Conversions != 0 {
		t.Fatalf("unexpected unattributed row: %+v", unattributed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// TestHandleOffersRollup_OrgScoping: the org from the request is the org in
// BOTH queries — a different org's request never sees this org's rows.
func TestHandleOffersRollup_OrgScoping(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	otherOrg := uuid.New().String()
	emptyAgg := sqlmock.NewRows([]string{
		"offer_id", "offer_key", "offer_name", "campaigns", "total_recipients",
		"sent_count", "delivered_count", "unique_open_count", "unique_click_count",
		"hard_bounce_count", "soft_bounce_count",
	})
	mock.ExpectQuery(`LEFT JOIN mailing_offers`).
		WithArgs(otherOrg, sqlmock.AnyArg()). // org param is the requester's, verbatim
		WillReturnRows(emptyAgg)
	mock.ExpectQuery(`reason = 'converted'`).
		WithArgs(otherOrg, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"offer_id", "count"}))

	req := httptest.NewRequest("GET", "/api/mailing/offers/rollup", nil)
	req.Header.Set("X-Organization-ID", otherOrg)
	rec := httptest.NewRecorder()
	HandleOffersRollup(db)(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Rows []offerRollupRow `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Rows) != 0 {
		t.Fatalf("rows = %d, want 0 for the other org", len(body.Rows))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// TestHandleOffersList_LeanCatalog: the picker endpoint returns id/key/name/
// everflow_id/status, org-scoped.
func TestHandleOffersList_LeanCatalog(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	orgID := uuid.New().String()
	id1 := uuid.New().String()

	mock.ExpectQuery(`FROM mailing_offers`).
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "key", "name", "ef", "status"}).
			AddRow(id1, "liberty-mutual", "Liberty Mutual Insurance", "1090", "active"))

	req := httptest.NewRequest("GET", "/api/mailing/offers/list", nil)
	req.Header.Set("X-Organization-ID", orgID)
	rec := httptest.NewRecorder()
	HandleOffersList(db)(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Offers []offerListRow `json:"offers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Offers) != 1 || body.Offers[0].ID != id1 ||
		body.Offers[0].Key != "liberty-mutual" || body.Offers[0].Status != "active" {
		t.Fatalf("offers = %+v", body.Offers)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}
