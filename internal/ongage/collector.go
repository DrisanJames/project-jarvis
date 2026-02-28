package ongage

import (
	"context"
	"log"
	"sync"
	"time"
)

// volumeCacheEntry caches a date-range volume query result
type volumeCacheEntry struct {
	total     int64
	fetchedAt time.Time
}

// dsCacheEntry caches a date-range per-DS volume map
type dsCacheEntry struct {
	data      map[string]int64
	fetchedAt time.Time
	exact     bool // true if from Contact Activity (24h TTL), false if from fallback (30min TTL)
}

// Collector handles periodic collection of Ongage metrics
type Collector struct {
	client          *Client
	metrics         *CollectorMetrics
	mu              sync.RWMutex
	fetchInterval   time.Duration
	lookbackDays    int
	stopChan        chan struct{}

	// List-level sending volume: data_set_code -> total sends
	// Built by combining GetLists() metadata with GetSendsByList() report data.
	listSendsByDS map[string]int64

	// Cache for date-range volume queries to avoid Ongage API rate limits.
	// Key is "from|to" date string, entries expire after 30 minutes.
	volumeCache   map[string]*volumeCacheEntry
	volumeCacheMu sync.RWMutex

	// Cache for date-range per-DS volume queries.
	dsCache   map[string]*dsCacheEntry
	dsCacheMu sync.RWMutex

	// Track in-flight Contact Activity report requests to avoid duplicates.
	caInFlight   map[string]bool
	caInFlightMu sync.Mutex

	// S3-backed persistence for Contact Activity volume data.
	// Survives server restarts so the 15-30 min report only runs once per day.
	s3Cache *S3VolumeCache
}

// NewCollector creates a new Ongage metrics collector
func NewCollector(client *Client, fetchInterval time.Duration, lookbackDays int) *Collector {
	return &Collector{
		client:        client,
		metrics:       &CollectorMetrics{},
		fetchInterval: fetchInterval,
		lookbackDays:  lookbackDays,
		stopChan:      make(chan struct{}),
		volumeCache:   make(map[string]*volumeCacheEntry),
		dsCache:       make(map[string]*dsCacheEntry),
		caInFlight:    make(map[string]bool),
	}
}

// SetS3Cache configures S3-backed persistence for Contact Activity volume data.
// When set, exact volume results survive server restarts without re-running the
// 15-30 minute Ongage report.
func (c *Collector) SetS3Cache(sc *S3VolumeCache) {
	c.s3Cache = sc
}

// Start begins the periodic metrics collection
func (c *Collector) Start() {
	log.Println("Starting Ongage metrics collector...")
	go c.collectLoop()
}

// Stop halts the metrics collection
func (c *Collector) Stop() {
	close(c.stopChan)
}

// collectLoop runs the periodic collection
func (c *Collector) collectLoop() {
	// Initial fetch
	c.fetchAll()

	ticker := time.NewTicker(c.fetchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.fetchAll()
		case <-c.stopChan:
			log.Println("Ongage collector stopped")
			return
		}
	}
}

// fetchAll collects all Ongage metrics
func (c *Collector) fetchAll() {
	log.Println("Fetching Ongage metrics...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	startTime := time.Now()
	successCount := 0
	totalTasks := 7

	// Fetch ESP connections first (needed for campaign processing)
	espConnections, err := c.client.GetESPConnections(ctx, true)
	if err != nil {
		log.Printf("Ongage: Failed to fetch ESP connections: %v", err)
	} else {
		successCount++
		log.Printf("Ongage: Got %d ESP connections", len(espConnections))
	}

	// Build ESP connection map for lookups
	espMap := make(map[string]ESPConnection)
	for _, esp := range espConnections {
		espMap[esp.ID] = esp
	}

	// Fetch campaign stats
	var campaigns []ProcessedCampaign
	campaignStats, err := c.client.GetCampaignStats(ctx, c.lookbackDays)
	if err != nil {
		log.Printf("Ongage: Failed to fetch campaign stats: %v", err)
	} else {
		successCount++
		campaigns = c.processCampaignStats(campaignStats, espMap)
		log.Printf("Ongage: Got stats for %d campaigns", len(campaigns))
	}

	// Fetch ESP performance
	var espPerformance []ESPPerformance
	espStats, err := c.client.GetCampaignStatsByESP(ctx, c.lookbackDays)
	if err != nil {
		log.Printf("Ongage: Failed to fetch ESP stats: %v", err)
	} else {
		successCount++
		espPerformance = c.processESPStats(espStats)
		log.Printf("Ongage: Got performance for %d ESP connections", len(espPerformance))
	}

	// Fetch schedule analysis
	var scheduleAnalysis []ScheduleAnalysis
	scheduleStats, err := c.client.GetCampaignStatsByHour(ctx, c.lookbackDays)
	if err != nil {
		log.Printf("Ongage: Failed to fetch schedule stats: %v", err)
	} else {
		successCount++
		scheduleAnalysis = c.processScheduleStats(scheduleStats)
		log.Printf("Ongage: Got schedule analysis for %d time slots", len(scheduleAnalysis))
	}

	// Fetch audience analysis
	var audienceAnalysis []AudienceAnalysis
	segmentStats, err := c.client.GetCampaignStatsBySegment(ctx, c.lookbackDays)
	if err != nil {
		log.Printf("Ongage: Failed to fetch segment stats: %v", err)
	} else {
		successCount++
		audienceAnalysis = c.processAudienceStats(segmentStats)
		log.Printf("Ongage: Got audience analysis for %d segments", len(audienceAnalysis))
	}

	// Fetch daily pipeline stats
	var pipelineMetrics []PipelineMetrics
	dailyStats, err := c.client.GetDailyStats(ctx, c.lookbackDays)
	if err != nil {
		log.Printf("Ongage: Failed to fetch daily stats: %v", err)
	} else {
		successCount++
		pipelineMetrics = c.processDailyStats(dailyStats)
		log.Printf("Ongage: Got pipeline metrics for %d days", len(pipelineMetrics))
	}

	// Fetch list metadata + sends-by-list for data partner volume attribution.
	// Strategy: try list-level sends first, then fall back to segment-level sends.
	var listSendsByDS map[string]int64
	lists, err := c.client.GetLists(ctx)
	if err != nil {
		log.Printf("Ongage: Failed to fetch list metadata: %v", err)
	} else {
		log.Printf("Ongage: Got %d lists", len(lists))
		sendRows, err := c.client.GetSendsByList(ctx, c.lookbackDays)
		if err != nil {
			log.Printf("Ongage: Failed to fetch sends-by-list: %v", err)
		} else {
			listSendsByDS = c.buildListSendsByDataSetCode(lists, sendRows)
			log.Printf("Ongage: Built list-sends map with %d data set codes", len(listSendsByDS))
			for dsCode, sends := range listSendsByDS {
				log.Printf("  List DS=%s -> %d sends", dsCode, sends)
			}
		}
	}

	// If list-level approach didn't produce useful per-data-partner results,
	// try segment-level sends. Segment names may contain data set codes or
	// data partner prefixes.
	segmentSendsByDS, err := c.fetchSegmentSends(ctx)
	if err != nil {
		log.Printf("Ongage: Failed to fetch sends-by-segment: %v", err)
	} else if len(segmentSendsByDS) > 0 {
		successCount++
		log.Printf("Ongage: Built segment-sends map with %d data set codes", len(segmentSendsByDS))
		for dsCode, sends := range segmentSendsByDS {
			log.Printf("  Segment DS=%s -> %d sends", dsCode, sends)
		}
		// Merge segment results into listSendsByDS (segment data supplements list data)
		if listSendsByDS == nil {
			listSendsByDS = segmentSendsByDS
		} else {
			// Only add segment-derived entries that don't already exist in list data
			for dsCode, sends := range segmentSendsByDS {
				if _, exists := listSendsByDS[dsCode]; !exists {
					listSendsByDS[dsCode] = sends
				}
			}
		}
	} else {
		successCount++ // still count as success even if no matching segments
	}

	// Analyze subject lines
	subjectAnalysis := c.analyzeSubjectLines(campaigns)
	log.Printf("Ongage: Analyzed %d unique subject lines", len(subjectAnalysis))

	// Enrich audience analysis with campaign counts from campaign data
	audienceAnalysis = c.enrichAudienceWithCampaignCounts(audienceAnalysis, campaigns)

	// Count active campaigns
	activeCampaigns := 0
	for _, camp := range campaigns {
		if camp.Status == StatusInProgress || camp.Status == StatusScheduled {
			activeCampaigns++
		}
	}

	// Update metrics
	c.mu.Lock()
	c.metrics = &CollectorMetrics{
		Campaigns:        campaigns,
		ESPConnections:   espConnections,
		SubjectAnalysis:  subjectAnalysis,
		ScheduleAnalysis: scheduleAnalysis,
		ESPPerformance:   espPerformance,
		AudienceAnalysis: audienceAnalysis,
		PipelineMetrics:  pipelineMetrics,
		LastFetch:        time.Now(),
		TotalCampaigns:   len(campaigns),
		ActiveCampaigns:  activeCampaigns,
	}
	if listSendsByDS != nil {
		c.listSendsByDS = listSendsByDS
	}
	c.mu.Unlock()

	elapsed := time.Since(startTime)
	log.Printf("Ongage metrics fetch completed in %s (%d/%d successful)", elapsed, successCount, totalTasks)
}

// GetLatestMetrics returns the latest collected metrics
func (c *Collector) GetLatestMetrics() *CollectorMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.metrics
}

// GetCampaigns returns the latest campaigns
func (c *Collector) GetCampaigns() []ProcessedCampaign {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.metrics == nil {
		return nil
	}
	return c.metrics.Campaigns
}

// GetSubjectAnalysis returns subject line analysis
func (c *Collector) GetSubjectAnalysis() []SubjectLineAnalysis {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.metrics == nil {
		return nil
	}
	return c.metrics.SubjectAnalysis
}

// GetScheduleAnalysis returns schedule optimization analysis
func (c *Collector) GetScheduleAnalysis() []ScheduleAnalysis {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.metrics == nil {
		return nil
	}
	return c.metrics.ScheduleAnalysis
}

// GetESPPerformance returns ESP performance metrics
func (c *Collector) GetESPPerformance() []ESPPerformance {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.metrics == nil {
		return nil
	}
	return c.metrics.ESPPerformance
}

// GetAudienceAnalysis returns audience/segment analysis
func (c *Collector) GetAudienceAnalysis() []AudienceAnalysis {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.metrics == nil {
		return nil
	}
	return c.metrics.AudienceAnalysis
}

// GetPipelineMetrics returns daily pipeline metrics
func (c *Collector) GetPipelineMetrics() []PipelineMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.metrics == nil {
		return nil
	}
	return c.metrics.PipelineMetrics
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
