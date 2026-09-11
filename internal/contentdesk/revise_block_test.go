package contentdesk

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func reviseClaims() []Claim {
	return []Claim{{ClaimID: cU1, Version: 1, Status: ClaimSupported}, {ClaimID: cU2, Version: 3, Status: ClaimSupported}}
}

// Only the flagged block is rewritten; the rest of the article and its refs
// are untouched, and the rewrite's markers become refs on the forced id.
func TestReviseBlock_OnlyFlaggedBlocksAreRewrittenAndMerged(t *testing.T) {
	llm := &fakeLLM{out: `{"block":{"id":"wrong","type":"lede","heading":"","text":"Rates vary by lender and term [[c:` + cU1 + `@1]].","items":[],"rows":[],"calc":null,"caption":""}}`}
	p := &Pipeline{LLM: llm}
	a := testAssessment() // b1 lede "APR includes lender fees."; b2 section "Rates" / "Rates vary by lender."
	a.refs = []ClaimRef{{BlockID: "b1", SentenceIdx: 0, ClaimID: cU2, Version: 3}}
	a.judgment = append(a.judgment, JudgmentItem{ID: "j2", Kind: "claim", BlockID: "b2", SentenceIdx: 1, Verdict: "overstated", LostQualifier: "and term"})
	res, _, err := p.revise(context.Background(), PipelineInput{OrgID: "o"}, a, reviseClaims())
	if err != nil {
		t.Fatal(err)
	}
	if len(llm.reqs) != 1 {
		t.Fatalf("only the flagged block may be sent: %d calls", len(llm.reqs))
	}
	if _, ok := llm.reqs[0].Schema["properties"].(map[string]any)["block"]; !ok || !llm.reqs[0].NoThinking {
		t.Fatalf("a block rewrite asks for one block, without thinking: %+v", llm.reqs[0])
	}
	d := res.(draftResult)
	if len(d.Blocks) != 2 || d.Blocks[0].Text != "APR includes lender fees." {
		t.Fatalf("the unflagged block must be untouched: %+v", d.Blocks)
	}
	b2 := d.Blocks[1]
	if b2.ID != "b2" || b2.Type != "section" || b2.Heading != "Rates" || b2.Text != "Rates vary by lender and term." {
		t.Fatalf("rewrite must keep id, type and heading, with markers stripped: %+v", b2)
	}
	want := []ClaimRef{{BlockID: "b1", SentenceIdx: 0, ClaimID: cU2, Version: 3}, {BlockID: "b2", SentenceIdx: 1, ClaimID: cU1, Version: 1}}
	if !reflect.DeepEqual(sortRefs(d.ClaimRefs), sortRefs(want)) {
		t.Fatalf("refs %+v, want %+v", d.ClaimRefs, want)
	}
}

// Live 2026-09-11: the reviser returned a single empty block. An unusable
// rewrite keeps the original block and its refs.
func TestReviseBlock_EmptyRewriteKeepsTheOriginal(t *testing.T) {
	llm := &fakeLLM{out: `{"block":{"id":"","type":"comparison_table","heading":"","text":"","items":[],"rows":[],"calc":null,"caption":""}}`}
	p := &Pipeline{LLM: llm}
	a := testAssessment()
	a.refs = []ClaimRef{{BlockID: "b2", SentenceIdx: 1, ClaimID: cU1, Version: 1}}
	a.judgment = append(a.judgment, JudgmentItem{ID: "j2", Kind: "claim", BlockID: "b2", SentenceIdx: 1, Verdict: "unsupported"})
	res, _, err := p.revise(context.Background(), PipelineInput{OrgID: "o"}, a, reviseClaims())
	if err != nil {
		t.Fatal(err)
	}
	d := res.(draftResult)
	if !reflect.DeepEqual(d.Blocks, a.pkg.Blocks) || !reflect.DeepEqual(d.ClaimRefs, a.refs) {
		t.Fatalf("an empty rewrite must leave the article unchanged: %+v / %+v", d.Blocks, d.ClaimRefs)
	}
}

// A code-check failure is pinned to the unit its detail names, so the
// per-block reviser knows where to fix it.
func TestFindings_PinCodeCheckDetailsToTheirBlock(t *testing.T) {
	a := testAssessment()
	a.code = append(a.code, CheckResult{Name: "claim_refs_resolve", Severity: "S1",
		Details: []string{"ref[11] b2#1 → " + cU2 + "@2: claim status is \"conflicting\", not supported"}})
	var got *reviseFinding
	for _, f := range a.findings() {
		if f.Problem == "code check claim_refs_resolve (S1)" {
			f := f
			got = &f
		}
	}
	if got == nil || got.BlockID != "b2" || got.SentenceIdx != 1 || got.Sentence != "Rates vary by lender." {
		t.Fatalf("finding must name block b2 sentence 1: %+v", got)
	}
}

// Package-level findings (title, subjects…) never reach a block rewrite.
func TestReviseBlock_PackageFindingsAreNotSentToBlocks(t *testing.T) {
	llm := &fakeLLM{out: `{"block":{}}`}
	p := &Pipeline{LLM: llm}
	a := testAssessment()
	a.judgment = append(a.judgment, JudgmentItem{ID: "j3", Kind: "headline_overpromise", BlockID: "title", Verdict: "flag"})
	if _, _, err := p.revise(context.Background(), PipelineInput{OrgID: "o"}, a, reviseClaims()); err != nil {
		t.Fatal(err)
	}
	if len(llm.reqs) != 0 {
		t.Fatalf("a title finding must not trigger a block rewrite: %d calls", len(llm.reqs))
	}
}

// Live :1131: a rewrite added an uncited factual sentence that became the
// article's last hard blocker. The reviser's rules forbid new uncited facts
// and name cutting as a correct fix, and they are in the revise cache key.
func TestReviseSystem_NoNewUncitedFacts(t *testing.T) {
	if !strings.Contains(reviseSystem, "Add no new factual sentence unless it carries a marker") ||
		!strings.Contains(reviseSystem, "cutting the sentence is a correct fix") {
		t.Fatal("the reviser must be told not to add uncited facts and that cutting is a correct fix")
	}
	if !strings.Contains(revisePromptVersion, "nonew") {
		t.Fatalf("the rule change must be in the revise cache key: %s", revisePromptVersion)
	}
}
