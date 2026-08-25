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
