package api

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/worker"
)

// laneBrandResolver answers "is this brand code a real drip lane brand?" for the
// Property Ledger journey/roster surfaces.
//
// WHY THIS EXISTS (verified 2026-08-19): propertyLedgerValidBrand
// (property_ledger.go:59) tests membership in worker.DripIntroBrands(), which
// returns a copy of the COMPILE-TIME dripBrands slice
// (partner_drip_orchestrator.go:309, :66). But the live domain-to-drip binding
// lives in the partner_drip_vertical_roster TABLE, which the orchestrator
// refreshes every tick (partner_drip_orchestrator.go:2476). Those two sets have
// already drifted: 'wcl' is actively rostered on the refi_heloc lane and is NOT
// in the compiled slice, so the static validator 400s a lane that is genuinely
// mailing.
//
// The fix mirrors the pattern brand.Domains() uses for owned domains: a UNION of
// the compiled list with the active DB rows, never a replacement. Losing the
// table degrades to the compiled set rather than rejecting everything.
//
// DELIBERATELY NOT unioned with mailing_brand_metadata: that table keys on the
// Python registry's brand_code (BW, HW, LP, MR, TT, WF, YI, RR — 17 rows), which
// is a DIFFERENT coding scheme from the drip roster's (bwp, hws, lpl, mrd, tot,
// wfy, yih, rru). Merging them would admit 'bw'/'hw'/'lp' as drip brands, none of
// which the orchestrator can route. Verified 2026-08-19. The only code common to
// both schemes is WCL/wcl. See TestLaneBrand_DoesNotAdmitRegistryCodeScheme.
//
// Deliberately a SEPARATE validator rather than widening propertyLedgerValidBrand:
// that helper gates several existing handlers, and loosening a live gate is a
// blast radius this change does not need. Existing callers keep byte-identical
// behavior.
type laneBrandResolver struct {
	mu       sync.RWMutex
	cached   map[string]bool
	fetched  time.Time
	cacheTTL time.Duration
}

var laneBrands = &laneBrandResolver{cacheTTL: 60 * time.Second}

// compiledLaneBrands is the always-available floor: the compiled drip roster.
func compiledLaneBrands() map[string]bool {
	out := make(map[string]bool, 16)
	for _, c := range worker.DripIntroBrands() {
		out[strings.ToLower(strings.TrimSpace(c))] = true
	}
	return out
}

// resolve returns compiled brands UNION active roster brands. On any DB error it
// returns the compiled set so a database blip narrows the surface instead of
// closing it entirely. Cached for cacheTTL so a canvas that polls does not run
// this per request.
func (r *laneBrandResolver) resolve(ctx context.Context, db *sql.DB) map[string]bool {
	r.mu.RLock()
	if r.cached != nil && time.Since(r.fetched) < r.cacheTTL {
		c := r.cached
		r.mu.RUnlock()
		return c
	}
	r.mu.RUnlock()

	set := compiledLaneBrands()
	if db != nil {
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		rows, err := db.QueryContext(qctx, `
			SELECT DISTINCT lower(btrim(brand))
			FROM partner_drip_vertical_roster
			WHERE active AND brand IS NOT NULL AND btrim(brand) <> ''`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var b string
				if rows.Scan(&b) == nil && b != "" {
					set[b] = true
				}
			}
		}
	}

	r.mu.Lock()
	r.cached, r.fetched = set, time.Now()
	r.mu.Unlock()
	return set
}

// propertyLedgerValidLaneBrand reports whether brand may be addressed by the
// journey/roster lane surfaces. Use this — not propertyLedgerValidBrand — for
// anything that reads the roster table, or actively-mailing lanes get 400s.
func propertyLedgerValidLaneBrand(ctx context.Context, db *sql.DB, brand string) bool {
	b := strings.ToLower(strings.TrimSpace(brand))
	if b == "" {
		return false
	}
	// Fast path: the compiled roster is the common case and needs no DB round
	// trip. Only a brand ABSENT from the compiled slice (e.g. 'wcl') pays for
	// the roster lookup, and that answer is memoized.
	if compiledLaneBrands()[b] {
		return true
	}
	return laneBrands.resolve(ctx, db)[b]
}

// invalidateLaneBrandCache drops the memo so a roster write is visible to
// validation immediately rather than up to cacheTTL later.
func invalidateLaneBrandCache() {
	laneBrands.mu.Lock()
	laneBrands.cached = nil
	laneBrands.mu.Unlock()
}
