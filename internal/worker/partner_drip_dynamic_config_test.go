package worker

import "testing"

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
