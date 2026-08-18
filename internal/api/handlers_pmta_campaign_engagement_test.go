package api

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
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

// The bug this guards: mailing_segments.subscriber_count is a CACHED tally that
// SegmentRefreshWorker zeroes when its count query times out. Prod 2026-08-18
// had "DB 30D Clickers" at counter=0 with 1,534 materialized members, and a dry
// run over that segment planned 4,477 recipients — the picker must never show
// the operator a 0 for an audience that will actually mail.
func TestApplyLiveMemberCountsOverridesStaleCounter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	resp := engagementTiersResponse{
		Clickers: []engagementTier{
			{SegmentID: "11111111-1111-1111-1111-111111111111", Name: "DB 30D Clickers", Count: 0, CounterCount: 0},
		},
		Openers: []engagementTier{
			{SegmentID: "22222222-2222-2222-2222-222222222222", Name: "DB 7D Openers", Count: 19673, CounterCount: 19673},
			{SegmentID: "33333333-3333-3333-3333-333333333333", Name: "DB 30D Openers (genuinely empty)", Count: 5, CounterCount: 5},
		},
	}

	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_segment_members")).
		WillReturnRows(sqlmock.NewRows([]string{"segment_id", "count"}).
			AddRow("11111111-1111-1111-1111-111111111111", 1534).
			AddRow("22222222-2222-2222-2222-222222222222", 19673))
	// segment 3 is absent from the GROUP BY result = genuinely zero members

	applyLiveMemberCounts(context.Background(), db, &resp)

	clk := resp.Clickers[0]
	if clk.Count != 1534 {
		t.Fatalf("stale counter not overridden: count=%d, want 1534", clk.Count)
	}
	if !clk.CounterMismatch {
		t.Error("counter_mismatch must flag the broken tally (0 vs 1534)")
	}
	if !clk.CountIsLive {
		t.Error("count_is_live must be true after a successful read")
	}
	if clk.CounterCount != 0 {
		t.Errorf("the cached counter must be preserved for display: %d", clk.CounterCount)
	}

	if op := resp.Openers[0]; op.Count != 19673 || op.CounterMismatch {
		t.Errorf("an agreeing counter must not be flagged: count=%d mismatch=%v", op.Count, op.CounterMismatch)
	}
	if empty := resp.Openers[1]; empty.Count != 0 || !empty.CounterMismatch {
		t.Errorf("a segment with no members must read 0 live and flag the disagreeing counter: count=%d mismatch=%v",
			empty.Count, empty.CounterMismatch)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// Fail-soft: a DB error must leave the cached counters standing, not blank the panel.
func TestApplyLiveMemberCountsFallsBackOnError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	resp := engagementTiersResponse{
		Clickers: []engagementTier{{SegmentID: "11111111-1111-1111-1111-111111111111", Count: 1234, CounterCount: 1234}},
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_segment_members")).
		WillReturnError(context.DeadlineExceeded)

	applyLiveMemberCounts(context.Background(), db, &resp)

	if got := resp.Clickers[0]; got.Count != 1234 || got.CountIsLive {
		t.Fatalf("must fall back to the cached counter and mark it not-live: count=%d live=%v",
			got.Count, got.CountIsLive)
	}
}
