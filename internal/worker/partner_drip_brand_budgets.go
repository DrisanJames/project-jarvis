package worker

// Per-(sending-domain × ISP) daily introduction budgets for the partner drip
// system (operator 2026-08-15: "What is missing is sending domain distribution
// to ensure we are also properly warming the ISP by sending domain").
//
// The ledger is partner_drip_brand_budgets: one row per (brand, isp) with a
// daily_budget of NEW-RECORD introductions (first touches) that domain may
// absorb for that ISP per America/Denver day, plus a hold flag. It is an
// OVERLAY on the existing welcome-pass cap chain, applied after
// applyNewRecordDailyBudget/applyISPBrandRouting in processVerticalWith:
//
//   - (brand, isp) with NO row        -> untouched (behavior identical to today)
//   - hold = TRUE or daily_budget <= 0 -> per-wave cap forced to 0 (no count query)
//   - otherwise                        -> cap = min(cap, daily_budget - introduced today)
//
// Spend is counted from partner_clean_queue (mailed_brand, isp_family,
// mailed_at) — written exactly once on the first touch — via a single grouped
// query per wave, served by idx_pcq_governed_daily_count. Follow-up touches do
// not stamp mailed_brand and are NOT governed by this ledger (v1 scope: the
// warming lever is new-data introduction volume).
//
// Scope guard: governed (Kumo) brands are excluded — partner_property_governor
// is their single ceiling — and the ledger is welcome-pass only.
//
// Fail-open: an unreadable/missing table keeps the previous cache (empty at
// boot), which means "no constraint" — the safe direction for an overlay whose
// absence is today's behavior. Kill switch: PARTNER_DRIP_BRAND_BUDGETS_DISABLED.

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strings"
)

// brandBudgetRow is one (brand, isp) cell of the introduction-budget ledger.
type brandBudgetRow struct {
	daily int
	hold  bool
}

// brandBudgetsDisabled is the one-move kill switch for the whole ledger.
func brandBudgetsDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PARTNER_DRIP_BRAND_BUDGETS_DISABLED"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// loadBrandBudgets refreshes the in-memory partner_drip_brand_budgets cache.
// Called once per tick (tickOnce) so operator edits take effect within one tick
// without a deploy. On error the PREVIOUS cache is kept — for this overlay an
// empty cache means "unconstrained", so a transient read failure never zeroes
// caps or invents constraints.
func (po *PartnerDripOrchestrator) loadBrandBudgets(ctx context.Context) {
	loaded := map[string]map[string]brandBudgetRow{}
	err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT lower(btrim(brand)), lower(btrim(isp)), daily_budget, hold
			FROM partner_drip_brand_budgets
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b, isp string
			var daily int
			var hold bool
			if err := rows.Scan(&b, &isp, &daily, &hold); err != nil {
				return err
			}
			if b == "" || isp == "" {
				continue
			}
			if loaded[b] == nil {
				loaded[b] = map[string]brandBudgetRow{}
			}
			loaded[b][isp] = brandBudgetRow{daily: daily, hold: hold}
		}
		return rows.Err()
	})
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] loadBrandBudgets failed (%v) — keeping previous budget cache", err)
		return
	}
	po.brandBudgetMu.Lock()
	po.brandBudgetCache = loaded
	po.brandBudgetMu.Unlock()
}

// brandIntroSpendToday returns today's (America/Denver) first-touch
// introductions for `brand`, grouped by ISP family — one round trip per wave,
// served by idx_pcq_governed_daily_count. Best-effort: on error returns nil and
// the caller keeps the static caps (same posture as governedDailyCount).
func (po *PartnerDripOrchestrator) brandIntroSpendToday(ctx context.Context, brand string) map[string]int {
	spend := map[string]int{}
	err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT LOWER(COALESCE(NULLIF(isp_family, ''), 'other')), COUNT(*)
			FROM partner_clean_queue
			WHERE mailed_brand = $1
			  AND mailed_at >= date_trunc('day', NOW() AT TIME ZONE 'America/Denver') AT TIME ZONE 'America/Denver'
			GROUP BY 1
		`, brand)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var isp string
			var n int
			if err := rows.Scan(&isp, &n); err != nil {
				return err
			}
			spend[isp] = n
		}
		return rows.Err()
	})
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] brand-budget spend count failed brand=%s (%v) — keeping static caps", brand, err)
		return nil
	}
	return spend
}

// applyBrandIntroBudgets clamps a welcome wave's per-ISP caps to the brand's
// remaining introduction budget. ISPs without a ledger row are untouched, so an
// empty/absent ledger is byte-identical to today's behavior.
func (po *PartnerDripOrchestrator) applyBrandIntroBudgets(ctx context.Context, brand string, caps map[string]int) map[string]int {
	if brandBudgetsDisabled() {
		return caps
	}
	lb := strings.ToLower(strings.TrimSpace(brand))
	po.brandBudgetMu.RLock()
	budgets := po.brandBudgetCache[lb]
	po.brandBudgetMu.RUnlock()
	if len(budgets) == 0 {
		return caps
	}

	// Only pay for a spend query when at least one budgeted ISP could bind.
	needSpend := false
	for isp, cap := range caps {
		b, ok := budgets[strings.ToLower(strings.TrimSpace(isp))]
		if ok && !b.hold && b.daily > 0 && cap > 0 {
			needSpend = true
			break
		}
	}
	var spend map[string]int
	if needSpend {
		spend = po.brandIntroSpendToday(ctx, lb)
		if spend == nil {
			return caps // count failed — keep static caps (fail-open)
		}
	}

	out := cloneISPCapMap(caps)
	for isp := range out {
		lisp := strings.ToLower(strings.TrimSpace(isp))
		b, ok := budgets[lisp]
		if !ok {
			continue // no ledger row -> unconstrained
		}
		if b.hold || b.daily <= 0 {
			out[isp] = 0
			continue
		}
		remaining := b.daily - spend[lisp]
		if remaining < 0 {
			remaining = 0
		}
		if out[isp] > remaining {
			out[isp] = remaining
		}
	}
	return out
}

// capsAnyPositive reports whether any ISP in the map still has a positive cap.
func capsAnyPositive(caps map[string]int) bool {
	for _, c := range caps {
		if c > 0 {
			return true
		}
	}
	return false
}

// advanceBrandRotation persists ONLY the rotation pointer for a pass's state
// row. Used when a brand's caps clamp to all-zero (budget exhausted) so the
// round-robin moves past it instead of re-picking the same exhausted brand
// every tick and pinning the vertical for the rest of the day. last_wave_* is
// deliberately untouched — no wave shipped.
func (po *PartnerDripOrchestrator) advanceBrandRotation(ctx context.Context, stateKey string, nextIdx int) error {
	_, err := po.db.ExecContext(ctx, `
		INSERT INTO partner_drip_state (vertical, next_brand_index, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (vertical) DO UPDATE SET
		    next_brand_index = EXCLUDED.next_brand_index,
		    updated_at = NOW()
	`, stateKey, nextIdx)
	return err
}
