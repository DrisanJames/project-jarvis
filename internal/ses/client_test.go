package ses

import (
	"testing"
	"time"
)

func TestAllMetrics(t *testing.T) {
	metrics := AllMetrics()
	
	expected := []string{
		"SEND",
		"DELIVERY",
		"PERMANENT_BOUNCE",
		"TRANSIENT_BOUNCE",
		"COMPLAINT",
		"OPEN",
		"CLICK",
	}
	
	if len(metrics) != len(expected) {
		t.Errorf("AllMetrics() returned %d metrics, want %d", len(metrics), len(expected))
	}
	
	for i, m := range metrics {
		if m != expected[i] {
			t.Errorf("AllMetrics()[%d] = %s, want %s", i, m, expected[i])
		}
	}
}

func TestContainsMetric(t *testing.T) {
	tests := []struct {
		id       string
		metric   string
		expected bool
	}{
		{"q0_SEND", "SEND", true},
		{"q1_DELIVERY", "DELIVERY", true},
		{"q2_PERMANENT_BOUNCE", "PERMANENT_BOUNCE", true},
		{"query_OPEN", "OPEN", true},
		{"q0_SEND", "DELIVERY", false},
		{"q1_DELIVERY", "OPEN", false},
		{"", "SEND", false},
		{"SEND", "DELIVERY", false},
	}
	
	for _, tt := range tests {
		t.Run(tt.id+"_"+tt.metric, func(t *testing.T) {
			result := containsMetric(tt.id, tt.metric)
			if result != tt.expected {
				t.Errorf("containsMetric(%s, %s) = %v, want %v", tt.id, tt.metric, result, tt.expected)
			}
		})
	}
}

func TestConvertISPDataToMetrics(t *testing.T) {
	data := ISPMetricData{
		ISP:             "Gmail",
		Send:            1000,
		Delivery:        950,
		PermanentBounce: 30,
		TransientBounce: 20,
		Complaint:       5,
		Open:            400,
		Click:           50,
	}
	
	pm := ConvertISPDataToMetrics(data)
	
	if pm.Source != "ses" {
		t.Errorf("Source = %s, want ses", pm.Source)
	}
	if pm.GroupBy != "isp" {
		t.Errorf("GroupBy = %s, want isp", pm.GroupBy)
	}
	if pm.GroupValue != "Gmail" {
		t.Errorf("GroupValue = %s, want Gmail", pm.GroupValue)
	}
	if pm.Sent != 1000 {
		t.Errorf("Sent = %d, want 1000", pm.Sent)
	}
	if pm.Delivered != 950 {
		t.Errorf("Delivered = %d, want 950", pm.Delivered)
	}
	if pm.HardBounced != 30 {
		t.Errorf("HardBounced = %d, want 30", pm.HardBounced)
	}
	if pm.SoftBounced != 20 {
		t.Errorf("SoftBounced = %d, want 20", pm.SoftBounced)
	}
	if pm.Bounced != 50 {
		t.Errorf("Bounced = %d, want 50", pm.Bounced)
	}
	if pm.Complaints != 5 {
		t.Errorf("Complaints = %d, want 5", pm.Complaints)
	}
	if pm.Opened != 400 {
		t.Errorf("Opened = %d, want 400", pm.Opened)
	}
	if pm.Clicked != 50 {
		t.Errorf("Clicked = %d, want 50", pm.Clicked)
	}
	
	// Check calculated rates
	expectedDeliveryRate := 0.95 // 950/1000
	if pm.DeliveryRate < expectedDeliveryRate-0.001 || pm.DeliveryRate > expectedDeliveryRate+0.001 {
		t.Errorf("DeliveryRate = %f, want ~%f", pm.DeliveryRate, expectedDeliveryRate)
	}
	
	expectedBounceRate := 0.05 // 50/1000
	if pm.BounceRate < expectedBounceRate-0.001 || pm.BounceRate > expectedBounceRate+0.001 {
		t.Errorf("BounceRate = %f, want ~%f", pm.BounceRate, expectedBounceRate)
	}
}

func TestConvertISPDataToISPMetrics(t *testing.T) {
	data := ISPMetricData{
		ISP:             "Yahoo",
		Send:            10000,
		Delivery:        9800,
		PermanentBounce: 100,
		TransientBounce: 100,
		Complaint:       2, // 0.02% - well below threshold
		Open:            4000,
		Click:           500,
	}
	
	isp := ConvertISPDataToISPMetrics(data)
	
	if isp.Provider != "Yahoo" {
		t.Errorf("Provider = %s, want Yahoo", isp.Provider)
	}
	if isp.Status != "healthy" {
		t.Errorf("Status = %s, want healthy (complaint rate: %f)", isp.Status, isp.Metrics.ComplaintRate)
	}
	if isp.Metrics.Sent != 10000 {
		t.Errorf("Metrics.Sent = %d, want 10000", isp.Metrics.Sent)
	}
}

func TestEvaluateISPHealth_Healthy(t *testing.T) {
	pm := &ProcessedMetrics{
		Sent:          10000,
		Delivered:     9500,
		Bounced:       400,
		Complaints:    2,
		DeliveryRate:  0.95,
		BounceRate:    0.04,
		ComplaintRate: 0.0002, // 0.02% - below warning threshold
	}
	
	status, reason := EvaluateISPHealth(pm)
	
	if status != "healthy" {
		t.Errorf("Status = %s, want healthy", status)
	}
	if reason != "" {
		t.Errorf("Reason = %s, want empty", reason)
	}
}

func TestEvaluateISPHealth_HighComplaintRate(t *testing.T) {
	pm := &ProcessedMetrics{
		Sent:          1000,
		Delivered:     950,
		Complaints:    15,
		ComplaintRate: 0.015, // 1.5% - critical
	}
	
	status, reason := EvaluateISPHealth(pm)
	
	if status != "critical" {
		t.Errorf("Status = %s, want critical", status)
	}
	if reason == "" {
		t.Error("Reason should not be empty for critical status")
	}
}

func TestEvaluateISPHealth_HighBounceRate(t *testing.T) {
	pm := &ProcessedMetrics{
		Sent:       1000,
		Delivered:  850,
		Bounced:    150,
		BounceRate: 0.15, // 15% - critical
	}
	
	status, _ := EvaluateISPHealth(pm)
	
	if status != "critical" {
		t.Errorf("Status = %s, want critical", status)
	}
}

func TestEvaluateISPHealth_LowDeliveryRate(t *testing.T) {
	pm := &ProcessedMetrics{
		Sent:         1000,
		Delivered:    800,
		DeliveryRate: 0.80, // 80% - warning
	}
	
	status, reason := EvaluateISPHealth(pm)
	
	if status != "warning" {
		t.Errorf("Status = %s, want warning", status)
	}
	if reason == "" {
		t.Error("Reason should not be empty for warning status")
	}
}

func TestAggregateISPMetricsToSummary(t *testing.T) {
	metrics := []ISPMetrics{
		{
			Provider: "Gmail",
			Metrics: ProcessedMetrics{
				Targeted:    1000,
				Delivered:   950,
				Opened:      400,
				Clicked:     50,
				Bounced:     50,
				Complaints:  5,
			},
		},
		{
			Provider: "Yahoo",
			Metrics: ProcessedMetrics{
				Targeted:    500,
				Delivered:   480,
				Opened:      200,
				Clicked:     25,
				Bounced:     20,
				Complaints:  2,
			},
		},
	}
	
	from := time.Now().Add(-24 * time.Hour)
	to := time.Now()
	
	summary := AggregateISPMetricsToSummary(metrics, from, to)
	
	if summary.TotalTargeted != 1500 {
		t.Errorf("TotalTargeted = %d, want 1500", summary.TotalTargeted)
	}
	if summary.TotalDelivered != 1430 {
		t.Errorf("TotalDelivered = %d, want 1430", summary.TotalDelivered)
	}
	if summary.TotalOpened != 600 {
		t.Errorf("TotalOpened = %d, want 600", summary.TotalOpened)
	}
	if summary.TotalClicked != 75 {
		t.Errorf("TotalClicked = %d, want 75", summary.TotalClicked)
	}
	if summary.TotalBounced != 70 {
		t.Errorf("TotalBounced = %d, want 70", summary.TotalBounced)
	}
	if summary.TotalComplaints != 7 {
		t.Errorf("TotalComplaints = %d, want 7", summary.TotalComplaints)
	}
	
	// Check calculated rates
	expectedDeliveryRate := float64(1430) / float64(1500)
	if summary.DeliveryRate < expectedDeliveryRate-0.001 || summary.DeliveryRate > expectedDeliveryRate+0.001 {
		t.Errorf("DeliveryRate = %f, want ~%f", summary.DeliveryRate, expectedDeliveryRate)
	}
}

func TestProcessedMetrics_CalculateRates(t *testing.T) {
	pm := &ProcessedMetrics{
		Sent:        1000,
		Delivered:   950,
		Opened:      400,
		Clicked:     50,
		Bounced:     50,
		HardBounced: 30,
		SoftBounced: 20,
		Complaints:  5,
	}
	
	pm.CalculateRates()
	
	tests := []struct {
		name     string
		got      float64
		expected float64
	}{
		{"DeliveryRate", pm.DeliveryRate, 0.95},
		{"BounceRate", pm.BounceRate, 0.05},
		{"HardBounceRate", pm.HardBounceRate, 0.03},
		{"SoftBounceRate", pm.SoftBounceRate, 0.02},
		{"ComplaintRate", pm.ComplaintRate, 0.005},
		{"OpenRate", pm.OpenRate, float64(400) / float64(950)},
		{"ClickRate", pm.ClickRate, float64(50) / float64(950)},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got < tt.expected-0.001 || tt.got > tt.expected+0.001 {
				t.Errorf("%s = %f, want ~%f", tt.name, tt.got, tt.expected)
			}
		})
	}
}

func TestProcessedMetrics_CalculateRates_ZeroSent(t *testing.T) {
	pm := &ProcessedMetrics{
		Sent:      0,
		Delivered: 0,
	}
	
	pm.CalculateRates()
	
	// Should not panic and rates should be 0
	if pm.DeliveryRate != 0 {
		t.Errorf("DeliveryRate = %f, want 0", pm.DeliveryRate)
	}
}

func TestSummary_CalculateRates(t *testing.T) {
	summary := &Summary{
		TotalTargeted:   1000,
		TotalDelivered:  950,
		TotalOpened:     400,
		TotalClicked:    50,
		TotalBounced:    50,
		TotalComplaints: 5,
	}
	
	summary.CalculateRates()
	
	if summary.DeliveryRate < 0.949 || summary.DeliveryRate > 0.951 {
		t.Errorf("DeliveryRate = %f, want ~0.95", summary.DeliveryRate)
	}
	if summary.BounceRate < 0.049 || summary.BounceRate > 0.051 {
		t.Errorf("BounceRate = %f, want ~0.05", summary.BounceRate)
	}
}

func TestGenerateRecommendations(t *testing.T) {
	issues := []Issue{
		{Category: "complaint", Severity: "critical"},
		{Category: "bounce", Severity: "warning"},
		{Category: "delivery", Severity: "warning"},
	}
	
	recs := generateRecommendations(issues)
	
	if len(recs) == 0 {
		t.Error("Expected recommendations to be generated")
	}
	
	// Check that recommendations are unique (no duplicates)
	seen := make(map[string]bool)
	for _, rec := range recs {
		if seen[rec] {
			t.Errorf("Duplicate recommendation: %s", rec)
		}
		seen[rec] = true
	}
}

func TestISPMetricData_Fields(t *testing.T) {
	data := ISPMetricData{
		ISP:             "Outlook",
		Send:            100,
		Delivery:        95,
		PermanentBounce: 3,
		TransientBounce: 2,
		Complaint:       1,
		Open:            40,
		Click:           5,
	}
	
	if data.ISP != "Outlook" {
		t.Errorf("ISP = %s, want Outlook", data.ISP)
	}
	if data.Send != 100 {
		t.Errorf("Send = %d, want 100", data.Send)
	}
}

func TestMetricsQuery_Fields(t *testing.T) {
	now := time.Now()
	query := MetricsQuery{
		From: now.Add(-24 * time.Hour),
		To:   now,
		ISPs: []string{"Gmail", "Yahoo"},
	}
	
	if len(query.ISPs) != 2 {
		t.Errorf("ISPs length = %d, want 2", len(query.ISPs))
	}
	if query.From.After(query.To) {
		t.Error("From should be before To")
	}
}

func TestIssue_Fields(t *testing.T) {
	issue := Issue{
		Severity:       "critical",
		Category:       "complaint",
		Description:    "High complaint rate",
		AffectedISP:    "Gmail",
		Count:          100,
		Recommendation: "Review list hygiene",
	}
	
	if issue.Severity != "critical" {
		t.Errorf("Severity = %s, want critical", issue.Severity)
	}
	if issue.Category != "complaint" {
		t.Errorf("Category = %s, want complaint", issue.Category)
	}
}

func TestSignalsData_Fields(t *testing.T) {
	signals := SignalsData{
		Timestamp: time.Now(),
		TopIssues: []Issue{
			{Severity: "warning", Description: "Test issue"},
		},
		Recommendations: []string{"Test recommendation"},
	}
	
	if len(signals.TopIssues) != 1 {
		t.Errorf("TopIssues length = %d, want 1", len(signals.TopIssues))
	}
	if len(signals.Recommendations) != 1 {
		t.Errorf("Recommendations length = %d, want 1", len(signals.Recommendations))
	}
}
