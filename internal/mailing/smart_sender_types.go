package mailing

import (
	"time"

	"github.com/google/uuid"
)

// ============================================================================
// AI SMART SENDER TYPES
// ============================================================================

// TargetMetric represents the optimization target
type TargetMetric string

const (
	TargetMetricOpens       TargetMetric = "opens"
	TargetMetricClicks      TargetMetric = "clicks"
	TargetMetricConversions TargetMetric = "conversions"
	TargetMetricRevenue     TargetMetric = "revenue"
)

// DecisionType represents the type of AI decision
type DecisionType string

const (
	DecisionThrottleIncrease DecisionType = "throttle_increase"
	DecisionThrottleDecrease DecisionType = "throttle_decrease"
	DecisionPause            DecisionType = "pause"
	DecisionResume           DecisionType = "resume"
	DecisionVariantWinner    DecisionType = "variant_winner"
	DecisionAlert            DecisionType = "alert"
	DecisionSendTimeAdjust   DecisionType = "send_time_adjust"
)

// AlertSeverity represents alert severity levels
type AlertSeverity string

const (
	AlertSeverityInfo     AlertSeverity = "info"
	AlertSeverityWarning  AlertSeverity = "warning"
	AlertSeverityCritical AlertSeverity = "critical"
)

// AlertType represents types of anomaly alerts
type AlertType string

const (
	AlertHighBounce     AlertType = "high_bounce"
	AlertHighComplaint  AlertType = "high_complaint"
	AlertDeliveryDrop   AlertType = "delivery_drop"
	AlertEngagementDrop AlertType = "engagement_drop"
	AlertThrottleHit    AlertType = "throttle_hit"
)

// VariantStatus represents A/B variant status
type VariantStatus string

const (
	VariantStatusActive  VariantStatus = "active"
	VariantStatusWinner  VariantStatus = "winner"
	VariantStatusLoser   VariantStatus = "loser"
	VariantStatusStopped VariantStatus = "stopped"
)

// CampaignAISettings represents AI optimization settings for a campaign
type CampaignAISettings struct {
	ID                       uuid.UUID    `json:"id" db:"id"`
	CampaignID               uuid.UUID    `json:"campaign_id" db:"campaign_id"`
	EnableSmartSending       bool         `json:"enable_smart_sending" db:"enable_smart_sending"`
	EnableThrottleOptimization bool       `json:"enable_throttle_optimization" db:"enable_throttle_optimization"`
	EnableSendTimeOptimization bool       `json:"enable_send_time_optimization" db:"enable_send_time_optimization"`
	EnableABAutoWinner       bool         `json:"enable_ab_auto_winner" db:"enable_ab_auto_winner"`
	TargetMetric             TargetMetric `json:"target_metric" db:"target_metric"`
	MinThrottleRate          int          `json:"min_throttle_rate" db:"min_throttle_rate"`
	MaxThrottleRate          int          `json:"max_throttle_rate" db:"max_throttle_rate"`
	CurrentThrottleRate      int          `json:"current_throttle_rate" db:"current_throttle_rate"`
	LearningPeriodMinutes    int          `json:"learning_period_minutes" db:"learning_period_minutes"`
	ABConfidenceThreshold    float64      `json:"ab_confidence_threshold" db:"ab_confidence_threshold"`
	ABMinSampleSize          int          `json:"ab_min_sample_size" db:"ab_min_sample_size"`
	PauseOnHighComplaints    bool         `json:"pause_on_high_complaints" db:"pause_on_high_complaints"`
	ComplaintThreshold       float64      `json:"complaint_threshold" db:"complaint_threshold"`
	BounceThreshold          float64      `json:"bounce_threshold" db:"bounce_threshold"`
	CreatedAt                time.Time    `json:"created_at" db:"created_at"`
	UpdatedAt                time.Time    `json:"updated_at" db:"updated_at"`
}

// RealtimeMetrics represents a real-time metrics snapshot
type RealtimeMetrics struct {
	ID                   uuid.UUID  `json:"id" db:"id"`
	CampaignID           uuid.UUID  `json:"campaign_id" db:"campaign_id"`
	Timestamp            time.Time  `json:"timestamp" db:"timestamp"`
	IntervalStart        time.Time  `json:"interval_start" db:"interval_start"`
	IntervalEnd          time.Time  `json:"interval_end" db:"interval_end"`
	SentCount            int        `json:"sent_count" db:"sent_count"`
	DeliveredCount       int        `json:"delivered_count" db:"delivered_count"`
	OpenCount            int        `json:"open_count" db:"open_count"`
	UniqueOpenCount      int        `json:"unique_open_count" db:"unique_open_count"`
	ClickCount           int        `json:"click_count" db:"click_count"`
	UniqueClickCount     int        `json:"unique_click_count" db:"unique_click_count"`
	BounceCount          int        `json:"bounce_count" db:"bounce_count"`
	HardBounceCount      int        `json:"hard_bounce_count" db:"hard_bounce_count"`
	SoftBounceCount      int        `json:"soft_bounce_count" db:"soft_bounce_count"`
	ComplaintCount       int        `json:"complaint_count" db:"complaint_count"`
	UnsubscribeCount     int        `json:"unsubscribe_count" db:"unsubscribe_count"`
	OpenRate             float64    `json:"open_rate" db:"open_rate"`
	ClickRate            float64    `json:"click_rate" db:"click_rate"`
	BounceRate           float64    `json:"bounce_rate" db:"bounce_rate"`
	ComplaintRate        float64    `json:"complaint_rate" db:"complaint_rate"`
	CumulativeSent       int        `json:"cumulative_sent" db:"cumulative_sent"`
	CumulativeOpens      int        `json:"cumulative_opens" db:"cumulative_opens"`
	CumulativeClicks     int        `json:"cumulative_clicks" db:"cumulative_clicks"`
	CumulativeBounces    int        `json:"cumulative_bounces" db:"cumulative_bounces"`
	CumulativeComplaints int        `json:"cumulative_complaints" db:"cumulative_complaints"`
	CurrentThrottleRate  int        `json:"current_throttle_rate" db:"current_throttle_rate"`
	ThrottleUtilization  float64    `json:"throttle_utilization" db:"throttle_utilization"`
	AIRecommendation     string     `json:"ai_recommendation" db:"ai_recommendation"`
	AIConfidence         float64    `json:"ai_confidence" db:"ai_confidence"`
	CreatedAt            time.Time  `json:"created_at" db:"created_at"`
}

// InboxProfile represents learned behavior patterns for a subscriber
type InboxProfile struct {
	ID                      uuid.UUID  `json:"id" db:"id"`
	EmailHash               string     `json:"email_hash" db:"email_hash"`
	Domain                  string     `json:"domain" db:"domain"`
	ISP                     string     `json:"isp" db:"isp"`
	OptimalSendHour         *int       `json:"optimal_send_hour" db:"optimal_send_hour"`
	OptimalSendDay          *int       `json:"optimal_send_day" db:"optimal_send_day"`
	OptimalSendHourConfidence float64  `json:"optimal_send_hour_confidence" db:"optimal_send_hour_confidence"`
	AvgOpenDelayMinutes     *int       `json:"avg_open_delay_minutes" db:"avg_open_delay_minutes"`
	AvgClickDelayMinutes    *int       `json:"avg_click_delay_minutes" db:"avg_click_delay_minutes"`
	EngagementScore         float64    `json:"engagement_score" db:"engagement_score"`
	EngagementTrend         string     `json:"engagement_trend" db:"engagement_trend"`
	LastOpenAt              *time.Time `json:"last_open_at" db:"last_open_at"`
	LastClickAt             *time.Time `json:"last_click_at" db:"last_click_at"`
	LastSendAt              *time.Time `json:"last_send_at" db:"last_send_at"`
	TotalSends              int        `json:"total_sends" db:"total_sends"`
	TotalOpens              int        `json:"total_opens" db:"total_opens"`
	TotalClicks             int        `json:"total_clicks" db:"total_clicks"`
	TotalBounces            int        `json:"total_bounces" db:"total_bounces"`
	TotalComplaints         int        `json:"total_complaints" db:"total_complaints"`
	HourlyOpenHistogram     JSON       `json:"hourly_open_histogram" db:"hourly_open_histogram"`
	DailyOpenHistogram      JSON       `json:"daily_open_histogram" db:"daily_open_histogram"`
	FirstSeenAt             time.Time  `json:"first_seen_at" db:"first_seen_at"`
	UpdatedAt               time.Time  `json:"updated_at" db:"updated_at"`
}

// AIDecision represents an AI decision log entry
type AIDecision struct {
	ID              uuid.UUID    `json:"id" db:"id"`
	CampaignID      uuid.UUID    `json:"campaign_id" db:"campaign_id"`
	OrganizationID  *uuid.UUID   `json:"organization_id" db:"organization_id"`
	DecisionType    DecisionType `json:"decision_type" db:"decision_type"`
	DecisionReason  string       `json:"decision_reason" db:"decision_reason"`
	OldValue        string       `json:"old_value" db:"old_value"`
	NewValue        string       `json:"new_value" db:"new_value"`
	MetricsSnapshot JSON         `json:"metrics_snapshot" db:"metrics_snapshot"`
	AIModel         string       `json:"ai_model" db:"ai_model"`
	Confidence      float64      `json:"confidence" db:"confidence"`
	Applied         bool         `json:"applied" db:"applied"`
	AppliedAt       *time.Time   `json:"applied_at" db:"applied_at"`
	Reverted        bool         `json:"reverted" db:"reverted"`
	RevertedAt      *time.Time   `json:"reverted_at" db:"reverted_at"`
	RevertReason    string       `json:"revert_reason" db:"revert_reason"`
	CreatedAt       time.Time    `json:"created_at" db:"created_at"`
}

// ABVariant represents an A/B test variant
type ABVariant struct {
	ID                uuid.UUID     `json:"id" db:"id"`
	CampaignID        uuid.UUID     `json:"campaign_id" db:"campaign_id"`
	VariantName       string        `json:"variant_name" db:"variant_name"`
	VariantType       string        `json:"variant_type" db:"variant_type"`
	VariantValue      string        `json:"variant_value" db:"variant_value"`
	TrafficPercentage int           `json:"traffic_percentage" db:"traffic_percentage"`
	SentCount         int           `json:"sent_count" db:"sent_count"`
	DeliveredCount    int           `json:"delivered_count" db:"delivered_count"`
	OpenCount         int           `json:"open_count" db:"open_count"`
	UniqueOpenCount   int           `json:"unique_open_count" db:"unique_open_count"`
	ClickCount        int           `json:"click_count" db:"click_count"`
	UniqueClickCount  int           `json:"unique_click_count" db:"unique_click_count"`
	ConversionCount   int           `json:"conversion_count" db:"conversion_count"`
	Revenue           float64       `json:"revenue" db:"revenue"`
	OpenRate          float64       `json:"open_rate" db:"open_rate"`
	ClickRate         float64       `json:"click_rate" db:"click_rate"`
	ConversionRate    float64       `json:"conversion_rate" db:"conversion_rate"`
	RevenuePerSend    float64       `json:"revenue_per_send" db:"revenue_per_send"`
	ZScore            float64       `json:"z_score" db:"z_score"`
	PValue            float64       `json:"p_value" db:"p_value"`
	ConfidenceLevel   float64       `json:"confidence_level" db:"confidence_level"`
	IsWinner          bool          `json:"is_winner" db:"is_winner"`
	IsControl         bool          `json:"is_control" db:"is_control"`
	Status            VariantStatus `json:"status" db:"status"`
	DeclaredWinnerAt  *time.Time    `json:"declared_winner_at" db:"declared_winner_at"`
	CreatedAt         time.Time     `json:"created_at" db:"created_at"`
	UpdatedAt         time.Time     `json:"updated_at" db:"updated_at"`
}

// CampaignAlert represents an anomaly alert
type CampaignAlert struct {
	ID              uuid.UUID     `json:"id" db:"id"`
	CampaignID      uuid.UUID     `json:"campaign_id" db:"campaign_id"`
	OrganizationID  *uuid.UUID    `json:"organization_id" db:"organization_id"`
	AlertType       AlertType     `json:"alert_type" db:"alert_type"`
	Severity        AlertSeverity `json:"severity" db:"severity"`
	Title           string        `json:"title" db:"title"`
	Message         string        `json:"message" db:"message"`
	MetricsSnapshot JSON          `json:"metrics_snapshot" db:"metrics_snapshot"`
	ThresholdValue  float64       `json:"threshold_value" db:"threshold_value"`
	ActualValue     float64       `json:"actual_value" db:"actual_value"`
	Acknowledged    bool          `json:"acknowledged" db:"acknowledged"`
	AcknowledgedBy  *uuid.UUID    `json:"acknowledged_by" db:"acknowledged_by"`
	AcknowledgedAt  *time.Time    `json:"acknowledged_at" db:"acknowledged_at"`
	AutoActionTaken string        `json:"auto_action_taken" db:"auto_action_taken"`
	CreatedAt       time.Time     `json:"created_at" db:"created_at"`
}

// DomainSendTime represents optimal send times for a domain
type DomainSendTime struct {
	ID                    uuid.UUID  `json:"id" db:"id"`
	Domain                string     `json:"domain" db:"domain"`
	ISP                   string     `json:"isp" db:"isp"`
	WeekdayOptimalHours   JSON       `json:"weekday_optimal_hours" db:"weekday_optimal_hours"`
	WeekendOptimalHours   JSON       `json:"weekend_optimal_hours" db:"weekend_optimal_hours"`
	HourlyEngagementScores JSON      `json:"hourly_engagement_scores" db:"hourly_engagement_scores"`
	SampleSize            int        `json:"sample_size" db:"sample_size"`
	AvgOpenRate           float64    `json:"avg_open_rate" db:"avg_open_rate"`
	AvgClickRate          float64    `json:"avg_click_rate" db:"avg_click_rate"`
	LastCalculatedAt      *time.Time `json:"last_calculated_at" db:"last_calculated_at"`
	CreatedAt             time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at" db:"updated_at"`
}

// ============================================================================
// REQUEST/RESPONSE TYPES
// ============================================================================

// CreateAISettingsRequest represents a request to create/update AI settings
type CreateAISettingsRequest struct {
	CampaignID                 uuid.UUID    `json:"campaign_id"`
	EnableSmartSending         *bool        `json:"enable_smart_sending,omitempty"`
	EnableThrottleOptimization *bool        `json:"enable_throttle_optimization,omitempty"`
	EnableSendTimeOptimization *bool        `json:"enable_send_time_optimization,omitempty"`
	EnableABAutoWinner         *bool        `json:"enable_ab_auto_winner,omitempty"`
	TargetMetric               TargetMetric `json:"target_metric,omitempty"`
	MinThrottleRate            *int         `json:"min_throttle_rate,omitempty"`
	MaxThrottleRate            *int         `json:"max_throttle_rate,omitempty"`
	LearningPeriodMinutes      *int         `json:"learning_period_minutes,omitempty"`
	ABConfidenceThreshold      *float64     `json:"ab_confidence_threshold,omitempty"`
	ABMinSampleSize            *int         `json:"ab_min_sample_size,omitempty"`
	PauseOnHighComplaints      *bool        `json:"pause_on_high_complaints,omitempty"`
	ComplaintThreshold         *float64     `json:"complaint_threshold,omitempty"`
	BounceThreshold            *float64     `json:"bounce_threshold,omitempty"`
}

// ThrottleOptimizationResult represents the result of throttle optimization
type ThrottleOptimizationResult struct {
	CampaignID       uuid.UUID `json:"campaign_id"`
	PreviousRate     int       `json:"previous_rate"`
	NewRate          int       `json:"new_rate"`
	ChangePercentage float64   `json:"change_percentage"`
	Reason           string    `json:"reason"`
	Confidence       float64   `json:"confidence"`
	Metrics          JSON      `json:"metrics"`
}

// OptimalSendTimeResult represents optimal send time calculation result
type OptimalSendTimeResult struct {
	Email           string    `json:"email"`
	OptimalTime     time.Time `json:"optimal_time"`
	OptimalHourUTC  int       `json:"optimal_hour_utc"`
	Confidence      float64   `json:"confidence"`
	Source          string    `json:"source"` // subscriber, domain, default
	EngagementScore float64   `json:"engagement_score"`
}

// ABTestAnalysis represents A/B test analysis results
type ABTestAnalysis struct {
	CampaignID       uuid.UUID    `json:"campaign_id"`
	TotalVariants    int          `json:"total_variants"`
	SampleSize       int          `json:"sample_size"`
	LearningComplete bool         `json:"learning_complete"`
	WinnerDeclared   bool         `json:"winner_declared"`
	WinnerVariant    *ABVariant   `json:"winner_variant,omitempty"`
	Variants         []*ABVariant `json:"variants"`
	Analysis         string       `json:"analysis"`
	Recommendation   string       `json:"recommendation"`
}

// MetricsTrend represents a trend in metrics over time
type MetricsTrend struct {
	MetricName    string    `json:"metric_name"`
	CurrentValue  float64   `json:"current_value"`
	PreviousValue float64   `json:"previous_value"`
	ChangePercent float64   `json:"change_percent"`
	Trend         string    `json:"trend"` // increasing, decreasing, stable
	DataPoints    []float64 `json:"data_points"`
	Timestamps    []int64   `json:"timestamps"`
}

// CampaignHealthScore represents overall campaign health
type CampaignHealthScore struct {
	CampaignID       uuid.UUID       `json:"campaign_id"`
	OverallScore     float64         `json:"overall_score"` // 0-100
	DeliverabilityScore float64      `json:"deliverability_score"`
	EngagementScore  float64         `json:"engagement_score"`
	ReputationScore  float64         `json:"reputation_score"`
	Issues           []string        `json:"issues"`
	Recommendations  []string        `json:"recommendations"`
	Trends           []MetricsTrend  `json:"trends"`
	LastUpdated      time.Time       `json:"last_updated"`
}

// AIOptimizationSummary represents a summary of AI optimizations
type AIOptimizationSummary struct {
	CampaignID          uuid.UUID     `json:"campaign_id"`
	TotalDecisions      int           `json:"total_decisions"`
	ThrottleAdjustments int           `json:"throttle_adjustments"`
	SendTimeOptimized   int           `json:"send_time_optimized"`
	ABTestsAnalyzed     int           `json:"ab_tests_analyzed"`
	AlertsTriggered     int           `json:"alerts_triggered"`
	EstimatedLift       float64       `json:"estimated_lift"` // % improvement
	RecentDecisions     []*AIDecision `json:"recent_decisions"`
}

// SparkPostEvent represents a SparkPost webhook event for processing
type SparkPostWebhookEvent struct {
	Type        string    `json:"type"`
	MessageID   string    `json:"message_id"`
	CampaignID  string    `json:"campaign_id"`
	Recipient   string    `json:"recipient"`
	Timestamp   time.Time `json:"timestamp"`
	BounceClass string    `json:"bounce_class,omitempty"`
	RawReason   string    `json:"raw_reason,omitempty"`
}

// MetricsWindow represents metrics for a time window
type MetricsWindow struct {
	WindowStart      time.Time `json:"window_start"`
	WindowEnd        time.Time `json:"window_end"`
	DurationMinutes  int       `json:"duration_minutes"`
	SentCount        int       `json:"sent_count"`
	DeliveredCount   int       `json:"delivered_count"`
	OpenCount        int       `json:"open_count"`
	ClickCount       int       `json:"click_count"`
	BounceCount      int       `json:"bounce_count"`
	ComplaintCount   int       `json:"complaint_count"`
	OpenRate         float64   `json:"open_rate"`
	ClickRate        float64   `json:"click_rate"`
	BounceRate       float64   `json:"bounce_rate"`
	ComplaintRate    float64   `json:"complaint_rate"`
	SendRate         float64   `json:"send_rate_per_hour"`
}

// ThrottleRecommendation represents AI throttle recommendation
type ThrottleRecommendation struct {
	Action           string  `json:"action"` // increase, decrease, maintain, pause
	NewRate          int     `json:"new_rate"`
	Reason           string  `json:"reason"`
	Confidence       float64 `json:"confidence"`
	RiskLevel        string  `json:"risk_level"` // low, medium, high
	ExpectedImpact   string  `json:"expected_impact"`
}
