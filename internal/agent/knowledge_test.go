package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewKnowledgeBase(t *testing.T) {
	tempDir := t.TempDir()
	storagePath := filepath.Join(tempDir, "kb.json")

	kb := NewKnowledgeBase(storagePath)

	require.NotNil(t, kb)
	assert.Equal(t, storagePath, kb.storagePath)
	assert.NotNil(t, kb.EcosystemState)
	assert.NotNil(t, kb.LearnedPatterns)
	assert.NotNil(t, kb.HistoricalInsights)
	assert.NotNil(t, kb.PerformanceBenchmarks)
	assert.NotNil(t, kb.ISPKnowledge)
	assert.NotNil(t, kb.BestPractices)
	assert.NotNil(t, kb.ComplianceRules)
}

func TestNewKnowledgeBase_EmptyPath(t *testing.T) {
	kb := NewKnowledgeBase("")

	require.NotNil(t, kb)
	assert.Empty(t, kb.storagePath)
}

func TestInitializeIndustryBestPractices(t *testing.T) {
	bp := initializeIndustryBestPractices()

	require.NotNil(t, bp)
	
	// Verify deliverability targets
	assert.Equal(t, 0.95, bp.DeliveryRateTarget)
	assert.Equal(t, 0.02, bp.BounceRateThreshold)
	assert.Equal(t, 0.001, bp.ComplaintRateThreshold)
	
	// Verify engagement targets
	assert.Equal(t, 0.18, bp.OpenRateHealthy)
	assert.Equal(t, 0.025, bp.ClickRateHealthy)
	
	// Verify guidelines exist
	assert.NotEmpty(t, bp.GmailGuidelines)
	assert.NotEmpty(t, bp.YahooGuidelines)
	assert.NotEmpty(t, bp.OutlookGuidelines)
	assert.NotEmpty(t, bp.AppleGuidelines)
	assert.NotEmpty(t, bp.WarmupGuidelines)
	assert.NotEmpty(t, bp.AuthenticationRequired)
	assert.NotEmpty(t, bp.ContentGuidelines)
	assert.NotEmpty(t, bp.SendTimeOptimization)
}

func TestInitializeComplianceKnowledge(t *testing.T) {
	compliance := initializeComplianceKnowledge()

	require.NotNil(t, compliance)
	assert.NotEmpty(t, compliance.CANSPAMRules)
	assert.NotEmpty(t, compliance.GDPRRules)
	assert.NotEmpty(t, compliance.CCPARules)
	assert.NotEmpty(t, compliance.CASLRules)
	assert.NotEmpty(t, compliance.RequiredElements)
	assert.NotEmpty(t, compliance.ProhibitedPractices)
}

func TestKnowledgeBase_InitializeISPProfiles(t *testing.T) {
	kb := &KnowledgeBase{
		ISPKnowledge: make(map[string]*ISPProfile),
	}
	kb.initializeISPProfiles()

	assert.Contains(t, kb.ISPKnowledge, "gmail")
	assert.Contains(t, kb.ISPKnowledge, "yahoo")
	assert.Contains(t, kb.ISPKnowledge, "outlook")
	assert.Contains(t, kb.ISPKnowledge, "apple")
	assert.Contains(t, kb.ISPKnowledge, "aol")

	// Verify Gmail profile
	gmail := kb.ISPKnowledge["gmail"]
	assert.Equal(t, "Gmail", gmail.Name)
	assert.Equal(t, 0.001, gmail.MaxComplaintRate)
	assert.NotEmpty(t, gmail.BestPractices)
}

func TestKnowledgeBase_SaveAndLoad(t *testing.T) {
	tempDir := t.TempDir()
	storagePath := filepath.Join(tempDir, "kb.json")

	// Create and populate knowledge base
	kb := NewKnowledgeBase(storagePath)
	kb.TotalLearningCycles = 10
	kb.DataPointsAnalyzed = 1000
	kb.EcosystemState.OverallHealth = "healthy"
	kb.EcosystemState.HealthScore = 85.0
	
	kb.AddLearnedPattern(LearnedPattern{
		Type:        "correlation",
		Description: "Test pattern",
		Confidence:  0.8,
	})

	// Save
	err := kb.Save()
	require.NoError(t, err)

	// Verify file exists
	_, err = os.Stat(storagePath)
	assert.NoError(t, err)

	// Create new knowledge base and load
	kb2 := NewKnowledgeBase(storagePath)
	
	assert.Equal(t, 10, kb2.TotalLearningCycles)
	assert.Equal(t, int64(1000), kb2.DataPointsAnalyzed)
	assert.Equal(t, "healthy", kb2.EcosystemState.OverallHealth)
}

func TestKnowledgeBase_Save_EmptyPath(t *testing.T) {
	kb := &KnowledgeBase{storagePath: ""}
	
	err := kb.Save()
	assert.NoError(t, err) // Should return nil for empty path
}

func TestKnowledgeBase_Load_EmptyPath(t *testing.T) {
	kb := &KnowledgeBase{storagePath: ""}
	
	err := kb.Load()
	assert.NoError(t, err) // Should return nil for empty path
}

func TestKnowledgeBase_Load_NonExistent(t *testing.T) {
	kb := &KnowledgeBase{storagePath: "/nonexistent/path/kb.json"}
	
	err := kb.Load()
	assert.Error(t, err)
}

func TestKnowledgeBase_AddLearnedPattern(t *testing.T) {
	kb := &KnowledgeBase{
		LearnedPatterns: make([]LearnedPattern, 0),
	}

	pattern := LearnedPattern{
		Type:             "correlation",
		Description:      "Volume affects complaint rate",
		Confidence:       0.85,
		TriggerCondition: "volume > 5M",
		ExpectedOutcome:  "complaint_rate increases",
	}

	kb.AddLearnedPattern(pattern)

	require.Len(t, kb.LearnedPatterns, 1)
	assert.NotEmpty(t, kb.LearnedPatterns[0].ID)
	assert.Equal(t, 1, kb.LearnedPatterns[0].Occurrences)
	assert.False(t, kb.LearnedPatterns[0].FirstObserved.IsZero())
}

func TestKnowledgeBase_AddLearnedPattern_DuplicateUpdate(t *testing.T) {
	kb := &KnowledgeBase{
		LearnedPatterns: make([]LearnedPattern, 0),
	}

	pattern := LearnedPattern{
		Type:             "correlation",
		TriggerCondition: "volume > 5M",
		Confidence:       0.80,
	}

	kb.AddLearnedPattern(pattern)
	assert.Equal(t, 1, kb.LearnedPatterns[0].Occurrences)

	// Add same pattern again (should update, not add)
	pattern2 := LearnedPattern{
		Type:             "correlation",
		TriggerCondition: "volume > 5M",
		Confidence:       0.90, // Higher confidence
	}
	kb.AddLearnedPattern(pattern2)

	assert.Len(t, kb.LearnedPatterns, 1)
	assert.Equal(t, 2, kb.LearnedPatterns[0].Occurrences)
	assert.Equal(t, 0.90, kb.LearnedPatterns[0].Confidence) // Updated to higher
}

func TestKnowledgeBase_AddHistoricalInsight(t *testing.T) {
	kb := &KnowledgeBase{
		HistoricalInsights: make([]HistoricalInsight, 0),
	}

	insight := HistoricalInsight{
		TimeRange:       "7d",
		Category:        "deliverability",
		Title:           "Weekly Performance Report",
		Summary:         "Delivery rates improved by 2%",
		KeyFindings:     []string{"Gmail improved", "Yahoo stable"},
		Recommendations: []string{"Continue current practices"},
	}

	kb.AddHistoricalInsight(insight)

	require.Len(t, kb.HistoricalInsights, 1)
	assert.NotEmpty(t, kb.HistoricalInsights[0].ID)
	assert.False(t, kb.HistoricalInsights[0].GeneratedAt.IsZero())
}

func TestKnowledgeBase_AddHistoricalInsight_Limit(t *testing.T) {
	kb := &KnowledgeBase{
		HistoricalInsights: make([]HistoricalInsight, 0),
	}

	// Add 105 insights (max is 100)
	for i := 0; i < 105; i++ {
		kb.AddHistoricalInsight(HistoricalInsight{
			Title: "Insight",
		})
	}

	assert.Len(t, kb.HistoricalInsights, 100) // Should be capped at 100
}

func TestKnowledgeBase_UpdateBenchmark(t *testing.T) {
	kb := &KnowledgeBase{
		PerformanceBenchmarks: make(map[string]*Benchmark),
	}

	benchmark := &Benchmark{
		MetricName:      "delivery_rate",
		EntityType:      "ecosystem",
		CurrentValue:    0.96,
		TargetValue:     0.95,
		IndustryAverage: 0.94,
	}

	kb.UpdateBenchmark("ecosystem:delivery_rate", benchmark)

	assert.Contains(t, kb.PerformanceBenchmarks, "ecosystem:delivery_rate")
	assert.False(t, kb.PerformanceBenchmarks["ecosystem:delivery_rate"].CurrentDate.IsZero())
}

func TestKnowledgeBase_RecordAnalysis(t *testing.T) {
	kb := &KnowledgeBase{
		AnalysisHistory: make([]AnalysisResult, 0),
	}

	result := AnalysisResult{
		Timestamp:       time.Now(),
		EcosystemHealth: "healthy",
		HealthScore:     85.0,
		TotalVolume24h:  10000000,
		CriticalIssues:  []string{},
		WarningIssues:   []string{"High bounce rate on Yahoo"},
	}

	kb.RecordAnalysis(result)

	assert.NotNil(t, kb.LastAnalysis)
	assert.Equal(t, "healthy", kb.LastAnalysis.EcosystemHealth)
	assert.Equal(t, 1, kb.TotalLearningCycles)
	assert.Len(t, kb.AnalysisHistory, 1)
}

func TestKnowledgeBase_RecordAnalysis_Limit(t *testing.T) {
	kb := &KnowledgeBase{
		AnalysisHistory: make([]AnalysisResult, 0),
	}

	// Add 170 analyses (max is 168)
	for i := 0; i < 170; i++ {
		kb.RecordAnalysis(AnalysisResult{
			Timestamp: time.Now(),
		})
	}

	assert.Len(t, kb.AnalysisHistory, 168)
	assert.Equal(t, 170, kb.TotalLearningCycles)
}

func TestKnowledgeBase_GetKnowledgeSummary(t *testing.T) {
	kb := NewKnowledgeBase("")
	kb.TotalLearningCycles = 50
	kb.DataPointsAnalyzed = 5000
	kb.EcosystemState.OverallHealth = "warning"
	kb.EcosystemState.HealthScore = 72.0

	summary := kb.GetKnowledgeSummary()

	assert.Equal(t, "warning", summary["ecosystem_health"])
	assert.Equal(t, 72.0, summary["health_score"])
	assert.Equal(t, 50, summary["total_learning_cycles"])
	assert.Equal(t, int64(5000), summary["data_points_analyzed"])
}

func TestKnowledgeBase_GetBestPracticesForISP(t *testing.T) {
	kb := NewKnowledgeBase("")

	// Known ISP
	practices := kb.GetBestPracticesForISP("gmail")
	assert.NotEmpty(t, practices)

	// Unknown ISP - should return warmup guidelines
	practices = kb.GetBestPracticesForISP("unknown-isp")
	assert.NotEmpty(t, practices)
}

func TestKnowledgeBase_GetComplianceChecklist(t *testing.T) {
	kb := NewKnowledgeBase("")

	checklist := kb.GetComplianceChecklist()

	assert.Contains(t, checklist, "canspam")
	assert.Contains(t, checklist, "gdpr")
	assert.Contains(t, checklist, "ccpa")
	assert.Contains(t, checklist, "casl")
	assert.Contains(t, checklist, "required")
	assert.Contains(t, checklist, "prohibited")
}

func TestKnowledgeBase_GetRecentPatterns(t *testing.T) {
	kb := &KnowledgeBase{
		LearnedPatterns: []LearnedPattern{
			{ID: "1", LastObserved: time.Now().Add(-time.Hour)},
			{ID: "2", LastObserved: time.Now()},
			{ID: "3", LastObserved: time.Now().Add(-2 * time.Hour)},
		},
	}

	// Get all
	patterns := kb.GetRecentPatterns(0)
	assert.Len(t, patterns, 3)

	// Get limited
	patterns = kb.GetRecentPatterns(2)
	assert.Len(t, patterns, 2)
}

func TestEcosystemKnowledge_Structure(t *testing.T) {
	eco := &EcosystemKnowledge{
		OverallHealth:         "healthy",
		HealthScore:           85.0,
		DailyAverageVolume:    10000000,
		WeeklyTrend:           5.2,
		MonthlyTrend:          12.5,
		BaselineDeliveryRate:  0.96,
		BaselineOpenRate:      0.20,
		BaselineClickRate:     0.025,
		BaselineBounceRate:    0.015,
		BaselineComplaintRate: 0.0008,
		DailyAverageRevenue:   25000.0,
		RevenueTrend:          8.5,
		TopRevenueOffers:      []string{"offer1", "offer2"},
		ESPVolumeShare:        map[string]float64{"sparkpost": 0.6, "mailgun": 0.3, "ses": 0.1},
		ESPHealthStatus:       map[string]string{"sparkpost": "healthy", "mailgun": "healthy"},
		ActiveIssues:          []string{},
		ResolvedIssues:        []string{"issue1"},
	}

	assert.Equal(t, "healthy", eco.OverallHealth)
	assert.Equal(t, 85.0, eco.HealthScore)
	assert.Contains(t, eco.ESPVolumeShare, "sparkpost")
}

func TestLearnedPattern_Structure(t *testing.T) {
	pattern := LearnedPattern{
		ID:               "pattern-123",
		Type:             "correlation",
		Description:      "High volume leads to complaints",
		Confidence:       0.85,
		Occurrences:      25,
		LastObserved:     time.Now(),
		FirstObserved:    time.Now().Add(-7 * 24 * time.Hour),
		TriggerCondition: "volume > 5M",
		ExpectedOutcome:  "complaint_rate > 0.1%",
		RevenueImpact:    -1500.0,
		HealthImpact:     "negative",
		Recommendation:   "Reduce volume to Yahoo",
	}

	assert.Equal(t, "correlation", pattern.Type)
	assert.Equal(t, 0.85, pattern.Confidence)
	assert.Equal(t, "negative", pattern.HealthImpact)
}

func TestHistoricalInsight_Structure(t *testing.T) {
	insight := HistoricalInsight{
		ID:               "insight-456",
		GeneratedAt:      time.Now(),
		TimeRange:        "30d",
		Category:         "revenue",
		Title:            "Monthly Revenue Analysis",
		Summary:          "Revenue increased by 15%",
		DetailedAnalysis: "Detailed analysis content...",
		KeyFindings:      []string{"Finding 1", "Finding 2"},
		Recommendations:  []string{"Rec 1", "Rec 2"},
		MetricsUsed:      []string{"revenue", "conversions"},
		DataSources:      []string{"everflow", "ongage"},
	}

	assert.Equal(t, "30d", insight.TimeRange)
	assert.Equal(t, "revenue", insight.Category)
	assert.Len(t, insight.KeyFindings, 2)
}

func TestBenchmark_Structure(t *testing.T) {
	benchmark := &Benchmark{
		MetricName:      "delivery_rate",
		EntityType:      "isp",
		EntityName:      "gmail",
		CurrentValue:    0.97,
		CurrentDate:     time.Now(),
		TargetValue:     0.95,
		IndustryAverage: 0.94,
		BestInClass:     0.99,
		Last7Days:       0.965,
		Last30Days:      0.96,
		Last90Days:      0.958,
		Trend:           "improving",
		TrendPercentage: 1.5,
		Status:          "exceeding",
		Gap:             0.02,
	}

	assert.Equal(t, "delivery_rate", benchmark.MetricName)
	assert.Equal(t, "exceeding", benchmark.Status)
	assert.Equal(t, "improving", benchmark.Trend)
}

func TestISPProfile_Structure(t *testing.T) {
	profile := &ISPProfile{
		Name:                  "Gmail",
		DeliveryRate:          0.98,
		OpenRate:              0.22,
		ClickRate:             0.03,
		BounceRate:            0.012,
		ComplaintRate:         0.0005,
		BaselineDeliveryRate:  0.97,
		BaselineBounceRate:    0.015,
		BaselineComplaintRate: 0.0006,
		MaxComplaintRate:      0.001,
		MaxBounceRate:         0.02,
		Status:                "healthy",
		StatusReason:          "",
		KnownIssues:           []string{},
		Recommendations:       []string{"Continue good practices"},
		BestPractices:         []string{"Use Postmaster Tools"},
	}

	assert.Equal(t, "Gmail", profile.Name)
	assert.Equal(t, 0.001, profile.MaxComplaintRate)
	assert.NotEmpty(t, profile.BestPractices)
}

func TestAnalysisResult_Structure(t *testing.T) {
	result := AnalysisResult{
		Timestamp:           time.Now(),
		AnalysisDuration:    "2.5s",
		EcosystemHealth:     "warning",
		HealthScore:         72.0,
		TotalVolume24h:      8000000,
		TotalRevenue24h:     18000.0,
		AvgDeliveryRate:     0.94,
		AvgBounceRate:       0.025,
		AvgComplaintRate:    0.0009,
		CriticalIssues:      []string{},
		WarningIssues:       []string{"Yahoo bounce rate elevated"},
		ImmediateActions:    []string{"Monitor Yahoo"},
		ShortTermActions:    []string{"Clean list"},
		LongTermActions:     []string{"Improve targeting"},
		NewPatternsFound:    2,
		TrendChanges:        []string{"Volume trending up"},
		ActiveKanbanTasks:   5,
		TasksCreatedThisCycle: 1,
	}

	assert.Equal(t, "warning", result.EcosystemHealth)
	assert.Equal(t, 72.0, result.HealthScore)
	assert.Len(t, result.WarningIssues, 1)
}
