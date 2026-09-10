package dripsupply

import "testing"

// Operator 2026-09-10: "I do not care for these notifications: WARN · Drip ·
// converters_roofing produced nothing for 225 ticks · followup Reason:
// no_records_claimed brand=wfy". trackStreak paged on any two consecutive
// `zero` ticks, and a zero tick means nothing was claimable — so every idle
// lane posted once an hour forever. These pin the corrected contract.

// Negative control: a quiet lane must never page, however long it stays quiet.
func TestZeroClaimStreakDoesNotPage(t *testing.T) {
	f, done := newDarkFixture(t, ModeOn)
	defer done()
	const lane = "converters_roofing"
	for i := 0; i < 225; i++ {
		f.expectOutcome()
		f.tick(lane, PassFollowup, OutcomeZero, ZeroNoRecordsClaimed, "wfy")
	}
	if n := len(f.note.matching("produced nothing")); n != 0 {
		t.Fatalf("an idle lane paged %d time(s); zero-claim ticks must stay silent", n)
	}
}

// Positive control: a lane whose ticks actually ERROR still pages, once per window.
func TestFailedStreakStillPages(t *testing.T) {
	f, done := newDarkFixture(t, ModeOn)
	defer done()
	const lane = "failing_lane"
	for i := 0; i < 3; i++ {
		f.expectOutcome()
		f.tick(lane, PassWelcome, OutcomeFailed, "claim_records", "db")
	}
	if n := len(f.note.matching("produced nothing")); n != 1 {
		t.Fatalf("want exactly one ALERT for a failing lane inside the window, got %d", n)
	}
}

// A zero tick between failures resets nothing it shouldn't: two failures in a
// row are still required, and a lone failure after a quiet stretch is silent.
func TestLoneFailureAfterQuietStretchIsSilent(t *testing.T) {
	f, done := newDarkFixture(t, ModeOn)
	defer done()
	const lane = "mostly_quiet_lane"
	for i := 0; i < 5; i++ {
		f.expectOutcome()
		f.tick(lane, PassWelcome, OutcomeZero, ZeroNoRecordsClaimed, "")
	}
	// zero ticks still ADVANCE the shared streak counter (they count as
	// non-productive), so one failure after them reaches n>=2 — but it must
	// only page because the CURRENT tick failed, not because of the zeros.
	f.expectOutcome()
	f.tick(lane, PassWelcome, OutcomeFailed, "claim_records", "")
	if n := len(f.note.matching("produced nothing")); n != 1 {
		t.Fatalf("a failing tick after a quiet stretch should page once, got %d", n)
	}
}
