package isp

import "testing"

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
