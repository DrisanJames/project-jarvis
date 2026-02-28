package financial

import (
	"fmt"
	"time"
)

// GrowthScenario represents a growth projection scenario
type GrowthScenario struct {
	Name           string  `json:"name"`
	Description    string  `json:"description"`
	MonthlyGrowth  float64 `json:"monthly_growth"`  // Percentage (e.g., 5.0 = 5%)
	VolumeGrowth   float64 `json:"volume_growth"`   // Email volume growth %
	ECPMChange     float64 `json:"ecpm_change"`     // eCPM improvement %
	IsDefault      bool    `json:"is_default"`
}

// GrowthDrivers contains the factors that drive revenue growth
type GrowthDrivers struct {
	// Current Metrics (from actual data)
	CurrentMonthlyRevenue  float64 `json:"current_monthly_revenue"`
	CurrentMonthlySent     int64   `json:"current_monthly_sent"`
	CurrentECPM            float64 `json:"current_ecpm"`           // Revenue per 1000 emails
	RevenuePerEmployee     float64 `json:"revenue_per_employee"`
	
	// Volume Metrics by ESP
	ESPVolumes  map[string]int64   `json:"esp_volumes"`
	ESPRevenue  map[string]float64 `json:"esp_revenue"`
	ESPeCPMs    map[string]float64 `json:"esp_ecpms"`
	
	// Growth Levers
	VolumeGrowthPotential   float64 `json:"volume_growth_potential"`   // Based on ESP capacity
	ECPMOptimizationPotential float64 `json:"ecpm_optimization_potential"` // Based on offer mix
	ESPMixOptimization      float64 `json:"esp_mix_optimization"`      // Shift to higher margin ESPs
	
	// Historical Cost Analysis
	HistoricalCosts      []HistoricalCostPoint `json:"historical_costs"`
	CostGrowthRate       float64               `json:"cost_growth_rate"`       // YoY growth rate
	CurrentOperatingCost float64               `json:"current_operating_cost"` // Current ops costs (excl ESP)
	TargetOperatingCost  float64               `json:"target_operating_cost"`  // Target after reduction
	CostReductionTarget  float64               `json:"cost_reduction_target"`
}

// HistoricalCostPoint represents a single month's historical cost
type HistoricalCostPoint struct {
	Month string  `json:"month"`
	Cost  float64 `json:"cost"`
}

// ScenarioForecast represents a complete forecast under a specific scenario
type ScenarioForecast struct {
	Scenario       GrowthScenario  `json:"scenario"`
	Months         []MonthlyPL     `json:"months"`
	AnnualSummary  AnnualSummary   `json:"annual_summary"`
	GrowthMetrics  GrowthMetrics   `json:"growth_metrics"`
}

// AnnualSummary for scenario forecasting
type AnnualSummary struct {
	TotalRevenue    float64 `json:"total_revenue"`
	TotalCosts      float64 `json:"total_costs"`
	TotalProfit     float64 `json:"total_profit"`
	AverageMargin   float64 `json:"average_margin"`
	EndingMRR       float64 `json:"ending_mrr"`      // Monthly recurring revenue at year end
	RevenueGrowth   float64 `json:"revenue_growth"`  // YoY growth %
}

// GrowthMetrics tracks growth KPIs
type GrowthMetrics struct {
	StartingMRR          float64 `json:"starting_mrr"`
	EndingMRR            float64 `json:"ending_mrr"`
	MRRGrowth            float64 `json:"mrr_growth"`            // Percentage
	MonthsToBreakEven    int     `json:"months_to_break_even"`  // If currently losing money
	ProfitableMonths     int     `json:"profitable_months"`
	ProjectedAnnualProfit float64 `json:"projected_annual_profit"`
}

// CostOverride allows users to override specific costs for scenario planning
type CostOverride struct {
	Category    string  `json:"category"`     // "vendor", "esp", "payroll", "revenue_share"
	Name        string  `json:"name"`         // Specific item name
	NewCost     float64 `json:"new_cost"`     // New monthly cost
	OriginalCost float64 `json:"original_cost"` // For reference
}

// ScenarioPlanningRequest is the request for scenario planning
type ScenarioPlanningRequest struct {
	BaseScenario    string         `json:"base_scenario"`     // "conservative", "moderate", "aggressive", "custom"
	CustomGrowthRate float64       `json:"custom_growth_rate"` // If custom scenario
	CostOverrides   []CostOverride `json:"cost_overrides"`
	StartMonth      string         `json:"start_month"`       // "2026-01"
	Months          int            `json:"months"`            // Number of months to forecast
}

// ScenarioPlanningResponse is the full response with multiple scenarios
type ScenarioPlanningResponse struct {
	Timestamp       time.Time          `json:"timestamp"`
	GrowthDrivers   GrowthDrivers      `json:"growth_drivers"`
	Scenarios       []ScenarioForecast `json:"scenarios"`
	CostBreakdown   *CostBreakdown     `json:"cost_breakdown"`
	Recommendations []string           `json:"recommendations"`
}

// DefaultGrowthScenarios returns the predefined growth scenarios
func DefaultGrowthScenarios() []GrowthScenario {
	return []GrowthScenario{
		{
			Name:          "Conservative",
			Description:   "Flat volume, no eCPM improvement - maintain current operations",
			MonthlyGrowth: 0.0,
			VolumeGrowth:  0.0,
			ECPMChange:    0.0,
			IsDefault:     false,
		},
		{
			Name:          "Moderate",
			Description:   "3% monthly volume growth with stable eCPM - organic expansion",
			MonthlyGrowth: 3.0,
			VolumeGrowth:  3.0,
			ECPMChange:    0.0,
			IsDefault:     true,
		},
		{
			Name:          "Aggressive",
			Description:   "5% volume growth + 1% eCPM improvement - active optimization",
			MonthlyGrowth: 6.0,
			VolumeGrowth:  5.0,
			ECPMChange:    1.0,
			IsDefault:     false,
		},
		{
			Name:          "Optimized",
			Description:   "3% volume growth + 2% eCPM improvement - focus on offer mix optimization",
			MonthlyGrowth: 5.0,
			VolumeGrowth:  3.0,
			ECPMChange:    2.0,
			IsDefault:     false,
		},
	}
}

// CalculateGrowthDrivers analyzes actual data to determine growth potential
func (s *RevenueModelService) CalculateGrowthDrivers() *GrowthDrivers {
	drivers := &GrowthDrivers{
		ESPVolumes:       make(map[string]int64),
		ESPRevenue:       make(map[string]float64),
		ESPeCPMs:         make(map[string]float64),
		HistoricalCosts:  []HistoricalCostPoint{},
	}

	// Get current month data
	currentMonth := s.GetCurrentMonthPL()
	drivers.CurrentMonthlyRevenue = currentMonth.GrossRevenue
	
	// Get ESP revenue breakdown
	for esp, revenue := range currentMonth.ESPRevenue {
		drivers.ESPRevenue[esp] = revenue
	}

	// Get ESP volumes from everflow collector
	if s.everflowCollector != nil {
		espRevenue := s.everflowCollector.GetESPRevenue()
		var totalSent int64
		for _, esp := range espRevenue {
			drivers.ESPVolumes[esp.ESPName] = esp.TotalSent
			totalSent += esp.TotalSent
			
			// Calculate eCPM per ESP
			if esp.TotalSent > 0 {
				drivers.ESPeCPMs[esp.ESPName] = (esp.Revenue / float64(esp.TotalSent)) * 1000
			}
		}
		drivers.CurrentMonthlySent = totalSent
	}

	// Calculate overall eCPM
	if drivers.CurrentMonthlySent > 0 {
		drivers.CurrentECPM = (drivers.CurrentMonthlyRevenue / float64(drivers.CurrentMonthlySent)) * 1000
	}

	// Calculate revenue per employee
	employeeCount := float64(len(s.config.EmployeeCosts))
	if employeeCount > 0 {
		drivers.RevenuePerEmployee = drivers.CurrentMonthlyRevenue / employeeCount
	}

	// Load historical costs and calculate trends
	if s.config.HistoricalCosts != nil && len(s.config.HistoricalCosts) > 0 {
		// Sort months chronologically
		months := make([]string, 0, len(s.config.HistoricalCosts))
		for month := range s.config.HistoricalCosts {
			months = append(months, month)
		}
		// Simple sort (works for YYYY-MM format)
		for i := 0; i < len(months)-1; i++ {
			for j := i + 1; j < len(months); j++ {
				if months[i] > months[j] {
					months[i], months[j] = months[j], months[i]
				}
			}
		}
		
		for _, month := range months {
			cost := s.config.HistoricalCosts[month]
			drivers.HistoricalCosts = append(drivers.HistoricalCosts, HistoricalCostPoint{
				Month: month,
				Cost:  cost,
			})
		}

		// Calculate YoY growth rate
		if len(drivers.HistoricalCosts) >= 2 {
			firstCost := drivers.HistoricalCosts[0].Cost
			lastCost := drivers.HistoricalCosts[len(drivers.HistoricalCosts)-1].Cost
			if firstCost > 0 {
				drivers.CostGrowthRate = ((lastCost - firstCost) / firstCost) * 100
			}
		}
	}

	// Set cost projection parameters from config
	if s.config.CostProjection.CurrentMax > 0 {
		drivers.CurrentOperatingCost = s.config.CostProjection.CurrentMax
	} else {
		// Calculate from current costs (excluding ESP)
		breakdown := s.GetCostBreakdown()
		drivers.CurrentOperatingCost = breakdown.TotalMonthlyCost
		for _, esp := range breakdown.ESPCosts {
			drivers.CurrentOperatingCost -= esp.MonthlyCost
		}
	}
	
	drivers.TargetOperatingCost = s.config.CostProjection.TargetCosts
	if drivers.TargetOperatingCost == 0 {
		drivers.TargetOperatingCost = 75000 // Default target
	}
	
	drivers.CostReductionTarget = s.config.CostProjection.TargetReduction
	if drivers.CostReductionTarget == 0 {
		drivers.CostReductionTarget = drivers.CurrentOperatingCost - drivers.TargetOperatingCost
	}

	// Estimate growth potentials based on the data
	drivers.VolumeGrowthPotential = 15.0 // Conservative estimate
	drivers.ECPMOptimizationPotential = 10.0 // 10% improvement possible
	drivers.ESPMixOptimization = 5.0 // 5% improvement by shifting volume

	return drivers
}

// GenerateScenarioForecast creates a forecast for a specific scenario
func (s *RevenueModelService) GenerateScenarioForecast(
	scenario GrowthScenario,
	startingRevenue float64,
	costOverrides []CostOverride,
	months int,
) *ScenarioForecast {
	forecast := &ScenarioForecast{
		Scenario: scenario,
		Months:   make([]MonthlyPL, months),
	}

	// Get base costs (with overrides applied)
	baseCosts := s.GetCostBreakdownWithOverrides(costOverrides)
	
	// Calculate ESP costs (these stay constant based on volume)
	espCosts := 0.0
	for _, item := range baseCosts.ESPCosts {
		espCosts += item.MonthlyCost
	}
	
	// Use the configured current max for operational costs
	// This represents the actual current spending level from historical data
	currentOpsCosts := s.config.CostProjection.CurrentMax
	if currentOpsCosts == 0 {
		// Fall back to breakdown calculation if not configured
		currentOpsCosts = baseCosts.TotalMonthlyCost - espCosts
	}
	
	// Get cost projection parameters from config
	targetOpsCosts := s.config.CostProjection.TargetCosts
	if targetOpsCosts == 0 {
		targetOpsCosts = 75000 // Default target
	}
	
	reductionTimeline := s.config.CostProjection.ReductionTimelineMonths
	if reductionTimeline == 0 {
		reductionTimeline = 3 // Default 3 months to achieve reduction
	}
	
	// Calculate monthly cost reduction step
	costReductionPerMonth := 0.0
	if currentOpsCosts > targetOpsCosts && reductionTimeline > 0 {
		costReductionPerMonth = (currentOpsCosts - targetOpsCosts) / float64(reductionTimeline)
	}
	
	currentRevenue := startingRevenue
	totalRevenue := 0.0
	totalCosts := 0.0
	totalProfit := 0.0
	profitableMonths := 0

	// Combined monthly growth rate
	combinedGrowthRate := 1 + (scenario.VolumeGrowth/100) + (scenario.ECPMChange/100)

	for i := 0; i < months; i++ {
		month := time.Date(2026, time.Month(i+1), 1, 0, 0, 0, 0, time.UTC)
		
		pl := MonthlyPL{
			Month:       month.Format("2006-01"),
			MonthName:   month.Format("January 2006"),
			ESPRevenue:  make(map[string]float64),
			IsActual:    i == 0,
			IsForecast:  i > 0,
		}

		// Apply growth to revenue (compound monthly)
		if i > 0 {
			currentRevenue *= combinedGrowthRate
		}

		// Calculate operational costs with reduction trajectory
		monthOpsCosts := currentOpsCosts
		if i < reductionTimeline {
			// Gradually reduce costs over the timeline
			monthOpsCosts = currentOpsCosts - (costReductionPerMonth * float64(i))
		} else {
			// After timeline, maintain target costs
			monthOpsCosts = targetOpsCosts
		}
		
		// Ensure we don't go below target
		if monthOpsCosts < targetOpsCosts {
			monthOpsCosts = targetOpsCosts
		}

		pl.GrossRevenue = currentRevenue
		pl.ESPCosts = espCosts
		pl.TotalCosts = espCosts + monthOpsCosts
		
		// Break down operational costs proportionally
		opsCostRatio := monthOpsCosts / currentOpsCosts
		if currentOpsCosts == 0 {
			opsCostRatio = 1
		}
		
		for _, item := range baseCosts.VendorCosts {
			pl.VendorCosts += item.MonthlyCost * opsCostRatio
		}
		for _, item := range baseCosts.PayrollCosts {
			pl.PayrollCosts += item.MonthlyCost * opsCostRatio
		}
		for _, item := range baseCosts.RevenueShareCosts {
			pl.RevenueShare += item.MonthlyCost * opsCostRatio
		}

		// Calculate P&L
		pl.GrossProfit = pl.GrossRevenue - pl.ESPCosts
		if pl.GrossRevenue > 0 {
			pl.GrossMargin = (pl.GrossProfit / pl.GrossRevenue) * 100
		}
		
		pl.NetProfit = pl.GrossRevenue - pl.TotalCosts
		if pl.GrossRevenue > 0 {
			pl.NetMargin = (pl.NetProfit / pl.GrossRevenue) * 100
		}

		forecast.Months[i] = pl

		// Accumulate totals
		totalRevenue += pl.GrossRevenue
		totalCosts += pl.TotalCosts
		totalProfit += pl.NetProfit
		
		if pl.NetProfit >= 0 {
			profitableMonths++
		}
	}

	// Calculate annual summary
	forecast.AnnualSummary = AnnualSummary{
		TotalRevenue:   totalRevenue,
		TotalCosts:     totalCosts,
		TotalProfit:    totalProfit,
		AverageMargin:  (totalProfit / totalRevenue) * 100,
		EndingMRR:      forecast.Months[months-1].GrossRevenue,
		RevenueGrowth:  ((forecast.Months[months-1].GrossRevenue / startingRevenue) - 1) * 100,
	}

	// Calculate growth metrics
	forecast.GrowthMetrics = GrowthMetrics{
		StartingMRR:           startingRevenue,
		EndingMRR:             forecast.Months[months-1].GrossRevenue,
		MRRGrowth:             ((forecast.Months[months-1].GrossRevenue / startingRevenue) - 1) * 100,
		ProfitableMonths:      profitableMonths,
		ProjectedAnnualProfit: totalProfit,
	}

	// Calculate months to break-even if not already profitable
	if forecast.Months[0].NetProfit < 0 {
		for i, month := range forecast.Months {
			if month.NetProfit >= 0 {
				forecast.GrowthMetrics.MonthsToBreakEven = i + 1
				break
			}
		}
	}

	return forecast
}

// GetCostBreakdownWithOverrides applies cost overrides to the base breakdown
func (s *RevenueModelService) GetCostBreakdownWithOverrides(overrides []CostOverride) *CostBreakdown {
	base := s.GetCostBreakdown()
	
	if len(overrides) == 0 {
		return base
	}

	// Create a map for quick override lookup
	overrideMap := make(map[string]float64)
	for _, o := range overrides {
		key := o.Category + ":" + o.Name
		overrideMap[key] = o.NewCost
	}

	// Apply overrides to each category
	newTotal := 0.0

	// ESP costs
	for i := range base.ESPCosts {
		key := "esp:" + base.ESPCosts[i].Name
		if newCost, ok := overrideMap[key]; ok {
			base.ESPCosts[i].MonthlyCost = newCost
		}
		newTotal += base.ESPCosts[i].MonthlyCost
	}

	// Vendor costs
	for i := range base.VendorCosts {
		key := "vendor:" + base.VendorCosts[i].Name
		if newCost, ok := overrideMap[key]; ok {
			base.VendorCosts[i].MonthlyCost = newCost
		}
		newTotal += base.VendorCosts[i].MonthlyCost
	}

	// Payroll costs
	for i := range base.PayrollCosts {
		key := "payroll:" + base.PayrollCosts[i].Name
		if newCost, ok := overrideMap[key]; ok {
			base.PayrollCosts[i].MonthlyCost = newCost
		}
		newTotal += base.PayrollCosts[i].MonthlyCost
	}

	// Revenue share costs
	for i := range base.RevenueShareCosts {
		key := "revenue_share:" + base.RevenueShareCosts[i].Name
		if newCost, ok := overrideMap[key]; ok {
			base.RevenueShareCosts[i].MonthlyCost = newCost
		}
		newTotal += base.RevenueShareCosts[i].MonthlyCost
	}

	base.TotalMonthlyCost = newTotal

	// Recalculate costs by category
	base.CostsByCategory = make(map[string]float64)
	for _, item := range base.ESPCosts {
		base.CostsByCategory["ESP"] += item.MonthlyCost
	}
	for _, item := range base.VendorCosts {
		base.CostsByCategory[item.Category] += item.MonthlyCost
	}
	for _, item := range base.PayrollCosts {
		base.CostsByCategory[item.Category] += item.MonthlyCost
	}
	for _, item := range base.RevenueShareCosts {
		base.CostsByCategory["Revenue Share"] += item.MonthlyCost
	}

	return base
}

// GetScenarioPlanningResponse generates a complete scenario planning response
func (s *RevenueModelService) GetScenarioPlanningResponse(req *ScenarioPlanningRequest) *ScenarioPlanningResponse {
	response := &ScenarioPlanningResponse{
		Timestamp:     time.Now(),
		GrowthDrivers: *s.CalculateGrowthDrivers(),
		Scenarios:     []ScenarioForecast{},
	}

	// Get current month revenue as baseline
	currentMonth := s.GetCurrentMonthPL()
	startingRevenue := currentMonth.GrossRevenue
	if startingRevenue == 0 {
		startingRevenue = 100000 // Default fallback
	}

	months := 12
	if req != nil && req.Months > 0 {
		months = req.Months
	}

	var overrides []CostOverride
	if req != nil {
		overrides = req.CostOverrides
	}

	// Generate forecasts for all scenarios
	for _, scenario := range DefaultGrowthScenarios() {
		forecast := s.GenerateScenarioForecast(scenario, startingRevenue, overrides, months)
		response.Scenarios = append(response.Scenarios, *forecast)
	}

	// If custom scenario requested, add it
	if req != nil && req.BaseScenario == "custom" {
		customScenario := GrowthScenario{
			Name:          "Custom",
			Description:   "User-defined growth scenario",
			MonthlyGrowth: req.CustomGrowthRate,
			VolumeGrowth:  req.CustomGrowthRate * 0.8, // Assume 80% from volume
			ECPMChange:    req.CustomGrowthRate * 0.2, // 20% from eCPM
			IsDefault:     false,
		}
		forecast := s.GenerateScenarioForecast(customScenario, startingRevenue, overrides, months)
		response.Scenarios = append(response.Scenarios, *forecast)
	}

	// Get cost breakdown with overrides
	response.CostBreakdown = s.GetCostBreakdownWithOverrides(overrides)

	// Generate recommendations based on data
	response.Recommendations = s.generateRecommendations(&response.GrowthDrivers)

	return response
}

// generateRecommendations provides data-driven recommendations
func (s *RevenueModelService) generateRecommendations(drivers *GrowthDrivers) []string {
	recommendations := []string{}

	// eCPM optimization
	if drivers.CurrentECPM < 0.50 {
		recommendations = append(recommendations, 
			"Current eCPM ($"+formatFloat(drivers.CurrentECPM, 2)+") is below $0.50. Focus on higher-paying CPM offers to improve revenue per email.")
	}

	// ESP mix optimization
	if sparkposteCPM, ok := drivers.ESPeCPMs["SparkPost"]; ok {
		if mailguneCPM, ok := drivers.ESPeCPMs["Mailgun"]; ok {
			if sparkposteCPM > mailguneCPM*1.1 {
				recommendations = append(recommendations,
					"SparkPost eCPM is higher than Mailgun. Consider shifting more volume to SparkPost for better revenue yield.")
			}
		}
	}

	// Volume growth
	if drivers.VolumeGrowthPotential > 10 {
		recommendations = append(recommendations,
			"ESP contracts have headroom for volume growth. Explore list expansion or increased send frequency.")
	}

	// Cost optimization
	currentMonth := s.GetCurrentMonthPL()
	if currentMonth.NetMargin < 20 {
		recommendations = append(recommendations,
			"Net margin ("+formatFloat(currentMonth.NetMargin, 1)+"%) is below 20%. Review vendor costs for optimization opportunities.")
	}

	// Break-even
	breakdown := s.GetCostBreakdown()
	if currentMonth.GrossRevenue < breakdown.TotalMonthlyCost*1.1 {
		recommendations = append(recommendations,
			"Revenue is close to break-even. Prioritize revenue growth initiatives or cost reduction.")
	}

	// Revenue concentration
	if len(drivers.ESPRevenue) > 0 {
		maxRevenue := 0.0
		maxESP := ""
		for esp, rev := range drivers.ESPRevenue {
			if rev > maxRevenue {
				maxRevenue = rev
				maxESP = esp
			}
		}
		concentration := (maxRevenue / currentMonth.GrossRevenue) * 100
		if concentration > 60 {
			recommendations = append(recommendations,
				maxESP+" accounts for "+formatFloat(concentration, 0)+"% of revenue. Consider diversifying to reduce concentration risk.")
		}
	}

	return recommendations
}

func formatFloat(f float64, decimals int) string {
	format := "%." + string(rune('0'+decimals)) + "f"
	return fmt.Sprintf(format, f)
}
