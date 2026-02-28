package snowflake

import (
	"context"
	"log"
	"sync"
	"time"
)

// Collector collects and aggregates validation data from Snowflake
type Collector struct {
	client          *Client
	config          Config
	mu              sync.RWMutex
	summary         *ValidationSummary
	lastFetch       time.Time
	refreshInterval time.Duration
}

// NewCollector creates a new Snowflake collector
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

// fetchMetrics fetches all metrics from Snowflake
func (c *Collector) fetchMetrics(ctx context.Context) {
	log.Println("Snowflake: Fetching validation metrics...")
	
	// Get total record count
	totalRecords, err := c.client.GetTotalRecordCount(ctx)
	if err != nil {
		log.Printf("Snowflake: Error getting total count: %v", err)
		return
	}
	
	// Get today's record count
	todayRecords, err := c.client.GetTodayRecordCount(ctx)
	if err != nil {
		log.Printf("Snowflake: Error getting today count: %v", err)
	}
	
	// Get validation status breakdown
	statusBreakdown, err := c.client.GetValidationStatusCounts(ctx)
	if err != nil {
		log.Printf("Snowflake: Error getting status breakdown: %v", err)
	}
	
	// Get daily metrics for last 7 days
	dailyMetrics, err := c.client.GetDailyValidationCounts(ctx, 7)
	if err != nil {
		log.Printf("Snowflake: Error getting daily metrics: %v", err)
	}
	
	// Get domain group breakdown
	domainBreakdown, err := c.client.GetDomainGroupCounts(ctx)
	if err != nil {
		log.Printf("Snowflake: Error getting domain breakdown: %v", err)
	}
	
	// Get unique status count
	uniqueStatuses, err := c.client.GetUniqueStatusCount(ctx)
	if err != nil {
		log.Printf("Snowflake: Error getting unique status count: %v", err)
	}
	
	// Update stored metrics
	now := time.Now()
	c.mu.Lock()
	c.summary = &ValidationSummary{
		Timestamp:            now,
		TotalRecords:         totalRecords,
		TodayRecords:         todayRecords,
		UniqueStatuses:       uniqueStatuses,
		DailyMetrics:         dailyMetrics,
		StatusBreakdown:      statusBreakdown,
		DomainGroupBreakdown: domainBreakdown,
	}
	c.lastFetch = now
	c.mu.Unlock()
	
	log.Printf("Snowflake: Collected validation metrics - %d total, %d today, %d statuses",
		totalRecords, todayRecords, uniqueStatuses)
}

// GetSummary returns the current validation summary
func (c *Collector) GetSummary() *ValidationSummary {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.summary
}

// GetDailyMetrics returns daily validation metrics
func (c *Collector) GetDailyMetrics() []DailyValidationMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.summary == nil {
		return nil
	}
	return c.summary.DailyMetrics
}

// GetStatusBreakdown returns the validation status breakdown
func (c *Collector) GetStatusBreakdown() []ValidationStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.summary == nil {
		return nil
	}
	return c.summary.StatusBreakdown
}

// GetDomainGroupBreakdown returns the domain group breakdown
func (c *Collector) GetDomainGroupBreakdown() []DomainGroupMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.summary == nil {
		return nil
	}
	return c.summary.DomainGroupBreakdown
}

// LastFetch returns the time of the last successful fetch
func (c *Collector) LastFetch() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastFetch
}

// GetTodayRecords returns today's record count
func (c *Collector) GetTodayRecords() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.summary == nil {
		return 0
	}
	return c.summary.TodayRecords
}

// GetTotalRecords returns the total record count
func (c *Collector) GetTotalRecords() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.summary == nil {
		return 0
	}
	return c.summary.TotalRecords
}
