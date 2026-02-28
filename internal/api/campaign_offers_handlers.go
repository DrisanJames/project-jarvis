package api

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/everflow"
)

// AffiliateInfo contains information about an Everflow affiliate
type AffiliateInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// OfferWithMetrics extends OfferPerformance with additional AI-computed metrics
type OfferWithMetrics struct {
	OfferID          string  `json:"offer_id"`
	OfferName        string  `json:"offer_name"`
	OfferType        string  `json:"offer_type"` // CPM, CPL, CPA, etc.
	AdvertiserName   string  `json:"advertiser_name,omitempty"`
	
	// Performance Metrics (from lookback period)
	Clicks           int64   `json:"clicks"`
	Conversions      int64   `json:"conversions"`
	Revenue          float64 `json:"revenue"`
	Payout           float64 `json:"payout"`
	ConversionRate   float64 `json:"conversion_rate"`
	EPC              float64 `json:"epc"` // Earnings per click
	
	// Today's Performance
	TodayClicks      int64   `json:"today_clicks"`
	TodayConversions int64   `json:"today_conversions"`
	TodayRevenue     float64 `json:"today_revenue"`
	
	// Trend (compared to 7-day average)
	RevenueTrend     string  `json:"revenue_trend"` // "up", "down", "stable"
	TrendPercentage  float64 `json:"trend_percentage"`
	
	// AI Scoring
	AIScore          float64 `json:"ai_score"`          // 0-100 composite score
	AIRecommendation string  `json:"ai_recommendation"` // "highly_recommended", "recommended", "neutral", "caution"
	AIReason         string  `json:"ai_reason"`         // Explanation for the recommendation
}

// NetworkStats contains overall network performance stats
type NetworkStats struct {
	TotalRevenue        float64 `json:"total_revenue"`
	TotalClicks         int64   `json:"total_clicks"`
	TotalConversions    int64   `json:"total_conversions"`
	AvgConversionRate   float64 `json:"avg_conversion_rate"`
	AvgEPC              float64 `json:"avg_epc"`
	TopPerformingType   string  `json:"top_performing_type"` // CPM, CPL, etc.
}

// CampaignOffersResponse is the response for the campaign offers endpoint
type CampaignOffersResponse struct {
	Timestamp              time.Time                                `json:"timestamp"`
	AffiliateID            string                                   `json:"affiliate_id"`
	AffiliateName          string                                   `json:"affiliate_name"`
	LookbackDays           int                                      `json:"lookback_days"`
	TotalOffers            int                                      `json:"total_offers"`
	Offers                 []OfferWithMetrics                       `json:"offers"`
	TopRecommendations     []everflow.AudienceMatchRecommendation   `json:"top_recommendations"`
	NetworkStats           NetworkStats                             `json:"network_stats"`
}

// GetAvailableAffiliates returns the list of configured Everflow affiliates
func (h *Handlers) GetAvailableAffiliates(w http.ResponseWriter, r *http.Request) {
	if h.everflowCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow integration not configured")
		return
	}

	client := h.everflowCollector.GetClient()
	if client == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow client not available")
		return
	}

	affiliateIDs := client.GetAffiliateIDs()
	affiliates := make([]AffiliateInfo, len(affiliateIDs))
	
	affiliateNames := map[string]string{
		"9533": "Ignite Media Internal Email",
		"9572": "Ignite Media Internal Email 4",
	}
	
	for i, id := range affiliateIDs {
		name := affiliateNames[id]
		if name == "" {
			name = "Affiliate " + id
		}
		affiliates[i] = AffiliateInfo{
			ID:          id,
			Name:        name,
			Description: "Everflow affiliate network",
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"affiliates": affiliates,
		"count":      len(affiliates),
	})
}

// GetNetworkTopOffers returns the top-performing offers across the ENTIRE Everflow network
// This is the primary endpoint called when the user selects "Offer Revenue" - no affiliate filter needed
func (h *Handlers) GetNetworkTopOffers(w http.ResponseWriter, r *http.Request) {
	// First try the cached network intelligence (background worker)
	if h.networkIntelCollector != nil {
		snapshot := h.networkIntelCollector.GetSnapshot()
		if snapshot != nil && time.Since(snapshot.LastUpdated) < 30*time.Minute {
			// Serve from cache - this is the fast path
			respondJSON(w, http.StatusOK, map[string]interface{}{
				"timestamp":                snapshot.Timestamp,
				"last_updated":             snapshot.LastUpdated,
				"top_offers":               snapshot.TopOffers,
				"audience_recommendations":  snapshot.AudienceRecommendations,
				"network_total_clicks":      snapshot.NetworkTotalClicks,
				"network_total_conversions": snapshot.NetworkTotalConversions,
				"network_total_revenue":     snapshot.NetworkTotalRevenue,
				"network_avg_cvr":           snapshot.NetworkAvgCVR,
				"network_avg_epc":           snapshot.NetworkAvgEPC,
				"clicks_analyzed":           snapshot.TotalClicksAnalyzed,
				"conversions_analyzed":      snapshot.TotalConversionsAnalyzed,
				"source":                    "cached",
			})
			return
		}
	}

	// Fallback: fetch network-wide data on demand (slower)
	if h.everflowCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow integration not configured")
		return
	}

	client := h.everflowCollector.GetClient()
	if client == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow client not available")
		return
	}

	ctx := context.Background()
	now := time.Now()
	sevenDaysAgo := now.AddDate(0, 0, -7)
	todayStart := now.Truncate(24 * time.Hour)

	// Fetch network-wide offer performance
	offerReport, err := client.GetEntityReportByOfferNetworkWide(ctx, sevenDaysAgo, now)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to fetch network offer data: "+err.Error())
		return
	}

	todayReport, _ := client.GetEntityReportByOfferNetworkWide(ctx, todayStart, now)

	// Build today map
	todayMap := make(map[string]*everflow.EntityReportRow)
	if todayReport != nil {
		for i := range todayReport.Table {
			row := &todayReport.Table[i]
			for _, col := range row.Columns {
				if col.ColumnType == "offer" {
					todayMap[col.ID] = row
					break
				}
			}
		}
	}

	// Process offers
	var networkOffers []everflow.NetworkOfferIntelligence
	var totalClicks, totalConversions int64
	var totalRevenue float64

	for _, row := range offerReport.Table {
		var offerID, offerName string
		for _, col := range row.Columns {
			if col.ColumnType == "offer" {
				offerID = col.ID
				offerName = col.Label
				break
			}
		}
		if offerID == "" {
			continue
		}

		offer := everflow.NetworkOfferIntelligence{
			OfferID:            offerID,
			OfferName:          offerName,
			OfferType:          everflow.GetOfferType(offerName),
			NetworkClicks:      row.Reporting.TotalClick,
			NetworkConversions: row.Reporting.Conversions,
			NetworkRevenue:     row.Reporting.Revenue,
		}

		if row.Reporting.TotalClick > 0 {
			offer.NetworkCVR = float64(row.Reporting.Conversions) / float64(row.Reporting.TotalClick) * 100
			offer.NetworkEPC = row.Reporting.Revenue / float64(row.Reporting.TotalClick)
		}

		if todayRow, ok := todayMap[offerID]; ok {
			offer.TodayClicks = todayRow.Reporting.TotalClick
			offer.TodayConversions = todayRow.Reporting.Conversions
			offer.TodayRevenue = todayRow.Reporting.Revenue
			if todayRow.Reporting.TotalClick > 0 {
				offer.TodayEPC = todayRow.Reporting.Revenue / float64(todayRow.Reporting.TotalClick)
			}
		}

		// Trend
		dailyAvg := row.Reporting.Revenue / 7.0
		if dailyAvg > 0 {
			trendPct := ((offer.TodayRevenue - dailyAvg) / dailyAvg) * 100
			offer.TrendPercentage = math.Round(trendPct*100) / 100
			if trendPct > 15 {
				offer.RevenueTrend = "accelerating"
			} else if trendPct < -15 {
				offer.RevenueTrend = "decelerating"
			} else {
				offer.RevenueTrend = "stable"
			}
		} else {
			offer.RevenueTrend = "stable"
		}

		// AI Score
		offer.AIScore, offer.AIRecommendation, offer.AIReason = calculateNetworkScore(offer, offerReport.Summary)

		networkOffers = append(networkOffers, offer)
		totalClicks += row.Reporting.TotalClick
		totalConversions += row.Reporting.Conversions
		totalRevenue += row.Reporting.Revenue
	}

	// Sort by today's revenue
	sort.Slice(networkOffers, func(i, j int) bool {
		return networkOffers[i].TodayRevenue > networkOffers[j].TodayRevenue
	})

	for i := range networkOffers {
		networkOffers[i].NetworkRank = i + 1
	}

	if len(networkOffers) > 50 {
		networkOffers = networkOffers[:50]
	}

	avgCVR := 0.0
	avgEPC := 0.0
	if totalClicks > 0 {
		avgCVR = float64(totalConversions) / float64(totalClicks) * 100
		avgEPC = totalRevenue / float64(totalClicks)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":                time.Now(),
		"top_offers":               networkOffers,
		"audience_recommendations":  []everflow.AudienceMatchRecommendation{}, // No metadata in fallback mode
		"network_total_clicks":      totalClicks,
		"network_total_conversions": totalConversions,
		"network_total_revenue":     totalRevenue,
		"network_avg_cvr":           math.Round(avgCVR*100) / 100,
		"network_avg_epc":           math.Round(avgEPC*1000) / 1000,
		"source":                    "on_demand",
	})
}

// GetCampaignOffers returns available offers for campaign selection with AI recommendations
// This endpoint supports BOTH network-wide mode (no affiliate_id) and affiliate-filtered mode
func (h *Handlers) GetCampaignOffers(w http.ResponseWriter, r *http.Request) {
	if h.everflowCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow integration not configured")
		return
	}

	// Parse query parameters
	affiliateID := r.URL.Query().Get("affiliate_id")
	lookbackDaysStr := r.URL.Query().Get("lookback_days")
	offerType := r.URL.Query().Get("offer_type")
	
	lookbackDays := 7
	if lookbackDaysStr != "" {
		if d, err := strconv.Atoi(lookbackDaysStr); err == nil && d > 0 && d <= 30 {
			lookbackDays = d
		}
	}

	client := h.everflowCollector.GetClient()
	if client == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow client not available")
		return
	}

	// Determine which affiliate IDs to query
	var affiliateIDs []string
	if affiliateID != "" {
		affiliateIDs = []string{affiliateID}
	} else {
		affiliateIDs = client.GetAffiliateIDs()
	}

	endDate := time.Now()
	startDate := endDate.AddDate(0, 0, -lookbackDays)
	todayStart := time.Now().Truncate(24 * time.Hour)

	ctx := context.Background()

	offerReport, err := client.GetEntityReportByOffer(ctx, startDate, endDate, affiliateIDs)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to fetch offer data: "+err.Error())
		return
	}

	todayReport, _ := client.GetEntityReportByOffer(ctx, todayStart, endDate, affiliateIDs)

	todayMap := make(map[string]*everflow.EntityReportRow)
	if todayReport != nil {
		for i := range todayReport.Table {
			row := &todayReport.Table[i]
			for _, col := range row.Columns {
				if col.ColumnType == "offer" {
					todayMap[col.ID] = row
					break
				}
			}
		}
	}

	sevenDaysAgo := endDate.AddDate(0, 0, -7)
	historicalReport, _ := client.GetEntityReportByOffer(ctx, sevenDaysAgo, endDate, affiliateIDs)
	
	historicalAvg := make(map[string]float64)
	if historicalReport != nil {
		for _, row := range historicalReport.Table {
			for _, col := range row.Columns {
				if col.ColumnType == "offer" {
					historicalAvg[col.ID] = row.Reporting.Revenue / 7.0
					break
				}
			}
		}
	}

	offers := make([]OfferWithMetrics, 0, len(offerReport.Table))
	var totalRevenue float64
	var totalClicks, totalConversions int64

	for _, row := range offerReport.Table {
		var offerID, offerName string
		for _, col := range row.Columns {
			if col.ColumnType == "offer" {
				offerID = col.ID
				offerName = col.Label
				break
			}
		}
		if offerID == "" {
			continue
		}

		detectedType := everflow.GetOfferType(offerName)
		if offerType != "" && !strings.EqualFold(detectedType, offerType) {
			continue
		}

		offer := OfferWithMetrics{
			OfferID:        offerID,
			OfferName:      offerName,
			OfferType:      detectedType,
			Clicks:         row.Reporting.TotalClick,
			Conversions:    row.Reporting.Conversions,
			Revenue:        row.Reporting.Revenue,
			Payout:         row.Reporting.Payout,
			ConversionRate: row.Reporting.CVR,
			EPC:            row.Reporting.RPC,
		}

		if todayRow, ok := todayMap[offerID]; ok {
			offer.TodayClicks = todayRow.Reporting.TotalClick
			offer.TodayConversions = todayRow.Reporting.Conversions
			offer.TodayRevenue = todayRow.Reporting.Revenue
		}

		if avgRevenue, ok := historicalAvg[offerID]; ok && avgRevenue > 0 {
			trendPct := ((offer.TodayRevenue - avgRevenue) / avgRevenue) * 100
			offer.TrendPercentage = math.Round(trendPct*100) / 100
			if trendPct > 10 {
				offer.RevenueTrend = "up"
			} else if trendPct < -10 {
				offer.RevenueTrend = "down"
			} else {
				offer.RevenueTrend = "stable"
			}
		} else {
			offer.RevenueTrend = "stable"
		}

		offer.AIScore, offer.AIRecommendation, offer.AIReason = calculateOfferScore(offer, offerReport.Summary)

		offers = append(offers, offer)
		totalRevenue += offer.Revenue
		totalClicks += offer.Clicks
		totalConversions += offer.Conversions
	}

	sort.Slice(offers, func(i, j int) bool {
		return offers[i].AIScore > offers[j].AIScore
	})

	// Get audience match recommendations from network intelligence
	var audienceRecs []everflow.AudienceMatchRecommendation
	if h.networkIntelCollector != nil {
		snapshot := h.networkIntelCollector.GetSnapshot()
		if snapshot != nil {
			audienceRecs = snapshot.AudienceRecommendations
		}
	}

	// Filter to just the offers in our affiliate set
	if len(audienceRecs) > 0 && len(offers) > 0 {
		offerIDSet := make(map[string]bool)
		for _, o := range offers {
			offerIDSet[o.OfferID] = true
		}
		var filteredRecs []everflow.AudienceMatchRecommendation
		for _, rec := range audienceRecs {
			if offerIDSet[rec.OfferID] {
				filteredRecs = append(filteredRecs, rec)
			}
		}
		// If we have affiliate-specific recs, use them; otherwise show network-wide
		if len(filteredRecs) > 0 {
			audienceRecs = filteredRecs
		}
	}

	// Limit to top 5 recommendations
	if len(audienceRecs) > 5 {
		audienceRecs = audienceRecs[:5]
	}

	typeRevenue := make(map[string]float64)
	for _, o := range offers {
		typeRevenue[o.OfferType] += o.Revenue
	}
	topType := "CPM"
	topTypeRevenue := 0.0
	for t, rev := range typeRevenue {
		if rev > topTypeRevenue {
			topType = t
			topTypeRevenue = rev
		}
	}

	avgCVR := 0.0
	avgEPC := 0.0
	if totalClicks > 0 {
		avgCVR = float64(totalConversions) / float64(totalClicks) * 100
		avgEPC = totalRevenue / float64(totalClicks)
	}

	affiliateName := "All Affiliates"
	if affiliateID != "" {
		affiliateNames := map[string]string{
			"9533": "Ignite Media Internal Email",
			"9572": "Ignite Media Internal Email 4",
		}
		if name, ok := affiliateNames[affiliateID]; ok {
			affiliateName = name
		}
	}

	response := CampaignOffersResponse{
		Timestamp:          time.Now(),
		AffiliateID:        affiliateID,
		AffiliateName:      affiliateName,
		LookbackDays:       lookbackDays,
		TotalOffers:        len(offers),
		Offers:             offers,
		TopRecommendations: audienceRecs,
		NetworkStats: NetworkStats{
			TotalRevenue:      totalRevenue,
			TotalClicks:       totalClicks,
			TotalConversions:  totalConversions,
			AvgConversionRate: math.Round(avgCVR*100) / 100,
			AvgEPC:            math.Round(avgEPC*1000) / 1000,
			TopPerformingType: topType,
		},
	}

	respondJSON(w, http.StatusOK, response)
}

// calculateOfferScore computes an AI score for an affiliate-filtered offer
func calculateOfferScore(offer OfferWithMetrics, summary everflow.EntityReportSummary) (float64, string, string) {
	var score float64
	var reasons []string

	// Factor 1: Revenue contribution (0-30 points)
	if summary.Revenue > 0 {
		revenueShare := (offer.Revenue / summary.Revenue) * 100
		revenueScore := math.Min(revenueShare*3, 30)
		score += revenueScore
		if revenueShare > 5 {
			reasons = append(reasons, "High revenue contributor")
		}
	}

	// Factor 2: Conversion rate vs average (0-25 points)
	avgCVR := 0.0
	if summary.TotalClick > 0 {
		avgCVR = float64(summary.Conversions) / float64(summary.TotalClick) * 100
	}
	if offer.ConversionRate > avgCVR*1.5 && avgCVR > 0 {
		score += 25
		reasons = append(reasons, "Above average conversion rate")
	} else if offer.ConversionRate > avgCVR {
		score += 15
	} else if offer.ConversionRate > avgCVR*0.5 {
		score += 5
	}

	// Factor 3: EPC (0-20 points)
	avgEPC := 0.0
	if summary.TotalClick > 0 {
		avgEPC = summary.Revenue / float64(summary.TotalClick)
	}
	if offer.EPC > avgEPC*1.5 && avgEPC > 0 {
		score += 20
		reasons = append(reasons, "Strong earnings per click")
	} else if offer.EPC > avgEPC {
		score += 12
	} else if offer.EPC > avgEPC*0.5 {
		score += 5
	}

	// Factor 4: Trend (0-15 points)
	if offer.RevenueTrend == "up" {
		score += 15
		reasons = append(reasons, "Trending upward today")
	} else if offer.RevenueTrend == "stable" {
		score += 8
	}

	// Factor 5: Volume/Activity (0-10 points)
	if offer.TodayClicks > 100 {
		score += 10
		reasons = append(reasons, "High activity today")
	} else if offer.TodayClicks > 50 {
		score += 7
	} else if offer.TodayClicks > 10 {
		score += 4
	}

	score = math.Min(score, 100)
	score = math.Round(score*10) / 10

	var recommendation string
	var reason string
	
	if score >= 75 {
		recommendation = "highly_recommended"
		if len(reasons) > 0 {
			reason = strings.Join(reasons[:min(2, len(reasons))], ". ") + "."
		} else {
			reason = "Strong overall performance metrics."
		}
	} else if score >= 50 {
		recommendation = "recommended"
		if len(reasons) > 0 {
			reason = reasons[0] + "."
		} else {
			reason = "Good performance with room for growth."
		}
	} else if score >= 25 {
		recommendation = "neutral"
		reason = "Average performance. Consider testing."
	} else {
		recommendation = "caution"
		reason = "Below average performance. May need optimization."
	}

	return score, recommendation, reason
}

// calculateNetworkScore computes an AI score for a network-wide offer (used in fallback mode)
func calculateNetworkScore(offer everflow.NetworkOfferIntelligence, summary everflow.EntityReportSummary) (float64, string, string) {
	var score float64
	var reasons []string

	if summary.Revenue > 0 {
		revenueShare := (offer.NetworkRevenue / summary.Revenue) * 100
		score += math.Min(revenueShare*2.5, 25)
		if revenueShare > 3 {
			reasons = append(reasons, "Top network revenue contributor")
		}
	}

	if offer.TodayRevenue > 0 && offer.RevenueTrend == "accelerating" {
		score += 25
		reasons = append(reasons, "Revenue accelerating today")
	} else if offer.RevenueTrend == "stable" {
		score += 15
	} else {
		score += 5
	}

	avgCVR := 0.0
	if summary.TotalClick > 0 {
		avgCVR = float64(summary.Conversions) / float64(summary.TotalClick) * 100
	}
	if offer.NetworkCVR > avgCVR*1.5 && avgCVR > 0 {
		score += 20
	} else if offer.NetworkCVR > avgCVR {
		score += 12
	}

	avgEPC := 0.0
	if summary.TotalClick > 0 {
		avgEPC = summary.Revenue / float64(summary.TotalClick)
	}
	if offer.NetworkEPC > avgEPC*1.5 && avgEPC > 0 {
		score += 15
	} else if offer.NetworkEPC > avgEPC {
		score += 8
	}

	if offer.TodayClicks > 500 {
		score += 10
	} else if offer.TodayClicks > 100 {
		score += 7
	}

	score = math.Min(score, 100)
	score = math.Round(score*10) / 10

	var recommendation, reason string
	if score >= 75 {
		recommendation = "highly_recommended"
		if len(reasons) >= 2 {
			reason = strings.Join(reasons[:2], ". ") + "."
		} else if len(reasons) == 1 {
			reason = reasons[0] + "."
		} else {
			reason = "Top performer across the network."
		}
	} else if score >= 50 {
		recommendation = "recommended"
		if len(reasons) > 0 {
			reason = reasons[0] + "."
		} else {
			reason = "Good network performance."
		}
	} else if score >= 25 {
		recommendation = "neutral"
		reason = "Average network performance."
	} else {
		recommendation = "caution"
		reason = "Below average network performance."
	}

	return score, recommendation, reason
}

// min returns the smaller of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// GetOfferDetails returns detailed information about a specific offer
func (h *Handlers) GetOfferDetails(w http.ResponseWriter, r *http.Request) {
	if h.everflowCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow integration not configured")
		return
	}

	offerID := r.URL.Query().Get("offer_id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer_id is required")
		return
	}

	affiliateID := r.URL.Query().Get("affiliate_id")

	client := h.everflowCollector.GetClient()
	if client == nil {
		respondError(w, http.StatusServiceUnavailable, "Everflow client not available")
		return
	}

	var affiliateIDs []string
	if affiliateID != "" {
		affiliateIDs = []string{affiliateID}
	}

	ctx := context.Background()
	
	endDate := time.Now()
	startDate := endDate.AddDate(0, 0, -30)
	
	dailyReport, err := client.GetEntityReportByDate(ctx, startDate, endDate, affiliateIDs)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to fetch offer details")
		return
	}

	offerReport, _ := client.GetEntityReportByOffer(ctx, startDate, endDate, affiliateIDs)

	var offerData *everflow.EntityReportRow
	if offerReport != nil {
		for i := range offerReport.Table {
			for _, col := range offerReport.Table[i].Columns {
				if col.ColumnType == "offer" && col.ID == offerID {
					offerData = &offerReport.Table[i]
					break
				}
			}
		}
	}

	// Also include network intelligence for this offer if available
	var audienceProfile *everflow.AudienceProfile
	if h.networkIntelCollector != nil {
		snapshot := h.networkIntelCollector.GetSnapshot()
		if snapshot != nil {
			for _, networkOffer := range snapshot.TopOffers {
				if networkOffer.OfferID == offerID {
					audienceProfile = networkOffer.AudienceProfile
					break
				}
			}
		}
	}

	response := map[string]interface{}{
		"offer_id":         offerID,
		"daily_data":       dailyReport,
		"offer_summary":    offerData,
		"audience_profile": audienceProfile,
		"period": map[string]string{
			"start": startDate.Format("2006-01-02"),
			"end":   endDate.Format("2006-01-02"),
		},
	}

	respondJSON(w, http.StatusOK, response)
}
