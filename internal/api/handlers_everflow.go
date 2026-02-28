package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/everflow"
)

// EverflowDashboardResponse represents the full Everflow dashboard data
type EverflowDashboardResponse struct {
	Timestamp           time.Time                      `json:"timestamp"`
	LastFetch           time.Time                      `json:"last_fetch"`
	TodayClicks         int64                          `json:"today_clicks"`
	TodayConversions    int64                          `json:"today_conversions"`
	TodayRevenue        float64                        `json:"today_revenue"`
	TodayPayout         float64                        `json:"today_payout"`
	TotalRevenue        float64                        `json:"total_revenue"`
	TotalConversions    int64                          `json:"total_conversions"`
	DailyPerformance    []everflow.DailyPerformance    `json:"daily_performance"`
	OfferPerformance    []everflow.OfferPerformance    `json:"offer_performance"`
	PropertyPerformance []everflow.PropertyPerformance `json:"property_performance"`
	CampaignRevenue     []everflow.CampaignRevenue     `json:"campaign_revenue"`
	RevenueBreakdown    *everflow.RevenueBreakdown     `json:"revenue_breakdown"`
}

// GetEverflowDashboard returns the complete Everflow dashboard data
func (h *Handlers) GetEverflowDashboard(w http.ResponseWriter, r *http.Request) {
	if h.everflowCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow integration not configured")
		return
	}

	// Parse date range from query params (defaults to MTD)
	dateRange := parseDateRange(r)

	metrics := h.everflowCollector.GetLatestMetrics()
	if metrics == nil {
		// Return empty but valid dashboard response during initialization
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"timestamp":            time.Now(),
			"last_fetch":           time.Time{},
			"date_range": map[string]interface{}{
				"type":       dateRange.Type,
				"start_date": dateRange.StartDate.Format("2006-01-02"),
				"end_date":   dateRange.EndDate.Format("2006-01-02"),
			},
			"today_clicks":         0,
			"today_conversions":    0,
			"today_revenue":        0,
			"today_payout":         0,
			"total_revenue":        0,
			"total_conversions":    0,
			"daily_performance":    []everflow.DailyPerformance{},
			"offer_performance":    []everflow.OfferPerformance{},
			"property_performance": []everflow.PropertyPerformance{},
			"campaign_revenue":     []everflow.CampaignRevenue{},
		})
		return
	}

	// Filter daily performance by date range
	dailyPerf := h.everflowCollector.GetDailyPerformanceByDateRange(dateRange.StartDate, dateRange.EndDate)
	if dailyPerf == nil {
		dailyPerf = []everflow.DailyPerformance{}
	}
	
	offerPerf := metrics.OfferPerformance
	if offerPerf == nil {
		offerPerf = []everflow.OfferPerformance{}
	}
	propPerf := metrics.PropertyPerformance
	if propPerf == nil {
		propPerf = []everflow.PropertyPerformance{}
	}
	campRev := metrics.CampaignRevenue
	if campRev == nil {
		campRev = []everflow.CampaignRevenue{}
	}

	// Calculate totals from filtered daily performance
	var totalRevenue float64
	var totalConversions int64
	var totalClicks int64
	var totalPayout float64
	for _, d := range dailyPerf {
		totalRevenue += d.Revenue
		totalConversions += d.Conversions
		totalClicks += d.Clicks
		totalPayout += d.Payout
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":  time.Now(),
		"last_fetch": metrics.LastFetch,
		"date_range": map[string]interface{}{
			"type":       dateRange.Type,
			"start_date": dateRange.StartDate.Format("2006-01-02"),
			"end_date":   dateRange.EndDate.Format("2006-01-02"),
		},
		"today_clicks":         totalClicks,
		"today_conversions":    totalConversions,
		"today_revenue":        totalRevenue,
		"today_payout":         totalPayout,
		"total_revenue":        totalRevenue,
		"total_conversions":    totalConversions,
		"daily_performance":    dailyPerf,
		"offer_performance":    offerPerf,
		"property_performance": propPerf,
		"campaign_revenue":     campRev,
		"revenue_breakdown":    metrics.RevenueBreakdown,
	})
}

// GetEverflowDailyPerformance returns daily performance metrics
func (h *Handlers) GetEverflowDailyPerformance(w http.ResponseWriter, r *http.Request) {
	if h.everflowCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow integration not configured")
		return
	}

	// Parse date range from query params (defaults to MTD)
	dateRange := parseDateRange(r)
	
	daily := h.everflowCollector.GetDailyPerformanceByDateRange(dateRange.StartDate, dateRange.EndDate)
	if daily == nil {
		daily = []everflow.DailyPerformance{}
	}

	// Calculate totals
	var totalClicks, totalConversions int64
	var totalRevenue, totalPayout float64
	for _, d := range daily {
		totalClicks += d.Clicks
		totalConversions += d.Conversions
		totalRevenue += d.Revenue
		totalPayout += d.Payout
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":        time.Now(),
		"date_range": map[string]interface{}{
			"type":       dateRange.Type,
			"start_date": dateRange.StartDate.Format("2006-01-02"),
			"end_date":   dateRange.EndDate.Format("2006-01-02"),
		},
		"days":             len(daily),
		"daily":            daily,
		"totals": map[string]interface{}{
			"clicks":      totalClicks,
			"conversions": totalConversions,
			"revenue":     totalRevenue,
			"payout":      totalPayout,
		},
	})
}

// GetEverflowWeeklyPerformance returns weekly performance metrics
func (h *Handlers) GetEverflowWeeklyPerformance(w http.ResponseWriter, r *http.Request) {
	if h.everflowCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow integration not configured")
		return
	}

	weekly := h.everflowCollector.GetWeeklyPerformance()

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now(),
		"weeks":     len(weekly),
		"weekly":    weekly,
	})
}

// GetEverflowMonthlyPerformance returns monthly performance metrics
func (h *Handlers) GetEverflowMonthlyPerformance(w http.ResponseWriter, r *http.Request) {
	if h.everflowCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow integration not configured")
		return
	}

	monthly := h.everflowCollector.GetMonthlyPerformance()

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now(),
		"months":    len(monthly),
		"monthly":   monthly,
	})
}

// GetEverflowOfferPerformance returns offer-level performance metrics
func (h *Handlers) GetEverflowOfferPerformance(w http.ResponseWriter, r *http.Request) {
	if h.everflowCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow integration not configured")
		return
	}

	offers := h.everflowCollector.GetOfferPerformance()
	if offers == nil {
		offers = []everflow.OfferPerformance{}
	}

	// Calculate totals
	var totalRevenue float64
	for _, o := range offers {
		totalRevenue += o.Revenue
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":     time.Now(),
		"count":         len(offers),
		"offers":        offers,
		"total_revenue": totalRevenue,
	})
}

// GetEverflowPropertyPerformance returns property/domain performance metrics
func (h *Handlers) GetEverflowPropertyPerformance(w http.ResponseWriter, r *http.Request) {
	if h.everflowCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow integration not configured")
		return
	}

	properties := h.everflowCollector.GetPropertyPerformance()
	if properties == nil {
		properties = []everflow.PropertyPerformance{}
	}

	// Calculate totals
	var totalRevenue float64
	for _, p := range properties {
		totalRevenue += p.Revenue
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":     time.Now(),
		"count":         len(properties),
		"properties":    properties,
		"total_revenue": totalRevenue,
	})
}

// GetEverflowCampaignRevenue returns campaign-level revenue metrics
func (h *Handlers) GetEverflowCampaignRevenue(w http.ResponseWriter, r *http.Request) {
	if h.everflowCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow integration not configured")
		return
	}

	campaigns := h.everflowCollector.GetCampaignRevenue()
	if campaigns == nil {
		campaigns = []everflow.CampaignRevenue{}
	}

	// Check for min_audience filter parameter
	// Data is pre-enriched in background, so filtering is instant
	minAudienceStr := r.URL.Query().Get("min_audience")
	if minAudienceStr != "" {
		minAudience := int64(0)
		fmt.Sscanf(minAudienceStr, "%d", &minAudience)
		
		if minAudience > 0 {
			// Filter campaigns by pre-enriched audience size (no API calls needed)
			filteredCampaigns := make([]everflow.CampaignRevenue, 0)
			for _, campaign := range campaigns {
				// Include if audience >= minAudience, or if not Ongage linked (don't filter unknowns)
				if campaign.AudienceSize >= minAudience || !campaign.OngageLinked {
					filteredCampaigns = append(filteredCampaigns, campaign)
				}
			}
			campaigns = filteredCampaigns
		}
	}

	// Calculate totals
	var totalRevenue float64
	for _, c := range campaigns {
		totalRevenue += c.Revenue
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":     time.Now(),
		"count":         len(campaigns),
		"campaigns":     campaigns,
		"total_revenue": totalRevenue,
	})
}

// GetEverflowRecentConversions returns recent conversions
func (h *Handlers) GetEverflowRecentConversions(w http.ResponseWriter, r *http.Request) {
	if h.everflowCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow integration not configured")
		return
	}

	conversions := h.everflowCollector.GetRecentConversions()
	if conversions == nil {
		conversions = []everflow.Conversion{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":   time.Now(),
		"count":       len(conversions),
		"conversions": conversions,
	})
}

// GetEverflowRecentClicks returns recent clicks
func (h *Handlers) GetEverflowRecentClicks(w http.ResponseWriter, r *http.Request) {
	if h.everflowCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow integration not configured")
		return
	}

	clicks := h.everflowCollector.GetRecentClicks()
	if clicks == nil {
		clicks = []everflow.Click{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now(),
		"count":     len(clicks),
		"clicks":    clicks,
	})
}

// GetDataPartnerAnalytics returns data partner performance analytics
// derived from Everflow sub2 parameter (data set codes).
func (h *Handlers) GetDataPartnerAnalytics(w http.ResponseWriter, r *http.Request) {
	if h.everflowCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow integration not configured")
		return
	}

	dateRange := parseDateRange(r)
	result := h.everflowCollector.GetDataPartnerAnalytics(dateRange.StartDate, dateRange.EndDate)

	respondJSON(w, http.StatusOK, result)
}

// RefreshDataPartnerCache triggers a manual rebuild of the data partner analytics cache
func (h *Handlers) RefreshDataPartnerCache(w http.ResponseWriter, r *http.Request) {
	if h.everflowCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow integration not configured")
		return
	}

	result := h.everflowCollector.RefreshDataPartnerCache()
	respondJSON(w, http.StatusOK, result)
}

// GetEverflowHealth returns the health status of Everflow integration
func (h *Handlers) GetEverflowHealth(w http.ResponseWriter, r *http.Request) {
	if h.everflowCollector == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"status":    "disabled",
			"message":   "Everflow integration not configured",
			"timestamp": time.Now(),
		})
		return
	}

	lastFetch := h.everflowCollector.LastFetch()
	status := "healthy"

	// Consider unhealthy if no data in last 15 minutes
	if time.Since(lastFetch) > 15*time.Minute && !lastFetch.IsZero() {
		status = "degraded"
	}
	if lastFetch.IsZero() {
		status = "initializing"
	}

	metrics := h.everflowCollector.GetLatestMetrics()
	var todayRevenue float64
	var totalRevenue float64
	if metrics != nil {
		todayRevenue = metrics.TodayRevenue
		totalRevenue = h.everflowCollector.GetTotalRevenue()
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":        status,
		"timestamp":     time.Now(),
		"last_fetch":    lastFetch,
		"today_revenue": todayRevenue,
		"total_revenue": totalRevenue,
	})
}

// GetEverflowRevenueBreakdown returns CPM vs Non-CPM revenue breakdown
func (h *Handlers) GetEverflowRevenueBreakdown(w http.ResponseWriter, r *http.Request) {
	if h.everflowCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow integration not configured")
		return
	}

	breakdown := h.everflowCollector.GetRevenueBreakdown()
	if breakdown == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "initializing",
			"message": "Revenue breakdown data is being collected",
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now(),
		"breakdown": breakdown,
	})
}

// GetEverflowESPRevenue returns revenue breakdown by ESP (SparkPost, Mailgun, SES)
func (h *Handlers) GetEverflowESPRevenue(w http.ResponseWriter, r *http.Request) {
	if h.everflowCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow integration not configured")
		return
	}

	espRevenue := h.everflowCollector.GetESPRevenue()
	if espRevenue == nil || len(espRevenue) == 0 {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "initializing",
			"message": "ESP revenue data is being collected. This requires Ongage campaign enrichment.",
		})
		return
	}

	// Calculate totals
	var totalRevenue, totalPayout float64
	var totalSent, totalDelivered, totalClicks, totalConversions int64
	for _, esp := range espRevenue {
		totalRevenue += esp.Revenue
		totalPayout += esp.Payout
		totalSent += esp.TotalSent
		totalDelivered += esp.TotalDelivered
		totalClicks += esp.Clicks
		totalConversions += esp.Conversions
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":         time.Now(),
		"esp_revenue":       espRevenue,
		"total_revenue":     totalRevenue,
		"total_payout":      totalPayout,
		"total_sent":        totalSent,
		"total_delivered":   totalDelivered,
		"total_clicks":      totalClicks,
		"total_conversions": totalConversions,
	})
}

// GetEverflowESPContracts returns loaded ESP contracts for debugging
func (h *Handlers) GetEverflowESPContracts(w http.ResponseWriter, r *http.Request) {
	if h.everflowCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow integration not configured")
		return
	}

	contracts := h.everflowCollector.GetLoadedContracts()
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"loaded":    contracts != nil,
		"count":     len(contracts),
		"contracts": contracts,
	})
}

// GetEverflowCampaignDetails returns enriched campaign details by mailing ID
func (h *Handlers) GetEverflowCampaignDetails(w http.ResponseWriter, r *http.Request) {
	if h.enrichmentService == nil {
		respondError(w, http.StatusServiceUnavailable, "Enrichment service not configured")
		return
	}

	// Get mailing ID from URL path
	mailingID := chi.URLParam(r, "mailingId")
	if mailingID == "" {
		respondError(w, http.StatusBadRequest, "Missing mailing ID")
		return
	}

	// Get enriched details
	ctx := r.Context()
	details, err := h.enrichmentService.GetEnrichedCampaignDetails(ctx, mailingID)
	if err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("Campaign not found: %v", err))
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now(),
		"campaign":  details,
	})
}
