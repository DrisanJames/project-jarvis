package api

import (
	"strings"
	"testing"
)

// REQ-045 / SIGNAL-GRADING (docs/JAOS/core.md §14, 2026-07-18) — pins
// nonScannerClickFilter's exclusion set (the click-side consumer of the verdict
// classes) so a verdict-function change can never silently diverge from the
// segment-entry filter again. The scanner verdict is a scanner-STORM filter, NOT
// a human detector, so the ONLY excluded class is:
//   EXCLUDED: farm (the 75.98.0.0/16 Yahoo seed farm — proven pure machine,
//             0 human converters)
//   KEPT:     everything else — human, human-relay, proxy-view, apple-mpp,
//             ses-tracked, human-ua-only, AND the recovered machine-ish classes
//             (datacenter, machine-bare, unknown, google-egress) that were shown
//             to contain ~20% of proven human clickers behind proxies/scanners —
//             plus NULL (not yet classified — never cut the audience before backfill).
func TestNonScannerClickFilter_SignalGradingExclusionSet(t *testing.T) {
	frag := nonScannerClickFilter("clicked", "e")

	if !strings.HasPrefix(frag, " AND (") {
		t.Fatalf("fragment must be a leading-AND parenthesized predicate; got %q", frag)
	}
	if !strings.Contains(frag, "e.click_verdict IS NULL OR") {
		t.Fatalf("fragment must keep NULL (not-yet-classified) rows; got %q", frag)
	}

	// 'farm' is the ONLY class excluded.
	if !strings.Contains(frag, "'farm'") {
		t.Fatalf("exclusion set must contain 'farm'; got %q", frag)
	}
	// Every recovered/kept class must NOT appear in the exclusion predicate.
	kept := []string{
		"datacenter", "machine-bare", "unknown", "google-egress", // recovered
		"ses-tracked", "human-ua-only", "apple-mpp", "proxy-view", "human-relay", // already human
	}
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
