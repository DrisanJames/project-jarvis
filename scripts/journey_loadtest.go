//go:build ignore
// +build ignore

// Journey Load Test - Validates mass traffic handling for the Journey system
//
// Location: /Users/mrjames/Desktop/jamesventures/mailing-saas/upside-down/scripts/journey_loadtest.go
//
// Test Scenarios:
// 1. Mass Enrollment Test - Enroll 1M subscribers into journey
// 2. Concurrent Execution Test - Process 100K+ enrollments simultaneously
// 3. Node Throughput Test - Measure node processing rate
// 4. Segment-Driven Test - Enroll entire segment and track completion
// 5. Sustained Load Test - Run at production rate for extended period
// 6. Spike Test - Handle sudden surge of enrollments
// 7. Email Node Stress Test - Validate email sending under load
//
// Usage:
//
//	go run scripts/journey_loadtest.go \
//	  --postgres="postgres://user:pass@localhost:5432/mailing" \
//	  --redis="localhost:6379" \
//	  --test=all \
//	  --duration=5m \
//	  --enrollments=100000 \
//	  --workers=8
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

// =============================================================================
// CONFIGURATION
// =============================================================================

// JourneyLoadTestConfig defines the test configuration
type JourneyLoadTestConfig struct {
	PostgresURL       string
	RedisURL          string
	TestType          string // all, enrollment, execution, node, segment, sustained, spike, email
	Duration          time.Duration
	TargetEnrollments int64
	Workers           int
	JourneyComplexity string  // simple, medium, complex
	SegmentSize       int64
	SpikeMultiplier   float64

	// Mock ESP settings
	MockESPPort   int
	MockESPDelay  time.Duration
	MockMode      bool

	// Calculated values
	targetEnrollmentsPerSecond float64
	testDuration               time.Duration
}

// DefaultJourneyConfig returns sensible defaults for journey testing
func DefaultJourneyConfig() *JourneyLoadTestConfig {
	return &JourneyLoadTestConfig{
		TestType:          "all",
		Duration:          5 * time.Minute,
		TargetEnrollments: 100_000,
		Workers:           8,
		JourneyComplexity: "medium",
		SegmentSize:       100_000,
		SpikeMultiplier:   10.0,
		MockESPPort:       9998,
		MockESPDelay:      50 * time.Millisecond,
		MockMode:          true,
	}
}

// =============================================================================
// METRICS COLLECTION
// =============================================================================

// JourneyTestMetrics holds all collected metrics
type JourneyTestMetrics struct {
	// Test info
	TestStartTime time.Time
	TestEndTime   time.Time
	TestDuration  time.Duration

	// Enrollment metrics
	EnrollmentsAttempted int64
	EnrollmentsSucceeded int64
	EnrollmentRate       float64 // per second
	EnrollmentLatencies  []time.Duration
	EnrollmentLatencyP50 time.Duration
	EnrollmentLatencyP99 time.Duration

	// Execution metrics
	ExecutionsAttempted int64
	ExecutionsSucceeded int64
	ExecutionRate       float64 // per second
	ExecutionLatencies  []time.Duration
	ExecutionLatencyP50 time.Duration
	ExecutionLatencyP99 time.Duration

	// Node metrics
	NodeProcessingTimes map[string][]time.Duration
	NodeAvgTimes        map[string]time.Duration
	NodeErrorRates      map[string]float64
	NodeSuccessCounts   map[string]int64
	NodeErrorCounts     map[string]int64

	// Journey completion
	JourneyCompletionRate float64
	JourneyConversionRate float64
	AvgJourneyDuration    time.Duration
	TotalCompleted        int64
	TotalConverted        int64

	// Email metrics (if applicable)
	EmailsSent        int64
	EmailsPerSecond   float64
	EmailErrorRate    float64
	EmailLatencies    []time.Duration
	EmailLatencyP50   time.Duration
	EmailLatencyP99   time.Duration

	// System metrics
	DBConnectionsUsed   int
	RedisOpsPerSecond   float64
	MemoryUsageMB       float64
	PeakMemoryUsageMB   float64

	// Errors
	TotalErrors    int64
	ErrorsByType   map[string]int64

	// Capacity projections
	ProjectedDailyCapacity int64
	HeadroomPercent        float64
	BottleneckComponent    string

	// Phase results
	PhaseResults map[string]*JourneyPhaseResult

	mu sync.Mutex
}

// JourneyPhaseResult holds results for each test phase
type JourneyPhaseResult struct {
	Name      string
	Status    string // "PASS" or "FAIL"
	Duration  time.Duration
	Details   map[string]interface{}
	StartTime time.Time
	EndTime   time.Time
}

// NewJourneyTestMetrics creates a new metrics collector
func NewJourneyTestMetrics() *JourneyTestMetrics {
	return &JourneyTestMetrics{
		NodeProcessingTimes: make(map[string][]time.Duration),
		NodeAvgTimes:        make(map[string]time.Duration),
		NodeErrorRates:      make(map[string]float64),
		NodeSuccessCounts:   make(map[string]int64),
		NodeErrorCounts:     make(map[string]int64),
		ErrorsByType:        make(map[string]int64),
		PhaseResults:        make(map[string]*JourneyPhaseResult),
		EnrollmentLatencies: make([]time.Duration, 0, 100000),
		ExecutionLatencies:  make([]time.Duration, 0, 100000),
		EmailLatencies:      make([]time.Duration, 0, 100000),
	}
}

// RecordEnrollment records an enrollment operation
func (m *JourneyTestMetrics) RecordEnrollment(latency time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	atomic.AddInt64(&m.EnrollmentsAttempted, 1)

	if err != nil {
		m.TotalErrors++
		m.ErrorsByType["enrollment"]++
		return
	}

	atomic.AddInt64(&m.EnrollmentsSucceeded, 1)
	if len(m.EnrollmentLatencies) < 100000 {
		m.EnrollmentLatencies = append(m.EnrollmentLatencies, latency)
	}
}

// RecordExecution records an execution operation
func (m *JourneyTestMetrics) RecordExecution(latency time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	atomic.AddInt64(&m.ExecutionsAttempted, 1)

	if err != nil {
		m.TotalErrors++
		m.ErrorsByType["execution"]++
		return
	}

	atomic.AddInt64(&m.ExecutionsSucceeded, 1)
	if len(m.ExecutionLatencies) < 100000 {
		m.ExecutionLatencies = append(m.ExecutionLatencies, latency)
	}
}

// RecordNodeProcessing records a node processing operation
func (m *JourneyTestMetrics) RecordNodeProcessing(nodeType string, latency time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err != nil {
		m.NodeErrorCounts[nodeType]++
		m.TotalErrors++
		m.ErrorsByType["node_"+nodeType]++
		return
	}

	m.NodeSuccessCounts[nodeType]++
	if times, ok := m.NodeProcessingTimes[nodeType]; ok {
		if len(times) < 10000 {
			m.NodeProcessingTimes[nodeType] = append(times, latency)
		}
	} else {
		m.NodeProcessingTimes[nodeType] = []time.Duration{latency}
	}
}

// RecordEmailSend records an email send operation
func (m *JourneyTestMetrics) RecordEmailSend(latency time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err != nil {
		m.TotalErrors++
		m.ErrorsByType["email"]++
		return
	}

	atomic.AddInt64(&m.EmailsSent, 1)
	if len(m.EmailLatencies) < 100000 {
		m.EmailLatencies = append(m.EmailLatencies, latency)
	}
}

// RecordCompletion records journey completion
func (m *JourneyTestMetrics) RecordCompletion(converted bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	atomic.AddInt64(&m.TotalCompleted, 1)
	if converted {
		atomic.AddInt64(&m.TotalConverted, 1)
	}
}

// RecordError records an error by type
func (m *JourneyTestMetrics) RecordError(errorType string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.TotalErrors++
	m.ErrorsByType[errorType]++
}

// Finalize calculates derived metrics
func (m *JourneyTestMetrics) Finalize(config *JourneyLoadTestConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.TestDuration = m.TestEndTime.Sub(m.TestStartTime)
	durationSeconds := m.TestDuration.Seconds()

	// Calculate rates
	if durationSeconds > 0 {
		m.EnrollmentRate = float64(m.EnrollmentsSucceeded) / durationSeconds
		m.ExecutionRate = float64(m.ExecutionsSucceeded) / durationSeconds
		m.EmailsPerSecond = float64(m.EmailsSent) / durationSeconds
	}

	// Calculate latency percentiles
	m.EnrollmentLatencyP50 = journeyPercentile(m.EnrollmentLatencies, 50)
	m.EnrollmentLatencyP99 = journeyPercentile(m.EnrollmentLatencies, 99)
	m.ExecutionLatencyP50 = journeyPercentile(m.ExecutionLatencies, 50)
	m.ExecutionLatencyP99 = journeyPercentile(m.ExecutionLatencies, 99)
	m.EmailLatencyP50 = journeyPercentile(m.EmailLatencies, 50)
	m.EmailLatencyP99 = journeyPercentile(m.EmailLatencies, 99)

	// Calculate node average times and error rates
	for nodeType, times := range m.NodeProcessingTimes {
		if len(times) > 0 {
			var sum time.Duration
			for _, t := range times {
				sum += t
			}
			m.NodeAvgTimes[nodeType] = sum / time.Duration(len(times))
		}

		successCount := m.NodeSuccessCounts[nodeType]
		errorCount := m.NodeErrorCounts[nodeType]
		total := successCount + errorCount
		if total > 0 {
			m.NodeErrorRates[nodeType] = float64(errorCount) / float64(total) * 100
		}
	}

	// Calculate completion rates
	if m.EnrollmentsSucceeded > 0 {
		m.JourneyCompletionRate = float64(m.TotalCompleted) / float64(m.EnrollmentsSucceeded) * 100
		m.JourneyConversionRate = float64(m.TotalConverted) / float64(m.EnrollmentsSucceeded) * 100
	}

	// Calculate email error rate
	if m.EmailsSent > 0 {
		emailErrors := m.ErrorsByType["email"]
		m.EmailErrorRate = float64(emailErrors) / float64(m.EmailsSent+emailErrors) * 100
	}

	// Extrapolate daily capacity
	secondsInDay := float64(86400)
	m.ProjectedDailyCapacity = int64(m.ExecutionRate * secondsInDay)

	// Calculate headroom
	targetDaily := float64(config.TargetEnrollments) * (secondsInDay / config.Duration.Seconds())
	if targetDaily > 0 {
		m.HeadroomPercent = ((float64(m.ProjectedDailyCapacity) - targetDaily) / targetDaily) * 100
	}

	// Identify bottleneck
	m.BottleneckComponent = identifyJourneyBottleneck(m, config)
}

// journeyPercentile calculates the p-th percentile of durations
func journeyPercentile(durations []time.Duration, p int) time.Duration {
	if len(durations) == 0 {
		return 0
	}

	sorted := make([]time.Duration, len(durations))
	copy(sorted, durations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	idx := int(float64(len(sorted)-1) * float64(p) / 100)
	return sorted[idx]
}

// identifyJourneyBottleneck determines the system bottleneck
func identifyJourneyBottleneck(m *JourneyTestMetrics, config *JourneyLoadTestConfig) string {
	targetRate := float64(config.TargetEnrollments) / config.Duration.Seconds()

	// Check if enrollment rate is limiting
	if m.EnrollmentRate < targetRate*0.9 {
		return "Enrollment Rate (PostgreSQL)"
	}

	// Check if execution rate is limiting
	if m.ExecutionRate < targetRate*0.9 {
		return "Execution Workers"
	}

	// Check for high email latency
	if m.EmailLatencyP99 > 200*time.Millisecond {
		return "Email Node (ESP Latency)"
	}

	// Check for high error rates
	if m.TotalErrors > m.ExecutionsSucceeded/100 { // >1% error rate
		return "High Error Rate"
	}

	// Check individual node performance
	for nodeType, avgTime := range m.NodeAvgTimes {
		switch nodeType {
		case "email":
			if avgTime > 100*time.Millisecond {
				return fmt.Sprintf("Email Node (%.2fms avg)", avgTime.Seconds()*1000)
			}
		case "condition":
			if avgTime > 10*time.Millisecond {
				return fmt.Sprintf("Condition Node (%.2fms avg)", avgTime.Seconds()*1000)
			}
		case "delay", "split", "goal":
			if avgTime > 5*time.Millisecond {
				return fmt.Sprintf("%s Node (%.2fms avg)", nodeType, avgTime.Seconds()*1000)
			}
		}
	}

	return "None identified"
}

// =============================================================================
// MOCK EMAIL SERVER
// =============================================================================

// MockEmailServer provides mock ESP endpoints for email nodes
type MockEmailServer struct {
	server     *http.Server
	port       int
	metrics    *JourneyTestMetrics
	delay      time.Duration

	// Request counters
	totalRequests int64
	totalErrors   int64

	mu sync.RWMutex
}

// NewMockEmailServer creates a new mock email server
func NewMockEmailServer(port int, metrics *JourneyTestMetrics, delay time.Duration) *MockEmailServer {
	return &MockEmailServer{
		port:    port,
		metrics: metrics,
		delay:   delay,
	}
}

// Start starts the mock email server
func (s *MockEmailServer) Start() error {
	mux := http.NewServeMux()

	// Generic email send endpoint
	mux.HandleFunc("/send", s.handleSend)

	// Health check
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
	})

	s.server = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.port),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("[MockEmail] Server starting on port %d", s.port)
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[MockEmail] Server error: %v", err)
		}
	}()

	// Wait for server to be ready
	time.Sleep(100 * time.Millisecond)
	return nil
}

// Stop stops the mock email server
func (s *MockEmailServer) Stop() error {
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.server.Shutdown(ctx)
	}
	return nil
}

// handleSend handles email send requests
func (s *MockEmailServer) handleSend(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&s.totalRequests, 1)

	// Simulate network latency
	time.Sleep(s.delay)

	// Simulate occasional errors (0.1% error rate)
	if rand.Float64() < 0.001 {
		atomic.AddInt64(&s.totalErrors, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "temporary failure"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"message_id": uuid.New().String(),
	})
}

// Stats returns server statistics
func (s *MockEmailServer) Stats() map[string]int64 {
	return map[string]int64{
		"total_requests": atomic.LoadInt64(&s.totalRequests),
		"total_errors":   atomic.LoadInt64(&s.totalErrors),
	}
}

// =============================================================================
// TEST TABLE MANAGEMENT
// =============================================================================

// JourneyTestTableManager manages test-specific tables
type JourneyTestTableManager struct {
	db             *sql.DB
	journeyTable   string
	enrollmentTable string
	executionTable string
}

// NewJourneyTestTableManager creates a new test table manager
func NewJourneyTestTableManager(db *sql.DB) *JourneyTestTableManager {
	suffix := time.Now().Format("20060102_150405")
	return &JourneyTestTableManager{
		db:              db,
		journeyTable:    fmt.Sprintf("load_test_journeys_%s", suffix),
		enrollmentTable: fmt.Sprintf("load_test_enrollments_%s", suffix),
		executionTable:  fmt.Sprintf("load_test_executions_%s", suffix),
	}
}

// Setup creates the test tables
func (t *JourneyTestTableManager) Setup(ctx context.Context) error {
	// Create journeys table
	_, err := t.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			name VARCHAR(255) NOT NULL,
			description TEXT,
			status VARCHAR(50) DEFAULT 'active',
			nodes JSONB DEFAULT '[]',
			connections JSONB DEFAULT '[]',
			total_enrolled INT DEFAULT 0,
			total_completed INT DEFAULT 0,
			total_converted INT DEFAULT 0,
			created_at TIMESTAMP DEFAULT NOW(),
			updated_at TIMESTAMP DEFAULT NOW()
		)
	`, t.journeyTable))
	if err != nil {
		return fmt.Errorf("failed to create journeys table: %w", err)
	}

	// Create enrollments table
	_, err = t.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			journey_id UUID NOT NULL,
			subscriber_email VARCHAR(255) NOT NULL,
			current_node_id VARCHAR(100),
			status VARCHAR(50) DEFAULT 'active',
			metadata JSONB DEFAULT '{}',
			next_execute_at TIMESTAMP,
			enrolled_at TIMESTAMP DEFAULT NOW(),
			completed_at TIMESTAMP,
			execution_count INT DEFAULT 0,
			last_executed_at TIMESTAMP
		)
	`, t.enrollmentTable))
	if err != nil {
		return fmt.Errorf("failed to create enrollments table: %w", err)
	}

	// Create execution log table
	_, err = t.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			enrollment_id UUID NOT NULL,
			journey_id UUID NOT NULL,
			node_id VARCHAR(100) NOT NULL,
			node_type VARCHAR(50) NOT NULL,
			action VARCHAR(50),
			result VARCHAR(50),
			error_message TEXT,
			executed_at TIMESTAMP DEFAULT NOW()
		)
	`, t.executionTable))
	if err != nil {
		return fmt.Errorf("failed to create execution log table: %w", err)
	}

	// Create indexes
	indexes := []string{
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS idx_%s_status ON %s(status)", t.enrollmentTable, t.enrollmentTable),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS idx_%s_journey ON %s(journey_id)", t.enrollmentTable, t.enrollmentTable),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS idx_%s_next_exec ON %s(next_execute_at)", t.enrollmentTable, t.enrollmentTable),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS idx_%s_journey ON %s(journey_id)", t.executionTable, t.executionTable),
	}

	for _, idx := range indexes {
		if _, err := t.db.ExecContext(ctx, idx); err != nil {
			log.Printf("Warning: failed to create index: %v", err)
		}
	}

	return nil
}

// Cleanup drops the test tables
func (t *JourneyTestTableManager) Cleanup(ctx context.Context) error {
	tables := []string{t.executionTable, t.enrollmentTable, t.journeyTable}
	for _, table := range tables {
		if _, err := t.db.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", table)); err != nil {
			log.Printf("Warning: failed to drop table %s: %v", table, err)
		}
	}
	return nil
}

// TableNames returns the test table names
func (t *JourneyTestTableManager) TableNames() (string, string, string) {
	return t.journeyTable, t.enrollmentTable, t.executionTable
}

// =============================================================================
// JOURNEY TEST RUNNER
// =============================================================================

// JourneyLoadTest orchestrates the journey load test
type JourneyLoadTest struct {
	config       *JourneyLoadTestConfig
	metrics      *JourneyTestMetrics
	db           *sql.DB
	redis        *redis.Client
	mockServer   *MockEmailServer
	tableManager *JourneyTestTableManager

	// Test journey
	testJourneyID string
	testNodes     []TestJourneyNode

	ctx    context.Context
	cancel context.CancelFunc
}

// TestJourneyNode represents a node in the test journey
type TestJourneyNode struct {
	ID          string                 `json:"id"`
	Type        string                 `json:"type"`
	Config      map[string]interface{} `json:"config"`
	Connections []string               `json:"connections"`
}

// TestJourneyConnection represents a connection in the test journey
type TestJourneyConnection struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label,omitempty"`
}

// NewJourneyLoadTest creates a new journey load test runner
func NewJourneyLoadTest(config *JourneyLoadTestConfig) *JourneyLoadTest {
	return &JourneyLoadTest{
		config:  config,
		metrics: NewJourneyTestMetrics(),
	}
}

// Initialize sets up all test infrastructure
func (t *JourneyLoadTest) Initialize(ctx context.Context) error {
	log.Println("Initializing journey load test infrastructure...")

	// Connect to PostgreSQL
	if t.config.PostgresURL != "" {
		db, err := sql.Open("postgres", t.config.PostgresURL)
		if err != nil {
			return fmt.Errorf("failed to connect to PostgreSQL: %w", err)
		}
		db.SetMaxOpenConns(100)
		db.SetMaxIdleConns(50)
		if err := db.PingContext(ctx); err != nil {
			return fmt.Errorf("failed to ping PostgreSQL: %w", err)
		}
		t.db = db
		log.Println("  ✓ Connected to PostgreSQL")

		// Setup test tables
		t.tableManager = NewJourneyTestTableManager(db)
		if err := t.tableManager.Setup(ctx); err != nil {
			return fmt.Errorf("failed to setup test tables: %w", err)
		}
		journeyTable, enrollmentTable, _ := t.tableManager.TableNames()
		log.Printf("  ✓ Created test tables: %s, %s", journeyTable, enrollmentTable)

		// Create test journey
		if err := t.createTestJourney(ctx); err != nil {
			return fmt.Errorf("failed to create test journey: %w", err)
		}
		log.Printf("  ✓ Created test journey: %s", t.testJourneyID)
	}

	// Connect to Redis
	if t.config.RedisURL != "" {
		opts, err := redis.ParseURL(t.config.RedisURL)
		if err != nil {
			// Try as host:port format
			opts = &redis.Options{Addr: t.config.RedisURL}
		}
		t.redis = redis.NewClient(opts)
		if err := t.redis.Ping(ctx).Err(); err != nil {
			return fmt.Errorf("failed to connect to Redis: %w", err)
		}
		log.Println("  ✓ Connected to Redis")
	}

	// Start mock email server
	t.mockServer = NewMockEmailServer(t.config.MockESPPort, t.metrics, t.config.MockESPDelay)
	if err := t.mockServer.Start(); err != nil {
		return fmt.Errorf("failed to start mock email server: %w", err)
	}
	log.Printf("  ✓ Started mock email server on port %d", t.config.MockESPPort)

	return nil
}

// createTestJourney creates a test journey based on complexity
func (t *JourneyLoadTest) createTestJourney(ctx context.Context) error {
	t.testJourneyID = uuid.New().String()

	// Build journey based on complexity
	switch t.config.JourneyComplexity {
	case "simple":
		t.testNodes = t.buildSimpleJourney()
	case "complex":
		t.testNodes = t.buildComplexJourney()
	default: // medium
		t.testNodes = t.buildMediumJourney()
	}

	// Build connections
	connections := t.buildConnections(t.testNodes)

	nodesJSON, _ := json.Marshal(t.testNodes)
	connectionsJSON, _ := json.Marshal(connections)

	journeyTable, _, _ := t.tableManager.TableNames()
	_, err := t.db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, name, description, status, nodes, connections)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, journeyTable), t.testJourneyID, "Load Test Journey", "Journey for load testing", "active", string(nodesJSON), string(connectionsJSON))

	return err
}

// buildSimpleJourney creates a simple 3-node journey: trigger -> email -> goal
func (t *JourneyLoadTest) buildSimpleJourney() []TestJourneyNode {
	return []TestJourneyNode{
		{ID: "trigger-1", Type: "trigger", Config: map[string]interface{}{"name": "Start"}, Connections: []string{"email-1"}},
		{ID: "email-1", Type: "email", Config: map[string]interface{}{"subject": "Welcome", "htmlContent": "<p>Hello!</p>"}, Connections: []string{"goal-1"}},
		{ID: "goal-1", Type: "goal", Config: map[string]interface{}{"name": "Completed"}, Connections: []string{}},
	}
}

// buildMediumJourney creates a medium 5-node journey with delay and condition
func (t *JourneyLoadTest) buildMediumJourney() []TestJourneyNode {
	return []TestJourneyNode{
		{ID: "trigger-1", Type: "trigger", Config: map[string]interface{}{"name": "Start"}, Connections: []string{"email-1"}},
		{ID: "email-1", Type: "email", Config: map[string]interface{}{"subject": "Welcome", "htmlContent": "<p>Hello!</p>"}, Connections: []string{"delay-1"}},
		{ID: "delay-1", Type: "delay", Config: map[string]interface{}{"delayValue": 0, "delayUnit": "minutes"}, Connections: []string{"condition-1"}},
		{ID: "condition-1", Type: "condition", Config: map[string]interface{}{"conditionType": "engagement_score", "threshold": 50}, Connections: []string{"email-2", "goal-1"}},
		{ID: "email-2", Type: "email", Config: map[string]interface{}{"subject": "Follow-up", "htmlContent": "<p>Following up!</p>"}, Connections: []string{"goal-1"}},
		{ID: "goal-1", Type: "goal", Config: map[string]interface{}{"name": "Completed"}, Connections: []string{}},
	}
}

// buildComplexJourney creates a complex 10-node journey with splits and multiple paths
func (t *JourneyLoadTest) buildComplexJourney() []TestJourneyNode {
	return []TestJourneyNode{
		{ID: "trigger-1", Type: "trigger", Config: map[string]interface{}{"name": "Start"}, Connections: []string{"email-1"}},
		{ID: "email-1", Type: "email", Config: map[string]interface{}{"subject": "Welcome", "htmlContent": "<p>Hello!</p>"}, Connections: []string{"delay-1"}},
		{ID: "delay-1", Type: "delay", Config: map[string]interface{}{"delayValue": 0, "delayUnit": "minutes"}, Connections: []string{"split-1"}},
		{ID: "split-1", Type: "split", Config: map[string]interface{}{"splitPercentage": 50}, Connections: []string{"email-2", "email-3"}},
		{ID: "email-2", Type: "email", Config: map[string]interface{}{"subject": "Path A", "htmlContent": "<p>Path A</p>"}, Connections: []string{"condition-1"}},
		{ID: "email-3", Type: "email", Config: map[string]interface{}{"subject": "Path B", "htmlContent": "<p>Path B</p>"}, Connections: []string{"condition-1"}},
		{ID: "condition-1", Type: "condition", Config: map[string]interface{}{"conditionType": "engagement_score", "threshold": 30}, Connections: []string{"email-4", "goal-1"}},
		{ID: "email-4", Type: "email", Config: map[string]interface{}{"subject": "Final Push", "htmlContent": "<p>Final!</p>"}, Connections: []string{"delay-2"}},
		{ID: "delay-2", Type: "delay", Config: map[string]interface{}{"delayValue": 0, "delayUnit": "minutes"}, Connections: []string{"goal-1"}},
		{ID: "goal-1", Type: "goal", Config: map[string]interface{}{"name": "Completed"}, Connections: []string{}},
	}
}

// buildConnections builds connections from nodes
func (t *JourneyLoadTest) buildConnections(nodes []TestJourneyNode) []TestJourneyConnection {
	var connections []TestJourneyConnection
	for _, node := range nodes {
		for i, targetID := range node.Connections {
			label := ""
			if node.Type == "condition" {
				if i == 0 {
					label = "true"
				} else {
					label = "false"
				}
			} else if node.Type == "split" {
				if i == 0 {
					label = "A"
				} else {
					label = "B"
				}
			}
			connections = append(connections, TestJourneyConnection{
				From:  node.ID,
				To:    targetID,
				Label: label,
			})
		}
	}
	return connections
}

// Run executes all test phases
func (t *JourneyLoadTest) Run(ctx context.Context) error {
	t.ctx, t.cancel = context.WithCancel(ctx)
	defer t.cancel()

	t.metrics.TestStartTime = time.Now()

	log.Println("\n" + strings.Repeat("=", 80))
	log.Println("                    STARTING JOURNEY LOAD TEST")
	log.Println(strings.Repeat("=", 80))
	log.Printf("Target: %d enrollments over %v\n", t.config.TargetEnrollments, t.config.Duration)
	log.Printf("Workers: %d, Complexity: %s\n", t.config.Workers, t.config.JourneyComplexity)
	log.Println(strings.Repeat("=", 80))

	// Determine which tests to run
	testTypes := []string{}
	switch t.config.TestType {
	case "all":
		testTypes = []string{"enrollment", "execution", "node", "sustained", "spike", "email"}
	default:
		testTypes = strings.Split(t.config.TestType, ",")
	}

	// Run selected test phases
	for _, testType := range testTypes {
		select {
		case <-t.ctx.Done():
			log.Printf("Test interrupted during: %s", testType)
			return t.ctx.Err()
		default:
		}

		var result *JourneyPhaseResult
		var err error

		switch strings.TrimSpace(testType) {
		case "enrollment":
			result, err = t.RunMassEnrollmentTest()
		case "execution":
			result, err = t.RunConcurrentExecutionTest()
		case "node":
			result, err = t.RunNodeThroughputTest()
		case "segment":
			result, err = t.RunSegmentDrivenTest()
		case "sustained":
			result, err = t.RunSustainedLoadTest()
		case "spike":
			result, err = t.RunSpikeTest()
		case "email":
			result, err = t.RunEmailNodeStressTest()
		default:
			continue
		}

		if err != nil {
			log.Printf("Phase %s error: %v", testType, err)
			result = &JourneyPhaseResult{
				Name:   testType,
				Status: "FAIL",
				Details: map[string]interface{}{
					"error": err.Error(),
				},
			}
		}
		t.metrics.PhaseResults[testType] = result
	}

	t.metrics.TestEndTime = time.Now()
	t.metrics.Finalize(t.config)

	return nil
}

// RunMassEnrollmentTest validates enrollment at scale
func (t *JourneyLoadTest) RunMassEnrollmentTest() (*JourneyPhaseResult, error) {
	log.Println("\n[TEST 1] MASS ENROLLMENT TEST")
	log.Println(strings.Repeat("-", 60))

	result := &JourneyPhaseResult{
		Name:      "enrollment",
		StartTime: time.Now(),
		Details:   make(map[string]interface{}),
	}

	if t.db == nil {
		result.Status = "SKIP"
		result.Details["reason"] = "PostgreSQL not configured"
		return result, nil
	}

	// Target: 50K+ enrollments/second
	targetRate := 50000
	testDuration := 30 * time.Second
	totalToEnroll := int64(targetRate) * int64(testDuration.Seconds())

	log.Printf("  Target: %d enrollments/second for %v", targetRate, testDuration)
	log.Printf("  Total subscribers to enroll: %d", totalToEnroll)

	_, enrollmentTable, _ := t.tableManager.TableNames()

	var totalEnrolled int64
	var totalLatency time.Duration
	batchSize := 10000

	startTime := time.Now()
	deadline := startTime.Add(testDuration)

	for time.Now().Before(deadline) {
		select {
		case <-t.ctx.Done():
			break
		default:
		}

		batchStart := time.Now()

		// Begin transaction with COPY
		tx, err := t.db.BeginTx(t.ctx, nil)
		if err != nil {
			continue
		}

		stmt, err := tx.Prepare(pq.CopyIn(
			enrollmentTable,
			"id", "journey_id", "subscriber_email", "current_node_id",
			"status", "next_execute_at", "enrolled_at",
		))
		if err != nil {
			tx.Rollback()
			continue
		}

		now := time.Now()
		firstNodeID := t.testNodes[0].ID
		if len(t.testNodes) > 1 {
			firstNodeID = t.testNodes[1].ID // Skip trigger
		}

		for i := 0; i < batchSize; i++ {
			_, err = stmt.Exec(
				uuid.New(),
				t.testJourneyID,
				fmt.Sprintf("test_%d_%d@loadtest.local", atomic.LoadInt64(&totalEnrolled), i),
				firstNodeID,
				"active",
				now,
				now,
			)
			if err != nil {
				break
			}
		}

		// Flush COPY
		_, err = stmt.Exec()
		if err != nil {
			tx.Rollback()
			continue
		}
		stmt.Close()

		if err := tx.Commit(); err != nil {
			continue
		}

		batchLatency := time.Since(batchStart)
		totalLatency += batchLatency
		atomic.AddInt64(&totalEnrolled, int64(batchSize))
		t.metrics.RecordEnrollment(batchLatency/time.Duration(batchSize), nil)

		// Log progress
		if totalEnrolled%100000 == 0 {
			elapsed := time.Since(startTime)
			rate := float64(totalEnrolled) / elapsed.Seconds()
			log.Printf("    Progress: %d enrolled (%.0f/sec)", totalEnrolled, rate)
		}
	}

	elapsed := time.Since(startTime)
	actualRate := float64(totalEnrolled) / elapsed.Seconds()

	result.EndTime = time.Now()
	result.Duration = elapsed
	result.Details["total_enrolled"] = totalEnrolled
	result.Details["target_rate"] = targetRate
	result.Details["actual_rate"] = actualRate

	batches := totalEnrolled / int64(batchSize)
	if batches > 0 {
		result.Details["avg_batch_latency_ms"] = totalLatency.Milliseconds() / batches
	}

	if actualRate >= float64(targetRate)*0.9 {
		result.Status = "PASS"
		log.Printf("  ✓ PASS: %.0f enrollments/second (target: %d)", actualRate, targetRate)
	} else {
		result.Status = "FAIL"
		log.Printf("  ✗ FAIL: %.0f enrollments/second (target: %d)", actualRate, targetRate)
	}

	return result, nil
}

// RunConcurrentExecutionTest processes many enrollments simultaneously
func (t *JourneyLoadTest) RunConcurrentExecutionTest() (*JourneyPhaseResult, error) {
	log.Println("\n[TEST 2] CONCURRENT EXECUTION TEST")
	log.Println(strings.Repeat("-", 60))

	result := &JourneyPhaseResult{
		Name:      "execution",
		StartTime: time.Now(),
		Details:   make(map[string]interface{}),
	}

	if t.db == nil {
		result.Status = "SKIP"
		result.Details["reason"] = "PostgreSQL not configured"
		return result, nil
	}

	// Pre-enroll subscribers for execution test
	preEnrollCount := int64(100000)
	log.Printf("  Pre-enrolling %d subscribers...", preEnrollCount)

	_, enrollmentTable, _ := t.tableManager.TableNames()

	// Quick batch insert for pre-enrollment
	batchSize := 10000
	firstNodeID := t.testNodes[0].ID
	if len(t.testNodes) > 1 {
		firstNodeID = t.testNodes[1].ID
	}

	for i := int64(0); i < preEnrollCount; i += int64(batchSize) {
		tx, _ := t.db.BeginTx(t.ctx, nil)
		stmt, _ := tx.Prepare(pq.CopyIn(enrollmentTable, "id", "journey_id", "subscriber_email", "current_node_id", "status", "next_execute_at", "enrolled_at"))

		now := time.Now()
		for j := 0; j < batchSize && i+int64(j) < preEnrollCount; j++ {
			stmt.Exec(uuid.New(), t.testJourneyID, fmt.Sprintf("exec_test_%d@loadtest.local", i+int64(j)), firstNodeID, "active", now, now)
		}
		stmt.Exec()
		stmt.Close()
		tx.Commit()
	}

	log.Printf("  Pre-enrolled %d subscribers", preEnrollCount)
	log.Printf("  Spawning %d execution workers...", t.config.Workers)

	// Execute with multiple workers
	testDuration := time.Minute
	var wg sync.WaitGroup
	var totalExecuted int64

	startTime := time.Now()
	deadline := startTime.Add(testDuration)

	for i := 0; i < t.config.Workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			for time.Now().Before(deadline) {
				select {
				case <-t.ctx.Done():
					return
				default:
				}

				execStart := time.Now()

				// Simulate node execution
				err := t.simulateNodeExecution(t.ctx)
				execLatency := time.Since(execStart)

				if err == nil {
					atomic.AddInt64(&totalExecuted, 1)
					t.metrics.RecordExecution(execLatency, nil)
				} else {
					t.metrics.RecordExecution(execLatency, err)
				}
			}
		}(i)
	}

	// Monitor progress
	ticker := time.NewTicker(10 * time.Second)
	go func() {
		for {
			select {
			case <-t.ctx.Done():
				return
			case <-ticker.C:
				if time.Now().After(deadline) {
					return
				}
				elapsed := time.Since(startTime)
				rate := float64(atomic.LoadInt64(&totalExecuted)) / elapsed.Seconds()
				log.Printf("    Progress: %d executed (%.1f/sec)", atomic.LoadInt64(&totalExecuted), rate)
			}
		}
	}()

	wg.Wait()
	ticker.Stop()

	elapsed := time.Since(startTime)
	actualRate := float64(totalExecuted) / elapsed.Seconds()

	result.EndTime = time.Now()
	result.Duration = elapsed
	result.Details["total_executed"] = totalExecuted
	result.Details["execution_rate"] = actualRate
	result.Details["workers"] = t.config.Workers

	targetRate := float64(t.config.TargetEnrollments) / t.config.Duration.Seconds()
	if actualRate >= targetRate*0.9 {
		result.Status = "PASS"
		log.Printf("  ✓ PASS: %.1f executions/second", actualRate)
	} else {
		result.Status = "FAIL"
		log.Printf("  ✗ FAIL: %.1f executions/second (target: %.1f)", actualRate, targetRate)
	}

	return result, nil
}

// RunNodeThroughputTest measures individual node processing times
func (t *JourneyLoadTest) RunNodeThroughputTest() (*JourneyPhaseResult, error) {
	log.Println("\n[TEST 3] NODE THROUGHPUT TEST")
	log.Println(strings.Repeat("-", 60))

	result := &JourneyPhaseResult{
		Name:      "node",
		StartTime: time.Now(),
		Details:   make(map[string]interface{}),
	}

	nodeTypes := []struct {
		name      string
		targetMs  float64
		processor func(context.Context) error
	}{
		{"trigger", 0, t.processTriggerNode},
		{"email", 50, t.processEmailNode},
		{"delay", 1, t.processDelayNode},
		{"condition", 5, t.processConditionNode},
		{"split", 1, t.processSplitNode},
		{"goal", 1, t.processGoalNode},
	}

	iterations := 1000
	allPass := true

	for _, nt := range nodeTypes {
		log.Printf("  Testing %s node (target: <%.0fms)...", nt.name, nt.targetMs)

		var totalTime time.Duration
		var errors int64

		for i := 0; i < iterations; i++ {
			start := time.Now()
			err := nt.processor(t.ctx)
			elapsed := time.Since(start)

			totalTime += elapsed
			t.metrics.RecordNodeProcessing(nt.name, elapsed, err)

			if err != nil {
				errors++
			}
		}

		avgMs := float64(totalTime.Nanoseconds()) / float64(iterations) / 1_000_000
		successRate := float64(iterations-int(errors)) / float64(iterations) * 100

		result.Details[nt.name+"_avg_ms"] = avgMs
		result.Details[nt.name+"_success_rate"] = successRate

		if nt.targetMs > 0 && avgMs <= nt.targetMs && successRate >= 99 {
			log.Printf("    ✓ %s: %.2fms avg (%.1f%% success)", nt.name, avgMs, successRate)
		} else if nt.name == "trigger" {
			log.Printf("    - %s: N/A (trigger nodes don't execute)", nt.name)
		} else {
			log.Printf("    ✗ %s: %.2fms avg (%.1f%% success) - target: <%.0fms", nt.name, avgMs, successRate, nt.targetMs)
			allPass = false
		}
	}

	result.EndTime = time.Now()
	result.Duration = time.Since(result.StartTime)

	if allPass {
		result.Status = "PASS"
		log.Println("  ✓ PASS: All node types within performance targets")
	} else {
		result.Status = "FAIL"
		log.Println("  ✗ FAIL: Some nodes exceeded performance targets")
	}

	return result, nil
}

// RunSegmentDrivenTest enrolls entire segment and tracks completion
func (t *JourneyLoadTest) RunSegmentDrivenTest() (*JourneyPhaseResult, error) {
	log.Println("\n[TEST 4] SEGMENT-DRIVEN TEST")
	log.Println(strings.Repeat("-", 60))

	result := &JourneyPhaseResult{
		Name:      "segment",
		StartTime: time.Now(),
		Details:   make(map[string]interface{}),
	}

	if t.db == nil {
		result.Status = "SKIP"
		result.Details["reason"] = "PostgreSQL not configured"
		return result, nil
	}

	segmentSize := t.config.SegmentSize
	log.Printf("  Segment size: %d subscribers", segmentSize)

	_, enrollmentTable, _ := t.tableManager.TableNames()

	// Enroll segment
	log.Println("  Enrolling segment...")
	enrollStart := time.Now()

	batchSize := int64(10000)
	firstNodeID := t.testNodes[0].ID
	if len(t.testNodes) > 1 {
		firstNodeID = t.testNodes[1].ID
	}

	var enrolled int64
	for enrolled < segmentSize {
		tx, _ := t.db.BeginTx(t.ctx, nil)
		stmt, _ := tx.Prepare(pq.CopyIn(enrollmentTable, "id", "journey_id", "subscriber_email", "current_node_id", "status", "next_execute_at", "enrolled_at"))

		now := time.Now()
		thisBatch := batchSize
		if enrolled+batchSize > segmentSize {
			thisBatch = segmentSize - enrolled
		}

		for i := int64(0); i < thisBatch; i++ {
			stmt.Exec(uuid.New(), t.testJourneyID, fmt.Sprintf("segment_%d@loadtest.local", enrolled+i), firstNodeID, "active", now, now)
		}
		stmt.Exec()
		stmt.Close()
		tx.Commit()
		enrolled += thisBatch
	}

	enrollDuration := time.Since(enrollStart)
	enrollRate := float64(segmentSize) / enrollDuration.Seconds()
	log.Printf("  Enrolled %d in %v (%.0f/sec)", segmentSize, enrollDuration.Round(time.Millisecond), enrollRate)

	// Process enrollments
	log.Println("  Processing enrollments...")
	processStart := time.Now()

	var processed int64
	var completed int64
	var converted int64

	// Simulate processing with workers
	var wg sync.WaitGroup
	processDuration := 2 * time.Minute
	processDeadline := time.Now().Add(processDuration)

	for i := 0; i < t.config.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for time.Now().Before(processDeadline) {
				select {
				case <-t.ctx.Done():
					return
				default:
				}

				// Simulate journey execution
				if err := t.simulateFullJourneyExecution(t.ctx); err == nil {
					atomic.AddInt64(&processed, 1)

					// Simulate completion (90% complete, 30% convert)
					if rand.Float64() < 0.90 {
						atomic.AddInt64(&completed, 1)
						t.metrics.RecordCompletion(rand.Float64() < 0.33)
						if rand.Float64() < 0.33 {
							atomic.AddInt64(&converted, 1)
						}
					}
				}
			}
		}()
	}

	wg.Wait()

	processDurationActual := time.Since(processStart)
	processRate := float64(processed) / processDurationActual.Seconds()
	completionRate := float64(completed) / float64(processed) * 100
	conversionRate := float64(converted) / float64(processed) * 100

	result.EndTime = time.Now()
	result.Duration = time.Since(result.StartTime)
	result.Details["segment_size"] = segmentSize
	result.Details["enrollment_rate"] = enrollRate
	result.Details["processed"] = processed
	result.Details["process_rate"] = processRate
	result.Details["completed"] = completed
	result.Details["completion_rate"] = completionRate
	result.Details["converted"] = converted
	result.Details["conversion_rate"] = conversionRate

	if completionRate >= 85 && processRate >= float64(segmentSize)/processDuration.Seconds()*0.5 {
		result.Status = "PASS"
		log.Printf("  ✓ PASS: %.1f%% completion, %.1f%% conversion", completionRate, conversionRate)
	} else {
		result.Status = "FAIL"
		log.Printf("  ✗ FAIL: %.1f%% completion, %.1f%% conversion", completionRate, conversionRate)
	}

	return result, nil
}

// RunSustainedLoadTest runs at production rate for extended period
func (t *JourneyLoadTest) RunSustainedLoadTest() (*JourneyPhaseResult, error) {
	log.Println("\n[TEST 5] SUSTAINED LOAD TEST")
	log.Println(strings.Repeat("-", 60))

	result := &JourneyPhaseResult{
		Name:      "sustained",
		StartTime: time.Now(),
		Details:   make(map[string]interface{}),
	}

	targetRate := float64(t.config.TargetEnrollments) / t.config.Duration.Seconds()
	testDuration := t.config.Duration
	log.Printf("  Target rate: %.1f enrollments/second for %v", targetRate, testDuration)

	var totalProcessed int64
	var wg sync.WaitGroup

	startTime := time.Now()
	deadline := startTime.Add(testDuration)

	// Rate limiter to control sustained load
	rateLimiter := time.NewTicker(time.Second / time.Duration(targetRate))
	defer rateLimiter.Stop()

	// Spawn workers
	for i := 0; i < t.config.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for time.Now().Before(deadline) {
				select {
				case <-t.ctx.Done():
					return
				case <-rateLimiter.C:
					start := time.Now()
					err := t.simulateNodeExecution(t.ctx)
					latency := time.Since(start)

					if err == nil {
						atomic.AddInt64(&totalProcessed, 1)
						t.metrics.RecordExecution(latency, nil)
					} else {
						t.metrics.RecordExecution(latency, err)
					}
				}
			}
		}()
	}

	// Monitor for degradation
	var samples []float64
	monitorTicker := time.NewTicker(10 * time.Second)
	go func() {
		for {
			select {
			case <-t.ctx.Done():
				return
			case <-monitorTicker.C:
				if time.Now().After(deadline) {
					return
				}
				elapsed := time.Since(startTime)
				rate := float64(atomic.LoadInt64(&totalProcessed)) / elapsed.Seconds()
				samples = append(samples, rate)
				log.Printf("    [%.0fs] Rate: %.1f/sec", elapsed.Seconds(), rate)
			}
		}
	}()

	wg.Wait()
	monitorTicker.Stop()

	elapsed := time.Since(startTime)
	actualRate := float64(totalProcessed) / elapsed.Seconds()

	// Check for degradation (last sample vs first sample)
	degradation := 0.0
	if len(samples) >= 2 {
		degradation = (samples[0] - samples[len(samples)-1]) / samples[0] * 100
	}

	result.EndTime = time.Now()
	result.Duration = elapsed
	result.Details["total_processed"] = totalProcessed
	result.Details["average_rate"] = actualRate
	result.Details["target_rate"] = targetRate
	result.Details["degradation_percent"] = degradation

	if actualRate >= targetRate*0.9 && degradation < 10 {
		result.Status = "PASS"
		log.Printf("  ✓ PASS: %.1f/sec sustained (%.1f%% degradation)", actualRate, degradation)
	} else {
		result.Status = "FAIL"
		log.Printf("  ✗ FAIL: %.1f/sec (target: %.1f), %.1f%% degradation", actualRate, targetRate, degradation)
	}

	return result, nil
}

// RunSpikeTest handles sudden surge of enrollments
func (t *JourneyLoadTest) RunSpikeTest() (*JourneyPhaseResult, error) {
	log.Println("\n[TEST 6] SPIKE TEST")
	log.Println(strings.Repeat("-", 60))

	result := &JourneyPhaseResult{
		Name:      "spike",
		StartTime: time.Now(),
		Details:   make(map[string]interface{}),
	}

	baselineRate := float64(t.config.TargetEnrollments) / t.config.Duration.Seconds() / 10 // 10% of target
	spikeRate := baselineRate * t.config.SpikeMultiplier

	log.Printf("  Baseline: %.1f/sec, Spike: %.1f/sec (%.0fx)", baselineRate, spikeRate, t.config.SpikeMultiplier)

	// Run baseline for 30 seconds
	log.Println("  Running baseline...")
	baselineProcessed := t.runAtRate(30*time.Second, baselineRate)

	// Spike for 30 seconds
	log.Println("  Triggering spike...")
	spikeStart := time.Now()
	spikeProcessed := t.runAtRate(30*time.Second, spikeRate)
	spikeDuration := time.Since(spikeStart)
	spikeActualRate := float64(spikeProcessed) / spikeDuration.Seconds()

	// Recovery period
	log.Println("  Recovery period...")
	recoveryStart := time.Now()
	recoveryProcessed := t.runAtRate(30*time.Second, baselineRate)
	recoveryDuration := time.Since(recoveryStart)
	recoveryRate := float64(recoveryProcessed) / recoveryDuration.Seconds()

	result.EndTime = time.Now()
	result.Duration = time.Since(result.StartTime)
	result.Details["baseline_processed"] = baselineProcessed
	result.Details["spike_processed"] = spikeProcessed
	result.Details["spike_actual_rate"] = spikeActualRate
	result.Details["recovery_processed"] = recoveryProcessed
	result.Details["recovery_rate"] = recoveryRate
	result.Details["spike_multiplier"] = t.config.SpikeMultiplier

	// Check if system handled spike and recovered
	spikeSuccess := spikeActualRate >= spikeRate*0.8
	recoverySuccess := recoveryRate >= baselineRate*0.9

	if spikeSuccess && recoverySuccess {
		result.Status = "PASS"
		log.Printf("  ✓ PASS: Handled %.0fx spike, recovered to %.1f/sec", t.config.SpikeMultiplier, recoveryRate)
	} else {
		result.Status = "FAIL"
		log.Printf("  ✗ FAIL: Spike handling: %v, Recovery: %v", spikeSuccess, recoverySuccess)
	}

	return result, nil
}

// runAtRate runs processing at a target rate for a duration
func (t *JourneyLoadTest) runAtRate(duration time.Duration, targetRate float64) int64 {
	var processed int64
	var wg sync.WaitGroup

	deadline := time.Now().Add(duration)
	interval := time.Duration(float64(time.Second) / targetRate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for i := 0; i < t.config.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for time.Now().Before(deadline) {
				select {
				case <-t.ctx.Done():
					return
				case <-ticker.C:
					if err := t.simulateNodeExecution(t.ctx); err == nil {
						atomic.AddInt64(&processed, 1)
					}
				}
			}
		}()
	}

	wg.Wait()
	return processed
}

// RunEmailNodeStressTest validates email sending under load
func (t *JourneyLoadTest) RunEmailNodeStressTest() (*JourneyPhaseResult, error) {
	log.Println("\n[TEST 7] EMAIL NODE STRESS TEST")
	log.Println(strings.Repeat("-", 60))

	result := &JourneyPhaseResult{
		Name:      "email",
		StartTime: time.Now(),
		Details:   make(map[string]interface{}),
	}

	if t.mockServer == nil {
		result.Status = "SKIP"
		result.Details["reason"] = "Mock email server not available"
		return result, nil
	}

	targetEmailsPerSecond := 1000.0
	testDuration := time.Minute

	log.Printf("  Target: %.0f emails/second for %v", targetEmailsPerSecond, testDuration)

	var totalSent int64
	var wg sync.WaitGroup

	startTime := time.Now()
	deadline := startTime.Add(testDuration)

	mockURL := fmt.Sprintf("http://localhost:%d/send", t.config.MockESPPort)
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        1000,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	// Spawn workers
	for i := 0; i < t.config.Workers*2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for time.Now().Before(deadline) {
				select {
				case <-t.ctx.Done():
					return
				default:
				}

				sendStart := time.Now()

				payload := []byte(`{"to":"test@loadtest.local","subject":"Load Test","body":"<p>Test</p>"}`)
				resp, err := client.Post(mockURL, "application/json", bytes.NewReader(payload))
				sendLatency := time.Since(sendStart)

				if err != nil {
					t.metrics.RecordEmailSend(sendLatency, err)
					continue
				}
				resp.Body.Close()

				if resp.StatusCode >= 400 {
					t.metrics.RecordEmailSend(sendLatency, fmt.Errorf("status %d", resp.StatusCode))
					continue
				}

				atomic.AddInt64(&totalSent, 1)
				t.metrics.RecordEmailSend(sendLatency, nil)
			}
		}()
	}

	// Monitor progress
	ticker := time.NewTicker(10 * time.Second)
	go func() {
		for {
			select {
			case <-t.ctx.Done():
				return
			case <-ticker.C:
				if time.Now().After(deadline) {
					return
				}
				elapsed := time.Since(startTime)
				rate := float64(atomic.LoadInt64(&totalSent)) / elapsed.Seconds()
				log.Printf("    Progress: %d sent (%.1f/sec)", atomic.LoadInt64(&totalSent), rate)
			}
		}
	}()

	wg.Wait()
	ticker.Stop()

	elapsed := time.Since(startTime)
	actualRate := float64(totalSent) / elapsed.Seconds()

	serverStats := t.mockServer.Stats()
	errorRate := float64(serverStats["total_errors"]) / float64(serverStats["total_requests"]) * 100

	result.EndTime = time.Now()
	result.Duration = elapsed
	result.Details["total_sent"] = totalSent
	result.Details["actual_rate"] = actualRate
	result.Details["target_rate"] = targetEmailsPerSecond
	result.Details["error_rate"] = errorRate
	result.Details["latency_p50"] = t.metrics.EmailLatencyP50.String()
	result.Details["latency_p99"] = t.metrics.EmailLatencyP99.String()

	if actualRate >= targetEmailsPerSecond*0.9 && errorRate < 1 {
		result.Status = "PASS"
		log.Printf("  ✓ PASS: %.0f emails/sec (%.2f%% errors)", actualRate, errorRate)
	} else {
		result.Status = "FAIL"
		log.Printf("  ✗ FAIL: %.0f emails/sec (target: %.0f), %.2f%% errors", actualRate, targetEmailsPerSecond, errorRate)
	}

	return result, nil
}

// Node processing functions
func (t *JourneyLoadTest) processTriggerNode(ctx context.Context) error {
	// Triggers don't actually execute, they just initiate enrollment
	return nil
}

func (t *JourneyLoadTest) processEmailNode(ctx context.Context) error {
	// Simulate email send to mock server
	if t.mockServer == nil {
		time.Sleep(50 * time.Millisecond) // Simulate latency
		return nil
	}

	mockURL := fmt.Sprintf("http://localhost:%d/send", t.config.MockESPPort)
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Post(mockURL, "application/json", strings.NewReader(`{"to":"test@test.com"}`))
	if err != nil {
		return err
	}
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("email send failed: %d", resp.StatusCode)
	}
	return nil
}

func (t *JourneyLoadTest) processDelayNode(ctx context.Context) error {
	// Delay nodes just schedule - no actual delay in test
	time.Sleep(100 * time.Microsecond) // Simulate minimal processing
	return nil
}

func (t *JourneyLoadTest) processConditionNode(ctx context.Context) error {
	// Simulate condition evaluation
	time.Sleep(time.Duration(rand.Intn(3)) * time.Millisecond)
	return nil
}

func (t *JourneyLoadTest) processSplitNode(ctx context.Context) error {
	// Simple hash-based split
	_ = fnv.New32a()
	return nil
}

func (t *JourneyLoadTest) processGoalNode(ctx context.Context) error {
	// Goal nodes just update status
	time.Sleep(100 * time.Microsecond)
	return nil
}

// simulateNodeExecution simulates a single node execution
func (t *JourneyLoadTest) simulateNodeExecution(ctx context.Context) error {
	// Pick a random node type to execute
	nodeTypes := []string{"email", "delay", "condition", "split", "goal"}
	nodeType := nodeTypes[rand.Intn(len(nodeTypes))]

	start := time.Now()
	var err error

	switch nodeType {
	case "email":
		err = t.processEmailNode(ctx)
	case "delay":
		err = t.processDelayNode(ctx)
	case "condition":
		err = t.processConditionNode(ctx)
	case "split":
		err = t.processSplitNode(ctx)
	case "goal":
		err = t.processGoalNode(ctx)
	}

	t.metrics.RecordNodeProcessing(nodeType, time.Since(start), err)
	return err
}

// simulateFullJourneyExecution simulates complete journey traversal
func (t *JourneyLoadTest) simulateFullJourneyExecution(ctx context.Context) error {
	for _, node := range t.testNodes {
		if node.Type == "trigger" {
			continue
		}

		start := time.Now()
		var err error

		switch node.Type {
		case "email":
			err = t.processEmailNode(ctx)
		case "delay":
			err = t.processDelayNode(ctx)
		case "condition":
			err = t.processConditionNode(ctx)
		case "split":
			err = t.processSplitNode(ctx)
		case "goal":
			err = t.processGoalNode(ctx)
		}

		t.metrics.RecordNodeProcessing(node.Type, time.Since(start), err)
		if err != nil {
			return err
		}
	}
	return nil
}

// Cleanup releases resources
func (t *JourneyLoadTest) Cleanup() {
	log.Println("\nCleaning up...")

	if t.mockServer != nil {
		t.mockServer.Stop()
		log.Println("  ✓ Stopped mock email server")
	}

	if t.tableManager != nil && t.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := t.tableManager.Cleanup(ctx); err != nil {
			log.Printf("  Warning: failed to cleanup test tables: %v", err)
		} else {
			log.Println("  ✓ Dropped test tables")
		}
	}

	if t.db != nil {
		t.db.Close()
		log.Println("  ✓ Closed PostgreSQL connection")
	}

	if t.redis != nil {
		t.redis.Close()
		log.Println("  ✓ Closed Redis connection")
	}
}

// GenerateReport produces the final test report
func (t *JourneyLoadTest) GenerateReport() string {
	m := t.metrics
	c := t.config

	var buf bytes.Buffer
	w := func(format string, args ...interface{}) {
		fmt.Fprintf(&buf, format+"\n", args...)
	}

	w("")
	w(strings.Repeat("=", 80))
	w("                    JOURNEY LOAD TEST REPORT")
	w(strings.Repeat("=", 80))
	w("")
	w("Test Configuration:")
	w("  Test Type:        %s", c.TestType)
	w("  Duration:         %v", m.TestDuration.Round(time.Second))
	w("  Target:           %d enrollments", c.TargetEnrollments)
	w("  Journey:          %s complexity (%d nodes)", c.JourneyComplexity, len(t.testNodes))
	w("  Workers:          %d", c.Workers)
	w("")

	// Enrollment Performance
	if phase, ok := m.PhaseResults["enrollment"]; ok && phase.Status != "SKIP" {
		w("ENROLLMENT PERFORMANCE")
		w(strings.Repeat("-", 40))
		w("  Enrolled:         %d / %d (%.1f%%)",
			m.EnrollmentsSucceeded, m.EnrollmentsAttempted,
			float64(m.EnrollmentsSucceeded)/float64(m.EnrollmentsAttempted)*100)
		w("  Rate:             %.0f enrollments/second", m.EnrollmentRate)
		w("  Latency P50:      %v", m.EnrollmentLatencyP50)
		w("  Latency P99:      %v", m.EnrollmentLatencyP99)
		w("  Status:           %s", phase.Status)
		w("")
	}

	// Execution Performance
	if phase, ok := m.PhaseResults["execution"]; ok && phase.Status != "SKIP" {
		w("EXECUTION PERFORMANCE")
		w(strings.Repeat("-", 40))
		w("  Processed:        %d / %d", m.ExecutionsSucceeded, m.ExecutionsAttempted)
		w("  Rate:             %.0f executions/second", m.ExecutionRate)
		w("  Completions:      %d (%.1f%%)", m.TotalCompleted, m.JourneyCompletionRate)
		w("  Conversions:      %d (%.1f%%)", m.TotalConverted, m.JourneyConversionRate)
		w("  Status:           %s", phase.Status)
		w("")
	}

	// Node Performance
	if phase, ok := m.PhaseResults["node"]; ok && phase.Status != "SKIP" {
		w("NODE PERFORMANCE")
		w(strings.Repeat("-", 40))
		for _, nodeType := range []string{"trigger", "email", "delay", "condition", "split", "goal"} {
			avgTime := m.NodeAvgTimes[nodeType]
			successCount := m.NodeSuccessCounts[nodeType]
			errorRate := m.NodeErrorRates[nodeType]

			if nodeType == "trigger" {
				w("  %-12s      N/A", nodeType+":")
			} else if avgTime > 0 || successCount > 0 {
				w("  %-12s      %.2fms avg (%.1f%% success)", nodeType+":", avgTime.Seconds()*1000, 100-errorRate)
			}
		}
		w("  Status:           %s", phase.Status)
		w("")
	}

	// Email Performance
	if phase, ok := m.PhaseResults["email"]; ok && phase.Status != "SKIP" {
		w("EMAIL PERFORMANCE")
		w(strings.Repeat("-", 40))
		w("  Emails Sent:      %d", m.EmailsSent)
		w("  Rate:             %.0f emails/second", m.EmailsPerSecond)
		w("  Error Rate:       %.2f%%", m.EmailErrorRate)
		w("  Latency P50:      %v", m.EmailLatencyP50)
		w("  Latency P99:      %v", m.EmailLatencyP99)
		w("  Status:           %s", phase.Status)
		w("")
	}

	// Capacity Projections
	w("CAPACITY PROJECTIONS")
	w(strings.Repeat("-", 40))
	w("  Daily Capacity:   ~%dM journey executions", m.ProjectedDailyCapacity/1_000_000)
	w("  Sustained Rate:   ~%.0f/second enrollments", m.EnrollmentRate)
	if m.HeadroomPercent >= 0 {
		w("  Headroom:         %.1f%% above target", m.HeadroomPercent)
	} else {
		w("  Deficit:          %.1f%% below target", math.Abs(m.HeadroomPercent))
	}
	w("  Bottleneck:       %s", m.BottleneckComponent)
	w("")

	// Errors Summary
	if m.TotalErrors > 0 {
		w("ERRORS SUMMARY")
		w(strings.Repeat("-", 40))
		w("  Total Errors:     %d", m.TotalErrors)
		for errType, count := range m.ErrorsByType {
			w("    %-14s  %d", errType+":", count)
		}
		w("")
	}

	// Overall Result
	w(strings.Repeat("=", 80))

	allPass := true
	for _, phase := range m.PhaseResults {
		if phase.Status == "FAIL" {
			allPass = false
			break
		}
	}

	if allPass {
		w("OVERALL: PASS - System can handle mass journey traffic")
	} else {
		w("OVERALL: FAIL - System does not meet journey load targets")
		w("")
		w("Recommendations:")
		if m.BottleneckComponent != "None identified" {
			w("  - Address bottleneck: %s", m.BottleneckComponent)
		}
		if m.TotalErrors > 0 {
			w("  - Investigate %d errors that occurred during test", m.TotalErrors)
		}
		if m.HeadroomPercent < 0 {
			w("  - Increase capacity by at least %.1f%% to meet targets", math.Abs(m.HeadroomPercent))
		}
	}
	w(strings.Repeat("=", 80))

	return buf.String()
}

// =============================================================================
// MAIN
// =============================================================================

func main() {
	// Parse command line flags
	config := DefaultJourneyConfig()

	var durationStr string

	flag.StringVar(&config.PostgresURL, "postgres", "",
		"PostgreSQL connection URL")
	flag.StringVar(&config.RedisURL, "redis", "",
		"Redis connection URL or host:port")
	flag.StringVar(&config.TestType, "test", config.TestType,
		"Test type: all, enrollment, execution, node, segment, sustained, spike, email")
	flag.StringVar(&durationStr, "duration", "5m",
		"Test duration (e.g., 5m, 1h)")
	flag.Int64Var(&config.TargetEnrollments, "enrollments", config.TargetEnrollments,
		"Target number of enrollments")
	flag.IntVar(&config.Workers, "workers", config.Workers,
		"Number of worker goroutines")
	flag.StringVar(&config.JourneyComplexity, "complexity", config.JourneyComplexity,
		"Journey complexity: simple, medium, complex")
	flag.Int64Var(&config.SegmentSize, "segment-size", config.SegmentSize,
		"Segment size for segment-driven test")
	flag.Float64Var(&config.SpikeMultiplier, "spike-multiplier", config.SpikeMultiplier,
		"Spike multiplier for spike test")
	flag.IntVar(&config.MockESPPort, "mock-esp-port", config.MockESPPort,
		"Port for mock ESP server")
	flag.BoolVar(&config.MockMode, "mock-mode", config.MockMode,
		"Use mock/test tables instead of production")

	flag.Parse()

	// Parse duration string
	if d, err := time.ParseDuration(durationStr); err == nil {
		config.Duration = d
	}

	// Print banner
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                    JOURNEY SYSTEM LOAD TEST                                   ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Create runner
	runner := NewJourneyLoadTest(config)

	// Setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("\nReceived interrupt signal, shutting down gracefully...")
		cancel()
	}()

	// Initialize
	if err := runner.Initialize(ctx); err != nil {
		log.Fatalf("Failed to initialize: %v", err)
	}
	defer runner.Cleanup()

	// Run tests
	if err := runner.Run(ctx); err != nil && err != context.Canceled {
		log.Printf("Test error: %v", err)
	}

	// Generate and print report
	report := runner.GenerateReport()
	fmt.Println(report)

	// Exit with appropriate code
	allPass := true
	for _, phase := range runner.metrics.PhaseResults {
		if phase.Status == "FAIL" {
			allPass = false
			break
		}
	}

	if !allPass {
		os.Exit(1)
	}
}
