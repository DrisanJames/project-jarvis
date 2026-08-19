package mailing

// Proof harness for the WCL-MTG-SIDECAR personalized creative.
//
// Purpose: prove — not assume — that the merge tokens embedded in the WCL
// sidecar creative render correctly through the SAME TemplateService that the
// live send path uses (internal/worker/send_worker.go:1841-1852), including the
// empty-first_name fallback path that covers the subscribers on list
// 0bc4187c-9838-4890-ab5a-b7d61d68f6ff who have no first_name.
//
// The render context here mirrors buildRenderContext
// (internal/worker/send_worker.go:3228-3246) exactly for the personalized keys:
//	rc["first_name"] = item.FirstName            (COALESCE(s.first_name,'') -> "" when NULL)
//	rc["custom"]     = map[string]interface{}(item.CustomFields)
//
// Set WCL_SNAPSHOT=/path/to/snapshot.html to additionally render the full
// production snapshot document and assert zero residual tokens.

import (
	"os"
	"strings"
	"testing"
)

// Verbatim token fragments copied from mailing_content_snapshots.html_content
// (snapshot 860c17f3-db89-472b-a9a6-fe68769f590f, the bytes actually sent on
// 2026-08-12). Do not reformat: the single quotes and spacing are the shipped form.
const (
	wclGreetingTpl = "Hi {{ first_name | default: 'there' }} — a HELOC isn't a loan for one purpose."
	wclCityTpl     = "homeowners in {{ custom.city | default: 'your area' }} are using it in some smart ways right now:"
)

// realSubscriber mirrors a row from mailing_subscribers on the target list.
type realSubscriber struct {
	label     string
	firstName string // as delivered by COALESCE(s.first_name,'')
	custom    map[string]interface{}
	wantName  string // expected rendered name token
	wantCity  string // expected rendered city token
}

func wclSubscribers() []realSubscriber {
	return []realSubscriber{
		{
			// mailing_subscribers.id = 223e0a53-3ced-495c-9dfd-91ff4477fd27 (gmail)
			label:     "first_name present + city present",
			firstName: "Linda",
			custom: map[string]interface{}{
				"city": "Santa Rosa Beach", "state": "FL",
				"address": "215 Sally Lane", "postal_code": "32459",
			},
			wantName: "Linda",
			wantCity: "Santa Rosa Beach",
		},
		{
			// mailing_subscribers.id = 21fcd2a7-aa89-46a6-bb9b-f4fc8dbed48a (gmail)
			// first_name is '' in production -> must fall back to 'there'.
			label:     "first_name EMPTY (fallback) + city present",
			firstName: "",
			custom: map[string]interface{}{
				"city": "Memphis", "state": "TN",
				"address": "981 Alaska Street", "postal_code": "38107",
			},
			wantName: "there",
			wantCity: "Memphis",
		},
		{
			// Defensive: no subscriber on the list currently has both blank, but
			// the creative must still degrade cleanly if one appears.
			label:     "both EMPTY (worst case, both fallbacks)",
			firstName: "",
			custom:    map[string]interface{}{},
			wantName:  "there",
			wantCity:  "your area",
		},
	}
}

// renderCtx builds the send-path render context for the personalized keys.
func (s realSubscriber) renderCtx() map[string]interface{} {
	return map[string]interface{}{
		"first_name": s.firstName,
		"custom":     s.custom,
	}
}

func TestWCLPersonalizationRendersThroughSendPathEngine(t *testing.T) {
	ts := NewTemplateService()

	for _, sub := range wclSubscribers() {
		t.Run(sub.label, func(t *testing.T) {
			gotGreeting, err := ts.Render("", wclGreetingTpl, sub.renderCtx())
			if err != nil {
				t.Fatalf("greeting render error: %v", err)
			}
			gotCity, err := ts.Render("", wclCityTpl, sub.renderCtx())
			if err != nil {
				t.Fatalf("city render error: %v", err)
			}

			t.Logf("RENDERED greeting: %s", gotGreeting)
			t.Logf("RENDERED city    : %s", gotCity)

			// 1. No residual template syntax may survive.
			for _, frag := range []string{"{{", "}}", "|", "default:"} {
				if strings.Contains(gotGreeting, frag) {
					t.Errorf("greeting still contains %q — literal token would ship: %s", frag, gotGreeting)
				}
				if strings.Contains(gotCity, frag) {
					t.Errorf("city still contains %q — literal token would ship: %s", frag, gotCity)
				}
			}

			// 2. The intended value must be present.
			wantGreeting := "Hi " + sub.wantName + " —"
			if !strings.Contains(gotGreeting, wantGreeting) {
				t.Errorf("greeting = %q, want it to contain %q", gotGreeting, wantGreeting)
			}
			if !strings.Contains(gotCity, "in "+sub.wantCity+" are using") {
				t.Errorf("city = %q, want city %q", gotCity, sub.wantCity)
			}

			// 3. The specific visible defect we are guarding against: "Hi ,"
			// or "Hi  —" (bare/empty name).
			if strings.Contains(gotGreeting, "Hi  ") || strings.Contains(gotGreeting, "Hi ,") {
				t.Errorf("empty-name defect rendered: %q", gotGreeting)
			}
		})
	}
}

// TestWCLWrongTokenFormsAreCaught is the negative control: it proves the
// assertions above actually have teeth by feeding the harness the token forms
// that a well-meaning author would plausibly reach for and that DO NOT resolve
// on the send path. Each must fail to produce the subscriber's name.
//
//	{{ subscriber.first_name }} — buildRenderContext's "subscriber" map has no
//	  first_name key (send_worker.go:3332-3338); renders EMPTY. It renders
//	  "Proof" in a proof send (offer_proof_send.go:480), so a proof send LIES.
//	{{ city }}                  — custom fields are only under "custom."
//	                              (send_worker.go:3243); renders EMPTY.
//	{{ custom_fields.city }}    — no such key anywhere; renders EMPTY.
//	{{FIRST_NAME}}              — site-token leakage, SeverityError in
//	                              message_validator.go:50; ships literally here.
func TestWCLWrongTokenFormsAreCaught(t *testing.T) {
	ts := NewTemplateService()
	sub := wclSubscribers()[0] // Linda / Santa Rosa Beach

	cases := []struct {
		name     string
		tpl      string
		wantMiss string // the value that must NOT appear
	}{
		{"subscriber.first_name renders empty", "Hi {{ subscriber.first_name }} —", "Linda"},
		{"bare city renders empty", "in {{ city }} are", "Santa Rosa Beach"},
		{"custom_fields.city renders empty", "in {{ custom_fields.city }} are", "Santa Rosa Beach"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ts.Render("", c.tpl, sub.renderCtx())
			if err != nil {
				t.Fatalf("render error: %v", err)
			}
			t.Logf("WRONG-FORM %q -> %q", c.tpl, got)
			if strings.Contains(got, c.wantMiss) {
				t.Fatalf("negative control FAILED: %q unexpectedly resolved to contain %q — "+
					"the send-path context must not expose this form", c.tpl, c.wantMiss)
			}
		})
	}

	// Uppercase site token: osteele/liquid parses {{FIRST_NAME}} as a variable
	// named FIRST_NAME, which is absent from the send context, so it renders to
	// the EMPTY STRING rather than surviving literally. That makes it a SILENT
	// defect ("Hi  —"), not a visible one — worse, not better. Verified here.
	got, err := ts.Render("", "Hi {{FIRST_NAME}} —", sub.renderCtx())
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	t.Logf("WRONG-FORM %q -> %q (silent empty, NOT literal)", "Hi {{FIRST_NAME}} —", got)
	if strings.Contains(got, "Linda") {
		t.Errorf("{{FIRST_NAME}} must not resolve to the name, got %q", got)
	}
	if !strings.Contains(got, "Hi  —") {
		t.Errorf("expected {{FIRST_NAME}} to render empty (\"Hi  —\"), got %q", got)
	}
}

// TestWCLFullSnapshotRenders renders the entire production snapshot document
// and asserts no token survives. Skipped unless WCL_SNAPSHOT is set.
func TestWCLFullSnapshotRenders(t *testing.T) {
	path := os.Getenv("WCL_SNAPSHOT")
	if path == "" {
		t.Skip("WCL_SNAPSHOT not set; skipping full-document render")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	ts := NewTemplateService()

	for _, sub := range wclSubscribers() {
		t.Run(sub.label, func(t *testing.T) {
			ctx := sub.renderCtx()
			// The send path also supplies these; without them Liquid emits ""
			// which would leave the unsubscribe href empty, so assert they fill.
			ctx["system"] = map[string]interface{}{
				"unsubscribe_url": "https://t.em.wcl-heloc.com/u/PROOF",
			}
			ctx["brand"] = map[string]interface{}{"domain": "wcl-heloc.com"}
			ctx["subscriber"] = map[string]interface{}{"id": "PROOF-SUB-ID"}

			out, err := ts.Render("", string(raw), ctx)
			if err != nil {
				t.Fatalf("full render error: %v", err)
			}
			if strings.Contains(out, "{{") || strings.Contains(out, "{%") {
				idx := strings.Index(out, "{{")
				if idx < 0 {
					idx = strings.Index(out, "{%")
				}
				lo := idx - 80
				if lo < 0 {
					lo = 0
				}
				hi := idx + 120
				if hi > len(out) {
					hi = len(out)
				}
				t.Errorf("residual token in rendered document near: %q", out[lo:hi])
			}
			if !strings.Contains(out, "Hi "+sub.wantName+" —") {
				t.Errorf("rendered document missing personalized greeting for %q", sub.wantName)
			}
			if !strings.Contains(out, sub.wantCity) {
				t.Errorf("rendered document missing city %q", sub.wantCity)
			}
			t.Logf("full document rendered clean: %d bytes, name=%q city=%q",
				len(out), sub.wantName, sub.wantCity)
		})
	}
}
