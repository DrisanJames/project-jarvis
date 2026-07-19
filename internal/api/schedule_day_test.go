package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// Regression: a day with no board-family campaigns (or no campaigns at all)
// must serialize board_waves/sidecars as [], never null — a null crashed the
// portal Schedule tab ("board_waves is not iterable" at the frontend spread).
func TestHandleScheduleDay_EmptyDayEmitsArraysNotNull(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := &Server{mailingDB: db}

	mock.ExpectQuery(`FROM mailing_campaigns c`).
		WillReturnRows(sqlmock.NewRows([]string{"name", "scheduled_at", "total_recipients", "sending_domain", "isp_quotas"}))

	req := httptest.NewRequest("GET", "/api/mailing/schedule/day?date=2026-07-19", nil)
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rec := httptest.NewRecorder()
	s.HandleScheduleDay(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"board_waves", "sidecars", "offers"} {
		raw, ok := resp[key]
		if !ok {
			t.Fatalf("%s missing from response", key)
		}
		if string(raw) == "null" {
			t.Fatalf("%s = null, want []", key)
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			t.Fatalf("%s is not an array: %v (raw=%s)", key, err, raw)
		}
		if len(arr) != 0 {
			t.Fatalf("%s = %s, want empty", key, raw)
		}
	}
}
