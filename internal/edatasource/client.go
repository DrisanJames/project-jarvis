package edatasource

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/httpretry"
)

const (
	BaseURL        = "https://api.edatasource.com/v4"
	DefaultTimeout = 30 * time.Second
)

// Client is the eDataSource V4 API client for inbox placement monitoring
type Client struct {
	apiKey     string
	httpClient httpretry.HTTPDoer
	baseURL    string
	dryRun     bool // When true, returns simulated data instead of calling API
}

// NewClient creates a new eDataSource API client
func NewClient(apiKey string, dryRun bool) *Client {
	return &Client{
		apiKey: apiKey,
		httpClient: httpretry.NewRetryClient(&http.Client{
			Timeout: DefaultTimeout,
		}, 3),
		baseURL: BaseURL,
		dryRun:  dryRun,
	}
}

// ============================================================================
// TYPES
// ============================================================================

// InboxPlacementResult represents inbox placement data for a campaign
type InboxPlacementResult struct {
	CampaignID    string               `json:"campaign_id"`
	Subject       string               `json:"subject"`
	FromName      string               `json:"from_name"`
	FromEmail     string               `json:"from_email"`
	SendDate      time.Time            `json:"send_date"`
	TotalSeeds    int                  `json:"total_seeds"`
	InboxCount    int                  `json:"inbox_count"`
	SpamCount     int                  `json:"spam_count"`
	MissingCount  int                  `json:"missing_count"`
	InboxRate     float64              `json:"inbox_rate"`
	SpamRate      float64              `json:"spam_rate"`
	MissingRate   float64              `json:"missing_rate"`
	ISPBreakdown  []ISPPlacementResult `json:"isp_breakdown"`
	LastCheckedAt time.Time            `json:"last_checked_at"`
}

// ISPPlacementResult represents per-ISP inbox placement
type ISPPlacementResult struct {
	ISP          string  `json:"isp"`
	InboxCount   int     `json:"inbox_count"`
	SpamCount    int     `json:"spam_count"`
	MissingCount int     `json:"missing_count"`
	InboxRate    float64 `json:"inbox_rate"`
	SpamRate     float64 `json:"spam_rate"`
	Trend        string  `json:"trend"` // improving, stable, declining
}

// SenderReputation represents eDataSource sender reputation data
type SenderReputation struct {
	Domain            string  `json:"domain"`
	ReputationScore   float64 `json:"reputation_score"` // 0-100
	InboxRate30Day    float64 `json:"inbox_rate_30day"`
	SpamRate30Day     float64 `json:"spam_rate_30day"`
	ComplaintRate     float64 `json:"complaint_rate"`
	BlacklistCount    int     `json:"blacklist_count"`
	AuthenticationPct float64 `json:"authentication_pct"` // SPF/DKIM pass rate
	VolumeChange      float64 `json:"volume_change"`      // % change from prior period
	Risk              string  `json:"risk"`                // low, medium, high, critical
}

// YahooInboxData represents Yahoo-specific inbox placement from eDataSource panel
type YahooInboxData struct {
	Domain        string    `json:"domain"`
	InboxRate     float64   `json:"inbox_rate"`
	SpamRate      float64   `json:"spam_rate"`
	MissingRate   float64   `json:"missing_rate"`
	BulkFolderPct float64   `json:"bulk_folder_pct"`
	SampleSize    int       `json:"sample_size"`
	LastUpdated   time.Time `json:"last_updated"`
	Trend         string    `json:"trend"` // improving, stable, declining
	RiskLevel     string    `json:"risk_level"`
}

// CampaignSearchResult is a search result from the eDataSource campaign database
type CampaignSearchResult struct {
	ID          string    `json:"id"`
	Subject     string    `json:"subject"`
	FromName    string    `json:"from_name"`
	FromDomain  string    `json:"from_domain"`
	SendDate    time.Time `json:"send_date"`
	InboxRate   float64   `json:"inbox_rate"`
	Volume      int       `json:"volume"`
	ContentType string    `json:"content_type"`
}

// ============================================================================
// API METHODS
// ============================================================================

// GetInboxPlacement fetches inbox placement data for a sending domain
func (c *Client) GetInboxPlacement(ctx context.Context, domain string, days int) (*InboxPlacementResult, error) {
	if c.dryRun {
		return c.mockInboxPlacement(domain), nil
	}

	params := url.Values{}
	params.Set("Authorization", c.apiKey)
	params.Set("d", domain)
	if days > 0 {
		params.Set("days", fmt.Sprintf("%d", days))
	}

	resp, err := c.get(ctx, "/inbox/placement", params)
	if err != nil {
		return nil, fmt.Errorf("failed to get inbox placement: %w", err)
	}

	var result InboxPlacementResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to parse inbox placement: %w", err)
	}

	return &result, nil
}

// GetYahooInboxData fetches Yahoo-specific inbox data (the primary use case)
func (c *Client) GetYahooInboxData(ctx context.Context, domain string) (*YahooInboxData, error) {
	if c.dryRun {
		return c.mockYahooInboxData(domain), nil
	}

	params := url.Values{}
	params.Set("Authorization", c.apiKey)
	params.Set("d", domain)
	params.Set("isp", "yahoo")

	resp, err := c.get(ctx, "/inbox/placement/isp", params)
	if err != nil {
		return nil, fmt.Errorf("failed to get Yahoo inbox data: %w", err)
	}

	var result YahooInboxData
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to parse Yahoo inbox data: %w", err)
	}

	return &result, nil
}

// GetSenderReputation fetches reputation data for a domain
func (c *Client) GetSenderReputation(ctx context.Context, domain string) (*SenderReputation, error) {
	if c.dryRun {
		return c.mockSenderReputation(domain), nil
	}

	params := url.Values{}
	params.Set("Authorization", c.apiKey)
	params.Set("d", domain)

	resp, err := c.get(ctx, "/sender/reputation", params)
	if err != nil {
		return nil, fmt.Errorf("failed to get sender reputation: %w", err)
	}

	var result SenderReputation
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to parse sender reputation: %w", err)
	}

	return &result, nil
}

// SearchCampaigns searches the eDataSource campaign database for competitive intelligence
func (c *Client) SearchCampaigns(ctx context.Context, query string, days int) ([]CampaignSearchResult, error) {
	if c.dryRun {
		return c.mockCampaignSearch(query), nil
	}

	params := url.Values{}
	params.Set("Authorization", c.apiKey)
	params.Set("q", query)
	if days > 0 {
		params.Set("days", fmt.Sprintf("%d", days))
	}

	resp, err := c.get(ctx, "/campaigns/search", params)
	if err != nil {
		return nil, fmt.Errorf("failed to search campaigns: %w", err)
	}

	var results []CampaignSearchResult
	if err := json.Unmarshal(resp, &results); err != nil {
		return nil, fmt.Errorf("failed to parse campaign search: %w", err)
	}

	return results, nil
}

// ============================================================================
// HTTP HELPER
// ============================================================================

func (c *Client) get(ctx context.Context, path string, params url.Values) ([]byte, error) {
	reqURL := fmt.Sprintf("%s%s?%s", c.baseURL, path, params.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("eDataSource API error %d: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// ============================================================================
// DRY-RUN MOCK DATA (realistic simulated responses for testing)
// ============================================================================

func (c *Client) mockInboxPlacement(domain string) *InboxPlacementResult {
	return &InboxPlacementResult{
		CampaignID:   "dry-run-test",
		Subject:      "Get $30 Sam's Cash with $50 Membership",
		FromName:     "Sam's Club Affiliate",
		FromEmail:    fmt.Sprintf("offers@%s", domain),
		SendDate:     time.Now(),
		TotalSeeds:   50,
		InboxCount:   38,
		SpamCount:    8,
		MissingCount: 4,
		InboxRate:    76.0,
		SpamRate:     16.0,
		MissingRate:  8.0,
		ISPBreakdown: []ISPPlacementResult{
			{ISP: "yahoo", InboxCount: 12, SpamCount: 5, MissingCount: 3, InboxRate: 60.0, SpamRate: 25.0, Trend: "stable"},
			{ISP: "gmail", InboxCount: 15, SpamCount: 1, MissingCount: 0, InboxRate: 93.75, SpamRate: 6.25, Trend: "improving"},
			{ISP: "outlook", InboxCount: 8, SpamCount: 2, MissingCount: 1, InboxRate: 72.7, SpamRate: 18.2, Trend: "stable"},
			{ISP: "aol", InboxCount: 3, SpamCount: 0, MissingCount: 0, InboxRate: 100.0, SpamRate: 0.0, Trend: "improving"},
		},
		LastCheckedAt: time.Now(),
	}
}

func (c *Client) mockYahooInboxData(domain string) *YahooInboxData {
	return &YahooInboxData{
		Domain:        domain,
		InboxRate:     62.5,
		SpamRate:      25.0,
		MissingRate:   12.5,
		BulkFolderPct: 22.0,
		SampleSize:    40,
		LastUpdated:   time.Now(),
		Trend:         "stable",
		RiskLevel:     "medium",
	}
}

func (c *Client) mockSenderReputation(domain string) *SenderReputation {
	return &SenderReputation{
		Domain:            domain,
		ReputationScore:   68.5,
		InboxRate30Day:    71.2,
		SpamRate30Day:     18.3,
		ComplaintRate:     0.045,
		BlacklistCount:    0,
		AuthenticationPct: 99.8,
		VolumeChange:      0.0,
		Risk:              "medium",
	}
}

func (c *Client) mockCampaignSearch(query string) []CampaignSearchResult {
	return []CampaignSearchResult{
		{
			ID:         "mock-1",
			Subject:    "Get $30 Sam's Cash with $50 Membership",
			FromName:   "Sam's Club Affiliate",
			FromDomain: "promotions.samsclub.com",
			SendDate:   time.Now().AddDate(0, 0, -2),
			InboxRate:  82.0,
			Volume:     500000,
		},
		{
			ID:         "mock-2",
			Subject:    "A $20 Sam's Club Membership?",
			FromName:   "Sam's Club Partner",
			FromDomain: "offers.samsclub.com",
			SendDate:   time.Now().AddDate(0, 0, -5),
			InboxRate:  78.5,
			Volume:     350000,
		},
	}
}
