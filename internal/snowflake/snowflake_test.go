package snowflake

import (
	"testing"
	"time"
)

func TestParseConnectionString(t *testing.T) {
	connStr := "scheme=https;ACCOUNT=HZDABLB-WLB56571;HOST=HZDABLB-WLB56571.azure.snowflakecomputing.com;port=443;USER=testuser;PASSWORD=testpass;DB=IGNITE_DATA_LAKE.REFINEDEMAILS;"
	
	cfg := ParseConnectionString(connStr)
	
	if cfg.Account != "HZDABLB-WLB56571" {
		t.Errorf("Expected Account 'HZDABLB-WLB56571', got '%s'", cfg.Account)
	}
	if cfg.User != "testuser" {
		t.Errorf("Expected User 'testuser', got '%s'", cfg.User)
	}
	if cfg.Password != "testpass" {
		t.Errorf("Expected Password 'testpass', got '%s'", cfg.Password)
	}
	if cfg.Database != "IGNITE_DATA_LAKE" {
		t.Errorf("Expected Database 'IGNITE_DATA_LAKE', got '%s'", cfg.Database)
	}
	if cfg.Schema != "REFINEDEMAILS" {
		t.Errorf("Expected Schema 'REFINEDEMAILS', got '%s'", cfg.Schema)
	}
}

func TestParseConnectionStringNoTrailingSemicolon(t *testing.T) {
	connStr := "ACCOUNT=test;USER=user;PASSWORD=pass;DB=mydb"
	
	cfg := ParseConnectionString(connStr)
	
	if cfg.Account != "test" {
		t.Errorf("Expected Account 'test', got '%s'", cfg.Account)
	}
	if cfg.Database != "mydb" {
		t.Errorf("Expected Database 'mydb', got '%s'", cfg.Database)
	}
}

func TestIndexOfChar(t *testing.T) {
	if idx := indexOfChar("key=value", '='); idx != 3 {
		t.Errorf("Expected index 3, got %d", idx)
	}
	
	if idx := indexOfChar("noequals", '='); idx != -1 {
		t.Errorf("Expected index -1, got %d", idx)
	}
	
	if idx := indexOfChar("", '='); idx != -1 {
		t.Errorf("Expected index -1 for empty string, got %d", idx)
	}
}

func TestValidationStatus(t *testing.T) {
	status := ValidationStatus{
		StatusID: "valid",
		Count:    1000,
	}
	
	if status.StatusID != "valid" {
		t.Errorf("Expected StatusID 'valid', got '%s'", status.StatusID)
	}
	if status.Count != 1000 {
		t.Errorf("Expected Count 1000, got %d", status.Count)
	}
}

func TestDailyValidationMetrics(t *testing.T) {
	metrics := DailyValidationMetrics{
		Date:         "2026-01-28",
		TotalRecords: 5000,
		StatusBreakdown: []ValidationStatus{
			{StatusID: "valid", Count: 4000},
			{StatusID: "invalid", Count: 1000},
		},
	}
	
	if metrics.Date != "2026-01-28" {
		t.Errorf("Expected Date '2026-01-28', got '%s'", metrics.Date)
	}
	if metrics.TotalRecords != 5000 {
		t.Errorf("Expected TotalRecords 5000, got %d", metrics.TotalRecords)
	}
	if len(metrics.StatusBreakdown) != 2 {
		t.Errorf("Expected 2 status breakdowns, got %d", len(metrics.StatusBreakdown))
	}
}

func TestDomainGroupMetrics(t *testing.T) {
	metrics := DomainGroupMetrics{
		DomainGroup:      "Gmail",
		DomainGroupShort: "GMAL",
		Count:            50000,
	}
	
	if metrics.DomainGroup != "Gmail" {
		t.Errorf("Expected DomainGroup 'Gmail', got '%s'", metrics.DomainGroup)
	}
	if metrics.Count != 50000 {
		t.Errorf("Expected Count 50000, got %d", metrics.Count)
	}
}

func TestValidationSummary(t *testing.T) {
	summary := ValidationSummary{
		Timestamp:      time.Now(),
		TotalRecords:   100000,
		TodayRecords:   5000,
		UniqueStatuses: 5,
		DailyMetrics: []DailyValidationMetrics{
			{Date: "2026-01-28", TotalRecords: 5000},
		},
		StatusBreakdown: []ValidationStatus{
			{StatusID: "valid", Count: 80000},
		},
		DomainGroupBreakdown: []DomainGroupMetrics{
			{DomainGroup: "Gmail", Count: 40000},
		},
	}
	
	if summary.TotalRecords != 100000 {
		t.Errorf("Expected TotalRecords 100000, got %d", summary.TotalRecords)
	}
	if summary.TodayRecords != 5000 {
		t.Errorf("Expected TodayRecords 5000, got %d", summary.TodayRecords)
	}
	if summary.UniqueStatuses != 5 {
		t.Errorf("Expected UniqueStatuses 5, got %d", summary.UniqueStatuses)
	}
}

func TestSubscriberValidation(t *testing.T) {
	validation := SubscriberValidation{
		ID:                   1,
		Email:                "test@example.com",
		ValidationStatusID:   "valid",
		CreationDate:         "2026-01-28 12:00:00",
		LastValidationDate:   "2026-01-28 12:00:00",
		EmailDomainGroupID:   1,
		EmailDomainGroup:     "Gmail",
		EmailDomainGroupShort: "GMAL",
		Filename:             "import_001.csv",
	}
	
	if validation.Email != "test@example.com" {
		t.Errorf("Expected Email 'test@example.com', got '%s'", validation.Email)
	}
	if validation.ValidationStatusID != "valid" {
		t.Errorf("Expected ValidationStatusID 'valid', got '%s'", validation.ValidationStatusID)
	}
}

func TestCollectorGetSummary(t *testing.T) {
	collector := &Collector{}
	
	// Initially nil
	if collector.GetSummary() != nil {
		t.Error("Expected nil summary initially")
	}
	
	// Set summary
	collector.summary = &ValidationSummary{
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

func TestCollectorGetDailyMetrics(t *testing.T) {
	collector := &Collector{
		summary: &ValidationSummary{
			DailyMetrics: []DailyValidationMetrics{
				{Date: "2026-01-28", TotalRecords: 5000},
				{Date: "2026-01-27", TotalRecords: 4500},
			},
		},
	}
	
	metrics := collector.GetDailyMetrics()
	if len(metrics) != 2 {
		t.Errorf("Expected 2 daily metrics, got %d", len(metrics))
	}
}

func TestCollectorGetStatusBreakdown(t *testing.T) {
	collector := &Collector{
		summary: &ValidationSummary{
			StatusBreakdown: []ValidationStatus{
				{StatusID: "valid", Count: 8000},
				{StatusID: "invalid", Count: 2000},
			},
		},
	}
	
	breakdown := collector.GetStatusBreakdown()
	if len(breakdown) != 2 {
		t.Errorf("Expected 2 status breakdowns, got %d", len(breakdown))
	}
}

func TestCollectorGetDomainGroupBreakdown(t *testing.T) {
	collector := &Collector{
		summary: &ValidationSummary{
			DomainGroupBreakdown: []DomainGroupMetrics{
				{DomainGroup: "Gmail", Count: 5000},
				{DomainGroup: "Yahoo", Count: 3000},
			},
		},
	}
	
	breakdown := collector.GetDomainGroupBreakdown()
	if len(breakdown) != 2 {
		t.Errorf("Expected 2 domain group breakdowns, got %d", len(breakdown))
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

func TestCollectorGetTodayRecords(t *testing.T) {
	collector := &Collector{}
	
	// Initially 0
	if collector.GetTodayRecords() != 0 {
		t.Errorf("Expected 0 today records initially, got %d", collector.GetTodayRecords())
	}
	
	collector.summary = &ValidationSummary{
		TodayRecords: 500,
	}
	
	if collector.GetTodayRecords() != 500 {
		t.Errorf("Expected 500 today records, got %d", collector.GetTodayRecords())
	}
}

func TestCollectorGetTotalRecords(t *testing.T) {
	collector := &Collector{}
	
	// Initially 0
	if collector.GetTotalRecords() != 0 {
		t.Errorf("Expected 0 total records initially, got %d", collector.GetTotalRecords())
	}
	
	collector.summary = &ValidationSummary{
		TotalRecords: 10000,
	}
	
	if collector.GetTotalRecords() != 10000 {
		t.Errorf("Expected 10000 total records, got %d", collector.GetTotalRecords())
	}
}

func TestNewCollector(t *testing.T) {
	cfg := Config{
		Account:  "test",
		User:     "user",
		Password: "pass",
		Database: "db",
		Schema:   "schema",
	}
	
	// Note: We can't actually create a client without a real Snowflake connection
	// This test just verifies the collector constructor works
	collector := NewCollector(nil, cfg)
	if collector == nil {
		t.Fatal("Expected non-nil collector")
	}
	if collector.refreshInterval != 5*time.Minute {
		t.Errorf("Expected refresh interval of 5 minutes, got %v", collector.refreshInterval)
	}
}

func TestConfigFields(t *testing.T) {
	cfg := Config{
		Account:   "myaccount",
		User:      "myuser",
		Password:  "mypassword",
		Database:  "mydb",
		Schema:    "myschema",
		Warehouse: "mywarehouse",
		Enabled:   true,
	}
	
	if cfg.Account != "myaccount" {
		t.Errorf("Expected Account 'myaccount', got '%s'", cfg.Account)
	}
	if cfg.Warehouse != "mywarehouse" {
		t.Errorf("Expected Warehouse 'mywarehouse', got '%s'", cfg.Warehouse)
	}
	if !cfg.Enabled {
		t.Error("Expected Enabled to be true")
	}
}
