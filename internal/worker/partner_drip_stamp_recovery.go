package worker

// =============================================================================
// PARTNER-DRIP STAMP DURABILITY + RECOVERY
// =============================================================================
// The partner drip ships a wave (DeployFn) and THEN advances the ladder in
// partner_clean_queue (markMailed). Those are two separate writes with no
// transaction between them, so a failure of the second one is pure data loss:
// the mail is already gone, but touch_count / next_touch_at / status still say
// the record was never mailed, and it stays claimable.
//
// 2026-08-20, campaign ff01ad90-e3fc-4d0e-bc55-239e8ce35d69
// ("[partner-drip] consumer tot 20260820T213025 …"):
//
//	[PartnerDripOrchestrator] mark_mailed (campaign ff01ad90… already deployed!):
//	  pq: canceling statement due to statement timeout
//
// Measured on that campaign: 314 rows in mailing_message_log (real sends),
// 314 matching partner_clean_queue rows still status='claimed', touch_count=1,
// last_touch_campaign_id pointing at the PREVIOUS touch. That is the same shape
// that re-mailed 4,104 people three times on 2026-08-17.
//
// This file supplies the three missing pieces:
//
//	1. stampMailedChunk  — the stamp, chunked + retried under the orchestrator's
//	                       elevated statement_timeout, with an idempotency guard
//	                       so a retry can never double-advance the ladder.
//	2. recordStampFailure — a durable, operator-queryable signal
//	                       (partner_drip_stamp_failures) for every lost stamp.
//	3. RecoverUnstampedTouches — an always-on (opt-OUT via
//	                       PARTNER_DRIP_STAMP_RECOVERY_DISABLED) sweep that
//	                       re-stamps lost rows FROM mailing_message_log (send
//	                       truth), never by inference.
//
// Nothing here changes what gets sent, how records are claimed, or how waves
// are dispatched. It only records what already happened.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
)

// -----------------------------------------------------------------------------
// Tunables
// -----------------------------------------------------------------------------

// markMailedChunkSize bounds the id array in a single stamp UPDATE.
//
// Why chunking is the PRIMARY fix and not just a nicety: a wave carries up to
// cfg.MaxWaveSize (5000, partner_drip_orchestrator.go:517) ids, and the stamp
// runs `WHERE id = ANY($1::uuid[])` against an 11.8M-row / 10GB table that is
// simultaneously being scanned by the claim CTEs. Chunking (a) keeps each
// statement a bounded set of PK probes on partner_clean_queue_pkey that comfortably
// fits any timeout, (b) shrinks the row-lock footprint held against concurrent
// FOR UPDATE SKIP LOCKED claims, and (c) makes progress DURABLE — a chunk that
// commits stays committed even if a later chunk dies, so a failure costs 500 rows
// instead of 5000.
const markMailedChunkSize = 500

// markMailedMaxAttempts / markMailedRetryBackoff bound the per-chunk retry.
// Retry is the SECONDARY fix: chunking removes the ordinary timeout, retry
// covers the residual (an RDS brownout, a deadlock against the claim CTE, a
// serialization failure). It is only safe because of the idempotency guard in
// markMailedStampSQL — without that, a retry would double-increment touch_count.
// 4 attempts with 300ms linear backoff caps the worst case at ~1.8s of sleep per
// chunk, which stays well inside a 15s orchestrator tick.
const markMailedMaxAttempts = 4
const markMailedRetryBackoff = 300 * time.Millisecond

// markMailedStampSQL is the ladder advance. Byte-identical to the pre-2026-08-20
// statement EXCEPT for the final predicate.
//
// IDEMPOTENCY GUARD: `last_touch_campaign_id IS DISTINCT FROM $2::uuid`.
// One campaign == one wave == one touch (deployWaveGroup mints a fresh campaign
// per routing group, buildCampaignInput mints a fresh name per segment), so a row
// whose last_touch_campaign_id already equals this campaign has ALREADY been
// advanced by this touch. Re-applying is then a zero-row no-op: touch_count moves
// exactly once no matter how many times the statement runs. IS DISTINCT FROM (not
// `<>`) is required because last_touch_campaign_id is NULL on a first touch.
const markMailedStampSQL = `
		UPDATE partner_clean_queue
		SET status = 'mailed',
		    mailed_campaign_id = COALESCE(mailed_campaign_id, $2::uuid),
		    mailed_brand = COALESCE(mailed_brand, $3),
		    mailed_at = COALESCE(mailed_at, NOW()),
		    touch_count = LEAST(COALESCE(touch_count, 0) + 1, $4),
		    last_touch_brand = $3,
		    last_touch_campaign_id = $2::uuid,
		    next_touch_at = CASE
		        WHEN COALESCE(touch_count, 0) + 1 < $4 THEN $5::timestamptz
		        ELSE NULL
		    END,
		    terminal_reason = CASE
		        WHEN COALESCE(touch_count, 0) + 1 >= $4 THEN 'completed'
		        ELSE terminal_reason
		    END
		WHERE id = ANY($1::uuid[])
		  AND last_touch_campaign_id IS DISTINCT FROM $2::uuid
`

// -----------------------------------------------------------------------------
// Observability
// -----------------------------------------------------------------------------

// stampCounters is the in-process half of the observability answer. The durable
// half is the partner_drip_stamp_failures table (see recordStampFailure), which
// is what an operator can actually query.
type stampCounters struct {
	rowsStamped     atomic.Int64
	rowsLost        atomic.Int64
	chunkRetries    atomic.Int64
	wavesFailed     atomic.Int64
	rowsRecovered   atomic.Int64
	lastFailureUnix atomic.Int64
}

// StampStats is a snapshot of the ladder-stamp health since process start.
type StampStats struct {
	RowsStamped   int64     `json:"rows_stamped"`
	RowsLost      int64     `json:"rows_lost"`
	ChunkRetries  int64     `json:"chunk_retries"`
	WavesFailed   int64     `json:"waves_failed"`
	RowsRecovered int64     `json:"rows_recovered"`
	LastFailureAt time.Time `json:"last_failure_at,omitempty"`
}

// StampStats returns the ladder-stamp counters. RowsLost > 0 means mail went out
// whose ladder advance was dropped — records that will be re-mailed unless
// RecoverUnstampedTouches runs.
func (po *PartnerDripOrchestrator) StampStats() StampStats {
	out := StampStats{
		RowsStamped:   po.stamp.rowsStamped.Load(),
		RowsLost:      po.stamp.rowsLost.Load(),
		ChunkRetries:  po.stamp.chunkRetries.Load(),
		WavesFailed:   po.stamp.wavesFailed.Load(),
		RowsRecovered: po.stamp.rowsRecovered.Load(),
	}
	if u := po.stamp.lastFailureUnix.Load(); u > 0 {
		out.LastFailureAt = time.Unix(u, 0).UTC()
	}
	return out
}

// stampHeartbeatWorkerName is the mailing_worker_heartbeats row this subsystem
// beats into every tick. This is the answer to "can an operator tell without
// reading logs" — worker_health.go already surfaces that table in the portal and
// carries last_status / last_error into the stall alert body:
//
//	SELECT last_status, last_error, last_beat_at
//	FROM mailing_worker_heartbeats
//	WHERE worker_name = 'partner_drip_ladder_stamp';
//
// last_status flips to 'error' the moment a ladder advance is dropped and stays
// there until the loss is recovered — on 2026-08-20 the ONLY evidence was one log
// line inside a 30-minute window.
const stampHeartbeatWorkerName = "partner_drip_ladder_stamp"

// emitStampHeartbeat beats once per orchestrator tick. Health is derived from the
// counters, not from the tick's own success: 'error' means "rows were mailed whose
// touch was never recorded and has not been recovered", which is a STANDING
// condition, not a transient one.
//
// Note the counters are per-process (they reset on an ECS bounce). The durable
// cross-restart record is partner_drip_stamp_failures; this row is the always-on
// health surface that points an operator at it.
func (po *PartnerDripOrchestrator) emitStampHeartbeat(ctx context.Context) {
	st := po.StampStats()
	outstanding := st.RowsLost - st.RowsRecovered
	if outstanding < 0 {
		outstanding = 0
	}
	status, detail := "ok", ""
	if outstanding > 0 {
		status = "error"
		detail = fmt.Sprintf("%d ladder stamps LOST across %d waves (last %s) — mail shipped, touch NOT recorded. Worklist: SELECT * FROM partner_drip_stamp_failures WHERE recovered_at IS NULL. Recovery runs automatically each tick cadence unless PARTNER_DRIP_STAMP_RECOVERY_DISABLED is set.",
			outstanding, st.WavesFailed, st.LastFailureAt.Format(time.RFC3339))
	}
	interval := int(po.cfg.TickInterval.Seconds())
	if interval <= 0 {
		interval = 900
	}
	EmitHeartbeat(ctx, po.db, stampHeartbeatWorkerName, interval, status, detail)
}

// -----------------------------------------------------------------------------
// The stamp itself
// -----------------------------------------------------------------------------

// stampMailedChunk applies markMailedStampSQL to one bounded id chunk, retrying
// a retryable failure with bounded backoff. Returns the rows actually advanced.
//
// Each attempt runs inside withDBTimeout, i.e. its OWN read-committed transaction
// with `SET LOCAL statement_timeout = '120000ms'`. That is the established
// orchestrator idiom (partner_drip_orchestrator.go:962) and the third leg of the
// fix: the app pool ships at 30s (main.go statement_timeout=30000) and markMailed
// was the one orchestrator write still running there. SET LOCAL is transaction-
// scoped, so the pooled connection returns with the app's 30s ceiling intact.
// One transaction PER CHUNK (rather than one wrapping the whole wave) is what
// keeps the lock footprint small and each chunk's progress durable.
func (po *PartnerDripOrchestrator) stampMailedChunk(ctx context.Context, ids []string, campaignID, brand string, nextTouchAt time.Time) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	arr := "{" + strings.Join(ids, ",") + "}"
	var lastErr error
	for attempt := 1; attempt <= markMailedMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var n int64
		err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, markMailedStampSQL, arr, campaignID, brand, MaxTouchCount, nextTouchAt)
			if err != nil {
				return err
			}
			n, err = res.RowsAffected()
			return err
		})
		if err == nil {
			return n, nil
		}
		lastErr = err
		// A cancelled parent context is not a transient DB fault — abort now
		// rather than burning the remaining attempts on a shutting-down worker.
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if !isRetryableStampError(err) || attempt == markMailedMaxAttempts {
			break
		}
		po.stamp.chunkRetries.Add(1)
		log.Printf("[PartnerDripOrchestrator] stamp chunk retry %d/%d campaign=%s rows=%d: %v",
			attempt, markMailedMaxAttempts, campaignID, len(ids), err)
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(time.Duration(attempt) * markMailedRetryBackoff):
		}
	}
	return 0, lastErr
}

// isRetryableStampError reports whether a stamp failure is transient — i.e.
// whether re-running the (idempotent) statement has any chance of succeeding.
//
//	57014 query_canceled        — statement_timeout, the 2026-08-20 failure
//	40001 serialization_failure
//	40P01 deadlock_detected     — against the claim CTE's row locks
//	55P03 lock_not_available
//	53300 too_many_connections
//	08006 / 08003               — connection dropped mid-statement
//
// Anything else (a constraint violation, a missing column, a syntax error) is a
// PERMANENT fault: retrying wastes a tick and hides the real defect, so it is
// surfaced immediately.
func isRetryableStampError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var pe *pq.Error
	if errors.As(err, &pe) {
		switch string(pe.Code) {
		case "57014", "40001", "40P01", "55P03", "53300", "08006", "08003", "08000":
			return true
		}
		return false
	}
	// Driver-level connection churn surfaces as a plain error from database/sql.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "driver: bad connection") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "statement timeout")
}

// -----------------------------------------------------------------------------
// The durable signal
// -----------------------------------------------------------------------------

// PartnerDripStampFailuresDDL is applied by runStartupMigrations (cmd/server/main.go).
// Kept here next to its only writer so the two never drift.
const PartnerDripStampFailuresDDL = `CREATE TABLE IF NOT EXISTS partner_drip_stamp_failures (
	campaign_id    UUID PRIMARY KEY,
	brand          TEXT NOT NULL DEFAULT '',
	rows_lost      INTEGER NOT NULL DEFAULT 0,
	rows_recovered INTEGER NOT NULL DEFAULT 0,
	last_error     TEXT NOT NULL DEFAULT '',
	first_seen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	last_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	recovered_at   TIMESTAMPTZ
)`

// recordStampFailure writes the operator-queryable evidence that a wave shipped
// without advancing the ladder:
//
//	SELECT * FROM partner_drip_stamp_failures WHERE recovered_at IS NULL;
//
// Best-effort by design — this is a signal, not a gate, and a wave that already
// mailed must never be failed harder because its bookkeeping row could not be
// written. It runs on a context detached from cancellation (with its own short
// deadline) so the signal survives the ECS bounce that caused the loss.
func (po *PartnerDripOrchestrator) recordStampFailure(ctx context.Context, campaignID, brand string, rowsLost int64, cause error) {
	if campaignID == "" || po.db == nil {
		return
	}
	msg := ""
	if cause != nil {
		msg = cause.Error()
		if len(msg) > 500 {
			msg = msg[:500]
		}
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := po.db.ExecContext(wctx, `
		INSERT INTO partner_drip_stamp_failures (campaign_id, brand, rows_lost, last_error)
		VALUES ($1::uuid, $2, $3, $4)
		ON CONFLICT (campaign_id) DO UPDATE
		SET rows_lost    = GREATEST(partner_drip_stamp_failures.rows_lost, EXCLUDED.rows_lost),
		    last_error   = EXCLUDED.last_error,
		    last_seen_at = NOW(),
		    recovered_at = NULL
	`, campaignID, brand, rowsLost, msg); err != nil {
		log.Printf("[PartnerDripOrchestrator] ALERT stamp_failure_ledger write failed campaign=%s (loss is LOG-ONLY now): %v", campaignID, err)
	}
}

// -----------------------------------------------------------------------------
// Recovery
// -----------------------------------------------------------------------------

// stampRecoveryMode controls the recovery sweep. Since 2026-08-22 recovery is
// ALWAYS ON (mode "apply") by default: the residual reconcile in tickOnce used
// to make un-stamped rows re-claimable every tick (the 2026-08-17 triple-fire),
// so replaying lost stamps cannot depend on an operator remembering to arm an
// env var. The gate is now an opt-OUT kill switch:
//
//	PARTNER_DRIP_STAMP_RECOVERY_DISABLED=1 — inert. Nothing runs.
//
// The legacy PARTNER_DRIP_STAMP_RECOVERY var is still honored as a mode
// override: "dry" measures and reports without writing; "off"/"0" also
// disables. Unset (the production default) now means "apply".
type stampRecoveryMode string

const (
	stampRecoveryOff   stampRecoveryMode = "off"
	stampRecoveryDry   stampRecoveryMode = "dry"
	stampRecoveryApply stampRecoveryMode = "apply"
)

func currentStampRecoveryMode() stampRecoveryMode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PARTNER_DRIP_STAMP_RECOVERY_DISABLED"))) {
	case "1", "true", "yes", "on":
		return stampRecoveryOff
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PARTNER_DRIP_STAMP_RECOVERY"))) {
	case "dry", "report":
		return stampRecoveryDry
	case "off", "0", "none":
		return stampRecoveryOff
	default:
		// Unset, "apply", "live", or anything unrecognized: recovery runs.
		return stampRecoveryApply
	}
}

// stampRecoveryInterval is the minimum wall-clock gap between recovery passes.
// The orchestrator ticks every 15 minutes (cfg.TickInterval default,
// partner_drip_orchestrator.go:521); the sweep is a bounded but non-trivial read,
// so it runs at half that rate — and the guard is what stops an ECS bounce (which
// re-fires tickOnce immediately on boot) from turning it into a per-restart sweep.
const stampRecoveryInterval = 30 * time.Minute

// stampRecoveryLookback is how far back the sweep looks for partner-drip
// campaigns. Override with PARTNER_DRIP_STAMP_RECOVERY_HOURS.
const stampRecoveryLookbackHours = 48

// stampRecoveryMaxCampaigns bounds one pass.
const stampRecoveryMaxCampaigns = 500

// StampRecoveryCampaign is the per-campaign result of a recovery pass.
type StampRecoveryCampaign struct {
	CampaignID string `json:"campaign_id"`
	DatasetID  string `json:"dataset_id"`
	Brand      string `json:"brand"`
	Name       string `json:"name"`
	RowsFound  int64  `json:"rows_found"`
	RowsFixed  int64  `json:"rows_fixed"`
}

// StampRecoveryReport is what one pass did (or, in dry mode, would do).
type StampRecoveryReport struct {
	Mode              string                  `json:"mode"`
	CampaignsScanned  int                     `json:"campaigns_scanned"`
	CampaignsWithLoss int                     `json:"campaigns_with_loss"`
	RowsFound         int64                   `json:"rows_found"`
	RowsFixed         int64                   `json:"rows_fixed"`
	Campaigns         []StampRecoveryCampaign `json:"campaigns"`
}

// stampRecoveryFindSQL counts partner_clean_queue rows that a campaign PROVABLY
// mailed (they have a mailing_message_log row for it) but whose ladder was never
// advanced to that campaign.
//
// INDEX PATH — this must never seq-scan (or dataset-scan) partner_clean_queue,
// the hottest table in the system at ~11.8M rows / 10GB. The plan is PINNED with
// `WITH sent AS MATERIALIZED`, which forces Postgres to build the small side first
// instead of choosing to drive from the queue:
//   - `sent`: `idx_message_log_campaign (campaign_id)` → a few hundred/thousand
//     rows for one wave, deduped to one row per subscriber.
//   - `partner_clean_queue q`: `idx_pcq_subscriber_id (subscriber_id) WHERE
//     subscriber_id IS NOT NULL` probed once per `sent` row (nested loop).
//     linkSubscriberIDsToQueue stamps subscriber_id BEFORE deploy, so every row a
//     campaign could have mailed has one.
//
// Without AS MATERIALIZED the planner could pick `idx_pcq_dataset_status_ingested`
// off the dataset_id predicate and scan every row of a multi-million-row dataset.
// The GROUP BY in the apply CTE also makes a duplicate message-log row for one
// subscriber deterministic (MIN(sent_at) = the real first send) rather than
// arbitrary.
//
// The dataset_id predicate is NOT cosmetic. partner_clean_queue is unique on
// (vertical, email_md5), so one human in two verticals has TWO rows sharing a
// subscriber_id. Measured on the incident campaign: 314 correct rows matched
// AND 48 rows in other datasets (touch_count up to 5, two of them 'held') matched
// on subscriber_id alone. Without `q.dataset_id = $2` this recovery would have
// corrupted 48 rows in unrelated lanes.
const stampRecoveryFindSQL = `
		WITH sent AS MATERIALIZED (
		    SELECT DISTINCT subscriber_id
		    FROM mailing_message_log
		    WHERE campaign_id = $1::uuid
		      AND subscriber_id IS NOT NULL
		)
		SELECT COUNT(*)
		FROM sent s
		JOIN partner_clean_queue q ON q.subscriber_id = s.subscriber_id
		WHERE q.dataset_id = $2::uuid
		  AND q.last_touch_campaign_id IS DISTINCT FROM $1::uuid
`

// stampRecoveryApplySQL stamps those rows. Same ladder transition as
// markMailedStampSQL and the same idempotency guard, with two differences:
//
//   - mailed_at / next_touch_at are derived from ml.sent_at (the real send time),
//     not NOW(), so a record recovered hours late is not pushed a further 24h out.
//     Both COALESCE to NOW() so a NULL sent_at can never write a NULL next_touch_at,
//     which would silently eject the record from the follow-up ladder for good.
//   - last_touch_brand / mailed_brand only move when the brand could be resolved
//     ($3 non-empty); a NULL-brand recovery leaves the existing value alone rather
//     than blanking a field the per-brand daily budgets count on.
//
// UPDATE ... FROM never applies a row twice within one statement, so a duplicate
// message-log row for the same subscriber cannot double-increment either.
const stampRecoveryApplySQL = `
		WITH sent AS MATERIALIZED (
		    SELECT subscriber_id, MIN(sent_at) AS sent_at
		    FROM mailing_message_log
		    WHERE campaign_id = $1::uuid
		      AND subscriber_id IS NOT NULL
		    GROUP BY subscriber_id
		)
		UPDATE partner_clean_queue q
		SET status = 'mailed',
		    mailed_campaign_id = COALESCE(q.mailed_campaign_id, $1::uuid),
		    mailed_brand = COALESCE(q.mailed_brand, NULLIF($3, '')),
		    mailed_at = COALESCE(q.mailed_at, ml.sent_at, NOW()),
		    touch_count = LEAST(COALESCE(q.touch_count, 0) + 1, $4),
		    last_touch_brand = COALESCE(NULLIF($3, ''), q.last_touch_brand),
		    last_touch_campaign_id = $1::uuid,
		    next_touch_at = CASE
		        WHEN COALESCE(q.touch_count, 0) + 1 < $4
		        THEN COALESCE(ml.sent_at, NOW()) + make_interval(hours => $5::int)
		        ELSE NULL
		    END,
		    terminal_reason = CASE
		        WHEN COALESCE(q.touch_count, 0) + 1 >= $4 THEN 'completed'
		        ELSE q.terminal_reason
		    END
		FROM sent ml
		WHERE q.subscriber_id = ml.subscriber_id
		  AND q.dataset_id = $2::uuid
		  AND q.last_touch_campaign_id IS DISTINCT FROM $1::uuid
`

// stampRecoveryWorklistSQL lists recent partner-drip campaigns that actually
// reached the sending stage. Rides `idx_campaigns_org_status_created
// (organization_id, status, created_at DESC)`; partner_drip_tag is a filter on
// the already-narrow result, not the driver.
const stampRecoveryWorklistSQL = `
		SELECT c.id::text,
		       COALESCE(c.partner_dataset_id::text, ''),
		       COALESCE(c.name, '')
		FROM mailing_campaigns c
		WHERE c.organization_id = $1::uuid
		  AND c.status IN ('sending', 'sent', 'completed', 'completed_with_errors')
		  AND c.created_at >= NOW() - make_interval(hours => $2::int)
		  AND c.partner_drip_tag IS NOT NULL
		ORDER BY c.created_at DESC
		LIMIT $3
`

// RecoverUnstampedTouches re-stamps drip records that were MAILED but whose
// ladder advance was lost, using mailing_message_log as the sole authority for
// "was this actually sent".
//
// Contract:
//   - NEVER stamps a row without a mailing_message_log row for that campaign.
//   - NEVER deletes, and never un-stamps.
//   - Safe to run repeatedly: the `last_touch_campaign_id IS DISTINCT FROM`
//     guard makes a second pass a zero-row no-op.
//   - apply=false reports exactly what apply=true would change.
//   - Honors ctx cancellation between campaigns.
func (po *PartnerDripOrchestrator) RecoverUnstampedTouches(ctx context.Context, lookbackHours int, apply bool) (*StampRecoveryReport, error) {
	if lookbackHours <= 0 {
		lookbackHours = stampRecoveryLookbackHours
	}
	mode := "dry"
	if apply {
		mode = "apply"
	}
	rep := &StampRecoveryReport{Mode: mode}

	type candidate struct{ id, dataset, name string }
	var cands []candidate
	err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, stampRecoveryWorklistSQL,
			po.cfg.OrganizationID, lookbackHours, stampRecoveryMaxCampaigns)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c candidate
			if err := rows.Scan(&c.id, &c.dataset, &c.name); err != nil {
				return err
			}
			cands = append(cands, c)
		}
		return rows.Err()
	})
	if err != nil {
		return rep, fmt.Errorf("stamp_recovery worklist: %w", err)
	}

	for _, c := range cands {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		rep.CampaignsScanned++
		if c.dataset == "" {
			// No dataset => the (vertical, email_md5) row cannot be disambiguated
			// from a same-subscriber row in another lane. Refuse rather than guess.
			log.Printf("[PartnerDripOrchestrator] stamp_recovery SKIP campaign=%s — no partner_dataset_id, cannot disambiguate", c.id)
			continue
		}
		var found int64
		if err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, stampRecoveryFindSQL, c.id, c.dataset).Scan(&found)
		}); err != nil {
			log.Printf("[PartnerDripOrchestrator] stamp_recovery find campaign=%s: %v", c.id, err)
			continue
		}
		if found == 0 {
			continue
		}
		brand := dripBrandFromCampaignName(c.name)
		entry := StampRecoveryCampaign{
			CampaignID: c.id, DatasetID: c.dataset, Brand: brand, Name: c.name, RowsFound: found,
		}
		rep.CampaignsWithLoss++
		rep.RowsFound += found

		if apply {
			var fixed int64
			if err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
				res, err := tx.ExecContext(ctx, stampRecoveryApplySQL,
					c.id, c.dataset, brand, MaxTouchCount, followupTouchGapHours)
				if err != nil {
					return err
				}
				fixed, err = res.RowsAffected()
				return err
			}); err != nil {
				log.Printf("[PartnerDripOrchestrator] stamp_recovery apply campaign=%s: %v", c.id, err)
				rep.Campaigns = append(rep.Campaigns, entry)
				continue
			}
			entry.RowsFixed = fixed
			rep.RowsFixed += fixed
			po.stamp.rowsRecovered.Add(fixed)
			po.markStampFailureRecovered(ctx, c.id, fixed)
			log.Printf("[PartnerDripOrchestrator] stamp_recovery RECOVERED campaign=%s brand=%s rows=%d", c.id, brand, fixed)
		} else {
			log.Printf("[PartnerDripOrchestrator] stamp_recovery DRY campaign=%s brand=%s rows_would_fix=%d", c.id, brand, found)
		}
		rep.Campaigns = append(rep.Campaigns, entry)
	}
	return rep, nil
}

// markStampFailureRecovered closes out the durable signal row. Best-effort: the
// queue rows are already fixed, and a missing ledger table must not turn a
// successful recovery into an error.
func (po *PartnerDripOrchestrator) markStampFailureRecovered(ctx context.Context, campaignID string, fixed int64) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, _ = po.db.ExecContext(wctx, `
		UPDATE partner_drip_stamp_failures
		SET recovered_at = NOW(), rows_recovered = $2
		WHERE campaign_id = $1::uuid
	`, campaignID, fixed)
}

// maybeRunStampRecovery is the tick hook. ALWAYS ON by default since 2026-08-22
// (kill switch: PARTNER_DRIP_STAMP_RECOVERY_DISABLED=1 — see
// currentStampRecoveryMode), and rate-limited to stampRecoveryInterval so a
// re-fired scheduler (every ECS bounce restarts this worker) cannot turn it
// into a per-tick sweep. tickOnce runs it BEFORE reconcileShippedClaims so the
// precise per-campaign replay lands before the coarse residual flip.
func (po *PartnerDripOrchestrator) maybeRunStampRecovery(ctx context.Context) {
	mode := currentStampRecoveryMode()
	if mode == stampRecoveryOff {
		return
	}
	if !po.lastStampRecovery.IsZero() && time.Since(po.lastStampRecovery) < stampRecoveryInterval {
		return
	}
	po.lastStampRecovery = time.Now()

	hours := stampRecoveryLookbackHours
	if v := strings.TrimSpace(os.Getenv("PARTNER_DRIP_STAMP_RECOVERY_HOURS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			hours = n
		}
	}
	rep, err := po.RecoverUnstampedTouches(ctx, hours, mode == stampRecoveryApply)
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] stamp_recovery pass failed (mode=%s): %v", mode, err)
		return
	}
	if rep.CampaignsWithLoss > 0 {
		log.Printf("[PartnerDripOrchestrator] stamp_recovery mode=%s scanned=%d campaigns_with_loss=%d rows_found=%d rows_fixed=%d",
			rep.Mode, rep.CampaignsScanned, rep.CampaignsWithLoss, rep.RowsFound, rep.RowsFixed)
	}
}

// dripBrandFromCampaignName recovers the drip brand from a campaign name minted
// by buildCampaignInput:
//
//	"[partner-drip] <vertical> <brand> <ts> <htmlhash> <nonce> [suffixes…]"
//
// Returns "" unless the prefix matches AND the extracted token is a known drip
// brand — a guess would poison mailed_brand, which the per-brand daily budget
// ledger counts on.
func dripBrandFromCampaignName(name string) string {
	const prefix = "[partner-drip] "
	if !strings.HasPrefix(name, prefix) {
		return ""
	}
	fields := strings.Fields(strings.TrimPrefix(name, prefix))
	if len(fields) < 3 {
		return ""
	}
	cand := strings.ToLower(fields[1])
	for _, b := range dripBrands {
		if strings.ToLower(b) == cand {
			return cand
		}
	}
	return ""
}
