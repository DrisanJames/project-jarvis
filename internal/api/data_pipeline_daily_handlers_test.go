package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
)

// newDPRouter mirrors production: the service mounted at /api/mailing/data-pipeline.
// The clock is 2026-11-10 12:00 MT.
func newDPRouter(t *testing.T) (http.Handler, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc := NewDataPipelineService(db)
	svc.now = func() time.Time { return time.Date(2026, 11, 10, 19, 0, 0, 0, time.UTC) }
	r := chi.NewRouter()
	r.Route("/api/mailing", func(mr chi.Router) {
		mr.Route("/data-pipeline", svc.RegisterRoutes)
	})
	return r, mock
}

const dpBody = `{"rows":[
 {"day":"2026-11-10","lane":"ses","isp":"microsoft","stage":"clean","n":198921},
 {"day":"2026-11-10","lane":"ses","isp":"microsoft","stage":"contract","sending_domain":"em.discountblog.com","n":5000}
]}`

func adminHdr() map[string]string { return map[string]string{"X-Admin-Key": "k"} }

var dpCols = []string{"day", "isp", "stage", "source", "sending_domain", "n"}

func TestDataPipelineRollup_AdminKeyRequired(t *testing.T) {
	// Unset ADMIN_API_KEY authorizes nobody — not even a blank header.
	t.Setenv("ADMIN_API_KEY", "")
	h, _ := newDPRouter(t)
	for _, hdr := range []map[string]string{nil, {"X-Admin-Key": ""}, sessionHdr()} {
		if rec := diDo(h, http.MethodPost, "/api/mailing/data-pipeline/rollup", dpBody, hdr); rec.Code != http.StatusUnauthorized {
			t.Fatalf("unset key, hdr %v: want 401, got %d", hdr, rec.Code)
		}
	}
	t.Setenv("ADMIN_API_KEY", "k")
	for _, hdr := range []map[string]string{nil, {"X-Admin-Key": "wrong"}, sessionHdr()} {
		rec := diDo(h, http.MethodPost, "/api/mailing/data-pipeline/rollup", dpBody, hdr)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("hdr %v: want 401, got %d", hdr, rec.Code)
		}
		var e map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e["error"] == "" {
			t.Fatalf("want {\"error\":...}, got %s", rec.Body.String())
		}
	}
}

func TestDataPipelineSummary_ClosedWithoutSession(t *testing.T) {
	h, _ := newDPRouter(t)
	if rec := diDo(h, http.MethodGet, "/api/mailing/data-pipeline/summary", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

// Posting the same rows twice issues the same ON CONFLICT DO UPDATE upsert,
// org-scoped, with the same values — the second post converges, never adds.
func TestDataPipelineRollup_UpsertIdempotent(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "k")
	h, mock := newDPRouter(t)
	upsert := regexp.QuoteMeta("ON CONFLICT (org_id, day, lane, isp, stage, source, sending_domain)") + `\s+DO UPDATE SET n = EXCLUDED.n`
	for pass := 0; pass < 2; pass++ {
		mock.ExpectBegin()
		mock.ExpectExec(upsert).
			WithArgs(diOrg, "2026-11-10", "ses", "microsoft", "clean", "", "", int64(198921)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(upsert).
			WithArgs(diOrg, "2026-11-10", "ses", "microsoft", "contract", "", "em.discountblog.com", int64(5000)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
	}
	for pass := 0; pass < 2; pass++ {
		rec := diDo(h, http.MethodPost, "/api/mailing/data-pipeline/rollup", dpBody, adminHdr())
		if rec.Code != http.StatusOK {
			t.Fatalf("pass %d: want 200, got %d %s", pass, rec.Code, rec.Body.String())
		}
		var out pipelineRollupResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Upserted != 2 || out.Rows != 2 || len(out.Days) != 1 || out.Days[0] != "2026-11-10" || out.OrgID != diOrg {
			t.Fatalf("pass %d: unexpected body %s", pass, rec.Body.String())
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDataPipelineRollup_EnumValidation(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "k")
	h, mock := newDPRouter(t)
	bad := map[string]string{
		"lane":      `{"rows":[{"day":"2026-11-10","lane":"pmta","isp":"apple","stage":"clean","n":1}]}`,
		"isp":       `{"rows":[{"day":"2026-11-10","lane":"ses","isp":"gmail","stage":"clean","n":1}]}`,
		"stage":     `{"rows":[{"day":"2026-11-10","lane":"ses","isp":"apple","stage":"sent","n":1}]}`,
		"day":       `{"rows":[{"day":"11/10/2026","lane":"ses","isp":"apple","stage":"clean","n":1}]}`,
		"n missing": `{"rows":[{"day":"2026-11-10","lane":"ses","isp":"apple","stage":"clean"}]}`,
		"n < 0":     `{"rows":[{"day":"2026-11-10","lane":"ses","isp":"apple","stage":"clean","n":-1}]}`,
		"unknown":   `{"rows":[{"day":"2026-11-10","lane":"ses","isp":"apple","stage":"clean","n":1,"domain":"x"}]}`,
		"duplicate": `{"rows":[{"day":"2026-11-10","lane":"ses","isp":"apple","stage":"clean","n":1},{"day":"2026-11-10","lane":"ses","isp":"apple","stage":"clean","n":2}]}`,
		"empty":     `{"rows":[]}`,
	}
	for name, body := range bad {
		if rec := diDo(h, http.MethodPost, "/api/mailing/data-pipeline/rollup", body, adminHdr()); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// Nothing reached the database.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDataPipelineRollup_BatchBounded(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "k")
	h, _ := newDPRouter(t)
	rows := make([]map[string]any, pipelineRollupMaxRows+1)
	for i := range rows {
		rows[i] = map[string]any{"day": "2026-11-10", "lane": "ses", "isp": "apple", "stage": "clean", "sending_domain": time.Duration(i).String(), "n": 1}
	}
	b, _ := json.Marshal(map[string]any{"rows": rows})
	if rec := diDo(h, http.MethodPost, "/api/mailing/data-pipeline/rollup", string(b), adminHdr()); rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 over the row bound, got %d", rec.Code)
	}
}

// The summary read is bound to the RESOLVED org: the header org goes to $1.
func TestDataPipelineSummary_OrgScoped(t *testing.T) {
	h, mock := newDPRouter(t)
	other := "11111111-2222-3333-4444-555555555555"
	mock.ExpectQuery(regexp.QuoteMeta("FROM data_pipeline_daily")+`\s+WHERE org_id = \$1`).
		WithArgs(other, "ses", "2026-10-28", "2026-11-10").
		WillReturnRows(sqlmock.NewRows(dpCols))
	hdr := sessionHdr()
	hdr["X-Organization-ID"] = other
	rec := diDo(h, http.MethodGet, "/api/mailing/data-pipeline/summary?lane=ses&days=14", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	var out pipelineSummaryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.OrgID != other || out.LatestDay != nil || len(out.ISPs) != len(pipelineISPs) || out.Fields["clean"] != fieldNotMeasured {
		t.Fatalf("empty summary wrong: %s", rec.Body.String())
	}
}

func TestDataPipelineSummary_BadParams(t *testing.T) {
	h, _ := newDPRouter(t)
	for _, q := range []string{"lane=pmta", "days=0", "days=61", "days=x", "low_days=-1"} {
		if rec := diDo(h, http.MethodGet, "/api/mailing/data-pipeline/summary?"+q, "", sessionHdr()); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d", q, rec.Code)
		}
	}
}

func summaryFrom(t *testing.T, rows *sqlmock.Rows) pipelineSummaryResponse {
	t.Helper()
	h, mock := newDPRouter(t)
	mock.ExpectQuery(regexp.QuoteMeta("FROM data_pipeline_daily")).WillReturnRows(rows)
	rec := diDo(h, http.MethodGet, "/api/mailing/data-pipeline/summary", "", sessionHdr())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var out pipelineSummaryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func dpISP(out pipelineSummaryResponse, isp string) pipelineISP {
	for _, x := range out.ISPs {
		if x.ISP == isp {
			return x
		}
	}
	return pipelineISP{}
}

func TestDataPipelineSummary_DaysOfSupplyContractZero(t *testing.T) {
	out := summaryFrom(t, sqlmock.NewRows(dpCols).
		AddRow("2026-11-10", "apple", "clean", "", "", int64(22651)).
		AddRow("2026-11-10", "apple", "contract", "", "", int64(0)).
		AddRow("2026-11-10", "microsoft", "clean", "", "", int64(10000)).
		AddRow("2026-11-10", "microsoft", "contract", "", "", int64(5000)))
	ap := dpISP(out, "apple")
	if ap.DaysOfSupply != nil || ap.Low {
		t.Fatalf("contract 0: want days_of_supply null and not low, got %v low=%v", ap.DaysOfSupply, ap.Low)
	}
	ms := dpISP(out, "microsoft")
	if ms.DaysOfSupply == nil || *ms.DaysOfSupply != 2 || !ms.Low {
		t.Fatalf("microsoft: want 2.0 days and low (<3), got %v low=%v", ms.DaysOfSupply, ms.Low)
	}
	// An ISP with no rows is unknown, not zero.
	if cc := dpISP(out, "comcast"); cc.Clean != nil || cc.DaysOfSupply != nil {
		t.Fatalf("comcast with no rows must be null, got %+v", cc)
	}
}

func TestDataPipelineSummary_WindstreamNotDoubleCounted(t *testing.T) {
	out := summaryFrom(t, sqlmock.NewRows(dpCols).
		AddRow("2026-11-10", "longtail", "clean", "", "", int64(119000)).
		AddRow("2026-11-10", "windstream", "clean", "", "", int64(19766)).
		AddRow("2026-11-10", "microsoft", "clean", "", "", int64(1000)).
		AddRow("2026-11-10", "longtail", "first_mailed", "", "em.discountblog.com", int64(100)).
		AddRow("2026-11-10", "windstream", "first_mailed", "", "em.discountblog.com", int64(40)).
		AddRow("2026-11-10", "longtail", "raw", "partner", "", int64(50000)).
		AddRow("2026-11-10", "windstream", "raw", "partner", "", int64(9000)))
	if out.Totals.Clean == nil || *out.Totals.Clean != 120000 {
		t.Fatalf("lane clean total: want 120000 (windstream inside longtail), got %v", out.Totals.Clean)
	}
	if ws := dpISP(out, "windstream"); ws.Parent != "longtail" || ws.Clean == nil || *ws.Clean != 19766 {
		t.Fatalf("windstream row: want parent longtail and 19766, got %+v", ws)
	}
	if len(out.Domains) != 1 || *out.Domains[0].FirstMailed != 100 || len(out.Domains[0].ByISP) != 2 {
		t.Fatalf("domain total must skip windstream: %+v", out.Domains)
	}
	if len(out.Sources) != 1 || *out.Sources[0].Raw != 50000 {
		t.Fatalf("source total must skip windstream: %+v", out.Sources)
	}
	for _, f := range out.Funnel {
		if f.Stage == "clean" && (f.Today == nil || *f.Today != 120000) {
			t.Fatalf("funnel clean: want 120000, got %v", f.Today)
		}
	}
}

// A posted total row is authoritative; its breakdown rows are not added to it.
func TestDataPipelineSummary_TotalRowWinsOverBreakdown(t *testing.T) {
	out := summaryFrom(t, sqlmock.NewRows(dpCols).
		AddRow("2026-11-10", "microsoft", "clean", "", "", int64(1000)).
		AddRow("2026-11-10", "microsoft", "clean", "partner", "", int64(700)).
		AddRow("2026-11-10", "microsoft", "clean", "vertical", "", int64(300)).
		AddRow("2026-11-10", "apple", "clean", "partner", "", int64(40)).
		AddRow("2026-11-10", "apple", "clean", "vertical", "", int64(60)).
		AddRow("2026-11-10", "microsoft", "first_mailed", "", "", int64(400)).
		AddRow("2026-11-10", "microsoft", "target", "", "", int64(800)).
		AddRow("2026-11-10", "microsoft", "unique_mailed", "", "", int64(4000)).
		AddRow("2026-11-09", "microsoft", "first_mailed", "", "", int64(200)))
	ms := dpISP(out, "microsoft")
	if *ms.Clean != 1000 {
		t.Fatalf("total row must win: got %d", *ms.Clean)
	}
	if ap := dpISP(out, "apple"); *ap.Clean != 100 {
		t.Fatalf("no total row: sum of sources, got %d", *ap.Clean)
	}
	if ms.PctOfTarget == nil || *ms.PctOfTarget != 50 || ms.NewShare == nil || *ms.NewShare != 10 {
		t.Fatalf("pct_of_target 50 / new_share 10: got %v %v", ms.PctOfTarget, ms.NewShare)
	}
	if a := ms.Avg7d["first_mailed"]; a == nil || *a != 300 {
		t.Fatalf("7-day mean over days with a value: want 300, got %v", a)
	}
	if out.LatestDay == nil || *out.LatestDay != "2026-11-10" {
		t.Fatalf("latest_day: %v", out.LatestDay)
	}
	// first_mailed came only from microsoft → the lane total names the gap.
	if len(out.Degraded) == 0 {
		t.Fatal("partial lane totals must be named in degraded[]")
	}
}
