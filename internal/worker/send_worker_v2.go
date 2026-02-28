package worker

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"

	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

// SendWorkerPoolV2 manages a pool of workers for sending emails at scale
// Uses the normalized queue schema (no HTML storage in queue)
// Content is fetched from campaigns table and merged with substitution data
type SendWorkerPoolV2 struct {
	db              *sql.DB
	workerID        string
	numWorkers      int
	batchSize       int
	pollInterval    time.Duration

	// Stats
	totalSent       int64
	totalFailed     int64
	totalSkipped    int64

	// Control
	ctx             context.Context
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	running         bool
	mu              sync.RWMutex

	// Rate limiter (optional)
	rateLimiter     *RateLimiter

	// Campaign content cache (reduces DB queries)
	contentCache    map[string]*CampaignContent
	cacheMu         sync.RWMutex

	// Profile-based sender
	profileSender   *ProfileBasedSender

	// Redis client for agent decision lookups
	redis           *redis.Client

	// Agent preprocessor (optional — enables AI-driven send decisions)
	agentPreprocessor *AgentPreprocessor
}

// CampaignContent holds the static content for a campaign
type CampaignContent struct {
	Subject            string
	HTMLContent        string
	TextContent        string
	FromName           string
	FromEmail          string
	ReplyTo            string
	ProfileID          string
	ESPType            string
	SuppressionListIDs []string // Campaign's selected suppression lists
	FetchedAt          time.Time
}

// QueueItemV2 represents an item from the normalized queue
type QueueItemV2 struct {
	ID              uuid.UUID
	CampaignID      uuid.UUID
	SubscriberID    uuid.UUID
	Email           string
	SubstitutionData map[string]interface{}
	Priority        int
}

// AgentDecisionCache is the Redis-cached decision for a single recipient.
// Populated by the agent preprocessor pipeline, read by the send worker.
type AgentDecisionCache struct {
	Classification  string `json:"classification"`   // suppress, defer, send_now, send_later
	ContentStrategy string `json:"content_strategy"`  // text_personalized, text_generic, image_personalized, image_generic
	Priority        int    `json:"priority"`
	OptimalSendHour int    `json:"optimal_send_hour"`
}

// NewSendWorkerPoolV2 creates a new worker pool using the normalized queue
func NewSendWorkerPoolV2(db *sql.DB, numWorkers int) *SendWorkerPoolV2 {
	if numWorkers <= 0 {
		numWorkers = 100 // Default workers for 50M/day capacity
	}

	return &SendWorkerPoolV2{
		db:            db,
		workerID:      fmt.Sprintf("worker-v2-%s", uuid.New().String()[:8]),
		numWorkers:    numWorkers,
		batchSize:     1000, // Batch size for 50M/day capacity
		pollInterval:  100 * time.Millisecond,
		contentCache:  make(map[string]*CampaignContent),
		profileSender: NewProfileBasedSender(db),
	}
}

// SetRateLimiter sets the rate limiter
func (p *SendWorkerPoolV2) SetRateLimiter(rl *RateLimiter) {
	p.rateLimiter = rl
}

// SetRedis sets the Redis client used for agent decision lookups
func (p *SendWorkerPoolV2) SetRedis(rc *redis.Client) {
	p.redis = rc
}

// SetAgentPreprocessor attaches the agent preprocessor to enable
// AI-driven per-recipient send decisions
func (p *SendWorkerPoolV2) SetAgentPreprocessor(ap *AgentPreprocessor) {
	p.agentPreprocessor = ap
}

// Start begins the worker pool
func (p *SendWorkerPoolV2) Start() {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	p.running = true
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.mu.Unlock()

	log.Printf("[SendWorkerPoolV2] Starting %d workers (batch_size=%d)", p.numWorkers, p.batchSize)

	// Start workers
	for i := 0; i < p.numWorkers; i++ {
		p.wg.Add(1)
		go p.worker(i)
	}

	// Start cache cleanup routine
	go p.cacheCleanup()
}

// Stop gracefully stops the worker pool
func (p *SendWorkerPoolV2) Stop() {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return
	}
	p.running = false
	p.cancel()
	p.mu.Unlock()

	log.Println("[SendWorkerPoolV2] Stopping workers...")
	p.wg.Wait()

	log.Printf("[SendWorkerPoolV2] Stopped. Total sent: %d, failed: %d, skipped: %d",
		atomic.LoadInt64(&p.totalSent), atomic.LoadInt64(&p.totalFailed), atomic.LoadInt64(&p.totalSkipped))
}

// Stats returns current statistics
func (p *SendWorkerPoolV2) Stats() map[string]int64 {
	return map[string]int64{
		"total_sent":    atomic.LoadInt64(&p.totalSent),
		"total_failed":  atomic.LoadInt64(&p.totalFailed),
		"total_skipped": atomic.LoadInt64(&p.totalSkipped),
	}
}

// worker is the main worker loop
func (p *SendWorkerPoolV2) worker(workerNum int) {
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
				time.Sleep(p.pollInterval)
				continue
			}

			// Process batch
			for _, item := range items {
				if err := p.processItem(item); err != nil {
					log.Printf("[Worker %d] Error processing item %s: %v", workerNum, item.ID, err)
				}
			}
		}
	}
}

// claimBatch claims a batch of queue items from the normalized queue
func (p *SendWorkerPoolV2) claimBatch() ([]QueueItemV2, error) {
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()

	// Claim items from the normalized queue (v2)
	// Content is NOT in the queue - only subscriber data
	rows, err := p.db.QueryContext(ctx, `
		WITH claimed AS (
			UPDATE mailing_campaign_queue_v2
			SET 
				status = 'sending',
				worker_id = $1,
				claimed_at = NOW()
			WHERE id IN (
				SELECT q.id FROM mailing_campaign_queue_v2 q
				WHERE q.status = 'queued'
				  AND q.scheduled_at <= NOW()
				ORDER BY q.priority DESC, q.scheduled_at ASC
				LIMIT $2
				FOR UPDATE SKIP LOCKED
			)
			RETURNING id, campaign_id, subscriber_id, email, substitution_data, priority
		)
		SELECT id, campaign_id, subscriber_id, email, 
			   COALESCE(substitution_data, '{}')::text, priority
		FROM claimed
	`, p.workerID, p.batchSize)

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []QueueItemV2
	for rows.Next() {
		var item QueueItemV2
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
			continue
		}

		// Parse substitution data
		if err := json.Unmarshal([]byte(subDataJSON), &item.SubstitutionData); err != nil {
			item.SubstitutionData = make(map[string]interface{})
		}

		items = append(items, item)
	}

	return items, nil
}

// getCampaignContent gets campaign content from cache or database
func (p *SendWorkerPoolV2) getCampaignContent(ctx context.Context, campaignID uuid.UUID) (*CampaignContent, error) {
	key := campaignID.String()

	// Check cache first
	p.cacheMu.RLock()
	if content, ok := p.contentCache[key]; ok {
		p.cacheMu.RUnlock()
		return content, nil
	}
	p.cacheMu.RUnlock()

	// Fetch from database (including suppression_list_ids)
	var content CampaignContent
	var profileID sql.NullString
	var suppressionJSON sql.NullString
	err := p.db.QueryRowContext(ctx, `
		SELECT 
			c.subject,
			c.html_content,
			COALESCE(c.plain_content, ''),
			c.from_name,
			c.from_email,
			COALESCE(c.reply_to, ''),
			c.sending_profile_id::text,
			COALESCE(sp.vendor_type, 'ses'),
			COALESCE(c.suppression_list_ids::text, '[]')
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
		&suppressionJSON,
	)

	if err != nil {
		return nil, fmt.Errorf("failed to fetch campaign content: %w", err)
	}

	if profileID.Valid {
		content.ProfileID = profileID.String
	}

	// Parse suppression list IDs from JSONB column
	if suppressionJSON.Valid && suppressionJSON.String != "" && suppressionJSON.String != "[]" && suppressionJSON.String != "null" {
		json.Unmarshal([]byte(suppressionJSON.String), &content.SuppressionListIDs)
	}

	content.FetchedAt = time.Now()

	// Cache it
	p.cacheMu.Lock()
	p.contentCache[key] = &content
	p.cacheMu.Unlock()

	return &content, nil
}

// processItem processes a single queue item
func (p *SendWorkerPoolV2) processItem(item QueueItemV2) error {
	ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
	defer cancel()

	// Get campaign content (from cache or DB)
	content, err := p.getCampaignContent(ctx, item.CampaignID)
	if err != nil {
		atomic.AddInt64(&p.totalFailed, 1)
		return p.markFailed(ctx, item.ID, err.Error())
	}

	// Check for agent decision (from Redis cache populated by AgentPreprocessor)
	var decision *AgentDecisionCache
	var emailHash string
	if p.redis != nil {
		emailHash = sha256Hex(strings.ToLower(item.Email))
		decision = p.getAgentDecision(ctx, item.CampaignID, emailHash)
	}

	if decision != nil {
		// Agent has a decision for this recipient
		switch decision.Classification {
		case "suppress":
			// Skip this recipient — agent says don't send
			atomic.AddInt64(&p.totalSkipped, 1)
			p.markSkipped(ctx, item.ID, "agent_suppress")
			return nil
		case "defer":
			// Re-queue for later or skip
			atomic.AddInt64(&p.totalSkipped, 1)
			p.markSkipped(ctx, item.ID, "agent_defer")
			return nil
		case "send_later":
			// Could re-queue with delayed time, for now treat as send_now
			// but log the preference
			log.Printf("[SendWorkerPoolV2] Agent suggests send_later for %s, proceeding anyway", logger.RedactEmail(item.Email))
		case "send_now":
			// Proceed with agent's content strategy
		}

		// Apply agent's content strategy (clone content to avoid mutating cache)
		if decision.ContentStrategy != "" {
			content = p.applyAgentContentStrategy(ctx, content, decision.ContentStrategy, item)
		}
	}

	// Check suppression against campaign's selected suppression lists
	if len(content.SuppressionListIDs) > 0 {
		suppressed, err := p.checkSuppression(ctx, item.Email, content.SuppressionListIDs)
		if err != nil {
			log.Printf("[SendWorkerPoolV2] Suppression check error for %s: %v", logger.RedactEmail(item.Email), err)
		}
		if suppressed {
			atomic.AddInt64(&p.totalSkipped, 1)
			return p.markSkipped(ctx, item.ID, "suppressed")
		}
	}

	// Check rate limits if limiter is configured
	if p.rateLimiter != nil {
		allowed, waitTime, err := p.rateLimiter.CheckAndIncrement(ctx, content.ESPType, 1)
		if err != nil {
			log.Printf("[SendWorkerPoolV2] Rate limit error: %v", err)
		}
		if !allowed && waitTime > 0 {
			// Return item to queue and wait
			p.returnToQueue(ctx, item.ID)
			time.Sleep(waitTime)
			return nil
		}
	}

	// Merge substitution data into content (personalization)
	subject := p.applySubstitutions(content.Subject, item.SubstitutionData)
	htmlContent := p.applySubstitutions(content.HTMLContent, item.SubstitutionData)
	textContent := p.applySubstitutions(content.TextContent, item.SubstitutionData)

	// Build message
	msg := &EmailMessage{
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
		ProfileID:    content.ProfileID,
		ESPType:      content.ESPType,
		Metadata:     item.SubstitutionData,
	}

	// Send using profile-based sender
	result, err := p.profileSender.Send(ctx, msg)
	if err != nil {
		atomic.AddInt64(&p.totalFailed, 1)
		return p.markFailed(ctx, item.ID, err.Error())
	}

	if !result.Success {
		atomic.AddInt64(&p.totalFailed, 1)
		errMsg := "send failed"
		if result.Error != nil {
			errMsg = result.Error.Error()
		}
		return p.markFailed(ctx, item.ID, errMsg)
	}

	// Success
	atomic.AddInt64(&p.totalSent, 1)

	// After successful send, mark agent decision as executed
	if decision != nil && p.redis != nil {
		campaignID := item.CampaignID
		hash := emailHash
		go func() {
			bgCtx := context.Background()
			p.db.ExecContext(bgCtx,
				`UPDATE mailing_agent_send_decisions SET executed = true, executed_at = NOW(), result = 'sent' WHERE campaign_id = $1 AND email_hash = $2 AND executed = false`,
				campaignID, hash,
			)
		}()
	}

	return p.markSent(ctx, item.ID, result.MessageID)
}

// sha256Hex returns the lowercase hex-encoded SHA-256 hash of s.
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

// getAgentDecision looks up a cached agent decision for this campaign+recipient
// from Redis. Returns nil if no decision exists (normal non-agent path).
func (p *SendWorkerPoolV2) getAgentDecision(ctx context.Context, campaignID uuid.UUID, emailHash string) *AgentDecisionCache {
	key := fmt.Sprintf("agent:decisions:%s:%s", campaignID, emailHash)

	val, err := p.redis.Get(ctx, key).Result()
	if err != nil {
		// No agent decision found — proceed with normal send path
		return nil
	}

	var decision AgentDecisionCache
	if err := json.Unmarshal([]byte(val), &decision); err != nil {
		log.Printf("[SendWorkerPoolV2] Failed to unmarshal agent decision for key %s: %v", key, err)
		return nil
	}
	return &decision
}

// applyAgentContentStrategy clones the campaign content and applies the
// agent's content strategy override. The cached original is never mutated.
func (p *SendWorkerPoolV2) applyAgentContentStrategy(ctx context.Context, content *CampaignContent, strategy string, item QueueItemV2) *CampaignContent {
	// Clone content to avoid mutating the cached version
	modified := *content

	switch strategy {
	case "text_personalized", "text_generic":
		// Prefer text content — if text version exists, clear HTML to force text-only send
		if modified.TextContent != "" {
			modified.HTMLContent = "" // Force text-only send
		}
	case "image_personalized", "image_generic":
		// Keep HTML content as-is (images are in HTML)
	}

	// Apply personalization boost if strategy includes "personalized"
	if strings.Contains(strategy, "personalized") {
		// Ensure substitution data includes personalization marker
		if item.SubstitutionData == nil {
			item.SubstitutionData = make(map[string]interface{})
		}
		item.SubstitutionData["personalized"] = true
	}

	return &modified
}

// checkSuppression checks if an email is suppressed against the given suppression lists.
// It always compares by MD5 hash, which handles both plaintext-email and MD5-only
// suppression entries (the md5_hash column is populated for all entries).
func (p *SendWorkerPoolV2) checkSuppression(ctx context.Context, email string, suppressionListIDs []string) (bool, error) {
	if len(suppressionListIDs) == 0 {
		return false, nil
	}

	// Compute MD5 of the lowercased, trimmed email (matches import logic)
	cleaned := strings.ToLower(strings.TrimSpace(email))
	hash := md5.Sum([]byte(cleaned))
	emailMD5 := hex.EncodeToString(hash[:])

	var exists bool
	err := p.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM mailing_suppression_entries
			WHERE md5_hash = $1
			  AND list_id = ANY($2)
		)
	`, emailMD5, pq.Array(suppressionListIDs)).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check suppression: %w", err)
	}
	return exists, nil
}

// markSkipped marks a queue item as skipped (suppressed)
func (p *SendWorkerPoolV2) markSkipped(ctx context.Context, id uuid.UUID, reason string) error {
	_, err := p.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue_v2
		SET status = 'skipped', error_code = $2
		WHERE id = $1
	`, id, reason)
	return err
}

// applySubstitutions replaces Liquid-style placeholders with actual values
func (p *SendWorkerPoolV2) applySubstitutions(template string, data map[string]interface{}) string {
	result := template
	for key, value := range data {
		placeholder := fmt.Sprintf("{{ %s }}", key)
		result = replaceAll(result, placeholder, fmt.Sprintf("%v", value))
		
		// Also handle without spaces
		placeholder2 := fmt.Sprintf("{{%s}}", key)
		result = replaceAll(result, placeholder2, fmt.Sprintf("%v", value))
	}
	return result
}

func replaceAll(s, old, new string) string {
	// Simple string replacement
	for {
		i := indexOf(s, old)
		if i < 0 {
			break
		}
		s = s[:i] + new + s[i+len(old):]
	}
	return s
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// markSent marks a queue item as sent
func (p *SendWorkerPoolV2) markSent(ctx context.Context, id uuid.UUID, messageID string) error {
	_, err := p.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue_v2
		SET status = 'sent', sent_at = NOW(), message_id = $2
		WHERE id = $1
	`, id, messageID)
	return err
}

// markFailed marks a queue item as failed, or as dead_letter if max retries exceeded
func (p *SendWorkerPoolV2) markFailed(ctx context.Context, id uuid.UUID, errorMsg string) error {
	truncated := errorMsg
	if len(truncated) > 50 {
		truncated = truncated[:50]
	}

	// Check current retry count to decide between 'failed' and 'dead_letter'
	var retryCount int
	_ = p.db.QueryRowContext(ctx, `
		SELECT COALESCE(retry_count, 0) FROM mailing_campaign_queue_v2 WHERE id = $1
	`, id).Scan(&retryCount)

	// If we've hit the max retry limit, move to dead_letter instead of failed
	if retryCount+1 >= MaxRetryCount {
		_, err := p.db.ExecContext(ctx, `
			UPDATE mailing_campaign_queue_v2
			SET status = 'dead_letter', error_code = $2, retry_count = retry_count + 1
			WHERE id = $1
		`, id, truncated)
		if err == nil {
			log.Printf("[SendWorkerPoolV2] Item %s moved to dead_letter after %d retries", id, retryCount+1)
		}
		return err
	}

	_, err := p.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue_v2
		SET status = 'failed', error_code = $2, retry_count = retry_count + 1
		WHERE id = $1
	`, id, truncated)
	return err
}

// returnToQueue returns an item to the queue for retry
func (p *SendWorkerPoolV2) returnToQueue(ctx context.Context, id uuid.UUID) error {
	_, err := p.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue_v2
		SET status = 'queued', worker_id = NULL, claimed_at = NULL
		WHERE id = $1
	`, id)
	return err
}

// cacheCleanup periodically cleans up old cache entries
func (p *SendWorkerPoolV2) cacheCleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.cacheMu.Lock()
			now := time.Now()
			for key, content := range p.contentCache {
				if now.Sub(content.FetchedAt) > 10*time.Minute {
					delete(p.contentCache, key)
				}
			}
			p.cacheMu.Unlock()
		}
	}
}

// min is now a builtin function in Go 1.21+
