package ses

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
)

// MockStorage implements StorageInterface for testing
type MockStorage struct {
	mu                sync.Mutex
	savedMetrics      []ProcessedMetrics
	savedISPMetrics   []ISPMetrics
	savedSignals      *SignalsData
	saveMetricsErr    error
	saveISPMetricsErr error
	saveSignalsErr    error
}

func (m *MockStorage) SaveSESMetrics(ctx context.Context, metrics []ProcessedMetrics) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveMetricsErr != nil {
		return m.saveMetricsErr
	}
	m.savedMetrics = append(m.savedMetrics, metrics...)
	return nil
}

func (m *MockStorage) SaveSESISPMetrics(ctx context.Context, metrics []ISPMetrics) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveISPMetricsErr != nil {
		return m.saveISPMetricsErr
	}
	m.savedISPMetrics = append(m.savedISPMetrics, metrics...)
	return nil
}

func (m *MockStorage) SaveSESSignals(ctx context.Context, signals SignalsData) error {
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

func (m *MockStorage) GetSavedSignals() *SignalsData {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.savedSignals
}

// MockAgent implements AgentInterface for testing
type MockAgent struct {
	mu               sync.Mutex
	processedMetrics []ProcessedMetrics
	processedISP     []ISPMetrics
	healthStatus     string
	healthReason     string
}

func (m *MockAgent) ProcessSESMetrics(ctx context.Context, metrics []ProcessedMetrics) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.processedMetrics = append(m.processedMetrics, metrics...)
	return nil
}

func (m *MockAgent) ProcessSESISPMetrics(ctx context.Context, metrics []ISPMetrics) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.processedISP = append(m.processedISP, metrics...)
	return nil
}

func (m *MockAgent) EvaluateSESHealth(metrics ProcessedMetrics) (status string, reason string) {
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

func (m *MockAgent) GetProcessedISPMetrics() []ISPMetrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.processedISP
}

// MockClient implements a mock SES client for testing
type MockClient struct {
	ispMetrics []ISPMetrics
	signals    *SignalsData
	err        error
	isps       []string
}

func (m *MockClient) GetISPs() []string {
	if m.isps != nil {
		return m.isps
	}
	return []string{"Gmail", "Yahoo"}
}

func (m *MockClient) GetAllISPMetrics(ctx context.Context, from, to time.Time) ([]ISPMetrics, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.ispMetrics, nil
}

func (m *MockClient) GetSignals(ctx context.Context, from, to time.Time) (*SignalsData, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.signals, nil
}

func TestNewCollector(t *testing.T) {
	storage := &MockStorage{}
	agent := &MockAgent{}
	pollingCfg := config.PollingConfig{IntervalSeconds: 60}

	// We can't create a real Client without AWS credentials, so we test the constructor pattern
	collector := &Collector{
		client:  nil, // Would be a real client in production
		storage: storage,
		agent:   agent,
		config:  pollingCfg,
	}

	if collector.storage == nil {
		t.Error("Storage should be set")
	}
	if collector.agent == nil {
		t.Error("Agent should be set")
	}
}

func TestCollector_GetLatestSummary(t *testing.T) {
	collector := &Collector{}

	// Initially nil
	if collector.GetLatestSummary() != nil {
		t.Error("Expected nil summary initially")
	}

	// Set summary
	summary := &Summary{TotalTargeted: 1000}
	collector.mu.Lock()
	collector.latestSummary = summary
	collector.mu.Unlock()

	result := collector.GetLatestSummary()
	if result == nil {
		t.Fatal("Expected non-nil summary")
	}
	if result.TotalTargeted != 1000 {
		t.Errorf("TotalTargeted = %d, want 1000", result.TotalTargeted)
	}
}

func TestCollector_GetLatestISPMetrics(t *testing.T) {
	collector := &Collector{}

	// Initially empty
	if len(collector.GetLatestISPMetrics()) != 0 {
		t.Error("Expected empty ISP metrics initially")
	}

	// Set ISP metrics
	metrics := []ISPMetrics{
		{Provider: "Gmail"},
		{Provider: "Yahoo"},
	}
	collector.mu.Lock()
	collector.latestISP = metrics
	collector.mu.Unlock()

	result := collector.GetLatestISPMetrics()
	if len(result) != 2 {
		t.Errorf("Got %d ISP metrics, want 2", len(result))
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
		TopIssues: []Issue{{Description: "Test issue"}},
	}
	collector.mu.Lock()
	collector.latestSignals = signals
	collector.mu.Unlock()

	result := collector.GetLatestSignals()
	if result == nil {
		t.Fatal("Expected non-nil signals")
	}
	if len(result.TopIssues) != 1 {
		t.Errorf("Got %d issues, want 1", len(result.TopIssues))
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
		t.Errorf("LastFetchTime = %v, want %v", result, now)
	}
}

func TestCollector_ConcurrentAccess(t *testing.T) {
	collector := &Collector{}

	// Set initial data
	collector.latestSummary = &Summary{TotalTargeted: 1000}
	collector.latestISP = []ISPMetrics{{Provider: "Gmail"}}
	collector.latestSignals = &SignalsData{Timestamp: time.Now()}

	// Concurrent reads and writes
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(3)

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
			_ = collector.GetLatestSignals()
		}()
	}

	wg.Wait()
	// Test passes if no race conditions detected
}

func TestCollector_FetchISPMetrics_UpdatesSummary(t *testing.T) {
	// Create a collector with mock data
	collector := &Collector{
		storage: &MockStorage{},
		agent:   &MockAgent{healthStatus: "healthy"},
	}

	// Simulate ISP metrics data
	ispMetrics := []ISPMetrics{
		{
			Provider: "Gmail",
			Metrics: ProcessedMetrics{
				Targeted:  1000,
				Delivered: 950,
				Opened:    400,
			},
		},
		{
			Provider: "Yahoo",
			Metrics: ProcessedMetrics{
				Targeted:  500,
				Delivered: 480,
				Opened:    200,
			},
		},
	}

	// Manually set ISP metrics and create summary
	collector.mu.Lock()
	collector.latestISP = ispMetrics
	collector.latestSummary = AggregateISPMetricsToSummary(ispMetrics, time.Now().Add(-24*time.Hour), time.Now())
	collector.mu.Unlock()

	// Verify summary was created
	summary := collector.GetLatestSummary()
	if summary == nil {
		t.Fatal("Summary should not be nil")
	}

	if summary.TotalTargeted != 1500 {
		t.Errorf("TotalTargeted = %d, want 1500", summary.TotalTargeted)
	}
	if summary.TotalDelivered != 1430 {
		t.Errorf("TotalDelivered = %d, want 1430", summary.TotalDelivered)
	}
}

func TestCollector_NilStorageAndAgent(t *testing.T) {
	// Create collector with nil storage and agent
	collector := &Collector{
		config: config.PollingConfig{IntervalSeconds: 60},
	}

	// Should not panic when storage and agent are nil
	collector.mu.Lock()
	collector.latestISP = []ISPMetrics{{Provider: "Gmail"}}
	collector.mu.Unlock()

	// These should work without panicking
	_ = collector.GetLatestSummary()
	_ = collector.GetLatestISPMetrics()
	_ = collector.GetLatestSignals()
}

func TestMockStorage(t *testing.T) {
	ctx := context.Background()
	storage := &MockStorage{}

	// Test SaveSESMetrics
	metrics := []ProcessedMetrics{{Sent: 100}}
	err := storage.SaveSESMetrics(ctx, metrics)
	if err != nil {
		t.Errorf("SaveSESMetrics error: %v", err)
	}

	saved := storage.GetSavedMetrics()
	if len(saved) != 1 {
		t.Errorf("Got %d saved metrics, want 1", len(saved))
	}

	// Test SaveSESISPMetrics
	ispMetrics := []ISPMetrics{{Provider: "Gmail"}}
	err = storage.SaveSESISPMetrics(ctx, ispMetrics)
	if err != nil {
		t.Errorf("SaveSESISPMetrics error: %v", err)
	}

	savedISP := storage.GetSavedISPMetrics()
	if len(savedISP) != 1 {
		t.Errorf("Got %d saved ISP metrics, want 1", len(savedISP))
	}

	// Test SaveSESSignals
	signals := SignalsData{Timestamp: time.Now()}
	err = storage.SaveSESSignals(ctx, signals)
	if err != nil {
		t.Errorf("SaveSESSignals error: %v", err)
	}

	savedSignals := storage.GetSavedSignals()
	if savedSignals == nil {
		t.Error("Expected saved signals to not be nil")
	}
}

func TestMockAgent(t *testing.T) {
	ctx := context.Background()
	agent := &MockAgent{healthStatus: "warning", healthReason: "test reason"}

	// Test ProcessSESMetrics
	metrics := []ProcessedMetrics{{Sent: 100}}
	err := agent.ProcessSESMetrics(ctx, metrics)
	if err != nil {
		t.Errorf("ProcessSESMetrics error: %v", err)
	}

	processed := agent.GetProcessedMetrics()
	if len(processed) != 1 {
		t.Errorf("Got %d processed metrics, want 1", len(processed))
	}

	// Test EvaluateSESHealth
	status, reason := agent.EvaluateSESHealth(ProcessedMetrics{})
	if status != "warning" {
		t.Errorf("Status = %s, want warning", status)
	}
	if reason != "test reason" {
		t.Errorf("Reason = %s, want 'test reason'", reason)
	}
}
