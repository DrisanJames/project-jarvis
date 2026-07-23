package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

type ProofSendHandler struct {
	db             *sql.DB
	sender         worker.ESPSender
	trackingURL    string
	trackingSecret string
	orgID          string
	// smartLinks resolves the active Smart Link Gateway row when a creative
	// proof opts into gateway routing (HandleCreativeProof). nil-safe: the
	// offer proof path never touches it.
	smartLinks *SmartLinkService
}

type proofSendRequest struct {
	Proofs         []proofItem `json:"proofs"`
	RecipientEmail string      `json:"recipient_email"`
	// Transport: "pmta" (default — the brand's dedicated-IP profile,
	// unchanged) or "ses" (the brand's live SES tenant profile) so proofs can
	// measure cold-inbox placement on either route. See normalizeProofTransport.
	Transport string `json:"transport"`
}

type proofItem struct {
	CreativeID    string `json:"creative_id"`
	SubjectLineID string `json:"subject_line_id"`
	FromNameID    string `json:"from_name_id"`
}

type proofResult struct {
	CreativeID string `json:"creative_id"`
	Status     string `json:"status"`
	MessageID  string `json:"message_id,omitempty"`
	Error      string `json:"error,omitempty"`
}

type deployOfferRequest struct {
	Creatives []proofItem `json:"creatives"`
}

type deployResult struct {
	CreativeID string `json:"creative_id"`
	TemplateID string `json:"template_id"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
}

const proofCampaignID = "00000000-0000-0000-0000-00000000ff01"
const proofSubscriberID = "00000000-0000-0000-0000-00000000ff02"

func (h *ProofSendHandler) HandleProofSend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		http.Error(w, `{"error":"missing offer id"}`, http.StatusBadRequest)
		return
	}

	var req proofSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if len(req.Proofs) == 0 {
		http.Error(w, `{"error":"proofs array is empty"}`, http.StatusBadRequest)
		return
	}
	if req.RecipientEmail == "" || !strings.Contains(req.RecipientEmail, "@") {
		http.Error(w, `{"error":"invalid recipient_email"}`, http.StatusBadRequest)
		return
	}

	transport, terr := normalizeProofTransport(req.Transport)
	if terr != nil {
		http.Error(w, `{"error":"`+terr.Error()+`"}`, http.StatusBadRequest)
		return
	}

	var webProperty string
	err := h.db.QueryRowContext(ctx,
		`SELECT COALESCE(web_property,'') FROM mailing_offers WHERE id = $1`, offerID,
	).Scan(&webProperty)
	if err != nil {
		http.Error(w, `{"error":"offer not found"}`, http.StatusNotFound)
		return
	}

	var brandDomain string
	if kit, ok := GetBrandKit(webProperty); ok {
		brandDomain = kit.SendingDomain
	}
	pp, perr := h.resolveProofProfile(ctx, brandDomain, transport)
	if perr != nil {
		http.Error(w, `{"error":"no active `+transport+` sending profile for this brand"}`, http.StatusUnprocessableEntity)
		return
	}

	recipientISP := worker.ClassifySubscriberISP(req.RecipientEmail)
	log.Printf("[proof-send] offer=%s domain=%s transport=%s profile=%s recipientISP=%s recipient=%s",
		offerID, pp.SendingDomain, transport, pp.ID, recipientISP, req.RecipientEmail)

	ts := mailing.NewTemplateService()

	results := make([]proofResult, 0, len(req.Proofs))
	for i, p := range req.Proofs {
		res := h.sendOneProof(ctx, ts, offerID, transport, pp, req.RecipientEmail, recipientISP, p, i)
		results = append(results, res)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"results":   results,
		"transport": transport,
	})
}

func (h *ProofSendHandler) sendOneProof(
	ctx context.Context,
	ts *mailing.TemplateService,
	offerID, transport string,
	pp proofProfile,
	recipientEmail, recipientISP string,
	p proofItem,
	index int,
) proofResult {
	profileID, fromEmail, trackBase, sendingDomain := pp.ID, pp.FromEmail, pp.TrackBase, pp.SendingDomain
	var htmlContent string
	err := h.db.QueryRowContext(ctx,
		`SELECT COALESCE(html_content,'') FROM mailing_offer_creatives WHERE id = $1 AND offer_id = $2`,
		p.CreativeID, offerID,
	).Scan(&htmlContent)
	if err != nil || htmlContent == "" {
		return proofResult{CreativeID: p.CreativeID, Status: "error", Error: "creative not found or empty"}
	}

	var subjectLine string
	err = h.db.QueryRowContext(ctx,
		`SELECT subject_line FROM mailing_offer_subject_lines WHERE id = $1 AND offer_id = $2`,
		p.SubjectLineID, offerID,
	).Scan(&subjectLine)
	if err != nil {
		return proofResult{CreativeID: p.CreativeID, Status: "error", Error: "subject line not found"}
	}

	var fromName string
	err = h.db.QueryRowContext(ctx,
		`SELECT from_name FROM mailing_offer_from_names WHERE id = $1 AND offer_id = $2`,
		p.FromNameID, offerID,
	).Scan(&fromName)
	if err != nil {
		return proofResult{CreativeID: p.CreativeID, Status: "error", Error: "from name not found"}
	}

	subject := "[PROOF] " + subjectLine
	emailID := uuid.New().String()

	var unsubURL string
	if trackBase != "" && h.trackingSecret != "" {
		unsubURL = worker.GenerateUnsubscribeURL(h.orgID, proofCampaignID, proofSubscriberID, trackBase, h.trackingSecret)
	}

	rc := buildProofRenderContext(recipientEmail, trackBase, emailID, unsubURL)

	renderedSubject, err := ts.Render("", subject, rc)
	if err != nil {
		log.Printf("[proof-send] Liquid render error (subject): %v", err)
		renderedSubject = subject
	}

	renderedHTML, err := ts.Render("", htmlContent, rc)
	if err != nil {
		log.Printf("[proof-send] Liquid render error (html): %v", err)
		renderedHTML = htmlContent
	}

	renderedHTML = worker.ReplaceTrackingMergeTags(renderedHTML, proofCampaignID, proofSubscriberID, p.CreativeID)

	if trackBase != "" && h.trackingSecret != "" {
		renderedHTML = worker.InjectTrackingPixelAndLinks(
			renderedHTML, proofCampaignID, proofSubscriberID, emailID, trackBase, h.orgID, h.trackingSecret,
		)
		renderedHTML = strings.ReplaceAll(renderedHTML, "{{ system.unsubscribe_url }}", unsubURL)
		renderedHTML = strings.ReplaceAll(renderedHTML, "{{system.unsubscribe_url}}", unsubURL)
	}

	if !strings.Contains(renderedHTML, "<html") {
		renderedHTML = "<html><body>" + renderedHTML + "</body></html>"
	}

	headers := map[string]string{
		"X-Proof-Send": "true",
		"X-Offer-ID":   offerID,
	}
	// SES tenant routing parity with the production send worker (send_worker.go
	// via_ses branch) — see the matching block in sendProofMessage.
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
		worker.BuildListUnsubscribeHeaders(h.orgID, proofCampaignID, proofSubscriberID,
			brand.RootFromEmail(fromEmail), fromEmail, trackBase, h.trackingSecret, headers)
	}

	msg := &worker.EmailMessage{
		Email:        recipientEmail,
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
		log.Printf("[proof-send] %s error for creative %s: %v", transport, p.CreativeID, sendErr)
		return proofResult{CreativeID: p.CreativeID, Status: "error", Error: sendErr.Error()}
	}

	messageID := ""
	if result != nil {
		messageID = result.MessageID
	}
	log.Printf("[proof-send] sent proof #%d creative=%s to=%s via %s messageID=%s domain=%s profile=%s isp=%s",
		index+1, p.CreativeID, recipientEmail, transport, messageID, sendingDomain, profileID, recipientISP)
	return proofResult{CreativeID: p.CreativeID, Status: "sent", MessageID: messageID}
}

// HandleDeployOffer promotes proofed offer creatives into the mailing_templates
// table so they become selectable in the PMTACampaignWizard's Content Library.
func (h *ProofSendHandler) HandleDeployOffer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		http.Error(w, `{"error":"missing offer id"}`, http.StatusBadRequest)
		return
	}

	var req deployOfferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if len(req.Creatives) == 0 {
		http.Error(w, `{"error":"creatives array is empty"}`, http.StatusBadRequest)
		return
	}

	var offerName, webProperty string
	err := h.db.QueryRowContext(ctx,
		`SELECT COALESCE(name,''), COALESCE(web_property,'') FROM mailing_offers WHERE id = $1`, offerID,
	).Scan(&offerName, &webProperty)
	if err != nil {
		http.Error(w, `{"error":"offer not found"}`, http.StatusNotFound)
		return
	}

	_, fromEmail, _, _ := h.resolveSendingProfile(ctx, webProperty)

	orgID, _ := uuid.Parse(h.orgID)
	templateStore := mailing.NewEmailTemplateStore(h.db)
	folderSvc := mailing.NewTemplateFolderService(h.db)

	folderPath := "Offer Deployments/" + offerName
	folder, folderErr := folderSvc.GetOrCreateFolderPath(ctx, orgID, folderPath)
	var folderID *uuid.UUID
	if folderErr == nil && folder != nil {
		folderID = &folder.ID
	} else if folderErr != nil {
		log.Printf("[deploy-offer] folder creation warning: %v", folderErr)
	}

	results := make([]deployResult, 0, len(req.Creatives))
	deployed := 0

	for _, item := range req.Creatives {
		res := h.deployOneCreative(ctx, templateStore, offerID, offerName, fromEmail, orgID, folderID, item)
		if res.Status == "deployed" {
			deployed++
		}
		results = append(results, res)
	}

	log.Printf("[deploy-offer] offer=%s deployed=%d/%d folder=%s", offerID, deployed, len(req.Creatives), folderPath)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"deployed":    deployed,
		"results":     results,
		"folder_path": folderPath,
	})
}

func (h *ProofSendHandler) deployOneCreative(
	ctx context.Context,
	templateStore *mailing.EmailTemplateStore,
	offerID, offerName, fromEmail string,
	orgID uuid.UUID,
	folderID *uuid.UUID,
	item proofItem,
) deployResult {
	var htmlContent string
	var version int
	err := h.db.QueryRowContext(ctx,
		`SELECT COALESCE(html_content,''), COALESCE(version,1) FROM mailing_offer_creatives WHERE id = $1 AND offer_id = $2`,
		item.CreativeID, offerID,
	).Scan(&htmlContent, &version)
	if err != nil || htmlContent == "" {
		return deployResult{CreativeID: item.CreativeID, Status: "error", Error: "creative not found or empty"}
	}

	var subjectLine string
	err = h.db.QueryRowContext(ctx,
		`SELECT subject_line FROM mailing_offer_subject_lines WHERE id = $1 AND offer_id = $2`,
		item.SubjectLineID, offerID,
	).Scan(&subjectLine)
	if err != nil {
		return deployResult{CreativeID: item.CreativeID, Status: "error", Error: "subject line not found"}
	}

	var fromName string
	err = h.db.QueryRowContext(ctx,
		`SELECT from_name FROM mailing_offer_from_names WHERE id = $1 AND offer_id = $2`,
		item.FromNameID, offerID,
	).Scan(&fromName)
	if err != nil {
		return deployResult{CreativeID: item.CreativeID, Status: "error", Error: "from name not found"}
	}

	var existingID string
	dupErr := h.db.QueryRowContext(ctx,
		`SELECT id::text FROM mailing_templates
		 WHERE folder_id = $1 AND subject = $2 AND html_content = $3
		 LIMIT 1`,
		folderID, subjectLine, htmlContent,
	).Scan(&existingID)
	if dupErr == nil && existingID != "" {
		return deployResult{CreativeID: item.CreativeID, TemplateID: existingID, Status: "deployed", Error: ""}
	}

	tplName := fmt.Sprintf("%s — %s (v%d)", offerName, subjectLine, version)
	if len(tplName) > 200 {
		tplName = tplName[:200]
	}

	createReq := &mailing.CreateTemplateRequest{
		FolderID:    folderID,
		Name:        tplName,
		Description: fmt.Sprintf("Deployed from offer: %s", offerName),
		Subject:     subjectLine,
		FromName:    fromName,
		FromEmail:   fromEmail,
		HTMLContent: htmlContent,
	}

	tpl, createErr := templateStore.CreateTemplate(ctx, orgID, createReq)
	if createErr != nil {
		log.Printf("[deploy-offer] template insert error for creative %s: %v", item.CreativeID, createErr)
		return deployResult{CreativeID: item.CreativeID, Status: "error", Error: createErr.Error()}
	}

	_, _ = h.db.ExecContext(ctx,
		`INSERT INTO mailing_offer_deployments (offer_id, campaign_id, creative_id, subject_line_id, from_name_id)
		 VALUES ($1, $2, $3, $4, $5)`,
		offerID, tpl.ID, item.CreativeID, item.SubjectLineID, item.FromNameID,
	)

	return deployResult{CreativeID: item.CreativeID, TemplateID: tpl.ID.String(), Status: "deployed"}
}

func (h *ProofSendHandler) resolveSendingProfile(ctx context.Context, webProperty string) (profileID, fromEmail, trackBase, sendingDomain string) {
	if webProperty != "" {
		if kit, ok := GetBrandKit(webProperty); ok {
			sendingDomain = kit.SendingDomain
		}
	}
	if sendingDomain == "" {
		return "", "", "", ""
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
		log.Printf("[proof-send] no sending profile for domain %s: %v", sendingDomain, err)
		return "", "", "", ""
	}

	tb := h.trackingURL
	if trackingDomain.Valid && trackingDomain.String != "" {
		tb = ensureHTTPS(trackingDomain.String)
	} else if sDomain.Valid && sDomain.String != "" {
		tb = ensureHTTPS("trk." + sDomain.String)
	}

	return pID, fEmail, tb, sendingDomain
}

func buildProofRenderContext(email, trackBase, emailID, unsubURL string) map[string]interface{} {
	rc := make(mailing.RenderContext)

	rc["first_name"] = "Proof"
	rc["last_name"] = "Recipient"
	rc["email"] = email
	rc["full_name"] = "Proof Recipient"

	if parts := strings.SplitN(email, "@", 2); len(parts) == 2 {
		rc["email_local"] = parts[0]
		rc["email_domain"] = parts[1]
	}

	now := time.Now()
	system := map[string]interface{}{
		"date":       now.Format("2006-01-02"),
		"year":       fmt.Sprintf("%d", now.Year()),
		"month_name": now.Format("January"),
	}
	if unsubURL != "" {
		system["unsubscribe_url"] = unsubURL
	} else if trackBase != "" {
		system["unsubscribe_url"] = trackBase + "/track/unsubscribe"
	}
	system["preferences_url"] = "#preferences"
	system["view_in_browser_url"] = "#view-in-browser"
	rc["system"] = system

	rc["campaign"] = map[string]interface{}{
		"name": "Proof Send",
		"id":   proofCampaignID,
	}
	rc["campaignId"] = proofCampaignID
	rc["campaign_name"] = "Proof Send"

	subscriber := map[string]interface{}{
		"email":      email,
		"first_name": "Proof",
		"last_name":  "Recipient",
		"id":         proofSubscriberID,
	}
	rc["subscriber"] = subscriber

	rc["custom"] = make(map[string]interface{})
	rc["engagement"] = map[string]interface{}{
		"score":  float64(100),
		"opens":  0,
		"clicks": 0,
	}

	return rc
}

func NewProofSendHandler(db *sql.DB) *ProofSendHandler {
	trackURL := os.Getenv("TRACKING_URL")
	if trackURL == "" {
		trackURL = "http://localhost:8080"
	}
	trackSecret := os.Getenv("TRACKING_SECRET")
	if trackSecret == "" {
		trackSecret = "ignite-tracking-secret-dev"
	}
	return &ProofSendHandler{
		db:             db,
		sender:         worker.NewProfileBasedSender(db),
		trackingURL:    trackURL,
		trackingSecret: trackSecret,
		orgID:          "00000000-0000-0000-0000-000000000001",
		smartLinks:     NewSmartLinkService(db),
	}
}
