package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/mail"
	"strings"

	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// FBLHandler receives Abuse Reporting Format (ARF) feedback loop reports from
// ISPs and routes complaints to the global suppression hub.
//
// Supports:
//   - ARF multipart/report (RFC 5965) - Yahoo, Outlook, AOL
//   - JSON webhook (SparkPost, Mailgun, SES SNS)
//   - Simple POST with email address
type FBLHandler struct {
	db        *sql.DB
	globalHub *engine.GlobalSuppressionHub
}

func NewFBLHandler(db *sql.DB, hub *engine.GlobalSuppressionHub) *FBLHandler {
	return &FBLHandler{db: db, globalHub: hub}
}

// HandleARFReport processes a multipart/report ARF message (RFC 5965).
// Yahoo/Outlook FBL programs forward complaints as MIME messages to a configured endpoint.
func (h *FBLHandler) HandleARFReport(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	contentType := r.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		http.Error(w, "bad content-type", http.StatusBadRequest)
		return
	}

	var recipient, campaignID, sourceISP string

	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		recipient, campaignID, sourceISP = h.parseARFMultipart(body, params["boundary"])
	case mediaType == "application/json":
		recipient, campaignID, sourceISP = h.parseJSONWebhook(body)
	case mediaType == "message/feedback-report":
		recipient, campaignID, sourceISP = h.parseFeedbackReport(body)
	default:
		recipient = strings.TrimSpace(string(body))
	}

	if recipient == "" {
		http.Error(w, "no recipient found", http.StatusBadRequest)
		return
	}

	log.Printf("[FBL] Complaint received: %s (isp=%s, campaign=%s)", recipient, sourceISP, campaignID)

	if h.globalHub != nil {
		h.globalHub.Suppress(
			r.Context(), recipient,
			"spam_complaint", "fbl_report", sourceISP,
			"", "", "", campaignID,
		)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "processed", "email": recipient})
}

func (h *FBLHandler) parseARFMultipart(body []byte, boundary string) (recipient, campaignID, isp string) {
	if boundary == "" {
		return "", "", ""
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		ct := part.Header.Get("Content-Type")
		partBody, _ := io.ReadAll(part)

		switch {
		case strings.Contains(ct, "message/feedback-report"):
			recipient, campaignID, isp = h.parseFeedbackReport(partBody)
		case strings.Contains(ct, "message/rfc822"):
			msg, err := mail.ReadMessage(bytes.NewReader(partBody))
			if err == nil {
				if to := msg.Header.Get("To"); to != "" && recipient == "" {
					addr, _ := mail.ParseAddress(to)
					if addr != nil {
						recipient = addr.Address
					}
				}
				if xCampaign := msg.Header.Get("X-Campaign-ID"); xCampaign != "" {
					campaignID = xCampaign
				}
			}
		}
	}
	return
}

func (h *FBLHandler) parseFeedbackReport(body []byte) (recipient, campaignID, isp string) {
	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(strings.ToLower(parts[0]))
		val := strings.TrimSpace(parts[1])
		switch key {
		case "original-rcpt-to":
			recipient = val
		case "removal-recipient":
			if recipient == "" {
				recipient = val
			}
		case "reported-domain":
			isp = val
		case "x-campaign-id":
			campaignID = val
		}
	}
	return
}

func (h *FBLHandler) parseJSONWebhook(body []byte) (recipient, campaignID, isp string) {
	var payload struct {
		Email      string `json:"email"`
		Recipient  string `json:"recipient"`
		CampaignID string `json:"campaign_id"`
		ISP        string `json:"isp"`
		Type       string `json:"type"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", ""
	}
	recipient = payload.Email
	if recipient == "" {
		recipient = payload.Recipient
	}
	return recipient, payload.CampaignID, payload.ISP
}
