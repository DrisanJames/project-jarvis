package domain

import (
	"time"
)

// CampaignStatus enumerates the lifecycle states of a campaign.
type CampaignStatus string

const (
	CampaignDraft     CampaignStatus = "draft"
	CampaignScheduled CampaignStatus = "scheduled"
	CampaignPreparing CampaignStatus = "preparing"
	CampaignSending   CampaignStatus = "sending"
	CampaignSent      CampaignStatus = "sent"
	CampaignPaused    CampaignStatus = "paused"
	CampaignFailed    CampaignStatus = "failed"
	CampaignCancelled CampaignStatus = "cancelled"
)

// Campaign represents an email campaign with its content and delivery config.
type Campaign struct {
	ID             string         `json:"id" db:"id"`
	OrganizationID string         `json:"organization_id" db:"organization_id"`
	ListID         *string        `json:"list_id" db:"list_id"`
	SegmentID      *string        `json:"segment_id" db:"segment_id"`
	TemplateID     *string        `json:"template_id" db:"template_id"`
	ProfileID      *string        `json:"sending_profile_id" db:"sending_profile_id"`
	Name           string         `json:"name" db:"name"`
	Subject        string         `json:"subject" db:"subject"`
	FromName       string         `json:"from_name" db:"from_name"`
	FromEmail      string         `json:"from_email" db:"from_email"`
	ReplyTo        string         `json:"reply_to" db:"reply_to"`
	HTMLContent    string         `json:"html_content" db:"html_content"`
	PlainContent   string         `json:"plain_content" db:"plain_content"`
	PreviewText    string         `json:"preview_text" db:"preview_text"`
	Status         CampaignStatus `json:"status" db:"status"`
	ScheduledAt    *time.Time     `json:"scheduled_at" db:"scheduled_at"`
	ThrottleSpeed  string         `json:"throttle_speed" db:"throttle_speed"`
	MaxRecipients  *int           `json:"max_recipients" db:"max_recipients"`
	TrackingDomain string         `json:"tracking_domain" db:"tracking_domain"`

	// Stats (read-only, populated by queries)
	TotalRecipients  int     `json:"total_recipients" db:"total_recipients"`
	SentCount        int     `json:"sent_count" db:"sent_count"`
	DeliveredCount   int     `json:"delivered_count" db:"delivered_count"`
	OpenCount        int     `json:"open_count" db:"open_count"`
	ClickCount       int     `json:"click_count" db:"click_count"`
	BounceCount      int     `json:"bounce_count" db:"bounce_count"`
	ComplaintCount   int     `json:"complaint_count" db:"complaint_count"`
	UnsubscribeCount int     `json:"unsubscribe_count" db:"unsubscribe_count"`
	Revenue          float64 `json:"revenue" db:"revenue"`

	StartedAt   *time.Time `json:"started_at" db:"started_at"`
	CompletedAt *time.Time `json:"completed_at" db:"completed_at"`
	CreatedAt   time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at" db:"updated_at"`
}

// IsTerminal returns true if the campaign is in a final state.
func (c *Campaign) IsTerminal() bool {
	return c.Status == CampaignSent || c.Status == CampaignFailed || c.Status == CampaignCancelled
}

// QueueItemStatus enumerates the lifecycle of a single email in the send queue.
type QueueItemStatus string

const (
	QueueQueued     QueueItemStatus = "queued"
	QueueClaimed    QueueItemStatus = "claimed"
	QueueSending    QueueItemStatus = "sending"
	QueueSent       QueueItemStatus = "sent"
	QueueFailed     QueueItemStatus = "failed"
	QueueSkipped    QueueItemStatus = "skipped"
	QueueDeadLetter QueueItemStatus = "dead_letter"
)
