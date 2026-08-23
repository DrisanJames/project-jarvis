package api

// Offer deploy gate + attach-offer repair (operator mandate 2026-08-22).
//
// WHY: mailing_campaigns.offer_id drives BOTH offer-level suppression (the
// planner loads mailing_offer_suppressions by OfferID at plan time —
// planPMTAAudience, pmta_campaign_planner.go) and conversion attribution
// (stampCampaignAttribution, campaign_attribution.go). A campaign deployed
// with a NULL offer therefore plans its audience WITHOUT the advertiser's
// suppression file and leaves its conversions unattributable. The board
// grid's MISSING_OFFER gate audits after the fact; this file is deploy-time
// ENFORCEMENT — the phase offer_proofs.go v1 explicitly deferred ("nothing
// here gates HandleDeployCampaign — enforcement is a later, separately gated
// phase").
//
// Three surfaces:
//  1. offerGateCheck — pure gate consulted by deployFromInput BEFORE any DB
//     work. Exemptions: KUMO-WARM/NEWSLETTER editorial names (offers are
//     BANNED in warm-up content, CLAUDE.md §13.1) and the partner drip
//     orchestrator (drip offers bind at the creative-row level). Kill switch
//     OFFER_DEPLOY_GATE_DISABLED=1 fails OPEN and logs loudly per deploy.
//  2. POST /pmta-campaign/{campaignId}/attach-offer — repair for a campaign
//     that already shipped/scheduled without an offer: sets offer_id +
//     offer_key and re-runs the attribution stamp. The response ALWAYS
//     carries suppression_caveat because attaching after planning cannot
//     retroactively apply the offer's suppression file — the full fix is a
//     Day Cards rebuild with the offer.
//  3. The exempt-name predicate (moved here from board_grid.go so the audit
//     gate and the deploy gate can never disagree).

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// offerGateKillSwitch fails the deploy gate OPEN when set to "1". Every
// bypassed deploy is logged loudly — the switch is a live-send-day escape
// hatch, not a quiet opt-out.
const offerGateKillSwitch = "OFFER_DEPLOY_GATE_DISABLED"

// offerGateFailMessage is the actionable 422 body for a blocked deploy.
const offerGateFailMessage = "campaign has no offer_id — offer suppression and conversion attribution cannot apply. " +
	"Assign an offer (Campaign Manager, or Day Cards → rebuild with offer) or, for editorial/newsletter content, " +
	"name it accordingly. Kill switch: OFFER_DEPLOY_GATE_DISABLED."

// attachOfferSuppressionCaveat rides on EVERY attach-offer response: the
// audience was planned before the offer existed on the row, so the offer's
// suppression file did not subtract from it.
const attachOfferSuppressionCaveat = "audience was planned before this offer was attached — the offer's suppression " +
	"file did NOT apply; for full correctness rebuild via Day Cards"

// isOfferExemptName reports whether a campaign legitimately carries no offer:
// KUMO-WARM (and any explicitly newsletter-labelled) campaigns mail editorial
// warm-up content in which offers are BANNED (CLAUDE.md §13.1), so gating them
// on offer_id would raise a false blocker on every kumo cell, every day.
// Shared by the board grid's MISSING_OFFER audit gate (board_grid.go) and the
// deploy-time offer gate below — one predicate, never two.
func isOfferExemptName(name string) bool {
	n := strings.ToUpper(name)
	return strings.Contains(n, "KUMO-WARM") || strings.Contains(n, "NEWSLETTER")
}

// internalCallerCtxKey threads the X-Internal-Caller header value from
// HandleDeployCampaign (the only place the header is read) down to
// deployFromInput, which has no *http.Request. In-process callers that never
// set it (Domain Agent, Day Cards rebuild) read back "".
type internalCallerCtxKey struct{}

func withInternalCaller(ctx context.Context, caller string) context.Context {
	return context.WithValue(ctx, internalCallerCtxKey{}, caller)
}

func internalCallerFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(internalCallerCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// offerGateError maps to HTTP 422 in HandleDeployCampaign — distinct from
// deployInputError (400) because the payload is well-formed; it is the offer
// binding that is missing.
type offerGateError struct{ reason string }

func (e *offerGateError) Error() string { return e.reason }

// offerGateCheck is the deploy-time offer gate. Pure — no DB, no network —
// so deployFromInput can run it before ANY write or even preflight.
// ok=false blocks the deploy; ok=true with a non-empty reason is an exempted
// or bypassed pass the caller should log.
func offerGateCheck(input engine.PMTACampaignInput, internalCaller string) (bool, string) {
	if strings.TrimSpace(input.OfferID) != "" {
		return true, ""
	}
	if isOfferExemptName(input.Name) {
		return true, "offer-exempt name (KUMO-WARM/NEWSLETTER editorial — offers are banned in warm-up content)"
	}
	if internalCaller == "partner_drip_orchestrator" {
		return true, "internal caller partner_drip_orchestrator (drip offers bind at the creative-row level)"
	}
	if os.Getenv(offerGateKillSwitch) == "1" {
		return true, "OFFER GATE BYPASSED by " + offerGateKillSwitch + "=1 — deploying with NO offer_id: " +
			"offer suppression and conversion attribution will NOT apply to this campaign"
	}
	return false, offerGateFailMessage
}

// resolveOfferKeyForOfferID resolves the operational offer_key for a
// mailing_offers UUID the same way stampCampaignAttribution's payload path
// does: landing_page_slug first, then the slug-map reverse lookup through
// everflow_offer_id. Returns "" when nothing resolves (log-and-continue).
func resolveOfferKeyForOfferID(ctx context.Context, db attributionQuerier, orgID, campaignID, offerID string) string {
	var slug, everflowID string
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(NULLIF(lower(landing_page_slug), ''), ''), COALESCE(everflow_offer_id, '')
		FROM mailing_offers
		WHERE id = $1 AND organization_id = $2
	`, offerID, orgID).Scan(&slug, &everflowID)
	if err != nil && err != sql.ErrNoRows {
		log.Printf("[attribution] campaign %s: offer_key lookup for payload offer %s failed (continuing): %v", campaignID, offerID, err)
	}
	if slug == "" && everflowID != "" {
		// Best-effort reverse lookup through the slug map.
		if err := db.QueryRowContext(ctx, `
			SELECT lower(cratoolpro_slug) FROM mailing_offer_slug_map
			WHERE everflow_offer_id = $1
			ORDER BY cratoolpro_slug ASC LIMIT 1
		`, everflowID).Scan(&slug); err != nil && err != sql.ErrNoRows {
			log.Printf("[attribution] campaign %s: slug-map reverse lookup ef=%s failed (continuing): %v", campaignID, everflowID, err)
		}
	}
	return slug
}

// attachOfferTerminalStatuses cannot take a repair attach: the send is over
// (or the row is gone) and the sanctioned recovery is a Day Cards rebuild of
// a sibling.
var attachOfferTerminalStatuses = map[string]bool{
	"cancelled":             true,
	"deleted":               true,
	"sent":                  true,
	"completed":             true,
	"completed_with_errors": true,
}

// HandleAttachOffer repairs an already-deployed/scheduled campaign that has
// no offer: POST /api/mailing/pmta-campaign/{campaignId}/attach-offer
// {offer_id, confirmed}. Unconfirmed → dry preview, zero writes. Confirmed →
// sets mailing_campaigns.offer_id + offer_key, re-runs the attribution stamp
// (stampCampaignAttribution — idempotent; for an unstamped row it lands the
// full payload-path stamp), audit-logged. suppression_caveat rides on every
// response: the planner already ran, so the offer's suppression file did NOT
// subtract from this audience.
func (s *PMTACampaignService) HandleAttachOffer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)

	campaignID := strings.TrimSpace(chi.URLParam(r, "campaignId"))
	if _, err := uuid.Parse(campaignID); err != nil {
		respondError(w, http.StatusBadRequest, "campaignId must be a UUID")
		return
	}

	var req struct {
		OfferID   string `json:"offer_id"`
		Confirmed bool   `json:"confirmed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	offerID := strings.TrimSpace(req.OfferID)
	if _, err := uuid.Parse(offerID); err != nil {
		respondError(w, http.StatusBadRequest, "offer_id must be a mailing_offers UUID")
		return
	}

	// ── Campaign: org-scoped, non-terminal ──────────────────────────────
	var name, status string
	var curOfferID, curOfferKey, configJSON sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(name,''), status, offer_id::text, COALESCE(offer_key,''), COALESCE(pmta_config::text,'')
		FROM mailing_campaigns
		WHERE id = $1 AND organization_id = $2
	`, campaignID, orgID).Scan(&name, &status, &curOfferID, &curOfferKey, &configJSON)
	if err != nil {
		respondError(w, http.StatusNotFound, "campaign not found")
		return
	}
	if attachOfferTerminalStatuses[status] {
		respondJSON(w, http.StatusConflict, map[string]interface{}{
			"error":              "campaign is " + status + " — an attach cannot repair a finished send; rebuild a sibling via Day Cards instead",
			"status":             status,
			"suppression_caveat": attachOfferSuppressionCaveat,
		})
		return
	}

	// ── Offer: must exist, org-scoped ───────────────────────────────────
	var offerName, offerStatus string
	err = s.db.QueryRowContext(ctx, `
		SELECT name, COALESCE(status, 'draft')
		FROM mailing_offers
		WHERE id = $1 AND organization_id = $2
	`, offerID, orgID).Scan(&offerName, &offerStatus)
	if err != nil {
		respondError(w, http.StatusNotFound, "offer not found: "+offerID)
		return
	}
	offerKey := resolveOfferKeyForOfferID(ctx, s.db, orgID, campaignID, offerID)

	// ── Unconfirmed → dry preview, zero writes ──────────────────────────
	if !req.Confirmed {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"dry_run": true,
			"campaign": map[string]interface{}{
				"id":        campaignID,
				"name":      name,
				"status":    status,
				"offer_id":  strings.TrimSpace(curOfferID.String),
				"offer_key": curOfferKey.String,
			},
			"offer": map[string]interface{}{
				"id":     offerID,
				"name":   offerName,
				"key":    offerKey,
				"status": offerStatus,
			},
			"suppression_caveat": attachOfferSuppressionCaveat,
		})
		return
	}

	// ── Confirmed: land offer_id + offer_key on the row ─────────────────
	res, err := s.db.ExecContext(ctx, `
		UPDATE mailing_campaigns
		SET offer_id = $2::uuid,
		    offer_key = COALESCE(NULLIF($3, ''), offer_key),
		    updated_at = NOW()
		WHERE id = $1 AND organization_id = $4
	`, campaignID, offerID, offerKey, orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "attach offer update failed: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(w, http.StatusNotFound, "campaign not found")
		return
	}

	// ── Re-run the attribution stamp exactly the way the finalizer does ──
	// (audience_finalizer.go:209-217). The stored campaign_input blob supplies
	// name/subject/html/from_name; a pre-blob campaign stamps with what it has
	// — the offer_id/offer_key themselves already landed via the UPDATE above.
	// stampCampaignAttribution is idempotency-gated on attribution_source: an
	// unstamped row gets the full payload-path stamp (creative/subject dims +
	// attribution_source='payload' + auto-tags); an already-stamped row keeps
	// its stamp (the UPDATE above corrected the offer columns) and re-derives
	// tags. Log-and-continue by design — the attach itself is already durable.
	input := engine.PMTACampaignInput{Name: name}
	if configJSON.Valid && configJSON.String != "" && configJSON.String != "{}" {
		var cfg struct {
			CampaignInput json.RawMessage `json:"campaign_input"`
		}
		if json.Unmarshal([]byte(configJSON.String), &cfg) == nil && len(cfg.CampaignInput) > 2 {
			_ = json.Unmarshal(cfg.CampaignInput, &input)
		}
	}
	input.OfferID = offerID
	if input.Name == "" {
		input.Name = name
	}
	stampSubject, stampHTML, stampFromName := "", "", ""
	if len(input.Variants) > 0 {
		stampSubject = input.Variants[0].Subject
		stampHTML = input.Variants[0].HTMLContent
		stampFromName = input.Variants[0].FromName
	}
	stampCtx, stampCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	stampCampaignAttribution(stampCtx, s.db, orgID, campaignID, input, input.Name, stampSubject, stampHTML, stampFromName)
	stampCancel()

	writeAuditLog(ctx, s.db, actorFromRequest(r), "attach_offer", "mailing_campaign", campaignID,
		map[string]interface{}{
			"offer_id":  strings.TrimSpace(curOfferID.String),
			"offer_key": curOfferKey.String,
			"status":    status,
		},
		map[string]interface{}{
			"offer_id":   offerID,
			"offer_key":  offerKey,
			"offer_name": offerName,
			"caveat":     attachOfferSuppressionCaveat,
		})
	log.Printf("[OfferGate] attach-offer: campaign %s (%q, %s) ← offer %s (%q key=%q) by %s — suppression caveat applies",
		campaignID, name, status, offerID, offerName, offerKey, actorFromRequest(r))

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"campaign_id":        campaignID,
		"name":               name,
		"status":             status,
		"offer_id":           offerID,
		"offer_key":          offerKey,
		"offer_name":         offerName,
		"suppression_caveat": attachOfferSuppressionCaveat,
	})
}
