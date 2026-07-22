package api

import (
	"regexp"
	"strings"
)

// cratoolproHrefRe matches a cratoolpro money link inside an href="..."
// attribute. It is the Go port of _CRATOOLPRO_HREF_VALUE in
// mailing-saas/smartlink_gateway.py:
//
//	href="(https?://(?:www\.)?cratoolpro\.com/(?!integration/)[^"\s]+)"
//
// RE2 (Go's regexp) does NOT support the (?!integration/) negative lookahead,
// so we capture the path in group 2 instead and reject the /integration/
// prefix in code — behaviorally identical to the Python exclusion. The match
// is case-insensitive ((?i)) like the Python re.IGNORECASE. Group 1 is the
// full URL, group 2 is the path after "cratoolpro.com/".
var cratoolproHrefRe = regexp.MustCompile(`(?i)href="(https?://(?:www\.)?cratoolpro\.com/([^"\s]+))"`)

// RewriteMoneyLinksToTracking rewrites every cratoolpro money href to the
// tracking-layer /o/ URL https://<trackingDomain>/o/<sub>/<hash>/<campaign>.
// Same cratoolpro regex and /integration/ exclusion as smartlink_gateway.py;
// the destination is the tracking service's offer-link dictionary URL.
//
// Idempotent: only cratoolpro hrefs match, and the emitted /o/ URL never
// re-matches, so a second pass is a no-op. Empty trackingDomain, subscriberID,
// hash, or campaignID leaves html unchanged and returns 0 — a missing segment
// would otherwise mint a malformed link (e.g. https:///o/...), and refusing to
// rewrite is the safe failure (the caller keeps the original cratoolpro href
// rather than shipping a broken tracking URL).
//
// Returns the rewritten html and the number of hrefs rewritten.
func RewriteMoneyLinksToTracking(html, trackingDomain, subscriberID, hash, campaignID string) (string, int) {
	if trackingDomain == "" || subscriberID == "" || hash == "" || campaignID == "" {
		return html, 0
	}
	tracking := `href="` + SmartLinkTrackingURL(trackingDomain, subscriberID, hash, campaignID) + `"`
	n := 0
	out := cratoolproHrefRe.ReplaceAllStringFunc(html, func(m string) string {
		sub := cratoolproHrefRe.FindStringSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		if strings.HasPrefix(strings.ToLower(sub[2]), "integration/") {
			return m
		}
		n++
		return tracking
	})
	return out, n
}
