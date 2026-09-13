package brain

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LakeSQL runs a read-only Athena query and returns column headers + string
// rows. Injected so the runner is testable without Athena and fails soft when
// the lake reader is disabled.
type LakeSQL func(ctx context.Context, sql string) (cols []string, rows [][]string, err error)

// Runner executes evals. Shared by the API's "run now" and the daily worker so
// both record identical run rows.
type Runner struct {
	db          *sql.DB
	store       *Store
	lake        LakeSQL
	httpBase    string // e.g. http://127.0.0.1:8080 — the server calling itself
	adminKey    string
	codeVersion string
	client      *http.Client
}

func NewRunner(db *sql.DB, store *Store, lake LakeSQL, httpBase, adminKey, codeVersion string) *Runner {
	return &Runner{db: db, store: store, lake: lake, httpBase: strings.TrimRight(httpBase, "/"),
		adminKey: adminKey, codeVersion: codeVersion, client: &http.Client{Timeout: 120 * time.Second}}
}

// Run executes one eval, records the run, and returns it. Never panics on a
// bad spec: the run is recorded as failed with the error.
func (r *Runner) Run(ctx context.Context, e Eval) EvalRun {
	start := time.Now()
	run := EvalRun{EvalID: e.ID, CodeVersion: r.codeVersion}
	actual, err := r.execute(ctx, e)
	if err != nil {
		run.Pass, run.Error = false, err.Error()
		run.Result = map[string]any{"error": err.Error()}
	} else {
		pass, diffs := Compare(e.Checker, e.Expected, actual, e.TolerancePct)
		run.Pass = pass
		run.Result = map[string]any{"actual": actual, "diffs": diffs, "as_of": e.AsOf}
	}
	run.DurationMs = time.Since(start).Milliseconds()
	saved, serr := r.store.RecordEvalRun(ctx, run)
	if serr != nil {
		run.Error = strings.TrimSpace(run.Error + "; record: " + serr.Error())
		return run
	}
	return saved
}

func (r *Runner) execute(ctx context.Context, e Eval) (any, error) {
	switch e.Checker {
	case "pg_sql":
		q, _ := e.Spec["query"].(string)
		if err := AssertReadOnlySQL(q); err != nil {
			return nil, err
		}
		return r.runPG(ctx, q)
	case "lake_sql":
		q, _ := e.Spec["query"].(string)
		if err := AssertReadOnlySQL(q); err != nil {
			return nil, err
		}
		if r.lake == nil {
			return nil, fmt.Errorf("lake reader disabled")
		}
		cols, rows, err := r.lake(ctx, q)
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			m := map[string]any{}
			for i, c := range cols {
				if i < len(row) {
					m[c] = row[i]
				}
			}
			out = append(out, m)
		}
		return out, nil
	case "http":
		return r.runHTTP(ctx, e)
	}
	return nil, fmt.Errorf("unknown checker %q", e.Checker)
}

func (r *Runner) runPG(ctx context.Context, q string) ([]map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = '110s'"); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := map[string]any{}
		for i, c := range cols {
			m[c] = jsonSafe(vals[i])
		}
		out = append(out, m)
		if len(out) > 1000 {
			return nil, fmt.Errorf("eval query returned more than 1000 rows")
		}
	}
	return out, rows.Err()
}

func jsonSafe(v any) any {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	default:
		return v
	}
}

func (r *Runner) runHTTP(ctx context.Context, e Eval) (any, error) {
	method, _ := e.Spec["method"].(string)
	if method == "" {
		method = http.MethodGet
	}
	path, _ := e.Spec["path"].(string)
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("http eval: spec.path must start with /")
	}
	var body io.Reader
	if b, ok := e.Spec["body"]; ok && b != nil {
		bs, _ := json.Marshal(b)
		body = bytes.NewReader(bs)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.httpBase+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", e.OrgID)
	if r.adminKey != "" {
		req.Header.Set("X-Admin-Key", r.adminKey)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return map[string]any{"status": resp.StatusCode, "body": string(raw)}, nil
	}
	return map[string]any{"status": resp.StatusCode, "body": parsed}, nil
}

// Compare decides pass/fail. SQL checkers compare expected rows to actual rows
// positionally (both sorted by their JSON encoding so row order never matters);
// numeric cells pass within tolerancePct, other cells must be equal. The http
// checker requires every expected key to be present and equal/within tolerance
// (a subset match on the response body, plus status when given).
func Compare(checker string, expected, actual any, tolerancePct float64) (bool, []string) {
	var diffs []string
	switch checker {
	case "http":
		diffs = subsetDiff("", expected, actual, tolerancePct)
	default:
		exp := toRows(expected)
		act := toRows(actual)
		if exp == nil {
			return false, []string{"expected is not a row array"}
		}
		if len(exp) != len(act) {
			diffs = append(diffs, fmt.Sprintf("row count: expected %d, actual %d", len(exp), len(act)))
		}
		sortRows(exp)
		sortRows(act)
		for i := 0; i < len(exp) && i < len(act); i++ {
			diffs = append(diffs, subsetDiff(fmt.Sprintf("row[%d].", i), exp[i], act[i], tolerancePct)...)
		}
	}
	return len(diffs) == 0, diffs
}

func toRows(v any) []map[string]any {
	switch x := v.(type) {
	case []map[string]any:
		return x
	case []any:
		out := make([]map[string]any, 0, len(x))
		for _, e := range x {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			} else {
				return nil
			}
		}
		return out
	}
	return nil
}

func sortRows(rows []map[string]any) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, _ := json.Marshal(rows[i])
		b, _ := json.Marshal(rows[j])
		return string(a) < string(b)
	})
}

func subsetDiff(prefix string, expected, actual any, tol float64) []string {
	var diffs []string
	em, eok := expected.(map[string]any)
	am, aok := actual.(map[string]any)
	if eok {
		if !aok {
			return []string{prefix + "expected object, actual is not"}
		}
		keys := make([]string, 0, len(em))
		for k := range em {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			av, ok := am[k]
			if !ok {
				diffs = append(diffs, prefix+k+": missing")
				continue
			}
			diffs = append(diffs, subsetDiff(prefix+k+".", em[k], av, tol)...)
		}
		return diffs
	}
	if ef, ok := asFloat(expected); ok {
		af, ok2 := asFloat(actual)
		if !ok2 {
			return []string{fmt.Sprintf("%s expected %v, actual %v", strings.TrimSuffix(prefix, "."), expected, actual)}
		}
		if !within(ef, af, tol) {
			return []string{fmt.Sprintf("%s expected %v, actual %v (tol %.2f%%)", strings.TrimSuffix(prefix, "."), ef, af, tol)}
		}
		return nil
	}
	if fmt.Sprint(expected) != fmt.Sprint(actual) {
		return []string{fmt.Sprintf("%s expected %v, actual %v", strings.TrimSuffix(prefix, "."), expected, actual)}
	}
	return nil
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	case []byte:
		f, err := strconv.ParseFloat(strings.TrimSpace(string(x)), 64)
		return f, err == nil
	}
	return 0, false
}

func within(exp, act, tolPct float64) bool {
	if exp == act {
		return true
	}
	if tolPct <= 0 {
		return false
	}
	if exp == 0 {
		return math.Abs(act) <= tolPct/100.0
	}
	return math.Abs(act-exp)/math.Abs(exp)*100.0 <= tolPct
}
