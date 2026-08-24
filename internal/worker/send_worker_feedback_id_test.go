package worker

import "testing"

// Google FBL spec: identifiers must aggregate — never per-message-unique — and
// SenderId must be a constant 5–15 char token. Pinned so the header can never
// silently regress to the dead per-message format.
func TestBuildFeedbackID(t *testing.T) {
	cases := []struct{ campaign, from, want string }{
		{"c-1", "offers@m.financialcalculate.com", "c-1:financialcalculate:bcast:jvmail1"},
		{"c-2", "news@em.discountblog.com", "c-2:discountblog:drip:jvmail1"},
		{"c-3", "hi@quizfiesta.com", "c-3:quizfiesta:other:jvmail1"},
		{"c-4", "bad-address-no-at", "c-4:bad-address-no-at:other:jvmail1"},
	}
	for _, c := range cases {
		if got := buildFeedbackID(c.campaign, c.from); got != c.want {
			t.Errorf("buildFeedbackID(%q,%q) = %q, want %q", c.campaign, c.from, got, c.want)
		}
	}
}
