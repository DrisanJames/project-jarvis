package segmentation

// criteria_v2.go — AUDIENCE UNIFICATION Phase 2 (docs/AUDIENCE_UNIFICATION.md).
//
// The canonical criteria-as-data shape, stored in mailing_segments.conditions
// under a top-level "v2" key:
//
//	{"v2": {
//	  "lists":       ["<list uuid>", ...],
//	  "performance": {"opened_within_days": 30, "clicked_within_days": 60, "op": "or"},
//	  "refinements": [{"field": "isp", "op": "eq", "value": "gmail"}, ...]
//	}}
//
// A v2 spec compiles to ONE SELECT over mailing_subscribers using only
// indexed columns: list_id, status, last_open_at / last_click_at (the
// idx_subscribers_list_last_open/click partial indexes in cmd/server/main.go
// concurrentIndexSpecs — their predicate `status IN ('active','confirmed')`
// is emitted verbatim below so the planner can prove implication), isp,
// created_at, split_part(email,'@',2) for email_domain, and
// first_name/last_name IS NOT NULL checks.
//
// Detection contract (mirrors parseLakeSpec in internal/api):
//   - ParseV2Criteria returns (nil, nil) for anything that is NOT a
//     {"v2":...} payload — legacy arrays, ConditionGroupBuilder objects and
//     lake_spec blobs all pass through untouched.
//   - It returns (nil, error) when the "v2" key IS present but the spec is
//     malformed or fails validation — callers must fail CLOSED (empty set /
//     rejected build), never fall through to the legacy parser, whose
//     discard-the-error path degrades to an unscoped org-wide query.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// V2Performance is the engagement-recency block. Nil pointers mean "no
// constraint on that signal"; Op joins the two windows when both are set
// ("and" default, "or" allowed).
type V2Performance struct {
	OpenedWithinDays  *int   `json:"opened_within_days"`
	ClickedWithinDays *int   `json:"clicked_within_days"`
	Op                string `json:"op"`
}

// V2Refinement is one chainable filter over an indexed subscriber column.
type V2Refinement struct {
	Field string      `json:"field"`
	Op    string      `json:"op"`
	Value interface{} `json:"value"`
}

// V2Criteria is the parsed canonical shape.
type V2Criteria struct {
	Lists       []uuid.UUID    `json:"lists"`
	Performance *V2Performance `json:"performance"`
	Refinements []V2Refinement `json:"refinements"`
}

// v2MaxWindowDays bounds the performance recency windows (10 years — the
// point is rejecting garbage like 0/negative/1e9, not policing strategy).
const v2MaxWindowDays = 3650

// v2CountCap is the LIMIT applied inside CompileV2CountSQL's subquery so a
// preview COUNT can never scan past this many matching rows.
const v2CountCap = 2000000

// v2RefinementFields → allowed ops per field. created_after/created_before
// and has_first_name/has_last_name accept only "eq" (the op carries no
// meaning there; the field name itself is the operator).
var v2RefinementFields = map[string]map[string]bool{
	"isp":            {"eq": true, "neq": true, "in": true, "contains": true},
	"email_domain":   {"eq": true, "neq": true, "in": true, "contains": true},
	"status":         {"eq": true, "neq": true, "in": true},
	"source_system":  {"eq": true, "neq": true, "in": true, "contains": true},
	"created_after":  {"eq": true},
	"created_before": {"eq": true},
	"has_first_name": {"eq": true},
	"has_last_name":  {"eq": true},
}

// v2TextColumns maps text refinement fields to their SQL expression.
// email_domain lowercases so mixed-case stored emails still match.
var v2TextColumns = map[string]string{
	"isp":           "s.isp",
	"email_domain":  "split_part(LOWER(s.email), '@', 2)",
	"status":        "s.status",
	"source_system": "s.source_system",
}

// v2LowercasedFields are text fields whose comparison values are normalized
// to lowercase (isp and email domains are stored lowercase).
var v2LowercasedFields = map[string]bool{"isp": true, "email_domain": true}

// ParseV2Criteria extracts and validates a v2 criteria block from a raw
// mailing_segments.conditions value. See the detection contract above.
func ParseV2Criteria(conditionsRaw string) (*V2Criteria, error) {
	raw := strings.TrimSpace(conditionsRaw)
	if raw == "" || raw[0] != '{' {
		return nil, nil
	}
	var probe struct {
		V2 json.RawMessage `json:"v2"`
	}
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return nil, nil // not even JSON object shaped — legacy path's problem
	}
	trimmed := bytes.TrimSpace(probe.V2)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil // no v2 key
	}
	var c V2Criteria
	if err := json.Unmarshal(trimmed, &c); err != nil {
		return nil, fmt.Errorf("invalid v2 criteria: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid v2 criteria: %w", err)
	}
	return &c, nil
}

// Validate rejects empty and malformed specs. An empty v2 object would
// otherwise compile to "every active subscriber" — never allowed implicitly.
func (c *V2Criteria) Validate() error {
	hasPerf := c.Performance != nil &&
		(c.Performance.OpenedWithinDays != nil || c.Performance.ClickedWithinDays != nil)
	if len(c.Lists) == 0 && !hasPerf && len(c.Refinements) == 0 {
		return fmt.Errorf("empty criteria: at least one of lists, performance, refinements is required")
	}
	for _, id := range c.Lists {
		if id == uuid.Nil {
			return fmt.Errorf("lists: nil UUID not allowed")
		}
	}
	if p := c.Performance; p != nil {
		switch p.Op {
		case "", "and", "or":
		default:
			return fmt.Errorf("performance.op must be \"and\" or \"or\", got %q", p.Op)
		}
		for name, w := range map[string]*int{
			"opened_within_days":  p.OpenedWithinDays,
			"clicked_within_days": p.ClickedWithinDays,
		} {
			if w != nil && (*w < 1 || *w > v2MaxWindowDays) {
				return fmt.Errorf("performance.%s must be in [1,%d], got %d", name, v2MaxWindowDays, *w)
			}
		}
	}
	for i, r := range c.Refinements {
		ops, ok := v2RefinementFields[r.Field]
		if !ok {
			return fmt.Errorf("refinements[%d]: unknown field %q", i, r.Field)
		}
		op := r.Op
		if op == "" {
			op = "eq"
		}
		if !ops[op] {
			return fmt.Errorf("refinements[%d]: op %q not allowed for field %q", i, r.Op, r.Field)
		}
		if _, err := v2RefinementValue(r); err != nil {
			return fmt.Errorf("refinements[%d]: %w", i, err)
		}
	}
	return nil
}

// v2RefinementValue type-checks (and normalizes) a refinement's value for
// its field/op. Returned value types: string, []string, bool, or time.Time.
func v2RefinementValue(r V2Refinement) (interface{}, error) {
	op := r.Op
	if op == "" {
		op = "eq"
	}
	switch r.Field {
	case "has_first_name", "has_last_name":
		b, ok := r.Value.(bool)
		if !ok {
			return nil, fmt.Errorf("field %q requires a boolean value", r.Field)
		}
		return b, nil
	case "created_after", "created_before":
		s, ok := r.Value.(string)
		if !ok {
			return nil, fmt.Errorf("field %q requires a date string (YYYY-MM-DD or RFC3339)", r.Field)
		}
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC(), nil
			}
		}
		return nil, fmt.Errorf("field %q: cannot parse %q as YYYY-MM-DD or RFC3339", r.Field, s)
	}
	// Text fields.
	lower := v2LowercasedFields[r.Field]
	norm := func(s string) string {
		s = strings.TrimSpace(s)
		if lower {
			s = strings.ToLower(s)
		}
		return s
	}
	if op == "in" {
		rawList, ok := r.Value.([]interface{})
		if !ok || len(rawList) == 0 {
			return nil, fmt.Errorf("op \"in\" requires a non-empty string array value")
		}
		out := make([]string, 0, len(rawList))
		for _, v := range rawList {
			s, ok := v.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return nil, fmt.Errorf("op \"in\" requires non-empty string entries")
			}
			out = append(out, norm(s))
		}
		return out, nil
	}
	s, ok := r.Value.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("field %q requires a non-empty string value", r.Field)
	}
	return norm(s), nil
}

// hasStatusRefinement reports whether the operator constrained status
// explicitly (which replaces the default active/confirmed filter).
func (c *V2Criteria) hasStatusRefinement() bool {
	for _, r := range c.Refinements {
		if r.Field == "status" {
			return true
		}
	}
	return false
}

// CompileV2SQL compiles a validated spec to one SELECT returning
// (id::text, email) — the same contract as buildSegmentQuery so the
// materializer's INSERT..SELECT wrapper works unchanged. Deterministic
// clause order: lists → default status → performance → refinements.
func CompileV2SQL(c *V2Criteria) (string, []interface{}, error) {
	if c == nil {
		return "", nil, fmt.Errorf("nil v2 criteria")
	}
	if err := c.Validate(); err != nil {
		return "", nil, err
	}

	var where []string
	var args []interface{}
	arg := func(v interface{}) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	if len(c.Lists) > 0 {
		ids := make([]string, len(c.Lists))
		for i, u := range c.Lists {
			ids[i] = u.String()
		}
		where = append(where, "s.list_id = ANY("+arg(pq.Array(ids))+"::uuid[])")
	}

	// Default liveness filter — emitted VERBATIM as the partial-index
	// predicate (idx_subscribers_list_last_open/click) so the planner can
	// use those indexes. Skipped when the operator refines status directly.
	if !c.hasStatusRefinement() {
		where = append(where, "s.status IN ('active','confirmed')")
	}

	if p := c.Performance; p != nil {
		var perf []string
		if p.OpenedWithinDays != nil {
			perf = append(perf, "s.last_open_at >= NOW() - ("+arg(*p.OpenedWithinDays)+" * INTERVAL '1 day')")
		}
		if p.ClickedWithinDays != nil {
			perf = append(perf, "s.last_click_at >= NOW() - ("+arg(*p.ClickedWithinDays)+" * INTERVAL '1 day')")
		}
		switch len(perf) {
		case 1:
			where = append(where, perf[0])
		case 2:
			joiner := " AND "
			if p.Op == "or" {
				joiner = " OR "
			}
			where = append(where, "("+strings.Join(perf, joiner)+")")
		}
	}

	for i, r := range c.Refinements {
		frag, err := compileV2Refinement(r, arg)
		if err != nil {
			return "", nil, fmt.Errorf("refinements[%d]: %w", i, err)
		}
		where = append(where, frag)
	}

	query := "SELECT s.id::text, s.email FROM mailing_subscribers s WHERE " +
		strings.Join(where, " AND ")
	return query, args, nil
}

// compileV2Refinement renders one refinement as a WHERE fragment, binding
// values through arg().
func compileV2Refinement(r V2Refinement, arg func(interface{}) string) (string, error) {
	val, err := v2RefinementValue(r)
	if err != nil {
		return "", err
	}
	op := r.Op
	if op == "" {
		op = "eq"
	}

	switch r.Field {
	case "has_first_name", "has_last_name":
		col := "s.first_name"
		if r.Field == "has_last_name" {
			col = "s.last_name"
		}
		if val.(bool) {
			return "(" + col + " IS NOT NULL AND " + col + " <> '')", nil
		}
		return "(" + col + " IS NULL OR " + col + " = '')", nil
	case "created_after":
		return "s.created_at > " + arg(val.(time.Time)), nil
	case "created_before":
		return "s.created_at < " + arg(val.(time.Time)), nil
	}

	col, ok := v2TextColumns[r.Field]
	if !ok {
		return "", fmt.Errorf("unknown field %q", r.Field)
	}
	switch op {
	case "eq":
		return col + " = " + arg(val.(string)), nil
	case "neq":
		return col + " <> " + arg(val.(string)), nil
	case "in":
		return col + " = ANY(" + arg(pq.Array(val.([]string))) + "::text[])", nil
	case "contains":
		return col + " ILIKE " + arg("%"+escapeLikePattern(val.(string))+"%"), nil
	}
	return "", fmt.Errorf("unknown op %q", op)
}

// CompileV2CountSQL wraps the compiled SELECT in a LIMIT-capped COUNT so a
// preview can never scan unbounded rows (v2CountCap ceiling; callers add
// their own context timeout).
func CompileV2CountSQL(c *V2Criteria) (string, []interface{}, error) {
	q, args, err := CompileV2SQL(c)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("SELECT COUNT(*) FROM (%s LIMIT %d) q", q, v2CountCap), args, nil
}

// SummarizeV2 renders a compact human-readable definition line for catalog
// screens ("2 lists · opened ≤30d or clicked ≤60d · isp = gmail").
func SummarizeV2(c *V2Criteria) string {
	if c == nil {
		return ""
	}
	parts := make([]string, 0, 4)
	if n := len(c.Lists); n == 1 {
		parts = append(parts, "1 list")
	} else if n > 1 {
		parts = append(parts, fmt.Sprintf("%d lists", n))
	}
	if p := c.Performance; p != nil {
		var perf []string
		if p.OpenedWithinDays != nil {
			perf = append(perf, fmt.Sprintf("opened ≤%dd", *p.OpenedWithinDays))
		}
		if p.ClickedWithinDays != nil {
			perf = append(perf, fmt.Sprintf("clicked ≤%dd", *p.ClickedWithinDays))
		}
		joiner := " and "
		if p.Op == "or" {
			joiner = " or "
		}
		if len(perf) > 0 {
			parts = append(parts, strings.Join(perf, joiner))
		}
	}
	for _, r := range c.Refinements {
		op := r.Op
		if op == "" {
			op = "eq"
		}
		switch r.Field {
		case "has_first_name", "has_last_name":
			label := strings.TrimPrefix(r.Field, "has_")
			if b, ok := r.Value.(bool); ok && !b {
				parts = append(parts, "no "+label)
			} else {
				parts = append(parts, "has "+label)
			}
		case "created_after", "created_before":
			parts = append(parts, fmt.Sprintf("%s %v", strings.ReplaceAll(r.Field, "_", " "), r.Value))
		default:
			sym := map[string]string{"eq": "=", "neq": "≠", "in": "in", "contains": "~"}[op]
			parts = append(parts, fmt.Sprintf("%s %s %v", r.Field, sym, r.Value))
		}
	}
	if len(parts) == 0 {
		return "criteria-v2"
	}
	return strings.Join(parts, " · ")
}
