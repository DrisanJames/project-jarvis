package preferences

import (
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	tOrg    = "00000000-0000-0000-0000-000000000001"
	tSub    = "55555555-5555-5555-5555-555555555555"
	tSecret = "test-secret"
)

var tNow = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

func TestToken_SignVerifyRoundTrip(t *testing.T) {
	for _, scope := range []Scope{ScopeBrand, ScopeAll} {
		tok := Mint(tOrg, tSub, "DiscountBlog.com", scope, tNow, tSecret)
		if tok == "" {
			t.Fatalf("scope=%s: empty token", scope)
		}
		c, err := Verify(tok, tSecret, tNow.Add(time.Hour))
		if err != nil {
			t.Fatalf("scope=%s verify: %v", scope, err)
		}
		if c.OrgID != tOrg || c.SubscriberID != tSub || c.BrandRoot != "discountblog.com" || c.Scope != scope {
			t.Fatalf("scope=%s claims wrong: %+v", scope, c)
		}
		if !c.Exp.Equal(tNow.Add(TokenTTL).Truncate(time.Second)) {
			t.Fatalf("exp = %v, want now+180d", c.Exp)
		}
		// full-length HMAC-SHA256 hex
		if sig := tok[strings.LastIndexByte(tok, '.')+1:]; len(sig) != 64 {
			t.Fatalf("signature length %d, want 64", len(sig))
		}
	}
}

func TestToken_ExpiryAndGrace(t *testing.T) {
	tok := Mint(tOrg, tSub, "discountblog.com", ScopeBrand, tNow, tSecret)
	// Inside the TTL, and inside the grace window past exp: valid.
	for _, at := range []time.Time{tNow.Add(TokenTTL - time.Hour), tNow.Add(TokenTTL + ExpiryGrace - time.Hour)} {
		if _, err := Verify(tok, tSecret, at); err != nil {
			t.Fatalf("at %v: %v", at, err)
		}
	}
	// Past exp+grace: ErrExpired, but claims come back (signature was valid).
	c, err := Verify(tok, tSecret, tNow.Add(TokenTTL+ExpiryGrace+time.Hour))
	if err != ErrExpired {
		t.Fatalf("want ErrExpired, got %v", err)
	}
	if c.SubscriberID != tSub || c.BrandRoot != "discountblog.com" {
		t.Fatalf("expired token must still return claims: %+v", c)
	}
}

func TestToken_TamperRejected(t *testing.T) {
	tok := Mint(tOrg, tSub, "discountblog.com", ScopeBrand, tNow, tSecret)
	dot := strings.LastIndexByte(tok, '.')
	raw, _ := base64.RawURLEncoding.DecodeString(tok[:dot])

	// Widen scope brand -> all, keep the original signature.
	widened := strings.Replace(string(raw), "|brand|", "|all|", 1)
	forged := base64.RawURLEncoding.EncodeToString([]byte(widened)) + tok[dot:]
	if _, err := Verify(forged, tSecret, tNow); err != ErrSignature {
		t.Fatalf("scope-widened token: want ErrSignature, got %v", err)
	}
	// Swap the subscriber.
	other := strings.Replace(string(raw), tSub, "66666666-6666-6666-6666-666666666666", 1)
	if _, err := Verify(base64.RawURLEncoding.EncodeToString([]byte(other))+tok[dot:], tSecret, tNow); err != ErrSignature {
		t.Fatalf("subscriber-swapped token: want ErrSignature, got %v", err)
	}
	// Flip one signature char.
	b := []byte(tok)
	if b[len(b)-1] == 'a' {
		b[len(b)-1] = 'b'
	} else {
		b[len(b)-1] = 'a'
	}
	if _, err := Verify(string(b), tSecret, tNow); err != ErrSignature {
		t.Fatalf("sig-flipped token: want ErrSignature, got %v", err)
	}
	// Wrong secret.
	if _, err := Verify(tok, "other-secret", tNow); err != ErrSignature {
		t.Fatalf("wrong secret: want ErrSignature, got %v", err)
	}
	// No secret configured never verifies.
	if _, err := Verify(tok, "", tNow); err != ErrNoSecret {
		t.Fatalf("empty secret: want ErrNoSecret, got %v", err)
	}
	if Mint(tOrg, tSub, "discountblog.com", ScopeBrand, tNow, "") != "" {
		t.Fatal("Mint with empty secret must return empty token")
	}
}

func TestToken_ScopeValidation(t *testing.T) {
	if Mint(tOrg, tSub, "", ScopeBrand, tNow, tSecret) != "" {
		t.Fatal("brand scope without a brand root must not mint")
	}
	if Mint(tOrg, tSub, "", ScopeAll, tNow, tSecret) == "" {
		t.Fatal("all scope without a brand root must mint")
	}
	if Mint(tOrg, tSub, "x.com", Scope("everything"), tNow, tSecret) != "" {
		t.Fatal("unknown scope must not mint")
	}
	if Mint(tOrg, tSub, "a.com|all", ScopeBrand, tNow, tSecret) != "" {
		t.Fatal("brand root containing the field separator must not mint")
	}
	if Mint(tOrg, "not-a-uuid", "x.com", ScopeBrand, tNow, tSecret) != "" {
		t.Fatal("non-uuid subscriber must not mint")
	}
}

// The legacy context_builder token was md5(sid|cid|key)[:16]. It must never
// verify as a v1 token.
func TestToken_LegacyMD5Rejected(t *testing.T) {
	legacy := fmt.Sprintf("%x", md5.Sum([]byte(tSub+"|cid|"+tSecret)))[:16]
	if _, err := Verify(legacy, tSecret, tNow); err == nil {
		t.Fatalf("legacy MD5 token %q verified", legacy)
	}
}

func TestNonce_BoundToTokenAndWindow(t *testing.T) {
	tok := Mint(tOrg, tSub, "discountblog.com", ScopeBrand, tNow, tSecret)
	n := Nonce(tok, tSecret, tNow)
	if !VerifyNonce(tok, n, tSecret, tNow.Add(59*time.Minute)) {
		t.Fatal("nonce must verify within the window")
	}
	if VerifyNonce(tok, n, tSecret, tNow.Add(3*time.Hour)) {
		t.Fatal("nonce must expire after the window")
	}
	other := Mint(tOrg, tSub, "discountblog.com", ScopeAll, tNow, tSecret)
	if VerifyNonce(other, n, tSecret, tNow) {
		t.Fatal("nonce must be bound to its token")
	}
	if VerifyNonce(tok, "", tSecret, tNow) {
		t.Fatal("empty nonce must fail")
	}
}

func TestEmailHash_Canonical(t *testing.T) {
	if EmailHash("  Human@Example.COM ") != EmailHash("human@example.com") {
		t.Fatal("hash must be over lower(trim(email))")
	}
	if len(EmailHash("a@b.c")) != 64 {
		t.Fatal("sha256 hex expected")
	}
}
