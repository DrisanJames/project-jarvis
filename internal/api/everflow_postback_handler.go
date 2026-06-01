package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"github.com/google/uuid"
)

type EverflowPostbackHandler struct {
	db *sql.DB
}

func NewEverflowPostbackHandler(db *sql.DB) *EverflowPostbackHandler {
	return &EverflowPostbackHandler{db: db}
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

	if subscriberID == uuid.Nil {
		log.Printf("[EverflowPostback] WARN: no subscriber_id in sub1, skipping")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "skipped", "reason": "no_subscriber_id"})
		return
	}

	ctx := r.Context()
	orgID := "00000000-0000-0000-0000-000000000001"

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

	if offerID == uuid.Nil {
		log.Printf("[EverflowPostback] WARN: could not resolve offer_id (ef_offer_id=%s, campaign=%s), skipping suppression", efOfferID, sub3)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "skipped", "reason": "no_offer_id"})
		return
	}

	_, err := h.db.ExecContext(ctx, `
		INSERT INTO mailing_offer_suppressions (id, organization_id, offer_id, subscriber_id, reason, source, everflow_conversion_id, suppressed_at)
		VALUES ($1, $2, $3, $4, 'converted', 'everflow_postback', $5, NOW())
		ON CONFLICT (offer_id, subscriber_id) DO NOTHING
	`, uuid.New(), orgID, offerID, subscriberID, txnID)
	if err != nil {
		log.Printf("[EverflowPostback] ERROR inserting suppression: %v", err)
	}

	// ────────────────────────────────────────────────────────────────────
	// Click-Drip exit-on-convert (Phase 4, 2026-06-01).
	//
	// If the subscriber is in an active click-drip journey for this same
	// offer, cancel the remaining reminders. They bought it; do not pelt
	// them with reminder emails.
	//
	// We resolve subscriber → email then UPDATE all matching active
	// enrollments. enrollment_offer_id is the everflow numeric id (string),
	// which is what the click-postback enroller stamps.
	// ────────────────────────────────────────────────────────────────────
	if efOfferID != "" {
		var subEmail string
		_ = h.db.QueryRowContext(ctx,
			`SELECT email FROM mailing_subscribers WHERE id=$1`,
			subscriberID).Scan(&subEmail)
		if subEmail != "" {
			res, exErr := h.db.ExecContext(ctx, `
				UPDATE mailing_journey_enrollments
				SET status='exited',
				    exited_at=NOW(),
				    exit_reason='converted',
				    converted_at=NOW()
				WHERE status='active'
				  AND enrollment_offer_id=$1
				  AND LOWER(subscriber_email)=LOWER($2)
			`, efOfferID, subEmail)
			if exErr != nil {
				log.Printf("[EverflowPostback] ERROR exiting click-drip enrollments: %v", exErr)
			} else if res != nil {
				if n, _ := res.RowsAffected(); n > 0 {
					log.Printf("[EverflowPostback] click-drip exit-on-convert: exited %d enrollment(s) for offer=%s subscriber=%s",
						n, efOfferID, subEmail)
				}
			}
		}
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

	if campaignID != uuid.Nil {
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

	log.Printf("[EverflowPostback] OK: offer=%s subscriber=%s creative=%s campaign=%s payout=%.2f",
		offerID, subscriberID, creativeID, campaignID, payout)

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status":        "ok",
		"offer_id":      offerID.String(),
		"subscriber_id": subscriberID.String(),
	})
}
