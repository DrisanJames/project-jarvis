package mailgun

import (
	"strings"
	"time"
)

// MetricsRequest represents a request to the Mailgun Metrics API
type MetricsRequest struct {
	Start      string   `json:"start"`                // RFC3339 timestamp
	End        string   `json:"end"`                  // RFC3339 timestamp
	Resolution string   `json:"resolution"`           // "hour", "day", "month"
	Dimensions []string `json:"dimensions,omitempty"` // Group by: "domain", "ip", "provider", etc.
	Metrics    []string `json:"metrics"`              // What metrics to fetch
	Filter     *Filter  `json:"filter,omitempty"`
	Limit      int      `json:"limit,omitempty"`      // Number of results to return (default 100)
	Skip       int      `json:"skip,omitempty"`       // Number of results to skip for pagination
}

// Filter represents filter conditions for the Metrics API
type Filter struct {
	AND []FilterCondition `json:"AND,omitempty"`
}

// FilterCondition represents a single filter condition
type FilterCondition struct {
	Attribute  string         `json:"attribute"`
	Comparator string         `json:"comparator"` // "=", "!=", etc.
	Values     []LabeledValue `json:"values"`
}

// LabeledValue represents a value with label for the Analytics API filter
type LabeledValue struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// MetricsResponse represents a response from the Mailgun Metrics API
type MetricsResponse struct {
	Items      []MetricsItem `json:"items"`
	Dimensions []string      `json:"dimensions"`
	Pagination *Pagination   `json:"pagination,omitempty"`
	Resolution string        `json:"resolution"`
	Start      string        `json:"start"`
	End        string        `json:"end"`
}

// Pagination represents pagination info in API responses
type Pagination struct {
	Total int    `json:"total"`
	Skip  int    `json:"skip"`
	Limit int    `json:"limit"`
	Sort  string `json:"sort"`
}

// MetricsItem represents a single item in the metrics response
type MetricsItem struct {
	Dimensions []Dimension `json:"dimensions"`
	Metrics    MetricsData `json:"metrics"`
}

// Dimension represents a dimension in the metrics response
type Dimension struct {
	Dimension    string `json:"dimension"`
	Value        string `json:"value"`
	DisplayValue string `json:"display_value"`
}

// MetricsData holds the actual metric values
type MetricsData struct {
	// Volume metrics
	AcceptedIncomingCount   int64 `json:"accepted_incoming_count"`
	AcceptedOutgoingCount   int64 `json:"accepted_outgoing_count"`
	DeliveredSMTPCount      int64 `json:"delivered_smtp_count"`
	DeliveredHTTPCount      int64 `json:"delivered_http_count"`
	DeliveredOptimizedCount int64 `json:"delivered_optimized_count"`
	StoredCount             int64 `json:"stored_count"`

	// Engagement metrics
	OpenedCount        int64 `json:"opened_count"`
	UniqueOpenedCount  int64 `json:"unique_opened_count"`
	ClickedCount       int64 `json:"clicked_count"`
	UniqueClickedCount int64 `json:"unique_clicked_count"`

	// Bounce metrics
	BouncedCount     int64 `json:"bounced_count"`
	HardBouncesCount int64 `json:"hard_bounces_count"`
	SoftBouncesCount int64 `json:"soft_bounces_count"`

	// Other metrics
	UnsubscribedCount int64 `json:"unsubscribed_count"`
	ComplainedCount   int64 `json:"complained_count"`
	FailedCount       int64 `json:"failed_count"`
	SentCount         int64 `json:"sent_count"`
}

// StatsResponse represents a response from the domain stats API
type StatsResponse struct {
	End        string      `json:"end"`
	Resolution string      `json:"resolution"`
	Start      string      `json:"start"`
	Stats      []StatsItem `json:"stats"`
}

// StatsItem represents stats for a single time period
type StatsItem struct {
	Time      string       `json:"time"`
	Accepted  StatsCounter `json:"accepted"`
	Delivered StatsCounter `json:"delivered"`
	Failed    FailedStats  `json:"failed"`
	Opened    StatsCounter `json:"opened"`
	Clicked   StatsCounter `json:"clicked"`
	Complained StatsCounter `json:"complained"`
	Unsubscribed StatsCounter `json:"unsubscribed"`
	Stored    StatsCounter `json:"stored"`
}

// StatsCounter represents a simple count metric
type StatsCounter struct {
	Total int64 `json:"total"`
}

// FailedStats represents failed delivery stats
type FailedStats struct {
	Permanent FailedDetail `json:"permanent"`
	Temporary FailedDetail `json:"temporary"`
}

// FailedDetail represents details about failed deliveries
type FailedDetail struct {
	Total              int64 `json:"total"`
	SuppressBounce     int64 `json:"suppress-bounce"`
	SuppressComplaint  int64 `json:"suppress-complaint"`
	SuppressUnsubscribe int64 `json:"suppress-unsubscribe"`
	Bounce             int64 `json:"bounce"`
	DelayedBounce      int64 `json:"delayed-bounce"`
	EspBlock           int64 `json:"espblock"`
	Webhook            int64 `json:"webhook"`
}

// ProviderAggregatesResponse represents provider aggregates from the stats API
type ProviderAggregatesResponse struct {
	Providers map[string]ProviderStats `json:"provider"`
}

// ProviderStats represents stats for a single provider/ISP
type ProviderStats struct {
	Accepted     int64 `json:"accepted"`
	Delivered    int64 `json:"delivered"`
	Opened       int64 `json:"opened"`
	Clicked      int64 `json:"clicked"`
	Unsubscribed int64 `json:"unsubscribed"`
	Complained   int64 `json:"complained"`
	Bounced      int64 `json:"bounced"`
}

// BounceClassificationRequest represents a request to the bounce classification API
type BounceClassificationRequest struct {
	Start  string   `json:"start"`
	End    string   `json:"end"`
	Domain string   `json:"domain,omitempty"`
	Filter *Filter  `json:"filter,omitempty"`
}

// BounceClassificationResponse represents bounce classification metrics
type BounceClassificationResponse struct {
	Items []BounceClassificationItem `json:"items"`
}

// BounceClassificationItem represents a single bounce classification
type BounceClassificationItem struct {
	Entity         string `json:"entity"`         // ESP or domain
	Classification string `json:"classification"` // "hard", "soft", "espblock"
	Count          int64  `json:"count"`
	Reason         string `json:"reason,omitempty"`
}

// EventsResponse represents events from the logs API
type EventsResponse struct {
	Items  []Event    `json:"items"`
	Paging *Paging    `json:"paging,omitempty"`
}

// Paging represents pagination for events
type Paging struct {
	Next     string `json:"next"`
	Previous string `json:"previous"`
	First    string `json:"first"`
	Last     string `json:"last"`
}

// Event represents a single event from the logs API
type Event struct {
	ID            string            `json:"id"`
	Timestamp     float64           `json:"timestamp"`
	Event         string            `json:"event"`         // "delivered", "opened", "clicked", "bounced", etc.
	Recipient     string            `json:"recipient"`
	RecipientDomain string          `json:"recipient-domain"`
	Tags          []string          `json:"tags"`
	DeliveryStatus *DeliveryStatus  `json:"delivery-status,omitempty"`
	Message       *MessageInfo      `json:"message,omitempty"`
	Flags         map[string]bool   `json:"flags,omitempty"`
	Envelope      *Envelope         `json:"envelope,omitempty"`
	Campaigns     []Campaign        `json:"campaigns,omitempty"`
	UserVariables map[string]string `json:"user-variables,omitempty"`
	IP            string            `json:"ip,omitempty"`
	ClientInfo    *ClientInfo       `json:"client-info,omitempty"`
	Geolocation   *Geolocation      `json:"geolocation,omitempty"`
	Reason        string            `json:"reason,omitempty"`
	Severity      string            `json:"severity,omitempty"` // For bounces: "permanent", "temporary"
}

// DeliveryStatus represents delivery status info
type DeliveryStatus struct {
	AttemptNo          int    `json:"attempt-no"`
	Code               int    `json:"code"`
	Description        string `json:"description"`
	Message            string `json:"message"`
	SessionSeconds     float64 `json:"session-seconds"`
	EnhancedCode       string `json:"enhanced-code,omitempty"`
	MXHost             string `json:"mx-host,omitempty"`
	TLS                bool   `json:"tls"`
	UTF8               bool   `json:"utf8"`
	CertificateVerified bool  `json:"certificate-verified"`
}

// MessageInfo represents message information
type MessageInfo struct {
	Headers     map[string]string `json:"headers"`
	Size        int64             `json:"size"`
	Attachments []Attachment      `json:"attachments,omitempty"`
}

// Attachment represents an email attachment
type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content-type"`
	Size        int64  `json:"size"`
}

// Envelope represents email envelope info
type Envelope struct {
	Sender      string   `json:"sender"`
	SendingIP   string   `json:"sending-ip"`
	Targets     string   `json:"targets"`
	Transport   string   `json:"transport"`
}

// Campaign represents campaign info
type Campaign struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ClientInfo represents client/device info for opens/clicks
type ClientInfo struct {
	ClientOS     string `json:"client-os"`
	DeviceType   string `json:"device-type"`
	ClientName   string `json:"client-name"`
	ClientType   string `json:"client-type"`
	UserAgent    string `json:"user-agent"`
}

// Geolocation represents geographic info
type Geolocation struct {
	Country string `json:"country"`
	Region  string `json:"region"`
	City    string `json:"city"`
}

// IPInfo represents information about a sending IP
type IPInfo struct {
	IP        string `json:"ip"`
	RDNS      string `json:"rdns"`
	Dedicated bool   `json:"dedicated"`
}

// IPPool represents an IP pool
type IPPool struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	IPs         []string `json:"ips"`
}

// DomainInfo represents information about a sending domain
type DomainInfo struct {
	Name            string `json:"name"`
	State           string `json:"state"`
	Type            string `json:"type"`
	SpamAction      string `json:"spam_action"`
	WebScheme       string `json:"web_scheme"`
	Wildcard        bool   `json:"wildcard"`
	SkipVerification bool  `json:"skip_verification"`
	CreatedAt       string `json:"created_at"`
}

// DomainsResponse represents the response from the domains API
type DomainsResponse struct {
	TotalCount int          `json:"total_count"`
	Items      []DomainInfo `json:"items"`
}

// IPsResponse represents the response from the IPs API
type IPsResponse struct {
	TotalCount int      `json:"total_count"`
	Items      []IPInfo `json:"items"`
}

// IPPoolsResponse represents the response from the IP pools API
type IPPoolsResponse struct {
	IPPools []IPPool `json:"ip_pools"`
}

// ProcessedMetrics represents calculated metrics ready for display/storage
// This matches the SparkPost ProcessedMetrics structure for compatibility
type ProcessedMetrics struct {
	Timestamp    time.Time `json:"timestamp"`
	Source       string    `json:"source"` // "mailgun"
	GroupBy      string    `json:"group_by"`
	GroupValue   string    `json:"group_value"`
	Domain       string    `json:"domain,omitempty"` // Mailgun sending domain

	// Raw counts
	Targeted      int64 `json:"targeted"`
	Injected      int64 `json:"injected"`
	Sent          int64 `json:"sent"`
	Delivered     int64 `json:"delivered"`
	Opened        int64 `json:"opened"`
	UniqueOpened  int64 `json:"unique_opened"`
	Clicked       int64 `json:"clicked"`
	UniqueClicked int64 `json:"unique_clicked"`
	Bounced       int64 `json:"bounced"`
	HardBounced   int64 `json:"hard_bounced"`
	SoftBounced   int64 `json:"soft_bounced"`
	BlockBounced  int64 `json:"block_bounced"`
	Complaints    int64 `json:"complaints"`
	Unsubscribes  int64 `json:"unsubscribes"`
	Delayed       int64 `json:"delayed"`
	Rejected      int64 `json:"rejected"`

	// Calculated rates
	DeliveryRate    float64 `json:"delivery_rate"`
	OpenRate        float64 `json:"open_rate"`
	ClickRate       float64 `json:"click_rate"`
	BounceRate      float64 `json:"bounce_rate"`
	HardBounceRate  float64 `json:"hard_bounce_rate"`
	SoftBounceRate  float64 `json:"soft_bounce_rate"`
	BlockRate       float64 `json:"block_rate"`
	ComplaintRate   float64 `json:"complaint_rate"`
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

// Summary represents an aggregated summary of Mailgun metrics
type Summary struct {
	Timestamp     time.Time `json:"timestamp"`
	PeriodStart   time.Time `json:"period_start"`
	PeriodEnd     time.Time `json:"period_end"`

	TotalTargeted     int64 `json:"total_targeted"`
	TotalDelivered    int64 `json:"total_delivered"`
	TotalOpened       int64 `json:"total_opened"`
	TotalClicked      int64 `json:"total_clicked"`
	TotalBounced      int64 `json:"total_bounced"`
	TotalComplaints   int64 `json:"total_complaints"`
	TotalUnsubscribes int64 `json:"total_unsubscribes"`

	DeliveryRate    float64 `json:"delivery_rate"`
	OpenRate        float64 `json:"open_rate"`
	ClickRate       float64 `json:"click_rate"`
	BounceRate      float64 `json:"bounce_rate"`
	ComplaintRate   float64 `json:"complaint_rate"`
	UnsubscribeRate float64 `json:"unsubscribe_rate"`

	// Comparisons to previous period
	VolumeChange    float64 `json:"volume_change"`
	DeliveryChange  float64 `json:"delivery_change"`
	OpenRateChange  float64 `json:"open_rate_change"`
	ComplaintChange float64 `json:"complaint_change"`
}

// ISPMetrics represents metrics for a specific ISP/mailbox provider
type ISPMetrics struct {
	Provider     string           `json:"provider"`
	Metrics      ProcessedMetrics `json:"metrics"`
	Status       string           `json:"status"` // "healthy", "warning", "critical"
	StatusReason string           `json:"status_reason,omitempty"`
	Trends       MetricTrends     `json:"trends"`
}

// IPMetrics represents metrics for a specific sending IP
type IPMetrics struct {
	IP           string           `json:"ip"`
	Pool         string           `json:"pool"`
	Metrics      ProcessedMetrics `json:"metrics"`
	Status       string           `json:"status"`
	StatusReason string           `json:"status_reason,omitempty"`
	Trends       MetricTrends     `json:"trends"`
}

// DomainMetrics represents metrics for a specific sending domain
type DomainMetrics struct {
	Domain       string           `json:"domain"`
	Metrics      ProcessedMetrics `json:"metrics"`
	Status       string           `json:"status"`
	StatusReason string           `json:"status_reason,omitempty"`
	Trends       MetricTrends     `json:"trends"`
}

// MetricTrends represents trend data for a metric
type MetricTrends struct {
	VolumeDirection     string  `json:"volume_direction"` // "up", "down", "stable"
	VolumeChangePercent float64 `json:"volume_change_percent"`
	DeliveryTrend       string  `json:"delivery_trend"`
	DeliveryChange      float64 `json:"delivery_change"`
	ComplaintTrend      string  `json:"complaint_trend"`
	ComplaintChange     float64 `json:"complaint_change"`
}

// SignalsData represents deliverability signals
type SignalsData struct {
	Timestamp        time.Time                  `json:"timestamp"`
	BounceReasons    []BounceClassificationItem `json:"bounce_reasons"`
	TopIssues        []Issue                    `json:"top_issues"`
}

// Issue represents a deliverability issue
type Issue struct {
	Severity       string `json:"severity"` // "warning", "critical"
	Category       string `json:"category"` // "bounce", "complaint", "block", "delay"
	Description    string `json:"description"`
	AffectedISP    string `json:"affected_isp,omitempty"`
	AffectedIP     string `json:"affected_ip,omitempty"`
	AffectedDomain string `json:"affected_domain,omitempty"`
	Count          int64  `json:"count"`
	Recommendation string `json:"recommendation"`
}

// MetricsQuery represents parameters for querying Mailgun metrics
type MetricsQuery struct {
	From       time.Time
	To         time.Time
	Resolution string   // "hour", "day", "month"
	Domains    []string // Filter by sending domains
	Events     []string // Filter by event types
	Limit      int
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

// DefaultMetrics returns the default set of metrics to fetch from Mailgun
func DefaultMetrics() []string {
	return []string{
		"accepted_outgoing_count",
		"delivered_smtp_count",
		"opened_count",
		"unique_opened_count",
		"clicked_count",
		"unique_clicked_count",
		"bounced_count",
		"hard_bounces_count",
		"soft_bounces_count",
		"unsubscribed_count",
		"complained_count",
		"failed_count",
		"stored_count",
	}
}

// domainToISP maps recipient domains to ISP names
var domainToISP = map[string]string{
	// Gmail
	"gmail.com":       "Gmail",
	"googlemail.com":  "Gmail",
	"google.com":      "Gmail",
	// Yahoo
	"yahoo.com":       "Yahoo",
	"yahoo.co.uk":     "Yahoo",
	"yahoo.ca":        "Yahoo",
	"yahoo.com.au":    "Yahoo",
	"yahoo.co.in":     "Yahoo",
	"ymail.com":       "Yahoo",
	"rocketmail.com":  "Yahoo",
	// Microsoft / Outlook
	"hotmail.com":     "Hotmail / Outlook",
	"outlook.com":     "Hotmail / Outlook",
	"live.com":        "Hotmail / Outlook",
	"msn.com":         "Hotmail / Outlook",
	"hotmail.co.uk":   "Hotmail / Outlook",
	"live.co.uk":      "Hotmail / Outlook",
	// Apple
	"icloud.com":      "Apple",
	"me.com":          "Apple",
	"mac.com":         "Apple",
	// AOL
	"aol.com":         "AOL",
	"aim.com":         "AOL",
	// Comcast
	"comcast.net":     "Comcast",
	"xfinity.com":     "Comcast",
	// AT&T
	"att.net":         "AT&T",
	"sbcglobal.net":   "AT&T",
	"bellsouth.net":   "AT&T",
	// Verizon
	"verizon.net":     "Verizon",
	// Proton
	"protonmail.com":  "Proton Mail",
	"proton.me":       "Proton Mail",
	"pm.me":           "Proton Mail",
	// Others
	"zoho.com":        "Zoho",
	"fastmail.com":    "Fastmail",
	"gmx.com":         "GMX",
	"gmx.net":         "GMX",
	"mail.com":        "Mail.com",
	"yandex.com":      "Yandex",
	"yandex.ru":       "Yandex",
}

// MapDomainToISP maps a recipient domain to its ISP name
func MapDomainToISP(domain string) string {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if isp, ok := domainToISP[domain]; ok {
		return isp
	}
	return "Other"
}

// ConvertStatsToProcessedMetrics converts Mailgun stats to ProcessedMetrics
func ConvertStatsToProcessedMetrics(stats StatsItem, domain, groupBy, groupValue string) ProcessedMetrics {
	pm := ProcessedMetrics{
		Timestamp:     time.Now(),
		Source:        "mailgun",
		Domain:        domain,
		GroupBy:       groupBy,
		GroupValue:    groupValue,
		Targeted:      stats.Accepted.Total,
		Sent:          stats.Accepted.Total,
		Delivered:     stats.Delivered.Total,
		Opened:        stats.Opened.Total,
		UniqueOpened:  stats.Opened.Total, // Mailgun doesn't separate unique in stats API
		Clicked:       stats.Clicked.Total,
		UniqueClicked: stats.Clicked.Total,
		Bounced:       stats.Failed.Permanent.Total + stats.Failed.Temporary.Total,
		HardBounced:   stats.Failed.Permanent.Bounce,
		SoftBounced:   stats.Failed.Temporary.Total,
		BlockBounced:  stats.Failed.Permanent.EspBlock,
		Complaints:    stats.Complained.Total,
		Unsubscribes:  stats.Unsubscribed.Total,
	}
	pm.CalculateRates()
	return pm
}

// ConvertMetricsDataToProcessedMetrics converts MetricsData to ProcessedMetrics
func ConvertMetricsDataToProcessedMetrics(data MetricsData, domain, groupBy, groupValue string) ProcessedMetrics {
	pm := ProcessedMetrics{
		Timestamp:     time.Now(),
		Source:        "mailgun",
		Domain:        domain,
		GroupBy:       groupBy,
		GroupValue:    groupValue,
		Targeted:      data.AcceptedOutgoingCount,
		Injected:      data.AcceptedOutgoingCount,
		Sent:          data.AcceptedOutgoingCount,
		Delivered:     data.DeliveredSMTPCount + data.DeliveredHTTPCount + data.DeliveredOptimizedCount,
		Opened:        data.OpenedCount,
		UniqueOpened:  data.UniqueOpenedCount,
		Clicked:       data.ClickedCount,
		UniqueClicked: data.UniqueClickedCount,
		Bounced:       data.BouncedCount,
		HardBounced:   data.HardBouncesCount,
		SoftBounced:   data.SoftBouncesCount,
		Complaints:    data.ComplainedCount,
		Unsubscribes:  data.UnsubscribedCount,
		Rejected:      data.FailedCount,
	}
	pm.CalculateRates()
	return pm
}

// ConvertProviderStatsToISPMetrics converts provider stats to ISPMetrics
func ConvertProviderStatsToISPMetrics(provider string, stats ProviderStats, domain string) ISPMetrics {
	pm := ProcessedMetrics{
		Timestamp:     time.Now(),
		Source:        "mailgun",
		Domain:        domain,
		GroupBy:       "provider",
		GroupValue:    provider,
		Targeted:      stats.Accepted,
		Sent:          stats.Accepted,
		Delivered:     stats.Delivered,
		Opened:        stats.Opened,
		UniqueOpened:  stats.Opened,
		Clicked:       stats.Clicked,
		UniqueClicked: stats.Clicked,
		Bounced:       stats.Bounced,
		Complaints:    stats.Complained,
		Unsubscribes:  stats.Unsubscribed,
	}
	pm.CalculateRates()

	return ISPMetrics{
		Provider: provider,
		Metrics:  pm,
		Status:   "healthy", // Default, will be evaluated by agent
	}
}
