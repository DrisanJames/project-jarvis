package worker

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Rotation must cycle through the whole estate set (not pin one brand) and be
// deterministic within a 15-minute slot so both instances agree.
func TestAOLRotationBrand_CyclesAndStable(t *testing.T) {
	t.Setenv("PARTNER_DRIP_AOL_ROTATE_BRANDS", "")
	base := time.Unix(1_700_000_000/(15*60)*(15*60), 0).UTC()
	seen := map[string]bool{}
	for i := 0; i < len(defaultAOLRotateBrands); i++ {
		b := aolRotationBrand("internal_auto_insurance_v5", base.Add(time.Duration(i)*15*time.Minute))
		seen[b] = true
	}
	if len(seen) != len(defaultAOLRotateBrands) {
		t.Fatalf("full cycle hit %d distinct brands, want %d", len(seen), len(defaultAOLRotateBrands))
	}
	// stable within a slot
	if aolRotationBrand("internal_auto_insurance_v5", base) != aolRotationBrand("internal_auto_insurance_v5", base.Add(7*time.Minute)) {
		t.Fatal("brand changed within one rotation slot")
	}
	// verticals fan across brands in the same slot
	if aolRotationBrand("internal_auto_insurance_v5", base) == aolRotationBrand("internal_auto_insurance_v7", base) &&
		aolRotationBrand("internal_auto_insurance_v5", base.Add(15*time.Minute)) == aolRotationBrand("internal_auto_insurance_v7", base.Add(15*time.Minute)) {
		t.Fatal("verticals never fan out across brands")
	}
}

func TestAOLRotationScopeAndKill(t *testing.T) {
	t.Setenv("PARTNER_DRIP_AOL_ROTATE_DISABLED", "")
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_VERTICALS", "")
	if !aolRotationActive("internal_auto_insurance_v7") {
		t.Fatal("internal vertical should rotate")
	}
	if aolRotationActive("refi_heloc") {
		t.Fatal("partner vertical must not rotate")
	}
	t.Setenv("PARTNER_DRIP_AOL_ROTATE_DISABLED", "1")
	if aolRotationActive("internal_auto_insurance_v7") {
		t.Fatal("kill switch inert")
	}
	// blank brand override falls back to the default set, never empty
	t.Setenv("PARTNER_DRIP_AOL_ROTATE_BRANDS", " , ")
	if len(aolRotateBrands()) != len(defaultAOLRotateBrands) {
		t.Fatal("blank override must fall back to the estate set")
	}
}

// The roster wave must cede AOL while rotation is on — one AOL path.
func TestRosterWaveCedesAOLUnderRotation(t *testing.T) {
	src, err := readOrchestratorSource()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(src, `if aolRotationActive(v.vertical) {
		perISPCaps["aol"] = 0`) {
		t.Fatal("roster welcome wave does not cede AOL to the rotated pass")
	}
	if !strings.Contains(src, "po.processAOLRotated(po.ctx, v)") {
		t.Fatal("rotated AOL pass is not wired into the welcome tick")
	}
}

func readOrchestratorSource() (string, error) {
	b, err := os.ReadFile("partner_drip_orchestrator.go")
	return string(b), err
}

// The rotated companion pass is the ONLY AOL path for the gated verticals
// (the roster wave zeroes its AOL cap while rotation is active), so it must
// run the per-domain x ISP governor in the same chain position the welcome
// pass does: after applyBrandIntroBudgets, before keepOnlyISPCaps.
func TestAOLRotatedRunsDomainGovernorInChainOrder(t *testing.T) {
	b, err := os.ReadFile("partner_drip_aol_rotate.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	intro := strings.Index(src, "perISPCaps = po.applyBrandIntroBudgets(ctx, brand, perISPCaps)")
	// The phase label is the named constant now (phaseAOLRotate == "aol_rotate"),
	// so this guard follows the symbol rather than the literal.
	gov := strings.Index(src, `po.applyDomainGovernor(ctx, brand, v.vertical, phaseAOLRotate, perISPCaps)`)
	keep := strings.Index(src, `perISPCaps = keepOnlyISPCaps(perISPCaps, "aol")`)
	if gov < 0 {
		t.Fatal("processAOLRotated does not apply the domain governor — the gated lanes' AOL volume is ungoverned")
	}
	if intro < 0 || keep < 0 {
		t.Fatal("aol_rotate cap chain no longer matches the welcome pass")
	}
	if !(intro < gov && gov < keep) {
		t.Fatalf("governor out of chain order: intro=%d gov=%d keep=%d", intro, gov, keep)
	}
}
