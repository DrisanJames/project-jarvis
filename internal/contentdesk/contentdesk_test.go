package contentdesk

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	tClaim = "11111111-1111-1111-1111-111111111111"
	tOther = "22222222-2222-2222-2222-222222222222"
)

// goodFixture is a package that passes EVERY code check; each negative test
// mutates exactly one thing and asserts exactly the check that must fail.
func goodFixture() CheckInput {
	pkg := Package{
		Title:           "How the IRA contribution limit works",
		Excerpt:         "Most savers can contribute $7,000.",
		MetaTitle:       "IRA contribution limit",
		MetaDescription: "What the IRA contribution limit is and who it applies to.",
		Subjects:        []string{"Your IRA limit, explained"},
		Preheaders:      []string{"Who it applies to and when"},
		Blocks: []Block{
			{ID: "b1", Type: "lede", Text: "Most savers can put $7,000 into an IRA for 2026. See [the IRS page](https://www.irs.gov/retirement-plans)."},
			{ID: "b2", Type: "worked_example", Text: "Two spouses can each contribute.",
				Calc: &Calc{Inputs: []CalcInput{{Name: "limit", Value: 7000}, {Name: "people", Value: 2}}, Formula: "limit * people", Result: 14000}},
		},
	}
	raw, _ := json.Marshal(pkg)
	return CheckInput{
		Domain: "financialcalculate.com", SourceDomains: CategoryAllowlist[CatFinance], Package: pkg,
		RawOutputs: [][]byte{raw},
		Refs:       []ClaimRef{{BlockID: "b1", SentenceIdx: 0, ClaimID: tClaim, Version: 1}, {BlockID: UnitExcerpt, SentenceIdx: 0, ClaimID: tClaim, Version: 1}},
		Claims: map[string]Claim{ClaimVersionKey(tClaim, 1): {ClaimID: tClaim, Version: 1, Text: "The 2026 IRA limit is $7,000.",
			Type: ClaimSourcedFact, Status: ClaimSupported, SourceURL: "https://www.irs.gov/retirement-plans", Passage: "The limit is $7,000."}},
		Latest:     map[string]int{tClaim: 1},
		Slug:       "ira-contribution-limit",
		TakenSlugs: []string{"some-other-article"},
	}
}

func checkNamed(t *testing.T, res []CheckResult, name string) CheckResult {
	t.Helper()
	for _, r := range res {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("check %q not in results", name)
	return CheckResult{}
}

func TestCodeChecks_CleanPackagePassesEverything(t *testing.T) {
	for _, r := range RunCodeChecks(goodFixture()) {
		if !r.Passed {
			t.Errorf("clean fixture failed %s: %v", r.Name, r.Details)
		}
	}
}

func TestCodeChecks_NegativePaths(t *testing.T) {
	cases := []struct {
		name   string
		check  string
		mutate func(*CheckInput)
	}{
		{"bad claim ref", "claim_refs_resolve", func(in *CheckInput) {
			in.Refs = append(in.Refs, ClaimRef{BlockID: "b1", SentenceIdx: 0, ClaimID: tOther, Version: 1})
		}},
		{"insufficient_evidence ref", "claim_refs_resolve", func(in *CheckInput) {
			c := in.Claims[ClaimVersionKey(tClaim, 1)]
			c.Status = ClaimInsufficientEvidence
			in.Claims[ClaimVersionKey(tClaim, 1)] = c
		}},
		{"stale claim version", "claim_refs_resolve", func(in *CheckInput) { in.Latest[tClaim] = 2 }},
		{"ref to a sentence that does not exist", "claim_refs_resolve", func(in *CheckInput) { in.Refs[0].SentenceIdx = 9 }},
		{"banned term", "banned_terms", func(in *CheckInput) { in.Package.Blocks[1].Text += " Ask Liberty Mutual." }},
		{"banned term in a subject", "banned_terms", func(in *CheckInput) { in.Package.Subjects[0] = "Save with TruGreen" }},
		{"money host", "no_money_links", func(in *CheckInput) {
			in.Package.Blocks[1].Text += " [deal](https://cratoolpro.com/x?y=1)"
		}},
		{"/o/ gateway link", "no_money_links", func(in *CheckInput) {
			in.Package.Blocks[1].Text += " [go](https://www.financialcalculate.com/o/abc)"
		}},
		{"raw HTML in a block", "no_raw_html", func(in *CheckInput) { in.Package.Blocks[1].Text = "<b>Two</b> spouses can each contribute." }},
		{"HTML entity", "no_raw_html", func(in *CheckInput) { in.Package.Blocks[1].Text = "Two spouses &amp; partners." }},
		{"fabricated field", "no_fabricated_trust", func(in *CheckInput) {
			in.RawOutputs = append(in.RawOutputs, []byte(`{"blocks":[],"meta":{"rating":4.8}}`))
		}},
		{"fabricated trust text", "no_fabricated_trust", func(in *CheckInput) { in.Package.Excerpt += " Rated 4.9 stars by readers." }},
		{"calc mismatch", "calc_recompute", func(in *CheckInput) { in.Package.Blocks[1].Calc.Result = 15000 }},
		{"calc unknown input", "calc_recompute", func(in *CheckInput) { in.Package.Blocks[1].Calc.Formula = "limit * spouses" }},
		{"own link with trailing slash", "url_form", func(in *CheckInput) {
			in.Package.Blocks[1].Text += " [more](https://www.financialcalculate.com/ira/)"
		}},
		{"own link without www", "url_form", func(in *CheckInput) {
			in.Package.Blocks[1].Text += " [more](https://financialcalculate.com/ira)"
		}},
		{"external host off the allow-list", "url_form", func(in *CheckInput) {
			in.Package.Blocks[1].Text += " [x](https://www.example.com/a)"
		}},
		{"slug taken", "slug_unique", func(in *CheckInput) { in.TakenSlugs = append(in.TakenSlugs, "ira-contribution-limit") }},
		{"block type not allowed", "schema", func(in *CheckInput) {
			in.Package.Blocks = append(in.Package.Blocks, Block{ID: "b3", Type: "html", Text: "x"})
		}},
		{"block id reserved", "schema", func(in *CheckInput) { in.Package.Blocks[1].ID = "title" }},
		{"meta description too long", "length_limits", func(in *CheckInput) { in.Package.MetaDescription = strings.Repeat("x", 161) }},
		{"number without a claim ref", "numeric_sentences_referenced", func(in *CheckInput) { in.Package.Blocks[1].Text = "About 40 percent qualify." }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := goodFixture()
			tc.mutate(&in)
			if r := checkNamed(t, RunCodeChecks(in), tc.check); r.Passed {
				t.Fatalf("%s: check %s passed, want failure", tc.name, tc.check)
			}
		})
	}
}

func TestNearDuplicateIsLabelledHeuristic(t *testing.T) {
	in := goodFixture()
	in.Others = []OtherArticle{{ArticleID: "a2", Body: PackageBodyText(in.Package)}}
	r := checkNamed(t, RunCodeChecks(in), "near_duplicate")
	if r.Passed || !r.Heuristic || r.Severity != "S2" {
		t.Fatalf("identical body must fail the heuristic S2 check: %+v", r)
	}
}

func TestRevisionHash_StableUnderRefOrderAndBoundToClaimVersion(t *testing.T) {
	in := goodFixture()
	h1, _ := RevisionHash(in.Package, in.Refs)
	h2, _ := RevisionHash(in.Package, []ClaimRef{in.Refs[1], in.Refs[0]})
	if h1 != h2 || len(h1) != 64 {
		t.Fatalf("hash must ignore ref order: %s vs %s", h1, h2)
	}
	bumped := append([]ClaimRef(nil), in.Refs...)
	bumped[0].Version = 2
	if h3, _ := RevisionHash(in.Package, bumped); h3 == h1 {
		t.Fatal("a claim version change must change the revision hash")
	}
	in.Package.Title += "!"
	if h4, _ := RevisionHash(in.Package, in.Refs); h4 == h1 {
		t.Fatal("a package change must change the revision hash")
	}
}

func TestBriefChangeChangesStageInputHash(t *testing.T) {
	b := Brief{ID: "b", SiteID: "s", Format: "explainer", ReaderQuestion: "q1", Category: CatFinance, Outline: json.RawMessage(`[]`)}
	h1 := mustHash(briefKey(b))
	if mustHash(briefKey(b)) != h1 {
		t.Fatal("same brief must hash the same (idempotent re-run)")
	}
	b.ReaderQuestion = "q2"
	if mustHash(briefKey(b)) == h1 {
		t.Fatal("a changed brief must produce a new input_hash (new run)")
	}
}

// A stage output reused from content_pipeline_runs comes back through JSONB
// (whitespace added, keys reordered — observed on real PG). The next stage's
// input_hash is computed over it, so the hash must not see the difference or
// a resumed pipeline would re-run (and re-pay for) every later stage.
func TestHashJSON_InvariantToJSONBReformatting(t *testing.T) {
	fresh := mustHash(map[string]any{"draft": json.RawMessage(`{"blocks":[{"id":"b1","type":"lede"}],"claim_refs":[]}`)})
	fromDB := mustHash(map[string]any{"draft": json.RawMessage(`{"claim_refs": [], "blocks": [{"type": "lede", "id": "b1"}]}`)})
	if fresh != fromDB {
		t.Fatal("input hash must be stable across a JSONB round trip")
	}
}

func TestEvalFormula(t *testing.T) {
	v, err := EvalFormula("(a + b) * 2 ^ 2 - -1", map[string]float64{"a": 1, "b": 2})
	if err != nil || v != 13 {
		t.Fatalf("got %v %v", v, err)
	}
	if _, err := EvalFormula("a / 0", map[string]float64{"a": 1}); err == nil {
		t.Fatal("division by zero must error")
	}
	if _, err := EvalFormula("a + system()", map[string]float64{"a": 1}); err == nil {
		t.Fatal("anything outside arithmetic must error")
	}
}

func TestSplitSentencesKeepsDecimals(t *testing.T) {
	got := SplitSentences("Rates rose 3.5% in May. Why? Costs!")
	if len(got) != 3 || got[0] != "Rates rose 3.5% in May." {
		t.Fatalf("got %q", got)
	}
}

// ── judgment parsing with a mocked LLM ──

type fakeLLM struct {
	reqs []GenerateRequest
	out  string
	err  error
}

func (f *fakeLLM) Generate(_ context.Context, req GenerateRequest) (*GenerateResult, error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return &GenerateResult{Usage: Usage{Model: "m", USD: 0.01}}, f.err
	}
	return &GenerateResult{JSON: json.RawMessage(f.out), Usage: Usage{Model: "claude-opus-5", USD: 0.25}}, nil
}

func TestJudgment_ParsesVerdictsAndFlagsFromMockedLLM(t *testing.T) {
	in := goodFixture()
	refs := SortClaimRefs(in.Refs)
	llm := &fakeLLM{out: `{"items":[{"ref_index":0,"verdict":"supported","lost_qualifier":"","note":""},
		{"ref_index":1,"verdict":"overstated","lost_qualifier":"for most savers under 50","note":"drops age"}],
		"flags":[{"kind":"headline_overpromise","block_id":"title","sentence_idx":0,"note":"promises a full guide"}]}`}
	p := &Pipeline{LLM: llm}
	res, u, err := p.judge(context.Background(), PipelineInput{OrgID: "o"}, in.Package, refs, in.Claims)
	if err != nil {
		t.Fatal(err)
	}
	items := res.([]JudgmentItem)
	if len(items) != 3 || items[0].ID != "j1" || items[2].ID != "j3" {
		t.Fatalf("items: %+v", items)
	}
	if items[1].Verdict != "overstated" || items[1].LostQualifier == "" || items[1].ClaimID != tClaim {
		t.Fatalf("overstated item wrong: %+v", items[1])
	}
	if items[2].Kind != "headline_overpromise" || items[2].Verdict != "flag" {
		t.Fatalf("flag wrong: %+v", items[2])
	}
	if llm.reqs[0].Tier != TierJudge || llm.reqs[0].WebCategory != "" || u.USD != 0.25 {
		t.Fatalf("judge must use the JUDGE tier with structured output, no web: %+v", llm.reqs[0])
	}
	if !strings.Contains(llm.reqs[0].Prompt, "Most savers can put $7,000 into an IRA for 2026.") {
		t.Fatal("the judge prompt must carry the sentence under test")
	}
}

func TestJudgment_MissingVerdictFailsClosed(t *testing.T) {
	refs := SortClaimRefs(goodFixture().Refs)
	items, err := ParseJudgment(json.RawMessage(`{"items":[{"ref_index":0,"verdict":"supported","lost_qualifier":"","note":""}],"flags":[]}`), refs)
	if err != nil {
		t.Fatal(err)
	}
	if items[1].Verdict != "unsupported" || !strings.Contains(items[1].Note, "fail closed") {
		t.Fatalf("an unjudged sentence must be unsupported: %+v", items[1])
	}
}

func TestJudgment_RejectsMalformedOutput(t *testing.T) {
	refs := SortClaimRefs(goodFixture().Refs)
	for name, raw := range map[string]string{
		"bad verdict":   `{"items":[{"ref_index":0,"verdict":"probably","lost_qualifier":"","note":""}],"flags":[]}`,
		"out of range":  `{"items":[{"ref_index":7,"verdict":"supported","lost_qualifier":"","note":""}],"flags":[]}`,
		"bad flag kind": `{"items":[],"flags":[{"kind":"vibes","block_id":"b1","sentence_idx":0,"note":""}]}`,
		"not json":      `nope`,
	} {
		if _, err := ParseJudgment(json.RawMessage(raw), refs); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// ── review decisions ──

func TestDecideReview(t *testing.T) {
	clean := Checks{Code: []CheckResult{{Name: "schema", Passed: true, Severity: "S1"}},
		Judgment: []JudgmentItem{{ID: "j1", Kind: "claim", Verdict: "supported"}, {ID: "j2", Kind: "claim", Verdict: "overstated"}}}
	approve := ReviewInput{Reviewer: "ann", Role: "primary", Decision: "approve"}

	if o := DecideReview(clean, false, nil, approve); o.Status != StatusInReview || len(o.Blockers) == 0 {
		t.Fatalf("an unaccepted overstated item must block: %+v", o)
	}
	approve.AcceptedIDs = []string{"j2"}
	if o := DecideReview(clean, false, nil, approve); o.Status != StatusApproved {
		t.Fatalf("accepted items → approved: %+v", o)
	}
	s1 := clean
	s1.Code = []CheckResult{{Name: "banned_terms", Passed: false, Severity: "S1"}}
	a2 := approve
	a2.AcceptedIDs = []string{"j2", "code:banned_terms"}
	if o := DecideReview(s1, false, nil, a2); len(o.Blockers) == 0 {
		t.Fatal("a failed S1 code check cannot be accepted away")
	}
	withS1 := approve
	withS1.Findings = []Finding{{Severity: "S1", CaughtBy: "human", Text: "wrong year"}}
	if o := DecideReview(clean, false, nil, withS1); len(o.Blockers) == 0 {
		t.Fatal("an open S1 finding must block approval")
	}
	// Consequential: primary alone is not enough; the same person as second is not enough.
	if o := DecideReview(clean, true, nil, approve); o.Status != StatusInReview || o.Awaiting == "" {
		t.Fatalf("consequential primary alone must await a second: %+v", o)
	}
	prior := []PriorReview{{Reviewer: "ann", Role: "primary", Decision: "approve"}}
	sameSecond := ReviewInput{Reviewer: "ann", Role: "second", Decision: "approve", AcceptedIDs: []string{"j2"}}
	if o := DecideReview(clean, true, prior, sameSecond); o.Status == StatusApproved {
		t.Fatal("the primary reviewer cannot also be the second")
	}
	second := ReviewInput{Reviewer: "bob", Role: "second", Decision: "approve", AcceptedIDs: []string{"j2"}}
	if o := DecideReview(clean, true, prior, second); o.Status != StatusApproved {
		t.Fatalf("primary + different second → approved: %+v", o)
	}
	if o := DecideReview(clean, false, nil, ReviewInput{Decision: "changes"}); o.Status != StatusChangesRequested {
		t.Fatal("changes → changes_requested")
	}
}

func TestManifestDropGuard(t *testing.T) {
	prev := []ManifestEntry{{ArticleID: "A", Slug: "a"}, {ArticleID: "B", Slug: "b"}}
	next := []ManifestEntry{{ArticleID: "A", Slug: "a"}}
	if v := ManifestDropViolations(prev, next, nil); len(v) != 1 || !strings.Contains(v[0], "B") {
		t.Fatalf("dropping live B must be refused: %v", v)
	}
	if v := ManifestDropViolations(prev, next, map[string]bool{"B": true}); len(v) != 0 {
		t.Fatalf("a withdrawn article may drop: %v", v)
	}
	if !ValidReleaseTransition(ReleaseDeployed, ReleaseLiveConfirmed) || ValidReleaseTransition(ReleasePending, ReleaseLiveConfirmed) {
		t.Fatal("release state machine wrong")
	}
}

// ── rederive / claim normalization ──

func TestAnswersMatchAndCompare(t *testing.T) {
	if !AnswersMatch("$7,000", "7000") || AnswersMatch("7,000", "6,500") || !AnswersMatch("Yes", "yes.") || AnswersMatch("", "") {
		t.Fatal("AnswersMatch wrong")
	}
	keys := []researchRef{{ClaimID: "c0", Version: 1, Answer: "7000"}, {ClaimID: "c1", Version: 1, Answer: "April 15"}, {ClaimID: "c2", Version: 1, Answer: "yes"}}
	got := CompareRederivation(keys, []rederiveAnswer{
		{QuestionIndex: 0, Answer: "$7,000", Status: ClaimSupported},
		{QuestionIndex: 1, Answer: "April 18", Status: ClaimSupported},
	})
	if got[0].Outcome != "match" || got[1].Outcome != "mismatch" || got[2].Outcome != "unconfirmed" {
		t.Fatalf("comparisons: %+v", got)
	}
}

func TestNormalizeClaimOnlyDowngrades(t *testing.T) {
	allow := CategoryAllowlist[CatFinance]
	c, _ := NormalizeClaim(modelClaim{Text: "x", Type: ClaimSourcedFact, Status: ClaimSupported, SourceURL: "https://www.example.com/a", Passage: "p"}, allow, DerivationPrimary)
	if c.Status != ClaimInsufficientEvidence {
		t.Fatal("an off-allow-list source must not stay supported")
	}
	c, _ = NormalizeClaim(modelClaim{Text: "x", Type: ClaimSourcedFact, Status: ClaimSupported, SourceURL: "https://www.irs.gov/a"}, allow, DerivationPrimary)
	if c.Status != ClaimInsufficientEvidence {
		t.Fatal("a supported fact without a passage must be demoted")
	}
	c, _ = NormalizeClaim(modelClaim{Text: "x", Type: ClaimCalculation, Status: ClaimSupported,
		Calc: &Calc{Inputs: []CalcInput{{Name: "a", Value: 2}}, Formula: "a*2", Result: 5}}, allow, DerivationPrimary)
	if c.Status != ClaimConflicting {
		t.Fatal("a calc that does not recompute must be conflicting")
	}
	c, _ = NormalizeClaim(modelClaim{Text: "x", Type: ClaimSourcedFact, Status: ClaimInsufficientEvidence}, allow, DerivationPrimary)
	if c.Status != ClaimInsufficientEvidence {
		t.Fatal("never upgrades")
	}
}

// ── ops report merge ──

func TestMergeReportChecks_PrefixAndStale(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	reports := []OpsReport{
		{Source: "job_watchdog", GeneratedAt: now.Add(-30 * time.Hour), Checks: []OpsCheck{{Name: "nightly", Status: CheckOK}}}, // 30h < 2×26h
		{Source: "site_probes", GeneratedAt: now.Add(-5 * time.Hour), Checks: []OpsCheck{{Name: "db", Status: CheckOK}, {Name: "ht", Status: CheckFail}}},
		{Source: "tracking_sig", GeneratedAt: now.Add(-90 * time.Minute), Checks: []OpsCheck{{Name: "summary", Status: CheckOK}}},
		{Source: "supply_runway", GeneratedAt: now.Add(-time.Hour), Payload: json.RawMessage(`{"consumers":[{"lane":"yahoo_family","days":4}]}`)},
	}
	got := MergeReportChecks(reports, now)
	names := map[string]string{}
	for _, c := range got {
		names[c.Name] = c.Status
	}
	if names["job_watchdog: nightly"] != CheckOK {
		t.Fatalf("fresh report checks must be source-prefixed: %v", names)
	}
	if names["site_probes: stale report (last 5h0m)"] != CheckWarn || names["site_probes: ht"] != "" {
		t.Fatalf("a stale report collapses to ONE warn check: %v", names)
	}
	if _, ok := names["tracking_sig: summary"]; !ok {
		t.Fatalf("90m < 2×1h: tracking_sig is fresh: %v", names)
	}
	if string(SupplyConsumers(reports)) != `[{"lane":"yahoo_family","days":4}]` {
		t.Fatalf("supply consumers: %s", SupplyConsumers(reports))
	}
	stale := MergeReportChecks([]OpsReport{{Source: "tracking_sig", GeneratedAt: now.Add(-3 * time.Hour)}}, now)
	if len(stale) != 1 || stale[0].Status != CheckWarn {
		t.Fatalf("tracking_sig 3h old (cadence 1h) must be stale: %+v", stale)
	}
}

func TestServerChecks(t *testing.T) {
	now := time.Now()
	o := &OpsStatus{}
	o.Spend.BudgetUSD, o.Spend.TodayUSD = 25, 25
	got := ServerChecks(o, ServerSignals{TrackSig: &TrackSigState{Mode: "shadow", Keys: 0,
		Counters: map[string]int64{"click.sig.invalid": 4, "click.sig.valid": 10}}}, now)
	st := map[string]OpsCheck{}
	for _, c := range got {
		st[c.Name] = c
	}
	if st["server: track_sig"].Status != CheckFail || !strings.Contains(st["server: track_sig"].Detail, "invalid=4") {
		t.Fatalf("shadow with zero keys must fail and report invalid: %+v", st["server: track_sig"])
	}
	if st["server: budget"].Status != CheckFail || st["server: content_desk_enabled"].Status != CheckWarn ||
		st["server: preferences_mode"].Status != CheckWarn || st["server: last_pipeline_error"].Status != CheckOK {
		t.Fatalf("server checks: %+v", st)
	}
}

func TestOpsReportValidate(t *testing.T) {
	ok := OpsReport{Source: "site_probes", GeneratedAt: time.Now(), Checks: []OpsCheck{{Name: "db", Status: CheckOK}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := ok
	bad.Checks = []OpsCheck{{Name: "db", Status: "great"}}
	if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatal("unknown status must be rejected")
	}
	bad = ok
	bad.Source = "Site Probes!"
	if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatal("bad source must be rejected")
	}
}
