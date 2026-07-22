package api

import (
	"regexp"
	"strings"
)

// moneyRewriteHosts is the maintained set of offer-network money-link hosts
// whose creative hrefs get rewritten to the tracking-layer /o/ URL. EXTEND this
// when a new offer network is onboarded — the source of truth is the money_url
// hosts in agents/scheduling/offers.py (cratoolpro/eos57ytf/k8k0hfdt/codefortwo/
// kj3rwth8trk/muqes as of 2026-07-22). Kept as a plain slice so onboarding a
// network is a one-line edit; moneyHrefRe is derived from it.
//
// NOTE: this is a DISTINCT list from money_link_check.go's `moneyHosts` (the
// send-day creative *validator*'s host set, which additionally carries dead-link
// history hosts like xnonu/jyqye). They are intentionally separate — the
// validator's membership must not silently change the set of hosts that get /o/
// -rewritten, and vice-versa. Keep this one aligned with offers.py.
var moneyRewriteHosts = []string{
	"cratoolpro.com",
	"eos57ytf.com",
	"k8k0hfdt.com",
	"codefortwo.com",
	"kj3rwth8trk.com",
	"muqes.com",
}

// moneyHrefRe matches a money-link href="..." on ANY host in moneyRewriteHosts.
// It is the multi-network successor of the cratoolpro-only regex (the Go port of
// _CRATOOLPRO_HREF_VALUE in mailing-saas/smartlink_gateway.py):
//
//	href="(https?://(?:www\.)?(<host1>|<host2>|...)/(?!integration/)[^"\s]+)"
//
// RE2 (Go's regexp) does NOT support the (?!integration/) negative lookahead,
// so we capture the path in group 2 instead and reject the /integration/ prefix
// in code — behaviorally identical to the Python exclusion (harmless for the
// non-cratoolpro hosts, which have no /integration/ postback path). The match
// is case-insensitive ((?i)). Group 1 is the full URL, group 2 is the path
// after "<host>/". Hosts are regexp-quoted so a literal dot never matches an
// arbitrary character.
var moneyHrefRe = buildMoneyHrefRe(moneyRewriteHosts)

func buildMoneyHrefRe(hosts []string) *regexp.Regexp {
	quoted := make([]string, len(hosts))
	for i, h := range hosts {
		quoted[i] = regexp.QuoteMeta(h)
	}
	return regexp.MustCompile(`(?i)href="(https?://(?:www\.)?(?:` + strings.Join(quoted, "|") + `)/([^"\s]+))"`)
}

// RewriteMoneyLinksToTracking rewrites every money href on any moneyRewriteHosts
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
	n := 0
	out := moneyHrefRe.ReplaceAllStringFunc(html, func(m string) string {
		sub := moneyHrefRe.FindStringSubmatch(m)
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
