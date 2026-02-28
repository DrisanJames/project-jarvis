package everflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/httpretry"
)

// Client is the Everflow API client
type Client struct {
	baseURL      string
	apiKey       string
	timezoneID   int
	currencyID   string
	affiliateIDs []string
	httpClient   httpretry.HTTPDoer
}

// NewClient creates a new Everflow API client
func NewClient(config Config) *Client {
	return &Client{
		baseURL:      config.BaseURL,
		apiKey:       config.APIKey,
		timezoneID:   config.TimezoneID,
		currencyID:   config.CurrencyID,
		affiliateIDs: config.AffiliateIDs,
		httpClient: httpretry.NewRetryClient(&http.Client{
			Timeout: 120 * time.Second, // Longer timeout for historical queries
		}, 3),
	}
}

// SetHTTPClient sets a custom HTTP client (useful for testing)
func (c *Client) SetHTTPClient(client httpretry.HTTPDoer) {
	c.httpClient = client
}

// doRequest performs an authenticated request to the Everflow API
func (c *Client) doRequest(ctx context.Context, method, endpoint string, body interface{}) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewBuffer(jsonBody)
	}

	reqURL := fmt.Sprintf("%s%s", c.baseURL, endpoint)

	req, err := http.NewRequestWithContext(ctx, method, reqURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Eflow-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// GetClicks retrieves click data for a date range
func (c *Client) GetClicks(ctx context.Context, from, to string, affiliateIDs []string) ([]ClickRecord, error) {
	if len(affiliateIDs) == 0 {
		affiliateIDs = c.affiliateIDs
	}

	// Build filters for each affiliate
	filters := make([]Filter, len(affiliateIDs))
	for i, id := range affiliateIDs {
		filters[i] = Filter{
			ResourceType:  "affiliate",
			FilterIDValue: id,
		}
	}

	request := ClicksRequest{
		TimezoneID: c.timezoneID,
		From:       from,
		To:         to,
		Query: ClicksQuery{
			Filters:       filters,
			UserMetrics:   []interface{}{},
			Exclusions:    []interface{}{},
			MetricFilters: []interface{}{},
			Settings: ClicksSettings{
				CampaignDataOnly:       false,
				IgnoreFailTraffic:      false,
				OnlyIncludeFailTraffic: false,
			},
		},
	}

	respBody, err := c.doRequest(ctx, http.MethodPost, "/v1/networks/reporting/clicks", request)
	if err != nil {
		return nil, err
	}

	var response ClicksResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		// Try parsing as array directly
		var clicks []ClickRecord
		if err2 := json.Unmarshal(respBody, &clicks); err2 == nil {
			return clicks, nil
		}
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return response.Clicks, nil
}

// GetConversions retrieves conversion data for a date range with pagination
func (c *Client) GetConversions(ctx context.Context, from, to string, affiliateIDs []string, approvedOnly bool) ([]ConversionRecord, error) {
	if len(affiliateIDs) == 0 {
		affiliateIDs = c.affiliateIDs
	}

	// Build filters
	filters := make([]Filter, 0, len(affiliateIDs)+1)
	
	if approvedOnly {
		filters = append(filters, Filter{
			ResourceType:  "status",
			FilterIDValue: "approved",
		})
	}

	for _, id := range affiliateIDs {
		filters = append(filters, Filter{
			ResourceType:  "affiliate",
			FilterIDValue: id,
		})
	}

	var allConversions []ConversionRecord
	page := 1
	pageSize := 50 // API max is 50

	for {
		request := ConversionsRequest{
			TimezoneID:          c.timezoneID,
			CurrencyID:          c.currencyID,
			From:                from,
			To:                  to,
			ShowEvents:          true,
			ShowConversions:     true,
			ShowOnlyVT:          false,
			ShowOnlyFailTraffic: false,
			ShowOnlyScrub:       false,
			ShowOnlyCT:          false,
			Query: ConversionsQuery{
				Filters:     filters,
				SearchTerms: []string{},
			},
		}

		// Use URL query params for pagination
		endpoint := fmt.Sprintf("/v1/networks/reporting/conversions?page=%d&page_size=%d", page, pageSize)
		respBody, err := c.doRequest(ctx, http.MethodPost, endpoint, request)
		if err != nil {
			return nil, err
		}

		var response ConversionsResponsePaged
		if err := json.Unmarshal(respBody, &response); err != nil {
			// Try parsing as array directly (legacy format)
			var conversions []ConversionRecord
			if err2 := json.Unmarshal(respBody, &conversions); err2 == nil {
				return conversions, nil
			}
			return nil, fmt.Errorf("failed to parse response: %w", err)
		}

		allConversions = append(allConversions, response.Conversions...)

		// Check if there are more pages
		if response.Paging == nil || len(response.Conversions) < pageSize || 
		   (page * pageSize) >= response.Paging.TotalCount {
			break
		}
		page++
		
		// Safety limit
		if page > 100 {
			break
		}
	}

	return allConversions, nil
}

// GetClicksForDate retrieves clicks for a specific date
func (c *Client) GetClicksForDate(ctx context.Context, date time.Time) ([]ClickRecord, error) {
	from := date.Format("2006-01-02 00:00:00")
	to := date.Format("2006-01-02 23:59:59")
	return c.GetClicks(ctx, from, to, nil)
}

// GetConversionsForDate retrieves conversions for a specific date
func (c *Client) GetConversionsForDate(ctx context.Context, date time.Time, approvedOnly bool) ([]ConversionRecord, error) {
	from := date.Format("2006-01-02")
	to := date.Format("2006-01-02")
	return c.GetConversions(ctx, from, to, nil, approvedOnly)
}

// GetClicksForDateRange retrieves clicks for a date range
func (c *Client) GetClicksForDateRange(ctx context.Context, startDate, endDate time.Time) ([]ClickRecord, error) {
	from := startDate.Format("2006-01-02 00:00:00")
	to := endDate.Format("2006-01-02 23:59:59")
	return c.GetClicks(ctx, from, to, nil)
}

// GetConversionsForDateRange retrieves conversions for a date range
func (c *Client) GetConversionsForDateRange(ctx context.Context, startDate, endDate time.Time, approvedOnly bool) ([]ConversionRecord, error) {
	from := startDate.Format("2006-01-02")
	to := endDate.Format("2006-01-02")
	return c.GetConversions(ctx, from, to, nil, approvedOnly)
}

// GetEntityReportByDate retrieves aggregated daily stats with clicks from the entity reporting API
func (c *Client) GetEntityReportByDate(ctx context.Context, startDate, endDate time.Time, affiliateIDs []string) (*EntityReportResponse, error) {
	if len(affiliateIDs) == 0 {
		affiliateIDs = c.affiliateIDs
	}

	// Build filters
	filters := make([]Filter, 0, len(affiliateIDs))
	for _, id := range affiliateIDs {
		filters = append(filters, Filter{
			ResourceType:  "affiliate",
			FilterIDValue: id,
		})
	}

	request := EntityReportRequest{
		TimezoneID: c.timezoneID,
		CurrencyID: c.currencyID,
		From:       startDate.Format("2006-01-02"),
		To:         endDate.Format("2006-01-02"),
		Columns: []EntityReportColumn{
			{Column: "date"},
		},
		Query: EntityReportQuery{
			Filters: filters,
		},
	}

	respBody, err := c.doRequest(ctx, http.MethodPost, "/v1/networks/reporting/entity", request)
	if err != nil {
		return nil, err
	}

	var response EntityReportResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse entity report response: %w", err)
	}

	return &response, nil
}

// GetEntityReportByOffer retrieves aggregated stats by offer with clicks
func (c *Client) GetEntityReportByOffer(ctx context.Context, startDate, endDate time.Time, affiliateIDs []string) (*EntityReportResponse, error) {
	if len(affiliateIDs) == 0 {
		affiliateIDs = c.affiliateIDs
	}

	filters := make([]Filter, 0, len(affiliateIDs))
	for _, id := range affiliateIDs {
		filters = append(filters, Filter{
			ResourceType:  "affiliate",
			FilterIDValue: id,
		})
	}

	request := EntityReportRequest{
		TimezoneID: c.timezoneID,
		CurrencyID: c.currencyID,
		From:       startDate.Format("2006-01-02"),
		To:         endDate.Format("2006-01-02"),
		Columns: []EntityReportColumn{
			{Column: "offer"},
		},
		Query: EntityReportQuery{
			Filters: filters,
		},
	}

	respBody, err := c.doRequest(ctx, http.MethodPost, "/v1/networks/reporting/entity", request)
	if err != nil {
		return nil, err
	}

	var response EntityReportResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse entity report response: %w", err)
	}

	return &response, nil
}

// GetEntityReportByOfferAndSub2 retrieves aggregated stats by offer × sub2 (cross-tab).
// Each row represents a unique (offer, sub2) combination with click counts, revenue, etc.
// This is used for CPM revenue attribution: we need to know how many clicks each partner
// (identified by sub2) generated on each CPM offer.
func (c *Client) GetEntityReportByOfferAndSub2(ctx context.Context, startDate, endDate time.Time, affiliateIDs []string) (*EntityReportResponse, error) {
	if len(affiliateIDs) == 0 {
		affiliateIDs = c.affiliateIDs
	}

	filters := make([]Filter, 0, len(affiliateIDs))
	for _, id := range affiliateIDs {
		filters = append(filters, Filter{
			ResourceType:  "affiliate",
			FilterIDValue: id,
		})
	}

	request := EntityReportRequest{
		TimezoneID: c.timezoneID,
		CurrencyID: c.currencyID,
		From:       startDate.Format("2006-01-02"),
		To:         endDate.Format("2006-01-02"),
		Columns: []EntityReportColumn{
			{Column: "offer"},
			{Column: "sub2"},
		},
		Query: EntityReportQuery{
			Filters: filters,
		},
	}

	respBody, err := c.doRequest(ctx, http.MethodPost, "/v1/networks/reporting/entity", request)
	if err != nil {
		return nil, err
	}

	var response EntityReportResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse entity report response: %w", err)
	}

	return &response, nil
}

// GetEntityReportBySub1 retrieves aggregated stats by sub1 (campaign/mailing ID) with clicks
func (c *Client) GetEntityReportBySub1(ctx context.Context, startDate, endDate time.Time, affiliateIDs []string) (*EntityReportResponse, error) {
	if len(affiliateIDs) == 0 {
		affiliateIDs = c.affiliateIDs
	}

	filters := make([]Filter, 0, len(affiliateIDs))
	for _, id := range affiliateIDs {
		filters = append(filters, Filter{
			ResourceType:  "affiliate",
			FilterIDValue: id,
		})
	}

	request := EntityReportRequest{
		TimezoneID: c.timezoneID,
		CurrencyID: c.currencyID,
		From:       startDate.Format("2006-01-02"),
		To:         endDate.Format("2006-01-02"),
		Columns: []EntityReportColumn{
			{Column: "sub1"},
		},
		Query: EntityReportQuery{
			Filters: filters,
		},
	}

	respBody, err := c.doRequest(ctx, http.MethodPost, "/v1/networks/reporting/entity", request)
	if err != nil {
		return nil, err
	}

	var response EntityReportResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse entity report response: %w", err)
	}

	return &response, nil
}

// GetEntityReportBySub2 retrieves aggregated stats by sub2 (data set code) with clicks
func (c *Client) GetEntityReportBySub2(ctx context.Context, startDate, endDate time.Time, affiliateIDs []string) (*EntityReportResponse, error) {
	if len(affiliateIDs) == 0 {
		affiliateIDs = c.affiliateIDs
	}

	filters := make([]Filter, 0, len(affiliateIDs))
	for _, id := range affiliateIDs {
		filters = append(filters, Filter{
			ResourceType:  "affiliate",
			FilterIDValue: id,
		})
	}

	request := EntityReportRequest{
		TimezoneID: c.timezoneID,
		CurrencyID: c.currencyID,
		From:       startDate.Format("2006-01-02"),
		To:         endDate.Format("2006-01-02"),
		Columns: []EntityReportColumn{
			{Column: "sub2"},
		},
		Query: EntityReportQuery{
			Filters: filters,
		},
	}

	respBody, err := c.doRequest(ctx, http.MethodPost, "/v1/networks/reporting/entity", request)
	if err != nil {
		return nil, err
	}

	var response EntityReportResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse entity report response: %w", err)
	}

	return &response, nil
}

// ProcessClickRecord converts a raw ClickRecord to a processed Click
func ProcessClickRecord(record ClickRecord) Click {
	click := Click{
		ClickID:       record.ClickID,
		TransactionID: record.TransactionID,
		AffiliateID:   record.AffiliateID,
		AffiliateName: record.AffiliateName,
		OfferID:       record.OfferID,
		OfferName:     record.OfferName,
		Sub1:          record.Sub1,
		Sub2:          record.Sub2,
		Sub3:          record.Sub3,
		IPAddress:     record.IPAddress,
		Device:        record.Device,
		Browser:       record.Browser,
		Country:       record.Country,
		Region:        record.Region,
		City:          record.City,
		IsFailed:      record.IsFailed,
	}

	// Parse timestamp
	if ts, err := ParseTimestamp(record.Timestamp); err == nil {
		click.Timestamp = ts
	}

	// Parse sub1
	if parsed, err := ParseSub1(record.Sub1); err == nil {
		click.PropertyCode = parsed.PropertyCode
		click.PropertyName = parsed.PropertyName
		click.MailingID = parsed.MailingID
		click.ParsedOfferID = parsed.OfferID
	}

	// Parse sub2 for data partner attribution
	if parsed := ParseSub2(record.Sub2); parsed != nil && !parsed.IsEmailHash {
		click.DataSetCode = parsed.DataSetCode
		click.DataPartner = parsed.PartnerName
	}

	return click
}

// ProcessConversionRecord converts a raw ConversionRecord to a processed Conversion
func ProcessConversionRecord(record ConversionRecord) Conversion {
	// Extract offer info from relationship if available
	offerID := record.OfferID
	offerName := record.OfferName
	affiliateID := record.AffiliateID
	affiliateName := record.AffiliateName
	advertiserID := record.AdvertiserID
	advertiserName := record.AdvertiserName

	if record.Relationship != nil {
		if record.Relationship.Offer != nil {
			offerID = fmt.Sprintf("%d", record.Relationship.Offer.NetworkOfferID)
			offerName = record.Relationship.Offer.Name
		}
		if record.Relationship.Affiliate != nil {
			affiliateID = fmt.Sprintf("%d", record.Relationship.Affiliate.NetworkAffiliateID)
			affiliateName = record.Relationship.Affiliate.Name
		}
		if record.Relationship.Advertiser != nil {
			advertiserID = fmt.Sprintf("%d", record.Relationship.Advertiser.NetworkAdvertiserID)
			advertiserName = record.Relationship.Advertiser.Name
		}
	}

	conv := Conversion{
		ConversionID:   record.ConversionID,
		TransactionID:  record.TransactionID,
		ClickID:        record.ClickID,
		AffiliateID:    affiliateID,
		AffiliateName:  affiliateName,
		OfferID:        offerID,
		OfferName:      offerName,
		AdvertiserID:   advertiserID,
		AdvertiserName: advertiserName,
		Status:         record.Status,
		EventName:      record.EventName,
		Revenue:        record.Revenue,
		Payout:         record.Payout,
		RevenueType:    record.RevenueType,
		PayoutType:     record.PayoutType,
		Currency:       record.Currency,
		Sub1:           record.Sub1,
		Sub2:           record.Sub2,
		Sub3:           record.Sub3,
		IPAddress:      record.ConversionUserIP,
		Device:         record.DeviceType,
		Browser:        record.Browser,
		Country:        record.Country,
		Region:         record.Region,
		City:           record.City,
	}

	// Parse Unix timestamps
	if record.ConversionUnixTimestamp > 0 {
		conv.ConversionTime = time.Unix(record.ConversionUnixTimestamp, 0)
	}
	if record.ClickUnixTimestamp > 0 {
		conv.ClickTime = time.Unix(record.ClickUnixTimestamp, 0)
	}

	// Parse sub1
	if parsed, err := ParseSub1(record.Sub1); err == nil {
		conv.PropertyCode = parsed.PropertyCode
		conv.PropertyName = parsed.PropertyName
		conv.MailingID = parsed.MailingID
		conv.ParsedOfferID = parsed.OfferID
	}

	// Parse sub2 for data partner attribution
	if parsed := ParseSub2(record.Sub2); parsed != nil && !parsed.IsEmailHash {
		conv.DataSetCode = parsed.DataSetCode
		conv.DataPartner = parsed.PartnerName
	}

	return conv
}

// ProcessClicks converts a slice of ClickRecords to processed Clicks
func ProcessClicks(records []ClickRecord) []Click {
	clicks := make([]Click, len(records))
	for i, r := range records {
		clicks[i] = ProcessClickRecord(r)
	}
	return clicks
}

// ProcessConversions converts a slice of ConversionRecords to processed Conversions
func ProcessConversions(records []ConversionRecord) []Conversion {
	conversions := make([]Conversion, len(records))
	for i, r := range records {
		conversions[i] = ProcessConversionRecord(r)
	}
	return conversions
}

// HealthCheck performs a simple API health check
func (c *Client) HealthCheck(ctx context.Context) error {
	// Try to fetch today's data with a small query
	today := time.Now()
	_, err := c.GetConversionsForDate(ctx, today, true)
	return err
}

// GetAffiliateIDs returns the configured affiliate IDs
func (c *Client) GetAffiliateIDs() []string {
	return c.affiliateIDs
}

// ========== Network-Wide Methods (No Affiliate Filter) ==========
// These methods query the ENTIRE Everflow network, not filtered by affiliate ID.
// Used for building network intelligence and understanding what's converting globally.

// GetEntityReportByOfferNetworkWide retrieves aggregated stats by offer across the ENTIRE network
// No affiliate filter is applied - this shows all offer performance across all affiliates
func (c *Client) GetEntityReportByOfferNetworkWide(ctx context.Context, startDate, endDate time.Time) (*EntityReportResponse, error) {
	request := EntityReportRequest{
		TimezoneID: c.timezoneID,
		CurrencyID: c.currencyID,
		From:       startDate.Format("2006-01-02"),
		To:         endDate.Format("2006-01-02"),
		Columns: []EntityReportColumn{
			{Column: "offer"},
		},
		Query: EntityReportQuery{
			Filters: []Filter{}, // No affiliate filter - entire network
		},
	}

	respBody, err := c.doRequest(ctx, http.MethodPost, "/v1/networks/reporting/entity", request)
	if err != nil {
		return nil, err
	}

	var response EntityReportResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse network-wide entity report: %w", err)
	}

	return &response, nil
}

// GetEntityReportByDateNetworkWide retrieves daily stats across the ENTIRE network
func (c *Client) GetEntityReportByDateNetworkWide(ctx context.Context, startDate, endDate time.Time) (*EntityReportResponse, error) {
	request := EntityReportRequest{
		TimezoneID: c.timezoneID,
		CurrencyID: c.currencyID,
		From:       startDate.Format("2006-01-02"),
		To:         endDate.Format("2006-01-02"),
		Columns: []EntityReportColumn{
			{Column: "date"},
		},
		Query: EntityReportQuery{
			Filters: []Filter{}, // No affiliate filter
		},
	}

	respBody, err := c.doRequest(ctx, http.MethodPost, "/v1/networks/reporting/entity", request)
	if err != nil {
		return nil, err
	}

	var response EntityReportResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse network-wide date report: %w", err)
	}

	return &response, nil
}

// GetClicksNetworkWide retrieves click data across the ENTIRE network (no affiliate filter)
func (c *Client) GetClicksNetworkWide(ctx context.Context, from, to string) ([]ClickRecord, error) {
	request := ClicksRequest{
		TimezoneID: c.timezoneID,
		From:       from,
		To:         to,
		Query: ClicksQuery{
			Filters:       []Filter{}, // No affiliate filter
			UserMetrics:   []interface{}{},
			Exclusions:    []interface{}{},
			MetricFilters: []interface{}{},
			Settings: ClicksSettings{
				CampaignDataOnly:       false,
				IgnoreFailTraffic:      false,
				OnlyIncludeFailTraffic: false,
			},
		},
	}

	respBody, err := c.doRequest(ctx, http.MethodPost, "/v1/networks/reporting/clicks", request)
	if err != nil {
		return nil, err
	}

	var response ClicksResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		var clicks []ClickRecord
		if err2 := json.Unmarshal(respBody, &clicks); err2 == nil {
			return clicks, nil
		}
		return nil, fmt.Errorf("failed to parse network-wide clicks: %w", err)
	}

	return response.Clicks, nil
}

// GetConversionsNetworkWide retrieves conversion data across the ENTIRE network (no affiliate filter)
func (c *Client) GetConversionsNetworkWide(ctx context.Context, from, to string) ([]ConversionRecord, error) {
	request := ConversionsRequest{
		TimezoneID:          c.timezoneID,
		CurrencyID:          c.currencyID,
		From:                from,
		To:                  to,
		ShowEvents:          true,
		ShowConversions:     true,
		ShowOnlyVT:          false,
		ShowOnlyFailTraffic: false,
		ShowOnlyScrub:       false,
		ShowOnlyCT:          false,
		Query: ConversionsQuery{
			Filters:     []Filter{}, // No affiliate filter
			SearchTerms: []string{},
		},
	}

	var allConversions []ConversionRecord
	page := 1
	pageSize := 50

	for {
		endpoint := fmt.Sprintf("/v1/networks/reporting/conversions?page=%d&page_size=%d", page, pageSize)
		respBody, err := c.doRequest(ctx, http.MethodPost, endpoint, request)
		if err != nil {
			return nil, err
		}

		var response ConversionsResponsePaged
		if err := json.Unmarshal(respBody, &response); err != nil {
			var conversions []ConversionRecord
			if err2 := json.Unmarshal(respBody, &conversions); err2 == nil {
				return conversions, nil
			}
			return nil, fmt.Errorf("failed to parse network-wide conversions: %w", err)
		}

		allConversions = append(allConversions, response.Conversions...)

		if response.Paging == nil || len(response.Conversions) < pageSize ||
			(page*pageSize) >= response.Paging.TotalCount {
			break
		}
		page++

		if page > 100 {
			break
		}
	}

	return allConversions, nil
}
