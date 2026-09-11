package contentdesk

import (
	"reflect"
	"testing"
)

func trimFixture() assessment {
	return assessment{
		pkg: Package{Blocks: []Block{
			{ID: "s1", Type: "section", Heading: "Rates", Text: "A holds. B holds. C holds.", Items: []string{"Item one."}},
			{ID: "s2", Type: "section", Text: "Only sentence."},
		}},
		refs: []ClaimRef{
			{BlockID: "s1", SentenceIdx: 1, ClaimID: cU1, Version: 1},    // A
			{BlockID: "s1", SentenceIdx: 3, ClaimID: cU2, Version: 3},    // C
			{BlockID: "s1", SentenceIdx: 4, ClaimID: cU1, Version: 1},    // the item
			{BlockID: "title", SentenceIdx: 0, ClaimID: cU1, Version: 1}, // package ref: rebuilt by the packager
		},
		judgment: []JudgmentItem{
			{ID: "j1", Kind: "unreferenced_claim", BlockID: "s1", SentenceIdx: 2, Verdict: "flag"}, // B
			{ID: "j2", Kind: "claim", BlockID: "s2", SentenceIdx: 0, Verdict: "unsupported"},       // a block's only sentence
			{ID: "j3", Kind: "headline_overpromise", BlockID: "title", Verdict: "flag"},
			{ID: "j4", Kind: "claim", BlockID: "s1", SentenceIdx: 0, Verdict: "unsupported"}, // the heading
			{ID: "j5", Kind: "claim", BlockID: "s1", SentenceIdx: 1, Verdict: "overstated"},  // not cut: fixable
		},
	}
}

// Live :1132: an uncited sentence the reviser kept for 4 rounds was
// myownhealth's only hard blocker. Trim cuts exactly the flagged text
// sentence, never a heading, item or a block's last sentence, and every
// remaining reference still points at the sentence it cited.
func TestTrimFlagged_CutsFlaggedTextSentencesAndShiftsRefs(t *testing.T) {
	d, n := trimFlagged(trimFixture())
	if n != 1 {
		t.Fatalf("exactly B must be cut: %d", n)
	}
	if d.Blocks[0].Text != "A holds. C holds." || d.Blocks[0].Heading != "Rates" || d.Blocks[1].Text != "Only sentence." {
		t.Fatalf("blocks %+v", d.Blocks)
	}
	want := []ClaimRef{
		{BlockID: "s1", SentenceIdx: 1, ClaimID: cU1, Version: 1},
		{BlockID: "s1", SentenceIdx: 2, ClaimID: cU2, Version: 3},
		{BlockID: "s1", SentenceIdx: 3, ClaimID: cU1, Version: 1},
	}
	if !reflect.DeepEqual(d.ClaimRefs, want) {
		t.Fatalf("refs %+v, want %+v", d.ClaimRefs, want)
	}
	bs := BlockSentences(d.Blocks[0])
	if bs[1] != "A holds." || bs[2] != "C holds." || bs[3] != "Item one." {
		t.Fatalf("each ref must still land on its sentence: %q", bs)
	}
}

// A cut sentence's own reference goes with it.
func TestTrimFlagged_DropsTheCutSentencesRef(t *testing.T) {
	a := trimFixture()
	a.judgment = []JudgmentItem{{ID: "j1", Kind: "claim", BlockID: "s1", SentenceIdx: 3, Verdict: "unsupported"}} // C
	d, n := trimFlagged(a)
	if n != 1 || d.Blocks[0].Text != "A holds. B holds." {
		t.Fatalf("C must be cut: n=%d %q", n, d.Blocks[0].Text)
	}
	for _, r := range d.ClaimRefs {
		if r.ClaimID == cU2 {
			t.Fatalf("the cut sentence's ref must be dropped: %+v", d.ClaimRefs)
		}
	}
}

func TestTrimFlagged_NothingToCut(t *testing.T) {
	a := trimFixture()
	a.judgment = []JudgmentItem{{ID: "j1", Kind: "low_usefulness", BlockID: "s1", SentenceIdx: 1, Verdict: "flag"}}
	if _, n := trimFlagged(a); n != 0 {
		t.Fatalf("an adjudicable flag must not be cut: %d", n)
	}
}
