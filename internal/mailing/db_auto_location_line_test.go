package mailing

import (
	"regexp"
	"strings"
	"testing"
)

// The DiscountBlog auto-insurance drip renders a composed
// "City, State ZIP" line inside an editable details panel, and the same
// composition appears in the subject preheaders. Naive composition breaks in a
// way the operator caught on 2026-08-20: with `city` absent the hard comma has
// nothing in front of it and the line ships as ", DE 19703".
//
// Only assign-through-default carries its own punctuation safely (the idiom
// pinned by TestLiquidPersonalizationGuardIdioms). This test renders the exact
// production string through the real TemplateService across every combination
// of present / empty / missing, and fails on any dangling punctuation — a
// leading comma, a doubled space, or a comma with nothing before it.
func TestDBAutoLocationLineNeverDanglesPunctuation(t *testing.T) {
	ts := NewTemplateService()

	// The shipped composition. Each fragment owns the separator that PRECEDES
	// its own value, so an absent field removes its punctuation with it.
	const locationLine = `{% assign c = custom.city | default: "" %}` +
		`{% assign s = custom.state | default: "" %}` +
		`{% assign z = custom.postal_code | default: "" %}` +
		`{% if c != "" %}{{ c }}{% endif %}` +
		`{% if s != "" %}{% if c != "" %}, {% endif %}{{ s }}{% endif %}` +
		`{% if z != "" %}{% if c != "" or s != "" %} {% endif %}{{ z }}{% endif %}` +
		`{% if c == "" and s == "" and z == "" %}On file{% endif %}`

	type field struct {
		name string
		vals []interface{} // present, empty, missing sentinel
	}
	states := []struct {
		label string
		set   bool
		val   string
	}{
		{"set", true, "X"},
		{"empty", true, ""},
		{"missing", false, ""},
	}

	danglers := regexp.MustCompile(`^[,\s]|,\s*$|,,|\s{2,}|,\s*,`)

	// The NAME row has the same shape of hole: first_name and last_name are
	// top-level fields, so both are present-but-empty for most subscribers, and
	// naive `{{ first_name }} {{ last_name }}` renders a bare space — a details
	// row that looks blank rather than one that reads "On file".
	const nameLine = `{% assign f = first_name | default: "" %}` +
		`{% assign l = last_name | default: "" %}` +
		`{% if f != "" or l != "" %}{{ f }}{% if f != "" and l != "" %} {% endif %}{{ l }}` +
		`{% else %}On file{% endif %}`
	for _, fs := range states {
		for _, ls := range states {
			ctx := map[string]interface{}{}
			if fs.set {
				ctx["first_name"] = strings.Replace(fs.val, "X", "Drisan", 1)
			}
			if ls.set {
				ctx["last_name"] = strings.Replace(ls.val, "X", "James", 1)
			}
			out, err := ts.Render("", nameLine, ctx)
			if err != nil {
				t.Fatalf("name first=%s last=%s: render: %v", fs.label, ls.label, err)
			}
			if strings.TrimSpace(out) == "" {
				t.Errorf("name first=%s last=%s: rendered blank — the row would look empty", fs.label, ls.label)
			}
			if danglers.MatchString(out) {
				t.Errorf("name first=%s last=%s: dangling whitespace in %q", fs.label, ls.label, out)
			}
			t.Logf("name first=%-7s last=%-7s -> %q", fs.label, ls.label, out)
		}
	}

	for _, cs := range states {
		for _, ss := range states {
			for _, zs := range states {
				custom := map[string]interface{}{}
				if cs.set {
					custom["city"] = strings.Replace(cs.val, "X", "Claymont", 1)
				}
				if ss.set {
					custom["state"] = strings.Replace(ss.val, "X", "DE", 1)
				}
				if zs.set {
					custom["postal_code"] = strings.Replace(zs.val, "X", "19703", 1)
				}
				out, err := ts.Render("", locationLine, map[string]interface{}{"custom": custom})
				if err != nil {
					t.Fatalf("city=%s state=%s zip=%s: render: %v", cs.label, ss.label, zs.label, err)
				}
				if out == "" {
					t.Errorf("city=%s state=%s zip=%s: rendered EMPTY — the panel row would collapse", cs.label, ss.label, zs.label)
					continue
				}
				if danglers.MatchString(out) {
					t.Errorf("city=%s state=%s zip=%s: dangling punctuation in %q", cs.label, ss.label, zs.label, out)
				}
				t.Logf("city=%-7s state=%-7s zip=%-7s -> %q", cs.label, ss.label, zs.label, out)
			}
		}
	}
}
