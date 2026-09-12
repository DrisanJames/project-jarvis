package contentdesk

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// Only S2 heuristic checks and editorial flags may be adjudicated; claims,
// unreferenced_claim, S1 and non-heuristic checks stay hard blockers.
func TestAdjudicable_OnlyHeuristicsAndEditorialFlags(t *testing.T) {
	a := testAssessment()
	a.code = append(a.code,
		CheckResult{Name: "numeric_sentences_referenced", Severity: "S2", Heuristic: true, Details: []string{"meta_description#0 has a number but no claim ref: \"x\""}},
		CheckResult{Name: "near_duplicate", Severity: "S2"},
		CheckResult{Name: "claim_refs_resolve", Severity: "S1"})
	a.judgment = append(a.judgment,
		JudgmentItem{ID: "j2", Kind: "headline_overpromise", BlockID: "title", Verdict: "flag", Note: "promises a full guide"},
		JudgmentItem{ID: "j3", Kind: "claim", BlockID: "b2", SentenceIdx: 1, Verdict: "overstated"},
		JudgmentItem{ID: "j4", Kind: "unreferenced_claim", BlockID: "b1", Verdict: "flag"},
		JudgmentItem{ID: "j5", Kind: "omitted_exception", BlockID: "b2", SentenceIdx: 1, Verdict: "flag"})
	var ids []string
	for _, it := range a.adjudicable() {
		ids = append(ids, it.ID)
	}
	if want := []string{"code:numeric_sentences_referenced", "j2", "j5"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("adjudicable %v, want %v", ids, want)
	}
	if got := a.hardBlockers(); got != 4 { // near_duplicate, claim_refs_resolve, j3, j4
		t.Fatalf("hard blockers %d, want 4: %v", got, a.hardBlockerList())
	}
	if len(a.blockers()) != 7 {
		t.Fatalf("total blockers %d, want 7", len(a.blockers()))
	}
}

// A revise round that fixes a claim is kept even if the judge re-samples more
// editorial flags; one that adds a hard blocker is not, whatever the flags do.
func TestAcceptRevision_HardBlockersRankFirst(t *testing.T) {
	flag := func(id string) JudgmentItem {
		return JudgmentItem{ID: id, Kind: "low_usefulness", BlockID: "b1", Verdict: "flag"}
	}
	prev := testAssessment()
	prev.judgment = append(prev.judgment, JudgmentItem{ID: "j2", Kind: "claim", BlockID: "b2", SentenceIdx: 1, Verdict: "overstated"}, flag("j3"))
	fixedClaimMoreFlags := testAssessment()
	fixedClaimMoreFlags.judgment = append(fixedClaimMoreFlags.judgment, flag("j3"), flag("j4"), flag("j5"))
	if !acceptRevision(prev, fixedClaimMoreFlags) {
		t.Fatal("fewer hard blockers must be accepted even with more flags")
	}
	fewerFlagsNewClaim := testAssessment()
	fewerFlagsNewClaim.judgment = append(fewerFlagsNewClaim.judgment,
		JudgmentItem{ID: "j2", Kind: "claim", BlockID: "b2", SentenceIdx: 1, Verdict: "overstated"},
		JudgmentItem{ID: "j6", Kind: "claim", BlockID: "b1", Verdict: "unsupported"})
	if acceptRevision(prev, fewerFlagsNewClaim) {
		t.Fatal("more hard blockers must be rejected even with fewer flags")
	}
	sameHardFewerFlags := testAssessment()
	sameHardFewerFlags.judgment = append(sameHardFewerFlags.judgment, JudgmentItem{ID: "j2", Kind: "claim", BlockID: "b2", SentenceIdx: 1, Verdict: "overstated"})
	if !acceptRevision(prev, sameHardFewerFlags) {
		t.Fatal("equal hard blockers and fewer in total must be accepted")
	}
}

// Accepted only with accept=true AND a written reason; missing and refused
// ids stay in refused; every decision — acceptance or refusal — is recorded
// as an S3 finding on its unit (a refusal no longer blocks by default, so its
// reason must survive on the review).
func TestAdjudicate_NeedsAcceptanceWithAReason(t *testing.T) {
	llm := &fakeLLM{out: `{"decisions":[
		{"id":"code:numeric_sentences_referenced","reason":"the meta description restates the cited body sentence in [b1] verbatim","verdict":"accept"},
		{"id":"j2","reason":"the title promises a complete guide; the body covers two cases","verdict":"refuse"},
		{"id":"j5","reason":"ok","verdict":"accept"}]}`}
	p := &Pipeline{LLM: llm}
	items := []adjItem{{ID: "code:numeric_sentences_referenced", Kind: "code check numeric_sentences_referenced"},
		{ID: "j2", Kind: "headline_overpromise", Unit: "title#0"}, {ID: "j5", Kind: "omitted_exception", Unit: "b2#1"}, {ID: "j7", Kind: "low_usefulness", Unit: "b1#0"}}
	acc, fs, refused, err := p.adjudicate(context.Background(), PipelineInput{OrgID: "o"}, testAssessment().pkg, items)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(acc, []string{"code:numeric_sentences_referenced"}) || !reflect.DeepEqual(refused, []string{"j2", "j5", "j7"}) {
		t.Fatalf("accepted %v refused %v", acc, refused)
	}
	if len(fs) != 4 || fs[0].Severity != "S3" || fs[0].CaughtBy != "judgment" || !strings.Contains(fs[0].Text, "restates the cited body sentence") {
		t.Fatalf("finding %+v", fs)
	}
	for i, want := range []struct{ block, id string }{{"title", "j2"}, {"b2", "j5"}, {"b1", "j7"}} {
		f := fs[i+1]
		if f.Severity != "S3" || f.BlockID != want.block || !strings.Contains(f.Text, "editor refused "+want.id) {
			t.Fatalf("refusal %s must be recorded on %s: %+v", want.id, want.block, f)
		}
	}
	r := llm.reqs[0]
	if r.Tier != TierJudge || r.System != adjudicateSystem || !strings.Contains(r.Prompt, "Rates vary by lender") {
		t.Fatalf("adjudication must be the judge tier, with the article: %+v", r)
	}
	// The accepted ids really clear the approval gate, and nothing else does.
	a := testAssessment()
	a.code = append(a.code, CheckResult{Name: "numeric_sentences_referenced", Severity: "S2", Heuristic: true})
	if b := ApprovalBlockers(Checks{Code: a.code, Judgment: a.judgment}, nil, ReviewInput{AcceptedIDs: acc, Findings: fs}); len(b) != 0 {
		t.Fatalf("accepted heuristic must clear the gate: %v", b)
	}
}

// Findings on package units go to the packager with the previous package;
// block findings do not.
func TestPackageFindings_GoToThePackager(t *testing.T) {
	a := testAssessment()
	a.pkg.Title = "The complete guide to APR"
	a.judgment = append(a.judgment,
		JudgmentItem{ID: "j2", Kind: "headline_overpromise", BlockID: "title", Verdict: "flag", Note: "promises a complete guide"},
		JudgmentItem{ID: "j3", Kind: "claim", BlockID: "b2", SentenceIdx: 1, Verdict: "overstated"})
	fs := a.packageFindings()
	if len(fs) != 1 || fs[0].BlockID != "title" {
		t.Fatalf("package findings %+v", fs)
	}
	p := packageRevisePrompt(PipelineInput{}, draftResult{Blocks: a.pkg.Blocks}, nil, a.pkg, fs)
	if !strings.Contains(p, "The complete guide to APR") || !strings.Contains(p, "promises a complete guide") {
		t.Fatalf("the package prompt must carry the previous package and its findings:\n%s", p)
	}
}

// A brief created before its category became consequential still takes the
// two-reviewer path at the review gate.
func TestConsequentialSQL_CoversCategories(t *testing.T) {
	if !strings.Contains(consequentialSQL, "b.consequential") || !strings.Contains(consequentialSQL, "'"+CatFinance+"'") || !strings.Contains(consequentialSQL, "'"+CatHealth+"'") {
		t.Fatalf("consequentialSQL must OR the stored flag with the consequential categories: %s", consequentialSQL)
	}
	if strings.Contains(consequentialSQL, "'"+CatHistory+"'") {
		t.Fatalf("history is not consequential: %s", consequentialSQL)
	}
}
