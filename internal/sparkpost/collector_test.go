package sparkpost

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MockStorage implements StorageInterface for testing
type MockStorage struct {
	mu              sync.Mutex
	savedMetrics    []ProcessedMetrics
	savedISP        []ISPMetrics
	savedIP         []IPMetrics
	savedDomain     []DomainMetrics
	savedSignals    *SignalsData
	savedTimeSeries []TimeSeries
}

func (m *MockStorage) SaveMetrics(ctx context.Context, metrics []ProcessedMetrics) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.savedMetrics = append(m.savedMetrics, metrics...)
	return nil
}

func (m *MockStorage) SaveISPMetrics(ctx context.Context, metrics []ISPMetrics) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.savedISP = metrics
	return nil
}

func (m *MockStorage) SaveIPMetrics(ctx context.Context, metrics []IPMetrics) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.savedIP = metrics
	return nil
}

func (m *MockStorage) SaveDomainMetrics(ctx context.Context, metrics []DomainMetrics) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.savedDomain = metrics
	return nil
}

func (m *MockStorage) SaveSignals(ctx context.Context, signals SignalsData) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.savedSignals = &signals
	return nil
}

func (m *MockStorage) SaveTimeSeries(ctx context.Context, series []TimeSeries) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.savedTimeSeries = series
	return nil
}

// MockAgent implements AgentInterface for testing
type MockAgent struct {
	processedMetrics    []ProcessedMetrics
	processedISPMetrics []ISPMetrics
}

func (m *MockAgent) ProcessMetrics(ctx context.Context, metrics []ProcessedMetrics) error {
	m.processedMetrics = append(m.processedMetrics, metrics...)
	return nil
}

func (m *MockAgent) ProcessISPMetrics(ctx context.Context, metrics []ISPMetrics) error {
	m.processedISPMetrics = append(m.processedISPMetrics, metrics...)
	return nil
}

func (m *MockAgent) EvaluateHealth(metrics ProcessedMetrics) (status string, reason string) {
	if metrics.ComplaintRate > 0.0005 {
		return "critical", "Complaint rate exceeds threshold"
	}
	if metrics.ComplaintRate > 0.0003 {
		return "warning", "Complaint rate approaching threshold"
	}
	if metrics.BounceRate > 0.05 {
		return "critical", "Bounce rate exceeds threshold"
	}
	return "healthy", ""
}

func TestNewCollector(t *testing.T) {
	cfg := config.SparkPostConfig{
		APIKey:  "test-key",
		BaseURL: "https://api.sparkpost.com/api/v1",
	}
	client := NewClient(cfg)
	storage := &MockStorage{}
	agent := &MockAgent{}
	pollingCfg := config.PollingConfig{
		IntervalSeconds: 60,
	}

	collector := NewCollector(client, storage, agent, pollingCfg)

	require.NotNil(t, collector)
	assert.NotNil(t, collector.client)
	assert.NotNil(t, collector.storage)
	assert.NotNil(t, collector.agent)
}

func TestCollector_AnalyzeIssues(t *testing.T) {
	collector := &Collector{}

	signals := SignalsData{
		BounceReasons: []BounceReasonResult{
			{
				Reason:             "550 User unknown",
				BounceCategoryName: "Hard",
				CountBounce:        15000,
			},
			{
				Reason:             "421 Try again later",
				BounceCategoryName: "Soft",
				CountBounce:        5000,
			},
			{
				Reason:             "550 IP blocked",
				BounceCategoryName: "Block",
				CountBounce:        2000,
			},
		},
		DelayReasons: []DelayReasonResult{
			{
				Reason:       "451 Try again later",
				CountDelayed: 8000,
			},
		},
	}

	issues := collector.analyzeIssues(signals)

	require.GreaterOrEqual(t, len(issues), 3)
	
	// Should be sorted by count (highest first)
	assert.Equal(t, int64(15000), issues[0].Count)
	assert.Equal(t, "critical", issues[0].Severity)
	assert.Equal(t, "bounce", issues[0].Category)
}

func TestCollector_GetBounceRecommendation(t *testing.T) {
	collector := &Collector{}

	tests := []struct {
		bounce   BounceReasonResult
		expected string
	}{
		{
			bounce:   BounceReasonResult{BounceCategoryName: "Hard"},
			expected: "Review list hygiene - remove invalid addresses",
		},
		{
			bounce:   BounceReasonResult{BounceCategoryName: "Soft"},
			expected: "Monitor and retry - temporary issue",
		},
		{
			bounce:   BounceReasonResult{BounceCategoryName: "Block"},
			expected: "Check IP reputation and blocklist status",
		},
		{
			bounce:   BounceReasonResult{BounceCategoryName: "Unknown"},
			expected: "Investigate bounce reason and take appropriate action",
		},
	}

	for _, tt := range tests {
		t.Run(tt.bounce.BounceCategoryName, func(t *testing.T) {
			result := collector.getBounceRecommendation(tt.bounce)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestCollector_GetLatestMethods(t *testing.T) {
	collector := &Collector{
		latestSummary: &Summary{
			TotalTargeted: 1000000,
		},
		latestISP: []ISPMetrics{
			{Provider: "Gmail"},
		},
		latestIP: []IPMetrics{
			{IP: "18.236.253.72"},
		},
		latestDomain: []DomainMetrics{
			{Domain: "example.com"},
		},
		latestSignals: &SignalsData{
			Timestamp: time.Now(),
		},
		lastFetch: time.Now(),
		isRunning: true,
	}

	summary := collector.GetLatestSummary()
	require.NotNil(t, summary)
	assert.Equal(t, int64(1000000), summary.TotalTargeted)

	ispMetrics := collector.GetLatestISPMetrics()
	require.Len(t, ispMetrics, 1)
	assert.Equal(t, "Gmail", ispMetrics[0].Provider)

	ipMetrics := collector.GetLatestIPMetrics()
	require.Len(t, ipMetrics, 1)
	assert.Equal(t, "18.236.253.72", ipMetrics[0].IP)

	domainMetrics := collector.GetLatestDomainMetrics()
	require.Len(t, domainMetrics, 1)
	assert.Equal(t, "example.com", domainMetrics[0].Domain)

	signals := collector.GetLatestSignals()
	require.NotNil(t, signals)

	assert.True(t, collector.IsRunning())
	assert.False(t, collector.GetLastFetchTime().IsZero())
}

func TestCollector_ConcurrentAccess(t *testing.T) {
	collector := &Collector{
		latestSummary: &Summary{
			TotalTargeted: 1000000,
		},
		latestISP: []ISPMetrics{
			{Provider: "Gmail"},
		},
	}

	var wg sync.WaitGroup
	
	// Simulate concurrent reads
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = collector.GetLatestSummary()
			_ = collector.GetLatestISPMetrics()
		}()
	}

	// Simulate concurrent write
	wg.Add(1)
	go func() {
		defer wg.Done()
		collector.mu.Lock()
		collector.latestSummary = &Summary{TotalTargeted: 2000000}
		collector.mu.Unlock()
	}()

	wg.Wait()
}

func TestMockAgent_EvaluateHealth(t *testing.T) {
	agent := &MockAgent{}

	tests := []struct {
		name           string
		complaintRate  float64
		bounceRate     float64
		expectedStatus string
	}{
		{
			name:           "healthy",
			complaintRate:  0.0001,
			bounceRate:     0.02,
			expectedStatus: "healthy",
		},
		{
			name:           "warning - complaint",
			complaintRate:  0.0004,
			bounceRate:     0.02,
			expectedStatus: "warning",
		},
		{
			name:           "critical - complaint",
			complaintRate:  0.0006,
			bounceRate:     0.02,
			expectedStatus: "critical",
		},
		{
			name:           "critical - bounce",
			complaintRate:  0.0001,
			bounceRate:     0.06,
			expectedStatus: "critical",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metrics := ProcessedMetrics{
				ComplaintRate: tt.complaintRate,
				BounceRate:    tt.bounceRate,
			}
			status, _ := agent.EvaluateHealth(metrics)
			assert.Equal(t, tt.expectedStatus, status)
		})
	}
}
