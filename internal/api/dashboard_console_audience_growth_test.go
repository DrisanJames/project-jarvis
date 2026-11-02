package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const audienceGrowthTestOrg = "00000000-0000-0000-0000-000000000001"

func audienceGrowthGet(t *testing.T, s *Server) (int, map[string]interface{}) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/mailing/dashboard/audience-growth", nil)
	r.Header.Set("X-Organization-ID", audienceGrowthTestOrg)
	w := httptest.NewRecorder()
	s.HandleDashboardAudienceGrowth(w, r)
	var out map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestAudienceGrowth_ReadsRollupByISP(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Date(2026, 11, 2, 19, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM mailing_audience_growth_daily").WillReturnRows(
		sqlmock.NewRows([]string{"isp", "acquired", "churned", "computed_at"}).
			AddRow("microsoft", 900, 100, at).
			AddRow("apple", 100, 150, at.Add(-time.Hour)))
	mock.ExpectQuery("total_emails_received BETWEEN 1 AND 4").WillReturnRows(
		sqlmock.NewRows([]string{"count"}).AddRow(175449))

	code, out := audienceGrowthGet(t, &Server{mailingDB: db})
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if out["acquired_7d"] != float64(1000) || out["churned_7d"] != float64(250) || out["net"] != float64(750) {
		t.Fatalf("totals %+v", out)
	}
	if out["ready"] != true || out["activated"] != float64(175449) {
		t.Fatalf("ready/activated %+v", out)
	}
	rows := out["by_isp"].([]interface{})
	apple := rows[1].(map[string]interface{})
	if len(rows) != 2 || apple["isp"] != "apple" || apple["net"] != float64(-50) {
		t.Fatalf("by_isp %+v", rows)
	}
}

// Before the first rollup pass the panel must read as pending, never as +0.
func TestAudienceGrowth_EmptyRollupIsNotReady(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery("FROM mailing_audience_growth_daily").WillReturnRows(
		sqlmock.NewRows([]string{"isp", "acquired", "churned", "computed_at"}))
	mock.ExpectQuery("total_emails_received").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	code, out := audienceGrowthGet(t, &Server{mailingDB: db})
	if code != http.StatusOK || out["ready"] != false {
		t.Fatalf("code=%d out=%+v", code, out)
	}
	if rows := out["by_isp"].([]interface{}); len(rows) != 0 {
		t.Fatalf("by_isp %+v", rows)
	}
}

// A failed activated read is unknown, not zero, and must not fail the panel.
func TestAudienceGrowth_ActivatedFailureIsNull(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery("FROM mailing_audience_growth_daily").WillReturnRows(
		sqlmock.NewRows([]string{"isp", "acquired", "churned", "computed_at"}).AddRow("gmail", 1, 0, time.Now()))
	mock.ExpectQuery("total_emails_received").WillReturnError(errors.New("timeout"))

	code, out := audienceGrowthGet(t, &Server{mailingDB: db})
	if code != http.StatusOK || out["activated"] != nil {
		t.Fatalf("code=%d activated=%v", code, out["activated"])
	}
}
