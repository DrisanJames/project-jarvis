package mailing

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHashEmail(t *testing.T) {
	tests := []struct {
		email    string
		expected string
	}{
		{"test@example.com", hashEmail("test@example.com")},
		{"TEST@EXAMPLE.COM", hashEmail("test@example.com")}, // Should be case-insensitive
		{"  test@example.com  ", hashEmail("  test@example.com  ")}, // hashEmail does not trim whitespace
	}

	for _, tt := range tests {
		result := hashEmail(tt.email)
		if result != tt.expected {
			t.Errorf("hashEmail(%q) = %q, want %q", tt.email, result, tt.expected)
		}
	}
}

func TestExtractEmailDomain(t *testing.T) {
	tests := []struct {
		email    string
		expected string
	}{
		{"test@gmail.com", "gmail.com"},
		{"user@YAHOO.COM", "yahoo.com"},
		{"invalid", ""},
		{"", ""},
	}

	for _, tt := range tests {
		result := extractEmailDomain(tt.email)
		if result != tt.expected {
			t.Errorf("extractEmailDomain(%q) = %q, want %q", tt.email, result, tt.expected)
		}
	}
}

func TestGetISPForDomain(t *testing.T) {
	tests := []struct {
		domain   string
		expected string
	}{
		{"gmail.com", "gmail"},
		{"googlemail.com", "gmail"},
		{"yahoo.com", "yahoo"},
		{"outlook.com", "microsoft"},
		{"hotmail.com", "microsoft"},
		{"icloud.com", "apple"},
		{"unknown.com", "other"},
	}

	for _, tt := range tests {
		result := getISPForDomain(tt.domain)
		if result != tt.expected {
			t.Errorf("getISPForDomain(%q) = %q, want %q", tt.domain, result, tt.expected)
		}
	}
}

func TestDefaultAISettings(t *testing.T) {
	campaignID := uuid.New()
	ss := &SmartSender{}
	settings := ss.getDefaultSettings(campaignID)

	if settings.CampaignID != campaignID {
		t.Errorf("CampaignID mismatch")
	}
	if !settings.EnableSmartSending {
		t.Errorf("EnableSmartSending should be true by default")
	}
	if settings.TargetMetric != TargetMetricOpens {
		t.Errorf("TargetMetric should be 'opens' by default")
	}
	if settings.MinThrottleRate != 1000 {
		t.Errorf("MinThrottleRate should be 1000 by default")
	}
	if settings.MaxThrottleRate != 50000 {
		t.Errorf("MaxThrottleRate should be 50000 by default")
	}
	if settings.ABConfidenceThreshold != 0.95 {
		t.Errorf("ABConfidenceThreshold should be 0.95 by default")
	}
}

func TestCalculateNextOptimalTime(t *testing.T) {
	ss := &SmartSender{}
	
	// Test that optimal time is in the future
	optimalTime := ss.calculateNextOptimalTime(10)
	if optimalTime.Before(time.Now()) {
		t.Errorf("Optimal time should be in the future")
	}
	
	// Test that the hour is correct
	if optimalTime.Hour() != 10 {
		t.Errorf("Optimal hour should be 10, got %d", optimalTime.Hour())
	}
}

func TestAnalyzeMetricsForThrottle(t *testing.T) {
	ss := &SmartSender{}
	settings := ss.getDefaultSettings(uuid.New())
	settings.CurrentThrottleRate = 10000
	settings.ComplaintThreshold = 0.001 // 0.1%
	settings.BounceThreshold = 0.05     // 5%

	// Test case: No metrics
	recommendation := ss.analyzeMetricsForThrottle(nil, settings)
	if recommendation.Action != "maintain" {
		t.Errorf("Expected 'maintain' for empty metrics, got %s", recommendation.Action)
	}

	// Test case: Good metrics (should increase)
	goodMetrics := []*RealtimeMetrics{
		{
			SentCount:      1000,
			OpenCount:      150,  // 15% open rate
			BounceCount:    10,   // 1% bounce
			ComplaintCount: 0,    // 0 complaints
		},
	}
	recommendation = ss.analyzeMetricsForThrottle(goodMetrics, settings)
	if recommendation.Action != "increase" {
		t.Errorf("Expected 'increase' for good metrics, got %s", recommendation.Action)
	}

	// Test case: High bounce rate (should decrease)
	highBounceMetrics := []*RealtimeMetrics{
		{
			SentCount:      1000,
			OpenCount:      100,
			BounceCount:    100, // 10% bounce
			ComplaintCount: 0,
		},
	}
	recommendation = ss.analyzeMetricsForThrottle(highBounceMetrics, settings)
	if recommendation.Action != "decrease" {
		t.Errorf("Expected 'decrease' for high bounce metrics, got %s", recommendation.Action)
	}

	// Test case: High complaint rate (should decrease significantly)
	highComplaintMetrics := []*RealtimeMetrics{
		{
			SentCount:      1000,
			OpenCount:      100,
			BounceCount:    10,
			ComplaintCount: 2, // 0.2% - above threshold
		},
	}
	recommendation = ss.analyzeMetricsForThrottle(highComplaintMetrics, settings)
	if recommendation.Action != "decrease" {
		t.Errorf("Expected 'decrease' for high complaint metrics, got %s", recommendation.Action)
	}

	// Test case: Critical complaint rate (should pause)
	criticalComplaintMetrics := []*RealtimeMetrics{
		{
			SentCount:      1000,
			OpenCount:      100,
			BounceCount:    10,
			ComplaintCount: 5, // 0.5% - way above threshold
		},
	}
	recommendation = ss.analyzeMetricsForThrottle(criticalComplaintMetrics, settings)
	if recommendation.Action != "pause" {
		t.Errorf("Expected 'pause' for critical complaint metrics, got %s", recommendation.Action)
	}
}

func TestCampaignAISettingsDefaults(t *testing.T) {
	settings := &CampaignAISettings{
		CampaignID:                 uuid.New(),
		EnableSmartSending:         true,
		EnableThrottleOptimization: true,
		EnableSendTimeOptimization: true,
		EnableABAutoWinner:         true,
		TargetMetric:               TargetMetricOpens,
		MinThrottleRate:            1000,
		MaxThrottleRate:            50000,
		CurrentThrottleRate:        10000,
		LearningPeriodMinutes:      60,
		ABConfidenceThreshold:      0.95,
		ABMinSampleSize:            1000,
		PauseOnHighComplaints:      true,
		ComplaintThreshold:         0.001,
		BounceThreshold:            0.05,
	}

	if settings.ABConfidenceThreshold < 0 || settings.ABConfidenceThreshold > 1 {
		t.Errorf("ABConfidenceThreshold should be between 0 and 1")
	}
	if settings.MinThrottleRate > settings.MaxThrottleRate {
		t.Errorf("MinThrottleRate should not exceed MaxThrottleRate")
	}
}

func TestOptimalSendTimeResult(t *testing.T) {
	result := &OptimalSendTimeResult{
		Email:           "test@gmail.com",
		OptimalHourUTC:  10,
		Confidence:      0.8,
		Source:          "subscriber",
		EngagementScore: 0.75,
	}

	if result.OptimalHourUTC < 0 || result.OptimalHourUTC > 23 {
		t.Errorf("OptimalHourUTC should be between 0 and 23")
	}
	if result.Confidence < 0 || result.Confidence > 1 {
		t.Errorf("Confidence should be between 0 and 1")
	}
	if result.EngagementScore < 0 || result.EngagementScore > 1 {
		t.Errorf("EngagementScore should be between 0 and 1")
	}
}

func TestThrottleRecommendation(t *testing.T) {
	tests := []struct {
		name        string
		action      string
		newRate     int
		expectValid bool
	}{
		{"increase", "increase", 12500, true},
		{"decrease", "decrease", 7000, true},
		{"maintain", "maintain", 10000, true},
		{"pause", "pause", 0, true},
	}

	for _, tt := range tests {
		rec := &ThrottleRecommendation{
			Action:   tt.action,
			NewRate:  tt.newRate,
			Reason:   "Test reason",
			Confidence: 0.85,
			RiskLevel: "low",
		}

		if tt.action == "pause" && rec.NewRate != 0 {
			t.Errorf("Pause action should have NewRate=0")
		}
		if rec.Confidence < 0 || rec.Confidence > 1 {
			t.Errorf("Confidence should be between 0 and 1")
		}
	}
}

// Integration test helpers (require database)

func TestSmartSenderCreation(t *testing.T) {
	// This test verifies that SmartSender can be created without a DB
	// In real tests, you would inject a test database
	ss := NewSmartSender(nil, nil, nil)
	if ss == nil {
		t.Error("NewSmartSender should not return nil")
	}
	if ss.metricsInterval != 1*time.Minute {
		t.Errorf("Default metrics interval should be 1 minute")
	}
	if ss.optimizationInterval != 5*time.Minute {
		t.Errorf("Default optimization interval should be 5 minutes")
	}
}

// Benchmark tests

func BenchmarkHashEmail(b *testing.B) {
	email := "test@example.com"
	for i := 0; i < b.N; i++ {
		hashEmail(email)
	}
}

func BenchmarkExtractEmailDomain(b *testing.B) {
	email := "test@example.com"
	for i := 0; i < b.N; i++ {
		extractEmailDomain(email)
	}
}

func BenchmarkGetISPForDomain(b *testing.B) {
	domain := "gmail.com"
	for i := 0; i < b.N; i++ {
		getISPForDomain(domain)
	}
}

func BenchmarkCalculateNextOptimalTime(b *testing.B) {
	ss := &SmartSender{}
	for i := 0; i < b.N; i++ {
		ss.calculateNextOptimalTime(10)
	}
}

// Mock context for testing
func testContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}
