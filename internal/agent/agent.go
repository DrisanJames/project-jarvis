package agent

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/mailgun"
	"github.com/ignite/sparkpost-monitor/internal/ses"
	"github.com/ignite/sparkpost-monitor/internal/sparkpost"
	"github.com/ignite/sparkpost-monitor/internal/storage"
)

// Agent is the learning agent that analyzes metrics and provides insights
type Agent struct {
	config  config.AgentConfig
	storage *storage.Storage
	
	mu sync.RWMutex
	
	// ESP collectors for unified access
	collectors *ESPCollectors
	
	// Learning state
	baselines    map[string]*storage.Baseline
	correlations []storage.Correlation
	alerts       []Alert
	insights     []Insight
	
	// Rolling statistics for real-time learning
	rollingStats map[string]*RollingStats
	
	// Last analysis times
	lastBaselineUpdate    time.Time
	lastCorrelationUpdate time.Time
}

// Alert represents a generated alert
type Alert struct {
	ID          string    `json:"id"`
	Timestamp   time.Time `json:"timestamp"`
	Severity    string    `json:"severity"` // "info", "warning", "critical"
	Category    string    `json:"category"` // "complaint", "bounce", "delivery", "volume"
	Title       string    `json:"title"`
	Description string    `json:"description"`
	EntityType  string    `json:"entity_type"`
	EntityName  string    `json:"entity_name"`
	MetricName  string    `json:"metric_name"`
	CurrentValue float64  `json:"current_value"`
	BaselineValue float64 `json:"baseline_value"`
	Deviation   float64   `json:"deviation"` // Number of standard deviations
	Recommendation string `json:"recommendation"`
	Acknowledged bool     `json:"acknowledged"`
}

// Insight represents a learned insight
type Insight struct {
	ID          string    `json:"id"`
	Timestamp   time.Time `json:"timestamp"`
	Type        string    `json:"type"` // "pattern", "correlation", "trend", "anomaly"
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Confidence  float64   `json:"confidence"`
	EntityType  string    `json:"entity_type,omitempty"`
	EntityName  string    `json:"entity_name,omitempty"`
}

// RollingStats tracks rolling statistics for a metric
type RollingStats struct {
	MetricName string
	EntityType string
	EntityName string
	Values     []float64
	Timestamps []time.Time
	MaxSize    int
}

// New creates a new learning agent
func New(cfg config.AgentConfig, store *storage.Storage) *Agent {
	agent := &Agent{
		config:       cfg,
		storage:      store,
		baselines:    make(map[string]*storage.Baseline),
		correlations: make([]storage.Correlation, 0),
		alerts:       make([]Alert, 0),
		insights:     make([]Insight, 0),
		rollingStats: make(map[string]*RollingStats),
	}

	// Load existing baselines from storage
	if store != nil {
		ctx := context.Background()
		if baselines, err := store.GetAllBaselines(ctx); err == nil {
			agent.baselines = baselines
		}
		if correlations, err := store.GetCorrelations(ctx); err == nil {
			agent.correlations = correlations
		}
	}

	return agent
}

// ProcessMetrics processes new metrics and updates learning state
func (a *Agent) ProcessMetrics(ctx context.Context, metrics []sparkpost.ProcessedMetrics) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, m := range metrics {
		// Update rolling statistics
		a.updateRollingStats(m)
		
		// Check for anomalies
		alerts := a.detectAnomalies(m)
		a.alerts = append(a.alerts, alerts...)
	}

	// Periodically update baselines
	if time.Since(a.lastBaselineUpdate) > time.Duration(a.config.BaselineRecalcHours)*time.Hour {
		a.recalculateBaselines(ctx)
		a.lastBaselineUpdate = time.Now()
	}

	// Periodically update correlations
	if time.Since(a.lastCorrelationUpdate) > time.Duration(a.config.CorrelationRecalcHours)*time.Hour {
		a.analyzeCorrelations(ctx)
		a.lastCorrelationUpdate = time.Now()
	}

	return nil
}

// ProcessISPMetrics processes ISP-specific metrics
func (a *Agent) ProcessISPMetrics(ctx context.Context, metrics []sparkpost.ISPMetrics) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, isp := range metrics {
		pm := isp.Metrics
		pm.GroupBy = "isp"
		pm.GroupValue = isp.Provider
		
		a.updateRollingStats(pm)
		
		alerts := a.detectAnomalies(pm)
		a.alerts = append(a.alerts, alerts...)
	}

	return nil
}

// EvaluateHealth evaluates the health status of metrics
func (a *Agent) EvaluateHealth(metrics sparkpost.ProcessedMetrics) (status string, reason string) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	key := fmt.Sprintf("%s:%s", metrics.GroupBy, metrics.GroupValue)
	baseline, exists := a.baselines[key]

	// If we have a learned baseline, use it
	if exists && baseline.DataPoints >= a.config.MinDataPoints {
		return a.evaluateWithBaseline(metrics, baseline)
	}

	// Fall back to general thresholds
	return a.evaluateWithDefaults(metrics)
}

// evaluateWithBaseline evaluates health using learned baselines
func (a *Agent) evaluateWithBaseline(metrics sparkpost.ProcessedMetrics, baseline *storage.Baseline) (string, string) {
	var reasons []string
	severity := "healthy"

	checkMetric := func(name string, value float64) {
		if mb, ok := baseline.Metrics[name]; ok && mb.StdDev > 0 {
			deviation := (value - mb.Mean) / mb.StdDev
			
			if math.Abs(deviation) > a.config.AnomalySigma*1.5 {
				severity = "critical"
				reasons = append(reasons, fmt.Sprintf("%s is %.1fσ from baseline", name, deviation))
			} else if math.Abs(deviation) > a.config.AnomalySigma {
				if severity != "critical" {
					severity = "warning"
				}
				reasons = append(reasons, fmt.Sprintf("%s is %.1fσ from baseline", name, deviation))
			}
		}
	}

	checkMetric("complaint_rate", metrics.ComplaintRate)
	checkMetric("bounce_rate", metrics.BounceRate)
	checkMetric("block_rate", metrics.BlockRate)
	checkMetric("delivery_rate", -metrics.DeliveryRate) // Inverted - lower is worse

	if len(reasons) == 0 {
		return "healthy", ""
	}
	return severity, reasons[0]
}

// evaluateWithDefaults evaluates health using default thresholds
func (a *Agent) evaluateWithDefaults(metrics sparkpost.ProcessedMetrics) (string, string) {
	// Critical thresholds
	if metrics.ComplaintRate > 0.0005 { // 0.05%
		return "critical", fmt.Sprintf("Complaint rate %.4f%% exceeds critical threshold", metrics.ComplaintRate*100)
	}
	if metrics.BounceRate > 0.05 { // 5%
		return "critical", fmt.Sprintf("Bounce rate %.2f%% exceeds critical threshold", metrics.BounceRate*100)
	}
	if metrics.BlockRate > 0.05 { // 5%
		return "critical", fmt.Sprintf("Block rate %.2f%% exceeds critical threshold", metrics.BlockRate*100)
	}

	// Warning thresholds
	if metrics.ComplaintRate > 0.0003 { // 0.03%
		return "warning", fmt.Sprintf("Complaint rate %.4f%% approaching threshold", metrics.ComplaintRate*100)
	}
	if metrics.BounceRate > 0.03 { // 3%
		return "warning", fmt.Sprintf("Bounce rate %.2f%% approaching threshold", metrics.BounceRate*100)
	}
	if metrics.BlockRate > 0.03 { // 3%
		return "warning", fmt.Sprintf("Block rate %.2f%% approaching threshold", metrics.BlockRate*100)
	}

	return "healthy", ""
}

// updateRollingStats updates rolling statistics for a metric
func (a *Agent) updateRollingStats(metrics sparkpost.ProcessedMetrics) {
	key := fmt.Sprintf("%s:%s", metrics.GroupBy, metrics.GroupValue)
	
	metricValues := map[string]float64{
		"complaint_rate": metrics.ComplaintRate,
		"bounce_rate":    metrics.BounceRate,
		"block_rate":     metrics.BlockRate,
		"delivery_rate":  metrics.DeliveryRate,
		"open_rate":      metrics.OpenRate,
		"click_rate":     metrics.ClickRate,
		"volume":         float64(metrics.Targeted),
	}

	for metricName, value := range metricValues {
		statsKey := fmt.Sprintf("%s:%s", key, metricName)
		
		stats, exists := a.rollingStats[statsKey]
		if !exists {
			stats = &RollingStats{
				MetricName: metricName,
				EntityType: metrics.GroupBy,
				EntityName: metrics.GroupValue,
				Values:     make([]float64, 0),
				Timestamps: make([]time.Time, 0),
				MaxSize:    10080, // 7 days at 1-minute intervals
			}
			a.rollingStats[statsKey] = stats
		}

		stats.Values = append(stats.Values, value)
		stats.Timestamps = append(stats.Timestamps, metrics.Timestamp)

		// Trim to max size
		if len(stats.Values) > stats.MaxSize {
			stats.Values = stats.Values[1:]
			stats.Timestamps = stats.Timestamps[1:]
		}
	}
}

// detectAnomalies detects anomalies in metrics
func (a *Agent) detectAnomalies(metrics sparkpost.ProcessedMetrics) []Alert {
	var alerts []Alert
	key := fmt.Sprintf("%s:%s", metrics.GroupBy, metrics.GroupValue)
	baseline, exists := a.baselines[key]

	if !exists || baseline.DataPoints < a.config.MinDataPoints {
		return alerts // Not enough data to detect anomalies
	}

	checkAnomaly := func(metricName string, value float64, higherIsBad bool) {
		mb, ok := baseline.Metrics[metricName]
		if !ok || mb.StdDev == 0 {
			return
		}

		deviation := (value - mb.Mean) / mb.StdDev
		
		// Check if this is an anomaly in the "bad" direction
		isBadAnomaly := (higherIsBad && deviation > a.config.AnomalySigma) ||
			(!higherIsBad && deviation < -a.config.AnomalySigma)

		if isBadAnomaly {
			severity := "warning"
			if math.Abs(deviation) > a.config.AnomalySigma*1.5 {
				severity = "critical"
			}

			alert := Alert{
				ID:            fmt.Sprintf("%s-%s-%d", key, metricName, time.Now().UnixNano()),
				Timestamp:     time.Now(),
				Severity:      severity,
				Category:      metricName,
				Title:         fmt.Sprintf("Anomaly detected: %s for %s", metricName, metrics.GroupValue),
				Description:   fmt.Sprintf("%s is %.2fσ from baseline (current: %.6f, baseline: %.6f)", 
					metricName, deviation, value, mb.Mean),
				EntityType:    metrics.GroupBy,
				EntityName:    metrics.GroupValue,
				MetricName:    metricName,
				CurrentValue:  value,
				BaselineValue: mb.Mean,
				Deviation:     deviation,
				Recommendation: a.getRecommendation(metricName, deviation, metrics.GroupValue),
			}
			alerts = append(alerts, alert)
		}
	}

	checkAnomaly("complaint_rate", metrics.ComplaintRate, true)
	checkAnomaly("bounce_rate", metrics.BounceRate, true)
	checkAnomaly("block_rate", metrics.BlockRate, true)
	checkAnomaly("delivery_rate", metrics.DeliveryRate, false) // Lower is bad

	return alerts
}

// getRecommendation generates a recommendation based on the anomaly
func (a *Agent) getRecommendation(metricName string, deviation float64, entityName string) string {
	switch metricName {
	case "complaint_rate":
		if deviation > 3 {
			return fmt.Sprintf("Critical: Consider pausing sends to %s until complaint rate normalizes. Review recent content changes.", entityName)
		}
		return fmt.Sprintf("Monitor %s closely. Review content and targeting for this segment.", entityName)
	case "bounce_rate":
		if deviation > 3 {
			return "Urgent: Check list hygiene and remove invalid addresses. Verify sending domain authentication."
		}
		return "Review bounce reasons and clean affected addresses from list."
	case "block_rate":
		if deviation > 3 {
			return "Critical: Check IP reputation on major blocklists. Consider shifting traffic to healthy IPs."
		}
		return "Monitor IP reputation. Review sending patterns that may trigger blocks."
	case "delivery_rate":
		return "Investigate delivery issues. Check bounce and delay reasons for root cause."
	default:
		return "Monitor this metric and investigate if the trend continues."
	}
}

// recalculateBaselines recalculates baselines from rolling statistics
func (a *Agent) recalculateBaselines(ctx context.Context) {
	// Group rolling stats by entity
	entityStats := make(map[string]map[string]*RollingStats)
	
	for _, stats := range a.rollingStats {
		key := fmt.Sprintf("%s:%s", stats.EntityType, stats.EntityName)
		if _, ok := entityStats[key]; !ok {
			entityStats[key] = make(map[string]*RollingStats)
		}
		entityStats[key][stats.MetricName] = stats
	}

	for key, metrics := range entityStats {
		baseline := &storage.Baseline{
			EntityType: "",
			EntityName: "",
			Metrics:    make(map[string]*storage.MetricBaseline),
			UpdatedAt:  time.Now(),
		}

		// Parse key
		for k, stats := range metrics {
			if baseline.EntityType == "" {
				baseline.EntityType = stats.EntityType
				baseline.EntityName = stats.EntityName
			}

			if len(stats.Values) < a.config.MinDataPoints {
				continue
			}

			mb := calculateMetricBaseline(stats.Values)
			baseline.Metrics[k] = mb
			baseline.DataPoints = len(stats.Values)
		}

		if len(baseline.Metrics) > 0 {
			a.baselines[key] = baseline
			
			// Persist to storage
			if a.storage != nil {
				a.storage.SaveBaseline(ctx, baseline)
			}
		}
	}
}

// calculateMetricBaseline calculates statistical baseline from values
func calculateMetricBaseline(values []float64) *storage.MetricBaseline {
	if len(values) == 0 {
		return nil
	}

	// Calculate mean
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(len(values))

	// Calculate standard deviation
	sumSquares := 0.0
	for _, v := range values {
		sumSquares += (v - mean) * (v - mean)
	}
	stdDev := math.Sqrt(sumSquares / float64(len(values)))

	// Sort for percentiles
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)

	percentile := func(p float64) float64 {
		idx := int(float64(len(sorted)-1) * p)
		return sorted[idx]
	}

	return &storage.MetricBaseline{
		Mean:         mean,
		StdDev:       stdDev,
		Min:          sorted[0],
		Max:          sorted[len(sorted)-1],
		Percentile50: percentile(0.50),
		Percentile75: percentile(0.75),
		Percentile90: percentile(0.90),
		Percentile95: percentile(0.95),
		Percentile99: percentile(0.99),
		Values:       values,
	}
}

// analyzeCorrelations analyzes correlations between metrics
func (a *Agent) analyzeCorrelations(ctx context.Context) {
	// Group data by entity
	entityData := make(map[string]map[string][]float64)
	
	for _, stats := range a.rollingStats {
		key := fmt.Sprintf("%s:%s", stats.EntityType, stats.EntityName)
		if _, ok := entityData[key]; !ok {
			entityData[key] = make(map[string][]float64)
		}
		entityData[key][stats.MetricName] = stats.Values
	}

	var correlations []storage.Correlation

	for key, metrics := range entityData {
		// Check volume vs complaint correlation
		if volumes, ok := metrics["volume"]; ok {
			if complaints, ok := metrics["complaint_rate"]; ok && len(volumes) == len(complaints) {
				corr := analyzeCorrelation(volumes, complaints)
				if corr.Correlation > 0.5 { // Significant positive correlation
					// Find threshold where complaints spike
					threshold := findThreshold(volumes, complaints)
					if threshold > 0 {
						var entityType, entityName string
						fmt.Sscanf(key, "%s:%s", &entityType, &entityName)
						
						correlations = append(correlations, storage.Correlation{
							EntityType:       entityType,
							EntityName:       entityName,
							TriggerMetric:    "volume",
							TriggerThreshold: threshold,
							TriggerOperator:  "gt",
							EffectMetric:     "complaint_rate",
							EffectChange:     corr.EffectSize,
							Confidence:       corr.Correlation,
							Occurrences:      corr.Occurrences,
							LastObserved:     time.Now(),
						})
					}
				}
			}
		}
	}

	a.correlations = correlations
	
	// Persist
	if a.storage != nil {
		a.storage.SaveCorrelations(ctx, correlations)
	}
}

// CorrelationResult holds correlation analysis results
type CorrelationResult struct {
	Correlation float64
	EffectSize  float64
	Occurrences int
}

// analyzeCorrelation analyzes correlation between two metric series
func analyzeCorrelation(x, y []float64) CorrelationResult {
	if len(x) != len(y) || len(x) < 10 {
		return CorrelationResult{}
	}

	// Calculate means
	meanX, meanY := 0.0, 0.0
	for i := range x {
		meanX += x[i]
		meanY += y[i]
	}
	meanX /= float64(len(x))
	meanY /= float64(len(y))

	// Calculate correlation coefficient
	numerator := 0.0
	denomX, denomY := 0.0, 0.0
	for i := range x {
		dx := x[i] - meanX
		dy := y[i] - meanY
		numerator += dx * dy
		denomX += dx * dx
		denomY += dy * dy
	}

	if denomX == 0 || denomY == 0 {
		return CorrelationResult{}
	}

	correlation := numerator / (math.Sqrt(denomX) * math.Sqrt(denomY))

	return CorrelationResult{
		Correlation: correlation,
		EffectSize:  0.0, // Would need more sophisticated analysis
		Occurrences: len(x),
	}
}

// findThreshold finds the volume threshold where complaints increase
func findThreshold(volumes, complaints []float64) float64 {
	if len(volumes) < 20 {
		return 0
	}

	// Sort by volume and find where complaints start increasing
	type pair struct {
		volume    float64
		complaint float64
	}
	pairs := make([]pair, len(volumes))
	for i := range volumes {
		pairs[i] = pair{volumes[i], complaints[i]}
	}
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].volume < pairs[j].volume
	})

	// Find the 75th percentile of volume where complaints increase
	idx := int(float64(len(pairs)) * 0.75)
	return pairs[idx].volume
}

// GetAlerts returns current alerts
func (a *Agent) GetAlerts() []Alert {
	a.mu.RLock()
	defer a.mu.RUnlock()

	result := make([]Alert, len(a.alerts))
	copy(result, a.alerts)
	return result
}

// GetInsights returns current insights
func (a *Agent) GetInsights() []Insight {
	a.mu.RLock()
	defer a.mu.RUnlock()

	result := make([]Insight, len(a.insights))
	copy(result, a.insights)
	return result
}

// GetCorrelations returns learned correlations
func (a *Agent) GetCorrelations() []storage.Correlation {
	a.mu.RLock()
	defer a.mu.RUnlock()

	result := make([]storage.Correlation, len(a.correlations))
	copy(result, a.correlations)
	return result
}

// GetBaselines returns learned baselines
func (a *Agent) GetBaselines() map[string]*storage.Baseline {
	a.mu.RLock()
	defer a.mu.RUnlock()

	result := make(map[string]*storage.Baseline)
	for k, v := range a.baselines {
		result[k] = v
	}
	return result
}

// ClearAlerts clears all alerts
func (a *Agent) ClearAlerts() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.alerts = make([]Alert, 0)
}

// AcknowledgeAlert acknowledges an alert
func (a *Agent) AcknowledgeAlert(alertID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	for i := range a.alerts {
		if a.alerts[i].ID == alertID {
			a.alerts[i].Acknowledged = true
			return true
		}
	}
	return false
}

// Mailgun-specific methods

// ProcessMailgunMetrics processes new Mailgun metrics and updates learning state
func (a *Agent) ProcessMailgunMetrics(ctx context.Context, metrics []mailgun.ProcessedMetrics) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, m := range metrics {
		// Convert to internal format for rolling stats
		pm := sparkpost.ProcessedMetrics{
			Timestamp:       m.Timestamp,
			Source:          m.Source,
			GroupBy:         "mailgun:" + m.GroupBy,
			GroupValue:      m.GroupValue,
			Targeted:        m.Targeted,
			Delivered:       m.Delivered,
			UniqueOpened:    m.UniqueOpened,
			UniqueClicked:   m.UniqueClicked,
			Bounced:         m.Bounced,
			HardBounced:     m.HardBounced,
			SoftBounced:     m.SoftBounced,
			BlockBounced:    m.BlockBounced,
			Complaints:      m.Complaints,
			Unsubscribes:    m.Unsubscribes,
			DeliveryRate:    m.DeliveryRate,
			OpenRate:        m.OpenRate,
			ClickRate:       m.ClickRate,
			BounceRate:      m.BounceRate,
			HardBounceRate:  m.HardBounceRate,
			SoftBounceRate:  m.SoftBounceRate,
			BlockRate:       m.BlockRate,
			ComplaintRate:   m.ComplaintRate,
			UnsubscribeRate: m.UnsubscribeRate,
		}

		// Update rolling statistics
		a.updateRollingStats(pm)

		// Check for anomalies
		alerts := a.detectAnomalies(pm)
		a.alerts = append(a.alerts, alerts...)
	}

	// Periodically update baselines
	if time.Since(a.lastBaselineUpdate) > time.Duration(a.config.BaselineRecalcHours)*time.Hour {
		a.recalculateBaselines(ctx)
		a.lastBaselineUpdate = time.Now()
	}

	return nil
}

// ProcessMailgunISPMetrics processes Mailgun ISP-specific metrics
func (a *Agent) ProcessMailgunISPMetrics(ctx context.Context, metrics []mailgun.ISPMetrics) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, isp := range metrics {
		m := isp.Metrics
		pm := sparkpost.ProcessedMetrics{
			Timestamp:       m.Timestamp,
			Source:          m.Source,
			GroupBy:         "mailgun:isp",
			GroupValue:      isp.Provider,
			Targeted:        m.Targeted,
			Delivered:       m.Delivered,
			UniqueOpened:    m.UniqueOpened,
			UniqueClicked:   m.UniqueClicked,
			Bounced:         m.Bounced,
			Complaints:      m.Complaints,
			Unsubscribes:    m.Unsubscribes,
			DeliveryRate:    m.DeliveryRate,
			OpenRate:        m.OpenRate,
			ClickRate:       m.ClickRate,
			BounceRate:      m.BounceRate,
			ComplaintRate:   m.ComplaintRate,
			UnsubscribeRate: m.UnsubscribeRate,
		}

		a.updateRollingStats(pm)

		alerts := a.detectAnomalies(pm)
		a.alerts = append(a.alerts, alerts...)
	}

	return nil
}

// EvaluateMailgunHealth evaluates the health status of Mailgun metrics
func (a *Agent) EvaluateMailgunHealth(metrics mailgun.ProcessedMetrics) (status string, reason string) {
	// Convert to SparkPost format for evaluation
	pm := sparkpost.ProcessedMetrics{
		GroupBy:         "mailgun:" + metrics.GroupBy,
		GroupValue:      metrics.GroupValue,
		ComplaintRate:   metrics.ComplaintRate,
		BounceRate:      metrics.BounceRate,
		BlockRate:       metrics.BlockRate,
		DeliveryRate:    metrics.DeliveryRate,
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	key := fmt.Sprintf("%s:%s", pm.GroupBy, pm.GroupValue)
	baseline, exists := a.baselines[key]

	// If we have a learned baseline, use it
	if exists && baseline.DataPoints >= a.config.MinDataPoints {
		return a.evaluateWithBaseline(pm, baseline)
	}

	// Fall back to general thresholds
	return a.evaluateWithDefaults(pm)
}

// SES-specific methods

// ProcessSESMetrics processes new SES metrics and updates learning state
func (a *Agent) ProcessSESMetrics(ctx context.Context, metrics []ses.ProcessedMetrics) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, m := range metrics {
		// Convert to internal format for rolling stats
		pm := sparkpost.ProcessedMetrics{
			Timestamp:       m.Timestamp,
			Source:          m.Source,
			GroupBy:         "ses:" + m.GroupBy,
			GroupValue:      m.GroupValue,
			Targeted:        m.Targeted,
			Delivered:       m.Delivered,
			UniqueOpened:    m.UniqueOpened,
			UniqueClicked:   m.UniqueClicked,
			Bounced:         m.Bounced,
			HardBounced:     m.HardBounced,
			SoftBounced:     m.SoftBounced,
			BlockBounced:    m.BlockBounced,
			Complaints:      m.Complaints,
			Unsubscribes:    m.Unsubscribes,
			DeliveryRate:    m.DeliveryRate,
			OpenRate:        m.OpenRate,
			ClickRate:       m.ClickRate,
			BounceRate:      m.BounceRate,
			HardBounceRate:  m.HardBounceRate,
			SoftBounceRate:  m.SoftBounceRate,
			BlockRate:       m.BlockRate,
			ComplaintRate:   m.ComplaintRate,
			UnsubscribeRate: m.UnsubscribeRate,
		}

		// Update rolling statistics
		a.updateRollingStats(pm)

		// Check for anomalies
		alerts := a.detectAnomalies(pm)
		a.alerts = append(a.alerts, alerts...)
	}

	// Periodically update baselines
	if time.Since(a.lastBaselineUpdate) > time.Duration(a.config.BaselineRecalcHours)*time.Hour {
		a.recalculateBaselines(ctx)
		a.lastBaselineUpdate = time.Now()
	}

	return nil
}

// ProcessSESISPMetrics processes SES ISP-specific metrics
func (a *Agent) ProcessSESISPMetrics(ctx context.Context, metrics []ses.ISPMetrics) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, isp := range metrics {
		m := isp.Metrics
		pm := sparkpost.ProcessedMetrics{
			Timestamp:       m.Timestamp,
			Source:          m.Source,
			GroupBy:         "ses:isp",
			GroupValue:      isp.Provider,
			Targeted:        m.Targeted,
			Delivered:       m.Delivered,
			UniqueOpened:    m.UniqueOpened,
			UniqueClicked:   m.UniqueClicked,
			Bounced:         m.Bounced,
			Complaints:      m.Complaints,
			Unsubscribes:    m.Unsubscribes,
			DeliveryRate:    m.DeliveryRate,
			OpenRate:        m.OpenRate,
			ClickRate:       m.ClickRate,
			BounceRate:      m.BounceRate,
			ComplaintRate:   m.ComplaintRate,
			UnsubscribeRate: m.UnsubscribeRate,
		}

		a.updateRollingStats(pm)

		alerts := a.detectAnomalies(pm)
		a.alerts = append(a.alerts, alerts...)
	}

	return nil
}

// EvaluateSESHealth evaluates the health status of SES metrics
func (a *Agent) EvaluateSESHealth(metrics ses.ProcessedMetrics) (status string, reason string) {
	// Convert to SparkPost format for evaluation
	pm := sparkpost.ProcessedMetrics{
		GroupBy:         "ses:" + metrics.GroupBy,
		GroupValue:      metrics.GroupValue,
		ComplaintRate:   metrics.ComplaintRate,
		BounceRate:      metrics.BounceRate,
		BlockRate:       metrics.BlockRate,
		DeliveryRate:    metrics.DeliveryRate,
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	key := fmt.Sprintf("%s:%s", pm.GroupBy, pm.GroupValue)
	baseline, exists := a.baselines[key]

	// If we have a learned baseline, use it
	if exists && baseline.DataPoints >= a.config.MinDataPoints {
		return a.evaluateWithBaseline(pm, baseline)
	}

	// Fall back to general thresholds
	return a.evaluateWithDefaults(pm)
}
