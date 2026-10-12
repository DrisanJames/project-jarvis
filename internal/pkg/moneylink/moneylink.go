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
	"sort"
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

// ensureHTTPSOrigin normalizes a bare host, a host with a trailing slash, or a
// full URL to an https-scheme origin with no trailing slash. It mirrors the
// ensureHTTPS helpers in internal/api and internal/worker; it lives here so the
// shared /o/ builder owns its own scheme handling without importing either
// package. An empty input stays "" (the caller guards against minting a link
// with no host).
func ensureHTTPSOrigin(domainOrURL string) string {
	d := strings.TrimSpace(domainOrURL)
	if d == "" {
		return ""
	}
	if !strings.HasPrefix(d, "http") {
		d = "https://" + d
	}
	return strings.TrimRight(d, "/")
}

// BrandFromTrackingDomain derives the SENDING brand apex from a tracking domain.
// It lowercases, drops any scheme/path/port, strips the tracking-subdomain
// prefixes ("t.em.", "trk.em.", "www.") and returns the remaining apex:
//
//	"https://t.em.consumerpro.net"  -> "consumerpro.net"
//	"trk.em.historythinking.com"    -> "historythinking.com"
//	"em.discountblog.com"           -> "em.discountblog.com" (no known prefix)
//	"consumerpro.net"               -> "consumerpro.net"
//
// Returns "" when nothing usable remains (empty input, "https://", "t.em." with
// no apex) so callers can fall back to the legacy brand-less /o/ form. This is
// the mint-time counterpart of the tracking service's brandRootFromHost: the
// brand is baked into the /o/ path here because our own CloudFront strips the
// viewer Host before the tracking service can read it.
func BrandFromTrackingDomain(td string) string {
	s := strings.ToLower(strings.TrimSpace(td))
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i] // drop any path
	}
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i] // drop any port
	}
	for _, p := range []string{"t.em.", "trk.em.", "www."} {
		if strings.HasPrefix(s, p) {
			s = strings.TrimPrefix(s, p)
			break
		}
	}
	return strings.TrimSpace(s)
}

// OfferTrackingURL builds the tracking-layer offer-link URL for the
// brand-in-path contract (2026-07-22):
//
//	https://<trackingDomain>/o/<brand>/<subscriberID>/<hash>/<campaignID>
//
// where <brand> = BrandFromTrackingDomain(trackingDomain). Carrying the sending
// brand as the FIRST /o/ path segment is what lets the tracking service derive
// sub2 (brand.domain) even though our CloudFront distribution strips the viewer
// Host (origin-request-policy allExcept:[host]) before the tracking service sees
// the request. The hash stays MID-PATH so a truncated link still carries it.
//
// When no brand can be derived (empty/malformed trackingDomain), the brand
// segment is OMITTED and the legacy 4-segment form
// https://<trackingDomain>/o/<sub>/<hash>/<campaign> is emitted instead — a
// safe fallback that the tracking handler's legacy route still resolves.
// trackingDomain may be a bare host, a host with a trailing slash, or a full
// https URL; it is normalized to an https origin first. An empty trackingDomain
// yields a host-less "/o/..." — callers (the emitter, the rewriter) already
// guard against empty inputs, matching the pre-existing SmartLinkTrackingURL
// contract this replaces.
func OfferTrackingURL(trackingDomain, subscriberID, hash, campaignID string) string {
	origin := ensureHTTPSOrigin(trackingDomain)
	brand := BrandFromTrackingDomain(trackingDomain)
	if brand == "" {
		return origin + "/o/" + subscriberID + "/" + hash + "/" + campaignID
	}
	return origin + "/o/" + brand + "/" + subscriberID + "/" + hash + "/" + campaignID
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

// ---------------------------------------------------------------------------
// Disambiguating by fixed query params (2026-10-12)
// ---------------------------------------------------------------------------
//
// Normalize drops the whole query string, so two smart links on the same host
// and path that differ only in a FIXED query param collide on one key. That is
// exactly the Insurify pair: Auto  https://protectedoptions.com/?a=1512&c=18903&s1={{subscriber.id}}…
// and Home https://protectedoptions.com/?a=1512&c=20906&s1={{subscriber.id}}…
// both normalized to https://protectedoptions.com. The Home row (created
// 2026-10-06 18:30Z) won last-writer-wins, and from then every Insurify Auto
// click was minted onto the Home hash — the network credited Auto clicks as
// Home leads ($17, not $25).
//
// MatchKey keeps a template's FIXED params (value present and free of a
// {{placeholder}}) in the key; per-recipient params never enter it. Index looks
// a rendered href up by its Normalize base: one candidate → that candidate,
// exactly as before (backward compatible, whatever the href's params); several
// → the candidate whose fixed params ALL appear in the href with the same value,
// most specific first; no unique winner → miss, so the caller falls back to the
// /track/click wrap of the literal href (the right destination, just not /o/).

// FixedParams returns the template's fixed query params: every param whose
// value is non-empty and carries no "{{" placeholder. "&amp;" is read as "&".
func FixedParams(raw string) map[string]string {
	out := map[string]string{}
	for k, vs := range hrefQuery(raw) {
		for _, v := range vs {
			if v != "" && !strings.Contains(v, "{{") {
				out[k] = v
			}
		}
	}
	return out
}

// MatchKey is Normalize(raw) plus the sorted fixed params ("base?k=v&k=v").
// A template with no fixed params keys exactly as Normalize does.
func MatchKey(raw string) string {
	base := Normalize(raw)
	if base == "" {
		return ""
	}
	fp := FixedParams(raw)
	if len(fp) == 0 {
		return base
	}
	keys := make([]string, 0, len(fp))
	for k := range fp {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + fp[k]
	}
	return base + "?" + strings.Join(parts, "&")
}

// hrefQuery parses the query of a template or rendered href, tolerating
// "&amp;" entities, a fragment, and malformed pairs (kept best-effort).
func hrefQuery(raw string) url.Values {
	s := strings.TrimSpace(raw)
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = s[:i]
	}
	i := strings.IndexByte(s, '?')
	if i < 0 {
		return url.Values{}
	}
	q, _ := url.ParseQuery(strings.ReplaceAll(s[i+1:], "&amp;", "&"))
	return q
}

type indexEntry struct {
	key   string
	fixed map[string]string
}

// Index maps MatchKeys to values and resolves rendered hrefs to one of them.
// Build it fully, then read it; it is not safe for concurrent writes.
type Index struct {
	vals   map[string]string
	byBase map[string][]indexEntry
}

// NewIndex builds an Index from MatchKey -> value. Keys produced by plain
// Normalize (no "?") are valid MatchKeys with no fixed params.
func NewIndex(byKey map[string]string) *Index {
	ix := &Index{vals: byKey, byBase: map[string][]indexEntry{}}
	for k := range byKey {
		base, fixed := k, map[string]string{}
		if i := strings.IndexByte(k, '?'); i >= 0 {
			base = k[:i]
			for _, p := range strings.Split(k[i+1:], "&") {
				if j := strings.IndexByte(p, '='); j > 0 {
					fixed[p[:j]] = p[j+1:]
				}
			}
		}
		ix.byBase[base] = append(ix.byBase[base], indexEntry{key: k, fixed: fixed})
	}
	return ix
}

// Len is the number of keys.
func (ix *Index) Len() int {
	if ix == nil {
		return 0
	}
	return len(ix.vals)
}

// Lookup resolves a rendered href. See the block comment above for the rules.
func (ix *Index) Lookup(href string) (string, bool) {
	if ix == nil {
		return "", false
	}
	cands := ix.byBase[Normalize(href)]
	switch len(cands) {
	case 0:
		return "", false
	case 1:
		return ix.vals[cands[0].key], true
	}
	q := hrefQuery(href)
	best, bestN, tie := "", -1, false
	for _, c := range cands {
		ok := true
		for k, v := range c.fixed {
			if q.Get(k) != v {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		switch n := len(c.fixed); {
		case n > bestN:
			best, bestN, tie = c.key, n, false
		case n == bestN:
			tie = true
		}
	}
	if bestN < 0 || tie {
		return "", false
	}
	return ix.vals[best], true
}
