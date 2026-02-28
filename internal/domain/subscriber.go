package domain

import "time"

// SubscriberStatus enumerates the states a subscriber can be in.
type SubscriberStatus string

const (
	SubscriberConfirmed    SubscriberStatus = "confirmed"
	SubscriberUnconfirmed  SubscriberStatus = "unconfirmed"
	SubscriberUnsubscribed SubscriberStatus = "unsubscribed"
	SubscriberBounced      SubscriberStatus = "bounced"
	SubscriberComplained   SubscriberStatus = "complained"
)

// Subscriber represents a single email recipient within a mailing list.
type Subscriber struct {
	ID             string           `json:"id" db:"id"`
	OrganizationID string           `json:"organization_id" db:"organization_id"`
	ListID         string           `json:"list_id" db:"list_id"`
	Email          string           `json:"email" db:"email"`
	EmailHash      string           `json:"-" db:"email_hash"`
	FirstName      string           `json:"first_name" db:"first_name"`
	LastName       string           `json:"last_name" db:"last_name"`
	Status         SubscriberStatus `json:"status" db:"status"`
	CustomFields   map[string]any   `json:"custom_fields" db:"custom_fields"`

	EngagementScore     float64    `json:"engagement_score" db:"engagement_score"`
	TotalEmailsReceived int        `json:"total_emails_received" db:"total_emails_received"`
	TotalOpens          int        `json:"total_opens" db:"total_opens"`
	TotalClicks         int        `json:"total_clicks" db:"total_clicks"`
	LastOpenAt          *time.Time `json:"last_open_at" db:"last_open_at"`
	LastClickAt         *time.Time `json:"last_click_at" db:"last_click_at"`
	LastEmailAt         *time.Time `json:"last_email_at" db:"last_email_at"`
	OptimalSendHourUTC  *int       `json:"optimal_send_hour_utc" db:"optimal_send_hour_utc"`

	SubscribedAt   time.Time  `json:"subscribed_at" db:"subscribed_at"`
	UnsubscribedAt *time.Time `json:"unsubscribed_at" db:"unsubscribed_at"`
	CreatedAt      time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at" db:"updated_at"`
}

// List represents a mailing list that holds subscribers.
type List struct {
	ID              string    `json:"id" db:"id"`
	OrganizationID  string    `json:"organization_id" db:"organization_id"`
	Name            string    `json:"name" db:"name"`
	Description     string    `json:"description" db:"description"`
	SubscriberCount int       `json:"subscriber_count" db:"subscriber_count"`
	ActiveCount     int       `json:"active_count" db:"active_count"`
	Status          string    `json:"status" db:"status"`
	CreatedAt       time.Time `json:"created_at" db:"created_at"`
	UpdatedAt       time.Time `json:"updated_at" db:"updated_at"`
}
