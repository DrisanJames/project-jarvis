package worker

import (
	"strings"
	"testing"
)

func TestInjectOpenPixel_PixelOnlyNoLinkRewrite(t *testing.T) {
	html := `<html><body><a href="https://www.codefortwo.com/K4C5ZLC/KQCKQ7/">CTA</a></body></html>`
	out := InjectOpenPixel(html, "camp", "sub", "email", "https://t.em.example.com", "org", "secret")

	if got := strings.Count(out, "/track/open/"); got != 2 {
		t.Fatalf("want 2 open pixels (top+bottom), got %d", got)
	}
	if strings.Contains(out, "/track/click/") {
		t.Fatalf("links must NOT be rewritten on the SES path")
	}
	if !strings.Contains(out, `href="https://www.codefortwo.com/K4C5ZLC/KQCKQ7/"`) {
		t.Fatalf("original money link must survive untouched")
	}
}

func TestInjectOpenPixel_NoBodyTagStillAppends(t *testing.T) {
	out := InjectOpenPixel("<div>bare fragment</div>", "c", "s", "e", "https://t.x", "o", "k")
	if !strings.Contains(out, "/track/open/") {
		t.Fatalf("pixel must append even without <body>")
	}
}
