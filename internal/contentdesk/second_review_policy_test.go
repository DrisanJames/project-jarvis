package contentdesk

import "testing"

// Operator 2026-09-12: one reviewer going forward. "advisory" = the fact
// check alone approves a consequential article; anything else (unset,
// "required") keeps the second reviewer.
func TestSecondReviewRequired_Policy(t *testing.T) {
	for v, want := range map[string]bool{"": true, "required": true, "advisory": false, " advisory ": false, "nonsense": true} {
		t.Setenv(EnvSecondReview, v)
		if got := SecondReviewRequired(); got != want {
			t.Fatalf("%q: got %v want %v", v, got, want)
		}
	}
}

// The store passes consequential && SecondReviewRequired() to DecideReview:
// in advisory mode a primary approval on a consequential article approves.
func TestDecideReview_AdvisoryApprovesOnPrimary(t *testing.T) {
	checks := Checks{Judgment: []JudgmentItem{{ID: "j1", Kind: "claim", Verdict: "supported"}}}
	primary := ReviewInput{Reviewer: AgentReviewer, Role: "primary", Decision: "approve"}
	t.Setenv(EnvSecondReview, "advisory")
	if out := DecideReview(checks, true && SecondReviewRequired(), nil, primary); out.Status != StatusApproved {
		t.Fatalf("advisory: primary approval must approve: %+v", out)
	}
	t.Setenv(EnvSecondReview, "")
	if out := DecideReview(checks, true && SecondReviewRequired(), nil, primary); out.Status != StatusInReview {
		t.Fatalf("required: a second reviewer is still needed: %+v", out)
	}
}
