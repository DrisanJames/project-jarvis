package azure

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Client provides access to Azure Table Storage
type Client struct {
	accountName    string
	accountKey     string
	endpointSuffix string
	tableName      string
	httpClient     *http.Client
}

// NewClient creates a new Azure Table Storage client
func NewClient(cfg Config) (*Client, error) {
	accountName, accountKey, endpointSuffix := cfg.ParseConnectionString()
	
	if accountName == "" || accountKey == "" {
		return nil, fmt.Errorf("invalid connection string: missing AccountName or AccountKey")
	}
	
	if endpointSuffix == "" {
		endpointSuffix = "core.windows.net"
	}
	
	tableName := cfg.TableName
	if tableName == "" {
		tableName = "ignitemediagroupcrm"
	}

	return &Client{
		accountName:    accountName,
		accountKey:     accountKey,
		endpointSuffix: endpointSuffix,
		tableName:      tableName,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}, nil
}

// getTableURL returns the full URL for the table
func (c *Client) getTableURL() string {
	return fmt.Sprintf("https://%s.table.%s/%s", c.accountName, c.endpointSuffix, c.tableName)
}

// generateAuthHeader creates the SharedKey authentication header
func (c *Client) generateAuthHeader(method, urlPath string, headers map[string]string, queryParams url.Values) string {
	// Build the string to sign for SharedKey authentication
	// Format: VERB\nContent-MD5\nContent-Type\nDate\nCanonicalizedResource
	
	date := headers["x-ms-date"]
	contentType := headers["Content-Type"]
	if contentType == "" {
		contentType = ""
	}
	
	// Build canonicalized resource
	canonicalizedResource := fmt.Sprintf("/%s/%s", c.accountName, c.tableName)
	if urlPath != "" && urlPath != "/" {
		canonicalizedResource += urlPath
	}
	
	// For Table service SharedKey authentication, only the "comp" query parameter
	// should be included in the canonicalized resource. OData query parameters
	// ($filter, $top, $select, etc.) should NOT be included in the signature.
	if comp := queryParams.Get("comp"); comp != "" {
		canonicalizedResource += fmt.Sprintf("\ncomp:%s", comp)
	}
	
	// String to sign for Table service
	stringToSign := fmt.Sprintf("%s\n\n%s\n%s\n%s",
		method,
		contentType,
		date,
		canonicalizedResource,
	)
	
	// Decode the account key
	keyBytes, _ := base64.StdEncoding.DecodeString(c.accountKey)
	
	// Create HMAC-SHA256 signature
	h := hmac.New(sha256.New, keyBytes)
	h.Write([]byte(stringToSign))
	signature := base64.StdEncoding.EncodeToString(h.Sum(nil))
	
	return fmt.Sprintf("SharedKey %s:%s", c.accountName, signature)
}

// QueryEntities queries entities from the table with optional filter
func (c *Client) QueryEntities(ctx context.Context, filter string, top int) ([]map[string]interface{}, error) {
	var allEntities []map[string]interface{}
	var continuationToken string
	
	for {
		entities, nextToken, err := c.queryEntitiesPage(ctx, filter, top, continuationToken)
		if err != nil {
			return nil, err
		}
		
		allEntities = append(allEntities, entities...)
		
		if nextToken == "" || (top > 0 && len(allEntities) >= top) {
			break
		}
		continuationToken = nextToken
	}
	
	return allEntities, nil
}

// queryEntitiesPage queries a single page of entities
func (c *Client) queryEntitiesPage(ctx context.Context, filter string, top int, continuationToken string) ([]map[string]interface{}, string, error) {
	baseURL := c.getTableURL()
	
	// Build query parameters
	params := url.Values{}
	if filter != "" {
		params.Set("$filter", filter)
	}
	if top > 0 {
		params.Set("$top", fmt.Sprintf("%d", top))
	}
	if continuationToken != "" {
		// Parse continuation token (NextPartitionKey;NextRowKey)
		parts := strings.Split(continuationToken, ";")
		if len(parts) >= 1 && parts[0] != "" {
			params.Set("NextPartitionKey", parts[0])
		}
		if len(parts) >= 2 && parts[1] != "" {
			params.Set("NextRowKey", parts[1])
		}
	}
	
	fullURL := baseURL + "()"
	if len(params) > 0 {
		fullURL += "?" + params.Encode()
	}
	
	req, err := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	if err != nil {
		return nil, "", err
	}
	
	// Set headers
	date := time.Now().UTC().Format(http.TimeFormat)
	req.Header.Set("x-ms-date", date)
	req.Header.Set("x-ms-version", "2019-02-02")
	req.Header.Set("Accept", "application/json;odata=nometadata")
	req.Header.Set("DataServiceVersion", "3.0;NetFx")
	
	// Generate auth header
	headers := map[string]string{
		"x-ms-date":    date,
		"Content-Type": "",
	}
	authHeader := c.generateAuthHeader("GET", "()", headers, params)
	req.Header.Set("Authorization", authHeader)
	
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("Azure Table query failed: %s - %s", resp.Status, string(body))
	}
	
	var response TableQueryResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, "", fmt.Errorf("failed to parse response: %w", err)
	}
	
	// Extract continuation token from headers
	nextToken := ""
	nextPK := resp.Header.Get("x-ms-continuation-NextPartitionKey")
	nextRK := resp.Header.Get("x-ms-continuation-NextRowKey")
	if nextPK != "" {
		nextToken = nextPK + ";" + nextRK
	}
	
	return response.Value, nextToken, nil
}

// GetPartitionKeys returns all unique partition keys (data set codes)
func (c *Client) GetPartitionKeys(ctx context.Context) ([]string, error) {
	// Query with select to get only PartitionKey
	// Note: Azure Table Storage doesn't support DISTINCT, so we need to handle deduplication
	entities, err := c.QueryEntities(ctx, "", 0)
	if err != nil {
		return nil, err
	}
	
	// Deduplicate partition keys
	pkMap := make(map[string]bool)
	for _, entity := range entities {
		if pk, ok := entity["PartitionKey"].(string); ok {
			pkMap[pk] = true
		}
	}
	
	var partitionKeys []string
	for pk := range pkMap {
		partitionKeys = append(partitionKeys, pk)
	}
	sort.Strings(partitionKeys)
	
	return partitionKeys, nil
}

// CountByPartitionKey returns the count of entities for a given partition key
func (c *Client) CountByPartitionKey(ctx context.Context, partitionKey string) (int64, error) {
	filter := fmt.Sprintf("PartitionKey eq '%s'", partitionKey)
	entities, err := c.QueryEntities(ctx, filter, 0)
	if err != nil {
		return 0, err
	}
	return int64(len(entities)), nil
}

// GetTodayCountByPartitionKey returns today's count for a partition key
func (c *Client) GetTodayCountByPartitionKey(ctx context.Context, partitionKey string) (int64, error) {
	// Azure Table Storage Timestamp is auto-generated, filter by today
	now := time.Now().UTC()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	
	filter := fmt.Sprintf("PartitionKey eq '%s' and Timestamp ge datetime'%s'",
		partitionKey, startOfDay.Format("2006-01-02T15:04:05Z"))
	
	entities, err := c.QueryEntities(ctx, filter, 0)
	if err != nil {
		return 0, err
	}
	return int64(len(entities)), nil
}

// GetLatestTimestamp returns the most recent timestamp for a partition key
func (c *Client) GetLatestTimestamp(ctx context.Context, partitionKey string) (time.Time, error) {
	filter := fmt.Sprintf("PartitionKey eq '%s'", partitionKey)
	entities, err := c.QueryEntities(ctx, filter, 1)
	if err != nil {
		return time.Time{}, err
	}
	
	if len(entities) == 0 {
		return time.Time{}, nil
	}
	
	// Find the latest timestamp
	var latest time.Time
	for _, entity := range entities {
		if ts, ok := entity["Timestamp"].(string); ok {
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				if t.After(latest) {
					latest = t
				}
			}
		}
	}
	
	return latest, nil
}

// GetSampleContactData returns a sample contact data for a partition key
func (c *Client) GetSampleContactData(ctx context.Context, partitionKey string) (*ContactData, error) {
	filter := fmt.Sprintf("PartitionKey eq '%s'", partitionKey)
	entities, err := c.QueryEntities(ctx, filter, 1)
	if err != nil {
		return nil, err
	}
	
	if len(entities) == 0 {
		return nil, nil
	}
	
	if contactDataStr, ok := entities[0]["ContactData"].(string); ok {
		entity := &TableEntity{ContactData: contactDataStr}
		return entity.ParseContactData()
	}
	
	return nil, nil
}

// GetDailyCounts returns daily counts for the last N days by partition key
func (c *Client) GetDailyCounts(ctx context.Context, days int) ([]DailyDataSetCount, error) {
	now := time.Now().UTC()
	startDate := now.AddDate(0, 0, -days)
	
	filter := fmt.Sprintf("Timestamp ge datetime'%s'", startDate.Format("2006-01-02T15:04:05Z"))
	entities, err := c.QueryEntities(ctx, filter, 0)
	if err != nil {
		return nil, err
	}
	
	// Aggregate by date and partition key
	counts := make(map[string]map[string]int64) // date -> partitionKey -> count
	
	for _, entity := range entities {
		pk, _ := entity["PartitionKey"].(string)
		tsStr, _ := entity["Timestamp"].(string)
		
		if pk == "" || tsStr == "" {
			continue
		}
		
		ts, err := time.Parse(time.RFC3339Nano, tsStr)
		if err != nil {
			continue
		}
		
		date := ts.Format("2006-01-02")
		if counts[date] == nil {
			counts[date] = make(map[string]int64)
		}
		counts[date][pk]++
	}
	
	// Convert to slice
	var result []DailyDataSetCount
	for date, pkCounts := range counts {
		for pk, count := range pkCounts {
			result = append(result, DailyDataSetCount{
				Date:        date,
				DataSetCode: pk,
				Count:       count,
			})
		}
	}
	
	// Sort by date descending
	sort.Slice(result, func(i, j int) bool {
		return result[i].Date > result[j].Date
	})
	
	return result, nil
}
