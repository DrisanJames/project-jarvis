package dripsupply

import (
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// decide() — the allocation arithmetic, tested WITHOUT a database
// -----------------------------------------------------------------------------
//
// The 2026-09-08 defect was not that the estate was capped; it was that
// 343,829 contracted became 113,764 committed and nothing said which term did
// it. A mediator is a go-between: it may return the LESSER of the two contracts
// and it must NAME which one. So every case below asserts BOTH halves — the
// number and the term — and the first case is the negative control that the
// function is not simply always returning the smallest thing it can find.

// grantCase is one (inputs → decision) row.
type grantCase struct {
	name       string
	in         GrantInputs
	wantGrant  int
	wantReason string
}

// fullBalance is a domain balance with nothing reduced: contracted == effective,
// no governor, tokens and headroom both at n.
func fullBalance(n int) Balance {
	return Balance{Contracted: n, Effective: n, Tokens: float64(n)}
}

func TestDecide_BindingTermIsNamed(t *testing.T) {
	const N = 50_000

	cases := []grantCase{
		{
			// THE CONTRACT CASE. Both contracts say N and demand is N, so the
			// go-between must hand back N — not a number it invented. If this
			// ever returns less than N, the mediator is inventing a constraint
			// neither party agreed to, which is the whole incident.
			name: "contract N and demand N grants N",
			in: GrantInputs{
				Requested:      N,
				Domain:         fullBalance(N),
				Lane:           LaneBalance{Desired: N, Unfilled: N},
				PlanRemaining:  N,
				PlanBounded:    true,
				MailableSupply: N,
			},
			wantGrant:  N,
			wantReason: ReasonRequested,
		},
		{
			name: "demand below every ceiling binds on requested",
			in: GrantInputs{
				Requested:      1_000,
				Domain:         fullBalance(N),
				Lane:           LaneBalance{Unfilled: N},
				MailableSupply: N,
			},
			wantGrant:  1_000,
			wantReason: ReasonRequested,
		},
		{
			// effective == contracted, so the headroom term carries the
			// domain_tokens label: no governor did this, the day is simply spent.
			name: "domain headroom binds",
			in: GrantInputs{
				Requested:      N,
				Domain:         Balance{Contracted: N, Effective: N, Reserved: N - 700, Tokens: float64(N)},
				Lane:           LaneBalance{Unfilled: N},
				MailableSupply: N,
			},
			wantGrant:  700,
			wantReason: ReasonDomainTokens,
		},
		{
			name: "token bucket binds",
			in: GrantInputs{
				Requested:      N,
				Domain:         Balance{Contracted: N, Effective: N, Tokens: 412.9},
				Lane:           LaneBalance{Unfilled: N},
				MailableSupply: N,
			},
			wantGrant:  412, // floor, never round: a bucket may not lend
			wantReason: ReasonDomainTokens,
		},
		{
			name: "lane demand binds",
			in: GrantInputs{
				Requested:      N,
				Domain:         fullBalance(N),
				Lane:           LaneBalance{Desired: N, Unfilled: 3_100},
				MailableSupply: N,
			},
			wantGrant:  3_100,
			wantReason: ReasonLaneDemand,
		},
		{
			name: "plan share binds",
			in: GrantInputs{
				Requested:      N,
				Domain:         fullBalance(N),
				Lane:           LaneBalance{Unfilled: N},
				PlanRemaining:  2_400,
				PlanBounded:    true,
				MailableSupply: N,
			},
			wantGrant:  2_400,
			wantReason: ReasonPlanShare,
		},
		{
			name: "mailable supply binds",
			in: GrantInputs{
				Requested:      N,
				Domain:         fullBalance(N),
				Lane:           LaneBalance{Unfilled: N},
				MailableSupply: 900,
			},
			wantGrant:  900,
			wantReason: ReasonSupply,
		},
		{
			name: "a governor that reduced effective is named, not 'domain_tokens'",
			in: GrantInputs{
				Requested: N,
				// This is the shape the incident had: a ceiling below the
				// contract. The operator must be able to read WHICH ceiling.
				Domain:         Balance{Contracted: N, Effective: 25_000, EffectiveReason: "health_band:amber", Tokens: float64(N)},
				Lane:           LaneBalance{Unfilled: N},
				MailableSupply: N,
			},
			wantGrant:  25_000,
			wantReason: ReasonGovernor + ":health_band:amber",
		},
		{
			// A governor that zeroes `effective` also zeroes `tokens` at the
			// next refill, so BOTH terms read 0. Reporting domain_tokens there
			// blames pacing for a governor stop and sends the operator to the
			// wrong screen; the term order in decide() is what prevents it.
			name: "a governor stop outranks the token term at zero",
			in: GrantInputs{
				Requested:      N,
				Domain:         Balance{Contracted: N, Effective: 0, EffectiveReason: "health_band:red", Tokens: 0},
				Lane:           LaneBalance{Unfilled: N},
				MailableSupply: N,
			},
			wantGrant:  0,
			wantReason: ReasonGovernor + ":health_band:red",
		},
		{
			name: "effective reduced with no reason recorded still says a governor did it",
			in: GrantInputs{
				Requested:      N,
				Domain:         Balance{Contracted: N, Effective: 8_000, Tokens: float64(N)},
				Lane:           LaneBalance{Unfilled: N},
				MailableSupply: N,
			},
			wantGrant:  8_000,
			wantReason: ReasonGovernor + ":reduced",
		},
		{
			name: "supply zero is a real zero and names supply",
			in: GrantInputs{
				Requested:      N,
				Domain:         fullBalance(N),
				Lane:           LaneBalance{Unfilled: N},
				MailableSupply: 0,
			},
			wantGrant:  0,
			wantReason: ReasonSupply,
		},
		{
			name: "lane demand zero names lane_demand",
			in: GrantInputs{
				Requested:      N,
				Domain:         fullBalance(N),
				Lane:           LaneBalance{Unfilled: 0},
				MailableSupply: N,
			},
			wantGrant:  0,
			wantReason: ReasonLaneDemand,
		},
		{
			// NEGATIVE CONTROL for the two optional terms. A supply of -1 means
			// "unknown", and an unbounded plan means "the plan does not
			// constrain this cell". Neither may participate as a 0 — treating
			// "unknown" as "none" is how a whole estate goes dark.
			name: "unknown supply and an unbounded plan do NOT constrain",
			in: GrantInputs{
				Requested:      N,
				Domain:         fullBalance(N),
				Lane:           LaneBalance{Unfilled: N},
				PlanRemaining:  0,     // ignored: PlanBounded is false
				PlanBounded:    false, //
				MailableSupply: -1,    // ignored: negative = unknown
			},
			wantGrant:  N,
			wantReason: ReasonRequested,
		},
		{
			name: "a negative term is floored at zero, not treated as unbounded",
			in: GrantInputs{
				Requested:      N,
				Domain:         Balance{Contracted: N, Effective: N, Reserved: N + 5_000, Tokens: float64(N)},
				Lane:           LaneBalance{Unfilled: N},
				MailableSupply: N,
			},
			wantGrant:  0,
			wantReason: ReasonDomainTokens,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decide(c.in)
			if got.Granted != c.wantGrant {
				t.Errorf("granted = %d, want %d", got.Granted, c.wantGrant)
			}
			if got.BindingReason != c.wantReason {
				t.Errorf("binding_reason = %q, want %q — a number with no term next to it is the 2026-09-08 incident", got.BindingReason, c.wantReason)
			}
			if got.Granted == 0 && got.BindingReason == ReasonRequested && c.in.Requested > 0 {
				t.Errorf("granted 0 while claiming nothing constrained below a demand of %d", c.in.Requested)
			}
		})
	}
}

// TestDecide_IsPure proves decide() has no I/O and mutates nothing the caller
// owns: the same inputs must produce the same Decision, and the input struct
// must be byte-identical afterwards.
func TestDecide_IsPure(t *testing.T) {
	in := GrantInputs{
		Requested:      10_000,
		Domain:         Balance{Contracted: 9_000, Effective: 7_000, EffectiveReason: "throttle", Tokens: 6_500.7, Reserved: 100},
		Lane:           LaneBalance{Unfilled: 8_000},
		PlanRemaining:  6_600,
		PlanBounded:    true,
		MailableSupply: 6_700,
	}
	before := in
	first := decide(in)
	for i := 0; i < 100; i++ {
		if got := decide(in); got != first {
			t.Fatalf("decide is not deterministic: call %d = %+v, first = %+v", i, got, first)
		}
	}
	if in != before {
		t.Fatalf("decide mutated its input:\n got %+v\nwant %+v", in, before)
	}
	// 6,500 (floor of tokens) is the smallest term here.
	if first.Granted != 6_500 || first.BindingReason != ReasonDomainTokens {
		t.Fatalf("decide = %+v, want 6500 / %s", first, ReasonDomainTokens)
	}
}

// -----------------------------------------------------------------------------
// refill() — pure, and it now SIZES what it throws away
// -----------------------------------------------------------------------------

func TestRefill_IsPureAndSizesTheForfeit(t *testing.T) {
	w := DefaultWindow() // 76 intervals, burst 2
	const effective = 7600
	perInterval := float64(effective) / 76 // 100
	ceiling := perInterval * 2             // 200

	in := bucketBalance(t, effective, 0)
	before := in
	// Nine hours of downtime: 36 intervals accrue, 2 survive the clamp.
	out, res := refill(in, w, dayOf(in.Day).Add(10*time.Hour))

	if in != before {
		t.Fatalf("refill mutated its argument:\n got %+v\nwant %+v", in, before)
	}
	if out.Tokens != ceiling {
		t.Fatalf("tokens = %v, want the burst ceiling %v", out.Tokens, ceiling)
	}
	if !res.Capped {
		t.Fatal("Capped is false — the clamp is not reporting itself")
	}
	wantForfeit := perInterval*float64(res.IntervalsElapsed) - ceiling // 3600 - 200
	if res.Forfeited != wantForfeit {
		t.Fatalf("Forfeited = %v, want %v — 'Capped' alone cannot tell an operator HOW MUCH the domain lost", res.Forfeited, wantForfeit)
	}
	// Negative control: a normal single-interval refill forfeits nothing.
	_, ok := refill(bucketBalance(t, effective, 0), w, dayOf(in.Day).Add(time.Hour+15*time.Minute))
	if ok.Forfeited != 0 || ok.Capped {
		t.Fatalf("a single-interval refill reported Forfeited=%v Capped=%v — the forfeit table would fill with non-events", ok.Forfeited, ok.Capped)
	}
}

// TestRefill_DayRollIsNotAForfeit pins the distinction the two tables depend on:
// tokens expiring at the day boundary are allowance that ran out on schedule,
// not allowance the clamp took. Summing them would make the forfeit table read
// as if pacing lost a day's mail every night.
func TestRefill_DayRollIsNotAForfeit(t *testing.T) {
	b := bucketBalance(t, 7600, 175)
	out, res := refill(b, DefaultWindow(), dayOf(b.Day).AddDate(0, 0, 1).Add(2*time.Hour))
	if !res.DayRolled || out.Tokens != 0 {
		t.Fatalf("day roll = %+v tokens %v, want DayRolled with tokens 0", res, out.Tokens)
	}
	if res.Forfeited != 0 {
		t.Fatalf("Forfeited = %v on a day roll, want 0", res.Forfeited)
	}
}
