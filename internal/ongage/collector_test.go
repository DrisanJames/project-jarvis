package ongage

import (
	"testing"
	"time"
)

func TestContainsEmoji(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"Hello World", false},
		{"Hello 😀 World", true},
		{"🎉 Sale!", true},
		{"Check out ☀️ today", true},
		{"Normal text here", false},
		{"Numbers 123 and symbols !@#", false},
		{"🚀 Launch", true},
	}

	for _, tc := range tests {
		result := containsEmoji(tc.input)
		if result != tc.expected {
			t.Errorf("containsEmoji(%q) = %v, expected %v", tc.input, result, tc.expected)
		}
	}
}

func TestContainsNumber(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"Hello World", false},
		{"50% Off!", true},
		{"Save $100", true},
		{"No numbers here", false},
		{"2024 Sale", true},
		{"Deal #1", true},
	}

	for _, tc := range tests {
		result := containsNumber(tc.input)
		if result != tc.expected {
			t.Errorf("containsNumber(%q) = %v, expected %v", tc.input, result, tc.expected)
		}
	}
}

func TestContainsUrgencyWords(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"Check out our products", false},
		{"URGENT: Action Required", true},
		{"Last Chance to Save!", true},
		{"Limited Time Offer", true},
		{"Don't miss out!", true},
		{"Hurry! Sale ends soon", true},
		{"Act Now!", true},
		{"Today Only: 50% Off", true},
		{"This offer expires tomorrow", true},
		{"Final sale of the year", true},
		{"Shop now for great deals", true},
		{"Regular newsletter content", false},
	}

	for _, tc := range tests {
		result := containsUrgencyWords(tc.input)
		if result != tc.expected {
			t.Errorf("containsUrgencyWords(%q) = %v, expected %v", tc.input, result, tc.expected)
		}
	}
}

func TestGetStringValue(t *testing.T) {
	row := ReportRow{
		"string_field": "hello",
		"float_field":  float64(123.45),
		"int_field":    int64(42),
		"nil_field":    nil,
	}

	tests := []struct {
		key      string
		expected string
	}{
		{"string_field", "hello"},
		{"float_field", "123.45"},
		{"int_field", "42"},
		{"missing_field", ""},
	}

	for _, tc := range tests {
		result := getStringValue(row, tc.key)
		if result != tc.expected {
			t.Errorf("getStringValue(row, %q) = %q, expected %q", tc.key, result, tc.expected)
		}
	}
}

func TestGetInt64Value(t *testing.T) {
	row := ReportRow{
		"float_field":  float64(123),
		"int64_field":  int64(456),
		"string_field": "789",
		"invalid":      "not a number",
	}

	tests := []struct {
		key      string
		expected int64
	}{
		{"float_field", 123},
		{"int64_field", 456},
		{"string_field", 789},
		{"invalid", 0},
		{"missing", 0},
	}

	for _, tc := range tests {
		result := getInt64Value(row, tc.key)
		if result != tc.expected {
			t.Errorf("getInt64Value(row, %q) = %d, expected %d", tc.key, result, tc.expected)
		}
	}
}

func TestGetFloat64Value(t *testing.T) {
	row := ReportRow{
		"float_field":  float64(123.45),
		"int64_field":  int64(456),
		"string_field": "78.9",
		"invalid":      "not a number",
	}

	tests := []struct {
		key      string
		expected float64
	}{
		{"float_field", 123.45},
		{"int64_field", 456.0},
		{"string_field", 78.9},
		{"invalid", 0},
		{"missing", 0},
	}

	for _, tc := range tests {
		result := getFloat64Value(row, tc.key)
		if result != tc.expected {
			t.Errorf("getFloat64Value(row, %q) = %f, expected %f", tc.key, result, tc.expected)
		}
	}
}

func TestProcessCampaignStats(t *testing.T) {
	collector := &Collector{}

	rows := []ReportRow{
		{
			"mailing_id":            "12345",
			"mailing_name":          "Test Campaign",
			"email_message_subject": "Check out our deals!",
			"schedule_date":         "1704067200",
			"esp_name":              "SparkPost",
			"esp_connection_id":     "5001",
			"segment_name":          "Active Users",
			"targeted":              float64(10000),
			"sent":                  float64(10000),
			"success":               float64(9800),
			"opens":                 float64(3000),
			"unique_opens":          float64(2500),
			"clicks":                float64(500),
			"unique_clicks":         float64(400),
			"unsubscribes":          float64(10),
			"complaints":            float64(2),
			"hard_bounces":          float64(100),
			"soft_bounces":          float64(100),
		},
	}

	espMap := map[string]ESPConnection{
		"5001": {ID: "5001", ESPID: ESPIDSparkPost, Name: "SparkPost"},
	}

	campaigns := collector.processCampaignStats(rows, espMap)

	if len(campaigns) != 1 {
		t.Fatalf("Expected 1 campaign, got %d", len(campaigns))
	}

	c := campaigns[0]

	if c.ID != "12345" {
		t.Errorf("Expected ID '12345', got '%s'", c.ID)
	}

	if c.Subject != "Check out our deals!" {
		t.Errorf("Expected subject 'Check out our deals!', got '%s'", c.Subject)
	}

	if c.ESP != "SparkPost" {
		t.Errorf("Expected ESP 'SparkPost', got '%s'", c.ESP)
	}

	if c.Sent != 10000 {
		t.Errorf("Expected Sent=10000, got %d", c.Sent)
	}

	if c.Delivered != 9800 {
		t.Errorf("Expected Delivered=9800, got %d", c.Delivered)
	}

	// Check calculated rates
	expectedDeliveryRate := 0.98
	if c.DeliveryRate != expectedDeliveryRate {
		t.Errorf("Expected DeliveryRate=%f, got %f", expectedDeliveryRate, c.DeliveryRate)
	}

	expectedOpenRate := 0.25
	if c.OpenRate != expectedOpenRate {
		t.Errorf("Expected OpenRate=%f, got %f", expectedOpenRate, c.OpenRate)
	}
}

func TestProcessESPStats(t *testing.T) {
	collector := &Collector{}

	rows := []ReportRow{
		{
			"esp_name":             "SparkPost",
			"esp_connection_id":    "5001",
			"esp_connection_title": "SparkPost Production",
			"sent":                 float64(100000),
			"success":              float64(98000),
			"unique_opens":         float64(25000),
			"unique_clicks":        float64(5000),
			"hard_bounces":         float64(1000),
			"soft_bounces":         float64(1000),
			"complaints":           float64(50),
		},
	}

	perfs := collector.processESPStats(rows)

	if len(perfs) != 1 {
		t.Fatalf("Expected 1 ESP performance, got %d", len(perfs))
	}

	p := perfs[0]

	if p.ESPName != "SparkPost" {
		t.Errorf("Expected ESPName 'SparkPost', got '%s'", p.ESPName)
	}

	if p.TotalSent != 100000 {
		t.Errorf("Expected TotalSent=100000, got %d", p.TotalSent)
	}

	expectedDeliveryRate := 0.98
	if p.DeliveryRate != expectedDeliveryRate {
		t.Errorf("Expected DeliveryRate=%f, got %f", expectedDeliveryRate, p.DeliveryRate)
	}
}

func TestProcessScheduleStats(t *testing.T) {
	collector := &Collector{}

	// Use a Unix timestamp - the hour and day will depend on local timezone
	// but we can still verify the stats processing works correctly
	rows := []ReportRow{
		{
			"schedule_date": "1737367200", // Some timestamp
			"mailing_id":    "123",
			"sent":          float64(50000),
			"success":       float64(49000),
			"unique_opens":  float64(15000),
			"unique_clicks": float64(3000),
		},
	}

	analyses := collector.processScheduleStats(rows)

	if len(analyses) != 1 {
		t.Fatalf("Expected 1 schedule analysis, got %d", len(analyses))
	}

	a := analyses[0]

	// Verify hour is between 0-23
	if a.Hour < 0 || a.Hour > 23 {
		t.Errorf("Hour should be 0-23, got %d", a.Hour)
	}

	// Verify day of week is 1-7
	if a.DayOfWeek < 1 || a.DayOfWeek > 7 {
		t.Errorf("DayOfWeek should be 1-7, got %d", a.DayOfWeek)
	}

	// Verify day name is not empty
	if a.DayName == "" {
		t.Error("DayName should not be empty")
	}

	// 15000/50000 = 0.30, which is >= 0.25, so "optimal"
	if a.Performance != "optimal" {
		t.Errorf("Expected Performance='optimal', got '%s'", a.Performance)
	}

	// Verify totals
	if a.TotalSent != 50000 {
		t.Errorf("Expected TotalSent=50000, got %d", a.TotalSent)
	}

	// Verify rate calculation
	expectedOpenRate := 0.30 // 15000/50000
	if a.AvgOpenRate != expectedOpenRate {
		t.Errorf("Expected AvgOpenRate=%f, got %f", expectedOpenRate, a.AvgOpenRate)
	}
}

func TestProcessAudienceStats(t *testing.T) {
	collector := &Collector{}

	rows := []ReportRow{
		{
			"segment_id":     "2001",
			"segment_name":   "VIP Customers",
			"campaign_count": float64(20),
			"targeted":       float64(50000),
			"sent":           float64(50000),
			"unique_opens":   float64(15000),
			"unique_clicks":  float64(2000),
			"hard_bounces":   float64(500),
			"soft_bounces":   float64(500),
		},
	}

	analyses := collector.processAudienceStats(rows)

	if len(analyses) != 1 {
		t.Fatalf("Expected 1 audience analysis, got %d", len(analyses))
	}

	a := analyses[0]

	if a.SegmentName != "VIP Customers" {
		t.Errorf("Expected SegmentName='VIP Customers', got '%s'", a.SegmentName)
	}

	// 15000/50000 = 0.30 open rate, 2000/50000 = 0.04 click rate
	// This qualifies as "high" engagement
	if a.Engagement != "high" {
		t.Errorf("Expected Engagement='high', got '%s'", a.Engagement)
	}
}

func TestAnalyzeSubjectLines(t *testing.T) {
	collector := &Collector{}

	campaigns := []ProcessedCampaign{
		{
			Subject:      "🎉 50% Off Today Only!",
			Sent:         10000,
			UniqueOpens:  3000,
			UniqueClicks: 600,
			ESP:          "SparkPost",
		},
		{
			Subject:      "🎉 50% Off Today Only!",
			Sent:         15000,
			UniqueOpens:  4500,
			UniqueClicks: 900,
			ESP:          "SparkPost",
		},
		{
			Subject:      "Monthly Newsletter",
			Sent:         20000,
			UniqueOpens:  2000,
			UniqueClicks: 200,
			ESP:          "Mailgun",
		},
	}

	analyses := collector.analyzeSubjectLines(campaigns)

	if len(analyses) != 2 {
		t.Fatalf("Expected 2 subject analyses, got %d", len(analyses))
	}

	// Find the emoji subject
	var emojiAnalysis *SubjectLineAnalysis
	for i := range analyses {
		if analyses[i].Subject == "🎉 50% Off Today Only!" {
			emojiAnalysis = &analyses[i]
			break
		}
	}

	if emojiAnalysis == nil {
		t.Fatal("Could not find emoji subject analysis")
	}

	if emojiAnalysis.CampaignCount != 2 {
		t.Errorf("Expected CampaignCount=2, got %d", emojiAnalysis.CampaignCount)
	}

	if !emojiAnalysis.HasEmoji {
		t.Error("Expected HasEmoji=true")
	}

	if !emojiAnalysis.HasNumber {
		t.Error("Expected HasNumber=true (50%)")
	}

	if !emojiAnalysis.HasUrgency {
		t.Error("Expected HasUrgency=true (Today Only)")
	}

	// Total sent = 25000, total opens = 7500, rate = 0.30 = "high"
	if emojiAnalysis.Performance != "high" {
		t.Errorf("Expected Performance='high', got '%s'", emojiAnalysis.Performance)
	}
}

func TestCollectorGetMethods(t *testing.T) {
	collector := &Collector{
		metrics: &CollectorMetrics{
			Campaigns: []ProcessedCampaign{
				{ID: "1", Name: "Test"},
			},
			SubjectAnalysis: []SubjectLineAnalysis{
				{Subject: "Test Subject"},
			},
			ScheduleAnalysis: []ScheduleAnalysis{
				{Hour: 10, DayOfWeek: 2},
			},
			ESPPerformance: []ESPPerformance{
				{ESPName: "SparkPost"},
			},
			AudienceAnalysis: []AudienceAnalysis{
				{SegmentName: "Test Segment"},
			},
			PipelineMetrics: []PipelineMetrics{
				{Date: "2024-01-01"},
			},
			LastFetch:       time.Now(),
			TotalCampaigns:  1,
			ActiveCampaigns: 0,
		},
	}

	// Test GetCampaigns
	campaigns := collector.GetCampaigns()
	if len(campaigns) != 1 || campaigns[0].ID != "1" {
		t.Error("GetCampaigns failed")
	}

	// Test GetSubjectAnalysis
	subjects := collector.GetSubjectAnalysis()
	if len(subjects) != 1 || subjects[0].Subject != "Test Subject" {
		t.Error("GetSubjectAnalysis failed")
	}

	// Test GetScheduleAnalysis
	schedules := collector.GetScheduleAnalysis()
	if len(schedules) != 1 || schedules[0].Hour != 10 {
		t.Error("GetScheduleAnalysis failed")
	}

	// Test GetESPPerformance
	espPerfs := collector.GetESPPerformance()
	if len(espPerfs) != 1 || espPerfs[0].ESPName != "SparkPost" {
		t.Error("GetESPPerformance failed")
	}

	// Test GetAudienceAnalysis
	audiences := collector.GetAudienceAnalysis()
	if len(audiences) != 1 || audiences[0].SegmentName != "Test Segment" {
		t.Error("GetAudienceAnalysis failed")
	}

	// Test GetPipelineMetrics
	pipelines := collector.GetPipelineMetrics()
	if len(pipelines) != 1 || pipelines[0].Date != "2024-01-01" {
		t.Error("GetPipelineMetrics failed")
	}

	// Test LastFetch
	lastFetch := collector.LastFetch()
	if lastFetch.IsZero() {
		t.Error("LastFetch should not be zero")
	}
}

func TestCollectorNilMetrics(t *testing.T) {
	collector := &Collector{}

	// All getters should return nil when metrics is nil
	if collector.GetCampaigns() != nil {
		t.Error("Expected nil campaigns")
	}
	if collector.GetSubjectAnalysis() != nil {
		t.Error("Expected nil subject analysis")
	}
	if collector.GetScheduleAnalysis() != nil {
		t.Error("Expected nil schedule analysis")
	}
	if collector.GetESPPerformance() != nil {
		t.Error("Expected nil ESP performance")
	}
	if collector.GetAudienceAnalysis() != nil {
		t.Error("Expected nil audience analysis")
	}
	if collector.GetPipelineMetrics() != nil {
		t.Error("Expected nil pipeline metrics")
	}
	if !collector.LastFetch().IsZero() {
		t.Error("Expected zero time for LastFetch")
	}
}

func TestNewCollector(t *testing.T) {
	config := Config{
		BaseURL:     "https://api.ongage.net/12345",
		Username:    "test",
		Password:    "test",
		AccountCode: "test",
	}
	client := NewClient(config)

	collector := NewCollector(client, 5*time.Minute, 30)

	if collector.client != client {
		t.Error("Client not set correctly")
	}
	if collector.fetchInterval != 5*time.Minute {
		t.Errorf("Expected fetchInterval 5m, got %v", collector.fetchInterval)
	}
	if collector.lookbackDays != 30 {
		t.Errorf("Expected lookbackDays 30, got %d", collector.lookbackDays)
	}
}
