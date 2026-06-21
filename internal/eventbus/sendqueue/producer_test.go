package sendqueue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/eventbus"
)

// EnqueueSendCommands produces one record per command onto TopicSendCommands,
// keyed by the idempotency-key bytes (partition affinity + duplicate
// co-location).
func TestEnqueueSendCommands_ProducesKeyedRecords(t *testing.T) {
	fp := &eventbus.FakeProducer{}
	cmds := []SendCommand{newCmd(), newCmd()}

	if err := EnqueueSendCommands(context.Background(), fp, cmds); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	recs := fp.Records()
	if len(recs) != 2 {
		t.Fatalf("produced %d records, want 2", len(recs))
	}
	for i, rec := range recs {
		if rec.Topic != TopicSendCommands {
			t.Fatalf("record %d topic=%q, want %q", i, rec.Topic, TopicSendCommands)
		}
		// Key MUST be the idempotency-key bytes.
		if !bytes.Equal(rec.Key, cmds[i].IdempotencyKey[:]) {
			t.Fatalf("record %d key != idempotency key bytes", i)
		}
		// Value round-trips to the same command.
		var got SendCommand
		if err := json.Unmarshal(rec.Value, &got); err != nil {
			t.Fatalf("record %d value not JSON: %v", i, err)
		}
		if got.IdempotencyKey != cmds[i].IdempotencyKey {
			t.Fatalf("record %d key mismatch after round-trip", i)
		}
	}
}

// A zero idempotency key is refused — an unkeyed command cannot be deduped and
// would defeat the no-double-send invariant.
func TestEnqueueSendCommands_RejectsZeroKey(t *testing.T) {
	fp := &eventbus.FakeProducer{}
	err := EnqueueSendCommands(context.Background(), fp, []SendCommand{{IdempotencyKey: uuid.Nil}})
	if err == nil {
		t.Fatal("expected error for zero idempotency_key")
	}
	if fp.Count() != 0 {
		t.Fatalf("nothing should be produced for a zero key; got %d", fp.Count())
	}
}

// A produce error is surfaced (not swallowed) — send commands are durable work,
// not lossy hot-path events.
func TestEnqueueSendCommands_PropagatesProduceError(t *testing.T) {
	fp := &eventbus.FakeProducer{Err: errors.New("broker down")}
	err := EnqueueSendCommands(context.Background(), fp, []SendCommand{newCmd()})
	if err == nil {
		t.Fatal("expected produce error to propagate")
	}
}
