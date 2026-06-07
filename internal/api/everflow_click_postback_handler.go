package api

// EverflowClickPostbackHandler receives Everflow click postbacks and queues
// them for the JourneyEventEnroller worker (cmd/worker/journey_event_enroller.go).
// It is the entry point for the Click-Drip Journey feature designed
// 2026-06-01:
//
//   subscriber clicks an offer link → Everflow fires this postback →
//   handler validates + queues a row in mailing_journey_event_triggers →
//   JourneyEventEnroller picks up pending rows → creates a row in
//   mailing_journey_enrollments → JourneyExecutor runs the configured
//   click drip (e.g. +1h, +6h, +24h, +72h reminder emails on the same
//   sending profile the subscriber originally clicked from).
//
// This handler is INERT in Phase 1: it records the trigger and returns
// 200, but no enrollment worker exists yet. Phase 2 wires the enroller.
//
// Postback URL (configured per-offer in Everflow):
//   /api/mailing/everflow/click-postback
//     ?offer_id=9539           (Everflow numeric offer id)
//     &sub1=<subscriber_uuid>  (our internal subscriber id)
//     &sub2=<brand_domain>     (brand ROOT, e.g. discountblog.com — creatives
//                               send sub2={{ brand.domain }} which is the root,
//                               NOT the sending domain em.discountblog.com)
//     &sub3=<campaign_uuid>    (the mailing_campaigns.id that drove the click)
//     &transaction_id=<click_id>
//     &click_url=<url>         (optional, original target URL)
//
// Behaviour rules (locked in 2026-06-01 with operator):
//   1. CPC offers are skipped — revenue is already captured on click.
//      We compare mailing_offer_journey_map.payout_type to 'CPC'.
//   2. Offers without a journey map row are skipped (no journey configured).
//   3. Offers with payout_type set but enabled=FALSE are skipped (operator pause).
//   4. Subscribers who already have a 'converted' suppression for this offer
//      are skipped — they bought it; do not re-mail.
//   5. Idempotency: a duplicate click for the same (subscriber_id, offer_id)
//      within 24 hours is collapsed (the journey is already enrolled).
//   6. Always returns HTTP 200 to Everflow regardless of skip reason —
//      Everflow retries on non-200 and we have no recovery state for it.
//
// Mirrors the public-route pattern of EverflowPostbackHandler (sibling
// file). No auth, registered on s.router not s.apiRouter.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/notify"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
)

// EverflowClickPostbackHandler is the HTTP entry for /api/mailing/everflow/click-postback.
type EverflowClickPostbackHandler struct {
	db       *sql.DB
	notifier notify.Notifier
}

// NewEverflowClickPostbackHandler wires the handler with a *sql.DB.
func NewEverflowClickPostbackHandler(db *sql.DB) *EverflowClickPostbackHandler {
	return &EverflowClickPostbackHandler{db: db, notifier: notify.NoopNotifier{}}
}

// WithConversionNotifier attaches the Slack notifier used to announce conversions
// (the conversion branch of this endpoint) to the operator's #conversions
// channel. A nil notifier is ignored. Returns the handler for chaining.
func (h *EverflowClickPostbackHandler) WithConversionNotifier(n notify.Notifier) *EverflowClickPostbackHandler {
	if n != nil {
		h.notifier = n
	}
	return h
}

// clickPostbackInput is the parsed request, normalized.
type clickPostbackInput struct {
	SubscriberIDStr string
	Sub2Brand       string
	CampaignIDStr   string
	EverflowOfferID string
	TransactionID   string
	ClickURL        string

	subscriberID uuid.UUID
	campaignID   uuid.UUID
}

// IdempotencyWindowHours: a duplicate click for the same (subscriber, offer)
// within this many hours is treated as a no-op. Conservative — most legit
// re-clicks (forwarded email, second device, etc.) happen within minutes.
const ClickPostbackIdempotencyHours = 24

// HandleClickPostback is the HTTP handler. Always returns 200.
func (h *EverflowClickPostbackHandler) HandleClickPostback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	in := parseClickPostback(r)

	log.Printf("[EverflowClickPostback] sub1=%s sub2=%s sub3=%s offer_id=%s txn=%s",
		in.SubscriberIDStr, in.Sub2Brand, in.CampaignIDStr, in.EverflowOfferID, in.TransactionID)

	// Validate subscriber id — required for everything downstream.
	if in.subscriberID == uuid.Nil {
		respondClick(w, "skipped", "no_subscriber_id", "")
		return
	}

	// Require an Everflow offer id to look up the journey map.
	if in.EverflowOfferID == "" {
		respondClick(w, "skipped", "no_offer_id", "")
		return
	}

	ctx := r.Context()

	// Conversion event on the click endpoint.
	//
	// Operators may point an Everflow CONVERSION postback at this same URL and
	// "interchange the event" (add &event=conversion). Without this branch a
	// conversion would ENROLL the converter into the drip — the exact opposite
	// of what we want. When the postback signals a conversion we route to the
	// shared conversion-STOP path (dictionary-gated exit + converted suppression,
	// associated by subscriber UUID) and never enroll.
	if isConversionEvent(r) {
		// Surface the conversion to the operator's #conversions Slack channel —
		// email, offer, payout, date — for every conversion received on this
		// endpoint, independent of the click-drip dictionary outcome below.
		// Async + best-effort so the 200 to Everflow is never delayed.
		notifyConversionAsync(h.notifier, h.db, in.subscriberID, in.EverflowOfferID, parsePostbackPayout(r), in.TransactionID)

		inDict, exited, exErr := exitClickDripEnrollmentsOnConversion(ctx, h.db, in.subscriberID, in.EverflowOfferID)
		if exErr != nil {
			log.Printf("[EverflowClickPostback] ERROR conversion exit: %v", exErr)
		}
		// Suppress on a resolvable internal offer so future clicks short-circuit.
		var offerUUID uuid.UUID
		h.db.QueryRowContext(ctx,
			`SELECT id FROM mailing_offers WHERE everflow_offer_id=$1 LIMIT 1`,
			in.EverflowOfferID).Scan(&offerUUID)
		if offerUUID != uuid.Nil {
			if err := writeConvertedSuppression(ctx, h.db,
				"00000000-0000-0000-0000-000000000001", offerUUID, in.subscriberID, in.TransactionID); err != nil {
				log.Printf("[EverflowClickPostback] ERROR conversion suppression: %v", err)
			}
		}
		if !inDict {
			log.Printf("[EverflowClickPostback] conversion offer=%s not in dictionary; skipping (subscriber=%s)",
				in.EverflowOfferID, in.subscriberID)
			respondClick(w, "skipped", "offer_not_in_dictionary", "")
			return
		}
		log.Printf("[EverflowClickPostback] conversion-stop: exited %d drip enrollment(s) offer=%s subscriber=%s",
			exited, in.EverflowOfferID, in.subscriberID)
		respondClick(w, "converted", "", "")
		return
	}

	// Look up the offer in the journey map.
	jm, err := h.lookupOfferJourneyMap(ctx, in.EverflowOfferID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			respondClick(w, "skipped", "offer_not_mapped", "")
			return
		}
		log.Printf("[EverflowClickPostback] ERROR looking up offer journey map: %v", err)
		respondClick(w, "skipped", "internal_lookup_error", "")
		return
	}

	if !jm.Enabled {
		respondClick(w, "skipped", "offer_journey_disabled", "")
		return
	}
	if isCPCPayout(jm.PayoutType) {
		respondClick(w, "skipped", "cpc_offer", "")
		return
	}
	if jm.ClickJourneyID == "" {
		respondClick(w, "skipped", "no_click_journey_configured", "")
		return
	}

	// Resolve internal offer UUID (for suppression lookup) — non-fatal if missing.
	var offerUUID uuid.UUID
	h.db.QueryRowContext(ctx,
		`SELECT id FROM mailing_offers WHERE everflow_offer_id=$1 LIMIT 1`,
		in.EverflowOfferID).Scan(&offerUUID)

	// Skip if subscriber has already converted on this offer.
	if offerUUID != uuid.Nil {
		alreadyConverted, err := h.subscriberConverted(ctx, offerUUID, in.subscriberID)
		if err != nil {
			log.Printf("[EverflowClickPostback] ERROR suppression check: %v", err)
		} else if alreadyConverted {
			respondClick(w, "skipped", "already_converted", "")
			return
		}
	}

	// Idempotency window: skip if we already saw this click recently.
	dup, err := h.recentDuplicate(ctx, in.subscriberID, in.EverflowOfferID)
	if err != nil {
		log.Printf("[EverflowClickPostback] ERROR idempotency check: %v", err)
	}
	if dup {
		respondClick(w, "skipped", "duplicate_within_idempotency_window", "")
		return
	}

	// Best-effort: resolve subscriber email + sending profile id from sub2 (brand domain).
	var subscriberEmail string
	h.db.QueryRowContext(ctx,
		`SELECT email FROM mailing_subscribers WHERE id=$1`,
		in.subscriberID).Scan(&subscriberEmail)

	// sub2 carries the brand ROOT (creatives use sub2={{ brand.domain }},
	// which resolves to e.g. "quizfiesta.com"), NOT the sending domain
	// ("em.quizfiesta.com"). Match on the brand root and prefer the
	// canonical "em.<root>" sending domain. This is only a best-effort hint
	// stored on the trigger; the JourneyEventEnroller does the authoritative
	// resolution (preferring the original campaign's sending_profile_id).
	var sendingProfileID uuid.UUID
	var sendingDomain string
	if in.Sub2Brand != "" {
		sendingDomain = strings.ToLower(in.Sub2Brand)
		root := brand.Root(sendingDomain)
		h.db.QueryRowContext(ctx, `
			SELECT id FROM mailing_sending_profiles
			WHERE status='active' AND vendor_type='pmta'
			  AND (
			        LOWER(sending_domain) = $1
			     OR LOWER(sending_domain) = 'em.' || $2
			     OR LOWER(sending_domain) = $2
			     OR LOWER(sending_domain) LIKE '%.' || $2
			      )
			ORDER BY (LOWER(sending_domain) = 'em.' || $2) DESC,
			         (LOWER(sending_domain) = $1) DESC
			LIMIT 1
		`, sendingDomain, root).Scan(&sendingProfileID)
	}

	// Insert the trigger row.
	triggerID, err := h.insertEventTrigger(ctx, in, sendingProfileID, sendingDomain, subscriberEmail)
	if err != nil {
		log.Printf("[EverflowClickPostback] ERROR inserting trigger: %v", err)
		respondClick(w, "error", "insert_failed", "")
		return
	}

	log.Printf("[EverflowClickPostback] OK: trigger=%s offer=%s subscriber=%s journey=%s sending_profile=%s",
		triggerID, in.EverflowOfferID, in.subscriberID, jm.ClickJourneyID, sendingProfileID)

	respondClick(w, "queued", "", triggerID)
}

// parseClickPostback extracts the click postback fields from query params,
// falling back to a JSON body when sub1 is empty (matches the Everflow
// conversion-postback pattern).
func parseClickPostback(r *http.Request) clickPostbackInput {
	q := r.URL.Query()
	in := clickPostbackInput{
		SubscriberIDStr: q.Get("sub1"),
		Sub2Brand:       q.Get("sub2"),
		CampaignIDStr:   q.Get("sub3"),
		EverflowOfferID: q.Get("offer_id"),
		TransactionID:   q.Get("transaction_id"),
		ClickURL:        q.Get("click_url"),
	}

	if in.SubscriberIDStr == "" {
		var body struct {
			Sub1          string `json:"sub1"`
			Sub2          string `json:"sub2"`
			Sub3          string `json:"sub3"`
			OfferID       string `json:"offer_id"`
			TransactionID string `json:"transaction_id"`
			ClickURL      string `json:"click_url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		in.SubscriberIDStr = body.Sub1
		in.Sub2Brand = body.Sub2
		in.CampaignIDStr = body.Sub3
		in.EverflowOfferID = body.OfferID
		in.TransactionID = body.TransactionID
		in.ClickURL = body.ClickURL
	}

	in.subscriberID, _ = uuid.Parse(in.SubscriberIDStr)
	in.campaignID, _ = uuid.Parse(in.CampaignIDStr)
	in.EverflowOfferID = strings.TrimSpace(in.EverflowOfferID)
	return in
}

// isConversionEvent reports whether a postback to the click endpoint should be
// treated as a CONVERSION (stop the drip) rather than a CLICK (start it).
//
// Everflow's per-offer postbacks fire on conversion, so an operator who reuses
// this URL for a conversion postback must tag it explicitly with a discriminator
// — &event=conversion (also accepts type / postback_type / event_type, values
// conversion|cv|sale|conv|conversion_postback). A non-zero payout/amount param,
// which only conversions carry, is treated as a secondary signal. Absent any of
// these, the postback is a click and enrolls as before.
func isConversionEvent(r *http.Request) bool {
	q := r.URL.Query()
	for _, k := range []string{"event", "type", "postback_type", "event_type"} {
		switch strings.ToLower(strings.TrimSpace(q.Get(k))) {
		case "conversion", "cv", "sale", "conv", "conversion_postback":
			return true
		}
	}
	for _, k := range []string{"payout", "amount"} {
		v := strings.TrimSpace(q.Get(k))
		if v != "" && v != "0" && v != "0.0" && v != "0.00" {
			return true
		}
	}
	return false
}

// offerJourneyMap is one row from mailing_offer_journey_map.
type offerJourneyMap struct {
	EverflowOfferID string
	ClickJourneyID  string
	PayoutType      string
	Enabled         bool
}

// lookupOfferJourneyMap returns sql.ErrNoRows if the offer isn't mapped.
func (h *EverflowClickPostbackHandler) lookupOfferJourneyMap(ctx context.Context, everflowOfferID string) (*offerJourneyMap, error) {
	row := h.db.QueryRowContext(ctx, `
		SELECT everflow_offer_id, COALESCE(click_journey_id,''), COALESCE(payout_type,'UNKNOWN'), COALESCE(enabled,false)
		FROM mailing_offer_journey_map WHERE everflow_offer_id=$1
	`, everflowOfferID)
	out := &offerJourneyMap{}
	if err := row.Scan(&out.EverflowOfferID, &out.ClickJourneyID, &out.PayoutType, &out.Enabled); err != nil {
		return nil, err
	}
	return out, nil
}

// isCPCPayout returns true for any payout-type variant that bills on click.
// We treat CPC as the only "skip" type — every other payout type benefits
// from the click-drip follow-up because the conversion (and revenue) lands
// AFTER the click.
func isCPCPayout(payoutType string) bool {
	return strings.EqualFold(strings.TrimSpace(payoutType), "CPC")
}

// subscriberConverted returns true if mailing_offer_suppressions has a
// 'converted' row for (offer, subscriber). These are written by the
// existing /api/mailing/everflow/postback (conversion) handler.
func (h *EverflowClickPostbackHandler) subscriberConverted(ctx context.Context, offerID, subscriberID uuid.UUID) (bool, error) {
	var n int
	err := h.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_offer_suppressions
		WHERE offer_id=$1 AND subscriber_id=$2 AND reason='converted'
	`, offerID, subscriberID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// recentDuplicate is the idempotency check. Looks for a row in
// mailing_journey_event_triggers for this (subscriber, offer) within the
// last ClickPostbackIdempotencyHours.
func (h *EverflowClickPostbackHandler) recentDuplicate(ctx context.Context, subscriberID uuid.UUID, everflowOfferID string) (bool, error) {
	q := fmt.Sprintf(`
		SELECT COUNT(*) FROM mailing_journey_event_triggers
		WHERE subscriber_id=$1 AND everflow_offer_id=$2
		  AND received_at > NOW() - INTERVAL '%d hours'
	`, ClickPostbackIdempotencyHours)
	var n int
	if err := h.db.QueryRowContext(ctx, q, subscriberID, everflowOfferID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// insertEventTrigger writes the queue row. Returns the new trigger UUID.
func (h *EverflowClickPostbackHandler) insertEventTrigger(
	ctx context.Context,
	in clickPostbackInput,
	sendingProfileID uuid.UUID,
	sendingDomain string,
	subscriberEmail string,
) (string, error) {
	id := uuid.New()
	var sendingProfileArg interface{}
	if sendingProfileID != uuid.Nil {
		sendingProfileArg = sendingProfileID
	}
	_, err := h.db.ExecContext(ctx, `
		INSERT INTO mailing_journey_event_triggers (
			id, source, everflow_offer_id, subscriber_id, subscriber_email,
			sub2_brand, sub3_campaign_id, click_id, sending_profile_id,
			sending_domain, click_url, status, received_at
		) VALUES (
			$1, 'everflow_click_postback', $2, $3, NULLIF($4, ''),
			$5, $6, $7, $8,
			$9, $10, 'pending', NOW()
		)
	`,
		id,
		in.EverflowOfferID,
		in.subscriberID,
		subscriberEmail,
		in.Sub2Brand,
		in.CampaignIDStr,
		in.TransactionID,
		sendingProfileArg,
		sendingDomain,
		in.ClickURL,
	)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// respondClick writes a 200 JSON response. status is one of
// "queued" | "skipped" | "error". reason explains skipped/error;
// triggerID is set on "queued" only.
func respondClick(w http.ResponseWriter, status, reason, triggerID string) {
	w.WriteHeader(http.StatusOK)
	out := map[string]string{"status": status}
	if reason != "" {
		out["reason"] = reason
	}
	if triggerID != "" {
		out["trigger_id"] = triggerID
	}
	_ = json.NewEncoder(w).Encode(out)
}
