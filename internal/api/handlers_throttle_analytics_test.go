package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThrottleAnalytics_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	now := time.Now().UTC().Truncate(time.Second)
	rows := sqlmock.NewRows([]string{"isp", "action_taken", "action_params", "result", "created_at"}).
		AddRow("gmail", "reduce_rate", `{"effective_rate":350,"rate_adj":0.7,"deferral_rate":25.3,"backoff_step":3}`, "applied", now).
		AddRow("yahoo", "increase_rate", `{"effective_rate":500,"rate_adj":1.0,"deferral_rate":5.0,"recovering":true}`, "applied", now.Add(-time.Minute))

	mock.ExpectQuery(`SELECT isp, action_taken, action_params, result, created_at`).
		WithArgs("org-1", ).
		WillReturnRows(rows)

	registry := engine.NewISPRateRegistry()
	registry.SetRate(engine.ISPGmail, 350)
	registry.SetRate(engine.ISPYahoo, 500)

	configs := map[engine.ISP]engine.ISPConfig{
		engine.ISPGmail: {DisplayName: "Gmail", MaxMsgRate: 500},
		engine.ISPYahoo: {DisplayName: "Yahoo", MaxMsgRate: 500},
	}

	handler := &throttleAnalyticsHandler{
		registry: registry,
		configs:  configs,
		db:       db,
		orgID:    "org-1",
	}

	req := httptest.NewRequest(http.MethodGet, "/engine/throttle-analytics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp throttleAnalyticsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Live rates: sorted alphabetically
	require.Len(t, resp.LiveRates, 2)
	assert.Equal(t, "gmail", resp.LiveRates[0].ISP)
	assert.Equal(t, "Gmail", resp.LiveRates[0].DisplayName)
	assert.Equal(t, 350.0, resp.LiveRates[0].CurrentRate)
	assert.Equal(t, 500, resp.LiveRates[0].MaxRate)
	assert.Equal(t, 70.0, resp.LiveRates[0].RatePct)

	assert.Equal(t, "yahoo", resp.LiveRates[1].ISP)
	assert.Equal(t, 100.0, resp.LiveRates[1].RatePct)

	// Decisions
	require.Len(t, resp.RecentDecisions, 2)
	assert.Equal(t, "gmail", resp.RecentDecisions[0].ISP)
	assert.Equal(t, "reduce_rate", resp.RecentDecisions[0].Action)
	assert.Equal(t, 350.0, resp.RecentDecisions[0].EffectiveRate)
	assert.Equal(t, 0.7, resp.RecentDecisions[0].RateAdj)
	assert.Equal(t, 25.3, resp.RecentDecisions[0].DeferralRate)
	assert.Equal(t, 3.0, resp.RecentDecisions[0].BackoffStep)
	assert.False(t, resp.RecentDecisions[0].Recovering)

	assert.Equal(t, "yahoo", resp.RecentDecisions[1].ISP)
	assert.Equal(t, "increase_rate", resp.RecentDecisions[1].Action)
	assert.True(t, resp.RecentDecisions[1].Recovering)

	// Convictions: empty because no conviction store was provided
	assert.Empty(t, resp.Convictions)

	assert.False(t, resp.UpdatedAt.IsZero())

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestThrottleAnalytics_NilRegistryAndDB(t *testing.T) {
	handler := &throttleAnalyticsHandler{
		registry: nil,
		configs:  nil,
		db:       nil,
		orgID:    "org-1",
	}

	req := httptest.NewRequest(http.MethodGet, "/engine/throttle-analytics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp throttleAnalyticsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Empty(t, resp.LiveRates)
	assert.Empty(t, resp.RecentDecisions)
	assert.Empty(t, resp.Convictions)
}

func TestThrottleAnalytics_DBError(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery(`SELECT isp, action_taken, action_params, result, created_at`).
		WillReturnError(fmt.Errorf("connection refused"))

	registry := engine.NewISPRateRegistry()
	registry.SetRate(engine.ISPGmail, 500)

	handler := &throttleAnalyticsHandler{
		registry: registry,
		configs:  map[engine.ISP]engine.ISPConfig{engine.ISPGmail: {DisplayName: "Gmail", MaxMsgRate: 500}},
		db:       db,
		orgID:    "org-1",
	}

	req := httptest.NewRequest(http.MethodGet, "/engine/throttle-analytics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp throttleAnalyticsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Live rates still returned despite DB error
	require.Len(t, resp.LiveRates, 1)
	assert.Equal(t, "gmail", resp.LiveRates[0].ISP)

	// Decisions empty due to DB error
	assert.Empty(t, resp.RecentDecisions)

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestThrottleAnalytics_EmptyDBResults(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	rows := sqlmock.NewRows([]string{"isp", "action_taken", "action_params", "result", "created_at"})
	mock.ExpectQuery(`SELECT isp, action_taken, action_params, result, created_at`).
		WillReturnRows(rows)

	handler := &throttleAnalyticsHandler{
		registry: engine.NewISPRateRegistry(),
		db:       db,
		orgID:    "org-1",
	}

	req := httptest.NewRequest(http.MethodGet, "/engine/throttle-analytics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var resp throttleAnalyticsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Empty(t, resp.LiveRates)
	assert.Empty(t, resp.RecentDecisions)
}

func TestThrottleAnalytics_RatePctCappedAt100(t *testing.T) {
	registry := engine.NewISPRateRegistry()
	registry.SetRate(engine.ISPGmail, 600)

	configs := map[engine.ISP]engine.ISPConfig{
		engine.ISPGmail: {DisplayName: "Gmail", MaxMsgRate: 500},
	}

	handler := &throttleAnalyticsHandler{
		registry: registry,
		configs:  configs,
		orgID:    "org-1",
	}

	rates := handler.buildLiveRates()
	require.Len(t, rates, 1)
	assert.Equal(t, 100.0, rates[0].RatePct, "rate_pct must be capped at 100 even when current > max")
	assert.Equal(t, 600.0, rates[0].CurrentRate, "current_rate should still reflect the actual value")
}

func TestThrottleAnalytics_LiveRatesSorted(t *testing.T) {
	registry := engine.NewISPRateRegistry()
	registry.SetRate(engine.ISPYahoo, 300)
	registry.SetRate(engine.ISPGmail, 500)
	registry.SetRate(engine.ISPComcast, 100)

	handler := &throttleAnalyticsHandler{
		registry: registry,
		configs:  map[engine.ISP]engine.ISPConfig{},
		orgID:    "org-1",
	}

	rates := handler.buildLiveRates()
	require.Len(t, rates, 3)
	assert.Equal(t, "comcast", rates[0].ISP)
	assert.Equal(t, "gmail", rates[1].ISP)
	assert.Equal(t, "yahoo", rates[2].ISP)
}

func TestThrottleAnalytics_MalformedActionParams(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{"isp", "action_taken", "action_params", "result", "created_at"}).
		AddRow("gmail", "reduce_rate", `{invalid json`, "applied", now).
		AddRow("yahoo", "increase_rate", `null`, "applied", now)

	mock.ExpectQuery(`SELECT isp, action_taken, action_params, result, created_at`).
		WillReturnRows(rows)

	handler := &throttleAnalyticsHandler{
		db:    db,
		orgID: "org-1",
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	entries := handler.queryRecentDecisions(req)

	require.Len(t, entries, 2)
	// Malformed JSON: all numeric fields default to 0
	assert.Equal(t, "gmail", entries[0].ISP)
	assert.Equal(t, 0.0, entries[0].EffectiveRate)
	assert.Equal(t, 0.0, entries[0].RateAdj)

	// null params: all fields default to 0
	assert.Equal(t, "yahoo", entries[1].ISP)
	assert.Equal(t, 0.0, entries[1].EffectiveRate)

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestJsonFloat_EdgeCases(t *testing.T) {
	assert.Equal(t, 0.0, jsonFloat(nil, "key"))
	assert.Equal(t, 0.0, jsonFloat(map[string]interface{}{}, "missing"))
	assert.Equal(t, 0.0, jsonFloat(map[string]interface{}{"key": "not_a_number"}, "key"))
	assert.Equal(t, 42.5, jsonFloat(map[string]interface{}{"key": 42.5}, "key"))
}

func TestJsonBool_EdgeCases(t *testing.T) {
	assert.False(t, jsonBool(nil, "key"))
	assert.False(t, jsonBool(map[string]interface{}{}, "missing"))
	assert.False(t, jsonBool(map[string]interface{}{"key": "not_a_bool"}, "key"))
	assert.True(t, jsonBool(map[string]interface{}{"key": true}, "key"))
	assert.False(t, jsonBool(map[string]interface{}{"key": false}, "key"))
}
