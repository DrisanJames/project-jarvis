package api

import "time"

// ---------------------------------------------------------------------------
// Dashboard DTOs
// ---------------------------------------------------------------------------

// DashboardResponse is the typed contract for GET /api/mailing/dashboard.
// Every field the frontend reads must be present here. New fields are additive.
type DashboardResponse struct {
	Overview        DashboardOverview         `json:"overview"`
	Performance     DashboardPerformance      `json:"performance"`
	RecentCampaigns []DashboardRecentCampaign `json:"recent_campaigns"`
	ThrottleStatus  ThrottleStatus            `json:"throttle_status"`

	// Org-scoped counts
	TotalSuppressions       int `json:"total_suppressions"`
	GlobalSuppressionsTotal int `json:"global_suppressions_total"`
	SuppressionsToday       int `json:"suppressions_today"`
	SuppressionsYesterday   int `json:"suppressions_yesterday"`
	ActiveAutomations       int `json:"active_automations"`

	// Org-scoped subscriber total
	TotalSubscribers int `json:"total_subscribers"`

	// Platform-wide infrastructure capacity (shared, not org-owned).
	// platform_daily_sent is the TRUE numerator for the platform gauge: it sums
	// every org's sends today. org_daily_sent is the calling org's contribution.
	// daily_used remains for backward-compat clients (= org_daily_sent).
	// daily_remaining = capacity − platform_daily_sent.
	PlatformDailyCapacity    int64   `json:"platform_daily_capacity"`
	PlatformDailySent        int64   `json:"platform_daily_sent"`
	OrgDailySent             int64   `json:"org_daily_sent"`
	DailyUsed                int64   `json:"daily_used"` // alias for org_daily_sent (back-compat)
	PlatformDailyUtilization float64 `json:"platform_daily_utilization"`
	DailyRemaining           int64   `json:"daily_remaining"`

	// PMTA infrastructure
	PMTAConnected   bool `json:"pmta_connected"`
	PMTAServerCount int  `json:"pmta_server_count"`

	// Global-scope data lives ONLY in platform_intelligence.
	// No top-level duplicates — clients must read from here.
	PlatformIntelligence *PlatformIntelligence `json:"platform_intelligence,omitempty"`

	// Observability
	APIVersion    string            `json:"api_version"`
	SectionErrors map[string]string `json:"section_errors,omitempty"`
	MetricSources map[string]string `json:"metric_sources"`
	GeneratedAt   time.Time         `json:"generated_at"`
}

type DashboardOverview struct {
	TotalSubscribers         int     `json:"total_subscribers"`
	TotalLists               int     `json:"total_lists"`
	TotalCampaigns           int     `json:"total_campaigns"`
	SuppressedEmails         int     `json:"suppressed_emails"`
	PlatformDailyCapacity    int64   `json:"platform_daily_capacity"`
	PlatformDailySent        int64   `json:"platform_daily_sent"`
	OrgDailySent             int64   `json:"org_daily_sent"`
	PlatformDailyUtilization float64 `json:"platform_daily_utilization"`
}

type DashboardPerformance struct {
	TotalSent    int     `json:"total_sent"`
	TotalOpens   int     `json:"total_opens"`
	TotalClicks  int     `json:"total_clicks"`
	TotalRevenue float64 `json:"total_revenue"`
	// 0-1 fractions (frontend expects this range)
	OpenRate  float64 `json:"open_rate"`
	ClickRate float64 `json:"click_rate"`
	// 0-100 percentages (from ComputeMetrics)
	HardBounces    int     `json:"hard_bounces"`
	SoftBounces    int     `json:"soft_bounces"`
	Complaints     int     `json:"complaints"`
	Delivered      int     `json:"delivered"`
	HardBounceRate float64 `json:"hard_bounce_rate"`
	SoftBounceRate float64 `json:"soft_bounce_rate"`
	ComplaintRate  float64 `json:"complaint_rate"`
	DeliveryRate   float64 `json:"delivery_rate"`
}

type DashboardRecentCampaign struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	SentCount  int       `json:"sent_count"`
	OpenCount  int       `json:"open_count"`
	ClickCount int       `json:"click_count"`
	CreatedAt  time.Time `json:"created_at"`
}

type ThrottleStatus struct {
	MinuteUsed  int `json:"minute_used"`
	MinuteLimit int `json:"minute_limit"`
	HourUsed    int `json:"hour_used"`
	HourLimit   int `json:"hour_limit"`
}

// PlatformIntelligence holds metrics that are global (not org-scoped)
// because the underlying tables lack an organization_id column.
type PlatformIntelligence struct {
	Scope              string     `json:"scope"` // always "global"
	InboxProfiles      int        `json:"inbox_profiles"`
	InboxProfilesToday int        `json:"inbox_profiles_today"`
	ActiveAudience60d  int        `json:"active_audience_60d,omitempty"`
	GlobalChurnPct     float64    `json:"global_churn_pct,omitempty"`
	GlobalIntroPct     float64    `json:"global_intro_pct,omitempty"`
	MetricsComputedAt  *time.Time `json:"metrics_computed_at,omitempty"`
}

// ---------------------------------------------------------------------------
// Campaign DTOs
// ---------------------------------------------------------------------------

type CampaignListItem struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Subject          string    `json:"subject"`
	FromName         string    `json:"from_name"`
	FromEmail        string    `json:"from_email"`
	Status           string    `json:"status"`
	SentCount        int       `json:"sent_count"`
	OpenCount        int       `json:"open_count"`
	ClickCount       int       `json:"click_count"`
	BounceCount      int       `json:"bounce_count"`
	HardBounceCount       int `json:"hard_bounce_count"`
	SoftBounceCount       int `json:"soft_bounce_count"`
	// DerivedDeliveredCount is COALESCE(delivered_count, sent - bounce).
	// When the column is NULL, this is an approximation — not ground truth.
	DerivedDeliveredCount int `json:"derived_delivered_count"`
	ComplaintCount        int `json:"complaint_count"`
	UnsubscribeCount int       `json:"unsubscribe_count"`
	Revenue          float64   `json:"revenue"`
	CreatedAt        time.Time `json:"created_at"`
}

type CampaignListResponse struct {
	Campaigns  []CampaignListItem `json:"campaigns"`
	Total      int64              `json:"total"`
	Pagination *PaginationMeta    `json:"pagination,omitempty"`
}

type CampaignCreateResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Subject   string `json:"subject"`
	FromName  string `json:"from_name"`
	FromEmail string `json:"from_email"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

// ---------------------------------------------------------------------------
// List DTOs
// ---------------------------------------------------------------------------

type ListItem struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Description     string    `json:"description"`
	SubscriberCount int       `json:"subscriber_count"`
	ActiveCount     int       `json:"active_count"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
	MailedToCount   int       `json:"mailed_to_count"`
	OpenPct         float64   `json:"open_pct"`
	ClickPct        float64   `json:"click_pct"`
	ComplaintPct    float64   `json:"complaint_pct"`
}

type ListsResponse struct {
	Lists []ListItem `json:"lists"`
	Total int        `json:"total"`
}

type ListCreateResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
}

// ---------------------------------------------------------------------------
// Sending Plan DTOs
// ---------------------------------------------------------------------------

type PlanPredictions struct {
	EstimatedOpens   int     `json:"estimated_opens"`
	EstimatedClicks  int     `json:"estimated_clicks"`
	EstimatedRevenue float64 `json:"estimated_revenue"`
}

type SendingPlan struct {
	TimePeriod        string          `json:"time_period"`
	Name              string          `json:"name"`
	Description       string          `json:"description"`
	RecommendedVolume int             `json:"recommended_volume"`
	Predictions       PlanPredictions `json:"predictions"`
	ConfidenceScore   float64         `json:"confidence_score"`
	AIExplanation     string          `json:"ai_explanation"`
	Explanation       string          `json:"explanation"`
	Recommendations   []string        `json:"recommendations"`
	Warnings          []string        `json:"warnings,omitempty"`
}

type SendingPlanResponse struct {
	Plans       []SendingPlan `json:"plans"`
	TargetDate  string        `json:"target_date"`
	GeneratedAt string        `json:"generated_at"`
}

// ---------------------------------------------------------------------------
// Delivery Server DTOs
// ---------------------------------------------------------------------------

type DeliveryServerItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ServerType  string `json:"server_type"`
	HourlyQuota int    `json:"hourly_quota"`
	DailyQuota  int    `json:"daily_quota"`
	Status      string `json:"status"`
}

type DeliveryServersResponse struct {
	Servers []DeliveryServerItem `json:"servers"`
	Total   int                  `json:"total"`
}
