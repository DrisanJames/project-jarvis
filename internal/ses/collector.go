package ses

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
)

// StorageInterface defines the storage operations needed by the collector
type StorageInterface interface {
	SaveSESMetrics(ctx context.Context, metrics []ProcessedMetrics) error
	SaveSESISPMetrics(ctx context.Context, metrics []ISPMetrics) error
	SaveSESSignals(ctx context.Context, signals SignalsData) error
}

// AgentInterface defines the agent operations needed by the collector
type AgentInterface interface {
	ProcessSESMetrics(ctx context.Context, metrics []ProcessedMetrics) error
	ProcessSESISPMetrics(ctx context.Context, metrics []ISPMetrics) error
	EvaluateSESHealth(metrics ProcessedMetrics) (status string, reason string)
}

// Collector handles fetching metrics from AWS SES
type Collector struct {
	client  *Client
	storage StorageInterface
	agent   AgentInterface
	config  config.PollingConfig

	mu            sync.RWMutex
	latestSummary *Summary
	latestISP     []ISPMetrics
	latestSignals *SignalsData
	lastFetch     time.Time
	isRunning     bool
}

// NewCollector creates a new SES metrics collector
func NewCollector(client *Client, storage StorageInterface, agent AgentInterface, cfg config.PollingConfig) *Collector {
	return &Collector{
		client:  client,
		storage: storage,
		agent:   agent,
		config:  cfg,
	}
}

// Start begins the polling loop
func (c *Collector) Start(ctx context.Context) {
	c.mu.Lock()
	c.isRunning = true
	c.mu.Unlock()

	log.Println("Starting SES metrics collector...")

	// Initial fetch
	c.fetchAll(ctx)

	// Set up ticker for periodic fetching
	ticker := time.NewTicker(c.config.Interval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Stopping SES metrics collector...")
			c.mu.Lock()
			c.isRunning = false
			c.mu.Unlock()
			return
		case <-ticker.C:
			c.fetchAll(ctx)
		}
	}
}

// fetchAll fetches all metrics concurrently using goroutines
func (c *Collector) fetchAll(ctx context.Context) {
	log.Println("Fetching SES metrics...")
	startTime := time.Now()

	// Create a wait group for concurrent fetches
	var wg sync.WaitGroup

	// Channel for results
	type fetchResult struct {
		name string
		err  error
	}
	results := make(chan fetchResult, 3)

	// Define time range for queries (yesterday midnight to today midnight UTC)
	// AWS SES BatchGetMetricData requires midnight-to-midnight UTC timestamps
	now := time.Now().UTC()
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	yesterdayMidnight := todayMidnight.Add(-24 * time.Hour)
	from := yesterdayMidnight
	to := todayMidnight

	// Fetch ISP metrics (this is the primary data source)
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := c.fetchISPMetrics(ctx, from, to)
		results <- fetchResult{name: "isp", err: err}
	}()

	// Fetch signals
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := c.fetchSignals(ctx, from, to)
		results <- fetchResult{name: "signals", err: err}
	}()

	// Wait for all fetches to complete
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results
	successCount := 0
	for result := range results {
		if result.err != nil {
			log.Printf("Error fetching SES %s metrics: %v", result.name, result.err)
		} else {
			successCount++
		}
	}

	c.mu.Lock()
	c.lastFetch = time.Now()
	c.mu.Unlock()

	log.Printf("SES metrics fetch completed in %v (%d/2 successful)", time.Since(startTime), successCount)
}

// FetchNow triggers an immediate fetch of all metrics
func (c *Collector) FetchNow(ctx context.Context) {
	c.fetchAll(ctx)
}

// fetchISPMetrics fetches metrics by ISP and creates summary
func (c *Collector) fetchISPMetrics(ctx context.Context, from, to time.Time) error {
	ispMetrics, err := c.client.GetAllISPMetrics(ctx, from, to)
	if err != nil {
		return err
	}

	// Evaluate health status for each ISP
	for i := range ispMetrics {
		if c.agent != nil {
			status, reason := c.agent.EvaluateSESHealth(ispMetrics[i].Metrics)
			ispMetrics[i].Status = status
			ispMetrics[i].StatusReason = reason
		}
	}

	// Create summary from ISP metrics
	summary := AggregateISPMetricsToSummary(ispMetrics, from, to)

	c.mu.Lock()
	c.latestISP = ispMetrics
	c.latestSummary = summary
	c.lastFetch = time.Now()
	c.mu.Unlock()

	log.Printf("SES: Stored %d ISP metrics, summary total=%d", len(ispMetrics), summary.TotalTargeted)

	// Store and process
	if c.storage != nil {
		if err := c.storage.SaveSESISPMetrics(ctx, ispMetrics); err != nil {
			log.Printf("Error saving SES ISP metrics: %v", err)
		}
	}

	if c.agent != nil {
		if err := c.agent.ProcessSESISPMetrics(ctx, ispMetrics); err != nil {
			log.Printf("Error processing SES ISP metrics: %v", err)
		}
	}

	// Convert ISP metrics to ProcessedMetrics for storage
	if c.storage != nil && len(ispMetrics) > 0 {
		processedMetrics := make([]ProcessedMetrics, len(ispMetrics))
		for i, isp := range ispMetrics {
			processedMetrics[i] = isp.Metrics
		}
		if err := c.storage.SaveSESMetrics(ctx, processedMetrics); err != nil {
			log.Printf("Error saving SES processed metrics: %v", err)
		}
	}

	if c.agent != nil && len(ispMetrics) > 0 {
		processedMetrics := make([]ProcessedMetrics, len(ispMetrics))
		for i, isp := range ispMetrics {
			processedMetrics[i] = isp.Metrics
		}
		if err := c.agent.ProcessSESMetrics(ctx, processedMetrics); err != nil {
			log.Printf("Error processing SES metrics: %v", err)
		}
	}

	return nil
}

// fetchSignals fetches deliverability signals
func (c *Collector) fetchSignals(ctx context.Context, from, to time.Time) error {
	signals, err := c.client.GetSignals(ctx, from, to)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.latestSignals = signals
	c.mu.Unlock()

	// Store signals
	if c.storage != nil && signals != nil {
		if err := c.storage.SaveSESSignals(ctx, *signals); err != nil {
			log.Printf("Error saving SES signals: %v", err)
		}
	}

	return nil
}

// GetLatestSummary returns the most recent summary
func (c *Collector) GetLatestSummary() *Summary {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latestSummary
}

// GetLatestISPMetrics returns the most recent ISP metrics
func (c *Collector) GetLatestISPMetrics() []ISPMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latestISP
}

// GetLatestSignals returns the most recent signals data
func (c *Collector) GetLatestSignals() *SignalsData {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latestSignals
}

// GetLastFetchTime returns the time of the last successful fetch
func (c *Collector) GetLastFetchTime() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastFetch
}

// IsRunning returns whether the collector is currently running
func (c *Collector) IsRunning() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isRunning
}

// GetClient returns the underlying SES client
func (c *Collector) GetClient() *Client {
	return c.client
}
