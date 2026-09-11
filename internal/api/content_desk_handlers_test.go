package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/contentdesk"
)

const cdOrg = "00000000-0000-0000-0000-000000000001"

// newCDRouter mirrors production: session routes inside a mounted
// /api/mailing group, admin routes on the same ROOT router.
func newCDRouter(t *testing.T) (http.Handler, sqlmock.Sqlmock, *ContentDeskService) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc := NewContentDeskService(db)
	svc.now = func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }
	svc.serverSignals = func() contentdesk.ServerSignals {
		return contentdesk.ServerSignals{TrackSig: &contentdesk.TrackSigState{Mode: "shadow", Keys: 1, Counters: map[string]int64{"open.sig.invalid": 2}},
			PreferencesModeSet: true, PreferencesMode: "shadow"}
	}
	r := chi.NewRouter()
	r.Route("/api/mailing", func(mr chi.Router) { svc.RegisterRoutes(mr) })
	svc.RegisterAdminRoutes(r)
	return r, mock, svc
}

func cdDo(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Organization-ID", cdOrg)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestContentDeskReview_HashMismatchIs409(t *testing.T) {
	h, mock, _ := newCDRouter(t)
	art := "33333333-3333-3333-3333-333333333333"
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FOR UPDATE OF a")).WithArgs(art, cdOrg).
		WillReturnRows(sqlmock.NewRows([]string{"status", "rid", "hash", "checks", "cons"}).
			AddRow("in_review", "rev1", strings.Repeat("b", 64), []byte(`{}`), false))
	mock.ExpectRollback()
	body := `{"revision_hash":"` + strings.Repeat("a", 64) + `","reviewer":"ann","role":"primary","decision":"approve"}`
	rec := cdDo(h, http.MethodPost, "/api/mailing/content-desk/articles/"+art+"/reviews", body, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestContentDeskAdminRoutes_ClosedByDefault(t *testing.T) {
	h, mock, _ := newCDRouter(t)
	report := `{"source":"site_probes","generated_at":"2026-09-11T11:00:00Z","checks":[{"name":"db","status":"ok","detail":"200"}]}`
	paths := []struct{ method, path, body string }{
		{http.MethodPost, "/api/mailing/content-desk/ops-report", report},
		{http.MethodGet, "/api/mailing/content-desk/releases/manifest?site=44444444-4444-4444-4444-444444444444", ""},
		{http.MethodPost, "/api/mailing/content-desk/releases", `{"site_id":"x"}`},
		{http.MethodPost, "/api/mailing/content-desk/releases/abc/status", `{"status":"building"}`},
	}
	t.Setenv("ADMIN_API_KEY", "")
	for _, p := range paths {
		if rec := cdDo(h, p.method, p.path, p.body, map[string]string{"X-Admin-Key": ""}); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with ADMIN_API_KEY unset: want 401, got %d", p.method, p.path, rec.Code)
		}
	}
	t.Setenv("ADMIN_API_KEY", "right")
	for _, p := range paths {
		if rec := cdDo(h, p.method, p.path, p.body, map[string]string{"X-Admin-Key": "wrong"}); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s wrong key: want 401, got %d", p.method, p.path, rec.Code)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("an unauthorized call must not touch the DB: %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO content_ops_reports")).
		WithArgs(cdOrg, "site_probes", sqlmock.AnyArg(), sqlmock.AnyArg(), nil).
		WillReturnRows(sqlmock.NewRows([]string{"received_at"}).AddRow(time.Now()))
	rec := cdDo(h, http.MethodPost, "/api/mailing/content-desk/ops-report", report, map[string]string{"X-Admin-Key": "right"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("right key: want 202, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := cdDo(h, http.MethodPost, "/api/mailing/content-desk/ops-report", `{"source":"x","generated_at":"2026-09-11T11:00:00Z","checks":[{"name":"a","status":"great"}]}`,
		map[string]string{"X-Admin-Key": "right"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid check status: want 400, got %d", rec.Code)
	}
}

func TestContentDeskOpsStatus_Shape(t *testing.T) {
	t.Setenv(contentdesk.EnvEnabled, "")
	t.Setenv(contentdesk.EnvDailyUSD, "")
	h, mock, _ := newCDRouter(t)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT stage, status, COUNT(*) FROM content_pipeline_runs")).WithArgs(cdOrg).
		WillReturnRows(sqlmock.NewRows([]string{"stage", "status", "n"}).AddRow("draft", "succeeded", 3).AddRow("judgment", "failed", 1))
	mock.ExpectQuery(regexp.QuoteMeta("status = 'in_review'")).
		WillReturnRows(sqlmock.NewRows([]string{"n", "age"}).AddRow(2, 3600.0))
	mock.ExpectQuery(regexp.QuoteMeta("LEFT JOIN LATERAL")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "domain", "rid", "status", "err", "st", "fin"}).
			AddRow("s1", "discountblog.com", "r1", "live_confirmed", "", now, now).
			AddRow("s2", "hfcl.net", "", "", "", nil, nil))
	mock.ExpectQuery(regexp.QuoteMeta("to_char(")).WillReturnRows(sqlmock.NewRows([]string{"day", "usd"}).AddRow("2026-09-11", 4.5))
	mock.ExpectQuery(regexp.QuoteMeta("status = 'failed' ORDER BY finished_at DESC")).
		WillReturnRows(sqlmock.NewRows([]string{"a", "s", "e", "f"}).AddRow("art1", "judgment", "judgment: model refused", now.Add(-2*time.Hour)))
	mock.ExpectQuery(regexp.QuoteMeta("FROM content_ops_reports")).WithArgs(cdOrg).
		WillReturnRows(sqlmock.NewRows([]string{"source", "generated_at", "checks", "payload", "received_at"}).
			AddRow("job_watchdog", now.Add(-time.Hour), []byte(`[{"name":"nightly","status":"ok","detail":"12/12"}]`), []byte("null"), now).
			AddRow("site_probes", now.Add(-5*time.Hour), []byte(`[{"name":"db","status":"ok"}]`), []byte("null"), now).
			AddRow("supply_runway", now.Add(-time.Hour), []byte(`[]`), []byte(`{"consumers":[{"lane":"yahoo_family","days":4}]}`), now))

	rec := cdDo(h, http.MethodGet, "/api/mailing/content-desk/ops-status", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"pipeline", "review_queue", "releases", "spend", "kill_switch", "models", "last_pipeline_error",
		"newsletter_supply_runway", "newsletter_supply_runway_reason", "checks", "supply"} {
		if _, ok := got[k]; !ok {
			t.Errorf("ops-status missing %q", k)
		}
	}
	if string(got["newsletter_supply_runway"]) != "null" {
		t.Errorf("runway must be null with a reason, got %s", got["newsletter_supply_runway"])
	}
	var o contentdesk.OpsStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &o); err != nil {
		t.Fatal(err)
	}
	if o.ReviewQueue.Size != 2 || o.Spend.TodayUSD != 4.5 || o.Spend.BudgetUSD != 25 || o.KillSwitch.Enabled || o.Models.Judge != "claude-opus-5" {
		t.Fatalf("ops-status values wrong: %+v", o)
	}
	if o.LastPipelineError == nil || o.LastPipelineError.Stage != "judgment" || len(o.Releases) != 2 || len(o.Pipeline) != 2 {
		t.Fatalf("ops-status rows wrong: %+v", o)
	}
	status := map[string]string{}
	for _, c := range o.Checks {
		status[c.Name] = c.Status
	}
	for name, want := range map[string]string{
		"server: track_sig": "ok", "server: content_desk_enabled": "warn", "server: budget": "ok",
		"server: last_pipeline_error": "warn", "server: preferences_mode": "ok",
		"job_watchdog: nightly": "ok", "site_probes: stale report (last 5h0m)": "warn",
	} {
		if status[name] != want {
			t.Errorf("check %q = %q, want %q (all: %v)", name, status[name], want, status)
		}
	}
	if string(o.Supply.Consumers) != `[{"lane":"yahoo_family","days":4}]` {
		t.Errorf("supply.consumers = %s", o.Supply.Consumers)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
