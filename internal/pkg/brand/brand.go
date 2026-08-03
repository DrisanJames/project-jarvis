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
	// May 9 2026 brand expansion — IPXO-hosted PMTA sending domains.
	// Same pattern as the May 8 batch; tracking on t.em.<apex>, sending
	// from em.<apex>, brand-scoped suppression rooted at the apex.
	"myrepairdiy.com",
	"casainsure.com",
	"learnpersonalloans.com",
	"ratesbazar.com",
	// May 9 2026 (B) — single-brand follow-up expansion (Server B donor: mh-*).
	"warrantyforyou.com",
	// Jun 2026 KumoMTA Colo1 expansion — sending from em.<apex> via the
	// KumoMTA injector (40.160.129.116), dedicated egress IPs 51.81.135.220-222
	// in warmup. Server/pools/IPs/profiles seeded in runStartupMigrations
	// (seed_kumo_*); routing_mode='kumo' on each profile.
	"mypersonalfinancial.com",
	"paymydebit.com",
	"theretirementblog.com",
	// Jun 2026 KumoMTA Colo1 ISP-pool expansion — 5 new brands on the IPXO
	// /24 (16.217.96.0/24), ISP-pooled (2 IPs/ISP), routing_mode='kumo'.
	"bestcreditcare.com",
	"us-finance.com",
	"yourfinancialblog.com",
	"homeloansbyjaime.com",
	"firsttimebuyerhomeloan.com",
	// Jun 2026 — additional Kumo brand (htm), IPs 16.217.96.186-207.
	"hometracmortgage.com",
	// Jul 2026 — first-party SES-routed domain (opted-in audience native to
	// the domain; SES identity em.wcl-heloc.com per
	// docs/RUNBOOK-FIRST-PARTY-SES-DOMAIN.md). Profile seeded as draft in
	// runStartupMigrations (seed_pmta_wclheloc_ses_tenant_profile).
	"wcl-heloc.com",
}

// labels maps each brand root to its recipient-facing display name, used by
// the {{ brand.name }} Liquid merge tag (partner-offer disclosures, footers).
// Keep in sync with agents/registry/brand_metadata.py brand_label.
var labels = map[string]string{
	"discountblog.com":           "Discount Blog",
	"quizfiesta.com":             "Quiz Fiesta",
	"historythinking.com":        "History Thinking",
	"myownhealth.net":            "My Own Health",
	"getmecoupons.net":           "Get Me Coupons",
	"businessweeklypro.com":      "Business Weekly Pro",
	"financialcalculate.com":     "Financial Calculate",
	"consumerpro.net":            "Consumer Pro",
	"homewarrantyservices.org":   "Home Warranty Services",
	"refinanceratesusa.com":      "Refinance Rates USA",
	"thingoftheday.org":          "Thing of the Day",
	"yourinsurancehub.com":       "Your Insurance Hub",
	"myrepairdiy.com":            "My Repair DIY",
	"casainsure.com":             "Casa Insure",
	"learnpersonalloans.com":     "Learn Personal Loans",
	"ratesbazar.com":             "Rates Bazar",
	"warrantyforyou.com":         "Warranty For You",
	"mypersonalfinancial.com":    "MyPersonalFinancial",
	"paymydebit.com":             "PayMyDebit",
	"theretirementblog.com":      "The Retirement Blog",
	"bestcreditcare.com":         "Best Credit Care",
	"us-finance.com":             "US Finance",
	"yourfinancialblog.com":      "Your Financial Blog",
	"homeloansbyjaime.com":       "Home Loans by Jaime",
	"firsttimebuyerhomeloan.com": "First Time Buyer Home Loan",
	"hometracmortgage.com":       "HomeTrac Mortgage",
	"wcl-heloc.com":              "West Capital Lending",
}

// Label maps a sending domain (or brand root) to the brand's recipient-facing
// display name: em.quizfiesta.com -> "Quiz Fiesta". Unknown domains fall back
// to their brand root verbatim (truthful, never empty for non-empty input).
func Label(sendingDomain string) string {
	root := Root(sendingDomain)
	if l, ok := labels[root]; ok {
		return l
	}
	return root
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
	for _, od := range Domains() {
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
