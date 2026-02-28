package api

import "time"

// ESPQuota represents quota allocation for a single ESP
type ESPQuota struct {
	ProfileID  string `json:"profile_id"`
	Percentage int    `json:"percentage"` // 0-100
}

// CampaignInput is the simplified input for creating/updating campaigns
type CampaignInput struct {
	// Basic Info
	Name        string `json:"name"`
	Subject     string `json:"subject"`
	PreviewText string `json:"preview_text,omitempty"` // Email preview text
	
	// Content
	HTMLContent string `json:"html_content"`
	TextContent string `json:"text_content,omitempty"` // Auto-generated if empty
	
	// Audience - SEGMENT-BASED (segments can span multiple lists)
	SegmentIDs []string `json:"segment_ids,omitempty"` // Primary: multiple segments to mail to
	SegmentID  *string  `json:"segment_id,omitempty"`  // Backward compat: single segment
	ListID     *string  `json:"list_id,omitempty"`     // Backward compat: single list
	ListIDs    []string `json:"list_ids,omitempty"`    // Backward compat: multiple lists
	
	// Suppression - MULTI-SUPPRESSION SUPPORT (can be 1000+ lists)
	SuppressionListIDs    []string `json:"suppression_list_ids,omitempty"`    // Email blocklists to exclude
	SuppressionSegmentIDs []string `json:"suppression_segment_ids,omitempty"` // Segments to exclude (by conditions)
	
	// Sending Profile (ESP selection - like Ongage Vendors)
	SendingProfileID *string    `json:"sending_profile_id,omitempty"` // Primary ESP (backward compat)
	ESPQuotas        []ESPQuota `json:"esp_quotas,omitempty"`         // Multi-ESP with quotas
	
	// From/Reply (optional - uses profile defaults if not set)
	FromName   *string `json:"from_name,omitempty"`
	FromEmail  *string `json:"from_email,omitempty"`
	ReplyEmail *string `json:"reply_email,omitempty"`
	
	// Scheduling - SIMPLIFIED from Ongage's complex options
	SendType string `json:"send_type"` // "instant", "scheduled", "smart"
	
	// For scheduled sends
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
	
	// For smart sends (AI-optimized timing)
	SmartSendWindow int `json:"smart_send_window,omitempty"` // Hours (default 24)
	
	// Throttling - SIMPLIFIED presets OR custom duration
	ThrottleSpeed         string `json:"throttle_speed,omitempty"`           // "instant", "gentle", "moderate", "careful", "custom"
	ThrottleRatePerMinute int    `json:"throttle_rate_per_minute,omitempty"` // Custom rate (for custom throttle)
	ThrottleDurationHours int    `json:"throttle_duration_hours,omitempty"`  // Hours to spread delivery over
	
	// Quota (optional limit)
	MaxRecipients *int `json:"max_recipients,omitempty"` // Limit total sends (like Ongage quota)
	
	// Timezone handling
	SendByTimezone bool   `json:"send_by_timezone,omitempty"` // Send at local time
	TimezoneField  string `json:"timezone_field,omitempty"`   // Which field has timezone
	
	// Tags for organization
	Tags []string `json:"tags,omitempty"`
	
	// Everflow Creative Integration
	EverflowCreativeID *int    `json:"everflow_creative_id,omitempty"`
	EverflowOfferID    *int    `json:"everflow_offer_id,omitempty"`
	TrackingLinkTemplate *string `json:"tracking_link_template,omitempty"`
}

// Campaign represents a complete campaign record
type Campaign struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Subject          string     `json:"subject"`
	PreviewText      string     `json:"preview_text,omitempty"`
	HTMLContent      string     `json:"html_content,omitempty"`
	TextContent      string     `json:"text_content,omitempty"`
	
	// Audience - supports multi-list
	ListID                *string  `json:"list_id,omitempty"`
	ListIDs               []string `json:"list_ids,omitempty"`
	SegmentID             *string  `json:"segment_id,omitempty"`
	SegmentIDs            []string `json:"segment_ids,omitempty"`
	SuppressionListIDs    []string `json:"suppression_list_ids,omitempty"`
	SuppressionSegmentIDs []string `json:"suppression_segment_ids,omitempty"` // Segments to exclude
	
	// ESP - supports multi-ESP with quotas
	SendingProfileID *string    `json:"sending_profile_id,omitempty"`
	ESPQuotas        []ESPQuota `json:"esp_quotas,omitempty"`
	
	FromName         string     `json:"from_name"`
	FromEmail        string     `json:"from_email"`
	ReplyEmail       string     `json:"reply_email,omitempty"`
	SendType         string     `json:"send_type"`
	ScheduledAt      *time.Time `json:"scheduled_at,omitempty"`
	
	// Throttling
	ThrottleSpeed         string `json:"throttle_speed"`
	ThrottleRatePerMinute int    `json:"throttle_rate_per_minute,omitempty"`
	ThrottleDurationHours int    `json:"throttle_duration_hours,omitempty"`
	MaxRecipients    *int       `json:"max_recipients,omitempty"`
	
	// Stats
	Status        string     `json:"status"`
	TotalRecipients int      `json:"total_recipients"`
	SentCount     int        `json:"sent_count"`
	OpenCount     int        `json:"open_count"`
	ClickCount    int        `json:"click_count"`
	BounceCount   int        `json:"bounce_count"`
	ComplaintCount int       `json:"complaint_count"`
	UnsubscribeCount int     `json:"unsubscribe_count"`
	
	// Timestamps
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	
	// Profile info (denormalized for display)
	ProfileName   string `json:"profile_name,omitempty"`
	ProfileVendor string `json:"profile_vendor,omitempty"`
	
	// Audience info
	ListName    string `json:"list_name,omitempty"`
	SegmentName string `json:"segment_name,omitempty"`
	
	Tags []string `json:"tags,omitempty"`
}

// ThrottlePreset defines throttling speeds
type ThrottlePreset struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	PerMinute   int    `json:"per_minute"`
	PerHour     int    `json:"per_hour"`
}

// ThrottlePresets - MUCH simpler than Ongage's complex UI
var ThrottlePresets = map[string]ThrottlePreset{
	"instant": {
		Name:        "Instant",
		Description: "Send as fast as possible",
		PerMinute:   1000,
		PerHour:     50000,
	},
	"gentle": {
		Name:        "Gentle",
		Description: "Spread over 2-4 hours for better deliverability",
		PerMinute:   100,
		PerHour:     5000,
	},
	"moderate": {
		Name:        "Moderate", 
		Description: "Spread over 6-12 hours for warming IPs",
		PerMinute:   50,
		PerHour:     2500,
	},
	"careful": {
		Name:        "Careful",
		Description: "Spread over 24+ hours for reputation building",
		PerMinute:   20,
		PerHour:     1000,
	},
}

// Minimum preparation time before a campaign can be sent (in minutes)
const MinPreparationMinutes = 5
