package api

// Tests for the REQ-037 durable per-conversion revenue ledger
// (mailing_everflow_conversions): the postback-enrichment write on both
// Everflow entry points — including the blank-sub1 class that used to be
// dropped entirely — and the admin conversions-report pull that backfills
// authoritative payout/status from the Everflow API.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// TestPostback_BlankSub1_StillPersistsDurableRow pins the Tahiti-class fix:
// a conversion postback with no sub1 previously vanished ("skipped",
// no_subscriber_id, zero writes). It must now land a durable revenue row —
// payout included — BEFORE the skip response, so the dollars survive and the
// row stays joinable later by transaction_id via the export pull.
func TestPostback_BlankSub1_StillPersistsDurableRow(t *testing.T) {
	h, mock := newConvertExitMockDB(t)

	// The ONLY database touch: the durable insert. The skip must still be
	// returned (no suppression, no drip exit — nothing else is actionable).
	// Args pin the anti-clobber fix: the GET query's offer/payout/txn must
	// survive the empty JSON-body fallback (they used to be wiped to
	// ""/0.00/"" whenever sub1 was blank).
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_everflow_conversions`)).
		WithArgs(sqlmock.AnyArg(), "00000000-0000-0000-0000-000000000001",
			"txn-blank-sub1", ceOfferEF, nil, nil, "", "", "", 42.50).
		WillReturnResult(sqlmock.NewResult(1, 1))

	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/everflow/postback?offer_id="+ceOfferEF+"&payout=42.50&transaction_id=txn-blank-sub1", nil)
	rec := httptest.NewRecorder()
	h.HandlePostback(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "skipped", body["status"], "skip semantics preserved")
	require.Equal(t, "no_subscriber_id", body["reason"])
	require.NoError(t, mock.ExpectationsWereMet(),
		"blank-sub1 conversion must still write the durable revenue row")
}

// TestPostback_DurableInsertFailure_NeverBlocks200 pins the best-effort
// contract: a failed durable insert logs and the handler proceeds — Everflow
// always gets its 200 and the existing suppression/drip machinery still runs.
func TestPostback_DurableInsertFailure_NeverBlocks200(t *testing.T) {
	h, mock := newConvertExitMockDB(t)

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_everflow_conversions`)).
		WillReturnError(sqlmock.ErrCancelled)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM mailing_offers WHERE everflow_offer_id=$1`)).
		WithArgs(ceOfferEF).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(ceOfferUUID))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_offer_suppressions`)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`FROM mailing_offer_journey_map`).
		WithArgs(ceOfferEF).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}))

	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/everflow/postback?sub1="+ceSubID+"&offer_id="+ceOfferEF+"&payout=5.00&transaction_id=txnF", nil)
	rec := httptest.NewRecorder()
	h.HandlePostback(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestClickPostback_ConversionEvent_PersistsDurableRow verifies the click
// endpoint's conversion branch (operators may point a conversion postback at
// the click URL with &event=conversion) writes the same durable ledger row —
// even when sub1 is blank, in which case the endpoint's usual
// no_subscriber_id skip still follows.
func TestClickPostback_ConversionEvent_PersistsDurableRow(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	h := NewEverflowClickPostbackHandler(db)

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_everflow_conversions`)).
		WithArgs(sqlmock.AnyArg(), "00000000-0000-0000-0000-000000000001",
			"txn-click-cv", ceOfferEF, nil, nil, "", "", "", 9.99).
		WillReturnResult(sqlmock.NewResult(1, 1))

	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/everflow/click-postback?offer_id="+ceOfferEF+"&event=conversion&payout=9.99&transaction_id=txn-click-cv", nil)
	rec := httptest.NewRecorder()
	h.HandleClickPostback(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet(),
		"conversion routed at the click endpoint must persist the revenue row before any guard")
}

// TestClickPostback_PlainClick_NoDurableRow proves the ledger write is gated
// on the conversion discriminator: an ordinary click (no &event=conversion,
// no payout) with a blank sub1 makes zero ledger writes.
func TestClickPostback_PlainClick_NoDurableRow(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	h := NewEverflowClickPostbackHandler(db)

	// No expectations: blank sub1 short-circuits before any click machinery,
	// and the ledger insert must NOT fire for a non-conversion event.
	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/everflow/click-postback?offer_id="+ceOfferEF+"&transaction_id=txn-click", nil)
	rec := httptest.NewRecorder()
	h.HandleClickPostback(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestEverflowConversionsSync_EnrichesAndUpserts drives the admin pull against
// a stubbed Everflow API: record A shares a transaction_id with an existing
// postback row (→ ENRICH in place, no new row), record B is unseen (→ upsert
// keyed by conversion_id). Blank-keyed records are skipped.
func TestEverflowConversionsSync_EnrichesAndUpserts(t *testing.T) {
	// Stub the Everflow reporting API (one page, 3 records).
	ef := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/networks/reporting/conversions", r.URL.Path)
		require.NotEmpty(t, r.Header.Get("X-Eflow-API-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"conversions": [
				{"conversion_id":"cvA","transaction_id":"txnA","offer_id":"9539",
				 "sub1":"11111111-2222-3333-4444-555555555555","sub2":"discountblog.com","sub3":"",
				 "payout":12.50,"status":"approved","event_name":"","conversion_unix_timestamp":1752300000},
				{"conversion_id":"cvB","transaction_id":"txnB","offer_id":"9539",
				 "sub1":"","sub2":"","sub3":"",
				 "payout":30.00,"status":"pending","event_name":"","conversion_unix_timestamp":1752300100},
				{"conversion_id":"","transaction_id":"","offer_id":"9539","payout":1.00}
			],
			"paging": {"page":1,"page_size":50,"total_count":3}
		}`))
	}))
	t.Cleanup(ef.Close)
	t.Setenv("EVERFLOW_BASE_URL", ef.URL)
	t.Setenv("EVERFLOW_API_KEY", "test-key")

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// Record A: enrich UPDATE matches an existing postback row → 1 row, done.
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_everflow_conversions`)).
		WithArgs("cvA", 12.50, "approved", "", "txnA").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Record B: enrich UPDATE matches nothing → fall through to the upsert.
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_everflow_conversions`)).
		WithArgs("cvB", 30.00, "pending", "", "txnB").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_everflow_conversions`)).
		WillReturnResult(sqlmock.NewResult(1, 1))

	// Record C (blank conversion_id AND transaction_id): skipped, no SQL.

	req := httptest.NewRequest(http.MethodPost, "/api/admin/everflow-conversions-sync?days=3", nil)
	rec := httptest.NewRecorder()
	HandleEverflowConversionsSync(db)(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.EqualValues(t, 3, body["fetched"])
	require.EqualValues(t, 1, body["enriched"])
	require.EqualValues(t, 1, body["upserted"])
	require.EqualValues(t, 1, body["skipped"])
	require.EqualValues(t, 0, body["failed"])
	require.NoError(t, mock.ExpectationsWereMet())
}
