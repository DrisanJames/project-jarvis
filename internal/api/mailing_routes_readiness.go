package api

// Mailing-route readiness (incident 2026-08-06/07).
//
// SetMailingDB registers every /api/mailing/* route, and cmd/server/main.go
// runs it in a BACKGROUND goroutine because it is heavy ("can take >10
// minutes"). Meanwhile the HTTP listener is already open and /health returns
// an unconditional 200, so ECS puts the task in the ALB target group and real
// traffic arrives before the routes exist. chi then answers with its default
// `404 page not found` — 19 bytes, ~30µs, no handler run.
//
// That is indistinguishable from "this endpoint does not exist", so clients
// treat a transient startup window as a permanent negative. On 2026-08-06 the
// operator's board approval fired 31 POSTs to /api/mailing/pmta-campaign/deploy
// during a rolling deploy: 4 succeeded, 25 were silently 404'd, 2 timed out.
// The undeployed campaigns were only caught up 10.5h later, by which time the
// OFR-CLK (window→15:01) and OFR-ENG (→17:01) wave windows had expired and the
// wave janitor cancelled 6,212 waves. 227k planned recipients never mailed.
//
// Fix: track whether route registration has completed and answer /api/* with
// 503 + Retry-After (a RETRYABLE signal) instead of 404 while it is pending.
//
// Why not gate the ALB's /health on this: ECS would deregister and kill tasks
// that take longer than the health-check grace period to register routes,
// turning a silent-404 window into a crash loop. /health stays an unconditional
// 200; /health/ready reports the real state for deploy scripts and operators.

import (
	"net/http"
	"strings"
	"sync/atomic"
)

// mailingRoutesReady flips to true once SetMailingDB has finished registering
// the /api/mailing/* tree. Atomic: set on the registration goroutine, read on
// every request goroutine.
var mailingRoutesReady atomic.Bool

// MarkMailingRoutesReady records that /api/mailing/* is now served. Called at
// the end of SetMailingDB.
func MarkMailingRoutesReady() { mailingRoutesReady.Store(true) }

// MailingRoutesReady reports whether the mailing route tree is registered.
func MailingRoutesReady() bool { return mailingRoutesReady.Load() }

// resetMailingRoutesReadyForTest restores the zero value between tests.
func resetMailingRoutesReadyForTest() { mailingRoutesReady.Store(false) }

// apiPathNeedingRoutes reports whether an unmatched path should be treated as
// "still starting up" rather than "does not exist". Scoped to /api because the
// SPA and static assets legitimately 404.
func apiPathNeedingRoutes(path string) bool {
	return strings.HasPrefix(path, "/api/")
}

// writeRoutesInitializing emits the retryable startup response. 503 +
// Retry-After is the correct semantic: the resource exists but is not yet
// being served, and the caller should try again rather than conclude absence.
func writeRoutesInitializing(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "15")
	respondError(w, http.StatusServiceUnavailable,
		"server is still starting up (mailing routes registering) — retry shortly")
}

// handleUnmatchedRoute is the router's NotFound handler: 503 for /api/* while
// registration is pending, ordinary 404 otherwise.
func handleUnmatchedRoute(w http.ResponseWriter, r *http.Request) {
	if !MailingRoutesReady() && apiPathNeedingRoutes(r.URL.Path) {
		writeRoutesInitializing(w)
		return
	}
	http.NotFound(w, r)
}
