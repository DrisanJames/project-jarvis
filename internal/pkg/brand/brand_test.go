package brand

import "testing"

func TestRoot(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Owned brands: subdomain should collapse to the root.
		{"em.discountblog.com", "discountblog.com"},
		{"m.discountblog.com", "discountblog.com"},
		{"discountblog.com", "discountblog.com"},
		{"DISCOUNTBLOG.COM", "discountblog.com"},
		{"   em.quizfiesta.com   ", "quizfiesta.com"},
		{"em.historythinking.com", "historythinking.com"},
		{"em.myownhealth.net", "myownhealth.net"},
		{"em.getmecoupons.net", "getmecoupons.net"},

		// May 2026 brand expansion — collapse em.<apex> -> apex.
		{"em.businessweeklypro.com", "businessweeklypro.com"},
		{"em.financialcalculate.com", "financialcalculate.com"},
		{"em.consumerpro.net", "consumerpro.net"},
		{"em.homewarrantyservices.org", "homewarrantyservices.org"},
		{"em.refinanceratesusa.com", "refinanceratesusa.com"},
		{"em.thingoftheday.org", "thingoftheday.org"},
		{"em.yourinsurancehub.com", "yourinsurancehub.com"},
		{"BUSINESSWEEKLYPRO.COM", "businessweeklypro.com"},

		// Unknown/unowned: returned unchanged (lowercased/trimmed).
		{"unknown.io", "unknown.io"},
		{"EXAMPLE.COM", "example.com"},
		{"   ", ""},
		{"", ""},

		// Defensive: input that *contains* a brand name but is not a
		// suffix match must NOT collapse. "discountblog.com.attacker.io"
		// is a separate domain — never treat it as our brand.
		{"discountblog.com.attacker.io", "discountblog.com.attacker.io"},
		{"fakediscountblog.com", "fakediscountblog.com"},
	}
	for _, c := range cases {
		if got := Root(c.in); got != c.want {
			t.Errorf("Root(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRootFromEmail(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"news@em.discountblog.com", "discountblog.com"},
		{"hello@quizfiesta.com", "quizfiesta.com"},
		{"mixed@EM.HistoryThinking.COM", "historythinking.com"},
		{"malformed", ""},
		{"trailing@", ""},
		{"", ""},
		{"@leading-at", "leading-at"},
	}
	for _, c := range cases {
		if got := RootFromEmail(c.in); got != c.want {
			t.Errorf("RootFromEmail(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
