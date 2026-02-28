package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

// WebhookReceiver handles incoming ESP webhook events
// Designed for high-volume ingestion (10M+ events/day)
// Events are written to a staging table and processed asynchronously
type WebhookReceiver struct {
	db         *sql.DB
	insertStmt *sql.Stmt

	// Stats
	eventsReceived  int64
	eventsProcessed int64
	errors          int64
}

// NewWebhookReceiver creates a new webhook receiver
func NewWebhookReceiver(db *sql.DB) (*WebhookReceiver, error) {
	stmt, err := db.Prepare(`
		INSERT INTO mailing_webhook_events 
		(esp_type, event_type, message_id, payload, event_timestamp)
		VALUES ($1, $2, $3, $4, $5)
	`)
	if err != nil {
		return nil, err
	}
	return &WebhookReceiver{db: db, insertStmt: stmt}, nil
}

// SparkPostEvent represents a SparkPost webhook event
type SparkPostEvent struct {
	MSysShortMessage struct {
		Type      string `json:"type"`
		MessageID string `json:"message_id"`
		Timestamp string `json:"timestamp"`
	} `json:"msys"`
}

// SESEvent represents an AWS SES notification event
type SESEvent struct {
	EventType string `json:"eventType"`
	Mail      struct {
		MessageID string `json:"messageId"`
	} `json:"mail"`
	Timestamp time.Time `json:"timestamp"`
}

// SNSMessage represents an AWS SNS notification wrapper
type SNSMessage struct {
	Type             string `json:"Type"`
	SubscribeURL     string `json:"SubscribeURL"`
	Message          string `json:"Message"`
	MessageId        string `json:"MessageId"`
	TopicArn         string `json:"TopicArn"`
}

// MailgunEvent represents a Mailgun webhook event
type MailgunEvent struct {
	EventData struct {
		Event     string `json:"event"`
		MessageID string `json:"message-id"`
		Timestamp float64 `json:"timestamp"`
	} `json:"event-data"`
}

// SendGridEvent represents a SendGrid webhook event
type SendGridEvent struct {
	Event       string `json:"event"`
	SGMessageID string `json:"sg_message_id"`
	Timestamp   int64  `json:"timestamp"`
}

// HandleSparkPostWebhook processes SparkPost webhook batches
func (w *WebhookReceiver) HandleSparkPostWebhook(rw http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(rw, "Failed to read body", http.StatusBadRequest)
		return
	}

	var events []map[string]interface{}
	if err := json.Unmarshal(body, &events); err != nil {
		http.Error(rw, "Invalid JSON", http.StatusBadRequest)
		return
	}

	for _, event := range events {
		// SparkPost wraps events in msys object
		msys, ok := event["msys"].(map[string]interface{})
		if !ok {
			continue
		}

		// Get the first key which is the event type container
		for eventCategory, eventData := range msys {
			data, ok := eventData.(map[string]interface{})
			if !ok {
				continue
			}

			eventType := ""
			switch eventCategory {
			case "message_event":
				eventType, _ = data["type"].(string)
			case "track_event":
				eventType, _ = data["type"].(string)
			case "gen_event":
				eventType, _ = data["type"].(string)
			case "unsubscribe_event":
				eventType = "unsubscribe"
			}

			messageID, _ := data["message_id"].(string)
			timestampStr, _ := data["timestamp"].(string)
			timestamp, _ := time.Parse(time.RFC3339, timestampStr)
			if timestamp.IsZero() {
				timestamp = time.Now()
			}

			payload, _ := json.Marshal(data)

			_, err := w.insertStmt.Exec("sparkpost", eventType, messageID, payload, timestamp)
			if err != nil {
				atomic.AddInt64(&w.errors, 1)
				log.Printf("[WebhookReceiver] SparkPost insert error: %v", err)
			} else {
				atomic.AddInt64(&w.eventsReceived, 1)
			}
		}
	}

	rw.WriteHeader(http.StatusOK)
}

// HandleSESWebhook processes AWS SES/SNS webhook events
func (w *WebhookReceiver) HandleSESWebhook(rw http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(rw, "Failed to read body", http.StatusBadRequest)
		return
	}

	var snsMessage SNSMessage
	if err := json.Unmarshal(body, &snsMessage); err != nil {
		http.Error(rw, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Handle subscription confirmation
	if snsMessage.Type == "SubscriptionConfirmation" {
		log.Printf("[WebhookReceiver] SES subscription confirmation received, confirming...")
		resp, err := http.Get(snsMessage.SubscribeURL)
		if err != nil {
			log.Printf("[WebhookReceiver] Failed to confirm subscription: %v", err)
		} else {
			resp.Body.Close()
			log.Printf("[WebhookReceiver] SES subscription confirmed")
		}
		rw.WriteHeader(http.StatusOK)
		return
	}

	// Parse the actual SES event from the SNS message
	var sesNotification struct {
		NotificationType string    `json:"notificationType"`
		Mail             struct {
			MessageID string `json:"messageId"`
		} `json:"mail"`
		Bounce   *struct{} `json:"bounce,omitempty"`
		Complaint *struct{} `json:"complaint,omitempty"`
		Delivery *struct{} `json:"delivery,omitempty"`
	}

	if err := json.Unmarshal([]byte(snsMessage.Message), &sesNotification); err != nil {
		log.Printf("[WebhookReceiver] Failed to parse SES notification: %v", err)
		rw.WriteHeader(http.StatusOK) // Still return 200 to prevent retries
		return
	}

	eventType := sesNotification.NotificationType
	messageID := sesNotification.Mail.MessageID

	_, err = w.insertStmt.Exec("ses", eventType, messageID, snsMessage.Message, time.Now())
	if err != nil {
		atomic.AddInt64(&w.errors, 1)
		log.Printf("[WebhookReceiver] SES insert error: %v", err)
	} else {
		atomic.AddInt64(&w.eventsReceived, 1)
	}

	rw.WriteHeader(http.StatusOK)
}

// HandleMailgunWebhook processes Mailgun webhook events
func (w *WebhookReceiver) HandleMailgunWebhook(rw http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(rw, "Failed to read body", http.StatusBadRequest)
		return
	}

	var event MailgunEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(rw, "Invalid JSON", http.StatusBadRequest)
		return
	}

	timestamp := time.Unix(int64(event.EventData.Timestamp), 0)

	_, err = w.insertStmt.Exec(
		"mailgun",
		event.EventData.Event,
		event.EventData.MessageID,
		body,
		timestamp,
	)
	if err != nil {
		atomic.AddInt64(&w.errors, 1)
	} else {
		atomic.AddInt64(&w.eventsReceived, 1)
	}

	rw.WriteHeader(http.StatusOK)
}

// HandleSendGridWebhook processes SendGrid webhook events
func (w *WebhookReceiver) HandleSendGridWebhook(rw http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(rw, "Failed to read body", http.StatusBadRequest)
		return
	}

	var events []SendGridEvent
	if err := json.Unmarshal(body, &events); err != nil {
		http.Error(rw, "Invalid JSON", http.StatusBadRequest)
		return
	}

	for _, event := range events {
		timestamp := time.Unix(event.Timestamp, 0)
		payload, _ := json.Marshal(event)

		_, err := w.insertStmt.Exec("sendgrid", event.Event, event.SGMessageID, payload, timestamp)
		if err != nil {
			atomic.AddInt64(&w.errors, 1)
		} else {
			atomic.AddInt64(&w.eventsReceived, 1)
		}
	}

	rw.WriteHeader(http.StatusOK)
}

// Stats returns current statistics
func (w *WebhookReceiver) Stats() map[string]int64 {
	return map[string]int64{
		"events_received":  atomic.LoadInt64(&w.eventsReceived),
		"events_processed": atomic.LoadInt64(&w.eventsProcessed),
		"errors":           atomic.LoadInt64(&w.errors),
	}
}

// Close closes prepared statements
func (w *WebhookReceiver) Close() error {
	return w.insertStmt.Close()
}

// ============================================================================
// Event Aggregator Worker
// ============================================================================

// EventAggregator processes webhook events and updates the message log
type EventAggregator struct {
	db       *sql.DB
	interval time.Duration
	running  bool
	stopCh   chan struct{}
}

// NewEventAggregator creates a new event aggregator
func NewEventAggregator(db *sql.DB, interval time.Duration) *EventAggregator {
	if interval == 0 {
		interval = 30 * time.Second
	}
	return &EventAggregator{
		db:       db,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

// Start begins the aggregation loop
func (a *EventAggregator) Start(ctx context.Context) {
	a.running = true
	log.Printf("[EventAggregator] Starting with interval %v", a.interval)

	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			a.running = false
			return
		case <-a.stopCh:
			a.running = false
			return
		case <-ticker.C:
			if err := a.processBatch(ctx); err != nil {
				log.Printf("[EventAggregator] Error processing batch: %v", err)
			}
		}
	}
}

// Stop stops the aggregator
func (a *EventAggregator) Stop() {
	close(a.stopCh)
}

// MessageUpdate holds aggregated event data for a message
type MessageUpdate struct {
	DeliveredAt  *time.Time
	OpenedAt     *time.Time
	ClickedAt    *time.Time
	BouncedAt    *time.Time
	ComplainedAt *time.Time
}

func (a *EventAggregator) processBatch(ctx context.Context) error {
	// Claim a batch of unprocessed events
	rows, err := a.db.QueryContext(ctx, `
		UPDATE mailing_webhook_events
		SET processed = TRUE, processed_at = NOW()
		WHERE id IN (
			SELECT id FROM mailing_webhook_events
			WHERE processed = FALSE
			ORDER BY received_at
			LIMIT 10000
			FOR UPDATE SKIP LOCKED
		)
		RETURNING message_id, event_type, event_timestamp
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	// Group events by message_id
	updates := make(map[string]*MessageUpdate)
	count := 0

	for rows.Next() {
		var messageID, eventType string
		var eventTime time.Time
		if err := rows.Scan(&messageID, &eventType, &eventTime); err != nil {
			continue
		}

		if updates[messageID] == nil {
			updates[messageID] = &MessageUpdate{}
		}

		// Only keep the first occurrence of each event type
		switch eventType {
		case "delivered", "delivery", "Delivery":
			if updates[messageID].DeliveredAt == nil {
				updates[messageID].DeliveredAt = &eventTime
			}
		case "opened", "open", "Open":
			if updates[messageID].OpenedAt == nil {
				updates[messageID].OpenedAt = &eventTime
			}
		case "clicked", "click", "Click":
			if updates[messageID].ClickedAt == nil {
				updates[messageID].ClickedAt = &eventTime
			}
		case "bounced", "bounce", "Bounce":
			if updates[messageID].BouncedAt == nil {
				updates[messageID].BouncedAt = &eventTime
			}
		case "complained", "complaint", "spam", "Complaint", "spam_report":
			if updates[messageID].ComplainedAt == nil {
				updates[messageID].ComplainedAt = &eventTime
			}
		}
		count++
	}

	if count == 0 {
		return nil
	}

	// Batch update message log
	for messageID, update := range updates {
		_, err := a.db.ExecContext(ctx, `
			UPDATE mailing_message_log
			SET 
				delivered_at = COALESCE($2, delivered_at),
				opened_at = COALESCE($3, opened_at),
				clicked_at = COALESCE($4, clicked_at),
				bounced_at = COALESCE($5, bounced_at),
				complained_at = COALESCE($6, complained_at),
				updated_at = NOW()
			WHERE message_id = $1
		`, messageID,
			update.DeliveredAt,
			update.OpenedAt,
			update.ClickedAt,
			update.BouncedAt,
			update.ComplainedAt,
		)
		if err != nil {
			log.Printf("[EventAggregator] Failed to update message %s: %v", messageID, err)
		}
	}

	log.Printf("[EventAggregator] Processed %d events, updated %d messages", count, len(updates))
	return nil
}
