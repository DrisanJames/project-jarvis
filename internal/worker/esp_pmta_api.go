package worker

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/quotedprintable"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PMTAAPISender sends emails via PMTA's HTTP injection API (port 19000).
// Bypasses SMTP port blocking between AWS ECS and OVH.
type PMTAAPISender struct {
	apiEndpoint string
	db          *sql.DB
	client      *http.Client
	ipPool      *vmtaPool
}

// NewPMTAAPISender creates a PMTA API sender.
func NewPMTAAPISender(apiEndpoint string, db *sql.DB) *PMTAAPISender {
	return &PMTAAPISender{
		apiEndpoint: apiEndpoint,
		db:          db,
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
		ipPool: newVMTAPool(db),
	}
}

// Send delivers a single email through the PMTA HTTP injection API.
func (s *PMTAAPISender) Send(ctx context.Context, msg *EmailMessage) (*SendResult, error) {
	if s.apiEndpoint == "" {
		return nil, fmt.Errorf("PMTA API endpoint not configured")
	}

	injectURL := strings.TrimRight(s.apiEndpoint, "/") + "/api/inject/v1"
	messageID := fmt.Sprintf("%s@pmta-api", uuid.New().String())

	// Build RFC822 message
	boundary := fmt.Sprintf("=_%s", uuid.New().String()[:16])
	var rfc822 bytes.Buffer

	rfc822.WriteString(fmt.Sprintf("From: %s <%s>\r\n", msg.FromName, msg.FromEmail))
	rfc822.WriteString(fmt.Sprintf("To: %s\r\n", msg.Email))
	rfc822.WriteString(fmt.Sprintf("Subject: %s\r\n", msg.Subject))
	rfc822.WriteString(fmt.Sprintf("Message-ID: <%s>\r\n", messageID))
	rfc822.WriteString("MIME-Version: 1.0\r\n")

	if msg.ReplyTo != "" {
		rfc822.WriteString(fmt.Sprintf("Reply-To: %s\r\n", msg.ReplyTo))
	}

	if msg.CampaignID != "" {
		rfc822.WriteString(fmt.Sprintf("X-Campaign-ID: %s\r\n", msg.CampaignID))
	}
	if msg.SubscriberID != "" {
		rfc822.WriteString(fmt.Sprintf("X-Subscriber-ID: %s\r\n", msg.SubscriberID))
	}
	if msg.ID != "" {
		rfc822.WriteString(fmt.Sprintf("X-Message-ID: %s\r\n", msg.ID))
	}

	for k, v := range msg.Headers {
		rfc822.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
	}

	rfc822.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n", boundary))
	rfc822.WriteString("\r\n")

	if msg.TextContent != "" {
		rfc822.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		rfc822.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
		rfc822.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		qpText := quotedprintableEncode(msg.TextContent)
		rfc822.WriteString(qpText)
		rfc822.WriteString("\r\n")
	}
	rfc822.WriteString(fmt.Sprintf("--%s\r\n", boundary))
	rfc822.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	rfc822.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	qpHTML := quotedprintableEncode(msg.HTMLContent)
	rfc822.WriteString(qpHTML)
	rfc822.WriteString("\r\n")
	rfc822.WriteString(fmt.Sprintf("--%s--\r\n", boundary))

	payload := map[string]interface{}{
		"envelope_sender": msg.FromEmail,
		"recipients":      []map[string]string{{"email": msg.Email}},
		"content":         rfc822.String(),
	}

	// Resolve VMTA from IP pool (same rotation as SMTP sender)
	vmtaResolved := false
	if s.ipPool != nil && msg.ProfileID != "" {
		s.ipPool.refresh(ctx, msg.ProfileID)
		if ip, err := s.ipPool.next(); err == nil {
			payload["vmta"] = ip.Hostname
			vmtaResolved = true
			log.Printf("[PMTA-API] Routing %s via VMTA %s", msg.Email, ip.Hostname)
		}
	}
	if !vmtaResolved {
		payload["vmta"] = "default-pool"
		log.Printf("[PMTA-API] Routing %s via default-pool (no IP pool data)", msg.Email)
	}
	// Header override takes precedence if explicitly set
	if vmta, ok := msg.Headers["X-Virtual-MTA"]; ok && vmta != "" {
		payload["vmta"] = vmta
	}

	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, fmt.Errorf("marshal PMTA payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", injectURL, &body)
	if err != nil {
		return nil, fmt.Errorf("create PMTA request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("PMTA API request to %s: %w", injectURL, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("PMTA API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	log.Printf("[PMTA-API] Sent to %s via %s (id: %s, status: %d)", msg.Email, injectURL, messageID, resp.StatusCode)
	return &SendResult{Success: true, MessageID: messageID, ESPType: "pmta-api", SentAt: time.Now()}, nil
}

func quotedprintableEncode(s string) string {
	var buf bytes.Buffer
	w := quotedprintable.NewWriter(&buf)
	w.Write([]byte(s))
	w.Close()
	return buf.String()
}
