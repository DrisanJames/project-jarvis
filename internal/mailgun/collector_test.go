package mailgun

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
)

// MockStorage implements StorageInterface for testing
type MockStorage struct {
	mu                  sync.Mutex
	savedMetrics        []ProcessedMetrics
	savedISPMetrics     []ISPMetrics
	savedDomainMetrics  []DomainMetrics
	savedSignals        *SignalsData
	saveMetricsErr      error
	saveISPMetricsErr   error
	saveDomainMetricsErr error
	saveSignalsErr      error
}

func (m *MockStorage) SaveMailgunMetrics(ctx context.Context, metrics []ProcessedMetrics) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveMetricsErr != nil {
		return m.saveMetricsErr
	}
	m.savedMetrics = append(m.savedMetrics, metrics...)
	return nil
}

func (m *MockStorage) SaveMailgunISPMetrics(ctx context.Context, metrics []ISPMetrics) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveISPMetricsErr != nil {
		return m.saveISPMetricsErr
	}
	m.savedISPMetrics = append(m.savedISPMetrics, metrics...)
	return nil
}

func (m *MockStorage) SaveMailgunDomainMetrics(ctx context.Context, metrics []DomainMetrics) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveDomainMetricsErr != nil {
		return m.saveDomainMetricsErr
	}
	m.savedDomainMetrics = append(m.savedDomainMetrics, metrics...)
	return nil
}

func (m *MockStorage) SaveMailgunSignals(ctx context.Context, signals SignalsData) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveSignalsErr != nil {
		return m.saveSignalsErr
	}
	m.savedSignals = &signals
	return nil
}

func (m *MockStorage) GetSavedMetrics() []ProcessedMetrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.savedMetrics
}

func (m *MockStorage) GetSavedISPMetrics() []ISPMetrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.savedISPMetrics
}

func (m *MockStorage) GetSavedDomainMetrics() []DomainMetrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.savedDomainMetrics
}

// MockAgent implements AgentInterface for testing
type MockAgent struct {
	mu                     sync.Mutex
	processedMetrics       []ProcessedMetrics
	processedISPMetrics    []ISPMetrics
	healthStatus           string
	healthReason           string
	processMetricsErr      error
	processISPMetricsErr   error
}

func (m *MockAgent) ProcessMailgunMetrics(ctx context.Context, metrics []ProcessedMetrics) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.processMetricsErr != nil {
		return m.processMetricsErr
	}
	m.processedMetrics = append(m.processedMetrics, metrics...)
	return nil
}

func (m *MockAgent) ProcessMailgunISPMetrics(ctx context.Context, metrics []ISPMetrics) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.processISPMetricsErr != nil {
		return m.processISPMetricsErr
	}
	m.processedISPMetrics = append(m.processedISPMetrics, metrics...)
	return nil
}

func (m *MockAgent) EvaluateMailgunHealth(metrics ProcessedMetrics) (status string, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.healthStatus != "" {
		return m.healthStatus, m.healthReason
	}
	return "healthy", ""
}

func (m *MockAgent) GetProcessedMetrics() []ProcessedMetrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.processedMetrics
}

func TestNewCollector(t *testing.T) {
	cfg := config.MailgunConfig{
		APIKey:         "test-key",
		BaseURL:        "https://api.mailgun.net",
		TimeoutSeconds: 30,
		Domains:        []string{"test.com"},
	}

	client := NewClient(cfg)
	storage := &MockStorage{}
	agent := &MockAgent{}
	pollingCfg := config.PollingConfig{IntervalSeconds: 60}

	collector := NewCollector(client, storage, agent, pollingCfg)

	if collector == nil {
		t.Fatal("NewCollector returned nil")
	}

	if collector.client != client {
		t.Error("Collector client not set correctly")
	}

	if collector.storage != storage {
		t.Error("Collector storage not set correctly")
	}

	if collector.agent != agent {
		t.Error("Collector agent not set correctly")
	}
}

func TestCollector_GetLatestSummary(t *testing.T) {
	collector := &Collector{}

	// Initially nil
	if collector.GetLatestSummary() != nil {
		t.Error("Expected nil summary initially")
	}

	// Set a summary
	summary := &Summary{
		Timestamp:      time.Now(),
		TotalTargeted:  1000,
		TotalDelivered: 950,
	}
	collector.mu.Lock()
	collector.latestSummary = summary
	collector.mu.Unlock()

	result := collector.GetLatestSummary()
	if result == nil {
		t.Fatal("GetLatestSummary returned nil")
	}

	if result.TotalTargeted != 1000 {
		t.Errorf("TotalTargeted = %d, want %d", result.TotalTargeted, 1000)
	}
}

func TestCollector_GetLatestISPMetrics(t *testing.T) {
	collector := &Collector{}

	// Initially nil/empty
	if len(collector.GetLatestISPMetrics()) != 0 {
		t.Error("Expected empty ISP metrics initially")
	}

	// Set ISP metrics
	ispMetrics := []ISPMetrics{
		{Provider: "Gmail", Status: "healthy"},
		{Provider: "Yahoo", Status: "warning"},
	}
	collector.mu.Lock()
	collector.latestISP = ispMetrics
	collector.mu.Unlock()

	result := collector.GetLatestISPMetrics()
	if len(result) != 2 {
		t.Errorf("len(result) = %d, want %d", len(result), 2)
	}

	if result[0].Provider != "Gmail" {
		t.Errorf("result[0].Provider = %q, want %q", result[0].Provider, "Gmail")
	}
}

func TestCollector_GetLatestDomainMetrics(t *testing.T) {
	collector := &Collector{}

	// Initially nil/empty
	if len(collector.GetLatestDomainMetrics()) != 0 {
		t.Error("Expected empty domain metrics initially")
	}

	// Set domain metrics
	domainMetrics := []DomainMetrics{
		{Domain: "test.com", Status: "healthy"},
	}
	collector.mu.Lock()
	collector.latestDomain = domainMetrics
	collector.mu.Unlock()

	result := collector.GetLatestDomainMetrics()
	if len(result) != 1 {
		t.Errorf("len(result) = %d, want %d", len(result), 1)
	}
}

func TestCollector_GetLatestSignals(t *testing.T) {
	collector := &Collector{}

	// Initially nil
	if collector.GetLatestSignals() != nil {
		t.Error("Expected nil signals initially")
	}

	// Set signals
	signals := &SignalsData{
		Timestamp: time.Now(),
		BounceReasons: []BounceClassificationItem{
			{Classification: "hard", Count: 100},
		},
	}
	collector.mu.Lock()
	collector.latestSignals = signals
	collector.mu.Unlock()

	result := collector.GetLatestSignals()
	if result == nil {
		t.Fatal("GetLatestSignals returned nil")
	}

	if len(result.BounceReasons) != 1 {
		t.Errorf("len(BounceReasons) = %d, want %d", len(result.BounceReasons), 1)
	}
}

func TestCollector_IsRunning(t *testing.T) {
	collector := &Collector{}

	if collector.IsRunning() {
		t.Error("Expected IsRunning to be false initially")
	}

	collector.mu.Lock()
	collector.isRunning = true
	collector.mu.Unlock()

	if !collector.IsRunning() {
		t.Error("Expected IsRunning to be true after setting")
	}
}

func TestCollector_GetLastFetchTime(t *testing.T) {
	collector := &Collector{}

	if !collector.GetLastFetchTime().IsZero() {
		t.Error("Expected zero time initially")
	}

	now := time.Now()
	collector.mu.Lock()
	collector.lastFetch = now
	collector.mu.Unlock()

	result := collector.GetLastFetchTime()
	if !result.Equal(now) {
		t.Errorf("GetLastFetchTime = %v, want %v", result, now)
	}
}

func TestCollector_GetClient(t *testing.T) {
	cfg := config.MailgunConfig{
		APIKey:  "test-key",
		BaseURL: "https://api.mailgun.net",
		Domains: []string{"test.com"},
	}

	client := NewClient(cfg)
	collector := &Collector{client: client}

	if collector.GetClient() != client {
		t.Error("GetClient returned wrong client")
	}
}

func TestCollector_FetchNow_Integration(t *testing.T) {
	// Create mock server for all endpoints
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/v3/test.com/stats/total":
			resp := StatsResponse{
				Stats: []StatsItem{
					{
						Time:     time.Now().Format(time.RFC3339),
						Accepted: StatsCounter{Total: 1000},
						Delivered: StatsCounter{Total: 950},
						Opened:   StatsCounter{Total: 200},
						Clicked:  StatsCounter{Total: 50},
						Failed: FailedStats{
							Permanent: FailedDetail{Total: 30, Bounce: 25},
							Temporary: FailedDetail{Total: 20},
						},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)

		case r.URL.Path == "/v3/test.com/aggregates/providers":
			resp := ProviderAggregatesResponse{
				Providers: map[string]ProviderStats{
					"gmail.com": {
						Accepted:  500,
						Delivered: 480,
						Opened:    100,
					},
				},
			}
			json.NewEncoder(w).Encode(resp)

		case r.URL.Path == "/v1/analytics/metrics":
			// Analytics API response with recipient_domain dimension
			resp := MetricsResponse{
				Items: []MetricsItem{
					{
						Dimensions: []Dimension{
							{Dimension: "recipient_domain", Value: "gmail.com", DisplayValue: "gmail.com"},
						},
						Metrics: MetricsData{
							AcceptedOutgoingCount: 500,
							DeliveredSMTPCount:    480,
							OpenedCount:           100,
							ClickedCount:          25,
							BouncedCount:          10,
							ComplainedCount:       1,
						},
					},
					{
						Dimensions: []Dimension{
							{Dimension: "recipient_domain", Value: "yahoo.com", DisplayValue: "yahoo.com"},
						},
						Metrics: MetricsData{
							AcceptedOutgoingCount: 300,
							DeliveredSMTPCount:    290,
							OpenedCount:           60,
							ClickedCount:          15,
							BouncedCount:          5,
							ComplainedCount:       0,
						},
					},
				},
				Resolution: "day",
			}
			json.NewEncoder(w).Encode(resp)

		case r.URL.Path == "/v2/bounce-classification/metrics":
			resp := BounceClassificationResponse{
				Items: []BounceClassificationItem{
					{Classification: "hard", Count: 100, Reason: "Unknown user"},
				},
			}
			json.NewEncoder(w).Encode(resp)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	cfg := config.MailgunConfig{
		APIKey:         "test-key",
		BaseURL:        server.URL,
		TimeoutSeconds: 30,
		Domains:        []string{"test.com"},
	}

	client := NewClient(cfg)
	storage := &MockStorage{}
	agent := &MockAgent{healthStatus: "healthy"}
	pollingCfg := config.PollingConfig{IntervalSeconds: 60}

	collector := NewCollector(client, storage, agent, pollingCfg)

	ctx := context.Background()
	collector.FetchNow(ctx)

	// Verify summary was fetched
	summary := collector.GetLatestSummary()
	if summary == nil {
		t.Fatal("Summary was not fetched")
	}

	if summary.TotalTargeted != 1000 {
		t.Errorf("TotalTargeted = %d, want %d", summary.TotalTargeted, 1000)
	}

	// Verify ISP metrics were fetched
	ispMetrics := collector.GetLatestISPMetrics()
	if len(ispMetrics) == 0 {
		t.Error("ISP metrics were not fetched")
	}

	// Verify domain metrics were fetched
	domainMetrics := collector.GetLatestDomainMetrics()
	if len(domainMetrics) == 0 {
		t.Error("Domain metrics were not fetched")
	}

	// Verify signals were fetched
	signals := collector.GetLatestSignals()
	if signals == nil {
		t.Error("Signals were not fetched")
	}

	// Verify storage was called
	savedMetrics := storage.GetSavedMetrics()
	if len(savedMetrics) == 0 {
		t.Error("Metrics were not saved to storage")
	}

	// Verify agent was called
	processedMetrics := agent.GetProcessedMetrics()
	if len(processedMetrics) == 0 {
		t.Error("Metrics were not processed by agent")
	}

	// Verify last fetch time was updated
	if collector.GetLastFetchTime().IsZero() {
		t.Error("Last fetch time was not updated")
	}
}

func TestCollector_FetchNow_HandlesErrors(t *testing.T) {
	// Create mock server that returns errors for all endpoints
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return error for all requests
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "Internal server error"}`))
	}))
	defer server.Close()

	cfg := config.MailgunConfig{
		APIKey:         "test-key",
		BaseURL:        server.URL,
		TimeoutSeconds: 30,
		Domains:        []string{"test.com"},
	}

	client := NewClient(cfg)
	storage := &MockStorage{}
	agent := &MockAgent{}
	pollingCfg := config.PollingConfig{IntervalSeconds: 60}

	collector := NewCollector(client, storage, agent, pollingCfg)

	ctx := context.Background()

	// Should not panic even with errors
	collector.FetchNow(ctx)

	// Last fetch time should still be updated even with errors
	if collector.GetLastFetchTime().IsZero() {
		t.Error("Last fetch time should be updated even with errors")
	}

	// Note: Summary may or may not be nil depending on how errors are handled
	// The important thing is that the collector doesn't panic and updates last fetch time
}

func TestCollector_FetchNow_NilStorageAndAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := StatsResponse{
			Stats: []StatsItem{
				{
					Accepted: StatsCounter{Total: 100},
					Delivered: StatsCounter{Total: 95},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := config.MailgunConfig{
		APIKey:         "test-key",
		BaseURL:        server.URL,
		TimeoutSeconds: 30,
		Domains:        []string{"test.com"},
	}

	client := NewClient(cfg)
	pollingCfg := config.PollingConfig{IntervalSeconds: 60}

	// Create collector with nil storage and agent
	collector := NewCollector(client, nil, nil, pollingCfg)

	ctx := context.Background()

	// Should not panic with nil storage and agent
	collector.FetchNow(ctx)

	// Summary should still be populated
	summary := collector.GetLatestSummary()
	if summary == nil {
		t.Error("Summary should be fetched even with nil storage/agent")
	}
}

func TestCollector_ConcurrentAccess(t *testing.T) {
	collector := &Collector{}

	// Set initial values
	collector.mu.Lock()
	collector.latestSummary = &Summary{TotalTargeted: 100}
	collector.latestISP = []ISPMetrics{{Provider: "Gmail"}}
	collector.latestDomain = []DomainMetrics{{Domain: "test.com"}}
	collector.latestSignals = &SignalsData{Timestamp: time.Now()}
	collector.mu.Unlock()

	// Concurrent reads
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(4)

		go func() {
			defer wg.Done()
			_ = collector.GetLatestSummary()
		}()

		go func() {
			defer wg.Done()
			_ = collector.GetLatestISPMetrics()
		}()

		go func() {
			defer wg.Done()
			_ = collector.GetLatestDomainMetrics()
		}()

		go func() {
			defer wg.Done()
			_ = collector.GetLatestSignals()
		}()
	}

	wg.Wait()
}

func TestCollector_Start_Stop(t *testing.T) {
	cfg := config.MailgunConfig{
		APIKey:         "test-key",
		BaseURL:        "https://api.mailgun.net",
		TimeoutSeconds: 30,
		Domains:        []string{"test.com"},
	}

	client := NewClient(cfg)
	pollingCfg := config.PollingConfig{IntervalSeconds: 1}

	collector := NewCollector(client, nil, nil, pollingCfg)

	ctx, cancel := context.WithCancel(context.Background())

	// Start in goroutine
	go collector.Start(ctx)

	// Give it time to start
	time.Sleep(100 * time.Millisecond)

	if !collector.IsRunning() {
		t.Error("Expected collector to be running")
	}

	// Stop
	cancel()

	// Give it time to stop
	time.Sleep(100 * time.Millisecond)

	if collector.IsRunning() {
		t.Error("Expected collector to be stopped")
	}
}
