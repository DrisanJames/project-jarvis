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

func newTestLiveHandlers(t *testing.T) (*LiveCampaignHandlers, sqlmock.Sqlmock) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return NewLiveCampaignHandlers(db), mock
}

func TestLiveSnapshot_HardSoftBounceSplit(t *testing.T) {
	h, mock := newTestLiveHandlers(t)

	// 1. Campaign query: now includes hard_bounce_count and soft_bounce_count
	mock.ExpectQuery(`SELECT`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"name", "status", "subject", "total_recipients", "sent_count",
				"delivered_count", "open_count", "click_count", "bounce_count",
				"hard_bounce_count", "soft_bounce_count", "complaint_count", "revenue", "started_at"},
		).AddRow("Test Campaign", "sending", "Hello", 1000, 800,
			750, 200, 50, 30, 20, 10, 2, 150.0, nil))

	// 2. Deferred count from tracking_events
	mock.ExpectQuery(`deferred`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(45))

	// 3. Skipped count from campaign_queue
	mock.ExpectQuery(`mailing_campaign_queue`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(15))

	// 4. Recent tracking events
	mock.ExpectQuery(`mailing_tracking_events`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "event_type", "email", "event_at"}))

	// 5. AB variants query
	mock.ExpectQuery(`mailing_ab_variants`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "variant_name", "subject", "from_name", "sent", "opens", "clicks", "is_winner"}))

	// 6. Agent decisions query
	mock.ExpectQuery(`mailing_campaign_agent_decisions`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "decision_type", "reasoning", "action_taken", "impact", "decided_at"}))

	r := chi.NewRouter()
	r.Get("/campaigns/{id}/live", h.GetLiveSnapshot)
	req := httptest.NewRequest("GET", "/campaigns/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/live", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	funnel := resp["funnel"].(map[string]interface{})

	// Deferred count
	assert.Equal(t, float64(45), funnel["total_deferred"],
		"deferred messages in PMTA queue")

	// Bounce split is present
	assert.Equal(t, float64(30), funnel["total_bounced"])
	assert.Equal(t, float64(20), funnel["total_hard_bounced"],
		"must include hard bounce count separately")
	assert.Equal(t, float64(10), funnel["total_soft_bounced"],
		"must include soft bounce count separately")

	// Rates: open/click use delivered (750) as denominator
	assert.InDelta(t, 26.67, funnel["open_rate"].(float64), 0.5,
		"open_rate = 200/750*100 (delivered denom)")
	assert.InDelta(t, 6.67, funnel["click_rate"].(float64), 0.5,
		"click_rate = 50/750*100 (delivered denom)")

	// Bounce rates use sent (800) as denominator
	assert.InDelta(t, 3.75, funnel["bounce_rate"].(float64), 0.1,
		"bounce_rate = 30/800*100")
	assert.InDelta(t, 2.5, funnel["hard_bounce_rate"].(float64), 0.1,
		"hard_bounce_rate = 20/800*100")
	assert.InDelta(t, 1.25, funnel["soft_bounce_rate"].(float64), 0.1,
		"soft_bounce_rate = 10/800*100")

	// Complaint rate uses sent as denominator
	assert.InDelta(t, 0.25, funnel["complaint_rate"].(float64), 0.05,
		"complaint_rate = 2/800*100")

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLiveSnapshot_ZeroDelivered_RatesSafe(t *testing.T) {
	h, mock := newTestLiveHandlers(t)

	mock.ExpectQuery(`SELECT`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"name", "status", "subject", "total_recipients", "sent_count",
				"delivered_count", "open_count", "click_count", "bounce_count",
				"hard_bounce_count", "soft_bounce_count", "complaint_count", "revenue", "started_at"},
		).AddRow("Bounce Campaign", "completed", "Oops", 100, 100,
			0, 0, 0, 100, 80, 20, 0, 0.0, nil))

	mock.ExpectQuery(`deferred`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`mailing_campaign_queue`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`mailing_tracking_events`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "event_type", "email", "event_at"}))
	mock.ExpectQuery(`mailing_ab_variants`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "variant_name", "subject", "from_name", "sent", "opens", "clicks", "is_winner"}))
	mock.ExpectQuery(`mailing_campaign_agent_decisions`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "decision_type", "reasoning", "action_taken", "impact", "decided_at"}))

	r := chi.NewRouter()
	r.Get("/campaigns/{id}/live", h.GetLiveSnapshot)
	req := httptest.NewRequest("GET", "/campaigns/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/live", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	funnel := resp["funnel"].(map[string]interface{})

	// With 0 delivered, open/click rates must be 0 (not NaN/Inf)
	assert.Equal(t, float64(0), funnel["open_rate"], "open_rate safe at 0 when delivered=0")
	assert.Equal(t, float64(0), funnel["click_rate"], "click_rate safe at 0 when delivered=0")

	// Bounce rates still work (sent=100)
	assert.InDelta(t, 100.0, funnel["bounce_rate"].(float64), 0.1)
	assert.InDelta(t, 80.0, funnel["hard_bounce_rate"].(float64), 0.1)
	assert.InDelta(t, 20.0, funnel["soft_bounce_rate"].(float64), 0.1)

	require.NoError(t, mock.ExpectationsWereMet())
}
