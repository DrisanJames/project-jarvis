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

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tt, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return tt
}

// Phase D + Phase E handler tests.
//
// Each test uses sqlmock to verify the handler:
//   1. Calls the expected DB query path
//   2. Decodes its response into the documented JSON shape
//   3. Surfaces api_version for deploy-drift detection (testing.mdc rule)

// ─── Phase D — Promoted analytics ────────────────────────────────────────────

func newPromotedSvc(t *testing.T) (*AdvancedMailingService, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &AdvancedMailingService{db: db}, mock
}

func TestHandleTerminalStateMatrix_HappyPath(t *testing.T) {
	svc, mock := newPromotedSvc(t)

	mock.ExpectQuery(`per_recipient`).
		WillReturnRows(sqlmock.NewRows([]string{
			"sending_domain", "recipient_domain",
			"recipients", "delivered", "hard_bounce", "soft_bounce_final",
			"deferred_open", "sent_only", "soft_bounce_events", "deferral_events",
		}).AddRow("em.discountblog.com", "gmail.com", 1000, 950, 5, 10, 5, 30, 22, 18))

	req := httptest.NewRequest("GET", "/x?range_type=today", nil)
	rec := httptest.NewRecorder()
	svc.HandleTerminalStateMatrix(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, VersionTerminalStateMatrix, resp["api_version"])
	assert.NotNil(t, resp["grand_total"])
	assert.NotNil(t, resp["by_isp"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleDomainISPMatrix_HappyPath(t *testing.T) {
	svc, mock := newPromotedSvc(t)

	mock.ExpectQuery(`pmta_acct_daily_summary`).
		WillReturnRows(sqlmock.NewRows([]string{
			"sending_domain", "isp", "sent", "delivered",
			"hard_bounces", "soft_bounces", "complaints", "deferred",
		}).AddRow("em.discountblog.com", "gmail", 1000, 950, 5, 15, 1, 4))

	req := httptest.NewRequest("GET", "/x?range_type=today", nil)
	rec := httptest.NewRecorder()
	svc.HandleDomainISPMatrix(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, VersionDomainISPMatrix, resp["api_version"])
	cells, ok := resp["cells"].([]interface{})
	require.True(t, ok)
	require.Len(t, cells, 1)
	cell := cells[0].(map[string]interface{})
	assert.InDelta(t, 95.0, cell["delivery_rate"], 0.1) // 950/1000
	assert.InDelta(t, 0.5, cell["hard_bounce_rate"], 0.01) // 5/1000
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleEngagementQuality_BucketsCorrectly(t *testing.T) {
	svc, mock := newPromotedSvc(t)

	// dom, sending_domain, mpp, opened_at, sent_at
	mock.ExpectQuery(`is_machine_open`).
		WillReturnRows(sqlmock.NewRows([]string{"dom", "sending_domain", "mpp", "opened_at", "sent_at"}).
			// Row 1: MPP — bucketed as mpp
			AddRow("gmail.com", "em.brand.com", true, mustParseTime(t, "2026-04-01T10:00:00Z"), mustParseTime(t, "2026-04-01T09:50:00Z")).
			// Row 2: human (>30s after send)
			AddRow("yahoo.com", "em.brand.com", false, mustParseTime(t, "2026-04-01T10:01:00Z"), mustParseTime(t, "2026-04-01T09:50:00Z")).
			// Row 3: scanner (within 30s)
			AddRow("aol.com", "em.brand.com", false, mustParseTime(t, "2026-04-01T10:00:10Z"), mustParseTime(t, "2026-04-01T10:00:00Z")).
			// Row 4: orphan (no send found)
			AddRow("hotmail.com", "em.brand.com", false, mustParseTime(t, "2026-04-01T10:00:00Z"), nil))

	req := httptest.NewRequest("GET", "/x?range_type=today", nil)
	rec := httptest.NewRecorder()
	svc.HandleEngagementQuality(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		APIVersion string                 `json:"api_version"`
		GrandTotal map[string]interface{} `json:"grand_total"`
		ByISP      []map[string]interface{} `json:"by_isp"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, VersionEngagementQuality, resp.APIVersion)
	assert.EqualValues(t, 4, resp.GrandTotal["total"])
	assert.EqualValues(t, 1, resp.GrandTotal["mpp"])
	assert.EqualValues(t, 1, resp.GrandTotal["scanner"])
	assert.EqualValues(t, 1, resp.GrandTotal["human"])
	assert.EqualValues(t, 1, resp.GrandTotal["no_send"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleQueueStatusHistogram_AggregatesByCampaign(t *testing.T) {
	svc, mock := newPromotedSvc(t)

	cols := []string{"campaign_id", "name", "status", "created_at",
		"scheduled_at", "started_at", "organization_id", "queue_status", "cnt"}
	t1 := mustParseTime(t, "2026-04-01T10:00:00Z")
	t2 := mustParseTime(t, "2026-04-01T11:00:00Z")
	mock.ExpectQuery(`mailing_campaigns[\s\S]*mailing_campaign_queue`).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("c1", "Campaign One", "sending", t1,
				nil, nil, "org-1", "sent", 800).
			AddRow("c1", "Campaign One", "sending", t1,
				nil, nil, "org-1", "pending", 200).
			AddRow("c2", "Campaign Two", "scheduled", t2,
				nil, nil, "org-1", "queued", 500))

	req := httptest.NewRequest("GET", "/x", nil)
	rec := httptest.NewRecorder()
	svc.HandleQueueStatusHistogram(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Campaigns []struct {
			CampaignID    string         `json:"campaign_id"`
			QueueByStatus map[string]int `json:"queue_by_status"`
			QueueTotal    int            `json:"queue_total"`
		} `json:"campaigns"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Campaigns, 2)
	c1Found := false
	for _, c := range resp.Campaigns {
		if c.CampaignID == "c1" {
			c1Found = true
			assert.Equal(t, 1000, c.QueueTotal)
			assert.Equal(t, 800, c.QueueByStatus["sent"])
			assert.Equal(t, 200, c.QueueByStatus["pending"])
		}
	}
	assert.True(t, c1Found, "campaign c1 should be present")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleWaveSchedulerHealth_FlagsZombies(t *testing.T) {
	svc, mock := newPromotedSvc(t)

	mock.ExpectQuery(`mailing_campaign_waves`).
		WillReturnRows(sqlmock.NewRows([]string{
			"zombies", "expired", "due_now", "planned", "enqueued", "running",
		}).AddRow(5, 12, 3, 100, 50, 25))

	mock.ExpectQuery(`mailing_campaign_waves[\s\S]*LIMIT 50`).
		WillReturnRows(sqlmock.NewRows([]string{
			"wave_id", "campaign_id", "name", "campaign_status",
			"wave_status", "scheduled_at", "window_end_at", "last_error",
		}))

	req := httptest.NewRequest("GET", "/x", nil)
	rec := httptest.NewRecorder()
	svc.HandleWaveSchedulerHealth(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Summary        map[string]int `json:"summary"`
		ActionRequired bool           `json:"action_required"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 5, resp.Summary["zombies"])
	assert.True(t, resp.ActionRequired, "zombies > 0 should set action_required")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleDispatchTimeline_BucketsBySent(t *testing.T) {
	svc, mock := newPromotedSvc(t)

	mock.ExpectQuery(`DATE_TRUNC\('minute'`).
		WillReturnRows(sqlmock.NewRows([]string{
			"minute", "sent", "delivered", "hard_bounces", "soft_bounces", "deferred",
		}).AddRow(mustParseTime(t, "2026-04-01T10:00:00Z"), 100, 95, 1, 4, 0))

	req := httptest.NewRequest("GET", "/x?minutes=15", nil)
	rec := httptest.NewRecorder()
	svc.HandleDispatchTimeline(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Minutes int                      `json:"minutes"`
		Buckets []map[string]interface{} `json:"buckets"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 15, resp.Minutes)
	require.Len(t, resp.Buckets, 1)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleGrowthNarrative_ComputesRates(t *testing.T) {
	svc, mock := newPromotedSvc(t)

	mock.ExpectQuery(`pmta_acct_daily_summary`).
		WillReturnRows(sqlmock.NewRows([]string{
			"summary_date", "sent", "delivered", "hard_bounces", "soft_bounces", "complaints", "deferred",
		}).
			AddRow(mustParseTime(t, "2026-03-30T00:00:00Z"), 1000, 950, 5, 15, 1, 4).
			AddRow(mustParseTime(t, "2026-03-31T00:00:00Z"), 2000, 1900, 10, 30, 2, 8))

	req := httptest.NewRequest("GET", "/x?days=30", nil)
	rec := httptest.NewRecorder()
	svc.HandleGrowthNarrative(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Summary map[string]interface{} `json:"summary"`
		Daily   []map[string]interface{} `json:"daily"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.EqualValues(t, 3000, resp.Summary["total_sent"])
	assert.EqualValues(t, 2850, resp.Summary["total_delivered"])
	require.Len(t, resp.Daily, 2)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ─── Phase E — Offer attribution ─────────────────────────────────────────────

func TestHandleOfferPerformance_PerSlugQuery(t *testing.T) {
	svc, mock := newPromotedSvc(t)

	// Every slug in the catalog runs one query — set the same mock
	// response for each. AnyArgsCount lets sqlmock match repeated
	// queries with the same shape.
	for range []string{"BXPFT55", "93W8N2N", "J876SLX", "K5C8PQQ", "K3TL7NJ", "KFSPRLK", "KG53427"} {
		mock.ExpectQuery(`link_url ILIKE`).
			WillReturnRows(sqlmock.NewRows([]string{"clicks", "unique_clickers", "campaigns_with_slug"}).
				AddRow(50, 30, 3))
	}

	req := httptest.NewRequest("GET", "/x?range_type=today", nil)
	rec := httptest.NewRecorder()
	svc.HandleOfferPerformance(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		APIVersion string                   `json:"api_version"`
		Rows       []map[string]interface{} `json:"rows"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, VersionOfferPerformance, resp.APIVersion)
	assert.GreaterOrEqual(t, len(resp.Rows), 5, "catalog should yield at least 5 known offers")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleSlugCoverage_DetectsSlugAndSuffix(t *testing.T) {
	svc, mock := newPromotedSvc(t)

	html := `Click <a href="https://www.cratoolpro.com/BJB4Q5BF/BXPFT55/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}">here</a>`
	mock.ExpectQuery(`mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "html"}).
			AddRow("c1", "TruGreen send", html).
			AddRow("c2", "Bad creative", "no slug, no suffix"))

	req := httptest.NewRequest("GET", "/x?range_type=today", nil)
	rec := httptest.NewRecorder()
	svc.HandleSlugCoverage(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Campaigns []struct {
			Name              string   `json:"name"`
			DetectedSlugs     []string `json:"detected_slugs"`
			HasTrackingSuffix bool     `json:"has_tracking_suffix"`
		} `json:"campaigns"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Campaigns, 2)

	var good, bad bool
	for _, c := range resp.Campaigns {
		if c.Name == "TruGreen send" {
			good = true
			assert.Contains(t, c.DetectedSlugs, "BXPFT55")
			assert.True(t, c.HasTrackingSuffix)
		}
		if c.Name == "Bad creative" {
			bad = true
			assert.Empty(t, c.DetectedSlugs)
			assert.False(t, c.HasTrackingSuffix)
		}
	}
	assert.True(t, good && bad)
	require.NoError(t, mock.ExpectationsWereMet())
}
