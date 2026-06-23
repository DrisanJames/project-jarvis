package api

// Creative-registry proof send. Sister endpoint to offer_proof_send.go's
// HandleProofSend, but keyed on a mailing_creatives row (the ReviewForge /
// converter archive surfaced in the portal) rather than an offer's creative.
//
// POST /api/mailing/creatives/{id}/send-proof  body {"to_email":"<addr>"}
//
// It reuses ProofSendHandler's machinery wholesale: the proof campaign/
// subscriber UUIDs, the Liquid render context (buildProofRenderContext),
// tracking-pixel/link injection (worker.InjectTrackingPixelAndLinks), and the
// profile-based PMTA ESPSender. The ONLY new piece is brand resolution: a
// mailing_creatives row carries a brand_code (e.g. "DB", or the studio slug
// "discount-blog"), not a web_property, so we resolve it through the same
// brandRootFromCode() the money-link checker uses, derive the em.<apex> sending
// domain, and look up the active PMTA sending profile for that domain.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

type creativeProofRequest struct {
	ToEmail string `json:"to_email"`
}

// HandleCreativeProof sends ONE proof of a mailing_creatives row to to_email.
// Mirrors the frontend contract used by the offer proof: 200 with a
// {"status":"sent",...} / {"status":"error",...} body for send-time failures,
// and conventional 4xx JSON errors for request/lookup problems.
func (h *ProofSendHandler) HandleCreativeProof(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "organization not resolved: " + err.Error()})
		return
	}
	creativeID := chi.URLParam(r, "id")
	if creativeID == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "missing creative id"})
		return
	}

	var req creativeProofRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	to := strings.TrimSpace(req.ToEmail)
	if to == "" || !looksLikeEmail(to) {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "to_email required"})
		return
	}

	// Org-scoped creative load.
	var htmlContent, subject, preheader, brandCode string
	err = h.db.QueryRowContext(ctx,
		`SELECT COALESCE(html_content,''), COALESCE(subject,''), COALESCE(preheader,''), COALESCE(brand_code,'')
		 FROM mailing_creatives WHERE id = $1 AND organization_id = $2`,
		creativeID, orgID.String(),
	).Scan(&htmlContent, &subject, &preheader, &brandCode)
	if err == sql.ErrNoRows {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "creative not found"})
		return
	}
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if htmlContent == "" {
		respondJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "creative has no html_content"})
		return
	}

	// Resolve the sending domain from the creative's brand_code, then the active
	// PMTA sending profile for that domain.
	sendingDomain := sendingDomainFromBrandCode(brandCode)
	if sendingDomain == "" {
		respondJSON(w, http.StatusUnprocessableEntity,
			map[string]string{"error": "could not resolve a sending domain for brand_code '" + brandCode + "'"})
		return
	}
	if subject == "" {
		subject = "Creative Proof"
	}
	messageID, sendErr := h.sendProofMessage(ctx, orgID.String(), sendingDomain,
		"[PROOF] "+subject, "", preheader, htmlContent, to, creativeID,
		map[string]string{"X-Proof-Send": "true", "X-Creative-ID": creativeID})
	if sendErr != nil {
		log.Printf("[creative-proof] PMTA error creative=%s domain=%s: %v", creativeID, sendingDomain, sendErr)
		respondJSON(w, http.StatusOK, map[string]string{"status": "error", "error": sendErr.Error()})
		return
	}
	log.Printf("[creative-proof] sent creative=%s to=%s via PMTA messageID=%s domain=%s isp=%s",
		creativeID, to, messageID, sendingDomain, worker.ClassifySubscriberISP(to))
	respondJSON(w, http.StatusOK, map[string]string{"status": "sent", "message_id": messageID})
}

// sendProofMessage renders an HTML creative with the proof Liquid context,
// injects tracking + unsubscribe, and sends ONE message to `to` through the
// active PMTA sending profile for `sendingDomain`. It is the reusable core
// shared by the creative-proof and offer-proof send paths. extraHeaders are
// merged in (List-Unsubscribe is added automatically when a track base exists).
// Returns the PMTA message id, or an error if the profile is unresolved or the
// send fails. fromName "" leaves the profile/default from name.
func (h *ProofSendHandler) sendProofMessage(ctx context.Context, orgID, sendingDomain, subject, fromName, preheader, htmlContent, to, refID string, extraHeaders map[string]string) (string, error) {
	profileID, fromEmail, trackBase := h.resolveSendingProfileByDomain(ctx, orgID, sendingDomain)
	if profileID == "" {
		return "", fmt.Errorf("no active PMTA sending profile for domain '%s'", sendingDomain)
	}

	recipientISP := worker.ClassifySubscriberISP(to)
	ts := mailing.NewTemplateService()
	emailID := uuid.New().String()

	var unsubURL string
	if trackBase != "" && h.trackingSecret != "" {
		unsubURL = worker.GenerateUnsubscribeURL(orgID, proofCampaignID, proofSubscriberID, trackBase, h.trackingSecret)
	}

	rc := buildProofRenderContext(to, trackBase, emailID, unsubURL)
	if preheader != "" {
		rc["preheader"] = preheader
	}

	renderedSubject, rerr := ts.Render("", subject, rc)
	if rerr != nil {
		log.Printf("[proof-send] Liquid render error (subject): %v", rerr)
		renderedSubject = subject
	}
	renderedHTML, rerr := ts.Render("", htmlContent, rc)
	if rerr != nil {
		log.Printf("[proof-send] Liquid render error (html): %v", rerr)
		renderedHTML = htmlContent
	}

	renderedHTML = worker.ReplaceTrackingMergeTags(renderedHTML, proofCampaignID, proofSubscriberID, refID)
	if trackBase != "" && h.trackingSecret != "" {
		renderedHTML = worker.InjectTrackingPixelAndLinks(
			renderedHTML, proofCampaignID, proofSubscriberID, emailID, trackBase, orgID, h.trackingSecret,
		)
		renderedHTML = strings.ReplaceAll(renderedHTML, "{{ system.unsubscribe_url }}", unsubURL)
		renderedHTML = strings.ReplaceAll(renderedHTML, "{{system.unsubscribe_url}}", unsubURL)
	}
	if !strings.Contains(renderedHTML, "<html") {
		renderedHTML = "<html><body>" + renderedHTML + "</body></html>"
	}

	headers := map[string]string{}
	for k, v := range extraHeaders {
		headers[k] = v
	}
	if unsubURL != "" {
		headers["List-Unsubscribe"] = "<" + unsubURL + ">"
		headers["List-Unsubscribe-Post"] = "List-Unsubscribe=One-Click"
	}

	msg := &worker.EmailMessage{
		Email:        to,
		FromName:     fromName,
		FromEmail:    fromEmail,
		Subject:      renderedSubject,
		HTMLContent:  renderedHTML,
		ProfileID:    profileID,
		RecipientISP: recipientISP,
		Headers:      headers,
	}

	result, sendErr := h.sender.Send(ctx, msg)
	if sendErr != nil {
		return "", sendErr
	}
	if result != nil {
		return result.MessageID, nil
	}
	return "", nil
}

// resolveSendingProfileByDomain looks up the active PMTA sending profile for an
// explicit sending domain (the brand_code path can't go through brandKits, which
// only covers 4 brands). Mirrors resolveSendingProfile's query/track-base logic.
func (h *ProofSendHandler) resolveSendingProfileByDomain(ctx context.Context, orgID, sendingDomain string) (profileID, fromEmail, trackBase string) {
	if sendingDomain == "" {
		return "", "", ""
	}
	var pID, fEmail string
	var trackingDomain, sDomain sql.NullString
	err := h.db.QueryRowContext(ctx,
		`SELECT id::text, COALESCE(from_email,''), tracking_domain, sending_domain
		 FROM mailing_sending_profiles
		 WHERE sending_domain = $1 AND vendor_type = 'pmta' AND status = 'active'
		 ORDER BY is_default DESC, created_at DESC LIMIT 1`,
		sendingDomain,
	).Scan(&pID, &fEmail, &trackingDomain, &sDomain)
	if err != nil {
		log.Printf("[creative-proof] no sending profile for domain %s: %v", sendingDomain, err)
		return "", "", ""
	}
	tb := h.trackingURL
	if trackingDomain.Valid && trackingDomain.String != "" {
		tb = ensureHTTPS(trackingDomain.String)
	} else if sDomain.Valid && sDomain.String != "" {
		tb = ensureHTTPS("trk." + sDomain.String)
	}
	return pID, fEmail, tb
}

// sendingDomainFromBrandCode maps a mailing_creatives brand_code to its em.<apex>
// PMTA sending domain. It first reuses brandRootFromCode (money_link_check.go),
// which resolves the canonical code map, owned-domain pass-through, AND the
// Creative Studio slug form — then prefixes "em.". Returns "" when unresolved.
func sendingDomainFromBrandCode(brandCode string) string {
	root := brandRootFromCode(brandCode)
	if root == "" {
		// Last-resort: a brand_code that is already a full sending domain.
		bc := strings.ToLower(strings.TrimSpace(brandCode))
		if strings.HasPrefix(bc, "em.") {
			return bc
		}
		return ""
	}
	return "em." + root
}

// looksLikeEmail is a minimal non-empty + shape check (one @, a dot in the
// domain, no spaces). Deliberately lenient — the PMTA sender is the real gate.
func looksLikeEmail(s string) bool {
	at := strings.Index(s, "@")
	if at <= 0 || at == len(s)-1 {
		return false
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	domain := s[at+1:]
	return strings.Contains(domain, ".") && !strings.HasSuffix(domain, ".")
}
