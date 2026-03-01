package tracking

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

type EventType string

const (
	EventOpen        EventType = "opened"
	EventClick       EventType = "clicked"
	EventUnsubscribe EventType = "unsubscribed"
)

type TrackingEvent struct {
	EventType    EventType `json:"event_type"`
	OrgID        string    `json:"org_id"`
	CampaignID   string    `json:"campaign_id"`
	SubscriberID string    `json:"subscriber_id"`
	EmailID      string    `json:"email_id,omitempty"`
	LinkURL      string    `json:"link_url,omitempty"`
	IPAddress    string    `json:"ip_address"`
	UserAgent    string    `json:"user_agent"`
	Timestamp    time.Time `json:"timestamp"`
}

type Publisher struct {
	client   *sqs.Client
	queueURL string
}

func NewPublisher(client *sqs.Client, queueURL string) *Publisher {
	return &Publisher{client: client, queueURL: queueURL}
}

func (p *Publisher) Publish(ctx context.Context, evt TrackingEvent) {
	body, err := json.Marshal(evt)
	if err != nil {
		log.Printf("ERROR marshal tracking event: %v", err)
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, err := p.client.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl:    aws.String(p.queueURL),
			MessageBody: aws.String(string(body)),
		})
		if err != nil {
			log.Printf("ERROR publishing to SQS: %v", err)
		}
	}()
}
