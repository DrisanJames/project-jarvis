package worker

import (
	"context"
	"testing"
)

// The overlay must be ADDITIVE: an ISP absent from partner_drip_isp_caps keeps
// its compiled value, so a partial/empty table can never silently zero a lane.
func TestBasePerISPCapsOverlayIsAdditive(t *testing.T) {
	po := &PartnerDripOrchestrator{}
	po.cfg.PerISPCapPerWave = map[string]int{"gmail": 0, "yahoo": 32, "aol": 60}

	// no overlay -> compiled values verbatim
	got := po.basePerISPCaps()
	for isp, want := range map[string]int{"gmail": 0, "yahoo": 32, "aol": 60} {
		if got[isp] != want {
			t.Fatalf("no overlay: %s = %d, want %d", isp, got[isp], want)
		}
	}

	// overlay RAISES gmail (the whole point — governor can only lower)
	po.ispCapCache = map[string]int{"gmail": 25}
	got = po.basePerISPCaps()
	if got["gmail"] != 25 {
		t.Fatalf("overlay did not raise gmail: got %d want 25", got["gmail"])
	}
	if got["yahoo"] != 32 || got["aol"] != 60 {
		t.Fatalf("overlay clobbered untouched ISPs: %+v", got)
	}
}

// A new lane must be addable with a DB row only; the compiled map remains the
// fallback so existing brands are unaffected by a failed/empty load.
func TestResolveBrandSendingDomainOverlay(t *testing.T) {
	t.Cleanup(func() { setDynamicBrandDomains(map[string]string{}) })

	if _, ok := resolveBrandSendingDomain("wcl"); ok {
		t.Fatal("wcl resolved before any overlay was set")
	}
	if d, ok := resolveBrandSendingDomain("db"); !ok || d != "em.discountblog.com" {
		t.Fatalf("compiled fallback broken: %q %v", d, ok)
	}

	setDynamicBrandDomains(map[string]string{"wcl": "m.wcl-heloc.com"})
	if d, ok := resolveBrandSendingDomain("wcl"); !ok || d != "m.wcl-heloc.com" {
		t.Fatalf("overlay lane not resolved: %q %v", d, ok)
	}
	if d, ok := resolveBrandSendingDomain("WCL "); !ok || d != "m.wcl-heloc.com" {
		t.Fatalf("lookup must be case/space insensitive: %q %v", d, ok)
	}
	// compiled brands still resolve while an overlay is active
	if d, ok := resolveBrandSendingDomain("db"); !ok || d != "em.discountblog.com" {
		t.Fatalf("overlay broke compiled brand: %q %v", d, ok)
	}
	// empty overlay value must NOT mask the compiled map
	setDynamicBrandDomains(map[string]string{"db": ""})
	if d, ok := resolveBrandSendingDomain("db"); !ok || d != "em.discountblog.com" {
		t.Fatalf("empty overlay masked compiled brand: %q %v", d, ok)
	}
}

// A DB roster must confine a vertical to exactly the brands configured, and an
// empty/absent overlay must leave the compiled behaviour untouched.
func TestBrandRosterForOverlay(t *testing.T) {
	t.Cleanup(func() { setDynamicRoster(map[string][]string{}) })

	base := brandRosterFor("refi_heloc")
	if len(base) != len(dripBrands) {
		t.Fatalf("no overlay should fall back to dripBrands: got %d want %d", len(base), len(dripBrands))
	}

	setDynamicRoster(map[string][]string{"refi_heloc": {"wcl"}})
	got := brandRosterFor("refi_heloc")
	if len(got) != 1 || got[0] != "wcl" {
		t.Fatalf("overlay roster not applied: %v", got)
	}
	// an unrelated vertical is unaffected
	if other := brandRosterFor("term_life"); len(other) != len(dripBrands) {
		t.Fatalf("overlay leaked to another vertical: %v", other)
	}
	// case/space tolerant
	if got := brandRosterFor("  REFI_HELOC "); len(got) != 1 || got[0] != "wcl" {
		t.Fatalf("roster lookup not normalised: %v", got)
	}
}

// The warm-up ISP clamp (yahoo/aol only) is derived from the COMPILED roster.
// A DB lane must never be dragged into it — that would silently drop a lane's
// non-yahoo audience to zero.
func TestDBRosterDoesNotJoinWarmupSet(t *testing.T) {
	t.Cleanup(func() { setDynamicRoster(map[string][]string{}) })
	setDynamicRoster(map[string][]string{"refi_heloc": {"wcl"}})
	if warmupRosterBrands["wcl"] {
		t.Fatal("DB roster brand leaked into warmupRosterBrands — would force yahoo/aol only")
	}
}

// The FOLLOW-UP pass must honour the DB roster too — this is the gap that kept
// the wcl lane unreachable after brandRosterFor went dynamic (welcome used the
// roster; pickNextFollowupBrand still walked the compiled dripBrands).
func TestPickNextFollowupBrandUsesDBRoster(t *testing.T) {
	t.Cleanup(func() { setDynamicRoster(map[string][]string{}) })
	po := &PartnerDripOrchestrator{}
	state := &followupState{brandIndex: 5}

	setDynamicRoster(map[string][]string{"refi_heloc": {"wcl"}})
	got, err := po.pickNextFollowupBrand(context.Background(), "refi_heloc", state)
	if err != nil || got != "wcl" {
		t.Fatalf("DB-roster vertical: got %q err %v, want wcl", got, err)
	}
	if state.brandIndex != 5 {
		t.Fatalf("DB-roster pick mutated the shared rotation index: %d", state.brandIndex)
	}
	// an unrostered vertical keeps the exact legacy rotation
	got, err = po.pickNextFollowupBrand(context.Background(), "term_life", state)
	if err != nil || got != dripBrands[5] {
		t.Fatalf("legacy vertical: got %q err %v, want %q", got, err, dripBrands[5])
	}
}
