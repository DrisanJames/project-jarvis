package ongage

import (
	"context"
	"log"
	"strings"
	"time"
)

// GetTotalSends returns the best estimate of total email volume across all sources.
// It compares three Ongage data sources and returns the highest (most inclusive):
//   1. List-level sends (sum of GetSendsByList)
//   2. ESP performance sends (sum of TotalSent across all ESP connections)
//   3. Pipeline metrics (sum of TotalSent across all days)
func (c *Collector) GetTotalSends() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.metrics == nil {
		return 0
	}

	var listTotal, espTotal, pipelineTotal int64

	// Source 1: List-level sends
	for _, v := range c.listSendsByDS {
		listTotal += v
	}

	// Source 2: ESP performance
	for _, esp := range c.metrics.ESPPerformance {
		espTotal += esp.TotalSent
	}

	// Source 3: Pipeline metrics (daily totals)
	for _, day := range c.metrics.PipelineMetrics {
		pipelineTotal += day.TotalSent
	}

	log.Printf("Ongage volume sources: list=%d, esp=%d, pipeline=%d", listTotal, espTotal, pipelineTotal)

	// Use the highest value (most inclusive)
	best := listTotal
	if espTotal > best {
		best = espTotal
	}
	if pipelineTotal > best {
		best = pipelineTotal
	}
	return best
}

// GetTotalSendsForDateRange returns the total email volume for the specified
// date range. Results are cached for 30 minutes to avoid Ongage API rate limits.
func (c *Collector) GetTotalSendsForDateRange(ctx context.Context, from, to time.Time) int64 {
	cacheKey := from.Format("2006-01-02") + "|" + to.Format("2006-01-02")

	// Check cache first
	c.volumeCacheMu.RLock()
	if entry, ok := c.volumeCache[cacheKey]; ok && time.Since(entry.fetchedAt) < 30*time.Minute {
		c.volumeCacheMu.RUnlock()
		log.Printf("Ongage volume cache hit for %s: %d", cacheKey, entry.total)
		return entry.total
	}
	c.volumeCacheMu.RUnlock()

	// Cache miss -- make fresh API calls
	// Source 1: Daily stats (pipeline) -- usually the most complete
	var pipelineTotal int64
	dailyRows, err := c.client.GetDailyStatsForDateRange(ctx, from, to)
	if err != nil {
		log.Printf("Ongage: Failed to fetch daily stats for %s to %s: %v",
			from.Format("2006-01-02"), to.Format("2006-01-02"), err)
	} else {
		for _, row := range dailyRows {
			pipelineTotal += getInt64Value(row, "sent")
		}
	}

	// Source 2: List-level sends
	var listTotal int64
	listRows, err := c.client.GetSendsByListForDateRange(ctx, from, to)
	if err != nil {
		log.Printf("Ongage: Failed to fetch list sends for %s to %s: %v",
			from.Format("2006-01-02"), to.Format("2006-01-02"), err)
	} else {
		for _, row := range listRows {
			listTotal += getInt64Value(row, "sent")
		}
	}

	// Use the highest (most inclusive)
	best := pipelineTotal
	if listTotal > best {
		best = listTotal
	}

	log.Printf("Ongage volume for %s to %s: pipeline=%d, list=%d, using=%d",
		from.Format("2006-01-02"), to.Format("2006-01-02"), pipelineTotal, listTotal, best)

	// If both returned 0 (likely rate limited), fall back to cached periodic data
	if best == 0 {
		fallback := c.GetTotalSends()
		if fallback > 0 {
			log.Printf("Ongage: Using cached periodic total %d as fallback (API likely rate limited)", fallback)
			best = fallback
		}
	}

	// Cache the result
	c.volumeCacheMu.Lock()
	c.volumeCache[cacheKey] = &volumeCacheEntry{total: best, fetchedAt: time.Now()}
	c.volumeCacheMu.Unlock()

	return best
}

// GetListSendsByDataSetCode returns a map of data_set_code -> total sends.
// This is the authoritative source for per-data-partner sending volume.
// The map is built by combining Ongage list metadata (list_id -> name) with
// reporting data (list_id -> sum(sent)). List names are parsed to extract
// the data set code using the same convention as Everflow sub2 parsing.
func (c *Collector) GetListSendsByDataSetCode() map[string]int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.listSendsByDS == nil {
		return nil
	}
	// Return a copy to avoid race conditions
	result := make(map[string]int64, len(c.listSendsByDS))
	for k, v := range c.listSendsByDS {
		result[k] = v
	}
	return result
}

// GetListSendsByDataSetCodeForDateRange returns per-data-set-code volume for
// a specific date range. All data is dynamically fetched from the Ongage API.
//
// Resolution strategy (in order of preference):
//  0. Contact Activity API: Creates an async report with data_set + sent fields,
//     filtered by data_set notempty. Exports the CSV and aggregates sent counts
//     per data_set value. This gives exact per-DATA_SET volumes — no estimation.
//  1. Segment-level grouping: Query sends grouped by segment_id and parse segment
//     names for data set codes. If segments are named after data sets (e.g.,
//     "M77_WIT_OPENERS"), this provides per-data-set granularity dynamically.
//  2. List-level grouping: Query sends grouped by list_id and derive data set codes
//     from list names. Typically produces only 1-2 entries (one per account).
//  3. Empty map: If all API calls fail, return an empty map. The Everflow collector
//     will fall back to proportional estimation using total sends × click share.
//
// Results are cached for 30 minutes to reduce API calls.
func (c *Collector) GetListSendsByDataSetCodeForDateRange(ctx context.Context, from, to time.Time) map[string]int64 {
	cacheKey := from.Format("2006-01-02") + "|" + to.Format("2006-01-02")

	// Check cache — Contact Activity results (exact=true) are cached for 24 hours,
	// fallback results (exact=false) are cached for 30 minutes.
	c.dsCacheMu.RLock()
	if entry, ok := c.dsCache[cacheKey]; ok {
		ttl := 30 * time.Minute
		if entry.exact {
			ttl = 24 * time.Hour
		}
		if time.Since(entry.fetchedAt) < ttl {
			c.dsCacheMu.RUnlock()
			src := "estimated"
			if entry.exact {
				src = "exact/contact-activity"
			}
			log.Printf("Ongage DS volume cache hit for %s (%d entries, %s)", cacheKey, len(entry.data), src)
			return entry.data
		}
	}
	c.dsCacheMu.RUnlock()

	// Check S3 persistent cache (survives server restarts).
	// If a Contact Activity result was written to S3 less than 24h ago, load it.
	if c.s3Cache != nil {
		s3Data, generatedAt, err := c.s3Cache.Load(ctx, from, to)
		if err != nil {
			log.Printf("Ongage: S3 volume cache load error for %s: %v (continuing without S3)", cacheKey, err)
		} else if s3Data != nil && len(s3Data) > 2 && time.Since(generatedAt) < 24*time.Hour {
			log.Printf("Ongage: S3 volume cache hit for %s (%d entries, generated %s)",
				cacheKey, len(s3Data), generatedAt.Format(time.RFC3339))
			// Promote to in-memory cache
			c.dsCacheMu.Lock()
			c.dsCache[cacheKey] = &dsCacheEntry{data: s3Data, fetchedAt: generatedAt, exact: true}
			c.dsCacheMu.Unlock()
			return s3Data
		} else if s3Data != nil {
			log.Printf("Ongage: S3 volume cache stale for %s (generated %s, %d entries)",
				cacheKey, generatedAt.Format(time.RFC3339), len(s3Data))
		}
	}

	var result map[string]int64

	// Strategy 0: Contact Activity API (most accurate, async).
	// The Contact Activity flow creates an async report on Ongage's side that
	// can take 5-15 minutes to build. We DO NOT block the HTTP response for this.
	// Instead:
	//   - If a Contact Activity result is already cached → use it (exact data)
	//   - If not cached and no background job running → kick off a goroutine
	//   - Fall through to Strategy 1/2 for immediate (estimated) response
	//   - The goroutine will update dsCache when complete → next request gets exact data
	c.caInFlightMu.Lock()
	alreadyRunning := c.caInFlight[cacheKey]
	if !alreadyRunning {
		c.caInFlight[cacheKey] = true
		log.Printf("Ongage: Launching background Contact Activity report for %s to %s (will update cache when complete)",
			from.Format("2006-01-02"), to.Format("2006-01-02"))
		go func() {
			defer func() {
				c.caInFlightMu.Lock()
				delete(c.caInFlight, cacheKey)
				c.caInFlightMu.Unlock()
			}()
			caResult, caErr := c.fetchVolumeViaContactActivity(context.Background(), from, to)
			if caErr != nil {
				log.Printf("Ongage: Background Contact Activity failed for %s: %v", cacheKey, caErr)
				return
			}
			if len(caResult) <= 2 {
				log.Printf("Ongage: Background Contact Activity returned only %d entries for %s, not caching", len(caResult), cacheKey)
				return
			}
			log.Printf("Ongage: Background Contact Activity completed for %s: %d data-set codes (exact volumes now cached)",
				cacheKey, len(caResult))

			// Persist to S3 so the result survives server restarts.
			if c.s3Cache != nil {
				if s3Err := c.s3Cache.Save(context.Background(), from, to, caResult); s3Err != nil {
					log.Printf("Ongage: Warning: failed to save Contact Activity to S3 for %s: %v", cacheKey, s3Err)
				}
			}

			c.dsCacheMu.Lock()
			c.dsCache[cacheKey] = &dsCacheEntry{data: caResult, fetchedAt: time.Now(), exact: true}
			c.dsCacheMu.Unlock()
		}()
	} else {
		log.Printf("Ongage: Contact Activity report already in progress for %s, using fallback strategies", cacheKey)
	}
	c.caInFlightMu.Unlock()

	// Strategy 1: Segment-level grouping (fallback).
	// If Ongage segments are named after data set codes (e.g., "M77_WIT_OPENERS",
	// "ATT_30DC_ALL"), we can parse segment names to derive per-data-set volumes
	// dynamically. This is the most granular approach available via the API.
	if len(result) == 0 {
		segRows, err := c.client.GetSendsBySegmentForDateRange(ctx, from, to)
		if err == nil && len(segRows) > 0 {
			result = c.buildSegmentSendsByDataSetCode(segRows)
			if len(result) > 2 {
				log.Printf("Ongage: Segment-level volume map has %d data-set entries for %s to %s (dynamic)",
					len(result), from.Format("2006-01-02"), to.Format("2006-01-02"))
			} else {
				// Segment names didn't yield meaningful data set code granularity
				log.Printf("Ongage: Segment-level query returned %d rows but only %d matched data-set codes, falling back to list-level",
					len(segRows), len(result))
				result = nil
			}
		} else if err != nil {
			log.Printf("Ongage: Segment-level query failed: %v", err)
		}
	}

	// Strategy 2: List-level grouping (fallback).
	// List-level typically returns only 1-2 entries (one per Ongage list).
	// The Everflow collector will use proportional estimation when this is the
	// only data available.
	if len(result) == 0 {
		lists, err := c.client.GetLists(ctx)
		if err != nil {
			log.Printf("Ongage: Failed to fetch list metadata for DS volume: %v", err)
		} else {
			sendRows, err := c.client.GetSendsByListForDateRange(ctx, from, to)
			if err != nil {
				log.Printf("Ongage: Failed to fetch list sends for %s to %s: %v",
					from.Format("2006-01-02"), to.Format("2006-01-02"), err)
			} else {
				result = c.buildListSendsByDataSetCode(lists, sendRows)
				log.Printf("Ongage: List-level volume map has %d entries for %s to %s (per-partner volumes use proportional estimation from Everflow click share)",
					len(result), from.Format("2006-01-02"), to.Format("2006-01-02"))
			}
		}
	}

	if result == nil {
		result = make(map[string]int64)
	}

	// Only cache non-empty results. Empty maps typically mean API rate limiting
	// or transient failures — we don't want to lock in a bad result for 30 minutes.
	if len(result) > 0 {
		c.dsCacheMu.Lock()
		c.dsCache[cacheKey] = &dsCacheEntry{data: result, fetchedAt: time.Now()}
		c.dsCacheMu.Unlock()
	} else {
		log.Printf("Ongage: Not caching empty DS volume result for %s (API may be rate limited)", cacheKey)
	}

	return result
}

// fetchSegmentSends queries sends-by-segment and extracts data set codes from segment names.
// Segment names may follow conventions like "M77_WIT_OPENERS", "GLB_BR_ALL", etc.
// We parse the prefix and check if it matches a known data partner.
func (c *Collector) fetchSegmentSends(ctx context.Context) (map[string]int64, error) {
	rows, err := c.client.GetSendsBySegment(ctx, c.lookbackDays)
	if err != nil {
		return nil, err
	}

	result := make(map[string]int64)
	for _, row := range rows {
		segName := getStringValue(row, "segment_name")
		sent := getInt64Value(row, "sent")
		if segName == "" || sent == 0 {
			continue
		}

		// Try to extract a data set code or partner prefix from the segment name.
		// Segment names may be like "M77_WIT_OPENERS", "GLB_BR_CLICKERS", etc.
		// We look for a known data partner prefix at the start of the segment name.
		upper := strings.ToUpper(strings.TrimSpace(segName))

		// Try matching the first N characters (3 or 4) against known prefixes
		matched := false
		for prefix := range knownDataPartnerPrefixes {
			if strings.HasPrefix(upper, prefix+"_") || upper == prefix {
				// Extract the data set code (everything before the engagement suffix)
				dsCode := extractDataSetCodeFromSegment(upper)
				if dsCode != "" {
					result[dsCode] += sent
					matched = true
				}
				break
			}
		}
		// unmatched segments are silently skipped
		_ = matched
	}

	return result, nil
}

// knownDataPartnerPrefixes is a set of known data partner prefixes for quick lookup.
// This mirrors everflow.DataPartnerMapping but avoids a circular import.
var knownDataPartnerPrefixes = map[string]bool{
	"ATT": true, "GLB": true, "SCO": true, "M77": true,
	"IGN": true, "HAR": true, "EVS": true, "MAS": true,
}

// extractDataSetCodeFromSegment tries to extract a data set code from a segment name.
// For example: "M77_WIT_OPENERS" -> "M77_WIT", "GLB_BR_ALL" -> "GLB_BR"
// It strips common engagement suffixes like _OPENERS, _CLICKERS, _ALL, _ABS, etc.
func extractDataSetCodeFromSegment(name string) string {
	// Common engagement/behavioral suffixes to strip
	suffixes := []string{
		"_OPENERS", "_CLICKERS", "_ALL", "_ACTIVE", "_INACTIVE",
		"_ABS", "_CAB", "_OPENS", "_CLICKS", "_ENGAGED", "_UNENGAGED",
		"_30D", "_60D", "_90D", "_7D", "_14D",
	}

	result := name
	for _, suffix := range suffixes {
		if strings.HasSuffix(result, suffix) {
			result = strings.TrimSuffix(result, suffix)
		}
	}

	// Strip trailing underscores
	result = strings.TrimRight(result, "_")
	if result == "" {
		return ""
	}

	return result
}

// buildSegmentSendsByDataSetCode aggregates sends from per-segment report rows
// by extracting data set codes from segment_name. Segment names may follow
// conventions like "M77_WIT_OPENERS", "GLB_BR_ALL", "ATT_30DC_CLICKERS", etc.
// We parse the name, strip engagement suffixes, and check if the prefix matches
// a known data partner.
func (c *Collector) buildSegmentSendsByDataSetCode(rows []ReportRow) map[string]int64 {
	result := make(map[string]int64)
	var matchedCount, unmatchedCount int

	for _, row := range rows {
		name := strings.TrimSpace(getStringValue(row, "segment_name"))
		sent := getInt64Value(row, "sent")
		if name == "" || sent == 0 {
			continue
		}

		upper := strings.ToUpper(name)

		// Check if segment name starts with a known data partner prefix
		matched := false
		for prefix := range knownDataPartnerPrefixes {
			if strings.HasPrefix(upper, prefix+"_") || upper == prefix {
				dsCode := extractDataSetCodeFromSegment(upper)
				if dsCode != "" {
					result[dsCode] += sent
					matched = true
					matchedCount++
				}
				break
			}
		}
		if !matched {
			unmatchedCount++
		}
	}

	log.Printf("Ongage: Segment parsing: %d matched, %d unmatched out of %d rows, %d unique DS codes",
		matchedCount, unmatchedCount, len(rows), len(result))

	// Log first 10 segment names for diagnostics (helps identify naming conventions)
	if matchedCount == 0 && len(rows) > 0 {
		limit := 10
		if len(rows) < limit {
			limit = len(rows)
		}
		log.Printf("Ongage: Sample segment names (first %d):", limit)
		for i := 0; i < limit; i++ {
			name := getStringValue(rows[i], "segment_name")
			sent := getInt64Value(rows[i], "sent")
			log.Printf("  [%d] %q sent=%d", i, name, sent)
		}
	}

	return result
}

// buildListSendsByDataSetCode combines list metadata with sends-by-list report data
// to produce a map of data_set_code -> total sends.
//
// Strategy:
//   - Build a map of list_id (int) -> list name from GetLists() response.
//   - For each row from GetSendsByList(), look up the list name.
//   - Parse the list name to extract a data set code. List names follow the same
//     convention as Everflow sub2 values (e.g. "M77_WIT" -> data set code "M77_WIT",
//     partner prefix "M77"). If the list name doesn't match a known data partner
//     prefix, it's skipped (it's likely an internal/non-partner list).
//   - Aggregate sends per data set code.
func (c *Collector) buildListSendsByDataSetCode(lists []ListInfo, sendRows []ReportRow) map[string]int64 {
	// Build list_id -> name map
	listNameByID := make(map[int]string, len(lists))
	for _, li := range lists {
		listNameByID[li.ID] = li.Name
	}

	result := make(map[string]int64)

	for _, row := range sendRows {
		listID := int(getInt64Value(row, "list_id"))
		sent := getInt64Value(row, "sent")
		if listID == 0 || sent == 0 {
			continue
		}

		listName, ok := listNameByID[listID]
		if !ok || listName == "" {
			continue
		}

		// Extract data set code from list name.
		// List names may follow the convention: PREFIX_SUFFIX (e.g. "M77_WIT", "GLB_BR")
		// Or they may be full names that need more sophisticated parsing.
		// Strip leading/trailing whitespace and trailing underscores.
		dsCode := strings.TrimSpace(listName)
		dsCode = strings.TrimRight(dsCode, "_")
		if dsCode == "" {
			continue
		}

		// Normalize to uppercase to match Everflow conventions.
		// The Everflow collector will match on prefix (e.g. M77, GLB) and
		// handle unknowns. We include all list data set codes here.
		result[strings.ToUpper(dsCode)] += sent
	}

	return result
}
