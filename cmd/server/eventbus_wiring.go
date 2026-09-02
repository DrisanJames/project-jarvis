package main

// eventbus_wiring.go is the SA-6 integration seam for the Kafka event backbone.
//
// DARK BY DEFAULT. Nothing in here does anything unless BOTH are true:
//   - KAFKA_BROKERS is set (eventbus.Config.Enabled() == true), AND
//   - the relevant per-flow flag is ON (KAFKA_FLAG_* env or kafka:flag:* Redis).
//
// When KAFKA_BROKERS is unset (today's production), wireEventBus() logs a single
// "disabled" line and returns a zero handle: no Producer is constructed, no Tap
// is started, WireTaps is NEVER called (so the package-level Publish* pointers
// stay nil and every hot-path Publish* is a first-line nil return), and no
// consumer goroutine is launched. Behavior is byte-identical to before.
//
// This file is the ONLY place in cmd/server that references internal/eventbus or
// its consumers, so the blast radius of the integration is contained here.

import (
	"context"
	"database/sql"
	"log"
	"os"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/ignite/sparkpost-monitor/internal/api"
	"github.com/ignite/sparkpost-monitor/internal/eventbus"
	"github.com/ignite/sparkpost-monitor/internal/eventbus/consumers"
	"github.com/ignite/sparkpost-monitor/internal/eventbus/sendqueue"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

// eventBusHandle owns the lifecycle of everything the event backbone wires:
// the producer, the three producer-side taps, the per-flow flag gates, and the
// three shadow consumers. A zero handle (the dark-default return) is fully
// inert — Stop() is a safe no-op on it.
type eventBusHandle struct {
	enabled bool

	prod eventbus.Producer

	lakeTap     *eventbus.Tap
	ingestTap   *eventbus.Tap
	suppressTap *eventbus.Tap

	lakeGate     *eventbus.FlagGate
	ingestGate   *eventbus.FlagGate
	suppressGate *eventbus.FlagGate

	ingestConsumer *consumers.IngestShadowConsumer
	suppProjector  *consumers.SuppressionProjector
	lakeSink       *consumers.LakeShadowSink

	// running flags reflect whether each consumer's Start actually launched a
	// group-consumer goroutine (true only when the bus is enabled and, for the
	// lake sink, the shadow Firehose stream is set).
	ingestRunning bool
	suppRunning   bool
	lakeRunning   bool

	suppMode string

	// ── SK-4 Kafka-primary send-queue (DARK BY DEFAULT) ──
	// Only constructed when the bus is enabled AND worker.KafkaSendQueueEnabled()
	// (a routing allowlist / KAFKA_SEND_QUEUE_ENABLED is set). Default: nil/false,
	// so EnqueuePMTAWave routes nothing → byte-identical to today.
	sendQueueProd    eventbus.Producer
	queueWriter      *sendqueue.QueueWriterConsumer
	ledgerReconciler *sendqueue.LedgerReconciler
	reconcilerCancel context.CancelFunc
	sendQueueEnabled bool
	// queueWriterStarted records only that Start() returned nil at boot. It is
	// NOT liveness and must never be published as consumer_running again — that
	// boot boolean is exactly what reported healthy through the 2026-09-01 SK-4
	// wedge. Liveness comes from queueWriter.Snapshot().Running.
	queueWriterStarted bool
	reconcilerRunning  bool
}

// wireEventBus performs the gated boot wiring. ctx is the server lifecycle
// context (cancelled on shutdown) used to drive the consumer loops; db is the
// mailing primary the shadow consumers project into; rdb is the existing redis
// client (may be nil — the FlagGates then run env-only). It never blocks the
// boot path and never returns an error: any construction failure is logged and
// degrades to the dark/legacy path.
func wireEventBus(ctx context.Context, db *sql.DB, rdb *redis.Client) *eventBusHandle {
	h := &eventBusHandle{}

	cfg := eventbus.LoadConfig()
	if !cfg.Enabled() {
		log.Println("[eventbus] disabled (KAFKA_BROKERS unset) — producer taps NOT wired, no consumers started; behavior unchanged")
		setEventBusStatus(h) // publish the all-off snapshot for /health
		return h
	}
	h.enabled = true

	// ── Producer + per-flow taps (gated individually by their FlagGates) ──
	prod, err := eventbus.NewKgoProducer(cfg)
	if err != nil {
		// Could not build the producer (bad SASL mech, client error). Stay dark:
		// do NOT call WireTaps, do NOT start consumers — fall back to legacy.
		log.Printf("[eventbus] producer construction failed (%v) — staying dark, taps NOT wired", err)
		h.enabled = false
		setEventBusStatus(h)
		return h
	}
	h.prod = prod

	// Ensure the topics exist. MSK Serverless does not reliably honor client-side
	// producer auto-create, so when KAFKA_ALLOW_AUTO_TOPICS is set we create them
	// explicitly from here (the only in-VPC component that can reach the private
	// brokers). Delete-policy only — Serverless does not support compaction, and
	// the suppression flow (the compacted topic) is OFF in this configuration.
	if cfg.AllowAutoTopics {
		specs := []eventbus.TopicSpec{
			{Name: eventbus.TopicLake, Partitions: 12},
			{Name: eventbus.TopicIngest, Partitions: 12},
			{Name: eventbus.TopicLake + ".dlq", Partitions: 3},
			{Name: eventbus.TopicIngest + ".dlq", Partitions: 3},
		}
		if err := eventbus.EnsureTopics(ctx, cfg, specs); err != nil {
			log.Printf("[eventbus] EnsureTopics: %v (producer may fail until topics exist)", err)
		} else {
			log.Println("[eventbus] topics ensured (evt.lake.v1, evt.ingest.v1, + .dlq)")
		}
	}

	// One Tap per flow over the SAME producer. Each is still individually
	// flag-gated inside publish.go, so a wired-but-flag-off flow is a no-op.
	h.lakeTap = eventbus.NewTap(prod)
	h.ingestTap = eventbus.NewTap(prod)
	h.suppressTap = eventbus.NewTap(prod)

	// interval 0 => FlagGate uses DefaultFlagPollInterval (cfg.FlagPollInterval
	// is honored by the consumers' own Config; the gates poll at the default).
	h.lakeGate = eventbus.NewFlagGate("produce_lake", rdb, cfg.FlagPollInterval)
	h.ingestGate = eventbus.NewFlagGate("produce_ingest", rdb, cfg.FlagPollInterval)
	h.suppressGate = eventbus.NewFlagGate("produce_suppress", rdb, cfg.FlagPollInterval)

	// Install the package-level taps/gates so the hot-path Publish* calls become
	// live (each still gated by its own FlagGate, default OFF).
	eventbus.WireTaps(
		h.lakeTap, h.ingestTap, h.suppressTap,
		h.lakeGate, h.ingestGate, h.suppressGate,
	)

	// Start the FlagGate background pollers so a kill-switch can flip without a
	// redeploy.
	h.lakeGate.Start()
	h.ingestGate.Start()
	h.suppressGate.Start()

	log.Printf("[eventbus] producer taps wired (lake/ingest/suppress) — each flag-gated, defaults OFF (lake=%v ingest=%v suppress=%v)",
		h.lakeGate.Enabled(), h.ingestGate.Enabled(), h.suppressGate.Enabled())

	// ── Consumers (shadow / dark passengers) ──
	// Each consumer's Start is a no-op when the bus is disabled; here the bus is
	// enabled, so they join their groups and project parity-only writes.

	// 1) Ingest shadow consumer → ingest_events_shadow.
	{
		c := consumers.NewIngestShadowConsumer(db, eventbus.Config{
			Brokers:          cfg.Brokers,
			ClientID:         cfg.ClientID,
			SASLMechanism:    cfg.SASLMechanism,
			TLS:              cfg.TLS,
			Group:            "ignite-ingest-shadow",
			Topics:           []string{eventbus.TopicIngest},
			FlagPollInterval: cfg.FlagPollInterval,
		})
		if err := c.Start(ctx); err != nil {
			log.Printf("[eventbus] ingest-shadow consumer start failed: %v", err)
		} else {
			h.ingestConsumer = c
			h.ingestRunning = true
			log.Println("[eventbus] ingest-shadow consumer started (group=ignite-ingest-shadow, topic=evt.ingest.v1)")
		}
	}

	// 2) Suppression projector in SHADOW mode (default): writes ONLY
	//    suppression_shadow, NEVER the live hub. UNIQUE consumer group per
	//    instance so it BROADCASTS (fans out) — every task reads all partitions.
	{
		instanceID := eventBusInstanceID()
		groupID := consumers.SuppressionGroupID("supp-projector", instanceID)
		p := consumers.NewSuppressionProjector(
			db,
			eventbus.Config{
				Brokers:          cfg.Brokers,
				ClientID:         cfg.ClientID,
				SASLMechanism:    cfg.SASLMechanism,
				TLS:              cfg.TLS,
				Group:            groupID,
				Topics:           []string{eventbus.TopicSuppression},
				FlagPollInterval: cfg.FlagPollInterval,
			},
			"",  // "" => SuppressionModeShadow (DEFAULT — does NOT mutate the live hub)
			nil, // add func nil is OK in shadow mode
		)
		h.suppMode = string(consumers.SuppressionModeShadow)
		if err := p.Start(ctx); err != nil {
			log.Printf("[eventbus] suppression projector start failed: %v", err)
		} else {
			h.suppProjector = p
			h.suppRunning = true
			log.Printf("[eventbus] suppression projector started in SHADOW mode (group=%s — unique per instance for broadcast fan-out)", groupID)
		}
	}

	// 3) Lake shadow sink: no-op unless ANALYTICS_FIREHOSE_STREAM_SHADOW is set.
	{
		sink := consumers.NewLakeShadowSinkFromEnv(eventbus.Config{
			Brokers:          cfg.Brokers,
			ClientID:         cfg.ClientID,
			SASLMechanism:    cfg.SASLMechanism,
			TLS:              cfg.TLS,
			Group:            "ignite-lake-shadow",
			Topics:           []string{eventbus.TopicLake},
			FlagPollInterval: cfg.FlagPollInterval,
		})
		h.lakeSink = sink
		if !sink.Enabled() {
			log.Println("[eventbus] lake shadow sink NOT started (ANALYTICS_FIREHOSE_STREAM_SHADOW unset — dark)")
		} else if err := sink.Start(ctx); err != nil {
			log.Printf("[eventbus] lake shadow sink start failed: %v", err)
		} else {
			h.lakeRunning = true
			log.Println("[eventbus] lake shadow sink started (group=ignite-lake-shadow, topic=evt.lake.v1)")
		}
	}

	// 4) SK-4 Kafka-PRIMARY send queue (DARK BY DEFAULT). Only wires when the
	//    operator has opted in via env (worker.KafkaSendQueueEnabled): a routing
	//    allowlist (KAFKA_SEND_QUEUE_WAVES / KAFKA_SEND_QUEUE_CAMPAIGNS), or
	//    KAFKA_SEND_QUEUE_ALL, or KAFKA_SEND_QUEUE_ENABLED. With all unset, this
	//    whole block is skipped: no topic ensured, no consumer/reconciler started,
	//    no producer installed → EnqueuePMTAWave routes NOTHING and the send path
	//    is byte-identical to today.
	wireSendQueue(ctx, h, db, cfg)

	setEventBusStatus(h)
	return h
}

// wireSendQueue is the SK-4 dark-gated wiring. Preconditions: the bus is enabled
// (KAFKA_BROKERS set, producer built). It is a no-op unless the operator opted in
// via env. On opt-in it: (a) ensures send.commands.v1 (+ .dlq); (b) installs the
// durable producer into the wave dispatcher (worker.SetKafkaSendProducer) so a
// routed wave can PRODUCE; (c) starts the QueueWriterConsumer (INSERTs the same
// queue row) and the LedgerReconciler. Any construction failure leaves the
// producer UNINSTALLED so kafkaRouteWave stays false (the dispatcher falls back
// to the direct INSERT) — no recipient is ever lost.
func wireSendQueue(ctx context.Context, h *eventBusHandle, db *sql.DB, cfg eventbus.Config) {
	if !worker.KafkaSendQueueEnabled() {
		log.Println("[eventbus] send-queue (SK-4) DARK — KAFKA_SEND_QUEUE_* unset; EnqueuePMTAWave routes nothing, send path unchanged")
		return
	}
	h.sendQueueEnabled = true

	// Ensure the send-commands topic + DLQ exist (idempotent). The send-queue
	// reuses the same producer-side topic the ledger path defines.
	if cfg.AllowAutoTopics {
		if err := eventbus.EnsureTopics(ctx, cfg, []eventbus.TopicSpec{
			{Name: sendqueue.TopicQueueWrites, Partitions: 12},
			{Name: sendqueue.TopicQueueWrites + ".dlq", Partitions: 3},
		}); err != nil {
			log.Printf("[eventbus] send-queue EnsureTopics: %v (producer may fail until topics exist)", err)
		} else {
			log.Printf("[eventbus] send-queue topics ensured (%s + .dlq)", sendqueue.TopicQueueWrites)
		}
	}

	// Durable producer for the wave-dispatcher fork. We reuse the bus producer
	// (h.prod): it is the idempotent franz-go client, exactly what
	// EnqueueSendCommands wants (synchronous, error-returning, not the lossy Tap).
	h.sendQueueProd = h.prod
	worker.SetKafkaSendProducer(h.sendQueueProd)
	log.Println("[eventbus] send-queue (SK-4) producer INSTALLED into wave dispatcher — routed waves will PRODUCE to send.commands.v1")

	// QueueWriterConsumer: INSERTs the SAME mailing_campaign_queue row the
	// dispatcher would. A stat hook feeds /health.
	qw := sendqueue.NewQueueWriterConsumer(db, eventbus.Config{
		Brokers:          cfg.Brokers,
		ClientID:         cfg.ClientID,
		SASLMechanism:    cfg.SASLMechanism,
		TLS:              cfg.TLS,
		Group:            "ignite-send-queue-writer",
		Topics:           []string{sendqueue.TopicQueueWrites},
		FlagPollInterval: cfg.FlagPollInterval,
	}).WithStatHook(func(inserted, conflicts, failed uint64) {
		setSendQueueCounters(inserted, conflicts, failed)
	}).WithDLQHook(func(dlqRecords uint64) {
		sendQueueDLQRecords.Store(dlqRecords)
	})
	dlq := consumers.NewProducerDLQ(h.prod)
	if err := qw.Start(ctx, dlq); err != nil {
		log.Printf("[eventbus] send-queue QueueWriterConsumer start failed: %v", err)
	} else {
		h.queueWriter = qw
		h.queueWriterStarted = true
		// Register the LIVE liveness snapshot. This is what makes
		// eventbus.SendQueueHealth() real for BOTH /health (internal/api, via
		// setEventBusStatus below) and the send-path monitors (internal/worker
		// OutboxSelfCheck, REQ-087). Without it both read the never-ran zero.
		eventbus.SetSendQueueHealthProvider(qw.Snapshot)
		log.Printf("[eventbus] send-queue QueueWriterConsumer started (group=ignite-send-queue-writer, topic=send.commands.v1, task_id=%s) — INSERTs into mailing_campaign_queue; send path UNCHANGED",
			eventbus.TaskID())
	}

	// LedgerReconciler: re-drives any stranded ledger rows by re-producing the
	// stored command. Runs in its own cancellable goroutine.
	rctx, cancel := context.WithCancel(ctx)
	h.reconcilerCancel = cancel
	rec := sendqueue.NewLedgerReconcilerWithProducer(db, h.sendQueueProd, 0, 0)
	h.ledgerReconciler = rec
	h.reconcilerRunning = true
	go rec.Start(rctx)
	log.Println("[eventbus] send-queue LedgerReconciler started (re-drives stranded commands)")
}

// Stop gracefully tears down everything the handle owns: it stops the consumers,
// the flag-gate pollers, and the producer taps (which flush + close the shared
// producer). Safe to call on a zero/dark handle (every field nil → no-ops).
func (h *eventBusHandle) Stop() {
	if h == nil {
		return
	}
	// SK-4 send-queue first: stop the reconciler goroutine + the queue writer
	// before the producer/taps close (they share h.prod).
	if h.reconcilerCancel != nil {
		h.reconcilerCancel()
	}
	if h.queueWriter != nil {
		h.queueWriter.Stop()
	}
	// Clear the dispatcher's producer hook so no further wave routes after Stop.
	worker.SetKafkaSendProducer(nil)
	// Consumers first (stop pulling), then producers/taps.
	if h.ingestConsumer != nil {
		h.ingestConsumer.Stop()
	}
	if h.suppProjector != nil {
		h.suppProjector.Stop()
	}
	if h.lakeSink != nil {
		h.lakeSink.Stop()
	}
	if h.lakeGate != nil {
		h.lakeGate.Stop()
	}
	if h.ingestGate != nil {
		h.ingestGate.Stop()
	}
	if h.suppressGate != nil {
		h.suppressGate.Stop()
	}
	// Each Tap.Close flushes its buffer; they share one producer, so closing the
	// last tap also closes the producer. Closing in sequence is safe.
	if h.lakeTap != nil {
		_ = h.lakeTap.Close()
	}
	if h.ingestTap != nil {
		_ = h.ingestTap.Close()
	}
	if h.suppressTap != nil {
		_ = h.suppressTap.Close()
	}
}

// eventBusInstanceID returns a per-PROCESS unique id so the broadcast
// suppression projector gets a unique consumer group on every ECS task (each
// must read ALL partitions, not work-split). A random uuid is the safest choice:
// even two tasks on the same host get distinct ids. ECS_TASK_ARN / HOSTNAME, if
// present, are folded in only for human-readable group names.
func eventBusInstanceID() string {
	base := uuid.New().String()
	if h := os.Getenv("HOSTNAME"); h != "" {
		return h + "-" + base
	}
	return base
}

// sendQueueInserted/Conflicts/Failed are the live queue-write counters the
// QueueWriterConsumer's stat hook updates; the /health snapshot reads them via
// atomic loads. They are package-level (not on the handle) because the hook
// closure outlives a single wireEventBus call and there is one bus per process.
var (
	sendQueueInserted   atomic.Uint64
	sendQueueConflicts  atomic.Uint64
	sendQueueFailed     atomic.Uint64
	sendQueueDLQRecords atomic.Uint64
)

// setSendQueueCounters stores the latest running counts (the hook passes
// monotonic totals, so a plain Store is correct).
func setSendQueueCounters(inserted, conflicts, failed uint64) {
	sendQueueInserted.Store(inserted)
	sendQueueConflicts.Store(conflicts)
	sendQueueFailed.Store(failed)
}

// setEventBusStatus registers a /health snapshot provider that reflects this
// handle. The provider closure reads LIVE producer counters (eventbus.
// ProducerStats — cheap atomic loads) on every call, while the wiring/consumer
// running flags come from the handle (fixed at boot). When the bus is dark
// (h.enabled == false) the snapshot reports disabled + brokers_set=false and all
// flows wired=false / flag_on=false, matching the never-wired Publish* state.
func setEventBusStatus(h *eventBusHandle) {
	api.SetEventBusStatusProvider(func() api.EventBusStatus {
		ps := eventbus.ProducerStats() // dark default: all zero, wired=false
		st := api.EventBusStatus{
			Enabled:    h.enabled,
			BrokersSet: h.enabled,
			Producer: api.EventBusProducerStatus{
				Produced: ps.Produced,
				Dropped:  ps.Dropped,
				Failed:   ps.Failed,
				Lake:     api.EventBusFlowStatus{Wired: ps.Lake.Wired, FlagOn: ps.Lake.FlagOn},
				Ingest:   api.EventBusFlowStatus{Wired: ps.Ingest.Wired, FlagOn: ps.Ingest.FlagOn},
				Suppress: api.EventBusFlowStatus{Wired: ps.Suppress.Wired, FlagOn: ps.Suppress.FlagOn},
			},
			Flags: api.EventBusFlags{
				Lake:     ps.Lake.FlagOn,
				Ingest:   ps.Ingest.FlagOn,
				Suppress: ps.Suppress.FlagOn,
			},
			Consumers: api.EventBusConsumerStatus{
				IngestRunning:      h.ingestRunning,
				SuppressionRunning: h.suppRunning,
				SuppressionMode:    h.suppMode,
				LakeRunning:        h.lakeRunning,
			},
			SendQueue: sendQueueStatus(h),
		}
		return st
	})
}

// sendQueueStatus renders the SK-4 send-queue block from the LIVE consumer
// snapshot (eventbus.SendQueueHealth) rather than boot booleans.
//
// Read this block as a whole, never one field:
//   - consumer_running=false        -> the loop has exited; this task consumes nothing.
//   - running=true, in_flight>0 and seconds_since_last_handled large -> WEDGED
//     inside the handler (the 2026-09-01 signature).
//   - lag_max is only meaningful when seconds_since_last_poll is small: it is
//     derived from the fetches this loop received (no kadm — see health.go), so
//     a consumer that stopped polling FREEZES its lag reading.
//   - every counter is PER TASK; task_id says which task answered.
func sendQueueStatus(h *eventBusHandle) api.EventBusSendQueueStatus {
	snap := eventbus.SendQueueHealth()
	st := api.EventBusSendQueueStatus{
		Enabled:              h.sendQueueEnabled,
		ConsumerStarted:      h.queueWriterStarted,
		ConsumerRunning:      snap.Running,
		ReconcilerRunning:    h.reconcilerRunning,
		RoutedWaves:          worker.KafkaRoutedWaves(),
		QueueWritesInsert:    sendQueueInserted.Load(),
		QueueWritesConflict:  sendQueueConflicts.Load(),
		QueueWritesFailed:    sendQueueFailed.Load(),
		QueueDLQRecords:      sendQueueDLQRecords.Load(),
		TaskID:               snap.TaskID,
		InFlight:             snap.InFlight,
		LagMax:               snap.LagMax,
		LagKnown:             snap.LagKnown,
		RetainedPartitions:   snap.RetainedPartitions,
		SecondsSinceLastPoll: snap.SecondsSinceLastPoll(),
		SecondsSinceHandled:  snap.SecondsSinceLastHandled(),
	}
	if !snap.LastPollAt.IsZero() {
		st.LastPollAt = snap.LastPollAt.UTC().Format(time.RFC3339)
	}
	if !snap.LastHandledAt.IsZero() {
		st.LastHandledAt = snap.LastHandledAt.UTC().Format(time.RFC3339)
	}
	return st
}
