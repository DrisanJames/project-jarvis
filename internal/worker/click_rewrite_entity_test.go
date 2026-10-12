package worker

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

// An &amp;-escaped money href must reach the network with real "&" separators:
// the browser decodes the attribute, so the click token must carry the decoded URL.
func TestRewriteClickLinksDecodesEntitiesInDestination(t *testing.T) {
	in := `<a href="https://www.kj3rwth8trk.com/GZHPZ/BCFZ3Q/?creative_id=1017&amp;uid=266&amp;source_id=email&amp;sub1=S&amp;sub5=1017">x</a>`
	out := RewriteClickLinks(in, "C", "S", "E", "https://trk.example.com", "O", "k")
	m := regexp.MustCompile(`/track/click/([A-Za-z0-9_=-]+)/`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("link not wrapped: %s", out)
	}
	raw, err := base64.URLEncoding.DecodeString(m[1])
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(raw), "|")
	got := parts[len(parts)-1]
	want := "https://www.kj3rwth8trk.com/GZHPZ/BCFZ3Q/?creative_id=1017&uid=266&source_id=email&sub1=S&sub5=1017"
	if got != want {
		t.Fatalf("destination\n got %s\nwant %s", got, want)
	}
}

// A plain-& href is unchanged by the decode (the estate's common case).
func TestRewriteClickLinksPlainAmpersandUnchanged(t *testing.T) {
	url := "https://www.codefortwo.com/K4C5ZLC/31KGZPQ/?creative_id=6457&source_id=email&sub1=S&sub2=d.com"
	out := RewriteClickLinks(`<a href="`+url+`">x</a>`, "C", "S", "E", "https://trk.example.com", "O", "k")
	m := regexp.MustCompile(`/track/click/([A-Za-z0-9_=-]+)/`).FindStringSubmatch(out)
	raw, _ := base64.URLEncoding.DecodeString(m[1])
	if !strings.HasSuffix(string(raw), "|"+url) {
		t.Fatalf("plain url changed: %s", raw)
	}
}
