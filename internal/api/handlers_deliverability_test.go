package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newDeliverabilityTestHandler wires a deliverabilityHandler with a mocked
// DB. The other dependencies (rateRegistry, agentFactory, ispConfigs) are
// nil because none of the read endpoints under test touch them.
func newDeliverabilityTestHandler(t *testing.T) (*deliverabilityHandler, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return &deliverabilityHandler{db: db}, mock
}

// ─── /timeseries ─────────────────────────────────────────────────────────────

func TestHandleGetTimeSeries_HappyPath(t *testing.T) {
	h, mock := newDeliverabilityTestHandler(t)

	t1 := time.Date(2026, 4, 27, 19, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)

	mock.ExpectQuery(`FROM pmta_acct_raw\s+WHERE received_at`).
		WithArgs("1 hour").
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "series_key", "value"}).
			AddRow(t1, "gmail", 100).
			AddRow(t1, "yahoo", 50).
			AddRow(t2, "gmail", 120).
			AddRow(t2, "yahoo", 55))

	r := chi.NewRouter()
	r.Get("/timeseries", h.HandleGetTimeSeries)
	req := httptest.NewRequest("GET", "/timeseries?metric=delivered&groupBy=isp&window=1h&bucket=1m", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Metric  string `json:"metric"`
		GroupBy string `json:"group_by"`
		Window  string `json:"window"`
		Bucket  string `json:"bucket"`
		Keys    []string `json:"keys"`
		Buckets []struct {
			Bucket string         `json:"bucket"`
			Series map[string]int `json:"series"`
		} `json:"buckets"`
		Total      int    `json:"total"`
		APIVersion string `json:"api_version"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "delivered", resp.Metric)
	assert.Equal(t, "isp", resp.GroupBy)
	assert.Equal(t, "1 hour", resp.Window)
	assert.Equal(t, "1m", resp.Bucket)
	assert.ElementsMatch(t, []string{"gmail", "yahoo"}, resp.Keys)
	require.Len(t, resp.Buckets, 2)
	assert.Equal(t, 100, resp.Buckets[0].Series["gmail"])
	assert.Equal(t, 50, resp.Buckets[0].Series["yahoo"])
	assert.Equal(t, 325, resp.Total)
	assert.Equal(t, "2.0", resp.APIVersion)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleGetTimeSeries_Empty(t *testing.T) {
	h, mock := newDeliverabilityTestHandler(t)
	mock.ExpectQuery(`FROM pmta_acct_raw`).
		WithArgs("1 hour").
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "series_key", "value"}))

	req := httptest.NewRequest("GET", "/timeseries?metric=delivered&window=1h", nil)
	rec := httptest.NewRecorder()
	h.HandleGetTimeSeries(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Total   int           `json:"total"`
		Buckets []interface{} `json:"buckets"`
		Keys    []string      `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 0, resp.Total)
	assert.Empty(t, resp.Buckets)
	assert.Empty(t, resp.Keys)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleGetTimeSeries_DBError(t *testing.T) {
	h, mock := newDeliverabilityTestHandler(t)
	mock.ExpectQuery(`FROM pmta_acct_raw`).
		WithArgs("1 hour").
		WillReturnError(assertableError("boom"))

	req := httptest.NewRequest("GET", "/timeseries?metric=delivered&window=1h", nil)
	rec := httptest.NewRecorder()
	h.HandleGetTimeSeries(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleGetTimeSeries_InvalidMetric(t *testing.T) {
	h, _ := newDeliverabilityTestHandler(t)
	req := httptest.NewRequest("GET", "/timeseries?metric=clicks&window=1h", nil)
	rec := httptest.NewRecorder()
	h.HandleGetTimeSeries(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleGetTimeSeries_InvalidGroupBy(t *testing.T) {
	h, _ := newDeliverabilityTestHandler(t)
	req := httptest.NewRequest("GET", "/timeseries?metric=delivered&groupBy=campaign&window=1h", nil)
	rec := httptest.NewRecorder()
	h.HandleGetTimeSeries(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleGetTimeSeries_GroupBySendingDomain(t *testing.T) {
	h, mock := newDeliverabilityTestHandler(t)
	t1 := time.Date(2026, 4, 27, 19, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`split_part`).
		WithArgs("6 hours").
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "series_key", "value"}).
			AddRow(t1, "em.quizfiesta.com", 800).
			AddRow(t1, "em.discountblog.com", 450))

	req := httptest.NewRequest("GET", "/timeseries?metric=hard_bounce&groupBy=sending_domain&window=6h", nil)
	rec := httptest.NewRecorder()
	h.HandleGetTimeSeries(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		GroupBy string `json:"group_by"`
		Window  string `json:"window"`
		Bucket  string `json:"bucket"`
		Total   int    `json:"total"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "sending_domain", resp.GroupBy)
	assert.Equal(t, "6 hours", resp.Window)
	// 6h default bucket is 5m
	assert.Equal(t, "5m", resp.Bucket)
	assert.Equal(t, 1250, resp.Total)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ─── /matrix ─────────────────────────────────────────────────────────────────

func TestHandleGetMatrix_HappyPath(t *testing.T) {
	h, mock := newDeliverabilityTestHandler(t)

	cols := []string{"isp", "sending_domain", "attempted", "delivered",
		"hard_bounce", "soft_bounce", "deferred", "complaint", "reputation_blocked"}

	mock.ExpectQuery(`FROM pmta_acct_raw\s+WHERE received_at`).
		WithArgs("1 hour").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("gmail", "em.quizfiesta.com", 1000, 950, 5, 10, 30, 0, 5).
			AddRow("yahoo", "em.discountblog.com", 500, 480, 8, 5, 7, 0, 0))

	req := httptest.NewRequest("GET", "/matrix?window=1h", nil)
	rec := httptest.NewRecorder()
	h.HandleGetMatrix(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Window string       `json:"window"`
		Cells  []matrixCell `json:"cells"`
		Totals matrixCell   `json:"totals"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Cells, 2)
	assert.Equal(t, "gmail", resp.Cells[0].ISP)
	assert.InDelta(t, 95.0, resp.Cells[0].AcceptPct, 0.01) // 950/1000
	assert.Equal(t, 1500, resp.Totals.Attempted)
	assert.Equal(t, 1430, resp.Totals.Delivered)
	assert.InDelta(t, 95.33, resp.Totals.AcceptPct, 0.01)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleGetMatrix_Empty(t *testing.T) {
	h, mock := newDeliverabilityTestHandler(t)
	cols := []string{"isp", "sending_domain", "attempted", "delivered",
		"hard_bounce", "soft_bounce", "deferred", "complaint", "reputation_blocked"}
	mock.ExpectQuery(`FROM pmta_acct_raw`).
		WithArgs("24 hours").
		WillReturnRows(sqlmock.NewRows(cols))

	req := httptest.NewRequest("GET", "/matrix?window=24h", nil)
	rec := httptest.NewRecorder()
	h.HandleGetMatrix(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Cells  []matrixCell `json:"cells"`
		Totals matrixCell   `json:"totals"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.Cells)
	assert.Equal(t, 0, resp.Totals.Attempted)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleGetMatrix_DBError(t *testing.T) {
	h, mock := newDeliverabilityTestHandler(t)
	mock.ExpectQuery(`FROM pmta_acct_raw`).
		WithArgs("1 hour").
		WillReturnError(assertableError("db dead"))

	req := httptest.NewRequest("GET", "/matrix?window=1h", nil)
	rec := httptest.NewRecorder()
	h.HandleGetMatrix(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// ─── /deferrals, /bounces, /fbl drilldowns ───────────────────────────────────

func TestHandleGetDeferrals_HappyPath(t *testing.T) {
	h, mock := newDeliverabilityTestHandler(t)
	cols := []string{"isp", "sending_domain", "group_key", "dsn_sample", "count"}

	mock.ExpectQuery(`record_type IN \('t','tq'\)`).
		WithArgs("1 hour", 20).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("charter", "em.discountblog.com", "4.7.1", "rate-limited", 250).
			AddRow("apple", "em.discountblog.com", "4.2.1", "throttled", 80))

	req := httptest.NewRequest("GET", "/deferrals?window=1h", nil)
	rec := httptest.NewRecorder()
	h.HandleGetDeferrals(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Window string        `json:"window"`
		Limit  int           `json:"limit"`
		Rows   []deferralRow `json:"rows"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "1 hour", resp.Window)
	assert.Equal(t, 20, resp.Limit)
	require.Len(t, resp.Rows, 2)
	assert.Equal(t, "charter", resp.Rows[0].ISP)
	assert.Equal(t, 250, resp.Rows[0].Count)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleGetDeferrals_WithFilters(t *testing.T) {
	h, mock := newDeliverabilityTestHandler(t)
	cols := []string{"isp", "sending_domain", "group_key", "dsn_sample", "count"}

	// 4 args: window, isp, sending_domain, limit
	mock.ExpectQuery(`record_type IN \('t','tq'\)`).
		WithArgs("24 hours", "charter", "em.discountblog.com", 5).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("charter", "em.discountblog.com", "4.7.1", "rate-limited", 100))

	req := httptest.NewRequest("GET",
		"/deferrals?window=24h&isp=charter&sending_domain=em.discountblog.com&limit=5", nil)
	rec := httptest.NewRecorder()
	h.HandleGetDeferrals(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Limit int           `json:"limit"`
		Rows  []deferralRow `json:"rows"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 5, resp.Limit)
	require.Len(t, resp.Rows, 1)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleGetBounces_HappyPath(t *testing.T) {
	h, mock := newDeliverabilityTestHandler(t)
	cols := []string{"isp", "sending_domain", "group_key", "dsn_sample", "count"}

	mock.ExpectQuery(`record_type = 'b'`).
		WithArgs("24 hours", 20).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("other", "em.discountblog.com", "bad-mailbox", "550 5.1.1 user unknown", 600))

	req := httptest.NewRequest("GET", "/bounces?window=24h", nil)
	rec := httptest.NewRecorder()
	h.HandleGetBounces(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Rows []deferralRow `json:"rows"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Rows, 1)
	assert.Equal(t, "bad-mailbox", resp.Rows[0].DSNStatus)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleGetFBL_HappyPath(t *testing.T) {
	h, mock := newDeliverabilityTestHandler(t)
	cols := []string{"isp", "sending_domain", "group_key", "dsn_sample", "count"}

	// FBL default window = 7d
	mock.ExpectQuery(`record_type = 'f'`).
		WithArgs("7 days", 20).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("aol", "em.discountblog.com", "abuse", "feedback-loop", 3))

	req := httptest.NewRequest("GET", "/fbl", nil)
	rec := httptest.NewRecorder()
	h.HandleGetFBL(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Window string `json:"window"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "7 days", resp.Window)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleGetDeferrals_DBError(t *testing.T) {
	h, mock := newDeliverabilityTestHandler(t)
	mock.ExpectQuery(`record_type IN \('t','tq'\)`).
		WillReturnError(assertableError("oops"))

	req := httptest.NewRequest("GET", "/deferrals?window=1h", nil)
	rec := httptest.NewRecorder()
	h.HandleGetDeferrals(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// ─── /ip-activity ────────────────────────────────────────────────────────────

func TestHandleGetIPActivity_HappyPath(t *testing.T) {
	h, mock := newDeliverabilityTestHandler(t)
	cols := []string{"ip", "hostname", "pool", "status", "warmup_day",
		"sent", "delivered", "hard_bounce", "soft_bounce", "deferred", "complaint", "last_seen_at"}

	now := time.Now().UTC()
	mock.ExpectQuery(`FROM mailing_ip_addresses`).
		WithArgs("24 hours").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("15.204.22.177", "mta2.mail.projectjarvis.io", "warmup-pool", "warmup", 5,
				1000, 980, 5, 5, 8, 2, now).
			AddRow("15.204.22.176", "mta1.mail.projectjarvis.io", "warmup-pool", "paused", 0,
				0, 0, 0, 0, 0, 0, nil).
			AddRow("15.204.22.190", "mta14.mail.projectjarvis.io", "default-pool", "warmup", 1,
				0, 0, 0, 0, 0, 0, nil))

	req := httptest.NewRequest("GET", "/ip-activity?window=24h", nil)
	rec := httptest.NewRecorder()
	h.HandleGetIPActivity(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp ipActivityResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "24 hours", resp.Window)
	require.Len(t, resp.Active, 1, "active = sent>0")
	require.Len(t, resp.Paused, 1, "paused IP routed to paused list")
	require.Len(t, resp.Cold, 1, "warmup with zero traffic = cold")
	assert.Equal(t, "15.204.22.177", resp.Active[0].IP)
	assert.InDelta(t, 98.0, resp.Active[0].AcceptPct, 0.01)
	assert.Equal(t, "2.0", resp.APIVersion)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleGetIPActivity_Empty(t *testing.T) {
	h, mock := newDeliverabilityTestHandler(t)
	cols := []string{"ip", "hostname", "pool", "status", "warmup_day",
		"sent", "delivered", "hard_bounce", "soft_bounce", "deferred", "complaint", "last_seen_at"}
	mock.ExpectQuery(`FROM mailing_ip_addresses`).
		WithArgs("24 hours").
		WillReturnRows(sqlmock.NewRows(cols))

	req := httptest.NewRequest("GET", "/ip-activity", nil)
	rec := httptest.NewRecorder()
	h.HandleGetIPActivity(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp ipActivityResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.Active)
	assert.Empty(t, resp.Cold)
	assert.Empty(t, resp.Paused)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleGetIPActivity_DBError(t *testing.T) {
	h, mock := newDeliverabilityTestHandler(t)
	mock.ExpectQuery(`FROM mailing_ip_addresses`).
		WithArgs("24 hours").
		WillReturnError(assertableError("ip query dead"))

	req := httptest.NewRequest("GET", "/ip-activity", nil)
	rec := httptest.NewRecorder()
	h.HandleGetIPActivity(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// ─── pure helpers ────────────────────────────────────────────────────────────

func TestPct(t *testing.T) {
	cases := []struct {
		num, denom int
		want       float64
	}{
		{950, 1000, 95.0},
		{0, 0, 0},
		{0, 100, 0},
		{50, 100, 50.0},
		{1, 3, 33.33},
	}
	for _, c := range cases {
		got := pct(c.num, c.denom)
		assert.InDelta(t, c.want, got, 0.01, "pct(%d,%d)", c.num, c.denom)
	}
}

func TestParseWindow(t *testing.T) {
	assert.Equal(t, "1 hour", parseWindow("1h", "24h"))
	assert.Equal(t, "6 hours", parseWindow("6h", "24h"))
	assert.Equal(t, "24 hours", parseWindow("24h", "1h"))
	assert.Equal(t, "7 days", parseWindow("7d", "24h"))
	// Unknown values fall through to default.
	assert.Equal(t, "24 hours", parseWindow("99h", "24h"))
	assert.Equal(t, "1 hour", parseWindow("", "1h"))
}

func TestMetricToFilter(t *testing.T) {
	for _, m := range []string{"attempted", "delivered", "hard_bounce", "soft_bounce", "deferred", "complaint", "reputation_blocked"} {
		_, ok := metricToFilter(m)
		assert.True(t, ok, "metric=%s", m)
	}
	_, ok := metricToFilter("opens")
	assert.False(t, ok)
}

// assertableError is a tiny helper so tests can return errors via sqlmock.
type errString string

func (e errString) Error() string { return string(e) }
func assertableError(s string) error {
	return errString(s)
}
