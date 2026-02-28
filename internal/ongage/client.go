package ongage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/httpretry"
)

// Client is the Ongage API client
type Client struct {
	baseURL     string
	username    string
	password    string
	accountCode string
	listID      string
	httpClient  httpretry.HTTPDoer
}

// NewClient creates a new Ongage API client
func NewClient(config Config) *Client {
	return &Client{
		baseURL:     config.BaseURL,
		username:    config.Username,
		password:    config.Password,
		accountCode: config.AccountCode,
		listID:      config.ListID,
		httpClient: httpretry.NewRetryClient(&http.Client{
			Timeout: 60 * time.Second,
		}, 3),
	}
}

// SetHTTPClient sets a custom HTTP client (useful for testing)
func (c *Client) SetHTTPClient(client httpretry.HTTPDoer) {
	c.httpClient = client
}

// doRequest performs an authenticated request to the Ongage API
func (c *Client) doRequest(ctx context.Context, method, endpoint string, body interface{}) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewBuffer(jsonBody)
	}

	// Build URL - handle list_id in path if needed
	reqURL := fmt.Sprintf("%s%s", c.baseURL, endpoint)

	req, err := http.NewRequestWithContext(ctx, method, reqURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set authentication headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X_USERNAME", c.username)
	req.Header.Set("X_PASSWORD", c.password)
	req.Header.Set("X_ACCOUNT_CODE", c.accountCode)
	if c.listID != "" {
		req.Header.Set("X_LIST_ID", c.listID)
	}

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

// ========== Campaign/Mailing Methods ==========

// GetCampaigns retrieves all campaigns with optional filters
func (c *Client) GetCampaigns(ctx context.Context, dateFrom, dateTo *time.Time, limit, offset int) ([]Campaign, error) {
	endpoint := "/api/mailings"
	
	// Build query parameters
	params := url.Values{}
	if dateFrom != nil {
		params.Set("date_from", strconv.FormatInt(dateFrom.Unix(), 10))
	}
	if dateTo != nil {
		params.Set("date_to", strconv.FormatInt(dateTo.Unix(), 10))
	}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		params.Set("offset", strconv.Itoa(offset))
	}
	params.Set("is_test", "false") // Exclude test campaigns by default
	
	if len(params) > 0 {
		endpoint = endpoint + "?" + params.Encode()
	}

	respBody, err := c.doRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	var response CampaignListResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if response.Metadata.Error {
		return nil, fmt.Errorf("API returned error")
	}

	return response.Payload, nil
}

// GetCampaign retrieves a single campaign by ID
func (c *Client) GetCampaign(ctx context.Context, campaignID string) (*Campaign, error) {
	endpoint := fmt.Sprintf("/api/mailings/%s", campaignID)

	respBody, err := c.doRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	var response CampaignDetailResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if response.Metadata.Error {
		return nil, fmt.Errorf("API returned error")
	}

	return &response.Payload, nil
}

// GetRecentCampaigns gets campaigns from the last N days
func (c *Client) GetRecentCampaigns(ctx context.Context, days int) ([]Campaign, error) {
	now := time.Now()
	dateFrom := now.AddDate(0, 0, -days)
	return c.GetCampaigns(ctx, &dateFrom, &now, 1000, 0)
}

// ========== Report Methods ==========

// QueryReports executes a report query
func (c *Client) QueryReports(ctx context.Context, query ReportQuery) ([]ReportRow, error) {
	endpoint := "/api/reports/query"

	respBody, err := c.doRequest(ctx, http.MethodPost, endpoint, query)
	if err != nil {
		return nil, err
	}

	var response ReportResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if response.Metadata.Error {
		return nil, fmt.Errorf("API returned error")
	}

	return response.Payload, nil
}

// GetCampaignStats retrieves aggregated campaign statistics
func (c *Client) GetCampaignStats(ctx context.Context, daysBack int) ([]ReportRow, error) {
	now := time.Now()
	dateFrom := now.AddDate(0, 0, -daysBack)

	query := ReportQuery{
		Select: []interface{}{
			"mailing_id",
			"mailing_name",
			"email_message_subject",
			[]string{"MAX(`schedule_date`)", "schedule_date"},
			[]string{"MAX(`stats_date`)", "stats_date"},
			"esp_name",
			"esp_connection_id",
			"esp_connection_title",
			"segment_name",
			"sum(`targeted`)",
			"sum(`sent`)",
			"sum(`success`)",
			"sum(`failed`)",
			"sum(`opens`)",
			"sum(`unique_opens`)",
			"sum(`clicks`)",
			"sum(`unique_clicks`)",
			"sum(`unsubscribes`)",
			"sum(`complaints`)",
			"sum(`hard_bounces`)",
			"sum(`soft_bounces`)",
		},
		From:   "mailing",
		Group:  []interface{}{"mailing_id"},
		Order:  []interface{}{[]string{"schedule_date", "DESC"}},
		Filter: [][]interface{}{
			{"is_test_campaign", "=", 0},
			{"stats_date", ">=", dateFrom.Format("2006-01-02")},
		},
		ListIDs:        "all",
		CalculateRates: true,
	}

	return c.QueryReports(ctx, query)
}

// GetCampaignStatsByESP retrieves campaign stats grouped by ESP
func (c *Client) GetCampaignStatsByESP(ctx context.Context, daysBack int) ([]ReportRow, error) {
	now := time.Now()
	dateFrom := now.AddDate(0, 0, -daysBack)

	query := ReportQuery{
		Select: []interface{}{
			"esp_name",
			"esp_connection_id",
			"esp_connection_title",
			"sum(`targeted`)",
			"sum(`sent`)",
			"sum(`success`)",
			"sum(`failed`)",
			"sum(`opens`)",
			"sum(`unique_opens`)",
			"sum(`clicks`)",
			"sum(`unique_clicks`)",
			"sum(`unsubscribes`)",
			"sum(`complaints`)",
			"sum(`hard_bounces`)",
			"sum(`soft_bounces`)",
		},
		From:   "mailing",
		Group:  []interface{}{"esp_connection_id"},
		Order:  []interface{}{[]string{"sum(`sent`)", "DESC"}},
		Filter: [][]interface{}{
			{"is_test_campaign", "=", 0},
			{"stats_date", ">=", dateFrom.Format("2006-01-02")},
		},
		ListIDs:        "all",
		CalculateRates: true,
	}

	return c.QueryReports(ctx, query)
}

// GetCampaignStatsByHour retrieves campaign stats grouped by schedule date for schedule optimization
// Note: Ongage API doesn't support HOUR/DAYOFWEEK functions, so we get stats by date and process locally
func (c *Client) GetCampaignStatsByHour(ctx context.Context, daysBack int) ([]ReportRow, error) {
	now := time.Now()
	dateFrom := now.AddDate(0, 0, -daysBack)

	query := ReportQuery{
		Select: []interface{}{
			"schedule_date",
			"mailing_id",
			"sum(`sent`)",
			"sum(`success`)",
			"sum(`opens`)",
			"sum(`unique_opens`)",
			"sum(`clicks`)",
			"sum(`unique_clicks`)",
		},
		From:   "mailing",
		Group:  []interface{}{"mailing_id"},
		Order:  []interface{}{[]string{"schedule_date", "DESC"}},
		Filter: [][]interface{}{
			{"is_test_campaign", "=", 0},
			{"stats_date", ">=", dateFrom.Format("2006-01-02")},
		},
		ListIDs:        "all",
		CalculateRates: true,
	}

	return c.QueryReports(ctx, query)
}

// GetCampaignStatsBySegment retrieves campaign stats grouped by segment
func (c *Client) GetCampaignStatsBySegment(ctx context.Context, daysBack int) ([]ReportRow, error) {
	now := time.Now()
	dateFrom := now.AddDate(0, 0, -daysBack)

	query := ReportQuery{
		Select: []interface{}{
			"segment_id",
			"segment_name",
			"sum(`targeted`)",
			"sum(`sent`)",
			"sum(`success`)",
			"sum(`opens`)",
			"sum(`unique_opens`)",
			"sum(`clicks`)",
			"sum(`unique_clicks`)",
			"sum(`complaints`)",
			"sum(`hard_bounces`)",
			"sum(`soft_bounces`)",
		},
		From:   "mailing",
		Group:  []interface{}{"segment_id"},
		Order:  []interface{}{[]string{"sum(`sent`)", "DESC"}},
		Filter: [][]interface{}{
			{"is_test_campaign", "=", 0},
			{"stats_date", ">=", dateFrom.Format("2006-01-02")},
		},
		ListIDs:        "all",
		CalculateRates: true,
	}

	return c.QueryReports(ctx, query)
}

// GetCampaignStatsByISP retrieves campaign stats grouped by ISP/domain
func (c *Client) GetCampaignStatsByISP(ctx context.Context, daysBack int) ([]ReportRow, error) {
	now := time.Now()
	dateFrom := now.AddDate(0, 0, -daysBack)

	query := ReportQuery{
		Select: []interface{}{
			"isp_name",
			"isp_id",
			"sum(`sent`)",
			"sum(`success`)",
			"sum(`opens`)",
			"sum(`unique_opens`)",
			"sum(`clicks`)",
			"sum(`unique_clicks`)",
			"sum(`complaints`)",
			"sum(`hard_bounces`)",
			"sum(`soft_bounces`)",
		},
		From:   "mailing",
		Group:  []interface{}{"isp_id"},
		Order:  []interface{}{[]string{"sum(`sent`)", "DESC"}},
		Filter: [][]interface{}{
			{"is_test_campaign", "=", 0},
			{"stats_date", ">=", dateFrom.Format("2006-01-02")},
		},
		ListIDs:        "all",
		CalculateRates: true,
	}

	return c.QueryReports(ctx, query)
}

// GetDailyStats retrieves daily aggregated stats for pipeline tracking
func (c *Client) GetDailyStats(ctx context.Context, daysBack int) ([]ReportRow, error) {
	now := time.Now()
	dateFrom := now.AddDate(0, 0, -daysBack)

	query := ReportQuery{
		Select: []interface{}{
			"stats_date",
			"sum(`targeted`)",
			"sum(`sent`)",
			"sum(`success`)",
			"sum(`opens`)",
			"sum(`unique_opens`)",
			"sum(`clicks`)",
			"sum(`unique_clicks`)",
			"sum(`complaints`)",
			"sum(`unsubscribes`)",
		},
		From:   "mailing",
		Group:  []interface{}{[]string{"stats_date", "day"}},
		Order:  []interface{}{[]string{"stats_date", "DESC"}},
		Filter: [][]interface{}{
			{"is_test_campaign", "=", 0},
			{"stats_date", ">=", dateFrom.Format("2006-01-02")},
		},
		ListIDs:        "all",
		CalculateRates: true,
	}

	return c.QueryReports(ctx, query)
}

// ========== ESP Connection Methods ==========

// GetESPConnections retrieves all ESP connections
func (c *Client) GetESPConnections(ctx context.Context, activeOnly bool) ([]ESPConnection, error) {
	endpoint := "/api/esp_connections/options"
	if activeOnly {
		endpoint += "?active_only=true"
	}

	respBody, err := c.doRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	var response ESPConnectionResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if response.Metadata.Error {
		return nil, fmt.Errorf("API returned error")
	}

	return response.Payload, nil
}

// ========== List Metadata Methods ==========

// GetLists retrieves all lists with their IDs and names.
// Used to map list_id from reports to data set codes.
func (c *Client) GetLists(ctx context.Context) ([]ListInfo, error) {
	respBody, err := c.doRequest(ctx, http.MethodGet, "/api/lists", nil)
	if err != nil {
		return nil, err
	}

	var response ListInfoResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse lists response: %w", err)
	}

	if response.Metadata.Error {
		return nil, fmt.Errorf("API returned error fetching lists")
	}

	return response.Payload, nil
}

// ========== List Stats Methods ==========

// GetListStats retrieves list-level statistics
func (c *Client) GetListStats(ctx context.Context, daysBack int) ([]ReportRow, error) {
	now := time.Now()
	dateFrom := now.AddDate(0, 0, -daysBack)

	query := ReportQuery{
		Select: []interface{}{
			"record_date",
			"active",
			"not_active",
			"complaints",
			"unsubscribes",
			"bounces",
			"opened",
			"clicked",
			"no_activity",
		},
		From:  "list",
		Group: []interface{}{[]string{"record_date", "day"}},
		Order: []interface{}{[]string{"record_date", "DESC"}},
		Filter: [][]interface{}{
			{"record_date", ">=", dateFrom.Format("2006-01-02")},
		},
		ListIDs: "all",
	}

	return c.QueryReports(ctx, query)
}

// GetSendsByList retrieves sending volume grouped by list_id
// Returns one row per list with sum(sent) and sum(success).
// Used to derive exact per-data-set sending volume for data partner analytics.
func (c *Client) GetSendsByList(ctx context.Context, daysBack int) ([]ReportRow, error) {
	now := time.Now()
	dateFrom := now.AddDate(0, 0, -daysBack)

	query := ReportQuery{
		Select: []interface{}{
			"list_id",
			"sum(`sent`)",
			"sum(`success`)",
		},
		From:   "mailing",
		Group:  []interface{}{"list_id"},
		Order:  []interface{}{[]string{"sum(`sent`)", "DESC"}},
		Filter: [][]interface{}{
			{"is_test_campaign", "=", 0},
			{"stats_date", ">=", dateFrom.Format("2006-01-02")},
		},
		ListIDs: "all",
	}

	return c.QueryReports(ctx, query)
}

// GetSendsByListForDateRange retrieves sending volume grouped by list_id for an
// explicit date range. This allows volume queries to respect the global date filter.
func (c *Client) GetSendsByListForDateRange(ctx context.Context, from, to time.Time) ([]ReportRow, error) {
	query := ReportQuery{
		Select: []interface{}{
			"list_id",
			"sum(`sent`)",
			"sum(`success`)",
		},
		From:   "mailing",
		Group:  []interface{}{"list_id"},
		Order:  []interface{}{[]string{"sum(`sent`)", "DESC"}},
		Filter: [][]interface{}{
			{"is_test_campaign", "=", 0},
			{"stats_date", ">=", from.Format("2006-01-02")},
			{"stats_date", "<=", to.Format("2006-01-02")},
		},
		ListIDs: "all",
	}

	return c.QueryReports(ctx, query)
}

// GetDailyStatsForDateRange retrieves daily pipeline stats for an explicit date range.
func (c *Client) GetDailyStatsForDateRange(ctx context.Context, from, to time.Time) ([]ReportRow, error) {
	query := ReportQuery{
		Select: []interface{}{
			"stats_date",
			"sum(`targeted`)",
			"sum(`sent`)",
			"sum(`success`)",
			"sum(`opens`)",
			"sum(`unique_opens`)",
			"sum(`clicks`)",
			"sum(`unique_clicks`)",
			"sum(`complaints`)",
			"sum(`unsubscribes`)",
		},
		From:   "mailing",
		Group:  []interface{}{[]string{"stats_date", "day"}},
		Order:  []interface{}{[]string{"stats_date", "DESC"}},
		Filter: [][]interface{}{
			{"is_test_campaign", "=", 0},
			{"stats_date", ">=", from.Format("2006-01-02")},
			{"stats_date", "<=", to.Format("2006-01-02")},
		},
		ListIDs: "all",
	}

	return c.QueryReports(ctx, query)
}

// GetSendsBySegment retrieves sending volume grouped by segment_id
// Returns one row per segment with sum(sent), segment_id, and segment_name.
// Used as an alternative volume source for data partner analytics if
// segments are organized by data partner/data set code.
func (c *Client) GetSendsBySegment(ctx context.Context, daysBack int) ([]ReportRow, error) {
	now := time.Now()
	dateFrom := now.AddDate(0, 0, -daysBack)

	query := ReportQuery{
		Select: []interface{}{
			"segment_id",
			"segment_name",
			"sum(`sent`)",
			"sum(`success`)",
		},
		From:   "mailing",
		Group:  []interface{}{"segment_id"},
		Order:  []interface{}{[]string{"sum(`sent`)", "DESC"}},
		Filter: [][]interface{}{
			{"is_test_campaign", "=", 0},
			{"stats_date", ">=", dateFrom.Format("2006-01-02")},
		},
		ListIDs: "all",
	}

	return c.QueryReports(ctx, query)
}

// GetSendsBySegmentForDateRange retrieves sending volume grouped by segment_id
// for a specific date range. Segment names may contain data set codes (e.g.,
// "M77_WIT_OPENERS", "ATT_30DC_ALL") which can be parsed to derive per-data-set
// volume dynamically.
func (c *Client) GetSendsBySegmentForDateRange(ctx context.Context, from, to time.Time) ([]ReportRow, error) {
	query := ReportQuery{
		Select: []interface{}{
			"segment_id",
			"segment_name",
			"sum(`sent`)",
			"sum(`success`)",
		},
		From:   "mailing",
		Group:  []interface{}{"segment_id"},
		Order:  []interface{}{[]string{"sum(`sent`)", "DESC"}},
		Filter: [][]interface{}{
			{"is_test_campaign", "=", 0},
			{"stats_date", ">=", from.Format("2006-01-02")},
			{"stats_date", "<=", to.Format("2006-01-02")},
		},
		ListIDs: "all",
	}

	return c.QueryReports(ctx, query)
}

// ========== Contact Activity Methods ==========

// CreateContactActivityReport creates an asynchronous contact activity report.
// The report filters contacts by the given criteria and includes the specified fields.
// Returns the report ID which can be used to poll status and export results.
func (c *Client) CreateContactActivityReport(ctx context.Context, req ContactActivityRequest) (string, error) {
	respBody, err := c.doRequest(ctx, http.MethodPost, "/api/contact_activity", req)
	if err != nil {
		return "", fmt.Errorf("failed to create contact activity report: %w", err)
	}

	var response ContactActivityCreateResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return "", fmt.Errorf("failed to parse contact activity response: %w", err)
	}

	if response.Metadata.Error {
		return "", fmt.Errorf("API error creating contact activity report")
	}

	reportID := response.Payload.ID.String()
	if reportID == "" || reportID == "0" {
		return "", fmt.Errorf("contact activity report created but no ID returned")
	}

	return reportID, nil
}

// GetContactActivityStatus checks the status of a contact activity report.
// Returns status: 1 = Pending, 2 = Completed.
func (c *Client) GetContactActivityStatus(ctx context.Context, reportID string) (int, error) {
	endpoint := fmt.Sprintf("/api/contact_activity/%s", reportID)
	respBody, err := c.doRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to check contact activity status: %w", err)
	}

	var response ContactActivityStatusResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return 0, fmt.Errorf("failed to parse contact activity status: %w", err)
	}

	if response.Metadata.Error {
		return 0, fmt.Errorf("API error checking contact activity status")
	}

	return response.Payload.Status, nil
}

// ExportContactActivityCSV retrieves the aggregated CSV export of a completed
// contact activity report. The report must be in Completed status (2).
func (c *Client) ExportContactActivityCSV(ctx context.Context, reportID string) ([]byte, error) {
	endpoint := fmt.Sprintf("/api/contact_activity/%s/export", reportID)
	csvData, err := c.doRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to export contact activity CSV: %w", err)
	}
	return csvData, nil
}

// DeleteContactActivityReport deletes a contact activity report (cleanup).
func (c *Client) DeleteContactActivityReport(ctx context.Context, reportID string) error {
	endpoint := fmt.Sprintf("/api/contact_activity/%s", reportID)
	_, err := c.doRequest(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to delete contact activity report: %w", err)
	}
	return nil
}

// ========== Health Check ==========

// HealthCheck performs a simple API health check
func (c *Client) HealthCheck(ctx context.Context) error {
	_, err := c.GetESPConnections(ctx, false)
	return err
}

// ========== Import API ==========

// GetImports retrieves all imports with optional filters
func (c *Client) GetImports(ctx context.Context, limit, offset int) ([]Import, error) {
	endpoint := "/api/import"
	
	if limit > 0 || offset > 0 {
		params := ""
		if limit > 0 {
			params += fmt.Sprintf("limit=%d", limit)
		}
		if offset > 0 {
			if params != "" {
				params += "&"
			}
			params += fmt.Sprintf("offset=%d", offset)
		}
		endpoint += "?" + params
	}
	
	respBody, err := c.doRequest(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	
	var response ImportResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse imports response: %w", err)
	}
	
	if response.Metadata.Error {
		return nil, fmt.Errorf("API error fetching imports")
	}
	
	return response.Payload, nil
}

// GetImport retrieves a single import by ID
func (c *Client) GetImport(ctx context.Context, importID string) (*Import, error) {
	endpoint := fmt.Sprintf("/api/import/%s", importID)
	
	respBody, err := c.doRequest(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	
	var response SingleImportResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse import response: %w", err)
	}
	
	if response.Metadata.Error {
		return nil, fmt.Errorf("API error fetching import %s", importID)
	}
	
	return &response.Payload, nil
}

// GetTodayImports retrieves imports from today
func (c *Client) GetTodayImports(ctx context.Context) ([]Import, error) {
	// Get all imports and filter by today's date
	imports, err := c.GetImports(ctx, 100, 0)
	if err != nil {
		return nil, err
	}
	
	today := time.Now().Format("2006-01-02")
	var todayImports []Import
	
	for _, imp := range imports {
		// Parse created timestamp
		created, err := ParseUnixTimestamp(imp.Created)
		if err != nil {
			continue
		}
		
		if created.Format("2006-01-02") == today {
			todayImports = append(todayImports, imp)
		}
	}
	
	return todayImports, nil
}

// GetRecentImports retrieves imports from the last N days
func (c *Client) GetRecentImports(ctx context.Context, days int) ([]Import, error) {
	// Get all imports and filter by date
	imports, err := c.GetImports(ctx, 500, 0)
	if err != nil {
		return nil, err
	}
	
	cutoff := time.Now().AddDate(0, 0, -days)
	var recentImports []Import
	
	for _, imp := range imports {
		// Parse created timestamp
		created, err := ParseUnixTimestamp(imp.Created)
		if err != nil {
			continue
		}
		
		if created.After(cutoff) {
			recentImports = append(recentImports, imp)
		}
	}
	
	return recentImports, nil
}

// GetImportMetrics calculates aggregated import metrics
func (c *Client) GetImportMetrics(ctx context.Context, imports []Import) ImportMetrics {
	metrics := ImportMetrics{}
	
	today := time.Now().Format("2006-01-02")
	
	for _, imp := range imports {
		metrics.TotalImports++
		
		// Check if today
		created, err := ParseUnixTimestamp(imp.Created)
		if err == nil && created.Format("2006-01-02") == today {
			metrics.TodayImports++
		}
		
		// Parse counts
		if total, err := strconv.ParseInt(imp.Total, 10, 64); err == nil {
			metrics.TotalRecords += total
		}
		if success, err := strconv.ParseInt(imp.Success, 10, 64); err == nil {
			metrics.SuccessRecords += success
		}
		if failed, err := strconv.ParseInt(imp.Failed, 10, 64); err == nil {
			metrics.FailedRecords += failed
		}
		if duplicate, err := strconv.ParseInt(imp.Duplicate, 10, 64); err == nil {
			metrics.DuplicateRecords += duplicate
		}
		if existing, err := strconv.ParseInt(imp.Existing, 10, 64); err == nil {
			metrics.ExistingRecords += existing
		}
		
		// Check status
		if imp.StatusDesc != "" && (strings.Contains(imp.StatusDesc, "Processing") || imp.Status == "40002") {
			metrics.InProgress++
		} else if imp.Status == "40004" || strings.Contains(imp.StatusDesc, "Completed") {
			metrics.Completed++
		}
	}
	
	return metrics
}

// ========== Helper Methods ==========

// ParseUnixTimestamp parses a Unix timestamp string to time.Time
func ParseUnixTimestamp(ts string) (time.Time, error) {
	if ts == "" || ts == "0" {
		return time.Time{}, nil
	}
	
	unixTime, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		// Try parsing as date string
		return time.Parse("2006-01-02 15:04:05", ts)
	}
	
	return time.Unix(unixTime, 0), nil
}

// GetStatusDescription returns the human-readable status description
func GetStatusDescription(statusCode string) string {
	if desc, ok := StatusDescriptions[statusCode]; ok {
		return desc
	}
	return "Unknown"
}

// GetESPName returns the ESP vendor name from ESP ID
func GetESPName(espID string) string {
	if name, ok := ESPNames[espID]; ok {
		return name
	}
	return "Unknown ESP"
}

// MapESPToProvider maps Ongage ESP to our internal provider names
func MapESPToProvider(espID string) string {
	switch espID {
	case ESPIDAmazonSES:
		return "ses"
	case ESPIDMailgun:
		return "mailgun"
	case ESPIDSparkPost, ESPIDSparkPostEnterprise, ESPIDSparkPostMomentum:
		return "sparkpost"
	default:
		return "unknown"
	}
}
