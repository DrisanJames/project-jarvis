// Package activation provides Data Activation Intelligence for the email marketing
// platform. It analyzes ecosystem sending data across ESPs (SparkPost, Mailgun, SES)
// and ISPs (Gmail, Yahoo, Outlook, AOL, Apple Mail) to generate strategic
// recommendations for warming data, repairing reputation, and activating subscribers.
//
// Built by Jarvis (Opus 4.6) — enhanced by overseer.
package activation

import "time"

// ISP represents a major inbox service provider
type ISP string

const (
	ISPGmail   ISP = "gmail"
	ISPYahoo   ISP = "yahoo"
	ISPOutlook ISP = "outlook"
	ISPAOL     ISP = "aol"
	ISPApple   ISP = "apple"
	ISPOther   ISP = "other"
)

// AllISPs returns all tracked ISPs in priority order
func AllISPs() []ISP {
	return []ISP{ISPGmail, ISPYahoo, ISPOutlook, ISPAOL, ISPApple, ISPOther}
}

// StrategyType categorizes the kind of activation strategy
type StrategyType string

const (
	StrategyWarmup           StrategyType = "domain_warmup"
	StrategyColdReactivation StrategyType = "cold_reactivation"
	StrategyEngagementRamp   StrategyType = "engagement_ramp"
	StrategyComplaintControl StrategyType = "complaint_control"
	StrategyAuthFocus        StrategyType = "authentication_focus"
	StrategyListHygiene      StrategyType = "list_hygiene"
	StrategyRepRepair        StrategyType = "reputation_repair"
	StrategyVolumeScaling    StrategyType = "volume_scaling"
)

// Priority for recommendations
type Priority string

const (
	PriorityCritical Priority = "critical"
	PriorityHigh     Priority = "high"
	PriorityMedium   Priority = "medium"
	PriorityLow      Priority = "low"
)

// RiskLevel represents the risk level of a sending domain or ISP relationship
type RiskLevel string

const (
	RiskCritical RiskLevel = "critical"
	RiskHigh     RiskLevel = "high"
	RiskMedium   RiskLevel = "medium"
	RiskLow      RiskLevel = "low"
	RiskHealthy  RiskLevel = "healthy"
)

// ISPSendingData holds per-ISP sending data aggregated across ESPs
type ISPSendingData struct {
	ISP          ISP   `json:"isp"`
	TotalSent    int64 `json:"total_sent"`
	Delivered    int64 `json:"delivered"`
	Bounced      int64 `json:"bounced"`
	HardBounced  int64 `json:"hard_bounced"`
	SoftBounced  int64 `json:"soft_bounced"`
	Opens        int64 `json:"opens"`
	UniqueOpens  int64 `json:"unique_opens"`
	Clicks       int64 `json:"clicks"`
	UniqueClicks int64 `json:"unique_clicks"`
	Complaints   int64 `json:"complaints"`
	Unsubscribes int64 `json:"unsubscribes"`
	SpamTraps    int64 `json:"spam_traps"`
}

// DataHealthScore represents the computed health of sending data for a specific ISP
type DataHealthScore struct {
	ISP                   ISP       `json:"isp"`
	ISPDisplayName        string    `json:"isp_display_name"`
	OverallScore          float64   `json:"overall_score"`         // 0-100
	BounceRate            float64   `json:"bounce_rate"`           // percentage
	HardBounceRate        float64   `json:"hard_bounce_rate"`      // percentage
	ComplaintRate         float64   `json:"complaint_rate"`        // percentage
	EngagementRate        float64   `json:"engagement_rate"`       // percentage (unique opens / delivered)
	ClickToOpenRate       float64   `json:"click_to_open_rate"`    // percentage
	DeliveryRate          float64   `json:"delivery_rate"`         // percentage
	UnsubscribeRate       float64   `json:"unsubscribe_rate"`      // percentage
	SpamTrapRate          float64   `json:"spam_trap_rate"`        // percentage
	RiskLevel             RiskLevel `json:"risk_level"`
	TotalVolume           int64     `json:"total_volume"`
	BounceScoreImpact     float64   `json:"bounce_score_impact"`     // 0-30
	ComplaintScoreImpact  float64   `json:"complaint_score_impact"`  // 0-35
	EngagementScoreImpact float64   `json:"engagement_score_impact"` // 0-35
	Diagnostics           []string  `json:"diagnostics"`
}

// WarmupPhase defines a single phase in a warmup/activation schedule
type WarmupPhase struct {
	PhaseNumber      int      `json:"phase_number"`
	DurationDays     int      `json:"duration_days"`
	DailyVolume      int64    `json:"daily_volume"`
	VolumePercent    float64  `json:"volume_percent"`     // percent of target volume
	TargetSegment    string   `json:"target_segment"`     // e.g. "most_engaged", "30d_active"
	MaxBounceRate    float64  `json:"max_bounce_rate"`
	MaxComplaintRate float64  `json:"max_complaint_rate"`
	MinEngagement    float64  `json:"min_engagement_rate"`
	Actions          []string `json:"actions"`
	GatingCriteria   []string `json:"gating_criteria"`
}

// ISPActivationStrategy defines a detailed per-ISP activation/warmup plan
type ISPActivationStrategy struct {
	ISP              ISP          `json:"isp"`
	ISPDisplayName   string       `json:"isp_display_name"`
	StrategyType     StrategyType `json:"strategy_type"`
	StrategyName     string       `json:"strategy_name"`
	Description      string       `json:"description"`       // rich multi-paragraph strategy
	KeyActions       []string     `json:"key_actions"`        // ordered action items
	WarmupSchedule   []WarmupPhase `json:"warmup_schedule"`   // phased volume plan
	Risks            []string     `json:"risks"`              // what could go wrong
	SuccessMetrics   []string     `json:"success_metrics"`    // how to measure success
	EstimatedDays    int          `json:"estimated_days"`     // days to full activation
	DifficultyLevel  string       `json:"difficulty_level"`   // easy, moderate, hard
}

// ActivationRecommendation represents a complete recommendation for data activation
type ActivationRecommendation struct {
	ID                string                `json:"id"`
	Title             string                `json:"title"`
	Description       string                `json:"description"`
	Priority          Priority              `json:"priority"`
	ISP               ISP                   `json:"isp"`
	ISPDisplayName    string                `json:"isp_display_name"`
	StrategyType      StrategyType          `json:"strategy_type"`
	HealthScore       DataHealthScore       `json:"health_score"`
	Strategy          ISPActivationStrategy `json:"strategy"`
	ImpactEstimate    string                `json:"impact_estimate"`
	TimelineEstimate  string                `json:"timeline_estimate"`
	CampaignSuggestion CampaignSuggestion   `json:"campaign_suggestion"`
}

// CampaignSuggestion provides a ready-to-use campaign configuration
type CampaignSuggestion struct {
	CampaignName    string   `json:"campaign_name"`
	TargetSegment   string   `json:"target_segment"`
	SegmentCriteria string   `json:"segment_criteria"`
	SubjectLines    []string `json:"subject_lines"`
	SendSchedule    string   `json:"send_schedule"`
	ESPRecommended  string   `json:"esp_recommended"`
	Volume          string   `json:"volume"`
	Notes           string   `json:"notes"`
}

// ActivationSnapshot represents the full data activation intelligence state
type ActivationSnapshot struct {
	Timestamp          time.Time                        `json:"timestamp"`
	HealthScores       map[ISP]DataHealthScore           `json:"health_scores"`
	OverallHealth      float64                           `json:"overall_health"` // weighted average
	OverallRisk        RiskLevel                         `json:"overall_risk"`
	Recommendations    []ActivationRecommendation        `json:"recommendations"`
	ISPStrategies      map[ISP]ISPActivationStrategy     `json:"isp_strategies"`
	TotalSendingVolume int64                             `json:"total_sending_volume"`
	Summary            string                            `json:"summary"`
}

// ISPThresholds defines acceptable thresholds per ISP
type ISPThresholds struct {
	MaxBounceRate     float64
	MaxComplaintRate  float64
	MinEngagementRate float64
	MaxSpamTrapRate   float64
	MaxUnsubRate      float64
}
