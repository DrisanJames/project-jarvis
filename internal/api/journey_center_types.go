package api

import "time"

// JourneyCenterOverview represents the dashboard overview response
type JourneyCenterOverview struct {
	TotalJourneys          int                     `json:"total_journeys"`
	ActiveJourneys         int                     `json:"active_journeys"`
	DraftJourneys          int                     `json:"draft_journeys"`
	PausedJourneys         int                     `json:"paused_journeys"`
	TotalActiveEnrollments int                     `json:"total_active_enrollments"`
	EnrollmentsToday       int                     `json:"enrollments_today"`
	CompletionsToday       int                     `json:"completions_today"`
	ConversionsToday       int                     `json:"conversions_today"`
	OverallConversionRate  float64                 `json:"overall_conversion_rate"`
	TopJourneys            []JourneyOverviewItem   `json:"top_journeys"`
	RecentActivity         []JourneyActivityItem   `json:"recent_activity"`
}

// JourneyOverviewItem represents a top-performing journey
type JourneyOverviewItem struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Status         string  `json:"status"`
	ActiveEnrolled int     `json:"active_enrolled"`
	Completed      int     `json:"completed"`
	Converted      int     `json:"converted"`
	ConversionRate float64 `json:"conversion_rate"`
}

// JourneyActivityItem represents recent journey activity
type JourneyActivityItem struct {
	Type       string    `json:"type"` // enrollment, completion, conversion, email_sent
	JourneyID  string    `json:"journey_id"`
	JourneyName string   `json:"journey_name"`
	Email      string    `json:"email,omitempty"`
	NodeID     string    `json:"node_id,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}

// JourneyListItem represents a journey in the list view
type JourneyListItem struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	Description        string          `json:"description,omitempty"`
	Status             string          `json:"status"`
	NodeCount          int             `json:"node_count"`
	ActiveEnrollments  int             `json:"active_enrollments"`
	TotalEnrollments   int             `json:"total_enrollments"`
	CompletionRate     float64         `json:"completion_rate"`
	ConversionRate     float64         `json:"conversion_rate"`
	EmailsSent         int             `json:"emails_sent"`
	OpenRate           float64         `json:"open_rate"`
	ClickRate          float64         `json:"click_rate"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	LastEnrollmentAt   *time.Time      `json:"last_enrollment_at,omitempty"`
}

// JourneyMetrics represents detailed journey metrics
type JourneyMetrics struct {
	JourneyID          string               `json:"journey_id"`
	JourneyName        string               `json:"journey_name"`
	Status             string               `json:"status"`
	TotalEnrollments   int                  `json:"total_enrollments"`
	ActiveEnrollments  int                  `json:"active_enrollments"`
	CompletedCount     int                  `json:"completed_count"`
	ConvertedCount     int                  `json:"converted_count"`
	ExitedCount        int                  `json:"exited_count"`
	CompletionRate     float64              `json:"completion_rate"`
	ConversionRate     float64              `json:"conversion_rate"`
	AverageTimeToComplete string            `json:"avg_time_to_complete"`
	EmailMetrics       EmailMetricsSummary  `json:"email_metrics"`
	NodeMetrics        []NodeMetric         `json:"node_metrics"`
	HourlyDistribution []HourlyMetric       `json:"hourly_distribution"`
}

// EmailMetricsSummary summarizes email performance
type EmailMetricsSummary struct {
	TotalSent     int     `json:"total_sent"`
	TotalOpens    int     `json:"total_opens"`
	UniqueOpens   int     `json:"unique_opens"`
	TotalClicks   int     `json:"total_clicks"`
	UniqueClicks  int     `json:"unique_clicks"`
	Bounces       int     `json:"bounces"`
	Unsubscribes  int     `json:"unsubscribes"`
	OpenRate      float64 `json:"open_rate"`
	ClickRate     float64 `json:"click_rate"`
	ClickToOpen   float64 `json:"click_to_open_rate"`
	BounceRate    float64 `json:"bounce_rate"`
}

// NodeMetric represents metrics for a specific journey node
type NodeMetric struct {
	NodeID       string  `json:"node_id"`
	NodeType     string  `json:"node_type"`
	NodeName     string  `json:"node_name"`
	Entered      int     `json:"entered"`
	Completed    int     `json:"completed"`
	Exited       int     `json:"exited"`
	AvgTimeSpent string  `json:"avg_time_spent"`
	Conversions  int     `json:"conversions,omitempty"`
}

// HourlyMetric represents hourly enrollment data
type HourlyMetric struct {
	Hour        int `json:"hour"`
	Enrollments int `json:"enrollments"`
	Completions int `json:"completions"`
}

// JourneyFunnelResponse represents the funnel analysis
type JourneyFunnelResponse struct {
	JourneyID   string        `json:"journey_id"`
	JourneyName string        `json:"journey_name"`
	TotalStart  int           `json:"total_start"`
	FunnelSteps []FunnelStep  `json:"funnel_steps"`
}

// FunnelStep represents a step in the journey funnel
type FunnelStep struct {
	StepNumber     int     `json:"step_number"`
	NodeID         string  `json:"node_id"`
	NodeType       string  `json:"node_type"`
	NodeName       string  `json:"node_name"`
	Entered        int     `json:"entered"`
	Completed      int     `json:"completed"`
	DroppedOff     int     `json:"dropped_off"`
	DropOffRate    float64 `json:"drop_off_rate"`
	ConversionRate float64 `json:"conversion_rate_from_start"`
}

// JourneyTrendsResponse represents historical trends
type JourneyTrendsResponse struct {
	JourneyID  string           `json:"journey_id"`
	Period     string           `json:"period"` // 7d, 30d, 90d
	DataPoints []TrendDataPoint `json:"data_points"`
	Summary    TrendSummary     `json:"summary"`
}

// TrendDataPoint represents a single data point in trends
type TrendDataPoint struct {
	Date         string  `json:"date"`
	Enrollments  int     `json:"enrollments"`
	Completions  int     `json:"completions"`
	Conversions  int     `json:"conversions"`
	EmailsSent   int     `json:"emails_sent"`
	OpenRate     float64 `json:"open_rate"`
	ClickRate    float64 `json:"click_rate"`
}

// TrendSummary summarizes trend data
type TrendSummary struct {
	TotalEnrollments    int     `json:"total_enrollments"`
	EnrollmentTrend     float64 `json:"enrollment_trend_pct"` // % change from previous period
	TotalCompletions    int     `json:"total_completions"`
	CompletionTrend     float64 `json:"completion_trend_pct"`
	TotalConversions    int     `json:"total_conversions"`
	ConversionTrend     float64 `json:"conversion_trend_pct"`
	AvgDailyEnrollments float64 `json:"avg_daily_enrollments"`
}

// EnrollmentListItem represents an enrollment in the list
type EnrollmentListItem struct {
	ID              string                 `json:"id"`
	Email           string                 `json:"email"`
	SubscriberID    string                 `json:"subscriber_id,omitempty"`
	Status          string                 `json:"status"` // active, completed, converted, exited
	CurrentNodeID   string                 `json:"current_node_id"`
	CurrentNodeName string                 `json:"current_node_name"`
	Progress        float64                `json:"progress"` // 0-100%
	EnrolledAt      time.Time              `json:"enrolled_at"`
	CompletedAt     *time.Time             `json:"completed_at,omitempty"`
	ConvertedAt     *time.Time             `json:"converted_at,omitempty"`
	LastActivityAt  *time.Time             `json:"last_activity_at,omitempty"`
	Metadata        map[string]interface{} `json:"metadata,omitempty"`
}

// EnrollmentDetail represents detailed enrollment info with execution history
type EnrollmentDetail struct {
	EnrollmentListItem
	JourneyID        string                   `json:"journey_id"`
	JourneyName      string                   `json:"journey_name"`
	ExecutionHistory []ExecutionHistoryItem   `json:"execution_history"`
	EmailsReceived   []EnrollmentEmailItem    `json:"emails_received"`
	SubscriberInfo   map[string]interface{}   `json:"subscriber_info,omitempty"`
}

// ExecutionHistoryItem represents a node execution event
type ExecutionHistoryItem struct {
	NodeID       string                 `json:"node_id"`
	NodeType     string                 `json:"node_type"`
	NodeName     string                 `json:"node_name"`
	Action       string                 `json:"action"` // entered, completed, skipped, failed
	EnteredAt    time.Time              `json:"entered_at"`
	CompletedAt  *time.Time             `json:"completed_at,omitempty"`
	Duration     string                 `json:"duration,omitempty"`
	Details      map[string]interface{} `json:"details,omitempty"`
}

// EnrollmentEmailItem represents an email received during enrollment
type EnrollmentEmailItem struct {
	EmailID    string     `json:"email_id"`
	Subject    string     `json:"subject"`
	SentAt     time.Time  `json:"sent_at"`
	OpenedAt   *time.Time `json:"opened_at,omitempty"`
	ClickedAt  *time.Time `json:"clicked_at,omitempty"`
	Status     string     `json:"status"` // sent, opened, clicked, bounced
}

// SegmentForEnrollment represents a segment available for journey enrollment
type SegmentForEnrollment struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Description     string    `json:"description,omitempty"`
	SubscriberCount int       `json:"subscriber_count"`
	LastCalculated  time.Time `json:"last_calculated"`
}

// JourneyPerformanceItem represents a journey in the performance comparison
type JourneyPerformanceItem struct {
	JourneyID         string  `json:"journey_id"`
	JourneyName       string  `json:"journey_name"`
	Status            string  `json:"status"`
	TotalEnrollments  int     `json:"total_enrollments"`
	CompletionRate    float64 `json:"completion_rate"`
	ConversionRate    float64 `json:"conversion_rate"`
	EmailsSent        int     `json:"emails_sent"`
	OpenRate          float64 `json:"open_rate"`
	ClickRate         float64 `json:"click_rate"`
	RevenueGenerated  float64 `json:"revenue_generated,omitempty"`
	AvgTimeToComplete string  `json:"avg_time_to_complete"`
}
