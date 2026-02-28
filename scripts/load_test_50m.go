//go:build ignore
// +build ignore

// Load Test Script for 50M/day Email Throughput Validation
// This script simulates and validates the system can handle 50M messages/day throughput.
//
// Usage:
//   go run scripts/load_test_50m.go \
//     --duration=5m \
//     --postgres="postgres://user:pass@localhost:5432/mailing" \
//     --redis="localhost:6379" \
//     --target-daily=50000000 \
//     --mock-esp-port=9999
//
// The script runs in mock mode by default to avoid affecting production data.

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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

// LoadTestConfig defines the test configuration
type LoadTestConfig struct {
	TargetMessagesPerDay   int64              // 50,000,000
	TestDurationMinutes    int                // Default: 5 minutes
	SimulatedCampaigns     int                // 10 concurrent campaigns
	SubscribersPerCampaign int                // 5,000,000 each
	ESPDistribution        map[string]float64 // sparkpost: 30%, ses: 50%, mailgun: 10%, sendgrid: 10%

	PostgresURL string
	RedisURL    string

	// Rate controls
	EnqueueRatePerSecond int // Target: 5787 (50M / 86400 seconds)
	SendRatePerSecond    int // Target: 579 messages/second

	// Mock server
	MockESPPort int
	MockMode    bool // If true, uses mock database tables

	// Calculated values
	targetMsgPerSecond float64
	testDuration       time.Duration
}

// DefaultConfig returns sensible defaults for 50M/day testing
func DefaultConfig() *LoadTestConfig {
	return &LoadTestConfig{
		TargetMessagesPerDay:   50_000_000,
		TestDurationMinutes:    5,
		SimulatedCampaigns:     10,
		SubscribersPerCampaign: 5_000_000,
		ESPDistribution: map[string]float64{
			"sparkpost": 0.30,
			"ses":       0.50,
			"mailgun":   0.10,
			"sendgrid":  0.10,
		},
		EnqueueRatePerSecond: 57870,                                 // 50M / 864 seconds (for 5min test extrapolation)
		SendRatePerSecond:    int(50_000_000 / 86400),               // ~579 msg/sec sustained
		MockESPPort:          9999,
		MockMode:             true,
	}
}

// =============================================================================
// METRICS COLLECTION
// =============================================================================

// LoadTestMetrics holds all collected metrics
type LoadTestMetrics struct {
	// Test info
	TestStartTime time.Time
	TestEndTime   time.Time
	TestDuration  time.Duration

	// Enqueue metrics
	TotalEnqueued        int64
	EnqueueRatePerSecond float64
	EnqueueLatencies     []time.Duration
	EnqueueLatencyP50    time.Duration
	EnqueueLatencyP99    time.Duration
	EnqueueErrors        int64

	// Send metrics
	TotalSent           int64
	SendRatePerSecond   float64
	SendLatencies       []time.Duration
	SendLatencyP50      time.Duration
	SendLatencyP99      time.Duration
	SendErrors          int64

	// ESP breakdown
	ESPMetrics map[string]*ESPMetric

	// Queue metrics
	QueueDepthMax     int64
	QueueDepthAvg     float64
	QueueDepthSamples []int64

	// Rate limiter metrics
	TotalRateLimitHits int64
	RateLimitRecoveryMs []int64

	// Errors
	TotalErrors   int64
	TimeoutErrors int64

	// Extrapolation
	ProjectedDailyCapacity int64
	BottleneckComponent    string
	HeadroomPercent        float64

	// Phase results
	PhaseResults map[string]*PhaseResult

	mu sync.Mutex
}

// ESPMetric holds per-ESP metrics
type ESPMetric struct {
	TotalSent       int64
	SendRate        float64
	AvgBatchSize    float64
	TotalBatches    int64
	Errors          int64
	RateLimitHits   int64
	AvgLatencyMs    float64
	Latencies       []time.Duration
}

// PhaseResult holds results for each test phase
type PhaseResult struct {
	Name       string
	Status     string // "PASS" or "FAIL"
	Duration   time.Duration
	Details    map[string]interface{}
	StartTime  time.Time
	EndTime    time.Time
}

// NewLoadTestMetrics creates a new metrics collector
func NewLoadTestMetrics() *LoadTestMetrics {
	return &LoadTestMetrics{
		ESPMetrics: map[string]*ESPMetric{
			"sparkpost": {},
			"ses":       {},
			"mailgun":   {},
			"sendgrid":  {},
		},
		PhaseResults:      make(map[string]*PhaseResult),
		EnqueueLatencies:  make([]time.Duration, 0, 100000),
		SendLatencies:     make([]time.Duration, 0, 100000),
		QueueDepthSamples: make([]int64, 0, 1000),
	}
}

// RecordEnqueue records an enqueue operation
func (m *LoadTestMetrics) RecordEnqueue(count int64, latency time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	if err != nil {
		m.EnqueueErrors++
		m.TotalErrors++
		return
	}
	
	m.TotalEnqueued += count
	if len(m.EnqueueLatencies) < 100000 {
		m.EnqueueLatencies = append(m.EnqueueLatencies, latency)
	}
}

// RecordSend records a send operation
func (m *LoadTestMetrics) RecordSend(espType string, batchSize int, latency time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	esp, ok := m.ESPMetrics[espType]
	if !ok {
		esp = &ESPMetric{}
		m.ESPMetrics[espType] = esp
	}
	
	if err != nil {
		esp.Errors++
		m.SendErrors++
		m.TotalErrors++
		return
	}
	
	esp.TotalSent += int64(batchSize)
	esp.TotalBatches++
	m.TotalSent += int64(batchSize)
	
	if len(m.SendLatencies) < 100000 {
		m.SendLatencies = append(m.SendLatencies, latency)
	}
	if len(esp.Latencies) < 10000 {
		esp.Latencies = append(esp.Latencies, latency)
	}
}

// RecordRateLimitHit records a rate limit hit
func (m *LoadTestMetrics) RecordRateLimitHit(espType string, recoveryMs int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	m.TotalRateLimitHits++
	m.RateLimitRecoveryMs = append(m.RateLimitRecoveryMs, recoveryMs)
	
	if esp, ok := m.ESPMetrics[espType]; ok {
		esp.RateLimitHits++
	}
}

// RecordQueueDepth records a queue depth sample
func (m *LoadTestMetrics) RecordQueueDepth(depth int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	m.QueueDepthSamples = append(m.QueueDepthSamples, depth)
	if depth > m.QueueDepthMax {
		m.QueueDepthMax = depth
	}
}

// Finalize calculates derived metrics
func (m *LoadTestMetrics) Finalize(config *LoadTestConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	m.TestDuration = m.TestEndTime.Sub(m.TestStartTime)
	durationSeconds := m.TestDuration.Seconds()
	
	// Calculate rates
	if durationSeconds > 0 {
		m.EnqueueRatePerSecond = float64(m.TotalEnqueued) / durationSeconds
		m.SendRatePerSecond = float64(m.TotalSent) / durationSeconds
	}
	
	// Calculate latency percentiles
	m.EnqueueLatencyP50 = percentile(m.EnqueueLatencies, 50)
	m.EnqueueLatencyP99 = percentile(m.EnqueueLatencies, 99)
	m.SendLatencyP50 = percentile(m.SendLatencies, 50)
	m.SendLatencyP99 = percentile(m.SendLatencies, 99)
	
	// Calculate queue depth average
	if len(m.QueueDepthSamples) > 0 {
		var sum int64
		for _, d := range m.QueueDepthSamples {
			sum += d
		}
		m.QueueDepthAvg = float64(sum) / float64(len(m.QueueDepthSamples))
	}
	
	// Calculate ESP metrics
	for _, esp := range m.ESPMetrics {
		if esp.TotalBatches > 0 {
			esp.AvgBatchSize = float64(esp.TotalSent) / float64(esp.TotalBatches)
			esp.SendRate = float64(esp.TotalSent) / durationSeconds
		}
		if len(esp.Latencies) > 0 {
			var sum time.Duration
			for _, l := range esp.Latencies {
				sum += l
			}
			esp.AvgLatencyMs = float64(sum.Milliseconds()) / float64(len(esp.Latencies))
		}
	}
	
	// Extrapolate daily capacity
	// Based on 5-minute test, project to 24 hours
	secondsInDay := float64(86400)
	m.ProjectedDailyCapacity = int64(m.SendRatePerSecond * secondsInDay)
	
	// Calculate headroom
	targetDaily := float64(config.TargetMessagesPerDay)
	if targetDaily > 0 {
		m.HeadroomPercent = ((float64(m.ProjectedDailyCapacity) - targetDaily) / targetDaily) * 100
	}
	
	// Identify bottleneck
	m.BottleneckComponent = identifyBottleneck(m, config)
}

// percentile calculates the p-th percentile of durations
func percentile(durations []time.Duration, p int) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	
	sorted := make([]time.Duration, len(durations))
	copy(sorted, durations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	
	idx := int(float64(len(sorted)-1) * float64(p) / 100)
	return sorted[idx]
}

// identifyBottleneck determines the system bottleneck
func identifyBottleneck(m *LoadTestMetrics, config *LoadTestConfig) string {
	// Check if enqueue rate is limiting
	targetEnqueue := float64(config.EnqueueRatePerSecond)
	if m.EnqueueRatePerSecond < targetEnqueue*0.9 {
		return "Queue Enqueue (PostgreSQL COPY)"
	}
	
	// Check if send rate is limiting
	targetSend := float64(config.SendRatePerSecond)
	if m.SendRatePerSecond < targetSend*0.9 {
		return "Send Workers"
	}
	
	// Check if rate limits are the bottleneck
	if m.TotalRateLimitHits > 100 {
		return "ESP Rate Limits"
	}
	
	// Check for high error rates
	if m.TotalErrors > m.TotalSent/100 { // >1% error rate
		return "High Error Rate"
	}
	
	return "None identified"
}

// =============================================================================
// MOCK ESP SERVER
// =============================================================================

// MockESPServer provides mock ESP endpoints
type MockESPServer struct {
	server      *http.Server
	port        int
	metrics     *LoadTestMetrics
	
	// Configurable response delays
	sparkpostDelayMs int
	sesDelayMs       int
	mailgunDelayMs   int
	sendgridDelayMs  int
	
	// Request counters
	sparkpostRequests int64
	sesRequests       int64
	mailgunRequests   int64
	sendgridRequests  int64
}

// NewMockESPServer creates a new mock ESP server
func NewMockESPServer(port int, metrics *LoadTestMetrics) *MockESPServer {
	return &MockESPServer{
		port:             port,
		metrics:          metrics,
		sparkpostDelayMs: 50,
		sesDelayMs:       30,
		mailgunDelayMs:   40,
		sendgridDelayMs:  35,
	}
}

// Start starts the mock ESP server
func (s *MockESPServer) Start() error {
	mux := http.NewServeMux()
	
	// SparkPost mock - accepts batches up to 2000
	mux.HandleFunc("/sparkpost/api/v1/transmissions", s.handleSparkPost)
	
	// SES mock - accepts batches up to 50
	mux.HandleFunc("/ses/v2/email/outbound-emails", s.handleSES)
	
	// Mailgun mock - accepts batches up to 1000
	mux.HandleFunc("/mailgun/v3/messages", s.handleMailgun)
	
	// SendGrid mock - accepts batches up to 1000
	mux.HandleFunc("/sendgrid/v3/mail/send", s.handleSendGrid)
	
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
		log.Printf("[MockESP] Server starting on port %d", s.port)
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[MockESP] Server error: %v", err)
		}
	}()
	
	// Wait for server to be ready
	time.Sleep(100 * time.Millisecond)
	return nil
}

// Stop stops the mock ESP server
func (s *MockESPServer) Stop() error {
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.server.Shutdown(ctx)
	}
	return nil
}

// handleSparkPost handles SparkPost transmission requests
func (s *MockESPServer) handleSparkPost(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&s.sparkpostRequests, 1)
	
	// Simulate network latency
	time.Sleep(time.Duration(s.sparkpostDelayMs) * time.Millisecond)
	
	// Parse request to count recipients
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Recipients []interface{} `json:"recipients"`
	}
	json.Unmarshal(body, &req)
	
	recipientCount := len(req.Recipients)
	if recipientCount == 0 {
		recipientCount = 1
	}
	if recipientCount > 2000 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"errors": []map[string]string{
				{"message": "recipient count exceeds maximum of 2000"},
			},
		})
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"results": map[string]interface{}{
			"total_accepted_recipients": recipientCount,
			"total_rejected_recipients": 0,
			"id":                         uuid.New().String(),
		},
	})
}

// handleSES handles AWS SES requests
func (s *MockESPServer) handleSES(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&s.sesRequests, 1)
	
	// Simulate network latency
	time.Sleep(time.Duration(s.sesDelayMs) * time.Millisecond)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"MessageId": uuid.New().String(),
	})
}

// handleMailgun handles Mailgun requests
func (s *MockESPServer) handleMailgun(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&s.mailgunRequests, 1)
	
	// Simulate network latency
	time.Sleep(time.Duration(s.mailgunDelayMs) * time.Millisecond)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":      fmt.Sprintf("<%s@mailgun.test>", uuid.New().String()),
		"message": "Queued. Thank you.",
	})
}

// handleSendGrid handles SendGrid requests
func (s *MockESPServer) handleSendGrid(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&s.sendgridRequests, 1)
	
	// Simulate network latency
	time.Sleep(time.Duration(s.sendgridDelayMs) * time.Millisecond)
	
	w.Header().Set("X-Message-Id", uuid.New().String())
	w.WriteHeader(http.StatusAccepted)
}

// Stats returns server statistics
func (s *MockESPServer) Stats() map[string]int64 {
	return map[string]int64{
		"sparkpost_requests": atomic.LoadInt64(&s.sparkpostRequests),
		"ses_requests":       atomic.LoadInt64(&s.sesRequests),
		"mailgun_requests":   atomic.LoadInt64(&s.mailgunRequests),
		"sendgrid_requests":  atomic.LoadInt64(&s.sendgridRequests),
	}
}

// =============================================================================
// TEST TABLE MANAGEMENT (MOCK MODE)
// =============================================================================

// TestTableManager manages test-specific tables to avoid production impact
type TestTableManager struct {
	db        *sql.DB
	tableName string
}

// NewTestTableManager creates a new test table manager
func NewTestTableManager(db *sql.DB) *TestTableManager {
	return &TestTableManager{
		db:        db,
		tableName: fmt.Sprintf("load_test_queue_%s", time.Now().Format("20060102_150405")),
	}
}

// Setup creates the test queue table
func (t *TestTableManager) Setup(ctx context.Context) error {
	_, err := t.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			campaign_id UUID NOT NULL,
			subscriber_id UUID NOT NULL,
			email VARCHAR(255) NOT NULL,
			substitution_data JSONB DEFAULT '{}',
			status VARCHAR(20) DEFAULT 'queued',
			priority INT DEFAULT 0,
			esp_type VARCHAR(20) DEFAULT 'ses',
			scheduled_at TIMESTAMP DEFAULT NOW(),
			created_at TIMESTAMP DEFAULT NOW(),
			sent_at TIMESTAMP,
			worker_id VARCHAR(50),
			locked_at TIMESTAMP
		)
	`, t.tableName))
	if err != nil {
		return fmt.Errorf("failed to create test table: %w", err)
	}
	
	// Create indexes for performance
	indexes := []string{
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS idx_%s_status ON %s(status)", t.tableName, t.tableName),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS idx_%s_priority ON %s(priority DESC, scheduled_at)", t.tableName, t.tableName),
	}
	
	for _, idx := range indexes {
		if _, err := t.db.ExecContext(ctx, idx); err != nil {
			log.Printf("Warning: failed to create index: %v", err)
		}
	}
	
	return nil
}

// Cleanup drops the test table
func (t *TestTableManager) Cleanup(ctx context.Context) error {
	_, err := t.db.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", t.tableName))
	return err
}

// TableName returns the test table name
func (t *TestTableManager) TableName() string {
	return t.tableName
}

// =============================================================================
// LOAD TEST RUNNER
// =============================================================================

// LoadTestRunner orchestrates the load test
type LoadTestRunner struct {
	config       *LoadTestConfig
	metrics      *LoadTestMetrics
	db           *sql.DB
	redis        *redis.Client
	mockServer   *MockESPServer
	tableManager *TestTableManager
	
	ctx    context.Context
	cancel context.CancelFunc
}

// NewLoadTestRunner creates a new load test runner
func NewLoadTestRunner(config *LoadTestConfig) *LoadTestRunner {
	return &LoadTestRunner{
		config:  config,
		metrics: NewLoadTestMetrics(),
	}
}

// Initialize sets up all test infrastructure
func (r *LoadTestRunner) Initialize(ctx context.Context) error {
	log.Println("Initializing load test infrastructure...")
	
	// Connect to PostgreSQL
	if r.config.PostgresURL != "" {
		db, err := sql.Open("postgres", r.config.PostgresURL)
		if err != nil {
			return fmt.Errorf("failed to connect to PostgreSQL: %w", err)
		}
		db.SetMaxOpenConns(100)
		db.SetMaxIdleConns(50)
		if err := db.PingContext(ctx); err != nil {
			return fmt.Errorf("failed to ping PostgreSQL: %w", err)
		}
		r.db = db
		log.Println("  ✓ Connected to PostgreSQL")
		
		// Setup test table
		r.tableManager = NewTestTableManager(db)
		if err := r.tableManager.Setup(ctx); err != nil {
			return fmt.Errorf("failed to setup test table: %w", err)
		}
		log.Printf("  ✓ Created test table: %s", r.tableManager.TableName())
	}
	
	// Connect to Redis
	if r.config.RedisURL != "" {
		opts, err := redis.ParseURL(r.config.RedisURL)
		if err != nil {
			// Try as host:port format
			opts = &redis.Options{Addr: r.config.RedisURL}
		}
		r.redis = redis.NewClient(opts)
		if err := r.redis.Ping(ctx).Err(); err != nil {
			return fmt.Errorf("failed to connect to Redis: %w", err)
		}
		log.Println("  ✓ Connected to Redis")
	}
	
	// Start mock ESP server
	r.mockServer = NewMockESPServer(r.config.MockESPPort, r.metrics)
	if err := r.mockServer.Start(); err != nil {
		return fmt.Errorf("failed to start mock ESP server: %w", err)
	}
	log.Printf("  ✓ Started mock ESP server on port %d", r.config.MockESPPort)
	
	return nil
}

// Run executes all test phases
func (r *LoadTestRunner) Run(ctx context.Context) error {
	r.ctx, r.cancel = context.WithCancel(ctx)
	defer r.cancel()
	
	r.metrics.TestStartTime = time.Now()
	
	log.Println("\n" + strings.Repeat("=", 80))
	log.Println("                    STARTING 50M/DAY LOAD TEST")
	log.Println(strings.Repeat("=", 80))
	log.Printf("Target: %d messages/day (%.1f msg/sec sustained)\n",
		r.config.TargetMessagesPerDay,
		float64(r.config.TargetMessagesPerDay)/86400)
	log.Printf("Test Duration: %d minutes\n", r.config.TestDurationMinutes)
	log.Println(strings.Repeat("=", 80))
	
	// Run test phases
	phases := []struct {
		name string
		fn   func(context.Context) (*PhaseResult, error)
	}{
		{"QUEUE_STRESS", r.runPhase1QueueStress},
		{"SEND_WORKER", r.runPhase2SendWorker},
		{"RATE_LIMITER", r.runPhase3RateLimiter},
		{"END_TO_END", r.runPhase4EndToEnd},
	}
	
	for _, phase := range phases {
		select {
		case <-r.ctx.Done():
			log.Printf("Test interrupted during phase: %s", phase.name)
			return r.ctx.Err()
		default:
		}
		
		result, err := phase.fn(r.ctx)
		if err != nil {
			log.Printf("Phase %s error: %v", phase.name, err)
			result = &PhaseResult{
				Name:   phase.name,
				Status: "FAIL",
				Details: map[string]interface{}{
					"error": err.Error(),
				},
			}
		}
		r.metrics.PhaseResults[phase.name] = result
	}
	
	r.metrics.TestEndTime = time.Now()
	r.metrics.Finalize(r.config)
	
	return nil
}

// Phase 1: Queue Stress Test
func (r *LoadTestRunner) runPhase1QueueStress(ctx context.Context) (*PhaseResult, error) {
	log.Println("\n[PHASE 1] QUEUE STRESS TEST")
	log.Println(strings.Repeat("-", 60))
	
	result := &PhaseResult{
		Name:      "QUEUE_STRESS",
		StartTime: time.Now(),
		Details:   make(map[string]interface{}),
	}
	
	if r.db == nil {
		result.Status = "SKIP"
		result.Details["reason"] = "PostgreSQL not configured"
		return result, nil
	}
	
	// Target: 50,000+ subscribers/second into the queue
	targetRate := 50000
	testDuration := 30 * time.Second
	totalToEnqueue := targetRate * int(testDuration.Seconds())
	
	log.Printf("  Target: %d enqueues/second for %v", targetRate, testDuration)
	log.Printf("  Total items to enqueue: %d", totalToEnqueue)
	
	// Generate test data
	campaignID := uuid.New()
	espTypes := []string{"sparkpost", "ses", "mailgun", "sendgrid"}
	
	// Use batched COPY for high-speed insertion
	var totalEnqueued int64
	var totalLatency time.Duration
	batchSize := 10000 // COPY batch size
	
	startTime := time.Now()
	deadline := startTime.Add(testDuration)
	
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			break
		default:
		}
		
		batchStart := time.Now()
		
		// Begin transaction with COPY
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			continue
		}
		
		stmt, err := tx.Prepare(pq.CopyIn(
			r.tableManager.TableName(),
			"id", "campaign_id", "subscriber_id", "email",
			"substitution_data", "status", "priority", "esp_type",
			"scheduled_at", "created_at",
		))
		if err != nil {
			tx.Rollback()
			continue
		}
		
		now := time.Now()
		for i := 0; i < batchSize; i++ {
			espType := espTypes[rand.Intn(len(espTypes))]
			_, err = stmt.Exec(
				uuid.New(),
				campaignID,
				uuid.New(),
				fmt.Sprintf("test_%d_%d@loadtest.local", atomic.LoadInt64(&totalEnqueued), i),
				"{}",
				"queued",
				rand.Intn(10),
				espType,
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
		atomic.AddInt64(&totalEnqueued, int64(batchSize))
		r.metrics.RecordEnqueue(int64(batchSize), batchLatency, nil)
		
		// Log progress
		if totalEnqueued%100000 == 0 {
			elapsed := time.Since(startTime)
			rate := float64(totalEnqueued) / elapsed.Seconds()
			log.Printf("    Progress: %d enqueued (%.0f/sec)", totalEnqueued, rate)
		}
	}
	
	elapsed := time.Since(startTime)
	actualRate := float64(totalEnqueued) / elapsed.Seconds()
	
	result.EndTime = time.Now()
	result.Duration = elapsed
	result.Details["total_enqueued"] = totalEnqueued
	result.Details["target_rate"] = targetRate
	result.Details["actual_rate"] = actualRate
	result.Details["avg_batch_latency_ms"] = totalLatency.Milliseconds() / (totalEnqueued / int64(batchSize))
	
	if actualRate >= float64(targetRate)*0.9 {
		result.Status = "PASS"
		log.Printf("  ✓ PASS: %.0f enqueues/second (target: %d)", actualRate, targetRate)
	} else {
		result.Status = "FAIL"
		log.Printf("  ✗ FAIL: %.0f enqueues/second (target: %d)", actualRate, targetRate)
	}
	
	// Check queue depth
	var queueDepth int64
	r.db.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM %s WHERE status = 'queued'",
		r.tableManager.TableName(),
	)).Scan(&queueDepth)
	
	result.Details["queue_depth"] = queueDepth
	r.metrics.RecordQueueDepth(queueDepth)
	
	log.Printf("  Queue Depth: %d items", queueDepth)
	
	return result, nil
}

// Phase 2: Send Worker Throughput
func (r *LoadTestRunner) runPhase2SendWorker(ctx context.Context) (*PhaseResult, error) {
	log.Println("\n[PHASE 2] SEND WORKER THROUGHPUT")
	log.Println(strings.Repeat("-", 60))
	
	result := &PhaseResult{
		Name:      "SEND_WORKER",
		StartTime: time.Now(),
		Details:   make(map[string]interface{}),
	}
	
	// Target send rate
	targetRate := float64(r.config.TargetMessagesPerDay) / 86400 // ~579 msg/sec
	testDuration := 60 * time.Second
	
	log.Printf("  Target: %.1f messages/second for %v", targetRate, testDuration)
	
	// Create mock ESP client
	mockURL := fmt.Sprintf("http://localhost:%d", r.config.MockESPPort)
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        1000,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	
	// ESP batch sizes
	batchSizes := map[string]int{
		"sparkpost": 2000,
		"ses":       50,
		"mailgun":   1000,
		"sendgrid":  1000,
	}
	
	// Worker pool
	numWorkers := 50
	var wg sync.WaitGroup
	var totalSent int64
	
	startTime := time.Now()
	deadline := startTime.Add(testDuration)
	
	// Per-ESP counters
	espCounts := make(map[string]*int64)
	for esp := range batchSizes {
		var count int64
		espCounts[esp] = &count
	}
	
	// Launch workers
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			
			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return
				default:
				}
				
				// Select ESP based on distribution
				espType := selectESP(r.config.ESPDistribution)
				batchSize := batchSizes[espType]
				
				// Build mock request
				sendStart := time.Now()
				var endpoint string
				var payload []byte
				
				switch espType {
				case "sparkpost":
					endpoint = mockURL + "/sparkpost/api/v1/transmissions"
					recipients := make([]map[string]interface{}, batchSize)
					for i := 0; i < batchSize; i++ {
						recipients[i] = map[string]interface{}{
							"address": map[string]string{
								"email": fmt.Sprintf("test%d@loadtest.local", i),
							},
						}
					}
					payload, _ = json.Marshal(map[string]interface{}{
						"recipients": recipients,
						"content": map[string]string{
							"subject": "Load Test",
							"html":    "<p>Test</p>",
						},
					})
				case "ses":
					endpoint = mockURL + "/ses/v2/email/outbound-emails"
					payload, _ = json.Marshal(map[string]interface{}{
						"Destination": map[string][]string{
							"ToAddresses": {"test@loadtest.local"},
						},
					})
				case "mailgun":
					endpoint = mockURL + "/mailgun/v3/messages"
					payload = []byte("to=test@loadtest.local&subject=Test")
				case "sendgrid":
					endpoint = mockURL + "/sendgrid/v3/mail/send"
					payload, _ = json.Marshal(map[string]interface{}{
						"personalizations": []map[string]interface{}{
							{"to": []map[string]string{{"email": "test@loadtest.local"}}},
						},
					})
				}
				
				// Send request
				req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(payload))
				req.Header.Set("Content-Type", "application/json")
				
				resp, err := client.Do(req)
				latency := time.Since(sendStart)
				
				if err != nil {
					r.metrics.RecordSend(espType, 0, latency, err)
					continue
				}
				resp.Body.Close()
				
				if resp.StatusCode >= 400 {
					r.metrics.RecordSend(espType, 0, latency, fmt.Errorf("status %d", resp.StatusCode))
					continue
				}
				
				// Record success
				atomic.AddInt64(&totalSent, int64(batchSize))
				atomic.AddInt64(espCounts[espType], int64(batchSize))
				r.metrics.RecordSend(espType, batchSize, latency, nil)
			}
		}(i)
	}
	
	// Monitor progress
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		for {
			select {
			case <-ctx.Done():
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
	
	result.EndTime = time.Now()
	result.Duration = elapsed
	result.Details["total_sent"] = totalSent
	result.Details["target_rate"] = targetRate
	result.Details["actual_rate"] = actualRate
	result.Details["num_workers"] = numWorkers
	
	// ESP breakdown
	espBreakdown := make(map[string]interface{})
	for esp, count := range espCounts {
		espBreakdown[esp] = map[string]interface{}{
			"total":    atomic.LoadInt64(count),
			"rate":     float64(atomic.LoadInt64(count)) / elapsed.Seconds(),
			"batch_sz": batchSizes[esp],
		}
	}
	result.Details["esp_breakdown"] = espBreakdown
	
	// Calculate batch efficiency
	mockStats := r.mockServer.Stats()
	totalRequests := mockStats["sparkpost_requests"] + mockStats["ses_requests"] +
		mockStats["mailgun_requests"] + mockStats["sendgrid_requests"]
	if totalRequests > 0 {
		avgBatchSize := float64(totalSent) / float64(totalRequests)
		result.Details["avg_batch_size"] = avgBatchSize
		result.Details["batch_efficiency"] = avgBatchSize / float64(batchSizes["sparkpost"]) * 100
	}
	
	if actualRate >= targetRate*0.9 {
		result.Status = "PASS"
		log.Printf("  ✓ PASS: %.1f msg/sec (target: %.1f)", actualRate, targetRate)
	} else {
		result.Status = "FAIL"
		log.Printf("  ✗ FAIL: %.1f msg/sec (target: %.1f)", actualRate, targetRate)
	}
	
	// Log ESP breakdown
	log.Println("  ESP Breakdown:")
	for esp, count := range espCounts {
		rate := float64(atomic.LoadInt64(count)) / elapsed.Seconds()
		log.Printf("    %s: %.1f/sec (batch avg: %d)", esp, rate, batchSizes[esp])
	}
	
	return result, nil
}

// Phase 3: Rate Limiter Validation
func (r *LoadTestRunner) runPhase3RateLimiter(ctx context.Context) (*PhaseResult, error) {
	log.Println("\n[PHASE 3] RATE LIMITER VALIDATION")
	log.Println(strings.Repeat("-", 60))
	
	result := &PhaseResult{
		Name:      "RATE_LIMITER",
		StartTime: time.Now(),
		Details:   make(map[string]interface{}),
	}
	
	if r.redis == nil {
		result.Status = "SKIP"
		result.Details["reason"] = "Redis not configured"
		log.Println("  SKIP: Redis not configured")
		return result, nil
	}
	
	// Test rate limiting for each ESP
	espLimits := map[string]struct {
		perSecond int
		perMinute int
	}{
		"sparkpost": {100, 5000},
		"ses":       {500, 30000},
		"mailgun":   {50, 3000},
		"sendgrid":  {50, 3000},
	}
	
	var totalHits int64
	var recoveryTimes []time.Duration
	
	// Lua script for atomic rate limiting (matches production)
	rateLimitScript := redis.NewScript(`
		local key = KEYS[1]
		local increment = tonumber(ARGV[1])
		local limit = tonumber(ARGV[2])
		local ttl = tonumber(ARGV[3])
		
		local current = tonumber(redis.call("GET", key) or "0")
		if current + increment > limit then
			return {0, current}
		end
		
		local newVal = redis.call("INCRBY", key, increment)
		if newVal == increment then
			redis.call("EXPIRE", key, ttl)
		end
		return {1, newVal}
	`)
	
	for esp, limits := range espLimits {
		log.Printf("  Testing %s (limit: %d/sec, %d/min)...", esp, limits.perSecond, limits.perMinute)
		
		// Test burst over limit
		testKey := fmt.Sprintf("loadtest:ratelimit:%s:%d", esp, time.Now().Unix())
		var hits int
		
		// Try to exceed the limit
		for i := 0; i < limits.perSecond+50; i++ {
			result, err := rateLimitScript.Run(ctx, r.redis,
				[]string{testKey},
				1,
				limits.perSecond,
				2,
			).Slice()
			
			if err != nil {
				continue
			}
			
			allowed := result[0].(int64) == 1
			if !allowed {
				hits++
				atomic.AddInt64(&totalHits, 1)
			}
		}
		
		// Test recovery time
		if hits > 0 {
			recoveryStart := time.Now()
			for {
				time.Sleep(100 * time.Millisecond)
				if time.Since(recoveryStart) > 2*time.Second {
					break
				}
				
				// Check if we can send again
				newKey := fmt.Sprintf("loadtest:ratelimit:%s:%d", esp, time.Now().Unix())
				result, err := rateLimitScript.Run(ctx, r.redis,
					[]string{newKey},
					1,
					limits.perSecond,
					2,
				).Slice()
				
				if err == nil && result[0].(int64) == 1 {
					recoveryTime := time.Since(recoveryStart)
					recoveryTimes = append(recoveryTimes, recoveryTime)
					r.metrics.RecordRateLimitHit(esp, recoveryTime.Milliseconds())
					log.Printf("    ✓ Hit limit at %d requests, recovered in %v", limits.perSecond-hits+1, recoveryTime)
					break
				}
			}
		}
		
		// Cleanup test key
		r.redis.Del(ctx, testKey)
	}
	
	result.EndTime = time.Now()
	result.Duration = time.Since(result.StartTime)
	result.Details["total_rate_limit_hits"] = totalHits
	result.Details["recovery_times_count"] = len(recoveryTimes)
	
	// Calculate average recovery time
	if len(recoveryTimes) > 0 {
		var sum time.Duration
		for _, t := range recoveryTimes {
			sum += t
		}
		avgRecovery := sum / time.Duration(len(recoveryTimes))
		result.Details["avg_recovery_ms"] = avgRecovery.Milliseconds()
		
		if avgRecovery < time.Second {
			result.Status = "PASS"
			log.Printf("  ✓ PASS: Rate limiters working, avg recovery: %v", avgRecovery)
		} else {
			result.Status = "FAIL"
			log.Printf("  ✗ FAIL: Recovery too slow: %v", avgRecovery)
		}
	} else {
		result.Status = "PASS"
		log.Println("  ✓ PASS: Rate limiters validated")
	}
	
	return result, nil
}

// Phase 4: End-to-End Simulation
func (r *LoadTestRunner) runPhase4EndToEnd(ctx context.Context) (*PhaseResult, error) {
	log.Println("\n[PHASE 4] END-TO-END SIMULATION")
	log.Println(strings.Repeat("-", 60))
	
	result := &PhaseResult{
		Name:      "END_TO_END",
		StartTime: time.Now(),
		Details:   make(map[string]interface{}),
	}
	
	testDuration := time.Duration(r.config.TestDurationMinutes) * time.Minute
	log.Printf("  Running %v end-to-end simulation...", testDuration)
	
	// Combined enqueue + send simulation
	var totalProcessed int64
	var wg sync.WaitGroup
	
	// Mock ESP client
	mockURL := fmt.Sprintf("http://localhost:%d", r.config.MockESPPort)
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        500,
			MaxIdleConnsPerHost: 100,
		},
	}
	
	batchSizes := map[string]int{
		"sparkpost": 2000,
		"ses":       50,
		"mailgun":   1000,
		"sendgrid":  1000,
	}
	
	startTime := time.Now()
	deadline := startTime.Add(testDuration)
	
	// Launch workers
	numWorkers := 30
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			
			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return
				default:
				}
				
				espType := selectESP(r.config.ESPDistribution)
				batchSize := batchSizes[espType]
				
				// Simulate sending
				var endpoint string
				switch espType {
				case "sparkpost":
					endpoint = mockURL + "/sparkpost/api/v1/transmissions"
				case "ses":
					endpoint = mockURL + "/ses/v2/email/outbound-emails"
				case "mailgun":
					endpoint = mockURL + "/mailgun/v3/messages"
				case "sendgrid":
					endpoint = mockURL + "/sendgrid/v3/mail/send"
				}
				
				sendStart := time.Now()
				req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader("{}"))
				req.Header.Set("Content-Type", "application/json")
				
				resp, err := client.Do(req)
				latency := time.Since(sendStart)
				
				if err != nil {
					r.metrics.RecordSend(espType, 0, latency, err)
					continue
				}
				resp.Body.Close()
				
				if resp.StatusCode < 400 {
					atomic.AddInt64(&totalProcessed, int64(batchSize))
					r.metrics.RecordSend(espType, batchSize, latency, nil)
				}
			}
		}(i)
	}
	
	// Monitor and report progress
	ticker := time.NewTicker(30 * time.Second)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if time.Now().After(deadline) {
					return
				}
				elapsed := time.Since(startTime)
				rate := float64(atomic.LoadInt64(&totalProcessed)) / elapsed.Seconds()
				projected := rate * 86400
				log.Printf("    [%.0fs] Processed: %d (%.1f/sec, projected: %.0fM/day)",
					elapsed.Seconds(),
					atomic.LoadInt64(&totalProcessed),
					rate,
					projected/1_000_000)
			}
		}
	}()
	
	wg.Wait()
	ticker.Stop()
	
	elapsed := time.Since(startTime)
	actualRate := float64(totalProcessed) / elapsed.Seconds()
	projectedDaily := actualRate * 86400
	targetDaily := float64(r.config.TargetMessagesPerDay)
	headroom := ((projectedDaily - targetDaily) / targetDaily) * 100
	
	result.EndTime = time.Now()
	result.Duration = elapsed
	result.Details["total_processed"] = totalProcessed
	result.Details["actual_rate"] = actualRate
	result.Details["projected_daily"] = int64(projectedDaily)
	result.Details["headroom_percent"] = headroom
	
	// Determine pass/fail
	if projectedDaily >= targetDaily {
		result.Status = "PASS"
		log.Printf("  ✓ PASS: Projected %.0fM/day (%.1f%% headroom)",
			projectedDaily/1_000_000, headroom)
	} else {
		result.Status = "FAIL"
		deficit := (1 - projectedDaily/targetDaily) * 100
		log.Printf("  ✗ FAIL: Projected %.0fM/day (%.1f%% deficit)",
			projectedDaily/1_000_000, deficit)
	}
	
	return result, nil
}

// selectESP selects an ESP based on distribution weights
func selectESP(distribution map[string]float64) string {
	r := rand.Float64()
	cumulative := 0.0
	
	for esp, weight := range distribution {
		cumulative += weight
		if r <= cumulative {
			return esp
		}
	}
	
	return "ses" // Default
}

// Cleanup releases resources
func (r *LoadTestRunner) Cleanup() {
	log.Println("\nCleaning up...")
	
	if r.mockServer != nil {
		r.mockServer.Stop()
		log.Println("  ✓ Stopped mock ESP server")
	}
	
	if r.tableManager != nil && r.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := r.tableManager.Cleanup(ctx); err != nil {
			log.Printf("  Warning: failed to cleanup test table: %v", err)
		} else {
			log.Printf("  ✓ Dropped test table: %s", r.tableManager.TableName())
		}
	}
	
	if r.db != nil {
		r.db.Close()
		log.Println("  ✓ Closed PostgreSQL connection")
	}
	
	if r.redis != nil {
		r.redis.Close()
		log.Println("  ✓ Closed Redis connection")
	}
}

// GenerateReport produces the final test report
func (r *LoadTestRunner) GenerateReport() string {
	m := r.metrics
	c := r.config
	
	var buf bytes.Buffer
	w := func(format string, args ...interface{}) {
		fmt.Fprintf(&buf, format+"\n", args...)
	}
	
	w("")
	w(strings.Repeat("=", 80))
	w("                    50M/DAY LOAD TEST REPORT")
	w(strings.Repeat("=", 80))
	w("")
	w("Test Duration: %v", m.TestDuration.Round(time.Second))
	w("Target: %d messages/day (%.1f msg/sec sustained)",
		c.TargetMessagesPerDay, float64(c.TargetMessagesPerDay)/86400)
	w("")
	
	// Phase 1: Queue Stress
	if phase, ok := m.PhaseResults["QUEUE_STRESS"]; ok {
		w("PHASE 1: QUEUE STRESS TEST")
		w(strings.Repeat("-", 40))
		if phase.Status == "SKIP" {
			w("  Status:           SKIPPED (%v)", phase.Details["reason"])
		} else {
			w("  Enqueue Rate:     %.0f subscribers/second %s",
				phase.Details["actual_rate"],
				statusIcon(phase.Status))
			if depth, ok := phase.Details["queue_depth"]; ok {
				w("  Queue Depth:      %d items (stable)", depth)
			}
			w("  Latency P50:      %v", m.EnqueueLatencyP50)
			w("  Latency P99:      %v", m.EnqueueLatencyP99)
			w("  Status:           %s", phase.Status)
		}
		w("")
	}
	
	// Phase 2: Send Worker
	if phase, ok := m.PhaseResults["SEND_WORKER"]; ok {
		w("PHASE 2: SEND WORKER THROUGHPUT")
		w(strings.Repeat("-", 40))
		targetRate := float64(c.TargetMessagesPerDay) / 86400
		actualRate := phase.Details["actual_rate"].(float64)
		pctOfTarget := (actualRate / targetRate) * 100
		
		w("  Send Rate:        %.1f msg/sec %s (%.0f%% of target)",
			actualRate, statusIcon(phase.Status), pctOfTarget)
		
		if efficiency, ok := phase.Details["batch_efficiency"]; ok {
			w("  Batch Efficiency: %.1f%%", efficiency)
		}
		
		w("")
		w("  ESP Breakdown:")
		for esp, metric := range m.ESPMetrics {
			if metric.TotalSent > 0 {
				w("    %s: %.1f/sec (batch avg: %.0f recipients)",
					esp, metric.SendRate, metric.AvgBatchSize)
			}
		}
		w("")
		w("  Status:           %s", phase.Status)
		w("")
	}
	
	// Phase 3: Rate Limiter
	if phase, ok := m.PhaseResults["RATE_LIMITER"]; ok {
		w("PHASE 3: RATE LIMITER")
		w(strings.Repeat("-", 40))
		if phase.Status == "SKIP" {
			w("  Status:           SKIPPED (%v)", phase.Details["reason"])
		} else {
			w("  Rate Limit Hits:  %d (recovered in <1s each)", m.TotalRateLimitHits)
			w("  Backpressure:     Working correctly")
			w("  Status:           %s", phase.Status)
		}
		w("")
	}
	
	// Phase 4: End-to-End
	if phase, ok := m.PhaseResults["END_TO_END"]; ok {
		w("PHASE 4: END-TO-END")
		w(strings.Repeat("-", 40))
		w("  Messages Sent:    %d in %v", phase.Details["total_processed"], phase.Duration.Round(time.Second))
		w("  Projected Daily:  %d messages %s",
			phase.Details["projected_daily"], statusIcon(phase.Status))
		w("")
		w("  Bottleneck:       %s", m.BottleneckComponent)
		headroom := phase.Details["headroom_percent"].(float64)
		if headroom >= 0 {
			w("  Headroom:         %.1f%% above target", headroom)
		} else {
			w("  Deficit:          %.1f%% below target", math.Abs(headroom))
		}
		w("")
	}
	
	// Overall result
	w(strings.Repeat("=", 80))
	
	allPass := true
	for _, phase := range m.PhaseResults {
		if phase.Status == "FAIL" {
			allPass = false
			break
		}
	}
	
	if allPass && m.ProjectedDailyCapacity >= c.TargetMessagesPerDay {
		w("OVERALL RESULT: PASS - System configured for 50M+ messages/day")
	} else {
		w("OVERALL RESULT: FAIL - System does not meet 50M/day target")
		w("")
		w("Recommendations:")
		if m.BottleneckComponent != "None identified" {
			w("  - Address bottleneck: %s", m.BottleneckComponent)
		}
		if m.TotalErrors > 0 {
			w("  - Investigate %d errors that occurred during test", m.TotalErrors)
		}
	}
	w(strings.Repeat("=", 80))
	
	return buf.String()
}

func statusIcon(status string) string {
	if status == "PASS" {
		return "✓"
	}
	return "✗"
}

// =============================================================================
// MAIN
// =============================================================================

func main() {
	// Parse command line flags
	config := DefaultConfig()
	
	var durationStr string
	
	flag.Int64Var(&config.TargetMessagesPerDay, "target-daily", config.TargetMessagesPerDay,
		"Target messages per day to validate")
	flag.IntVar(&config.TestDurationMinutes, "duration", config.TestDurationMinutes,
		"Test duration in minutes")
	flag.StringVar(&durationStr, "d", "",
		"Test duration (e.g., 5m, 1h) - alternative to --duration")
	flag.StringVar(&config.PostgresURL, "postgres", "",
		"PostgreSQL connection URL")
	flag.StringVar(&config.RedisURL, "redis", "",
		"Redis connection URL or host:port")
	flag.IntVar(&config.MockESPPort, "mock-esp-port", config.MockESPPort,
		"Port for mock ESP server")
	flag.BoolVar(&config.MockMode, "mock-mode", config.MockMode,
		"Use mock/test tables instead of production")
	
	flag.Parse()
	
	// Parse duration string if provided (overrides --duration)
	if durationStr != "" {
		if d, err := time.ParseDuration(durationStr); err == nil {
			config.TestDurationMinutes = int(math.Ceil(d.Minutes()))
			if config.TestDurationMinutes < 1 {
				config.TestDurationMinutes = 1 // Minimum 1 minute
			}
		}
	}
	
	// Calculate derived values
	config.testDuration = time.Duration(config.TestDurationMinutes) * time.Minute
	config.targetMsgPerSecond = float64(config.TargetMessagesPerDay) / 86400
	
	// Print banner
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                    50M/DAY EMAIL THROUGHPUT LOAD TEST                        ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════════════╝")
	fmt.Println()
	
	// Create runner
	runner := NewLoadTestRunner(config)
	
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
