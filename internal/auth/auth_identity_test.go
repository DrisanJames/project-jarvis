package auth

import (
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/config"
)

func TestAllowedIdentity(t *testing.T) {
	const d = "jamesventurescorp.com"
	cases := []struct {
		name string
		u    *GoogleUserInfo
		want bool
	}{
		{"workspace member", &GoogleUserInfo{Email: "a@jamesventurescorp.com", VerifiedEmail: true, HD: d}, true},
		{"case-insensitive", &GoogleUserInfo{Email: "A@JamesVenturesCorp.com", VerifiedEmail: true, HD: "JamesVenturesCorp.com"}, true},
		{"personal gmail", &GoogleUserInfo{Email: "a@gmail.com", VerifiedEmail: true}, false},
		{"consumer account on company address (no hd)", &GoogleUserInfo{Email: "a@jamesventurescorp.com", VerifiedEmail: true}, false},
		{"other workspace hd", &GoogleUserInfo{Email: "a@jamesventurescorp.com", VerifiedEmail: true, HD: "evil.com"}, false},
		{"unverified email", &GoogleUserInfo{Email: "a@jamesventurescorp.com", VerifiedEmail: false, HD: d}, false},
		{"lookalike suffix", &GoogleUserInfo{Email: "a@xjamesventurescorp.com", VerifiedEmail: true, HD: "xjamesventurescorp.com"}, false},
		{"subdomain", &GoogleUserInfo{Email: "a@mail.jamesventurescorp.com", VerifiedEmail: true, HD: d}, false},
		{"two @", &GoogleUserInfo{Email: "a@b@jamesventurescorp.com", VerifiedEmail: true, HD: d}, false},
		{"empty local part", &GoogleUserInfo{Email: "@jamesventurescorp.com", VerifiedEmail: true, HD: d}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := allowedIdentity(c.u, d); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	if allowedIdentity(&GoogleUserInfo{Email: "a@jamesventurescorp.com", VerifiedEmail: true, HD: d}, "") {
		t.Error("empty allowed domain must reject everything")
	}
}

func TestHandleLogin_GenericChooserNoDomainHint(t *testing.T) {
	am := NewAuthManager(&config.AuthConfig{GoogleClientID: "cid", AllowedDomain: "jamesventurescorp.com"}, "https://example.test")
	rec := httptest.NewRecorder()
	am.HandleLogin(rec, httptest.NewRequest("GET", "/auth/login", nil))
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	if q.Has("hd") {
		t.Errorf("login URL must not carry hd=: %s", loc)
	}
	if q.Get("prompt") != "select_account" {
		t.Errorf("prompt = %q, want select_account", q.Get("prompt"))
	}
	if q.Get("state") == "" {
		t.Error("state missing")
	}
}
