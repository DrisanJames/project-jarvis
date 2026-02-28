package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// =============================================================================
// BATCH SEND WORKER TESTS
// =============================================================================

// MockBatchESPSender is a mock implementation of BatchESPSender for testing
type MockBatchESPSender struct {
	mu           sync.Mutex
	sentBatches  [][]EmailMessage
	maxBatchSize int
	failAfter    int // Fail after N messages (0 = never fail)
	failError    error
	delay        time.Duration // Artificial delay per batch
}

func NewMockBatchESPSender(maxBatchSize int) *MockBatchESPSender {
	return &MockBatchESPSender{
		maxBatchSize: maxBatchSize,
		sentBatches:  make([][]EmailMessage, 0),
	}
}

func (m *MockBatchESPSender) MaxBatchSize() int {
	return m.maxBatchSize
}

func (m *MockBatchESPSender) SendBatch(ctx context.Context, messages []EmailMessage) (*BatchSendResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.delay > 0 {
		time.Sleep(m.delay)
	}

	// Store the batch for verification
	m.sentBatches = append(m.sentBatches, messages)

	result := &BatchSendResult{
		TransmissionID: uuid.New().String(),
		Results:        make([]SendResult, len(messages)),
	}

	for i, msg := range messages {
		// Check if we should fail this message
		totalSent := 0
		for _, batch := range m.sentBatches[:len(m.sentBatches)-1] {
			totalSent += len(batch)
		}
		totalSent += i

		if m.failAfter > 0 && totalSent >= m.failAfter {
			result.Results[i] = SendResult{
				Success: false,
				Error:   m.failError,
				ESPType: "mock",
			}
			result.Rejected++
		} else {
			result.Results[i] = SendResult{
				Success:   true,
				MessageID: fmt.Sprintf("mock-%s", uuid.New().String()[:8]),
				ESPType:   "mock",
				SentAt:    time.Now(),
			}
			result.Accepted++
		}
		_ = msg
	}

	return result, nil
}

func (m *MockBatchESPSender) GetSentBatches() [][]EmailMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sentBatches
}

func (m *MockBatchESPSender) TotalMessagesSent() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := 0
	for _, batch := range m.sentBatches {
		total += len(batch)
	}
	return total
}

// =============================================================================
// BATCH GROUPER TESTS
// =============================================================================

func TestBatchGrouper_GetBatchSize(t *testing.T) {
	grouper := NewBatchGrouper()

	tests := []struct {
		espType  string
		expected int
	}{
		{"sparkpost", SparkPostBatchSize},
		{"ses", SESBatchSize},
		{"mailgun", MailgunBatchSize},
		{"sendgrid", SendGridBatchSize},
		{"unknown", DefaultBatchSize},
		{"", DefaultBatchSize},
	}

	for _, tc := range tests {
		t.Run(tc.espType, func(t *testing.T) {
			got := grouper.GetBatchSize(tc.espType)
			if got != tc.expected {
				t.Errorf("GetBatchSize(%q) = %d, want %d", tc.espType, got, tc.expected)
			}
		})
	}
}

func TestBatchGrouper_GroupIntoBatches(t *testing.T) {
	grouper := NewBatchGrouper()

	tests := []struct {
		name           string
		itemCount      int
		espType        string
		expectedBatches int
	}{
		{"sparkpost_small", 100, "sparkpost", 1},      // 100 items, batch size 2000 = 1 batch
		{"sparkpost_large", 5000, "sparkpost", 3},     // 5000 items, batch size 2000 = 3 batches
		{"ses_small", 30, "ses", 1},                   // 30 items, batch size 50 = 1 batch
		{"ses_exact", 50, "ses", 1},                   // 50 items, batch size 50 = 1 batch
		{"ses_large", 150, "ses", 3},                  // 150 items, batch size 50 = 3 batches
		{"mailgun_medium", 1500, "mailgun", 2},        // 1500 items, batch size 1000 = 2 batches
		{"empty", 0, "sparkpost", 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			items := make([]BatchQueueItem, tc.itemCount)
			for i := 0; i < tc.itemCount; i++ {
				items[i] = BatchQueueItem{
					ID:    uuid.New(),
					Email: fmt.Sprintf("test%d@example.com", i),
				}
			}

			batches := grouper.GroupIntoBatches(items, tc.espType)
			if len(batches) != tc.expectedBatches {
				t.Errorf("GroupIntoBatches() returned %d batches, want %d", len(batches), tc.expectedBatches)
			}

			// Verify all items are included
			totalItems := 0
			for _, batch := range batches {
				totalItems += len(batch)
			}
			if totalItems != tc.itemCount {
				t.Errorf("Total items in batches = %d, want %d", totalItems, tc.itemCount)
			}

			// Verify batch sizes don't exceed limit
			batchSize := grouper.GetBatchSize(tc.espType)
			for i, batch := range batches {
				if len(batch) > batchSize {
					t.Errorf("Batch %d has %d items, exceeds limit %d", i, len(batch), batchSize)
				}
			}
		})
	}
}

// =============================================================================
// BATCH QUEUE ITEM GROUPING TESTS
// =============================================================================

func TestBatchSendWorker_GroupByESP(t *testing.T) {
	worker := &BatchSendWorker{}

	items := []BatchQueueItem{
		{ID: uuid.New(), Email: "test1@example.com", ESPType: "sparkpost"},
		{ID: uuid.New(), Email: "test2@example.com", ESPType: "sparkpost"},
		{ID: uuid.New(), Email: "test3@example.com", ESPType: "ses"},
		{ID: uuid.New(), Email: "test4@example.com", ESPType: "mailgun"},
		{ID: uuid.New(), Email: "test5@example.com", ESPType: "ses"},
		{ID: uuid.New(), Email: "test6@example.com", ESPType: ""}, // Should default to ses
	}

	groups := worker.groupByESP(items)

	// Check sparkpost group
	if len(groups["sparkpost"]) != 2 {
		t.Errorf("sparkpost group has %d items, want 2", len(groups["sparkpost"]))
	}

	// Check ses group (includes empty ESP type)
	if len(groups["ses"]) != 3 {
		t.Errorf("ses group has %d items, want 3", len(groups["ses"]))
	}

	// Check mailgun group
	if len(groups["mailgun"]) != 1 {
		t.Errorf("mailgun group has %d items, want 1", len(groups["mailgun"]))
	}
}

// =============================================================================
// SUBSTITUTION TESTS
// =============================================================================

func TestBatchSendWorker_ApplySubstitutions(t *testing.T) {
	worker := &BatchSendWorker{}

	tests := []struct {
		name     string
		template string
		data     map[string]interface{}
		expected string
	}{
		{
			name:     "simple substitution",
			template: "Hello {{ first_name }}!",
			data:     map[string]interface{}{"first_name": "John"},
			expected: "Hello John!",
		},
		{
			name:     "no spaces",
			template: "Hello {{first_name}}!",
			data:     map[string]interface{}{"first_name": "John"},
			expected: "Hello John!",
		},
		{
			name:     "multiple substitutions",
			template: "Hello {{ first_name }} {{ last_name }}, your email is {{email}}",
			data: map[string]interface{}{
				"first_name": "John",
				"last_name":  "Doe",
				"email":      "john@example.com",
			},
			expected: "Hello John Doe, your email is john@example.com",
		},
		{
			name:     "missing placeholder",
			template: "Hello {{ first_name }}, your age is {{ age }}",
			data:     map[string]interface{}{"first_name": "John"},
			expected: "Hello John, your age is {{ age }}",
		},
		{
			name:     "numeric value",
			template: "You have {{ count }} items",
			data:     map[string]interface{}{"count": 42},
			expected: "You have 42 items",
		},
		{
			name:     "empty data",
			template: "Hello {{ name }}!",
			data:     map[string]interface{}{},
			expected: "Hello {{ name }}!",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := worker.applySubstitutions(tc.template, tc.data)
			if result != tc.expected {
				t.Errorf("applySubstitutions() = %q, want %q", result, tc.expected)
			}
		})
	}
}

// =============================================================================
// BATCH RESULT PROCESSING TESTS
// =============================================================================

func TestBatchItemResult_StatusMapping(t *testing.T) {
	tests := []struct {
		name           string
		result         BatchItemResult
		expectedStatus string
	}{
		{
			name: "success",
			result: BatchItemResult{
				ID:        uuid.New(),
				Success:   true,
				MessageID: "msg-123",
			},
			expectedStatus: "sent",
		},
		{
			name: "failure with error",
			result: BatchItemResult{
				ID:        uuid.New(),
				Success:   false,
				ErrorCode: "rate_limited",
				Error:     fmt.Errorf("rate limit exceeded"),
			},
			expectedStatus: "failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var status string
			if tc.result.Success {
				status = "sent"
			} else {
				status = "failed"
			}

			if status != tc.expectedStatus {
				t.Errorf("status = %q, want %q", status, tc.expectedStatus)
			}
		})
	}
}

// =============================================================================
// MOCK BATCH SENDER TESTS
// =============================================================================

func TestMockBatchESPSender_SendBatch(t *testing.T) {
	sender := NewMockBatchESPSender(100)
	ctx := context.Background()

	messages := make([]EmailMessage, 50)
	for i := 0; i < 50; i++ {
		messages[i] = EmailMessage{
			ID:    uuid.New().String(),
			Email: fmt.Sprintf("test%d@example.com", i),
		}
	}

	result, err := sender.SendBatch(ctx, messages)
	if err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}

	if result.Accepted != 50 {
		t.Errorf("Accepted = %d, want 50", result.Accepted)
	}

	if result.Rejected != 0 {
		t.Errorf("Rejected = %d, want 0", result.Rejected)
	}

	if len(result.Results) != 50 {
		t.Errorf("Results length = %d, want 50", len(result.Results))
	}

	// Verify all results are successful
	for i, r := range result.Results {
		if !r.Success {
			t.Errorf("Result[%d].Success = false, want true", i)
		}
		if r.MessageID == "" {
			t.Errorf("Result[%d].MessageID is empty", i)
		}
	}

	// Verify batch was recorded
	batches := sender.GetSentBatches()
	if len(batches) != 1 {
		t.Errorf("GetSentBatches() length = %d, want 1", len(batches))
	}

	if sender.TotalMessagesSent() != 50 {
		t.Errorf("TotalMessagesSent() = %d, want 50", sender.TotalMessagesSent())
	}
}

func TestMockBatchESPSender_PartialFailure(t *testing.T) {
	sender := NewMockBatchESPSender(100)
	sender.failAfter = 30
	sender.failError = fmt.Errorf("simulated failure")
	ctx := context.Background()

	messages := make([]EmailMessage, 50)
	for i := 0; i < 50; i++ {
		messages[i] = EmailMessage{
			ID:    uuid.New().String(),
			Email: fmt.Sprintf("test%d@example.com", i),
		}
	}

	result, err := sender.SendBatch(ctx, messages)
	if err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}

	if result.Accepted != 30 {
		t.Errorf("Accepted = %d, want 30", result.Accepted)
	}

	if result.Rejected != 20 {
		t.Errorf("Rejected = %d, want 20", result.Rejected)
	}
}

// =============================================================================
// ESP BATCH SENDER TESTS
// =============================================================================

func TestSparkPostBatchSender_MaxBatchSize(t *testing.T) {
	sender := NewSparkPostBatchSender("test-key", nil)
	if sender.MaxBatchSize() != SparkPostBatchSize {
		t.Errorf("MaxBatchSize() = %d, want %d", sender.MaxBatchSize(), SparkPostBatchSize)
	}
}

func TestSESBatchSender_MaxBatchSize(t *testing.T) {
	sender := NewSESBatchSender("key", "secret", "us-east-1", nil)
	if sender.MaxBatchSize() != SESBatchSize {
		t.Errorf("MaxBatchSize() = %d, want %d", sender.MaxBatchSize(), SESBatchSize)
	}
}

func TestMailgunBatchSender_MaxBatchSize(t *testing.T) {
	sender := NewMailgunBatchSender("test-key", "example.com", nil)
	if sender.MaxBatchSize() != MailgunBatchSize {
		t.Errorf("MaxBatchSize() = %d, want %d", sender.MaxBatchSize(), MailgunBatchSize)
	}
}

func TestSendGridBatchSender_MaxBatchSize(t *testing.T) {
	sender := NewSendGridBatchSender("test-key", nil)
	if sender.MaxBatchSize() != SendGridBatchSize {
		t.Errorf("MaxBatchSize() = %d, want %d", sender.MaxBatchSize(), SendGridBatchSize)
	}
}

func TestSparkPostBatchSender_EmptyBatch(t *testing.T) {
	sender := NewSparkPostBatchSender("test-key", nil)
	ctx := context.Background()

	result, err := sender.SendBatch(ctx, []EmailMessage{})
	if err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}

	if result.Accepted != 0 {
		t.Errorf("Accepted = %d, want 0", result.Accepted)
	}
}

func TestSparkPostBatchSender_NoApiKey(t *testing.T) {
	sender := NewSparkPostBatchSender("", nil)
	ctx := context.Background()

	messages := []EmailMessage{{Email: "test@example.com"}}
	_, err := sender.SendBatch(ctx, messages)
	if err == nil {
		t.Error("SendBatch() should return error when API key is missing")
	}
}

func TestSESBatchSender_NoCredentials(t *testing.T) {
	sender := NewSESBatchSender("", "", "us-east-1", nil)
	ctx := context.Background()

	messages := []EmailMessage{{Email: "test@example.com"}}
	_, err := sender.SendBatch(ctx, messages)
	if err == nil {
		t.Error("SendBatch() should return error when credentials are missing")
	}
}

func TestMailgunBatchSender_NoApiKey(t *testing.T) {
	sender := NewMailgunBatchSender("", "example.com", nil)
	ctx := context.Background()

	messages := []EmailMessage{{Email: "test@example.com"}}
	_, err := sender.SendBatch(ctx, messages)
	if err == nil {
		t.Error("SendBatch() should return error when API key is missing")
	}
}

func TestSendGridBatchSender_NoApiKey(t *testing.T) {
	sender := NewSendGridBatchSender("", nil)
	ctx := context.Background()

	messages := []EmailMessage{{Email: "test@example.com"}}
	_, err := sender.SendBatch(ctx, messages)
	if err == nil {
		t.Error("SendBatch() should return error when API key is missing")
	}
}

// =============================================================================
// BATCH SEND WORKER CONFIGURATION TESTS
// =============================================================================

func TestBatchSendWorker_NewBatchSendWorker(t *testing.T) {
	worker := NewBatchSendWorker(nil, nil)

	if worker.workerID == "" {
		t.Error("workerID should not be empty")
	}

	if worker.claimSize != 1000 {
		t.Errorf("claimSize = %d, want 1000", worker.claimSize)
	}

	if worker.numWorkers != 4 {
		t.Errorf("numWorkers = %d, want 4", worker.numWorkers)
	}

	if worker.batchGrouper == nil {
		t.Error("batchGrouper should not be nil")
	}
}

func TestBatchSendWorker_SetClaimSize(t *testing.T) {
	worker := NewBatchSendWorker(nil, nil)

	worker.SetClaimSize(2000)
	if worker.claimSize != 2000 {
		t.Errorf("claimSize = %d, want 2000", worker.claimSize)
	}

	// Should not change for invalid values
	worker.SetClaimSize(0)
	if worker.claimSize != 2000 {
		t.Errorf("claimSize should remain 2000 for invalid value")
	}

	worker.SetClaimSize(-100)
	if worker.claimSize != 2000 {
		t.Errorf("claimSize should remain 2000 for negative value")
	}
}

func TestBatchSendWorker_SetNumWorkers(t *testing.T) {
	worker := NewBatchSendWorker(nil, nil)

	worker.SetNumWorkers(8)
	if worker.numWorkers != 8 {
		t.Errorf("numWorkers = %d, want 8", worker.numWorkers)
	}

	// Should not change for invalid values
	worker.SetNumWorkers(0)
	if worker.numWorkers != 8 {
		t.Errorf("numWorkers should remain 8 for invalid value")
	}
}

func TestBatchSendWorker_Stats(t *testing.T) {
	worker := NewBatchSendWorker(nil, nil)

	stats := worker.Stats()

	if stats["total_sent"] != 0 {
		t.Errorf("total_sent = %d, want 0", stats["total_sent"])
	}

	if stats["total_failed"] != 0 {
		t.Errorf("total_failed = %d, want 0", stats["total_failed"])
	}

	if stats["total_skipped"] != 0 {
		t.Errorf("total_skipped = %d, want 0", stats["total_skipped"])
	}

	// Simulate some activity
	atomic.AddInt64(&worker.totalSent, 100)
	atomic.AddInt64(&worker.totalFailed, 5)

	stats = worker.Stats()
	if stats["total_sent"] != 100 {
		t.Errorf("total_sent = %d, want 100", stats["total_sent"])
	}
	if stats["total_failed"] != 5 {
		t.Errorf("total_failed = %d, want 5", stats["total_failed"])
	}
}

// =============================================================================
// TRUNCATE STRING TESTS
// =============================================================================

func TestTruncateString(t *testing.T) {
	tests := []struct {
		input    string
		maxLen   int
		expected string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hello"},
		{"", 10, ""},
		{"test", 4, "test"},
		{"longer string", 6, "longer"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := truncateString(tc.input, tc.maxLen)
			if result != tc.expected {
				t.Errorf("truncateString(%q, %d) = %q, want %q", tc.input, tc.maxLen, result, tc.expected)
			}
		})
	}
}

// =============================================================================
// CONCURRENT BATCH PROCESSING TESTS
// =============================================================================

func TestBatchSendWorker_ConcurrentStats(t *testing.T) {
	worker := NewBatchSendWorker(nil, nil)

	// Simulate concurrent updates
	var wg sync.WaitGroup
	numGoroutines := 100
	updatesPerGoroutine := 1000

	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < updatesPerGoroutine; j++ {
				atomic.AddInt64(&worker.totalSent, 1)
			}
		}()
	}
	wg.Wait()

	stats := worker.Stats()
	expectedTotal := int64(numGoroutines * updatesPerGoroutine)
	if stats["total_sent"] != expectedTotal {
		t.Errorf("total_sent = %d, want %d", stats["total_sent"], expectedTotal)
	}
}

// =============================================================================
// BATCH RESULT AGGREGATION TESTS
// =============================================================================

func TestBatchSendResult_Aggregation(t *testing.T) {
	result := &BatchSendResult{
		TransmissionID: "test-tx-123",
		Results:        make([]SendResult, 100),
	}

	// Simulate mixed results
	for i := 0; i < 100; i++ {
		if i%5 == 0 {
			result.Results[i] = SendResult{
				Success: false,
				Error:   fmt.Errorf("simulated error"),
			}
			result.Rejected++
		} else {
			result.Results[i] = SendResult{
				Success:   true,
				MessageID: fmt.Sprintf("msg-%d", i),
			}
			result.Accepted++
		}
	}

	if result.Accepted != 80 {
		t.Errorf("Accepted = %d, want 80", result.Accepted)
	}

	if result.Rejected != 20 {
		t.Errorf("Rejected = %d, want 20", result.Rejected)
	}

	// Verify totals
	if result.Accepted+result.Rejected != 100 {
		t.Error("Accepted + Rejected should equal 100")
	}
}

// =============================================================================
// BATCH GROUPER EDGE CASES
// =============================================================================

func TestBatchGrouper_EdgeCases(t *testing.T) {
	grouper := NewBatchGrouper()

	// Test exact batch size boundary
	t.Run("exact_boundary", func(t *testing.T) {
		items := make([]BatchQueueItem, SESBatchSize) // Exactly 50
		for i := 0; i < SESBatchSize; i++ {
			items[i] = BatchQueueItem{ID: uuid.New()}
		}

		batches := grouper.GroupIntoBatches(items, "ses")
		if len(batches) != 1 {
			t.Errorf("Expected 1 batch for exact boundary, got %d", len(batches))
		}
	})

	// Test one over batch size boundary
	t.Run("one_over_boundary", func(t *testing.T) {
		items := make([]BatchQueueItem, SESBatchSize+1) // 51
		for i := 0; i < SESBatchSize+1; i++ {
			items[i] = BatchQueueItem{ID: uuid.New()}
		}

		batches := grouper.GroupIntoBatches(items, "ses")
		if len(batches) != 2 {
			t.Errorf("Expected 2 batches for one over boundary, got %d", len(batches))
		}

		// First batch should be full, second should have 1
		if len(batches[0]) != SESBatchSize {
			t.Errorf("First batch should have %d items, got %d", SESBatchSize, len(batches[0]))
		}
		if len(batches[1]) != 1 {
			t.Errorf("Second batch should have 1 item, got %d", len(batches[1]))
		}
	})

	// Test large batch
	t.Run("large_batch", func(t *testing.T) {
		items := make([]BatchQueueItem, 10000)
		for i := 0; i < 10000; i++ {
			items[i] = BatchQueueItem{ID: uuid.New()}
		}

		batches := grouper.GroupIntoBatches(items, "sparkpost")
		expectedBatches := 5 // 10000 / 2000 = 5
		if len(batches) != expectedBatches {
			t.Errorf("Expected %d batches, got %d", expectedBatches, len(batches))
		}
	})
}

// =============================================================================
// BENCHMARK TESTS
// =============================================================================

func BenchmarkBatchGrouper_GroupIntoBatches(b *testing.B) {
	grouper := NewBatchGrouper()
	items := make([]BatchQueueItem, 10000)
	for i := 0; i < 10000; i++ {
		items[i] = BatchQueueItem{
			ID:      uuid.New(),
			Email:   fmt.Sprintf("test%d@example.com", i),
			ESPType: "sparkpost",
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		grouper.GroupIntoBatches(items, "sparkpost")
	}
}

func BenchmarkBatchSendWorker_GroupByESP(b *testing.B) {
	worker := &BatchSendWorker{}
	items := make([]BatchQueueItem, 1000)
	espTypes := []string{"sparkpost", "ses", "mailgun", "sendgrid"}

	for i := 0; i < 1000; i++ {
		items[i] = BatchQueueItem{
			ID:      uuid.New(),
			Email:   fmt.Sprintf("test%d@example.com", i),
			ESPType: espTypes[i%len(espTypes)],
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		worker.groupByESP(items)
	}
}

func BenchmarkBatchSendWorker_ApplySubstitutions(b *testing.B) {
	worker := &BatchSendWorker{}
	template := "Hello {{ first_name }} {{ last_name }}, your order #{{ order_id }} has shipped to {{ address }}"
	data := map[string]interface{}{
		"first_name": "John",
		"last_name":  "Doe",
		"order_id":   12345,
		"address":    "123 Main St, City, State 12345",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		worker.applySubstitutions(template, data)
	}
}

func BenchmarkMockBatchESPSender_SendBatch(b *testing.B) {
	sender := NewMockBatchESPSender(1000)
	ctx := context.Background()

	messages := make([]EmailMessage, 1000)
	for i := 0; i < 1000; i++ {
		messages[i] = EmailMessage{
			ID:    uuid.New().String(),
			Email: fmt.Sprintf("test%d@example.com", i),
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sender.SendBatch(ctx, messages)
	}
}

// =============================================================================
// CAMPAIGN CONTENT CACHE TESTS
// =============================================================================

func TestCampaignContentCache(t *testing.T) {
	worker := NewBatchSendWorker(nil, nil)

	// Test cache write and read
	campaignID := uuid.New()
	content := &CampaignContent{
		Subject:     "Test Subject",
		HTMLContent: "<h1>Test</h1>",
		FromEmail:   "test@example.com",
		FetchedAt:   time.Now(),
	}

	// Write to cache
	worker.cacheMu.Lock()
	worker.contentCache[campaignID.String()] = content
	worker.cacheMu.Unlock()

	// Read from cache
	worker.cacheMu.RLock()
	cached, ok := worker.contentCache[campaignID.String()]
	worker.cacheMu.RUnlock()

	if !ok {
		t.Fatal("Expected content to be in cache")
	}

	if cached.Subject != content.Subject {
		t.Errorf("Cached subject = %q, want %q", cached.Subject, content.Subject)
	}
}

// =============================================================================
// INTEGRATION-STYLE TESTS (Without DB)
// =============================================================================

func TestBatchSendWorker_SendBatch_Integration(t *testing.T) {
	worker := NewBatchSendWorker(nil, nil)

	// Set up mock senders
	sparkpostSender := NewMockBatchESPSender(SparkPostBatchSize)
	sesSender := NewMockBatchESPSender(SESBatchSize)
	mailgunSender := NewMockBatchESPSender(MailgunBatchSize)
	sendgridSender := NewMockBatchESPSender(SendGridBatchSize)

	worker.SetBatchSenders(sparkpostSender, sesSender, mailgunSender, sendgridSender)

	// Pre-populate cache with test campaign content
	testCampaignID := uuid.New()
	worker.cacheMu.Lock()
	worker.contentCache[testCampaignID.String()] = &CampaignContent{
		Subject:     "Test Campaign",
		HTMLContent: "<h1>Hello {{ first_name }}!</h1>",
		TextContent: "Hello {{ first_name }}!",
		FromName:    "Test Sender",
		FromEmail:   "sender@example.com",
		ProfileID:   "profile-123",
		ESPType:     "sparkpost",
		FetchedAt:   time.Now(),
	}
	worker.cacheMu.Unlock()

	// Create test items
	items := make([]BatchQueueItem, 100)
	for i := 0; i < 100; i++ {
		items[i] = BatchQueueItem{
			ID:           uuid.New(),
			CampaignID:   testCampaignID,
			SubscriberID: uuid.New(),
			Email:        fmt.Sprintf("test%d@example.com", i),
			ESPType:      "sparkpost",
			SubstitutionData: map[string]interface{}{
				"first_name": fmt.Sprintf("User%d", i),
			},
		}
	}

	// Send batch
	ctx := context.Background()
	results := worker.sendBatch(ctx, "sparkpost", items)

	// Verify results
	if len(results) != 100 {
		t.Fatalf("Expected 100 results, got %d", len(results))
	}

	successCount := 0
	for _, r := range results {
		if r.Success {
			successCount++
		}
	}

	if successCount != 100 {
		t.Errorf("Expected 100 successes, got %d", successCount)
	}

	// Verify mock sender received the batch
	if sparkpostSender.TotalMessagesSent() != 100 {
		t.Errorf("SparkPost sender should have 100 messages, got %d", sparkpostSender.TotalMessagesSent())
	}
}

// =============================================================================
// ERROR HANDLING TESTS
// =============================================================================

func TestBatchSendWorker_SendBatch_NoSenderConfigured(t *testing.T) {
	worker := NewBatchSendWorker(nil, nil)
	// Don't set any senders

	// Pre-populate cache
	testCampaignID := uuid.New()
	worker.cacheMu.Lock()
	worker.contentCache[testCampaignID.String()] = &CampaignContent{
		Subject:   "Test",
		FetchedAt: time.Now(),
	}
	worker.cacheMu.Unlock()

	items := []BatchQueueItem{
		{
			ID:         uuid.New(),
			CampaignID: testCampaignID,
			Email:      "test@example.com",
			ESPType:    "sparkpost",
		},
	}

	ctx := context.Background()
	results := worker.sendBatch(ctx, "sparkpost", items)

	if len(results) != 1 {
		t.Fatalf("Expected 1 result, got %d", len(results))
	}

	if results[0].Success {
		t.Error("Expected failure when no sender is configured")
	}

	if results[0].ErrorCode != "no_sender_configured" {
		t.Errorf("Expected error code 'no_sender_configured', got %q", results[0].ErrorCode)
	}
}

// =============================================================================
// JSON SERIALIZATION TESTS
// =============================================================================

func TestBatchQueueItem_SubstitutionDataParsing(t *testing.T) {
	jsonData := `{"first_name": "John", "last_name": "Doe", "order_count": 5}`

	var data map[string]interface{}
	err := json.Unmarshal([]byte(jsonData), &data)
	if err != nil {
		t.Fatalf("Failed to parse JSON: %v", err)
	}

	if data["first_name"] != "John" {
		t.Errorf("first_name = %v, want John", data["first_name"])
	}

	// Note: JSON numbers are parsed as float64
	if data["order_count"].(float64) != 5 {
		t.Errorf("order_count = %v, want 5", data["order_count"])
	}
}
