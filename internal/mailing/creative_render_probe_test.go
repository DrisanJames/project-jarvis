package mailing

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// Renders every creative in PROBE_DIR through the REAL production engine and
// fails on anything left unrendered, in both a fully-personalized context and
// the bare context that ~90% of recipients actually have (see Claude memory
// liquid-personalization-guard-idioms for the measured coverage).
//
// This exists because the sidecar runner (agents/jobs/remail_auto_nonclickers.py)
// can only emulate `{{ x | default: \"y\" }}` in Python. Its emulator does NOT
// implement `{% assign %}` / `{% if %}`, so on 2026-08-30 it reported every
// approved offer creative as having 30+ \"unresolved tokens\" — the exact
// assign-through-default guard idiom the creatives are REQUIRED to use. A gate
// that cries wolf on correct input trains you to click through it, so the
// runner now calls this test instead of trusting its own renderer.
func TestCreativeRenderProbe(t *testing.T) {
	dir := os.Getenv("PROBE_DIR")
	if dir == "" {
		t.Skip("PROBE_DIR unset")
	}
	ts := NewTemplateService()
	leftover := regexp.MustCompile(`\{%[^%]*%\}|\{\{[^}]*\}\}`)

	ctxs := map[string]map[string]interface{}{
		"rich": {
			"first_name": "Bobbie", "last_name": "Nguyen", "email": "b@example.com",
			"custom": map[string]interface{}{
				"city": "Uvalde", "state": "TX", "postal_code": "78801",
				"signup_url": "https://www.example.com/quote?x=1", "tid": "abc123",
			},
			"subscriber": map[string]interface{}{"id": "sub-1"},
			"brand":      map[string]interface{}{"domain": "discountblog.com", "name": "Discount Blog"},
			"system":     map[string]interface{}{"current_date": "today", "dispatch_date": "today"},
		},
		"bare": { // the ~90% case: no name, no geo, no custom at all
			"first_name": "", "last_name": "", "email": "b@example.com",
			"custom":     map[string]interface{}{},
			"subscriber": map[string]interface{}{"id": "sub-2"},
			"brand":      map[string]interface{}{"domain": "discountblog.com", "name": "Discount Blog"},
			"system":     map[string]interface{}{"current_date": "today", "dispatch_date": "today"},
		},
	}

	files, _ := filepath.Glob(filepath.Join(dir, "*.html"))
	if len(files) == 0 {
		t.Fatalf("no creatives in %s", dir)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, ck := range []string{"rich", "bare"} {
			out, err := ts.Render("", string(raw), ctxs[ck])
			if err != nil {
				t.Errorf("%s [%s]: RENDER ERROR %v", filepath.Base(f), ck, err)
				continue
			}
			left := leftover.FindAllString(out, -1)
			t.Logf("%-12s [%s] in=%6db out=%6db leftover=%d", filepath.Base(f), ck, len(raw), len(out), len(left))
			for i, l := range left {
				if i < 5 {
					t.Errorf("   %s [%s] LEFTOVER: %q", filepath.Base(f), ck, l)
				}
			}
		}
	}
}
