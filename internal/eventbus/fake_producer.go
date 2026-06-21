package eventbus

import (
	"context"
	"sync"
)

// FakeRecord is one record captured by a FakeProducer.
type FakeRecord struct {
	Topic string
	Key   []byte
	Value []byte
}

// FakeProducer is an in-memory Producer for unit tests (this package's and
// downstream packages'). It records every produced record for assertions and
// can be configured to fail. It is concurrency-safe so it can sit behind a Tap
// whose background goroutine produces from another goroutine.
type FakeProducer struct {
	mu      sync.Mutex
	records []FakeRecord
	closed  bool

	// Err, when non-nil, is returned by every Produce call (to exercise the
	// Tap's failed-counter and the Consumer's DLQ path indirectly).
	Err error

	// OnProduce, when set, is invoked (under no lock) after a record is recorded
	// but before Produce returns. Useful for signaling test channels.
	OnProduce func(FakeRecord)
}

// compile-time assertion: FakeProducer satisfies Producer.
var _ Producer = (*FakeProducer)(nil)

// Produce records the record (unless Err is set, in which case it records
// nothing and returns Err — mirroring a real produce failure).
func (f *FakeProducer) Produce(_ context.Context, topic string, key, value []byte) error {
	f.mu.Lock()
	if f.Err != nil {
		err := f.Err
		f.mu.Unlock()
		return err
	}
	// Copy the byte slices so later mutation by the caller can't corrupt the
	// captured record.
	rec := FakeRecord{Topic: topic, Key: cloneBytes(key), Value: cloneBytes(value)}
	f.records = append(f.records, rec)
	cb := f.OnProduce
	f.mu.Unlock()
	if cb != nil {
		cb(rec)
	}
	return nil
}

// Close marks the producer closed (idempotent).
func (f *FakeProducer) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

// Records returns a copy of all captured records.
func (f *FakeProducer) Records() []FakeRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeRecord, len(f.records))
	copy(out, f.records)
	return out
}

// Count returns the number of captured records.
func (f *FakeProducer) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.records)
}

// Closed reports whether Close was called.
func (f *FakeProducer) Closed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
