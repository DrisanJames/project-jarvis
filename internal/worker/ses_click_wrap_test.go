package worker

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

// These tests pin the SES click-wrap restore (2026-07-13): SES silently
// declines to click-wrap PMTA-DKIM-signed mail (15 of 16 brands), so the
// via_ses branch's historical "SES is the sole tracker" skip left money
// links unwrapped and clicks invisible. RewriteClickLinks is the links-only
// injector factored from InjectTrackingPixelAndLinks; applySESTracking is
// the via_ses branch body (open pixel + click wrap, each behind its own
// kill switch).

const (
	clickTestBase   = "https://t.em.discountblog.com"
	clickTestOrg    = "org-1"
	clickTestSecret = "test-secret"
)

// wrappedClickRe extracts the data + sig segments of a wrapped click link.
var wrappedClickRe = regexp.MustCompile(`href="` + clickTestBase + `/track/click/([^/"]+)/([^/"]+)"`)

// resolveWrappedClick mirrors EXACTLY how the tracking service resolves a
// t.em click link (internal/tracking/handler.go HandleClick): base64-decode
// {data}, split on "|", require >=5 parts, redirect to parts[4]. The wrapped
// URL is only correct if that resolution yields the full original money URL.
func resolveWrappedClick(t *testing.T, html string) (orgID, campaignID, subscriberID, emailID, originalURL string) {
	t.Helper()
	m := wrappedClickRe.FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("no wrapped click link found in html:\n%s", html)
	}
	decoded, err := base64.URLEncoding.DecodeString(m[1])
	if err != nil {
		t.Fatalf("token does not base64-decode (handler would 400): %v", err)
	}
	parts := strings.Split(string(decoded), "|")
	if len(parts) < 5 {
		t.Fatalf("token has %d parts, handler requires >=5: %q", len(parts), decoded)
	}
	if got := TrackSign(m[1], clickTestSecret); got != m[2] {
		t.Fatalf("signature mismatch: link carries %q, TrackSign yields %q", m[2], got)
	}
	return parts[0], parts[1], parts[2], parts[3], parts[4]
}

func TestRewriteClickLinks_WrapsMoneyLinkAndPreservesFullURL(t *testing.T) {
	// Everflow money URL with attribution params — the CRITICAL case: the
	// redirect must deliver source_id, sub1 and sub2 byte-for-byte.
	money := "https://www.cratoolpro.com/BJB4Q5BF/ABC123/?source_id=email&sub1={{subscriber.id}}&sub2=discountblog.com"
	html := `<html><body><a href="` + money + `">CTA</a></body></html>`

	out := RewriteClickLinks(html, "camp-1", "sub-1", "email-1", clickTestBase, clickTestOrg, clickTestSecret)

	if strings.Contains(out, `href="`+money+`"`) {
		t.Fatalf("money link left unwrapped")
	}
	org, camp, sub, email, orig := resolveWrappedClick(t, out)
	if org != clickTestOrg || camp != "camp-1" || sub != "sub-1" || email != "email-1" {
		t.Fatalf("token identity wrong: org=%q camp=%q sub=%q email=%q", org, camp, sub, email)
	}
	if orig != money {
		t.Fatalf("redirect target corrupted:\n want %q\n got  %q", money, orig)
	}
}

func TestRewriteClickLinks_MatchesFullInjectorTokenFormat(t *testing.T) {
	// The factored links-only injector must emit byte-identical wrapped
	// links to InjectTrackingPixelAndLinks (same token, same signature) so
	// the tracking service cannot tell the paths apart.
	html := `<html><body><a href="https://example.com/offer">CTA</a></body></html>`
	full := InjectTrackingPixelAndLinks(html, "c", "s", "e", clickTestBase, clickTestOrg, clickTestSecret)
	linksOnly := RewriteClickLinks(html, "c", "s", "e", clickTestBase, clickTestOrg, clickTestSecret)

	fullLink := wrappedClickRe.FindString(full)
	linksOnlyLink := wrappedClickRe.FindString(linksOnly)
	if fullLink == "" || linksOnlyLink == "" {
		t.Fatalf("wrapped link missing: full=%q linksOnly=%q", fullLink, linksOnlyLink)
	}
	if fullLink != linksOnlyLink {
		t.Fatalf("token drift between injectors:\n full      %q\n linksOnly %q", fullLink, linksOnlyLink)
	}
}

func TestRewriteClickLinks_SkipRules(t *testing.T) {
	unsub := clickTestBase + "/track/unsubscribe/dG9rZW4=/sig"
	html := `<html><body>` +
		`<a href="` + unsub + `">Unsubscribe</a>` +
		`<a href="mailto:hi@discountblog.com">Mail us</a>` +
		`<a href="#top">Back to top</a>` +
		`<a href="/preferences">Prefs</a>` +
		`<a href="tel:+15551234567">Call</a>` +
		`<a href="https://example.com/offer?sub1=x">Money</a>` +
		`</body></html>`

	out := RewriteClickLinks(html, "c", "s", "e", clickTestBase, clickTestOrg, clickTestSecret)

	// Preserved skip rules (send_worker.go linkRe + the /track/ + mailto:
	// substring guards): unsubscribe, mailto, anchors, relative, tel.
	for _, keep := range []string{
		`href="` + unsub + `"`,
		`href="mailto:hi@discountblog.com"`,
		`href="#top"`,
		`href="/preferences"`,
		`href="tel:+15551234567"`,
	} {
		if !strings.Contains(out, keep) {
			t.Errorf("skip rule violated, link was rewritten: %s", keep)
		}
	}
	// Exactly the one absolute http(s) money link is wrapped.
	if got := strings.Count(out, "/track/click/"); got != 1 {
		t.Errorf("want exactly 1 wrapped link, got %d", got)
	}
	// Links-only: it must NEVER add a pixel (that is InjectOpenPixel's job).
	if strings.Contains(out, "/track/open/") {
		t.Errorf("RewriteClickLinks must not inject an open pixel")
	}
}

func TestApplySESTracking_Default_WrapsLinksAndInjectsPixelOnce(t *testing.T) {
	t.Setenv("DISABLE_SES_OPEN_PIXEL", "")
	t.Setenv("DISABLE_SES_CLICK_WRAP", "")

	money := "https://www.codefortwo.com/K4C5ZLC/KQCKQ7/?source_id=email&sub1=abc&sub2=discountblog.com"
	html := `<html><body><a href="` + money + `">CTA</a></body></html>`

	out := applySESTracking(html, "camp-1", "sub-1", "email-1", clickTestBase, clickTestOrg, clickTestSecret)

	// Regression: open pixel injected exactly ONCE — a single InjectOpenPixel
	// call places 2 byte-identical pixels (top + bottom, deduped downstream
	// via ON CONFLICT). 4 occurrences would mean double-injection.
	if got := strings.Count(out, "/track/open/"); got != 2 {
		t.Fatalf("want exactly 2 open-pixel occurrences (one injection, top+bottom), got %d", got)
	}
	// Clicks: wrapped, and the redirect resolves to the full money URL.
	_, _, _, _, orig := resolveWrappedClick(t, out)
	if orig != money {
		t.Fatalf("redirect target corrupted:\n want %q\n got  %q", money, orig)
	}
	if strings.Contains(out, `href="`+money+`"`) {
		t.Fatalf("raw money href still present alongside wrap")
	}
}

func TestApplySESTracking_ClickWrapKillSwitch(t *testing.T) {
	t.Setenv("DISABLE_SES_OPEN_PIXEL", "")
	t.Setenv("DISABLE_SES_CLICK_WRAP", "true")

	money := "https://www.codefortwo.com/K4C5ZLC/KQCKQ7/"
	html := `<html><body><a href="` + money + `">CTA</a></body></html>`

	out := applySESTracking(html, "c", "s", "e", clickTestBase, clickTestOrg, clickTestSecret)

	// Negative path: today's (pre-fix) behavior restored — no click wrap...
	if strings.Contains(out, "/track/click/") {
		t.Fatalf("DISABLE_SES_CLICK_WRAP=true must suppress click wrapping")
	}
	if !strings.Contains(out, `href="`+money+`"`) {
		t.Fatalf("original money link must survive untouched under the kill switch")
	}
	// ...while the jul02 open pixel keeps working independently.
	if got := strings.Count(out, "/track/open/"); got != 2 {
		t.Fatalf("open pixel must be unaffected by the click kill switch, got %d occurrences", got)
	}
}

func TestApplySESTracking_OpenPixelSwitchDoesNotKillClicks(t *testing.T) {
	t.Setenv("DISABLE_SES_OPEN_PIXEL", "true")
	t.Setenv("DISABLE_SES_CLICK_WRAP", "")

	html := `<html><body><a href="https://example.com/offer">CTA</a></body></html>`
	out := applySESTracking(html, "c", "s", "e", clickTestBase, clickTestOrg, clickTestSecret)

	if strings.Contains(out, "/track/open/") {
		t.Fatalf("DISABLE_SES_OPEN_PIXEL=true must suppress the pixel")
	}
	if got := strings.Count(out, "/track/click/"); got != 1 {
		t.Fatalf("click wrap must be independent of the pixel switch, got %d wrapped links", got)
	}
}

func TestApplySESTracking_BothDisabled_HTMLUntouched(t *testing.T) {
	t.Setenv("DISABLE_SES_OPEN_PIXEL", "true")
	t.Setenv("DISABLE_SES_CLICK_WRAP", "true")

	html := `<html><body><a href="https://example.com/offer">CTA</a></body></html>`
	out := applySESTracking(html, "c", "s", "e", clickTestBase, clickTestOrg, clickTestSecret)
	if out != html {
		t.Fatalf("both kill switches set must return html byte-identical:\n want %q\n got  %q", html, out)
	}
}
