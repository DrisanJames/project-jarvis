package worker

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strings"
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

// perRecordDatasetMarkerDisabled reports whether the operator has flipped the
// kill switch for the 2026-07-26 per-record dataset re-key (see
// buildEngagementMarkSQL). When set, the marker reverts to the legacy
// campaign-stamp join (mailing_campaigns.partner_dataset_id) — the behavior
// with the proven board-send blind spot. Read per sweep so an ECS env flip
// takes effect on the next tick without a restart.
func perRecordDatasetMarkerDisabled() bool {
	v := os.Getenv("DISABLE_MARKER_PER_RECORD_DATASET")
	return v == "1" || v == "true"
}

// buildEngagementMarkSQL builds the engaged_at stamping statement.
//
// verdictFiltered=true (the default since REQ-036, operator-approved
// 2026-07-13) counts only HUMAN-verdict clicks: scanner/bot click detonations
// no longer stamp engaged_at, so scanner-clicked records stay in the drip and
// receive their full ladder — that consequence is intended and approved.
// verdictFiltered=false reproduces the original raw-click behavior exactly
// (kill switch path).
//
// perRecord=true (the default since 2026-07-26) re-keys attribution from the
// CAMPAIGN-level stamp to PER-RECORD dataset truth. The legacy join
// (`c.partner_dataset_id = q.dataset_id`) had a proven blind spot:
// mailing_campaigns.partner_dataset_id is stamped ONLY by the drip
// orchestrator (stampPartnerAttributionOnCampaign), and only with the
// vertical's single "dominant dataset" — so (1) board-deployed sends carry no
// stamp at all (attribits-spicy-clickers api_feed: engaged_at on 14 of
// 496,936 records vs ~3,500 expected at its measured 0.72% action-click
// rate), and (2) shared-vertical datasets other than the dominant one never
// match (attribits-term-life: 60 of 85,878).
//
// The fix does NOT read mailing_tracking_events.partner_dataset_id — that
// REQ-034 stamp exists only on 'sent' mirror rows (send_worker.go markSent);
// no 'clicked' insert path (pixel svc, tracking consumer, SES events,
// everflow postback) carries it, and even sent-row coverage is partial
// (post-2026-07-13, latch/kill-switch gated, absent on the sync builder
// path). Instead it computes the SAME deterministic REQ-034 attribution rule
// (pickDatasetID, send_worker.go) in SQL from the marker's inputs at mark
// time — campaign stamp + the subscriber's partner_clean_queue memberships:
//
//	ORDER BY (m.dataset_id = c.partner_dataset_id) DESC NULLS LAST,
//	         m.ingested_at ASC, m.dataset_id ASC  LIMIT 1
//
// = campaign's dataset when the subscriber is provably a member of it, else
// earliest-ingested membership (ties → smallest dataset id), else no mark
// (a subscriber with no linked-back pcq rows has no queue row to stamp).
// Membership lookups ride idx_pcq_subscriber_id (cmd/server/main.go
// concurrentIndexSpecs) — the caller gates on that index being valid
// (perRecordIndexReady) so this can never seq-scan the >1M-row queue; the
// clicked-window scan shape (event_at window + event_type) is unchanged from
// legacy, and the EXISTS pre-filter prunes non-partner clickers before the
// GROUP BY/LATERAL. Kill switch: DISABLE_MARKER_PER_RECORD_DATASET=true.
//
// Exported-shape note: kept as a pure function so the regression tests can pin
// ALL generated statements without a live PG (the verdict functions
// ignite_event_verdict/ignite_verdict_is_human exist only in prod PG).
func buildEngagementMarkSQL(lookbackMins int, verdictFiltered bool, perRecord bool) string {
	timeFilter := ""
	if lookbackMins > 0 {
		timeFilter = "AND te.event_at > NOW() - make_interval(mins => $1)"
	}
	verdictFilter := ""
	if verdictFiltered {
		verdictFilter = "AND " + humanVerdictSQL
	}
	if !perRecord {
		// Legacy campaign-stamp join — byte-stable kill-switch path.
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
	return `
		UPDATE partner_clean_queue q
		SET engaged_at = e.first_click
		FROM (
			SELECT k.subscriber_id, ds.dataset_id, MIN(k.first_click) AS first_click
			FROM (
				SELECT te.subscriber_id, te.campaign_id, MIN(te.event_at) AS first_click
				FROM mailing_tracking_events te
				WHERE te.event_type = 'clicked'
				  AND te.subscriber_id IS NOT NULL
				  AND EXISTS (
					SELECT 1 FROM partner_clean_queue pm
					WHERE pm.subscriber_id = te.subscriber_id
				  )
				  ` + verdictFilter + `
				  ` + timeFilter + `
				GROUP BY te.subscriber_id, te.campaign_id
			) k
			LEFT JOIN mailing_campaigns c ON c.id = k.campaign_id
			CROSS JOIN LATERAL (
				SELECT m.dataset_id
				FROM partner_clean_queue m
				WHERE m.subscriber_id = k.subscriber_id
				ORDER BY (m.dataset_id = c.partner_dataset_id) DESC NULLS LAST,
					m.ingested_at ASC, m.dataset_id ASC
				LIMIT 1
			) ds
			GROUP BY k.subscriber_id, ds.dataset_id
		) e
		WHERE q.subscriber_id = e.subscriber_id
		  AND q.dataset_id = e.dataset_id
		  AND q.engaged_at IS NULL`
}

// ---------------------------------------------------------------------------
// ONE-SHOT HISTORICAL BACKFILL (do NOT run from code — operator-gated).
//
// Retro-stamps engagement the legacy campaign-join marker missed (the
// board-mailed spicy clickers and the shared-vertical term_life class). Run
// ONCE after this binary deploys, in a calm window, on a session with a
// raised timeout — the per-tick sweep only looks back 30m/7d and will not
// self-heal history:
//
//	SET statement_timeout = '30min';
//	UPDATE partner_clean_queue q
//	SET engaged_at = e.first_click
//	FROM (
//	    SELECT k.subscriber_id, ds.dataset_id, MIN(k.first_click) AS first_click
//	    FROM (
//	        SELECT te.subscriber_id, te.campaign_id, MIN(te.event_at) AS first_click
//	        FROM mailing_tracking_events te
//	        WHERE te.event_type = 'clicked'
//	          AND te.subscriber_id IS NOT NULL
//	          AND ignite_verdict_is_human(ignite_event_verdict(te.user_agent, te.ip_address))
//	          AND EXISTS (
//	            SELECT 1 FROM partner_clean_queue pm
//	            WHERE pm.subscriber_id = te.subscriber_id
//	              AND pm.engaged_at IS NULL
//	          )
//	        GROUP BY te.subscriber_id, te.campaign_id
//	    ) k
//	    LEFT JOIN mailing_campaigns c ON c.id = k.campaign_id
//	    CROSS JOIN LATERAL (
//	        SELECT m.dataset_id
//	        FROM partner_clean_queue m
//	        WHERE m.subscriber_id = k.subscriber_id
//	        ORDER BY (m.dataset_id = c.partner_dataset_id) DESC NULLS LAST,
//	            m.ingested_at ASC, m.dataset_id ASC
//	        LIMIT 1
//	    ) ds
//	    GROUP BY k.subscriber_id, ds.dataset_id
//	) e
//	WHERE q.subscriber_id = e.subscriber_id
//	  AND q.dataset_id = e.dataset_id
//	  AND q.engaged_at IS NULL;
//
// Notes for the operator:
//   - Idempotent (engaged_at IS NULL guard) — safe to re-run; a partial run
//     that timed out keeps its progress.
//   - If the all-history click scan is too heavy in one pass, chunk it by
//     month: add `AND te.event_at >= '<start>' AND te.event_at < '<end>'` to
//     the inner scan and run per month, oldest first (engaged_at takes the
//     earliest click seen so far; MIN + the IS NULL guard keep later chunks
//     from regressing it — earlier months stamp first).
//   - The verdict predicate matches the live marker (REQ-036 semantics).
// ---------------------------------------------------------------------------

// PartnerEngagementMarker stamps partner_clean_queue.engaged_at when a record's
// subscriber HUMAN-clicks a campaign attributable to that record's dataset —
// per-record REQ-034 attribution (see buildEngagementMarkSQL): the campaign's
// stamped dataset when the subscriber is a member of it, else the subscriber's
// earliest-ingested membership (board/sidecar/broadcast sends included).
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
// reliable human re-engagement signal this drip is designed to detect. Attribution
// is scoped by the deterministic REQ-034 per-record rule so cross-lane clicks
// resolve to exactly one dataset per subscriber-click — the same rule the send
// worker stamps messages with (pickDatasetID). Idempotent via the
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
// Kill switches: DISABLE_PARTNER_ENGAGEMENT_MARKER=1 disables it entirely;
// DISABLE_MARKER_PER_RECORD_DATASET=true reverts to the legacy campaign-stamp
// join (the pre-2026-07-26 blind-spot behavior).
type PartnerEngagementMarker struct {
	db       *sql.DB
	interval time.Duration

	// Per-record mode requires idx_pcq_subscriber_id (built CONCURRENTLY by
	// ensureConcurrentIndexes, possibly hours after first boot of the binary
	// that introduced it). Until the index is valid, sweeps use the legacy
	// campaign-join SQL so the marker can never seq-scan the >1M-row queue.
	// Same latch pattern as SendWorkerPool.datasetResolveIndexReady. Touched
	// only from the Start goroutine — no mutex needed.
	perRecordReady     bool
	perRecordNextProbe time.Time
}

func NewPartnerEngagementMarker(db *sql.DB) *PartnerEngagementMarker {
	return &PartnerEngagementMarker{db: db, interval: 3 * time.Minute}
}

// perRecordIndexReady reports whether the per-record membership lookup may
// run: idx_pcq_subscriber_id must exist and be valid. Fails CLOSED (legacy
// SQL) on probe error; latches true; re-probes at most every
// datasetResolveProbeTTL while not ready.
func (m *PartnerEngagementMarker) perRecordIndexReady(ctx context.Context) bool {
	if m.perRecordReady {
		return true
	}
	if time.Now().Before(m.perRecordNextProbe) {
		return false
	}
	m.perRecordNextProbe = time.Now().Add(datasetResolveProbeTTL)
	var valid bool
	err := m.db.QueryRowContext(ctx, `
		SELECT i.indisvalid
		FROM pg_class c
		JOIN pg_index i ON i.indexrelid = c.oid
		WHERE c.relname = 'idx_pcq_subscriber_id'
	`).Scan(&valid)
	if err != nil || !valid {
		log.Printf("[PartnerEngagementMarker] per-record dataset mode dormant: idx_pcq_subscriber_id not valid yet (err=%v valid=%v); legacy campaign-join sweep, re-probe in %s", err, valid, datasetResolveProbeTTL)
		return false
	}
	m.perRecordReady = true
	log.Printf("[PartnerEngagementMarker] per-record dataset mode ACTIVE (idx_pcq_subscriber_id valid)")
	return true
}

// Start runs a one-time catch-up ~2min after boot, then a windowed sweep on
// each tick. Runs until ctx is cancelled.
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
	// one-time full-history backfill is a manual op (already applied
	// 2026-06-22; per-record re-key backfill pending — see the BACKFILL block
	// above).
	m.markOnce(ctx, 7*24*60)

	// Progression backfill BEFORE any cold bucketing is allowed. The 30m
	// windowed sweeps only ever see the last half hour, so without this the
	// gate would judge every older record on NULL columns.
	if m.backfillProgressionSignals(ctx) {
		MarkProgressionSignalsReady()
	}

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

// markOnce stamps engaged_at on records whose subscriber HUMAN-clicked a
// campaign attributable to that record's dataset (verdict filter per REQ-036;
// per-record REQ-034 attribution unless the kill switch or a missing index
// forces the legacy campaign-join). lookbackMins <= 0 means all history
// (backfill).
func (m *PartnerEngagementMarker) markOnce(ctx context.Context, lookbackMins int) {
	var args []interface{}
	if lookbackMins > 0 {
		args = append(args, lookbackMins)
	}
	perRecord := !perRecordDatasetMarkerDisabled() && m.perRecordIndexReady(ctx)
	q := buildEngagementMarkSQL(lookbackMins, !verdictEngagementMarkerDisabled(), perRecord)
	res, err := m.db.ExecContext(ctx, q, args...)
	if err != nil {
		// DO NOT return. The engaged_at statement and the progression-signal
		// statement answer different questions and the ladder gate depends on
		// the latter. Coupling them cost us the 2026-08-24 rollout: the 7-day
		// engaged_at catch-up hit the statement timeout, so the progression
		// backfill never ran and every record looked un-engaged.
		log.Printf("[PartnerEngagementMarker] mark err (lookback=%dm perRecord=%v): %v", lookbackMins, perRecord, err)
	} else if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[PartnerEngagementMarker] marked %d records engaged (clicks, lookback=%dm, perRecord=%v)", n, lookbackMins, perRecord)
	}

	m.markProgressionSignals(ctx, lookbackMins, lookbackMins)
}

// progressionBackfillDays is how far back the boot backfill walks the
// progression signal. It must exceed the OLDEST window the cold sweep can
// judge: a record's window opens at next_touch_at - 24h, and rows sit past due
// for days on a starved lane (measured 2026-08-24: v7 touch-2 rows were 105h
// past due, so their window opened ~129h back). 14 days covers that with room.
const progressionBackfillDays = 14

// backfillProgressionSignals walks the progression signal back day by day.
//
// One DAY-sized chunk per statement, oldest first, so no single statement can
// hit the timeout the way an all-history pass does — that timeout is exactly
// what left the gate judging on empty columns. Returns true only if EVERY chunk
// succeeded: partial coverage must not latch readiness, or the cold sweep would
// bucket records whose engagement simply had not been stamped yet.
func (m *PartnerEngagementMarker) backfillProgressionSignals(ctx context.Context) bool {
	// 6h chunks, not 24h. Measured 2026-08-24: a full day of drip tracking
	// events still hit the statement timeout on the first chunk, so the backfill
	// never completed and the gate stayed held.
	const chunkMins = 6 * 60
	chunks := progressionBackfillDays * 24 * 60 / chunkMins
	for i := chunks; i >= 1; i-- {
		if ctx.Err() != nil {
			return false
		}
		// Window [now-i*chunk, now-(i-1)*chunk).
		if !m.markProgressionSignals(ctx, i*chunkMins, (i-1)*chunkMins) {
			log.Printf("[PartnerEngagementMarker] progression backfill FAILED at chunk -%d (%dh back) — cold bucketing stays held",
				i, i*chunkMins/60)
			return false
		}
	}
	log.Printf("[PartnerEngagementMarker] progression backfill complete (%d days in %d chunks) — cold bucketing may proceed",
		progressionBackfillDays, chunks)
	return true
}

// markProgressionSignals stamps last_open_at / last_click_at — the LADDER
// PROGRESSION signal, which is a different question from engaged_at.
//
// engaged_at answers "has this record ever human-clicked?" and means EXIT (and
// feeds every data-partner Activation/Churn metric). These two columns answer
// "did this record react to the touch we just sent?" and mean ADVANCE, for the
// engagement-gated verticals (operator 2026-08-24 — see
// partner_drip_engagement_gate.go).
//
// Differences from the engaged_at statement, all deliberate:
//   - OPENS COUNT. The engaged_at marker is clicks-only because on a 4-touch
//     ladder an open-based exit would eject nearly everyone after touch 1. For
//     progression the polarity is reversed: an open is the weakest acceptable
//     proof that a human saw the message, and the operator asked for openers
//     AND clickers. Measured on the gated lanes, opens are 0.2-3% (not the
//     ~90% MPP figure that governs Apple), so this does not wave everyone
//     through.
//   - NO engaged_at IS NULL GUARD, and idempotent via GREATEST: these columns
//     track the LATEST signal and must keep moving forward every sweep.
//   - MATCHED BY SUBSCRIBER + VERTICAL, not the REQ-034 dataset rule. A touch
//     is served per (vertical, brand); reacting to it is a fact about the
//     record's own lane, and the dataset-disambiguation rule exists to avoid
//     double-counting Activation metrics, which these columns do not feed.
//
// Clicks keep the human-verdict filter (a scanner detonation must not advance a
// record — that is precisely the batch-and-blast signal the gate exists to
// stop). Opens are taken raw: there is no verdict function for opens, and the
// gate's own measurements were taken on raw opens.
func (m *PartnerEngagementMarker) markProgressionSignals(ctx context.Context, sinceMins, untilMins int) bool {
	if engagementGateDisabled() {
		return false
	}
	var args []interface{}
	timeFilter := ""
	if sinceMins > 0 {
		timeFilter = "AND te.event_at > NOW() - make_interval(mins => $1)"
		args = append(args, sinceMins)
		if untilMins > 0 && untilMins < sinceMins {
			timeFilter += " AND te.event_at <= NOW() - make_interval(mins => $2)"
			args = append(args, untilMins)
		}
	}
	verdictFilter := ""
	if !verdictEngagementMarkerDisabled() {
		verdictFilter = "AND " + humanVerdictSQL
	}

	// Scope to the GATED lanes only. The signal is consumed by the engagement
	// gate and nothing else, so scanning every drip campaign is pure cost — the
	// internal family is ~25k of ~280k daily drip sends, and the unscoped
	// version timed out on every chunk (2026-08-24).
	laneFilter := "AND (" + strings.Join(gatedCampaignNameLike(), " OR ") + ")"

	q := `
		UPDATE partner_clean_queue q
		SET last_open_at  = GREATEST(q.last_open_at,  e.last_open),
		    last_click_at = GREATEST(q.last_click_at, e.last_click)
		FROM (
			SELECT te.subscriber_id,
			       split_part(c.name, ' ', 2) AS vertical,
			       MAX(te.event_at) FILTER (WHERE te.event_type = 'opened')  AS last_open,
			       MAX(te.event_at) FILTER (WHERE te.event_type = 'clicked' ` + verdictFilter + `) AS last_click
			FROM mailing_tracking_events te
			JOIN mailing_campaigns c ON c.id = te.campaign_id
			WHERE te.event_type IN ('opened', 'clicked')
			  AND te.subscriber_id IS NOT NULL
			  ` + laneFilter + `
			  ` + timeFilter + `
			GROUP BY 1, 2
		) e
		WHERE q.subscriber_id = e.subscriber_id
		  AND q.vertical = e.vertical
		  AND (
		        q.last_open_at  IS DISTINCT FROM GREATEST(q.last_open_at,  e.last_open)
		     OR q.last_click_at IS DISTINCT FROM GREATEST(q.last_click_at, e.last_click)
		  )`

	// Own transaction with a raised local timeout: the app default is tuned for
	// request handlers, and this is a bounded background backfill chunk.
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("[PartnerEngagementMarker] progression-signal begin err (window %dm..%dm): %v", sinceMins, untilMins, err)
		return false
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = '120s'"); err != nil {
		log.Printf("[PartnerEngagementMarker] progression-signal timeout-set err: %v", err)
		return false
	}
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		log.Printf("[PartnerEngagementMarker] progression-signal mark err (window %dm..%dm): %v", sinceMins, untilMins, err)
		return false
	}
	if err := tx.Commit(); err != nil {
		log.Printf("[PartnerEngagementMarker] progression-signal commit err (window %dm..%dm): %v", sinceMins, untilMins, err)
		return false
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[PartnerEngagementMarker] stamped progression signals on %d records (opens+clicks, window %dm..%dm)", n, sinceMins, untilMins)
	}
	return true
}
