package worker

import (
	"context"
	"database/sql"
	"log"
	"os"
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
	//
	// Derivation (REQ-005 criterion 2; findings 2026-07-13-B §1): a claimed
	// row can legitimately sit claimed-but-unprocessed far longer than the
	// old 5m value. Worst-case claimed backlog in a live pool:
	//     workCh buffer            = batchSize*2 = 200   (send_worker.go Start)
	//   + dispatch parallelism 8 × batch 100      ≈ 800  (dispatchCampaignParallelism,
	//                                                      dispatchOneCampaign)
	//   ≈ 1,000 rows claimed ahead of processing.
	// Drain rate under a slow PMTA: 25 workers each pinned at processItem's
	// 30s per-item timeout → 1,000 / 25 × 30s = 20 min tail latency from
	// claim to submission. Recovery must only reclaim rows older than that,
	// otherwise it requeues rows a live worker still holds and a second
	// dispatch double-sends them. 25m = 20m worst case + 5m margin. The
	// pre-submission ownership re-check (send_worker.go stillOwnsItem) is
	// the second line of defense for anything that still slips through.
	DefaultStaleAge = 25 * time.Minute

	// MaxRetryCount is the maximum number of times an item can be retried
	// before it is moved to dead_letter status.
	MaxRetryCount = 5

	// MaxStrictPoolRetries is the max retries for strict-isolation pool exhaustion.
	// After this many deferrals, the message moves to dead_letter_strict (not suppressed).
	MaxStrictPoolRetries = 10

	// StrictPoolBackoffBase is the initial backoff for strict-pool deferrals (30s).
	StrictPoolBackoffBase = 30 * time.Second

	// StrictPoolBackoffCap is the maximum backoff interval (5 minutes).
	StrictPoolBackoffCap = 5 * time.Minute
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
	//
	// Counter unification (REQ-005 criterion 3; findings 2026-07-13-B §2):
	// SendWorkerPool's markFailed/deferStrictPool and the outbox dead-letter
	// panel (internal/api/outbox_admin.go) all use `attempts`; this worker
	// previously kept its own `retry_count`, which markFailed never touched —
	// so a row parked at status='failed' had retry_count=0 forever: never
	// requeued, never dead-lettered, invisible to the panel. Every v1 pass
	// below now keys on `attempts`. (v2 passes are untouched: the only
	// mailing_campaign_queue_v2 writers — SendWorkerPoolV2/CampaignProcessor,
	// neither started at boot — use retry_count consistently on both sides.)

	// 1a. Requeue stuck items (under retry limit).
	// SendWorkerPool sets locked_at (not claimed_at) when claiming items,
	// so we check both columns to catch items stuck by either code path.
	// A crash-requeue consumes one attempt so a row that repeatedly kills
	// its worker still converges to dead_letter instead of looping forever.
	res, err := qr.db.ExecContext(queryCtx, `
		UPDATE mailing_campaign_queue
		SET status = 'queued',
		    worker_id = NULL,
		    claimed_at = NULL,
		    locked_at = NULL,
		    attempts = COALESCE(attempts, 0) + 1
		WHERE status IN ('claimed', 'sending')
		  AND COALESCE(locked_at, claimed_at) < NOW() - $1::interval
		  AND COALESCE(attempts, 0) < $2
	`, qr.staleAge.String(), MaxRetryCount)
	if err != nil {
		log.Printf("[QueueRecovery] v1 requeue error: %v", err)
	} else if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[QueueRecovery] v1: requeued %d stuck items", n)
	}

	// 1a-bis. Retry legacy 'failed' rows (REQ-005 criterion 3). Legacy-mode
	// markFailed parks non-transient failures at status='failed' with
	// attempts already incremented (send_worker.go markFailed); the claim
	// query only picks 'queued', so those rows were silently terminal on
	// first failure. Requeue them once their last attempt is staleAge old
	// (staleAge doubles as the retry backoff), but ONLY while the campaign
	// is still 'sending' — claimISPForOne requires camp.status='sending',
	// so requeueing rows of finished/cancelled campaigns would only
	// manufacture zombie 'queued' rows. attempts is NOT incremented here:
	// markFailed already counted the failed attempt, and it dead-letters at
	// the MaxRetryCount-th attempt on its own.
	// Prod runs OUTBOX_MODE=durable (which writes failed_retryable, not
	// 'failed'); this pass exists for the documented legacy rollback escape
	// hatch. Kill switch: DISABLE_FAILED_ROW_RECOVERY=true.
	if os.Getenv("DISABLE_FAILED_ROW_RECOVERY") != "true" {
		res, err = qr.db.ExecContext(queryCtx, `
			UPDATE mailing_campaign_queue q
			SET status = 'queued',
			    worker_id = NULL,
			    claimed_at = NULL,
			    locked_at = NULL
			WHERE q.status = 'failed'
			  AND COALESCE(q.last_attempt_at, q.created_at) < NOW() - $1::interval
			  AND COALESCE(q.attempts, 0) < $2
			  AND EXISTS (
			      SELECT 1 FROM mailing_campaigns c
			      WHERE c.id = q.campaign_id AND c.status = 'sending'
			  )
		`, qr.staleAge.String(), MaxRetryCount)
		if err != nil {
			log.Printf("[QueueRecovery] v1 failed-row requeue error: %v", err)
		} else if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("[QueueRecovery] v1: requeued %d 'failed' rows for retry", n)
		}
	}

	// 1b. Dead-letter items that exceeded max retries. status='dead_letter'
	// is listed by the Delivery Queue dead-letter panel (outbox_admin.go),
	// so exhausted rows are an honest, operator-visible terminal state.
	res, err = qr.db.ExecContext(queryCtx, `
		UPDATE mailing_campaign_queue
		SET status = 'dead_letter'
		WHERE status IN ('claimed', 'sending', 'failed')
		  AND COALESCE(attempts, 0) >= $1
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
