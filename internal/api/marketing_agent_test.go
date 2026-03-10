package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestAgent(t *testing.T) (*EmailMarketingAgent, sqlmock.Sqlmock) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	agent := &EmailMarketingAgent{db: db, openAIKey: "test-key", model: "gpt-4.1"}
	return agent, mock
}

// ── System prompt tests ──────────────────────────────────────────────────────

func TestBuildAgentSystemPrompt_Empty(t *testing.T) {
	prompt := buildAgentSystemPrompt(nil, nil)
	assert.Contains(t, prompt, "Maven")
	assert.Contains(t, prompt, "email marketing strategist")
	assert.NotContains(t, prompt, "What I Remember")
	assert.NotContains(t, prompt, "Active Domain Strategies")
}

func TestBuildAgentSystemPrompt_WithMemories(t *testing.T) {
	memories := []string{"preference: User prefers 6am MST sends", "decision: Approved 10% daily increase"}
	prompt := buildAgentSystemPrompt(memories, nil)
	assert.Contains(t, prompt, "What I Remember")
	assert.Contains(t, prompt, "6am MST")
	assert.Contains(t, prompt, "10% daily increase")
}

func TestBuildAgentSystemPrompt_WithStrategies(t *testing.T) {
	strategies := []string{"em.quizfiesta.com (warmup): {\"daily_volume_increase_pct\":10}"}
	prompt := buildAgentSystemPrompt(nil, strategies)
	assert.Contains(t, prompt, "Active Domain Strategies")
	assert.Contains(t, prompt, "quizfiesta")
}

// ── Tool definitions tests ───────────────────────────────────────────────────

func TestGetAgentTools_Count(t *testing.T) {
	tools := getAgentTools()
	assert.True(t, len(tools) >= 14, "expected at least 14 tools, got %d", len(tools))

	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Function.Name] = true
	}
	assert.True(t, names["get_isp_health"])
	assert.True(t, names["list_campaigns"])
	assert.True(t, names["get_campaign_details"])
	assert.True(t, names["list_lists"])
	assert.True(t, names["list_templates"])
	assert.True(t, names["read_template"])
	assert.True(t, names["get_engagement_breakdown"])
	assert.True(t, names["get_domain_strategy"])
	assert.True(t, names["create_recommendation"])
	assert.True(t, names["save_domain_strategy"])
	assert.True(t, names["deploy_approved_campaign"])
}

// ── Conversation CRUD handler tests ──────────────────────────────────────────

func TestHandleListConversations_Empty(t *testing.T) {
	agent, mock := newTestAgent(t)
	defer agent.db.Close()

	rows := sqlmock.NewRows([]string{"id", "title", "summary", "message_count", "created_at", "updated_at"})
	mock.ExpectQuery("SELECT id::text").WillReturnRows(rows)

	req := httptest.NewRequest("GET", "/api/mailing/agent/conversations", nil)
	req.Header.Set("X-Organization-ID", "test-org")
	w := httptest.NewRecorder()
	agent.HandleListConversations(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var result []interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	assert.Equal(t, 0, len(result))
}

func TestHandleDeleteConversation_NotFound(t *testing.T) {
	agent, mock := newTestAgent(t)
	defer agent.db.Close()

	mock.ExpectExec("DELETE FROM agent_conversations").
		WithArgs("bad-id", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	r := chi.NewRouter()
	r.Delete("/conversations/{id}", agent.HandleDeleteConversation)

	req := httptest.NewRequest("DELETE", "/conversations/bad-id", nil)
	req.Header.Set("X-Organization-ID", "test-org")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ── Strategy CRUD handler tests ──────────────────────────────────────────────

func TestHandleSaveStrategy_InvalidStrategy(t *testing.T) {
	agent, _ := newTestAgent(t)
	defer agent.db.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"sending_domain": "test.com",
		"strategy":       "invalid",
	})
	req := httptest.NewRequest("POST", "/api/mailing/agent/strategies", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", "test-org")
	w := httptest.NewRecorder()
	agent.HandleSaveStrategy(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	var result map[string]string
	json.Unmarshal(w.Body.Bytes(), &result)
	assert.Contains(t, result["error"], "warmup")
}

func TestHandleSaveStrategy_MissingDomain(t *testing.T) {
	agent, _ := newTestAgent(t)
	defer agent.db.Close()

	body, _ := json.Marshal(map[string]interface{}{"strategy": "warmup"})
	req := httptest.NewRequest("POST", "/api/mailing/agent/strategies", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", "test-org")
	w := httptest.NewRecorder()
	agent.HandleSaveStrategy(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── Calendar & Recommendation handler tests ──────────────────────────────────

func TestHandleApproveRecommendation_NotPending(t *testing.T) {
	agent, mock := newTestAgent(t)
	defer agent.db.Close()

	mock.ExpectQuery("SELECT status FROM agent_campaign_recommendations").
		WithArgs("rec-id", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("approved"))

	r := chi.NewRouter()
	r.Post("/recommendations/{id}/approve", agent.HandleApproveRecommendation)

	req := httptest.NewRequest("POST", "/recommendations/rec-id/approve", nil)
	req.Header.Set("X-Organization-ID", "test-org")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	var result map[string]string
	json.Unmarshal(w.Body.Bytes(), &result)
	assert.Contains(t, result["error"], "pending")
}

// ── Chat handler tests ───────────────────────────────────────────────────────

func TestHandleChat_NoAPIKey(t *testing.T) {
	agent, _ := newTestAgent(t)
	agent.openAIKey = ""
	defer agent.db.Close()

	body, _ := json.Marshal(map[string]string{"message": "hello"})
	req := httptest.NewRequest("POST", "/api/mailing/agent/chat", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	agent.HandleChat(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestHandleChat_EmptyMessage(t *testing.T) {
	agent, _ := newTestAgent(t)
	defer agent.db.Close()

	body, _ := json.Marshal(map[string]string{"message": "   "})
	req := httptest.NewRequest("POST", "/api/mailing/agent/chat", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	agent.HandleChat(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── Forecast generation handler tests ────────────────────────────────────────

func TestHandleGenerateForecast_MissingParams(t *testing.T) {
	agent, _ := newTestAgent(t)
	defer agent.db.Close()

	body, _ := json.Marshal(map[string]string{"month": "2026-03"})
	req := httptest.NewRequest("POST", "/api/mailing/agent/calendar/generate", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", "test-org")
	w := httptest.NewRecorder()
	agent.HandleGenerateForecast(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}
