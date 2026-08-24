package api

import (
	"os"
	"testing"
)

// Operator ruling 2026-08-24: capped inclusion-segment draws fill
// most-recently-engaged first (SDS recency for the campaign's sending
// domain). These pin the flag semantics — recency is the DEFAULT, the kill
// switch falls back to rotation, and rotation's own kill switch still works
// underneath it.
func TestRecencyAudienceDrawFlag(t *testing.T) {
	t.Setenv("RECENCY_AUDIENCE_DRAW_DISABLED", "")
	if !recencyAudienceDrawEnabled() {
		t.Fatal("recency draw must be ON by default")
	}
	t.Setenv("RECENCY_AUDIENCE_DRAW_DISABLED", "true")
	if recencyAudienceDrawEnabled() {
		t.Fatal("kill switch must disable the recency draw")
	}
	// With recency disabled, rotation remains available under its own flag.
	t.Setenv("DISABLE_ROTATING_AUDIENCE_SELECTION", "")
	if !rotatingAudienceSelectionEnabled() {
		t.Fatal("rotation fallback must remain available")
	}
	os.Unsetenv("RECENCY_AUDIENCE_DRAW_DISABLED")
}
