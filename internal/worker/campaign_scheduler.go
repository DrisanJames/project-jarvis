package worker

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// =============================================================================
// CAMPAIGN SCHEDULER WORKER
// =============================================================================
// Retained functionality:
//   - preparationLoop: marks campaigns as 'preparing' within edit lock window
//   - checkCompletedCampaigns: transitions 'sending' -> 'sent' when all work done
//   - heartbeatLoop: worker registration heartbeat
//
// The standard subscriber enqueue path (schedulerLoop -> processReadyCampaigns ->
// processCampaign -> getRecipientCount -> enqueueSubscribers) has been removed.
// All campaigns use the PMTA ISP wave pipeline:
//   AudienceWorker -> PMTAWaveScheduler -> EnqueuePMTAWave -> SendWorkerPool

const (
	// MinPreparationMinutes is the minimum time before a scheduled send
	// to allow for suppression checks, audience calculation, etc.
	MinPreparationMinutes = 5

	// EditLockMinutes is when the campaign becomes locked for editing
	// (same as preparation time)
	EditLockMinutes = 5

	// DefaultPollInterval is how often to check for scheduled campaigns
	DefaultSchedulerPollInterval = 30 * time.Second
)

// CampaignScheduler monitors campaign lifecycle: preparation marking,
// completion detection, and worker heartbeat.
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

	// Start preparation checker (moves campaigns to 'preparing' status)
	// and completion checker (transitions 'sending' -> 'sent')
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
			cs.checkCompletedCampaigns()
		}
	}
}

// checkCompletedCampaigns checks for 'sending' campaigns where all queue items are processed
func (cs *CampaignScheduler) checkCompletedCampaigns() {
	ctx, cancel := context.WithTimeout(cs.ctx, 30*time.Second)
	defer cancel()

	// Find campaigns in 'sending' status where all queue items are done.
	// Durable outbox: accepted = handoff complete. Legacy: sent = complete.
	rows, err := cs.db.QueryContext(ctx, `
		SELECT c.id, 
			   COALESCE(SUM(CASE WHEN q.status IN ('sent','accepted') THEN 1 ELSE 0 END), 0) as sent,
			   COALESCE(SUM(CASE WHEN q.status IN ('failed','dead_letter','dead_letter_strict','failed_permanent') THEN 1 ELSE 0 END), 0) as failed,
			   COALESCE(SUM(CASE WHEN q.status = 'skipped' THEN 1 ELSE 0 END), 0) as skipped,
			   COALESCE(SUM(CASE WHEN q.status IN ('queued','sending','claimed','pending','submitting','failed_retryable') THEN 1 ELSE 0 END), 0) as pending,
			   COUNT(q.id) as total
		FROM mailing_campaigns c
		LEFT JOIN mailing_campaign_queue q ON q.campaign_id = c.id
		WHERE c.status = 'sending'
		GROUP BY c.id
		HAVING COALESCE(SUM(CASE WHEN q.status IN ('queued','sending','claimed','pending','submitting','failed_retryable') THEN 1 ELSE 0 END), 0) = 0
		   AND NOT EXISTS (
		       SELECT 1 FROM mailing_campaign_waves w
		       WHERE w.campaign_id = c.id
		         AND w.status NOT IN ('completed','cancelled','failed','dead_letter')
		   )
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
		// Use 'sent'/'cancelled' to match DB CHECK constraint
		var finalStatus string
		if failed == total {
			finalStatus = "cancelled"
		} else {
			finalStatus = "sent"
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

	// Pre-enqueued campaigns: queue items may have been fully processed before the
	// scheduler transitioned the campaign to 'sending'. Catch campaigns in 'draft'
	// or 'scheduled' that have queue items all done (no pending items remain).
	preRows, _ := cs.db.QueryContext(ctx, `
		SELECT c.id,
		       COALESCE(SUM(CASE WHEN q.status IN ('sent','accepted') THEN 1 ELSE 0 END), 0) as sent,
		       COALESCE(SUM(CASE WHEN q.status IN ('failed','dead_letter','dead_letter_strict','failed_permanent') THEN 1 ELSE 0 END), 0) as failed,
		       COUNT(q.id) as total
		FROM mailing_campaigns c
		JOIN mailing_campaign_queue q ON q.campaign_id = c.id
		WHERE c.status IN ('draft', 'scheduled')
		GROUP BY c.id
		HAVING COALESCE(SUM(CASE WHEN q.status IN ('queued','sending','claimed','pending','submitting','failed_retryable') THEN 1 ELSE 0 END), 0) = 0
		   AND NOT EXISTS (
		       SELECT 1 FROM mailing_campaign_waves w
		       WHERE w.campaign_id = c.id
		         AND w.status NOT IN ('completed','cancelled','failed','dead_letter')
		   )
		   AND COUNT(q.id) > 0
	`)
	if preRows != nil {
		for preRows.Next() {
			var cid uuid.UUID
			var sent, failed, total int
			if preRows.Scan(&cid, &sent, &failed, &total) == nil && total > 0 {
				finalStatus := "sent"
				if failed == total {
					finalStatus = "cancelled"
				}
				cs.db.ExecContext(ctx, `
					UPDATE mailing_campaigns
					SET status = $2, sent_count = $3, total_recipients = GREATEST(total_recipients, $4),
					    started_at = COALESCE(started_at, NOW()), completed_at = NOW(), updated_at = NOW()
					WHERE id = $1
				`, cid, finalStatus, sent, total)
				log.Printf("[CampaignScheduler] Pre-enqueued campaign %s completed: status=%s sent=%d failed=%d", cid, finalStatus, sent, failed)
			}
		}
		preRows.Close()
	}

	// Orphaned campaigns: stuck in 'sending' for >10 min with 0 queue items.
	_, _ = cs.db.ExecContext(ctx, `
		UPDATE mailing_campaigns
		SET status = 'failed', completed_at = NOW(), updated_at = NOW()
		WHERE status = 'sending'
		  AND started_at < NOW() - INTERVAL '10 minutes'
		  AND NOT EXISTS (
		      SELECT 1 FROM mailing_campaign_queue WHERE campaign_id = mailing_campaigns.id
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM mailing_campaign_waves w
		      WHERE w.campaign_id = mailing_campaigns.id
		        AND w.status NOT IN ('completed','cancelled','failed','dead_letter')
		  )
	`)
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
		  AND COALESCE(execution_mode, '') != 'pmta_isp_wave'
	`)

	if err != nil {
		log.Printf("[CampaignScheduler] Error marking campaigns as preparing: %v", err)
		return
	}

	if count, _ := result.RowsAffected(); count > 0 {
		log.Printf("[CampaignScheduler] Moved %d campaigns to 'preparing' status", count)
	}
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
	if status == "draft" {
		return true, ""
	}

	if status == "sending" || status == "sent" || status == "cancelled" ||
		status == "completed" || status == "failed" || status == "completed_with_errors" || status == "preparing" {
		return false, fmt.Sprintf("cannot edit campaign in '%s' status", status)
	}

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
	case "sent", "completed", "completed_with_errors", "cancelled", "failed":
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

func nilUUID(id uuid.UUID) interface{} {
	if id == uuid.Nil {
		return nil
	}
	return id
}

// classifyEmailISP is kept as a thin alias for backward compatibility with
// any code that calls it by domain string. New code should use ClassifySubscriberISP.
func classifyEmailISP(domain string) string {
	return ClassifySubscriberISP("x@" + domain)
}
