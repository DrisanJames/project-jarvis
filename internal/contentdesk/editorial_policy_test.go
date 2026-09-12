package contentdesk

import (
	"context"
	"encoding/json"
	"testing"
)

const (
	policyJudge  = `{"items":[{"ref_index":0,"verdict":"supported","lost_qualifier":"","note":""},{"ref_index":1,"verdict":"supported","lost_qualifier":"","note":""}],"flags":[{"kind":"headline_overpromise","block_id":"l1","sentence_idx":1,"note":"promises more than the body"}]}`
	policyRefuse = `{"decisions":[{"id":"j3","reason":"The heading promises a comparison the body never computes.","verdict":"refuse"}]}`
)

func policyArticle() assessment {
	return assessment{
		pkg:     Package{Title: "T", Blocks: []Block{{ID: "l1", Type: "lede", Text: "A holds. B holds."}}},
		refs:    []ClaimRef{{BlockID: "l1", SentenceIdx: 0, ClaimID: cU1, Version: 1}, {BlockID: "l1", SentenceIdx: 1, ClaimID: cU2, Version: 3}},
		revHash: "h1",
	}
}

// Operator 2026-09-12: the gate is the material facts. An editorial flag the
// managing editor refuses is recorded with its reason and does not block —
// nothing is cut for it, and the second pass comes back clean on claims.
func TestSecondPassConverge_EditorialRefusalRecordedNotBlocking(t *testing.T) {
	t.Setenv(EnvEditorialBlocks, "")
	a := policyArticle()
	llm := &seqLLM{outs: []string{policyJudge, policyRefuse}}
	res := (&Pipeline{LLM: llm}).secondPassConverge(context.Background(), PipelineInput{OrgID: "o"}, "a1", &a, nil,
		func(json.RawMessage, draftResult, *assessment, *packageResult) (assessment, error) {
			t.Fatal("an editorial refusal must not cut anything")
			return assessment{}, nil
		})
	if res == nil || !res.clean || len(res.findings) != 1 {
		t.Fatalf("clean on claims with the refusal recorded: %+v", res)
	}
	if f := res.findings[0]; f.Severity != "S3" || f.BlockID != "l1" {
		t.Fatalf("refusal must be an S3 finding on its unit: %+v", f)
	}
}

// With CONTENT_DESK_EDITORIAL_BLOCKS=1 the old gate holds: the refused flag
// is cut (here the only cut would empty the lede, so nothing is cuttable and
// the article is held).
func TestSecondPassConverge_EditorialBlocksRestoresTheOldGate(t *testing.T) {
	t.Setenv(EnvEditorialBlocks, "1")
	a := policyArticle()
	llm := &seqLLM{outs: []string{policyJudge, policyRefuse}}
	res := (&Pipeline{LLM: llm}).secondPassConverge(context.Background(), PipelineInput{OrgID: "o"}, "a1", &a, nil,
		func(raw json.RawMessage, d draftResult, prev *assessment, pk *packageResult) (assessment, error) {
			return assessment{pkg: Package{Title: pk.Title, Blocks: d.Blocks}, refs: append(d.ClaimRefs, pk.ClaimRefs...), revHash: "h2"}, nil
		})
	if res != nil && res.clean && res.revHash == "h1" {
		t.Fatalf("with the old gate a refused flag must not pass untouched: %+v", res)
	}
}

func TestEditorialFlagsBlock_DefaultsOff(t *testing.T) {
	t.Setenv(EnvEditorialBlocks, "")
	if EditorialFlagsBlock() {
		t.Fatal("default: editorial flags do not block")
	}
	t.Setenv(EnvEditorialBlocks, "1")
	if !EditorialFlagsBlock() {
		t.Fatal("1 restores the old gate")
	}
}
