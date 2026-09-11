package contentdesk

import (
	"context"
	"strings"
	"testing"
)

func incrementalFixture() (assessment, []ClaimRef) {
	pkg := Package{Blocks: []Block{
		{ID: "b1", Type: "lede", Text: "Fees apply. Rates vary."},
		{ID: "b2", Type: "lede", Text: "Terms differ. Caps exist."},
	}}
	refs := []ClaimRef{
		{BlockID: "b1", SentenceIdx: 0, ClaimID: cU1, Version: 1},
		{BlockID: "b1", SentenceIdx: 1, ClaimID: cU2, Version: 3},
		{BlockID: "b2", SentenceIdx: 0, ClaimID: cU1, Version: 1},
		{BlockID: "b2", SentenceIdx: 1, ClaimID: cU2, Version: 3},
	}
	prev := assessment{pkg: pkg, refs: refs, judgment: []JudgmentItem{
		{ID: "j1", Kind: "claim", BlockID: "b1", SentenceIdx: 0, ClaimID: cU1, Version: 1, Verdict: "supported"},
		{ID: "j2", Kind: "claim", BlockID: "b1", SentenceIdx: 1, ClaimID: cU2, Version: 3, Verdict: "overstated", LostQualifier: "by lender"},
		{ID: "j3", Kind: "claim", BlockID: "b2", SentenceIdx: 0, ClaimID: cU1, Version: 1, Verdict: "supported"},
		{ID: "j4", Kind: "claim", BlockID: "b2", SentenceIdx: 1, ClaimID: cU2, Version: 3, Verdict: "unsupported", Note: failClosedNote},
		{ID: "j5", Kind: "low_usefulness", BlockID: "b1", Verdict: "flag"},
	}}
	return prev, refs
}

func changedB2(prev assessment) Package {
	cur := prev.pkg
	cur.Blocks = []Block{prev.pkg.Blocks[0], {ID: "b2", Type: "lede", Text: "Terms differ by lender. Caps exist."}}
	return cur
}

// Only references whose sentence, paragraph and claim version are unchanged
// carry their verdict; a fail-closed placeholder never does.
func TestCarriedVerdicts_OnlyUnchangedContext(t *testing.T) {
	prev, refs := incrementalFixture()
	c := carriedVerdicts(prev, changedB2(prev), refs)
	if len(c) != 2 || c[0].Verdict != "supported" || c[1].Verdict != "overstated" {
		t.Fatalf("only b1's refs are unchanged: %+v", c)
	}
	refs2 := append([]ClaimRef(nil), refs...)
	refs2[0].Version = 2
	if _, ok := carriedVerdicts(prev, prev.pkg, refs2)[0]; ok {
		t.Fatal("a new claim version must be re-judged")
	}
	if _, ok := carriedVerdicts(prev, prev.pkg, refs)[3]; ok {
		t.Fatal("a fail-closed placeholder must be re-judged")
	}
}

// The judge sees only the changed references, is told which sentences
// already carry a judged reference, and the merge keeps positions and order.
func TestJudgeIncremental_JudgesOnlyChangedRefs(t *testing.T) {
	prev, refs := incrementalFixture()
	cur := changedB2(prev)
	llm := &seqLLM{outs: []string{`{"items":[{"ref_index":0,"verdict":"supported","lost_qualifier":"","note":""},{"ref_index":1,"verdict":"supported","lost_qualifier":"","note":""}],"flags":[{"kind":"omitted_exception","block_id":"b2","sentence_idx":1,"note":"x"},{"kind":"unreferenced_claim","block_id":"b1","sentence_idx":0,"note":"re-sampled"}]}`}}
	p := &Pipeline{LLM: llm}
	res, _, err := p.judgeIncremental(context.Background(), PipelineInput{OrgID: "o"}, cur, refs, nil, carriedVerdicts(prev, cur, refs), &prev)
	if err != nil {
		t.Fatal(err)
	}
	items := res.([]JudgmentItem)
	if len(items) != 6 {
		t.Fatalf("4 refs + b2's fresh flag + b1's carried flag: %+v", items)
	}
	if items[5].Kind != "low_usefulness" || items[5].BlockID != "b1" || items[5].ID != "j6" {
		t.Fatalf("an unchanged unit keeps its previous flag: %+v", items[5])
	}
	for _, it := range items {
		if it.Note == "re-sampled" {
			t.Fatalf("a fresh flag on an unchanged unit must be dropped (live :1133): %+v", it)
		}
	}
	if items[0].Verdict != "supported" || items[1].Verdict != "overstated" || items[1].LostQualifier != "by lender" {
		t.Fatalf("b1's verdicts must carry: %+v", items[:2])
	}
	if items[2].Verdict != "supported" || items[3].Verdict != "supported" || items[3].BlockID != "b2" || items[3].SentenceIdx != 1 {
		t.Fatalf("b2's refs must take the fresh verdicts at their positions: %+v", items[2:4])
	}
	if items[4].Kind != "omitted_exception" || items[4].ID != "j5" || items[0].ID != "j1" {
		t.Fatalf("fresh flags follow the refs, numbered on: %+v", items)
	}
	pr := llm.reqs[0].Prompt
	if !strings.Contains(pr, "There are 2 references") || !strings.Contains(pr, "already carry a judged reference") || !strings.Contains(pr, "b1#0, b1#1") {
		t.Fatalf("the judge must see only the changed refs and the already-referenced sentences:\n%s", pr)
	}
}

// With nothing carried it is the full judge.
func TestJudgeIncremental_NothingCarriedIsAFullJudgment(t *testing.T) {
	pkg, refs := tenRefs()
	llm := &seqLLM{outs: []string{verdicts(10)}}
	res, _, err := (&Pipeline{LLM: llm}).judgeIncremental(context.Background(), PipelineInput{OrgID: "o"}, pkg, refs, nil, nil, nil)
	if err != nil || judgedRefs(res.([]JudgmentItem)) != 10 || strings.Contains(llm.reqs[0].Prompt, "already carry a judged reference") {
		t.Fatalf("err=%v", err)
	}
}
