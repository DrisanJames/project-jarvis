package api

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

func TestIsCanonicalISP(t *testing.T) {
	canonical := []string{"gmail", "yahoo", "aol", "microsoft", "apple", "comcast", "att", "sbcglobal", "cox", "charter"}
	for _, isp := range canonical {
		if !isCanonicalISP(isp) {
			t.Errorf("isCanonicalISP(%q) = false, want true", isp)
		}
	}
	nonCanonical := []string{"verizon", "protonmail", "zoho", "other", "unknown", ""}
	for _, isp := range nonCanonical {
		if isCanonicalISP(isp) {
			t.Errorf("isCanonicalISP(%q) = true, want false", isp)
		}
	}
}

func TestNormalize_OtherISPIncludedInTargetISPs(t *testing.T) {
	now := time.Now().UTC()
	scheduled := now.Add(1 * time.Hour)
	endTime := scheduled.Add(8 * time.Hour)
	input := engine.PMTACampaignInput{
		SendMode:    "scheduled",
		ScheduledAt: &scheduled,
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 1000, TimeSpans: []engine.PMTATimeSpanInput{{
				Type: "absolute", StartAt: &scheduled, EndAt: &endTime,
			}}},
			{ISP: "other", Quota: 500, TimeSpans: []engine.PMTATimeSpanInput{{
				Type: "absolute", StartAt: &scheduled, EndAt: &endTime,
			}}},
		},
	}
	normalized, err := normalizePMTACampaignInput(input)
	if err != nil {
		t.Fatalf("normalizePMTACampaignInput: %v", err)
	}
	found := false
	for _, isp := range normalized.TargetISPs {
		if string(isp) == "other" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("TargetISPs does not include 'other': %v", normalized.TargetISPs)
	}
}

func TestMinSpanForVolume(t *testing.T) {
	tests := []struct {
		name       string
		recipients int
		want       time.Duration
	}{
		{"0 (unlimited) uses full 8h", 0, 8 * time.Hour},
		{"negative uses full 8h", -1, 8 * time.Hour},
		{"100 recipients = 1h (floor)", 100, 1 * time.Hour},
		{"50 recipients = 1h (floor clamp)", 50, 1 * time.Hour},
		{"500 recipients = 5h", 500, 5 * time.Hour},
		{"600 recipients = 6h", 600, 6 * time.Hour},
		{"800 recipients = 8h (matches default)", 800, 8 * time.Hour},
		{"1000 recipients = 8h (cap)", 1000, 8 * time.Hour},
		{"5000 recipients = 8h (cap)", 5000, 8 * time.Hour},
		{"200 recipients = 2h", 200, 2 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := minSpanForVolume(tt.recipients)
			if got != tt.want {
				t.Errorf("minSpanForVolume(%d) = %v, want %v", tt.recipients, got, tt.want)
			}
		})
	}
}

func TestWaveSanityCheck_ProportionalSpan(t *testing.T) {
	now := time.Now().UTC()

	makeWaves := func(span time.Duration, totalRecipients int) []pmtaWaveSpec {
		waves := make([]pmtaWaveSpec, 4)
		perWave := totalRecipients / 4
		for i := 0; i < 4; i++ {
			planned := perWave
			if i == 3 {
				planned = totalRecipients - perWave*3
			}
			waves[i] = pmtaWaveSpec{
				WaveNumber:        i + 1,
				ScheduledAt:       now.Add(time.Duration(i) * span / 3),
				PlannedRecipients: planned,
			}
		}
		return waves
	}

	tests := []struct {
		name       string
		quota      int
		recipients int
		span       time.Duration
		wantErr    bool
	}{
		{"600 actual recipients, 6h span — passes", 2000, 600, 6 * time.Hour, false},
		{"600 actual recipients, 4h span — fails", 2000, 600, 4 * time.Hour, true},
		{"270 actual recipients (quota 1500), 7h15m span — passes (under threshold)", 1500, 270, 7*time.Hour + 15*time.Minute, false},
		{"500 actual recipients, 5h span — passes", 1500, 500, 5 * time.Hour, false},
		{"500 actual recipients, 3h span — fails", 1500, 500, 3 * time.Hour, true},
		{"1000 actual recipients, 8h span — passes", 1500, 1000, 8 * time.Hour, false},
		{"1000 actual recipients, 7h span — fails", 1500, 1000, 7 * time.Hour, true},
		{"300 actual recipients (quota 1500), any span — skipped", 1500, 300, 1 * time.Hour, false},
		{"200 actual recipients (quota 800), any span — skipped", 800, 200, 30 * time.Minute, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plans := []pmtaNormalizedPlan{{
				ISP:   "charter",
				Quota: tt.quota,
			}}
			wavesByISP := map[string][]pmtaWaveSpec{
				"charter": makeWaves(tt.span, tt.recipients),
			}
			err := waveSanityCheck(plans, wavesByISP)
			if tt.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestWaveSanityCheck_QuotaVsActualCount(t *testing.T) {
	now := time.Now().UTC()

	t.Run("high quota low actual count skips check", func(t *testing.T) {
		plans := []pmtaNormalizedPlan{{
			ISP:   "charter",
			Quota: 1500,
		}}
		waves := map[string][]pmtaWaveSpec{
			"charter": {
				{WaveNumber: 1, ScheduledAt: now, PlannedRecipients: 70},
				{WaveNumber: 2, ScheduledAt: now.Add(15 * time.Minute), PlannedRecipients: 70},
				{WaveNumber: 3, ScheduledAt: now.Add(30 * time.Minute), PlannedRecipients: 70},
				{WaveNumber: 4, ScheduledAt: now.Add(45 * time.Minute), PlannedRecipients: 60},
			},
		}
		err := waveSanityCheck(plans, waves)
		if err != nil {
			t.Errorf("should skip: actual recipients (270) < 500 threshold, but got: %v", err)
		}
	})

	t.Run("high quota high actual count enforces check", func(t *testing.T) {
		plans := []pmtaNormalizedPlan{{
			ISP:   "gmail",
			Quota: 5000,
		}}
		waves := map[string][]pmtaWaveSpec{
			"gmail": {
				{WaveNumber: 1, ScheduledAt: now, PlannedRecipients: 500},
				{WaveNumber: 2, ScheduledAt: now.Add(1 * time.Hour), PlannedRecipients: 500},
				{WaveNumber: 3, ScheduledAt: now.Add(2 * time.Hour), PlannedRecipients: 500},
				{WaveNumber: 4, ScheduledAt: now.Add(3 * time.Hour), PlannedRecipients: 500},
			},
		}
		err := waveSanityCheck(plans, waves)
		if err == nil {
			t.Error("should fail: 2000 recipients in 3h span, but passed")
		}
	})
}

func TestWaveSanityCheck_UserExplicitDurationBypass(t *testing.T) {
	now := time.Now().UTC()

	t.Run("user-explicit duration-calc bypasses min-span", func(t *testing.T) {
		plans := []pmtaNormalizedPlan{{
			ISP:   "microsoft",
			Quota: 1500,
			TimeSpans: []pmtaNormalizedTimeSpan{{
				StartAt: now,
				EndAt:   now.Add(4 * time.Hour),
				Source:  "duration-calc",
			}},
		}}
		waves := map[string][]pmtaWaveSpec{
			"microsoft": {
				{WaveNumber: 1, ScheduledAt: now, PlannedRecipients: 213},
				{WaveNumber: 2, ScheduledAt: now.Add(90 * time.Minute), PlannedRecipients: 213},
				{WaveNumber: 3, ScheduledAt: now.Add(3 * time.Hour), PlannedRecipients: 214},
				{WaveNumber: 4, ScheduledAt: now.Add(4*time.Hour + 30*time.Minute), PlannedRecipients: 213},
			},
		}
		err := waveSanityCheck(plans, waves)
		if err != nil {
			t.Errorf("user-explicit duration should bypass min-span check, got: %v", err)
		}
	})

	t.Run("user-explicit manual source bypasses min-span", func(t *testing.T) {
		plans := []pmtaNormalizedPlan{{
			ISP:   "gmail",
			Quota: 5000,
			TimeSpans: []pmtaNormalizedTimeSpan{{
				StartAt: now,
				EndAt:   now.Add(3 * time.Hour),
				Source:  "manual",
			}},
		}}
		waves := map[string][]pmtaWaveSpec{
			"gmail": {
				{WaveNumber: 1, ScheduledAt: now, PlannedRecipients: 500},
				{WaveNumber: 2, ScheduledAt: now.Add(1 * time.Hour), PlannedRecipients: 500},
				{WaveNumber: 3, ScheduledAt: now.Add(2 * time.Hour), PlannedRecipients: 500},
				{WaveNumber: 4, ScheduledAt: now.Add(3 * time.Hour), PlannedRecipients: 500},
			},
		}
		err := waveSanityCheck(plans, waves)
		if err != nil {
			t.Errorf("manual source should bypass min-span check, got: %v", err)
		}
	})

	t.Run("auto-generated span still enforces min-span", func(t *testing.T) {
		plans := []pmtaNormalizedPlan{{
			ISP:   "gmail",
			Quota: 5000,
			TimeSpans: []pmtaNormalizedTimeSpan{{
				StartAt: now,
				EndAt:   now.Add(3 * time.Hour),
				Source:  "default_throttle_window",
			}},
		}}
		waves := map[string][]pmtaWaveSpec{
			"gmail": {
				{WaveNumber: 1, ScheduledAt: now, PlannedRecipients: 500},
				{WaveNumber: 2, ScheduledAt: now.Add(1 * time.Hour), PlannedRecipients: 500},
				{WaveNumber: 3, ScheduledAt: now.Add(2 * time.Hour), PlannedRecipients: 500},
				{WaveNumber: 4, ScheduledAt: now.Add(3 * time.Hour), PlannedRecipients: 500},
			},
		}
		err := waveSanityCheck(plans, waves)
		if err == nil {
			t.Error("auto-generated span should still enforce min-span, but passed")
		}
	})

	t.Run("user-explicit still enforces min-wave-count", func(t *testing.T) {
		plans := []pmtaNormalizedPlan{{
			ISP:   "microsoft",
			Quota: 1500,
			TimeSpans: []pmtaNormalizedTimeSpan{{
				StartAt: now,
				EndAt:   now.Add(4 * time.Hour),
				Source:  "duration-calc",
			}},
		}}
		waves := map[string][]pmtaWaveSpec{
			"microsoft": {
				{WaveNumber: 1, ScheduledAt: now, PlannedRecipients: 400},
				{WaveNumber: 2, ScheduledAt: now.Add(2 * time.Hour), PlannedRecipients: 400},
				{WaveNumber: 3, ScheduledAt: now.Add(4 * time.Hour), PlannedRecipients: 400},
			},
		}
		err := waveSanityCheck(plans, waves)
		if err == nil {
			t.Error("user-explicit should still enforce min-wave-count, but passed")
		}
	})
}

// TestWaveSanityCheck_GlobalDefaultSourceFails locks in the regression for the
// wizard bug where Quick Schedule emitted source: "global-default" with
// start_at == end_at (zero span). At >= small-ISP threshold this silently
// failed wave creation. The fix re-points Quick Schedule to source: "manual"
// with a computed 8h span; this test ensures any future re-introduction of
// "global-default" continues to be rejected.
func TestWaveSanityCheck_GlobalDefaultSourceFails(t *testing.T) {
	now := time.Now().UTC()

	t.Run("global-default does NOT bypass min-span", func(t *testing.T) {
		plans := []pmtaNormalizedPlan{{
			ISP:   "gmail",
			Quota: 5000,
			TimeSpans: []pmtaNormalizedTimeSpan{{
				StartAt: now,
				EndAt:   now,
				Source:  "global-default",
			}},
		}}
		waves := map[string][]pmtaWaveSpec{
			"gmail": {
				{WaveNumber: 1, ScheduledAt: now, PlannedRecipients: 5000},
			},
		}
		err := waveSanityCheck(plans, waves)
		if err == nil {
			t.Error("'global-default' source must NOT bypass waveSanityCheck (regression: zero-span Quick Schedule wizard bug)")
		}
	})

	t.Run("global-default zero-span fails even at small ISP threshold", func(t *testing.T) {
		// 600 recipients > smallISPThreshold (500); auto-generated span path
		// must enforce min-span and the zero-span here must be rejected.
		plans := []pmtaNormalizedPlan{{
			ISP:   "yahoo",
			Quota: 600,
			TimeSpans: []pmtaNormalizedTimeSpan{{
				StartAt: now,
				EndAt:   now,
				Source:  "global-default",
			}},
		}}
		waves := map[string][]pmtaWaveSpec{
			"yahoo": {
				{WaveNumber: 1, ScheduledAt: now, PlannedRecipients: 600},
			},
		}
		err := waveSanityCheck(plans, waves)
		if err == nil {
			t.Error("'global-default' zero-span at 600 recipients must be rejected, not bypassed")
		}
	})
}

func TestIsUserExplicitSpan(t *testing.T) {
	tests := []struct {
		source string
		want   bool
	}{
		{"duration-calc", true},
		{"manual", true},
		{"default_throttle_window", false},
		{"legacy_throttle_window", false},
		{"global-default", false}, // wizard bug regression: must NOT bypass
		{"auto", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			if got := isUserExplicitSpan(tt.source); got != tt.want {
				t.Errorf("isUserExplicitSpan(%q) = %v, want %v", tt.source, got, tt.want)
			}
		})
	}
}

func TestPlanPMTAAudience_EarlyQuotaCutoff(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	listID := "aaaaaaaa-0000-0000-0000-000000000001"

	gmailQuota := 5
	yahooQuota := 3
	totalQuota := gmailQuota + yahooQuota

	// Seed 100 Gmail + 100 Yahoo subscribers — far exceeding the quotas.
	subscriberRows := sqlmock.NewRows([]string{"id", "email"})
	for i := 0; i < 100; i++ {
		subscriberRows.AddRow(
			fmt.Sprintf("11111111-0000-0000-0000-%012d", i),
			fmt.Sprintf("user%d@gmail.com", i),
		)
	}
	for i := 0; i < 100; i++ {
		subscriberRows.AddRow(
			fmt.Sprintf("22222222-0000-0000-0000-%012d", i),
			fmt.Sprintf("user%d@yahoo.com", i),
		)
	}

	mock.ExpectQuery("SELECT s.id::text, s.email").
		WithArgs(listID).
		WillReturnRows(subscriberRows)

	input := engine.PMTACampaignInput{
		InclusionLists: []string{listID},
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: gmailQuota},
			{ISP: "yahoo", Quota: yahooQuota},
		},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{
			{ISP: "gmail", Quota: gmailQuota},
			{ISP: "yahoo", Quota: yahooQuota},
		},
	}

	result, err := planPMTAAudience(context.Background(), db, orgID, input, normalized, NewSuppressionMatcher(), nil)
	if err != nil {
		t.Fatalf("planPMTAAudience: %v", err)
	}

	if result.CountsByISP["gmail"] != gmailQuota {
		t.Errorf("gmail count = %d, want %d", result.CountsByISP["gmail"], gmailQuota)
	}
	if result.CountsByISP["yahoo"] != yahooQuota {
		t.Errorf("yahoo count = %d, want %d", result.CountsByISP["yahoo"], yahooQuota)
	}
	if result.SelectedTotal != totalQuota {
		t.Errorf("SelectedTotal = %d, want %d", result.SelectedTotal, totalQuota)
	}

	// The early cutoff should have stopped scanning well before 200 rows.
	// TotalSeen is the count of unique emails processed (deduped). With
	// the cutoff active, it must be much less than the full 200.
	if result.TotalSeen >= 200 {
		t.Errorf("TotalSeen = %d — expected early cutoff to stop before scanning all 200 rows", result.TotalSeen)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestPlanPMTAAudience_UnlimitedQuotaStreamsAll(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	listID := "aaaaaaaa-0000-0000-0000-000000000002"

	subscriberRows := sqlmock.NewRows([]string{"id", "email"})
	for i := 0; i < 50; i++ {
		subscriberRows.AddRow(
			fmt.Sprintf("33333333-0000-0000-0000-%012d", i),
			fmt.Sprintf("user%d@gmail.com", i),
		)
	}

	mock.ExpectQuery("SELECT s.id::text, s.email").
		WithArgs(listID).
		WillReturnRows(subscriberRows)

	input := engine.PMTACampaignInput{
		InclusionLists: []string{listID},
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 0}, // 0 = unlimited
		},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{
			{ISP: "gmail", Quota: 0},
		},
	}

	result, err := planPMTAAudience(context.Background(), db, orgID, input, normalized, NewSuppressionMatcher(), nil)
	if err != nil {
		t.Fatalf("planPMTAAudience: %v", err)
	}

	if result.CountsByISP["gmail"] != 50 {
		t.Errorf("gmail count = %d, want 50 (unlimited quota should select all)", result.CountsByISP["gmail"])
	}
	if result.TotalSeen != 50 {
		t.Errorf("TotalSeen = %d, want 50", result.TotalSeen)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Master-selection tests (Master List Migration P3b/P5c).
//
// These cover the SDS-sourced audience path that fires when the campaign row
// has use_master_selection=true. The path has two passes:
//
//   1. Primary — mailing_subscribers JOIN mailing_subscriber_domain_state on
//      (subscriber_id, sending_domain). Selects subscribers with an existing
//      SDS row for this sending domain.
//
//   2. Cold-fallback — mailing_subscribers with a NOT EXISTS anti-join on
//      SDS. Covers two real scenarios:
//        - Cold-start for a new sending domain (zero SDS rows exist yet)
//        - Subscribers imported after the P3a backfill who have no SDS row
//          anywhere yet; first send will mint the row and flip to 'warming'.
//
// Both passes must enforce global suppression filters (status,
// hard_bounced_at, complained_at) and organization scoping.
// ---------------------------------------------------------------------------

func TestPlanPMTAAudience_MasterSelection_PrimaryPassReturnsCandidates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "bbbbbbbb-0000-0000-0000-000000000001"
	sendingDomain := "em.discountblog.com"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)

	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(true))

	// Primary SDS pass returns 3 gmail subscribers. The query embeds the
	// sending_domain and org_id as args; we assert both are passed so a
	// regression dropping either filter is caught immediately.
	sdsRows := sqlmock.NewRows([]string{"id", "email"}).
		AddRow("11111111-0000-0000-0000-000000000001", "alice@gmail.com").
		AddRow("11111111-0000-0000-0000-000000000002", "bob@gmail.com").
		AddRow("11111111-0000-0000-0000-000000000003", "carol@gmail.com")
	mock.ExpectQuery(`FROM mailing_subscribers sub\s+JOIN mailing_subscriber_domain_state sds`).
		WithArgs(sendingDomain, orgID, sqlmock.AnyArg()).
		WillReturnRows(sdsRows)

	// Cold-fallback fires a per-ISP stripe query for each ISP with a
	// remaining shortfall (gmail here: quota 10, primary supplied 3).
	// The new shape passes the ISP name as a dedicated argument so the
	// planner can filter mailing_subscribers.isp directly.
	mock.ExpectQuery(`FROM mailing_subscribers sub\s+WHERE sub\.status IN \('active','confirmed'\).*sub\.isp = \$3`).
		WithArgs(sendingDomain, orgID, "gmail", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))

	// After per-ISP pass, a catch-all scan picks up unclassified
	// subscribers (isp IS NULL OR isp = ''); also returns empty here.
	mock.ExpectQuery(`FROM mailing_subscribers sub\s+WHERE sub\.status IN \('active','confirmed'\).*sub\.isp IS NULL OR sub\.isp = ''`).
		WithArgs(sendingDomain, orgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))

	input := engine.PMTACampaignInput{
		CampaignID:    campaignID,
		SendingDomain: sendingDomain,
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 10},
		},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{{ISP: "gmail", Quota: 10}},
	}

	result, err := planPMTAAudience(context.Background(), db, orgID, input, normalized, NewSuppressionMatcher(), nil)
	if err != nil {
		t.Fatalf("planPMTAAudience: %v", err)
	}

	if result.CountsByISP["gmail"] != 3 {
		t.Errorf("gmail count = %d, want 3", result.CountsByISP["gmail"])
	}
	for _, rec := range result.RecipientsByISP["gmail"] {
		if rec.SourceType != "sds" {
			t.Errorf("expected SourceType=sds, got %q", rec.SourceType)
		}
		if rec.SourceID != sendingDomain {
			t.Errorf("expected SourceID=%q, got %q", sendingDomain, rec.SourceID)
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestPlanPMTAAudience_MasterSelection_ColdFallbackWhenPrimaryEmpty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "bbbbbbbb-0000-0000-0000-000000000002"
	sendingDomain := "em.brand-new.com"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(true))

	mock.ExpectQuery(`FROM mailing_subscribers sub\s+JOIN mailing_subscriber_domain_state sds`).
		WithArgs(sendingDomain, orgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))

	// Cold-fallback fills the audience from mailing_subscribers without
	// an SDS row for this domain. This is the cold-start scenario:
	// brand-new sending domain, zero SDS coverage.
	//
	// With the per-ISP + hash-stripe fallback, quota 2 is exhausted by the
	// gmail per-ISP pass; allQuotasMet() short-circuits the catch-all pass.
	fallbackRows := sqlmock.NewRows([]string{"id", "email"}).
		AddRow("22222222-0000-0000-0000-000000000001", "new1@gmail.com").
		AddRow("22222222-0000-0000-0000-000000000002", "new2@gmail.com")
	mock.ExpectQuery(`FROM mailing_subscribers sub\s+WHERE sub\.status IN \('active','confirmed'\).*sub\.isp = \$3`).
		WithArgs(sendingDomain, orgID, "gmail", sqlmock.AnyArg()).
		WillReturnRows(fallbackRows)

	input := engine.PMTACampaignInput{
		CampaignID:    campaignID,
		SendingDomain: sendingDomain,
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 2},
		},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{{ISP: "gmail", Quota: 2}},
	}

	result, err := planPMTAAudience(context.Background(), db, orgID, input, normalized, NewSuppressionMatcher(), nil)
	if err != nil {
		t.Fatalf("planPMTAAudience: %v", err)
	}

	if result.CountsByISP["gmail"] != 2 {
		t.Errorf("gmail count = %d, want 2 (cold-fallback)", result.CountsByISP["gmail"])
	}
	for _, rec := range result.RecipientsByISP["gmail"] {
		if rec.SourceType != "sds_cold" {
			t.Errorf("expected SourceType=sds_cold, got %q", rec.SourceType)
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestPlanPMTAAudience_MasterSelection_RequiresSendingDomain(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "bbbbbbbb-0000-0000-0000-000000000003"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(true))

	input := engine.PMTACampaignInput{
		CampaignID:    campaignID,
		SendingDomain: "", // deliberately empty to trigger the guard
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 10},
		},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{{ISP: "gmail", Quota: 10}},
	}

	_, err = planPMTAAudience(context.Background(), db, orgID, input, normalized, NewSuppressionMatcher(), nil)
	if err == nil {
		t.Fatal("expected error when master selection has no sending_domain, got nil")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestPlanPMTAAudience_MasterSelection_FlagOffSkipsSDSQueries(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "bbbbbbbb-0000-0000-0000-000000000004"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)
	// Flag is false — SDS path must NOT be invoked and no SDS queries
	// should be expected. Without any inclusion lists there is nothing
	// else to run, so the planner returns an empty audience cleanly.
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(false))

	input := engine.PMTACampaignInput{
		CampaignID:    campaignID,
		SendingDomain: "em.example.com",
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 10},
		},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{{ISP: "gmail", Quota: 10}},
	}

	result, err := planPMTAAudience(context.Background(), db, orgID, input, normalized, NewSuppressionMatcher(), nil)
	if err != nil {
		t.Fatalf("planPMTAAudience: %v", err)
	}
	if result.SelectedTotal != 0 {
		t.Errorf("SelectedTotal = %d, want 0 (no inclusion lists, master flag off)", result.SelectedTotal)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestPlanPMTAAudience_RespectsSendPriority(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	list1 := "aaaaaaaa-0000-0000-0000-000000000003"
	list2 := "aaaaaaaa-0000-0000-0000-000000000004"

	list1Rows := sqlmock.NewRows([]string{"id", "email"})
	for i := 0; i < 10; i++ {
		list1Rows.AddRow(
			fmt.Sprintf("44444444-0000-0000-0000-%012d", i),
			fmt.Sprintf("priority%d@gmail.com", i),
		)
	}
	list2Rows := sqlmock.NewRows([]string{"id", "email"})
	for i := 0; i < 10; i++ {
		list2Rows.AddRow(
			fmt.Sprintf("55555555-0000-0000-0000-%012d", i),
			fmt.Sprintf("secondary%d@gmail.com", i),
		)
	}

	// First list streamed (high priority)
	mock.ExpectQuery("SELECT s.id::text, s.email").
		WithArgs(list1).
		WillReturnRows(list1Rows)
	// Second list should NOT be queried because quota (3) is met by list1

	input := engine.PMTACampaignInput{
		InclusionLists: []string{list1, list2},
		SendPriority: []engine.PriorityItem{
			{ID: list1, Type: "list"},
			{ID: list2, Type: "list"},
		},
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 3},
		},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{
			{ISP: "gmail", Quota: 3},
		},
	}

	result, err := planPMTAAudience(context.Background(), db, orgID, input, normalized, NewSuppressionMatcher(), nil)
	if err != nil {
		t.Fatalf("planPMTAAudience: %v", err)
	}

	if result.CountsByISP["gmail"] != 3 {
		t.Errorf("gmail count = %d, want 3", result.CountsByISP["gmail"])
	}

	// All selected should be from the priority list
	for _, rec := range result.RecipientsByISP["gmail"] {
		if rec.SourceID != list1 {
			t.Errorf("expected all recipients from priority list %s, got source %s", list1, rec.SourceID)
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}
