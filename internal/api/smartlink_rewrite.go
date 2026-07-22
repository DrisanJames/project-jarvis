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

// RewriteMoneyLinksToGateway rewrites every cratoolpro money href in html to
// the brand-site gateway URL https://<brandRoot>/<slug>. It is the server-side
// port of smartlink_gateway.py's rewrite_offer_to_gateway.
//
// Idempotent: only cratoolpro hrefs match, and the rewritten gateway URLs
// (https://<brandRoot>/<slug>) never re-match — so running twice equals
// running once. cratoolpro.com/integration/... links are left untouched,
// mirroring the Python negative-lookahead exclusion.
//
// brandRoot and slug are expected to be validated by the caller (an owned
// brand apex + a slugPattern-valid slug). As a defensive belt-and-suspenders,
// an empty brandRoot or slug leaves html unchanged and returns count 0.
//
// Returns the rewritten html and the number of hrefs rewritten.
func RewriteMoneyLinksToGateway(html, brandRoot, slug string) (string, int) {
	if brandRoot == "" || slug == "" {
		return html, 0
	}
	gateway := `href="https://` + brandRoot + `/` + slug + `"`
	n := 0
	out := cratoolproHrefRe.ReplaceAllStringFunc(html, func(m string) string {
		sub := cratoolproHrefRe.FindStringSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		// Exclude cratoolpro.com/integration/... exactly as the Python
		// (?!integration/) lookahead does (case-insensitive).
		if strings.HasPrefix(strings.ToLower(sub[2]), "integration/") {
			return m
		}
		n++
		return gateway
	})
	return out, n
}

// RewriteMoneyLinksToTracking rewrites every cratoolpro money href to the
// tracking-layer /o/ URL https://<trackingDomain>/o/<sub>/<hash>/<campaign>.
// It is the Wave-1 sibling of RewriteMoneyLinksToGateway: same cratoolpro
// regex and same /integration/ exclusion, but the destination is the tracking
// service's offer-link dictionary URL instead of the brand-site gateway.
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
