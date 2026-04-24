package worker

import (
	"crypto/sha1" // nolint:gosec // uuidv5 per RFC 4122 uses SHA-1 by design
	"encoding/binary"

	"github.com/google/uuid"
)

// outboxNamespace is a stable UUIDv4 picked once for this application. It is
// combined with campaign/subscriber/wave identifiers to produce deterministic
// idempotency keys via RFC-4122 v5.
var outboxNamespace = uuid.MustParse("2f9c14c6-7c4f-4d3a-93ef-8e7d4ce3a2d1")

// OutboxIdempotencyKey returns a deterministic UUIDv5 for a (campaign,
// subscriber, wave) tuple. Re-running the same wave enqueue for the same
// subscriber produces the same key, so the partial-unique index on
// mailing_campaign_queue(idempotency_key) rejects the duplicate row and the
// INSERT becomes a no-op via ON CONFLICT DO NOTHING.
//
// Callers outside a wave context (ad-hoc reprocessing, legacy code paths) can
// use OutboxIdempotencyKeyWithSlot with a unique slot to opt into the same
// guarantee without colliding with wave-driven sends.
func OutboxIdempotencyKey(campaignID, subscriberID, waveID uuid.UUID) uuid.UUID {
	return outboxIdempotencyKey(campaignID, subscriberID, waveID)
}

// outboxIdempotencyKey is the lower-case alias used inside this package.
func outboxIdempotencyKey(campaignID, subscriberID, waveID uuid.UUID) uuid.UUID {
	// uuid.NewSHA1 is the stdlib-style helper but we want a single Sum so that
	// repeated calls inside a tight loop do not allocate. The result is
	// identical to uuid.NewSHA1(outboxNamespace, data).
	data := make([]byte, 0, 48)
	data = append(data, campaignID[:]...)
	data = append(data, subscriberID[:]...)
	data = append(data, waveID[:]...)
	h := sha1.New() // nolint:gosec // RFC 4122 v5
	h.Write(outboxNamespace[:])
	h.Write(data)
	sum := h.Sum(nil)
	var k uuid.UUID
	copy(k[:], sum[:16])
	k[6] = (k[6] & 0x0f) | 0x50 // version 5
	k[8] = (k[8] & 0x3f) | 0x80 // variant RFC 4122
	return k
}

// OutboxIdempotencyKeyWithSlot lets non-wave call sites generate a
// deterministic key by mixing in an arbitrary discriminator. Typical slot
// values: "manual-trigger", "reprocess-2026-04-23", "automation-step-3".
func OutboxIdempotencyKeyWithSlot(campaignID, subscriberID uuid.UUID, slot string) uuid.UUID {
	var pad [8]byte
	binary.BigEndian.PutUint64(pad[:], uint64(len(slot)))
	data := make([]byte, 0, 32+len(slot)+8)
	data = append(data, campaignID[:]...)
	data = append(data, subscriberID[:]...)
	data = append(data, pad[:]...)
	data = append(data, []byte(slot)...)
	h := sha1.New() // nolint:gosec // RFC 4122 v5
	h.Write(outboxNamespace[:])
	h.Write(data)
	sum := h.Sum(nil)
	var k uuid.UUID
	copy(k[:], sum[:16])
	k[6] = (k[6] & 0x0f) | 0x50
	k[8] = (k[8] & 0x3f) | 0x80
	return k
}
