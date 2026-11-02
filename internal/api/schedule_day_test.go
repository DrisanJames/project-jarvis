package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
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
		WillReturnRows(sqlmock.NewRows([]string{"name", "scheduled_at", "total_recipients", "vendor_type", "routing_mode", "ip_pool", "via_ses", "isp_quotas"}))

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
	for _, key := range []string{"offers"} {
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
	var bands []scheduleBand
	if err := json.Unmarshal(resp["bands"], &bands); err != nil {
		t.Fatalf("bands: %v (raw=%s)", err, resp["bands"])
	}
	if len(bands) != 3 || bands[0].Key != "ses" || bands[1].Key != "pmta" || bands[2].Key != "kumo" {
		t.Fatalf("bands = %s, want ses, pmta, kumo", resp["bands"])
	}
	if !strings.Contains(string(resp["bands"]), `"lanes":[]`) {
		t.Fatalf("empty band lanes must be [], got %s", resp["bands"])
	}
}

func TestScheduleRoute(t *testing.T) {
	cases := []struct {
		name, vendor, routing, pool string
		viaSES                      bool
		want                        string
	}{
		{"kumo profile carries vendor pmta", "pmta", "kumo", "", false, "kumo"},
		{"kumo wins over a ses-looking pool", "pmta", "kumo", "ses-relay-a", false, "kumo"},
		{"direct ses vendor", "ses", "", "", false, "ses"},
		{"pmta relaying to ses by flag", "pmta", "", "", true, "ses"},
		{"pmta relaying to ses by pool", "pmta", "", "db-ses-pool", false, "ses"},
		{"gmail relay pool", "pmta", "", "ses-relay-a", false, "ses"},
		{"dedicated pmta", "pmta", "", "nx-pool", false, "pmta"},
		{"no profile", "", "", "", false, "pmta"},
	}
	for _, c := range cases {
		if got := scheduleRoute(c.vendor, c.routing, c.pool, c.viaSES); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestScheduleLaneOffer(t *testing.T) {
	cases := []struct{ in, lane, offer string }{
		{"11022026 - HT - Liberty-A", "Liberty-A", "Liberty-A"},
		{"11022026 - NX-at.historythinking.com - NL-COLD-NEWSLETTER-D43", "NL-COLD-NEWSLETTER", "NL-COLD-NEWSLETTER-D43"},
		{"11022026 - DBZ - NL-WARMUP-NEWSLETTER-COLO2 - 0003", "NL-WARMUP-NEWSLETTER-COLO2", "NL-WARMUP-NEWSLETTER-COLO2"},
		{"11022026 - trb - KUMO-WARM - OFFER-YH-eHSav-D22", "KUMO-WARM", "OFFER-YH-eHSav-D22"},
		{"11022026 - HT - LOANDEPOT-RELEASE-R2-W1", "LOANDEPOT-RELEASE", "LOANDEPOT-RELEASE-R2-W1"},
		{"odd name", "Other", "Other"},
	}
	for _, c := range cases {
		lane, offer := scheduleLaneOffer(c.in)
		if lane != c.lane || offer != c.offer {
			t.Errorf("%s: got (%s, %s), want (%s, %s)", c.in, lane, offer, c.lane, c.offer)
		}
	}
}
