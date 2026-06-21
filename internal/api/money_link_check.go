package api

// Server-side money-link checker — a Go port of the operator-side Python
// validator core (mailing-saas agents/scheduling/money_link_validator.py). It
// catches dead / mistagged / wrong-brand money URLs in a creative's HTML, with
// an optional out-of-band HTTP liveness follow.
//
// Three send-day footguns this guards against (same as the Python original):
//   - The Warby-class empty-everflow href (<a data-everflow="true" href="">) —
//     a monetized creative that ships with NO destination (zero revenue).
//   - A creative still carrying a KNOWN-DEAD slug (e.g. the jun21 metal-roofing
//     J78S2MD destination): the tracking service salvages already-mailed clicks
//     via deadLinkRemap, but a NEW send must carry the live slug.
//   - Mistagged money URLs — missing source_id/sub1/sub2/sub3, or a sub2= brand
//     domain that doesn't match the campaign brand (Everflow attribution misfires).
//
// The KNOWN-DEAD slug set is derived from the in-package deadLinkRemap slice
// (internal/api/mailing_tracking.go) — the source of truth — so we never parse a
// file at runtime; we reference the slice directly.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
)

// moneyHosts enumerates every affiliate money-network host the platform sends to.
// Mirrors the Python MONEY_HOSTS set. A money href whose host is NOT here is not
// treated as a money URL by the extractor.
var moneyHosts = []string{
	"cratoolpro.com", "eos57ytf.com", "k8k0hfdt.com",
	"xnonu.com", "muqes.com", "jyqye.com",
}

// moneyHostRe matches any money <a href="..."> and captures the full URL. Accepts
// single or double quotes and tolerates other attributes before/after href. The
// host alternation is built from the first label of each moneyHosts entry.
var moneyHostRe = func() *regexp.Regexp {
	labels := make([]string, 0, len(moneyHosts))
	for _, h := range moneyHosts {
		labels = append(labels, strings.Split(h, ".")[0])
	}
	return regexp.MustCompile(
		`(?i)href\s*=\s*["']` +
			`(https?://(?:www\.)?(?:` + strings.Join(labels, "|") + `)\.com/[^"']*)` +
			`["']`)
}()

// anchorTagRe matches a single <a ...> open tag — used to find empty-href
// everflow anchors (the Warby class).
var anchorTagRe = regexp.MustCompile(`(?i)<a\b[^>]*>`)

// slugFromURLRe extracts the offer slug: the path segment AFTER the account id
// (host.com/<acct>/<slug>/...).
var slugFromURLRe = regexp.MustCompile(`(?i)https?://(?:www\.)?[A-Za-z0-9]+\.com/[A-Za-z0-9]+/([A-Za-z0-9]+)`)

// sub2ValueRe pulls the sub2= value (the brand domain) out of a money href.
var sub2ValueRe = regexp.MustCompile(`[?&]sub2=([^&"'\s>]*)`)

// requiredMoneyTags are the tracking params every money href must carry (footgun
// #5 + the sub3 campaign tie). Mirrors the Python _REQUIRED_TAGS tuple.
var requiredMoneyTags = []string{
	"source_id=email",
	"sub1={{subscriber.id}}",
	"sub2=",
	"sub3={{campaign.id}}",
}

// extractMoneyHrefs returns every money-network href in the HTML, in document order.
func extractMoneyHrefs(html string) []string {
	if html == "" {
		return nil
	}
	matches := moneyHostRe.FindAllStringSubmatch(html, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// slugFromMoneyURL extracts the offer slug (segment after the account id).
//
//	cratoolpro.com/BJB4Q5BF/93W8N2N/ -> 93W8N2N
//	eos57ytf.com/K4C5ZLC/PS8241/?x=1 -> PS8241
func slugFromMoneyURL(u string) string {
	if u == "" {
		return ""
	}
	m := slugFromURLRe.FindStringSubmatch(u)
	if m == nil {
		return ""
	}
	return m[1]
}

// sub2Value returns the sub2= value (brand domain) from a money href, or "".
func sub2Value(u string) string {
	m := sub2ValueRe.FindStringSubmatch(u)
	if m == nil {
		return ""
	}
	return m[1]
}

// deadSlugSet derives the KNOWN-DEAD slug set from the in-package deadLinkRemap
// slice (the source of truth). Each entry's .match is a substring like
// "cratoolpro.com/BJB4Q5BF/J78S2MD"; we extract the slug after the account id.
// Returned slugs are upper-cased so lookups are case-insensitive.
func deadSlugSet() map[string]bool {
	out := map[string]bool{}
	for _, m := range deadLinkRemap {
		match := m.match
		if !strings.Contains(match, "://") {
			match = "https://www." + match
		}
		if slug := slugFromMoneyURL(match); slug != "" {
			out[strings.ToUpper(slug)] = true
		}
	}
	return out
}

// emptyEverflowAnchors returns the raw <a> tags that carry data-everflow="true"
// with a non-functional href (empty, whitespace-only, "#", or javascript:). All
// are the same zero-revenue class. Mirrors the Python _empty_everflow_anchors.
func emptyEverflowAnchors(html string) []string {
	if html == "" {
		return nil
	}
	emptyHrefRe := regexp.MustCompile(`(?i)href\s*=\s*["']\s*["']`)
	hrefValRe := regexp.MustCompile(`(?i)href\s*=\s*["']([^"']*)["']`)
	var out []string
	for _, tag := range anchorTagRe.FindAllString(html, -1) {
		low := strings.ToLower(tag)
		if !strings.Contains(low, `data-everflow="true"`) && !strings.Contains(low, `data-everflow='true'`) {
			continue
		}
		if emptyHrefRe.MatchString(tag) {
			out = append(out, tag)
			continue
		}
		if hm := hrefValRe.FindStringSubmatch(tag); hm != nil {
			val := strings.TrimSpace(hm[1])
			if val == "" || val == "#" || strings.HasPrefix(strings.ToLower(val), "javascript:") {
				out = append(out, tag)
			}
		}
	}
	return out
}

// MoneyLinkFinding is one money-link issue. Level ∈ block|warn|ok.
type MoneyLinkFinding struct {
	Level  string `json:"level"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// hasWhitespace reports whether the URL contains an embedded whitespace/newline,
// which slips past the tag check but breaks the live link.
func hasWhitespace(s string) bool {
	for _, c := range s {
		switch c {
		case ' ', '\t', '\n', '\r', '\f', '\v':
			return true
		}
	}
	return false
}

// checkCreativeMoneyLinksLocal runs every LOCAL (no-network) money-link check on
// one creative. An empty slice means the creative is clean. brandRoot is the
// expected sub2= value (apex domain, e.g. "discountblog.com"); pass "" when it
// could not be resolved (sub2 brand match is then surfaced as a warn, not blocked
// — mirroring the Python warn-not-block).
//
// Codes: no_money_link | missing_tags | brand_mismatch | empty_everflow_href
//
//	| stale_dead_slug | brand_unresolved | malformed_href
func checkCreativeMoneyLinksLocal(html, brandRoot string) []MoneyLinkFinding {
	var findings []MoneyLinkFinding

	hrefs := extractMoneyHrefs(html)

	hasAnyMoneyHost := false
	for _, h := range moneyHosts {
		if strings.Contains(html, h) {
			hasAnyMoneyHost = true
			break
		}
	}

	// 1. Empty-href everflow anchor (Warby class) — checked before no_money_link
	//    so the operator sees the real cause.
	empties := emptyEverflowAnchors(html)
	if len(empties) > 0 {
		findings = append(findings, MoneyLinkFinding{
			Level:  "block",
			Code:   "empty_everflow_href",
			Detail: strconv.Itoa(len(empties)) + " data-everflow anchor(s) with empty href (money link did not render — zero revenue)",
		})
	}

	// 2. No money link at all.
	//   - A money host IS referenced but no usable href was captured
	//     (present-but-broken) → BLOCK: the creative meant to monetize and failed.
	//   - No money-network host string anywhere → genuinely non-monetized
	//     (engagement-only creative) → WARN, not block: don't paint it red.
	if len(hrefs) == 0 && hasAnyMoneyHost {
		findings = append(findings, MoneyLinkFinding{
			Level:  "block",
			Code:   "no_money_link",
			Detail: "money network referenced but no valid money href found",
		})
	} else if len(hrefs) == 0 && len(empties) == 0 {
		findings = append(findings, MoneyLinkFinding{
			Level:  "warn",
			Code:   "no_money_link",
			Detail: "no money-network link present (engagement-only creative?)",
		})
	}

	// 3. Per-href tag + brand + dead-slug checks.
	dead := deadSlugSet()
	var badTags, badBrand, deadHit, malformed []string
	for _, h := range hrefs {
		if hasWhitespace(h) {
			slug := slugFromMoneyURL(h)
			if slug == "" {
				slug = "?"
			}
			malformed = append(malformed, slug)
		}
		var missing []string
		for _, t := range requiredMoneyTags {
			if !strings.Contains(h, t) {
				missing = append(missing, t)
			}
		}
		if len(missing) > 0 {
			badTags = append(badTags, slugFromMoneyURL(h)+" missing ["+strings.Join(missing, " ")+"]")
		}
		sub2 := sub2Value(h)
		if sub2 != "" && brandRoot != "" && sub2 != brandRoot {
			badBrand = append(badBrand, slugFromMoneyURL(h)+" sub2='"+sub2+"' != brand '"+brandRoot+"'")
		}
		if slug := slugFromMoneyURL(h); slug != "" && dead[strings.ToUpper(slug)] {
			deadHit = append(deadHit, slug)
		}
	}

	// Brand-mismatch is un-checkable when brandRoot did not resolve. Surface it
	// (warn) rather than silently skipping the sub2 match.
	if len(hrefs) > 0 && brandRoot == "" {
		findings = append(findings, MoneyLinkFinding{
			Level:  "warn",
			Code:   "brand_unresolved",
			Detail: "brand_code did not resolve to a domain — sub2 brand match not verified",
		})
	}

	for _, slug := range malformed {
		findings = append(findings, MoneyLinkFinding{
			Level:  "block",
			Code:   "malformed_href",
			Detail: "money href contains whitespace/newline: " + slug,
		})
	}
	if len(badTags) > 0 {
		findings = append(findings, MoneyLinkFinding{
			Level:  "block",
			Code:   "missing_tags",
			Detail: "money href(s) missing required tracking params: " + strings.Join(badTags, "; "),
		})
	}
	if len(badBrand) > 0 {
		findings = append(findings, MoneyLinkFinding{
			Level:  "block",
			Code:   "brand_mismatch",
			Detail: "money href sub2 != campaign brand domain: " + strings.Join(badBrand, "; "),
		})
	}
	if len(deadHit) > 0 {
		findings = append(findings, MoneyLinkFinding{
			Level:  "block",
			Code:   "stale_dead_slug",
			Detail: "creative ships KNOWN-DEAD slug(s) [" + strings.Join(dedupStrings(deadHit), " ") + "] — rebuild with the live slug (do not rely on the tracking-service remap)",
		})
	}

	return findings
}

// dedupStrings returns the input with duplicate (case-insensitive) values removed,
// preserving first-seen order.
func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		k := strings.ToUpper(s)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	return out
}

// isInternalHost is a string-based SSRF guard: true for loopback / private /
// link-local / cloud-metadata hosts. Intentionally NOT a DNS resolution — a
// simple fail-closed string check blocking the obvious internal targets a
// redirect could point at. Mirrors the Python _is_internal_host.
func isInternalHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	// Strip a bracketed/zoned IPv6 form and any :port suffix for the simple cases.
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if h == "" {
		return true
	}
	switch h {
	case "localhost", "::1", "metadata.google.internal":
		return true
	}
	if strings.HasPrefix(h, "127.") || strings.HasPrefix(h, "10.") ||
		strings.HasPrefix(h, "192.168.") || strings.HasPrefix(h, "169.254.") {
		return true
	}
	// 172.16.0.0 – 172.31.255.255
	if strings.HasPrefix(h, "172.") {
		parts := strings.Split(h, ".")
		if len(parts) >= 2 {
			if n, err := strconv.Atoi(parts[1]); err == nil && n >= 16 && n <= 31 {
				return true
			}
		}
	}
	return false
}

// hostOf returns the lower-cased hostname of a URL with any leading "www.".
func hostOf(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	return strings.TrimPrefix(host, "www.")
}

const moneyFollowUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/124.0 Safari/537.36"

// maxMoneyURLsToFollow bounds how many DISTINCT money URLs the on-demand checker
// will HTTP-follow in a single request, so a creative with many slow money URLs
// can't hang the handler for minutes. URLs beyond the cap are reported (not
// followed) via an http_check_capped warn finding.
const maxMoneyURLsToFollow = 12

// errTooManyRedirects caps the redirect chain.
var errTooManyRedirects = errors.New("stopped after too many redirects")

// httpFollowMoney GETs a money URL (NOT HEAD — smartlinks 200 only on GET),
// follows up to ~6 redirects, and classifies the destination. It refuses any
// final/redirect host that is internal (SSRF guard) and treats it as dead.
//
//	dead       -> 404/410, or a redirect that lands on an internal host
//	transient  -> 5xx, timeout, connection error (NOT dead — retry later)
func httpFollowMoney(ctx context.Context, rawURL string) (status int, finalHost string, dead bool, transient bool, reason string) {
	// Guard the initial target host too — not just redirects.
	if isInternalHost(hostOf(rawURL)) {
		return 0, hostOf(rawURL), true, false, "internal/SSRF host blocked: " + hostOf(rawURL)
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 6 {
				return errTooManyRedirects
			}
			if isInternalHost(req.URL.Hostname()) {
				return errors.New("internal/SSRF host blocked: " + req.URL.Hostname())
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, hostOf(rawURL), false, true, "request build error: " + err.Error()
	}
	req.Header.Set("User-Agent", moneyFollowUA)

	resp, err := client.Do(req)
	if err != nil {
		// Distinguish an SSRF-redirect rejection (dead) from a transient network error.
		if strings.Contains(err.Error(), "internal/SSRF host blocked") {
			return 0, "", true, false, "internal/SSRF host blocked on redirect"
		}
		if errors.Is(err, errTooManyRedirects) {
			return 0, hostOf(rawURL), false, true, "too many redirects"
		}
		return 0, hostOf(rawURL), false, true, "conn error: " + err.Error()
	}
	defer resp.Body.Close()

	finalHost = hostOf(resp.Request.URL.String())
	if isInternalHost(finalHost) {
		return resp.StatusCode, finalHost, true, false, "internal/SSRF host blocked: " + finalHost
	}
	status = resp.StatusCode
	switch {
	case status == 404 || status == 410:
		return status, finalHost, true, false, "http " + strconv.Itoa(status)
	case status >= 500 && status < 600:
		return status, finalHost, false, true, "http " + strconv.Itoa(status)
	}
	return status, finalHost, false, false, "ok"
}

// checkCreativeMoneyLinks combines the LOCAL checks with an optional HTTP liveness
// follow over the distinct money URLs. Overall status:
//
//	"dead" if any local block OR any HTTP-dead;
//	"warn" if any local warn OR any HTTP-transient (but no dead);
//	"pass" otherwise.
//
// detail is a compact human summary suitable for storing in money_link_detail.
func checkCreativeMoneyLinks(ctx context.Context, html, brandRoot string, doHTTP bool) (status string, detail string, findings []MoneyLinkFinding) {
	findings = checkCreativeMoneyLinksLocal(html, brandRoot)

	anyBlock := false
	anyWarn := false
	for _, f := range findings {
		switch f.Level {
		case "block":
			anyBlock = true
		case "warn":
			anyWarn = true
		}
	}

	anyDead := false
	anyTransient := false
	if doHTTP {
		// Dedupe the distinct money URLs before following.
		seen := map[string]bool{}
		distinct := make([]string, 0)
		for _, h := range extractMoneyHrefs(html) {
			if seen[h] {
				continue
			}
			seen[h] = true
			distinct = append(distinct, h)
		}

		// Cap the number of distinct URLs HTTP-followed; report the overflow.
		toFollow := distinct
		if len(distinct) > maxMoneyURLsToFollow {
			toFollow = distinct[:maxMoneyURLsToFollow]
			anyWarn = true
			findings = append(findings, MoneyLinkFinding{
				Level:  "warn",
				Code:   "http_check_capped",
				Detail: strconv.Itoa(len(distinct)-maxMoneyURLsToFollow) + " of " + strconv.Itoa(len(distinct)) +
					" distinct money URLs were NOT HTTP-followed (capped at " + strconv.Itoa(maxMoneyURLsToFollow) + ")",
			})
		}

		// Bound the WHOLE HTTP-follow phase so even with the cap the check can't
		// exceed ~25s. Derived from the request context so a client cancel/timeout
		// still propagates.
		httpCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		for _, h := range toFollow {
			st, finalHost, dead, transient, reason := httpFollowMoney(httpCtx, h)
			slug := slugFromMoneyURL(h)
			if dead {
				anyDead = true
				findings = append(findings, MoneyLinkFinding{
					Level:  "block",
					Code:   "http_dead",
					Detail: "money URL dead (slug " + slug + ", http " + strconv.Itoa(st) + ", host " + finalHost + "): " + reason,
				})
			} else if transient {
				anyTransient = true
				findings = append(findings, MoneyLinkFinding{
					Level:  "warn",
					Code:   "http_transient",
					Detail: "money URL transient (slug " + slug + "): " + reason,
				})
			}
		}
		cancel()
	}

	switch {
	case anyBlock || anyDead:
		status = "dead"
	case anyWarn || anyTransient:
		status = "warn"
	default:
		status = "pass"
	}
	detail = summarizeFindings(status, findings)
	return status, detail, findings
}

// summarizeFindings builds a compact one-line summary of the findings for storage.
func summarizeFindings(status string, findings []MoneyLinkFinding) string {
	if len(findings) == 0 {
		return status + ": all money links clean"
	}
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		parts = append(parts, f.Code)
	}
	return status + ": " + strings.Join(parts, ", ")
}

// brandRootFromCode resolves a creative's brand_code to its apex brand root (the
// expected sub2= value). brand_code is stored two ways in mailing_creatives:
//   - the forge-sync path stores the uppercase brand CODE (DB, RRU, HT, ...);
//   - the Creative Studio path stores a display-name slug ("discount-blog").
//
// We resolve in this order: (1) the canonical code→root map; (2) try the code
// itself as a sending domain via brand.Root; (3) collapse the slug ("discount-blog"
// -> "discountblog") and match the base label of an OwnedDomains entry. Returns ""
// when unresolved — the local checker then warns (not blocks) on sub2 mismatch.
func brandRootFromCode(brandCode string) string {
	code := strings.TrimSpace(brandCode)
	if code == "" {
		return ""
	}
	if root := brandCodeRoot[strings.ToUpper(code)]; root != "" {
		return root
	}
	// brand.Root returns the input unchanged for an unknown domain, so only
	// accept its result when it is an OwnedDomains apex (covers both a bare apex
	// like "quizfiesta.com" and a sending domain like "em.discountblog.com" that
	// collapses to "discountblog.com").
	lc := strings.ToLower(code)
	if r := brand.Root(lc); isOwnedBrandRoot(r) {
		return r
	}
	// Studio slug form: strip non-alphanumerics and match the base label of an
	// owned domain (discount-blog -> discountblog -> discountblog.com).
	collapsed := nonAlnumRe.ReplaceAllString(lc, "")
	if collapsed != "" {
		for _, od := range brand.OwnedDomains {
			base := strings.Split(od, ".")[0]
			if base == collapsed {
				return od
			}
		}
	}
	return ""
}

var nonAlnumRe = regexp.MustCompile(`[^a-z0-9]+`)

// isOwnedBrandRoot reports whether root is one of brand.OwnedDomains (an apex we
// operate). brand.Root returns its input unchanged for unknown domains, so this
// distinguishes "resolved to an owned apex" from "passed through unknown".
func isOwnedBrandRoot(root string) bool {
	for _, od := range brand.OwnedDomains {
		if od == root {
			return true
		}
	}
	return false
}

// brandCodeRoot maps the canonical uppercase brand codes (brand_metadata.py
// BRAND_REGISTRY keys) to their apex brand root. Kept in sync with
// internal/pkg/brand.OwnedDomains and brand_metadata.py.
var brandCodeRoot = map[string]string{
	"DB": "discountblog.com",
	"HT": "historythinking.com",
	"MH": "myownhealth.net",
	"QF": "quizfiesta.com",
	"BW": "businessweeklypro.com",
	"FC": "financialcalculate.com",
	"CP": "consumerpro.net",
	"HW": "homewarrantyservices.org",
	"RR": "refinanceratesusa.com",
	"TT": "thingoftheday.org",
	"YI": "yourinsurancehub.com",
	"MR": "myrepairdiy.com",
	"CI": "casainsure.com",
	"LP": "learnpersonalloans.com",
	"RB": "ratesbazar.com",
	"WF": "warrantyforyou.com",
	"MP": "mypersonalfinancial.com",
	"PD": "paymydebit.com",
	"TR": "theretirementblog.com",
}
