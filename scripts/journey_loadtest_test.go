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

func TestNewJourneyTestMetrics(t *testing.T) {
	m := NewJourneyTestMetrics()

	if m.NodeProcessingTimes == nil {
		t.Error("NodeProcessingTimes should not be nil")
	}

	if m.NodeAvgTimes == nil {
		t.Error("NodeAvgTimes should not be nil")
	}

	if m.NodeErrorRates == nil {
		t.Error("NodeErrorRates should not be nil")
	}

	if m.ErrorsByType == nil {
		t.Error("ErrorsByType should not be nil")
	}

	if m.PhaseResults == nil {
		t.Error("PhaseResults should not be nil")
	}

	if m.EnrollmentLatencies == nil {
		t.Error("EnrollmentLatencies should not be nil")
	}
}

func TestRecordEnrollment(t *testing.T) {
	m := NewJourneyTestMetrics()

	// Record successful enrollment
	m.RecordEnrollment(10*time.Millisecond, nil)

	if m.EnrollmentsSucceeded != 1 {
		t.Errorf("Expected EnrollmentsSucceeded=1, got %d", m.EnrollmentsSucceeded)
	}

	if m.EnrollmentsAttempted != 1 {
		t.Errorf("Expected EnrollmentsAttempted=1, got %d", m.EnrollmentsAttempted)
	}

	if len(m.EnrollmentLatencies) != 1 {
		t.Errorf("Expected 1 latency record, got %d", len(m.EnrollmentLatencies))
	}

	// Record failed enrollment
	m.RecordEnrollment(5*time.Millisecond, errJourneyTest)

	if m.EnrollmentsAttempted != 2 {
		t.Errorf("Expected EnrollmentsAttempted=2, got %d", m.EnrollmentsAttempted)
	}

	if m.TotalErrors != 1 {
		t.Errorf("Expected TotalErrors=1, got %d", m.TotalErrors)
	}

	if m.ErrorsByType["enrollment"] != 1 {
		t.Errorf("Expected ErrorsByType[enrollment]=1, got %d", m.ErrorsByType["enrollment"])
	}

	// EnrollmentsSucceeded should not change on error
	if m.EnrollmentsSucceeded != 1 {
		t.Errorf("EnrollmentsSucceeded should remain 1, got %d", m.EnrollmentsSucceeded)
	}
}

func TestRecordExecution(t *testing.T) {
	m := NewJourneyTestMetrics()

	// Record successful execution
	m.RecordExecution(50*time.Millisecond, nil)

	if m.ExecutionsSucceeded != 1 {
		t.Errorf("Expected ExecutionsSucceeded=1, got %d", m.ExecutionsSucceeded)
	}

	if len(m.ExecutionLatencies) != 1 {
		t.Errorf("Expected 1 latency record, got %d", len(m.ExecutionLatencies))
	}

	// Record failed execution
	m.RecordExecution(30*time.Millisecond, errJourneyTest)

	if m.ExecutionsAttempted != 2 {
		t.Errorf("Expected ExecutionsAttempted=2, got %d", m.ExecutionsAttempted)
	}

	if m.TotalErrors != 1 {
		t.Errorf("Expected TotalErrors=1, got %d", m.TotalErrors)
	}

	if m.ErrorsByType["execution"] != 1 {
		t.Errorf("Expected ErrorsByType[execution]=1, got %d", m.ErrorsByType["execution"])
	}
}

func TestRecordNodeProcessing(t *testing.T) {
	m := NewJourneyTestMetrics()

	// Record successful node processing for different types
	nodeTypes := []string{"email", "delay", "condition", "split", "goal"}
	for _, nt := range nodeTypes {
		m.RecordNodeProcessing(nt, time.Duration(10+len(nt))*time.Millisecond, nil)
	}

	if len(m.NodeProcessingTimes) != 5 {
		t.Errorf("Expected 5 node types, got %d", len(m.NodeProcessingTimes))
	}

	// Check individual node metrics
	if m.NodeSuccessCounts["email"] != 1 {
		t.Errorf("Expected email success count=1, got %d", m.NodeSuccessCounts["email"])
	}

	if len(m.NodeProcessingTimes["email"]) != 1 {
		t.Errorf("Expected 1 email latency record, got %d", len(m.NodeProcessingTimes["email"]))
	}

	// Record error
	m.RecordNodeProcessing("email", 100*time.Millisecond, errJourneyTest)

	if m.NodeErrorCounts["email"] != 1 {
		t.Errorf("Expected email error count=1, got %d", m.NodeErrorCounts["email"])
	}

	if m.TotalErrors != 1 {
		t.Errorf("Expected TotalErrors=1, got %d", m.TotalErrors)
	}

	if m.ErrorsByType["node_email"] != 1 {
		t.Errorf("Expected ErrorsByType[node_email]=1, got %d", m.ErrorsByType["node_email"])
	}
}

func TestRecordEmailSend(t *testing.T) {
	m := NewJourneyTestMetrics()

	// Record successful email sends
	for i := 0; i < 100; i++ {
		m.RecordEmailSend(time.Duration(30+i%20)*time.Millisecond, nil)
	}

	if m.EmailsSent != 100 {
		t.Errorf("Expected EmailsSent=100, got %d", m.EmailsSent)
	}

	if len(m.EmailLatencies) != 100 {
		t.Errorf("Expected 100 email latency records, got %d", len(m.EmailLatencies))
	}

	// Record error
	m.RecordEmailSend(50*time.Millisecond, errJourneyTest)

	if m.TotalErrors != 1 {
		t.Errorf("Expected TotalErrors=1, got %d", m.TotalErrors)
	}

	if m.ErrorsByType["email"] != 1 {
		t.Errorf("Expected ErrorsByType[email]=1, got %d", m.ErrorsByType["email"])
	}
}

func TestRecordCompletion(t *testing.T) {
	m := NewJourneyTestMetrics()

	// Record completions (some converted, some not)
	for i := 0; i < 100; i++ {
		m.RecordCompletion(i%3 == 0) // ~33% conversion
	}

	if m.TotalCompleted != 100 {
		t.Errorf("Expected TotalCompleted=100, got %d", m.TotalCompleted)
	}

	// 33, 66, 99 divisible by 3 -> 34 converted
	if m.TotalConverted != 34 {
		t.Errorf("Expected TotalConverted=34, got %d", m.TotalConverted)
	}
}

func TestRecordError(t *testing.T) {
	m := NewJourneyTestMetrics()

	m.RecordError("database")
	m.RecordError("database")
	m.RecordError("network")

	if m.TotalErrors != 3 {
		t.Errorf("Expected TotalErrors=3, got %d", m.TotalErrors)
	}

	if m.ErrorsByType["database"] != 2 {
		t.Errorf("Expected ErrorsByType[database]=2, got %d", m.ErrorsByType["database"])
	}

	if m.ErrorsByType["network"] != 1 {
		t.Errorf("Expected ErrorsByType[network]=1, got %d", m.ErrorsByType["network"])
	}
}

func TestJourneyPercentile(t *testing.T) {
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
			expected: 19,
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
			result := journeyPercentile(tt.values, tt.p)
			if result != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestJourneyMetricsFinalize(t *testing.T) {
	m := NewJourneyTestMetrics()
	config := DefaultJourneyConfig()

	// Simulate test data
	m.TestStartTime = time.Now().Add(-5 * time.Minute)
	m.TestEndTime = time.Now()
	m.EnrollmentsAttempted = 100000
	m.EnrollmentsSucceeded = 99000
	m.ExecutionsAttempted = 95000
	m.ExecutionsSucceeded = 94000
	m.TotalCompleted = 90000
	m.TotalConverted = 30000
	m.EmailsSent = 80000

	// Add latencies
	for i := 0; i < 100; i++ {
		m.EnrollmentLatencies = append(m.EnrollmentLatencies, time.Duration(i)*time.Millisecond)
		m.ExecutionLatencies = append(m.ExecutionLatencies, time.Duration(i)*time.Millisecond)
		m.EmailLatencies = append(m.EmailLatencies, time.Duration(30+i)*time.Millisecond)
	}

	// Add node processing times
	nodeTypes := []string{"email", "delay", "condition", "split", "goal"}
	for _, nt := range nodeTypes {
		m.NodeSuccessCounts[nt] = 1000
		m.NodeErrorCounts[nt] = 10
		m.NodeProcessingTimes[nt] = make([]time.Duration, 100)
		for i := 0; i < 100; i++ {
			m.NodeProcessingTimes[nt][i] = time.Duration(i) * time.Millisecond
		}
	}

	m.Finalize(config)

	// Check calculations - allow for small timing variations
	if m.TestDuration.Round(time.Second) != 5*time.Minute {
		t.Errorf("Expected TestDuration=~5m, got %v", m.TestDuration)
	}

	expectedEnrollmentRate := float64(m.EnrollmentsSucceeded) / m.TestDuration.Seconds()
	if m.EnrollmentRate != expectedEnrollmentRate {
		t.Errorf("Expected EnrollmentRate=%.2f, got %.2f", expectedEnrollmentRate, m.EnrollmentRate)
	}

	// Check percentiles were calculated
	if m.EnrollmentLatencyP50 == 0 {
		t.Error("EnrollmentLatencyP50 should not be 0")
	}

	if m.ExecutionLatencyP99 == 0 {
		t.Error("ExecutionLatencyP99 should not be 0")
	}

	if m.EmailLatencyP50 == 0 {
		t.Error("EmailLatencyP50 should not be 0")
	}

	// Check node average times
	for _, nt := range nodeTypes {
		if m.NodeAvgTimes[nt] == 0 {
			t.Errorf("NodeAvgTimes[%s] should not be 0", nt)
		}
	}

	// Check completion rates
	expectedCompletionRate := float64(m.TotalCompleted) / float64(m.EnrollmentsSucceeded) * 100
	if m.JourneyCompletionRate != expectedCompletionRate {
		t.Errorf("Expected JourneyCompletionRate=%.2f, got %.2f", expectedCompletionRate, m.JourneyCompletionRate)
	}

	// Check projected daily capacity
	if m.ProjectedDailyCapacity == 0 {
		t.Error("ProjectedDailyCapacity should not be 0")
	}
}

func TestIdentifyJourneyBottleneck(t *testing.T) {
	config := DefaultJourneyConfig()

	tests := []struct {
		name     string
		setup    func(*JourneyTestMetrics)
		expected string
	}{
		{
			name: "enrollment bottleneck",
			setup: func(m *JourneyTestMetrics) {
				targetRate := float64(config.TargetEnrollments) / config.Duration.Seconds()
				m.EnrollmentRate = targetRate * 0.5
				m.ExecutionRate = targetRate
			},
			expected: "Enrollment Rate (PostgreSQL)",
		},
		{
			name: "execution bottleneck",
			setup: func(m *JourneyTestMetrics) {
				targetRate := float64(config.TargetEnrollments) / config.Duration.Seconds()
				m.EnrollmentRate = targetRate
				m.ExecutionRate = targetRate * 0.5
			},
			expected: "Execution Workers",
		},
		{
			name: "email latency bottleneck",
			setup: func(m *JourneyTestMetrics) {
				targetRate := float64(config.TargetEnrollments) / config.Duration.Seconds()
				m.EnrollmentRate = targetRate
				m.ExecutionRate = targetRate
				m.EmailLatencyP99 = 300 * time.Millisecond
			},
			expected: "Email Node (ESP Latency)",
		},
		{
			name: "error rate bottleneck",
			setup: func(m *JourneyTestMetrics) {
				targetRate := float64(config.TargetEnrollments) / config.Duration.Seconds()
				m.EnrollmentRate = targetRate
				m.ExecutionRate = targetRate
				m.ExecutionsSucceeded = 10000
				m.TotalErrors = 500 // 5% error rate
			},
			expected: "High Error Rate",
		},
		{
			name: "no bottleneck",
			setup: func(m *JourneyTestMetrics) {
				targetRate := float64(config.TargetEnrollments) / config.Duration.Seconds()
				m.EnrollmentRate = targetRate
				m.ExecutionRate = targetRate
				m.ExecutionsSucceeded = 10000
				m.TotalErrors = 10
			},
			expected: "None identified",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewJourneyTestMetrics()
			tt.setup(m)
			result := identifyJourneyBottleneck(m, config)
			if result != tt.expected {
				t.Errorf("Expected bottleneck '%s', got '%s'", tt.expected, result)
			}
		})
	}
}

// =============================================================================
// MOCK EMAIL SERVER TESTS
// =============================================================================

func TestMockEmailServerSend(t *testing.T) {
	m := NewJourneyTestMetrics()
	server := NewMockEmailServer(0, m, 10*time.Millisecond)

	handler := http.HandlerFunc(server.handleSend)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Test successful send
	payload := `{"to":"test@test.com","subject":"Test","body":"<p>Test</p>"}`
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	var result struct {
		Success   bool   `json:"success"`
		MessageID string `json:"message_id"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	if !result.Success {
		t.Error("Expected success=true")
	}

	if result.MessageID == "" {
		t.Error("Expected message_id in response")
	}
}

func TestMockEmailServerStats(t *testing.T) {
	m := NewJourneyTestMetrics()
	server := NewMockEmailServer(0, m, 1*time.Millisecond)

	// Simulate some requests
	server.totalRequests = 100
	server.totalErrors = 2

	stats := server.Stats()

	if stats["total_requests"] != 100 {
		t.Errorf("Expected total_requests=100, got %d", stats["total_requests"])
	}
	if stats["total_errors"] != 2 {
		t.Errorf("Expected total_errors=2, got %d", stats["total_errors"])
	}
}

// =============================================================================
// CONFIG TESTS
// =============================================================================

func TestDefaultJourneyConfig(t *testing.T) {
	config := DefaultJourneyConfig()

	if config.TestType != "all" {
		t.Errorf("Expected TestType='all', got '%s'", config.TestType)
	}

	if config.Duration != 5*time.Minute {
		t.Errorf("Expected Duration=5m, got %v", config.Duration)
	}

	if config.TargetEnrollments != 100_000 {
		t.Errorf("Expected TargetEnrollments=100000, got %d", config.TargetEnrollments)
	}

	if config.Workers != 8 {
		t.Errorf("Expected Workers=8, got %d", config.Workers)
	}

	if config.JourneyComplexity != "medium" {
		t.Errorf("Expected JourneyComplexity='medium', got '%s'", config.JourneyComplexity)
	}

	if config.SpikeMultiplier != 10.0 {
		t.Errorf("Expected SpikeMultiplier=10.0, got %f", config.SpikeMultiplier)
	}

	if config.MockMode != true {
		t.Error("Expected MockMode=true by default")
	}
}

// =============================================================================
// JOURNEY BUILDER TESTS
// =============================================================================

func TestBuildSimpleJourney(t *testing.T) {
	config := DefaultJourneyConfig()
	config.JourneyComplexity = "simple"

	runner := NewJourneyLoadTest(config)
	nodes := runner.buildSimpleJourney()

	if len(nodes) != 3 {
		t.Errorf("Expected 3 nodes in simple journey, got %d", len(nodes))
	}

	// Check node types
	expectedTypes := []string{"trigger", "email", "goal"}
	for i, expected := range expectedTypes {
		if nodes[i].Type != expected {
			t.Errorf("Expected node %d type='%s', got '%s'", i, expected, nodes[i].Type)
		}
	}
}

func TestBuildMediumJourney(t *testing.T) {
	config := DefaultJourneyConfig()
	config.JourneyComplexity = "medium"

	runner := NewJourneyLoadTest(config)
	nodes := runner.buildMediumJourney()

	if len(nodes) != 6 {
		t.Errorf("Expected 6 nodes in medium journey, got %d", len(nodes))
	}

	// Check that it has all required node types
	typeCount := make(map[string]int)
	for _, node := range nodes {
		typeCount[node.Type]++
	}

	if typeCount["trigger"] != 1 {
		t.Error("Medium journey should have 1 trigger")
	}
	if typeCount["email"] != 2 {
		t.Error("Medium journey should have 2 emails")
	}
	if typeCount["delay"] != 1 {
		t.Error("Medium journey should have 1 delay")
	}
	if typeCount["condition"] != 1 {
		t.Error("Medium journey should have 1 condition")
	}
	if typeCount["goal"] != 1 {
		t.Error("Medium journey should have 1 goal")
	}
}

func TestBuildComplexJourney(t *testing.T) {
	config := DefaultJourneyConfig()
	config.JourneyComplexity = "complex"

	runner := NewJourneyLoadTest(config)
	nodes := runner.buildComplexJourney()

	if len(nodes) != 10 {
		t.Errorf("Expected 10 nodes in complex journey, got %d", len(nodes))
	}

	// Check that it has a split node
	hasSplit := false
	for _, node := range nodes {
		if node.Type == "split" {
			hasSplit = true
			break
		}
	}
	if !hasSplit {
		t.Error("Complex journey should have a split node")
	}
}

func TestBuildConnections(t *testing.T) {
	config := DefaultJourneyConfig()
	runner := NewJourneyLoadTest(config)

	nodes := []TestJourneyNode{
		{ID: "node1", Type: "trigger", Connections: []string{"node2"}},
		{ID: "node2", Type: "email", Connections: []string{"node3"}},
		{ID: "node3", Type: "condition", Connections: []string{"node4", "node5"}},
		{ID: "node4", Type: "email", Connections: []string{"node6"}},
		{ID: "node5", Type: "goal", Connections: []string{}},
		{ID: "node6", Type: "goal", Connections: []string{}},
	}

	connections := runner.buildConnections(nodes)

	if len(connections) != 5 {
		t.Errorf("Expected 5 connections, got %d", len(connections))
	}

	// Check condition labels
	for _, conn := range connections {
		if conn.From == "node3" {
			if conn.To == "node4" && conn.Label != "true" {
				t.Error("First condition branch should be labeled 'true'")
			}
			if conn.To == "node5" && conn.Label != "false" {
				t.Error("Second condition branch should be labeled 'false'")
			}
		}
	}
}

// =============================================================================
// PHASE RESULT TESTS
// =============================================================================

func TestJourneyPhaseResult(t *testing.T) {
	result := &JourneyPhaseResult{
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
// REPORT GENERATION TESTS
// =============================================================================

func TestGenerateJourneyReport(t *testing.T) {
	config := DefaultJourneyConfig()
	runner := NewJourneyLoadTest(config)

	// Setup mock metrics
	runner.testNodes = runner.buildMediumJourney()
	runner.metrics.TestStartTime = time.Now().Add(-5 * time.Minute)
	runner.metrics.TestEndTime = time.Now()
	runner.metrics.EnrollmentsAttempted = 100000
	runner.metrics.EnrollmentsSucceeded = 99500
	runner.metrics.EnrollmentRate = 35420
	runner.metrics.EnrollmentLatencyP50 = 2100 * time.Microsecond
	runner.metrics.EnrollmentLatencyP99 = 12300 * time.Microsecond
	runner.metrics.ExecutionsAttempted = 98500
	runner.metrics.ExecutionsSucceeded = 98000
	runner.metrics.ExecutionRate = 8420
	runner.metrics.TotalCompleted = 95200
	runner.metrics.TotalConverted = 28560
	runner.metrics.JourneyCompletionRate = 96.7
	runner.metrics.JourneyConversionRate = 29.0
	runner.metrics.EmailsSent = 80000
	runner.metrics.EmailsPerSecond = 1500
	runner.metrics.EmailLatencyP50 = 45 * time.Millisecond
	runner.metrics.EmailLatencyP99 = 95 * time.Millisecond
	runner.metrics.ProjectedDailyCapacity = 3_000_000
	runner.metrics.HeadroomPercent = 42.0
	runner.metrics.BottleneckComponent = "None identified"

	// Add node metrics
	nodeTypes := []string{"email", "delay", "condition", "split", "goal"}
	for _, nt := range nodeTypes {
		runner.metrics.NodeAvgTimes[nt] = time.Duration(10+len(nt)) * time.Millisecond
		runner.metrics.NodeSuccessCounts[nt] = 1000
		runner.metrics.NodeErrorRates[nt] = 1.5
	}

	// Add phase results
	runner.metrics.PhaseResults["enrollment"] = &JourneyPhaseResult{
		Name:   "enrollment",
		Status: "PASS",
		Details: map[string]interface{}{
			"actual_rate": 35420.0,
		},
	}
	runner.metrics.PhaseResults["execution"] = &JourneyPhaseResult{
		Name:   "execution",
		Status: "PASS",
		Details: map[string]interface{}{
			"execution_rate": 8420.0,
		},
	}
	runner.metrics.PhaseResults["node"] = &JourneyPhaseResult{
		Name:   "node",
		Status: "PASS",
	}
	runner.metrics.PhaseResults["email"] = &JourneyPhaseResult{
		Name:   "email",
		Status: "PASS",
	}

	report := runner.GenerateReport()

	// Check report contains expected sections
	expectedSections := []string{
		"JOURNEY LOAD TEST REPORT",
		"Test Configuration:",
		"ENROLLMENT PERFORMANCE",
		"EXECUTION PERFORMANCE",
		"NODE PERFORMANCE",
		"EMAIL PERFORMANCE",
		"CAPACITY PROJECTIONS",
		"OVERALL:",
	}

	for _, section := range expectedSections {
		if !strings.Contains(report, section) {
			t.Errorf("Report missing section: %s", section)
		}
	}

	if !strings.Contains(report, "PASS") {
		t.Error("Report should contain PASS status")
	}
}

func TestGenerateJourneyReportFailure(t *testing.T) {
	config := DefaultJourneyConfig()
	runner := NewJourneyLoadTest(config)

	// Setup failing metrics
	runner.testNodes = runner.buildSimpleJourney()
	runner.metrics.TestStartTime = time.Now().Add(-5 * time.Minute)
	runner.metrics.TestEndTime = time.Now()
	runner.metrics.ProjectedDailyCapacity = 500000 // Below target
	runner.metrics.BottleneckComponent = "Execution Workers"
	runner.metrics.HeadroomPercent = -40.0
	runner.metrics.TotalErrors = 1000

	runner.metrics.PhaseResults["enrollment"] = &JourneyPhaseResult{
		Name:   "enrollment",
		Status: "FAIL",
		Details: map[string]interface{}{
			"actual_rate": 5000.0,
		},
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

func TestJourneyMetricsConcurrentAccess(t *testing.T) {
	m := NewJourneyTestMetrics()

	var wg sync.WaitGroup
	numGoroutines := 100
	opsPerGoroutine := 1000

	// Concurrent enrollment recording
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				m.RecordEnrollment(time.Millisecond, nil)
			}
		}()
	}

	// Concurrent execution recording
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				m.RecordExecution(time.Millisecond, nil)
			}
		}()
	}

	// Concurrent node processing recording
	for i := 0; i < numGoroutines/10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nodeTypes := []string{"email", "delay", "condition", "split", "goal"}
			for j := 0; j < opsPerGoroutine/10; j++ {
				m.RecordNodeProcessing(nodeTypes[j%5], time.Millisecond, nil)
			}
		}()
	}

	wg.Wait()

	// Verify counts are correct
	expectedEnrollments := int64(numGoroutines * opsPerGoroutine)
	if m.EnrollmentsSucceeded != expectedEnrollments {
		t.Errorf("Expected EnrollmentsSucceeded=%d, got %d", expectedEnrollments, m.EnrollmentsSucceeded)
	}

	expectedExecutions := int64(numGoroutines * opsPerGoroutine)
	if m.ExecutionsSucceeded != expectedExecutions {
		t.Errorf("Expected ExecutionsSucceeded=%d, got %d", expectedExecutions, m.ExecutionsSucceeded)
	}
}

// =============================================================================
// NODE PROCESSING TESTS
// =============================================================================

func TestProcessTriggerNode(t *testing.T) {
	config := DefaultJourneyConfig()
	runner := NewJourneyLoadTest(config)

	err := runner.processTriggerNode(context.Background())
	if err != nil {
		t.Errorf("Trigger node should not return error: %v", err)
	}
}

func TestProcessDelayNode(t *testing.T) {
	config := DefaultJourneyConfig()
	runner := NewJourneyLoadTest(config)

	start := time.Now()
	err := runner.processDelayNode(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("Delay node should not return error: %v", err)
	}

	// Should be very fast (< 1ms)
	if elapsed > time.Millisecond {
		t.Errorf("Delay node took too long: %v", elapsed)
	}
}

func TestProcessConditionNode(t *testing.T) {
	config := DefaultJourneyConfig()
	runner := NewJourneyLoadTest(config)

	err := runner.processConditionNode(context.Background())
	if err != nil {
		t.Errorf("Condition node should not return error: %v", err)
	}
}

func TestProcessSplitNode(t *testing.T) {
	config := DefaultJourneyConfig()
	runner := NewJourneyLoadTest(config)

	err := runner.processSplitNode(context.Background())
	if err != nil {
		t.Errorf("Split node should not return error: %v", err)
	}
}

func TestProcessGoalNode(t *testing.T) {
	config := DefaultJourneyConfig()
	runner := NewJourneyLoadTest(config)

	err := runner.processGoalNode(context.Background())
	if err != nil {
		t.Errorf("Goal node should not return error: %v", err)
	}
}

// =============================================================================
// INTEGRATION TEST HELPERS
// =============================================================================

func TestJourneyLoadTestInitialize(t *testing.T) {
	config := DefaultJourneyConfig()
	config.PostgresURL = "" // No DB
	config.RedisURL = ""    // No Redis

	runner := NewJourneyLoadTest(config)

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

// =============================================================================
// BENCHMARK TESTS
// =============================================================================

func BenchmarkRecordEnrollment(b *testing.B) {
	m := NewJourneyTestMetrics()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.RecordEnrollment(time.Millisecond, nil)
	}
}

func BenchmarkRecordExecution(b *testing.B) {
	m := NewJourneyTestMetrics()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.RecordExecution(time.Millisecond, nil)
	}
}

func BenchmarkRecordNodeProcessing(b *testing.B) {
	m := NewJourneyTestMetrics()
	nodeTypes := []string{"email", "delay", "condition", "split", "goal"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.RecordNodeProcessing(nodeTypes[i%5], time.Millisecond, nil)
	}
}

func BenchmarkJourneyPercentile(b *testing.B) {
	// Create a slice with 100k latencies
	latencies := make([]time.Duration, 100000)
	for i := 0; i < 100000; i++ {
		latencies[i] = time.Duration(i) * time.Microsecond
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		journeyPercentile(latencies, 99)
	}
}

func BenchmarkBuildConnections(b *testing.B) {
	config := DefaultJourneyConfig()
	runner := NewJourneyLoadTest(config)
	nodes := runner.buildComplexJourney()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runner.buildConnections(nodes)
	}
}

// =============================================================================
// HELPER FUNCTIONS
// =============================================================================

var errJourneyTest = journeyTestError("test error")

type journeyTestError string

func (e journeyTestError) Error() string {
	return string(e)
}
