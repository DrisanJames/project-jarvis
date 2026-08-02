package api

// Tests for ClickDripAdminHandlers (click_drip_admin_handlers.go).
//
// Pattern follows partner_test.go + everflow_click_postback_handler_test.go:
// go-sqlmock with the regex query matcher, httptest for the request/response,
// chi router (via chi.RouteCtxKey) to wire {everflow_offer_id} and
// {sequence_index} into the URL context.
//
// Coverage:
//   - One happy path + at least one error path per method (7 methods).
//   - Cross-cutting validation tests for the pure helpers
//     (payoutTypeAllowed, canonicalPayoutType, parseSequenceIndex).
//
// All tests are prefixed TestClickDripAdmin* so the requested target
// `go test ./internal/api/... -run "TestClickDripAdmin" -v` runs them all.

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── shared fixtures ───────────────────────────────────────────────────────

const (
	cdTestOfferID    = "9539"
	cdTestSeqIdx     = 0
	cdTestSeqIdxStr  = "0"
	cdTestJourneyID  = "click-drip-4touch-72h"
	cdTestSubject    = "You looked at metal roofing — quick reminder"
	cdTestPreheader  = "Your luxury roofing quote is just one click away."
	cdTestFromName   = ""
	cdTestNotes      = "+1h reminder; operator-editable"
)

func newClickDripAdmin(t *testing.T) (*ClickDripAdminHandlers, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
		sqlmock.MonitorPingsOption(false),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.MatchExpectationsInOrder(false)
	return NewClickDripAdminHandlers(db), mock
}

// withURLParams attaches chi URL params to the request so handlers that
// call chi.URLParam pick them up the way the production router does.
func withURLParams(req *http.Request, pairs map[string]string) *http.Request {
	rctx := chi.NewRouteContext()
	for k, v := range pairs {
		rctx.URLParams.Add(k, v)
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func decodeClickDripBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func reminderSubjectCols() []string {
	return []string{
		"everflow_offer_id", "sequence_index", "subject",
		"preheader", "from_name_override", "enabled",
		"notes", "updated_at",
	}
}

func offerJourneyMapCols() []string {
	return []string{
		"everflow_offer_id", "click_journey_id", "payout_type",
		"enabled", "notes", "created_at", "updated_at",
	}
}

// ─── ListReminderSubjects ──────────────────────────────────────────────────

func TestClickDripAdmin_ListReminderSubjects_HappyPath(t *testing.T) {
	h, mock := newClickDripAdmin(t)

	now := time.Now()
	mock.ExpectQuery(`FROM mailing_offer_reminder_subjects`).
		WithArgs(cdTestOfferID).
		WillReturnRows(sqlmock.NewRows(reminderSubjectCols()).
			AddRow(cdTestOfferID, 0, cdTestSubject, cdTestPreheader, "", true, cdTestNotes, now).
			AddRow(cdTestOfferID, 1, "Did you forget?", "Some preheader", "", true, "+6h", now))

	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/offer-reminder-subjects?everflow_offer_id="+cdTestOfferID, nil)
	rec := httptest.NewRecorder()
	h.ListReminderSubjects(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := decodeClickDripBody(t, rec)
	data, ok := body["data"].([]interface{})
	require.True(t, ok, "expected `data` to be an array")
	require.Len(t, data, 2)
	first := data[0].(map[string]interface{})
	assert.Equal(t, cdTestOfferID, first["everflow_offer_id"])
	assert.Equal(t, float64(0), first["sequence_index"])
	assert.Equal(t, cdTestSubject, first["subject"])
	assert.Equal(t, true, first["enabled"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClickDripAdmin_ListReminderSubjects_DBError(t *testing.T) {
	h, mock := newClickDripAdmin(t)

	mock.ExpectQuery(`FROM mailing_offer_reminder_subjects`).
		WithArgs("").
		WillReturnError(sql.ErrConnDone)

	req := httptest.NewRequest(http.MethodGet, "/api/mailing/offer-reminder-subjects", nil)
	rec := httptest.NewRecorder()
	h.ListReminderSubjects(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	body := decodeClickDripBody(t, rec)
	assert.Contains(t, body["error"].(string), "list_reminder_subjects_failed")
	require.NoError(t, mock.ExpectationsWereMet())
}

// ─── GetReminderSubject ────────────────────────────────────────────────────

func TestClickDripAdmin_GetReminderSubject_HappyPath(t *testing.T) {
	h, mock := newClickDripAdmin(t)

	now := time.Now()
	mock.ExpectQuery(`FROM mailing_offer_reminder_subjects[\s\S]*WHERE everflow_offer_id`).
		WithArgs(cdTestOfferID, cdTestSeqIdx).
		WillReturnRows(sqlmock.NewRows(reminderSubjectCols()).
			AddRow(cdTestOfferID, cdTestSeqIdx, cdTestSubject, cdTestPreheader, "", true, cdTestNotes, now))

	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/offer-reminder-subjects/"+cdTestOfferID+"/"+cdTestSeqIdxStr, nil)
	req = withURLParams(req, map[string]string{
		"everflow_offer_id": cdTestOfferID,
		"sequence_index":    cdTestSeqIdxStr,
	})
	rec := httptest.NewRecorder()
	h.GetReminderSubject(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := decodeClickDripBody(t, rec)
	data, ok := body["data"].(map[string]interface{})
	require.True(t, ok, "expected `data` to be an object")
	assert.Equal(t, cdTestOfferID, data["everflow_offer_id"])
	assert.Equal(t, float64(cdTestSeqIdx), data["sequence_index"])
	assert.Equal(t, cdTestSubject, data["subject"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClickDripAdmin_GetReminderSubject_NotFound(t *testing.T) {
	h, mock := newClickDripAdmin(t)

	mock.ExpectQuery(`FROM mailing_offer_reminder_subjects[\s\S]*WHERE everflow_offer_id`).
		WithArgs(cdTestOfferID, 7).
		WillReturnError(sql.ErrNoRows)

	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/offer-reminder-subjects/"+cdTestOfferID+"/7", nil)
	req = withURLParams(req, map[string]string{
		"everflow_offer_id": cdTestOfferID,
		"sequence_index":    "7",
	})
	rec := httptest.NewRecorder()
	h.GetReminderSubject(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	body := decodeClickDripBody(t, rec)
	assert.Contains(t, body["error"].(string), "not found")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClickDripAdmin_GetReminderSubject_InvalidSequenceIndex(t *testing.T) {
	h, _ := newClickDripAdmin(t)

	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/offer-reminder-subjects/"+cdTestOfferID+"/abc", nil)
	req = withURLParams(req, map[string]string{
		"everflow_offer_id": cdTestOfferID,
		"sequence_index":    "abc",
	})
	rec := httptest.NewRecorder()
	h.GetReminderSubject(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	body := decodeClickDripBody(t, rec)
	assert.Contains(t, body["error"].(string), "integer")
}

// ─── UpsertReminderSubject ─────────────────────────────────────────────────

func TestClickDripAdmin_UpsertReminderSubject_HappyPath(t *testing.T) {
	h, mock := newClickDripAdmin(t)

	now := time.Now()
	mock.ExpectQuery(`INSERT INTO mailing_offer_reminder_subjects[\s\S]*ON CONFLICT`).
		WithArgs(cdTestOfferID, cdTestSeqIdx, cdTestSubject, cdTestPreheader, "", true, cdTestNotes, nil).
		WillReturnRows(sqlmock.NewRows(reminderSubjectCols()).
			AddRow(cdTestOfferID, cdTestSeqIdx, cdTestSubject, cdTestPreheader, "", true, cdTestNotes, now))

	body := `{
		"subject": "` + cdTestSubject + `",
		"preheader": "` + cdTestPreheader + `",
		"from_name_override": "",
		"enabled": true,
		"notes": "` + cdTestNotes + `"
	}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/mailing/offer-reminder-subjects/"+cdTestOfferID+"/"+cdTestSeqIdxStr,
		strings.NewReader(body))
	req = withURLParams(req, map[string]string{
		"everflow_offer_id": cdTestOfferID,
		"sequence_index":    cdTestSeqIdxStr,
	})
	rec := httptest.NewRecorder()
	h.UpsertReminderSubject(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	respBody := decodeClickDripBody(t, rec)
	data, ok := respBody["data"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, cdTestSubject, data["subject"])
	assert.Equal(t, true, data["enabled"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClickDripAdmin_UpsertReminderSubject_MissingSubject(t *testing.T) {
	h, mock := newClickDripAdmin(t)

	body := `{"subject":"","preheader":"x"}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/mailing/offer-reminder-subjects/"+cdTestOfferID+"/"+cdTestSeqIdxStr,
		strings.NewReader(body))
	req = withURLParams(req, map[string]string{
		"everflow_offer_id": cdTestOfferID,
		"sequence_index":    cdTestSeqIdxStr,
	})
	rec := httptest.NewRecorder()
	h.UpsertReminderSubject(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	respBody := decodeClickDripBody(t, rec)
	assert.Contains(t, respBody["error"].(string), "subject is required")
	// No DB query should fire because validation happens before the DB call.
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClickDripAdmin_UpsertReminderSubject_SequenceOutOfRange(t *testing.T) {
	h, _ := newClickDripAdmin(t)

	body := `{"subject":"valid"}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/mailing/offer-reminder-subjects/"+cdTestOfferID+"/100",
		strings.NewReader(body))
	req = withURLParams(req, map[string]string{
		"everflow_offer_id": cdTestOfferID,
		"sequence_index":    "100",
	})
	rec := httptest.NewRecorder()
	h.UpsertReminderSubject(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	respBody := decodeClickDripBody(t, rec)
	assert.Contains(t, respBody["error"].(string), "sequence_index must be in")
}

// ─── DeleteReminderSubject ─────────────────────────────────────────────────

func TestClickDripAdmin_DeleteReminderSubject_HappyPath(t *testing.T) {
	h, mock := newClickDripAdmin(t)

	mock.ExpectExec(`DELETE FROM mailing_offer_reminder_subjects`).
		WithArgs(cdTestOfferID, cdTestSeqIdx).
		WillReturnResult(sqlmock.NewResult(0, 1))

	req := httptest.NewRequest(http.MethodDelete,
		"/api/mailing/offer-reminder-subjects/"+cdTestOfferID+"/"+cdTestSeqIdxStr, nil)
	req = withURLParams(req, map[string]string{
		"everflow_offer_id": cdTestOfferID,
		"sequence_index":    cdTestSeqIdxStr,
	})
	rec := httptest.NewRecorder()
	h.DeleteReminderSubject(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := decodeClickDripBody(t, rec)
	assert.Equal(t, true, body["ok"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClickDripAdmin_DeleteReminderSubject_NotFound(t *testing.T) {
	h, mock := newClickDripAdmin(t)

	mock.ExpectExec(`DELETE FROM mailing_offer_reminder_subjects`).
		WithArgs(cdTestOfferID, 9).
		WillReturnResult(sqlmock.NewResult(0, 0))

	req := httptest.NewRequest(http.MethodDelete,
		"/api/mailing/offer-reminder-subjects/"+cdTestOfferID+"/9", nil)
	req = withURLParams(req, map[string]string{
		"everflow_offer_id": cdTestOfferID,
		"sequence_index":    "9",
	})
	rec := httptest.NewRecorder()
	h.DeleteReminderSubject(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ─── ListOfferJourneyMap ───────────────────────────────────────────────────

func TestClickDripAdmin_ListOfferJourneyMap_HappyPath(t *testing.T) {
	h, mock := newClickDripAdmin(t)

	now := time.Now()
	mock.ExpectQuery(`FROM mailing_offer_journey_map`).
		WillReturnRows(sqlmock.NewRows(offerJourneyMapCols()).
			AddRow(cdTestOfferID, cdTestJourneyID, "CPM", true, "live", now, now).
			AddRow("7667", cdTestJourneyID, "eCPM", false, "paused", now, now))

	req := httptest.NewRequest(http.MethodGet, "/api/mailing/offer-journey-map", nil)
	rec := httptest.NewRecorder()
	h.ListOfferJourneyMap(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := decodeClickDripBody(t, rec)
	data, ok := body["data"].([]interface{})
	require.True(t, ok, "expected `data` to be an array")
	require.Len(t, data, 2)
	first := data[0].(map[string]interface{})
	assert.Equal(t, cdTestOfferID, first["everflow_offer_id"])
	assert.Equal(t, "CPM", first["payout_type"])
	assert.Equal(t, true, first["enabled"])
	second := data[1].(map[string]interface{})
	assert.Equal(t, "eCPM", second["payout_type"])
	assert.Equal(t, false, second["enabled"], "enabled=false must round-trip — operator kill switch")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClickDripAdmin_ListOfferJourneyMap_DBError(t *testing.T) {
	h, mock := newClickDripAdmin(t)

	mock.ExpectQuery(`FROM mailing_offer_journey_map`).
		WillReturnError(sql.ErrConnDone)

	req := httptest.NewRequest(http.MethodGet, "/api/mailing/offer-journey-map", nil)
	rec := httptest.NewRecorder()
	h.ListOfferJourneyMap(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	body := decodeClickDripBody(t, rec)
	assert.Contains(t, body["error"].(string), "list_offer_journey_map_failed")
	require.NoError(t, mock.ExpectationsWereMet())
}

// ─── UpsertOfferJourneyMap ─────────────────────────────────────────────────

func TestClickDripAdmin_UpsertOfferJourneyMap_HappyPath(t *testing.T) {
	h, mock := newClickDripAdmin(t)

	now := time.Now()
	mock.ExpectQuery(`INSERT INTO mailing_offer_journey_map[\s\S]*ON CONFLICT`).
		WithArgs(cdTestOfferID, cdTestJourneyID, "CPM", true, "live").
		WillReturnRows(sqlmock.NewRows(offerJourneyMapCols()).
			AddRow(cdTestOfferID, cdTestJourneyID, "CPM", true, "live", now, now))

	body := `{
		"click_journey_id": "` + cdTestJourneyID + `",
		"payout_type": "CPM",
		"enabled": true,
		"notes": "live"
	}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/mailing/offer-journey-map/"+cdTestOfferID,
		strings.NewReader(body))
	req = withURLParams(req, map[string]string{
		"everflow_offer_id": cdTestOfferID,
	})
	rec := httptest.NewRecorder()
	h.UpsertOfferJourneyMap(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	respBody := decodeClickDripBody(t, rec)
	data, ok := respBody["data"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, cdTestOfferID, data["everflow_offer_id"])
	assert.Equal(t, "CPM", data["payout_type"])
	assert.Equal(t, true, data["enabled"])
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestClickDripAdmin_UpsertOfferJourneyMap_EnableFalse confirms the kill
// switch round-trips correctly. Per the project rule (and the
// click-postback handler contract), enabled=false on the journey map
// must halt new enrollments instantly because the postback handler reads
// the table on every request without caching.
func TestClickDripAdmin_UpsertOfferJourneyMap_EnableFalse(t *testing.T) {
	h, mock := newClickDripAdmin(t)

	now := time.Now()
	mock.ExpectQuery(`INSERT INTO mailing_offer_journey_map[\s\S]*ON CONFLICT`).
		WithArgs(cdTestOfferID, cdTestJourneyID, "eCPM", false, "paused").
		WillReturnRows(sqlmock.NewRows(offerJourneyMapCols()).
			AddRow(cdTestOfferID, cdTestJourneyID, "eCPM", false, "paused", now, now))

	body := `{
		"click_journey_id": "` + cdTestJourneyID + `",
		"payout_type": "eCPM",
		"enabled": false,
		"notes": "paused"
	}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/mailing/offer-journey-map/"+cdTestOfferID,
		strings.NewReader(body))
	req = withURLParams(req, map[string]string{
		"everflow_offer_id": cdTestOfferID,
	})
	rec := httptest.NewRecorder()
	h.UpsertOfferJourneyMap(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	respBody := decodeClickDripBody(t, rec)
	data := respBody["data"].(map[string]interface{})
	assert.Equal(t, false, data["enabled"],
		"enabled=false must round-trip — this is the operator kill switch the click-postback handler reads on every postback")
	assert.Equal(t, "eCPM", data["payout_type"],
		"eCPM must preserve the lowercase 'e' (CHECK constraint mirrors it exactly)")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClickDripAdmin_UpsertOfferJourneyMap_InvalidPayoutType(t *testing.T) {
	h, mock := newClickDripAdmin(t)

	body := `{
		"click_journey_id": "` + cdTestJourneyID + `",
		"payout_type": "PIZZA",
		"enabled": true
	}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/mailing/offer-journey-map/"+cdTestOfferID,
		strings.NewReader(body))
	req = withURLParams(req, map[string]string{
		"everflow_offer_id": cdTestOfferID,
	})
	rec := httptest.NewRecorder()
	h.UpsertOfferJourneyMap(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	respBody := decodeClickDripBody(t, rec)
	assert.Contains(t, respBody["error"].(string), "payout_type")
	// No DB query should fire because validation rejects the payload first.
	require.NoError(t, mock.ExpectationsWereMet())
}

// ─── DeleteOfferJourneyMap ─────────────────────────────────────────────────

func TestClickDripAdmin_DeleteOfferJourneyMap_HappyPath(t *testing.T) {
	h, mock := newClickDripAdmin(t)

	mock.ExpectExec(`DELETE FROM mailing_offer_journey_map`).
		WithArgs(cdTestOfferID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	req := httptest.NewRequest(http.MethodDelete,
		"/api/mailing/offer-journey-map/"+cdTestOfferID, nil)
	req = withURLParams(req, map[string]string{
		"everflow_offer_id": cdTestOfferID,
	})
	rec := httptest.NewRecorder()
	h.DeleteOfferJourneyMap(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := decodeClickDripBody(t, rec)
	assert.Equal(t, true, body["ok"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClickDripAdmin_DeleteOfferJourneyMap_NotFound(t *testing.T) {
	h, mock := newClickDripAdmin(t)

	mock.ExpectExec(`DELETE FROM mailing_offer_journey_map`).
		WithArgs("ghost-offer").
		WillReturnResult(sqlmock.NewResult(0, 0))

	req := httptest.NewRequest(http.MethodDelete,
		"/api/mailing/offer-journey-map/ghost-offer", nil)
	req = withURLParams(req, map[string]string{
		"everflow_offer_id": "ghost-offer",
	})
	rec := httptest.NewRecorder()
	h.DeleteOfferJourneyMap(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClickDripAdmin_DeleteOfferJourneyMap_MissingPathParam(t *testing.T) {
	h, _ := newClickDripAdmin(t)

	req := httptest.NewRequest(http.MethodDelete,
		"/api/mailing/offer-journey-map/", nil)
	// Empty everflow_offer_id param — chi would never route this in
	// production, but we test the handler's defensive guard.
	req = withURLParams(req, map[string]string{
		"everflow_offer_id": "",
	})
	rec := httptest.NewRecorder()
	h.DeleteOfferJourneyMap(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	body := decodeClickDripBody(t, rec)
	assert.Contains(t, body["error"].(string), "everflow_offer_id is required")
}

// ─── pure helpers ──────────────────────────────────────────────────────────

func TestClickDripAdmin_PayoutTypeAllowed(t *testing.T) {
	allowed := []string{"CPM", "eCPM", "ECPM", "CPA", "CPL", "CPC", "IO", "PRV", "UNKNOWN",
		"cpm", " cpc ", "Unknown"}
	for _, v := range allowed {
		assert.True(t, payoutTypeAllowed(v), "payoutTypeAllowed(%q) should be true", v)
	}
	for _, v := range []string{"", "PIZZA", "CPCFOO", "cpm-extra"} {
		assert.False(t, payoutTypeAllowed(v), "payoutTypeAllowed(%q) should be false", v)
	}
}

func TestClickDripAdmin_CanonicalPayoutType(t *testing.T) {
	cases := []struct{ in, want string }{
		{"cpm", "CPM"},
		{" CPM ", "CPM"},
		{"eCPM", "eCPM"},
		{"ECPM", "eCPM"},
		{"ecpm", "eCPM"},
		{"cpa", "CPA"},
		{"unknown", "UNKNOWN"},
	}
	for _, c := range cases {
		got := canonicalPayoutType(c.in)
		assert.Equal(t, c.want, got, "canonicalPayoutType(%q)", c.in)
	}
}

func TestClickDripAdmin_ParseSequenceIndex(t *testing.T) {
	cases := []struct {
		in     string
		want   int
		ok     bool
		status int
	}{
		{"0", 0, true, http.StatusOK},
		{"50", 50, true, http.StatusOK},
		{"99", 99, true, http.StatusOK},
		{" 3 ", 3, true, http.StatusOK},
		{"-1", 0, false, http.StatusBadRequest},
		{"100", 0, false, http.StatusBadRequest},
		{"abc", 0, false, http.StatusBadRequest},
		{"", 0, false, http.StatusBadRequest},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		got, ok := parseSequenceIndex(rec, c.in)
		assert.Equal(t, c.ok, ok, "parseSequenceIndex(%q) ok", c.in)
		if c.ok {
			assert.Equal(t, c.want, got, "parseSequenceIndex(%q) value", c.in)
		} else {
			assert.Equal(t, c.status, rec.Code, "parseSequenceIndex(%q) error status", c.in)
		}
	}
}
