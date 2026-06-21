// Package consumers holds the three Kafka SHADOW consumers for the Project
// Jarvis event backbone (see docs/KAFKA_EVENT_BACKBONE_PLAN.md). They are the
// Wave-2 "dark passenger" projectors that read the producer-tapped topics and
// write PARITY-ONLY shadow projections (tables created by migration 049, a
// shadow Firehose stream). NONE of them touch the live ingestion, suppression,
// or lake path by default.
//
// SHIPS DARK. Like internal/eventbus and internal/analytics.lake_emitter:
//
//   - Constructed only when explicitly enabled. The integration step (SA-6)
//     builds the eventbus.Config/Producer, then constructs whichever of these
//     consumers it wants and calls Start(ctx). Until then the code compiles and
//     unit-tests on its own but the app does nothing with it.
//   - Default OFF. A consumer is only useful once SA-6 wires it; nothing in
//     cmd/ references this package yet.
//   - At-least-once via the eventbus.Consumer framework (manual commit after
//     handle, per-record panic recovery, bounded retry, DLQ on permanent
//     failure). Idempotency lives at the SINK (ON CONFLICT / lake event_uid
//     dedup), exactly as the framework contract requires.
//
// Each consumer exposes:
//   - a constructor that takes its deps explicitly (a *sql.DB or a small write
//     interface, plus config) so the HANDLER is unit-testable with sqlmock / a
//     fake, with NO broker;
//   - a Handler(ctx, key, value) error suitable for eventbus.Consumer;
//   - Start(ctx) / Stop() that construct the real group consumer from an
//     eventbus.Config and a DLQ that parks dead records on "<topic>.dlq".
package consumers

import (
	"context"

	"github.com/ignite/sparkpost-monitor/internal/eventbus"
)

// dlqSuffix is appended to a source topic to form its dead-letter topic, e.g.
// "evt.ingest.v1" -> "evt.ingest.v1.dlq". The plan pairs every consumer with a
// DLQ so a poison record is parked durably, never lost and never blocking.
const dlqSuffix = ".dlq"

// DLQTopic returns the dead-letter topic name for a source topic.
func DLQTopic(topic string) string { return topic + dlqSuffix }

// producerDLQ adapts an eventbus.Producer into an eventbus.DLQ that re-produces
// the dead record onto "<topic>.dlq". A nil producer yields a DLQ that fails
// (so the framework refuses to commit and the record is redelivered rather than
// silently dropped) — but the integration step is expected to always supply a
// producer when it constructs a consumer.
type producerDLQ struct {
	prod eventbus.Producer
}

// NewProducerDLQ builds a DLQ backed by a Kafka producer. The returned DLQ
// writes each dead record to DLQTopic(sourceTopic).
func NewProducerDLQ(prod eventbus.Producer) eventbus.DLQ {
	return &producerDLQ{prod: prod}
}

// Dead re-produces the record onto the source topic's DLQ. Returning a non-nil
// error makes the consumer framework retain (not commit) the record.
func (d *producerDLQ) Dead(ctx context.Context, topic string, key, value []byte, cause error) error {
	if d.prod == nil {
		return errNoDLQProducer
	}
	return d.prod.Produce(ctx, DLQTopic(topic), key, value)
}

// errNoDLQProducer is returned when a DLQ has no producer, so the framework
// retains the record instead of losing it.
var errNoDLQProducer = &dlqError{"eventbus/consumers: DLQ has no producer (record retained)"}

type dlqError struct{ s string }

func (e *dlqError) Error() string { return e.s }
