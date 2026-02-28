package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MailgunSender sends emails via the Mailgun Messages API.
// Supports both single and batch sends (up to 1000 recipients per call).
type MailgunSender struct {
	apiKey   string
	domain   string
	baseURL  string
	db       *sql.DB
	maxBatch int
	client   *http.Client
}

// NewMailgunSender creates a Mailgun sender targeting the given domain.
func NewMailgunSender(apiKey, domain string, db *sql.DB) *MailgunSender {
	return &MailgunSender{
		apiKey:   apiKey,
		domain:   domain,
		baseURL:  "https://api.mailgun.net/v3",
		db:       db,
		maxBatch: 1000,
		client:   &http.Client{Timeout: 60 * time.Second},
	}
}

// MaxBatchSize returns the maximum recipients per batch (1000 for Mailgun).
func (s *MailgunSender) MaxBatchSize() int { return s.maxBatch }

// Send delivers a single email through Mailgun.
func (s *MailgunSender) Send(ctx context.Context, msg *EmailMessage) (*SendResult, error) {
	if s.apiKey == "" {
		return nil, fmt.Errorf("Mailgun API key not configured")
	}

	form := url.Values{}
	form.Add("from", fmt.Sprintf("%s <%s>", msg.FromName, msg.FromEmail))
	form.Add("to", msg.Email)
	form.Add("subject", msg.Subject)
	form.Add("html", msg.HTMLContent)
	if msg.TextContent != "" {
		form.Add("text", msg.TextContent)
	}
	if msg.ReplyTo != "" {
		form.Add("h:Reply-To", msg.ReplyTo)
	}
	form.Add("v:campaign_id", msg.CampaignID)
	form.Add("v:subscriber_id", msg.SubscriberID)

	endpoint := fmt.Sprintf("%s/%s/messages", s.baseURL, s.domain)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("api", s.apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return &SendResult{Success: false, Error: fmt.Errorf("Mailgun error %d: %s", resp.StatusCode, string(body)), ESPType: "mailgun"}, nil
	}

	var result struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	}
	json.Unmarshal(body, &result)
	messageID := strings.Trim(result.ID, "<>")
	log.Printf("[Mailgun] Sent to %s (id: %s)", msg.Email, messageID)

	return &SendResult{Success: true, MessageID: messageID, ESPType: "mailgun", SentAt: time.Now()}, nil
}

// SendBatch sends multiple emails using Mailgun's recipient-variables feature.
func (s *MailgunSender) SendBatch(ctx context.Context, messages []EmailMessage) (*BatchSendResult, error) {
	if s.apiKey == "" {
		return nil, fmt.Errorf("Mailgun API key not configured")
	}
	if len(messages) == 0 {
		return &BatchSendResult{}, nil
	}
	if len(messages) > s.maxBatch {
		return nil, fmt.Errorf("batch size %d exceeds Mailgun max of %d", len(messages), s.maxBatch)
	}

	recipients := make([]string, len(messages))
	recipientVars := make(map[string]map[string]interface{})
	for i, msg := range messages {
		recipients[i] = msg.Email
		vars := map[string]interface{}{
			"campaign_id": msg.CampaignID, "subscriber_id": msg.SubscriberID, "id": msg.ID,
		}
		if msg.Metadata != nil {
			for k, v := range msg.Metadata {
				vars[k] = v
			}
		}
		recipientVars[msg.Email] = vars
	}

	recipientVarsJSON, err := json.Marshal(recipientVars)
	if err != nil {
		return nil, fmt.Errorf("marshal recipient-variables: %w", err)
	}

	tpl := messages[0]
	form := url.Values{}
	form.Add("from", fmt.Sprintf("%s <%s>", tpl.FromName, tpl.FromEmail))
	form.Add("to", strings.Join(recipients, ","))
	form.Add("subject", tpl.Subject)
	form.Add("html", tpl.HTMLContent)
	form.Add("recipient-variables", string(recipientVarsJSON))
	if tpl.TextContent != "" {
		form.Add("text", tpl.TextContent)
	}
	if tpl.ReplyTo != "" {
		form.Add("h:Reply-To", tpl.ReplyTo)
	}
	form.Add("o:tracking", "yes")
	form.Add("o:tracking-clicks", "yes")
	form.Add("o:tracking-opens", "yes")

	endpoint := fmt.Sprintf("%s/%s/messages", s.baseURL, s.domain)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("api", s.apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send batch: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var mgResp struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &mgResp)
	txnID := strings.Trim(mgResp.ID, "<>")

	result := &BatchSendResult{TransmissionID: txnID, Results: make([]SendResult, len(messages))}
	if resp.StatusCode >= 400 {
		errMsg := fmt.Errorf("Mailgun batch error %d: %s", resp.StatusCode, string(body))
		result.Rejected = len(messages)
		result.Errors = append(result.Errors, errMsg)
		for i := range messages {
			result.Results[i] = SendResult{Success: false, Error: errMsg, ESPType: "mailgun"}
		}
		return result, nil
	}

	result.Accepted = len(messages)
	for i, msg := range messages {
		result.Results[i] = SendResult{Success: true, MessageID: fmt.Sprintf("%s-%s", txnID, msg.SubscriberID), ESPType: "mailgun", SentAt: time.Now()}
	}
	log.Printf("[Mailgun] Batch sent %d emails (id: %s)", len(messages), txnID)
	return result, nil
}
