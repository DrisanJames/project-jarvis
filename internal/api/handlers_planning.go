package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/storage"
)

// ========== Volume Planning Dashboard ==========

// PlanningISPNode represents an ISP node in the planning dashboard
type PlanningISPNode struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	VolumeToday  int64   `json:"volume_today"`   // Aggregate volume across all ESPs
	TotalClicks  int64   `json:"total_clicks"`   // Total clicks
	DeliveryRate float64 `json:"delivery_rate"`  // Average delivery rate
	OpenRate     float64 `json:"open_rate"`      // Average open rate
	ClickRate    float64 `json:"click_rate"`     // Average click rate
	Status       string  `json:"status"`         // healthy, warning, critical
}

// PlanningESPNode represents an ESP node in the planning dashboard
type PlanningESPNode struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	MonthlyAllocation int64   `json:"monthly_allocation"` // From contract (0 = unlimited)
	DailyAllocation   int64   `json:"daily_allocation"`   // Monthly / days in month
	UsedToday         int64   `json:"used_today"`         // Used today
	UsedMTD           int64   `json:"used_mtd"`           // Used month to date
	RemainingToday    int64   `json:"remaining_today"`    // Remaining today (-1 = unlimited)
	RemainingMTD      int64   `json:"remaining_mtd"`      // Remaining this month (-1 = unlimited)
	DailyAverage      int64   `json:"daily_average"`      // Average daily usage this month
	MonthlyFee        float64 `json:"monthly_fee"`        // Contract fee
	OverageRate       float64 `json:"overage_rate"`       // Cost per 1000 overage
	IsPayAsYouGo      bool    `json:"is_pay_as_you_go"`   // True for SES-style pricing
}

// PlanningESPISPConnection represents current routing from an ESP to an ISP
type PlanningESPISPConnection struct {
	ESPID      string `json:"esp_id"`
	ISPID      string `json:"isp_id"`
	Volume     int64  `json:"volume"`      // Volume sent via this route
	Percentage float64 `json:"percentage"` // Percentage of ISP volume
}

// PlanningDashboardResponse is the response for the planning dashboard
type PlanningDashboardResponse struct {
	Timestamp   time.Time                  `json:"timestamp"`
	ISPs        []PlanningISPNode          `json:"isps"`
	ESPs        []PlanningESPNode          `json:"esps"`
	Connections []PlanningESPISPConnection `json:"connections"` // Current routing
}

// GetPlanningDashboard returns data for the volume planning dashboard
func (h *Handlers) GetPlanningDashboard(w http.ResponseWriter, r *http.Request) {
	// Priority ISPs to include (most significant volume)
	priorityISPs := map[string]bool{
		"yahoo":     true,
		"gmail":     true,
		"aol":       true,
		"hotmail":   true,
		"outlook":   true,
		"att":       true,
		"icloud":    true,
		"cox":       true,
		"sbcglobal": true,
		"bellsouth": true,
		"comcast":   true,
		"verizon":   true,
	}

	// Normalize ISP name for matching
	normalizeISP := func(name string) string {
		lower := strings.ToLower(name)
		// Handle variations
		if strings.Contains(lower, "yahoo") {
			return "yahoo"
		}
		if strings.Contains(lower, "gmail") || strings.Contains(lower, "google") {
			return "gmail"
		}
		if strings.Contains(lower, "aol") {
			return "aol"
		}
		if strings.Contains(lower, "hotmail") || strings.Contains(lower, "outlook") || strings.Contains(lower, "microsoft") || strings.Contains(lower, "live.com") {
			return "hotmail"
		}
		if strings.Contains(lower, "att") || strings.Contains(lower, "at&t") {
			return "att"
		}
		if strings.Contains(lower, "icloud") || strings.Contains(lower, "apple") || strings.Contains(lower, "me.com") {
			return "icloud"
		}
		if strings.Contains(lower, "cox") {
			return "cox"
		}
		if strings.Contains(lower, "sbcglobal") || strings.Contains(lower, "sbc") {
			return "sbcglobal"
		}
		if strings.Contains(lower, "bellsouth") {
			return "bellsouth"
		}
		if strings.Contains(lower, "comcast") || strings.Contains(lower, "xfinity") {
			return "comcast"
		}
		if strings.Contains(lower, "verizon") {
			return "verizon"
		}
		return lower
	}

	// Display name for ISPs
	ispDisplayNames := map[string]string{
		"yahoo":     "Yahoo",
		"gmail":     "Gmail",
		"aol":       "AOL",
		"hotmail":   "Hotmail/Outlook",
		"att":       "AT&T",
		"icloud":    "iCloud",
		"cox":       "Cox",
		"sbcglobal": "SBCGlobal",
		"bellsouth": "BellSouth",
		"comcast":   "Comcast",
		"verizon":   "Verizon",
	}

	// Aggregate ISP metrics from all ESPs
	ispAggregates := make(map[string]*struct {
		Volume       int64
		Delivered    int64
		Opens        int64
		Clicks       int64
		DeliverySum  float64
		OpenSum      float64
		ClickSum     float64
		Count        int
		WorstStatus  string
	})

	// ESP-specific ISP volumes for connections
	espISPVolumes := make(map[string]map[string]int64) // esp -> isp -> volume

	// Helper to aggregate ISP metrics
	addMetrics := func(espName, ispName string, volume, delivered, opens, clicks int64, deliveryRate, openRate, clickRate float64, status string) {
		normalized := normalizeISP(ispName)
		if !priorityISPs[normalized] {
			return
		}

		if ispAggregates[normalized] == nil {
			ispAggregates[normalized] = &struct {
				Volume       int64
				Delivered    int64
				Opens        int64
				Clicks       int64
				DeliverySum  float64
				OpenSum      float64
				ClickSum     float64
				Count        int
				WorstStatus  string
			}{WorstStatus: "healthy"}
		}

		agg := ispAggregates[normalized]
		agg.Volume += volume
		agg.Delivered += delivered
		agg.Opens += opens
		agg.Clicks += clicks
		agg.DeliverySum += deliveryRate
		agg.OpenSum += openRate
		agg.ClickSum += clickRate
		agg.Count++

		// Track worst status
		if status == "critical" {
			agg.WorstStatus = "critical"
		} else if status == "warning" && agg.WorstStatus != "critical" {
			agg.WorstStatus = "warning"
		}

		// Track ESP-ISP volumes
		espKey := strings.ToLower(espName)
		if espISPVolumes[espKey] == nil {
			espISPVolumes[espKey] = make(map[string]int64)
		}
		espISPVolumes[espKey][normalized] += volume
	}

	// Yahoo family domains we'll get from recipient domain metrics
	yahooFamilyDomains := map[string]string{
		"att.net":       "att",
		"sbcglobal.net": "sbcglobal",
		"bellsouth.net": "bellsouth",
		"yahoo.com":     "yahoo",
		"aol.com":       "aol",
	}

	// ISPs to skip from mailbox provider since we get them from recipient domains
	skipFromMailboxProvider := map[string]bool{
		"yahoo": true,
		"aol":   true,
	}

	// Collect Yahoo family from SparkPost recipient domain metrics (domain-level breakout)
	if h.collector != nil {
		for _, m := range h.collector.GetLatestRecipientDomainMetrics() {
			normalizedID, isYahooFamily := yahooFamilyDomains[m.Domain]
			if !isYahooFamily {
				continue
			}
			
			// Add with the domain-specific display name
			if ispAggregates[normalizedID] == nil {
				ispAggregates[normalizedID] = &struct {
					Volume       int64
					Delivered    int64
					Opens        int64
					Clicks       int64
					DeliverySum  float64
					OpenSum      float64
					ClickSum     float64
					Count        int
					WorstStatus  string
				}{WorstStatus: "healthy"}
			}

			agg := ispAggregates[normalizedID]
			agg.Volume += m.Metrics.Targeted
			agg.Delivered += m.Metrics.Delivered
			agg.Opens += m.Metrics.UniqueOpened
			agg.Clicks += m.Metrics.UniqueClicked
			agg.DeliverySum += m.Metrics.DeliveryRate
			agg.OpenSum += m.Metrics.OpenRate
			agg.ClickSum += m.Metrics.ClickRate
			agg.Count++
			if m.Status == "critical" {
				agg.WorstStatus = "critical"
			} else if m.Status == "warning" && agg.WorstStatus != "critical" {
				agg.WorstStatus = "warning"
			}

			// Track ESP-ISP volumes
			if espISPVolumes["sparkpost"] == nil {
				espISPVolumes["sparkpost"] = make(map[string]int64)
			}
			espISPVolumes["sparkpost"][normalizedID] += m.Metrics.Targeted
		}

		// Collect other ISPs from mailbox provider (skip Yahoo/AOL since we have domain breakout)
		for _, m := range h.collector.GetLatestISPMetrics() {
			normalized := normalizeISP(m.Provider)
			if skipFromMailboxProvider[normalized] {
				continue // Skip Yahoo/AOL - we have their domain breakout
			}
			addMetrics("sparkpost", m.Provider, m.Metrics.Targeted, m.Metrics.Delivered,
				m.Metrics.UniqueOpened, m.Metrics.UniqueClicked,
				m.Metrics.DeliveryRate, m.Metrics.OpenRate, m.Metrics.ClickRate, m.Status)
		}
	}

	// Collect from Mailgun (no recipient domain breakout available, use mailbox provider)
	if h.mailgunCollector != nil {
		for _, m := range h.mailgunCollector.GetLatestISPMetrics() {
			addMetrics("mailgun", m.Provider, m.Metrics.Targeted, m.Metrics.Delivered,
				m.Metrics.UniqueOpened, m.Metrics.UniqueClicked,
				m.Metrics.DeliveryRate, m.Metrics.OpenRate, m.Metrics.ClickRate, m.Status)
		}
	}

	// Collect from SES
	if h.sesCollector != nil {
		for _, m := range h.sesCollector.GetLatestISPMetrics() {
			addMetrics("ses", m.Provider, m.Metrics.Targeted, m.Metrics.Delivered,
				m.Metrics.UniqueOpened, m.Metrics.UniqueClicked,
				m.Metrics.DeliveryRate, m.Metrics.OpenRate, m.Metrics.ClickRate, m.Status)
		}
	}

	// Build ISP nodes (sorted by volume)
	var isps []PlanningISPNode
	for id, agg := range ispAggregates {
		if agg.Volume < 10000 { // Minimum 10k volume to include
			continue
		}
		isps = append(isps, PlanningISPNode{
			ID:           id,
			Name:         ispDisplayNames[id],
			VolumeToday:  agg.Volume,
			TotalClicks:  agg.Clicks,
			DeliveryRate: agg.DeliverySum / float64(agg.Count),
			OpenRate:     agg.OpenSum / float64(agg.Count),
			ClickRate:    agg.ClickSum / float64(agg.Count),
			Status:       agg.WorstStatus,
		})
	}

	// Sort ISPs by volume (descending)
	sort.Slice(isps, func(i, j int) bool {
		return isps[i].VolumeToday > isps[j].VolumeToday
	})

	// Build ESP nodes from contracts
	var esps []PlanningESPNode

	// Get contracts from config
	if h.config != nil {
		for _, contract := range h.config.ESPContracts {
			if !contract.Enabled {
				continue
			}

			// Calculate used MTD from collectors
			var usedMTD int64
			espKey := strings.ToLower(contract.ESPName)

			switch {
			case strings.Contains(espKey, "sparkpost"):
				if h.collector != nil {
					summary := h.collector.GetLatestSummary()
					if summary != nil {
						usedMTD = summary.TotalTargeted
					}
				}
			case strings.Contains(espKey, "mailgun"):
				if h.mailgunCollector != nil {
					summary := h.mailgunCollector.GetLatestSummary()
					if summary != nil {
						usedMTD = summary.TotalTargeted
					}
				}
			case strings.Contains(espKey, "ses") || strings.Contains(espKey, "amazon"):
				if h.sesCollector != nil {
					summary := h.sesCollector.GetLatestSummary()
					if summary != nil {
						usedMTD = summary.TotalTargeted
					}
				}
			}

			// Calculate days in current month
			now := time.Now()
			year, month, _ := now.Date()
			daysInMonth := time.Date(year, month+1, 0, 0, 0, 0, 0, now.Location()).Day()
			dayOfMonth := now.Day()

			// Calculate daily allocation
			dailyAllocation := int64(0)
			if daysInMonth > 0 {
				dailyAllocation = contract.MonthlyIncluded / int64(daysInMonth)
			}

			// Get today's usage from the ISP volumes we collected
			var usedToday int64
			if volumes, ok := espISPVolumes[espKey]; ok {
				for _, vol := range volumes {
					usedToday += vol
				}
			}

			// Calculate remaining (monthly)
			remainingMTD := contract.MonthlyIncluded - usedMTD
			isPayAsYouGo := contract.MonthlyIncluded == 0

			// Calculate remaining today (daily allocation - today's usage)
			remainingToday := dailyAllocation - usedToday

			// For pay-as-you-go (like SES with low allocation), mark as unlimited
			if contract.ESPName == "Amazon SES" || isPayAsYouGo {
				isPayAsYouGo = true
				remainingMTD = -1   // -1 indicates unlimited
				remainingToday = -1
			}

			// Calculate daily average from MTD
			dailyAvg := int64(0)
			if dayOfMonth > 0 {
				dailyAvg = usedMTD / int64(dayOfMonth)
			}

			esps = append(esps, PlanningESPNode{
				ID:                strings.ToLower(strings.ReplaceAll(contract.ESPName, " ", "-")),
				Name:              contract.ESPName,
				MonthlyAllocation: contract.MonthlyIncluded,
				DailyAllocation:   dailyAllocation,
				UsedToday:         usedToday,
				UsedMTD:           usedMTD,
				RemainingToday:    remainingToday,
				RemainingMTD:      remainingMTD,
				DailyAverage:      dailyAvg,
				MonthlyFee:        contract.MonthlyFee,
				OverageRate:       contract.OverageRatePer1000,
				IsPayAsYouGo:      isPayAsYouGo,
			})
		}
	}

	// Build connections (current routing)
	var connections []PlanningESPISPConnection
	for espID, ispVolumes := range espISPVolumes {
		for ispID, volume := range ispVolumes {
			if volume < 10000 { // Minimum volume for connection
				continue
			}

			// Calculate percentage of total ISP volume
			totalISPVolume := int64(0)
			if agg, ok := ispAggregates[ispID]; ok {
				totalISPVolume = agg.Volume
			}

			percentage := 0.0
			if totalISPVolume > 0 {
				percentage = float64(volume) / float64(totalISPVolume) * 100
			}

			connections = append(connections, PlanningESPISPConnection{
				ESPID:      espID,
				ISPID:      ispID,
				Volume:     volume,
				Percentage: percentage,
			})
		}
	}

	respondJSON(w, http.StatusOK, PlanningDashboardResponse{
		Timestamp:   time.Now(),
		ISPs:        isps,
		ESPs:        esps,
		Connections: connections,
	})
}

// RoutingPlanRequest represents a request to save a routing plan
type RoutingPlanRequest struct {
	ID          string                `json:"id,omitempty"`
	Name        string                `json:"name"`
	Description string                `json:"description,omitempty"`
	Routes      []RoutingRuleRequest  `json:"routes"`
	IsActive    bool                  `json:"is_active"`
}

// RoutingRuleRequest represents a single routing rule in a request
type RoutingRuleRequest struct {
	ISPID   string `json:"isp_id"`
	ISPName string `json:"isp_name"`
	ESPID   string `json:"esp_id"`
	ESPName string `json:"esp_name"`
}

// GetRoutingPlans returns all saved routing plans
func (h *Handlers) GetRoutingPlans(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	awsStorage := h.storage.GetAWSStorage()
	if awsStorage == nil {
		respondError(w, http.StatusServiceUnavailable, "Storage not available")
		return
	}

	plans, err := awsStorage.GetAllRoutingPlans(ctx)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to get routing plans")
		return
	}

	if plans == nil {
		plans = []storage.RoutingPlan{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"plans": plans,
	})
}

// GetRoutingPlan returns a specific routing plan by ID
func (h *Handlers) GetRoutingPlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	planID := chi.URLParam(r, "id")

	awsStorage := h.storage.GetAWSStorage()
	if awsStorage == nil {
		respondError(w, http.StatusServiceUnavailable, "Storage not available")
		return
	}

	plan, err := awsStorage.GetRoutingPlan(ctx, planID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to get routing plan")
		return
	}

	if plan == nil {
		respondError(w, http.StatusNotFound, "Routing plan not found")
		return
	}

	respondJSON(w, http.StatusOK, plan)
}

// SaveRoutingPlan creates or updates a routing plan
func (h *Handlers) SaveRoutingPlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	awsStorage := h.storage.GetAWSStorage()
	if awsStorage == nil {
		respondError(w, http.StatusServiceUnavailable, "Storage not available")
		return
	}

	var req RoutingPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if req.Name == "" {
		respondError(w, http.StatusBadRequest, "Plan name is required")
		return
	}

	// Convert request to storage type
	routes := make([]storage.RoutingRule, len(req.Routes))
	for i, route := range req.Routes {
		routes[i] = storage.RoutingRule{
			ISPID:   route.ISPID,
			ISPName: route.ISPName,
			ESPID:   route.ESPID,
			ESPName: route.ESPName,
		}
	}

	plan := &storage.RoutingPlan{
		ID:          req.ID,
		Name:        req.Name,
		Description: req.Description,
		Routes:      routes,
		IsActive:    req.IsActive,
	}

	// If updating existing plan, preserve created_at
	if req.ID != "" {
		existingPlan, _ := awsStorage.GetRoutingPlan(ctx, req.ID)
		if existingPlan != nil {
			plan.CreatedAt = existingPlan.CreatedAt
		}
	}

	if err := awsStorage.SaveRoutingPlan(ctx, plan); err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to save routing plan")
		return
	}

	// If this plan should be active, deactivate others
	if req.IsActive {
		_ = awsStorage.SetActiveRoutingPlan(ctx, plan.ID)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"plan":    plan,
	})
}

// DeleteRoutingPlan removes a routing plan
func (h *Handlers) DeleteRoutingPlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	planID := chi.URLParam(r, "id")

	awsStorage := h.storage.GetAWSStorage()
	if awsStorage == nil {
		respondError(w, http.StatusServiceUnavailable, "Storage not available")
		return
	}

	if err := awsStorage.DeleteRoutingPlan(ctx, planID); err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to delete routing plan")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
	})
}

// GetActiveRoutingPlan returns the currently active routing plan
func (h *Handlers) GetActiveRoutingPlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	awsStorage := h.storage.GetAWSStorage()
	if awsStorage == nil {
		respondError(w, http.StatusServiceUnavailable, "Storage not available")
		return
	}

	plan, err := awsStorage.GetActiveRoutingPlan(ctx)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to get active routing plan")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"plan": plan,
	})
}

// SetActiveRoutingPlan sets a plan as the active one
func (h *Handlers) SetActiveRoutingPlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	planID := chi.URLParam(r, "id")

	awsStorage := h.storage.GetAWSStorage()
	if awsStorage == nil {
		respondError(w, http.StatusServiceUnavailable, "Storage not available")
		return
	}

	if err := awsStorage.SetActiveRoutingPlan(ctx, planID); err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to set active routing plan")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
	})
}
