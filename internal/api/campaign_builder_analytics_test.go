package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestCampaignBuilder(t *testing.T) (*CampaignBuilder, sqlmock.Sqlmock) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return &CampaignBuilder{db: db}, mock
}

// Phase B (VersionCampaignBuilder=1.0) reshaped HandleCampaignStats:
//
//   - The combined `bounces`/`bounce_rate` fields are gone (per
//     .cursor/rules/bounce-metrics.mdc — hard and soft are always separate).
//   - The expensive per-domain breakdown query only runs when ?include=domain.
//   - The hourly timeline only runs when ?include=hourly.
//   - The campaign-summary SELECT no longer fetches bounce_count (taken from
//     mailing_tracking_events instead, which is the only place the hard/soft
//     split lives).
//   - A queue_status histogram from mailing_campaign_queue is always returned.
//
// The tests below mock the new query order and assert the new contract.

// expectStatsCorePath sets up the mock expectations every HandleCampaignStats
// run hits regardless of ?include= flags.
//
//   1. campaign summary (sent, delivered, opens, clicks, complaints, unsubs)
//   2. tracking_events bounce split (hard, soft, deferred)
//   3. ISP aggregation query (when domain breakdown NOT requested)
//   4. pmta_config row
//   5. mailing_campaign_waves rows
//   6. mailing_campaign_queue histogram
func expectStatsCorePath(mock sqlmock.Sqlmock, sent, delivered, opens, clicks, complaints, unsubs int, hard, soft, deferred int) {
	mock.ExpectQuery(`SELECT COALESCE\(sent_count`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"sent", "delivered", "opens", "clicks", "complaints", "unsubscribes"},
		).AddRow(sent, delivered, opens, clicks, complaints, unsubs))

	mock.ExpectQuery(`event_type = 'bounced'`).
		WillReturnRows(sqlmock.NewRows([]string{"hard", "soft", "deferred"}).AddRow(hard, soft, deferred))
}

// expectISPAggOnly mocks the ISP-only path used when ?include=domain is NOT set.
func expectISPAggOnly(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`recipient_domain IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"domain", "sent", "delivered", "opens", "clicks", "hard", "soft", "complaints"},
		))
}

func expectStatsTailQueries(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`pmta_config`).WillReturnRows(sqlmock.NewRows([]string{"cfg"}))
	mock.ExpectQuery(`mailing_campaign_waves`).
		WillReturnRows(sqlmock.NewRows([]string{"plan_id", "planned", "enqueued"}))
	mock.ExpectQuery(`mailing_campaign_queue`).
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}))
}

func TestCampaignStats_BounceClassification(t *testing.T) {
	cb, mock := newTestCampaignBuilder(t)

	expectStatsCorePath(mock, 100, 90, 30, 10, 1, 2, 8, 4, 15)
	expectISPAggOnly(mock)
	expectStatsTailQueries(mock)

	r := chi.NewRouter()
	r.Get("/campaigns/{id}/stats", cb.HandleCampaignStats)
	req := httptest.NewRequest("GET", "/campaigns/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/stats", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, float64(8), resp["hard_bounces"], "hard_bounces should be 8, not 0")
	assert.Equal(t, float64(4), resp["soft_bounces"], "soft_bounces should be 4, not 0")
	assert.Equal(t, float64(15), resp["deferred"], "deferred count from tracking events")

	// Phase B: combined bounces/bounce_rate are intentionally absent.
	_, hasCombined := resp["bounces"]
	assert.False(t, hasCombined, "bounces (combined) must not be in response — see bounce-metrics.mdc")
	_, hasCombinedRate := resp["bounce_rate"]
	assert.False(t, hasCombinedRate, "bounce_rate (combined) must not be in response — see bounce-metrics.mdc")

	openRate := resp["open_rate"].(float64)
	clickRate := resp["click_rate"].(float64)
	assert.InDelta(t, 33.33, openRate, 0.5, "open_rate = 30/90*100 (delivered denom)")
	assert.InDelta(t, 11.11, clickRate, 0.5, "click_rate = 10/90*100 (delivered denom)")

	hardBounceRate := resp["hard_bounce_rate"].(float64)
	softBounceRate := resp["soft_bounce_rate"].(float64)
	assert.InDelta(t, 8.0, hardBounceRate, 0.1, "hard_bounce_rate = 8/100*100")
	assert.InDelta(t, 4.0, softBounceRate, 0.1, "soft_bounce_rate = 4/100*100")

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCampaignStats_ZeroBounces(t *testing.T) {
	cb, mock := newTestCampaignBuilder(t)

	expectStatsCorePath(mock, 50, 48, 15, 5, 0, 0, 0, 0, 0)
	expectISPAggOnly(mock)
	expectStatsTailQueries(mock)

	r := chi.NewRouter()
	r.Get("/campaigns/{id}/stats", cb.HandleCampaignStats)
	req := httptest.NewRequest("GET", "/campaigns/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/stats", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, float64(0), resp["hard_bounces"])
	assert.Equal(t, float64(0), resp["soft_bounces"])
	assert.Equal(t, float64(0), resp["hard_bounce_rate"])
	assert.Equal(t, float64(0), resp["soft_bounce_rate"])

	// queue_status field is always present (may be empty {} when no queue rows).
	require.NotNil(t, resp["queue_status"], "queue_status histogram should always be in response")

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCampaignStats_RateDenominators(t *testing.T) {
	cb, mock := newTestCampaignBuilder(t)

	expectStatsCorePath(mock, 200, 100, 50, 20, 2, 1, 7, 3, 90)
	expectISPAggOnly(mock)
	expectStatsTailQueries(mock)

	r := chi.NewRouter()
	r.Get("/campaigns/{id}/stats", cb.HandleCampaignStats)
	req := httptest.NewRequest("GET", "/campaigns/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/stats", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.InDelta(t, 50.0, resp["open_rate"].(float64), 0.1,
		"open_rate = 50/100*100 = 50%% (delivered denom, NOT sent)")
	assert.InDelta(t, 20.0, resp["click_rate"].(float64), 0.1,
		"click_rate = 20/100*100 = 20%% (delivered denom, NOT sent)")

	assert.InDelta(t, 3.5, resp["hard_bounce_rate"].(float64), 0.1,
		"hard_bounce_rate = 7/200*100 = 3.5%% (sent denom)")
	assert.InDelta(t, 1.5, resp["soft_bounce_rate"].(float64), 0.1,
		"soft_bounce_rate = 3/200*100 = 1.5%% (sent denom)")

	assert.InDelta(t, 1.0, resp["complaint_rate"].(float64), 0.1,
		"complaint_rate = 2/200*100 = 1%% (sent denom)")

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCampaignStats_ZeroDelivered_OpenRateIsZero(t *testing.T) {
	cb, mock := newTestCampaignBuilder(t)

	expectStatsCorePath(mock, 50, 0, 0, 0, 0, 0, 40, 10, 0)
	expectISPAggOnly(mock)
	expectStatsTailQueries(mock)

	r := chi.NewRouter()
	r.Get("/campaigns/{id}/stats", cb.HandleCampaignStats)
	req := httptest.NewRequest("GET", "/campaigns/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/stats", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, float64(0), resp["open_rate"], "open_rate should be 0 when delivered=0")
	assert.Equal(t, float64(0), resp["click_rate"], "click_rate should be 0 when delivered=0")
	assert.InDelta(t, 80.0, resp["hard_bounce_rate"].(float64), 0.1, "hard_bounce_rate = 40/50*100")
	assert.InDelta(t, 20.0, resp["soft_bounce_rate"].(float64), 0.1, "soft_bounce_rate = 10/50*100")

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCampaignStats_DomainBreakdown_OptIn(t *testing.T) {
	// With ?include=domain the handler runs the heavy SPLIT_PART(s.email,'@',2)
	// aggregation; without it, only the lighter ISP-only query is issued.
	cb, mock := newTestCampaignBuilder(t)

	expectStatsCorePath(mock, 100, 90, 30, 10, 0, 0, 3, 2, 5)

	mock.ExpectQuery(`SPLIT_PART\(s.email`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"domain", "sent", "delivered", "opens", "clicks", "hard", "soft", "complaints"},
		).AddRow("gmail.com", 60, 55, 20, 6, 2, 1, 0).
			AddRow("outlook.com", 40, 35, 10, 4, 1, 1, 0))

	expectStatsTailQueries(mock)

	r := chi.NewRouter()
	r.Get("/campaigns/{id}/stats", cb.HandleCampaignStats)
	req := httptest.NewRequest("GET", "/campaigns/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/stats?include=domain", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	domains := resp["domain_breakdown"].([]interface{})
	require.Len(t, domains, 2, "domain breakdown should be populated when include=domain is set")

	gmail := domains[0].(map[string]interface{})
	assert.Equal(t, "gmail.com", gmail["domain"])
	assert.Equal(t, float64(2), gmail["hard_bounces"], "gmail hard bounces from canonical SQL")
	assert.Equal(t, float64(1), gmail["soft_bounces"], "gmail soft bounces from canonical SQL")

	gmailOR := gmail["open_rate"].(float64)
	assert.InDelta(t, 36.36, gmailOR, 0.5, "gmail open_rate = 20/55*100 (delivered denom)")

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCampaignStats_DomainBreakdown_DefaultOff(t *testing.T) {
	// Without ?include=domain we must NOT issue the SPLIT_PART query.
	// The lighter recipient_domain aggregation runs instead.
	cb, mock := newTestCampaignBuilder(t)

	expectStatsCorePath(mock, 100, 90, 30, 10, 0, 0, 3, 2, 5)
	expectISPAggOnly(mock)
	expectStatsTailQueries(mock)

	r := chi.NewRouter()
	r.Get("/campaigns/{id}/stats", cb.HandleCampaignStats)
	req := httptest.NewRequest("GET", "/campaigns/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/stats", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	domains := resp["domain_breakdown"].([]interface{})
	assert.Empty(t, domains, "domain_breakdown should be empty when include=domain is NOT set")

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCampaignStats_HourlyTimeline_OptIn(t *testing.T) {
	cb, mock := newTestCampaignBuilder(t)

	expectStatsCorePath(mock, 100, 90, 30, 10, 0, 0, 3, 2, 5)
	expectISPAggOnly(mock)
	// pmta_config + waves run, then the timeline (because wantHourly=true),
	// then the queue histogram. Order matches HandleCampaignStats body.
	mock.ExpectQuery(`pmta_config`).WillReturnRows(sqlmock.NewRows([]string{"cfg"}))
	mock.ExpectQuery(`mailing_campaign_waves`).
		WillReturnRows(sqlmock.NewRows([]string{"plan_id", "planned", "enqueued"}))
	mock.ExpectQuery(`DATE_TRUNC\('hour'`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"hour", "sent", "delivered", "deferred", "opens", "clicks", "hard", "soft"},
		))
	mock.ExpectQuery(`mailing_campaign_queue`).
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}))

	r := chi.NewRouter()
	r.Get("/campaigns/{id}/stats", cb.HandleCampaignStats)
	req := httptest.NewRequest("GET", "/campaigns/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/stats?include=hourly", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	require.NotNil(t, resp["hourly_timeline"], "hourly_timeline should be present (possibly empty) when include=hourly")

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCampaignStats_QueueStatus_Surfaced(t *testing.T) {
	// Phase B: queue_status histogram is added so the UI can show what's
	// actually in flight without bouncing to Outbox.
	cb, mock := newTestCampaignBuilder(t)

	expectStatsCorePath(mock, 1000, 900, 0, 0, 0, 0, 0, 0, 0)
	expectISPAggOnly(mock)
	mock.ExpectQuery(`pmta_config`).WillReturnRows(sqlmock.NewRows([]string{"cfg"}))
	mock.ExpectQuery(`mailing_campaign_waves`).
		WillReturnRows(sqlmock.NewRows([]string{"plan_id", "planned", "enqueued"}))
	mock.ExpectQuery(`mailing_campaign_queue`).
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}).
			AddRow("queued", 250).
			AddRow("sending", 50).
			AddRow("sent", 700).
			AddRow("dead_lettered", 0))

	r := chi.NewRouter()
	r.Get("/campaigns/{id}/stats", cb.HandleCampaignStats)
	req := httptest.NewRequest("GET", "/campaigns/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/stats", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	qs, ok := resp["queue_status"].(map[string]interface{})
	require.True(t, ok, "queue_status should be a map")
	assert.Equal(t, float64(250), qs["queued"])
	assert.Equal(t, float64(50), qs["sending"])
	assert.Equal(t, float64(700), qs["sent"])

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCampaignStats_APIVersion_Surfaced(t *testing.T) {
	cb, mock := newTestCampaignBuilder(t)

	expectStatsCorePath(mock, 1, 1, 0, 0, 0, 0, 0, 0, 0)
	expectISPAggOnly(mock)
	expectStatsTailQueries(mock)

	r := chi.NewRouter()
	r.Get("/campaigns/{id}/stats", cb.HandleCampaignStats)
	req := httptest.NewRequest("GET", "/campaigns/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/stats", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, VersionCampaignBuilder, resp["api_version"])

	require.NoError(t, mock.ExpectationsWereMet())
}
