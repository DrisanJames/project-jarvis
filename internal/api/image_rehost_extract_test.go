package api

import "testing"

// TestCollectImageURLs guards the image extractor against the regression that
// shipped first: only <img src> was matched, so network creatives that embed
// images via <td background>, CSS url(), unquoted src, or Outlook VML passed
// through unrehosted (the imageports.com/icon_1.png incident).
func TestCollectImageURLs(t *testing.T) {
	html := `
	<html><body>
	  <img src="https://www.imageports.com/hero.png" width="600">
	  <img src='https://www.imageports.com/single.png'>
	  <img width="20" border="0" src="https://www.imageports.com/attr_first.png" alt="x">
	  <img src=https://www.imageports.com/unquoted.png>
	  <td background="https://www.imageports.com/icon_1.png">spacer</td>
	  <table background='https://www.imageports.com/tablebg.jpg'></table>
	  <div style="background-image:url('https://www.imageports.com/cssbg.png')"></div>
	  <div style="background:url(https://www.imageports.com/cssbg2.png)"></div>
	  <v:fill type="tile" src="https://www.imageports.com/vml.png" />
	  <img src="data:image/png;base64,AAAA">
	  <img src="/relative/local.png">
	</body></html>`

	got := collectImageURLs(html)
	gotSet := map[string]bool{}
	for _, u := range got {
		gotSet[u] = true
	}

	mustHave := []string{
		"https://www.imageports.com/hero.png",
		"https://www.imageports.com/single.png",
		"https://www.imageports.com/attr_first.png",
		"https://www.imageports.com/unquoted.png",
		"https://www.imageports.com/icon_1.png", // the bug: <td background>
		"https://www.imageports.com/tablebg.jpg",
		"https://www.imageports.com/cssbg.png",
		"https://www.imageports.com/cssbg2.png",
		"https://www.imageports.com/vml.png",
		"data:image/png;base64,AAAA", // collected; RehostHTML skips data: at fetch time
		"/relative/local.png",        // collected; RehostHTML skips non-absolute at fetch time
	}
	for _, want := range mustHave {
		if !gotSet[want] {
			t.Errorf("collectImageURLs missed %q; got %v", want, got)
		}
	}

	// Dedup: the same URL referenced twice appears once.
	dupHTML := `<img src="https://x.com/a.png"><div style="background:url(https://x.com/a.png)">`
	if d := collectImageURLs(dupHTML); len(d) != 1 {
		t.Errorf("expected 1 deduped url, got %v", d)
	}
}
