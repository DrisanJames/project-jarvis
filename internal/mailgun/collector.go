package mailgun

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
)

// StorageInterface defines the storage operations needed by the collector
type StorageInterface interface {
	SaveMailgunMetrics(ctx context.Context, metrics []ProcessedMetrics) error
	SaveMailgunISPMetrics(ctx context.Context, metrics []ISPMetrics) error
	SaveMailgunDomainMetrics(ctx context.Context, metrics []DomainMetrics) error
	SaveMailgunSignals(ctx context.Context, signals SignalsData) error
}

// AgentInterface defines the agent operations needed by the collector
type AgentInterface interface {
	ProcessMailgunMetrics(ctx context.Context, metrics []ProcessedMetrics) error
	ProcessMailgunISPMetrics(ctx context.Context, metrics []ISPMetrics) error
	EvaluateMailgunHealth(metrics ProcessedMetrics) (status string, reason string)
}

// Collector handles fetching metrics from Mailgun
type Collector struct {
	client  *Client
	storage StorageInterface
	agent   AgentInterface
	config  config.PollingConfig

	mu            sync.RWMutex
	latestSummary *Summary
	latestISP     []ISPMetrics
	latestDomain  []DomainMetrics
	latestSignals *SignalsData
	lastFetch     time.Time
	isRunning     bool
}

// NewCollector creates a new Mailgun metrics collector
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

	log.Println("Starting Mailgun metrics collector...")

	// Initial fetch
	c.fetchAll(ctx)

	// Set up ticker for periodic fetching
	ticker := time.NewTicker(c.config.Interval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Stopping Mailgun metrics collector...")
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
	log.Println("Fetching Mailgun metrics...")
	startTime := time.Now()

	// Create a wait group for concurrent fetches
	var wg sync.WaitGroup

	// Channel for results
	type fetchResult struct {
		name string
		err  error
	}
	results := make(chan fetchResult, 4)

	// Define time range for queries
	now := time.Now().UTC()
	from := now.Add(-24 * time.Hour)
	to := now

	query := MetricsQuery{
		From:       from,
		To:         to,
		Resolution: "hour",
		Domains:    c.client.GetDomains(),
	}

	// Fetch summary metrics
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := c.fetchSummary(ctx, query)
		results <- fetchResult{name: "summary", err: err}
	}()

	// Fetch ISP/Provider metrics
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := c.fetchISPMetrics(ctx, from, to)
		results <- fetchResult{name: "isp", err: err}
	}()

	// Fetch domain metrics
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := c.fetchDomainMetrics(ctx, query)
		results <- fetchResult{name: "domain", err: err}
	}()

	// Fetch bounce signals
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
			log.Printf("Error fetching Mailgun %s metrics: %v", result.name, result.err)
		} else {
			successCount++
		}
	}

	c.mu.Lock()
	c.lastFetch = time.Now()
	c.mu.Unlock()

	log.Printf("Mailgun metrics fetch completed in %v (%d/4 successful)", time.Since(startTime), successCount)
}

// fetchSummary fetches overall summary metrics
func (c *Collector) fetchSummary(ctx context.Context, query MetricsQuery) error {
	pm, err := c.client.GetMetricsSummary(ctx, query)
	if err != nil {
		log.Printf("Mailgun fetchSummary error: %v", err)
		return err
	}

	if pm == nil {
		log.Println("Mailgun fetchSummary: no data returned")
		return nil
	}

	summary := &Summary{
		Timestamp:         time.Now(),
		PeriodStart:       query.From,
		PeriodEnd:         query.To,
		TotalTargeted:     pm.Targeted,
		TotalDelivered:    pm.Delivered,
		TotalOpened:       pm.UniqueOpened,
		TotalClicked:      pm.UniqueClicked,
		TotalBounced:      pm.Bounced,
		TotalComplaints:   pm.Complaints,
		TotalUnsubscribes: pm.Unsubscribes,
		DeliveryRate:      pm.DeliveryRate,
		OpenRate:          pm.OpenRate,
		ClickRate:         pm.ClickRate,
		BounceRate:        pm.BounceRate,
		ComplaintRate:     pm.ComplaintRate,
		UnsubscribeRate:   pm.UnsubscribeRate,
	}

	c.mu.Lock()
	c.latestSummary = summary
	c.mu.Unlock()

	// Store and process
	if c.storage != nil {
		if err := c.storage.SaveMailgunMetrics(ctx, []ProcessedMetrics{*pm}); err != nil {
			log.Printf("Error saving Mailgun summary metrics: %v", err)
		}
	}

	if c.agent != nil {
		if err := c.agent.ProcessMailgunMetrics(ctx, []ProcessedMetrics{*pm}); err != nil {
			log.Printf("Error processing Mailgun summary metrics: %v", err)
		}
	}

	return nil
}

// fetchISPMetrics fetches metrics by provider/ISP
func (c *Collector) fetchISPMetrics(ctx context.Context, from, to time.Time) error {
	ispMetrics, err := c.client.GetMetricsByProvider(ctx, from, to)
	if err != nil {
		return err
	}

	// Evaluate health status for each ISP (may override the status set by client)
	for i := range ispMetrics {
		if c.agent != nil {
			status, reason := c.agent.EvaluateMailgunHealth(ispMetrics[i].Metrics)
			ispMetrics[i].Status = status
			ispMetrics[i].StatusReason = reason
		}
	}

	c.mu.Lock()
	c.latestISP = ispMetrics
	c.mu.Unlock()

	// Store and process
	if c.storage != nil {
		if err := c.storage.SaveMailgunISPMetrics(ctx, ispMetrics); err != nil {
			log.Printf("Error saving Mailgun ISP metrics: %v", err)
		}
	}

	if c.agent != nil {
		if err := c.agent.ProcessMailgunISPMetrics(ctx, ispMetrics); err != nil {
			log.Printf("Error processing Mailgun ISP metrics: %v", err)
		}
	}

	return nil
}

// fetchDomainMetrics fetches metrics by sending domain
func (c *Collector) fetchDomainMetrics(ctx context.Context, query MetricsQuery) error {
	domainMetrics, err := c.client.GetMetricsByDomain(ctx, query)
	if err != nil {
		return err
	}

	// Evaluate health status for each domain
	for i := range domainMetrics {
		if c.agent != nil {
			status, reason := c.agent.EvaluateMailgunHealth(domainMetrics[i].Metrics)
			domainMetrics[i].Status = status
			domainMetrics[i].StatusReason = reason
		}
	}

	c.mu.Lock()
	c.latestDomain = domainMetrics
	c.mu.Unlock()

	// Store
	if c.storage != nil {
		if err := c.storage.SaveMailgunDomainMetrics(ctx, domainMetrics); err != nil {
			log.Printf("Error saving Mailgun domain metrics: %v", err)
		}
	}

	return nil
}

// fetchSignals fetches bounce signals
func (c *Collector) fetchSignals(ctx context.Context, from, to time.Time) error {
	signals, err := c.client.GetBounceReasons(ctx, from, to)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.latestSignals = signals
	c.mu.Unlock()

	// Store
	if c.storage != nil {
		if err := c.storage.SaveMailgunSignals(ctx, *signals); err != nil {
			log.Printf("Error saving Mailgun signals: %v", err)
		}
	}

	return nil
}

// GetLatestSummary returns the latest summary metrics
func (c *Collector) GetLatestSummary() *Summary {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latestSummary
}

// GetLatestISPMetrics returns the latest ISP metrics
func (c *Collector) GetLatestISPMetrics() []ISPMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latestISP
}

// GetLatestDomainMetrics returns the latest domain metrics
func (c *Collector) GetLatestDomainMetrics() []DomainMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latestDomain
}

// GetLatestSignals returns the latest signals data
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

// IsRunning returns whether the collector is running
func (c *Collector) IsRunning() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isRunning
}

// FetchNow triggers an immediate fetch
func (c *Collector) FetchNow(ctx context.Context) {
	c.fetchAll(ctx)
}

// GetClient returns the underlying Mailgun client
func (c *Collector) GetClient() *Client {
	return c.client
}
