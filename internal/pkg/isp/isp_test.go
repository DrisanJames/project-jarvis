package isp

import "testing"

func TestGroup(t *testing.T) {
	tests := []struct {
		email string
		want  string
	}{
		{"user@gmail.com", Gmail},
		{"USER@GMAIL.COM", Gmail},
		{"user@googlemail.com", Gmail},
		{"user@yahoo.com", Yahoo},
		{"user@ymail.com", Yahoo},
		{"user@aol.com", Yahoo},
		{"user@att.net", ATT},
		{"user@outlook.com", Microsoft},
		{"user@hotmail.com", Microsoft},
		{"user@live.com", Microsoft},
		{"user@msn.com", Microsoft},
		{"user@icloud.com", Apple},
		{"user@me.com", Apple},
		{"user@mac.com", Apple},
		{"user@comcast.net", Comcast},
		{"user@xfinity.com", Comcast},
		{"user@charter.net", Charter},
		{"user@spectrum.net", Charter},
		{"user@cox.net", Cox},
		{"user@protonmail.com", Other},
		{"user@example.com", Other},
		{"invalid-no-at", Other},
		{"trailing@", Other},
		{"", Other},
	}

	for _, tt := range tests {
		got := Group(tt.email)
		if got != tt.want {
			t.Errorf("Group(%q) = %q, want %q", tt.email, got, tt.want)
		}
	}
}

func TestGroupFromDomain(t *testing.T) {
	if GroupFromDomain("Gmail.com") != Gmail {
		t.Error("expected case-insensitive match")
	}
	if GroupFromDomain(" gmail.com ") != Gmail {
		t.Error("expected trimmed match")
	}
}

func TestKnownGroups(t *testing.T) {
	groups := KnownGroups()
	if len(groups) < 6 {
		t.Errorf("expected at least 6 known groups, got %d", len(groups))
	}
}
