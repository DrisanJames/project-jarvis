package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
)

func onboardingManager(id *GoogleUserInfo) *AuthManager {
	am := NewAuthManager(&config.AuthConfig{
		GoogleClientSecret: "test-secret", AllowedDomain: "jamesventurescorp.com",
		CookieName: "jarvis_session", CookieMaxAge: 3600,
	}, "https://projectjarvis.io")
	am.identityFromCode = func(context.Context, string) (*GoogleUserInfo, error) { return id, nil }
	return am
}

func callback(am *AuthManager) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?state=abc&code=c", nil)
	req.AddCookie(&http.Cookie{Name: "oauth_state", Value: "abc"})
	rec := httptest.NewRecorder()
	am.HandleCallback(rec, req)
	return rec
}

func TestOnboardingTokenRoundTrip(t *testing.T) {
	am := onboardingManager(nil)
	now := time.Unix(1_800_000_000, 0)
	tok, err := am.IssueOnboardingToken(&GoogleUserInfo{ID: "1", Email: "A@Gmail.com", Name: "A", VerifiedEmail: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	c, err := am.VerifyOnboardingToken(tok, now.Add(29*time.Minute))
	if err != nil || c.Email != "a@gmail.com" {
		t.Fatalf("claims=%+v err=%v", c, err)
	}
	if _, err := am.VerifyOnboardingToken(tok, now.Add(31*time.Minute)); err == nil {
		t.Fatal("expired token accepted")
	}
	if _, err := am.IssueOnboardingToken(&GoogleUserInfo{Email: "a@gmail.com"}, now); err == nil {
		t.Fatal("issued a token for an unverified email")
	}
}

func TestOnboardingTokenForgery(t *testing.T) {
	am := onboardingManager(nil)
	now := time.Unix(1_800_000_000, 0)
	tok, _ := am.IssueOnboardingToken(&GoogleUserInfo{ID: "1", Email: "a@gmail.com", VerifiedEmail: true}, now)
	parts := strings.Split(tok, ".")

	raw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var c OnboardingClaims
	json.Unmarshal(raw, &c)
	c.Email = "drisan@jamesventurescorp.com"
	tampered, _ := json.Marshal(c)

	other := onboardingManager(nil)
	other.config.GoogleClientSecret = "different"
	foreign, _ := other.IssueOnboardingToken(&GoogleUserInfo{Email: "a@gmail.com", VerifiedEmail: true}, now)

	for name, bad := range map[string]string{
		"tampered payload": base64.RawURLEncoding.EncodeToString(tampered) + "." + parts[1],
		"foreign key":      foreign,
		"no signature":     parts[0],
		"empty":            "",
		"garbage":          "a.b.c",
	} {
		if _, err := am.VerifyOnboardingToken(bad, now); err == nil {
			t.Errorf("%s accepted", name)
		}
	}

	noSecret := onboardingManager(nil)
	noSecret.config.GoogleClientSecret = ""
	if _, err := noSecret.VerifyOnboardingToken(tok, now); err == nil {
		t.Error("verified with no secret configured")
	}
	if _, err := noSecret.IssueOnboardingToken(&GoogleUserInfo{Email: "a@gmail.com", VerifiedEmail: true}, now); err == nil {
		t.Error("issued with no secret configured")
	}
}

func TestDerivedKeysArePurposeBound(t *testing.T) {
	am := onboardingManager(nil)
	if string(am.DerivedKey("a")) == string(am.DerivedKey("b")) {
		t.Fatal("purposes share a key")
	}
}

// A verified outside account gets the form and nothing else.
func TestCallbackOutsideAccountGetsNoSession(t *testing.T) {
	for _, id := range []*GoogleUserInfo{
		{ID: "1", Email: "a@gmail.com", VerifiedEmail: true},
		{ID: "2", Email: "a@jamesventurescorp.com", VerifiedEmail: true}, // no hd
	} {
		am := onboardingManager(id)
		rec := callback(am)
		loc := rec.Header().Get("Location")
		if !strings.HasPrefix(loc, "/onboarding#t=") {
			t.Fatalf("%s: location %q", id.Email, loc)
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == "jarvis_session" {
				t.Fatalf("%s: session cookie set", id.Email)
			}
		}
		if len(am.sessions) != 0 {
			t.Fatalf("%s: session stored", id.Email)
		}

		// The token used as a session cookie authenticates nothing.
		token := strings.TrimPrefix(loc, "/onboarding#t=")
		for _, path := range []string{"/auth/user", "/api/mailing/campaigns"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.AddCookie(&http.Cookie{Name: "jarvis_session", Value: token})
			if am.IsAuthenticated(req) {
				t.Fatalf("%s: token worked as a session on %s", id.Email, path)
			}
		}
		req := httptest.NewRequest(http.MethodGet, "/auth/user", nil)
		req.AddCookie(&http.Cookie{Name: "jarvis_session", Value: token})
		w := httptest.NewRecorder()
		am.HandleUserInfo(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("/auth/user = %d", w.Code)
		}
		api := httptest.NewRecorder()
		apiReq := httptest.NewRequest(http.MethodGet, "/api/mailing/campaigns", nil)
		apiReq.Header.Set("Authorization", "Bearer "+token)
		am.RequireAuth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("reached the protected handler")
		})).ServeHTTP(api, apiReq)
		if api.Code != http.StatusUnauthorized {
			t.Fatalf("/api = %d", api.Code)
		}
	}
}

func TestCallbackUnverifiedEmailRefused(t *testing.T) {
	am := onboardingManager(&GoogleUserInfo{Email: "a@gmail.com"})
	rec := callback(am)
	if loc := rec.Header().Get("Location"); loc != "/sign-in?error=email_not_verified" {
		t.Fatalf("location %q", loc)
	}
}

func TestCallbackWorkspaceMemberStillSignsIn(t *testing.T) {
	am := onboardingManager(&GoogleUserInfo{ID: "1", Email: "a@jamesventurescorp.com", VerifiedEmail: true, HD: "jamesventurescorp.com"})
	rec := callback(am)
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Fatalf("location %q", loc)
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "jarvis_session" {
			session = c
		}
	}
	if session == nil || !session.Secure || !session.HttpOnly {
		t.Fatalf("session cookie %+v", session)
	}
	req := httptest.NewRequest(http.MethodGet, "/auth/user", nil)
	req.AddCookie(session)
	if !am.IsAuthenticated(req) {
		t.Fatal("not authenticated")
	}
}

func TestCallbackRejectsBadState(t *testing.T) {
	am := onboardingManager(&GoogleUserInfo{Email: "a@jamesventurescorp.com", VerifiedEmail: true, HD: "jamesventurescorp.com"})
	for name, req := range map[string]*http.Request{
		"mismatch": func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/auth/callback?state=x&code=c", nil)
			r.AddCookie(&http.Cookie{Name: "oauth_state", Value: "y"})
			return r
		}(),
		"both empty": func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/auth/callback?code=c", nil)
			r.AddCookie(&http.Cookie{Name: "oauth_state", Value: ""})
			return r
		}(),
		"no cookie": httptest.NewRequest(http.MethodGet, "/auth/callback?state=x&code=c", nil),
	} {
		rec := httptest.NewRecorder()
		am.HandleCallback(rec, req)
		if loc := rec.Header().Get("Location"); loc != "/sign-in?error=invalid_state" {
			t.Errorf("%s: location %q", name, loc)
		}
		if len(am.sessions) != 0 {
			t.Errorf("%s: session stored", name)
		}
	}
}
