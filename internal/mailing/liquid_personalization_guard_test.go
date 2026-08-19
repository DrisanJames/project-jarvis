package mailing

import "testing"

// Guards the personalization idioms the approved offer creatives depend on.
//
// osteele/liquid v1.7.0 makes BOTH naive guards wrong, in opposite directions:
//
//	{% if x %}              -> FALSE when x is missing, TRUE when x is ""   (renders "Hi ,")
//	{% if x != "" %}        -> TRUE when x is MISSING                       (renders "Hi ,")
//
// The first trap is documented; the second is not, and it is the dangerous one
// for nested custom.* fields, which are genuinely absent for most subscribers
// (measured 2026-08-18: custom.city present on 4.1% of the 30D-clicker tier).
// Top-level subscriber fields are always PRESENT-but-empty because
// buildRenderContext assigns them unconditionally (send_worker.go:3231), so
// `!= ""` happens to be safe there — but it is not safe in general.
//
// The one idiom correct in all four states is assign-through-default:
//
//	{% assign v = x | default: "" %}{% if v != "" %} ... {% endif %}
func TestLiquidPersonalizationGuardIdioms(t *testing.T) {
	ts := NewTemplateService()

	ctxMissing := map[string]interface{}{"custom": map[string]interface{}{}}
	ctxEmpty := map[string]interface{}{"custom": map[string]interface{}{"city": ""}}
	ctxSet := map[string]interface{}{"custom": map[string]interface{}{"city": "Mesa"}}

	render := func(tpl string, ctx map[string]interface{}) string {
		out, err := ts.Render("", tpl, ctx)
		if err != nil {
			t.Fatalf("render %q: %v", tpl, err)
		}
		return out
	}

	// The UNSAFE idiom — pinned so a future engine bump surfaces the change.
	unsafe := `{% if custom.city != "" %}[{{ custom.city }}]{% else %}NONE{% endif %}`
	if got := render(unsafe, ctxMissing); got != "[]" {
		t.Errorf("engine behavior changed: `!= \"\"` on a MISSING nested key gave %q, want %q", got, "[]")
	}

	// The bare-truthiness trap — empty string is truthy in this engine.
	bare := `{% if custom.city %}YES{% else %}NO{% endif %}`
	if got := render(bare, ctxEmpty); got != "YES" {
		t.Errorf("engine behavior changed: bare if on \"\" gave %q, want YES", got)
	}

	// The SAFE idiom — must be correct in all three states.
	safe := `{% assign v = custom.city | default: "" %}{% if v != "" %}[{{ v }}]{% else %}NONE{% endif %}`
	for _, tc := range []struct {
		name string
		ctx  map[string]interface{}
		want string
	}{
		{"missing", ctxMissing, "NONE"},
		{"empty", ctxEmpty, "NONE"},
		{"set", ctxSet, "[Mesa]"},
	} {
		if got := render(safe, tc.ctx); got != tc.want {
			t.Errorf("safe idiom / %s: got %q, want %q", tc.name, got, tc.want)
		}
	}

	// Inline default filter is safe for both absent and empty (this is why the
	// shipping v3-personalized proof renders cleanly rather than "Hi ,").
	inline := `{{ custom.city | default: "your area" }}`
	for _, tc := range []struct {
		ctx  map[string]interface{}
		want string
	}{{ctxMissing, "your area"}, {ctxEmpty, "your area"}, {ctxSet, "Mesa"}} {
		if got := render(inline, tc.ctx); got != tc.want {
			t.Errorf("inline default: got %q, want %q", got, tc.want)
		}
	}
}
