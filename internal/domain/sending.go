package domain

import "time"

// ESPType identifies the email service provider used for sending.
type ESPType string

const (
	ESPSparkPost ESPType = "sparkpost"
	ESPSES       ESPType = "ses"
	ESPMailgun   ESPType = "mailgun"
	ESPSendGrid  ESPType = "sendgrid"
	ESPPMTA      ESPType = "pmta"
	ESPSMTP      ESPType = "smtp"
)

// EmailMessage is the fully-resolved message ready for an ESP sender.
// By the time a message reaches this struct, all template substitution,
// tracking injection, and header generation is complete.
type EmailMessage struct {
	ID           string            `json:"id"`
	CampaignID   string            `json:"campaign_id"`
	SubscriberID string            `json:"subscriber_id"`
	Email        string            `json:"email"`
	FromName     string            `json:"from_name"`
	FromEmail    string            `json:"from_email"`
	ReplyTo      string            `json:"reply_to"`
	Subject      string            `json:"subject"`
	HTMLContent  string            `json:"html_content"`
	TextContent  string            `json:"text_content"`
	Headers      map[string]string `json:"headers,omitempty"`
	ProfileID    string            `json:"profile_id"`
	ESPType      ESPType           `json:"esp_type"`
}

// SendResult is returned by an ESP sender after attempting delivery.
type SendResult struct {
	Success   bool      `json:"success"`
	MessageID string    `json:"message_id"`
	ESPType   ESPType   `json:"esp_type"`
	SentAt    time.Time `json:"sent_at"`
	Error     string    `json:"error,omitempty"`
}

// SendingProfile holds the credentials and configuration for an ESP.
type SendingProfile struct {
	ID             string  `json:"id" db:"id"`
	OrganizationID string  `json:"organization_id" db:"organization_id"`
	Name           string  `json:"name" db:"name"`
	VendorType     ESPType `json:"vendor_type" db:"vendor_type"`
	FromName       string  `json:"from_name" db:"from_name"`
	FromEmail      string  `json:"from_email" db:"from_email"`
	ReplyEmail     string  `json:"reply_email" db:"reply_email"`
	SMTPHost       string  `json:"smtp_host" db:"smtp_host"`
	SMTPPort       int     `json:"smtp_port" db:"smtp_port"`
	SMTPUser       string  `json:"-" db:"smtp_username"`
	SMTPPass       string  `json:"-" db:"smtp_password"`
	APIKey         string  `json:"-" db:"api_key"`
	APISecret      string  `json:"-" db:"api_secret"`
	SendingDomain  string  `json:"sending_domain" db:"sending_domain"`
	TrackingDomain string  `json:"tracking_domain" db:"tracking_domain"`
	IPPool         string  `json:"ip_pool" db:"ip_pool"`
	HourlyLimit    int     `json:"hourly_limit" db:"hourly_limit"`
	DailyLimit     int     `json:"daily_limit" db:"daily_limit"`
	Status         string  `json:"status" db:"status"`
}

// SendingDomain represents a configured sending domain with DNS verification status.
type SendingDomain struct {
	ID              string `json:"id" db:"id"`
	OrganizationID  string `json:"organization_id" db:"organization_id"`
	Domain          string `json:"domain" db:"domain"`
	DKIMSelector    string `json:"dkim_selector" db:"dkim_selector"`
	DKIMKeyPath     string `json:"-" db:"dkim_key_path"`
	SPFStatus       string `json:"spf_status" db:"spf_status"`
	DKIMStatus      string `json:"dkim_status" db:"dkim_status"`
	DMARCStatus     string `json:"dmarc_status" db:"dmarc_status"`
	TrackingDomain  string `json:"tracking_domain" db:"tracking_domain"`
	IsActive        bool   `json:"is_active" db:"is_active"`
	WarmupStage     int    `json:"warmup_stage" db:"warmup_stage"`
	DailyLimit      int    `json:"daily_limit" db:"daily_limit"`
}
