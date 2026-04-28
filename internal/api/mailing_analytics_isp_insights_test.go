package api

// Tests for HandleISPSendingInsights and its pure helpers.
//
// The design goal for this endpoint is that it MUST be extremely testable:
// it is reporting on existing tables, nothing is written, and every boundary
// (days whitelist, MST window alignment, optional isp filter, optional
// sending_domain filter, top_campaigns rollup) must be verifiable in
// isolation without a live database.
//
// Layers covered here:
//   1. parseISPInsightsDays          — whitelist + defaulting for ?days=
//   2. sanitizeISPKey                — lowercase/trim semantics
//   3. computeMSTWindow              — MST-midnight alignment across DST
//   4. HandleISPSendingInsights      — wires the above + returns expected
//                                      response shape (via sqlmock)
//
// None of these tests hit a real database. All SQL is mocked via sqlmock
// using non-strict ordering and regex matchers — the handler fires several
// parallel queries and we care about the response shape and parameter
// propagation, not query-level ordering.

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// parseISPInsightsDays
// ─────────────────────────────────────────────────────────────────────────────

func TestParseISPInsightsDays(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"blank defaults to 7", "", 7},
		{"whitespace defaults to 7", "   ", 7},
		{"explicit 3", "3", 3},
		{"explicit 7", "7", 7},
		{"explicit 14", "14", 14},
		{"out of whitelist 1 falls back to 7", "1", 7},
		{"out of whitelist 5 falls back to 7", "5", 7},
		{"out of whitelist 30 falls back to 7", "30", 7},
		{"out of whitelist 90 falls back to 7", "90", 7},
		{"negative falls back to 7", "-3", 7},
		{"zero falls back to 7", "0", 7},
		{"non-numeric falls back to 7", "abc", 7},
		{"float-ish falls back to 7", "7.5", 7},
		{"leading+trailing whitespace on 14", "  14  ", 14},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseISPInsightsDays(c.in)
			if got != c.want {
				t.Errorf("parseISPInsightsDays(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// buildISPDomainCaseSQL — unresolved_subscriber bucket
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildISPDomainCaseSQL_UnresolvedBucketLeadsCase(t *testing.T) {
	sql := buildISPDomainCaseSQL()

	// The unresolved_subscriber check must be emitted BEFORE any IN-list
	// branches: dom can only be NULL/'' here, so checking it first avoids
	// any chance of NULL slipping into an IN-comparison and falling through
	// to ELSE 'other'.
	idxUnresolved := strings.Index(sql, "'unresolved_subscriber'")
	idxFirstIn := strings.Index(sql, "WHEN dom IN")
	if idxUnresolved == -1 {
		t.Fatalf("expected 'unresolved_subscriber' bucket in CASE SQL; got:\n%s", sql)
	}
	if idxFirstIn == -1 {
		t.Fatalf("expected at least one 'WHEN dom IN' branch; got:\n%s", sql)
	}
	if idxUnresolved >= idxFirstIn {
		t.Errorf("'unresolved_subscriber' WHEN must come before 'WHEN dom IN' branches\nsql=%s",
			sql)
	}
	if !strings.Contains(sql, "WHEN dom IS NULL OR dom = '' THEN 'unresolved_subscriber'") {
		t.Errorf("expected NULL/empty guard for unresolved_subscriber; got:\n%s", sql)
	}
	if !strings.Contains(sql, "ELSE 'other'") {
		t.Errorf("expected ELSE 'other' fallthrough; got:\n%s", sql)
	}
}

func TestISPLabels_HasUnresolvedDisplayName(t *testing.T) {
	got, ok := ispLabels["unresolved_subscriber"]
	if !ok {
		t.Fatalf("ispLabels missing 'unresolved_subscriber'")
	}
	if got == "" || got == "unresolved_subscriber" {
		t.Errorf("ispLabels[unresolved_subscriber] should be human-readable, got %q", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// sanitizeISPKey
// ─────────────────────────────────────────────────────────────────────────────

func TestSanitizeISPKey(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"   ", ""},
		{"gmail", "gmail"},
		{"Gmail", "gmail"},
		{"  GMAIL  ", "gmail"},
		{"YaHoO", "yahoo"},
		{"Microsoft", "microsoft"},
	}
	for _, c := range cases {
		if got := sanitizeISPKey(c.in); got != c.want {
			t.Errorf("sanitizeISPKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// computeMSTWindow
// ─────────────────────────────────────────────────────────────────────────────

// helper: parse a time in America/Denver so test fixtures read as-written.
func mst(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", s, mstLoc)
	require.NoError(t, err)
	return parsed
}

func TestComputeMSTWindow_EndIsExactInputAsUTC(t *testing.T) {
	now := mst(t, "2026-04-20 09:43:00") // 9:43 AM MDT on Apr 20 2026
	_, end := computeMSTWindow(now, 7)
	if !end.Equal(now.UTC()) {
		t.Errorf("end should equal now in UTC; got %v, want %v", end, now.UTC())
	}
	if end.Location() != time.UTC {
		t.Errorf("end should be UTC, got %v", end.Location())
	}
}

func TestComputeMSTWindow_StartIsMSTMidnight(t *testing.T) {
	now := mst(t, "2026-04-20 09:43:00")
	start, _ := computeMSTWindow(now, 7)
	startMST := start.In(mstLoc)

	if startMST.Hour() != 0 || startMST.Minute() != 0 || startMST.Second() != 0 {
		t.Errorf("start should be MST midnight; got %v", startMST)
	}

	// 7-day window ending on Apr 20 → start on Apr 14.
	wantDay := time.Date(2026, 4, 14, 0, 0, 0, 0, mstLoc)
	if !startMST.Equal(wantDay) {
		t.Errorf("start MST = %v, want %v (7-day window incl. today)", startMST, wantDay)
	}
}

func TestComputeMSTWindow_DaysVariants(t *testing.T) {
	now := mst(t, "2026-04-20 09:43:00")
	cases := []struct {
		days  int
		wantY int
		wantM time.Month
		wantD int
	}{
		{3, 2026, 4, 18},  // 3-day window: Apr 18, 19, 20
		{7, 2026, 4, 14},  // 7-day window: Apr 14..20
		{14, 2026, 4, 7},  // 14-day window: Apr 7..20
	}
	for _, c := range cases {
		start, _ := computeMSTWindow(now, c.days)
		s := start.In(mstLoc)
		if s.Year() != c.wantY || s.Month() != c.wantM || s.Day() != c.wantD {
			t.Errorf("days=%d: start MST = %v, want %04d-%02d-%02d",
				c.days, s, c.wantY, c.wantM, c.wantD)
		}
	}
}

func TestComputeMSTWindow_DSTSpringForward(t *testing.T) {
	// US DST starts 2026-03-08: 02:00 MST → 03:00 MDT (lose an hour).
	// A 7-day window ending on 2026-03-10 09:43 MDT should still start at
	// MST midnight on 2026-03-04 (pre-DST), even though the window crosses
	// the transition. computeMSTWindow uses named-zone arithmetic so the
	// wall-clock midnight is preserved on both sides of DST.
	now := mst(t, "2026-03-10 09:43:00")
	start, end := computeMSTWindow(now, 7)
	s := start.In(mstLoc)
	if s.Hour() != 0 || s.Minute() != 0 {
		t.Errorf("DST-straddle start should still be MST midnight; got %v", s)
	}
	if s.Year() != 2026 || s.Month() != 3 || s.Day() != 4 {
		t.Errorf("DST-straddle start = %v, want 2026-03-04 00:00", s)
	}
	if !end.Equal(now.UTC()) {
		t.Errorf("end should equal now.UTC() even across DST; got %v, want %v", end, now.UTC())
	}
}

func TestComputeMSTWindow_DSTFallBack(t *testing.T) {
	// US DST ends 2026-11-01: 02:00 MDT → 01:00 MST (gain an hour).
	now := mst(t, "2026-11-03 09:43:00")
	start, _ := computeMSTWindow(now, 7)
	s := start.In(mstLoc)
	if s.Hour() != 0 || s.Minute() != 0 {
		t.Errorf("fall-back start should be local midnight; got %v", s)
	}
	if s.Year() != 2026 || s.Month() != 10 || s.Day() != 28 {
		t.Errorf("fall-back start = %v, want 2026-10-28 00:00", s)
	}
}

func TestComputeMSTWindow_JustAfterMSTMidnight(t *testing.T) {
	// 00:05 MST on Apr 20: the day has just ticked over, so today's bucket
	// is Apr 20 and a 3-day window starts on Apr 18.
	now := mst(t, "2026-04-20 00:05:00")
	start, _ := computeMSTWindow(now, 3)
	s := start.In(mstLoc)
	if s.Year() != 2026 || s.Month() != 4 || s.Day() != 18 {
		t.Errorf("00:05 MST anchor: start = %v, want 2026-04-18 00:00", s)
	}
}

func TestComputeMSTWindow_JustBeforeMSTMidnight(t *testing.T) {
	// 23:58 MST on Apr 19: still Apr 19 local, 3-day window starts Apr 17.
	now := mst(t, "2026-04-19 23:58:00")
	start, _ := computeMSTWindow(now, 3)
	s := start.In(mstLoc)
	if s.Year() != 2026 || s.Month() != 4 || s.Day() != 17 {
		t.Errorf("23:58 MST anchor: start = %v, want 2026-04-17 00:00", s)
	}
}

func TestComputeMSTWindow_UTCInputConvertsCorrectly(t *testing.T) {
	// 06:00 UTC on Apr 20 = 00:00 MDT on Apr 20 (MDT is UTC-6).
	// A 3-day window anchored there should start at MST midnight on Apr 18.
	now := time.Date(2026, 4, 20, 6, 0, 0, 0, time.UTC)
	start, _ := computeMSTWindow(now, 3)
	s := start.In(mstLoc)
	if s.Year() != 2026 || s.Month() != 4 || s.Day() != 18 || s.Hour() != 0 {
		t.Errorf("UTC-input anchor: start = %v, want 2026-04-18 00:00 MDT", s)
	}
}

func TestComputeMSTWindow_InvalidDaysFallsBackTo7(t *testing.T) {
	now := mst(t, "2026-04-20 09:43:00")
	start, _ := computeMSTWindow(now, 0)
	s := start.In(mstLoc)
	// Fallback: 7-day window → start on Apr 14.
	if s.Day() != 14 {
		t.Errorf("days=0 should fall back to 7: start day = %d, want 14", s.Day())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// HandleISPSendingInsights — response shape + parameter propagation
// ─────────────────────────────────────────────────────────────────────────────

// newInsightsTestService wires an AdvancedMailingService against a sqlmock DB
// configured for non-strict expectation ordering — the handler runs several
// independent queries and the order is an implementation detail we should
// not lock in.
func newInsightsTestService(t *testing.T) (*AdvancedMailingService, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	mock.MatchExpectationsInOrder(false)
	t.Cleanup(func() { db.Close() })
	return &AdvancedMailingService{db: db}, mock, db
}

// expectSetTimeouts registers N `SET statement_timeout` expectations. The
// handler now fans out its queries across N pooled connections and runs
// the timeout bump on each one, so the mock needs to accept the SET as
// many times as there are parallel queries.
func expectSetTimeouts(mock sqlmock.Sqlmock, n int) {
	for i := 0; i < n; i++ {
		mock.ExpectExec(`SET statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	}
}

// expectBaseQueries sets up the queries the handler always runs when no
// isp filter is present: sending_domains list, daily rollup, bounce
// categories, hourly deferrals, quotas. That's 5 queries → 5 SET.
// Rows are empty by default (callers can override before this is invoked
// if they need specific data).
func expectBaseQueries(mock sqlmock.Sqlmock) {
	expectSetTimeouts(mock, 5)

	mock.ExpectQuery(`SELECT DISTINCT LOWER\(COALESCE\(NULLIF\(sending_domain,''\),'unknown'\)\).*FROM mailing_tracking_events`).
		WillReturnRows(sqlmock.NewRows([]string{"sd"}))

	// Match the per-ISP daily rollup specifically (by_sending_domain also
	// contains the MST cast, so we pin on `GROUP BY isp, day` here).
	mock.ExpectQuery(`GROUP BY isp, day`).
		WillReturnRows(sqlmock.NewRows([]string{
			"isp", "day", "sent", "delivered", "hard_bounces", "soft_bounces",
			"deferred", "complained", "opened", "mpp_opens",
		}))

	mock.ExpectQuery(`WHERE d\.event_type = 'bounced'\s*GROUP BY isp, category`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "category", "cnt"}))

	mock.ExpectQuery(`EXTRACT\(HOUR FROM d\.event_at\)`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "hr", "cnt"}))

	mock.ExpectQuery(`FROM mailing_campaign_isp_plans`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "quota"}))
}

// expectBySDQuery registers the per-sending-domain rollup that fires only
// when ispFilter is set. Columns match the handler's scan order.
func expectBySDQuery(mock sqlmock.Sqlmock, args ...driver.Value) {
	q := mock.ExpectQuery(`GROUP BY sd, day`)
	if len(args) > 0 {
		q = q.WithArgs(args...)
	}
	q.WillReturnRows(sqlmock.NewRows([]string{
		"sd", "day", "sent", "delivered", "hard_bounces", "soft_bounces",
		"deferred", "opened", "clicked",
	}))
}

func TestHandleISPSendingInsights_DefaultDaysIs7(t *testing.T) {
	svc, mock, _ := newInsightsTestService(t)
	expectBaseQueries(mock)

	req := httptest.NewRequest(http.MethodGet, "/api/mailing/analytics/isp-sending-insights", nil)
	rec := httptest.NewRecorder()
	svc.HandleISPSendingInsights(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	if v, _ := resp["days"].(float64); v != 7 {
		t.Errorf("default days = %v, want 7", resp["days"])
	}
	if resp["timezone"] != "America/Denver" {
		t.Errorf("timezone = %v, want America/Denver", resp["timezone"])
	}
	if resp["api_version"] != VersionISPSendingInsights {
		t.Errorf("api_version = %v, want %s", resp["api_version"], VersionISPSendingInsights)
	}
	// top_campaigns must be an empty array (not null) when no isp filter.
	tc, ok := resp["top_campaigns"].([]interface{})
	if !ok {
		t.Fatalf("top_campaigns not an array: %T %v", resp["top_campaigns"], resp["top_campaigns"])
	}
	if len(tc) != 0 {
		t.Errorf("top_campaigns should be empty without isp filter, got %d rows", len(tc))
	}
}

func TestHandleISPSendingInsights_ExplicitDays(t *testing.T) {
	for _, d := range []string{"3", "7", "14"} {
		t.Run("days="+d, func(t *testing.T) {
			svc, mock, _ := newInsightsTestService(t)
			expectBaseQueries(mock)

			req := httptest.NewRequest(http.MethodGet, "/x?days="+d, nil)
			rec := httptest.NewRecorder()
			svc.HandleISPSendingInsights(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			var resp map[string]interface{}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

			gotDays, _ := resp["days"].(float64)
			wantDays := parseISPInsightsDays(d)
			if int(gotDays) != wantDays {
				t.Errorf("days echo = %v, want %d", gotDays, wantDays)
			}
		})
	}
}

func TestHandleISPSendingInsights_InvalidDaysFallsBackTo7(t *testing.T) {
	for _, bad := range []string{"1", "10", "30", "abc", "-3"} {
		t.Run("days="+bad, func(t *testing.T) {
			svc, mock, _ := newInsightsTestService(t)
			expectBaseQueries(mock)

			req := httptest.NewRequest(http.MethodGet, "/x?days="+bad, nil)
			rec := httptest.NewRecorder()
			svc.HandleISPSendingInsights(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			var resp map[string]interface{}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			if v, _ := resp["days"].(float64); v != 7 {
				t.Errorf("invalid days=%q: echo = %v, want 7", bad, v)
			}
		})
	}
}

func TestHandleISPSendingInsights_WindowIsMSTAnchored(t *testing.T) {
	svc, mock, _ := newInsightsTestService(t)
	expectBaseQueries(mock)

	req := httptest.NewRequest(http.MethodGet, "/x?days=7", nil)
	rec := httptest.NewRecorder()
	svc.HandleISPSendingInsights(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	window, ok := resp["window"].(map[string]interface{})
	if !ok {
		t.Fatalf("window missing or wrong type: %T", resp["window"])
	}

	for _, key := range []string{"start", "end", "start_mst", "end_mst"} {
		if _, present := window[key]; !present {
			t.Errorf("window.%s missing", key)
		}
	}

	startMSTStr, _ := window["start_mst"].(string)
	// Must include MST or MDT zone suffix (depends on date) and 00:00:00 time.
	if !strings.Contains(startMSTStr, "00:00:00") {
		t.Errorf("start_mst should be at MST midnight; got %q", startMSTStr)
	}
	if !strings.Contains(startMSTStr, "MST") && !strings.Contains(startMSTStr, "MDT") {
		t.Errorf("start_mst should carry MST/MDT suffix; got %q", startMSTStr)
	}
}

func TestHandleISPSendingInsights_DailyQueryUsesMSTCast(t *testing.T) {
	// This test pins the SQL fragment that does the MST date-bucket cast.
	// If someone regresses the handler back to raw DATE(event_at), this
	// test catches it immediately.
	svc, mock, _ := newInsightsTestService(t)
	// Override default base queries so we can match the daily SQL precisely
	// and assert it carries the MST cast. Other queries are permissive.
	expectSetTimeouts(mock, 5)
	mock.ExpectQuery(`SELECT DISTINCT LOWER`).WillReturnRows(sqlmock.NewRows([]string{"sd"}))
	mock.ExpectQuery(regexp.QuoteMeta(`DATE(d.event_at AT TIME ZONE 'America/Denver')`)).
		WillReturnRows(sqlmock.NewRows([]string{
			"isp", "day", "sent", "delivered", "hard_bounces", "soft_bounces",
			"deferred", "complained", "opened", "mpp_opens",
		}))
	mock.ExpectQuery(`event_type = 'bounced'`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "category", "cnt"}))
	mock.ExpectQuery(`EXTRACT\(HOUR`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "hr", "cnt"}))
	mock.ExpectQuery(`FROM mailing_campaign_isp_plans`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "quota"}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	svc.HandleISPSendingInsights(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sql expectations (MST cast not found?): %v", err)
	}
}

func TestHandleISPSendingInsights_ISPFilterTriggersTopCampaigns(t *testing.T) {
	svc, mock, _ := newInsightsTestService(t)
	// ispFilter is present → 7 parallel queries: base 5 + by_sending_domain + top_campaigns.
	expectSetTimeouts(mock, 7)
	mock.ExpectQuery(`SELECT DISTINCT LOWER\(COALESCE\(NULLIF\(sending_domain,''\),'unknown'\)\).*FROM mailing_tracking_events`).
		WillReturnRows(sqlmock.NewRows([]string{"sd"}))
	mock.ExpectQuery(`GROUP BY isp, day`).
		WillReturnRows(sqlmock.NewRows([]string{
			"isp", "day", "sent", "delivered", "hard_bounces", "soft_bounces",
			"deferred", "complained", "opened", "mpp_opens",
		}))
	mock.ExpectQuery(`WHERE d\.event_type = 'bounced'\s*GROUP BY isp, category`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "category", "cnt"}))
	mock.ExpectQuery(`EXTRACT\(HOUR FROM d\.event_at\)`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "hr", "cnt"}))
	mock.ExpectQuery(`FROM mailing_campaign_isp_plans`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "quota"}))
	expectBySDQuery(mock, sqlmock.AnyArg(), sqlmock.AnyArg(), "gmail")

	// Extra top_campaigns query fires only when ?isp= is present. Return
	// two rows to verify the handler shapes them correctly.
	mock.ExpectQuery(`LIMIT 25`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "gmail").
		WillReturnRows(sqlmock.NewRows([]string{
			"cid", "name", "sending_domain",
			"sent", "delivered", "opens", "clicks",
			"hard_bounces", "soft_bounces",
		}).
			AddRow("c1", "DB Engager Morning 4/20", "em.discountblog.com",
				1000, 980, 400, 50, 5, 15).
			AddRow("c2", "QF Welcome 4/20", "em.quizfiesta.com",
				2000, 1940, 600, 30, 10, 50))

	req := httptest.NewRequest(http.MethodGet, "/x?isp=Gmail", nil)
	rec := httptest.NewRecorder()
	svc.HandleISPSendingInsights(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// isp_filter echoed and lowercased.
	if resp["isp_filter"] != "gmail" {
		t.Errorf("isp_filter = %v, want gmail (lowercased)", resp["isp_filter"])
	}

	tc, ok := resp["top_campaigns"].([]interface{})
	if !ok {
		t.Fatalf("top_campaigns missing or wrong type: %T", resp["top_campaigns"])
	}
	if len(tc) != 2 {
		t.Fatalf("top_campaigns len = %d, want 2", len(tc))
	}

	first, _ := tc[0].(map[string]interface{})
	wantFields := []string{
		"campaign_id", "name", "sending_domain",
		"sent", "delivered", "opens", "clicks",
		"hard_bounces", "soft_bounces",
		"open_rate", "click_rate", "hard_bounce_rate", "soft_bounce_rate",
	}
	for _, f := range wantFields {
		if _, present := first[f]; !present {
			t.Errorf("top_campaigns[0] missing field %q", f)
		}
	}

	// Derived rates should match ComputeInfraRates output.
	if first["name"] != "DB Engager Morning 4/20" {
		t.Errorf("top_campaigns[0].name = %v", first["name"])
	}
	if first["sending_domain"] != "em.discountblog.com" {
		t.Errorf("top_campaigns[0].sending_domain = %v", first["sending_domain"])
	}
	// click_rate = 50 / 980 = 5.1020...% → rounds to 5.1
	if cr, _ := first["click_rate"].(float64); cr < 5.0 || cr > 5.2 {
		t.Errorf("top_campaigns[0].click_rate = %v, want ~5.1", cr)
	}
}

func TestHandleISPSendingInsights_BySendingDomainPopulated(t *testing.T) {
	// When ispFilter is set, by_sending_domain should be populated with one
	// entry per sending_domain row returned, each containing daily buckets
	// and pre-computed totals. This replaces the prior frontend 1+N fan-out.
	svc, mock, _ := newInsightsTestService(t)
	expectSetTimeouts(mock, 7)
	mock.ExpectQuery(`SELECT DISTINCT LOWER\(COALESCE\(NULLIF\(sending_domain,''\),'unknown'\)\).*FROM mailing_tracking_events`).
		WillReturnRows(sqlmock.NewRows([]string{"sd"}))
	mock.ExpectQuery(`GROUP BY isp, day`).
		WillReturnRows(sqlmock.NewRows([]string{
			"isp", "day", "sent", "delivered", "hard_bounces", "soft_bounces",
			"deferred", "complained", "opened", "mpp_opens",
		}))
	mock.ExpectQuery(`WHERE d\.event_type = 'bounced'\s*GROUP BY isp, category`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "category", "cnt"}))
	mock.ExpectQuery(`EXTRACT\(HOUR FROM d\.event_at\)`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "hr", "cnt"}))
	mock.ExpectQuery(`FROM mailing_campaign_isp_plans`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "quota"}))

	// Two domains, two days each, with non-trivial opens/clicks so we can
	// verify the totals rollup.
	mock.ExpectQuery(`GROUP BY sd, day`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "gmail").
		WillReturnRows(sqlmock.NewRows([]string{
			"sd", "day", "sent", "delivered", "hard_bounces", "soft_bounces",
			"deferred", "opened", "clicked",
		}).
			AddRow("em.quizfiesta.com", time.Date(2026, 4, 19, 0, 0, 0, 0, time.UTC), 1000, 960, 10, 30, 5, 300, 40).
			AddRow("em.quizfiesta.com", time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC), 2000, 1920, 20, 60, 10, 600, 80).
			AddRow("em.discountblog.com", time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC), 500, 480, 5, 15, 2, 120, 15))

	mock.ExpectQuery(`LIMIT 25`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "gmail").
		WillReturnRows(sqlmock.NewRows([]string{
			"cid", "name", "sending_domain",
			"sent", "delivered", "opens", "clicks",
			"hard_bounces", "soft_bounces",
		}))

	req := httptest.NewRequest(http.MethodGet, "/x?isp=gmail&days=7", nil)
	rec := httptest.NewRecorder()
	svc.HandleISPSendingInsights(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	bySD, ok := resp["by_sending_domain"].(map[string]interface{})
	require.True(t, ok, "by_sending_domain missing or wrong type: %T", resp["by_sending_domain"])
	require.Len(t, bySD, 2, "expected 2 sending domains in by_sending_domain, got %d", len(bySD))

	qf, ok := bySD["em.quizfiesta.com"].(map[string]interface{})
	require.True(t, ok, "em.quizfiesta.com missing")

	qfDaily, ok := qf["daily"].([]interface{})
	require.True(t, ok)
	require.Len(t, qfDaily, 2, "QF should have 2 daily buckets")

	qfTotals, ok := qf["totals"].(map[string]interface{})
	require.True(t, ok)
	// Sent: 1000 + 2000 = 3000
	if v, _ := qfTotals["sent"].(float64); v != 3000 {
		t.Errorf("qf totals.sent = %v, want 3000", v)
	}
	// Clicks: 40 + 80 = 120
	if v, _ := qfTotals["clicks"].(float64); v != 120 {
		t.Errorf("qf totals.clicks = %v, want 120", v)
	}
	// Click rate: 120 / 2880 delivered ≈ 4.17%
	if v, _ := qfTotals["click_rate"].(float64); v < 4.0 || v > 4.3 {
		t.Errorf("qf totals.click_rate = %v, want ~4.17", v)
	}

	// Daily rows carry opens and clicks per day.
	first, _ := qfDaily[0].(map[string]interface{})
	for _, f := range []string{"date", "sent", "delivered", "hard_bounces", "soft_bounces", "deferred", "opens", "clicks"} {
		if _, present := first[f]; !present {
			t.Errorf("by_sending_domain daily[0] missing field %q", f)
		}
	}
}

func TestHandleISPSendingInsights_BySendingDomainAbsentWithoutISPFilter(t *testing.T) {
	// Without an ispFilter the panel is not scoped to a single ISP, so the
	// by_sending_domain rollup is skipped and should come back as an empty
	// object (never nil, never omitted).
	svc, mock, _ := newInsightsTestService(t)
	expectBaseQueries(mock)

	req := httptest.NewRequest(http.MethodGet, "/x?days=7", nil)
	rec := httptest.NewRecorder()
	svc.HandleISPSendingInsights(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	bySD, ok := resp["by_sending_domain"].(map[string]interface{})
	require.True(t, ok, "by_sending_domain missing or wrong type")
	require.Len(t, bySD, 0, "by_sending_domain should be empty without isp filter, got %d entries", len(bySD))
}

func TestHandleISPSendingInsights_NoISPFilter_NoTopCampaignsQuery(t *testing.T) {
	svc, mock, _ := newInsightsTestService(t)
	expectBaseQueries(mock)

	// Deliberately no expectation for the top_campaigns LIMIT 25 query.
	// If the handler fires it anyway, sqlmock fails the test.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	svc.HandleISPSendingInsights(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestHandleISPSendingInsights_SendingDomainFilterPropagates(t *testing.T) {
	svc, mock, _ := newInsightsTestService(t)

	// domain-only (no isp filter) → 5 parallel queries, each carrying the
	// sending_domain as $3 placeholder. The base list query has just 2 args.
	expectSetTimeouts(mock, 5)
	mock.ExpectQuery(`SELECT DISTINCT LOWER`).
		WillReturnRows(sqlmock.NewRows([]string{"sd"}))

	mock.ExpectQuery(`GROUP BY isp, day`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "em.discountblog.com").
		WillReturnRows(sqlmock.NewRows([]string{
			"isp", "day", "sent", "delivered", "hard_bounces", "soft_bounces",
			"deferred", "complained", "opened", "mpp_opens",
		}))

	mock.ExpectQuery(`event_type = 'bounced'\s*GROUP BY isp, category`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "em.discountblog.com").
		WillReturnRows(sqlmock.NewRows([]string{"isp", "category", "cnt"}))

	mock.ExpectQuery(`EXTRACT\(HOUR`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "em.discountblog.com").
		WillReturnRows(sqlmock.NewRows([]string{"isp", "hr", "cnt"}))

	mock.ExpectQuery(`FROM mailing_campaign_isp_plans`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "quota"}))

	req := httptest.NewRequest(http.MethodGet, "/x?sending_domain=em.discountblog.com", nil)
	rec := httptest.NewRecorder()
	svc.HandleISPSendingInsights(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	if resp["domain_filter"] != "em.discountblog.com" {
		t.Errorf("domain_filter = %v, want em.discountblog.com", resp["domain_filter"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestHandleISPSendingInsights_SendingDomainAndISPFilterCombined(t *testing.T) {
	svc, mock, _ := newInsightsTestService(t)

	// Both filters → 7 parallel queries; domain is $3, isp is $4 for
	// queries that take both (by_sending_domain + top_campaigns).
	expectSetTimeouts(mock, 7)
	mock.ExpectQuery(`SELECT DISTINCT LOWER`).
		WillReturnRows(sqlmock.NewRows([]string{"sd"}))
	mock.ExpectQuery(`GROUP BY isp, day`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "em.quizfiesta.com").
		WillReturnRows(sqlmock.NewRows([]string{
			"isp", "day", "sent", "delivered", "hard_bounces", "soft_bounces",
			"deferred", "complained", "opened", "mpp_opens",
		}))
	mock.ExpectQuery(`event_type = 'bounced'\s*GROUP BY`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "em.quizfiesta.com").
		WillReturnRows(sqlmock.NewRows([]string{"isp", "category", "cnt"}))
	mock.ExpectQuery(`EXTRACT\(HOUR`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "em.quizfiesta.com").
		WillReturnRows(sqlmock.NewRows([]string{"isp", "hr", "cnt"}))

	expectBySDQuery(mock, sqlmock.AnyArg(), sqlmock.AnyArg(), "em.quizfiesta.com", "gmail")

	// top_campaigns should be called with BOTH args: domain at $3, isp at $4.
	mock.ExpectQuery(`LIMIT 25`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "em.quizfiesta.com", "gmail").
		WillReturnRows(sqlmock.NewRows([]string{
			"cid", "name", "sending_domain",
			"sent", "delivered", "opens", "clicks",
			"hard_bounces", "soft_bounces",
		}))

	mock.ExpectQuery(`FROM mailing_campaign_isp_plans`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "quota"}))

	req := httptest.NewRequest(http.MethodGet,
		"/x?sending_domain=em.quizfiesta.com&isp=gmail&days=7", nil)
	rec := httptest.NewRecorder()
	svc.HandleISPSendingInsights(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}
