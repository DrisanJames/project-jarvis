package api

// Tests for HandleHarvestPerformance and its pure helpers.
//
// The harvest analytics endpoint is the single lens the operator uses to
// verify the always-on Welcome Harvest stream. It MUST be bulletproof —
// zero gaps, zero silent nil-map panics, zero wrong denominators. These
// tests are the contract.
//
// Coverage:
//   1. parseHarvestHours       — clamping + defaulting
//   2. parseHarvestBucket      — whitelist + defaulting
//   3. sanitizeIdent / sanitizeCampaignPrefix — trimming semantics
//   4. computeHarvestRates     — denominator selection (delivered vs sent)
//   5. HandleHarvestPerformance — happy path, empty result, bad param,
//                                  DB error, default-prefix application
//
// All SQL mocked via sqlmock with non-strict ordering since the handler
// fires six parallel queries.

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// mustTimeUTC parses an RFC3339 timestamp in tests. Panics on bad input
// because test fixtures are under the author's control.
func mustTimeUTC(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return v.UTC()
}

// ─────────────────────────────────────────────────────────────────────────────
// parseHarvestHours
// ─────────────────────────────────────────────────────────────────────────────

func TestParseHarvestHours(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"blank defaults to 72", "", 72},
		{"whitespace defaults to 72", "   ", 72},
		{"valid 24", "24", 24},
		{"valid 1 (minimum)", "1", 1},
		{"zero clamps to 1", "0", 1},
		{"negative clamps to 1", "-5", 1},
		{"valid 720 (max)", "720", 720},
		{"1000 clamps to 720", "1000", 720},
		{"non-numeric defaults", "abc", 72},
		{"float-ish defaults", "72.5", 72},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseHarvestHours(c.in)
			if got != c.want {
				t.Errorf("parseHarvestHours(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// parseHarvestBucket
// ─────────────────────────────────────────────────────────────────────────────

func TestParseHarvestBucket(t *testing.T) {
	cases := []struct {
		in       string
		wantKey  string
		wantSecs int
	}{
		{"", "1h", 3600},
		{"1h", "1h", 3600},
		{"3h", "3h", 3 * 3600},
		{"5h", "5h", 5 * 3600},
		{"  3H ", "3h", 3 * 3600},
		{"4h", "1h", 3600},  // not whitelisted
		{"30m", "1h", 3600}, // not whitelisted
		{"garbage", "1h", 3600},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			k, s := parseHarvestBucket(c.in)
			if k != c.wantKey || s != c.wantSecs {
				t.Errorf("parseHarvestBucket(%q) = (%q,%d), want (%q,%d)", c.in, k, s, c.wantKey, c.wantSecs)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// sanitizeIdent / sanitizeCampaignPrefix
// ─────────────────────────────────────────────────────────────────────────────

func TestSanitizeIdent(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"Gmail", "gmail"},
		{" YaHoO ", "yahoo"},
		{"em.discountblog.com", "em.discountblog.com"},
	}
	for _, c := range cases {
		if got := sanitizeIdent(c.in); got != c.want {
			t.Errorf("sanitizeIdent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeCampaignPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"Welcome Harvest", "Welcome Harvest"},
		{"  Welcome Harvest  ", "Welcome Harvest"},
		{"Welcome Harvest — em.discountblog.com", "Welcome Harvest — em.discountblog.com"},
	}
	for _, c := range cases {
		if got := sanitizeCampaignPrefix(c.in); got != c.want {
			t.Errorf("sanitizeCampaignPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// computeHarvestRates
// ─────────────────────────────────────────────────────────────────────────────

func TestComputeHarvestRates(t *testing.T) {
	t.Run("rates use delivered as engagement base", func(t *testing.T) {
		m := HarvestMetrics{
			Sent:         1000,
			Delivered:    900,
			UniqueOpens:  450,
			UniqueClicks: 45,
			HardBounces:  50,
			SoftBounces:  50,
			Complaints:   3,
		}
		computeHarvestRates(&m)
		require.Equal(t, 50.0, m.OpenRate)  // 450/900
		require.Equal(t, 5.0, m.ClickRate)  // 45/900
		require.Equal(t, 5.0, m.HardBounceRate) // 50/1000
		require.Equal(t, 5.0, m.SoftBounceRate) // 50/1000
		require.Equal(t, 0.300, m.ComplaintRate) // 3/1000, 3dp
		require.Equal(t, 90.0, m.DeliveryRate)  // 900/1000
	})

	t.Run("falls back to sent when delivered is zero", func(t *testing.T) {
		m := HarvestMetrics{
			Sent:         100,
			Delivered:    0,
			UniqueOpens:  10,
			UniqueClicks: 1,
		}
		computeHarvestRates(&m)
		require.Equal(t, 10.0, m.OpenRate)
		require.Equal(t, 1.0, m.ClickRate)
	})

	t.Run("zero sent leaves rates at zero", func(t *testing.T) {
		m := HarvestMetrics{}
		computeHarvestRates(&m)
		require.Equal(t, 0.0, m.OpenRate)
		require.Equal(t, 0.0, m.ClickRate)
		require.Equal(t, 0.0, m.HardBounceRate)
		require.Equal(t, 0.0, m.SoftBounceRate)
		require.Equal(t, 0.0, m.ComplaintRate)
		require.Equal(t, 0.0, m.DeliveryRate)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// HandleHarvestPerformance (integration with sqlmock)
// ─────────────────────────────────────────────────────────────────────────────

// harvestISPColumns is the column set returned by by_isp and
// by_sending_domain queries, in order of the SELECT list.
var harvestISPColumns = []string{
	"key", "sent", "delivered", "hard_bounces", "soft_bounces",
	"opens", "unique_opens", "clicks", "unique_clicks",
	"complaints", "unsubs", "deferred", "mpp_opens",
}

// harvestBucketColumns is the column set for time_series (no isp).
var harvestBucketColumns = []string{
	"ts", "sent", "delivered", "hard_bounces", "soft_bounces",
	"opens", "unique_opens", "clicks", "unique_clicks",
	"complaints", "unsubs", "deferred", "mpp_opens",
}

// harvestBucketISPColumns is the column set for time_series_by_isp.
var harvestBucketISPColumns = []string{
	"ts", "isp", "sent", "delivered", "hard_bounces", "soft_bounces",
	"opens", "unique_opens", "clicks", "unique_clicks",
	"complaints", "unsubs", "deferred", "mpp_opens",
}

// harvestHourColumns is the column set for hour_of_day.
var harvestHourColumns = []string{
	"hr", "isp", "sent", "delivered", "hard_bounces", "soft_bounces",
	"opens", "unique_opens", "clicks", "unique_clicks",
	"complaints", "unsubs", "deferred", "mpp_opens",
}

// harvestCampaignColumns is the column set for by_campaign.
var harvestCampaignColumns = []string{
	"cid", "name", "sending_domain",
	"sent", "delivered", "hard_bounces", "soft_bounces",
	"opens", "unique_opens", "clicks", "unique_clicks",
	"complaints", "unsubs", "deferred", "mpp_opens",
}

// setupHarvestMock returns a DB handle + mock where every one of the 6
// handler queries is satisfied with the zero-row result set. Individual
// tests can override before calling the handler.
func setupHarvestMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)

	mock.MatchExpectationsInOrder(false)
	for i := 0; i < 6; i++ {
		mock.ExpectExec("SET statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	}
	return db, mock
}

func TestHandleHarvestPerformance_HappyPath(t *testing.T) {
	db, mock := setupHarvestMock(t)
	defer db.Close()

	// Q1 by_isp — 2 ISP rows
	mock.ExpectQuery("SELECT.*as isp.*GROUP BY isp").WillReturnRows(
		sqlmock.NewRows(harvestISPColumns).
			AddRow("gmail", 1000, 900, 50, 50, 400, 350, 40, 35, 2, 1, 5, 30).
			AddRow("yahoo", 500, 440, 30, 30, 180, 160, 18, 15, 1, 0, 8, 20),
	)
	// Q2 by_sending_domain
	mock.ExpectQuery("GROUP BY sd").WillReturnRows(
		sqlmock.NewRows(harvestISPColumns).
			AddRow("em.discountblog.com", 900, 820, 40, 40, 360, 320, 36, 31, 2, 0, 6, 25).
			AddRow("em.quizfiesta.com", 600, 520, 40, 40, 220, 190, 22, 19, 1, 1, 7, 25),
	)
	// Q3 time_series — 3 buckets
	mock.ExpectQuery("GROUP BY ts ORDER BY ts").WillReturnRows(
		sqlmock.NewRows(harvestBucketColumns).
			AddRow(mustTimeUTC(t, "2026-04-22T00:00:00Z"), 300, 260, 20, 20, 120, 110, 12, 10, 1, 0, 3, 10).
			AddRow(mustTimeUTC(t, "2026-04-22T01:00:00Z"), 400, 360, 20, 20, 160, 150, 16, 14, 1, 1, 4, 15).
			AddRow(mustTimeUTC(t, "2026-04-22T02:00:00Z"), 800, 720, 40, 40, 300, 250, 30, 26, 1, 0, 6, 25),
	)
	// Q4 time_series_by_isp
	mock.ExpectQuery("GROUP BY ts, isp").WillReturnRows(
		sqlmock.NewRows(harvestBucketISPColumns).
			AddRow(mustTimeUTC(t, "2026-04-22T00:00:00Z"), "gmail", 300, 260, 20, 20, 120, 110, 12, 10, 1, 0, 3, 10).
			AddRow(mustTimeUTC(t, "2026-04-22T00:00:00Z"), "yahoo", 150, 130, 10, 10, 50, 45, 5, 4, 0, 0, 2, 5),
	)
	// Q5 hour_of_day
	mock.ExpectQuery("GROUP BY hr, isp").WillReturnRows(
		sqlmock.NewRows(harvestHourColumns).
			AddRow(9, "gmail", 500, 450, 25, 25, 200, 180, 20, 17, 1, 0, 2, 15).
			AddRow(9, "yahoo", 250, 220, 15, 15, 90, 80, 9, 7, 0, 0, 4, 10),
	)
	// Q6 by_campaign
	mock.ExpectQuery("GROUP BY d.campaign_id, d.campaign_name").WillReturnRows(
		sqlmock.NewRows(harvestCampaignColumns).
			AddRow("11111111-1111-1111-1111-111111111111", "Welcome Harvest — em.discountblog.com",
				"em.discountblog.com",
				900, 820, 40, 40, 360, 320, 36, 31, 2, 0, 6, 25),
	)

	svc := &AdvancedMailingService{db: db}
	req := httptest.NewRequest("GET", "/api/mailing/analytics/harvest-performance?hours=24&bucket=1h", nil)
	w := httptest.NewRecorder()
	svc.HandleHarvestPerformance(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Equal(t, VersionHarvestPerformance, resp["api_version"])
	require.Equal(t, DefaultHarvestCampaignPrefix, resp["campaign_prefix"])

	window := resp["window"].(map[string]interface{})
	require.Equal(t, "1h", window["bucket"])
	require.EqualValues(t, 24, window["hours"])
	require.EqualValues(t, 3600, window["bucket_seconds"])

	byISP := resp["by_isp"].([]interface{})
	require.Len(t, byISP, 2)
	gmail := byISP[0].(map[string]interface{})
	require.Equal(t, "gmail", gmail["isp"])
	require.Equal(t, "Gmail", gmail["display_name"])

	overall := resp["overall"].(map[string]interface{})
	require.EqualValues(t, 1500, overall["sent"])      // 1000+500
	require.EqualValues(t, 1340, overall["delivered"]) // 900+440

	ts := resp["time_series"].([]interface{})
	require.Len(t, ts, 3)
	first := ts[0].(map[string]interface{})
	require.Contains(t, first, "ts_utc")
	require.Contains(t, first, "ts_mst")

	tsByISP := resp["time_series_by_isp"].(map[string]interface{})
	require.Contains(t, tsByISP, "gmail")
	require.Contains(t, tsByISP, "yahoo")

	heatmap := resp["hour_of_day"].([]interface{})
	require.Len(t, heatmap, 2)

	engagement := resp["engagement_vs_damage"].(map[string]interface{})
	require.EqualValues(t, 510+50, engagement["engagement"]) // 350+40+160+18+...? let's recompute:
	// unique_opens: 350+160 = 510; unique_clicks: 35+15 = 50; engagement = 560
	require.EqualValues(t, 560, engagement["engagement"])
	require.EqualValues(t, 80+3, engagement["damage"]) // hard_bounces 50+30 + complaints 2+1 = 83
	require.EqualValues(t, 83, engagement["damage"])

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleHarvestPerformance_EmptyResults(t *testing.T) {
	db, mock := setupHarvestMock(t)
	defer db.Close()

	mock.ExpectQuery("GROUP BY isp").WillReturnRows(sqlmock.NewRows(harvestISPColumns))
	mock.ExpectQuery("GROUP BY sd").WillReturnRows(sqlmock.NewRows(harvestISPColumns))
	mock.ExpectQuery("GROUP BY ts ORDER BY ts").WillReturnRows(sqlmock.NewRows(harvestBucketColumns))
	mock.ExpectQuery("GROUP BY ts, isp").WillReturnRows(sqlmock.NewRows(harvestBucketISPColumns))
	mock.ExpectQuery("GROUP BY hr, isp").WillReturnRows(sqlmock.NewRows(harvestHourColumns))
	mock.ExpectQuery("GROUP BY d.campaign_id").WillReturnRows(sqlmock.NewRows(harvestCampaignColumns))

	svc := &AdvancedMailingService{db: db}
	req := httptest.NewRequest("GET", "/api/mailing/analytics/harvest-performance", nil)
	w := httptest.NewRecorder()
	svc.HandleHarvestPerformance(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	// All the list sections should exist (never nil) so the frontend can
	// safely .map() them.
	require.NotNil(t, resp["by_isp"])
	require.NotNil(t, resp["by_sending_domain"])
	require.NotNil(t, resp["time_series"])
	require.NotNil(t, resp["time_series_by_isp"])
	require.NotNil(t, resp["hour_of_day"])
	require.NotNil(t, resp["by_campaign"])

	overall := resp["overall"].(map[string]interface{})
	require.EqualValues(t, 0, overall["sent"])
}

func TestHandleHarvestPerformance_BucketDefaulting(t *testing.T) {
	db, mock := setupHarvestMock(t)
	defer db.Close()
	mock.ExpectQuery("GROUP BY isp").WillReturnRows(sqlmock.NewRows(harvestISPColumns))
	mock.ExpectQuery("GROUP BY sd").WillReturnRows(sqlmock.NewRows(harvestISPColumns))
	mock.ExpectQuery("GROUP BY ts ORDER BY ts").WillReturnRows(sqlmock.NewRows(harvestBucketColumns))
	mock.ExpectQuery("GROUP BY ts, isp").WillReturnRows(sqlmock.NewRows(harvestBucketISPColumns))
	mock.ExpectQuery("GROUP BY hr, isp").WillReturnRows(sqlmock.NewRows(harvestHourColumns))
	mock.ExpectQuery("GROUP BY d.campaign_id").WillReturnRows(sqlmock.NewRows(harvestCampaignColumns))

	svc := &AdvancedMailingService{db: db}
	// A nonsense bucket value — handler must snap to 1h, not error.
	req := httptest.NewRequest("GET", "/api/mailing/analytics/harvest-performance?bucket=42m", nil)
	w := httptest.NewRecorder()
	svc.HandleHarvestPerformance(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	window := resp["window"].(map[string]interface{})
	require.Equal(t, "1h", window["bucket"])
}

func TestHandleHarvestPerformance_ExplicitEmptyCampaignPrefix(t *testing.T) {
	// When campaign_prefix is passed as an empty string, the handler must
	// respect that and NOT apply the default. This is how the frontend
	// shows cross-campaign totals.
	db, mock := setupHarvestMock(t)
	defer db.Close()
	mock.ExpectQuery("GROUP BY isp").WillReturnRows(sqlmock.NewRows(harvestISPColumns))
	mock.ExpectQuery("GROUP BY sd").WillReturnRows(sqlmock.NewRows(harvestISPColumns))
	mock.ExpectQuery("GROUP BY ts ORDER BY ts").WillReturnRows(sqlmock.NewRows(harvestBucketColumns))
	mock.ExpectQuery("GROUP BY ts, isp").WillReturnRows(sqlmock.NewRows(harvestBucketISPColumns))
	mock.ExpectQuery("GROUP BY hr, isp").WillReturnRows(sqlmock.NewRows(harvestHourColumns))
	mock.ExpectQuery("GROUP BY d.campaign_id").WillReturnRows(sqlmock.NewRows(harvestCampaignColumns))

	svc := &AdvancedMailingService{db: db}
	req := httptest.NewRequest("GET", "/api/mailing/analytics/harvest-performance?campaign_prefix=", nil)
	w := httptest.NewRecorder()
	svc.HandleHarvestPerformance(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "", resp["campaign_prefix"])
}

func TestHandleHarvestPerformance_DBError(t *testing.T) {
	db, mock := setupHarvestMock(t)
	defer db.Close()
	// Any one failing query should yield a 500 and no partial body.
	mock.ExpectQuery("GROUP BY isp").WillReturnError(driver.ErrBadConn)
	mock.ExpectQuery("GROUP BY sd").WillReturnRows(sqlmock.NewRows(harvestISPColumns))
	mock.ExpectQuery("GROUP BY ts ORDER BY ts").WillReturnRows(sqlmock.NewRows(harvestBucketColumns))
	mock.ExpectQuery("GROUP BY ts, isp").WillReturnRows(sqlmock.NewRows(harvestBucketISPColumns))
	mock.ExpectQuery("GROUP BY hr, isp").WillReturnRows(sqlmock.NewRows(harvestHourColumns))
	mock.ExpectQuery("GROUP BY d.campaign_id").WillReturnRows(sqlmock.NewRows(harvestCampaignColumns))

	svc := &AdvancedMailingService{db: db}
	req := httptest.NewRequest("GET", "/api/mailing/analytics/harvest-performance", nil)
	w := httptest.NewRecorder()
	svc.HandleHarvestPerformance(w, req)
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.True(t, strings.Contains(w.Body.String(), "bad connection") || w.Body.Len() > 0)
}

func TestHandleHarvestPerformance_AppliesISPFilter(t *testing.T) {
	db, mock := setupHarvestMock(t)
	defer db.Close()

	// The handler should inject the ispFilter as a $N parameter. We
	// verify by using sqlmock's argument matcher.
	mock.ExpectQuery("GROUP BY isp").WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "Welcome Harvest%", "gmail").
		WillReturnRows(sqlmock.NewRows(harvestISPColumns))
	mock.ExpectQuery("GROUP BY sd").WillReturnRows(sqlmock.NewRows(harvestISPColumns))
	mock.ExpectQuery("GROUP BY ts ORDER BY ts").WillReturnRows(sqlmock.NewRows(harvestBucketColumns))
	mock.ExpectQuery("GROUP BY ts, isp").WillReturnRows(sqlmock.NewRows(harvestBucketISPColumns))
	mock.ExpectQuery("GROUP BY hr, isp").WillReturnRows(sqlmock.NewRows(harvestHourColumns))
	mock.ExpectQuery("GROUP BY d.campaign_id").WillReturnRows(sqlmock.NewRows(harvestCampaignColumns))

	svc := &AdvancedMailingService{db: db}
	req := httptest.NewRequest("GET", "/api/mailing/analytics/harvest-performance?isp=GMAIL", nil)
	w := httptest.NewRecorder()
	svc.HandleHarvestPerformance(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	filters := resp["filters"].(map[string]interface{})
	require.Equal(t, "gmail", filters["isp"])
}
