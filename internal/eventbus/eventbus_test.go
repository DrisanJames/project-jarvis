package eventbus

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Compile-time interface assertions: both impls satisfy Producer.
// (kgoProducer is asserted in producer.go; FakeProducer in fake_producer.go.
// Re-assert here so a refactor that breaks either is caught by the test build.)
// ---------------------------------------------------------------------------

var (
	_ Producer = (*kgoProducer)(nil)
	_ Producer = (*FakeProducer)(nil)
	_ DLQ      = DLQFunc(nil)
)

// ---------------------------------------------------------------------------
// Tap
// ---------------------------------------------------------------------------

func TestTap_ProducesViaFake(t *testing.T) {
	fake := &FakeProducer{}
	var got int32
	fake.OnProduce = func(FakeRecord) { atomic.AddInt32(&got, 1) }

	tap := NewTap(fake)
	defer tap.Close()

	const n = 50
	for i := 0; i < n; i++ {
		tap.Publish("evt.lake.v1", []byte("k"), []byte("v"))
	}

	// Wait for the drain goroutine to flush.
	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&got) == n })

	produced, dropped, failed := tap.Stats()
	if produced != n {
		t.Fatalf("produced = %d, want %d", produced, n)
	}
	if dropped != 0 || failed != 0 {
		t.Fatalf("dropped=%d failed=%d, want 0/0", dropped, failed)
	}
	if fake.Count() != n {
		t.Fatalf("fake captured %d, want %d", fake.Count(), n)
	}
}

func TestTap_DropsAndCountsWhenFull_NeverBlocks(t *testing.T) {
	// Block the producer indefinitely so the drain goroutine wedges after taking
	// exactly one record; every subsequent Publish must drop, not block.
	release := make(chan struct{})
	blocker := &blockingProducer{release: release}
	tap := NewTap(blocker)
	defer func() { close(release); tap.Close() }()

	// Give the drain goroutine a moment to pull the first record and block.
	// Fill far beyond the channel depth; Publish must return immediately every
	// time. We assert non-blocking by bounding wall-clock time.
	total := tapChannelDepth + 5000
	start := time.Now()
	for i := 0; i < total; i++ {
		tap.Publish("t", nil, []byte("x"))
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("Publish blocked: %d publishes took %v", total, elapsed)
	}

	// Some were buffered (channel depth) / one in-flight; the rest dropped.
	waitFor(t, time.Second, func() bool {
		_, dropped, _ := tap.Stats()
		return dropped > 0
	})
	produced, dropped, failed := tap.Stats()
	if dropped == 0 {
		t.Fatalf("expected drops when buffer full, got dropped=0 (produced=%d failed=%d)", produced, failed)
	}
	// We published `total`; everything is either dropped, buffered, or the one
	// in-flight. Dropped should be roughly total - depth - (in-flight).
	if dropped < uint64(total-tapChannelDepth-2) {
		t.Fatalf("dropped=%d too low for total=%d depth=%d", dropped, total, tapChannelDepth)
	}
}

func TestTap_Disabled_NilProducer_IsNoOp(t *testing.T) {
	tap := NewTap(nil)
	if tap.Enabled() {
		t.Fatal("nil-producer tap should be disabled")
	}
	// Must not panic or block.
	tap.Publish("t", []byte("k"), []byte("v"))
	produced, dropped, failed := tap.Stats()
	if produced != 0 || dropped != 0 || failed != 0 {
		t.Fatalf("disabled tap stats = %d/%d/%d, want 0/0/0", produced, dropped, failed)
	}
	if err := tap.Close(); err != nil {
		t.Fatalf("Close on disabled tap: %v", err)
	}
}

func TestTap_CountsFailures(t *testing.T) {
	fake := &FakeProducer{Err: errors.New("boom")}
	tap := NewTap(fake)
	defer tap.Close()

	for i := 0; i < 10; i++ {
		tap.Publish("t", nil, []byte("v"))
	}
	waitFor(t, time.Second, func() bool {
		_, _, failed := tap.Stats()
		return failed == 10
	})
	produced, _, failed := tap.Stats()
	if produced != 0 || failed != 10 {
		t.Fatalf("produced=%d failed=%d, want 0/10", produced, failed)
	}
}

// ---------------------------------------------------------------------------
// Consumer (broker-free, via processRecord)
// ---------------------------------------------------------------------------

func TestProcessRecord_SuccessHandled(t *testing.T) {
	var seen int32
	h := func(ctx context.Context, key, value []byte) error {
		atomic.AddInt32(&seen, 1)
		return nil
	}
	dlq := &recordingDLQ{}
	err := processRecord(context.Background(), h, dlq, ConsumerOptions{}, "t", []byte("k"), []byte("v"))
	if err != nil {
		t.Fatalf("processRecord err = %v, want nil (commit)", err)
	}
	if seen != 1 {
		t.Fatalf("handler called %d times, want 1", seen)
	}
	if dlq.count() != 0 {
		t.Fatalf("DLQ hit %d times on success, want 0", dlq.count())
	}
}

func TestProcessRecord_PanicIsolatedThenDLQ(t *testing.T) {
	var calls int32
	h := func(ctx context.Context, key, value []byte) error {
		atomic.AddInt32(&calls, 1)
		panic("kaboom")
	}
	dlq := &recordingDLQ{}
	opts := ConsumerOptions{MaxRetries: 2, RetryBackoff: time.Millisecond}

	// Must NOT propagate the panic; returns nil because the record is parked.
	err := processRecord(context.Background(), h, dlq, opts, "t", []byte("k"), []byte("v"))
	if err != nil {
		t.Fatalf("processRecord returned err for DLQ-parked panic record: %v", err)
	}
	// First attempt + 2 retries = 3 calls, all panicking, all recovered.
	if calls != 3 {
		t.Fatalf("handler called %d times, want 3 (1 + 2 retries)", calls)
	}
	if dlq.count() != 1 {
		t.Fatalf("DLQ hit %d times, want 1", dlq.count())
	}
	if dlq.lastCause == nil {
		t.Fatal("DLQ cause should carry the panic")
	}
}

func TestProcessRecord_PermanentFailureHitsDLQ(t *testing.T) {
	wantErr := errors.New("permanent")
	h := func(ctx context.Context, key, value []byte) error { return wantErr }
	dlq := &recordingDLQ{}
	opts := ConsumerOptions{MaxRetries: 1, RetryBackoff: time.Millisecond}

	err := processRecord(context.Background(), h, dlq, opts, "t", []byte("k"), []byte("v"))
	if err != nil {
		t.Fatalf("processRecord err = %v, want nil (DLQ park => commit)", err)
	}
	if dlq.count() != 1 {
		t.Fatalf("DLQ hit %d, want 1", dlq.count())
	}
	if !errors.Is(dlq.lastCause, wantErr) {
		t.Fatalf("DLQ cause = %v, want wrap of %v", dlq.lastCause, wantErr)
	}
}

func TestProcessRecord_TransientThenSuccess_NoDLQ(t *testing.T) {
	var attempt int32
	h := func(ctx context.Context, key, value []byte) error {
		if atomic.AddInt32(&attempt, 1) < 3 {
			return errors.New("transient")
		}
		return nil
	}
	dlq := &recordingDLQ{}
	opts := ConsumerOptions{MaxRetries: 5, RetryBackoff: time.Millisecond}

	err := processRecord(context.Background(), h, dlq, opts, "t", nil, nil)
	if err != nil {
		t.Fatalf("processRecord err = %v, want nil", err)
	}
	if attempt != 3 {
		t.Fatalf("handler called %d times, want 3", attempt)
	}
	if dlq.count() != 0 {
		t.Fatalf("DLQ hit %d, want 0 (eventually succeeded)", dlq.count())
	}
}

func TestProcessRecord_DLQFailure_RecordRetained(t *testing.T) {
	h := func(ctx context.Context, key, value []byte) error { return errors.New("handler fail") }
	dlqErr := errors.New("dlq write fail")
	dlq := DLQFunc(func(ctx context.Context, topic string, key, value []byte, cause error) error {
		return dlqErr
	})
	opts := ConsumerOptions{MaxRetries: 0, RetryBackoff: time.Millisecond}

	// Both handler and DLQ fail => must return non-nil so the caller does NOT
	// commit (record retained for redelivery — never lost).
	err := processRecord(context.Background(), h, dlq, opts, "t", nil, nil)
	if err == nil {
		t.Fatal("expected non-nil error when DLQ write fails (record must be retained)")
	}
}

func TestProcessRecord_NoDLQ_RetainsOnFailure(t *testing.T) {
	h := func(ctx context.Context, key, value []byte) error { return errors.New("fail") }
	opts := ConsumerOptions{MaxRetries: 0, RetryBackoff: time.Millisecond}
	err := processRecord(context.Background(), h, nil, opts, "t", nil, nil)
	if err == nil {
		t.Fatal("expected non-nil error when no DLQ and handler fails (do not lose record)")
	}
}

// ---------------------------------------------------------------------------
// FlagGate
// ---------------------------------------------------------------------------

func TestFlagGate_DefaultOff(t *testing.T) {
	// No env, no redis.
	t.Setenv("KAFKA_FLAG_MYFLOW", "") // LookupEnv would still find ""; unset below
	// Actually unset to test the truly-absent path:
	if err := unsetenv(t, "KAFKA_FLAG_MYFLOW"); err != nil {
		t.Fatal(err)
	}
	g := NewFlagGate("myflow", nil, 50*time.Millisecond)
	if g.Enabled() {
		t.Fatal("flag should default OFF when unset")
	}
}

func TestFlagGate_EnvOn(t *testing.T) {
	t.Setenv("KAFKA_FLAG_PRODUCE_LAKE", "true")
	g := NewFlagGate("produce_lake", nil, 50*time.Millisecond)
	if !g.Enabled() {
		t.Fatal("flag should be ON from env=true")
	}

	// And env=false / junk => off.
	t.Setenv("KAFKA_FLAG_PRODUCE_LAKE", "false")
	g2 := NewFlagGate("produce_lake", nil, 50*time.Millisecond)
	if g2.Enabled() {
		t.Fatal("flag should be OFF from env=false")
	}
}

func TestFlagGate_ReflectsChangedValue(t *testing.T) {
	// env starts unset (off). Background poll should pick up a later env change.
	if err := unsetenv(t, "KAFKA_FLAG_DYNAMIC"); err != nil {
		t.Fatal(err)
	}
	g := NewFlagGate("dynamic", nil, 20*time.Millisecond)
	if g.Enabled() {
		t.Fatal("should start OFF")
	}
	g.Start()
	defer g.Stop()

	t.Setenv("KAFKA_FLAG_DYNAMIC", "on")
	waitFor(t, time.Second, func() bool { return g.Enabled() })
	if !g.Enabled() {
		t.Fatal("flag should reflect runtime env change to ON")
	}

	// Flip back to off; Refresh forces immediate re-eval too.
	t.Setenv("KAFKA_FLAG_DYNAMIC", "off")
	if g.Refresh(context.Background()) {
		t.Fatal("Refresh should report OFF after env=off")
	}
	if g.Enabled() {
		t.Fatal("flag should reflect change back to OFF")
	}
}

func TestFlagGate_NilRedisTolerated(t *testing.T) {
	// Purely exercises that a nil *redis.Client never panics and falls to env.
	if err := unsetenv(t, "KAFKA_FLAG_NILREDIS"); err != nil {
		t.Fatal(err)
	}
	g := NewFlagGate("nilredis", nil, time.Hour)
	if g.Refresh(context.Background()) {
		t.Fatal("nil-redis + unset env should be OFF")
	}
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

func TestConfig_DisabledByDefault(t *testing.T) {
	if err := unsetenv(t, EnvBrokers); err != nil {
		t.Fatal(err)
	}
	c := LoadConfig()
	if c.Enabled() {
		t.Fatal("Config with no KAFKA_BROKERS must be disabled")
	}
}

func TestConfig_EnabledWithBrokers(t *testing.T) {
	t.Setenv(EnvBrokers, "b1:9092, b2:9092 b3:9092")
	c := LoadConfig()
	if !c.Enabled() {
		t.Fatal("Config with KAFKA_BROKERS must be enabled")
	}
	if len(c.Brokers) != 3 {
		t.Fatalf("brokers = %v, want 3", c.Brokers)
	}
	if c.pollInterval() != DefaultFlagPollInterval {
		t.Fatalf("pollInterval = %v, want default", c.pollInterval())
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// blockingProducer blocks in Produce until release is closed, used to wedge the
// Tap drain goroutine and prove drop-and-count.
type blockingProducer struct {
	release chan struct{}
}

func (b *blockingProducer) Produce(ctx context.Context, topic string, key, value []byte) error {
	<-b.release
	return nil
}
func (b *blockingProducer) Close() error { return nil }

type recordingDLQ struct {
	mu        sync.Mutex
	n         int
	lastCause error
}

func (d *recordingDLQ) Dead(ctx context.Context, topic string, key, value []byte, cause error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.n++
	d.lastCause = cause
	return nil
}
func (d *recordingDLQ) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// unsetenv removes a var and restores it after the test (t.Setenv only sets).
func unsetenv(t *testing.T, key string) error {
	t.Helper()
	prev, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		return err
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, prev)
		} else {
			_ = os.Unsetenv(key)
		}
	})
	return nil
}
