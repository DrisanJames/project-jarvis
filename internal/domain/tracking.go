package domain

import "time"

// TrackingEventType enumerates the types of email engagement events.
type TrackingEventType string

const (
	EventOpen        TrackingEventType = "open"
	EventClick       TrackingEventType = "click"
	EventUnsubscribe TrackingEventType = "unsubscribe"
	EventBounce      TrackingEventType = "bounce"
	EventComplaint   TrackingEventType = "complaint"
	EventDelivered   TrackingEventType = "delivered"
	EventInjected    TrackingEventType = "injected"
)

// TrackingEvent represents a single engagement event from an email recipient.
type TrackingEvent struct {
	ID             string            `json:"id"`
	OrganizationID string            `json:"organization_id"`
	CampaignID     string            `json:"campaign_id"`
	SubscriberID   string            `json:"subscriber_id,omitempty"`
	Email          string            `json:"email"`
	EventType      TrackingEventType `json:"event_type"`
	IPAddress      string            `json:"ip_address,omitempty"`
	UserAgent      string            `json:"user_agent,omitempty"`
	URL            string            `json:"url,omitempty"`
	IsUnique       bool              `json:"is_unique"`
	CreatedAt      time.Time         `json:"created_at"`
}
