package worker

// CAP-BREACH DETECTOR + AUTOMATIC SHUTOFF (operator 2026-08-20: "make sure we
// are not sending over caps — have a defined way of shutting off a given drip
// if it exceeds the cap by more than 50% of its intention").
//
// ⚠️ SCOPE — this is an EXPLICIT, NARROW operator-authorised exception to JAOS
// core §1.8 ("no autonomous remediation"). The ONLY autonomous action this code
// may ever take is: set hold=TRUE on ONE (brand × ISP) cell of
// partner_drip_brand_budgets that has an explicitly declared, positive
// daily_budget and has already introduced more than the threshold multiple of
// it today. It never edits a budget, never cancels a campaign, never touches a
// dataset cap, never holds the estate, and never widens its own scope. Anything
// beyond that one write remains operator-executed.
//
// ── 1. INTENTION ────────────────────────────────────────────────────────────
// "Intended" = partner_drip_brand_budgets.daily_budget for a cell whose ROW
// EXISTS and whose budget is > 0. Two deliberate exclusions:
//
//   - NO ROW = UNGOVERNED, never a breach. The driving table of
//     capBreachCandidatesSQL is the ledger itself and the counters are LEFT
//     JOINed onto it, so a counter can never manufacture a governed cell. This
//     is the whole reason the lane cap (partner_isp_distribution_overrides.
//     daily_cap, dataset × ISP) is NOT the intention used here: absence of that
//     row means "falls through to the global NewRecordDailyISPCaps default",
//     not "intended zero" (docs/JAOS/drip-lanes.md §2.1), and a detector that
//     read a missing row as 0 would trip a shutoff on every ungoverned lane.
//   - daily_budget = 0 is EXCLUDED. 1.5 × 0 = 0 makes any single stray record a
//     "breach", and there is nothing for a shutoff to add: applyBrandIntroBudgets
//     (partner_drip_brand_budgets.go) already forces the per-wave cap to 0 for a
//     zero/held cell with no DB round trip. A zero cell is already shut off.
//
// The judgment value is the LIVE daily_budget — the exact number
// loadBrandBudgets/applyBrandIntroBudgets clamped against today — not
// property_budget_versions. This detector measures ENFORCEMENT failure, so it
// must be judged against what enforcement enforced. (I-2 promotion happens at
// the Denver boundary, so the live value is constant across the day.)
//
// ── 2. ACTUAL, and the per-wave-overshoot problem ───────────────────────────
// "Actual" = property_intro_counters.introduced for today's Denver day —
// first-touch introductions per (brand, ISP), already materialised every 10
// minutes by PropertyIntroRollupWorker off idx_pcq_intro_rollup. The detector
// therefore adds ZERO incremental load to the ~11.2M-row partner_clean_queue:
// it rides a counter table that is at most 16 × 14 rows per day. A counter cell
// that is ABSENT is skipped, never read as 0 (the rollup writes explicit zeros
// for the full grid, so absence really is absence).
//
// Overshoot past a cap is NORMAL in small amounts and must not trip a shutoff.
// applyBrandIntroBudgets clamps a wave to `daily_budget - introduced-so-far`,
// so a single wave cannot exceed the budget; real overshoot comes from
// (a) concurrent waves for the same brand each reading the same spend before
// either stamps mailed_at, and (b) mailed_at stamp lag/loss (see
// partner_drip_stamp_failures). Both are bounded by roughly one per-wave
// per-ISP cap per concurrent wave — PerISPCapPerWave tops out at 100 for the
// budgeted ISPs (partner_drip_orchestrator.go:587-600, att=100).
//
// So a breach requires BOTH tests to pass:
//
//	ratio      actual * 100 > intended * capBreachThresholdPct   (default 150 → strictly more than +50%)
//	abs. floor actual - intended > capBreachMinExcess             (default 100 → one wave of legitimate granularity)
//
// Integer arithmetic only; no floats. At intended=100 the ratio test fires at
// 151 and NOT at 150 ("more than 50%"), and at 140 (1.4×) it does not fire.
//
// ── 3. SHUTOFF — an EXISTING lever, reversible and audited ──────────────────
// The action is hold=TRUE on that one ledger cell. Blast radius = exactly the
// breach's scope: applyBrandIntroBudgets zeroes only that (brand, ISP) welcome
// cap. daily_budget is NEVER written, so the operator's intended value survives
// untouched and un-holding restores the prior behaviour exactly — there is no
// "value before" to stash. The trip is recorded as a half-open
// property_hold_intervals row (I-3/I-4) carrying reason (the numbers),
// changed_by = capBreachActor and held_from, plus a best-effort
// partner_admin_audit_log row. If the interval row cannot be written the whole
// trip is rolled back: NO AUDIT ROW, NO ACTION.
//
// Un-do is the existing operator path: Property Ledger → hold=false + reason
// (POST …/property-ledger/update). Nothing new to learn.
//
// ── 4. IDEMPOTENCY / RESTART SAFETY / NOT FIGHTING A HUMAN ──────────────────
// Dedup key = (brand, isp, Denver day, changed_by=capBreachActor). The detector
// trips a cell AT MOST ONCE PER DENVER DAY. Consequences:
//   - re-fires every 10-minute tick and every ECS bounce are no-ops;
//   - a human who un-holds a still-over-cap cell is NOT fought — the closed
//     same-day interval is still the dedup record, so the detector stands down
//     for the rest of that Denver day;
//   - a cell already held (by anyone) is skipped;
//   - the authoritative check runs INSIDE the tx after SELECT … FOR UPDATE on
//     the ledger row (I-10), so a double-fire from two processes is safe even
//     without the distlock the pass already holds.
// All state lives in Postgres; nothing is remembered in memory across restarts.
//
// ── 5. KILL SWITCH (fail-OPEN, matching the existing pattern) ───────────────
//   PROPERTY_CAP_BREACH_SHUTOFF_DISABLED=1  detector does nothing (today's
//                                           behaviour: no autonomous shutoff)
//   PROPERTY_CAP_BREACH_DETECT_ONLY=1       detect + log + heartbeat, NO write
//   PROPERTY_CAP_BREACH_PCT=<int ≥100>      ratio threshold, default 150
//   PROPERTY_CAP_BREACH_MIN_EXCESS=<int ≥0> absolute floor, default 100
// No deploy is needed to disable or retune. Every mode is reported in the
// property_cap_breach heartbeat AND in the ledger list API (`cap_breach`
// block), so a disabled/detect-only/ungoverned detector is VISIBLE rather than
// silently inert.
//
// ── 6. NO NEW SCHEMA ────────────────────────────────────────────────────────
// Every table used already exists and is already written by shipped code:
// partner_drip_brand_budgets, property_intro_counters, property_hold_intervals,
// partner_admin_audit_log, mailing_worker_heartbeats. Nothing is added to
// criticalSendPathDDL, runStartupMigrations or concurrentIndexSpecs.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// CapBreachActor is the changed_by / updated_by stamp on every automatic
	// shutoff. It is BOTH the audit signature and half the dedup key — the
	// portal keys its "auto-held" badge off it. Never reuse it for anything a
	// human does.
	CapBreachActor = "cap-breach-detector"

	// capBreachWorkerName is the heartbeat identity (mailing_worker_heartbeats)
	// so an operator can see the detector is alive, armed, and what it did.
	capBreachWorkerName = "property_cap_breach"

	// defaultCapBreachPct — the operator's rule: shut off above 150% of
	// intention ("exceeds the cap by more than 50% of its intention").
	defaultCapBreachPct = 150

	// defaultCapBreachMinExcess — the per-wave-overshoot tolerance in absolute
	// records. See §2: legitimate overshoot is bounded by roughly one per-wave
	// per-ISP cap, and PerISPCapPerWave tops out at 100 for the budgeted ISPs.
	defaultCapBreachMinExcess = 100

	// capBreachMaxTripsPerPass bounds one pass's autonomous writes. A pass that
	// wants to trip more cells than this is not a lane runaway — it is a
	// systemic fault (a counter regression, a budget wipe), and mass-holding the
	// estate on a bad reading is exactly what §1.8 forbids. The pass holds
	// NOTHING and reports 'error' so a human looks.
	capBreachMaxTripsPerPass = 6
)

// CapBreachDisabled is the one-move kill switch (fail-OPEN: set = no
// autonomous shutoff, i.e. behaviour identical to before this shipped).
func CapBreachDisabled() bool { return envFlagOn("PROPERTY_CAP_BREACH_SHUTOFF_DISABLED") }

// CapBreachDetectOnly runs the full detection and reports it, but performs no
// write. Intended for the first deploy so the lever can be observed armed
// before it is allowed to act — NOT a permanent resting state.
func CapBreachDetectOnly() bool { return envFlagOn("PROPERTY_CAP_BREACH_DETECT_ONLY") }

func envFlagOn(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// CapBreachThresholdPct is the ratio threshold in percent. Floored at 100: the
// detector can never be tuned to trip a lane that is at or under its intention.
func CapBreachThresholdPct() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("PROPERTY_CAP_BREACH_PCT"))); err == nil && v >= 100 {
		return v
	}
	return defaultCapBreachPct
}

// CapBreachMinExcess is the absolute per-wave-overshoot tolerance in records.
func CapBreachMinExcess() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("PROPERTY_CAP_BREACH_MIN_EXCESS"))); err == nil && v >= 0 {
		return v
	}
	return defaultCapBreachMinExcess
}

// capBreachCandidate is one (brand × ISP) ledger cell as the detector sees it.
// Intended and Actual are pointers on purpose: nil is "no such thing", which is
// a DIFFERENT state from zero and must never be evaluated as one.
type capBreachCandidate struct {
	Brand string
	ISP   string
	// Intended = daily_budget. nil = NO ledger row for this cell → UNGOVERNED.
	Intended *int
	// Actual = today's introductions. nil = counter cell ABSENT (not zero).
	Actual *int
	// Hold = the cell is already stopped (by anyone).
	Hold bool
	// LockVersion is the CAS token for the ledger write (I-10).
	LockVersion int64
	// AutoTrippedToday = this detector already tripped this cell this Denver
	// day (open OR closed interval). Once per cell per day, full stop.
	AutoTrippedToday bool
}

// capBreachVerdict is the decision for one cell. Skip carries WHY, so a pass
// that trips nothing can still explain itself.
type capBreachVerdict struct {
	Breach bool
	Reason string
}

// evalCapBreach is the WHOLE decision. Pure, total, integer-only — every gate
// this feature claims is proven here, not in SQL (property_cap_breach_test.go).
func evalCapBreach(c capBreachCandidate, thresholdPct, minExcess int) capBreachVerdict {
	if c.Intended == nil {
		return capBreachVerdict{false, "ungoverned: no ledger row (absence of a row is absence of an intention, never an intention of zero)"}
	}
	if *c.Intended <= 0 {
		return capBreachVerdict{false, "daily_budget <= 0: already hard-suppressed by applyBrandIntroBudgets; no ratio is meaningful against 0"}
	}
	if c.Actual == nil {
		return capBreachVerdict{false, "no counter cell for today: ABSENT, not zero"}
	}
	if c.Hold {
		return capBreachVerdict{false, "already held"}
	}
	if c.AutoTrippedToday {
		return capBreachVerdict{false, "already auto-tripped this Denver day (one trip per cell per day; a human un-hold is not fought)"}
	}
	intended, actual := *c.Intended, *c.Actual
	if actual <= intended {
		return capBreachVerdict{false, fmt.Sprintf("within cap: %d of %d", actual, intended)}
	}
	// Ratio test — integer, no floats. Strictly greater: at exactly the
	// threshold multiple this is NOT a breach ("more than 50%").
	if int64(actual)*100 <= int64(intended)*int64(thresholdPct) {
		return capBreachVerdict{false, fmt.Sprintf("over cap but within threshold: %d of %d (%s of intended, threshold %d%%)",
			actual, intended, capBreachPctString(actual, intended), thresholdPct)}
	}
	// Absolute floor — the per-wave-overshoot tolerance.
	if actual-intended <= minExcess {
		return capBreachVerdict{false, fmt.Sprintf("over threshold by ratio but excess %d <= min_excess %d (normal per-wave overshoot, not a runaway)",
			actual-intended, minExcess)}
	}
	return capBreachVerdict{true, fmt.Sprintf("cap breach: %d introduced vs daily_budget %d (%s of intended; threshold %d%%, min excess %d, actual excess %d)",
		actual, intended, capBreachPctString(actual, intended), thresholdPct, minExcess, actual-intended)}
}

// capBreachPctString renders actual/intended as a percentage for humans.
func capBreachPctString(actual, intended int) string {
	if intended <= 0 {
		return "n/a"
	}
	return strconv.Itoa(int(int64(actual)*100/int64(intended))) + "%"
}

// capBreachCandidatesSQL loads every GOVERNED cell with today's counter.
//
// The driving table is partner_drip_brand_budgets and the counters are LEFT
// JOINed onto it — that is the structural guarantee that a counter row can
// never manufacture a governed cell, i.e. an UNGOVERNED (no-row) lane is not in
// this result set at all. Cost: the ledger is at most 16 brands × 14 ledger ISP
// groups; property_intro_counters is keyed (day, brand, isp). partner_clean_queue
// is NOT touched.
//
// $1 = Denver date (YYYY-MM-DD), $2 = CapBreachActor, $3 = Denver midnight (UTC instant).
const capBreachCandidatesSQL = `
	SELECT b.brand, b.isp, b.daily_budget, b.hold, b.lock_version,
	       c.introduced,
	       EXISTS (
	           SELECT 1 FROM property_hold_intervals h
	           WHERE lower(btrim(h.brand)) = lower(btrim(b.brand))
	             AND lower(btrim(h.isp))   = lower(btrim(b.isp))
	             AND h.changed_by = $2
	             AND h.held_from >= $3
	       ) AS auto_tripped_today
	FROM partner_drip_brand_budgets b
	LEFT JOIN property_intro_counters c
	       ON c.day = $1::date
	      AND lower(btrim(c.brand)) = lower(btrim(b.brand))
	      AND lower(btrim(c.isp))   = lower(btrim(b.isp))
	ORDER BY b.brand, b.isp`

// capBreachReportBlocked emits the detector's heartbeat for a pass that could
// not run because the counters it rides were not produced. This exists so the
// detector can NEVER fail silent: a blocked counter pass shows up as a blocked
// detector, not as "no breaches found".
func (w *PropertyIntroRollupWorker) capBreachReportBlocked(ctx context.Context, detail string) {
	log.Printf("[CapBreach] BLOCKED — %s. No cap-breach judgment this pass (counters are the ONLY input; a missing counter is never read as zero).", detail)
	EmitHeartbeat(ctx, w.db, capBreachWorkerName, int(w.interval.Seconds()), "blocked", detail)
}

// RunCapBreachDetector is one full detection pass over today's Denver day.
// Called at the end of PropertyIntroRollupWorker.RunOnce, immediately after
// today's counters were recomputed in the same leased pass — so the input is
// fresh by construction and the whole thing runs under the pass's distlock.
// Exported so tests and operational tooling can drive a pass directly; the
// caller owns locking.
func (w *PropertyIntroRollupWorker) RunCapBreachDetector(ctx context.Context, today time.Time) {
	if ctx.Err() != nil {
		return
	}
	thresholdPct, minExcess := CapBreachThresholdPct(), CapBreachMinExcess()
	mode := fmt.Sprintf("pct=%d min_excess=%d", thresholdPct, minExcess)

	if CapBreachDisabled() {
		log.Printf("[CapBreach] DISABLED (PROPERTY_CAP_BREACH_SHUTOFF_DISABLED) — no detection, no shutoff.")
		EmitHeartbeat(ctx, w.db, capBreachWorkerName, int(w.interval.Seconds()), "disabled", "PROPERTY_CAP_BREACH_SHUTOFF_DISABLED set")
		return
	}
	detectOnly := CapBreachDetectOnly()

	dayStart, _ := denverDayWindowUTC(today, w.loc)
	cands, err := w.loadCapBreachCandidates(ctx, today, dayStart)
	if err != nil {
		log.Printf("[CapBreach] candidate load failed: %v — no judgment this pass", err)
		EmitHeartbeat(ctx, w.db, capBreachWorkerName, int(w.interval.Seconds()), "error", "candidate load: "+err.Error())
		return
	}
	if len(cands) == 0 {
		// LOUD on purpose: an empty ledger means this gate governs NOTHING.
		log.Printf("[CapBreach] ledger has 0 (brand × ISP) cells — the cap-breach shutoff governs NOTHING today. Seed partner_drip_brand_budgets to arm it. (%s)", mode)
		EmitHeartbeat(ctx, w.db, capBreachWorkerName, int(w.interval.Seconds()), "ok",
			"governed=0 — ledger empty, detector governs nothing ("+mode+")")
		return
	}

	breaches := make([]capBreachCandidate, 0, 4)
	reasons := make([]string, 0, 4)
	governed := 0
	for _, c := range cands {
		if c.Intended != nil && *c.Intended > 0 {
			governed++
		}
		v := evalCapBreach(c, thresholdPct, minExcess)
		if v.Breach {
			breaches = append(breaches, c)
			reasons = append(reasons, v.Reason)
			log.Printf("[CapBreach] BREACH %s/%s — %s", c.Brand, c.ISP, v.Reason)
		}
	}

	base := fmt.Sprintf("cells=%d governed=%d breached=%d (%s)", len(cands), governed, len(breaches), mode)
	if len(breaches) == 0 {
		EmitHeartbeat(ctx, w.db, capBreachWorkerName, int(w.interval.Seconds()), "ok", base)
		return
	}
	if len(breaches) > capBreachMaxTripsPerPass {
		log.Printf("[CapBreach] REFUSING TO ACT — %d cells breached in one pass (max %d). That is a systemic reading, not a lane runaway; holding NOTHING. Investigate the counter pass and the ledger before re-arming.",
			len(breaches), capBreachMaxTripsPerPass)
		EmitHeartbeat(ctx, w.db, capBreachWorkerName, int(w.interval.Seconds()), "error",
			fmt.Sprintf("%s — REFUSED: breaches exceed capBreachMaxTripsPerPass=%d, no cell held", base, capBreachMaxTripsPerPass))
		return
	}
	if detectOnly {
		for i, c := range breaches {
			log.Printf("[CapBreach] DETECT-ONLY — would hold %s/%s: %s", c.Brand, c.ISP, reasons[i])
		}
		EmitHeartbeat(ctx, w.db, capBreachWorkerName, int(w.interval.Seconds()), "detect-only",
			base+" — PROPERTY_CAP_BREACH_DETECT_ONLY set, NO cell held")
		return
	}

	held, failed := 0, 0
	for i, c := range breaches {
		ok, err := w.tripCapBreachHold(ctx, c, reasons[i], dayStart)
		switch {
		case err != nil:
			failed++
			log.Printf("[CapBreach] hold FAILED %s/%s: %v", c.Brand, c.ISP, err)
		case ok:
			held++
			log.Printf("[CapBreach] HELD %s/%s — %s. Reversible: Property Ledger → hold=false (daily_budget was NOT modified).", c.Brand, c.ISP, reasons[i])
		default:
			log.Printf("[CapBreach] hold skipped %s/%s — state changed between detection and write (already held, already tripped today, or lock_version moved)", c.Brand, c.ISP)
		}
	}
	status := "ok"
	if failed > 0 {
		status = "error"
	}
	EmitHeartbeat(ctx, w.db, capBreachWorkerName, int(w.interval.Seconds()), status,
		fmt.Sprintf("%s held=%d failed=%d", base, held, failed))
}

// loadCapBreachCandidates reads the governed grid + today's counters.
func (w *PropertyIntroRollupWorker) loadCapBreachCandidates(ctx context.Context, today, dayStart time.Time) ([]capBreachCandidate, error) {
	rows, err := w.db.QueryContext(ctx, capBreachCandidatesSQL, today.Format("2006-01-02"), CapBreachActor, dayStart)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []capBreachCandidate{}
	for rows.Next() {
		var c capBreachCandidate
		var budget, introduced sql.NullInt64
		if err := rows.Scan(&c.Brand, &c.ISP, &budget, &c.Hold, &c.LockVersion, &introduced, &c.AutoTrippedToday); err != nil {
			return nil, err
		}
		if budget.Valid {
			v := int(budget.Int64)
			c.Intended = &v
		}
		if introduced.Valid {
			v := int(introduced.Int64)
			c.Actual = &v
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// tripCapBreachHold performs the ONE autonomous write, in one transaction:
//
//	SELECT … FOR UPDATE on the ledger row      (I-10 — the contract, not an assumption)
//	re-check hold + same-day auto-trip          (authoritative; the read-side check is only a prefilter)
//	INSERT the hold interval                    (the audit record — must affect exactly 1 row)
//	UPDATE hold=TRUE with lock_version CAS      (daily_budget untouched)
//
// Returns (true, nil) when the cell was held, (false, nil) when the write was
// correctly skipped because state moved under us — both are success. Any error
// rolls the whole thing back: no hold without its audit row.
func (w *PropertyIntroRollupWorker) tripCapBreachHold(ctx context.Context, c capBreachCandidate, reason string, dayStart time.Time) (bool, error) {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	// Bounded lock wait: never join a lock queue on a saturated primary (the
	// 2026-08-20 216s ADD COLUMN barricade). Losing the lock is a skipped tick,
	// not a stuck one — the next pass retries in 10 minutes.
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '3s'; SET LOCAL statement_timeout = '10s'`); err != nil {
		return false, fmt.Errorf("session setup: %w", err)
	}

	var curHold bool
	var curLock int64
	var curBudget int
	err = tx.QueryRowContext(ctx, `
		SELECT daily_budget, hold, lock_version
		FROM partner_drip_brand_budgets
		WHERE brand = $1 AND isp = $2 FOR UPDATE`, c.Brand, c.ISP).Scan(&curBudget, &curHold, &curLock)
	if err == sql.ErrNoRows {
		return false, nil // cell deleted under us — ungoverned now, nothing to do
	}
	if err != nil {
		return false, fmt.Errorf("row lock: %w", err)
	}
	if curHold {
		return false, nil // someone (human or a racing pass) already stopped it
	}
	if curBudget <= 0 {
		return false, nil // budget zeroed under us — already hard-suppressed
	}

	// Authoritative once-per-Denver-day guard. This is what stops the detector
	// re-tripping a cell a human deliberately un-held while it is still, and
	// will remain, over cap for the rest of the day.
	var tripped bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM property_hold_intervals
			WHERE lower(btrim(brand)) = lower(btrim($1))
			  AND lower(btrim(isp))   = lower(btrim($2))
			  AND changed_by = $3
			  AND held_from >= $4
		)`, c.Brand, c.ISP, CapBreachActor, dayStart).Scan(&tripped); err != nil {
		return false, fmt.Errorf("same-day guard: %w", err)
	}
	if tripped {
		return false, nil
	}

	// Audit FIRST: one open interval per cell (uq_phi_one_open). If another
	// open interval already exists this affects 0 rows and we refuse to act —
	// no audit row, no action.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO property_hold_intervals (brand, isp, held_from, reason, changed_by)
		SELECT $1, $2, NOW(), $3, $4
		WHERE NOT EXISTS (
			SELECT 1 FROM property_hold_intervals
			WHERE brand = $1 AND isp = $2 AND held_to IS NULL)`,
		c.Brand, c.ISP, reason, CapBreachActor)
	if err != nil {
		return false, fmt.Errorf("hold interval: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, fmt.Errorf("hold interval not written (an open interval already exists) — refusing to hold %s/%s without an audit record", c.Brand, c.ISP)
	}

	// The one write. daily_budget is deliberately NOT in this SET list.
	res, err = tx.ExecContext(ctx, `
		UPDATE partner_drip_brand_budgets
		SET hold = TRUE, updated_by = $3, updated_at = NOW(),
		    lock_version = lock_version + 1
		WHERE brand = $1 AND isp = $2 AND lock_version = $4`,
		c.Brand, c.ISP, CapBreachActor, curLock)
	if err != nil {
		return false, fmt.Errorf("hold update: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, fmt.Errorf("lock_version conflict holding %s/%s", c.Brand, c.ISP)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}

	capBreachAuditLog(ctx, w.db, c, curBudget, curLock, reason)
	return true, nil
}

// capBreachAuditLog mirrors internal/api.writeAuditLog into the same
// partner_admin_audit_log table. Duplicated (not shared) because internal/api
// already imports internal/worker — importing back would be a cycle. Strictly
// best-effort: the durable audit record is the property_hold_intervals row
// written inside the trip transaction.
func capBreachAuditLog(ctx context.Context, db *sql.DB, c capBreachCandidate, budget int, lockVersion int64, reason string) {
	if db == nil {
		return
	}
	actual := -1
	if c.Actual != nil {
		actual = *c.Actual
	}
	before, _ := json.Marshal(map[string]interface{}{
		"hold": false, "daily_budget": budget, "lock_version": lockVersion,
	})
	after, _ := json.Marshal(map[string]interface{}{
		"hold": true, "daily_budget": budget, "lock_version": lockVersion + 1,
		"introduced_today": actual, "reason": reason,
		"threshold_pct": CapBreachThresholdPct(), "min_excess": CapBreachMinExcess(),
		"undo": "Property Ledger → hold=false (daily_budget was never modified)",
	})
	if _, err := db.ExecContext(ctx, `
		INSERT INTO partner_admin_audit_log (actor, action, target_type, target_id, before_state, after_state)
		VALUES ($1, $2, $3, $4, NULLIF($5, 'null')::jsonb, NULLIF($6, 'null')::jsonb)`,
		CapBreachActor, "property_ledger_cap_breach_hold", "property_ledger_cell",
		c.Brand+"/"+c.ISP, string(before), string(after)); err != nil {
		log.Printf("[CapBreach] audit-log write failed for %s/%s (%v) — the hold interval remains the durable record", c.Brand, c.ISP, err)
	}
}
