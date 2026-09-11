package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPreferencesRedirect(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := withPreferencesRedirect(next, "https://api.example")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://t.em.x.com/track/preferences?t=abc.def", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://api.example/preferences?t=abc.def" {
		t.Fatalf("redirect: code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("token must not leak via Referer")
	}

	// Everything else — including the live /track/unsubscribe format — passes through.
	for _, p := range []string{"/track/unsubscribe/abc/def", "/track/click/a/b", "/o/x/y/z", "/track/preferencesX"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusTeapot {
			t.Fatalf("%s was intercepted (code %d)", p, rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/track/preferences", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatal("POST /track/preferences must pass through")
	}
}

func TestPreferencesBaseURL_Default(t *testing.T) {
	t.Setenv("PREFERENCES_BASE_URL", "")
	if preferencesBaseURL() != "https://projectjarvis.io" {
		t.Fatal(preferencesBaseURL())
	}
	t.Setenv("PREFERENCES_BASE_URL", "https://x.example/")
	if preferencesBaseURL() != "https://x.example" {
		t.Fatal(preferencesBaseURL())
	}
}
