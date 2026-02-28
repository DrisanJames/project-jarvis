package everflow

import (
	"context"
	"log"
	"time"
)

// getSub2ReportForDateRange returns a sub2 entity report for the given date range,
// using a 30-minute cache to avoid repeated Everflow API calls.
func (c *Collector) getSub2ReportForDateRange(ctx context.Context, from, to time.Time) *EntityReportResponse {
	cacheKey := from.Format("2006-01-02") + "|" + to.Format("2006-01-02")

	// Check cache
	c.sub2ReportCacheMu.RLock()
	if entry, ok := c.sub2ReportCache[cacheKey]; ok && time.Since(entry.fetchedAt) < 30*time.Minute {
		c.sub2ReportCacheMu.RUnlock()
		log.Printf("Sub2 report cache hit for %s", cacheKey)
		return entry.report
	}
	c.sub2ReportCacheMu.RUnlock()

	// Cache miss -- fetch from Everflow API
	if c.sub2ReportForDateRange == nil {
		return nil
	}
	report := c.sub2ReportForDateRange(ctx, from, to)
	if report != nil {
		c.sub2ReportCacheMu.Lock()
		c.sub2ReportCache[cacheKey] = &sub2CacheEntry{report: report, fetchedAt: time.Now()}
		c.sub2ReportCacheMu.Unlock()
		log.Printf("Sub2 report fetched and cached for %s (%d rows)", cacheKey, len(report.Table))
	}
	return report
}

// FetchSub2ReportForRange fetches a sub2 entity report from the Everflow API
// for the given date range. This is a thin wrapper around the client method,
// exposed so it can be called from the main.go wiring closure.
func (c *Collector) FetchSub2ReportForRange(ctx context.Context, from, to time.Time) (*EntityReportResponse, error) {
	return c.client.GetEntityReportBySub2(ctx, from, to, nil)
}

// FetchOfferSub2ReportForRange fetches a date-range-specific offer×sub2 entity report.
// Uses a 30-minute cache to avoid repeated API calls for the same date window.
func (c *Collector) FetchOfferSub2ReportForRange(ctx context.Context, from, to time.Time) (*EntityReportResponse, error) {
	cacheKey := from.Format("2006-01-02") + "_" + to.Format("2006-01-02")

	c.offerSub2ReportCacheMu.RLock()
	if entry, ok := c.offerSub2ReportCache[cacheKey]; ok && time.Since(entry.fetchedAt) < 30*time.Minute {
		c.offerSub2ReportCacheMu.RUnlock()
		return entry.report, nil
	}
	c.offerSub2ReportCacheMu.RUnlock()

	report, err := c.client.GetEntityReportByOfferAndSub2(ctx, from, to, nil)
	if err != nil {
		return nil, err
	}

	c.offerSub2ReportCacheMu.Lock()
	c.offerSub2ReportCache[cacheKey] = &sub2CacheEntry{report: report, fetchedAt: time.Now()}
	c.offerSub2ReportCacheMu.Unlock()
	log.Printf("Offer×sub2 report fetched and cached for %s (%d rows)", cacheKey, len(report.Table))

	return report, nil
}

// getConversionsForDateRange returns all conversions for the given date range.
// It first checks if allConversions (from the lookback period) fully covers the
// requested range. If the requested start date is before the lookback start,
// it fetches the missing days from Everflow and caches the combined result.
// The conversions endpoint is NOT a BigQuery query, so it won't hit BQ limits.
func (c *Collector) getConversionsForDateRange(from, to time.Time) []Conversion {
	lookbackStart := time.Now().AddDate(0, 0, -c.lookbackDays)

	// If allConversions already covers the full range, just filter and return
	if !from.Before(lookbackStart) {
		c.mu.RLock()
		var result []Conversion
		for _, conv := range c.allConversions {
			if !conv.ConversionTime.Before(from) && !conv.ConversionTime.After(to) {
				result = append(result, conv)
			}
		}
		c.mu.RUnlock()
		log.Printf("CPA: allConversions covers range %s to %s (%d conversions)",
			from.Format("2006-01-02"), to.Format("2006-01-02"), len(result))
		return result
	}

	// The requested range extends before the lookback. Check conversion cache.
	cacheKey := from.Format("2006-01-02") + "|" + to.Format("2006-01-02")
	c.conversionCacheMu.RLock()
	if entry, ok := c.conversionCache[cacheKey]; ok && time.Since(entry.fetchedAt) < 30*time.Minute {
		c.conversionCacheMu.RUnlock()
		log.Printf("CPA: conversion cache hit for %s (%d conversions)", cacheKey, len(entry.conversions))
		return entry.conversions
	}
	c.conversionCacheMu.RUnlock()

	// Fetch missing days from Everflow (before lookback start)
	log.Printf("CPA: Fetching conversions for %s to %s (lookback starts %s, need to fetch earlier days)",
		from.Format("2006-01-02"), to.Format("2006-01-02"), lookbackStart.Format("2006-01-02"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var allConv []Conversion

	// Fetch days BEFORE the lookback (exclusive — don't re-fetch the lookback
	// start day since it's already in allConversions)
	fetchEnd := lookbackStart.AddDate(0, 0, -1) // day BEFORE lookback starts
	if fetchEnd.After(to) {
		fetchEnd = to
	}
	for date := from; !date.After(fetchEnd); date = date.AddDate(0, 0, 1) {
		convRecords, err := c.client.GetConversionsForDate(ctx, date, true)
		if err != nil {
			log.Printf("CPA: Error fetching conversions for %s: %v", date.Format("2006-01-02"), err)
			continue
		}
		dayConv := ProcessConversions(convRecords)
		allConv = append(allConv, dayConv...)
		time.Sleep(200 * time.Millisecond) // Rate limiting
	}
	log.Printf("CPA: Fetched %d conversions for days before lookback", len(allConv))

	// Add conversions from allConversions that fall within the requested range
	c.mu.RLock()
	for _, conv := range c.allConversions {
		if !conv.ConversionTime.Before(from) && !conv.ConversionTime.After(to) {
			allConv = append(allConv, conv)
		}
	}
	c.mu.RUnlock()

	log.Printf("CPA: Total %d conversions for %s to %s (cached for 30 min)", len(allConv), from.Format("2006-01-02"), to.Format("2006-01-02"))

	// Cache the combined result
	c.conversionCacheMu.Lock()
	c.conversionCache[cacheKey] = &conversionCacheEntry{conversions: allConv, fetchedAt: time.Now()}
	c.conversionCacheMu.Unlock()

	return allConv
}
