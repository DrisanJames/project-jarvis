package isp

import "strings"

// Known ISP group names returned by Group and GroupFromDomain.
const (
	Gmail      = "gmail"
	Yahoo      = "yahoo"
	Microsoft  = "microsoft"
	Apple      = "apple"
	Comcast    = "comcast"
	Charter    = "charter"
	ATT        = "att"
	Cox        = "cox"
	Verizon    = "verizon"
	Protonmail = "protonmail"
	Zoho       = "zoho"
	Other      = "other"
)

// domainToISP is the canonical domain-to-ISP classification map.
// All consumers in the codebase (analytics, sending pipeline, campaign
// planning, rate registry) MUST use this map via Group/GroupFromDomain.
var domainToISP = map[string]string{
	// Gmail / Google
	"gmail.com":      Gmail,
	"googlemail.com": Gmail,

	// Yahoo / Oath family
	"yahoo.com":      Yahoo,
	"ymail.com":      Yahoo,
	"rocketmail.com": Yahoo,
	"yahoo.ca":       Yahoo,
	"yahoo.co.uk":    Yahoo,
	"yahoo.co.jp":    Yahoo,
	"aol.com":        Yahoo,
	"aim.com":        Yahoo,

	// Microsoft
	"outlook.com": Microsoft,
	"hotmail.com": Microsoft,
	"live.com":    Microsoft,
	"msn.com":     Microsoft,

	// Apple
	"icloud.com": Apple,
	"me.com":     Apple,
	"mac.com":    Apple,

	// AT&T
	"att.net":       ATT,
	"sbcglobal.net": ATT,
	"bellsouth.net": ATT,

	// Comcast / Xfinity
	"comcast.net": Comcast,
	"xfinity.com": Comcast,

	// Charter / Spectrum
	"charter.net":    Charter,
	"spectrum.net":   Charter,
	"rr.com":         Charter,
	"roadrunner.com": Charter,
	"twc.com":        Charter,
	"brighthouse.com": Charter,

	// Cox
	"cox.net": Cox,

	// Verizon
	"verizon.net": Verizon,

	// Protonmail
	"protonmail.com": Protonmail,
	"proton.me":      Protonmail,

	// Zoho
	"zoho.com": Zoho,
}

// ispToPoolSuffix maps ISP group names to PMTA pool name suffixes.
// ISPs without a dedicated pool route to "general".
var ispToPoolSuffix = map[string]string{
	Gmail:     "gmail",
	Yahoo:     "yahoo",
	Microsoft: "msft",
	Apple:     "apple",
	Comcast:   "comcast",
	ATT:       "att",
	Cox:       "cox",
	Charter:   "charter",
}

// Group returns the ISP group name for an email address.
// Returns "other" for unrecognized domains or malformed addresses.
func Group(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at == len(email)-1 {
		return Other
	}
	return GroupFromDomain(email[at+1:])
}

// GroupFromDomain returns the ISP group name for a bare domain.
func GroupFromDomain(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	if g, ok := domainToISP[d]; ok {
		return g
	}
	if strings.HasSuffix(d, ".rr.com") {
		return Charter
	}
	return Other
}

// PoolSuffix returns the PMTA pool name suffix for a given ISP group.
// For example, PoolSuffix("microsoft") returns "msft".
// Unmapped ISPs (verizon, protonmail, zoho, other, etc.) return "general".
func PoolSuffix(isp string) string {
	if suffix, ok := ispToPoolSuffix[isp]; ok {
		return suffix
	}
	return "general"
}

// KnownGroups returns the list of recognized ISP group names
// (only the 8 major groups with dedicated pool routing).
func KnownGroups() []string {
	return []string{Gmail, Yahoo, Microsoft, Apple, Comcast, Charter, ATT, Cox}
}

// AllGroups returns every ISP group name including minor ones.
func AllGroups() []string {
	return []string{Gmail, Yahoo, Microsoft, Apple, Comcast, Charter, ATT, Cox, Verizon, Protonmail, Zoho}
}
