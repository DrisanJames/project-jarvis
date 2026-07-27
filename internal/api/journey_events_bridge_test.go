package api

// Tests for JourneyEventsBridge (converter-journey event inlet + HELD prefill
// API). Pattern matches internal/api: go-sqlmock regex matcher + httptest.
//
// Pinned behaviors (operator spec 2026-07-27):
//   - event idempotency per (type, transid, step): ON CONFLICT DO NOTHING;
//     duplicate insert → {"status":"duplicate"}
//   - lead_accepted always dedupes on step='' (once per transid)
//   - prefill is HELD: flag off (default) → 404 even for a VALID token
//   - a bare subscriber uuid is NEVER a valid prefill credential (404)
//   - expired / tampered tokens → 404; valid+armed → field mapping incl.
//     custom_fields (M9 store) values and null street/zip

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/pkg/prefilltoken"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newJourneyBridge(t *testing.T) (*JourneyEventsBridge, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
		sqlmock.MonitorPingsOption(false),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewJourneyEventsBridge(db), mock
}

func postJourneyEvent(t *testing.T, h *JourneyEventsBridge, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/journey/events", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.HandleJourneyEvent(rr, req)
	return rr
}

func TestJourneyEventRecorded(t *testing.T) {
	h, mock := newJourneyBridge(t)
	mock.ExpectExec(`INSERT INTO mailing_journey_events`).
		WithArgs("lead_accepted", "txn-1", "sess-1", "", "marcoxpaez@gmail.com",
			"", sqlmock.AnyArg(), nil).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rr := postJourneyEvent(t, h, `{"type":"lead_accepted","transid":"txn-1",
		"session_id":"sess-1","email":"MarcoXPaez@gmail.com",
		"form_data":{"loan_purpose":"home_improvement"}}`)
	assert.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "recorded", resp["status"])
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestJourneyEventIdempotentDuplicate(t *testing.T) {
	h, mock := newJourneyBridge(t)
	// ON CONFLICT DO NOTHING → 0 rows affected → "duplicate", still 200.
	mock.ExpectExec(`INSERT INTO mailing_journey_events`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	rr := postJourneyEvent(t, h, `{"type":"lead_accepted","transid":"txn-1"}`)
	assert.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "duplicate", resp["status"])
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestJourneyEventSessionProgressStepIsTheDiscriminator(t *testing.T) {
	h, mock := newJourneyBridge(t)
	mock.ExpectExec(`INSERT INTO mailing_journey_events`).
		WithArgs("session_progress", "txn-2", "sess-2", "sub-uuid", "",
			"home_value", sqlmock.AnyArg(), nil).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rr := postJourneyEvent(t, h, `{"type":"session_progress","transid":"txn-2",
		"session_id":"sess-2","sub1":"sub-uuid","form_data":{"step":"home_value"}}`)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestJourneyEventInvalidTypeSkippedWithoutDB(t *testing.T) {
	h, mock := newJourneyBridge(t) // no expectations: any query fails the test
	rr := postJourneyEvent(t, h, `{"type":"something_else","transid":"txn-3"}`)
	assert.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "skipped", resp["status"])
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestJourneyEventMissingTransidSkipped(t *testing.T) {
	h, mock := newJourneyBridge(t)
	rr := postJourneyEvent(t, h, `{"type":"lead_accepted"}`)
	assert.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "skipped", resp["status"])
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestJourneyEventSharedKeyEnforcedWhenSet(t *testing.T) {
	h, mock := newJourneyBridge(t)
	t.Setenv("JOURNEY_EVENTS_KEY", "sekrit")
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/journey/events",
		strings.NewReader(`{"type":"lead_accepted","transid":"txn-4"}`))
	rr := httptest.NewRecorder()
	h.HandleJourneyEvent(rr, req)
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// ----- prefill (HELD, flag-gated) ------------------------------------------

const prefillSubUUID = "2e578331-542e-472e-aa6e-71b8deafecf9"

func getPrefill(t *testing.T, h *JourneyEventsBridge, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/mailing/journey/prefill?token="+token, nil)
	rr := httptest.NewRecorder()
	h.HandlePrefill(rr, req)
	return rr
}

func TestPrefillDisarmedFlagIs404EvenWithValidToken(t *testing.T) {
	h, mock := newJourneyBridge(t) // no DB expectations — disarmed must not touch the DB
	// Provider-approved default is ON; explicit "false" disarms the surface.
	t.Setenv("JOURNEY_PREFILL_ENABLED", "false")
	token := prefilltoken.Mint(prefillSubUUID, time.Hour, prefilltoken.SecretFromEnv())
	rr := getPrefill(t, h, token)
	assert.Equal(t, http.StatusNotFound, rr.Code)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestPrefillRejectsBareUUID(t *testing.T) {
	// THE PII-harvesting pin: a bare subscriber uuid must NEVER resolve, even
	// with the surface armed.
	h, mock := newJourneyBridge(t)
	t.Setenv("JOURNEY_PREFILL_ENABLED", "true")
	rr := getPrefill(t, h, prefillSubUUID)
	assert.Equal(t, http.StatusNotFound, rr.Code)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestPrefillRejectsExpiredAndTamperedTokens(t *testing.T) {
	h, mock := newJourneyBridge(t)
	t.Setenv("JOURNEY_PREFILL_ENABLED", "true")

	expired := prefilltoken.Mint(prefillSubUUID, -time.Hour, prefilltoken.SecretFromEnv())
	assert.Equal(t, http.StatusNotFound, getPrefill(t, h, expired).Code)

	valid := prefilltoken.Mint(prefillSubUUID, time.Hour, prefilltoken.SecretFromEnv())
	tampered := valid[:len(valid)-4] + "0000"
	assert.Equal(t, http.StatusNotFound, getPrefill(t, h, tampered).Code)

	assert.Equal(t, http.StatusNotFound, getPrefill(t, h, "").Code)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestPrefillFieldMappingWhenArmed(t *testing.T) {
	h, mock := newJourneyBridge(t)
	t.Setenv("JOURNEY_PREFILL_ENABLED", "true")

	custom := `{"city":"Little Falls","state":"NJ","property_value":860000,"mortgage_balance":350000}`
	mock.ExpectQuery(`SELECT COALESCE\(first_name,''\), COALESCE\(last_name,''\), email`).
		WithArgs(prefillSubUUID).
		WillReturnRows(sqlmock.NewRows([]string{"first_name", "last_name", "email", "custom"}).
			AddRow("Marco", "Paez", "marcoxpaez@gmail.com", custom))

	token := prefilltoken.Mint(prefillSubUUID, time.Hour, prefilltoken.SecretFromEnv())
	rr := getPrefill(t, h, token)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "Marco", resp["first_name"])
	assert.Equal(t, "Paez", resp["last_name"])
	assert.Equal(t, "marcoxpaez@gmail.com", resp["email"])
	assert.Equal(t, "Little Falls", resp["city"])
	assert.Equal(t, "NJ", resp["state"])
	assert.Equal(t, float64(860000), resp["property_value"])
	assert.Equal(t, float64(350000), resp["mortgage_balance"])
	assert.Nil(t, resp["street"], "street is not held by the platform")
	assert.Nil(t, resp["zip"], "zip is not held by the platform")
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestPrefillUnknownSubscriberIs404(t *testing.T) {
	h, mock := newJourneyBridge(t)
	t.Setenv("JOURNEY_PREFILL_ENABLED", "true")
	mock.ExpectQuery(`SELECT COALESCE\(first_name,''\), COALESCE\(last_name,''\), email`).
		WillReturnError(sql.ErrNoRows)
	token := prefilltoken.Mint(prefillSubUUID, time.Hour, prefilltoken.SecretFromEnv())
	assert.Equal(t, http.StatusNotFound, getPrefill(t, h, token).Code)
	assert.NoError(t, mock.ExpectationsWereMet())
}
