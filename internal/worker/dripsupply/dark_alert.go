package dripsupply

// dark_alert.go — make a drip lane going DARK under contract enforcement LOUD.
//
// INCIDENT 2026-09-05. DRIP_SUPPLY_CHAIN_MODE=on from 03:52Z to 17:43Z stopped
// drip mail entirely: last record 05:58Z, next 18:00Z, zero mailed for 11h42m
// against 141,267 the prior day. Mediator.failClosed (executor.go:836) returns
// Skip=true for every wave of a lane with no active dispatch contract, so the
// orchestrator wrote `skipped/no_contract` into drip_tick_outcomes and did
// nothing else. One lane (internal_auto_insurance_v12) was completely dark for
// an hour before a watcher happened to notice; the operator found the outage by
// eyeballing total volume, hours late.
//
// WHY THE EXISTING STREAK DID NOT CATCH IT. trackStreak (executor.go:1155)
// counts OutcomeZero and OutcomeFailed and hits `default: delete(...)` for
// OutcomeSkipped. A contract-denied wave is `skipped`, so every dark tick did
// not merely fail to count — it actively RESET the counter. The lane could stay
// denied forever at streak 0.
//
// WHAT THIS ADDS. A second streak, `Mediator.darkStreak`, that counts the
// opposite thing: consecutive outcomes whose reason says the CONTRACT/BALANCE
// state denied the wave (no_contract, no_contract_key, no_balance,
// no_lane_balance) — never the reasons that mean the lane is legitimately
// quiet (paused, budget_exhausted, outside_window, no_wave_size,
// no_positive_grant, no_records_claimed). At ContractDarkStreak consecutive
// denials it probes partner_clean_queue for records the claim COULD have taken,
// and alerts only if some exist. "No supply" is benign and already common; the
// incident is "supply exists and capacity/contract denied it".
//
// Three properties this file must keep:
//
//   - INERT in shadow/off. Production runs `shadow`, where failClosed never
//     sets Skip and the legacy chain still ships the wave. An alert there is a
//     pure false positive once per lane per hour, forever — a regression worse
//     than the silence it replaces. The mode gate is the FIRST thing checked.
//   - ONE alert per lane per window. It reuses alertOnce / m.lastAlert; it does
//     not invent a second dedupe. The key is distinct from trackStreak's
//     "lane:<lane>" on purpose: sharing it would let a benign zero-claim WARN
//     swallow the dark ALERT for an hour.
//   - BOUNDED cost. The supply probe is a LIMIT-1 index probe behind alertDue,
//     so it runs at most once per lane×pass per AlertEvery window (≈15 reads an
//     hour estate-wide), under its own 5s timeout so a database brownout cannot
//     hold the tick against the 30s prod statement_timeout.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/ignite/sparkpost-monitor/internal/notify"
)

const (
	// ContractDarkStreak is how many CONSECUTIVE contract-denied outcomes one
	// lane×pass must produce before the alert fires. Two, matching trackStreak:
	// one denied tick is a contract being rotated, two in a row is a lane that
	// is not coming back on its own.
	ContractDarkStreak = 2

	// DarkAlertDisabledEnv is the kill switch. Read at CALL time, not at
	// construction, so an operator can silence a storm without a deploy — the
	// same shape as governorCeilingDisabled / convertersPinDisabled.
	DarkAlertDisabledEnv = "DRIP_SUPPLY_DARK_ALERT_DISABLED"

	// darkSupplyProbeLimit is the LIMIT on the supply probe. The alert needs
	// "is there ANY claimable record", never an exact count, so the probe stops
	// at the first row instead of counting a 5.8M-row table.
	darkSupplyProbeLimit = 1

	// darkSupplyProbeTimeout bounds the probe well inside the prod 30s
	// statement_timeout: an alert path must never be what wedges a tick.
	darkSupplyProbeTimeout = 5 * time.Second
)

// contractDenialReasons is the CLOSED set of reasons that mean "the contract or
// balance state denied this wave before any record was considered".
//
// Membership is deliberately narrow. Every reason NOT here is either normal
// pacing (no_positive_grant, reserve_timeout), a deliberate operator state
// (paused, budget_exhausted, outside_window), or genuine empty supply
// (no_records_claimed, all_records_deferred) — none of which is this incident,
// and all of which are common enough that alerting on them is an alert storm.
var contractDenialReasons = map[string]bool{
	SkipNoContract:      true, // no active domain OR dispatch contract (the incident)
	SkipNoContractKey:   true, // CONTRACT_TOKEN_KEY unset — every lane fails closed
	ReasonNoBalance:     true, // no drip_capacity_balance row for domain×ISP
	ReasonNoLaneBalance: true, // no drip_lane_balance row for lane×ISP
}

// isContractDenialReason tokenises rather than compares whole strings, on BOTH
// separators the package composes reasons with:
//
//	whitespace — Outcome folds the brand in ("no_contract brand=db")
//	colon      — a qualified reason ("governor:<name>",
//	             "no_positive_grant:no_lane_balance" from ZeroGrantReason)
//
// The colon half is load-bearing, not defensive. A wave whose every per-ISP
// grant is zero reports no_positive_grant, and ZeroGrantReason appends the
// constraint that actually bound. no_lane_balance arrives EXCLUSIVELY in that
// composed form — a missing lane balance produces a zero grant, never a skip —
// so a whitespace-only tokeniser would classify the single largest cause of the
// 2026-09-05 outage (44,658 denied grants) as ordinary pacing and stay silent
// through the whole thing. Splitting on ':' is what connects the two halves.
//
// Qualifying a reason can only ADD a token, so this cannot reclassify anything
// that was already benign: "governor:<name>" still matches nothing.
func isContractDenialReason(reason string) bool {
	for _, tok := range strings.FieldsFunc(reason, func(r rune) bool {
		return r == ':' || unicode.IsSpace(r)
	}) {
		if contractDenialReasons[tok] {
			return true
		}
	}
	return false
}

func darkAlertDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(DarkAlertDisabledEnv))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// trackContractDark is the second half of Outcome's streak tracking: the one
// that counts what trackStreak resets on.
//
// `reason` is the RAW reason, before Outcome folds the brand in; the classifier
// tolerates either.
func (m *Mediator) trackContractDark(ctx context.Context, lane, pass, outcome, reason string) {
	if m == nil {
		return
	}
	// INERT unless a cell can actually be enforced (§ "three properties").
	// Checked before the streak is even counted, so a shadow-mode process
	// carries no state for this at all.
	if !m.cfg.Mode.Enforces() {
		return
	}

	// A wave that FIRED is not dark, whatever its reason says.
	denied := outcome != OutcomeFired && isContractDenialReason(reason)

	key := lane + "|" + pass
	m.mu.Lock()
	if denied {
		m.darkStreak[key]++
	} else {
		delete(m.darkStreak, key)
	}
	n := m.darkStreak[key]
	m.mu.Unlock()

	if !denied || n < ContractDarkStreak {
		return
	}
	// Kill switches BEFORE the probe: a silenced alert must cost zero reads.
	if m.cfg.AlertsDisabled || darkAlertDisabled() {
		return
	}
	// Dedupe BEFORE the probe: a lane that already alerted this window must not
	// pay for a database read on every subsequent tick.
	alertKey := "contract_dark:" + lane
	if !m.alertDue(alertKey) {
		return
	}

	ready, probeErr := m.laneClaimableSupply(ctx, lane, pass)
	if probeErr == nil && ready == 0 {
		// Benign: the contract denied a lane that had nothing to send anyway.
		// This is the common case the incident is NOT.
		return
	}

	supply := fmt.Sprintf("%d+ record(s) the claim could have taken", ready)
	if probeErr != nil {
		// Fail OPEN. A probe that timed out is not evidence of empty supply,
		// and the conjunction that got us here (enforcing + contract-denied +
		// %d consecutive ticks) is already a strong enough signal to page on.
		supply = "UNKNOWN — supply probe failed: " + probeErr.Error()
	}

	m.alertOnce(ctx, alertKey, notify.TierAlert,
		fmt.Sprintf("%s is DARK under contract enforcement · %d ticks, 0 mailed", lane, n),
		"Pass: "+pass+
			"\nMode: "+string(m.cfg.Mode)+
			"\nReason: "+reason+
			"\nSupply: "+supply+
			"\nEffect: every wave for this lane is skipped before any record is claimed — the lane mails 0 while this holds",
		"Run: POST /api/mailing/supply/contracts/dispatch/"+lane+" (or set DRIP_SUPPLY_CHAIN_MODE=shadow to roll back)")
}

// alertDue reports whether alertOnce would deliver for `key` right now WITHOUT
// stamping the window.
//
// It exists purely so the supply probe stays behind the dedupe. The check is
// then repeated inside alertOnce, which is what actually stamps: the worst a
// race between the two can cost is one extra LIMIT-1 probe, never a duplicate
// alert.
func (m *Mediator) alertDue(key string) bool {
	if m == nil || m.cfg.AlertsDisabled {
		return false
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	last, seen := m.lastAlert[key]
	return !seen || now.Sub(last) >= m.cfg.AlertEvery
}

// introSupplyProbeSQL mirrors the WHERE of the intro claim
// (Transitions.ClaimByISPCaps, transition.go:308) minus the per-ISP cap
// arithmetic: status='ready' for this lane, excluding emergency-paused
// datasets. `vertical` IS the lane — the same column planner.readFresh groups
// on (planner.go:1794).
const introSupplyProbeSQL = `
SELECT COUNT(*) FROM (
	SELECT 1
	  FROM partner_clean_queue
	 WHERE vertical = $1
	   AND status = 'ready'` + datasetNotEmergencyPausedSQL + `
	 LIMIT $2
) s`

// followupSupplyProbeSQL mirrors the follow-up claim's due predicate
// (partner_drip_orchestrator.go:5390). The per-vertical engagement-exit clause
// is deliberately NOT ported: it lives in the orchestrator, it varies by lane,
// and copying it here would be a second implementation that drifts. Its absence
// can only OVER-count, which biases this alert toward firing — the correct bias
// for a detector whose whole job is to stop being silent.
const followupSupplyProbeSQL = `
SELECT COUNT(*) FROM (
	SELECT 1
	  FROM partner_clean_queue
	 WHERE vertical = $1
	   AND status = 'mailed'
	   AND next_touch_at IS NOT NULL
	   AND next_touch_at <= NOW()
	   AND terminal_reason IS NULL` + datasetNotEmergencyPausedSQL + `
	 LIMIT $2
) s`

// laneClaimableSupply answers ONE question: does this lane hold records the
// claim could have taken, had the contract not denied the wave?
//
// It returns a count capped at darkSupplyProbeLimit, so callers may only test
// it against zero.
func (m *Mediator) laneClaimableSupply(ctx context.Context, lane, pass string) (int, error) {
	if m == nil || m.db == nil {
		return 0, errors.New("dripsupply: no database for the supply probe")
	}
	q := introSupplyProbeSQL
	if pass == PassFollowup {
		q = followupSupplyProbeSQL
	}
	pctx, cancel := context.WithTimeout(ctx, darkSupplyProbeTimeout)
	defer cancel()
	var n int
	if err := m.db.QueryRowContext(pctx, q, lane, darkSupplyProbeLimit).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
