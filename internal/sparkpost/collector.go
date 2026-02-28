package sparkpost

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
)

// StorageInterface defines the storage operations needed by the collector
type StorageInterface interface {
	SaveMetrics(ctx context.Context, metrics []ProcessedMetrics) error
	SaveISPMetrics(ctx context.Context, metrics []ISPMetrics) error
	SaveIPMetrics(ctx context.Context, metrics []IPMetrics) error
	SaveDomainMetrics(ctx context.Context, metrics []DomainMetrics) error
	SaveSignals(ctx context.Context, signals SignalsData) error
	SaveTimeSeries(ctx context.Context, series []TimeSeries) error
}

// AgentInterface defines the agent operations needed by the collector
type AgentInterface interface {
	ProcessMetrics(ctx context.Context, metrics []ProcessedMetrics) error
	ProcessISPMetrics(ctx context.Context, metrics []ISPMetrics) error
	EvaluateHealth(metrics ProcessedMetrics) (status string, reason string)
}

// Collector handles fetching metrics from SparkPost
type Collector struct {
	client   *Client
	storage  StorageInterface
	agent    AgentInterface
	config   config.PollingConfig
	
	mu                    sync.RWMutex
	latestSummary         *Summary
	latestISP             []ISPMetrics
	latestIP              []IPMetrics
	latestDomain          []DomainMetrics
	latestRecipientDomain []RecipientDomainMetrics // For Yahoo family breakout
	latestSignals         *SignalsData
	lastFetch             time.Time
	isRunning             bool
}

// NewCollector creates a new metrics collector
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

	log.Println("Starting metrics collector...")

	// Initial fetch
	c.fetchAll(ctx)

	// Set up ticker for periodic fetching
	ticker := time.NewTicker(c.config.Interval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Stopping metrics collector...")
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
	log.Println("Fetching metrics...")
	startTime := time.Now()

	// Create a wait group for concurrent fetches
	var wg sync.WaitGroup
	
	// Channels for results
	type fetchResult struct {
		name  string
		err   error
	}
	results := make(chan fetchResult, 6)

	// Define time range for queries
	now := time.Now().UTC()
	from := now.Add(-24 * time.Hour)
	to := now

	query := MetricsQuery{
		From:      from,
		To:        to,
		Precision: "hour",
		Limit:     100,
		OrderBy:   "count_targeted",
	}

	// Fetch summary metrics
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := c.fetchSummary(ctx, query)
		results <- fetchResult{name: "summary", err: err}
	}()

	// Fetch ISP metrics
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := c.fetchISPMetrics(ctx, query)
		results <- fetchResult{name: "isp", err: err}
	}()

	// Fetch IP metrics
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := c.fetchIPMetrics(ctx, query)
		results <- fetchResult{name: "ip", err: err}
	}()

	// Fetch sending domain metrics
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := c.fetchDomainMetrics(ctx, query)
		results <- fetchResult{name: "domain", err: err}
	}()

	// Fetch recipient domain metrics (Yahoo family breakout)
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := c.fetchRecipientDomainMetrics(ctx, query)
		results <- fetchResult{name: "recipient_domain", err: err}
	}()

	// Fetch signals (bounce/delay reasons)
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := c.fetchSignals(ctx, query)
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
			log.Printf("Error fetching %s metrics: %v", result.name, result.err)
		} else {
			successCount++
		}
	}

	c.mu.Lock()
	c.lastFetch = time.Now()
	c.mu.Unlock()

	log.Printf("Metrics fetch completed in %v (%d/5 successful)", time.Since(startTime), successCount)
}

// fetchSummary fetches overall summary metrics
func (c *Collector) fetchSummary(ctx context.Context, query MetricsQuery) error {
	resp, err := c.client.GetMetricsSummary(ctx, query)
	if err != nil {
		return err
	}

	if len(resp.Results) == 0 {
		return nil
	}

	result := resp.Results[0]
	pm := ConvertToProcessedMetrics(result, "summary", "all")

	summary := &Summary{
		Timestamp:       time.Now(),
		PeriodStart:     query.From,
		PeriodEnd:       query.To,
		TotalTargeted:   pm.Targeted,
		TotalDelivered:  pm.Delivered,
		TotalOpened:     pm.UniqueOpened,
		TotalClicked:    pm.UniqueClicked,
		TotalBounced:    pm.Bounced,
		TotalComplaints: pm.Complaints,
		TotalUnsubscribes: pm.Unsubscribes,
		DeliveryRate:    pm.DeliveryRate,
		OpenRate:        pm.OpenRate,
		ClickRate:       pm.ClickRate,
		BounceRate:      pm.BounceRate,
		ComplaintRate:   pm.ComplaintRate,
		UnsubscribeRate: pm.UnsubscribeRate,
	}

	c.mu.Lock()
	c.latestSummary = summary
	c.mu.Unlock()

	// Store and process
	if c.storage != nil {
		if err := c.storage.SaveMetrics(ctx, []ProcessedMetrics{pm}); err != nil {
			log.Printf("Error saving summary metrics: %v", err)
		}
	}

	if c.agent != nil {
		if err := c.agent.ProcessMetrics(ctx, []ProcessedMetrics{pm}); err != nil {
			log.Printf("Error processing summary metrics: %v", err)
		}
	}

	return nil
}

// fetchISPMetrics fetches metrics by mailbox provider
func (c *Collector) fetchISPMetrics(ctx context.Context, query MetricsQuery) error {
	resp, err := c.client.GetMetricsByMailboxProvider(ctx, query)
	if err != nil {
		return err
	}

	var ispMetrics []ISPMetrics
	for _, result := range resp.Results {
		pm := ConvertToProcessedMetrics(result, "mailbox_provider", result.MailboxProvider)
		
		status := "healthy"
		statusReason := ""
		if c.agent != nil {
			status, statusReason = c.agent.EvaluateHealth(pm)
		}

		ispMetrics = append(ispMetrics, ISPMetrics{
			Provider:     result.MailboxProvider,
			Metrics:      pm,
			Status:       status,
			StatusReason: statusReason,
		})
	}

	c.mu.Lock()
	c.latestISP = ispMetrics
	c.mu.Unlock()

	// Store and process
	if c.storage != nil {
		if err := c.storage.SaveISPMetrics(ctx, ispMetrics); err != nil {
			log.Printf("Error saving ISP metrics: %v", err)
		}
	}

	if c.agent != nil {
		if err := c.agent.ProcessISPMetrics(ctx, ispMetrics); err != nil {
			log.Printf("Error processing ISP metrics: %v", err)
		}
	}

	return nil
}

// fetchIPMetrics fetches metrics by sending IP
func (c *Collector) fetchIPMetrics(ctx context.Context, query MetricsQuery) error {
	resp, err := c.client.GetMetricsBySendingIP(ctx, query)
	if err != nil {
		return err
	}

	// Also fetch IP pools to associate IPs with pools
	poolResp, _ := c.client.GetMetricsByIPPool(ctx, query)
	ipToPool := make(map[string]string)
	if poolResp != nil {
		for _, result := range poolResp.Results {
			// Note: We'd need additional logic to map IPs to pools
			// For now, we'll leave this as a placeholder
			_ = result
		}
	}

	var ipMetrics []IPMetrics
	for _, result := range resp.Results {
		pm := ConvertToProcessedMetrics(result, "sending_ip", result.SendingIP)
		
		status := "healthy"
		statusReason := ""
		if c.agent != nil {
			status, statusReason = c.agent.EvaluateHealth(pm)
		}

		pool := ipToPool[result.SendingIP]
		if pool == "" {
			pool = "default"
		}

		ipMetrics = append(ipMetrics, IPMetrics{
			IP:           result.SendingIP,
			Pool:         pool,
			Metrics:      pm,
			Status:       status,
			StatusReason: statusReason,
		})
	}

	c.mu.Lock()
	c.latestIP = ipMetrics
	c.mu.Unlock()

	if c.storage != nil {
		if err := c.storage.SaveIPMetrics(ctx, ipMetrics); err != nil {
			log.Printf("Error saving IP metrics: %v", err)
		}
	}

	return nil
}

// fetchDomainMetrics fetches metrics by sending domain
func (c *Collector) fetchDomainMetrics(ctx context.Context, query MetricsQuery) error {
	resp, err := c.client.GetMetricsBySendingDomain(ctx, query)
	if err != nil {
		return err
	}

	var domainMetrics []DomainMetrics
	for _, result := range resp.Results {
		pm := ConvertToProcessedMetrics(result, "sending_domain", result.SendingDomain)
		
		status := "healthy"
		statusReason := ""
		if c.agent != nil {
			status, statusReason = c.agent.EvaluateHealth(pm)
		}

		domainMetrics = append(domainMetrics, DomainMetrics{
			Domain:       result.SendingDomain,
			Metrics:      pm,
			Status:       status,
			StatusReason: statusReason,
		})
	}

	c.mu.Lock()
	c.latestDomain = domainMetrics
	c.mu.Unlock()

	if c.storage != nil {
		if err := c.storage.SaveDomainMetrics(ctx, domainMetrics); err != nil {
			log.Printf("Error saving domain metrics: %v", err)
		}
	}

	return nil
}

// YahooFamilyDomains are the recipient domains we want to break out from the Yahoo mailbox provider
var YahooFamilyDomains = map[string]string{
	"att.net":       "AT&T",
	"sbcglobal.net": "SBCGlobal",
	"bellsouth.net": "BellSouth",
	"yahoo.com":     "Yahoo",
	"aol.com":       "AOL",
}

// fetchRecipientDomainMetrics fetches metrics for specific recipient domains (Yahoo family breakout)
func (c *Collector) fetchRecipientDomainMetrics(ctx context.Context, query MetricsQuery) error {
	resp, err := c.client.GetMetricsByDomain(ctx, query)
	if err != nil {
		log.Printf("Error fetching recipient domain metrics: %v", err)
		return err
	}

	var recipientDomainMetrics []RecipientDomainMetrics
	for _, result := range resp.Results {
		// Only include Yahoo family domains
		displayName, isYahooFamily := YahooFamilyDomains[result.Domain]
		if !isYahooFamily {
			continue
		}

		pm := ConvertToProcessedMetrics(result, "recipient_domain", result.Domain)
		
		status := "healthy"
		statusReason := ""
		if c.agent != nil {
			status, statusReason = c.agent.EvaluateHealth(pm)
		}

		recipientDomainMetrics = append(recipientDomainMetrics, RecipientDomainMetrics{
			Domain:       result.Domain,
			DisplayName:  displayName,
			Metrics:      pm,
			Status:       status,
			StatusReason: statusReason,
		})
	}

	log.Printf("SparkPost: Got recipient domain metrics for %d Yahoo family domains", len(recipientDomainMetrics))

	c.mu.Lock()
	c.latestRecipientDomain = recipientDomainMetrics
	c.mu.Unlock()

	return nil
}

// fetchSignals fetches bounce, delay, and rejection reasons
func (c *Collector) fetchSignals(ctx context.Context, query MetricsQuery) error {
	signals := SignalsData{
		Timestamp: time.Now(),
	}

	// Fetch bounce reasons
	bounceResp, err := c.client.GetBounceReasons(ctx, query)
	if err == nil && bounceResp != nil {
		signals.BounceReasons = bounceResp.Results
	}

	// Fetch delay reasons
	delayResp, err := c.client.GetDelayReasons(ctx, query)
	if err == nil && delayResp != nil {
		signals.DelayReasons = delayResp.Results
	}

	// Fetch rejection reasons
	rejectResp, err := c.client.GetRejectionReasons(ctx, query)
	if err == nil && rejectResp != nil {
		signals.RejectionReasons = rejectResp.Results
	}

	// Analyze for top issues
	signals.TopIssues = c.analyzeIssues(signals)

	c.mu.Lock()
	c.latestSignals = &signals
	c.mu.Unlock()

	if c.storage != nil {
		if err := c.storage.SaveSignals(ctx, signals); err != nil {
			log.Printf("Error saving signals: %v", err)
		}
	}

	return nil
}

// analyzeIssues analyzes signals data to identify top issues
func (c *Collector) analyzeIssues(signals SignalsData) []Issue {
	var issues []Issue

	// Analyze bounce reasons
	for _, bounce := range signals.BounceReasons {
		if bounce.CountBounce > 1000 {
			severity := "warning"
			if bounce.CountBounce > 10000 {
				severity = "critical"
			}

			issues = append(issues, Issue{
				Severity:       severity,
				Category:       "bounce",
				Description:    bounce.Reason,
				Count:          bounce.CountBounce,
				Recommendation: c.getBounceRecommendation(bounce),
			})
		}
	}

	// Analyze delay reasons
	for _, delay := range signals.DelayReasons {
		if delay.CountDelayed > 5000 {
			issues = append(issues, Issue{
				Severity:       "warning",
				Category:       "delay",
				Description:    delay.Reason,
				Count:          delay.CountDelayed,
				Recommendation: "Monitor delivery times and consider throttling if delays persist",
			})
		}
	}

	// Sort by count (most significant first) and limit
	// Simple bubble sort for small lists
	for i := 0; i < len(issues)-1; i++ {
		for j := 0; j < len(issues)-i-1; j++ {
			if issues[j].Count < issues[j+1].Count {
				issues[j], issues[j+1] = issues[j+1], issues[j]
			}
		}
	}

	if len(issues) > 10 {
		issues = issues[:10]
	}

	return issues
}

// getBounceRecommendation returns a recommendation based on bounce type
func (c *Collector) getBounceRecommendation(bounce BounceReasonResult) string {
	switch bounce.BounceCategoryName {
	case "Hard":
		return "Review list hygiene - remove invalid addresses"
	case "Soft":
		return "Monitor and retry - temporary issue"
	case "Block":
		return "Check IP reputation and blocklist status"
	default:
		return "Investigate bounce reason and take appropriate action"
	}
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

// GetLatestIPMetrics returns the latest IP metrics
func (c *Collector) GetLatestIPMetrics() []IPMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latestIP
}

// GetLatestDomainMetrics returns the latest domain metrics
func (c *Collector) GetLatestDomainMetrics() []DomainMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latestDomain
}

// GetLatestRecipientDomainMetrics returns the latest recipient domain metrics (Yahoo family)
func (c *Collector) GetLatestRecipientDomainMetrics() []RecipientDomainMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latestRecipientDomain
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

// DashboardData holds the complete dashboard response for a date range query.
type DashboardData struct {
	Summary       *Summary               `json:"summary"`
	ISPMetrics    []ISPMetrics            `json:"isp_metrics"`
	IPMetrics     []IPMetrics             `json:"ip_metrics"`
	DomainMetrics []DomainMetrics         `json:"domain_metrics"`
	Signals       *SignalsData            `json:"signals"`
}

// GetDashboardForDateRange makes fresh SparkPost API calls for the given date
// range and returns the results directly. It does NOT mutate the periodic cache
// so the background polling continues to serve the "live" view independently.
func (c *Collector) GetDashboardForDateRange(ctx context.Context, from, to time.Time) (*DashboardData, error) {
	// Choose precision based on the span
	span := to.Sub(from)
	precision := "hour"
	if span > 7*24*time.Hour {
		precision = "day"
	}

	query := MetricsQuery{
		From:      from,
		To:        to,
		Precision: precision,
		Limit:     100,
		OrderBy:   "count_targeted",
	}

	dashboard := &DashboardData{}

	// ── Summary ────────────────────────────────────────────────────────
	summaryResp, err := c.client.GetMetricsSummary(ctx, query)
	if err != nil {
		return nil, err
	}
	if len(summaryResp.Results) > 0 {
		result := summaryResp.Results[0]
		pm := ConvertToProcessedMetrics(result, "summary", "all")
		dashboard.Summary = &Summary{
			Timestamp:         time.Now(),
			PeriodStart:       from,
			PeriodEnd:         to,
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
	}

	// ── ISP Metrics ────────────────────────────────────────────────────
	ispResp, err := c.client.GetMetricsByMailboxProvider(ctx, query)
	if err == nil {
		for _, result := range ispResp.Results {
			pm := ConvertToProcessedMetrics(result, "mailbox_provider", result.MailboxProvider)
			status := "healthy"
			statusReason := ""
			if c.agent != nil {
				status, statusReason = c.agent.EvaluateHealth(pm)
			}
			dashboard.ISPMetrics = append(dashboard.ISPMetrics, ISPMetrics{
				Provider:     result.MailboxProvider,
				Metrics:      pm,
				Status:       status,
				StatusReason: statusReason,
			})
		}
	}

	// ── IP Metrics ─────────────────────────────────────────────────────
	ipResp, err := c.client.GetMetricsBySendingIP(ctx, query)
	if err == nil {
		for _, result := range ipResp.Results {
			pm := ConvertToProcessedMetrics(result, "sending_ip", result.SendingIP)
			status := "healthy"
			statusReason := ""
			if c.agent != nil {
				status, statusReason = c.agent.EvaluateHealth(pm)
			}
			dashboard.IPMetrics = append(dashboard.IPMetrics, IPMetrics{
				IP:           result.SendingIP,
				Pool:         "default",
				Metrics:      pm,
				Status:       status,
				StatusReason: statusReason,
			})
		}
	}

	// ── Domain Metrics ─────────────────────────────────────────────────
	domResp, err := c.client.GetMetricsBySendingDomain(ctx, query)
	if err == nil {
		for _, result := range domResp.Results {
			pm := ConvertToProcessedMetrics(result, "sending_domain", result.SendingDomain)
			status := "healthy"
			statusReason := ""
			if c.agent != nil {
				status, statusReason = c.agent.EvaluateHealth(pm)
			}
			dashboard.DomainMetrics = append(dashboard.DomainMetrics, DomainMetrics{
				Domain:       result.SendingDomain,
				Metrics:      pm,
				Status:       status,
				StatusReason: statusReason,
			})
		}
	}

	// ── Signals ────────────────────────────────────────────────────────
	signals := SignalsData{Timestamp: time.Now()}
	if bounceResp, err := c.client.GetBounceReasons(ctx, query); err == nil && bounceResp != nil {
		signals.BounceReasons = bounceResp.Results
	}
	if delayResp, err := c.client.GetDelayReasons(ctx, query); err == nil && delayResp != nil {
		signals.DelayReasons = delayResp.Results
	}
	if rejectResp, err := c.client.GetRejectionReasons(ctx, query); err == nil && rejectResp != nil {
		signals.RejectionReasons = rejectResp.Results
	}
	signals.TopIssues = c.analyzeIssues(signals)
	dashboard.Signals = &signals

	return dashboard, nil
}
