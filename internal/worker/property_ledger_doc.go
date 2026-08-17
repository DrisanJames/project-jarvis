// Package worker — Property Ledger (Vector A) STATE-SEMANTICS INVARIANTS.
//
// This file is documentation only (no behavior). It carries §1 of
// tasks/platform-work/vector-a-property-ledger-plan.md (revision 4) verbatim so
// every builder/reviewer sees the contract next to the code. Every Property
// Ledger table, worker, and endpoint is built against these invariants.
//
// I-1 Operational day. An operational day is an America/Denver calendar date.
// Every table and worker that uses it stores the explicit UTC bounds
// (window_start_utc, window_end_utc), Go-computed. VDM data lives on UTC days
// and never mixes into budget judgments.
//
// I-2 Budget effectiveness (NEXT-DAY). An ordinary budget edit becomes
// effective at the next Denver day boundary. Mechanics: the edit writes
// pending_budget + pending_effective_day on the live row AND a
// property_budget_versions row keyed by effective_day; the counter worker's
// first tick on/after the boundary promotes pending→daily_budget (Step 14).
// Judgment for day D uses the single version with the greatest
// (effective_day, version) where effective_day <= D. Enforcement and judgment
// therefore agree by construction; max skew = one promotion cadence (≤10 min)
// + one orchestrator cache tick, disclosed in evidence. Operator-visible
// behavior (UI copy, Step 19): "Budget edits apply from tomorrow (Denver).
// Hold is immediate."
//
// I-3 Holds are IMMEDIATE and interval-tracked. Cell holds and the global hold
// apply to the live row/flag at once and are recorded as half-open intervals
// [held_from, held_to) (property_hold_intervals,
// property_ledger_flag_versions). A hold violation counts ONLY sends with
// mailed_at inside a hold interval — never sends from before the hold began.
//
// I-4 Half-open intervals everywhere. All interval predicates are [from, to):
// ts >= from AND (to IS NULL OR ts < to). CHECK to > from. One open interval
// per key, enforced by partial unique indexes — never by convention.
//
// I-5 Global hold is fail-CLOSED and outranks the overlay kill switch. Check
// order in enforcement: global hold FIRST, then
// PARTNER_DRIP_BRAND_BUDGETS_DISABLED. Unreadable flag at process start (no
// cached value yet) = TREAT AS HELD, loud log. Max propagation delay = one
// orchestrator tick (the cache reload cadence), stated in the flag UI.
//
// I-6 One ISP authority. isp.LedgerGroups() = AllGroups() ∪ {Other} (14
// static, reviewed values — see plan §0 evidence). Observed production strings
// NEVER auto-promote into control dimensions; the seed preflight ABORTS on any
// prod isp_family value outside the authority.
//
// I-7 Preliminary-data rule. Any telemetry status other than
// INSUFFICIENT_DATA requires EVERY expected contributing UTC cell to exist
// with complete = true AND finalized_at IS NOT NULL. One of two expected days
// present → INSUFFICIENT_DATA. Finalized VDM rows are IMMUTABLE and excluded
// from later fetch work-sets.
//
// I-8 Runs are first-class. Counter, VDM, and reconciliation passes each
// record a run row (expected/completed/status/error). A day is served/judged
// only from a COMPLETED run — a crash at 70/176 cells must never present as a
// handled day.
//
// I-9 Alerts are at-least-once. Outbox with states
// pending/sending/delivered/failed/suppressed, attempt counts, retry backoff,
// row locking; deterministic alert_uid in the message text for human dedupe.
// No "exactly-once" claims anywhere.
//
// I-10 Version writes lock the live row. Every writer of
// versions/intervals/proposals BEGINs by
// SELECT … FROM partner_drip_brand_budgets WHERE brand=$1 AND isp=$2
// FOR UPDATE — stated contract, not an implicit assumption. CAS is
// lock_version BIGINT (increment on every write; compare on update) — never
// timestamp equality.
//
// I-11 Negative fixtures are PERMANENT. Gate proofs are committed
// failing-fixture tests (a fixture that must produce the gated outcome), never
// "invert production logic, watch RED, restore" — temporary inversions on this
// shared tree risk being committed by a concurrent session.
package worker
