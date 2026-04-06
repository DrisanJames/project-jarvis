package isp

import (
	"sort"
	"strings"
)

// Known ISP group names returned by Group and GroupFromDomain.
//
// These constants are the single source of truth for ISP classification
// across the entire platform: analytics, campaign planner, wave builder,
// sending pipeline, rate registry, and throttle engine all consume them.
//
// AOL is intentionally separate from Yahoo. Although AOL mail is hosted on
// Yahoo infrastructure, we route them through independent sending pools so
// that reputation issues with one domain family (e.g., yahoo.com TSS04
// deferrals) do not block volume to the other (aol.com, aim.com). This
// also enables independent quota and wave cadence control per ISP.
//
// SBC Global/BellSouth is intentionally separate from AT&T. Although these
// legacy domains route through AT&T MX infrastructure, isolating them into
// their own pool enables independent reputation tracking and quota control.
const (
	Gmail      = "gmail"
	Yahoo      = "yahoo"
	Aol        = "aol"
	Microsoft  = "microsoft"
	Apple      = "apple"
	Comcast    = "comcast"
	Charter    = "charter"
	ATT        = "att"
	Sbcglobal  = "sbcglobal"
	Cox        = "cox"
	Verizon    = "verizon"
	Protonmail = "protonmail"
	Zoho       = "zoho"
	Other      = "other"
)

// domainToISP is the canonical domain-to-ISP classification map.
//
// Every consumer in the codebase — analytics SQL, sending pipeline, campaign
// planning, rate registry, data pipeline — MUST use this map exclusively via
// Group() or GroupFromDomain(). Direct domain-to-ISP comparisons elsewhere
// are a bug waiting to diverge.
//
// Design decision: aol.com and aim.com are classified as "aol" (not "yahoo")
// to enable separate sending pools, quotas, and wave cadences. Yahoo and AOL
// share MX infrastructure, but their reputation signals are evaluated per
// source IP — isolating them lets us protect healthy AOL delivery when Yahoo
// proper is in backoff (TSS04), and vice versa.
var domainToISP = map[string]string{
	// Gmail / Google
	"gmail.com":      Gmail,
	"googlemail.com": Gmail,

	// Yahoo proper — yahoo.com and its international/legacy aliases.
	// Does NOT include AOL domains; those are classified separately below.
	"yahoo.com":      Yahoo,
	"ymail.com":      Yahoo,
	"rocketmail.com": Yahoo,
	"myyahoo.com":    Yahoo,
	"yahoo.ca":       Yahoo,
	"yahoo.co.uk":    Yahoo,
	"yahoo.co.in":    Yahoo,
	"yahoo.com.au":   Yahoo,
	"yahoo.co.jp":    Yahoo,

	// AOL — separated from Yahoo for independent pool routing and quota control.
	// AOL mail is hosted on Yahoo MX infrastructure, but reputation is tracked
	// per source IP, so isolating the sending pool prevents cross-contamination.
	"aol.com": Aol,
	"aim.com": Aol,

	// Microsoft — all Outlook/Hotmail/Live/MSN domains unified under one ISP.
	"outlook.com":   Microsoft,
	"outlook.co.uk": Microsoft,
	"hotmail.com":   Microsoft,
	"hotmail.co.uk": Microsoft,
	"hotmail.fr":    Microsoft,
	"live.com":      Microsoft,
	"live.co.uk":    Microsoft,
	"msn.com":       Microsoft,

	// Apple
	"icloud.com": Apple,
	"me.com":     Apple,
	"mac.com":    Apple,

	// AT&T
	"att.net": ATT,

	// SBC Global / BellSouth — separated from AT&T for independent pool routing
	// and quota control. These legacy domains route through AT&T MX infrastructure,
	// but reputation is tracked per source IP, so isolating the sending pool
	// prevents cross-contamination (same rationale as AOL vs Yahoo).
	"sbcglobal.net": Sbcglobal,
	"bellsouth.net": Sbcglobal,
	"pacbell.net":   Sbcglobal,
	"swbell.net":    Sbcglobal,
	"ameritech.net": Sbcglobal,
	"nvbell.net":    Sbcglobal,

	// Comcast / Xfinity
	"comcast.net": Comcast,
	"xfinity.com": Comcast,

	// Charter / Spectrum — includes all Time Warner Cable legacy domains.
	"charter.net":    Charter,
	"spectrum.net":   Charter,
	"rr.com":         Charter,
	"roadrunner.com": Charter,
	"twc.com":        Charter,
	"brighthouse.com": Charter,

	// Cox
	"cox.net": Cox,

	// Verizon — note: verizon.net routes to "general" pool (no dedicated pool).
	// Verizon email is low volume and does not warrant a dedicated sending pool.
	"verizon.net": Verizon,

	// Protonmail
	"protonmail.com": Protonmail,
	"proton.me":      Protonmail,

	// Zoho
	"zoho.com": Zoho,
}

// ispToPoolSuffix maps ISP group names to PMTA pool name suffixes.
//
// Pool names follow the pattern: {brand_prefix}-{suffix}-pool
// Example: "db-aol-pool" for Discount Blog's AOL sending pool.
//
// ISPs without a dedicated pool (verizon, protonmail, zoho, other, etc.)
// fall through to "general", which maps to {brand_prefix}-general-pool.
var ispToPoolSuffix = map[string]string{
	Gmail:     "gmail",
	Yahoo:     "yahoo",
	Aol:       "aol",
	Microsoft: "msft",
	Apple:     "apple",
	Comcast:   "comcast",
	ATT:       "att",
	Sbcglobal: "sbcglobal",
	Cox:       "cox",
	Charter:   "charter",
}

// Group returns the ISP group name for an email address.
// Returns "other" for unrecognized domains or malformed addresses.
//
// This is the primary entry point for ISP classification. All code that
// needs to know "which ISP does this email belong to?" should call Group().
func Group(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at == len(email)-1 {
		return Other
	}
	return GroupFromDomain(email[at+1:])
}

// GroupFromDomain returns the ISP group name for a bare domain (no @ prefix).
//
// Called by Group() after extracting the domain, and also used directly when
// only the domain portion is available (e.g., tracking events store
// recipient_domain separately).
func GroupFromDomain(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	if g, ok := domainToISP[d]; ok {
		return g
	}
	// Wildcard match for Charter/Spectrum regional subdomains (e.g., nyc.rr.com).
	if strings.HasSuffix(d, ".rr.com") {
		return Charter
	}
	return Other
}

// PoolSuffix returns the PMTA pool name suffix for a given ISP group.
//
// Used by the send worker to resolve which DB pool to pull IPs from:
//   vmtaPool.next(recipientISP) → ISPPoolSuffix(isp) → "{prefix}-{suffix}-pool"
//
// Example: PoolSuffix("aol") returns "aol" → pool name "db-aol-pool".
// Unmapped ISPs return "general" → pool name "db-general-pool".
func PoolSuffix(isp string) string {
	if suffix, ok := ispToPoolSuffix[isp]; ok {
		return suffix
	}
	return "general"
}

// KnownGroups returns the 10 major ISP groups that have dedicated sending pools.
//
// Used by analytics aggregation to enumerate all ISP buckets when building
// per-ISP breakdowns. If you add a new ISP with a dedicated pool, add it here.
func KnownGroups() []string {
	return []string{Gmail, Yahoo, Aol, Microsoft, Apple, Comcast, Charter, ATT, Sbcglobal, Cox}
}

// AllGroups returns every ISP group name including minor ones that route
// through the "general" pool (verizon, protonmail, zoho).
func AllGroups() []string {
	return []string{Gmail, Yahoo, Aol, Microsoft, Apple, Comcast, Charter, ATT, Sbcglobal, Cox, Verizon, Protonmail, Zoho}
}

// DomainsForGroup returns all domains classified under the given ISP group.
// Used by throttle and analytics layers that need domain lists per ISP.
func DomainsForGroup(group string) []string {
	var domains []string
	for domain, g := range domainToISP {
		if g == group {
			domains = append(domains, domain)
		}
	}
	sort.Strings(domains)
	return domains
}
