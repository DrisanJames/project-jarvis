package worker

import (
	"testing"

	"github.com/google/uuid"
)

// TestOutboxIdempotencyKey_Deterministic confirms that the same
// (campaign, subscriber, wave) tuple always produces the same UUIDv5. This is
// the property the partial unique index relies on to reject duplicate
// enqueues via ON CONFLICT DO NOTHING.
func TestOutboxIdempotencyKey_Deterministic(t *testing.T) {
	camp := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	sub := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	wave := uuid.MustParse("00000000-0000-0000-0000-000000000003")

	a := OutboxIdempotencyKey(camp, sub, wave)
	b := OutboxIdempotencyKey(camp, sub, wave)
	if a != b {
		t.Fatalf("expected deterministic key, got %s vs %s", a, b)
	}

	// Variant 5 (UUIDv5) must produce a value with version=5 and RFC4122 variant.
	if v := a.Version(); v != 5 {
		t.Fatalf("expected UUID version 5, got %d", v)
	}
	if v := a.Variant(); v != uuid.RFC4122 {
		t.Fatalf("expected RFC4122 variant, got %v", v)
	}
}

// TestOutboxIdempotencyKey_DifferentInputsDiffer guards against a hash
// collision in our composition: different campaigns, subscribers, or waves
// must produce different keys.
func TestOutboxIdempotencyKey_DifferentInputsDiffer(t *testing.T) {
	camp1 := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	camp2 := uuid.MustParse("00000000-0000-0000-0000-000000000099")
	sub := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	wave := uuid.MustParse("00000000-0000-0000-0000-000000000003")

	if OutboxIdempotencyKey(camp1, sub, wave) == OutboxIdempotencyKey(camp2, sub, wave) {
		t.Fatalf("different campaigns produced same key (collision)")
	}

	sub2 := uuid.MustParse("00000000-0000-0000-0000-000000000099")
	if OutboxIdempotencyKey(camp1, sub, wave) == OutboxIdempotencyKey(camp1, sub2, wave) {
		t.Fatalf("different subscribers produced same key (collision)")
	}

	wave2 := uuid.MustParse("00000000-0000-0000-0000-000000000099")
	if OutboxIdempotencyKey(camp1, sub, wave) == OutboxIdempotencyKey(camp1, sub, wave2) {
		t.Fatalf("different waves produced same key (collision)")
	}
}

// TestOutboxIdempotencyKey_MatchesNewSHA1 verifies that our hand-rolled
// implementation produces the exact same UUID that the stdlib helper would.
// If this ever diverges, downstream systems that re-derive the key will
// start producing duplicates instead of being deduplicated.
func TestOutboxIdempotencyKey_MatchesNewSHA1(t *testing.T) {
	camp := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	sub := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	wave := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	got := OutboxIdempotencyKey(camp, sub, wave)

	data := make([]byte, 0, 48)
	data = append(data, camp[:]...)
	data = append(data, sub[:]...)
	data = append(data, wave[:]...)
	want := uuid.NewSHA1(outboxNamespace, data)

	if got != want {
		t.Fatalf("hand-rolled hash diverged from uuid.NewSHA1\n got=%s\nwant=%s", got, want)
	}
}

// TestOutboxIdempotencyKeyWithSlot_SlotMatters confirms that different slot
// strings produce different keys, which is the invariant non-wave call sites
// rely on to avoid colliding with wave-driven sends.
func TestOutboxIdempotencyKeyWithSlot_SlotMatters(t *testing.T) {
	camp := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	sub := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	a := OutboxIdempotencyKeyWithSlot(camp, sub, "manual")
	b := OutboxIdempotencyKeyWithSlot(camp, sub, "reprocess-2026-04-23")
	if a == b {
		t.Fatalf("different slots produced same key")
	}
	c := OutboxIdempotencyKeyWithSlot(camp, sub, "manual")
	if a != c {
		t.Fatalf("same slot produced different keys: %s vs %s", a, c)
	}
}
