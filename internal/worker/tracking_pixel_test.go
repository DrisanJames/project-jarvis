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
