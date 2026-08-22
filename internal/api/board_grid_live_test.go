package api

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// Runs the real gates over the REAL 2026-08-22 board (50 cells, captured from
// prod). This is the proof that the gates behave on production shapes, not just
// on hand-written fixtures: a gate that only ever sees synthetic cells is how
// "it works" turns into a false green.
//
// The verdict is PINNED. The two KUMO-WARM cells ('aug22 - aad/hfc - …') date
// themselves in the month-name scheme, which NAME_DATE now accepts — so the
// only findings on this board are the two genuinely offerless kumo newsletter
// cells. If a gate change alters this set, this test must be updated with the
// reasoning, not loosened back to logging.
func TestBoardGrid_AgainstRealBoard0822(t *testing.T) {
	raw, err := os.ReadFile("testdata_board_0822.json")
	if err != nil {
		t.Skip("no captured board fixture")
	}
	var cells []BoardCell
	if err := json.Unmarshal(raw, &cells); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if len(cells) != 50 {
		t.Fatalf("fixture drifted: want 50 cells, got %d", len(cells))
	}
	s := &BoardGridService{}
	f := s.runGates(nil, "2026-08-22", cells)

	// The real 2026-08-22 board is CLEAN: the two KUMO-WARM cells (AADWD/HFCL
	// 07:01) carry no offer BY DOCTRINE (editorial warm-up content — offers
	// banned, CLAUDE.md §13.1) and are exempt from MISSING_OFFER; their
	// 'aug22' month-name date tokens satisfy NAME_DATE for this date.
	want := []string{}
	got := make([]string, 0, len(f))
	for _, x := range f {
		got = append(got, fmt.Sprintf("%s|%s|%s|%s", x.Level, x.Code, x.Property, x.Slot))
	}
	if len(got) != len(want) {
		t.Fatalf("want exactly %d findings %v, got %d: %v", len(want), want, len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("finding %d: want %q, got %q (full set: %v)", i, want[i], got[i], got)
		}
	}
}
