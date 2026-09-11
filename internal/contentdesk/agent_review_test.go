package contentdesk

import (
	"context"
	"strings"
	"testing"
)

func testAssessment() assessment {
	return assessment{
		pkg: Package{Blocks: []Block{
			{ID: "b1", Type: "lede", Text: "APR includes lender fees."},
			{ID: "b2", Type: "section", Heading: "Rates", Text: "Rates vary by lender."},
		}},
		code:     []CheckResult{{Name: "length_limits", Passed: true, Severity: "S2"}},
		judgment: []JudgmentItem{{ID: "j1", Kind: "claim", BlockID: "b1", SentenceIdx: 0, Verdict: "supported"}},
	}
}

func TestAssessment_CleanOnlyWithoutBlockers(t *testing.T) {
	a := testAssessment()
	if !a.clean() || len(a.findings()) != 0 {
		t.Fatalf("all checks passed and every claim supported must be clean: %v", a.blockers())
	}

	// A section's unit 0 is its heading (Units, as the judge sees it); the
	// body sentence is unit 1.
	a.judgment = append(a.judgment, JudgmentItem{ID: "j2", Kind: "claim", BlockID: "b2", SentenceIdx: 1,
		Verdict: "overstated", LostQualifier: "for most borrowers"})
	if a.clean() {
		t.Fatal("an overstated sentence must block")
	}
	f := a.findings()
	if len(f) != 1 || f[0].Problem != "overstated" || f[0].BlockID != "b2" || !strings.Contains(f[0].Detail, "for most borrowers") {
		t.Fatalf("finding: %+v", f)
	}
	if !strings.Contains(f[0].Sentence, "Rates vary by lender") {
		t.Fatalf("finding must carry the flagged sentence: %+v", f[0])
	}

	b := testAssessment()
	b.code[0].Passed = false
	if b.clean() || !strings.Contains(b.findings()[0].Problem, "length_limits") {
		t.Fatalf("a failed code check must block and be sent to the writer: %+v", b.findings())
	}

	c := testAssessment()
	c.judgment = append(c.judgment, JudgmentItem{ID: "j3", Kind: "headline_overpromise", BlockID: "title", Verdict: "flag", Note: "promises a full guide"})
	if c.clean() || c.findings()[0].Problem != "headline_overpromise" {
		t.Fatalf("a judgment flag must block: %+v", c.findings())
	}
}

func TestSecondPassClean(t *testing.T) {
	if secondPassClean(nil) {
		t.Fatal("nothing to verify is not a pass")
	}
	ok := []JudgmentItem{{Kind: "claim", Verdict: "supported"}, {Kind: "claim", Verdict: "supported"}}
	if !secondPassClean(ok) {
		t.Fatal("all supported must pass")
	}
	if secondPassClean(append(ok, JudgmentItem{Kind: "claim", Verdict: "overstated"})) {
		t.Fatal("an overstated item must fail the second pass")
	}
	if secondPassClean(append(ok, JudgmentItem{Kind: "omitted_exception", Verdict: "flag"})) {
		t.Fatal("a flag must fail the second pass")
	}
}

func TestAgentReviewEnabled_DefaultsOff(t *testing.T) {
	t.Setenv(EnvAgentReview, "")
	if AgentReviewEnabled() {
		t.Fatal("unset must mean humans review")
	}
	t.Setenv(EnvAgentReview, "1")
	if !AgentReviewEnabled() {
		t.Fatal("1 must enable agent review")
	}
}

func TestRevise_SendsFindingsWithoutThinking(t *testing.T) {
	llm := &fakeLLM{out: `{"blocks":[],"claim_refs":[]}`}
	p := &Pipeline{LLM: llm}
	a := testAssessment()
	a.judgment = append(a.judgment, JudgmentItem{ID: "j2", Kind: "claim", BlockID: "b2", SentenceIdx: 0,
		Verdict: "unsupported", Note: "the passage does not say this"})
	if _, _, err := p.revise(context.Background(), PipelineInput{OrgID: "o"}, a, nil); err != nil {
		t.Fatal(err)
	}
	if len(llm.reqs) != 1 {
		t.Fatalf("want one revise request, got %d", len(llm.reqs))
	}
	r := llm.reqs[0]
	if r.Tier != TierWrite || !r.NoThinking || r.MaxTokens != draftMaxTokens || !strings.HasPrefix(r.System, draftSystem) {
		t.Fatalf("revise must be TierWrite, no thinking, draft budget, draft rules: %+v", r)
	}
	if !strings.Contains(r.Prompt, "the passage does not say this") || !strings.Contains(r.Prompt, "Rates vary by lender") {
		t.Fatal("the revise prompt must carry the findings and the current draft")
	}
	if (&Pipeline{}).maxReviseRounds() != defaultReviseRounds {
		t.Fatal("zero MaxReviseRounds must mean the default")
	}
}
