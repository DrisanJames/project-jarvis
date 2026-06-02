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

	totalProcessed int64
	totalEnrolled  int64
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

// Stats returns lifetime counters for observability.
func (w *JourneyEventEnroller) Stats() (processed, enrolled, skipped, errors int64) {
	return atomic.LoadInt64(&w.totalProcessed),
		atomic.LoadInt64(&w.totalEnrolled),
		atomic.LoadInt64(&w.totalSkipped),
		atomic.LoadInt64(&w.totalErrors)
}

// tick drains one batch of pending triggers.
func (w *JourneyEventEnroller) tick(ctx context.Context) {
	tickCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

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
	var journeyID, payoutType string
	var enabled bool
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(click_journey_id,''), COALESCE(payout_type,'UNKNOWN'), COALESCE(enabled,false)
		FROM mailing_offer_journey_map WHERE everflow_offer_id=$1
	`, everflowOfferID).Scan(&journeyID, &payoutType, &enabled)
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
