// Package prefilltoken mints and verifies the signed, short-lived tokens that
// key the journey prefill API (GET /api/mailing/journey/prefill?token=...).
//
// PROVIDER-APPROVED (operator 2026-07-27, supersedes the same-day hold):
// "The provider is okay if we take the data we have on file and
// pre-populate." The flag JOURNEY_PREFILL_ENABLED therefore defaults ON
// (set "false" to disarm). The compliant shape STANDS regardless:
// convenience + previously-typed fields prefill; the consent step is NEVER
// prefilled; and every prefilled field carries PROVENANCE
// (prefilled_from_file | restored_from_session | consumer_typed |
// consumer_edited) in the funnel's session record and lead payload — the
// audit trail is the answer to the cert concern, documented rather than
// masked.
//
// Token shape (mirrors the tracking layer's TrackSign scheme —
// internal/worker/send_worker.go TrackSign: HMAC-SHA256, hex, first 16):
//
//	base64url(subscriberID|expiryUnix) + "." + hex(hmac_sha256(payload))[:16]
//
// A bare subscriber UUID is NEVER accepted — every lookup requires a valid
// signature and an unexpired timestamp (PII-harvesting hole otherwise; pinned
// by TestPrefillRejectsBareUUID).
package prefilltoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DefaultTTL is the token lifetime: short by design (24h from mint/click).
const DefaultTTL = 24 * time.Hour

// Enabled reports whether the prefill surface is armed. Default ON
// (provider-approved 2026-07-27 — see the package comment); the exact string
// "false" disarms it.
func Enabled() bool {
	return os.Getenv("JOURNEY_PREFILL_ENABLED") != "false"
}

// SecretFromEnv returns the shared HMAC secret. Same env + dev default as the
// send worker's tracking config (cmd/server/main.go:541), so tokens minted at
// send time verify at the API without new key plumbing.
func SecretFromEnv() string {
	if v := os.Getenv("TRACKING_SECRET"); v != "" {
		return v
	}
	return "ignite-tracking-secret-dev"
}

func sign(payload, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(payload))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Mint produces a signed token for subscriberID expiring ttl from now.
// ttl == 0 uses DefaultTTL (a negative ttl mints an already-expired token —
// deliberate, for expiry tests).
func Mint(subscriberID string, ttl time.Duration, secret string) string {
	if ttl == 0 {
		ttl = DefaultTTL
	}
	payload := fmt.Sprintf("%s|%d", subscriberID, time.Now().Add(ttl).Unix())
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sign(payload, secret)
}

// Verify checks signature + expiry and returns the subscriber id. Every
// failure mode returns an error — callers respond 404, never differentiating
// (no oracle for token probing).
func Verify(token, secret string) (string, error) {
	parts := strings.SplitN(strings.TrimSpace(token), ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("malformed token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("bad payload encoding")
	}
	payload := string(raw)
	if !hmac.Equal([]byte(sign(payload, secret)), []byte(parts[1])) {
		return "", fmt.Errorf("bad signature")
	}
	fields := strings.SplitN(payload, "|", 2)
	if len(fields) != 2 {
		return "", fmt.Errorf("bad payload shape")
	}
	exp, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", fmt.Errorf("expired")
	}
	if _, err := uuid.Parse(fields[0]); err != nil {
		return "", fmt.Errorf("bad subscriber id")
	}
	return fields[0], nil
}
