package api

import (
	"context"
	"strings"
	"testing"
)

func TestMoneyLinkExtractMoneyHrefs(t *testing.T) {
	html := `
		<a data-everflow="true" href="https://www.eos57ytf.com/K4C5ZLC/S6WFF5/?source_id=email&sub1={{subscriber.id}}&sub2=discountblog.com&sub3={{campaign.id}}">Get it</a>
		<a href="https://cratoolpro.com/BJB4Q5BF/93W8N2N/?source_id=email">legacy</a>
		<a href="https://example.com/not-money">nope</a>
	`
	hrefs := extractMoneyHrefs(html)
	if len(hrefs) != 2 {
		t.Fatalf("expected 2 money hrefs, got %d: %v", len(hrefs), hrefs)
	}
	if !strings.Contains(hrefs[0], "eos57ytf.com") {
		t.Errorf("first href not eos57ytf: %q", hrefs[0])
	}
	if !strings.Contains(hrefs[1], "cratoolpro.com") {
		t.Errorf("second href not cratoolpro: %q", hrefs[1])
	}
	if got := extractMoneyHrefs(""); got != nil {
		t.Errorf("empty html should yield nil, got %v", got)
	}
}

func TestMoneyLinkSlugFromMoneyURL(t *testing.T) {
	cases := map[string]string{
		"https://cratoolpro.com/BJB4Q5BF/93W8N2N/":     "93W8N2N",
		"https://www.eos57ytf.com/K4C5ZLC/PS8241/?x=1": "PS8241",
		"https://xnonu.com/TQ5MX18J/XF1SR2CS/":         "XF1SR2CS",
		"not a url":                                    "",
		"":                                             "",
	}
	for in, want := range cases {
		if got := slugFromMoneyURL(in); got != want {
			t.Errorf("slugFromMoneyURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMoneyLinkDeadSlugSetFromRemap(t *testing.T) {
	dead := deadSlugSet()
	// deadLinkRemap (mailing_tracking.go) currently maps J78S2MD (metal roofing)
	// and 3MZNPR (empire). These must surface as dead slugs (upper-cased).
	if !dead["J78S2MD"] {
		t.Errorf("expected J78S2MD in dead slug set, got %v", dead)
	}
	if !dead["3MZNPR"] {
		t.Errorf("expected 3MZNPR in dead slug set, got %v", dead)
	}
}

const goodMoneyHref = `<a data-everflow="true" href="https://www.eos57ytf.com/K4C5ZLC/S6WFF5/?source_id=email&sub1={{subscriber.id}}&sub2=discountblog.com&sub3={{campaign.id}}">x</a>`

func hasFindingCode(findings []MoneyLinkFinding, code string) bool {
	for _, f := range findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

func TestMoneyLinkLocalClean(t *testing.T) {
	findings := checkCreativeMoneyLinksLocal(goodMoneyHref, "discountblog.com")
	if len(findings) != 0 {
		t.Fatalf("expected a clean creative, got findings: %+v", findings)
	}
}

func TestMoneyLinkLocalMissingTags(t *testing.T) {
	// Missing sub2/sub3 — should block with missing_tags.
	html := `<a data-everflow="true" href="https://www.eos57ytf.com/K4C5ZLC/S6WFF5/?source_id=email&sub1={{subscriber.id}}">x</a>`
	findings := checkCreativeMoneyLinksLocal(html, "discountblog.com")
	if !hasFindingCode(findings, "missing_tags") {
		t.Errorf("expected missing_tags, got %+v", findings)
	}
}

func TestMoneyLinkLocalBrandMismatch(t *testing.T) {
	// sub2 says quizfiesta.com but brandRoot is discountblog.com.
	html := `<a data-everflow="true" href="https://www.eos57ytf.com/K4C5ZLC/S6WFF5/?source_id=email&sub1={{subscriber.id}}&sub2=quizfiesta.com&sub3={{campaign.id}}">x</a>`
	findings := checkCreativeMoneyLinksLocal(html, "discountblog.com")
	if !hasFindingCode(findings, "brand_mismatch") {
		t.Errorf("expected brand_mismatch, got %+v", findings)
	}
}

func TestMoneyLinkLocalBrandUnresolvedWarn(t *testing.T) {
	// Unknown brandRoot → sub2 mismatch is a warn (brand_unresolved), not block.
	html := `<a data-everflow="true" href="https://www.eos57ytf.com/K4C5ZLC/S6WFF5/?source_id=email&sub1={{subscriber.id}}&sub2=quizfiesta.com&sub3={{campaign.id}}">x</a>`
	findings := checkCreativeMoneyLinksLocal(html, "")
	if !hasFindingCode(findings, "brand_unresolved") {
		t.Errorf("expected brand_unresolved warn, got %+v", findings)
	}
	if hasFindingCode(findings, "brand_mismatch") {
		t.Errorf("brand_mismatch should not fire when brandRoot empty: %+v", findings)
	}
}

func TestMoneyLinkLocalStaleDeadSlug(t *testing.T) {
	// A fully-tagged href that carries the known-dead J78S2MD slug.
	html := `<a data-everflow="true" href="https://cratoolpro.com/BJB4Q5BF/J78S2MD/?source_id=email&sub1={{subscriber.id}}&sub2=discountblog.com&sub3={{campaign.id}}">x</a>`
	findings := checkCreativeMoneyLinksLocal(html, "discountblog.com")
	if !hasFindingCode(findings, "stale_dead_slug") {
		t.Errorf("expected stale_dead_slug, got %+v", findings)
	}
}

func TestMoneyLinkLocalEmptyEverflow(t *testing.T) {
	cases := []string{
		`<a data-everflow="true" href="">x</a>`,
		`<a data-everflow="true" href="#">x</a>`,
		`<a data-everflow="true" href="javascript:void(0)">x</a>`,
		`<a data-everflow="true" href="  ">x</a>`,
	}
	for _, html := range cases {
		findings := checkCreativeMoneyLinksLocal(html, "discountblog.com")
		if !hasFindingCode(findings, "empty_everflow_href") {
			t.Errorf("expected empty_everflow_href for %q, got %+v", html, findings)
		}
	}
}

func findingLevel(findings []MoneyLinkFinding, code string) string {
	for _, f := range findings {
		if f.Code == code {
			return f.Level
		}
	}
	return ""
}

func TestMoneyLinkLocalNoMoneyLinkEngagementOnlyWarns(t *testing.T) {
	// No money host anywhere and no everflow anchor → genuinely non-monetized
	// (engagement-only) creative → no_money_link at WARN (not block).
	html := `<a href="https://example.com/article">read</a>`
	findings := checkCreativeMoneyLinksLocal(html, "discountblog.com")
	if lvl := findingLevel(findings, "no_money_link"); lvl != "warn" {
		t.Errorf("expected no_money_link at warn for engagement-only creative, got level %q in %+v", lvl, findings)
	}
}

func TestMoneyLinkLocalMoneyHostPresentButNoHrefBlocks(t *testing.T) {
	// A money-network host string IS referenced (cratoolpro.com in plain text)
	// but no usable <a href> was captured → present-but-broken → BLOCK.
	html := `<p>visit cratoolpro.com to claim your offer</p>`
	findings := checkCreativeMoneyLinksLocal(html, "discountblog.com")
	if lvl := findingLevel(findings, "no_money_link"); lvl != "block" {
		t.Errorf("expected no_money_link at block when money host referenced but no href, got level %q in %+v", lvl, findings)
	}
}

func TestMoneyLinkOverallEngagementOnlyIsWarnNotDead(t *testing.T) {
	// Engagement-only creative should map to overall "warn" (amber), not "dead".
	html := `<a href="https://example.com/article">read</a>`
	status, _, _ := checkCreativeMoneyLinks(context.Background(), html, "discountblog.com", false)
	if status != "warn" {
		t.Errorf("expected overall status warn for engagement-only creative, got %q", status)
	}
}

func TestMoneyLinkOverallBrokenMoneyHostIsDead(t *testing.T) {
	// Money host referenced but no usable href → present-but-broken → overall dead.
	html := `<p>visit cratoolpro.com to claim your offer</p>`
	status, _, _ := checkCreativeMoneyLinks(context.Background(), html, "discountblog.com", false)
	if status != "dead" {
		t.Errorf("expected overall status dead for broken money host, got %q", status)
	}
}

func TestMoneyLinkIsInternalHostGuard(t *testing.T) {
	internal := []string{
		"localhost", "127.0.0.1", "10.0.0.5", "192.168.1.1",
		"169.254.169.254", "172.16.0.1", "172.31.255.255",
		"metadata.google.internal", "::1", "[::1]", "",
	}
	for _, h := range internal {
		if !isInternalHost(h) {
			t.Errorf("isInternalHost(%q) = false, want true", h)
		}
	}
	external := []string{
		"eos57ytf.com", "www.cratoolpro.com", "8.8.8.8",
		"172.15.0.1", "172.32.0.1", "11.0.0.1", "example.com",
	}
	for _, h := range external {
		if isInternalHost(h) {
			t.Errorf("isInternalHost(%q) = true, want false", h)
		}
	}
}

func TestMoneyLinkBrandRootFromCode(t *testing.T) {
	cases := map[string]string{
		"DB":            "discountblog.com", // canonical code
		"db":            "discountblog.com", // case-insensitive
		"RR":            "refinanceratesusa.com",
		"discount-blog": "discountblog.com", // studio slug form
		"quizfiesta.com": "quizfiesta.com",  // bare apex via brand.Root
		"em.discountblog.com": "discountblog.com", // sending domain collapses
		"":            "", // empty
		"ZZ-unknown":  "", // unresolved
	}
	for in, want := range cases {
		if got := brandRootFromCode(in); got != want {
			t.Errorf("brandRootFromCode(%q) = %q, want %q", in, got, want)
		}
	}
}
