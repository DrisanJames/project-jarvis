package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
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
	to := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	if got := rewriteNameDate("08222026 - DB - Sams", to); got != "08232026 - DB - Sams" {
		t.Fatalf("clone rename produced %q", got)
	}
}

// KUMO-WARM and FRESH-BCAST names date themselves 'aug22', not '08222026'.
// The clone must rewrite that scheme too, in the same style — otherwise a
// cloned kumo cell ships yesterday's date and NAME_DATE flags a proposal the
// clone itself produced.
func TestBoardGrid_CloneRewritesMonthNameToken(t *testing.T) {
	to := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	for in, want := range map[string]string{
		"aug22 - aad - KUMO-WARM - newsletter": "aug23 - aad - KUMO-WARM - newsletter",
		"jul28 - hfc - KUMO-WARM - newsletter": "aug23 - hfc - KUMO-WARM - newsletter",
		"AUG22 - DB - FRESH-BCAST":             "AUG23 - DB - FRESH-BCAST",
		"Aug22 - DB - FRESH-BCAST":             "Aug23 - DB - FRESH-BCAST",
		// No date token in either scheme: name passes through untouched.
		"DB - Sams": "DB - Sams",
	} {
		if got := rewriteNameDate(in, to); got != want {
			t.Errorf("rewriteNameDate(%q) = %q, want %q", in, got, want)
		}
	}
	// Single-digit day emits the unpadded style the scheme uses ('aug8').
	sep := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	if got := rewriteNameDate("aug22 - aad - KUMO-WARM", sep); got != "sep8 - aad - KUMO-WARM" {
		t.Fatalf("single-digit day rewrite produced %q", got)
	}
}

// The board day is a Denver day. `scheduled_at >= $1::date` compared in the
// session TZ, which is wrong for the hours around midnight — the bounds must
// be the Denver day's UTC instants.
func TestBoardGrid_DenverDayBounds(t *testing.T) {
	// August: MDT = UTC-6. The Denver day starts at 06:00Z.
	start, end, err := denverDayBounds("2026-08-22")
	if err != nil {
		t.Fatalf("denverDayBounds: %v", err)
	}
	if want := time.Date(2026, 8, 22, 6, 0, 0, 0, time.UTC); !start.UTC().Equal(want) {
		t.Fatalf("start = %s, want %s", start.UTC(), want)
	}
	if want := time.Date(2026, 8, 23, 6, 0, 0, 0, time.UTC); !end.UTC().Equal(want) {
		t.Fatalf("end = %s, want %s", end.UTC(), want)
	}
	// The boundary case the ::date predicate got wrong: 2026-08-23 05:59Z is
	// still 2026-08-22 in Denver (23:59 MDT) and must fall inside the day.
	edge := time.Date(2026, 8, 23, 5, 59, 0, 0, time.UTC)
	if !(edge.After(start) || edge.Equal(start)) || !edge.Before(end) {
		t.Fatalf("23:59 Denver must be inside the day: start=%s edge=%s end=%s", start, edge, end)
	}
	// January: MST = UTC-7.
	wStart, _, err := denverDayBounds("2026-01-15")
	if err != nil {
		t.Fatalf("denverDayBounds winter: %v", err)
	}
	if want := time.Date(2026, 1, 15, 7, 0, 0, 0, time.UTC); !wStart.UTC().Equal(want) {
		t.Fatalf("winter start = %s, want %s", wStart.UTC(), want)
	}
	if _, _, err := denverDayBounds("not-a-date"); err == nil {
		t.Fatal("malformed date must error")
	}
}

// NAME_DATE must accept BOTH live date schemes for the board's date — the
// 8-digit token and the month-name token — and still flag a wrong or absent
// date in either scheme.
func TestBoardGrid_NameDateAcceptsMonthNameScheme(t *testing.T) {
	s := &BoardGridService{}
	run := func(name string) int {
		c := cell("AADWD", "07:01", "o1", "NL", name, "s")
		c.BrandRoot = "aadwd.com"
		return findingCodes(s.runGates(nil, "2026-08-22", []BoardCell{c}))["NAME_DATE"]
	}
	for _, ok := range []string{
		"aug22 - aad - KUMO-WARM - newsletter",
		"AUG22 - aad - KUMO-WARM",
		"08222026 - aad - KUMO-WARM",
	} {
		if run(ok) != 0 {
			t.Errorf("%q carries the board's date and must not flag", ok)
		}
	}
	for _, bad := range []string{
		"jul28 - aad - KUMO-WARM",    // wrong date, month-name scheme
		"aug2 - aad - KUMO-WARM",     // wrong day
		"08062026 - aad - KUMO-WARM", // wrong date, 8-digit scheme
		"aad - KUMO-WARM",            // no date at all
	} {
		if run(bad) != 1 {
			t.Errorf("%q does not carry 2026-08-22 and must flag NAME_DATE", bad)
		}
	}
}

// Two colliding cells that BOTH failed brand-metadata mapping are a metadata
// gap, not a proven double-book: demoted to warn UNMAPPED_COLLISION. A mapped
// property colliding stays a blocker.
func TestBoardGrid_UnmappedCollisionDemoted(t *testing.T) {
	s := &BoardGridService{}
	unmapped := []BoardCell{
		{Property: "(unmapped)", Slot: "11:01", OfferID: "o1", OfferName: "A", Name: "08232026 - A", Subject: "s"},
		{Property: "(unmapped)", Slot: "11:01", OfferID: "o2", OfferName: "B", Name: "08232026 - B", Subject: "s"},
	}
	got := findingCodes(s.runGates(nil, "2026-08-23", unmapped))
	if got["SLOT_COLLISION"] != 0 {
		t.Fatalf("unmapped collision must not block: %v", got)
	}
	if got["UNMAPPED_COLLISION"] != 1 {
		t.Fatalf("want 1 UNMAPPED_COLLISION warn, got %v", got)
	}
	// And the demotion must not leak onto real properties.
	mapped := []BoardCell{
		cell("WF", "11:01", "o1", "CHW", "08232026 - WF - CHW", "s"),
		cell("WF", "11:01", "o2", "Sams", "08232026 - WF - Sams", "s"),
	}
	if c := findingCodes(s.runGates(nil, "2026-08-23", mapped)); c["SLOT_COLLISION"] != 1 {
		t.Fatalf("mapped collision must stay a blocker: %v", c)
	}
}

// POST /gates must parse the date like GET does. A malformed date used to slip
// through, leave dateTok empty, and silently disable NAME_DATE — a gate run
// that reports clean because it never ran is worse than a 400.
func TestBoardGrid_RunGatesRejectsBadDate(t *testing.T) {
	s := &BoardGridService{}
	post := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/mailing/board-grid/gates",
			strings.NewReader(body))
		w := httptest.NewRecorder()
		s.HandleRunGates(w, req)
		return w.Code
	}
	if code := post(`{"date":"08/23/2026","cells":[]}`); code != http.StatusBadRequest {
		t.Fatalf("malformed date: want 400, got %d", code)
	}
	if code := post(`{"cells":[]}`); code != http.StatusBadRequest {
		t.Fatalf("missing date: want 400, got %d", code)
	}
	if code := post(`{"date":"2026-08-23","cells":[]}`); code != http.StatusOK {
		t.Fatalf("valid date: want 200, got %d", code)
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
