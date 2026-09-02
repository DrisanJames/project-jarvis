package api

// mailing_isp_bans — the declarative brand × ISP ban registry (REQ-083).
//
// Operator ruling 2026-08-30: WFY/RB/RRU/TOT/CP/LPL/YIH/CI must NEVER mail
// gmail (uniform 5.7.1 domain blocks, 0.4-1.3% delivery). Until this file the
// ruling was enforced only by a nightly Python wave-cancel
// (agents/jobs/gmail_ban_sweep.py, 23:50 MT) — and the PreDispatch rebind
// re-deployed each cell's own blob ~1h before its anchor, i.e. AFTER the
// sweep, re-minting the gmail waves it had cancelled. 3,416 gmail messages
// went out on the banned brands on 2026-09-01 alone (board-chain SEV-1,
// daily-process F-01).
//
// The fix has to live where audiences are PLANNED, not where waves are
// cancelled: blob ISP plans carry no quota key, and the planner reads a
// missing/zero quota as UNLIMITED (pmta_campaign_planner.go, hasUnlimited),
// so "gmail: 0" in a board cell is audience-bound, never off.
// normalizePMTACampaignInput is the single choke point every deploy passes
// through — the HTTP deploy, the board-grid stage, the PreDispatch sibling
// rebind (all via deployFromInput) and the AudienceFinalizationWorker, which
// re-normalizes the persisted blob before planning. Dropping the plan here
// removes it from normalized.Plans, and mailing_campaign_isp_plans,
// _isp_time_spans, _plan_recipients and _waves are all built from that slice
// (pmta_campaign_persistence.go) — so no row, no recipient and no wave for a
// banned ISP is ever created.
//
// Removal semantics mirror applyCellISPControls (board_grid_stage.go): the
// ISP disappears from ISPPlans, ISPQuotas and TargetISPs, and an unknown ISP
// class refuses loudly rather than silently no-opping.
//
// FAIL CLOSED: if the ban table cannot be read, the deploy is REFUSED. A ban
// that fails open re-creates exactly the leak this exists to close.
//
// Rollback: DELETE the rows (the gate is data, not code); the table itself is
// additive and unused by anything else.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brandident"
)

// ispBanTable is one loaded snapshot of mailing_isp_bans.
//
//   - byOrg is the org-scoped view: org id → brand_code → isp → banned.
//   - union is the same data with the org dimension collapsed, and is what
//     the org-blind caller uses (see forOrg).
type ispBanTable struct {
	byOrg map[string]map[string]map[string]bool
	union map[string]map[string]bool
}

// forOrg returns the ban set for one org. orgID == "" asks for the union
// across every org, which is what normalizePMTACampaignInput uses: normalize
// is a pure function of the campaign input and carries no org id, and the
// union can only ever ban MORE, never less. In this single-tenant deployment
// (org_context.go SingleTenantFallbackOrgID) union == the one org's rows, so
// the two are identical today; the org-scoped view exists so a caller that
// does know its org can pass it without changing this file.
func (t *ispBanTable) forOrg(orgID string) map[string]map[string]bool {
	if t == nil {
		return nil
	}
	if o := strings.ToLower(strings.TrimSpace(orgID)); o != "" {
		return t.byOrg[o]
	}
	return t.union
}

// ispBanResolver caches the table for cacheTTL. Same shape as the sibling
// laneBrandResolver (property_lane_brands.go) — with one deliberate
// difference: a load error is NOT degraded into an empty set, it is returned,
// because an unreadable ban must stop the deploy.
type ispBanResolver struct {
	mu       sync.RWMutex
	db       *sql.DB
	cached   *ispBanTable
	fetched  time.Time
	cacheTTL time.Duration
}

var ispBans = &ispBanResolver{cacheTTL: 60 * time.Second}

// SetISPBanDB wires the mailing DB into the ban registry and loads it once,
// loudly (the SetConsciousnessDB pattern, handlers_consciousness.go). Call it
// at boot AFTER runStartupMigrations has created the table.
//
// Until it is called the registry is INERT and no ban is applied: every
// in-process test and every binary that does not wire a DB keeps byte-
// identical behavior. That also bounds the boot window — a deploy that
// arrives before wiring behaves exactly as it does today rather than being
// refused for a table that does not exist yet.
func SetISPBanDB(db *sql.DB) {
	ispBans.mu.Lock()
	ispBans.db = db
	ispBans.cached = nil
	ispBans.fetched = time.Time{}
	ispBans.mu.Unlock()
	if db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tbl, err := ispBans.resolve(ctx)
	if err != nil {
		log.Printf("[ISPBan] ARMED but the first load FAILED: %v — every campaign deploy will be REFUSED until mailing_isp_bans reads cleanly", err)
		return
	}
	n := 0
	for _, isps := range tbl.union {
		n += len(isps)
	}
	log.Printf("[ISPBan] armed: %d brand×ISP bans loaded from mailing_isp_bans (%d brands)", n, len(tbl.union))
}

// resolve returns the cached snapshot, reloading when stale. Errors are never
// cached — a transient DB blip must not pin the refusal for a whole TTL.
func (r *ispBanResolver) resolve(ctx context.Context) (*ispBanTable, error) {
	r.mu.RLock()
	db := r.db
	if r.cached != nil && time.Since(r.fetched) < r.cacheTTL {
		c := r.cached
		r.mu.RUnlock()
		return c, nil
	}
	r.mu.RUnlock()

	if db == nil {
		// Registry not wired (tests, non-server binaries) — inert.
		return nil, nil
	}

	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// The whole table is read (8 rows in production) and scoped by org in
	// memory — a config table, not a tenant data table. Rows are still
	// org-keyed so a per-org lookup is exact.
	rows, err := db.QueryContext(qctx, `
		SELECT lower(btrim(organization_id::text)), lower(btrim(brand_code)), lower(btrim(isp))
		FROM mailing_isp_bans
	`)
	if err != nil {
		return nil, fmt.Errorf("load: %w", err)
	}
	defer rows.Close()

	tbl := &ispBanTable{
		byOrg: map[string]map[string]map[string]bool{},
		union: map[string]map[string]bool{},
	}
	for rows.Next() {
		var org, code, isp string
		if err := rows.Scan(&org, &code, &isp); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if code == "" || isp == "" {
			return nil, fmt.Errorf("row (org=%q) has an empty brand_code or isp", org)
		}
		// A typo'd ISP class is a policy violation that LOOKS applied —
		// refuse at load, exactly as applyCellISPControls refuses an unknown
		// exclude_isps entry (board_grid_stage.go).
		if !boardGridCanonicalISPs[isp] {
			return nil, fmt.Errorf("unknown ISP class %q for brand %q — fix the row or the ban is unenforceable", isp, code)
		}
		if tbl.byOrg[org] == nil {
			tbl.byOrg[org] = map[string]map[string]bool{}
		}
		if tbl.byOrg[org][code] == nil {
			tbl.byOrg[org][code] = map[string]bool{}
		}
		tbl.byOrg[org][code][isp] = true
		if tbl.union[code] == nil {
			tbl.union[code] = map[string]bool{}
		}
		tbl.union[code][isp] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}

	r.mu.Lock()
	r.cached = tbl
	r.fetched = time.Now()
	r.mu.Unlock()
	return tbl, nil
}

// ispBanBrandCode resolves a campaign's sending domain to the canonical
// brand_code through the platform's existing mapping — brand.Root (owned-
// domain roots, union of the Go slice and mailing_owned_domains) then
// brandident.CodeForApex (the ONE brand_code↔apex normalizer). Nothing here
// re-hardcodes a brand map.
//
// brand.Root returns its input unchanged when no owned domain matches
// (CLAUDE.md §7), so a lost mailing_owned_domains row would otherwise leave a
// banned brand unresolvable; the second attempt strips the sending label
// ("em.warrantyforyou.com" → "warrantyforyou.com") to close that hole.
func ispBanBrandCode(sendingDomain string) (string, bool) {
	d := strings.ToLower(strings.TrimSpace(sendingDomain))
	if d == "" {
		return "", false
	}
	apex := brand.Root(d)
	if code, ok := brandident.CodeForApex(apex); ok {
		return code, true
	}
	if i := strings.Index(apex, "."); i > 0 {
		if code, ok := brandident.CodeForApex(apex[i+1:]); ok {
			return code, true
		}
	}
	return "", false
}

func ispBanNorm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// applyISPBansForOrg strips every banned ISP for the input's brand out of the
// deploy input, in place. orgID == "" uses the cross-org union (see forOrg).
//
// Returns an error when the table cannot be read (refuse the deploy) or when
// the ban would leave the campaign with nothing to send (refuse loudly rather
// than deploy an empty campaign).
func applyISPBansForOrg(orgID string, input *engine.PMTACampaignInput) error {
	tbl, err := ispBans.resolve(context.Background())
	if err != nil {
		return fmt.Errorf("isp ban registry unreadable (%v) — refusing the deploy; a ban that fails open is the bug it exists to prevent", err)
	}
	if tbl == nil || len(tbl.union) == 0 {
		return nil
	}
	code, ok := ispBanBrandCode(input.SendingDomain)
	if !ok {
		return nil
	}
	banned := tbl.forOrg(orgID)[code]
	if len(banned) == 0 {
		return nil
	}

	hadPlans := len(input.ISPPlans) > 0
	hadTargets := len(input.TargetISPs) > 0
	dropped := map[string]bool{}

	if hadPlans {
		kept := make([]engine.PMTAISPScheduleInput, 0, len(input.ISPPlans))
		for _, p := range input.ISPPlans {
			if banned[ispBanNorm(p.ISP)] {
				dropped[ispBanNorm(p.ISP)] = true
				continue
			}
			kept = append(kept, p)
		}
		if len(kept) == 0 {
			return fmt.Errorf("isp ban: every ISP plan for brand %q is banned (%s) — nothing would send", code, ispBanList(banned))
		}
		input.ISPPlans = kept
	}

	if len(input.ISPQuotas) > 0 {
		keptQ := make([]engine.ISPQuota, 0, len(input.ISPQuotas))
		for _, q := range input.ISPQuotas {
			if banned[ispBanNorm(q.ISP)] {
				dropped[ispBanNorm(q.ISP)] = true
				continue
			}
			keptQ = append(keptQ, q)
		}
		input.ISPQuotas = keptQ
	}

	if hadTargets {
		keptT := make([]engine.ISP, 0, len(input.TargetISPs))
		for _, t := range input.TargetISPs {
			if banned[ispBanNorm(string(t))] {
				dropped[ispBanNorm(string(t))] = true
				continue
			}
			keptT = append(keptT, t)
		}
		if len(keptT) == 0 && !hadPlans {
			return fmt.Errorf("isp ban: every target ISP for brand %q is banned (%s) — nothing would send", code, ispBanList(banned))
		}
		input.TargetISPs = keptT
	}

	if len(dropped) > 0 {
		log.Printf("[ISPBan] campaign %q (%s / brand %s): dropped %s per mailing_isp_bans",
			input.Name, input.SendingDomain, code, ispBanList(dropped))
	}
	return nil
}

func ispBanList(set map[string]bool) string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	// Small sets; a stable order keeps log lines diffable.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return strings.Join(out, ",")
}
