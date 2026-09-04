package api

// Per-domain engagement engine — SA-2 (audience finalizer integration).
//
// This file holds the SQL fragments and bulk-load helpers used by
// planPMTAAudience to enforce the new per-(subscriber, sending_domain)
// state machine when selecting recipients:
//
//   - Excludes subscribers already mailed by THIS sending_domain in the
//     last 20h (4-hour buffer for slot stagger so today's send isn't
//     blocked by yesterday's same-slot send that ran a few minutes late).
//   - Excludes subscribers in state='cold' or 'suppressed' for THIS
//     sending_domain.
//   - Prioritizes selection by:
//       1. cross_engaged subscribers FIRST (proven inbox placement
//          across the portfolio)
//       2. state='engaged' for this domain SECOND
//       3. state='probe' (no SDS row, or row with no signal yet) LAST
//     Within each tier, more recent engagement wins.
//
// Two integration surfaces:
//
//   1. SQL injection (preferred) via buildSDSEligibilityClause — used
//      where the candidate-selection query joins mailing_subscribers
//      directly so we can splice a LEFT JOIN on
//      mailing_subscriber_domain_state without restructuring the SELECT
//      list. Currently wired into streamList.
//
//   2. In-memory filter — populated from a single bulk pre-load at the
//      top of planPMTAAudience (loadSDSStateForDomain +
//      loadCrossEngagedSet). Used by streamSegment which selects from
//      mailing_segment_members and could not absorb the JOIN without
//      breaking the query shape pinned by existing hybrid tests. The
//      in-memory filter also acts as a defense-in-depth check for the
//      SQL-injected path.
//
// KILL SWITCH: setting DISABLE_SDS_FREQUENCY_CAP=true reverts every
// fragment and bulk loader to a no-op so the planner behaves exactly
// as before SA-2. This is the rollout safety net documented in the
// spec at .scratch/PER_DOMAIN_ENGAGEMENT_ENGINE_SPEC.md §3 (SA-2).

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// sdsFilterEnabled returns false when DISABLE_SDS_FREQUENCY_CAP=true
// (case-insensitive). Any other value — including unset, empty, "false",
// "0", "no" — means the filter is enabled.
func sdsFilterEnabled() bool {
	return strings.ToLower(strings.TrimSpace(os.Getenv("DISABLE_SDS_FREQUENCY_CAP"))) != "true"
}

// sdsClause is the fragment set produced by buildSDSEligibilityClause.
// Splice each non-empty field into the candidate-selection query in this
// order: existing JOINs → Join → existing WHERE → Where → OrderBy. Append
// BindArgs to the existing args slice in the same order.
type sdsClause struct {
	Join     string        // empty when filter is disabled
	Where    string        // empty when filter is disabled
	OrderBy  string        // legacy fallback when filter is disabled
	BindArgs []interface{} // empty when filter is disabled
}

// buildSDSEligibilityClause returns SQL fragments to inject into a
// candidate selection query for the given sending_domain.
//
// The caller supplies:
//   - sendingDomain: the lower-cased domain the campaign is sending from.
//     Empty disables the filter (legacy behaviour).
//   - subscriberAlias: the SQL alias the caller has already given to
//     mailing_subscribers. The injected clauses use it as
//     "<alias>.id" / "<alias>.cross_engaged".
//   - baseBindCount: the count of bind placeholders the caller has
//     already declared (e.g. 1 if the original query uses $1). The
//     sending_domain bind appears at $(baseBindCount+1).
//
// The introduced alias for mailing_subscriber_domain_state is
// "sds_filter" — chosen so it does not collide with the master-selection
// path's "sds" alias. If you change it here, update the integration
// regression test in audience_finalizer_sds_test.go.
// `policy` is the per-ISP minimum touch gap for THIS sending domain
// (isp_touch_policy.go). It is applied INDEPENDENTLY of the kill switch: the
// switch governs the SA-2 state exclusion and ordering, the policy governs
// frequency alone. That separation is deliberate — it lets one ISP be capped
// without turning on three behaviours for every ISP (operator 2026-09-04,
// "I just want this to be for Gmail only"). Pass a nil/empty policy for the
// pre-policy behaviour.
func buildSDSEligibilityClause(sendingDomain string, subscriberAlias string, baseBindCount int, policy ispTouchPolicy) sdsClause {
	domainKnown := strings.TrimSpace(sendingDomain) != ""
	gapWhere := ""
	if domainKnown {
		gapWhere = policy.touchGapWhere(subscriberAlias, "sds_filter")
	}
	if !sdsFilterEnabled() || !domainKnown {
		// Kill switch on (or no domain): no state exclusion, no reordering.
		// A touch policy still binds, and needs the JOIN to read
		// last_mailed_at, so emit the minimal join-and-where form.
		if gapWhere == "" {
			// Safe deterministic fallback. Many candidate-selection queries
			// historically had no ORDER BY (heap order). ORDER BY <alias>.id
			// is cheap when the PK index is present and gives reproducible
			// behaviour across runs.
			return sdsClause{
				OrderBy: "ORDER BY " + subscriberAlias + ".id",
			}
		}
		bindIdx := baseBindCount + 1
		return sdsClause{
			Join: fmt.Sprintf(
				"LEFT JOIN mailing_subscriber_domain_state sds_filter ON sds_filter.subscriber_id = %s.id AND sds_filter.sending_domain = $%d",
				subscriberAlias, bindIdx,
			),
			Where:    strings.TrimSpace(gapWhere),
			OrderBy:  "ORDER BY " + subscriberAlias + ".id",
			BindArgs: []interface{}{strings.ToLower(strings.TrimSpace(sendingDomain))},
		}
	}
	bindIdx := baseBindCount + 1
	join := fmt.Sprintf(
		"LEFT JOIN mailing_subscriber_domain_state sds_filter ON sds_filter.subscriber_id = %s.id AND sds_filter.sending_domain = $%d",
		subscriberAlias, bindIdx,
	)
	where := "AND (sds_filter.state IS NULL OR sds_filter.state IN ('probe','engaged')) " +
		"AND (sds_filter.last_mailed_at IS NULL OR sds_filter.last_mailed_at < NOW() - INTERVAL '20 hours')" +
		gapWhere
	orderBy := fmt.Sprintf(
		"ORDER BY %s.cross_engaged DESC NULLS LAST, "+
			"CASE COALESCE(sds_filter.state, 'probe') WHEN 'engaged' THEN 1 WHEN 'probe' THEN 2 ELSE 3 END ASC, "+
			"GREATEST(COALESCE(sds_filter.last_open_at, 'epoch'::timestamptz), COALESCE(sds_filter.last_click_at, 'epoch'::timestamptz)) DESC NULLS LAST, "+
			"%s.id",
		subscriberAlias, subscriberAlias,
	)
	return sdsClause{
		Join:     join,
		Where:    where,
		OrderBy:  orderBy,
		BindArgs: []interface{}{strings.ToLower(strings.TrimSpace(sendingDomain))},
	}
}

// sdsRow is the per-(subscriber, sending_domain) snapshot loaded once
// per finalize. Only the fields the in-memory filter needs are
// retained — total counters and warmup_status are intentionally absent
// because the new state column already encodes the relevant signal.
type sdsRow struct {
	state        string
	lastMailedAt sql.NullTime
	lastOpenAt   sql.NullTime
	lastClickAt  sql.NullTime
}

// loadSDSStateForDomain bulk-loads the SDS state map for one sending
// domain. Returns an empty map (never nil) when:
//   - the kill switch is set
//   - sendingDomain is empty
//   - the query errors (logged at WARNING; planner continues without
//     the in-memory filter)
//
// The soft-fail behaviour is intentional: the goal is to never block a
// finalize because the new SDS-state read failed. The SQL-injected
// list-path filter remains active independently.
// needState is true when the caller has a touch policy to enforce. The policy
// reads last_mailed_at from this same map, and it binds while the SA-2 kill
// switch is on, so the switch alone must not decide whether the map is loaded.
func loadSDSStateForDomain(ctx context.Context, db dbQuerier, sendingDomain string, needState ...bool) map[string]sdsRow {
	out := make(map[string]sdsRow)
	forced := len(needState) > 0 && needState[0]
	if !sdsFilterEnabled() && !forced {
		return out
	}
	d := strings.ToLower(strings.TrimSpace(sendingDomain))
	if d == "" {
		return out
	}
	rows, err := db.QueryContext(ctx, `
		SELECT subscriber_id::text, state, last_mailed_at, last_open_at, last_click_at
		FROM mailing_subscriber_domain_state
		WHERE sending_domain = $1
	`, d)
	if err != nil {
		log.Printf("[finalizer/sds] WARNING: bulk SDS load for domain=%q failed: %v — proceeding without in-memory state filter", d, err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var state sql.NullString
		var lm, lo, lc sql.NullTime
		if err := rows.Scan(&id, &state, &lm, &lo, &lc); err != nil {
			continue
		}
		s := "probe"
		if state.Valid && state.String != "" {
			s = state.String
		}
		out[id] = sdsRow{state: s, lastMailedAt: lm, lastOpenAt: lo, lastClickAt: lc}
	}
	return out
}

// loadCrossEngagedSet bulk-loads the set of subscriber IDs flagged
// cross_engaged=true on mailing_subscribers. Same soft-fail contract as
// loadSDSStateForDomain. The set is expected to be small (subscribers
// engaged on >= 2 distinct sending domains) so an in-memory map is
// efficient even for the largest tenants.
func loadCrossEngagedSet(ctx context.Context, db dbQuerier) map[string]bool {
	out := make(map[string]bool)
	if !sdsFilterEnabled() {
		return out
	}
	rows, err := db.QueryContext(ctx, `SELECT id::text FROM mailing_subscribers WHERE cross_engaged = true`)
	if err != nil {
		log.Printf("[finalizer/sds] WARNING: cross_engaged load failed: %v — proceeding without cross-engaged prioritization", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// stateRank turns a state string into the ordinal used by the
// post-streaming priority sort. Lower = higher priority.
//
//	1 = engaged   (proven engagement on this domain)
//	2 = probe     (no SDS row, or fresh row with no signal yet)
//	3 = cold      (defensive — these are filtered out upstream)
//	4 = suppressed (defensive — also filtered out upstream)
func sdsStateRank(state string) int {
	switch state {
	case "engaged":
		return 1
	case "probe":
		return 2
	case "cold":
		return 3
	case "suppressed":
		return 4
	default:
		return 2
	}
}

// sdsLastEngagedAt returns max(last_open_at, last_click_at) for sort
// stability. Zero when neither timestamp is set.
func sdsLastEngagedAt(r sdsRow) time.Time {
	var t time.Time
	if r.lastOpenAt.Valid && r.lastOpenAt.Time.After(t) {
		t = r.lastOpenAt.Time
	}
	if r.lastClickAt.Valid && r.lastClickAt.Time.After(t) {
		t = r.lastClickAt.Time
	}
	return t
}
