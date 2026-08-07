package worker

// Systematic partner-offer disclosure (operator 2026-08-07): every dispatched
// email opens with a sender-identification header and closes with an
// intended-recipient + unsubscribe footer. Both are injected AT SEND TIME with
// concrete, already-rendered strings — never merge tokens. The prior attempt
// baked '{{ brand.name }}' blocks into stored creatives, and the click-drip
// path (which doesn't render that token) shipped them raw to inboxes on
// 2026-08-04.
//
// Exemptions:
//   - raw_creative sending profiles (first-party mail, e.g. em.wcl-heloc.com)
//     — enforced by the CALLERS, matching the existing compliance-footer
//     bypass semantics.
//   - creatives that still carry a legacy baked-in "Partner offer sent by"
//     block — the header injector skips them so a subscriber never sees the
//     disclosure twice. Once the stored-creative cleanup strips those blocks,
//     injection takes over automatically.
//
// Kill switch: DISABLE_PARTNER_DISCLOSURE=true suppresses both injections
// without a rollback deploy; the pre-existing CAN-SPAM unsubscribe fallback
// then resumes covering the unsub-link requirement.

import (
	"fmt"
	"html"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	partnerDisclosureMarker = `<!-- jv-partner-disclosure -->`
	intendedForMarker       = `<!-- jv-intended-for -->`
	// legacyDisclosurePhrase detects operator-era baked-in disclosure blocks
	// (rendered or raw-token) so injection never doubles them.
	legacyDisclosurePhrase = "partner offer sent by"
)

var bodyOpenTagRe = regexp.MustCompile(`(?i)<body[^>]*>`)

func partnerDisclosureDisabled() bool {
	return strings.EqualFold(os.Getenv("DISABLE_PARTNER_DISCLOSURE"), "true")
}

// injectPartnerDisclosureHeader inserts the approved sender-identification
// header immediately after the opening <body> tag (prepended when the
// creative has no <body>). No-ops when disabled, when brandLabel is empty,
// or when the creative already carries a disclosure (marker or legacy baked
// block) — making the call idempotent.
func injectPartnerDisclosureHeader(htmlContent, brandLabel string) string {
	if partnerDisclosureDisabled() || strings.TrimSpace(brandLabel) == "" {
		return htmlContent
	}
	if strings.Contains(strings.ToLower(htmlContent), legacyDisclosurePhrase) {
		return htmlContent
	}
	label := html.EscapeString(brandLabel)
	block := partnerDisclosureMarker +
		`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="border-collapse:collapse;background-color:#ffffff;">` +
		`<tr><td align="center" style="padding:6px 16px 5px;background-color:#ffffff;">` +
		`<div style="font-family:Arial,Helvetica,sans-serif;font-size:9px;line-height:13px;color:#9b9b9b;max-width:600px;">` +
		`Partner offer sent by <span style="color:#6b6b6b;">` + label + `</span>. You subscribed to ` + label + ` partner promotions.` +
		`</div></td></tr></table>`
	if loc := bodyOpenTagRe.FindStringIndex(htmlContent); loc != nil {
		return htmlContent[:loc[1]] + block + htmlContent[loc[1]:]
	}
	return block + htmlContent
}

// injectIntendedForFooter inserts the intended-recipient + date + unsubscribe
// line just before </body> (appended when the creative has no </body>).
// No-ops when disabled, already present, or when email/unsubURL is empty —
// a footer without a working unsubscribe link is worse than the existing
// CAN-SPAM fallback, which remains in place for that case.
func injectIntendedForFooter(htmlContent, email, unsubURL string, now time.Time) string {
	if partnerDisclosureDisabled() || strings.TrimSpace(email) == "" || strings.TrimSpace(unsubURL) == "" {
		return htmlContent
	}
	if strings.Contains(htmlContent, intendedForMarker) {
		return htmlContent
	}
	block := intendedForMarker +
		`<div style="text-align:center;padding:12px 16px 16px;font-size:9px;line-height:13px;color:#9b9b9b;font-family:Arial,Helvetica,sans-serif;">` +
		`This email was intended for ` + html.EscapeString(email) + `, ` + now.Format("January 2, 2006") + ` ` +
		`<a href="` + unsubURL + `" style="color:#9b9b9b;text-decoration:underline;">unsubscribe</a></div>`
	if idx := strings.LastIndex(strings.ToLower(htmlContent), "</body>"); idx >= 0 {
		return htmlContent[:idx] + block + htmlContent[idx:]
	}
	return htmlContent + block
}

// disclosureTextParts returns the text/plain equivalents (header line, footer
// line) for multipart parity on senders that build the text part separately
// from the final HTML. Empty strings mean "skip" under the same rules as the
// HTML injectors.
func disclosureTextParts(brandLabel, email, unsubURL string, now time.Time) (header, footer string) {
	if partnerDisclosureDisabled() {
		return "", ""
	}
	if strings.TrimSpace(brandLabel) != "" {
		header = fmt.Sprintf("Partner offer sent by %s. You subscribed to %s partner promotions.", brandLabel, brandLabel)
	}
	if strings.TrimSpace(email) != "" && strings.TrimSpace(unsubURL) != "" {
		footer = fmt.Sprintf("This email was intended for %s, %s. Unsubscribe: %s", email, now.Format("January 2, 2006"), unsubURL)
	}
	return header, footer
}
