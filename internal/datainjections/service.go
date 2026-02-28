package datainjections

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/azure"
	"github.com/ignite/sparkpost-monitor/internal/ongage"
	"github.com/ignite/sparkpost-monitor/internal/snowflake"
)

// Service orchestrates data injection metrics from all sources
type Service struct {
	azureCollector     *azure.Collector
	snowflakeCollector *snowflake.Collector
	ongageClient       *ongage.Client
	
	mu                 sync.RWMutex
	dashboard          *DataInjectionsDashboard
	lastFetch          time.Time
	refreshInterval    time.Duration
}

// NewService creates a new Data Injections service
func NewService(azureCollector *azure.Collector, snowflakeCollector *snowflake.Collector, ongageClient *ongage.Client) *Service {
	return &Service{
		azureCollector:     azureCollector,
		snowflakeCollector: snowflakeCollector,
		ongageClient:       ongageClient,
		refreshInterval:    5 * time.Minute,
	}
}

// Start begins the service collection loop
func (s *Service) Start(ctx context.Context) {
	// Wait briefly for collectors to complete initial fetch
	time.Sleep(5 * time.Second)
	
	// Initial fetch
	s.fetchAllMetrics(ctx)
	
	ticker := time.NewTicker(s.refreshInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.fetchAllMetrics(ctx)
		}
	}
}

// FetchNow triggers an immediate metrics fetch
func (s *Service) FetchNow(ctx context.Context) {
	s.fetchAllMetrics(ctx)
}

// fetchAllMetrics fetches metrics from all sources and builds the dashboard
func (s *Service) fetchAllMetrics(ctx context.Context) {
	log.Println("DataInjections: Fetching metrics from all sources...")
	
	now := time.Now()
	dashboard := &DataInjectionsDashboard{
		Timestamp:    now,
		HealthIssues: []string{},
	}
	
	// Collect from Azure (Ingestion)
	ingestion := s.collectIngestionMetrics()
	dashboard.Ingestion = ingestion
	
	// Collect from Snowflake (Validation)
	validation := s.collectValidationMetrics()
	dashboard.Validation = validation
	
	// Collect from Ongage (Imports)
	importSummary := s.collectImportMetrics(ctx)
	dashboard.Import = importSummary
	
	// Calculate overall health
	dashboard.OverallHealth, dashboard.HealthIssues = s.calculateOverallHealth(ingestion, validation, importSummary)
	
	// Store the dashboard
	s.mu.Lock()
	s.dashboard = dashboard
	s.lastFetch = now
	s.mu.Unlock()
	
	log.Printf("DataInjections: Dashboard updated - Health: %s", dashboard.OverallHealth)
}

// collectIngestionMetrics collects metrics from Azure Table Storage
func (s *Service) collectIngestionMetrics() *IngestionSummary {
	if s.azureCollector == nil {
		return &IngestionSummary{
			Status:        StatusUnknown,
			SystemHealth:  &azure.SystemHealth{Status: StatusUnknown, ProcessorRunning: false},
			PartnerAlerts: []azure.PartnerHealth{},
			Historical:    make(map[string]*azure.HistoricalMetrics),
		}
	}
	
	summary := s.azureCollector.GetSummary()
	if summary == nil {
		return &IngestionSummary{
			Status:        StatusUnknown,
			SystemHealth:  &azure.SystemHealth{Status: StatusUnknown, ProcessorRunning: false},
			PartnerAlerts: []azure.PartnerHealth{},
			Historical:    make(map[string]*azure.HistoricalMetrics),
		}
	}
	
	// Calculate system health - check if ANY data set has been hydrated in the last hour
	systemHealth := s.calculateSystemHealth(summary.DataSetMetrics)
	
	// Calculate partner alerts - individual partners with gaps (>24h no data)
	partnerAlerts := s.calculatePartnerAlerts(summary.DataSetMetrics)
	
	// Determine overall status
	// CRITICAL: Processor not running (no hydration in last hour)
	// WARNING: Individual partner gaps exist
	// HEALTHY: All systems go
	status := StatusHealthy
	if !systemHealth.ProcessorRunning {
		status = StatusCritical
	} else if len(partnerAlerts) > 0 {
		status = StatusWarning
	}
	
	// Get historical data for comparison
	historical := s.collectHistoricalMetrics()
	
	return &IngestionSummary{
		Status:           status,
		TotalRecords:     summary.TotalRecords,
		TodayRecords:     summary.TodayRecords,
		AcceptedToday:    summary.TodayRecords, // Same as TodayRecords from Azure
		DataSetsActive:   summary.DataSetsActive,
		DataSetsWithGaps: len(partnerAlerts),
		DataSets:         summary.DataSetMetrics,
		DailyCounts:      s.azureCollector.GetDailyCounts(),
		LastFetch:        s.azureCollector.LastFetch(),
		SystemHealth:     systemHealth,
		PartnerAlerts:    partnerAlerts,
		Historical:       historical,
	}
}

// calculateSystemHealth determines if the processor is running
// CRITICAL if no data set has been hydrated in the last hour
func (s *Service) calculateSystemHealth(dataSets []azure.DataSetMetrics) *azure.SystemHealth {
	now := time.Now()
	var mostRecentTime time.Time
	
	// Find the most recent timestamp across ALL data sets
	for _, ds := range dataSets {
		if ds.LastTimestamp.After(mostRecentTime) {
			mostRecentTime = ds.LastTimestamp
		}
	}
	
	hoursSinceHydration := 0.0
	processorRunning := false
	status := StatusCritical
	
	if !mostRecentTime.IsZero() {
		hoursSinceHydration = now.Sub(mostRecentTime).Hours()
		// Processor is considered running if ANY data set was hydrated in the last hour
		processorRunning = hoursSinceHydration < 1.0
		if processorRunning {
			status = StatusHealthy
		}
	}
	
	return &azure.SystemHealth{
		Status:              status,
		LastHydrationTime:   mostRecentTime,
		HoursSinceHydration: hoursSinceHydration,
		ProcessorRunning:    processorRunning,
	}
}

// calculatePartnerAlerts identifies individual partners with data gaps
// Less prominent than system health - notification level alerts
func (s *Service) calculatePartnerAlerts(dataSets []azure.DataSetMetrics) []azure.PartnerHealth {
	alerts := make([]azure.PartnerHealth, 0) // Initialize as empty slice, not nil
	
	for _, ds := range dataSets {
		// Partner alert if no data in >24 hours (not the 1 hour system threshold)
		if ds.GapHours > 24 {
			status := StatusWarning
			if ds.GapHours > 48 {
				status = StatusCritical // Still less prominent than system critical
			}
			
			alerts = append(alerts, azure.PartnerHealth{
				DataPartner:   ds.DataPartner,
				DataSetCode:   ds.DataSetCode,
				LastTimestamp: ds.LastTimestamp,
				GapHours:      ds.GapHours,
				Status:        status,
			})
		}
	}
	
	return alerts
}

// collectHistoricalMetrics gets metrics for different time ranges
func (s *Service) collectHistoricalMetrics() map[string]*azure.HistoricalMetrics {
	historical := make(map[string]*azure.HistoricalMetrics)
	
	// Initialize empty entries for all ranges
	for _, rangeKey := range []string{"7d", "30d", "365d"} {
		historical[rangeKey] = &azure.HistoricalMetrics{
			DateRange:    rangeKey,
			TotalRecords: 0,
			DailyAverage: 0,
			DailyCounts:  []azure.DailyDataSetCount{},
		}
	}
	
	if s.azureCollector == nil {
		return historical
	}
	
	dailyCounts := s.azureCollector.GetDailyCounts()
	if len(dailyCounts) == 0 {
		return historical
	}
	
	now := time.Now()
	
	// Calculate metrics for different ranges
	ranges := map[string]int{
		"7d":  7,
		"30d": 30,
		"365d": 365,
	}
	
	for rangeKey, days := range ranges {
		startDate := now.AddDate(0, 0, -days)
		endDate := now
		
		var totalRecords int64
		var relevantCounts []azure.DailyDataSetCount
		
		for _, dc := range dailyCounts {
			// Parse date string to compare
			dcDate, err := time.Parse("2006-01-02", dc.Date)
			if err != nil {
				continue
			}
			
			if dcDate.After(startDate) && dcDate.Before(endDate.Add(24*time.Hour)) {
				totalRecords += dc.Count
				relevantCounts = append(relevantCounts, dc)
			}
		}
		
		dailyAverage := 0.0
		if days > 0 {
			dailyAverage = float64(totalRecords) / float64(days)
		}
		
		historical[rangeKey] = &azure.HistoricalMetrics{
			DateRange:    rangeKey,
			StartDate:    startDate,
			EndDate:      endDate,
			TotalRecords: totalRecords,
			DailyAverage: dailyAverage,
			DailyCounts:  relevantCounts,
		}
	}
	
	return historical
}

// collectValidationMetrics collects metrics from Snowflake
func (s *Service) collectValidationMetrics() *ValidationSummary {
	if s.snowflakeCollector == nil {
		return &ValidationSummary{Status: StatusUnknown}
	}
	
	summary := s.snowflakeCollector.GetSummary()
	if summary == nil {
		return &ValidationSummary{Status: StatusUnknown}
	}
	
	// Calculate status based on validation metrics
	status := StatusHealthy
	// Check if there's a significant number of invalid records
	for _, vs := range summary.StatusBreakdown {
		if vs.StatusID == "invalid" || vs.StatusID == "risky" {
			if float64(vs.Count)/float64(summary.TotalRecords) > 0.2 {
				status = StatusWarning
			}
		}
	}
	
	return &ValidationSummary{
		Status:           status,
		TotalRecords:     summary.TotalRecords,
		TodayRecords:     summary.TodayRecords,
		UniqueStatuses:   summary.UniqueStatuses,
		StatusBreakdown:  summary.StatusBreakdown,
		DailyMetrics:     summary.DailyMetrics,
		DomainBreakdown:  summary.DomainGroupBreakdown,
		LastFetch:        s.snowflakeCollector.LastFetch(),
	}
}

// collectImportMetrics collects metrics from Ongage
func (s *Service) collectImportMetrics(ctx context.Context) *ImportSummary {
	if s.ongageClient == nil {
		return &ImportSummary{Status: StatusUnknown}
	}
	
	// Get recent imports
	imports, err := s.ongageClient.GetRecentImports(ctx, 7)
	if err != nil {
		log.Printf("DataInjections: Error fetching imports: %v", err)
		return &ImportSummary{Status: StatusUnknown}
	}
	
	// Calculate metrics
	metrics := s.ongageClient.GetImportMetrics(ctx, imports)
	
	// Calculate daily metrics
	dailyMetrics := s.calculateDailyImportMetrics(imports)
	
	// Determine status
	status := StatusHealthy
	if metrics.InProgress > 5 {
		status = StatusWarning // Many imports stuck in progress
	}
	if metrics.FailedRecords > 0 && float64(metrics.FailedRecords)/float64(metrics.TotalRecords) > 0.1 {
		status = StatusWarning
	}
	
	// Limit recent imports to last 10
	recentImports := imports
	if len(recentImports) > 10 {
		recentImports = recentImports[:10]
	}
	
	return &ImportSummary{
		Status:           status,
		TotalImports:     metrics.TotalImports,
		TodayImports:     metrics.TodayImports,
		TotalRecords:     metrics.TotalRecords,
		SuccessRecords:   metrics.SuccessRecords,
		FailedRecords:    metrics.FailedRecords,
		DuplicateRecords: metrics.DuplicateRecords,
		InProgress:       metrics.InProgress,
		Completed:        metrics.Completed,
		RecentImports:    recentImports,
		DailyMetrics:     dailyMetrics,
		LastFetch:        time.Now(),
	}
}

// calculateDailyImportMetrics groups imports by date
func (s *Service) calculateDailyImportMetrics(imports []ongage.Import) []ongage.DailyImportMetrics {
	dailyMap := make(map[string]*ongage.DailyImportMetrics)
	
	for _, imp := range imports {
		created, err := ongage.ParseUnixTimestamp(imp.Created)
		if err != nil {
			continue
		}
		
		date := created.Format("2006-01-02")
		if dailyMap[date] == nil {
			dailyMap[date] = &ongage.DailyImportMetrics{Date: date}
		}
		
		dm := dailyMap[date]
		dm.TotalImports++
		
		if total, err := strconv.ParseInt(imp.Total, 10, 64); err == nil {
			dm.TotalRecords += total
		}
		if success, err := strconv.ParseInt(imp.Success, 10, 64); err == nil {
			dm.SuccessRecords += success
		}
		if failed, err := strconv.ParseInt(imp.Failed, 10, 64); err == nil {
			dm.FailedRecords += failed
		}
		if duplicate, err := strconv.ParseInt(imp.Duplicate, 10, 64); err == nil {
			dm.DuplicateRecords += duplicate
		}
	}
	
	// Convert to slice and sort by date descending
	var result []ongage.DailyImportMetrics
	for _, dm := range dailyMap {
		result = append(result, *dm)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Date > result[j].Date
	})
	
	return result
}

// calculateOverallHealth determines the overall health status
func (s *Service) calculateOverallHealth(ingestion *IngestionSummary, validation *ValidationSummary, importSummary *ImportSummary) (string, []string) {
	var issues []string
	hasCritical := false
	
	// Check ingestion - System Health takes priority
	if ingestion != nil {
		// System-level critical: Processor not running
		if ingestion.SystemHealth != nil && !ingestion.SystemHealth.ProcessorRunning {
			issues = append(issues, fmt.Sprintf("CRITICAL: Data processor not running - no hydration in %.1f hours", ingestion.SystemHealth.HoursSinceHydration))
			hasCritical = true
		}
		
		// Partner-level warnings (less prominent)
		if len(ingestion.PartnerAlerts) > 0 {
			for _, alert := range ingestion.PartnerAlerts {
				if alert.Status == StatusCritical {
					issues = append(issues, fmt.Sprintf("Partner Alert (Critical): %s (%s) - no data in %.0f hours", alert.DataPartner, alert.DataSetCode, alert.GapHours))
				} else if alert.Status == StatusWarning {
					issues = append(issues, fmt.Sprintf("Partner Alert (Warning): %s (%s) - no data in %.0f hours", alert.DataPartner, alert.DataSetCode, alert.GapHours))
				}
			}
		}
		
		if ingestion.Status == StatusUnknown {
			issues = append(issues, "Warning: Azure ingestion status unknown")
		}
	}
	
	// Check validation
	if validation != nil {
		switch validation.Status {
		case StatusCritical:
			issues = append(issues, "Critical: High validation failure rate")
			hasCritical = true
		case StatusWarning:
			issues = append(issues, "Warning: Elevated invalid/risky records")
		case StatusUnknown:
			issues = append(issues, "Warning: Snowflake validation status unknown")
		}
	}
	
	// Check imports
	if importSummary != nil {
		switch importSummary.Status {
		case StatusCritical:
			issues = append(issues, "Critical: Import system failures")
			hasCritical = true
		case StatusWarning:
			issues = append(issues, "Warning: Import issues detected")
		case StatusUnknown:
			issues = append(issues, "Warning: Ongage import status unknown")
		}
	}
	
	// Determine overall status
	if len(issues) == 0 {
		return StatusHealthy, issues
	}
	
	if hasCritical {
		return StatusCritical, issues
	}
	
	return StatusWarning, issues
}

// GetDashboard returns the current dashboard data
func (s *Service) GetDashboard() *DataInjectionsDashboard {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dashboard
}

// GetIngestionSummary returns just the ingestion summary
func (s *Service) GetIngestionSummary() *IngestionSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dashboard == nil {
		return nil
	}
	return s.dashboard.Ingestion
}

// GetValidationSummary returns just the validation summary
func (s *Service) GetValidationSummary() *ValidationSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dashboard == nil {
		return nil
	}
	return s.dashboard.Validation
}

// GetImportSummary returns just the import summary
func (s *Service) GetImportSummary() *ImportSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dashboard == nil {
		return nil
	}
	return s.dashboard.Import
}

// LastFetch returns the time of the last successful fetch
func (s *Service) LastFetch() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastFetch
}

// GetHealthStatus returns the current health status
func (s *Service) GetHealthStatus() (string, []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dashboard == nil {
		return StatusUnknown, []string{"Data injections service not initialized"}
	}
	return s.dashboard.OverallHealth, s.dashboard.HealthIssues
}
