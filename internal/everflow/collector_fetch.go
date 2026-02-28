package everflow

import (
	"context"
	"log"
	"time"
)

// collectLoop runs the periodic collection
func (c *Collector) collectLoop() {
	// Initial fetch
	c.fetchAll()

	ticker := time.NewTicker(c.fetchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.fetchToday() // Only fetch today's data on periodic updates
		case <-c.stopChan:
			log.Println("Everflow collector stopped")
			return
		}
	}
}

// fetchAll collects all data for the lookback period
func (c *Collector) fetchAll() {
	log.Println("Fetching Everflow metrics for lookback period...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	now := time.Now()
	startDate := now.AddDate(0, 0, -c.lookbackDays)

	// First, fetch entity report for daily aggregated stats (includes clicks)
	entityReportByDate, err := c.client.GetEntityReportByDate(ctx, startDate, now, nil)
	if err != nil {
		log.Printf("Everflow: Error fetching daily entity report: %v", err)
	} else {
		log.Printf("Everflow: Got daily entity report with %d entries, summary: %d clicks, %d conv, $%.2f rev",
			len(entityReportByDate.Table), entityReportByDate.Summary.TotalClick, entityReportByDate.Summary.Conversions, entityReportByDate.Summary.Revenue)
	}

	// Fetch entity report by offer for click counts per offer
	entityReportByOffer, err := c.client.GetEntityReportByOffer(ctx, startDate, now, nil)
	if err != nil {
		log.Printf("Everflow: Error fetching offer entity report: %v", err)
	} else {
		log.Printf("Everflow: Got offer entity report with %d offers",
			len(entityReportByOffer.Table))
	}

	// Space out BigQuery entity report requests to avoid 429 "Big Query usage
	// is above limit" errors. Each request consumes Everflow's BQ quota.
	// Wait 10s between requests, retry with 60s backoff if rate-limited.
	time.Sleep(10 * time.Second)

	// Fetch entity report by sub1 for campaign-level click counts
	entityReportBySub1, err := c.client.GetEntityReportBySub1(ctx, startDate, now, nil)
	if err != nil {
		log.Printf("Everflow: Error fetching sub1 entity report: %v, retrying in 60s", err)
		time.Sleep(60 * time.Second)
		entityReportBySub1, err = c.client.GetEntityReportBySub1(ctx, startDate, now, nil)
		if err != nil {
			log.Printf("Everflow: Retry also failed for sub1 entity report: %v", err)
		}
	}
	if entityReportBySub1 != nil {
		log.Printf("Everflow: Got sub1 entity report with %d entries (campaign-level clicks)",
			len(entityReportBySub1.Table))
	}

	time.Sleep(10 * time.Second)

	// Fetch entity report by sub2 for data-partner click counts + CPA revenue
	entityReportBySub2, err := c.client.GetEntityReportBySub2(ctx, startDate, now, nil)
	if err != nil {
		log.Printf("Everflow: Error fetching sub2 entity report: %v, retrying in 60s", err)
		time.Sleep(60 * time.Second)
		entityReportBySub2, err = c.client.GetEntityReportBySub2(ctx, startDate, now, nil)
		if err != nil {
			log.Printf("Everflow: Retry also failed for sub2 entity report: %v", err)
		}
	}
	if entityReportBySub2 != nil {
		log.Printf("Everflow: Got sub2 entity report with %d entries (data-partner clicks)",
			len(entityReportBySub2.Table))
	}

	time.Sleep(10 * time.Second)

	// Fetch entity report by offer × sub2 for CPM revenue attribution.
	// This cross-tab gives per-offer clicks broken down by data partner (sub2).
	entityReportByOfferSub2, err := c.client.GetEntityReportByOfferAndSub2(ctx, startDate, now, nil)
	if err != nil {
		log.Printf("Everflow: Error fetching offer×sub2 entity report: %v, retrying in 60s", err)
		time.Sleep(60 * time.Second)
		entityReportByOfferSub2, err = c.client.GetEntityReportByOfferAndSub2(ctx, startDate, now, nil)
		if err != nil {
			log.Printf("Everflow: Retry also failed for offer×sub2 entity report: %v", err)
		}
	}
	if entityReportByOfferSub2 != nil {
		log.Printf("Everflow: Got offer×sub2 entity report with %d entries (CPM attribution)",
			len(entityReportByOfferSub2.Table))
	}

	// Fetch conversions in daily chunks for detailed breakdown
	var allConversions []Conversion

	for date := startDate; !date.After(now); date = date.AddDate(0, 0, 1) {
		dateStr := date.Format("2006-01-02")

		// Fetch conversions (approved only)
		convRecords, err := c.client.GetConversionsForDate(ctx, date, true)
		if err != nil {
			log.Printf("Everflow: Error fetching conversions for %s: %v", dateStr, err)
		} else {
			conversions := ProcessConversions(convRecords)
			allConversions = append(allConversions, conversions...)
			log.Printf("Everflow: Got %d conversions for %s", len(conversions), dateStr)
		}

		// Rate limiting to avoid API throttling
		time.Sleep(200 * time.Millisecond)
	}

	// Process and store the data
	c.mu.Lock()
	c.allConversions = allConversions
	c.allClicks = nil // Raw clicks not used anymore, using entity report instead
	// Only overwrite cached entity reports when we got a successful response.
	// This prevents rate-limited (nil) fetches from wiping out valid cached data,
	// which would cause CPM revenue to disappear intermittently.
	if entityReportBySub2 != nil {
		c.sub2EntityReport = entityReportBySub2
	}
	if entityReportByOfferSub2 != nil {
		c.offerSub2EntityReport = entityReportByOfferSub2
	}
	c.metrics = c.aggregateMetricsWithEntityReports(entityReportByDate, entityReportByOffer, entityReportBySub1, allConversions)
	c.metrics.LastFetch = time.Now()
	c.mu.Unlock()

	log.Printf("Everflow: Total %d conversions, $%.2f revenue",
		len(allConversions), c.GetTotalRevenue())

	// Pre-compute data partner analytics cache
	c.refreshDataPartnerCache()
}

// fetchToday fetches only today's data and updates existing metrics
func (c *Collector) fetchToday() {
	log.Println("Fetching Everflow metrics for today...")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	today := time.Now()
	todayStr := today.Format("2006-01-02")

	// Fetch entity report for today (includes clicks)
	entityReport, err := c.client.GetEntityReportByDate(ctx, today, today, nil)
	if err != nil {
		log.Printf("Everflow: Error fetching today's entity report: %v", err)
		return
	}

	// Refresh sub2 entity report (full lookback) for data partner click counts
	startDate := today.AddDate(0, 0, -c.lookbackDays)
	entityReportBySub2, err := c.client.GetEntityReportBySub2(ctx, startDate, today, nil)
	if err != nil {
		log.Printf("Everflow: Error refreshing sub2 entity report: %v", err)
	} else if entityReportBySub2 != nil {
		c.mu.Lock()
		c.sub2EntityReport = entityReportBySub2
		c.mu.Unlock()
		log.Printf("Everflow: Refreshed sub2 entity report with %d entries", len(entityReportBySub2.Table))
	}

	// Refresh offer×sub2 entity report (full lookback) for CPM revenue attribution.
	// This is critical — if fetchAll failed to get this report on startup due to
	// rate limiting, CPM revenue would be missing until this succeeds.
	time.Sleep(10 * time.Second) // Delay to avoid back-to-back BigQuery requests
	entityReportByOfferSub2, err := c.client.GetEntityReportByOfferAndSub2(ctx, startDate, today, nil)
	if err != nil {
		log.Printf("Everflow: Error refreshing offer×sub2 entity report: %v", err)
	} else if entityReportByOfferSub2 != nil {
		c.mu.Lock()
		c.offerSub2EntityReport = entityReportByOfferSub2
		c.mu.Unlock()
		log.Printf("Everflow: Refreshed offer×sub2 entity report with %d entries (CPM attribution)", len(entityReportByOfferSub2.Table))
	}

	// Fetch today's conversions for detailed breakdown
	convRecords, err := c.client.GetConversionsForDate(ctx, today, true)
	if err != nil {
		log.Printf("Everflow: Error fetching today's conversions: %v", err)
		return
	}

	todayConversions := ProcessConversions(convRecords)

	// Update metrics (explicit unlock — do NOT use defer here because
	// refreshDataPartnerCache below needs to acquire its own locks)
	c.mu.Lock()

	// Filter out old today data from conversions
	var filteredConversions []Conversion
	for _, conv := range c.allConversions {
		if conv.ConversionTime.Format("2006-01-02") != todayStr {
			filteredConversions = append(filteredConversions, conv)
		}
	}
	c.allConversions = append(filteredConversions, todayConversions...)

	// Update today's daily metrics from entity report
	if c.metrics != nil && entityReport != nil && len(entityReport.Table) > 0 {
		row := entityReport.Table[0]

		// Update today in daily performance
		found := false
		for i, d := range c.metrics.DailyPerformance {
			if d.Date == todayStr {
				c.metrics.DailyPerformance[i].Clicks = row.Reporting.TotalClick
				c.metrics.DailyPerformance[i].Conversions = row.Reporting.Conversions
				c.metrics.DailyPerformance[i].Revenue = row.Reporting.Revenue
				c.metrics.DailyPerformance[i].Payout = row.Reporting.Payout
				if row.Reporting.TotalClick > 0 {
					c.metrics.DailyPerformance[i].ConversionRate = float64(row.Reporting.Conversions) / float64(row.Reporting.TotalClick)
					c.metrics.DailyPerformance[i].EPC = row.Reporting.Revenue / float64(row.Reporting.TotalClick)
				}
				found = true
				break
			}
		}

		// Add today if not found
		if !found {
			todayPerf := DailyPerformance{
				Date:        todayStr,
				Clicks:      row.Reporting.TotalClick,
				Conversions: row.Reporting.Conversions,
				Revenue:     row.Reporting.Revenue,
				Payout:      row.Reporting.Payout,
			}
			if row.Reporting.TotalClick > 0 {
				todayPerf.ConversionRate = float64(row.Reporting.Conversions) / float64(row.Reporting.TotalClick)
				todayPerf.EPC = row.Reporting.Revenue / float64(row.Reporting.TotalClick)
			}
			c.metrics.DailyPerformance = append([]DailyPerformance{todayPerf}, c.metrics.DailyPerformance...)
		}

		// Update today's summary metrics
		c.metrics.TodayClicks = row.Reporting.TotalClick
		c.metrics.TodayConversions = row.Reporting.Conversions
		c.metrics.TodayRevenue = row.Reporting.Revenue
		c.metrics.TodayPayout = row.Reporting.Payout
	}

	c.metrics.LastFetch = time.Now()

	log.Printf("Everflow: Today - %d clicks, %d conversions, $%.2f revenue",
		c.metrics.TodayClicks, c.metrics.TodayConversions, c.metrics.TodayRevenue)

	c.mu.Unlock()

	// Refresh data partner analytics cache (MUST be called after releasing c.mu
	// because it acquires its own read/write locks internally)
	c.refreshDataPartnerCache()
}
