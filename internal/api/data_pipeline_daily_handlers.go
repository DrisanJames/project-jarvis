package api

// data_pipeline_daily_handlers.go serves the New Data Pipeline screen (Data Center →
// Pipeline, tasks/eng-team/requirements/2026-11-10-new-data-pipeline.md
// "Screen data contract") at /api/mailing/data-pipeline/*:
//
//	POST /rollup   X-Admin-Key ONLY — the Python pipeline posts one Denver
//	               day's per-ISP stage counts; idempotent upsert on the PK.
//	GET  /summary  session (or admin key) — the operator's read.
//
// STORAGE: data_pipeline_daily (cmd/server/main.go runStartupMigrations). It is
// a separate table rather than rows in data_ingest_rollup because that table
// has no organization column, no sending-domain column, and a uniqueness key
// built around dataset_id/supply_class/transition — carrying these rows there
// would need a schema change to a table the Data Ingest dashboard reads.
//
// READING RULE for one (day, isp, stage): a row with source='' AND
// sending_domain='' is the authoritative total. Without it the total is the
// sum of the per-source rows (sending_domain=''); without those, the sum of
// the per-sending-domain rows (source=''); without those, the sum of every
// row. A breakdown is never added to a total that was posted alongside it.
//
// UNKNOWN IS NOT ZERO (METRIC_CONTRACT §11.7): a stage with no row is null on
// the wire and "not yet measured" on screen. A lane total over ISPs that are
// partly missing names the missing ISPs in degraded[].
//
// WINDSTREAM is a sub-row of the long tail: longtail already includes it, so
// every lane / domain / source total skips the windstream rows.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ignite/sparkpost-monitor/internal/dataingest"
)

const (
	// pipelineLowDaysDefault: an ISP whose clean reservoir lasts fewer days
	// than this at the current contract is flagged "Low — get data".
	// Overridable per request with ?low_days=.
	pipelineLowDaysDefault = 3.0
	pipelineDaysDefault    = 14
	pipelineDaysMax        = 60
	pipelineAvgWindow      = 7
	// pipelineRollupMaxRows bounds one POST (one day is ~6 ISPs × 12 stages ×
	// ~30 sending domains plus the source rows, well under this).
	pipelineRollupMaxRows = 2000
	pipelineQueryTimeout  = 20 * time.Second
	pipelineWriteTimeout  = 25 * time.Second
	pipelineMaxTextLen    = 253
)

// The enumerations the POST validates (plan "Screen data contract").
var (
	pipelineLanes = []string{"ses"}
	// Display order. windstream is the long tail's sub-row.
	pipelineISPs   = []string{"microsoft", "apple", "comcast", "yahoo_family", "longtail", "windstream"}
	pipelineStages = []string{
		"received", "raw", "cleaned_ok", "cleaned_rejected", "clean", "unique_mailed",
		"target", "contract", "first_mailed", "in_funnel", "promoted", "retired",
	}
	pipelineISPParent = map[string]string{"windstream": "longtail"}
	// The funnel strip, in order.
	pipelineFunnel = []string{"received", "cleaned_ok", "clean", "first_mailed", "promoted"}
)

func pipelineIn(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// DataPipelineService owns the pipeline rollup write + summary read.
type DataPipelineService struct {
	db  *sql.DB
	now func() time.Time
}

func NewDataPipelineService(db *sql.DB) *DataPipelineService {
	return &DataPipelineService{db: db, now: time.Now}
}

// RegisterRoutes mounts under /api/mailing/data-pipeline. The read takes the
// Data Center's session gate; the write is admin-key only.
func (s *DataPipelineService) RegisterRoutes(r chi.Router) {
	r.With(dataIngestAuth).Get("/summary", s.HandleSummary)
	r.With(dataPipelineAdminOnly).Post("/rollup", s.HandleRollup)
}

// dataPipelineAdminOnly: closed by default — an UNSET ADMIN_API_KEY authorizes
// nobody, and a session alone cannot write the rollup.
func dataPipelineAdminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adminKey := os.Getenv("ADMIN_API_KEY")
		if adminKey == "" || r.Header.Get("X-Admin-Key") != adminKey {
			respondError(w, http.StatusUnauthorized, "unauthorized: X-Admin-Key required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *DataPipelineService) meta(r *http.Request) diMeta {
	return diMeta{
		AsOf:   s.now().UTC().Format(time.RFC3339),
		Source: sourceTable,
		Fields: map[string]string{},
		OrgID:  dataIngestOrgID(r),
	}
}

// ── POST /rollup ────────────────────────────────────────────────────────────

type pipelineRow struct {
	Day           string `json:"day"`
	Lane          string `json:"lane"`
	ISP           string `json:"isp"`
	Stage         string `json:"stage"`
	Source        string `json:"source"`
	SendingDomain string `json:"sending_domain"`
	N             *int64 `json:"n"`
}

type pipelineRollupRequest struct {
	Rows []pipelineRow `json:"rows"`
}

type pipelineRollupResponse struct {
	diMeta
	Rows     int      `json:"rows"`
	Upserted int      `json:"upserted"`
	Days     []string `json:"days"`
}

const dataPipelineUpsert = `
	INSERT INTO data_pipeline_daily (org_id, day, lane, isp, stage, source, sending_domain, n, updated_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
	ON CONFLICT (org_id, day, lane, isp, stage, source, sending_domain)
	DO UPDATE SET n = EXCLUDED.n, updated_at = NOW()`

// validatePipelineRows normalizes in place and returns the first problem.
func validatePipelineRows(rows []pipelineRow) error {
	seen := make(map[string]int, len(rows))
	for i := range rows {
		row := &rows[i]
		row.Day = strings.TrimSpace(row.Day)
		row.Lane = strings.ToLower(strings.TrimSpace(row.Lane))
		row.ISP = strings.ToLower(strings.TrimSpace(row.ISP))
		row.Stage = strings.ToLower(strings.TrimSpace(row.Stage))
		row.Source = strings.ToLower(strings.TrimSpace(row.Source))
		row.SendingDomain = strings.ToLower(strings.TrimSpace(row.SendingDomain))
		if _, err := time.Parse(dataingest.DayLayout, row.Day); err != nil {
			return fmt.Errorf("rows[%d]: day must be YYYY-MM-DD", i)
		}
		if !pipelineIn(pipelineLanes, row.Lane) {
			return fmt.Errorf("rows[%d]: lane %q not one of %s", i, row.Lane, strings.Join(pipelineLanes, ", "))
		}
		if !pipelineIn(pipelineISPs, row.ISP) {
			return fmt.Errorf("rows[%d]: isp %q not one of %s", i, row.ISP, strings.Join(pipelineISPs, ", "))
		}
		if !pipelineIn(pipelineStages, row.Stage) {
			return fmt.Errorf("rows[%d]: stage %q not one of %s", i, row.Stage, strings.Join(pipelineStages, ", "))
		}
		if len(row.Source) > pipelineMaxTextLen || len(row.SendingDomain) > pipelineMaxTextLen {
			return fmt.Errorf("rows[%d]: source / sending_domain longer than %d characters", i, pipelineMaxTextLen)
		}
		if row.N == nil {
			return fmt.Errorf("rows[%d]: n is required", i)
		}
		if *row.N < 0 {
			return fmt.Errorf("rows[%d]: n must be >= 0", i)
		}
		key := strings.Join([]string{row.Day, row.Lane, row.ISP, row.Stage, row.Source, row.SendingDomain}, "\x00")
		if j, dup := seen[key]; dup {
			return fmt.Errorf("rows[%d]: same key as rows[%d] (day, lane, isp, stage, source, sending_domain)", i, j)
		}
		seen[key] = i
	}
	return nil
}

// HandleRollup upserts the posted rows for the caller's org. Re-posting the
// same rows converges (ON CONFLICT DO UPDATE), it never adds.
func (s *DataPipelineService) HandleRollup(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	dec.DisallowUnknownFields()
	var req pipelineRollupRequest
	if err := dec.Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(req.Rows) == 0 {
		respondError(w, http.StatusBadRequest, "no rows")
		return
	}
	if len(req.Rows) > pipelineRollupMaxRows {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("at most %d rows per request", pipelineRollupMaxRows))
		return
	}
	if err := validatePipelineRows(req.Rows); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	out := pipelineRollupResponse{diMeta: s.meta(r), Rows: len(req.Rows)}
	org := out.OrgID
	qctx, cancel := context.WithTimeout(r.Context(), pipelineWriteTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(qctx, nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "rollup_tx_failed: "+err.Error())
		return
	}
	defer tx.Rollback() //nolint:errcheck // committed below on success
	days := map[string]bool{}
	for _, row := range req.Rows {
		if _, err := tx.ExecContext(qctx, dataPipelineUpsert,
			org, row.Day, row.Lane, row.ISP, row.Stage, row.Source, row.SendingDomain, *row.N); err != nil {
			respondError(w, http.StatusInternalServerError, "rollup_write_failed: "+err.Error())
			return
		}
		out.Upserted++
		days[row.Day] = true
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "rollup_commit_failed: "+err.Error())
		return
	}
	for d := range days {
		out.Days = append(out.Days, d)
	}
	sort.Strings(out.Days)
	out.mark("upserted", fieldMeasured)
	respondJSON(w, http.StatusOK, out)
}

// ── GET /summary ────────────────────────────────────────────────────────────

// pipelineISP is one ISP's latest-day row. Every count is nullable.
type pipelineISP struct {
	ISP    string `json:"isp"`
	Parent string `json:"parent,omitempty"`
	pipelineFigures
	Avg7d map[string]*float64 `json:"avg_7d"`
}

// pipelineFigures is shared by an ISP row and the lane total.
type pipelineFigures struct {
	Received        *int64   `json:"received"`
	Raw             *int64   `json:"raw"`
	CleanedOK       *int64   `json:"cleaned_ok"`
	CleanedRejected *int64   `json:"cleaned_rejected"`
	Clean           *int64   `json:"clean"`
	UniqueMailed    *int64   `json:"unique_mailed"`
	Target          *int64   `json:"target"`
	Contract        *int64   `json:"contract"`
	FirstMailed     *int64   `json:"first_mailed"`
	InFunnel        *int64   `json:"in_funnel"`
	Promoted        *int64   `json:"promoted"`
	Retired         *int64   `json:"retired"`
	DaysOfSupply    *float64 `json:"days_of_supply"`
	PctOfTarget     *float64 `json:"pct_of_target"`
	NewShare        *float64 `json:"new_share"`
	Low             bool     `json:"low"`
}

type pipelineFunnelStep struct {
	Stage         string   `json:"stage"`
	Today         *int64   `json:"today"`
	Avg7d         *float64 `json:"avg_7d"`
	DropFromPrior *int64   `json:"drop_from_prior"`
	PctOfPrior    *float64 `json:"pct_of_prior"`
}

type pipelineSeriesPoint struct {
	Day    string           `json:"day"`
	ISP    string           `json:"isp"`
	Stages map[string]int64 `json:"stages"`
}

type pipelineDomainISP struct {
	ISP           string   `json:"isp"`
	Contract      *int64   `json:"contract"`
	FirstMailed   *int64   `json:"first_mailed"`
	Target        *int64   `json:"target"`
	PctOfContract *float64 `json:"pct_of_contract"`
}

type pipelineDomain struct {
	SendingDomain string              `json:"sending_domain"`
	Contract      *int64              `json:"contract"`
	FirstMailed   *int64              `json:"first_mailed"`
	Target        *int64              `json:"target"`
	PctOfContract *float64            `json:"pct_of_contract"`
	ByISP         []pipelineDomainISP `json:"by_isp"`
}

type pipelineSourceISP struct {
	ISP   string `json:"isp"`
	Raw   *int64 `json:"raw"`
	Clean *int64 `json:"clean"`
}

type pipelineSource struct {
	Source string              `json:"source"`
	Raw    *int64              `json:"raw"`
	Clean  *int64              `json:"clean"`
	ByISP  []pipelineSourceISP `json:"by_isp"`
}

type pipelineSummaryResponse struct {
	diMeta
	Lane        string                `json:"lane"`
	Days        int                   `json:"days"`
	From        string                `json:"from"`
	To          string                `json:"to"`
	DenverDay   string                `json:"denver_day"`
	LatestDay   *string               `json:"latest_day"`
	LowDays     float64               `json:"low_days_threshold"`
	RowCount    int                   `json:"row_count"`
	ISPs        []pipelineISP         `json:"isps"`
	Totals      pipelineFigures       `json:"totals"`
	TotalsAvg7d map[string]*float64   `json:"totals_avg_7d"`
	Funnel      []pipelineFunnelStep  `json:"funnel"`
	Series      []pipelineSeriesPoint `json:"series"`
	Domains     []pipelineDomain      `json:"domains"`
	Sources     []pipelineSource      `json:"sources"`
	Degraded    []string              `json:"degraded"`
}

const dataPipelineSummarySQL = `
	SELECT to_char(day, 'YYYY-MM-DD'), isp, stage, source, sending_domain, n
	FROM data_pipeline_daily
	WHERE org_id = $1 AND lane = $2 AND day >= $3::date AND day <= $4::date`

type pipelineDBRow struct {
	day, isp, stage, source, domain string
	n                               int64
}

// stageAcc applies the READING RULE (file header) for one (day, isp, stage).
type stageAcc struct {
	total, bySource, byDomain, both         int64
	hasTotal, hasSource, hasDomain, hasBoth bool
}

func (a *stageAcc) add(source, domain string, n int64) {
	switch {
	case source == "" && domain == "":
		a.total, a.hasTotal = a.total+n, true
	case domain == "":
		a.bySource, a.hasSource = a.bySource+n, true
	case source == "":
		a.byDomain, a.hasDomain = a.byDomain+n, true
	default:
		a.both, a.hasBoth = a.both+n, true
	}
}

func (a *stageAcc) value() int64 {
	switch {
	case a.hasTotal:
		return a.total
	case a.hasSource:
		return a.bySource
	case a.hasDomain:
		return a.byDomain
	default:
		return a.both
	}
}

func ptrI64(v int64) *int64     { return &v }
func ptrF64(v float64) *float64 { return &v }

// ratio returns num/den (×scale) or nil when either is unknown or den is 0.
func ratio(num, den *int64, scale float64) *float64 {
	if num == nil || den == nil || *den == 0 {
		return nil
	}
	return ptrF64(float64(*num) / float64(*den) * scale)
}

// derive fills the computed fields from the counted ones.
func (f *pipelineFigures) derive(lowDays float64) {
	f.DaysOfSupply = ratio(f.Clean, f.Contract, 1)
	f.PctOfTarget = ratio(f.FirstMailed, f.Target, 100)
	f.NewShare = ratio(f.FirstMailed, f.UniqueMailed, 100)
	f.Low = f.DaysOfSupply != nil && *f.DaysOfSupply < lowDays
}

func (f *pipelineFigures) set(stage string, v *int64) {
	switch stage {
	case "received":
		f.Received = v
	case "raw":
		f.Raw = v
	case "cleaned_ok":
		f.CleanedOK = v
	case "cleaned_rejected":
		f.CleanedRejected = v
	case "clean":
		f.Clean = v
	case "unique_mailed":
		f.UniqueMailed = v
	case "target":
		f.Target = v
	case "contract":
		f.Contract = v
	case "first_mailed":
		f.FirstMailed = v
	case "in_funnel":
		f.InFunnel = v
	case "promoted":
		f.Promoted = v
	case "retired":
		f.Retired = v
	}
}

func addOpt(dst **int64, v *int64) {
	if v == nil {
		return
	}
	if *dst == nil {
		*dst = ptrI64(0)
	}
	**dst += *v
}

func dayShift(day string, n int) string {
	t, err := time.ParseInLocation(dataingest.DayLayout, day, dataingest.Denver)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, n).Format(dataingest.DayLayout)
}

// HandleSummary — GET /summary?lane=ses&days=14[&low_days=3]
func (s *DataPipelineService) HandleSummary(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lane := strings.ToLower(strings.TrimSpace(q.Get("lane")))
	if lane == "" {
		lane = "ses"
	}
	if !pipelineIn(pipelineLanes, lane) {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("lane must be one of %s", strings.Join(pipelineLanes, ", ")))
		return
	}
	days := pipelineDaysDefault
	if v := strings.TrimSpace(q.Get("days")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > pipelineDaysMax {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("days must be 1..%d", pipelineDaysMax))
			return
		}
		days = n
	}
	lowDays := pipelineLowDaysDefault
	if v := strings.TrimSpace(q.Get("low_days")); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 {
			respondError(w, http.StatusBadRequest, "low_days must be a positive number")
			return
		}
		lowDays = f
	}
	if s.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	today := dataingest.DayOf(s.now())
	from := dayShift(today, -(days - 1))
	out := pipelineSummaryResponse{
		diMeta: s.meta(r), Lane: lane, Days: days, From: from, To: today,
		DenverDay: today, LowDays: lowDays, Degraded: []string{},
		Series: []pipelineSeriesPoint{}, Domains: []pipelineDomain{}, Sources: []pipelineSource{},
	}

	qctx, cancel := context.WithTimeout(r.Context(), pipelineQueryTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(qctx, dataPipelineSummarySQL, out.OrgID, lane, from, today)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "pipeline_query_failed: "+err.Error())
		return
	}
	var dbRows []pipelineDBRow
	for rows.Next() {
		var x pipelineDBRow
		if err := rows.Scan(&x.day, &x.isp, &x.stage, &x.source, &x.domain, &x.n); err != nil {
			rows.Close()
			respondError(w, http.StatusInternalServerError, "pipeline_scan_failed: "+err.Error())
			return
		}
		dbRows = append(dbRows, x)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		respondError(w, http.StatusInternalServerError, "pipeline_query_failed: "+err.Error())
		return
	}
	rows.Close()
	out.RowCount = len(dbRows)
	buildPipelineSummary(&out, dbRows)
	respondJSON(w, http.StatusOK, out)
}

// buildPipelineSummary is the pure aggregation (tested without a DB).
func buildPipelineSummary(out *pipelineSummaryResponse, dbRows []pipelineDBRow) {
	// (day, isp, stage) → reading-rule accumulator.
	acc := map[string]map[string]map[string]*stageAcc{}
	latest := ""
	for _, x := range dbRows {
		if acc[x.day] == nil {
			acc[x.day] = map[string]map[string]*stageAcc{}
		}
		if acc[x.day][x.isp] == nil {
			acc[x.day][x.isp] = map[string]*stageAcc{}
		}
		a := acc[x.day][x.isp][x.stage]
		if a == nil {
			a = &stageAcc{}
			acc[x.day][x.isp][x.stage] = a
		}
		a.add(x.source, x.domain, x.n)
		if x.day > latest {
			latest = x.day
		}
	}
	val := func(day, isp, stage string) *int64 {
		if a := acc[day][isp][stage]; a != nil {
			return ptrI64(a.value())
		}
		return nil
	}
	// Lane total for one day/stage: non-windstream ISPs that reported.
	laneTotal := func(day, stage string) *int64 {
		var t *int64
		for _, isp := range pipelineISPs {
			if pipelineISPParent[isp] != "" {
				continue
			}
			addOpt(&t, val(day, isp, stage))
		}
		return t
	}

	sortedDays := make([]string, 0, len(acc))
	for d := range acc {
		sortedDays = append(sortedDays, d)
	}
	sort.Strings(sortedDays)
	for _, d := range sortedDays {
		for _, isp := range pipelineISPs {
			if len(acc[d][isp]) == 0 {
				continue
			}
			p := pipelineSeriesPoint{Day: d, ISP: isp, Stages: map[string]int64{}}
			for stage, a := range acc[d][isp] {
				p.Stages[stage] = a.value()
			}
			out.Series = append(out.Series, p)
		}
	}

	// 7-day mean over the days that HAVE a value (a missing day is unknown,
	// not zero, so it is not averaged in as one).
	avg := func(get func(day string) *int64) *float64 {
		if latest == "" {
			return nil
		}
		var sum float64
		n := 0
		for i := 0; i < pipelineAvgWindow; i++ {
			if v := get(dayShift(latest, -i)); v != nil {
				sum += float64(*v)
				n++
			}
		}
		if n == 0 {
			return nil
		}
		return ptrF64(sum / float64(n))
	}

	allFields := append(append([]string{}, pipelineStages...), "days_of_supply", "pct_of_target", "new_share", "low", "avg_7d")
	if latest == "" {
		out.markAll(fieldNotMeasured, allFields...)
		out.ISPs = make([]pipelineISP, 0, len(pipelineISPs))
		for _, isp := range pipelineISPs {
			out.ISPs = append(out.ISPs, pipelineISP{ISP: isp, Parent: pipelineISPParent[isp], Avg7d: map[string]*float64{}})
		}
		out.TotalsAvg7d = map[string]*float64{}
		out.Funnel = []pipelineFunnelStep{}
		for _, st := range pipelineFunnel {
			out.Funnel = append(out.Funnel, pipelineFunnelStep{Stage: st})
		}
		return
	}
	out.LatestDay = &latest

	for _, isp := range pipelineISPs {
		row := pipelineISP{ISP: isp, Parent: pipelineISPParent[isp], Avg7d: map[string]*float64{}}
		for _, st := range pipelineStages {
			row.set(st, val(latest, isp, st))
			stage := st
			row.Avg7d[st] = avg(func(day string) *int64 { return val(day, isp, stage) })
		}
		row.derive(out.LowDays)
		out.ISPs = append(out.ISPs, row)
	}

	out.TotalsAvg7d = map[string]*float64{}
	for _, st := range pipelineStages {
		out.Totals.set(st, laneTotal(latest, st))
		stage := st
		out.TotalsAvg7d[st] = avg(func(day string) *int64 { return laneTotal(day, stage) })
		// A partial total names who is missing rather than reading as complete.
		var have, missing []string
		for _, isp := range pipelineISPs {
			if pipelineISPParent[isp] != "" {
				continue
			}
			if acc[latest][isp][st] != nil {
				have = append(have, isp)
			} else {
				missing = append(missing, isp)
			}
		}
		if len(have) > 0 && len(missing) > 0 {
			out.Degraded = append(out.Degraded, fmt.Sprintf("%s on %s: no row for %s — the lane total covers %d of %d ISPs",
				st, latest, strings.Join(missing, ", "), len(have), len(have)+len(missing)))
		}
	}
	out.Totals.derive(out.LowDays)

	var prior *int64
	for i, st := range pipelineFunnel {
		step := pipelineFunnelStep{Stage: st, Today: laneTotal(latest, st), Avg7d: out.TotalsAvg7d[st]}
		if i > 0 && prior != nil && step.Today != nil {
			step.DropFromPrior = ptrI64(*prior - *step.Today)
			step.PctOfPrior = ratio(step.Today, prior, 100)
		}
		prior = step.Today
		out.Funnel = append(out.Funnel, step)
	}

	// Per sending domain (latest day): rows that carry a domain, summed across
	// sources; the domain total skips windstream.
	domAcc := map[string]map[string]map[string]int64{} // domain → isp → stage → n
	srcAcc := map[string]map[string]map[string]int64{} // source → isp → stage → n
	for _, x := range dbRows {
		if x.day != latest {
			continue
		}
		if x.domain != "" {
			if domAcc[x.domain] == nil {
				domAcc[x.domain] = map[string]map[string]int64{}
			}
			if domAcc[x.domain][x.isp] == nil {
				domAcc[x.domain][x.isp] = map[string]int64{}
			}
			domAcc[x.domain][x.isp][x.stage] += x.n
		} else if x.source != "" && (x.stage == "raw" || x.stage == "clean") {
			if srcAcc[x.source] == nil {
				srcAcc[x.source] = map[string]map[string]int64{}
			}
			if srcAcc[x.source][x.isp] == nil {
				srcAcc[x.source][x.isp] = map[string]int64{}
			}
			srcAcc[x.source][x.isp][x.stage] += x.n
		}
	}
	get := func(m map[string]int64, stage string) *int64 {
		if v, ok := m[stage]; ok {
			return ptrI64(v)
		}
		return nil
	}
	domNames := make([]string, 0, len(domAcc))
	for d := range domAcc {
		domNames = append(domNames, d)
	}
	sort.Strings(domNames)
	for _, d := range domNames {
		dom := pipelineDomain{SendingDomain: d, ByISP: []pipelineDomainISP{}}
		for _, isp := range pipelineISPs {
			m, ok := domAcc[d][isp]
			if !ok {
				continue
			}
			di := pipelineDomainISP{ISP: isp, Contract: get(m, "contract"), FirstMailed: get(m, "first_mailed"), Target: get(m, "target")}
			di.PctOfContract = ratio(di.FirstMailed, di.Contract, 100)
			dom.ByISP = append(dom.ByISP, di)
			if pipelineISPParent[isp] == "" {
				addOpt(&dom.Contract, di.Contract)
				addOpt(&dom.FirstMailed, di.FirstMailed)
				addOpt(&dom.Target, di.Target)
			}
		}
		dom.PctOfContract = ratio(dom.FirstMailed, dom.Contract, 100)
		out.Domains = append(out.Domains, dom)
	}
	srcNames := make([]string, 0, len(srcAcc))
	for src := range srcAcc {
		srcNames = append(srcNames, src)
	}
	sort.Strings(srcNames)
	for _, src := range srcNames {
		so := pipelineSource{Source: src, ByISP: []pipelineSourceISP{}}
		for _, isp := range pipelineISPs {
			m, ok := srcAcc[src][isp]
			if !ok {
				continue
			}
			si := pipelineSourceISP{ISP: isp, Raw: get(m, "raw"), Clean: get(m, "clean")}
			so.ByISP = append(so.ByISP, si)
			if pipelineISPParent[isp] == "" {
				addOpt(&so.Raw, si.Raw)
				addOpt(&so.Clean, si.Clean)
			}
		}
		out.Sources = append(out.Sources, so)
	}

	for _, st := range pipelineStages {
		if out.Totals.get(st) != nil {
			out.mark(st, fieldMeasured)
		} else {
			out.mark(st, fieldNotMeasured)
		}
	}
	out.markAll(fieldDerived, "days_of_supply", "pct_of_target", "new_share", "low", "avg_7d")
	// windstream-only stages still count as measured for the windstream row.
	for _, st := range pipelineStages {
		if out.Fields[st] == fieldNotMeasured && acc[latest]["windstream"][st] != nil {
			out.mark(st, fieldMeasured)
		}
	}
}

func (f *pipelineFigures) get(stage string) *int64 {
	switch stage {
	case "received":
		return f.Received
	case "raw":
		return f.Raw
	case "cleaned_ok":
		return f.CleanedOK
	case "cleaned_rejected":
		return f.CleanedRejected
	case "clean":
		return f.Clean
	case "unique_mailed":
		return f.UniqueMailed
	case "target":
		return f.Target
	case "contract":
		return f.Contract
	case "first_mailed":
		return f.FirstMailed
	case "in_funnel":
		return f.InFunnel
	case "promoted":
		return f.Promoted
	case "retired":
		return f.Retired
	}
	return nil
}
