package contentdesk

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const (
	cU1 = "11111111-1111-4111-8111-111111111111"
	cU2 = "22222222-2222-4222-8222-222222222222"
)

func sortRefs(r []ClaimRef) []ClaimRef {
	out := append([]ClaimRef(nil), r...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.BlockID != b.BlockID {
			return a.BlockID < b.BlockID
		}
		if a.SentenceIdx != b.SentenceIdx {
			return a.SentenceIdx < b.SentenceIdx
		}
		return a.ClaimID < b.ClaimID
	})
	return out
}

// The span splitter must agree with SplitSentences on every input, or a
// marker would be attributed to a different sentence than the checks see.
func TestSentenceSpans_MatchSplitSentences(t *testing.T) {
	for _, s := range []string{
		"", "One.", "One. Two!", "  lead. trail  ", "3.5% of loans. Next?", "e.g. x splits", "No end",
		"A.\nB.\tC.", "Wait... what? Yes!", "Line one.  Line two.", "Ünïcode ✓. Nächste.",
	} {
		spans := sentenceSpans(s)
		rs := []rune(s)
		var got []string
		for _, sp := range spans {
			got = append(got, string(rs[sp.start:sp.end]))
		}
		if want := SplitSentences(s); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: spans %q, SplitSentences %q", s, got, want)
		}
	}
}

func TestStripMarkers_AttachesToTheRightSentence(t *testing.T) {
	vs := map[string]int{cU1: 1, cU2: 3}
	cases := []struct {
		name, text, clean string
		want              []ClaimRef
	}{
		{"before punctuation", "The APR includes fees [[c:" + cU1 + "@1]]. Rates vary.",
			"The APR includes fees. Rates vary.", []ClaimRef{{BlockID: "b", SentenceIdx: 0, ClaimID: cU1, Version: 1}}},
		{"after punctuation attaches to the previous sentence", "The APR includes fees. [[c:" + cU1 + "@1]] Rates vary [[c:" + cU2 + "@3]].",
			"The APR includes fees. Rates vary.", []ClaimRef{{BlockID: "b", SentenceIdx: 0, ClaimID: cU1, Version: 1}, {BlockID: "b", SentenceIdx: 1, ClaimID: cU2, Version: 3}}},
		{"missing version resolves to the listed one", "37% of borrowers [[c:" + cU2 + "]] qualify.",
			"37% of borrowers qualify.", []ClaimRef{{BlockID: "b", SentenceIdx: 0, ClaimID: cU2, Version: 3}}},
		{"N/A marker is removed and never recorded", "Fees apply [[c:N/A]].", "Fees apply.", nil},
		{"no space after the period keeps the boundary", "Fees apply.[[c:" + cU1 + "@1]] Next one.",
			"Fees apply. Next one.", []ClaimRef{{BlockID: "b", SentenceIdx: 0, ClaimID: cU1, Version: 1}}},
	}
	for _, c := range cases {
		b := Block{ID: "b", Type: "lede", Text: c.text}
		got := extractBlockRefs(&b, vs)
		if b.Text != c.clean {
			t.Errorf("%s: clean text %q, want %q", c.name, b.Text, c.clean)
		}
		if !reflect.DeepEqual(sortRefs(got), sortRefs(c.want)) {
			t.Errorf("%s: refs %+v, want %+v", c.name, got, c.want)
		}
		for _, r := range got { // every ref must land on a unit the checks can resolve
			if r.SentenceIdx >= len(BlockSentences(b)) {
				t.Errorf("%s: ref %+v out of range of %q", c.name, r, BlockSentences(b))
			}
		}
	}
}

// markedBlocks → extractBlockRefs must round-trip every unit kind: heading,
// text, items, rows, caption.
func TestMarkedBlocks_RoundTrip(t *testing.T) {
	b := Block{ID: "s1", Type: "section", Heading: "Rates", Text: "APR includes fees. Rates vary by lender.",
		Items: []string{"Step one. Step two."}, Rows: [][]string{{"Term", "APR"}, {"36 mo", "9.9%"}}, Caption: "Source note."}
	refs := []ClaimRef{
		{BlockID: "s1", SentenceIdx: 1, ClaimID: cU1, Version: 1}, // text sentence 0
		{BlockID: "s1", SentenceIdx: 2, ClaimID: cU2, Version: 3}, // text sentence 1
		{BlockID: "s1", SentenceIdx: 4, ClaimID: cU1, Version: 1}, // item sentence 1
		{BlockID: "s1", SentenceIdx: 6, ClaimID: cU2, Version: 3}, // second row
		{BlockID: "s1", SentenceIdx: 7, ClaimID: cU1, Version: 1}, // caption
	}
	marked := markedBlocks([]Block{b}, refs)[0]
	if !strings.Contains(marked.Text, "[[c:"+cU1+"@1]]") {
		t.Fatalf("markers not inserted: %q", marked.Text)
	}
	got := extractBlockRefs(&marked, map[string]int{cU1: 1, cU2: 3})
	if !reflect.DeepEqual(sortRefs(got), sortRefs(refs)) {
		t.Fatalf("round trip refs %+v, want %+v", got, refs)
	}
	if !reflect.DeepEqual(BlockSentences(marked), BlockSentences(b)) {
		t.Fatalf("round trip text %q, want %q", BlockSentences(marked), BlockSentences(b))
	}
}

func TestExtractPackageRefs_AndLimits(t *testing.T) {
	long := strings.Repeat("x", 140) + " first [[c:" + cU1 + "@1]]. " + strings.Repeat("y", 120) + " second. " + strings.Repeat("z", 40) + " third [[c:" + cU2 + "@3]]."
	pk := packageResult{Title: "APR vs rate [[c:" + cU1 + "@1]]", Excerpt: long, Subjects: []string{strings.Repeat("s", 95)}}
	pk.ClaimRefs = extractPackageRefs(&pk, []Claim{{ClaimID: cU1, Version: 1}, {ClaimID: cU2, Version: 3}})
	if pk.Title != "APR vs rate" || strings.Contains(pk.Excerpt, "[[") {
		t.Fatalf("markers must be stripped: title %q excerpt %q", pk.Title, pk.Excerpt)
	}
	if len(pk.Excerpt) <= MaxExcerptLen {
		t.Fatalf("test setup: excerpt must overrun (%d)", len(pk.Excerpt))
	}
	enforcePackageLimits(&pk)
	if len(pk.Excerpt) > MaxExcerptLen || len(SplitSentences(pk.Excerpt)) != 2 {
		t.Fatalf("excerpt must keep whole leading sentences within %d: %d bytes, %d sentences", MaxExcerptLen, len(pk.Excerpt), len(SplitSentences(pk.Excerpt)))
	}
	for _, r := range pk.ClaimRefs {
		if r.BlockID == UnitExcerpt && r.SentenceIdx >= 2 {
			t.Fatalf("a ref to a trimmed sentence must be dropped: %+v", r)
		}
	}
	want := []ClaimRef{{BlockID: UnitTitle, SentenceIdx: 0, ClaimID: cU1, Version: 1}, {BlockID: UnitExcerpt, SentenceIdx: 0, ClaimID: cU1, Version: 1}}
	if !reflect.DeepEqual(sortRefs(pk.ClaimRefs), sortRefs(want)) {
		t.Fatalf("package refs %+v, want %+v", pk.ClaimRefs, want)
	}
	if len(pk.Subjects[0]) > MaxSubjectLen || !strings.HasSuffix(pk.Subjects[0], "…") {
		t.Fatalf("subject must be cut to fit: %q", pk.Subjects[0])
	}
}

// Wiring: the draft stage asks for no claim_refs and computes them from markers.
func TestDraftStage_ComputesRefsFromMarkers(t *testing.T) {
	if _, ok := draftSchema()["properties"].(map[string]any)["claim_refs"]; ok {
		t.Fatal("the draft schema must not ask the model for claim_refs")
	}
	llm := &fakeLLM{out: `{"blocks":[{"id":"l1","type":"lede","heading":"","text":"Fees apply [[c:` + cU1 + `@1]]. Plain close.","items":[],"rows":[],"calc":null,"caption":""}]}`}
	p := &Pipeline{LLM: llm}
	res, _, err := p.draft(context.Background(), PipelineInput{OrgID: "o"}, []Claim{{ClaimID: cU1, Version: 1, Status: ClaimSupported}})
	if err != nil {
		t.Fatal(err)
	}
	d := res.(draftResult)
	if d.Blocks[0].Text != "Fees apply. Plain close." {
		t.Fatalf("markers must be stripped from stored text: %q", d.Blocks[0].Text)
	}
	if want := []ClaimRef{{BlockID: "l1", SentenceIdx: 0, ClaimID: cU1, Version: 1}}; !reflect.DeepEqual(d.ClaimRefs, want) {
		t.Fatalf("refs %+v, want %+v", d.ClaimRefs, want)
	}
}
