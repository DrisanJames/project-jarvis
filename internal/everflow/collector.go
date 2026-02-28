package everflow

import (
	"context"
	"log"
	"sync"
	"time"
)

// sub2CacheEntry caches a date-range-specific sub2 entity report result.
type sub2CacheEntry struct {
	report    *EntityReportResponse
	fetchedAt time.Time
}

// Collector handles periodic collection of Everflow metrics
type Collector struct {
	client           *Client
	metrics          *CollectorMetrics
	mu               sync.RWMutex
	fetchInterval    time.Duration
	lookbackDays     int
	stopChan         chan struct{}
	campaignEnricher *CampaignEnricher
	costCalculator   *CostCalculator
	
	// Historical data cache
	dailyMetrics   map[string]*DailyPerformance // key: date string
	allClicks      []Click
	allConversions []Conversion

	// Data partner: sub2 entity report for click counts per data set
	sub2EntityReport *EntityReportResponse

	// Data partner: offer × sub2 cross-tab entity report for CPM attribution
	// Each row represents a unique (offer, sub2) pair with click/revenue data.
	offerSub2EntityReport *EntityReportResponse

	// Data partner analytics cache
	dataPartnerCache     *DataPartnerAnalyticsResponse
	dataPartnerCacheTime time.Time

	// Volume provider — returns (dataSetCode -> recordCount) for injection volume
	volumeProvider func() map[string]int64

	// Total sends provider — returns the best estimate of total email volume
	// from Ongage for a specific date range (makes fresh API calls).
	totalSendsForDateRange func(ctx context.Context, from, to time.Time) int64

	// Date-range-aware sub2 entity report provider (for click counts).
	// Returns a fresh entity report for the requested date window.
	sub2ReportForDateRange func(ctx context.Context, from, to time.Time) *EntityReportResponse

	// Cache for date-range sub2 entity reports to avoid repeated Everflow API calls.
	sub2ReportCache   map[string]*sub2CacheEntry
	sub2ReportCacheMu sync.RWMutex

	// Date-range-aware volume provider (per-data-set-code volume for a specific window).
	volumeProviderForDateRange func(ctx context.Context, from, to time.Time) map[string]int64

	// Cache for date-range offer×sub2 entity reports (for CPM attribution).
	offerSub2ReportCache   map[string]*sub2CacheEntry
	offerSub2ReportCacheMu sync.RWMutex

	// Cache for date-range conversion fetches. The conversions endpoint is NOT
	// a BigQuery query, so it won't hit BQ rate limits. This ensures CPA
	// revenue is accurate even when the requested date range extends beyond
	// the lookback period covered by allConversions.
	conversionCache   map[string]*conversionCacheEntry
	conversionCacheMu sync.RWMutex
}

type conversionCacheEntry struct {
	conversions []Conversion
	fetchedAt   time.Time
}

// NewCollector creates a new Everflow metrics collector
func NewCollector(client *Client, fetchInterval time.Duration, lookbackDays int) *Collector {
	return &Collector{
		client:               client,
		metrics:              &CollectorMetrics{},
		fetchInterval:        fetchInterval,
		lookbackDays:         lookbackDays,
		stopChan:             make(chan struct{}),
		dailyMetrics:         make(map[string]*DailyPerformance),
		sub2ReportCache:      make(map[string]*sub2CacheEntry),
		offerSub2ReportCache: make(map[string]*sub2CacheEntry),
		conversionCache:      make(map[string]*conversionCacheEntry),
	}
}

// SetCampaignEnricher sets the campaign enricher for Ongage data
func (c *Collector) SetCampaignEnricher(enricher *CampaignEnricher) {
	c.campaignEnricher = enricher
}

// SetVolumeProvider sets a function that returns data-set-code → sending volume.
// The primary source is Ongage list-level sends (list_id -> sum(sent), resolved
// via list name -> data_set_code). Falls back to proportional estimation if nil.
func (c *Collector) SetVolumeProvider(fn func() map[string]int64) {
	c.volumeProvider = fn
}

// SetTotalSendsForDateRange sets a function that queries Ongage for the total
// email volume within a specific date range. This ensures volume matches the
// global date filter instead of using cached periodic data.
func (c *Collector) SetTotalSendsForDateRange(fn func(ctx context.Context, from, to time.Time) int64) {
	c.totalSendsForDateRange = fn
}

// SetSub2ReportForDateRange sets a function that fetches a fresh sub2 entity
// report from Everflow for a specific date range. This ensures click counts
// in Data Partner Analytics match the global date filter.
func (c *Collector) SetSub2ReportForDateRange(fn func(ctx context.Context, from, to time.Time) *EntityReportResponse) {
	c.sub2ReportForDateRange = fn
}

// SetVolumeProviderForDateRange sets a date-range-aware per-data-set-code volume
// provider. Falls back to the cached volumeProvider if this is nil.
func (c *Collector) SetVolumeProviderForDateRange(fn func(ctx context.Context, from, to time.Time) map[string]int64) {
	c.volumeProviderForDateRange = fn
}

// SetCostCalculator sets the cost calculator for ESP cost analysis
func (c *Collector) SetCostCalculator(calculator *CostCalculator) {
	c.costCalculator = calculator
}

// GetClient returns the underlying Everflow API client
func (c *Collector) GetClient() *Client {
	return c.client
}

// GetLoadedContracts returns the loaded ESP contracts for debugging
func (c *Collector) GetLoadedContracts() []ESPContractInfo {
	if c.costCalculator == nil {
		return nil
	}
	return c.costCalculator.GetAllContracts()
}

// Start begins the periodic metrics collection
func (c *Collector) Start() {
	log.Println("Starting Everflow metrics collector...")
	go c.collectLoop()
}

// Stop halts the metrics collection
func (c *Collector) Stop() {
	close(c.stopChan)
}
