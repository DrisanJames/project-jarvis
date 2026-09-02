package sendqueue

// REQ-089 DoD 4 — ONE FLUSH PER WAVE.
//
// EnqueuePMTAWave used to call ProduceSync once per recipient, inside the wave
// transaction. At a conservative 1-3ms per synchronous round trip a 45k-recipient
// wave is 45-135 SECONDS of broker wait while the wave/campaign/isp_plan rows are
// locked — which is how a wave outran WAVE_PROCESSOR_TIMEOUT_SECONDS (120s in
// prod), rolled back, and re-produced itself every 15s forever.

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/eventbus"
)

// fakeBatchProducer is a FakeProducer that ALSO implements
// eventbus.BatchProducer, counting flushes (batch calls) separately from
// records, so a test can assert "one flush per wave" rather than "N produces".
type fakeBatchProducer struct {
	mu      sync.Mutex
	flushes int
	records int
	err     error
}

func (f *fakeBatchProducer) Produce(context.Context, string, []byte, []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// A per-record produce means the batch path was NOT taken.
	f.flushes++
	f.records++
	return f.err
}

func (f *fakeBatchProducer) ProduceBatch(_ context.Context, _ string, keys, values [][]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushes++
	f.records += len(values)
	return f.err
}

func (f *fakeBatchProducer) Close() error { return nil }

var _ eventbus.BatchProducer = (*fakeBatchProducer)(nil)

func cmdsN(n int) []SendCommand {
	out := make([]SendCommand, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, SendCommand{IdempotencyKey: uuid.New(), CampaignID: uuid.New(), SubscriberID: uuid.New()})
	}
	return out
}

// A wave inside one chunk = exactly ONE flush, carrying every record.
func TestEnqueueSendCommandsBatch_OneFlushPerWave(t *testing.T) {
	p := &fakeBatchProducer{}
	cmds := cmdsN(750)
	if err := EnqueueSendCommandsBatch(context.Background(), p, cmds); err != nil {
		t.Fatalf("EnqueueSendCommandsBatch: %v", err)
	}
	if p.flushes != 1 {
		t.Fatalf("want exactly 1 flush for a 750-recipient wave, got %d", p.flushes)
	}
	if p.records != 750 {
		t.Fatalf("want 750 records, got %d", p.records)
	}
}

// A wave larger than the chunk is flushed in ceil(n/chunk) calls — still O(waves),
// never O(recipients).
func TestEnqueueSendCommandsBatch_ChunksLargeWave(t *testing.T) {
	p := &fakeBatchProducer{}
	const n = 4501
	if err := EnqueueSendCommandsBatch(context.Background(), p, cmdsN(n)); err != nil {
		t.Fatalf("EnqueueSendCommandsBatch: %v", err)
	}
	want := (n + batchProduceChunk - 1) / batchProduceChunk
	if p.flushes != want {
		t.Fatalf("want %d flushes for %d recipients, got %d", want, n, p.flushes)
	}
	if p.records != n {
		t.Fatalf("want %d records, got %d", n, p.records)
	}
}

// A producer WITHOUT the batch extension (eventbus.FakeProducer, and any future
// Producer implementation) still works — per-record, exactly as before.
func TestEnqueueSendCommandsBatch_FallsBackForPlainProducer(t *testing.T) {
	p := &eventbus.FakeProducer{}
	if err := EnqueueSendCommandsBatch(context.Background(), p, cmdsN(3)); err != nil {
		t.Fatalf("EnqueueSendCommandsBatch: %v", err)
	}
	if p.Count() != 3 {
		t.Fatalf("want 3 records via the fallback loop, got %d", p.Count())
	}
}

// The batch path keeps the keying contract: record key == idempotency-key bytes,
// so duplicates of a key still co-locate on one partition.
func TestEnqueueSendCommandsBatch_KeysAreIdempotencyKeys(t *testing.T) {
	var gotKeys [][]byte
	p := &keyCapturingBatchProducer{onBatch: func(keys [][]byte) { gotKeys = keys }}
	cmds := cmdsN(2)
	if err := EnqueueSendCommandsBatch(context.Background(), p, cmds); err != nil {
		t.Fatalf("EnqueueSendCommandsBatch: %v", err)
	}
	if len(gotKeys) != 2 {
		t.Fatalf("want 2 keys, got %d", len(gotKeys))
	}
	for i, cmd := range cmds {
		if string(gotKeys[i]) != string(cmd.IdempotencyKey[:]) {
			t.Fatalf("key %d is not the idempotency-key bytes", i)
		}
	}
}

type keyCapturingBatchProducer struct {
	onBatch func(keys [][]byte)
}

func (k *keyCapturingBatchProducer) Produce(context.Context, string, []byte, []byte) error {
	return nil
}
func (k *keyCapturingBatchProducer) ProduceBatch(_ context.Context, _ string, keys, _ [][]byte) error {
	k.onBatch(keys)
	return nil
}
func (k *keyCapturingBatchProducer) Close() error { return nil }
