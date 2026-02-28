package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// =============================================================================
// SparkPostBatchSender Tests
// =============================================================================

func TestNewSparkPostBatchSender(t *testing.T) {
	sender := NewSparkPostBatchSender("test-api-key", nil)

	if sender.apiKey != "test-api-key" {
		t.Errorf("expected apiKey 'test-api-key', got '%s'", sender.apiKey)
	}
	if sender.maxBatch != SparkPostBatchSize {
		t.Errorf("expected maxBatch %d, got %d", SparkPostBatchSize, sender.maxBatch)
	}
	if sender.maxPayloadMB != SparkPostMaxPayloadMB {
		t.Errorf("expected maxPayloadMB %d, got %d", SparkPostMaxPayloadMB, sender.maxPayloadMB)
	}
	if sender.baseURL != "https://api.sparkpost.com/api/v1" {
		t.Errorf("unexpected baseURL: %s", sender.baseURL)
	}
}

func TestNewSparkPostBatchSenderWithConfig(t *testing.T) {
	tests := []struct {
		name          string
		config        SparkPostBatchConfig
		expectedBatch int
		expectedMB    int
		expectedURL   string
	}{
		{
			name: "custom values",
			config: SparkPostBatchConfig{
				APIKey:       "key",
				BaseURL:      "https://custom.api.com",
				MaxBatch:     1000,
				MaxPayloadMB: 3,
				Timeout:      30 * time.Second,
			},
			expectedBatch: 1000,
			expectedMB:    3,
			expectedURL:   "https://custom.api.com",
		},
		{
			name: "exceeds max batch - capped",
			config: SparkPostBatchConfig{
				APIKey:   "key",
				MaxBatch: 5000, // exceeds 2000 max
			},
			expectedBatch: SparkPostBatchSize,
			expectedMB:    SparkPostMaxPayloadMB,
			expectedURL:   "https://api.sparkpost.com/api/v1",
		},
		{
			name: "zero values - use defaults",
			config: SparkPostBatchConfig{
				APIKey: "key",
			},
			expectedBatch: SparkPostBatchSize,
			expectedMB:    SparkPostMaxPayloadMB,
			expectedURL:   "https://api.sparkpost.com/api/v1",
		},
		{
			name: "negative values - use defaults",
			config: SparkPostBatchConfig{
				APIKey:       "key",
				MaxBatch:     -1,
				MaxPayloadMB: -1,
			},
			expectedBatch: SparkPostBatchSize,
			expectedMB:    SparkPostMaxPayloadMB,
			expectedURL:   "https://api.sparkpost.com/api/v1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := NewSparkPostBatchSenderWithConfig(tt.config, nil)

			if sender.maxBatch != tt.expectedBatch {
				t.Errorf("expected maxBatch %d, got %d", tt.expectedBatch, sender.maxBatch)
			}
			if sender.maxPayloadMB != tt.expectedMB {
				t.Errorf("expected maxPayloadMB %d, got %d", tt.expectedMB, sender.maxPayloadMB)
			}
			if sender.baseURL != tt.expectedURL {
				t.Errorf("expected baseURL '%s', got '%s'", tt.expectedURL, sender.baseURL)
			}
		})
	}
}

func TestSparkPostBatchSender_SendBatch_Success(t *testing.T) {
	// Create mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/transmissions" {
			t.Errorf("expected /transmissions, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "test-api-key" {
			t.Errorf("missing or wrong Authorization header")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("missing or wrong Content-Type header")
		}

		// Parse request body
		var transmission SparkPostTransmission
		if err := json.NewDecoder(r.Body).Decode(&transmission); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}

		// Verify recipients
		if len(transmission.Recipients) != 3 {
			t.Errorf("expected 3 recipients, got %d", len(transmission.Recipients))
		}

		// Verify metadata on recipients (queue_id, campaign_id, subscriber_id)
		for i, recipient := range transmission.Recipients {
			if recipient.Metadata["queue_id"] == nil {
				t.Errorf("recipient %d missing queue_id metadata", i)
			}
			if recipient.Metadata["campaign_id"] == nil {
				t.Errorf("recipient %d missing campaign_id metadata", i)
			}
			if recipient.Metadata["subscriber_id"] == nil {
				t.Errorf("recipient %d missing subscriber_id metadata", i)
			}
		}

		// Verify content
		if transmission.Content.Subject != "Test Subject" {
			t.Errorf("unexpected subject: %s", transmission.Content.Subject)
		}

		// Return success response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(SparkPostBatchResponse{
			Results: struct {
				ID                  string `json:"id"`
				TotalAcceptedRecips int    `json:"total_accepted_recipients"`
				TotalRejectedRecips int    `json:"total_rejected_recipients"`
			}{
				ID:                  "transmission-123",
				TotalAcceptedRecips: 3,
				TotalRejectedRecips: 0,
			},
		})
	}))
	defer server.Close()

	// Create sender with test server
	sender := NewSparkPostBatchSenderWithConfig(SparkPostBatchConfig{
		APIKey:  "test-api-key",
		BaseURL: server.URL,
	}, nil)

	// Create test messages with substitution_data for personalization
	messages := []EmailMessage{
		{
			ID:           "msg-1",
			CampaignID:   "camp-1",
			SubscriberID: "sub-1",
			Email:        "user1@example.com",
			FromName:     "Test Sender",
			FromEmail:    "sender@example.com",
			Subject:      "Test Subject",
			HTMLContent:  "<h1>Hello {{name}}</h1>",
			TextContent:  "Hello {{name}}",
			Metadata:     map[string]interface{}{"name": "User 1"},
		},
		{
			ID:           "msg-2",
			CampaignID:   "camp-1",
			SubscriberID: "sub-2",
			Email:        "user2@example.com",
			FromName:     "Test Sender",
			FromEmail:    "sender@example.com",
			Subject:      "Test Subject",
			HTMLContent:  "<h1>Hello {{name}}</h1>",
			TextContent:  "Hello {{name}}",
			Metadata:     map[string]interface{}{"name": "User 2"},
		},
		{
			ID:           "msg-3",
			CampaignID:   "camp-1",
			SubscriberID: "sub-3",
			Email:        "user3@example.com",
			FromName:     "Test Sender",
			FromEmail:    "sender@example.com",
			Subject:      "Test Subject",
			HTMLContent:  "<h1>Hello {{name}}</h1>",
			TextContent:  "Hello {{name}}",
			Metadata:     map[string]interface{}{"name": "User 3"},
		},
	}

	// Send batch
	ctx := context.Background()
	result, err := sender.SendBatch(ctx, messages)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.TransmissionID != "transmission-123" {
		t.Errorf("expected TransmissionID 'transmission-123', got '%s'", result.TransmissionID)
	}
	if result.Accepted != 3 {
		t.Errorf("expected Accepted 3, got %d", result.Accepted)
	}
	if result.Rejected != 0 {
		t.Errorf("expected Rejected 0, got %d", result.Rejected)
	}
	if len(result.Results) != 3 {
		t.Errorf("expected 3 results, got %d", len(result.Results))
	}

	for i, r := range result.Results {
		if !r.Success {
			t.Errorf("result %d: expected success", i)
		}
		if r.ESPType != "sparkpost" {
			t.Errorf("result %d: expected ESPType 'sparkpost', got '%s'", i, r.ESPType)
		}
	}
}

func TestSparkPostBatchSender_SendBatch_EmptyBatch(t *testing.T) {
	sender := NewSparkPostBatchSender("test-api-key", nil)

	result, err := sender.SendBatch(context.Background(), []EmailMessage{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Results) != 0 {
		t.Errorf("expected 0 results, got %d", len(result.Results))
	}
}

func TestSparkPostBatchSender_SendBatch_NoAPIKey(t *testing.T) {
	sender := NewSparkPostBatchSender("", nil)

	messages := []EmailMessage{{Email: "test@example.com"}}
	_, err := sender.SendBatch(context.Background(), messages)

	if err == nil {
		t.Error("expected error for missing API key")
	}
	if !strings.Contains(err.Error(), "API key not configured") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSparkPostBatchSender_SendBatch_ExceedsMaxBatch(t *testing.T) {
	sender := NewSparkPostBatchSenderWithConfig(SparkPostBatchConfig{
		APIKey:   "key",
		MaxBatch: 10,
	}, nil)

	// Create 15 messages (exceeds max of 10)
	messages := make([]EmailMessage, 15)
	for i := range messages {
		messages[i] = EmailMessage{Email: fmt.Sprintf("user%d@example.com", i)}
	}

	_, err := sender.SendBatch(context.Background(), messages)

	if err == nil {
		t.Error("expected error for exceeding max batch")
	}
	if !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSparkPostBatchSender_SendBatch_PayloadTooLarge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request should not have been made")
	}))
	defer server.Close()

	sender := NewSparkPostBatchSenderWithConfig(SparkPostBatchConfig{
		APIKey:       "key",
		BaseURL:      server.URL,
		MaxPayloadMB: 1, // 1MB limit
	}, nil)

	// Create message with large content
	largeContent := strings.Repeat("x", 2*1024*1024) // 2MB content
	messages := []EmailMessage{{
		Email:       "test@example.com",
		HTMLContent: largeContent,
	}}

	_, err := sender.SendBatch(context.Background(), messages)

	if err == nil {
		t.Error("expected error for payload too large")
	}
	if !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSparkPostBatchSender_SendBatch_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(SparkPostBatchResponse{
			Errors: []struct {
				Message     string `json:"message"`
				Code        string `json:"code"`
				Description string `json:"description"`
			}{
				{Code: "1001", Message: "Invalid recipient"},
			},
		})
	}))
	defer server.Close()

	sender := NewSparkPostBatchSenderWithConfig(SparkPostBatchConfig{
		APIKey:  "key",
		BaseURL: server.URL,
	}, nil)

	messages := []EmailMessage{{Email: "invalid"}}
	_, err := sender.SendBatch(context.Background(), messages)

	if err == nil {
		t.Error("expected error for API error response")
	}
	if !strings.Contains(err.Error(), "SparkPost API error") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSparkPostBatchSender_EstimatePayloadSize(t *testing.T) {
	sender := NewSparkPostBatchSender("key", nil)

	messages := []EmailMessage{
		{
			Email:       "user1@example.com",
			HTMLContent: strings.Repeat("x", 1000),
			Subject:     "Test Subject",
			FromEmail:   "sender@example.com",
		},
		{
			Email: "user2@example.com",
		},
	}

	size := sender.EstimatePayloadSize(messages)

	// Should include base content + recipient overhead
	if size < 1000 {
		t.Errorf("estimated size %d seems too small", size)
	}
}

func TestSparkPostBatchSender_EstimatePayloadSize_Empty(t *testing.T) {
	sender := NewSparkPostBatchSender("key", nil)

	size := sender.EstimatePayloadSize([]EmailMessage{})

	if size != 0 {
		t.Errorf("expected 0 for empty messages, got %d", size)
	}
}

func TestSparkPostBatchSender_ValidateBatch(t *testing.T) {
	sender := NewSparkPostBatchSenderWithConfig(SparkPostBatchConfig{
		APIKey:   "key",
		MaxBatch: 10,
	}, nil)

	tests := []struct {
		name          string
		messages      []EmailMessage
		expectError   bool
		errorContains string
	}{
		{
			name:          "empty batch",
			messages:      []EmailMessage{},
			expectError:   true,
			errorContains: "empty",
		},
		{
			name: "valid batch",
			messages: []EmailMessage{
				{ID: "1", Email: "test@example.com"},
			},
			expectError: false,
		},
		{
			name: "exceeds count",
			messages: func() []EmailMessage {
				msgs := make([]EmailMessage, 15)
				for i := range msgs {
					msgs[i] = EmailMessage{ID: fmt.Sprintf("%d", i), Email: fmt.Sprintf("user%d@example.com", i)}
				}
				return msgs
			}(),
			expectError:   true,
			errorContains: "exceeds maximum",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := sender.ValidateBatch(tt.messages)

			if tt.expectError && err == nil {
				t.Error("expected error but got nil")
			}
			if !tt.expectError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tt.expectError && err != nil && tt.errorContains != "" {
				if !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("error '%v' should contain '%s'", err, tt.errorContains)
				}
			}
		})
	}
}

func TestSparkPostBatchSender_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow response
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := NewSparkPostBatchSenderWithConfig(SparkPostBatchConfig{
		APIKey:  "key",
		BaseURL: server.URL,
		Timeout: 5 * time.Second,
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	messages := []EmailMessage{{Email: "test@example.com", HTMLContent: "<p>Test</p>"}}
	_, err := sender.SendBatch(ctx, messages)

	if err == nil {
		t.Error("expected error due to context cancellation")
	}
}

// =============================================================================
// BatchGrouper Tests
// =============================================================================

func TestNewBatchGrouper(t *testing.T) {
	grouper := NewBatchGrouper()

	if grouper.GetBatchSize("sparkpost") != SparkPostBatchSize {
		t.Errorf("expected sparkpost batch size %d, got %d", SparkPostBatchSize, grouper.GetBatchSize("sparkpost"))
	}
	if grouper.GetBatchSize("ses") != SESBatchSize {
		t.Errorf("expected ses batch size %d, got %d", SESBatchSize, grouper.GetBatchSize("ses"))
	}
	if grouper.GetBatchSize("unknown") != DefaultBatchSize {
		t.Errorf("expected default batch size %d for unknown ESP, got %d", DefaultBatchSize, grouper.GetBatchSize("unknown"))
	}
}

func TestBatchGrouper_GetPayloadLimit(t *testing.T) {
	grouper := NewBatchGrouper()

	if grouper.GetPayloadLimit("sparkpost") != SparkPostMaxPayloadBytes {
		t.Errorf("expected sparkpost payload limit %d, got %d", SparkPostMaxPayloadBytes, grouper.GetPayloadLimit("sparkpost"))
	}
}

func TestBatchGrouper_GroupIntoBatches_ByCount(t *testing.T) {
	grouper := NewBatchGrouper()
	// Use SES which has max 50 recipients for easier testing
	
	messages := make([]BatchQueueItem, 120)
	for i := range messages {
		messages[i] = BatchQueueItem{
			ID:    uuid.New(),
			Email: fmt.Sprintf("user%d@example.com", i),
		}
	}

	batches := grouper.GroupIntoBatches(messages, "ses")

	// 120 messages with max 50 = 3 batches (50, 50, 20)
	if len(batches) != 3 {
		t.Errorf("expected 3 batches, got %d", len(batches))
	}

	// Verify batch sizes
	expectedSizes := []int{50, 50, 20}
	for i, batch := range batches {
		if len(batch) != expectedSizes[i] {
			t.Errorf("batch %d: expected %d messages, got %d", i, expectedSizes[i], len(batch))
		}
	}
}

func TestBatchGrouper_GroupIntoBatches_SparkPost2000(t *testing.T) {
	grouper := NewBatchGrouper()

	messages := make([]BatchQueueItem, 5000)
	for i := range messages {
		messages[i] = BatchQueueItem{
			ID:    uuid.New(),
			Email: fmt.Sprintf("user%d@example.com", i),
		}
	}

	batches := grouper.GroupIntoBatches(messages, "sparkpost")

	// 5000 messages with max 2000 = 3 batches (2000, 2000, 1000)
	if len(batches) != 3 {
		t.Errorf("expected 3 batches, got %d", len(batches))
	}

	// Verify first two batches are at max capacity
	for i := 0; i < 2; i++ {
		if len(batches[i]) != SparkPostBatchSize {
			t.Errorf("batch %d: expected %d messages, got %d", i, SparkPostBatchSize, len(batches[i]))
		}
	}

	// Verify last batch has remaining
	if len(batches[2]) != 1000 {
		t.Errorf("batch 2: expected 1000 messages, got %d", len(batches[2]))
	}
}

func TestBatchGrouper_GroupIntoBatches_Empty(t *testing.T) {
	grouper := NewBatchGrouper()

	batches := grouper.GroupIntoBatches([]BatchQueueItem{}, "sparkpost")

	if batches != nil && len(batches) != 0 {
		t.Errorf("expected empty batches for empty input, got %d", len(batches))
	}
}

func TestBatchGrouper_GroupIntoBatches_SingleMessage(t *testing.T) {
	grouper := NewBatchGrouper()

	messages := []BatchQueueItem{{ID: uuid.New(), Email: "test@example.com"}}
	batches := grouper.GroupIntoBatches(messages, "sparkpost")

	if len(batches) != 1 {
		t.Errorf("expected 1 batch, got %d", len(batches))
	}
	if len(batches[0]) != 1 {
		t.Errorf("expected 1 message in batch, got %d", len(batches[0]))
	}
}

func TestBatchGrouper_GroupIntoBatches_WithSubstitutionData(t *testing.T) {
	grouper := NewBatchGrouper()

	messages := make([]BatchQueueItem, 10)
	for i := range messages {
		messages[i] = BatchQueueItem{
			ID:    uuid.New(),
			Email: fmt.Sprintf("user%d@example.com", i),
			SubstitutionData: map[string]interface{}{
				"first_name": strings.Repeat("x", 100),
				"last_name":  strings.Repeat("y", 100),
				"custom":     strings.Repeat("z", 100),
			},
		}
	}

	batches := grouper.GroupIntoBatches(messages, "ses")

	// Should account for substitution data size
	totalMessages := 0
	for _, batch := range batches {
		totalMessages += len(batch)
	}
	if totalMessages != 10 {
		t.Errorf("expected 10 total messages, got %d", totalMessages)
	}
}

func TestBatchGrouper_ValidateBatch(t *testing.T) {
	grouper := NewBatchGrouper()

	tests := []struct {
		name          string
		messages      []BatchQueueItem
		espType       string
		expectError   bool
		errorContains string
	}{
		{
			name:          "empty batch",
			messages:      []BatchQueueItem{},
			espType:       "sparkpost",
			expectError:   true,
			errorContains: "empty",
		},
		{
			name: "valid batch",
			messages: []BatchQueueItem{
				{ID: uuid.New(), Email: "test@example.com"},
			},
			espType:     "sparkpost",
			expectError: false,
		},
		{
			name: "exceeds count for ses",
			messages: func() []BatchQueueItem {
				msgs := make([]BatchQueueItem, 60) // SES max is 50
				for i := range msgs {
					msgs[i] = BatchQueueItem{ID: uuid.New(), Email: fmt.Sprintf("user%d@example.com", i)}
				}
				return msgs
			}(),
			espType:       "ses",
			expectError:   true,
			errorContains: "exceeds maximum",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := grouper.ValidateBatch(tt.messages, tt.espType)

			if tt.expectError && err == nil {
				t.Error("expected error but got nil")
			}
			if !tt.expectError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tt.expectError && err != nil && tt.errorContains != "" {
				if !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("error '%v' should contain '%s'", err, tt.errorContains)
				}
			}
		})
	}
}

func TestBatchGrouper_GroupIntoBatchesSimple(t *testing.T) {
	grouper := NewBatchGrouper()

	messages := make([]BatchQueueItem, 10)
	for i := range messages {
		messages[i] = BatchQueueItem{ID: uuid.New(), Email: fmt.Sprintf("user%d@example.com", i)}
	}

	// Use simple batching (legacy behavior)
	batches := grouper.GroupIntoBatchesSimple(messages, "ses")

	// 10 messages with max 50 = 1 batch
	if len(batches) != 1 {
		t.Errorf("expected 1 batch, got %d", len(batches))
	}
}

// =============================================================================
// Integration Tests
// =============================================================================

func TestBatchGrouper_Integration_LargeBatch(t *testing.T) {
	// Simulate real-world scenario with 5000 recipients for SparkPost
	grouper := NewBatchGrouper()

	messages := make([]BatchQueueItem, 5000)
	for i := range messages {
		messages[i] = BatchQueueItem{
			ID:           uuid.New(),
			CampaignID:   uuid.New(),
			SubscriberID: uuid.New(),
			Email:        fmt.Sprintf("user%d@example.com", i),
			SubstitutionData: map[string]interface{}{
				"name":            fmt.Sprintf("User %d", i),
				"company":         "Test Corp",
				"unsubscribe_url": fmt.Sprintf("https://example.com/unsubscribe/%d", i),
			},
		}
	}

	batches := grouper.GroupIntoBatches(messages, "sparkpost")

	// Should create batches respecting 2000 recipient limit
	if len(batches) < 3 {
		t.Errorf("expected at least 3 batches for 5000 messages, got %d", len(batches))
	}

	// Verify no messages lost
	totalMessages := 0
	for i, batch := range batches {
		totalMessages += len(batch)
		t.Logf("Batch %d: %d messages", i, len(batch))

		// Verify no batch exceeds limit
		if len(batch) > SparkPostBatchSize {
			t.Errorf("batch %d has %d messages, exceeds max %d",
				i, len(batch), SparkPostBatchSize)
		}
	}

	if totalMessages != 5000 {
		t.Errorf("expected 5000 total messages, got %d", totalMessages)
	}
}

func TestSparkPostBatchSender_MaxBatchSize_WithConfig(t *testing.T) {
	sender := NewSparkPostBatchSender("key", nil)

	if sender.MaxBatchSize() != SparkPostBatchSize {
		t.Errorf("expected MaxBatchSize %d, got %d", SparkPostBatchSize, sender.MaxBatchSize())
	}

	// Test with custom config
	customSender := NewSparkPostBatchSenderWithConfig(SparkPostBatchConfig{
		APIKey:   "key",
		MaxBatch: 500,
	}, nil)
	if customSender.MaxBatchSize() != 500 {
		t.Errorf("expected custom MaxBatchSize 500, got %d", customSender.MaxBatchSize())
	}
}

func TestSparkPostBatchSender_MaxPayloadSize_Calculation(t *testing.T) {
	sender := NewSparkPostBatchSender("key", nil)

	expectedSize := SparkPostMaxPayloadMB * 1024 * 1024
	if sender.MaxPayloadSize() != expectedSize {
		t.Errorf("expected MaxPayloadSize %d, got %d", expectedSize, sender.MaxPayloadSize())
	}

	// Test with custom config
	customSender := NewSparkPostBatchSenderWithConfig(SparkPostBatchConfig{
		APIKey:       "key",
		MaxPayloadMB: 3,
	}, nil)
	if customSender.MaxPayloadSize() != 3*1024*1024 {
		t.Errorf("expected custom MaxPayloadSize %d, got %d", 3*1024*1024, customSender.MaxPayloadSize())
	}
}

// =============================================================================
// Benchmark Tests
// =============================================================================

func BenchmarkBatchGrouper_GroupIntoBatches_LargeSet(b *testing.B) {
	grouper := NewBatchGrouper()

	messages := make([]BatchQueueItem, 10000)
	for i := range messages {
		messages[i] = BatchQueueItem{
			ID:           uuid.New(),
			CampaignID:   uuid.New(),
			SubscriberID: uuid.New(),
			Email:        fmt.Sprintf("user%d@example.com", i),
			SubstitutionData: map[string]interface{}{
				"name": fmt.Sprintf("User %d", i),
			},
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = grouper.GroupIntoBatches(messages, "sparkpost")
	}
}

func BenchmarkSparkPostBatchSender_EstimatePayloadSize(b *testing.B) {
	sender := NewSparkPostBatchSender("key", nil)

	messages := make([]EmailMessage, 2000)
	for i := range messages {
		messages[i] = EmailMessage{
			ID:          fmt.Sprintf("msg-%d", i),
			Email:       fmt.Sprintf("user%d@example.com", i),
			HTMLContent: strings.Repeat("x", 1000),
			Metadata:    map[string]interface{}{"name": "Test", "company": "Test Corp"},
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = sender.EstimatePayloadSize(messages)
	}
}

func BenchmarkBatchGrouper_ValidateBatch(b *testing.B) {
	grouper := NewBatchGrouper()

	messages := make([]BatchQueueItem, 2000)
	for i := range messages {
		messages[i] = BatchQueueItem{
			ID:    uuid.New(),
			Email: fmt.Sprintf("user%d@example.com", i),
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = grouper.ValidateBatch(messages, "sparkpost")
	}
}
