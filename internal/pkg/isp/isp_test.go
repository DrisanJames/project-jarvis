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
		{"yahoo.co.jp", Yahoo},

		// AOL (separated from Yahoo for dedicated pool routing)
		{"aol.com", Aol},
		{"aim.com", Aol},

		// Microsoft
		{"outlook.com", Microsoft},
		{"hotmail.com", Microsoft},
		{"live.com", Microsoft},
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
