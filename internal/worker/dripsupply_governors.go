package worker

// dripsupply_governors.go — the REQ-118 governor stack, in ONE place.
//
// D3: before this file, cmd/server/main.go wired exactly one governor:
//
//	supplyGovernors := dripsupply.ThrottleGovernor{DB: db}
//
// dripsupply.NewGmailHoldGovernor (bucket.go:346) and
// dripsupply.NewSESQuotaGovernor (bucket.go:528) shipped implemented and
// unit-tested (TestGmailHold_*, TestSESQuota_*) and registered NOWHERE, so the
// reservation path had **no SES daily-quota ceiling at all** — the exact
// "built-but-unregistered" shape the wiring rule exists to catch, against a
// failure mode the estate has already paid for (memory
// ses-quota-breach-burns-recipients).
//
// dripsupply.HealthBandGovernor is deliberately NOT here: it is not a
// GovernorReader (it has no Ceilings method — it cannot satisfy the interface).
// RefillDomain applies the band directly from the *DomainContract it is already
// holding (bucket.go:809), which is why it needs no registration and why adding
// it would be a second, DB-round-tripping copy of a decision already made.

import (
	"os"
	"strings"

	"github.com/ignite/sparkpost-monitor/internal/worker/dripsupply"
)

// DripSupplyGovernorsOffEnv is the D3 kill switch: a comma-separated list of
// governor names to leave OUT of the stack (e.g. "ses_quota,gmail_hold"). One
// env var, no deploy — which matters because GmailHoldGovernor is the one
// governor that fails CLOSED, so an unreadable ban table zeroes gmail.
const DripSupplyGovernorsOffEnv = "DRIP_SUPPLY_GOVERNORS_OFF"

// DripSupplyGovernors builds the governor stack the reservation path (and the
// daily planner) applies. Order is the order Governors.Ceilings evaluates in;
// it does not affect the outcome (the lowest ceiling wins) but it fixes the
// name list the wiring test asserts on.
//
//	db       the mailing pool. nil leaves the DB-backed governors inert rather
//	         than erroring — ThrottleGovernor and GmailHoldGovernor both check.
//	orgID    "" = the cross-org ban union, which can only ban MORE.
//	sesQuota nil makes SESQuotaGovernor permanently inert and it says so once;
//	         it is never a source of zeroes.
func DripSupplyGovernors(db dripsupply.Queryer, orgID string, sesQuota dripsupply.SESQuotaFunc) dripsupply.Governors {
	off := dripSupplyGovernorsOff()
	out := make(dripsupply.Governors, 0, 3)
	if !off["throttle"] {
		out = append(out, dripsupply.ThrottleGovernor{DB: db})
	}
	if !off["gmail_hold"] {
		out = append(out, dripsupply.NewGmailHoldGovernor(db, orgID))
	}
	if !off["ses_quota"] {
		out = append(out, dripsupply.NewSESQuotaGovernor(sesQuota))
	}
	return out
}

// DripSupplyGovernorNames is the stack's roster by the name each governor
// stamps on its GovernorCeiling. Logged at boot so "which ceilings are live"
// is answerable from the task log instead of from the source.
func DripSupplyGovernorNames(g dripsupply.Governors) []string {
	out := make([]string, 0, len(g))
	for _, r := range g {
		switch r.(type) {
		case dripsupply.ThrottleGovernor, *dripsupply.ThrottleGovernor:
			out = append(out, "throttle")
		case *dripsupply.GmailHoldGovernor:
			out = append(out, "gmail_hold")
		case *dripsupply.SESQuotaGovernor:
			out = append(out, "ses_quota")
		case nil:
			continue
		default:
			out = append(out, "unknown")
		}
	}
	return out
}

func dripSupplyGovernorsOff() map[string]bool {
	out := map[string]bool{}
	for _, n := range strings.Split(os.Getenv(DripSupplyGovernorsOffEnv), ",") {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			out[n] = true
		}
	}
	return out
}
