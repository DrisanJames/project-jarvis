package worker

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
	"github.com/lib/pq"
)

// SendWorkerPool manages a pool of workers for sending emails at scale
// Designed for 8.4M emails/day = 350K/hour = 5.8K/minute = ~100/second
// GlobalSuppressionChecker checks the global suppression hub (in-memory O(1)).
type GlobalSuppressionChecker interface {
	IsSuppressed(email string) bool
}

type SendWorkerPool struct {
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
	
	// ESP Senders (injected)
	sparkpostSender ESPSender
	sesSender       ESPSender
	mailgunSender   ESPSender
	sendgridSender  ESPSender

	// Global suppression hub (single source of truth)
	globalHub       GlobalSuppressionChecker

	// Tracking infrastructure
	trackingURL     string // Base URL for open/click/unsubscribe tracking
	trackingSecret  string // HMAC signing key for tracking tokens
	orgID           string // Organization ID for tracking data
}

// ESPSender interface for sending via different ESPs
type ESPSender interface {
	Send(ctx context.Context, msg *EmailMessage) (*SendResult, error)
}

// EmailMessage represents an email to be sent
type EmailMessage struct {
	ID            string
	CampaignID    string
	SubscriberID  string
	Email         string
	FromName      string
	FromEmail     string
	ReplyTo       string
	Subject       string
	HTMLContent   string
	TextContent   string
	PreviewText   string // Pre-header text (injected as hidden span before <body> content)
	ProfileID     string
	ESPType       string
	Metadata      map[string]interface{}
	Headers       map[string]string // Custom SMTP headers (List-Unsubscribe, X-Job, etc.)
}

// injectPreviewText prepends a hidden preheader span into the HTML body.
// This is the industry-standard technique for email preview text:
// a visually hidden span at the top of <body>, followed by whitespace padding
// to push any other preview content out of the preview pane.
func injectPreviewText(html, previewText string) string {
	if previewText == "" || html == "" {
		return html
	}

	preheaderHTML := fmt.Sprintf(
		`<div style="display:none;font-size:1px;color:#ffffff;line-height:1px;max-height:0px;max-width:0px;opacity:0;overflow:hidden;">%s</div>`,
		previewText,
	)

	// Insert after <body> tag if present
	bodyIdx := strings.Index(strings.ToLower(html), "<body")
	if bodyIdx >= 0 {
		// Find the closing > of the <body> tag
		closeIdx := strings.Index(html[bodyIdx:], ">")
		if closeIdx >= 0 {
			insertAt := bodyIdx + closeIdx + 1
			return html[:insertAt] + preheaderHTML + html[insertAt:]
		}
	}

	// No <body> tag — prepend to the HTML
	return preheaderHTML + html
}

// SendResult contains the result of a send attempt
type SendResult struct {
	Success   bool
	MessageID string
	Error     error
	ESPType   string
	SentAt    time.Time
}

// QueueItem represents an item from the send queue
type QueueItem struct {
	ID           uuid.UUID
	CampaignID   uuid.UUID
	SubscriberID uuid.UUID
	Email        string
	Subject      string
	HTMLContent  string
	TextContent  string
	PreviewText  string
	FromName     string
	FromEmail    string
	ReplyTo      string
	ProfileID    string
	ESPType      string
	Priority     int
}

// NewSendWorkerPool creates a new worker pool
func NewSendWorkerPool(db *sql.DB, numWorkers int) *SendWorkerPool {
	if numWorkers <= 0 {
		numWorkers = 50 // Default for ~100 emails/second with batching
	}
	
	return &SendWorkerPool{
		db:           db,
		workerID:     fmt.Sprintf("worker-%s", uuid.New().String()[:8]),
		numWorkers:   numWorkers,
		batchSize:    100, // Claim 100 items per batch
		pollInterval: 100 * time.Millisecond, // Poll frequently for low latency
	}
}

// SetGlobalSuppressionHub connects the worker pool to the global
// suppression single source of truth for pre-send checking.
func (p *SendWorkerPool) SetGlobalSuppressionHub(hub GlobalSuppressionChecker) {
	p.globalHub = hub
}

// SetESPSenders sets the ESP sender implementations
func (p *SendWorkerPool) SetESPSenders(sparkpost, ses, mailgun, sendgrid ESPSender) {
	p.sparkpostSender = sparkpost
	p.sesSender = ses
	p.mailgunSender = mailgun
	p.sendgridSender = sendgrid
}

// SetTrackingConfig configures tracking pixel/click/unsubscribe injection.
func (p *SendWorkerPool) SetTrackingConfig(trackingURL, trackingSecret, orgID string) {
	p.trackingURL = trackingURL
	p.trackingSecret = trackingSecret
	p.orgID = orgID
}

// Start begins the worker pool
func (p *SendWorkerPool) Start() {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	p.running = true
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.mu.Unlock()
	
	log.Printf("SendWorkerPool: Starting %d workers (batch_size=%d)", p.numWorkers, p.batchSize)
	
	// Register this worker
	p.registerWorker()
	
	// Start heartbeat
	go p.heartbeatLoop()
	
	// Start workers
	for i := 0; i < p.numWorkers; i++ {
		p.wg.Add(1)
		go p.worker(i)
	}
}

// Stop gracefully stops the worker pool
func (p *SendWorkerPool) Stop() {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return
	}
	p.running = false
	p.cancel()
	p.mu.Unlock()
	
	log.Println("SendWorkerPool: Stopping workers...")
	p.wg.Wait()
	
	// Deregister worker
	p.deregisterWorker()
	
	log.Printf("SendWorkerPool: Stopped. Total sent: %d, failed: %d, skipped: %d",
		atomic.LoadInt64(&p.totalSent), atomic.LoadInt64(&p.totalFailed), atomic.LoadInt64(&p.totalSkipped))
}

// Stats returns current statistics
func (p *SendWorkerPool) Stats() map[string]int64 {
	return map[string]int64{
		"total_sent":    atomic.LoadInt64(&p.totalSent),
		"total_failed":  atomic.LoadInt64(&p.totalFailed),
		"total_skipped": atomic.LoadInt64(&p.totalSkipped),
	}
}

// worker is the main worker loop
func (p *SendWorkerPool) worker(workerNum int) {
	defer p.wg.Done()
	
	for {
		select {
		case <-p.ctx.Done():
			return
		default:
			// Claim a batch of items
			items, err := p.claimBatch()
			if err != nil {
				log.Printf("Worker %d: Error claiming batch: %v", workerNum, err)
				time.Sleep(time.Second)
				continue
			}
			
			if len(items) == 0 {
				// No items available, wait before polling again
				time.Sleep(p.pollInterval)
				continue
			}
			
			// Process batch
			for _, item := range items {
				if err := p.processItem(item); err != nil {
					log.Printf("Worker %d: Error processing item %s: %v", workerNum, item.ID, err)
				}
			}
		}
	}
}

// claimBatch claims a batch of queue items
func (p *SendWorkerPool) claimBatch() ([]QueueItem, error) {
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()
	
	// Use the claim function from the database
	rows, err := p.db.QueryContext(ctx, `
		WITH claimed AS (
			UPDATE mailing_campaign_queue
			SET 
				status = 'sending',
				worker_id = $1,
				locked_at = NOW()
			WHERE id IN (
				SELECT q.id FROM mailing_campaign_queue q
				WHERE q.status = 'queued'
				  AND q.scheduled_at <= NOW()
				  AND (q.locked_at IS NULL OR q.locked_at < NOW() - INTERVAL '5 minutes')
				ORDER BY q.priority DESC, q.scheduled_at ASC
				LIMIT $2
				FOR UPDATE SKIP LOCKED
			)
			RETURNING id, campaign_id, subscriber_id, subject, html_content, plain_content
		)
		SELECT 
			c.id,
			c.campaign_id,
			c.subscriber_id,
			s.email,
			COALESCE(c.subject, ''),
			COALESCE(c.html_content, ''),
			COALESCE(c.plain_content, ''),
			COALESCE(camp.preview_text, ''),
			COALESCE(camp.from_name, ''),
			COALESCE(camp.from_email, ''),
			COALESCE(camp.reply_to, ''),
			COALESCE(camp.sending_profile_id::text, ''),
			COALESCE(sp.vendor_type, 'ses')
		FROM claimed c
		JOIN mailing_subscribers s ON s.id = c.subscriber_id
		JOIN mailing_campaigns camp ON camp.id = c.campaign_id
		LEFT JOIN mailing_sending_profiles sp ON sp.id = camp.sending_profile_id
	`, p.workerID, p.batchSize)
	
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	
	var items []QueueItem
	for rows.Next() {
		var item QueueItem
		var profileID, espType string
		err := rows.Scan(
			&item.ID,
			&item.CampaignID,
			&item.SubscriberID,
			&item.Email,
			&item.Subject,
			&item.HTMLContent,
			&item.TextContent,
			&item.PreviewText,
			&item.FromName,
			&item.FromEmail,
			&item.ReplyTo,
			&profileID,
			&espType,
		)
		if err != nil {
			continue
		}
		item.ProfileID = profileID
		item.ESPType = espType
		items = append(items, item)
	}
	
	return items, nil
}

// processItem processes a single queue item
func (p *SendWorkerPool) processItem(item QueueItem) error {
	ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
	defer cancel()

	// Load campaign's suppression list IDs and check suppression
	suppressionListIDs, err := p.getCampaignSuppressionListIDs(ctx, item.CampaignID)
	if err != nil {
		log.Printf("Error loading suppression lists for campaign %s: %v", item.CampaignID, err)
	}
	if len(suppressionListIDs) > 0 {
		suppressed, err := p.checkSuppression(ctx, item.Email, suppressionListIDs)
		if err != nil {
			log.Printf("Suppression check error for %s: %v", logger.RedactEmail(item.Email), err)
		}
		if suppressed {
			atomic.AddInt64(&p.totalSkipped, 1)
			return p.markSkipped(ctx, item.ID, "suppressed")
		}
	}

	// Global suppression hub — single source of truth (in-memory O(1))
	if p.globalHub != nil && p.globalHub.IsSuppressed(item.Email) {
		atomic.AddInt64(&p.totalSkipped, 1)
		return p.markSkipped(ctx, item.ID, "global_suppressed")
	}
	
	// Inject preview text (preheader) into HTML if provided
	htmlContent := item.HTMLContent
	if item.PreviewText != "" {
		htmlContent = injectPreviewText(htmlContent, item.PreviewText)
	}

	// Replace tracking link merge tags at send time
	htmlContent = replaceTrackingMergeTags(htmlContent, item.CampaignID.String(), item.SubscriberID.String())

	// Inject open pixel, click redirects, and unsubscribe link
	headers := make(map[string]string)
	if p.trackingURL != "" {
		htmlContent = p.injectTrackingPixelAndLinks(
			htmlContent,
			item.CampaignID.String(), item.SubscriberID.String(), item.ID.String(),
		)
		unsubURL := p.generateUnsubscribeURL(item.CampaignID.String(), item.SubscriberID.String())
		headers["List-Unsubscribe"] = fmt.Sprintf("<%s>", unsubURL)
		headers["List-Unsubscribe-Post"] = "List-Unsubscribe=One-Click"
	}
	headers["X-Job"] = item.CampaignID.String()

	// Build message
	msg := &EmailMessage{
		ID:           item.ID.String(),
		CampaignID:   item.CampaignID.String(),
		SubscriberID: item.SubscriberID.String(),
		Email:        item.Email,
		FromName:     item.FromName,
		FromEmail:    item.FromEmail,
		ReplyTo:      item.ReplyTo,
		Subject:      item.Subject,
		HTMLContent:  htmlContent,
		TextContent:  item.TextContent,
		PreviewText:  item.PreviewText,
		ProfileID:    item.ProfileID,
		ESPType:      item.ESPType,
		Headers:      headers,
	}
	
	// Select sender based on ESP type
	var sender ESPSender
	switch item.ESPType {
	case "sparkpost":
		sender = p.sparkpostSender
	case "ses":
		sender = p.sesSender
	case "mailgun":
		sender = p.mailgunSender
	case "sendgrid":
		sender = p.sendgridSender
	default:
		sender = p.sesSender // Default to SES
	}
	
	if sender == nil {
		atomic.AddInt64(&p.totalFailed, 1)
		return p.markFailed(ctx, item.ID, "no sender configured for "+item.ESPType)
	}
	
	// Send the email
	result, err := sender.Send(ctx, msg)
	if err != nil || !result.Success {
		atomic.AddInt64(&p.totalFailed, 1)
		errMsg := "unknown error"
		if err != nil {
			errMsg = err.Error()
		}
		return p.markFailed(ctx, item.ID, errMsg)
	}
	
	// Mark as sent and update campaign stats
	atomic.AddInt64(&p.totalSent, 1)
	if err := p.markSent(ctx, item, result.MessageID); err != nil {
		log.Printf("Error marking sent: %v", err)
	}
	
	// Update campaign sent count
	p.db.ExecContext(ctx, `SELECT update_campaign_stat($1, 'sent_count', 1)`, item.CampaignID)
	
	return nil
}

// checkSuppression checks if an email is suppressed against the given suppression lists.
// It always compares by MD5 hash, which handles both plaintext-email and MD5-only
// suppression entries (the md5_hash column is populated for all entries).
func (p *SendWorkerPool) checkSuppression(ctx context.Context, email string, suppressionListIDs []string) (bool, error) {
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

// getCampaignSuppressionListIDs loads the suppression_list_ids JSONB column
// from the mailing_campaigns table for the given campaign.
func (p *SendWorkerPool) getCampaignSuppressionListIDs(ctx context.Context, campaignID uuid.UUID) ([]string, error) {
	var raw sql.NullString
	err := p.db.QueryRowContext(ctx, `
		SELECT suppression_list_ids::text FROM mailing_campaigns WHERE id = $1
	`, campaignID).Scan(&raw)
	if err != nil {
		return nil, fmt.Errorf("load campaign suppression lists: %w", err)
	}
	if !raw.Valid || raw.String == "" || raw.String == "[]" || raw.String == "null" {
		return nil, nil
	}
	var listIDs []string
	if err := json.Unmarshal([]byte(raw.String), &listIDs); err != nil {
		return nil, fmt.Errorf("parse suppression_list_ids: %w", err)
	}
	return listIDs, nil
}

// markSent marks a queue item as sent
func (p *SendWorkerPool) markSent(ctx context.Context, item QueueItem, messageID string) error {
	_, err := p.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue 
		SET status = 'sent', message_id = $2, sent_at = NOW()
		WHERE id = $1
	`, item.ID, messageID)
	
	if err != nil {
		return err
	}
	
	// Log message for webhook correlation
	p.db.ExecContext(ctx, `
		INSERT INTO mailing_message_log (message_id, organization_id, campaign_id, subscriber_id, email, esp_type, sent_at)
		SELECT $1, camp.organization_id, $2, $3, $4, $5, NOW()
		FROM mailing_campaigns camp WHERE camp.id = $2
	`, messageID, item.CampaignID, item.SubscriberID, item.Email, item.ESPType)
	
	// Record tracking event
	p.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, event_type, event_at)
		SELECT uuid_generate_v4(), camp.organization_id, $1, $2, 'sent', NOW()
		FROM mailing_campaigns camp WHERE camp.id = $1
	`, item.CampaignID, item.SubscriberID)
	
	return nil
}

// markFailed marks a queue item as failed, or as dead_letter if max retries exceeded
func (p *SendWorkerPool) markFailed(ctx context.Context, itemID uuid.UUID, errMsg string) error {
	// Check current attempt count to decide between 'failed' and 'dead_letter'
	var attempts int
	_ = p.db.QueryRowContext(ctx, `
		SELECT COALESCE(attempts, 0) FROM mailing_campaign_queue WHERE id = $1
	`, itemID).Scan(&attempts)

	// If we've hit the max retry limit, move to dead_letter instead of failed
	if attempts+1 >= MaxRetryCount {
		_, err := p.db.ExecContext(ctx, `
			UPDATE mailing_campaign_queue 
			SET status = 'dead_letter', error_message = $2, attempts = attempts + 1, last_attempt_at = NOW()
			WHERE id = $1
		`, itemID, errMsg)
		if err == nil {
			log.Printf("[SendWorkerPool] Item %s moved to dead_letter after %d attempts", itemID, attempts+1)
		}
		return err
	}

	_, err := p.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue 
		SET status = 'failed', error_message = $2, attempts = attempts + 1, last_attempt_at = NOW()
		WHERE id = $1
	`, itemID, errMsg)
	return err
}

// markSkipped marks a queue item as skipped
func (p *SendWorkerPool) markSkipped(ctx context.Context, itemID uuid.UUID, reason string) error {
	_, err := p.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue 
		SET status = 'skipped', error_message = $2
		WHERE id = $1
	`, itemID, reason)
	return err
}

// registerWorker registers this worker in the database
func (p *SendWorkerPool) registerWorker() {
	p.db.Exec(`
		INSERT INTO mailing_workers (id, worker_type, hostname, status, max_concurrent, started_at, last_heartbeat_at)
		VALUES ($1, 'campaign_sender', $2, 'running', $3, NOW(), NOW())
		ON CONFLICT (id) DO UPDATE SET
			status = 'running',
			started_at = NOW(),
			last_heartbeat_at = NOW()
	`, p.workerID, getHostname(), p.numWorkers*p.batchSize)
}

// deregisterWorker removes this worker from the database
func (p *SendWorkerPool) deregisterWorker() {
	p.db.Exec(`UPDATE mailing_workers SET status = 'stopped' WHERE id = $1`, p.workerID)
}

// heartbeatLoop sends periodic heartbeats
func (p *SendWorkerPool) heartbeatLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			stats := p.Stats()
			statsJSON, _ := json.Marshal(stats)
			p.db.Exec(`
				UPDATE mailing_workers 
				SET 
					last_heartbeat_at = NOW(),
					total_processed = $2,
					total_errors = $3,
					metadata = $4
				WHERE id = $1
			`, p.workerID, stats["total_sent"], stats["total_failed"], string(statsJSON))
		}
	}
}

func getHostname() string {
	// Would normally use os.Hostname()
	return "ignite-worker"
}

// replaceTrackingMergeTags replaces Everflow tracking link merge tags at send time.
// {{DATE_MMDDYYYY}} -> current date in mmddYYYY format
// {{MAILING_ID}} -> the campaign/mailing ID (subscriber-specific for tracking)
func replaceTrackingMergeTags(html string, campaignID string, subscriberID string) string {
	now := time.Now()
	dateStr := fmt.Sprintf("%02d%02d%d", now.Month(), now.Day(), now.Year())
	
	html = strings.ReplaceAll(html, "{{DATE_MMDDYYYY}}", dateStr)
	html = strings.ReplaceAll(html, "{{MAILING_ID}}", subscriberID)
	
	return html
}

// ---------------------------------------------------------------------------
// Tracking injection: open pixel, click redirects, unsubscribe URL
// ---------------------------------------------------------------------------

var linkRe = regexp.MustCompile(`href=["'](https?://[^"']+)["']`)

func (p *SendWorkerPool) trackSign(data string) string {
	h := hmac.New(sha256.New, []byte(p.trackingSecret))
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (p *SendWorkerPool) injectTrackingPixelAndLinks(html, campaignID, subscriberID, emailID string) string {
	orgID := p.orgID
	data := fmt.Sprintf("%s|%s|%s|%s", orgID, campaignID, subscriberID, emailID)
	sig := p.trackSign(data)
	encoded := base64.URLEncoding.EncodeToString([]byte(data))

	pixel := fmt.Sprintf(`<img src="%s/track/open/%s/%s" width="1" height="1" alt="" style="display:none;width:1px;height:1px" />`, p.trackingURL, encoded, sig)
	if idx := strings.LastIndex(strings.ToLower(html), "</body>"); idx >= 0 {
		html = html[:idx] + pixel + html[idx:]
	} else {
		html += pixel
	}

	html = linkRe.ReplaceAllStringFunc(html, func(match string) string {
		parts := linkRe.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		origURL := parts[1]
		if strings.Contains(origURL, "/track/") || strings.Contains(origURL, "mailto:") {
			return match
		}
		linkData := fmt.Sprintf("%s|%s", data, origURL)
		linkSig := p.trackSign(linkData)
		linkEncoded := base64.URLEncoding.EncodeToString([]byte(linkData))
		return fmt.Sprintf(`href="%s/track/click/%s/%s"`, p.trackingURL, linkEncoded, linkSig)
	})

	return html
}

func (p *SendWorkerPool) generateUnsubscribeURL(campaignID, subscriberID string) string {
	data := fmt.Sprintf("%s|%s|%s", p.orgID, campaignID, subscriberID)
	sig := p.trackSign(data)
	encoded := base64.URLEncoding.EncodeToString([]byte(data))
	return fmt.Sprintf("%s/track/unsubscribe/%s/%s", p.trackingURL, encoded, sig)
}
