package azure

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseConnectionString(t *testing.T) {
	cfg := Config{
		ConnectionString: "DefaultEndpointsProtocol=https;AccountName=testaccount;AccountKey=dGVzdGtleQ==;EndpointSuffix=core.windows.net",
	}
	
	accountName, accountKey, endpointSuffix := cfg.ParseConnectionString()
	
	if accountName != "testaccount" {
		t.Errorf("Expected accountName 'testaccount', got '%s'", accountName)
	}
	if accountKey != "dGVzdGtleQ==" {
		t.Errorf("Expected accountKey 'dGVzdGtleQ==', got '%s'", accountKey)
	}
	if endpointSuffix != "core.windows.net" {
		t.Errorf("Expected endpointSuffix 'core.windows.net', got '%s'", endpointSuffix)
	}
}

func TestParseContactData(t *testing.T) {
	entity := &TableEntity{
		PartitionKey: "GLB_BR",
		RowKey:       "test-uuid",
		ContactData: `{"email":"test@example.com","first_name":"John","last_name":"Doe","custom_field":{"dataPartner":"GLOBE USA","dataSet":"GLOBE_USA_BEDROCK","sourceUrl":"www.example.com","ipAddress":"1.2.3.4","postalAddress":"123 Main St","city":"TestCity","state":"TS","zipCode":"12345","opt_in_date":"2026-01-28 12:00:00"}}`,
	}
	
	data, err := entity.ParseContactData()
	if err != nil {
		t.Fatalf("Failed to parse contact data: %v", err)
	}
	
	if data.Email != "test@example.com" {
		t.Errorf("Expected email 'test@example.com', got '%s'", data.Email)
	}
	if data.FirstName != "John" {
		t.Errorf("Expected first_name 'John', got '%s'", data.FirstName)
	}
	if data.LastName != "Doe" {
		t.Errorf("Expected last_name 'Doe', got '%s'", data.LastName)
	}
	if data.CustomField.DataPartner != "GLOBE USA" {
		t.Errorf("Expected dataPartner 'GLOBE USA', got '%s'", data.CustomField.DataPartner)
	}
	if data.CustomField.DataSet != "GLOBE_USA_BEDROCK" {
		t.Errorf("Expected dataSet 'GLOBE_USA_BEDROCK', got '%s'", data.CustomField.DataSet)
	}
}

func TestParseContactDataInvalid(t *testing.T) {
	entity := &TableEntity{
		ContactData: "invalid json",
	}
	
	_, err := entity.ParseContactData()
	if err == nil {
		t.Error("Expected error for invalid JSON, got nil")
	}
}

func TestNewClient(t *testing.T) {
	cfg := Config{
		ConnectionString: "DefaultEndpointsProtocol=https;AccountName=testaccount;AccountKey=dGVzdGtleQ==;EndpointSuffix=core.windows.net",
		TableName:        "testtable",
	}
	
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	
	if client.accountName != "testaccount" {
		t.Errorf("Expected accountName 'testaccount', got '%s'", client.accountName)
	}
	if client.tableName != "testtable" {
		t.Errorf("Expected tableName 'testtable', got '%s'", client.tableName)
	}
}

func TestNewClientInvalidConnectionString(t *testing.T) {
	cfg := Config{
		ConnectionString: "InvalidConnectionString",
	}
	
	_, err := NewClient(cfg)
	if err == nil {
		t.Error("Expected error for invalid connection string, got nil")
	}
}

func TestNewClientDefaultTableName(t *testing.T) {
	cfg := Config{
		ConnectionString: "DefaultEndpointsProtocol=https;AccountName=testaccount;AccountKey=dGVzdGtleQ==",
	}
	
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	
	if client.tableName != "ignitemediagroupcrm" {
		t.Errorf("Expected default tableName 'ignitemediagroupcrm', got '%s'", client.tableName)
	}
}

func TestDataSetMetrics(t *testing.T) {
	metrics := DataSetMetrics{
		DataSetCode:   "GLB_BR",
		DataPartner:   "GLOBE USA",
		DataSetName:   "GLOBE_USA_BEDROCK",
		RecordCount:   1000,
		TodayCount:    50,
		LastTimestamp: time.Now(),
		HasGap:        false,
		GapHours:      0.5,
	}
	
	if metrics.DataSetCode != "GLB_BR" {
		t.Errorf("Expected DataSetCode 'GLB_BR', got '%s'", metrics.DataSetCode)
	}
	if metrics.RecordCount != 1000 {
		t.Errorf("Expected RecordCount 1000, got %d", metrics.RecordCount)
	}
	if metrics.HasGap {
		t.Error("Expected HasGap to be false")
	}
}

func TestDataInjectionSummary(t *testing.T) {
	summary := DataInjectionSummary{
		Timestamp:        time.Now(),
		TotalRecords:     5000,
		TodayRecords:     250,
		DataSetsActive:   5,
		DataSetsWithGaps: 1,
		DataSetMetrics: []DataSetMetrics{
			{DataSetCode: "GLB_BR", RecordCount: 1000},
			{DataSetCode: "USA_PR", RecordCount: 4000},
		},
	}
	
	if summary.TotalRecords != 5000 {
		t.Errorf("Expected TotalRecords 5000, got %d", summary.TotalRecords)
	}
	if len(summary.DataSetMetrics) != 2 {
		t.Errorf("Expected 2 data set metrics, got %d", len(summary.DataSetMetrics))
	}
}

func TestCollectorGetSummary(t *testing.T) {
	collector := &Collector{}
	
	// Initially nil
	if collector.GetSummary() != nil {
		t.Error("Expected nil summary initially")
	}
	
	// Set summary
	collector.summary = &DataInjectionSummary{
		TotalRecords: 1000,
	}
	
	summary := collector.GetSummary()
	if summary == nil {
		t.Fatal("Expected non-nil summary")
	}
	if summary.TotalRecords != 1000 {
		t.Errorf("Expected TotalRecords 1000, got %d", summary.TotalRecords)
	}
}

func TestCollectorGetDataSetMetrics(t *testing.T) {
	collector := &Collector{
		summary: &DataInjectionSummary{
			DataSetMetrics: []DataSetMetrics{
				{DataSetCode: "GLB_BR"},
				{DataSetCode: "USA_PR"},
			},
		},
	}
	
	metrics := collector.GetDataSetMetrics()
	if len(metrics) != 2 {
		t.Errorf("Expected 2 metrics, got %d", len(metrics))
	}
}

func TestCollectorGetDataSetByCode(t *testing.T) {
	collector := &Collector{
		summary: &DataInjectionSummary{
			DataSetMetrics: []DataSetMetrics{
				{DataSetCode: "GLB_BR", RecordCount: 1000},
				{DataSetCode: "USA_PR", RecordCount: 2000},
			},
		},
	}
	
	ds := collector.GetDataSetByCode("GLB_BR")
	if ds == nil {
		t.Fatal("Expected to find data set GLB_BR")
	}
	if ds.RecordCount != 1000 {
		t.Errorf("Expected RecordCount 1000, got %d", ds.RecordCount)
	}
	
	ds = collector.GetDataSetByCode("NONEXISTENT")
	if ds != nil {
		t.Error("Expected nil for nonexistent data set")
	}
}

func TestCollectorHasGaps(t *testing.T) {
	collector := &Collector{
		summary: &DataInjectionSummary{
			DataSetsWithGaps: 0,
		},
	}
	
	if collector.HasGaps() {
		t.Error("Expected HasGaps to be false when DataSetsWithGaps is 0")
	}
	
	collector.summary.DataSetsWithGaps = 2
	if !collector.HasGaps() {
		t.Error("Expected HasGaps to be true when DataSetsWithGaps is 2")
	}
}

func TestCollectorGetDataSetsWithGaps(t *testing.T) {
	collector := &Collector{
		summary: &DataInjectionSummary{
			DataSetMetrics: []DataSetMetrics{
				{DataSetCode: "GLB_BR", HasGap: false},
				{DataSetCode: "USA_PR", HasGap: true, GapHours: 48},
				{DataSetCode: "EUR_FR", HasGap: true, GapHours: 72},
			},
		},
	}
	
	gapped := collector.GetDataSetsWithGaps()
	if len(gapped) != 2 {
		t.Errorf("Expected 2 data sets with gaps, got %d", len(gapped))
	}
}

func TestClientQueryEntitiesMock(t *testing.T) {
	// Create mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := map[string]interface{}{
			"value": []map[string]interface{}{
				{
					"PartitionKey": "GLB_BR",
					"RowKey":       "test-1",
					"Timestamp":    time.Now().Format(time.RFC3339Nano),
					"ContactData":  `{"email":"test@example.com"}`,
				},
				{
					"PartitionKey": "GLB_BR",
					"RowKey":       "test-2",
					"Timestamp":    time.Now().Format(time.RFC3339Nano),
					"ContactData":  `{"email":"test2@example.com"}`,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	
	// Note: This test is limited because we can't easily mock the Azure auth
	// In a real scenario, you'd use interface-based mocking
}

func TestDailyDataSetCount(t *testing.T) {
	count := DailyDataSetCount{
		Date:        "2026-01-28",
		DataSetCode: "GLB_BR",
		Count:       150,
	}
	
	if count.Date != "2026-01-28" {
		t.Errorf("Expected Date '2026-01-28', got '%s'", count.Date)
	}
	if count.DataSetCode != "GLB_BR" {
		t.Errorf("Expected DataSetCode 'GLB_BR', got '%s'", count.DataSetCode)
	}
	if count.Count != 150 {
		t.Errorf("Expected Count 150, got %d", count.Count)
	}
}

func TestSplitConnectionString(t *testing.T) {
	parts := splitConnectionString("a=1;b=2;c=3")
	if len(parts) != 3 {
		t.Errorf("Expected 3 parts, got %d", len(parts))
	}
	
	parts = splitConnectionString("")
	if len(parts) != 0 {
		t.Errorf("Expected 0 parts for empty string, got %d", len(parts))
	}
}

func TestIndexOf(t *testing.T) {
	idx := indexOf("key=value", '=')
	if idx != 3 {
		t.Errorf("Expected index 3, got %d", idx)
	}
	
	idx = indexOf("noequals", '=')
	if idx != -1 {
		t.Errorf("Expected index -1, got %d", idx)
	}
}

func TestNewCollector(t *testing.T) {
	cfg := Config{
		ConnectionString: "DefaultEndpointsProtocol=https;AccountName=test;AccountKey=dGVzdA==",
	}
	client, _ := NewClient(cfg)
	
	collector := NewCollector(client, cfg)
	if collector == nil {
		t.Fatal("Expected non-nil collector")
	}
	if collector.client != client {
		t.Error("Expected collector to have the provided client")
	}
}

func TestCollectorLastFetch(t *testing.T) {
	collector := &Collector{}
	
	// Initially zero
	if !collector.LastFetch().IsZero() {
		t.Error("Expected zero time initially")
	}
	
	now := time.Now()
	collector.lastFetch = now
	
	if !collector.LastFetch().Equal(now) {
		t.Error("Expected LastFetch to return the set time")
	}
}
