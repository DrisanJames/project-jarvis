package sendqueue

// queuewrite_liveness_test.go covers the SK-4 side of REQ-088: the DLQ park is
// attributed to a campaign/wave and counted as a RECORD, and the /health
// snapshot is derived from the live consumer rather than a boot boolean.

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/eventbus"
)

func TestDLQPark_CountsRecordsAndFiresHook(t *testing.T) {
	c := NewQueueWriterConsumer(nil, eventbus.Config{})
	var hookSeen []uint64
	c.WithDLQHook(func(n uint64) { hookSeen = append(hookSeen, n) })

	cmd := SendCommand{
		IdempotencyKey: uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		CampaignID:     uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		WaveID:         uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		SubscriberID:   uuid.MustParse("44444444-4444-4444-8444-444444444444"),
	}
	value := mustJSON(t, cmd)

	c.onDLQPark(TopicQueueWrites, []byte("k"), value, 4, errTest)
	if got := c.DLQRecords(); got != 1 {
		t.Fatalf("DLQRecords = %d, want 1", got)
	}
	// An undecodable payload must still be counted — a recipient left the send
	// path either way; only the log line degrades.
	c.onDLQPark(TopicQueueWrites, []byte("k"), []byte("{not json"), 4, errTest)
	if got := c.DLQRecords(); got != 2 {
		t.Fatalf("DLQRecords = %d after an undecodable park, want 2", got)
	}
	if len(hookSeen) != 2 || hookSeen[1] != 2 {
		t.Fatalf("hook saw %v, want the running count on each park", hookSeen)
	}
}

func TestDLQ_SnapshotIsLiveNotBootBoolean(t *testing.T) {
	// A consumer that was never started must report Running=false. The bug this
	// replaces set consumer_running=true once, after Start returned nil, and
	// held it through a three-hour wedge.
	c := NewQueueWriterConsumer(nil, eventbus.Config{})
	if c.Running() {
		t.Fatal("un-started QueueWriterConsumer must not report Running()")
	}
	snap := c.Snapshot()
	if snap.Running {
		t.Fatal("un-started QueueWriterConsumer must not report Snapshot().Running")
	}
	if snap.Name != "send-queue-writer" {
		t.Fatalf("snapshot name = %q", snap.Name)
	}
	if snap.TaskID == "" {
		t.Fatal("snapshot must carry a task id so two ECS tasks are distinguishable")
	}
	if snap.SecondsSinceLastHandled() != -1 {
		t.Fatal("never-handled must read -1, not 0")
	}

	// Start is a no-op on a disabled bus (no brokers) — and must NOT leave the
	// health surface claiming a running consumer.
	if err := c.Start(t.Context(), nil); err != nil {
		t.Fatalf("Start on a dark bus: %v", err)
	}
	if c.Running() || c.Snapshot().Running {
		t.Fatal("dark-bus Start must not report a running consumer")
	}
}

var errTest = errTestErr{}

type errTestErr struct{}

func (errTestErr) Error() string { return "test failure" }

func mustJSON(t *testing.T, cmd SendCommand) []byte {
	t.Helper()
	b, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
