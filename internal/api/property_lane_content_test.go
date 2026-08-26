package api

import (
	"os"
	"strings"
	"testing"
)

// ── from-name template guard (production incident 2026-08-25) ───────────────

// TestValidateFromNameNoTemplate_RejectsTheExactStringThatShipped pins the
// literal from-name that reached real inboxes. The drip send path renders
// subject and body through Liquid but NOT from_name, so this went out verbatim
// on 38 campaigns across six lanes.
func TestValidateFromNameNoTemplate_RejectsTheExactStringThatShipped(t *testing.T) {
	shipped := []string{
		`{{ custom.postal_code | default: "Local" }} - Insurance Savings Pro`,
		`{{ custom.postal_code | default: "Local" }} - Insurance Savings Pro Auto Insure`,
		`{{ custom.vehicle | default: "Auto" }} | Insurance Savings Pro`,
	}
	for _, v := range shipped {
		if err := validateFromNameNoTemplate(v); err == nil {
			t.Errorf("from-name %q was ACCEPTED — this is the exact string that reached inboxes", v)
		}
	}
	// Tag-style markup must be rejected too, not just output markup.
	if err := validateFromNameNoTemplate(`{% if x %}A{% endif %}`); err == nil {
		t.Error("{% %} tag markup was accepted")
	}
}

// TestValidateFromNameNoTemplate_AcceptsRealFromNames — the guard must not
// reject the names actually in production, including punctuation and braces
// that are not template delimiters.
func TestValidateFromNameNoTemplate_AcceptsRealFromNames(t *testing.T) {
	ok := []string{
		"Insurance Savings Pro",
		"Insurance Savings Pro Auto Insure",
		"Auto Coverage Map",
		"Driver Policy Line",
		"Simple Insure Auto",
		"Jamie @ Discount Blog",
		"Warranty For You",
		"(offer-center)", // the onboarding path writes this literal
		"Rates {Bazar}",  // single braces are not template markup
	}
	for _, v := range ok {
		if err := validateFromNameNoTemplate(v); err != nil {
			t.Errorf("legitimate from-name %q was rejected: %v", v, err)
		}
	}
}

// TestFromNameGuard_DoesNotCoverRenderedFields is the over-correction guard.
// subject_line and preheader ARE rendered and 36 production rows legitimately
// carry Liquid; extending this validator to them would break working copy.
func TestFromNameGuard_DoesNotCoverRenderedFields(t *testing.T) {
	body, err := os.ReadFile("property_lane_content.go")
	if err != nil {
		t.Skipf("cannot read source: %v", err)
	}
	src := string(body)
	// The validator must be applied to FromName and nothing else.
	if !strings.Contains(src, "validateFromNameNoTemplate(*req.FromName)") {
		t.Fatal("the from-name guard is no longer wired into the touch-copy handler")
	}
	for _, forbidden := range []string{
		"validateFromNameNoTemplate(*req.SubjectLine)",
		"validateFromNameNoTemplate(*req.Preheader)",
	} {
		if strings.Contains(src, forbidden) {
			t.Errorf("%s — subject/preheader ARE rendered; constraining them breaks live copy", forbidden)
		}
	}
}

// TestFromNameGuard_HasAStructuralBackstop — the handler guard only covers
// writers that come through the API. These rows were authored by script, so the
// CHECK constraint is the half that actually would have prevented the incident.
func TestFromNameGuard_HasAStructuralBackstop(t *testing.T) {
	main, err := os.ReadFile("../../cmd/server/main.go")
	if err != nil {
		t.Skipf("cannot read main.go: %v", err)
	}
	src := string(main)
	for _, want := range []string{
		"partner_drip_creatives_fromname_no_template",
		"partner_drip_followup_creatives_fromname_no_template",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("CHECK constraint %s is missing — a script write could reintroduce the outage", want)
		}
	}
}
