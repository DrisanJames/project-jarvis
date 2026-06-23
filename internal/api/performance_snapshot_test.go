package api

import "testing"

// TestSnapshotSlugFromLinkURL covers anchored money-slug extraction across the
// networks the platform sends to. The slug is the path segment AFTER the
// account id; a non-money / unrecognisable URL yields "".
func TestSnapshotSlugFromLinkURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"cratoolpro no-scheme + query", "cratoolpro.com/BJB4Q5BF/93W8N2N/?x", "93W8N2N"},
		{"cratoolpro https www", "https://www.cratoolpro.com/BJB4Q5BF/93W8N2N/?source_id=email", "93W8N2N"},
		{"eos57ytf alnum slug", "https://www.eos57ytf.com/K4C5ZLC/2HH43PB/?source_id=email", "2HH43PB"},
		{"eos57ytf PS slug", "https://www.eos57ytf.com/K8K0HFDT/PS8241/?creative_id=4989", "PS8241"},
		{"k8k0hfdt host", "https://k8k0hfdt.com/AB12CD3/K86F3PC/", "K86F3PC"},
		{"xnonu host", "https://www.xnonu.com/TQ5MX18J/XF1SR2CS/?sub1=42", "XF1SR2CS"},
		{"empty", "", ""},
		{"non-money tracking pixel", "https://t.em.discountblog.com/track/open?id=abc", ""},
		{"unsubscribe link (no second segment)", "https://em.quizfiesta.com/u", ""},
		{"plain domain root", "https://example.com/", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := slugFromLinkURL(c.url); got != c.want {
				t.Fatalf("slugFromLinkURL(%q) = %q, want %q", c.url, got, c.want)
			}
		})
	}
}

// TestSnapshotNormalizeSendingDomain covers the em./m. prefix-stripping to a
// brand root, matching the acq Python reporters' root().
func TestSnapshotNormalizeSendingDomain(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"em.discountblog.com", "discountblog.com"},
		{"EM.HistoryThinking.com", "historythinking.com"},
		{"m.refinanceratesusa.com", "refinanceratesusa.com"},
		{"myownhealth.net", "myownhealth.net"}, // no prefix
		{"  em.quizfiesta.com  ", "quizfiesta.com"},
		{"", "(none)"},
	}
	for _, c := range cases {
		if got := normalizeSendingDomain(c.in); got != c.want {
			t.Errorf("normalizeSendingDomain(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSnapshotPct1 verifies the one-decimal percentage helper used for every
// rate field (e.g. delivery_pct 90.8).
func TestSnapshotPct1(t *testing.T) {
	cases := []struct {
		n, d int64
		want float64
	}{
		{908, 1000, 90.8},
		{0, 0, 0}, // zero denom -> 0, never NaN
		{5, 0, 0}, // zero denom guard
		{1, 3, 33.3},
		{2, 3, 66.7}, // rounds half up
	}
	for _, c := range cases {
		if got := pct1(c.n, c.d); got != c.want {
			t.Errorf("pct1(%d,%d) = %v, want %v", c.n, c.d, got, c.want)
		}
	}
}

// TestSnapshotDominantOffer covers the volume-follow tie-break: a campaign's
// volume tracks the offer its clicks predominantly landed on. Most clicks wins;
// ties resolve to the smallest first-seen order (first offer encountered).
func TestSnapshotDominantOffer(t *testing.T) {
	cases := []struct {
		name      string
		cnt       map[string]int
		seenOrder map[string]int
		want      string
	}{
		{"empty", map[string]int{}, map[string]int{}, ""},
		{"single", map[string]int{"A": 3}, map[string]int{"A": 0}, "A"},
		{
			"clear majority",
			map[string]int{"A": 5, "B": 2},
			map[string]int{"A": 1, "B": 0},
			"A", // A wins on count even though B was seen first
		},
		{
			"tie -> first seen",
			map[string]int{"A": 2, "B": 2},
			map[string]int{"A": 1, "B": 0},
			"B", // equal counts -> smaller seen order
		},
		{
			"tie -> first seen (reversed order)",
			map[string]int{"A": 2, "B": 2},
			map[string]int{"A": 0, "B": 1},
			"A",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dominantOffer(c.cnt, c.seenOrder); got != c.want {
				t.Fatalf("dominantOffer(%v,%v) = %q, want %q", c.cnt, c.seenOrder, got, c.want)
			}
		})
	}
}

// TestSnapshotNameSuffixOffer covers the operator " - " naming-convention parse.
func TestSnapshotNameSuffixOffer(t *testing.T) {
	cases := []struct{ in, want string }{
		{"DB Engager AM - Quicken Loans", "Quicken Loans"},
		{"QF Welcome - 2026 Personal Loans", "2026 Personal Loans"},
		{"NoSuffixCampaign", ""},
		{"trailing - ", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := nameSuffixOffer(c.in); got != c.want {
			t.Errorf("nameSuffixOffer(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
