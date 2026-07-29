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
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

type creativeProofRequest struct {
	ToEmail string `json:"to_email"`
	// Transport picks the sending route for this proof: "pmta" (default —
	// the brand's dedicated-IP em.<apex> profile, unchanged behavior) or
	// "ses" (the brand's live SES tenant profile: m.<apex>, via_ses=true —
	// the exact profile SES-routed campaigns deploy with), so a proof
	// measures cold-inbox placement on the same route live mail takes.
	Transport string `json:"transport"`
	// RouteViaGateway opts this proof into Smart Link Gateway routing: the
	// creative's cratoolpro money links are rewritten to the brand-site
	// gateway BEFORE tracking injection, so the operator can click the proof
	// and confirm the real bot/human/bridge routing before broad rollout.
	RouteViaGateway bool `json:"route_via_gateway"`
	// GatewaySlug is the smart-link slug to route through (e.g. "auto-refi").
	// Required when RouteViaGateway is true; must be a valid slugPattern slug
	// and correspond to an active mailing_smart_links row for the resolved
	// brand root.
	GatewaySlug string `json:"gateway_slug"`
	// ZWSecret optionally supplies the zero-width Subject payload for THIS test
	// send directly (parameterizing encode_secret), bypassing the per-domain
	// mailing_sending_profiles config so the operator can verify the mechanism
	// without a live DB write. It is still gated Yahoo-only + the global kill
	// switch: the payload is only woven in when the recipient classifies to the
	// yahoo ISP group. Empty → fall back to the domain's stored config.
	ZWSecret string `json:"zw_secret"`
}

// proofTransportPMTA / proofTransportSES are the two sending routes a proof
// can take. PMTA is the default and matches all pre-existing behavior.
const (
	proofTransportPMTA = "pmta"
	proofTransportSES  = "ses"
)

// normalizeProofTransport maps a request's transport field to a canonical
// value. "" defaults to PMTA (backward compatible); anything else must be an
// exact known transport.
func normalizeProofTransport(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", proofTransportPMTA:
		return proofTransportPMTA, nil
	case proofTransportSES:
		return proofTransportSES, nil
	default:
		return "", fmt.Errorf("invalid transport %q — must be \"pmta\" or \"ses\"", s)
	}
}

// proofProfile is the resolved sending profile a proof will go out through,
// including the SES tenant routing fields needed for header parity with the
// production send worker (send_worker.go processItem's via_ses branch).
type proofProfile struct {
	ID            string
	FromEmail     string
	TrackBase     string
	SendingDomain string
	ViaSES        bool
	SESConfigSet  string
	SESTenant     string
	// RawCreative (operator 2026-07-25, wcl-heloc): proofs mirror the live
	// worker — send the creative AS-IS, stripping any stored compliance-footer
	// block instead of appending one.
	RawCreative bool
}

// proofGateway carries the validated brand root + slug used to rewrite a
// proof's money links to the Smart Link Gateway. nil means "no gateway" —
// the default-safe path with zero smart-link queries and no rewrite.
type proofGateway struct {
	BrandRoot string
	Slug      string
	// Hash is the smart-link dictionary key read at preflight; it keys the
	// tracking-layer /o/<sub>/<hash>/<campaign> offer URL the proof is
	// rewritten to.
	Hash string
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
	transport, terr := normalizeProofTransport(req.Transport)
	if terr != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": terr.Error()})
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

	// Optional Smart Link Gateway routing. When RouteViaGateway is false we make
	// ZERO smart-link queries and no rewrite — behavior identical to before this
	// feature existed. When true, we hard-preflight the active gateway row and
	// DO NOT SEND if it is absent, so a proof never silently ships un-routed.
	var gw *proofGateway
	var gwSmartLink SmartLink
	if req.RouteViaGateway {
		slug := strings.ToLower(strings.TrimSpace(req.GatewaySlug))
		if slug == "" || !slugPattern.MatchString(slug) {
			respondJSON(w, http.StatusBadRequest,
				map[string]string{"error": "gateway_slug required and must be a valid slug"})
			return
		}
		brandRoot := brandRootFromCode(brandCode)
		if brandRoot == "" {
			respondJSON(w, http.StatusUnprocessableEntity,
				map[string]string{"error": "cannot resolve a brand root for gateway routing from brand_code '" + brandCode + "'"})
			return
		}
		sl, resolveErr := h.smartLinks.resolveActiveSmartLink(ctx, brandRoot, slug)
		if resolveErr == sql.ErrNoRows {
			respondJSON(w, http.StatusUnprocessableEntity,
				map[string]string{"error": "no active smart-link gateway " + brandRoot + "/" + slug + " — seed it in the Smart Links tab first"})
			return
		}
		if resolveErr != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": resolveErr.Error()})
			return
		}
		gwSmartLink = sl
		// Hash comes from the preflighted row; it keys the tracking-layer /o/
		// offer URL the proof's money links are rewritten to.
		gw = &proofGateway{BrandRoot: brandRoot, Slug: slug, Hash: sl.Hash}
	}

	messageID, trackingURL, zwRes, sendErr := h.sendProofMessage(ctx, orgID.String(), sendingDomain, transport,
		"[PROOF] "+subject, "", preheader, htmlContent, to, creativeID,
		map[string]string{"X-Proof-Send": "true", "X-Creative-ID": creativeID}, gw, req.ZWSecret)
	if sendErr != nil {
		log.Printf("[creative-proof] %s send error creative=%s domain=%s: %v", transport, creativeID, sendingDomain, sendErr)
		respondJSON(w, http.StatusOK, map[string]string{"status": "error", "error": sendErr.Error()})
		return
	}
	log.Printf("[creative-proof] sent creative=%s to=%s via %s messageID=%s domain=%s isp=%s zw_encoded=%t zw_reason=%s",
		creativeID, to, transport, messageID, sendingDomain, worker.ClassifySubscriberISP(to), zwRes.Applied, zwRes.Reason)

	resp := map[string]string{"status": "sent", "message_id": messageID, "transport": transport}
	// Report the zero-width Subject decision so a test send is self-explaining:
	// zw_encoded=true means the invisible payload was woven in; otherwise
	// zw_skip_reason names the gate that stopped it (e.g. recipient_not_yahoo).
	resp["zw_encoded"] = strconv.FormatBool(zwRes.Applied)
	if !zwRes.Applied {
		resp["zw_skip_reason"] = zwRes.Reason
	}
	if gw != nil {
		// tracking_url is the exact /o/ URL minted into the proof HTML so the
		// operator (and the UI) can see and click precisely what was sent.
		resp["tracking_url"] = trackingURL
		resp["gateway_slug"] = gw.Slug
		resp["hash"] = gw.Hash
		resp["risk_profile"] = gwSmartLink.RiskProfile
	}
	respondJSON(w, http.StatusOK, resp)
}

// sendProofMessage renders an HTML creative with the proof Liquid context,
// injects tracking + unsubscribe, and sends ONE message to `to` through the
// active PMTA sending profile for `sendingDomain`. It is the reusable core
// shared by the creative-proof and offer-proof send paths. extraHeaders are
// merged in (List-Unsubscribe is added automatically when a track base exists).
// Returns the PMTA message id, or an error if the profile is unresolved or the
// send fails. fromName "" leaves the profile/default from name.
// It also returns the tracking-layer /o/ offer URL that was minted into the
// HTML when gw != nil (empty otherwise), so the caller can surface the exact
// clickable link in the response.
func (h *ProofSendHandler) sendProofMessage(ctx context.Context, orgID, sendingDomain, transport, subject, fromName, preheader, htmlContent, to, refID string, extraHeaders map[string]string, gw *proofGateway, zwSecret string) (string, string, worker.SubjectZWResult, error) {
	pp, err := h.resolveProofProfile(ctx, sendingDomain, transport)
	if err != nil {
		return "", "", worker.SubjectZWResult{Subject: subject}, err
	}
	profileID, fromEmail, trackBase := pp.ID, pp.FromEmail, pp.TrackBase

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
	// Zero-width Subject steganography — mirror the production send worker
	// (send_worker.go processItem): apply the invisible payload AFTER Liquid
	// render, gated Yahoo-only + the global kill switch. The secret comes from
	// the per-domain mailing_sending_profiles config, or the inline zwSecret
	// test override when supplied. No-op (subject unchanged) for every other
	// case; zwRes records the decision for the response.
	zwRes := worker.ApplySubjectZW(ctx, h.db, renderedSubject, recipientISP, profileID, zwSecret)
	renderedSubject = zwRes.Subject
	renderedHTML, rerr := ts.Render("", htmlContent, rc)
	if rerr != nil {
		log.Printf("[proof-send] Liquid render error (html): %v", rerr)
		renderedHTML = htmlContent
	}
	// raw_creative profiles (operator 2026-07-25, wcl-heloc): the proof mirrors
	// the live send — creative AS-IS, any stored compliance-footer block removed.
	if pp.RawCreative {
		renderedHTML = stripUnsubDisclaimer(renderedHTML)
	}

	// Wave-1 tracking-layer rewrite (opt-in): cratoolpro money links -> the
	// tracking service's /o/<sub>/<hash>/<campaign> offer URL, BEFORE tracking
	// injection. The /o/ URL is itself a tracking URL, so RewriteClickLinks
	// (inside InjectTrackingPixelAndLinks) is taught to skip it — the final
	// href stays the bare /o/ URL, exactly what a real click-tester hits. The
	// rewrite needs a real tracking domain (trackBase); when trackBase=="" the
	// rewriter no-ops (returns 0) and the original cratoolpro href survives,
	// which is the safe failure rather than shipping https:///o/...
	var trackingURL string
	if gw != nil && trackBase != "" {
		trackingURL = SmartLinkTrackingURL(trackBase, proofSubscriberID, gw.Hash, proofCampaignID)
		var rewritten int
		renderedHTML, rewritten = RewriteMoneyLinksToTracking(renderedHTML, trackBase, proofSubscriberID, gw.Hash, proofCampaignID)
		log.Printf("[proof-send] tracking-rewrote %d money link(s) → %s", rewritten, trackingURL)
	} else if gw != nil {
		log.Printf("[proof-send] gateway routing requested but no tracking base resolved for domain %s — money links left as-is", sendingDomain)
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
	// SES tenant routing parity with the production send worker
	// (send_worker.go processItem, via_ses branch): PMTA's HTTP bridge passes
	// these through to the SES SMTP relay, which routes the message to the
	// named tenant + configuration set. Without them a via_ses proof would
	// relay untagged and not mirror the live campaign route. Message tags are
	// deliberately omitted — they're SES-side metadata (invisible to the
	// receiving ISP) and would attribute events to the pseudo proof campaign.
	if pp.ViaSES {
		if pp.SESConfigSet != "" {
			headers["X-SES-CONFIGURATION-SET"] = pp.SESConfigSet
		}
		if pp.SESTenant != "" {
			headers["X-SES-TENANT"] = pp.SESTenant
		}
	}
	if unsubURL != "" {
		// Shared RFC 8058 helper (2026-07-21): proofs previously hand-rolled an
		// https-only List-Unsubscribe with no mailto leg; emit the identical
		// header pair production sends carry so a proof is header-parity with
		// the real campaign send.
		worker.BuildListUnsubscribeHeaders(orgID, proofCampaignID, proofSubscriberID,
			brand.RootFromEmail(fromEmail), fromEmail, trackBase, h.trackingSecret, headers)
	}

	msg := &worker.EmailMessage{
		Email:        to,
		FromName:     fromName,
		FromEmail:    fromEmail,
		Subject:      renderedSubject,
		HTMLContent:  renderedHTML,
		TextContent:  worker.GenerateTextFromHTML(renderedHTML),
		ProfileID:    profileID,
		RecipientISP: recipientISP,
		Headers:      headers,
	}

	result, sendErr := h.sender.Send(ctx, msg)
	if sendErr != nil {
		return "", "", zwRes, sendErr
	}
	if result != nil {
		return result.MessageID, trackingURL, zwRes, nil
	}
	return "", trackingURL, zwRes, nil
}

// resolveProofProfile looks up the active sending profile a proof should route
// through for a brand sending domain + transport.
//
//   - transport "pmta": the dedicated-IP profile on em.<apex> — the exact
//     query the proof paths have always used (vendor_type='pmta', active,
//     is_default first). All em.<apex> rows are via_ses=false in prod.
//   - transport "ses": the brand's live SES tenant profile — sending domain
//     m.<apex>, via_ses=true. This is the SAME profile SES-routed board
//     campaigns pin (e.g. "Discount Blog (SES Tenant)"), so the proof's
//     route, MAIL FROM, relay pool, and tracking domain are all
//     campaign-parity. The ses_configuration_set / ses_tenant_name fields
//     feed the X-SES-* headers the send worker stamps on live via_ses sends.
//
// The domain argument may be em.<apex>, m.<apex>, or a bare apex; the SES
// branch normalizes to m.<apex>.
func (h *ProofSendHandler) resolveProofProfile(ctx context.Context, sendingDomain, transport string) (proofProfile, error) {
	if sendingDomain == "" {
		return proofProfile{}, fmt.Errorf("no sending domain")
	}
	// Default (pmta) keeps the exact historical lookup: whatever active PMTA
	// profile lives on the given domain, no via_ses filter — so operator
	// selections of an m.<apex> domain keep resolving as they always have.
	// "ses" FORCES the brand's SES tenant profile: normalize any domain form
	// to m.<apex> and require via_ses=true.
	lookupDomain := sendingDomain
	sesFilter := ""
	if transport == proofTransportSES {
		apex := strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(sendingDomain), "em."), "m.")
		lookupDomain = "m." + apex
		sesFilter = "AND COALESCE(via_ses, FALSE) = TRUE"
	}

	var pp proofProfile
	var trackingDomain, sDomain sql.NullString
	err := h.db.QueryRowContext(ctx,
		`SELECT id::text, COALESCE(from_email,''), tracking_domain, sending_domain,
		        COALESCE(via_ses, FALSE), COALESCE(ses_configuration_set,''), COALESCE(ses_tenant_name,''),
		        COALESCE(raw_creative, FALSE)
		 FROM mailing_sending_profiles
		 WHERE sending_domain = $1 AND vendor_type = 'pmta' AND status = 'active' `+sesFilter+`
		 ORDER BY is_default DESC, created_at DESC LIMIT 1`,
		lookupDomain,
	).Scan(&pp.ID, &pp.FromEmail, &trackingDomain, &sDomain, &pp.ViaSES, &pp.SESConfigSet, &pp.SESTenant,
		&pp.RawCreative)
	if err != nil {
		log.Printf("[proof-send] no %s sending profile for domain %s: %v", transport, lookupDomain, err)
		return proofProfile{}, fmt.Errorf("no active %s sending profile for domain '%s'", transport, lookupDomain)
	}

	pp.SendingDomain = lookupDomain
	pp.TrackBase = h.trackingURL
	if trackingDomain.Valid && trackingDomain.String != "" {
		pp.TrackBase = ensureHTTPS(trackingDomain.String)
	} else if sDomain.Valid && sDomain.String != "" {
		pp.TrackBase = ensureHTTPS("trk." + sDomain.String)
	}
	return pp, nil
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
