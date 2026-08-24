package api

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// The REAL 2026-08-24 board — the one the operator was fighting, captured from
// prod after the audience finalizer had settled every row.
//
// On the OLD gates this board reported 11 blockers + 9 warnings and every one
// of them was an artifact:
//   - 6 MISSING_OFFER      : read during finalization, before offer_id lands
//   - 5 LIQUID_SUBJECT     : `{{ first_name | default: "Homeowner" }}`, the ONE
//     idiom verified safe (missing key / "" / set)
//   - 5 NAME_PROPERTY      : sending_profile_id still NULL, so no property to
//   - 2 UNMAPPED_COLLISION  name and every in-flight cell collapsed into one
//     '(unmapped)' row
//
// The board itself was correct. This test pins that the fixed gates say so.
func TestBoardGrid_AgainstRealBoard0824(t *testing.T) {
	raw, err := os.ReadFile("testdata_board_0824.json")
	if err != nil {
		t.Skip("no captured board fixture")
	}
	var cells []BoardCell
	if err := json.Unmarshal(raw, &cells); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if len(cells) != 11 {
		t.Fatalf("fixture drifted: want 11 cells, got %d", len(cells))
	}
	s := &BoardGridService{}
	got := make([]string, 0, 8)
	for _, x := range s.runGates(nil, "2026-08-24", cells) {
		got = append(got, fmt.Sprintf("%s|%s|%s|%s", x.Level, x.Code, x.Property, x.Slot))
	}
	// CLEAN. The two KUMO-WARM cells carry no offer BY DOCTRINE (offers are
	// banned in warm-up editorial, CLAUDE.md §13.1) and their 'aug24' tokens
	// satisfy NAME_DATE. WCL now joins its own metadata row (m.wcl-heloc.com),
	// so it names its property instead of warning.
	if len(got) != 0 {
		t.Fatalf("the real 08/24 board must gate CLEAN, got %d findings: %v", len(got), got)
	}
}

// Same board, rewound to the moment the operator actually looked at it: the
// audience finalizer had not written sending_profile_id or offer_id yet. The
// gates must degrade to "not yet", never to blockers.
func TestBoardGrid_RealBoard0824_MidFinalizationIsNotBlocked(t *testing.T) {
	raw, err := os.ReadFile("testdata_board_0824.json")
	if err != nil {
		t.Skip("no captured board fixture")
	}
	var cells []BoardCell
	if err := json.Unmarshal(raw, &cells); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	for i := range cells {
		cells[i].OfferID, cells[i].OfferName = "", ""
		cells[i].Status, cells[i].PendingFinalize = "finalizing_audience", true
	}
	blockers := 0
	pending := 0
	for _, x := range (&BoardGridService{}).runGates(nil, "2026-08-24", cells) {
		if x.Level == "blocker" {
			blockers++
		}
		if x.Code == "OFFER_PENDING" {
			pending++
		}
	}
	if blockers != 0 {
		t.Fatalf("a mid-finalization board must produce ZERO blockers, got %d", blockers)
	}
	// 9 of 11 — the two KUMO-WARM cells are offer-exempt.
	if pending != 9 {
		t.Fatalf("want 9 OFFER_PENDING warns, got %d", pending)
	}
}
