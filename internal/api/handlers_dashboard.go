package api

import (
	"net/http"
	"time"
)

// GetCombinedDashboard returns data from all providers: SparkPost, Mailgun, and SES
func (h *Handlers) GetCombinedDashboard(w http.ResponseWriter, r *http.Request) {
	// SparkPost data
	spSummary := h.collector.GetLatestSummary()
	spISPMetrics := h.collector.GetLatestISPMetrics()
	spIPMetrics := h.collector.GetLatestIPMetrics()
	spDomainMetrics := h.collector.GetLatestDomainMetrics()
	spSignals := h.collector.GetLatestSignals()
	alerts := h.agent.GetAlerts()

	// Count active alerts
	activeAlerts := 0
	for _, alert := range alerts {
		if !alert.Acknowledged {
			activeAlerts++
		}
	}

	dashboard := map[string]interface{}{
		"timestamp": time.Now(),
		"sparkpost": map[string]interface{}{
			"last_fetch":     h.collector.GetLastFetchTime(),
			"summary":        spSummary,
			"isp_metrics":    spISPMetrics,
			"ip_metrics":     spIPMetrics,
			"domain_metrics": spDomainMetrics,
			"signals":        spSignals,
		},
		"alerts": map[string]interface{}{
			"active_count": activeAlerts,
			"total_count":  len(alerts),
			"items":        alerts,
		},
	}

	// Add Mailgun data if collector is configured
	if h.mailgunCollector != nil {
		mgSummary := h.mailgunCollector.GetLatestSummary()
		mgISPMetrics := h.mailgunCollector.GetLatestISPMetrics()
		mgDomainMetrics := h.mailgunCollector.GetLatestDomainMetrics()
		mgSignals := h.mailgunCollector.GetLatestSignals()

		dashboard["mailgun"] = map[string]interface{}{
			"last_fetch":     h.mailgunCollector.GetLastFetchTime(),
			"summary":        mgSummary,
			"isp_metrics":    mgISPMetrics,
			"domain_metrics": mgDomainMetrics,
			"signals":        mgSignals,
		}
	}

	// Add SES data if collector is configured
	if h.sesCollector != nil {
		sesSummary := h.sesCollector.GetLatestSummary()
		sesISPMetrics := h.sesCollector.GetLatestISPMetrics()
		sesSignals := h.sesCollector.GetLatestSignals()

		dashboard["ses"] = map[string]interface{}{
			"last_fetch":  h.sesCollector.GetLastFetchTime(),
			"summary":     sesSummary,
			"isp_metrics": sesISPMetrics,
			"signals":     sesSignals,
		}
	}

	respondJSON(w, http.StatusOK, dashboard)
}

// UnifiedISPMetric represents a single ISP's metrics from any provider
type UnifiedISPMetric struct {
	Provider      string  `json:"provider"`       // sparkpost, mailgun, ses
	ISP           string  `json:"isp"`            // Gmail, Yahoo, etc.
	Volume        int64   `json:"volume"`         // Total sends
	Delivered     int64   `json:"delivered"`      // Total delivered
	DeliveryRate  float64 `json:"delivery_rate"`  // Delivery percentage
	Opens         int64   `json:"opens"`          // Total opens
	OpenRate      float64 `json:"open_rate"`      // Open percentage
	Clicks        int64   `json:"clicks"`         // Total clicks
	ClickRate     float64 `json:"click_rate"`     // Click percentage (CTR)
	Bounces       int64   `json:"bounces"`        // Total bounces
	BounceRate    float64 `json:"bounce_rate"`    // Bounce percentage
	Complaints    int64   `json:"complaints"`     // Total complaints
	ComplaintRate float64 `json:"complaint_rate"` // Complaint percentage
	Status        string  `json:"status"`         // healthy, warning, critical
}

// UnifiedISPResponse is the response for the unified ISP metrics endpoint
type UnifiedISPResponse struct {
	Timestamp time.Time          `json:"timestamp"`
	Metrics   []UnifiedISPMetric `json:"metrics"`
	Providers []string           `json:"providers"` // List of active providers
}

// GetUnifiedISPMetrics returns ISP metrics from all configured providers
func (h *Handlers) GetUnifiedISPMetrics(w http.ResponseWriter, r *http.Request) {
	var metrics []UnifiedISPMetric
	var providers []string

	// Get SparkPost ISP metrics
	if h.collector != nil {
		providers = append(providers, "sparkpost")
		spMetrics := h.collector.GetLatestISPMetrics()
		for _, m := range spMetrics {
			metrics = append(metrics, UnifiedISPMetric{
				Provider:      "sparkpost",
				ISP:           m.Provider,
				Volume:        m.Metrics.Targeted,
				Delivered:     m.Metrics.Delivered,
				DeliveryRate:  m.Metrics.DeliveryRate,
				Opens:         m.Metrics.UniqueOpened,
				OpenRate:      m.Metrics.OpenRate,
				Clicks:        m.Metrics.UniqueClicked,
				ClickRate:     m.Metrics.ClickRate,
				Bounces:       m.Metrics.Bounced,
				BounceRate:    m.Metrics.BounceRate,
				Complaints:    m.Metrics.Complaints,
				ComplaintRate: m.Metrics.ComplaintRate,
				Status:        m.Status,
			})
		}
	}

	// Get Mailgun ISP metrics
	if h.mailgunCollector != nil {
		providers = append(providers, "mailgun")
		mgMetrics := h.mailgunCollector.GetLatestISPMetrics()
		for _, m := range mgMetrics {
			metrics = append(metrics, UnifiedISPMetric{
				Provider:      "mailgun",
				ISP:           m.Provider,
				Volume:        m.Metrics.Targeted,
				Delivered:     m.Metrics.Delivered,
				DeliveryRate:  m.Metrics.DeliveryRate,
				Opens:         m.Metrics.UniqueOpened,
				OpenRate:      m.Metrics.OpenRate,
				Clicks:        m.Metrics.UniqueClicked,
				ClickRate:     m.Metrics.ClickRate,
				Bounces:       m.Metrics.Bounced,
				BounceRate:    m.Metrics.BounceRate,
				Complaints:    m.Metrics.Complaints,
				ComplaintRate: m.Metrics.ComplaintRate,
				Status:        m.Status,
			})
		}
	}

	// Get SES ISP metrics
	if h.sesCollector != nil {
		providers = append(providers, "ses")
		sesMetrics := h.sesCollector.GetLatestISPMetrics()
		for _, m := range sesMetrics {
			metrics = append(metrics, UnifiedISPMetric{
				Provider:      "ses",
				ISP:           m.Provider,
				Volume:        m.Metrics.Targeted,
				Delivered:     m.Metrics.Delivered,
				DeliveryRate:  m.Metrics.DeliveryRate,
				Opens:         m.Metrics.UniqueOpened,
				OpenRate:      m.Metrics.OpenRate,
				Clicks:        m.Metrics.UniqueClicked,
				ClickRate:     m.Metrics.ClickRate,
				Bounces:       m.Metrics.Bounced,
				BounceRate:    m.Metrics.BounceRate,
				Complaints:    m.Metrics.Complaints,
				ComplaintRate: m.Metrics.ComplaintRate,
				Status:        m.Status,
			})
		}
	}

	respondJSON(w, http.StatusOK, UnifiedISPResponse{
		Timestamp: time.Now(),
		Metrics:   metrics,
		Providers: providers,
	})
}

// UnifiedIPMetric represents a single IP's metrics from any provider
type UnifiedIPMetric struct {
	Provider      string  `json:"provider"`       // sparkpost, mailgun, ses
	IP            string  `json:"ip"`             // IP address
	Pool          string  `json:"pool"`           // IP pool name
	PoolType      string  `json:"pool_type"`      // dedicated or shared
	PoolDesc      string  `json:"pool_description"` // Pool description
	Volume        int64   `json:"volume"`         // Total sends
	Delivered     int64   `json:"delivered"`      // Total delivered
	DeliveryRate  float64 `json:"delivery_rate"`  // Delivery percentage
	Opens         int64   `json:"opens"`          // Total opens
	OpenRate      float64 `json:"open_rate"`      // Open percentage
	Clicks        int64   `json:"clicks"`         // Total clicks
	ClickRate     float64 `json:"click_rate"`     // Click percentage
	Bounces       int64   `json:"bounces"`        // Total bounces
	BounceRate    float64 `json:"bounce_rate"`    // Bounce percentage
	Complaints    int64   `json:"complaints"`     // Total complaints
	ComplaintRate float64 `json:"complaint_rate"` // Complaint percentage
	Status        string  `json:"status"`         // healthy, warning, critical
	StatusReason  string  `json:"status_reason"`  // Reason for status
}

// UnifiedIPResponse is the response for the unified IP metrics endpoint
type UnifiedIPResponse struct {
	Timestamp time.Time         `json:"timestamp"`
	Metrics   []UnifiedIPMetric `json:"metrics"`
	Providers []string          `json:"providers"` // List of active providers with IP data
}

// GetUnifiedIPMetrics returns IP metrics from all configured providers
func (h *Handlers) GetUnifiedIPMetrics(w http.ResponseWriter, r *http.Request) {
	var metrics []UnifiedIPMetric
	var providers []string

	// Helper to get pool info from config
	getPoolInfo := func(provider, poolName string) (poolType, poolDesc string) {
		if h.config.IPPools == nil {
			return "unknown", ""
		}
		pools, ok := h.config.IPPools[provider]
		if !ok {
			return "unknown", ""
		}
		for _, pool := range pools {
			if pool.Name == poolName {
				return pool.Type, pool.Description
			}
		}
		// If pool not found in config, default to unknown
		return "unknown", ""
	}

	// Get SparkPost IP metrics
	if h.collector != nil {
		ipMetrics := h.collector.GetLatestIPMetrics()
		if len(ipMetrics) > 0 {
			providers = append(providers, "sparkpost")
			for _, m := range ipMetrics {
				poolType, poolDesc := getPoolInfo("sparkpost", m.Pool)
				metrics = append(metrics, UnifiedIPMetric{
					Provider:      "sparkpost",
					IP:            m.IP,
					Pool:          m.Pool,
					PoolType:      poolType,
					PoolDesc:      poolDesc,
					Volume:        m.Metrics.Targeted,
					Delivered:     m.Metrics.Delivered,
					DeliveryRate:  m.Metrics.DeliveryRate,
					Opens:         m.Metrics.UniqueOpened,
					OpenRate:      m.Metrics.OpenRate,
					Clicks:        m.Metrics.UniqueClicked,
					ClickRate:     m.Metrics.ClickRate,
					Bounces:       m.Metrics.Bounced,
					BounceRate:    m.Metrics.BounceRate,
					Complaints:    m.Metrics.Complaints,
					ComplaintRate: m.Metrics.ComplaintRate,
					Status:        m.Status,
					StatusReason:  m.StatusReason,
				})
			}
		}
	}

	// Note: Mailgun and SES don't provide IP-level metrics in the same way
	// They operate at domain/ISP level. If they add IP metrics in the future,
	// we can extend this endpoint.

	respondJSON(w, http.StatusOK, UnifiedIPResponse{
		Timestamp: time.Now(),
		Metrics:   metrics,
		Providers: providers,
	})
}

// UnifiedDomainMetric represents a single domain's metrics from any provider
type UnifiedDomainMetric struct {
	Provider      string  `json:"provider"`       // sparkpost, mailgun, ses
	Domain        string  `json:"domain"`         // Sending domain
	Volume        int64   `json:"volume"`         // Total sends
	Delivered     int64   `json:"delivered"`      // Total delivered
	DeliveryRate  float64 `json:"delivery_rate"`  // Delivery percentage
	Opens         int64   `json:"opens"`          // Total opens
	OpenRate      float64 `json:"open_rate"`      // Open percentage
	Clicks        int64   `json:"clicks"`         // Total clicks
	ClickRate     float64 `json:"click_rate"`     // Click percentage
	Bounces       int64   `json:"bounces"`        // Total bounces
	BounceRate    float64 `json:"bounce_rate"`    // Bounce percentage
	Complaints    int64   `json:"complaints"`     // Total complaints
	ComplaintRate float64 `json:"complaint_rate"` // Complaint percentage
	Status        string  `json:"status"`         // healthy, warning, critical
	StatusReason  string  `json:"status_reason"`  // Reason for status
}

// UnifiedDomainResponse is the response for the unified domain metrics endpoint
type UnifiedDomainResponse struct {
	Timestamp time.Time             `json:"timestamp"`
	Metrics   []UnifiedDomainMetric `json:"metrics"`
	Providers []string              `json:"providers"` // List of active providers with domain data
}

// GetUnifiedDomainMetrics returns domain metrics from all configured providers
func (h *Handlers) GetUnifiedDomainMetrics(w http.ResponseWriter, r *http.Request) {
	var metrics []UnifiedDomainMetric
	var providers []string

	// Get SparkPost domain metrics
	if h.collector != nil {
		domainMetrics := h.collector.GetLatestDomainMetrics()
		if len(domainMetrics) > 0 {
			providers = append(providers, "sparkpost")
			for _, m := range domainMetrics {
				metrics = append(metrics, UnifiedDomainMetric{
					Provider:      "sparkpost",
					Domain:        m.Domain,
					Volume:        m.Metrics.Targeted,
					Delivered:     m.Metrics.Delivered,
					DeliveryRate:  m.Metrics.DeliveryRate,
					Opens:         m.Metrics.UniqueOpened,
					OpenRate:      m.Metrics.OpenRate,
					Clicks:        m.Metrics.UniqueClicked,
					ClickRate:     m.Metrics.ClickRate,
					Bounces:       m.Metrics.Bounced,
					BounceRate:    m.Metrics.BounceRate,
					Complaints:    m.Metrics.Complaints,
					ComplaintRate: m.Metrics.ComplaintRate,
					Status:        m.Status,
					StatusReason:  m.StatusReason,
				})
			}
		}
	}

	// Get Mailgun domain metrics
	if h.mailgunCollector != nil {
		domainMetrics := h.mailgunCollector.GetLatestDomainMetrics()
		if len(domainMetrics) > 0 {
			providers = append(providers, "mailgun")
			for _, m := range domainMetrics {
				metrics = append(metrics, UnifiedDomainMetric{
					Provider:      "mailgun",
					Domain:        m.Domain,
					Volume:        m.Metrics.Targeted,
					Delivered:     m.Metrics.Delivered,
					DeliveryRate:  m.Metrics.DeliveryRate,
					Opens:         m.Metrics.UniqueOpened,
					OpenRate:      m.Metrics.OpenRate,
					Clicks:        m.Metrics.UniqueClicked,
					ClickRate:     m.Metrics.ClickRate,
					Bounces:       m.Metrics.Bounced,
					BounceRate:    m.Metrics.BounceRate,
					Complaints:    m.Metrics.Complaints,
					ComplaintRate: m.Metrics.ComplaintRate,
					Status:        m.Status,
					StatusReason:  m.StatusReason,
				})
			}
		}
	}

	// Note: SES doesn't provide domain-level metrics in the same way
	// It operates at ISP/account level

	respondJSON(w, http.StatusOK, UnifiedDomainResponse{
		Timestamp: time.Now(),
		Metrics:   metrics,
		Providers: providers,
	})
}
