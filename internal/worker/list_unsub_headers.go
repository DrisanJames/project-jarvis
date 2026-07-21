package worker

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// BuildListUnsubscribeHeaders is THE single shared RFC 8058 header builder for
// every outbound marketing send path — broadcast (PMTA / SES relay / SES direct
// / KumoMTA), click-drip journey reminders, and the API proof-send paths. It
// computes the signed global + brand-scoped unsubscribe URLs and — unless
// disabled — writes the one-click List-Unsubscribe / List-Unsubscribe-Post
// headers into the provided map.
//
// Extracted 2026-07-21 from SendWorkerPool.buildUnsubscribeHeaders (the
// 2026-07-14 SES-relay regression fix) after Google Postmaster flagged the
// sending domains "Not compliant" for missing one-click unsubscribe: the
// pool-method shape meant only processItem could call it, so the click-drip
// journey sender shipped reminders with NO List-Unsubscribe at all, and the
// proof paths hand-rolled an https-only variant with no mailto leg. Every
// caller now emits the identical header pair.
//
// RFC 8058 + Gmail bulk-sender requirements encoded here:
//   - List-Unsubscribe carries BOTH a mailto: leg (domain-aligned with the
//     From address, for ISP trust) and an https: leg (the brand-scoped signed
//     one-click URL that ISPs POST to).
//   - List-Unsubscribe-Post: List-Unsubscribe=One-Click.
//
// The returned URLs are used by callers to resolve in-body {{ system.*_url }}
// tokens, so they are ALWAYS computed — even when the header kill switch is
// set — so a send never ships a literal, unresolved unsubscribe token.
//
// Kill switch: DISABLE_LIST_UNSUB_HEADERS=true skips only the header writes
// (restores the pre-fix behavior) while still returning resolved URLs.
func BuildListUnsubscribeHeaders(orgID, campaignID, subscriberID, brandRoot, fromEmail, trackBase, secret string, headers map[string]string) (unsubURL, brandUnsubURL string) {
	unsubURL = GenerateUnsubscribeURL(orgID, campaignID, subscriberID, trackBase, secret)
	// Brand-scoped URL for the TOP unsubscribe link and the HTTPS leg of
	// List-Unsubscribe. Falls back to the global URL shape when brandRoot is
	// empty (unknown sending domain) so there is no broken-link risk.
	brandUnsubURL = GenerateBrandUnsubscribeURL(orgID, campaignID, subscriberID, brandRoot, trackBase, secret)

	if os.Getenv("DISABLE_LIST_UNSUB_HEADERS") == "true" {
		return unsubURL, brandUnsubURL
	}

	// RFC 8058: both mailto: and https: for maximum ISP compatibility.
	// mailto: must be domain-aligned with the From address for ISP trust.
	// The HTTPS leg uses the brand-scoped URL so ISP one-click POSTs hit the
	// brand suppression path. The mailto leg stays 3-part global — there is no
	// inbound handler for unsub+<token>@<domain> in this repo (the mailto is
	// ceremonial for ISP trust scoring); extending its payload shape would
	// propagate the pre-existing unsigned-mailto bug at a wider scope for zero
	// functional gain.
	fromDomain := fromEmail
	if atIdx := strings.LastIndex(fromEmail, "@"); atIdx >= 0 {
		fromDomain = fromEmail[atIdx+1:]
	}
	unsubData := fmt.Sprintf("%s|%s|%s", orgID, campaignID, subscriberID)
	unsubEncoded := base64.URLEncoding.EncodeToString([]byte(unsubData))
	mailtoAddr := fmt.Sprintf("unsub+%s@%s", unsubEncoded, fromDomain)
	headers["List-Unsubscribe"] = fmt.Sprintf("<mailto:%s?subject=unsubscribe>, <%s>", mailtoAddr, brandUnsubURL)
	headers["List-Unsubscribe-Post"] = "List-Unsubscribe=One-Click"
	return unsubURL, brandUnsubURL
}
