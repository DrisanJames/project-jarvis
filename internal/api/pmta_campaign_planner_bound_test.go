package api

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/ignite/sparkpost-monitor/internal/engine"
)

func segItems(ids ...string) []engine.PriorityItem {
	out := make([]engine.PriorityItem, 0, len(ids))
	for _, id := range ids {
		out = append(out, engine.PriorityItem{ID: id, Type: "segment"})
	}
	return out
}

// 2026-11-10 S1101: a board payload narrowed to one static segment kept the
// source's 5-segment send_priority and finalized to the whole engaged tier.
func TestBoundPriorityToInclusion_NarrowedCellMailsOnlyItsSegment(t *testing.T) {
	in := engine.PMTACampaignInput{
		AudienceBoundToInclusion: true,
		InclusionSegments:        []string{"static"},
		SendPriority:             segItems("clk30", "opn7", "clk7", "opn30", "eng"),
		SegmentReserves:          []engine.SegmentReserve{{SegmentID: "clk30", Reserve: 100}},
	}
	if got := boundPriorityToInclusion(&in); got != 6 {
		t.Fatalf("dropped = %d, want 6", got)
	}
	if want := segItems("static"); !reflect.DeepEqual(in.SendPriority, want) {
		t.Fatalf("send_priority = %+v, want %+v", in.SendPriority, want)
	}
	if len(in.SegmentReserves) != 0 {
		t.Fatalf("segment_reserves = %+v, want none", in.SegmentReserves)
	}
}

func TestBoundPriorityToInclusion_NarrowedToMemberOfPriority(t *testing.T) {
	in := engine.PMTACampaignInput{
		AudienceBoundToInclusion: true,
		InclusionSegments:        []string{"clk30"},
		SendPriority:             append(segItems("opn7", "clk30", "eng"), engine.PriorityItem{ID: "L", Type: "list"}),
	}
	if got := boundPriorityToInclusion(&in); got != 2 {
		t.Fatalf("dropped = %d, want 2", got)
	}
	want := []engine.PriorityItem{{ID: "clk30", Type: "segment"}, {ID: "L", Type: "list"}}
	if !reflect.DeepEqual(in.SendPriority, want) {
		t.Fatalf("send_priority = %+v, want %+v", in.SendPriority, want)
	}
}

// Board cells: send_priority == inclusion (any order) -> byte-identical.
func TestBoundPriorityToInclusion_BoardCellUntouched(t *testing.T) {
	in := engine.PMTACampaignInput{
		AudienceBoundToInclusion: true,
		InclusionSegments:        []string{"opn7", "clk30", "eng"},
		SendPriority:             segItems("clk30", "opn7", "eng"),
		SegmentReserves:          []engine.SegmentReserve{{SegmentID: "clk30", Reserve: 50}},
	}
	before := in
	before.SendPriority = append([]engine.PriorityItem(nil), in.SendPriority...)
	before.SegmentReserves = append([]engine.SegmentReserve(nil), in.SegmentReserves...)
	if got := boundPriorityToInclusion(&in); got != 0 {
		t.Fatalf("dropped = %d, want 0", got)
	}
	if !reflect.DeepEqual(in.SendPriority, before.SendPriority) || !reflect.DeepEqual(in.SegmentReserves, before.SegmentReserves) {
		t.Fatalf("board cell mutated: %+v / %+v", in.SendPriority, in.SegmentReserves)
	}
}

// Priority a strict subset of inclusion: untouched (no append — prior behaviour kept).
func TestBoundPriorityToInclusion_SubsetPriorityUntouched(t *testing.T) {
	in := engine.PMTACampaignInput{
		AudienceBoundToInclusion: true,
		InclusionSegments:        []string{"a", "b"},
		SendPriority:             segItems("a"),
	}
	if got := boundPriorityToInclusion(&in); got != 0 || !reflect.DeepEqual(in.SendPriority, segItems("a")) {
		t.Fatalf("dropped=%d sp=%+v", got, in.SendPriority)
	}
}

// No inclusion segments (list- or priority-only payloads): untouched.
func TestBoundPriorityToInclusion_NoInclusionUntouched(t *testing.T) {
	in := engine.PMTACampaignInput{AudienceBoundToInclusion: true, SendPriority: segItems("x", "y")}
	if got := boundPriorityToInclusion(&in); got != 0 || len(in.SendPriority) != 2 {
		t.Fatalf("dropped=%d sp=%+v", got, in.SendPriority)
	}
}

// Default (flag unset): the board shape — send_priority carrying segments
// beyond inclusion ("30D/60D Openers") — is preserved exactly.
func TestBoundPriorityToInclusion_FlagUnsetBoardExpansionPreserved(t *testing.T) {
	in := engine.PMTACampaignInput{
		InclusionSegments: []string{"clk30", "opn7"},
		SendPriority:      segItems("clk30", "opn7", "opn30", "opn60"),
		SegmentReserves:   []engine.SegmentReserve{{SegmentID: "opn30", Reserve: 10}},
	}
	if got := boundPriorityToInclusion(&in); got != 0 {
		t.Fatalf("dropped = %d, want 0", got)
	}
	if !reflect.DeepEqual(in.SendPriority, segItems("clk30", "opn7", "opn30", "opn60")) || len(in.SegmentReserves) != 1 {
		t.Fatalf("board expansion changed: %+v / %+v", in.SendPriority, in.SegmentReserves)
	}
}

// End-to-end through planPMTAAudience (segment-bound path, master selection
// off): the S1101 shape. With the flag the planner reads ONLY the listed
// inclusion segment; without it, it reads the send_priority segments (the
// board behaviour, pinned so a later change to the default is deliberate).
func planBoundFixture(t *testing.T, bound bool, expectSegs []string) pmtaAudiencePlan {
	t.Helper()
	t.Setenv("RECENCY_AUDIENCE_DRAW_DISABLED", "true")
	t.Setenv("DISABLE_ROTATING_AUDIENCE_SELECTION", "true")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	campaignID := "bbbbbbbb-0000-0000-0000-00000000b001"
	ring1 := "cccccccc-0000-0000-0000-0000000000r1"
	ring2 := "cccccccc-0000-0000-0000-0000000000r2"
	static := "cccccccc-0000-0000-0000-0000000000s1"
	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).WithArgs(campaignID).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(false))
	rows := map[string][2]string{
		ring1:  {"11111111-1111-1111-1111-1111111111a1", "ring1@gmail.com"},
		ring2:  {"11111111-1111-1111-1111-1111111111a2", "ring2@gmail.com"},
		static: {"11111111-1111-1111-1111-1111111111b1", "static@gmail.com"},
	}
	for _, s := range expectSegs {
		mock.ExpectQuery(segmentMembersQuery).WithArgs(s).
			WillReturnRows(sqlmock.NewRows([]string{"subscriber_id", "email"}).AddRow(rows[s][0], rows[s][1]))
	}
	input := engine.PMTACampaignInput{
		CampaignID:               campaignID,
		SendingDomain:            "m.bound-test.com",
		InclusionSegments:        []string{static},
		SendPriority:             segItems(ring1, ring2),
		AudienceBoundToInclusion: bound,
		ISPPlans:                 []engine.PMTAISPScheduleInput{{ISP: "gmail", Quota: 10}},
	}
	normalized := pmtaNormalizedCampaign{Plans: []pmtaNormalizedPlan{{ISP: "gmail", Quota: 10}}}
	res, err := planPMTAAudience(context.Background(), db, "00000000-0000-0000-0000-000000000001", input, normalized, NewSuppressionMatcher(), nil)
	if err != nil {
		t.Fatalf("planPMTAAudience: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
	return res
}

func TestPlanPMTAAudience_BoundToInclusion_ReadsOnlyInclusionSegment(t *testing.T) {
	res := planBoundFixture(t, true, []string{"cccccccc-0000-0000-0000-0000000000s1"})
	if res.CountsByISP["gmail"] != 1 {
		t.Fatalf("gmail = %d, want 1 (the static segment's one member only)", res.CountsByISP["gmail"])
	}
}

func TestPlanPMTAAudience_DefaultReadsSendPriority(t *testing.T) {
	res := planBoundFixture(t, false, []string{"cccccccc-0000-0000-0000-0000000000r1", "cccccccc-0000-0000-0000-0000000000r2"})
	if res.CountsByISP["gmail"] != 2 {
		t.Fatalf("gmail = %d, want 2 (send_priority drives the default path)", res.CountsByISP["gmail"])
	}
}
