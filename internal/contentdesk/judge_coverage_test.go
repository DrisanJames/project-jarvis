package contentdesk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// seqLLM returns its outputs in order, repeating the last.
type seqLLM struct {
	outs []string
	reqs []GenerateRequest
}

func (s *seqLLM) Generate(_ context.Context, req GenerateRequest) (*GenerateResult, error) {
	i := len(s.reqs)
	s.reqs = append(s.reqs, req)
	if i >= len(s.outs) {
		i = len(s.outs) - 1
	}
	return &GenerateResult{JSON: json.RawMessage(s.outs[i]), Usage: Usage{Model: "claude-opus-5", USD: 0.25}}, nil
}

func tenRefs() (Package, []ClaimRef) {
	var sents []string
	var refs []ClaimRef
	for i := 0; i < 10; i++ {
		sents = append(sents, fmt.Sprintf("Fact %d holds.", i))
		refs = append(refs, ClaimRef{BlockID: "b1", SentenceIdx: i, ClaimID: cU1, Version: 1})
	}
	return Package{Blocks: []Block{{ID: "b1", Type: "lede", Text: strings.Join(sents, " ")}}}, refs
}

// verdicts returns a judgment with supported verdicts for refs 0..n-1.
func verdicts(n int) string {
	var items []string
	for i := 0; i < n; i++ {
		items = append(items, fmt.Sprintf(`{"ref_index":%d,"verdict":"supported","lost_qualifier":"","note":""}`, i))
	}
	return `{"items":[` + strings.Join(items, ",") + `],"flags":[{"kind":"low_usefulness","block_id":"b1","sentence_idx":0,"note":"x"}]}`
}

// Live 2026-09-11: the judge sometimes returned a verdict for 1 reference and
// only flags. That call must be re-run, not recorded as 9 unsupported.
func TestJudge_ReRunsACollapsedJudgment(t *testing.T) {
	pkg, refs := tenRefs()
	llm := &seqLLM{outs: []string{verdicts(1), verdicts(10)}}
	p := &Pipeline{LLM: llm}
	res, u, err := p.judge(context.Background(), PipelineInput{OrgID: "o"}, pkg, refs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(llm.reqs) != 2 {
		t.Fatalf("a collapsed judgment must be re-run once: %d calls", len(llm.reqs))
	}
	items := res.([]JudgmentItem)
	if judgedRefs(items) != 10 {
		t.Fatalf("the re-run's verdicts must be kept: %d judged", judgedRefs(items))
	}
	if u.USD != 0.5 {
		t.Fatalf("both calls must be counted: %v", u.USD)
	}
}

func TestJudge_FailsTheStageAfterRepeatedCollapse(t *testing.T) {
	pkg, refs := tenRefs()
	llm := &seqLLM{outs: []string{verdicts(1)}}
	p := &Pipeline{LLM: llm}
	if _, _, err := p.judge(context.Background(), PipelineInput{OrgID: "o"}, pkg, refs, nil); !errors.Is(err, ErrNoStructuredOutput) {
		t.Fatalf("repeated collapse must fail the stage, not record fabricated verdicts: %v", err)
	}
	if len(llm.reqs) != judgeAttempts {
		t.Fatalf("want %d attempts, got %d", judgeAttempts, len(llm.reqs))
	}
}

// Live 2026-09-11: "ref_index 0 returned twice" failed whole judgments. A
// duplicate keeps the stricter verdict, in either order.
func TestParseJudgment_DuplicateKeepsTheStricterVerdict(t *testing.T) {
	_, refs := tenRefs()
	for _, raw := range []string{
		`{"items":[{"ref_index":0,"verdict":"supported","lost_qualifier":"","note":""},{"ref_index":0,"verdict":"unsupported","lost_qualifier":"","note":"no"}],"flags":[]}`,
		`{"items":[{"ref_index":0,"verdict":"unsupported","lost_qualifier":"","note":"no"},{"ref_index":0,"verdict":"supported","lost_qualifier":"","note":""}],"flags":[]}`,
	} {
		items, err := ParseJudgment(json.RawMessage(raw), refs)
		if err != nil {
			t.Fatal(err)
		}
		if items[0].Verdict != "unsupported" || items[0].Note != "no" {
			t.Fatalf("a duplicate must keep the stricter verdict: %+v", items[0])
		}
	}
}

// Malformed output is re-run like a collapsed one.
func TestJudge_ReRunsMalformedOutput(t *testing.T) {
	pkg, refs := tenRefs()
	llm := &seqLLM{outs: []string{`nope`, verdicts(10)}}
	p := &Pipeline{LLM: llm}
	if _, _, err := p.judge(context.Background(), PipelineInput{OrgID: "o"}, pkg, refs, nil); err != nil || len(llm.reqs) != 2 {
		t.Fatalf("malformed output must be re-run: err=%v calls=%d", err, len(llm.reqs))
	}
	bad := &seqLLM{outs: []string{`nope`}}
	if _, _, err := (&Pipeline{LLM: bad}).judge(context.Background(), PipelineInput{OrgID: "o"}, pkg, refs, nil); !errors.Is(err, ErrNoStructuredOutput) || len(bad.reqs) != judgeAttempts {
		t.Fatalf("repeated malformed output must fail after %d attempts: err=%v calls=%d", judgeAttempts, err, len(bad.reqs))
	}
}

// 9 of 10 meets the bar; the missing one still fails closed.
func TestJudge_AcceptsNearFullCoverage(t *testing.T) {
	pkg, refs := tenRefs()
	llm := &seqLLM{outs: []string{verdicts(9)}}
	p := &Pipeline{LLM: llm}
	res, _, err := p.judge(context.Background(), PipelineInput{OrgID: "o"}, pkg, refs, nil)
	if err != nil || len(llm.reqs) != 1 {
		t.Fatalf("90%% coverage must pass in one call: err=%v calls=%d", err, len(llm.reqs))
	}
	items := res.([]JudgmentItem)
	if items[9].Verdict != "unsupported" || items[9].Note != failClosedNote {
		t.Fatalf("the missing reference must still fail closed: %+v", items[9])
	}
	if !strings.Contains(llm.reqs[0].Prompt, "There are 10 references") {
		t.Fatal("the judge prompt must state the number of references")
	}
}
