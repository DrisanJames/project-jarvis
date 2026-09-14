package brain

// Observations (2026-09-13, operator: "the brain becomes the expert of the
// system … constantly learning, without much cost"). An observation is one
// MEASURED number — metric × grain × Denver day — written by the observer
// (agents/brain/observer.py, daily) from the platform's own capabilities:
// lake delivery, PG human engagement, worker health, DLQ depth, WAF, VDM,
// drip contracts. No LLM is involved in producing them. Baselines and
// deviations are computed from this series; a deviation becomes a finding
// claim, and the daily "state of the system" claim is composed from the
// day's rows. Idempotent by (org_id, metric, grain, day): re-observing a day
// overwrites (a fresh day is re-observed once settled).

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Observation is one measured value.
type Observation struct {
	ID         int64          `json:"id"`
	OrgID      string         `json:"org_id"`
	Metric     string         `json:"metric"` // e.g. lake.delivered, pg.human_clicks, ops.workers_stalled
	Grain      string         `json:"grain"`  // "all" | "src:ses" | "isp:gmail" | "domain:em.x.com" | "src:ses|isp:gmail" | "lane:yahoo_family"
	Day        string         `json:"day"`    // Denver operating day YYYY-MM-DD
	Value      float64        `json:"value"`
	Unit       string         `json:"unit"`   // count | pct | seconds | usd
	Source     string         `json:"source"` // collector name
	Meta       map[string]any `json:"meta,omitempty"`
	ObservedAt time.Time      `json:"observed_at"`
}

// ObservationInput is one row of a batch write.
type ObservationInput struct {
	Metric string         `json:"metric"`
	Grain  string         `json:"grain"`
	Day    string         `json:"day"`
	Value  float64        `json:"value"`
	Unit   string         `json:"unit"`
	Source string         `json:"source"`
	Meta   map[string]any `json:"meta,omitempty"`
}

var (
	obsMetricRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+){1,3}$`)
	obsGrainRe  = regexp.MustCompile(`^[a-z0-9_.:|@-]{1,120}$`)
	obsDayRe    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

const obsCols = `id, org_id, metric, grain, to_char(day,'YYYY-MM-DD'), value, unit, source, meta, observed_at`

// MaxObservationBatch bounds one write.
const MaxObservationBatch = 5000

func scanObservation(sc interface{ Scan(...any) error }) (Observation, error) {
	var o Observation
	var meta []byte
	if err := sc.Scan(&o.ID, &o.OrgID, &o.Metric, &o.Grain, &o.Day, &o.Value, &o.Unit, &o.Source, &meta, &o.ObservedAt); err != nil {
		return o, err
	}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &o.Meta)
	}
	return o, nil
}

func validateObservation(in ObservationInput) error {
	if !obsMetricRe.MatchString(in.Metric) {
		return fmt.Errorf("%w: metric %q must look like family.name[.sub]", ErrBadInput, in.Metric)
	}
	if in.Grain == "" {
		return fmt.Errorf("%w: grain is required (use \"all\")", ErrBadInput)
	}
	if !obsGrainRe.MatchString(in.Grain) {
		return fmt.Errorf("%w: grain %q", ErrBadInput, in.Grain)
	}
	if !obsDayRe.MatchString(in.Day) {
		return fmt.Errorf("%w: day %q must be YYYY-MM-DD", ErrBadInput, in.Day)
	}
	if _, err := time.Parse("2006-01-02", in.Day); err != nil {
		return fmt.Errorf("%w: day %q", ErrBadInput, in.Day)
	}
	switch in.Unit {
	case "count", "pct", "seconds", "usd", "ratio":
	default:
		return fmt.Errorf("%w: unit %q must be count|pct|seconds|usd|ratio", ErrBadInput, in.Unit)
	}
	if strings.TrimSpace(in.Source) == "" {
		return fmt.Errorf("%w: source is required", ErrBadInput)
	}
	return nil
}

// UpsertObservations writes a batch; every row is validated before the first
// write so a bad row rejects the whole batch. Returns the number written.
func (s *Store) UpsertObservations(ctx context.Context, orgID string, rows []ObservationInput) (int, error) {
	if len(rows) == 0 {
		return 0, fmt.Errorf("%w: no observations", ErrBadInput)
	}
	if len(rows) > MaxObservationBatch {
		return 0, fmt.Errorf("%w: batch of %d exceeds %d", ErrBadInput, len(rows), MaxObservationBatch)
	}
	for i, r := range rows {
		if err := validateObservation(r); err != nil {
			return 0, fmt.Errorf("row %d: %w", i, err)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO jarvis_brain_observations (org_id, metric, grain, day, value, unit, source, meta, observed_at)
		VALUES ($1, $2, $3, $4::date, $5, $6, $7, $8, NOW())
		ON CONFLICT (org_id, metric, grain, day) DO UPDATE SET
			value = EXCLUDED.value, unit = EXCLUDED.unit, source = EXCLUDED.source,
			meta = EXCLUDED.meta, observed_at = NOW()`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	n := 0
	for _, r := range rows {
		meta := r.Meta
		if meta == nil {
			meta = map[string]any{}
		}
		mb, _ := json.Marshal(meta)
		if _, err := stmt.ExecContext(ctx, orgID, r.Metric, r.Grain, r.Day, r.Value, r.Unit, r.Source, mb); err != nil {
			return 0, err
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// ObservationQuery selects a series. Metric may be a prefix ("lake." matches
// every lake metric); Grain empty = every grain; Days bounds the window
// ending at End (default today, Denver-agnostic — callers pass the day).
type ObservationQuery struct {
	Metric string
	Grain  string
	End    string // YYYY-MM-DD; empty = no upper bound
	Days   int    // default 35, max 400
	Limit  int    // default 5000
}

// ListObservations returns rows newest-day first.
func (s *Store) ListObservations(ctx context.Context, orgID string, q ObservationQuery) ([]Observation, error) {
	if q.Days <= 0 || q.Days > 400 {
		q.Days = 35
	}
	if q.Limit <= 0 || q.Limit > 20000 {
		q.Limit = 5000
	}
	where := []string{"org_id=$1"}
	args := []any{orgID}
	if q.Metric != "" {
		if strings.HasSuffix(q.Metric, ".") {
			args = append(args, q.Metric+"%")
			where = append(where, fmt.Sprintf("metric LIKE $%d", len(args)))
		} else {
			args = append(args, q.Metric)
			where = append(where, fmt.Sprintf("metric=$%d", len(args)))
		}
	}
	if q.Grain != "" {
		args = append(args, q.Grain)
		where = append(where, fmt.Sprintf("grain=$%d", len(args)))
	}
	if q.End != "" {
		if !obsDayRe.MatchString(q.End) {
			return nil, fmt.Errorf("%w: end %q must be YYYY-MM-DD", ErrBadInput, q.End)
		}
		args = append(args, q.End)
		where = append(where, fmt.Sprintf("day <= $%d::date", len(args)))
		args = append(args, q.Days)
		where = append(where, fmt.Sprintf("day > $%d::date - ($%d || ' days')::interval", len(args)-1, len(args)))
	} else {
		args = append(args, q.Days)
		where = append(where, fmt.Sprintf("day > CURRENT_DATE - ($%d || ' days')::interval", len(args)))
	}
	args = append(args, q.Limit)
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT `+obsCols+` FROM jarvis_brain_observations
		WHERE %s ORDER BY day DESC, metric, grain LIMIT $%d`, strings.Join(where, " AND "), len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Observation{}
	for rows.Next() {
		o, err := scanObservation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ObservationDays lists the distinct days observed for a metric prefix (for
// "what has been observed" summaries).
func (s *Store) ObservationDays(ctx context.Context, orgID, metricPrefix string, limit int) ([]string, error) {
	if limit <= 0 || limit > 400 {
		limit = 60
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT to_char(day,'YYYY-MM-DD') FROM jarvis_brain_observations
		WHERE org_id=$1 AND metric LIKE $2 ORDER BY 1 DESC LIMIT $3`, orgID, metricPrefix+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
