//go:build repro

package preferences

// Mints one v1 preferences token for a read-only live page check (operator
// approved 2026-09-11: seed subscriber, GET only). Never runs in CI.
//   TRACKING_SECRET=… PREF_ORG=… PREF_SUB=… PREF_BRAND=… PREF_OUT=path \
//     go test -tags repro -run TestMintForLiveCheck ./internal/preferences/
// The token is written to PREF_OUT (0600), never printed.

import (
	"os"
	"testing"
	"time"
)

func TestMintForLiveCheck(t *testing.T) {
	secret, out := os.Getenv("TRACKING_SECRET"), os.Getenv("PREF_OUT")
	if secret == "" || out == "" {
		t.Fatal("TRACKING_SECRET and PREF_OUT are required")
	}
	now := time.Now()
	tok := Mint(os.Getenv("PREF_ORG"), os.Getenv("PREF_SUB"), os.Getenv("PREF_BRAND"), ScopeBrand, now, secret)
	if _, err := Verify(tok, secret, now); err != nil {
		t.Fatalf("minted token does not verify locally: %v", err)
	}
	if err := os.WriteFile(out, []byte(tok), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("token minted and verified locally (%d chars) → %s", len(tok), out)
}
