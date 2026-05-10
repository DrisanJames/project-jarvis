package tracking

import (
	"strings"
	"time"
)

// scannerUAs are case-insensitive substrings that match known scanner /
// security / proxy user agents. Hits here are the strongest signal we
// have that the click was machine-generated. The list is intentionally
// short — we only include vendors whose link-rewrite or click-time
// protection products are widely deployed and whose UA strings are
// well-documented. Adding a new entry should be done only when there
// is concrete production evidence of the substring appearing in a
// click event that was demonstrably not a human.
var scannerUAs = []string{
	"safelinks",   // Microsoft Defender SafeLinks
	"office365",   // generic Office 365 scanner UA
	"microsoft.com",
	"mimecast",    // Mimecast URL Defense
	"proofpoint",  // Proofpoint URL Defense
	"barracuda",   // Barracuda Networks
	"ironport",    // Cisco IronPort
	"forcepoint",  // Forcepoint
	"symantec",    // Symantec Click-Time URL Protection
	"trendmicro",  // Trend Micro
	"trend micro",
	"avanan",       // Avanan
	"messagelabs",  // Symantec MessageLabs
	"googlebot",    // Google bot (rare on click but seen)
	"slurp",        // Yahoo legacy
}

// ClassifyClickAsMachine returns true when the click event matches a
// known scanner / proxy / security signature. CONSERVATIVE: false
// negatives are preferred over false positives. We never mark a click
// as machine unless we have strong signal.
//
// Rules (in order, short-circuit on first true):
//  1. UA matches an explicit scanner substring -> true.
//  2. UA is empty OR exactly "Mozilla/5.0" AND IP is in a cloud /
//     security CIDR -> true.
//  3. deltaSinceSend < 30 seconds AND UA is bare "Mozilla/5.0" -> true
//     (mirrors the MPP-open heuristic for clicks).
//  4. Otherwise -> false.
//
// deltaSinceSend may be zero / negative if the send timestamp is
// unknown — in that case rule 3 cannot fire and the function falls
// through to false. Callers that cannot afford the extra DB round-trip
// to look up the send timestamp may pass 0 and rely on rules 1 and 2
// only; this trade-off is documented at the call site.
func ClassifyClickAsMachine(userAgent, ipAddress string, deltaSinceSend time.Duration) bool {
	uaLower := strings.ToLower(strings.TrimSpace(userAgent))

	// Rule 1: explicit scanner UA.
	for _, scanner := range scannerUAs {
		if strings.Contains(uaLower, scanner) {
			return true
		}
	}

	// "Bare Mozilla" = empty UA OR the literal string "mozilla/5.0"
	// with no browser identification token appended. Real browsers
	// always extend this prefix with at least a platform comment
	// (Windows NT, Macintosh, X11) and a rendering engine token
	// (AppleWebKit, Gecko). Scanners that bother spoofing a UA almost
	// always stop at the bare prefix.
	bareMozilla := uaLower == "" || uaLower == "mozilla/5.0"

	// Rule 2: bare/empty UA + cloud IP.
	if bareMozilla && IsCloudOrScannerIP(ipAddress) {
		return true
	}

	// Rule 3: bare Mozilla + sub-30s delta (mirrors the 120s MPP-open
	// heuristic; clicks fire much faster than image loads, so a 30s
	// window is the analog).
	if bareMozilla && deltaSinceSend > 0 && deltaSinceSend < 30*time.Second {
		return true
	}

	return false
}
