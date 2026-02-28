//go:build ignore
// +build ignore

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// =============================================================================
// METRICS TESTS
// =============================================================================

func TestNewLoadTestMetrics(t *testing.T) {
	m := NewLoadTestMetrics()
	
	if m.ESPMetrics == nil {
		t.Error("ESPMetrics should not be nil")
	}
	
	if len(m.ESPMetrics) != 4 {
		t.Errorf("Expected 4 ESP metrics, got %d", len(m.ESPMetrics))
	}
	
	for _, esp := range []string{"sparkpost", "ses", "mailgun", "sendgrid"} {
		if _, ok := m.ESPMetrics[esp]; !ok {
			t.Errorf("Missing ESP metric for %s", esp)
		}
	}
	
	if m.PhaseResults == nil {
		t.Error("PhaseResults should not be nil")
	}
}

func TestRecordEnqueue(t *testing.T) {
	m := NewLoadTestMetrics()
	
	// Record successful enqueue
	m.RecordEnqueue(100, 10*time.Millisecond, nil)
	
	if m.TotalEnqueued != 100 {
		t.Errorf("Expected TotalEnqueued=100, got %d", m.TotalEnqueued)
	}
	
	if len(m.EnqueueLatencies) != 1 {
		t.Errorf("Expected 1 latency record, got %d", len(m.EnqueueLatencies))
	}
	
	// Record failed enqueue
	m.RecordEnqueue(50, 5*time.Millisecond, errTest)
	
	if m.EnqueueErrors != 1 {
		t.Errorf("Expected EnqueueErrors=1, got %d", m.EnqueueErrors)
	}
	
	if m.TotalErrors != 1 {
		t.Errorf("Expected TotalErrors=1, got %d", m.TotalErrors)
	}
	
	// TotalEnqueued should not change on error
	if m.TotalEnqueued != 100 {
		t.Errorf("TotalEnqueued should remain 100, got %d", m.TotalEnqueued)
	}
}

func TestRecordSend(t *testing.T) {
	m := NewLoadTestMetrics()
	
	// Record successful sends for different ESPs
	m.RecordSend("sparkpost", 2000, 50*time.Millisecond, nil)
	m.RecordSend("ses", 50, 30*time.Millisecond, nil)
	m.RecordSend("mailgun", 1000, 40*time.Millisecond, nil)
	m.RecordSend("sendgrid", 1000, 35*time.Millisecond, nil)
	
	if m.TotalSent != 4050 {
		t.Errorf("Expected TotalSent=4050, got %d", m.TotalSent)
	}
	
	// Check individual ESP metrics
	if m.ESPMetrics["sparkpost"].TotalSent != 2000 {
		t.Errorf("Expected sparkpost TotalSent=2000, got %d", m.ESPMetrics["sparkpost"].TotalSent)
	}
	
	if m.ESPMetrics["sparkpost"].TotalBatches != 1 {
		t.Errorf("Expected sparkpost TotalBatches=1, got %d", m.ESPMetrics["sparkpost"].TotalBatches)
	}
	
	// Record error
	m.RecordSend("ses", 0, 100*time.Millisecond, errTest)
	
	if m.ESPMetrics["ses"].Errors != 1 {
		t.Errorf("Expected ses Errors=1, got %d", m.ESPMetrics["ses"].Errors)
	}
	
	if m.SendErrors != 1 {
		t.Errorf("Expected SendErrors=1, got %d", m.SendErrors)
	}
}

func TestRecordRateLimitHit(t *testing.T) {
	m := NewLoadTestMetrics()
	
	m.RecordRateLimitHit("sparkpost", 100)
	m.RecordRateLimitHit("sparkpost", 150)
	m.RecordRateLimitHit("ses", 50)
	
	if m.TotalRateLimitHits != 3 {
		t.Errorf("Expected TotalRateLimitHits=3, got %d", m.TotalRateLimitHits)
	}
	
	if len(m.RateLimitRecoveryMs) != 3 {
		t.Errorf("Expected 3 recovery times, got %d", len(m.RateLimitRecoveryMs))
	}
	
	if m.ESPMetrics["sparkpost"].RateLimitHits != 2 {
		t.Errorf("Expected sparkpost RateLimitHits=2, got %d", m.ESPMetrics["sparkpost"].RateLimitHits)
	}
}

func TestRecordQueueDepth(t *testing.T) {
	m := NewLoadTestMetrics()
	
	m.RecordQueueDepth(1000)
	m.RecordQueueDepth(1500)
	m.RecordQueueDepth(1200)
	
	if m.QueueDepthMax != 1500 {
		t.Errorf("Expected QueueDepthMax=1500, got %d", m.QueueDepthMax)
	}
	
	if len(m.QueueDepthSamples) != 3 {
		t.Errorf("Expected 3 samples, got %d", len(m.QueueDepthSamples))
	}
}

func TestPercentile(t *testing.T) {
	tests := []struct {
		name     string
		values   []time.Duration
		p        int
		expected time.Duration
	}{
		{
			name:     "empty slice",
			values:   []time.Duration{},
			p:        50,
			expected: 0,
		},
		{
			name:     "single value",
			values:   []time.Duration{100 * time.Millisecond},
			p:        50,
			expected: 100 * time.Millisecond,
		},
		{
			name:     "p50 even count",
			values:   []time.Duration{10, 20, 30, 40, 50, 60, 70, 80, 90, 100},
			p:        50,
			expected: 50,
		},
		{
			name:     "p99",
			values:   []time.Duration{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 100},
			p:        99,
			expected: 19, // p99 of 20 values is at index 18 (0-indexed), which is value 19
		},
		{
			name:     "unsorted input",
			values:   []time.Duration{50, 10, 90, 30, 70, 20, 80, 40, 60, 100},
			p:        50,
			expected: 50,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := percentile(tt.values, tt.p)
			if result != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestMetricsFinalize(t *testing.T) {
	m := NewLoadTestMetrics()
	config := DefaultConfig()
	
	// Simulate test data
	m.TestStartTime = time.Now().Add(-5 * time.Minute)
	m.TestEndTime = time.Now()
	m.TotalEnqueued = 1000000
	m.TotalSent = 500000
	
	// Add latencies
	for i := 0; i < 100; i++ {
		m.EnqueueLatencies = append(m.EnqueueLatencies, time.Duration(i)*time.Millisecond)
		m.SendLatencies = append(m.SendLatencies, time.Duration(i)*time.Millisecond)
	}
	
	// Add queue depth samples
	for i := 0; i < 10; i++ {
		m.QueueDepthSamples = append(m.QueueDepthSamples, int64(1000+i*100))
	}
	
	// Add ESP metrics
	m.ESPMetrics["sparkpost"].TotalSent = 150000
	m.ESPMetrics["sparkpost"].TotalBatches = 75
	m.ESPMetrics["ses"].TotalSent = 250000
	m.ESPMetrics["ses"].TotalBatches = 5000
	m.ESPMetrics["mailgun"].TotalSent = 50000
	m.ESPMetrics["mailgun"].TotalBatches = 50
	m.ESPMetrics["sendgrid"].TotalSent = 50000
	m.ESPMetrics["sendgrid"].TotalBatches = 50
	
	m.Finalize(config)
	
	// Check calculations
	// Allow for small timing variations
	if m.TestDuration.Round(time.Second) != 5*time.Minute {
		t.Errorf("Expected TestDuration=~5m, got %v", m.TestDuration)
	}
	
	expectedEnqueueRate := float64(m.TotalEnqueued) / m.TestDuration.Seconds()
	if m.EnqueueRatePerSecond != expectedEnqueueRate {
		t.Errorf("Expected EnqueueRatePerSecond=%.2f, got %.2f", expectedEnqueueRate, m.EnqueueRatePerSecond)
	}
	
	// Check percentiles were calculated
	if m.EnqueueLatencyP50 == 0 {
		t.Error("EnqueueLatencyP50 should not be 0")
	}
	
	if m.SendLatencyP99 == 0 {
		t.Error("SendLatencyP99 should not be 0")
	}
	
	// Check queue depth average
	if m.QueueDepthAvg == 0 {
		t.Error("QueueDepthAvg should not be 0")
	}
	
	// Check ESP average batch sizes
	if m.ESPMetrics["sparkpost"].AvgBatchSize != 2000 {
		t.Errorf("Expected sparkpost AvgBatchSize=2000, got %.2f", m.ESPMetrics["sparkpost"].AvgBatchSize)
	}
	
	// Check projected daily capacity
	if m.ProjectedDailyCapacity == 0 {
		t.Error("ProjectedDailyCapacity should not be 0")
	}
}

func TestIdentifyBottleneck(t *testing.T) {
	config := DefaultConfig()
	
	tests := []struct {
		name     string
		setup    func(*LoadTestMetrics)
		expected string
	}{
		{
			name: "enqueue bottleneck",
			setup: func(m *LoadTestMetrics) {
				m.EnqueueRatePerSecond = float64(config.EnqueueRatePerSecond) * 0.5
				m.SendRatePerSecond = float64(config.SendRatePerSecond)
			},
			expected: "Queue Enqueue (PostgreSQL COPY)",
		},
		{
			name: "send bottleneck",
			setup: func(m *LoadTestMetrics) {
				m.EnqueueRatePerSecond = float64(config.EnqueueRatePerSecond)
				m.SendRatePerSecond = float64(config.SendRatePerSecond) * 0.5
			},
			expected: "Send Workers",
		},
		{
			name: "rate limit bottleneck",
			setup: func(m *LoadTestMetrics) {
				m.EnqueueRatePerSecond = float64(config.EnqueueRatePerSecond)
				m.SendRatePerSecond = float64(config.SendRatePerSecond)
				m.TotalRateLimitHits = 200
			},
			expected: "ESP Rate Limits",
		},
		{
			name: "error rate bottleneck",
			setup: func(m *LoadTestMetrics) {
				m.EnqueueRatePerSecond = float64(config.EnqueueRatePerSecond)
				m.SendRatePerSecond = float64(config.SendRatePerSecond)
				m.TotalSent = 10000
				m.TotalErrors = 500 // 5% error rate
			},
			expected: "High Error Rate",
		},
		{
			name: "no bottleneck",
			setup: func(m *LoadTestMetrics) {
				m.EnqueueRatePerSecond = float64(config.EnqueueRatePerSecond)
				m.SendRatePerSecond = float64(config.SendRatePerSecond)
				m.TotalSent = 10000
				m.TotalErrors = 10
			},
			expected: "None identified",
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewLoadTestMetrics()
			tt.setup(m)
			result := identifyBottleneck(m, config)
			if result != tt.expected {
				t.Errorf("Expected bottleneck '%s', got '%s'", tt.expected, result)
			}
		})
	}
}

// =============================================================================
// MOCK ESP SERVER TESTS
// =============================================================================

func TestMockESPServerSparkPost(t *testing.T) {
	m := NewLoadTestMetrics()
	server := NewMockESPServer(0, m)
	
	// Create test server
	handler := http.HandlerFunc(server.handleSparkPost)
	ts := httptest.NewServer(handler)
	defer ts.Close()
	
	// Test with valid batch
	recipients := make([]map[string]interface{}, 100)
	for i := 0; i < 100; i++ {
		recipients[i] = map[string]interface{}{
			"address": map[string]string{"email": "test@test.com"},
		}
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"recipients": recipients,
	})
	
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
	
	var result struct {
		Results struct {
			TotalAcceptedRecipients int `json:"total_accepted_recipients"`
		} `json:"results"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	
	if result.Results.TotalAcceptedRecipients != 100 {
		t.Errorf("Expected 100 accepted, got %d", result.Results.TotalAcceptedRecipients)
	}
}

func TestMockESPServerSparkPostExceedsBatch(t *testing.T) {
	m := NewLoadTestMetrics()
	server := NewMockESPServer(0, m)
	
	handler := http.HandlerFunc(server.handleSparkPost)
	ts := httptest.NewServer(handler)
	defer ts.Close()
	
	// Test with batch exceeding limit
	recipients := make([]map[string]interface{}, 2500)
	for i := 0; i < 2500; i++ {
		recipients[i] = map[string]interface{}{
			"address": map[string]string{"email": "test@test.com"},
		}
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"recipients": recipients,
	})
	
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("Expected status 400 for exceeding batch, got %d", resp.StatusCode)
	}
}

func TestMockESPServerSES(t *testing.T) {
	m := NewLoadTestMetrics()
	server := NewMockESPServer(0, m)
	
	handler := http.HandlerFunc(server.handleSES)
	ts := httptest.NewServer(handler)
	defer ts.Close()
	
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
	
	var result struct {
		MessageId string `json:"MessageId"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	
	if result.MessageId == "" {
		t.Error("Expected MessageId in response")
	}
}

func TestMockESPServerMailgun(t *testing.T) {
	m := NewLoadTestMetrics()
	server := NewMockESPServer(0, m)
	
	handler := http.HandlerFunc(server.handleMailgun)
	ts := httptest.NewServer(handler)
	defer ts.Close()
	
	resp, err := http.Post(ts.URL, "application/x-www-form-urlencoded", strings.NewReader("to=test@test.com"))
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
	
	var result struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	
	if result.ID == "" {
		t.Error("Expected id in response")
	}
}

func TestMockESPServerSendGrid(t *testing.T) {
	m := NewLoadTestMetrics()
	server := NewMockESPServer(0, m)
	
	handler := http.HandlerFunc(server.handleSendGrid)
	ts := httptest.NewServer(handler)
	defer ts.Close()
	
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("Expected status 202, got %d", resp.StatusCode)
	}
	
	messageID := resp.Header.Get("X-Message-Id")
	if messageID == "" {
		t.Error("Expected X-Message-Id header")
	}
}

func TestMockESPServerStats(t *testing.T) {
	m := NewLoadTestMetrics()
	server := NewMockESPServer(0, m)
	
	// Simulate some requests
	server.sparkpostRequests = 10
	server.sesRequests = 20
	server.mailgunRequests = 5
	server.sendgridRequests = 15
	
	stats := server.Stats()
	
	if stats["sparkpost_requests"] != 10 {
		t.Errorf("Expected sparkpost_requests=10, got %d", stats["sparkpost_requests"])
	}
	if stats["ses_requests"] != 20 {
		t.Errorf("Expected ses_requests=20, got %d", stats["ses_requests"])
	}
	if stats["mailgun_requests"] != 5 {
		t.Errorf("Expected mailgun_requests=5, got %d", stats["mailgun_requests"])
	}
	if stats["sendgrid_requests"] != 15 {
		t.Errorf("Expected sendgrid_requests=15, got %d", stats["sendgrid_requests"])
	}
}

// =============================================================================
// CONFIG TESTS
// =============================================================================

func TestDefaultConfig(t *testing.T) {
	config := DefaultConfig()
	
	if config.TargetMessagesPerDay != 50_000_000 {
		t.Errorf("Expected TargetMessagesPerDay=50000000, got %d", config.TargetMessagesPerDay)
	}
	
	if config.TestDurationMinutes != 5 {
		t.Errorf("Expected TestDurationMinutes=5, got %d", config.TestDurationMinutes)
	}
	
	if config.SimulatedCampaigns != 10 {
		t.Errorf("Expected SimulatedCampaigns=10, got %d", config.SimulatedCampaigns)
	}
	
	// Check ESP distribution adds up to 1.0
	var total float64
	for _, weight := range config.ESPDistribution {
		total += weight
	}
	if total != 1.0 {
		t.Errorf("Expected ESP distribution to sum to 1.0, got %f", total)
	}
	
	if config.MockMode != true {
		t.Error("Expected MockMode=true by default")
	}
}

func TestSelectESP(t *testing.T) {
	distribution := map[string]float64{
		"sparkpost": 0.30,
		"ses":       0.50,
		"mailgun":   0.10,
		"sendgrid":  0.10,
	}
	
	// Run many selections and check distribution is roughly correct
	counts := make(map[string]int)
	iterations := 10000
	
	for i := 0; i < iterations; i++ {
		esp := selectESP(distribution)
		counts[esp]++
	}
	
	// Check each ESP is selected roughly according to weight (within 5%)
	for esp, weight := range distribution {
		expected := float64(iterations) * weight
		actual := float64(counts[esp])
		tolerance := expected * 0.1 // 10% tolerance
		
		if actual < expected-tolerance || actual > expected+tolerance {
			t.Errorf("ESP %s: expected ~%.0f selections, got %d", esp, expected, counts[esp])
		}
	}
}

// =============================================================================
// REPORT GENERATION TESTS
// =============================================================================

func TestGenerateReport(t *testing.T) {
	config := DefaultConfig()
	runner := NewLoadTestRunner(config)
	
	// Setup mock metrics
	runner.metrics.TestStartTime = time.Now().Add(-5 * time.Minute)
	runner.metrics.TestEndTime = time.Now()
	runner.metrics.TotalEnqueued = 15000000
	runner.metrics.TotalSent = 183720
	runner.metrics.EnqueueRatePerSecond = 50000
	runner.metrics.SendRatePerSecond = 612.4
	runner.metrics.EnqueueLatencyP50 = 2300 * time.Microsecond
	runner.metrics.EnqueueLatencyP99 = 15200 * time.Microsecond
	runner.metrics.SendLatencyP50 = 45 * time.Millisecond
	runner.metrics.SendLatencyP99 = 95 * time.Millisecond
	runner.metrics.TotalRateLimitHits = 23
	runner.metrics.ProjectedDailyCapacity = 52909280
	runner.metrics.BottleneckComponent = "None identified"
	runner.metrics.HeadroomPercent = 5.8
	
	// Add ESP metrics
	runner.metrics.ESPMetrics["sparkpost"].TotalSent = 55116
	runner.metrics.ESPMetrics["sparkpost"].SendRate = 183.7
	runner.metrics.ESPMetrics["sparkpost"].AvgBatchSize = 1847
	runner.metrics.ESPMetrics["ses"].TotalSent = 91860
	runner.metrics.ESPMetrics["ses"].SendRate = 306.2
	runner.metrics.ESPMetrics["ses"].AvgBatchSize = 48
	runner.metrics.ESPMetrics["mailgun"].TotalSent = 18372
	runner.metrics.ESPMetrics["mailgun"].SendRate = 61.2
	runner.metrics.ESPMetrics["mailgun"].AvgBatchSize = 923
	runner.metrics.ESPMetrics["sendgrid"].TotalSent = 18372
	runner.metrics.ESPMetrics["sendgrid"].SendRate = 61.3
	runner.metrics.ESPMetrics["sendgrid"].AvgBatchSize = 912
	
	// Add phase results
	runner.metrics.PhaseResults["QUEUE_STRESS"] = &PhaseResult{
		Name:   "QUEUE_STRESS",
		Status: "PASS",
		Details: map[string]interface{}{
			"actual_rate": 52341.0,
			"queue_depth": int64(1245000),
		},
	}
	runner.metrics.PhaseResults["SEND_WORKER"] = &PhaseResult{
		Name:   "SEND_WORKER",
		Status: "PASS",
		Details: map[string]interface{}{
			"actual_rate":      612.4,
			"batch_efficiency": 94.2,
		},
	}
	runner.metrics.PhaseResults["RATE_LIMITER"] = &PhaseResult{
		Name:   "RATE_LIMITER",
		Status: "PASS",
	}
	runner.metrics.PhaseResults["END_TO_END"] = &PhaseResult{
		Name:   "END_TO_END",
		Status: "PASS",
		Details: map[string]interface{}{
			"total_processed":   int64(183720),
			"projected_daily":   int64(52909280),
			"headroom_percent":  5.8,
		},
		Duration: 5 * time.Minute,
	}
	
	report := runner.GenerateReport()
	
	// Check report contains expected sections
	expectedSections := []string{
		"50M/DAY LOAD TEST REPORT",
		"PHASE 1: QUEUE STRESS TEST",
		"PHASE 2: SEND WORKER THROUGHPUT",
		"PHASE 3: RATE LIMITER",
		"PHASE 4: END-TO-END",
		"OVERALL RESULT",
	}
	
	for _, section := range expectedSections {
		if !strings.Contains(report, section) {
			t.Errorf("Report missing section: %s", section)
		}
	}
	
	// Check report contains key metrics
	if !strings.Contains(report, "50,000,000 messages/day") && !strings.Contains(report, "50000000") {
		t.Error("Report should contain target messages per day")
	}
	
	if !strings.Contains(report, "PASS") {
		t.Error("Report should contain PASS status")
	}
}

func TestGenerateReportFailure(t *testing.T) {
	config := DefaultConfig()
	runner := NewLoadTestRunner(config)
	
	// Setup failing metrics
	runner.metrics.TestStartTime = time.Now().Add(-5 * time.Minute)
	runner.metrics.TestEndTime = time.Now()
	runner.metrics.ProjectedDailyCapacity = 30000000 // Below target
	runner.metrics.BottleneckComponent = "Send Workers"
	runner.metrics.TotalErrors = 1000
	
	runner.metrics.PhaseResults["END_TO_END"] = &PhaseResult{
		Name:   "END_TO_END",
		Status: "FAIL",
		Details: map[string]interface{}{
			"total_processed":   int64(100000),
			"projected_daily":   int64(30000000),
			"headroom_percent":  -40.0,
		},
		Duration: 5 * time.Minute,
	}
	
	report := runner.GenerateReport()
	
	if !strings.Contains(report, "FAIL") {
		t.Error("Report should contain FAIL for below-target performance")
	}
	
	if !strings.Contains(report, "Recommendations") {
		t.Error("Failed report should contain recommendations")
	}
}

// =============================================================================
// CONCURRENT ACCESS TESTS
// =============================================================================

func TestMetricsConcurrentAccess(t *testing.T) {
	m := NewLoadTestMetrics()
	
	var wg sync.WaitGroup
	numGoroutines := 100
	opsPerGoroutine := 1000
	
	// Concurrent enqueue recording
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				m.RecordEnqueue(1, time.Millisecond, nil)
			}
		}()
	}
	
	// Concurrent send recording
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				esp := []string{"sparkpost", "ses", "mailgun", "sendgrid"}[j%4]
				m.RecordSend(esp, 10, time.Millisecond, nil)
			}
		}()
	}
	
	// Concurrent rate limit recording
	for i := 0; i < numGoroutines/10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine/10; j++ {
				m.RecordRateLimitHit("sparkpost", 100)
			}
		}()
	}
	
	wg.Wait()
	
	// Verify counts are correct
	expectedEnqueued := int64(numGoroutines * opsPerGoroutine)
	if m.TotalEnqueued != expectedEnqueued {
		t.Errorf("Expected TotalEnqueued=%d, got %d", expectedEnqueued, m.TotalEnqueued)
	}
	
	expectedSent := int64(numGoroutines * opsPerGoroutine * 10)
	if m.TotalSent != expectedSent {
		t.Errorf("Expected TotalSent=%d, got %d", expectedSent, m.TotalSent)
	}
	
	expectedRateLimitHits := int64(numGoroutines / 10 * opsPerGoroutine / 10)
	if m.TotalRateLimitHits != expectedRateLimitHits {
		t.Errorf("Expected TotalRateLimitHits=%d, got %d", expectedRateLimitHits, m.TotalRateLimitHits)
	}
}

// =============================================================================
// PHASE RESULT TESTS
// =============================================================================

func TestPhaseResult(t *testing.T) {
	result := &PhaseResult{
		Name:      "TEST_PHASE",
		Status:    "PASS",
		StartTime: time.Now(),
		EndTime:   time.Now().Add(time.Minute),
		Duration:  time.Minute,
		Details: map[string]interface{}{
			"key1": "value1",
			"key2": 123,
		},
	}
	
	if result.Name != "TEST_PHASE" {
		t.Errorf("Expected Name='TEST_PHASE', got '%s'", result.Name)
	}
	
	if result.Status != "PASS" {
		t.Errorf("Expected Status='PASS', got '%s'", result.Status)
	}
	
	if result.Duration != time.Minute {
		t.Errorf("Expected Duration=1m, got %v", result.Duration)
	}
	
	if result.Details["key1"] != "value1" {
		t.Errorf("Expected Details['key1']='value1', got '%v'", result.Details["key1"])
	}
}

// =============================================================================
// HELPER FUNCTIONS
// =============================================================================

var errTest = testError("test error")

type testError string

func (e testError) Error() string {
	return string(e)
}

// =============================================================================
// BENCHMARK TESTS
// =============================================================================

func BenchmarkRecordEnqueue(b *testing.B) {
	m := NewLoadTestMetrics()
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.RecordEnqueue(100, time.Millisecond, nil)
	}
}

func BenchmarkRecordSend(b *testing.B) {
	m := NewLoadTestMetrics()
	esps := []string{"sparkpost", "ses", "mailgun", "sendgrid"}
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.RecordSend(esps[i%4], 100, time.Millisecond, nil)
	}
}

func BenchmarkPercentile(b *testing.B) {
	// Create a slice with 100k latencies
	latencies := make([]time.Duration, 100000)
	for i := 0; i < 100000; i++ {
		latencies[i] = time.Duration(i) * time.Microsecond
	}
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		percentile(latencies, 99)
	}
}

func BenchmarkSelectESP(b *testing.B) {
	distribution := map[string]float64{
		"sparkpost": 0.30,
		"ses":       0.50,
		"mailgun":   0.10,
		"sendgrid":  0.10,
	}
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		selectESP(distribution)
	}
}

// =============================================================================
// INTEGRATION TEST HELPERS
// =============================================================================

// TestLoadTestRunnerInitialize tests initialization without actual connections
func TestLoadTestRunnerInitialize(t *testing.T) {
	config := DefaultConfig()
	config.PostgresURL = ""  // No DB
	config.RedisURL = ""     // No Redis
	
	runner := NewLoadTestRunner(config)
	
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	err := runner.Initialize(ctx)
	if err != nil {
		t.Errorf("Initialize should succeed without DB/Redis: %v", err)
	}
	
	// Mock server should be running
	if runner.mockServer == nil {
		t.Error("Mock server should be initialized")
	}
	
	// Cleanup
	runner.Cleanup()
}
