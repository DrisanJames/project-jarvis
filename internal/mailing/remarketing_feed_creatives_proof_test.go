package mailing

// Proof harness for the five Auto Insurance Remarketing feed creatives
// (operator 2026-08-14; datasets internal-auto-insurance-v3..v7). Renders each
// template through the SAME TemplateService the live send path uses and proves:
//
//   1. tokenid={{ custom.tid | default: "" }} resolves to the subscriber's tid
//      when present and to empty (not the literal token) when absent — the tid
//      is the feed's ONLY attribution key (see internal-auto lane precedent).
//   2. The empty-persona worst case degrades to the written fallbacks with zero
//      residual Liquid syntax (empty string is TRUTHY in osteele/liquid, so a
//      creative that used if-tags instead of default-filters would fail here).
//
// The HTML sources live in the parent repo (agents/jobs/data/
// remarketing_creatives/); the test skips when that directory is absent so the
// module still tests standalone.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func remarketingCreativesDir(t *testing.T) string {
	t.Helper()
	if d := os.Getenv("REMARKETING_CREATIVES_DIR"); d != "" {
		return d
	}
	// upside-down/internal/mailing -> mailing-saas/agents/jobs/data/remarketing_creatives
	d := filepath.Join("..", "..", "..", "agents", "jobs", "data", "remarketing_creatives")
	if _, err := os.Stat(d); err != nil {
		t.Skipf("remarketing creatives dir not present (%s) — parent-repo layout only", d)
	}
	return d
}

func TestRemarketingFeedCreativesRenderThroughSendPathEngine(t *testing.T) {
	files := map[string]string{
		"v3_vehiclecoveragecenter.html": "vehiclecoveragecenter.com/coverage-match?id=ebc74c",
		"v4_vehiclequotefinder.html":    "vehiclequotefinder.com/coverage-match?id=ae5d3a",
		"v5_autopolicybridge.html":      "autopolicybridge.com/coverage-match?id=681626",
		"v6_autocoveragemap.html":       "autocoveragemap.com/coverage-match?id=ec38c3",
		"v7_driverpolicyline.html":      "driverpolicyline.com/coverage-match?id=e928a8",
	}
	dir := remarketingCreativesDir(t)
	ts := NewTemplateService()

	fullPersona := map[string]interface{}{
		"first_name": "Lorena",
		"full_name":  "Lorena Solos",
		"custom": map[string]interface{}{
			"tid":         "496e49454c785965383241526a33664964522f4b4a673d3d",
			"city":        "Burlington",
			"state":       "NC",
			"postal_code": "27217",
			"vehicle":     "2019 Honda CR-V",
			"signup_url":  "https://quotes.ratesavings.org",
		},
	}
	emptyPersona := map[string]interface{}{
		"first_name": "",
		"full_name":  "",
		"custom":     map[string]interface{}{},
	}

	for file, moneyPrefix := range files {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			tpl := string(raw)
			if !strings.Contains(tpl, moneyPrefix) {
				t.Fatalf("template does not carry its own money link %q", moneyPrefix)
			}

			full, err := ts.Render("", tpl, fullPersona)
			if err != nil {
				t.Fatalf("render(full persona): %v", err)
			}
			empty, err := ts.Render("", tpl, emptyPersona)
			if err != nil {
				t.Fatalf("render(empty persona): %v", err)
			}

			// tid reaches the money link when present, and never ships literal.
			if !strings.Contains(full, "tokenid=496e49454c785965383241526a33664964522f4b4a673d3d") {
				t.Errorf("full persona: tid did not reach tokenid= in the money link")
			}
			if !strings.Contains(empty, "tokenid=") || strings.Contains(empty, "tokenid={{") {
				t.Errorf("empty persona: tokenid must render empty, never literal")
			}

			// Personalization present with the full persona.
			for _, want := range []string{"Lorena", "Burlington", "2019 Honda CR-V"} {
				if !strings.Contains(full, want) {
					t.Errorf("full persona: rendered HTML missing %q", want)
				}
			}

			// Zero residual Liquid syntax in either rendering.
			for label, got := range map[string]string{"full": full, "empty": empty} {
				if i := strings.Index(got, "{{"); i >= 0 {
					end := i + 60
					if end > len(got) {
						end = len(got)
					}
					t.Errorf("%s persona: residual token syntax would ship: …%s…", label, got[i:end])
				}
			}
		})
	}
}
