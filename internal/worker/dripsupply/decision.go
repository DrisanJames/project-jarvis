package dripsupply

import (
	"math"
	"strings"
)

// -----------------------------------------------------------------------------
// The allocation arithmetic, as a pure function
// -----------------------------------------------------------------------------
//
// §2.2's min() used to exist twice: inline inside Service.Reserve's transaction
// (reservation.go, between the two FOR UPDATE reads) and again in
// executor.go's shadowTerms. Both copies mixed the decision with the I/O around
// it, so the only way to ask "what would this cell be granted, and by which
// term" was to open a transaction and take two row locks.
//
// decide() is that arithmetic with the I/O removed: inputs in, Decision out, no
// database handle, no clock, no mutation of anything the caller owns. The
// callers read the rows and persist the answer; this function only chooses.
//
// It is deliberately the ONLY implementation. The mediator's 2026-09-08 defect
// — 343,829 contracted, 113,764 committed, and nothing recording which term
// took the other 230,065 — is a defect of the record, not of the arithmetic,
// and a single arithmetic with a named binding term is what makes the record
// possible.

// GrantInputs is every number §2.2's min() consumes for one domain×ISP×lane
// cell. It is a value: decide() cannot reach past it.
type GrantInputs struct {
	// Requested is the wave's ask.
	Requested int
	// Domain is the locked drip_capacity_balance row (contracted, effective,
	// effective_reason, tokens, reserved, committed).
	Domain Balance
	// Lane is the locked drip_lane_balance row.
	Lane LaneBalance
	// PlanRemaining is WP6's plan_share term; it participates only when
	// PlanBounded is true. A nil PlanReader (or a plan that does not constrain
	// this cell) leaves the term out entirely rather than passing 0, which
	// would zero every grant in the estate.
	PlanRemaining int
	PlanBounded   bool
	// MailableSupply is the §2.2 supply term. NEGATIVE means "unknown / not
	// supply-bound" and the term does not participate; 0 means there is
	// genuinely nothing mailable and the grant is 0 with reason 'supply'.
	MailableSupply int
}

// Decision is the answer: the number, and the name of the term that produced
// it. Every caller writes BindingReason to drip_capacity_ledger.binding_reason,
// which is the whole point — a number with no term next to it is the shape of
// the 2026-09-08 incident.
type Decision struct {
	Granted       int
	BindingReason string
}

// DomainTermReason names the domain-headroom term for one balance row.
//
// The label comes off the ROW (effective_reason, written by RefillDomain), not
// from process memory: both orchestrator instances and the §3 API must report
// the same reason for the same cell. `effective` below `contracted` with an
// empty effective_reason is a refill that predates the column or a hand-edited
// row — it reports "governor:reduced" rather than naming a governor that may
// not have done it.
func DomainTermReason(b Balance) string {
	if b.Effective >= b.Contracted {
		return ReasonDomainTokens
	}
	name := strings.TrimSpace(b.EffectiveReason)
	if name == "" {
		name = "reduced"
	}
	return ReasonGovernor + ":" + name
}

// decide is §2.2 step (3): granted = min(requested, domain headroom,
// floor(tokens), lane unfilled, plan_remaining, mailable_supply), and the name
// of the term that bound it.
//
// PURE: no I/O, no clock, no mutation. Term ORDER is load-bearing — it is
// bindingMin's tie-break, so it is pinned here and by
// TestDecide_BindingTermIsNamed:
//
//  1. requested       — ties resolve here first, so a fully satisfied wave
//     reads 'requested' rather than a constraint that happened to equal demand.
//  2. domain headroom  — ahead of the token term ON PURPOSE: when a governor
//     zeroes `effective` the refill also zeroes `tokens`, both terms are 0, and
//     reporting 'domain_tokens' there would blame pacing for a governor stop.
//  3. floor(tokens)
//  4. lane unfilled
//  5. plan_share  (only when bounded)
//  6. supply      (only when >= 0)
func decide(in GrantInputs) Decision {
	terms := []term{
		{ReasonRequested, in.Requested},
		{DomainTermReason(in.Domain), in.Domain.Headroom()},
		{ReasonDomainTokens, int(math.Floor(in.Domain.Tokens))},
		{ReasonLaneDemand, in.Lane.Unfilled},
	}
	if in.PlanBounded {
		terms = append(terms, term{ReasonPlanShare, in.PlanRemaining})
	}
	if in.MailableSupply >= 0 {
		terms = append(terms, term{ReasonSupply, in.MailableSupply})
	}
	granted, reason := bindingMin(terms)
	return Decision{Granted: granted, BindingReason: reason}
}
