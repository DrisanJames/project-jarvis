package sendqueue

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// SendSink is the boundary between the idempotent send pipeline and the actual
// transport. processSend calls Send EXACTLY once per key it owns (the ledger
// claim serializes that), passing the full SendCommand so the sink can carry the
// IdempotencyKey as a PMTA X-Idempotency header — the backstop that makes a
// re-send (after a crash before RECORD) safe even if the ledger and PMTA
// momentarily disagree.
//
// Send returns the transport's message id on success. An error leaves the ledger
// row 'claimed' and the Kafka offset UNCOMMITTED, so the record is redelivered
// and retried — never dropped.
//
// The real PMTA sink is deliberately NOT in this package: wiring it is a later,
// separately-gated step. This file ships only the interface + the two fakes the
// shadow default and the resilience harness need.
type SendSink interface {
	Send(ctx context.Context, cmd SendCommand) (messageID string, err error)
}

// NoOpSink is the SHADOW DEFAULT: it sends NO real email, records nothing, and
// returns a synthetic-but-deterministic message id derived from the idempotency
// key. Because it is deterministic, even a re-send after a crash yields the same
// "message id", so the ledger and any downstream correlation stay consistent in
// shadow mode. This is what a dark deploy uses until the real PMTA sink is
// explicitly wired and flagged on.
type NoOpSink struct{}

// compile-time assertion.
var _ SendSink = NoOpSink{}

// Send returns a synthetic id and never errors. The id is a stable UUIDv5 over
// the idempotency key so it is reproducible across redeliveries.
func (NoOpSink) Send(_ context.Context, cmd SendCommand) (string, error) {
	return "noop-" + uuid.NewSHA1(noOpSinkNamespace, cmd.IdempotencyKey[:]).String(), nil
}

// noOpSinkNamespace is a fixed namespace so NoOpSink ids are stable and never
// collide with real PMTA message ids.
var noOpSinkNamespace = uuid.MustParse("b3d9c1e2-7a64-4f0b-9c1d-2e5f8a3b6c7d")

// CountingSink is the test/resilience double. It MODELS PMTA's own idempotency:
// it dedups by IdempotencyKey, so asking it to send a key it already sent is
// recorded as a DUPLICATE rather than a second real send. The resilience harness
// asserts DuplicateKeySends()==0 to prove no double-send reached "PMTA".
//
// It is exported and thread-safe so multiple concurrent processSend goroutines
// (the redelivery / concurrent-consumer simulation) can hammer it. Failure and
// slowness are injectable via BeforeSend and Delay so the harness can drive the
// error / crash-window paths.
type CountingSink struct {
	// BeforeSend, if non-nil, runs at the start of every Send. Returning a
	// non-nil error makes Send fail WITHOUT recording the key (modeling a
	// transport that rejected before accepting the message) — the ledger stays
	// 'claimed' and the offset is not committed, so the record redelivers.
	BeforeSend func(SendCommand) error

	// Delay, if > 0, sleeps inside Send. Lets the harness widen the race window
	// between two concurrent claims of the same key.
	Delay time.Duration

	mu sync.Mutex
	// sends counts every accepted Send call (including the second time a key is
	// presented — those increment sends AND duplicateKeySends, modeling that the
	// caller asked PMTA to send a key it had already accepted).
	sends int
	// seen records keys already accepted; its size is DistinctKeys().
	seen map[uuid.UUID]string
	// duplicateKeySends counts accepted Send calls for a key already in seen.
	duplicateKeySends int
}

// compile-time assertion.
var _ SendSink = (*CountingSink)(nil)

// NewCountingSink returns a ready CountingSink.
func NewCountingSink() *CountingSink {
	return &CountingSink{seen: make(map[uuid.UUID]string)}
}

// Send models PMTA: it accepts the message and returns a stable id per key. If
// BeforeSend returns an error, the call fails and nothing is recorded. A key
// presented twice is accepted again (PMTA would dedup it) but counted as a
// duplicate so the harness can assert it never happens through the ledger path.
func (s *CountingSink) Send(_ context.Context, cmd SendCommand) (string, error) {
	if s.BeforeSend != nil {
		if err := s.BeforeSend(cmd); err != nil {
			return "", err
		}
	}
	if s.Delay > 0 {
		time.Sleep(s.Delay)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = make(map[uuid.UUID]string)
	}
	s.sends++
	if id, ok := s.seen[cmd.IdempotencyKey]; ok {
		// PMTA would dedup this; we count it so the harness can prove the ledger
		// path never reaches here.
		s.duplicateKeySends++
		return id, nil
	}
	id := "pmta-" + cmd.IdempotencyKey.String()
	s.seen[cmd.IdempotencyKey] = id
	return id, nil
}

// Sends is the total number of accepted Send calls (includes duplicates).
func (s *CountingSink) Sends() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sends
}

// DistinctKeys is the number of unique idempotency keys ever accepted.
func (s *CountingSink) DistinctKeys() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// DuplicateKeySends is the number of times Send was called for a key it had
// already accepted — the metric the no-double-send invariant pins to ZERO.
func (s *CountingSink) DuplicateKeySends() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.duplicateKeySends
}
