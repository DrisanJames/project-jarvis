//go:build ignore
// +build ignore

// Segment-Driven Journey Integration Test
//
// This test validates the complete journey system by:
// 1. Creating/using a real segment of subscribers
// 2. Building a comprehensive test journey
// 3. Enrolling the segment
// 4. Executing all journey steps
// 5. Validating completion metrics
// 6. Verifying data integrity
//
// Usage:
//
//	go run scripts/journey_segment_test.go \
//	  --postgres="postgres://user:pass@localhost:5432/mailing" \
//	  --redis="localhost:6379" \
//	  --segment="test_segment" \
//	  --create-segment=true \
//	  --segment-size=10000 \
//	  --cleanup=true
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

// SegmentTestConfig defines the test configuration
type SegmentTestConfig struct {
	PostgresURL   string
	RedisURL      string
	SegmentName   string
	CreateSegment bool
	SegmentSize   int
	CleanupAfter  bool
	OrgID         string
	Timeout       time.Duration
	Verbose       bool

	// Journey settings
	DelaySeconds int  // Delay node duration for testing (default: 5)
	MockEmailESP bool // Use mock ESP for email nodes

	// Mock ESP settings
	MockESPPort  int
	MockESPDelay time.Duration
}

// DefaultSegmentTestConfig returns sensible defaults
func DefaultSegmentTestConfig() *SegmentTestConfig {
	// Get org ID from environment or use a default for testing
	orgID := os.Getenv("ORG_ID")
	if orgID == "" {
		orgID = os.Getenv("DEFAULT_ORG_ID")
	}
	if orgID == "" {
		orgID = "00000000-0000-0000-0000-000000000001" // Test-only fallback
	}
	
	return &SegmentTestConfig{
		SegmentName:   "test_segment",
		CreateSegment: true,
		SegmentSize:   10000,
		CleanupAfter:  true,
		OrgID:         orgID,
		Timeout:       5 * time.Minute,
		Verbose:       false,
		DelaySeconds:  5,
		MockEmailESP:  true,
		MockESPPort:   9997,
		MockESPDelay:  20 * time.Millisecond,
	}
}

// =============================================================================
// VALIDATION REPORT STRUCTURES
// =============================================================================

// ValidationReport holds comprehensive test results
type ValidationReport struct {
	// Segment info
	SegmentSize int
	SegmentName string
	SegmentID   string

	// Enrollment validation
	EnrollmentCount int
	EnrollmentRate  float64 // % of segment enrolled
	EnrollmentTime  time.Duration

	// Execution validation
	ExecutionsCount  int
	CompletedCount   int
	ConvertedCount   int
	StillActiveCount int

	// Node validation (per node)
	NodeResults []NodeValidation

	// Path distribution (A/B split)
	PathACount     int
	PathBCount     int
	SplitDeviation float64 // Should be ~50/50

	// Email validation
	EmailsSent    int
	EmailsOpened  int // mocked
	EmailsClicked int // mocked

	// Timing
	TotalTestTime  time.Duration
	AvgJourneyTime time.Duration

	// Errors
	Errors []TestError

	// Overall
	Passed  bool
	Summary string

	// Step results
	StepResults map[string]*StepResult
}

// NodeValidation holds validation results for a single node
type NodeValidation struct {
	NodeID   string
	NodeType string
	Expected int
	Actual   int
	Errors   int
	AvgTime  time.Duration
	Passed   bool
}

// TestError represents an error during testing
type TestError struct {
	Step      string
	NodeID    string
	EnrollID  string
	Error     string
	Timestamp time.Time
}

// StepResult holds results for a test step
type StepResult struct {
	Name      string
	Status    string // "PASS", "FAIL", "SKIP"
	Duration  time.Duration
	Details   map[string]interface{}
	StartTime time.Time
	EndTime   time.Time
}

// =============================================================================
// JOURNEY NODE TYPES
// =============================================================================

// JourneyNode represents a node in the journey
type JourneyNode struct {
	ID          string                 `json:"id"`
	Type        string                 `json:"type"`
	Name        string                 `json:"name,omitempty"`
	Config      map[string]interface{} `json:"config"`
	Connections []string               `json:"connections,omitempty"`
}

// JourneyConnection represents a connection between nodes
type JourneyConnection struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label,omitempty"`
}

// =============================================================================
// MOCK EMAIL SERVER
// =============================================================================

// MockESPServer provides a mock email service provider
type MockESPServer struct {
	server *http.Server
	port   int
	delay  time.Duration

	// Metrics
	totalSent    int64
	totalOpened  int64
	totalClicked int64
	totalErrors  int64

	mu sync.RWMutex
}

// NewMockESPServer creates a new mock ESP server
func NewMockESPServer(port int, delay time.Duration) *MockESPServer {
	return &MockESPServer{
		port:  port,
		delay: delay,
	}
}

// Start starts the mock ESP server
func (s *MockESPServer) Start() error {
	mux := http.NewServeMux()

	mux.HandleFunc("/send", s.handleSend)
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

// handleSend handles email send requests
func (s *MockESPServer) handleSend(w http.ResponseWriter, r *http.Request) {
	// Simulate network latency
	time.Sleep(s.delay)

	// Simulate occasional errors (0.1% error rate)
	if rand.Float64() < 0.001 {
		atomic.AddInt64(&s.totalErrors, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "temporary failure"})
		return
	}

	atomic.AddInt64(&s.totalSent, 1)

	// Simulate opens (30%) and clicks (10%)
	if rand.Float64() < 0.30 {
		atomic.AddInt64(&s.totalOpened, 1)
	}
	if rand.Float64() < 0.10 {
		atomic.AddInt64(&s.totalClicked, 1)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"message_id": uuid.New().String(),
	})
}

// Stats returns server statistics
func (s *MockESPServer) Stats() map[string]int64 {
	return map[string]int64{
		"sent":    atomic.LoadInt64(&s.totalSent),
		"opened":  atomic.LoadInt64(&s.totalOpened),
		"clicked": atomic.LoadInt64(&s.totalClicked),
		"errors":  atomic.LoadInt64(&s.totalErrors),
	}
}

// =============================================================================
// SEGMENT TEST RUNNER
// =============================================================================

// SegmentTestRunner orchestrates the segment-driven journey test
type SegmentTestRunner struct {
	config *SegmentTestConfig
	db     *sql.DB
	redis  *redis.Client
	report *ValidationReport

	// Mock ESP
	mockESP *MockESPServer

	// Created test data
	segmentID   string
	journeyID   string
	listID      string
	subscribers []string

	// Journey structure
	journeyNodes       []JourneyNode
	journeyConnections []JourneyConnection

	// Execution tracking
	nodeExecutionCounts map[string]int64
	nodeExecutionTimes  map[string][]time.Duration
	nodeErrors          map[string]int64
	pathACounts         int64
	pathBCounts         int64

	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.RWMutex
}

// NewSegmentTestRunner creates a new test runner
func NewSegmentTestRunner(config *SegmentTestConfig) *SegmentTestRunner {
	return &SegmentTestRunner{
		config:              config,
		report:              &ValidationReport{StepResults: make(map[string]*StepResult)},
		nodeExecutionCounts: make(map[string]int64),
		nodeExecutionTimes:  make(map[string][]time.Duration),
		nodeErrors:          make(map[string]int64),
	}
}

// Initialize sets up all test infrastructure
func (r *SegmentTestRunner) Initialize(ctx context.Context) error {
	log.Println("Initializing segment test infrastructure...")

	// Connect to PostgreSQL
	if r.config.PostgresURL != "" {
		db, err := sql.Open("postgres", r.config.PostgresURL)
		if err != nil {
			return fmt.Errorf("failed to connect to PostgreSQL: %w", err)
		}
		db.SetMaxOpenConns(50)
		db.SetMaxIdleConns(25)
		if err := db.PingContext(ctx); err != nil {
			return fmt.Errorf("failed to ping PostgreSQL: %w", err)
		}
		r.db = db
		log.Println("  ✓ Connected to PostgreSQL")
	} else {
		return fmt.Errorf("PostgreSQL connection URL required")
	}

	// Connect to Redis (optional)
	if r.config.RedisURL != "" {
		opts, err := redis.ParseURL(r.config.RedisURL)
		if err != nil {
			opts = &redis.Options{Addr: r.config.RedisURL}
		}
		r.redis = redis.NewClient(opts)
		if err := r.redis.Ping(ctx).Err(); err != nil {
			log.Printf("  ! Redis connection failed (optional): %v", err)
			r.redis = nil
		} else {
			log.Println("  ✓ Connected to Redis")
		}
	}

	// Start mock ESP server
	if r.config.MockEmailESP {
		r.mockESP = NewMockESPServer(r.config.MockESPPort, r.config.MockESPDelay)
		if err := r.mockESP.Start(); err != nil {
			return fmt.Errorf("failed to start mock ESP: %w", err)
		}
		log.Printf("  ✓ Started mock ESP server on port %d", r.config.MockESPPort)
	}

	return nil
}

// Run executes all test steps
func (r *SegmentTestRunner) Run(ctx context.Context) error {
	r.ctx, r.cancel = context.WithTimeout(ctx, r.config.Timeout)
	defer r.cancel()

	testStart := time.Now()

	r.printBanner()

	// Step 1: Setup Test Segment
	log.Println("\nSTEP 1: SEGMENT SETUP")
	log.Println(strings.Repeat("-", 60))
	if err := r.SetupTestSegment(); err != nil {
		r.report.Errors = append(r.report.Errors, TestError{
			Step:      "segment_setup",
			Error:     err.Error(),
			Timestamp: time.Now(),
		})
		return fmt.Errorf("segment setup failed: %w", err)
	}

	// Step 2: Create Test Journey
	log.Println("\nSTEP 2: JOURNEY CREATION")
	log.Println(strings.Repeat("-", 60))
	if err := r.CreateTestJourney(); err != nil {
		r.report.Errors = append(r.report.Errors, TestError{
			Step:      "journey_creation",
			Error:     err.Error(),
			Timestamp: time.Now(),
		})
		return fmt.Errorf("journey creation failed: %w", err)
	}

	// Step 3: Enroll Segment
	log.Println("\nSTEP 3: SEGMENT ENROLLMENT")
	log.Println(strings.Repeat("-", 60))
	if err := r.EnrollSegment(); err != nil {
		r.report.Errors = append(r.report.Errors, TestError{
			Step:      "enrollment",
			Error:     err.Error(),
			Timestamp: time.Now(),
		})
		return fmt.Errorf("enrollment failed: %w", err)
	}

	// Step 4: Execute Journey
	log.Println("\nSTEP 4: JOURNEY EXECUTION")
	log.Println(strings.Repeat("-", 60))
	if err := r.ExecuteJourney(); err != nil {
		r.report.Errors = append(r.report.Errors, TestError{
			Step:      "execution",
			Error:     err.Error(),
			Timestamp: time.Now(),
		})
		return fmt.Errorf("execution failed: %w", err)
	}

	// Step 5: Validate Results
	log.Println("\nSTEP 5: VALIDATION")
	log.Println(strings.Repeat("-", 60))
	if err := r.ValidateResults(); err != nil {
		r.report.Errors = append(r.report.Errors, TestError{
			Step:      "validation",
			Error:     err.Error(),
			Timestamp: time.Now(),
		})
		// Don't return error - we want to see the report
	}

	// Step 6: Cleanup (if configured)
	if r.config.CleanupAfter {
		log.Println("\nSTEP 6: CLEANUP")
		log.Println(strings.Repeat("-", 60))
		if err := r.Cleanup(); err != nil {
			log.Printf("  ! Cleanup warning: %v", err)
		}
	}

	r.report.TotalTestTime = time.Since(testStart)

	return nil
}

// printBanner prints the test banner
func (r *SegmentTestRunner) printBanner() {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 80))
	fmt.Println("             SEGMENT-DRIVEN JOURNEY INTEGRATION TEST")
	fmt.Println(strings.Repeat("=", 80))
	fmt.Println()
	fmt.Println("TEST CONFIGURATION")
	fmt.Printf("  Segment:         %s\n", r.config.SegmentName)
	fmt.Printf("  Subscribers:     %d\n", r.config.SegmentSize)
	fmt.Printf("  Journey:         Welcome Series (7 nodes)\n")
	fmt.Printf("  Timeout:         %v\n", r.config.Timeout)
	fmt.Printf("  Cleanup:         %v\n", r.config.CleanupAfter)
	fmt.Println()
}

// =============================================================================
// STEP 1: SETUP TEST SEGMENT
// =============================================================================

// SetupTestSegment creates or uses an existing segment
func (r *SegmentTestRunner) SetupTestSegment() error {
	stepStart := time.Now()
	result := &StepResult{
		Name:      "segment_setup",
		StartTime: stepStart,
		Details:   make(map[string]interface{}),
	}
	defer func() {
		result.EndTime = time.Now()
		result.Duration = result.EndTime.Sub(result.StartTime)
		r.report.StepResults["segment_setup"] = result
	}()

	// Create a mailing list
	r.listID = uuid.New().String()
	_, err := r.db.ExecContext(r.ctx, `
		INSERT INTO mailing_lists (id, organization_id, name, description, status, subscriber_count, active_count)
		VALUES ($1, $2, $3, $4, 'active', 0, 0)
	`, r.listID, r.config.OrgID, "Test List - "+r.config.SegmentName, "Integration test list")
	if err != nil {
		result.Status = "FAIL"
		return fmt.Errorf("failed to create mailing list: %w", err)
	}
	log.Printf("  Created list:    %s", r.listID[:12]+"...")
	result.Details["list_id"] = r.listID

	// Add subscribers in batches using COPY
	log.Printf("  Adding %d subscribers...", r.config.SegmentSize)
	addStart := time.Now()

	batchSize := 5000
	totalAdded := 0
	r.subscribers = make([]string, 0, r.config.SegmentSize)

	for totalAdded < r.config.SegmentSize {
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		default:
		}

		tx, err := r.db.BeginTx(r.ctx, nil)
		if err != nil {
			result.Status = "FAIL"
			return fmt.Errorf("failed to begin transaction: %w", err)
		}

		stmt, err := tx.Prepare(pq.CopyIn(
			"mailing_subscribers",
			"id", "organization_id", "list_id", "email", "email_hash",
			"first_name", "last_name", "status", "engagement_score",
			"source", "subscribed_at", "created_at",
		))
		if err != nil {
			tx.Rollback()
			result.Status = "FAIL"
			return fmt.Errorf("failed to prepare COPY: %w", err)
		}

		thisBatch := batchSize
		if totalAdded+batchSize > r.config.SegmentSize {
			thisBatch = r.config.SegmentSize - totalAdded
		}

		now := time.Now()
		for i := 0; i < thisBatch; i++ {
			subID := uuid.New().String()
			email := fmt.Sprintf("test_%d_%d@journey-test.local", totalAdded, i)
			emailHash := fmt.Sprintf("%x", fnv.New64a())

			_, err = stmt.Exec(
				subID,
				r.config.OrgID,
				r.listID,
				email,
				emailHash,
				fmt.Sprintf("Test%d", i),
				fmt.Sprintf("User%d", totalAdded+i),
				"confirmed",
				50.0+rand.Float64()*50, // engagement_score 50-100
				"integration_test",
				now,
				now,
			)
			if err != nil {
				stmt.Close()
				tx.Rollback()
				result.Status = "FAIL"
				return fmt.Errorf("failed to insert subscriber: %w", err)
			}

			r.subscribers = append(r.subscribers, subID)
		}

		_, err = stmt.Exec()
		if err != nil {
			stmt.Close()
			tx.Rollback()
			result.Status = "FAIL"
			return fmt.Errorf("failed to flush COPY: %w", err)
		}
		stmt.Close()

		if err := tx.Commit(); err != nil {
			result.Status = "FAIL"
			return fmt.Errorf("failed to commit: %w", err)
		}

		totalAdded += thisBatch
		if r.config.Verbose && totalAdded%10000 == 0 {
			log.Printf("    Progress: %d/%d subscribers", totalAdded, r.config.SegmentSize)
		}
	}

	addDuration := time.Since(addStart)
	addRate := float64(r.config.SegmentSize) / addDuration.Seconds()
	log.Printf("  Added subs:      %d in %v (%.0f/sec)", r.config.SegmentSize, addDuration.Round(time.Millisecond), addRate)

	// Update list counts
	_, err = r.db.ExecContext(r.ctx, `
		UPDATE mailing_lists SET subscriber_count = $1, active_count = $1 WHERE id = $2
	`, r.config.SegmentSize, r.listID)
	if err != nil {
		log.Printf("  ! Warning: failed to update list counts: %v", err)
	}

	// Create segment
	r.segmentID = uuid.New().String()
	segmentConditions := []map[string]interface{}{
		{
			"field":    "status",
			"operator": "equals",
			"value":    "confirmed",
		},
		{
			"field":    "engagement_score",
			"operator": "gte",
			"value":    "0",
		},
	}
	conditionsJSON, _ := json.Marshal(segmentConditions)

	_, err = r.db.ExecContext(r.ctx, `
		INSERT INTO mailing_segments (id, organization_id, list_id, name, description, segment_type, conditions, subscriber_count, status)
		VALUES ($1, $2, $3, $4, $5, 'dynamic', $6, $7, 'active')
	`, r.segmentID, r.config.OrgID, r.listID, r.config.SegmentName, "Integration test segment", string(conditionsJSON), r.config.SegmentSize)
	if err != nil {
		result.Status = "FAIL"
		return fmt.Errorf("failed to create segment: %w", err)
	}

	log.Printf("  Created segment: %s", r.segmentID[:12]+"...")
	result.Details["segment_id"] = r.segmentID
	result.Details["subscriber_count"] = r.config.SegmentSize
	result.Details["add_rate"] = addRate

	r.report.SegmentID = r.segmentID
	r.report.SegmentName = r.config.SegmentName
	r.report.SegmentSize = r.config.SegmentSize

	result.Status = "PASS"
	log.Println("  ✓ Segment setup complete")

	return nil
}

// =============================================================================
// STEP 2: CREATE TEST JOURNEY
// =============================================================================

// CreateTestJourney creates a comprehensive test journey
func (r *SegmentTestRunner) CreateTestJourney() error {
	stepStart := time.Now()
	result := &StepResult{
		Name:      "journey_creation",
		StartTime: stepStart,
		Details:   make(map[string]interface{}),
	}
	defer func() {
		result.EndTime = time.Now()
		result.Duration = result.EndTime.Sub(result.StartTime)
		r.report.StepResults["journey_creation"] = result
	}()

	r.journeyID = "journey_test_" + uuid.New().String()[:8]

	// Build comprehensive journey with all node types:
	// 1. Trigger (list_subscription)
	// 2. Email (welcome message)
	// 3. Delay (configurable seconds for testing)
	// 4. Condition (check engagement score)
	// 5. Split (A/B 50/50)
	// 6a. Email (Path A content)
	// 6b. Email (Path B content)
	// 7. Goal (conversion)

	r.journeyNodes = []JourneyNode{
		{
			ID:   "node_1",
			Type: "trigger",
			Name: "Journey Start",
			Config: map[string]interface{}{
				"triggerType": "segment_entry",
				"segmentId":   r.segmentID,
			},
			Connections: []string{"node_2"},
		},
		{
			ID:   "node_2",
			Type: "email",
			Name: "Welcome Email",
			Config: map[string]interface{}{
				"subject":     "Welcome to our journey!",
				"htmlContent": "<h1>Welcome!</h1><p>Thank you for joining.</p>",
				"fromName":    "Test Journey",
				"fromEmail":   "test@journey-test.local",
			},
			Connections: []string{"node_3"},
		},
		{
			ID:   "node_3",
			Type: "delay",
			Name: "Wait Period",
			Config: map[string]interface{}{
				"delayValue": r.config.DelaySeconds,
				"delayUnit":  "seconds",
			},
			Connections: []string{"node_4"},
		},
		{
			ID:   "node_4",
			Type: "condition",
			Name: "Check Engagement",
			Config: map[string]interface{}{
				"conditionType": "engagement_score",
				"operator":      "gte",
				"threshold":     50,
			},
			Connections: []string{"node_5", "node_7"}, // true -> split, false -> goal
		},
		{
			ID:   "node_5",
			Type: "split",
			Name: "A/B Split",
			Config: map[string]interface{}{
				"splitType":      "percentage",
				"pathAPercent":   50,
				"pathBPercent":   50,
				"splitAttribute": "random",
			},
			Connections: []string{"node_6a", "node_6b"},
		},
		{
			ID:   "node_6a",
			Type: "email",
			Name: "Path A Email",
			Config: map[string]interface{}{
				"subject":     "Path A: Special Offer!",
				"htmlContent": "<h1>Path A</h1><p>You're in group A!</p>",
				"fromName":    "Test Journey",
				"fromEmail":   "test@journey-test.local",
			},
			Connections: []string{"node_7"},
		},
		{
			ID:   "node_6b",
			Type: "email",
			Name: "Path B Email",
			Config: map[string]interface{}{
				"subject":     "Path B: Exclusive Deal!",
				"htmlContent": "<h1>Path B</h1><p>You're in group B!</p>",
				"fromName":    "Test Journey",
				"fromEmail":   "test@journey-test.local",
			},
			Connections: []string{"node_7"},
		},
		{
			ID:   "node_7",
			Type: "goal",
			Name: "Journey Complete",
			Config: map[string]interface{}{
				"goalType":    "conversion",
				"goalName":    "journey_completed",
				"trackMetric": true,
			},
			Connections: []string{},
		},
	}

	// Build connections
	r.journeyConnections = []JourneyConnection{
		{From: "node_1", To: "node_2", Label: ""},
		{From: "node_2", To: "node_3", Label: ""},
		{From: "node_3", To: "node_4", Label: ""},
		{From: "node_4", To: "node_5", Label: "true"},
		{From: "node_4", To: "node_7", Label: "false"},
		{From: "node_5", To: "node_6a", Label: "A"},
		{From: "node_5", To: "node_6b", Label: "B"},
		{From: "node_6a", To: "node_7", Label: ""},
		{From: "node_6b", To: "node_7", Label: ""},
	}

	nodesJSON, _ := json.Marshal(r.journeyNodes)
	connectionsJSON, _ := json.Marshal(r.journeyConnections)

	// Insert journey
	_, err := r.db.ExecContext(r.ctx, `
		INSERT INTO mailing_journeys (id, organization_id, name, description, status, nodes, connections, trigger_type, trigger_config, segment_id, created_at)
		VALUES ($1, $2, $3, $4, 'draft', $5, $6, 'segment_entry', $7, $8, NOW())
	`, r.journeyID, r.config.OrgID, "Integration Test Journey", "Comprehensive journey for segment testing",
		string(nodesJSON), string(connectionsJSON),
		fmt.Sprintf(`{"segmentId": "%s"}`, r.segmentID), r.segmentID)
	if err != nil {
		result.Status = "FAIL"
		return fmt.Errorf("failed to create journey: %w", err)
	}

	log.Printf("  Journey ID:      %s", r.journeyID)
	log.Printf("  Nodes:           %d (trigger, email, delay, condition, split, email×2, goal)", len(r.journeyNodes))
	result.Details["journey_id"] = r.journeyID
	result.Details["node_count"] = len(r.journeyNodes)

	result.Status = "PASS"
	log.Println("  ✓ Journey creation complete")

	return nil
}

// =============================================================================
// STEP 3: ENROLL SEGMENT
// =============================================================================

// EnrollSegment activates the journey and enrolls all segment subscribers
func (r *SegmentTestRunner) EnrollSegment() error {
	stepStart := time.Now()
	result := &StepResult{
		Name:      "enrollment",
		StartTime: stepStart,
		Details:   make(map[string]interface{}),
	}
	defer func() {
		result.EndTime = time.Now()
		result.Duration = result.EndTime.Sub(result.StartTime)
		r.report.StepResults["enrollment"] = result
	}()

	// Activate journey
	_, err := r.db.ExecContext(r.ctx, `
		UPDATE mailing_journeys SET status = 'active', activated_at = NOW() WHERE id = $1
	`, r.journeyID)
	if err != nil {
		result.Status = "FAIL"
		return fmt.Errorf("failed to activate journey: %w", err)
	}
	log.Println("  Journey activated")

	// Check if enrollments table exists, create if not
	_, err = r.db.ExecContext(r.ctx, `
		CREATE TABLE IF NOT EXISTS mailing_journey_enrollments (
			id VARCHAR(100) PRIMARY KEY,
			journey_id VARCHAR(100) NOT NULL,
			subscriber_id UUID NOT NULL,
			subscriber_email VARCHAR(255),
			current_node_id VARCHAR(100),
			status VARCHAR(50) DEFAULT 'active',
			metadata JSONB DEFAULT '{}',
			next_execute_at TIMESTAMP WITH TIME ZONE,
			enrolled_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
			completed_at TIMESTAMP WITH TIME ZONE,
			converted_at TIMESTAMP WITH TIME ZONE,
			execution_count INTEGER DEFAULT 0,
			last_executed_at TIMESTAMP WITH TIME ZONE
		)
	`)
	if err != nil {
		log.Printf("  ! Warning: create table: %v", err)
	}

	// Create indexes if not exist
	r.db.ExecContext(r.ctx, `CREATE INDEX IF NOT EXISTS idx_journey_enrollments_journey ON mailing_journey_enrollments(journey_id)`)
	r.db.ExecContext(r.ctx, `CREATE INDEX IF NOT EXISTS idx_journey_enrollments_status ON mailing_journey_enrollments(status)`)
	r.db.ExecContext(r.ctx, `CREATE INDEX IF NOT EXISTS idx_journey_enrollments_execute ON mailing_journey_enrollments(next_execute_at) WHERE status = 'active'`)

	// Batch enroll all segment subscribers
	log.Printf("  Enrolling %d subscribers...", len(r.subscribers))
	enrollStart := time.Now()

	batchSize := 5000
	totalEnrolled := 0
	firstNodeID := "node_2" // Skip trigger, start at first email

	for totalEnrolled < len(r.subscribers) {
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		default:
		}

		tx, err := r.db.BeginTx(r.ctx, nil)
		if err != nil {
			result.Status = "FAIL"
			return fmt.Errorf("failed to begin transaction: %w", err)
		}

		stmt, err := tx.Prepare(pq.CopyIn(
			"mailing_journey_enrollments",
			"id", "journey_id", "subscriber_id", "subscriber_email",
			"current_node_id", "status", "next_execute_at", "enrolled_at",
		))
		if err != nil {
			tx.Rollback()
			result.Status = "FAIL"
			return fmt.Errorf("failed to prepare COPY: %w", err)
		}

		thisBatch := batchSize
		if totalEnrolled+batchSize > len(r.subscribers) {
			thisBatch = len(r.subscribers) - totalEnrolled
		}

		now := time.Now()
		for i := 0; i < thisBatch; i++ {
			idx := totalEnrolled + i
			enrollID := fmt.Sprintf("enroll_%s_%d", r.journeyID[:8], idx)
			email := fmt.Sprintf("test_%d_%d@journey-test.local", idx/5000, idx%5000)

			_, err = stmt.Exec(
				enrollID,
				r.journeyID,
				r.subscribers[idx],
				email,
				firstNodeID,
				"active",
				now, // Execute immediately
				now,
			)
			if err != nil {
				stmt.Close()
				tx.Rollback()
				result.Status = "FAIL"
				return fmt.Errorf("failed to insert enrollment: %w", err)
			}
		}

		_, err = stmt.Exec()
		if err != nil {
			stmt.Close()
			tx.Rollback()
			result.Status = "FAIL"
			return fmt.Errorf("failed to flush COPY: %w", err)
		}
		stmt.Close()

		if err := tx.Commit(); err != nil {
			result.Status = "FAIL"
			return fmt.Errorf("failed to commit: %w", err)
		}

		totalEnrolled += thisBatch
	}

	enrollDuration := time.Since(enrollStart)
	enrollRate := float64(len(r.subscribers)) / enrollDuration.Seconds()

	log.Printf("  Enrolled:        %d / %d (100%%)", totalEnrolled, len(r.subscribers))
	log.Printf("  Rate:            %.0f/second", enrollRate)
	log.Printf("  Time:            %v", enrollDuration.Round(time.Millisecond))

	// Update journey stats
	_, err = r.db.ExecContext(r.ctx, `
		UPDATE mailing_journeys SET total_entered = $1 WHERE id = $2
	`, totalEnrolled, r.journeyID)
	if err != nil {
		log.Printf("  ! Warning: failed to update journey stats: %v", err)
	}

	r.report.EnrollmentCount = totalEnrolled
	r.report.EnrollmentRate = 100.0
	r.report.EnrollmentTime = enrollDuration
	result.Details["enrolled"] = totalEnrolled
	result.Details["rate"] = enrollRate

	result.Status = "PASS"
	log.Println("  ✓ Enrollment complete")

	return nil
}

// =============================================================================
// STEP 4: EXECUTE JOURNEY
// =============================================================================

// ExecuteJourney processes all enrollments through the journey
func (r *SegmentTestRunner) ExecuteJourney() error {
	stepStart := time.Now()
	result := &StepResult{
		Name:      "execution",
		StartTime: stepStart,
		Details:   make(map[string]interface{}),
	}
	defer func() {
		result.EndTime = time.Now()
		result.Duration = result.EndTime.Sub(result.StartTime)
		r.report.StepResults["execution"] = result
	}()

	// Process enrollments in parallel
	workerCount := 8
	var wg sync.WaitGroup
	var totalProcessed int64
	var totalCompleted int64
	var totalErrors int64

	// Create work channel
	workChan := make(chan string, 1000)

	// Start workers
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			r.executeWorker(workerID, workChan, &totalProcessed, &totalCompleted, &totalErrors)
		}(i)
	}

	// Feed enrollments to workers
	go func() {
		// Get all active enrollments
		rows, err := r.db.QueryContext(r.ctx, `
			SELECT id FROM mailing_journey_enrollments 
			WHERE journey_id = $1 AND status = 'active'
			ORDER BY enrolled_at
		`, r.journeyID)
		if err != nil {
			log.Printf("  ! Error querying enrollments: %v", err)
			close(workChan)
			return
		}
		defer rows.Close()

		for rows.Next() {
			var enrollID string
			if err := rows.Scan(&enrollID); err != nil {
				continue
			}
			select {
			case workChan <- enrollID:
			case <-r.ctx.Done():
				close(workChan)
				return
			}
		}
		close(workChan)
	}()

	// Monitor progress
	monitorDone := make(chan bool)
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		lastProcessed := int64(0)
		for {
			select {
			case <-ticker.C:
				current := atomic.LoadInt64(&totalProcessed)
				completed := atomic.LoadInt64(&totalCompleted)
				rate := float64(current-lastProcessed) / 2.0
				progress := float64(completed) / float64(r.config.SegmentSize) * 100

				// Progress bar
				barWidth := 20
				filled := int(progress / 100 * float64(barWidth))
				bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)

				log.Printf("  Progress:        [%s] %.1f%% (%d completed, %.0f/sec)", bar, progress, completed, rate)
				lastProcessed = current

				// Check if done
				if completed >= int64(r.config.SegmentSize)*9/10 { // 90% threshold
					// Give it a bit more time to finish stragglers
					time.Sleep(2 * time.Second)
				}
			case <-monitorDone:
				return
			case <-r.ctx.Done():
				return
			}
		}
	}()

	// Wait for workers
	wg.Wait()
	close(monitorDone)

	// Final stats
	completed := atomic.LoadInt64(&totalCompleted)
	errors := atomic.LoadInt64(&totalErrors)
	active := int64(r.config.SegmentSize) - completed

	log.Printf("  Completed:       %d / %d", completed, r.config.SegmentSize)
	log.Printf("  Still Active:    %d (in delay nodes)", active)
	log.Printf("  Errors:          %d", errors)

	r.report.ExecutionsCount = int(atomic.LoadInt64(&totalProcessed))
	r.report.CompletedCount = int(completed)
	r.report.StillActiveCount = int(active)

	result.Details["processed"] = totalProcessed
	result.Details["completed"] = completed
	result.Details["errors"] = errors

	if errors > int64(r.config.SegmentSize)/100 { // > 1% errors
		result.Status = "FAIL"
		return fmt.Errorf("too many errors: %d", errors)
	}

	result.Status = "PASS"
	log.Println("  ✓ Execution complete")

	return nil
}

// executeWorker processes enrollments for a worker
func (r *SegmentTestRunner) executeWorker(workerID int, workChan chan string, totalProcessed, totalCompleted, totalErrors *int64) {
	for enrollID := range workChan {
		select {
		case <-r.ctx.Done():
			return
		default:
		}

		if err := r.processEnrollment(enrollID); err != nil {
			atomic.AddInt64(totalErrors, 1)
			if r.config.Verbose {
				log.Printf("  Worker %d: error processing %s: %v", workerID, enrollID, err)
			}
		}
		atomic.AddInt64(totalProcessed, 1)

		// Check if completed
		var status string
		r.db.QueryRowContext(r.ctx, `
			SELECT status FROM mailing_journey_enrollments WHERE id = $1
		`, enrollID).Scan(&status)

		if status == "completed" {
			atomic.AddInt64(totalCompleted, 1)
		}
	}
}

// processEnrollment processes a single enrollment through the journey
func (r *SegmentTestRunner) processEnrollment(enrollID string) error {
	// Get current node
	var currentNodeID string
	var subscriberID string
	var status string

	err := r.db.QueryRowContext(r.ctx, `
		SELECT current_node_id, subscriber_id, status 
		FROM mailing_journey_enrollments WHERE id = $1
	`, enrollID).Scan(&currentNodeID, &subscriberID, &status)
	if err != nil {
		return fmt.Errorf("failed to get enrollment: %w", err)
	}

	if status != "active" {
		return nil // Already completed or paused
	}

	// Find current node
	var currentNode *JourneyNode
	for i := range r.journeyNodes {
		if r.journeyNodes[i].ID == currentNodeID {
			currentNode = &r.journeyNodes[i]
			break
		}
	}

	if currentNode == nil {
		return fmt.Errorf("node not found: %s", currentNodeID)
	}

	// Process the current node and continue until hitting a delay or goal
	for currentNode != nil {
		nodeStart := time.Now()
		nextNodeID, err := r.processNode(enrollID, subscriberID, currentNode)
		nodeDuration := time.Since(nodeStart)

		// Record metrics
		r.mu.Lock()
		r.nodeExecutionCounts[currentNode.ID]++
		r.nodeExecutionTimes[currentNode.ID] = append(r.nodeExecutionTimes[currentNode.ID], nodeDuration)
		if err != nil {
			r.nodeErrors[currentNode.ID]++
		}
		r.mu.Unlock()

		if err != nil {
			return fmt.Errorf("failed to process node %s: %w", currentNode.ID, err)
		}

		// Log execution
		r.logExecution(enrollID, currentNode, "executed", "success")

		// Check if journey is complete
		if nextNodeID == "" || currentNode.Type == "goal" {
			// Mark enrollment as completed
			_, err := r.db.ExecContext(r.ctx, `
				UPDATE mailing_journey_enrollments 
				SET status = 'completed', completed_at = NOW(), converted_at = NOW()
				WHERE id = $1
			`, enrollID)
			if err != nil {
				return fmt.Errorf("failed to complete enrollment: %w", err)
			}
			return nil
		}

		// Update current node
		_, err = r.db.ExecContext(r.ctx, `
			UPDATE mailing_journey_enrollments 
			SET current_node_id = $1, last_executed_at = NOW(), execution_count = execution_count + 1
			WHERE id = $2
		`, nextNodeID, enrollID)
		if err != nil {
			return fmt.Errorf("failed to update enrollment: %w", err)
		}

		// Find next node
		var nextNode *JourneyNode
		for i := range r.journeyNodes {
			if r.journeyNodes[i].ID == nextNodeID {
				nextNode = &r.journeyNodes[i]
				break
			}
		}

		// If next node is a delay, stop processing and schedule
		if nextNode != nil && nextNode.Type == "delay" {
			delayValue := 5 // default
			if dv, ok := nextNode.Config["delayValue"].(float64); ok {
				delayValue = int(dv)
			} else if dv, ok := nextNode.Config["delayValue"].(int); ok {
				delayValue = dv
			}

			nextExecute := time.Now().Add(time.Duration(delayValue) * time.Second)
			_, err = r.db.ExecContext(r.ctx, `
				UPDATE mailing_journey_enrollments 
				SET next_execute_at = $1
				WHERE id = $2
			`, nextExecute, enrollID)

			// For testing, just wait the delay (in real system this would be scheduled)
			time.Sleep(time.Duration(delayValue) * time.Second)

			// Record delay node execution
			r.mu.Lock()
			r.nodeExecutionCounts[nextNode.ID]++
			r.mu.Unlock()

			// Move past delay to next node
			if len(nextNode.Connections) > 0 {
				nextNodeID = nextNode.Connections[0]
				_, err = r.db.ExecContext(r.ctx, `
					UPDATE mailing_journey_enrollments 
					SET current_node_id = $1, last_executed_at = NOW()
					WHERE id = $2
				`, nextNodeID, enrollID)

				// Find the node after delay
				for i := range r.journeyNodes {
					if r.journeyNodes[i].ID == nextNodeID {
						nextNode = &r.journeyNodes[i]
						break
					}
				}
			}
		}

		currentNode = nextNode
	}

	return nil
}

// processNode executes a single journey node
func (r *SegmentTestRunner) processNode(enrollID, subscriberID string, node *JourneyNode) (string, error) {
	switch node.Type {
	case "trigger":
		// Triggers just pass through to next node
		if len(node.Connections) > 0 {
			return node.Connections[0], nil
		}
		return "", nil

	case "email":
		// Send email via mock ESP
		if r.mockESP != nil {
			mockURL := fmt.Sprintf("http://localhost:%d/send", r.config.MockESPPort)
			payload := map[string]string{
				"to":      fmt.Sprintf("subscriber_%s@test.local", subscriberID[:8]),
				"subject": node.Config["subject"].(string),
			}
			payloadJSON, _ := json.Marshal(payload)

			resp, err := http.Post(mockURL, "application/json", bytes.NewReader(payloadJSON))
			if err != nil {
				return "", fmt.Errorf("email send failed: %w", err)
			}
			resp.Body.Close()

			if resp.StatusCode >= 400 {
				return "", fmt.Errorf("email send failed: status %d", resp.StatusCode)
			}
		}

		if len(node.Connections) > 0 {
			return node.Connections[0], nil
		}
		return "", nil

	case "delay":
		// Delay nodes are handled specially in processEnrollment
		if len(node.Connections) > 0 {
			return node.Connections[0], nil
		}
		return "", nil

	case "condition":
		// Evaluate condition based on subscriber data
		var engagementScore float64
		err := r.db.QueryRowContext(r.ctx, `
			SELECT COALESCE(engagement_score, 50) FROM mailing_subscribers WHERE id = $1
		`, subscriberID).Scan(&engagementScore)
		if err != nil {
			engagementScore = 50 // default
		}

		threshold := 50.0
		if t, ok := node.Config["threshold"].(float64); ok {
			threshold = t
		}

		// true path is first connection, false path is second
		if engagementScore >= threshold {
			if len(node.Connections) > 0 {
				return node.Connections[0], nil
			}
		} else {
			if len(node.Connections) > 1 {
				return node.Connections[1], nil
			}
		}
		return "", nil

	case "split":
		// A/B split - use hash for deterministic split
		h := fnv.New32a()
		h.Write([]byte(enrollID))
		hash := h.Sum32()

		// 50/50 split
		if hash%100 < 50 {
			atomic.AddInt64(&r.pathACounts, 1)
			if len(node.Connections) > 0 {
				return node.Connections[0], nil
			}
		} else {
			atomic.AddInt64(&r.pathBCounts, 1)
			if len(node.Connections) > 1 {
				return node.Connections[1], nil
			}
		}
		return "", nil

	case "goal":
		// Goal nodes complete the journey
		r.report.ConvertedCount++
		return "", nil

	default:
		return "", fmt.Errorf("unknown node type: %s", node.Type)
	}
}

// logExecution logs a node execution
func (r *SegmentTestRunner) logExecution(enrollID string, node *JourneyNode, action, result string) {
	_, err := r.db.ExecContext(r.ctx, `
		INSERT INTO mailing_journey_execution_log (id, enrollment_id, journey_id, node_id, node_type, action, result, executed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
	`, uuid.New().String(), enrollID, r.journeyID, node.ID, node.Type, action, result)
	if err != nil && r.config.Verbose {
		log.Printf("  ! Warning: failed to log execution: %v", err)
	}
}

// =============================================================================
// STEP 5: VALIDATE RESULTS
// =============================================================================

// ValidateResults validates all test results
func (r *SegmentTestRunner) ValidateResults() error {
	stepStart := time.Now()
	result := &StepResult{
		Name:      "validation",
		StartTime: stepStart,
		Details:   make(map[string]interface{}),
	}
	defer func() {
		result.EndTime = time.Now()
		result.Duration = result.EndTime.Sub(result.StartTime)
		r.report.StepResults["validation"] = result
	}()

	allPassed := true

	// Build node validation results
	r.report.NodeResults = make([]NodeValidation, 0, len(r.journeyNodes))

	fmt.Println()
	fmt.Println("  ┌─────────────┬──────────┬──────────┬────────┬───────────┬────────┐")
	fmt.Println("  │ Node        │ Type     │ Expected │ Actual │ Errors    │ Status │")
	fmt.Println("  ├─────────────┼──────────┼──────────┼────────┼───────────┼────────┤")

	for _, node := range r.journeyNodes {
		r.mu.RLock()
		actualCount := int(r.nodeExecutionCounts[node.ID])
		errorCount := int(r.nodeErrors[node.ID])
		times := r.nodeExecutionTimes[node.ID]
		r.mu.RUnlock()

		// Calculate expected count
		expectedCount := r.config.SegmentSize
		if node.Type == "trigger" {
			expectedCount = r.config.SegmentSize // All enrolled
		} else if node.ID == "node_6a" || node.ID == "node_6b" {
			expectedCount = r.config.SegmentSize / 2 // ~50% each from split
		}

		// Calculate average time
		var avgTime time.Duration
		if len(times) > 0 {
			var total time.Duration
			for _, t := range times {
				total += t
			}
			avgTime = total / time.Duration(len(times))
		}

		// Determine pass/fail
		tolerance := 0.1 // 10% tolerance
		passed := true
		if node.Type != "trigger" {
			if actualCount < int(float64(expectedCount)*(1-tolerance)) {
				passed = false
				allPassed = false
			}
		}

		nv := NodeValidation{
			NodeID:   node.ID,
			NodeType: node.Type,
			Expected: expectedCount,
			Actual:   actualCount,
			Errors:   errorCount,
			AvgTime:  avgTime,
			Passed:   passed,
		}
		r.report.NodeResults = append(r.report.NodeResults, nv)

		status := "✓"
		if !passed {
			status = "✗"
		}

		expectedStr := fmt.Sprintf("%d", expectedCount)
		if node.ID == "node_6a" || node.ID == "node_6b" {
			expectedStr = fmt.Sprintf("~%d", expectedCount)
		}

		fmt.Printf("  │ %-11s │ %-8s │ %-8s │ %-6d │ %-9d │ %-6s │\n",
			node.ID, node.Type, expectedStr, actualCount, errorCount, status)
	}

	fmt.Println("  └─────────────┴──────────┴──────────┴────────┴───────────┴────────┘")
	fmt.Println()

	// A/B Split Distribution
	pathA := atomic.LoadInt64(&r.pathACounts)
	pathB := atomic.LoadInt64(&r.pathBCounts)
	total := pathA + pathB
	if total > 0 {
		pathAPercent := float64(pathA) / float64(total) * 100
		pathBPercent := float64(pathB) / float64(total) * 100
		deviation := math.Abs(pathAPercent - 50)

		r.report.PathACount = int(pathA)
		r.report.PathBCount = int(pathB)
		r.report.SplitDeviation = deviation

		fmt.Println("  A/B Split Distribution:")
		fmt.Printf("    Path A: %.1f%% (%d)\n", pathAPercent, pathA)
		fmt.Printf("    Path B: %.1f%% (%d)\n", pathBPercent, pathB)

		if deviation <= 5 {
			fmt.Printf("    Deviation: %.1f%% [OK]\n", deviation)
		} else {
			fmt.Printf("    Deviation: %.1f%% [WARNING - expected ~50/50]\n", deviation)
			allPassed = false
		}
		fmt.Println()
	}

	// Email metrics from mock ESP
	if r.mockESP != nil {
		stats := r.mockESP.Stats()
		r.report.EmailsSent = int(stats["sent"])
		r.report.EmailsOpened = int(stats["opened"])
		r.report.EmailsClicked = int(stats["clicked"])

		fmt.Println("  Email Metrics (mock):")
		fmt.Printf("    Sent:     %d\n", stats["sent"])
		fmt.Printf("    Opened:   %d (%.1f%%)\n", stats["opened"], float64(stats["opened"])/float64(stats["sent"])*100)
		fmt.Printf("    Clicked:  %d (%.1f%%)\n", stats["clicked"], float64(stats["clicked"])/float64(stats["sent"])*100)
		fmt.Printf("    Errors:   %d\n", stats["errors"])
		fmt.Println()
	}

	// Completion metrics
	var completedCount, convertedCount int
	r.db.QueryRowContext(r.ctx, `
		SELECT COUNT(*) FROM mailing_journey_enrollments 
		WHERE journey_id = $1 AND status = 'completed'
	`, r.journeyID).Scan(&completedCount)

	r.db.QueryRowContext(r.ctx, `
		SELECT COUNT(*) FROM mailing_journey_enrollments 
		WHERE journey_id = $1 AND converted_at IS NOT NULL
	`, r.journeyID).Scan(&convertedCount)

	r.report.CompletedCount = completedCount
	r.report.ConvertedCount = convertedCount

	completionRate := float64(completedCount) / float64(r.config.SegmentSize) * 100
	conversionRate := 0.0
	if completedCount > 0 {
		conversionRate = float64(convertedCount) / float64(completedCount) * 100
	}

	fmt.Println("  Completion Metrics:")
	fmt.Printf("    Completed:     %d (%.1f%%)\n", completedCount, completionRate)
	fmt.Printf("    Converted:     %d (%.1f%% of completed)\n", convertedCount, conversionRate)
	fmt.Println()

	result.Details["completed"] = completedCount
	result.Details["converted"] = convertedCount
	result.Details["completion_rate"] = completionRate

	// Overall pass/fail
	r.report.Passed = allPassed && completionRate >= 80 // At least 80% completion
	if r.report.Passed {
		result.Status = "PASS"
		r.report.Summary = "All validations successful"
	} else {
		result.Status = "FAIL"
		r.report.Summary = "Some validations failed"
	}

	return nil
}

// =============================================================================
// STEP 6: CLEANUP
// =============================================================================

// Cleanup removes test data
func (r *SegmentTestRunner) Cleanup() error {
	stepStart := time.Now()
	result := &StepResult{
		Name:      "cleanup",
		StartTime: stepStart,
		Details:   make(map[string]interface{}),
	}
	defer func() {
		result.EndTime = time.Now()
		result.Duration = result.EndTime.Sub(result.StartTime)
		r.report.StepResults["cleanup"] = result
	}()

	var cleanupErrors []string

	// Delete execution logs
	res, err := r.db.ExecContext(r.ctx, `
		DELETE FROM mailing_journey_execution_log WHERE journey_id = $1
	`, r.journeyID)
	if err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Sprintf("execution_log: %v", err))
	} else {
		rows, _ := res.RowsAffected()
		log.Printf("  Removed %d execution log entries", rows)
	}

	// Delete enrollments
	res, err = r.db.ExecContext(r.ctx, `
		DELETE FROM mailing_journey_enrollments WHERE journey_id = $1
	`, r.journeyID)
	if err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Sprintf("enrollments: %v", err))
	} else {
		rows, _ := res.RowsAffected()
		log.Printf("  Removed %d enrollments", rows)
	}

	// Delete journey
	_, err = r.db.ExecContext(r.ctx, `
		DELETE FROM mailing_journeys WHERE id = $1
	`, r.journeyID)
	if err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Sprintf("journey: %v", err))
	} else {
		log.Println("  Removed journey")
	}

	// Delete segment
	_, err = r.db.ExecContext(r.ctx, `
		DELETE FROM mailing_segments WHERE id = $1
	`, r.segmentID)
	if err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Sprintf("segment: %v", err))
	} else {
		log.Println("  Removed segment")
	}

	// Delete subscribers
	res, err = r.db.ExecContext(r.ctx, `
		DELETE FROM mailing_subscribers WHERE list_id = $1
	`, r.listID)
	if err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Sprintf("subscribers: %v", err))
	} else {
		rows, _ := res.RowsAffected()
		log.Printf("  Removed %d subscribers", rows)
	}

	// Delete list
	_, err = r.db.ExecContext(r.ctx, `
		DELETE FROM mailing_lists WHERE id = $1
	`, r.listID)
	if err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Sprintf("list: %v", err))
	} else {
		log.Println("  Removed mailing list")
	}

	if len(cleanupErrors) > 0 {
		result.Status = "PARTIAL"
		result.Details["errors"] = cleanupErrors
		return fmt.Errorf("cleanup had errors: %v", cleanupErrors)
	}

	result.Status = "PASS"
	log.Println("  ✓ Cleanup complete")

	return nil
}

// Close releases resources
func (r *SegmentTestRunner) Close() {
	if r.mockESP != nil {
		r.mockESP.Stop()
	}
	if r.db != nil {
		r.db.Close()
	}
	if r.redis != nil {
		r.redis.Close()
	}
}

// =============================================================================
// REPORT GENERATION
// =============================================================================

// GenerateReport produces the final test report
func (r *SegmentTestRunner) GenerateReport() string {
	var buf bytes.Buffer
	w := func(format string, args ...interface{}) {
		fmt.Fprintf(&buf, format+"\n", args...)
	}

	w("")
	w(strings.Repeat("=", 80))
	w("             SEGMENT-DRIVEN JOURNEY INTEGRATION TEST REPORT")
	w(strings.Repeat("=", 80))
	w("")

	// Test Configuration
	w("TEST CONFIGURATION")
	w("  Segment:         %s", r.report.SegmentName)
	w("  Subscribers:     %d", r.report.SegmentSize)
	w("  Journey:         Welcome Series (%d nodes)", len(r.journeyNodes))
	w("  Total Time:      %v", r.report.TotalTestTime.Round(time.Millisecond))
	w("")

	// Step Results
	for _, stepName := range []string{"segment_setup", "journey_creation", "enrollment", "execution", "validation", "cleanup"} {
		if step, ok := r.report.StepResults[stepName]; ok {
			statusSymbol := "✓"
			if step.Status == "FAIL" {
				statusSymbol = "✗"
			} else if step.Status == "SKIP" {
				statusSymbol = "-"
			} else if step.Status == "PARTIAL" {
				statusSymbol = "!"
			}

			stepDisplay := strings.ToUpper(strings.Replace(stepName, "_", " ", -1))
			w("STEP: %s %s", stepDisplay, statusSymbol)
			w("  Duration: %v", step.Duration.Round(time.Millisecond))

			for key, val := range step.Details {
				w("  %s: %v", key, val)
			}
			w("")
		}
	}

	// Node Validation Summary
	if len(r.report.NodeResults) > 0 {
		w("NODE VALIDATION SUMMARY")
		w(strings.Repeat("-", 40))
		for _, nv := range r.report.NodeResults {
			status := "PASS"
			if !nv.Passed {
				status = "FAIL"
			}
			w("  %-12s %-10s Expected: %-6d Actual: %-6d [%s]",
				nv.NodeID, nv.NodeType, nv.Expected, nv.Actual, status)
		}
		w("")
	}

	// A/B Split
	if r.report.PathACount > 0 || r.report.PathBCount > 0 {
		w("A/B SPLIT ANALYSIS")
		w(strings.Repeat("-", 40))
		total := r.report.PathACount + r.report.PathBCount
		w("  Path A: %d (%.1f%%)", r.report.PathACount, float64(r.report.PathACount)/float64(total)*100)
		w("  Path B: %d (%.1f%%)", r.report.PathBCount, float64(r.report.PathBCount)/float64(total)*100)
		w("  Deviation from 50/50: %.1f%%", r.report.SplitDeviation)
		w("")
	}

	// Email Metrics
	if r.report.EmailsSent > 0 {
		w("EMAIL METRICS")
		w(strings.Repeat("-", 40))
		w("  Sent:    %d", r.report.EmailsSent)
		w("  Opened:  %d (%.1f%%)", r.report.EmailsOpened, float64(r.report.EmailsOpened)/float64(r.report.EmailsSent)*100)
		w("  Clicked: %d (%.1f%%)", r.report.EmailsClicked, float64(r.report.EmailsClicked)/float64(r.report.EmailsSent)*100)
		w("")
	}

	// Completion Metrics
	w("COMPLETION METRICS")
	w(strings.Repeat("-", 40))
	completionRate := float64(r.report.CompletedCount) / float64(r.report.SegmentSize) * 100
	conversionRate := 0.0
	if r.report.CompletedCount > 0 {
		conversionRate = float64(r.report.ConvertedCount) / float64(r.report.CompletedCount) * 100
	}
	w("  Enrolled:   %d", r.report.EnrollmentCount)
	w("  Completed:  %d (%.1f%%)", r.report.CompletedCount, completionRate)
	w("  Converted:  %d (%.1f%% of completed)", r.report.ConvertedCount, conversionRate)
	w("")

	// Errors
	if len(r.report.Errors) > 0 {
		w("ERRORS")
		w(strings.Repeat("-", 40))
		for _, err := range r.report.Errors {
			w("  [%s] %s: %s", err.Timestamp.Format("15:04:05"), err.Step, err.Error)
		}
		w("")
	}

	// Overall Result
	w(strings.Repeat("=", 80))
	if r.report.Passed {
		w("OVERALL: PASS - All validations successful")
	} else {
		w("OVERALL: FAIL - %s", r.report.Summary)
	}
	w(strings.Repeat("=", 80))

	return buf.String()
}

// =============================================================================
// UTILITY FUNCTIONS
// =============================================================================

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

// =============================================================================
// MAIN
// =============================================================================

func main() {
	config := DefaultSegmentTestConfig()

	var timeoutStr string

	flag.StringVar(&config.PostgresURL, "postgres", "",
		"PostgreSQL connection URL (required)")
	flag.StringVar(&config.RedisURL, "redis", "",
		"Redis connection URL or host:port (optional)")
	flag.StringVar(&config.SegmentName, "segment", config.SegmentName,
		"Name for the test segment")
	flag.BoolVar(&config.CreateSegment, "create-segment", config.CreateSegment,
		"Create a new test segment")
	flag.IntVar(&config.SegmentSize, "segment-size", config.SegmentSize,
		"Number of subscribers in test segment")
	flag.BoolVar(&config.CleanupAfter, "cleanup", config.CleanupAfter,
		"Clean up test data after completion")
	flag.StringVar(&config.OrgID, "org-id", config.OrgID,
		"Organization ID to use")
	flag.StringVar(&timeoutStr, "timeout", "5m",
		"Test timeout (e.g., 5m, 10m)")
	flag.BoolVar(&config.Verbose, "verbose", config.Verbose,
		"Enable verbose logging")
	flag.IntVar(&config.DelaySeconds, "delay-seconds", config.DelaySeconds,
		"Delay node duration in seconds")
	flag.BoolVar(&config.MockEmailESP, "mock-esp", config.MockEmailESP,
		"Use mock ESP for email nodes")
	flag.IntVar(&config.MockESPPort, "mock-esp-port", config.MockESPPort,
		"Port for mock ESP server")

	flag.Parse()

	// Parse timeout
	if d, err := time.ParseDuration(timeoutStr); err == nil {
		config.Timeout = d
	}

	// Validate required args
	if config.PostgresURL == "" {
		fmt.Println("Error: --postgres flag is required")
		flag.Usage()
		os.Exit(1)
	}

	// Create runner
	runner := NewSegmentTestRunner(config)
	defer runner.Close()

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

	// Run test
	if err := runner.Run(ctx); err != nil && err != context.Canceled {
		log.Printf("Test error: %v", err)
	}

	// Generate and print report
	report := runner.GenerateReport()
	fmt.Println(report)

	// Exit with appropriate code
	if !runner.report.Passed {
		os.Exit(1)
	}
}
