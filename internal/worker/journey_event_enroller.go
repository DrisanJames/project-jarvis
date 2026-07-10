package worker

// JourneyEventEnroller is the consumer side of the click-drip queue.
//
// Producer: EverflowClickPostbackHandler
//   (internal/api/everflow_click_postback_handler.go) writes a row to
//   mailing_journey_event_triggers with status='pending' on every legit
//   Everflow click postback.
//
// Consumer: this worker, polling every 5 seconds.
//   1. SELECT … WHERE status='pending' ORDER BY received_at ASC
//      LIMIT N FOR UPDATE SKIP LOCKED  — concurrency-safe drain.
//   2. For each trigger:
//        - Resolve the configured click journey from
//          mailing_offer_journey_map (re-checking enabled / payout type;
//          operator may have flipped a switch since the postback).
//        - Re-check mailing_offer_suppressions['converted'] — the
//          subscriber may have converted in the seconds between click
//          and enrollment; do not enroll converted users.
//        - Pick the first non-trigger node in the journey graph as
//          current_node_id (matches JourneySegmentEnroller pattern).
//        - INSERT INTO mailing_journey_enrollments with metadata
//          carrying sending_profile_id, sub2_brand, click_url, etc.
//          The email-node executor (Phase 3) will read this metadata
//          to drive sending profile + reminder subject overrides.
//        - Mark the trigger row 'processed' (or 'skipped' + reason).
//   3. The poll cadence is intentionally tight (5s) because operators
//      want a +1h reminder to be exactly +1h, not +1h05m.
//
// Failure model: any unhandled error inside processOne will leave the
// trigger as 'pending' (no UPDATE) and the next tick retries it. We
// avoid a retry counter because the queue is bounded to the click
// volume (a few hundred per day initially) and failures are rare.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
)

// DefaultEventEnrollerInterval — operator-tuned cadence. 5s keeps the
// click → first-email latency under 10s in steady state.
const DefaultEventEnrollerInterval = 5 * time.Second

// DefaultEventEnrollerBatchLimit caps rows drained per tick. With 5s
// cadence this is 24,000 enrollments/min ceiling — far above any realistic
// click volume but bounded so a backlog doesn't choke the DB.
const DefaultEventEnrollerBatchLimit = 200

// JourneyEventEnroller is the worker handle. Construct via
// NewJourneyEventEnroller, then Start(ctx) once at boot. Stop() is safe
// to call multiple times.
type JourneyEventEnroller struct {
	db         *sql.DB
	interval   time.Duration
	batchLimit int
	stopChan   chan struct{}
	stopOnce   sync.Once

	// routingColsOK is re-probed at the top of every tick (single goroutine):
	// false = the jul10_lane_governor_* columns are absent (pre-migration
	// window) and processOne must use the legacy offer-map query shape.
	routingColsOK bool

	totalProcessed int64
	totalEnrolled  int64
	totalRearmed   int64
	totalSkipped   int64
	totalErrors    int64
}

// NewJourneyEventEnroller wires the worker.
func NewJourneyEventEnroller(db *sql.DB) *JourneyEventEnroller {
	return &JourneyEventEnroller{
		db:         db,
		interval:   DefaultEventEnrollerInterval,
		batchLimit: DefaultEventEnrollerBatchLimit,
		stopChan:   make(chan struct{}),
	}
}

// WithInterval overrides the poll cadence (tests / incidents).
func (w *JourneyEventEnroller) WithInterval(d time.Duration) *JourneyEventEnroller {
	w.interval = d
	return w
}

// WithBatchLimit overrides the per-tick drain cap.
func (w *JourneyEventEnroller) WithBatchLimit(n int) *JourneyEventEnroller {
	if n > 0 {
		w.batchLimit = n
	}
	return w
}

// Start runs the polling loop until ctx is cancelled or Stop() is
// called. Idempotent (calling more than once is a no-op for goroutine
// count via stopOnce).
func (w *JourneyEventEnroller) Start(ctx context.Context) {
	go func() {
		log.Printf("JourneyEventEnroller: started (interval=%s, batchLimit=%d)", w.interval, w.batchLimit)
		// Run once immediately so the first click after boot enrolls promptly.
		w.tick(ctx)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.tick(ctx)
			case <-w.stopChan:
				log.Println("JourneyEventEnroller: stopped")
				return
			case <-ctx.Done():
				log.Println("JourneyEventEnroller: context cancelled, stopping")
				return
			}
		}
	}()
}

// Stop terminates the polling loop. Idempotent.
func (w *JourneyEventEnroller) Stop() {
	w.stopOnce.Do(func() { close(w.stopChan) })
}

// Stats returns lifetime counters for observability. rearmed counts prior
// exited/completed enrollments re-armed for a fresh funnel pass (Unit A,
// 2026-07-10) — disjoint from enrolled (fresh INSERTs).
func (w *JourneyEventEnroller) Stats() (processed, enrolled, rearmed, skipped, errors int64) {
	return atomic.LoadInt64(&w.totalProcessed),
		atomic.LoadInt64(&w.totalEnrolled),
		atomic.LoadInt64(&w.totalRearmed),
		atomic.LoadInt64(&w.totalSkipped),
		atomic.LoadInt64(&w.totalErrors)
}

// tick drains one batch of pending triggers.
func (w *JourneyEventEnroller) tick(ctx context.Context) {
	tickCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Probe for the lane-governor columns BEFORE opening the batch tx. If the
	// jul10_lane_governor_* startup migrations haven't landed (5s-budget skip
	// on a contended boot), referencing routing_state inside the batch tx
	// would abort the tx and poison every remaining trigger in the tick — a
	// silent full enrollment stall. Probing outside the tx keeps the batch on
	// the legacy query shape until the columns exist. (Mirrors the scanner's
	// loadLaneRouting tolerance.)
	w.routingColsOK = true
	if _, err := w.db.ExecContext(tickCtx,
		`SELECT routing_state FROM mailing_offer_journey_map LIMIT 1`); err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			w.routingColsOK = false
		}
		// Any other probe error: leave routingColsOK=true; a real outage will
		// surface on the batch queries themselves.
	}

	// Use a transaction so FOR UPDATE SKIP LOCKED holds row locks
	// until we commit. Each tick processes at most batchLimit rows.
	tx, err := w.db.BeginTx(tickCtx, nil)
	if err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyEventEnroller: begin tx: %v", err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	rows, err := tx.QueryContext(tickCtx, `
		SELECT id, everflow_offer_id, subscriber_id,
		       COALESCE(subscriber_email, '') AS subscriber_email,
		       COALESCE(sub2_brand, '') AS sub2_brand,
		       COALESCE(sub3_campaign_id, '') AS sub3_campaign_id,
		       COALESCE(click_id, '') AS click_id,
		       sending_profile_id,
		       COALESCE(sending_domain, '') AS sending_domain,
		       COALESCE(click_url, '') AS click_url
		FROM mailing_journey_event_triggers
		WHERE status = 'pending'
		ORDER BY received_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, w.batchLimit)
	if err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyEventEnroller: select pending: %v", err)
		return
	}

	type trigger struct {
		ID               string
		EverflowOfferID  string
		SubscriberID     string
		SubscriberEmail  string
		Sub2Brand        string
		Sub3CampaignID   string
		ClickID          string
		SendingProfileID sql.NullString
		SendingDomain    string
		ClickURL         string
	}

	var batch []trigger
	for rows.Next() {
		var t trigger
		if err := rows.Scan(
			&t.ID, &t.EverflowOfferID, &t.SubscriberID, &t.SubscriberEmail,
			&t.Sub2Brand, &t.Sub3CampaignID, &t.ClickID,
			&t.SendingProfileID, &t.SendingDomain, &t.ClickURL,
		); err != nil {
			log.Printf("JourneyEventEnroller: scan: %v", err)
			continue
		}
		batch = append(batch, t)
	}
	rows.Close()

	for _, t := range batch {
		atomic.AddInt64(&w.totalProcessed, 1)
		outcome := w.processOne(tickCtx, tx, t.ID, t.EverflowOfferID, t.SubscriberID, t.SubscriberEmail,
			t.Sub2Brand, t.Sub3CampaignID, t.ClickID, t.SendingProfileID, t.SendingDomain, t.ClickURL)
		switch outcome.kind {
		case enrollmentEnrolled:
			atomic.AddInt64(&w.totalEnrolled, 1)
		case enrollmentRearmed:
			atomic.AddInt64(&w.totalRearmed, 1)
		case enrollmentSkipped:
			atomic.AddInt64(&w.totalSkipped, 1)
		case enrollmentErrored:
			atomic.AddInt64(&w.totalErrors, 1)
		}
	}

	if err := tx.Commit(); err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyEventEnroller: commit: %v", err)
		return
	}
	committed = true

	if len(batch) > 0 {
		log.Printf("JourneyEventEnroller: tick processed %d triggers", len(batch))
	}
}

type enrollmentOutcomeKind int

const (
	enrollmentEnrolled enrollmentOutcomeKind = iota + 1
	enrollmentRearmed
	enrollmentSkipped
	enrollmentErrored
)

type enrollmentOutcome struct {
	kind   enrollmentOutcomeKind
	reason string
}

// processOne handles a single locked trigger row. All status updates
// happen on the same tx so they commit atomically with the row lock
// release.
func (w *JourneyEventEnroller) processOne(
	ctx context.Context,
	tx *sql.Tx,
	triggerID, everflowOfferID, subscriberIDStr, subscriberEmail,
	sub2Brand, sub3CampaignID, clickID string,
	sendingProfileID sql.NullString,
	sendingDomain, clickURL string,
) enrollmentOutcome {

	subscriberID, err := uuid.Parse(subscriberIDStr)
	if err != nil || subscriberID == uuid.Nil {
		w.markSkipped(ctx, tx, triggerID, "invalid_subscriber_id")
		return enrollmentOutcome{kind: enrollmentSkipped, reason: "invalid_subscriber_id"}
	}

	// Re-validate the offer-journey map. Operator may have flipped 'enabled'
	// or changed the journey since the postback was queued.
	var journeyID, payoutType, routingState string
	var enabled bool
	if w.routingColsOK {
		err = tx.QueryRowContext(ctx, `
			SELECT COALESCE(click_journey_id,''), COALESCE(payout_type,'UNKNOWN'), COALESCE(enabled,false), COALESCE(routing_state,'active')
			FROM mailing_offer_journey_map WHERE everflow_offer_id=$1
		`, everflowOfferID).Scan(&journeyID, &payoutType, &enabled, &routingState)
	} else {
		// Pre-migration schema (see the tick() probe): legacy column set,
		// routing_state behaves as 'active'.
		routingState = "active"
		err = tx.QueryRowContext(ctx, `
			SELECT COALESCE(click_journey_id,''), COALESCE(payout_type,'UNKNOWN'), COALESCE(enabled,false)
			FROM mailing_offer_journey_map WHERE everflow_offer_id=$1
		`, everflowOfferID).Scan(&journeyID, &payoutType, &enabled)
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.markSkipped(ctx, tx, triggerID, "offer_unmapped_at_processing")
			return enrollmentOutcome{kind: enrollmentSkipped, reason: "offer_unmapped"}
		}
		log.Printf("JourneyEventEnroller[%s]: lookup offer map: %v", triggerID, err)
		return enrollmentOutcome{kind: enrollmentErrored, reason: "lookup_failed"}
	}
	if !enabled {
		w.markSkipped(ctx, tx, triggerID, "offer_journey_disabled_at_processing")
		return enrollmentOutcome{kind: enrollmentSkipped, reason: "disabled"}
	}
	// Re-check the lane governor's routing_state exactly like 'enabled': the
	// governor may have paused or redirected this lane after the trigger was
	// queued. The click-tracking scanner already re-routes NEW clicks; a
	// pending trigger for the old offer must not enroll into a lane the
	// governor killed.
	switch routingState {
	case "paused_auto":
		w.markSkipped(ctx, tx, triggerID, "lane_paused_auto_at_processing")
		return enrollmentOutcome{kind: enrollmentSkipped, reason: "lane_paused_auto"}
	case "redirect":
		w.markSkipped(ctx, tx, triggerID, "lane_redirected_at_processing")
		return enrollmentOutcome{kind: enrollmentSkipped, reason: "lane_redirected"}
	}
	if journeyID == "" {
		w.markSkipped(ctx, tx, triggerID, "no_click_journey_at_processing")
		return enrollmentOutcome{kind: enrollmentSkipped, reason: "no_journey"}
	}
	if payoutType == "CPC" {
		w.markSkipped(ctx, tx, triggerID, "cpc_offer_at_processing")
		return enrollmentOutcome{kind: enrollmentSkipped, reason: "cpc_offer"}
	}

	// Re-check conversion suppression. Race: postback queued before
	// the subscriber's conversion postback landed.
	var offerUUID uuid.UUID
	tx.QueryRowContext(ctx,
		`SELECT id FROM mailing_offers WHERE everflow_offer_id=$1 LIMIT 1`,
		everflowOfferID).Scan(&offerUUID)
	if offerUUID != uuid.Nil {
		var suppCount int
		_ = tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM mailing_offer_suppressions
			WHERE offer_id=$1 AND subscriber_id=$2 AND reason='converted'
		`, offerUUID, subscriberID).Scan(&suppCount)
		if suppCount > 0 {
			w.markSkipped(ctx, tx, triggerID, "already_converted_at_processing")
			return enrollmentOutcome{kind: enrollmentSkipped, reason: "already_converted"}
		}
	}

	// Resolve current journey graph for first non-trigger node.
	var nodesJSON sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT nodes::text FROM mailing_journeys WHERE id=$1 AND status='active'
	`, journeyID).Scan(&nodesJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.markSkipped(ctx, tx, triggerID, "journey_not_found_or_inactive")
			return enrollmentOutcome{kind: enrollmentSkipped, reason: "journey_inactive"}
		}
		log.Printf("JourneyEventEnroller[%s]: load journey: %v", triggerID, err)
		return enrollmentOutcome{kind: enrollmentErrored, reason: "load_journey_failed"}
	}
	if !nodesJSON.Valid || nodesJSON.String == "" {
		w.markSkipped(ctx, tx, triggerID, "journey_has_no_nodes")
		return enrollmentOutcome{kind: enrollmentSkipped, reason: "empty_graph"}
	}
	firstNodeID, ok := firstExecutableNodeID(nodesJSON.String)
	if !ok {
		w.markSkipped(ctx, tx, triggerID, "no_executable_node")
		return enrollmentOutcome{kind: enrollmentSkipped, reason: "no_executable_node"}
	}

	// Hydrate subscriber email if the postback didn't carry one.
	if subscriberEmail == "" {
		_ = tx.QueryRowContext(ctx,
			`SELECT email FROM mailing_subscribers WHERE id=$1`,
			subscriberID).Scan(&subscriberEmail)
	}
	if subscriberEmail == "" {
		w.markSkipped(ctx, tx, triggerID, "subscriber_email_unresolvable")
		return enrollmentOutcome{kind: enrollmentSkipped, reason: "no_email"}
	}

	// Capture the original creative + sender identity from the campaign
	// the subscriber clicked. Reminders reuse this body so the drip feels
	// like a continuation rather than a brand-new pitch (operator policy
	// 2026-06-01). Subject comes from mailing_offer_reminder_subjects;
	// only body + identity are reused.
	//
	// We ALSO read the original campaign's sending_profile_id here because
	// it is the single most accurate brand-continuity signal: the reminder
	// must go out on the EXACT profile (IP pool, DKIM domain, api_endpoint)
	// the click originated from. The handler's sub2-derived guess is only a
	// fallback — sub2 carries the brand ROOT (e.g. "quizfiesta.com") which
	// does not exact-match the sending_domain ("em.quizfiesta.com").
	var originalBodyHTML, originalFromName, originalFromEmail string
	var originalProfileID sql.NullString
	if camID, err := uuid.Parse(sub3CampaignID); err == nil && camID != uuid.Nil {
		_ = tx.QueryRowContext(ctx, `
			SELECT
				COALESCE(html_content, ''),
				COALESCE(from_name, ''),
				COALESCE(from_email, ''),
				sending_profile_id
			FROM mailing_campaigns WHERE id=$1
		`, camID).Scan(&originalBodyHTML, &originalFromName, &originalFromEmail, &originalProfileID)
	}

	// Resolve the effective sending profile with a clear precedence:
	//   1. original campaign's sending_profile_id (sub3) — exact original profile
	//   2. handler-supplied sending_profile_id (sub2 brand-root match) — fallback
	//   3. brand-root match against active PMTA profiles — last-resort fallback
	effectiveProfileID := sendingProfileID
	if originalProfileID.Valid && originalProfileID.String != "" {
		effectiveProfileID = originalProfileID
	}
	if !effectiveProfileID.Valid || effectiveProfileID.String == "" {
		if resolved := resolveProfileByBrandRoot(ctx, tx, sub2Brand, sendingDomain); resolved != "" {
			effectiveProfileID = sql.NullString{String: resolved, Valid: true}
		}
	}

	// Build metadata JSON with everything the email-node executor (Phase 3)
	// needs to send reminders on the original sending profile and pull the
	// per-step reminder subject from mailing_offer_reminder_subjects.
	metadata := map[string]interface{}{
		"source":              "click_postback",
		"enrolled_via":        "click_postback",
		"trigger_id":          triggerID,
		"everflow_offer_id":   everflowOfferID,
		"sub2_brand":          sub2Brand,
		"sub3_campaign_id":    sub3CampaignID,
		"click_id":            clickID,
		"sending_domain":      sendingDomain,
		"click_url":           clickURL,
		"original_subscriber": subscriberID.String(),
	}
	if effectiveProfileID.Valid && effectiveProfileID.String != "" {
		metadata["sending_profile_id"] = effectiveProfileID.String
	}
	if originalBodyHTML != "" {
		metadata["body_html"] = originalBodyHTML
	}
	if originalFromName != "" {
		metadata["original_from_name"] = originalFromName
	}
	if originalFromEmail != "" {
		metadata["original_from_email"] = originalFromEmail
	}
	// Re-enrollment (re-arm) flow (Unit A, 2026-07-10). Instead of a blind
	// INSERT..ON CONFLICT DO NOTHING (which silently no-oped every repeat
	// clicker forever — one funnel pass per lifetime), lock the prior pass
	// row (if any) and decide:
	//   - no prior row      → fresh INSERT (ON CONFLICT kept as a race guard)
	//   - status='active'   → skip: one active funnel pass per person (hard
	//                         safety, no env override)
	//   - exited/completed  → re-arm for a fresh pass when past the cooldown
	//                         (CLICKDRIP_REENROLL_COOLDOWN_DAYS, default 30)
	//                         unless CLICKDRIP_REENROLL_DISABLED restores the
	//                         legacy one-pass behavior.
	// Conversion safety is enforced BELOW on the prior row itself
	// (exit_reason='converted' / converted_at) — the offer-scoped suppression
	// check above no-ops for offers without a mailing_offers row, which is
	// most click-drip offers. FOR UPDATE rides the existing per-tick tx,
	// serializing against concurrent enrollers and the executor's
	// exit/complete writes.
	var (
		priorID, priorStatus, priorOffer, priorExitReason string
		priorEnrolledAt                                   time.Time
		priorExitedAt, priorCompletedAt, priorConvertedAt sql.NullTime
		priorMetaRaw                                      sql.NullString
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, status, enrolled_at, exited_at, completed_at, converted_at,
		       COALESCE(enrollment_offer_id,''), COALESCE(exit_reason,''), metadata
		FROM mailing_journey_enrollments
		WHERE journey_id=$1 AND subscriber_email=$2
		FOR UPDATE
	`, journeyID, subscriberEmail).Scan(
		&priorID, &priorStatus, &priorEnrolledAt, &priorExitedAt, &priorCompletedAt, &priorConvertedAt,
		&priorOffer, &priorExitReason, &priorMetaRaw,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("JourneyEventEnroller[%s]: prior enrollment lookup: %v", triggerID, err)
		return enrollmentOutcome{kind: enrollmentErrored, reason: "prior_lookup_failed"}
	}

	if errors.Is(err, sql.ErrNoRows) {
		// No prior pass — fresh enrollment, exactly the legacy INSERT.
		metaBytes, _ := json.Marshal(metadata)
		enrollmentID := fmt.Sprintf("enroll-clk-%s", uuid.NewString()[:8])
		_, err = tx.ExecContext(ctx, `
			INSERT INTO mailing_journey_enrollments (
				id, journey_id, subscriber_email, current_node_id,
				status, enrolled_at, next_execute_at, metadata,
				enrolled_via, enrollment_offer_id
			) VALUES (
				$1, $2, $3, $4,
				'active', NOW(), NOW(), $5::jsonb,
				'click_postback', $6
			)
			ON CONFLICT (journey_id, subscriber_email) DO NOTHING
		`, enrollmentID, journeyID, subscriberEmail, firstNodeID, string(metaBytes), everflowOfferID)
		if err != nil {
			log.Printf("JourneyEventEnroller[%s]: insert enrollment: %v", triggerID, err)
			return enrollmentOutcome{kind: enrollmentErrored, reason: "insert_failed"}
		}

		if err := w.markProcessed(ctx, tx, triggerID); err != nil {
			log.Printf("JourneyEventEnroller[%s]: mark processed: %v", triggerID, err)
			return enrollmentOutcome{kind: enrollmentErrored, reason: "mark_processed_failed"}
		}
		return enrollmentOutcome{kind: enrollmentEnrolled}
	}

	// Prior pass exists.
	if priorStatus == "active" {
		w.markSkipped(ctx, tx, triggerID, "active_pass_exists")
		return enrollmentOutcome{kind: enrollmentSkipped, reason: "active_pass_exists"}
	}
	if clickdripReenrollDisabled() {
		w.markSkipped(ctx, tx, triggerID, "reenroll_disabled_prior_pass")
		return enrollmentOutcome{kind: enrollmentSkipped, reason: "reenroll_disabled"}
	}

	// Conversion guard, independent of the offer-scoped suppression table
	// above (which no-ops for offers without a mailing_offers row — true for
	// most click-drip offers). Conversions exit the row with
	// exit_reason='converted' + converted_at (everflow_postback_handler
	// exitClickDripEnrollmentsOnConversion), NOT status='converted'. A
	// converted customer must never be re-mailed the SAME offer's funnel; a
	// click on a DIFFERENT offer may re-arm (cross-sell), with the conversion
	// counting as recent activity for the cooldown and archived+cleared below
	// so the governor never attributes it to the new offer.
	priorWasConverted := priorConvertedAt.Valid || priorExitReason == "converted"
	if priorWasConverted && priorOffer == everflowOfferID {
		w.markSkipped(ctx, tx, triggerID, "converted_prior_pass_same_offer")
		return enrollmentOutcome{kind: enrollmentSkipped, reason: "converted_same_offer"}
	}

	// Cooldown: eligible when GREATEST(exited_at, completed_at, converted_at,
	// enrolled_at) is older than the cooldown window.
	lastActivity := priorEnrolledAt
	if priorExitedAt.Valid && priorExitedAt.Time.After(lastActivity) {
		lastActivity = priorExitedAt.Time
	}
	if priorCompletedAt.Valid && priorCompletedAt.Time.After(lastActivity) {
		lastActivity = priorCompletedAt.Time
	}
	if priorConvertedAt.Valid && priorConvertedAt.Time.After(lastActivity) {
		lastActivity = priorConvertedAt.Time
	}
	cooldown := time.Duration(clickdripReenrollCooldownDays()) * 24 * time.Hour
	if time.Since(lastActivity) < cooldown {
		w.markSkipped(ctx, tx, triggerID, "reenroll_cooldown")
		return enrollmentOutcome{kind: enrollmentSkipped, reason: "reenroll_cooldown"}
	}

	// RE-ARM: reset the existing row (the UNIQUE(journey_id, subscriber_email)
	// constraint is load-bearing for 4 other enrollment paths — never insert a
	// second row). converted_at is deliberately left as-is (historical fact).
	// metadata is the freshly built map plus a prior_passes audit trail; any
	// prior_passes already accumulated on the old metadata carry forward
	// (malformed/empty old metadata tolerated — we just start a new array).
	metadata["enrolled_via"] = "click_postback_rearm"
	var priorPasses []interface{}
	if priorMetaRaw.Valid && priorMetaRaw.String != "" {
		var oldMeta map[string]interface{}
		if json.Unmarshal([]byte(priorMetaRaw.String), &oldMeta) == nil {
			if pp, ok := oldMeta["prior_passes"].([]interface{}); ok {
				priorPasses = pp
			}
		}
	}
	priorSummary := map[string]interface{}{
		"offer":       priorOffer,
		"enrolled_at": priorEnrolledAt.UTC().Format(time.RFC3339),
	}
	if priorExitedAt.Valid {
		priorSummary["exited_at"] = priorExitedAt.Time.UTC().Format(time.RFC3339)
	}
	if priorExitReason != "" {
		priorSummary["exit_reason"] = priorExitReason
	}
	if priorConvertedAt.Valid {
		priorSummary["converted_at"] = priorConvertedAt.Time.UTC().Format(time.RFC3339)
	}
	metadata["prior_passes"] = append(priorPasses, priorSummary)
	metaBytes, _ := json.Marshal(metadata)

	// converted_at is CLEARED (after archiving above): it belongs to the
	// prior pass's offer, and leaving it would make the lane governor count a
	// foreign-offer conversion as the re-armed offer's conversion (suppressing
	// dead-lane detection). The durable conversion record lives in the
	// postback/suppression layer, not this row.
	_, err = tx.ExecContext(ctx, `
		UPDATE mailing_journey_enrollments
		SET status='active', enrolled_at=NOW(), next_execute_at=NOW(),
		    current_node_id=$2, enrollment_offer_id=$3,
		    enrolled_via='click_postback_rearm',
		    exited_at=NULL, exit_reason=NULL, completed_at=NULL,
		    converted_at=NULL,
		    metadata=$4::jsonb
		WHERE id=$1
	`, priorID, firstNodeID, everflowOfferID, string(metaBytes))
	if err != nil {
		log.Printf("JourneyEventEnroller[%s]: re-arm enrollment %s: %v", triggerID, priorID, err)
		return enrollmentOutcome{kind: enrollmentErrored, reason: "rearm_failed"}
	}

	if err := w.markProcessed(ctx, tx, triggerID); err != nil {
		log.Printf("JourneyEventEnroller[%s]: mark processed: %v", triggerID, err)
		return enrollmentOutcome{kind: enrollmentErrored, reason: "mark_processed_failed"}
	}
	return enrollmentOutcome{kind: enrollmentRearmed}
}

// clickdripReenrollDisabled is the re-arm kill switch. Truthy
// CLICKDRIP_REENROLL_DISABLED restores the legacy one-pass-per-lifetime
// behavior (prior exited/completed passes block re-enrollment forever).
func clickdripReenrollDisabled() bool {
	return envTruthy(os.Getenv("CLICKDRIP_REENROLL_DISABLED"))
}

// clickdripReenrollCooldownDays is the minimum quiet period after a prior
// pass (exit/complete/enroll, whichever is latest) before a new click may
// re-arm the funnel. Unset, empty, non-numeric, or negative → default 30.
func clickdripReenrollCooldownDays() int {
	v := strings.TrimSpace(os.Getenv("CLICKDRIP_REENROLL_COOLDOWN_DAYS"))
	if v == "" {
		return 30
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 30
	}
	return n
}

// resolveProfileByBrandRoot finds an active PMTA sending profile whose
// sending_domain belongs to the same brand root as the click's sub2 value.
//
// This is the last-resort fallback when neither the original campaign
// (sub3) nor the handler supplied a profile id. sub2 carries the brand
// ROOT (e.g. "quizfiesta.com") because creatives use
// sub2={{ brand.domain }}, while sending_domain is "em.quizfiesta.com".
// We resolve the root on BOTH sides (brand.Root) and prefer the "em.<root>"
// primary sending domain over secondary domains (e.g. "m.<root>").
//
// Returns "" when nothing matches; the caller leaves sending_profile_id
// unset and the executor falls back to the platform default (logged).
func resolveProfileByBrandRoot(ctx context.Context, tx *sql.Tx, sub2Brand, sendingDomain string) string {
	candidates := []string{}
	if r := brand.Root(sub2Brand); r != "" {
		candidates = append(candidates, r)
	}
	if r := brand.Root(sendingDomain); r != "" && (len(candidates) == 0 || r != candidates[0]) {
		candidates = append(candidates, r)
	}
	if len(candidates) == 0 {
		return ""
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT id, COALESCE(sending_domain,'')
		FROM mailing_sending_profiles
		WHERE status='active' AND vendor_type='pmta'
	`)
	if err != nil {
		log.Printf("resolveProfileByBrandRoot: query profiles: %v", err)
		return ""
	}
	defer rows.Close()

	var primaryID, secondaryID string
	for rows.Next() {
		var id, dom string
		if err := rows.Scan(&id, &dom); err != nil {
			continue
		}
		domRoot := brand.Root(dom)
		matched := false
		for _, c := range candidates {
			if domRoot == c {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		// Prefer the canonical "em.<root>" sending domain.
		if strings.HasPrefix(strings.ToLower(dom), "em.") {
			primaryID = id
		} else if secondaryID == "" {
			secondaryID = id
		}
	}
	if primaryID != "" {
		return primaryID
	}
	return secondaryID
}

func (w *JourneyEventEnroller) markProcessed(ctx context.Context, tx *sql.Tx, triggerID string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE mailing_journey_event_triggers
		SET status='processed', processed_at=NOW()
		WHERE id=$1 AND status='pending'
	`, triggerID)
	return err
}

func (w *JourneyEventEnroller) markSkipped(ctx context.Context, tx *sql.Tx, triggerID, reason string) {
	_, err := tx.ExecContext(ctx, `
		UPDATE mailing_journey_event_triggers
		SET status='skipped', processed_at=NOW(), skip_reason=$2
		WHERE id=$1 AND status='pending'
	`, triggerID, reason)
	if err != nil {
		log.Printf("JourneyEventEnroller[%s]: mark skipped: %v", triggerID, err)
	}
}

// firstExecutableNodeID parses a journey nodes JSON blob and returns
// the id of the first non-'trigger' node. Pure function so it can be
// unit tested without a DB. Mirrors the simpler shape of
// extractSegmentTrigger in journey_segment_enroller.go but doesn't
// require a segment trigger — any trigger type is fine.
func firstExecutableNodeID(nodesJSON string) (string, bool) {
	type node struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	var nodes []node
	if err := json.Unmarshal([]byte(nodesJSON), &nodes); err != nil {
		return "", false
	}
	// Match the segment-enroller convention: first node whose type
	// isn't "trigger" is the entry point. The journey UI guarantees
	// trigger comes first, but we don't depend on order.
	for _, n := range nodes {
		if n.Type != "trigger" {
			return n.ID, true
		}
	}
	return "", false
}
