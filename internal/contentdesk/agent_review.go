package contentdesk

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Agent review (operator 2026-09-11: "Why can't an agent review"). After
// judgment, a revise loop sends the writer every failed code check and every
// unresolved judgment; only the flagged sentences change, and the revision is
// re-assessed. A revision with no remaining ApprovalBlockers is approved by
// the agent reviewer. A consequential article also needs an adversarial
// second pass: Opus with a fresh context and its own prompt, re-deriving every
// verdict. That is not independent the way a second person is — the writer is
// Sonnet 5 and both passes are Opus 5 — but it must find nothing.
// Approval never publishes: releases are a separate step.

const (
	EnvAgentReview      = "CONTENT_DESK_AGENT_REVIEW"
	AgentReviewer       = "content-desk-agent"
	AgentSecondReviewer = "content-desk-agent-second-pass"
	// 5, not 3: on :1129 discountblog and financialcalculate accepted all 3
	// rounds, each one improving, and ran out of rounds at 3 hard blockers.
	defaultReviseRounds = 5
)

// AgentReviewEnabled reports CONTENT_DESK_AGENT_REVIEW=1. Unset = humans review.
func AgentReviewEnabled() bool {
	return strings.TrimSpace(os.Getenv(EnvAgentReview)) == "1"
}

func (p *Pipeline) maxReviseRounds() int {
	if p.MaxReviseRounds <= 0 {
		return defaultReviseRounds
	}
	return p.MaxReviseRounds
}

// assessment is one packaged revision with its checks.
type assessment struct {
	pkg      Package
	refs     []ClaimRef
	revHash  string
	code     []CheckResult
	judgment []JudgmentItem
}

func (a assessment) blockers() []string {
	return ApprovalBlockers(Checks{Code: a.code, Judgment: a.judgment}, nil, ReviewInput{})
}

func (a assessment) clean() bool { return len(a.blockers()) == 0 }

// adjudicableFlags are the judge's editorial flags an agent may accept with a
// written reason. Claim verdicts and unreferenced_claim (a factual sentence
// with no source) never are; neither are S1 or non-heuristic code checks.
var adjudicableFlags = map[string]bool{"headline_overpromise": true, "omitted_exception": true, "low_usefulness": true}

// adjItem is one item the managing editor may accept.
type adjItem struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Unit     string `json:"unit,omitempty"`
	Sentence string `json:"sentence,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// adjudicable lists the blockers an agent may accept: failed S2 heuristic
// code checks and editorial flags.
func (a assessment) adjudicable() []adjItem {
	units := Units(a.pkg)
	var out []adjItem
	for _, c := range a.code {
		if !c.Passed && c.Severity == "S2" && c.Heuristic {
			out = append(out, adjItem{ID: "code:" + c.Name, Kind: "code check " + c.Name, Detail: clip(strings.Join(c.Details, "; "), 800)})
		}
	}
	for _, j := range a.judgment {
		if !adjudicableFlags[j.Kind] {
			continue
		}
		it := adjItem{ID: j.ID, Kind: j.Kind, Unit: fmt.Sprintf("%s#%d", j.BlockID, j.SentenceIdx), Detail: j.Note}
		if s := units[j.BlockID]; j.SentenceIdx >= 0 && j.SentenceIdx < len(s) {
			it.Sentence = s[j.SentenceIdx]
		}
		out = append(out, it)
	}
	return out
}

// hardBlockerList is what still blocks if every adjudicable item is accepted —
// by construction, ApprovalBlockers with those ids accepted.
func (a assessment) hardBlockerList() []string {
	var ids []string
	for _, it := range a.adjudicable() {
		ids = append(ids, it.ID)
	}
	return ApprovalBlockers(Checks{Code: a.code, Judgment: a.judgment}, nil, ReviewInput{AcceptedIDs: ids})
}

func (a assessment) hardBlockers() int { return len(a.hardBlockerList()) }

// packageFindings are the findings on package units (title, excerpt, meta,
// subjects, preheaders); the package stage fixes them, not the block reviser.
func (a assessment) packageFindings() []reviseFinding {
	var out []reviseFinding
	for _, f := range a.findings() {
		if IsReservedUnitID(f.BlockID) {
			out = append(out, f)
		}
	}
	return out
}

// reviseFinding is one thing the writer must fix.
type reviseFinding struct {
	BlockID     string `json:"block_id,omitempty"`
	SentenceIdx int    `json:"sentence_idx"`
	Sentence    string `json:"sentence,omitempty"`
	Problem     string `json:"problem"`
	Detail      string `json:"detail,omitempty"`
}

// findings is exactly what ApprovalBlockers blocks on: every failed code
// check and every judgment that is not a supported claim.
func (a assessment) findings() []reviseFinding {
	units := Units(a.pkg)
	var out []reviseFinding
	for _, c := range a.code {
		if c.Passed {
			continue
		}
		// Pin a check failure to its unit when the detail names one
		// ("ref[11] key-takeaways#3 → …", "faq-1#7 has a number…"), so the
		// per-block reviser knows where to fix it.
		located := false
		for _, d := range c.Details {
			m := checkDetailUnitRE.FindStringSubmatch(d)
			if m == nil {
				continue
			}
			idx, _ := strconv.Atoi(m[2])
			f := reviseFinding{BlockID: m[1], SentenceIdx: idx, Problem: "code check " + c.Name + " (" + c.Severity + ")", Detail: clip(d, 300)}
			if s := units[m[1]]; idx >= 0 && idx < len(s) {
				f.Sentence = s[idx]
			}
			out = append(out, f)
			located = true
		}
		if !located {
			out = append(out, reviseFinding{SentenceIdx: -1, Problem: "failed code check " + c.Name + " (" + c.Severity + ")",
				Detail: clip(strings.Join(c.Details, "; "), 300)})
		}
	}
	for _, j := range a.judgment {
		if j.Kind == "claim" && j.Verdict == "supported" {
			continue
		}
		f := reviseFinding{BlockID: j.BlockID, SentenceIdx: j.SentenceIdx, Problem: j.Verdict}
		if j.Kind != "claim" {
			f.Problem = j.Kind
		}
		if s := units[j.BlockID]; j.SentenceIdx >= 0 && j.SentenceIdx < len(s) {
			f.Sentence = s[j.SentenceIdx]
		}
		var detail []string
		if j.LostQualifier != "" {
			detail = append(detail, "lost qualifier: "+j.LostQualifier)
		}
		if j.Note != "" {
			detail = append(detail, j.Note)
		}
		f.Detail = strings.Join(detail, "; ")
		out = append(out, f)
	}
	return out
}

const reviseSystem = draftSystem + `

You are now revising ONE block of your article after the standards editor's review. You get that block, with its inline claim markers, the findings on it, and the rest of the article for context only.
- Fix every finding at its cause: restore an overstated sentence's lost qualifier or narrow it to exactly what the passage says; rewrite or remove an unsupported sentence; give an unreferenced factual sentence a marker or cut it; a marker citing a claim that is not listed must be replaced by a listed claim or its sentence cut; fix length or link problems where they arise.
- Change nothing that has no finding, and keep every existing marker on sentences you do not change.
- Add no new factual sentence unless it carries a marker for a listed claim. For an unreferenced or unsupported finding, cutting the sentence is a correct fix; prefer it to inventing new wording.
- Return that one block, complete — every sentence, item and row it should have, with its markers — with the same id and type.
- A marker's claim_id must be one of the listed claims (all supported). If a sentence has no supported claim, cut it.`

const secondReviewSystem = judgeSystem + `

You are the SECOND standards editor. A first review already passed this package. Assume it missed something: re-derive every verdict from the passage yourself, read each sentence with its paragraph, and flag anything a careful reader could be misled by. Supported means the passage alone justifies the sentence as written.`

// revisePromptBlock is the prompt for rewriting one block.
func revisePromptBlock(in PipelineInput, marked Block, fs []reviseFinding, article []Block, supported []Claim) string {
	blk, _ := json.Marshal(marked)
	fj, _ := json.Marshal(fs)
	var rest strings.Builder
	for _, b := range article {
		if b.ID != marked.ID {
			fmt.Fprintf(&rest, "[%s:%s] %s\n", b.ID, b.Type, clip(BlockParagraph(b), 400))
		}
	}
	return fmt.Sprintf("%s\n%s\n\nThe block to revise, with its inline claim markers (a finding's sentence_idx counts this block's units: heading, text sentences, item sentences, one per table row, caption sentences):\n%s\n\nFindings on this block:\n%s\n\nThe rest of the article, for context only (do not return it):\n%s\nClaims (all supported — cite only these):\n%s",
		briefBlock(in), sentenceRules, blk, fj, rest.String(), claimLines(supported))
}

// unitCount is how many units a block has once its markers are stripped.
func unitCount(b Block) int {
	strip := func(s string) string { c, _ := stripMarkers(s); return c }
	n := 0
	if strings.TrimSpace(strip(b.Heading)) != "" {
		n++
	}
	n += len(SplitSentences(strip(b.Text)))
	for _, it := range b.Items {
		n += len(SplitSentences(strip(it)))
	}
	n += len(b.Rows)
	n += len(SplitSentences(strip(b.Caption)))
	return n
}

// usableRevisedBlock forces a rewrite's id and type to the original's,
// restores a heading or worked-example calc the rewrite dropped, and rejects
// a rewrite that lost all its content (the empty-object failure).
func usableRevisedBlock(orig Block, nb *Block) bool {
	nb.ID, nb.Type = orig.ID, orig.Type
	body := func(b Block) int { b.Heading = ""; return unitCount(b) }
	if body(*nb) == 0 && body(orig) > 0 {
		return false
	}
	if strings.TrimSpace(nb.Heading) == "" && strings.TrimSpace(orig.Heading) != "" {
		nb.Heading = orig.Heading
	}
	if nb.Calc == nil && orig.Calc != nil {
		nb.Calc = orig.Calc
	}
	return unitCount(*nb) > 0 || unitCount(orig) == 0
}

// revise rewrites only the blocks that carry findings — one small call per
// block — and merges them into the current draft. Whole-article rewrites came
// back as a single EMPTY block in all 3 rounds on discountblog (2026-09-11:
// the model emitting a minimal valid object); a one-block call can only lose
// that block, and an unusable rewrite simply keeps the block as it was.
// Package-level findings (title, excerpt, subjects…) are left to the package
// stage. Runs without thinking, like the draft.
func (p *Pipeline) revise(ctx context.Context, in PipelineInput, a assessment, claims []Claim) (any, Usage, error) {
	var supported []Claim
	for _, c := range claims {
		if c.Status == ClaimSupported {
			supported = append(supported, c)
		}
	}
	vs := claimVersions(claims)
	byBlock := map[string][]reviseFinding{}
	for _, f := range a.findings() {
		if f.BlockID != "" && !IsReservedUnitID(f.BlockID) {
			byBlock[f.BlockID] = append(byBlock[f.BlockID], f)
		}
	}
	inBody := map[string]bool{}
	for _, b := range a.pkg.Blocks {
		inBody[b.ID] = true
	}
	var bodyRefs []ClaimRef // package-level refs are rebuilt by the package stage
	for _, r := range a.refs {
		if inBody[r.BlockID] {
			bodyRefs = append(bodyRefs, r)
		}
	}
	keep := func(out *draftResult, b Block) {
		out.Blocks = append(out.Blocks, b)
		for _, r := range bodyRefs {
			if r.BlockID == b.ID {
				out.ClaimRefs = append(out.ClaimRefs, r)
			}
		}
	}
	marked := markedBlocks(a.pkg.Blocks, bodyRefs)
	var total Usage
	var out draftResult
	for i, b := range a.pkg.Blocks {
		fs := byBlock[b.ID]
		if len(fs) == 0 {
			keep(&out, b)
			continue
		}
		gen, err := p.LLM.Generate(ctx, GenerateRequest{OrgID: in.OrgID, Tier: TierWrite, System: reviseSystem,
			Prompt: revisePromptBlock(in, marked[i], fs, a.pkg.Blocks, supported), Schema: reviseBlockSchema(),
			MaxTokens: draftMaxTokens, NoThinking: true})
		total.Add(resultUsage(gen))
		if err != nil {
			return nil, total, err
		}
		var res struct {
			Block Block `json:"block"`
		}
		if err := json.Unmarshal(gen.JSON, &res); err != nil || !usableRevisedBlock(b, &res.Block) {
			log.Printf("[ContentDesk] revise article=%s block=%s: unusable rewrite — keeping the block as it was", in.Article.ID, b.ID)
			keep(&out, b)
			continue
		}
		nb := res.Block
		out.ClaimRefs = append(out.ClaimRefs, extractBlockRefs(&nb, vs)...)
		out.Blocks = append(out.Blocks, nb)
	}
	return out, total, nil
}

// revisePromptVersion is part of the revise stage's input hash, so changing
// the revise prompt never replays outputs cached under an older prompt.
// .5: no new uncited facts (live :1131: a round-1 rewrite of myownhealth's
// section-2 added "check the serving size at the top of the label" with no
// marker, which became its last hard blocker).
const revisePromptVersion = "2026-09-11.5-perblock-nonew-" + citationContractVersion

// checkDetailUnitRE finds the unit a code-check detail names: an optional
// "ref[i] " then "<unit>#<sentence_idx>".
var checkDetailUnitRE = regexp.MustCompile(`^(?:ref\[\d+\] )?([^\s#]+)#(\d+)`)

// degenerateRevision names why a rewrite lost the article, or "". The live
// model sometimes returns a minimal valid object (2026-09-11: 1 block and 0
// claim_refs in two of three rounds on discountblog; a claim_id of "N/A").
func degenerateRevision(prev assessment, next draftResult) string {
	pb := len(prev.pkg.Blocks)
	if len(next.Blocks) == 0 || (pb >= 4 && len(next.Blocks)*2 < pb) {
		return fmt.Sprintf("%d blocks (was %d)", len(next.Blocks), pb)
	}
	inBody := map[string]bool{}
	for _, b := range prev.pkg.Blocks {
		inBody[b.ID] = true
	}
	prevRefs := 0
	for _, r := range prev.refs {
		if inBody[r.BlockID] {
			prevRefs++
		}
	}
	if prevRefs > 0 && len(next.ClaimRefs) == 0 {
		return fmt.Sprintf("0 claim_refs (was %d)", prevRefs)
	}
	for _, r := range next.ClaimRefs {
		if validUUID(r.ClaimID) != nil {
			return fmt.Sprintf("claim_ref with a non-UUID claim_id %q", clip(r.ClaimID, 40))
		}
	}
	return ""
}

// acceptRevision keeps a rewrite only if it introduces no code-check failure
// the previous revision did not have AND improves: fewer hard blockers (those
// no agent may accept), or as many hard and fewer in total. A broken
// calculation or dangling reference is never traded for fewer flags
// (2026-09-11: aadwd's accepted rewrite cut ~26 blockers to 12 but newly failed
// calc_recompute and claim_refs_resolve, both S1). Hard blockers rank first
// because the judge re-samples its editorial flags every round: on :1125 a
// round that fixed claims could be rejected for 3 new flags elsewhere.
func acceptRevision(prev, cand assessment) bool {
	failedBefore := map[string]bool{}
	for _, c := range prev.code {
		if !c.Passed {
			failedBefore[c.Name] = true
		}
	}
	for _, c := range cand.code {
		if !c.Passed && !failedBefore[c.Name] {
			return false
		}
	}
	if ch, ph := cand.hardBlockers(), prev.hardBlockers(); ch != ph {
		return ch < ph
	}
	return len(cand.blockers()) < len(prev.blockers())
}

// secondPassClean: at least one item (nothing to verify is not a pass), every
// claim supported, and no flag an agent may not accept. The editorial flags it
// raises go to the managing editor like the first pass's.
func secondPassClean(items []JudgmentItem) bool {
	if len(items) == 0 {
		return false
	}
	for _, j := range items {
		if j.Kind == "claim" {
			if j.Verdict != "supported" {
				return false
			}
			continue
		}
		if !adjudicableFlags[j.Kind] {
			return false
		}
	}
	return true
}

// minAdjudicationReason is the shortest reason that counts as written.
const minAdjudicationReason = 20

// packageText is the article as the managing editor reads it.
func packageText(pkg Package) string {
	var b strings.Builder
	fmt.Fprintf(&b, "title: %s\nexcerpt: %s\nmeta_title: %s\nmeta_description: %s\n", pkg.Title, pkg.Excerpt, pkg.MetaTitle, pkg.MetaDescription)
	for i, s := range pkg.Subjects {
		fmt.Fprintf(&b, "subject:%d: %s\n", i, s)
	}
	for i, s := range pkg.Preheaders {
		fmt.Fprintf(&b, "preheader:%d: %s\n", i, s)
	}
	for _, bl := range pkg.Blocks {
		fmt.Fprintf(&b, "[%s:%s] %s\n", bl.ID, bl.Type, BlockParagraph(bl))
	}
	return b.String()
}

// adjudicate asks the managing editor (Opus) which items may stand. An item is
// accepted only when the model accepts it with a written reason; a missing,
// refused or unreasoned decision leaves it a blocker. Each accepted item is
// recorded as an S3 finding carrying the reason.
func (p *Pipeline) adjudicate(ctx context.Context, in PipelineInput, pkg Package, items []adjItem) (accepted []string, findings []Finding, refused []string, err error) {
	ij, _ := json.Marshal(items)
	gen, err := p.LLM.Generate(ctx, GenerateRequest{OrgID: in.OrgID, Tier: TierJudge, System: adjudicateSystem,
		Prompt: fmt.Sprintf("%s\n\nThe article:\n%s\nItems to decide:\n%s", briefBlock(in), packageText(pkg), ij),
		Schema: adjudicationSchema(), MaxTokens: 8000})
	if err != nil {
		return nil, nil, nil, err
	}
	var res struct {
		Decisions []struct {
			ID     string `json:"id"`
			Accept bool   `json:"accept"`
			Reason string `json:"reason"`
		} `json:"decisions"`
	}
	if err := json.Unmarshal(gen.JSON, &res); err != nil {
		return nil, nil, nil, fmt.Errorf("%w: %v", ErrNoStructuredOutput, err)
	}
	type decision struct {
		accept bool
		reason string
	}
	byID := map[string]decision{}
	for _, d := range res.Decisions {
		byID[d.ID] = decision{d.Accept, strings.TrimSpace(d.Reason)}
	}
	for _, it := range items {
		d, ok := byID[it.ID]
		if !ok || !d.accept || len(d.reason) < minAdjudicationReason {
			refused = append(refused, it.ID)
			continue
		}
		accepted = append(accepted, it.ID)
		block, _, _ := strings.Cut(it.Unit, "#")
		findings = append(findings, Finding{Severity: "S3", CaughtBy: "judgment", BlockID: block,
			Text: clip("accepted "+it.ID+" ("+it.Kind+"): "+d.reason, 500)})
	}
	return accepted, findings, refused, nil
}

// agentReview approves a clean revision. Anything it cannot approve stays in
// review for a person, with the reason logged.
func (p *Pipeline) agentReview(ctx context.Context, in PipelineInput, org, articleID string, a assessment, claims map[string]Claim) error {
	if !AgentReviewEnabled() {
		return nil
	}
	if hard := a.hardBlockerList(); len(hard) > 0 {
		log.Printf("[ContentDesk] agent-review article=%s: held for a person — %d blocker(s) no agent may accept (of %d): %s",
			articleID, len(hard), len(a.blockers()), clip(strings.Join(hard, "; "), 300))
		return nil
	}
	// What remains is editorial flags and heuristic checks: the managing
	// editor accepts each with a written reason, or the article is held.
	var accepted []string
	var findings []Finding
	if items := a.adjudicable(); len(items) > 0 {
		acc, fs, refused, err := p.adjudicate(ctx, in, a.pkg, items)
		if err != nil {
			log.Printf("[ContentDesk] agent-review article=%s: adjudication failed (%v) — held for a person", articleID, err)
			return nil
		}
		if len(refused) > 0 {
			log.Printf("[ContentDesk] agent-review article=%s: held for a person — the managing editor did not accept %s (accepted %d of %d)",
				articleID, strings.Join(refused, ", "), len(acc), len(items))
			return nil
		}
		accepted, findings = acc, fs
		log.Printf("[ContentDesk] agent-review article=%s: managing editor accepted %d item(s) with reasons: %s", articleID, len(acc), strings.Join(acc, ", "))
	}
	_, out, err := p.Store.SubmitReview(ctx, org, articleID, ReviewInput{RevisionHash: a.revHash, Reviewer: AgentReviewer, Role: "primary", Decision: "approve",
		AcceptedIDs: accepted, Findings: findings})
	if err != nil {
		return fmt.Errorf("agent review (primary): %w", err)
	}
	if out.Status == StatusApproved {
		log.Printf("[ContentDesk] agent-review article=%s: approved by %s", articleID, AgentReviewer)
		return nil
	}
	// Consequential: the adversarial second pass must also find nothing it
	// cannot accept. Same coverage guard as the first judge.
	items, _, err := p.judgeCall(ctx, in, secondReviewSystem, a.pkg, a.refs, claims, nil)
	if err != nil {
		log.Printf("[ContentDesk] agent-review article=%s: second pass failed (%v) — awaiting a second reviewer", articleID, err)
		return nil
	}
	if !secondPassClean(items) {
		log.Printf("[ContentDesk] agent-review article=%s: second pass not clean — awaiting a second reviewer", articleID)
		return nil
	}
	secondFindings := append([]Finding(nil), findings...)
	if flags := (assessment{pkg: a.pkg, judgment: items}).adjudicable(); len(flags) > 0 {
		_, fs, refused, err := p.adjudicate(ctx, in, a.pkg, flags)
		if err != nil || len(refused) > 0 {
			log.Printf("[ContentDesk] agent-review article=%s: second pass raised %d flag(s), not all accepted (err=%v) — awaiting a second reviewer", articleID, len(flags), err)
			return nil
		}
		secondFindings = append(secondFindings, fs...)
	}
	// The second review is gated on the revision's stored checks, so it
	// carries the same accepted ids as the primary.
	if _, _, err := p.Store.SubmitReview(ctx, org, articleID, ReviewInput{RevisionHash: a.revHash, Reviewer: AgentSecondReviewer, Role: "second", Decision: "approve",
		AcceptedIDs: accepted, Findings: secondFindings}); err != nil {
		return fmt.Errorf("agent review (second): %w", err)
	}
	log.Printf("[ContentDesk] agent-review article=%s: approved (primary + adversarial second pass)", articleID)
	return nil
}
