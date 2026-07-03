// Package sendqueue is the Kafka send-queue core: the LOAD-BEARING design that
// lets Kafka be the primary send queue WITHOUT double-sending.
//
// Read internal/eventbus/doc (config.go) first — this package builds on the
// dark-by-default eventbus seams (Producer, Consumer, Config, FlagGate,
// EnsureTopics) and inherits their posture: it compiles and unit-tests on its
// own, has no import-time side effects, and is NOT wired into cmd/ or the real
// send path. It ships SHADOW-capable: the default SendSink (NoOpSink) sends NO
// real email.
//
// THE TWO INVARIANTS (the whole point — do not violate):
//
//   - No double-send: at most one SEND per idempotency_key reaches PMTA, even
//     under redelivery, rebalance, concurrent consumers, or crash. The
//     serialization point is the mailing_send_ledger PRIMARY KEY (idempotency_key),
//     CLAIMED via INSERT ... ON CONFLICT DO NOTHING BEFORE the send. The conflict
//     is the cross-worker mutual-exclusion primitive.
//
//   - No drop: every idempotency_key eventually reaches status='sent'. The Kafka
//     offset is committed ONLY after the send is recorded (or a definitive skip).
//     At-least-once delivery + an idempotent send = effectively-once. A crash in
//     the claim->send->record window leaves a 'claimed' row that the
//     LedgerReconciler (mirroring worker/outbox_reconciler.go) resolves.
//
// The idempotency key is the same deterministic UUIDv5 the outbox path uses
// (worker.OutboxIdempotencyKey over campaign+subscriber+wave) so the ledger and
// the existing mailing_campaign_queue agree on key identity during cutover.
package sendqueue

import (
	"github.com/google/uuid"
)

// TopicSendCommands is the Kafka topic carrying send commands. v1 in the name so
// a future incompatible schema can run a parallel topic during migration. Kept
// as an exported constant so wiring code, the consumer, and tests agree on
// spelling (mirrors eventbus.TopicLake/TopicIngest).
const TopicSendCommands = "send.commands.v1"

// SendCommand is one unit of work on TopicSendCommands: "send this campaign's
// content to this recipient, exactly once". It is intentionally SMALL — it
// carries identity + routing + the minimal references a send needs, NOT a
// fully-rendered body. The real send path re-hydrates content from
// ContentSnapshotID (the set-based enqueue snapshot the send worker already
// uses, see worker/send_worker.go hydrateSnapshots), exactly as the live queue
// does; a large inline body on every Kafka record would balloon the topic and
// duplicate the snapshot cache for no benefit.
//
// JSON-serializable: produced as the Kafka record value, KEYED by IdempotencyKey
// bytes (see EnqueueSendCommands) so all duplicates of a key land on the same
// partition (per-key ordering) and a single consumer instance owns them.
type SendCommand struct {
	// IdempotencyKey is the dedup identity. The deterministic UUIDv5 from
	// worker.OutboxIdempotencyKey(campaign, subscriber, wave). It is the ledger
	// PRIMARY KEY, the Kafka record key, and (via X-Idempotency header) the PMTA
	// backstop. Required.
	IdempotencyKey uuid.UUID `json:"idempotency_key"`

	// Identity of the work, denormalized onto the ledger row for observability
	// and for the reconciler. CampaignID/SubscriberID/WaveID are the same tuple
	// that produced IdempotencyKey.
	CampaignID   uuid.UUID `json:"campaign_id"`
	SubscriberID uuid.UUID `json:"subscriber_id"`
	WaveID       uuid.UUID `json:"wave_id"`

	// Routing for the send: the recipient address and the resolved ISP group
	// (gmail/yahoo/microsoft/…), used by the real sink to pick a VMTA pool /
	// transport. The shadow sinks ignore them.
	Recipient string `json:"recipient"`
	ISP       string `json:"isp"`

	// ContentSnapshotID references the wave's shared base creative in the
	// snapshot store (the set-based enqueue path). The real sink re-hydrates the
	// body from it and applies the per-recipient fingerprint mutation, exactly
	// like the live send worker. uuid.Nil means "no snapshot" (legacy inline
	// path) — the shadow sinks don't care either way.
	ContentSnapshotID uuid.UUID `json:"content_snapshot_id,omitempty"`
	// CreativeID carries the wave-path A/B variant id (mailing_ab_variants.id)
	// chosen at enqueue (ab_split.go). uuid.Nil = no A/B test on the campaign.
	// Additive field: pre-A/B messages unmarshal to Nil -> SQL NULL.
	CreativeID uuid.UUID `json:"creative_id,omitempty"`

	// Subject is the small, ready-to-send subject line (cheap to carry inline;
	// the body is the only thing worth deferring to the snapshot).
	Subject string `json:"subject,omitempty"`

	// ─────────────────────────────────────────────────────────────────────────
	// QUEUE-WRITER FIELDS (SK-4): the full mailing_campaign_queue row.
	//
	// When a wave is routed to Kafka as the PRIMARY transport, EnqueuePMTAWave
	// PRODUCES one SendCommand carrying EVERY column it would otherwise INSERT
	// directly into mailing_campaign_queue, and the QueueWriterConsumer performs
	// the EXACT same INSERT ... ON CONFLICT (idempotency_key) DO NOTHING. These
	// fields are that full row payload; with them the consumer reconstructs the
	// identical INSERT and the existing SendWorkerPool then claims + sends the
	// row UNCHANGED. The send path (worker/processItem) is never touched.
	//
	// The set-based enqueue path leaves HTMLContent/PlainContent empty and sets
	// ContentSnapshotID (the body lives once in the snapshot, hydrated at send
	// time); the legacy row-at-a-time path carries the per-recipient HTML/plain
	// inline and leaves ContentSnapshotID Nil. The QueueWriterConsumer mirrors
	// both exactly — same NULL/value choices as the direct INSERT.
	//
	// QueueID is the random per-row id (mailing_campaign_queue.id). It is carried
	// so a re-produced/redelivered command yields the SAME row id; the dedup is
	// authoritative on idempotency_key (ON CONFLICT), so QueueID only matters for
	// stable identity, not correctness.
	QueueID            uuid.UUID `json:"queue_id,omitempty"`
	ISPPlanID          uuid.UUID `json:"isp_plan_id,omitempty"`
	RecipientISP       string    `json:"recipient_isp,omitempty"`
	SelectionRank      int       `json:"selection_rank,omitempty"`
	AudienceSourceType string    `json:"audience_source_type,omitempty"`
	// AudienceSourceID is a UUID string or "" (the queue column is nullable and
	// the dispatcher already normalizes a non-UUID to SQL NULL).
	AudienceSourceID string `json:"audience_source_id,omitempty"`
	HTMLContent      string `json:"html_content,omitempty"`
	PlainContent     string `json:"plain_content,omitempty"`
	// ScheduledAtUnix is the queue row's scheduled_at as a Unix epoch (seconds);
	// 0 means "unset" (the consumer then uses NOW(), but EnqueuePMTAWave always
	// populates it). Carried as an int so the JSON is timezone-free and stable.
	ScheduledAtUnix int64 `json:"scheduled_at_unix,omitempty"`
	// Priority is the queue row priority (the direct INSERT uses a literal 5).
	Priority int `json:"priority,omitempty"`
}
