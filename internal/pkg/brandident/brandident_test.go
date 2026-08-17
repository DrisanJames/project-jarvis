package brandident

import (
	"strings"
	"testing"
)

// TestBrandIdentLiteralConsistency is the P0/P1 parity gate on the
// compile-time literal (plan §5.9): 27 brands, no duplicate codes, no
// duplicate apexes, everything lowercased and non-empty. The P2 shadow step
// compares the seeded TABLE against the Python registry via the
// audience-knowledge MCP; this test pins the Go side.
func TestBrandIdentLiteralConsistency(t *testing.T) {
	pairs := Canonical()
	if len(pairs) != 27 {
		t.Fatalf("canonical literal must carry exactly the 27-brand registry, got %d", len(pairs))
	}
	seenCode := make(map[string]bool, len(pairs))
	seenApex := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		if p.Code == "" || p.Apex == "" {
			t.Fatalf("empty code or apex in literal: %+v", p)
		}
		if p.Code != strings.ToLower(strings.TrimSpace(p.Code)) {
			t.Errorf("code %q is not lowercased/trimmed", p.Code)
		}
		if p.Apex != strings.ToLower(strings.TrimSpace(p.Apex)) {
			t.Errorf("apex %q is not lowercased/trimmed", p.Apex)
		}
		if seenCode[p.Code] {
			t.Errorf("duplicate brand_code %q", p.Code)
		}
		if seenApex[p.Apex] {
			t.Errorf("duplicate apex %q", p.Apex)
		}
		seenCode[p.Code] = true
		seenApex[p.Apex] = true
	}
}

// TestBrandIdentRoundTrip pins CodeForApex/ApexForCode as exact inverses
// over the whole literal, with case/space normalization on input.
func TestBrandIdentRoundTrip(t *testing.T) {
	for _, p := range Canonical() {
		apex, ok := ApexForCode(p.Code)
		if !ok || apex != p.Apex {
			t.Errorf("ApexForCode(%q) = %q, %v; want %q, true", p.Code, apex, ok, p.Apex)
		}
		code, ok := CodeForApex(p.Apex)
		if !ok || code != p.Code {
			t.Errorf("CodeForApex(%q) = %q, %v; want %q, true", p.Apex, code, ok, p.Code)
		}
	}
	// Normalization: mixed case + surrounding space resolve identically.
	if code, ok := CodeForApex("  DiscountBlog.COM "); !ok || code != "db" {
		t.Errorf("normalized CodeForApex = %q, %v; want db, true", code, ok)
	}
	if apex, ok := ApexForCode(" AAD "); !ok || apex != "aadwd.com" {
		t.Errorf("normalized ApexForCode = %q, %v; want aadwd.com, true", apex, ok)
	}
}

// TestBrandIdentMissIsExplicit pins the quarantine contract: a miss returns
// ok=false — never a guessed code, never a silent zero value that looks valid.
func TestBrandIdentMissIsExplicit(t *testing.T) {
	if code, ok := CodeForApex("em.discountblog.com"); ok {
		// The mapping is APEX-keyed; the em. sending host must NOT resolve.
		t.Errorf("CodeForApex(em.discountblog.com) must miss, got %q", code)
	}
	if _, ok := CodeForApex("not-a-brand.example"); ok {
		t.Error("unknown apex must miss")
	}
	if _, ok := ApexForCode("zz"); ok {
		t.Error("unknown code must miss")
	}
	if _, ok := CodeForApex(""); ok {
		t.Error("empty apex must miss")
	}
	if _, ok := ApexForCode(""); ok {
		t.Error("empty code must miss")
	}
}

// TestBrandIdentSeedSQL pins the seed generator: every canonical pair
// appears, and the statement is idempotent (ON CONFLICT DO NOTHING).
func TestBrandIdentSeedSQL(t *testing.T) {
	sql := SeedSQL()
	if !strings.HasPrefix(sql, "INSERT INTO mailing_brand_codes (brand_code, apex, source) VALUES ") {
		t.Fatalf("unexpected seed SQL head: %s", sql[:80])
	}
	if !strings.HasSuffix(sql, "ON CONFLICT (brand_code) DO NOTHING") {
		t.Fatal("seed SQL must be idempotent via ON CONFLICT (brand_code) DO NOTHING")
	}
	for _, p := range Canonical() {
		want := "('" + p.Code + "', '" + p.Apex + "', 'seed')"
		if !strings.Contains(sql, want) {
			t.Errorf("seed SQL missing pair %s", want)
		}
	}
	if got := strings.Count(sql, "'seed'"); got != 27 {
		t.Errorf("seed SQL must carry exactly 27 rows, got %d", got)
	}
}
