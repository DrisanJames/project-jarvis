package mailgun

import (
	"testing"
	"time"
)

func TestProcessedMetrics_CalculateRates(t *testing.T) {
	tests := []struct {
		name           string
		metrics        ProcessedMetrics
		expectedDelivery float64
		expectedOpen     float64
		expectedBounce   float64
	}{
		{
			name: "calculates rates correctly with valid data",
			metrics: ProcessedMetrics{
				Sent:          1000,
				Delivered:     950,
				UniqueOpened:  200,
				UniqueClicked: 50,
				Bounced:       50,
				HardBounced:   30,
				SoftBounced:   20,
				Complaints:    5,
				Unsubscribes:  10,
			},
			expectedDelivery: 0.95,
			expectedOpen:     200.0 / 950.0,
			expectedBounce:   0.05,
		},
		{
			name: "handles zero sent gracefully",
			metrics: ProcessedMetrics{
				Sent:      0,
				Delivered: 0,
			},
			expectedDelivery: 0,
			expectedOpen:     0,
			expectedBounce:   0,
		},
		{
			name: "handles zero delivered gracefully",
			metrics: ProcessedMetrics{
				Sent:         1000,
				Delivered:    0,
				UniqueOpened: 0,
			},
			expectedDelivery: 0,
			expectedOpen:     0,
			expectedBounce:   0,
		},
		{
			name: "calculates perfect delivery",
			metrics: ProcessedMetrics{
				Sent:      1000,
				Delivered: 1000,
				Bounced:   0,
			},
			expectedDelivery: 1.0,
			expectedBounce:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.metrics.CalculateRates()

			if tt.expectedDelivery != 0 && tt.metrics.DeliveryRate != tt.expectedDelivery {
				t.Errorf("DeliveryRate = %v, want %v", tt.metrics.DeliveryRate, tt.expectedDelivery)
			}

			if tt.expectedOpen != 0 && tt.metrics.OpenRate != tt.expectedOpen {
				t.Errorf("OpenRate = %v, want %v", tt.metrics.OpenRate, tt.expectedOpen)
			}

			if tt.expectedBounce != 0 && tt.metrics.BounceRate != tt.expectedBounce {
				t.Errorf("BounceRate = %v, want %v", tt.metrics.BounceRate, tt.expectedBounce)
			}
		})
	}
}

func TestMapDomainToISP(t *testing.T) {
	tests := []struct {
		domain   string
		expected string
	}{
		{"gmail.com", "Gmail"},
		{"GMAIL.COM", "Gmail"},
		{"Gmail.com", "Gmail"},
		{"googlemail.com", "Gmail"},
		{"yahoo.com", "Yahoo"},
		{"yahoo.co.uk", "Yahoo"},
		{"hotmail.com", "Hotmail / Outlook"},
		{"outlook.com", "Hotmail / Outlook"},
		{"live.com", "Hotmail / Outlook"},
		{"icloud.com", "Apple"},
		{"me.com", "Apple"},
		{"aol.com", "AOL"},
		{"comcast.net", "Comcast"},
		{"att.net", "AT&T"},
		{"protonmail.com", "Proton Mail"},
		{"unknown-domain.com", "Other"},
		{"", "Other"},
		{"  gmail.com  ", "Gmail"},
	}

	for _, tt := range tests {
		t.Run(tt.domain, func(t *testing.T) {
			result := MapDomainToISP(tt.domain)
			if result != tt.expected {
				t.Errorf("MapDomainToISP(%q) = %q, want %q", tt.domain, result, tt.expected)
			}
		})
	}
}

func TestDefaultMetrics(t *testing.T) {
	metrics := DefaultMetrics()

	if len(metrics) == 0 {
		t.Error("DefaultMetrics() returned empty slice")
	}

	// Check for essential metrics
	essentialMetrics := []string{
		"accepted_outgoing_count",
		"delivered_smtp_count",
		"bounced_count",
		"complained_count",
	}

	for _, essential := range essentialMetrics {
		found := false
		for _, m := range metrics {
			if m == essential {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DefaultMetrics() missing essential metric: %s", essential)
		}
	}
}

func TestConvertStatsToProcessedMetrics(t *testing.T) {
	stats := StatsItem{
		Time: "2025-01-27T00:00:00Z",
		Accepted: StatsCounter{Total: 1000},
		Delivered: StatsCounter{Total: 950},
		Opened: StatsCounter{Total: 200},
		Clicked: StatsCounter{Total: 50},
		Complained: StatsCounter{Total: 5},
		Unsubscribed: StatsCounter{Total: 10},
		Failed: FailedStats{
			Permanent: FailedDetail{Total: 30, Bounce: 25, EspBlock: 5},
			Temporary: FailedDetail{Total: 20},
		},
	}

	pm := ConvertStatsToProcessedMetrics(stats, "test.com", "domain", "test.com")

	if pm.Source != "mailgun" {
		t.Errorf("Source = %q, want %q", pm.Source, "mailgun")
	}

	if pm.Targeted != 1000 {
		t.Errorf("Targeted = %d, want %d", pm.Targeted, 1000)
	}

	if pm.Delivered != 950 {
		t.Errorf("Delivered = %d, want %d", pm.Delivered, 950)
	}

	if pm.Opened != 200 {
		t.Errorf("Opened = %d, want %d", pm.Opened, 200)
	}

	if pm.HardBounced != 25 {
		t.Errorf("HardBounced = %d, want %d", pm.HardBounced, 25)
	}

	if pm.BlockBounced != 5 {
		t.Errorf("BlockBounced = %d, want %d", pm.BlockBounced, 5)
	}

	// Check that rates were calculated
	if pm.DeliveryRate == 0 {
		t.Error("DeliveryRate was not calculated")
	}
}

func TestConvertMetricsDataToProcessedMetrics(t *testing.T) {
	data := MetricsData{
		AcceptedOutgoingCount:   1000,
		DeliveredSMTPCount:      900,
		DeliveredHTTPCount:      40,
		DeliveredOptimizedCount: 10,
		OpenedCount:            200,
		UniqueOpenedCount:      150,
		ClickedCount:           50,
		UniqueClickedCount:     40,
		BouncedCount:           50,
		HardBouncesCount:       30,
		SoftBouncesCount:       20,
		ComplainedCount:        5,
		UnsubscribedCount:      10,
		FailedCount:            5,
	}

	pm := ConvertMetricsDataToProcessedMetrics(data, "test.com", "summary", "all")

	if pm.Source != "mailgun" {
		t.Errorf("Source = %q, want %q", pm.Source, "mailgun")
	}

	if pm.Delivered != 950 {
		t.Errorf("Delivered = %d, want %d (sum of SMTP+HTTP+Optimized)", pm.Delivered, 950)
	}

	if pm.UniqueOpened != 150 {
		t.Errorf("UniqueOpened = %d, want %d", pm.UniqueOpened, 150)
	}

	if pm.Rejected != 5 {
		t.Errorf("Rejected = %d, want %d", pm.Rejected, 5)
	}
}

func TestConvertProviderStatsToISPMetrics(t *testing.T) {
	stats := ProviderStats{
		Accepted:     1000,
		Delivered:    950,
		Opened:       200,
		Clicked:      50,
		Unsubscribed: 10,
		Complained:   5,
		Bounced:      50,
	}

	isp := ConvertProviderStatsToISPMetrics("Gmail", stats, "test.com")

	if isp.Provider != "Gmail" {
		t.Errorf("Provider = %q, want %q", isp.Provider, "Gmail")
	}

	if isp.Status != "healthy" {
		t.Errorf("Status = %q, want %q", isp.Status, "healthy")
	}

	if isp.Metrics.Source != "mailgun" {
		t.Errorf("Metrics.Source = %q, want %q", isp.Metrics.Source, "mailgun")
	}

	if isp.Metrics.Delivered != 950 {
		t.Errorf("Metrics.Delivered = %d, want %d", isp.Metrics.Delivered, 950)
	}
}

func TestSummaryFields(t *testing.T) {
	now := time.Now()
	summary := Summary{
		Timestamp:         now,
		PeriodStart:       now.Add(-24 * time.Hour),
		PeriodEnd:         now,
		TotalTargeted:     10000,
		TotalDelivered:    9500,
		TotalOpened:       2000,
		TotalClicked:      500,
		TotalBounced:      500,
		TotalComplaints:   50,
		TotalUnsubscribes: 100,
		DeliveryRate:      0.95,
		OpenRate:          0.21,
		ClickRate:         0.05,
		BounceRate:        0.05,
		ComplaintRate:     0.005,
		UnsubscribeRate:   0.01,
	}

	if summary.TotalTargeted != 10000 {
		t.Errorf("TotalTargeted = %d, want %d", summary.TotalTargeted, 10000)
	}

	if summary.DeliveryRate != 0.95 {
		t.Errorf("DeliveryRate = %f, want %f", summary.DeliveryRate, 0.95)
	}
}

func TestISPMetricsFields(t *testing.T) {
	isp := ISPMetrics{
		Provider:     "Gmail",
		Status:       "warning",
		StatusReason: "High bounce rate",
		Trends: MetricTrends{
			VolumeDirection:     "up",
			VolumeChangePercent: 15.5,
			DeliveryTrend:       "down",
			DeliveryChange:      -2.5,
			ComplaintTrend:      "up",
			ComplaintChange:     0.5,
		},
	}

	if isp.Provider != "Gmail" {
		t.Errorf("Provider = %q, want %q", isp.Provider, "Gmail")
	}

	if isp.Status != "warning" {
		t.Errorf("Status = %q, want %q", isp.Status, "warning")
	}

	if isp.Trends.VolumeDirection != "up" {
		t.Errorf("Trends.VolumeDirection = %q, want %q", isp.Trends.VolumeDirection, "up")
	}
}

func TestDomainMetricsFields(t *testing.T) {
	dm := DomainMetrics{
		Domain:       "e.newproductsforyou.com",
		Status:       "healthy",
		StatusReason: "",
	}

	if dm.Domain != "e.newproductsforyou.com" {
		t.Errorf("Domain = %q, want %q", dm.Domain, "e.newproductsforyou.com")
	}

	if dm.Status != "healthy" {
		t.Errorf("Status = %q, want %q", dm.Status, "healthy")
	}
}

func TestIPMetricsFields(t *testing.T) {
	ip := IPMetrics{
		IP:     "192.168.1.1",
		Pool:   "default",
		Status: "healthy",
	}

	if ip.IP != "192.168.1.1" {
		t.Errorf("IP = %q, want %q", ip.IP, "192.168.1.1")
	}

	if ip.Pool != "default" {
		t.Errorf("Pool = %q, want %q", ip.Pool, "default")
	}
}

func TestIssueFields(t *testing.T) {
	issue := Issue{
		Severity:       "critical",
		Category:       "bounce",
		Description:    "High hard bounce rate",
		AffectedISP:    "Gmail",
		AffectedDomain: "test.com",
		Count:          1000,
		Recommendation: "Review list hygiene",
	}

	if issue.Severity != "critical" {
		t.Errorf("Severity = %q, want %q", issue.Severity, "critical")
	}

	if issue.Category != "bounce" {
		t.Errorf("Category = %q, want %q", issue.Category, "bounce")
	}

	if issue.Count != 1000 {
		t.Errorf("Count = %d, want %d", issue.Count, 1000)
	}
}

func TestMetricsQueryFields(t *testing.T) {
	now := time.Now()
	query := MetricsQuery{
		From:       now.Add(-24 * time.Hour),
		To:         now,
		Resolution: "hour",
		Domains:    []string{"test.com", "example.com"},
		Events:     []string{"delivered", "bounced"},
		Limit:      100,
	}

	if query.Resolution != "hour" {
		t.Errorf("Resolution = %q, want %q", query.Resolution, "hour")
	}

	if len(query.Domains) != 2 {
		t.Errorf("len(Domains) = %d, want %d", len(query.Domains), 2)
	}

	if query.Limit != 100 {
		t.Errorf("Limit = %d, want %d", query.Limit, 100)
	}
}
