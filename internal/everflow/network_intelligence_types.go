package everflow

import "time"

// ========== Network Intelligence Types ==========
// These types support network-wide offer analytics and AI-driven audience matching

// InboxProvider represents an inferred email inbox provider
type InboxProvider string

const (
	InboxGmail      InboxProvider = "gmail"
	InboxYahoo      InboxProvider = "yahoo"
	InboxOutlook    InboxProvider = "outlook"
	InboxAppleMail  InboxProvider = "apple_mail"
	InboxAOL        InboxProvider = "aol"
	InboxOther      InboxProvider = "other"
)

// UserAgentProfile represents parsed user agent intelligence
type UserAgentProfile struct {
	Browser        string `json:"browser"`
	BrowserVersion string `json:"browser_version,omitempty"`
	OS             string `json:"os"`
	OSVersion      string `json:"os_version,omitempty"`
	DeviceType     string `json:"device_type"`    // desktop, mobile, tablet
	DeviceModel    string `json:"device_model,omitempty"`
	Brand          string `json:"brand,omitempty"`
	IsChromium     bool   `json:"is_chromium"`     // Chrome, Edge, Brave, etc.
	IsWebKit       bool   `json:"is_webkit"`       // Safari-based
	IsMozilla      bool   `json:"is_mozilla"`      // Firefox
	IsOutlook      bool   `json:"is_outlook"`      // Outlook client
}

// AudienceProfile represents the aggregated audience characteristics for an offer
// derived from analyzing click and conversion metadata across the network
type AudienceProfile struct {
	// Inbox Provider Distribution (inferred from user agent + ISP)
	InboxDistribution map[InboxProvider]float64 `json:"inbox_distribution"` // provider -> percentage
	PrimaryInbox      InboxProvider             `json:"primary_inbox"`      // dominant inbox provider
	PrimaryInboxPct   float64                   `json:"primary_inbox_pct"`  // percentage of primary

	// Browser Distribution
	BrowserDistribution map[string]float64 `json:"browser_distribution"` // browser -> percentage
	ChromiumPercentage  float64            `json:"chromium_percentage"`  // % using Chromium-based browsers

	// Device Distribution
	DeviceDistribution map[string]float64 `json:"device_distribution"` // desktop/mobile/tablet -> percentage
	PrimaryDevice      string             `json:"primary_device"`

	// OS Distribution
	OSDistribution map[string]float64 `json:"os_distribution"` // os -> percentage

	// Geographic Distribution (top regions)
	TopCountries []GeoDistribution `json:"top_countries"`
	TopRegions   []GeoDistribution `json:"top_regions"`

	// ISP Distribution (from conversion records)
	ISPDistribution map[string]float64 `json:"isp_distribution,omitempty"`

	// Timing Patterns
	PeakConversionHours []int   `json:"peak_conversion_hours"` // hours in UTC when most conversions happen
	BestDayOfWeek       string  `json:"best_day_of_week"`
	PeakHourUTC         int     `json:"peak_hour_utc"`

	// Sample Size
	TotalSamples int64 `json:"total_samples"` // clicks+conversions analyzed
}

// GeoDistribution represents geographic distribution data
type GeoDistribution struct {
	Name       string  `json:"name"`
	Count      int64   `json:"count"`
	Percentage float64 `json:"percentage"`
}

// NetworkOfferIntelligence represents comprehensive intelligence about an offer across the entire network
type NetworkOfferIntelligence struct {
	OfferID        string  `json:"offer_id"`
	OfferName      string  `json:"offer_name"`
	OfferType      string  `json:"offer_type"` // CPM, CPL, CPA, etc.
	AdvertiserName string  `json:"advertiser_name,omitempty"`

	// Network-Wide Performance (all affiliates)
	NetworkClicks      int64   `json:"network_clicks"`
	NetworkConversions int64   `json:"network_conversions"`
	NetworkRevenue     float64 `json:"network_revenue"`
	NetworkCVR         float64 `json:"network_cvr"`
	NetworkEPC         float64 `json:"network_epc"`

	// Today's Network Performance
	TodayClicks      int64   `json:"today_clicks"`
	TodayConversions int64   `json:"today_conversions"`
	TodayRevenue     float64 `json:"today_revenue"`
	TodayEPC         float64 `json:"today_epc"`

	// Velocity / Trend
	RevenueTrend    string  `json:"revenue_trend"`    // "accelerating", "stable", "decelerating"
	TrendPercentage float64 `json:"trend_percentage"` // compared to 7-day avg
	HourlyVelocity  float64 `json:"hourly_velocity"`  // revenue per hour today

	// Audience Profile (derived from click/conversion metadata)
	AudienceProfile *AudienceProfile `json:"audience_profile,omitempty"`

	// AI Scoring
	AIScore          float64 `json:"ai_score"`
	AIRecommendation string  `json:"ai_recommendation"` // highly_recommended, recommended, neutral, caution
	AIReason         string  `json:"ai_reason"`

	// Rank across network
	NetworkRank int `json:"network_rank"` // 1 = top performer
}

// AudienceMatchRecommendation is an AI-driven recommendation that connects
// a network-performing offer to a specific audience segment in YOUR ecosystem
type AudienceMatchRecommendation struct {
	// The offer
	OfferID   string `json:"offer_id"`
	OfferName string `json:"offer_name"`
	OfferType string `json:"offer_type"`

	// The recommended audience
	TargetAudience     string `json:"target_audience"`      // e.g., "Gmail Engaged Subscribers"
	TargetISP          string `json:"target_isp,omitempty"`  // e.g., "gmail.com"
	TargetSegmentHint  string `json:"target_segment_hint"`   // e.g., "engagement_behavior == 'highly_engaged' AND email LIKE '%@gmail.com'"
	EstimatedAudience  int64  `json:"estimated_audience"`    // estimated subscriber count

	// Why this match
	MatchReasons []string `json:"match_reasons"`
	MatchScore   float64  `json:"match_score"` // 0-100

	// Predictions
	PredictedCVR        float64 `json:"predicted_cvr"`         // estimated conversion rate
	PredictedEPC        float64 `json:"predicted_epc"`
	PredictedRevenue    float64 `json:"predicted_revenue"`     // estimated revenue for this audience
	ConfidenceLevel     float64 `json:"confidence_level"`      // 0-1

	// Strategy
	RecommendedStrategy string   `json:"recommended_strategy"` // "volume", "conversion", "revenue"
	SendTimeHint        string   `json:"send_time_hint"`       // e.g., "9:00 AM - 12:00 PM EST"
	CreativeHints       []string `json:"creative_hints"`       // subject line / content suggestions
}

// NetworkIntelligenceSnapshot represents the full network intelligence state at a point in time
type NetworkIntelligenceSnapshot struct {
	Timestamp          time.Time                      `json:"timestamp"`
	LastUpdated        time.Time                      `json:"last_updated"`
	CollectionDuration time.Duration                  `json:"collection_duration_ms"`

	// Top offers across the entire network today
	TopOffers []NetworkOfferIntelligence `json:"top_offers"`

	// Network-wide stats
	NetworkTotalClicks      int64   `json:"network_total_clicks"`
	NetworkTotalConversions int64   `json:"network_total_conversions"`
	NetworkTotalRevenue     float64 `json:"network_total_revenue"`
	NetworkAvgCVR           float64 `json:"network_avg_cvr"`
	NetworkAvgEPC           float64 `json:"network_avg_epc"`

	// AI-driven audience match recommendations
	AudienceRecommendations []AudienceMatchRecommendation `json:"audience_recommendations"`

	// Metadata analysis summary
	TotalClicksAnalyzed      int64 `json:"total_clicks_analyzed"`
	TotalConversionsAnalyzed int64 `json:"total_conversions_analyzed"`
}
