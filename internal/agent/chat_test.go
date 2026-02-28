package agent

import (
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/sparkpost"
	"github.com/ignite/sparkpost-monitor/internal/storage"
	"github.com/stretchr/testify/assert"
)

func createTestData() (*sparkpost.Summary, []sparkpost.ISPMetrics) {
	summary := &sparkpost.Summary{
		TotalTargeted:    10000000,
		TotalDelivered:   9800000,
		DeliveryRate:     0.98,
		OpenRate:         0.20,
		ClickRate:        0.028,
		ComplaintRate:    0.00012,
		BounceRate:       0.02,
		UnsubscribeRate:  0.001,
	}

	ispMetrics := []sparkpost.ISPMetrics{
		{
			Provider: "Gmail",
			Metrics: sparkpost.ProcessedMetrics{
				Targeted:      5000000,
				Delivered:     4950000,
				DeliveryRate:  0.99,
				OpenRate:      0.22,
				ComplaintRate: 0.0001,
			},
			Status: "healthy",
		},
		{
			Provider: "Yahoo",
			Metrics: sparkpost.ProcessedMetrics{
				Targeted:      3000000,
				Delivered:     2900000,
				DeliveryRate:  0.967,
				OpenRate:      0.15,
				ComplaintRate: 0.00045,
			},
			Status:       "warning",
			StatusReason: "Complaint rate approaching threshold",
		},
		{
			Provider: "Hotmail",
			Metrics: sparkpost.ProcessedMetrics{
				Targeted:      2000000,
				Delivered:     1950000,
				DeliveryRate:  0.975,
				OpenRate:      0.18,
				ComplaintRate: 0.0002,
			},
			Status: "healthy",
		},
	}

	return summary, ispMetrics
}

func TestChat_PerformanceQuery(t *testing.T) {
	agent := newTestAgent()
	summary, ispMetrics := createTestData()

	tests := []struct {
		query    string
		contains string
	}{
		{"How is performance?", "Overall Performance"},
		{"how is gmail doing?", "Gmail Performance"},
		{"what's the status?", "Status"},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			response := agent.Chat(tt.query, summary, ispMetrics)
			assert.Contains(t, response.Message, tt.contains)
			assert.NotEmpty(t, response.Suggestions)
		})
	}
}

func TestChat_RecommendationQuery(t *testing.T) {
	agent := newTestAgent()
	summary, ispMetrics := createTestData()

	queries := []string{
		"Give me recommendations",
		"What do you recommend?",
		"Should I do something?",
	}

	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			response := agent.Chat(query, summary, ispMetrics)
			assert.Contains(t, response.Message, "Recommendation")
		})
	}
}

func TestChat_ConcernsQuery(t *testing.T) {
	agent := newTestAgent()
	summary, ispMetrics := createTestData()

	response := agent.Chat("What are the concerns?", summary, ispMetrics)
	assert.Contains(t, response.Message, "Attention")
	
	// Should mention Yahoo which has warning status
	assert.Contains(t, response.Message, "Yahoo")
}

func TestChat_VolumeQuery(t *testing.T) {
	agent := newTestAgent()
	summary, ispMetrics := createTestData()

	tests := []struct {
		query    string
		contains string
	}{
		{"Can I increase volume?", "Volume"},
		{"Can I send more to gmail?", "Gmail"},
		{"What about volume?", "Volume"},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			response := agent.Chat(tt.query, summary, ispMetrics)
			assert.Contains(t, response.Message, tt.contains)
		})
	}
}

func TestChat_BaselineQuery(t *testing.T) {
	agent := newTestAgent()
	
	// Add some baselines
	agent.baselines["isp:Gmail"] = &storage.Baseline{
		EntityType: "isp",
		EntityName: "Gmail",
		Metrics: map[string]*storage.MetricBaseline{
			"complaint_rate": {Mean: 0.0001, StdDev: 0.00002},
		},
		DataPoints: 100,
	}

	response := agent.Chat("What are the learned baselines?", nil, nil)
	assert.Contains(t, response.Message, "Baseline")
	assert.Contains(t, response.Message, "Gmail")
}

func TestChat_CorrelationQuery(t *testing.T) {
	agent := newTestAgent()
	
	// Add a correlation
	agent.correlations = []storage.Correlation{
		{
			EntityName:       "Yahoo",
			TriggerMetric:    "volume",
			TriggerThreshold: 3000000,
			EffectMetric:     "complaint_rate",
			EffectChange:     0.45,
			Confidence:       0.82,
			Occurrences:      15,
		},
	}

	response := agent.Chat("Show me correlations", nil, nil)
	assert.Contains(t, response.Message, "Correlation")
	assert.Contains(t, response.Message, "Yahoo")
	assert.Contains(t, response.Message, "volume")
}

func TestChat_ForecastQuery(t *testing.T) {
	agent := newTestAgent()
	summary, ispMetrics := createTestData()

	response := agent.Chat("What do you predict for tomorrow?", summary, ispMetrics)
	assert.Contains(t, response.Message, "Forecast")
}

func TestChat_GeneralQuery(t *testing.T) {
	agent := newTestAgent()
	summary, ispMetrics := createTestData()

	response := agent.Chat("hello", summary, ispMetrics)
	assert.Contains(t, response.Message, "SparkPost Agent")
	assert.Contains(t, response.Message, "I can help with")
}

func TestChat_NilSummary(t *testing.T) {
	agent := newTestAgent()

	response := agent.Chat("How is performance?", nil, nil)
	assert.Contains(t, response.Message, "don't have current metrics")
}

func TestFormatNumber(t *testing.T) {
	tests := []struct {
		input    int64
		expected string
	}{
		{500, "500"},
		{1500, "1.5K"},
		{1500000, "1.5M"},
		{10000000, "10.0M"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := formatNumber(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestContainsAny(t *testing.T) {
	tests := []struct {
		s        string
		substrs  []string
		expected bool
	}{
		{"how is performance", []string{"how", "what"}, true},
		{"display the data", []string{"how", "what"}, false},
		{"WHAT is happening", []string{"how", "what"}, false}, // case sensitive
	}

	for _, tt := range tests {
		t.Run(tt.s, func(t *testing.T) {
			result := containsAny(tt.s, tt.substrs)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFormatISPPerformance(t *testing.T) {
	cfg := config.AgentConfig{MinDataPoints: 10}
	agent := New(cfg, nil)
	
	isp := sparkpost.ISPMetrics{
		Provider: "Gmail",
		Metrics: sparkpost.ProcessedMetrics{
			Timestamp:     time.Now(),
			Targeted:      5000000,
			Delivered:     4950000,
			DeliveryRate:  0.99,
			OpenRate:      0.22,
			ClickRate:     0.03,
			ComplaintRate: 0.0001,
			BounceRate:    0.01,
		},
		Status: "healthy",
	}

	response := agent.formatISPPerformance(isp)
	
	assert.Contains(t, response.Message, "Gmail Performance")
	assert.Contains(t, response.Message, "5.0M")
	assert.Contains(t, response.Message, "HEALTHY")
}

func TestGetVolumeAdvice_WithCorrelation(t *testing.T) {
	agent := newTestAgent()
	
	// Add correlation
	agent.correlations = []storage.Correlation{
		{
			EntityName:       "Gmail",
			TriggerMetric:    "volume",
			TriggerThreshold: 6000000,
			EffectMetric:     "complaint_rate",
			EffectChange:     0.35,
			Confidence:       0.85,
		},
	}

	isp := sparkpost.ISPMetrics{
		Provider: "Gmail",
		Metrics: sparkpost.ProcessedMetrics{
			Targeted: 4000000, // Below threshold
		},
		Status: "healthy",
	}

	response := agent.getVolumeAdvice(isp)
	assert.Contains(t, response.Message, "Threshold")
	assert.Contains(t, response.Message, "Safe to increase")
}

func TestGetISPRecommendation_Critical(t *testing.T) {
	agent := newTestAgent()

	isp := sparkpost.ISPMetrics{
		Provider:     "Yahoo",
		Status:       "critical",
		StatusReason: "Complaint rate exceeds threshold",
		Metrics: sparkpost.ProcessedMetrics{
			ComplaintRate: 0.0006,
		},
	}

	response := agent.getISPRecommendation(isp)
	assert.Contains(t, response.Message, "CRITICAL")
	assert.Contains(t, response.Message, "reducing volume")
}

func TestGetISPRecommendation_Healthy(t *testing.T) {
	agent := newTestAgent()
	
	// Add baseline
	agent.baselines["isp:Gmail"] = &storage.Baseline{
		EntityType: "isp",
		EntityName: "Gmail",
		Metrics: map[string]*storage.MetricBaseline{
			"complaint_rate": {Mean: 0.0001, StdDev: 0.00003},
		},
		DataPoints: 100,
	}

	isp := sparkpost.ISPMetrics{
		Provider: "Gmail",
		Status:   "healthy",
		Metrics: sparkpost.ProcessedMetrics{
			ComplaintRate: 0.00008, // Below mean
		},
	}

	response := agent.getISPRecommendation(isp)
	assert.Contains(t, response.Message, "HEALTHY")
}
