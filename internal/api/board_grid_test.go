package api

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func cell(prop, slot, offerID, offerName, name, subj string) BoardCell {
	return BoardCell{Property: prop, Slot: slot, OfferID: offerID, OfferName: offerName,
		Name: name, Subject: subj, SendingDomain: "m." + strings.ToLower(prop) + ".com"}
}

func findingCodes(f []BoardFinding) map[string]int {
	m := map[string]int{}
	for _, x := range f {
		m[x.Code]++
	}
	return m
}

// A grid that mirrors a correct day must produce ZERO findings. Without this the
// gates could pass by flagging everything.
func TestBoardGrid_CleanBoardHasNoFindings(t *testing.T) {
	s := &BoardGridService{}
	cells := []BoardCell{
		cell("DB", "01:01", "o1", "Blinds", "08232026 - DB - Blinds", "Save on blinds"),
		cell("DB", "06:01", "o2", "Sams", "08232026 - DB - Sams", "Your membership"),
		cell("DB", "11:01", "o3", "ADT", "08232026 - DB - ADT", "Home security"),
		cell("CI", "01:01", "o2", "Sams", "08232026 - CI - Sams", "Your membership"),
	}
	got := s.runGates(nil, "2026-08-23", cells)
	if len(got) != 0 {
		t.Fatalf("clean board produced findings: %+v", got)
	}
}

func TestBoardGrid_SlotCollision(t *testing.T) {
	s := &BoardGridService{}
	cells := []BoardCell{
		cell("WF", "11:01", "o1", "CHW", "08232026 - WF - CHW", "s"),
		cell("WF", "11:01", "o2", "Sams", "08232026 - WF - Sams", "s"),
	}
	got := findingCodes(s.runGates(nil, "2026-08-23", cells))
	if got["SLOT_COLLISION"] != 1 {
		t.Fatalf("want 1 SLOT_COLLISION, got %v", got)
	}
}

// The operator rule when the third slot was added: no repeated offer on one
// sending domain.
func TestBoardGrid_RepeatOfferOnOneProperty(t *testing.T) {
	s := &BoardGridService{}
	cells := []BoardCell{
		cell("MR", "01:01", "o1", "Sams", "08232026 - MR - Sams", "s"),
		cell("MR", "11:01", "o1", "Sams", "08232026 - MR - Sams", "s"),
	}
	got := findingCodes(s.runGates(nil, "2026-08-23", cells))
	if got["REPEAT_OFFER"] != 1 {
		t.Fatalf("want 1 REPEAT_OFFER, got %v", got)
	}
	// The same offer on DIFFERENT properties is normal and must not flag.
	ok := []BoardCell{
		cell("MR", "01:01", "o1", "Sams", "08232026 - MR - Sams", "s"),
		cell("CI", "01:01", "o1", "Sams", "08232026 - CI - Sams", "s"),
	}
	if c := findingCodes(s.runGates(nil, "2026-08-23", ok)); c["REPEAT_OFFER"] != 0 {
		t.Fatalf("same offer on two properties must not flag: %v", c)
	}
}

func TestBoardGrid_MissingOfferAndLiquidSubject(t *testing.T) {
	s := &BoardGridService{}
	cells := []BoardCell{
		cell("RR", "01:01", "", "", "08232026 - RR - Globe", "Globe Life"),
		cell("RB", "06:01", "o9", "HELOC", "08232026 - RB - HELOC",
			"You could tap {{custom.equity_estimate}} today"),
	}
	got := findingCodes(s.runGates(nil, "2026-08-23", cells))
	if got["MISSING_OFFER"] != 1 {
		t.Fatalf("want MISSING_OFFER, got %v", got)
	}
	if got["LIQUID_SUBJECT"] != 1 {
		t.Fatalf("want LIQUID_SUBJECT, got %v", got)
	}
}

func TestBoardGrid_NameGates(t *testing.T) {
	s := &BoardGridService{}
	cells := []BoardCell{
		cell("MH", "01:01", "o1", "Globe", "08232026 - MF - Globe", "s"), // wrong code
		cell("DB", "06:01", "o2", "Sams", "08062026 - DB - Sams", "s"),   // wrong date
	}
	got := findingCodes(s.runGates(nil, "2026-08-23", cells))
	if got["NAME_PROPERTY"] != 1 {
		t.Fatalf("want NAME_PROPERTY, got %v", got)
	}
	if got["NAME_DATE"] != 1 {
		t.Fatalf("want NAME_DATE, got %v", got)
	}
}

// Substring matching would pass 'CURRENT' as naming property RR. Token matching
// must not.
func TestBoardGrid_PropertyTokenNotSubstring(t *testing.T) {
	if nameMentionsProperty("08232026 - CURRENT - Sams", "RR") {
		t.Fatal("'CURRENT' must not satisfy property RR")
	}
	if nameMentionsProperty("08232026 - MRD - Sams", "MR") {
		t.Fatal("'MRD' must not satisfy property MR")
	}
	if !nameMentionsProperty("08232026 - MR - Sams", "MR") {
		t.Fatal("'MR' as its own token must satisfy property MR")
	}
}

func TestBoardGrid_ApexFromSendingDomain(t *testing.T) {
	for in, want := range map[string]string{
		"m.discountblog.com":  "DISCOUNTBLOG",
		"em.discountblog.com": "DISCOUNTBLOG",
		"":                    "(unmapped)",
	} {
		if got := apexFromSendingDomain(in); got != want {
			t.Fatalf("apexFromSendingDomain(%q)=%q want %q", in, got, want)
		}
	}
}

// The clone must renumber the date token so a cloned board does not inherit
// yesterday's name — the defect that shipped '08062026' on an 08/22 board.
func TestBoardGrid_CloneRewritesDateToken(t *testing.T) {
	got := reDateToken.ReplaceAllString("08222026 - DB - Sams", "08232026")
	if got != "08232026 - DB - Sams" {
		t.Fatalf("clone rename produced %q", got)
	}
}

// This file must never grow a send path. Cloning returns a proposal; deploying
// belongs to /pmta-campaign/deploy, which owns audience planning and the wave
// sanity check.
func TestBoardGrid_ReadOnly(t *testing.T) {
	src, err := os.ReadFile("board_grid.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	// Strip comments so the doc block describing the forbidden tables does not
	// trip the assertion.
	body := regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(string(src), "")
	up := strings.ToUpper(body)
	// Whole-statement forms only. A substring check is wrong here: the status
	// predicate legitimately contains 'deleted', which contains "DELETE".
	for _, bad := range []string{
		"INSERT INTO", "UPDATE ", "DELETE FROM", "TRUNCATE", "DROP ",
		"MAILING_CAMPAIGN_QUEUE", "PARTNER_CLEAN_QUEUE",
	} {
		if strings.Contains(up, bad) {
			t.Fatalf("board_grid.go must stay read-only, found %q", bad)
		}
	}
}

// The two code systems are both correct. A board naming BWP / HWS / LPL / TOT /
// YIH must NOT warn just because mailing_brand_metadata stores BW / HW / LP /
// TT / YI — that false-positived on 15 of 50 cells of a correct board.
func TestBoardGrid_AcceptsBothCodeSystems(t *testing.T) {
	ok := []struct{ name, code, root string }{
		{"08222026 - BWP - Sams", "BW", "businessweeklypro.com"},
		{"08222026 - HWS - ADT", "HW", "homewarrantyservices.org"},
		{"08222026 - LPL - Optima", "LP", "learnpersonalloans.com"},
		{"08222026 - TOT - Globe", "TT", "thingoftheday.org"},
		{"08222026 - YIH - Liberty", "YI", "yourinsurancehub.com"},
		{"08222026 - DB - Sams", "DB", "discountblog.com"},
	}
	for _, c := range ok {
		if !nameMentionsPropertyRoot(c.name, c.code, c.root) {
			t.Errorf("false warning: %q should satisfy %s (%s)", c.name, c.code, c.root)
		}
	}
	// It must still reject what actually shipped broken.
	bad := []struct{ name, code, root string }{
		{"08232026 - MF - Globe", "MH", "myownhealth.net"}, // typo'd code
		{"08222026 - DB - Sams", "CP", "consumerpro.net"},  // wrong property
	}
	for _, c := range bad {
		if nameMentionsPropertyRoot(c.name, c.code, c.root) {
			t.Errorf("missed defect: %q must NOT satisfy %s (%s)", c.name, c.code, c.root)
		}
	}
}
