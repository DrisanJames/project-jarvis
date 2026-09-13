// Package brain is the platform's durable operational memory (operator
// 2026-09-12: "It should live in the jarvis platform not on my desktop").
//
// It stores CLAIMS, not files: each claim has a type that decides what counts
// as proof, an authority that decides who may verify it, a validity window,
// and a status that starts at `candidate`. Evidence is a separate row that
// keeps the checker AND the result it returned, when, against which
// environment and code version — a stored query without its result is not
// evidence. Capabilities map a recognised task to the maintained tool that
// performs it (a campaign question resolves to `campaign_performance` by name,
// never by search). Evals pin known answers that a fresh session must still
// reproduce; the worker re-runs them and records every run.
//
// Every table is org-scoped like the rest of the platform.
package brain

import "time"

// Claim types and what verifies each (operator ruling 2026-09-12):
//
//	policy              operator instruction — authority: operator
//	definition          the metric contract + whether code follows it — authority: operator
//	system_fact         a timestamped observation of the live system — authority: runtime (checker + result)
//	historical_finding  evidence from the period it describes — authority: review
//	procedure           reviewed implementation + tests of its outcomes — authority: review
//	hypothesis          stays a hypothesis until outcome evidence supports it — authority: review
var ClaimTypes = []string{"policy", "definition", "system_fact", "historical_finding", "procedure", "hypothesis"}

var Authorities = []string{"operator", "contract", "runtime", "review"}

var ClaimStatuses = []string{"candidate", "active", "superseded", "retracted"}

// RequiredVerifier returns who may move a claim of this type to `active`.
// policy/definition: only the operator. Everything else: an automated checker
// or a reviewing agent, but ALWAYS with a stored result.
func RequiredVerifier(claimType string) string {
	switch claimType {
	case "policy", "definition":
		return "operator"
	case "system_fact":
		return "checker"
	default:
		return "review"
	}
}

type Claim struct {
	ID             int64      `json:"id"`
	OrgID          string     `json:"org_id"`
	ClaimType      string     `json:"claim_type"`
	Authority      string     `json:"authority"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	Scope          []string   `json:"scope"`
	Status         string     `json:"status"`
	EffectiveFrom  *string    `json:"effective_from,omitempty"`
	EffectiveUntil *string    `json:"effective_until,omitempty"`
	Supersedes     *int64     `json:"supersedes,omitempty"`
	SupersededBy   *int64     `json:"superseded_by,omitempty"`
	Source         string     `json:"source"`
	SourceRef      *string    `json:"source_ref,omitempty"`
	CreatedBy      string     `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	VerifiedAt     *time.Time `json:"verified_at,omitempty"`
	VerifiedBy     *string    `json:"verified_by,omitempty"`
	Rank           float64    `json:"rank,omitempty"`
	EvidenceCount  int        `json:"evidence_count"`
}

type Evidence struct {
	ID          int64     `json:"id"`
	ClaimID     int64     `json:"claim_id"`
	Checker     string    `json:"checker"`      // the query / command / file:line that was run
	Result      string    `json:"result"`       // what it returned, verbatim, credentials redacted
	ObservedAt  time.Time `json:"observed_at"`  // when the result was observed
	Environment string    `json:"environment"`  // prod-pg-us-west-2 | lake | repo | ses-us-west-1 | operator
	CodeVersion string    `json:"code_version"` // git sha of the code that produced/consumed it
	Verdict     string    `json:"verdict"`      // supports | refutes | inconclusive
	RecordedBy  string    `json:"recorded_by"`
	CreatedAt   time.Time `json:"created_at"`
}

type Capability struct {
	ID                  int64     `json:"id"`
	OrgID               string    `json:"org_id"`
	Name                string    `json:"name"`
	Version             int       `json:"version"`
	Task                string    `json:"task"`                 // the recognised question/request it answers
	Tool                string    `json:"tool"`                 // endpoint / module / command that performs it
	Inputs              string    `json:"inputs"`               // required inputs, in prose or JSON
	RequiredEvidence    string    `json:"required_evidence"`    // what every answer must carry
	CompletionCondition string    `json:"completion_condition"` // when the task is done
	Status              string    `json:"status"`
	CreatedBy           string    `json:"created_by"`
	CreatedAt           time.Time `json:"created_at"`
}

// Eval checkers the worker can run on its own:
//
//	pg_sql    spec.query against the platform DB (read-only tx); expected = rows
//	lake_sql  spec.query against Athena; expected = rows
//	http      spec.method/url/body against this server; expected = JSON subset
type Eval struct {
	ID              int64          `json:"id"`
	OrgID           string         `json:"org_id"`
	Name            string         `json:"name"`
	Question        string         `json:"question"`
	Checker         string         `json:"checker"`
	Spec            map[string]any `json:"spec"`
	Expected        any            `json:"expected"`
	TolerancePct    float64        `json:"tolerance_pct"`
	AsOf            *string        `json:"as_of,omitempty"` // date the expected values were pinned
	ClaimID         *int64         `json:"claim_id,omitempty"`
	CapabilityID    *int64         `json:"capability_id,omitempty"`
	Status          string         `json:"status"`
	LastRunAt       *time.Time     `json:"last_run_at,omitempty"`
	LastPass        *bool          `json:"last_pass,omitempty"`
	LastResult      any            `json:"last_result,omitempty"`
	LastCodeVersion *string        `json:"last_code_version,omitempty"`
	CreatedBy       string         `json:"created_by"`
	CreatedAt       time.Time      `json:"created_at"`
}

type EvalRun struct {
	ID          int64     `json:"id"`
	EvalID      int64     `json:"eval_id"`
	RanAt       time.Time `json:"ran_at"`
	Pass        bool      `json:"pass"`
	Result      any       `json:"result"`
	Error       string    `json:"error,omitempty"`
	CodeVersion string    `json:"code_version"`
	DurationMs  int64     `json:"duration_ms"`
}
