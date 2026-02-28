package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
	"github.com/redis/go-redis/v9"
)

// =============================================================================
// CAMPAIGN PROCESSOR - Background Queue Processing with Multi-ESP Support
// =============================================================================
// Processes campaigns from the queue with:
// - Multi-ESP distribution based on quotas
// - Real-time throttle control
// - Pause/resume support
// - Progress tracking
// - Failover handling

// CampaignProcessor handles background campaign sending
type CampaignProcessor struct {
	db          *sql.DB
	redis       *redis.Client
	distributor *ESPDistributor
	throttle    *ThrottleManager
	rateLimiter *RateLimiter
	sender      *ProfileBasedSender

	// Worker configuration
	workerID   string
	numWorkers int
	batchSize  int

	// Stats
	totalSent   int64
	totalFailed int64
	totalSkipped int64

	// Control
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.RWMutex
	running bool
}

// CampaignProcessorConfig holds processor configuration
type CampaignProcessorConfig struct {
	NumWorkers int
	BatchSize  int
}

// DefaultProcessorConfig returns default configuration
func DefaultProcessorConfig() CampaignProcessorConfig {
	return CampaignProcessorConfig{
		NumWorkers: 10,
		BatchSize:  50,
	}
}

// NewCampaignProcessor creates a new campaign processor
func NewCampaignProcessor(db *sql.DB, redisClient *redis.Client, config CampaignProcessorConfig) *CampaignProcessor {
	if config.NumWorkers <= 0 {
		config.NumWorkers = 10
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 50
	}

	return &CampaignProcessor{
		db:          db,
		redis:       redisClient,
		distributor: NewESPDistributor(redisClient),
		throttle:    NewThrottleManager(redisClient),
		rateLimiter: NewRateLimiter(redisClient),
		sender:      NewProfileBasedSender(db),
		workerID:    fmt.Sprintf("processor-%s", uuid.New().String()[:8]),
		numWorkers:  config.NumWorkers,
		batchSize:   config.BatchSize,
	}
}

// Start begins the campaign processor workers
func (p *CampaignProcessor) Start() error {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return fmt.Errorf("processor already running")
	}
	p.running = true
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.mu.Unlock()

	log.Printf("[CampaignProcessor] Starting %d workers (batch_size=%d)", p.numWorkers, p.batchSize)

	// Start workers
	for i := 0; i < p.numWorkers; i++ {
		p.wg.Add(1)
		go p.worker(i)
	}

	return nil
}

// Stop gracefully stops the processor
func (p *CampaignProcessor) Stop() {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return
	}
	p.running = false
	p.cancel()
	p.mu.Unlock()

	log.Println("[CampaignProcessor] Stopping workers...")
	p.wg.Wait()

	log.Printf("[CampaignProcessor] Stopped. Total sent: %d, failed: %d, skipped: %d",
		atomic.LoadInt64(&p.totalSent),
		atomic.LoadInt64(&p.totalFailed),
		atomic.LoadInt64(&p.totalSkipped))
}

// Stats returns current processing statistics
func (p *CampaignProcessor) Stats() map[string]int64 {
	return map[string]int64{
		"total_sent":    atomic.LoadInt64(&p.totalSent),
		"total_failed":  atomic.LoadInt64(&p.totalFailed),
		"total_skipped": atomic.LoadInt64(&p.totalSkipped),
	}
}

// worker is the main processing loop for a single worker
func (p *CampaignProcessor) worker(workerNum int) {
	defer p.wg.Done()

	for {
		select {
		case <-p.ctx.Done():
			return
		default:
			items, err := p.claimBatch()
			if err != nil {
				log.Printf("[Worker %d] Error claiming batch: %v", workerNum, err)
				time.Sleep(time.Second)
				continue
			}

			if len(items) == 0 {
				time.Sleep(100 * time.Millisecond)
				continue
			}

			// Process items
			for _, item := range items {
				if err := p.processItem(item); err != nil {
					log.Printf("[Worker %d] Error processing item %s: %v", workerNum, item.ID, err)
				}
			}
		}
	}
}

// ProcessorQueueItem represents an item from the campaign queue for background processing
type ProcessorQueueItem struct {
	ID              uuid.UUID
	CampaignID      uuid.UUID
	SubscriberID    uuid.UUID
	Email           string
	Subject         string
	HTMLContent     string
	PlainContent    string
	ProfileID       sql.NullString
	ESPQuotas       []ESPQuota
	SubstitutionData map[string]interface{}
	Priority        int
}

// claimBatch claims a batch of queue items for processing
func (p *CampaignProcessor) claimBatch() ([]ProcessorQueueItem, error) {
	ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
	defer cancel()

	// Claim items using SELECT FOR UPDATE SKIP LOCKED for concurrency safety
	rows, err := p.db.QueryContext(ctx, `
		WITH claimed AS (
			UPDATE mailing_campaign_queue
			SET 
				status = 'claimed',
				worker_id = $1,
				claimed_at = NOW()
			WHERE id IN (
				SELECT q.id 
				FROM mailing_campaign_queue q
				JOIN mailing_campaigns c ON c.id = q.campaign_id
				WHERE q.status = 'queued'
				  AND q.scheduled_at <= NOW()
				  AND c.status = 'sending'
				ORDER BY q.priority DESC, q.scheduled_at ASC
				LIMIT $2
				FOR UPDATE OF q SKIP LOCKED
			)
			RETURNING id, campaign_id, subscriber_id, subject, 
					  COALESCE(html_content, ''), COALESCE(plain_content, ''),
					  priority
		)
		SELECT c.id, c.campaign_id, c.subscriber_id, c.subject, 
			   c.html_content, c.plain_content, c.priority,
			   s.email, camp.sending_profile_id, COALESCE(camp.esp_quotas::text, '[]')
		FROM claimed c
		JOIN mailing_subscribers s ON s.id = c.subscriber_id
		JOIN mailing_campaigns camp ON camp.id = c.campaign_id
	`, p.workerID, p.batchSize)

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []ProcessorQueueItem
	for rows.Next() {
		var item ProcessorQueueItem
		var profileID sql.NullString
		var espQuotasJSON string
		
		err := rows.Scan(
			&item.ID,
			&item.CampaignID,
			&item.SubscriberID,
			&item.Subject,
			&item.HTMLContent,
			&item.PlainContent,
			&item.Priority,
			&item.Email,
			&profileID,
			&espQuotasJSON,
		)
		if err != nil {
			continue
		}

		item.ProfileID = profileID
		
		// Parse ESP quotas
		if espQuotasJSON != "" && espQuotasJSON != "[]" {
			json.Unmarshal([]byte(espQuotasJSON), &item.ESPQuotas)
		}

		items = append(items, item)
	}

	return items, nil
}

// processItem processes a single queue item
func (p *CampaignProcessor) processItem(item ProcessorQueueItem) error {
	// Use parent context if set, otherwise use background context (for testing)
	parentCtx := p.ctx
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()

	// Check if campaign is still sending (not paused/cancelled)
	var campaignStatus string
	err := p.db.QueryRowContext(ctx, `
		SELECT status FROM mailing_campaigns WHERE id = $1
	`, item.CampaignID).Scan(&campaignStatus)
	
	if err != nil || campaignStatus != "sending" {
		// Campaign no longer active, return item to queue or skip
		if campaignStatus == "paused" {
			return p.pauseItem(ctx, item.ID)
		}
		return p.skipItem(ctx, item.ID, "campaign_not_active")
	}

	// Get current throttle rate
	throttleConfig, err := p.throttle.GetThrottle(ctx, item.CampaignID.String())
	if err != nil {
		throttleConfig = &ThrottleConfig{Rate: ThrottleGentle, RatePerMinute: 100}
	}

	// Apply throttling
	if err := p.applyThrottle(ctx, item.CampaignID.String(), throttleConfig.RatePerMinute); err != nil {
		// Return to queue on throttle error
		return p.returnToQueue(ctx, item.ID)
	}

	// Select ESP based on quotas or use default profile
	profileID, err := p.selectESP(ctx, item)
	if err != nil {
		atomic.AddInt64(&p.totalFailed, 1)
		return p.markFailed(ctx, item.ID, item.CampaignID, err.Error())
	}

	// Check ESP-level rate limits
	espType := p.getESPType(ctx, profileID)
	allowed, waitTime, err := p.rateLimiter.CheckAndIncrement(ctx, espType, 1)
	if err != nil {
		log.Printf("[CampaignProcessor] Rate limit check error: %v", err)
	}
	if !allowed {
		if waitTime > 0 {
			time.Sleep(waitTime)
		}
		return p.returnToQueue(ctx, item.ID)
	}

	// Build and send email
	msg := &EmailMessage{
		ID:           item.ID.String(),
		CampaignID:   item.CampaignID.String(),
		SubscriberID: item.SubscriberID.String(),
		Email:        item.Email,
		Subject:      item.Subject,
		HTMLContent:  item.HTMLContent,
		TextContent:  item.PlainContent,
		ProfileID:    profileID,
	}

	// Get from details from campaign
	p.db.QueryRowContext(ctx, `
		SELECT from_name, from_email, COALESCE(reply_to, '')
		FROM mailing_campaigns WHERE id = $1
	`, item.CampaignID).Scan(&msg.FromName, &msg.FromEmail, &msg.ReplyTo)

	// Send
	result, err := p.sender.Send(ctx, msg)
	if err != nil {
		p.distributor.RecordFailure(ctx, item.CampaignID.String(), profileID)
		atomic.AddInt64(&p.totalFailed, 1)
		return p.markFailed(ctx, item.ID, item.CampaignID, err.Error())
	}

	if !result.Success {
		p.distributor.RecordFailure(ctx, item.CampaignID.String(), profileID)
		atomic.AddInt64(&p.totalFailed, 1)
		errMsg := "send failed"
		if result.Error != nil {
			errMsg = result.Error.Error()
		}
		return p.markFailed(ctx, item.ID, item.CampaignID, errMsg)
	}

	// Success
	p.distributor.RecordSend(ctx, item.CampaignID.String(), profileID)
	p.distributor.RecordSuccess(ctx, profileID)
	atomic.AddInt64(&p.totalSent, 1)
	
	return p.markSent(ctx, item.ID, item.CampaignID, result.MessageID)
}

// selectESP chooses which ESP to use based on quotas
func (p *CampaignProcessor) selectESP(ctx context.Context, item ProcessorQueueItem) (string, error) {
	// If ESP quotas are configured, use the distributor
	if len(item.ESPQuotas) > 0 {
		return p.distributor.SelectESP(ctx, item.CampaignID.String(), item.ESPQuotas)
	}

	// Fall back to campaign's sending profile
	if item.ProfileID.Valid && item.ProfileID.String != "" {
		return item.ProfileID.String, nil
	}

	// Fall back to default profile
	var defaultProfileID string
	err := p.db.QueryRowContext(ctx, `
		SELECT id FROM mailing_sending_profiles
		WHERE is_default = true AND status = 'active'
		LIMIT 1
	`).Scan(&defaultProfileID)
	
	if err != nil {
		return "", fmt.Errorf("no sending profile available")
	}

	return defaultProfileID, nil
}

// applyThrottle applies throttle delay based on rate per minute
func (p *CampaignProcessor) applyThrottle(ctx context.Context, campaignID string, ratePerMinute int) error {
	if ratePerMinute <= 0 {
		return nil
	}

	// Use Redis for distributed rate limiting
	key := fmt.Sprintf("throttle:campaign:%s", campaignID)
	
	// Simple token bucket implementation
	script := `
		local key = KEYS[1]
		local rate = tonumber(ARGV[1])
		local now = tonumber(ARGV[2])
		
		local bucket = redis.call('get', key)
		if not bucket then
			bucket = '{"tokens":' .. rate .. ',"last":' .. now .. '}'
		end
		
		local data = cjson.decode(bucket)
		local elapsed = now - data.last
		local tokens = math.min(rate, data.tokens + elapsed * (rate / 60))
		
		if tokens >= 1 then
			tokens = tokens - 1
			redis.call('setex', key, 120, cjson.encode({tokens=tokens, last=now}))
			return 1
		else
			return 0
		end
	`

	result, err := p.redis.Eval(ctx, script, []string{key}, ratePerMinute, time.Now().Unix()).Int()
	if err != nil {
		return err
	}

	if result == 0 {
		// Wait before retry
		waitTime := time.Duration(60000/ratePerMinute) * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitTime):
			return nil
		}
	}

	return nil
}

// getESPType returns the vendor type for a profile
func (p *CampaignProcessor) getESPType(ctx context.Context, profileID string) string {
	var vendorType string
	p.db.QueryRowContext(ctx, `
		SELECT COALESCE(vendor_type, 'ses') FROM mailing_sending_profiles WHERE id = $1
	`, profileID).Scan(&vendorType)
	return vendorType
}

// markSent marks a queue item as sent and updates campaign stats
func (p *CampaignProcessor) markSent(ctx context.Context, itemID, campaignID uuid.UUID, messageID string) error {
	_, err := p.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue
		SET status = 'sent', sent_at = NOW(), message_id = $2
		WHERE id = $1
	`, itemID, messageID)
	
	if err == nil {
		// Update campaign sent count
		p.db.ExecContext(ctx, `
			UPDATE mailing_campaigns 
			SET sent_count = sent_count + 1, updated_at = NOW()
			WHERE id = $1
		`, campaignID)
	}
	
	return err
}

// markFailed marks a queue item as failed, or as dead_letter if max retries exceeded
func (p *CampaignProcessor) markFailed(ctx context.Context, itemID, campaignID uuid.UUID, errorMsg string) error {
	// Truncate error message
	if len(errorMsg) > 255 {
		errorMsg = errorMsg[:255]
	}

	// Check current retry count to decide between 'failed' and 'dead_letter'
	var retryCount int
	_ = p.db.QueryRowContext(ctx, `
		SELECT COALESCE(retry_count, 0) FROM mailing_campaign_queue WHERE id = $1
	`, itemID).Scan(&retryCount)

	// If we've hit the max retry limit, move to dead_letter instead of failed
	if retryCount+1 >= MaxRetryCount {
		_, err := p.db.ExecContext(ctx, `
			UPDATE mailing_campaign_queue
			SET status = 'dead_letter', error_code = $2, retry_count = retry_count + 1
			WHERE id = $1
		`, itemID, errorMsg)
		if err == nil {
			log.Printf("[CampaignProcessor] Item %s moved to dead_letter after %d retries", itemID, retryCount+1)
		}
		return err
	}

	_, err := p.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue
		SET status = 'failed', error_code = $2, retry_count = retry_count + 1
		WHERE id = $1
	`, itemID, errorMsg)
	return err
}

// skipItem marks an item as skipped
func (p *CampaignProcessor) skipItem(ctx context.Context, itemID uuid.UUID, reason string) error {
	atomic.AddInt64(&p.totalSkipped, 1)
	_, err := p.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue
		SET status = 'skipped', error_code = $2
		WHERE id = $1
	`, itemID, reason)
	return err
}

// pauseItem pauses an item (when campaign is paused)
func (p *CampaignProcessor) pauseItem(ctx context.Context, itemID uuid.UUID) error {
	_, err := p.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue
		SET status = 'queued', worker_id = NULL, claimed_at = NULL
		WHERE id = $1
	`, itemID)
	return err
}

// returnToQueue returns an item to the queue for retry
func (p *CampaignProcessor) returnToQueue(ctx context.Context, itemID uuid.UUID) error {
	_, err := p.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue
		SET status = 'queued', worker_id = NULL, claimed_at = NULL
		WHERE id = $1
	`, itemID)
	return err
}

// =============================================================================
// CAMPAIGN QUEUE API
// =============================================================================

// packageBackpressure is the package-level backpressure monitor shared by
// standalone enqueue functions (EnqueueCampaign). Set via SetPackageBackpressure.
var packageBackpressure *BackpressureMonitor

// SetPackageBackpressure sets the backpressure monitor used by the package-level
// EnqueueCampaign function. Call this once during startup.
func SetPackageBackpressure(bp *BackpressureMonitor) {
	packageBackpressure = bp
}

// EnqueueCampaign adds a campaign to the background queue for async processing
// Returns immediately with a job ID
func EnqueueCampaign(ctx context.Context, db *sql.DB, campaignID string) (string, error) {
	// Check backpressure — refuse to enqueue if queue is overloaded.
	// The caller should retry later; the campaign stays in its current state.
	if packageBackpressure != nil && packageBackpressure.IsPaused() {
		return "", fmt.Errorf("backpressure active: queue depth exceeds threshold, try again later")
	}

	campUUID, err := uuid.Parse(campaignID)
	if err != nil {
		return "", fmt.Errorf("invalid campaign ID: %w", err)
	}

	// Acquire distributed lock to prevent duplicate sends across workers.
	// Uses PG advisory lock (Redis not available at this call site).
	lock := distlock.NewLock(nil, db, fmt.Sprintf("campaign:%s", campaignID), 10*time.Minute)
	acquired, lockErr := lock.Acquire(ctx)
	if lockErr != nil {
		log.Printf("[EnqueueCampaign] Warning: failed to acquire lock for campaign %s: %v", campaignID, lockErr)
		// Continue without lock — SQL status guard below is a secondary safety net
	} else if !acquired {
		return "", fmt.Errorf("campaign %s is already being processed by another worker", campaignID)
	} else {
		// Release after the synchronous part; the goroutine manages its own scope.
		defer lock.Release(ctx)
	}

	// Check campaign exists and is in valid state
	var status string
	var listID, segmentID sql.NullString
	err = db.QueryRowContext(ctx, `
		SELECT status, list_id, segment_id FROM mailing_campaigns WHERE id = $1
	`, campUUID).Scan(&status, &listID, &segmentID)
	
	if err != nil {
		return "", fmt.Errorf("campaign not found")
	}
	
	if status != "draft" && status != "scheduled" {
		return "", fmt.Errorf("cannot send campaign in %s status", status)
	}

	// Update status to sending
	_, err = db.ExecContext(ctx, `
		UPDATE mailing_campaigns 
		SET status = 'sending', started_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status IN ('draft', 'scheduled')
	`, campUUID)
	
	if err != nil {
		return "", fmt.Errorf("failed to start campaign: %w", err)
	}

	// Generate job ID
	jobID := uuid.New().String()

	// Enqueue subscribers asynchronously
	go func() {
		bgCtx := context.Background()
		enqueueSubscribersForCampaign(bgCtx, db, campUUID, listID, segmentID)

		// After enqueue, check if campaign has agent assignments
		var agentCount int
		db.QueryRowContext(bgCtx,
			`SELECT COUNT(*) FROM mailing_agent_campaigns WHERE campaign_id = $1`,
			campUUID,
		).Scan(&agentCount)

		if agentCount > 0 {
			log.Printf("Campaign %s has %d agent assignments, running pre-processor", campUUID, agentCount)
			preprocessor := NewAgentPreprocessor(db, nil)
			if err := preprocessor.PreprocessCampaign(bgCtx, campUUID); err != nil {
				log.Printf("Agent pre-processing warning (continuing anyway): %v", err)
				// Don't fail the campaign — just log and continue without agent decisions
			}
		}
	}()

	return jobID, nil
}

// enqueueSubscribersForCampaign adds all subscribers to the queue
func enqueueSubscribersForCampaign(ctx context.Context, db *sql.DB, campaignID uuid.UUID, listID, segmentID sql.NullString) {
	// Get campaign content
	var subject, htmlContent, plainContent string
	var maxRecipients sql.NullInt64
	var throttleSpeed string
	
	db.QueryRowContext(ctx, `
		SELECT subject, COALESCE(html_content, ''), COALESCE(plain_content, ''),
			   max_recipients, COALESCE(throttle_speed, 'gentle')
		FROM mailing_campaigns WHERE id = $1
	`, campaignID).Scan(&subject, &htmlContent, &plainContent, &maxRecipients, &throttleSpeed)

	// Calculate priority based on throttle
	priority := 5
	switch throttleSpeed {
	case "instant":
		priority = 10
	case "gentle":
		priority = 7
	case "moderate":
		priority = 5
	case "careful":
		priority = 3
	}

	// Build subscriber query
	var query string
	var args []interface{}
	
	if segmentID.Valid && segmentID.String != "" {
		query = `
			SELECT s.id, s.email FROM mailing_subscribers s
			WHERE s.status = 'confirmed'
			AND NOT EXISTS (
				SELECT 1 FROM mailing_suppressions sup 
				WHERE LOWER(sup.email) = LOWER(s.email) AND sup.active = true
			)
			AND NOT EXISTS (
				SELECT 1 FROM mailing_global_suppressions gs
				WHERE gs.md5_hash = MD5(LOWER(TRIM(s.email)))
			)
			ORDER BY s.id
		`
		args = []interface{}{}
	} else if listID.Valid && listID.String != "" {
		query = `
			SELECT s.id, s.email FROM mailing_subscribers s
			WHERE s.list_id = $1 AND s.status = 'confirmed'
			AND NOT EXISTS (
				SELECT 1 FROM mailing_suppressions sup 
				WHERE LOWER(sup.email) = LOWER(s.email) AND sup.active = true
			)
			AND NOT EXISTS (
				SELECT 1 FROM mailing_global_suppressions gs
				WHERE gs.md5_hash = MD5(LOWER(TRIM(s.email)))
			)
			ORDER BY s.id
		`
		args = []interface{}{listID.String}
	} else {
		log.Printf("[EnqueueCampaign] Campaign %s has no list or segment", campaignID)
		db.ExecContext(ctx, `
			UPDATE mailing_campaigns SET status = 'failed', completed_at = NOW() WHERE id = $1
		`, campaignID)
		return
	}

	// Apply limit
	if maxRecipients.Valid && maxRecipients.Int64 > 0 {
		query += fmt.Sprintf(" LIMIT %d", maxRecipients.Int64)
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		log.Printf("[EnqueueCampaign] Error querying subscribers: %v", err)
		return
	}
	defer rows.Close()

	// Insert into queue in batches
	var queued int
	const batchSize = 1000
	var batch []struct {
		ID    uuid.UUID
		Email string
	}

	for rows.Next() {
		var subID uuid.UUID
		var email string
		if err := rows.Scan(&subID, &email); err != nil {
			continue
		}
		batch = append(batch, struct {
			ID    uuid.UUID
			Email string
		}{subID, email})

		if len(batch) >= batchSize {
			queued += insertQueueBatch(ctx, db, campaignID, batch, subject, htmlContent, plainContent, priority)
			batch = batch[:0]
		}
	}

	// Insert remaining
	if len(batch) > 0 {
		queued += insertQueueBatch(ctx, db, campaignID, batch, subject, htmlContent, plainContent, priority)
	}

	// Update campaign with queue count
	if queued == 0 {
		// No subscribers to send to - mark as completed immediately
		db.ExecContext(ctx, `
			UPDATE mailing_campaigns SET status = 'completed', total_recipients = 0, 
			sent_count = 0, completed_at = NOW(), updated_at = NOW()
			WHERE id = $1
		`, campaignID)
	} else {
		db.ExecContext(ctx, `
			UPDATE mailing_campaigns SET total_recipients = $1, queued_count = $1, updated_at = NOW()
			WHERE id = $2
		`, queued, campaignID)
	}

	log.Printf("[EnqueueCampaign] Campaign %s: queued %d subscribers", campaignID, queued)
}

func insertQueueBatch(ctx context.Context, db *sql.DB, campaignID uuid.UUID, batch []struct{ ID uuid.UUID; Email string }, subject, html, plain string, priority int) int {
	inserted := 0
	for _, sub := range batch {
		queueID := uuid.New()
		_, err := db.ExecContext(ctx, `
			INSERT INTO mailing_campaign_queue (
				id, campaign_id, subscriber_id, subject, html_content, plain_content,
				status, priority, scheduled_at, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, 'queued', $7, NOW(), NOW())
			ON CONFLICT DO NOTHING
		`, queueID, campaignID, sub.ID, subject, html, plain, priority)
		
		if err == nil {
			inserted++
		}
	}
	return inserted
}
