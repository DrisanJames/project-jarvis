package tracking

// Unit tests for ClassifyClickAsMachine and IsCloudOrScannerIP.
//
// The classifier is the SOLE producer of the is_machine_click column on
// mailing_tracking_events (SA-5 wiring in api.HandleTrackClick and
// tracking.processClick). These tests pin its rule order and lock the
// conservative posture: when in doubt, return false. Any change that
// turns a previously-false case into true must demonstrably reflect a
// real production scanner pattern, not a hunch.
//
// Coverage:
//   Rule 1 — explicit scanner UA substring  (SafeLinks, Mimecast)
//   Rule 2 — bare/empty UA + cloud IP
//   Rule 3 — bare Mozilla + sub-30s send delta
//   Negative — humans, residential IPs, long deltas, malformed input

import (
	"testing"
	"time"
)

// TestClassifyClickAsMachine_SafeLinks pins Rule 1: any UA containing
// "safelinks" (case-insensitive) is classified as machine, regardless
// of IP or delta. Microsoft Defender SafeLinks rewrites every link in
// inbound mail and pre-fetches the destination from a sandbox before
// the user ever sees it.
func TestClassifyClickAsMachine_SafeLinks(t *testing.T) {
	got := ClassifyClickAsMachine(
		"Mozilla/5.0 (compatible; MSIE 9.0; Windows NT 6.1; Trident/5.0; SafeLinks)",
		"76.123.45.67", // residential — should not matter
		5*time.Minute,  // long delta — should not matter
	)
	if !got {
		t.Errorf("expected true for SafeLinks UA, got false")
	}
}

// TestClassifyClickAsMachine_Mimecast pins Rule 1 for Mimecast URL
// Defense, the second-most-common rewriter in our production click
// stream after SafeLinks.
func TestClassifyClickAsMachine_Mimecast(t *testing.T) {
	got := ClassifyClickAsMachine(
		"Mimecast Link Checker",
		"8.8.8.8",
		1*time.Hour,
	)
	if !got {
		t.Errorf("expected true for Mimecast UA, got false")
	}
}

// TestClassifyClickAsMachine_BareMozillaCloudIP pins Rule 2: bare
// "Mozilla/5.0" with no browser/platform tokens, originating from a
// cloud provider IP, is classified as machine. The IP "3.5.140.1" is
// inside AWS 3.0.0.0/9 per cloud_cidrs.go.
func TestClassifyClickAsMachine_BareMozillaCloudIP(t *testing.T) {
	got := ClassifyClickAsMachine("Mozilla/5.0", "3.5.140.1", 0)
	if !got {
		t.Errorf("expected true for bare Mozilla on AWS IP, got false")
	}
}

// TestClassifyClickAsMachine_BareMozillaResidential pins the
// CONSERVATIVE posture: a bare Mozilla/5.0 from a residential IP is
// NOT enough to flag as machine. Some legitimate clients (older email
// readers, link previews on mobile keyboards) do emit just
// "Mozilla/5.0", and we never want to drop a real subscriber's click
// just because their UA looks lazy. Without a cloud IP and without a
// sub-30s delta, the classifier must stay quiet.
func TestClassifyClickAsMachine_BareMozillaResidential(t *testing.T) {
	got := ClassifyClickAsMachine("Mozilla/5.0", "76.123.45.67", 0)
	if got {
		t.Errorf("expected false for bare Mozilla on residential IP with delta=0, got true")
	}
}

// TestClassifyClickAsMachine_BareMozillaSubThirtySeconds pins Rule 3:
// bare Mozilla + a click that landed less than 30 seconds after the
// send is the click-side analog of the MPP-open 120-second heuristic.
// No human reads, decides, and clicks within 10 seconds of receiving
// an email — that's a security scanner running its post-delivery
// pre-fetch.
func TestClassifyClickAsMachine_BareMozillaSubThirtySeconds(t *testing.T) {
	got := ClassifyClickAsMachine("Mozilla/5.0", "76.123.45.67", 10*time.Second)
	if !got {
		t.Errorf("expected true for bare Mozilla with 10s delta, got false")
	}
}

// TestClassifyClickAsMachine_BareMozillaOverThirtySeconds confirms the
// 30-second window is a hard cutoff: at 60s post-send a bare Mozilla
// from a residential IP is NOT machine. This is the most likely
// false-positive zone in the rule set, so the test is explicit about
// staying on the safe side.
func TestClassifyClickAsMachine_BareMozillaOverThirtySeconds(t *testing.T) {
	got := ClassifyClickAsMachine("Mozilla/5.0", "76.123.45.67", 60*time.Second)
	if got {
		t.Errorf("expected false for bare Mozilla with 60s delta on residential IP, got true")
	}
}

// TestClassifyClickAsMachine_HumanChrome covers the dominant happy
// path: a real Chrome on Windows, regardless of IP, is never flagged.
// If this test ever flips it means the scanner UA list grew a
// substring that accidentally matches a real browser token.
func TestClassifyClickAsMachine_HumanChrome(t *testing.T) {
	got := ClassifyClickAsMachine(
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"3.5.140.1", // even on a cloud IP
		1*time.Second, // even sub-30s
	)
	if got {
		t.Errorf("expected false for human Chrome UA, got true")
	}
}

// TestClassifyClickAsMachine_EmptyUA_CloudIP pins Rule 2 for the
// empty-UA case. Some scanners (notably older Defender variants and
// homegrown corporate proxies) strip the UA entirely. Combined with a
// cloud-provider IP, that's strong machine signal.
func TestClassifyClickAsMachine_EmptyUA_CloudIP(t *testing.T) {
	got := ClassifyClickAsMachine("", "3.5.140.1", 0)
	if !got {
		t.Errorf("expected true for empty UA on AWS IP, got false")
	}
}

// TestIsCloudOrScannerIP_KnownAWS confirms a known AWS IP returns true.
// 3.5.140.1 sits inside the 3.0.0.0/9 CIDR which is the broadest of
// our AWS samples; if it ever stops matching, parsedCloudCIDRs is not
// being initialised correctly.
func TestIsCloudOrScannerIP_KnownAWS(t *testing.T) {
	if !IsCloudOrScannerIP("3.5.140.1") {
		t.Errorf("expected true for known AWS IP 3.5.140.1, got false")
	}
}

// TestIsCloudOrScannerIP_Residential confirms a residential IP returns
// false. 76.123.45.67 is in the 76.0.0.0/8 Comcast block which is
// intentionally NOT in the cloud CIDR list.
func TestIsCloudOrScannerIP_Residential(t *testing.T) {
	if IsCloudOrScannerIP("76.123.45.67") {
		t.Errorf("expected false for residential IP 76.123.45.67, got true")
	}
}

// TestIsCloudOrScannerIP_InvalidIP pins the no-panic guarantee for
// malformed input. The IP string passed into the classifier comes from
// request headers / SQS payloads — both can carry arbitrary garbage
// (proxies that prepend "X-Forwarded-For: <comma list>", clients that
// send a hostname instead of a literal IP, etc.). The function must
// return false rather than panic.
func TestIsCloudOrScannerIP_InvalidIP(t *testing.T) {
	if IsCloudOrScannerIP("not.an.ip") {
		t.Errorf("expected false for invalid IP, got true")
	}
	if IsCloudOrScannerIP("") {
		t.Errorf("expected false for empty IP, got true")
	}
	if IsCloudOrScannerIP("999.999.999.999") {
		t.Errorf("expected false for out-of-range IP, got true")
	}
}
