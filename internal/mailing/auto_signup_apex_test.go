package mailing

import "testing"

// The auto-insurance drip discloses where the recipient actually signed up:
//
//	"You requested auto insurance quotes through <signup_url>"
//
// custom.signup_url is 100% populated on every auto lane, but its VALUES are
// raw and inconsistent — some carry a protocol, some a www, some a full path,
// some a trailing slash, some are a bare host, some a subdomain. Rendering the
// field as-is puts a URL in a disclosure sentence; hardcoding one partner is
// worse (measured 2026-08-20: savemaxauto.com is correct for 120 of ~5,110
// db-lane records, so a hardcode misnames the source for ~98% of readers).
//
// This pins the apex extraction across every real-world form observed in prod.
func TestAutoSignupApexRendersCleanly(t *testing.T) {
	ts := NewTemplateService()

	const apex = `{% assign su = custom.signup_url | default: "" %}` +
		`{% assign su = su | remove: "https://" | remove: "http://" | remove: "www." %}` +
		// NOTE: `split | first` on an empty string yields NIL, which slips past a
		// bare `!= ""` guard and renders an empty disclosure. Re-default after
		// each split so su is always a string.
		`{% assign su = su | split: "/" | first | default: "" %}` +
		`{% assign su = su | split: "?" | first | default: "" %}` +
		`{% if su != "" %}{{ su }}{% else %}one of our partner quote sites{% endif %}`

	cases := []struct{ in, want string }{
		// Real values pulled from prod 2026-08-20.
		{"https://www.bestmoney.com/car-insurance/compare", "bestmoney.com"},
		{"auto.everquote.com", "auto.everquote.com"},
		{"https://v2.sparkautoinsurance.com", "v2.sparkautoinsurance.com"},
		{"https://gettinsured.com", "gettinsured.com"},
		{"https://rapidinsurancequotes.com/", "rapidinsurancequotes.com"},
		{"insure.com", "insure.com"},
		{"www.financebuzz.com", "financebuzz.com"},
		{"https://savemaxauto.com", "savemaxauto.com"},
		// Shapes the feed could plausibly send.
		{"http://www.example.com/a/b?utm=1", "example.com"},
		{"https://example.com?ref=x", "example.com"},
		{"", "one of our partner quote sites"},
	}
	for _, c := range cases {
		ctx := map[string]interface{}{"custom": map[string]interface{}{"signup_url": c.in}}
		got, err := ts.Render("", apex, ctx)
		if err != nil {
			t.Fatalf("render %q: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("signup_url %q -> %q, want %q", c.in, got, c.want)
		}
	}
	// The key is genuinely ABSENT (not merely empty) — the case that breaks
	// naive guards. Must fall back, never render empty into the sentence.
	got, err := ts.Render("", apex, map[string]interface{}{"custom": map[string]interface{}{}})
	if err != nil {
		t.Fatalf("render missing-key: %v", err)
	}
	if got != "one of our partner quote sites" {
		t.Errorf("missing signup_url -> %q, want the fallback phrase", got)
	}
}
