package api

import (
	"context"
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// =============================================================================
// Hybrid priority-prelude tests (2026-04-22)
// =============================================================================
// These tests pin the behaviour of the master-selection path when
// send_priority and/or inclusion_segments are set. The prelude drains the
// priority sources FIRST and whatever ISP quota remains falls through to
// the existing SDS + cold-fallback passes.
//
// Invariants enforced:
//   1. When the prelude fills every ISP quota, streamSDS short-circuits via
//      allQuotasMet() and fires NO primary SDS query. (This is what makes
//      a freshly-imported vendor cohort addressable on day one — the
//      subscribers have no SDS row yet.)
//   2. When the prelude undersupplies an ISP, the SDS primary pass runs
//      for that ISP and the union satisfies the quota.
//   3. An empty materialized segment (matCount=0) is skipped gracefully
//      and control flows to SDS exactly as if no prelude was configured.
//   4. The prelude does NOT fire for non-master-selection campaigns
//      (backwards compatibility with the existing SendPriority branch
//      which is already exercised by pmta_campaign_planner_test.go and
//      handlers_pmta_campaign.go integration paths).

// segmentCountRegex / segmentMembersRegex capture the two queries streamSegment
// issues against mailing_segment_members. Kept as constants so the tests
// double as regression guards: if anyone changes the materialized-members
// read shape, these tests fail loudly.
const (
	segmentCountQuery   = `SELECT COUNT\(\*\) FROM mailing_segment_members WHERE segment_id = \$1`
	segmentMembersQuery = `SELECT subscriber_id::text, email FROM mailing_segment_members WHERE segment_id = \$1`
)

// TestPlanPMTAAudience_Hybrid_PreludeFillsAllQuotas_SkipsSDS verifies the
// prelude can fully satisfy a campaign when the priority segment carries
// enough subscribers for every ISP quota. SDS MUST NOT be queried in this
// case — not just for efficiency but because a freshly-imported vendor
// cohort has no SDS rows and the SDS query would return nothing anyway.
func TestPlanPMTAAudience_Hybrid_PreludeFillsAllQuotas_SkipsSDS(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "bbbbbbbb-0000-0000-0000-00000000f001"
	segmentID := "cccccccc-0000-0000-0000-00000000abcd"
	sendingDomain := "em.hybrid-full.com"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(true))

	// Prelude: count says 3 materialized members, then stream returns
	// one per configured ISP, exactly filling the quotas.
	mock.ExpectQuery(segmentCountQuery).
		WithArgs(segmentID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	mock.ExpectQuery(segmentMembersQuery).
		WithArgs(segmentID).
		WillReturnRows(sqlmock.NewRows([]string{"subscriber_id", "email"}).
			AddRow("11111111-1111-1111-1111-111111111101", "a@gmail.com").
			AddRow("11111111-1111-1111-1111-111111111102", "b@yahoo.com").
			AddRow("11111111-1111-1111-1111-111111111103", "c@aol.com"))

	// No further expectations — allQuotasMet() is true when streamSDS
	// runs, so the planner MUST NOT issue the primary or fallback queries.

	input := engine.PMTACampaignInput{
		CampaignID:    campaignID,
		SendingDomain: sendingDomain,
		SendPriority: []engine.PriorityItem{
			{Type: "segment", ID: segmentID},
		},
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 1},
			{ISP: "yahoo", Quota: 1},
			{ISP: "aol", Quota: 1},
		},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{
			{ISP: "gmail", Quota: 1},
			{ISP: "yahoo", Quota: 1},
			{ISP: "aol", Quota: 1},
		},
	}
	result, err := planPMTAAudience(context.Background(), db, orgID, input, normalized, NewSuppressionMatcher(), nil)
	if err != nil {
		t.Fatalf("planPMTAAudience: %v", err)
	}
	if result.CountsByISP["gmail"] != 1 {
		t.Errorf("gmail count = %d, want 1", result.CountsByISP["gmail"])
	}
	if result.CountsByISP["yahoo"] != 1 {
		t.Errorf("yahoo count = %d, want 1", result.CountsByISP["yahoo"])
	}
	if result.CountsByISP["aol"] != 1 {
		t.Errorf("aol count = %d, want 1", result.CountsByISP["aol"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// TestPlanPMTAAudience_Hybrid_PreludeUndersupplies_SDSFillsRemaining
// verifies that when the priority segment carries SOME but not all of
// the required subscribers, the SDS pass runs for the remaining ISP quota.
// This is the common case for a medium vendor batch paired with a large
// quota plan.
func TestPlanPMTAAudience_Hybrid_PreludeUndersupplies_SDSFillsRemaining(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "bbbbbbbb-0000-0000-0000-00000000f002"
	segmentID := "cccccccc-0000-0000-0000-00000000bcde"
	sendingDomain := "em.hybrid-partial.com"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(true))

	// Prelude: segment has 2 members (gmail), quota is 5. After the
	// prelude we still need 3 gmail subscribers.
	mock.ExpectQuery(segmentCountQuery).
		WithArgs(segmentID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(segmentMembersQuery).
		WithArgs(segmentID).
		WillReturnRows(sqlmock.NewRows([]string{"subscriber_id", "email"}).
			AddRow("22222222-0000-0000-0000-000000000001", "s1@gmail.com").
			AddRow("22222222-0000-0000-0000-000000000002", "s2@gmail.com"))

	// SDS primary pass returns two more gmail rows (real flow scans
	// scanLimit rows, we pretend the engaged pool had exactly two).
	mock.ExpectQuery(`FROM mailing_subscribers sub\s+JOIN mailing_subscriber_domain_state sds`).
		WithArgs(sendingDomain, orgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}).
			AddRow("33333333-0000-0000-0000-000000000001", "sds1@gmail.com").
			AddRow("33333333-0000-0000-0000-000000000002", "sds2@gmail.com"))

	// Cold-fallback fires once for gmail to cover the remaining 1 shortfall.
	mock.ExpectQuery(`FROM mailing_subscribers sub.*sub\.isp = \$3`).
		WithArgs(sendingDomain, orgID, "gmail", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}).
			AddRow("44444444-0000-0000-0000-000000000001", "cold1@gmail.com"))

	input := engine.PMTACampaignInput{
		CampaignID:    campaignID,
		SendingDomain: sendingDomain,
		SendPriority: []engine.PriorityItem{
			{Type: "segment", ID: segmentID},
		},
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 5},
		},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{{ISP: "gmail", Quota: 5}},
	}
	result, err := planPMTAAudience(context.Background(), db, orgID, input, normalized, NewSuppressionMatcher(), nil)
	if err != nil {
		t.Fatalf("planPMTAAudience: %v", err)
	}
	if got := result.CountsByISP["gmail"]; got != 5 {
		t.Errorf("gmail count = %d, want 5 (2 from segment + 2 from SDS + 1 from fallback)", got)
	}
	// source-type distribution is preserved — validates that prelude-sourced
	// rows are stamped source_type=segment (vs the SDS rows' source_type=sds/sds_cold_fallback).
	segmentSourced := 0
	for _, rec := range result.RecipientsByISP["gmail"] {
		if rec.SourceType == "segment" {
			segmentSourced++
		}
	}
	if segmentSourced != 2 {
		t.Errorf("expected 2 recipients stamped SourceType=segment, got %d", segmentSourced)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// TestPlanPMTAAudience_Hybrid_EmptySegmentFallsThroughToSDS verifies an
// un-materialized or empty segment (matCount=0) is logged and skipped,
// and the SDS path runs as if no prelude was configured. This is the
// failure mode we hit when a segment is created but not yet hydrated —
// better to fall through than crash the campaign.
func TestPlanPMTAAudience_Hybrid_EmptySegmentFallsThroughToSDS(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "bbbbbbbb-0000-0000-0000-00000000f003"
	segmentID := "cccccccc-0000-0000-0000-00000000cdef"
	sendingDomain := "em.hybrid-empty.com"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(true))

	// Prelude: count returns 0. streamSegment logs the warning and
	// returns nil WITHOUT issuing the members read.
	mock.ExpectQuery(segmentCountQuery).
		WithArgs(segmentID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// SDS primary pass then runs normally and supplies everyone.
	mock.ExpectQuery(`FROM mailing_subscribers sub\s+JOIN mailing_subscriber_domain_state sds`).
		WithArgs(sendingDomain, orgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}).
			AddRow("55555555-0000-0000-0000-000000000001", "sds1@gmail.com").
			AddRow("55555555-0000-0000-0000-000000000002", "sds2@gmail.com"))

	input := engine.PMTACampaignInput{
		CampaignID:    campaignID,
		SendingDomain: sendingDomain,
		SendPriority: []engine.PriorityItem{
			{Type: "segment", ID: segmentID},
		},
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
	if got := result.CountsByISP["gmail"]; got != 2 {
		t.Errorf("gmail count = %d, want 2 (SDS-only since segment was empty)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// TestPlanPMTAAudience_Hybrid_InclusionSegmentAfterSendPriority verifies
// the prelude drains send_priority items first, then inclusion_segments,
// then falls through. The planner must not reorder — operators rely on
// send_priority being the highest-priority source.
func TestPlanPMTAAudience_Hybrid_InclusionSegmentAfterSendPriority(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "bbbbbbbb-0000-0000-0000-00000000f004"
	prioritySeg := "dddddddd-0000-0000-0000-000000000001"
	inclusionSeg := "dddddddd-0000-0000-0000-000000000002"
	sendingDomain := "em.hybrid-order.com"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(true))

	// send_priority segment FIRST — 1 gmail row.
	mock.ExpectQuery(segmentCountQuery).
		WithArgs(prioritySeg).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(segmentMembersQuery).
		WithArgs(prioritySeg).
		WillReturnRows(sqlmock.NewRows([]string{"subscriber_id", "email"}).
			AddRow("66666666-0000-0000-0000-000000000001", "pri@gmail.com"))

	// inclusion_segment SECOND — 1 more gmail row, which fills quota=2.
	mock.ExpectQuery(segmentCountQuery).
		WithArgs(inclusionSeg).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(segmentMembersQuery).
		WithArgs(inclusionSeg).
		WillReturnRows(sqlmock.NewRows([]string{"subscriber_id", "email"}).
			AddRow("66666666-0000-0000-0000-000000000002", "inc@gmail.com"))

	// No SDS queries: quotas fully met after the prelude.

	input := engine.PMTACampaignInput{
		CampaignID:        campaignID,
		SendingDomain:     sendingDomain,
		SendPriority:      []engine.PriorityItem{{Type: "segment", ID: prioritySeg}},
		InclusionSegments: []string{inclusionSeg},
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
	if got := result.CountsByISP["gmail"]; got != 2 {
		t.Errorf("gmail count = %d, want 2", got)
	}
	// Both recipients must be stamped source_type=segment so reporting
	// can attribute this campaign's traffic to the vendor cohort rather
	// than the engaged master pool.
	segCount := 0
	for _, rec := range result.RecipientsByISP["gmail"] {
		if rec.SourceType == "segment" {
			segCount++
		}
	}
	if segCount != 2 {
		t.Errorf("expected both recipients stamped source_type=segment, got %d", segCount)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}
