package tracking

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
)

type Consumer struct {
	sqsClient *sqs.Client
	queueURL  string
	db        *sql.DB
	done      chan struct{}
}

func NewConsumer(sqsClient *sqs.Client, queueURL string, db *sql.DB) *Consumer {
	return &Consumer{
		sqsClient: sqsClient,
		queueURL:  queueURL,
		db:        db,
		done:      make(chan struct{}),
	}
}

// pollWorkers controls how many concurrent SQS receive loops run per task.
// Each worker long-polls independently; within a batch messages are fanned
// out via goroutines. With 2 ECS tasks × 16 workers × 10-msg batches plus
// the idempotency fast-path, the consumer can drain large redelivery
// backlogs at >2k msg/min while each new event still flows through the
// full insert + aggregate update + engagement recompute pipeline.
const pollWorkers = 16

func (c *Consumer) Start(ctx context.Context) {
	log.Printf("SQS tracking consumer started (queue=%s, workers=%d)", c.queueURL, pollWorkers)
	for i := 0; i < pollWorkers; i++ {
		go c.poll(ctx)
	}
}

func (c *Consumer) Stop() {
	close(c.done)
}

func (c *Consumer) poll(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		default:
		}

		out, err := c.sqsClient.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(c.queueURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     20,
			VisibilityTimeout:   120,
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("SQS receive error: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		if len(out.Messages) == 0 {
			continue
		}

		var wg sync.WaitGroup
		for _, msg := range out.Messages {
			wg.Add(1)
			go func(msg sqstypes.Message) {
				defer wg.Done()

				var evt TrackingEvent
				if err := json.Unmarshal([]byte(*msg.Body), &evt); err != nil {
					log.Printf("SQS bad message: %v", err)
					c.deleteMessage(ctx, msg.ReceiptHandle)
					return
				}

				if err := c.processEvent(ctx, evt); err != nil {
					log.Printf("SQS process error (%s): %v", evt.EventType, err)
					return
				}

				c.deleteMessage(ctx, msg.ReceiptHandle)
			}(msg)
		}
		wg.Wait()
	}
}

func (c *Consumer) deleteMessage(ctx context.Context, handle *string) {
	c.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.queueURL),
		ReceiptHandle: handle,
	})
}

func (c *Consumer) processEvent(ctx context.Context, evt TrackingEvent) error {
	switch evt.EventType {
	case EventOpen:
		return c.processOpen(ctx, evt)
	case EventClick:
		return c.processClick(ctx, evt)
	case EventUnsubscribe:
		return c.processUnsubscribe(ctx, evt)
	default:
		log.Printf("unknown event type: %s", evt.EventType)
		return nil
	}
}

func (c *Consumer) processOpen(ctx context.Context, evt TrackingEvent) error {
	orgID, _ := uuid.Parse(evt.OrgID)
	campaignID, _ := uuid.Parse(evt.CampaignID)
	subscriberID, _ := uuid.Parse(evt.SubscriberID)
	emailID, _ := uuid.Parse(evt.EmailID)

	// Fast-path idempotency check: if this exact (id, event_at) pair is already
	// in the tracking_events partition, skip all downstream work. This lets us
	// drain SQS redelivery backlogs at 10-20x the rate since no aggregate
	// UPDATEs / engagement recomputes run for duplicates.
	var exists bool
	if err := c.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM mailing_tracking_events
			WHERE id = $1 AND event_at = $2 AND event_type = 'opened'
		)
	`, emailID, evt.Timestamp).Scan(&exists); err == nil && exists {
		return nil
	}

	var email string
	c.db.QueryRowContext(ctx, `SELECT email FROM mailing_subscribers WHERE id = $1`, subscriberID).Scan(&email)

	// MPP detection: open within 120s of the sent event (sent timestamp is reliable; delivered is batch-delayed)
	isMachineOpen := false
	var refAt time.Time
	if err := c.db.QueryRowContext(ctx, `
		SELECT event_at FROM mailing_tracking_events
		WHERE subscriber_id = $1 AND campaign_id = $2 AND event_type = 'sent'
		ORDER BY event_at DESC LIMIT 1
	`, subscriberID, campaignID).Scan(&refAt); err == nil {
		if evt.Timestamp.Sub(refAt) <= 120*time.Second {
			isMachineOpen = true
		}
	}

	res, err := c.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, event_type, event_at, ip_address, user_agent, device_type, sending_domain, is_machine_open)
		SELECT $1, $2, $3, $4, 'opened', $5, $6, $7, $8,
			LOWER(SPLIT_PART(c.from_email, '@', 2)), $9
		FROM mailing_campaigns c WHERE c.id = $3
		ON CONFLICT DO NOTHING
	`, emailID, orgID, campaignID, subscriberID, evt.Timestamp, evt.IPAddress, evt.UserAgent, detectDevice(evt.UserAgent), isMachineOpen)
	if err != nil {
		return err
	}

	// Guard the aggregate UPDATEs: only run them if the INSERT actually wrote
	// a row. Without this, duplicate SQS deliveries double-count open_count etc.
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}

	if !isMachineOpen {
		c.db.ExecContext(ctx, `UPDATE mailing_campaigns SET open_count = open_count + 1 WHERE id = $1`, campaignID)
		c.db.ExecContext(ctx, `UPDATE mailing_subscribers SET total_opens = total_opens + 1, last_open_at = NOW(), updated_at = NOW() WHERE id = $1`, subscriberID)
		c.db.ExecContext(ctx, `UPDATE mailing_inbox_profiles SET total_opens = total_opens + 1, last_open_at = NOW(), updated_at = NOW() WHERE email = $1`, email)
		c.updateEngagementScore(ctx, subscriberID)
		log.Printf("PROCESSED OPEN: campaign=%s subscriber=%s email=%s mpp=false", campaignID, subscriberID, email)
	} else {
		log.Printf("PROCESSED OPEN (MPP skipped aggregates): campaign=%s subscriber=%s email=%s", campaignID, subscriberID, email)
	}
	return nil
}

// clickNamespace derives a deterministic UUID per (campaign, subscriber,
// timestamp, link_url) tuple so duplicate SQS deliveries hit ON CONFLICT
// instead of creating duplicate click rows.
var clickNamespace = uuid.MustParse("2f9c9d2c-2f3e-4f5a-9b1a-0b8c7e2a0001")

func clickDeterministicID(campaignID, subscriberID uuid.UUID, ts time.Time, linkURL string) uuid.UUID {
	key := campaignID.String() + "|" + subscriberID.String() + "|" + ts.UTC().Format(time.RFC3339Nano) + "|" + linkURL
	return uuid.NewSHA1(clickNamespace, []byte(key))
}

func (c *Consumer) processClick(ctx context.Context, evt TrackingEvent) error {
	orgID, _ := uuid.Parse(evt.OrgID)
	campaignID, _ := uuid.Parse(evt.CampaignID)
	subscriberID, _ := uuid.Parse(evt.SubscriberID)
	clickID := clickDeterministicID(campaignID, subscriberID, evt.Timestamp, evt.LinkURL)

	// Fast-path idempotency check — same reasoning as processOpen.
	var exists bool
	if err := c.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM mailing_tracking_events
			WHERE id = $1 AND event_at = $2 AND event_type = 'clicked'
		)
	`, clickID, evt.Timestamp).Scan(&exists); err == nil && exists {
		return nil
	}

	var email string
	c.db.QueryRowContext(ctx, `SELECT email FROM mailing_subscribers WHERE id = $1`, subscriberID).Scan(&email)

	res, err := c.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, event_type, event_at, ip_address, user_agent, device_type, link_url, sending_domain)
		SELECT $1, $2, $3, $4, 'clicked', $5, $6, $7, $8, $9,
			LOWER(SPLIT_PART(c.from_email, '@', 2))
		FROM mailing_campaigns c WHERE c.id = $3
		ON CONFLICT DO NOTHING
	`, clickID, orgID, campaignID, subscriberID, evt.Timestamp, evt.IPAddress, evt.UserAgent, detectDevice(evt.UserAgent), evt.LinkURL)
	if err != nil {
		return err
	}

	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}

	c.db.ExecContext(ctx, `UPDATE mailing_campaigns SET click_count = click_count + 1 WHERE id = $1`, campaignID)
	c.db.ExecContext(ctx, `UPDATE mailing_subscribers SET total_clicks = total_clicks + 1, last_click_at = NOW(), updated_at = NOW() WHERE id = $1`, subscriberID)
	c.db.ExecContext(ctx, `UPDATE mailing_inbox_profiles SET total_clicks = total_clicks + 1, last_click_at = NOW(), updated_at = NOW() WHERE email = $1`, email)
	c.updateEngagementScore(ctx, subscriberID)

	log.Printf("PROCESSED CLICK: campaign=%s subscriber=%s url=%s", campaignID, subscriberID, evt.LinkURL)
	return nil
}

func (c *Consumer) processUnsubscribe(ctx context.Context, evt TrackingEvent) error {
	orgID, _ := uuid.Parse(evt.OrgID)
	campaignID, _ := uuid.Parse(evt.CampaignID)
	subscriberID, _ := uuid.Parse(evt.SubscriberID)

	var email string
	c.db.QueryRowContext(ctx, `SELECT email FROM mailing_subscribers WHERE id = $1`, subscriberID).Scan(&email)

	c.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, event_type, event_at, ip_address, user_agent)
		VALUES ($1, $2, $3, $4, 'unsubscribed', $5, $6, $7)
	`, uuid.New(), orgID, campaignID, subscriberID, evt.Timestamp, evt.IPAddress, evt.UserAgent)

	c.db.ExecContext(ctx, `UPDATE mailing_subscribers SET status = 'unsubscribed', updated_at = NOW() WHERE id = $1`, subscriberID)
	c.db.ExecContext(ctx, `UPDATE mailing_campaigns SET unsubscribe_count = COALESCE(unsubscribe_count, 0) + 1 WHERE id = $1`, campaignID)
	c.db.ExecContext(ctx, `
		INSERT INTO mailing_suppressions (id, email, reason, source, active, created_at, updated_at)
		VALUES ($1, $2, 'User unsubscribed', 'unsubscribe', true, NOW(), NOW())
		ON CONFLICT (email) DO UPDATE SET active = true, reason = 'User unsubscribed', updated_at = NOW()
	`, uuid.New(), email)

	log.Printf("PROCESSED UNSUB: campaign=%s subscriber=%s email=%s", campaignID, subscriberID, email)
	return nil
}

func (c *Consumer) updateEngagementScore(ctx context.Context, subscriberID uuid.UUID) {
	var totalOpens, totalClicks, totalEmails int
	var lastOpenAt *time.Time
	c.db.QueryRowContext(ctx, `
		SELECT COALESCE(total_opens,0), COALESCE(total_clicks,0), COALESCE(total_emails_received,1), last_open_at
		FROM mailing_subscribers WHERE id = $1
	`, subscriberID).Scan(&totalOpens, &totalClicks, &totalEmails, &lastOpenAt)

	if totalEmails < 1 {
		totalEmails = 1
	}
	openRate := float64(totalOpens) / float64(totalEmails) * 100
	clickRate := float64(totalClicks) / float64(totalEmails) * 100
	score := (openRate * 0.4) + (clickRate * 0.6)

	if lastOpenAt != nil && time.Since(*lastOpenAt) < 7*24*time.Hour {
		score += 20
	} else if lastOpenAt != nil && time.Since(*lastOpenAt) < 30*24*time.Hour {
		score += 10
	}
	if score > 100 {
		score = 100
	}

	c.db.ExecContext(ctx, `UPDATE mailing_subscribers SET engagement_score = $2, updated_at = NOW() WHERE id = $1`, subscriberID, score)
}

func detectDevice(ua string) string {
	ua = strings.ToLower(ua)
	if strings.Contains(ua, "mobile") || strings.Contains(ua, "android") || strings.Contains(ua, "iphone") {
		return "mobile"
	}
	if strings.Contains(ua, "tablet") || strings.Contains(ua, "ipad") {
		return "tablet"
	}
	return "desktop"
}
