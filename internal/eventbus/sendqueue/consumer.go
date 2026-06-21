package sendqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/eventbus"
)

// ledgerDB is the tiny DB seam processSend writes through, so the core can be
// unit-tested with sqlmock (or any fake) and never needs a real *sql.DB.
// *sql.DB satisfies it (it has both QueryRowContext and ExecContext).
type ledgerDB interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// DefaultStaleGrace is how long a 'claimed' row may sit before another worker
// may take it over (the owner is presumed dead). Mirrors the conservative
// crash-window headroom of worker.DefaultReconcilerGrace.
const DefaultStaleGrace = 10 * time.Minute

// --- the EXACT SQL the invariants depend on (kept as named constants so the
// tests assert against the same strings the consumer runs) --------------------

// claimSQL atomically CLAIMS a key: insert-if-absent. ON CONFLICT DO NOTHING +
// RETURNING is the serialization point — under concurrent INSERTs of the same
// key, exactly one transaction inserts (RETURNING a row); every other gets zero
// rows. This is the no-double-send primitive.
//
// GAP 1: the marshaled SendCommand is persisted into the `command` JSONB column
// at claim time so the row is SELF-SUFFICIENT for guaranteed re-drive — the
// LedgerReconciler can re-PRODUCE this command rather than orphaning a 'failed'
// row that depends on Kafka redelivery.
const claimSQL = `
INSERT INTO mailing_send_ledger
    (idempotency_key, campaign_id, subscriber_id, wave_id, status, worker_id, claimed_at, attempts, command)
VALUES ($1, $2, $3, $4, 'claimed', $5, NOW(), 1, $6)
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING idempotency_key`

// statusSQL reads the current status (+ stale-detection inputs) of a conflicting
// row so the loser of the claim can decide skip / take-over / re-claim.
const statusSQL = `
SELECT status, worker_id, (claimed_at < NOW() - $2::interval) AS stale
FROM mailing_send_ledger
WHERE idempotency_key = $1`

// reclaimSQL re-claims a 'failed'/'requeued' (or stale 'claimed') row for a
// fresh send attempt: flips it back to 'claimed', takes ownership, bumps
// attempts. The status guard makes it a no-op if a concurrent worker already
// moved the row. It also backfills the `command` JSONB ($4) when the stored
// command is NULL (e.g. a legacy row claimed before GAP 1, or a row the
// reconciler requeued) so the row stays self-sufficient for future re-drive.
const reclaimSQL = `
UPDATE mailing_send_ledger
SET status = 'claimed', worker_id = $2, claimed_at = NOW(), attempts = attempts + 1, message_id = NULL,
    command = COALESCE(command, $4)
WHERE idempotency_key = $1 AND status = $3
RETURNING idempotency_key`

// recordSentSQL is the single atomic commit point: claimed -> sent carrying the
// message id. The status='claimed' guard makes the transition idempotent — a
// concurrent worker or a crash-recovery replay cannot flip a row to 'sent'
// twice, mirroring worker.markSent's prior-status guard.
const recordSentSQL = `
UPDATE mailing_send_ledger
SET status = 'sent', message_id = $2, sent_at = NOW()
WHERE idempotency_key = $1 AND status = 'claimed'`

// processSend runs the EXACT claim -> send -> record sequence for ONE command.
// It is pure (DB + sink injected), broker-free, and the unit-tested core of the
// consumer. The harness calls it directly AND drives it through the consumer.
//
// Return contract (drives offset commit at the call site):
//   - (skipped=true,  err=nil): definitively handled WITHOUT a send (dedup or a
//     live owner). Safe to COMMIT the offset.
//   - (skipped=false, err=nil): we claimed, sent, and recorded 'sent'. Safe to
//     COMMIT the offset.
//   - (_, err!=nil): something failed (DB or sink). The ledger is left 'claimed'
//     (so the reconciler / a redelivery retries) and the caller MUST NOT commit
//     — the record redelivers. No drop.
func processSend(ctx context.Context, db ledgerDB, sink SendSink, workerID string, staleGrace time.Duration, cmd SendCommand) (skipped bool, err error) {
	if cmd.IdempotencyKey == uuid.Nil {
		// Un-claimable: cannot be deduped. Treat as a poison record — skip it so
		// it does not wedge the partition forever. (EnqueueSendCommands refuses
		// to produce these; this guards a hand-crafted/corrupt record.)
		return true, nil
	}
	if staleGrace <= 0 {
		staleGrace = DefaultStaleGrace
	}

	// Marshal the command ONCE so the ledger row is self-sufficient for the
	// reconciler's guaranteed re-drive (GAP 1). A marshal failure is a real error
	// (do not commit); the redelivery retries.
	cmdJSON, mErr := json.Marshal(cmd)
	if mErr != nil {
		return false, fmt.Errorf("sendqueue: marshal command for ledger key=%s: %w", cmd.IdempotencyKey, mErr)
	}

	// ① CLAIM
	var claimedKey uuid.UUID
	err = db.QueryRowContext(ctx, claimSQL,
		cmd.IdempotencyKey, nullUUID(cmd.CampaignID), nullUUID(cmd.SubscriberID), nullUUID(cmd.WaveID), workerID, cmdJSON,
	).Scan(&claimedKey)
	switch {
	case err == nil:
		// We inserted the row: we own this key. Fall through to ② SEND.
	case errors.Is(err, sql.ErrNoRows):
		// Conflict: the key already exists. Decide based on its status.
		var status, ownerID string
		var stale bool
		if serr := db.QueryRowContext(ctx, statusSQL, cmd.IdempotencyKey, staleGrace.String()).Scan(&status, ownerStringPtr(&ownerID), &stale); serr != nil {
			return false, fmt.Errorf("sendqueue: read status for key=%s: %w", cmd.IdempotencyKey, serr)
		}
		switch status {
		case "sent":
			// Already sent — pure dedup. Skip and commit. The sink is NEVER called.
			return true, nil
		case "claimed":
			// Another worker owns it, OR this is our own redelivery, OR the owner
			// is stranded. If it's ours (we're retrying after a crash) or it's
			// gone stale, take it over and re-send (PMTA idempotency makes that
			// safe). Otherwise a live owner has it — skip and commit.
			if ownerID == workerID || stale {
				if rerr := reclaim(ctx, db, cmd.IdempotencyKey, workerID, "claimed", cmdJSON); rerr != nil {
					return false, rerr
				}
				// fall through to ② SEND below
			} else {
				return true, nil // live owner; not ours; not stale -> skip+commit
			}
		case "failed", "requeued":
			// A prior attempt failed (or the reconciler demoted/re-drove a stranded
			// row). Re-claim and re-send. 'requeued' is the reconciler's re-drive
			// state (GAP 1): it re-produced the stored command and parked the row in
			// 'requeued'; consuming that re-produced record lands here and re-sends.
			if rerr := reclaim(ctx, db, cmd.IdempotencyKey, workerID, status, cmdJSON); rerr != nil {
				return false, rerr
			}
			// fall through to ② SEND below
		case "dead":
			// Terminal: the reconciler exhausted the re-drive budget (poison
			// command). Do NOT re-send. Skip+commit so the partition drains.
			return true, nil
		default:
			return false, fmt.Errorf("sendqueue: unknown ledger status %q for key=%s", status, cmd.IdempotencyKey)
		}
	default:
		// Real DB error on the claim. Do not commit; redelivery retries.
		return false, fmt.Errorf("sendqueue: claim key=%s: %w", cmd.IdempotencyKey, err)
	}

	// ② SEND. carries IdempotencyKey (the PMTA X-Idempotency header is the
	// backstop). On error: leave the ledger 'claimed' (reconciler / redelivery
	// retries) and DO NOT commit the offset.
	messageID, sendErr := sink.Send(ctx, cmd)
	if sendErr != nil {
		return false, fmt.Errorf("sendqueue: send key=%s: %w", cmd.IdempotencyKey, sendErr)
	}

	// ③ RECORD: claimed -> sent. The status='claimed' guard makes this idempotent.
	if _, rerr := db.ExecContext(ctx, recordSentSQL, cmd.IdempotencyKey, messageID); rerr != nil {
		// Send happened but the record write failed. Leave the row 'claimed'
		// (message_id is still NULL in the DB) — the reconciler will see a stale
		// claimed/NULL row and demote it to 'failed' for a safe re-send, OR a
		// redelivery re-claims it. Do NOT commit.
		return false, fmt.Errorf("sendqueue: record sent key=%s: %w", cmd.IdempotencyKey, rerr)
	}

	// ④ COMMIT happens at the call site, ONLY now that ③ succeeded.
	return false, nil
}

// reclaim runs reclaimSQL and verifies a row was actually re-claimed. If the
// guarded UPDATE affected zero rows, a concurrent worker won the race for this
// key — we must NOT proceed to send (that would be a double-send), so we return
// an error that leaves the offset uncommitted; the redelivery will re-evaluate
// and find the row 'sent'/'claimed-by-other' and skip.
func reclaim(ctx context.Context, db ledgerDB, key uuid.UUID, workerID, fromStatus string, cmdJSON []byte) error {
	var got uuid.UUID
	err := db.QueryRowContext(ctx, reclaimSQL, key, workerID, fromStatus, cmdJSON).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("sendqueue: reclaim lost race for key=%s (from=%s)", key, fromStatus)
	}
	if err != nil {
		return fmt.Errorf("sendqueue: reclaim key=%s (from=%s): %w", key, fromStatus, err)
	}
	return nil
}

// nullUUID maps a zero UUID to SQL NULL (the ledger columns are nullable). A
// non-zero UUID is passed through as the uuid.UUID (lib/pq handles it).
func nullUUID(u uuid.UUID) any {
	if u == uuid.Nil {
		return nil
	}
	return u
}

// ownerStringPtr lets the worker_id column (nullable TEXT) scan into a string,
// defaulting a SQL NULL owner to "". It wraps a *string so Scan can write
// through it.
type ownerStringScanner struct{ s *string }

func (o ownerStringScanner) Scan(src any) error {
	if src == nil {
		*o.s = ""
		return nil
	}
	switch v := src.(type) {
	case string:
		*o.s = v
	case []byte:
		*o.s = string(v)
	default:
		*o.s = fmt.Sprintf("%v", v)
	}
	return nil
}

func ownerStringPtr(s *string) ownerStringScanner { return ownerStringScanner{s: s} }

// KafkaSendConsumer drives processSend through the eventbus Consumer framework
// (manual offset commit). It SHIPS SHADOW-capable: the default sink is NoOpSink,
// so a dark deploy sends NO real email. It is NOT wired into cmd/ here.
type KafkaSendConsumer struct {
	db         ledgerDB
	sink       SendSink
	workerID   string
	staleGrace time.Duration
	cfg        eventbus.Config

	consumer *eventbus.Consumer
	dlqProd  eventbus.Producer
}

// NewKafkaSendConsumer constructs the consumer with deps explicit. A nil sink
// defaults to NoOpSink (the shadow default). workerID identifies this instance
// in the ledger (so its own redeliveries are recognized); empty defaults to a
// random id. cfg supplies brokers/group/topics for Start; it can be the zero
// Config in handler unit tests (which call Handle/processSend directly).
func NewKafkaSendConsumer(db ledgerDB, sink SendSink, workerID string, cfg eventbus.Config) *KafkaSendConsumer {
	if sink == nil {
		sink = NoOpSink{} // shadow default: no real email
	}
	if workerID == "" {
		workerID = "sendqueue-" + uuid.NewString()
	}
	return &KafkaSendConsumer{
		db:         db,
		sink:       sink,
		workerID:   workerID,
		staleGrace: DefaultStaleGrace,
		cfg:        cfg,
	}
}

// Handle is the eventbus.Handler: unmarshal the SendCommand and run processSend.
// A skip and a successful send both return nil (offset committable). A failure
// returns the error so the framework retries and, ultimately, does NOT commit —
// the no-drop half of the contract. key is the idempotency-key bytes; we read
// identity from the payload (authoritative), so key is unused here.
func (c *KafkaSendConsumer) Handle(ctx context.Context, _, value []byte) error {
	var cmd SendCommand
	if err := json.Unmarshal(value, &cmd); err != nil {
		// A corrupt record can never be deduped or sent; route it to the DLQ via
		// the framework (returning an error after MaxRetries parks it) rather
		// than wedging the partition. We do NOT skip-commit poison silently.
		return fmt.Errorf("sendqueue: unmarshal command: %w", err)
	}
	_, err := processSend(ctx, c.db, c.sink, c.workerID, c.staleGrace, cmd)
	return err
}

// Start constructs the real group consumer and runs its loop in a goroutine. It
// is a no-op (returns nil) when the bus is disabled (no brokers) — the
// dark-by-default path. The DLQ parks dead records on send.commands.v1.dlq.
func (c *KafkaSendConsumer) Start(ctx context.Context, dlq eventbus.DLQ) error {
	if !c.cfg.Enabled() {
		return nil
	}
	cfg := c.cfg
	if len(cfg.Topics) == 0 {
		cfg.Topics = []string{TopicSendCommands}
	}
	con, err := eventbus.NewConsumer(cfg, c.Handle, dlq, eventbus.ConsumerOptions{})
	if err != nil {
		return fmt.Errorf("sendqueue: consumer: %w", err)
	}
	c.consumer = con
	go func() { _ = con.Run(ctx) }()
	return nil
}

// Stop closes the consumer and any DLQ producer it owns. Safe when never started.
func (c *KafkaSendConsumer) Stop() {
	if c.consumer != nil {
		_ = c.consumer.Close()
	}
	if c.dlqProd != nil {
		_ = c.dlqProd.Close()
	}
}
