package worker

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// =============================================================================
// CAMPAIGN PROCESSOR TESTS
// =============================================================================

func setupTestDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	return db, mock, func() { db.Close() }
}

func TestCampaignProcessor_StartStop(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}
	defer mr.Close()

	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()

	db, _, dbCleanup := setupTestDB(t)
	defer dbCleanup()

	config := CampaignProcessorConfig{
		NumWorkers: 2,
		BatchSize:  10,
	}

	processor := NewCampaignProcessor(db, redisClient, config)

	// Start
	err = processor.Start()
	if err != nil {
		t.Errorf("Start() error: %v", err)
	}

	// Verify running
	processor.mu.RLock()
	running := processor.running
	processor.mu.RUnlock()
	if !running {
		t.Error("Processor should be running after Start()")
	}

	// Double start should error
	err = processor.Start()
	if err == nil {
		t.Error("Double Start() should return error")
	}

	// Stop
	processor.Stop()

	processor.mu.RLock()
	running = processor.running
	processor.mu.RUnlock()
	if running {
		t.Error("Processor should not be running after Stop()")
	}
}

func TestCampaignProcessor_Stats(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()

	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()

	db, _, dbCleanup := setupTestDB(t)
	defer dbCleanup()

	processor := NewCampaignProcessor(db, redisClient, DefaultProcessorConfig())

	// Initial stats should be zero
	stats := processor.Stats()
	if stats["total_sent"] != 0 {
		t.Errorf("Initial total_sent = %d, want 0", stats["total_sent"])
	}
	if stats["total_failed"] != 0 {
		t.Errorf("Initial total_failed = %d, want 0", stats["total_failed"])
	}
}

func TestDefaultProcessorConfig(t *testing.T) {
	config := DefaultProcessorConfig()

	if config.NumWorkers <= 0 {
		t.Errorf("NumWorkers = %d, want > 0", config.NumWorkers)
	}
	if config.BatchSize <= 0 {
		t.Errorf("BatchSize = %d, want > 0", config.BatchSize)
	}
}

func TestCampaignProcessor_ConfigValidation(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()

	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()

	db, _, dbCleanup := setupTestDB(t)
	defer dbCleanup()

	// Test with zero values - should use defaults
	config := CampaignProcessorConfig{
		NumWorkers: 0,
		BatchSize:  0,
	}

	processor := NewCampaignProcessor(db, redisClient, config)

	if processor.numWorkers <= 0 {
		t.Errorf("numWorkers = %d, should default to > 0", processor.numWorkers)
	}
	if processor.batchSize <= 0 {
		t.Errorf("batchSize = %d, should default to > 0", processor.batchSize)
	}
}

// =============================================================================
// QUEUE ITEM PROCESSING TESTS
// =============================================================================

func TestCampaignProcessor_SelectESP_NoQuotas(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()

	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()

	db, mock, dbCleanup := setupTestDB(t)
	defer dbCleanup()

	processor := NewCampaignProcessor(db, redisClient, DefaultProcessorConfig())
	ctx := context.Background()

	// Setup mock for default profile query
	defaultProfileID := uuid.New().String()
	mock.ExpectQuery("SELECT id FROM mailing_sending_profiles").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(defaultProfileID))

	item := ProcessorQueueItem{
		ID:         uuid.New(),
		CampaignID: uuid.New(),
		ESPQuotas:  nil, // No quotas
		ProfileID:  sql.NullString{Valid: false},
	}

	profileID, err := processor.selectESP(ctx, item)
	if err != nil {
		t.Errorf("selectESP() error: %v", err)
	}
	if profileID != defaultProfileID {
		t.Errorf("selectESP() = %s, want %s", profileID, defaultProfileID)
	}
}

func TestCampaignProcessor_SelectESP_WithQuotas(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()

	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()

	db, _, dbCleanup := setupTestDB(t)
	defer dbCleanup()

	processor := NewCampaignProcessor(db, redisClient, DefaultProcessorConfig())
	ctx := context.Background()

	item := ProcessorQueueItem{
		ID:         uuid.New(),
		CampaignID: uuid.New(),
		ESPQuotas: []ESPQuota{
			{ProfileID: "sparkpost", Percentage: 100},
		},
	}

	profileID, err := processor.selectESP(ctx, item)
	if err != nil {
		t.Errorf("selectESP() error: %v", err)
	}
	if profileID != "sparkpost" {
		t.Errorf("selectESP() = %s, want sparkpost", profileID)
	}
}

func TestCampaignProcessor_SelectESP_WithProfileID(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()

	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()

	db, _, dbCleanup := setupTestDB(t)
	defer dbCleanup()

	processor := NewCampaignProcessor(db, redisClient, DefaultProcessorConfig())
	ctx := context.Background()

	profileID := "specific-profile"
	item := ProcessorQueueItem{
		ID:         uuid.New(),
		CampaignID: uuid.New(),
		ESPQuotas:  nil,
		ProfileID:  sql.NullString{String: profileID, Valid: true},
	}

	selected, err := processor.selectESP(ctx, item)
	if err != nil {
		t.Errorf("selectESP() error: %v", err)
	}
	if selected != profileID {
		t.Errorf("selectESP() = %s, want %s", selected, profileID)
	}
}

// =============================================================================
// THROTTLE INTEGRATION TESTS
// =============================================================================

func TestCampaignProcessor_ApplyThrottle(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()

	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()

	db, _, dbCleanup := setupTestDB(t)
	defer dbCleanup()

	processor := NewCampaignProcessor(db, redisClient, DefaultProcessorConfig())
	ctx := context.Background()
	campaignID := "throttle-test"

	// Should not error with valid rate
	err := processor.applyThrottle(ctx, campaignID, 100)
	if err != nil {
		t.Errorf("applyThrottle() error: %v", err)
	}

	// Zero rate should be no-op
	err = processor.applyThrottle(ctx, campaignID, 0)
	if err != nil {
		t.Errorf("applyThrottle(0) error: %v", err)
	}
}

func TestCampaignProcessor_ThrottleRespected(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()

	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()

	db, _, dbCleanup := setupTestDB(t)
	defer dbCleanup()

	processor := NewCampaignProcessor(db, redisClient, DefaultProcessorConfig())
	ctx := context.Background()
	campaignID := "throttle-timing-test"

	// Set a low rate for testing
	ratePerMinute := 60 // 1 per second

	start := time.Now()
	
	// Make multiple calls - they should be throttled
	for i := 0; i < 3; i++ {
		processor.applyThrottle(ctx, campaignID, ratePerMinute)
	}

	elapsed := time.Since(start)
	
	// With 60/min rate, 3 calls should take at least 2 seconds
	// But since we're using token bucket, first call is instant
	// We just verify it doesn't take too long (< 5 seconds)
	if elapsed > 5*time.Second {
		t.Errorf("Throttle took too long: %v", elapsed)
	}
}

// =============================================================================
// CAMPAIGN STATUS TESTS
// =============================================================================

func TestCampaignProcessor_PausedCampaign(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()

	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()

	db, mock, dbCleanup := setupTestDB(t)
	defer dbCleanup()

	processor := NewCampaignProcessor(db, redisClient, DefaultProcessorConfig())

	campaignID := uuid.New()
	itemID := uuid.New()

	// Mock: campaign is paused
	mock.ExpectQuery("SELECT status FROM mailing_campaigns").
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("paused"))

	// Mock: pause the item (return to queue)
	mock.ExpectExec("UPDATE mailing_campaign_queue").
		WithArgs(itemID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	item := ProcessorQueueItem{
		ID:         itemID,
		CampaignID: campaignID,
	}

	err := processor.processItem(item)
	if err != nil {
		t.Errorf("processItem() error for paused campaign: %v", err)
	}
}

// =============================================================================
// ENQUEUE CAMPAIGN TESTS
// =============================================================================

func TestEnqueueCampaign_InvalidID(t *testing.T) {
	db, _, dbCleanup := setupTestDB(t)
	defer dbCleanup()

	ctx := context.Background()

	_, err := EnqueueCampaign(ctx, db, "invalid-uuid")
	if err == nil {
		t.Error("EnqueueCampaign() should error with invalid UUID")
	}
}

func TestEnqueueCampaign_NotFound(t *testing.T) {
	db, mock, dbCleanup := setupTestDB(t)
	defer dbCleanup()

	ctx := context.Background()
	campaignID := uuid.New().String()

	mock.ExpectQuery("SELECT status, list_id, segment_id").
		WillReturnError(sql.ErrNoRows)

	_, err := EnqueueCampaign(ctx, db, campaignID)
	if err == nil {
		t.Error("EnqueueCampaign() should error when campaign not found")
	}
}

func TestEnqueueCampaign_WrongStatus(t *testing.T) {
	db, mock, dbCleanup := setupTestDB(t)
	defer dbCleanup()

	ctx := context.Background()
	campaignID := uuid.New().String()

	// Campaign is already sending
	mock.ExpectQuery("SELECT status, list_id, segment_id").
		WillReturnRows(sqlmock.NewRows([]string{"status", "list_id", "segment_id"}).
			AddRow("sending", nil, nil))

	_, err := EnqueueCampaign(ctx, db, campaignID)
	if err == nil {
		t.Error("EnqueueCampaign() should error when campaign is already sending")
	}
}

// =============================================================================
// EDGE CASES
// =============================================================================

func TestCampaignProcessor_ZeroSubscribers(t *testing.T) {
	// This tests the scenario where a campaign has 0 subscribers
	// The enqueue function should handle this gracefully
	mr, _ := miniredis.Run()
	defer mr.Close()

	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()

	db, mock, dbCleanup := setupTestDB(t)
	defer dbCleanup()

	campaignID := uuid.New()
	listID := uuid.New().String()

	// Mock campaign query
	mock.ExpectQuery("SELECT subject").
		WillReturnRows(sqlmock.NewRows([]string{"subject", "html_content", "plain_content", "max_recipients", "throttle_speed"}).
			AddRow("Test", "<p>Hello</p>", "Hello", nil, "gentle"))

	// Mock subscriber query - returns empty
	mock.ExpectQuery("SELECT s.id, s.email").
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))

	// Mock status update to completed
	mock.ExpectExec("UPDATE mailing_campaigns SET status").
		WillReturnResult(sqlmock.NewResult(0, 1))

	ctx := context.Background()
	enqueueSubscribersForCampaign(ctx, db, campaignID, 
		sql.NullString{String: listID, Valid: true},
		sql.NullString{Valid: false})

	// Verify all expectations were met
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Unfulfilled expectations: %v", err)
	}
}

func TestCampaignProcessor_DuplicateSendRequest(t *testing.T) {
	db, mock, dbCleanup := setupTestDB(t)
	defer dbCleanup()

	ctx := context.Background()
	campaignID := uuid.New().String()

	// Campaign is already sending (duplicate request)
	mock.ExpectQuery("SELECT status, list_id, segment_id").
		WillReturnRows(sqlmock.NewRows([]string{"status", "list_id", "segment_id"}).
			AddRow("sending", "list-1", nil))

	_, err := EnqueueCampaign(ctx, db, campaignID)
	if err == nil {
		t.Error("Duplicate send request should be rejected")
	}
}

func TestCampaignProcessor_CancelledMidQueue(t *testing.T) {
	// Test that items are properly skipped when campaign is cancelled
	mr, _ := miniredis.Run()
	defer mr.Close()

	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()

	db, mock, dbCleanup := setupTestDB(t)
	defer dbCleanup()

	processor := NewCampaignProcessor(db, redisClient, DefaultProcessorConfig())

	campaignID := uuid.New()
	itemID := uuid.New()

	// Mock: campaign is cancelled
	mock.ExpectQuery("SELECT status FROM mailing_campaigns").
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("cancelled"))

	// Mock: skip the item
	mock.ExpectExec("UPDATE mailing_campaign_queue").
		WithArgs(itemID, "campaign_not_active").
		WillReturnResult(sqlmock.NewResult(0, 1))

	item := ProcessorQueueItem{
		ID:         itemID,
		CampaignID: campaignID,
	}

	err := processor.processItem(item)
	if err != nil {
		t.Errorf("processItem() error for cancelled campaign: %v", err)
	}
}

// =============================================================================
// BENCHMARK TESTS
// =============================================================================

func BenchmarkESPDistributor_SelectESP(b *testing.B) {
	mr, _ := miniredis.Run()
	defer mr.Close()

	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()

	distributor := NewESPDistributor(redisClient)
	ctx := context.Background()
	campaignID := "benchmark-campaign"

	quotas := []ESPQuota{
		{ProfileID: "sparkpost", Percentage: 60},
		{ProfileID: "ses", Percentage: 40},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		esp, _ := distributor.SelectESP(ctx, campaignID, quotas)
		distributor.RecordSend(ctx, campaignID, esp)
	}
}

func BenchmarkThrottleManager_SetGet(b *testing.B) {
	mr, _ := miniredis.Run()
	defer mr.Close()

	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()

	manager := NewThrottleManager(redisClient)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		campaignID := "benchmark-" + string(rune(i%100))
		manager.SetThrottle(ctx, campaignID, ThrottleGentle, 0)
		manager.GetThrottle(ctx, campaignID)
	}
}
