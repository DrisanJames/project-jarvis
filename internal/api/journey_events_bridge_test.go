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
	mock.ExpectQuery(`INSERT INTO mailing_journey_events`).
		WithArgs("lead_accepted", "txn-1", "sess-1", "", "", "marcoxpaez@gmail.com",
			"", sqlmock.AnyArg(), nil).
		WillReturnRows(sqlmock.NewRows([]string{"inserted"}).AddRow(true))

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
	// ON CONFLICT DO UPDATE → xmax<>0 → "enriched", still 200.
	mock.ExpectQuery(`INSERT INTO mailing_journey_events`).
		WillReturnRows(sqlmock.NewRows([]string{"inserted"}).AddRow(false))

	rr := postJourneyEvent(t, h, `{"type":"lead_accepted","transid":"txn-1"}`)
	assert.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "enriched", resp["status"])
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestJourneyEventSessionProgressStepIsTheDiscriminator(t *testing.T) {
	h, mock := newJourneyBridge(t)
	mock.ExpectQuery(`INSERT INTO mailing_journey_events`).
		WithArgs("session_progress", "txn-2", "sess-2", "sub-uuid", "", "",
			"home_value", sqlmock.AnyArg(), nil).
		WillReturnRows(sqlmock.NewRows([]string{"inserted"}).AddRow(true))

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

// TestJourneyEventCapturesAffid pins the 2026-08-04 contract addition.
//
// Before this, the platform never received the funnel's traffic source, so no
// per-affiliate rule could be expressed and — worse — no per-affiliate coverage
// could be MEASURED. Abandon recovery was silently dark for whole affiliates:
// 1,093 of 2,378 abandons (46%) were unreachable, and 36 of the 103 sessions
// that reached the `email` step never sent us the address, with nothing in the
// platform able to show it. affid is what makes that reportable per affiliate.
func TestJourneyEventCapturesAffid(t *testing.T) {
	h, mock := newJourneyBridge(t)
	mock.ExpectQuery(`INSERT INTO mailing_journey_events`).
		WithArgs("session_progress", "txn-9", "sess-9", "PMK_iT2", "10",
			"gbryan52@icloud.com", "email", sqlmock.AnyArg(), nil).
		WillReturnRows(sqlmock.NewRows([]string{"inserted"}).AddRow(true))

	rr := postJourneyEvent(t, h, `{"type":"session_progress","transid":"txn-9",
		"session_id":"sess-9","sub1":"PMK_iT2","affid":"10",
		"email":"GBryan52@iCloud.com","form_data":{"step":"email"}}`)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestJourneyEventMissingAffidDegradesToEmpty: an affiliate that omits affid
// must still have its event stored. Losing the event would be strictly worse
// than not knowing the source.
func TestJourneyEventMissingAffidDegradesToEmpty(t *testing.T) {
	h, mock := newJourneyBridge(t)
	mock.ExpectQuery(`INSERT INTO mailing_journey_events`).
		WithArgs("session_progress", "txn-10", "sess-10", "sub-x", "", "",
			"zip", sqlmock.AnyArg(), nil).
		WillReturnRows(sqlmock.NewRows([]string{"inserted"}).AddRow(true))

	rr := postJourneyEvent(t, h, `{"type":"session_progress","transid":"txn-10",
		"session_id":"sess-10","sub1":"sub-x","form_data":{"step":"zip"}}`)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestJourneyEventEnrichmentFillsBlankEmail is the regression guard for the
// 2026-08-04 data-loss bug.
//
// The funnel fires TWO beacons per step with the SAME step id: `view` on
// arrival (no values) then `complete` on advance (values, incl. the typed
// email). Under the old ON CONFLICT DO NOTHING the view landed first and the
// complete — the one carrying the address — was thrown away as a duplicate.
// Live proof before the fix: posting view-then-complete stored email=”.
// Consequence: 4,399 session_progress events with ZERO emails and 1,093 of
// 2,378 abandoned sessions unreachable.
//
// The SQL must therefore enrich on conflict, and monotonically.
func TestJourneyEventEnrichmentFillsBlankEmail(t *testing.T) {
	sql := strings.Join(strings.Fields(insertJourneyEventSQL), " ")
	assert.Contains(t, sql, "ON CONFLICT (event_type, transid, step) DO UPDATE",
		"a repeat beacon MUST enrich — DO NOTHING discards the complete-beacon email")
	// Monotonic: a blank incoming value can never erase a stored one.
	assert.Contains(t, sql, "email = COALESCE(NULLIF(EXCLUDED.email, ''), mailing_journey_events.email)")
	assert.Contains(t, sql, "affid = COALESCE(NULLIF(EXCLUDED.affid, ''), mailing_journey_events.affid)")
	// RowsAffected cannot distinguish insert from update under DO UPDATE.
	assert.Contains(t, sql, "RETURNING (xmax = 0)")
}

// TestJourneyEventEnrichedStatusOnConflict: the second beacon reports
// "enriched", not "recorded" — it is not a new event, it completed one.
func TestJourneyEventEnrichedStatusOnConflict(t *testing.T) {
	h, mock := newJourneyBridge(t)
	mock.ExpectQuery(`INSERT INTO mailing_journey_events`).
		WillReturnRows(sqlmock.NewRows([]string{"inserted"}).AddRow(false))

	rr := postJourneyEvent(t, h, `{"type":"session_progress","transid":"txn-11",
		"session_id":"s","email":"late@example.com","form_data":{"step":"email"}}`)
	assert.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "enriched", resp["status"])
	assert.NoError(t, mock.ExpectationsWereMet())
}
