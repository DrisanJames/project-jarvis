package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

type campaignVariant struct {
	VariantName string
	FromName    string
	Subject     string
	HTMLContent string
}

// capAwareClaimDisabled returns true when the wave dispatcher should
// revert to the legacy single-status claim path (no Peek, no reserve
// substitution). Set DISABLE_CAP_AWARE_CLAIM=true to disable. Default is
// enabled — the reserve pool only fills its purpose when this is on.
func capAwareClaimDisabled() bool {
	return os.Getenv("DISABLE_CAP_AWARE_CLAIM") == "true"
}

// setBasedEnqueueDisabled is the kill switch for the set-based wave enqueue
// (content snapshots + batched inserts). Set DISABLE_SETBASED_ENQUEUE=true to
// revert to the legacy row-at-a-time loop that copies the full creative into
// every queue row. Default is the set-based path — see
// docs/CAMPAIGN_QUEUE_STORAGE_REDESIGN.md §5/§7.
func setBasedEnqueueDisabled() bool {
	return os.Getenv("DISABLE_SETBASED_ENQUEUE") == "true"
}

// perWaveTouchCap is the max number of times a subscriber may be enqueued
// across ALL campaigns sharing the same wave slot (campaign.scheduled_at
// bucketed by perWaveWindowHours). When >0 it is the cross-campaign dedup: a
// person engaged with N brands gets ONE touch per wave instead of N. Enforced
// at enqueue via CapChecker.ClaimWaveSlot (one owner per slot), so it only
// applies to campaign waves (the partner-drip sender uses a separate dispatch
// path, untouched).
//
// Treated as an ON/OFF switch: >0 enables strict one-touch-per-wave (the
// owner-claim model is inherently 1-per-slot); 0 disables it.
//
// ───────────────────────────────────────────────────────────────────────────
// DEFAULT 0 (OFF) — cross-marketing ENABLED. Operator decision 2026-06-26:
// lane isolation (one-touch-per-wave) cut daily volume from ~500k to ~100k and
// lowered conversions, so we are back to cap-based sending and cross-market our
// engagers. BEFORE re-enabling this limiter, check back with the user (operator)
// first: this audience member engaged with the brand and it's THEIR
// responsibility to unsubscribe, not ours — and the ISPs are only going to get
// mad at the brand the audience member complains to. So an engager appearing in
// multiple brands' audiences SHOULD be mailed by each; do not silently restore
// the per-wave cap. Override via env PER_WAVE_TOUCH_CAP only on explicit
// operator instruction.
// ───────────────────────────────────────────────────────────────────────────
//
// Requires Redis when ON; with Redis down the cap fails open (ClaimWaveSlot
// returns allowed) and the send proceeds uncapped.
func perWaveTouchCap() int {
	v := strings.TrimSpace(os.Getenv("PER_WAVE_TOUCH_CAP"))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// perWaveWindowHours is the bucket width for the per-wave slot key. The
// campaign's scheduled_at is truncated to this many hours so every campaign
// in the same wave shares one slot bucket. Default 1h — fine when waves are
// spaced ≥1h apart (board waves are 4h apart). Min 1.
func perWaveWindowHours() int {
	v := strings.TrimSpace(os.Getenv("PER_WAVE_WINDOW_HOURS"))
	if v == "" {
		return 1
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 1
	}
	return n
}

// EnqueuePMTAWave materializes one due PMTA wave into the existing recipient queue.
//
// Logging (SA-7, per-domain engagement engine, 2026-05-09): the function
// emits exactly one structured "[wave_processor] ..." line per invocation,
// keyed by waveID + sendingDomain + campaignID, with duration_ms for
// throughput correlation. The named return values + deferred logger keep
// the contract intact across every error-return point in the body without
// having to touch each one. Pure observability — never gate behavior on
// these fields.
//
// Cap-aware reserve pool (Slice 4): when capChecker is non-nil and
// DISABLE_CAP_AWARE_CLAIM is not set, the candidate claim pulls from both
// status='selected' and status='reserve' (over-pulling by 2x) and skips
// rows whose subscribers Peek as over-cap, substituting the next reserve
// in rank order. Pass nil for capChecker (or set the kill switch) to
// revert to the legacy single-status claim with no Peek.
func EnqueuePMTAWave(ctx context.Context, db *sql.DB, waveID string, capChecker *mailing.CapChecker) (enqueued int, retErr error) {
	start := time.Now()
	var (
		campaignID          uuid.UUID
		ispPlanID           uuid.UUID
		orgID               uuid.UUID
		waveStatus          string
		campaignStatus      string
		planStatus          string
		scheduledAt         time.Time
		campaignScheduledAt sql.NullTime
		plannedRecipients   int
		enqueuedRecipients  int
		sendingDomain       string
		partnerDripTag      sql.NullString
	)
	defer func() {
		durationMs := time.Since(start).Milliseconds()
		if retErr != nil {
			log.Printf("[wave_processor] wave=%s domain=%s ERROR: %v duration_ms=%d",
				waveID, sendingDomain, retErr, durationMs)
			return
		}
		log.Printf("[wave_processor] wave=%s domain=%s campaign=%s recipients_enqueued=%d duration_ms=%d",
			waveID, sendingDomain, campaignID, enqueued, durationMs)
	}()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	err = tx.QueryRowContext(ctx, `
		SELECT w.campaign_id, w.isp_plan_id, COALESCE(p.organization_id, c.organization_id),
		       w.status, COALESCE(c.status, 'draft'),
		       COALESCE(p.status, 'planned'), w.scheduled_at, c.scheduled_at, w.planned_recipients, w.enqueued_recipients, c.partner_drip_tag
		FROM mailing_campaign_waves w
		JOIN mailing_campaigns c ON c.id = w.campaign_id
		JOIN mailing_campaign_isp_plans p ON p.id = w.isp_plan_id
		WHERE w.id = $1
		FOR UPDATE
	`, waveID).Scan(&campaignID, &ispPlanID, &orgID, &waveStatus, &campaignStatus, &planStatus, &scheduledAt, &campaignScheduledAt, &plannedRecipients, &enqueuedRecipients, &partnerDripTag)
	if err != nil {
		return 0, err
	}

	switch waveStatus {
	case "completed", "cancelled", "failed", "dead_letter":
		return 0, tx.Commit()
	}

	// Resolve sending_domain for log correlation (SA-7). Separate
	// non-locking query — the FOR UPDATE above locks
	// waves/campaigns/isp_plans and we do not want to extend the lock
	// to sending_profiles. Failures here are swallowed because the
	// field is observability-only; placing the lookup after the
	// terminal-status fast path avoids a wasted query for already-
	// completed waves.
	if scanErr := tx.QueryRowContext(ctx, `
		SELECT COALESCE(sp.sending_domain, '')
		FROM mailing_campaigns c
		LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
		WHERE c.id = $1
	`, campaignID).Scan(&sendingDomain); scanErr != nil {
		sendingDomain = ""
	}
	if campaignStatus == "cancelled" || campaignStatus == "failed" || planStatus == "cancelled" || planStatus == "failed" || planStatus == "paused" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE mailing_campaign_waves
			SET status = 'cancelled', updated_at = NOW()
			WHERE id = $1
		`, waveID); err != nil {
			return 0, err
		}
		return 0, tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE mailing_campaign_waves
		SET status = 'enqueuing', started_at = COALESCE(started_at, NOW()), updated_at = NOW()
		WHERE id = $1
	`, waveID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE mailing_campaigns
		SET status = 'sending', started_at = COALESCE(started_at, NOW()), updated_at = NOW()
		WHERE id = $1 AND status IN ('draft', 'scheduled', 'preparing')
	`, campaignID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE mailing_campaign_isp_plans
		SET status = 'running', updated_at = NOW()
		WHERE id = $1 AND status IN ('planned', 'ready')
	`, ispPlanID); err != nil {
		return 0, err
	}

	remaining := plannedRecipients - enqueuedRecipients
	if remaining <= 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE mailing_campaign_waves
			SET status = 'completed', completed_at = NOW(), updated_at = NOW()
			WHERE id = $1
		`, waveID); err != nil {
			return 0, err
		}
		return 0, tx.Commit()
	}

	var campaignFromName, campaignFromEmail, campaignSubject, campaignHTML, campaignName, campaignPlain sql.NullString
	var contentLocked sql.NullBool
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(from_name, ''), COALESCE(from_email, ''), COALESCE(subject, ''),
		       COALESCE(html_content, ''), COALESCE(name, ''), COALESCE(plain_content, ''),
		       COALESCE(content_locked, FALSE)
		FROM mailing_campaigns WHERE id = $1
	`, campaignID).Scan(&campaignFromName, &campaignFromEmail, &campaignSubject, &campaignHTML, &campaignName, &campaignPlain, &contentLocked); err != nil {
		return 0, err
	}
	isContentLocked := contentLocked.Valid && contentLocked.Bool
	if isContentLocked {
		log.Printf("[WaveEnqueue] campaign %s content_locked=true — bypassing subject/HTML fingerprint mutations (honeypot still injected)", campaignID)
	}

	// Hash Fingerprint Diversification: use the campaign's own content directly.
	// Per-recipient HTML/subject mutations are applied in the queue insertion loop
	// to produce unique fingerprints without changing visible content.
	brandKey := deriveBrandKey(campaignFromEmail.String)
	_ = campaignFromName // from_name lives on the campaign; not needed per-recipient
	_ = campaignName     // campaign type detection reserved for future editorial path

	baseHTML := campaignHTML.String
	baseSubject := campaignSubject.String

	// URL sanitization still runs on the base content before per-recipient mutation
	sanitized := sanitizeVariantURLsAtDispatch([]campaignVariant{{HTMLContent: baseHTML}}, brandKey)
	baseHTML = sanitized[0].HTMLContent

	// Deterministic idempotency key namespace: uuidv5 of (campaign,
	// subscriber, wave). Parsed once here — both enqueue paths need it, and a
	// malformed waveID must fail the wave loudly rather than silently drop
	// the idempotency guarantee.
	waveUUID, waveParseErr := uuid.Parse(waveID)
	if waveParseErr != nil {
		return 0, fmt.Errorf("wave_id %q is not a valid UUID: %w", waveID, waveParseErr)
	}

	useCapAware := capChecker != nil && !capAwareClaimDisabled()

	// SK-4 producer fork (DARK BY DEFAULT): decide ONCE per wave whether this
	// wave's queue rows are PRODUCED to Kafka (send.commands.v1) instead of
	// INSERTed directly. kafkaRouteWave is false unless the operator opted this
	// wave/campaign in AND a producer is wired, so by default this is false and
	// the direct INSERT path below runs UNCHANGED (byte-identical to today).
	routeToKafka := kafkaRouteWave(waveID, campaignID.String())
	if routeToKafka {
		kafkaRoutedWaves++
		log.Printf("[WaveEnqueue] wave %s campaign %s ROUTED to Kafka send.commands.v1 (SK-4 primary transport)", waveID, campaignID)
	}

	// Per-wave 1-touch slot key: bucket the CAMPAIGN's scheduled_at (the slot
	// anchor — identical for every brand's campaign in this wave, and stable
	// across this campaign's sub-waves, unlike w.scheduled_at which staggers).
	// Falls back to the wave's own scheduled_at if the campaign has none.
	slotAnchor := scheduledAt
	if campaignScheduledAt.Valid && !campaignScheduledAt.Time.IsZero() {
		slotAnchor = campaignScheduledAt.Time
	}
	waveSlotBucket := slotAnchor.UTC().Truncate(time.Duration(perWaveWindowHours()) * time.Hour).Format("20060102T15")
	perWaveCap := perWaveTouchCap()
	// Scope: the per-wave 1-touch cap is for the operator's campaign BOARD
	// only. Partner-drip campaigns (partner_drip_tag set) run their own
	// per-dataset cadence and must NOT be deduped against each other or the
	// board, so disable the cap for them.
	if partnerDripTag.Valid && strings.TrimSpace(partnerDripTag.String) != "" {
		perWaveCap = 0
	}

	params := waveEnqueueParams{
		waveID:         waveID,
		waveUUID:       waveUUID,
		campaignID:     campaignID,
		ispPlanID:      ispPlanID,
		orgID:          orgID,
		scheduledAt:    scheduledAt,
		remaining:      remaining,
		baseHTML:       baseHTML,
		baseSubject:    baseSubject,
		plainContent:   campaignPlain.String,
		isLocked:       isContentLocked,
		brandKey:       brandKey,
		useCapAware:    useCapAware,
		routeToKafka:   routeToKafka,
		waveSlotBucket: waveSlotBucket,
		perWaveCap:     perWaveCap,
	}

	var queuedCount, skippedCount, capSkippedCount, reserveUsedCount int
	if setBasedEnqueueDisabled() {
		queuedCount, skippedCount, capSkippedCount, reserveUsedCount, err = enqueueWaveRowAtATime(ctx, tx, capChecker, params)
	} else {
		queuedCount, skippedCount, capSkippedCount, reserveUsedCount, err = enqueueWaveSetBased(ctx, tx, db, capChecker, params)
	}
	if err != nil {
		return 0, err
	}

	if skippedCount > 0 {
		log.Printf("[WaveEnqueue] wave %s: skipped %d recipients (Phase D compliance)", waveID, skippedCount)
	}
	if useCapAware && (capSkippedCount > 0 || reserveUsedCount > 0) {
		log.Printf("[WaveEnqueue] wave %s: cap_skipped=%d reserve_used=%d queued=%d remaining=%d",
			waveID, capSkippedCount, reserveUsedCount, queuedCount, remaining)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE mailing_campaign_waves
		SET enqueued_recipients = enqueued_recipients + $2,
		    cap_skip_count = cap_skip_count + $3,
		    reserve_used_count = reserve_used_count + $4,
		    status = 'completed',
		    completed_at = NOW(),
		    updated_at = NOW()
		WHERE id = $1
	`, waveID, queuedCount, capSkippedCount, reserveUsedCount); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE mailing_campaign_isp_plans
		SET enqueued_count = enqueued_count + $2,
		    status = CASE
		        WHEN audience_selected_count <= enqueued_count + $2 THEN 'completed'
		        ELSE 'running'
		    END,
		    updated_at = NOW()
		WHERE id = $1
	`, ispPlanID, queuedCount); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE mailing_campaigns
		SET queued_count = queued_count + $2, updated_at = NOW()
		WHERE id = $1
	`, campaignID, queuedCount); err != nil {
		return 0, err
	}

	return queuedCount, tx.Commit()
}

// waveEnqueueParams carries the per-wave context both enqueue paths need.
type waveEnqueueParams struct {
	waveID       string
	waveUUID     uuid.UUID
	campaignID   uuid.UUID
	ispPlanID    uuid.UUID
	orgID        uuid.UUID
	scheduledAt  time.Time
	remaining    int
	baseHTML     string
	baseSubject  string
	plainContent string
	isLocked     bool
	brandKey     string
	useCapAware  bool
	// routeToKafka (SK-4) routes this wave's queue rows through Kafka
	// (send.commands.v1) instead of the direct INSERT. Default false (dark).
	routeToKafka bool
	// waveSlotBucket identifies this wave's slot (campaign.scheduled_at
	// bucketed to perWaveWindowHours); shared by every campaign in the wave.
	// perWaveCap is the max touches per subscriber per slot (0 = disabled).
	waveSlotBucket string
	perWaveCap     int
}

// enqueueWaveRowAtATime is the legacy enqueue path (pre set-based cutover):
// per-recipient compliance round trips and a full creative copy into every
// queue row. Kept verbatim behind DISABLE_SETBASED_ENQUEUE=true.
func enqueueWaveRowAtATime(ctx context.Context, tx *sql.Tx, capChecker *mailing.CapChecker, p waveEnqueueParams) (queuedCount, skippedCount, capSkippedCount, reserveUsedCount int, retErr error) {
	var rows *sql.Rows
	var err error
	if p.useCapAware {
		claimLimit := p.remaining * 2
		if claimLimit < p.remaining {
			claimLimit = p.remaining
		}
		rows, err = tx.QueryContext(ctx, `
			SELECT id, subscriber_id, email, recipient_isp, selection_rank,
			       audience_source_type, audience_source_id, status
			FROM mailing_campaign_plan_recipients
			WHERE isp_plan_id = $1
			  AND status IN ('selected','reserve')
			ORDER BY (CASE WHEN status = 'selected' THEN 0 ELSE 1 END) ASC,
			         selection_rank ASC
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		`, p.ispPlanID, claimLimit)
	} else {
		rows, err = tx.QueryContext(ctx, `
			SELECT id, subscriber_id, email, recipient_isp, selection_rank,
			       audience_source_type, audience_source_id, 'selected'::text AS status
			FROM mailing_campaign_plan_recipients
			WHERE isp_plan_id = $1
			  AND status = 'selected'
			ORDER BY selection_rank ASC
			LIMIT $2
			FOR UPDATE
		`, p.ispPlanID, p.remaining)
	}
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer rows.Close()

	type queueCandidate struct {
		recordID           uuid.UUID
		subscriberID       uuid.UUID
		email              string
		recipientISP       string
		selectionRank      int
		audienceSourceType string
		audienceSourceID   sql.NullString
		// planRecStatus is either "selected" or "reserve" — used to
		// (a) report reserve_used_count on the wave and (b) update the
		// plan_recipient row's prior status when we mark it as 'queued'
		// or 'cap_skipped'.
		planRecStatus string
	}

	var candidates []queueCandidate
	for rows.Next() {
		var rec queueCandidate
		if err := rows.Scan(&rec.recordID, &rec.subscriberID, &rec.email, &rec.recipientISP, &rec.selectionRank, &rec.audienceSourceType, &rec.audienceSourceID, &rec.planRecStatus); err != nil {
			return 0, 0, 0, 0, err
		}
		candidates = append(candidates, rec)
	}

	var waveSlotSkipped int
	for _, rec := range candidates {
		// Stop as soon as we've satisfied the wave's planned size. The
		// cap-aware path over-pulls candidates so this is the early-exit
		// gate for the reserve substitution loop.
		if queuedCount >= p.remaining {
			break
		}
		// Phase D: last-second compliance guard — skip if subscriber is no longer active
		// or email is globally suppressed. This protects against unsubs/bounces that
		// occurred between audience snapshot and wave enqueue.
		//
		// We accept either status as the row's prior state because the
		// claim now spans selected+reserve in the cap-aware path.
		var subStatus string
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(status, 'active') FROM mailing_subscribers WHERE id = $1`,
			rec.subscriberID,
		).Scan(&subStatus); err == nil && subStatus != "active" && subStatus != "confirmed" {
			tx.ExecContext(ctx, `UPDATE mailing_campaign_plan_recipients SET status = 'skipped' WHERE id = $1 AND status IN ('selected','reserve')`, rec.recordID)
			skippedCount++
			continue
		}
		var suppExists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM mailing_global_suppressions WHERE md5_hash = md5($1))`,
			strings.ToLower(rec.email),
		).Scan(&suppExists); err == nil && suppExists {
			tx.ExecContext(ctx, `UPDATE mailing_campaign_plan_recipients SET status = 'skipped' WHERE id = $1 AND status IN ('selected','reserve')`, rec.recordID)
			skippedCount++
			continue
		}

		// Cap-aware Peek (Slice 4): if the subscriber has already hit
		// the cross-brand daily cap from earlier brands, marking the
		// plan_recipient 'cap_skipped' frees the slot for the next
		// reserve in line. The worker still does the authoritative
		// Reserve in applyCrossBrandCap — Peek is advisory and may
		// return false negatives under Redis hiccups; we let those rows
		// through and let the worker decide.
		if p.useCapAware {
			overCap, _, peekErr := capChecker.Peek(ctx, p.orgID.String(), rec.subscriberID.String())
			if peekErr == nil && overCap {
				if _, execErr := tx.ExecContext(ctx,
					`UPDATE mailing_campaign_plan_recipients SET status = 'cap_skipped' WHERE id = $1 AND status IN ('selected','reserve')`,
					rec.recordID,
				); execErr != nil {
					return 0, 0, 0, 0, execErr
				}
				capSkippedCount++
				continue
			}
		}

		// Per-wave 1-touch claim: if another campaign in this same wave slot
		// already claimed this subscriber, mark cap_skipped (freeing the slot
		// for the next reserve in line) so the person is mailed once per wave
		// across all brands, not once per brand. Fail-open inside ClaimWaveSlot.
		if p.perWaveCap > 0 && capChecker != nil {
			allowed, _ := capChecker.ClaimWaveSlot(ctx, rec.subscriberID.String(), p.waveSlotBucket, p.waveID)
			if !allowed {
				if _, execErr := tx.ExecContext(ctx,
					`UPDATE mailing_campaign_plan_recipients SET status = 'cap_skipped' WHERE id = $1 AND status IN ('selected','reserve')`,
					rec.recordID,
				); execErr != nil {
					return 0, 0, 0, 0, execErr
				}
				capSkippedCount++
				waveSlotSkipped++
				continue
			}
		}

		seed := computeMutationSeed(rec.subscriberID, p.waveID)
		var recipientHTML, recipientSubject string
		if p.isLocked {
			recipientHTML = p.baseHTML
			recipientSubject = p.baseSubject
		} else {
			recipientHTML = mutateHTMLHash(p.baseHTML, seed)
			recipientSubject = mutateSubjectLine(p.baseSubject, seed, p.brandKey)
		}
		recipientHTML = injectHoneypotLink(recipientHTML, rec.subscriberID.String())

		var sourceID interface{}
		if rec.audienceSourceID.Valid {
			parsed, err := uuid.Parse(rec.audienceSourceID.String)
			if err == nil {
				sourceID = parsed
			}
		}
		// Deterministic idempotency key: uuidv5 of (campaign, subscriber, wave).
		// Re-running the wave enqueue for any reason (retry, leader change,
		// crash recovery) will produce the same key, and the partial unique
		// index uq_mcq_idempotency_key will reject the duplicate row via
		// ON CONFLICT DO NOTHING.
		idempotencyKey := outboxIdempotencyKey(p.campaignID, rec.subscriberID, p.waveUUID)
		queueID := uuid.New()

		// SK-5 HARD SEND PATH: when this wave is routed to Kafka, PRODUCE the
		// full-row command to send.commands.v1 INSTEAD of inserting directly; the
		// QueueWriterConsumer runs the SAME INSERT. Kafka is now the HARD send path:
		// on ANY produce failure we DO NOT fall back to a direct INSERT — we fail
		// the wave enqueue loudly. The surrounding transaction rolls back (no
		// partial INSERTs land), the plan_recipients stay 'selected', and the
		// PMTAWaveScheduler re-dispatches the wave. No recipient is dropped; the
		// row is simply written later, via Kafka. When NOT routed (the default,
		// dark path), the direct INSERT below runs exactly as before.
		if p.routeToKafka {
			cmd := buildSendCommand(
				queueID, p.campaignID, rec.subscriberID, p.waveUUID, p.ispPlanID, idempotencyKey,
				recipientSubject, recipientHTML, p.plainContent, rec.recipientISP, rec.audienceSourceType, sourceIDString(sourceID),
				rec.selectionRank, p.scheduledAt, uuid.Nil, uuid.Nil,
			)
			if perr := produceQueueCommand(ctx, cmd); perr != nil {
				log.Printf("[kafka-route] PRODUCE FAILED wave=%s key=%s (%v) — NOT falling back; Kafka is the hard send path. Failing wave enqueue for re-dispatch.", p.waveID, idempotencyKey, perr)
				return 0, 0, 0, 0, fmt.Errorf("kafka-route: produce failed for wave %s key %s: %w", p.waveID, idempotencyKey, perr)
			}
		} else {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO mailing_campaign_queue (
					id, campaign_id, subscriber_id, subject, html_content, plain_content,
					status, priority, scheduled_at, created_at, isp_plan_id, wave_id,
					recipient_isp, selection_rank, audience_source_type, audience_source_id,
					idempotency_key
				) VALUES (
					$1, $2, $3, $4, $5, $13,
					'queued', 5, $6, NOW(), $7, $8,
					$9, $10, $11, $12, $14
				)
				ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
			`, queueID, p.campaignID, rec.subscriberID, recipientSubject, recipientHTML,
				p.scheduledAt, p.ispPlanID, p.waveID, rec.recipientISP, rec.selectionRank, rec.audienceSourceType, sourceID,
				p.plainContent, idempotencyKey,
			); err != nil {
				return 0, 0, 0, 0, err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE mailing_campaign_plan_recipients
			SET status = 'queued', queued_at = NOW(), wave_id = $2
			WHERE id = $1
		`, rec.recordID, p.waveID); err != nil {
			return 0, 0, 0, 0, err
		}
		queuedCount++
		if rec.planRecStatus == "reserve" {
			reserveUsedCount++
		}
	}

	if waveSlotSkipped > 0 {
		log.Printf("[WaveEnqueue] wave %s slot=%s: per_wave_skipped=%d (1-touch-per-wave, cap=%d)",
			p.waveID, p.waveSlotBucket, waveSlotSkipped, p.perWaveCap)
	}

	return queuedCount, skippedCount, capSkippedCount, reserveUsedCount, nil
}

// setBasedCandidate is one claimed plan_recipient row plus the compliance
// verdicts computed inside the claim statement itself (subscriber status via
// LEFT JOIN, global suppression via EXISTS) — replacing the legacy path's two
// per-recipient round trips.
type setBasedCandidate struct {
	recordID           uuid.UUID
	subscriberID       uuid.UUID
	email              string
	recipientISP       string
	selectionRank      int
	audienceSourceType string
	audienceSourceID   sql.NullString
	planRecStatus      string
	subscriberStatus   string
	suppressed         bool
}

// enqueueWaveSetBased is the Phase A+B enqueue path
// (docs/CAMPAIGN_QUEUE_STORAGE_REDESIGN.md §5/§7): the wave's base creative is
// written ONCE to mailing_content_snapshots and every queue row references it
// by id; compliance verdicts ride the claim statement; queue inserts and
// plan_recipient transitions are batched. The whole wave is ~6 statements
// regardless of size, so the wave/campaign/plan row locks held by the
// surrounding transaction last milliseconds instead of the full per-recipient
// loop duration.
//
// Behavioral parity with enqueueWaveRowAtATime, by construction:
//   - same candidate ordering, early exit, and reserve substitution;
//   - same compliance semantics (missing subscriber row counts as active;
//     suppression keyed on md5(lower(email)));
//   - subject mutation still happens here (CPU-only, stays inline on the row);
//   - HTML mutation + honeypot move to send time via renderSnapshotBody,
//     which is deterministic in (subscriber_id, wave_id) — identical bytes;
//   - identical idempotency keys and ON CONFLICT semantics.
func enqueueWaveSetBased(ctx context.Context, tx *sql.Tx, db *sql.DB, capChecker *mailing.CapChecker, p waveEnqueueParams) (queuedCount, skippedCount, capSkippedCount, reserveUsedCount int, retErr error) {
	// Snapshot first, on db (autocommit) — committed before any queue row
	// can reference it, and shared across waves by content_hash.
	snapshotID, err := ensureContentSnapshot(ctx, db, p.campaignID, p.waveID, p.baseHTML, p.plainContent, p.isLocked)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("content snapshot: %w", err)
	}

	// Wave-path A/B split (ab_split.go): nil unless the campaign has >=2 usable
	// mailing_ab_variants and DISABLE_WAVE_AB_SPLIT is unset. With variants, each
	// recipient gets a deterministic variant snapshot + creative_id stamp; without,
	// every code path below is byte-identical to before.
	abVariants := loadWaveABVariants(ctx, db, p.campaignID, p.waveID, p.plainContent, p.isLocked)

	var rows *sql.Rows
	if p.useCapAware {
		claimLimit := p.remaining * 2
		if claimLimit < p.remaining {
			claimLimit = p.remaining
		}
		rows, err = tx.QueryContext(ctx, `
			SELECT pr.id, pr.subscriber_id, pr.email, pr.recipient_isp, pr.selection_rank,
			       pr.audience_source_type, pr.audience_source_id, pr.status,
			       COALESCE(s.status, 'active'),
			       EXISTS(SELECT 1 FROM mailing_global_suppressions g WHERE g.md5_hash = md5(lower(pr.email)))
			FROM mailing_campaign_plan_recipients pr
			LEFT JOIN mailing_subscribers s ON s.id = pr.subscriber_id
			WHERE pr.isp_plan_id = $1
			  AND pr.status IN ('selected','reserve')
			ORDER BY (CASE WHEN pr.status = 'selected' THEN 0 ELSE 1 END) ASC,
			         pr.selection_rank ASC
			LIMIT $2
			FOR UPDATE OF pr SKIP LOCKED
		`, p.ispPlanID, claimLimit)
	} else {
		rows, err = tx.QueryContext(ctx, `
			SELECT pr.id, pr.subscriber_id, pr.email, pr.recipient_isp, pr.selection_rank,
			       pr.audience_source_type, pr.audience_source_id, 'selected'::text,
			       COALESCE(s.status, 'active'),
			       EXISTS(SELECT 1 FROM mailing_global_suppressions g WHERE g.md5_hash = md5(lower(pr.email)))
			FROM mailing_campaign_plan_recipients pr
			LEFT JOIN mailing_subscribers s ON s.id = pr.subscriber_id
			WHERE pr.isp_plan_id = $1
			  AND pr.status = 'selected'
			ORDER BY pr.selection_rank ASC
			LIMIT $2
			FOR UPDATE OF pr
		`, p.ispPlanID, p.remaining)
	}
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer rows.Close()

	var candidates []setBasedCandidate
	for rows.Next() {
		var rec setBasedCandidate
		if err := rows.Scan(&rec.recordID, &rec.subscriberID, &rec.email, &rec.recipientISP, &rec.selectionRank,
			&rec.audienceSourceType, &rec.audienceSourceID, &rec.planRecStatus,
			&rec.subscriberStatus, &rec.suppressed); err != nil {
			return 0, 0, 0, 0, err
		}
		candidates = append(candidates, rec)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, 0, err
	}

	// Partition in memory. The only remaining per-candidate external call is
	// the advisory Redis cap Peek (same as legacy; the worker still does the
	// authoritative Reserve in applyCrossBrandCap).
	var (
		queueIDs, subscriberIDs, subjects, isps, sourceTypes, idemKeys, queuedPlanIDs []string
		ranks                                                                         []int64
		sourceIDs                                                                     []sql.NullString
		skippedPlanIDs, capSkippedPlanIDs                                             []string
		waveSlotSkipped                                                               int
		abSnapIDs, abCreativeIDs                                                      []string // parallel to queueIDs when abVariants active
	)
	for _, rec := range candidates {
		if queuedCount >= p.remaining {
			break
		}
		// Phase D compliance — same predicates as legacy, verdicts already
		// computed by the claim statement.
		if rec.subscriberStatus != "active" && rec.subscriberStatus != "confirmed" {
			skippedPlanIDs = append(skippedPlanIDs, rec.recordID.String())
			skippedCount++
			continue
		}
		if rec.suppressed {
			skippedPlanIDs = append(skippedPlanIDs, rec.recordID.String())
			skippedCount++
			continue
		}
		if p.useCapAware {
			overCap, _, peekErr := capChecker.Peek(ctx, p.orgID.String(), rec.subscriberID.String())
			if peekErr == nil && overCap {
				capSkippedPlanIDs = append(capSkippedPlanIDs, rec.recordID.String())
				capSkippedCount++
				continue
			}
		}

		// Per-wave 1-touch claim (see enqueueWaveRowAtATime for rationale):
		// the first campaign in this wave slot to reach a subscriber claims
		// them; siblings see the slot full and skip → one touch per wave.
		if p.perWaveCap > 0 && capChecker != nil {
			allowed, _ := capChecker.ClaimWaveSlot(ctx, rec.subscriberID.String(), p.waveSlotBucket, p.waveID)
			if !allowed {
				capSkippedPlanIDs = append(capSkippedPlanIDs, rec.recordID.String())
				capSkippedCount++
				waveSlotSkipped++
				continue
			}
		}

		recipientSubject := p.baseSubject
		if !p.isLocked {
			recipientSubject = mutateSubjectLine(p.baseSubject, computeMutationSeed(rec.subscriberID, p.waveID), p.brandKey)
		}

		idempotencyKey := outboxIdempotencyKey(p.campaignID, rec.subscriberID, p.waveUUID)
		queueID := uuid.New()
		normSource := normalizeSourceID(rec.audienceSourceID)

		// The plan_recipient ALWAYS transitions to 'queued' once accepted —
		// whether the row goes to Kafka or the direct INSERT — so it is never
		// re-claimed by a later pass. (Counted below.)
		queuedPlanIDs = append(queuedPlanIDs, rec.recordID.String())
		queuedCount++
		if rec.planRecStatus == "reserve" {
			reserveUsedCount++
		}

		// SK-5 HARD SEND PATH: when routed, PRODUCE the full-row command to
		// send.commands.v1 (the set-based path leaves HTML/plain empty and carries
		// content_snapshot_id, exactly like the direct INSERT's NULL,NULL + $5).
		// Kafka is now the HARD send path: on ANY produce failure we DO NOT fall
		// back to a direct INSERT — we fail the wave enqueue loudly. The
		// surrounding transaction rolls back (the batched INSERT below never runs,
		// no partial rows land), the plan_recipients stay 'selected', and the
		// PMTAWaveScheduler re-dispatches the wave. No recipient is dropped; the
		// rows are written later, via Kafka. When NOT routed (default), every
		// recipient goes to the direct-INSERT arrays unchanged.
		if p.routeToKafka {
			rowSnap, rowCreative := snapshotID, uuid.Nil
			if len(abVariants) > 0 {
				v := abVariants[pickWaveABVariant(rec.subscriberID, abVariants)]
				rowSnap, rowCreative = v.SnapshotID, v.CreativeID
			}
			cmd := buildSendCommand(
				queueID, p.campaignID, rec.subscriberID, p.waveUUID, p.ispPlanID, idempotencyKey,
				recipientSubject, "", "", rec.recipientISP, rec.audienceSourceType, nullStringToSource(normSource),
				rec.selectionRank, p.scheduledAt, rowSnap, rowCreative,
			)
			if perr := produceQueueCommand(ctx, cmd); perr != nil {
				log.Printf("[kafka-route] PRODUCE FAILED wave=%s key=%s (%v) — NOT falling back; Kafka is the hard send path. Failing wave enqueue for re-dispatch.", p.waveID, idempotencyKey, perr)
				return 0, 0, 0, 0, fmt.Errorf("kafka-route: produce failed for wave %s key %s: %w", p.waveID, idempotencyKey, perr)
			}
			continue // written to Kafka; skip the direct-INSERT arrays
		}

		sourceIDs = append(sourceIDs, normSource)
		queueIDs = append(queueIDs, queueID.String())
		subscriberIDs = append(subscriberIDs, rec.subscriberID.String())
		subjects = append(subjects, recipientSubject)
		isps = append(isps, rec.recipientISP)
		ranks = append(ranks, int64(rec.selectionRank))
		sourceTypes = append(sourceTypes, rec.audienceSourceType)
		idemKeys = append(idemKeys, idempotencyKey.String())
		if len(abVariants) > 0 {
			v := abVariants[pickWaveABVariant(rec.subscriberID, abVariants)]
			abSnapIDs = append(abSnapIDs, v.SnapshotID.String())
			abCreativeIDs = append(abCreativeIDs, v.CreativeID.String())
		}
	}

	if len(skippedPlanIDs) > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE mailing_campaign_plan_recipients SET status = 'skipped' WHERE id = ANY($1::uuid[]) AND status IN ('selected','reserve')`,
			pq.Array(skippedPlanIDs)); err != nil {
			return 0, 0, 0, 0, err
		}
	}
	if len(capSkippedPlanIDs) > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE mailing_campaign_plan_recipients SET status = 'cap_skipped' WHERE id = ANY($1::uuid[]) AND status IN ('selected','reserve')`,
			pq.Array(capSkippedPlanIDs)); err != nil {
			return 0, 0, 0, 0, err
		}
	}
	if len(queueIDs) > 0 && len(abVariants) > 0 {
		// A/B branch: per-row content_snapshot_id + creative_id (= ab_variant id)
		// carried in two extra parallel arrays. Everything else identical to the
		// single-snapshot statement below.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mailing_campaign_queue (
				id, campaign_id, subscriber_id, subject, html_content, plain_content,
				status, priority, scheduled_at, created_at, isp_plan_id, wave_id,
				recipient_isp, selection_rank, audience_source_type, audience_source_id,
				idempotency_key, content_snapshot_id, creative_id
			)
			SELECT t.id, $1, t.subscriber_id, t.subject, NULL, NULL,
			       'queued', 5, $2, NOW(), $3, $4,
			       t.recipient_isp, t.selection_rank, t.audience_source_type, t.audience_source_id,
			       t.idempotency_key, t.snapshot_id, t.creative_id
			FROM unnest(
				$5::uuid[], $6::uuid[], $7::text[], $8::text[], $9::int[],
				$10::text[], $11::uuid[], $12::uuid[], $13::uuid[], $14::uuid[]
			) AS t(id, subscriber_id, subject, recipient_isp, selection_rank,
			       audience_source_type, audience_source_id, idempotency_key, snapshot_id, creative_id)
			ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		`, p.campaignID, p.scheduledAt, p.ispPlanID, p.waveUUID,
			pq.Array(queueIDs), pq.Array(subscriberIDs), pq.Array(subjects), pq.Array(isps), pq.Array(ranks),
			pq.Array(sourceTypes), pq.Array(sourceIDs), pq.Array(idemKeys),
			pq.Array(abSnapIDs), pq.Array(abCreativeIDs),
		); err != nil {
			return 0, 0, 0, 0, err
		}
	} else if len(queueIDs) > 0 {
		// html_content/plain_content stay NULL: the body lives once in the
		// snapshot; the send worker dereferences content_snapshot_id and
		// applies the deterministic per-recipient mutation.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mailing_campaign_queue (
				id, campaign_id, subscriber_id, subject, html_content, plain_content,
				status, priority, scheduled_at, created_at, isp_plan_id, wave_id,
				recipient_isp, selection_rank, audience_source_type, audience_source_id,
				idempotency_key, content_snapshot_id
			)
			SELECT t.id, $1, t.subscriber_id, t.subject, NULL, NULL,
			       'queued', 5, $2, NOW(), $3, $4,
			       t.recipient_isp, t.selection_rank, t.audience_source_type, t.audience_source_id,
			       t.idempotency_key, $5
			FROM unnest(
				$6::uuid[], $7::uuid[], $8::text[], $9::text[], $10::int[],
				$11::text[], $12::uuid[], $13::uuid[]
			) AS t(id, subscriber_id, subject, recipient_isp, selection_rank,
			       audience_source_type, audience_source_id, idempotency_key)
			ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		`, p.campaignID, p.scheduledAt, p.ispPlanID, p.waveUUID, snapshotID,
			pq.Array(queueIDs), pq.Array(subscriberIDs), pq.Array(subjects), pq.Array(isps), pq.Array(ranks),
			pq.Array(sourceTypes), pq.Array(sourceIDs), pq.Array(idemKeys),
		); err != nil {
			return 0, 0, 0, 0, err
		}
	}
	// The plan_recipient queued-transition is driven by queuedPlanIDs, NOT
	// queueIDs: a routed wave PRODUCES its rows (so queueIDs is empty) but the
	// plan_recipients must STILL move to 'queued' so a later pass never re-claims
	// them. In the un-routed (default) path queuedPlanIDs == queueIDs's
	// recipients, so this is byte-identical to before.
	if len(queuedPlanIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE mailing_campaign_plan_recipients
			SET status = 'queued', queued_at = NOW(), wave_id = $2
			WHERE id = ANY($1::uuid[])
		`, pq.Array(queuedPlanIDs), p.waveUUID); err != nil {
			return 0, 0, 0, 0, err
		}
	}

	if waveSlotSkipped > 0 {
		log.Printf("[WaveEnqueue] wave %s slot=%s: per_wave_skipped=%d (1-touch-per-wave, cap=%d)",
			p.waveID, p.waveSlotBucket, waveSlotSkipped, p.perWaveCap)
	}

	return queuedCount, skippedCount, capSkippedCount, reserveUsedCount, nil
}

// normalizeSourceID mirrors the legacy per-row handling of
// plan_recipients.audience_source_id: a value that parses as a UUID is kept,
// anything else becomes SQL NULL.
func normalizeSourceID(raw sql.NullString) sql.NullString {
	if !raw.Valid {
		return sql.NullString{}
	}
	parsed, err := uuid.Parse(raw.String)
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: parsed.String(), Valid: true}
}

func loadCampaignVariantsForWave(ctx context.Context, tx *sql.Tx, campaignID, fallbackFromName, fallbackSubject, fallbackHTML string) ([]campaignVariant, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT COALESCE(v.variant_name, ''),
		       COALESCE(NULLIF(v.from_name, ''), $2),
		       COALESCE(NULLIF(v.subject, ''), $3),
		       COALESCE(NULLIF(v.html_content, ''), $4)
		FROM mailing_ab_variants v
		JOIN mailing_ab_tests t ON t.id = v.test_id
		WHERE t.campaign_id = $1
		ORDER BY v.variant_name ASC
	`, campaignID, fallbackFromName, fallbackSubject, fallbackHTML)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var variants []campaignVariant
	for rows.Next() {
		var v campaignVariant
		if err := rows.Scan(&v.VariantName, &v.FromName, &v.Subject, &v.HTMLContent); err != nil {
			return nil, err
		}
		variants = append(variants, v)
	}
	if len(variants) == 0 {
		variants = append(variants, campaignVariant{
			VariantName: "A",
			FromName:    fallbackFromName,
			Subject:     fallbackSubject,
			HTMLContent: fallbackHTML,
		})
	}
	return variants, nil
}

func coalesceWaveValue(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// deriveBrandKey extracts the brand key from a from_email address.
// "hello@em.discountblog.com" → "discountblog"
// "hello@em.quizfiesta.com"   → "quizfiesta"
func deriveBrandKey(fromEmail string) string {
	at := strings.LastIndex(fromEmail, "@")
	if at < 0 {
		return ""
	}
	domain := strings.ToLower(fromEmail[at+1:])
	domain = strings.TrimPrefix(domain, "em.")
	domain = strings.TrimPrefix(domain, "m.")
	dot := strings.Index(domain, ".")
	if dot > 0 {
		return domain[:dot]
	}
	return domain
}

// detectCampaignType infers the campaign type from its name.
func detectCampaignType(name string) string {
	lower := strings.ToLower(name)
	if strings.Contains(lower, "welcome") {
		return "welcome"
	}
	if strings.Contains(lower, "trivia") || strings.Contains(lower, "quiz") {
		return "trivia"
	}
	return "newsletter"
}

// buildVariantFromEditorial takes cached editorial JSON and re-renders it into
// the campaign's own HTML template. If editorial JSON is available and the
// campaign HTML has placeholders, the editorial is merged in. Otherwise, only
// the subject/preview are varied.
func buildVariantFromEditorial(cached *CachedWaveContent, fallbackFromName, fallbackSubject, campaignHTML, brandKey string) campaignVariant {
	subject := coalesceWaveValue(cached.Variation.Subject, fallbackSubject)
	fromName := coalesceWaveValue(cached.Variation.FromName, fallbackFromName)

	htmlOut := campaignHTML
	if len(cached.EditorialJSON) > 0 && hasPlaceholders(campaignHTML) {
		var editorial mailing.WaveEditorialContent
		if err := json.Unmarshal(cached.EditorialJSON, &editorial); err == nil {
			req := mailing.WaveContentRequest{
				HTMLTemplate: campaignHTML,
				BrandName:    brandKey,
			}
			rendered := mailing.TemplateFillWithVariation([]mailing.WaveEditorialContent{editorial}, req)
			if len(rendered) > 0 {
				htmlOut = rendered[0].HTMLContent
				subject = coalesceWaveValue(rendered[0].Subject, subject)
			}
		} else {
			log.Printf("[wave-dispatch] failed to parse editorial JSON: %v", err)
		}
	} else if len(cached.EditorialJSON) == 0 && hasPlaceholders(campaignHTML) {
		log.Printf("[wave-dispatch] campaign has placeholders but no editorial JSON — using cached rendered HTML")
		htmlOut = cached.Variation.HTMLContent
	}

	return campaignVariant{
		VariantName: fmt.Sprintf("wave-%d", cached.Variation.WaveIndex),
		FromName:    fromName,
		Subject:     subject,
		HTMLContent: htmlOut,
	}
}

func hasPlaceholders(html string) bool {
	return strings.Contains(html, "{{INTRO}}") || strings.Contains(html, "{{ARTICLE_1_HEADLINE}}")
}

// brandURLFallbacks maps brand keys to their fallback redirect target.
// AI-generated article slugs that don't match any known path are rewritten
// to the fallback URL (typically the blog root) so users land on real content.
var brandURLFallbacks = map[string]struct {
	Domain     string
	Fallback   string
	KnownPaths []string
}{
	"historythinking": {
		Domain:   "historythinking.com",
		Fallback: "/blog",
		KnownPaths: []string{
			"/blog", "/privacy", "/terms", "/about", "/auth",
			"/blog/category/ancient-civilizations",
			"/blog/category/american-history",
			"/blog/category/cultural-history",
			"/blog/category/historical-figures",
			"/blog/category/medieval-world",
			"/blog/category/revolutionary-movements",
			"/blog/category/science-and-discovery",
			"/blog/category/world-wars",
		},
	},
}

// sanitizeVariantURLsAtDispatch rewrites hallucinated article URLs to the
// brand's blog root. AI sometimes fabricates article slugs that 404; this
// catches them at dispatch time and redirects to real content.
func sanitizeVariantURLsAtDispatch(variants []campaignVariant, brandKey string) []campaignVariant {
	rule, ok := brandURLFallbacks[brandKey]
	if !ok {
		return variants
	}

	baseURL := "https://" + rule.Domain
	re := regexp.MustCompile(`href="` + regexp.QuoteMeta(baseURL) + `/([^"]+)"`)

	for i := range variants {
		html := variants[i].HTMLContent
		replaced := false
		html = re.ReplaceAllStringFunc(html, func(match string) string {
			slug := re.FindStringSubmatch(match)
			if len(slug) < 2 {
				return match
			}
			path := "/" + slug[1]
			for _, kp := range rule.KnownPaths {
				if path == kp || strings.HasPrefix(path, kp+"/") {
					return match
				}
			}
			replaced = true
			return `href="` + baseURL + rule.Fallback + `"`
		})
		if replaced {
			variants[i].HTMLContent = html
			log.Printf("[wave-dispatch-urlfix] rewrote hallucinated URLs to %s%s for variant %s (brand: %s)",
				baseURL, rule.Fallback, variants[i].VariantName, brandKey)
		}
	}
	return variants
}
