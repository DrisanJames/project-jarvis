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

func newTestJourneyCenter(t *testing.T) (*JourneyCenter, sqlmock.Sqlmock) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return &JourneyCenter{db: db}, mock
}

// ─── Phase 0 (Welcome Series plan) ─────────────────────────────────────────────
// Journey analytics endpoints used to query a non-existent
// mailing_journey_executions table and silently return zeros. After the
// Phase 0 view is in place, the same queries hit a view that wraps the
// executor's mailing_journey_execution_log writes. These tests verify that
// the queries succeed and that VersionJourneyAnalytics is surfaced in the
// response so the frontend can confirm it is talking to the right build.

func TestHandleJourneyMetrics_FromExecutionLogView(t *testing.T) {
	jc, mock := newTestJourneyCenter(t)

	// 1. Journey row with empty nodes JSON so the per-node loop is skipped.
	mock.ExpectQuery(`SELECT id, name, status, nodes FROM mailing_journeys`).
		WithArgs("j-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status", "nodes"}).
			AddRow("j-1", "Discount Blog Welcome", "active", "[]"))

	// 2. Enrollment counts.
	mock.ExpectQuery(`FROM mailing_journey_enrollments\s+WHERE journey_id`).
		WithArgs("j-1").
		WillReturnRows(sqlmock.NewRows(
			[]string{"total", "active", "completed", "converted", "exited"},
		).AddRow(120, 80, 30, 10, 10))

	// 3. Average time to complete.
	mock.ExpectQuery(`AVG\(EXTRACT\(EPOCH FROM \(completed_at - enrolled_at\)\)\)`).
		WithArgs("j-1").
		WillReturnRows(sqlmock.NewRows([]string{"avg_seconds"}).AddRow(86400.0))

	// 4. Email metrics (2026-08-04) — sent from the message_log send-truth
	// ledger, engagement from tracking events on the journey's shadow
	// campaigns. The old details-JSONB source was a documented '{}'
	// placeholder that rendered every email number as a confident zero.
	mock.ExpectQuery(`FROM mailing_message_log ml\s+JOIN mailing_campaigns c`).
		WithArgs("j-1").
		WillReturnRows(sqlmock.NewRows([]string{"sent"}).AddRow(994))
	mock.ExpectQuery(`FROM mailing_tracking_events t\s+JOIN mailing_campaigns c`).
		WithArgs("j-1").
		WillReturnRows(sqlmock.NewRows(
			[]string{"opens", "u_opens", "clicks", "u_clicks", "bounces", "unsubs"},
		).AddRow(277, 180, 644, 402, 2, 1))

	// 5. Hourly distribution.
	mock.ExpectQuery(`EXTRACT\(HOUR FROM enrolled_at\)`).
		WithArgs("j-1").
		WillReturnRows(sqlmock.NewRows([]string{"hour", "enrollments", "completions"}))

	r := chi.NewRouter()
	r.Get("/journeys/{journeyId}/metrics", jc.HandleJourneyMetrics)
	req := httptest.NewRequest("GET", "/journeys/j-1/metrics", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp JourneyMetrics
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, "j-1", resp.JourneyID)
	assert.Equal(t, "Discount Blog Welcome", resp.JourneyName)
	assert.Equal(t, 120, resp.TotalEnrollments)
	assert.Equal(t, 80, resp.ActiveEnrollments)
	assert.Equal(t, 30, resp.CompletedCount)
	assert.Equal(t, 10, resp.ConvertedCount)
	assert.Equal(t, 10, resp.ExitedCount)
	assert.InDelta(t, 0.25, resp.CompletionRate, 0.001)
	assert.InDelta(t, 1.0/12.0, resp.ConversionRate, 0.001)
	// Email metrics from the real ledgers — a zero here means the handler
	// regressed to a source click-drip does not write (the aug04 false alarm).
	assert.Equal(t, 994, resp.EmailMetrics.TotalSent)
	assert.Equal(t, 277, resp.EmailMetrics.TotalOpens)
	assert.Equal(t, 180, resp.EmailMetrics.UniqueOpens)
	assert.Equal(t, 644, resp.EmailMetrics.TotalClicks)
	assert.Equal(t, 402, resp.EmailMetrics.UniqueClicks)
	assert.Equal(t, 2, resp.EmailMetrics.Bounces)
	assert.Equal(t, 1, resp.EmailMetrics.Unsubscribes)
	assert.InDelta(t, 180.0/994.0, resp.EmailMetrics.OpenRate, 0.001)
	assert.InDelta(t, 402.0/994.0, resp.EmailMetrics.ClickRate, 0.001)
	assert.Equal(t, VersionJourneyAnalytics, resp.APIVersion,
		"api_version must surface so the frontend can verify the deployed build")

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleJourneyMetrics_NotFound(t *testing.T) {
	jc, mock := newTestJourneyCenter(t)

	mock.ExpectQuery(`SELECT id, name, status, nodes FROM mailing_journeys`).
		WithArgs("missing").
		WillReturnError(sqlmock.ErrCancelled)

	r := chi.NewRouter()
	r.Get("/journeys/{journeyId}/metrics", jc.HandleJourneyMetrics)
	req := httptest.NewRequest("GET", "/journeys/missing/metrics", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleJourneyFunnel_ApiVersionSurfaced(t *testing.T) {
	jc, mock := newTestJourneyCenter(t)

	mock.ExpectQuery(`SELECT id, name, nodes FROM mailing_journeys`).
		WithArgs("j-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "nodes"}).
			AddRow("j-1", "DB Welcome", "[]"))

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM mailing_journey_enrollments`).
		WithArgs("j-1").
		WillReturnRows(sqlmock.NewRows([]string{"total"}).AddRow(50))

	r := chi.NewRouter()
	r.Get("/journeys/{journeyId}/funnel", jc.HandleJourneyFunnel)
	req := httptest.NewRequest("GET", "/journeys/j-1/funnel", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp JourneyFunnelResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 50, resp.TotalStart)
	assert.Equal(t, VersionJourneyAnalytics, resp.APIVersion)

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleJourneyPerformanceComparison_ApiVersionSurfaced(t *testing.T) {
	jc, mock := newTestJourneyCenter(t)

	// No journeys returned — exercises the success path with empty rowset.
	mock.ExpectQuery(`FROM mailing_journeys j\s+LEFT JOIN mailing_journey_enrollments`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "name", "status", "total", "completed", "converted", "avg_time",
		}))

	r := chi.NewRouter()
	r.Get("/performance", jc.HandleJourneyPerformanceComparison)
	req := httptest.NewRequest("GET", "/performance", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, VersionJourneyAnalytics, resp["api_version"])

	require.NoError(t, mock.ExpectationsWereMet())
}
