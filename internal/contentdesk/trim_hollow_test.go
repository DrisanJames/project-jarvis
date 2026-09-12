package contentdesk

import "testing"

// Live :1148: repeated cuts left a heading over one sentence and the managing
// editor refused it. A section a cut leaves at one sentence is dropped whole,
// with its references; the lede and the last two blocks are never dropped.
func TestTrimFlagged_DropsASectionLeftWithOneSentence(t *testing.T) {
	a := assessment{
		pkg: Package{Blocks: []Block{
			{ID: "l1", Type: "lede", Text: "A holds. B holds."},
			{ID: "s1", Type: "section", Heading: "Three layers", Text: "C holds. D holds."},
			{ID: "s2", Type: "section", Heading: "More", Text: "E holds. F holds."},
		}},
		refs: []ClaimRef{ref("l1", 0, cU1, 1), ref("s1", 2, cU2, 3), ref("s2", 1, cU1, 1)},
		judgment: []JudgmentItem{
			{ID: "j1", Kind: "claim", BlockID: "s1", SentenceIdx: 1, Verdict: "overstated"}, // C: leaves only D
		},
	}
	d, _, n := trimFlagged(a)
	if len(d.Blocks) != 2 || d.Blocks[0].ID != "l1" || d.Blocks[1].ID != "s2" || n != 3 {
		t.Fatalf("s1 must be dropped whole (heading + 2 sentences): n=%d blocks=%+v", n, d.Blocks)
	}
	assertRefsLand(t, d, map[ClaimRef]string{ref("l1", 0, cU1, 1): "A holds.", ref("s2", 1, cU1, 1): "E holds."})
}

func TestTrimFlagged_NeverDropsBelowTwoBlocksOrTheLede(t *testing.T) {
	a := assessment{
		pkg: Package{Blocks: []Block{
			{ID: "l1", Type: "lede", Text: "A holds. B holds."},
			{ID: "s1", Type: "section", Heading: "Only", Text: "C holds. D holds."},
		}},
		judgment: []JudgmentItem{
			{ID: "j1", Kind: "claim", BlockID: "s1", SentenceIdx: 1, Verdict: "overstated"},
			{ID: "j2", Kind: "claim", BlockID: "l1", SentenceIdx: 0, Verdict: "overstated"},
		},
	}
	d, _, _ := trimFlagged(a)
	if len(d.Blocks) != 2 || d.Blocks[0].Text != "B holds." || d.Blocks[1].Text != "D holds." {
		t.Fatalf("two blocks must remain, trimmed not dropped: %+v", d.Blocks)
	}
}
