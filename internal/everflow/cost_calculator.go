package everflow

// CostCalculator handles ESP cost calculations
type CostCalculator struct {
	contracts map[string]*ESPContractInfo // key: ESP name (lowercase)
}

// NewCostCalculator creates a new cost calculator with the given contracts
func NewCostCalculator(contracts []ESPContractInfo) *CostCalculator {
	cc := &CostCalculator{
		contracts: make(map[string]*ESPContractInfo),
	}
	for i := range contracts {
		cc.contracts[normalizeESPName(contracts[i].ESPName)] = &contracts[i]
	}
	return cc
}

// normalizeESPName normalizes ESP names for matching
func normalizeESPName(name string) string {
	// Normalize common variations
	switch name {
	case "SparkPost", "sparkpost", "Sparkpost", "SPARKPOST",
		"SparkPost Enterprise", "SparkPost Momentum", "sparkpost enterprise", "sparkpost momentum":
		return "sparkpost"
	case "Mailgun", "mailgun", "MAILGUN":
		return "mailgun"
	case "SES", "ses", "AWS SES", "aws_ses", "Amazon SES", "amazon ses":
		return "ses"
	default:
		return name
	}
}

// GetContract returns the contract for an ESP, or nil if not found
func (cc *CostCalculator) GetContract(espName string) *ESPContractInfo {
	return cc.contracts[normalizeESPName(espName)]
}

// CalculateCosts calculates cost metrics for an ESP given the period data
// periodDays: number of days in the reporting period (for pro-rating monthly fees)
// emailsSent: total emails sent in the period
// revenue: total revenue generated in the period
func (cc *CostCalculator) CalculateCosts(espName string, periodDays int, emailsSent int64, revenue float64) *ESPCostMetrics {
	contract := cc.GetContract(espName)
	if contract == nil {
		return nil
	}

	metrics := &ESPCostMetrics{
		MonthlyIncluded:    contract.MonthlyIncluded,
		MonthlyFee:         contract.MonthlyFee,
		OverageRatePer1000: contract.OverageRatePer1000,
		EmailsSent:         emailsSent,
	}

	// Pro-rate the base cost for the period (assuming 30 days per month)
	periodFraction := float64(periodDays) / 30.0
	metrics.ProRatedBaseCost = contract.MonthlyFee * periodFraction

	// Calculate pro-rated included emails for the period
	proRatedIncluded := int64(float64(contract.MonthlyIncluded) * periodFraction)

	// Calculate overage
	if emailsSent > proRatedIncluded {
		metrics.EmailsOverIncluded = emailsSent - proRatedIncluded
		// Overage cost = (emails over / 1000) * overage rate per 1000
		metrics.OverageCost = (float64(metrics.EmailsOverIncluded) / 1000.0) * contract.OverageRatePer1000
	}

	// Total cost
	metrics.TotalCost = metrics.ProRatedBaseCost + metrics.OverageCost

	// eCPM calculations (per 1000 emails)
	if emailsSent > 0 {
		metrics.CostECPM = (metrics.TotalCost / float64(emailsSent)) * 1000
		metrics.RevenueECPM = (revenue / float64(emailsSent)) * 1000
		metrics.NetRevenuePerEmail = (revenue - metrics.TotalCost) / float64(emailsSent)
	}

	// Profitability
	metrics.GrossProfit = revenue - metrics.TotalCost
	if revenue > 0 {
		metrics.GrossMargin = (metrics.GrossProfit / revenue) * 100
	}
	if metrics.TotalCost > 0 {
		metrics.ROI = ((revenue - metrics.TotalCost) / metrics.TotalCost) * 100
	}

	return metrics
}

// CalculateMonthlyCosts calculates cost metrics assuming a full month period
func (cc *CostCalculator) CalculateMonthlyCosts(espName string, emailsSent int64, revenue float64) *ESPCostMetrics {
	return cc.CalculateCosts(espName, 30, emailsSent, revenue)
}

// GetAllContracts returns all configured ESP contracts
func (cc *CostCalculator) GetAllContracts() []ESPContractInfo {
	contracts := make([]ESPContractInfo, 0, len(cc.contracts))
	for _, c := range cc.contracts {
		contracts = append(contracts, *c)
	}
	return contracts
}

// HasContract checks if a contract exists for the given ESP
func (cc *CostCalculator) HasContract(espName string) bool {
	return cc.GetContract(espName) != nil
}
