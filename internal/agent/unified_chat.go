package agent

import (
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/everflow"
	"github.com/ignite/sparkpost-monitor/internal/mailgun"
	"github.com/ignite/sparkpost-monitor/internal/ongage"
	"github.com/ignite/sparkpost-monitor/internal/ses"
	"github.com/ignite/sparkpost-monitor/internal/sparkpost"
)

// ESPCollectors holds references to all ESP collectors
type ESPCollectors struct {
	SparkPost SparkPostCollector
	Mailgun   MailgunCollector
	SES       SESCollector
	Ongage    OngageCollector
	Everflow  EverflowCollector
	Kanban    KanbanService
}

// KanbanService interface for accessing Kanban tasks
type KanbanService interface {
	GetBoardData() interface{}
}

// SparkPostCollector interface for SparkPost data access
type SparkPostCollector interface {
	GetLatestSummary() *sparkpost.Summary
	GetLatestISPMetrics() []sparkpost.ISPMetrics
	GetLastFetchTime() time.Time
}

// MailgunCollector interface for Mailgun data access
type MailgunCollector interface {
	GetLatestSummary() *mailgun.Summary
	GetLatestISPMetrics() []mailgun.ISPMetrics
	GetLastFetchTime() time.Time
}

// SESCollector interface for SES data access
type SESCollector interface {
	GetLatestSummary() *ses.Summary
	GetLatestISPMetrics() []ses.ISPMetrics
	GetLastFetchTime() time.Time
}

// OngageCollector interface for Ongage data access
type OngageCollector interface {
	GetCampaigns() []ongage.ProcessedCampaign
	GetSubjectAnalysis() []ongage.SubjectLineAnalysis
	GetScheduleAnalysis() []ongage.ScheduleAnalysis
	GetESPPerformance() []ongage.ESPPerformance
	GetAudienceAnalysis() []ongage.AudienceAnalysis
	GetPipelineMetrics() []ongage.PipelineMetrics
	LastFetch() time.Time
}

// EverflowCollector interface for Everflow data access
type EverflowCollector interface {
	GetDailyPerformance() []everflow.DailyPerformance
	GetOfferPerformance() []everflow.OfferPerformance
	GetPropertyPerformance() []everflow.PropertyPerformance
	GetCampaignRevenue() []everflow.CampaignRevenue
	GetTotalRevenue() float64
	LastFetch() time.Time
}

// UnifiedISP represents ISP metrics from any provider
type UnifiedISP struct {
	Provider      string  // sparkpost, mailgun, ses
	ISP           string
	Volume        int64
	Delivered     int64
	DeliveryRate  float64
	OpenRate      float64
	ClickRate     float64
	BounceRate    float64
	ComplaintRate float64
	Status        string
	StatusReason  string
}

// EcosystemSummary represents the entire email ecosystem
type EcosystemSummary struct {
	TotalVolume     int64
	TotalDelivered  int64
	TotalOpens      int64
	TotalClicks     int64
	TotalBounces    int64
	TotalComplaints int64
	DeliveryRate    float64
	OpenRate        float64
	ClickRate       float64
	BounceRate      float64
	ComplaintRate   float64
	ProviderCount   int
	ISPCount        int
	HealthyISPs     int
	WarningISPs     int
	CriticalISPs    int
}

// SetCollectors sets the ESP collectors for unified access
func (a *Agent) SetCollectors(collectors ESPCollectors) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.collectors = &collectors
}

// UnifiedChat handles queries across all ESPs
func (a *Agent) UnifiedChat(query string) ChatResponse {
	query = strings.ToLower(query)

	// Get unified data
	ecosystem, allISPs := a.getEcosystemData()

	// Detect intent and respond accordingly
	switch {
	// Everflow-specific queries (revenue/conversions)
	case containsAny(query, []string{"revenue", "earnings", "money", "income"}):
		return a.handleEverflowRevenueQuery(query)
	case containsAny(query, []string{"conversion", "conversions", "converted"}):
		return a.handleEverflowConversionQuery(query)
	case containsAny(query, []string{"offer", "offers"}) && containsAny(query, []string{"performance", "best", "top", "revenue"}):
		return a.handleEverflowOfferQuery(query)
	case containsAny(query, []string{"property", "properties", "domain"}) && containsAny(query, []string{"performance", "best", "top", "revenue"}):
		return a.handleEverflowPropertyQuery(query)
	case containsAny(query, []string{"epc", "earnings per click", "click value"}):
		return a.handleEverflowEPCQuery()
	case containsAny(query, []string{"everflow", "affiliate", "tracking"}):
		return a.handleEverflowSummaryQuery()
	// Ongage-specific queries
	case containsAny(query, []string{"campaign", "campaigns", "mailing", "mailings"}):
		return a.handleOngageCampaignQuery(query)
	case containsAny(query, []string{"subject line", "subject", "subjects", "email title"}):
		return a.handleOngageSubjectQuery(query)
	case containsAny(query, []string{"send time", "schedule", "optimal time", "best time", "when to send"}):
		return a.handleOngageScheduleQuery()
	case containsAny(query, []string{"segment", "audience", "targeting", "list"}):
		return a.handleOngageAudienceQuery()
	case containsAny(query, []string{"ongage", "campaign platform"}):
		return a.handleOngageSummaryQuery()
	// ESP ecosystem queries
	case containsAny(query, []string{"ecosystem", "all provider", "all esp", "overall", "total", "combined"}):
		return a.handleEcosystemQuery(ecosystem, allISPs)
	case containsAny(query, []string{"compare", "comparison", "vs", "versus", "better", "worse"}):
		return a.handleComparisonQuery(query, allISPs)
	case containsAny(query, []string{"sparkpost"}):
		return a.handleProviderQuery("sparkpost", allISPs)
	case containsAny(query, []string{"mailgun"}):
		return a.handleProviderQuery("mailgun", allISPs)
	case containsAny(query, []string{"ses", "aws"}):
		return a.handleProviderQuery("ses", allISPs)
	case containsAny(query, []string{"forecast", "predict", "expect", "tomorrow", "next week"}):
		return a.handleUnifiedForecastQuery(query, ecosystem, allISPs)
	case containsAny(query, []string{"concern", "issue", "problem", "alert", "watch"}):
		return a.handleUnifiedConcernsQuery(ecosystem, allISPs)
	case containsAny(query, []string{"recommend", "suggest", "advice", "should i"}):
		return a.handleUnifiedRecommendationQuery(query, ecosystem, allISPs)
	case containsAny(query, []string{"isp", "gmail", "yahoo", "outlook", "hotmail", "aol"}):
		return a.handleISPQuery(query, allISPs)
	case containsAny(query, []string{"volume", "increase", "decrease", "throttle"}):
		return a.handleUnifiedVolumeQuery(query, ecosystem, allISPs)
	case containsAny(query, []string{"how", "performance", "doing", "status"}):
		return a.handleEcosystemQuery(ecosystem, allISPs)
	case containsAny(query, []string{"learn", "pattern", "baseline", "correlation"}):
		return a.handleUnifiedLearningQuery()
	default:
		return a.handleUnifiedGeneralQuery(ecosystem)
	}
}

// getEcosystemData gathers data from all ESPs
func (a *Agent) getEcosystemData() (EcosystemSummary, []UnifiedISP) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var allISPs []UnifiedISP
	ecosystem := EcosystemSummary{}
	providers := make(map[string]bool)

	if a.collectors == nil {
		return ecosystem, allISPs
	}

	// Get SparkPost data
	if a.collectors.SparkPost != nil {
		providers["sparkpost"] = true
		if summary := a.collectors.SparkPost.GetLatestSummary(); summary != nil {
			ecosystem.TotalVolume += summary.TotalTargeted
			ecosystem.TotalDelivered += summary.TotalDelivered
			ecosystem.TotalOpens += summary.TotalOpened
			ecosystem.TotalClicks += summary.TotalClicked
			ecosystem.TotalBounces += summary.TotalBounced
			ecosystem.TotalComplaints += summary.TotalComplaints
		}
		for _, isp := range a.collectors.SparkPost.GetLatestISPMetrics() {
			u := UnifiedISP{
				Provider:      "sparkpost",
				ISP:           isp.Provider,
				Volume:        isp.Metrics.Targeted,
				Delivered:     isp.Metrics.Delivered,
				DeliveryRate:  isp.Metrics.DeliveryRate,
				OpenRate:      isp.Metrics.OpenRate,
				ClickRate:     isp.Metrics.ClickRate,
				BounceRate:    isp.Metrics.BounceRate,
				ComplaintRate: isp.Metrics.ComplaintRate,
				Status:        isp.Status,
				StatusReason:  isp.StatusReason,
			}
			allISPs = append(allISPs, u)
			a.updateISPCounts(&ecosystem, isp.Status)
		}
	}

	// Get Mailgun data
	if a.collectors.Mailgun != nil {
		providers["mailgun"] = true
		if summary := a.collectors.Mailgun.GetLatestSummary(); summary != nil {
			ecosystem.TotalVolume += summary.TotalTargeted
			ecosystem.TotalDelivered += summary.TotalDelivered
			ecosystem.TotalOpens += summary.TotalOpened
			ecosystem.TotalClicks += summary.TotalClicked
			ecosystem.TotalBounces += summary.TotalBounced
			ecosystem.TotalComplaints += summary.TotalComplaints
		}
		for _, isp := range a.collectors.Mailgun.GetLatestISPMetrics() {
			u := UnifiedISP{
				Provider:      "mailgun",
				ISP:           isp.Provider,
				Volume:        isp.Metrics.Targeted,
				Delivered:     isp.Metrics.Delivered,
				DeliveryRate:  isp.Metrics.DeliveryRate,
				OpenRate:      isp.Metrics.OpenRate,
				ClickRate:     isp.Metrics.ClickRate,
				BounceRate:    isp.Metrics.BounceRate,
				ComplaintRate: isp.Metrics.ComplaintRate,
				Status:        isp.Status,
				StatusReason:  isp.StatusReason,
			}
			allISPs = append(allISPs, u)
			a.updateISPCounts(&ecosystem, isp.Status)
		}
	}

	// Get SES data
	if a.collectors.SES != nil {
		providers["ses"] = true
		if summary := a.collectors.SES.GetLatestSummary(); summary != nil {
			ecosystem.TotalVolume += summary.TotalTargeted
			ecosystem.TotalDelivered += summary.TotalDelivered
			ecosystem.TotalOpens += summary.TotalOpened
			ecosystem.TotalClicks += summary.TotalClicked
			ecosystem.TotalBounces += summary.TotalBounced
			ecosystem.TotalComplaints += summary.TotalComplaints
		}
		for _, isp := range a.collectors.SES.GetLatestISPMetrics() {
			u := UnifiedISP{
				Provider:      "ses",
				ISP:           isp.Provider,
				Volume:        isp.Metrics.Targeted,
				Delivered:     isp.Metrics.Delivered,
				DeliveryRate:  isp.Metrics.DeliveryRate,
				OpenRate:      isp.Metrics.OpenRate,
				ClickRate:     isp.Metrics.ClickRate,
				BounceRate:    isp.Metrics.BounceRate,
				ComplaintRate: isp.Metrics.ComplaintRate,
				Status:        isp.Status,
				StatusReason:  isp.StatusReason,
			}
			allISPs = append(allISPs, u)
			a.updateISPCounts(&ecosystem, isp.Status)
		}
	}

	// Calculate rates
	if ecosystem.TotalVolume > 0 {
		ecosystem.DeliveryRate = float64(ecosystem.TotalDelivered) / float64(ecosystem.TotalVolume)
		ecosystem.OpenRate = float64(ecosystem.TotalOpens) / float64(ecosystem.TotalDelivered)
		ecosystem.ClickRate = float64(ecosystem.TotalClicks) / float64(ecosystem.TotalDelivered)
		ecosystem.BounceRate = float64(ecosystem.TotalBounces) / float64(ecosystem.TotalVolume)
		ecosystem.ComplaintRate = float64(ecosystem.TotalComplaints) / float64(ecosystem.TotalDelivered)
	}
	ecosystem.ProviderCount = len(providers)
	ecosystem.ISPCount = len(allISPs)

	return ecosystem, allISPs
}

func (a *Agent) updateISPCounts(e *EcosystemSummary, status string) {
	switch status {
	case "healthy":
		e.HealthyISPs++
	case "warning":
		e.WarningISPs++
	case "critical":
		e.CriticalISPs++
	}
}
