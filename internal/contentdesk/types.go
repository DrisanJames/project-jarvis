// Package contentdesk is the editorial system that drafts articles for the
// brand sites under FULL HUMAN REVIEW. Nothing auto-publishes: the pipeline
// (research → rederive → draft → package → code_checks → judgment) produces a
// complete package, a human approves it, and the approval binds to the
// revision hash. A changed package or a bumped claim version is a different
// hash, so an approval can never silently carry over to content nobody read.
//
// Layout: types/hash/text/calc/simhash/checks are pure; llm.go is the only
// network surface (Anthropic SDK, kill switch + daily budget, fail closed);
// store.go is the only SQL; pipeline.go orchestrates stages idempotently per
// (article, stage, input_hash). The HTTP surface is the thin
// internal/api/content_desk_handlers.go and the scheduler is
// internal/worker/content_desk_worker.go.
package contentdesk

import "errors"

// Claim types, statuses and derivations — mirror the CHECK-free TEXT columns
// of content_claims; validated here, never trusted from the model.
const (
	ClaimSourcedFact    = "sourced_fact"
	ClaimCalculation    = "calculation"
	ClaimAssumption     = "assumption"
	ClaimInterpretation = "interpretation"

	ClaimSupported            = "supported"
	ClaimInsufficientEvidence = "insufficient_evidence"
	ClaimConflicting          = "conflicting"

	DerivationPrimary  = "primary"
	DerivationRederive = "rederive"
)

// Article statuses.
const (
	StatusDrafting         = "drafting"
	StatusInReview         = "in_review"
	StatusChangesRequested = "changes_requested"
	StatusApproved         = "approved"
	StatusPublishing       = "publishing"
	StatusLiveConfirmed    = "live_confirmed"
	StatusWithdrawn        = "withdrawn"
	StatusRejected         = "rejected"
)

// Release statuses.
const (
	ReleasePending       = "pending"
	ReleaseBuilding      = "building"
	ReleaseDeployed      = "deployed"
	ReleaseLiveConfirmed = "live_confirmed"
	ReleaseFailed        = "failed"
)

// Pipeline stages, in execution order.
const (
	StageResearch   = "research"
	StageRederive   = "rederive"
	StageDraft      = "draft"
	StagePackage    = "package"
	StageCodeChecks = "code_checks"
	StageJudgment   = "judgment"
	// StageRevise rewrites only the sentences judgment or code checks flagged.
	StageRevise = "revise"
)

// Stages is the canonical order.
var Stages = []string{StageResearch, StageRederive, StageDraft, StagePackage, StageCodeChecks, StageJudgment, StageRevise}

// AllowedBlockTypes is the closed set of typed blocks a draft may contain.
var AllowedBlockTypes = map[string]bool{
	"lede": true, "section": true, "key_takeaways": true, "worked_example": true,
	"document_anatomy": true, "stat": true, "comparison_table": true, "steps": true,
	"faq": true, "pull_quote": true, "callout": true,
}

// Categories. Every category has a research allow-list (llm.go); a category
// without one cannot be researched (fail closed, never open web).
const (
	CatFinance   = "finance"
	CatTax       = "tax"
	CatHealth    = "health"
	CatBenefits  = "benefits"
	CatInsurance = "insurance"
	CatHistory   = "history"
	CatDIY       = "diy"
)

// ConsequentialCategories force consequential=true on a brief. DIY is
// included wholesale because "hazardous" cannot be decided mechanically —
// over-inclusion costs a second review, under-inclusion ships an unreviewed
// safety claim.
var ConsequentialCategories = map[string]bool{
	CatHealth: true, CatBenefits: true, CatInsurance: true, CatTax: true, CatDIY: true,
	// Finance added 2026-09-11: APR / loan / refinance articles steer money
	// decisions, so they get the two-reviewer path. Applies to NEW briefs —
	// the flag is stored at brief creation.
	CatFinance: true,
}

// Block is one typed draft block. Text fields carry plain text plus the tiny
// inline-markdown subset (**bold**, *italic*, [text](https://…)); raw HTML is
// refused by code checks.
type Block struct {
	ID      string     `json:"id"`
	Type    string     `json:"type"`
	Heading string     `json:"heading,omitempty"`
	Text    string     `json:"text,omitempty"`
	Items   []string   `json:"items,omitempty"`
	Rows    [][]string `json:"rows,omitempty"`
	Calc    *Calc      `json:"calc,omitempty"`
	Caption string     `json:"caption,omitempty"`
}

// CalcInput is a named numeric input.
type CalcInput struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

// Calc is {inputs, formula, result}; code checks recompute result.
type Calc struct {
	Inputs  []CalcInput `json:"inputs"`
	Formula string      `json:"formula"`
	Result  float64     `json:"result"`
}

// HeroImage is optional; the pipeline never invents an image URL.
type HeroImage struct {
	URL    string `json:"url,omitempty"`
	Alt    string `json:"alt,omitempty"`
	Credit string `json:"credit,omitempty"`
}

// Package is the reviewable unit.
type Package struct {
	Title           string     `json:"title"`
	Excerpt         string     `json:"excerpt"`
	MetaTitle       string     `json:"meta_title"`
	MetaDescription string     `json:"meta_description"`
	Blocks          []Block    `json:"blocks"`
	HeroImage       *HeroImage `json:"hero_image"`
	Subjects        []string   `json:"subjects"`
	Preheaders      []string   `json:"preheaders"`
}

// ClaimRef binds one sentence (block_id + sentence_idx, see Units) to one
// claim version.
type ClaimRef struct {
	BlockID     string `json:"block_id"`
	SentenceIdx int    `json:"sentence_idx"`
	ClaimID     string `json:"claim_id"`
	Version     int    `json:"version"`
}

// Claim is one row of content_claims (append-only; (claim_id, version) PK).
type Claim struct {
	ClaimID      string `json:"claim_id"`
	Version      int    `json:"version"`
	Text         string `json:"text"`
	Type         string `json:"type"`
	SourceURL    string `json:"source_url"`
	Passage      string `json:"passage"`
	Context      string `json:"context"`
	PublishedAt  string `json:"published_at"`
	EffectiveAt  string `json:"effective_at"`
	RetrievedAt  string `json:"retrieved_at,omitempty"`
	Jurisdiction string `json:"jurisdiction"`
	Population   string `json:"population"`
	Conditions   string `json:"conditions"`
	Status       string `json:"status"`
	Derivation   string `json:"derivation"`
	Calc         *Calc  `json:"calc,omitempty"`
	ClaimKey     string `json:"claim_key,omitempty"`
	Question     string `json:"question,omitempty"`
	Answer       string `json:"answer,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
}

// ClaimVersionKey keys a claim version.
func ClaimVersionKey(id string, version int) string {
	return id + "@" + itoa(version)
}

// CheckResult is one deterministic code check.
type CheckResult struct {
	Name      string   `json:"name"`
	Passed    bool     `json:"passed"`
	Severity  string   `json:"severity"`
	Heuristic bool     `json:"heuristic,omitempty"`
	Details   []string `json:"details,omitempty"`
}

// JudgmentItem is one judge verdict (a claim-restating sentence) or one flag
// (headline overpromise, omitted exception, usefulness, unreferenced claim).
type JudgmentItem struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	BlockID     string `json:"block_id"`
	SentenceIdx int    `json:"sentence_idx"`
	// Sentence is the unit text at (block_id, sentence_idx), filled at read
	// time by GetArticleDetail (never trusted from the model, never hashed).
	Sentence      string `json:"sentence,omitempty"`
	ClaimID       string `json:"claim_id,omitempty"`
	Version       int    `json:"version,omitempty"`
	Verdict       string `json:"verdict"`
	LostQualifier string `json:"lost_qualifier,omitempty"`
	Note          string `json:"note,omitempty"`
}

// Checks is content_revisions.checks.
type Checks struct {
	Code     []CheckResult  `json:"code"`
	Judgment []JudgmentItem `json:"judgment"`
}

// Usage is content_revisions.usage and each stage's spend.
type Usage struct {
	Model        string  `json:"model"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	WebSearches  int64   `json:"web_searches"`
	USD          float64 `json:"usd"`
}

// Add accumulates o into u; models are joined distinct.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.WebSearches += o.WebSearches
	u.USD += o.USD
	if o.Model == "" {
		return
	}
	for _, m := range splitComma(u.Model) {
		if m == o.Model {
			return
		}
	}
	if u.Model == "" {
		u.Model = o.Model
	} else {
		u.Model += "," + o.Model
	}
}

// Finding is one human/code/judgment finding on a review.
type Finding struct {
	Severity string `json:"severity"`
	CaughtBy string `json:"caught_by"`
	BlockID  string `json:"block_id"`
	Text     string `json:"text"`
}

// ManifestEntry is one row of a release manifest.
type ManifestEntry struct {
	ArticleID    string `json:"article_id"`
	RevisionID   string `json:"revision_id"`
	RevisionHash string `json:"revision_hash"`
	Slug         string `json:"slug"`
}

// Sentinel errors mapped to HTTP statuses by the handler.
var (
	ErrNotFound        = errors.New("not found")
	ErrConflict        = errors.New("conflict")
	ErrHashMismatch    = errors.New("revision_hash does not match the article's current revision")
	ErrInvalid         = errors.New("invalid request")
	ErrInFlight        = errors.New("stage run already in flight")
	ErrRunExhausted    = errors.New("stage run exhausted its attempts")
	ErrApprovalBlocked = errors.New("approval blocked")
	ErrManifestDrop    = errors.New("manifest drops a live_confirmed article that was not withdrawn")
)
