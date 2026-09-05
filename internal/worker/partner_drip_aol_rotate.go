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
// REQ-118: this pass IS metered against drip_domain_contracts — it reserves
// through grantWaveCapacity under phase phaseAOLRotate and settles with
// commitWave, like every other spender. DRIP_SUPPLY_AOL_ROTATE_FENCE=1 still
// stops it outright; see the fence block below.
//
// Kill switch: PARTNER_DRIP_AOL_ROTATE_DISABLED=1 restores roster-only AOL.
// Brand set override: PARTNER_DRIP_AOL_ROTATE_BRANDS="db,ht,..." (blank falls
// back to the default estate set — same fail-safe direction as the gate's
// vertical override: an accidentally empty deploy var must not silently
// change behavior to an empty rotation).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/worker/dripsupply"
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

// ── REQ-118 aol_rotate: the fence, and why it is no longer the only control ──
//
// HISTORY (the state this fence was shipped into). This pass used to be an
// UNMETERED second spender: it ran the legacy cap chain and claimed directly,
// so it spent zero contract tokens while drip_domain_contracts funded AOL.
// Because the roster welcome wave zeroes its own AOL cap while
// aolRotationActive (grep `aolRotationActive` in partner_drip_orchestrator.go's
// welcome pass), this is the ONLY AOL intro path on the gated-prefix lanes —
// so every governed AOL introduction was ungoverned by the contract.
//
// It could not be routed through the shared mediator helper from this file:
// grantWaveCapacity DERIVED its phase from the touch class (anything not
// TouchClassFollowup was phase "welcome"), and the kept layers zero AOL for
// phase "welcome" whenever aolRotationActive. The shared helper would therefore
// have (a) requested a grant for every ISP EXCEPT aol and (b) written those
// non-AOL grants back into this pass's AOL-only cap map, turning an AOL-only
// companion wave into a gmail/microsoft/… wave.
//
// NOW: grantWaveCapacity takes the phase EXPLICITLY, and phaseAOLRotate is a
// third phase whose kept layers keep AOL and keep NOTHING ELSE
// (keptCapLayersWithCeiling, the `case phase == phaseAOLRotate` branch). The
// widening failure mode above is structurally impossible — the grant ISP list
// is derived from that map — so processAOLRotated reserves and commits like
// every other spender.
//
// The fence STAYS, unchanged and still default-off. Metering does not replace
// it: it is the operator's stop switch for this pass, in the fresh-broadcast
// shape (internal/api/fresh_broadcast_runner.go, broadcastFenceEnabled) —
// env-gated, read at CALL time, refusing BEFORE the first write.

// errAOLRotateFenced is returned by processAOLRotated when the fence is armed.
// The message names the switch and the replacement path so an operator reading
// it in a log knows what produced it.
var errAOLRotateFenced = errors.New("aol_rotate claims are fenced (REQ-118): this pass spends no contract tokens — reserve through dripsupply before re-enabling")

// aolRotateFenceReason is the drip_tick_outcomes reason for a fenced tick, so a
// fenced lane is visible in the outcomes table and not merely absent from it.
const aolRotateFenceReason = "aol_rotate_fenced"

// aolRotateFenceEnabled reads DRIP_SUPPLY_AOL_ROTATE_FENCE at CALL time, not at
// process start: the fence is an operator switch during the REQ-118 cutover and
// must take effect on a task restart without a code change. Unset = today's
// behaviour, byte for byte on the send path.
func aolRotateFenceEnabled() bool {
	return strings.TrimSpace(os.Getenv("DRIP_SUPPLY_AOL_ROTATE_FENCE")) == "1"
}

// processAOLRotated fires one AOL-only welcome wave for the vertical under the
// tick's rotated brand. Runs the full standard cap chain for that brand so
// budgets/holds/doctrine all bind; skips silently when the rotated brand has
// no AOL allowance or the vertical holds no AOL-ready records.
func (po *PartnerDripOrchestrator) processAOLRotated(ctx context.Context, v verticalState) error {
	brand := aolRotationBrand(v.vertical, time.Now().UTC())

	// REQ-118 fence. Checked FIRST — before the cap chain reads and long before
	// claimRecordsByISPCaps writes — so a fenced tick leaves nothing behind:
	// no claimed rows, no promoted subscribers, no half-deployed wave. Unset
	// flag = the pass runs exactly as it does today.
	if aolRotateFenceEnabled() {
		po.tickOutcome(ctx, v.vertical, dripsupply.PassAOLRotate, dripsupply.OutcomeSkipped, aolRotateFenceReason, brand, nil, 0, "")
		return errAOLRotateFenced
	}

	perISPCaps, err := po.resolvePerISPCaps(ctx, v.vertical, v.datasetID, ispCapBacklogReady)
	if err != nil {
		po.tickOutcome(ctx, v.vertical, dripsupply.PassAOLRotate, dripsupply.OutcomeFailed, "resolve_caps", brand, nil, 0, "")
		return fmt.Errorf("aol_rotate resolve_caps: %w", err)
	}
	perISPCaps = po.applyNewRecordDailyBudget(ctx, brand, v.datasetID, perISPCaps)
	perISPCaps = po.applyISPBrandRouting(brand, perISPCaps)
	perISPCaps = po.applyBrandIntroBudgets(ctx, brand, perISPCaps)
	// Per-sending-domain × ISP GLOBAL recovery cap (operator 2026-08-27), same
	// chain position it holds in the welcome pass. Required here, not optional:
	// the welcome pass zeroes the roster wave's AOL cap while
	// aolRotationActive(vertical) — true for every gated-prefix (internal
	// insurance) lane — so this companion pass is the ONLY AOL path for those
	// verticals. Without it every governed AOL send is ungoverned. (The governor
	// runs a SECOND time inside the kept layers below, under the same
	// phaseAOLRotate label; it is idempotent — a pure read + clamp — and there
	// it supplies the ceiling the enforced branch min()s the grant against.)
	perISPCaps = po.applyDomainGovernor(ctx, brand, v.vertical, phaseAOLRotate, perISPCaps)
	perISPCaps = keepOnlyISPCaps(perISPCaps, "aol")
	if !capsAnyPositive(perISPCaps) {
		// Was silent. REQ-118 WP5: every tick leaves a reason behind, in every
		// mediator mode, so an AOL lane that ships nothing is visible in
		// drip_tick_outcomes instead of merely absent from it.
		po.tickOutcome(ctx, v.vertical, dripsupply.PassAOLRotate, dripsupply.OutcomeSkipped, dripsupply.SkipBudgetExhausted, brand, perISPCaps, 0, "")
		return nil // brand exhausted/held for AOL this tick — next tick rotates on
	}

	// ---- REQ-118: capacity mediation ---------------------------------------
	// The last unmetered spender in the supply chain closes here. phaseAOLRotate
	// is the third cap-chain phase: its kept layers keep AOL (which the roster
	// wave surrendered) and keep nothing else, so the grant this asks for can
	// never widen the companion wave into another lane. At ModeOff — and for a
	// governed/warm-up brand, or a brand with no resolvable sending domain —
	// grantWaveCapacity returns (nil, perISPCaps, nil) and everything below is
	// byte-identical to the unmetered path.
	alloc, effCaps, mErr := po.grantWaveCapacity(ctx, v, brand, dripsupply.PassAOLRotate,
		phaseAOLRotate, dripsupply.TouchClassIntro, "", po.cfg.MaxWaveSize, perISPCaps, false)
	if mErr != nil {
		po.tickOutcome(ctx, v.vertical, dripsupply.PassAOLRotate, dripsupply.OutcomeFailed, "grant_capacity", brand, perISPCaps, 0, "")
		return fmt.Errorf("aol_rotate grant_capacity: %w", mErr)
	}
	if alloc.ShouldSkip() {
		po.tickOutcome(ctx, v.vertical, dripsupply.PassAOLRotate, dripsupply.OutcomeSkipped, alloc.SkipReason(), brand, perISPCaps, 0, "")
		return nil
	}
	perISPCaps = effCaps

	claimed, err := po.claimWaveByCaps(ctx, v.vertical, brand, perISPCaps, po.cfg.MaxWaveSize, alloc)
	if err != nil {
		_ = alloc.Release(ctx, "claim_failed")
		if errors.Is(err, dripsupply.ErrNoPositiveGrant) {
			// Every enforced ISP granted 0 — the contract is spent for this
			// (domain, aol, lane) this interval. A zero, not a failure.
			po.tickOutcome(ctx, v.vertical, dripsupply.PassAOLRotate, dripsupply.OutcomeZero, dripsupply.SkipNoPositiveGrant, brand, perISPCaps, 0, "")
			return nil
		}
		po.tickOutcome(ctx, v.vertical, dripsupply.PassAOLRotate, dripsupply.OutcomeFailed, "claim_records", brand, perISPCaps, 0, "")
		return fmt.Errorf("aol_rotate claim: %w", err)
	}
	if len(claimed) == 0 {
		_ = alloc.Release(ctx, dripsupply.ZeroNoRecordsClaimed)
		po.tickOutcome(ctx, v.vertical, dripsupply.PassAOLRotate, dripsupply.OutcomeZero, dripsupply.ZeroNoRecordsClaimed, brand, perISPCaps, 0, "")
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
		_ = alloc.Release(ctx, dripsupply.ZeroAllDeferred)
		po.tickOutcome(ctx, v.vertical, dripsupply.PassAOLRotate, dripsupply.OutcomeZero, dripsupply.ZeroAllDeferred, brand, perISPCaps, 0, "")
		return nil
	}

	creative, err := po.resolveCreative(ctx, v.vertical, brand)
	if err != nil {
		_ = po.releaseClaim(ctx, keep)
		_ = alloc.Release(ctx, "resolve_creative_failed")
		po.tickOutcome(ctx, v.vertical, dripsupply.PassAOLRotate, dripsupply.OutcomeFailed, "resolve_creative", brand, perISPCaps, 0, "")
		return fmt.Errorf("aol_rotate resolve_creative vertical=%s brand=%s: %w", v.vertical, brand, err)
	}
	subscriberIDs, err := po.promoteToSubscribers(ctx, v, keep)
	if err != nil {
		_ = po.releaseClaim(ctx, keep)
		_ = alloc.Release(ctx, "promote_failed")
		po.tickOutcome(ctx, v.vertical, dripsupply.PassAOLRotate, dripsupply.OutcomeFailed, "promote_subscribers", brand, perISPCaps, 0, "")
		return fmt.Errorf("aol_rotate promote: %w", err)
	}
	lastCampaignID, deployedCount := po.deployWaveGroups(ctx, v, brand, creative, keep, subscriberIDs, "")
	if deployedCount == 0 {
		_ = alloc.Release(ctx, "deploy_all_groups_failed")
		po.tickOutcome(ctx, v.vertical, dripsupply.PassAOLRotate, dripsupply.OutcomeFailed, "deploy_all_groups_failed", brand, perISPCaps, 0, "")
		return fmt.Errorf("aol_rotate: all wave groups failed vertical=%s brand=%s", v.vertical, brand)
	}
	// Settle the reservation against what actually shipped — the same commit
	// every other spender makes. A no-op on an unenforced allocation
	// (EnforcedCaps() == nil), which is what ModeOff hands back.
	po.commitWave(ctx, alloc, keep, deployedCount, lastCampaignID)
	po.tickOutcome(ctx, v.vertical, dripsupply.PassAOLRotate, dripsupply.OutcomeFired, "", brand, perISPCaps, deployedCount, lastCampaignID)
	log.Printf("[PartnerDripOrchestrator] aol_rotate wave: vertical=%s brand=%s size=%d", v.vertical, brand, len(keep))
	return nil
}
