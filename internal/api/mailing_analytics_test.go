package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── parseAnalyticsRange Tests ────────────────────────────────────────────────

func TestParseAnalyticsRange_StartAndEnd(t *testing.T) {
	start := "2026-02-01T00:00:00Z"
	end := "2026-02-28T23:59:59Z"
	req := httptest.NewRequest("GET", "/?start_date="+start+"&end_date="+end, nil)

	s, e := parseAnalyticsRange(req)
	assert.Equal(t, 2026, s.Year())
	assert.Equal(t, time.February, s.Month())
	assert.Equal(t, 1, s.Day())
	assert.Equal(t, 2026, e.Year())
	assert.Equal(t, time.February, e.Month())
	assert.Equal(t, 28, e.Day())
}

func TestParseAnalyticsRange_OnlyStart(t *testing.T) {
	start := "2026-02-01T00:00:00Z"
	req := httptest.NewRequest("GET", "/?start_date="+start, nil)

	s, e := parseAnalyticsRange(req)
	assert.Equal(t, 2026, s.Year())
	assert.Equal(t, time.February, s.Month())
	assert.Equal(t, 1, s.Day())
	assert.WithinDuration(t, time.Now().UTC(), e, 5*time.Second, "end should default to ~now")
}

func TestParseAnalyticsRange_RangeTypes(t *testing.T) {
	tests := []struct {
		name      string
		rangeType string
		minAgo    time.Duration
		maxAgo    time.Duration
	}{
		{"1h", "1h", 55 * time.Minute, 65 * time.Minute},
		{"24h", "24h", 23*time.Hour + 55*time.Minute, 24*time.Hour + 5*time.Minute},
		{"7d", "7", 6*24*time.Hour + 23*time.Hour, 7*24*time.Hour + 1*time.Hour},
		{"14d", "14", 13*24*time.Hour + 23*time.Hour, 14*24*time.Hour + 1*time.Hour},
		{"90d", "90", 89*24*time.Hour + 23*time.Hour, 90*24*time.Hour + 1*time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/?range_type="+tc.rangeType, nil)
			s, e := parseAnalyticsRange(req)
			age := e.Sub(s)
			assert.True(t, age >= tc.minAgo && age <= tc.maxAgo,
				"range_type=%s: age=%v not in [%v, %v]", tc.rangeType, age, tc.minAgo, tc.maxAgo)
		})
	}
}

func TestParseAnalyticsRange_Today(t *testing.T) {
	req := httptest.NewRequest("GET", "/?range_type=today", nil)
	s, e := parseAnalyticsRange(req)

	now := time.Now().UTC()
	expectedStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	assert.Equal(t, expectedStart, s)
	assert.WithinDuration(t, now, e, 5*time.Second)
}

func TestParseAnalyticsRange_LegacyDays(t *testing.T) {
	req := httptest.NewRequest("GET", "/?days=45", nil)
	s, e := parseAnalyticsRange(req)
	age := e.Sub(s)
	assert.True(t, age >= 44*24*time.Hour && age <= 45*24*time.Hour+time.Hour,
		"days=45: age=%v", age)
}

func TestParseAnalyticsRange_DateOnlyFormat(t *testing.T) {
	req := httptest.NewRequest("GET", "/?start_date=2026-02-01&end_date=2026-02-28", nil)
	s, e := parseAnalyticsRange(req)

	assert.Equal(t, 2026, s.Year())
	assert.Equal(t, time.February, s.Month())
	assert.Equal(t, 1, s.Day())
	assert.Equal(t, 0, s.Hour())

	assert.Equal(t, 28, e.Day())
	assert.Equal(t, 23, e.Hour(), "date-only end should get 23:59:59")
	assert.Equal(t, 59, e.Minute())
}

func TestParseAnalyticsRange_Default30Days(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	s, e := parseAnalyticsRange(req)
	age := e.Sub(s)
	assert.True(t, age >= 29*24*time.Hour && age <= 30*24*time.Hour+time.Hour,
		"default: age=%v", age)
}

// ─── ComputeInfraRates Tests ──────────────────────────────────────────────────

func TestComputeInfraRates_Normal(t *testing.T) {
	r := ComputeInfraRates(1000, 900, 180, 45, 30, 20, 2, 10)

	assert.InDelta(t, 20.0, r.OpenRate, 0.1, "open_rate: 180/900*100")
	assert.InDelta(t, 5.0, r.ClickRate, 0.1, "click_rate: 45/900*100")
	assert.InDelta(t, 3.0, r.HardBounceRate, 0.1, "hard_bounce_rate: 30/1000*100")
	assert.InDelta(t, 2.0, r.SoftBounceRate, 0.1, "soft_bounce_rate: 20/1000*100")
	assert.InDelta(t, 0.2, r.ComplaintRate, 0.01, "complaint_rate: 2/1000*100")
	assert.InDelta(t, 1.0, r.DeferralRate, 0.1, "deferral_rate: 10/1000*100")
}

func TestComputeInfraRates_ZeroDelivered_FallbackToSent(t *testing.T) {
	r := ComputeInfraRates(100, 0, 20, 5, 7, 3, 1, 3)

	assert.InDelta(t, 20.0, r.OpenRate, 0.1, "should use sent as base when delivered=0")
	assert.InDelta(t, 5.0, r.ClickRate, 0.1)
	assert.InDelta(t, 7.0, r.HardBounceRate, 0.1)
	assert.InDelta(t, 3.0, r.SoftBounceRate, 0.1)
}

func TestComputeInfraRates_ZeroSent(t *testing.T) {
	r := ComputeInfraRates(0, 0, 0, 0, 0, 0, 0, 0)

	assert.Equal(t, 0.0, r.OpenRate)
	assert.Equal(t, 0.0, r.ClickRate)
	assert.Equal(t, 0.0, r.HardBounceRate)
	assert.Equal(t, 0.0, r.SoftBounceRate)
	assert.Equal(t, 0.0, r.ComplaintRate)
	assert.Equal(t, 0.0, r.DeferralRate)
}

func TestComputeInfraRates_EngagementBasedOnDelivered(t *testing.T) {
	r := ComputeInfraRates(100, 50, 50, 10, 3, 2, 0, 0)

	assert.InDelta(t, 100.0, r.OpenRate, 0.1, "50 opens / 50 delivered = 100%")
	assert.InDelta(t, 20.0, r.ClickRate, 0.1, "10 clicks / 50 delivered = 20%")
	assert.InDelta(t, 3.0, r.HardBounceRate, 0.1, "hard bounces use sent as base")
	assert.InDelta(t, 2.0, r.SoftBounceRate, 0.1, "soft bounces use sent as base")
}

// ─── HandleInfrastructureBreakdown Handler Tests ──────────────────────────────

func newInfraService(t *testing.T) (*AdvancedMailingService, sqlmock.Sqlmock) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return &AdvancedMailingService{db: db}, mock
}

func infraColumns() []string {
	return []string{"entity", "sent", "delivered", "opens", "clicks", "hard_bounces", "soft_bounces", "complaints", "deferred"}
}

func TestInfraBreakdown_Level1_DomainAggregate(t *testing.T) {
	svc, mock := newInfraService(t)

	campRows := sqlmock.NewRows([]string{"entity", "sent", "delivered"}).
		AddRow("example.com", 1000, 900).
		AddRow("other.com", 500, 450)
	mock.ExpectQuery("SELECT .+ FROM mailing_campaigns c WHERE .+").
		WillReturnRows(campRows)

	evtCols := []string{"entity", "opens", "clicks", "hard_bounces", "soft_bounces", "complaints", "deferred"}
	evtRows := sqlmock.NewRows(evtCols).
		AddRow("example.com", 180, 45, 30, 20, 2, 10).
		AddRow("other.com", 90, 20, 15, 10, 1, 5)
	mock.ExpectQuery("SELECT .+ FROM mailing_tracking_events t WHERE .+").
		WillReturnRows(evtRows)

	req := httptest.NewRequest("GET", "/?start_date=2026-01-01T00:00:00Z&end_date=2026-02-01T00:00:00Z", nil)
	rec := httptest.NewRecorder()
	svc.HandleInfrastructureBreakdown(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, VersionInfrastructureBreakdown, resp["api_version"])
	assert.Equal(t, "", resp["level"])

	data := resp["data"].([]interface{})
	assert.Len(t, data, 2)

	first := data[0].(map[string]interface{})
	assert.Equal(t, "example.com", first["entity"])
	assert.Equal(t, float64(1000), first["sent"])
	assert.Equal(t, float64(900), first["delivered"])
	assert.Equal(t, float64(180), first["opens"])
	assert.Equal(t, float64(45), first["clicks"])
}

func TestInfraBreakdown_Level2_IPDrilldown(t *testing.T) {
	svc, mock := newInfraService(t)

	// Level 2 first queries parent totals from campaigns
	campRow := sqlmock.NewRows([]string{"sent", "delivered"}).AddRow(1000, 900)
	mock.ExpectQuery("SELECT .+ FROM mailing_campaigns c WHERE .+").WillReturnRows(campRow)

	rows := sqlmock.NewRows(infraColumns()).
		AddRow("10.0.0.1", 500, 450, 90, 20, 15, 10, 1, 5)
	mock.ExpectQuery("SELECT .+ FROM mailing_tracking_events t WHERE .+").
		WillReturnRows(rows)

	req := httptest.NewRequest("GET", "/?start_date=2026-01-01T00:00:00Z&end_date=2026-02-01T00:00:00Z&domain=example.com&drilldown=ip", nil)
	rec := httptest.NewRecorder()
	svc.HandleInfrastructureBreakdown(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, "example.com", resp["level"])
	assert.Equal(t, "ip", resp["drilldown_type"])

	data := resp["data"].([]interface{})
	assert.Len(t, data, 1)
	row := data[0].(map[string]interface{})
	assert.Equal(t, "10.0.0.1", row["entity"])
	assert.Equal(t, float64(1000), row["parent_sent"])
	assert.Equal(t, float64(900), row["parent_delivered"])
}

func TestInfraBreakdown_Level2_ISPDrilldown(t *testing.T) {
	svc, mock := newInfraService(t)

	// Level 2 first queries parent totals from campaigns
	campRow := sqlmock.NewRows([]string{"sent", "delivered"}).AddRow(1000, 900)
	mock.ExpectQuery("SELECT .+ FROM mailing_campaigns c WHERE .+").WillReturnRows(campRow)

	rows := sqlmock.NewRows(infraColumns()).
		AddRow("gmail.com", 300, 280, 60, 15, 6, 4, 0, 2).
		AddRow("comcast.net", 200, 180, 40, 8, 3, 2, 1, 3)
	mock.ExpectQuery("SELECT .+ FROM mailing_tracking_events t LEFT JOIN mailing_subscribers sub .+").
		WillReturnRows(rows)

	req := httptest.NewRequest("GET", "/?start_date=2026-01-01T00:00:00Z&end_date=2026-02-01T00:00:00Z&domain=example.com&drilldown=isp", nil)
	rec := httptest.NewRecorder()
	svc.HandleInfrastructureBreakdown(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, "isp", resp["drilldown_type"])
	data := resp["data"].([]interface{})
	assert.Len(t, data, 2)
	assert.Equal(t, "gmail.com", data[0].(map[string]interface{})["entity"])
}

func TestInfraBreakdown_CampaignFilter(t *testing.T) {
	svc, mock := newInfraService(t)

	campRows := sqlmock.NewRows([]string{"entity", "sent", "delivered"}).
		AddRow("example.com", 500, 450)
	mock.ExpectQuery("SELECT .+ FROM mailing_campaigns c WHERE .+").
		WillReturnRows(campRows)

	evtRows := sqlmock.NewRows([]string{"entity", "opens", "clicks", "bounces", "complaints", "deferred"}).
		AddRow("example.com", 90, 20, 10, 0, 5)
	mock.ExpectQuery("SELECT .+ FROM mailing_tracking_events t WHERE .+").
		WillReturnRows(evtRows)

	req := httptest.NewRequest("GET", "/?start_date=2026-01-01T00:00:00Z&end_date=2026-02-01T00:00:00Z&campaign_id=abc-123", nil)
	rec := httptest.NewRecorder()
	svc.HandleInfrastructureBreakdown(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, "abc-123", resp["campaign_id"])
	data := resp["data"].([]interface{})
	assert.Len(t, data, 1)
}

func TestInfraBreakdown_InvalidDrilldown(t *testing.T) {
	svc, _ := newInfraService(t)

	req := httptest.NewRequest("GET", "/?start_date=2026-01-01T00:00:00Z&end_date=2026-02-01T00:00:00Z&domain=example.com&drilldown=garbage", nil)
	rec := httptest.NewRecorder()
	svc.HandleInfrastructureBreakdown(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid drilldown type")
}

func TestInfraBreakdown_DBError(t *testing.T) {
	svc, mock := newInfraService(t)

	mock.ExpectQuery("SELECT .+ FROM mailing_campaigns").WillReturnError(assert.AnError)

	req := httptest.NewRequest("GET", "/?start_date=2026-01-01T00:00:00Z&end_date=2026-02-01T00:00:00Z", nil)
	rec := httptest.NewRecorder()
	svc.HandleInfrastructureBreakdown(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "database error")
}

func TestInfraBreakdown_EmptyResults(t *testing.T) {
	svc, mock := newInfraService(t)

	campRows := sqlmock.NewRows([]string{"entity", "sent", "delivered"})
	mock.ExpectQuery("SELECT .+ FROM mailing_campaigns").WillReturnRows(campRows)
	evtRows := sqlmock.NewRows([]string{"entity", "opens", "clicks", "hard_bounces", "soft_bounces", "complaints", "deferred"})
	mock.ExpectQuery("SELECT .+ FROM mailing_tracking_events").WillReturnRows(evtRows)

	req := httptest.NewRequest("GET", "/?start_date=2026-01-01T00:00:00Z&end_date=2026-02-01T00:00:00Z", nil)
	rec := httptest.NewRecorder()
	svc.HandleInfrastructureBreakdown(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	data := resp["data"].([]interface{})
	assert.Len(t, data, 0, "empty results should return []")
}

func TestInfraBreakdown_RateCalculation_DeliveredBase(t *testing.T) {
	svc, mock := newInfraService(t)

	campRows := sqlmock.NewRows([]string{"entity", "sent", "delivered"}).
		AddRow("example.com", 100, 90)
	mock.ExpectQuery("SELECT .+ FROM mailing_campaigns").WillReturnRows(campRows)

	evtRows := sqlmock.NewRows([]string{"entity", "opens", "clicks", "hard_bounces", "soft_bounces", "complaints", "deferred"}).
		AddRow("example.com", 20, 5, 5, 3, 1, 2)
	mock.ExpectQuery("SELECT .+ FROM mailing_tracking_events").WillReturnRows(evtRows)

	req := httptest.NewRequest("GET", "/?start_date=2026-01-01T00:00:00Z&end_date=2026-02-01T00:00:00Z", nil)
	rec := httptest.NewRecorder()
	svc.HandleInfrastructureBreakdown(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	data := resp["data"].([]interface{})
	require.Len(t, data, 1)
	row := data[0].(map[string]interface{})

	openRate := row["open_rate"].(float64)
	clickRate := row["click_rate"].(float64)
	hardBounceRate := row["hard_bounce_rate"].(float64)
	softBounceRate := row["soft_bounce_rate"].(float64)

	assert.InDelta(t, 22.2, openRate, 0.2, "open_rate should be 20/90*100, not 20/100*100")
	assert.InDelta(t, 5.6, clickRate, 0.2, "click_rate should be 5/90*100")
	assert.InDelta(t, 5.0, hardBounceRate, 0.1, "hard_bounce_rate should be 5/100*100 (uses sent)")
	assert.InDelta(t, 3.0, softBounceRate, 0.1, "soft_bounce_rate should be 3/100*100 (uses sent)")
}

func TestInfraBreakdown_VersionInResponse(t *testing.T) {
	svc, mock := newInfraService(t)

	campRows := sqlmock.NewRows([]string{"entity", "sent", "delivered"})
	mock.ExpectQuery("SELECT .+ FROM mailing_campaigns").WillReturnRows(campRows)
	evtRows := sqlmock.NewRows([]string{"entity", "opens", "clicks", "hard_bounces", "soft_bounces", "complaints", "deferred"})
	mock.ExpectQuery("SELECT .+ FROM mailing_tracking_events").WillReturnRows(evtRows)

	req := httptest.NewRequest("GET", "/?start_date=2026-01-01T00:00:00Z&end_date=2026-02-01T00:00:00Z", nil)
	rec := httptest.NewRecorder()
	svc.HandleInfrastructureBreakdown(rec, req)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	ver, ok := resp["api_version"]
	assert.True(t, ok, "response must include api_version")
	assert.Equal(t, VersionInfrastructureBreakdown, ver)
}
