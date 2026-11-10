package api

import (
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// Every v2 stage and every family sub-row is accepted by the rollup.
func TestDataPipelineRollup_V2StagesAndSubRowsAccepted(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "k")
	h, mock := newDPRouter(t)
	v2 := []string{"dirty_outside_queue", "touch_1", "touch_2", "touch_3", "touch_4", "touch_5",
		"cohort_mailed", "cohort_activated", "ramp_target", "cleaning_ordered", "cleaning_passed",
		"cleaning_cost_cents", "arrivals", "sent", "delivered", "deferred"}
	subs := []string{"yahoo", "aol", "att", "sbcglobal", "cox", "verizon", "frontier"}
	var rows []map[string]any
	for _, st := range v2 {
		rows = append(rows, map[string]any{"day": "2026-11-10", "lane": "ses", "isp": "microsoft", "stage": st, "n": 1})
	}
	for _, isp := range subs {
		rows = append(rows, map[string]any{"day": "2026-11-10", "lane": "ses", "isp": isp, "stage": "clean", "n": 1})
	}
	mock.ExpectBegin()
	for range rows {
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO data_pipeline_daily")).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectCommit()
	b, _ := json.Marshal(map[string]any{"rows": rows})
	rec := diDo(h, http.MethodPost, "/api/mailing/data-pipeline/rollup", string(b), adminHdr())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// yahoo_family is the roll-up; its sub-rows are shown but never added again.
func TestDataPipelineSummary_FamilySubRowsNotDoubleCounted(t *testing.T) {
	out := summaryFrom(t, sqlmock.NewRows(dpCols).
		AddRow("2026-11-10", "yahoo_family", "clean", "", "", int64(994000)).
		AddRow("2026-11-10", "yahoo", "clean", "", "", int64(600000)).
		AddRow("2026-11-10", "aol", "clean", "", "", int64(200000)).
		AddRow("2026-11-10", "verizon", "clean", "", "", int64(194000)).
		AddRow("2026-11-10", "microsoft", "clean", "", "", int64(6000)).
		AddRow("2026-11-10", "yahoo_family", "dirty_outside_queue", "s3", "", int64(500)).
		AddRow("2026-11-10", "aol", "dirty_outside_queue", "s3", "", int64(200)).
		AddRow("2026-11-10", "yahoo_family", "contract", "", "em.historythinking.com", int64(1000)).
		AddRow("2026-11-10", "aol", "contract", "", "em.historythinking.com", int64(300)))
	if *out.Totals.Clean != 1000000 {
		t.Fatalf("lane clean: want 1,000,000 (sub-rows inside yahoo_family), got %d", *out.Totals.Clean)
	}
	if aol := dpISP(out, "aol"); aol.Parent != "yahoo_family" || *aol.Clean != 200000 {
		t.Fatalf("aol sub-row: %+v", aol)
	}
	if len(out.Sources) != 1 || *out.Sources[0].DirtyOutsideQueue != 500 {
		t.Fatalf("source dirty total must skip sub-rows: %+v", out.Sources)
	}
	if len(out.Domains) != 1 || *out.Domains[0].Contract != 1000 {
		t.Fatalf("domain contract must skip sub-rows: %+v", out.Domains)
	}
}

func TestDataPipelineSummary_ActivationCleaningThrottleContracts(t *testing.T) {
	out := summaryFrom(t, sqlmock.NewRows(dpCols).
		// cohorts: 11-08 1000/50, 11-09 1000/30 → 7-day 4.0%
		AddRow("2026-11-08", "microsoft", "cohort_mailed", "", "", int64(1000)).
		AddRow("2026-11-08", "microsoft", "cohort_activated", "", "", int64(50)).
		AddRow("2026-11-09", "microsoft", "cohort_mailed", "", "", int64(1000)).
		AddRow("2026-11-09", "microsoft", "cohort_activated", "", "", int64(30)).
		// Email Oversight: two nights in November; month to date sums both.
		AddRow("2026-11-01", "microsoft", "cleaning_ordered", "", "", int64(10000)).
		AddRow("2026-11-01", "microsoft", "cleaning_cost_cents", "", "", int64(3000)).
		AddRow("2026-11-10", "microsoft", "cleaning_ordered", "", "", int64(20000)).
		AddRow("2026-11-10", "microsoft", "cleaning_passed", "", "", int64(15000)).
		AddRow("2026-11-10", "microsoft", "cleaning_cost_cents", "", "", int64(6000)).
		// throttling
		AddRow("2026-11-10", "microsoft", "sent", "", "", int64(1000)).
		AddRow("2026-11-10", "microsoft", "delivered", "", "", int64(900)).
		AddRow("2026-11-10", "microsoft", "deferred", "", "", int64(80)).
		// contracts: one domain over contract (1100 > 1000×1.05), one off the ramp (contract 600 > ramp 500)
		AddRow("2026-11-10", "microsoft", "contract", "", "em.a.com", int64(1000)).
		AddRow("2026-11-10", "microsoft", "first_mailed", "", "em.a.com", int64(1100)).
		AddRow("2026-11-10", "microsoft", "contract", "", "em.b.com", int64(600)).
		AddRow("2026-11-10", "microsoft", "ramp_target", "", "em.b.com", int64(500)).
		AddRow("2026-11-10", "microsoft", "first_mailed", "", "em.b.com", int64(600)).
		// touches
		AddRow("2026-11-10", "microsoft", "touch_3", "", "", int64(4200)))
	ms := dpISP(out, "microsoft")
	if ms.ActivationRate7d == nil || *ms.ActivationRate7d != 4 {
		t.Fatalf("activation 7d: want 4.0, got %v", ms.ActivationRate7d)
	}
	if len(out.Activation) != 2 || out.Activation[0].Rate == nil || *out.Activation[0].Rate != 5 {
		t.Fatalf("activation rows: %+v", out.Activation)
	}
	if ms.CleaningPassRate == nil || *ms.CleaningPassRate != 75 || *ms.DeliveredRate != 90 || *ms.DeferredRate != 8 {
		t.Fatalf("rates: pass %v delivered %v deferred %v", ms.CleaningPassRate, ms.DeliveredRate, ms.DeferredRate)
	}
	if *ms.Touch3 != 4200 || ms.Touch1 != nil {
		t.Fatalf("touches: %v %v", ms.Touch3, ms.Touch1)
	}
	m := out.CleaningMTD
	if m.Month != "2026-11" || *m.Ordered != 30000 || *m.CostCents != 9000 || *m.Passed != 15000 {
		t.Fatalf("month to date: %+v", m)
	}
	if len(out.Domains) != 2 || !out.Domains[0].OverContract || out.Domains[0].OffRamp ||
		out.Domains[1].OverContract || !out.Domains[1].OffRamp {
		t.Fatalf("contract flags: %+v", out.Domains)
	}
}

// Month-to-date reaches back before the requested window; the series does not.
func TestDataPipelineSummary_MTDReachesBeforeWindow(t *testing.T) {
	h, mock := newDPRouter(t)
	// days=3 → window 11-08..11-10, read from 11-01.
	mock.ExpectQuery(regexp.QuoteMeta("FROM data_pipeline_daily")).
		WithArgs(diOrg, "ses", "2026-11-01", "2026-11-10").
		WillReturnRows(sqlmock.NewRows(dpCols).
			AddRow("2026-11-02", "apple", "cleaning_ordered", "", "", int64(700)).
			AddRow("2026-11-10", "apple", "cleaning_ordered", "", "", int64(300)))
	rec := diDo(h, http.MethodGet, "/api/mailing/data-pipeline/summary?days=3", "", sessionHdr())
	var out pipelineSummaryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if *out.CleaningMTD.Ordered != 1000 || len(out.Cleaning) != 1 || out.RowCount != 1 {
		t.Fatalf("mtd %v nights %d rows %d", *out.CleaningMTD.Ordered, len(out.Cleaning), out.RowCount)
	}
}

// ── agent ───────────────────────────────────────────────────────────────────

const agentEntry = `{"entries":[{"kind":"check","status":"ok","title":"Reservoirs checked","body":{"isps_checked":5}}]}`

func TestDataPipelineAgent_PostAdminOnly(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "")
	h, _ := newDPRouter(t)
	if rec := diDo(h, http.MethodPost, "/api/mailing/data-pipeline/agent", agentEntry, map[string]string{"X-Admin-Key": ""}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unset key: want 401, got %d", rec.Code)
	}
	t.Setenv("ADMIN_API_KEY", "k")
	for _, hdr := range []map[string]string{nil, {"X-Admin-Key": "wrong"}, sessionHdr()} {
		if rec := diDo(h, http.MethodPost, "/api/mailing/data-pipeline/agent", agentEntry, hdr); rec.Code != http.StatusUnauthorized {
			t.Fatalf("hdr %v: want 401, got %d", hdr, rec.Code)
		}
	}
	if rec := diDo(h, http.MethodGet, "/api/mailing/data-pipeline/agent", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET without session: want 401, got %d", rec.Code)
	}
}

func TestDataPipelineAgent_PostValidatesAndAppendsOrgScoped(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "k")
	h, mock := newDPRouter(t)
	for name, body := range map[string]string{
		"kind":    `{"entries":[{"kind":"chat","status":"ok","title":"x"}]}`,
		"status":  `{"entries":[{"kind":"check","status":"great","title":"x"}]}`,
		"isp":     `{"entries":[{"kind":"check","status":"ok","isp":"gmail","title":"x"}]}`,
		"title":   `{"entries":[{"kind":"check","status":"ok","title":""}]}`,
		"body":    `{"entries":[{"kind":"check","status":"ok","title":"x","body":[1,2]}]}`,
		"at":      `{"entries":[{"kind":"check","status":"ok","title":"x","at":"yesterday"}]}`,
		"unknown": `{"entries":[{"kind":"check","status":"ok","title":"x","level":"high"}]}`,
	} {
		if rec := diDo(h, http.MethodPost, "/api/mailing/data-pipeline/agent", body, adminHdr()); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d %s", name, rec.Code, rec.Body.String())
		}
	}
	other := "11111111-2222-3333-4444-555555555555"
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO data_pipeline_agent_log")).
		WithArgs(other, "2026-11-10T19:00:00Z", "check", "ok", "", "Reservoirs checked", `{"agent":"pipeline-agent","isps_checked":5}`).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	hdr := adminHdr()
	hdr["X-Organization-ID"] = other
	rec := diDo(h, http.MethodPost, "/api/mailing/data-pipeline/agent", agentEntry, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func agentGet(t *testing.T, checkAt *time.Time, org string) pipelineAgentResponse {
	t.Helper()
	h, mock := newDPRouter(t) // clock 2026-11-10 19:00Z
	at := time.Date(2026, 11, 10, 18, 50, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("FROM data_pipeline_agent_log")+`\s+WHERE org_id = \$1\s+ORDER BY`).
		WithArgs(org, 50).
		WillReturnRows(sqlmock.NewRows([]string{"id", "at", "kind", "status", "isp", "title", "body"}).
			AddRow(int64(2), at, "decision", "warn", "apple", "Ordered 20,000 Apple records for cleaning", `{"ordered":20000}`))
	if checkAt == nil {
		mock.ExpectQuery(regexp.QuoteMeta("kind = 'check'")).WithArgs(org).WillReturnRows(sqlmock.NewRows([]string{"at", "status"}))
	} else {
		mock.ExpectQuery(regexp.QuoteMeta("kind = 'check'")).WithArgs(org).
			WillReturnRows(sqlmock.NewRows([]string{"at", "status"}).AddRow(*checkAt, "warn"))
	}
	mock.ExpectQuery(regexp.QuoteMeta("kind = 'report'")).WithArgs(org).
		WillReturnRows(sqlmock.NewRows([]string{"at", "mandate"}).AddRow(at, `{"may":["order cleaning up to $500/month"]}`))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE org_id = $1 AND at >= $2")).WithArgs(org, sqlmock.AnyArg(), pipelineAgentScorecardRows).
		WillReturnRows(sqlmock.NewRows([]string{"at", "kind", "status", "title", "body"}).
			AddRow(at, "check", "ok", "Reservoirs", `{"agent":"monitor"}`))
	hdr := sessionHdr()
	hdr["X-Organization-ID"] = org
	rec := diDo(h, http.MethodGet, "/api/mailing/data-pipeline/agent?limit=50", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	var out pipelineAgentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDataPipelineAgent_StaleFlag(t *testing.T) {
	fresh := time.Date(2026, 11, 10, 18, 30, 0, 0, time.UTC) // 30 min ago
	out := agentGet(t, &fresh, diOrg)
	if out.Header.Stale || *out.Header.MinutesSinceLastCheck != 30 || *out.Header.Status != "warn" {
		t.Fatalf("30 minutes: want not stale, got %+v", out.Header)
	}
	if len(out.Entries) != 1 || string(out.Header.Mandate) != `{"may":["order cleaning up to $500/month"]}` {
		t.Fatalf("entries/mandate: %+v", out)
	}
	old := time.Date(2026, 11, 10, 18, 14, 0, 0, time.UTC) // 46 min ago
	if out := agentGet(t, &old, diOrg); !out.Header.Stale {
		t.Fatalf("46 minutes: want stale, got %+v", out.Header)
	}
	other := "11111111-2222-3333-4444-555555555555"
	if out := agentGet(t, nil, other); !out.Header.Stale || out.Header.LastCheckAt != nil || out.OrgID != other {
		t.Fatalf("never checked in: want stale + null, got %+v", out.Header)
	}
}

func TestDataPipelineAgent_ScorecardInHeader(t *testing.T) {
	fresh := time.Date(2026, 11, 10, 18, 30, 0, 0, time.UTC)
	out := agentGet(t, &fresh, diOrg)
	sc := out.Header.Scorecard
	if sc == nil || sc.Last24h.Checks != 1 || len(sc.ChecksLatest) != 1 || sc.ChecksLatest[0].Agent != "monitor" {
		t.Fatalf("scorecard: %+v", sc)
	}
}

func TestDataPipelineAgent_ScorecardMaths(t *testing.T) {
	now := time.Date(2026, 11, 10, 19, 0, 0, 0, time.UTC)
	at := func(minAgo int) time.Time { return now.Add(-time.Duration(minAgo) * time.Minute) }
	row := func(m int, kind, status, title string, body map[string]interface{}) agentLogRow {
		if body == nil {
			body = map[string]interface{}{}
		}
		return agentLogRow{at: at(m), kind: kind, status: status, title: title, body: body, agent: "pipeline-agent"}
	}
	log := []agentLogRow{
		// cycle 1 (120 min ago): two checks 1 minute apart
		row(120, "check", "fail", "Microsoft reservoir", nil),
		row(119, "check", "ok", "Apple reservoir", nil),
		// cycle 2 (60 min ago): Microsoft fails again, Apple warns
		row(60, "check", "fail", "Microsoft reservoir", nil),
		row(60, "check", "warn", "Apple reservoir", nil),
		// cycle 3 (30 min ago): Microsoft cleared by ok; Comcast fails and stays open
		row(30, "check", "ok", "Microsoft reservoir", nil),
		row(29, "check", "fail", "Comcast reservoir", nil),
		row(29, "alert", "warn", "Comcast low", nil),
		row(28, "decision", "ok", "Order cleaning", nil),
		row(27, "repair", "ok", "Re-ran draw", nil),
		row(26, "learned", "ok", "forecast", map[string]interface{}{"forecast": 110.0, "actual": 100.0}),
		row(25, "learned", "ok", "forecast", map[string]interface{}{"forecast": 70.0, "actual": 100.0}),
		row(24, "learned", "ok", "no numbers", map[string]interface{}{"forecast": "high"}),
		// 2 days ago: outside 24h, inside 7d
		row(2*24*60, "check", "ok", "Apple reservoir", nil),
	}
	sc := scorePipelineAgent(log, now)
	w := sc.Last24h
	if w.Checks != 6 || w.OK != 2 || w.Warn != 1 || w.Fail != 3 {
		t.Fatalf("counts: %+v", w)
	}
	if w.PassRate == nil || math.Abs(*w.PassRate-100.0/3) > 1e-9 {
		t.Fatalf("pass rate: want 33.3, got %v", w.PassRate)
	}
	if w.Cycles != 3 || w.AvgMinutesBetweenCycles == nil || *w.AvgMinutesBetweenCycles != 45 {
		t.Fatalf("cycles: want 3 cycles starting 120, 60, 30 min ago = 45 min apart, got %d %v", w.Cycles, w.AvgMinutesBetweenCycles)
	}
	if w.LongestGapMinutes == nil || *w.LongestGapMinutes != 59 {
		t.Fatalf("longest gap: want 59 (119→60), got %v", w.LongestGapMinutes)
	}
	if w.Alerts != 1 || w.Decisions != 1 || w.Repairs != 1 || w.Learned != 3 {
		t.Fatalf("activity counts: %+v", w)
	}
	if w.ForecastSamples != 2 || w.ForecastMAPE == nil || *w.ForecastMAPE != 20 {
		t.Fatalf("forecast MAPE: want 20 over 2, got %v over %d", w.ForecastMAPE, w.ForecastSamples)
	}
	if sc.Last7d.Checks != 7 {
		t.Fatalf("7d checks: want 7, got %d", sc.Last7d.Checks)
	}
	// Microsoft's fails were cleared by the later ok; Comcast stays open, 29 min old.
	if len(sc.OpenFailures) != 1 || sc.OpenFailures[0].Title != "Comcast reservoir" || sc.OpenFailures[0].AgeMinutes != 29 {
		t.Fatalf("open failures: %+v", sc.OpenFailures)
	}
	latest := map[string]string{}
	for _, c := range sc.ChecksLatest {
		latest[c.Title] = c.Status
	}
	if latest["Microsoft reservoir"] != "ok" || latest["Apple reservoir"] != "warn" || latest["Comcast reservoir"] != "fail" {
		t.Fatalf("latest per check: %+v", latest)
	}
	// A fail followed only by a warn stays open.
	sc2 := scorePipelineAgent([]agentLogRow{row(10, "check", "fail", "X", nil), row(5, "check", "warn", "X", nil)}, now)
	if len(sc2.OpenFailures) != 1 || sc2.OpenFailures[0].AgeMinutes != 10 {
		t.Fatalf("warn must not clear a failure: %+v", sc2.OpenFailures)
	}
	// No checks → nil rates, zero counts.
	if e := scorePipelineAgent(nil, now); e.Last24h.PassRate != nil || e.Last24h.LongestGapMinutes != nil {
		t.Fatalf("empty scorecard: %+v", e.Last24h)
	}
}
