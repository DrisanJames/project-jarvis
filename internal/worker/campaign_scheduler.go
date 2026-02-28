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
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

// =============================================================================
// CAMPAIGN SCHEDULER WORKER
// =============================================================================
// This worker polls for campaigns with status='scheduled' where scheduled_at
// has arrived, then enqueues all subscribers into mailing_campaign_queue for
// the SendWorkerPool to process.
//
// Key Features:
// - Minimum preparation time: 5 minutes before scheduled send
// - Edit lock window: Cannot edit within 5 minutes of scheduled send
// - Preparing status: Campaign moves to 'preparing' at 5-minute mark
// - Cancel/Pause always allowed, even during sending

const (
	// MinPreparationMinutes is the minimum time before a scheduled send
	// to allow for suppression checks, audience calculation, etc.
	MinPreparationMinutes = 5

	// EditLockMinutes is when the campaign becomes locked for editing
	// (same as preparation time)
	EditLockMinutes = 5

	// DefaultPollInterval is how often to check for scheduled campaigns
	DefaultSchedulerPollInterval = 30 * time.Second

	// EnqueueBatchSize is how many subscribers to enqueue at once (increased for 50M/day capacity)
	EnqueueBatchSize = 5000
)

// CampaignScheduler polls for scheduled campaigns and enqueues them for sending
type CampaignScheduler struct {
	db           *sql.DB
	redisClient  *redis.Client // optional; nil falls back to PG advisory locks
	workerID     string
	pollInterval time.Duration

	// Backpressure
	backpressure *BackpressureMonitor

	// Stats
	campaignsProcessed int64
	subscribersQueued  int64
	errors             int64

	// Control
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	running bool
	mu      sync.RWMutex
}

// ScheduledCampaign represents a campaign ready for processing
type ScheduledCampaign struct {
	ID                     uuid.UUID
	Name                   string
	Subject                string
	HTMLContent            string
	TextContent            string
	FromName               string
	FromEmail              string
	ListID                 sql.NullString
	SegmentID              sql.NullString
	ProfileID              sql.NullString
	ScheduledAt            time.Time
	ThrottleSpeed          string
	MaxRecipients          sql.NullInt64
	SuppressionLists       []string
	AISendTimeOptimization bool
}

// NewCampaignScheduler creates a new campaign scheduler
func NewCampaignScheduler(db *sql.DB) *CampaignScheduler {
	hostname := getHostname()
	return &CampaignScheduler{
		db:           db,
		workerID:     fmt.Sprintf("scheduler-%s-%d", hostname, time.Now().UnixNano()%10000),
		pollInterval: DefaultSchedulerPollInterval,
	}
}

// SetRedisClient sets the Redis client for distributed locking.
// If set, the scheduler uses Redis-based locks; otherwise it falls back
// to PostgreSQL advisory locks.
func (cs *CampaignScheduler) SetRedisClient(client *redis.Client) {
	cs.redisClient = client
}

// SetBackpressure sets the backpressure monitor used to pause enqueueing
// when the queue is too deep.
func (cs *CampaignScheduler) SetBackpressure(bp *BackpressureMonitor) {
	cs.backpressure = bp
}

// Start begins the scheduler polling loop
func (cs *CampaignScheduler) Start() error {
	cs.mu.Lock()
	if cs.running {
		cs.mu.Unlock()
		return fmt.Errorf("scheduler already running")
	}
	cs.running = true
	cs.ctx, cs.cancel = context.WithCancel(context.Background())
	cs.mu.Unlock()

	log.Printf("[CampaignScheduler] Starting with poll interval: %v", cs.pollInterval)

	// Register worker
	cs.registerWorker()

	// Start heartbeat
	cs.wg.Add(1)
	go cs.heartbeatLoop()

	// Start main scheduler loop
	cs.wg.Add(1)
	go cs.schedulerLoop()

	// Start preparation checker (moves campaigns to 'preparing' status)
	cs.wg.Add(1)
	go cs.preparationLoop()

	return nil
}

// Stop gracefully stops the scheduler
func (cs *CampaignScheduler) Stop() {
	cs.mu.Lock()
	if !cs.running {
		cs.mu.Unlock()
		return
	}
	cs.running = false
	cs.mu.Unlock()

	log.Printf("[CampaignScheduler] Stopping...")
	cs.cancel()
	cs.wg.Wait()
	cs.deregisterWorker()
	log.Printf("[CampaignScheduler] Stopped. Processed: %d campaigns, Queued: %d subscribers",
		atomic.LoadInt64(&cs.campaignsProcessed), atomic.LoadInt64(&cs.subscribersQueued))
}

// schedulerLoop is the main loop that polls for campaigns ready to send
func (cs *CampaignScheduler) schedulerLoop() {
	defer cs.wg.Done()

	ticker := time.NewTicker(cs.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-cs.ctx.Done():
			return
		case <-ticker.C:
			cs.processReadyCampaigns()
		}
	}
}

// preparationLoop checks for campaigns that need to move to 'preparing' status
func (cs *CampaignScheduler) preparationLoop() {
	defer cs.wg.Done()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-cs.ctx.Done():
			return
		case <-ticker.C:
			cs.markCampaignsAsPreparing()
			cs.checkCompletedCampaigns() // Also check for completed campaigns
		}
	}
}

// checkCompletedCampaigns checks for 'sending' campaigns where all queue items are processed
func (cs *CampaignScheduler) checkCompletedCampaigns() {
	ctx, cancel := context.WithTimeout(cs.ctx, 30*time.Second)
	defer cancel()
	
	// Find campaigns in 'sending' status where all queue items are done
	rows, err := cs.db.QueryContext(ctx, `
		SELECT c.id, 
			   COALESCE(SUM(CASE WHEN q.status = 'sent' THEN 1 ELSE 0 END), 0) as sent,
			   COALESCE(SUM(CASE WHEN q.status = 'failed' THEN 1 ELSE 0 END), 0) as failed,
			   COALESCE(SUM(CASE WHEN q.status = 'skipped' THEN 1 ELSE 0 END), 0) as skipped,
			   COALESCE(SUM(CASE WHEN q.status = 'pending' OR q.status = 'claimed' THEN 1 ELSE 0 END), 0) as pending,
			   COUNT(q.id) as total
		FROM mailing_campaigns c
		LEFT JOIN mailing_campaign_queue q ON q.campaign_id = c.id
		WHERE c.status = 'sending'
		GROUP BY c.id
		HAVING COALESCE(SUM(CASE WHEN q.status = 'pending' OR q.status = 'claimed' THEN 1 ELSE 0 END), 0) = 0
		   AND COUNT(q.id) > 0
	`)
	if err != nil {
		return
	}
	defer rows.Close()
	
	for rows.Next() {
		var campaignID uuid.UUID
		var sent, failed, skipped, pending, total int
		if err := rows.Scan(&campaignID, &sent, &failed, &skipped, &pending, &total); err != nil {
			continue
		}
		
		// Determine final status
		var finalStatus string
		if failed > 0 && sent > 0 {
			finalStatus = "completed_with_errors"
		} else if failed == total {
			finalStatus = "failed"
		} else {
			finalStatus = "completed"
		}
		
		// Update campaign
		_, err := cs.db.ExecContext(ctx, `
			UPDATE mailing_campaigns 
			SET status = $2, 
				sent_count = $3,
				completed_at = NOW(),
				updated_at = NOW()
			WHERE id = $1
		`, campaignID, finalStatus, sent)
		
		if err == nil {
			log.Printf("[CampaignScheduler] Campaign %s marked as %s (sent: %d, failed: %d, skipped: %d)", 
				campaignID, finalStatus, sent, failed, skipped)
		}
	}
}

// markCampaignsAsPreparing moves scheduled campaigns to 'preparing' status
// when they are within the edit lock window
func (cs *CampaignScheduler) markCampaignsAsPreparing() {
	ctx, cancel := context.WithTimeout(cs.ctx, 10*time.Second)
	defer cancel()

	// Find campaigns scheduled within the next EditLockMinutes that are still 'scheduled'
	// and move them to 'preparing'
	// NOTE: We check BOTH scheduled_at and send_at for backwards compatibility
	result, err := cs.db.ExecContext(ctx, `
		UPDATE mailing_campaigns
		SET status = 'preparing', updated_at = NOW()
		WHERE status = 'scheduled'
		  AND COALESCE(scheduled_at, send_at) <= NOW() + INTERVAL '`+fmt.Sprintf("%d", EditLockMinutes)+` minutes'
		  AND COALESCE(scheduled_at, send_at) > NOW()
	`)

	if err != nil {
		log.Printf("[CampaignScheduler] Error marking campaigns as preparing: %v", err)
		return
	}

	if count, _ := result.RowsAffected(); count > 0 {
		log.Printf("[CampaignScheduler] Moved %d campaigns to 'preparing' status", count)
	}
}

// processReadyCampaigns finds campaigns ready to send and enqueues them
func (cs *CampaignScheduler) processReadyCampaigns() {
	ctx, cancel := context.WithTimeout(cs.ctx, 60*time.Second)
	defer cancel()

	// Find campaigns that are scheduled (or preparing) and their time has arrived
	// NOTE: We check BOTH scheduled_at and send_at for backwards compatibility
	// - scheduled_at is the preferred column (used by campaign_builder.go)
	// - send_at is used by legacy code (store.go, some scripts)
	rows, err := cs.db.QueryContext(ctx, `
		SELECT 
			id, name, subject, 
			COALESCE(html_content, ''), COALESCE(plain_content, ''),
			COALESCE(from_name, ''), COALESCE(from_email, ''),
			list_id, segment_id, sending_profile_id,
			COALESCE(scheduled_at, send_at), COALESCE(throttle_speed, 'gentle'),
			max_recipients, COALESCE(ai_send_time_optimization, false)
		FROM mailing_campaigns
		WHERE status IN ('scheduled', 'preparing')
		  AND COALESCE(scheduled_at, send_at) <= NOW()
		ORDER BY COALESCE(scheduled_at, send_at) ASC
		LIMIT 10
	`)

	if err != nil {
		log.Printf("[CampaignScheduler] Error fetching ready campaigns: %v", err)
		atomic.AddInt64(&cs.errors, 1)
		return
	}
	defer rows.Close()

	var campaigns []ScheduledCampaign
	for rows.Next() {
		var c ScheduledCampaign
		err := rows.Scan(
			&c.ID, &c.Name, &c.Subject,
			&c.HTMLContent, &c.TextContent,
			&c.FromName, &c.FromEmail,
			&c.ListID, &c.SegmentID, &c.ProfileID,
			&c.ScheduledAt, &c.ThrottleSpeed,
			&c.MaxRecipients, &c.AISendTimeOptimization,
		)
		if err != nil {
			log.Printf("[CampaignScheduler] Error scanning campaign: %v", err)
			continue
		}
		campaigns = append(campaigns, c)
	}

	// Process each campaign
	for _, campaign := range campaigns {
		cs.processCampaign(ctx, campaign)
	}
}

// processCampaign enqueues all subscribers for a single campaign
func (cs *CampaignScheduler) processCampaign(ctx context.Context, campaign ScheduledCampaign) {
	// Check backpressure — if the queue is too deep, defer this campaign.
	// The scheduler will retry on the next poll cycle. We don't mark the
	// campaign as failed so it remains in 'scheduled'/'preparing' state.
	if cs.backpressure != nil && cs.backpressure.IsPaused() {
		log.Printf("[CampaignScheduler] Campaign %s enqueue deferred — backpressure active (queue depth: %d)",
			campaign.ID, cs.backpressure.QueueDepth())
		return
	}

	// Acquire distributed lock to prevent duplicate processing across workers
	lock := distlock.NewLock(cs.redisClient, cs.db, fmt.Sprintf("campaign:%s", campaign.ID), 10*time.Minute)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[CampaignScheduler] Error acquiring lock for campaign %s: %v", campaign.ID, err)
		return
	}
	if !acquired {
		log.Printf("[CampaignScheduler] Campaign %s already being processed by another worker", campaign.ID)
		return
	}
	defer lock.Release(ctx)

	log.Printf("[CampaignScheduler] Processing campaign: %s (%s)", campaign.Name, campaign.ID)

	// Lock the campaign for processing (set status to 'sending')
	result, err := cs.db.ExecContext(ctx, `
		UPDATE mailing_campaigns 
		SET status = 'sending', started_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status IN ('scheduled', 'preparing')
	`, campaign.ID)

	if err != nil {
		log.Printf("[CampaignScheduler] Error locking campaign %s: %v", campaign.ID, err)
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		log.Printf("[CampaignScheduler] Campaign %s was already processed or cancelled", campaign.ID)
		return
	}

	// Get total recipient count
	totalRecipients, err := cs.getRecipientCount(ctx, campaign)
	if err != nil {
		log.Printf("[CampaignScheduler] Error getting recipient count for %s: %v", campaign.ID, err)
		cs.markCampaignFailed(ctx, campaign.ID, "Failed to get recipients")
		return
	}

	if totalRecipients == 0 {
		log.Printf("[CampaignScheduler] No recipients for campaign %s", campaign.ID)
		cs.markCampaignCompleted(ctx, campaign.ID, 0)
		return
	}

	log.Printf("[CampaignScheduler] Campaign %s has %d recipients to enqueue", campaign.ID, totalRecipients)

	// Update campaign with total recipients
	cs.db.ExecContext(ctx, `
		UPDATE mailing_campaigns SET total_recipients = $1, updated_at = NOW() WHERE id = $2
	`, totalRecipients, campaign.ID)

	// Enqueue subscribers in batches
	queued, err := cs.enqueueSubscribers(ctx, campaign)
	if err != nil {
		log.Printf("[CampaignScheduler] Error enqueuing subscribers for %s: %v", campaign.ID, err)
		atomic.AddInt64(&cs.errors, 1)
		return
	}

	atomic.AddInt64(&cs.campaignsProcessed, 1)
	atomic.AddInt64(&cs.subscribersQueued, int64(queued))

	log.Printf("[CampaignScheduler] Campaign %s enqueued: %d subscribers", campaign.ID, queued)
}

// getRecipientCount returns the number of recipients for a campaign
func (cs *CampaignScheduler) getRecipientCount(ctx context.Context, campaign ScheduledCampaign) (int, error) {
	var count int

	if campaign.SegmentID.Valid {
		// Count from segment (may span multiple lists)
		err := cs.db.QueryRowContext(ctx, `
			SELECT COUNT(DISTINCT s.id)
			FROM mailing_subscribers s
			JOIN mailing_segment_conditions sc ON sc.segment_id = $1
			WHERE s.list_id = ANY(
				SELECT DISTINCT list_id FROM mailing_segment_conditions WHERE segment_id = $1
			)
			AND s.status = 'confirmed'
		`, campaign.SegmentID.String).Scan(&count)
		if err != nil {
			// Fallback: direct segment subscriber count
			cs.db.QueryRowContext(ctx, `
				SELECT COALESCE(subscriber_count, 0) FROM mailing_segments WHERE id = $1
			`, campaign.SegmentID.String).Scan(&count)
		}
	} else if campaign.ListID.Valid {
		// Count from list
		err := cs.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM mailing_subscribers 
			WHERE list_id = $1 AND status = 'confirmed'
		`, campaign.ListID.String).Scan(&count)
		if err != nil {
			return 0, err
		}
	}

	// Apply max recipients limit if set
	if campaign.MaxRecipients.Valid && campaign.MaxRecipients.Int64 > 0 && int64(count) > campaign.MaxRecipients.Int64 {
		count = int(campaign.MaxRecipients.Int64)
	}

	return count, nil
}

// enqueueSubscribers adds all subscribers to the send queue
func (cs *CampaignScheduler) enqueueSubscribers(ctx context.Context, campaign ScheduledCampaign) (int, error) {
	// =================================================================
	// Load suppression list IDs from the JSONB column on mailing_campaigns.
	// This is the source of truth — campaign builder stores them here,
	// and the send worker also reads from here for its per-email check.
	// =================================================================
	var suppressionListIDs []string
	var rawSuppJSON sql.NullString
	cs.db.QueryRowContext(ctx, `
		SELECT suppression_list_ids::text FROM mailing_campaigns WHERE id = $1
	`, campaign.ID).Scan(&rawSuppJSON)
	if rawSuppJSON.Valid && rawSuppJSON.String != "" && rawSuppJSON.String != "[]" && rawSuppJSON.String != "null" {
		json.Unmarshal([]byte(rawSuppJSON.String), &suppressionListIDs)
	}
	
	if len(suppressionListIDs) > 0 {
		log.Printf("[CampaignScheduler] Campaign %s: filtering against %d suppression lists at enqueue time", campaign.ID, len(suppressionListIDs))
	}

	// Build query to get subscribers
	var query string
	var args []interface{}

	if campaign.SegmentID.Valid {
		// Get segment conditions and build dynamic query
		var conditionsJSON []byte
		var segmentListID sql.NullString
		err := cs.db.QueryRowContext(ctx, `
			SELECT conditions, list_id FROM mailing_segments WHERE id = $1
		`, campaign.SegmentID.String).Scan(&conditionsJSON, &segmentListID)
		if err != nil {
			return 0, fmt.Errorf("failed to get segment conditions: %w", err)
		}
		
		// Parse conditions
		var conditions []struct {
			Field    string `json:"field"`
			Operator string `json:"operator"`
			Value    string `json:"value"`
			Group    int    `json:"group"`
		}
		if len(conditionsJSON) > 0 {
			json.Unmarshal(conditionsJSON, &conditions)
		}
		
		// Build base query — check BOTH global suppressions AND named suppression lists
		query = `
			SELECT DISTINCT s.id, s.email
			FROM mailing_subscribers s
			WHERE s.status = 'confirmed'
			AND NOT EXISTS (
				SELECT 1 FROM mailing_suppressions sup 
				WHERE LOWER(sup.email) = LOWER(s.email) AND sup.active = true
			)
		`
		args = []interface{}{}
		argIdx := 1
		
		// Add named suppression list filter (Sam's Club, Optizmo, etc.)
		if len(suppressionListIDs) > 0 {
			query += fmt.Sprintf(`
				AND NOT EXISTS (
					SELECT 1 FROM mailing_suppression_entries se
					WHERE se.md5_hash = MD5(LOWER(TRIM(s.email)))
					  AND se.list_id = ANY($%d)
				)
			`, argIdx)
			args = append(args, pq.Array(suppressionListIDs))
			argIdx++
		}
		
		// Add list_id filter if segment has one
		if segmentListID.Valid && segmentListID.String != "" {
			query += fmt.Sprintf(" AND s.list_id = $%d", argIdx)
			args = append(args, segmentListID.String)
			argIdx++
		}
		
		// Add condition filters
		for _, cond := range conditions {
			col := cs.mapFieldToColumn(cond.Field)
			switch cond.Operator {
			case "equals", "is":
				query += fmt.Sprintf(" AND %s = $%d", col, argIdx)
				args = append(args, cond.Value)
				argIdx++
			case "not_equals", "is_not":
				query += fmt.Sprintf(" AND %s != $%d", col, argIdx)
				args = append(args, cond.Value)
				argIdx++
			case "contains":
				query += fmt.Sprintf(" AND %s ILIKE $%d", col, argIdx)
				args = append(args, "%"+cond.Value+"%")
				argIdx++
			case "not_contains":
				query += fmt.Sprintf(" AND %s NOT ILIKE $%d", col, argIdx)
				args = append(args, "%"+cond.Value+"%")
				argIdx++
			case "starts_with":
				query += fmt.Sprintf(" AND %s ILIKE $%d", col, argIdx)
				args = append(args, cond.Value+"%")
				argIdx++
			case "ends_with":
				query += fmt.Sprintf(" AND %s ILIKE $%d", col, argIdx)
				args = append(args, "%"+cond.Value)
				argIdx++
			case "is_empty", "is_blank":
				query += fmt.Sprintf(" AND (%s IS NULL OR %s = '')", col, col)
			case "is_not_empty", "is_present":
				query += fmt.Sprintf(" AND %s IS NOT NULL AND %s != ''", col, col)
			case "greater_than", ">":
				query += fmt.Sprintf(" AND CAST(%s AS NUMERIC) > $%d", col, argIdx)
				args = append(args, cond.Value)
				argIdx++
			case "less_than", "<":
				query += fmt.Sprintf(" AND CAST(%s AS NUMERIC) < $%d", col, argIdx)
				args = append(args, cond.Value)
				argIdx++
			case "greater_or_equal", ">=":
				query += fmt.Sprintf(" AND CAST(%s AS NUMERIC) >= $%d", col, argIdx)
				args = append(args, cond.Value)
				argIdx++
			case "less_or_equal", "<=":
				query += fmt.Sprintf(" AND CAST(%s AS NUMERIC) <= $%d", col, argIdx)
				args = append(args, cond.Value)
				argIdx++
			}
		}
		
		query += " ORDER BY s.id"
	} else if campaign.ListID.Valid {
		// Build list-based query with both global + named suppression checks
		if len(suppressionListIDs) > 0 {
			query = `
				SELECT s.id, s.email
				FROM mailing_subscribers s
				WHERE s.list_id = $1 
				AND s.status = 'confirmed'
				AND NOT EXISTS (
					SELECT 1 FROM mailing_suppressions sup 
					WHERE LOWER(sup.email) = LOWER(s.email) AND sup.active = true
				)
				AND NOT EXISTS (
					SELECT 1 FROM mailing_suppression_entries se
					WHERE se.md5_hash = MD5(LOWER(TRIM(s.email)))
					  AND se.list_id = ANY($2)
				)
				ORDER BY s.id
			`
			args = []interface{}{campaign.ListID.String, pq.Array(suppressionListIDs)}
		} else {
			query = `
				SELECT s.id, s.email
				FROM mailing_subscribers s
				WHERE s.list_id = $1 
				AND s.status = 'confirmed'
				AND NOT EXISTS (
					SELECT 1 FROM mailing_suppressions sup 
					WHERE LOWER(sup.email) = LOWER(s.email) AND sup.active = true
				)
				ORDER BY s.id
			`
			args = []interface{}{campaign.ListID.String}
		}
	} else {
		return 0, fmt.Errorf("campaign has no list or segment")
	}

	// Apply limit if set
	if campaign.MaxRecipients.Valid && campaign.MaxRecipients.Int64 > 0 {
		query += fmt.Sprintf(" LIMIT %d", campaign.MaxRecipients.Int64)
	}

	rows, err := cs.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to get subscribers: %w", err)
	}
	defer rows.Close()

	// Collect subscribers
	var subscribers []subscriberForScheduling
	for rows.Next() {
		var s subscriberForScheduling
		if err := rows.Scan(&s.ID, &s.Email); err != nil {
			continue
		}
		subscribers = append(subscribers, s)
	}

	if len(subscribers) == 0 {
		return 0, nil
	}

	// Calculate priority based on throttle speed
	priority := 5 // default
	switch campaign.ThrottleSpeed {
	case "instant":
		priority = 10
	case "gentle":
		priority = 7
	case "moderate":
		priority = 5
	case "careful":
		priority = 3
	}

	// =================================================================
	// A/B VARIANT ASSIGNMENT
	// Load variants from mailing_ab_variants if this campaign has an
	// A/B test. Each subscriber gets round-robin assigned to a variant,
	// which overrides the campaign's default subject/content.
	// =================================================================
	type abVariant struct {
		ID          string
		Subject     string
		HTMLContent string
		TextContent string
		FromName    string
	}
	var abVariants []abVariant

	abRows, abErr := cs.db.QueryContext(ctx, `
		SELECT v.id::text, COALESCE(v.subject, ''), COALESCE(v.html_content, ''),
		       COALESCE(v.text_content, ''), COALESCE(v.from_name, '')
		FROM mailing_ab_variants v
		JOIN mailing_ab_tests t ON t.id = v.test_id
		WHERE t.campaign_id = $1
		  AND v.status != 'eliminated'
		ORDER BY v.variant_name
	`, campaign.ID)
	if abErr == nil {
		defer abRows.Close()
		for abRows.Next() {
			var v abVariant
			if err := abRows.Scan(&v.ID, &v.Subject, &v.HTMLContent, &v.TextContent, &v.FromName); err == nil {
				// Only include variants that have content
				if v.Subject != "" || v.HTMLContent != "" {
					abVariants = append(abVariants, v)
				}
			}
		}
	}

	if len(abVariants) > 0 {
		log.Printf("[CampaignScheduler] Campaign %s: A/B test with %d active variants — assigning round-robin", campaign.ID, len(abVariants))
	}

	// Get per-subscriber optimal send times if AI optimization is enabled
	optimalTimes := make(map[uuid.UUID]time.Time)
	if campaign.AISendTimeOptimization {
		optimalTimes = cs.getSubscriberOptimalTimes(ctx, campaign, subscribers)
		log.Printf("[CampaignScheduler] AI Send Time Optimization enabled for campaign %s, got %d optimal times", 
			campaign.ID, len(optimalTimes))
	}

	// Enqueue in batches
	queued := 0
	variantIdx := 0

	for i := 0; i < len(subscribers); i += EnqueueBatchSize {
		end := i + EnqueueBatchSize
		if end > len(subscribers) {
			end = len(subscribers)
		}
		batch := subscribers[i:end]

		// Check if campaign was cancelled
		var status string
		cs.db.QueryRowContext(ctx, `SELECT status FROM mailing_campaigns WHERE id = $1`, campaign.ID).Scan(&status)
		if status == "cancelled" || status == "paused" {
			log.Printf("[CampaignScheduler] Campaign %s was %s, stopping enqueue", campaign.ID, status)
			break
		}

		// Insert batch into queue
		for _, sub := range batch {
			queueID := uuid.New()
			
			// Determine scheduled time - use optimal time if available, otherwise NOW()
			var scheduledAt time.Time
			if optTime, ok := optimalTimes[sub.ID]; ok && campaign.AISendTimeOptimization {
				scheduledAt = optTime
			} else {
				scheduledAt = time.Now()
			}

			// Select subject/content — use A/B variant if available, otherwise campaign defaults
			subject := campaign.Subject
			htmlContent := campaign.HTMLContent
			textContent := campaign.TextContent

			if len(abVariants) > 0 {
				v := abVariants[variantIdx%len(abVariants)]
				if v.Subject != "" {
					subject = v.Subject
				}
				if v.HTMLContent != "" {
					htmlContent = v.HTMLContent
				}
				if v.TextContent != "" {
					textContent = v.TextContent
				}
				variantIdx++

				// Record the variant assignment for tracking
				cs.db.ExecContext(ctx, `
					INSERT INTO mailing_ab_assignments (id, test_id, variant_id, subscriber_id, assigned_at)
					SELECT gen_random_uuid(), t.id, $1, $2, NOW()
					FROM mailing_ab_tests t WHERE t.campaign_id = $3
					ON CONFLICT DO NOTHING
				`, v.ID, sub.ID, campaign.ID)
			}
			
			_, err := cs.db.ExecContext(ctx, `
				INSERT INTO mailing_campaign_queue (
					id, campaign_id, subscriber_id, 
					subject, html_content, plain_content,
					status, priority, scheduled_at, created_at
				) VALUES (
					$1, $2, $3,
					$4, $5, $6,
					'queued', $7, $8, NOW()
				)
				ON CONFLICT DO NOTHING
			`,
				queueID, campaign.ID, sub.ID,
				subject, htmlContent, textContent,
				priority, scheduledAt,
			)

			if err == nil {
				queued++
			}
		}

		// Update progress
		cs.db.ExecContext(ctx, `
			UPDATE mailing_campaigns SET queued_count = $1, updated_at = NOW() WHERE id = $2
		`, queued, campaign.ID)
	}

	return queued, nil
}

// subscriberForScheduling represents a subscriber being scheduled for sending
type subscriberForScheduling struct {
	ID    uuid.UUID
	Email string
}

// getSubscriberOptimalTimes retrieves optimal send times for subscribers
func (cs *CampaignScheduler) getSubscriberOptimalTimes(ctx context.Context, campaign ScheduledCampaign, subscribers []subscriberForScheduling) map[uuid.UUID]time.Time {
	optimalTimes := make(map[uuid.UUID]time.Time)
	
	if len(subscribers) == 0 {
		return optimalTimes
	}
	
	// Build subscriber ID list
	subIDs := make([]uuid.UUID, len(subscribers))
	for i, s := range subscribers {
		subIDs[i] = s.ID
	}
	
	// Query optimal times from mailing_subscriber_optimal_times or mailing_campaign_scheduled_times
	// First check if campaign has pre-scheduled times
	preScheduledRows, err := cs.db.QueryContext(ctx, `
		SELECT subscriber_id, scheduled_time
		FROM mailing_campaign_scheduled_times
		WHERE campaign_id = $1 AND status = 'pending'
	`, campaign.ID)
	
	if err == nil {
		defer preScheduledRows.Close()
		for preScheduledRows.Next() {
			var subID uuid.UUID
			var scheduledTime time.Time
			if err := preScheduledRows.Scan(&subID, &scheduledTime); err == nil {
				optimalTimes[subID] = scheduledTime
			}
		}
	}
	
	// For subscribers without pre-scheduled times, calculate from optimal_times table
	missingSubIDs := make([]uuid.UUID, 0)
	for _, subID := range subIDs {
		if _, exists := optimalTimes[subID]; !exists {
			missingSubIDs = append(missingSubIDs, subID)
		}
	}
	
	if len(missingSubIDs) > 0 {
		// Get optimal hours for remaining subscribers
		optHourRows, err := cs.db.QueryContext(ctx, `
			SELECT sot.subscriber_id, sot.optimal_hour, COALESCE(sot.timezone, s.timezone, 'UTC')
			FROM mailing_subscriber_optimal_times sot
			JOIN mailing_subscribers s ON s.id = sot.subscriber_id
			WHERE sot.subscriber_id = ANY($1)
			AND sot.confidence >= 0.5
		`, missingSubIDs)
		
		if err == nil {
			defer optHourRows.Close()
			
			// Base date is today or campaign scheduled date
			baseDate := campaign.ScheduledAt
			if baseDate.IsZero() || baseDate.Before(time.Now()) {
				baseDate = time.Now()
			}
			
			for optHourRows.Next() {
				var subID uuid.UUID
				var optimalHour int
				var timezone string
				
				if err := optHourRows.Scan(&subID, &optimalHour, &timezone); err != nil {
					continue
				}
				
				// Calculate the optimal send time
				scheduledTime := time.Date(
					baseDate.Year(), baseDate.Month(), baseDate.Day(),
					optimalHour, 0, 0, 0, time.UTC,
				)
				
				// If the time has already passed today, schedule for tomorrow
				if scheduledTime.Before(time.Now()) {
					scheduledTime = scheduledTime.Add(24 * time.Hour)
				}
				
				optimalTimes[subID] = scheduledTime
			}
		}
	}
	
	// Get audience-level default for remaining subscribers without individual data
	if len(optimalTimes) < len(subscribers) && campaign.ListID.Valid {
		var audienceBestHour int
		err := cs.db.QueryRowContext(ctx, `
			SELECT COALESCE(overall_best_hour, 10)
			FROM mailing_audience_optimal_times
			WHERE list_id = $1
		`, campaign.ListID.String).Scan(&audienceBestHour)
		
		if err != nil {
			audienceBestHour = 10 // Default to 10 AM
		}
		
		baseDate := campaign.ScheduledAt
		if baseDate.IsZero() || baseDate.Before(time.Now()) {
			baseDate = time.Now()
		}
		
		defaultTime := time.Date(
			baseDate.Year(), baseDate.Month(), baseDate.Day(),
			audienceBestHour, 0, 0, 0, time.UTC,
		)
		if defaultTime.Before(time.Now()) {
			defaultTime = defaultTime.Add(24 * time.Hour)
		}
		
		for _, sub := range subscribers {
			if _, exists := optimalTimes[sub.ID]; !exists {
				optimalTimes[sub.ID] = defaultTime
			}
		}
	}
	
	return optimalTimes
}

// mapFieldToColumn maps a segment condition field to the database column
func (cs *CampaignScheduler) mapFieldToColumn(field string) string {
	// Standard subscriber fields
	switch field {
	case "email":
		return "s.email"
	case "first_name":
		return "s.first_name"
	case "last_name":
		return "s.last_name"
	case "phone":
		return "s.phone"
	case "city":
		return "s.city"
	case "state":
		return "s.state"
	case "country":
		return "s.country"
	case "zip", "postal_code":
		return "s.zip"
	case "status":
		return "s.status"
	case "source":
		return "s.source"
	case "engagement_score":
		return "COALESCE(s.engagement_score, 0)"
	case "created_at":
		return "s.created_at"
	case "updated_at":
		return "s.updated_at"
	default:
		// Check if it's a custom field (custom.field_name)
		if len(field) > 7 && field[:7] == "custom." {
			return fmt.Sprintf("s.custom_fields->>'%s'", field[7:])
		}
		// Assume it's a direct column
		return "s." + field
	}
}

// markCampaignFailed marks a campaign as failed
func (cs *CampaignScheduler) markCampaignFailed(ctx context.Context, id uuid.UUID, reason string) {
	cs.db.ExecContext(ctx, `
		UPDATE mailing_campaigns 
		SET status = 'failed', completed_at = NOW(), updated_at = NOW()
		WHERE id = $1
	`, id)
}

// markCampaignCompleted marks a campaign as completed
func (cs *CampaignScheduler) markCampaignCompleted(ctx context.Context, id uuid.UUID, sent int) {
	cs.db.ExecContext(ctx, `
		UPDATE mailing_campaigns 
		SET status = 'completed', sent_count = $2, completed_at = NOW(), updated_at = NOW()
		WHERE id = $1
	`, id, sent)
}

// registerWorker registers this scheduler in the workers table
func (cs *CampaignScheduler) registerWorker() {
	_, err := cs.db.Exec(`
		INSERT INTO mailing_workers (id, worker_type, hostname, status, started_at, last_heartbeat_at)
		VALUES ($1, 'scheduler', $2, 'running', NOW(), NOW())
		ON CONFLICT (id) DO UPDATE SET status = 'running', last_heartbeat_at = NOW()
	`, cs.workerID, getHostname())
	if err != nil {
		log.Printf("[CampaignScheduler] Warning: Failed to register worker: %v", err)
	}
}

// deregisterWorker removes this scheduler from the workers table
func (cs *CampaignScheduler) deregisterWorker() {
	_, err := cs.db.Exec(`
		UPDATE mailing_workers SET status = 'stopped' WHERE id = $1
	`, cs.workerID)
	if err != nil {
		log.Printf("[CampaignScheduler] Warning: Failed to deregister worker: %v", err)
	}
}

// heartbeatLoop sends periodic heartbeats
func (cs *CampaignScheduler) heartbeatLoop() {
	defer cs.wg.Done()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-cs.ctx.Done():
			return
		case <-ticker.C:
			cs.db.Exec(`
				UPDATE mailing_workers 
				SET last_heartbeat_at = NOW(),
				    metadata = $2
				WHERE id = $1
			`, cs.workerID, fmt.Sprintf(`{"campaigns_processed": %d, "subscribers_queued": %d, "errors": %d}`,
				atomic.LoadInt64(&cs.campaignsProcessed),
				atomic.LoadInt64(&cs.subscribersQueued),
				atomic.LoadInt64(&cs.errors)))
		}
	}
}

// =============================================================================
// VALIDATION HELPERS
// =============================================================================

// ValidateScheduleTime checks if the scheduled time is valid
// Returns error if scheduled time is less than MinPreparationMinutes from now
func ValidateScheduleTime(scheduledAt time.Time) error {
	minTime := time.Now().Add(time.Duration(MinPreparationMinutes) * time.Minute)
	if scheduledAt.Before(minTime) {
		return fmt.Errorf(
			"scheduled time must be at least %d minutes in the future (minimum: %s)",
			MinPreparationMinutes,
			minTime.Format(time.RFC3339),
		)
	}
	return nil
}

// CanEditCampaign checks if a campaign can still be edited
// Returns false if the campaign is within EditLockMinutes of its scheduled time
func CanEditCampaign(status string, scheduledAt *time.Time) (bool, string) {
	// Can always edit drafts
	if status == "draft" {
		return true, ""
	}

	// Cannot edit if already sending, completed, cancelled, or failed
	if status == "sending" || status == "completed" || status == "cancelled" || 
	   status == "failed" || status == "completed_with_errors" || status == "preparing" {
		return false, fmt.Sprintf("cannot edit campaign in '%s' status", status)
	}

	// For scheduled campaigns, check the edit lock window
	if status == "scheduled" && scheduledAt != nil {
		lockTime := scheduledAt.Add(-time.Duration(EditLockMinutes) * time.Minute)
		if time.Now().After(lockTime) {
			return false, fmt.Sprintf(
				"campaign is locked for sending preparation (scheduled at %s, edit lock started at %s)",
				scheduledAt.Format(time.RFC3339),
				lockTime.Format(time.RFC3339),
			)
		}
	}

	return true, ""
}

// CanCancelCampaign checks if a campaign can be cancelled
// Campaigns can ALWAYS be cancelled except when completed or already cancelled
func CanCancelCampaign(status string) (bool, string) {
	switch status {
	case "completed", "completed_with_errors", "cancelled", "failed":
		return false, fmt.Sprintf("cannot cancel campaign in '%s' status", status)
	default:
		return true, ""
	}
}

// CanPauseCampaign checks if a campaign can be paused
// Can pause scheduled, preparing, or sending campaigns
func CanPauseCampaign(status string) (bool, string) {
	switch status {
	case "scheduled", "preparing", "sending":
		return true, ""
	default:
		return false, fmt.Sprintf("cannot pause campaign in '%s' status", status)
	}
}

// newContext creates a new context for the scheduler (exposed for testing)
func (cs *CampaignScheduler) newContext() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}
