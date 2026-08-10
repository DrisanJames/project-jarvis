package api

import (
	"strings"
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// gateCreativeBroken is the 2026-08-10 incident creative: an unterminated
// Liquid if-tag inside an HTML comment. Liquid tokenizes tags inside comments,
// so the parse aborts and TemplateService.Render hands back the raw source.
const gateCreativeBroken = `<html><body>
<!-- editor note: {% if subscriber.first_name %} greet by name -->
<p>Hello {{ first_name }}</p>
</body></html>`

const gateCreativeGood = `<html><body>
{% if first_name %}<p>Hello {{ first_name }}</p>{% else %}<p>Hello</p>{% endif %}
<a href="{{ system.unsubscribe_url }}">Unsubscribe</a>
</body></html>`

func gateInput(v engine.ContentVariant) engine.PMTACampaignInput {
	return engine.PMTACampaignInput{
		Name:     "test-campaign",
		Variants: []engine.ContentVariant{v},
	}
}

// TestValidateVariantTemplates_RejectsUnterminatedTag is the deploy-layer
// regression guard: a creative that cannot be parsed must be rejected at the
// door, not discovered one message at a time by 25 send workers.
func TestValidateVariantTemplates_RejectsUnterminatedTag(t *testing.T) {
	err := validateVariantTemplates(gateInput(engine.ContentVariant{
		VariantName: "A",
		Subject:     "Your weekly deals",
		HTMLContent: gateCreativeBroken,
	}))
	if err == nil {
		t.Fatal("unparseable creative was ACCEPTED at deploy — the gate is inert")
	}
	if _, ok := err.(*deployInputError); !ok {
		t.Errorf("error type = %T, want *deployInputError (maps to HTTP 400)", err)
	}
	if !strings.Contains(err.Error(), "raw source") {
		t.Errorf("error should tell the operator what would have happened, got: %v", err)
	}
	if !strings.Contains(err.Error(), "html_content") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
}

// TestValidateVariantTemplates_RejectsBrokenSubject — the subject is
// recipient-visible too, so it gets the same gate.
func TestValidateVariantTemplates_RejectsBrokenSubject(t *testing.T) {
	err := validateVariantTemplates(gateInput(engine.ContentVariant{
		VariantName: "A",
		Subject:     "Deals {% if first_name %}for you",
		HTMLContent: gateCreativeGood,
	}))
	if err == nil {
		t.Fatal("unparseable subject was ACCEPTED at deploy")
	}
	if !strings.Contains(err.Error(), "subject") {
		t.Errorf("error should name the subject field, got: %v", err)
	}
}

// TestValidateVariantTemplates_AcceptsValidCreative is the false-positive
// guard: real creatives use Liquid conditionals heavily, and blocking a valid
// board would be a worse outage than the one being fixed.
func TestValidateVariantTemplates_AcceptsValidCreative(t *testing.T) {
	if err := validateVariantTemplates(gateInput(engine.ContentVariant{
		VariantName:  "A",
		Subject:      "Hello {{ first_name | default: 'there' }}",
		PreviewText:  "This week's picks",
		HTMLContent:  gateCreativeGood,
		PlainContent: "Hello {{ first_name }}",
	})); err != nil {
		t.Fatalf("valid creative was rejected: %v", err)
	}
}

// TestValidateVariantTemplates_EmptyFieldsSkipped — empty optional fields must
// not trip the gate (empty HTML is caught by its own earlier check).
func TestValidateVariantTemplates_EmptyFieldsSkipped(t *testing.T) {
	if err := validateVariantTemplates(gateInput(engine.ContentVariant{
		VariantName: "A",
		Subject:     "Plain subject",
		HTMLContent: "<p>no liquid at all</p>",
	})); err != nil {
		t.Fatalf("creative with empty optional fields was rejected: %v", err)
	}
}

// TestValidateVariantTemplates_KillSwitch proves
// DISABLE_TEMPLATE_PARSE_GATE=true is the one-move rollback if this gate ever
// blocks a legitimate send-day board.
func TestValidateVariantTemplates_KillSwitch(t *testing.T) {
	t.Setenv("DISABLE_TEMPLATE_PARSE_GATE", "true")
	if err := validateVariantTemplates(gateInput(engine.ContentVariant{
		VariantName: "A",
		HTMLContent: gateCreativeBroken,
	})); err != nil {
		t.Fatalf("kill switch did not disable the gate: %v", err)
	}
}
