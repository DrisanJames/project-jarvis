package api

import (
	"strings"
	"testing"
)

// TestAppendUnsubDisclaimerPosition guards the footer-placement bug: the footer
// must land at the BOTTOM, including for fragment creatives with no <body> (which
// the old top-injector prepended into the header).
func TestAppendUnsubDisclaimerPosition(t *testing.T) {
	marker := unsubDisclaimerMarker

	// fragment (no body) — footer must be APPENDED, not prepended.
	frag := `<table><tr><td>creative</td></tr></table>`
	out := appendUnsubDisclaimer(frag, "discountblog.com", "")
	if !strings.HasPrefix(out, frag) {
		t.Errorf("fragment: footer not at bottom; got prefix %.40q", out)
	}
	if strings.Index(out, marker) <= strings.Index(out, "creative") {
		t.Error("fragment: footer marker should come AFTER the creative content")
	}
	if !strings.Contains(out, "discountblog.com") {
		t.Error("fragment: sending brand not in footer")
	}

	// full doc — footer must be inserted before </body>.
	full := `<html><body><p>hi</p></body></html>`
	out2 := appendUnsubDisclaimer(full, "myownhealth.net", "")
	bodyClose := strings.Index(out2, "</body>")
	if strings.Index(out2, marker) >= bodyClose {
		t.Error("full doc: footer must be before </body>")
	}
	if strings.Index(out2, marker) <= strings.Index(out2, "<p>hi</p>") {
		t.Error("full doc: footer should be after the body content")
	}

	// idempotent: already-marked html is untouched.
	if got := appendUnsubDisclaimer(out, "x.com", ""); got != out {
		t.Error("append should be idempotent when marker present")
	}
}
