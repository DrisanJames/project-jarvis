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
	"os"
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

// kafkaRouteWave is THE routing check (the dark-default guard). It returns true
// ONLY when the operator has explicitly opted this wave/campaign in:
//
//   - KAFKA_SEND_QUEUE_ALL=1 (or true): route every wave; OR
//   - KAFKA_SEND_QUEUE_WAVES contains waveID; OR
//   - KAFKA_SEND_QUEUE_CAMPAIGNS contains campaignID.
//
// Allowlists are comma/space-separated. With all three unset/empty it returns
// false — the byte-identical dark default. It ALSO requires a producer to be
// wired; a routing flag set without a producer (mis-config) safely returns false
// so nothing is ever lost.
func kafkaRouteWave(waveID, campaignID string) bool {
	if getKafkaSendProducer() == nil {
		return false // no transport wired → never route (dark)
	}
	if envTruthy(os.Getenv("KAFKA_SEND_QUEUE_ALL")) {
		return true
	}
	if listContains(os.Getenv("KAFKA_SEND_QUEUE_WAVES"), waveID) {
		return true
	}
	if listContains(os.Getenv("KAFKA_SEND_QUEUE_CAMPAIGNS"), campaignID) {
		return true
	}
	return false
}

// KafkaSendQueueEnabled reports whether ANY routing is configured (the wiring
// gate): KAFKA_SEND_QUEUE_ENABLED truthy, OR any allowlist/ALL env non-empty.
// cmd/server uses it to decide whether to EnsureTopics + start the consumer +
// install the producer. It does NOT consult the producer (the wiring sets the
// producer based on this), so it is a pure env read.
func KafkaSendQueueEnabled() bool {
	// Delegate to the canonical low-level gate (sendqueue.SendRouteEnabled) so the
	// wiring gate here and the bypass-blocking gate in api/mailing/repository can
	// never drift — same env names, same semantics, one implementation.
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

// listContains reports whether the comma/space-separated allowlist contains the
// target id (case-insensitive, trimmed). An empty list never matches.
func listContains(list, target string) bool {
	if strings.TrimSpace(list) == "" || target == "" {
		return false
	}
	target = strings.ToLower(strings.TrimSpace(target))
	for _, f := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		if strings.ToLower(strings.TrimSpace(f)) == target {
			return true
		}
	}
	return false
}
