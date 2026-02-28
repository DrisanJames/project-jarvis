package worker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
)

// SendGridSender sends emails via the SendGrid v3 Mail Send API.
// Supports both single and batch sends (up to 1000 personalizations per call).
type SendGridSender struct {
	apiKey   string
	baseURL  string
	db       *sql.DB
	maxBatch int
	client   *http.Client
}

// NewSendGridSender creates a SendGrid sender.
func NewSendGridSender(apiKey string, db *sql.DB) *SendGridSender {
	return &SendGridSender{
		apiKey:   apiKey,
		baseURL:  "https://api.sendgrid.com/v3",
		db:       db,
		maxBatch: 1000,
		client:   &http.Client{Timeout: 60 * time.Second},
	}
}

// MaxBatchSize returns the maximum personalizations per batch (1000 for SendGrid).
func (s *SendGridSender) MaxBatchSize() int { return s.maxBatch }

// Send delivers a single email through SendGrid.
func (s *SendGridSender) Send(ctx context.Context, msg *EmailMessage) (*SendResult, error) {
	if s.apiKey == "" {
		return nil, fmt.Errorf("SendGrid API key not configured")
	}

	payload := map[string]interface{}{
		"personalizations": []map[string]interface{}{
			{
				"to":          []map[string]string{{"email": msg.Email}},
				"custom_args": map[string]string{"campaign_id": msg.CampaignID, "subscriber_id": msg.SubscriberID},
			},
		},
		"from":    map[string]string{"email": msg.FromEmail, "name": msg.FromName},
		"subject": msg.Subject,
		"content": []map[string]string{{"type": "text/html", "value": msg.HTMLContent}},
		"tracking_settings": map[string]interface{}{
			"click_tracking": map[string]bool{"enable": true},
			"open_tracking":  map[string]bool{"enable": true},
		},
	}

	if msg.TextContent != "" {
		payload["content"] = []map[string]string{
			{"type": "text/plain", "value": msg.TextContent},
			{"type": "text/html", "value": msg.HTMLContent},
		}
	}
	if msg.ReplyTo != "" {
		payload["reply_to"] = map[string]string{"email": msg.ReplyTo}
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", s.baseURL+"/mail/send", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return &SendResult{Success: false, Error: fmt.Errorf("SendGrid error %d: %s", resp.StatusCode, string(body)), ESPType: "sendgrid"}, nil
	}

	messageID := resp.Header.Get("X-Message-Id")
	if messageID == "" {
		messageID = uuid.New().String()
	}

	log.Printf("[SendGrid] Sent to %s (id: %s)", logger.RedactEmail(msg.Email), messageID)
	return &SendResult{Success: true, MessageID: messageID, ESPType: "sendgrid", SentAt: time.Now()}, nil
}

// SendBatch sends multiple emails using SendGrid's personalizations array.
func (s *SendGridSender) SendBatch(ctx context.Context, messages []EmailMessage) (*BatchSendResult, error) {
	if s.apiKey == "" {
		return nil, fmt.Errorf("SendGrid API key not configured")
	}
	if len(messages) == 0 {
		return &BatchSendResult{}, nil
	}
	if len(messages) > s.maxBatch {
		return nil, fmt.Errorf("batch size %d exceeds SendGrid max of %d", len(messages), s.maxBatch)
	}

	personalizations := make([]map[string]interface{}, len(messages))
	for i, msg := range messages {
		p := map[string]interface{}{
			"to":          []map[string]string{{"email": msg.Email}},
			"custom_args": map[string]string{"campaign_id": msg.CampaignID, "subscriber_id": msg.SubscriberID, "message_id": msg.ID},
		}
		if msg.Metadata != nil {
			subs := make(map[string]string)
			for k, v := range msg.Metadata {
				subs[fmt.Sprintf("{{%s}}", k)] = fmt.Sprintf("%v", v)
			}
			if len(subs) > 0 {
				p["substitutions"] = subs
			}
		}
		personalizations[i] = p
	}

	tpl := messages[0]
	payload := map[string]interface{}{
		"personalizations": personalizations,
		"from":             map[string]string{"email": tpl.FromEmail, "name": tpl.FromName},
		"subject":          tpl.Subject,
		"content":          []map[string]string{{"type": "text/html", "value": tpl.HTMLContent}},
		"tracking_settings": map[string]interface{}{
			"click_tracking": map[string]bool{"enable": true},
			"open_tracking":  map[string]bool{"enable": true},
		},
	}
	if tpl.TextContent != "" {
		payload["content"] = []map[string]string{
			{"type": "text/plain", "value": tpl.TextContent},
			{"type": "text/html", "value": tpl.HTMLContent},
		}
	}
	if tpl.ReplyTo != "" {
		payload["reply_to"] = map[string]string{"email": tpl.ReplyTo}
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", s.baseURL+"/mail/send", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send batch: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	messageID := resp.Header.Get("X-Message-Id")
	if messageID == "" {
		messageID = uuid.New().String()
	}

	result := &BatchSendResult{TransmissionID: messageID, Results: make([]SendResult, len(messages))}
	if resp.StatusCode >= 400 {
		errMsg := fmt.Errorf("SendGrid batch error %d: %s", resp.StatusCode, string(body))
		result.Rejected = len(messages)
		result.Errors = append(result.Errors, errMsg)
		for i := range messages {
			result.Results[i] = SendResult{Success: false, Error: errMsg, ESPType: "sendgrid"}
		}
		return result, nil
	}

	result.Accepted = len(messages)
	for i := range messages {
		result.Results[i] = SendResult{Success: true, MessageID: fmt.Sprintf("%s-%d", messageID, i), ESPType: "sendgrid", SentAt: time.Now()}
	}
	log.Printf("[SendGrid] Batch sent %d emails (id: %s)", len(messages), messageID)
	return result, nil
}
