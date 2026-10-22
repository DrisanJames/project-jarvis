package api

import (
	"regexp"
	"strings"
)

// Inbox Score groups campaigns by OFFER (operator 2026-10-22: "I do not care about
// newsletters. Offers only. They should be categorized."). The board cells carry
// mailing_campaigns.offer_key; the nx / Kumo / sidecar offer cells carry the offer
// only in the campaign name, so the label falls back to the name's offer token.

// offerKeyLabel maps the platform's offer keys to the words the operator uses.
var offerKeyLabel = map[string]string{
	"insurhomev1":              "Insurify Home",
	"insurautov1":              "Insurify Auto",
	"empireflooringcpm70offv1": "Empire Today",
	"liberty-mutual":           "Liberty Mutual",
	"iwchelocv4":               "WCL HELOC",
}

// offerTokenLabel maps the offer tokens that appear in campaign names.
var offerTokenLabel = map[string]string{
	"inshome": "Insurify Home", "insauto": "Insurify Auto",
	"ehsw": "eHealth Switchers", "ehealth-switchers": "eHealth Switchers",
	"ehsav": "eHealth Savings", "ehsavings": "eHealth Savings", "ehealth-savings": "eHealth Savings",
	"ehadv": "eHealth Advisor", "ehealth-advisor": "eHealth Advisor",
	"aepsav": "AEP Savings", "aepsw": "AEP Switchers",
	"nyl": "New York Life", "empiretoday": "Empire Today", "liberty": "Liberty Mutual",
	"healthcare": "HealthCare", "heloc": "WCL HELOC", "globe": "Globe Life", "moo": "Mutual of Omaha",
	"destiny": "Destiny", "sams": "Sam's Club", "samsclub": "Sam's Club",
}

var (
	reNewsletter   = regexp.MustCompile(`(?i)NEWSLETTER|(^|[\s-])NL-`)
	reOfferPrefix  = regexp.MustCompile(`(?i)^(KUMO-WARM\s*-\s*)?(OFFER-((ENG|YH|YF|CLK|COLD)-)?|APPLE-WINBACK-)`)
	reOfferSuffix  = regexp.MustCompile(`(?i)(-(D\d+|RAMP|YF|APPLE|CLK|COLD|ENG|TOPUP|\d{4}|Apple-\d{4}|Apple))+$`)
	reReleaseNames = regexp.MustCompile(`(?i)^(AUTO-RELEASE|INTERNAL-AUTO)`)
)

// inboxScoreKind classifies a campaign for the Inbox Score screen.
func inboxScoreKind(name string) string {
	if reNewsletter.MatchString(name) {
		return "newsletter"
	}
	return "offer"
}

// inboxScoreOffer returns the offer label for a campaign: the platform's offer key
// when set, else the offer token in the campaign name, else the name's tail.
func inboxScoreOffer(name, offerKey string) string {
	if k := strings.ToLower(strings.TrimSpace(offerKey)); k != "" {
		if l, ok := offerKeyLabel[k]; ok {
			return l
		}
		return offerKey
	}
	// "<date> - <brand or NX-sub.domain> - <the rest>": everything after the brand segment.
	tail := strings.TrimSpace(name)
	if segs := strings.Split(name, " - "); len(segs) >= 3 {
		tail = strings.TrimSpace(strings.Join(segs[2:], " - "))
	}
	if reReleaseNames.MatchString(tail) {
		return "Internal Auto"
	}
	if strings.HasPrefix(strings.ToUpper(tail), "SRC2-RELEASE") {
		return "SRC2 release"
	}
	tail = reOfferPrefix.ReplaceAllString(tail, "")
	tail = reOfferSuffix.ReplaceAllString(tail, "")
	tail = strings.TrimSpace(strings.Trim(tail, "- "))
	if tail == "" {
		return "Unknown offer"
	}
	key := strings.ToLower(tail)
	if l, ok := offerTokenLabel[key]; ok {
		return l
	}
	// "eHealth-Switchers-Apple-0901" style: first token before a dash that maps
	first := strings.ToLower(strings.SplitN(tail, "-", 2)[0])
	if l, ok := offerTokenLabel[first]; ok {
		return l
	}
	return tail
}
