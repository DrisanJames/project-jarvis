package api

// REQ-C17 — planner segment_reserves ({segment_id, reserve}, additive).
//
// Contract pinned here (SCHEMA-CONTRACTS.md §5):
//  1. GOLDEN: a payload WITHOUT segment_reserves (nil OR empty slice) issues
//     exactly today's query sequence and produces identical plan output —
//     sqlmock's ordered expectations make ANY extra query a failure, and the
//     two plans are compared field-for-field. (The pre-existing planner test
//     corpus — hybrid/rotation/failclosed/req043/coldfallback/SDS — passes
//     unmodified on this tree, pinning the same invariant across every
//     branch.)
//  2. Reserved-first capped draw: a reserved segment is streamed BEFORE the
//     inclusion loop and stops accepting at its reserve.
//  3. Shortfall fall-through: a reserve the segment cannot fill records the
//     LOUD structured warning (code reserve_shortfall, segment named) and the
//     remaining sources fill the quota — the plan never fails mid-flight nor
//     silently plans short.
//  4. Normalize validation: reserve <= 0 or empty segment_id rejects the
//     deploy at the door.
//
// Run: go test ./internal/api/ -run 'SegmentReserves' -v

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

const (
	rsvOrgID      = "00000000-0000-0000-0000-000000000001"
	rsvCampaignID = "bbbbbbbb-0000-0000-0000-000000000017"
	rsvSegA       = "aaaaaaaa-0000-0000-0000-000000000017" // ordinary inclusion segment
	rsvSegB       = "cccccccc-0000-0000-0000-000000000017" // reserved segment
)

// rsvExpectPreamble mocks the two campaign lookups every plan with a
// CampaignID issues (offer suppression resolve + use_master_selection).
func rsvExpectPreamble(mock sqlmock.Sqlmock, afterReserve func()) {
	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(rsvCampaignID).
		WillReturnError(sql.ErrNoRows)
	if afterReserve != nil {
		afterReserve() // reserved-first reads happen BEFORE the master-selection lookup
	}
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(rsvCampaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(false))
}

func rsvMembersRows(pairs ...[2]string) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"subscriber_id", "email"})
	for _, p := range pairs {
		rows.AddRow(p[0], p[1])
	}
	return rows
}

func rsvPlan(t *testing.T, db *sql.DB, reserves []engine.SegmentReserve, quota int, segments ...string) pmtaAudiencePlan {
	t.Helper()
	input := engine.PMTACampaignInput{
		CampaignID:        rsvCampaignID,
		InclusionSegments: segments,
		SegmentReserves:   reserves,
		ISPPlans:          []engine.PMTAISPScheduleInput{{ISP: "gmail", Quota: quota}},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{{ISP: "gmail", Quota: quota}},
	}
	plan, err := planPMTAAudience(context.Background(), db, rsvOrgID, input, normalized, NewSuppressionMatcher(), nil)
	if err != nil {
		t.Fatalf("planPMTAAudience: %v", err)
	}
	return plan
}

// ---------------------------------------------------------------------------
// 1. GOLDEN — absent field (nil AND empty) = identical queries + identical plan
// ---------------------------------------------------------------------------

func TestSegmentReserves_GoldenAbsentFieldIdentical(t *testing.T) {
	t.Setenv("DISABLE_RESERVE_POOL", "true")

	runOnce := func(reserves []engine.SegmentReserve) pmtaAudiencePlan {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock: %v", err)
		}
		defer db.Close()

		// The EXACT pre-change sequence: offer lookup → master-selection
		// lookup → ONE members read for the inclusion segment. Any
		// reserved-pass query would violate the ordered expectations.
		rsvExpectPreamble(mock, nil)
		mock.ExpectQuery(segmentMembersQuery).
			WithArgs(rsvSegA).
			WillReturnRows(rsvMembersRows(
				[2]string{"55555555-0000-0000-0000-000000000001", "g1@gmail.com"},
				[2]string{"55555555-0000-0000-0000-000000000002", "g2@gmail.com"},
			))

		plan := rsvPlan(t, db, reserves, 2, rsvSegA)
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("query sequence deviated from the pre-change golden: %v", err)
		}
		return plan
	}

	nilPlan := runOnce(nil)
	emptyPlan := runOnce([]engine.SegmentReserve{})

	if !reflect.DeepEqual(nilPlan, emptyPlan) {
		t.Fatalf("nil vs empty segment_reserves produced different plans:\nnil:   %+v\nempty: %+v", nilPlan, emptyPlan)
	}
	if nilPlan.SelectedTotal != 2 || nilPlan.CountsByISP["gmail"] != 2 {
		t.Fatalf("golden fixture selected %d (gmail %d), want 2/2", nilPlan.SelectedTotal, nilPlan.CountsByISP["gmail"])
	}
	if len(nilPlan.PlanWarnings) != 0 {
		t.Fatalf("golden fixture must carry no warnings, got %+v", nilPlan.PlanWarnings)
	}
}

// ---------------------------------------------------------------------------
// 2. Reserved-first capped draw
// ---------------------------------------------------------------------------

func TestSegmentReserves_ReservedFirstCappedDraw(t *testing.T) {
	t.Setenv("DISABLE_RESERVE_POOL", "true")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// Reserved pass: segment B is read FIRST (before the master-selection
	// lookup) and must stop accepting at reserve=2 even though 4 members
	// stream. Then the inclusion loop reads A; quota (3) fills after 1 A
	// member, and B's normal-position turn never issues a query.
	rsvExpectPreamble(mock, func() {
		mock.ExpectQuery(segmentMembersQuery).
			WithArgs(rsvSegB).
			WillReturnRows(rsvMembersRows(
				[2]string{"66666666-0000-0000-0000-000000000001", "b1@gmail.com"},
				[2]string{"66666666-0000-0000-0000-000000000002", "b2@gmail.com"},
				[2]string{"66666666-0000-0000-0000-000000000003", "b3@gmail.com"},
				[2]string{"66666666-0000-0000-0000-000000000004", "b4@gmail.com"},
			))
	})
	mock.ExpectQuery(segmentMembersQuery).
		WithArgs(rsvSegA).
		WillReturnRows(rsvMembersRows(
			[2]string{"55555555-0000-0000-0000-000000000001", "a1@gmail.com"},
			[2]string{"55555555-0000-0000-0000-000000000002", "a2@gmail.com"},
		))

	plan := rsvPlan(t, db,
		[]engine.SegmentReserve{{SegmentID: rsvSegB, Reserve: 2}},
		3, rsvSegA, rsvSegB)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	recips := plan.RecipientsByISP["gmail"]
	if len(recips) != 3 {
		t.Fatalf("selected %d recipients, want 3", len(recips))
	}
	// Reserved-first ordering: B's two reserved members hold ranks 1-2.
	if recips[0].SourceID != rsvSegB || recips[1].SourceID != rsvSegB {
		t.Errorf("reserved segment must fill first: got sources %s, %s", recips[0].SourceID, recips[1].SourceID)
	}
	if recips[2].SourceID != rsvSegA {
		t.Errorf("third seat should come from segment A, got %s", recips[2].SourceID)
	}
	// Capped draw: exactly 2 from B despite 4 streaming.
	bCount := 0
	for _, r := range recips {
		if r.SourceID == rsvSegB {
			bCount++
		}
	}
	if bCount != 2 {
		t.Errorf("reserve-capped draw violated: %d from reserved segment, want 2", bCount)
	}
	if len(plan.PlanWarnings) != 0 {
		t.Errorf("filled reserve must not warn, got %+v", plan.PlanWarnings)
	}
}

// ---------------------------------------------------------------------------
// 3. Shortfall fall-through — LOUD warning, remaining sources fill the plan
// ---------------------------------------------------------------------------

func TestSegmentReserves_ShortfallFallsThroughLoudly(t *testing.T) {
	t.Setenv("DISABLE_RESERVE_POOL", "true")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// Reserved pass: B reserved 3 but only 1 member exists. The reserved
	// read returns 1 row; shortfall recorded. Inclusion loop: A fills the
	// remaining 2 seats; then B's normal turn re-reads it (quota not yet
	// met when its turn arrives is false here — quota 3 met after A), so no
	// second B query.
	rsvExpectPreamble(mock, func() {
		mock.ExpectQuery(segmentMembersQuery).
			WithArgs(rsvSegB).
			WillReturnRows(rsvMembersRows(
				[2]string{"66666666-0000-0000-0000-000000000001", "b1@gmail.com"},
			))
	})
	mock.ExpectQuery(segmentMembersQuery).
		WithArgs(rsvSegA).
		WillReturnRows(rsvMembersRows(
			[2]string{"55555555-0000-0000-0000-000000000001", "a1@gmail.com"},
			[2]string{"55555555-0000-0000-0000-000000000002", "a2@gmail.com"},
			[2]string{"55555555-0000-0000-0000-000000000003", "a3@gmail.com"},
		))

	plan := rsvPlan(t, db,
		[]engine.SegmentReserve{{SegmentID: rsvSegB, Reserve: 3}},
		3, rsvSegA, rsvSegB)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	if plan.SelectedTotal != 3 {
		t.Fatalf("fall-through violated: selected %d, want 3 (quota filled by remaining sources)", plan.SelectedTotal)
	}
	var shortfall *pmtaPlanWarning
	for i := range plan.PlanWarnings {
		if plan.PlanWarnings[i].Code == planWarnReserveShortfall {
			shortfall = &plan.PlanWarnings[i]
		}
	}
	if shortfall == nil {
		t.Fatalf("reserve shortfall must record a structured plan warning (never silently plans short); warnings=%+v", plan.PlanWarnings)
	}
	if shortfall.SegmentID != rsvSegB {
		t.Errorf("shortfall warning must NAME the segment: got %q, want %q", shortfall.SegmentID, rsvSegB)
	}
	if shortfall.Scope != "inclusion" {
		t.Errorf("shortfall scope = %q, want inclusion", shortfall.Scope)
	}
}

// ---------------------------------------------------------------------------
// 4. Normalize validation — malformed reservations rejected at the door
// ---------------------------------------------------------------------------

func TestSegmentReserves_NormalizeValidation(t *testing.T) {
	base := engine.PMTACampaignInput{
		TargetISPs: []engine.ISP{engine.ISPGmail},
	}

	bad := base
	bad.SegmentReserves = []engine.SegmentReserve{{SegmentID: rsvSegB, Reserve: 0}}
	if _, err := normalizePMTACampaignInput(bad); err == nil {
		t.Fatal("reserve=0 must be rejected (volume-0 ambiguity is exactly the banned class)")
	} else if got := err.Error(); !containsAll(got, rsvSegB, "reserve must be > 0") {
		t.Errorf("error must name the segment and rule, got: %v", got)
	}

	bad2 := base
	bad2.SegmentReserves = []engine.SegmentReserve{{SegmentID: "  ", Reserve: 100}}
	if _, err := normalizePMTACampaignInput(bad2); err == nil {
		t.Fatal("empty segment_id must be rejected")
	}

	ok := base
	ok.SegmentReserves = []engine.SegmentReserve{{SegmentID: rsvSegB, Reserve: 100}}
	if _, err := normalizePMTACampaignInput(ok); err != nil {
		t.Fatalf("valid reserve rejected: %v", err)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
