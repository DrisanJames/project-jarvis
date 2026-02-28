package agent

import (
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/mailgun"
	"github.com/ignite/sparkpost-monitor/internal/ses"
	"github.com/ignite/sparkpost-monitor/internal/sparkpost"
	"github.com/stretchr/testify/assert"
)

// Mock collectors for testing

type mockSparkPostCollector struct {
	summary *sparkpost.Summary
	isps    []sparkpost.ISPMetrics
}

func (m *mockSparkPostCollector) GetLatestSummary() *sparkpost.Summary {
	return m.summary
}

func (m *mockSparkPostCollector) GetLatestISPMetrics() []sparkpost.ISPMetrics {
	return m.isps
}

func (m *mockSparkPostCollector) GetLastFetchTime() time.Time {
	return time.Now()
}

type mockMailgunCollector struct {
	summary *mailgun.Summary
	isps    []mailgun.ISPMetrics
}

func (m *mockMailgunCollector) GetLatestSummary() *mailgun.Summary {
	return m.summary
}

func (m *mockMailgunCollector) GetLatestISPMetrics() []mailgun.ISPMetrics {
	return m.isps
}

func (m *mockMailgunCollector) GetLastFetchTime() time.Time {
	return time.Now()
}

type mockSESCollector struct {
	summary *ses.Summary
	isps    []ses.ISPMetrics
}

func (m *mockSESCollector) GetLatestSummary() *ses.Summary {
	return m.summary
}

func (m *mockSESCollector) GetLatestISPMetrics() []ses.ISPMetrics {
	return m.isps
}

func (m *mockSESCollector) GetLastFetchTime() time.Time {
	return time.Now()
}

func createTestAgentWithCollectors() *Agent {
	cfg := config.AgentConfig{
		AnomalySigma:  2.0,
		MinDataPoints: 10,
	}
	agent := New(cfg, nil)

	// Setup SparkPost mock
	spCollector := &mockSparkPostCollector{
		summary: &sparkpost.Summary{
			TotalTargeted:   5000000,
			TotalDelivered:  4900000,
			TotalOpened:     1000000,
			TotalClicked:    100000,
			TotalBounced:    50000,
			TotalComplaints: 500,
		},
		isps: []sparkpost.ISPMetrics{
			{
				Provider: "Gmail",
				Metrics: sparkpost.ProcessedMetrics{
					Targeted:      3000000,
					Delivered:     2950000,
					DeliveryRate:  0.983,
					OpenRate:      0.22,
					ClickRate:     0.025,
					BounceRate:    0.012,
					ComplaintRate: 0.00008,
				},
				Status: "healthy",
			},
			{
				Provider: "Yahoo",
				Metrics: sparkpost.ProcessedMetrics{
					Targeted:      2000000,
					Delivered:     1950000,
					DeliveryRate:  0.975,
					OpenRate:      0.18,
					ClickRate:     0.02,
					BounceRate:    0.02,
					ComplaintRate: 0.00035,
				},
				Status:       "warning",
				StatusReason: "Complaint rate elevated",
			},
		},
	}

	// Setup Mailgun mock
	mgCollector := &mockMailgunCollector{
		summary: &mailgun.Summary{
			TotalTargeted:   2000000,
			TotalDelivered:  1950000,
			TotalOpened:     400000,
			TotalClicked:    40000,
			TotalBounced:    30000,
			TotalComplaints: 200,
		},
		isps: []mailgun.ISPMetrics{
			{
				Provider: "Gmail",
				Metrics: mailgun.ProcessedMetrics{
					Targeted:      1200000,
					Delivered:     1180000,
					DeliveryRate:  0.983,
					OpenRate:      0.21,
					ClickRate:     0.022,
					BounceRate:    0.015,
					ComplaintRate: 0.0001,
				},
				Status: "healthy",
			},
		},
	}

	// Setup SES mock
	sesCollector := &mockSESCollector{
		summary: &ses.Summary{
			TotalTargeted:   1000000,
			TotalDelivered:  980000,
			TotalOpened:     180000,
			TotalClicked:    18000,
			TotalBounced:    15000,
			TotalComplaints: 80,
		},
		isps: []ses.ISPMetrics{
			{
				Provider: "Gmail",
				Metrics: ses.ProcessedMetrics{
					Targeted:      600000,
					Delivered:     590000,
					DeliveryRate:  0.983,
					OpenRate:      0.19,
					ClickRate:     0.018,
					BounceRate:    0.014,
					ComplaintRate: 0.00007,
				},
				Status: "healthy",
			},
		},
	}

	agent.SetCollectors(ESPCollectors{
		SparkPost: spCollector,
		Mailgun:   mgCollector,
		SES:       sesCollector,
	})

	return agent
}

func TestSetCollectors(t *testing.T) {
	cfg := config.AgentConfig{}
	agent := New(cfg, nil)

	collectors := ESPCollectors{
		SparkPost: &mockSparkPostCollector{},
	}

	agent.SetCollectors(collectors)

	assert.NotNil(t, agent.collectors)
}

func TestGetEcosystemData(t *testing.T) {
	agent := createTestAgentWithCollectors()

	eco, allISPs := agent.getEcosystemData()

	// Verify aggregated volumes
	assert.Greater(t, eco.TotalVolume, int64(0))
	assert.Greater(t, eco.TotalDelivered, int64(0))
	assert.Greater(t, eco.ProviderCount, 0)
	assert.Greater(t, eco.ISPCount, 0)

	// Verify ISPs collected
	assert.NotEmpty(t, allISPs)
}

func TestGetEcosystemData_NilCollectors(t *testing.T) {
	cfg := config.AgentConfig{}
	agent := New(cfg, nil)

	eco, allISPs := agent.getEcosystemData()

	assert.Equal(t, int64(0), eco.TotalVolume)
	assert.Empty(t, allISPs)
}

func TestUpdateISPCounts(t *testing.T) {
	agent := newTestAgent()
	eco := &EcosystemSummary{}

	agent.updateISPCounts(eco, "healthy")
	assert.Equal(t, 1, eco.HealthyISPs)

	agent.updateISPCounts(eco, "warning")
	assert.Equal(t, 1, eco.WarningISPs)

	agent.updateISPCounts(eco, "critical")
	assert.Equal(t, 1, eco.CriticalISPs)
}

func TestUnifiedChat_EcosystemQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()

	queries := []string{
		"show ecosystem overview",
		"how is overall performance",
		"what is the total volume",
	}

	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			response := agent.UnifiedChat(query)
			assert.NotEmpty(t, response.Message)
			assert.NotEmpty(t, response.Suggestions)
		})
	}
}

func TestUnifiedChat_ComparisonQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()

	response := agent.UnifiedChat("compare providers")
	assert.Contains(t, response.Message, "Comparison")
}

func TestUnifiedChat_ProviderQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()

	tests := []struct {
		query    string
		contains string
	}{
		{"how is sparkpost", "SPARKPOST"},
		{"show mailgun performance", "MAILGUN"},
		{"what about ses", "SES"},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			response := agent.UnifiedChat(tt.query)
			assert.Contains(t, response.Message, tt.contains)
		})
	}
}

func TestUnifiedChat_ISPQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()

	response := agent.UnifiedChat("how is gmail performing across providers")
	assert.Contains(t, response.Message, "Gmail")
}

func TestUnifiedChat_ConcernsQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()

	response := agent.UnifiedChat("what concerns should I watch")
	assert.Contains(t, response.Message, "Concern")
}

func TestUnifiedChat_RecommendationQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()

	response := agent.UnifiedChat("give me recommendations")
	assert.Contains(t, response.Message, "Recommendation")
}

func TestUnifiedChat_VolumeQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()

	response := agent.UnifiedChat("can I increase volume")
	assert.Contains(t, response.Message, "Volume")
}

func TestUnifiedChat_ForecastQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()

	response := agent.UnifiedChat("forecast for tomorrow")
	assert.Contains(t, response.Message, "Forecast")
}

func TestUnifiedChat_LearningQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()

	response := agent.UnifiedChat("what have you learned")
	assert.Contains(t, response.Message, "Learning")
}

func TestUnifiedChat_GeneralQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()

	response := agent.UnifiedChat("hello")
	assert.Contains(t, response.Message, "Email Ecosystem Agent")
}

func TestHandleEcosystemQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()
	eco, allISPs := agent.getEcosystemData()

	response := agent.handleEcosystemQuery(eco, allISPs)

	assert.Contains(t, response.Message, "Ecosystem")
	assert.NotEmpty(t, response.Suggestions)
}

func TestHandleComparisonQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()
	_, allISPs := agent.getEcosystemData()

	response := agent.handleComparisonQuery("compare", allISPs)

	assert.Contains(t, response.Message, "Comparison")
}

func TestHandleProviderQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()
	_, allISPs := agent.getEcosystemData()

	response := agent.handleProviderQuery("sparkpost", allISPs)

	assert.Contains(t, response.Message, "SPARKPOST")
}

func TestHandleProviderQuery_NoData(t *testing.T) {
	agent := createTestAgentWithCollectors()

	response := agent.handleProviderQuery("unknownprovider", []UnifiedISP{})

	assert.Contains(t, response.Message, "No data available")
}

func TestHandleISPQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()
	_, allISPs := agent.getEcosystemData()

	response := agent.handleISPQuery("gmail", allISPs)

	assert.Contains(t, response.Message, "Gmail")
}

func TestHandleISPQuery_NoMatch(t *testing.T) {
	agent := createTestAgentWithCollectors()

	response := agent.handleISPQuery("unknownisp", []UnifiedISP{})

	assert.Contains(t, response.Message, "No matching")
}

func TestHandleUnifiedConcernsQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()
	eco, allISPs := agent.getEcosystemData()

	response := agent.handleUnifiedConcernsQuery(eco, allISPs)

	assert.Contains(t, response.Message, "Concern")
}

func TestHandleUnifiedConcernsQuery_NoConcerns(t *testing.T) {
	agent := createTestAgentWithCollectors()
	eco := EcosystemSummary{
		TotalVolume:    1000000,
		ComplaintRate:  0.0001,
		BounceRate:     0.01,
		ProviderCount:  3,
		ISPCount:       5,
	}

	allISPs := []UnifiedISP{
		{Provider: "sparkpost", ISP: "Gmail", Status: "healthy"},
	}

	response := agent.handleUnifiedConcernsQuery(eco, allISPs)

	assert.Contains(t, response.Message, "All Clear")
}

func TestHandleUnifiedRecommendationQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()
	eco, allISPs := agent.getEcosystemData()

	response := agent.handleUnifiedRecommendationQuery("recommend", eco, allISPs)

	assert.Contains(t, response.Message, "Recommendation")
}

func TestHandleUnifiedVolumeQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()
	eco, allISPs := agent.getEcosystemData()

	response := agent.handleUnifiedVolumeQuery("volume", eco, allISPs)

	assert.Contains(t, response.Message, "Volume")
}

func TestHandleUnifiedForecastQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()
	eco, allISPs := agent.getEcosystemData()

	response := agent.handleUnifiedForecastQuery("forecast", eco, allISPs)

	assert.Contains(t, response.Message, "Forecast")
}

func TestHandleUnifiedLearningQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()

	response := agent.handleUnifiedLearningQuery()

	assert.Contains(t, response.Message, "Learning")
}

func TestHandleUnifiedGeneralQuery(t *testing.T) {
	agent := createTestAgentWithCollectors()
	eco, _ := agent.getEcosystemData()

	response := agent.handleUnifiedGeneralQuery(eco)

	assert.Contains(t, response.Message, "Email Ecosystem Agent")
}

func TestForecastVolume(t *testing.T) {
	agent := newTestAgent()

	// Add rolling stats
	for i := 0; i < 10; i++ {
		agent.rollingStats["sparkpost:volume"] = &RollingStats{
			MetricName: "volume",
			Values:     []float64{1000000, 1100000, 1200000, 1300000, 1400000, 1500000, 1600000, 1700000, 1800000, 1900000},
		}
	}

	forecasts, err := agent.ForecastVolume("sparkpost", 3)

	assert.NoError(t, err)
	assert.Len(t, forecasts, 3)
	for _, f := range forecasts {
		assert.Greater(t, f, int64(0))
	}
}

func TestForecastVolume_InsufficientData(t *testing.T) {
	agent := newTestAgent()

	_, err := agent.ForecastVolume("sparkpost", 3)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "insufficient data")
}

func TestEcosystemSummary_Structure(t *testing.T) {
	summary := EcosystemSummary{
		TotalVolume:     8000000,
		TotalDelivered:  7800000,
		TotalOpens:      1500000,
		TotalClicks:     150000,
		TotalBounces:    100000,
		TotalComplaints: 800,
		DeliveryRate:    0.975,
		OpenRate:        0.192,
		ClickRate:       0.019,
		BounceRate:      0.0125,
		ComplaintRate:   0.0001,
		ProviderCount:   3,
		ISPCount:        15,
		HealthyISPs:     12,
		WarningISPs:     2,
		CriticalISPs:    1,
	}

	assert.Equal(t, int64(8000000), summary.TotalVolume)
	assert.Equal(t, 3, summary.ProviderCount)
	assert.Equal(t, 15, summary.ISPCount)
}

func TestUnifiedISP_Structure(t *testing.T) {
	isp := UnifiedISP{
		Provider:      "sparkpost",
		ISP:           "Gmail",
		Volume:        3000000,
		Delivered:     2950000,
		DeliveryRate:  0.983,
		OpenRate:      0.22,
		ClickRate:     0.025,
		BounceRate:    0.012,
		ComplaintRate: 0.00008,
		Status:        "healthy",
		StatusReason:  "",
	}

	assert.Equal(t, "sparkpost", isp.Provider)
	assert.Equal(t, "Gmail", isp.ISP)
	assert.Equal(t, "healthy", isp.Status)
}
