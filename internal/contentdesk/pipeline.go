package contentdesk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Pipeline runs research → rederive → draft → package → code_checks →
// judgment for one article, each stage idempotent per (article, stage,
// input_hash), then writes the revision and moves the article to in_review.
type Pipeline struct {
	Store             *Store
	LLM               LLM
	MaxAttempts       int
	StaleAfter        time.Duration
	SimhashMaxHamming int
	// MaxReviseRounds bounds the revise loop (0 = defaultReviseRounds).
	MaxReviseRounds int
}

// NewPipeline wires defaults: 3 attempts per stage input, a running row is
// retakeable after 45 min (longer than the worker's 30 min lock TTL).
func NewPipeline(store *Store, llm LLM) *Pipeline {
	return &Pipeline{Store: store, LLM: llm, MaxAttempts: 3, StaleAfter: 45 * time.Minute,
		SimhashMaxHamming: SimhashMaxHammingFromEnv()}
}

func (p *Pipeline) maxAttempts() int {
	if p.MaxAttempts <= 0 {
		return 3
	}
	return p.MaxAttempts
}

func (p *Pipeline) staleAfter() time.Duration {
	if p.StaleAfter <= 0 {
		return 45 * time.Minute
	}
	return p.StaleAfter
}

type stageEnvelope struct {
	Result json.RawMessage `json:"result"`
	Usage  Usage           `json:"usage"`
}

// StageFunc produces a stage result and what it cost.
type StageFunc func(ctx context.Context) (any, Usage, error)

// RunStage is the idempotency unit. A stage whose (article, stage,
// input_hash) already succeeded returns the stored output without calling
// fn; a concurrent claim (double fire) returns ErrInFlight without calling fn.
func (p *Pipeline) RunStage(ctx context.Context, org, articleID, stage, inputHash string, fn StageFunc) (json.RawMessage, Usage, error) {
	claim, err := p.Store.ClaimRun(ctx, org, articleID, stage, inputHash, p.maxAttempts(), p.staleAfter())
	if err != nil {
		return nil, Usage{}, fmt.Errorf("%s: %w", stage, err)
	}
	if claim.Prior != nil {
		var env stageEnvelope
		if err := json.Unmarshal(claim.Prior, &env); err != nil {
			return nil, Usage{}, fmt.Errorf("%s: stored output unreadable: %w", stage, err)
		}
		return env.Result, env.Usage, nil
	}
	res, u, err := fn(ctx)
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err != nil {
		mode := FailCount
		switch {
		case errors.Is(err, ErrDisabled), errors.Is(err, ErrBudgetExceeded), errors.Is(err, ErrBudgetUnavailable),
			errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			mode = FailRefund
		case errors.Is(err, ErrRefused), errors.Is(err, ErrNoAllowlist):
			mode = FailTerminal
		}
		if ferr := p.Store.FailRun(bg, claim.RunID, err.Error(), mode, p.maxAttempts()); ferr != nil {
			log.Printf("[ContentDesk] ERROR step=fail-run article=%s stage=%s: %v", articleID, stage, ferr)
		}
		return nil, u, fmt.Errorf("%s: %w", stage, err)
	}
	b, err := json.Marshal(res)
	if err != nil {
		return nil, u, fmt.Errorf("%s: marshal output: %w", stage, err)
	}
	env, _ := json.Marshal(stageEnvelope{Result: b, Usage: u})
	if err := p.Store.FinishRun(bg, claim.RunID, env); err != nil {
		return nil, u, fmt.Errorf("%s: record output: %w", stage, err)
	}
	return b, u, nil
}

// ── stage payloads ──────────────────────────────────────────────────────

type researchRef struct {
	ClaimID  string `json:"claim_id"`
	Version  int    `json:"version"`
	Key      bool   `json:"key"`
	ClaimKey string `json:"claim_key"`
	Question string `json:"question"`
	Answer   string `json:"answer"`
	Status   string `json:"status"`
}

type researchResult struct {
	Claims []researchRef `json:"claims"`
}

// Comparison is one primary-vs-rederived outcome.
type Comparison struct {
	ClaimID         string `json:"claim_id"`
	PrimaryVersion  int    `json:"primary_version"`
	RederiveClaimID string `json:"rederive_claim_id,omitempty"`
	Outcome         string `json:"outcome"` // match | mismatch | unconfirmed
	Detail          string `json:"detail,omitempty"`
}

type rederiveResult struct {
	Comparisons []Comparison `json:"comparisons"`
	Conflicting []string     `json:"conflicting"`
}

type draftResult struct {
	Blocks    []Block    `json:"blocks"`
	ClaimRefs []ClaimRef `json:"claim_refs"`
}

type packageResult struct {
	Title           string     `json:"title"`
	Excerpt         string     `json:"excerpt"`
	MetaTitle       string     `json:"meta_title"`
	MetaDescription string     `json:"meta_description"`
	Subjects        []string   `json:"subjects"`
	Preheaders      []string   `json:"preheaders"`
	ClaimRefs       []ClaimRef `json:"claim_refs"`
}

func briefKey(b Brief) map[string]any {
	return map[string]any{
		"id": b.ID, "site_id": b.SiteID, "format": b.Format, "reader_question": b.ReaderQuestion,
		"angle": b.Angle, "outline": b.Outline, "category": b.Category, "consequential": b.Consequential,
	}
}

func claimFingerprint(claims []Claim) []string {
	out := make([]string, 0, len(claims))
	for _, c := range claims {
		out = append(out, fmt.Sprintf("%s@%d:%s", c.ClaimID, c.Version, c.Status))
	}
	sort.Strings(out)
	return out
}

func mustHash(v any) string {
	h, err := HashJSON(v)
	if err != nil {
		// Only reachable with an unmarshalable value — a programming error.
		panic(err)
	}
	return h
}

// Run executes the whole pipeline for one drafting article.
func (p *Pipeline) Run(ctx context.Context, org, articleID string) error {
	in, err := p.Store.LoadPipelineInput(ctx, org, articleID)
	if err != nil {
		return err
	}
	var total Usage
	briefHash := mustHash(briefKey(in.Brief))

	// 1. research
	raw, u, err := p.RunStage(ctx, org, articleID, StageResearch, briefHash, func(ctx context.Context) (any, Usage, error) {
		return p.research(ctx, in)
	})
	total.Add(u)
	if err != nil {
		return err
	}
	var research researchResult
	if err := json.Unmarshal(raw, &research); err != nil {
		return fmt.Errorf("research output: %w", err)
	}

	// 2. rederive — its input is the brief plus the key QUESTIONS only.
	keys := keyClaims(research)
	questions := make([]string, len(keys))
	for i, k := range keys {
		questions[i] = k.Question
	}
	_, u, err = p.RunStage(ctx, org, articleID, StageRederive, mustHash(map[string]any{"brief": briefHash, "questions": questions}),
		func(ctx context.Context) (any, Usage, error) { return p.rederive(ctx, in, keys) })
	total.Add(u)
	if err != nil {
		return err
	}

	// Current claim versions (rederive may have appended 'conflicting' versions).
	ids := make([]string, 0, len(research.Claims))
	for _, c := range research.Claims {
		ids = append(ids, c.ClaimID)
	}
	all, err := p.Store.ClaimsByIDs(ctx, org, ids)
	if err != nil {
		return err
	}
	byKey, latest, latestClaim := IndexClaims(all)
	current := make([]Claim, 0, len(latestClaim))
	for _, id := range ids {
		if c, ok := latestClaim[id]; ok {
			current = append(current, c)
		}
	}
	fp := claimFingerprint(current)

	// 3. draft
	raw, u, err = p.RunStage(ctx, org, articleID, StageDraft,
		mustHash(map[string]any{"brief": briefHash, "voice": in.Site.Voice, "claims": fp, "contract": citationContractVersion}),
		func(ctx context.Context) (any, Usage, error) { return p.draft(ctx, in, current) })
	total.Add(u)
	if err != nil {
		return err
	}
	draftRaw := raw
	var draft draftResult
	if err := json.Unmarshal(raw, &draft); err != nil {
		return fmt.Errorf("draft output: %w", err)
	}

	// 4–6. package → code checks → judgment, as one reusable assessment.
	// fix is the previous assessment in a revise round: findings on its
	// package units go back to the packager with the previous package.
	assess := func(draftRaw json.RawMessage, draft draftResult, fix *assessment) (assessment, error) {
		var a assessment
		pkgKey := map[string]any{"draft": draftRaw, "claims": fp, "contract": citationContractVersion}
		var prevPkg Package
		var pkgFix []reviseFinding
		if fix != nil {
			if pkgFix = fix.packageFindings(); len(pkgFix) > 0 {
				prevPkg = fix.pkg
				pkgKey["fix"], pkgKey["prev_revision"], pkgKey["prompt"] = pkgFix, fix.revHash, packageRevisePromptVersion
			}
		}
		raw, u, err := p.RunStage(ctx, org, articleID, StagePackage, mustHash(pkgKey),
			func(ctx context.Context) (any, Usage, error) {
				return p.packageStage(ctx, in, draft, current, prevPkg, pkgFix)
			})
		total.Add(u)
		if err != nil {
			return a, err
		}
		pkgRaw := raw
		var pk packageResult
		if err := json.Unmarshal(raw, &pk); err != nil {
			return a, fmt.Errorf("package output: %w", err)
		}
		a.pkg = Package{Title: pk.Title, Excerpt: pk.Excerpt, MetaTitle: pk.MetaTitle, MetaDescription: pk.MetaDescription,
			Blocks: draft.Blocks, Subjects: pk.Subjects, Preheaders: pk.Preheaders}
		a.refs = SortClaimRefs(append(append([]ClaimRef(nil), draft.ClaimRefs...), pk.ClaimRefs...))
		if a.revHash, err = RevisionHash(a.pkg, a.refs); err != nil {
			return a, err
		}

		// code checks (deterministic)
		taken, err := p.Store.TakenSlugs(ctx, org, in.Site.ID, articleID)
		if err != nil {
			return a, err
		}
		others, err := p.Store.OtherBodies(ctx, org, in.Site.ID, articleID)
		if err != nil {
			return a, err
		}
		otherFP := make([]string, len(others))
		for i, o := range others {
			otherFP[i] = fmt.Sprintf("%s:%x", o.ArticleID, Simhash(o.Body))
		}
		sort.Strings(otherFP)
		checkIn := CheckInput{
			Domain: in.Site.Domain, SourceDomains: CategoryAllowlist[in.Brief.Category], Package: a.pkg,
			RawOutputs: [][]byte{draftRaw, pkgRaw}, Refs: a.refs, Claims: byKey, Latest: latest, Slug: in.Article.Slug,
			TakenSlugs: taken, Others: others, SimhashMaxHamming: p.SimhashMaxHamming,
		}
		raw, _, err = p.RunStage(ctx, org, articleID, StageCodeChecks,
			mustHash(map[string]any{"revision": a.revHash, "claims": claimFingerprint(all), "slug": in.Article.Slug,
				"taken": taken, "others": otherFP, "max_hamming": p.SimhashMaxHamming}),
			func(ctx context.Context) (any, Usage, error) { return RunCodeChecks(checkIn), Usage{}, nil })
		if err != nil {
			return a, err
		}
		if err := json.Unmarshal(raw, &a.code); err != nil {
			return a, fmt.Errorf("code_checks output: %w", err)
		}

		// judgment (JUDGE model)
		// In a revise round, references whose context is unchanged keep their
		// verdict; only changed ones are judged again (judge_incremental.go).
		judgeKey := map[string]any{"revision": a.revHash, "claims": claimFingerprint(all), "judge": judgePromptVersion}
		var carried map[int]JudgmentItem
		if fix != nil {
			if carried = carriedVerdicts(*fix, a.pkg, a.refs); len(carried) > 0 {
				judgeKey["carry_from"] = fix.revHash
			}
		}
		raw, u, err = p.RunStage(ctx, org, articleID, StageJudgment, mustHash(judgeKey),
			func(ctx context.Context) (any, Usage, error) {
				return p.judgeIncremental(ctx, in, a.pkg, a.refs, byKey, carried)
			})
		total.Add(u)
		if err != nil {
			return a, err
		}
		if err := json.Unmarshal(raw, &a.judgment); err != nil {
			return a, fmt.Errorf("judgment output: %w", err)
		}
		return a, nil
	}

	a, err := assess(draftRaw, draft, nil)
	if err != nil {
		return err
	}
	// 7. revise loop: failed code checks and unresolved judgments go back to
	// the writer (only flagged sentences change), then re-assess — until clean
	// or maxReviseRounds. Whatever remains stays a review blocker.
	// The loop runs only under agent review, and it can only keep or improve:
	// a collapsed rewrite is rejected outright, and a revision is kept only if
	// it has strictly fewer blockers than the one before (2026-09-11: without
	// this guard two drafts degraded into 1-block stubs and judge-coverage
	// collapse). The prompt version is in the input hash so a revised prompt
	// never replays revise outputs cached under an old one.
	for round := 1; AgentReviewEnabled() && round <= p.maxReviseRounds() && !a.clean(); round++ {
		prev := a
		raw, u, err = p.RunStage(ctx, org, articleID, StageRevise,
			mustHash(map[string]any{"revision": prev.revHash, "findings": prev.findings(), "claims": fp, "round": round, "prompt": revisePromptVersion}),
			func(ctx context.Context) (any, Usage, error) { return p.revise(ctx, in, prev, current) })
		total.Add(u)
		if err != nil {
			return err
		}
		var next draftResult
		if err := json.Unmarshal(raw, &next); err != nil {
			return fmt.Errorf("revise output: %w", err)
		}
		// A rejected round keeps the previous revision and the next round tries
		// a fresh rewrite of it (the round number is in the revise hash). On
		// :1129 stopping at the first non-improving round left myownhealth at 5
		// hard blockers after 1 of 3 rounds; incremental judgment makes a retry
		// cheap (only changed references are judged).
		if why := degenerateRevision(prev, next); why != "" {
			log.Printf("[ContentDesk] revise article=%s round=%d rejected: %s — keeping the previous revision", articleID, round, why)
			continue
		}
		cand, err := assess(raw, next, &prev)
		if err != nil {
			return err
		}
		if !acceptRevision(prev, cand) {
			log.Printf("[ContentDesk] revise article=%s round=%d rejected: %d blockers (%d hard) vs %d (%d hard) before — keeping the previous revision",
				articleID, round, len(cand.blockers()), cand.hardBlockers(), len(prev.blockers()), prev.hardBlockers())
			continue
		}
		a = cand
	}

	if _, _, err := p.Store.SaveRevision(ctx, org, articleID, a.pkg, a.refs, Checks{Code: a.code, Judgment: a.judgment}, total); err != nil {
		return err
	}
	// 8. agent review (CONTENT_DESK_AGENT_REVIEW=1): approve a clean revision.
	return p.agentReview(ctx, in, org, articleID, a, byKey)
}

func keyClaims(r researchResult) []researchRef {
	var out []researchRef
	for _, c := range r.Claims {
		if c.Key && c.Status == ClaimSupported && strings.TrimSpace(c.Question) != "" {
			out = append(out, c)
		}
	}
	return out
}

// ── research ────────────────────────────────────────────────────────────

type modelClaim struct {
	Text         string `json:"text"`
	Type         string `json:"type"`
	SourceURL    string `json:"source_url"`
	Passage      string `json:"passage"`
	Context      string `json:"context"`
	PublishedAt  string `json:"published_at"`
	EffectiveAt  string `json:"effective_at"`
	Jurisdiction string `json:"jurisdiction"`
	Population   string `json:"population"`
	Conditions   string `json:"conditions"`
	Status       string `json:"status"`
	Key          bool   `json:"key"`
	ClaimKey     string `json:"claim_key"`
	Question     string `json:"question"`
	Answer       string `json:"answer"`
	Calc         *Calc  `json:"calc"`
}

// NormalizeClaim validates a model-produced claim deterministically. It can
// only DOWNGRADE: an unsourced or off-allow-list "supported" fact becomes
// insufficient_evidence; a calculation that does not recompute becomes
// conflicting. It never upgrades.
func NormalizeClaim(m modelClaim, allow []string, derivation string) (Claim, bool) {
	c := Claim{Text: strings.TrimSpace(m.Text), Type: m.Type, SourceURL: strings.TrimSpace(m.SourceURL),
		Passage: strings.TrimSpace(m.Passage), Context: m.Context, PublishedAt: m.PublishedAt, EffectiveAt: m.EffectiveAt,
		Jurisdiction: m.Jurisdiction, Population: m.Population, Conditions: m.Conditions, Status: m.Status,
		Derivation: derivation, Calc: m.Calc, ClaimKey: m.ClaimKey, Question: strings.TrimSpace(m.Question),
		Answer: strings.TrimSpace(m.Answer)}
	if c.Text == "" {
		return c, false
	}
	demote := func(status, why string) {
		c.Status = status
		c.Context = strings.TrimSpace(c.Context + " [desk: " + why + "]")
	}
	switch c.Type {
	case ClaimSourcedFact, ClaimCalculation, ClaimAssumption, ClaimInterpretation:
	default:
		c.Type = ClaimAssumption
		demote(ClaimInsufficientEvidence, "unknown claim type")
	}
	switch c.Status {
	case ClaimSupported, ClaimInsufficientEvidence, ClaimConflicting:
	default:
		demote(ClaimInsufficientEvidence, "unknown status")
	}
	if c.Status == ClaimSupported && c.Type == ClaimSourcedFact {
		u, err := url.Parse(c.SourceURL)
		switch {
		case c.SourceURL == "" || c.Passage == "":
			demote(ClaimInsufficientEvidence, "no source passage")
		case err != nil || u.Scheme != "https" || !hostAllowed(strings.ToLower(u.Host), allow):
			demote(ClaimInsufficientEvidence, "source not on the category allow-list")
		}
	}
	if c.Type == ClaimCalculation {
		if c.Calc == nil {
			demote(ClaimInsufficientEvidence, "calculation without calc")
		} else if _, ok, err := CalcMatches(*c.Calc); err != nil || !ok {
			demote(ClaimConflicting, "calc does not recompute")
		}
	}
	return c, true
}

func (p *Pipeline) research(ctx context.Context, in PipelineInput) (any, Usage, error) {
	res, err := p.LLM.Generate(ctx, GenerateRequest{OrgID: in.OrgID, Tier: TierWrite, System: researchSystem,
		Prompt: researchPrompt(in), Schema: researchSchema(), ToolName: "submit_research",
		WebCategory: in.Brief.Category, MaxTokens: 16000})
	u := resultUsage(res)
	if err != nil {
		return nil, u, err
	}
	var out struct {
		Claims []modelClaim `json:"claims"`
	}
	if err := json.Unmarshal(res.JSON, &out); err != nil {
		return nil, u, fmt.Errorf("%w: %v", ErrNoStructuredOutput, err)
	}
	allow := CategoryAllowlist[in.Brief.Category]
	var claims []Claim
	var keyFlags []bool
	for _, m := range out.Claims {
		c, ok := NormalizeClaim(m, allow, DerivationPrimary)
		if !ok {
			continue
		}
		claims = append(claims, c)
		keyFlags = append(keyFlags, m.Key)
	}
	if len(claims) == 0 {
		return nil, u, fmt.Errorf("%w: research produced no claims", ErrNoStructuredOutput)
	}
	saved, err := p.Store.InsertClaims(ctx, in.OrgID, in.Article.ID, claims)
	if err != nil {
		return nil, u, err
	}
	r := researchResult{}
	for i, c := range saved {
		r.Claims = append(r.Claims, researchRef{ClaimID: c.ClaimID, Version: c.Version, Key: keyFlags[i],
			ClaimKey: c.ClaimKey, Question: c.Question, Answer: c.Answer, Status: c.Status})
	}
	return r, u, nil
}

// ── rederive ────────────────────────────────────────────────────────────

type rederiveAnswer struct {
	QuestionIndex int    `json:"question_index"`
	Text          string `json:"text"`
	Answer        string `json:"answer"`
	SourceURL     string `json:"source_url"`
	Passage       string `json:"passage"`
	Context       string `json:"context"`
	PublishedAt   string `json:"published_at"`
	EffectiveAt   string `json:"effective_at"`
	Jurisdiction  string `json:"jurisdiction"`
	Population    string `json:"population"`
	Conditions    string `json:"conditions"`
	Status        string `json:"status"`
}

var (
	numberRE  = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)
	nonWordRE = regexp.MustCompile(`[^a-z0-9]+`)
)

func numberSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, m := range numberRE.FindAllString(s, -1) {
		if f, err := strconv.ParseFloat(strings.ReplaceAll(m, ",", ""), 64); err == nil {
			out[strconv.FormatFloat(f, 'f', -1, 64)] = true
		}
	}
	return out
}

// AnswersMatch compares a primary and an independently re-derived answer.
// When either carries numbers, the number SETS must be equal (thousands
// separators and trailing zeros normalized); otherwise the normalized text
// must be equal. Deterministic and deliberately strict — a false mismatch
// costs a human look, a false match ships a wrong number.
func AnswersMatch(a, b string) bool {
	na, nb := numberSet(a), numberSet(b)
	if len(na) > 0 || len(nb) > 0 {
		if len(na) != len(nb) {
			return false
		}
		for k := range na {
			if !nb[k] {
				return false
			}
		}
		return true
	}
	norm := func(s string) string { return strings.Trim(nonWordRE.ReplaceAllString(strings.ToLower(s), " "), " ") }
	return norm(a) != "" && norm(a) == norm(b)
}

// CompareRederivation pairs key claims with the independent answers.
func CompareRederivation(keys []researchRef, answers []rederiveAnswer) []Comparison {
	byIdx := map[int]rederiveAnswer{}
	for _, a := range answers {
		if _, dup := byIdx[a.QuestionIndex]; !dup {
			byIdx[a.QuestionIndex] = a
		}
	}
	out := make([]Comparison, 0, len(keys))
	for i, k := range keys {
		c := Comparison{ClaimID: k.ClaimID, PrimaryVersion: k.Version}
		a, ok := byIdx[i]
		switch {
		case !ok:
			c.Outcome, c.Detail = "unconfirmed", "no independent answer returned"
		case a.Status != ClaimSupported:
			c.Outcome, c.Detail = "unconfirmed", "independent pass: "+a.Status
		case AnswersMatch(k.Answer, a.Answer):
			c.Outcome = "match"
		default:
			c.Outcome = "mismatch"
			c.Detail = fmt.Sprintf("primary %q vs independent %q (%s)", k.Answer, a.Answer, a.SourceURL)
		}
		out = append(out, c)
	}
	return out
}

func (p *Pipeline) rederive(ctx context.Context, in PipelineInput, keys []researchRef) (any, Usage, error) {
	res := rederiveResult{Comparisons: []Comparison{}, Conflicting: []string{}}
	if len(keys) == 0 {
		return res, Usage{}, nil
	}
	gen, err := p.LLM.Generate(ctx, GenerateRequest{OrgID: in.OrgID, Tier: TierJudge, System: rederiveSystem,
		Prompt: rederivePrompt(in, keys), Schema: rederiveSchema(), ToolName: "submit_rederivation",
		WebCategory: in.Brief.Category, MaxTokens: 16000})
	u := resultUsage(gen)
	if err != nil {
		return nil, u, err
	}
	var out struct {
		Answers []rederiveAnswer `json:"answers"`
	}
	if err := json.Unmarshal(gen.JSON, &out); err != nil {
		return nil, u, fmt.Errorf("%w: %v", ErrNoStructuredOutput, err)
	}
	allow := CategoryAllowlist[in.Brief.Category]
	var rclaims []Claim
	var idxOf []int
	for _, a := range out.Answers {
		if a.QuestionIndex < 0 || a.QuestionIndex >= len(keys) {
			continue
		}
		c, ok := NormalizeClaim(modelClaim{Text: a.Text, Type: ClaimSourcedFact, SourceURL: a.SourceURL, Passage: a.Passage,
			Context: a.Context, PublishedAt: a.PublishedAt, EffectiveAt: a.EffectiveAt, Jurisdiction: a.Jurisdiction,
			Population: a.Population, Conditions: a.Conditions, Status: a.Status, ClaimKey: keys[a.QuestionIndex].ClaimKey,
			Question: keys[a.QuestionIndex].Question, Answer: a.Answer}, allow, DerivationRederive)
		if !ok {
			continue
		}
		rclaims = append(rclaims, c)
		idxOf = append(idxOf, a.QuestionIndex)
	}
	// The comparison uses the NORMALIZED status (an off-allow-list "supported"
	// answer is unconfirmed, not a match).
	normalized := make([]rederiveAnswer, len(rclaims))
	for i, c := range rclaims {
		normalized[i] = rederiveAnswer{QuestionIndex: idxOf[i], Answer: c.Answer, Status: c.Status, SourceURL: c.SourceURL}
	}
	saved := []Claim{}
	if len(rclaims) > 0 {
		if saved, err = p.Store.InsertClaims(ctx, in.OrgID, in.Article.ID, rclaims); err != nil {
			return nil, u, err
		}
	}
	res.Comparisons = CompareRederivation(keys, normalized)
	rid := map[int]string{}
	for i, c := range saved {
		rid[idxOf[i]] = c.ClaimID
	}
	var mismatchIDs []string
	for i := range res.Comparisons {
		res.Comparisons[i].RederiveClaimID = rid[i]
		if res.Comparisons[i].Outcome == "mismatch" {
			mismatchIDs = append(mismatchIDs, res.Comparisons[i].ClaimID)
		}
	}
	if len(mismatchIDs) == 0 {
		return res, u, nil
	}
	existing, err := p.Store.ClaimsByIDs(ctx, in.OrgID, mismatchIDs)
	if err != nil {
		return nil, u, err
	}
	_, _, latestClaim := IndexClaims(existing)
	for _, cmp := range res.Comparisons {
		if cmp.Outcome != "mismatch" {
			continue
		}
		c, ok := latestClaim[cmp.ClaimID]
		if !ok || c.Status == ClaimConflicting {
			continue
		}
		c.Status = ClaimConflicting
		c.Derivation = DerivationPrimary
		c.Context = strings.TrimSpace(c.Context + " [rederive mismatch: " + cmp.Detail + "]")
		if _, _, err := p.Store.NewClaimVersion(ctx, in.OrgID, c.ClaimID, c); err != nil {
			return nil, u, err
		}
		res.Conflicting = append(res.Conflicting, c.ClaimID)
	}
	return res, u, nil
}

// ── draft / package ─────────────────────────────────────────────────────

// draftMaxTokens: headroom for the article JSON (~3-4k tokens for a
// 1,400-word draft). The 2026-09-11 truncations were NOT length: Sonnet 5
// thinks by default and a 68k-char thinking block consumed the whole budget
// (repro, 54-claim fixture). The draft only transcribes verified claims into
// blocks, so it runs with thinking disabled (NoThinking below).
const draftMaxTokens = 24000

func (p *Pipeline) draft(ctx context.Context, in PipelineInput, claims []Claim) (any, Usage, error) {
	gen, err := p.LLM.Generate(ctx, GenerateRequest{OrgID: in.OrgID, Tier: TierWrite, System: draftSystem,
		Prompt: draftPrompt(in, claims), Schema: draftSchema(), MaxTokens: draftMaxTokens, NoThinking: true})
	u := resultUsage(gen)
	if err != nil {
		return nil, u, err
	}
	var d draftResult
	if err := json.Unmarshal(gen.JSON, &d); err != nil {
		return nil, u, fmt.Errorf("%w: %v", ErrNoStructuredOutput, err)
	}
	d.ClaimRefs = extractDraftRefs(d.Blocks, claims)
	return d, u, nil
}

// packageStage packages the draft; with findings (a revise round) it also gets
// the previous package and fixes what was flagged on it.
func (p *Pipeline) packageStage(ctx context.Context, in PipelineInput, d draftResult, claims []Claim, prev Package, fix []reviseFinding) (any, Usage, error) {
	prompt := packagePrompt(in, d, claims)
	if len(fix) > 0 {
		prompt = packageRevisePrompt(in, d, claims, prev, fix)
	}
	gen, err := p.LLM.Generate(ctx, GenerateRequest{OrgID: in.OrgID, Tier: TierLight, System: packageSystem,
		Prompt: prompt, Schema: packageSchema(), MaxTokens: 4000})
	u := resultUsage(gen)
	if err != nil {
		return nil, u, err
	}
	var pk packageResult
	if err := json.Unmarshal(gen.JSON, &pk); err != nil {
		return nil, u, fmt.Errorf("%w: %v", ErrNoStructuredOutput, err)
	}
	pk.ClaimRefs = extractPackageRefs(&pk, claims)
	enforcePackageLimits(&pk)
	return pk, u, nil
}

// ── judgment ────────────────────────────────────────────────────────────

var judgeFlagKinds = map[string]bool{
	"headline_overpromise": true, "omitted_exception": true, "low_usefulness": true, "unreferenced_claim": true,
}

// verdictRank orders claim verdicts from lenient to strict.
var verdictRank = map[string]int{"supported": 0, "overstated": 1, "unsupported": 2}

// ParseJudgment validates the judge's output against the refs it was shown
// (in SortClaimRefs order). Unknown verdicts, out-of-range ref_index values
// and unknown flag kinds are errors; a ref_index returned twice keeps its
// stricter verdict. A ref the judge did not
// return a verdict for is recorded as unsupported (fail closed) — silence is
// never approval.
func ParseJudgment(raw json.RawMessage, refs []ClaimRef) ([]JudgmentItem, error) {
	var out struct {
		Items []struct {
			RefIndex      int    `json:"ref_index"`
			Verdict       string `json:"verdict"`
			LostQualifier string `json:"lost_qualifier"`
			Note          string `json:"note"`
		} `json:"items"`
		Flags []struct {
			Kind        string `json:"kind"`
			BlockID     string `json:"block_id"`
			SentenceIdx int    `json:"sentence_idx"`
			Note        string `json:"note"`
		} `json:"flags"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: judgment: %v", ErrNoStructuredOutput, err)
	}
	byRef := map[int]JudgmentItem{}
	for _, it := range out.Items {
		if it.RefIndex < 0 || it.RefIndex >= len(refs) {
			return nil, fmt.Errorf("%w: judgment ref_index %d out of range (%d refs)", ErrNoStructuredOutput, it.RefIndex, len(refs))
		}
		switch it.Verdict {
		case "supported", "overstated", "unsupported":
		default:
			return nil, fmt.Errorf("%w: judgment verdict %q", ErrNoStructuredOutput, it.Verdict)
		}
		// A reference returned twice keeps its stricter verdict (live
		// 2026-09-11: "ref_index 0 returned twice" failed whole judgments).
		if prev, dup := byRef[it.RefIndex]; dup && verdictRank[prev.Verdict] >= verdictRank[it.Verdict] {
			continue
		}
		r := refs[it.RefIndex]
		byRef[it.RefIndex] = JudgmentItem{Kind: "claim", BlockID: r.BlockID, SentenceIdx: r.SentenceIdx, ClaimID: r.ClaimID,
			Version: r.Version, Verdict: it.Verdict, LostQualifier: it.LostQualifier, Note: it.Note}
	}
	items := make([]JudgmentItem, 0, len(refs)+len(out.Flags))
	for i, r := range refs {
		it, ok := byRef[i]
		if !ok {
			it = JudgmentItem{Kind: "claim", BlockID: r.BlockID, SentenceIdx: r.SentenceIdx, ClaimID: r.ClaimID,
				Version: r.Version, Verdict: "unsupported", Note: failClosedNote}
		}
		items = append(items, it)
	}
	for _, f := range out.Flags {
		if !judgeFlagKinds[f.Kind] {
			return nil, fmt.Errorf("%w: judgment flag kind %q", ErrNoStructuredOutput, f.Kind)
		}
		items = append(items, JudgmentItem{Kind: f.Kind, BlockID: f.BlockID, SentenceIdx: f.SentenceIdx, Verdict: "flag", Note: f.Note})
	}
	for i := range items {
		items[i].ID = "j" + strconv.Itoa(i+1)
	}
	return items, nil
}

func (p *Pipeline) judge(ctx context.Context, in PipelineInput, pkg Package, refs []ClaimRef, claims map[string]Claim) (any, Usage, error) {
	items, u, err := p.judgeCall(ctx, in, judgeSystem, pkg, refs, claims, nil)
	if err != nil {
		return nil, u, err
	}
	return items, u, nil
}

const (
	// judgePromptVersion is part of the judgment stage's input hash, so a
	// changed judge prompt never replays a judgment cached under an old one.
	judgePromptVersion = "2026-09-11.coverage2"
	// judgeMinCoverage is the share of references the judge must return a
	// verdict for; below it the call is re-run. Live 2026-09-11: 3 of ~40
	// judgments returned verdicts for about 1 reference plus flags, and fail
	// closed turned 33–38 sentences into "unsupported" (discountblog's round
	// 3 went from 4 hard blockers to 40).
	judgeMinCoverage = 0.9
	judgeAttempts    = 3
	failClosedNote   = "judge returned no verdict for this sentence — fail closed"
)

// judgeCall runs one judge system over the package and re-runs a call that
// returns verdicts for too few references. After judgeAttempts it fails the
// stage rather than record fabricated "unsupported" verdicts; the article
// stays out of review and retries next tick.
// referenced lists sentences that already carry a judged reference, so the
// judge does not flag them as unreferenced (incremental judgment).
func (p *Pipeline) judgeCall(ctx context.Context, in PipelineInput, system string, pkg Package, refs []ClaimRef, claims map[string]Claim, referenced []ClaimRef) ([]JudgmentItem, Usage, error) {
	var total Usage
	for attempt := 1; ; attempt++ {
		gen, err := p.LLM.Generate(ctx, GenerateRequest{OrgID: in.OrgID, Tier: TierJudge, System: system,
			Prompt: judgePrompt(pkg, refs, claims, referenced), Schema: judgmentSchema(), MaxTokens: 16000})
		total.Add(resultUsage(gen))
		if err != nil {
			return nil, total, err
		}
		items, err := ParseJudgment(gen.JSON, refs)
		if err != nil {
			if attempt >= judgeAttempts {
				return nil, total, err
			}
			log.Printf("[ContentDesk] judge article=%s attempt=%d: %v — re-running", in.Article.ID, attempt, err)
			continue
		}
		got := judgedRefs(items)
		if float64(got) >= judgeMinCoverage*float64(len(refs)) {
			return items, total, nil
		}
		log.Printf("[ContentDesk] judge article=%s attempt=%d: verdicts for %d of %d references — re-running", in.Article.ID, attempt, got, len(refs))
		if attempt >= judgeAttempts {
			return nil, total, fmt.Errorf("%w: judge returned verdicts for %d of %d references after %d attempts", ErrNoStructuredOutput, got, len(refs), attempt)
		}
	}
}

// judgedRefs counts the references the judge actually returned a verdict for.
func judgedRefs(items []JudgmentItem) int {
	n := 0
	for _, j := range items {
		if j.Kind == "claim" && j.Note != failClosedNote {
			n++
		}
	}
	return n
}

func resultUsage(r *GenerateResult) Usage {
	if r == nil {
		return Usage{}
	}
	return r.Usage
}

// Slugify makes a lowercase-hyphenated slug ≤ MaxSlugLen, cut at a hyphen.
func Slugify(s string) string {
	out := strings.Trim(nonWordRE.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(out) > MaxSlugLen {
		out = out[:MaxSlugLen]
		if i := strings.LastIndex(out, "-"); i > 20 {
			out = out[:i]
		}
		out = strings.Trim(out, "-")
	}
	return out
}
