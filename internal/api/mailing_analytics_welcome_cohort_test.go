package api

// Tests for HandleWelcomeCohortAudit and its pure helpers.
//
// Like the ISP insights tests, this endpoint is pure reporting against
// mailing_subscriber_domain_state — no side effects, no external I/O.
// Everything is mocked via sqlmock. The goal is to verify:
//
//   1. parseCohortDays whitelist + defaulting
//   2. The handler issues the two expected queries (snapshot + daily).
//   3. Projection math uses the correct rates and only applies to
//      created_today_net_new (not created_today_mst).
//   4. Daily buckets are zero-filled for every MST calendar day in the window,
//      even when the DB returns no row for that day.
//   5. Totals roll up across domains and zero-filled day slots remain aligned.
//   6. Response shape includes api_version, as_of_mst, window_mst, and the
//      by_sending_domain / totals blocks.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// parseCohortDays
// ─────────────────────────────────────────────────────────────────────────────

func TestParseCohortDays(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 7},
		{"   ", 7},
		{"3", 3},
		{"7", 7},
		{"14", 14},
		{"30", 30},
		{"1", 7},
		{"5", 7},
		{"60", 7},
		{"abc", 7},
		{"7.5", 7},
	}
	for _, c := range cases {
		t.Run("in="+c.in, func(t *testing.T) {
			if got := parseCohortDays(c.in); got != c.want {
				t.Errorf("parseCohortDays(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// HandleWelcomeCohortAudit — response shape + rollup correctness
// ─────────────────────────────────────────────────────────────────────────────

// expectCohortSnapshotQuery registers the per-domain snapshot query with
// the provided rows. Columns match the handler's scan order.
func expectCohortSnapshotQuery(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(`FROM mailing_subscriber_domain_state\s+WHERE unsubscribed_at IS NULL`).
		WillReturnRows(rows)
}

// expectCohortDailyQuery registers the per-domain per-MST-day query.
func expectCohortDailyQuery(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(`DATE\(created_at AT TIME ZONE 'America/Denver'\)`).
		WillReturnRows(rows)
}

func TestHandleWelcomeCohortAudit_EmptyDB(t *testing.T) {
	svc, mock, _ := newInsightsTestService(t)

	expectCohortSnapshotQuery(mock, sqlmock.NewRows([]string{
		"sending_domain", "sds_total", "created_today", "created_today_net_new",
		"openers_30d", "clickers_30d", "openers_7d", "clickers_7d",
	}))
	expectCohortDailyQuery(mock, sqlmock.NewRows([]string{
		"sending_domain", "day_mst", "new_subs",
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/mailing/analytics/welcome-cohort-audit", nil)
	rec := httptest.NewRecorder()
	svc.HandleWelcomeCohortAudit(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	require.Equal(t, VersionWelcomeCohortAudit, resp["api_version"])

	win, _ := resp["window_mst"].(map[string]interface{})
	require.NotNil(t, win)
	require.Equal(t, float64(7), win["days"])

	by, _ := resp["by_sending_domain"].([]interface{})
	require.Equal(t, 0, len(by), "by_sending_domain should be empty when no rows")

	totals, _ := resp["totals"].(map[string]interface{})
	require.NotNil(t, totals)
	// Even with no domains, totals.created_by_mst_day must be a fully zero-filled
	// 7-element array so the frontend chart renders a flat baseline.
	days, _ := totals["created_by_mst_day"].([]interface{})
	require.Equal(t, 7, len(days))
	for i, d := range days {
		b := d.(map[string]interface{})
		require.Equal(t, float64(0), b["new"], "day %d should be zero", i)
	}
}

func TestHandleWelcomeCohortAudit_SingleDomainProjection(t *testing.T) {
	svc, mock, _ := newInsightsTestService(t)

	// 2,000 created today, of which 1,800 are net-new (total_sent = 1).
	// With the default rates (open=15%, click=2%), the projection should be
	// 270 new openers and 36 new clickers.
	expectCohortSnapshotQuery(mock, sqlmock.NewRows([]string{
		"sending_domain", "sds_total", "created_today", "created_today_net_new",
		"openers_30d", "clickers_30d", "openers_7d", "clickers_7d",
	}).AddRow("em.quizfiesta.com", 7152, 2000, 1800, 120, 18, 40, 5))

	expectCohortDailyQuery(mock, sqlmock.NewRows([]string{
		"sending_domain", "day_mst", "new_subs",
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/mailing/analytics/welcome-cohort-audit", nil)
	rec := httptest.NewRecorder()
	svc.HandleWelcomeCohortAudit(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	by, _ := resp["by_sending_domain"].([]interface{})
	require.Equal(t, 1, len(by))

	dom := by[0].(map[string]interface{})
	require.Equal(t, "em.quizfiesta.com", dom["sending_domain"])
	require.Equal(t, float64(7152), dom["sds_total"])
	require.Equal(t, float64(2000), dom["created_today_mst"])
	require.Equal(t, float64(1800), dom["created_today_net_new"])

	pool30 := dom["engager_pool_30d"].(map[string]interface{})
	require.Equal(t, float64(120), pool30["openers"])
	require.Equal(t, float64(18), pool30["clickers"])

	pool7 := dom["engager_pool_7d"].(map[string]interface{})
	require.Equal(t, float64(40), pool7["openers"])
	require.Equal(t, float64(5), pool7["clickers"])

	proj := dom["projected_additions_tomorrow"].(map[string]interface{})
	// Projections must be based on created_today_net_new (1800), NOT
	// created_today_mst (2000). This is the critical correctness check:
	// re-welcomes (total_sent > 1) do not add new engager audience.
	require.Equal(t, float64(270), proj["new_openers"])   // 1800 * 0.15
	require.Equal(t, float64(36), proj["new_clickers"])   // 1800 * 0.02
}

func TestHandleWelcomeCohortAudit_CustomRatesOverride(t *testing.T) {
	svc, mock, _ := newInsightsTestService(t)

	expectCohortSnapshotQuery(mock, sqlmock.NewRows([]string{
		"sending_domain", "sds_total", "created_today", "created_today_net_new",
		"openers_30d", "clickers_30d", "openers_7d", "clickers_7d",
	}).AddRow("em.discountblog.com", 9572, 1000, 1000, 0, 0, 0, 0))

	expectCohortDailyQuery(mock, sqlmock.NewRows([]string{
		"sending_domain", "day_mst", "new_subs",
	}))

	req := httptest.NewRequest(http.MethodGet, "/x?open_rate=25&click_rate=5", nil)
	rec := httptest.NewRecorder()
	svc.HandleWelcomeCohortAudit(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	rates := resp["assumed_rates_pct"].(map[string]interface{})
	require.Equal(t, float64(25), rates["open"])
	require.Equal(t, float64(5), rates["click"])

	by, _ := resp["by_sending_domain"].([]interface{})
	require.Equal(t, 1, len(by))
	proj := by[0].(map[string]interface{})["projected_additions_tomorrow"].(map[string]interface{})
	require.Equal(t, float64(250), proj["new_openers"])  // 1000 * 0.25
	require.Equal(t, float64(50), proj["new_clickers"])  // 1000 * 0.05
}

func TestHandleWelcomeCohortAudit_CustomRatesIgnoredWhenOutOfRange(t *testing.T) {
	svc, mock, _ := newInsightsTestService(t)

	expectCohortSnapshotQuery(mock, sqlmock.NewRows([]string{
		"sending_domain", "sds_total", "created_today", "created_today_net_new",
		"openers_30d", "clickers_30d", "openers_7d", "clickers_7d",
	}))
	expectCohortDailyQuery(mock, sqlmock.NewRows([]string{
		"sending_domain", "day_mst", "new_subs",
	}))

	// Negative / >100 / non-numeric rates must fall back to defaults.
	req := httptest.NewRequest(http.MethodGet, "/x?open_rate=-5&click_rate=200", nil)
	rec := httptest.NewRecorder()
	svc.HandleWelcomeCohortAudit(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	rates := resp["assumed_rates_pct"].(map[string]interface{})
	require.Equal(t, float64(15), rates["open"])
	require.Equal(t, float64(2), rates["click"])
}

func TestHandleWelcomeCohortAudit_TotalsRollup(t *testing.T) {
	svc, mock, _ := newInsightsTestService(t)

	expectCohortSnapshotQuery(mock, sqlmock.NewRows([]string{
		"sending_domain", "sds_total", "created_today", "created_today_net_new",
		"openers_30d", "clickers_30d", "openers_7d", "clickers_7d",
	}).
		AddRow("em.quizfiesta.com", 7000, 2000, 1800, 100, 20, 30, 5).
		AddRow("em.discountblog.com", 9000, 1500, 1400, 200, 30, 50, 8))

	expectCohortDailyQuery(mock, sqlmock.NewRows([]string{
		"sending_domain", "day_mst", "new_subs",
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	svc.HandleWelcomeCohortAudit(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	totals := resp["totals"].(map[string]interface{})
	require.Equal(t, float64(16000), totals["sds_total"])               // 7000 + 9000
	require.Equal(t, float64(3500), totals["created_today_mst"])        // 2000 + 1500
	require.Equal(t, float64(3200), totals["created_today_net_new"])    // 1800 + 1400

	pool30 := totals["engager_pool_30d"].(map[string]interface{})
	require.Equal(t, float64(300), pool30["openers"])   // 100 + 200
	require.Equal(t, float64(50), pool30["clickers"])   // 20 + 30

	proj := totals["projected_additions_tomorrow"].(map[string]interface{})
	// 1800 * 0.15 + 1400 * 0.15 = 270 + 210 = 480
	require.Equal(t, float64(480), proj["new_openers"])
	// 1800 * 0.02 + 1400 * 0.02 = 36 + 28 = 64
	require.Equal(t, float64(64), proj["new_clickers"])
}

func TestHandleWelcomeCohortAudit_DailyBucketsZeroFilled(t *testing.T) {
	svc, mock, _ := newInsightsTestService(t)

	expectCohortSnapshotQuery(mock, sqlmock.NewRows([]string{
		"sending_domain", "sds_total", "created_today", "created_today_net_new",
		"openers_30d", "clickers_30d", "openers_7d", "clickers_7d",
	}).AddRow("em.myownhealth.net", 500, 100, 100, 0, 0, 0, 0))

	// DB returns zero daily rows — the handler must still emit a 7-element
	// array of zero-filled days for this domain.
	expectCohortDailyQuery(mock, sqlmock.NewRows([]string{
		"sending_domain", "day_mst", "new_subs",
	}))

	req := httptest.NewRequest(http.MethodGet, "/x?days=7", nil)
	rec := httptest.NewRecorder()
	svc.HandleWelcomeCohortAudit(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	by := resp["by_sending_domain"].([]interface{})
	require.Equal(t, 1, len(by))
	days := by[0].(map[string]interface{})["created_by_mst_day"].([]interface{})
	require.Equal(t, 7, len(days), "must emit 7 day slots for days=7")
	for _, d := range days {
		b := d.(map[string]interface{})
		require.Equal(t, float64(0), b["new"])
		require.NotEmpty(t, b["date"])
	}
}

func TestHandleWelcomeCohortAudit_WindowDaysPropagates(t *testing.T) {
	for _, d := range []string{"3", "7", "14", "30"} {
		t.Run("days="+d, func(t *testing.T) {
			svc, mock, _ := newInsightsTestService(t)

			expectCohortSnapshotQuery(mock, sqlmock.NewRows([]string{
				"sending_domain", "sds_total", "created_today", "created_today_net_new",
				"openers_30d", "clickers_30d", "openers_7d", "clickers_7d",
			}))
			expectCohortDailyQuery(mock, sqlmock.NewRows([]string{
				"sending_domain", "day_mst", "new_subs",
			}))

			req := httptest.NewRequest(http.MethodGet, "/x?days="+d, nil)
			rec := httptest.NewRecorder()
			svc.HandleWelcomeCohortAudit(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			var resp map[string]interface{}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

			win := resp["window_mst"].(map[string]interface{})
			want := parseCohortDays(d)
			require.Equal(t, float64(want), win["days"])

			totals := resp["totals"].(map[string]interface{})
			daySlots := totals["created_by_mst_day"].([]interface{})
			require.Equal(t, want, len(daySlots))
		})
	}
}
