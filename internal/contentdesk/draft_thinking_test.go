package contentdesk

import (
	"context"
	"testing"
)

// Live 2026-09-11 repro (54-claim fixture): with the model's default thinking
// the draft hit max_tokens at 24,000 output tokens (68k-char thinking block);
// with thinking disabled the same request ended at 5,688 tokens, 11 blocks,
// 1,176 words. The draft stage must keep sending it disabled.
func TestDraftStage_DisablesThinking(t *testing.T) {
	llm := &fakeLLM{out: `{"blocks":[],"claim_refs":[]}`}
	p := &Pipeline{LLM: llm}
	if _, _, err := p.draft(context.Background(), PipelineInput{OrgID: "o"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(llm.reqs) != 1 {
		t.Fatalf("want one draft request, got %d", len(llm.reqs))
	}
	if r := llm.reqs[0]; !r.NoThinking || r.MaxTokens != draftMaxTokens || r.Tier != TierWrite {
		t.Fatalf("draft request must be TierWrite, NoThinking, draftMaxTokens: %+v", r)
	}
}
