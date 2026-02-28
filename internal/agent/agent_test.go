package agent

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/sparkpost"
	"github.com/ignite/sparkpost-monitor/internal/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestAgent() *Agent {
	cfg := config.AgentConfig{
		BaselineRecalcHours:    24,
		CorrelationRecalcHours: 168,
		AnomalySigma:           2.0,
		MinDataPoints:          10, // Lower for testing
	}
	return New(cfg, nil)
}

func TestNew(t *testing.T) {
	cfg := config.AgentConfig{
		AnomalySigma:  2.0,
		MinDataPoints: 100,
	}
	
	agent := New(cfg, nil)
	
	require.NotNil(t, agent)
	assert.NotNil(t, agent.baselines)
	assert.NotNil(t, agent.correlations)
	assert.NotNil(t, agent.alerts)
	assert.NotNil(t, agent.rollingStats)
}

func TestEvaluateHealth_Defaults(t *testing.T) {
	agent := newTestAgent()

	tests := []struct {
		name           string
		metrics        sparkpost.ProcessedMetrics
		expectedStatus string
	}{
		{
			name: "healthy",
			metrics: sparkpost.ProcessedMetrics{
				ComplaintRate: 0.0001,
				BounceRate:    0.01,
				BlockRate:     0.001,
			},
			expectedStatus: "healthy",
		},
		{
			name: "warning - complaint",
			metrics: sparkpost.ProcessedMetrics{
				ComplaintRate: 0.00035,
				BounceRate:    0.01,
				BlockRate:     0.001,
			},
			expectedStatus: "warning",
		},
		{
			name: "critical - complaint",
			metrics: sparkpost.ProcessedMetrics{
				ComplaintRate: 0.0006,
				BounceRate:    0.01,
				BlockRate:     0.001,
			},
			expectedStatus: "critical",
		},
		{
			name: "critical - bounce",
			metrics: sparkpost.ProcessedMetrics{
				ComplaintRate: 0.0001,
				BounceRate:    0.06,
				BlockRate:     0.001,
			},
			expectedStatus: "critical",
		},
		{
			name: "critical - block",
			metrics: sparkpost.ProcessedMetrics{
				ComplaintRate: 0.0001,
				BounceRate:    0.01,
				BlockRate:     0.06, // 6% - exceeds new 5% critical threshold
			},
			expectedStatus: "critical",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, _ := agent.EvaluateHealth(tt.metrics)
			assert.Equal(t, tt.expectedStatus, status)
		})
	}
}

func TestEvaluateHealth_WithBaseline(t *testing.T) {
	agent := newTestAgent()

	// Set up a learned baseline
	baseline := &storage.Baseline{
		EntityType: "isp",
		EntityName: "Gmail",
		Metrics: map[string]*storage.MetricBaseline{
			"complaint_rate": {
				Mean:   0.00012,
				StdDev: 0.00004,
			},
			"bounce_rate": {
				Mean:   0.015,
				StdDev: 0.005,
			},
		},
		DataPoints: 100,
	}
	agent.baselines["isp:Gmail"] = baseline

	// Test within normal range
	metrics := sparkpost.ProcessedMetrics{
		GroupBy:       "isp",
		GroupValue:    "Gmail",
		ComplaintRate: 0.00014, // Within 1σ
		BounceRate:    0.016,   // Within 1σ
	}

	status, reason := agent.EvaluateHealth(metrics)
	assert.Equal(t, "healthy", status)
	assert.Empty(t, reason)

	// Test anomaly
	metrics.ComplaintRate = 0.00025 // > 2σ above mean
	status, reason = agent.EvaluateHealth(metrics)
	assert.NotEqual(t, "healthy", status)
	assert.NotEmpty(t, reason)
}

func TestProcessMetrics(t *testing.T) {
	agent := newTestAgent()
	ctx := context.Background()

	metrics := []sparkpost.ProcessedMetrics{
		{
			Timestamp:     time.Now(),
			GroupBy:       "summary",
			GroupValue:    "all",
			Targeted:      1000000,
			ComplaintRate: 0.0001,
			BounceRate:    0.01,
			DeliveryRate:  0.98,
		},
	}

	err := agent.ProcessMetrics(ctx, metrics)
	require.NoError(t, err)

	// Check rolling stats were updated
	assert.NotEmpty(t, agent.rollingStats)
}

func TestProcessISPMetrics(t *testing.T) {
	agent := newTestAgent()
	ctx := context.Background()

	metrics := []sparkpost.ISPMetrics{
		{
			Provider: "Gmail",
			Metrics: sparkpost.ProcessedMetrics{
				Timestamp:     time.Now(),
				Targeted:      500000,
				ComplaintRate: 0.0001,
			},
		},
		{
			Provider: "Yahoo",
			Metrics: sparkpost.ProcessedMetrics{
				Timestamp:     time.Now(),
				Targeted:      300000,
				ComplaintRate: 0.0002,
			},
		},
	}

	err := agent.ProcessISPMetrics(ctx, metrics)
	require.NoError(t, err)
}

func TestCalculateMetricBaseline(t *testing.T) {
	values := []float64{
		0.0001, 0.00012, 0.00011, 0.00013, 0.0001,
		0.00015, 0.00009, 0.00011, 0.00012, 0.00014,
	}

	baseline := calculateMetricBaseline(values)

	require.NotNil(t, baseline)
	assert.InDelta(t, 0.000117, baseline.Mean, 0.00001)
	assert.Greater(t, baseline.StdDev, 0.0)
	assert.Equal(t, 0.00009, baseline.Min)
	assert.Equal(t, 0.00015, baseline.Max)
	assert.GreaterOrEqual(t, baseline.Percentile95, baseline.Percentile50)
}

func TestCalculateMetricBaseline_Empty(t *testing.T) {
	baseline := calculateMetricBaseline([]float64{})
	assert.Nil(t, baseline)
}

func TestAnalyzeCorrelation(t *testing.T) {
	// Strong positive correlation
	x := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	y := []float64{2, 4, 6, 8, 10, 12, 14, 16, 18, 20}

	result := analyzeCorrelation(x, y)
	assert.InDelta(t, 1.0, result.Correlation, 0.001) // Perfect positive correlation

	// No correlation (random)
	y2 := []float64{5, 2, 8, 1, 9, 3, 7, 4, 6, 10}
	result2 := analyzeCorrelation(x, y2)
	assert.Less(t, math.Abs(result2.Correlation), 0.5) // Low correlation
}

func TestAnalyzeCorrelation_DifferentLengths(t *testing.T) {
	x := []float64{1, 2, 3}
	y := []float64{1, 2}

	result := analyzeCorrelation(x, y)
	assert.Equal(t, 0.0, result.Correlation)
}

func TestGetRecommendation(t *testing.T) {
	agent := newTestAgent()

	tests := []struct {
		metricName string
		deviation  float64
		contains   string
	}{
		{"complaint_rate", 4.0, "pausing sends"},
		{"complaint_rate", 2.5, "Monitor"},
		{"bounce_rate", 4.0, "list hygiene"},
		{"block_rate", 4.0, "IP reputation"},
		{"delivery_rate", 2.5, "Investigate"},
		{"unknown", 2.5, "Monitor"},
	}

	for _, tt := range tests {
		t.Run(tt.metricName, func(t *testing.T) {
			rec := agent.getRecommendation(tt.metricName, tt.deviation, "TestEntity")
			assert.Contains(t, rec, tt.contains)
		})
	}
}

func TestDetectAnomalies(t *testing.T) {
	agent := newTestAgent()

	// Set up baseline
	baseline := &storage.Baseline{
		EntityType: "isp",
		EntityName: "Gmail",
		Metrics: map[string]*storage.MetricBaseline{
			"complaint_rate": {
				Mean:   0.00010,
				StdDev: 0.00002,
			},
		},
		DataPoints: 100,
	}
	agent.baselines["isp:Gmail"] = baseline

	// Normal metrics - no anomaly
	normalMetrics := sparkpost.ProcessedMetrics{
		GroupBy:       "isp",
		GroupValue:    "Gmail",
		ComplaintRate: 0.00011, // Within 1σ
	}
	
	alerts := agent.detectAnomalies(normalMetrics)
	assert.Empty(t, alerts)

	// Anomalous metrics
	anomalousMetrics := sparkpost.ProcessedMetrics{
		GroupBy:       "isp",
		GroupValue:    "Gmail",
		ComplaintRate: 0.00020, // 5σ above mean
	}

	alerts = agent.detectAnomalies(anomalousMetrics)
	assert.NotEmpty(t, alerts)
	assert.Equal(t, "complaint_rate", alerts[0].MetricName)
}

func TestAlertManagement(t *testing.T) {
	agent := newTestAgent()

	// Add some alerts
	agent.alerts = []Alert{
		{ID: "alert-1", Title: "Test Alert 1"},
		{ID: "alert-2", Title: "Test Alert 2"},
	}

	// Get alerts
	alerts := agent.GetAlerts()
	assert.Len(t, alerts, 2)

	// Acknowledge alert
	found := agent.AcknowledgeAlert("alert-1")
	assert.True(t, found)
	assert.True(t, agent.alerts[0].Acknowledged)

	// Try to acknowledge non-existent alert
	found = agent.AcknowledgeAlert("non-existent")
	assert.False(t, found)

	// Clear alerts
	agent.ClearAlerts()
	alerts = agent.GetAlerts()
	assert.Empty(t, alerts)
}

func TestGetBaselines(t *testing.T) {
	agent := newTestAgent()

	agent.baselines["isp:Gmail"] = &storage.Baseline{
		EntityType: "isp",
		EntityName: "Gmail",
	}
	agent.baselines["isp:Yahoo"] = &storage.Baseline{
		EntityType: "isp",
		EntityName: "Yahoo",
	}

	baselines := agent.GetBaselines()
	assert.Len(t, baselines, 2)
	assert.Contains(t, baselines, "isp:Gmail")
	assert.Contains(t, baselines, "isp:Yahoo")
}

func TestGetCorrelations(t *testing.T) {
	agent := newTestAgent()

	agent.correlations = []storage.Correlation{
		{
			EntityName:    "Yahoo",
			TriggerMetric: "volume",
			EffectMetric:  "complaint_rate",
		},
	}

	correlations := agent.GetCorrelations()
	assert.Len(t, correlations, 1)
	assert.Equal(t, "Yahoo", correlations[0].EntityName)
}

func TestConcurrentAccess(t *testing.T) {
	agent := newTestAgent()
	ctx := context.Background()

	done := make(chan bool)

	// Concurrent ProcessMetrics
	go func() {
		for i := 0; i < 100; i++ {
			agent.ProcessMetrics(ctx, []sparkpost.ProcessedMetrics{
				{Timestamp: time.Now(), GroupBy: "test", GroupValue: "test"},
			})
		}
		done <- true
	}()

	// Concurrent reads
	go func() {
		for i := 0; i < 100; i++ {
			_ = agent.GetAlerts()
			_ = agent.GetBaselines()
		}
		done <- true
	}()

	<-done
	<-done
}

func TestRollingStats(t *testing.T) {
	agent := newTestAgent()

	// Add metrics
	for i := 0; i < 20; i++ {
		metrics := sparkpost.ProcessedMetrics{
			Timestamp:     time.Now(),
			GroupBy:       "isp",
			GroupValue:    "Gmail",
			ComplaintRate: 0.0001 + float64(i)*0.00001,
		}
		agent.updateRollingStats(metrics)
	}

	// Check rolling stats exist
	key := "isp:Gmail:complaint_rate"
	stats, exists := agent.rollingStats[key]
	assert.True(t, exists)
	assert.Len(t, stats.Values, 20)
}

func TestFindThreshold(t *testing.T) {
	volumes := []float64{
		1000000, 1500000, 2000000, 2500000, 3000000,
		3500000, 4000000, 4500000, 5000000, 5500000,
		6000000, 6500000, 7000000, 7500000, 8000000,
		8500000, 9000000, 9500000, 10000000, 10500000,
	}
	complaints := []float64{
		0.0001, 0.0001, 0.0001, 0.0001, 0.00012,
		0.00015, 0.0002, 0.00025, 0.0003, 0.00035,
		0.0004, 0.00045, 0.0005, 0.00055, 0.0006,
		0.00065, 0.0007, 0.00075, 0.0008, 0.00085,
	}

	threshold := findThreshold(volumes, complaints)
	assert.Greater(t, threshold, 0.0)
	// Should be around the 75th percentile
	assert.GreaterOrEqual(t, threshold, 7000000.0)
}

func TestFindThreshold_InsufficientData(t *testing.T) {
	volumes := []float64{1000000, 2000000}
	complaints := []float64{0.0001, 0.0002}

	threshold := findThreshold(volumes, complaints)
	assert.Equal(t, 0.0, threshold)
}
