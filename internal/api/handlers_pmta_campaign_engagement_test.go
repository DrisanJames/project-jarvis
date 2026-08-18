package api

import (
	"context"
	"regexp"
	"testing"
	"time"

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
// SegmentRefreshWorker zeroes when its count query times out — wrong for 46 of
// the 108 active engagement segments when measured 2026-08-18, including
// "DB 30D Openers" reading 0 against a real 47,516. The picker must never show
// an operator a 0 for an audience that will actually mail.
func TestApplyBuildLedgerCountsOverridesStaleCounter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	built := time.Date(2026, 8, 18, 6, 34, 58, 0, time.UTC)
	resp := engagementTiersResponse{
		Clickers: []engagementTier{
			{SegmentID: "11111111-1111-1111-1111-111111111111", Name: "DB 14D Clickers", Count: 0, CounterCount: 0},
			{SegmentID: "44444444-4444-4444-4444-444444444444", Name: "DB 30D Clickers", Count: 0, CounterCount: 0},
		},
		Openers: []engagementTier{
			{SegmentID: "22222222-2222-2222-2222-222222222222", Name: "DB 7D Openers", Count: 21335, CounterCount: 21335},
			{SegmentID: "33333333-3333-3333-3333-333333333333", Name: "No ledger row", Count: 7, CounterCount: 7},
		},
	}

	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_segment_build_ledger")).
		WillReturnRows(sqlmock.NewRows([]string{"segment_id", "subscriber_count", "last_build_status", "last_built_at"}).
			AddRow("11111111-1111-1111-1111-111111111111", 1577, "ok", built).
			AddRow("44444444-4444-4444-4444-444444444444", 1534, "running", built).
			AddRow("22222222-2222-2222-2222-222222222222", 21335, "ok", built))

	applyBuildLedgerCounts(context.Background(), db, &resp)

	if got := resp.Clickers[0]; got.Count != 1577 || !got.CounterMismatch || !got.CountIsLive {
		t.Fatalf("stale counter not overridden: count=%d mismatch=%v live=%v",
			got.Count, got.CounterMismatch, got.CountIsLive)
	}
	if got := resp.Clickers[0]; got.CounterCount != 0 {
		t.Errorf("the cached counter must be preserved for display: %d", got.CounterCount)
	}
	// A wedged build must be visible: its count describes the PREVIOUS build.
	if got := resp.Clickers[1]; got.BuildStatus != "running" {
		t.Errorf("build status must surface, got %q", got.BuildStatus)
	}
	if got := resp.Openers[0]; got.Count != 21335 || got.CounterMismatch {
		t.Errorf("an agreeing counter must not be flagged: count=%d mismatch=%v", got.Count, got.CounterMismatch)
	}
	// No ledger row = keep the cached value and admit it is not authoritative.
	if got := resp.Openers[1]; got.Count != 7 || got.CountIsLive {
		t.Errorf("a segment with no ledger row must keep its cached count and stay not-live: count=%d live=%v",
			got.Count, got.CountIsLive)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// Fail-soft: a DB error must leave the cached counters standing, not blank the panel.
func TestApplyBuildLedgerCountsFallsBackOnError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	resp := engagementTiersResponse{
		Clickers: []engagementTier{{SegmentID: "11111111-1111-1111-1111-111111111111", Count: 1234, CounterCount: 1234}},
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_segment_build_ledger")).
		WillReturnError(context.DeadlineExceeded)

	applyBuildLedgerCounts(context.Background(), db, &resp)

	if got := resp.Clickers[0]; got.Count != 1234 || got.CountIsLive {
		t.Fatalf("must fall back to the cached counter and mark it not-live: count=%d live=%v",
			got.Count, got.CountIsLive)
	}
}

// cancelRateOf must measure TERMINAL outcomes only. Counting in-flight
// campaigns would make a domain that is mid-send look held back, which is the
// opposite of the signal this exists to give.
func TestCancelRateOf(t *testing.T) {
	cases := []struct {
		name   string
		counts map[string]int
		want   float64
	}{
		{
			// The kumo estate shape measured 2026-08-18: staged then cancelled.
			name:   "estate held back",
			counts: map[string]int{"cancelled": 9, "sent": 1},
			want:   0.9,
		},
		{
			name:   "healthy domain",
			counts: map[string]int{"sent": 40, "cancelled": 0},
			want:   0,
		},
		{
			// In-flight work must not dilute or inflate the rate.
			name:   "in-flight excluded",
			counts: map[string]int{"cancelled": 1, "sent": 1, "scheduled": 50, "sending": 20, "draft": 8},
			want:   0.5,
		},
		{
			name:   "nothing terminal yet",
			counts: map[string]int{"scheduled": 4, "draft": 2},
			want:   0,
		},
		{name: "empty", counts: map[string]int{}, want: 0},
		{
			name:   "completed variants count as terminal",
			counts: map[string]int{"cancelled": 1, "completed": 2, "completed_with_errors": 1},
			want:   0.25,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cancelRateOf(tc.counts); got != tc.want {
				t.Fatalf("cancelRateOf(%v) = %v, want %v", tc.counts, got, tc.want)
			}
		})
	}
}
