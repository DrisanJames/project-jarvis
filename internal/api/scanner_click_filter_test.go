package api

import (
	"strings"
	"testing"
)

// REQ-045 — pins nonScannerClickFilter's exclusion set (the click-side
// consumer of the verdict classes) so a verdict-function change can never
// silently diverge from the segment-entry filter again. The v2 contract:
//   EXCLUDED: datacenter, machine-bare, farm, unknown, google-egress
//             (google-egress added per S7-gmail: 14d google-egress CLICKERS
//             = 0 — near-zero membership impact, closes the automation door)
//   KEPT:     human, human-relay, proxy-view, apple-mpp (Apple audience),
//             ses-tracked (vessel/probation — SES clicks formerly died as
//             'unknown'), human-ua-only (lower-confidence human), and NULL
//             (not yet classified — never cut the audience before backfill).
func TestNonScannerClickFilter_V2ExclusionSet(t *testing.T) {
	frag := nonScannerClickFilter("clicked", "e")

	if !strings.HasPrefix(frag, " AND (") {
		t.Fatalf("fragment must be a leading-AND parenthesized predicate; got %q", frag)
	}
	if !strings.Contains(frag, "e.click_verdict IS NULL OR") {
		t.Fatalf("fragment must keep NULL (not-yet-classified) rows; got %q", frag)
	}

	excluded := []string{"'datacenter'", "'machine-bare'", "'farm'", "'unknown'", "'google-egress'"}
	for _, cls := range excluded {
		if !strings.Contains(frag, cls) {
			t.Fatalf("exclusion set must contain %s; got %q", cls, frag)
		}
	}
	kept := []string{"ses-tracked", "human-ua-only", "apple-mpp", "proxy-view", "human-relay"}
	for _, cls := range kept {
		if strings.Contains(frag, cls) {
			t.Fatalf("class %q must be KEPT (not appear in the exclusion set); got %q", cls, frag)
		}
	}
}

func TestNonScannerClickFilter_EmptyForNonClickEvents(t *testing.T) {
	for _, et := range []string{"opened", "bounced", "sent", ""} {
		if got := nonScannerClickFilter(et, "e"); got != "" {
			t.Fatalf("eventType %q must produce no filter (click_verdict is NULL there); got %q", et, got)
		}
	}
}
