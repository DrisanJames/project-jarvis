package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The 2026-08-06 incident in one test: a POST to a real mailing endpoint that
// arrives before SetMailingDB has registered the route tree must NOT be told
// "404 page not found" (19 bytes, indistinguishable from "endpoint does not
// exist"). It must get a retryable 503 so the caller stops and retries instead
// of silently discarding a board deploy.
func TestUnmatchedAPIRoute_BeforeRoutesRegistered_Returns503(t *testing.T) {
	resetMailingRoutesReadyForTest()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", strings.NewReader("{}"))
	handleUnmatchedRoute(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 while routes are registering, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("503 must carry Retry-After so clients back off rather than treat it as permanent")
	}
	if body := rec.Body.String(); !strings.Contains(body, "starting up") {
		t.Fatalf("body should explain the startup window, got %q", body)
	}
	// The exact failure signature from the incident: Go's http.NotFound writes
	// "404 page not found\n" — 19 bytes. That must never be the answer here.
	if rec.Body.Len() == 19 && strings.Contains(rec.Body.String(), "404 page not found") {
		t.Fatal("regression: still emitting the 19-byte router 404 that ate 25 deploy POSTs")
	}
}

// Once registration completes, unmatched paths must go back to honest 404s —
// the 503 is a startup window, not a permanent mask over real routing errors.
func TestUnmatchedAPIRoute_AfterRoutesRegistered_Returns404(t *testing.T) {
	resetMailingRoutesReadyForTest()
	MarkMailingRoutesReady()
	t.Cleanup(resetMailingRoutesReadyForTest)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/does-not-exist", nil)
	handleUnmatchedRoute(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 once routes are registered, got %d", rec.Code)
	}
}

// Non-API paths (SPA routes, static assets) must 404 normally even during the
// startup window — otherwise a browser hitting the app mid-deploy would see
// 503s for ordinary page loads.
func TestUnmatchedNonAPIRoute_AlwaysReturns404(t *testing.T) {
	resetMailingRoutesReadyForTest()
	t.Cleanup(resetMailingRoutesReadyForTest)

	for _, path := range []string{"/assets/app-abc123.js", "/favicon.ico"} {
		rec := httptest.NewRecorder()
		handleUnmatchedRoute(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: expected 404 during startup, got %d", path, rec.Code)
		}
	}
}

func TestAPIPathNeedingRoutes(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/mailing/pmta-campaign/deploy": true,
		"/api/campaigns":                    true,
		"/health":                           false,
		"/assets/x.js":                      false,
		"/":                                 false,
	} {
		if got := apiPathNeedingRoutes(path); got != want {
			t.Errorf("apiPathNeedingRoutes(%q) = %v, want %v", path, got, want)
		}
	}
}

// Readiness must distinguish "still starting" from "healthy", so a deploy
// script can wait for a task to actually serve mailing routes.
func TestReadiness_NotReadyUntilRoutesRegistered(t *testing.T) {
	resetMailingRoutesReadyForTest()
	t.Cleanup(resetMailingRoutesReadyForTest)

	hc := NewHealthChecker(nil, nil, nil, "")

	rec := httptest.NewRecorder()
	hc.HandleReadiness(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 before routes register, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"registered":false`) {
		t.Fatalf("readiness body should expose mailing_routes.registered=false, got %q", rec.Body.String())
	}

	MarkMailingRoutesReady()
	rec2 := httptest.NewRecorder()
	hc.HandleReadiness(rec2, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if !strings.Contains(rec2.Body.String(), `"registered":true`) {
		t.Fatalf("readiness should report registered=true after registration, got %q", rec2.Body.String())
	}
}

// The ALB's /health must stay an unconditional 200 during registration.
// Gating it would make ECS deregister and kill tasks that take >10 minutes to
// register routes, converting a silent-404 window into a crash loop.
func TestALBHealth_StaysHealthyDuringRegistration(t *testing.T) {
	resetMailingRoutesReadyForTest()
	t.Cleanup(resetMailingRoutesReadyForTest)

	hc := NewHealthChecker(nil, nil, nil, "")
	rec := httptest.NewRecorder()
	hc.HandleHealth(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("ALB /health must stay 200 during route registration (else ECS kills the task), got %d", rec.Code)
	}
}
