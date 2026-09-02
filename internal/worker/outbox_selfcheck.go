package worker

// =============================================================================
// OUTBOX SELF-CHECK — Continuous invariant checking for the durable injection
//                     outbox, with SMS alerts for operator paging.
// =============================================================================
// The durable outbox turns send intent into a DB-backed state machine (queued
// -> submitting -> accepted / failed_retryable / dead_letter). The reconciler
// closes the obvious crash-between-ESP-ACK-and-DB-COMMIT window, but there are
// several softer invariants that can silently degrade the system:
//
//   1. submitting rows held past reconciler grace — means the reconciler is
//      failing to promote or requeue. Could be a long-running transaction, a
//      replica-lag misconfiguration, or a hung worker pool.
//
//   2. dead-letter rate spike — a burst of permanent failures in a single
//      hour is usually a broken template, a purge-suppression mismatch, or
//      an authentication problem with a sending domain.
//
//   3. queued backlog above the backpressure ceiling — campaigns are enqueuing
//      faster than send workers can drain. If sustained, accepted deliverability
//      quotas will slip.
//
//   4. oldest queued age — a queue that's backing up because workers can't keep
//      up vs. a queue that has a stuck head because of a priority inversion.
//
// Every tick, the self-check runs a handful of bounded aggregate queries,
// evaluates each invariant, and fires exactly one SMS per crossed threshold
// per re-alert window. No per-message alerting (we already have that via the
// campaign health monitor); the self-check is explicitly a system-health
// sentinel, not a per-campaign incident router.
//
// The self-check is safe to run regardless of OutboxMode: in legacy mode the
// 'submitting' state never sees writes so those checks are no-ops. Dead-letter
// and queued-backlog invariants are useful in both modes.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
)

const (
	// DefaultSelfCheckInterval is the between-tick delay for the self-check.
	// 5 minutes is the sweet spot: frequent enough to catch a runaway
	// dead-letter burst within a single wave, rare enough that the queries
	// don't add material load.
	DefaultSelfCheckInterval = 5 * time.Minute

	// DefaultSelfCheckReAlert is the per-alert suppression window. Once the
	// self-check pages for a given invariant it will not page again for that
	// invariant until this much time passes, even if the condition remains.
	// Stops a broken template from generating a stream of identical SMSes.
	DefaultSelfCheckReAlert = 30 * time.Minute

	// Invariant thresholds. Chosen to be wide enough that the self-check
	// doesn't cry wolf during normal peak sends, but tight enough that an
	// actual outage trips within one interval.
	submittingStuckThresholdSec    = 600 // 10 minutes
	deadLetterRatePerHourThreshold = 500 // permanent fails / hr / system
	queuedBacklogThreshold         = 150000
	oldestQueuedAgeThresholdSec    = 3600 // 1 hour

	// selfCheckJanitorBatch bounds how many zombie 'queued' rows the
	// terminal-parent janitor cancels per tick. Keeps the sweep cheap and
	// non-blocking even if a huge cold send leaves a large tail behind.
	selfCheckJanitorBatch = 10000

	// selfCheckJanitorParentBatch bounds how many distinct campaigns the
	// terminal-parent janitor considers per tick. The janitor now evaluates a
	// produced-vs-landed safety rule per parent BEFORE cancelling anything
	// (see cancelTerminalParentQueued), so the parent set has to be bounded
	// independently of the row batch. Measured on prod 2026-09-01: 29 distinct
	// campaigns hold the entire 140k-row 'queued' population, and the
	// per-parent produced/landed probe costs 338ms for 100 parents.
	selfCheckJanitorParentBatch = 100

	// selfCheckZombieCampaignBatch bounds the scheduled-campaign zombie janitor
	// (REQ-087 DoD 5). Prod population 2026-09-01: 137 rows (29 aug07 + 105
	// aug14 + 3 aug23), so one tick clears the backlog and every later tick is
	// a no-op.
	selfCheckZombieCampaignBatch = 200

	// selfCheckZombieCampaignAgeDays is how far past scheduled_at a campaign
	// still sitting in 'scheduled' has to be before the janitor writes it off.
	// 7 days is far beyond any legitimate lateness (the CampaignHealthMonitor
	// pages at 5 minutes) and beyond any board's own re-deploy cycle.
	selfCheckZombieCampaignAgeDays = 7

	// waveUnlandedScanLimit bounds how many completed waves the wave_unlanded
	// invariant examines per tick, newest-first. Without a bound the anti-join
	// walks every wave completed in the 6h window (31,253 on 2026-09-01) and
	// the check itself becomes the load problem: measured 31.9s unbounded vs
	// 791ms at this limit, on the same connection, minutes apart.
	waveUnlandedScanLimit = 5000

	// campaignNoSendGraceMin is how long a campaign may sit in 'sending' with
	// queued_count>0 and sent_count=0 before it is a wedge rather than a slow
	// start. 15 min is ~4 send-worker batch cycles at any realistic rate.
	campaignNoSendGraceMin = 15

	// failedBurstMultiple / failedBurstFloor define the campaign-failure burst
	// invariant: the trailing hour must exceed BOTH the floor and the multiple
	// of the 7-day hourly median. The floor exists because the median is
	// frequently 0-3 (779 failed campaigns over 7 days on 2026-09-01, median
	// 3/h), and a bare 3x on a 0 median pages on a single unrelated failure.
	failedBurstMultiple = 3.0
	failedBurstFloor    = 10

	// selfCheckQueryTimeout is the transaction-local statement_timeout applied
	// to every self-check query. The app's main pool runs at the global 30s
	// ceiling (main.go: statement_timeout=30000), but these aggregates scan the
	// large mailing_campaign_queue and legitimately exceed 30s whenever RDS IO
	// is saturated by concurrent audience/segment evaluation or an import-driven
	// ISP backfill. When that happens at the global ceiling every check (and the
	// janitor) is cancelled, so the queue never gets cleaned and the monitor goes
	// blind. Raising the ceiling for just these queries keeps them alive under
	// load (2026-06-07 incident — same IO storm that stalled the partner drip).
	selfCheckQueryTimeout = 120 * time.Second
)

// liveQueuedClause restricts a queued-row aggregate to rows that are genuinely
// actionable. A 'queued' row only signals a real scheduler/worker problem if
// BOTH its campaign and its wave are still live. Two distinct zombie classes
// otherwise age forever and trip the oldest-queued / backlog invariants for a
// non-issue (2026-06-07 incident):
//
//  1. Parent campaign reached a terminal state (completed/cancelled/failed/sent)
//     but its leftover queue rows were never drained. ('paused' is excluded too —
//     those rows are intentionally held, not stalled.)
//
// The WAVE-terminal branch was REMOVED here 2026-09-01 (REQ-087 DoD 2). It was
// the same predicate the *cancel* path had already shed on 2026-06-18: the
// dispatcher marks a wave 'completed' the instant it ENQUEUES, so "row under a
// completed wave" describes EVERY row of every board and sidecar campaign for
// the whole of its send window. With it, the two monitors that exist to notice
// a stalled queue could not see the wave path at all — the exact blind spot
// that let the 2026-09-01 SK-4 transport wedge run 90 minutes unpaged. The
// campaign clause is kept: it is the one that genuinely retires zombie rows.
const liveQueuedClause = `
		  AND EXISTS (
			SELECT 1 FROM mailing_campaigns c
			WHERE c.id = q.campaign_id
			  AND c.status NOT IN ('completed','cancelled','failed','sent','paused')
		  )`

// selfCheckInvariantKey identifies each distinct alert channel so per-alert
// suppression is scoped correctly. Never exposed outside the package.
type selfCheckInvariantKey string

const (
	invSubmittingStuck   selfCheckInvariantKey = "submitting_stuck"
	invDeadLetterSpike   selfCheckInvariantKey = "dead_letter_spike"
	invQueuedBacklog     selfCheckInvariantKey = "queued_backlog"
	invOldestQueuedStuck selfCheckInvariantKey = "oldest_queued_stuck"

	// The four send-liveness invariants (REQ-087). Every one of them detects an
	// ABSENCE — mail that should exist and does not — which is precisely what
	// the four queue-row aggregates above structurally cannot see.
	invWaveUnlanded   selfCheckInvariantKey = "wave_unlanded"
	invCampaignNoSend selfCheckInvariantKey = "campaign_no_send"
	invFailedBurst    selfCheckInvariantKey = "failed_burst"
	invScheduledDead  selfCheckInvariantKey = "scheduled_dead"
)

// SendLivenessSnapshot is the send-path liveness gauge published on /health as
// `send_liveness`. It is refreshed by the OutboxSelfCheck tick (5 min) and read
// lock-free by the health handler, mirroring CurrentEventBusStatus(). Fields
// that a failed probe could not compute stay zero and the probe's name is
// appended to Errors — a consumer must treat `sent_last_15m: 0` with a
// non-empty Errors as UNKNOWN, not as "nothing is sending".
type SendLivenessSnapshot struct {
	// UnlandedWaves/UnlandedRecipients: waves marked 'completed' with a
	// non-zero enqueued_recipients for which not one queue row exists. On the
	// direct-INSERT path this is always 0 (rows land in the wave's own TX); on
	// the Kafka route it is the produced-but-not-landed backlog.
	UnlandedWaves      int64 `json:"unlanded_waves"`
	UnlandedRecipients int64 `json:"unlanded_recipients"`
	// SentLast15m/LastSentAt come from mailing_message_log, which is written
	// once per actual handoff to a transport — the one number that cannot be
	// faked by a wave or a queue row.
	SentLast15m int64      `json:"sent_last_15m"`
	LastSentAt  *time.Time `json:"last_sent_at"`
	// QueueReadyRows is the live 'queued' depth (liveQueuedClause) — work the
	// send pool still owes. Zero here WITH a non-zero UnlandedRecipients is the
	// SK-4 signature.
	QueueReadyRows int64      `json:"queue_ready_rows"`
	CheckedAt      *time.Time `json:"checked_at"`
	Errors         []string   `json:"errors,omitempty"`
}

// sendLiveness holds the most recent SendLivenessSnapshot. atomic.Value keeps
// /health off the self-check's DB path entirely: the handler never blocks and
// never opens a connection (the /health fast path has no DB access by design).
var sendLiveness atomic.Value

// CurrentSendLiveness returns the latest published snapshot. Before the first
// tick completes it returns the zero value with a nil CheckedAt — which reads
// on /health as "not yet measured", never as "healthy".
func CurrentSendLiveness() SendLivenessSnapshot {
	if v, ok := sendLiveness.Load().(SendLivenessSnapshot); ok {
		return v
	}
	return SendLivenessSnapshot{}
}

// selfCheckJanitorDisabled is the kill switch for BOTH janitors on this tick
// (terminal-parent queue rows and zombie 'scheduled' campaigns). Set
// OUTBOX_SELFCHECK_JANITOR_DISABLED=true to leave every row exactly where it
// is; the read-only invariants keep running and keep paging. This exists
// because the janitor is the only part of the self-check that WRITES, and on
// 2026-09-01 it cancelled 80,514 live recipients under campaigns that had been
// flipped terminal while their mail was still in flight. Matching the
// DISABLE_* idiom in pmta_wave_dispatcher.go.
func selfCheckJanitorDisabled() bool {
	return os.Getenv("OUTBOX_SELFCHECK_JANITOR_DISABLED") == "true"
}

// OutboxSelfCheck evaluates durable-outbox invariants on a ticker and pages
// via SMS when a threshold is crossed. Alert suppression is per-invariant so
// a broken subsystem doesn't drown out unrelated alerts.
type OutboxSelfCheck struct {
	db       *sql.DB
	interval time.Duration
	reAlert  time.Duration

	alerter    SMSAlerter
	recipients []string

	mu         sync.Mutex
	lastAlerts map[selfCheckInvariantKey]time.Time
}

// NewOutboxSelfCheck constructs a self-check with default timings. The Twilio
// alerter and recipient list are set after construction via SetAlerter so the
// self-check can still be started even when SMS alerting is disabled (the
// queries still fire; they log rather than page).
func NewOutboxSelfCheck(db *sql.DB) *OutboxSelfCheck {
	return &OutboxSelfCheck{
		db:         db,
		interval:   DefaultSelfCheckInterval,
		reAlert:    DefaultSelfCheckReAlert,
		lastAlerts: make(map[selfCheckInvariantKey]time.Time),
	}
}

// NewOutboxSelfCheckWithConfig lets tests plug in tighter timings. Zero /
// negative values fall back to defaults so callers can't accidentally disable.
func NewOutboxSelfCheckWithConfig(db *sql.DB, interval, reAlert time.Duration) *OutboxSelfCheck {
	if interval <= 0 {
		interval = DefaultSelfCheckInterval
	}
	if reAlert <= 0 {
		reAlert = DefaultSelfCheckReAlert
	}
	return &OutboxSelfCheck{
		db:         db,
		interval:   interval,
		reAlert:    reAlert,
		lastAlerts: make(map[selfCheckInvariantKey]time.Time),
	}
}

// SetAlerter wires a Twilio client + recipient list. Passing nil alerter or
// empty recipients leaves alerting disabled — invariant violations still log
// via the standard logger so operators can see them in CloudWatch.
func (c *OutboxSelfCheck) SetAlerter(alerter SMSAlerter, recipients []string) {
	if alerter == nil || len(recipients) == 0 {
		c.alerter = nil
		c.recipients = nil
		return
	}
	c.alerter = alerter
	c.recipients = append([]string(nil), recipients...)
}

// Start blocks until ctx is cancelled. Runs an immediate check on entry so
// a server restart that coincides with a degraded state reports within
// milliseconds rather than waiting an interval.
func (c *OutboxSelfCheck) Start(ctx context.Context) {
	log.Printf("[OutboxSelfCheck] starting (interval=%s, re_alert=%s, recipients=%d)",
		c.interval, c.reAlert, len(c.recipients))
	c.runOnce(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("[OutboxSelfCheck] stopping")
			return
		case <-ticker.C:
			c.runOnce(ctx)
		}
	}
}

// runOnce evaluates every invariant in sequence. A failure on one check does
// not short-circuit the others — we want operators to see every problem each
// tick surfaces, not just the first.
func (c *OutboxSelfCheck) runOnce(ctx context.Context) {
	// Hygiene first: terminalize zombie 'queued' rows so the age/backlog signals
	// below reflect only live work. Each step manages its own per-query budget
	// via runWithTimeout, so a slow sweep can't starve the checks.
	c.cancelTerminalParentQueued(ctx)
	c.cancelZombieScheduledCampaigns(ctx)

	c.checkSubmittingStuck(ctx)
	c.checkDeadLetterSpike(ctx)
	backlog := c.checkQueuedBacklog(ctx)
	c.checkOldestQueuedStuck(ctx)

	// Send-liveness invariants (REQ-087). Read-only; each publishes into the
	// snapshot so /health carries the same numbers the pager acts on.
	snap := SendLivenessSnapshot{QueueReadyRows: backlog.value}
	if backlog.err != nil {
		snap.Errors = append(snap.Errors, "queue_ready_rows")
	}
	c.checkWaveUnlanded(ctx, &snap)
	c.checkCampaignNoSend(ctx)
	c.checkFailedBurst(ctx)
	c.checkScheduledDead(ctx)
	c.sampleSendThroughput(ctx, &snap)

	now := time.Now().UTC()
	snap.CheckedAt = &now
	sendLiveness.Store(snap)
}

// selfCheckCount carries a scalar invariant result plus whether the query that
// produced it actually ran — so a failed probe is never published as a zero.
type selfCheckCount struct {
	value int64
	err   error
}

// runWithTimeout runs fn inside a read-committed transaction whose
// statement_timeout is raised to selfCheckQueryTimeout via SET LOCAL. Because
// SET LOCAL is transaction-scoped, the pooled connection returns to the pool
// with the app's default 30s ceiling intact — no cross-query leak. It also
// derives a child context bounded just above the statement_timeout so a wedged
// connection can't hang the tick indefinitely. Mirrors the partner-drip
// orchestrator's withDBTimeout.
func (c *OutboxSelfCheck) runWithTimeout(parent context.Context, fn func(ctx context.Context, tx *sql.Tx) error) error {
	ctx, cancel := context.WithTimeout(parent, selfCheckQueryTimeout+10*time.Second)
	defer cancel()

	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", selfCheckQueryTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("set statement_timeout: %w", err)
	}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// cancelTerminalParentQueued sweeps a bounded batch of abandoned 'queued' rows
// and marks them 'cancelled'. A row is abandoned ONLY when its parent CAMPAIGN
// reached a terminal state (completed/cancelled/failed/sent) but leftover queue
// rows were never drained — at that point no worker will ever claim them, since
// the send pool claims strictly WHERE campaign.status='sending'.
//
// The WAVE-terminal branch was REMOVED 2026-06-18 (operator). It was the root
// cause of mass silent under-delivery: the dispatcher marks a wave 'completed'
// the instant it ENQUEUES (not when sent), so during a wave's normal send window
// every still-'queued' row sits under a 'completed' wave while the campaign is
// still 'sending' and the workers are actively draining it. The wave-branch
// cancelled those LIVE rows out from under the send pool — a race the janitor
// won for any wave enqueued above the drain rate (jun17 W3/W4: ~89k/87k rows
// cancelled, attempts=0, never sent). Dispatch is keyed on the CAMPAIGN, not the
// wave, so a 'completed' wave under a 'sending' campaign is never abandoned.
// The "campaign completion is itself drain-gated" assumption that used to close
// this comment was FALSE for the Kafka route and cost 80,514 recipients on
// 2026-09-01: a routed wave is 'completed' at PRODUCE time, so completion saw an
// empty queue while 0-40% of the audience was still parked in Kafka, flipped the
// campaign 'sent', and this janitor then cancelled every row as it landed. The
// completion predicate is now itself gated on landed >= produced (REQ-082 DoD 1,
// cmd/server/main.go complete_finished_campaigns + campaign_scheduler.go), and
// this janitor carries the same rule as a SECOND, independent guard:
//
//	produced = SUM(waves.enqueued_recipients) for the campaign
//	landed   = COUNT(mailing_campaign_queue rows) for the campaign
//
// A campaign with produced > landed still has recipients in flight somewhere
// off-table, so NOTHING under it is cancelled, whatever its status says. (When
// REQ-089 lands waves.landed_recipients, produced/landed become per-wave columns
// and this stops being an inferred count.)
//
// The sweep is three bounded statements rather than one so the safety rule is
// evaluated in Go and can be proven by test (a single UPDATE ... WHERE would hide
// it inside the plan where no sqlmock test can reach it):
//
//	A. distinct campaigns holding 'queued' rows        (index-only, ~300ms/140k rows)
//	B. status + produced + landed for those campaigns  (~338ms for 100 parents)
//	C. bounded UPDATE for the parents that pass        (index scan, SKIP LOCKED)
//
// Re-run safety (two ECS tasks): C is idempotent — it only moves 'queued' to
// 'cancelled', so a second task re-running the same batch matches zero rows, and
// SKIP LOCKED means the two tasks never wait on each other. A/B are read-only.
func (c *OutboxSelfCheck) cancelTerminalParentQueued(ctx context.Context) {
	if selfCheckJanitorDisabled() {
		log.Println("[OutboxSelfCheck] terminal-parent janitor DISABLED (OUTBOX_SELFCHECK_JANITOR_DISABLED=true)")
		return
	}

	// A — which campaigns are holding 'queued' rows at all.
	var parents []string
	err := c.runWithTimeout(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
		SELECT q.campaign_id, COUNT(*)::bigint
		FROM mailing_campaign_queue q
		WHERE q.status = 'queued'
		  AND q.campaign_id IS NOT NULL
		GROUP BY q.campaign_id
		ORDER BY 2 DESC
		LIMIT $1
	`, selfCheckJanitorParentBatch)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var n int64
			if err := rows.Scan(&id, &n); err != nil {
				return err
			}
			parents = append(parents, id)
		}
		return rows.Err()
	})
	if err != nil {
		log.Printf("[OutboxSelfCheck] terminal-parent janitor (parents) failed: %v", err)
		return
	}
	if len(parents) == 0 {
		return
	}

	// B — of those, which are terminal AND have every produced recipient landed.
	var (
		victims  []string
		inflight int
	)
	err = c.runWithTimeout(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
		SELECT c.id,
		       COALESCE((SELECT SUM(w.enqueued_recipients) FROM mailing_campaign_waves w
		                  WHERE w.campaign_id = c.id), 0)::bigint AS produced,
		       (SELECT COUNT(*) FROM mailing_campaign_queue q
		         WHERE q.campaign_id = c.id)::bigint             AS landed
		FROM mailing_campaigns c
		WHERE c.id = ANY($1)
		  AND c.status IN ('completed','cancelled','failed','sent')
	`, pq.Array(parents))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var produced, landed int64
			if err := rows.Scan(&id, &produced, &landed); err != nil {
				return err
			}
			if produced > landed {
				// Recipients produced but not landed — in flight, do not touch.
				inflight++
				log.Printf("[OutboxSelfCheck] terminal-parent janitor SKIPPING campaign %s: produced=%d > landed=%d (recipients still in flight)",
					id, produced, landed)
				continue
			}
			victims = append(victims, id)
		}
		return rows.Err()
	})
	if err != nil {
		log.Printf("[OutboxSelfCheck] terminal-parent janitor (produced/landed) failed: %v", err)
		return
	}
	if inflight > 0 {
		log.Printf("[OutboxSelfCheck] terminal-parent janitor skipped %d campaign(s) with recipients in flight", inflight)
	}
	if len(victims) == 0 {
		return
	}

	// C — cancel a bounded batch under the campaigns that passed the rule.
	var affected int64
	err = c.runWithTimeout(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
		WITH victims AS (
			SELECT q.id
			FROM mailing_campaign_queue q
			WHERE q.status = 'queued'
			  AND q.campaign_id = ANY($1)
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE mailing_campaign_queue q
		SET status = 'cancelled',
		    updated_at = NOW(),
		    error_message = COALESCE(NULLIF(q.error_message, ''), '') ||
		                    ' [outbox-selfcheck janitor: terminal campaign]'
		FROM victims v
		WHERE q.id = v.id
	`, pq.Array(victims), selfCheckJanitorBatch)
		if err != nil {
			return err
		}
		affected, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		log.Printf("[OutboxSelfCheck] terminal-parent janitor failed: %v", err)
		return
	}
	if affected > 0 {
		log.Printf("[OutboxSelfCheck] terminal-parent janitor cancelled %d zombie queued row(s) under %d campaign(s)", affected, len(victims))
	}
}

// cancelZombieScheduledCampaigns writes off campaigns that have been sitting in
// 'scheduled' for more than a week with nothing left that could ever dispatch
// them (REQ-087 DoD 5). The dispatcher only ever picks up waves in
// planned/enqueuing/dispatched, so a 'scheduled' campaign with none of those is
// inert by construction — it cannot send, it carries phantom total_recipients
// into every board count, and the CampaignHealthMonitor re-pages on it forever
// (469 lateness stamps in 30 days, 139 in one day, mostly for 29 rows from
// 2026-08-07).
//
// The DoD names "0 waves"; the predicate here is "no LIVE wave", which strictly
// contains it. Measured on prod 2026-09-01: 137 campaigns are >7d late, and
// exactly 0 of them have zero waves — all 8,669 of their waves are 'cancelled'.
// The narrow predicate would have cancelled nothing and left the zombie count
// unchanged, which is the outcome the DoD's own post-check forbids.
//
// mailing_campaigns has no error_message column (VERIFIED against
// information_schema on prod), so the reason is merged into the pmta_config
// JSONB, which is additive and only ever touched on campaigns being written off.
//
// Re-run safety (two ECS tasks): the UPDATE moves 'scheduled' to 'cancelled', so
// a second run over the same batch matches nothing; SKIP LOCKED keeps the two
// tasks from blocking each other. Bounded at selfCheckZombieCampaignBatch/tick.
func (c *OutboxSelfCheck) cancelZombieScheduledCampaigns(ctx context.Context) {
	if selfCheckJanitorDisabled() {
		return
	}
	var affected int64
	err := c.runWithTimeout(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, fmt.Sprintf(`
		WITH victims AS (
			SELECT c.id
			FROM mailing_campaigns c
			WHERE c.status = 'scheduled'
			  AND c.scheduled_at < NOW() - INTERVAL '%d days'
			  AND NOT EXISTS (
			    SELECT 1 FROM mailing_campaign_waves w
			    WHERE w.campaign_id = c.id
			      AND w.status IN ('planned','enqueuing','dispatched')
			  )
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE mailing_campaigns c
		SET status = 'cancelled',
		    completed_at = COALESCE(c.completed_at, NOW()),
		    updated_at = NOW(),
		    pmta_config = COALESCE(c.pmta_config, '{}'::jsonb) ||
		                  jsonb_build_object('selfcheck_cancel_reason',
		                    'outbox-selfcheck zombie janitor: scheduled >%dd with no live wave')
		FROM victims v
		WHERE c.id = v.id
	`, selfCheckZombieCampaignAgeDays, selfCheckZombieCampaignAgeDays), selfCheckZombieCampaignBatch)
		if err != nil {
			return err
		}
		affected, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		log.Printf("[OutboxSelfCheck] zombie-scheduled janitor failed: %v", err)
		return
	}
	if affected > 0 {
		log.Printf("[OutboxSelfCheck] zombie-scheduled janitor cancelled %d campaign(s) stuck in 'scheduled' >%dd with no live wave",
			affected, selfCheckZombieCampaignAgeDays)
	}
}

// checkSubmittingStuck looks for rows that have been in 'submitting' beyond
// the reconciler's grace window. If the reconciler is healthy this should
// never return a non-zero value; if it does, it means the reconciler is
// failing to promote or requeue and every stuck row is a potential
// double-send or silent drop.
func (c *OutboxSelfCheck) checkSubmittingStuck(ctx context.Context) {
	var oldestSec int64
	var count int64
	err := c.runWithTimeout(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
		SELECT
			COALESCE(COUNT(*), 0),
			COALESCE(EXTRACT(EPOCH FROM (NOW() - MIN(locked_at)))::bigint, 0)
		FROM mailing_campaign_queue
		WHERE status = 'submitting'
		  AND locked_at IS NOT NULL
		  AND locked_at < NOW() - INTERVAL '10 minutes'
	`).Scan(&count, &oldestSec)
	})
	if err != nil {
		log.Printf("[OutboxSelfCheck] submitting-stuck query failed: %v", err)
		return
	}
	if count == 0 {
		return
	}
	msg := fmt.Sprintf(
		"[Project Jarvis] Outbox invariant breach: %d row(s) stuck in submitting state (oldest %ds). Reconciler may be failing; check /api/outbox/summary and logs.",
		count, oldestSec,
	)
	c.maybeAlert(ctx, invSubmittingStuck, msg)
}

// checkDeadLetterSpike looks for an unusual rate of permanent failures in the
// last hour. A single broken template or an auth failure on a sending domain
// will cause this to climb quickly. The threshold is absolute (per system, not
// per campaign) because the self-check is a system-health sentinel — the
// per-campaign pager is the existing CampaignHealthMonitor.
func (c *OutboxSelfCheck) checkDeadLetterSpike(ctx context.Context) {
	var count int64
	err := c.runWithTimeout(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
		SELECT COUNT(*)::bigint
		FROM mailing_campaign_queue
		WHERE status IN ('dead_letter','dead_letter_strict','failed_permanent')
		  AND COALESCE(last_attempt_at, created_at) > NOW() - INTERVAL '1 hour'
	`).Scan(&count)
	})
	if err != nil {
		log.Printf("[OutboxSelfCheck] dead-letter-spike query failed: %v", err)
		return
	}
	if count <= deadLetterRatePerHourThreshold {
		return
	}
	msg := fmt.Sprintf(
		"[Project Jarvis] Outbox invariant breach: %d permanent failures in the last hour (threshold %d). Likely broken template, auth failure, or DNS regression.",
		count, deadLetterRatePerHourThreshold,
	)
	c.maybeAlert(ctx, invDeadLetterSpike, msg)
}

// checkQueuedBacklog looks for a queue depth above the backpressure ceiling.
// The existing BackpressureMonitor will already be throttling enqueues at
// 100k, but this self-check exists because the backpressure signal tells
// enqueue to pause — it doesn't tell operators a sustained backup is
// happening. A 150k+ depth sustained across two ticks is a paging event.
// Restricted to live rows (see liveQueuedClause) so abandoned rows from
// finished campaigns/waves don't inflate the depth toward a false backlog alarm.
// Returns the depth it measured so runOnce can publish it as
// send_liveness.queue_ready_rows without paying for a second aggregate.
func (c *OutboxSelfCheck) checkQueuedBacklog(ctx context.Context) selfCheckCount {
	var count int64
	err := c.runWithTimeout(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
		SELECT COUNT(*)::bigint FROM mailing_campaign_queue q
		WHERE q.status = 'queued'`+liveQueuedClause+`
	`).Scan(&count)
	})
	if err != nil {
		log.Printf("[OutboxSelfCheck] queued-backlog query failed: %v", err)
		return selfCheckCount{err: err}
	}
	if count <= queuedBacklogThreshold {
		return selfCheckCount{value: count}
	}
	msg := fmt.Sprintf(
		"[Project Jarvis] Outbox invariant breach: queued backlog %d (threshold %d). Send workers may be stalled or backpressure saturated.",
		count, queuedBacklogThreshold,
	)
	c.maybeAlert(ctx, invQueuedBacklog, msg)
	return selfCheckCount{value: count}
}

// checkOldestQueuedStuck distinguishes "queue is deep because of a hot send"
// from "queue has a stuck head because scheduling is broken". A single row
// older than 1 hour in 'queued' usually means the scheduler stopped firing
// or a specific campaign's priority was set wrong. Restricted to live rows
// (see liveQueuedClause) so a zombie row from a long-finished campaign or a
// closed-out wave can't trip it forever.
func (c *OutboxSelfCheck) checkOldestQueuedStuck(ctx context.Context) {
	var ageSec int64
	err := c.runWithTimeout(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
		SELECT COALESCE(EXTRACT(EPOCH FROM (NOW() - MIN(scheduled_at)))::bigint, 0)
		FROM mailing_campaign_queue q
		WHERE q.status = 'queued'
		  AND q.scheduled_at IS NOT NULL`+liveQueuedClause+`
	`).Scan(&ageSec)
	})
	if err != nil {
		log.Printf("[OutboxSelfCheck] oldest-queued query failed: %v", err)
		return
	}
	if ageSec <= oldestQueuedAgeThresholdSec {
		return
	}
	msg := fmt.Sprintf(
		"[Project Jarvis] Outbox invariant breach: oldest queued row is %ds old (threshold %ds). Scheduler or send worker pool may be stalled.",
		ageSec, oldestQueuedAgeThresholdSec,
	)
	c.maybeAlert(ctx, invOldestQueuedStuck, msg)
}

// ---------------------------------------------------------------------------
// Send-liveness invariants (REQ-087)
//
// The four checks above all measure PRESENT queue rows. Every one of them reads
// perfectly healthy when the failure is that no rows exist at all — which is the
// shape of every silent send failure this platform has actually had: the
// 2026-09-01 SK-4 Kafka wedge (waves completed, 0 rows, 0 sends, 0 errors, 90
// minutes), the 2026-08-27 variant_name collision (scheduled, 0 recipients), the
// 2026-08-14 zero-recipient board. The four below detect absence.
// ---------------------------------------------------------------------------

// checkWaveUnlanded is the wave-path invariant: a wave the dispatcher marked
// 'completed' with a non-zero enqueued_recipients, for which not a single queue
// row exists. On the direct set-based path the rows are written in the wave's own
// transaction, so this is structurally 0. On the Kafka route the wave is
// 'completed' at PRODUCE time (pmta_wave_dispatcher.go), so a stalled or wedged
// QueueWriterConsumer shows up here — and nowhere else — within one tick.
//
// Window: 5 min of grace for a healthy consumer to drain, 6 h at the far end
// because the queue purge is 14 days (data_cleanup.go) — absence inside 6 h can
// never be a purge. Scan bounded to the newest waveUnlandedScanLimit waves so the
// check can never become the load problem (31.9s unbounded vs 791ms bounded,
// measured on prod 2026-09-01); the partial index idx_waves_completed_at in
// criticalSendPathDDL turns the candidate scan into a range read.
func (c *OutboxSelfCheck) checkWaveUnlanded(ctx context.Context, snap *SendLivenessSnapshot) {
	var waves, recipients int64
	err := c.runWithTimeout(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
		WITH cand AS (
			SELECT w.id,
			       GREATEST(w.produced_recipients - w.landed_recipients, 0) AS unlanded,
			       w.enqueued_recipients
			FROM mailing_campaign_waves w
			WHERE (
			        -- REQ-089: a routed wave parks in 'produced' until every
			        -- command lands; the gap is exact, no queue probe needed.
			        (w.status = 'produced'  AND w.produced_recipients > w.landed_recipients)
			        -- pre-REQ-089 shape: waves the previous binary marked
			        -- 'completed' at produce time during the rollout window.
			     OR (w.status = 'completed' AND w.enqueued_recipients > 0)
			      )
			  AND w.completed_at >= NOW() - INTERVAL '6 hours'
			  AND w.completed_at <= NOW() - INTERVAL '5 minutes'
			ORDER BY w.completed_at DESC
			LIMIT $1
		)
		SELECT COALESCE(COUNT(*), 0)::bigint,
		       COALESCE(SUM(GREATEST(cand.unlanded, cand.enqueued_recipients)), 0)::bigint
		FROM cand
		WHERE cand.unlanded > 0
		   OR NOT EXISTS (
			SELECT 1 FROM mailing_campaign_queue q WHERE q.wave_id = cand.id
		)
	`, waveUnlandedScanLimit).Scan(&waves, &recipients)
	})
	if err != nil {
		log.Printf("[OutboxSelfCheck] wave-unlanded query failed: %v", err)
		snap.Errors = append(snap.Errors, "unlanded_waves")
		return
	}
	snap.UnlandedWaves = waves
	snap.UnlandedRecipients = recipients
	if waves == 0 {
		return
	}
	msg := fmt.Sprintf(
		"[Project Jarvis] SEND LIVENESS breach: %d wave(s) completed with %d recipients but ZERO queue rows landed (5m-6h window). The send-queue consumer or the enqueue path is wedged — check /health send_liveness and event_bus.send_queue.",
		waves, recipients,
	)
	c.maybeAlert(ctx, invWaveUnlanded, msg)
}

// checkCampaignNoSend is the transport-path invariant: a campaign that is
// 'sending', has recipients queued, and has sent NOTHING well past the point
// where the first batch should have cleared. It catches a wedge BELOW the queue
// (PMTA bridge down, SES relay refusing, Kumo injector unreachable) that
// checkWaveUnlanded cannot see because the rows landed correctly.
// Pure campaign-counter read, no join — 7.6ms on prod.
func (c *OutboxSelfCheck) checkCampaignNoSend(ctx context.Context) {
	var count int64
	err := c.runWithTimeout(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COUNT(*)::bigint
		FROM mailing_campaigns c
		WHERE c.status = 'sending'
		  AND COALESCE(c.queued_count, 0) > 0
		  AND COALESCE(c.sent_count, 0) = 0
		  AND c.started_at IS NOT NULL
		  AND c.started_at < NOW() - INTERVAL '%d minutes'
	`, campaignNoSendGraceMin)).Scan(&count)
	})
	if err != nil {
		log.Printf("[OutboxSelfCheck] campaign-no-send query failed: %v", err)
		return
	}
	if count == 0 {
		return
	}
	msg := fmt.Sprintf(
		"[Project Jarvis] SEND LIVENESS breach: %d campaign(s) 'sending' with queued recipients and sent_count=0 for over %dm. Transport (PMTA/SES/Kumo) or the send worker pool is wedged.",
		count, campaignNoSendGraceMin,
	)
	c.maybeAlert(ctx, invCampaignNoSend, msg)
}

// checkFailedBurst pages when campaigns are failing far faster than this
// platform's own baseline. Absolute thresholds are useless here — the drip lanes
// legitimately fail 20-75 campaigns a day — so the comparison is against the
// 7-day hourly median of the same signal, with an absolute floor so a median of
// 0 cannot turn one unrelated failure into a page. 12ms on prod.
func (c *OutboxSelfCheck) checkFailedBurst(ctx context.Context) {
	var current int64
	var median float64
	err := c.runWithTimeout(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
		WITH hourly AS (
			SELECT date_trunc('hour', COALESCE(c.completed_at, c.updated_at)) AS h,
			       COUNT(*)::bigint AS n
			FROM mailing_campaigns c
			WHERE c.status = 'failed'
			  AND COALESCE(c.completed_at, c.updated_at) >= NOW() - INTERVAL '7 days'
			  AND COALESCE(c.completed_at, c.updated_at) <  date_trunc('hour', NOW())
			GROUP BY 1
		)
		SELECT
			(SELECT COUNT(*)::bigint FROM mailing_campaigns c2
			  WHERE c2.status = 'failed'
			    AND COALESCE(c2.completed_at, c2.updated_at) > NOW() - INTERVAL '1 hour'),
			COALESCE((SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY n) FROM hourly), 0)::float8
	`).Scan(&current, &median)
	})
	if err != nil {
		log.Printf("[OutboxSelfCheck] failed-burst query failed: %v", err)
		return
	}
	if current < failedBurstFloor || float64(current) <= failedBurstMultiple*median {
		return
	}
	msg := fmt.Sprintf(
		"[Project Jarvis] SEND LIVENESS breach: %d campaign(s) moved to 'failed' in the last hour vs a 7-day hourly median of %.1f (threshold %.0fx, floor %d). Likely a planner/deploy regression failing campaigns en masse.",
		current, median, failedBurstMultiple, failedBurstFloor,
	)
	c.maybeAlert(ctx, invFailedBurst, msg)
}

// checkScheduledDead catches the deploy-time silent failures: a campaign that
// reached 'scheduled', is past its own send time, and either planned zero
// recipients or produced no wave at all. Both are the signature of a payload
// that the planner rejected quietly (the time_spans[*].source footgun, the
// 2026-08-27 variant_name collision, an empty audience) — the campaign looks
// scheduled on every screen and will never mail. 0.07ms on prod (index scan on
// idx_campaigns_scheduled).
func (c *OutboxSelfCheck) checkScheduledDead(ctx context.Context) {
	var zeroRecipients, noWave int64
	err := c.runWithTimeout(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT
			COUNT(*) FILTER (WHERE COALESCE(c.total_recipients, 0) = 0)::bigint,
			COUNT(*) FILTER (WHERE NOT EXISTS (
				SELECT 1 FROM mailing_campaign_waves w WHERE w.campaign_id = c.id
			))::bigint
		FROM mailing_campaigns c
		WHERE c.status = 'scheduled'
		  AND c.scheduled_at >= NOW() - INTERVAL '24 hours'
		  AND c.scheduled_at <= NOW() - INTERVAL '%d minutes'
	`, campaignNoSendGraceMin)).Scan(&zeroRecipients, &noWave)
	})
	if err != nil {
		log.Printf("[OutboxSelfCheck] scheduled-dead query failed: %v", err)
		return
	}
	if zeroRecipients == 0 && noWave == 0 {
		return
	}
	msg := fmt.Sprintf(
		"[Project Jarvis] SEND LIVENESS breach: %d scheduled campaign(s) past send time with 0 recipients and %d with no wave (last 24h). Deploy produced a campaign that cannot mail — check the planner payload.",
		zeroRecipients, noWave,
	)
	c.maybeAlert(ctx, invScheduledDead, msg)
}

// sampleSendThroughput fills the two gauge fields that answer "is mail actually
// moving right now" without any inference. mailing_message_log is written once
// per handoff to a transport by the send worker (send_worker.go markSent), so it
// is the one counter a wave, a queue row, or a campaign status cannot fake. Both
// halves ride idx_message_log_sent — 8.3ms on prod.
//
// Read-only. It publishes; it never pages: the paging decision on a zero here
// belongs to checkWaveUnlanded/checkCampaignNoSend, which know whether anything
// was supposed to be sending.
func (c *OutboxSelfCheck) sampleSendThroughput(ctx context.Context, snap *SendLivenessSnapshot) {
	var sent15m int64
	var lastSent sql.NullTime
	err := c.runWithTimeout(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*)::bigint FROM mailing_message_log WHERE sent_at > NOW() - INTERVAL '15 minutes'),
			(SELECT MAX(sent_at) FROM mailing_message_log)
	`).Scan(&sent15m, &lastSent)
	})
	if err != nil {
		log.Printf("[OutboxSelfCheck] send-throughput query failed: %v", err)
		snap.Errors = append(snap.Errors, "sent_last_15m")
		return
	}
	snap.SentLast15m = sent15m
	if lastSent.Valid {
		t := lastSent.Time.UTC()
		snap.LastSentAt = &t
	}
}

// maybeAlert honours per-invariant re-alert suppression. The log line always
// fires so CloudWatch reflects every breach; the SMS only fires if the
// suppression window has elapsed.
func (c *OutboxSelfCheck) maybeAlert(ctx context.Context, key selfCheckInvariantKey, msg string) {
	log.Println(msg)

	if c.alerter == nil || len(c.recipients) == 0 {
		return
	}

	c.mu.Lock()
	last, seen := c.lastAlerts[key]
	if seen && time.Since(last) < c.reAlert {
		c.mu.Unlock()
		return
	}
	c.lastAlerts[key] = time.Now()
	c.mu.Unlock()

	for _, to := range c.recipients {
		to = strings.TrimSpace(to)
		if to == "" {
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if _, err := c.alerter.SendSMS(sendCtx, to, msg); err != nil {
			log.Printf("[OutboxSelfCheck] SMS send to %s failed: %v", to, err)
		}
		cancel()
	}
}
