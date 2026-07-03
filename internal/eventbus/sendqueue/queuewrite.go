package sendqueue

// queuewrite.go is the SK-4 "Kafka as the PRIMARY send transport" core: it lets
// EnqueuePMTAWave route a wave's recipients through Kafka INSTEAD of inserting
// them directly into mailing_campaign_queue, WITHOUT changing the send path.
//
// THE INVARIANT (the whole point — do not violate): the row a routed wave puts
// on Kafka, after passing through the QueueWriterConsumer, is BYTE-IDENTICAL to
// the row the un-routed wave would INSERT directly. The proven send path
// (SendWorkerPool.processItem) then claims and sends it UNCHANGED — it cannot
// tell whether the row arrived via the direct INSERT or via Kafka.
//
// To make "identical" enforceable rather than aspirational, the INSERT SQL lives
// HERE, ONCE (queueInsertSQL), and is run BOTH by EnqueuePMTAWave (its direct
// path) and by the QueueWriterConsumer. There is exactly one INSERT string, so
// the two paths can never drift.
//
// IDEMPOTENCY: the INSERT is `ON CONFLICT (idempotency_key) ... DO NOTHING`, the
// SAME partial-unique-index guard the dispatcher already relies on. Kafka is
// at-least-once; a redelivered command re-runs the INSERT and the conflict makes
// it a no-op — no duplicate queue row, no double-send (the send path dedups on
// the same queue row). The consumer commits the offset ONLY after the INSERT
// succeeds, so nothing is dropped.

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

// nullString maps an EMPTY string to SQL NULL (so the set-based path's empty
// html/plain become the dispatcher's literal `NULL`); a non-empty string passes
// through. This is the parity primitive for the nullable text columns.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullUUIDString maps "" or a non-UUID string to SQL NULL, and a valid UUID
// string to the parsed uuid.UUID — mirroring the dispatcher's normalizeSourceID
// for audience_source_id (a value that parses as a UUID is kept, anything else
// becomes NULL).
func nullUUIDString(s string) any {
	if s == "" {
		return nil
	}
	parsed, err := uuid.Parse(s)
	if err != nil {
		return nil
	}
	return parsed
}

// unixToTime converts a Unix epoch (seconds) to a time.Time for scheduled_at.
func unixToTime(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

// TopicQueueWrites aliases TopicSendCommands: SK-4 reuses the proven
// send.commands.v1 topic and its keyed-by-idempotency-key producer. Kept as a
// named alias so wiring/consumer/tests read intentionally.
const TopicQueueWrites = TopicSendCommands

// queueInsertSQL is the ONE canonical INSERT into mailing_campaign_queue. It is
// the exact statement enqueueWaveSetBased / enqueueWaveRowAtATime run for the
// direct (un-routed) path, lifted here so the routed (Kafka) path runs the SAME
// bytes. Column list and ON CONFLICT clause MUST match the dispatcher's INSERT.
//
// Parameter mapping (see SendCommand → args in QueueInsertArgs):
//
//	$1  id (mailing_campaign_queue.id)        $9  recipient_isp
//	$2  campaign_id                           $10 selection_rank
//	$3  subscriber_id                         $11 audience_source_type
//	$4  subject                               $12 audience_source_id (nullable uuid)
//	$5  html_content (nullable)               $13 plain_content (nullable)
//	$6  scheduled_at                          $14 idempotency_key
//	$7  isp_plan_id                           $15 content_snapshot_id (nullable uuid)
//	$8  wave_id                               $16 priority
//
// html_content/plain_content are passed as NULL for the set-based path (body in
// the snapshot) and as the inline strings for the legacy path — exactly mirroring
// the two direct INSERTs. content_snapshot_id is NULL for the legacy path.
const queueInsertSQL = `
INSERT INTO mailing_campaign_queue (
	id, campaign_id, subscriber_id, subject, html_content, plain_content,
	status, priority, scheduled_at, created_at, isp_plan_id, wave_id,
	recipient_isp, selection_rank, audience_source_type, audience_source_id,
	idempotency_key, content_snapshot_id, creative_id
) VALUES (
	$1, $2, $3, $4, $5, $13,
	'queued', $16, $6, NOW(), $7, $8,
	$9, $10, $11, $12,
	$14, $15, $17
)
ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`

// queueExecer is the tiny DB seam the queue INSERT runs through, so the writer
// is unit-testable with sqlmock and works against either *sql.DB or *sql.Tx.
type queueExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// QueueInsertArgs builds the ordered argument list for queueInsertSQL from a
// SendCommand. It applies the SAME NULL semantics as the direct INSERT:
//   - html/plain: a non-empty string is passed through; an EMPTY string becomes
//     SQL NULL (the set-based path leaves them empty → NULL, exactly like the
//     dispatcher's `NULL, NULL`). The legacy path always carries non-empty HTML.
//   - audience_source_id / content_snapshot_id: a zero/empty value becomes NULL.
//   - priority: 0 → the dispatcher's literal default of 5.
//   - scheduled_at: 0 → NULL (the column default/now applies); EnqueuePMTAWave
//     always populates it, so this only guards a hand-crafted command.
//   - queue id: uuid.Nil → a fresh random id, mirroring the dispatcher's
//     uuid.New() per row. A populated QueueID is honored for stable identity.
func QueueInsertArgs(cmd SendCommand) []any {
	queueID := cmd.QueueID
	if queueID == uuid.Nil {
		queueID = uuid.New()
	}
	priority := cmd.Priority
	if priority == 0 {
		priority = 5
	}
	var scheduledAt any
	if cmd.ScheduledAtUnix != 0 {
		scheduledAt = unixToTime(cmd.ScheduledAtUnix)
	} else {
		scheduledAt = nil
	}
	return []any{
		queueID,                              // $1 id
		nullUUID(cmd.CampaignID),             // $2 campaign_id
		nullUUID(cmd.SubscriberID),           // $3 subscriber_id
		cmd.Subject,                          // $4 subject
		nullString(cmd.HTMLContent),          // $5 html_content
		scheduledAt,                          // $6 scheduled_at
		nullUUID(cmd.ISPPlanID),              // $7 isp_plan_id
		nullUUID(cmd.WaveID),                 // $8 wave_id
		cmd.RecipientISP,                     // $9 recipient_isp
		cmd.SelectionRank,                    // $10 selection_rank
		cmd.AudienceSourceType,               // $11 audience_source_type
		nullUUIDString(cmd.AudienceSourceID), // $12 audience_source_id
		nullString(cmd.PlainContent),         // $13 plain_content
		cmd.IdempotencyKey,                   // $14 idempotency_key
		nullUUID(cmd.ContentSnapshotID),      // $15 content_snapshot_id
		priority,                             // $16 priority
		nullUUID(cmd.CreativeID),             // $17 creative_id (A/B variant; Nil -> NULL)
	}
}

// InsertQueueRow runs the ONE canonical INSERT for a single SendCommand. Both
// the dispatcher (direct path) and the QueueWriterConsumer call it, so the row
// is identical no matter which path produced it. ON CONFLICT DO NOTHING makes it
// idempotent — a redelivery is a no-op. Returns (inserted, err): inserted is
// true only when a NEW row landed (RowsAffected==1), false on a conflict.
func InsertQueueRow(ctx context.Context, db queueExecer, cmd SendCommand) (inserted bool, err error) {
	if cmd.IdempotencyKey == uuid.Nil {
		// Un-dedupable: refuse rather than write a row that could double-send.
		return false, fmt.Errorf("sendqueue: InsertQueueRow with zero idempotency_key")
	}
	res, err := db.ExecContext(ctx, queueInsertSQL, QueueInsertArgs(cmd)...)
	if err != nil {
		return false, fmt.Errorf("sendqueue: queue INSERT key=%s: %w", cmd.IdempotencyKey, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// =============================================================================
// QueueWriterConsumer — consumes send.commands.v1 and runs the SAME INSERT into
// mailing_campaign_queue that EnqueuePMTAWave runs directly. At-least-once +
// ON CONFLICT (idempotency_key) DO NOTHING = idempotent: a redelivered command
// never produces a duplicate queue row. The offset is committed ONLY after the
// INSERT (the framework commits when Handle returns nil), so nothing is dropped.
//
// It deliberately does NOT send — the existing SendWorkerPool claims the row and
// processItem sends it, UNCHANGED. This is the "reuse the proven send path"
// half: Kafka is in the critical path only as far as WRITING the queue row.
// =============================================================================
type QueueWriterConsumer struct {
	db  queueExecer
	cfg eventbus.Config

	// ledger, when non-nil, records each accepted command in the sendqueue
	// ledger for observability/redrive. It is OPTIONAL: the authoritative dedup
	// is the mailing_campaign_queue unique index, not the ledger. nil = skip.
	ledger ledgerDB

	consumer *eventbus.Consumer

	// metrics (atomic via the wiring's status reader); plain ints guarded by the
	// single-goroutine consumer loop are sufficient here.
	inserted  uint64
	conflicts uint64
	failed    uint64
	statHook  func(inserted, conflicts, failed uint64)
}

// NewQueueWriterConsumer constructs the consumer. db is the mailing primary
// (*sql.DB satisfies queueExecer). cfg supplies brokers/group/topics for Start;
// it can be the zero Config for handler unit tests that call Handle directly.
func NewQueueWriterConsumer(db queueExecer, cfg eventbus.Config) *QueueWriterConsumer {
	return &QueueWriterConsumer{db: db, cfg: cfg}
}

// WithStatHook installs a callback invoked after every handled record with the
// running (inserted, conflicts, failed) counts, so the /health surface can read
// live queue-write metrics. Optional.
func (c *QueueWriterConsumer) WithStatHook(fn func(inserted, conflicts, failed uint64)) *QueueWriterConsumer {
	c.statHook = fn
	return c
}

// Stats returns the running counters (inserted, conflicts, failed).
func (c *QueueWriterConsumer) Stats() (inserted, conflicts, failed uint64) {
	return c.inserted, c.conflicts, c.failed
}

// Handle is the eventbus.Handler: unmarshal the SendCommand and run the SAME
// queue INSERT EnqueuePMTAWave runs. A successful INSERT (new row OR conflict)
// returns nil so the framework commits the offset — a conflict is the idempotent
// no-op of a redelivery, which is success, not failure. A DB error returns the
// error so the framework retries and ultimately does NOT commit (no drop). A
// corrupt record cannot be deduped or written; it is routed to the DLQ via the
// framework (returning an error after MaxRetries parks it), never skip-committed.
// key is the idempotency-key bytes; identity is read from the payload, so key is
// unused here.
func (c *QueueWriterConsumer) Handle(ctx context.Context, _, value []byte) error {
	var cmd SendCommand
	if err := json.Unmarshal(value, &cmd); err != nil {
		return fmt.Errorf("sendqueue/queuewriter: unmarshal command: %w", err)
	}
	if cmd.IdempotencyKey == uuid.Nil {
		// Poison: un-dedupable. Route to DLQ rather than wedge the partition.
		return errors.New("sendqueue/queuewriter: command has zero idempotency_key")
	}

	inserted, err := InsertQueueRow(ctx, c.db, cmd)
	if err != nil {
		c.failed++
		c.fireHook()
		return err // retry; do NOT commit → no drop
	}
	if inserted {
		c.inserted++
	} else {
		c.conflicts++ // redelivery / duplicate — the idempotent no-op
	}

	// OPTIONAL ledger record (observability only; the queue unique index is the
	// authoritative dedup). A ledger error never blocks the offset commit.
	if c.ledger != nil {
		c.recordLedger(ctx, cmd)
	}
	c.fireHook()
	return nil // INSERT committed → commit offset
}

// recordLedger best-effort claims the key in mailing_send_ledger for
// observability/redrive parity. Errors are swallowed (logged by the caller of
// the consumer if desired) because the queue unique index is authoritative.
func (c *QueueWriterConsumer) recordLedger(ctx context.Context, cmd SendCommand) {
	cmdJSON, mErr := json.Marshal(cmd)
	if mErr != nil {
		return
	}
	// 'sent' is wrong here (we did not send); 'claimed' would invite the
	// reconciler to redrive a row that is already in the queue. Record as
	// 'requeued' so it is informational and the reconciler's grace guard leaves
	// it alone unless it genuinely strands. ON CONFLICT DO NOTHING keeps it idempotent.
	_, _ = c.ledger.ExecContext(ctx, `
INSERT INTO mailing_send_ledger
    (idempotency_key, campaign_id, subscriber_id, wave_id, status, worker_id, claimed_at, attempts, command)
VALUES ($1, $2, $3, $4, 'requeued', 'queue-writer', NOW(), 0, $5)
ON CONFLICT (idempotency_key) DO NOTHING`,
		cmd.IdempotencyKey, nullUUID(cmd.CampaignID), nullUUID(cmd.SubscriberID), nullUUID(cmd.WaveID), cmdJSON)
}

// WithLedger installs the optional ledger seam (see recordLedger).
func (c *QueueWriterConsumer) WithLedger(l ledgerDB) *QueueWriterConsumer {
	c.ledger = l
	return c
}

func (c *QueueWriterConsumer) fireHook() {
	if c.statHook != nil {
		c.statHook(c.inserted, c.conflicts, c.failed)
	}
}

// Start constructs the real group consumer and runs its loop in a goroutine. It
// is a no-op (returns nil) when the bus is disabled (no brokers) — the
// dark-by-default path. dlq parks poison records on send.commands.v1.dlq.
func (c *QueueWriterConsumer) Start(ctx context.Context, dlq eventbus.DLQ) error {
	if !c.cfg.Enabled() {
		return nil
	}
	cfg := c.cfg
	if len(cfg.Topics) == 0 {
		cfg.Topics = []string{TopicQueueWrites}
	}
	con, err := eventbus.NewConsumer(cfg, c.Handle, dlq, eventbus.ConsumerOptions{})
	if err != nil {
		return fmt.Errorf("sendqueue/queuewriter: consumer: %w", err)
	}
	c.consumer = con
	go func() { _ = con.Run(ctx) }()
	return nil
}

// Stop closes the consumer. Safe when never started.
func (c *QueueWriterConsumer) Stop() {
	if c.consumer != nil {
		_ = c.consumer.Close()
	}
}
