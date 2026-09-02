package worker

// kafka_send_route.go is the SK-4 PRODUCER FORK: the single, narrow seam that
// lets a wave's recipients be ROUTED through Kafka (send.commands.v1) as the
// PRIMARY transport instead of being INSERTed directly into
// mailing_campaign_queue — WITHOUT changing the send path.
//
// DARK BY DEFAULT. kafkaRouteWave() returns false unless the operator opts a
// specific wave/campaign in via an allowlist env (or the global KAFKA_SEND_QUEUE_ALL
// switch). With every routing env unset/empty, EnqueuePMTAWave routes NOTHING:
// the direct INSERT path runs UNCHANGED and production behavior is byte-identical
// to today. The guard is the FIRST thing checked in maybeRouteWaveToKafka.
//
// NEVER DROP A RECIPIENT: routing is best-effort at the SAFE side. If no producer
// is wired, or producing the Kafka command FAILS, the caller falls back to the
// direct INSERT for that recipient — the row is always written somewhere.

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/eventbus"
	"github.com/ignite/sparkpost-monitor/internal/eventbus/sendqueue"
)

// sourceIDString renders the legacy path's audience_source_id interface{} (which
// holds a parsed uuid.UUID or nil) into the string the SendCommand carries; the
// QueueWriterConsumer re-parses it back to NULL/uuid via nullUUIDString, so the
// round trip preserves the dispatcher's normalize-or-NULL semantics. nil → "".
func sourceIDString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case uuid.UUID:
		return t.String()
	case string:
		return t
	default:
		return ""
	}
}

// nullStringToSource renders the set-based path's normalized sql.NullString
// audience_source_id into the SendCommand string ("" when NULL).
func nullStringToSource(ns sql.NullString) string {
	if !ns.Valid {
		return ""
	}
	return ns.String
}

// kafkaSendProducer is the package-level producer the wiring installs (cmd/server
// eventbus_wiring.go) when the send-queue is enabled. It stays nil in the
// dark-default build and in tests, so maybeRouteWaveToKafka short-circuits to the
// direct INSERT. Guarded by a RWMutex because the scheduler goroutines read it
// concurrently while boot wiring sets it once.
var (
	kafkaSendProducerMu sync.RWMutex
	kafkaSendProducer   eventbus.Producer

	// kafkaRouteWaveCounter counts waves actually routed to Kafka (observability
	// for /health). Plain access is fine — it is only ever incremented under the
	// surrounding wave transaction's single goroutine and read for status.
	kafkaRoutedWaves uint64
)

// SetKafkaSendProducer installs (or clears, with nil) the producer used by the
// SK-4 routing fork. Called once from cmd/server wiring; default is nil (dark).
func SetKafkaSendProducer(p eventbus.Producer) {
	kafkaSendProducerMu.Lock()
	kafkaSendProducer = p
	kafkaSendProducerMu.Unlock()
}

// getKafkaSendProducer returns the installed producer (nil when dark).
func getKafkaSendProducer() eventbus.Producer {
	kafkaSendProducerMu.RLock()
	defer kafkaSendProducerMu.RUnlock()
	return kafkaSendProducer
}

// KafkaRoutedWaves returns the count of waves routed to Kafka (for /health).
func KafkaRoutedWaves() uint64 { return kafkaRoutedWaves }

// kafkaRouteWave is the wave dispatcher's routing decision. It delegates the
// WHOLE policy question to the canonical routing predicate
// (sendqueue.SendRouteMatches — KAFKA_SEND_QUEUE_ALL / _WAVES / _CAMPAIGNS, plus
// the Redis kafka:flag:send_route kill switch) and adds exactly one condition of
// its own: a producer must be wired. A routing flag set without a producer
// (mis-config) safely returns false so nothing is ever lost.
//
// It deliberately does NOT consult KAFKA_SEND_QUEUE_ENABLED — that is the
// WIRING gate (KafkaSendQueueEnabled), and routing on ENABLED alone is the
// 2026-09-01 half-off state REQ-090 removes. With every routing env unset this
// returns false: the byte-identical dark default.
func kafkaRouteWave(waveID, campaignID string) bool {
	if getKafkaSendProducer() == nil {
		return false // no transport wired → never route (dark)
	}
	return sendqueue.SendRouteMatches(waveID, campaignID)
}

// KafkaSendQueueEnabled reports whether the send-queue transport should be WIRED
// (producer installed, QueueWriterConsumer + LedgerReconciler started):
// KAFKA_SEND_QUEUE_ENABLED truthy, OR any routing env set. cmd/server uses it in
// wireSendQueue. It is a pure env read and is NOT a routing decision — ENABLED=1
// with ALL=0 wires a consumer that drains a backlog while nothing new routes.
func KafkaSendQueueEnabled() bool {
	// Delegate to the canonical wiring gate so "should this process wire the
	// transport?" has exactly one implementation.
	return sendqueue.SendRouteEnabled()
}

// produceQueueCommand builds a SendCommand carrying the FULL queue row and
// PRODUCES it to send.commands.v1, keyed by idempotency_key. It returns nil on
// success; any error means the caller MUST fall back to the direct INSERT for
// this recipient (no drop). prod is the installed producer (already non-nil when
// kafkaRouteWave returned true, but re-checked for safety).
func produceQueueCommand(ctx context.Context, cmd sendqueue.SendCommand) error {
	prod := getKafkaSendProducer()
	if prod == nil {
		return errNoKafkaProducer
	}
	return sendqueue.EnqueueSendCommands(ctx, prod, []sendqueue.SendCommand{cmd})
}

// errNoKafkaProducer signals "no transport" so the caller falls back.
var errNoKafkaProducer = &routeErr{"sendqueue route: no producer wired"}

type routeErr struct{ s string }

func (e *routeErr) Error() string { return e.s }

// buildSendCommand assembles the full-row SendCommand for one recipient. The
// fields map 1:1 onto sendqueue.queueInsertSQL via QueueInsertArgs, so the
// QueueWriterConsumer reconstructs the IDENTICAL INSERT. htmlContent/plainContent
// are passed through as-is: the set-based path passes "" (body lives in the
// snapshot, referenced by snapshotID); the legacy path passes the per-recipient
// inline HTML and leaves snapshotID Nil.
func buildSendCommand(
	queueID, campaignID, subscriberID, waveUUID, ispPlanID uuid.UUID,
	idempotencyKey uuid.UUID,
	subject, htmlContent, plainContent, recipientISP, audienceSourceType, audienceSourceID string,
	selectionRank int,
	scheduledAt time.Time,
	contentSnapshotID uuid.UUID,
	creativeID uuid.UUID,
) sendqueue.SendCommand {
	return sendqueue.SendCommand{
		IdempotencyKey:     idempotencyKey,
		QueueID:            queueID,
		CampaignID:         campaignID,
		SubscriberID:       subscriberID,
		WaveID:             waveUUID,
		ISPPlanID:          ispPlanID,
		Recipient:          "", // address not needed: the send path re-reads it from the queue/subscriber row
		ISP:                recipientISP,
		RecipientISP:       recipientISP,
		Subject:            subject,
		HTMLContent:        htmlContent,
		PlainContent:       plainContent,
		SelectionRank:      selectionRank,
		AudienceSourceType: audienceSourceType,
		AudienceSourceID:   audienceSourceID,
		ContentSnapshotID:  contentSnapshotID,
		CreativeID:         creativeID,
		ScheduledAtUnix:    scheduledAt.Unix(),
		Priority:           5,
	}
}

// envTruthy is the local truthy parser (1/true/yes/on, case-insensitive).
func envTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// (allowlist matching moved to sendqueue.listContains — the routing predicate
// owns it now, so there is exactly one parser for KAFKA_SEND_QUEUE_WAVES /
// _CAMPAIGNS.)
