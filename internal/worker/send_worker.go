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
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
	"github.com/lib/pq"
)

// SendWorkerPool manages a pool of workers for sending emails at scale
// Designed for 8.4M emails/day = 350K/hour = 5.8K/minute = ~100/second
// GlobalSuppressionChecker checks the global suppression hub (in-memory O(1)).
type GlobalSuppressionChecker interface {
	IsSuppressed(email string) bool
}

// GlobalSuppressionSuppressor writes to the global suppression hub when
// bounces or complaints are detected during sending.
type GlobalSuppressionSuppressor interface {
	Suppress(ctx context.Context, email, reason, source, isp, dsnCode, dsnDiag, sourceIP, campaign string) (bool, error)
}

type SendWorkerPool struct {
	db           *sql.DB
	workerID     string
	numWorkers   int
	batchSize    int
	pollInterval time.Duration

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

	// ESP Senders (injected)
	sparkpostSender ESPSender
	sesSender       ESPSender
	mailgunSender   ESPSender
	sendgridSender  ESPSender

	// Global suppression hub (single source of truth)
	globalHub        GlobalSuppressionChecker
	globalSuppressor GlobalSuppressionSuppressor

	// Tracking infrastructure
	trackingURL    string // Base URL for open/click/unsubscribe tracking
	trackingSecret string // HMAC signing key for tracking tokens
	orgID          string // Organization ID for tracking data

	profileTrackingDomainCache map[string]string // profileID -> resolved tracking base URL
	ptdMu                      sync.RWMutex
}

// ESPSender interface for sending via different ESPs
type ESPSender interface {
	Send(ctx context.Context, msg *EmailMessage) (*SendResult, error)
}

// EmailMessage represents an email to be sent
type EmailMessage struct {
	ID           string
	CampaignID   string
	SubscriberID string
	Email        string
	FromName     string
	FromEmail    string
	ReplyTo      string
	Subject      string
	HTMLContent  string
	TextContent  string
	PreviewText  string // Pre-header text (injected as hidden span before <body> content)
	ProfileID    string
	ESPType      string
	Metadata     map[string]interface{}
	Headers      map[string]string // Custom SMTP headers (List-Unsubscribe, X-Job, etc.)
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
	FirstName    string
	LastName     string

	// Extended subscriber fields for full Liquid personalization
	CustomFields        mailing.JSON
	EngagementScore     float64
	TotalEmailsReceived int
	TotalOpens          int
	TotalClicks         int
	LastOpenAt          *time.Time
	LastClickAt         *time.Time
	LastEmailAt         *time.Time
	OptimalSendHourUTC  *int
	Timezone            string
	SubscriberStatus    string
	SubscriberSource    string
	SubscribedAt        time.Time

	// Campaign metadata for template context
	CampaignName string
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
		batchSize:    100,                    // Claim 100 items per batch
		pollInterval: 100 * time.Millisecond, // Poll frequently for low latency
	}
}

// SetGlobalSuppressionHub connects the worker pool to the global
// suppression single source of truth for pre-send checking.
func (p *SendWorkerPool) SetGlobalSuppressionHub(hub GlobalSuppressionChecker) {
	p.globalHub = hub
}

// SetGlobalSuppressionWriter connects the worker pool to the global
// suppression hub for writing bounces/complaints during send failures.
func (p *SendWorkerPool) SetGlobalSuppressionWriter(w GlobalSuppressionSuppressor) {
	p.globalSuppressor = w
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
	p.profileTrackingDomainCache = make(map[string]string)
}

// resolveTrackingURL returns the per-profile tracking base URL if one is
// configured, falling back to the global trackingURL.
func (p *SendWorkerPool) resolveTrackingURL(ctx context.Context, profileID string) string {
	if profileID == "" {
		return p.trackingURL
	}
	p.ptdMu.RLock()
	if cached, ok := p.profileTrackingDomainCache[profileID]; ok {
		p.ptdMu.RUnlock()
		return cached
	}
	p.ptdMu.RUnlock()

	var td, sd sql.NullString
	err := p.db.QueryRowContext(ctx,
		`SELECT COALESCE(tracking_domain,''), COALESCE(sending_domain,'') FROM mailing_sending_profiles WHERE id = $1`,
		profileID).Scan(&td, &sd)
	if err != nil {
		log.Printf("resolveTrackingURL: error loading profile %s: %v", profileID, err)
		return p.trackingURL
	}

	resolved := p.trackingURL
	source := "global"
	if td.Valid && td.String != "" {
		resolved = ensureHTTPSWorker(td.String)
		source = "profile_tracking_domain"
	} else if sd.Valid && sd.String != "" {
		resolved = ensureHTTPSWorker("trk." + sd.String)
		source = "derived_from_sending_domain"
	}
	log.Printf("resolveTrackingURL: profile=%s source=%s url=%s", profileID, source, resolved)

	p.ptdMu.Lock()
	p.profileTrackingDomainCache[profileID] = resolved
	p.ptdMu.Unlock()
	return resolved
}

func ensureHTTPSWorker(domainOrURL string) string {
	d := strings.TrimSpace(domainOrURL)
	if d == "" {
		return ""
	}
	if !strings.HasPrefix(d, "http") {
		d = "https://" + d
	}
	return strings.TrimRight(d, "/")
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
				JOIN mailing_campaigns camp ON camp.id = q.campaign_id
				WHERE q.status = 'queued'
				  AND camp.status = 'sending'
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
			COALESCE(sp.vendor_type, 'ses'),
			COALESCE(s.first_name, ''),
			COALESCE(s.last_name, ''),
			s.custom_fields,
			COALESCE(s.engagement_score, 0),
			COALESCE(s.total_emails_received, 0),
			COALESCE(s.total_opens, 0),
			COALESCE(s.total_clicks, 0),
			s.last_open_at,
			s.last_click_at,
			s.last_email_at,
			s.optimal_send_hour_utc,
			COALESCE(s.timezone, ''),
			COALESCE(s.status, 'confirmed'),
			COALESCE(s.source, ''),
			COALESCE(s.subscribed_at, s.created_at),
			COALESCE(camp.name, '')
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
			&item.FirstName,
			&item.LastName,
			&item.CustomFields,
			&item.EngagementScore,
			&item.TotalEmailsReceived,
			&item.TotalOpens,
			&item.TotalClicks,
			&item.LastOpenAt,
			&item.LastClickAt,
			&item.LastEmailAt,
			&item.OptimalSendHourUTC,
			&item.Timezone,
			&item.SubscriberStatus,
			&item.SubscriberSource,
			&item.SubscribedAt,
			&item.CampaignName,
		)
		if err != nil {
			log.Printf("SendWorkerPool: scan error: %v", err)
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

	// Gate: skip if campaign was cancelled or paused after claim
	var campStatus string
	if p.db.QueryRowContext(ctx, "SELECT status FROM mailing_campaigns WHERE id=$1", item.CampaignID).Scan(&campStatus) == nil {
		if campStatus == "cancelled" || campStatus == "paused" {
			return p.markSkipped(ctx, item.ID, "campaign_"+campStatus)
		}
	}

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

	// ── Personalization: full Liquid template engine with all subscriber data ──
	renderCtx := p.buildRenderContext(item)
	templateSvc := mailing.NewTemplateService()

	subject, _ := templateSvc.Render("s:"+item.CampaignID.String(), item.Subject, renderCtx)
	previewText, _ := templateSvc.Render("pv:"+item.CampaignID.String(), item.PreviewText, renderCtx)
	htmlContent, _ := templateSvc.Render("h:"+item.CampaignID.String()+":"+item.SubscriberID.String(), item.HTMLContent, renderCtx)
	textContent, _ := templateSvc.Render("t:"+item.CampaignID.String()+":"+item.SubscriberID.String(), item.TextContent, renderCtx)

	if previewText != "" {
		htmlContent = injectPreviewText(htmlContent, previewText)
	}

	htmlContent = replaceTrackingMergeTags(htmlContent, item.CampaignID.String(), item.SubscriberID.String())

	// ── Tracking + Unsubscribe ──
	headers := make(map[string]string)
	var unsubURL string
	trackBase := p.resolveTrackingURL(ctx, item.ProfileID)
	if trackBase != "" {
		htmlContent = p.injectTrackingPixelAndLinks(
			htmlContent,
			item.CampaignID.String(), item.SubscriberID.String(), item.ID.String(),
			trackBase,
		)
		unsubURL = p.generateUnsubscribeURL(item.CampaignID.String(), item.SubscriberID.String(), trackBase)
		headers["List-Unsubscribe"] = fmt.Sprintf("<%s>", unsubURL)
		headers["List-Unsubscribe-Post"] = "List-Unsubscribe=One-Click"

		// Replace {{ system.unsubscribe_url }} in body if present
		htmlContent = strings.ReplaceAll(htmlContent, "{{ system.unsubscribe_url }}", unsubURL)
		htmlContent = strings.ReplaceAll(htmlContent, "{{system.unsubscribe_url}}", unsubURL)

		// CAN-SPAM: if no unsub link exists in the body, inject one before </body>
		if !strings.Contains(strings.ToLower(htmlContent), "/track/unsubscribe/") {
			unsubBlock := fmt.Sprintf(
				`<div style="text-align:center;padding:16px;font-size:12px;color:#999;font-family:Arial,sans-serif;">`+
					`<a href="%s" style="color:#999;text-decoration:underline;">Unsubscribe</a></div>`, unsubURL)
			if idx := strings.LastIndex(strings.ToLower(htmlContent), "</body>"); idx >= 0 {
				htmlContent = htmlContent[:idx] + unsubBlock + htmlContent[idx:]
			} else {
				htmlContent += unsubBlock
			}
		}
	}
	headers["X-Job"] = item.CampaignID.String()

	// Feedback-ID enables Gmail FBL and aids ISP complaint attribution.
	feedbackDomain := item.FromEmail
	if atIdx := strings.LastIndex(item.FromEmail, "@"); atIdx >= 0 {
		feedbackDomain = item.FromEmail[atIdx+1:]
	}
	headers["Feedback-ID"] = fmt.Sprintf("%s:%s:%s:%s",
		item.CampaignID.String(), item.SubscriberID.String(), item.ID.String(), feedbackDomain)

	msg := &EmailMessage{
		ID:           item.ID.String(),
		CampaignID:   item.CampaignID.String(),
		SubscriberID: item.SubscriberID.String(),
		Email:        item.Email,
		FromName:     item.FromName,
		FromEmail:    item.FromEmail,
		ReplyTo:      item.ReplyTo,
		Subject:      subject,
		HTMLContent:  htmlContent,
		TextContent:  textContent,
		PreviewText:  previewText,
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

		p.recordBounce(ctx, item, errMsg)
		return p.markFailed(ctx, item.ID, errMsg)
	}

	// Mark as sent and update campaign stats
	atomic.AddInt64(&p.totalSent, 1)
	if err := p.markSent(ctx, item, result.MessageID); err != nil {
		log.Printf("Error marking sent: %v", err)
	}

	// Update campaign sent count and subscriber email count
	p.db.ExecContext(ctx, `SELECT update_campaign_stat($1, 'sent_count', 1)`, item.CampaignID)
	p.db.ExecContext(ctx, `UPDATE mailing_subscribers SET total_emails_received = COALESCE(total_emails_received, 0) + 1, updated_at = NOW() WHERE id = $1`, item.SubscriberID)

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
		INSERT INTO mailing_message_log (id, message_id, organization_id, campaign_id, subscriber_id, email, esp_type, sent_at)
		SELECT gen_random_uuid(), $1, camp.organization_id, $2, $3, $4, $5, NOW()
		FROM mailing_campaigns camp WHERE camp.id = $2
	`, messageID, item.CampaignID, item.SubscriberID, item.Email, item.ESPType)

	// Extract sending domain from the from_email address
	sendingDomain := ""
	if atIdx := strings.LastIndex(item.FromEmail, "@"); atIdx >= 0 {
		sendingDomain = strings.ToLower(item.FromEmail[atIdx+1:])
	}

	if _, trackErr := p.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, event_type, event_at, sending_domain)
		SELECT gen_random_uuid(), camp.organization_id, $1, $2, 'sent', NOW(), $3
		FROM mailing_campaigns camp WHERE camp.id = $1
	`, item.CampaignID, item.SubscriberID, sendingDomain); trackErr != nil {
		log.Printf("[send_worker] tracking event INSERT failed for campaign=%s sub=%s: %v", item.CampaignID, item.SubscriberID, trackErr)
	}

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

// recordBounce inserts a tracking event for the failed send and, for hard
// bounces, adds the address to the global suppression hub so it is never
// mailed again.
func (p *SendWorkerPool) recordBounce(ctx context.Context, item QueueItem, errMsg string) {
	bounceType := classifySendError(errMsg)
	sendingDomain := ""
	if atIdx := strings.LastIndex(item.FromEmail, "@"); atIdx >= 0 {
		sendingDomain = strings.ToLower(item.FromEmail[atIdx+1:])
	}
	recipientDomain := ""
	if atIdx := strings.LastIndex(item.Email, "@"); atIdx >= 0 {
		recipientDomain = strings.ToLower(item.Email[atIdx+1:])
	}

	_, dbErr := p.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events
			(id, organization_id, campaign_id, subscriber_id, event_type, bounce_type, bounce_reason, event_at, sending_domain, recipient_domain)
		VALUES ($1, $2, $3, $4, 'bounced', $5, $6, NOW(), $7, $8)
	`, uuid.New(), p.orgID, item.CampaignID, item.SubscriberID,
		bounceType, errMsg, sendingDomain, recipientDomain)
	if dbErr != nil {
		log.Printf("[SendWorkerPool] bounce tracking insert error: %v", dbErr)
	}

	p.db.ExecContext(ctx, `SELECT update_campaign_stat($1, 'bounce_count', 1)`, item.CampaignID)
	if bounceType == "hard" {
		p.db.ExecContext(ctx, `SELECT update_campaign_stat($1, 'hard_bounce_count', 1)`, item.CampaignID)
	} else {
		p.db.ExecContext(ctx, `SELECT update_campaign_stat($1, 'soft_bounce_count', 1)`, item.CampaignID)
	}

	if bounceType == "hard" && p.globalSuppressor != nil {
		ispGroup := recipientDomain
		if _, suppressErr := p.globalSuppressor.Suppress(
			ctx, item.Email, "hard_bounce", "send_worker",
			ispGroup, "", errMsg, "", item.CampaignID.String(),
		); suppressErr != nil {
			log.Printf("[SendWorkerPool] global suppress error for %s: %v",
				logger.RedactEmail(item.Email), suppressErr)
		}
	}
}

// classifySendError determines hard vs soft bounce from SMTP error text.
func classifySendError(errMsg string) string {
	lower := strings.ToLower(errMsg)
	hardIndicators := []string{
		"550", "551", "552", "553", "554",
		"user unknown", "mailbox not found", "does not exist",
		"no such user", "invalid recipient", "rejected",
		"permanently", "disabled", "deactivated",
	}
	for _, ind := range hardIndicators {
		if strings.Contains(lower, ind) {
			return "hard"
		}
	}
	return "soft"
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

func (p *SendWorkerPool) injectTrackingPixelAndLinks(html, campaignID, subscriberID, emailID, baseURL string) string {
	orgID := p.orgID
	data := fmt.Sprintf("%s|%s|%s|%s", orgID, campaignID, subscriberID, emailID)
	sig := p.trackSign(data)
	encoded := base64.URLEncoding.EncodeToString([]byte(data))

	pixel := fmt.Sprintf(`<img src="%s/track/open/%s/%s" width="1" height="1" alt="" style="display:none;width:1px;height:1px" />`, baseURL, encoded, sig)
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
		return fmt.Sprintf(`href="%s/track/click/%s/%s"`, baseURL, linkEncoded, linkSig)
	})

	return html
}

func (p *SendWorkerPool) generateUnsubscribeURL(campaignID, subscriberID, baseURL string) string {
	data := fmt.Sprintf("%s|%s|%s", p.orgID, campaignID, subscriberID)
	sig := p.trackSign(data)
	encoded := base64.URLEncoding.EncodeToString([]byte(data))
	return fmt.Sprintf("%s/track/unsubscribe/%s/%s", baseURL, encoded, sig)
}

// buildRenderContext constructs a full Liquid render context from a queue item,
// matching the schema produced by mailing.ContextBuilder.BuildContext but built
// from data already loaded in the claim query (no extra DB round-trips).
func (p *SendWorkerPool) buildRenderContext(item QueueItem) mailing.RenderContext {
	rc := make(mailing.RenderContext)

	// Top-level profile fields
	rc["first_name"] = item.FirstName
	rc["last_name"] = item.LastName
	rc["email"] = item.Email
	rc["full_name"] = strings.TrimSpace(item.FirstName + " " + item.LastName)
	if parts := strings.SplitN(item.Email, "@", 2); len(parts) == 2 {
		rc["email_local"] = parts[0]
		rc["email_domain"] = parts[1]
	}

	// Custom fields
	if item.CustomFields != nil {
		rc["custom"] = map[string]interface{}(item.CustomFields)
	} else {
		rc["custom"] = make(map[string]interface{})
	}

	// Engagement
	rc["engagement"] = map[string]interface{}{
		"score":             item.EngagementScore,
		"total_emails":      item.TotalEmailsReceived,
		"total_opens":       item.TotalOpens,
		"total_clicks":      item.TotalClicks,
		"subscribed_at":     item.SubscribedAt,
		"optimal_send_hour": item.OptimalSendHourUTC,
	}

	// System fields
	now := time.Now()
	system := map[string]interface{}{
		"current_date":    now.Format("January 2, 2006"),
		"current_year":    now.Year(),
		"current_month":   now.Month().String(),
		"current_day":     now.Day(),
		"current_weekday": now.Weekday().String(),
		"current_hour":    now.Hour(),
		"timestamp":       now.Unix(),
	}
	if p.trackingURL != "" {
		tBase := p.trackingURL
		system["unsubscribe_url"] = p.generateUnsubscribeURL(item.CampaignID.String(), item.SubscriberID.String(), tBase)
		system["preferences_url"] = fmt.Sprintf("%s/preferences?sid=%s", tBase, item.SubscriberID.String())
		system["view_in_browser_url"] = fmt.Sprintf("%s/view?cid=%s&sid=%s", tBase, item.CampaignID.String(), item.SubscriberID.String())
	}
	rc["system"] = system
	rc["now"] = now
	rc["today"] = now.Format("January 2, 2006")
	rc["year"] = now.Year()

	// Campaign metadata
	rc["campaign"] = map[string]interface{}{
		"id":           item.CampaignID.String(),
		"name":         item.CampaignName,
		"subject":      item.Subject,
		"preview_text": item.PreviewText,
		"from_name":    item.FromName,
		"from_email":   item.FromEmail,
	}
	rc["campaignId"] = item.CampaignID.String()
	rc["campaign_name"] = item.CampaignName

	// Subscriber metadata
	rc["subscriber"] = map[string]interface{}{
		"id":            item.SubscriberID.String(),
		"status":        item.SubscriberStatus,
		"source":        item.SubscriberSource,
		"timezone":      item.Timezone,
		"subscribed_at": item.SubscribedAt,
	}

	return rc
}

// personalizeContent replaces all merge-field variations with subscriber data.
// Handles {{ field }}, {{field}}, and [FIELD] syntaxes with case-insensitive matching.
// Kept as fallback; the primary path now uses the full Liquid TemplateService.
func personalizeContent(content, email, firstName, lastName string) string {
	if content == "" {
		return content
	}

	fullName := strings.TrimSpace(firstName + " " + lastName)
	emailLocal := email
	emailDomain := ""
	if at := strings.LastIndex(email, "@"); at >= 0 {
		emailLocal = email[:at]
		emailDomain = email[at+1:]
	}

	replacements := []struct {
		tags  []string
		value string
	}{
		{[]string{"{{ first_name }}", "{{first_name}}", "[FIRST_NAME]"}, firstName},
		{[]string{"{{ last_name }}", "{{last_name}}", "[LAST_NAME]"}, lastName},
		{[]string{"{{ full_name }}", "{{full_name}}", "[FULL_NAME]"}, fullName},
		{[]string{"{{ email }}", "{{email}}", "[EMAIL]", "{{EMAIL}}", "{{ EMAIL }}"}, email},
		{[]string{"{{ FIRST_NAME }}", "{{ LAST_NAME }}", "{{ FULL_NAME }}"}, ""},
		{[]string{"{{ email_local }}", "{{email_local}}"}, emailLocal},
		{[]string{"{{ email_domain }}", "{{email_domain}}"}, emailDomain},
	}
	// Apply the concrete-value replacements first
	replacements[4].tags = nil // skip the empty row
	for _, r := range replacements {
		for _, tag := range r.tags {
			if strings.Contains(content, tag) {
				content = strings.ReplaceAll(content, tag, r.value)
			}
		}
	}
	// Uppercase variants
	content = strings.ReplaceAll(content, "{{ FIRST_NAME }}", firstName)
	content = strings.ReplaceAll(content, "{{ LAST_NAME }}", lastName)
	content = strings.ReplaceAll(content, "{{ FULL_NAME }}", fullName)
	content = strings.ReplaceAll(content, "{{FIRST_NAME}}", firstName)
	content = strings.ReplaceAll(content, "{{LAST_NAME}}", lastName)
	content = strings.ReplaceAll(content, "{{FULL_NAME}}", fullName)

	return content
}
