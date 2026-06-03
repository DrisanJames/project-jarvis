package api

// Tests for EverflowClickPostbackHandler.
//
// Pattern matches the rest of internal/api: go-sqlmock with the regex query
// matcher + httptest for request/response. Each test sets up only the DB
// expectations it expects to fire — sqlmock fails on unexpected queries, so
// "no further DB calls" assertions are implicit (covered by the absence of an
// expectation and the final ExpectationsWereMet call).
//
// The handler ALWAYS returns HTTP 200 (Everflow retries on non-200 and we
// have no recovery state for it), so every test asserts 200 + a JSON body
// with `status` ("queued" | "skipped" | "error") and an optional `reason`.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ----- test fixtures & helpers --------------------------------------------

const (
	testSubID           = "11111111-2222-3333-4444-555555555555"
	testCamID           = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	testOfferEverflow   = "9539"
	testOfferUUID       = "ffffffff-0000-1111-2222-333333333333"
	testProfileUUID     = "eeeeeeee-1111-2222-3333-444444444444"
	testClickJourneyID  = "00000000-1111-2222-3333-444444444444"
	testBrandSub2       = "em.discountblog.com"
	testTransactionID   = "txn-abc-123"
	testClickURL        = "https://example.com/landing?aff=99"
	testSubscriberEmail = "test@example.com"
)

// journeyMapCols mirrors the SELECT projection in lookupOfferJourneyMap.
// Keep this in lockstep with the SELECT list there — re-ordering or
// adding columns to the handler requires updating this slice and every
// AddRow call below.
var journeyMapCols = []string{
	"everflow_offer_id", "click_journey_id", "payout_type", "enabled",
}

func newClickPostbackHandler(t *testing.T) (*EverflowClickPostbackHandler, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
		sqlmock.MonitorPingsOption(false),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewEverflowClickPostbackHandler(db), mock
}

// clickGET fires a GET request against the handler with the given query
// string (without the leading ?). Empty string = no query at all.
func clickGET(t *testing.T, h *EverflowClickPostbackHandler, query string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/api/mailing/everflow/click-postback"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	h.HandleClickPostback(rec, req)
	return rec
}

// clickPOST fires a POST request with a JSON body.
func clickPOST(t *testing.T, h *EverflowClickPostbackHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/mailing/everflow/click-postback",
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.HandleClickPostback(rec, req)
	return rec
}

func decodeClickResp(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var out map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	return out
}

// ----- Phase 1: input validation (no DB) -----------------------------------

func TestClickPostback_NoSubscriberID_ReturnsSkipped(t *testing.T) {
	h, mock := newClickPostbackHandler(t)
	// sub1 missing entirely — handler should short-circuit before any DB call.
	rec := clickGET(t, h, "offer_id="+testOfferEverflow)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	resp := decodeClickResp(t, rec)
	assert.Equal(t, "skipped", resp["status"])
	assert.Equal(t, "no_subscriber_id", resp["reason"])
	_, hasTrigger := resp["trigger_id"]
	assert.False(t, hasTrigger, "trigger_id must be omitted for non-queued responses")

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClickPostback_NoOfferID_ReturnsSkipped(t *testing.T) {
	h, mock := newClickPostbackHandler(t)
	// Valid sub1 but no offer_id.
	rec := clickGET(t, h, "sub1="+testSubID)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeClickResp(t, rec)
	assert.Equal(t, "skipped", resp["status"])
	assert.Equal(t, "no_offer_id", resp["reason"])

	require.NoError(t, mock.ExpectationsWereMet())
}

// ----- Phase 2: journey-map gating -----------------------------------------

func TestClickPostback_OfferNotMapped_ReturnsSkipped(t *testing.T) {
	h, mock := newClickPostbackHandler(t)

	mock.ExpectQuery(`FROM mailing_offer_journey_map`).
		WithArgs(testOfferEverflow).
		WillReturnError(sql.ErrNoRows)

	rec := clickGET(t, h, "sub1="+testSubID+"&offer_id="+testOfferEverflow)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeClickResp(t, rec)
	assert.Equal(t, "skipped", resp["status"])
	assert.Equal(t, "offer_not_mapped", resp["reason"])

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClickPostback_OfferDisabled_ReturnsSkipped(t *testing.T) {
	h, mock := newClickPostbackHandler(t)

	// enabled=false should short-circuit BEFORE the CPC check and BEFORE
	// the empty-journey check. Doesn't matter what payout_type or
	// click_journey_id say — disabled wins.
	mock.ExpectQuery(`FROM mailing_offer_journey_map`).
		WithArgs(testOfferEverflow).
		WillReturnRows(sqlmock.NewRows(journeyMapCols).
			AddRow(testOfferEverflow, testClickJourneyID, "CPM", false))

	rec := clickGET(t, h, "sub1="+testSubID+"&offer_id="+testOfferEverflow)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeClickResp(t, rec)
	assert.Equal(t, "skipped", resp["status"])
	assert.Equal(t, "offer_journey_disabled", resp["reason"])

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClickPostback_CPCOffer_ReturnsSkipped(t *testing.T) {
	h, mock := newClickPostbackHandler(t)

	// Enabled=true + payout_type='CPC' — handler should skip with cpc_offer
	// and NOT insert anything. The absence of an INSERT expectation is
	// sufficient: if the handler tried to insert, sqlmock would refuse,
	// the handler would route to "insert_failed", and the test would fail
	// the response-body assertion below.
	mock.ExpectQuery(`FROM mailing_offer_journey_map`).
		WithArgs(testOfferEverflow).
		WillReturnRows(sqlmock.NewRows(journeyMapCols).
			AddRow(testOfferEverflow, testClickJourneyID, "CPC", true))

	rec := clickGET(t, h, "sub1="+testSubID+"&offer_id="+testOfferEverflow)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeClickResp(t, rec)
	assert.Equal(t, "skipped", resp["status"])
	assert.Equal(t, "cpc_offer", resp["reason"])
	_, hasTrigger := resp["trigger_id"]
	assert.False(t, hasTrigger, "CPC skips must NOT queue a trigger row")

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClickPostback_NoClickJourneyConfigured_ReturnsSkipped(t *testing.T) {
	h, mock := newClickPostbackHandler(t)

	// Enabled=true, non-CPC payout, but click_journey_id empty.
	mock.ExpectQuery(`FROM mailing_offer_journey_map`).
		WithArgs(testOfferEverflow).
		WillReturnRows(sqlmock.NewRows(journeyMapCols).
			AddRow(testOfferEverflow, "", "CPM", true))

	rec := clickGET(t, h, "sub1="+testSubID+"&offer_id="+testOfferEverflow)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeClickResp(t, rec)
	assert.Equal(t, "skipped", resp["status"])
	assert.Equal(t, "no_click_journey_configured", resp["reason"])

	require.NoError(t, mock.ExpectationsWereMet())
}

// ----- Phase 3: suppression + idempotency ----------------------------------

func TestClickPostback_AlreadyConverted_ReturnsSkipped(t *testing.T) {
	h, mock := newClickPostbackHandler(t)

	// 1. journey_map → enabled, non-CPC, journey_id set.
	mock.ExpectQuery(`FROM mailing_offer_journey_map`).
		WithArgs(testOfferEverflow).
		WillReturnRows(sqlmock.NewRows(journeyMapCols).
			AddRow(testOfferEverflow, testClickJourneyID, "CPM", true))

	// 2. mailing_offers lookup returns an internal UUID for this offer.
	mock.ExpectQuery(`SELECT id FROM mailing_offers WHERE everflow_offer_id`).
		WithArgs(testOfferEverflow).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(testOfferUUID))

	// 3. Suppression count > 0 → already_converted.
	mock.ExpectQuery(`FROM mailing_offer_suppressions`).
		WithArgs(testOfferUUID, testSubID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	rec := clickGET(t, h, "sub1="+testSubID+"&offer_id="+testOfferEverflow)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeClickResp(t, rec)
	assert.Equal(t, "skipped", resp["status"])
	assert.Equal(t, "already_converted", resp["reason"])

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClickPostback_DuplicateWithinIdempotency_ReturnsSkipped(t *testing.T) {
	h, mock := newClickPostbackHandler(t)

	// Full happy-path mocks UNTIL the duplicate check, which finds a row.
	mock.ExpectQuery(`FROM mailing_offer_journey_map`).
		WithArgs(testOfferEverflow).
		WillReturnRows(sqlmock.NewRows(journeyMapCols).
			AddRow(testOfferEverflow, testClickJourneyID, "CPM", true))

	mock.ExpectQuery(`SELECT id FROM mailing_offers WHERE everflow_offer_id`).
		WithArgs(testOfferEverflow).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(testOfferUUID))

	// Suppression check: not converted.
	mock.ExpectQuery(`FROM mailing_offer_suppressions`).
		WithArgs(testOfferUUID, testSubID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// Idempotency check: already seen → 1.
	mock.ExpectQuery(`FROM mailing_journey_event_triggers[\s\S]*received_at > NOW`).
		WithArgs(testSubID, testOfferEverflow).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	rec := clickGET(t, h, "sub1="+testSubID+"&offer_id="+testOfferEverflow)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeClickResp(t, rec)
	assert.Equal(t, "skipped", resp["status"])
	assert.Equal(t, "duplicate_within_idempotency_window", resp["reason"])

	require.NoError(t, mock.ExpectationsWereMet())
}

// ----- Phase 4: happy path -------------------------------------------------

func TestClickPostback_HappyPath_QueuesTriggerAndReturns200(t *testing.T) {
	h, mock := newClickPostbackHandler(t)

	// 1. journey_map: enabled CPM offer with a click journey.
	mock.ExpectQuery(`FROM mailing_offer_journey_map`).
		WithArgs(testOfferEverflow).
		WillReturnRows(sqlmock.NewRows(journeyMapCols).
			AddRow(testOfferEverflow, testClickJourneyID, "CPM", true))

	// 2. mailing_offers internal UUID.
	mock.ExpectQuery(`SELECT id FROM mailing_offers WHERE everflow_offer_id`).
		WithArgs(testOfferEverflow).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(testOfferUUID))

	// 3. Suppression check: subscriber has NOT converted.
	mock.ExpectQuery(`FROM mailing_offer_suppressions`).
		WithArgs(testOfferUUID, testSubID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// 4. Idempotency check: no recent duplicate.
	mock.ExpectQuery(`FROM mailing_journey_event_triggers[\s\S]*received_at > NOW`).
		WithArgs(testSubID, testOfferEverflow).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// 5. Subscriber email lookup.
	mock.ExpectQuery(`SELECT email FROM mailing_subscribers WHERE id`).
		WithArgs(testSubID).
		WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow(testSubscriberEmail))

	// 6. Sending profile lookup. sub2 carries the brand ROOT, so the handler
	// matches on both the lowercased sub2 ($1) and its computed brand root
	// ($2 = "discountblog.com" for "em.discountblog.com"), preferring the
	// canonical "em.<root>" sending domain.
	mock.ExpectQuery(`SELECT id FROM mailing_sending_profiles`).
		WithArgs(testBrandSub2, "discountblog.com").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(testProfileUUID))

	// 7. Trigger insert. id and sending_profile_id are uuid.UUID values
	// generated/resolved at runtime; both serialize to their canonical
	// lowercase string form via uuid.UUID.Value() so we can match
	// sending_profile_id by string, but the freshly-minted trigger id
	// needs sqlmock.AnyArg().
	//
	// Column order in the INSERT, per the handler:
	//   $1 id                  (uuid.New)
	//   $2 everflow_offer_id   = testOfferEverflow
	//   $3 subscriber_id       = testSubID (uuid.UUID → string)
	//   $4 subscriber_email    = testSubscriberEmail (NULLIF '' to NULL)
	//   $5 sub2_brand          = testBrandSub2 (raw, not lowercased)
	//   $6 sub3_campaign_id    = testCamID
	//   $7 click_id            = testTransactionID
	//   $8 sending_profile_id  = testProfileUUID (uuid.UUID → string)
	//   $9 sending_domain      = testBrandSub2 (lowercased version of sub2)
	//   $10 click_url          = testClickURL
	mock.ExpectExec(`INSERT INTO mailing_journey_event_triggers`).
		WithArgs(
			sqlmock.AnyArg(), // trigger id (uuid.New)
			testOfferEverflow,
			testSubID,
			testSubscriberEmail,
			testBrandSub2,
			testCamID,
			testTransactionID,
			testProfileUUID,
			testBrandSub2,
			testClickURL,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	query := "sub1=" + testSubID +
		"&sub2=" + testBrandSub2 +
		"&sub3=" + testCamID +
		"&offer_id=" + testOfferEverflow +
		"&transaction_id=" + testTransactionID +
		"&click_url=" + testClickURL
	rec := clickGET(t, h, query)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeClickResp(t, rec)
	assert.Equal(t, "queued", resp["status"])
	_, hasReason := resp["reason"]
	assert.False(t, hasReason, "happy-path response must omit the 'reason' key")

	triggerID := resp["trigger_id"]
	assert.NotEmpty(t, triggerID, "queued response must include a trigger_id")
	_, err := uuid.Parse(triggerID)
	assert.NoError(t, err, "trigger_id must be a valid UUID, got %q", triggerID)

	require.NoError(t, mock.ExpectationsWereMet())
}

// ----- Phase 5: parsing surfaces (GET / POST / pure parser) ----------------

// TestClickPostback_JSONBodyFallback_Works probes the parsing path: the
// query string is empty (sub1 absent), so the handler should fall back to
// the JSON body. We use the OfferNotMapped scenario as a "made it past
// parsing" probe — if parsing dropped sub1, the response would be
// no_subscriber_id and the journey_map query would never fire (sqlmock
// would then fail ExpectationsWereMet).
func TestClickPostback_JSONBodyFallback_Works(t *testing.T) {
	h, mock := newClickPostbackHandler(t)

	mock.ExpectQuery(`FROM mailing_offer_journey_map`).
		WithArgs(testOfferEverflow).
		WillReturnError(sql.ErrNoRows)

	body := `{"sub1":"` + testSubID + `","offer_id":"` + testOfferEverflow + `"}`
	rec := clickPOST(t, h, body)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeClickResp(t, rec)
	assert.Equal(t, "skipped", resp["status"])
	assert.Equal(t, "offer_not_mapped", resp["reason"],
		"reaching offer_not_mapped proves parser pulled sub1 from the JSON body")

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClickPostback_GETandPOST_BothWork(t *testing.T) {
	cases := []struct {
		name string
		fire func(t *testing.T, h *EverflowClickPostbackHandler) *httptest.ResponseRecorder
	}{
		{
			name: "GET via query string",
			fire: func(t *testing.T, h *EverflowClickPostbackHandler) *httptest.ResponseRecorder {
				return clickGET(t, h, "sub1="+testSubID+"&offer_id="+testOfferEverflow)
			},
		},
		{
			name: "POST via JSON body",
			fire: func(t *testing.T, h *EverflowClickPostbackHandler) *httptest.ResponseRecorder {
				body := `{"sub1":"` + testSubID + `","offer_id":"` + testOfferEverflow + `"}`
				return clickPOST(t, h, body)
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, mock := newClickPostbackHandler(t)
			mock.ExpectQuery(`FROM mailing_offer_journey_map`).
				WithArgs(testOfferEverflow).
				WillReturnError(sql.ErrNoRows)

			rec := c.fire(t, h)

			require.Equal(t, http.StatusOK, rec.Code)
			resp := decodeClickResp(t, rec)
			assert.Equal(t, "skipped", resp["status"])
			assert.Equal(t, "offer_not_mapped", resp["reason"])

			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// ----- Phase 6: pure-function unit tests -----------------------------------

func TestParseClickPostback_QueryAndBody(t *testing.T) {
	t.Run("query string populates all fields and trims offer_id", func(t *testing.T) {
		// Query has trailing whitespace around offer_id — the parser
		// must TrimSpace it because Everflow sometimes URL-encodes
		// stray spaces from offer-template editing.
		url := "/?sub1=" + testSubID +
			"&sub2=em.discountblog.com" +
			"&sub3=" + testCamID +
			"&offer_id=%20%209539%20%20" +
			"&transaction_id=txn-1" +
			"&click_url=https%3A%2F%2Fexample.com%2Flp"
		req := httptest.NewRequest(http.MethodGet, url, nil)
		in := parseClickPostback(req)

		assert.Equal(t, testSubID, in.SubscriberIDStr)
		assert.Equal(t, "em.discountblog.com", in.Sub2Brand)
		assert.Equal(t, testCamID, in.CampaignIDStr)
		assert.Equal(t, "9539", in.EverflowOfferID, "offer_id should be trimmed")
		assert.Equal(t, "txn-1", in.TransactionID)
		assert.Equal(t, "https://example.com/lp", in.ClickURL)
		assert.NotEqual(t, uuid.Nil, in.subscriberID, "valid sub1 must parse")
		assert.NotEqual(t, uuid.Nil, in.campaignID, "valid sub3 must parse")
	})

	t.Run("empty query falls back to JSON body", func(t *testing.T) {
		body := `{
			"sub1":"` + testSubID + `",
			"sub2":"em.quizfiesta.com",
			"sub3":"` + testCamID + `",
			"offer_id":"  9539  ",
			"transaction_id":"txn-2",
			"click_url":"https://example.com/lp2"
		}`
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		in := parseClickPostback(req)

		assert.Equal(t, testSubID, in.SubscriberIDStr)
		assert.Equal(t, "em.quizfiesta.com", in.Sub2Brand)
		assert.Equal(t, testCamID, in.CampaignIDStr)
		assert.Equal(t, "9539", in.EverflowOfferID, "offer_id from body should also be trimmed")
		assert.Equal(t, "txn-2", in.TransactionID)
		assert.Equal(t, "https://example.com/lp2", in.ClickURL)
		assert.NotEqual(t, uuid.Nil, in.subscriberID)
	})

	t.Run("invalid sub1 leaves subscriberID as uuid.Nil", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?sub1=not-a-uuid&offer_id=9539", nil)
		in := parseClickPostback(req)
		assert.Equal(t, "not-a-uuid", in.SubscriberIDStr)
		assert.Equal(t, uuid.Nil, in.subscriberID,
			"unparseable sub1 must leave subscriberID zero — handler relies on uuid.Nil for the skip check")
	})

	t.Run("missing sub1 triggers all-or-nothing body fallback", func(t *testing.T) {
		// Behavior contract: when sub1 is empty in the query string the
		// parser switches to body-mode and OVERWRITES every field from
		// the body — including any other query params that happened to
		// be set. So `?offer_id=9539` with no body and no sub1 ends up
		// with offer_id wiped to "". This is intentional to keep parity
		// with the existing /everflow/postback (conversion) handler:
		// callers send either a full query payload OR a full JSON body,
		// never a mix. Test documents the contract so a future "merge
		// query + body" refactor doesn't silently change it.
		req := httptest.NewRequest(http.MethodGet, "/?offer_id=9539", nil)
		in := parseClickPostback(req)
		assert.Equal(t, "", in.SubscriberIDStr)
		assert.Equal(t, uuid.Nil, in.subscriberID)
		assert.Equal(t, uuid.Nil, in.campaignID)
		assert.Equal(t, "", in.EverflowOfferID,
			"body fallback overwrites query offer_id even when body is empty")
	})
}

// ----- Phase 7: conversion-event branch on the click endpoint --------------

// TestClickPostback_ConversionEvent_ExitsDrip verifies that a conversion
// postback aimed at the click URL (event=conversion) STOPS the drip — exits
// active enrollments (matched by UUID) and writes a converted suppression —
// rather than enrolling the converter.
func TestClickPostback_ConversionEvent_ExitsDrip(t *testing.T) {
	h, mock := newClickPostbackHandler(t)

	// 1. Dictionary gate (inside exitClickDripEnrollmentsOnConversion).
	mock.ExpectQuery(`FROM mailing_offer_journey_map`).
		WithArgs(testOfferEverflow).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(1))

	// 2. Email lookup (fallback match).
	mock.ExpectQuery(`SELECT email FROM mailing_subscribers WHERE id`).
		WithArgs(testSubID).
		WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow(testSubscriberEmail))

	// 3. Enrollment exit UPDATE (matched by UUID OR email).
	mock.ExpectExec(`UPDATE mailing_journey_enrollments`).
		WithArgs(testOfferEverflow, testSubID, testSubscriberEmail).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 4. Internal offer UUID resolve for suppression.
	mock.ExpectQuery(`SELECT id FROM mailing_offers WHERE everflow_offer_id`).
		WithArgs(testOfferEverflow).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(testOfferUUID))

	// 5. Converted suppression.
	mock.ExpectExec(`INSERT INTO mailing_offer_suppressions`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rec := clickGET(t, h, "sub1="+testSubID+"&offer_id="+testOfferEverflow+"&event=conversion&transaction_id=txn-cv")

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeClickResp(t, rec)
	assert.Equal(t, "converted", resp["status"])

	require.NoError(t, mock.ExpectationsWereMet())
}

// TestClickPostback_ConversionEvent_OfferNotInDict_Skips verifies a conversion
// for an offer not in our dictionary is skipped (no enrollment changes), per
// the operator rule.
func TestClickPostback_ConversionEvent_OfferNotInDict_Skips(t *testing.T) {
	h, mock := newClickPostbackHandler(t)

	const otherOffer = "4321"

	// Dictionary gate → not mapped (helper returns before email/UPDATE).
	mock.ExpectQuery(`FROM mailing_offer_journey_map`).
		WithArgs(otherOffer).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}))

	// Offer UUID resolve still runs but finds nothing → no suppression.
	mock.ExpectQuery(`SELECT id FROM mailing_offers WHERE everflow_offer_id`).
		WithArgs(otherOffer).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	rec := clickGET(t, h, "sub1="+testSubID+"&offer_id="+otherOffer+"&event=conversion")

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeClickResp(t, rec)
	assert.Equal(t, "skipped", resp["status"])
	assert.Equal(t, "offer_not_in_dictionary", resp["reason"])

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestIsConversionEvent(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		{"event=conversion", true},
		{"event=CV", true},
		{"type=sale", true},
		{"postback_type=conv", true},
		{"event_type=conversion_postback", true},
		{"payout=2.50", true},
		{"amount=1.00", true},
		{"payout=0", false},
		{"payout=0.00", false},
		{"event=click", false},
		{"", false},
		{"offer_id=9539&sub1=x", false},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/?"+c.query, nil)
		got := isConversionEvent(req)
		assert.Equal(t, c.want, got, "isConversionEvent(%q)", c.query)
	}
}

func TestIsCPCPayout(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"CPC", true},
		{"cpc", true},
		{" CPC ", true},
		{"CPM", false},
		{"eCPM", false},
		{"", false},
		{"CPA", false},
		{"  cpa  ", false},
		{"CPCFOO", false}, // not equal-fold to CPC
	}
	for _, c := range cases {
		got := isCPCPayout(c.in)
		assert.Equal(t, c.want, got, "isCPCPayout(%q)", c.in)
	}
}
