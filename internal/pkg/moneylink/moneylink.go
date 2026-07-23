// Package moneylink is the SINGLE SOURCE OF TRUTH for offer money-link
// recognition shared across the platform: the send worker mints /o/ tracking
// hashes with it (internal/worker/smartlink_emitter.go), the API rewrites
// creative hrefs to the tracking layer with it (internal/api/smartlink_rewrite.go),
// and the Creative Studio offer-links reporter answers "is this destination
// already mapped?" with it (internal/api/creative_offer_links.go).
//
// Keeping Normalize + Hosts + HrefRe in ONE package is load-bearing: the UI's
// "mapped" verdict must never disagree with the send path's /o/ minting. If a
// rendered creative href and the stored smart-link template reduce to the same
// Normalize key, the send worker will swap that href for the seeded hash — so
// the reporter must key on the identical function, not a re-implementation.
package moneylink

import (
	"net/url"
	"regexp"
	"strings"
)

// Hosts is the maintained set of offer-network money-link hosts whose creative
// hrefs get rewritten to the tracking-layer /o/ URL. EXTEND this when a new
// offer network is onboarded — the source of truth is the money_url hosts in
// agents/scheduling/offers.py (cratoolpro/eos57ytf/k8k0hfdt/codefortwo/
// kj3rwth8trk/muqes as of 2026-07-22). Kept as a plain slice so onboarding a
// network is a one-line edit; the href regex is derived from it.
//
// NOTE: this is a DISTINCT list from money_link_check.go's `moneyHosts` (the
// send-day creative *validator*'s host set, which additionally carries dead-link
// history hosts like xnonu/jyqye). They are intentionally separate — the
// validator's membership must not silently change the set of hosts that get /o/
// -rewritten, and vice-versa. Keep this one aligned with offers.py.
var Hosts = []string{
	"cratoolpro.com",
	"eos57ytf.com",
	"k8k0hfdt.com",
	"codefortwo.com",
	"kj3rwth8trk.com",
	"muqes.com",
}

// hrefRe matches a money-link href="..." on ANY host in Hosts. It is the
// multi-network successor of the cratoolpro-only regex (the Go port of
// _CRATOOLPRO_HREF_VALUE in mailing-saas/smartlink_gateway.py):
//
//	href="(https?://(?:www\.)?(<host1>|<host2>|...)/(?!integration/)[^"\s]+)"
//
// RE2 (Go's regexp) does NOT support the (?!integration/) negative lookahead,
// so we capture the path in group 2 instead and callers reject the /integration/
// prefix in code — behaviorally identical to the Python exclusion (harmless for
// the non-cratoolpro hosts, which have no /integration/ postback path). The
// match is case-insensitive ((?i)). Group 1 is the full URL, group 2 is the
// path after "<host>/". Hosts are regexp-quoted so a literal dot never matches
// an arbitrary character.
var hrefRe = buildHrefRe(Hosts)

// HrefRe returns the compiled money-link href regex derived from Hosts. Group 1
// is the full URL; group 2 is the path after "<host>/" (used by callers for the
// /integration/ exclusion).
func HrefRe() *regexp.Regexp { return hrefRe }

func buildHrefRe(hosts []string) *regexp.Regexp {
	quoted := make([]string, len(hosts))
	for i, h := range hosts {
		quoted[i] = regexp.QuoteMeta(h)
	}
	return regexp.MustCompile(`(?i)href="(https?://(?:www\.)?(?:` + strings.Join(quoted, "|") + `)/([^"\s]+))"`)
}

// Normalize reduces an offer URL to a stable dictionary key:
// scheme + "://" + lowercased-host + path, with the query string, fragment,
// and any trailing slash(es) on the path stripped. Host is lowercased; the
// PATH IS KEPT AS-IS because cratoolpro paths are case-significant (uppercase
// account/offer segments). Stripping the query is what lets a rendered creative
// href (with concrete ?source_id=email&sub1=123&... params) and the stored
// template (with ?sub1={{subscriber.id}}&... mustache params) both reduce to
// the same key, e.g. "https://www.cratoolpro.com/XXX/YYY".
//
// The query is chopped off BEFORE url.Parse so mustache braces in a template's
// params can never trip the parser. An unparseable remainder degrades to a
// trailing-slash-trimmed copy of the pre-query string.
func Normalize(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = s[:i]
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return strings.TrimRight(s, "/")
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	path := strings.TrimRight(u.Path, "/")
	return scheme + "://" + host + path
}
