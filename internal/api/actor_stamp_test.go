package api

// Step-13 fixtures (Vector A plan rev4): apiAuthMiddleware stamps the trusted
// actor identity. Permanent fixtures (I-11):
//   - session-authenticated request with a FORGED X-User-Email → the handler
//     sees the session email (the forgery is stripped).
//   - admin-key request with X-User-Email set → passes through unchanged
//     (trusted server-to-server).
//   - no session, no key → 401 and the handler never runs.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/auth"
	"github.com/ignite/sparkpost-monitor/internal/config"
)

// mintTestSession builds an AuthManager and mints a real session via the
// test-login path (the only exported session-creation seam), returning the
// manager, the session cookie, and the session's email.
func mintTestSession(t *testing.T) (*auth.AuthManager, *http.Cookie, string) {
	t.Helper()
	t.Setenv("TEST_ACCESS_TOKEN", "actor-stamp-test-token")
	am := auth.NewAuthManager(&config.AuthConfig{
		AllowedDomain: "jamesventurescorp.com",
		CookieName:    "jarvis_session",
		CookieMaxAge:  3600,
	}, "http://localhost:8080")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/auth/test-login", nil)
	req.Header.Set("X-Test-Token", "actor-stamp-test-token")
	am.HandleTestLogin(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("test-login mint failed: %d %s", rec.Code, rec.Body.String())
	}
	res := rec.Result()
	defer res.Body.Close()
	for _, c := range res.Cookies() {
		if c.Name == "jarvis_session" {
			return am, c, "test@jamesventurescorp.com"
		}
	}
	t.Fatal("test-login did not set the session cookie")
	return nil, nil, ""
}

func TestActorStampSessionStripsForgedHeader(t *testing.T) {
	am, cookie, sessionEmail := mintTestSession(t)

	var seen string
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-User-Email")
		w.WriteHeader(http.StatusOK)
	})
	h := apiAuthMiddleware(am, "test-admin-key")(probe)

	req := httptest.NewRequest("GET", "/api/anything", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-User-Email", "evil@x") // the forgery
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("session request rejected: %d", rec.Code)
	}
	if seen != sessionEmail {
		t.Fatalf("handler saw actor %q, want session email %q (forged header must be stripped)", seen, sessionEmail)
	}
}

func TestActorStampAdminKeyPassthrough(t *testing.T) {
	am, _, _ := mintTestSession(t)

	var seen string
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-User-Email")
		w.WriteHeader(http.StatusOK)
	})
	h := apiAuthMiddleware(am, "test-admin-key")(probe)

	req := httptest.NewRequest("GET", "/api/anything", nil)
	req.Header.Set("X-Admin-Key", "test-admin-key")
	req.Header.Set("X-User-Email", "svc@x")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("admin-key request rejected: %d", rec.Code)
	}
	if seen != "svc@x" {
		t.Fatalf("handler saw actor %q, want passthrough %q on the admin-key branch", seen, "svc@x")
	}
}

func TestActorStampUnauthenticated401(t *testing.T) {
	am, _, _ := mintTestSession(t)

	ran := false
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ran = true })
	h := apiAuthMiddleware(am, "test-admin-key")(probe)

	req := httptest.NewRequest("GET", "/api/anything", nil)
	req.Header.Set("X-User-Email", "evil@x")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request: got %d, want 401", rec.Code)
	}
	if ran {
		t.Fatal("handler ran on an unauthenticated request")
	}
}
