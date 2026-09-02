package eventbus

// health.go carries the LIVENESS snapshot for a group consumer, and the
// process-global registry that lets /health (internal/api, via cmd/server) and
// the send-path monitors (internal/worker OutboxSelfCheck) read the SAME numbers
// without an import cycle.
//
// WHY THIS EXISTS (2026-09-01, SK-4 wedge): /health reported
// `consumer_running: true` for three hours while the send.commands.v1 consumer
// held its partition assignments and inserted nothing. That flag was set ONCE at
// boot, after Start returned nil, and never re-evaluated — a goroutine-was-
// launched boolean, not liveness. 54,097 sidecar recipients plus ~200k board
// recipients parked on the topic with zero errors anywhere.
//
// A boolean cannot express "alive but making no progress". These four fields
// can, and they fail in the SAFE direction (stale timestamps, growing lag):
//
//	LastPollAt    — the loop is calling PollFetches (broker session alive)
//	LastHandledAt — a record actually completed (the DB write landed)
//	InFlight      — >0 with an old LastHandledAt == stuck INSIDE the handler
//	LagMax        — records the broker has that this group has not consumed
//
// LAG WITHOUT kadm: github.com/twmb/franz-go/pkg/kadm is NOT a dependency of
// this module (go.mod carries franz-go v1.18.1 + kmsg only), so committed-vs-end
// offsets are not available. Lag is instead derived from the fetch itself:
// kgo.FetchPartition.HighWatermark minus the offset AFTER the last record this
// loop processed on that partition. That is a lower bound observed only while
// the loop is polling — a fully wedged consumer freezes its lag AND its
// LastPollAt, so the two must be read together (a stale LastPollAt invalidates
// LagMax). Adding kadm would make lag independent of the loop; that is a
// dependency decision, not a code one.

import (
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// ConsumerSnapshot is a point-in-time liveness reading for one group consumer.
// Every field is safe to read concurrently; the zero value means "never ran".
type ConsumerSnapshot struct {
	// Name identifies the consumer (e.g. "send-queue-writer").
	Name string
	// TaskID distinguishes ECS tasks, because the ALB answers /health from
	// whichever task it picks and the counters are PER TASK, not global.
	TaskID string
	// Running is false once the Run loop has returned, for ANY reason.
	Running bool
	// LastPollAt is when PollFetches last returned (broker session alive).
	LastPollAt time.Time
	// LastHandledAt is when a record last completed successfully.
	LastHandledAt time.Time
	// InFlight is the number of records currently inside the handler.
	InFlight int64
	// LagMax is the largest observed per-partition lag (see the file header:
	// derived from HighWatermark, only meaningful alongside a fresh LastPollAt).
	LagMax int64
	// LagKnown is false until at least one fetch carried records.
	LagKnown bool
	// RetainedPartitions is the number of partitions currently blocked from
	// committing forward because a record could be neither handled nor DLQ'd.
	RetainedPartitions int
	// DLQRecords counts RECORDS durably parked in the DLQ (distinct from the
	// per-ATTEMPT failure counters the queue writer keeps).
	DLQRecords uint64
}

// SecondsSinceLastHandled returns the age of the last successful handle, or -1
// when nothing has ever been handled (so a caller can tell "never" from "old").
func (s ConsumerSnapshot) SecondsSinceLastHandled() float64 {
	if s.LastHandledAt.IsZero() {
		return -1
	}
	return time.Since(s.LastHandledAt).Seconds()
}

// SecondsSinceLastPoll returns the age of the last broker poll, or -1 when the
// loop has never polled.
func (s ConsumerSnapshot) SecondsSinceLastPoll() float64 {
	if s.LastPollAt.IsZero() {
		return -1
	}
	return time.Since(s.LastPollAt).Seconds()
}

// sendQueueHealthProvider is the registered snapshot source for the SK-4
// send.commands.v1 queue-writer. cmd/server installs it after a successful
// Start; until then reads return the zero (never-ran) snapshot.
var sendQueueHealthProvider atomic.Pointer[func() ConsumerSnapshot]

// SetSendQueueHealthProvider registers the live snapshot source. Passing nil
// clears it (back to the never-ran default), which is what an un-wired or
// stopped send queue should report.
func SetSendQueueHealthProvider(fn func() ConsumerSnapshot) {
	if fn == nil {
		sendQueueHealthProvider.Store(nil)
		return
	}
	sendQueueHealthProvider.Store(&fn)
}

// SendQueueHealth is THE read point for send.commands.v1 consumer liveness.
// internal/api (via cmd/server) renders it on /health.event_bus.send_queue and
// internal/worker's OutboxSelfCheck alerts on it. It never blocks and never
// touches the network. A zero snapshot (Running=false, zero times) means the
// consumer was never wired in this process — NOT that it is wedged.
func SendQueueHealth() ConsumerSnapshot {
	if p := sendQueueHealthProvider.Load(); p != nil {
		return (*p)()
	}
	return ConsumerSnapshot{TaskID: TaskID()}
}

// ParkedLagThreshold / ParkedHandleAge are the alert thresholds for
// SendQueueParked. They are exported so the monitor and this package cannot
// drift apart on what "parked" means.
const (
	ParkedLagThreshold int64 = 1000
	ParkedHandleAge          = 10 * time.Minute
)

// SendQueueParked is THE alert predicate for "waves are routing to Kafka but
// nothing is landing" — the 2026-09-01 SK-4 wedge, expressed as a check.
// internal/worker's OutboxSelfCheck calls it each tick with the routed-wave
// counter delta since its previous tick (worker.KafkaRoutedWaves()).
//
// It fires only when waves ARE being routed (delta > 0) — a quiet topic is not
// a wedge — AND the consumer is failing to keep up in one of three ways:
//
//	consumer_running == false      -> the loop died; this task consumes nothing
//	lag_max > 1000                 -> the broker holds work this group has not taken
//	last_handled older than 10 min -> assigned, polling, inserting nothing
//
// A consumer that was never wired (zero snapshot, no poll ever) is NOT parked:
// on a task where SK-4 is dark there is nothing to consume. The returned string
// is the operator-facing reason, empty when not parked.
func SendQueueParked(routedWaveDelta uint64, snap ConsumerSnapshot) (bool, string) {
	if routedWaveDelta == 0 {
		return false, ""
	}
	// Never wired on this task: no provider registered, no poll ever recorded.
	if snap.LastPollAt.IsZero() && !snap.Running {
		return false, ""
	}
	if !snap.Running {
		return true, "send.commands.v1 consumer parked: consumer loop is NOT running (task " + snap.TaskID + ") while waves route to Kafka"
	}
	if snap.LagKnown && snap.LagMax > ParkedLagThreshold {
		return true, "send.commands.v1 consumer parked: lag_max=" + itoa(snap.LagMax) + " (task " + snap.TaskID + ")"
	}
	if snap.LastHandledAt.IsZero() || time.Since(snap.LastHandledAt) > ParkedHandleAge {
		return true, "send.commands.v1 consumer parked: no record handled in " + ParkedHandleAge.String() + " while waves route to Kafka (task " + snap.TaskID + ")"
	}
	return false, ""
}

// itoa avoids pulling strconv into the hot import set for one call site.
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// taskIDOnce caches the derived id; it cannot change within a process.
var taskIDCache atomic.Pointer[string]

// TaskID identifies this process for operators reading a per-task counter.
// Order: ECS_TASK_ID, the task id trailing ECS_CONTAINER_METADATA_URI_V4
// (Fargate always sets it), HOSTNAME (the container id under awsvpc), then
// os.Hostname(). Never empty — falls back to "unknown".
func TaskID() string {
	if p := taskIDCache.Load(); p != nil {
		return *p
	}
	id := deriveTaskID()
	taskIDCache.Store(&id)
	return id
}

func deriveTaskID() string {
	if v := strings.TrimSpace(os.Getenv("ECS_TASK_ID")); v != "" {
		return v
	}
	// .../v4/<task-id> or .../v4/<task-id>/task — take the segment after "v4".
	if uri := strings.TrimSpace(os.Getenv("ECS_CONTAINER_METADATA_URI_V4")); uri != "" {
		parts := strings.Split(strings.Trim(uri, "/"), "/")
		for i, p := range parts {
			if p == "v4" && i+1 < len(parts) {
				return parts[i+1]
			}
		}
		if n := len(parts); n > 0 && parts[n-1] != "" {
			return parts[n-1]
		}
	}
	if v := strings.TrimSpace(os.Getenv("HOSTNAME")); v != "" {
		return v
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}
