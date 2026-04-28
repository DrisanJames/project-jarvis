package mailing

import "testing"

func TestClassifyEmailForIngest(t *testing.T) {
	tests := []struct {
		name       string
		email      string
		wantAccept bool
		wantReason string
	}{
		// Happy path — legit mailbox providers
		{"gmail accept", "user@gmail.com", true, ""},
		{"hotmail accept", "person@hotmail.com", true, ""},
		{"yahoo accept", "someone@yahoo.com", true, ""},
		{"outlook accept", "name@outlook.com", true, ""},
		{"custom domain accept", "jane@mycompany.io", true, ""},
		{"icloud accept", "user@icloud.com", true, ""},
		{"me.com accept", "user@me.com", true, ""},
		{"comcast accept", "subscriber@comcast.net", true, ""},
		{"uppercase accept", "User@GMAIL.COM", true, ""},
		{"whitespace accept", "  user@gmail.com  ", true, ""},

		// Typo traps — must reject
		{"gmai.com typo", "user@gmai.com", false, "typo_trap_domain"},
		{"hotmial.com typo", "user@hotmial.com", false, "typo_trap_domain"},
		{"yaho.com typo", "user@yaho.com", false, "typo_trap_domain"},
		{"gmail.co typo", "user@gmail.co", false, "typo_trap_domain"},
		{"outlok.com typo", "user@outlok.com", false, "typo_trap_domain"},
		{"gnail.com typo", "person@gnail.com", false, "typo_trap_domain"},
		{"iclud.com typo", "user@iclud.com", false, "typo_trap_domain"},
		{"case-insensitive typo", "User@GMAI.COM", false, "typo_trap_domain"},

		// Disposable — must reject
		{"mailinator", "anything@mailinator.com", false, "disposable_domain"},
		{"guerrillamail", "foo@guerrillamail.com", false, "disposable_domain"},
		{"10minutemail", "x@10minutemail.com", false, "disposable_domain"},
		{"yopmail", "bar@yopmail.com", false, "disposable_domain"},
		{"temp-mail.io", "baz@temp-mail.io", false, "disposable_domain"},
		{"getnada", "qux@getnada.com", false, "disposable_domain"},

		// Role-based — must reject even on legit domains
		{"abuse@gmail", "abuse@gmail.com", false, "role_based_local_part"},
		{"postmaster", "postmaster@mycompany.io", false, "role_based_local_part"},
		{"admin", "admin@example.com", false, "role_based_local_part"},
		{"noreply", "noreply@example.com", false, "role_based_local_part"},
		{"no-reply", "no-reply@example.com", false, "role_based_local_part"},
		{"info", "info@example.com", false, "role_based_local_part"},
		{"webmaster", "webmaster@example.com", false, "role_based_local_part"},
		{"support", "support@example.com", false, "role_based_local_part"},

		// Malformed — must reject
		{"no at sign", "notanemail", false, "invalid_format"},
		{"leading at", "@gmail.com", false, "invalid_format"},
		{"trailing at", "user@", false, "invalid_format"},
		{"empty", "", false, "invalid_format"},
		{"only at", "@", false, "invalid_format"},
		{"only whitespace", "   ", false, "invalid_format"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyEmailForIngest(tc.email)
			if got.Accept != tc.wantAccept {
				t.Errorf("ClassifyEmailForIngest(%q) Accept=%v, want %v (reason=%q, category=%q)",
					tc.email, got.Accept, tc.wantAccept, got.Reason, got.Category)
			}
			if !got.Accept && got.Reason != tc.wantReason {
				t.Errorf("ClassifyEmailForIngest(%q) Reason=%q, want %q",
					tc.email, got.Reason, tc.wantReason)
			}
		})
	}
}

func TestIsTypoTrapDomain(t *testing.T) {
	if !IsTypoTrapDomain("gmai.com") {
		t.Error("gmai.com should be a typo trap")
	}
	if IsTypoTrapDomain("gmail.com") {
		t.Error("gmail.com should NOT be a typo trap")
	}
	if !IsTypoTrapDomain("GMAI.COM") {
		t.Error("uppercase typo-trap should match")
	}
}

func TestIsDisposableDomain(t *testing.T) {
	if !IsDisposableDomain("mailinator.com") {
		t.Error("mailinator.com should be disposable")
	}
	if IsDisposableDomain("gmail.com") {
		t.Error("gmail.com should NOT be disposable")
	}
}

func TestIsRoleBasedLocalPart(t *testing.T) {
	for _, lp := range []string{"abuse", "postmaster", "noreply", "admin", "info"} {
		if !IsRoleBasedLocalPart(lp) {
			t.Errorf("%q should be role-based", lp)
		}
	}
	for _, lp := range []string{"john", "jane.doe", "j.smith"} {
		if IsRoleBasedLocalPart(lp) {
			t.Errorf("%q should NOT be role-based", lp)
		}
	}
}

// Legitimate hotmail/yahoo ccTLD domains should NOT be flagged as typo traps.
// This is a guard against over-aggressive blocking.
func TestLegitCcTLDsNotBlocked(t *testing.T) {
	legit := []string{
		"user@hotmail.co.uk",
		"user@yahoo.co.uk",
		"user@yahoo.co.jp",
		"user@gmail.co.uk",
		"user@btinternet.com",
		"user@orange.fr",
	}
	for _, e := range legit {
		d := ClassifyEmailForIngest(e)
		if !d.Accept {
			t.Errorf("legit address %q was rejected: reason=%q category=%q",
				e, d.Reason, d.Category)
		}
	}
}
