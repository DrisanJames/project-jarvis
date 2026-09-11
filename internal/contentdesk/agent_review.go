package contentdesk

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
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
	defaultReviseRounds = 3
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
		if !c.Passed {
			out = append(out, reviseFinding{SentenceIdx: -1, Problem: "failed code check " + c.Name + " (" + c.Severity + ")"})
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

You are now revising your own draft after the standards editor's review. You get the current blocks, with their inline claim markers, and a list of findings.
- Fix every finding at its cause: restore an overstated sentence's lost qualifier or narrow it to exactly what the passage says; rewrite or remove an unsupported sentence; give an unreferenced factual sentence a marker or cut it; fix a failed code check (length limits, links) where it arises.
- Change nothing that has no finding. Keep block ids stable, and keep every existing marker on sentences you do not change.
- Return EVERY block of the article, unchanged ones included, with all their markers — the complete article, never a fragment or a diff.
- A marker's claim_id must be one of the listed claim ids with status=supported. If a sentence has no supported claim, cut it.`

const secondReviewSystem = judgeSystem + `

You are the SECOND standards editor. A first review already passed this package. Assume it missed something: re-derive every verdict from the passage yourself, read each sentence with its paragraph, and flag anything a careful reader could be misled by. Supported means the passage alone justifies the sentence as written.`

func revisePrompt(in PipelineInput, a assessment, claims []Claim) string {
	blockIDs := map[string]bool{}
	for _, b := range a.pkg.Blocks {
		blockIDs[b.ID] = true
	}
	var draftRefs []ClaimRef // package-level refs (title, excerpt…) are rebuilt by the package stage
	for _, r := range a.refs {
		if blockIDs[r.BlockID] {
			draftRefs = append(draftRefs, r)
		}
	}
	var supported []Claim
	for _, c := range claims {
		if c.Status == ClaimSupported {
			supported = append(supported, c)
		}
	}
	cur, _ := json.Marshal(map[string]any{"blocks": markedBlocks(a.pkg.Blocks, draftRefs)})
	fs, _ := json.Marshal(a.findings())
	return fmt.Sprintf("%s\n%s\n\nCurrent draft (blocks + claim_refs):\n%s\n\nFindings to fix:\n%s\n\nClaims (reference only status=supported):\n%s",
		briefBlock(in), sentenceRules, cur, fs, claimLines(supported))
}

// revise rewrites the flagged sentences. Like the draft it transcribes
// verified claims, so it runs without thinking.
func (p *Pipeline) revise(ctx context.Context, in PipelineInput, a assessment, claims []Claim) (any, Usage, error) {
	gen, err := p.LLM.Generate(ctx, GenerateRequest{OrgID: in.OrgID, Tier: TierWrite, System: reviseSystem,
		Prompt: revisePrompt(in, a, claims), Schema: draftSchema(), MaxTokens: draftMaxTokens, NoThinking: true})
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

// revisePromptVersion is part of the revise stage's input hash, so changing
// the revise prompt never replays outputs cached under an older prompt.
const revisePromptVersion = "2026-09-11.3-" + citationContractVersion

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
// the previous revision did not have AND strictly reduces the blockers. A
// broken calculation or dangling reference is never traded for fewer flags
// (2026-09-11: aadwd's accepted rewrite cut ~26 blockers to 12 but newly failed
// calc_recompute and claim_refs_resolve, both S1).
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
	return len(cand.blockers()) < len(prev.blockers())
}

// secondPassClean: every item is a supported claim and there is at least one.
// Nothing to verify is not a pass.
func secondPassClean(items []JudgmentItem) bool {
	if len(items) == 0 {
		return false
	}
	for _, j := range items {
		if j.Kind != "claim" || j.Verdict != "supported" {
			return false
		}
	}
	return true
}

// agentReview approves a clean revision. Anything it cannot approve stays in
// review for a person, with the reason logged.
func (p *Pipeline) agentReview(ctx context.Context, in PipelineInput, org, articleID string, a assessment, claims map[string]Claim) error {
	if !AgentReviewEnabled() {
		return nil
	}
	if b := a.blockers(); len(b) > 0 {
		log.Printf("[ContentDesk] agent-review article=%s: held for a person — %d blocker(s) left after revise: %s",
			articleID, len(b), clip(strings.Join(b, "; "), 300))
		return nil
	}
	_, out, err := p.Store.SubmitReview(ctx, org, articleID, ReviewInput{RevisionHash: a.revHash, Reviewer: AgentReviewer, Role: "primary", Decision: "approve"})
	if err != nil {
		return fmt.Errorf("agent review (primary): %w", err)
	}
	if out.Status == StatusApproved {
		log.Printf("[ContentDesk] agent-review article=%s: approved by %s", articleID, AgentReviewer)
		return nil
	}
	// Consequential: the adversarial second pass must also find nothing.
	gen, err := p.LLM.Generate(ctx, GenerateRequest{OrgID: in.OrgID, Tier: TierJudge, System: secondReviewSystem,
		Prompt: judgePrompt(a.pkg, a.refs, claims), Schema: judgmentSchema(), MaxTokens: 16000})
	if err != nil {
		log.Printf("[ContentDesk] agent-review article=%s: second pass failed (%v) — awaiting a second reviewer", articleID, err)
		return nil
	}
	items, err := ParseJudgment(gen.JSON, a.refs)
	if err != nil || !secondPassClean(items) {
		log.Printf("[ContentDesk] agent-review article=%s: second pass not clean — awaiting a second reviewer", articleID)
		return nil
	}
	if _, _, err := p.Store.SubmitReview(ctx, org, articleID, ReviewInput{RevisionHash: a.revHash, Reviewer: AgentSecondReviewer, Role: "second", Decision: "approve"}); err != nil {
		return fmt.Errorf("agent review (second): %w", err)
	}
	log.Printf("[ContentDesk] agent-review article=%s: approved (primary + adversarial second pass)", articleID)
	return nil
}
