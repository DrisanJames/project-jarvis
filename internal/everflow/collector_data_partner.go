package everflow

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// refreshDataPartnerCache recomputes the data partner analytics and stores it.
// This is called from fetchAll() and fetchToday() after the main metrics are updated.
// NOTE: caller must NOT hold c.mu — this method acquires it internally via buildDataPartnerAnalytics.
func (c *Collector) refreshDataPartnerCache() {
	log.Println("Refreshing data partner analytics cache...")

	// Use a broad window: lookback days ago to now
	now := time.Now()
	start := now.AddDate(0, 0, -c.lookbackDays)
	result := c.buildDataPartnerAnalytics(start, now)
	result.CachedAt = time.Now().UTC().Format(time.RFC3339)

	c.mu.Lock()
	c.dataPartnerCache = result
	c.dataPartnerCacheTime = time.Now()
	c.mu.Unlock()

	log.Printf("Data partner cache refreshed: %d partners, $%.2f total revenue",
		len(result.Partners), result.Totals.Revenue)
}

// RefreshDataPartnerCache is the public entry point for manual cache rebuilds.
func (c *Collector) RefreshDataPartnerCache() *DataPartnerAnalyticsResponse {
	c.refreshDataPartnerCache()
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dataPartnerCache
}

// GetDataPartnerAnalytics returns data-partner analytics for the requested
// date range.  It always computes fresh from in-memory data so the global
// date filter drives the numbers.  The internal cache is only used when a
// caller provides zero-value dates (i.e., the periodic background refresh).
func (c *Collector) GetDataPartnerAnalytics(startDate, endDate time.Time) *DataPartnerAnalyticsResponse {
	// Always compute inline with the requested date range so the
	// global date filter truly drives the numbers.
	result := c.buildDataPartnerAnalytics(startDate, endDate)
	result.CachedAt = time.Now().UTC().Format(time.RFC3339)
	return result
}

// buildDataPartnerAnalytics does the actual aggregation work. It acquires c.mu
// as a reader, so callers must NOT already hold it.
func (c *Collector) buildDataPartnerAnalytics(startDate, endDate time.Time) *DataPartnerAnalyticsResponse {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.metrics == nil {
		return &DataPartnerAnalyticsResponse{}
	}

	// ── helpers ──────────────────────────────────────────────────────────
	isCPMOffer := func(offerName string) bool {
		return strings.Contains(strings.ToUpper(offerName), "CPM")
	}

	type dsAccum struct {
		code        string
		clicks      int64
		conversions int64
		revenue     float64
	}
	type offerAccum struct {
		offerID     string
		offerName   string
		isCPM       bool
		clicks      int64
		conversions int64
		revenue     float64
	}
	type partnerAccum struct {
		prefix      string
		name        string
		dataSetCode string
		clicks      int64
		conversions int64
		revenue     float64
		cpaRevenue  float64
		cpmRevenue  float64
		payout      float64
		daily       map[string]*DataPartnerDailyMetrics // keyed by "2006-01-02"
		datasets    map[string]*dsAccum                 // keyed by full data-set code
		offers      map[string]*offerAccum              // keyed by offer ID
	}

	accum := make(map[string]*partnerAccum) // keyed by partner GROUP prefix (upper)
	// getAccum retrieves or creates a partner accumulator.
	// IMPORTANT: groupKey and groupName must already be resolved via
	// ResolvePartnerGroup (or ParseSub2) — this function does NOT re-resolve.
	getAccum := func(groupKey, groupName, dsCode string) *partnerAccum {
		if a, ok := accum[groupKey]; ok {
			return a
		}
		a := &partnerAccum{
			prefix:      groupKey,
			name:        groupName,
			dataSetCode: dsCode,
			daily:       make(map[string]*DataPartnerDailyMetrics),
			datasets:    make(map[string]*dsAccum),
			offers:      make(map[string]*offerAccum),
		}
		accum[groupKey] = a
		return a
	}
	getDS := func(a *partnerAccum, code string) *dsAccum {
		if code == "" {
			code = a.prefix // fallback
		}
		d, ok := a.datasets[code]
		if !ok {
			d = &dsAccum{code: code}
			a.datasets[code] = d
		}
		return d
	}

	// ── aggregate clicks from sub2 entity report (accurate totals) ──────
	// Use a date-range-specific sub2 report when available, so click counts
	// match the global date filter. Fall back to the cached periodic report.
	sub2Report := c.sub2EntityReport
	if c.sub2ReportForDateRange != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		dateReport := c.getSub2ReportForDateRange(ctx, startDate, endDate)
		cancel()
		if dateReport != nil && len(dateReport.Table) > 0 {
			sub2Report = dateReport
			log.Printf("DataPartner: Using date-filtered sub2 report for %s to %s (%d rows)",
				startDate.Format("2006-01-02"), endDate.Format("2006-01-02"), len(dateReport.Table))
		} else {
			log.Printf("DataPartner: Date-filtered sub2 report unavailable, using cached periodic report")
		}
	}

	if sub2Report != nil {
		for _, row := range sub2Report.Table {
			var sub2Value string
			for _, col := range row.Columns {
				if col.ColumnType == "sub2" {
					sub2Value = col.Label
					break
				}
			}
			if sub2Value == "" {
				continue
			}

			// Parse sub2 to extract data partner
			parsed := ParseSub2(sub2Value)
			if parsed == nil || parsed.IsEmailHash || parsed.PartnerName == "" {
				continue
			}

			a := getAccum(parsed.PartnerPrefix, parsed.PartnerName, parsed.DataSetCode)
			a.clicks += row.Reporting.TotalClick

			// Per-data-set-code click breakdown
			ds := getDS(a, parsed.DataSetCode)
			ds.clicks += row.Reporting.TotalClick
		}
	}

	// ── CPM revenue attribution from offer × sub2 entity report ──────────
	offerSub2Report := c.offerSub2EntityReport
	{
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		dateReport, err := c.FetchOfferSub2ReportForRange(ctx, startDate, endDate)
		cancel()
		if err == nil && dateReport != nil && len(dateReport.Table) > 0 {
			offerSub2Report = dateReport
			log.Printf("DataPartner: Using date-filtered offer×sub2 report for %s to %s (%d rows)",
				startDate.Format("2006-01-02"), endDate.Format("2006-01-02"), len(dateReport.Table))
		} else {
			if err != nil {
				log.Printf("DataPartner: Date-filtered offer×sub2 report error: %v, using cached periodic report", err)
			} else {
				log.Printf("DataPartner: Date-filtered offer×sub2 report unavailable, using cached periodic report")
			}
		}
	}

	if offerSub2Report != nil && len(offerSub2Report.Table) > 0 {
		// Fetch date-range-specific offer entity report for accurate CPM revenue
		offerRevenueMap := make(map[string]float64)
		{
			ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Minute)
			dateOfferReport, err := c.client.GetEntityReportByOffer(ctx2, startDate, endDate, nil)
			cancel2()
			if err == nil && dateOfferReport != nil && len(dateOfferReport.Table) > 0 {
				log.Printf("CPM attribution: using date-filtered offer entity report (%s to %s, %d rows)",
					startDate.Format("2006-01-02"), endDate.Format("2006-01-02"), len(dateOfferReport.Table))
				for _, row := range dateOfferReport.Table {
					var offerID, offerName string
					for _, col := range row.Columns {
						if col.ColumnType == "offer" {
							offerID = col.ID
							offerName = col.Label
							break
						}
					}
					if offerID == "" || !isCPMOffer(offerName) {
						continue
					}
					offerRevenueMap[offerID] = row.Reporting.Revenue
				}
			} else {
				log.Printf("CPM attribution: date-filtered offer report unavailable (err=%v), falling back to cached OfferPerformance", err)
				if c.metrics != nil {
					for _, op := range c.metrics.OfferPerformance {
						if isCPMOffer(op.OfferName) {
							offerRevenueMap[op.OfferID] = op.Revenue
						}
					}
				}
			}
		}
		var totalCPMOfferRevenue float64
		for _, rev := range offerRevenueMap {
			totalCPMOfferRevenue += rev
		}
		log.Printf("CPM attribution: found %d CPM offers with $%.2f total revenue in offer entity report",
			len(offerRevenueMap), totalCPMOfferRevenue)

		// Scan offer×sub2 rows to get per-partner click distribution for CPM offers.
		type cpmAgg struct {
			offerID       string
			offerName     string
			totalRevenue  float64
			partnerClicks map[string]int64
			totalClicks   int64
		}
		cpmAggMap := make(map[string]*cpmAgg)

		for _, row := range offerSub2Report.Table {
			var offerID, offerName, sub2Value string
			for _, col := range row.Columns {
				switch col.ColumnType {
				case "offer":
					offerID = col.ID
					offerName = col.Label
				case "sub2":
					sub2Value = col.Label
				}
			}
			if offerID == "" || sub2Value == "" {
				continue
			}
			if !isCPMOffer(offerName) {
				continue
			}
			parsed := ParseSub2(sub2Value)
			if parsed == nil || parsed.IsEmailHash || parsed.PartnerName == "" {
				continue
			}

			agg, ok := cpmAggMap[offerID]
			if !ok {
				totalRev := offerRevenueMap[offerID]
				agg = &cpmAgg{
					offerID:       offerID,
					offerName:     offerName,
					totalRevenue:  totalRev,
					partnerClicks: make(map[string]int64),
				}
				cpmAggMap[offerID] = agg
			}
			agg.partnerClicks[strings.ToUpper(parsed.PartnerPrefix)] += row.Reporting.TotalClick
			agg.totalClicks += row.Reporting.TotalClick
		}

		// Attribute CPM revenue by click share
		var totalCPMAttributed float64
		for _, agg := range cpmAggMap {
			if agg.totalClicks == 0 || agg.totalRevenue == 0 {
				if agg.totalRevenue > 0 && agg.totalClicks == 0 {
					log.Printf("CPM attribution WARN: offer %s (%s) has $%.2f revenue but 0 clicks",
						agg.offerID, agg.offerName, agg.totalRevenue)
				}
				continue
			}
			for partnerKey, partnerClicks := range agg.partnerClicks {
				attributedRevenue := agg.totalRevenue * float64(partnerClicks) / float64(agg.totalClicks)

				groupName := partnerKey
				if n, ok := partnerGroupNames[partnerKey]; ok {
					groupName = n
				}
				a := getAccum(partnerKey, groupName, "")
				a.revenue += attributedRevenue
				a.cpmRevenue += attributedRevenue

				oa, ok := a.offers[agg.offerID]
				if !ok {
					oa = &offerAccum{
						offerID:   agg.offerID,
						offerName: agg.offerName,
						isCPM:     true,
					}
					a.offers[agg.offerID] = oa
				} else {
					oa.isCPM = true
				}
				oa.clicks += partnerClicks
				oa.revenue += attributedRevenue

				totalCPMAttributed += attributedRevenue
			}
			log.Printf("CPM attribution: offer %s (%s) total=$%.2f clicks=%d across %d partners",
				agg.offerID, agg.offerName, agg.totalRevenue, agg.totalClicks, len(agg.partnerClicks))
		}
		unattribCPMRevenue := totalCPMOfferRevenue - totalCPMAttributed
		if unattribCPMRevenue > 0 {
			log.Printf("CPM attribution: $%.2f unattributed CPM revenue", unattribCPMRevenue)
		}
		log.Printf("CPM attribution complete: %d CPM offers, $%.2f attributed of $%.2f total ($%.2f unattributed)",
			len(cpmAggMap), totalCPMAttributed, totalCPMOfferRevenue, unattribCPMRevenue)
	} else {
		log.Printf("DataPartner: No offer×sub2 report available, skipping CPM attribution")
	}

	// ── CPA from allConversions (date-range-aware) ──────────────────────
	{
		conversionsForRange := c.getConversionsForDateRange(startDate, endDate)
		for _, conv := range conversionsForRange {
			var groupKey, groupName, dsCode string
			parsed := ParseSub2(conv.Sub2)
			if parsed != nil && !parsed.IsEmailHash && parsed.PartnerName != "" {
				groupKey = parsed.PartnerPrefix
				groupName = parsed.PartnerName
				dsCode = parsed.DataSetCode
			} else if conv.DataSetCode != "" {
				groupKey, groupName = ResolvePartnerGroup(conv.DataSetCode)
				dsCode = conv.DataSetCode
			} else if conv.DataPartner != "" {
				groupKey, groupName = ResolvePartnerGroup(conv.DataPartner)
				dsCode = conv.DataPartner
			} else {
				continue
			}
			a := getAccum(groupKey, groupName, dsCode)
			a.conversions++
			a.payout += conv.Payout
			a.revenue += conv.Revenue
			a.cpaRevenue += conv.Revenue

			ds := getDS(a, dsCode)
			ds.conversions++
			ds.revenue += conv.Revenue

			if conv.OfferID != "" {
				oa, ok := a.offers[conv.OfferID]
				if !ok {
					oa = &offerAccum{offerID: conv.OfferID, offerName: conv.OfferName, isCPM: false}
					a.offers[conv.OfferID] = oa
				}
				oa.conversions++
				oa.revenue += conv.Revenue
			}

			day := conv.ConversionTime.Format("2006-01-02")
			d, ok := a.daily[day]
			if !ok {
				d = &DataPartnerDailyMetrics{Date: day}
				a.daily[day] = d
			}
			d.Conversions++
			d.Revenue += conv.Revenue
		}
	}

	// ── resolve volume ──────────────────────────────────────────────────
	var grandTotalClicks, grandTotalConversions int64
	knownPrefixes := make(map[string]bool, len(accum))
	for key, a := range accum {
		knownPrefixes[key] = true
		grandTotalClicks += a.clicks
		grandTotalConversions += a.conversions
	}
	vr := c.newVolumeResolver(knownPrefixes, grandTotalClicks, grandTotalConversions, startDate, endDate)
	totalESPSends := vr.totalESPSends

	// ── build response partners slice ───────────────────────────────────
	var partners []DataPartnerPerformance
	var totalClicks, totalConversions int64
	var totalRevenue, totalCPARevenue, totalCPMRevenue float64
	var totalVolume int64

	for _, a := range accum {
		partnerVolume := vr.resolvePartnerVolume(a.prefix, a.clicks, a.conversions)

		p := DataPartnerPerformance{
			PartnerPrefix: a.prefix,
			PartnerName:   a.name,
			DataSetCode:   a.dataSetCode,
			Clicks:        a.clicks,
			Conversions:   a.conversions,
			Revenue:       a.revenue,
			CPARevenue:    a.cpaRevenue,
			CPMRevenue:    a.cpmRevenue,
			Volume:        partnerVolume,
			Payout:        a.payout,
		}
		if a.clicks > 0 {
			p.ConversionRate = float64(a.conversions) / float64(a.clicks) * 100
			p.EPC = a.revenue / float64(a.clicks)
		}

		// Build per-data-set-code breakdown with resolved volume
		p.DataSetBreakdown = make([]DataSetCodeMetrics, 0, len(a.datasets))
		for _, ds := range a.datasets {
			dsVolume := vr.resolveVolume(ds.code, ds.clicks, ds.conversions)
			dsm := DataSetCodeMetrics{
				DataSetCode: ds.code,
				Clicks:      ds.clicks,
				Conversions: ds.conversions,
				Revenue:     ds.revenue,
				Volume:      dsVolume,
			}
			if ds.clicks > 0 {
				dsm.CVR = float64(ds.conversions) / float64(ds.clicks) * 100
				dsm.EPC = ds.revenue / float64(ds.clicks)
			}
			p.DataSetBreakdown = append(p.DataSetBreakdown, dsm)
		}
		sortDataSetBreakdown(p.DataSetBreakdown)

		// Build per-offer breakdown
		p.OfferBreakdown = make([]OfferPartnerMetrics, 0, len(a.offers))
		for _, oa := range a.offers {
			p.OfferBreakdown = append(p.OfferBreakdown, OfferPartnerMetrics{
				OfferID:     oa.offerID,
				OfferName:   oa.offerName,
				IsCPM:       oa.isCPM,
				Clicks:      oa.clicks,
				Conversions: oa.conversions,
				Revenue:     oa.revenue,
			})
		}
		sortOfferBreakdown(p.OfferBreakdown)

		// Flatten daily map into sorted slice (always init to empty, never nil)
		p.DailySeries = make([]DataPartnerDailyMetrics, 0, len(a.daily))
		for _, dm := range a.daily {
			p.DailySeries = append(p.DailySeries, *dm)
		}
		sortDailyMetrics(p.DailySeries)

		partners = append(partners, p)
		totalClicks += a.clicks
		totalConversions += a.conversions
		totalRevenue += a.revenue
		totalCPARevenue += a.cpaRevenue
		totalCPMRevenue += a.cpmRevenue
		totalVolume += partnerVolume
	}

	sortPartnersByRevenue(partners)

	// ── offer-centric view + month-over-month ───────────────────────────
	cpmOffers, cpaOffers := buildOfferCentricView(partners)
	mom := c.buildDataPartnerMoM(totalClicks)

	// ── ESP cost defaults for the cost playground ────────────────────────
	defaultVolume, defaultCostECPM, totalESPCost := c.espCostPlaygroundDefaults(totalESPSends)

	displayTotalVolume := totalVolume
	if totalESPSends > totalVolume {
		displayTotalVolume = totalESPSends
	}

	return &DataPartnerAnalyticsResponse{
		Partners: partners,
		Totals: DataPartnerPeriodSummary{
			Label:       fmt.Sprintf("%s – %s", startDate.Format("Jan 2"), endDate.Format("Jan 2, 2006")),
			Clicks:      totalClicks,
			Conversions: totalConversions,
			Revenue:     totalRevenue,
			CPARevenue:  totalCPARevenue,
			CPMRevenue:  totalCPMRevenue,
			Volume:      displayTotalVolume,
		},
		MoMComparison:   mom,
		DefaultVolume:   defaultVolume,
		DefaultCostECPM: defaultCostECPM,
		TotalESPCost:    totalESPCost,
		CPMOffers:       cpmOffers,
		CPAOffers:       cpaOffers,
	}
}
