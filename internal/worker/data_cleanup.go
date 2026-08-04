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
//   - Operationally-terminal queue:    14 days (accepted/cancelled/failed/dead_letter*)
//   - Dead-letter queue items:         30 days (legacy path; terminal cleanup also covers dead_letter at 14d)
//   - Processed pmta_acct_raw:         14 days
//   - Tracking events:                 90 days
//   - Agent send decisions (executed):  30 days
//
// Deletes run in batches of 10 000 rows (terminal purge uses same size;
// start conservative — ops script uses 5k-10k for one-time catch-up).

const (
	// DefaultCleanupInterval is how often the cleanup cycle runs.
	DefaultCleanupInterval = 1 * time.Hour

	// cleanupBatchSize limits each DELETE to avoid table-level locks.
	cleanupBatchSize = 10000

	// slimBatchSize is the per-UPDATE row cap for the accepted-HTML slimmer.
	slimBatchSize = 10000

	// slimMaxRowsPerCycle bounds how much html_content backlog a single hourly
	// cycle will NULL out. A large historical backlog (the StorageGuard
	// queue_accepted_html alert flagged ~5.5M rows on 2026-06-07) is drained
	// gradually over many cycles instead of in one WAL-heavy burst.
	slimMaxRowsPerCycle = 500000

	// slimMaxIOWaitBackends is the primary-load gate: if at least this many
	// backends are already blocked in IO wait, the slimmer defers for this
	// cycle so it never piles WAL writes onto an active audience/segment IO
	// storm. The drain resumes automatically the next cycle once IO eases.
	slimMaxIOWaitBackends = 8

	// planRecipMaxCampaignsPerCycle bounds how many terminal campaigns one
	// hourly cycle purges from mailing_campaign_plan_recipients, so a large
	// backlog drains over cycles instead of one marathon pass. Steady-state
	// daily turnover is well under this.
	planRecipMaxCampaignsPerCycle = 500

	// terminalPurgeMaxRowsPerCycle bounds how many operationally-terminal queue
	// rows one hourly cycle deletes, so a large historical backlog (StorageGuard
	// flagged ~1.3M aged terminal rows on 2026-06-28) drains over a few cycles
	// instead of one WAL-heavy burst on the hot send queue. Steady-state daily
	// terminal turnover is well under this.
	terminalPurgeMaxRowsPerCycle = 500000
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
	dc.cleanupTerminalQueueItems(ctx)
	dc.slimAcceptedQueueHTML(ctx)
	dc.cleanupPlanRecipients(ctx)
	dc.cleanupContentSnapshots(ctx)
	dc.cleanupESPMetricSnapshots(ctx)
	dc.cleanupProcessedAcctRaw(ctx)
	dc.cleanupTrackingEvents(ctx)
	dc.cleanupAgentDecisions(ctx)
	dc.cleanupDeadLetterItems(ctx)

	log.Printf("[DataCleanup] Cleanup cycle completed in %s", time.Since(start).Round(time.Millisecond))
	EmitHeartbeat(ctx, dc.db, "data_cleanup", int(dc.interval.Seconds()), "ok", "")
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

// cleanupTerminalQueueItems deletes operationally-terminal queue rows older
// than 14 days. "Accepted" here means safe to delete for queue lifecycle —
// not final delivery state. Handoff metadata (campaign/subscriber/message_id)
// is retained until this TTL; HTML is nulled on send via markSent.
//
// Aged by the QUEUE ROW's own clock (COALESCE(updated_at, created_at)), exactly
// matching StorageGuard.checkQueueTerminalAged — NOT by the owning campaign's
// updated_at. The earlier campaign-driven shape gated deletion on
// `mailing_campaigns.updated_at < NOW()-14d`, but campaign updated_at is bumped
// on every engagement counter update (opens/clicks/deliveries/bounces); a
// campaign still receiving long-tail engagement (MPP/Apple re-fetch opens for
// weeks) keeps a fresh updated_at forever, so its 14-day-old terminal queue
// rows were never eligible and accumulated unbounded (StorageGuard flagged
// ~1.3M aged terminal rows on 2026-06-28, of which ~913k belonged to campaigns
// whose updated_at was as recent as that day). The partial index
// idx_mcq_terminal_cleanup (status, COALESCE(updated_at, created_at), id) makes
// the victim SELECT index-only regardless of table size, so the row-age shape
// no longer Seq-Scans the heap (the reason the campaign-keyed workaround
// existed before that index).
//
// Safety mirrors slimAcceptedQueueHTML: defer entirely if the primary is under
// heavy IO load, cap rows per cycle so a large backlog drains over a few cycles
// instead of one WAL burst, and pace between batches.
func (dc *DataCleanupWorker) cleanupTerminalQueueItems(ctx context.Context) {
	if dc.primaryBusy(ctx) {
		log.Printf("[DataCleanup] terminal queue purge deferred — primary under heavy IO load (>=%d backends in IO wait)", slimMaxIOWaitBackends)
		dc.logTerminalQueueStats(ctx)
		return
	}

	var total int64
	for total < terminalPurgeMaxRowsPerCycle {
		if ctx.Err() != nil {
			break
		}
		queryCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		// ORDER BY the exact prefix of idx_mcq_terminal_cleanup so the victim
		// SELECT is ALWAYS an index scan. Without it the planner pairs LIMIT
		// with a Seq Scan whenever it estimates matches are plentiful — and
		// once the aged rows are a thin slice clustered at the old end of a
		// multi-GB heap (aug03: 349k of 10.3M rows, 70 GB), that scan reads
		// most of the table before finding victims, hits statement_timeout,
		// and the purge makes zero progress every cycle while the backlog
		// grows. The ordered shape is deterministic regardless of planner
		// statistics.
		res, err := dc.db.ExecContext(queryCtx, `
			WITH doomed AS (
				SELECT id FROM mailing_campaign_queue
				WHERE status IN ('accepted','cancelled','failed','dead_letter','dead_letter_strict')
				  AND COALESCE(updated_at, created_at) < NOW() - INTERVAL '14 days'
				ORDER BY status, COALESCE(updated_at, created_at)
				LIMIT $1
			)
			DELETE FROM mailing_campaign_queue q
			USING doomed
			WHERE q.id = doomed.id
		`, cleanupBatchSize)
		cancel()
		if err != nil {
			log.Printf("[DataCleanup] terminal queue purge failed: %v", err)
			return
		}
		affected, _ := res.RowsAffected()
		total += affected
		if affected < int64(cleanupBatchSize) {
			break
		}
		// Pace between batches to keep WAL/IO pressure low on the hot queue.
		time.Sleep(100 * time.Millisecond)
	}
	if total > 0 {
		log.Printf("[DataCleanup] Purged %d operationally-terminal queue rows older than 14 days (row-age driven, cap %d/cycle)", total, terminalPurgeMaxRowsPerCycle)
	}
	dc.logTerminalQueueStats(ctx)
}

func (dc *DataCleanupWorker) logTerminalQueueStats(ctx context.Context) {
	queryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var terminalRemaining, acceptedHTML int64
	_ = dc.db.QueryRowContext(queryCtx, `
		SELECT COUNT(*)::bigint
		FROM mailing_campaign_queue
		WHERE status IN ('accepted', 'cancelled', 'failed', 'dead_letter', 'dead_letter_strict')
		  AND COALESCE(updated_at, created_at) < NOW() - INTERVAL '14 days'
	`).Scan(&terminalRemaining)
	_ = dc.db.QueryRowContext(queryCtx, `
		SELECT COUNT(*)::bigint
		FROM mailing_campaign_queue
		WHERE status = 'accepted' AND html_content IS NOT NULL
	`).Scan(&acceptedHTML)
	if terminalRemaining > 0 || acceptedHTML > 0 {
		log.Printf("[DataCleanup] Queue stats: terminal_aged_14d=%d accepted_with_html=%d",
			terminalRemaining, acceptedHTML)
	}
}

// ---------------------------------------------------------------------------
// slimAcceptedQueueHTML — NULL html_content/plain_content on accepted queue
// rows that still carry the rendered payload.
//
// The live send paths already slim on the accept transition (send_worker
// markSent, outbox_reconciler). This drains the HISTORICAL backlog of rows
// that reached 'accepted' before that path existed — the bloat StorageGuard's
// queue_accepted_html invariant flags (5,487,557 rows on 2026-06-07). Until
// drained, each row pins tens of KB of dead HTML in the table's TOAST, WAL,
// and backups.
//
// Two safety properties make this safe to run even while the primary is under
// an audience/segment IO storm:
//   - It is gated on primaryBusy(): if the primary already has heavy IO-wait
//     contention, the slim defers entirely for this cycle and retries next
//     hour once IO eases. WAL-heavy bulk UPDATEs never get added to a storm.
//   - It is bounded to slimMaxRowsPerCycle per cycle and paced (150ms between
//     batches), so a multi-million-row backlog drains gradually rather than in
//     one burst.
//
// Correctness does not depend on idx_mcq_accepted_html — that partial index
// (built CONCURRENTLY in runStartupMigrations) only makes the victim SELECT
// and StorageGuard's count index-only; without it the query falls back to
// idx_mcq_terminal_cleanup.
func (dc *DataCleanupWorker) slimAcceptedQueueHTML(ctx context.Context) {
	if dc.primaryBusy(ctx) {
		log.Printf("[DataCleanup] queue HTML slim deferred — primary under heavy IO load (>=%d backends in IO wait)", slimMaxIOWaitBackends)
		return
	}

	var slimmed int64
	for slimmed < slimMaxRowsPerCycle {
		if ctx.Err() != nil {
			break
		}
		queryCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		res, err := dc.db.ExecContext(queryCtx, `
			WITH victims AS (
				SELECT id
				FROM mailing_campaign_queue
				WHERE status = 'accepted'
				  AND html_content IS NOT NULL
				LIMIT $1
			)
			UPDATE mailing_campaign_queue q
			SET html_content = NULL,
			    plain_content = NULL
			FROM victims v
			WHERE q.id = v.id
		`, slimBatchSize)
		cancel()

		if err != nil {
			if isTableNotExistsError(err) {
				return
			}
			log.Printf("[DataCleanup] queue HTML slim error: %v", err)
			return
		}

		affected, _ := res.RowsAffected()
		if affected == 0 {
			break
		}
		slimmed += affected

		// Pace between batches to keep WAL/IO pressure low.
		time.Sleep(150 * time.Millisecond)
	}

	if slimmed > 0 {
		log.Printf("[DataCleanup] slimmed html_content from %d accepted queue rows this cycle (cap %d)", slimmed, slimMaxRowsPerCycle)
	}
}

// primaryBusy reports whether the primary is currently under heavy IO-wait
// contention, used to gate the WAL-heavy HTML slimmer. It fails CLOSED: if the
// lightweight pg_stat_activity probe cannot complete within 5s (itself a strong
// signal the primary is saturated), it reports busy so the slim defers.
func (dc *DataCleanupWorker) primaryBusy(ctx context.Context) bool {
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var ioWait int
	if err := dc.db.QueryRowContext(queryCtx, `
		SELECT COUNT(*)::int
		FROM pg_stat_activity
		WHERE datname = current_database()
		  AND state = 'active'
		  AND wait_event_type = 'IO'
		  AND pid <> pg_backend_pid()
	`).Scan(&ioWait); err != nil {
		log.Printf("[DataCleanup] primary load probe failed (%v) — deferring HTML slim", err)
		return true
	}
	return ioWait >= slimMaxIOWaitBackends
}

// ---------------------------------------------------------------------------
// cleanupPlanRecipients — Delete planned-recipient rows for TERMINAL campaigns
// older than 2 days. mailing_campaign_plan_recipients is the deploy-time
// audience snapshot consumed by the wave scheduler (EnqueuePMTAWave copies
// these into mailing_campaign_queue). Once a campaign reaches a terminal state
// (sent/cancelled/failed) the plan rows are dead weight (cancelled campaigns
// can't be un-cancelled; a redeploy of a failed/reused campaign re-plans fresh
// rows anyway), so a short window is safe and keeps this HOT table small.
//
// Retention was 14 days, but at ~450k+ planned recipients/day (plus redeploys
// and cancellations) that retained millions of terminal rows and the table
// re-bloated to ~8 GB (2026-06-22), whose cross-domain dedup scan times out the
// 20-min phase-2 deadline and silently fails the largest-audience brand's
// planning. 2 days keeps the live set to ~1-2 days of churn (a small, steady
// working set whose freed index pages get reused) while leaving a same-day
// debug/retry buffer. Active campaigns (scheduled / finalizing_audience /
// sending / preparing) are explicitly NOT touched: their plan rows are still
// being enqueued. Deletes are index-driven via idx_plan_recips_campaign and
// batched.
//
// NOTE: DELETE frees space for reuse but does NOT shrink the on-disk file;
// physical reclamation of pre-existing bloat is a one-time REINDEX/pg_repack
// (done manually 2026-06-22). With the 2-day window the live set stays small,
// so the indexes reach a bounded steady state without further reindexing.
//
// NOTE: this bounds growth and frees space for reuse; it does NOT shrink the
// on-disk file. Physical reclamation of existing bloat requires an online
// pg_repack — tracked separately.
// ---------------------------------------------------------------------------
func (dc *DataCleanupWorker) cleanupPlanRecipients(ctx context.Context) {
	// Post-cutover the table is RANGE-partitioned by created_at, so retention is
	// an instant DROP PARTITION (zero IO) via the maintenance functions, and we
	// keep ~3 weeks of future partitions provisioned. Pre-cutover (a regular
	// table) we fall back to the batched DELETE. Probing relkind keeps this safe
	// to deploy on either side of the partition swap.
	var partitioned bool
	if err := dc.db.QueryRowContext(ctx,
		`SELECT relkind = 'p' FROM pg_class WHERE relname = 'mailing_campaign_plan_recipients'`,
	).Scan(&partitioned); err != nil {
		log.Printf("[DataCleanup] plan_recipients relkind probe failed (%v); using DELETE fallback", err)
		partitioned = false
	}

	if partitioned {
		if _, err := dc.db.ExecContext(ctx,
			`SELECT ensure_plan_recipient_partitions(CURRENT_DATE, (CURRENT_DATE + INTERVAL '21 days')::date)`,
		); err != nil {
			log.Printf("[DataCleanup] ensure_plan_recipient_partitions failed: %v", err)
		}
		var dropped int
		if err := dc.db.QueryRowContext(ctx,
			`SELECT drop_old_plan_recipient_partitions(2)`,
		).Scan(&dropped); err != nil {
			log.Printf("[DataCleanup] drop_old_plan_recipient_partitions failed: %v", err)
			return
		}
		if dropped > 0 {
			log.Printf("[DataCleanup] dropped %d expired plan_recipient partition(s) (retention=14d, DROP PARTITION)", dropped)
		}
		return
	}

	// Campaign-driven delete: the old `pr.campaign_id IN (subquery) LIMIT n`
	// shape planned as a Seq Scan over the whole heap; once the matching rows
	// near the heap start were consumed, every batch re-scanned past the dead
	// prefix, blew the 60s batch timeout, and the cycle aborted with the table
	// still bloated (observed 13 GB / 29M rows on 2026-06-10, n_tup_del frozen
	// at ~550k). Pinning each DELETE to one campaign_id forces
	// idx_plan_recips_campaign regardless of table size.
	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	rows, err := dc.db.QueryContext(queryCtx, `
		SELECT c.id
		FROM mailing_campaigns c
		WHERE c.status IN ('sent', 'cancelled', 'failed')
		  AND COALESCE(c.updated_at, c.created_at) < NOW() - INTERVAL '2 days'
		  AND EXISTS (
			SELECT 1 FROM mailing_campaign_plan_recipients pr
			WHERE pr.campaign_id = c.id
		  )
		LIMIT $1
	`, planRecipMaxCampaignsPerCycle)
	cancel()
	if err != nil {
		if !isTableNotExistsError(err) {
			log.Printf("[DataCleanup] plan_recipients terminal-campaign scan failed: %v", err)
		}
		return
	}
	var campaignIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			campaignIDs = append(campaignIDs, id)
		}
	}
	rows.Close()

	var total int64
	for _, id := range campaignIDs {
		if ctx.Err() != nil {
			break
		}
		for {
			queryCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			res, err := dc.db.ExecContext(queryCtx, `
				WITH doomed AS (
					SELECT id FROM mailing_campaign_plan_recipients
					WHERE campaign_id = $1
					LIMIT $2
				)
				DELETE FROM mailing_campaign_plan_recipients pr
				USING doomed
				WHERE pr.id = doomed.id
			`, id, cleanupBatchSize)
			cancel()
			if err != nil {
				log.Printf("[DataCleanup] plan_recipients delete for campaign %s failed: %v", id, err)
				return
			}
			affected, _ := res.RowsAffected()
			total += affected
			if affected < int64(cleanupBatchSize) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if total > 0 {
		log.Printf("[DataCleanup] Removed %d plan-recipient rows across %d terminal campaigns older than 14 days (campaign-driven DELETE)", total, len(campaignIDs))
	}
}

// ---------------------------------------------------------------------------
// cleanupContentSnapshots — Delete hash-addressed wave creatives once no queue
// row references them and they're 30+ days old. Snapshots are tiny (one per
// distinct wave creative, ~15-200 KB) so this is hygiene, not pressure relief;
// the 30-day floor keeps recent waves auditable. The NOT EXISTS probe is
// index-driven via the partial idx_queue_content_snapshot_id, and the volume
// is small enough that a single batched pass per cycle suffices.
// ---------------------------------------------------------------------------
func (dc *DataCleanupWorker) cleanupContentSnapshots(ctx context.Context) {
	total := dc.batchDelete(ctx, "mailing_content_snapshots", `
		WITH doomed AS (
			SELECT s.id
			FROM mailing_content_snapshots s
			WHERE s.created_at < NOW() - INTERVAL '30 days'
			  AND NOT EXISTS (
				SELECT 1 FROM mailing_campaign_queue q
				WHERE q.content_snapshot_id = s.id
			  )
			LIMIT $1
		)
		DELETE FROM mailing_content_snapshots s
		USING doomed
		WHERE s.id = doomed.id
	`)
	if total > 0 {
		log.Printf("[DataCleanup] Removed %d unreferenced content snapshots older than 30 days", total)
	}
}

// cleanupESPMetricSnapshots bounds the PMTACollector's status/ip/domain
// snapshot history (esp_metric_snapshots, created 2026-06-10) to 30 days —
// the collector writes three rows per interval per server, forever.
func (dc *DataCleanupWorker) cleanupESPMetricSnapshots(ctx context.Context) {
	total := dc.batchDelete(ctx, "esp_metric_snapshots", `
		WITH doomed AS (
			SELECT id FROM esp_metric_snapshots
			WHERE collected_at < NOW() - INTERVAL '30 days'
			LIMIT $1
		)
		DELETE FROM esp_metric_snapshots s
		USING doomed
		WHERE s.id = doomed.id
	`)
	if total > 0 {
		log.Printf("[DataCleanup] Removed %d ESP metric snapshots older than 30 days", total)
	}
}

// cleanupProcessedAcctRaw removes processed accounting rows older than 14 days.
func (dc *DataCleanupWorker) cleanupProcessedAcctRaw(ctx context.Context) {
	total := dc.batchDelete(ctx, "pmta_acct_raw", `
		WITH doomed AS (
			SELECT id
			FROM pmta_acct_raw
			WHERE processed = TRUE
			  AND received_at < NOW() - INTERVAL '14 days'
			ORDER BY id
			LIMIT $1
		)
		DELETE FROM pmta_acct_raw r
		USING doomed
		WHERE r.id = doomed.id
	`)
	if total > 0 {
		log.Printf("[DataCleanup] Removed %d processed pmta_acct_raw rows older than 14 days", total)
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
