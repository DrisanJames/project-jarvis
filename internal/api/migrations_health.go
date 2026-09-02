package api

import "sync/atomic"

// =============================================================================
// /health "migrations" block (REQ-092 DoD 7)
// =============================================================================
// runStartupMigrations logs its outcome and nothing else, so a boot that fails
// 39 entries and times out 2 more looks identical to a clean one unless someone
// reads CloudWatch by hand. That is how mailing_tracking_events.email and
// idx_mte_click_verdict stayed absent for weeks while every boot dutifully
// logged "skipped — will retry next boot" (deploy-config SEV-2).
//
// Same shape as the event-bus block: cmd/server installs a provider after the
// migration slice finishes; reads go through an atomic pointer so the late
// wiring is race-free and a nil provider yields a zero (never-ran) status.

// MigrationsStatus is the whole /health "migrations" block.
type MigrationsStatus struct {
	// Ran is false until runStartupMigrations completes on this task — a task
	// that lost the advisory lock to its sibling never sets it.
	Ran bool `json:"ran"`
	// OK, Skipped (already applied, per the catalog probe), Timeout (hit the
	// 5s budget and will "retry next boot"), Failed (errored).
	OK      int `json:"ok"`
	Skipped int `json:"skipped"`
	Timeout int `json:"timeout"`
	Failed  int `json:"failed"`
	// FailedNames lists the entries that errored or timed out, so the count is
	// actionable without a log dive. Capped — see migrationsFailedNamesCap.
	FailedNames []string `json:"failed_names"`
	DurationMS  int64    `json:"duration_ms"`
}

// migrationsFailedNamesCap bounds the /health payload: 41 entries fail today,
// and an unbounded list would let a pathological boot inflate every health
// check response.
const migrationsFailedNamesCap = 60

var migrationsStatusProvider atomic.Pointer[func() MigrationsStatus]

// SetMigrationsStatusProvider registers the snapshot source (called once from
// cmd/server after runStartupMigrations returns).
func SetMigrationsStatusProvider(fn func() MigrationsStatus) {
	if fn == nil {
		migrationsStatusProvider.Store(nil)
		return
	}
	migrationsStatusProvider.Store(&fn)
}

// CurrentMigrationsStatus returns the live snapshot, or a zero (never-ran)
// status when no provider is installed.
func CurrentMigrationsStatus() MigrationsStatus {
	if p := migrationsStatusProvider.Load(); p != nil {
		return (*p)()
	}
	return MigrationsStatus{FailedNames: []string{}}
}
