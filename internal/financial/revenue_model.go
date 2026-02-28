package financial

import (
	"fmt"
	"sort"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/everflow"
)

// RevenueModelService handles P&L calculations and forecasting
type RevenueModelService struct {
	config            *config.RevenueModelConfig
	espContracts      []config.ESPContract
	everflowCollector *everflow.Collector
}

// NewRevenueModelService creates a new revenue model service
func NewRevenueModelService(cfg *config.RevenueModelConfig, espContracts []config.ESPContract) *RevenueModelService {
	return &RevenueModelService{
		config:       cfg,
		espContracts: espContracts,
	}
}

// SetEverflowCollector sets the Everflow collector for revenue data
func (s *RevenueModelService) SetEverflowCollector(collector *everflow.Collector) {
	s.everflowCollector = collector
}

// ========== Types ==========

// CostCategory represents a category of costs
type CostCategory struct {
	Name        string      `json:"name"`
	MonthlyCost float64     `json:"monthly_cost"`
	Items       []CostItem  `json:"items"`
}

// CostItem represents a single cost item
type CostItem struct {
	Name        string  `json:"name"`
	Category    string  `json:"category"`
	MonthlyCost float64 `json:"monthly_cost"`
	Type        string  `json:"type"` // "vendor", "esp", "payroll", "revenue_share"
}

// MonthlyPL represents a single month's P&L
type MonthlyPL struct {
	Month           string  `json:"month"`           // Format: "2026-01"
	MonthName       string  `json:"month_name"`      // Format: "January 2026"
	
	// Revenue
	GrossRevenue    float64 `json:"gross_revenue"`
	ESPRevenue      map[string]float64 `json:"esp_revenue"` // Revenue by ESP
	
	// Costs
	TotalCosts      float64 `json:"total_costs"`
	ESPCosts        float64 `json:"esp_costs"`
	VendorCosts     float64 `json:"vendor_costs"`
	PayrollCosts    float64 `json:"payroll_costs"`
	RevenueShare    float64 `json:"revenue_share"`
	
	// P&L
	GrossProfit     float64 `json:"gross_profit"`
	GrossMargin     float64 `json:"gross_margin"`     // Percentage
	NetProfit       float64 `json:"net_profit"`
	NetMargin       float64 `json:"net_margin"`       // Percentage
	
	// Flags
	IsActual        bool    `json:"is_actual"`        // True if based on actual data
	IsForecast      bool    `json:"is_forecast"`      // True if projected
}

// AnnualForecast represents a full year forecast
type AnnualForecast struct {
	FiscalYear      string      `json:"fiscal_year"`
	Months          []MonthlyPL `json:"months"`
	
	// Annual Totals
	TotalRevenue    float64 `json:"total_revenue"`
	TotalCosts      float64 `json:"total_costs"`
	TotalESPCosts   float64 `json:"total_esp_costs"`
	TotalVendorCosts float64 `json:"total_vendor_costs"`
	TotalPayroll    float64 `json:"total_payroll"`
	TotalRevenueShare float64 `json:"total_revenue_share"`
	AnnualProfit    float64 `json:"annual_profit"`
	AnnualMargin    float64 `json:"annual_margin"`
	
	// Summary
	AvgMonthlyRevenue float64 `json:"avg_monthly_revenue"`
	AvgMonthlyProfit  float64 `json:"avg_monthly_profit"`
	BreakEvenRevenue  float64 `json:"break_even_revenue"` // Monthly revenue needed to break even
}

// CostBreakdown provides detailed cost breakdown
type CostBreakdown struct {
	TotalMonthlyCost   float64        `json:"total_monthly_cost"`
	ESPCosts           []CostItem     `json:"esp_costs"`
	VendorCosts        []CostItem     `json:"vendor_costs"`
	PayrollCosts       []CostItem     `json:"payroll_costs"`
	RevenueShareCosts  []CostItem     `json:"revenue_share_costs"`
	CostsByCategory    map[string]float64 `json:"costs_by_category"`
}

// FinancialDashboard is the main response for the financial dashboard
type FinancialDashboard struct {
	Timestamp         time.Time       `json:"timestamp"`
	CurrentMonth      MonthlyPL       `json:"current_month"`
	CostBreakdown     CostBreakdown   `json:"cost_breakdown"`
	AnnualForecast    AnnualForecast  `json:"annual_forecast"`
	KeyMetrics        KeyMetrics      `json:"key_metrics"`
}

// KeyMetrics contains key financial metrics
type KeyMetrics struct {
	MonthlyBurnRate     float64 `json:"monthly_burn_rate"`
	RunwayMonths        float64 `json:"runway_months"`       // Based on current profit
	RevenuePerEmployee  float64 `json:"revenue_per_employee"`
	CostPerEmail        float64 `json:"cost_per_email"`      // Total cost / emails sent
	RevenuePerEmail     float64 `json:"revenue_per_email"`
	ProfitPerEmail      float64 `json:"profit_per_email"`
}

// ========== Methods ==========

// GetCostBreakdown returns detailed cost breakdown
func (s *RevenueModelService) GetCostBreakdown() *CostBreakdown {
	breakdown := &CostBreakdown{
		ESPCosts:          []CostItem{},
		VendorCosts:       []CostItem{},
		PayrollCosts:      []CostItem{},
		RevenueShareCosts: []CostItem{},
		CostsByCategory:   make(map[string]float64),
	}

	// ESP Costs from contracts
	for _, contract := range s.espContracts {
		if contract.Enabled {
			item := CostItem{
				Name:        contract.ESPName,
				Category:    "ESP",
				MonthlyCost: contract.MonthlyFee,
				Type:        "esp",
			}
			breakdown.ESPCosts = append(breakdown.ESPCosts, item)
			breakdown.TotalMonthlyCost += contract.MonthlyFee
			breakdown.CostsByCategory["ESP"] += contract.MonthlyFee
		}
	}

	// Vendor Costs
	for _, vendor := range s.config.VendorCosts {
		item := CostItem{
			Name:        vendor.Name,
			Category:    vendor.Category,
			MonthlyCost: vendor.MonthlyCost,
			Type:        "vendor",
		}
		breakdown.VendorCosts = append(breakdown.VendorCosts, item)
		breakdown.TotalMonthlyCost += vendor.MonthlyCost
		breakdown.CostsByCategory[vendor.Category] += vendor.MonthlyCost
	}

	// Contractor Costs
	for _, contractor := range s.config.ContractorCosts {
		item := CostItem{
			Name:        contractor.Name,
			Category:    "Contractors",
			MonthlyCost: contractor.MonthlyCost,
			Type:        "payroll",
		}
		breakdown.PayrollCosts = append(breakdown.PayrollCosts, item)
		breakdown.TotalMonthlyCost += contractor.MonthlyCost
		breakdown.CostsByCategory["Contractors"] += contractor.MonthlyCost
	}

	// Employee Costs (convert yearly to monthly)
	for _, employee := range s.config.EmployeeCosts {
		monthlySalary := employee.YearlySalary / 12
		item := CostItem{
			Name:        fmt.Sprintf("%s (%s)", employee.Name, employee.Role),
			Category:    "Employees",
			MonthlyCost: monthlySalary,
			Type:        "payroll",
		}
		breakdown.PayrollCosts = append(breakdown.PayrollCosts, item)
		breakdown.TotalMonthlyCost += monthlySalary
		breakdown.CostsByCategory["Employees"] += monthlySalary
	}

	// Revenue Share
	for _, share := range s.config.RevenueShare {
		item := CostItem{
			Name:        share.Name,
			Category:    "Revenue Share",
			MonthlyCost: share.MonthlyAmount,
			Type:        "revenue_share",
		}
		breakdown.RevenueShareCosts = append(breakdown.RevenueShareCosts, item)
		breakdown.TotalMonthlyCost += share.MonthlyAmount
		breakdown.CostsByCategory["Revenue Share"] += share.MonthlyAmount
	}

	return breakdown
}

// GetCurrentMonthPL returns P&L for the current month using actual Everflow data
func (s *RevenueModelService) GetCurrentMonthPL() *MonthlyPL {
	now := time.Now()
	month := now.Format("2006-01")
	monthName := now.Format("January 2006")

	pl := &MonthlyPL{
		Month:       month,
		MonthName:   monthName,
		ESPRevenue:  make(map[string]float64),
		IsActual:    true,
		IsForecast:  false,
	}

	// Get costs
	breakdown := s.GetCostBreakdown()
	pl.TotalCosts = breakdown.TotalMonthlyCost
	
	// Calculate cost components
	for _, item := range breakdown.ESPCosts {
		pl.ESPCosts += item.MonthlyCost
	}
	for _, item := range breakdown.VendorCosts {
		pl.VendorCosts += item.MonthlyCost
	}
	for _, item := range breakdown.PayrollCosts {
		pl.PayrollCosts += item.MonthlyCost
	}
	for _, item := range breakdown.RevenueShareCosts {
		pl.RevenueShare += item.MonthlyCost
	}

	// Get TOTAL revenue from Everflow (not just conversion-tracked ESP revenue)
	if s.everflowCollector != nil {
		// Get TRUE total Everflow revenue from daily performance (includes ALL revenue types)
		trueTotal := s.everflowCollector.GetTotalRevenue()
		
		// Get ESP revenue breakdown
		espRevenue := s.everflowCollector.GetESPRevenue()
		
		// Calculate sum of ESP revenue
		espSum := 0.0
		for _, esp := range espRevenue {
			espSum += esp.Revenue
		}
		
		// Determine which total to use and how to distribute
		// The TRUE total from daily performance is the authoritative source
		if trueTotal > 0 && espSum > 0 {
			// Scale ESP revenues proportionally to match the TRUE total
			// This ensures the sum of ESP revenues exactly equals gross revenue
			scaleFactor := trueTotal / espSum
			
			for _, esp := range espRevenue {
				scaledRevenue := esp.Revenue * scaleFactor
				pl.ESPRevenue[esp.ESPName] = scaledRevenue
			}
			pl.GrossRevenue = trueTotal
		} else if trueTotal > 0 {
			// No ESP breakdown available, use total as "Unattributed"
			pl.GrossRevenue = trueTotal
			pl.ESPRevenue["Unattributed"] = trueTotal
		} else if espSum > 0 {
			// No daily performance but have ESP data (unusual case)
			for _, esp := range espRevenue {
				pl.ESPRevenue[esp.ESPName] = esp.Revenue
			}
			pl.GrossRevenue = espSum
		}
		
		// Validate: Sum of ESP revenues should equal GrossRevenue
		var finalSum float64
		for _, rev := range pl.ESPRevenue {
			finalSum += rev
		}
		
		// If there's a small difference due to floating point, adjust the largest ESP
		diff := pl.GrossRevenue - finalSum
		if diff > 0.01 || diff < -0.01 { // More than 1 cent difference
			// Find largest ESP and adjust
			var largestESP string
			var largestRev float64
			for esp, rev := range pl.ESPRevenue {
				if rev > largestRev {
					largestRev = rev
					largestESP = esp
				}
			}
			if largestESP != "" {
				pl.ESPRevenue[largestESP] += diff
			}
		}
	}

	// Calculate P&L metrics
	pl.GrossProfit = pl.GrossRevenue - pl.ESPCosts
	if pl.GrossRevenue > 0 {
		pl.GrossMargin = (pl.GrossProfit / pl.GrossRevenue) * 100
	}
	
	pl.NetProfit = pl.GrossRevenue - pl.TotalCosts
	if pl.GrossRevenue > 0 {
		pl.NetMargin = (pl.NetProfit / pl.GrossRevenue) * 100
	}

	return pl
}

// GetAnnualForecast generates a 12-month forecast based on January actuals
func (s *RevenueModelService) GetAnnualForecast(januaryActuals *MonthlyPL) *AnnualForecast {
	forecast := &AnnualForecast{
		FiscalYear: "2026",
		Months:     make([]MonthlyPL, 12),
	}

	// Get cost breakdown for projections
	breakdown := s.GetCostBreakdown()
	
	// Calculate ESP costs (fixed based on contracts)
	espCosts := 0.0
	for _, item := range breakdown.ESPCosts {
		espCosts += item.MonthlyCost
	}
	
	// Get cost projection parameters from config
	currentOpsCosts := s.config.CostProjection.CurrentMax
	if currentOpsCosts == 0 {
		currentOpsCosts = breakdown.TotalMonthlyCost - espCosts
	}
	
	targetOpsCosts := s.config.CostProjection.TargetCosts
	if targetOpsCosts == 0 {
		targetOpsCosts = 75000
	}
	
	reductionTimeline := s.config.CostProjection.ReductionTimelineMonths
	if reductionTimeline == 0 {
		reductionTimeline = 3
	}
	
	// Calculate monthly cost reduction
	costReductionPerMonth := 0.0
	if currentOpsCosts > targetOpsCosts && reductionTimeline > 0 {
		costReductionPerMonth = (currentOpsCosts - targetOpsCosts) / float64(reductionTimeline)
	}
	
	// Use January actuals as the baseline for revenue projection
	baselineRevenue := januaryActuals.GrossRevenue
	if baselineRevenue == 0 {
		baselineRevenue = 100000 // Default if no actual data
	}

	// Generate 12 months
	for i := 0; i < 12; i++ {
		month := time.Date(2026, time.Month(i+1), 1, 0, 0, 0, 0, time.UTC)
		monthStr := month.Format("2006-01")
		monthName := month.Format("January 2006")

		pl := MonthlyPL{
			Month:       monthStr,
			MonthName:   monthName,
			ESPRevenue:  make(map[string]float64),
			IsActual:    i == 0, // Only January is actual
			IsForecast:  i > 0,
		}

		// Calculate operational costs with reduction trajectory
		monthOpsCosts := currentOpsCosts
		if i < reductionTimeline {
			monthOpsCosts = currentOpsCosts - (costReductionPerMonth * float64(i))
		} else {
			monthOpsCosts = targetOpsCosts
		}
		if monthOpsCosts < targetOpsCosts {
			monthOpsCosts = targetOpsCosts
		}

		// Use January actuals for January, baseline for others
		if i == 0 && januaryActuals != nil {
			pl = *januaryActuals
			// Override costs with the projection (January still at current level)
			pl.TotalCosts = espCosts + currentOpsCosts
		} else {
			// Project revenue (using baseline - flat for now)
			pl.GrossRevenue = baselineRevenue

			// Distribute revenue by ESP based on January proportions
			if januaryActuals != nil {
				for esp, rev := range januaryActuals.ESPRevenue {
					proportion := 0.0
					if januaryActuals.GrossRevenue > 0 {
						proportion = rev / januaryActuals.GrossRevenue
					}
					pl.ESPRevenue[esp] = pl.GrossRevenue * proportion
				}
			}

			// Apply cost reduction trajectory
			pl.ESPCosts = espCosts
			pl.TotalCosts = espCosts + monthOpsCosts
			
			// Break down operational costs proportionally
			opsCostRatio := monthOpsCosts / currentOpsCosts
			if currentOpsCosts == 0 {
				opsCostRatio = 1
			}
			
			for _, item := range breakdown.VendorCosts {
				pl.VendorCosts += item.MonthlyCost * opsCostRatio
			}
			for _, item := range breakdown.PayrollCosts {
				pl.PayrollCosts += item.MonthlyCost * opsCostRatio
			}
			for _, item := range breakdown.RevenueShareCosts {
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
		}

		forecast.Months[i] = pl

		// Accumulate annual totals
		forecast.TotalRevenue += pl.GrossRevenue
		forecast.TotalCosts += pl.TotalCosts
		forecast.TotalESPCosts += pl.ESPCosts
		forecast.TotalVendorCosts += pl.VendorCosts
		forecast.TotalPayroll += pl.PayrollCosts
		forecast.TotalRevenueShare += pl.RevenueShare
		forecast.AnnualProfit += pl.NetProfit
	}

	// Calculate annual metrics
	if forecast.TotalRevenue > 0 {
		forecast.AnnualMargin = (forecast.AnnualProfit / forecast.TotalRevenue) * 100
	}
	forecast.AvgMonthlyRevenue = forecast.TotalRevenue / 12
	forecast.AvgMonthlyProfit = forecast.AnnualProfit / 12
	
	// Break-even revenue = total monthly costs
	forecast.BreakEvenRevenue = breakdown.TotalMonthlyCost

	return forecast
}

// GetKeyMetrics calculates key financial metrics
func (s *RevenueModelService) GetKeyMetrics(currentMonth *MonthlyPL) *KeyMetrics {
	metrics := &KeyMetrics{}

	// Monthly burn rate (total costs)
	breakdown := s.GetCostBreakdown()
	metrics.MonthlyBurnRate = breakdown.TotalMonthlyCost

	// Runway based on current profit rate
	if currentMonth.NetProfit > 0 {
		metrics.RunwayMonths = -1 // Infinite if profitable
	} else if currentMonth.NetProfit < 0 {
		// Assuming some cash reserves (placeholder)
		cashReserves := 100000.0 // This should come from actual data
		metrics.RunwayMonths = cashReserves / (-currentMonth.NetProfit)
	}

	// Revenue per employee
	employeeCount := float64(len(s.config.EmployeeCosts))
	if employeeCount > 0 {
		metrics.RevenuePerEmployee = currentMonth.GrossRevenue / employeeCount
	}

	// Per-email metrics (need email volume from Everflow)
	if s.everflowCollector != nil {
		espRevenue := s.everflowCollector.GetESPRevenue()
		var totalSent int64
		for _, esp := range espRevenue {
			totalSent += esp.TotalSent
		}
		if totalSent > 0 {
			metrics.CostPerEmail = breakdown.TotalMonthlyCost / float64(totalSent)
			metrics.RevenuePerEmail = currentMonth.GrossRevenue / float64(totalSent)
			metrics.ProfitPerEmail = currentMonth.NetProfit / float64(totalSent)
		}
	}

	return metrics
}

// GetFinancialDashboard returns the complete financial dashboard data
func (s *RevenueModelService) GetFinancialDashboard() *FinancialDashboard {
	currentMonth := s.GetCurrentMonthPL()
	costBreakdown := s.GetCostBreakdown()
	annualForecast := s.GetAnnualForecast(currentMonth)
	keyMetrics := s.GetKeyMetrics(currentMonth)

	return &FinancialDashboard{
		Timestamp:      time.Now(),
		CurrentMonth:   *currentMonth,
		CostBreakdown:  *costBreakdown,
		AnnualForecast: *annualForecast,
		KeyMetrics:     *keyMetrics,
	}
}

// GetCostsByCategory returns costs grouped by category for charts
func (s *RevenueModelService) GetCostsByCategory() []CostCategory {
	breakdown := s.GetCostBreakdown()
	
	// Group by category
	categoryMap := make(map[string]*CostCategory)
	
	// Add all items
	allItems := append(append(append(
		breakdown.ESPCosts,
		breakdown.VendorCosts...),
		breakdown.PayrollCosts...),
		breakdown.RevenueShareCosts...)
	
	for _, item := range allItems {
		cat := item.Category
		if _, ok := categoryMap[cat]; !ok {
			categoryMap[cat] = &CostCategory{
				Name:  cat,
				Items: []CostItem{},
			}
		}
		categoryMap[cat].Items = append(categoryMap[cat].Items, item)
		categoryMap[cat].MonthlyCost += item.MonthlyCost
	}

	// Convert to slice and sort by cost
	result := make([]CostCategory, 0, len(categoryMap))
	for _, cat := range categoryMap {
		result = append(result, *cat)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].MonthlyCost > result[j].MonthlyCost
	})

	return result
}
