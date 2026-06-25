package worker

import (
	"strings"
	"testing"
)

// TestApexFromSendingDomain guards the prefix-strip that drives brand image-host
// matching. The send path produced img.projectjarvis.io on m.ratesbazar.com
// because the prior helper only stripped "em." — this is the regression the bug
// fix targets, so m./em. (and the other ESP prefixes) MUST all reduce to the apex.
func TestApexFromSendingDomain(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"m.ratesbazar.com", "ratesbazar.com"},    // the SES relay host that regressed
		{"em.ratesbazar.com", "ratesbazar.com"},   // dedicated host
		{"em.discountblog.com", "discountblog.com"},
		{"mail.example.com", "example.com"},
		{"send.example.com", "example.com"},
		{"trk.example.com", "example.com"},
		{"ratesbazar.com", "ratesbazar.com"},      // bare apex unchanged
		{"M.RatesBazar.com", "ratesbazar.com"},    // case-insensitive
		{" em.ratesbazar.com ", "ratesbazar.com"}, // trimmed
		{"", ""},
	}
	for _, c := range cases {
		if got := apexFromSendingDomain(c.in); got != c.want {
			t.Errorf("apexFromSendingDomain(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestImageHostSwapPurity asserts the swap (as applied in both send funnels)
// rewrites ONLY the neutral image host and leaves money/tracking/beacon URLs
// intact — mirrors the production dry-run on the live ratesbazar creative.
func TestImageHostSwapPurity(t *testing.T) {
	const brandHost = "img.ratesbazar.com"
	body := strings.Join([]string{
		`<img src="https://img.projectjarvis.io/images/liberty-mutual/logo.png">`,
		`<a href="https://www.eos57ytf.com/K4C5ZLC/KQCKQ7/?sub2=ratesbazar.com">money</a>`,
		`<a href="https://t.em.ratesbazar.com/track/unsubscribe/abc">unsub</a>`,
		`<a href="https://projectjarvis.io/api/mailing/bt/xyz">beacon</a>`, // bare host, no img.
		`<td background="https://img.projectjarvis.io/images/liberty-mutual/bg.jpg">`,
	}, "\n")

	// Exactly the production statement.
	got := strings.ReplaceAll(body, neutralImageHost, "https://"+brandHost)

	if strings.Contains(got, neutralImageHost) {
		t.Errorf("neutral host still present after swap:\n%s", got)
	}
	if n := strings.Count(got, "https://"+brandHost); n != 2 {
		t.Errorf("brand host count = %d, want 2", n)
	}
	// Money / tracking / beacon must be byte-for-byte untouched.
	for _, must := range []string{
		"https://www.eos57ytf.com/K4C5ZLC/KQCKQ7/?sub2=ratesbazar.com",
		"https://t.em.ratesbazar.com/track/unsubscribe/abc",
		"https://projectjarvis.io/api/mailing/bt/xyz",
	} {
		if !strings.Contains(got, must) {
			t.Errorf("swap clobbered a non-image URL; missing: %s", must)
		}
	}

	// Idempotent: re-running is a no-op.
	if again := strings.ReplaceAll(got, neutralImageHost, "https://"+brandHost); again != got {
		t.Error("swap is not idempotent on already-swapped body")
	}
}
