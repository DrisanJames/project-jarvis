package worker

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"time"
)

// =============================================================================
// DATA CLEANUP WORKER — Removes Old Queue Items, Tracking Events & Decisions
// =============================================================================
// Without periodic cleanup, completed queue items, tracking events, AI
// predictions, and agent decisions accumulate indefinitely, causing database
// bloat and performance degradation.
//
// Retention policies:
//   - Queue items (sent/skipped):      7 days
//   - Dead-letter queue items:         30 days
//   - Tracking events:                 90 days
//   - Agent send decisions (executed):  30 days
//
// Deletes run in batches of 10 000 rows to avoid long-running transactions
// that could lock tables and block production traffic.

const (
	// DefaultCleanupInterval is how often the cleanup cycle runs.
	DefaultCleanupInterval = 1 * time.Hour

	// cleanupBatchSize limits each DELETE to avoid table-level locks.
	cleanupBatchSize = 10000
)

// DataCleanupWorker periodically removes old data from the mailing tables.
type DataCleanupWorker struct {
	db       *sql.DB
	interval time.Duration
}

// NewDataCleanupWorker creates a new cleanup worker with default settings.
func NewDataCleanupWorker(db *sql.DB) *DataCleanupWorker {
	return &DataCleanupWorker{
		db:       db,
		interval: DefaultCleanupInterval,
	}
}

// Start begins the cleanup loop. It blocks until ctx is cancelled.
func (dc *DataCleanupWorker) Start(ctx context.Context) {
	log.Printf("[DataCleanup] Starting (interval=%s, batch_size=%d)", dc.interval, cleanupBatchSize)

	// Run once immediately on start
	dc.cleanup(ctx)

	ticker := time.NewTicker(dc.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[DataCleanup] Stopping")
			return
		case <-ticker.C:
			dc.cleanup(ctx)
		}
	}
}

func (dc *DataCleanupWorker) cleanup(ctx context.Context) {
	start := time.Now()
	log.Println("[DataCleanup] Cleanup cycle starting...")

	dc.cleanupQueueItems(ctx)
	dc.cleanupTrackingEvents(ctx)
	dc.cleanupAgentDecisions(ctx)
	dc.cleanupDeadLetterItems(ctx)

	log.Printf("[DataCleanup] Cleanup cycle completed in %s", time.Since(start).Round(time.Millisecond))
}

// ---------------------------------------------------------------------------
// cleanupQueueItems — Delete sent/skipped queue items older than 7 days.
// ---------------------------------------------------------------------------
func (dc *DataCleanupWorker) cleanupQueueItems(ctx context.Context) {
	// V1 queue
	total := dc.batchDelete(ctx, "mailing_campaign_queue", `
		DELETE FROM mailing_campaign_queue
		WHERE id IN (
			SELECT id FROM mailing_campaign_queue
			WHERE status IN ('sent', 'skipped')
			  AND updated_at < NOW() - INTERVAL '7 days'
			LIMIT $1
		)
	`)
	if total > 0 {
		log.Printf("[DataCleanup] Removed %d sent/skipped items from mailing_campaign_queue", total)
	}

	// V2 queue (may not exist)
	total = dc.batchDelete(ctx, "mailing_campaign_queue_v2", `
		DELETE FROM mailing_campaign_queue_v2
		WHERE id IN (
			SELECT id FROM mailing_campaign_queue_v2
			WHERE status IN ('sent', 'skipped')
			  AND updated_at < NOW() - INTERVAL '7 days'
			LIMIT $1
		)
	`)
	if total > 0 {
		log.Printf("[DataCleanup] Removed %d sent/skipped items from mailing_campaign_queue_v2", total)
	}
}

// ---------------------------------------------------------------------------
// cleanupTrackingEvents — Delete tracking events older than 90 days.
// ---------------------------------------------------------------------------
func (dc *DataCleanupWorker) cleanupTrackingEvents(ctx context.Context) {
	total := dc.batchDelete(ctx, "mailing_tracking_events", `
		DELETE FROM mailing_tracking_events
		WHERE id IN (
			SELECT id FROM mailing_tracking_events
			WHERE created_at < NOW() - INTERVAL '90 days'
			LIMIT $1
		)
	`)
	if total > 0 {
		log.Printf("[DataCleanup] Removed %d tracking events older than 90 days", total)
	}
}

// ---------------------------------------------------------------------------
// cleanupAgentDecisions — Delete executed agent decisions older than 30 days.
// ---------------------------------------------------------------------------
func (dc *DataCleanupWorker) cleanupAgentDecisions(ctx context.Context) {
	total := dc.batchDelete(ctx, "mailing_agent_send_decisions", `
		DELETE FROM mailing_agent_send_decisions
		WHERE id IN (
			SELECT id FROM mailing_agent_send_decisions
			WHERE executed = true
			  AND created_at < NOW() - INTERVAL '30 days'
			LIMIT $1
		)
	`)
	if total > 0 {
		log.Printf("[DataCleanup] Removed %d executed agent decisions older than 30 days", total)
	}
}

// ---------------------------------------------------------------------------
// cleanupDeadLetterItems — Delete dead-letter queue items older than 30 days.
// ---------------------------------------------------------------------------
func (dc *DataCleanupWorker) cleanupDeadLetterItems(ctx context.Context) {
	// V1 queue
	total := dc.batchDelete(ctx, "mailing_campaign_queue", `
		DELETE FROM mailing_campaign_queue
		WHERE id IN (
			SELECT id FROM mailing_campaign_queue
			WHERE status = 'dead_letter'
			  AND updated_at < NOW() - INTERVAL '30 days'
			LIMIT $1
		)
	`)
	if total > 0 {
		log.Printf("[DataCleanup] Removed %d dead-letter items from mailing_campaign_queue", total)
	}

	// V2 queue (may not exist)
	total = dc.batchDelete(ctx, "mailing_campaign_queue_v2", `
		DELETE FROM mailing_campaign_queue_v2
		WHERE id IN (
			SELECT id FROM mailing_campaign_queue_v2
			WHERE status = 'dead_letter'
			  AND updated_at < NOW() - INTERVAL '30 days'
			LIMIT $1
		)
	`)
	if total > 0 {
		log.Printf("[DataCleanup] Removed %d dead-letter items from mailing_campaign_queue_v2", total)
	}
}

// ---------------------------------------------------------------------------
// batchDelete runs the given DELETE statement in a loop, passing
// cleanupBatchSize as $1, until zero rows are affected. Returns the
// cumulative number of deleted rows.
//
// If the target table does not exist (or any other permanent error occurs on
// the first attempt), it logs once and returns 0 — this keeps the worker safe
// when migrations haven't run yet.
// ---------------------------------------------------------------------------
func (dc *DataCleanupWorker) batchDelete(ctx context.Context, table, query string) int64 {
	var totalDeleted int64

	for {
		if ctx.Err() != nil {
			return totalDeleted
		}

		queryCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		res, err := dc.db.ExecContext(queryCtx, query, cleanupBatchSize)
		cancel()

		if err != nil {
			// Gracefully handle missing tables (relation "..." does not exist)
			if isTableNotExistsError(err) {
				// Only log on first iteration so we don't spam
				if totalDeleted == 0 {
					log.Printf("[DataCleanup] Table %s does not exist, skipping", table)
				}
				return totalDeleted
			}
			log.Printf("[DataCleanup] Error deleting from %s: %v", table, err)
			return totalDeleted
		}

		affected, _ := res.RowsAffected()
		if affected == 0 {
			return totalDeleted
		}
		totalDeleted += affected

		// Small pause between batches to reduce load
		time.Sleep(100 * time.Millisecond)
	}
}

// isTableNotExistsError checks whether a Postgres error indicates the target
// table (relation) does not exist. This avoids importing the pq package just
// for error-code matching — a simple string check is reliable enough here.
func isTableNotExistsError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "relation") && strings.Contains(msg, "does not exist")
}
