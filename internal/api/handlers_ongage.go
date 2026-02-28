package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/ongage"
)

type OngageDashboardResponse struct {
	Timestamp        time.Time                   `json:"timestamp"`
	TotalCampaigns   int                         `json:"total_campaigns"`
	ActiveCampaigns  int                         `json:"active_campaigns"`
	LastFetch        time.Time                   `json:"last_fetch"`
	Campaigns        []ongage.ProcessedCampaign  `json:"campaigns"`
	ESPConnections   []ongage.ESPConnection      `json:"esp_connections"`
	SubjectAnalysis  []ongage.SubjectLineAnalysis `json:"subject_analysis"`
	ScheduleAnalysis []ongage.ScheduleAnalysis   `json:"schedule_analysis"`
	ESPPerformance   []ongage.ESPPerformance     `json:"esp_performance"`
	AudienceAnalysis []ongage.AudienceAnalysis   `json:"audience_analysis"`
	PipelineMetrics  []ongage.PipelineMetrics    `json:"pipeline_metrics"`
}

// GetOngageDashboard returns the complete Ongage dashboard data
func (h *Handlers) GetOngageDashboard(w http.ResponseWriter, r *http.Request) {
	if h.ongageCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Ongage integration not configured")
		return
	}

	// Parse date range from query parameters (uses global date filter)
	dateRange := parseDateRange(r)
	startDate := dateRange.StartDate
	endDate := dateRange.EndDate
	rangeType := dateRange.Type

	metrics := h.ongageCollector.GetLatestMetrics()
	if metrics == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "no_data",
			"message": "No Ongage metrics data available yet",
		})
		return
	}

	// Filter campaigns by date range and minimum audience size (10k)
	minAudienceSize := int64(10000)
	cutoffDate := startDate
	endCutoff := endDate.Add(24 * time.Hour) // Include the end date
	
	filteredCampaigns := make([]ongage.ProcessedCampaign, 0)
	for _, c := range metrics.Campaigns {
		// Filter by date range (between start and end date)
		if c.ScheduleTime.Before(cutoffDate) || c.ScheduleTime.After(endCutoff) {
			continue
		}
		// Filter by minimum audience size
		if c.Targeted < minAudienceSize {
			continue
		}
		filteredCampaigns = append(filteredCampaigns, c)
	}

	// Filter subject analysis - only include subjects from campaigns with >= 10k audience
	filteredSubjects := make([]ongage.SubjectLineAnalysis, 0)
	for _, s := range metrics.SubjectAnalysis {
		if s.TotalSent >= minAudienceSize {
			filteredSubjects = append(filteredSubjects, s)
		}
	}

	// Filter audience analysis - only include segments with > 10 campaigns
	minCampaignCount := 10
	filteredAudience := make([]ongage.AudienceAnalysis, 0)
	for _, a := range metrics.AudienceAnalysis {
		if a.CampaignCount > minCampaignCount {
			filteredAudience = append(filteredAudience, a)
		}
	}

	// Filter pipeline metrics by date range
	startDateStr := startDate.Format("2006-01-02")
	endDateStr := endDate.Format("2006-01-02")
	filteredPipeline := make([]ongage.PipelineMetrics, 0)
	for _, p := range metrics.PipelineMetrics {
		// Compare dates as strings (YYYY-MM-DD format allows lexicographic comparison)
		if p.Date >= startDateStr && p.Date <= endDateStr {
			filteredPipeline = append(filteredPipeline, p)
		}
	}

	// Get latest day's targeted count from pipeline metrics (most recent available)
	var latestTargeted int64
	if len(metrics.PipelineMetrics) > 0 {
		latestTargeted = metrics.PipelineMetrics[0].TotalTargeted
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":         time.Now(),
		"start_date":        startDate.Format("2006-01-02"),
		"end_date":          endDate.Format("2006-01-02"),
		"range_type":        rangeType,
		"total_campaigns":   len(filteredCampaigns),
		"active_campaigns":  metrics.ActiveCampaigns,
		"last_fetch":        metrics.LastFetch,
		"campaigns":         filteredCampaigns,
		"esp_connections":   metrics.ESPConnections,
		"subject_analysis":  filteredSubjects,
		"schedule_analysis": metrics.ScheduleAnalysis,
		"esp_performance":   metrics.ESPPerformance,
		"audience_analysis": filteredAudience,
		"pipeline_metrics":  filteredPipeline,
		"today_imports":     0, // Will be populated from list imports when available
		"today_targeted":    latestTargeted,
	})
}

// GetOngageCampaigns returns campaigns from Ongage
func (h *Handlers) GetOngageCampaigns(w http.ResponseWriter, r *http.Request) {
	if h.ongageCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Ongage integration not configured")
		return
	}

	// Parse date range parameter (1, 7, or 365 days - default 1)
	daysStr := r.URL.Query().Get("days")
	days := 1
	if daysStr != "" {
		fmt.Sscanf(daysStr, "%d", &days)
		if days != 1 && days != 7 && days != 365 {
			days = 1
		}
	}

	// Parse minimum audience filter (default 10k)
	minAudienceStr := r.URL.Query().Get("min_audience")
	minAudience := int64(10000)
	if minAudienceStr != "" {
		fmt.Sscanf(minAudienceStr, "%d", &minAudience)
	}

	campaigns := h.ongageCollector.GetCampaigns()
	if campaigns == nil {
		campaigns = []ongage.ProcessedCampaign{}
	}

	// Filter by date and audience
	cutoffDate := time.Now().AddDate(0, 0, -days)
	filtered := make([]ongage.ProcessedCampaign, 0)
	for _, c := range campaigns {
		if c.ScheduleTime.Before(cutoffDate) {
			continue
		}
		if c.Targeted < minAudience {
			continue
		}
		filtered = append(filtered, c)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":    time.Now(),
		"days":         days,
		"min_audience": minAudience,
		"count":        len(filtered),
		"campaigns":    filtered,
	})
}

// GetOngageSubjectAnalysis returns subject line analysis
func (h *Handlers) GetOngageSubjectAnalysis(w http.ResponseWriter, r *http.Request) {
	if h.ongageCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Ongage integration not configured")
		return
	}

	// Parse minimum audience filter (default 10k)
	minAudienceStr := r.URL.Query().Get("min_audience")
	minAudience := int64(10000)
	if minAudienceStr != "" {
		fmt.Sscanf(minAudienceStr, "%d", &minAudience)
	}

	analysis := h.ongageCollector.GetSubjectAnalysis()
	if analysis == nil {
		analysis = []ongage.SubjectLineAnalysis{}
	}

	// Filter by minimum audience (total_sent)
	filtered := make([]ongage.SubjectLineAnalysis, 0)
	for _, s := range analysis {
		if s.TotalSent >= minAudience {
			filtered = append(filtered, s)
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":        time.Now(),
		"min_audience":     minAudience,
		"count":            len(filtered),
		"subject_analysis": filtered,
	})
}

// GetOngageScheduleAnalysis returns schedule optimization analysis
func (h *Handlers) GetOngageScheduleAnalysis(w http.ResponseWriter, r *http.Request) {
	if h.ongageCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Ongage integration not configured")
		return
	}

	analysis := h.ongageCollector.GetScheduleAnalysis()
	if analysis == nil {
		analysis = []ongage.ScheduleAnalysis{}
	}

	// Find optimal times
	var optimalTimes []ongage.ScheduleAnalysis
	for _, a := range analysis {
		if a.Performance == "optimal" || a.Performance == "good" {
			optimalTimes = append(optimalTimes, a)
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":       time.Now(),
		"count":           len(analysis),
		"analysis":        analysis,
		"optimal_times":   optimalTimes,
		"optimal_count":   len(optimalTimes),
	})
}

// GetOngageESPPerformance returns ESP performance metrics
func (h *Handlers) GetOngageESPPerformance(w http.ResponseWriter, r *http.Request) {
	if h.ongageCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Ongage integration not configured")
		return
	}

	performance := h.ongageCollector.GetESPPerformance()
	if performance == nil {
		performance = []ongage.ESPPerformance{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":   time.Now(),
		"count":       len(performance),
		"performance": performance,
	})
}

// GetOngageAudienceAnalysis returns audience/segment analysis
func (h *Handlers) GetOngageAudienceAnalysis(w http.ResponseWriter, r *http.Request) {
	if h.ongageCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Ongage integration not configured")
		return
	}

	// Parse minimum campaign count filter (default > 10)
	minCampaignsStr := r.URL.Query().Get("min_campaigns")
	minCampaigns := 10 // Default: only show segments with > 10 campaigns
	if minCampaignsStr != "" {
		fmt.Sscanf(minCampaignsStr, "%d", &minCampaigns)
	}

	analysis := h.ongageCollector.GetAudienceAnalysis()
	if analysis == nil {
		analysis = []ongage.AudienceAnalysis{}
	}

	// Filter by minimum campaign count and count by engagement level
	filtered := make([]ongage.AudienceAnalysis, 0)
	engagementCounts := map[string]int{
		"high":   0,
		"medium": 0,
		"low":    0,
	}
	for _, a := range analysis {
		if a.CampaignCount > minCampaigns {
			filtered = append(filtered, a)
			engagementCounts[a.Engagement]++
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":         time.Now(),
		"min_campaigns":     minCampaigns,
		"count":             len(filtered),
		"audience_analysis": filtered,
		"engagement_counts": engagementCounts,
	})
}

// GetOngagePipelineMetrics returns daily pipeline metrics
func (h *Handlers) GetOngagePipelineMetrics(w http.ResponseWriter, r *http.Request) {
	if h.ongageCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Ongage integration not configured")
		return
	}

	metrics := h.ongageCollector.GetPipelineMetrics()
	if metrics == nil {
		metrics = []ongage.PipelineMetrics{}
	}

	// Calculate totals
	var totalSent, totalDelivered, totalOpens, totalClicks int64
	var totalCampaigns int
	for _, m := range metrics {
		totalSent += m.TotalSent
		totalDelivered += m.TotalDelivered
		totalOpens += m.TotalOpens
		totalClicks += m.TotalClicks
		totalCampaigns += m.CampaignsSent
	}

	var avgDeliveryRate, avgOpenRate, avgClickRate float64
	if totalSent > 0 {
		avgDeliveryRate = float64(totalDelivered) / float64(totalSent)
		avgOpenRate = float64(totalOpens) / float64(totalSent)
		avgClickRate = float64(totalClicks) / float64(totalSent)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":         time.Now(),
		"days":              len(metrics),
		"pipeline_metrics":  metrics,
		"totals": map[string]interface{}{
			"campaigns_sent":    totalCampaigns,
			"total_sent":        totalSent,
			"total_delivered":   totalDelivered,
			"total_opens":       totalOpens,
			"total_clicks":      totalClicks,
			"avg_delivery_rate": avgDeliveryRate,
			"avg_open_rate":     avgOpenRate,
			"avg_click_rate":    avgClickRate,
		},
	})
}

// GetOngageHealth returns the health status of Ongage integration
func (h *Handlers) GetOngageHealth(w http.ResponseWriter, r *http.Request) {
	if h.ongageCollector == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"status":    "disabled",
			"message":   "Ongage integration not configured",
			"timestamp": time.Now(),
		})
		return
	}

	lastFetch := h.ongageCollector.LastFetch()
	status := "healthy"

	// Consider unhealthy if no data in last 10 minutes
	if time.Since(lastFetch) > 10*time.Minute && !lastFetch.IsZero() {
		status = "degraded"
	}
	if lastFetch.IsZero() {
		status = "initializing"
	}

	metrics := h.ongageCollector.GetLatestMetrics()
	var campaignCount, activeCount int
	if metrics != nil {
		campaignCount = metrics.TotalCampaigns
		activeCount = metrics.ActiveCampaigns
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":           status,
		"timestamp":        time.Now(),
		"last_fetch":       lastFetch,
		"total_campaigns":  campaignCount,
		"active_campaigns": activeCount,
	})
}
