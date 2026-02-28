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

	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
)

// SparkPostSender sends emails via the SparkPost Transmissions API.
type SparkPostSender struct {
	apiKey  string
	baseURL string
	db      *sql.DB
}

// NewSparkPostSender creates a sender targeting the SparkPost v1 API.
func NewSparkPostSender(apiKey string, db *sql.DB) *SparkPostSender {
	return &SparkPostSender{
		apiKey:  apiKey,
		baseURL: "https://api.sparkpost.com/api/v1",
		db:      db,
	}
}

// Send delivers a single email through SparkPost.
func (s *SparkPostSender) Send(ctx context.Context, msg *EmailMessage) (*SendResult, error) {
	if s.apiKey == "" {
		return nil, fmt.Errorf("SparkPost API key not configured")
	}

	transmission := map[string]interface{}{
		"recipients": []map[string]interface{}{
			{"address": map[string]string{"email": msg.Email}},
		},
		"content": map[string]interface{}{
			"from":    map[string]string{"email": msg.FromEmail, "name": msg.FromName},
			"subject": msg.Subject,
			"html":    msg.HTMLContent,
			"text":    msg.TextContent,
		},
		"metadata": map[string]interface{}{
			"campaign_id":   msg.CampaignID,
			"subscriber_id": msg.SubscriberID,
		},
	}

	jsonData, err := json.Marshal(transmission)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", s.baseURL+"/transmissions", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return &SendResult{
			Success: false,
			Error:   fmt.Errorf("SparkPost error %d: %s", resp.StatusCode, string(body)),
		}, nil
	}

	var result struct {
		Results struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	json.Unmarshal(body, &result)

	log.Printf("[SparkPost] Sent to %s (id: %s)", logger.RedactEmail(msg.Email), result.Results.ID)

	return &SendResult{
		Success:   true,
		MessageID: result.Results.ID,
		ESPType:   "sparkpost",
		SentAt:    time.Now(),
	}, nil
}
