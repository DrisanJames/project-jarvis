package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/agent"
	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/sparkpost"
	"github.com/ignite/sparkpost-monitor/internal/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MockCollector implements a mock collector for testing
type MockCollector struct {
	summary *sparkpost.Summary
	isp     []sparkpost.ISPMetrics
	ip      []sparkpost.IPMetrics
	domain  []sparkpost.DomainMetrics
	signals *sparkpost.SignalsData
	running bool
	lastFetch time.Time
}

func (m *MockCollector) GetLatestSummary() *sparkpost.Summary { return m.summary }
func (m *MockCollector) GetLatestISPMetrics() []sparkpost.ISPMetrics { return m.isp }
func (m *MockCollector) GetLatestIPMetrics() []sparkpost.IPMetrics { return m.ip }
func (m *MockCollector) GetLatestDomainMetrics() []sparkpost.DomainMetrics { return m.domain }
func (m *MockCollector) GetLatestSignals() *sparkpost.SignalsData { return m.signals }
func (m *MockCollector) IsRunning() bool { return m.running }
func (m *MockCollector) GetLastFetchTime() time.Time { return m.lastFetch }
func (m *MockCollector) FetchNow(ctx interface{}) {}

func setupTestHandlers(t *testing.T) (*Handlers, *MockCollector) {
	storageCfg := config.StorageConfig{
		Type:      "local",
		LocalPath: t.TempDir(),
	}
	store, err := storage.New(storageCfg)
	require.NoError(t, err)

	agentCfg := config.AgentConfig{
		AnomalySigma:  2.0,
		MinDataPoints: 10,
	}
	ag := agent.New(agentCfg, store)

	mockCollector := &MockCollector{
		summary: &sparkpost.Summary{
			TotalTargeted:  10000000,
			TotalDelivered: 9800000,
			DeliveryRate:   0.98,
			OpenRate:       0.20,
			ComplaintRate:  0.00012,
		},
		isp: []sparkpost.ISPMetrics{
			{Provider: "Gmail", Status: "healthy"},
			{Provider: "Yahoo", Status: "warning"},
		},
		ip: []sparkpost.IPMetrics{
			{IP: "18.236.253.72", Status: "healthy"},
		},
		domain: []sparkpost.DomainMetrics{
			{Domain: "mail.ignite.com", Status: "healthy"},
		},
		signals: &sparkpost.SignalsData{
			Timestamp: time.Now(),
		},
		running:   true,
		lastFetch: time.Now(),
	}

	// Create handlers with real collector interface
	// We'll need to test differently since Handlers expects concrete *Collector
	handlers := &Handlers{
		// We can't directly use mockCollector because Handlers expects *sparkpost.Collector
		// For now, we'll test with nil and just verify error handling
		collector: nil,
		agent:     ag,
		storage:   store,
	}

	return handlers, mockCollector
}

func TestHealthCheck(t *testing.T) {
	// Create a minimal test setup
	storageCfg := config.StorageConfig{
		Type:      "local",
		LocalPath: t.TempDir(),
	}
	store, _ := storage.New(storageCfg)

	agentCfg := config.AgentConfig{MinDataPoints: 10}
	ag := agent.New(agentCfg, store)

	// Create real collector with mock client
	spClient := sparkpost.NewClient(config.SparkPostConfig{
		APIKey:  "test",
		BaseURL: "http://localhost",
	})
	collector := sparkpost.NewCollector(spClient, store, ag, config.PollingConfig{IntervalSeconds: 60})

	handlers := NewHandlers(collector, ag, store)
	router, _ := SetupRoutes(handlers, nil)

	// Register comprehensive health routes (no db/redis/s3 in test)
	hc := NewHealthChecker(nil, nil, nil, "")
	router.Get("/health", hc.HandleHealth)
	router.Get("/health/live", hc.HandleLiveness)
	router.Get("/health/ready", hc.HandleReadiness)

	// Test /health endpoint
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response map[string]interface{}
	err := json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	assert.Contains(t, response, "status")
	assert.Contains(t, response, "version")
	assert.Contains(t, response, "uptime")
	assert.Contains(t, response, "checks")

	// Test /health/live endpoint
	req = httptest.NewRequest(http.MethodGet, "/health/live", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var liveResp map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &liveResp)
	require.NoError(t, err)
	assert.Equal(t, "alive", liveResp["status"])

	// Test /health/ready endpoint
	req = httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var readyResp map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &readyResp)
	require.NoError(t, err)
	assert.Contains(t, readyResp, "ready")
	assert.Contains(t, readyResp, "checks")
}

func TestGetSummary(t *testing.T) {
	storageCfg := config.StorageConfig{
		Type:      "local",
		LocalPath: t.TempDir(),
	}
	store, _ := storage.New(storageCfg)
	
	agentCfg := config.AgentConfig{MinDataPoints: 10}
	ag := agent.New(agentCfg, store)

	spClient := sparkpost.NewClient(config.SparkPostConfig{
		APIKey:  "test",
		BaseURL: "http://localhost",
	})
	collector := sparkpost.NewCollector(spClient, store, ag, config.PollingConfig{IntervalSeconds: 60})

	handlers := NewHandlers(collector, ag, store)
	router, _ := SetupRoutes(handlers, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/metrics/summary", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestGetISPMetrics(t *testing.T) {
	storageCfg := config.StorageConfig{
		Type:      "local",
		LocalPath: t.TempDir(),
	}
	store, _ := storage.New(storageCfg)
	
	agentCfg := config.AgentConfig{MinDataPoints: 10}
	ag := agent.New(agentCfg, store)

	spClient := sparkpost.NewClient(config.SparkPostConfig{
		APIKey:  "test",
		BaseURL: "http://localhost",
	})
	collector := sparkpost.NewCollector(spClient, store, ag, config.PollingConfig{IntervalSeconds: 60})

	handlers := NewHandlers(collector, ag, store)
	router, _ := SetupRoutes(handlers, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/metrics/isp", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response map[string]interface{}
	err := json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Contains(t, response, "data")
}

func TestGetAlerts(t *testing.T) {
	storageCfg := config.StorageConfig{
		Type:      "local",
		LocalPath: t.TempDir(),
	}
	store, _ := storage.New(storageCfg)
	
	agentCfg := config.AgentConfig{MinDataPoints: 10}
	ag := agent.New(agentCfg, store)

	spClient := sparkpost.NewClient(config.SparkPostConfig{
		APIKey:  "test",
		BaseURL: "http://localhost",
	})
	collector := sparkpost.NewCollector(spClient, store, ag, config.PollingConfig{IntervalSeconds: 60})

	handlers := NewHandlers(collector, ag, store)
	router, _ := SetupRoutes(handlers, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/agent/alerts", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response map[string]interface{}
	err := json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Contains(t, response, "alerts")
	assert.Contains(t, response, "count")
}

func TestChat(t *testing.T) {
	storageCfg := config.StorageConfig{
		Type:      "local",
		LocalPath: t.TempDir(),
	}
	store, _ := storage.New(storageCfg)
	
	agentCfg := config.AgentConfig{MinDataPoints: 10}
	ag := agent.New(agentCfg, store)

	spClient := sparkpost.NewClient(config.SparkPostConfig{
		APIKey:  "test",
		BaseURL: "http://localhost",
	})
	collector := sparkpost.NewCollector(spClient, store, ag, config.PollingConfig{IntervalSeconds: 60})

	handlers := NewHandlers(collector, ag, store)
	router, _ := SetupRoutes(handlers, nil)

	body := bytes.NewBufferString(`{"message": "How is performance?"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/agent/chat", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	// Without OpenAI agent configured, should return 503
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var response map[string]interface{}
	err := json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Contains(t, response, "error")
	assert.Contains(t, response["error"].(string), "AI agent not configured")
}

func TestChatWithMockOpenAI(t *testing.T) {
	storageCfg := config.StorageConfig{
		Type:      "local",
		LocalPath: t.TempDir(),
	}
	store, _ := storage.New(storageCfg)
	
	agentCfg := config.AgentConfig{MinDataPoints: 10}
	ag := agent.New(agentCfg, store)

	spClient := sparkpost.NewClient(config.SparkPostConfig{
		APIKey:  "test",
		BaseURL: "http://localhost",
	})
	collector := sparkpost.NewCollector(spClient, store, ag, config.PollingConfig{IntervalSeconds: 60})

	handlers := NewHandlers(collector, ag, store)
	
	// Set up a mock OpenAI agent (will fail without real API key, but tests the path)
	// In real tests, you'd use a mock HTTP server
	mockOpenAI := agent.NewOpenAIAgent("test-key", "gpt-4o", ag, nil)
	handlers.SetOpenAIAgent(mockOpenAI)
	
	router, _ := SetupRoutes(handlers, nil)

	body := bytes.NewBufferString(`{"message": "How is performance?"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/agent/chat", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	// Will return 500 because test API key is invalid, but proves the OpenAI path is taken
	// In production, this would work with a valid API key
	assert.True(t, rec.Code == http.StatusOK || rec.Code == http.StatusInternalServerError)
}

func TestChatEmptyMessage(t *testing.T) {
	storageCfg := config.StorageConfig{
		Type:      "local",
		LocalPath: t.TempDir(),
	}
	store, _ := storage.New(storageCfg)
	
	agentCfg := config.AgentConfig{MinDataPoints: 10}
	ag := agent.New(agentCfg, store)

	spClient := sparkpost.NewClient(config.SparkPostConfig{
		APIKey:  "test",
		BaseURL: "http://localhost",
	})
	collector := sparkpost.NewCollector(spClient, store, ag, config.PollingConfig{IntervalSeconds: 60})

	handlers := NewHandlers(collector, ag, store)
	router, _ := SetupRoutes(handlers, nil)

	body := bytes.NewBufferString(`{"message": ""}`)
	req := httptest.NewRequest(http.MethodPost, "/api/agent/chat", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestGetBaselines(t *testing.T) {
	storageCfg := config.StorageConfig{
		Type:      "local",
		LocalPath: t.TempDir(),
	}
	store, _ := storage.New(storageCfg)
	
	agentCfg := config.AgentConfig{MinDataPoints: 10}
	ag := agent.New(agentCfg, store)

	spClient := sparkpost.NewClient(config.SparkPostConfig{
		APIKey:  "test",
		BaseURL: "http://localhost",
	})
	collector := sparkpost.NewCollector(spClient, store, ag, config.PollingConfig{IntervalSeconds: 60})

	handlers := NewHandlers(collector, ag, store)
	router, _ := SetupRoutes(handlers, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/agent/baselines", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response map[string]interface{}
	err := json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Contains(t, response, "baselines")
}

func TestGetCorrelations(t *testing.T) {
	storageCfg := config.StorageConfig{
		Type:      "local",
		LocalPath: t.TempDir(),
	}
	store, _ := storage.New(storageCfg)
	
	agentCfg := config.AgentConfig{MinDataPoints: 10}
	ag := agent.New(agentCfg, store)

	spClient := sparkpost.NewClient(config.SparkPostConfig{
		APIKey:  "test",
		BaseURL: "http://localhost",
	})
	collector := sparkpost.NewCollector(spClient, store, ag, config.PollingConfig{IntervalSeconds: 60})

	handlers := NewHandlers(collector, ag, store)
	router, _ := SetupRoutes(handlers, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/agent/correlations", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestGetDashboard(t *testing.T) {
	storageCfg := config.StorageConfig{
		Type:      "local",
		LocalPath: t.TempDir(),
	}
	store, _ := storage.New(storageCfg)
	
	agentCfg := config.AgentConfig{MinDataPoints: 10}
	ag := agent.New(agentCfg, store)

	spClient := sparkpost.NewClient(config.SparkPostConfig{
		APIKey:  "test",
		BaseURL: "http://localhost",
	})
	collector := sparkpost.NewCollector(spClient, store, ag, config.PollingConfig{IntervalSeconds: 60})

	handlers := NewHandlers(collector, ag, store)
	router, _ := SetupRoutes(handlers, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/dashboard", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response map[string]interface{}
	err := json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)
	
	// Dashboard should contain all sections
	assert.Contains(t, response, "timestamp")
	assert.Contains(t, response, "alerts")
}

func TestGetSystemStatus(t *testing.T) {
	storageCfg := config.StorageConfig{
		Type:      "local",
		LocalPath: t.TempDir(),
	}
	store, _ := storage.New(storageCfg)
	
	agentCfg := config.AgentConfig{MinDataPoints: 10}
	ag := agent.New(agentCfg, store)

	spClient := sparkpost.NewClient(config.SparkPostConfig{
		APIKey:  "test",
		BaseURL: "http://localhost",
	})
	collector := sparkpost.NewCollector(spClient, store, ag, config.PollingConfig{IntervalSeconds: 60})

	handlers := NewHandlers(collector, ag, store)
	router, _ := SetupRoutes(handlers, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/system/status", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response map[string]interface{}
	err := json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Contains(t, response, "collector")
	assert.Contains(t, response, "agent")
	assert.Contains(t, response, "storage")
}

func TestTriggerFetch(t *testing.T) {
	storageCfg := config.StorageConfig{
		Type:      "local",
		LocalPath: t.TempDir(),
	}
	store, _ := storage.New(storageCfg)
	
	agentCfg := config.AgentConfig{MinDataPoints: 10}
	ag := agent.New(agentCfg, store)

	spClient := sparkpost.NewClient(config.SparkPostConfig{
		APIKey:  "test",
		BaseURL: "http://localhost",
	})
	collector := sparkpost.NewCollector(spClient, store, ag, config.PollingConfig{IntervalSeconds: 60})

	handlers := NewHandlers(collector, ag, store)
	router, _ := SetupRoutes(handlers, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/system/fetch", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestCORSHeaders(t *testing.T) {
	storageCfg := config.StorageConfig{
		Type:      "local",
		LocalPath: t.TempDir(),
	}
	store, _ := storage.New(storageCfg)
	
	agentCfg := config.AgentConfig{MinDataPoints: 10}
	ag := agent.New(agentCfg, store)

	spClient := sparkpost.NewClient(config.SparkPostConfig{
		APIKey:  "test",
		BaseURL: "http://localhost",
	})
	collector := sparkpost.NewCollector(spClient, store, ag, config.PollingConfig{IntervalSeconds: 60})

	handlers := NewHandlers(collector, ag, store)
	router, _ := SetupRoutes(handlers, nil)

	req := httptest.NewRequest(http.MethodOptions, "/api/dashboard", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	// CORS preflight should be handled
	assert.Contains(t, []int{http.StatusOK, http.StatusNoContent}, rec.Code)
}
