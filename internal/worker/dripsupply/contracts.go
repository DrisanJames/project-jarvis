// Package dripsupply implements the static-contract layer of the Drip Supply
// Chain (REQ-118, docs/DRIP_SUPPLY_CHAIN_DESIGN.md).
//
// Contracts are POLICY. They are never mutated by a running mediator: an
// operator or the lead writes a `draft`, approves it to `scheduled` with an
// `effective_at`, and ActivateScheduled flips it to `active` at the day
// boundary while retiring the row it replaces. Exactly one row per subject may
// be `active`, enforced in the database by a partial unique index (§1.1) whose
// name this package owns (ActiveIndexName / ActiveIndexDDL) so the DDL and the
// activation path cannot drift apart.
//
// Everything here FAILS CLOSED. A lane or domain with no active contract is not
// given a default — LoadActive's lookups return *ErrNoActiveContract and the
// caller is expected to skip the cell, write a `skipped:no_contract` tick
// outcome and alert (§2.1).
package dripsupply

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/pkg/contractmeta"
)

// ---------------------------------------------------------------------------
// ISP classes
// ---------------------------------------------------------------------------

// ispClasses is the canonical set of ISP classes a contract may mention.
//
// SINGLE SOURCE: these are exactly the keys of
// worker.DefaultPerISPCapPerWave() (internal/worker/partner_drip_orchestrator.go).
// They are restated here rather than imported because internal/worker imports
// THIS package (§2, the service is injected into PartnerDripOrchestrator), so a
// production import of the parent package would be an import cycle. The parity
// is pinned by TestISPClassesMatchOrchestratorDefaults in the external test
// package, which can import both sides without creating a cycle. If that test
// fails, the orchestrator's map changed and this list must follow it.
var ispClasses = []string{
	"aol",
	"apple",
	"att",
	"charter",
	"comcast",
	"cox",
	"gmail",
	"microsoft",
	"other",
	"sbcglobal",
	"verizon",
	"yahoo",
}

var ispClassSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(ispClasses))
	for _, c := range ispClasses {
		m[c] = struct{}{}
	}
	return m
}()

// ISPClasses returns a copy of the canonical ISP class list, sorted.
func ISPClasses() []string {
	out := make([]string, len(ispClasses))
	copy(out, ispClasses)
	return out
}

// IsISPClass reports whether s is one of the canonical ISP classes.
func IsISPClass(s string) bool {
	_, ok := ispClassSet[s]
	return ok
}

// ---------------------------------------------------------------------------
// Status + kind
// ---------------------------------------------------------------------------

// ContractStatus is the lifecycle of a contract row (§1.1).
type ContractStatus string

const (
	StatusDraft      ContractStatus = "draft"
	StatusApproved   ContractStatus = "approved"
	StatusScheduled  ContractStatus = "scheduled"
	StatusActive     ContractStatus = "active"
	StatusSuperseded ContractStatus = "superseded"
)

// AllStatuses lists every legal status, in lifecycle order.
func AllStatuses() []ContractStatus {
	return []ContractStatus{StatusDraft, StatusApproved, StatusScheduled, StatusActive, StatusSuperseded}
}

// Valid reports whether s is a known status.
func (s ContractStatus) Valid() bool {
	switch s {
	case StatusDraft, StatusApproved, StatusScheduled, StatusActive, StatusSuperseded:
		return true
	}
	return false
}

// Scan implements sql.Scanner.
func (s *ContractStatus) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*s = ""
	case string:
		*s = ContractStatus(v)
	case []byte:
		*s = ContractStatus(string(v))
	default:
		return fmt.Errorf("dripsupply: cannot scan %T into ContractStatus", src)
	}
	return nil
}

// Value implements driver.Valuer.
func (s ContractStatus) Value() (driver.Value, error) { return string(s), nil }

// ContractKind names one of the four contract tables (§1.1).
type ContractKind string

const (
	KindDomain    ContractKind = "domain"
	KindDispatch  ContractKind = "dispatch"
	KindInventory ContractKind = "inventory"
	KindSource    ContractKind = "source"
)

// AllKinds lists every contract kind, in the order LoadActive and
// ActivateScheduled walk them (stable, so tests and logs are deterministic).
func AllKinds() []ContractKind {
	return []ContractKind{KindDomain, KindDispatch, KindInventory, KindSource}
}

// kindSpec binds a kind to its table, its subject column, and the name of the
// partial unique index that makes "two actives for one subject" impossible.
type kindSpec struct {
	Table      string
	SubjectCol string
	ActiveIdx  string
}

var kindSpecs = map[ContractKind]kindSpec{
	KindDomain:    {Table: "drip_domain_contracts", SubjectCol: "sending_domain", ActiveIdx: ActiveIndexDomain},
	KindDispatch:  {Table: "drip_dispatch_contracts", SubjectCol: "lane", ActiveIdx: ActiveIndexDispatch},
	KindInventory: {Table: "drip_inventory_contracts", SubjectCol: "lane", ActiveIdx: ActiveIndexInventory},
	KindSource:    {Table: "drip_source_contracts", SubjectCol: "source_slug", ActiveIdx: ActiveIndexSource},
}

// The partial unique indexes of §1.1 ("Exactly one row per subject may be
// active at a time"). ActivateScheduled RELIES on these existing: it orders its
// statements so the index is never momentarily violated, and it translates a
// 23505 on one of them into *ErrDuplicateActive. WP1's DDL must create these
// exact names — use ActiveIndexDDL to emit them.
const (
	ActiveIndexDomain    = "uq_drip_domain_contracts_active"
	ActiveIndexDispatch  = "uq_drip_dispatch_contracts_active"
	ActiveIndexInventory = "uq_drip_inventory_contracts_active"
	ActiveIndexSource    = "uq_drip_source_contracts_active"
)

// TableFor returns the contract table for a kind.
func TableFor(kind ContractKind) (string, error) {
	spec, ok := kindSpecs[kind]
	if !ok {
		return "", fmt.Errorf("dripsupply: unknown contract kind %q", kind)
	}
	return spec.Table, nil
}

// SubjectColumnFor returns the subject column for a kind.
func SubjectColumnFor(kind ContractKind) (string, error) {
	spec, ok := kindSpecs[kind]
	if !ok {
		return "", fmt.Errorf("dripsupply: unknown contract kind %q", kind)
	}
	return spec.SubjectCol, nil
}

// ActiveIndexName returns the name of the partial unique index that enforces
// one active row per subject for the kind.
func ActiveIndexName(kind ContractKind) (string, error) {
	spec, ok := kindSpecs[kind]
	if !ok {
		return "", fmt.Errorf("dripsupply: unknown contract kind %q", kind)
	}
	return spec.ActiveIdx, nil
}

// ActiveIndexDDL returns the idempotent DDL for the kind's partial unique
// index. It is the ONE definition of that index: the schema work package emits
// it, and ActivateScheduled's error handling keys off the same name.
func ActiveIndexDDL(kind ContractKind) (string, error) {
	spec, ok := kindSpecs[kind]
	if !ok {
		return "", fmt.Errorf("dripsupply: unknown contract kind %q", kind)
	}
	return fmt.Sprintf(
		"CREATE UNIQUE INDEX IF NOT EXISTS %s ON %s (%s) WHERE status = 'active'",
		spec.ActiveIdx, spec.Table, spec.SubjectCol,
	), nil
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// ValidationError is one broken rule on one field.
type ValidationError struct {
	Kind    ContractKind
	Subject string
	Field   string
	Msg     string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("dripsupply: %s contract %q: %s: %s", e.Kind, e.Subject, e.Field, e.Msg)
}

// ValidationErrors is every rule a contract broke. Validate returns all of
// them, not just the first, so the portal can render a whole form's problems.
type ValidationErrors []*ValidationError

func (e ValidationErrors) Error() string {
	parts := make([]string, 0, len(e))
	for _, v := range e {
		parts = append(parts, v.Field+": "+v.Msg)
	}
	if len(e) == 0 {
		return "dripsupply: no validation errors"
	}
	return fmt.Sprintf("dripsupply: %s contract %q invalid: %s", e[0].Kind, e[0].Subject, strings.Join(parts, "; "))
}

// Fields returns the field names that failed, sorted and de-duplicated.
func (e ValidationErrors) Fields() []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(e))
	for _, v := range e {
		if _, ok := seen[v.Field]; ok {
			continue
		}
		seen[v.Field] = struct{}{}
		out = append(out, v.Field)
	}
	sort.Strings(out)
	return out
}

// HasField reports whether field failed validation.
func (e ValidationErrors) HasField(field string) bool {
	for _, v := range e {
		if v.Field == field {
			return true
		}
	}
	return false
}

// ErrNoActiveContract is returned by every ActiveSet lookup that misses. It is
// deliberately an error and not a zero value: there is no default contract, and
// a caller that cannot find one must skip the cell and alert (§2.1, §5.1).
type ErrNoActiveContract struct {
	Kind    ContractKind
	Subject string
}

func (e *ErrNoActiveContract) Error() string {
	return fmt.Sprintf("dripsupply: no active %s contract for %q", e.Kind, e.Subject)
}

// ErrTokenMismatch means an active contract's integrity token does not match
// its body (§1.5 rule 2) — the row was edited outside the sanctioned path, or
// the signing key changed without re-issuing. It fails the WHOLE ActiveSet
// load: the caller skips its work and alerts. It is never recoverable in code.
type ErrTokenMismatch struct {
	Kind    ContractKind
	Subject string
	Version int
	Err     error
}

func (e *ErrTokenMismatch) Error() string {
	return fmt.Sprintf("dripsupply: %s contract %q v%d failed token verification: %v", e.Kind, e.Subject, e.Version, e.Err)
}

func (e *ErrTokenMismatch) Unwrap() error { return e.Err }

// ErrNotApproved is returned by Schedule when the target row is not in
// `approved`. It is what makes token issue exactly-once: a second Schedule for
// the same version finds `scheduled` and refuses rather than re-stamping.
type ErrNotApproved struct {
	Kind    ContractKind
	Subject string
	Version int
	Status  ContractStatus
}

func (e *ErrNotApproved) Error() string {
	return fmt.Sprintf("dripsupply: cannot schedule %s contract %q v%d: status is %q, want %q",
		e.Kind, e.Subject, e.Version, e.Status, StatusApproved)
}

// ErrDuplicateActive is returned when the database refuses to hold two active
// rows for one subject — i.e. the §1.1 partial unique index did its job.
type ErrDuplicateActive struct {
	Kind      ContractKind
	IndexName string
	Err       error
}

func (e *ErrDuplicateActive) Error() string {
	return fmt.Sprintf("dripsupply: two active %s contracts for one subject rejected by %s: %v", e.Kind, e.IndexName, e.Err)
}

func (e *ErrDuplicateActive) Unwrap() error { return e.Err }

// ---------------------------------------------------------------------------
// Shared row shape
// ---------------------------------------------------------------------------

// Meta is the shared column set of every contract table (§1.1).
type Meta struct {
	ID             uuid.UUID      `json:"id"`
	Version        int            `json:"version"`
	Status         ContractStatus `json:"status"`
	EffectiveAt    time.Time      `json:"effective_at"`
	SupersededAt   *time.Time     `json:"superseded_at,omitempty"`
	CreatedBy      string         `json:"created_by"`
	CreatedAt      time.Time      `json:"created_at"`
	ApprovedBy     string         `json:"approved_by,omitempty"`
	ApprovedAt     *time.Time     `json:"approved_at,omitempty"`
	ChangeLedgerID string         `json:"change_ledger_id"`
	Notes          string         `json:"notes"`

	// Metadata is the standard contract metadata block (§1.5): refs, mutation
	// and the integrity token. Written by InsertDraft and Schedule.
	Metadata contractmeta.Block `json:"metadata"`
	// Token duplicates Metadata.Token.Value for indexing (§1.5). LoadActive
	// refuses a contract whose column and block disagree.
	Token string `json:"token"`
}

const metaCols = `id, version, status, effective_at, superseded_at, created_by, created_at, approved_by, approved_at, change_ledger_id, notes, metadata, token`

type metaScan struct {
	id             uuid.UUID
	version        int
	status         ContractStatus
	effectiveAt    sql.NullTime
	supersededAt   sql.NullTime
	createdBy      sql.NullString
	createdAt      sql.NullTime
	approvedBy     sql.NullString
	approvedAt     sql.NullTime
	changeLedgerID sql.NullString
	notes          sql.NullString
	metadata       contractmeta.Block
	token          sql.NullString
}

func (s *metaScan) dests() []any {
	return []any{
		&s.id, &s.version, &s.status, &s.effectiveAt, &s.supersededAt,
		&s.createdBy, &s.createdAt, &s.approvedBy, &s.approvedAt,
		&s.changeLedgerID, &s.notes, &s.metadata, &s.token,
	}
}

func (s *metaScan) into(m *Meta) {
	m.ID = s.id
	m.Version = s.version
	m.Status = s.status
	if s.effectiveAt.Valid {
		m.EffectiveAt = s.effectiveAt.Time
	}
	if s.supersededAt.Valid {
		t := s.supersededAt.Time
		m.SupersededAt = &t
	}
	m.CreatedBy = s.createdBy.String
	if s.createdAt.Valid {
		m.CreatedAt = s.createdAt.Time
	}
	m.ApprovedBy = s.approvedBy.String
	if s.approvedAt.Valid {
		t := s.approvedAt.Time
		m.ApprovedAt = &t
	}
	m.ChangeLedgerID = s.changeLedgerID.String
	m.Notes = s.notes.String
	m.Metadata = s.metadata
	m.Token = s.token.String
}

// Contract is the behaviour every contract kind shares. The unexported methods
// keep the set closed to this package's four types.
type Contract interface {
	// Kind names the contract table.
	Kind() ContractKind
	// Subject is the value of the table's subject column.
	Subject() string
	// Validate returns nil or ValidationErrors listing every broken rule.
	Validate() error
	// TokenBody returns ONLY the policy fields the integrity token covers.
	// It deliberately excludes every lifecycle field (status, effective_at,
	// superseded_at, approvals, metadata, the token itself): those change as a
	// contract moves scheduled -> active -> superseded, and a token computed
	// over them would break at the first activation.
	TokenBody() any

	metaPtr() *Meta
	insertSQL() string
	insertArgs(id uuid.UUID, version int) []any
}

var (
	_ Contract = (*DomainContract)(nil)
	_ Contract = (*DispatchContract)(nil)
	_ Contract = (*InventoryContract)(nil)
	_ Contract = (*SourceContract)(nil)
)

// ---------------------------------------------------------------------------
// Domain contract
// ---------------------------------------------------------------------------

// DomainContract is the sending side of the model: what a sending domain is
// allowed to emit per ISP per day, and when (§1.1 drip_domain_contracts).
type DomainContract struct {
	Meta
	SendingDomain     string         `json:"sending_domain"`
	BrandCode         string         `json:"brand_code"`
	DailyMaxByISP     map[string]int `json:"daily_max_by_isp"`
	ActiveWindowStart string         `json:"active_window_start"`
	ActiveWindowEnd   string         `json:"active_window_end"`
	IntervalMinutes   int            `json:"interval_minutes"`
	MaxBurstIntervals int            `json:"max_burst_intervals"`
	RampSource        string         `json:"ramp_source,omitempty"`
}

func (c *DomainContract) Kind() ContractKind { return KindDomain }
func (c *DomainContract) Subject() string    { return c.SendingDomain }
func (c *DomainContract) metaPtr() *Meta     { return &c.Meta }

// RampSourceCards and RampSourceOperator are the two legal ramp_source values.
const (
	RampSourceCards    = "sending_domain_cards"
	RampSourceOperator = "operator"
)

// Validate implements §1.1: every one of the 12 ISP classes present, integer
// values >= 0, and gmail > 0 only with notes naming the operator ruling.
func (c *DomainContract) Validate() error {
	var errs ValidationErrors
	add := func(field, msg string) {
		errs = append(errs, &ValidationError{Kind: KindDomain, Subject: c.SendingDomain, Field: field, Msg: msg})
	}

	if strings.TrimSpace(c.SendingDomain) == "" {
		add("sending_domain", "required")
	}
	if strings.TrimSpace(c.BrandCode) == "" {
		add("brand_code", "required")
	}
	if c.Status != "" && !c.Status.Valid() {
		add("status", fmt.Sprintf("unknown status %q", c.Status))
	}

	// Every ISP class present, nothing else. Missing/NULL is a save error,
	// never a default (§1.1).
	if c.DailyMaxByISP == nil {
		add("daily_max_by_isp", "required; every ISP class must carry an explicit value")
	} else {
		var missing []string
		for _, isp := range ispClasses {
			if _, ok := c.DailyMaxByISP[isp]; !ok {
				missing = append(missing, isp)
			}
		}
		if len(missing) > 0 {
			add("daily_max_by_isp", "missing ISP class(es): "+strings.Join(missing, ", "))
		}
		var unknown []string
		for isp := range c.DailyMaxByISP {
			if !IsISPClass(isp) {
				unknown = append(unknown, isp)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			add("daily_max_by_isp", "not an ISP class: "+strings.Join(unknown, ", "))
		}
		var negative []string
		for _, isp := range sortedKeys(c.DailyMaxByISP) {
			if c.DailyMaxByISP[isp] < 0 {
				negative = append(negative, fmt.Sprintf("%s=%d", isp, c.DailyMaxByISP[isp]))
			}
		}
		if len(negative) > 0 {
			add("daily_max_by_isp", "must be >= 0: "+strings.Join(negative, ", "))
		}
		// Gmail is held to 0 estate-wide by standing ruling; opening it
		// requires the ruling to be named in notes.
		if c.DailyMaxByISP["gmail"] > 0 && strings.TrimSpace(c.Notes) == "" {
			add("daily_max_by_isp", "gmail > 0 requires notes naming the operator ruling")
		}
	}

	startMin, err := parseClock(c.ActiveWindowStart)
	if err != nil {
		add("active_window_start", err.Error())
	}
	endMin, err2 := parseClock(c.ActiveWindowEnd)
	if err2 != nil {
		add("active_window_end", err2.Error())
	}
	if err == nil && err2 == nil && endMin <= startMin {
		add("active_window_end", "must be after active_window_start")
	}

	// bucket.go divides the daily max by the number of intervals in the
	// window, so a zero interval is a divide-by-zero, not a policy choice.
	if c.IntervalMinutes <= 0 {
		add("interval_minutes", "must be > 0")
	} else if c.IntervalMinutes > 1440 {
		add("interval_minutes", "must be <= 1440")
	}
	if c.MaxBurstIntervals < 1 {
		add("max_burst_intervals", "must be >= 1")
	}
	if c.RampSource != "" && c.RampSource != RampSourceCards && c.RampSource != RampSourceOperator {
		add("ramp_source", fmt.Sprintf("must be %q or %q", RampSourceCards, RampSourceOperator))
	}

	if len(errs) == 0 {
		return nil
	}
	return errs
}

// TokenBody is the domain contract's POLICY body — what the integrity token
// covers. Clock strings are normalised to HH:MM because PostgreSQL returns a
// `time` column as HH:MM:SS: without this the token issued at Schedule would
// never verify after the row round-trips through the database.
func (c *DomainContract) TokenBody() any {
	return map[string]any{
		"sending_domain":      c.SendingDomain,
		"brand_code":          c.BrandCode,
		"daily_max_by_isp":    normIntMap(c.DailyMaxByISP),
		"active_window_start": normClock(c.ActiveWindowStart),
		"active_window_end":   normClock(c.ActiveWindowEnd),
		"interval_minutes":    c.IntervalMinutes,
		"max_burst_intervals": c.MaxBurstIntervals,
		"ramp_source":         c.RampSource,
	}
}

func (c *DomainContract) insertSQL() string {
	return `INSERT INTO drip_domain_contracts
        (id, version, status, effective_at, created_by, created_at, change_ledger_id, notes,
         sending_domain, brand_code, daily_max_by_isp, active_window_start, active_window_end,
         interval_minutes, max_burst_intervals, ramp_source, metadata, token)
        VALUES ($1, $2, $3, $4, $5, NOW(), $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`
}

func (c *DomainContract) insertArgs(id uuid.UUID, version int) []any {
	raw, _ := json.Marshal(c.DailyMaxByISP)
	return []any{
		id, version, string(StatusDraft), nullTime(c.EffectiveAt), c.CreatedBy, c.ChangeLedgerID, c.Notes,
		c.SendingDomain, c.BrandCode, string(raw), c.ActiveWindowStart, c.ActiveWindowEnd,
		c.IntervalMinutes, c.MaxBurstIntervals, nullString(c.RampSource),
		c.Metadata, c.Token,
	}
}

// ---------------------------------------------------------------------------
// Dispatch contract
// ---------------------------------------------------------------------------

// Demand modes (§1.1 drip_dispatch_contracts.demand_mode).
const (
	DemandModeTarget           = "target"
	DemandModeConsumeAvailable = "consume_available"
)

// ExplorationTier is the operator tier reserved for tests (§1.1, §5.3).
const ExplorationTier = 9

// DispatchContract is the lane's demand statement: how many intros it wants per
// ISP, on which domains, under what ladder (§1.1 drip_dispatch_contracts).
type DispatchContract struct {
	Meta
	Lane                 string         `json:"lane"`
	OperatorPriorityTier int            `json:"operator_priority_tier"`
	DesiredDailyIntros   map[string]int `json:"desired_daily_intros"`
	DemandMode           string         `json:"demand_mode"`
	DailyCeiling         *int           `json:"daily_ceiling,omitempty"`
	AllowedDomains       []string       `json:"allowed_domains"`
	ISPExclusions        []string       `json:"isp_exclusions"`
	LadderTouches        int            `json:"ladder_touches"`
	LadderGapHours       int            `json:"ladder_gap_hours"`
	FollowupsCommitted   bool           `json:"followups_committed"`
	MaxIntroShare        float64        `json:"max_intro_share"`
	ExplorationShare     float64        `json:"exploration_share"`
}

func (c *DispatchContract) Kind() ContractKind { return KindDispatch }
func (c *DispatchContract) Subject() string    { return c.Lane }
func (c *DispatchContract) metaPtr() *Meta     { return &c.Meta }

// Validate implements §1.1 for the dispatch kind.
func (c *DispatchContract) Validate() error {
	var errs ValidationErrors
	add := func(field, msg string) {
		errs = append(errs, &ValidationError{Kind: KindDispatch, Subject: c.Lane, Field: field, Msg: msg})
	}

	if strings.TrimSpace(c.Lane) == "" {
		add("lane", "required")
	}
	if c.Status != "" && !c.Status.Valid() {
		add("status", fmt.Sprintf("unknown status %q", c.Status))
	}

	switch c.OperatorPriorityTier {
	case 1, 2, 3, ExplorationTier:
	default:
		add("operator_priority_tier", "must be 1, 2, 3 or 9 (test/exploration)")
	}

	// An absent ISP means "not wanted" (= 0); a key that is not an ISP class
	// is silently inert demand, which is the defect this rule exists to stop.
	if c.DesiredDailyIntros == nil {
		add("desired_daily_intros", "required (may be empty; absent ISP means 0)")
	} else {
		var unknown, negative []string
		for _, isp := range sortedKeys(c.DesiredDailyIntros) {
			if !IsISPClass(isp) {
				unknown = append(unknown, isp)
			}
			if c.DesiredDailyIntros[isp] < 0 {
				negative = append(negative, fmt.Sprintf("%s=%d", isp, c.DesiredDailyIntros[isp]))
			}
		}
		if len(unknown) > 0 {
			add("desired_daily_intros", "not an ISP class: "+strings.Join(unknown, ", "))
		}
		if len(negative) > 0 {
			add("desired_daily_intros", "must be >= 0: "+strings.Join(negative, ", "))
		}
	}

	switch c.DemandMode {
	case DemandModeTarget:
		// daily_ceiling is optional and ignored in target mode.
	case DemandModeConsumeAvailable:
		if c.DailyCeiling == nil {
			add("daily_ceiling", "required when demand_mode is consume_available")
		} else if *c.DailyCeiling <= 0 {
			add("daily_ceiling", "must be > 0 when demand_mode is consume_available")
		}
	default:
		add("demand_mode", fmt.Sprintf("must be %q or %q", DemandModeTarget, DemandModeConsumeAvailable))
	}

	if len(c.AllowedDomains) == 0 {
		add("allowed_domains", "required; a lane with no allowed domain can never be awarded capacity")
	} else if dup := firstDuplicate(c.AllowedDomains); dup != "" {
		add("allowed_domains", "duplicate entry: "+dup)
	}

	var badExcl []string
	for _, isp := range c.ISPExclusions {
		if !IsISPClass(isp) {
			badExcl = append(badExcl, isp)
		}
	}
	if len(badExcl) > 0 {
		sort.Strings(badExcl)
		add("isp_exclusions", "not an ISP class: "+strings.Join(badExcl, ", "))
	}

	if c.LadderTouches < 1 {
		add("ladder_touches", "must be >= 1")
	}
	if c.LadderGapHours < 1 {
		add("ladder_gap_hours", "must be >= 1")
	}

	// (0,1]: a lane must be able to take some intro capacity, and it may take
	// all of it, but "share" is never above 1.
	if !(c.MaxIntroShare > 0 && c.MaxIntroShare <= 1) {
		add("max_intro_share", "must be in (0, 1]")
	}
	// [0,1): exploration never consumes a whole domain's intro tokens.
	if !(c.ExplorationShare >= 0 && c.ExplorationShare < 1) {
		add("exploration_share", "must be in [0, 1)")
	}

	if len(errs) == 0 {
		return nil
	}
	return errs
}

// TokenBody is the dispatch contract's POLICY body (see DomainContract.TokenBody).
func (c *DispatchContract) TokenBody() any {
	return map[string]any{
		"lane":                   c.Lane,
		"operator_priority_tier": c.OperatorPriorityTier,
		"desired_daily_intros":   normIntMap(c.DesiredDailyIntros),
		"demand_mode":            c.DemandMode,
		"daily_ceiling":          normIntPtr(c.DailyCeiling),
		"allowed_domains":        normStrings(c.AllowedDomains),
		"isp_exclusions":         normStrings(c.ISPExclusions),
		"ladder_touches":         c.LadderTouches,
		"ladder_gap_hours":       c.LadderGapHours,
		"followups_committed":    c.FollowupsCommitted,
		"max_intro_share":        c.MaxIntroShare,
		"exploration_share":      c.ExplorationShare,
	}
}

func (c *DispatchContract) insertSQL() string {
	return `INSERT INTO drip_dispatch_contracts
        (id, version, status, effective_at, created_by, created_at, change_ledger_id, notes,
         lane, operator_priority_tier, desired_daily_intros, demand_mode, daily_ceiling,
         allowed_domains, isp_exclusions, ladder_touches, ladder_gap_hours, followups_committed,
         max_intro_share, exploration_share, metadata, token)
        VALUES ($1, $2, $3, $4, $5, NOW(), $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)`
}

func (c *DispatchContract) insertArgs(id uuid.UUID, version int) []any {
	raw, _ := json.Marshal(c.DesiredDailyIntros)
	var ceiling any
	if c.DailyCeiling != nil {
		ceiling = *c.DailyCeiling
	}
	return []any{
		id, version, string(StatusDraft), nullTime(c.EffectiveAt), c.CreatedBy, c.ChangeLedgerID, c.Notes,
		c.Lane, c.OperatorPriorityTier, string(raw), c.DemandMode, ceiling,
		pq.Array(c.AllowedDomains), pq.Array(c.ISPExclusions), c.LadderTouches, c.LadderGapHours,
		c.FollowupsCommitted, c.MaxIntroShare, c.ExplorationShare,
		c.Metadata, c.Token,
	}
}

// ---------------------------------------------------------------------------
// Inventory contract
// ---------------------------------------------------------------------------

// Remail modes (§1.1 drip_inventory_contracts.remail_mode).
const (
	RemailModeFullLadder  = "full_ladder"
	RemailModeSingleTouch = "single_touch"
)

// InventoryContract is the lane's replenishment policy. It carries NO sending
// target — it replenishes to the planner's awards (§1.1).
type InventoryContract struct {
	Meta
	Lane                string   `json:"lane"`
	AcceptedSources     []string `json:"accepted_sources"`
	VerdictValidDays    int      `json:"verdict_valid_days"`
	EOEnabled           bool     `json:"eo_enabled"`
	MaxDailyEOSpendUSD  float64  `json:"max_daily_eo_spend_usd"`
	MinEOOrder          int      `json:"min_eo_order"`
	MinCoverageHours    int      `json:"min_coverage_hours"`
	TargetCoverageHours int      `json:"target_coverage_hours"`
	MaxCoverageHours    int      `json:"max_coverage_hours"`
	RemailEnabled       bool     `json:"remail_enabled"`
	RemailAfterDays     int      `json:"remail_after_days"`
	RemailMode          string   `json:"remail_mode"`
	MaxRemailShare      float64  `json:"max_remail_share"`
}

func (c *InventoryContract) Kind() ContractKind { return KindInventory }
func (c *InventoryContract) Subject() string    { return c.Lane }
func (c *InventoryContract) metaPtr() *Meta     { return &c.Meta }

// Validate implements §1.1 for the inventory kind.
func (c *InventoryContract) Validate() error {
	var errs ValidationErrors
	add := func(field, msg string) {
		errs = append(errs, &ValidationError{Kind: KindInventory, Subject: c.Lane, Field: field, Msg: msg})
	}

	if strings.TrimSpace(c.Lane) == "" {
		add("lane", "required")
	}
	if c.Status != "" && !c.Status.Valid() {
		add("status", fmt.Sprintf("unknown status %q", c.Status))
	}

	if len(c.AcceptedSources) == 0 {
		add("accepted_sources", "required; a lane that accepts no source can never replenish")
	} else if dup := firstDuplicate(c.AcceptedSources); dup != "" {
		add("accepted_sources", "duplicate entry: "+dup)
	}

	if c.VerdictValidDays < 1 {
		add("verdict_valid_days", "must be >= 1")
	}
	if c.MaxDailyEOSpendUSD < 0 {
		add("max_daily_eo_spend_usd", "must be >= 0")
	}
	if c.MinEOOrder < 0 {
		add("min_eo_order", "must be >= 0")
	}

	// Coverage hours are a band: min <= target <= max.
	if c.MinCoverageHours < 0 {
		add("min_coverage_hours", "must be >= 0")
	}
	if c.TargetCoverageHours < 0 {
		add("target_coverage_hours", "must be >= 0")
	}
	if c.MaxCoverageHours < 0 {
		add("max_coverage_hours", "must be >= 0")
	}
	if c.MinCoverageHours > c.TargetCoverageHours {
		add("target_coverage_hours", "must be >= min_coverage_hours")
	}
	if c.TargetCoverageHours > c.MaxCoverageHours {
		add("max_coverage_hours", "must be >= target_coverage_hours")
	}

	if c.RemailAfterDays < 0 {
		add("remail_after_days", "must be >= 0")
	}
	if c.RemailMode != RemailModeFullLadder && c.RemailMode != RemailModeSingleTouch {
		add("remail_mode", fmt.Sprintf("must be %q or %q", RemailModeFullLadder, RemailModeSingleTouch))
	}
	if !(c.MaxRemailShare >= 0 && c.MaxRemailShare <= 1) {
		add("max_remail_share", "must be in [0, 1]")
	}

	if len(errs) == 0 {
		return nil
	}
	return errs
}

// TokenBody is the inventory contract's POLICY body (see DomainContract.TokenBody).
func (c *InventoryContract) TokenBody() any {
	return map[string]any{
		"lane":                   c.Lane,
		"accepted_sources":       normStrings(c.AcceptedSources),
		"verdict_valid_days":     c.VerdictValidDays,
		"eo_enabled":             c.EOEnabled,
		"max_daily_eo_spend_usd": c.MaxDailyEOSpendUSD,
		"min_eo_order":           c.MinEOOrder,
		"min_coverage_hours":     c.MinCoverageHours,
		"target_coverage_hours":  c.TargetCoverageHours,
		"max_coverage_hours":     c.MaxCoverageHours,
		"remail_enabled":         c.RemailEnabled,
		"remail_after_days":      c.RemailAfterDays,
		"remail_mode":            c.RemailMode,
		"max_remail_share":       c.MaxRemailShare,
	}
}

func (c *InventoryContract) insertSQL() string {
	return `INSERT INTO drip_inventory_contracts
        (id, version, status, effective_at, created_by, created_at, change_ledger_id, notes,
         lane, accepted_sources, verdict_valid_days, eo_enabled, max_daily_eo_spend_usd, min_eo_order,
         min_coverage_hours, target_coverage_hours, max_coverage_hours, remail_enabled,
         remail_after_days, remail_mode, max_remail_share, metadata, token)
        VALUES ($1, $2, $3, $4, $5, NOW(), $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)`
}

func (c *InventoryContract) insertArgs(id uuid.UUID, version int) []any {
	return []any{
		id, version, string(StatusDraft), nullTime(c.EffectiveAt), c.CreatedBy, c.ChangeLedgerID, c.Notes,
		c.Lane, pq.Array(c.AcceptedSources), c.VerdictValidDays, c.EOEnabled, c.MaxDailyEOSpendUSD, c.MinEOOrder,
		c.MinCoverageHours, c.TargetCoverageHours, c.MaxCoverageHours, c.RemailEnabled,
		c.RemailAfterDays, c.RemailMode, c.MaxRemailShare,
		c.Metadata, c.Token,
	}
}

// ---------------------------------------------------------------------------
// Source contract
// ---------------------------------------------------------------------------

// SourceContract describes what a supplier delivers (§1.1 drip_source_contracts).
type SourceContract struct {
	Meta
	SourceSlug          string   `json:"source_slug"`
	RecordClass         string   `json:"record_class"`
	EligibleISPs        []string `json:"eligible_isps"`
	MaxDailyIntake      *int     `json:"max_daily_intake,omitempty"`
	ArrivalCadence      string   `json:"arrival_cadence"`
	ValidatedOnArrival  bool     `json:"validated_on_arrival"`
	RecordMaxAgeDays    *int     `json:"record_max_age_days,omitempty"`
	UnitAcquisitionCost float64  `json:"unit_acquisition_cost"`
}

func (c *SourceContract) Kind() ContractKind { return KindSource }
func (c *SourceContract) Subject() string    { return c.SourceSlug }
func (c *SourceContract) metaPtr() *Meta     { return &c.Meta }

// Validate implements §1.1 for the source kind.
func (c *SourceContract) Validate() error {
	var errs ValidationErrors
	add := func(field, msg string) {
		errs = append(errs, &ValidationError{Kind: KindSource, Subject: c.SourceSlug, Field: field, Msg: msg})
	}

	if strings.TrimSpace(c.SourceSlug) == "" {
		add("source_slug", "required")
	}
	if strings.TrimSpace(c.RecordClass) == "" {
		add("record_class", "required")
	}
	if c.Status != "" && !c.Status.Valid() {
		add("status", fmt.Sprintf("unknown status %q", c.Status))
	}

	if len(c.EligibleISPs) == 0 {
		add("eligible_isps", "required; a source eligible for no ISP can never be drawn from")
	} else {
		var unknown []string
		for _, isp := range c.EligibleISPs {
			if !IsISPClass(isp) {
				unknown = append(unknown, isp)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			add("eligible_isps", "not an ISP class: "+strings.Join(unknown, ", "))
		}
		if dup := firstDuplicate(c.EligibleISPs); dup != "" {
			add("eligible_isps", "duplicate entry: "+dup)
		}
	}

	if c.MaxDailyIntake != nil && *c.MaxDailyIntake < 0 {
		add("max_daily_intake", "must be >= 0 when set")
	}
	if strings.TrimSpace(c.ArrivalCadence) == "" {
		add("arrival_cadence", "required")
	}
	if c.RecordMaxAgeDays != nil && *c.RecordMaxAgeDays < 0 {
		add("record_max_age_days", "must be >= 0 when set")
	}
	if c.UnitAcquisitionCost < 0 {
		add("unit_acquisition_cost", "must be >= 0")
	}

	if len(errs) == 0 {
		return nil
	}
	return errs
}

// TokenBody is the source contract's POLICY body (see DomainContract.TokenBody).
func (c *SourceContract) TokenBody() any {
	return map[string]any{
		"source_slug":           c.SourceSlug,
		"record_class":          c.RecordClass,
		"eligible_isps":         normStrings(c.EligibleISPs),
		"max_daily_intake":      normIntPtr(c.MaxDailyIntake),
		"arrival_cadence":       c.ArrivalCadence,
		"validated_on_arrival":  c.ValidatedOnArrival,
		"record_max_age_days":   normIntPtr(c.RecordMaxAgeDays),
		"unit_acquisition_cost": c.UnitAcquisitionCost,
	}
}

func (c *SourceContract) insertSQL() string {
	return `INSERT INTO drip_source_contracts
        (id, version, status, effective_at, created_by, created_at, change_ledger_id, notes,
         source_slug, record_class, eligible_isps, max_daily_intake, arrival_cadence,
         validated_on_arrival, record_max_age_days, unit_acquisition_cost, metadata, token)
        VALUES ($1, $2, $3, $4, $5, NOW(), $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`
}

func (c *SourceContract) insertArgs(id uuid.UUID, version int) []any {
	var intake, maxAge any
	if c.MaxDailyIntake != nil {
		intake = *c.MaxDailyIntake
	}
	if c.RecordMaxAgeDays != nil {
		maxAge = *c.RecordMaxAgeDays
	}
	return []any{
		id, version, string(StatusDraft), nullTime(c.EffectiveAt), c.CreatedBy, c.ChangeLedgerID, c.Notes,
		c.SourceSlug, c.RecordClass, pq.Array(c.EligibleISPs), intake, c.ArrivalCadence,
		c.ValidatedOnArrival, maxAge, c.UnitAcquisitionCost,
		c.Metadata, c.Token,
	}
}

// ---------------------------------------------------------------------------
// ActiveSet
// ---------------------------------------------------------------------------

// ActiveSet is the four active contract sets in force for one Denver day. It is
// a snapshot: the executor loads it once per tick and never mutates it.
type ActiveSet struct {
	Day      time.Time `json:"day"`
	LoadedAt time.Time `json:"loaded_at"`

	Domains       map[string]*DomainContract    `json:"domains"`
	Dispatches    map[string]*DispatchContract  `json:"dispatches"`
	Inventories   map[string]*InventoryContract `json:"inventories"`
	SourcesBySlug map[string]*SourceContract    `json:"sources"`
}

// Domain returns the active domain contract for a sending domain, or
// *ErrNoActiveContract. There is no default: the caller must skip and alert.
func (s *ActiveSet) Domain(sendingDomain string) (*DomainContract, error) {
	if s != nil {
		if c, ok := s.Domains[sendingDomain]; ok {
			return c, nil
		}
	}
	return nil, &ErrNoActiveContract{Kind: KindDomain, Subject: sendingDomain}
}

// Dispatch returns the active dispatch contract for a lane, or *ErrNoActiveContract.
func (s *ActiveSet) Dispatch(lane string) (*DispatchContract, error) {
	if s != nil {
		if c, ok := s.Dispatches[lane]; ok {
			return c, nil
		}
	}
	return nil, &ErrNoActiveContract{Kind: KindDispatch, Subject: lane}
}

// Inventory returns the active inventory contract for a lane, or *ErrNoActiveContract.
func (s *ActiveSet) Inventory(lane string) (*InventoryContract, error) {
	if s != nil {
		if c, ok := s.Inventories[lane]; ok {
			return c, nil
		}
	}
	return nil, &ErrNoActiveContract{Kind: KindInventory, Subject: lane}
}

// Source returns the active source contract for a supplier slug, or *ErrNoActiveContract.
func (s *ActiveSet) Source(slug string) (*SourceContract, error) {
	if s != nil {
		if c, ok := s.SourcesBySlug[slug]; ok {
			return c, nil
		}
	}
	return nil, &ErrNoActiveContract{Kind: KindSource, Subject: slug}
}

// ContractQueryer is the read surface LoadActive needs (*sql.DB, *sql.Tx,
// *sql.Conn). It is deliberately read-only: loading policy never writes.
type ContractQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// ContractRowQueryer is the single-row read surface (*sql.DB, *sql.Tx, *sql.Conn).
type ContractRowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ContractExecer can run a statement and read one row — the surface
// InsertDraft needs.
type ContractExecer interface {
	ContractRowQueryer
	ContractQueryer
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ContractTxBeginner opens a transaction (*sql.DB).
type ContractTxBeginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// dayWindow returns [start, end) of the Denver day in the location the caller
// anchored `day` to. Keeping the location on the argument means this package
// never loads tzdata and never guesses which day the caller meant.
func dayWindow(day time.Time) (time.Time, time.Time) {
	loc := day.Location()
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	return start, start.AddDate(0, 0, 1)
}

const activeWhere = ` WHERE status = 'active' AND effective_at < $1`

// LoadActive reads the four active contract sets in force for the given Denver
// day. A row is in force when it is `active` and became effective before the
// day ended; an active row with a NULL effective_at is deliberately invisible
// (fail closed) rather than silently assumed to apply.
func LoadActive(ctx context.Context, db ContractQueryer, day time.Time) (*ActiveSet, error) {
	key, err := contractmeta.KeyFromEnv()
	if err != nil {
		return nil, fmt.Errorf("dripsupply: load active contracts: %w", err)
	}
	return LoadActiveWithKey(ctx, db, day, key)
}

// LoadActiveWithKey is LoadActive with the HMAC key supplied instead of read
// from the environment. Callers that already hold the key (and tests) use this;
// everything else uses LoadActive.
func LoadActiveWithKey(ctx context.Context, db ContractQueryer, day time.Time, key []byte) (*ActiveSet, error) {
	_, dayEnd := dayWindow(day)

	set := &ActiveSet{
		Day:           day,
		LoadedAt:      time.Now().UTC(),
		Domains:       map[string]*DomainContract{},
		Dispatches:    map[string]*DispatchContract{},
		Inventories:   map[string]*InventoryContract{},
		SourcesBySlug: map[string]*SourceContract{},
	}

	if err := loadDomains(ctx, db, dayEnd, set); err != nil {
		return nil, err
	}
	if err := loadDispatches(ctx, db, dayEnd, set); err != nil {
		return nil, err
	}
	if err := loadInventories(ctx, db, dayEnd, set); err != nil {
		return nil, err
	}
	if err := loadSources(ctx, db, dayEnd, set); err != nil {
		return nil, err
	}

	// §1.5 rule 2: every active contract's token is verified against its body,
	// and ONE bad token fails the whole load. A partial set would let the
	// executor mail under the contracts that happened to verify while a
	// hand-edited one silently disappeared — the caller must see an outage and
	// alert, not a quietly smaller estate.
	for _, subject := range sortedMapKeys(set.Domains) {
		if err := VerifyContract(key, set.Domains[subject]); err != nil {
			return nil, err
		}
	}
	for _, subject := range sortedMapKeys(set.Dispatches) {
		if err := VerifyContract(key, set.Dispatches[subject]); err != nil {
			return nil, err
		}
	}
	for _, subject := range sortedMapKeys(set.Inventories) {
		if err := VerifyContract(key, set.Inventories[subject]); err != nil {
			return nil, err
		}
	}
	for _, subject := range sortedMapKeys(set.SourcesBySlug) {
		if err := VerifyContract(key, set.SourcesBySlug[subject]); err != nil {
			return nil, err
		}
	}
	return set, nil
}

// VerifyContract checks one contract's integrity token against its policy body
// and against the duplicated `token` column (§1.5). Any disagreement is an
// *ErrTokenMismatch.
func VerifyContract(key []byte, c Contract) error {
	m := c.metaPtr()
	canon := contractmeta.Canonical(c.TokenBody(), string(c.Kind()), c.Subject(), m.Version)
	if err := contractmeta.Verify(key, canon, m.Metadata.Token); err != nil {
		return &ErrTokenMismatch{Kind: c.Kind(), Subject: c.Subject(), Version: m.Version, Err: err}
	}
	// The token column duplicates metadata.token.value for indexing; if the two
	// disagree, one of them was edited by hand.
	if m.Token != m.Metadata.Token.Value {
		return &ErrTokenMismatch{
			Kind: c.Kind(), Subject: c.Subject(), Version: m.Version,
			Err: errors.New("token column does not match metadata.token.value"),
		}
	}
	return nil
}

func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

const domainSelectBody = `SELECT ` + metaCols + `, sending_domain, brand_code, daily_max_by_isp,
       active_window_start, active_window_end, interval_minutes, max_burst_intervals, ramp_source
  FROM drip_domain_contracts`

const domainSelect = domainSelectBody + activeWhere
const domainSelectOne = domainSelectBody + ` WHERE sending_domain = $1 AND version = $2`

func scanDomain(rows *sql.Rows) (*DomainContract, error) {
	var (
		ms   metaScan
		c    DomainContract
		raw  []byte
		ramp sql.NullString
	)
	dests := append(ms.dests(),
		&c.SendingDomain, &c.BrandCode, &raw,
		&c.ActiveWindowStart, &c.ActiveWindowEnd, &c.IntervalMinutes, &c.MaxBurstIntervals, &ramp)
	if err := rows.Scan(dests...); err != nil {
		return nil, fmt.Errorf("dripsupply: scan domain contract: %w", err)
	}
	ms.into(&c.Meta)
	c.RampSource = ramp.String
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c.DailyMaxByISP); err != nil {
			return nil, fmt.Errorf("dripsupply: decode daily_max_by_isp for %q: %w", c.SendingDomain, err)
		}
	}
	return &c, nil
}

func loadDomains(ctx context.Context, db ContractQueryer, dayEnd time.Time, set *ActiveSet) error {
	rows, err := db.QueryContext(ctx, domainSelect, dayEnd)
	if err != nil {
		return fmt.Errorf("dripsupply: load active domain contracts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanDomain(rows)
		if err != nil {
			return err
		}
		set.Domains[c.SendingDomain] = c
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("dripsupply: iterate domain contracts: %w", err)
	}
	return nil
}

const dispatchSelectBody = `SELECT ` + metaCols + `, lane, operator_priority_tier, desired_daily_intros,
       demand_mode, daily_ceiling, allowed_domains, isp_exclusions, ladder_touches, ladder_gap_hours,
       followups_committed, max_intro_share, exploration_share
  FROM drip_dispatch_contracts`

const dispatchSelect = dispatchSelectBody + activeWhere
const dispatchSelectOne = dispatchSelectBody + ` WHERE lane = $1 AND version = $2`

func scanDispatch(rows *sql.Rows) (*DispatchContract, error) {
	var (
		ms      metaScan
		c       DispatchContract
		raw     []byte
		ceiling sql.NullInt64
	)
	dests := append(ms.dests(),
		&c.Lane, &c.OperatorPriorityTier, &raw, &c.DemandMode, &ceiling,
		pq.Array(&c.AllowedDomains), pq.Array(&c.ISPExclusions),
		&c.LadderTouches, &c.LadderGapHours, &c.FollowupsCommitted,
		&c.MaxIntroShare, &c.ExplorationShare)
	if err := rows.Scan(dests...); err != nil {
		return nil, fmt.Errorf("dripsupply: scan dispatch contract: %w", err)
	}
	ms.into(&c.Meta)
	if ceiling.Valid {
		v := int(ceiling.Int64)
		c.DailyCeiling = &v
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c.DesiredDailyIntros); err != nil {
			return nil, fmt.Errorf("dripsupply: decode desired_daily_intros for %q: %w", c.Lane, err)
		}
	}
	return &c, nil
}

func loadDispatches(ctx context.Context, db ContractQueryer, dayEnd time.Time, set *ActiveSet) error {
	rows, err := db.QueryContext(ctx, dispatchSelect, dayEnd)
	if err != nil {
		return fmt.Errorf("dripsupply: load active dispatch contracts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanDispatch(rows)
		if err != nil {
			return err
		}
		set.Dispatches[c.Lane] = c
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("dripsupply: iterate dispatch contracts: %w", err)
	}
	return nil
}

const inventorySelectBody = `SELECT ` + metaCols + `, lane, accepted_sources, verdict_valid_days, eo_enabled,
       max_daily_eo_spend_usd, min_eo_order, min_coverage_hours, target_coverage_hours, max_coverage_hours,
       remail_enabled, remail_after_days, remail_mode, max_remail_share
  FROM drip_inventory_contracts`

const inventorySelect = inventorySelectBody + activeWhere
const inventorySelectOne = inventorySelectBody + ` WHERE lane = $1 AND version = $2`

func scanInventory(rows *sql.Rows) (*InventoryContract, error) {
	var (
		ms metaScan
		c  InventoryContract
	)
	dests := append(ms.dests(),
		&c.Lane, pq.Array(&c.AcceptedSources), &c.VerdictValidDays, &c.EOEnabled,
		&c.MaxDailyEOSpendUSD, &c.MinEOOrder, &c.MinCoverageHours, &c.TargetCoverageHours,
		&c.MaxCoverageHours, &c.RemailEnabled, &c.RemailAfterDays, &c.RemailMode, &c.MaxRemailShare)
	if err := rows.Scan(dests...); err != nil {
		return nil, fmt.Errorf("dripsupply: scan inventory contract: %w", err)
	}
	ms.into(&c.Meta)
	return &c, nil
}

func loadInventories(ctx context.Context, db ContractQueryer, dayEnd time.Time, set *ActiveSet) error {
	rows, err := db.QueryContext(ctx, inventorySelect, dayEnd)
	if err != nil {
		return fmt.Errorf("dripsupply: load active inventory contracts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanInventory(rows)
		if err != nil {
			return err
		}
		set.Inventories[c.Lane] = c
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("dripsupply: iterate inventory contracts: %w", err)
	}
	return nil
}

const sourceSelectBody = `SELECT ` + metaCols + `, source_slug, record_class, eligible_isps, max_daily_intake,
       arrival_cadence, validated_on_arrival, record_max_age_days, unit_acquisition_cost
  FROM drip_source_contracts`

const sourceSelect = sourceSelectBody + activeWhere
const sourceSelectOne = sourceSelectBody + ` WHERE source_slug = $1 AND version = $2`

func scanSource(rows *sql.Rows) (*SourceContract, error) {
	var (
		ms     metaScan
		c      SourceContract
		intake sql.NullInt64
		maxAge sql.NullInt64
	)
	dests := append(ms.dests(),
		&c.SourceSlug, &c.RecordClass, pq.Array(&c.EligibleISPs), &intake,
		&c.ArrivalCadence, &c.ValidatedOnArrival, &maxAge, &c.UnitAcquisitionCost)
	if err := rows.Scan(dests...); err != nil {
		return nil, fmt.Errorf("dripsupply: scan source contract: %w", err)
	}
	ms.into(&c.Meta)
	if intake.Valid {
		v := int(intake.Int64)
		c.MaxDailyIntake = &v
	}
	if maxAge.Valid {
		v := int(maxAge.Int64)
		c.RecordMaxAgeDays = &v
	}
	return &c, nil
}

func loadSources(ctx context.Context, db ContractQueryer, dayEnd time.Time, set *ActiveSet) error {
	rows, err := db.QueryContext(ctx, sourceSelect, dayEnd)
	if err != nil {
		return fmt.Errorf("dripsupply: load active source contracts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanSource(rows)
		if err != nil {
			return err
		}
		set.SourcesBySlug[c.SourceSlug] = c
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("dripsupply: iterate source contracts: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Activation
// ---------------------------------------------------------------------------

// ActivationResult counts what ActivateScheduled did, per kind. It is the
// evidence the caller logs; an all-zero result is the normal steady state.
type ActivationResult struct {
	Activated    map[ContractKind]int `json:"activated"`
	Superseded   map[ContractKind]int `json:"superseded"`
	SkippedStale map[ContractKind]int `json:"skipped_stale"`
}

// Total returns the number of rows promoted to active across all kinds.
func (r ActivationResult) Total() int {
	n := 0
	for _, v := range r.Activated {
		n += v
	}
	return n
}

// supersedeStaleSQL retires every DUE scheduled row that is not the newest for
// its subject, so the promote step can never produce two actives. `DISTINCT ON`
// picks the newest effective_at, then the highest version, per subject.
func supersedeStaleSQL(spec kindSpec) string {
	return fmt.Sprintf(`UPDATE %[1]s SET status = 'superseded', superseded_at = $1
 WHERE status = 'scheduled' AND effective_at <= $1
   AND id NOT IN (
       SELECT DISTINCT ON (%[2]s) id FROM %[1]s
        WHERE status = 'scheduled' AND effective_at <= $1
        ORDER BY %[2]s, effective_at DESC, version DESC)`, spec.Table, spec.SubjectCol)
}

// supersedeActiveSQL retires the outgoing active row for every subject that has
// a due replacement. It MUST run before the promote statement: the partial
// unique index is checked per row, so the incoming row cannot be inserted into
// the index while the outgoing one is still in it.
func supersedeActiveSQL(spec kindSpec) string {
	return fmt.Sprintf(`UPDATE %[1]s SET status = 'superseded', superseded_at = $1
 WHERE status = 'active'
   AND %[2]s IN (SELECT %[2]s FROM %[1]s WHERE status = 'scheduled' AND effective_at <= $1)`,
		spec.Table, spec.SubjectCol)
}

// promoteSQL flips the remaining due scheduled rows to active.
func promoteSQL(spec kindSpec) string {
	return fmt.Sprintf(`UPDATE %s SET status = 'active'
 WHERE status = 'scheduled' AND effective_at <= $1`, spec.Table)
}

// ActivateScheduled advances the contract lifecycle at a day boundary, for all
// four kinds, in ONE transaction:
//
//  1. every DUE scheduled row that is not the newest for its subject -> superseded
//  2. the currently active row of every subject with a due replacement -> superseded
//  3. the remaining due scheduled rows -> active
//
// Order matters: (2) before (3) keeps the partial unique index satisfied at
// every instant. The whole thing is idempotent — a second run finds no due
// scheduled rows and updates nothing — so it is safe on every tick.
func ActivateScheduled(ctx context.Context, db ContractTxBeginner, now time.Time) (ActivationResult, error) {
	res := ActivationResult{
		Activated:    map[ContractKind]int{},
		Superseded:   map[ContractKind]int{},
		SkippedStale: map[ContractKind]int{},
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("dripsupply: begin activation tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for _, kind := range AllKinds() {
		spec := kindSpecs[kind]

		stale, err := execCount(ctx, tx, supersedeStaleSQL(spec), now)
		if err != nil {
			return res, wrapActivation(kind, spec, "supersede stale scheduled", err)
		}
		res.SkippedStale[kind] = stale

		sup, err := execCount(ctx, tx, supersedeActiveSQL(spec), now)
		if err != nil {
			return res, wrapActivation(kind, spec, "supersede outgoing active", err)
		}
		res.Superseded[kind] = sup

		act, err := execCount(ctx, tx, promoteSQL(spec), now)
		if err != nil {
			return res, wrapActivation(kind, spec, "promote scheduled to active", err)
		}
		res.Activated[kind] = act
	}

	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("dripsupply: commit activation: %w", err)
	}
	committed = true
	return res, nil
}

func execCount(ctx context.Context, tx *sql.Tx, query string, arg any) (int, error) {
	r, err := tx.ExecContext(ctx, query, arg)
	if err != nil {
		return 0, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		// Drivers that cannot report this are not a failure of the write.
		return 0, nil
	}
	return int(n), nil
}

// wrapActivation turns a unique violation on the kind's partial index into a
// typed *ErrDuplicateActive so the caller can alert on "two actives for one
// subject" specifically, instead of a generic SQL error.
func wrapActivation(kind ContractKind, spec kindSpec, step string, err error) error {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == pgUniqueViolation {
		if pqErr.Constraint == spec.ActiveIdx || strings.Contains(pqErr.Message, spec.ActiveIdx) {
			return &ErrDuplicateActive{Kind: kind, IndexName: spec.ActiveIdx, Err: err}
		}
	}
	return fmt.Errorf("dripsupply: %s (%s): %w", step, kind, err)
}

const pgUniqueViolation = "23505"

// ---------------------------------------------------------------------------
// Version helpers
// ---------------------------------------------------------------------------

// NextVersion returns the next version number for a subject: MAX(version)+1
// across every row of that subject regardless of status, so a superseded or
// rejected version number is never reused.
func NextVersion(ctx context.Context, tx ContractRowQueryer, kind ContractKind, subject string) (int, error) {
	spec, ok := kindSpecs[kind]
	if !ok {
		return 0, fmt.Errorf("dripsupply: unknown contract kind %q", kind)
	}
	if strings.TrimSpace(subject) == "" {
		return 0, fmt.Errorf("dripsupply: next version for %s: empty subject", kind)
	}
	q := fmt.Sprintf(`SELECT COALESCE(MAX(version), 0) + 1 FROM %s WHERE %s = $1`, spec.Table, spec.SubjectCol)
	var v int
	if err := tx.QueryRowContext(ctx, q, subject).Scan(&v); err != nil {
		return 0, fmt.Errorf("dripsupply: next version for %s %q: %w", kind, subject, err)
	}
	return v, nil
}

// InsertDraft validates a contract and writes it as a new `draft` row at the
// next version for its subject. It never writes an invalid contract and never
// writes without an audit trail (created_by + change_ledger_id), which is what
// makes the portal handler unable to persist something the mediators would
// then have to guess about.
func InsertDraft(ctx context.Context, tx ContractExecer, c Contract) (uuid.UUID, int, error) {
	if c == nil {
		return uuid.Nil, 0, errors.New("dripsupply: insert draft: nil contract")
	}
	if err := c.Validate(); err != nil {
		return uuid.Nil, 0, fmt.Errorf("dripsupply: insert draft rejected: %w", err)
	}
	m := c.metaPtr()
	if strings.TrimSpace(m.CreatedBy) == "" {
		return uuid.Nil, 0, fmt.Errorf("dripsupply: insert draft for %s %q: created_by is required", c.Kind(), c.Subject())
	}
	if strings.TrimSpace(m.ChangeLedgerID) == "" {
		return uuid.Nil, 0, fmt.Errorf("dripsupply: insert draft for %s %q: change_ledger_id is required", c.Kind(), c.Subject())
	}

	version, err := NextVersion(ctx, tx, c.Kind(), c.Subject())
	if err != nil {
		return uuid.Nil, 0, err
	}
	// §1.5 rule 4: mutation is written on every version and mirrors the change
	// ledger id. Refs the caller resolved (contractmeta.ResolveRefs) are kept;
	// identity and mutation are stamped here so no caller can forget them.
	// No token: a draft has none — it is issued at approved -> scheduled.
	id := uuid.New()
	m.Metadata.StampIdentity(id.String(), string(c.Kind()), version)
	m.Metadata.StampMutation(time.Now().UTC(), m.CreatedBy, m.ChangeLedgerID, version-1)
	m.Metadata.Token = contractmeta.Token{}
	m.Token = ""

	if _, err := tx.ExecContext(ctx, c.insertSQL(), c.insertArgs(id, version)...); err != nil {
		return uuid.Nil, 0, fmt.Errorf("dripsupply: insert %s draft for %q: %w", c.Kind(), c.Subject(), err)
	}
	m.ID = id
	m.Version = version
	m.Status = StatusDraft
	return id, version, nil
}

// ---------------------------------------------------------------------------
// Scheduling + token issue
// ---------------------------------------------------------------------------

// ErrContractNotFound means no row exists at that (kind, subject, version).
type ErrContractNotFound struct {
	Kind    ContractKind
	Subject string
	Version int
}

func (e *ErrContractNotFound) Error() string {
	return fmt.Sprintf("dripsupply: no %s contract %q at version %d", e.Kind, e.Subject, e.Version)
}

func selectOneFor(kind ContractKind) (string, error) {
	switch kind {
	case KindDomain:
		return domainSelectOne, nil
	case KindDispatch:
		return dispatchSelectOne, nil
	case KindInventory:
		return inventorySelectOne, nil
	case KindSource:
		return sourceSelectOne, nil
	}
	return "", fmt.Errorf("dripsupply: unknown contract kind %q", kind)
}

// LoadOne reads exactly one contract row by (kind, subject, version), whatever
// its status. Unlike LoadActive it does NOT verify the token — it is the reader
// Schedule uses to compute one.
func LoadOne(ctx context.Context, db ContractQueryer, kind ContractKind, subject string, version int) (Contract, error) {
	q, err := selectOneFor(kind)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, q, subject, version)
	if err != nil {
		return nil, fmt.Errorf("dripsupply: load %s contract %q v%d: %w", kind, subject, version, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("dripsupply: load %s contract %q v%d: %w", kind, subject, version, err)
		}
		return nil, &ErrContractNotFound{Kind: kind, Subject: subject, Version: version}
	}
	switch kind {
	case KindDomain:
		return scanDomain(rows)
	case KindDispatch:
		return scanDispatch(rows)
	case KindInventory:
		return scanInventory(rows)
	case KindSource:
		return scanSource(rows)
	}
	return nil, fmt.Errorf("dripsupply: unknown contract kind %q", kind)
}

func scheduleSQL(spec kindSpec) string {
	return fmt.Sprintf(`UPDATE %s SET status = 'scheduled', metadata = $1, token = $2
 WHERE id = $3 AND status = 'approved'`, spec.Table)
}

// Schedule moves an `approved` contract to `scheduled` and issues its integrity
// token in the same statement (§1.5 rule 1: a contract is issued a token ONLY
// on this transition).
//
// The token is HMAC-SHA256 over Canonical(policy body, kind, subject, version)
// — see Contract.TokenBody for why the lifecycle fields are excluded. It is
// written to both `metadata.token` and the duplicated `token` column, and
// LoadActive later refuses any active contract where those disagree or where
// the MAC does not match the body.
//
// Exactly-once: the UPDATE is guarded on `status = 'approved'`, so a second
// Schedule of the same version finds `scheduled` and returns *ErrNotApproved
// rather than re-stamping with a fresh issued_at. Fail-closed: with no key
// nothing is issued and nothing moves.
func Schedule(ctx context.Context, tx ContractExecer, kind ContractKind, subject string, version int, key []byte, now time.Time) (contractmeta.Token, error) {
	spec, ok := kindSpecs[kind]
	if !ok {
		return contractmeta.Token{}, fmt.Errorf("dripsupply: unknown contract kind %q", kind)
	}
	if len(key) == 0 {
		return contractmeta.Token{}, fmt.Errorf("dripsupply: schedule %s %q v%d: %w", kind, subject, version, contractmeta.ErrNoKey)
	}

	c, err := LoadOne(ctx, tx, kind, subject, version)
	if err != nil {
		return contractmeta.Token{}, err
	}
	m := c.metaPtr()
	if m.Status != StatusApproved {
		return contractmeta.Token{}, &ErrNotApproved{Kind: kind, Subject: subject, Version: version, Status: m.Status}
	}
	// A contract that cannot pass its own validation is never given a token —
	// otherwise an invalid row becomes permanently "verified".
	if err := c.Validate(); err != nil {
		return contractmeta.Token{}, fmt.Errorf("dripsupply: refusing to schedule invalid %s contract %q v%d: %w", kind, subject, version, err)
	}

	canon := contractmeta.Canonical(c.TokenBody(), string(kind), subject, version)
	tok := contractmeta.Issue(key, canon, now)
	if !tok.Issued() {
		return contractmeta.Token{}, fmt.Errorf("dripsupply: schedule %s %q v%d: %w", kind, subject, version, contractmeta.ErrNoKey)
	}

	blk := m.Metadata
	blk.StampIdentity(m.ID.String(), string(kind), version)
	blk.Token = tok

	res, err := tx.ExecContext(ctx, scheduleSQL(spec), blk, tok.Value, m.ID)
	if err != nil {
		return contractmeta.Token{}, fmt.Errorf("dripsupply: schedule %s contract %q v%d: %w", kind, subject, version, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// Lost the race: another writer moved it out of `approved`.
		return contractmeta.Token{}, &ErrNotApproved{Kind: kind, Subject: subject, Version: version, Status: m.Status}
	}

	m.Status = StatusScheduled
	m.Metadata = blk
	m.Token = tok.Value
	return tok, nil
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// parseClock accepts "HH:MM" or "HH:MM:SS" and returns minutes since midnight.
func parseClock(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("required (HH:MM)")
	}
	parts := strings.Split(s, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, fmt.Errorf("must be HH:MM, got %q", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("hour out of range in %q", s)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("minute out of range in %q", s)
	}
	if len(parts) == 3 {
		sec, err := strconv.Atoi(parts[2])
		if err != nil || sec < 0 || sec > 59 {
			return 0, fmt.Errorf("second out of range in %q", s)
		}
	}
	return h*60 + m, nil
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func firstDuplicate(vals []string) string {
	seen := make(map[string]struct{}, len(vals))
	for _, v := range vals {
		if _, ok := seen[v]; ok {
			return v
		}
		seen[v] = struct{}{}
	}
	return ""
}

// normClock renders a clock string as HH:MM so "01:00" and "01:00:00" — the
// form written and the form PostgreSQL returns — canonicalise identically.
func normClock(s string) string {
	mins, err := parseClock(s)
	if err != nil {
		return strings.TrimSpace(s)
	}
	return fmt.Sprintf("%02d:%02d", mins/60, mins%60)
}

// normStrings makes a nil slice and an empty slice canonicalise identically
// (PostgreSQL returns '{}' as an empty non-nil slice; a Go zero value is nil).
func normStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func normIntMap(v map[string]int) map[string]int {
	if v == nil {
		return map[string]int{}
	}
	return v
}

func normIntPtr(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
