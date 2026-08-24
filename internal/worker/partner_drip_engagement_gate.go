package worker

// ENGAGEMENT-GATED LADDER PROGRESSION (operator 2026-08-24, standing rule for
// the internal feeds): "Let's only allow openers and clickers to the next touch
// point. Let's isolate cold into a different bucket to where we can slice and
// use for our retargeting or activation campaigns. Google is smart enough to
// know if we're just batching and blasting hoping for engagement."
//
// BEFORE: every record whose next_touch_at elapsed received the next touch.
// Engagement (engaged_at, human CLICK only) was an EXIT — a clicker left the
// ladder. Non-engagers rode all touches to ladder_complete.
//
// AFTER, for gated verticals: a record advances to touch N+1 only if it OPENED
// or CLICKED touch N. Everything else is marked terminal_reason='cold_no_engagement'
// with cold_at + cold_touch and leaves the drip — staying IN partner_clean_queue,
// fully sliceable by (vertical, dataset_id, isp_family, mailed_brand, cold_touch,
// ingested_at) for retargeting/activation.
//
// engaged_at is deliberately NOT reused. Its EXIT meaning is load-bearing for
// every data-partner Activation/Churn metric (COUNT(*) FILTER (WHERE engaged_at
// IS NOT NULL)) — see partner_engagement_marker.go. This gate reads two new
// columns, last_open_at / last_click_at, written by the same marker.
//
// THE WINDOW IS THE TOUCH GAP. A touch is sent at next_touch_at -
// followupTouchGapHours, so "engaged with the touch we just sent" is exactly
// `last_open_at >= next_touch_at - 24h`. Measured lag from send (internal_auto,
// 7d): opens 87.3% @12h / 94.4% @24h; clicks 92.5% @12h / 96.6% @24h — so the
// existing 24h gap already captures ~95% of all signal with no added latency.
//
// MEASURED CONSEQUENCE at build time (2026-08-24): 6,559 of 90,784 live internal
// ladder rows (7.22%) carry an open or click. gmail_v1 0.33% and gmail_v2 0.32%
// lose their ladder almost entirely and become t1-only lanes. That is the
// intended trade — 0.3% engagement on ~25k delivered/day is the batch-and-blast
// signature the rule exists to stop.
//
// Kill switches (read per tick, no restart):
//   - PARTNER_DRIP_ENGAGEMENT_GATE_DISABLED=1  → gate off; ladder reverts byte-
//     for-byte to time-only progression. Rows already marked cold stay cold
//     (revive them with the reactivation sweep or by clearing terminal_reason).
//   - PARTNER_DRIP_ENGAGEMENT_GATE_VERTICALS=a,b  → override the gated set
//     (prefix match, lowercase). Default: the internal_auto_insurance family.
//   - PARTNER_DRIP_COLD_SWEEP_DISABLED=1 → stop marking cold (gate still
//     blocks progression; rows just sit due instead of being bucketed).
//   - PARTNER_DRIP_COLD_REVIVE_DISABLED=1 → a cold record that later engages
//     stays cold instead of re-entering the ladder.
//   - PARTNER_DRIP_COLD_GRACE_HOURS=N → extra hours past due before a
//     non-engager is bucketed (default 6).

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ColdTerminalReason is the terminal_reason stamped on a record that failed the
// engagement gate. It is a BUCKET, not a suppression: these rows are the
// retargeting/activation pool and must never be treated as unmailable.
const ColdTerminalReason = "cold_no_engagement"

// progressionSignalsStamped latches true once PartnerEngagementMarker has
// completed a progression-signal pass in THIS process.
//
// ORDERING HAZARD this exists to close: last_open_at/last_click_at are added by
// the boot migration as NULL on all ~11.2M rows. Between that migration and the
// marker's first pass, EVERY record looks un-engaged — so a cold sweep running
// in that window would bucket the entire internal ladder, engaged records
// included, and the damage is only undone by a manual re-open of the bucket.
// The sweeper therefore refuses to bucket until the marker has actually
// written signals at least once. Fail-CLOSED: an unset flag means "do not
// bucket", never "nothing engaged".
//
// Per-process, not per-estate: each of the 2 service instances gates its own
// sweeping, and each runs its own marker, so no cross-instance coordination is
// needed. Revive is NOT gated on this — reviving a cold record is always safe.
var progressionSignalsStamped atomic.Bool

// MarkProgressionSignalsReady latches the readiness flag. Called by the
// engagement marker after a successful progression-signal pass.
func MarkProgressionSignalsReady() { progressionSignalsStamped.Store(true) }

// defaultGatedVerticalPrefixes is the operator's "internal feeds" scope. Prefix
// match, so the whole internal_auto_insurance family (base + _v2.._v7 +
// _gmail_v1/_gmail_v2) is covered by one entry and any future _v8 inherits it.
var defaultGatedVerticalPrefixes = []string{"internal_auto_insurance"}

// defaultColdGraceHours is applied ON TOP of the 24h touch gap before a
// non-engager is bucketed, so the ~5% of opens/clicks that land after the gap
// still rescue the record.
const defaultColdGraceHours = 6

func engagementGateDisabled() bool {
	v := os.Getenv("PARTNER_DRIP_ENGAGEMENT_GATE_DISABLED")
	return v == "1" || v == "true"
}

func coldSweepDisabled() bool {
	v := os.Getenv("PARTNER_DRIP_COLD_SWEEP_DISABLED")
	return v == "1" || v == "true"
}

func coldReviveDisabled() bool {
	v := os.Getenv("PARTNER_DRIP_COLD_REVIVE_DISABLED")
	return v == "1" || v == "true"
}

func coldGraceHours() int {
	if raw := strings.TrimSpace(os.Getenv("PARTNER_DRIP_COLD_GRACE_HOURS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			return n
		}
	}
	return defaultColdGraceHours
}

// gatedVerticalPrefixes returns the lowercase prefixes whose verticals progress
// on engagement only. A non-empty env override REPLACES the default set.
//
// An empty / whitespace-only / all-commas value falls back to the DEFAULT scope
// rather than gating nothing. This is a standing operator rule, so there must be
// exactly one way to switch it off — PARTNER_DRIP_ENGAGEMENT_GATE_DISABLED. An
// accidentally-blanked variable (a templated deploy var that renders empty, an
// exported-but-unset shell var) must not silently revert the internal feeds to
// batch-and-blast.
func gatedVerticalPrefixes() []string {
	raw := os.Getenv("PARTNER_DRIP_ENGAGEMENT_GATE_VERTICALS")
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return defaultGatedVerticalPrefixes
	}
	return out
}

// VerticalEngagementGated reports whether `vertical` progresses on engagement
// only. False whenever the kill switch is set, so every caller reverts to
// time-only progression together.
func VerticalEngagementGated(vertical string) bool {
	if engagementGateDisabled() {
		return false
	}
	lv := strings.ToLower(strings.TrimSpace(vertical))
	if lv == "" {
		return false
	}
	for _, p := range gatedVerticalPrefixes() {
		if strings.HasPrefix(lv, p) {
			return true
		}
	}
	return false
}

// engagementGateSQL is the predicate a row must satisfy to earn its next touch:
// an open or a click landing at/after the moment its current touch was sent
// (next_touch_at - the touch gap). `alias` qualifies the columns ("" for an
// unaliased FROM). Returns "" when the vertical is not gated, so callers can
// concatenate it unconditionally and get byte-identical legacy SQL.
//
// NOT sargable on its own by design — every call site already narrows to a
// single (vertical, touch_count, next_touch_at<=NOW()) slice via
// idx_pcq_followup_due / idx_pcq_followup_isp, and this filters that slice.
func engagementGateSQL(vertical, alias string) string {
	if !VerticalEngagementGated(vertical) {
		return ""
	}
	q := ""
	if alias != "" {
		q = alias + "."
	}
	// next_touch_at is NOT NULL on every row these call sites select (they all
	// require next_touch_at <= NOW()), so the window start is well-defined.
	window := fmt.Sprintf("(%snext_touch_at - INTERVAL '%d hours')", q, followupTouchGapHours)
	return fmt.Sprintf(`
			  AND (
			        (%[1]slast_click_at IS NOT NULL AND %[1]slast_click_at >= %[2]s)
			     OR (%[1]slast_open_at  IS NOT NULL AND %[1]slast_open_at  >= %[2]s)
			  )`, q, window)
}

// engagementGateAnyVerticalSQL is the cross-vertical form of engagementGateSQL,
// for queries that scan every lane at once and therefore cannot be specialised
// per vertical. It passes a row through when the row's vertical is NOT gated,
// or when it is gated AND engaged within its touch window. Returns "" when the
// gate is off or scoped to nothing, keeping legacy SQL byte-identical.
//
// Uses only literals (prefixes are quoted inline), so it is safe to concatenate
// into a query with its own positional parameters without disturbing their
// numbering.
//
// ⚠️ The output CONTAINS '%' (the LIKE wildcards). Concatenate it into a query
// string passed straight to Query/Exec — NEVER into a fmt.Sprintf FORMAT string,
// which would read '%” as a verb and corrupt the SQL. (engagementGateSQL above
// is %-free and safe either way; this one is not.)
func engagementGateAnyVerticalSQL(alias string) string {
	if engagementGateDisabled() {
		return ""
	}
	prefixes := gatedVerticalPrefixes()
	if len(prefixes) == 0 {
		return ""
	}
	q := ""
	if alias != "" {
		q = alias + "."
	}
	clauses := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		clauses = append(clauses, fmt.Sprintf("LOWER(%svertical) LIKE %s", q, quoteSQLLiteral(p+"%")))
	}
	gated := "(" + strings.Join(clauses, " OR ") + ")"
	window := fmt.Sprintf("(%snext_touch_at - INTERVAL '%d hours')", q, followupTouchGapHours)
	return fmt.Sprintf(`
			  AND (
			        NOT %[1]s
			     OR (%[2]slast_click_at IS NOT NULL AND %[2]slast_click_at >= %[3]s)
			     OR (%[2]slast_open_at  IS NOT NULL AND %[2]slast_open_at  >= %[3]s)
			  )`, gated, q, window)
}

// quoteSQLLiteral single-quotes a string literal for inline SQL, doubling any
// embedded quote. Only ever fed operator-configured vertical prefixes.
func quoteSQLLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// ---------------------------------------------------------------------------
// Cold bucketing + reactivation sweep

// PartnerDripColdSweeper buckets non-engagers out of the gated ladders and
// revives cold records that engage later (an activation campaign landing a
// click is exactly the signal that should put someone back on the ladder —
// "positive engagement progresses you" applies from cold too).
//
// Both passes run in bounded SKIP LOCKED batches so they never contend with the
// orchestrator's claim path on the same rows.
type PartnerDripColdSweeper struct {
	db        *sql.DB
	interval  time.Duration
	batchSize int
	maxBatch  int
}

func NewPartnerDripColdSweeper(db *sql.DB) *PartnerDripColdSweeper {
	return &PartnerDripColdSweeper{
		db:        db,
		interval:  5 * time.Minute,
		batchSize: 5000,
		maxBatch:  10,
	}
}

// Start runs the sweep on a ticker until ctx is cancelled. Safe to double-fire:
// every statement is idempotent (the cold pass requires terminal_reason IS NULL,
// the revive pass requires terminal_reason = ColdTerminalReason), so a second
// instance racing the first simply finds nothing to do.
func (s *PartnerDripColdSweeper) Start(ctx context.Context) {
	if s.db == nil {
		log.Printf("[PartnerDripColdSweeper] disabled (db missing)")
		return
	}
	log.Printf("[PartnerDripColdSweeper] started interval=%s batch=%d max_batches=%d grace=%dh",
		s.interval, s.batchSize, s.maxBatch, coldGraceHours())

	select {
	case <-ctx.Done():
		return
	case <-time.After(90 * time.Second):
	}
	s.sweepOnce(ctx)

	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[PartnerDripColdSweeper] stopping")
			return
		case <-t.C:
			s.sweepOnce(ctx)
		}
	}
}

func (s *PartnerDripColdSweeper) sweepOnce(ctx context.Context) {
	if engagementGateDisabled() {
		return
	}
	prefixes := gatedVerticalPrefixes()
	if len(prefixes) == 0 {
		return
	}
	if !coldSweepDisabled() {
		// Fail-CLOSED on the boot-window hazard: until the marker has written
		// last_open_at/last_click_at at least once in this process, every row
		// reads as un-engaged and bucketing would cold the whole ladder.
		if !progressionSignalsStamped.Load() {
			log.Printf("[PartnerDripColdSweeper] cold bucketing HELD — engagement marker has not stamped progression signals yet this process (revive pass still runs)")
		} else if n := s.markCold(ctx, prefixes); n > 0 {
			log.Printf("[PartnerDripColdSweeper] bucketed %d records as %s", n, ColdTerminalReason)
		}
	}
	if !coldReviveDisabled() {
		if n := s.revive(ctx, prefixes); n > 0 {
			log.Printf("[PartnerDripColdSweeper] revived %d cold records that engaged", n)
		}
	}
}

// verticalPrefixPredicate builds an OR of prefix matches over `vertical`,
// appending each prefix as a bound parameter starting at $start.
func verticalPrefixPredicate(prefixes []string, start int) (string, []interface{}) {
	clauses := make([]string, 0, len(prefixes))
	args := make([]interface{}, 0, len(prefixes))
	for i, p := range prefixes {
		clauses = append(clauses, fmt.Sprintf("LOWER(vertical) LIKE $%d", start+i))
		args = append(args, p+"%")
	}
	return "(" + strings.Join(clauses, " OR ") + ")", args
}

// markCold buckets rows that are past due (plus grace) with no open or click
// since their last touch was sent. It mirrors engagementGateSQL's window
// exactly — a row is bucketed if and only if the gate would refuse it.
func (s *PartnerDripColdSweeper) markCold(ctx context.Context, prefixes []string) int64 {
	pred, args := verticalPrefixPredicate(prefixes, 1)
	next := len(args) + 1
	args = append(args, MaxTouchCount-1, coldGraceHours(), s.batchSize)

	q := fmt.Sprintf(`
		UPDATE partner_clean_queue q
		SET terminal_reason = '%[1]s',
		    cold_at         = NOW(),
		    cold_touch      = q.touch_count,
		    updated_at      = NOW()
		FROM (
			SELECT id FROM partner_clean_queue
			WHERE status = 'mailed'
			  AND %[2]s
			  AND terminal_reason IS NULL
			  AND engaged_at IS NULL
			  AND touch_count BETWEEN 1 AND $%[3]d
			  AND next_touch_at IS NOT NULL
			  AND next_touch_at <= NOW() - make_interval(hours => $%[4]d)
			  AND NOT (
			        (last_click_at IS NOT NULL AND last_click_at >= next_touch_at - INTERVAL '%[6]d hours')
			     OR (last_open_at  IS NOT NULL AND last_open_at  >= next_touch_at - INTERVAL '%[6]d hours')
			  )
			ORDER BY next_touch_at ASC
			LIMIT $%[5]d
			FOR UPDATE SKIP LOCKED
		) picked
		WHERE q.id = picked.id
	`, ColdTerminalReason, pred, next, next+1, next+2, followupTouchGapHours)

	return s.runBatched(ctx, "mark_cold", q, args)
}

// revive returns a cold record to the ladder when it engages later — the
// activation loop the cold bucket exists to feed. The record resumes at its
// current touch_count with next_touch_at = NOW() so the very next wave can
// claim it; cold_at/cold_touch are cleared so it is no longer in the bucket.
func (s *PartnerDripColdSweeper) revive(ctx context.Context, prefixes []string) int64 {
	pred, args := verticalPrefixPredicate(prefixes, 1)
	next := len(args) + 1
	args = append(args, ColdTerminalReason, s.batchSize)

	q := fmt.Sprintf(`
		UPDATE partner_clean_queue q
		SET terminal_reason = NULL,
		    cold_at         = NULL,
		    cold_touch      = NULL,
		    next_touch_at   = NOW(),
		    updated_at      = NOW()
		FROM (
			SELECT id FROM partner_clean_queue
			WHERE terminal_reason = $%[2]d
			  AND %[1]s
			  AND status = 'mailed'
			  AND engaged_at IS NULL
			  AND touch_count BETWEEN 1 AND %[4]d
			  AND cold_at IS NOT NULL
			  AND (
			        (last_click_at IS NOT NULL AND last_click_at >= cold_at)
			     OR (last_open_at  IS NOT NULL AND last_open_at  >= cold_at)
			  )
			ORDER BY cold_at ASC
			LIMIT $%[3]d
			FOR UPDATE SKIP LOCKED
		) picked
		WHERE q.id = picked.id
	`, pred, next, next+1, MaxTouchCount-1)

	return s.runBatched(ctx, "revive", q, args)
}

// runBatched executes q up to maxBatch times, stopping early when a pass
// affects fewer rows than the batch size (queue drained) or the context ends.
func (s *PartnerDripColdSweeper) runBatched(ctx context.Context, label, q string, args []interface{}) int64 {
	var total int64
	for i := 0; i < s.maxBatch; i++ {
		if ctx.Err() != nil {
			return total
		}
		res, err := s.db.ExecContext(ctx, q, args...)
		if err != nil {
			log.Printf("[PartnerDripColdSweeper] %s batch %d failed: %v", label, i, err)
			return total
		}
		n, _ := res.RowsAffected()
		total += n
		if n < int64(s.batchSize) {
			return total
		}
	}
	return total
}
