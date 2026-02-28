package sparkpost

import (
	"time"
)

// MetricsResponse represents a response from the SparkPost Metrics API
type MetricsResponse struct {
	Results []MetricResult `json:"results"`
	Links   []Link         `json:"links,omitempty"`
}

// MetricResult represents a single metric result
type MetricResult struct {
	// Grouping fields
	Domain                string `json:"domain,omitempty"`
	SendingIP             string `json:"sending_ip,omitempty"`
	IPPool                string `json:"ip_pool,omitempty"`
	SendingDomain         string `json:"sending_domain,omitempty"`
	CampaignID            string `json:"campaign_id,omitempty"`
	TemplateID            string `json:"template_id,omitempty"`
	MailboxProvider       string `json:"mailbox_provider,omitempty"`
	MailboxProviderRegion string `json:"mailbox_provider_region,omitempty"`
	WatchedDomain         string `json:"watched_domain,omitempty"`
	Timestamp             string `json:"ts,omitempty"`

	// Volume metrics
	CountTargeted           int64 `json:"count_targeted"`
	CountInjected           int64 `json:"count_injected"`
	CountSent               int64 `json:"count_sent"`
	CountAccepted           int64 `json:"count_accepted"`
	CountDelivered          int64 `json:"count_delivered"`
	CountDeliveredFirst     int64 `json:"count_delivered_first"`
	CountDeliveredSubsequent int64 `json:"count_delivered_subsequent"`

	// Engagement metrics
	CountRendered                      int64 `json:"count_rendered"`
	CountUniqueRendered                int64 `json:"count_unique_rendered"`
	CountNonprefetchedRendered         int64 `json:"count_nonprefetched_rendered"`
	CountNonprefetchedUniqueRendered   int64 `json:"count_nonprefetched_unique_rendered"`
	CountUniqueConfirmedOpened         int64 `json:"count_unique_confirmed_opened"`
	CountClicked                       int64 `json:"count_clicked"`
	CountUniqueClicked                 int64 `json:"count_unique_clicked"`

	// Bounce metrics
	CountBounce            int64 `json:"count_bounce"`
	CountHardBounce        int64 `json:"count_hard_bounce"`
	CountSoftBounce        int64 `json:"count_soft_bounce"`
	CountBlockBounce       int64 `json:"count_block_bounce"`
	CountAdminBounce       int64 `json:"count_admin_bounce"`
	CountUndeterminedBounce int64 `json:"count_undetermined_bounce"`
	CountInbandBounce      int64 `json:"count_inband_bounce"`
	CountOutofbandBounce   int64 `json:"count_outofband_bounce"`

	// Other metrics
	CountRejected           int64 `json:"count_rejected"`
	CountPolicyRejection    int64 `json:"count_policy_rejection"`
	CountGenerationRejection int64 `json:"count_generation_rejection"`
	CountGenerationFailed   int64 `json:"count_generation_failed"`
	CountDelayed            int64 `json:"count_delayed"`
	CountDelayedFirst       int64 `json:"count_delayed_first"`
	CountSpamComplaint      int64 `json:"count_spam_complaint"`
	CountUnsubscribe        int64 `json:"count_unsubscribe"`

	// Size and timing
	TotalMsgVolume              int64 `json:"total_msg_volume"`
	TotalDeliveryTimeFirst      int64 `json:"total_delivery_time_first"`
	TotalDeliveryTimeSubsequent int64 `json:"total_delivery_time_subsequent"`
}

// Link represents an API link
type Link struct {
	Href   string `json:"href"`
	Rel    string `json:"rel"`
	Method string `json:"method"`
}

// BounceReasonResponse represents bounce reason metrics
type BounceReasonResponse struct {
	Results []BounceReasonResult `json:"results"`
}

// BounceReasonResult represents a bounce reason
type BounceReasonResult struct {
	Reason                 string `json:"reason"`
	Domain                 string `json:"domain,omitempty"`
	BounceClassName        string `json:"bounce_class_name"`
	BounceClassDescription string `json:"bounce_class_description"`
	BounceCategoryID       int    `json:"bounce_category_id"`
	BounceCategoryName     string `json:"bounce_category_name"`
	ClassificationID       int    `json:"classification_id"`
	CountInbandBounce      int64  `json:"count_inband_bounce"`
	CountOutofbandBounce   int64  `json:"count_outofband_bounce"`
	CountBounce            int64  `json:"count_bounce"`
}

// DelayReasonResponse represents delay reason metrics
type DelayReasonResponse struct {
	Results []DelayReasonResult `json:"results"`
}

// DelayReasonResult represents a delay reason
type DelayReasonResult struct {
	Reason            string `json:"reason"`
	Domain            string `json:"domain,omitempty"`
	CountDelayed      int64  `json:"count_delayed"`
	CountDelayedFirst int64  `json:"count_delayed_first"`
}

// RejectionReasonResponse represents rejection reason metrics
type RejectionReasonResponse struct {
	Results []RejectionReasonResult `json:"results"`
}

// RejectionReasonResult represents a rejection reason
type RejectionReasonResult struct {
	Reason              string `json:"reason"`
	Domain              string `json:"domain,omitempty"`
	CountRejected       int64  `json:"count_rejected"`
	RejectionCategoryID int    `json:"rejection_category_id"`
	RejectionType       string `json:"rejection_type"`
}

// ListResponse represents a list of items (domains, IPs, etc.)
type ListResponse struct {
	Results map[string][]string `json:"results"`
}

// ProcessedMetrics represents calculated metrics ready for display/storage
type ProcessedMetrics struct {
	Timestamp    time.Time `json:"timestamp"`
	Source       string    `json:"source"` // e.g., "sparkpost"
	GroupBy      string    `json:"group_by"` // e.g., "mailbox_provider", "sending_ip"
	GroupValue   string    `json:"group_value"` // e.g., "Gmail", "18.236.253.72"
	
	// Raw counts
	Targeted     int64 `json:"targeted"`
	Injected     int64 `json:"injected"`
	Sent         int64 `json:"sent"`
	Delivered    int64 `json:"delivered"`
	Opened       int64 `json:"opened"`
	UniqueOpened int64 `json:"unique_opened"`
	Clicked      int64 `json:"clicked"`
	UniqueClicked int64 `json:"unique_clicked"`
	Bounced      int64 `json:"bounced"`
	HardBounced  int64 `json:"hard_bounced"`
	SoftBounced  int64 `json:"soft_bounced"`
	BlockBounced int64 `json:"block_bounced"`
	Complaints   int64 `json:"complaints"`
	Unsubscribes int64 `json:"unsubscribes"`
	Delayed      int64 `json:"delayed"`
	Rejected     int64 `json:"rejected"`

	// Calculated rates
	DeliveryRate   float64 `json:"delivery_rate"`
	OpenRate       float64 `json:"open_rate"`
	ClickRate      float64 `json:"click_rate"`
	BounceRate     float64 `json:"bounce_rate"`
	HardBounceRate float64 `json:"hard_bounce_rate"`
	SoftBounceRate float64 `json:"soft_bounce_rate"`
	BlockRate      float64 `json:"block_rate"`
	ComplaintRate  float64 `json:"complaint_rate"`
	UnsubscribeRate float64 `json:"unsubscribe_rate"`
}

// CalculateRates calculates rate metrics from raw counts
func (m *ProcessedMetrics) CalculateRates() {
	if m.Sent > 0 {
		m.DeliveryRate = float64(m.Delivered) / float64(m.Sent)
		m.BounceRate = float64(m.Bounced) / float64(m.Sent)
		m.HardBounceRate = float64(m.HardBounced) / float64(m.Sent)
		m.SoftBounceRate = float64(m.SoftBounced) / float64(m.Sent)
		m.BlockRate = float64(m.BlockBounced) / float64(m.Sent)
	}
	if m.Delivered > 0 {
		m.OpenRate = float64(m.UniqueOpened) / float64(m.Delivered)
		m.ClickRate = float64(m.UniqueClicked) / float64(m.Delivered)
		m.ComplaintRate = float64(m.Complaints) / float64(m.Delivered)
		m.UnsubscribeRate = float64(m.Unsubscribes) / float64(m.Delivered)
	}
}

// Summary represents an aggregated summary of metrics
type Summary struct {
	Timestamp     time.Time `json:"timestamp"`
	PeriodStart   time.Time `json:"period_start"`
	PeriodEnd     time.Time `json:"period_end"`
	
	TotalTargeted    int64 `json:"total_targeted"`
	TotalDelivered   int64 `json:"total_delivered"`
	TotalOpened      int64 `json:"total_opened"`
	TotalClicked     int64 `json:"total_clicked"`
	TotalBounced     int64 `json:"total_bounced"`
	TotalComplaints  int64 `json:"total_complaints"`
	TotalUnsubscribes int64 `json:"total_unsubscribes"`

	DeliveryRate    float64 `json:"delivery_rate"`
	OpenRate        float64 `json:"open_rate"`
	ClickRate       float64 `json:"click_rate"`
	BounceRate      float64 `json:"bounce_rate"`
	ComplaintRate   float64 `json:"complaint_rate"`
	UnsubscribeRate float64 `json:"unsubscribe_rate"`

	// Comparisons to previous period
	VolumeChange     float64 `json:"volume_change"`
	DeliveryChange   float64 `json:"delivery_change"`
	OpenRateChange   float64 `json:"open_rate_change"`
	ComplaintChange  float64 `json:"complaint_change"`
}

// ISPMetrics represents metrics for a specific ISP/mailbox provider
type ISPMetrics struct {
	Provider      string           `json:"provider"`
	Metrics       ProcessedMetrics `json:"metrics"`
	Status        string           `json:"status"` // "healthy", "warning", "critical"
	StatusReason  string           `json:"status_reason,omitempty"`
	Trends        MetricTrends     `json:"trends"`
}

// IPMetrics represents metrics for a specific sending IP
type IPMetrics struct {
	IP            string           `json:"ip"`
	Pool          string           `json:"pool"`
	Metrics       ProcessedMetrics `json:"metrics"`
	Status        string           `json:"status"`
	StatusReason  string           `json:"status_reason,omitempty"`
	Trends        MetricTrends     `json:"trends"`
}

// DomainMetrics represents metrics for a specific sending domain
type DomainMetrics struct {
	Domain        string           `json:"domain"`
	Metrics       ProcessedMetrics `json:"metrics"`
	Status        string           `json:"status"`
	StatusReason  string           `json:"status_reason,omitempty"`
	Trends        MetricTrends     `json:"trends"`
}

// RecipientDomainMetrics represents metrics for a specific recipient domain (e.g., att.net, sbcglobal.net)
type RecipientDomainMetrics struct {
	Domain        string           `json:"domain"`
	DisplayName   string           `json:"display_name"`  // Human-friendly name (e.g., "AT&T" for att.net)
	Metrics       ProcessedMetrics `json:"metrics"`
	Status        string           `json:"status"`
	StatusReason  string           `json:"status_reason,omitempty"`
}

// MetricTrends represents trend data for a metric
type MetricTrends struct {
	VolumeDirection    string  `json:"volume_direction"`    // "up", "down", "stable"
	VolumeChangePercent float64 `json:"volume_change_percent"`
	DeliveryTrend      string  `json:"delivery_trend"`
	DeliveryChange     float64 `json:"delivery_change"`
	ComplaintTrend     string  `json:"complaint_trend"`
	ComplaintChange    float64 `json:"complaint_change"`
}

// TimeSeriesPoint represents a single point in a time series
type TimeSeriesPoint struct {
	Timestamp time.Time `json:"timestamp"`
	Value     float64   `json:"value"`
}

// TimeSeries represents a series of time-based data points
type TimeSeries struct {
	Metric string            `json:"metric"`
	Points []TimeSeriesPoint `json:"points"`
}

// SignalsData represents deliverability signals
type SignalsData struct {
	Timestamp      time.Time              `json:"timestamp"`
	BounceReasons  []BounceReasonResult   `json:"bounce_reasons"`
	DelayReasons   []DelayReasonResult    `json:"delay_reasons"`
	RejectionReasons []RejectionReasonResult `json:"rejection_reasons"`
	TopIssues      []Issue                `json:"top_issues"`
}

// Issue represents a deliverability issue
type Issue struct {
	Severity    string `json:"severity"` // "warning", "critical"
	Category    string `json:"category"` // "bounce", "complaint", "block", "delay"
	Description string `json:"description"`
	AffectedISP string `json:"affected_isp,omitempty"`
	AffectedIP  string `json:"affected_ip,omitempty"`
	Count       int64  `json:"count"`
	Recommendation string `json:"recommendation"`
}

// MetricsQuery represents parameters for querying metrics
type MetricsQuery struct {
	From              time.Time
	To                time.Time
	Precision         string // "1min", "5min", "15min", "hour", "day"
	Domains           []string
	SendingIPs        []string
	IPPools           []string
	SendingDomains    []string
	Campaigns         []string
	MailboxProviders  []string
	Limit             int
	OrderBy           string
	Metrics           []string
}

// DefaultMetrics returns the default set of metrics to fetch
func DefaultMetrics() []string {
	return []string{
		"count_targeted",
		"count_injected",
		"count_sent",
		"count_accepted",
		"count_delivered",
		"count_bounce",
		"count_hard_bounce",
		"count_soft_bounce",
		"count_block_bounce",
		"count_rendered",
		"count_unique_rendered",
		"count_clicked",
		"count_unique_clicked",
		"count_spam_complaint",
		"count_unsubscribe",
		"count_rejected",
		"count_delayed",
	}
}

// AllMetrics returns all available metrics
func AllMetrics() []string {
	return []string{
		"count_targeted",
		"count_injected",
		"count_sent",
		"count_accepted",
		"count_delivered",
		"count_delivered_first",
		"count_delivered_subsequent",
		"count_bounce",
		"count_hard_bounce",
		"count_soft_bounce",
		"count_block_bounce",
		"count_admin_bounce",
		"count_undetermined_bounce",
		"count_inband_bounce",
		"count_outofband_bounce",
		"count_rendered",
		"count_unique_rendered",
		"count_nonprefetched_rendered",
		"count_nonprefetched_unique_rendered",
		"count_unique_confirmed_opened",
		"count_clicked",
		"count_unique_clicked",
		"count_spam_complaint",
		"count_unsubscribe",
		"count_rejected",
		"count_policy_rejection",
		"count_generation_rejection",
		"count_generation_failed",
		"count_delayed",
		"count_delayed_first",
		"total_msg_volume",
		"total_delivery_time_first",
		"total_delivery_time_subsequent",
	}
}
