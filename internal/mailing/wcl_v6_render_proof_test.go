package mailing

// Render proof for the WCL HELOC v6 set (base + v6a/b/c/d).
//
// The v6 review page asserts "25 renders through the production engine: zero
// unresolved tokens, zero `Hi ,` artifacts". One of that page's other claims —
// that custom.loan_type/property_type "render for the feed" — turned out to be
// false against the live drip (personaFieldsFromExtra never emitted those keys;
// 2 of 6,071 promoted subscribers had loan_type, zero had property_type). So
// the render claim is re-proved here rather than inherited, through the SAME
// TemplateService the send path uses (send_worker.go:1841).
//
// Persona shapes mirror the measured feed, not a convenient one: the review
// page's own numbers put first_name at ~9.5% and city at ~4.1% of the clicker
// tier, so the NO-DATA shape is what most recipients actually receive.
//
// Point WCL_V6_DIR at the review folder's source/ to run:
//   WCL_V6_DIR=~/Desktop/WCL-HELOC-v6-review/source go test ./internal/mailing/ -run V6 -v

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var v6Variants = []string{
	"iwchelocv6", "iwchelocv6a", "iwchelocv6b", "iwchelocv6c", "iwchelocv6d",
}

type v6Persona struct {
	label     string
	firstName string
	custom    map[string]interface{}
}

// The four shapes the WCL lane actually produces.
func v6Personas() []v6Persona {
	return []v6Persona{
		{
			// Post-fix mortgage-feed record: the only shape where the loan
			// clause can render at all.
			label:     "feed record (name+city+loan+property)",
			firstName: "Justin",
			custom: map[string]interface{}{
				"city": "Blue Springs", "state": "MO", "postal_code": "64015",
				"loan_type": "Conventional", "property_type": "Condominium",
			},
		},
		{
			label:     "name+city, no loan detail (pre-fix / broadcast)",
			firstName: "Linda",
			custom:    map[string]interface{}{"city": "Memphis", "state": "TN"},
		},
		{
			label:     "no name, state only",
			firstName: "",
			custom:    map[string]interface{}{"state": "FL"},
		},
		{
			// ~90% of recipients. This is the primary design, per the review page.
			label:     "NOTHING (the majority shape)",
			firstName: "",
			custom:    map[string]interface{}{},
		},
	}
}

func (p v6Persona) ctx() map[string]interface{} {
	return map[string]interface{}{
		"first_name": p.firstName,
		"last_name":  "",
		"custom":     p.custom,
		"brand": map[string]interface{}{
			"domain": "wcl-heloc.com",
			"name":   "West Capital Lending",
		},
		"system": map[string]interface{}{
			"dispatch_date":   "August 19, 2026",
			"unsubscribe_url": "https://t.wcl-heloc.com/u?c=CID&s=SID",
		},
		"subscriber": map[string]interface{}{"id": "SUBID-123"},
	}
}

func v6Dir(t *testing.T) string {
	d := strings.TrimSpace(os.Getenv("WCL_V6_DIR"))
	if d == "" {
		t.Skip("WCL_V6_DIR not set — point it at the v6 review folder's source/")
	}
	if strings.HasPrefix(d, "~/") {
		home, _ := os.UserHomeDir()
		d = filepath.Join(home, d[2:])
	}
	return d
}

// Every variant × every persona must render with nothing left unresolved and
// no empty-slot artifacts.
func TestWCLV6RendersCleanForEveryPersona(t *testing.T) {
	dir := v6Dir(t)
	ts := NewTemplateService()

	for _, name := range v6Variants {
		for _, ext := range []string{".html", ".txt"} {
			raw, err := os.ReadFile(filepath.Join(dir, name+ext))
			if err != nil {
				t.Fatalf("read %s%s: %v", name, ext, err)
			}
			for _, p := range v6Personas() {
				t.Run(name+ext+"/"+p.label, func(t *testing.T) {
					out, err := ts.Render("", string(raw), p.ctx())
					if err != nil {
						t.Fatalf("render error: %v", err)
					}
					// 1. No template syntax may survive into the wire body.
					for _, frag := range []string{"{{", "}}", "{%", "%}"} {
						if strings.Contains(out, frag) {
							t.Errorf("unresolved %q survives — literal token would ship", frag)
						}
					}
					// 2. Empty-slot artifacts from a missing personalization value.
					for _, bad := range []string{"Hi ,", "Hi  ", "in  are", " in ,", "your  home"} {
						if strings.Contains(out, bad) {
							t.Errorf("empty-slot artifact %q rendered", bad)
						}
					}
					// 3. Invariants that must survive on EVERY branch.
					if !strings.Contains(out, "e.wcl-heloc.com/GZHPZ/91Z47C/") {
						t.Errorf("money link missing")
					}
					if !strings.Contains(out, "sub1=SUBID-123") {
						t.Errorf("sub1 attribution not carried")
					}
					if !strings.Contains(out, "sub2=wcl-heloc.com") {
						t.Errorf("sub2 attribution not carried")
					}
					if !strings.Contains(out, "https://t.wcl-heloc.com/u?c=CID&s=SID") {
						t.Errorf("unsubscribe url missing")
					}
				})
			}
		}
	}
}

// The loan/property clause must appear ONLY for a record that carries the
// detail, and must vanish cleanly otherwise. This is the branch the
// personaFieldsFromExtra fix re-enables; without that fix it was dead for the
// whole feed.
//
// Coverage is per-variant BY DESIGN, established by reading the sources rather
// than assumed: v6 uses both loan_type and property_type, v6b uses loan_type,
// v6d uses property_type, and v6a (privacy) / v6c (consolidation) use neither —
// those two assign p_loan/p_prop and never reference them again (a harmless
// dead assign, not a broken branch). The test asserts what each variant
// actually claims, so a variant that LOSES its clause in a future edit fails
// here instead of shipping a silently generic body.
func TestWCLV6LoanClauseIsConditional(t *testing.T) {
	dir := v6Dir(t)
	ts := NewTemplateService()
	personas := v6Personas()
	withDetail, noDetail := personas[0], personas[3]

	// value the variant must show for a feed record, "" = variant uses neither
	want := map[string]string{
		"iwchelocv6":  "Conventional", // loan_type + property_type
		"iwchelocv6a": "",             // privacy angle — no lead detail
		"iwchelocv6b": "Conventional", // loan_type
		"iwchelocv6c": "",             // consolidation angle — no lead detail
		"iwchelocv6d": "Condominium",  // property_type
	}

	for _, name := range v6Variants {
		raw, err := os.ReadFile(filepath.Join(dir, name+".html"))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		gotWith, err := ts.Render("", string(raw), withDetail.ctx())
		if err != nil {
			t.Fatalf("%s render(with): %v", name, err)
		}
		gotWithout, err := ts.Render("", string(raw), noDetail.ctx())
		if err != nil {
			t.Fatalf("%s render(without): %v", name, err)
		}

		if tok := want[name]; tok != "" {
			if !strings.Contains(gotWith, tok) {
				t.Errorf("%s: %q did not render for a feed record — the clause is inert", name, tok)
			}
			if strings.Contains(gotWithout, tok) {
				t.Errorf("%s: lead detail %q leaked into a no-data render", name, tok)
			}
		} else {
			// Variant intentionally carries no lead detail; neither value may
			// appear on any branch.
			for _, tok := range []string{"Conventional", "Condominium"} {
				if strings.Contains(gotWith, tok) {
					t.Errorf("%s: unexpected lead detail %q — coverage map is stale", name, tok)
				}
			}
		}

		// Every variant still personalizes on name/city, so the personalized
		// render must differ from the generic one. Equal lengths would mean
		// the whole conditional layer is inert.
		if len(gotWith) <= len(gotWithout) {
			t.Errorf("%s: personalized render (%d) not longer than generic (%d) — "+
				"conditional layer may be inert", name, len(gotWith), len(gotWithout))
		}
	}
}

// ---------------------------------------------------------------- v7 set ---

// v7b ("One minute. No SSN.") was cut 2026-08-19 — operator: leading on
// "No SSN" reads too aggressive. The phrase survives only in the shared trust
// checklist near the footer, never in a subject, preheader or headline.
var v7Variants = []string{"iwchelocv7", "iwchelocv7a"}

func v7Dir(t *testing.T) string {
	d := strings.TrimSpace(os.Getenv("WCL_V7_DIR"))
	if d == "" {
		t.Skip("WCL_V7_DIR not set — point it at the v7 review folder's source/")
	}
	return d
}

// v7 adds two tokens v6 never used: custom.equity_estimate (derived, present
// for 75.9% of the mortgage feed) and custom.loan_purpose. Both must render
// for a record that has them and vanish cleanly for one that does not — the
// ~24% with no equity figure must still read a complete sentence.
func v7Personas() []v6Persona {
	full := map[string]interface{}{
		"city": "Cleveland", "state": "OH",
		"equity_estimate": "320,000", "loan_purpose": "HELOC",
		"loan_type": "Conventional", "property_type": "SFR",
	}
	return []v6Persona{
		{label: "full (name+city+equity+purpose)", firstName: "Jessica", custom: full},
		{label: "equity but no name", firstName: "", custom: map[string]interface{}{
			"city": "Boise", "equity_estimate": "80,000"}},
		{label: "name but NO equity (the 24%)", firstName: "Jessica",
			custom: map[string]interface{}{"city": "Cleveland"}},
		{label: "NOTHING", firstName: "", custom: map[string]interface{}{}},
	}
}

func TestWCLV7RendersCleanForEveryPersona(t *testing.T) {
	dir := v7Dir(t)
	ts := NewTemplateService()
	for _, name := range v7Variants {
		for _, ext := range []string{".html", ".txt"} {
			raw, err := os.ReadFile(filepath.Join(dir, name+ext))
			if err != nil {
				t.Fatalf("read %s%s: %v", name, ext, err)
			}
			for _, p := range v7Personas() {
				t.Run(name+ext+"/"+p.label, func(t *testing.T) {
					out, err := ts.Render("", string(raw), p.ctx())
					if err != nil {
						t.Fatalf("render error: %v", err)
					}
					for _, frag := range []string{"{{", "}}", "{%", "%}"} {
						if strings.Contains(out, frag) {
							t.Errorf("unresolved %q survives", frag)
						}
					}
					for _, bad := range []string{"Hi ,", "Hi  ", "estimated  in", "$ in", "your  home"} {
						if strings.Contains(out, bad) {
							t.Errorf("empty-slot artifact %q rendered", bad)
						}
					}
					if !strings.Contains(out, "e.wcl-heloc.com/GZHPZ/91Z47C/") {
						t.Errorf("money link missing")
					}
					if !strings.Contains(out, "sub1=SUBID-123") {
						t.Errorf("sub1 attribution not carried")
					}
					if !strings.Contains(out, "https://t.wcl-heloc.com/u?c=CID&s=SID") {
						t.Errorf("unsubscribe url missing")
					}
				})
			}
		}
	}
}

// The equity figure is the whole point of v7, and it is the one number we must
// never show when we cannot stand behind it.
func TestWCLV7EquityClauseIsConditional(t *testing.T) {
	dir := v7Dir(t)
	ts := NewTemplateService()
	ps := v7Personas()
	withEq, noEq := ps[0], ps[2]

	for _, name := range v7Variants {
		raw, err := os.ReadFile(filepath.Join(dir, name+".html"))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		got, err := ts.Render("", string(raw), withEq.ctx())
		if err != nil {
			t.Fatalf("%s render(with): %v", name, err)
		}
		none, err := ts.Render("", string(raw), noEq.ctx())
		if err != nil {
			t.Fatalf("%s render(without): %v", name, err)
		}
		if !strings.Contains(got, "$320,000") {
			t.Errorf("%s: equity figure did not render", name)
		}
		if strings.Contains(none, "320,000") || strings.Contains(none, "estimated $") {
			t.Errorf("%s: equity language leaked into a no-equity render", name)
		}
		// A dollar sign with nothing after it is the visible failure mode.
		if strings.Contains(none, "$<") || strings.Contains(none, "$ ") {
			t.Errorf("%s: bare currency symbol rendered with no figure", name)
		}
	}
}

// v7 must actually be a SHORTER read than v6 — that is the operator's ask, so
// it is asserted rather than eyeballed. Compared on the text part, which is
// the message without chassis markup.
func TestWCLV7IsShorterThanV6(t *testing.T) {
	v6d, v7d := v6Dir(t), v7Dir(t)
	longest := 0
	for _, n := range v7Variants {
		b, err := os.ReadFile(filepath.Join(v7d, n+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > longest {
			longest = len(b)
		}
	}
	shortestV6 := 1 << 30
	for _, n := range v6Variants {
		b, err := os.ReadFile(filepath.Join(v6d, n+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		if len(b) < shortestV6 {
			shortestV6 = len(b)
		}
	}
	if longest >= shortestV6 {
		t.Errorf("longest v7 text (%d b) is not shorter than the shortest v6 (%d b)",
			longest, shortestV6)
	}
	t.Logf("longest v7 text = %d b, shortest v6 text = %d b", longest, shortestV6)
}

// Operator ruling 2026-08-19: leading on "No SSN" is too aggressive. It may
// appear in the trust checklist near the footer — standard reassurance, and
// part of the already-reviewed chassis — but never in a headline, preheader or
// CTA line, where it functions as a hook. Preheaders are inbox-visible, so
// they count as leading.
func TestWCLV7DoesNotLeadWithNoSSN(t *testing.T) {
	dir := v7Dir(t)
	for _, name := range v7Variants {
		raw, err := os.ReadFile(filepath.Join(dir, name+".html"))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(raw)
		// Everything before the trust checklist is "leading" surface.
		head := body
		if i := strings.Index(body, "Your information is never sold"); i > 0 {
			head = body[:i]
		}
		for _, phrase := range []string{"No SSN", "No Social Security"} {
			if strings.Contains(head, phrase) {
				t.Errorf("%s: %q appears above the trust checklist — that is a lead", name, phrase)
			}
		}
	}
}
