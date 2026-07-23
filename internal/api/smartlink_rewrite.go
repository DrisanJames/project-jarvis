package api

import (
	"strings"

	"github.com/ignite/sparkpost-monitor/internal/pkg/moneylink"
)

// The money-host list + href regex now live in the shared moneylink package
// (internal/pkg/moneylink), the single source of truth co-owned with the send
// worker's /o/ emitter and the Creative Studio offer-links reporter. Use
// moneylink.Hosts and moneylink.HrefRe() here.

// RewriteMoneyLinksToTracking rewrites every money href on any moneylink.Hosts
// network to the tracking-layer /o/ URL
// https://<trackingDomain>/o/<sub>/<hash>/<campaign>. Same /integration/
// exclusion as smartlink_gateway.py; the destination is the tracking service's
// offer-link dictionary URL.
//
// Idempotent: only money-host hrefs match, and the emitted /o/ URL never
// re-matches, so a second pass is a no-op. Empty trackingDomain, subscriberID,
// hash, or campaignID leaves html unchanged and returns 0 — a missing segment
// would otherwise mint a malformed link (e.g. https:///o/...), and refusing to
// rewrite is the safe failure (the caller keeps the original money href rather
// than shipping a broken tracking URL).
//
// Returns the rewritten html and the number of hrefs rewritten.
func RewriteMoneyLinksToTracking(html, trackingDomain, subscriberID, hash, campaignID string) (string, int) {
	if trackingDomain == "" || subscriberID == "" || hash == "" || campaignID == "" {
		return html, 0
	}
	tracking := `href="` + SmartLinkTrackingURL(trackingDomain, subscriberID, hash, campaignID) + `"`
	re := moneylink.HrefRe()
	n := 0
	out := re.ReplaceAllStringFunc(html, func(m string) string {
		sub := re.FindStringSubmatch(m)
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
