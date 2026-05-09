// Package brand resolves sending domains (e.g. em.discountblog.com,
// m.discountblog.com) to their brand root (discountblog.com).
//
// The brand root is the unit of scope for brand-scoped unsubscribe:
// when a subscriber clicks the TOP unsubscribe link on an email from
// em.discountblog.com, they are suppressed for the entire discountblog.com
// brand (including m.discountblog.com) but remain mailable for other
// brands such as quizfiesta.com.
//
// OwnedDomains is the authoritative registry. Adding a new sending brand
// means adding a line here and to the click-tracker's rewriter.
package brand

import "strings"

// OwnedDomains enumerates the brand roots we operate. Any sending domain
// whose base matches an entry here is considered that brand; otherwise
// the input is treated as its own brand root.
//
// Adding a new sending brand:
//  1. Add the apex domain here (lowercase, no leading dot).
//  2. Ensure mailing_sending_profiles row(s) for em.<apex> exist.
//  3. mailing_image_domains row optional — only required for inline image
//     rehosting via that brand's CDN; otherwise images fall back to the
//     org-default image domain (img.projectjarvis.io).
var OwnedDomains = []string{
	"discountblog.com",
	"quizfiesta.com",
	"historythinking.com",
	"myownhealth.net",
	"getmecoupons.net",
	// May 2026 brand expansion — IPXO-hosted PMTA sending domains.
	// All seven launched 2026-05-08; tracking on t.em.<apex>, sending
	// from em.<apex>, brand-scoped suppression rooted at the apex.
	"businessweeklypro.com",
	"financialcalculate.com",
	"consumerpro.net",
	"homewarrantyservices.org",
	"refinanceratesusa.com",
	"thingoftheday.org",
	"yourinsurancehub.com",
}

// Root maps a sending domain to its brand root.
//
//	em.discountblog.com -> discountblog.com
//	m.discountblog.com  -> discountblog.com
//	DISCOUNTBLOG.COM    -> discountblog.com
//	unknown.io          -> unknown.io (treated as its own brand)
//	""                  -> ""
//
// For any domain not on OwnedDomains the input is returned unchanged.
// We own the brand list, so "unknown" means "treat as its own brand"
// (defensive — never collapse two unrelated unknowns together).
func Root(sendingDomain string) string {
	d := strings.ToLower(strings.TrimSpace(sendingDomain))
	if d == "" {
		return ""
	}
	for _, od := range OwnedDomains {
		if d == od || strings.HasSuffix(d, "."+od) {
			return od
		}
	}
	return d
}

// RootFromEmail extracts the brand root from a full email address
// ("news@em.discountblog.com" -> "discountblog.com"). Returns empty
// string for malformed input.
func RootFromEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return Root(email[at+1:])
}
