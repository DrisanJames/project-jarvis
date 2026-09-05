package dripsupply

import (
	"strings"
	"testing"
)

// The 2026-09-05 outage in one assertion: every per-ISP grant came back zero
// because no drip_lane_balance row existed, and the outcome row said
// "no_positive_grant" — the same string a healthy, fully-paced lane writes.
// The reason was known at Reserve and thrown away one frame later.
func TestZeroGrantReasonNamesTheConstraintThatBound(t *testing.T) {
	a := &Allocation{
		grants:  map[string]int{"gmail": 0, "yahoo": 0, "microsoft": 0},
		reasons: map[string]string{"gmail": ReasonNoLaneBalance, "yahoo": ReasonNoLaneBalance, "microsoft": ReasonNoLaneBalance},
	}
	got := a.ZeroGrantReason(SkipNoPositiveGrant)
	if got != SkipNoPositiveGrant+":"+ReasonNoLaneBalance {
		t.Fatalf("outage shape must name no_lane_balance, got %q", got)
	}
	// And it must still be recognisable as the base reason to anything that
	// keys on that, which is why it composes rather than replaces.
	if !strings.HasPrefix(got, SkipNoPositiveGrant) {
		t.Errorf("composed reason must keep the base as its prefix, got %q", got)
	}
}

// THE point of the whole change: the composed reason must reach the dark alert
// as a contract denial. A whitespace-only tokeniser reads it as pacing and the
// estate goes dark in silence.
func TestComposedReasonIsClassifiedAsContractDenial(t *testing.T) {
	composed := (&Allocation{
		grants:  map[string]int{"gmail": 0},
		reasons: map[string]string{"gmail": ReasonNoLaneBalance},
	}).ZeroGrantReason(SkipNoPositiveGrant)

	if !isContractDenialReason(composed) {
		t.Fatalf("%q must classify as a contract denial", composed)
	}
	// Also in the brand-folded form Outcome writes.
	if !isContractDenialReason(composed + " brand=db") {
		t.Errorf("brand-folded %q must classify as a contract denial", composed)
	}
}

// Composition must not make anything benign look like an incident.
func TestBenignReasonsStayBenign(t *testing.T) {
	for _, r := range []string{
		SkipNoPositiveGrant, // bare: ordinary token-bucket pacing
		SkipNoPositiveGrant + ":" + ReasonSupply,
		SkipNoPositiveGrant + ":" + ReasonDomainTokens,
		SkipNoPositiveGrant + ":" + ReasonLaneDemand,
		SkipNoPositiveGrant + ":" + ReasonPlanShare,
		ReasonGovernor + ":acme",
		SkipPaused, SkipBudgetExhausted, SkipOutsideWindow, SkipReserveTimeout,
		ZeroNoRecordsClaimed, ZeroAllDeferred,
	} {
		if isContractDenialReason(r) {
			t.Errorf("%q must NOT read as a contract denial", r)
		}
	}
}

// A wave reserves several ISPs. One incidental empty-supply lane must not mask
// a no_lane_balance affecting every other ISP on the domain.
func TestDominantReasonWinsNotFirstSeen(t *testing.T) {
	a := &Allocation{
		grants: map[string]int{"aol": 0, "gmail": 0, "microsoft": 0, "yahoo": 0},
		reasons: map[string]string{
			"aol":       ReasonSupply, // sorts first, but is the minority
			"gmail":     ReasonNoLaneBalance,
			"microsoft": ReasonNoLaneBalance,
			"yahoo":     ReasonNoLaneBalance,
		},
	}
	if got := a.ZeroGrantReason(SkipNoPositiveGrant); got != SkipNoPositiveGrant+":"+ReasonNoLaneBalance {
		t.Fatalf("majority reason must win, got %q", got)
	}
}

// Two orchestrator instances replaying the same wave must log the same string.
func TestTieBreakIsDeterministic(t *testing.T) {
	build := func() *Allocation {
		return &Allocation{
			grants:  map[string]int{"gmail": 0, "yahoo": 0},
			reasons: map[string]string{"gmail": ReasonSupply, "yahoo": ReasonNoLaneBalance},
		}
	}
	first := build().ZeroGrantReason(SkipNoPositiveGrant)
	for i := 0; i < 50; i++ {
		if got := build().ZeroGrantReason(SkipNoPositiveGrant); got != first {
			t.Fatalf("tie must be deterministic: %q then %q", first, got)
		}
	}
	// Ties break on the reason name, so the lexicographically smaller wins.
	if first != SkipNoPositiveGrant+":"+ReasonNoLaneBalance {
		t.Errorf("tie should resolve to the lexically first reason, got %q", first)
	}
}

// An ISP that was granted records did not bind; its reason is not the story.
func TestGrantedISPsDoNotContributeTheirReason(t *testing.T) {
	a := &Allocation{
		grants:  map[string]int{"gmail": 500, "yahoo": 0},
		reasons: map[string]string{"gmail": ReasonSupply, "yahoo": ReasonNoLaneBalance},
	}
	if got := a.ZeroGrantReason(SkipNoPositiveGrant); got != SkipNoPositiveGrant+":"+ReasonNoLaneBalance {
		t.Fatalf("only zero-granted ISPs bind, got %q", got)
	}
}

// "requested" means nothing constrained below demand. It cannot co-exist with a
// zero grant, and letting it win would rename a real constraint to a non-event.
func TestRequestedIsNeverTheBindingReason(t *testing.T) {
	a := &Allocation{
		grants:  map[string]int{"gmail": 0, "yahoo": 0},
		reasons: map[string]string{"gmail": ReasonRequested, "yahoo": ReasonRequested},
	}
	if got := a.ZeroGrantReason(SkipNoPositiveGrant); got != SkipNoPositiveGrant {
		t.Fatalf("requested must not qualify the reason, got %q", got)
	}
}

// Degenerate inputs must pass the base straight through — the caller uses the
// return value unconditionally.
func TestZeroGrantReasonFallsBackToBase(t *testing.T) {
	var nilAlloc *Allocation
	cases := map[string]*Allocation{
		"nil receiver": nilAlloc,
		"empty":        {grants: map[string]int{}, reasons: map[string]string{}},
		"no reason recorded": {
			grants:  map[string]int{"gmail": 0},
			reasons: map[string]string{"gmail": ""},
		},
		"reason equals base": {
			grants:  map[string]int{"gmail": 0},
			reasons: map[string]string{"gmail": SkipNoPositiveGrant},
		},
		"shadow allocation (nil maps)": {},
	}
	for name, a := range cases {
		if got := a.ZeroGrantReason(SkipNoPositiveGrant); got != SkipNoPositiveGrant {
			t.Errorf("%s: expected bare base, got %q", name, got)
		}
	}
}
