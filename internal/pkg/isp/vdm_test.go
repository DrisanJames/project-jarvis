package isp

// Step-11 fixtures (Vector A plan rev4): the static ledger authority and the
// VDM raw-name mapping. Includes the seed-preflight negative fixture — an
// observed production string outside the authority must ABORT, never auto-add
// (permanent fixture, invariant I-11).

import "testing"

func TestLedgerGroupsAuthority(t *testing.T) {
	got := LedgerGroups()
	if len(got) != 14 {
		t.Fatalf("LedgerGroups() = %d values, want 14 (AllGroups ∪ {other}): %v", len(got), got)
	}
	set := map[string]bool{}
	for _, g := range got {
		if set[g] {
			t.Fatalf("LedgerGroups() contains duplicate %q", g)
		}
		set[g] = true
	}
	// The two values a KnownGroups-based authority would orphan, plus the
	// COALESCE fallback key.
	for _, must := range []string{Other, Verizon, Protonmail, Zoho} {
		if !set[must] {
			t.Errorf("LedgerGroups() missing %q", must)
		}
	}
}

func TestGroupFromVDMISP(t *testing.T) {
	cases := []struct {
		raw    string
		want   string
		mapped bool
	}{
		{"Gmail", Gmail, true},
		{"Yahoo", Yahoo, true},
		{"Aol", Aol, true},
		{"Hotmail", Microsoft, true},
		{"Icloud", Apple, true},
		{"Att", ATT, true},
		{"Cox", Cox, true},
		// Unknown names come back lowered and UNMAPPED — never a control key.
		{"WP", "wp", false},
		{"Outlook", "outlook", false},
	}
	for _, c := range cases {
		got, ok := GroupFromVDMISP(c.raw)
		if got != c.want || ok != c.mapped {
			t.Errorf("GroupFromVDMISP(%q) = (%q,%v), want (%q,%v)", c.raw, got, ok, c.want, c.mapped)
		}
	}
}

func TestVDMISPsDeterministic(t *testing.T) {
	got := VDMISPs()
	if len(got) != 7 {
		t.Fatalf("VDMISPs() = %d names, want 7: %v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("VDMISPs() not sorted: %v", got)
		}
	}
}

// TestSeedPreflightAbortsOnStranger pins the preflight contract: any observed
// isp_family value outside LedgerGroups() must abort the seed. 'msft' is the
// canonical stranger from the plan.
func TestSeedPreflightAbortsOnStranger(t *testing.T) {
	authority := map[string]bool{}
	for _, g := range LedgerGroups() {
		authority[g] = true
	}
	observed := []string{"gmail", "yahoo", "msft", "other"}
	var strangers []string
	for _, o := range observed {
		if !authority[o] {
			strangers = append(strangers, o)
		}
	}
	if len(strangers) != 1 || strangers[0] != "msft" {
		t.Fatalf("preflight fixture: want abort on ['msft'], got %v", strangers)
	}
}
