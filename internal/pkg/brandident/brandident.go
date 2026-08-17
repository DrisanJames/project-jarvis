// Package brandident is the ONE shared brand-identity normalizer for the
// platform's two brand ground truths (Vector A/B cross-vector contract,
// tasks/platform-work/vector-b-drip-observatory-plan.md §0 + §5.9):
//
//   - brand_code    (db/ht/…/aad/hfc) — Vector A property-ledger ground truth
//   - sending apex  (discountblog.com/…)  — drip-side ground truth
//
// Every fact/audit/meta row stores BOTH fields, converted only through this
// package — neither vector hand-maps. The compile-time literal below mirrors
// the canonical 27-brand registry (agents/registry/brand_metadata.py
// BRAND_REGISTRY, codes lowercased) and seeds the mailing_brand_codes table
// in runStartupMigrations (SeedSQL). Parity gates: the unit test in this
// package asserts internal consistency; the P2 shadow step compares the
// table against the Python registry via the audience-knowledge MCP.
//
// A lookup miss is returned as ok=false — callers quarantine
// (reason='brand_unknown'), never guess a code, never proceed silently
// (plan §5.9).
//
// Wiring follows internal/pkg/brand/registry.go: the literal is the seed
// AND the fallback; RefreshFromDB installs union(literal, table rows) —
// union, never replacement, and a compile-time pair is never re-mapped by a
// table row, so identity resolution is monotone-safe if the table is lost
// or polluted.
package brandident

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
)

// Pair is one brand_code↔apex identity.
type Pair struct {
	Code string // lowercased registry brand_code (e.g. "db", "bcc", "aad")
	Apex string // sending apex domain (e.g. "discountblog.com")
}

// canonical mirrors agents/registry/brand_metadata.py BRAND_REGISTRY —
// all 27 brands in canonical registry order, codes lowercased (plan §5.9).
// Verified against the Python registry 2026-08-17.
var canonical = []Pair{
	// 16 legacy brands (PMTA/SES)
	{"db", "discountblog.com"},
	{"ht", "historythinking.com"},
	{"mh", "myownhealth.net"},
	{"qf", "quizfiesta.com"},
	{"bw", "businessweeklypro.com"},
	{"fc", "financialcalculate.com"},
	{"cp", "consumerpro.net"},
	{"hw", "homewarrantyservices.org"},
	{"rr", "refinanceratesusa.com"},
	{"tt", "thingoftheday.org"},
	{"yi", "yourinsurancehub.com"},
	{"mr", "myrepairdiy.com"},
	{"ci", "casainsure.com"},
	{"lp", "learnpersonalloans.com"},
	{"rb", "ratesbazar.com"},
	{"wf", "warrantyforyou.com"},
	// 11 KumoMTA warmup domains (routing_mode='kumo')
	{"mp", "mypersonalfinancial.com"},
	{"pd", "paymydebit.com"},
	{"tr", "theretirementblog.com"},
	{"bcc", "bestcreditcare.com"},
	{"usf", "us-finance.com"},
	{"yfb", "yourfinancialblog.com"},
	{"hlj", "homeloansbyjaime.com"},
	{"fth", "firsttimebuyerhomeloan.com"},
	{"htm", "hometracmortgage.com"},
	{"aad", "aadwd.com"},
	{"hfc", "hfcl.net"},
}

var (
	regMu sync.RWMutex
	// effective* are nil until the first successful RefreshFromDB; while
	// nil, lookups serve the compile-time literal alone.
	effectiveCodeToApex map[string]string
	effectiveApexToCode map[string]string
)

func literalMaps() (codeToApex, apexToCode map[string]string) {
	codeToApex = make(map[string]string, len(canonical))
	apexToCode = make(map[string]string, len(canonical))
	for _, p := range canonical {
		codeToApex[p.Code] = p.Apex
		apexToCode[p.Apex] = p.Code
	}
	return codeToApex, apexToCode
}

// Canonical returns a copy of the compile-time 27-brand literal, in
// registry order. Used by the seed generator and the parity tests.
func Canonical() []Pair {
	out := make([]Pair, len(canonical))
	copy(out, canonical)
	return out
}

// CodeForApex returns the brand_code for a sending apex (case/space
// normalized). ok=false on a miss — the caller must quarantine
// (reason='brand_unknown'), never guess.
func CodeForApex(apex string) (string, bool) {
	a := strings.ToLower(strings.TrimSpace(apex))
	if a == "" {
		return "", false
	}
	regMu.RLock()
	m := effectiveApexToCode
	regMu.RUnlock()
	if m != nil {
		c, ok := m[a]
		return c, ok
	}
	_, apexToCode := literalMaps()
	c, ok := apexToCode[a]
	return c, ok
}

// ApexForCode returns the sending apex for a brand_code (case/space
// normalized). ok=false on a miss — the caller must quarantine, never guess.
func ApexForCode(code string) (string, bool) {
	c := strings.ToLower(strings.TrimSpace(code))
	if c == "" {
		return "", false
	}
	regMu.RLock()
	m := effectiveCodeToApex
	regMu.RUnlock()
	if m != nil {
		a, ok := m[c]
		return a, ok
	}
	codeToApex, _ := literalMaps()
	a, ok := codeToApex[c]
	return a, ok
}

// SeedSQL generates the mailing_brand_codes seed from the compile-time
// literal — one source of truth (the ownedDomainsSeedSQL pattern,
// cmd/server/main.go): the seed can never drift from the Go literal because
// it IS the Go literal. Idempotent: ON CONFLICT (brand_code) DO NOTHING.
func SeedSQL() string {
	vals := make([]string, 0, len(canonical))
	for _, p := range canonical {
		vals = append(vals, fmt.Sprintf("('%s', '%s', 'seed')",
			strings.ReplaceAll(p.Code, "'", "''"),
			strings.ReplaceAll(p.Apex, "'", "''")))
	}
	return "INSERT INTO mailing_brand_codes (brand_code, apex, source) VALUES " +
		strings.Join(vals, ", ") + " ON CONFLICT (brand_code) DO NOTHING"
}

// RefreshFromDB loads mailing_brand_codes and installs
// union(literal, table rows) as the effective mapping ("loaded once per run
// cycle (refreshable)" — plan §5.9). A table row can EXTEND the mapping
// (e.g. a brand onboarded after this binary was built) but can never
// re-map or shadow a compile-time pair. On ANY error the previous effective
// mapping (or the literal fallback) is kept untouched and the error is
// returned for logging — no behavior change on failure.
func RefreshFromDB(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT brand_code, apex FROM mailing_brand_codes`)
	if err != nil {
		return fmt.Errorf("brandident refresh query: %w", err)
	}
	defer rows.Close()

	codeToApex, apexToCode := literalMaps()
	for rows.Next() {
		var code, apex string
		if err := rows.Scan(&code, &apex); err != nil {
			return fmt.Errorf("brandident refresh scan: %w", err)
		}
		code = strings.ToLower(strings.TrimSpace(code))
		apex = strings.ToLower(strings.TrimSpace(apex))
		if code == "" || apex == "" {
			continue
		}
		// Union, never replacement: skip any row that would re-map an
		// already-mapped code OR apex (literal pairs always win; first
		// table row wins among table rows).
		if _, exists := codeToApex[code]; exists {
			continue
		}
		if _, exists := apexToCode[apex]; exists {
			continue
		}
		codeToApex[code] = apex
		apexToCode[apex] = code
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("brandident refresh rows: %w", err)
	}

	regMu.Lock()
	effectiveCodeToApex = codeToApex
	effectiveApexToCode = apexToCode
	regMu.Unlock()
	return nil
}
