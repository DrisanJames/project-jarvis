package api

// Tests for the click-drip exit-on-convert hook in
// EverflowPostbackHandler.HandlePostback.
//
// Rewritten 2026-06-02 when the hook was hardened to satisfy the operator
// rule: "associate a converter by their UUID; converters for offers not in
// the dictionary must be skipped."
//
// The hardened flow:
//   - converted-suppression still fires for ANY offer with a resolvable
//     internal UUID (dictionary or not) — preserved behaviour;
//   - the click-drip enrollment exit is gated on the journey DICTIONARY
//     (mailing_offer_journey_map) and matched by subscriber UUID (with email
//     as a fallback), so it runs even when the offer has NO mailing_offers
//     row (true for most click-drip offers) — this is the bug fix;
//   - conversions for offers not in the dictionary make zero enrollment
//     changes.

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func newConvertExitMockDB(t *testing.T) (*EverflowPostbackHandler, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewEverflowPostbackHandler(db), mock
}

const (
	ceSubID     = "11111111-2222-3333-4444-555555555555"
	ceOfferEF   = "9539"
	ceOfferUUID = "ffffffff-0000-1111-2222-333333333333"
	ceEmail     = "buyer@example.com"
)

// expectDurableConversionInsert registers the REQ-037 durable revenue-row
// INSERT that HandlePostback now fires FIRST, before any offer resolution or
// sub1 gate (see recordEverflowConversion, everflow_conversions.go).
func expectDurableConversionInsert(mock sqlmock.Sqlmock) {
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_everflow_conversions`)).
		WillReturnResult(sqlmock.NewResult(1, 1))
}

// TestConvertExit_ResolvedOffer_InDictionary_ExitsDrip is the full happy path:
// offer resolves to an internal UUID AND is in the dictionary, so we both
// suppress and exit the drip, matching enrollments by subscriber UUID + email.
func TestConvertExit_ResolvedOffer_InDictionary_ExitsDrip(t *testing.T) {
	h, mock := newConvertExitMockDB(t)
	expectDurableConversionInsert(mock)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM mailing_offers WHERE everflow_offer_id=$1`)).
		WithArgs(ceOfferEF).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(ceOfferUUID))

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_offer_suppressions`)).
		WillReturnResult(sqlmock.NewResult(1, 1))

	// Dictionary gate → offer IS mapped.
	mock.ExpectQuery(`FROM mailing_offer_journey_map`).
		WithArgs(ceOfferEF).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(1))

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT email FROM mailing_subscribers WHERE id=$1`)).
		WithArgs(ceSubID).
		WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow(ceEmail))

	// UPDATE matched by UUID ($2) OR email ($3).
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_enrollments`)).
		WithArgs(ceOfferEF, ceSubID, ceEmail).
		WillReturnResult(sqlmock.NewResult(0, 2))

	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/everflow/postback?sub1="+ceSubID+"&offer_id="+ceOfferEF+"&payout=2.50&transaction_id=txn123", nil)
	rec := httptest.NewRecorder()
	h.HandlePostback(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestConvertExit_ClickDripOffer_NoMailingOffersRow_StillExits is the bug fix:
// most click-drip offers have NO mailing_offers row, so offerID stays NULL.
// The OLD code returned early ("no_offer_id") and never exited the drip. The
// new code keys the exit on the dictionary, so the drip still stops.
func TestConvertExit_ClickDripOffer_NoMailingOffersRow_StillExits(t *testing.T) {
	h, mock := newConvertExitMockDB(t)
	expectDurableConversionInsert(mock)

	// mailing_offers lookup → NO row (offerID stays nil).
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM mailing_offers WHERE everflow_offer_id=$1`)).
		WithArgs(ceOfferEF).
		WillReturnRows(sqlmock.NewRows([]string{"id"})) // empty

	// No suppression INSERT (offer_id is NOT NULL → cannot write without a uuid).

	// Dictionary gate → offer IS mapped.
	mock.ExpectQuery(`FROM mailing_offer_journey_map`).
		WithArgs(ceOfferEF).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(1))

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT email FROM mailing_subscribers WHERE id=$1`)).
		WithArgs(ceSubID).
		WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow(ceEmail))

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_enrollments`)).
		WithArgs(ceOfferEF, ceSubID, ceEmail).
		WillReturnResult(sqlmock.NewResult(0, 1))

	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/everflow/postback?sub1="+ceSubID+"&offer_id="+ceOfferEF+"&payout=2.50&transaction_id=txn123", nil)
	rec := httptest.NewRecorder()
	h.HandlePostback(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet(),
		"the drip must exit for a dictionary offer even with no mailing_offers row")
}

// TestConvertExit_MatchesByUUID_WhenEmailMissing proves the UUID association:
// even when the subscriber email cannot be resolved, the UPDATE still runs
// (matched by metadata.original_subscriber = subscriber UUID).
func TestConvertExit_MatchesByUUID_WhenEmailMissing(t *testing.T) {
	h, mock := newConvertExitMockDB(t)
	expectDurableConversionInsert(mock)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM mailing_offers WHERE everflow_offer_id=$1`)).
		WithArgs(ceOfferEF).
		WillReturnRows(sqlmock.NewRows([]string{"id"})) // empty

	mock.ExpectQuery(`FROM mailing_offer_journey_map`).
		WithArgs(ceOfferEF).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(1))

	// Email lookup returns nothing.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT email FROM mailing_subscribers WHERE id=$1`)).
		WithArgs(ceSubID).
		WillReturnRows(sqlmock.NewRows([]string{"email"}))

	// UPDATE STILL fires, with an empty email arg — the UUID predicate matches.
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_enrollments`)).
		WithArgs(ceOfferEF, ceSubID, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/everflow/postback?sub1="+ceSubID+"&offer_id="+ceOfferEF+"&payout=1.00", nil)
	rec := httptest.NewRecorder()
	h.HandlePostback(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet(),
		"the drip must exit by UUID even when the email is unresolvable")
}

// TestConvertExit_OfferNotInDictionary_Suppresses_NoExit verifies the operator
// rule: a converter for an offer NOT in our dictionary is skipped for the
// click-drip exit (no enrollment UPDATE), while the pre-existing converted
// suppression still fires for the resolvable offer.
func TestConvertExit_OfferNotInDictionary_Suppresses_NoExit(t *testing.T) {
	h, mock := newConvertExitMockDB(t)
	expectDurableConversionInsert(mock)

	const otherOffer = "1234"

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM mailing_offers WHERE everflow_offer_id=$1`)).
		WithArgs(otherOffer).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(ceOfferUUID))

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_offer_suppressions`)).
		WillReturnResult(sqlmock.NewResult(1, 1))

	// Dictionary gate → NOT mapped (no rows → sql.ErrNoRows). No email
	// lookup, no enrollment UPDATE.
	mock.ExpectQuery(`FROM mailing_offer_journey_map`).
		WithArgs(otherOffer).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}))

	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/everflow/postback?sub1="+ceSubID+"&offer_id="+otherOffer+"&payout=5.00", nil)
	rec := httptest.NewRecorder()
	h.HandlePostback(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet(),
		"non-dictionary converter must suppress but NOT touch enrollments")
}

// TestConvertExit_NoOffer_NotInDictionary_Skips verifies a conversion with no
// resolvable offer and no dictionary entry returns a clean skip and touches
// no enrollment rows.
func TestConvertExit_NoOffer_NotInDictionary_Skips(t *testing.T) {
	h, mock := newConvertExitMockDB(t)
	expectDurableConversionInsert(mock)

	const camID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// campaign-based offer resolve returns nothing; efOfferID empty → no
	// dictionary query (exit helper short-circuits on empty offer id).
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT offer_id FROM mailing_campaigns WHERE id=$1`)).
		WithArgs(camID).
		WillReturnRows(sqlmock.NewRows([]string{"offer_id"}))

	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/everflow/postback?sub1="+ceSubID+"&sub3="+camID+"&payout=0.00", nil)
	rec := httptest.NewRecorder()
	h.HandlePostback(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}
