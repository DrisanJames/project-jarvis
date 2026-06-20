package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/notify"
)

type EverflowPostbackHandler struct {
	db       *sql.DB
	notifier notify.Notifier
}

func NewEverflowPostbackHandler(db *sql.DB) *EverflowPostbackHandler {
	return &EverflowPostbackHandler{db: db, notifier: notify.NoopNotifier{}}
}

// WithConversionNotifier attaches the Slack notifier used to announce conversions
// to the operator's #conversions channel. A nil notifier is ignored, leaving the
// no-op default in place. Returns the handler for chaining at registration.
func (h *EverflowPostbackHandler) WithConversionNotifier(n notify.Notifier) *EverflowPostbackHandler {
	if n != nil {
		h.notifier = n
	}
	return h
}

// HandlePostback receives Everflow conversion postbacks.
// Postback URL: /api/mailing/everflow/postback?offer_id=123&sub1=SUBSCRIBER_UUID&sub2=CREATIVE_UUID&sub3=CAMPAIGN_UUID&payout=2.50&transaction_id=EF12345
// Always returns HTTP 200 to Everflow to prevent retries on non-retryable issues.
func (h *EverflowPostbackHandler) HandlePostback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	sub1 := r.URL.Query().Get("sub1")
	sub2 := r.URL.Query().Get("sub2")
	sub3 := r.URL.Query().Get("sub3")
	efOfferID := r.URL.Query().Get("offer_id")
	payoutStr := r.URL.Query().Get("payout")
	txnID := r.URL.Query().Get("transaction_id")

	if sub1 == "" {
		var body struct {
			Sub1          string  `json:"sub1"`
			Sub2          string  `json:"sub2"`
			Sub3          string  `json:"sub3"`
			OfferID       string  `json:"offer_id"`
			Payout        float64 `json:"payout"`
			TransactionID string  `json:"transaction_id"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		sub1 = body.Sub1
		sub2 = body.Sub2
		sub3 = body.Sub3
		efOfferID = body.OfferID
		payoutStr = fmt.Sprintf("%.2f", body.Payout)
		txnID = body.TransactionID
	}

	subscriberID, _ := uuid.Parse(sub1)
	creativeID, _ := uuid.Parse(sub2)
	campaignID, _ := uuid.Parse(sub3)
	payout, _ := strconv.ParseFloat(payoutStr, 64)

	log.Printf("[EverflowPostback] sub1=%s sub2=%s sub3=%s offer_id=%s payout=%.2f txn=%s",
		sub1, sub2, sub3, efOfferID, payout, txnID)

	// Associate the converter by their subscriber UUID (sub1). Without it we
	// cannot match a converter to a suppression row or to an active click-drip
	// enrollment, so there is nothing actionable — skip.
	if subscriberID == uuid.Nil {
		log.Printf("[EverflowPostback] WARN: no subscriber_id in sub1, skipping")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "skipped", "reason": "no_subscriber_id"})
		return
	}

	// Every call to this endpoint is a real conversion (revenue event). Surface
	// it to the operator's #conversions Slack channel — email, offer, payout,
	// date — independent of whether the offer resolves internally or is in the
	// click-drip dictionary. Async + best-effort so the 200 to Everflow is never
	// delayed or blocked by Slack.
	notifyConversionAsync(h.notifier, h.db, subscriberID, efOfferID, payout, txnID)

	ctx := r.Context()
	orgID := "00000000-0000-0000-0000-000000000001"

	// Resolve the internal offer UUID (campaign → mailing_offers). This stays
	// NULL for most click-drip offers, which have no mailing_offers row — that
	// is expected and MUST NOT short-circuit the click-drip exit below.
	var offerID uuid.UUID
	if campaignID != uuid.Nil {
		h.db.QueryRowContext(ctx,
			`SELECT offer_id FROM mailing_campaigns WHERE id=$1 AND offer_id IS NOT NULL`,
			campaignID).Scan(&offerID)
	}
	if offerID == uuid.Nil && efOfferID != "" {
		h.db.QueryRowContext(ctx,
			`SELECT id FROM mailing_offers WHERE everflow_offer_id=$1 LIMIT 1`,
			efOfferID).Scan(&offerID)
	}

	// (1) Converted-suppression. Preserve the existing behaviour for ANY offer
	//     with a resolvable internal UUID (dictionary or not) — the suppression
	//     table's offer_id is NOT NULL, so this only runs when we have one.
	if offerID != uuid.Nil {
		if err := writeConvertedSuppression(ctx, h.db, orgID, offerID, subscriberID, txnID); err != nil {
			log.Printf("[EverflowPostback] ERROR inserting suppression: %v", err)
		}
	}

	// (2) Click-drip exit-on-convert, gated on the journey DICTIONARY
	//     (mailing_offer_journey_map) and matched by subscriber UUID. This runs
	//     independent of whether offerID resolved, so the click-drip offers that
	//     lack a mailing_offers row still stop their drip. Converters for offers
	//     NOT in the dictionary make no changes here (no drip to stop) — exactly
	//     the "skip offers not in the dictionary" rule (2026-06-02).
	inDict, exited, exErr := exitClickDripEnrollmentsOnConversion(ctx, h.db, subscriberID, efOfferID)
	if exErr != nil {
		log.Printf("[EverflowPostback] ERROR exiting click-drip enrollments: %v", exErr)
	}
	switch {
	case !inDict:
		log.Printf("[EverflowPostback] conversion offer=%s not in click-drip dictionary; no drip to stop (subscriber=%s)",
			efOfferID, subscriberID)
	case exited > 0:
		log.Printf("[EverflowPostback] click-drip exit-on-convert: exited %d enrollment(s) offer=%s subscriber=%s",
			exited, efOfferID, subscriberID)
	}

	// If we could neither suppress (no resolvable offer UUID) nor stop a drip
	// (offer not in the dictionary), there is nothing to do — report a skip so
	// operators can see why. Stats updates below all require these ids anyway.
	if offerID == uuid.Nil && !inDict {
		log.Printf("[EverflowPostback] skip: offer=%s not resolvable and not in click-drip dictionary (subscriber=%s)",
			efOfferID, subscriberID)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "skipped", "reason": "offer_not_in_dictionary"})
		return
	}

	if creativeID != uuid.Nil {
		_, err := h.db.ExecContext(ctx, `
			UPDATE mailing_offer_creatives
			SET total_conversions = COALESCE(total_conversions, 0) + 1, updated_at = NOW()
			WHERE id = $1
		`, creativeID)
		if err != nil {
			log.Printf("[EverflowPostback] ERROR updating creative conversions: %v", err)
		}
	}

	if campaignID != uuid.Nil && offerID != uuid.Nil {
		_, err := h.db.ExecContext(ctx, `
			UPDATE mailing_offer_deployments
			SET total_conversions = COALESCE(total_conversions, 0) + 1,
				revenue = COALESCE(revenue, 0) + $1
			WHERE offer_id = $2 AND campaign_id = $3
		`, payout, offerID, campaignID)
		if err != nil {
			log.Printf("[EverflowPostback] ERROR updating deployment stats: %v", err)
		}
	}

	log.Printf("[EverflowPostback] OK: offer=%s subscriber=%s creative=%s campaign=%s payout=%.2f click_drip_exited=%d",
		offerID, subscriberID, creativeID, campaignID, payout, exited)

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status":            "ok",
		"offer_id":          offerID.String(),
		"subscriber_id":     subscriberID.String(),
		"click_drip_exited": strconv.Itoa(int(exited)),
	})
}

// notifyConversionAsync posts a single conversion alert to the #conversions
// Slack channel with the subscriber email, a human-readable offer label, the
// payout amount, and the date. It is shared by the conversion postback handler
// and the click endpoint's conversion branch.
//
// It runs in its own goroutine with a fresh context so it never delays the HTTP
// response to Everflow and is not cancelled when the request returns. A nil or
// no-op notifier short-circuits to nothing.
func notifyConversionAsync(notifier notify.Notifier, db *sql.DB, subscriberID uuid.UUID, efOfferID string, payout float64, txnID string) {
	if notifier == nil {
		return
	}
	if _, isNoop := notifier.(notify.NoopNotifier); isNoop {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		email := "(unknown subscriber)"
		if subscriberID != uuid.Nil && db != nil {
			var e sql.NullString
			_ = db.QueryRowContext(ctx,
				`SELECT email FROM mailing_subscribers WHERE id=$1`, subscriberID).Scan(&e)
			if e.Valid && strings.TrimSpace(e.String) != "" {
				email = e.String
			}
		}

		offer := conversionOfferLabel(ctx, db, efOfferID)

		when := time.Now()
		if loc, err := time.LoadLocation("America/Denver"); err == nil {
			when = when.In(loc)
		}

		body := fmt.Sprintf("Email: %s\nOffer: %s", email, offer)
		if strings.TrimSpace(txnID) != "" {
			body += "\nTransaction: " + txnID
		}

		msg := notify.Message{
			Tier:     notify.TierEvent,
			Scope:    "Conversion",
			Headline: fmt.Sprintf("%s · payout *$%.2f*", offer, payout),
			Context:  fmt.Sprintf("as of %s · Everflow", when.Format("Jan 2, 2006 3:04 PM MST")),
			Body:     body,
		}
		if err := notify.Deliver(notifier, msg); err != nil {
			log.Printf("[conversion-notify] failed to post to #conversions: %v", err)
		}
	}()
}

// conversionOfferLabel resolves a human-readable offer name for an Everflow
// numeric offer id, falling back to the click-drip journey-map note and finally
// the raw id. Best-effort: any lookup error yields the raw-id form.
func conversionOfferLabel(ctx context.Context, db *sql.DB, efOfferID string) string {
	efOfferID = strings.TrimSpace(efOfferID)
	if efOfferID == "" {
		return "(unknown offer)"
	}
	if db != nil {
		var name sql.NullString
		_ = db.QueryRowContext(ctx,
			`SELECT name FROM mailing_offers WHERE everflow_offer_id=$1 AND name<>'' LIMIT 1`,
			efOfferID).Scan(&name)
		if name.Valid && strings.TrimSpace(name.String) != "" {
			return fmt.Sprintf("%s (EF %s)", name.String, efOfferID)
		}

		var notes sql.NullString
		_ = db.QueryRowContext(ctx,
			`SELECT notes FROM mailing_offer_journey_map WHERE everflow_offer_id=$1`,
			efOfferID).Scan(&notes)
		if notes.Valid && strings.TrimSpace(notes.String) != "" {
			return fmt.Sprintf("%s (EF %s)", notes.String, efOfferID)
		}
	}
	return "EF offer " + efOfferID
}

// parsePostbackPayout extracts the conversion payout amount from a postback's
// query params, accepting either "payout" or "amount". Returns 0 when absent or
// unparseable.
func parsePostbackPayout(r *http.Request) float64 {
	q := r.URL.Query()
	for _, k := range []string{"payout", "amount"} {
		if v := strings.TrimSpace(q.Get(k)); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		}
	}
	return 0
}

// writeConvertedSuppression inserts a 'converted' suppression row keyed by
// (offer_id, subscriber_id). Idempotent via the idx_offer_suppressions_offer_sub
// unique index. The suppression table's offer_id is NOT NULL, so callers must
// pass a non-nil offerUUID.
func writeConvertedSuppression(ctx context.Context, db *sql.DB, orgID string, offerUUID, subscriberID uuid.UUID, txnID string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO mailing_offer_suppressions (id, organization_id, offer_id, subscriber_id, reason, source, everflow_conversion_id, suppressed_at)
		VALUES ($1, $2, $3, $4, 'converted', 'everflow_postback', $5, NOW())
		ON CONFLICT (offer_id, subscriber_id) DO NOTHING
	`, uuid.New(), orgID, offerUUID, subscriberID, txnID)
	return err
}

// exitClickDripEnrollmentsOnConversion stops a subscriber's active click-drip
// for a converted offer. It is the conversion-STOP half of the click-drip
// feature, shared by the conversion postback handler and the click postback
// handler's conversion branch.
//
// It is gated on the journey DICTIONARY (mailing_offer_journey_map keyed by the
// everflow numeric offer id): conversions for offers that are NOT mapped return
// inDictionary=false and make zero changes, so the caller treats them as a skip.
// This is the "there will be converters for offers not in your dictionary —
// skip those" rule (operator, 2026-06-02).
//
// Association is by subscriber UUID first — JourneyEventEnroller stamps
// metadata.original_subscriber with the subscriber UUID — and by resolved email
// as a fallback. Matching by UUID is what makes a converter reliably linkable
// even when the campaign's offer_id is NULL and the offer has no mailing_offers
// row (true for most click-drip offers).
func exitClickDripEnrollmentsOnConversion(ctx context.Context, db *sql.DB, subscriberID uuid.UUID, efOfferID string) (inDictionary bool, exited int64, err error) {
	efOfferID = strings.TrimSpace(efOfferID)
	if efOfferID == "" {
		return false, 0, nil
	}

	// Dictionary gate: is this offer one of ours with a configured journey?
	var one int
	dErr := db.QueryRowContext(ctx,
		`SELECT 1 FROM mailing_offer_journey_map WHERE everflow_offer_id=$1`, efOfferID).Scan(&one)
	if dErr == sql.ErrNoRows {
		return false, 0, nil
	}
	if dErr != nil {
		return false, 0, dErr
	}

	// Resolve email (best effort) for the fallback match; UUID match works even
	// if this is empty.
	var subEmail string
	_ = db.QueryRowContext(ctx,
		`SELECT email FROM mailing_subscribers WHERE id=$1`, subscriberID).Scan(&subEmail)

	res, uErr := db.ExecContext(ctx, `
		UPDATE mailing_journey_enrollments
		SET status='exited',
		    exited_at=NOW(),
		    exit_reason='converted',
		    converted_at=NOW(),
		    next_execute_at=NULL,
		    updated_at=NOW()
		WHERE status='active'
		  AND enrollment_offer_id=$1
		  AND (
		        metadata->>'original_subscriber' = $2
		     OR ($3 <> '' AND LOWER(subscriber_email)=LOWER($3))
		      )
	`, efOfferID, subscriberID.String(), subEmail)
	if uErr != nil {
		return true, 0, uErr
	}
	if res != nil {
		exited, _ = res.RowsAffected()
	}
	return true, exited, nil
}
