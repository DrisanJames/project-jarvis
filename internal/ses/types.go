package ses

import (
	"time"
)

// VDM Metric types from AWS SES
const (
	MetricSend            = "SEND"
	MetricDelivery        = "DELIVERY"
	MetricPermanentBounce = "PERMANENT_BOUNCE"
	MetricTransientBounce = "TRANSIENT_BOUNCE"
	MetricComplaint       = "COMPLAINT"
	MetricOpen            = "OPEN"
	MetricClick           = "CLICK"
)

// AllMetrics returns all VDM metric types
func AllMetrics() []string {
	return []string{
		MetricSend,
		MetricDelivery,
		MetricPermanentBounce,
		MetricTransientBounce,
		MetricComplaint,
		MetricOpen,
		MetricClick,
	}
}

// MetricQuery represents a single metric query for BatchGetMetricData
type MetricQuery struct {
	ID         string            `json:"id"`
	Namespace  string            `json:"namespace"` // Always "VDM"
	Metric     string            `json:"metric"`
	Dimensions map[string]string `json:"dimensions"`
	StartDate  time.Time         `json:"start_date"`
	EndDate    time.Time         `json:"end_date"`
}

// MetricResult represents a single metric result from BatchGetMetricData
type MetricResult struct {
	ID         string             `json:"id"`
	Timestamps []time.Time        `json:"timestamps"`
	Values     []float64          `json:"values"`
	Dimensions map[string]string  `json:"dimensions,omitempty"`
}

// ISPMetricData holds raw metric data for an ISP
type ISPMetricData struct {
	ISP              string  `json:"isp"`
	Send             int64   `json:"send"`
	Delivery         int64   `json:"delivery"`
	PermanentBounce  int64   `json:"permanent_bounce"`
	TransientBounce  int64   `json:"transient_bounce"`
	Complaint        int64   `json:"complaint"`
	Open             int64   `json:"open"`
	Click            int64   `json:"click"`
}

// ProcessedMetrics represents calculated metrics ready for display/storage
// Matches the structure used by SparkPost and Mailgun for consistency
type ProcessedMetrics struct {
	Timestamp    time.Time `json:"timestamp"`
	Source       string    `json:"source"` // "ses"
	GroupBy      string    `json:"group_by"`
	GroupValue   string    `json:"group_value"`

	// Core metrics
	Targeted     int64 `json:"targeted"`
	Injected     int64 `json:"injected"`
	Sent         int64 `json:"sent"`
	Delivered    int64 `json:"delivered"`
	Opened       int64 `json:"opened"`
	UniqueOpened int64 `json:"unique_opened"`
	Clicked      int64 `json:"clicked"`
	UniqueClicked int64 `json:"unique_clicked"`

	// Bounce metrics
	Bounced      int64 `json:"bounced"`
	HardBounced  int64 `json:"hard_bounced"`
	SoftBounced  int64 `json:"soft_bounced"`
	BlockBounced int64 `json:"block_bounced"`

	// Other metrics
	Complaints   int64 `json:"complaints"`
	Unsubscribes int64 `json:"unsubscribes"`
	Delayed      int64 `json:"delayed"`
	Rejected     int64 `json:"rejected"`

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

// CalculateRates calculates all the percentage rates
func (pm *ProcessedMetrics) CalculateRates() {
	if pm.Sent > 0 {
		pm.DeliveryRate = float64(pm.Delivered) / float64(pm.Sent)
		pm.BounceRate = float64(pm.Bounced) / float64(pm.Sent)
		pm.HardBounceRate = float64(pm.HardBounced) / float64(pm.Sent)
		pm.SoftBounceRate = float64(pm.SoftBounced) / float64(pm.Sent)
		pm.BlockRate = float64(pm.BlockBounced) / float64(pm.Sent)
		pm.ComplaintRate = float64(pm.Complaints) / float64(pm.Sent)
		pm.UnsubscribeRate = float64(pm.Unsubscribes) / float64(pm.Sent)
	}

	if pm.Delivered > 0 {
		pm.OpenRate = float64(pm.Opened) / float64(pm.Delivered)
		pm.ClickRate = float64(pm.Clicked) / float64(pm.Delivered)
	}
}

// Summary represents aggregated SES metrics for a time period
type Summary struct {
	Timestamp   time.Time `json:"timestamp"`
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`

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

// CalculateRates calculates all summary rates
func (s *Summary) CalculateRates() {
	if s.TotalTargeted > 0 {
		s.DeliveryRate = float64(s.TotalDelivered) / float64(s.TotalTargeted)
		s.BounceRate = float64(s.TotalBounced) / float64(s.TotalTargeted)
		s.ComplaintRate = float64(s.TotalComplaints) / float64(s.TotalTargeted)
		s.UnsubscribeRate = float64(s.TotalUnsubscribes) / float64(s.TotalTargeted)
	}

	if s.TotalDelivered > 0 {
		s.OpenRate = float64(s.TotalOpened) / float64(s.TotalDelivered)
		s.ClickRate = float64(s.TotalClicked) / float64(s.TotalDelivered)
	}
}

// ISPMetrics represents metrics for a specific ISP/mailbox provider
type ISPMetrics struct {
	Provider     string           `json:"provider"`
	Metrics      ProcessedMetrics `json:"metrics"`
	Status       string           `json:"status"` // "healthy", "warning", "critical"
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

// SignalsData represents deliverability signals and issues
type SignalsData struct {
	Timestamp     time.Time `json:"timestamp"`
	TopIssues     []Issue   `json:"top_issues"`
	Recommendations []string `json:"recommendations"`
}

// Issue represents a deliverability issue
type Issue struct {
	Severity       string `json:"severity"` // "warning", "critical"
	Category       string `json:"category"` // "bounce", "complaint", "block", "delay"
	Description    string `json:"description"`
	AffectedISP    string `json:"affected_isp,omitempty"`
	Count          int64  `json:"count"`
	Recommendation string `json:"recommendation"`
}

// MetricsQuery represents parameters for querying SES metrics
type MetricsQuery struct {
	From       time.Time
	To         time.Time
	ISPs       []string // Filter by ISPs
}

// ConvertISPDataToMetrics converts raw ISP metric data to ProcessedMetrics
func ConvertISPDataToMetrics(data ISPMetricData) ProcessedMetrics {
	pm := ProcessedMetrics{
		Timestamp:    time.Now(),
		Source:       "ses",
		GroupBy:      "isp",
		GroupValue:   data.ISP,
		Targeted:     data.Send,
		Sent:         data.Send,
		Delivered:    data.Delivery,
		Opened:       data.Open,
		UniqueOpened: data.Open,
		Clicked:      data.Click,
		UniqueClicked: data.Click,
		Bounced:      data.PermanentBounce + data.TransientBounce,
		HardBounced:  data.PermanentBounce,
		SoftBounced:  data.TransientBounce,
		Complaints:   data.Complaint,
	}
	pm.CalculateRates()
	return pm
}

// ConvertISPDataToISPMetrics converts raw ISP data to ISPMetrics with status evaluation
func ConvertISPDataToISPMetrics(data ISPMetricData) ISPMetrics {
	pm := ConvertISPDataToMetrics(data)
	status, reason := EvaluateISPHealth(&pm)
	
	return ISPMetrics{
		Provider:     data.ISP,
		Metrics:      pm,
		Status:       status,
		StatusReason: reason,
	}
}

// EvaluateISPHealth determines the health status of ISP metrics
func EvaluateISPHealth(pm *ProcessedMetrics) (string, string) {
	// Check complaint rate first (most critical)
	if pm.ComplaintRate >= 0.001 { // 0.1%
		return "critical", "Complaint rate exceeds critical threshold"
	}
	if pm.ComplaintRate >= 0.0005 { // 0.05%
		return "warning", "Complaint rate approaching threshold"
	}

	// Check bounce rate
	if pm.BounceRate >= 0.10 { // 10%
		return "critical", "Bounce rate exceeds critical threshold"
	}
	if pm.BounceRate >= 0.05 { // 5%
		return "warning", "Bounce rate approaching threshold"
	}

	// Check delivery rate
	if pm.DeliveryRate < 0.90 { // Below 90%
		return "warning", "Delivery rate below expected"
	}

	return "healthy", ""
}

// AggregateISPMetricsToSummary aggregates multiple ISP metrics into a summary
func AggregateISPMetricsToSummary(metrics []ISPMetrics, from, to time.Time) *Summary {
	summary := &Summary{
		Timestamp:   time.Now(),
		PeriodStart: from,
		PeriodEnd:   to,
	}

	for _, isp := range metrics {
		m := isp.Metrics
		summary.TotalTargeted += m.Targeted
		summary.TotalDelivered += m.Delivered
		summary.TotalOpened += m.Opened
		summary.TotalClicked += m.Clicked
		summary.TotalBounced += m.Bounced
		summary.TotalComplaints += m.Complaints
		summary.TotalUnsubscribes += m.Unsubscribes
	}

	summary.CalculateRates()
	return summary
}
