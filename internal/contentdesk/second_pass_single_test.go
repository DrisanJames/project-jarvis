package contentdesk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Live :1145: a batch cut stripped context and raised new hard items, and
// the article was held. Now the batch is retried one unit at a time: cutting
// B and C together cascades, cutting B alone holds up, and the next second
// pass comes back clean.
func TestSecondPassConverge_RetriesOneUnitAtATimeAfterABatchCascade(t *testing.T) {
	pkg := Package{Title: "T", Blocks: []Block{{ID: "l1", Type: "lede", Text: "A holds. B holds. C holds. D holds."}}}
	refs := []ClaimRef{
		{BlockID: "l1", SentenceIdx: 0, ClaimID: cU1, Version: 1},
		{BlockID: "l1", SentenceIdx: 1, ClaimID: cU2, Version: 3},
		{BlockID: "l1", SentenceIdx: 2, ClaimID: cU2, Version: 3},
		{BlockID: "l1", SentenceIdx: 3, ClaimID: cU1, Version: 1},
	}
	a := assessment{pkg: pkg, refs: refs, revHash: "h1"}
	llm := &seqLLM{outs: []string{
		`{"items":[{"ref_index":0,"verdict":"supported","lost_qualifier":"","note":""},{"ref_index":1,"verdict":"overstated","lost_qualifier":"x","note":"n"},{"ref_index":2,"verdict":"overstated","lost_qualifier":"x","note":"n"},{"ref_index":3,"verdict":"supported","lost_qualifier":"","note":""}],"flags":[]}`,
		`{"items":[{"ref_index":0,"verdict":"supported","lost_qualifier":"","note":""},{"ref_index":1,"verdict":"supported","lost_qualifier":"","note":""},{"ref_index":2,"verdict":"supported","lost_qualifier":"","note":""}],"flags":[]}`,
	}}
	calls := 0
	assess := func(raw json.RawMessage, d draftResult, prev *assessment, pk *packageResult) (assessment, error) {
		calls++
		cand := assessment{pkg: Package{Title: pk.Title, Blocks: d.Blocks}, refs: append(d.ClaimRefs, pk.ClaimRefs...), revHash: fmt.Sprintf("h%d", calls+1)}
		if txt := d.Blocks[0].Text; !strings.Contains(txt, "B holds.") && !strings.Contains(txt, "C holds.") {
			cand.judgment = []JudgmentItem{{ID: "jx", Kind: "claim", BlockID: "l1", SentenceIdx: 0, Verdict: "unsupported"}} // the cascade
		}
		return cand, nil
	}
	res := (&Pipeline{LLM: llm}).secondPassConverge(context.Background(), PipelineInput{OrgID: "o"}, "a1", &a, nil, assess)
	if res == nil || !res.clean || res.revHash != a.revHash {
		t.Fatalf("must converge after a single cut: %+v", res)
	}
	if a.pkg.Blocks[0].Text != "A holds. C holds. D holds." || calls != 2 || len(llm.reqs) != 2 {
		t.Fatalf("batch (cascades) then B alone: text=%q assess=%d judge=%d", a.pkg.Blocks[0].Text, calls, len(llm.reqs))
	}
}
