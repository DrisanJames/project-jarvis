package sendqueue

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/eventbus"
)

// =============================================================================
// LEDGER RECONCILER — SK-2 ONLY. NOT WIRED. NOT ON THE LIVE SEND PATH.
// =============================================================================
// UNWIRED 2026-09-01 (REQ-089). This reconciler is the partner of
// KafkaSendConsumer/processSend (consumer.go) — the SK-2 "send the mail FROM
// Kafka" prototype, which is constructed nowhere outside tests. It was
// nevertheless STARTED by cmd/server against the live SK-4 deployment, where
// nothing writes mailing_send_ledger: it ticked every 60s over an empty table
// for 18h while 219,237 produced-but-not-landed recipients sat invisible to it,
// and reported reconciler_running:true the whole time (send-transport SEV-1 #4).
//
// cmd/server now starts UnlandedWaveSweeper (reconciler.go) instead — the
// reconciler the SK-4 queue-writer path actually needs, which reconciles against
// mailing_campaign_plan_recipients and needs no ledger at all.
//
// Writer (processSend/reclaim) and reader (this file) are unwired TOGETHER, so
// the ledger is a coherent dormant subsystem rather than a half-state. Do not
// re-wire this without also wiring KafkaSendConsumer.
//
// Below is the original SK-2 documentation, unchanged.
//
// It closes the crash window in the claim -> send -> record sequence, mirroring
// worker/outbox_reconciler.go but keyed on the send ledger instead of
// mailing_campaign_queue.
// =============================================================================
// processSend's one unavoidable crash window: the process can die AFTER the
// sink accepted the message but BEFORE the RECORD (claimed -> sent) commits. A
// row stranded in 'claimed' is invisible to normal flow. A redelivery only
// re-fires if the offset was uncommitted — which is NOT guaranteed once the
// offset was committed via a skip path. So the reconciler makes re-drive
// GUARANTEED by re-PRODUCING the stored command back onto TopicSendCommands,
// rather than relying on Kafka to redeliver.
//
//   message_id IS NOT NULL -> the send succeeded and a message id was persisted;
//                             the record UPDATE crashed mid-flight. Commit it:
//                             status 'claimed' -> 'sent'. (No double-send: the
//                             message is already accepted.)
//
//   message_id IS NULL     -> the send either never completed or never returned
//   (stranded claimed,        a message id. RE-DRIVE it: re-produce the stored
//    or any 'failed'/         `command` JSON onto TopicSendCommands (keyed by
//    'requeued')              idempotency_key) and move the row to 'requeued'
//                             with a bumped attempts. When the re-produced record
//                             is consumed, processSend's reclaim path re-sends it;
//                             the INSERT ON CONFLICT + PMTA X-Idempotency backstop
//                             keep it single-delivery. After MaxRedriveAttempts a
//                             poison command is routed to terminal 'dead' so it
//                             can never loop forever.
//
// GAP 1 (the fix): previously stranded NULL rows were demoted to a TERMINAL
// 'failed' and NOTHING re-drove them — an orphaned row whose email had reached
// PMTA exactly once but whose ledger was inconsistent. Now there is no orphaned
// terminal failure: every stranded row is either re-driven (requeued) or, if it
// is poison, explicitly dead. The commit path (message_id present -> sent) is
// unchanged.

const (
	// DefaultReconcilerInterval mirrors worker.DefaultReconcilerInterval.
	DefaultReconcilerInterval = 60 * time.Second
	// DefaultReconcilerGrace mirrors worker.DefaultReconcilerGrace: how long a
	// row may sit 'claimed' before it is treated as stranded.
	DefaultReconcilerGrace = 10 * time.Minute
	// DefaultMaxRedriveAttempts caps how many times the reconciler will re-drive
	// a single key before declaring it poison ('dead'). attempts is bumped on
	// every claim/reclaim/re-drive, so this bounds total re-attempts for a key.
	DefaultMaxRedriveAttempts = 8
)

// reconcileCommitSQL commits crash-window sends: a stale 'claimed' row that
// already carries a message_id became 'sent' on the wire but the record UPDATE
// never landed. Flip it to 'sent' and stamp sent_at. UNCHANGED.
const reconcileCommitSQL = `
UPDATE mailing_send_ledger
SET status = 'sent', sent_at = COALESCE(sent_at, NOW())
WHERE status = 'claimed'
  AND claimed_at < NOW() - $1::interval
  AND message_id IS NOT NULL`

// selectStrandedSQL finds rows that need re-drive: a stranded 'claimed' row
// (no message_id, past grace) OR any 'failed'/'requeued' row past grace (a prior
// demote, or a re-drive whose re-produced record has not yet been consumed).
// It returns the stored command so the reconciler can re-produce it. Rows whose
// command is NULL (legacy, pre-GAP-1) cannot be re-driven by re-production — they
// are surfaced so the caller can dead-letter them rather than orphan them.
const selectStrandedSQL = `
SELECT idempotency_key, command, attempts
FROM mailing_send_ledger
WHERE (
        (status = 'claimed'  AND message_id IS NULL)
     OR  status = 'failed'
     OR (status = 'requeued' AND claimed_at < NOW() - $1::interval)
      )
  AND claimed_at < NOW() - $1::interval
LIMIT 1000`

// reconcileRequeueSQL marks a row as re-driven AFTER its command was re-produced:
// status -> 'requeued', bump attempts, stamp claimed_at NOW() (so it isn't
// re-swept until the next grace window). The status guard makes it a no-op if a
// live worker already moved the row (e.g. the re-produced record was consumed and
// the row is already 'claimed'/'sent'). worker_id is cleared so a re-consume
// treats it as a fresh ownerless re-claim.
const reconcileRequeueSQL = `
UPDATE mailing_send_ledger
SET status = 'requeued', attempts = attempts + 1, claimed_at = NOW(), worker_id = NULL
WHERE idempotency_key = $1
  AND status IN ('claimed','failed','requeued')`

// reconcileDeadSQL terminally dead-letters a key whose re-drive budget is
// exhausted (or whose command is NULL so it cannot be re-produced). It will
// never be re-sent; processSend skips+commits a 'dead' row so the partition
// drains. The status guard keeps it from clobbering a row a live worker revived.
const reconcileDeadSQL = `
UPDATE mailing_send_ledger
SET status = 'dead', attempts = attempts + 1
WHERE idempotency_key = $1
  AND status IN ('claimed','failed','requeued')`

// LedgerReconciler periodically resolves stranded ledger rows. Safe to run
// alongside live consumers — the status guards make every transition a no-op if
// a live worker already moved the row. It holds an eventbus.Producer so it can
// re-PRODUCE stored commands for guaranteed re-drive (GAP 1). A nil producer
// degrades gracefully: stranded re-driveable rows are left for Kafka redelivery
// (the pre-GAP-1 behavior) but are NOT dead-lettered, so nothing is lost.
type LedgerReconciler struct {
	db          *sql.DB
	prod        eventbus.Producer
	topic       string
	interval    time.Duration
	grace       time.Duration
	maxRedrives int
}

// NewLedgerReconciler constructs a reconciler with default timings and NO
// producer (re-drive falls back to Kafka redelivery; still no orphaned terminal
// failures because rows are kept requeued/claimed). Prefer
// NewLedgerReconcilerWithProducer in production.
func NewLedgerReconciler(db *sql.DB) *LedgerReconciler {
	return &LedgerReconciler{
		db: db, topic: TopicSendCommands,
		interval: DefaultReconcilerInterval, grace: DefaultReconcilerGrace,
		maxRedrives: DefaultMaxRedriveAttempts,
	}
}

// NewLedgerReconcilerWithConfig constructs a reconciler with explicit timings.
// Non-positive values fall back to defaults so a caller cannot accidentally
// disable it by passing zero.
func NewLedgerReconcilerWithConfig(db *sql.DB, interval, grace time.Duration) *LedgerReconciler {
	r := NewLedgerReconciler(db)
	if interval > 0 {
		r.interval = interval
	}
	if grace > 0 {
		r.grace = grace
	}
	return r
}

// NewLedgerReconcilerWithProducer is the PRODUCTION constructor: it wires the
// sendqueue producer so stranded work is re-driven by re-producing the stored
// command (GAP 1 — guaranteed re-drive). topic defaults to TopicSendCommands.
func NewLedgerReconcilerWithProducer(db *sql.DB, prod eventbus.Producer, interval, grace time.Duration) *LedgerReconciler {
	r := NewLedgerReconcilerWithConfig(db, interval, grace)
	r.prod = prod
	return r
}

// WithMaxRedrives overrides the re-drive attempt cap (mostly for tests).
func (r *LedgerReconciler) WithMaxRedrives(n int) *LedgerReconciler {
	if n > 0 {
		r.maxRedrives = n
	}
	return r
}

// Start blocks until ctx is cancelled. Runs one pass immediately on entry so a
// restart that coincides with a stranded row recovers within milliseconds.
func (r *LedgerReconciler) Start(ctx context.Context) {
	log.Printf("[LedgerReconciler] Starting (interval=%s, grace=%s, maxRedrives=%d, producer=%v)",
		r.interval, r.grace, r.maxRedrives, r.prod != nil)
	r.reconcileOnce(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("[LedgerReconciler] Stopping")
			return
		case <-ticker.C:
			r.reconcileOnce(ctx)
		}
	}
}

// reconcilerDB is the seam reconcilePass writes through, so it is testable with
// sqlmock. *sql.DB satisfies it.
type reconcilerDB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// reconcileOnce performs a single reconciliation pass under a timeout.
func (r *LedgerReconciler) reconcileOnce(ctx context.Context) {
	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	reconcilePass(queryCtx, r.db, r.prod, r.topic, r.grace, r.maxRedrives)
}

// reconcilePass is the broker-free, unit-tested body. It (1) commits
// message_id-present crash-window sends, then (2) re-drives every stranded
// re-driveable row by re-producing its stored command — capping at maxRedrives
// before dead-lettering. NOTHING is left orphaned in a terminal 'failed'.
func reconcilePass(ctx context.Context, db reconcilerDB, prod eventbus.Producer, topic string, grace time.Duration, maxRedrives int) {
	// (1) Commit crash-window sends (message_id present). UNCHANGED.
	if res, err := db.ExecContext(ctx, reconcileCommitSQL, grace.String()); err != nil {
		log.Printf("[LedgerReconciler] commit-window error: %v", err)
	} else if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[LedgerReconciler] committed %d crash-window sends (claimed->sent)", n)
	}

	// (2) Re-drive stranded re-driveable rows.
	rows, err := db.QueryContext(ctx, selectStrandedSQL, grace.String())
	if err != nil {
		log.Printf("[LedgerReconciler] select-stranded error: %v", err)
		return
	}
	type stranded struct {
		key     uuid.UUID
		cmd     []byte
		attempt int
	}
	var batch []stranded
	for rows.Next() {
		var s stranded
		var cmd []byte
		if err := rows.Scan(&s.key, &cmd, &s.attempt); err != nil {
			log.Printf("[LedgerReconciler] scan stranded: %v", err)
			continue
		}
		s.cmd = cmd
		batch = append(batch, s)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("[LedgerReconciler] iterate stranded: %v", err)
	}

	var requeued, dead int
	for _, s := range batch {
		// Poison / un-re-driveable: budget exhausted, no command to re-produce, or
		// no producer wired. Dead-letter rather than orphan or loop.
		if s.attempt >= maxRedrives || len(s.cmd) == 0 || prod == nil {
			// With no producer we still must not dead-letter prematurely: only
			// dead-letter on a genuine cap/poison; otherwise leave for next pass.
			if prod == nil && s.attempt < maxRedrives && len(s.cmd) > 0 {
				continue
			}
			if res, derr := db.ExecContext(ctx, reconcileDeadSQL, s.key); derr != nil {
				log.Printf("[LedgerReconciler] dead-letter key=%s error: %v", s.key, derr)
			} else if n, _ := res.RowsAffected(); n > 0 {
				dead++
				log.Printf("[LedgerReconciler] dead-lettered key=%s (attempts=%d, cmd=%dB)", s.key, s.attempt, len(s.cmd))
			}
			continue
		}

		// Re-PRODUCE the stored command, keyed by idempotency_key bytes (same
		// semantics as EnqueueSendCommands).
		k := make([]byte, len(s.key))
		copy(k, s.key[:])
		if perr := prod.Produce(ctx, topic, k, s.cmd); perr != nil {
			// Could not re-produce this pass; leave the row as-is and retry next
			// pass. Do NOT mark requeued (that would bump attempts without an
			// actual re-drive). Do NOT orphan.
			log.Printf("[LedgerReconciler] re-produce key=%s error: %v", s.key, perr)
			continue
		}
		if res, uerr := db.ExecContext(ctx, reconcileRequeueSQL, s.key); uerr != nil {
			log.Printf("[LedgerReconciler] requeue key=%s error: %v", s.key, uerr)
		} else if n, _ := res.RowsAffected(); n > 0 {
			requeued++
		}
	}
	if requeued > 0 || dead > 0 {
		log.Printf("[LedgerReconciler] re-drove %d stranded rows (requeued), dead-lettered %d", requeued, dead)
	}
}
