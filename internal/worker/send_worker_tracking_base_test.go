package worker

// Pins {{ system.tracking_base }} (operator 2026-09-06: the brand resolves
// from the SENDING domain, never hardcoded). Contract:
//   - value == the profile's tracking base (scheme+host, no trailing slash),
//     the same base unsubscribe_url / preferences_url are built on — so it
//     differs between a PMTA profile (t.em.<apex>) and an SES one (t.m.<apex>);
//   - an href written as {{ system.tracking_base }}/o/... renders with the
//     profile's host AND is recognised by RewriteClickLinks as our own
//     tracking URL (no /track/click double hop);
//   - NEGATIVE CONTROL: the historical hardcoded https://t.m.discountblog.com/o/
//     href mailed from a t.em.quizfiesta.com profile IS wrapped (the double
//     hop this key exists to remove);
//   - no tracking base → key absent, the token renders empty, never literal.

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

const (
	tbOrg    = "11111111-1111-1111-1111-111111111111"
	tbSecret = "tracking-base-test-secret"
)

func trackingBaseItem() QueueItem {
	return QueueItem{
		ID:           uuid.MustParse("44444444-4444-4444-4444-444444444444"),
		CampaignID:   uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		SubscriberID: uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		Email:        "someone@yahoo.com",
		BrandRoot:    "quizfiesta.com",
	}
}

func systemMap(t *testing.T, rc mailing.RenderContext) map[string]interface{} {
	t.Helper()
	sys, ok := rc["system"].(map[string]interface{})
	if !ok {
		t.Fatalf("rc[system] is %T, want map[string]interface{}", rc["system"])
	}
	return sys
}

// PMTA and SES profiles resolve different tracking hosts; the key must follow
// whichever profile is sending, exactly like unsubscribe_url does.
func TestBuildRenderContext_TrackingBase_FollowsSendingProfile(t *testing.T) {
	pool := &SendWorkerPool{orgID: tbOrg, trackingSecret: tbSecret, trackingURL: "https://track.ignite.media"}
	for _, tc := range []struct{ name, base string }{
		{"pmta profile t.em.<apex>", "https://t.em.quizfiesta.com"},
		{"ses profile t.m.<apex>", "https://t.m.quizfiesta.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sys := systemMap(t, pool.buildRenderContext(trackingBaseItem(), tc.base))
			got, _ := sys["tracking_base"].(string)
			if got != tc.base {
				t.Fatalf("system.tracking_base = %q, want %q", got, tc.base)
			}
			if strings.HasSuffix(got, "/") {
				t.Errorf("tracking_base must not carry a trailing slash: %q", got)
			}
			unsub, _ := sys["unsubscribe_url"].(string)
			if !strings.HasPrefix(unsub, got+"/") {
				t.Errorf("tracking_base %q is not the base unsubscribe_url %q is built on", got, unsub)
			}
		})
	}
}

// Empty per-profile base falls back to the pool's global tracking URL — the
// same fallback unsubscribe_url takes (tBase := trackBase || p.trackingURL).
func TestBuildRenderContext_TrackingBase_FallsBackToPoolTrackingURL(t *testing.T) {
	pool := &SendWorkerPool{orgID: tbOrg, trackingSecret: tbSecret, trackingURL: "https://track.ignite.media"}
	sys := systemMap(t, pool.buildRenderContext(trackingBaseItem(), ""))
	if got, _ := sys["tracking_base"].(string); got != "https://track.ignite.media" {
		t.Fatalf("system.tracking_base = %q, want the pool trackingURL", got)
	}
}

// No base anywhere → key absent and the Liquid token renders EMPTY, never the
// literal "{{ system.tracking_base }}".
func TestBuildRenderContext_TrackingBase_AbsentWhenNoBase(t *testing.T) {
	pool := &SendWorkerPool{orgID: tbOrg, trackingSecret: tbSecret} // trackingURL ""
	rc := pool.buildRenderContext(trackingBaseItem(), "")
	sys := systemMap(t, rc)
	if v, ok := sys["tracking_base"]; ok && v != "" {
		t.Fatalf("system.tracking_base present with no base configured: %v", v)
	}
	out, err := mailing.NewTemplateService().Render("tb:absent", `x{{ system.tracking_base }}y`, rc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if out != "xy" {
		t.Fatalf("absent tracking_base rendered %q, want %q (empty, never literal)", out, "xy")
	}
}

// The whole point: an href built from {{ system.tracking_base }}/o/... renders
// with the SENDING profile's host, and RewriteClickLinks leaves it alone
// (it starts with baseURL+"/o/", so it is already a tracking URL).
func TestTrackingBaseHref_RendersProfileHostAndIsNotDoubleWrapped(t *testing.T) {
	const base = "https://t.em.quizfiesta.com"
	pool := &SendWorkerPool{orgID: tbOrg, trackingSecret: tbSecret}
	item := trackingBaseItem()
	rc := pool.buildRenderContext(item, base)

	tpl := `<a href="{{ system.tracking_base }}/o/{{ brand.domain }}/{{ subscriber.id }}/s45cbugnln/{{ campaign.id }}">go</a>`
	html, err := mailing.NewTemplateService().Render("tb:dynamic", tpl, rc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := base + "/o/quizfiesta.com/" + item.SubscriberID.String() + "/s45cbugnln/" + item.CampaignID.String()
	if !strings.Contains(html, `href="`+want+`"`) {
		t.Fatalf("rendered href missing %q in:\n%s", want, html)
	}

	out := RewriteClickLinks(html, item.CampaignID.String(), item.SubscriberID.String(), item.ID.String(), base, tbOrg, tbSecret)
	if out != html {
		t.Fatalf("dynamic /o/ link was rewritten (double hop):\n got %s\nwant %s", out, html)
	}
	if strings.Contains(out, "/track/click/") {
		t.Errorf("dynamic /o/ link must not be wrapped in /track/click: %s", out)
	}
}

// NEGATIVE CONTROL — the old hardcoded discountblog host, mailed from a
// quizfiesta profile, does NOT match baseURL+"/o/" and IS wrapped. This is the
// exact double hop the dynamic key removes; if this stops wrapping, the skip
// rule got looser than "our own host".
func TestTrackingBaseHref_NegativeControl_HardcodedHostIsWrapped(t *testing.T) {
	const base = "https://t.em.quizfiesta.com"
	item := trackingBaseItem()
	html := `<a href="https://t.m.discountblog.com/o/quizfiesta.com/` + item.SubscriberID.String() +
		`/s45cbugnln/` + item.CampaignID.String() + `">go</a>`

	out := RewriteClickLinks(html, item.CampaignID.String(), item.SubscriberID.String(), item.ID.String(), base, tbOrg, tbSecret)
	if out == html {
		t.Fatalf("hardcoded t.m.discountblog.com /o/ link mailed from %s must be wrapped (negative control)", base)
	}
	if !strings.Contains(out, base+"/track/click/") {
		t.Errorf("expected a %s/track/click/ wrap, got: %s", base, out)
	}
}

// Click-drip journey overlay: SystemURLs must carry the same key so the
// profile base wins over the GLOBAL base BuildContext stamps on that path.
func TestClickDripSystemURLs_CarryTrackingBase(t *testing.T) {
	// No DB: profileID "" short-circuits resolveTrackingURL and a non-UUID
	// subscriber id short-circuits resolveOrgID.
	s := &JourneyClickDripSender{trackingURL: "https://t.em.quizfiesta.com", trackingSecret: tbSecret}
	urls := s.SystemURLs(t.Context(), "162", "node-1", "", "sub-1", "", "news@em.quizfiesta.com")
	if urls == nil {
		t.Fatal("SystemURLs returned nil with a configured trackingURL")
	}
	if got, _ := urls["tracking_base"].(string); got != "https://t.em.quizfiesta.com" {
		t.Fatalf("tracking_base = %q, want the profile base", got)
	}
}
