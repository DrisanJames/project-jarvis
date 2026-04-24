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
	"strings"
	"sync"
	"time"
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
	submittingStuckThresholdSec    = 600   // 10 minutes
	deadLetterRatePerHourThreshold = 500   // permanent fails / hr / system
	queuedBacklogThreshold         = 150000
	oldestQueuedAgeThresholdSec    = 3600 // 1 hour
)

// selfCheckInvariantKey identifies each distinct alert channel so per-alert
// suppression is scoped correctly. Never exposed outside the package.
type selfCheckInvariantKey string

const (
	invSubmittingStuck    selfCheckInvariantKey = "submitting_stuck"
	invDeadLetterSpike    selfCheckInvariantKey = "dead_letter_spike"
	invQueuedBacklog      selfCheckInvariantKey = "queued_backlog"
	invOldestQueuedStuck  selfCheckInvariantKey = "oldest_queued_stuck"
)

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
	queryCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	c.checkSubmittingStuck(queryCtx)
	c.checkDeadLetterSpike(queryCtx)
	c.checkQueuedBacklog(queryCtx)
	c.checkOldestQueuedStuck(queryCtx)
}

// checkSubmittingStuck looks for rows that have been in 'submitting' beyond
// the reconciler's grace window. If the reconciler is healthy this should
// never return a non-zero value; if it does, it means the reconciler is
// failing to promote or requeue and every stuck row is a potential
// double-send or silent drop.
func (c *OutboxSelfCheck) checkSubmittingStuck(ctx context.Context) {
	var oldestSec int64
	var count int64
	err := c.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(COUNT(*), 0),
			COALESCE(EXTRACT(EPOCH FROM (NOW() - MIN(locked_at)))::bigint, 0)
		FROM mailing_campaign_queue
		WHERE status = 'submitting'
		  AND locked_at IS NOT NULL
		  AND locked_at < NOW() - INTERVAL '10 minutes'
	`).Scan(&count, &oldestSec)
	if err != nil {
		log.Printf("[OutboxSelfCheck] submitting-stuck query failed: %v", err)
		return
	}
	if count == 0 {
		return
	}
	msg := fmt.Sprintf(
		"[IGNITE] Outbox invariant breach: %d row(s) stuck in submitting state (oldest %ds). Reconciler may be failing; check /api/outbox/summary and logs.",
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
	err := c.db.QueryRowContext(ctx, `
		SELECT COUNT(*)::bigint
		FROM mailing_campaign_queue
		WHERE status IN ('dead_letter','dead_letter_strict','failed_permanent')
		  AND COALESCE(last_attempt_at, created_at) > NOW() - INTERVAL '1 hour'
	`).Scan(&count)
	if err != nil {
		log.Printf("[OutboxSelfCheck] dead-letter-spike query failed: %v", err)
		return
	}
	if count <= deadLetterRatePerHourThreshold {
		return
	}
	msg := fmt.Sprintf(
		"[IGNITE] Outbox invariant breach: %d permanent failures in the last hour (threshold %d). Likely broken template, auth failure, or DNS regression.",
		count, deadLetterRatePerHourThreshold,
	)
	c.maybeAlert(ctx, invDeadLetterSpike, msg)
}

// checkQueuedBacklog looks for a queue depth above the backpressure ceiling.
// The existing BackpressureMonitor will already be throttling enqueues at
// 100k, but this self-check exists because the backpressure signal tells
// enqueue to pause — it doesn't tell operators a sustained backup is
// happening. A 150k+ depth sustained across two ticks is a paging event.
func (c *OutboxSelfCheck) checkQueuedBacklog(ctx context.Context) {
	var count int64
	err := c.db.QueryRowContext(ctx, `
		SELECT COUNT(*)::bigint FROM mailing_campaign_queue WHERE status = 'queued'
	`).Scan(&count)
	if err != nil {
		log.Printf("[OutboxSelfCheck] queued-backlog query failed: %v", err)
		return
	}
	if count <= queuedBacklogThreshold {
		return
	}
	msg := fmt.Sprintf(
		"[IGNITE] Outbox invariant breach: queued backlog %d (threshold %d). Send workers may be stalled or backpressure saturated.",
		count, queuedBacklogThreshold,
	)
	c.maybeAlert(ctx, invQueuedBacklog, msg)
}

// checkOldestQueuedStuck distinguishes "queue is deep because of a hot send"
// from "queue has a stuck head because scheduling is broken". A single row
// older than 1 hour in 'queued' usually means the scheduler stopped firing
// or a specific campaign's priority was set wrong.
func (c *OutboxSelfCheck) checkOldestQueuedStuck(ctx context.Context) {
	var ageSec int64
	err := c.db.QueryRowContext(ctx, `
		SELECT COALESCE(EXTRACT(EPOCH FROM (NOW() - MIN(scheduled_at)))::bigint, 0)
		FROM mailing_campaign_queue
		WHERE status = 'queued'
		  AND scheduled_at IS NOT NULL
	`).Scan(&ageSec)
	if err != nil {
		log.Printf("[OutboxSelfCheck] oldest-queued query failed: %v", err)
		return
	}
	if ageSec <= oldestQueuedAgeThresholdSec {
		return
	}
	msg := fmt.Sprintf(
		"[IGNITE] Outbox invariant breach: oldest queued row is %ds old (threshold %ds). Scheduler or send worker pool may be stalled.",
		ageSec, oldestQueuedAgeThresholdSec,
	)
	c.maybeAlert(ctx, invOldestQueuedStuck, msg)
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
