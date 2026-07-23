package moneylink

import "testing"

// The load-bearing property: a rendered creative href (concrete params) and the
// stored template ({{...}} params) reduce to the SAME key. This is what makes
// the send worker's inverse dictionary AND the Creative Studio offer-links
// reporter agree that a live href maps to its seeded hash.
func TestNormalize_RenderedAndTemplateAgree(t *testing.T) {
	template := "https://www.cratoolpro.com/BJB4Q5BF/ABC123/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}"
	rendered := "https://www.cratoolpro.com/BJB4Q5BF/ABC123/?source_id=email&sub1=sub-999&sub2=discountblog.com"
	if Normalize(template) != Normalize(rendered) {
		t.Fatalf("template and rendered href must normalize to the same key:\n template=%q\n rendered=%q",
			Normalize(template), Normalize(rendered))
	}
	if got, want := Normalize(template), "https://www.cratoolpro.com/BJB4Q5BF/ABC123"; got != want {
		t.Fatalf("Normalize = %q, want %q", got, want)
	}
}

func TestNormalize_Cases(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"strip query", "https://www.cratoolpro.com/BJB4Q5BF/ABC123/?source_id=email&sub1=123", "https://www.cratoolpro.com/BJB4Q5BF/ABC123"},
		{"strip mustache query", "https://www.cratoolpro.com/BJB4Q5BF/ABC123/?sub1={{subscriber.id}}", "https://www.cratoolpro.com/BJB4Q5BF/ABC123"},
		{"strip trailing slash", "https://www.cratoolpro.com/BJB4Q5BF/ABC123/", "https://www.cratoolpro.com/BJB4Q5BF/ABC123"},
		{"no trailing slash", "https://www.cratoolpro.com/BJB4Q5BF/ABC123", "https://www.cratoolpro.com/BJB4Q5BF/ABC123"},
		{"lowercase host, keep path case", "https://WWW.CratoolPro.COM/BJB4Q5BF/ABC123", "https://www.cratoolpro.com/BJB4Q5BF/ABC123"},
		{"strip fragment", "https://www.cratoolpro.com/BJB4Q5BF/ABC123#frag", "https://www.cratoolpro.com/BJB4Q5BF/ABC123"},
		{"whitespace trimmed", "  https://www.cratoolpro.com/BJB4Q5BF/ABC123/  ", "https://www.cratoolpro.com/BJB4Q5BF/ABC123"},
		{"http scheme lowercased", "HTTP://www.cratoolpro.com/A/B", "http://www.cratoolpro.com/A/B"},
		{"unparseable degrades to trimmed", "not a url/", "not a url"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Normalize(c.in); got != c.want {
				t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// Every one of the 6 networks must be recognized by HrefRe, capturing the full
// URL in group 1 and the path in group 2.
func TestHrefRe_AllHostsMatch(t *testing.T) {
	if len(Hosts) != 6 {
		t.Fatalf("expected 6 money hosts, got %d: %v", len(Hosts), Hosts)
	}
	re := HrefRe()
	for _, host := range Hosts {
		full := "https://www." + host + "/AA/BB"
		html := `<a href="` + full + `/?source_id=email">go</a>`
		m := re.FindStringSubmatch(html)
		if m == nil {
			t.Errorf("host %q not matched by HrefRe", host)
			continue
		}
		if m[1] != full+"/?source_id=email" {
			t.Errorf("host %q group 1 = %q, want the full URL", host, m[1])
		}
		if m[2] != "AA/BB/?source_id=email" {
			t.Errorf("host %q group 2 (path) = %q", host, m[2])
		}
	}
}

// A non-money host must never match.
func TestHrefRe_NonMoneyUntouched(t *testing.T) {
	re := HrefRe()
	for _, html := range []string{
		`<a href="https://example.com/AA/BB">x</a>`,
		`<a href="https://www.discountblog.com/reviews/auto">x</a>`,
		`<a href="https://notcratoolpro.evil.com/AA">x</a>`,
	} {
		if re.MatchString(html) {
			t.Errorf("non-money href should not match: %s", html)
		}
	}
}

// The /integration/ postback path is captured (RE2 can't express the negative
// lookahead) so callers can reject it: group 2 exposes the "integration/"
// prefix. This documents the contract the API rewriter + offer-links reporter
// rely on.
func TestHrefRe_IntegrationPathCaptured(t *testing.T) {
	re := HrefRe()
	m := re.FindStringSubmatch(`<a href="https://www.cratoolpro.com/integration/postback">x</a>`)
	if m == nil {
		t.Fatal("integration href should still match the regex (exclusion is in caller code)")
	}
	if m[2] != "integration/postback" {
		t.Fatalf("group 2 = %q, want integration/postback so callers can exclude it", m[2])
	}
}
