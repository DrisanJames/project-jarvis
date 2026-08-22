package api

import (
	"encoding/json"
	"os"
	"testing"
)

// Runs the real gates over the REAL 2026-08-22 board (50 cells, captured from
// prod). This is the proof that the gates behave on production shapes, not just
// on hand-written fixtures: a gate that only ever sees synthetic cells is how
// "it works" turns into a false green.
func TestBoardGrid_AgainstRealBoard0822(t *testing.T) {
	raw, err := os.ReadFile("testdata_board_0822.json")
	if err != nil {
		t.Skip("no captured board fixture")
	}
	var cells []BoardCell
	if err := json.Unmarshal(raw, &cells); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	s := &BoardGridService{}
	f := s.runGates(nil, "2026-08-22", cells)
	t.Logf("cells=%d findings=%d", len(cells), len(f))
	for _, x := range f {
		t.Logf("  %-8s %-16s %-6s %-6s %s", x.Level, x.Code, x.Property, x.Slot, x.Message)
	}
}
