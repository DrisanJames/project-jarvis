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

// TestHandleStatus_FallbackMetricsShape verifies that when no Jarvis campaign
// is running and the handler falls back to a DB campaign, the response metrics
// object contains all fields the JarvisDashboard frontend expects -- specifically
// total_sent, total_delivered, total_opens, total_clicks, total_conversions,
// total_bounces, conversion_rate, total_revenue, and revenue_per_send. Without
// these fields, the frontend crashes with "Cannot read properties of undefined
// (reading 'toFixed')".
func TestHandleStatus_FallbackMetricsShape(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	j := &JarvisOrchestrator{
		db:       db,
		campaign: nil, // no Jarvis campaign running — triggers fallback path
	}

	scheduledAt := time.Now().Add(-1 * time.Hour)
	startedAt := time.Now().Add(-30 * time.Minute)

	mock.ExpectQuery("SELECT id::text").
		WillReturnRows(
			sqlmock.NewRows([]string{
				"id", "name", "status", "from_name", "from_email", "subject",
				"sent_count", "total_recipients", "delivered_count",
				"bounce_count", "open_count", "click_count",
				"complaint_count", "unsubscribe_count",
				"scheduled_at", "started_at",
			}).AddRow(
				"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
				"Test Campaign", "sending",
				"Sender", "sender@example.com", "Test Subject",
				1000, 5000, 800,
				50, 200, 30,
				2, 5,
				scheduledAt, startedAt,
			),
		)

	req := httptest.NewRequest("GET", "/api/mailing/jarvis/status", nil)
	w := httptest.NewRecorder()

	j.HandleStatus(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "expected 200 OK")

	var resp map[string]interface{}
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err, "response should be valid JSON")

	metrics, ok := resp["metrics"].(map[string]interface{})
	require.True(t, ok, "response must contain 'metrics' object")

	// These are the fields the JarvisDashboard.tsx calls .toFixed() on.
	// If any are missing, the frontend crashes.
	requiredFields := []string{
		"total_sent", "total_delivered", "total_opens", "total_clicks",
		"total_conversions", "total_bounces", "conversion_rate",
		"total_revenue", "revenue_per_send",
		"open_rate", "click_rate",
	}
	for _, field := range requiredFields {
		_, exists := metrics[field]
		assert.True(t, exists, "metrics must contain '%s' to prevent frontend crash", field)
	}

	// Verify actual values are sane
	assert.Equal(t, float64(1000), metrics["total_sent"])
	assert.Equal(t, float64(800), metrics["total_delivered"])
	assert.Equal(t, float64(200), metrics["total_opens"])
	assert.Equal(t, float64(30), metrics["total_clicks"])
	assert.Equal(t, float64(0), metrics["total_conversions"])
	assert.Equal(t, float64(50), metrics["total_bounces"])
	assert.Equal(t, float64(0), metrics["conversion_rate"])

	// open_rate should be opens/max(delivered, sent) * 100 = 200/1000 * 100 = 20.0
	openRate, _ := metrics["open_rate"].(float64)
	assert.InDelta(t, 20.0, openRate, 0.1, "open_rate = opens/max(delivered,sent) * 100")

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestHandleStatus_Idle verifies that when no Jarvis campaign is running
// and no DB campaign exists, we get the idle response without crashing.
func TestHandleStatus_Idle(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	j := &JarvisOrchestrator{
		db:       db,
		campaign: nil,
	}

	mock.ExpectQuery("SELECT id::text").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "name", "status", "from_name", "from_email", "subject",
			"sent_count", "total_recipients", "delivered_count",
			"bounce_count", "open_count", "click_count",
			"complaint_count", "unsubscribe_count",
			"scheduled_at", "started_at",
		}))

	req := httptest.NewRequest("GET", "/api/mailing/jarvis/status", nil)
	w := httptest.NewRecorder()

	j.HandleStatus(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, "idle", resp["status"])

	assert.NoError(t, mock.ExpectationsWereMet())
}
