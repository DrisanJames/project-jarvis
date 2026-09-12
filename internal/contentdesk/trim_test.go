package contentdesk

import (
	"reflect"
	"testing"
)

func ref(block string, idx int, claim string, v int) ClaimRef {
	return ClaimRef{BlockID: block, SentenceIdx: idx, ClaimID: claim, Version: v}
}

// Every remaining reference must still point at the sentence it cited.
func assertRefsLand(t *testing.T, d draftResult, want map[ClaimRef]string) {
	t.Helper()
	byID := map[string]Block{}
	for _, b := range d.Blocks {
		byID[b.ID] = b
	}
	if len(d.ClaimRefs) != len(want) {
		t.Fatalf("refs %+v, want %d", d.ClaimRefs, len(want))
	}
	for _, r := range d.ClaimRefs {
		s, ok := want[r]
		if !ok {
			t.Fatalf("unexpected ref %+v (want %+v)", r, want)
		}
		if bs := BlockSentences(byID[r.BlockID]); r.SentenceIdx >= len(bs) || bs[r.SentenceIdx] != s {
			t.Fatalf("ref %+v must land on %q: %q", r, s, bs)
		}
	}
}

// Live :1132: an uncited text sentence the reviser kept for 4 rounds. Trim
// cuts exactly that sentence — never the heading — and re-indexes refs,
// including the ones on items after the text.
func TestTrimFlagged_TextSentence(t *testing.T) {
	a := assessment{
		pkg:  Package{Blocks: []Block{{ID: "s1", Type: "section", Heading: "Rates", Text: "A holds. B holds. C holds.", Items: []string{"Item one."}}}},
		refs: []ClaimRef{ref("s1", 1, cU1, 1), ref("s1", 3, cU2, 3), ref("s1", 4, cU1, 1), ref(UnitTitle, 0, cU1, 1)},
		judgment: []JudgmentItem{
			{ID: "j1", Kind: "unreferenced_claim", BlockID: "s1", SentenceIdx: 2, Verdict: "flag"}, // B
			{ID: "j2", Kind: "claim", BlockID: "s1", SentenceIdx: 0, Verdict: "unsupported"},       // heading: never cut
			{ID: "j3", Kind: "headline_overpromise", BlockID: UnitTitle, Verdict: "flag"},
		},
	}
	d, pk, n := trimFlagged(a)
	if n != 1 || d.Blocks[0].Text != "A holds. C holds." || d.Blocks[0].Heading != "Rates" {
		t.Fatalf("exactly B must be cut: n=%d %+v", n, d.Blocks[0])
	}
	assertRefsLand(t, d, map[ClaimRef]string{ref("s1", 1, cU1, 1): "A holds.", ref("s1", 2, cU2, 3): "C holds.", ref("s1", 3, cU1, 1): "Item one."})
	if !reflect.DeepEqual(pk.ClaimRefs, []ClaimRef{ref(UnitTitle, 0, cU1, 1)}) {
		t.Fatalf("package refs must be kept: %+v", pk.ClaimRefs)
	}
}

// An overstated claim no rewrite fixed is cut too, with its own reference.
func TestTrimFlagged_OverstatedSentenceAndItsRef(t *testing.T) {
	a := assessment{
		pkg:      Package{Blocks: []Block{{ID: "l1", Type: "lede", Text: "A holds. B holds."}}},
		refs:     []ClaimRef{ref("l1", 0, cU1, 1), ref("l1", 1, cU2, 3)},
		judgment: []JudgmentItem{{ID: "j1", Kind: "claim", BlockID: "l1", SentenceIdx: 0, Verdict: "overstated"}},
	}
	d, _, n := trimFlagged(a)
	if n != 1 || d.Blocks[0].Text != "B holds." {
		t.Fatalf("A must be cut: n=%d %q", n, d.Blocks[0].Text)
	}
	assertRefsLand(t, d, map[ClaimRef]string{ref("l1", 0, cU2, 3): "B holds."})
}

// Live :1134: an overstated FAQ answer. The whole question/answer pair goes,
// so no question is left without its answer.
func TestTrimFlagged_FAQPair(t *testing.T) {
	a := assessment{
		pkg: Package{Blocks: []Block{{ID: "faq-1", Type: "faq", Items: []string{
			"Where do DVs come from?", "They are set by FDA.", "Is it per serving?", "Yes. Always per serving.",
		}}}},
		// units: 0 Q1, 1 A1, 2 Q2, 3 A2a, 4 A2b
		refs:     []ClaimRef{ref("faq-1", 1, cU1, 1), ref("faq-1", 4, cU2, 3)},
		judgment: []JudgmentItem{{ID: "j1", Kind: "claim", BlockID: "faq-1", SentenceIdx: 1, Verdict: "overstated"}},
	}
	d, _, n := trimFlagged(a)
	if n != 2 || !reflect.DeepEqual(d.Blocks[0].Items, []string{"Is it per serving?", "Yes. Always per serving."}) {
		t.Fatalf("the first pair must go: n=%d %q", n, d.Blocks[0].Items)
	}
	assertRefsLand(t, d, map[ClaimRef]string{ref("faq-1", 2, cU2, 3): "Always per serving."})
}

// Never the last FAQ pair, the last list item, or a block's last sentence.
func TestTrimFlagged_NeverEmptiesABlock(t *testing.T) {
	a := assessment{
		pkg: Package{Blocks: []Block{
			{ID: "faq-1", Type: "faq", Items: []string{"Only question?", "Only answer."}},
			{ID: "st-1", Type: "steps", Items: []string{"Only step."}},
			{ID: "l1", Type: "lede", Text: "Only sentence."},
		}},
		judgment: []JudgmentItem{
			{ID: "j1", Kind: "claim", BlockID: "faq-1", SentenceIdx: 1, Verdict: "overstated"},
			{ID: "j2", Kind: "unreferenced_claim", BlockID: "st-1", SentenceIdx: 0, Verdict: "flag"},
			{ID: "j3", Kind: "claim", BlockID: "l1", SentenceIdx: 0, Verdict: "unsupported"},
		},
	}
	if d, _, n := trimFlagged(a); n != 0 || !reflect.DeepEqual(d.Blocks, a.pkg.Blocks) {
		t.Fatalf("nothing may be cut: n=%d %+v", n, d.Blocks)
	}
}

// A flagged list item goes on its own; the others keep their refs.
func TestTrimFlagged_ListItem(t *testing.T) {
	a := assessment{
		pkg:      Package{Blocks: []Block{{ID: "st-1", Type: "steps", Items: []string{"One.", "Two.", "Three."}}}},
		refs:     []ClaimRef{ref("st-1", 0, cU1, 1), ref("st-1", 2, cU2, 3)},
		judgment: []JudgmentItem{{ID: "j1", Kind: "unreferenced_claim", BlockID: "st-1", SentenceIdx: 1, Verdict: "flag"}},
	}
	d, _, n := trimFlagged(a)
	if n != 1 || !reflect.DeepEqual(d.Blocks[0].Items, []string{"One.", "Three."}) {
		t.Fatalf("Two must go: n=%d %q", n, d.Blocks[0].Items)
	}
	assertRefsLand(t, d, map[ClaimRef]string{ref("st-1", 0, cU1, 1): "One.", ref("st-1", 1, cU2, 3): "Three."})
}

// Live :1135: discountblog held on an overstated preheader. A flagged subject
// or preheader alternate is dropped and the survivors renumbered — never the
// last one, and never the title, excerpt or meta.
func TestTrimFlagged_SubjectAndPreheaderAlternates(t *testing.T) {
	a := assessment{
		pkg: Package{Title: "T", Excerpt: "E.", MetaTitle: "M", MetaDescription: "D.",
			Subjects: []string{"S0", "S1", "S2"}, Preheaders: []string{"P0"},
			Blocks: []Block{{ID: "l1", Type: "lede", Text: "A holds."}}},
		refs: []ClaimRef{ref(UnitTitle, 0, cU1, 1), ref(SubjectUnit(1), 0, cU1, 1), ref(SubjectUnit(2), 0, cU2, 3), ref(PreheaderUnit(0), 0, cU2, 3)},
		judgment: []JudgmentItem{
			{ID: "j1", Kind: "claim", BlockID: SubjectUnit(1), SentenceIdx: 0, Verdict: "overstated"},
			{ID: "j2", Kind: "claim", BlockID: PreheaderUnit(0), SentenceIdx: 0, Verdict: "overstated"}, // the only preheader: kept
			{ID: "j3", Kind: "claim", BlockID: UnitTitle, SentenceIdx: 0, Verdict: "overstated"},        // title: never cut
		},
	}
	_, pk, n := trimFlagged(a)
	if n != 1 || !reflect.DeepEqual(pk.Subjects, []string{"S0", "S2"}) || !reflect.DeepEqual(pk.Preheaders, []string{"P0"}) ||
		pk.Title != "T" || pk.Excerpt != "E." || pk.MetaTitle != "M" || pk.MetaDescription != "D." {
		t.Fatalf("exactly subject 1 must go: n=%d %+v", n, pk)
	}
	want := []ClaimRef{ref(UnitTitle, 0, cU1, 1), ref(SubjectUnit(1), 0, cU2, 3), ref(PreheaderUnit(0), 0, cU2, 3)}
	if !reflect.DeepEqual(pk.ClaimRefs, want) {
		t.Fatalf("package refs %+v, want %+v", pk.ClaimRefs, want)
	}
}

// Live :1135: financialcalculate's table caption claimed an APR difference it
// never computed. The flagged caption sentence is cut (a caption is
// optional); the heading and rows stay, and refs on the rows keep landing.
func TestTrimFlagged_CaptionSentence(t *testing.T) {
	a := assessment{
		pkg: Package{Blocks: []Block{{ID: "cmp-1", Type: "comparison_table", Heading: "Two loans",
			Rows:    [][]string{{"Loan", "Charge"}, {"A", "$978"}},
			Caption: "Both carry $978. So the APRs differ."}}},
		// units: 0 heading, 1-2 rows, 3-4 caption sentences
		refs:     []ClaimRef{ref("cmp-1", 2, cU1, 1), ref("cmp-1", 3, cU1, 1), ref("cmp-1", 4, cU2, 3)},
		judgment: []JudgmentItem{{ID: "j1", Kind: "claim", BlockID: "cmp-1", SentenceIdx: 4, Verdict: "overstated"}},
	}
	d, _, n := trimFlagged(a)
	if n != 1 || d.Blocks[0].Caption != "Both carry $978." || d.Blocks[0].Heading != "Two loans" || len(d.Blocks[0].Rows) != 2 {
		t.Fatalf("exactly the second caption sentence must go: n=%d %+v", n, d.Blocks[0])
	}
	assertRefsLand(t, d, map[ClaimRef]string{ref("cmp-1", 2, cU1, 1): "A | $978", ref("cmp-1", 3, cU1, 1): "Both carry $978."})
}

// Live :1140: a trim pass that held hard blockers level was rejected as "no
// improvement" and the article stalled. A pass is kept unless it adds hard
// blockers or newly fails a code check.
func TestTrimAcceptable_KeepsLevelPassesRejectsWorse(t *testing.T) {
	hard := func(n int) assessment {
		a := assessment{pkg: Package{Blocks: []Block{{ID: "l1", Type: "lede", Text: "A holds."}}},
			code: []CheckResult{{Name: "length_limits", Passed: true, Severity: "S2"}}}
		for i := 0; i < n; i++ {
			a.judgment = append(a.judgment, JudgmentItem{ID: "j" + string(rune('1'+i)), Kind: "claim", BlockID: "l1", Verdict: "unsupported"})
		}
		return a
	}
	if !trimAcceptable(hard(1), hard(1)) {
		t.Fatal("a pass that holds hard blockers level must be kept (it converges)")
	}
	if !trimAcceptable(hard(2), hard(0)) {
		t.Fatal("a pass that clears hard blockers must be kept")
	}
	if trimAcceptable(hard(1), hard(2)) {
		t.Fatal("a pass that adds a hard blocker must be rejected")
	}
	broke := hard(0)
	broke.code = []CheckResult{{Name: "length_limits", Passed: false, Severity: "S2"}}
	if trimAcceptable(hard(1), broke) {
		t.Fatal("a pass that newly fails a code check must be rejected")
	}
}

// Live :1142: the managing editor refused an omitted_exception on an FAQ
// answer and the article stalled. trimWhere cuts exactly what the editor
// refused — here the whole question/answer pair — and trimFlagged still never
// cuts an editorial flag on its own.
func TestTrimWhere_CutsWhatTheEditorRefused(t *testing.T) {
	a := assessment{
		pkg:  Package{Blocks: []Block{{ID: "faq-1", Type: "faq", Items: []string{"Q1?", "A1.", "Q2?", "A2."}}}},
		refs: []ClaimRef{ref("faq-1", 3, cU1, 1)},
		judgment: []JudgmentItem{
			{ID: "j5", Kind: "omitted_exception", BlockID: "faq-1", SentenceIdx: 1, Verdict: "flag"},
			{ID: "j6", Kind: "low_usefulness", BlockID: "faq-1", SentenceIdx: 3, Verdict: "flag"},
		},
	}
	d, _, n := trimWhere(a, func(j JudgmentItem) bool { return j.ID == "j5" })
	if n != 2 || !reflect.DeepEqual(d.Blocks[0].Items, []string{"Q2?", "A2."}) {
		t.Fatalf("the refused pair must go: n=%d %q", n, d.Blocks[0].Items)
	}
	assertRefsLand(t, d, map[ClaimRef]string{ref("faq-1", 1, cU1, 1): "A2."})
	if _, _, n := trimFlagged(a); n != 0 {
		t.Fatalf("trimFlagged must not cut editorial flags: %d", n)
	}
}

// Editorial flags are for the managing editor, never cut.
func TestTrimFlagged_NothingToCut(t *testing.T) {
	a := assessment{
		pkg:      Package{Subjects: []string{"S0", "S1"}, Blocks: []Block{{ID: "l1", Type: "lede", Text: "A holds. B holds."}}},
		judgment: []JudgmentItem{{ID: "j1", Kind: "low_usefulness", BlockID: "l1", SentenceIdx: 1, Verdict: "flag"}},
	}
	if _, _, n := trimFlagged(a); n != 0 {
		t.Fatalf("an adjudicable flag must not be cut: %d", n)
	}
}
