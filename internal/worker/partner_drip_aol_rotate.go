package worker

// AOL ROUND-ROBIN (operator 2026-08-25, Operating Plan v2): "AOL is a large
// lane in these feeds. I would be okay if we round robin this ISP across all
// available [sending domains]."
//
// On the gated-prefix (internal insurance) verticals, AOL introductions no
// longer ride the lane's single roster brand. Instead a dedicated AOL-only
// companion wave fires each tick under a brand rotated across the WHOLE
// estate roster — same shape as the yahoo-newsletter companion pass
// (processFollowupYahooNewsletter) and the warm-up keepOnlyISPCaps pattern.
//
// Mechanics per (tick, vertical):
//   - brand = rotation over aolRotateBrands() keyed to the wall-clock 15-min
//     bucket (stateless — both service instances agree, restarts lose nothing;
//     same device as pickFollowupTouch).
//   - caps = the STANDARD welcome chain for that brand (resolvePerISPCaps →
//     applyNewRecordDailyBudget → applyISPBrandRouting → applyBrandIntroBudgets)
//     then confined to AOL via keepOnlyISPCaps — so every per-domain AOL
//     budget, ledger hold, and doctrine cap governs the rotated wave exactly
//     as it would a roster wave.
//   - the ROSTER brand's own welcome wave has its AOL cap zeroed while
//     rotation is enabled (aolRotateStripsRoster), so rotation is the one AOL
//     path and the lane's AOL volume spreads instead of doubling.
//   - follow-ups need no changes: records pin to last_touch_brand/mailed_brand
//     and the express follow-up pass already fires per brand-holding-due.
//     Creative resolution requires (vertical, brand) t1 + follow-up rows for
//     every rotated brand — seeded from the brand-agnostic Insurance Savings
//     set (DB rows, no deploy).
//
// Kill switch: PARTNER_DRIP_AOL_ROTATE_DISABLED=1 restores roster-only AOL.
// Brand set override: PARTNER_DRIP_AOL_ROTATE_BRANDS="db,ht,..." (blank falls
// back to the default estate set — same fail-safe direction as the gate's
// vertical override: an accidentally empty deploy var must not silently
// change behavior to an empty rotation).

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// defaultAOLRotateBrands is the estate roster: the 16 orchestrator-coded
// sending domains. Ledger holds and budgets still gate each individually.
var defaultAOLRotateBrands = []string{
	"db", "ht", "mh", "qf", "bwp", "ci", "cp", "fc",
	"hws", "lpl", "mrd", "rb", "rru", "tot", "wfy", "yih",
}

func aolRotateDisabled() bool {
	v := os.Getenv("PARTNER_DRIP_AOL_ROTATE_DISABLED")
	return v == "1" || v == "true"
}

func aolRotateBrands() []string {
	raw := os.Getenv("PARTNER_DRIP_AOL_ROTATE_BRANDS")
	var out []string
	for _, b := range strings.Split(raw, ",") {
		if b = strings.ToLower(strings.TrimSpace(b)); b != "" {
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		return defaultAOLRotateBrands
	}
	return out
}

// aolRotationBrand picks this tick's sending domain for a vertical's AOL wave.
// Vertical is folded in so the estate's lanes fan across different domains in
// the same tick instead of all landing on one.
func aolRotationBrand(vertical string, now time.Time) string {
	brands := aolRotateBrands()
	slot := now.Unix() / int64(15*60)
	var vh int
	for _, r := range vertical {
		vh = vh*31 + int(r)
	}
	if vh < 0 {
		vh = -vh
	}
	return brands[int(slot%int64(len(brands))+int64(vh))%len(brands)]
}

// aolRotationActive reports whether the rotated AOL pass owns AOL for this
// vertical (gated-prefix scope = the operator's "these feeds").
func aolRotationActive(vertical string) bool {
	if aolRotateDisabled() {
		return false
	}
	lv := strings.ToLower(strings.TrimSpace(vertical))
	for _, p := range gatedVerticalPrefixes() {
		if strings.HasPrefix(lv, p) {
			return true
		}
	}
	return false
}

// processAOLRotated fires one AOL-only welcome wave for the vertical under the
// tick's rotated brand. Runs the full standard cap chain for that brand so
// budgets/holds/doctrine all bind; skips silently when the rotated brand has
// no AOL allowance or the vertical holds no AOL-ready records.
func (po *PartnerDripOrchestrator) processAOLRotated(ctx context.Context, v verticalState) error {
	brand := aolRotationBrand(v.vertical, time.Now().UTC())

	perISPCaps, err := po.resolvePerISPCaps(ctx, v.vertical, v.datasetID, ispCapBacklogReady)
	if err != nil {
		return fmt.Errorf("aol_rotate resolve_caps: %w", err)
	}
	perISPCaps = po.applyNewRecordDailyBudget(ctx, brand, v.datasetID, perISPCaps)
	perISPCaps = po.applyISPBrandRouting(brand, perISPCaps)
	perISPCaps = po.applyBrandIntroBudgets(ctx, brand, perISPCaps)
	// Per-sending-domain × ISP GLOBAL recovery cap (operator 2026-08-27), same
	// chain position it holds in the welcome pass. Required here, not optional:
	// partner_drip_orchestrator.go:1614 zeroes the roster wave's AOL cap while
	// aolRotationActive(vertical) — true for every gated-prefix (internal
	// insurance) lane — so this companion pass is the ONLY AOL path for those
	// verticals. Without it every governed AOL send is ungoverned.
	perISPCaps = po.applyDomainGovernor(ctx, brand, v.vertical, "aol_rotate", perISPCaps)
	perISPCaps = keepOnlyISPCaps(perISPCaps, "aol")
	if !capsAnyPositive(perISPCaps) {
		return nil // brand exhausted/held for AOL this tick — next tick rotates on
	}

	claimed, err := po.claimRecordsByISPCaps(ctx, v.vertical, brand, perISPCaps, po.cfg.MaxWaveSize)
	if err != nil {
		return fmt.Errorf("aol_rotate claim: %w", err)
	}
	if len(claimed) == 0 {
		return nil
	}

	keep, deferred, reasons, err := po.applyThroughputSafety(ctx, brand, claimed, perISPCaps)
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] aol_rotate throughput_safety vertical=%s: %v — proceeding without deferral", v.vertical, err)
		keep = claimed
	}
	if len(deferred) > 0 {
		if relErr := po.releaseClaim(ctx, deferred); relErr != nil {
			log.Printf("[PartnerDripOrchestrator] aol_rotate release deferred: %v", relErr)
		}
		log.Printf("[PartnerDripOrchestrator] aol_rotate deferred %d vertical=%s reasons=%v", len(deferred), v.vertical, reasons)
	}
	if len(keep) == 0 {
		return nil
	}

	creative, err := po.resolveCreative(ctx, v.vertical, brand)
	if err != nil {
		_ = po.releaseClaim(ctx, keep)
		return fmt.Errorf("aol_rotate resolve_creative vertical=%s brand=%s: %w", v.vertical, brand, err)
	}
	subscriberIDs, err := po.promoteToSubscribers(ctx, v, keep)
	if err != nil {
		_ = po.releaseClaim(ctx, keep)
		return fmt.Errorf("aol_rotate promote: %w", err)
	}
	_, deployedCount := po.deployWaveGroups(ctx, v, brand, creative, keep, subscriberIDs, "")
	if deployedCount == 0 {
		return fmt.Errorf("aol_rotate: all wave groups failed vertical=%s brand=%s", v.vertical, brand)
	}
	log.Printf("[PartnerDripOrchestrator] aol_rotate wave: vertical=%s brand=%s size=%d", v.vertical, brand, len(keep))
	return nil
}
