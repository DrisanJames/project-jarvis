package eventbus

import (
	"sync"
	"testing"
	"time"
)

// resetRegistry restores the package-level registry to its dark default so tests
// don't leak wiring into each other.
func resetRegistry(t *testing.T) {
	t.Helper()
	WireTaps(nil, nil, nil, nil, nil, nil)
}

// onFlagGate builds a FlagGate forced ON via its env var, without starting the
// background loop. Returns the gate and a cleanup that clears the env.
func onFlagGate(t *testing.T, name string) *FlagGate {
	t.Helper()
	t.Setenv("KAFKA_FLAG_"+upper(name), "true")
	return NewFlagGate(name, nil, time.Hour)
}

func offFlagGate(t *testing.T, name string) *FlagGate {
	t.Helper()
	// ensure env is unset/false => default OFF
	t.Setenv("KAFKA_FLAG_"+upper(name), "false")
	return NewFlagGate(name, nil, time.Hour)
}

// upper is a tiny local helper so the test doesn't depend on strings import
// styling; mirrors the gate's env-key uppercasing.
func upper(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

// TestPublish_NoOpWhenUnwired verifies that all three Publish* are no-ops when
// the registry has never been wired (nil taps + nil gates). The bar is simply:
// no panic, no crash. There is no producer to assert against — that's the point.
func TestPublish_NoOpWhenUnwired(t *testing.T) {
	resetRegistry(t)

	// Must not panic even though everything is nil.
	PublishLake("a@example.com", []byte(`{"k":1}`))
	PublishIngest("camp-1", []byte(`{"k":2}`))
	PublishSuppressAdd("a@example.com", []byte(`{"k":3}`))
}

// TestPublish_NoOpWhenFlagOff verifies that even with LIVE taps installed, a
// Publish* produces NOTHING while its flag is OFF (the default).
func TestPublish_NoOpWhenFlagOff(t *testing.T) {
	resetRegistry(t)
	defer resetRegistry(t)

	lakeF := &FakeProducer{}
	ingestF := &FakeProducer{}
	suppF := &FakeProducer{}

	lake := NewTap(lakeF)
	ingest := NewTap(ingestF)
	supp := NewTap(suppF)
	defer lake.Close()
	defer ingest.Close()
	defer supp.Close()

	WireTaps(
		lake, ingest, supp,
		offFlagGate(t, "produce_lake"),
		offFlagGate(t, "produce_ingest"),
		offFlagGate(t, "produce_suppress"),
	)

	PublishLake("a@example.com", []byte(`{"k":1}`))
	PublishIngest("camp-1", []byte(`{"k":2}`))
	PublishSuppressAdd("a@example.com", []byte(`{"k":3}`))

	// Give the (idle) drain goroutines a beat; nothing should have been queued.
	time.Sleep(50 * time.Millisecond)

	if n := lakeF.Count(); n != 0 {
		t.Fatalf("lake produced %d records with flag OFF, want 0", n)
	}
	if n := ingestF.Count(); n != 0 {
		t.Fatalf("ingest produced %d records with flag OFF, want 0", n)
	}
	if n := suppF.Count(); n != 0 {
		t.Fatalf("suppress produced %d records with flag OFF, want 0", n)
	}
}

// TestPublish_NoOpWhenGateNilButTapLive verifies the fail-safe: a live tap with a
// NIL gate is still a no-op (nil gate is treated as OFF, never fail-open).
func TestPublish_NoOpWhenGateNilButTapLive(t *testing.T) {
	resetRegistry(t)
	defer resetRegistry(t)

	f := &FakeProducer{}
	tap := NewTap(f)
	defer tap.Close()

	// Taps live, all gates nil.
	WireTaps(tap, tap, tap, nil, nil, nil)

	PublishLake("a@example.com", []byte(`{"k":1}`))
	PublishIngest("camp-1", []byte(`{"k":2}`))
	PublishSuppressAdd("a@example.com", []byte(`{"k":3}`))

	time.Sleep(50 * time.Millisecond)
	if n := f.Count(); n != 0 {
		t.Fatalf("produced %d records with nil gate, want 0 (must fail-safe)", n)
	}
}

// TestPublish_ProducesWhenWiredAndOn verifies the happy path: wired + flag ON =>
// each Publish* produces exactly one record on the right topic with the right
// key/value, via a FakeProducer-backed Tap.
func TestPublish_ProducesWhenWiredAndOn(t *testing.T) {
	resetRegistry(t)
	defer resetRegistry(t)

	lakeF := &FakeProducer{}
	ingestF := &FakeProducer{}
	suppF := &FakeProducer{}

	// Signal channels so we deterministically wait for the async produce.
	var wg sync.WaitGroup
	wg.Add(3)
	once := func() func(FakeRecord) {
		var o sync.Once
		return func(FakeRecord) { o.Do(wg.Done) }
	}
	lakeF.OnProduce = once()
	ingestF.OnProduce = once()
	suppF.OnProduce = once()

	lake := NewTap(lakeF)
	ingest := NewTap(ingestF)
	supp := NewTap(suppF)
	defer lake.Close()
	defer ingest.Close()
	defer supp.Close()

	WireTaps(
		lake, ingest, supp,
		onFlagGate(t, "produce_lake"),
		onFlagGate(t, "produce_ingest"),
		onFlagGate(t, "produce_suppress"),
	)

	PublishLake("lake-key", []byte(`{"lake":true}`))
	PublishIngest("ingest-key", []byte(`{"ingest":true}`))
	PublishSuppressAdd("supp-key", []byte(`{"supp":true}`))

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for taps to produce")
	}

	assertOne := func(name string, f *FakeProducer, wantTopic, wantKey, wantVal string) {
		recs := f.Records()
		if len(recs) != 1 {
			t.Fatalf("%s: produced %d records, want 1", name, len(recs))
		}
		r := recs[0]
		if r.Topic != wantTopic {
			t.Errorf("%s: topic = %q, want %q", name, r.Topic, wantTopic)
		}
		if string(r.Key) != wantKey {
			t.Errorf("%s: key = %q, want %q", name, string(r.Key), wantKey)
		}
		if string(r.Value) != wantVal {
			t.Errorf("%s: value = %q, want %q", name, string(r.Value), wantVal)
		}
	}

	assertOne("lake", lakeF, TopicLake, "lake-key", `{"lake":true}`)
	assertOne("ingest", ingestF, TopicIngest, "ingest-key", `{"ingest":true}`)
	assertOne("suppress", suppF, TopicSuppression, "supp-key", `{"supp":true}`)
}
