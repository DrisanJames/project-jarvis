package worker

// =============================================================================
// OUTBOX RECONCILER — Closes the crash-between-SMTP-ACK-and-DB-COMMIT window.
// =============================================================================
// In durable mode (SendWorkerPool.SetOutboxMode("durable")) the claim path
// transitions queue rows straight from 'queued' to 'submitting'. The send
// worker then calls the ESP/PMTA sender and, on success, executes a single
// atomic UPDATE flipping 'submitting' -> 'accepted' with message_id,
// sent_at, submitted_at, and pmta_response populated.
//
// There is one unavoidable crash window: the worker process can die AFTER
// PMTA accepts the message on the wire but BEFORE the row-UPDATE commits.
// The existing QueueRecoveryWorker only looks at 'claimed' and 'sending';
// a row stranded in 'submitting' is invisible to it, and a worker restart
// leaves the row orphaned forever. That is unacceptable for an enterprise
// platform.
//
// The reconciler closes this window by periodically scanning for rows stuck
// in 'submitting' beyond a grace period and making a deterministic decision:
//
//   message_id IS NOT NULL  -> the sender returned success, the markSent
//                              UPDATE crashed mid-flight. The message is
//                              already in PMTA's queue; commit as 'accepted'
//                              so downstream analytics and the 'sent' leg of
//                              the existing pipeline see it.
//
//   message_id IS NULL      -> the sender either never completed or never
//                              reached the point of returning a message_id.
//                              Re-queue via 'failed_retryable' with a
//                              next_attempt_at of NOW() so the standard
//                              claim query re-picks it on the next tick.
//
// Every transition carries the expected-prior-status guard so a concurrent
// live worker cannot race with the reconciler and double-transition a row.
//
// Wired into cmd/server/main.go next to the existing QueueRecoveryWorker,
// gated on mailing.outbox_mode=durable so legacy deployments remain
// byte-for-byte identical.

import (
	"context"
	"database/sql"
	"log"
	"time"
)

const (
	// DefaultReconcilerInterval is how often the reconciler scans for
	// stuck 'submitting' rows. 60s is a balance between tight recovery
	// SLOs and DB query frequency.
	DefaultReconcilerInterval = 60 * time.Second

	// DefaultReconcilerGrace is how long a row can stay in 'submitting'
	// before we treat it as stuck. A live worker completing an SMTP
	// DATA phase is bounded by the PMTA HTTP bridge timeout (20s) plus
	// network overhead; 10 minutes leaves ample headroom against false
	// positives while still recovering well inside a typical wave.
	DefaultReconcilerGrace = 10 * time.Minute
)

// OutboxReconciler closes the durable-outbox crash window between ESP
// acknowledgement and DB commit. Safe to run in parallel with the existing
// QueueRecoveryWorker — the two target disjoint status sets.
type OutboxReconciler struct {
	db       *sql.DB
	interval time.Duration
	grace    time.Duration
}

// NewOutboxReconciler constructs a reconciler with default timings.
func NewOutboxReconciler(db *sql.DB) *OutboxReconciler {
	return &OutboxReconciler{
		db:       db,
		interval: DefaultReconcilerInterval,
		grace:    DefaultReconcilerGrace,
	}
}

// NewOutboxReconcilerWithConfig constructs a reconciler with explicit timings.
// Non-positive values fall back to defaults so callers cannot accidentally
// disable the reconciler by passing zero.
func NewOutboxReconcilerWithConfig(db *sql.DB, interval, grace time.Duration) *OutboxReconciler {
	if interval <= 0 {
		interval = DefaultReconcilerInterval
	}
	if grace <= 0 {
		grace = DefaultReconcilerGrace
	}
	return &OutboxReconciler{
		db:       db,
		interval: interval,
		grace:    grace,
	}
}

// Start blocks until ctx is cancelled. Runs the reconcile pass immediately
// on entry so a server restart that coincides with a stuck row recovers
// within milliseconds rather than waiting for the first ticker fire.
func (r *OutboxReconciler) Start(ctx context.Context) {
	log.Printf("[OutboxReconciler] Starting (interval=%s, grace=%s)", r.interval, r.grace)
	r.reconcileOnce(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("[OutboxReconciler] Stopping")
			return
		case <-ticker.C:
			r.reconcileOnce(ctx)
		}
	}
}

// reconcileOnce performs a single reconciliation pass. Runs two UPDATEs —
// one to commit the crash-window sends (message_id present) and one to
// requeue the truly-lost sends (message_id absent). Each UPDATE carries
// the status='submitting' WHERE guard so a concurrent live worker cannot
// race us; the worst case if both fire at the same millisecond is a no-op
// on the reconciler side.
func (r *OutboxReconciler) reconcileOnce(ctx context.Context) {
	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Commit the crash-window sends: the sender succeeded (message_id is
	// populated because some prior partial-commit path wrote it) but the
	// state flip never landed. Transitioning to 'accepted' is safe — the
	// message is already in PMTA's queue, so "committing" here just
	// restores the outbox to the truth of what already happened.
	res, err := r.db.ExecContext(queryCtx, `
		UPDATE mailing_campaign_queue
		SET status = 'accepted',
		    submitted_at = COALESCE(submitted_at, NOW()),
		    html_content = NULL,
		    plain_content = NULL
		WHERE status = 'submitting'
		  AND locked_at < NOW() - $1::interval
		  AND message_id IS NOT NULL
	`, r.grace.String())
	if err != nil {
		log.Printf("[OutboxReconciler] commit-window error: %v", err)
	} else if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[OutboxReconciler] committed %d crash-window sends (submitting->accepted)", n)
	}

	// Re-queue the truly-lost sends. Worker crashed before the SMTP ACK
	// landed (or before message_id got persisted). Transition via
	// 'failed_retryable' so the standard claim query re-picks the row on
	// the next tick. Attempts counter is bumped so the retry policy can
	// still move the row to dead_letter after MaxRetryCount shots.
	res, err = r.db.ExecContext(queryCtx, `
		UPDATE mailing_campaign_queue
		SET status = 'failed_retryable',
		    error_message = COALESCE(error_message, 'reconciler: submitting timed out, requeued'),
		    attempts = attempts + 1,
		    last_attempt_at = NOW(),
		    next_attempt_at = NOW()
		WHERE status = 'submitting'
		  AND locked_at < NOW() - $1::interval
		  AND message_id IS NULL
	`, r.grace.String())
	if err != nil {
		log.Printf("[OutboxReconciler] requeue error: %v", err)
	} else if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[OutboxReconciler] requeued %d stranded rows (submitting->failed_retryable)", n)
	}
}
