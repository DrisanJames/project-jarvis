package api

// Tests for the click-drip exit-on-convert hook added to
// EverflowPostbackHandler.HandlePostback in Phase 4 (2026-06-01).
//
// Scope is intentionally narrow: we are NOT regression-testing the
// pre-existing conversion postback flow — that has been in production
// for months. We only verify that the new UPDATE on
// mailing_journey_enrollments fires under the expected conditions and
// is skipped under the expected conditions.

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

// TestConvertExit_Fires_When_Offer_And_Subscriber_Resolved verifies that
// when the conversion postback successfully resolves an offer and we have
// a subscriber email, the UPDATE on mailing_journey_enrollments runs.
func TestConvertExit_Fires_When_Offer_And_Subscriber_Resolved(t *testing.T) {
	h, mock := newConvertExitMockDB(t)

	const subID = "11111111-2222-3333-4444-555555555555"
	const efOfferID = "9539"

	// Offer resolved via everflow_offer_id (no campaign id in this test).
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM mailing_offers WHERE everflow_offer_id=$1`)).
		WithArgs(efOfferID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("ffffffff-0000-1111-2222-333333333333"))

	// Suppression insert (existing behavior — keep it green).
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_offer_suppressions`)).
		WillReturnResult(sqlmock.NewResult(1, 1))

	// NEW: subscriber email lookup for the click-drip exit hook.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT email FROM mailing_subscribers WHERE id=$1`)).
		WithArgs(subID).
		WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow("test@example.com"))

	// NEW: UPDATE on mailing_journey_enrollments.
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_enrollments`)).
		WithArgs(efOfferID, "test@example.com").
		WillReturnResult(sqlmock.NewResult(0, 2)) // 2 enrollments exited

	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/everflow/postback?sub1="+subID+"&offer_id="+efOfferID+"&payout=2.50&transaction_id=txn123", nil)
	rec := httptest.NewRecorder()
	h.HandlePostback(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet(),
		"expected the convert-exit UPDATE to fire after suppression insert")
}

// TestConvertExit_NoEverflowOfferID_SkipsHook verifies that we do not
// even attempt the subscriber email lookup when efOfferID is empty.
// This protects against runaway updates if Everflow ever sends a
// postback without an offer_id.
func TestConvertExit_NoEverflowOfferID_SkipsHook(t *testing.T) {
	h, mock := newConvertExitMockDB(t)

	const subID = "11111111-2222-3333-4444-555555555555"
	const camID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// No efOfferID, but campaignID is set → handler tries campaign-based offer resolve.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT offer_id FROM mailing_campaigns WHERE id=$1`)).
		WithArgs(camID).
		WillReturnRows(sqlmock.NewRows([]string{"offer_id"})) // empty result

	// Handler returns 200 + skipped: no_offer_id; nothing else expected.
	// In particular: NO subscriber email lookup, NO enrollment UPDATE.

	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/everflow/postback?sub1="+subID+"&sub3="+camID+"&payout=0.00", nil)
	rec := httptest.NewRecorder()
	h.HandlePostback(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestConvertExit_SubscriberEmailMissing_SkipsUpdate verifies that if
// we resolve the offer but cannot resolve a subscriber email, we
// gracefully skip the UPDATE rather than running it with an empty email.
func TestConvertExit_SubscriberEmailMissing_SkipsUpdate(t *testing.T) {
	h, mock := newConvertExitMockDB(t)

	const subID = "11111111-2222-3333-4444-555555555555"
	const efOfferID = "9539"

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM mailing_offers WHERE everflow_offer_id=$1`)).
		WithArgs(efOfferID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("ffffffff-0000-1111-2222-333333333333"))

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_offer_suppressions`)).
		WillReturnResult(sqlmock.NewResult(1, 1))

	// Subscriber email lookup returns no rows.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT email FROM mailing_subscribers WHERE id=$1`)).
		WithArgs(subID).
		WillReturnRows(sqlmock.NewRows([]string{"email"}))

	// CRITICAL: NO UPDATE on mailing_journey_enrollments.

	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/everflow/postback?sub1="+subID+"&offer_id="+efOfferID+"&payout=1.00", nil)
	rec := httptest.NewRecorder()
	h.HandlePostback(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet(),
		"expected NO enrollment update when subscriber email cannot be resolved")
}
