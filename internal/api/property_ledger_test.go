package api

// Step 17/18 fixtures (Vector A plan rev4). Permanent fixtures (I-11):
//   - the list SQL reads property_intro_counters and NEVER partner_clean_queue
//     (no portal-route live aggregation).
//   - update validation: unknown brand/isp, empty update, hold-without-reason,
//     missing lock_version → 400 before any write.
//   - CAS: stale lock_version → 409 carrying the current lock_version.
//   - ceiling: PROPERTY_LEDGER_TOTAL_MAX unset + an INCREASE → 422 (never
//     unlimited).
//   - global hold: stale flag lock_version → 409.
//
// Full tx semantics (version rows, supersede, hold intervals, next-day
// promotion) are pinned against real PG in the integration suite.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestPropertyLedgerListSQLCountersOnly(t *testing.T) {
	if !strings.Contains(propertyLedgerListSQL, "property_intro_counters") {
		t.Fatal("list SQL must read property_intro_counters (counter-backed list, Step 17)")
	}
	if strings.Contains(propertyLedgerListSQL, "partner_clean_queue") {
		t.Fatal("list SQL must NEVER aggregate partner_clean_queue live (plan Step 17)")
	}
}

func newLedgerServiceWithMock(t *testing.T) (*PMTACampaignService, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &PMTACampaignService{db: db}, mock
}

func postJSON(t *testing.T, h http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestPropertyLedgerUpdateValidation(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	cases := []struct {
		name string
		body string
	}{
		{"unknown brand", `{"brand":"zz","isp":"gmail","daily_budget":5,"lock_version":0}`},
		{"unknown isp", `{"brand":"db","isp":"msft","daily_budget":5,"lock_version":0}`},
		{"kumo brand excluded", `{"brand":"mpf","isp":"gmail","daily_budget":5,"lock_version":0}`},
		{"nothing to update", `{"brand":"db","isp":"gmail","lock_version":0}`},
		{"hold without reason", `{"brand":"db","isp":"gmail","hold":true,"lock_version":0}`},
		{"missing lock_version", `{"brand":"db","isp":"gmail","daily_budget":5}`},
		{"negative budget", `{"brand":"db","isp":"gmail","daily_budget":-1,"lock_version":0}`},
	}
	for _, c := range cases {
		rec := postJSON(t, s.HandleUpdatePropertyLedger, "/api/mailing/pmta-campaign/property-ledger/update", c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400 (%s)", c.name, rec.Code, rec.Body.String())
		}
	}
	// None of the rejects may have touched the DB.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("validation must reject before any DB access: %v", err)
	}
}

func TestPropertyLedgerUpdateCAS409(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT daily_budget, hold, lock_version, pending_budget, pending_effective_day, min_budget, max_budget`).
		WithArgs("db", "gmail").
		WillReturnRows(sqlmock.NewRows(
			[]string{"daily_budget", "hold", "lock_version", "pending_budget", "pending_effective_day", "min_budget", "max_budget"}).
			AddRow(100, false, int64(5), nil, nil, nil, nil))
	mock.ExpectRollback()

	rec := postJSON(t, s.HandleUpdatePropertyLedger, "/x",
		`{"brand":"db","isp":"gmail","daily_budget":120,"lock_version":4}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale lock_version: got %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"lock_version":5`) {
		t.Fatalf("409 must carry the CURRENT lock_version: %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("CAS flow: %v", err)
	}
}

func TestPropertyLedgerCeilingUnsetRefusesIncrease(t *testing.T) {
	t.Setenv("PROPERTY_LEDGER_TOTAL_MAX", "")
	s, mock := newLedgerServiceWithMock(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT daily_budget, hold, lock_version, pending_budget`).
		WithArgs("db", "gmail").
		WillReturnRows(sqlmock.NewRows(
			[]string{"daily_budget", "hold", "lock_version", "pending_budget", "pending_effective_day", "min_budget", "max_budget"}).
			AddRow(100, false, int64(0), nil, nil, nil, nil))
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	rec := postJSON(t, s.HandleUpdatePropertyLedger, "/x",
		`{"brand":"db","isp":"gmail","daily_budget":200,"lock_version":0}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("ceiling-unset increase: got %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ceiling unset") {
		t.Fatalf("422 must say the ceiling is unset: %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ceiling flow: %v", err)
	}
}

func TestPropertyLedgerGlobalHoldCAS409(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT value, lock_version FROM property_ledger_flags`).
		WillReturnRows(sqlmock.NewRows([]string{"value", "lock_version"}).AddRow(false, int64(3)))
	mock.ExpectRollback()

	rec := postJSON(t, s.HandleGlobalHold, "/x",
		`{"value":true,"reason":"incident","lock_version":2}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale flag lock_version: got %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("global-hold CAS flow: %v", err)
	}
}

func TestPropertyLedgerGlobalHoldRequiresReasonAndValue(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	for name, body := range map[string]string{
		"missing value":        `{"reason":"x","lock_version":0}`,
		"missing reason":       `{"value":true,"lock_version":0}`,
		"missing lock_version": `{"value":true,"reason":"x"}`,
	} {
		rec := postJSON(t, s.HandleGlobalHold, "/x", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, rec.Code)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("validation must reject before any DB access: %v", err)
	}
}

// TestPropertyLedgerListExposesCapBreachConfig pins the OBSERVABILITY half of
// the cap-breach shutoff (operator 2026-08-20). The screen must be able to tell
// the operator whether the automatic shutoff is armed, detect-only, or off —
// a gate the operator cannot see is the "documented gate that no-ops" failure
// this repo has shipped before.
func TestPropertyLedgerListExposesCapBreachConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		expects []string
	}{
		{
			"armed by default",
			nil,
			[]string{`"enabled":true`, `"detect_only":false`, `"threshold_pct":150`, `"min_excess":100`, `"actor":"cap-breach-detector"`},
		},
		{
			"kill switch is visible, not silent",
			map[string]string{"PROPERTY_CAP_BREACH_SHUTOFF_DISABLED": "1"},
			[]string{`"enabled":false`},
		},
		{
			"detect-only is visible, not silent",
			map[string]string{"PROPERTY_CAP_BREACH_DETECT_ONLY": "1"},
			[]string{`"enabled":true`, `"detect_only":true`},
		},
		{
			"threshold override is reported",
			map[string]string{"PROPERTY_CAP_BREACH_PCT": "200", "PROPERTY_CAP_BREACH_MIN_EXCESS": "250"},
			[]string{`"threshold_pct":200`, `"min_excess":250`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			s, mock := newLedgerServiceWithMock(t)
			mock.ExpectQuery(`FROM partner_drip_brand_budgets b`).
				WillReturnRows(sqlmock.NewRows(ledgerListCols()))
			mock.ExpectQuery(`FROM ses_vdm_daily`).
				WillReturnRows(sqlmock.NewRows([]string{"identity", "isp",
					"sent", "delivered", "opens", "clicks",
					"sent_7d", "delivered_7d", "opens_7d", "clicks_7d"}))

			req := httptest.NewRequest("GET", "/api/mailing/pmta-campaign/property-ledger", nil)
			rec := httptest.NewRecorder()
			s.HandleListPropertyLedger(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"cap_breach":`) {
				t.Fatalf("list response must carry the cap_breach block: %s", body)
			}
			for _, want := range tc.expects {
				if !strings.Contains(body, want) {
					t.Errorf("cap_breach block missing %s in: %s", want, body)
				}
			}
		})
	}
}
