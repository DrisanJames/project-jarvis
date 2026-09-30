package worker

// Tests for InjectTrackingPixelAndLinks.
//
// The function has very specific Gmail-image-proxy compatibility
// requirements that broke in the wild: the previous implementation
// emitted a single pixel at the bottom of the body with style="display:none"
// directly on the <img> element. Empirical evidence from production showed
// Gmail Image Proxy was not fetching that pixel — Gmail clicks fired
// reliably while opens were near-zero (an inverted ratio that no
// other ISP exhibits).
//
// The fix moves to a SparkPost / HistoryFacts style that we have direct
// proof works in Gmail:
//   - Two pixels, one near the top of <body> and one before </body>
//   - display:none lives on a wrapper <div>, not on the <img>
//   - The <img> itself has no inline style
//
// These tests assert the structural invariants so future refactors do
// not regress back into the broken state.

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

const (
	testCampaignID   = "11111111-1111-1111-1111-111111111111"
	testSubscriberID = "22222222-2222-2222-2222-222222222222"
	testEmailID      = "33333333-3333-3333-3333-333333333333"
	testOrgID        = "00000000-0000-0000-0000-000000000001"
	testBaseURL      = "https://trk.example.test"
	testSecret       = "s3cr3t-test-key"
)

func TestInjectTrackingPixel_EmitsTwoPixels(t *testing.T) {
	html := `<html><body><h1>hi</h1></body></html>`
	out := InjectTrackingPixelAndLinks(
		html, testCampaignID, testSubscriberID, testEmailID,
		testBaseURL, testOrgID, testSecret,
	)

	// Both pixels are byte-identical (same SRC); count occurrences of
	// /track/open/ to confirm two were inserted.
	if got := strings.Count(out, "/track/open/"); got != 2 {
		t.Fatalf("expected 2 tracking pixels, got %d\n--- output ---\n%s", got, out)
	}
}

func TestInjectTrackingPixel_TopPixelIsImmediatelyAfterBodyOpen(t *testing.T) {
	html := `<html><body class="x"><h1>hi</h1></body></html>`
	out := InjectTrackingPixelAndLinks(
		html, testCampaignID, testSubscriberID, testEmailID,
		testBaseURL, testOrgID, testSecret,
	)

	// The top pixel must appear BEFORE the <h1>hi</h1> body content so
	// Gmail's 102KB clipping rule cannot drop it.
	idxTopPixel := strings.Index(out, "/track/open/")
	idxContent := strings.Index(out, "<h1>hi</h1>")
	if idxTopPixel < 0 || idxContent < 0 {
		t.Fatalf("expected both top pixel and content; got\n%s", out)
	}
	if idxTopPixel >= idxContent {
		t.Errorf("top pixel must appear before body content; pixelIdx=%d contentIdx=%d\n%s",
			idxTopPixel, idxContent, out)
	}

	// And the top pixel must come AFTER the <body ...> tag, not before.
	idxBodyOpen := strings.Index(strings.ToLower(out), "<body")
	idxBodyClose := strings.Index(out[idxBodyOpen:], ">") + idxBodyOpen
	if idxTopPixel <= idxBodyClose {
		t.Errorf("top pixel must appear after body open close-bracket; bodyClose=%d pixel=%d",
			idxBodyClose, idxTopPixel)
	}
}

func TestInjectTrackingPixel_BottomPixelIsImmediatelyBeforeBodyClose(t *testing.T) {
	html := `<html><body><h1>hi</h1></body></html>`
	out := InjectTrackingPixelAndLinks(
		html, testCampaignID, testSubscriberID, testEmailID,
		testBaseURL, testOrgID, testSecret,
	)

	// LastIndex finds the BOTTOM pixel; it must come right before </body>.
	idxLastPixel := strings.LastIndex(out, "/track/open/")
	idxBodyClose := strings.LastIndex(strings.ToLower(out), "</body>")
	if idxLastPixel < 0 || idxBodyClose < 0 {
		t.Fatalf("expected bottom pixel and </body>; got\n%s", out)
	}
	if idxLastPixel >= idxBodyClose {
		t.Errorf("bottom pixel must appear before </body>; pixel=%d body=%d",
			idxLastPixel, idxBodyClose)
	}
}

func TestInjectTrackingPixel_DisplayNoneIsOnWrapperNotImg(t *testing.T) {
	html := `<html><body><h1>hi</h1></body></html>`
	out := InjectTrackingPixelAndLinks(
		html, testCampaignID, testSubscriberID, testEmailID,
		testBaseURL, testOrgID, testSecret,
	)

	// CRITICAL: the <img> tag itself MUST NOT have inline style, because
	// Gmail's image proxy skips elements with display:none on the IMG.
	// History Facts wraps in a hidden DIV instead and that pattern works
	// for Gmail.
	imgRe := regexp.MustCompile(`<img[^>]*src="[^"]*/track/open/[^"]+"[^>]*>`)
	matches := imgRe.FindAllString(out, -1)
	if len(matches) == 0 {
		t.Fatalf("no tracking pixel <img> found\n%s", out)
	}
	for _, m := range matches {
		if strings.Contains(m, "style=") {
			t.Errorf("tracking pixel <img> must not have an inline style attribute, got: %s", m)
		}
		if strings.Contains(m, "display:none") {
			t.Errorf("tracking pixel <img> must not contain display:none, got: %s", m)
		}
	}

	// Double-check: there IS a wrapper div with display:none somewhere.
	// We require the pixel to be inside such a wrapper.
	if !strings.Contains(out, "display:none") {
		t.Errorf("expected wrapper div with display:none for hiding the pixel\n%s", out)
	}
}

func TestInjectTrackingPixel_RewritesLinks(t *testing.T) {
	html := `<html><body><a href="https://example.com/x">x</a></body></html>`
	out := InjectTrackingPixelAndLinks(
		html, testCampaignID, testSubscriberID, testEmailID,
		testBaseURL, testOrgID, testSecret,
	)
	if !strings.Contains(out, "/track/click/") {
		t.Errorf("href was not rewritten to a click-tracking URL\n%s", out)
	}
	if strings.Contains(out, `href="https://example.com/x"`) {
		t.Errorf("original href should have been replaced; got\n%s", out)
	}
}

func TestInjectTrackingPixel_HandlesMissingBodyTag(t *testing.T) {
	// Some legacy templates have no <body> wrapper. Pixel should still
	// land at the end of the document (same fallback as the old code).
	html := `<h1>plain</h1>`
	out := InjectTrackingPixelAndLinks(
		html, testCampaignID, testSubscriberID, testEmailID,
		testBaseURL, testOrgID, testSecret,
	)
	if !strings.Contains(out, "/track/open/") {
		t.Errorf("pixel missing for body-less HTML\n%s", out)
	}
	// In this fallback path we get one pixel at the end (no <body> open
	// to insert after, no </body> to insert before).
	if got := strings.Count(out, "/track/open/"); got != 1 {
		t.Errorf("body-less HTML should yield 1 pixel (fallback), got %d\n%s", got, out)
	}
}

func TestInjectTrackingPixel_DoesNotRewriteAlreadyTrackedLinks(t *testing.T) {
	// Already-rewritten links (containing /track/) and mailto: must be
	// left alone so the unsubscribe + tracking links are not double-wrapped.
	html := `<html><body><a href="https://trk.example.test/track/click/abc/def">u</a><a href="mailto:foo@bar">m</a></body></html>`
	out := InjectTrackingPixelAndLinks(
		html, testCampaignID, testSubscriberID, testEmailID,
		testBaseURL, testOrgID, testSecret,
	)
	if !strings.Contains(out, `https://trk.example.test/track/click/abc/def`) {
		t.Errorf("already-tracked link must be preserved\n%s", out)
	}
	if !strings.Contains(out, `mailto:foo@bar`) {
		t.Errorf("mailto link must be preserved\n%s", out)
	}
}

// ── Position markers (2026-09-29) ────────────────────────────────────────
// Top pixel carries ?p=t, bottom ?p=b; path + signature identical so every
// route, sig check and the (campaign, subscriber) dedupe are unchanged.

var pixelSrcRe = regexp.MustCompile(`<img src="([^"]*/track/open/[^"]+)"`)

func expectedPixelToken() (encoded, sig string) {
	encoded = base64.URLEncoding.EncodeToString([]byte(testOrgID + "|" + testCampaignID + "|" + testSubscriberID + "|" + testEmailID))
	return encoded, TrackSign(encoded, testSecret)
}

func TestInjectTrackingPixel_PositionMarkers_TopAndBottom(t *testing.T) {
	html := `<html><BODY class="x" style="margin:0"><h1>hi</h1><a href="https://example.com/x">x</a></body></html>`
	out := InjectTrackingPixelAndLinks(html, testCampaignID, testSubscriberID, testEmailID, testBaseURL, testOrgID, testSecret)
	enc, sig := expectedPixelToken()
	base := testBaseURL + "/track/open/" + enc + "/" + sig

	srcs := pixelSrcRe.FindAllStringSubmatch(out, -1)
	if len(srcs) != 2 || strings.Count(out, "/track/open/") != 2 {
		t.Fatalf("want exactly 2 open pixels, got %d\n%s", len(srcs), out)
	}
	if srcs[0][1] != base+"?p=t" {
		t.Errorf("top pixel src = %q, want %q", srcs[0][1], base+"?p=t")
	}
	if srcs[1][1] != base+"?p=b" {
		t.Errorf("bottom pixel src = %q, want %q", srcs[1][1], base+"?p=b")
	}

	// Top pixel sits IMMEDIATELY after the <body ...> tag.
	bodyOpen := strings.Index(strings.ToLower(out), "<body")
	afterBody := bodyOpen + strings.Index(out[bodyOpen:], ">") + 1
	if !strings.HasPrefix(out[afterBody:], buildOpenPixelHTML(testBaseURL, enc, sig, "t")) {
		t.Errorf("top pixel not immediately after <body>:\n%s", out[afterBody:])
	}
	// Bottom pixel sits IMMEDIATELY before </body>.
	bodyClose := strings.LastIndex(strings.ToLower(out), "</body>")
	if !strings.HasSuffix(out[:bodyClose], buildOpenPixelHTML(testBaseURL, enc, sig, "b")) {
		t.Errorf("bottom pixel not immediately before </body>:\n%s", out[:bodyClose])
	}

	// Click rewriting touched the content link and NEITHER pixel.
	if !strings.Contains(out, "/track/click/") {
		t.Errorf("content link was not click-wrapped\n%s", out)
	}
	for _, m := range srcs {
		if strings.Contains(m[1], "/track/click/") {
			t.Errorf("pixel was click-wrapped: %s", m[1])
		}
	}
}

func TestInjectTrackingPixel_PositionMarkers_NoBodyGetsBottomOnly(t *testing.T) {
	out := InjectTrackingPixelAndLinks(`<h1>plain</h1>`, testCampaignID, testSubscriberID, testEmailID, testBaseURL, testOrgID, testSecret)
	enc, sig := expectedPixelToken()
	if !strings.HasSuffix(out, buildOpenPixelHTML(testBaseURL, enc, sig, "b")) {
		t.Errorf("body-less HTML must get the ?p=b pixel appended\n%s", out)
	}
	if strings.Contains(out, "?p=t") {
		t.Errorf("body-less HTML must not carry a top pixel\n%s", out)
	}
}

// The ?p= marker is outside the signed {data} segment: stripping it leaves
// the exact pre-change URL shape, and the signature still matches the token.
func TestInjectTrackingPixel_PositionMarkers_SignatureNeutral(t *testing.T) {
	out := InjectOpenPixel(`<html><body>x</body></html>`, testCampaignID, testSubscriberID, testEmailID, testBaseURL, testOrgID, testSecret)
	srcs := pixelSrcRe.FindAllStringSubmatch(out, -1)
	if len(srcs) != 2 {
		t.Fatalf("want 2 pixels, got %d", len(srcs))
	}
	top := strings.TrimSuffix(srcs[0][1], "?p=t")
	bot := strings.TrimSuffix(srcs[1][1], "?p=b")
	if top != bot {
		t.Fatalf("top/bottom must share path+sig: %q vs %q", top, bot)
	}
	parts := strings.Split(strings.TrimPrefix(top, testBaseURL+"/track/open/"), "/")
	if len(parts) != 2 || TrackSign(parts[0], testSecret) != parts[1] {
		t.Fatalf("signature does not verify over the token: %v", parts)
	}
}

// InjectOpenPixel and InjectTrackingPixelAndLinks emit byte-identical pixels
// (they share buildOpenPixelHTML) — on link-free HTML the outputs are equal.
func TestInjectOpenPixel_MatchesInjectTrackingPixelAndLinks(t *testing.T) {
	html := `<html><body><p>no links</p></body></html>`
	a := InjectOpenPixel(html, testCampaignID, testSubscriberID, testEmailID, testBaseURL, testOrgID, testSecret)
	b := InjectTrackingPixelAndLinks(html, testCampaignID, testSubscriberID, testEmailID, testBaseURL, testOrgID, testSecret)
	if a != b {
		t.Fatalf("pixel construction drifted:\n%s\n%s", a, b)
	}
}
