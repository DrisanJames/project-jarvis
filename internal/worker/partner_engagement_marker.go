package worker

import (
	"context"
	"database/sql"
	"log"
	"os"
	"time"
)

// humanVerdictSQL is the canonical HUMAN-verdict predicate over a
// mailing_tracking_events row aliased `te`. It MUST mirror
// agents/dbknowledge/_db.py HUMAN_VERDICT_PG byte-for-byte (modulo the te.
// column qualifier): ignite_verdict_is_human(ignite_event_verdict(user_agent,
// ip_address)). Do NOT invent a different human definition here — every
// consumer of "human?" on the platform (engagement KPIs, G3 scorecard, the
// partner rollup, this marker) must agree, or the numbers diverge silently.
const humanVerdictSQL = "ignite_verdict_is_human(ignite_event_verdict(te.user_agent, te.ip_address))"

// verdictEngagementMarkerDisabled reports whether the operator has flipped the
// REQ-036 kill switch that restores the pre-verdict (raw-click) marker
// behavior. Read per sweep (not once at boot) so an ECS env flip takes effect
// on the next tick without a restart.
//
// DISABLE_VERDICT_ENGAGEMENT_MARKER=true (or 1) → engaged_at stamps on ANY raw
// click again (the old scanner-contaminated behavior, measured 2-100x inflated
// per dataset in the 2026-07-13 G3 audit).
// DISABLE_MARKER_VERDICT_FILTER=true is honored as an alias — it is the switch
// name in the operator-approved REQ-036 assignment and backlog entry.
func verdictEngagementMarkerDisabled() bool {
	v := os.Getenv("DISABLE_VERDICT_ENGAGEMENT_MARKER")
	if v == "1" || v == "true" {
		return true
	}
	return os.Getenv("DISABLE_MARKER_VERDICT_FILTER") == "true"
}

// buildEngagementMarkSQL builds the engaged_at stamping statement.
// verdictFiltered=true (the default since REQ-036, operator-approved
// 2026-07-13) counts only HUMAN-verdict clicks: scanner/bot click detonations
// no longer stamp engaged_at, so scanner-clicked records stay in the drip and
// receive their full ladder — that consequence is intended and approved.
// verdictFiltered=false reproduces the original raw-click behavior exactly
// (kill switch path).
//
// Exported-shape note: kept as a pure function so the regression tests can pin
// BOTH generated statements without a live PG (the verdict functions
// ignite_event_verdict/ignite_verdict_is_human exist only in prod PG).
func buildEngagementMarkSQL(lookbackMins int, verdictFiltered bool) string {
	timeFilter := ""
	if lookbackMins > 0 {
		timeFilter = "AND te.event_at > NOW() - make_interval(mins => $1)"
	}
	verdictFilter := ""
	if verdictFiltered {
		verdictFilter = "AND " + humanVerdictSQL
	}
	return `
		UPDATE partner_clean_queue q
		SET engaged_at = e.first_click
		FROM (
			SELECT te.subscriber_id, c.partner_dataset_id, MIN(te.event_at) AS first_click
			FROM mailing_tracking_events te
			JOIN mailing_campaigns c ON c.id = te.campaign_id
			WHERE te.event_type = 'clicked'
			  AND c.partner_dataset_id IS NOT NULL
			  ` + verdictFilter + `
			  ` + timeFilter + `
			GROUP BY te.subscriber_id, c.partner_dataset_id
		) e
		WHERE q.subscriber_id = e.subscriber_id
		  AND q.dataset_id = e.partner_dataset_id
		  AND q.engaged_at IS NULL`
}

// PartnerEngagementMarker stamps partner_clean_queue.engaged_at when a record's
// subscriber CLICKS one of that record's own data-partner-drip campaigns (matched
// by mailing_campaigns.partner_dataset_id = partner_clean_queue.dataset_id).
//
// Why this exists (2026-06-22): the engaged_at column was added 2026-06-11 and its
// READERS were wired immediately — the drip's next-due query only mails records
// WHERE engaged_at IS NULL (so an engaged record should EXIT the drip), and every
// data-partner Activation/Churn metric counts COUNT(*) FILTER (WHERE engaged_at IS
// NOT NULL). But the WRITER — this "engagement marker" — was never built, so
// engaged_at was permanently NULL. Effect: (1) the drip never let an engaged record
// exit → it pushed every record through all four touches (over-mailing proven
// clickers); (2) every Activation metric read 0 / Churn read ~100% regardless of
// real performance (e.g. Spicy Clickers showed Engaged 0 despite ~2,123 real
// clickers). This worker closes the loop.
//
// Signal = CLICKS ONLY. Opens are ~90% Apple-MPP/machine; marking on opens would
// falsely "engage" nearly everyone after one touch and gut the drip. A click is the
// reliable human re-engagement signal this drip is designed to detect. Scoped to the
// dataset's own campaigns (partner_dataset_id) so cross-lane clicks don't leak — the
// same accuracy rule the Previous Activations v2 endpoint uses. Idempotent via the
// engaged_at IS NULL guard.
//
// HUMAN-VERDICT FILTER (REQ-036, operator-approved 2026-07-13): the click must be
// verdict-HUMAN — the canonical HUMAN_VERDICT_PG predicate (humanVerdictSQL above).
// Raw clicks are scanner-contaminated 2x-85x depending on ISP mix (G3,
// 2026-07-13), so unfiltered marking both inflated every Activation metric AND
// let scanner clicks exit records from the drip.
// APPROVED BEHAVIOR CHANGE: scanner-only clickers no longer get engaged_at — they
// continue the drip ladder to completion (ladder_complete), exactly like
// non-clickers. Kill switch DISABLE_VERDICT_ENGAGEMENT_MARKER=true (alias
// DISABLE_MARKER_VERDICT_FILTER=true) restores the old raw-click behavior.
//
// Kill switch: DISABLE_PARTNER_ENGAGEMENT_MARKER=1 disables it entirely.
type PartnerEngagementMarker struct {
	db       *sql.DB
	interval time.Duration
}

func NewPartnerEngagementMarker(db *sql.DB) *PartnerEngagementMarker {
	return &PartnerEngagementMarker{db: db, interval: 3 * time.Minute}
}

// Start runs a one-time all-history backfill ~2min after boot, then a windowed
// sweep on each tick. Runs until ctx is cancelled.
func (m *PartnerEngagementMarker) Start(ctx context.Context) {
	if m.db == nil {
		log.Printf("[PartnerEngagementMarker] disabled (db missing)")
		return
	}
	if os.Getenv("DISABLE_PARTNER_ENGAGEMENT_MARKER") == "1" {
		log.Printf("[PartnerEngagementMarker] disabled via DISABLE_PARTNER_ENGAGEMENT_MARKER")
		return
	}
	log.Printf("[PartnerEngagementMarker] started interval=%s (clicks->engaged_at)", m.interval)

	// Startup delay so migrations/boot settle before the catch-up pass.
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Minute):
	}
	// Boot catch-up: bounded to 7 days so it stays well under the app's
	// statement_timeout (an unbounded all-history pass scans every click ever
	// and times out). 7d comfortably covers any deploy/downtime gap; the
	// one-time full-history backfill is a manual op (already applied 2026-06-22).
	m.markOnce(ctx, 7*24*60)

	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[PartnerEngagementMarker] stopping")
			return
		case <-t.C:
			// Ongoing: only scan recent clicks. 30m overlaps the 3m interval
			// generously so no engagement is dropped across run boundaries.
			m.markOnce(ctx, 30)
		}
	}
}

// markOnce stamps engaged_at on records whose subscriber HUMAN-clicked one of
// that dataset's drip campaigns (verdict filter per REQ-036; kill switch —
// see verdictEngagementMarkerDisabled — reverts to raw clicks). lookbackMins
// <= 0 means all history (backfill).
func (m *PartnerEngagementMarker) markOnce(ctx context.Context, lookbackMins int) {
	var args []interface{}
	if lookbackMins > 0 {
		args = append(args, lookbackMins)
	}
	q := buildEngagementMarkSQL(lookbackMins, !verdictEngagementMarkerDisabled())
	res, err := m.db.ExecContext(ctx, q, args...)
	if err != nil {
		log.Printf("[PartnerEngagementMarker] mark err (lookback=%dm): %v", lookbackMins, err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[PartnerEngagementMarker] marked %d records engaged (clicks, lookback=%dm)", n, lookbackMins)
	}
}
