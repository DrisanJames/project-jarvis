package main

import (
	"net/netip"
	"strings"
	"testing"
)

// =============================================================================
// REQ-045 — verdict function v2 unit tests.
//
// The verdict lives in Postgres; these tests pin the COMMITTED body (the exact
// string runStartupMigrations installs and checkVerdictFunctionDrift compares
// against prosrc) two ways:
//   1. Structural pins: branch ORDER inside igniteEventVerdictBody — the
//      2026-07-13 S1 bug WAS an ordering bug (google-egress evaluated before
//      proxy-view made Gmail view-time reads unreachable), so the order is the
//      semantics.
//   2. A test-only Go transcription of the CASE, guarded by the order pins,
//      evaluated against the DoD scenarios (GoogleImageProxy UA + 66.249.x →
//      'proxy-view', SES-Tracked → 'ses-tracked', device UA + NULL IP →
//      'human-ua-only', ...). The pg-side confirmation plan (read-only
//      SELECTs) is in the REQ-045 build report for the lead to execute.
// =============================================================================

// verdictBranchOrder is the class-emission order the v2 body must keep.
// ignite_ip_is_datacenter stands in for the 'datacenter' branch (its THEN
// literal 'datacenter' also appears in provider comments, so the call site is
// the unambiguous anchor).
var verdictBranchOrder = []string{
	`'farm'`,
	`'apple-mpp'`,
	`'human-relay'`,
	`ignite_ip_is_datacenter(ip)`,
	`'proxy-view'`,
	`'google-egress'`,
	`'ses-tracked'`,
	`'machine-bare'`,
	`'human-ua-only'`,
	`'human'`,
	`'unknown'`,
}

func TestVerdictBodyBranchOrder(t *testing.T) {
	// Sequential scan: each marker must appear AFTER the previous one (a plain
	// full-string Index would trip on class names quoted inside comments).
	rest := igniteEventVerdictBody
	pos := 0
	for _, marker := range verdictBranchOrder {
		idx := strings.Index(rest, marker)
		if idx < 0 {
			t.Fatalf("verdict body is missing branch marker %s after offset %d — branch missing or out of order", marker, pos)
		}
		pos += idx + len(marker)
		rest = igniteEventVerdictBody[pos:]
	}

	// The load-bearing REQ-045 #1 relation, asserted explicitly: the
	// proxy-view UA branch must precede the google-egress IP branch.
	pv := strings.Index(igniteEventVerdictBody, `'proxy-view'`)
	ge := strings.Index(igniteEventVerdictBody, `'google-egress'`)
	if pv >= ge {
		t.Fatalf("proxy-view branch (index %d) must be evaluated BEFORE google-egress (index %d) — S1 branch-order bug", pv, ge)
	}
}

// --- test-only transcription of the v2 CASE -------------------------------

func cidrHas(ipStr string, cidrs ...string) bool {
	if ipStr == "" {
		return false // SQL: NULL <<= cidr IS NULL → branch not taken
	}
	ip, err := netip.ParseAddr(ipStr)
	if err != nil {
		return false
	}
	for _, c := range cidrs {
		if netip.MustParsePrefix(c).Contains(ip) {
			return true
		}
	}
	return false
}

func uaHas(ua string, uaNull bool, needles ...string) bool {
	if uaNull {
		return false // SQL: NULL ILIKE ... IS NULL → branch not taken
	}
	lower := strings.ToLower(ua)
	for _, n := range needles {
		if strings.Contains(lower, strings.ToLower(n)) {
			return true
		}
	}
	return false
}

var appleEgress = []string{"146.75.0.0/16", "140.248.0.0/16", "2a02:26f7::/32", "2a04:4e41::/32", "2a09:bac2::/32", "2a09:bac3::/32"}
var googleEgress = []string{"66.249.0.0/16", "74.125.0.0/16", "72.14.0.0/16", "209.85.0.0/16"}

// evalVerdictV2 mirrors igniteEventVerdictBody branch-for-branch, in the order
// TestVerdictBodyBranchOrder pins. isDatacenter stubs the table lookup
// (ignite_ip_is_datacenter; false for NULL ip like the SQL EXISTS).
func evalVerdictV2(ua string, uaNull bool, ip string, isDatacenter bool) string {
	deviceUA := uaHas(ua, uaNull, "Windows NT", "Macintosh", "iPhone", "iPad", "Android")
	switch {
	case cidrHas(ip, "75.98.0.0/16"):
		return "farm"
	case !uaNull && ua == "Mozilla/5.0" && cidrHas(ip, appleEgress...):
		return "apple-mpp"
	case uaHas(ua, uaNull, "iPhone", "iPad", "Macintosh") && cidrHas(ip, appleEgress...):
		return "human-relay"
	case ip != "" && isDatacenter:
		return "datacenter"
	case uaHas(ua, uaNull, "yahoo", "GoogleImageProxy"):
		return "proxy-view"
	case cidrHas(ip, googleEgress...):
		return "google-egress"
	case !uaNull && ua == "SES-Tracked":
		return "ses-tracked"
	case uaNull || ua == "" || ua == "Mozilla/5.0":
		return "machine-bare"
	case uaHas(ua, uaNull, "Go-http", "python", "curl"):
		return "machine-bare"
	case ip == "" && deviceUA:
		return "human-ua-only"
	case deviceUA:
		return "human"
	default:
		return "unknown"
	}
}

func TestVerdictV2Scenarios(t *testing.T) {
	const gipUA = "Mozilla/5.0 (Windows NT 5.1; rv:11.0) Gecko Firefox/11.0 (via ggpht.com GoogleImageProxy)"
	const iphoneUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15"
	const chromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/125.0 Safari/537.36"

	cases := []struct {
		name         string
		ua           string
		uaNull       bool
		ip           string
		isDatacenter bool
		want         string
	}{
		// REQ-045 DoD #1: the Gmail image proxy fetches FROM Google egress.
		{"GoogleImageProxy UA from Google egress IP", gipUA, false, "66.249.88.10", false, "proxy-view"},
		{"Yahoo proxy UA unaffected", "YahooMailProxy; https://help.yahoo.com/kb/CH2", false, "98.136.1.10", false, "proxy-view"},
		// Google automation (uniform Chrome, no proxy UA) still classifies google-egress.
		{"Chrome UA from Google egress IP", chromeUA, false, "66.249.88.10", false, "google-egress"},
		// REQ-045 DoD #2: SES ingest sentinel gets its own vessel class.
		{"SES-Tracked with NULL IP", "SES-Tracked", false, "", false, "ses-tracked"},
		{"SES-Tracked with residential IP", "SES-Tracked", false, "98.97.12.34", false, "ses-tracked"},
		{"SES-Tracked detonating from datacenter IP stays datacenter", "SES-Tracked", false, "20.44.10.10", true, "datacenter"},
		// REQ-045 DoD #4: device UA with no captured IP is NOT bare 'human'.
		{"device UA with NULL IP", iphoneUA, false, "", false, "human-ua-only"},
		{"device UA with residential IP", iphoneUA, false, "98.97.12.34", false, "human"},
		// v1 branches preserved.
		{"farm IP wins over everything", chromeUA, false, "75.98.3.4", false, "farm"},
		{"bare Mozilla from Apple egress", "Mozilla/5.0", false, "146.75.1.1", false, "apple-mpp"},
		{"iPhone via Apple egress is human-relay", iphoneUA, false, "146.75.1.1", false, "human-relay"},
		{"NULL UA is machine-bare", "", true, "98.97.12.34", false, "machine-bare"},
		{"curl is machine-bare", "curl/8.4.0", false, "98.97.12.34", false, "machine-bare"},
		{"no signal at all is unknown", "SomethingElse/1.0", false, "98.97.12.34", false, "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := evalVerdictV2(c.ua, c.uaNull, c.ip, c.isDatacenter)
			if got != c.want {
				t.Fatalf("verdict(%q, ip=%q, dc=%v) = %q, want %q", c.ua, c.ip, c.isDatacenter, got, c.want)
			}
		})
	}
}

func TestVerdictIsHumanBodyV2(t *testing.T) {
	body := igniteVerdictIsHumanBody
	// REQ-045: never NULL — the S0 lead-verification NULL-propagation bug.
	if !strings.Contains(body, "COALESCE") {
		t.Fatalf("ignite_verdict_is_human body must be NULL-safe via COALESCE; got %q", body)
	}
	if !strings.Contains(body, "'human','human-relay','proxy-view'") {
		t.Fatalf("human set changed unexpectedly: %q", body)
	}
	// Design decisions: vessel/probation and lower-confidence classes are NOT
	// bare-human (T1, not T0).
	for _, cls := range []string{"ses-tracked", "human-ua-only", "apple-mpp"} {
		if strings.Contains(body, cls) {
			t.Fatalf("class %q must NOT be in the ignite_verdict_is_human set: %q", cls, body)
		}
	}
}

func TestVerdictVersionMarker(t *testing.T) {
	if igniteVerdictVersionStr != "2" {
		t.Fatalf("verdict version = %s; this suite pins v2 semantics — extend the tests with the new semantics before bumping", igniteVerdictVersionStr)
	}
	marker := "-- verdict-version: " + igniteVerdictVersionStr
	if !strings.Contains(igniteEventVerdictBody, marker) {
		t.Fatalf("igniteEventVerdictBody must embed %q so prosrc carries the version", marker)
	}
	if !strings.Contains(igniteVerdictVersionBody, igniteVerdictVersionStr) {
		t.Fatalf("ignite_verdict_version() body %q must return %s", igniteVerdictVersionBody, igniteVerdictVersionStr)
	}
	if !strings.Contains(igniteVerdictVersionDDL, "ignite_verdict_version()") {
		t.Fatalf("version DDL must create ignite_verdict_version(): %q", igniteVerdictVersionDDL)
	}
	// The drift check normalizes comments away — the marker must not make the
	// semantic comparison flag cosmetic drift.
	if strings.Contains(normalizeSQLBody(igniteEventVerdictBody), "verdict-version") {
		t.Fatalf("normalizeSQLBody must strip the version marker comment")
	}
}
