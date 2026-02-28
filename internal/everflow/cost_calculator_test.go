package everflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewCostCalculator(t *testing.T) {
	contracts := []ESPContractInfo{
		{
			ESPName:            "SparkPost",
			MonthlyIncluded:    200000000, // 200M emails
			MonthlyFee:         21120.00,
			OverageRatePer1000: 0.13,
		},
	}

	cc := NewCostCalculator(contracts)
	assert.NotNil(t, cc)
	assert.True(t, cc.HasContract("SparkPost"))
	assert.True(t, cc.HasContract("sparkpost")) // Should normalize
	assert.False(t, cc.HasContract("Mailgun"))
}

func TestCostCalculator_CalculateCosts_WithinIncluded(t *testing.T) {
	contracts := []ESPContractInfo{
		{
			ESPName:            "SparkPost",
			MonthlyIncluded:    200000000,
			MonthlyFee:         21120.00,
			OverageRatePer1000: 0.13,
		},
	}

	cc := NewCostCalculator(contracts)

	// 30 day period, 100M emails (within the 200M included)
	metrics := cc.CalculateCosts("SparkPost", 30, 100000000, 50000.00)

	assert.NotNil(t, metrics)
	assert.Equal(t, int64(200000000), metrics.MonthlyIncluded)
	assert.Equal(t, 21120.00, metrics.MonthlyFee)
	assert.Equal(t, int64(100000000), metrics.EmailsSent)
	assert.Equal(t, int64(0), metrics.EmailsOverIncluded) // Within limit
	assert.InDelta(t, 21120.00, metrics.ProRatedBaseCost, 0.01)
	assert.InDelta(t, 0.0, metrics.OverageCost, 0.01)
	assert.InDelta(t, 21120.00, metrics.TotalCost, 0.01)

	// eCPM calculations
	// Cost eCPM = (21120 / 100000000) * 1000 = 0.2112
	assert.InDelta(t, 0.2112, metrics.CostECPM, 0.001)
	// Revenue eCPM = (50000 / 100000000) * 1000 = 0.50
	assert.InDelta(t, 0.50, metrics.RevenueECPM, 0.01)

	// Profitability
	assert.InDelta(t, 28880.00, metrics.GrossProfit, 0.01)      // 50000 - 21120
	assert.InDelta(t, 57.76, metrics.GrossMargin, 0.1)          // (28880 / 50000) * 100
	assert.InDelta(t, 136.74, metrics.ROI, 1.0)                 // (28880 / 21120) * 100
}

func TestCostCalculator_CalculateCosts_WithOverage(t *testing.T) {
	contracts := []ESPContractInfo{
		{
			ESPName:            "SparkPost",
			MonthlyIncluded:    200000000,
			MonthlyFee:         21120.00,
			OverageRatePer1000: 0.13,
		},
	}

	cc := NewCostCalculator(contracts)

	// 30 day period, 250M emails (50M over the 200M included)
	metrics := cc.CalculateCosts("SparkPost", 30, 250000000, 75000.00)

	assert.NotNil(t, metrics)
	assert.Equal(t, int64(50000000), metrics.EmailsOverIncluded) // 50M over
	
	// Overage cost = (50000000 / 1000) * 0.13 = 6500
	assert.InDelta(t, 6500.00, metrics.OverageCost, 0.01)
	assert.InDelta(t, 27620.00, metrics.TotalCost, 0.01) // 21120 + 6500

	// Profitability with overage
	assert.InDelta(t, 47380.00, metrics.GrossProfit, 0.01)
	assert.InDelta(t, 63.17, metrics.GrossMargin, 0.1)
}

func TestCostCalculator_CalculateCosts_ProRatedPeriod(t *testing.T) {
	contracts := []ESPContractInfo{
		{
			ESPName:            "SparkPost",
			MonthlyIncluded:    200000000,
			MonthlyFee:         21120.00,
			OverageRatePer1000: 0.13,
		},
	}

	cc := NewCostCalculator(contracts)

	// 7 day period (7/30 of a month)
	periodFraction := 7.0 / 30.0
	proRatedIncluded := int64(float64(200000000) * periodFraction) // ~46.67M

	// Send 50M emails (slightly over the pro-rated 46.67M)
	metrics := cc.CalculateCosts("SparkPost", 7, 50000000, 15000.00)

	assert.NotNil(t, metrics)
	
	// Pro-rated base cost = 21120 * (7/30) = ~4928
	expectedBaseCost := 21120.00 * periodFraction
	assert.InDelta(t, expectedBaseCost, metrics.ProRatedBaseCost, 1.0)
	
	// Emails over = 50M - 46.67M = ~3.33M
	expectedOverage := int64(50000000) - proRatedIncluded
	assert.InDelta(t, float64(expectedOverage), float64(metrics.EmailsOverIncluded), 1000)
}

func TestCostCalculator_NormalizeESPName(t *testing.T) {
	contracts := []ESPContractInfo{
		{ESPName: "SparkPost", MonthlyIncluded: 100000, MonthlyFee: 100.00, OverageRatePer1000: 0.10},
	}

	cc := NewCostCalculator(contracts)

	// All variations should find the same contract
	assert.True(t, cc.HasContract("SparkPost"))
	assert.True(t, cc.HasContract("sparkpost"))
	assert.True(t, cc.HasContract("Sparkpost"))
	assert.True(t, cc.HasContract("SPARKPOST"))
	// Enterprise and Momentum variations should also match
	assert.True(t, cc.HasContract("SparkPost Enterprise"))
	assert.True(t, cc.HasContract("SparkPost Momentum"))
	assert.True(t, cc.HasContract("sparkpost enterprise"))
}

func TestCostCalculator_NoContract(t *testing.T) {
	contracts := []ESPContractInfo{
		{ESPName: "SparkPost", MonthlyIncluded: 100000, MonthlyFee: 100.00, OverageRatePer1000: 0.10},
	}

	cc := NewCostCalculator(contracts)

	// Should return nil for unknown ESP
	metrics := cc.CalculateCosts("Mailgun", 30, 1000000, 5000.00)
	assert.Nil(t, metrics)
}

func TestCostCalculator_GetAllContracts(t *testing.T) {
	contracts := []ESPContractInfo{
		{ESPName: "SparkPost", MonthlyIncluded: 200000000, MonthlyFee: 21120.00, OverageRatePer1000: 0.13},
		{ESPName: "Mailgun", MonthlyIncluded: 50000000, MonthlyFee: 5000.00, OverageRatePer1000: 0.20},
	}

	cc := NewCostCalculator(contracts)
	allContracts := cc.GetAllContracts()

	assert.Len(t, allContracts, 2)
}
