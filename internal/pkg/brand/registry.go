package brand

// DB-backed owned-domains registry (audit G6): onboarding a new sending
// domain must not require a Go edit + full redeploy. The compile-time
// OwnedDomains list in brand.go remains the seed AND the fallback — the
// effective list served by Domains() is the UNION of the hardcoded list and
// the mailing_owned_domains table (union, never replacement: losing a table
// row can never un-brand a live domain, so brand-scoped suppression rooting
// is monotone-safe). If the table read fails, behavior is byte-identical to
// the pre-registry hardcoded list.
//
// Wiring (cmd/server/main.go): runStartupMigrations creates + seeds
// mailing_owned_domains from OwnedDomains, then RefreshFromDB is called once
// at boot and StartRefresher keeps a periodic refresh. The onboard endpoint
// (internal/api/sending_domain_onboard.go) calls Register() so a newly
// onboarded domain is honored in-process immediately.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

var (
	regMu sync.RWMutex
	// effective is nil until the first successful RefreshFromDB/Register;
	// while nil, Domains() serves the compile-time OwnedDomains fallback.
	effective []string
)

// Domains returns the effective owned-domain list: the union of the
// hardcoded OwnedDomains and the DB registry once loaded; the hardcoded
// list alone before that (or forever, if the DB is unreadable).
func Domains() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	if effective == nil {
		return OwnedDomains
	}
	return effective
}

// Register adds a domain to the in-process effective list immediately
// (idempotent). Used by the onboard endpoint so the click-tracker rewriter
// and brand-root resolution honor a new domain without waiting for the next
// DB refresh.
func Register(domain string) {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "" {
		return
	}
	regMu.Lock()
	defer regMu.Unlock()
	base := effective
	if base == nil {
		base = OwnedDomains
	}
	for _, od := range base {
		if od == d {
			return
		}
	}
	next := make([]string, 0, len(base)+1)
	next = append(next, base...)
	next = append(next, d)
	effective = next
}

// RefreshFromDB loads active rows from mailing_owned_domains and installs
// union(OwnedDomains, table rows) as the effective list. On ANY error the
// previous effective list (or the hardcoded fallback) is kept untouched and
// the error is returned for logging — no behavior change on failure.
func RefreshFromDB(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT domain FROM mailing_owned_domains WHERE active = TRUE`)
	if err != nil {
		return fmt.Errorf("owned-domains refresh query: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]bool, len(OwnedDomains)+8)
	next := make([]string, 0, len(OwnedDomains)+8)
	for _, od := range OwnedDomains {
		if !seen[od] {
			seen[od] = true
			next = append(next, od)
		}
	}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return fmt.Errorf("owned-domains refresh scan: %w", err)
		}
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		next = append(next, d)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("owned-domains refresh rows: %w", err)
	}

	regMu.Lock()
	effective = next
	regMu.Unlock()
	return nil
}

// StartRefresher runs RefreshFromDB every interval until ctx is done.
// Failures log and keep the previous list (fallback semantics above).
func StartRefresher(ctx context.Context, db *sql.DB, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := RefreshFromDB(ctx, db); err != nil {
					log.Printf("[brand] owned-domains refresh failed (keeping previous list): %v", err)
				}
			}
		}
	}()
}

// resetRegistryForTest clears the dynamic list (tests only — the registry is
// package-global state).
func resetRegistryForTest() {
	regMu.Lock()
	effective = nil
	regMu.Unlock()
}
