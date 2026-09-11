package api

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// 2026-09-11: click rewriters touch clickable tags only (<a>/<area>/VML) —
// see worker.RewriteInClickableTags. These pin the two api-side callers.

const fontsLinkTag = `<link href="https://fonts.googleapis.com/css2?family=Roboto&display=swap" rel="stylesheet">`

// Campaign-builder sync send (campaign_builder_send_sync.go → injectTrackingWithURL).
func TestInjectTrackingWithURL_LinkTagUntouched(t *testing.T) {
	svc := &MailingService{signingKey: "test-key"}
	in := `<html><head>` + fontsLinkTag + `<base href="https://www.example.com/"></head><body>` +
		`<A HREF='https://example.com/x'>x</A><a class="b" href="https://example.com/y">y</a>` +
		`<a href="mailto:a@b.c">m</a></body></html>`
	out := svc.injectTrackingWithURL(in, uuid.New(), uuid.New(), uuid.New(), uuid.New(), "https://t.em.example.com")
	if !strings.Contains(out, fontsLinkTag) || !strings.Contains(out, `<base href="https://www.example.com/">`) {
		t.Fatalf("<link>/<base> modified:\n%s", out)
	}
	if got := strings.Count(out, "/track/click/"); got != 2 {
		t.Fatalf("want 2 wrapped anchors, got %d\n%s", got, out)
	}
}

// Offer creative import (offer_creative_assets_handlers.go → classifyAndRewriteHTML):
// a stylesheet <link> must NOT be swapped for the offer tracking link.
func TestClassifyAndRewriteHTML_LinkTagNotReplacedByTrackingLink(t *testing.T) {
	const track = "https://track.example.com/c/OFFER"
	in := `<html><head>` + fontsLinkTag + `</head><body>` +
		`<a href="https://advertiser.example.com/landing">Go</a>` +
		`<!--[if mso]><v:roundrect href="https://advertiser.example.com/btn"></v:roundrect><![endif]-->` +
		`</body></html>`
	res := classifyAndRewriteHTML(context.Background(), in, track, "https://opt.example.com/out", "", "org",
		nil, &imageCache{cache: map[string]string{}})
	if !strings.Contains(res.RewrittenHTML, fontsLinkTag) {
		t.Fatalf("<link> rewritten:\n%s", res.RewrittenHTML)
	}
	if got := strings.Count(res.RewrittenHTML, `href="`+track+`"`); got != 2 {
		t.Fatalf("want anchor + VML CTA replaced (2), got %d\n%s", got, res.RewrittenHTML)
	}
}
