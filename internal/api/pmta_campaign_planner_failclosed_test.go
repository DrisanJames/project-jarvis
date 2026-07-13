package api

// REQ-002 — planner suppression/exclusion loading FAILS CLOSED.
//
// Before this fix, a load error on a named exclusion suppression list,
// exclusion segment, or the MinRemailHours recently-mailed set was a silent
// `continue` / log line: the campaign planned WITHOUT the protection the
// Draft Board showed it carrying. These tests pin the new contract:
//
//   - a load ERROR fails the plan (the audience finalizer then marks the
//     campaign 'failed' with the stated reason — audience_finalizer.go:179-184);
//   - an EMPTY exclusion list/segment is still fine (excludes nobody) and
//     must NOT brick a legitimate deploy.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

const (
	fcOrgID      = "00000000-0000-0000-0000-000000000001"
	fcListID     = "aaaaaaaa-0000-0000-0000-000000000001"
	fcExclListID = "bbbbbbbb-0000-0000-0000-000000000001"
	fcSegmentID  = "cccccccc-0000-0000-0000-000000000001"
)

func TestExclusionFailClosed_ListLoadError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT md5_hash FROM mailing_suppression_entries WHERE list_id = \$1`).
		WithArgs(fcExclListID).
		WillReturnError(errors.New("relation read failed"))

	input := engine.PMTACampaignInput{
		InclusionLists: []string{fcListID},
		ExclusionLists: []string{fcExclListID},
	}

	_, planErr := planPMTAAudience(context.Background(), db, fcOrgID, input, pmtaNormalizedCampaign{}, NewSuppressionMatcher(), nil)
	if planErr == nil {
		t.Fatal("planPMTAAudience must FAIL when a named exclusion suppression list cannot be loaded — got nil error (fail-open)")
	}
	if !strings.Contains(planErr.Error(), fcExclListID) || !strings.Contains(planErr.Error(), "fail-closed") {
		t.Errorf("plan error must state the failing list and the fail-closed reason, got: %v", planErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestExclusionFailClosed_SegmentCountError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM mailing_segment_members WHERE segment_id = \$1`).
		WithArgs(fcSegmentID).
		WillReturnError(errors.New("statement timeout"))

	input := engine.PMTACampaignInput{
		InclusionLists:    []string{fcListID},
		ExclusionSegments: []string{fcSegmentID},
	}

	_, planErr := planPMTAAudience(context.Background(), db, fcOrgID, input, pmtaNormalizedCampaign{}, NewSuppressionMatcher(), nil)
	if planErr == nil {
		t.Fatal("planPMTAAudience must FAIL when a named exclusion segment count errors — got nil error (fail-open)")
	}
	if !strings.Contains(planErr.Error(), fcSegmentID) {
		t.Errorf("plan error must name the failing segment, got: %v", planErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestExclusionFailClosed_SegmentReadError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM mailing_segment_members WHERE segment_id = \$1`).
		WithArgs(fcSegmentID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(42))
	mock.ExpectQuery(`SELECT email FROM mailing_segment_members WHERE segment_id = \$1`).
		WithArgs(fcSegmentID).
		WillReturnError(errors.New("read failed"))

	input := engine.PMTACampaignInput{
		InclusionLists:    []string{fcListID},
		ExclusionSegments: []string{fcSegmentID},
	}

	_, planErr := planPMTAAudience(context.Background(), db, fcOrgID, input, pmtaNormalizedCampaign{}, NewSuppressionMatcher(), nil)
	if planErr == nil {
		t.Fatal("planPMTAAudience must FAIL when a named exclusion segment read errors — got nil error (fail-open)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestExclusionFailClosed_MinRemailLoadError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT DISTINCT LOWER\(s\.email\)`).
		WillReturnError(errors.New("partition scan failed"))

	input := engine.PMTACampaignInput{
		InclusionLists: []string{fcListID},
		MinRemailHours: 48,
		SendingDomain:  "em.discountblog.com",
	}

	_, planErr := planPMTAAudience(context.Background(), db, fcOrgID, input, pmtaNormalizedCampaign{}, NewSuppressionMatcher(), nil)
	if planErr == nil {
		t.Fatal("planPMTAAudience must FAIL when the operator-requested remail gap cannot be loaded — got nil error (fail-open)")
	}
	if !strings.Contains(planErr.Error(), "min_remail_hours") {
		t.Errorf("plan error must state the remail-gap reason, got: %v", planErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// TestExclusionFailClosed_EmptyListStillPlans is the regression guard for the
// other half of the contract: fail-closed must NOT brick legitimate deploys.
// An exclusion list that loads successfully but is EMPTY plans normally.
func TestExclusionFailClosed_EmptyListStillPlans(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()

	// Exclusion list loads fine — zero rows.
	mock.ExpectQuery(`SELECT md5_hash FROM mailing_suppression_entries WHERE list_id = \$1`).
		WithArgs(fcExclListID).
		WillReturnRows(sqlmock.NewRows([]string{"md5_hash"}))

	subscriberRows := sqlmock.NewRows([]string{"id", "email"})
	for i := 0; i < 10; i++ {
		subscriberRows.AddRow(
			fmt.Sprintf("11111111-0000-0000-0000-%012d", i),
			fmt.Sprintf("user%d@gmail.com", i),
		)
	}
	mock.ExpectQuery("SELECT s.id::text, s.email").
		WithArgs(fcListID).
		WillReturnRows(subscriberRows)

	input := engine.PMTACampaignInput{
		InclusionLists: []string{fcListID},
		ExclusionLists: []string{fcExclListID},
		ISPPlans:       []engine.PMTAISPScheduleInput{{ISP: "gmail", Quota: 5}},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{{ISP: "gmail", Quota: 5}},
	}

	result, planErr := planPMTAAudience(context.Background(), db, fcOrgID, input, normalized, NewSuppressionMatcher(), nil)
	if planErr != nil {
		t.Fatalf("an EMPTY exclusion list must not fail the plan: %v", planErr)
	}
	if result.CountsByISP["gmail"] != 5 {
		t.Errorf("gmail count = %d, want 5", result.CountsByISP["gmail"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}
