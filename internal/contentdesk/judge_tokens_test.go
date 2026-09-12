package contentdesk

import (
	"context"
	"testing"
)

// Live :1147: the adversarial second pass on two 40+ reference articles was
// truncated at 16000 tokens (thinking counts against the cap) and held with
// no verdict. Every judge call now asks for judgeMaxTokens.
func TestJudgeCall_UsesTheJudgeTokenCeiling(t *testing.T) {
	pkg := Package{Title: "T", Blocks: []Block{{ID: "l1", Type: "lede", Text: "A holds."}}}
	refs := []ClaimRef{{BlockID: "l1", SentenceIdx: 0, ClaimID: cU1, Version: 1}}
	llm := &seqLLM{outs: []string{`{"items":[{"ref_index":0,"verdict":"supported","lost_qualifier":"","note":""}],"flags":[]}`}}
	if _, _, err := (&Pipeline{LLM: llm}).judgeCall(context.Background(), PipelineInput{OrgID: "o"}, secondReviewSystem, pkg, refs, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(llm.reqs) != 1 || llm.reqs[0].MaxTokens < 24000 {
		t.Fatalf("judge must ask for at least 24000 tokens: %+v", llm.reqs)
	}
}
