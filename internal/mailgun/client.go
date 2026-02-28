package mailgun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/pkg/httpretry"
)

// Client is a Mailgun API client
type Client struct {
	baseURL    string
	apiKey     string
	httpClient httpretry.HTTPDoer
	domains    []string
}

// NewClient creates a new Mailgun API client
func NewClient(cfg config.MailgunConfig) *Client {
	return &Client{
		baseURL: cfg.BaseURL,
		apiKey:  cfg.APIKey,
		domains: cfg.Domains,
		httpClient: httpretry.NewRetryClient(&http.Client{
			Timeout: cfg.Timeout(),
		}, 3),
	}
}

// GetDomains returns the configured sending domains
func (c *Client) GetDomains() []string {
	return c.domains
}

// doRequest makes an HTTP request to the Mailgun API with Basic Auth
func (c *Client) doRequest(ctx context.Context, method, path string, body interface{}) ([]byte, error) {
	fullURL := c.baseURL + path

	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshaling request body: %w", err)
		}
		reqBody = bytes.NewReader(jsonBody)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	// Mailgun uses Basic Auth with "api" as username
	req.SetBasicAuth("api", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// doRequestWithParams makes an HTTP GET request with URL parameters
func (c *Client) doRequestWithParams(ctx context.Context, path string, params url.Values) ([]byte, error) {
	fullURL := c.baseURL + path
	if len(params) > 0 {
		fullURL += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.SetBasicAuth("api", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("Mailgun API error: %s returned %d: %s", path, resp.StatusCode, string(body))
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// GetMetrics fetches metrics from the analytics API
func (c *Client) GetMetrics(ctx context.Context, req MetricsRequest) (*MetricsResponse, error) {
	body, err := c.doRequest(ctx, http.MethodPost, "/v1/analytics/metrics", req)
	if err != nil {
		return nil, fmt.Errorf("fetching metrics: %w", err)
	}

	var response MetricsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing metrics response: %w", err)
	}

	return &response, nil
}

// GetDomainStats fetches stats for a specific domain
func (c *Client) GetDomainStats(ctx context.Context, domain string, from, to time.Time, resolution string) (*StatsResponse, error) {
	params := url.Values{}
	// Mailgun uses RFC 2822 date format or Unix timestamp
	params.Set("start", from.Format(time.RFC1123Z))
	params.Set("end", to.Format(time.RFC1123Z))
	params.Set("resolution", resolution)
	// Request all event types
	params.Add("event", "accepted")
	params.Add("event", "delivered")
	params.Add("event", "failed")
	params.Add("event", "opened")
	params.Add("event", "clicked")
	params.Add("event", "unsubscribed")
	params.Add("event", "complained")
	params.Add("event", "stored")

	path := fmt.Sprintf("/v3/%s/stats/total", domain)
	body, err := c.doRequestWithParams(ctx, path, params)
	if err != nil {
		return nil, fmt.Errorf("fetching domain stats for %s: %w", domain, err)
	}

	var response StatsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing domain stats: %w", err)
	}

	return &response, nil
}

// GetProviderAggregates fetches provider/ISP aggregates for a domain
// Note: This endpoint may be deprecated - falls back to empty response if not available
func (c *Client) GetProviderAggregates(ctx context.Context, domain string) (*ProviderAggregatesResponse, error) {
	path := fmt.Sprintf("/v3/%s/aggregates/providers", domain)
	body, err := c.doRequestWithParams(ctx, path, nil)
	if err != nil {
		// This endpoint is deprecated and may not be available
		// Return empty response instead of error
		return &ProviderAggregatesResponse{
			Providers: make(map[string]ProviderStats),
		}, nil
	}

	var response ProviderAggregatesResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return &ProviderAggregatesResponse{
			Providers: make(map[string]ProviderStats),
		}, nil
	}

	return &response, nil
}

// GetBounceClassification fetches bounce classification metrics
// Note: This endpoint may require a specific Mailgun plan
func (c *Client) GetBounceClassification(ctx context.Context, domain string, from, to time.Time) (*BounceClassificationResponse, error) {
	req := BounceClassificationRequest{
		Start:  from.Format(time.RFC3339),
		End:    to.Format(time.RFC3339),
		Domain: domain,
	}

	body, err := c.doRequest(ctx, http.MethodPost, "/v2/bounce-classification/metrics", req)
	if err != nil {
		// This endpoint may not be available on all plans
		// Return empty response
		return &BounceClassificationResponse{
			Items: []BounceClassificationItem{},
		}, nil
	}

	var response BounceClassificationResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return &BounceClassificationResponse{
			Items: []BounceClassificationItem{},
		}, nil
	}

	return &response, nil
}

// GetEvents fetches events/logs for a domain
func (c *Client) GetEvents(ctx context.Context, domain string, from, to time.Time, eventTypes []string, limit int) (*EventsResponse, error) {
	params := url.Values{}
	params.Set("begin", fmt.Sprintf("%d", from.Unix()))
	params.Set("end", fmt.Sprintf("%d", to.Unix()))
	params.Set("ascending", "no")
	
	if limit > 0 {
		params.Set("limit", fmt.Sprintf("%d", limit))
	}

	for _, et := range eventTypes {
		params.Add("event", et)
	}

	path := fmt.Sprintf("/v3/%s/events", domain)
	body, err := c.doRequestWithParams(ctx, path, params)
	if err != nil {
		return nil, fmt.Errorf("fetching events for %s: %w", domain, err)
	}

	var response EventsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing events: %w", err)
	}

	return &response, nil
}

// GetSendingIPs fetches the list of sending IPs
func (c *Client) GetSendingIPs(ctx context.Context) (*IPsResponse, error) {
	body, err := c.doRequestWithParams(ctx, "/v3/ips", nil)
	if err != nil {
		return nil, fmt.Errorf("fetching sending IPs: %w", err)
	}

	var response IPsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing IPs response: %w", err)
	}

	return &response, nil
}

// GetIPPools fetches the list of IP pools
func (c *Client) GetIPPools(ctx context.Context) (*IPPoolsResponse, error) {
	body, err := c.doRequestWithParams(ctx, "/v1/ip_pools", nil)
	if err != nil {
		return nil, fmt.Errorf("fetching IP pools: %w", err)
	}

	var response IPPoolsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing IP pools response: %w", err)
	}

	return &response, nil
}

// GetDomainsInfo fetches the list of domains from the API
func (c *Client) GetDomainsInfo(ctx context.Context) (*DomainsResponse, error) {
	body, err := c.doRequestWithParams(ctx, "/v4/domains", nil)
	if err != nil {
		return nil, fmt.Errorf("fetching domains: %w", err)
	}

	var response DomainsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing domains response: %w", err)
	}

	return &response, nil
}

// BuildMetricsRequest creates a MetricsRequest from query parameters
func (c *Client) BuildMetricsRequest(query MetricsQuery) MetricsRequest {
	req := MetricsRequest{
		Start:      query.From.Format(time.RFC3339),
		End:        query.To.Format(time.RFC3339),
		Resolution: query.Resolution,
		Metrics:    DefaultMetrics(),
	}

	if req.Resolution == "" {
		req.Resolution = "hour"
	}

	// Add domain filter if specified
	if len(query.Domains) > 0 {
		labeledValues := make([]LabeledValue, len(query.Domains))
		for i, domain := range query.Domains {
			labeledValues[i] = LabeledValue{
				Label: domain,
				Value: domain,
			}
		}
		req.Filter = &Filter{
			AND: []FilterCondition{
				{
					Attribute:  "domain",
					Comparator: "=",
					Values:     labeledValues,
				},
			},
		}
	}

	return req
}

// GetAllDomainsStats fetches stats for all configured domains
func (c *Client) GetAllDomainsStats(ctx context.Context, from, to time.Time, resolution string) (map[string]*StatsResponse, error) {
	results := make(map[string]*StatsResponse)
	
	for _, domain := range c.domains {
		stats, err := c.GetDomainStats(ctx, domain, from, to, resolution)
		if err != nil {
			// Log error but continue with other domains
			continue
		}
		results[domain] = stats
	}

	return results, nil
}

// GetAllDomainsProviderAggregates fetches provider aggregates for all configured domains
func (c *Client) GetAllDomainsProviderAggregates(ctx context.Context) (map[string]*ProviderAggregatesResponse, error) {
	results := make(map[string]*ProviderAggregatesResponse)
	
	for _, domain := range c.domains {
		aggregates, err := c.GetProviderAggregates(ctx, domain)
		if err != nil {
			// Log error but continue with other domains
			continue
		}
		results[domain] = aggregates
	}

	return results, nil
}

// AggregateProviderStats aggregates provider stats across all domains
func (c *Client) AggregateProviderStats(allStats map[string]*ProviderAggregatesResponse) map[string]ProviderStats {
	aggregated := make(map[string]ProviderStats)

	for _, domainStats := range allStats {
		if domainStats == nil {
			continue
		}
		for provider, stats := range domainStats.Providers {
			existing := aggregated[provider]
			existing.Accepted += stats.Accepted
			existing.Delivered += stats.Delivered
			existing.Opened += stats.Opened
			existing.Clicked += stats.Clicked
			existing.Unsubscribed += stats.Unsubscribed
			existing.Complained += stats.Complained
			existing.Bounced += stats.Bounced
			aggregated[provider] = existing
		}
	}

	return aggregated
}

// GetMetricsSummary fetches overall metrics summary across all domains
func (c *Client) GetMetricsSummary(ctx context.Context, query MetricsQuery) (*ProcessedMetrics, error) {
	// Use the stats API for each domain and aggregate
	from := query.From
	to := query.To
	resolution := query.Resolution
	if resolution == "" {
		resolution = "day"
	}

	var totalPM ProcessedMetrics
	totalPM.Timestamp = time.Now()
	totalPM.Source = "mailgun"
	totalPM.GroupBy = "summary"
	totalPM.GroupValue = "all"

	successCount := 0
	for _, domain := range c.domains {
		stats, err := c.GetDomainStats(ctx, domain, from, to, resolution)
		if err != nil {
			log.Printf("Mailgun: Failed to get stats for domain %s: %v", domain, err)
			continue
		}
		successCount++

		for _, item := range stats.Stats {
			totalPM.Targeted += item.Accepted.Total
			totalPM.Sent += item.Accepted.Total
			totalPM.Delivered += item.Delivered.Total
			totalPM.Opened += item.Opened.Total
			totalPM.UniqueOpened += item.Opened.Total
			totalPM.Clicked += item.Clicked.Total
			totalPM.UniqueClicked += item.Clicked.Total
			totalPM.Bounced += item.Failed.Permanent.Total + item.Failed.Temporary.Total
			totalPM.HardBounced += item.Failed.Permanent.Bounce
			totalPM.SoftBounced += item.Failed.Temporary.Total
			totalPM.BlockBounced += item.Failed.Permanent.EspBlock
			totalPM.Complaints += item.Complained.Total
			totalPM.Unsubscribes += item.Unsubscribed.Total
		}
	}

	log.Printf("Mailgun: Got stats from %d/%d domains", successCount, len(c.domains))
	totalPM.CalculateRates()
	return &totalPM, nil
}

// GetMetricsByDomain fetches metrics grouped by sending domain
func (c *Client) GetMetricsByDomain(ctx context.Context, query MetricsQuery) ([]DomainMetrics, error) {
	from := query.From
	to := query.To
	resolution := query.Resolution
	if resolution == "" {
		resolution = "day"
	}

	var domainMetrics []DomainMetrics

	for _, domain := range c.domains {
		stats, err := c.GetDomainStats(ctx, domain, from, to, resolution)
		if err != nil {
			continue
		}

		var pm ProcessedMetrics
		pm.Timestamp = time.Now()
		pm.Source = "mailgun"
		pm.Domain = domain
		pm.GroupBy = "domain"
		pm.GroupValue = domain

		for _, item := range stats.Stats {
			pm.Targeted += item.Accepted.Total
			pm.Sent += item.Accepted.Total
			pm.Delivered += item.Delivered.Total
			pm.Opened += item.Opened.Total
			pm.UniqueOpened += item.Opened.Total
			pm.Clicked += item.Clicked.Total
			pm.UniqueClicked += item.Clicked.Total
			pm.Bounced += item.Failed.Permanent.Total + item.Failed.Temporary.Total
			pm.HardBounced += item.Failed.Permanent.Bounce
			pm.SoftBounced += item.Failed.Temporary.Total
			pm.BlockBounced += item.Failed.Permanent.EspBlock
			pm.Complaints += item.Complained.Total
			pm.Unsubscribes += item.Unsubscribed.Total
		}
		pm.CalculateRates()

		domainMetrics = append(domainMetrics, DomainMetrics{
			Domain:  domain,
			Metrics: pm,
			Status:  "healthy",
		})
	}

	return domainMetrics, nil
}

// GetMetricsByProvider fetches metrics grouped by email provider/ISP using Analytics API
func (c *Client) GetMetricsByProvider(ctx context.Context, from, to time.Time) ([]ISPMetrics, error) {
	// Aggregate by ISP (map recipient domains to ISP names)
	ispAggregates := make(map[string]*ProcessedMetrics)

	// Fetch all pages of results
	const pageSize = 100
	skip := 0
	totalFetched := 0

	for {
		// Use Analytics API with recipient_domain dimension
		// Note: Analytics API requires RFC 2822 format for dates
		req := MetricsRequest{
			Start:      from.Format(time.RFC1123Z),
			End:        to.Format(time.RFC1123Z),
			Resolution: "day",
			Dimensions: []string{"recipient_domain"},
			Metrics:    DefaultMetrics(),
			Limit:      pageSize,
			Skip:       skip,
		}

		// Add domain filter for configured sending domains
		if len(c.domains) > 0 {
			labeledValues := make([]LabeledValue, len(c.domains))
			for i, domain := range c.domains {
				labeledValues[i] = LabeledValue{
					Label: domain,
					Value: domain,
				}
			}
			req.Filter = &Filter{
				AND: []FilterCondition{
					{
						Attribute:  "domain",
						Comparator: "=",
						Values:     labeledValues,
					},
				},
			}
		}

		resp, err := c.GetMetrics(ctx, req)
		if err != nil {
			log.Printf("Mailgun Analytics API error for ISP metrics: %v", err)
			// Fall back to empty list
			return []ISPMetrics{}, nil
		}

		// Process items from this page
		for _, item := range resp.Items {
			// Extract recipient_domain from dimensions array
			var recipientDomain string
			for _, dim := range item.Dimensions {
				if dim.Dimension == "recipient_domain" {
					recipientDomain = dim.Value
					break
				}
			}
			if recipientDomain == "" {
				continue
			}

			ispName := MapDomainToISP(recipientDomain)

			if _, exists := ispAggregates[ispName]; !exists {
				ispAggregates[ispName] = &ProcessedMetrics{
					Timestamp:  time.Now(),
					Source:     "mailgun",
					GroupBy:    "isp",
					GroupValue: ispName,
				}
			}

			pm := ispAggregates[ispName]
			pm.Targeted += item.Metrics.AcceptedOutgoingCount
			pm.Sent += item.Metrics.AcceptedOutgoingCount
			pm.Delivered += item.Metrics.DeliveredSMTPCount
			pm.Opened += item.Metrics.OpenedCount
			pm.UniqueOpened += item.Metrics.UniqueOpenedCount
			pm.Clicked += item.Metrics.ClickedCount
			pm.UniqueClicked += item.Metrics.UniqueClickedCount
			pm.Bounced += item.Metrics.BouncedCount
			pm.HardBounced += item.Metrics.HardBouncesCount
			pm.SoftBounced += item.Metrics.SoftBouncesCount
			pm.Complaints += item.Metrics.ComplainedCount
			pm.Unsubscribes += item.Metrics.UnsubscribedCount
		}

		totalFetched += len(resp.Items)

		// Check if we need to fetch more pages
		if resp.Pagination == nil || resp.Pagination.Total <= totalFetched {
			// No pagination info or we've fetched all items
			break
		}

		// Move to next page
		skip += pageSize

		// Safety check to prevent infinite loops
		if skip >= resp.Pagination.Total || skip > 10000 {
			break
		}

		log.Printf("Mailgun: Fetched %d/%d recipient domains, getting next page...", totalFetched, resp.Pagination.Total)
	}

	// Convert to ISPMetrics slice
	var ispMetrics []ISPMetrics
	for ispName, pm := range ispAggregates {
		pm.CalculateRates()

		status, statusReason := evaluateISPHealth(pm)

		ispMetrics = append(ispMetrics, ISPMetrics{
			Provider:     ispName,
			Metrics:      *pm,
			Status:       status,
			StatusReason: statusReason,
		})
	}

	log.Printf("Mailgun: Got ISP metrics for %d providers from %d recipient domains", len(ispMetrics), totalFetched)
	return ispMetrics, nil
}

// evaluateISPHealth determines the health status of ISP metrics
func evaluateISPHealth(pm *ProcessedMetrics) (string, string) {
	// Check complaint rate first (most critical)
	if pm.ComplaintRate >= 0.001 { // 0.1%
		return "critical", fmt.Sprintf("Complaint rate %.4f%% exceeds critical threshold", pm.ComplaintRate*100)
	}
	if pm.ComplaintRate >= 0.0005 { // 0.05%
		return "warning", fmt.Sprintf("Complaint rate %.4f%% approaching threshold", pm.ComplaintRate*100)
	}

	// Check bounce rate
	if pm.BounceRate >= 0.10 { // 10%
		return "critical", fmt.Sprintf("Bounce rate %.2f%% exceeds critical threshold", pm.BounceRate*100)
	}
	if pm.BounceRate >= 0.05 { // 5%
		return "warning", fmt.Sprintf("Bounce rate %.2f%% approaching threshold", pm.BounceRate*100)
	}

	// Check delivery rate
	if pm.DeliveryRate < 0.90 { // Below 90%
		return "warning", fmt.Sprintf("Delivery rate %.2f%% below expected", pm.DeliveryRate*100)
	}

	return "healthy", ""
}

// GetBounceReasons fetches bounce reasons for all domains
func (c *Client) GetBounceReasons(ctx context.Context, from, to time.Time) (*SignalsData, error) {
	signals := SignalsData{
		Timestamp: time.Now(),
	}

	for _, domain := range c.domains {
		bounces, err := c.GetBounceClassification(ctx, domain, from, to)
		if err != nil {
			continue
		}
		signals.BounceReasons = append(signals.BounceReasons, bounces.Items...)
	}

	// Analyze for top issues
	signals.TopIssues = analyzeIssues(signals)

	return &signals, nil
}

// analyzeIssues analyzes signals data to identify top issues
func analyzeIssues(signals SignalsData) []Issue {
	var issues []Issue

	// Analyze bounce reasons
	for _, bounce := range signals.BounceReasons {
		if bounce.Count > 1000 {
			severity := "warning"
			if bounce.Count > 10000 {
				severity = "critical"
			}

			recommendation := getBounceRecommendation(bounce.Classification)

			issues = append(issues, Issue{
				Severity:       severity,
				Category:       "bounce",
				Description:    fmt.Sprintf("%s bounce: %s", bounce.Classification, bounce.Reason),
				Count:          bounce.Count,
				Recommendation: recommendation,
			})
		}
	}

	// Sort by count (most significant first)
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

// getBounceRecommendation returns a recommendation based on bounce classification
func getBounceRecommendation(classification string) string {
	switch classification {
	case "hard":
		return "Review list hygiene - remove invalid addresses"
	case "soft":
		return "Monitor and retry - temporary issue"
	case "espblock":
		return "Check IP reputation and ESP blocklist status"
	default:
		return "Investigate bounce reason and take appropriate action"
	}
}
