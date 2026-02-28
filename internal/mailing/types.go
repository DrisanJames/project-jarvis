package mailing

import (
	"database/sql/driver"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Status constants
const (
	StatusActive   = "active"
	StatusInactive = "inactive"
	StatusDraft    = "draft"
	StatusQueued   = "queued"
	StatusSending  = "sending"
	StatusSent     = "sent"
	StatusPaused   = "paused"
	StatusFailed   = "failed"
)

// Subscriber status constants
const (
	SubscriberConfirmed    = "confirmed"
	SubscriberUnconfirmed  = "unconfirmed"
	SubscriberUnsubscribed = "unsubscribed"
	SubscriberBounced      = "bounced"
	SubscriberComplained   = "complained"
)

// Event type constants
const (
	EventSent         = "sent"
	EventDelivered    = "delivered"
	EventOpened       = "opened"
	EventClicked      = "clicked"
	EventBounced      = "bounced"
	EventComplained   = "complained"
	EventUnsubscribed = "unsubscribed"
)

// Server type constants
const (
	ServerSparkPost = "sparkpost"
	ServerSES       = "ses"
	ServerMailgun   = "mailgun"
	ServerSMTP      = "smtp"
)

// JSON helper type for JSONB fields
type JSON map[string]interface{}

func (j JSON) Value() (driver.Value, error) {
	return json.Marshal(j)
}

func (j *JSON) Scan(value interface{}) error {
	if value == nil {
		*j = nil
		return nil
	}
	b, ok := value.([]byte)
	if !ok {
		return nil
	}
	return json.Unmarshal(b, j)
}

// Organization represents a tenant/organization
type Organization struct {
	ID          uuid.UUID  `json:"id" db:"id"`
	Name        string     `json:"name" db:"name"`
	Slug        string     `json:"slug" db:"slug"`
	Settings    JSON       `json:"settings" db:"settings"`
	Status      string     `json:"status" db:"status"`
	CreatedAt   time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at" db:"updated_at"`
}

// List represents a mailing list
type List struct {
	ID               uuid.UUID  `json:"id" db:"id"`
	OrganizationID   uuid.UUID  `json:"organization_id" db:"organization_id"`
	Name             string     `json:"name" db:"name"`
	Description      string     `json:"description" db:"description"`
	DefaultFromName  string     `json:"default_from_name" db:"default_from_name"`
	DefaultFromEmail string     `json:"default_from_email" db:"default_from_email"`
	DefaultReplyTo   string     `json:"default_reply_to" db:"default_reply_to"`
	SubscriberCount  int        `json:"subscriber_count" db:"subscriber_count"`
	ActiveCount      int        `json:"active_count" db:"active_count"`
	OptInType        string     `json:"opt_in_type" db:"opt_in_type"`
	Status           string     `json:"status" db:"status"`
	Settings         JSON       `json:"settings" db:"settings"`
	CreatedAt        time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at" db:"updated_at"`
}

// Subscriber represents an email subscriber
type Subscriber struct {
	ID                  uuid.UUID  `json:"id" db:"id"`
	OrganizationID      uuid.UUID  `json:"organization_id" db:"organization_id"`
	ListID              uuid.UUID  `json:"list_id" db:"list_id"`
	Email               string     `json:"email" db:"email"`
	EmailHash           string     `json:"-" db:"email_hash"`
	FirstName           string     `json:"first_name" db:"first_name"`
	LastName            string     `json:"last_name" db:"last_name"`
	Status              string     `json:"status" db:"status"`
	Source              string     `json:"source" db:"source"`
	IPAddress           string     `json:"ip_address" db:"ip_address"`
	CustomFields        JSON       `json:"custom_fields" db:"custom_fields"`
	EngagementScore     float64    `json:"engagement_score" db:"engagement_score"`
	TotalEmailsReceived int        `json:"total_emails_received" db:"total_emails_received"`
	TotalOpens          int        `json:"total_opens" db:"total_opens"`
	TotalClicks         int        `json:"total_clicks" db:"total_clicks"`
	LastOpenAt          *time.Time `json:"last_open_at" db:"last_open_at"`
	LastClickAt         *time.Time `json:"last_click_at" db:"last_click_at"`
	LastEmailAt         *time.Time `json:"last_email_at" db:"last_email_at"`
	OptimalSendHourUTC  *int       `json:"optimal_send_hour_utc" db:"optimal_send_hour_utc"`
	Timezone            string     `json:"timezone" db:"timezone"`
	SubscribedAt        time.Time  `json:"subscribed_at" db:"subscribed_at"`
	UnsubscribedAt      *time.Time `json:"unsubscribed_at" db:"unsubscribed_at"`
	CreatedAt           time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at" db:"updated_at"`
}

// Campaign represents an email campaign
type Campaign struct {
	ID                     uuid.UUID  `json:"id" db:"id"`
	OrganizationID         uuid.UUID  `json:"organization_id" db:"organization_id"`
	ListID                 *uuid.UUID `json:"list_id" db:"list_id"`
	TemplateID             *uuid.UUID `json:"template_id" db:"template_id"`
	SegmentID              *uuid.UUID `json:"segment_id" db:"segment_id"`
	Name                   string     `json:"name" db:"name"`
	CampaignType           string     `json:"campaign_type" db:"campaign_type"`
	Subject                string     `json:"subject" db:"subject"`
	FromName               string     `json:"from_name" db:"from_name"`
	FromEmail              string     `json:"from_email" db:"from_email"`
	ReplyTo                string     `json:"reply_to" db:"reply_to"`
	HTMLContent            string     `json:"html_content" db:"html_content"`
	PlainContent           string     `json:"plain_content" db:"plain_content"`
	PreviewText            string     `json:"preview_text" db:"preview_text"`
	DeliveryServerID       *uuid.UUID `json:"delivery_server_id" db:"delivery_server_id"`
	SendAt                 *time.Time `json:"send_at" db:"send_at"`
	Timezone               string     `json:"timezone" db:"timezone"`
	AISendTimeOptimization bool       `json:"ai_send_time_optimization" db:"ai_send_time_optimization"`
	AIContentOptimization  bool       `json:"ai_content_optimization" db:"ai_content_optimization"`
	AIAudienceOptimization bool       `json:"ai_audience_optimization" db:"ai_audience_optimization"`
	Status                 string     `json:"status" db:"status"`
	TotalRecipients        int        `json:"total_recipients" db:"total_recipients"`
	SentCount              int        `json:"sent_count" db:"sent_count"`
	DeliveredCount         int        `json:"delivered_count" db:"delivered_count"`
	OpenCount              int        `json:"open_count" db:"open_count"`
	UniqueOpenCount        int        `json:"unique_open_count" db:"unique_open_count"`
	ClickCount             int        `json:"click_count" db:"click_count"`
	UniqueClickCount       int        `json:"unique_click_count" db:"unique_click_count"`
	BounceCount            int        `json:"bounce_count" db:"bounce_count"`
	ComplaintCount         int        `json:"complaint_count" db:"complaint_count"`
	UnsubscribeCount       int        `json:"unsubscribe_count" db:"unsubscribe_count"`
	Revenue                float64    `json:"revenue" db:"revenue"`
	StartedAt              *time.Time `json:"started_at" db:"started_at"`
	CompletedAt            *time.Time `json:"completed_at" db:"completed_at"`
	CreatedAt              time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at" db:"updated_at"`
}

// Template represents an email template
type Template struct {
	ID             uuid.UUID  `json:"id" db:"id"`
	OrganizationID uuid.UUID  `json:"organization_id" db:"organization_id"`
	CategoryID     *uuid.UUID `json:"category_id" db:"category_id"`
	Name           string     `json:"name" db:"name"`
	Subject        string     `json:"subject" db:"subject"`
	HTMLContent    string     `json:"html_content" db:"html_content"`
	PlainContent   string     `json:"plain_content" db:"plain_content"`
	PreviewText    string     `json:"preview_text" db:"preview_text"`
	IsPublic       bool       `json:"is_public" db:"is_public"`
	Status         string     `json:"status" db:"status"`
	Variables      JSON       `json:"variables" db:"variables"`
	CreatedAt      time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at" db:"updated_at"`
}

// DeliveryServer represents an email delivery server/ESP
type DeliveryServer struct {
	ID               uuid.UUID  `json:"id" db:"id"`
	OrganizationID   uuid.UUID  `json:"organization_id" db:"organization_id"`
	Name             string     `json:"name" db:"name"`
	ServerType       string     `json:"server_type" db:"server_type"`
	Region           string     `json:"region" db:"region"`
	Settings         JSON       `json:"settings" db:"settings"`
	HourlyQuota      int        `json:"hourly_quota" db:"hourly_quota"`
	DailyQuota       int        `json:"daily_quota" db:"daily_quota"`
	MonthlyQuota     int        `json:"monthly_quota" db:"monthly_quota"`
	UsedHourly       int        `json:"used_hourly" db:"used_hourly"`
	UsedDaily        int        `json:"used_daily" db:"used_daily"`
	UsedMonthly      int        `json:"used_monthly" db:"used_monthly"`
	Probability      int        `json:"probability" db:"probability"`
	Priority         int        `json:"priority" db:"priority"`
	WarmupEnabled    bool       `json:"warmup_enabled" db:"warmup_enabled"`
	WarmupStage      int        `json:"warmup_stage" db:"warmup_stage"`
	Status           string     `json:"status" db:"status"`
	ReputationScore  float64    `json:"reputation_score" db:"reputation_score"`
	LastErrorAt      *time.Time `json:"last_error_at" db:"last_error_at"`
	CreatedAt        time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at" db:"updated_at"`
}

// Segment represents a subscriber segment
type Segment struct {
	ID             uuid.UUID  `json:"id" db:"id"`
	OrganizationID uuid.UUID  `json:"organization_id" db:"organization_id"`
	ListID         *uuid.UUID `json:"list_id" db:"list_id"`
	Name           string     `json:"name" db:"name"`
	Description    string     `json:"description" db:"description"`
	Conditions     JSON       `json:"conditions" db:"conditions"`
	MatchType      string     `json:"match_type" db:"match_type"`
	SubscriberCount int       `json:"subscriber_count" db:"subscriber_count"`
	Status         string     `json:"status" db:"status"`
	CreatedAt      time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at" db:"updated_at"`
}

// TrackingEvent represents an email tracking event
type TrackingEvent struct {
	ID             uuid.UUID  `json:"id" db:"id"`
	OrganizationID uuid.UUID  `json:"organization_id" db:"organization_id"`
	CampaignID     *uuid.UUID `json:"campaign_id" db:"campaign_id"`
	SubscriberID   *uuid.UUID `json:"subscriber_id" db:"subscriber_id"`
	EmailID        *uuid.UUID `json:"email_id" db:"email_id"`
	EventType      string     `json:"event_type" db:"event_type"`
	IPAddress      string     `json:"ip_address" db:"ip_address"`
	UserAgent      string     `json:"user_agent" db:"user_agent"`
	DeviceType     string     `json:"device_type" db:"device_type"`
	LinkURL        string     `json:"link_url" db:"link_url"`
	BounceType     string     `json:"bounce_type" db:"bounce_type"`
	BounceReason   string     `json:"bounce_reason" db:"bounce_reason"`
	EventAt        time.Time  `json:"event_at" db:"event_at"`
	CreatedAt      time.Time  `json:"created_at" db:"created_at"`
}

// QueueItem represents an email in the send queue
type QueueItem struct {
	ID                 uuid.UUID  `json:"id" db:"id"`
	CampaignID         uuid.UUID  `json:"campaign_id" db:"campaign_id"`
	SubscriberID       uuid.UUID  `json:"subscriber_id" db:"subscriber_id"`
	DeliveryServerID   *uuid.UUID `json:"delivery_server_id" db:"delivery_server_id"`
	Subject            string     `json:"subject" db:"subject"`
	HTMLContent        string     `json:"html_content" db:"html_content"`
	PlainContent       string     `json:"plain_content" db:"plain_content"`
	ScheduledAt        time.Time  `json:"scheduled_at" db:"scheduled_at"`
	Priority           int        `json:"priority" db:"priority"`
	PredictedOpenProb  float64    `json:"predicted_open_prob" db:"predicted_open_prob"`
	PredictedRevenue   float64    `json:"predicted_revenue" db:"predicted_revenue"`
	Status             string     `json:"status" db:"status"`
	MessageID          string     `json:"message_id" db:"message_id"`
	Attempts           int        `json:"attempts" db:"attempts"`
	LastAttemptAt      *time.Time `json:"last_attempt_at" db:"last_attempt_at"`
	SentAt             *time.Time `json:"sent_at" db:"sent_at"`
	ErrorMessage       string     `json:"error_message" db:"error_message"`
	CreatedAt          time.Time  `json:"created_at" db:"created_at"`
}

// SendingPlan represents an AI-generated sending plan
type SendingPlan struct {
	ID                  uuid.UUID  `json:"id" db:"id"`
	OrganizationID      uuid.UUID  `json:"organization_id" db:"organization_id"`
	PlanDate            time.Time  `json:"plan_date" db:"plan_date"`
	TimePeriod          string     `json:"time_period" db:"time_period"`
	VolumeCapacity      JSON       `json:"volume_capacity" db:"volume_capacity"`
	AudienceAnalysis    JSON       `json:"audience_analysis" db:"audience_analysis"`
	OfferAnalysis       JSON       `json:"offer_analysis" db:"offer_analysis"`
	TimingAnalysis      JSON       `json:"timing_analysis" db:"timing_analysis"`
	RecommendedVolume   int        `json:"recommended_volume" db:"recommended_volume"`
	PredictedOpens      int        `json:"predicted_opens" db:"predicted_opens"`
	PredictedClicks     int        `json:"predicted_clicks" db:"predicted_clicks"`
	PredictedRevenue    float64    `json:"predicted_revenue" db:"predicted_revenue"`
	ConfidenceScore     float64    `json:"confidence_score" db:"confidence_score"`
	AIExplanation       string     `json:"ai_explanation" db:"ai_explanation"`
	Status              string     `json:"status" db:"status"`
	ApprovedBy          *uuid.UUID `json:"approved_by" db:"approved_by"`
	ApprovedAt          *time.Time `json:"approved_at" db:"approved_at"`
	ExecutedAt          *time.Time `json:"executed_at" db:"executed_at"`
	ActualOpens         *int       `json:"actual_opens" db:"actual_opens"`
	ActualClicks        *int       `json:"actual_clicks" db:"actual_clicks"`
	ActualRevenue       *float64   `json:"actual_revenue" db:"actual_revenue"`
	CreatedAt           time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at" db:"updated_at"`
}

// Offer represents an affiliate offer
type Offer struct {
	ID                uuid.UUID  `json:"id" db:"id"`
	OrganizationID    uuid.UUID  `json:"organization_id" db:"organization_id"`
	ExternalID        string     `json:"external_id" db:"external_id"`
	Name              string     `json:"name" db:"name"`
	Description       string     `json:"description" db:"description"`
	Category          string     `json:"category" db:"category"`
	Payout            float64    `json:"payout" db:"payout"`
	PayoutType        string     `json:"payout_type" db:"payout_type"`
	TrackingURL       string     `json:"tracking_url" db:"tracking_url"`
	PreviewURL        string     `json:"preview_url" db:"preview_url"`
	AllowedGeos       JSON       `json:"allowed_geos" db:"allowed_geos"`
	Status            string     `json:"status" db:"status"`
	TotalSent         int        `json:"total_sent" db:"total_sent"`
	TotalClicks       int        `json:"total_clicks" db:"total_clicks"`
	TotalConversions  int        `json:"total_conversions" db:"total_conversions"`
	TotalRevenue      float64    `json:"total_revenue" db:"total_revenue"`
	AverageEPC        float64    `json:"average_epc" db:"average_epc"`
	ConversionRate    float64    `json:"conversion_rate" db:"conversion_rate"`
	CreatedAt         time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at" db:"updated_at"`
}

// SubscriberIntelligence represents AI-learned subscriber behavior
type SubscriberIntelligence struct {
	ID                 uuid.UUID  `json:"id" db:"id"`
	SubscriberID       uuid.UUID  `json:"subscriber_id" db:"subscriber_id"`
	EngagementProfile  JSON       `json:"engagement_profile" db:"engagement_profile"`
	TemporalProfile    JSON       `json:"temporal_profile" db:"temporal_profile"`
	ContentPreferences JSON       `json:"content_preferences" db:"content_preferences"`
	DeliveryProfile    JSON       `json:"delivery_profile" db:"delivery_profile"`
	RiskProfile        JSON       `json:"risk_profile" db:"risk_profile"`
	PredictiveScores   JSON       `json:"predictive_scores" db:"predictive_scores"`
	ProfileMaturity    float64    `json:"profile_maturity" db:"profile_maturity"`
	LastUpdatedAt      time.Time  `json:"last_updated_at" db:"last_updated_at"`
	CreatedAt          time.Time  `json:"created_at" db:"created_at"`
}

// CampaignStats provides computed campaign statistics
type CampaignStats struct {
	OpenRate       float64 `json:"open_rate"`
	ClickRate      float64 `json:"click_rate"`
	BounceRate     float64 `json:"bounce_rate"`
	ComplaintRate  float64 `json:"complaint_rate"`
	UnsubscribeRate float64 `json:"unsubscribe_rate"`
	CTR            float64 `json:"ctr"` // Click-to-open rate
	RevenuePerSend float64 `json:"revenue_per_send"`
}

// CalculateStats calculates campaign statistics
func (c *Campaign) CalculateStats() CampaignStats {
	stats := CampaignStats{}
	if c.SentCount > 0 {
		stats.OpenRate = float64(c.OpenCount) / float64(c.SentCount) * 100
		stats.ClickRate = float64(c.ClickCount) / float64(c.SentCount) * 100
		stats.BounceRate = float64(c.BounceCount) / float64(c.SentCount) * 100
		stats.ComplaintRate = float64(c.ComplaintCount) / float64(c.SentCount) * 100
		stats.UnsubscribeRate = float64(c.UnsubscribeCount) / float64(c.SentCount) * 100
		stats.RevenuePerSend = c.Revenue / float64(c.SentCount)
	}
	if c.OpenCount > 0 {
		stats.CTR = float64(c.ClickCount) / float64(c.OpenCount) * 100
	}
	return stats
}

// CampaignObjective represents the business purpose and goals for a campaign
type CampaignObjective struct {
	ID             uuid.UUID  `json:"id" db:"id"`
	CampaignID     uuid.UUID  `json:"campaign_id" db:"campaign_id"`
	OrganizationID uuid.UUID  `json:"organization_id" db:"organization_id"`
	
	// Purpose Classification: 'data_activation' or 'offer_revenue'
	Purpose string `json:"purpose" db:"purpose"`
	
	// Data Activation Settings
	ActivationGoal        string   `json:"activation_goal,omitempty" db:"activation_goal"`
	TargetEngagementRate  *float64 `json:"target_engagement_rate,omitempty" db:"target_engagement_rate"`
	TargetCleanRate       *float64 `json:"target_clean_rate,omitempty" db:"target_clean_rate"`
	WarmupDailyIncrement  int      `json:"warmup_daily_increment,omitempty" db:"warmup_daily_increment"`
	WarmupMaxDailyVolume  int      `json:"warmup_max_daily_volume,omitempty" db:"warmup_max_daily_volume"`
	
	// Offer Revenue Settings
	OfferModel      string   `json:"offer_model,omitempty" db:"offer_model"`
	ECPMTarget      *float64 `json:"ecpm_target,omitempty" db:"ecpm_target"`
	BudgetLimit     *float64 `json:"budget_limit,omitempty" db:"budget_limit"`
	BudgetSpent     float64  `json:"budget_spent" db:"budget_spent"`
	TargetMetric    string   `json:"target_metric,omitempty" db:"target_metric"`
	TargetValue     int      `json:"target_value,omitempty" db:"target_value"`
	TargetAchieved  int      `json:"target_achieved" db:"target_achieved"`
	
	// Everflow Integration
	EverflowOfferIDs      json.RawMessage `json:"everflow_offer_ids" db:"everflow_offer_ids"`
	EverflowSubIDTemplate string          `json:"everflow_sub_id_template,omitempty" db:"everflow_sub_id_template"`
	PropertyCode          string          `json:"property_code,omitempty" db:"property_code"`
	
	// Creative Rotation
	ApprovedCreatives     json.RawMessage `json:"approved_creatives" db:"approved_creatives"`
	RotationStrategy      string `json:"rotation_strategy" db:"rotation_strategy"`
	CurrentCreativeIndex  int    `json:"current_creative_index" db:"current_creative_index"`
	
	// AI Configuration
	AIOptimizationEnabled    bool `json:"ai_optimization_enabled" db:"ai_optimization_enabled"`
	AIThroughputOptimization bool `json:"ai_throughput_optimization" db:"ai_throughput_optimization"`
	AICreativeRotation       bool `json:"ai_creative_rotation" db:"ai_creative_rotation"`
	AIBudgetPacing           bool `json:"ai_budget_pacing" db:"ai_budget_pacing"`
	ESPSignalMonitoring      bool `json:"esp_signal_monitoring" db:"esp_signal_monitoring"`
	
	// Thresholds
	PauseOnSpamSignal     bool     `json:"pause_on_spam_signal" db:"pause_on_spam_signal"`
	SpamSignalThreshold   *float64 `json:"spam_signal_threshold,omitempty" db:"spam_signal_threshold"`
	BounceThreshold       *float64 `json:"bounce_threshold,omitempty" db:"bounce_threshold"`
	ThroughputSensitivity string   `json:"throughput_sensitivity" db:"throughput_sensitivity"`
	MinThroughputRate     int      `json:"min_throughput_rate" db:"min_throughput_rate"`
	MaxThroughputRate     int      `json:"max_throughput_rate" db:"max_throughput_rate"`
	
	// Pacing
	TargetCompletionHours int    `json:"target_completion_hours,omitempty" db:"target_completion_hours"`
	PacingStrategy        string `json:"pacing_strategy" db:"pacing_strategy"`
	
	// Timestamps
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
}

// ESPSignal represents a deliverability signal from an ESP
type ESPSignal struct {
	ID             uuid.UUID  `json:"id" db:"id"`
	CampaignID     uuid.UUID  `json:"campaign_id" db:"campaign_id"`
	OrganizationID uuid.UUID  `json:"organization_id" db:"organization_id"`
	
	ESPType         string `json:"esp_type" db:"esp_type"`
	SignalType      string `json:"signal_type" db:"signal_type"`
	ISP             string `json:"isp,omitempty" db:"isp"`
	ReceivingDomain string `json:"receiving_domain,omitempty" db:"receiving_domain"`
	
	SignalCount      int             `json:"signal_count" db:"signal_count"`
	SampleMessageIDs json.RawMessage `json:"sample_message_ids" db:"sample_message_ids"`
	BounceClass      string `json:"bounce_class,omitempty" db:"bounce_class"`
	ErrorCode        string `json:"error_code,omitempty" db:"error_code"`
	ErrorMessage     string `json:"error_message,omitempty" db:"error_message"`
	
	IntervalStart time.Time `json:"interval_start" db:"interval_start"`
	IntervalEnd   time.Time `json:"interval_end" db:"interval_end"`
	
	AIInterpretation  string     `json:"ai_interpretation,omitempty" db:"ai_interpretation"`
	AISeverity        string     `json:"ai_severity,omitempty" db:"ai_severity"`
	RecommendedAction string     `json:"recommended_action,omitempty" db:"recommended_action"`
	ActionTaken       bool       `json:"action_taken" db:"action_taken"`
	ActionTakenAt     *time.Time `json:"action_taken_at,omitempty" db:"action_taken_at"`
	
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}

// ISPBehavior represents learned ISP behavior patterns
type ISPBehavior struct {
	ID             uuid.UUID `json:"id" db:"id"`
	OrganizationID uuid.UUID `json:"organization_id" db:"organization_id"`
	
	ISP             string `json:"isp" db:"isp"`
	ReceivingDomain string `json:"receiving_domain,omitempty" db:"receiving_domain"`
	BehaviorStatus  string `json:"behavior_status" db:"behavior_status"`
	
	DeliveryRate       *float64 `json:"delivery_rate,omitempty" db:"delivery_rate"`
	BounceRate         *float64 `json:"bounce_rate,omitempty" db:"bounce_rate"`
	SpamComplaintRate  *float64 `json:"spam_complaint_rate,omitempty" db:"spam_complaint_rate"`
	OpenRate           *float64 `json:"open_rate,omitempty" db:"open_rate"`
	
	RecommendedHourlyVolume int             `json:"recommended_hourly_volume,omitempty" db:"recommended_hourly_volume"`
	RecommendedSendHours    json.RawMessage `json:"recommended_send_hours" db:"recommended_send_hours"`
	
	SampleSize      int       `json:"sample_size" db:"sample_size"`
	ConfidenceScore float64   `json:"confidence_score" db:"confidence_score"`
	LastAnalyzedAt  *time.Time `json:"last_analyzed_at,omitempty" db:"last_analyzed_at"`
	
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
}

// CreativePerformance tracks performance of creative variants
type CreativePerformance struct {
	ID             uuid.UUID  `json:"id" db:"id"`
	CampaignID     uuid.UUID  `json:"campaign_id" db:"campaign_id"`
	OrganizationID uuid.UUID  `json:"organization_id" db:"organization_id"`
	
	CreativeIndex int        `json:"creative_index" db:"creative_index"`
	SubjectLine   string     `json:"subject_line,omitempty" db:"subject_line"`
	Preheader     string     `json:"preheader,omitempty" db:"preheader"`
	TemplateID    *uuid.UUID `json:"template_id,omitempty" db:"template_id"`
	
	SentCount       int     `json:"sent_count" db:"sent_count"`
	DeliveredCount  int     `json:"delivered_count" db:"delivered_count"`
	OpenCount       int     `json:"open_count" db:"open_count"`
	UniqueOpenCount int     `json:"unique_open_count" db:"unique_open_count"`
	ClickCount      int     `json:"click_count" db:"click_count"`
	UniqueClickCount int    `json:"unique_click_count" db:"unique_click_count"`
	ConversionCount int     `json:"conversion_count" db:"conversion_count"`
	Revenue         float64 `json:"revenue" db:"revenue"`
	
	OpenRate        *float64 `json:"open_rate,omitempty" db:"open_rate"`
	ClickRate       *float64 `json:"click_rate,omitempty" db:"click_rate"`
	ClickToOpenRate *float64 `json:"click_to_open_rate,omitempty" db:"click_to_open_rate"`
	ConversionRate  *float64 `json:"conversion_rate,omitempty" db:"conversion_rate"`
	RevenuePerSend  *float64 `json:"revenue_per_send,omitempty" db:"revenue_per_send"`
	ECPM            *float64 `json:"ecpm,omitempty" db:"ecpm"`
	
	ZScore          *float64 `json:"z_score,omitempty" db:"z_score"`
	ConfidenceLevel *float64 `json:"confidence_level,omitempty" db:"confidence_level"`
	IsWinner        bool     `json:"is_winner" db:"is_winner"`
	Status          string   `json:"status" db:"status"`
	
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
}

// ApprovedCreative represents a pre-approved creative for rotation
type ApprovedCreative struct {
	Subject    string     `json:"subject"`
	Preheader  string     `json:"preheader,omitempty"`
	TemplateID *uuid.UUID `json:"template_id,omitempty"`
	ApprovedAt time.Time  `json:"approved_at"`
	ApprovedBy string     `json:"approved_by,omitempty"`
}

// CampaignOptimizationLog records AI optimization decisions
type CampaignOptimizationLog struct {
	ID             uuid.UUID `json:"id" db:"id"`
	CampaignID     uuid.UUID `json:"campaign_id" db:"campaign_id"`
	OrganizationID uuid.UUID `json:"organization_id" db:"organization_id"`
	
	OptimizationType string `json:"optimization_type" db:"optimization_type"`
	TriggerReason    string          `json:"trigger_reason" db:"trigger_reason"`
	TriggerMetrics   json.RawMessage `json:"trigger_metrics" db:"trigger_metrics"`
	
	OldValue string `json:"old_value,omitempty" db:"old_value"`
	NewValue string `json:"new_value,omitempty" db:"new_value"`
	
	AIReasoning   string   `json:"ai_reasoning,omitempty" db:"ai_reasoning"`
	AIConfidence  *float64 `json:"ai_confidence,omitempty" db:"ai_confidence"`
	
	Applied          bool       `json:"applied" db:"applied"`
	AppliedAt        *time.Time `json:"applied_at,omitempty" db:"applied_at"`
	OutcomeMeasured  bool       `json:"outcome_measured" db:"outcome_measured"`
	OutcomePositive  *bool      `json:"outcome_positive,omitempty" db:"outcome_positive"`
	OutcomeNotes     string     `json:"outcome_notes,omitempty" db:"outcome_notes"`
	
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}
