package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/auth"
	"github.com/ignite/sparkpost-monitor/internal/config"
)

type recordingNotifier struct {
	subjects, bodies []string
	err              error
}

func (n *recordingNotifier) Send(_ context.Context, subject, body string) error {
	n.subjects = append(n.subjects, subject)
	n.bodies = append(n.bodies, body)
	return n.err
}

var leadTestNow = time.Unix(1_800_000_000, 0)

func testLeads(t *testing.T) (*marketingLeads, sqlmock.Sqlmock, *auth.AuthManager) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	am := auth.NewAuthManager(&config.AuthConfig{GoogleClientSecret: "s", AllowedDomain: "jamesventurescorp.com", CookieName: "jarvis_session"}, "https://projectjarvis.io")
	m := &marketingLeads{now: func() time.Time { return leadTestNow }, wake: make(chan struct{}, 1), notifier: &recordingNotifier{}}
	m.setAuth(am)
	m.db.Store(db)
	return m, mock, am
}

func validToken(t *testing.T, m *marketingLeads) string {
	t.Helper()
	tok, err := m.issueFormToken(leadTestNow.Add(-10 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func post(t *testing.T, h http.HandlerFunc, path string, body map[string]interface{}, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	req.Header.Set("Origin", "https://projectjarvis.io")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	for _, f := range mutate {
		f(req)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func goodLead(tok string) map[string]interface{} {
	return map[string]interface{}{
		"form_token": tok, "kind": "early_access", "email": "a@example.com",
		"use_case": "Newsletters", "source_page": "/early-access",
	}
}

func expectRate(mock sqlmock.Sqlmock, ip, global int) {
	q := `INSERT INTO marketing_lead_rate`
	mock.ExpectQuery(q).WillReturnRows(sqlmock.NewRows([]string{"hits"}).AddRow(ip))
	if ip <= leadPerIPHourly {
		mock.ExpectQuery(q).WillReturnRows(sqlmock.NewRows([]string{"hits"}).AddRow(global))
	}
}

func TestLeadSavedThenSuccess(t *testing.T) {
	m, mock, _ := testLeads(t)
	expectRate(mock, 1, 1)
	mock.ExpectExec(`INSERT INTO marketing_leads`).WillReturnResult(sqlmock.NewResult(1, 1))
	rec := post(t, m.HandleLead, "/api/public/leads", goodLead(validToken(t, m)))
	if rec.Code != 200 {
		t.Fatalf("code %d %s", rec.Code, rec.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.wake:
	default:
		t.Fatal("notification sweep not woken")
	}
}

// Every bot layer answers like a success, and touches no table.
func TestCaughtBotsGetSuccessWithNoWrite(t *testing.T) {
	m, mock, _ := testLeads(t)
	young, _ := m.issueFormToken(leadTestNow.Add(-1 * time.Second))
	withBody := func(mut func(map[string]interface{})) map[string]interface{} {
		b := goodLead(validToken(t, m))
		mut(b)
		return b
	}
	cases := map[string]*httptest.ResponseRecorder{
		"honeypot":   post(t, m.HandleLead, "/x", withBody(func(b map[string]interface{}) { b["website"] = "http://spam" })),
		"forged":     post(t, m.HandleLead, "/x", withBody(func(b map[string]interface{}) { b["form_token"] = "1.2.3" })),
		"no token":   post(t, m.HandleLead, "/x", withBody(func(b map[string]interface{}) { delete(b, "form_token") })),
		"too fast":   post(t, m.HandleLead, "/x", withBody(func(b map[string]interface{}) { b["form_token"] = young })),
		"bad origin": post(t, m.HandleLead, "/x", goodLead(validToken(t, m)), func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }),
		"no origin":  post(t, m.HandleLead, "/x", goodLead(validToken(t, m)), func(r *http.Request) { r.Header.Del("Origin") }),
	}
	for name, rec := range cases {
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.wake:
		t.Fatal("a bot woke the mailer")
	default:
	}
}

func TestStaleTokenIsNotFakeSuccess(t *testing.T) {
	m, _, _ := testLeads(t)
	old, _ := m.issueFormToken(leadTestNow.Add(-3 * time.Hour))
	rec := post(t, m.HandleLead, "/x", goodLead(old))
	if rec.Code != http.StatusConflict {
		t.Fatalf("code %d", rec.Code)
	}
}

func TestRateLimits(t *testing.T) {
	m, mock, _ := testLeads(t)
	expectRate(mock, leadPerIPHourly+1, 0)
	if rec := post(t, m.HandleLead, "/x", goodLead(validToken(t, m))); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("per ip: %d", rec.Code)
	}
	expectRate(mock, 1, leadGlobalHourly+1)
	if rec := post(t, m.HandleLead, "/x", goodLead(validToken(t, m))); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("global: %d", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestClientIPIgnoresSpoofedForwardedFor(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "1.1.1.1, 2.2.2.2, 203.0.113.9")
	if got := leadClientIP(req); got != "203.0.113.9" {
		t.Fatal(got)
	}
}

func TestSaveFailureIsNeverSuccess(t *testing.T) {
	m, mock, _ := testLeads(t)
	expectRate(mock, 1, 1)
	mock.ExpectExec(`INSERT INTO marketing_leads`).WillReturnError(errors.New("db down"))
	if rec := post(t, m.HandleLead, "/x", goodLead(validToken(t, m))); rec.Code != 500 {
		t.Fatalf("code %d", rec.Code)
	}
	mock.ExpectQuery(`INSERT INTO marketing_lead_rate`).WillReturnError(errors.New("db down"))
	if rec := post(t, m.HandleLead, "/x", goodLead(validToken(t, m))); rec.Code != 500 {
		t.Fatalf("rate failure code %d", rec.Code)
	}
}

func TestLeadValidation(t *testing.T) {
	m, mock, _ := testLeads(t)
	for name, mut := range map[string]func(map[string]interface{}){
		"bad email":     func(b map[string]interface{}) { b["email"] = "not-an-email" },
		"header inject": func(b map[string]interface{}) { b["email"] = "a@example.com\r\nBcc: x@y.z" },
		"name newline":  func(b map[string]interface{}) { b["name"] = "a\nEmail: forged@x.y" },
		"too long":      func(b map[string]interface{}) { b["company"] = strings.Repeat("x", 161) },
		"missing use":   func(b map[string]interface{}) { delete(b, "use_case") },
		"unknown kind":  func(b map[string]interface{}) { b["kind"] = "admin" },
		"onboarding":    func(b map[string]interface{}) { b["kind"] = "onboarding" },
		"unknown field": func(b map[string]interface{}) { b["organization_id"] = "x" },
		"external page": func(b map[string]interface{}) { b["source_page"] = "https://evil" },
	} {
		b := goodLead(validToken(t, m))
		mut(b)
		if rec := post(t, m.HandleLead, "/x", b); rec.Code != 400 {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	big := goodLead(validToken(t, m))
	big["notes"] = strings.Repeat("x", leadBodyLimit+1)
	if rec := post(t, m.HandleLead, "/x", big); rec.Code != 400 {
		t.Errorf("body cap: %d", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestOnboardingRequiresGoogleToken(t *testing.T) {
	m, mock, am := testLeads(t)
	body := func(tok string) map[string]interface{} {
		return map[string]interface{}{
			"form_token": validToken(t, m), "onboarding_token": tok, "name": "A", "company": "Co",
			"volume": "Under 10,000", "use_case": "Newsletters", "email": "attacker@evil.example",
		}
	}
	id := &auth.GoogleUserInfo{ID: "g1", Email: "owner@gmail.com", VerifiedEmail: true}
	expired, _ := am.IssueOnboardingToken(id, leadTestNow.Add(-31*time.Minute))
	for name, tok := range map[string]string{"forged": "abc.def", "empty": "", "expired": expired} {
		if rec := post(t, m.HandleOnboarding, "/x", body(tok)); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	good, _ := am.IssueOnboardingToken(id, leadTestNow)
	expectRate(mock, 1, 1)
	// Saved under the Google email, never the one in the body, keyed so a
	// second submission from that account is a no-op.
	mock.ExpectExec(`INSERT INTO marketing_leads`).WithArgs(
		"onboarding", "owner@gmail.com", "A", "Co", "", "", "Under 10,000", "Newsletters", "", "", "", false,
		"g1", sqlmock.AnyArg(), sqlmock.AnyArg(), "onboarding|owner@gmail.com",
	).WillReturnResult(sqlmock.NewResult(1, 1))
	if rec := post(t, m.HandleOnboarding, "/x", body(good)); rec.Code != 200 {
		t.Fatalf("good token: %d %s", rec.Code, rec.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// An outside account has no way into the API: the real middleware rejects the
// onboarding token in every position it could be presented.
func TestOnboardingTokenDoesNotOpenAPI(t *testing.T) {
	_, _, am := testLeads(t)
	tok, _ := am.IssueOnboardingToken(&auth.GoogleUserInfo{Email: "a@gmail.com", VerifiedEmail: true}, time.Now())
	h := apiAuthMiddleware(am, "admin-key")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("reached a protected handler")
	}))
	for _, mut := range []func(*http.Request){
		func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "jarvis_session", Value: tok}) },
		func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) },
		func(r *http.Request) { r.Header.Set("X-Admin-Key", tok) },
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/mailing/campaigns", nil)
		mut(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("code %d", rec.Code)
		}
	}
}

func TestFlushMarksSentAndRecordsFailure(t *testing.T) {
	m, mock, _ := testLeads(t)
	n := m.notifier.(*recordingNotifier)
	cols := []string{"id", "kind", "email", "name", "company", "role", "website_url", "volume", "use_case",
		"current_platform", "notes", "source_page", "marketing_opt_in", "created_at"}
	row := func() *sqlmock.Rows {
		return sqlmock.NewRows(cols).AddRow("1", "demo", "a@example.com", "A", "Co", "", "", "Under 10,000", "Newsletters", "", "hi", "/demo", false, leadTestNow)
	}

	mock.ExpectQuery(`UPDATE marketing_leads SET notify_attempts`).WillReturnRows(row())
	mock.ExpectExec(`SET notified_at = NOW`).WithArgs("1").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := m.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n.subjects[0] != leadSubjects["demo"] || !strings.Contains(n.bodies[0], "Email: a@example.com") {
		t.Fatalf("mail %q %q", n.subjects, n.bodies)
	}

	n.err = errors.New("ses down")
	mock.ExpectQuery(`UPDATE marketing_leads SET notify_attempts`).WillReturnRows(row())
	mock.ExpectExec(`SET notify_error`).WithArgs("1", "ses down").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := m.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAdminListNeedsKey(t *testing.T) {
	m, _, _ := testLeads(t)
	t.Setenv("ADMIN_API_KEY", "k")
	rec := httptest.NewRecorder()
	m.HandleAdminList(rec, httptest.NewRequest(http.MethodGet, "/api/admin/marketing-leads", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code %d", rec.Code)
	}
	t.Setenv("ADMIN_API_KEY", "")
	req := httptest.NewRequest(http.MethodGet, "/api/admin/marketing-leads", nil)
	req.Header.Set("X-Admin-Key", "")
	rec = httptest.NewRecorder()
	m.HandleAdminList(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty key: %d", rec.Code)
	}
}

func TestSpaHandlerRoutes(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "_prerender"), 0o755)
	os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	write := func(name, body string) { os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644) }
	write("index.html", "shell")
	write("robots.txt", "robots")
	write("_prerender/home.html", "home page")
	write("_prerender/platform.html", "platform page")
	write("_prerender/not-found.html", "not found page")
	r := chi.NewRouter()
	spaHandler(r, dir)

	for _, c := range []struct {
		path, want string
		code       int
	}{
		{"/", "home page", 200},
		{"/platform", "platform page", 200},
		{"/platform/", "platform page", 200},
		{"/robots.txt", "robots", 200},
		{"/no-such-page", "not found page", 404},
		{"/a/b/c", "not found page", 404},
		{"/_prerender/", "not found page", 404},
		{"/assets/missing.js", "404 page not found\n", 404},
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != c.code || rec.Body.String() != c.want {
			t.Errorf("%s: %d %q", c.path, rec.Code, rec.Body)
		}
	}
	for from, to := range marketingRedirects {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, from, nil))
		if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != to {
			t.Errorf("%s: %d %q", from, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// Only the named paths are public: everything else under /api still needs a
// session.
func TestPublicRoutesAreOnlyTheNamedOnes(t *testing.T) {
	src, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatal(err)
	}
	found := regexp.MustCompile(`"/api/public/[^"]*"`).FindAllString(string(src), -1)
	if len(found) != 3 {
		t.Fatalf("public routes: %v", found)
	}
}
