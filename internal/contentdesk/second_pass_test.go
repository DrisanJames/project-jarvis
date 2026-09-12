package contentdesk

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSecondPassBlocking_OnlyWhatNoAgentMayAccept(t *testing.T) {
	items := []JudgmentItem{
		{ID: "j1", Kind: "claim", Verdict: "supported"},
		{ID: "j2", Kind: "claim", Verdict: "overstated"},
		{ID: "j3", Kind: "unreferenced_claim", Verdict: "flag"},
		{ID: "j4", Kind: "low_usefulness", Verdict: "flag"},
	}
	b := secondPassBlocking(items)
	if len(b) != 2 || b[0].ID != "j2" || b[1].ID != "j3" {
		t.Fatalf("blocking %+v", b)
	}
}

// Live :1135/:1136: the second pass objected and nothing acted on it. Now the
// objected sentence is cut, the article re-assessed, and the next second pass
// comes back clean — on the new revision.
func TestSecondPassConverge_CutsWhatItWillNotPassThenClean(t *testing.T) {
	pkg := Package{Title: "T", Blocks: []Block{{ID: "l1", Type: "lede", Text: "A holds. B holds. C holds."}}}
	refs := []ClaimRef{
		{BlockID: "l1", SentenceIdx: 0, ClaimID: cU1, Version: 1},
		{BlockID: "l1", SentenceIdx: 1, ClaimID: cU2, Version: 3},
		{BlockID: "l1", SentenceIdx: 2, ClaimID: cU1, Version: 1},
	}
	a := assessment{pkg: pkg, refs: refs, revHash: "h1"}
	llm := &seqLLM{outs: []string{
		`{"items":[{"ref_index":0,"verdict":"supported","lost_qualifier":"","note":""},{"ref_index":1,"verdict":"overstated","lost_qualifier":"x","note":"too broad"},{"ref_index":2,"verdict":"supported","lost_qualifier":"","note":""}],"flags":[]}`,
		`{"items":[{"ref_index":0,"verdict":"supported","lost_qualifier":"","note":""},{"ref_index":1,"verdict":"supported","lost_qualifier":"","note":""}],"flags":[]}`,
	}}
	assess := func(raw json.RawMessage, d draftResult, prev *assessment, pk *packageResult) (assessment, error) {
		return assessment{pkg: Package{Title: pk.Title, Blocks: d.Blocks}, refs: append(d.ClaimRefs, pk.ClaimRefs...), revHash: "h2"}, nil
	}
	res := (&Pipeline{LLM: llm}).secondPassConverge(context.Background(), PipelineInput{OrgID: "o"}, "a1", &a, nil, assess)
	if res == nil || !res.clean || res.revHash != "h2" {
		t.Fatalf("must converge clean on the trimmed revision: %+v", res)
	}
	if a.pkg.Blocks[0].Text != "A holds. C holds." || len(llm.reqs) != 2 {
		t.Fatalf("B must be cut and the second pass re-run once: %q, %d calls", a.pkg.Blocks[0].Text, len(llm.reqs))
	}
}

// An objection nothing can cut (the title) leaves the article held.
func TestSecondPassConverge_UncuttableObjectionIsHeld(t *testing.T) {
	pkg := Package{Title: "Big claim", Blocks: []Block{{ID: "l1", Type: "lede", Text: "A holds."}}}
	refs := []ClaimRef{{BlockID: UnitTitle, SentenceIdx: 0, ClaimID: cU1, Version: 1}, {BlockID: "l1", SentenceIdx: 0, ClaimID: cU1, Version: 1}}
	llm := &seqLLM{outs: []string{`{"items":[{"ref_index":0,"verdict":"overstated","lost_qualifier":"","note":"n"},{"ref_index":1,"verdict":"supported","lost_qualifier":"","note":""}],"flags":[]}`}}
	a := assessment{pkg: pkg, refs: refs, revHash: "h1"}
	res := (&Pipeline{LLM: llm}).secondPassConverge(context.Background(), PipelineInput{OrgID: "o"}, "a1", &a, nil,
		func(json.RawMessage, draftResult, *assessment, *packageResult) (assessment, error) {
			t.Fatal("nothing was cut, so there is nothing to re-assess")
			return assessment{}, nil
		})
	if res != nil {
		t.Fatalf("an uncuttable objection must hold the article: %+v", res)
	}
}
