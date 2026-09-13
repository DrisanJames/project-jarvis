package brain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Store is the only writer/reader of the jarvis_brain_* tables. Handlers and
// workers go through it; nothing else issues SQL against these tables.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("verifier not permitted for this claim type")
	ErrBadInput  = errors.New("bad input")
	scopeTokenRe = regexp.MustCompile(`^[a-z0-9_.:@+-]{1,64}$`)
	credentialRe = regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|api[_-]?key|authorization)\s*[:=]\s*\S+|postgres(ql)?://[^\s]+|AKIA[0-9A-Z]{16}|sk-[A-Za-z0-9_-]{16,}|xox[abp]-[A-Za-z0-9-]+`)
)

// Redact strips credential-shaped substrings before evidence is stored.
func Redact(s string) string {
	return credentialRe.ReplaceAllString(s, "[redacted]")
}

func normScope(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if !scopeTokenRe.MatchString(s) {
			return nil, fmt.Errorf("%w: scope token %q (use e.g. isp:gmail, system:vdm, domain:em.x.com)", ErrBadInput, s)
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- claims

type ClaimInput struct {
	ClaimType      string   `json:"claim_type"`
	Authority      string   `json:"authority"`
	Title          string   `json:"title"`
	Body           string   `json:"body"`
	Scope          []string `json:"scope"`
	EffectiveFrom  *string  `json:"effective_from"`
	EffectiveUntil *string  `json:"effective_until"`
	Supersedes     *int64   `json:"supersedes"`
	Source         string   `json:"source"`
	SourceRef      *string  `json:"source_ref"`
	CreatedBy      string   `json:"created_by"`
}

func (in *ClaimInput) validate() error {
	if !contains(ClaimTypes, in.ClaimType) {
		return fmt.Errorf("%w: claim_type must be one of %v", ErrBadInput, ClaimTypes)
	}
	if in.Authority == "" {
		switch RequiredVerifier(in.ClaimType) {
		case "operator":
			in.Authority = "operator"
		case "checker":
			in.Authority = "runtime"
		default:
			in.Authority = "review"
		}
	}
	if !contains(Authorities, in.Authority) {
		return fmt.Errorf("%w: authority must be one of %v", ErrBadInput, Authorities)
	}
	in.Title = strings.TrimSpace(in.Title)
	in.Body = strings.TrimSpace(in.Body)
	if in.Title == "" || in.Body == "" {
		return fmt.Errorf("%w: title and body are required", ErrBadInput)
	}
	if in.Source == "" {
		in.Source = "session"
	}
	if in.CreatedBy == "" {
		in.CreatedBy = "claude"
	}
	return nil
}

const claimCols = `c.id, c.org_id, c.claim_type, c.authority, c.title, c.body, c.scope, c.status,
	to_char(c.effective_from,'YYYY-MM-DD'), to_char(c.effective_until,'YYYY-MM-DD'),
	c.supersedes, c.superseded_by, c.source, c.source_ref, c.created_by, c.created_at, c.updated_at,
	c.verified_at, c.verified_by,
	(SELECT count(*) FROM jarvis_brain_evidence e WHERE e.claim_id = c.id)`

func scanClaim(sc interface{ Scan(...any) error }, withRank bool) (Claim, error) {
	var c Claim
	var scope pq.StringArray
	var effFrom, effUntil, srcRef, verBy sql.NullString
	var sup, supBy sql.NullInt64
	var verAt sql.NullTime
	dest := []any{&c.ID, &c.OrgID, &c.ClaimType, &c.Authority, &c.Title, &c.Body, &scope, &c.Status,
		&effFrom, &effUntil, &sup, &supBy, &c.Source, &srcRef, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
		&verAt, &verBy, &c.EvidenceCount}
	if withRank {
		dest = append(dest, &c.Rank)
	}
	if err := sc.Scan(dest...); err != nil {
		return c, err
	}
	c.Scope = []string(scope)
	if c.Scope == nil {
		c.Scope = []string{}
	}
	if effFrom.Valid {
		c.EffectiveFrom = &effFrom.String
	}
	if effUntil.Valid {
		c.EffectiveUntil = &effUntil.String
	}
	if sup.Valid {
		c.Supersedes = &sup.Int64
	}
	if supBy.Valid {
		c.SupersededBy = &supBy.Int64
	}
	if srcRef.Valid {
		c.SourceRef = &srcRef.String
	}
	if verAt.Valid {
		c.VerifiedAt = &verAt.Time
	}
	if verBy.Valid {
		c.VerifiedBy = &verBy.String
	}
	return c, nil
}

// RecordClaim inserts a claim as `candidate`. When source_ref is set the row
// is idempotent on (org_id, source, source_ref) and an existing row is updated
// in place (status untouched). When supersedes is set, the older claim is
// marked superseded in the same transaction.
func (s *Store) RecordClaim(ctx context.Context, orgID string, in ClaimInput) (Claim, error) {
	if err := in.validate(); err != nil {
		return Claim{}, err
	}
	scope, err := normScope(in.Scope)
	if err != nil {
		return Claim{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Claim{}, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO jarvis_brain_claims
		    (org_id, claim_type, authority, title, body, scope, effective_from, effective_until,
		     supersedes, source, source_ref, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7::date,$8::date,$9,$10,$11,$12)
		ON CONFLICT (org_id, source, source_ref) WHERE source_ref IS NOT NULL DO UPDATE SET
		    claim_type=EXCLUDED.claim_type, authority=EXCLUDED.authority, title=EXCLUDED.title,
		    body=EXCLUDED.body, scope=EXCLUDED.scope, effective_from=EXCLUDED.effective_from,
		    effective_until=EXCLUDED.effective_until, updated_at=now()
		RETURNING id`,
		orgID, in.ClaimType, in.Authority, in.Title, in.Body, pq.Array(scope), in.EffectiveFrom,
		in.EffectiveUntil, in.Supersedes, in.Source, in.SourceRef, in.CreatedBy).Scan(&id)
	if err != nil {
		return Claim{}, err
	}
	if in.Supersedes != nil && *in.Supersedes != id {
		if _, err := tx.ExecContext(ctx, `
			UPDATE jarvis_brain_claims SET status='superseded', superseded_by=$1, updated_at=now()
			WHERE id=$2 AND org_id=$3 AND status<>'retracted'`, id, *in.Supersedes, orgID); err != nil {
			return Claim{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Claim{}, err
	}
	return s.GetClaim(ctx, orgID, id)
}

func (s *Store) GetClaim(ctx context.Context, orgID string, id int64) (Claim, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+claimCols+` FROM jarvis_brain_claims c WHERE c.id=$1 AND c.org_id=$2`, id, orgID)
	c, err := scanClaim(row, false)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

type RecallOptions struct {
	ClaimType string
	Scope     []string
	Statuses  []string // default: active + candidate (candidates are LABELLED, never hidden)
	Limit     int
}

// Recall is ranked full-text search (title A > body B) with a trigram title
// fallback. Results carry status so a candidate is never mistaken for an
// active claim; supersedes/retracted rows are excluded unless asked for.
func (s *Store) Recall(ctx context.Context, orgID, query string, o RecallOptions) ([]Claim, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("%w: query is required", ErrBadInput)
	}
	if o.Limit <= 0 || o.Limit > 50 {
		o.Limit = 8
	}
	statuses := o.Statuses
	if len(statuses) == 0 {
		statuses = []string{"active", "candidate"}
	}
	for _, st := range statuses {
		if !contains(ClaimStatuses, st) {
			return nil, fmt.Errorf("%w: status %q", ErrBadInput, st)
		}
	}
	where := []string{"c.org_id=$1", "c.status = ANY($2)"}
	args := []any{orgID, pq.Array(statuses)}
	if o.ClaimType != "" {
		if !contains(ClaimTypes, o.ClaimType) {
			return nil, fmt.Errorf("%w: claim_type %q", ErrBadInput, o.ClaimType)
		}
		args = append(args, o.ClaimType)
		where = append(where, fmt.Sprintf("c.claim_type=$%d", len(args)))
	}
	if len(o.Scope) > 0 {
		sc, err := normScope(o.Scope)
		if err != nil {
			return nil, err
		}
		args = append(args, pq.Array(sc))
		where = append(where, fmt.Sprintf("c.scope && $%d", len(args)))
	}
	w := strings.Join(where, " AND ")
	args = append(args, query)
	qi := len(args)
	args = append(args, o.Limit)
	li := len(args)
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT `+claimCols+`, ts_rank_cd(c.tsv, q, 32) AS rank
		FROM jarvis_brain_claims c, websearch_to_tsquery('english', $%d) q
		WHERE %s AND c.tsv @@ q
		ORDER BY (c.status='active') DESC, rank DESC, c.updated_at DESC LIMIT $%d`, qi, w, li), args...)
	if err != nil {
		return nil, err
	}
	out, err := collectClaims(rows, true)
	if err != nil || len(out) > 0 {
		return out, err
	}
	rows, err = s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT `+claimCols+`, similarity(c.title, $%d) AS rank
		FROM jarvis_brain_claims c
		WHERE %s AND similarity(c.title, $%d) > 0.2
		ORDER BY (c.status='active') DESC, rank DESC, c.updated_at DESC LIMIT $%d`, qi, w, qi, li), args...)
	if err != nil {
		return nil, err
	}
	return collectClaims(rows, true)
}

func collectClaims(rows *sql.Rows, withRank bool) ([]Claim, error) {
	defer rows.Close()
	out := []Claim{}
	for rows.Next() {
		c, err := scanClaim(rows, withRank)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Recent(ctx context.Context, orgID string, days, limit int, status string) ([]Claim, error) {
	if days <= 0 {
		days = 7
	}
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	args := []any{orgID, days, limit}
	st := ""
	if status != "" {
		if !contains(ClaimStatuses, status) {
			return nil, fmt.Errorf("%w: status %q", ErrBadInput, status)
		}
		args = append(args, status)
		st = " AND c.status=$4"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+claimCols+` FROM jarvis_brain_claims c
		WHERE c.org_id=$1 AND c.created_at >= now() - make_interval(days => $2)`+st+`
		ORDER BY c.created_at DESC LIMIT $3`, args...)
	if err != nil {
		return nil, err
	}
	return collectClaims(rows, false)
}

// ---------------------------------------------------------------- evidence

type EvidenceInput struct {
	Checker     string     `json:"checker"`
	Result      string     `json:"result"`
	ObservedAt  *time.Time `json:"observed_at"`
	Environment string     `json:"environment"`
	CodeVersion string     `json:"code_version"`
	Verdict     string     `json:"verdict"` // supports | refutes | inconclusive
	RecordedBy  string     `json:"recorded_by"`
}

func (in *EvidenceInput) validate() error {
	in.Checker = strings.TrimSpace(in.Checker)
	in.Result = Redact(strings.TrimSpace(in.Result))
	if in.Checker == "" {
		return fmt.Errorf("%w: checker (the query/command/file:line that was run) is required", ErrBadInput)
	}
	if in.Result == "" {
		return fmt.Errorf("%w: result (what the checker returned) is required — a check without its result is not evidence", ErrBadInput)
	}
	switch in.Verdict {
	case "supports", "refutes", "inconclusive":
	case "":
		in.Verdict = "supports"
	default:
		return fmt.Errorf("%w: verdict must be supports|refutes|inconclusive", ErrBadInput)
	}
	if in.Environment == "" {
		return fmt.Errorf("%w: environment is required (prod-pg-us-west-2 | lake | repo | ses-us-west-1 | operator)", ErrBadInput)
	}
	if in.RecordedBy == "" {
		in.RecordedBy = "claude"
	}
	return nil
}

func (s *Store) AddEvidence(ctx context.Context, orgID string, claimID int64, in EvidenceInput) (Evidence, error) {
	if err := in.validate(); err != nil {
		return Evidence{}, err
	}
	if _, err := s.GetClaim(ctx, orgID, claimID); err != nil {
		return Evidence{}, err
	}
	obs := time.Now().UTC()
	if in.ObservedAt != nil {
		obs = *in.ObservedAt
	}
	var e Evidence
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO jarvis_brain_evidence
		    (claim_id, checker, result, observed_at, environment, code_version, verdict, recorded_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id, claim_id, checker, result, observed_at, environment, code_version, verdict, recorded_by, created_at`,
		claimID, in.Checker, in.Result, obs, in.Environment, in.CodeVersion, in.Verdict, in.RecordedBy,
	).Scan(&e.ID, &e.ClaimID, &e.Checker, &e.Result, &e.ObservedAt, &e.Environment, &e.CodeVersion,
		&e.Verdict, &e.RecordedBy, &e.CreatedAt)
	return e, err
}

func (s *Store) ListEvidence(ctx context.Context, orgID string, claimID int64) ([]Evidence, error) {
	if _, err := s.GetClaim(ctx, orgID, claimID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, claim_id, checker, result, observed_at, environment, code_version, verdict, recorded_by, created_at
		FROM jarvis_brain_evidence WHERE claim_id=$1 ORDER BY observed_at DESC, id DESC`, claimID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Evidence{}
	for rows.Next() {
		var e Evidence
		if err := rows.Scan(&e.ID, &e.ClaimID, &e.Checker, &e.Result, &e.ObservedAt, &e.Environment,
			&e.CodeVersion, &e.Verdict, &e.RecordedBy, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Verify records supporting evidence and moves the claim to `active`.
// verifierRole is who is asserting: operator | checker | agent. policy and
// definition claims accept ONLY the operator; every other type accepts a
// checker or an agent, but the evidence must carry a result either way.
// An agent calling this does not confer authority on itself — the row records
// exactly who verified, and policy/definition are refused.
func (s *Store) Verify(ctx context.Context, orgID string, claimID int64, verifierRole, verifierName string, ev EvidenceInput) (Claim, error) {
	c, err := s.GetClaim(ctx, orgID, claimID)
	if err != nil {
		return c, err
	}
	need := RequiredVerifier(c.ClaimType)
	switch verifierRole {
	case "operator":
	case "checker", "agent":
		if need == "operator" {
			return c, fmt.Errorf("%w: %s claims are verified by the operator only", ErrForbidden, c.ClaimType)
		}
	default:
		return c, fmt.Errorf("%w: verifier_role must be operator|checker|agent", ErrBadInput)
	}
	if c.Status == "superseded" || c.Status == "retracted" {
		return c, fmt.Errorf("%w: claim is %s", ErrBadInput, c.Status)
	}
	ev.Verdict = "supports"
	if ev.RecordedBy == "" {
		ev.RecordedBy = verifierRole + ":" + verifierName
	}
	if _, err := s.AddEvidence(ctx, orgID, claimID, ev); err != nil {
		return c, err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE jarvis_brain_claims SET status='active', verified_at=now(),
		verified_by=$1, updated_at=now() WHERE id=$2 AND org_id=$3`, verifierRole+":"+verifierName, claimID, orgID)
	if err != nil {
		return c, err
	}
	return s.GetClaim(ctx, orgID, claimID)
}

// Refute records refuting evidence and retracts the claim.
func (s *Store) Refute(ctx context.Context, orgID string, claimID int64, by string, ev EvidenceInput) (Claim, error) {
	ev.Verdict = "refutes"
	if ev.RecordedBy == "" {
		ev.RecordedBy = by
	}
	if _, err := s.AddEvidence(ctx, orgID, claimID, ev); err != nil {
		return Claim{}, err
	}
	return s.Retract(ctx, orgID, claimID, "refuted: "+ev.Checker, by)
}

func (s *Store) Retract(ctx context.Context, orgID string, claimID int64, reason, by string) (Claim, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jarvis_brain_claims
		SET status='retracted', body = body || E'\n\n[retracted ' || to_char(now(),'YYYY-MM-DD') || ' by ' || $1 || '] ' || $2,
		    updated_at=now()
		WHERE id=$3 AND org_id=$4 AND status<>'retracted'`, by, reason, claimID, orgID)
	if err != nil {
		return Claim{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.GetClaim(ctx, orgID, claimID); err != nil {
			return Claim{}, err
		}
	}
	return s.GetClaim(ctx, orgID, claimID)
}

func (s *Store) Supersede(ctx context.Context, orgID string, oldID, newID int64, reason, by string) (Claim, error) {
	if oldID == newID {
		return Claim{}, fmt.Errorf("%w: a claim cannot supersede itself", ErrBadInput)
	}
	if _, err := s.GetClaim(ctx, orgID, newID); err != nil {
		return Claim{}, fmt.Errorf("new claim: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Claim{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE jarvis_brain_claims
		SET status='superseded', superseded_by=$1,
		    body = body || E'\n\n[superseded ' || to_char(now(),'YYYY-MM-DD') || ' by ' || $2 || ' → #' || $6 || '] ' || $3,
		    updated_at=now()
		WHERE id=$4 AND org_id=$5 AND status IN ('active','candidate')`, newID, by, reason, oldID, orgID, strconv.FormatInt(newID, 10))
	if err != nil {
		return Claim{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Claim{}, fmt.Errorf("old claim: %w (or not active/candidate)", ErrNotFound)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jarvis_brain_claims SET supersedes=$1, updated_at=now()
		WHERE id=$2 AND org_id=$3`, oldID, newID, orgID); err != nil {
		return Claim{}, err
	}
	if err := tx.Commit(); err != nil {
		return Claim{}, err
	}
	return s.GetClaim(ctx, orgID, oldID)
}

// ---------------------------------------------------------------- capabilities

type CapabilityInput struct {
	Name                string `json:"name"`
	Task                string `json:"task"`
	Tool                string `json:"tool"`
	Inputs              string `json:"inputs"`
	RequiredEvidence    string `json:"required_evidence"`
	CompletionCondition string `json:"completion_condition"`
	CreatedBy           string `json:"created_by"`
}

var capNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)

// UpsertCapability registers a new VERSION of a named capability (versions
// are never edited in place) and retires the previous active version.
func (s *Store) UpsertCapability(ctx context.Context, orgID string, in CapabilityInput) (Capability, error) {
	if !capNameRe.MatchString(in.Name) {
		return Capability{}, fmt.Errorf("%w: name must match %s", ErrBadInput, capNameRe)
	}
	if strings.TrimSpace(in.Task) == "" || strings.TrimSpace(in.Tool) == "" {
		return Capability{}, fmt.Errorf("%w: task and tool are required", ErrBadInput)
	}
	if in.CreatedBy == "" {
		in.CreatedBy = "claude"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Capability{}, err
	}
	defer tx.Rollback()
	var ver int
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(version),0)+1 FROM jarvis_brain_capabilities
		WHERE org_id=$1 AND name=$2`, orgID, in.Name).Scan(&ver); err != nil {
		return Capability{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jarvis_brain_capabilities SET status='retired'
		WHERE org_id=$1 AND name=$2 AND status='active'`, orgID, in.Name); err != nil {
		return Capability{}, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO jarvis_brain_capabilities
		(org_id, name, version, task, tool, inputs, required_evidence, completion_condition, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		orgID, in.Name, ver, in.Task, in.Tool, in.Inputs, in.RequiredEvidence, in.CompletionCondition, in.CreatedBy).Scan(&id); err != nil {
		return Capability{}, err
	}
	if err := tx.Commit(); err != nil {
		return Capability{}, err
	}
	return s.getCapability(ctx, orgID, id)
}

const capCols = `id, org_id, name, version, task, tool, inputs, required_evidence, completion_condition, status, created_by, created_at`

func scanCap(sc interface{ Scan(...any) error }) (Capability, error) {
	var c Capability
	err := sc.Scan(&c.ID, &c.OrgID, &c.Name, &c.Version, &c.Task, &c.Tool, &c.Inputs, &c.RequiredEvidence,
		&c.CompletionCondition, &c.Status, &c.CreatedBy, &c.CreatedAt)
	return c, err
}

func (s *Store) getCapability(ctx context.Context, orgID string, id int64) (Capability, error) {
	c, err := scanCap(s.db.QueryRowContext(ctx, `SELECT `+capCols+` FROM jarvis_brain_capabilities WHERE id=$1 AND org_id=$2`, id, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// ResolveCapability returns the active version of a named capability — the
// direct, by-name path a task resolves through (search only adds context).
func (s *Store) ResolveCapability(ctx context.Context, orgID, name string) (Capability, error) {
	c, err := scanCap(s.db.QueryRowContext(ctx, `SELECT `+capCols+` FROM jarvis_brain_capabilities
		WHERE org_id=$1 AND name=$2 AND status='active' ORDER BY version DESC LIMIT 1`, orgID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

func (s *Store) ListCapabilities(ctx context.Context, orgID string, includeRetired bool) ([]Capability, error) {
	st := " AND status='active'"
	if includeRetired {
		st = ""
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+capCols+` FROM jarvis_brain_capabilities WHERE org_id=$1`+st+` ORDER BY name, version DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Capability{}
	for rows.Next() {
		c, err := scanCap(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- evals

type EvalInput struct {
	Name         string         `json:"name"`
	Question     string         `json:"question"`
	Checker      string         `json:"checker"`
	Spec         map[string]any `json:"spec"`
	Expected     any            `json:"expected"`
	TolerancePct float64        `json:"tolerance_pct"`
	AsOf         *string        `json:"as_of"`
	ClaimID      *int64         `json:"claim_id"`
	CapabilityID *int64         `json:"capability_id"`
	CreatedBy    string         `json:"created_by"`
}

var EvalCheckers = []string{"pg_sql", "lake_sql", "http"}

func (s *Store) UpsertEval(ctx context.Context, orgID string, in EvalInput) (Eval, error) {
	if !capNameRe.MatchString(in.Name) {
		return Eval{}, fmt.Errorf("%w: name must match %s", ErrBadInput, capNameRe)
	}
	if !contains(EvalCheckers, in.Checker) {
		return Eval{}, fmt.Errorf("%w: checker must be one of %v", ErrBadInput, EvalCheckers)
	}
	if strings.TrimSpace(in.Question) == "" || in.Spec == nil || in.Expected == nil {
		return Eval{}, fmt.Errorf("%w: question, spec and expected are required", ErrBadInput)
	}
	if in.Checker != "http" {
		q, _ := in.Spec["query"].(string)
		if err := AssertReadOnlySQL(q); err != nil {
			return Eval{}, err
		}
	}
	if in.CreatedBy == "" {
		in.CreatedBy = "claude"
	}
	spec, _ := json.Marshal(in.Spec)
	exp, _ := json.Marshal(in.Expected)
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO jarvis_brain_evals
		(org_id, name, question, checker, spec, expected, tolerance_pct, as_of, claim_id, capability_id, created_by)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7,$8::date,$9,$10,$11)
		ON CONFLICT (org_id, name) DO UPDATE SET question=EXCLUDED.question, checker=EXCLUDED.checker,
		    spec=EXCLUDED.spec, expected=EXCLUDED.expected, tolerance_pct=EXCLUDED.tolerance_pct,
		    as_of=EXCLUDED.as_of, claim_id=EXCLUDED.claim_id, capability_id=EXCLUDED.capability_id, status='active'
		RETURNING id`,
		orgID, in.Name, in.Question, in.Checker, string(spec), string(exp), in.TolerancePct, in.AsOf,
		in.ClaimID, in.CapabilityID, in.CreatedBy).Scan(&id)
	if err != nil {
		return Eval{}, err
	}
	return s.GetEval(ctx, orgID, id)
}

const evalCols = `id, org_id, name, question, checker, spec, expected, tolerance_pct, to_char(as_of,'YYYY-MM-DD'),
	claim_id, capability_id, status, last_run_at, last_pass, last_result, last_code_version, created_by, created_at`

func scanEval(sc interface{ Scan(...any) error }) (Eval, error) {
	var e Eval
	var spec, exp []byte
	var asOf, lastCode sql.NullString
	var claimID, capID sql.NullInt64
	var lastRun sql.NullTime
	var lastPass sql.NullBool
	var lastRes []byte
	err := sc.Scan(&e.ID, &e.OrgID, &e.Name, &e.Question, &e.Checker, &spec, &exp, &e.TolerancePct, &asOf,
		&claimID, &capID, &e.Status, &lastRun, &lastPass, &lastRes, &lastCode, &e.CreatedBy, &e.CreatedAt)
	if err != nil {
		return e, err
	}
	_ = json.Unmarshal(spec, &e.Spec)
	_ = json.Unmarshal(exp, &e.Expected)
	if asOf.Valid {
		e.AsOf = &asOf.String
	}
	if claimID.Valid {
		e.ClaimID = &claimID.Int64
	}
	if capID.Valid {
		e.CapabilityID = &capID.Int64
	}
	if lastRun.Valid {
		e.LastRunAt = &lastRun.Time
	}
	if lastPass.Valid {
		e.LastPass = &lastPass.Bool
	}
	if len(lastRes) > 0 {
		_ = json.Unmarshal(lastRes, &e.LastResult)
	}
	if lastCode.Valid {
		e.LastCodeVersion = &lastCode.String
	}
	return e, nil
}

func (s *Store) GetEval(ctx context.Context, orgID string, id int64) (Eval, error) {
	e, err := scanEval(s.db.QueryRowContext(ctx, `SELECT `+evalCols+` FROM jarvis_brain_evals WHERE id=$1 AND org_id=$2`, id, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	return e, err
}

func (s *Store) ListEvals(ctx context.Context, orgID string, activeOnly bool) ([]Eval, error) {
	st := ""
	if activeOnly {
		st = " AND status='active'"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+evalCols+` FROM jarvis_brain_evals WHERE org_id=$1`+st+` ORDER BY name`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Eval{}
	for rows.Next() {
		e, err := scanEval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListEvalsAllOrgs is the worker's entry: every active eval across orgs.
func (s *Store) ListEvalsAllOrgs(ctx context.Context) ([]Eval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+evalCols+` FROM jarvis_brain_evals WHERE status='active' ORDER BY org_id, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Eval{}
	for rows.Next() {
		e, err := scanEval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) RetireEval(ctx context.Context, orgID string, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE jarvis_brain_evals SET status='retired' WHERE id=$1 AND org_id=$2`, id, orgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordEvalRun appends a run row and updates the eval's last_* summary.
func (s *Store) RecordEvalRun(ctx context.Context, run EvalRun) (EvalRun, error) {
	res, _ := json.Marshal(run.Result)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return run, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `INSERT INTO jarvis_brain_eval_runs
		(eval_id, ran_at, pass, result, error, code_version, duration_ms)
		VALUES ($1, now(), $2, $3::jsonb, $4, $5, $6) RETURNING id, ran_at`,
		run.EvalID, run.Pass, string(res), run.Error, run.CodeVersion, run.DurationMs).Scan(&run.ID, &run.RanAt); err != nil {
		return run, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jarvis_brain_evals SET last_run_at=now(), last_pass=$1,
		last_result=$2::jsonb, last_code_version=$3 WHERE id=$4`, run.Pass, string(res), run.CodeVersion, run.EvalID); err != nil {
		return run, err
	}
	return run, tx.Commit()
}

func (s *Store) ListEvalRuns(ctx context.Context, orgID string, evalID int64, limit int) ([]EvalRun, error) {
	if _, err := s.GetEval(ctx, orgID, evalID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, eval_id, ran_at, pass, result, coalesce(error,''), code_version, duration_ms
		FROM jarvis_brain_eval_runs WHERE eval_id=$1 ORDER BY ran_at DESC LIMIT $2`, evalID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EvalRun{}
	for rows.Next() {
		var r EvalRun
		var res []byte
		if err := rows.Scan(&r.ID, &r.EvalID, &r.RanAt, &r.Pass, &res, &r.Error, &r.CodeVersion, &r.DurationMs); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(res, &r.Result)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- stats

type Stats struct {
	Claims       []map[string]any `json:"claims"`
	Capabilities []map[string]any `json:"capabilities"`
	Evals        map[string]any   `json:"evals"`
}

func (s *Store) Stats(ctx context.Context, orgID string) (Stats, error) {
	var st Stats
	rows, err := s.db.QueryContext(ctx, `SELECT claim_type, status, count(*) FROM jarvis_brain_claims WHERE org_id=$1 GROUP BY 1,2 ORDER BY 1,2`, orgID)
	if err != nil {
		return st, err
	}
	st.Claims = []map[string]any{}
	for rows.Next() {
		var t, s string
		var n int64
		if err := rows.Scan(&t, &s, &n); err != nil {
			rows.Close()
			return st, err
		}
		st.Claims = append(st.Claims, map[string]any{"claim_type": t, "status": s, "n": n})
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT name, max(version), bool_or(status='active') FROM jarvis_brain_capabilities WHERE org_id=$1 GROUP BY 1 ORDER BY 1`, orgID)
	if err != nil {
		return st, err
	}
	st.Capabilities = []map[string]any{}
	for rows.Next() {
		var n string
		var v int
		var a bool
		if err := rows.Scan(&n, &v, &a); err != nil {
			rows.Close()
			return st, err
		}
		st.Capabilities = append(st.Capabilities, map[string]any{"name": n, "version": v, "active": a})
	}
	rows.Close()
	var total, passing, failing, never int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*), count(*) FILTER (WHERE last_pass), count(*) FILTER (WHERE last_pass=false),
		count(*) FILTER (WHERE last_run_at IS NULL) FROM jarvis_brain_evals WHERE org_id=$1 AND status='active'`, orgID).
		Scan(&total, &passing, &failing, &never); err != nil {
		return st, err
	}
	st.Evals = map[string]any{"active": total, "passing": passing, "failing": failing, "never_run": never}
	return st, nil
}

// ---------------------------------------------------------------- read-only SQL guard

var writeRe = regexp.MustCompile(`(?i)\b(insert|update|delete|drop|alter|create|truncate|grant|revoke|copy|vacuum|merge|call|refresh|reindex|comment|lock|do|unload|msck)\b`)

// AssertReadOnlySQL mirrors agents/dbknowledge/_db.py assert_readonly_sql so
// an eval can never carry a write.
func AssertReadOnlySQL(q string) error {
	s := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(q), ";"))
	low := strings.ToLower(strings.TrimLeft(s, "( \t\n"))
	if !(strings.HasPrefix(low, "select") || strings.HasPrefix(low, "with")) {
		return fmt.Errorf("%w: only SELECT / WITH queries are permitted", ErrBadInput)
	}
	if strings.Contains(s, ";") {
		return fmt.Errorf("%w: multiple statements are not permitted", ErrBadInput)
	}
	if writeRe.MatchString(s) {
		return fmt.Errorf("%w: write/DDL keyword detected", ErrBadInput)
	}
	return nil
}
