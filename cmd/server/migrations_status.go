package main

import (
	"sync"

	"github.com/ignite/sparkpost-monitor/internal/api"
)

// =============================================================================
// Startup-migration status -> /health.migrations (REQ-092 DoD 7)
// =============================================================================
// runStartupMigrations runs once at boot and then the process forgets what
// happened; the only record was CloudWatch. This holds the last summary and
// installs the /health provider on first publish, so a boot that fails 39
// entries is visible from the deploy script and the portal instead of a log
// dive. Guarded by a mutex because /health is served concurrently with boot.

var (
	migrationsStatusMu   sync.RWMutex
	migrationsStatusLast api.MigrationsStatus
	migrationsStatusOnce sync.Once
)

// publishMigrationsStatus records the summary and (once) wires the provider.
func publishMigrationsStatus(st api.MigrationsStatus) {
	if st.FailedNames == nil {
		st.FailedNames = []string{}
	}
	migrationsStatusMu.Lock()
	migrationsStatusLast = st
	migrationsStatusMu.Unlock()

	migrationsStatusOnce.Do(func() {
		api.SetMigrationsStatusProvider(func() api.MigrationsStatus {
			migrationsStatusMu.RLock()
			defer migrationsStatusMu.RUnlock()
			return migrationsStatusLast
		})
	})
}
