package worker

import (
	"os"
	"strings"
	"testing"
)

func TestTouchGapHoursFor(t *testing.T) {
	t.Setenv("PARTNER_DRIP_TOUCH_GAP_BY_PREFIX", "")
	if g := touchGapHoursFor("converters_heloc"); g != 168 {
		t.Fatalf("converter lanes must default to weekly, got %dh", g)
	}
	if g := touchGapHoursFor("refi_heloc"); g != followupTouchGapHours {
		t.Fatalf("normal lanes keep the 24h gap, got %dh", g)
	}
	t.Setenv("PARTNER_DRIP_TOUCH_GAP_BY_PREFIX", "converters_=72, refi_=48")
	if g := touchGapHoursFor("converters_sams"); g != 72 {
		t.Fatalf("env override ignored, got %dh", g)
	}
	if g := touchGapHoursFor("refi_heloc"); g != 48 {
		t.Fatalf("second pair ignored, got %dh", g)
	}
	// garbage pair falls through to defaults
	t.Setenv("PARTNER_DRIP_TOUCH_GAP_BY_PREFIX", "converters_=zero")
	if g := touchGapHoursFor("converters_sams"); g != 168 {
		t.Fatalf("bad value must fall back to the built-in weekly default, got %dh", g)
	}
}

func TestHomeBrandPinSQL(t *testing.T) {
	t.Setenv("PARTNER_DRIP_CONVERTERS_PIN_DISABLED", "")
	if got := homeBrandPinSQL("refi_heloc", "rru", ""); got != "" {
		t.Fatalf("non-converter lanes must be pin-free (byte-identical SQL), got %q", got)
	}
	got := homeBrandPinSQL("converters_heloc", "QF", "q")
	for _, want := range []string{"q.extra_metadata->>'home_brand'", "= 'qf'", "COALESCE("} {
		if !strings.Contains(got, want) {
			t.Errorf("pin missing %q:\n%s", want, got)
		}
	}
	// unpinned records stay claimable by anyone (the COALESCE-empty branch)
	if !strings.Contains(got, "'') = ''") {
		t.Errorf("records without a home_brand must remain claimable:\n%s", got)
	}
	t.Setenv("PARTNER_DRIP_CONVERTERS_PIN_DISABLED", "1")
	if got := homeBrandPinSQL("converters_heloc", "qf", ""); got != "" {
		t.Fatal("kill switch inert")
	}
}

// Converters must never exit on click (same continuation rule as internal).
func TestConvertersNeverExit(t *testing.T) {
	t.Setenv("PARTNER_DRIP_CLICKER_EXIT_RESTORED", "")
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_VERTICALS", "")
	if got := engagedExitSQL("converters_sams", ""); got != "" {
		t.Fatalf("converter clicker must not exit, got %q", got)
	}
	if !strings.Contains(engagedExitAnyVerticalSQL("q"), "'converters_%'") {
		t.Fatal("cross-vertical exit does not spare converter lanes")
	}
}

// The pin must be applied in all four claim blocks and the gap must be
// vertical-aware at the single stamp point.
func TestConverterMechanicsWired(t *testing.T) {
	src, err := os.ReadFile("partner_drip_orchestrator.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if n := strings.Count(body, "+pin+`"); n != 4 {
		t.Fatalf("home-brand pin wired into %d claim blocks, want 4", n)
	}
	if !strings.Contains(body, "touchGapHoursFor(vertical)") {
		t.Fatal("markMailed does not use the vertical-aware gap")
	}
	if strings.Contains(body, "gap := time.Duration(followupTouchGapHours) * time.Hour") {
		t.Fatal("hardcoded 24h gap survives in markMailed")
	}
}
