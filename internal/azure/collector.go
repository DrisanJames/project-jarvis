package azure

import (
	"context"
	"log"
	"sync"
	"time"
)

// Collector collects and aggregates data from Azure Table Storage
type Collector struct {
	client           *Client
	config           Config
	mu               sync.RWMutex
	summary          *DataInjectionSummary
	dailyCounts      []DailyDataSetCount
	lastFetch        time.Time
	refreshInterval  time.Duration
}

// NewCollector creates a new Azure Table Storage collector
func NewCollector(client *Client, cfg Config) *Collector {
	return &Collector{
		client:          client,
		config:          cfg,
		refreshInterval: 5 * time.Minute,
	}
}

// Start begins the collection loop
func (c *Collector) Start(ctx context.Context) {
	// Initial fetch
	c.fetchMetrics(ctx)
	
	ticker := time.NewTicker(c.refreshInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.fetchMetrics(ctx)
		}
	}
}

// FetchNow triggers an immediate metrics fetch
func (c *Collector) FetchNow(ctx context.Context) {
	c.fetchMetrics(ctx)
}

// fetchMetrics fetches all metrics from Azure Table Storage
func (c *Collector) fetchMetrics(ctx context.Context) {
	log.Println("Azure: Fetching data injection metrics...")
	
	// Get all unique partition keys
	partitionKeys, err := c.client.GetPartitionKeys(ctx)
	if err != nil {
		log.Printf("Azure: Error fetching partition keys: %v", err)
		return
	}
	
	log.Printf("Azure: Found %d unique data set codes", len(partitionKeys))
	
	var metrics []DataSetMetrics
	var totalRecords, todayRecords int64
	dataSetsWithGaps := 0
	
	now := time.Now()
	gapThreshold := time.Duration(c.config.GapThresholdHours) * time.Hour
	if gapThreshold == 0 {
		gapThreshold = 24 * time.Hour // Default to 24 hours
	}
	
	// Fetch metrics for each partition key
	for _, pk := range partitionKeys {
		// Get count
		count, err := c.client.CountByPartitionKey(ctx, pk)
		if err != nil {
			log.Printf("Azure: Error counting %s: %v", pk, err)
			continue
		}
		
		// Get today's count
		todayCount, err := c.client.GetTodayCountByPartitionKey(ctx, pk)
		if err != nil {
			log.Printf("Azure: Error getting today count for %s: %v", pk, err)
		}
		
		// Get latest timestamp
		latestTS, err := c.client.GetLatestTimestamp(ctx, pk)
		if err != nil {
			log.Printf("Azure: Error getting latest timestamp for %s: %v", pk, err)
		}
		
		// Get sample contact data for partner info
		var dataPartner, dataSetName string
		sample, err := c.client.GetSampleContactData(ctx, pk)
		if err == nil && sample != nil {
			dataPartner = sample.CustomField.DataPartner
			dataSetName = sample.CustomField.DataSet
		}
		
		// Check for gaps
		hasGap := false
		gapHours := 0.0
		if !latestTS.IsZero() {
			gapDuration := now.Sub(latestTS)
			gapHours = gapDuration.Hours()
			if gapDuration > gapThreshold {
				hasGap = true
				dataSetsWithGaps++
			}
		}
		
		metrics = append(metrics, DataSetMetrics{
			DataSetCode:   pk,
			DataPartner:   dataPartner,
			DataSetName:   dataSetName,
			RecordCount:   count,
			TodayCount:    todayCount,
			LastTimestamp: latestTS,
			HasGap:        hasGap,
			GapHours:      gapHours,
		})
		
		totalRecords += count
		todayRecords += todayCount
	}
	
	// Get daily counts for trending
	dailyCounts, err := c.client.GetDailyCounts(ctx, 7)
	if err != nil {
		log.Printf("Azure: Error getting daily counts: %v", err)
	}
	
	// Update stored metrics
	c.mu.Lock()
	c.summary = &DataInjectionSummary{
		Timestamp:        now,
		TotalRecords:     totalRecords,
		TodayRecords:     todayRecords,
		DataSetsActive:   len(partitionKeys),
		DataSetsWithGaps: dataSetsWithGaps,
		DataSetMetrics:   metrics,
	}
	c.dailyCounts = dailyCounts
	c.lastFetch = now
	c.mu.Unlock()
	
	log.Printf("Azure: Collected metrics for %d data sets, %d total records, %d today",
		len(metrics), totalRecords, todayRecords)
}

// GetSummary returns the current data injection summary
func (c *Collector) GetSummary() *DataInjectionSummary {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.summary
}

// GetDataSetMetrics returns metrics for all data sets
func (c *Collector) GetDataSetMetrics() []DataSetMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.summary == nil {
		return nil
	}
	return c.summary.DataSetMetrics
}

// GetDailyCounts returns daily counts by data set
func (c *Collector) GetDailyCounts() []DailyDataSetCount {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dailyCounts
}

// GetLastFetchTime returns the time of the last successful fetch
func (c *Collector) LastFetch() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastFetch
}

// GetDataSetByCode returns metrics for a specific data set code
func (c *Collector) GetDataSetByCode(code string) *DataSetMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	if c.summary == nil {
		return nil
	}
	
	for _, ds := range c.summary.DataSetMetrics {
		if ds.DataSetCode == code {
			return &ds
		}
	}
	return nil
}

// HasGaps returns true if any data sets have gaps
func (c *Collector) HasGaps() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	if c.summary == nil {
		return false
	}
	
	return c.summary.DataSetsWithGaps > 0
}

// GetDataSetsWithGaps returns all data sets that have gaps
func (c *Collector) GetDataSetsWithGaps() []DataSetMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	if c.summary == nil {
		return nil
	}
	
	var result []DataSetMetrics
	for _, ds := range c.summary.DataSetMetrics {
		if ds.HasGap {
			result = append(result, ds)
		}
	}
	return result
}
