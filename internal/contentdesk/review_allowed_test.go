package contentdesk

import "testing"

// Live 2026-09-15..18: four articles were approved with an odd faq block the
// publishers refuse to render. Nothing could move them — a "changes" decision
// was refused because they were no longer in review — and their sites could
// publish nothing else, since one unrenderable article refuses the whole site.
func TestReviewAllowedFor(t *testing.T) {
	cases := []struct {
		status, decision string
		want             bool
	}{
		{StatusInReview, "approve", true},
		{StatusInReview, "changes", true},
		{StatusApproved, "changes", true}, // back to the desk: unpublishable
		{StatusApproved, "reject", true},
		{StatusApproved, "approve", false}, // no second approval after the fact
		{"live_confirmed", "changes", false},
		{"drafting", "changes", false},
		{"withdrawn", "changes", false},
	}
	for _, c := range cases {
		if got := reviewAllowedFor(c.status, c.decision); got != c.want {
			t.Fatalf("%s + %s = %v, want %v", c.status, c.decision, got, c.want)
		}
	}
}
