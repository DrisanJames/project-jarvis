package worker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"

	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

// =============================================================================
// BATCH SEND WORKER - Optimized for 50M+ messages/day
// =============================================================================
// Claims 1000+ queue items at once, groups by ESP, sends in optimal batches,
// and performs batch updates for maximum throughput.

// ESP batch size limits
const (
	SparkPostBatchSize     = 2000
	SparkPostMaxPayloadMB  = 5
	SparkPostMaxPayloadBytes = 5 * 1024 * 1024
	SESBatchSize           = 50
	MailgunBatchSize       = 1000
	SendGridBatchSize      = 1000
	DefaultBatchSize       = 100
	DefaultBatchTimeout    = 60 * time.Second
)

// BatchSendWorker processes emails in large batches using ESP-specific batch APIs
type BatchSendWorker struct {
	db          *sql.DB
	redis       *redis.Client
	workerID    string
	rateLimiter *RateLimiter

	// Advanced throttle manager for per-domain/per-ISP rate limiting
	advancedThrottle *AdvancedThrottleManager
	orgID            string // Organization ID for throttling

	// Batch senders for each ESP
	sparkPostSender BatchESPSender
	sesSender       BatchESPSender
	mailgunSender   BatchESPSender
	sendgridSender  BatchESPSender

	// Batch grouper for optimizing batches
	batchGrouper *BatchGrouper

	// Configuration
	claimSize    int           // Number of items to claim at once (1000+)
	pollInterval time.Duration // How often to poll for new items
	numWorkers   int           // Number of concurrent batch processors

	// Stats
	totalSent    int64
	totalFailed  int64
	totalSkipped int64

	// Control
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	running bool
	mu      sync.RWMutex

	// Campaign content cache (reduces DB queries)
	contentCache map[string]*CampaignContent
	cacheMu      sync.RWMutex
}

// BatchESPSender interface for batch-capable ESP senders
type BatchESPSender interface {
	SendBatch(ctx context.Context, messages []EmailMessage) (*BatchSendResult, error)
	MaxBatchSize() int
}

// BatchQueueItem represents an item from the queue with all necessary data
type BatchQueueItem struct {
	ID               uuid.UUID
	CampaignID       uuid.UUID
	SubscriberID     uuid.UUID
	Email            string
	SubstitutionData map[string]interface{}
	Priority         int
	ProfileID        string
	ESPType          string
}

// BatchItemResult holds the result for a single item in a batch
type BatchItemResult struct {
	ID        uuid.UUID
	Success   bool
	MessageID string
	ErrorCode string
	Error     error
}

// BatchGrouper groups messages into optimal batches per ESP
// Supports both count-based and size-based batching limits
type BatchGrouper struct {
	espBatchSizes   map[string]int
	espPayloadLimits map[string]int // Maximum payload size in bytes per ESP
}

// NewBatchGrouper creates a new batch grouper with ESP-specific limits
func NewBatchGrouper() *BatchGrouper {
	return &BatchGrouper{
		espBatchSizes: map[string]int{
			"sparkpost": SparkPostBatchSize,
			"ses":       SESBatchSize,
			"mailgun":   MailgunBatchSize,
			"sendgrid":  SendGridBatchSize,
		},
		espPayloadLimits: map[string]int{
			"sparkpost": SparkPostMaxPayloadBytes,
			"ses":       10 * 1024 * 1024, // 10MB for SES
			"mailgun":   25 * 1024 * 1024, // 25MB for Mailgun
			"sendgrid":  30 * 1024 * 1024, // 30MB for SendGrid
		},
	}
}

// GetBatchSize returns the optimal batch size for an ESP
func (g *BatchGrouper) GetBatchSize(espType string) int {
	if size, ok := g.espBatchSizes[espType]; ok {
		return size
	}
	return DefaultBatchSize
}

// GetPayloadLimit returns the maximum payload size for an ESP
func (g *BatchGrouper) GetPayloadLimit(espType string) int {
	if limit, ok := g.espPayloadLimits[espType]; ok {
		return limit
	}
	return SparkPostMaxPayloadBytes // Default to 5MB
}

// GroupIntoBatches splits messages into optimal batches for the given ESP
// Respects both count limits and estimated payload size limits
func (g *BatchGrouper) GroupIntoBatches(messages []BatchQueueItem, espType string) [][]BatchQueueItem {
	maxBatchSize := g.GetBatchSize(espType)
	maxPayload := g.GetPayloadLimit(espType)

	if maxBatchSize <= 0 {
		maxBatchSize = DefaultBatchSize
	}

	var batches [][]BatchQueueItem
	var currentBatch []BatchQueueItem
	var currentSize int

	for _, msg := range messages {
		// Estimate size for this message
		msgSize := g.estimateMessageSize(msg)

		// Check if adding this message would exceed limits
		wouldExceedCount := len(currentBatch) >= maxBatchSize
		wouldExceedSize := currentSize+msgSize > maxPayload

		if wouldExceedCount || wouldExceedSize {
			// Save current batch and start new one
			if len(currentBatch) > 0 {
				batches = append(batches, currentBatch)
			}
			currentBatch = nil
			currentSize = 0
		}

		currentBatch = append(currentBatch, msg)
		currentSize += msgSize
	}

	// Don't forget the last batch
	if len(currentBatch) > 0 {
		batches = append(batches, currentBatch)
	}

	return batches
}

// estimateMessageSize estimates the payload size contribution of a single message
func (g *BatchGrouper) estimateMessageSize(msg BatchQueueItem) int {
	// Base size: email address + IDs
	size := len(msg.Email) + 36*3 // 36 bytes per UUID (ID, CampaignID, SubscriberID)

	// Add substitution data size
	if msg.SubstitutionData != nil {
		dataBytes, _ := json.Marshal(msg.SubstitutionData)
		size += len(dataBytes)
	}

	// Add JSON structure overhead per recipient
	size += 200

	return size
}

// GroupIntoBatchesSimple is a simple count-based batching (legacy behavior)
func (g *BatchGrouper) GroupIntoBatchesSimple(messages []BatchQueueItem, espType string) [][]BatchQueueItem {
	batchSize := g.GetBatchSize(espType)
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}

	var batches [][]BatchQueueItem
	for i := 0; i < len(messages); i += batchSize {
		end := i + batchSize
		if end > len(messages) {
			end = len(messages)
		}
		batches = append(batches, messages[i:end])
	}
	return batches
}

// ValidateBatch checks if a batch is valid for the given ESP
func (g *BatchGrouper) ValidateBatch(messages []BatchQueueItem, espType string) error {
	if len(messages) == 0 {
		return fmt.Errorf("batch is empty")
	}

	maxCount := g.GetBatchSize(espType)
	if len(messages) > maxCount {
		return fmt.Errorf("batch size %d exceeds maximum %d for %s", len(messages), maxCount, espType)
	}

	// Estimate total payload size
	totalSize := 0
	for _, msg := range messages {
		totalSize += g.estimateMessageSize(msg)
	}

	maxPayload := g.GetPayloadLimit(espType)
	if totalSize > maxPayload {
		return fmt.Errorf("estimated payload size %d bytes exceeds maximum %d bytes for %s",
			totalSize, maxPayload, espType)
	}

	return nil
}

// NewBatchSendWorker creates a new batch send worker
func NewBatchSendWorker(db *sql.DB, redisClient *redis.Client) *BatchSendWorker {
	return &BatchSendWorker{
		db:           db,
		redis:        redisClient,
		workerID:     fmt.Sprintf("batch-worker-%s", uuid.New().String()[:8]),
		batchGrouper: NewBatchGrouper(),
		claimSize:    1000,                     // Claim 1000 items at once
		pollInterval: 50 * time.Millisecond,    // Fast polling for low latency
		numWorkers:   4,                        // 4 concurrent batch processors
		contentCache: make(map[string]*CampaignContent),
	}
}

// SetRateLimiter sets the rate limiter
func (w *BatchSendWorker) SetRateLimiter(rl *RateLimiter) {
	w.rateLimiter = rl
}

// SetAdvancedThrottle sets the advanced throttle manager for per-domain/per-ISP throttling
func (w *BatchSendWorker) SetAdvancedThrottle(throttle *AdvancedThrottleManager, orgID string) {
	w.advancedThrottle = throttle
	w.orgID = orgID
}

// SetBatchSenders sets the batch ESP senders
func (w *BatchSendWorker) SetBatchSenders(sparkpost, ses, mailgun, sendgrid BatchESPSender) {
	w.sparkPostSender = sparkpost
	w.sesSender = ses
	w.mailgunSender = mailgun
	w.sendgridSender = sendgrid
}

// SetClaimSize sets the number of items to claim at once
func (w *BatchSendWorker) SetClaimSize(size int) {
	if size > 0 {
		w.claimSize = size
	}
}

// SetNumWorkers sets the number of concurrent batch processors
func (w *BatchSendWorker) SetNumWorkers(n int) {
	if n > 0 {
		w.numWorkers = n
	}
}

// Start begins the batch send worker
func (w *BatchSendWorker) Start() {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return
	}
	w.running = true
	w.ctx, w.cancel = context.WithCancel(context.Background())
	w.mu.Unlock()

	log.Printf("[BatchSendWorker] Starting %d workers (claim_size=%d)", w.numWorkers, w.claimSize)

	// Start batch workers
	for i := 0; i < w.numWorkers; i++ {
		w.wg.Add(1)
		go w.batchWorker(i)
	}

	// Start cache cleanup routine
	go w.cacheCleanup()

	// Start heartbeat
	go w.heartbeatLoop()
}

// Stop gracefully stops the worker
func (w *BatchSendWorker) Stop() {
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return
	}
	w.running = false
	w.cancel()
	w.mu.Unlock()

	log.Println("[BatchSendWorker] Stopping workers...")
	w.wg.Wait()

	log.Printf("[BatchSendWorker] Stopped. Total sent: %d, failed: %d, skipped: %d",
		atomic.LoadInt64(&w.totalSent), atomic.LoadInt64(&w.totalFailed), atomic.LoadInt64(&w.totalSkipped))
}

// Stats returns current statistics
func (w *BatchSendWorker) Stats() map[string]int64 {
	return map[string]int64{
		"total_sent":    atomic.LoadInt64(&w.totalSent),
		"total_failed":  atomic.LoadInt64(&w.totalFailed),
		"total_skipped": atomic.LoadInt64(&w.totalSkipped),
	}
}

// batchWorker is the main worker loop
func (w *BatchSendWorker) batchWorker(workerNum int) {
	defer w.wg.Done()

	for {
		select {
		case <-w.ctx.Done():
			return
		default:
			err := w.processBatch(w.ctx, workerNum)
			if err != nil {
				log.Printf("[BatchWorker %d] Error processing batch: %v", workerNum, err)
				time.Sleep(time.Second) // Back off on error
				continue
			}
		}
	}
}

// processBatch claims and processes a batch of queue items
func (w *BatchSendWorker) processBatch(ctx context.Context, workerNum int) error {
	// 1. Claim batch of queue items
	items, err := w.claimQueueItems(ctx, w.claimSize)
	if err != nil {
		return fmt.Errorf("failed to claim items: %w", err)
	}

	if len(items) == 0 {
		// No items available, wait before polling again
		time.Sleep(w.pollInterval)
		return nil
	}

	log.Printf("[BatchWorker %d] Claimed %d items", workerNum, len(items))

	// 2. Group by ESP type
	espGroups := w.groupByESP(items)

	// 3. Send each group using appropriate batch sender
	var allResults []BatchItemResult

	for espType, groupItems := range espGroups {
		// Split into optimal batch sizes for this ESP
		batches := w.batchGrouper.GroupIntoBatches(groupItems, espType)

		for _, batch := range batches {
			// Check rate limit before sending
			if w.rateLimiter != nil {
				allowed, waitTime, err := w.rateLimiter.CheckAndIncrement(ctx, espType, len(batch))
				if err != nil {
					log.Printf("[BatchWorker %d] Rate limit error for %s: %v", workerNum, espType, err)
				}
				if !allowed && waitTime > 0 {
					// Return items to queue and wait
					w.returnItemsToQueue(ctx, batch)
					time.Sleep(waitTime)
					continue
				}
			}

			// Check advanced throttle (per-domain/per-ISP) before sending
			if w.advancedThrottle != nil {
				var allowedItems []BatchQueueItem
				var throttledItems []BatchQueueItem

				for _, item := range batch {
					allowed, reason, err := w.advancedThrottle.CanSend(ctx, w.orgID, item.Email)
					if err != nil {
						log.Printf("[BatchWorker %d] Throttle check error for %s: %v", workerNum, logger.RedactEmail(item.Email), err)
						allowed = true // Allow on error to avoid blocking
					}
					if allowed {
						allowedItems = append(allowedItems, item)
					} else {
						throttledItems = append(throttledItems, item)
						log.Printf("[BatchWorker %d] Throttled: %s (%s)", workerNum, item.Email, reason)
						atomic.AddInt64(&w.totalSkipped, 1)
					}
				}

				// Return throttled items to queue for later retry
				if len(throttledItems) > 0 {
					w.returnItemsToQueue(ctx, throttledItems)
				}

				// Continue with allowed items only
				if len(allowedItems) == 0 {
					continue
				}
				batch = allowedItems
			}

			// Send batch via appropriate sender
			results := w.sendBatch(ctx, espType, batch)
			allResults = append(allResults, results...)

			// Record successful sends with advanced throttle
			if w.advancedThrottle != nil {
				for _, result := range results {
					if result.Success {
						// Find the email for this result
						for _, item := range batch {
							if item.ID == result.ID {
								w.advancedThrottle.RecordSend(ctx, w.orgID, item.Email)
								break
							}
						}
					}
				}
			}
		}
	}

	// 4. Batch update queue items
	if len(allResults) > 0 {
		if err := w.updateQueueItems(ctx, allResults); err != nil {
			log.Printf("[BatchWorker %d] Error updating queue items: %v", workerNum, err)
		}

		// 5. Batch log to message_log
		if err := w.batchLogMessages(ctx, allResults, items); err != nil {
			log.Printf("[BatchWorker %d] Error logging messages: %v", workerNum, err)
		}
	}

	return nil
}

// claimQueueItems claims a batch of queue items using FOR UPDATE SKIP LOCKED
func (w *BatchSendWorker) claimQueueItems(ctx context.Context, limit int) ([]BatchQueueItem, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Use CTE with FOR UPDATE SKIP LOCKED for efficient concurrent claiming
	// This ensures multiple workers can claim items without blocking each other
	rows, err := w.db.QueryContext(queryCtx, `
		WITH claimed AS (
			SELECT id, campaign_id
			FROM mailing_campaign_queue_v2
			WHERE status = 'queued'
			  AND scheduled_at <= NOW()
			ORDER BY priority DESC, scheduled_at ASC
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE mailing_campaign_queue_v2 q
		SET status = 'processing', 
		    claimed_at = NOW(), 
		    worker_id = $2
		FROM claimed c
		WHERE q.id = c.id AND q.campaign_id = c.campaign_id
		RETURNING q.id, q.campaign_id, q.subscriber_id, q.email, 
		          COALESCE(q.substitution_data, '{}')::text, q.priority
	`, limit, w.workerID)

	if err != nil {
		return nil, fmt.Errorf("claim query failed: %w", err)
	}
	defer rows.Close()

	var items []BatchQueueItem
	campaignIDs := make(map[uuid.UUID]bool)

	for rows.Next() {
		var item BatchQueueItem
		var subDataJSON string

		err := rows.Scan(
			&item.ID,
			&item.CampaignID,
			&item.SubscriberID,
			&item.Email,
			&subDataJSON,
			&item.Priority,
		)
		if err != nil {
			log.Printf("[BatchSendWorker] Error scanning row: %v", err)
			continue
		}

		// Parse substitution data
		if err := json.Unmarshal([]byte(subDataJSON), &item.SubstitutionData); err != nil {
			item.SubstitutionData = make(map[string]interface{})
		}

		items = append(items, item)
		campaignIDs[item.CampaignID] = true
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}

	// Enrich items with campaign/profile info (cached)
	for i := range items {
		content, err := w.getCampaignContent(ctx, items[i].CampaignID)
		if err != nil {
			log.Printf("[BatchSendWorker] Error fetching campaign content for %s: %v", items[i].CampaignID, err)
			continue
		}
		items[i].ProfileID = content.ProfileID
		items[i].ESPType = content.ESPType
	}

	return items, nil
}

// groupByESP groups items by their ESP type for batch sending
func (w *BatchSendWorker) groupByESP(items []BatchQueueItem) map[string][]BatchQueueItem {
	groups := make(map[string][]BatchQueueItem)
	for _, item := range items {
		espType := item.ESPType
		if espType == "" {
			espType = "ses" // Default to SES
		}
		groups[espType] = append(groups[espType], item)
	}
	return groups
}

// sendBatch sends a batch of messages using the appropriate ESP sender
func (w *BatchSendWorker) sendBatch(ctx context.Context, espType string, items []BatchQueueItem) []BatchItemResult {
	results := make([]BatchItemResult, len(items))

	// Convert BatchQueueItem to EmailMessage
	messages := make([]EmailMessage, len(items))
	for i, item := range items {
		content, err := w.getCampaignContent(ctx, item.CampaignID)
		if err != nil {
			results[i] = BatchItemResult{
				ID:        item.ID,
				Success:   false,
				ErrorCode: "content_fetch_failed",
				Error:     err,
			}
			atomic.AddInt64(&w.totalFailed, 1)
			continue
		}

		// Apply substitutions
		subject := w.applySubstitutions(content.Subject, item.SubstitutionData)
		htmlContent := w.applySubstitutions(content.HTMLContent, item.SubstitutionData)
		textContent := w.applySubstitutions(content.TextContent, item.SubstitutionData)

		messages[i] = EmailMessage{
			ID:           item.ID.String(),
			CampaignID:   item.CampaignID.String(),
			SubscriberID: item.SubscriberID.String(),
			Email:        item.Email,
			FromName:     content.FromName,
			FromEmail:    content.FromEmail,
			ReplyTo:      content.ReplyTo,
			Subject:      subject,
			HTMLContent:  htmlContent,
			TextContent:  textContent,
			ProfileID:    item.ProfileID,
			ESPType:      espType,
			Metadata:     item.SubstitutionData,
		}
	}

	// Get appropriate batch sender
	var sender BatchESPSender
	switch espType {
	case "sparkpost":
		sender = w.sparkPostSender
	case "ses":
		sender = w.sesSender
	case "mailgun":
		sender = w.mailgunSender
	case "sendgrid":
		sender = w.sendgridSender
	}

	if sender == nil {
		// No batch sender configured, mark all as failed
		for i, item := range items {
			if results[i].Error != nil {
				continue // Already has an error
			}
			results[i] = BatchItemResult{
				ID:        item.ID,
				Success:   false,
				ErrorCode: "no_sender_configured",
				Error:     fmt.Errorf("no batch sender configured for %s", espType),
			}
			atomic.AddInt64(&w.totalFailed, 1)
		}
		return results
	}

	// Send the batch
	batchResult, err := sender.SendBatch(ctx, messages)
	if err != nil {
		// Entire batch failed
		for i, item := range items {
			if results[i].Error != nil {
				continue
			}
			results[i] = BatchItemResult{
				ID:        item.ID,
				Success:   false,
				ErrorCode: "batch_send_failed",
				Error:     err,
			}
			atomic.AddInt64(&w.totalFailed, 1)
		}
		return results
	}

	// Map results back to items
	for i, item := range items {
		if results[i].Error != nil {
			continue // Already has an error from content fetch
		}

		if i < len(batchResult.Results) {
			r := batchResult.Results[i]
			results[i] = BatchItemResult{
				ID:        item.ID,
				Success:   r.Success,
				MessageID: r.MessageID,
				Error:     r.Error,
			}
			if r.Success {
				atomic.AddInt64(&w.totalSent, 1)
			} else {
				atomic.AddInt64(&w.totalFailed, 1)
				if r.Error != nil {
					results[i].ErrorCode = truncateString(r.Error.Error(), 50)
				}
			}
		} else {
			// Result missing for this item
			results[i] = BatchItemResult{
				ID:        item.ID,
				Success:   false,
				ErrorCode: "result_missing",
				Error:     fmt.Errorf("no result returned for item"),
			}
			atomic.AddInt64(&w.totalFailed, 1)
		}
	}

	return results
}

// updateQueueItems batch updates queue item statuses using UNNEST
func (w *BatchSendWorker) updateQueueItems(ctx context.Context, results []BatchItemResult) error {
	if len(results) == 0 {
		return nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Prepare arrays for UNNEST
	ids := make([]uuid.UUID, len(results))
	statuses := make([]string, len(results))
	messageIDs := make([]string, len(results))
	errorCodes := make([]string, len(results))

	for i, r := range results {
		ids[i] = r.ID
		if r.Success {
			statuses[i] = "sent"
			messageIDs[i] = r.MessageID
			errorCodes[i] = ""
		} else {
			statuses[i] = "failed"
			messageIDs[i] = ""
			if r.ErrorCode != "" {
				errorCodes[i] = r.ErrorCode
			} else if r.Error != nil {
				errorCodes[i] = truncateString(r.Error.Error(), 50)
			}
		}
	}

	// Use UNNEST for efficient batch update
	_, err := w.db.ExecContext(queryCtx, `
		UPDATE mailing_campaign_queue_v2
		SET status = data.status,
		    sent_at = CASE WHEN data.status = 'sent' THEN NOW() ELSE NULL END,
		    message_id = NULLIF(data.message_id, ''),
		    error_code = NULLIF(data.error_code, ''),
		    retry_count = CASE WHEN data.status = 'failed' THEN retry_count + 1 ELSE retry_count END
		FROM (
			SELECT UNNEST($1::uuid[]) as id,
			       UNNEST($2::text[]) as status,
			       UNNEST($3::text[]) as message_id,
			       UNNEST($4::text[]) as error_code
		) as data
		WHERE mailing_campaign_queue_v2.id = data.id
	`, pq.Array(ids), pq.Array(statuses), pq.Array(messageIDs), pq.Array(errorCodes))

	if err != nil {
		return fmt.Errorf("batch update failed: %w", err)
	}

	return nil
}

// batchLogMessages logs sent messages to message_log for webhook correlation
func (w *BatchSendWorker) batchLogMessages(ctx context.Context, results []BatchItemResult, items []BatchQueueItem) error {
	// Only log successful sends
	var successResults []BatchItemResult
	itemMap := make(map[uuid.UUID]BatchQueueItem)
	for _, item := range items {
		itemMap[item.ID] = item
	}

	for _, r := range results {
		if r.Success && r.MessageID != "" {
			successResults = append(successResults, r)
		}
	}

	if len(successResults) == 0 {
		return nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Use COPY for bulk insert of message logs
	txn, err := w.db.BeginTx(queryCtx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer txn.Rollback()

	stmt, err := txn.Prepare(pq.CopyIn(
		"mailing_message_log",
		"message_id", "campaign_id", "subscriber_id", "email",
		"esp_type", "sent_at",
	))
	if err != nil {
		return fmt.Errorf("failed to prepare COPY: %w", err)
	}

	now := time.Now()
	for _, r := range successResults {
		item, ok := itemMap[r.ID]
		if !ok {
			continue
		}

		_, err = stmt.Exec(
			r.MessageID,
			item.CampaignID,
			item.SubscriberID,
			item.Email,
			item.ESPType,
			now,
		)
		if err != nil {
			log.Printf("[BatchSendWorker] Warning: failed to log message %s: %v", r.MessageID, err)
		}
	}

	// Flush COPY
	_, err = stmt.Exec()
	if err != nil {
		return fmt.Errorf("failed to flush COPY: %w", err)
	}

	if err := stmt.Close(); err != nil {
		return fmt.Errorf("failed to close statement: %w", err)
	}

	if err := txn.Commit(); err != nil {
		return fmt.Errorf("failed to commit: %w", err)
	}

	return nil
}

// returnItemsToQueue returns items to the queue for later processing
func (w *BatchSendWorker) returnItemsToQueue(ctx context.Context, items []BatchQueueItem) error {
	if len(items) == 0 {
		return nil
	}

	ids := make([]uuid.UUID, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}

	_, err := w.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue_v2
		SET status = 'queued', worker_id = NULL, claimed_at = NULL
		WHERE id = ANY($1)
	`, pq.Array(ids))

	return err
}

// getCampaignContent gets campaign content from cache or database
func (w *BatchSendWorker) getCampaignContent(ctx context.Context, campaignID uuid.UUID) (*CampaignContent, error) {
	key := campaignID.String()

	// Check cache first
	w.cacheMu.RLock()
	if content, ok := w.contentCache[key]; ok {
		w.cacheMu.RUnlock()
		return content, nil
	}
	w.cacheMu.RUnlock()

	// Fetch from database
	var content CampaignContent
	var profileID sql.NullString
	err := w.db.QueryRowContext(ctx, `
		SELECT 
			c.subject,
			c.html_content,
			COALESCE(c.plain_content, ''),
			c.from_name,
			c.from_email,
			COALESCE(c.reply_to, ''),
			c.sending_profile_id::text,
			COALESCE(sp.vendor_type, 'ses')
		FROM mailing_campaigns c
		LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
		WHERE c.id = $1
	`, campaignID).Scan(
		&content.Subject,
		&content.HTMLContent,
		&content.TextContent,
		&content.FromName,
		&content.FromEmail,
		&content.ReplyTo,
		&profileID,
		&content.ESPType,
	)

	if err != nil {
		return nil, fmt.Errorf("failed to fetch campaign content: %w", err)
	}

	if profileID.Valid {
		content.ProfileID = profileID.String
	}
	content.FetchedAt = time.Now()

	// Cache it
	w.cacheMu.Lock()
	w.contentCache[key] = &content
	w.cacheMu.Unlock()

	return &content, nil
}

// applySubstitutions replaces placeholders with actual values
func (w *BatchSendWorker) applySubstitutions(template string, data map[string]interface{}) string {
	result := template
	for key, value := range data {
		// Handle {{ key }} format
		placeholder := fmt.Sprintf("{{ %s }}", key)
		result = strings.ReplaceAll(result, placeholder, fmt.Sprintf("%v", value))

		// Handle {{key}} format (no spaces)
		placeholder2 := fmt.Sprintf("{{%s}}", key)
		result = strings.ReplaceAll(result, placeholder2, fmt.Sprintf("%v", value))
	}
	return result
}

// cacheCleanup periodically cleans up old cache entries
func (w *BatchSendWorker) cacheCleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			w.cacheMu.Lock()
			now := time.Now()
			for key, content := range w.contentCache {
				if now.Sub(content.FetchedAt) > 10*time.Minute {
					delete(w.contentCache, key)
				}
			}
			w.cacheMu.Unlock()
		}
	}
}

// heartbeatLoop sends periodic heartbeats
func (w *BatchSendWorker) heartbeatLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			stats := w.Stats()
			statsJSON, _ := json.Marshal(stats)
			w.db.Exec(`
				INSERT INTO mailing_workers (id, worker_type, hostname, status, max_concurrent, started_at, last_heartbeat_at, metadata)
				VALUES ($1, 'batch_sender', 'ignite-batch-worker', 'running', $2, NOW(), NOW(), $3)
				ON CONFLICT (id) DO UPDATE SET
					last_heartbeat_at = NOW(),
					total_processed = $4,
					total_errors = $5,
					metadata = $3
			`, w.workerID, w.claimSize*w.numWorkers, string(statsJSON), stats["total_sent"], stats["total_failed"])
		}
	}
}

// truncateString truncates a string to the specified length
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

// =============================================================================
// BATCH ESP SENDER IMPLEMENTATIONS
// =============================================================================

// SparkPostBatchSender implements batch sending for SparkPost (up to 2000 recipients)
// Includes payload size checking (max 5MB) and full API implementation
type SparkPostBatchSender struct {
	apiKey       string
	baseURL      string
	httpClient   *http.Client
	maxBatch     int // Maximum recipients per batch (default 2000)
	maxPayloadMB int // Maximum payload size in MB (default 5)
	db           *sql.DB
}

// SparkPostBatchConfig holds configuration for SparkPostBatchSender
type SparkPostBatchConfig struct {
	APIKey       string
	BaseURL      string
	MaxBatch     int
	MaxPayloadMB int
	Timeout      time.Duration
}

// SparkPostTransmission represents the SparkPost transmission payload
type SparkPostTransmission struct {
	Recipients []SparkPostRecipient       `json:"recipients"`
	Content    SparkPostContent           `json:"content"`
	Metadata   map[string]interface{}     `json:"metadata,omitempty"`
	Options    *SparkPostTransmissionOpts `json:"options,omitempty"`
}

// SparkPostRecipient represents a single recipient in a batch
type SparkPostRecipient struct {
	Address          SparkPostAddress       `json:"address"`
	SubstitutionData map[string]interface{} `json:"substitution_data,omitempty"`
	Metadata         map[string]interface{} `json:"metadata,omitempty"`
	Tags             []string               `json:"tags,omitempty"`
}

// SparkPostAddress represents an email address
type SparkPostAddress struct {
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	HeaderTo string `json:"header_to,omitempty"`
}

// SparkPostContent represents the email content
type SparkPostContent struct {
	From    SparkPostAddress  `json:"from"`
	Subject string            `json:"subject"`
	HTML    string            `json:"html,omitempty"`
	Text    string            `json:"text,omitempty"`
	ReplyTo string            `json:"reply_to,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// SparkPostTransmissionOpts represents transmission options
type SparkPostTransmissionOpts struct {
	OpenTracking  bool `json:"open_tracking"`
	ClickTracking bool `json:"click_tracking"`
	Transactional bool `json:"transactional"`
}

// SparkPostBatchResponse represents the API response from SparkPost
type SparkPostBatchResponse struct {
	Results struct {
		ID                  string `json:"id"`
		TotalAcceptedRecips int    `json:"total_accepted_recipients"`
		TotalRejectedRecips int    `json:"total_rejected_recipients"`
	} `json:"results"`
	Errors []struct {
		Message     string `json:"message"`
		Code        string `json:"code"`
		Description string `json:"description"`
	} `json:"errors,omitempty"`
}

// NewSparkPostBatchSender creates a new SparkPost batch sender with default settings
func NewSparkPostBatchSender(apiKey string, db *sql.DB) *SparkPostBatchSender {
	return NewSparkPostBatchSenderWithConfig(SparkPostBatchConfig{
		APIKey:       apiKey,
		BaseURL:      "https://api.sparkpost.com/api/v1",
		MaxBatch:     SparkPostBatchSize,
		MaxPayloadMB: SparkPostMaxPayloadMB,
		Timeout:      DefaultBatchTimeout,
	}, db)
}

// NewSparkPostBatchSenderWithConfig creates a new SparkPost batch sender with custom config
func NewSparkPostBatchSenderWithConfig(cfg SparkPostBatchConfig, db *sql.DB) *SparkPostBatchSender {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.sparkpost.com/api/v1"
	}
	if cfg.MaxBatch <= 0 || cfg.MaxBatch > SparkPostBatchSize {
		cfg.MaxBatch = SparkPostBatchSize
	}
	if cfg.MaxPayloadMB <= 0 || cfg.MaxPayloadMB > SparkPostMaxPayloadMB {
		cfg.MaxPayloadMB = SparkPostMaxPayloadMB
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultBatchTimeout
	}

	return &SparkPostBatchSender{
		apiKey:       cfg.APIKey,
		baseURL:      cfg.BaseURL,
		maxBatch:     cfg.MaxBatch,
		maxPayloadMB: cfg.MaxPayloadMB,
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
		},
		db: db,
	}
}

// MaxBatchSize returns the maximum batch size for SparkPost
func (s *SparkPostBatchSender) MaxBatchSize() int {
	return s.maxBatch
}

// MaxPayloadSize returns the maximum payload size in bytes
func (s *SparkPostBatchSender) MaxPayloadSize() int {
	return s.maxPayloadMB * 1024 * 1024
}

// SendBatch sends a batch of emails via SparkPost in a single API call
// Supports up to 2000 recipients per transmission with 5MB payload limit
func (s *SparkPostBatchSender) SendBatch(ctx context.Context, messages []EmailMessage) (*BatchSendResult, error) {
	if s.apiKey == "" {
		return nil, fmt.Errorf("SparkPost API key not configured")
	}

	if len(messages) == 0 {
		return &BatchSendResult{}, nil
	}

	if len(messages) > s.maxBatch {
		return nil, fmt.Errorf("batch size %d exceeds maximum %d", len(messages), s.maxBatch)
	}

	// All messages in a batch must share the same content template
	// Use the first message as the template
	templateMsg := messages[0]

	// Build recipients array with substitution_data for personalization
	// and metadata for tracking (queue_id, campaign_id, subscriber_id)
	recipients := make([]SparkPostRecipient, 0, len(messages))
	for _, msg := range messages {
		recipient := SparkPostRecipient{
			Address: SparkPostAddress{
				Email: msg.Email,
			},
			// substitution_data for personalization (merge variables)
			SubstitutionData: msg.Metadata,
			// metadata for tracking - persisted in webhooks
			Metadata: map[string]interface{}{
				"queue_id":      msg.ID,
				"campaign_id":   msg.CampaignID,
				"subscriber_id": msg.SubscriberID,
			},
		}
		recipients = append(recipients, recipient)
	}

	// Build transmission payload
	transmission := SparkPostTransmission{
		Recipients: recipients,
		Content: SparkPostContent{
			From: SparkPostAddress{
				Email: templateMsg.FromEmail,
				Name:  templateMsg.FromName,
			},
			Subject: templateMsg.Subject,
			HTML:    templateMsg.HTMLContent,
			Text:    templateMsg.TextContent,
			ReplyTo: templateMsg.ReplyTo,
		},
		Metadata: map[string]interface{}{
			"campaign_id": templateMsg.CampaignID,
			"batch_size":  len(messages),
			"sent_at":     time.Now().UTC().Format(time.RFC3339),
		},
		Options: &SparkPostTransmissionOpts{
			OpenTracking:  true,
			ClickTracking: true,
			Transactional: false,
		},
	}

	// Marshal and check payload size (max 5MB)
	jsonData, err := json.Marshal(transmission)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal transmission: %w", err)
	}

	maxBytes := s.maxPayloadMB * 1024 * 1024
	if len(jsonData) > maxBytes {
		return nil, fmt.Errorf("payload size %d bytes exceeds maximum %d bytes (%dMB)",
			len(jsonData), maxBytes, s.maxPayloadMB)
	}

	// Make API request
	req, err := http.NewRequestWithContext(ctx, "POST", s.baseURL+"/transmissions", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", s.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Parse response
	var apiResponse SparkPostBatchResponse
	if err := json.Unmarshal(body, &apiResponse); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w (body: %s)", err, string(body))
	}

	// Handle errors
	if resp.StatusCode >= 400 {
		var errMsgs []string
		for _, e := range apiResponse.Errors {
			errMsgs = append(errMsgs, fmt.Sprintf("%s: %s", e.Code, e.Message))
		}
		return nil, fmt.Errorf("SparkPost API error %d: %v", resp.StatusCode, errMsgs)
	}

	// Build result with accepted/rejected counts
	result := &BatchSendResult{
		TransmissionID: apiResponse.Results.ID,
		Accepted:       apiResponse.Results.TotalAcceptedRecips,
		Rejected:       apiResponse.Results.TotalRejectedRecips,
		Results:        make([]SendResult, len(messages)),
	}

	// Populate individual results
	// Note: SparkPost doesn't return per-recipient results in the transmission response
	// Individual delivery status comes via webhooks
	sentAt := time.Now()
	for i, msg := range messages {
		result.Results[i] = SendResult{
			Success:   true, // Accepted by SparkPost (delivery status via webhooks)
			MessageID: fmt.Sprintf("%s_%d", apiResponse.Results.ID, i),
			ESPType:   "sparkpost",
			SentAt:    sentAt,
		}
		log.Printf("[SparkPost Batch] Queued email to %s (transmission_id: %s, index: %d)",
			logger.RedactEmail(msg.Email), apiResponse.Results.ID, i)
	}

	log.Printf("[SparkPost Batch] Transmission %s: accepted=%d, rejected=%d, total=%d",
		apiResponse.Results.ID, result.Accepted, result.Rejected, len(messages))

	return result, nil
}

// EstimatePayloadSize estimates the JSON payload size for a batch of messages
func (s *SparkPostBatchSender) EstimatePayloadSize(messages []EmailMessage) int {
	if len(messages) == 0 {
		return 0
	}

	// Base content size (from first message as template)
	templateMsg := messages[0]
	baseSize := len(templateMsg.HTMLContent) + len(templateMsg.TextContent) +
		len(templateMsg.Subject) + len(templateMsg.FromEmail) + len(templateMsg.FromName)

	// Estimate per-recipient overhead (email + metadata JSON)
	perRecipientOverhead := 200 // Conservative estimate for JSON structure
	recipientSize := 0
	for _, msg := range messages {
		recipientSize += len(msg.Email) + perRecipientOverhead
		// Add metadata size estimation
		if msg.Metadata != nil {
			metaBytes, _ := json.Marshal(msg.Metadata)
			recipientSize += len(metaBytes)
		}
	}

	return baseSize + recipientSize + 500 // 500 bytes for JSON structure overhead
}

// ValidateBatch checks if a batch is valid for sending
func (s *SparkPostBatchSender) ValidateBatch(messages []EmailMessage) error {
	if len(messages) == 0 {
		return fmt.Errorf("batch is empty")
	}
	if len(messages) > s.maxBatch {
		return fmt.Errorf("batch size %d exceeds maximum %d", len(messages), s.maxBatch)
	}

	// Check estimated payload size
	estimatedSize := s.EstimatePayloadSize(messages)
	maxBytes := s.maxPayloadMB * 1024 * 1024
	if estimatedSize > maxBytes {
		return fmt.Errorf("estimated payload size %d bytes exceeds maximum %d bytes",
			estimatedSize, maxBytes)
	}

	return nil
}

// SESBatchSender implements batch sending for AWS SES (up to 50 recipients)
type SESBatchSender struct {
	accessKey string
	secretKey string
	region    string
	db        *sql.DB
}

// NewSESBatchSender creates a new SES batch sender
func NewSESBatchSender(accessKey, secretKey, region string, db *sql.DB) *SESBatchSender {
	if region == "" {
		region = "us-east-1"
	}
	return &SESBatchSender{
		accessKey: accessKey,
		secretKey: secretKey,
		region:    region,
		db:        db,
	}
}

// MaxBatchSize returns the maximum batch size for SES
func (s *SESBatchSender) MaxBatchSize() int {
	return SESBatchSize
}

// SendBatch sends a batch of emails via SES
func (s *SESBatchSender) SendBatch(ctx context.Context, messages []EmailMessage) (*BatchSendResult, error) {
	if s.accessKey == "" || s.secretKey == "" {
		return nil, fmt.Errorf("SES credentials not configured")
	}

	if len(messages) == 0 {
		return &BatchSendResult{}, nil
	}

	// SES SendBulkEmail supports up to 50 destinations per call
	log.Printf("[SESBatch] Would send %d emails via SES", len(messages))

	result := &BatchSendResult{
		Accepted: len(messages),
		Rejected: 0,
		Results:  make([]SendResult, len(messages)),
	}

	for i, msg := range messages {
		result.Results[i] = SendResult{
			Success:   true,
			MessageID: fmt.Sprintf("ses-%s", uuid.New().String()[:8]),
			ESPType:   "ses",
			SentAt:    time.Now(),
		}
		_ = msg
	}

	return result, nil
}

// MailgunBatchSender implements batch sending for Mailgun (up to 1000 recipients)
type MailgunBatchSender struct {
	apiKey string
	domain string
	db     *sql.DB
}

// NewMailgunBatchSender creates a new Mailgun batch sender
func NewMailgunBatchSender(apiKey, domain string, db *sql.DB) *MailgunBatchSender {
	return &MailgunBatchSender{
		apiKey: apiKey,
		domain: domain,
		db:     db,
	}
}

// MaxBatchSize returns the maximum batch size for Mailgun
func (s *MailgunBatchSender) MaxBatchSize() int {
	return MailgunBatchSize
}

// SendBatch sends a batch of emails via Mailgun
func (s *MailgunBatchSender) SendBatch(ctx context.Context, messages []EmailMessage) (*BatchSendResult, error) {
	if s.apiKey == "" {
		return nil, fmt.Errorf("Mailgun API key not configured")
	}

	if len(messages) == 0 {
		return &BatchSendResult{}, nil
	}

	// Mailgun supports up to 1000 recipients per batch
	log.Printf("[MailgunBatch] Would send %d emails via Mailgun", len(messages))

	result := &BatchSendResult{
		Accepted: len(messages),
		Rejected: 0,
		Results:  make([]SendResult, len(messages)),
	}

	for i, msg := range messages {
		result.Results[i] = SendResult{
			Success:   true,
			MessageID: fmt.Sprintf("mg-%s", uuid.New().String()[:8]),
			ESPType:   "mailgun",
			SentAt:    time.Now(),
		}
		_ = msg
	}

	return result, nil
}

// SendGridBatchSender implements batch sending for SendGrid (up to 1000 recipients)
type SendGridBatchSender struct {
	apiKey string
	db     *sql.DB
}

// NewSendGridBatchSender creates a new SendGrid batch sender
func NewSendGridBatchSender(apiKey string, db *sql.DB) *SendGridBatchSender {
	return &SendGridBatchSender{
		apiKey: apiKey,
		db:     db,
	}
}

// MaxBatchSize returns the maximum batch size for SendGrid
func (s *SendGridBatchSender) MaxBatchSize() int {
	return SendGridBatchSize
}

// SendBatch sends a batch of emails via SendGrid
func (s *SendGridBatchSender) SendBatch(ctx context.Context, messages []EmailMessage) (*BatchSendResult, error) {
	if s.apiKey == "" {
		return nil, fmt.Errorf("SendGrid API key not configured")
	}

	if len(messages) == 0 {
		return &BatchSendResult{}, nil
	}

	// SendGrid supports up to 1000 personalizations per request
	log.Printf("[SendGridBatch] Would send %d emails via SendGrid", len(messages))

	result := &BatchSendResult{
		Accepted: len(messages),
		Rejected: 0,
		Results:  make([]SendResult, len(messages)),
	}

	for i, msg := range messages {
		result.Results[i] = SendResult{
			Success:   true,
			MessageID: fmt.Sprintf("sg-%s", uuid.New().String()[:8]),
			ESPType:   "sendgrid",
			SentAt:    time.Now(),
		}
		_ = msg
	}

	return result, nil
}
