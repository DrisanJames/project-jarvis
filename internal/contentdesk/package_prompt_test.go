package contentdesk

import (
	"strings"
	"testing"
)

// The packager sees the body's inline markers, so a title or excerpt line
// that restates a cited body sentence can carry the same marker. Live :1130:
// myownhealth's only hard blocker was an excerpt sentence restating the cited
// lede-1#0 with no marker, because the packager had only seen plain text.
func TestPackagePrompt_ShowsBodyMarkers(t *testing.T) {
	d := draftResult{Blocks: []Block{{ID: "lede-1", Type: "lede", Text: "Fees apply. Rates vary."}},
		ClaimRefs: []ClaimRef{{BlockID: "lede-1", SentenceIdx: 0, ClaimID: cU1, Version: 1}}}
	p := packagePrompt(PipelineInput{}, d, []Claim{{ClaimID: cU1, Version: 1, Status: ClaimSupported}})
	if !strings.Contains(p, "[[c:"+cU1+"@1]]") || !strings.Contains(p, "Rates vary") {
		t.Fatalf("the package prompt must show the body with its markers:\n%s", p)
	}
	if !strings.Contains(packageSystem, "carry that body sentence's marker") {
		t.Fatal("the package rules must tell the packager to carry a restated sentence's marker")
	}
}
