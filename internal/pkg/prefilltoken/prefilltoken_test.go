package prefilltoken

import (
	"strings"
	"testing"
	"time"
)

const subID = "2e578331-542e-472e-aa6e-71b8deafecf9"
const secret = "test-secret"

func TestMintVerifyRoundTrip(t *testing.T) {
	tok := Mint(subID, time.Hour, secret)
	got, err := Verify(tok, secret)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != subID {
		t.Fatalf("subscriber id = %q, want %q", got, subID)
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	tok := Mint(subID, -time.Minute, secret)
	if _, err := Verify(tok, secret); err == nil {
		t.Fatal("expired token verified")
	}
}

func TestTamperedSignatureRejected(t *testing.T) {
	tok := Mint(subID, time.Hour, secret)
	if _, err := Verify(tok[:len(tok)-4]+"beef", secret); err == nil {
		t.Fatal("tampered token verified")
	}
}

func TestWrongSecretRejected(t *testing.T) {
	tok := Mint(subID, time.Hour, secret)
	if _, err := Verify(tok, "other-secret"); err == nil {
		t.Fatal("token verified under the wrong secret")
	}
}

func TestBareUUIDRejected(t *testing.T) {
	// The PII-harvesting pin: a raw subscriber uuid is not a credential.
	if _, err := Verify(subID, secret); err == nil {
		t.Fatal("bare uuid accepted as a token")
	}
}

func TestPayloadSwapRejected(t *testing.T) {
	// Signature from one subscriber must not authorize another.
	tok1 := Mint(subID, time.Hour, secret)
	tok2 := Mint("11111111-2222-3333-4444-555555555555", time.Hour, secret)
	frank := strings.SplitN(tok2, ".", 2)[0] + "." + strings.SplitN(tok1, ".", 2)[1]
	if _, err := Verify(frank, secret); err == nil {
		t.Fatal("payload-swapped token verified")
	}
}

func TestEnabledDefaultOn(t *testing.T) {
	// Provider-approved 2026-07-27: prefill defaults ON; "false" disarms.
	t.Setenv("JOURNEY_PREFILL_ENABLED", "")
	if !Enabled() {
		t.Fatal("prefill must default ON (provider-approved)")
	}
	t.Setenv("JOURNEY_PREFILL_ENABLED", "true")
	if !Enabled() {
		t.Fatal("armed flag not detected")
	}
	t.Setenv("JOURNEY_PREFILL_ENABLED", "false")
	if Enabled() {
		t.Fatal("explicit \"false\" must disarm prefill")
	}
}
