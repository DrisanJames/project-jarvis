package api

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// Operator ruling 2026-08-24: capped inclusion-segment draws fill
// most-recently-engaged first (SDS recency for the campaign's sending
// domain). These pin the flag semantics — recency is the DEFAULT, the kill
// switch falls back to rotation, and rotation's own kill switch still works
// underneath it.
func TestRecencyAudienceDrawFlag(t *testing.T) {
	t.Setenv("RECENCY_AUDIENCE_DRAW_DISABLED", "")
	if !recencyAudienceDrawEnabled() {
		t.Fatal("recency draw must be ON by default")
	}
	t.Setenv("RECENCY_AUDIENCE_DRAW_DISABLED", "true")
	if recencyAudienceDrawEnabled() {
		t.Fatal("kill switch must disable the recency draw")
	}
	// With recency disabled, rotation remains available under its own flag.
	t.Setenv("DISABLE_ROTATING_AUDIENCE_SELECTION", "")
	if !rotatingAudienceSelectionEnabled() {
		t.Fatal("rotation fallback must remain available")
	}
	os.Unsetenv("RECENCY_AUDIENCE_DRAW_DISABLED")
}

// The DEFAULT capped-segment draw (operator ruling 2026-08-24): the query
// orders by GLOBAL engagement recency via the mailing_subscribers join, NULLS
// LAST, bounded by the scan LIMIT. Mirrors the rotation twin below the flag.
func TestPlanPMTAAudience_CappedSegment_RecencyFirstByDefault(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "dddddddd-0000-0000-0000-000000000011"
	segmentID := "eeeeeeee-0000-0000-0000-0000000000c1"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(true))

	mock.ExpectQuery(`FROM mailing_segment_members m\s+LEFT JOIN mailing_subscribers s ON s\.id = m\.subscriber_id\s+WHERE m\.segment_id = \$1\s+ORDER BY GREATEST\(s\.last_click_at, s\.last_open_at\) DESC NULLS LAST LIMIT \d+`).
		WithArgs(segmentID).
		WillReturnRows(sqlmock.NewRows([]string{"subscriber_id", "email"}).
			AddRow("aaaaaaaa-0000-0000-0000-000000000002", "r1@gmail.com"))

	input := engine.PMTACampaignInput{
		CampaignID:    campaignID,
		SendingDomain: "em.recency.com",
		SendPriority: []engine.PriorityItem{
			{Type: "segment", ID: segmentID},
		},
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 1},
		},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{{ISP: "gmail", Quota: 1}},
	}
	result, err := planPMTAAudience(context.Background(), db, orgID, input, normalized, NewSuppressionMatcher(), nil)
	if err != nil {
		t.Fatalf("planPMTAAudience: %v", err)
	}
	if result.CountsByISP["gmail"] != 1 {
		t.Errorf("gmail count = %d, want 1", result.CountsByISP["gmail"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations (recency draw by default): %v", err)
	}
}
