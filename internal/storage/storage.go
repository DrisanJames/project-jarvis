package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/mailgun"
	"github.com/ignite/sparkpost-monitor/internal/ses"
	"github.com/ignite/sparkpost-monitor/internal/sparkpost"
)

// Storage provides persistent storage for metrics data
type Storage struct {
	config    config.StorageConfig
	mu        sync.RWMutex
	
	// AWS storage (optional)
	aws *AWSStorage
	
	// In-memory cache for SparkPost
	metricsCache    map[string][]sparkpost.ProcessedMetrics
	ispCache        map[string][]sparkpost.ISPMetrics
	ipCache         map[string][]sparkpost.IPMetrics
	domainCache     map[string][]sparkpost.DomainMetrics
	signalsCache    []sparkpost.SignalsData
	timeSeriesCache map[string][]sparkpost.TimeSeries

	// In-memory cache for Mailgun
	mailgunMetricsCache    map[string][]mailgun.ProcessedMetrics
	mailgunISPCache        map[string][]mailgun.ISPMetrics
	mailgunDomainCache     map[string][]mailgun.DomainMetrics
	mailgunSignalsCache    []mailgun.SignalsData

	// In-memory cache for SES
	sesMetricsCache    map[string][]ses.ProcessedMetrics
	sesISPCache        map[string][]ses.ISPMetrics
	sesSignalsCache    []ses.SignalsData
	
	// Baseline data for learning
	baselines    map[string]*Baseline
	correlations []Correlation
}

// Baseline represents learned baseline metrics for an entity
type Baseline struct {
	EntityType    string             `json:"entity_type"`    // "isp", "ip", "domain"
	EntityName    string             `json:"entity_name"`    // e.g., "Gmail", "18.236.253.72"
	Metrics       map[string]*MetricBaseline `json:"metrics"`
	UpdatedAt     time.Time          `json:"updated_at"`
	DataPoints    int                `json:"data_points"`
}

// MetricBaseline contains statistical data for a single metric
type MetricBaseline struct {
	Mean          float64   `json:"mean"`
	StdDev        float64   `json:"std_dev"`
	Min           float64   `json:"min"`
	Max           float64   `json:"max"`
	Percentile50  float64   `json:"p50"`
	Percentile75  float64   `json:"p75"`
	Percentile90  float64   `json:"p90"`
	Percentile95  float64   `json:"p95"`
	Percentile99  float64   `json:"p99"`
	HourlyPattern []float64 `json:"hourly_pattern,omitempty"` // 24 values
	DayOfWeek     []float64 `json:"day_of_week,omitempty"`    // 7 values
	Values        []float64 `json:"-"`                         // Raw values for recalculation
}

// Correlation represents a learned correlation between metrics
type Correlation struct {
	EntityType    string    `json:"entity_type"`
	EntityName    string    `json:"entity_name"`
	TriggerMetric string    `json:"trigger_metric"`
	TriggerThreshold float64 `json:"trigger_threshold"`
	TriggerOperator string   `json:"trigger_operator"` // "gt", "lt", "gte", "lte"
	EffectMetric  string    `json:"effect_metric"`
	EffectChange  float64   `json:"effect_change"`    // Percentage change
	Confidence    float64   `json:"confidence"`
	Occurrences   int       `json:"occurrences"`
	LastObserved  time.Time `json:"last_observed"`
}

// New creates a new Storage instance
func New(cfg config.StorageConfig) (*Storage, error) {
	s := &Storage{
		config:                 cfg,
		metricsCache:          make(map[string][]sparkpost.ProcessedMetrics),
		ispCache:              make(map[string][]sparkpost.ISPMetrics),
		ipCache:               make(map[string][]sparkpost.IPMetrics),
		domainCache:           make(map[string][]sparkpost.DomainMetrics),
		signalsCache:          make([]sparkpost.SignalsData, 0),
		timeSeriesCache:       make(map[string][]sparkpost.TimeSeries),
		mailgunMetricsCache:   make(map[string][]mailgun.ProcessedMetrics),
		mailgunISPCache:       make(map[string][]mailgun.ISPMetrics),
		mailgunDomainCache:    make(map[string][]mailgun.DomainMetrics),
		mailgunSignalsCache:   make([]mailgun.SignalsData, 0),
		sesMetricsCache:       make(map[string][]ses.ProcessedMetrics),
		sesISPCache:           make(map[string][]ses.ISPMetrics),
		sesSignalsCache:       make([]ses.SignalsData, 0),
		baselines:             make(map[string]*Baseline),
		correlations:          make([]Correlation, 0),
	}

	ctx := context.Background()

	switch cfg.Type {
	case "aws":
		// Initialize AWS storage
		awsStorage, err := NewAWSStorage(ctx, cfg.DynamoDBTable, cfg.S3Bucket, cfg.AWSRegion, cfg.GetAWSProfile())
		if err != nil {
			return nil, fmt.Errorf("initializing AWS storage: %w", err)
		}
		s.aws = awsStorage
		
		// Load baselines from S3
		if baselines, err := awsStorage.ListBaselinesFromS3(ctx); err == nil {
			s.baselines = baselines
		}
		
		// Load correlations from S3
		if correlations, err := awsStorage.GetCorrelationsFromS3(ctx); err == nil {
			s.correlations = correlations
		}
		
	case "local":
		// Ensure local storage directory exists
		if err := os.MkdirAll(cfg.LocalPath, 0755); err != nil {
			return nil, fmt.Errorf("creating storage directory: %w", err)
		}
		
		// Load existing data
		if err := s.loadFromDisk(); err != nil {
			// Not fatal - just log and continue
			fmt.Printf("Warning: could not load existing data: %v\n", err)
		}
	}

	return s, nil
}

// SaveMetrics saves processed metrics
func (s *Storage) SaveMetrics(ctx context.Context, metrics []sparkpost.ProcessedMetrics) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := time.Now().Format("2006-01-02")
	s.metricsCache[key] = append(s.metricsCache[key], metrics...)

	// Persist based on storage type
	switch s.config.Type {
	case "aws":
		if s.aws != nil {
			return s.aws.SaveMetricsToS3(ctx, metrics)
		}
	case "local":
		return s.saveToFile("metrics", key, s.metricsCache[key])
	}

	return nil
}

// SaveISPMetrics saves ISP metrics
func (s *Storage) SaveISPMetrics(ctx context.Context, metrics []sparkpost.ISPMetrics) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := time.Now().Format("2006-01-02")
	s.ispCache[key] = metrics

	switch s.config.Type {
	case "aws":
		if s.aws != nil {
			return s.aws.SaveISPMetrics(ctx, metrics)
		}
	case "local":
		return s.saveToFile("isp", key, metrics)
	}

	return nil
}

// SaveIPMetrics saves IP metrics
func (s *Storage) SaveIPMetrics(ctx context.Context, metrics []sparkpost.IPMetrics) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := time.Now().Format("2006-01-02")
	s.ipCache[key] = metrics

	switch s.config.Type {
	case "aws":
		if s.aws != nil {
			return s.aws.SaveIPMetrics(ctx, metrics)
		}
	case "local":
		return s.saveToFile("ip", key, metrics)
	}

	return nil
}

// SaveDomainMetrics saves domain metrics
func (s *Storage) SaveDomainMetrics(ctx context.Context, metrics []sparkpost.DomainMetrics) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := time.Now().Format("2006-01-02")
	s.domainCache[key] = metrics

	switch s.config.Type {
	case "aws":
		if s.aws != nil {
			return s.aws.SaveDomainMetrics(ctx, metrics)
		}
	case "local":
		return s.saveToFile("domain", key, metrics)
	}

	return nil
}

// SaveSignals saves signals data
func (s *Storage) SaveSignals(ctx context.Context, signals sparkpost.SignalsData) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.signalsCache = append(s.signalsCache, signals)
	
	// Keep only last 1000 signal entries
	if len(s.signalsCache) > 1000 {
		s.signalsCache = s.signalsCache[len(s.signalsCache)-1000:]
	}

	switch s.config.Type {
	case "aws":
		if s.aws != nil {
			return s.aws.SaveSignalsToS3(ctx, signals)
		}
	case "local":
		key := time.Now().Format("2006-01-02")
		return s.saveToFile("signals", key, s.signalsCache)
	}

	return nil
}

// SaveTimeSeries saves time series data
func (s *Storage) SaveTimeSeries(ctx context.Context, series []sparkpost.TimeSeries) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := time.Now().Format("2006-01-02")
	s.timeSeriesCache[key] = series

	switch s.config.Type {
	case "aws":
		if s.aws != nil {
			return s.aws.SaveTimeSeriesMetrics(ctx, series)
		}
	case "local":
		return s.saveToFile("timeseries", key, series)
	}

	return nil
}

// SaveBaseline saves a baseline
func (s *Storage) SaveBaseline(ctx context.Context, baseline *Baseline) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := fmt.Sprintf("%s:%s", baseline.EntityType, baseline.EntityName)
	s.baselines[key] = baseline

	switch s.config.Type {
	case "aws":
		if s.aws != nil {
			return s.aws.SaveBaselineToS3(ctx, baseline)
		}
	case "local":
		return s.saveToFile("baselines", key, baseline)
	}

	return nil
}

// GetBaseline retrieves a baseline
func (s *Storage) GetBaseline(ctx context.Context, entityType, entityName string) (*Baseline, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := fmt.Sprintf("%s:%s", entityType, entityName)
	baseline, ok := s.baselines[key]
	if !ok {
		return nil, nil
	}

	return baseline, nil
}

// GetAllBaselines returns all baselines
func (s *Storage) GetAllBaselines(ctx context.Context) (map[string]*Baseline, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Return a copy
	result := make(map[string]*Baseline)
	for k, v := range s.baselines {
		result[k] = v
	}
	return result, nil
}

// SaveCorrelations saves correlations
func (s *Storage) SaveCorrelations(ctx context.Context, correlations []Correlation) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.correlations = correlations

	switch s.config.Type {
	case "aws":
		if s.aws != nil {
			return s.aws.SaveCorrelationsToS3(ctx, correlations)
		}
	case "local":
		return s.saveToFile("correlations", "all", correlations)
	}

	return nil
}

// GetCorrelations retrieves correlations
func (s *Storage) GetCorrelations(ctx context.Context) ([]Correlation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Return a copy
	result := make([]Correlation, len(s.correlations))
	copy(result, s.correlations)
	return result, nil
}

// GetHistoricalMetrics retrieves historical metrics for a date range
func (s *Storage) GetHistoricalMetrics(ctx context.Context, from, to time.Time) ([]sparkpost.ProcessedMetrics, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []sparkpost.ProcessedMetrics
	
	current := from
	for current.Before(to) || current.Equal(to) {
		key := current.Format("2006-01-02")
		if metrics, ok := s.metricsCache[key]; ok {
			result = append(result, metrics...)
		}
		current = current.Add(24 * time.Hour)
	}

	return result, nil
}

// GetHistoricalISPMetrics retrieves historical ISP metrics
func (s *Storage) GetHistoricalISPMetrics(ctx context.Context, from, to time.Time) ([]sparkpost.ISPMetrics, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []sparkpost.ISPMetrics
	
	current := from
	for current.Before(to) || current.Equal(to) {
		key := current.Format("2006-01-02")
		if metrics, ok := s.ispCache[key]; ok {
			result = append(result, metrics...)
		}
		current = current.Add(24 * time.Hour)
	}

	return result, nil
}

// GetRecentSignals retrieves recent signals
func (s *Storage) GetRecentSignals(ctx context.Context, limit int) ([]sparkpost.SignalsData, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > len(s.signalsCache) {
		limit = len(s.signalsCache)
	}

	start := len(s.signalsCache) - limit
	if start < 0 {
		start = 0
	}

	result := make([]sparkpost.SignalsData, limit)
	copy(result, s.signalsCache[start:])
	return result, nil
}

// saveToFile saves data to a JSON file
func (s *Storage) saveToFile(category, key string, data interface{}) error {
	dir := filepath.Join(s.config.LocalPath, category)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// Sanitize key for filename
	safeKey := filepath.Base(key)
	path := filepath.Join(dir, safeKey+".json")

	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(data)
}

// loadFromDisk loads existing data from disk
func (s *Storage) loadFromDisk() error {
	// Load baselines
	baselinesDir := filepath.Join(s.config.LocalPath, "baselines")
	if entries, err := os.ReadDir(baselinesDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
				path := filepath.Join(baselinesDir, entry.Name())
				data, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				var baseline Baseline
				if err := json.Unmarshal(data, &baseline); err == nil {
					key := fmt.Sprintf("%s:%s", baseline.EntityType, baseline.EntityName)
					s.baselines[key] = &baseline
				}
			}
		}
	}

	// Load correlations
	correlationsPath := filepath.Join(s.config.LocalPath, "correlations", "all.json")
	if data, err := os.ReadFile(correlationsPath); err == nil {
		json.Unmarshal(data, &s.correlations)
	}

	return nil
}

// GetCacheStats returns statistics about the cache
func (s *Storage) GetCacheStats() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return map[string]interface{}{
		"metrics_days":    len(s.metricsCache),
		"isp_days":        len(s.ispCache),
		"ip_days":         len(s.ipCache),
		"domain_days":     len(s.domainCache),
		"signals_count":   len(s.signalsCache),
		"baselines_count": len(s.baselines),
		"correlations":    len(s.correlations),
	}
}

// ClearCache clears all in-memory cache
func (s *Storage) ClearCache() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.metricsCache = make(map[string][]sparkpost.ProcessedMetrics)
	s.ispCache = make(map[string][]sparkpost.ISPMetrics)
	s.ipCache = make(map[string][]sparkpost.IPMetrics)
	s.domainCache = make(map[string][]sparkpost.DomainMetrics)
	s.signalsCache = make([]sparkpost.SignalsData, 0)
	s.timeSeriesCache = make(map[string][]sparkpost.TimeSeries)
	s.mailgunMetricsCache = make(map[string][]mailgun.ProcessedMetrics)
	s.mailgunISPCache = make(map[string][]mailgun.ISPMetrics)
	s.mailgunDomainCache = make(map[string][]mailgun.DomainMetrics)
	s.mailgunSignalsCache = make([]mailgun.SignalsData, 0)
	s.sesMetricsCache = make(map[string][]ses.ProcessedMetrics)
	s.sesISPCache = make(map[string][]ses.ISPMetrics)
	s.sesSignalsCache = make([]ses.SignalsData, 0)
}

// Mailgun storage methods

// SaveMailgunMetrics saves Mailgun processed metrics
func (s *Storage) SaveMailgunMetrics(ctx context.Context, metrics []mailgun.ProcessedMetrics) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := time.Now().Format("2006-01-02")
	s.mailgunMetricsCache[key] = append(s.mailgunMetricsCache[key], metrics...)

	switch s.config.Type {
	case "aws":
		if s.aws != nil {
			return s.aws.SaveMailgunMetricsToS3(ctx, metrics)
		}
	case "local":
		return s.saveToFile("mailgun/metrics", key, s.mailgunMetricsCache[key])
	}

	return nil
}

// SaveMailgunISPMetrics saves Mailgun ISP metrics
func (s *Storage) SaveMailgunISPMetrics(ctx context.Context, metrics []mailgun.ISPMetrics) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := time.Now().Format("2006-01-02")
	s.mailgunISPCache[key] = metrics

	switch s.config.Type {
	case "aws":
		if s.aws != nil {
			return s.aws.SaveMailgunISPMetrics(ctx, metrics)
		}
	case "local":
		return s.saveToFile("mailgun/isp", key, metrics)
	}

	return nil
}

// SaveMailgunDomainMetrics saves Mailgun domain metrics
func (s *Storage) SaveMailgunDomainMetrics(ctx context.Context, metrics []mailgun.DomainMetrics) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := time.Now().Format("2006-01-02")
	s.mailgunDomainCache[key] = metrics

	switch s.config.Type {
	case "aws":
		if s.aws != nil {
			return s.aws.SaveMailgunDomainMetrics(ctx, metrics)
		}
	case "local":
		return s.saveToFile("mailgun/domain", key, metrics)
	}

	return nil
}

// SaveMailgunSignals saves Mailgun signals data
func (s *Storage) SaveMailgunSignals(ctx context.Context, signals mailgun.SignalsData) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.mailgunSignalsCache = append(s.mailgunSignalsCache, signals)

	// Keep only last 1000 signal entries
	if len(s.mailgunSignalsCache) > 1000 {
		s.mailgunSignalsCache = s.mailgunSignalsCache[len(s.mailgunSignalsCache)-1000:]
	}

	switch s.config.Type {
	case "aws":
		if s.aws != nil {
			return s.aws.SaveMailgunSignalsToS3(ctx, signals)
		}
	case "local":
		key := time.Now().Format("2006-01-02")
		return s.saveToFile("mailgun/signals", key, s.mailgunSignalsCache)
	}

	return nil
}

// GetHistoricalMailgunMetrics retrieves historical Mailgun metrics for a date range
func (s *Storage) GetHistoricalMailgunMetrics(ctx context.Context, from, to time.Time) ([]mailgun.ProcessedMetrics, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []mailgun.ProcessedMetrics

	current := from
	for current.Before(to) || current.Equal(to) {
		key := current.Format("2006-01-02")
		if metrics, ok := s.mailgunMetricsCache[key]; ok {
			result = append(result, metrics...)
		}
		current = current.Add(24 * time.Hour)
	}

	return result, nil
}

// GetHistoricalMailgunISPMetrics retrieves historical Mailgun ISP metrics
func (s *Storage) GetHistoricalMailgunISPMetrics(ctx context.Context, from, to time.Time) ([]mailgun.ISPMetrics, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []mailgun.ISPMetrics

	current := from
	for current.Before(to) || current.Equal(to) {
		key := current.Format("2006-01-02")
		if metrics, ok := s.mailgunISPCache[key]; ok {
			result = append(result, metrics...)
		}
		current = current.Add(24 * time.Hour)
	}

	return result, nil
}

// GetRecentMailgunSignals retrieves recent Mailgun signals
func (s *Storage) GetRecentMailgunSignals(ctx context.Context, limit int) ([]mailgun.SignalsData, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > len(s.mailgunSignalsCache) {
		limit = len(s.mailgunSignalsCache)
	}

	start := len(s.mailgunSignalsCache) - limit
	if start < 0 {
		start = 0
	}

	result := make([]mailgun.SignalsData, limit)
	copy(result, s.mailgunSignalsCache[start:])
	return result, nil
}

// SES storage methods

// SaveSESMetrics saves SES processed metrics
func (s *Storage) SaveSESMetrics(ctx context.Context, metrics []ses.ProcessedMetrics) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := time.Now().Format("2006-01-02")
	s.sesMetricsCache[key] = append(s.sesMetricsCache[key], metrics...)

	switch s.config.Type {
	case "aws":
		if s.aws != nil {
			return s.aws.SaveSESMetricsToS3(ctx, metrics)
		}
	case "local":
		return s.saveToFile("ses/metrics", key, s.sesMetricsCache[key])
	}

	return nil
}

// SaveSESISPMetrics saves SES ISP metrics
func (s *Storage) SaveSESISPMetrics(ctx context.Context, metrics []ses.ISPMetrics) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := time.Now().Format("2006-01-02")
	s.sesISPCache[key] = metrics

	switch s.config.Type {
	case "aws":
		if s.aws != nil {
			return s.aws.SaveSESISPMetrics(ctx, metrics)
		}
	case "local":
		return s.saveToFile("ses/isp", key, metrics)
	}

	return nil
}

// SaveSESSignals saves SES signals data
func (s *Storage) SaveSESSignals(ctx context.Context, signals ses.SignalsData) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sesSignalsCache = append(s.sesSignalsCache, signals)

	// Keep only last 1000 signal entries
	if len(s.sesSignalsCache) > 1000 {
		s.sesSignalsCache = s.sesSignalsCache[len(s.sesSignalsCache)-1000:]
	}

	switch s.config.Type {
	case "aws":
		if s.aws != nil {
			return s.aws.SaveSESSignalsToS3(ctx, signals)
		}
	case "local":
		key := time.Now().Format("2006-01-02")
		return s.saveToFile("ses/signals", key, s.sesSignalsCache)
	}

	return nil
}

// GetHistoricalSESMetrics retrieves historical SES metrics for a date range
func (s *Storage) GetHistoricalSESMetrics(ctx context.Context, from, to time.Time) ([]ses.ProcessedMetrics, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []ses.ProcessedMetrics

	current := from
	for current.Before(to) || current.Equal(to) {
		key := current.Format("2006-01-02")
		if metrics, ok := s.sesMetricsCache[key]; ok {
			result = append(result, metrics...)
		}
		current = current.Add(24 * time.Hour)
	}

	return result, nil
}

// GetHistoricalSESISPMetrics retrieves historical SES ISP metrics
func (s *Storage) GetHistoricalSESISPMetrics(ctx context.Context, from, to time.Time) ([]ses.ISPMetrics, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []ses.ISPMetrics

	current := from
	for current.Before(to) || current.Equal(to) {
		key := current.Format("2006-01-02")
		if metrics, ok := s.sesISPCache[key]; ok {
			result = append(result, metrics...)
		}
		current = current.Add(24 * time.Hour)
	}

	return result, nil
}

// GetRecentSESSignals retrieves recent SES signals
func (s *Storage) GetRecentSESSignals(ctx context.Context, limit int) ([]ses.SignalsData, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > len(s.sesSignalsCache) {
		limit = len(s.sesSignalsCache)
	}

	start := len(s.sesSignalsCache) - limit
	if start < 0 {
		start = 0
	}

	result := make([]ses.SignalsData, limit)
	copy(result, s.sesSignalsCache[start:])
	return result, nil
}

// ==================== Cost Configuration Methods ====================

// SaveCostConfiguration saves cost configuration to storage
func (s *Storage) SaveCostConfiguration(ctx context.Context, configType string, items []CostConfigItem) error {
	if s.aws != nil {
		return s.aws.SaveCostConfiguration(ctx, configType, items)
	}
	// Local fallback: save to file
	return s.saveToFile("cost_config", configType, items)
}

// GetCostConfiguration retrieves cost configuration from storage
func (s *Storage) GetCostConfiguration(ctx context.Context, configType string) (*CostConfiguration, error) {
	if s.aws != nil {
		return s.aws.GetCostConfiguration(ctx, configType)
	}
	// Local fallback: load from file
	var items []CostConfigItem
	if err := s.loadFromFile("cost_config", configType, &items); err != nil {
		return nil, err
	}
	return &CostConfiguration{
		SK:        configType,
		CostItems: items,
	}, nil
}

// GetAllCostConfigurations retrieves all cost configurations from storage
func (s *Storage) GetAllCostConfigurations(ctx context.Context) (map[string][]CostConfigItem, error) {
	if s.aws != nil {
		return s.aws.GetAllCostConfigurations(ctx)
	}
	// Local fallback: not implemented for local storage
	return make(map[string][]CostConfigItem), nil
}

// DeleteCostConfiguration removes a cost configuration from storage
func (s *Storage) DeleteCostConfiguration(ctx context.Context, configType string) error {
	if s.aws != nil {
		return s.aws.DeleteCostConfiguration(ctx, configType)
	}
	// Local fallback: delete file
	return s.deleteFile("cost_config", configType)
}

// deleteFile removes a file from local storage
func (s *Storage) deleteFile(category, name string) error {
	filePath := filepath.Join(s.config.LocalPath, category, name+".json")
	return os.Remove(filePath)
}

// loadFromFile loads data from a JSON file
func (s *Storage) loadFromFile(category, key string, data interface{}) error {
	safeKey := filepath.Base(key)
	path := filepath.Join(s.config.LocalPath, category, safeKey+".json")

	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	return json.NewDecoder(file).Decode(data)
}

// ==================== S3 Operations ====================

// SaveToS3 saves data to S3 via the AWS storage
func (s *Storage) SaveToS3(ctx context.Context, bucket, key string, data interface{}) error {
	if s.aws != nil {
		return s.aws.SaveToS3Bucket(ctx, bucket, key, data)
	}
	// Local fallback
	return s.saveToFile("s3_fallback", key, data)
}

// GetFromS3 retrieves data from S3 via the AWS storage
func (s *Storage) GetFromS3(ctx context.Context, bucket, key string, target interface{}) error {
	if s.aws != nil {
		return s.aws.GetFromS3Bucket(ctx, bucket, key, target)
	}
	// Local fallback
	return s.loadFromFile("s3_fallback", key, target)
}

// GetAWSStorage returns the underlying AWS storage (for direct access if needed)
func (s *Storage) GetAWSStorage() *AWSStorage {
	return s.aws
}
