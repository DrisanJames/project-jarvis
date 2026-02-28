package datainjections

import (
	"time"

	"github.com/ignite/sparkpost-monitor/internal/azure"
	"github.com/ignite/sparkpost-monitor/internal/ongage"
	"github.com/ignite/sparkpost-monitor/internal/snowflake"
)

// DataInjectionsDashboard represents the complete data injections overview
type DataInjectionsDashboard struct {
	Timestamp           time.Time                     `json:"timestamp"`
	OverallHealth       string                        `json:"overall_health"`       // healthy, warning, critical
	HealthIssues        []string                      `json:"health_issues"`
	
	// Ingestion (Azure Table Storage)
	Ingestion           *IngestionSummary             `json:"ingestion"`
	
	// Validation (Snowflake)
	Validation          *ValidationSummary            `json:"validation"`
	
	// Import (Ongage)
	Import              *ImportSummary                `json:"import"`
}

// IngestionSummary wraps Azure Table Storage metrics
type IngestionSummary struct {
	Status              string                        `json:"status"`               // healthy, warning, critical
	TotalRecords        int64                         `json:"total_records"`
	TodayRecords        int64                         `json:"today_records"`
	AcceptedToday       int64                         `json:"accepted_today"`       // Accepted records for the day
	DataSetsActive      int                           `json:"data_sets_active"`
	DataSetsWithGaps    int                           `json:"data_sets_with_gaps"`
	DataSets            []azure.DataSetMetrics        `json:"data_sets"`
	DailyCounts         []azure.DailyDataSetCount     `json:"daily_counts"`
	LastFetch           time.Time                     `json:"last_fetch"`
	
	// System Health - Processor status (Critical if no hydration in last hour)
	SystemHealth        *azure.SystemHealth           `json:"system_health"`
	
	// Partner Health - Individual partner feed status (less prominent alerts)
	PartnerAlerts       []azure.PartnerHealth         `json:"partner_alerts"`
	
	// Historical data for date range comparison
	Historical          map[string]*azure.HistoricalMetrics `json:"historical"` // "7d", "30d", "365d"
}

// ValidationSummary wraps Snowflake validation metrics
type ValidationSummary struct {
	Status              string                        `json:"status"`
	TotalRecords        int64                         `json:"total_records"`
	TodayRecords        int64                         `json:"today_records"`
	UniqueStatuses      int                           `json:"unique_statuses"`
	StatusBreakdown     []snowflake.ValidationStatus  `json:"status_breakdown"`
	DailyMetrics        []snowflake.DailyValidationMetrics `json:"daily_metrics"`
	DomainBreakdown     []snowflake.DomainGroupMetrics `json:"domain_breakdown"`
	LastFetch           time.Time                     `json:"last_fetch"`
}

// ImportSummary wraps Ongage import metrics
type ImportSummary struct {
	Status              string                        `json:"status"`
	TotalImports        int                           `json:"total_imports"`
	TodayImports        int                           `json:"today_imports"`
	TotalRecords        int64                         `json:"total_records"`
	SuccessRecords      int64                         `json:"success_records"`
	FailedRecords       int64                         `json:"failed_records"`
	DuplicateRecords    int64                         `json:"duplicate_records"`
	InProgress          int                           `json:"in_progress"`
	Completed           int                           `json:"completed"`
	RecentImports       []ongage.Import               `json:"recent_imports"`
	DailyMetrics        []ongage.DailyImportMetrics   `json:"daily_metrics"`
	LastFetch           time.Time                     `json:"last_fetch"`
}

// PipelineMetrics shows the flow from ingestion to import
type PipelineMetrics struct {
	Date                string  `json:"date"`
	Ingested            int64   `json:"ingested"`            // Records received from partners
	Validated           int64   `json:"validated"`           // Records validated
	ValidRate           float64 `json:"valid_rate"`          // % of validated that are valid
	Imported            int64   `json:"imported"`            // Records imported to Ongage
	ImportSuccessRate   float64 `json:"import_success_rate"` // % imported successfully
}

// HealthStatus constants
const (
	StatusHealthy  = "healthy"
	StatusWarning  = "warning"
	StatusCritical = "critical"
	StatusUnknown  = "unknown"
)
