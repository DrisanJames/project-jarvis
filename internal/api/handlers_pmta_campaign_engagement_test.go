package api

import (
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// Every condition shape below was read out of prod mailing_segments on
// 2026-08-17 — these are not invented fixtures.
func TestParseEngagementConditions(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantDomain string
		wantKind   string
		wantWindow int
	}{
		{
			name: "DB 7D Openers (the 98-row majority shape)",
			raw: `[{"field":"sending_domain","group":0,"value":"em.discountblog.com","operator":"equals"},
			       {"field":"email_opened","group":0,"value":"7","operator":"in_last_days"},
			       {"field":"exclude_list_pattern","group":0,"value":"seed","operator":"contains"}]`,
			wantDomain: "em.discountblog.com", wantKind: "openers", wantWindow: 7,
		},
		{
			name: "DB 30D Clickers",
			raw: `[{"field":"sending_domain","group":0,"value":"em.discountblog.com","operator":"equals"},
			       {"field":"email_clicked","group":0,"value":"30","operator":"in_last_days"},
			       {"field":"exclude_list_pattern","group":0,"value":"seed","operator":"contains"}]`,
			wantDomain: "em.discountblog.com", wantKind: "clickers", wantWindow: 30,
		},
		{
			name:       "KUMO-ALLTIME-BCC-ENG — bare apex, no window",
			raw:        `[{"field":"sending_domain","group":0,"value":"bestcreditcare.com","operator":"equals"}]`,
			wantDomain: "bestcreditcare.com", wantKind: "", wantWindow: 0,
		},
		{
			name:       "EXCL Charter Family Domains — no sending_domain at all",
			raw:        `[{"field":"email","group":0,"value":"charter.net","operator":"contains"}]`,
			wantDomain: "", wantKind: "", wantWindow: 0,
		},
		{
			name:       "case-insensitive field/operator",
			raw:        `[{"field":"Sending_Domain","value":"EM.QuizFiesta.com","operator":"Equals"},{"field":"Email_Clicked","value":"60","operator":"In_Last_Days"}]`,
			wantDomain: "em.quizfiesta.com", wantKind: "clickers", wantWindow: 60,
		},
		{
			name:       "non-recency operator is not a range",
			raw:        `[{"field":"sending_domain","value":"em.discountblog.com","operator":"equals"},{"field":"email_clicked","value":"true","operator":"equals"}]`,
			wantDomain: "em.discountblog.com", wantKind: "", wantWindow: 0,
		},
		{
			name:       "unparseable window is ignored, not defaulted",
			raw:        `[{"field":"sending_domain","value":"em.discountblog.com","operator":"equals"},{"field":"email_opened","value":"","operator":"in_last_days"}]`,
			wantDomain: "em.discountblog.com", wantKind: "", wantWindow: 0,
		},
		{name: "empty conditions", raw: `[]`},
		{name: "malformed json", raw: `{"not":"an array"}`},
		{name: "nil"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dom, kind, win := parseEngagementConditions([]byte(tc.raw))
			if dom != tc.wantDomain || kind != tc.wantKind || win != tc.wantWindow {
				t.Fatalf("got (%q,%q,%d), want (%q,%q,%d)",
					dom, kind, win, tc.wantDomain, tc.wantKind, tc.wantWindow)
			}
		})
	}
}

// The gate must actually FIRE — a documented guard that no-ops is worse than
// none. The first case is the exact payload blog_campaign_handler used to send.
func TestCoerceMasterSelectionForSegmentAudience(t *testing.T) {
	ptr := func(b bool) *bool { return &b }

	t.Run("fires on the uncapped segment-sourced shape", func(t *testing.T) {
		in := engine.PMTACampaignInput{
			Name:              "08182026 - Discount Blog - Engaged Audience",
			TargetISPs:        []engine.ISP{"gmail", "yahoo"},
			InclusionSegments: []string{"fee53e1a-c017-4545-92be-e31780103fa6"},
			// ISPQuotas / ISPPlans empty => every quota 0 => unlimited
		}
		if !coerceMasterSelectionForSegmentAudience(&in) {
			t.Fatal("expected coercion to fire")
		}
		if in.UseMasterSelection == nil || *in.UseMasterSelection {
			t.Fatalf("use_master_selection = %v, want explicit false", in.UseMasterSelection)
		}
	})

	t.Run("fires when the caller explicitly asked for true but is uncapped", func(t *testing.T) {
		in := engine.PMTACampaignInput{
			InclusionLists:     []string{"list-1"},
			UseMasterSelection: ptr(true),
			ISPPlans:           []engine.PMTAISPScheduleInput{{ISP: "gmail", Quota: 0}},
		}
		if !coerceMasterSelectionForSegmentAudience(&in) {
			t.Fatal("expected coercion to fire")
		}
		if *in.UseMasterSelection {
			t.Fatal("want false after coercion")
		}
	})

	t.Run("does NOT fire when a finite cap exists (top-up has a ceiling)", func(t *testing.T) {
		in := engine.PMTACampaignInput{
			InclusionSegments: []string{"seg"},
			ISPQuotas:         []engine.ISPQuota{{ISP: "gmail", Volume: 5000}},
		}
		if coerceMasterSelectionForSegmentAudience(&in) {
			t.Fatal("must not coerce a capped payload")
		}
		if in.UseMasterSelection != nil {
			t.Fatal("must leave the field untouched so the DB default applies")
		}
	})

	t.Run("does NOT fire on a per-plan quota", func(t *testing.T) {
		in := engine.PMTACampaignInput{
			InclusionSegments: []string{"seg"},
			ISPPlans:          []engine.PMTAISPScheduleInput{{ISP: "gmail", Quota: 1000}},
		}
		if coerceMasterSelectionForSegmentAudience(&in) {
			t.Fatal("must not coerce a per-plan-capped payload")
		}
	})

	t.Run("does NOT fire without a segment audience (welcome/acquisition sends)", func(t *testing.T) {
		in := engine.PMTACampaignInput{TargetISPs: []engine.ISP{"gmail"}}
		if coerceMasterSelectionForSegmentAudience(&in) {
			t.Fatal("must not coerce a pure master-selection payload")
		}
		if in.UseMasterSelection != nil {
			t.Fatal("must leave the field untouched")
		}
	})

	t.Run("leaves an explicit false alone", func(t *testing.T) {
		in := engine.PMTACampaignInput{
			InclusionSegments:  []string{"seg"},
			UseMasterSelection: ptr(false),
		}
		if coerceMasterSelectionForSegmentAudience(&in) {
			t.Fatal("nothing to coerce")
		}
		if *in.UseMasterSelection {
			t.Fatal("want false preserved")
		}
	})

	t.Run("nil input is safe", func(t *testing.T) {
		if coerceMasterSelectionForSegmentAudience(nil) {
			t.Fatal("nil must be a no-op")
		}
	})
}

// Regression guard for the live instance of the bug: the blog-campaign builder
// must never emit the unbounded shape again.
func TestBuildBlogCampaignInputIsSegmentBound(t *testing.T) {
	in, err := buildBlogCampaignInput(BlogCampaignInput{
		SendingDomain: "em.discountblog.com",
		Subject:       "test",
		HTMLContent:   "<p>hi</p>",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if in.UseMasterSelection == nil || *in.UseMasterSelection {
		t.Fatalf("blog campaigns are segment-bound: use_master_selection = %v, want explicit false",
			in.UseMasterSelection)
	}
	if !deployInputIsUncapped(in) {
		t.Fatal("precondition changed: blog campaigns are expected to be uncapped")
	}
}
