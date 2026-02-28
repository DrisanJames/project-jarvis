package sparkpost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/pkg/httpretry"
)

// Client is a SparkPost API client
type Client struct {
	baseURL    string
	apiKey     string
	httpClient httpretry.HTTPDoer
}

// NewClient creates a new SparkPost API client
func NewClient(cfg config.SparkPostConfig) *Client {
	return &Client{
		baseURL: cfg.BaseURL,
		apiKey:  cfg.APIKey,
		httpClient: httpretry.NewRetryClient(&http.Client{
			Timeout: cfg.Timeout(),
		}, 3),
	}
}

// doRequest makes an HTTP request to the SparkPost API
func (c *Client) doRequest(ctx context.Context, method, path string, params url.Values) ([]byte, error) {
	fullURL := c.baseURL + path
	if len(params) > 0 {
		fullURL += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Authorization", c.apiKey)
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

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// buildMetricsParams builds URL parameters for metrics requests
func (c *Client) buildMetricsParams(query MetricsQuery) url.Values {
	params := url.Values{}

	// Time range
	params.Set("from", query.From.Format("2006-01-02T15:04"))
	if !query.To.IsZero() {
		params.Set("to", query.To.Format("2006-01-02T15:04"))
	}

	// Precision
	if query.Precision != "" {
		params.Set("precision", query.Precision)
	}

	// Metrics
	if len(query.Metrics) > 0 {
		params.Set("metrics", strings.Join(query.Metrics, ","))
	} else {
		params.Set("metrics", strings.Join(DefaultMetrics(), ","))
	}

	// Filters
	if len(query.Domains) > 0 {
		params.Set("domains", strings.Join(query.Domains, ","))
	}
	if len(query.SendingIPs) > 0 {
		params.Set("sending_ips", strings.Join(query.SendingIPs, ","))
	}
	if len(query.IPPools) > 0 {
		params.Set("ip_pools", strings.Join(query.IPPools, ","))
	}
	if len(query.SendingDomains) > 0 {
		params.Set("sending_domains", strings.Join(query.SendingDomains, ","))
	}
	if len(query.Campaigns) > 0 {
		params.Set("campaigns", strings.Join(query.Campaigns, ","))
	}
	if len(query.MailboxProviders) > 0 {
		params.Set("mailbox_providers", strings.Join(query.MailboxProviders, ","))
	}

	// Limit and order
	if query.Limit > 0 {
		params.Set("limit", fmt.Sprintf("%d", query.Limit))
	}
	if query.OrderBy != "" {
		params.Set("order_by", query.OrderBy)
	}

	params.Set("timezone", "UTC")

	return params
}

// GetMetricsSummary fetches overall metrics summary
func (c *Client) GetMetricsSummary(ctx context.Context, query MetricsQuery) (*MetricsResponse, error) {
	params := c.buildMetricsParams(query)

	body, err := c.doRequest(ctx, http.MethodGet, "/metrics/deliverability", params)
	if err != nil {
		return nil, fmt.Errorf("fetching metrics summary: %w", err)
	}

	var response MetricsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing metrics summary: %w", err)
	}

	return &response, nil
}

// GetMetricsByMailboxProvider fetches metrics grouped by mailbox provider (ISP)
func (c *Client) GetMetricsByMailboxProvider(ctx context.Context, query MetricsQuery) (*MetricsResponse, error) {
	params := c.buildMetricsParams(query)

	body, err := c.doRequest(ctx, http.MethodGet, "/metrics/deliverability/mailbox-provider", params)
	if err != nil {
		return nil, fmt.Errorf("fetching metrics by mailbox provider: %w", err)
	}

	var response MetricsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing metrics by mailbox provider: %w", err)
	}

	return &response, nil
}

// GetMetricsByDomain fetches metrics grouped by recipient domain
func (c *Client) GetMetricsByDomain(ctx context.Context, query MetricsQuery) (*MetricsResponse, error) {
	params := c.buildMetricsParams(query)

	body, err := c.doRequest(ctx, http.MethodGet, "/metrics/deliverability/domain", params)
	if err != nil {
		return nil, fmt.Errorf("fetching metrics by domain: %w", err)
	}

	var response MetricsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing metrics by domain: %w", err)
	}

	return &response, nil
}

// GetMetricsBySendingIP fetches metrics grouped by sending IP
func (c *Client) GetMetricsBySendingIP(ctx context.Context, query MetricsQuery) (*MetricsResponse, error) {
	params := c.buildMetricsParams(query)

	body, err := c.doRequest(ctx, http.MethodGet, "/metrics/deliverability/sending-ip", params)
	if err != nil {
		return nil, fmt.Errorf("fetching metrics by sending IP: %w", err)
	}

	var response MetricsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing metrics by sending IP: %w", err)
	}

	return &response, nil
}

// GetMetricsByIPPool fetches metrics grouped by IP pool
func (c *Client) GetMetricsByIPPool(ctx context.Context, query MetricsQuery) (*MetricsResponse, error) {
	params := c.buildMetricsParams(query)

	body, err := c.doRequest(ctx, http.MethodGet, "/metrics/deliverability/ip-pool", params)
	if err != nil {
		return nil, fmt.Errorf("fetching metrics by IP pool: %w", err)
	}

	var response MetricsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing metrics by IP pool: %w", err)
	}

	return &response, nil
}

// GetMetricsBySendingDomain fetches metrics grouped by sending domain
func (c *Client) GetMetricsBySendingDomain(ctx context.Context, query MetricsQuery) (*MetricsResponse, error) {
	params := c.buildMetricsParams(query)

	body, err := c.doRequest(ctx, http.MethodGet, "/metrics/deliverability/sending-domain", params)
	if err != nil {
		return nil, fmt.Errorf("fetching metrics by sending domain: %w", err)
	}

	var response MetricsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing metrics by sending domain: %w", err)
	}

	return &response, nil
}

// GetMetricsByCampaign fetches metrics grouped by campaign
func (c *Client) GetMetricsByCampaign(ctx context.Context, query MetricsQuery) (*MetricsResponse, error) {
	params := c.buildMetricsParams(query)

	body, err := c.doRequest(ctx, http.MethodGet, "/metrics/deliverability/campaign", params)
	if err != nil {
		return nil, fmt.Errorf("fetching metrics by campaign: %w", err)
	}

	var response MetricsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing metrics by campaign: %w", err)
	}

	return &response, nil
}

// GetTimeSeriesMetrics fetches time-series metrics
func (c *Client) GetTimeSeriesMetrics(ctx context.Context, query MetricsQuery) (*MetricsResponse, error) {
	params := c.buildMetricsParams(query)

	body, err := c.doRequest(ctx, http.MethodGet, "/metrics/deliverability/time-series", params)
	if err != nil {
		return nil, fmt.Errorf("fetching time-series metrics: %w", err)
	}

	var response MetricsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing time-series metrics: %w", err)
	}

	return &response, nil
}

// GetBounceReasons fetches bounce reason metrics
func (c *Client) GetBounceReasons(ctx context.Context, query MetricsQuery) (*BounceReasonResponse, error) {
	params := c.buildMetricsParams(query)
	params.Set("metrics", "count_bounce,count_inband_bounce,count_outofband_bounce")

	body, err := c.doRequest(ctx, http.MethodGet, "/metrics/deliverability/bounce-reason", params)
	if err != nil {
		return nil, fmt.Errorf("fetching bounce reasons: %w", err)
	}

	var response BounceReasonResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing bounce reasons: %w", err)
	}

	return &response, nil
}

// GetBounceReasonsByDomain fetches bounce reasons grouped by domain
func (c *Client) GetBounceReasonsByDomain(ctx context.Context, query MetricsQuery) (*BounceReasonResponse, error) {
	params := c.buildMetricsParams(query)
	params.Set("metrics", "count_bounce,count_inband_bounce,count_outofband_bounce")

	body, err := c.doRequest(ctx, http.MethodGet, "/metrics/deliverability/bounce-reason/domain", params)
	if err != nil {
		return nil, fmt.Errorf("fetching bounce reasons by domain: %w", err)
	}

	var response BounceReasonResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing bounce reasons by domain: %w", err)
	}

	return &response, nil
}

// GetDelayReasons fetches delay reason metrics
func (c *Client) GetDelayReasons(ctx context.Context, query MetricsQuery) (*DelayReasonResponse, error) {
	params := c.buildMetricsParams(query)

	body, err := c.doRequest(ctx, http.MethodGet, "/metrics/deliverability/delay-reason", params)
	if err != nil {
		return nil, fmt.Errorf("fetching delay reasons: %w", err)
	}

	var response DelayReasonResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing delay reasons: %w", err)
	}

	return &response, nil
}

// GetRejectionReasons fetches rejection reason metrics
func (c *Client) GetRejectionReasons(ctx context.Context, query MetricsQuery) (*RejectionReasonResponse, error) {
	params := c.buildMetricsParams(query)

	body, err := c.doRequest(ctx, http.MethodGet, "/metrics/deliverability/rejection-reason", params)
	if err != nil {
		return nil, fmt.Errorf("fetching rejection reasons: %w", err)
	}

	var response RejectionReasonResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing rejection reasons: %w", err)
	}

	return &response, nil
}

// GetMailboxProviders fetches list of mailbox providers
func (c *Client) GetMailboxProviders(ctx context.Context, from, to time.Time) ([]string, error) {
	params := url.Values{}
	params.Set("from", from.Format("2006-01-02T15:04"))
	params.Set("to", to.Format("2006-01-02T15:04"))

	body, err := c.doRequest(ctx, http.MethodGet, "/metrics/mailbox-providers", params)
	if err != nil {
		return nil, fmt.Errorf("fetching mailbox providers: %w", err)
	}

	var response ListResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing mailbox providers: %w", err)
	}

	return response.Results["mailbox-providers"], nil
}

// GetSendingIPs fetches list of sending IPs
func (c *Client) GetSendingIPs(ctx context.Context, from, to time.Time) ([]string, error) {
	params := url.Values{}
	params.Set("from", from.Format("2006-01-02T15:04"))
	params.Set("to", to.Format("2006-01-02T15:04"))

	body, err := c.doRequest(ctx, http.MethodGet, "/metrics/sending-ips", params)
	if err != nil {
		return nil, fmt.Errorf("fetching sending IPs: %w", err)
	}

	var response ListResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing sending IPs: %w", err)
	}

	return response.Results["sending-ips"], nil
}

// GetSendingDomains fetches list of sending domains
func (c *Client) GetSendingDomains(ctx context.Context, from, to time.Time) ([]string, error) {
	params := url.Values{}
	params.Set("from", from.Format("2006-01-02T15:04"))
	params.Set("to", to.Format("2006-01-02T15:04"))

	body, err := c.doRequest(ctx, http.MethodGet, "/metrics/sending-domains", params)
	if err != nil {
		return nil, fmt.Errorf("fetching sending domains: %w", err)
	}

	var response ListResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing sending domains: %w", err)
	}

	return response.Results["sending-domains"], nil
}

// GetIPPools fetches list of IP pools
func (c *Client) GetIPPools(ctx context.Context, from, to time.Time) ([]string, error) {
	params := url.Values{}
	params.Set("from", from.Format("2006-01-02T15:04"))
	params.Set("to", to.Format("2006-01-02T15:04"))

	body, err := c.doRequest(ctx, http.MethodGet, "/metrics/ip-pools", params)
	if err != nil {
		return nil, fmt.Errorf("fetching IP pools: %w", err)
	}

	var response ListResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing IP pools: %w", err)
	}

	return response.Results["ip-pools"], nil
}

// ConvertToProcessedMetrics converts a MetricResult to ProcessedMetrics
func ConvertToProcessedMetrics(result MetricResult, groupBy, groupValue string) ProcessedMetrics {
	pm := ProcessedMetrics{
		Timestamp:     time.Now(),
		Source:        "sparkpost",
		GroupBy:       groupBy,
		GroupValue:    groupValue,
		Targeted:      result.CountTargeted,
		Injected:      result.CountInjected,
		Sent:          result.CountSent,
		Delivered:     result.CountDelivered,
		Opened:        result.CountRendered,
		UniqueOpened:  result.CountUniqueRendered,
		Clicked:       result.CountClicked,
		UniqueClicked: result.CountUniqueClicked,
		Bounced:       result.CountBounce,
		HardBounced:   result.CountHardBounce,
		SoftBounced:   result.CountSoftBounce,
		BlockBounced:  result.CountBlockBounce,
		Complaints:    result.CountSpamComplaint,
		Unsubscribes:  result.CountUnsubscribe,
		Delayed:       result.CountDelayed,
		Rejected:      result.CountRejected,
	}
	pm.CalculateRates()
	return pm
}
