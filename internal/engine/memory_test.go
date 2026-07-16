package engine

import (
	"context"
	"strings"
	"testing"
	"time"
)

// newTestMemoryStore builds a store with a nil S3 client (all S3 calls are
// no-ops that succeed) and WITHOUT the background flush loop, so tests drive
// Flush deterministically.
func newTestMemoryStore() *MemoryStore {
	return &MemoryStore{
		client:      nil,
		bucket:      "test",
		flushQueue:  make(map[string][]byte),
		appendQueue: make(map[string][]byte),
		stopCh:      make(chan struct{}),
	}
}

// Append* must buffer lines in the append queue — NEVER perform S3 I/O on the
// hot path (the old GET+concat+PUT per append was the 2026-06 NAT cost
// incident).
func TestAppendBuffersLinesPerStream(t *testing.T) {
	t.Setenv("ENGINE_MEMORY_STREAMS_S3_ENABLED", "true") // exercise the real conviction S3 append path
	m := newTestMemoryStore()
	ctx := context.Background()

	if err := m.AppendConviction(ctx, ISP("gmail"), AgentType("throttle"), map[string]string{"v": "1"}); err != nil {
		t.Fatalf("AppendConviction: %v", err)
	}
	if err := m.AppendConviction(ctx, ISP("gmail"), AgentType("throttle"), map[string]string{"v": "2"}); err != nil {
		t.Fatalf("AppendConviction: %v", err)
	}
	if err := m.AppendDecision(ctx, ISP("yahoo"), AgentType("reputation"), map[string]string{"d": "1"}); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	conv := string(m.appendQueue["agents/gmail/throttle/convictions"])
	if strings.Count(conv, "\n") != 2 {
		t.Errorf("expected 2 buffered conviction lines, got %q", conv)
	}
	dec := string(m.appendQueue["agents/yahoo/reputation/decisions"])
	if strings.Count(dec, "\n") != 1 {
		t.Errorf("expected 1 buffered decision line, got %q", dec)
	}
}

// Flush must drain the append queue (nil client == successful no-op PUTs).
func TestFlushDrainsAppendQueue(t *testing.T) {
	t.Setenv("ENGINE_MEMORY_STREAMS_S3_ENABLED", "true") // real conviction append so the flush has something to drain
	m := newTestMemoryStore()
	ctx := context.Background()
	_ = m.AppendConviction(ctx, ISP("gmail"), AgentType("throttle"), map[string]string{"v": "1"})
	_ = m.AppendSignal(ctx, ISP("aol"), AgentType("pool"), map[string]string{"s": "1"})

	m.Flush(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.appendQueue) != 0 {
		t.Errorf("append queue not drained: %v", m.appendQueue)
	}
}

// Delta keys must sort lexically in chronological order and never collide for
// flushes within the same second.
func TestDeltaKeyOrderingAndUniqueness(t *testing.T) {
	m := newTestMemoryStore()
	now := time.Date(2026, 6, 10, 4, 7, 30, 123e6, time.UTC)

	k1 := m.deltaKey("agents/gmail/throttle/convictions", now)
	k2 := m.deltaKey("agents/gmail/throttle/convictions", now) // same instant
	k3 := m.deltaKey("agents/gmail/throttle/convictions", now.Add(31*time.Second))

	if k1 == k2 {
		t.Errorf("same-instant keys collided: %s", k1)
	}
	if !(k1 < k2 && k2 < k3) {
		t.Errorf("keys not in chronological lexical order:\n%s\n%s\n%s", k1, k2, k3)
	}
	want := "agents/gmail/throttle/convictions/dt=2026-06-10/040730.123-"
	if !strings.HasPrefix(k1, want) {
		t.Errorf("key %q does not have expected prefix %q", k1, want)
	}
	if !strings.HasSuffix(k1, ".jsonl") {
		t.Errorf("key %q missing .jsonl suffix", k1)
	}
}

// With a nil client the read path must return empty without error (matches
// the rest of MemoryStore's nil-client no-op contract).
func TestReadConvictionsNilClient(t *testing.T) {
	m := newTestMemoryStore()
	got, err := m.ReadConvictions(context.Background(), ISP("gmail"), AgentType("throttle"))
	if err != nil {
		t.Fatalf("ReadConvictions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no convictions, got %d", len(got))
	}
}
