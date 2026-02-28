package datainjections

import (
	"fmt"
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/azure"
	"github.com/ignite/sparkpost-monitor/internal/ongage"
	"github.com/ignite/sparkpost-monitor/internal/snowflake"
)

func TestNewService(t *testing.T) {
	service := NewService(nil, nil, nil)
	if service == nil {
		t.Fatal("Expected non-nil service")
	}
	if service.refreshInterval != 5*time.Minute {
		t.Errorf("Expected refresh interval of 5 minutes, got %v", service.refreshInterval)
	}
}

func TestServiceGetDashboard(t *testing.T) {
	service := &Service{}
	
	// Initially nil
	if service.GetDashboard() != nil {
		t.Error("Expected nil dashboard initially")
	}
	
	// Set dashboard
	service.dashboard = &DataInjectionsDashboard{
		Timestamp:     time.Now(),
		OverallHealth: StatusHealthy,
	}
	
	dashboard := service.GetDashboard()
	if dashboard == nil {
		t.Fatal("Expected non-nil dashboard")
	}
	if dashboard.OverallHealth != StatusHealthy {
		t.Errorf("Expected OverallHealth 'healthy', got '%s'", dashboard.OverallHealth)
	}
}

func TestServiceGetIngestionSummary(t *testing.T) {
	service := &Service{
		dashboard: &DataInjectionsDashboard{
			Ingestion: &IngestionSummary{
				Status:       StatusHealthy,
				TotalRecords: 10000,
			},
		},
	}
	
	summary := service.GetIngestionSummary()
	if summary == nil {
		t.Fatal("Expected non-nil ingestion summary")
	}
	if summary.TotalRecords != 10000 {
		t.Errorf("Expected TotalRecords 10000, got %d", summary.TotalRecords)
	}
}

func TestServiceGetValidationSummary(t *testing.T) {
	service := &Service{
		dashboard: &DataInjectionsDashboard{
			Validation: &ValidationSummary{
				Status:       StatusHealthy,
				TotalRecords: 50000,
			},
		},
	}
	
	summary := service.GetValidationSummary()
	if summary == nil {
		t.Fatal("Expected non-nil validation summary")
	}
	if summary.TotalRecords != 50000 {
		t.Errorf("Expected TotalRecords 50000, got %d", summary.TotalRecords)
	}
}

func TestServiceGetImportSummary(t *testing.T) {
	service := &Service{
		dashboard: &DataInjectionsDashboard{
			Import: &ImportSummary{
				Status:       StatusHealthy,
				TotalImports: 25,
			},
		},
	}
	
	summary := service.GetImportSummary()
	if summary == nil {
		t.Fatal("Expected non-nil import summary")
	}
	if summary.TotalImports != 25 {
		t.Errorf("Expected TotalImports 25, got %d", summary.TotalImports)
	}
}

func TestServiceLastFetch(t *testing.T) {
	service := &Service{}
	
	// Initially zero
	if !service.LastFetch().IsZero() {
		t.Error("Expected zero time initially")
	}
	
	now := time.Now()
	service.lastFetch = now
	
	if !service.LastFetch().Equal(now) {
		t.Error("Expected LastFetch to return the set time")
	}
}

func TestServiceGetHealthStatus(t *testing.T) {
	service := &Service{}
	
	// Initially unknown
	status, issues := service.GetHealthStatus()
	if status != StatusUnknown {
		t.Errorf("Expected status 'unknown', got '%s'", status)
	}
	if len(issues) == 0 {
		t.Error("Expected at least one issue for uninitialized service")
	}
	
	// Set healthy status
	service.dashboard = &DataInjectionsDashboard{
		OverallHealth: StatusHealthy,
		HealthIssues:  []string{},
	}
	
	status, issues = service.GetHealthStatus()
	if status != StatusHealthy {
		t.Errorf("Expected status 'healthy', got '%s'", status)
	}
	if len(issues) != 0 {
		t.Errorf("Expected 0 issues for healthy status, got %d", len(issues))
	}
}

func TestCalculateOverallHealth(t *testing.T) {
	service := &Service{}
	
	tests := []struct {
		name           string
		ingestion      *IngestionSummary
		validation     *ValidationSummary
		importSummary  *ImportSummary
		expectedStatus string
		expectIssues   bool
	}{
		{
			name:           "all healthy",
			ingestion:      &IngestionSummary{Status: StatusHealthy},
			validation:     &ValidationSummary{Status: StatusHealthy},
			importSummary:  &ImportSummary{Status: StatusHealthy},
			expectedStatus: StatusHealthy,
			expectIssues:   false,
		},
		{
			name: "ingestion warning",
			ingestion: &IngestionSummary{
				Status: StatusWarning,
				SystemHealth: &azure.SystemHealth{ProcessorRunning: true},
				PartnerAlerts: []azure.PartnerHealth{
					{DataPartner: "TestPartner", GapHours: 25, Status: StatusWarning},
				},
			},
			validation:     &ValidationSummary{Status: StatusHealthy},
			importSummary:  &ImportSummary{Status: StatusHealthy},
			expectedStatus: StatusWarning,
			expectIssues:   true,
		},
		{
			name: "ingestion critical",
			ingestion: &IngestionSummary{
				Status: StatusCritical,
				SystemHealth: &azure.SystemHealth{
					ProcessorRunning:    false,
					HoursSinceHydration: 2.5,
				},
			},
			validation:     &ValidationSummary{Status: StatusHealthy},
			importSummary:  &ImportSummary{Status: StatusHealthy},
			expectedStatus: StatusCritical,
			expectIssues:   true,
		},
		{
			name:           "validation warning",
			ingestion:      &IngestionSummary{Status: StatusHealthy},
			validation:     &ValidationSummary{Status: StatusWarning},
			importSummary:  &ImportSummary{Status: StatusHealthy},
			expectedStatus: StatusWarning,
			expectIssues:   true,
		},
		{
			name:           "import unknown",
			ingestion:      &IngestionSummary{Status: StatusHealthy},
			validation:     &ValidationSummary{Status: StatusHealthy},
			importSummary:  &ImportSummary{Status: StatusUnknown},
			expectedStatus: StatusWarning,
			expectIssues:   true,
		},
		{
			name:           "multiple issues",
			ingestion:      &IngestionSummary{Status: StatusWarning},
			validation:     &ValidationSummary{Status: StatusWarning},
			importSummary:  &ImportSummary{Status: StatusWarning},
			expectedStatus: StatusWarning,
			expectIssues:   true,
		},
		{
			name: "critical overrides warning",
			ingestion: &IngestionSummary{
				Status: StatusCritical,
				SystemHealth: &azure.SystemHealth{
					ProcessorRunning:    false,
					HoursSinceHydration: 3.0,
				},
			},
			validation:     &ValidationSummary{Status: StatusWarning},
			importSummary:  &ImportSummary{Status: StatusWarning},
			expectedStatus: StatusCritical,
			expectIssues:   true,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, issues := service.calculateOverallHealth(tt.ingestion, tt.validation, tt.importSummary)
			
			if status != tt.expectedStatus {
				t.Errorf("Expected status '%s', got '%s'", tt.expectedStatus, status)
			}
			
			if tt.expectIssues && len(issues) == 0 {
				t.Error("Expected issues but got none")
			}
			if !tt.expectIssues && len(issues) > 0 {
				t.Errorf("Expected no issues but got: %v", issues)
			}
		})
	}
}

func TestDataInjectionsDashboard(t *testing.T) {
	dashboard := DataInjectionsDashboard{
		Timestamp:     time.Now(),
		OverallHealth: StatusHealthy,
		HealthIssues:  []string{},
		Ingestion: &IngestionSummary{
			Status:       StatusHealthy,
			TotalRecords: 10000,
		},
		Validation: &ValidationSummary{
			Status:       StatusHealthy,
			TotalRecords: 50000,
		},
		Import: &ImportSummary{
			Status:       StatusHealthy,
			TotalImports: 25,
		},
	}
	
	if dashboard.OverallHealth != StatusHealthy {
		t.Errorf("Expected OverallHealth 'healthy', got '%s'", dashboard.OverallHealth)
	}
	if dashboard.Ingestion.TotalRecords != 10000 {
		t.Errorf("Expected Ingestion TotalRecords 10000, got %d", dashboard.Ingestion.TotalRecords)
	}
}

func TestIngestionSummary(t *testing.T) {
	summary := IngestionSummary{
		Status:           StatusHealthy,
		TotalRecords:     10000,
		TodayRecords:     500,
		DataSetsActive:   5,
		DataSetsWithGaps: 0,
		DataSets: []azure.DataSetMetrics{
			{DataSetCode: "GLB_BR", RecordCount: 5000},
		},
		LastFetch: time.Now(),
	}
	
	if summary.Status != StatusHealthy {
		t.Errorf("Expected Status 'healthy', got '%s'", summary.Status)
	}
	if len(summary.DataSets) != 1 {
		t.Errorf("Expected 1 data set, got %d", len(summary.DataSets))
	}
}

func TestValidationSummary(t *testing.T) {
	summary := ValidationSummary{
		Status:         StatusHealthy,
		TotalRecords:   50000,
		TodayRecords:   2500,
		UniqueStatuses: 5,
		StatusBreakdown: []snowflake.ValidationStatus{
			{StatusID: "valid", Count: 40000},
			{StatusID: "invalid", Count: 10000},
		},
		LastFetch: time.Now(),
	}
	
	if summary.UniqueStatuses != 5 {
		t.Errorf("Expected UniqueStatuses 5, got %d", summary.UniqueStatuses)
	}
	if len(summary.StatusBreakdown) != 2 {
		t.Errorf("Expected 2 status breakdowns, got %d", len(summary.StatusBreakdown))
	}
}

func TestImportSummary(t *testing.T) {
	summary := ImportSummary{
		Status:           StatusHealthy,
		TotalImports:     25,
		TodayImports:     5,
		TotalRecords:     100000,
		SuccessRecords:   95000,
		FailedRecords:    1000,
		DuplicateRecords: 4000,
		InProgress:       2,
		Completed:        23,
		LastFetch:        time.Now(),
	}
	
	if summary.TotalImports != 25 {
		t.Errorf("Expected TotalImports 25, got %d", summary.TotalImports)
	}
	if summary.SuccessRecords != 95000 {
		t.Errorf("Expected SuccessRecords 95000, got %d", summary.SuccessRecords)
	}
}

func TestPipelineMetrics(t *testing.T) {
	metrics := PipelineMetrics{
		Date:              "2026-01-28",
		Ingested:          10000,
		Validated:         9500,
		ValidRate:         0.95,
		Imported:          9000,
		ImportSuccessRate: 0.947,
	}
	
	if metrics.Date != "2026-01-28" {
		t.Errorf("Expected Date '2026-01-28', got '%s'", metrics.Date)
	}
	if metrics.ValidRate != 0.95 {
		t.Errorf("Expected ValidRate 0.95, got %f", metrics.ValidRate)
	}
}

func TestCalculateDailyImportMetrics(t *testing.T) {
	service := &Service{}
	
	// Test with imports from different days
	now := time.Now()
	yesterday := now.AddDate(0, 0, -1)
	
	imports := []ongage.Import{
		{
			Created:   formatUnixTimestamp(now),
			Total:     "1000",
			Success:   "950",
			Failed:    "10",
			Duplicate: "40",
		},
		{
			Created:   formatUnixTimestamp(now),
			Total:     "2000",
			Success:   "1900",
			Failed:    "20",
			Duplicate: "80",
		},
		{
			Created:   formatUnixTimestamp(yesterday),
			Total:     "1500",
			Success:   "1400",
			Failed:    "50",
			Duplicate: "50",
		},
	}
	
	dailyMetrics := service.calculateDailyImportMetrics(imports)
	
	if len(dailyMetrics) != 2 {
		t.Fatalf("Expected 2 daily metrics, got %d", len(dailyMetrics))
	}
	
	// Check today's metrics (should be first due to sorting)
	todayMetrics := dailyMetrics[0]
	if todayMetrics.Date != now.Format("2006-01-02") {
		t.Errorf("Expected today's date, got '%s'", todayMetrics.Date)
	}
	if todayMetrics.TotalImports != 2 {
		t.Errorf("Expected 2 imports today, got %d", todayMetrics.TotalImports)
	}
	if todayMetrics.TotalRecords != 3000 {
		t.Errorf("Expected 3000 total records today, got %d", todayMetrics.TotalRecords)
	}
}

func formatUnixTimestamp(t time.Time) string {
	return fmt.Sprintf("%d", t.Unix()) // Unix timestamp as string number
}

func TestStatusConstants(t *testing.T) {
	if StatusHealthy != "healthy" {
		t.Errorf("Expected StatusHealthy 'healthy', got '%s'", StatusHealthy)
	}
	if StatusWarning != "warning" {
		t.Errorf("Expected StatusWarning 'warning', got '%s'", StatusWarning)
	}
	if StatusCritical != "critical" {
		t.Errorf("Expected StatusCritical 'critical', got '%s'", StatusCritical)
	}
	if StatusUnknown != "unknown" {
		t.Errorf("Expected StatusUnknown 'unknown', got '%s'", StatusUnknown)
	}
}
