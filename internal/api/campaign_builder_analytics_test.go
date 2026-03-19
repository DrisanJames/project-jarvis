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

// ─── HandleCampaignStats: Bounce Classification Tests ─────────────────────────

func TestCampaignStats_BounceClassification(t *testing.T) {
	cb, mock := newTestCampaignBuilder(t)

	// 1. Campaign summary query: sent=100, delivered=90, bounces=12 total
	mock.ExpectQuery(`SELECT COALESCE\(sent_count`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"sent", "delivered", "opens", "clicks", "bounces", "complaints", "unsubscribes"},
		).AddRow(100, 90, 30, 10, 12, 1, 2))

	// 2. Hard/soft bounce + deferred split from tracking events.
	mock.ExpectQuery(`event_type = 'bounced'`).
		WillReturnRows(sqlmock.NewRows([]string{"hard", "soft", "deferred"}).AddRow(8, 4, 15))

	// 3. Domain breakdown
	mock.ExpectQuery(`SPLIT_PART\(s.email`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"domain", "sent", "delivered", "opens", "clicks", "hard", "soft", "complaints"},
		).AddRow("gmail.com", 60, 55, 20, 6, 5, 2, 0).
			AddRow("yahoo.com", 40, 35, 10, 4, 3, 2, 1))

	// 4. PMTA config
	mock.ExpectQuery(`pmta_config`).
		WillReturnRows(sqlmock.NewRows([]string{"cfg"}))

	// 5. Wave data
	mock.ExpectQuery(`mailing_campaign_waves`).
		WillReturnRows(sqlmock.NewRows([]string{"plan_id", "planned", "enqueued"}))

	// 6. Timeline query: must also use event_type = 'bounced' + HardBounceSQL
	mock.ExpectQuery(`event_type = 'bounced'`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"hour", "sent", "delivered", "deferred", "opens", "clicks", "hard", "soft"},
		))

	r := chi.NewRouter()
	r.Get("/campaigns/{id}/stats", cb.HandleCampaignStats)
	req := httptest.NewRequest("GET", "/campaigns/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/stats", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Core assertion: hard/soft bounces are non-zero (the old bug returned 0)
	assert.Equal(t, float64(8), resp["hard_bounces"], "hard_bounces should be 8, not 0")
	assert.Equal(t, float64(4), resp["soft_bounces"], "soft_bounces should be 4, not 0")
	assert.Equal(t, float64(15), resp["deferred"], "deferred count from tracking events")
	assert.Equal(t, float64(12), resp["bounces"], "combined bounces should be 12")

	// Rate assertions: open_rate and click_rate use delivered as denominator
	openRate := resp["open_rate"].(float64)
	clickRate := resp["click_rate"].(float64)
	assert.InDelta(t, 33.33, openRate, 0.5, "open_rate = 30/90*100 (delivered denom)")
	assert.InDelta(t, 11.11, clickRate, 0.5, "click_rate = 10/90*100 (delivered denom)")

	// Bounce rates use sent as denominator
	hardBounceRate := resp["hard_bounce_rate"].(float64)
	softBounceRate := resp["soft_bounce_rate"].(float64)
	assert.InDelta(t, 8.0, hardBounceRate, 0.1, "hard_bounce_rate = 8/100*100")
	assert.InDelta(t, 4.0, softBounceRate, 0.1, "soft_bounce_rate = 4/100*100")

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCampaignStats_ZeroBounces(t *testing.T) {
	cb, mock := newTestCampaignBuilder(t)

	mock.ExpectQuery(`SELECT COALESCE\(sent_count`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"sent", "delivered", "opens", "clicks", "bounces", "complaints", "unsubscribes"},
		).AddRow(50, 48, 15, 5, 0, 0, 0))

	mock.ExpectQuery(`event_type = 'bounced'`).
		WillReturnRows(sqlmock.NewRows([]string{"hard", "soft", "deferred"}).AddRow(0, 0, 0))

	mock.ExpectQuery(`SPLIT_PART\(s.email`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"domain", "sent", "delivered", "opens", "clicks", "hard", "soft", "complaints"},
		))
	mock.ExpectQuery(`pmta_config`).
		WillReturnRows(sqlmock.NewRows([]string{"cfg"}))
	mock.ExpectQuery(`mailing_campaign_waves`).
		WillReturnRows(sqlmock.NewRows([]string{"plan_id", "planned", "enqueued"}))
	mock.ExpectQuery(`event_type = 'bounced'`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"hour", "sent", "delivered", "deferred", "opens", "clicks", "hard", "soft"},
		))

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
	assert.Equal(t, float64(0), resp["bounces"])
	assert.Equal(t, float64(0), resp["hard_bounce_rate"])
	assert.Equal(t, float64(0), resp["soft_bounce_rate"])
	assert.Equal(t, float64(0), resp["bounce_rate"])

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCampaignStats_RateDenominators(t *testing.T) {
	cb, mock := newTestCampaignBuilder(t)

	// sent=200, delivered=100 — big gap to verify denominator
	mock.ExpectQuery(`SELECT COALESCE\(sent_count`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"sent", "delivered", "opens", "clicks", "bounces", "complaints", "unsubscribes"},
		).AddRow(200, 100, 50, 20, 10, 2, 1))

	mock.ExpectQuery(`event_type = 'bounced'`).
		WillReturnRows(sqlmock.NewRows([]string{"hard", "soft", "deferred"}).AddRow(7, 3, 90))

	mock.ExpectQuery(`SPLIT_PART\(s.email`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"domain", "sent", "delivered", "opens", "clicks", "hard", "soft", "complaints"},
		))
	mock.ExpectQuery(`pmta_config`).
		WillReturnRows(sqlmock.NewRows([]string{"cfg"}))
	mock.ExpectQuery(`mailing_campaign_waves`).
		WillReturnRows(sqlmock.NewRows([]string{"plan_id", "planned", "enqueued"}))
	mock.ExpectQuery(`event_type = 'bounced'`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"hour", "sent", "delivered", "deferred", "opens", "clicks", "hard", "soft"},
		))

	r := chi.NewRouter()
	r.Get("/campaigns/{id}/stats", cb.HandleCampaignStats)
	req := httptest.NewRequest("GET", "/campaigns/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/stats", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// open_rate and click_rate must use delivered (100) as denominator
	assert.InDelta(t, 50.0, resp["open_rate"].(float64), 0.1,
		"open_rate = 50/100*100 = 50%% (delivered denom, NOT sent)")
	assert.InDelta(t, 20.0, resp["click_rate"].(float64), 0.1,
		"click_rate = 20/100*100 = 20%% (delivered denom, NOT sent)")

	// bounce rates must use sent (200) as denominator
	assert.InDelta(t, 3.5, resp["hard_bounce_rate"].(float64), 0.1,
		"hard_bounce_rate = 7/200*100 = 3.5%% (sent denom)")
	assert.InDelta(t, 1.5, resp["soft_bounce_rate"].(float64), 0.1,
		"soft_bounce_rate = 3/200*100 = 1.5%% (sent denom)")

	// complaint rate uses sent as denominator
	assert.InDelta(t, 1.0, resp["complaint_rate"].(float64), 0.1,
		"complaint_rate = 2/200*100 = 1%% (sent denom)")

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCampaignStats_ZeroDelivered_OpenRateIsZero(t *testing.T) {
	cb, mock := newTestCampaignBuilder(t)

	// sent=50, delivered=0 — open/click rates should be 0 (not Inf/NaN)
	mock.ExpectQuery(`SELECT COALESCE\(sent_count`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"sent", "delivered", "opens", "clicks", "bounces", "complaints", "unsubscribes"},
		).AddRow(50, 0, 0, 0, 50, 0, 0))

	mock.ExpectQuery(`event_type = 'bounced'`).
		WillReturnRows(sqlmock.NewRows([]string{"hard", "soft", "deferred"}).AddRow(40, 10, 0))

	mock.ExpectQuery(`SPLIT_PART\(s.email`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"domain", "sent", "delivered", "opens", "clicks", "hard", "soft", "complaints"},
		))
	mock.ExpectQuery(`pmta_config`).
		WillReturnRows(sqlmock.NewRows([]string{"cfg"}))
	mock.ExpectQuery(`mailing_campaign_waves`).
		WillReturnRows(sqlmock.NewRows([]string{"plan_id", "planned", "enqueued"}))
	mock.ExpectQuery(`event_type = 'bounced'`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"hour", "sent", "delivered", "deferred", "opens", "clicks", "hard", "soft"},
		))

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

func TestCampaignStats_DomainBreakdown_UsesCanonicalBounceSQL(t *testing.T) {
	cb, mock := newTestCampaignBuilder(t)

	mock.ExpectQuery(`SELECT COALESCE\(sent_count`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"sent", "delivered", "opens", "clicks", "bounces", "complaints", "unsubscribes"},
		).AddRow(100, 90, 30, 10, 5, 0, 0))

	mock.ExpectQuery(`event_type = 'bounced'`).
		WillReturnRows(sqlmock.NewRows([]string{"hard", "soft", "deferred"}).AddRow(3, 2, 5))

	// Domain breakdown: the query must use bounce_type classification, not event_type
	mock.ExpectQuery(`event_type = 'bounced' AND`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"domain", "sent", "delivered", "opens", "clicks", "hard", "soft", "complaints"},
		).AddRow("gmail.com", 60, 55, 20, 6, 2, 1, 0).
			AddRow("outlook.com", 40, 35, 10, 4, 1, 1, 0))

	mock.ExpectQuery(`pmta_config`).
		WillReturnRows(sqlmock.NewRows([]string{"cfg"}))
	mock.ExpectQuery(`mailing_campaign_waves`).
		WillReturnRows(sqlmock.NewRows([]string{"plan_id", "planned", "enqueued"}))
	mock.ExpectQuery(`event_type = 'bounced'`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"hour", "sent", "delivered", "deferred", "opens", "clicks", "hard", "soft"},
		))

	r := chi.NewRouter()
	r.Get("/campaigns/{id}/stats", cb.HandleCampaignStats)
	req := httptest.NewRequest("GET", "/campaigns/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/stats", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	domains := resp["domain_breakdown"].([]interface{})
	require.Len(t, domains, 2)

	gmail := domains[0].(map[string]interface{})
	assert.Equal(t, "gmail.com", gmail["domain"])
	assert.Equal(t, float64(2), gmail["hard_bounces"], "gmail hard bounces from canonical SQL")
	assert.Equal(t, float64(1), gmail["soft_bounces"], "gmail soft bounces from canonical SQL")

	// Domain-level open_rate uses delivered as denominator
	gmailOR := gmail["open_rate"].(float64)
	assert.InDelta(t, 36.36, gmailOR, 0.5, "gmail open_rate = 20/55*100 (delivered denom)")

	require.NoError(t, mock.ExpectationsWereMet())
}
