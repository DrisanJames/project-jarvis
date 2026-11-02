package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func sessionRequest(t *testing.T, am *AuthManager, email string) *http.Request {
	t.Helper()
	am.storeSession("sid", &Session{Email: email, ExpiresAt: time.Now().Add(time.Hour)})
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.AddCookie(&http.Cookie{Name: am.config.CookieName, Value: "sid"})
	return req
}

func TestAccessGate_DisabledDeniedAtLogin(t *testing.T) {
	am := onboardingManager(&GoogleUserInfo{Email: "a@jamesventurescorp.com", VerifiedEmail: true, HD: "jamesventurescorp.com"})
	am.SetAccessGate(func(string) bool { return false })
	rec := callback(am)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=access_disabled") {
		t.Fatalf("redirect %q", loc)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "jarvis_session" {
			t.Fatal("disabled user got a session cookie")
		}
	}
	if len(am.sessions) != 0 {
		t.Fatal("disabled user got a session")
	}
}

func TestAccessGate_AllowedUserStillLogsIn(t *testing.T) {
	am := onboardingManager(&GoogleUserInfo{Email: "a@jamesventurescorp.com", VerifiedEmail: true, HD: "jamesventurescorp.com"})
	am.SetAccessGate(func(string) bool { return true })
	callback(am)
	if len(am.sessions) != 1 {
		t.Fatal("no session")
	}
}

func TestAccessGate_DisabledDeniedInRequireAuth(t *testing.T) {
	am := onboardingManager(nil)
	reached := false
	h := am.RequireAuth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	req := sessionRequest(t, am, "a@jamesventurescorp.com")

	am.SetAccessGate(func(string) bool { return false })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || reached {
		t.Fatalf("got %d, reached=%v", rec.Code, reached)
	}
	if am.GetSession(req) != nil || am.IsAuthenticated(req) {
		t.Fatal("open session survived the gate")
	}

	am.SetAccessGate(func(string) bool { return true })
	if am.GetSession(req) == nil {
		t.Fatal("re-enabled user lost the session")
	}
}

func TestAccessGate_NoGateChangesNothing(t *testing.T) {
	am := onboardingManager(nil)
	if am.GetSession(sessionRequest(t, am, "a@jamesventurescorp.com")) == nil {
		t.Fatal("a session needs no gate")
	}
}

func TestEmailAllowed(t *testing.T) {
	am := onboardingManager(nil)
	for email, want := range map[string]bool{
		"a@jamesventurescorp.com":           true,
		" A@JamesVenturesCorp.com ":         true,
		"a@gmail.com":                       false,
		"@jamesventurescorp.com":            false,
		"a@b@jamesventurescorp.com":         false,
		"a@jamesventurescorp.com.evil.test": false,
		"":                                  false,
	} {
		if got := am.EmailAllowed(email); got != want {
			t.Errorf("%q = %v", email, got)
		}
	}
	var nilManager *AuthManager
	if nilManager.EmailAllowed("a@jamesventurescorp.com") {
		t.Fatal("nil manager allowed an email")
	}
}

func TestAccessGate_ArmedWhileRequestsAreServed(t *testing.T) {
	am := onboardingManager(nil)
	req := sessionRequest(t, am, "a@jamesventurescorp.com")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); am.SetAccessGate(func(string) bool { return true }) }()
		go func() { defer wg.Done(); am.GetSession(req) }()
	}
	wg.Wait()
}
