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

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

type ProofSendHandler struct {
	db             *sql.DB
	sender         worker.ESPSender
	trackingURL    string
	trackingSecret string
	orgID          string
}

type proofSendRequest struct {
	Proofs         []proofItem `json:"proofs"`
	RecipientEmail string      `json:"recipient_email"`
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

	var webProperty string
	err := h.db.QueryRowContext(ctx,
		`SELECT COALESCE(web_property,'') FROM mailing_offers WHERE id = $1`, offerID,
	).Scan(&webProperty)
	if err != nil {
		http.Error(w, `{"error":"offer not found"}`, http.StatusNotFound)
		return
	}

	profileID, fromEmail, trackBase := h.resolveSendingProfile(ctx, webProperty)
	if profileID == "" {
		http.Error(w, `{"error":"no active sending profile for this brand"}`, http.StatusUnprocessableEntity)
		return
	}

	ts := mailing.NewTemplateService()

	results := make([]proofResult, 0, len(req.Proofs))
	for i, p := range req.Proofs {
		res := h.sendOneProof(ctx, ts, offerID, profileID, fromEmail, trackBase, req.RecipientEmail, p, i)
		results = append(results, res)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"results": results,
	})
}

func (h *ProofSendHandler) sendOneProof(
	ctx context.Context,
	ts *mailing.TemplateService,
	offerID, profileID, fromEmail, trackBase, recipientEmail string,
	p proofItem,
	index int,
) proofResult {
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

	rc := buildProofRenderContext(recipientEmail, trackBase, emailID)

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

		unsubURL := worker.GenerateUnsubscribeURL(h.orgID, proofCampaignID, proofSubscriberID, trackBase, h.trackingSecret)
		renderedHTML = strings.ReplaceAll(renderedHTML, "{{ system.unsubscribe_url }}", unsubURL)
		renderedHTML = strings.ReplaceAll(renderedHTML, "{{system.unsubscribe_url}}", unsubURL)
	}

	headers := map[string]string{
		"X-Proof-Send": "true",
		"X-Offer-ID":   offerID,
	}
	if trackBase != "" && h.trackingSecret != "" {
		unsubURL := worker.GenerateUnsubscribeURL(h.orgID, proofCampaignID, proofSubscriberID, trackBase, h.trackingSecret)
		headers["List-Unsubscribe"] = "<" + unsubURL + ">"
		headers["List-Unsubscribe-Post"] = "List-Unsubscribe=One-Click"
	}

	msg := &worker.EmailMessage{
		ID:           emailID,
		CampaignID:   proofCampaignID,
		SubscriberID: proofSubscriberID,
		Email:        recipientEmail,
		FromName:     fromName,
		FromEmail:    fromEmail,
		Subject:      renderedSubject,
		HTMLContent:  renderedHTML,
		ProfileID:    profileID,
		RecipientISP: "proof",
		Headers:      headers,
	}

	result, sendErr := h.sender.Send(ctx, msg)
	if sendErr != nil {
		log.Printf("[proof-send] send error for creative %s: %v", p.CreativeID, sendErr)
		return proofResult{CreativeID: p.CreativeID, Status: "error", Error: sendErr.Error()}
	}
	if result != nil && !result.Success {
		errMsg := "send failed"
		if result.Error != nil {
			errMsg = result.Error.Error()
		}
		return proofResult{CreativeID: p.CreativeID, Status: "error", Error: errMsg}
	}

	messageID := ""
	if result != nil {
		messageID = result.MessageID
	}
	log.Printf("[proof-send] sent proof #%d creative=%s to=%s messageID=%s", index+1, p.CreativeID, recipientEmail, messageID)
	return proofResult{CreativeID: p.CreativeID, Status: "sent", MessageID: messageID}
}

func (h *ProofSendHandler) resolveSendingProfile(ctx context.Context, webProperty string) (profileID, fromEmail, trackBase string) {
	var sendingDomain string
	if webProperty != "" {
		if kit, ok := GetBrandKit(webProperty); ok {
			sendingDomain = kit.SendingDomain
		}
	}
	if sendingDomain == "" {
		return "", "", ""
	}

	var pID, fEmail string
	var trackingDomain, sDomain sql.NullString
	err := h.db.QueryRowContext(ctx,
		`SELECT id::text, COALESCE(from_email,''), COALESCE(tracking_domain,''), COALESCE(sending_domain,'')
		 FROM mailing_sending_profiles
		 WHERE sending_domain = $1 AND vendor_type = 'pmta' AND status = 'active'
		 ORDER BY created_at DESC LIMIT 1`,
		sendingDomain,
	).Scan(&pID, &fEmail, &trackingDomain, &sDomain)
	if err != nil {
		log.Printf("[proof-send] no sending profile for domain %s: %v", sendingDomain, err)
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

func buildProofRenderContext(email, trackBase, emailID string) map[string]interface{} {
	rc := make(mailing.RenderContext)

	rc["first_name"] = "Proof"
	rc["last_name"] = "Recipient"
	rc["email"] = email
	rc["full_name"] = "Proof Recipient"

	if parts := strings.SplitN(email, "@", 2); len(parts) == 2 {
		rc["email_local"] = parts[0]
		rc["email_domain"] = parts[1]
	}

	system := map[string]interface{}{
		"date":       "2026-03-18",
		"year":       "2026",
		"month_name": "March",
	}
	if trackBase != "" {
		unsubURL := fmt.Sprintf("%s/track/unsubscribe/proof/%s", trackBase, emailID)
		system["unsubscribe_url"] = unsubURL
		system["preferences_url"] = "#preferences"
		system["view_in_browser_url"] = "#view-in-browser"
	}
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

	custom := make(map[string]interface{})
	rc["custom"] = custom

	engagement := map[string]interface{}{
		"score":  float64(100),
		"opens":  0,
		"clicks": 0,
	}
	rc["engagement"] = engagement

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
	}
}
