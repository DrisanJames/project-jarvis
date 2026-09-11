package contentdesk

import (
	"fmt"
	"regexp"
	"strings"
)

// ReviewInput is the body of POST articles/{id}/reviews.
type ReviewInput struct {
	RevisionHash string    `json:"revision_hash"`
	Reviewer     string    `json:"reviewer"`
	Role         string    `json:"role"`
	Decision     string    `json:"decision"`
	Findings     []Finding `json:"findings"`
	Minutes      float64   `json:"minutes"`
	// AcceptedIDs are the judgment item ids ("j3") and S2 code checks
	// ("code:near_duplicate") the reviewer explicitly accepts. S1 code checks
	// cannot be accepted — they need a new revision.
	AcceptedIDs []string `json:"accepted_ids"`
}

var hexHashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate checks enums and required fields.
func (in ReviewInput) Validate() error {
	if !hexHashRE.MatchString(in.RevisionHash) {
		return fmt.Errorf("%w: revision_hash must be 64 lowercase hex", ErrInvalid)
	}
	if strings.TrimSpace(in.Reviewer) == "" {
		return fmt.Errorf("%w: reviewer is required", ErrInvalid)
	}
	if in.Role != "primary" && in.Role != "second" {
		return fmt.Errorf("%w: role must be primary|second", ErrInvalid)
	}
	switch in.Decision {
	case "approve", "reject", "changes":
	default:
		return fmt.Errorf("%w: decision must be approve|reject|changes", ErrInvalid)
	}
	for i, f := range in.Findings {
		if f.Severity != "S1" && f.Severity != "S2" && f.Severity != "S3" {
			return fmt.Errorf("%w: findings[%d].severity must be S1|S2|S3", ErrInvalid, i)
		}
		if f.CaughtBy != "code" && f.CaughtBy != "judgment" && f.CaughtBy != "human" {
			return fmt.Errorf("%w: findings[%d].caught_by must be code|judgment|human", ErrInvalid, i)
		}
	}
	if in.Minutes < 0 {
		return fmt.Errorf("%w: minutes must be ≥ 0", ErrInvalid)
	}
	return nil
}

// PriorReview is an earlier review on the SAME revision hash.
type PriorReview struct {
	Reviewer string
	Role     string
	Decision string
	Findings []Finding
}

// ReviewOutcome is the article's next status plus why.
type ReviewOutcome struct {
	Status   string   `json:"status"`
	Blockers []string `json:"blockers,omitempty"`
	Awaiting string   `json:"awaiting,omitempty"`
}

// ApprovalBlockers lists what stops an approve on this revision.
func ApprovalBlockers(checks Checks, prior []PriorReview, in ReviewInput) []string {
	accepted := map[string]bool{}
	for _, id := range in.AcceptedIDs {
		accepted[id] = true
	}
	var b []string
	for _, c := range checks.Code {
		if c.Passed {
			continue
		}
		if c.Severity == "S1" {
			b = append(b, fmt.Sprintf("code check %s failed (S1 — needs a new revision)", c.Name))
		} else if !accepted["code:"+c.Name] {
			b = append(b, fmt.Sprintf("code check %s failed and is not accepted (code:%s)", c.Name, c.Name))
		}
	}
	for _, j := range checks.Judgment {
		if j.Kind == "claim" && j.Verdict == "supported" {
			continue
		}
		if !accepted[j.ID] {
			b = append(b, fmt.Sprintf("judgment %s (%s %s at %s#%d) is unresolved", j.ID, j.Kind, j.Verdict, j.BlockID, j.SentenceIdx))
		}
	}
	for _, p := range prior {
		for _, f := range p.Findings {
			if f.Severity == "S1" {
				b = append(b, fmt.Sprintf("open S1 finding by %s: %s", p.Reviewer, clip(f.Text, 80)))
			}
		}
	}
	for _, f := range in.Findings {
		if f.Severity == "S1" {
			b = append(b, "this review carries an S1 finding: "+clip(f.Text, 80))
		}
	}
	return b
}

// DecideReview computes the next article status. Pure; the store applies it.
// A consequential article needs a primary approve AND a second approve by a
// different reviewer on the same hash — there is no path that skips it.
func DecideReview(checks Checks, consequential bool, prior []PriorReview, in ReviewInput) ReviewOutcome {
	switch in.Decision {
	case "changes":
		return ReviewOutcome{Status: StatusChangesRequested}
	case "reject":
		return ReviewOutcome{Status: StatusRejected}
	}
	if blockers := ApprovalBlockers(checks, prior, in); len(blockers) > 0 {
		return ReviewOutcome{Status: StatusInReview, Blockers: blockers}
	}
	primaryBy := map[string]bool{}
	secondBy := map[string]bool{}
	for _, p := range append(prior, PriorReview{Reviewer: in.Reviewer, Role: in.Role, Decision: in.Decision}) {
		if p.Decision != "approve" {
			continue
		}
		if p.Role == "primary" {
			primaryBy[p.Reviewer] = true
		} else {
			secondBy[p.Reviewer] = true
		}
	}
	if len(primaryBy) == 0 {
		return ReviewOutcome{Status: StatusInReview, Awaiting: "primary"}
	}
	if !consequential {
		return ReviewOutcome{Status: StatusApproved}
	}
	for s := range secondBy {
		for p := range primaryBy {
			if s != p {
				return ReviewOutcome{Status: StatusApproved}
			}
		}
	}
	return ReviewOutcome{Status: StatusInReview, Awaiting: "second (consequential: a different reviewer must also approve)"}
}

// ManifestDropViolations returns every article that was in the last
// live_confirmed manifest, is missing from the next one, and was not
// withdrawn. A non-empty result refuses the release.
func ManifestDropViolations(prevLive, next []ManifestEntry, withdrawn map[string]bool) []string {
	inNext := map[string]bool{}
	for _, e := range next {
		inNext[e.ArticleID] = true
	}
	var v []string
	for _, e := range prevLive {
		if !inNext[e.ArticleID] && !withdrawn[e.ArticleID] {
			v = append(v, fmt.Sprintf("article %s (%s) is live_confirmed but absent from the manifest and not withdrawn", e.ArticleID, e.Slug))
		}
	}
	return v
}

// ValidReleaseTransition is the release state machine.
func ValidReleaseTransition(from, to string) bool {
	switch to {
	case ReleaseFailed:
		return from == ReleasePending || from == ReleaseBuilding || from == ReleaseDeployed
	case ReleaseBuilding:
		return from == ReleasePending
	case ReleaseDeployed:
		return from == ReleaseBuilding
	case ReleaseLiveConfirmed:
		return from == ReleaseDeployed
	}
	return false
}

// SentencePair is one sentence↔passage row for the review UI.
type SentencePair struct {
	BlockID       string        `json:"block_id"`
	SentenceIdx   int           `json:"sentence_idx"`
	Sentence      string        `json:"sentence"`
	Paragraph     string        `json:"paragraph"`
	ClaimID       string        `json:"claim_id"`
	Version       int           `json:"version"`
	LatestVersion int           `json:"latest_version"`
	Stale         bool          `json:"stale"`
	Claim         *Claim        `json:"claim"`
	Judgment      *JudgmentItem `json:"judgment,omitempty"`
}

// BuildPairs joins every claim ref to its sentence, paragraph, claim version
// (with passage and scope) and judge verdict.
func BuildPairs(pkg Package, refs []ClaimRef, checks Checks, claims map[string]Claim, latest map[string]int) []SentencePair {
	units := Units(pkg)
	judged := map[string]*JudgmentItem{}
	for i := range checks.Judgment {
		j := &checks.Judgment[i]
		if j.Kind == "claim" {
			judged[fmt.Sprintf("%s#%d@%s@%d", j.BlockID, j.SentenceIdx, j.ClaimID, j.Version)] = j
		}
	}
	out := make([]SentencePair, 0, len(refs))
	for _, r := range refs {
		p := SentencePair{BlockID: r.BlockID, SentenceIdx: r.SentenceIdx, ClaimID: r.ClaimID, Version: r.Version,
			Paragraph: UnitParagraph(pkg, r.BlockID), LatestVersion: latest[r.ClaimID]}
		if s := units[r.BlockID]; r.SentenceIdx >= 0 && r.SentenceIdx < len(s) {
			p.Sentence = s[r.SentenceIdx]
		}
		if c, ok := claims[ClaimVersionKey(r.ClaimID, r.Version)]; ok {
			cc := c
			p.Claim = &cc
		}
		p.Stale = p.LatestVersion > r.Version
		p.Judgment = judged[fmt.Sprintf("%s#%d@%s@%d", r.BlockID, r.SentenceIdx, r.ClaimID, r.Version)]
		out = append(out, p)
	}
	return out
}
