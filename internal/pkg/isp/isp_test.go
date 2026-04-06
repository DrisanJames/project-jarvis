package isp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestGroupFromDomain(t *testing.T) {
	tests := []struct {
		domain string
		want   string
	}{
		// Gmail
		{"gmail.com", Gmail},
		{"googlemail.com", Gmail},
		{"GMAIL.COM", Gmail},

		// Yahoo
		{"yahoo.com", Yahoo},
		{"ymail.com", Yahoo},
		{"rocketmail.com", Yahoo},
		{"myyahoo.com", Yahoo},
		{"yahoo.ca", Yahoo},
		{"yahoo.co.uk", Yahoo},
		{"yahoo.co.in", Yahoo},
		{"yahoo.com.au", Yahoo},
		{"yahoo.co.jp", Yahoo},

		// AOL (separated from Yahoo for dedicated pool routing)
		{"aol.com", Aol},
		{"aim.com", Aol},

		// Microsoft
		{"outlook.com", Microsoft},
		{"outlook.co.uk", Microsoft},
		{"hotmail.com", Microsoft},
		{"hotmail.co.uk", Microsoft},
		{"hotmail.fr", Microsoft},
		{"live.com", Microsoft},
		{"live.co.uk", Microsoft},
		{"msn.com", Microsoft},

		// Apple
		{"icloud.com", Apple},
		{"me.com", Apple},
		{"mac.com", Apple},

		// AT&T
		{"att.net", ATT},

		// SBC Global / BellSouth (separated from AT&T for dedicated pool routing)
		{"sbcglobal.net", Sbcglobal},
		{"bellsouth.net", Sbcglobal},
		{"pacbell.net", Sbcglobal},
		{"swbell.net", Sbcglobal},
		{"ameritech.net", Sbcglobal},
		{"nvbell.net", Sbcglobal},

		// Comcast
		{"comcast.net", Comcast},
		{"xfinity.com", Comcast},

		// Charter / Spectrum / rr.com variants
		{"charter.net", Charter},
		{"spectrum.net", Charter},
		{"rr.com", Charter},
		{"roadrunner.com", Charter},
		{"twc.com", Charter},
		{"brighthouse.com", Charter},
		{"nyc.rr.com", Charter},
		{"austin.rr.com", Charter},
		{"socal.rr.com", Charter},

		// Cox
		{"cox.net", Cox},

		// Verizon
		{"verizon.net", Verizon},

		// Protonmail
		{"protonmail.com", Protonmail},
		{"proton.me", Protonmail},

		// Zoho
		{"zoho.com", Zoho},

		// Other / unknown
		{"example.com", Other},
		{"company.org", Other},
		{"", Other},
	}

	for _, tt := range tests {
		t.Run(tt.domain, func(t *testing.T) {
			got := GroupFromDomain(tt.domain)
			if got != tt.want {
				t.Errorf("GroupFromDomain(%q) = %q, want %q", tt.domain, got, tt.want)
			}
		})
	}
}

func TestGroup(t *testing.T) {
	tests := []struct {
		email string
		want  string
	}{
		{"user@gmail.com", Gmail},
		{"user@Yahoo.Com", Yahoo},
		{"user@nyc.rr.com", Charter},
		{"user@outlook.com", Microsoft},
		{"noatsign", Other},
		{"trailing@", Other},
		{"", Other},
	}

	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			got := Group(tt.email)
			if got != tt.want {
				t.Errorf("Group(%q) = %q, want %q", tt.email, got, tt.want)
			}
		})
	}
}

func TestPoolSuffix(t *testing.T) {
	tests := []struct {
		isp  string
		want string
	}{
		{Gmail, "gmail"},
		{Yahoo, "yahoo"},
		{Aol, "aol"},
		{Microsoft, "msft"},
		{Apple, "apple"},
		{Comcast, "comcast"},
		{ATT, "att"},
		{Sbcglobal, "sbcglobal"},
		{Cox, "cox"},
		{Charter, "charter"},
		{Verizon, "general"},
		{Protonmail, "general"},
		{Zoho, "general"},
		{Other, "general"},
		{"unknown", "general"},
	}

	for _, tt := range tests {
		t.Run(tt.isp, func(t *testing.T) {
			got := PoolSuffix(tt.isp)
			if got != tt.want {
				t.Errorf("PoolSuffix(%q) = %q, want %q", tt.isp, got, tt.want)
			}
		})
	}
}

func TestKnownGroups(t *testing.T) {
	groups := KnownGroups()
	if len(groups) != 10 {
		t.Errorf("KnownGroups() returned %d groups, want 10", len(groups))
	}
}

func TestAllGroups(t *testing.T) {
	groups := AllGroups()
	if len(groups) != 13 {
		t.Errorf("AllGroups() returned %d groups, want 13", len(groups))
	}
}

func TestDomainsForGroup(t *testing.T) {
	yahDomains := DomainsForGroup(Yahoo)
	if len(yahDomains) == 0 {
		t.Fatal("DomainsForGroup(Yahoo) returned empty")
	}
	found := false
	for _, d := range yahDomains {
		if d == "myyahoo.com" {
			found = true
		}
	}
	if !found {
		t.Error("DomainsForGroup(Yahoo) missing myyahoo.com")
	}

	sbcDomains := DomainsForGroup(Sbcglobal)
	wantSBC := map[string]bool{"sbcglobal.net": true, "bellsouth.net": true, "pacbell.net": true, "swbell.net": true, "ameritech.net": true, "nvbell.net": true}
	for d := range wantSBC {
		found := false
		for _, sd := range sbcDomains {
			if sd == d {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DomainsForGroup(Sbcglobal) missing %q", d)
		}
	}
}

func TestDomainSiblings_MicrosoftUnified(t *testing.T) {
	siblings, ok := DomainSiblings["@outlook.com"]
	if !ok {
		t.Fatal("@outlook.com not in DomainSiblings")
	}
	want := map[string]bool{
		"@outlook.com": true, "@outlook.co.uk": true,
		"@hotmail.com": true, "@hotmail.co.uk": true, "@hotmail.fr": true,
		"@live.com": true, "@live.co.uk": true, "@msn.com": true,
	}
	if len(siblings) != len(want) {
		t.Errorf("@outlook.com siblings count = %d, want %d", len(siblings), len(want))
	}
	for _, s := range siblings {
		if !want[s] {
			t.Errorf("unexpected sibling %q for @outlook.com", s)
		}
	}
	// Hotmail must be in the same group (unified Microsoft)
	hotmailSiblings, ok := DomainSiblings["@hotmail.com"]
	if !ok {
		t.Fatal("@hotmail.com not in DomainSiblings")
	}
	if len(hotmailSiblings) != len(siblings) {
		t.Errorf("@hotmail.com siblings count = %d, want %d (same as @outlook.com)", len(hotmailSiblings), len(siblings))
	}
}

func TestDomainSiblings_EveryDomainPresent(t *testing.T) {
	if len(DomainSiblings) == 0 {
		t.Fatal("DomainSiblings is empty")
	}
	for key, siblings := range DomainSiblings {
		if len(siblings) == 0 {
			t.Errorf("DomainSiblings[%q] has no siblings", key)
		}
		found := false
		for _, s := range siblings {
			if s == key {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DomainSiblings[%q] does not contain itself", key)
		}
	}
}

func TestExportCanonicalMapJSON(t *testing.T) {
	// Export the canonical domainToISP map as JSON to testdata/ for cross-language
	// consistency checks. CI or a developer can diff this against daily_acquisition.py's
	// DOMAIN_TO_ISP to detect drift.
	data := make(map[string]string, len(domainToISP))
	for domain, group := range domainToISP {
		data[domain] = group
	}

	jsonBytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Fatalf("failed to marshal canonical map: %v", err)
	}

	outDir := filepath.Join("testdata")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		t.Fatalf("failed to create testdata dir: %v", err)
	}

	outPath := filepath.Join(outDir, "canonical_isp_map.json")
	if err := os.WriteFile(outPath, jsonBytes, 0644); err != nil {
		t.Fatalf("failed to write canonical map: %v", err)
	}

	t.Logf("Wrote %d domain mappings to %s", len(data), outPath)

	// Verify key domains are present
	if data["myyahoo.com"] != Yahoo {
		t.Error("canonical map missing myyahoo.com -> yahoo")
	}
	if data["pacbell.net"] != Sbcglobal {
		t.Error("canonical map missing pacbell.net -> sbcglobal")
	}
}
