package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// These tests pin the 2026-09-11 fix: RewriteClickLinks wraps ONLY the hrefs of
// clickable elements (<a>, <area>, VML <v:*>). Before it, linkRe matched href
// on ANY tag, so <link rel="stylesheet" href="https://fonts.googleapis.com/…">
// was wrapped into /track/click and every client/proxy stylesheet fetch
// recorded a CLICK (fonts.googleapis.com = the largest /track/click
// destination in prod).

func anchorRewrite(h string) string {
	return RewriteClickLinks(h, "camp-1", "sub-1", "email-1", clickTestBase, clickTestOrg, clickTestSecret)
}

func TestRewriteClickLinks_OnlyClickableTags(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wrapped bool
		dest    string // decoded destination when wrapped
	}{
		// untouched — resources / document hints fetched without a click
		{name: "link stylesheet unquoted", in: `<link rel=stylesheet href=https://fonts.googleapis.com/css2?family=Roboto>`},
		{name: "link stylesheet quoted", in: `<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Roboto&display=swap">`},
		{name: "link href before rel", in: `<link href="https://fonts.googleapis.com/css2?family=Outfit:wght@100..900&display=swap" rel="stylesheet">`},
		{name: "link preconnect", in: `<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>`},
		{name: "LINK uppercase", in: `<LINK REL="stylesheet" HREF="https://cdnjs.cloudflare.com/ajax/libs/font-awesome/4.7.0/css/font-awesome.min.css">`},
		{name: "base", in: `<base href="https://www.discountblog.com/">`},
		{name: "img src", in: `<img src="https://img.example.com/a.png" alt="">`},
		{name: "script src", in: `<script src="https://cdn.example.com/x.js"></script>`},
		{name: "abbr is not a", in: `<abbr title="x" href="https://example.com/abbr">x</abbr>`},
		// untouched — existing exclusions inside <a>
		{name: "a mailto", in: `<a href="mailto:hi@discountblog.com">Mail</a>`},
		{name: "a already wrapped", in: `<a href="` + clickTestBase + `/track/click/YWJj/def">x</a>`},
		{name: "a tracked unsubscribe", in: `<a href="` + clickTestBase + `/track/unsubscribe/dG9rZW4=/sig">u</a>`},
		{name: "a offer /o/", in: `<a href="` + clickTestBase + `/o/discountblog.com/sub-1/hash/camp-1">o</a>`},
		{name: "a relative", in: `<a href="/preferences">p</a>`},
		// wrapped
		{name: "a plain", in: `<a href="https://example.com/a">a</a>`, wrapped: true, dest: "https://example.com/a"},
		{name: "A HREF single quoted", in: `<A HREF='https://example.com/b?x=1&y=2'>b</A>`, wrapped: true, dest: "https://example.com/b?x=1&y=2"},
		{name: "a attrs before href", in: `<a class="x" target="_blank" href="https://example.com/c">c</a>`, wrapped: true, dest: "https://example.com/c"},
		{name: "a attrs after href", in: `<a href="https://example.com/d" style="color:#fff" target="_blank">d</a>`, wrapped: true, dest: "https://example.com/d"},
		{name: "a quoted > before href", in: `<a title="a > b" href="https://example.com/gt">gt</a>`, wrapped: true, dest: "https://example.com/gt"},
		{name: "a multiline", in: "<a\n   style=\"color:#fff\"\n   href=\"https://example.com/nl\">nl</a>", wrapped: true, dest: "https://example.com/nl"},
		{name: "a in mso conditional", in: `<!--[if mso]><a href="https://example.com/mso">m</a><![endif]-->`, wrapped: true, dest: "https://example.com/mso"},
		{name: "a in !mso conditional", in: `<!--[if !mso]><!--><a href="https://example.com/notmso">m</a><!--<![endif]-->`, wrapped: true, dest: "https://example.com/notmso"},
		{name: "vml roundrect in mso", in: `<!--[if mso]><v:roundrect xmlns:v="urn:schemas-microsoft-com:vml" arcsize="10%" href="https://example.com/vml" style="height:52px"><w:anchorlock/><center>Go</center></v:roundrect><![endif]-->`, wrapped: true, dest: "https://example.com/vml"},
		{name: "area", in: `<map name="m"><area shape="rect" coords="0,0,10,10" href="https://example.com/area"></map>`, wrapped: true, dest: "https://example.com/area"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := anchorRewrite(tc.in)
			if !tc.wrapped {
				if out != tc.in {
					t.Fatalf("must be byte-identical\n in: %s\nout: %s", tc.in, out)
				}
				return
			}
			if got := strings.Count(out, "/track/click/"); got != 1 {
				t.Fatalf("want exactly 1 wrapped link, got %d\n%s", got, out)
			}
			_, _, _, _, orig := resolveWrappedClick(t, out)
			if orig != tc.dest {
				t.Fatalf("destination: got %q want %q", orig, tc.dest)
			}
		})
	}
}

func TestRewriteClickLinks_AttributesAroundHrefPreserved(t *testing.T) {
	out := anchorRewrite(`<a class="x" href="https://example.com/c" target="_blank">c</a>`)
	if !strings.HasPrefix(out, `<a class="x" href="`+clickTestBase+`/track/click/`) ||
		!strings.HasSuffix(out, `" target="_blank">c</a>`) {
		t.Fatalf("surrounding attributes changed: %s", out)
	}
}

// Anchors that were rewritten before the fix are rewritten byte-for-byte the
// same way after it (legacy = CLICK_WRAP_ANY_TAG=true, the whole-document match).
func TestRewriteClickLinks_AnchorOutputUnchangedVsLegacy(t *testing.T) {
	in := `<html><body><a href="https://example.com/a?sub1={{subscriber.id}}&amp;sub2={{brand.domain}}">a</a>` +
		`<p><a class="btn" href='https://example.com/b'>b</a></p><a href="mailto:x@y.z">m</a>` +
		`<a href="` + clickTestBase + `/track/unsubscribe/dG9rZW4=/sig">u</a></body></html>`
	now := anchorRewrite(in)
	t.Setenv("CLICK_WRAP_ANY_TAG", "true")
	if legacy := anchorRewrite(in); legacy != now {
		t.Fatalf("anchor output drifted from legacy\nlegacy: %s\n   now: %s", legacy, now)
	}
}

// Kill switch: CLICK_WRAP_ANY_TAG=true restores the historical any-tag match.
func TestRewriteClickLinks_KillSwitchRestoresAnyTag(t *testing.T) {
	in := `<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Roboto"><a href="https://example.com/a">a</a>`
	if got := strings.Count(anchorRewrite(in), "/track/click/"); got != 1 {
		t.Fatalf("default: want 1 wrapped (anchor only), got %d", got)
	}
	t.Setenv("CLICK_WRAP_ANY_TAG", "true")
	if got := strings.Count(anchorRewrite(in), "/track/click/"); got != 2 {
		t.Fatalf("CLICK_WRAP_ANY_TAG=true: want 2 wrapped (legacy), got %d", got)
	}
}

// InjectTrackingPixelAndLinks (PMTA path, proofs, click-drip) inherits the fix.
func TestInjectTrackingPixelAndLinks_LinkTagUntouched(t *testing.T) {
	link := `<link href="https://fonts.googleapis.com/css2?family=Roboto" rel="stylesheet">`
	out := InjectTrackingPixelAndLinks(`<html><head>`+link+`</head><body><a href="https://example.com/x">x</a></body></html>`,
		"c", "s", "e", clickTestBase, clickTestOrg, clickTestSecret)
	if !strings.Contains(out, link) {
		t.Fatalf("<link> was modified:\n%s", out)
	}
	if got := strings.Count(out, "/track/click/"); got != 1 {
		t.Fatalf("want 1 wrapped link, got %d", got)
	}
}

// --- golden: real creatives (READ-ONLY from prod PG 2026-09-11, template bodies,
// no subscriber data — merge tags only) ------------------------------------

var nonClickableHrefTagRe = regexp.MustCompile(`(?i)<(?:link|base)\b[^>]*>`)

// oracleClickable counts clickable elements carrying a wrappable absolute
// http(s) href, using the x/net/html tokenizer (independent of the production
// regexes). MSO conditional comments hold real markup, so comment bodies are
// tokenized too.
func oracleClickable(s string) (a, area, vml, nonClickable int) {
	z := html.NewTokenizer(strings.NewReader(s))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return
		case html.CommentToken:
			ca, car, cv, cn := oracleClickable(string(z.Text()))
			a, area, vml, nonClickable = a+ca, area+car, vml+cv, nonClickable+cn
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			href := ""
			for _, at := range tok.Attr {
				if at.Key == "href" {
					href = at.Val
				}
			}
			l := strings.ToLower(href)
			if !strings.HasPrefix(l, "http://") && !strings.HasPrefix(l, "https://") ||
				strings.Contains(href, "/track/") || strings.HasPrefix(href, clickTestBase+"/o/") || strings.Contains(href, "mailto:") {
				continue
			}
			switch {
			case tok.Data == "a":
				a++
			case tok.Data == "area":
				area++
			case strings.HasPrefix(tok.Data, "v:"):
				vml++
			default:
				nonClickable++
			}
		}
	}
}

func TestRewriteClickLinks_GoldenCreatives(t *testing.T) {
	files, err := filepath.Glob("testdata/click_rewrite/*.html")
	if err != nil || len(files) < 3 {
		t.Fatalf("want >=3 fixtures, got %d (%v)", len(files), err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			in := string(b)
			out := anchorRewrite(in)

			a, area, vml, nonClick := oracleClickable(in)
			got := strings.Count(out, "/track/click/")
			t.Logf("anchors=%d area=%d vml=%d non-clickable(link/base)=%d -> wrapped=%d", a, area, vml, nonClick, got)
			if got != a+area+vml {
				t.Errorf("wrapped %d, want %d (= <a> %d + <area> %d + VML %d)", got, a+area+vml, a, area, vml)
			}
			if a == 0 {
				t.Errorf("fixture has no wrappable <a> — not a useful golden")
			}
			// every <link>/<base> tag survives byte-for-byte
			tags := nonClickableHrefTagRe.FindAllString(in, -1)
			for _, tag := range tags {
				if !strings.Contains(out, tag) {
					t.Errorf("non-clickable tag modified: %s", tag)
				}
			}
			if strings.Count(out, "fonts.googleapis.com") != strings.Count(in, "fonts.googleapis.com") {
				t.Errorf("a fonts.googleapis.com reference was wrapped")
			}

			// Byte-for-byte vs legacy with the non-clickable tags masked out: the
			// ONLY difference the fix may introduce is leaving those tags alone.
			masked := in
			for i, tag := range tags {
				masked = strings.Replace(masked, tag, fmt.Sprintf("@@NONCLICK%d@@", i), 1)
			}
			t.Setenv("CLICK_WRAP_ANY_TAG", "true")
			legacy := anchorRewrite(masked)
			for i, tag := range tags {
				legacy = strings.Replace(legacy, fmt.Sprintf("@@NONCLICK%d@@", i), tag, 1)
			}
			if legacy != out {
				t.Errorf("output differs from legacy beyond the non-clickable tags")
			}
			if lg := strings.Count(anchorRewrite(in), "/track/click/"); lg != got+nonClick {
				t.Errorf("legacy wrapped %d, want %d (fix removes exactly the %d non-clickable)", lg, got+nonClick, nonClick)
			}
		})
	}
}
