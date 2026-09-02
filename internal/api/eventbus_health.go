package api

import "sync/atomic"

// eventbus_health.go exposes a CHEAP, read-only snapshot of the Kafka event
// backbone's runtime state for the /health surface. It is fully dark-safe: until
// the boot wiring (cmd/server) registers a status, the snapshot reports the bus
// as disabled with all-zero counters. No network I/O ever happens here.

// EventBusFlowStatus is the per-flow wiring + flag state.
type EventBusFlowStatus struct {
	Wired  bool `json:"wired"`
	FlagOn bool `json:"flag_on"`
}

// EventBusProducerStatus aggregates the producer-side tap counters and per-flow
// flag state.
type EventBusProducerStatus struct {
	Produced uint64             `json:"produced"`
	Dropped  uint64             `json:"dropped"`
	Failed   uint64             `json:"failed"`
	Lake     EventBusFlowStatus `json:"lake"`
	Ingest   EventBusFlowStatus `json:"ingest"`
	Suppress EventBusFlowStatus `json:"suppress"`
}

// EventBusConsumerStatus reports whether each shadow consumer is running (and
// the suppression projector's mode).
type EventBusConsumerStatus struct {
	IngestRunning      bool   `json:"ingest_running"`
	SuppressionRunning bool   `json:"suppression_running"`
	SuppressionMode    string `json:"suppression_mode,omitempty"`
	LakeRunning        bool   `json:"lake_running"`
}

// EventBusSendQueueStatus (SK-4) reports the Kafka-primary send-queue routing:
// whether the QueueWriterConsumer + LedgerReconciler are running, how many waves
// have been routed to Kafka, and the running queue-write counters. All zero/false
// when the send-queue is dark (the default).
// Liveness fields (2026-09-01): consumer_running was a BOOT boolean — set once
// after Start returned nil — and reported healthy for three hours while the
// consumer held its partitions and inserted nothing. It is now the live Run-loop
// flag, and it never travels alone: read it with last_handled_at / in_flight /
// lag_max, which are the fields that can express "alive but not progressing".
// Every counter here is PER ECS TASK (the ALB picks the task that answers
// /health), which is what task_id is for.
type EventBusSendQueueStatus struct {
	Enabled bool `json:"enabled"`
	// ConsumerStarted is the old boot boolean, kept only to distinguish
	// "never started" from "started and then died".
	ConsumerStarted     bool   `json:"consumer_started"`
	ConsumerRunning     bool   `json:"consumer_running"`
	ReconcilerRunning   bool   `json:"reconciler_running"`
	RoutedWaves         uint64 `json:"routed_waves"`
	QueueWritesInsert   uint64 `json:"queue_writes_inserted"`
	QueueWritesConflict uint64 `json:"queue_writes_conflicts"`
	// QueueWritesFailed counts failed INSERT ATTEMPTS (up to 4 per record).
	QueueWritesFailed uint64 `json:"queue_writes_failed"`
	// QueueDLQRecords counts RECORDS parked in the DLQ — i.e. recipients that
	// left the send path. Distinct from QueueWritesFailed on purpose.
	QueueDLQRecords uint64 `json:"queue_dlq_records"`

	TaskID string `json:"task_id"`
	// LastPollAt / LastHandledAt are RFC3339 UTC, empty when it never happened.
	LastPollAt    string `json:"last_poll_at,omitempty"`
	LastHandledAt string `json:"last_handled_at,omitempty"`
	// Seconds-since fields are -1 when the event never happened, so "never" is
	// distinguishable from "just now".
	SecondsSinceLastPoll float64 `json:"seconds_since_last_poll"`
	SecondsSinceHandled  float64 `json:"seconds_since_last_handled"`
	InFlight             int64   `json:"in_flight"`
	// LagMax is a fetch-derived lower bound (no kadm dependency) and is only
	// meaningful alongside a small seconds_since_last_poll.
	LagMax   int64 `json:"lag_max"`
	LagKnown bool  `json:"lag_known"`
	// RetainedPartitions > 0 means a record could be neither handled nor DLQ'd
	// and that partition's offset is deliberately frozen behind it.
	RetainedPartitions int `json:"retained_partitions"`
}

// EventBusStatus is the whole /health "event_bus" block.
type EventBusStatus struct {
	Enabled    bool                    `json:"enabled"`
	BrokersSet bool                    `json:"brokers_set"`
	Producer   EventBusProducerStatus  `json:"producer"`
	Flags      EventBusFlags           `json:"flags"`
	Consumers  EventBusConsumerStatus  `json:"consumers"`
	SendQueue  EventBusSendQueueStatus `json:"send_queue"`
}

// EventBusFlags surfaces the three per-flow producer flags as plain booleans.
type EventBusFlags struct {
	Lake     bool `json:"lake"`
	Ingest   bool `json:"ingest"`
	Suppress bool `json:"suppress"`
}

// eventBusStatusProvider is a func that returns the current snapshot. The boot
// wiring installs one via SetEventBusStatusProvider; reads go through an atomic
// pointer so /health never locks.
var eventBusStatusProvider atomic.Pointer[func() EventBusStatus]

// SetEventBusStatusProvider registers the snapshot source (called once from
// cmd/server after wireEventBus). Passing nil leaves the default disabled view.
func SetEventBusStatusProvider(fn func() EventBusStatus) {
	if fn == nil {
		eventBusStatusProvider.Store(nil)
		return
	}
	eventBusStatusProvider.Store(&fn)
}

// CurrentEventBusStatus returns the live snapshot, or a zero (disabled) status
// when no provider is registered — the dark default.
func CurrentEventBusStatus() EventBusStatus {
	if p := eventBusStatusProvider.Load(); p != nil {
		return (*p)()
	}
	return EventBusStatus{} // disabled: Enabled=false, BrokersSet=false, all zero
}
