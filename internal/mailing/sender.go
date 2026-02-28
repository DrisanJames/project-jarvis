package mailing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sender handles email sending through various ESPs
type Sender struct {
	store        *Store
	sparkpostKey string
	sparkpostURL string
	httpClient   *http.Client
}

// ServerSendResult represents the result of sending via a delivery server
type ServerSendResult struct {
	Success    bool
	MessageID  string
	Error      error
	ServerID   uuid.UUID
	ServerType string
}

// NewSender creates a new sender
func NewSender(store *Store, sparkpostKey string) *Sender {
	return &Sender{
		store:        store,
		sparkpostKey: sparkpostKey,
		sparkpostURL: "https://api.sparkpost.com/api/v1/transmissions",
		httpClient:   &http.Client{Timeout: 30 * time.Second},
	}
}

// SendEmail sends an email through the appropriate ESP
func (s *Sender) SendEmail(ctx context.Context, orgID uuid.UUID, item *QueueItem, sub *Subscriber) (*ServerSendResult, error) {
	server, err := s.store.GetAvailableServer(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("get delivery server: %w", err)
	}
	if server == nil {
		return nil, fmt.Errorf("no available delivery server")
	}

	htmlContent := s.personalizeContent(item.HTMLContent, sub)
	plainContent := s.personalizeContent(item.PlainContent, sub)
	subject := s.personalizeContent(item.Subject, sub)

	var result *ServerSendResult
	switch server.ServerType {
	case ServerSparkPost:
		result, err = s.sendViaSparkPost(ctx, server, sub.Email, subject, htmlContent, plainContent)
	case ServerSES:
		result, err = s.sendViaSES(ctx, server, sub.Email, subject, htmlContent, plainContent)
	default:
		return nil, fmt.Errorf("unsupported server type: %s", server.ServerType)
	}

	if err != nil {
		return &ServerSendResult{Success: false, Error: err, ServerID: server.ID, ServerType: server.ServerType}, err
	}

	s.store.IncrementServerUsage(ctx, server.ID)
	result.ServerID = server.ID
	result.ServerType = server.ServerType
	return result, nil
}

// sendViaSparkPost sends via SparkPost API
func (s *Sender) sendViaSparkPost(ctx context.Context, server *DeliveryServer, to, subject, html, plain string) (*ServerSendResult, error) {
	payload := map[string]interface{}{
		"recipients": []map[string]interface{}{
			{"address": map[string]string{"email": to}},
		},
		"content": map[string]interface{}{
			"from":    map[string]string{"email": server.Name + "@mail.ignite.com"},
			"subject": subject,
			"html":    html,
			"text":    plain,
		},
		"options": map[string]interface{}{
			"open_tracking":  true,
			"click_tracking": true,
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", s.sparkpostURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", s.sparkpostKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("sparkpost error: status %d", resp.StatusCode)
	}

	var result struct {
		Results struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	return &ServerSendResult{Success: true, MessageID: result.Results.ID}, nil
}

// sendViaSES sends via AWS SES (placeholder)
func (s *Sender) sendViaSES(ctx context.Context, server *DeliveryServer, to, subject, html, plain string) (*ServerSendResult, error) {
	// SES implementation would go here
	return &ServerSendResult{Success: true, MessageID: uuid.New().String()}, nil
}

// personalizeContent replaces personalization tags
func (s *Sender) personalizeContent(content string, sub *Subscriber) string {
	if content == "" {
		return content
	}
	replacements := map[string]string{
		"{{email}}":      sub.Email,
		"{{first_name}}": sub.FirstName,
		"{{last_name}}":  sub.LastName,
		"{{EMAIL}}":      sub.Email,
		"{{FIRST_NAME}}": sub.FirstName,
		"{{LAST_NAME}}":  sub.LastName,
		"[EMAIL]":        sub.Email,
		"[FIRST_NAME]":   sub.FirstName,
		"[LAST_NAME]":    sub.LastName,
	}
	result := content
	for tag, value := range replacements {
		result = strings.ReplaceAll(result, tag, value)
	}
	return result
}

// CampaignSender handles sending campaigns
type CampaignSender struct {
	store  *Store
	sender *Sender
}

// NewCampaignSender creates a new campaign sender
func NewCampaignSender(store *Store, sender *Sender) *CampaignSender {
	return &CampaignSender{store: store, sender: sender}
}

// PrepareCampaign prepares a campaign for sending
func (cs *CampaignSender) PrepareCampaign(ctx context.Context, campaign *Campaign) error {
	if campaign.ListID == nil {
		return fmt.Errorf("campaign has no list assigned")
	}

	subscribers, err := cs.store.GetActiveSubscribersForCampaign(ctx, *campaign.ListID)
	if err != nil {
		return fmt.Errorf("get subscribers: %w", err)
	}
	if len(subscribers) == 0 {
		return fmt.Errorf("no subscribers to send to")
	}

	items := make([]*QueueItem, 0, len(subscribers))
	baseTime := time.Now()
	if campaign.SendAt != nil && campaign.SendAt.After(baseTime) {
		baseTime = *campaign.SendAt
	}

	for i, sub := range subscribers {
		scheduledAt := baseTime
		if campaign.AISendTimeOptimization && sub.OptimalSendHourUTC != nil {
			scheduledAt = cs.calculateOptimalSendTime(baseTime, *sub.OptimalSendHourUTC)
		}

		items = append(items, &QueueItem{
			CampaignID:   campaign.ID,
			SubscriberID: sub.ID,
			Subject:      campaign.Subject,
			HTMLContent:  campaign.HTMLContent,
			PlainContent: campaign.PlainContent,
			ScheduledAt:  scheduledAt,
			Priority:     cs.calculatePriority(sub),
			Status:       StatusQueued,
		})

		if len(items) >= 1000 {
			if err := cs.store.EnqueueEmails(ctx, items); err != nil {
				return fmt.Errorf("enqueue batch %d: %w", i/1000, err)
			}
			items = items[:0]
		}
	}

	if len(items) > 0 {
		if err := cs.store.EnqueueEmails(ctx, items); err != nil {
			return fmt.Errorf("enqueue final batch: %w", err)
		}
	}

	campaign.TotalRecipients = len(subscribers)
	return cs.store.UpdateCampaignStatus(ctx, campaign.ID, StatusSending)
}

func (cs *CampaignSender) calculateOptimalSendTime(base time.Time, optimalHour int) time.Time {
	result := time.Date(base.Year(), base.Month(), base.Day(), optimalHour, 0, 0, 0, time.UTC)
	if result.Before(base) {
		result = result.Add(24 * time.Hour)
	}
	return result
}

func (cs *CampaignSender) calculatePriority(sub *Subscriber) int {
	if sub.EngagementScore >= 80 {
		return 10
	} else if sub.EngagementScore >= 60 {
		return 8
	} else if sub.EngagementScore >= 40 {
		return 6
	} else if sub.EngagementScore >= 20 {
		return 4
	}
	return 2
}

// ProcessQueue processes the send queue
func (cs *CampaignSender) ProcessQueue(ctx context.Context, batchSize int) (int, error) {
	items, err := cs.store.GetQueuedEmails(ctx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("get queued emails: %w", err)
	}

	sent := 0
	for _, item := range items {
		sub, err := cs.store.GetSubscriber(ctx, item.SubscriberID)
		if err != nil || sub == nil {
			cs.store.UpdateQueueItemStatus(ctx, item.ID, StatusFailed, "", "subscriber not found", nil)
			continue
		}

		result, err := cs.sender.SendEmail(ctx, sub.OrganizationID, item, sub)
		if err != nil {
			cs.store.UpdateQueueItemStatus(ctx, item.ID, StatusFailed, "", err.Error(), nil)
			continue
		}

		serverID := &result.ServerID
		cs.store.UpdateQueueItemStatus(ctx, item.ID, StatusSent, result.MessageID, "", serverID)
		cs.store.UpdateCampaignStats(ctx, item.CampaignID, "sent_count", 1)
		cs.store.UpdateSubscriberEngagement(ctx, item.SubscriberID, EventSent)
		cs.store.RecordTrackingEvent(ctx, &TrackingEvent{
			OrganizationID: sub.OrganizationID,
			CampaignID:     &item.CampaignID,
			SubscriberID:   &item.SubscriberID,
			EmailID:        &item.ID,
			EventType:      EventSent,
		})
		sent++
	}
	return sent, nil
}
