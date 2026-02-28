package everflow

import (
	"fmt"
	"sort"
	"time"
)

// GetLatestMetrics returns the latest collected metrics
func (c *Collector) GetLatestMetrics() *CollectorMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.metrics
}

// GetDailyPerformance returns daily performance metrics
func (c *Collector) GetDailyPerformance() []DailyPerformance {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.metrics == nil {
		return nil
	}
	return c.metrics.DailyPerformance
}

// GetOfferPerformance returns offer performance metrics
func (c *Collector) GetOfferPerformance() []OfferPerformance {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.metrics == nil {
		return nil
	}
	return c.metrics.OfferPerformance
}

// GetPropertyPerformance returns property performance metrics
func (c *Collector) GetPropertyPerformance() []PropertyPerformance {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.metrics == nil {
		return nil
	}
	return c.metrics.PropertyPerformance
}

// GetCampaignRevenue returns campaign revenue metrics
func (c *Collector) GetCampaignRevenue() []CampaignRevenue {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.metrics == nil {
		return nil
	}
	return c.metrics.CampaignRevenue
}

// GetRecentClicks returns recent clicks
func (c *Collector) GetRecentClicks() []Click {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.metrics == nil {
		return nil
	}
	return c.metrics.RecentClicks
}

// GetRecentConversions returns recent conversions
func (c *Collector) GetRecentConversions() []Conversion {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.metrics == nil {
		return nil
	}
	return c.metrics.RecentConversions
}

// LastFetch returns the time of the last successful fetch
func (c *Collector) LastFetch() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.metrics == nil {
		return time.Time{}
	}
	return c.metrics.LastFetch
}

// GetRevenueBreakdown returns the CPM vs Non-CPM revenue breakdown
func (c *Collector) GetRevenueBreakdown() *RevenueBreakdown {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.metrics == nil {
		return nil
	}
	return c.metrics.RevenueBreakdown
}

// GetESPRevenue returns revenue breakdown by ESP
func (c *Collector) GetESPRevenue() []ESPRevenuePerformance {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.metrics == nil {
		return nil
	}
	return c.metrics.ESPRevenue
}

// GetWeeklyPerformance aggregates data by week
func (c *Collector) GetWeeklyPerformance() []PeriodPerformance {
	c.mu.RLock()
	defer c.mu.RUnlock()

	weekMap := make(map[string]*PeriodPerformance)

	for _, d := range c.metrics.DailyPerformance {
		date, _ := time.Parse("2006-01-02", d.Date)
		year, week := date.ISOWeek()
		weekKey := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, (week-1)*7).Format("2006-W") + fmt.Sprintf("%02d", week)

		if _, ok := weekMap[weekKey]; !ok {
			weekMap[weekKey] = &PeriodPerformance{
				Period:     weekKey,
				PeriodType: "weekly",
			}
		}
		weekMap[weekKey].TotalClicks += d.Clicks
		weekMap[weekKey].TotalConversions += d.Conversions
		weekMap[weekKey].TotalRevenue += d.Revenue
		weekMap[weekKey].TotalPayout += d.Payout
	}

	weeks := make([]PeriodPerformance, 0, len(weekMap))
	for _, w := range weekMap {
		if w.TotalClicks > 0 {
			w.ConversionRate = float64(w.TotalConversions) / float64(w.TotalClicks)
			w.EPC = w.TotalRevenue / float64(w.TotalClicks)
		}
		weeks = append(weeks, *w)
	}

	sort.Slice(weeks, func(i, j int) bool {
		return weeks[i].Period > weeks[j].Period
	})

	return weeks
}

// GetMonthlyPerformance aggregates data by month
func (c *Collector) GetMonthlyPerformance() []PeriodPerformance {
	c.mu.RLock()
	defer c.mu.RUnlock()

	monthMap := make(map[string]*PeriodPerformance)

	for _, d := range c.metrics.DailyPerformance {
		date, _ := time.Parse("2006-01-02", d.Date)
		monthKey := date.Format("2006-01")

		if _, ok := monthMap[monthKey]; !ok {
			monthMap[monthKey] = &PeriodPerformance{
				Period:     monthKey,
				PeriodType: "monthly",
			}
		}
		monthMap[monthKey].TotalClicks += d.Clicks
		monthMap[monthKey].TotalConversions += d.Conversions
		monthMap[monthKey].TotalRevenue += d.Revenue
		monthMap[monthKey].TotalPayout += d.Payout
	}

	months := make([]PeriodPerformance, 0, len(monthMap))
	for _, m := range monthMap {
		if m.TotalClicks > 0 {
			m.ConversionRate = float64(m.TotalConversions) / float64(m.TotalClicks)
			m.EPC = m.TotalRevenue / float64(m.TotalClicks)
		}
		months = append(months, *m)
	}

	sort.Slice(months, func(i, j int) bool {
		return months[i].Period > months[j].Period
	})

	return months
}

// GetCampaignRevenueByID returns revenue for a specific campaign/mailing ID
func (c *Collector) GetCampaignRevenueByID(mailingID string) *CampaignRevenue {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, cr := range c.metrics.CampaignRevenue {
		if cr.MailingID == mailingID {
			return &cr
		}
	}
	return nil
}

// GetPropertyRevenueByCode returns revenue for a specific property
func (c *Collector) GetPropertyRevenueByCode(propertyCode string) *PropertyPerformance {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, p := range c.metrics.PropertyPerformance {
		if p.PropertyCode == propertyCode {
			return &p
		}
	}
	return nil
}

// GetTotalRevenue returns total revenue for the lookback period
func (c *Collector) GetTotalRevenue() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var total float64
	for _, d := range c.metrics.DailyPerformance {
		total += d.Revenue
	}
	return total
}

// GetDailyPerformanceByDateRange returns daily performance filtered by date range
func (c *Collector) GetDailyPerformanceByDateRange(startDate, endDate time.Time) []DailyPerformance {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.metrics == nil {
		return nil
	}

	startStr := startDate.Format("2006-01-02")
	endStr := endDate.Format("2006-01-02")

	var filtered []DailyPerformance
	for _, d := range c.metrics.DailyPerformance {
		if d.Date >= startStr && d.Date <= endStr {
			filtered = append(filtered, d)
		}
	}
	return filtered
}

// GetTotalRevenueByDateRange returns total revenue for a specific date range
func (c *Collector) GetTotalRevenueByDateRange(startDate, endDate time.Time) float64 {
	daily := c.GetDailyPerformanceByDateRange(startDate, endDate)
	var total float64
	for _, d := range daily {
		total += d.Revenue
	}
	return total
}

// GetMetricsByDateRange returns aggregated metrics for a date range
func (c *Collector) GetMetricsByDateRange(startDate, endDate time.Time) map[string]interface{} {
	daily := c.GetDailyPerformanceByDateRange(startDate, endDate)

	var totalClicks, totalConversions int64
	var totalRevenue, totalPayout float64

	for _, d := range daily {
		totalClicks += d.Clicks
		totalConversions += d.Conversions
		totalRevenue += d.Revenue
		totalPayout += d.Payout
	}

	convRate := float64(0)
	if totalClicks > 0 {
		convRate = float64(totalConversions) / float64(totalClicks) * 100
	}

	epc := float64(0)
	if totalClicks > 0 {
		epc = totalRevenue / float64(totalClicks)
	}

	return map[string]interface{}{
		"clicks":          totalClicks,
		"conversions":     totalConversions,
		"revenue":         totalRevenue,
		"payout":          totalPayout,
		"conversion_rate": convRate,
		"epc":             epc,
		"days":            len(daily),
		"daily":           daily,
	}
}

// GetESPRevenueByDateRange returns ESP revenue breakdown for a date range
// Note: Currently returns cached ESP revenue as conversion date filtering requires more complex implementation
func (c *Collector) GetESPRevenueByDateRange(startDate, endDate time.Time) []ESPRevenuePerformance {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.metrics == nil || c.metrics.ESPRevenue == nil {
		return nil
	}

	// TODO: Implement proper date filtering for ESP revenue
	// This requires storing conversion dates and filtering by them
	// For now, return the cached ESP revenue
	_ = startDate
	_ = endDate

	return c.metrics.ESPRevenue
}
