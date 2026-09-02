package api

// Global-suppression-hub readiness.
//
// The hub is built inside the SAME background goroutine that registers the
// /api/mailing/* tree (server_routes_mailing.go), and it is what every send
// pipeline consults before a message goes out: mailingSvc, campaignBuilder,
// pmtaCampaignAPI, the send-day scrub import, and (via s.GlobalHub, exported to
// cmd/server) SendWorkerPool.SetGlobalSuppressionHub.
//
// If that goroutine dies or stalls before the hub is wired, the server still
// answers /health with an unconditional 200 and nothing anywhere says the
// suppression check is absent. This flag makes the wiring observable, in the
// same shape and for the same reason as mailing_routes.registered
// (mailing_routes_readiness.go, incident 2026-08-06).
//
// It reports WIRING, not hub contents — false is a hard "suppression is not
// consulted on this task yet"; true is not a claim about list freshness.

import "sync/atomic"

// suppressionHubWired flips true once the global suppression hub has been
// constructed and exported to the send pipelines. Atomic: set on the route
// registration goroutine, read on /health request goroutines.
var suppressionHubWired atomic.Bool

// MarkSuppressionHubWired records that the global suppression hub is live.
func MarkSuppressionHubWired() { suppressionHubWired.Store(true) }

// SuppressionHubWired reports whether the global suppression hub is wired.
func SuppressionHubWired() bool { return suppressionHubWired.Load() }

// resetSuppressionHubWiredForTest restores the zero value between tests.
func resetSuppressionHubWiredForTest() { suppressionHubWired.Store(false) }
