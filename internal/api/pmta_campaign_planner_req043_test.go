package api

// REQ-043 — the planner no longer SILENTLY skips 0-member inclusion and
// exclusion segments.
//
// Contract pinned here (all four DoD criteria):
//
//  1. A 0-member EXCLUSION segment with no verified successful build
//     (mailing_segment_build_ledger: no row, last_built_at NULL, last build
//     failed/blocked/stale-running, or ledger-vs-members count mismatch)
//     FAILS the plan — matching the REQ-002 exclusion fail-closed idiom —
//     because an unbuilt exclusion silently WIDENS the audience. A segment
//     that was built and is GENUINELY empty (last_built_at set, status ok,
//     subscriber_count 0) still plans and records a structured warning.
//  2. A 0-member INCLUSION segment (either ledger state) records a
//     structured per-segment warning carried on pmtaAudiencePlan.PlanWarnings
//     and persisted to mailing_campaign_audience_funnel.plan_warnings —
//     never only a server log line.
//  3. streamSegment issues ONE members read (no separate COUNT racing it) —
//     pinned by the sqlmock expectations: a reintroduced COUNT would fail
//     the ordered expectation set.
//  4. Kill switch: DISABLE_SEGMENT_BUILD_FAIL_CLOSED=true neutralizes the
//     fail-closed enforcement (warn-and-skip, warning still recorded).
//
// Run: go test ./internal/api/ -run 'ExclusionSegmentFailClosed|InclusionSegmentZeroMembers|PersistAudienceFunnel_PlanWarnings|SegmentBuildLedgerState' -v

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

const (
	r43OrgID     = "00000000-0000-0000-0000-000000000001"
	r43ListID    = "aaaaaaaa-0000-0000-0000-000000000043"
	r43SegmentID = "cccccccc-0000-0000-0000-000000000043"
)

// r43LedgerCols matches segmentBuildLedgerState's SELECT column order.
func r43LedgerCols() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"last_built_at", "last_build_status", "updated_at", "subscriber_count"})
}

func r43ExpectEmptyExclusionRead(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT email FROM mailing_segment_members WHERE segment_id = \$1`).
		WithArgs(r43SegmentID).
		WillReturnRows(sqlmock.NewRows([]string{"email"}))
}

func r43PlanWithExclusionSegment(t *testing.T, db *sql.DB) (pmtaAudiencePlan, error) {
	t.Helper()
	input := engine.PMTACampaignInput{
		InclusionLists:    []string{r43ListID},
		ExclusionSegments: []string{r43SegmentID},
	}
	return planPMTAAudience(context.Background(), db, r43OrgID, input, pmtaNormalizedCampaign{}, NewSuppressionMatcher(), nil)
}

// ---------------------------------------------------------------------------
// DoD-1, fail branch: never built / failed build / stale running / wiped.
// ---------------------------------------------------------------------------

func TestExclusionSegmentFailClosed_NeverBuilt(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()

	r43ExpectEmptyExclusionRead(mock)
	mock.ExpectQuery(`FROM mailing_segment_build_ledger`).
		WithArgs(r43SegmentID).
		WillReturnError(sql.ErrNoRows) // no ledger row at all

	_, planErr := r43PlanWithExclusionSegment(t, db)
	if planErr == nil {
		t.Fatal("planPMTAAudience must FAIL when a 0-member exclusion segment was never built — got nil error (fail-open: audience silently widened)")
	}
	if !strings.Contains(planErr.Error(), r43SegmentID) || !strings.Contains(planErr.Error(), "fail-closed") {
		t.Errorf("plan error must name the segment and the fail-closed reason, got: %v", planErr)
	}
	if !strings.Contains(planErr.Error(), "never built") {
		t.Errorf("plan error must classify the ledger state (never built), got: %v", planErr)
	}
	if !strings.Contains(planErr.Error(), "DISABLE_SEGMENT_BUILD_FAIL_CLOSED") {
		t.Errorf("plan error must state the kill-switch override, got: %v", planErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestExclusionSegmentFailClosed_FailedBuild(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()

	r43ExpectEmptyExclusionRead(mock)
	// Ledger row exists but there was never a SUCCESSFUL build:
	// last_built_at NULL, most recent attempt failed.
	mock.ExpectQuery(`FROM mailing_segment_build_ledger`).
		WithArgs(r43SegmentID).
		WillReturnRows(r43LedgerCols().AddRow(nil, "failed", time.Now(), 0))

	_, planErr := r43PlanWithExclusionSegment(t, db)
	if planErr == nil {
		t.Fatal("planPMTAAudience must FAIL when the exclusion segment's only build attempts failed — got nil error")
	}
	if !strings.Contains(planErr.Error(), "never successfully built") || !strings.Contains(planErr.Error(), "failed") {
		t.Errorf("plan error must carry the failed-build ledger detail, got: %v", planErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestExclusionSegmentFailClosed_StaleRunningBuild(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()

	r43ExpectEmptyExclusionRead(mock)
	// A 'running' row whose updated_at exceeds ledgerRunningStaleAfter is a
	// crashed build (segment_ledger.go coerceLedgerStatus) — NOT a verified
	// empty build, even though a prior good build exists.
	mock.ExpectQuery(`FROM mailing_segment_build_ledger`).
		WithArgs(r43SegmentID).
		WillReturnRows(r43LedgerCols().AddRow(
			time.Now().Add(-48*time.Hour), "running", time.Now().Add(-ledgerRunningStaleAfter-5*time.Minute), 0))

	_, planErr := r43PlanWithExclusionSegment(t, db)
	if planErr == nil {
		t.Fatal("planPMTAAudience must FAIL when the exclusion segment's last build is a stale 'running' (crashed) row — got nil error")
	}
	if !strings.Contains(planErr.Error(), "status=failed") {
		t.Errorf("stale running must be coerced to failed in the detail, got: %v", planErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestExclusionSegmentFailClosed_LedgerCountMismatch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()

	r43ExpectEmptyExclusionRead(mock)
	// Last good build recorded 42 members but 0 are readable now — the
	// membership was wiped after the build (cleanup/delete race). That is
	// NOT "built and genuinely empty"; planning would widen the audience.
	mock.ExpectQuery(`FROM mailing_segment_build_ledger`).
		WithArgs(r43SegmentID).
		WillReturnRows(r43LedgerCols().AddRow(time.Now().Add(-2*time.Hour), "ok", time.Now().Add(-2*time.Hour), 42))

	_, planErr := r43PlanWithExclusionSegment(t, db)
	if planErr == nil {
		t.Fatal("planPMTAAudience must FAIL when the ledger says the last good build had members but 0 are readable — got nil error")
	}
	if !strings.Contains(planErr.Error(), "42") {
		t.Errorf("plan error must carry the ledger count for triage, got: %v", planErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// DoD-1, warn branch: built and genuinely empty still plans + warns.
// ---------------------------------------------------------------------------

func TestExclusionSegmentFailClosed_BuiltEmptyStillPlans(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()

	r43ExpectEmptyExclusionRead(mock)
	// Verified empty: successful build, ok status, 0 members recorded.
	mock.ExpectQuery(`FROM mailing_segment_build_ledger`).
		WithArgs(r43SegmentID).
		WillReturnRows(r43LedgerCols().AddRow(time.Now().Add(-1*time.Hour), "ok", time.Now().Add(-1*time.Hour), 0))

	subscriberRows := sqlmock.NewRows([]string{"id", "email"})
	for i := 0; i < 10; i++ {
		subscriberRows.AddRow(
			fmt.Sprintf("11111111-0000-0000-0000-%012d", i),
			fmt.Sprintf("user%d@gmail.com", i),
		)
	}
	mock.ExpectQuery("SELECT s.id::text, s.email").
		WithArgs(r43ListID).
		WillReturnRows(subscriberRows)

	input := engine.PMTACampaignInput{
		InclusionLists:    []string{r43ListID},
		ExclusionSegments: []string{r43SegmentID},
		ISPPlans:          []engine.PMTAISPScheduleInput{{ISP: "gmail", Quota: 5}},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{{ISP: "gmail", Quota: 5}},
	}

	result, planErr := planPMTAAudience(context.Background(), db, r43OrgID, input, normalized, NewSuppressionMatcher(), nil)
	if planErr != nil {
		t.Fatalf("a BUILT-AND-EMPTY exclusion segment must not fail the plan (it legitimately excludes nobody): %v", planErr)
	}
	if result.CountsByISP["gmail"] != 5 {
		t.Errorf("gmail count = %d, want 5", result.CountsByISP["gmail"])
	}
	if len(result.PlanWarnings) != 1 {
		t.Fatalf("PlanWarnings = %+v, want exactly 1 built-empty exclusion warning", result.PlanWarnings)
	}
	w := result.PlanWarnings[0]
	if w.Code != planWarnSegmentZeroMembers || w.Scope != "exclusion" || w.SegmentID != r43SegmentID {
		t.Errorf("warning = %+v, want code=%s scope=exclusion segment_id=%s", w, planWarnSegmentZeroMembers, r43SegmentID)
	}
	if !strings.Contains(w.Detail, "genuinely empty") {
		t.Errorf("warning detail must classify the verified-empty build, got %q", w.Detail)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// DoD-4: the kill switch neutralizes fail-closed WITHOUT a redeploy.
// ---------------------------------------------------------------------------

func TestExclusionSegmentFailClosed_KillSwitchRevertsToWarnAndSkip(t *testing.T) {
	t.Setenv("DISABLE_SEGMENT_BUILD_FAIL_CLOSED", "true")

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()

	r43ExpectEmptyExclusionRead(mock)
	mock.ExpectQuery(`FROM mailing_segment_build_ledger`).
		WithArgs(r43SegmentID).
		WillReturnError(sql.ErrNoRows) // never built — would fail closed without the switch

	subscriberRows := sqlmock.NewRows([]string{"id", "email"}).
		AddRow("11111111-0000-0000-0000-000000000001", "user1@gmail.com")
	mock.ExpectQuery("SELECT s.id::text, s.email").
		WithArgs(r43ListID).
		WillReturnRows(subscriberRows)

	input := engine.PMTACampaignInput{
		InclusionLists:    []string{r43ListID},
		ExclusionSegments: []string{r43SegmentID},
		ISPPlans:          []engine.PMTAISPScheduleInput{{ISP: "gmail", Quota: 1}},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{{ISP: "gmail", Quota: 1}},
	}

	result, planErr := planPMTAAudience(context.Background(), db, r43OrgID, input, normalized, NewSuppressionMatcher(), nil)
	if planErr != nil {
		t.Fatalf("with DISABLE_SEGMENT_BUILD_FAIL_CLOSED=true a never-built exclusion segment must revert to warn-and-skip: %v", planErr)
	}
	// The structured warning is still recorded — only enforcement is off.
	if len(result.PlanWarnings) != 1 || result.PlanWarnings[0].Scope != "exclusion" {
		t.Fatalf("kill switch must not suppress the structured warning, got %+v", result.PlanWarnings)
	}
	if !strings.Contains(result.PlanWarnings[0].Detail, "never built") {
		t.Errorf("warning detail must still classify never-built, got %q", result.PlanWarnings[0].Detail)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// DoD-2 + DoD-3 + re-run safety: a 0-member INCLUSION segment reachable via
// BOTH send_priority and inclusion_segments (streamSegment fires twice) still
// plans, issues exactly ONE members read per invocation (no COUNT), and the
// warning is deduplicated to a single structured entry.
// ---------------------------------------------------------------------------

func TestInclusionSegmentZeroMembers_WarnsOnceOnDoubleStream(t *testing.T) {
	t.Setenv("RECENCY_AUDIENCE_DRAW_DISABLED", "true") // pin the pre-recency path this test mocks
	t.Setenv("DISABLE_ROTATING_AUDIENCE_SELECTION", "true")
	t.Setenv("DISABLE_RESERVE_POOL", "true")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	campaignID := "bbbbbbbb-0000-0000-0000-000000000043"
	segmentID := r43SegmentID
	sendingDomain := "em.req043.com"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(true))

	// send_priority pass: one members read, 0 rows, ledger consult.
	mock.ExpectQuery(segmentMembersQuery).
		WithArgs(segmentID).
		WillReturnRows(sqlmock.NewRows([]string{"subscriber_id", "email"}))
	mock.ExpectQuery(segmentLedgerQuery).
		WithArgs(segmentID).
		WillReturnError(sql.ErrNoRows)

	// inclusion_segments pass over the SAME segment: read + ledger again
	// (fresh snapshot each invocation), but the warning must dedupe.
	mock.ExpectQuery(segmentMembersQuery).
		WithArgs(segmentID).
		WillReturnRows(sqlmock.NewRows([]string{"subscriber_id", "email"}))
	mock.ExpectQuery(segmentLedgerQuery).
		WithArgs(segmentID).
		WillReturnError(sql.ErrNoRows)

	// SDS primary pass fills the quota.
	mock.ExpectQuery(`FROM mailing_subscribers sub\s+JOIN mailing_subscriber_domain_state sds`).
		WithArgs(sendingDomain, r43OrgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}).
			AddRow("55555555-0000-0000-0000-000000000001", "sds1@gmail.com").
			AddRow("55555555-0000-0000-0000-000000000002", "sds2@gmail.com"))

	input := engine.PMTACampaignInput{
		CampaignID:        campaignID,
		SendingDomain:     sendingDomain,
		SendPriority:      []engine.PriorityItem{{Type: "segment", ID: segmentID}},
		InclusionSegments: []string{segmentID},
		ISPPlans:          []engine.PMTAISPScheduleInput{{ISP: "gmail", Quota: 2}},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{{ISP: "gmail", Quota: 2}},
	}

	result, planErr := planPMTAAudience(context.Background(), db, r43OrgID, input, normalized, NewSuppressionMatcher(), nil)
	if planErr != nil {
		t.Fatalf("a 0-member INCLUSION segment must warn, not fail: %v", planErr)
	}
	if got := result.CountsByISP["gmail"]; got != 2 {
		t.Errorf("gmail count = %d, want 2 (SDS supplied)", got)
	}
	if len(result.PlanWarnings) != 1 {
		t.Fatalf("PlanWarnings must dedupe the double-streamed segment to ONE entry, got %+v", result.PlanWarnings)
	}
	w := result.PlanWarnings[0]
	if w.Code != planWarnSegmentZeroMembers || w.Scope != "inclusion" || w.SegmentID != segmentID {
		t.Errorf("warning = %+v, want code=%s scope=inclusion segment_id=%s", w, planWarnSegmentZeroMembers, segmentID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// DoD-2 persistence: the warning lands on the campaign's plan record
// (mailing_campaign_audience_funnel.plan_warnings) via an idempotent upsert —
// a re-fired finalization REPLACES the set, never duplicates it.
// ---------------------------------------------------------------------------

func TestPersistAudienceFunnel_PlanWarnings(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	campaignID := "bbbbbbbb-0000-0000-0000-00000000f043"
	audience := pmtaAudiencePlan{
		TotalSeen:          10,
		SelectedTotal:      8,
		ReserveTotal:       0,
		SuppressionReasons: map[string]int{"dedup_or_empty": 2},
		PlanWarnings: []pmtaPlanWarning{{
			Code:      planWarnSegmentZeroMembers,
			Scope:     "inclusion",
			SegmentID: r43SegmentID,
			Detail:    "never built (no build-ledger row)",
		}},
	}
	wantWarnJSON := `[{"code":"segment_zero_members","scope":"inclusion","segment_id":"` +
		r43SegmentID + `","detail":"never built (no build-ledger row)"}]`

	// Twice: the scheduler re-fire / stale-preparing recovery re-finalizes a
	// campaign and persists again. The statement is a single-row
	// ON CONFLICT (campaign_id) DO UPDATE upsert, so the second write
	// replaces plan_warnings instead of appending — pinned by asserting the
	// identical upsert fires both times.
	for i := 0; i < 2; i++ {
		mock.ExpectExec(`INSERT INTO mailing_campaign_audience_funnel[\s\S]*ON CONFLICT \(campaign_id\) DO UPDATE[\s\S]*plan_warnings = EXCLUDED\.plan_warnings`).
			WithArgs(campaignID, r43OrgID, 10, 8, 0, 2, `{"dedup_or_empty":2}`, wantWarnJSON).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	persistAudienceFunnel(context.Background(), db, campaignID, r43OrgID, audience)
	persistAudienceFunnel(context.Background(), db, campaignID, r43OrgID, audience)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// A plan with NO warnings must serialize plan_warnings as '[]', not 'null',
// so JSONB consumers can iterate unconditionally.
func TestPersistAudienceFunnel_NilWarningsSerializeAsEmptyArray(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	campaignID := "bbbbbbbb-0000-0000-0000-00000000f044"
	mock.ExpectExec(`INSERT INTO mailing_campaign_audience_funnel`).
		WithArgs(campaignID, r43OrgID, 0, 0, 0, 0, `{}`, `[]`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	persistAudienceFunnel(context.Background(), db, campaignID, r43OrgID, pmtaAudiencePlan{
		SuppressionReasons: map[string]int{},
	})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// segmentBuildLedgerState unit coverage (the DoD-1 discriminator itself).
// ---------------------------------------------------------------------------

func TestSegmentBuildLedgerState_BuiltEmpty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM mailing_segment_build_ledger`).
		WithArgs(r43SegmentID).
		WillReturnRows(r43LedgerCols().AddRow(time.Now().Add(-time.Hour), "ok", time.Now().Add(-time.Hour), 0))

	builtEmpty, detail := segmentBuildLedgerState(context.Background(), db, r43SegmentID)
	if !builtEmpty {
		t.Errorf("ok build with 0 members must classify builtEmpty=true, got false (%s)", detail)
	}
}

func TestSegmentBuildLedgerState_LedgerReadErrorFailsClosed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM mailing_segment_build_ledger`).
		WithArgs(r43SegmentID).
		WillReturnError(fmt.Errorf("connection reset"))

	builtEmpty, detail := segmentBuildLedgerState(context.Background(), db, r43SegmentID)
	if builtEmpty {
		t.Error("a ledger READ ERROR must never classify as built-empty (fail-closed direction)")
	}
	if !strings.Contains(detail, "ledger read error") {
		t.Errorf("detail must state the read error, got %q", detail)
	}
}
