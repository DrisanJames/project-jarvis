package worker

import (
	"context"
	"database/sql"
	"log"
	"time"
)

// =============================================================================
// QUEUE RECOVERY WORKER — Reclaims Stuck Items & Enforces Max Retries
// =============================================================================
// If a send worker crashes mid-processing, queue items remain stuck in
// 'claimed' or 'sending' status indefinitely. This worker periodically
// scans for such items and either requeues them (if under the retry limit)
// or moves them to 'dead_letter' status.
//
// Covers both queue tables:
//   - mailing_campaign_queue   (CampaignProcessor / SendWorkerPool v1)
//   - mailing_campaign_queue_v2 (SendWorkerPoolV2)

const (
	// DefaultRecoveryInterval is how often we scan for stuck items.
	DefaultRecoveryInterval = 2 * time.Minute

	// DefaultStaleAge is how long an item can be claimed before we consider
	// it stuck (worker likely crashed).
	DefaultStaleAge = 5 * time.Minute

	// MaxRetryCount is the maximum number of times an item can be retried
	// before it is moved to dead_letter status.
	MaxRetryCount = 5
)

// QueueRecoveryWorker periodically reclaims stuck queue items and enforces
// a maximum retry limit by moving permanently failed items to dead_letter.
type QueueRecoveryWorker struct {
	db       *sql.DB
	interval time.Duration // check every 2 minutes by default
	staleAge time.Duration // items claimed > 5 minutes ago are stuck
}

// NewQueueRecoveryWorker creates a new recovery worker with default settings.
func NewQueueRecoveryWorker(db *sql.DB) *QueueRecoveryWorker {
	return &QueueRecoveryWorker{
		db:       db,
		interval: DefaultRecoveryInterval,
		staleAge: DefaultStaleAge,
	}
}

// NewQueueRecoveryWorkerWithConfig creates a recovery worker with custom timing.
func NewQueueRecoveryWorkerWithConfig(db *sql.DB, interval, staleAge time.Duration) *QueueRecoveryWorker {
	if interval <= 0 {
		interval = DefaultRecoveryInterval
	}
	if staleAge <= 0 {
		staleAge = DefaultStaleAge
	}
	return &QueueRecoveryWorker{
		db:       db,
		interval: interval,
		staleAge: staleAge,
	}
}

// Start begins the recovery loop. It blocks until ctx is cancelled.
func (qr *QueueRecoveryWorker) Start(ctx context.Context) {
	log.Printf("[QueueRecovery] Starting (interval=%s, stale_age=%s, max_retries=%d)",
		qr.interval, qr.staleAge, MaxRetryCount)

	ticker := time.NewTicker(qr.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[QueueRecovery] Stopping")
			return
		case <-ticker.C:
			qr.recoverStuckItems(ctx)
		}
	}
}

// recoverStuckItems performs two passes on each queue table:
//  1. Requeue items that have been claimed too long but are under the retry limit.
//  2. Move items that have exceeded the retry limit to dead_letter.
func (qr *QueueRecoveryWorker) recoverStuckItems(ctx context.Context) {
	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// ── V1 queue: mailing_campaign_queue ──────────────────────────────────

	// 1a. Requeue stuck items (under retry limit)
	res, err := qr.db.ExecContext(queryCtx, `
		UPDATE mailing_campaign_queue
		SET status = 'queued',
		    worker_id = NULL,
		    claimed_at = NULL,
		    retry_count = retry_count + 1
		WHERE status IN ('claimed', 'sending')
		  AND claimed_at < NOW() - $1::interval
		  AND retry_count < $2
	`, qr.staleAge.String(), MaxRetryCount)
	if err != nil {
		log.Printf("[QueueRecovery] v1 requeue error: %v", err)
	} else if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[QueueRecovery] v1: requeued %d stuck items", n)
	}

	// 1b. Dead-letter items that exceeded max retries
	res, err = qr.db.ExecContext(queryCtx, `
		UPDATE mailing_campaign_queue
		SET status = 'dead_letter'
		WHERE status IN ('claimed', 'sending', 'failed')
		  AND retry_count >= $1
	`, MaxRetryCount)
	if err != nil {
		log.Printf("[QueueRecovery] v1 dead-letter error: %v", err)
	} else if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[QueueRecovery] v1: moved %d items to dead_letter", n)
	}

	// ── V2 queue: mailing_campaign_queue_v2 ──────────────────────────────

	// 2a. Requeue stuck items (under retry limit)
	res, err = qr.db.ExecContext(queryCtx, `
		UPDATE mailing_campaign_queue_v2
		SET status = 'queued',
		    worker_id = NULL,
		    claimed_at = NULL,
		    retry_count = retry_count + 1
		WHERE status IN ('claimed', 'sending')
		  AND claimed_at < NOW() - $1::interval
		  AND retry_count < $2
	`, qr.staleAge.String(), MaxRetryCount)
	if err != nil {
		// Table may not exist yet — don't spam logs
		log.Printf("[QueueRecovery] v2 requeue error (table may not exist): %v", err)
	} else if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[QueueRecovery] v2: requeued %d stuck items", n)
	}

	// 2b. Dead-letter items that exceeded max retries
	res, err = qr.db.ExecContext(queryCtx, `
		UPDATE mailing_campaign_queue_v2
		SET status = 'dead_letter'
		WHERE status IN ('claimed', 'sending', 'failed')
		  AND retry_count >= $1
	`, MaxRetryCount)
	if err != nil {
		log.Printf("[QueueRecovery] v2 dead-letter error (table may not exist): %v", err)
	} else if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[QueueRecovery] v2: moved %d items to dead_letter", n)
	}
}
