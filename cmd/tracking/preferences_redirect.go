package main

import (
	"net/http"
	"os"
	"strings"
)

// preferencesBaseURL is where the API serves the preference page.
func preferencesBaseURL() string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("PREFERENCES_BASE_URL")), "/"); v != "" {
		return v
	}
	return "https://projectjarvis.io"
}

// withPreferencesRedirect serves GET/HEAD /track/preferences on every brand
// tracking host by redirecting to the API preference page, query preserved
// (the signed ?t= token). The ALB forwards only /track/* and /o/* to this
// service — a bare /preferences on a trk./t.em. host is 403'd at the edge —
// so the send path mints {trackBase}/track/preferences?t=… links and this
// hop carries them to the page. Wrapping (rather than a route inside
// internal/tracking) keeps the tracking handler and its link formats
// untouched. Every other request passes through unchanged.
func withPreferencesRedirect(next http.Handler, base string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/track/preferences" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			target := base + "/preferences"
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}
