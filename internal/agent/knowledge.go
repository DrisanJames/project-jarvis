package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StorageBackend defines the interface for knowledge base persistence
type StorageBackend interface {
	SaveKnowledgeBase(ctx context.Context, kb *KnowledgeBase) error
	LoadKnowledgeBase(ctx context.Context) (*KnowledgeBase, error)
}

// KnowledgeBase is the agent's persistent memory and learning system
type KnowledgeBase struct {
	mu sync.RWMutex

	// Persistent storage path (local file - legacy)
	storagePath string
	
	// S3 storage backend (preferred)
	s3Storage *S3Storage

	// Learned ecosystem knowledge
	EcosystemState      *EcosystemKnowledge     `json:"ecosystem_state"`
	LearnedPatterns     []LearnedPattern        `json:"learned_patterns"`
	HistoricalInsights  []HistoricalInsight     `json:"historical_insights"`
	PerformanceBenchmarks map[string]*Benchmark `json:"benchmarks"`
	ISPKnowledge        map[string]*ISPProfile  `json:"isp_knowledge"`
	
	// Industry best practices (static + learned)
	BestPractices       *IndustryBestPractices  `json:"best_practices"`
	ComplianceRules     *ComplianceKnowledge    `json:"compliance_rules"`
	
	// Recent analysis results
	LastAnalysis        *AnalysisResult         `json:"last_analysis"`
	AnalysisHistory     []AnalysisResult        `json:"analysis_history"`
	
	// Learning metadata
	LastLearningRun     time.Time               `json:"last_learning_run"`
	TotalLearningCycles int                     `json:"total_learning_cycles"`
	DataPointsAnalyzed  int64                   `json:"data_points_analyzed"`
}

// EcosystemKnowledge represents the agent's understanding of the email ecosystem
type EcosystemKnowledge struct {
	// Current state assessment
	OverallHealth       string    `json:"overall_health"` // healthy, warning, critical
	HealthScore         float64   `json:"health_score"`   // 0-100
	LastAssessment      time.Time `json:"last_assessment"`
	
	// Volume metrics
	DailyAverageVolume  int64     `json:"daily_avg_volume"`
	WeeklyTrend         float64   `json:"weekly_trend"`    // % change
	MonthlyTrend        float64   `json:"monthly_trend"`
	
	// Deliverability metrics (learned baselines)
	BaselineDeliveryRate   float64 `json:"baseline_delivery_rate"`
	BaselineOpenRate       float64 `json:"baseline_open_rate"`
	BaselineClickRate      float64 `json:"baseline_click_rate"`
	BaselineBounceRate     float64 `json:"baseline_bounce_rate"`
	BaselineComplaintRate  float64 `json:"baseline_complaint_rate"`
	
	// Revenue metrics
	DailyAverageRevenue    float64 `json:"daily_avg_revenue"`
	RevenueTrend           float64 `json:"revenue_trend"`
	TopRevenueOffers       []string `json:"top_revenue_offers"`
	TopRevenueProperties   []string `json:"top_revenue_properties"`
	
	// ESP distribution
	ESPVolumeShare         map[string]float64 `json:"esp_volume_share"`
	ESPHealthStatus        map[string]string  `json:"esp_health_status"`
	
	// Key issues identified
	ActiveIssues           []string `json:"active_issues"`
	ResolvedIssues         []string `json:"resolved_issues"`
}

// LearnedPattern represents a pattern the agent has learned from data
type LearnedPattern struct {
	ID              string    `json:"id"`
	Type            string    `json:"type"` // correlation, trend, anomaly, best_practice
	Description     string    `json:"description"`
	Confidence      float64   `json:"confidence"` // 0-1
	Occurrences     int       `json:"occurrences"`
	LastObserved    time.Time `json:"last_observed"`
	FirstObserved   time.Time `json:"first_observed"`
	
	// Pattern details
	TriggerCondition string   `json:"trigger_condition"`
	ExpectedOutcome  string   `json:"expected_outcome"`
	
	// Impact assessment
	RevenueImpact    float64  `json:"revenue_impact"`   // Estimated $ impact
	HealthImpact     string   `json:"health_impact"`    // positive, negative, neutral
	
	// Actionable recommendation
	Recommendation   string   `json:"recommendation"`
}

// HistoricalInsight represents an insight derived from historical analysis
type HistoricalInsight struct {
	ID              string    `json:"id"`
	GeneratedAt     time.Time `json:"generated_at"`
	TimeRange       string    `json:"time_range"` // "7d", "30d", "90d", "365d"
	Category        string    `json:"category"`   // revenue, deliverability, engagement, compliance
	
	Title           string    `json:"title"`
	Summary         string    `json:"summary"`
	DetailedAnalysis string   `json:"detailed_analysis"`
	
	KeyFindings     []string  `json:"key_findings"`
	Recommendations []string  `json:"recommendations"`
	
	// Metrics referenced
	MetricsUsed     []string  `json:"metrics_used"`
	DataSources     []string  `json:"data_sources"`
}

// Benchmark represents a performance benchmark for the ecosystem
type Benchmark struct {
	MetricName      string    `json:"metric_name"`
	EntityType      string    `json:"entity_type"` // ecosystem, esp, isp, campaign
	EntityName      string    `json:"entity_name"`
	
	// Current performance
	CurrentValue    float64   `json:"current_value"`
	CurrentDate     time.Time `json:"current_date"`
	
	// Benchmark targets
	TargetValue     float64   `json:"target_value"`
	IndustryAverage float64   `json:"industry_average"`
	BestInClass     float64   `json:"best_in_class"`
	
	// Historical performance
	Last7Days       float64   `json:"last_7_days"`
	Last30Days      float64   `json:"last_30_days"`
	Last90Days      float64   `json:"last_90_days"`
	
	// Trend
	Trend           string    `json:"trend"` // improving, stable, declining
	TrendPercentage float64   `json:"trend_percentage"`
	
	// Assessment
	Status          string    `json:"status"` // exceeding, meeting, below
	Gap             float64   `json:"gap"`    // Distance from target
}

// ISPProfile represents knowledge about a specific ISP
type ISPProfile struct {
	Name            string    `json:"name"`
	
	// Current performance with this ISP
	DeliveryRate    float64   `json:"delivery_rate"`
	OpenRate        float64   `json:"open_rate"`
	ClickRate       float64   `json:"click_rate"`
	BounceRate      float64   `json:"bounce_rate"`
	ComplaintRate   float64   `json:"complaint_rate"`
	
	// Historical baseline
	BaselineDeliveryRate  float64 `json:"baseline_delivery_rate"`
	BaselineBounceRate    float64 `json:"baseline_bounce_rate"`
	BaselineComplaintRate float64 `json:"baseline_complaint_rate"`
	
	// ISP-specific thresholds (industry knowledge)
	MaxComplaintRate      float64 `json:"max_complaint_rate"`
	MaxBounceRate         float64 `json:"max_bounce_rate"`
	
	// Health status
	Status          string    `json:"status"`
	StatusReason    string    `json:"status_reason"`
	
	// Known issues and recommendations
	KnownIssues     []string  `json:"known_issues"`
	Recommendations []string  `json:"recommendations"`
	
	// Best practices for this ISP
	BestPractices   []string  `json:"best_practices"`
}

// IndustryBestPractices contains email marketing best practices
type IndustryBestPractices struct {
	// Deliverability
	DeliveryRateTarget      float64  `json:"delivery_rate_target"`      // 95%+
	BounceRateThreshold     float64  `json:"bounce_rate_threshold"`     // <2%
	ComplaintRateThreshold  float64  `json:"complaint_rate_threshold"`  // <0.1%
	
	// Engagement
	OpenRateHealthy         float64  `json:"open_rate_healthy"`         // 15-25%
	ClickRateHealthy        float64  `json:"click_rate_healthy"`        // 2-5%
	UnsubscribeRateMax      float64  `json:"unsubscribe_rate_max"`      // <0.5%
	
	// List Hygiene
	ListCleaningFrequency   string   `json:"list_cleaning_frequency"`   // Monthly
	InactiveSubscriberDays  int      `json:"inactive_subscriber_days"`  // 90-180 days
	
	// ISP-Specific Guidelines
	GmailGuidelines         []string `json:"gmail_guidelines"`
	YahooGuidelines         []string `json:"yahoo_guidelines"`
	OutlookGuidelines       []string `json:"outlook_guidelines"`
	AppleGuidelines         []string `json:"apple_guidelines"`
	
	// General Best Practices
	WarmupGuidelines        []string `json:"warmup_guidelines"`
	AuthenticationRequired  []string `json:"authentication_required"` // SPF, DKIM, DMARC
	ContentGuidelines       []string `json:"content_guidelines"`
	SendTimeOptimization    []string `json:"send_time_optimization"`
}

// ComplianceKnowledge contains email compliance rules
type ComplianceKnowledge struct {
	// CAN-SPAM Requirements
	CANSPAMRules           []string `json:"canspam_rules"`
	
	// GDPR Requirements
	GDPRRules              []string `json:"gdpr_rules"`
	
	// CCPA Requirements
	CCPARules              []string `json:"ccpa_rules"`
	
	// CASL (Canada) Requirements
	CASLRules              []string `json:"casl_rules"`
	
	// General compliance checks
	RequiredElements       []string `json:"required_elements"`
	ProhibitedPractices    []string `json:"prohibited_practices"`
}

// AnalysisResult represents the result of an hourly analysis cycle
type AnalysisResult struct {
	Timestamp           time.Time `json:"timestamp"`
	AnalysisDuration    string    `json:"analysis_duration"`
	
	// Health summary
	EcosystemHealth     string    `json:"ecosystem_health"`
	HealthScore         float64   `json:"health_score"`
	
	// Key metrics snapshot
	TotalVolume24h      int64     `json:"total_volume_24h"`
	TotalRevenue24h     float64   `json:"total_revenue_24h"`
	AvgDeliveryRate     float64   `json:"avg_delivery_rate"`
	AvgBounceRate       float64   `json:"avg_bounce_rate"`
	AvgComplaintRate    float64   `json:"avg_complaint_rate"`
	
	// Issues found
	CriticalIssues      []string  `json:"critical_issues"`
	WarningIssues       []string  `json:"warning_issues"`
	
	// Recommendations
	ImmediateActions    []string  `json:"immediate_actions"`
	ShortTermActions    []string  `json:"short_term_actions"`
	LongTermActions     []string  `json:"long_term_actions"`
	
	// Patterns detected
	NewPatternsFound    int       `json:"new_patterns_found"`
	TrendChanges        []string  `json:"trend_changes"`
	
	// Task awareness
	ActiveKanbanTasks   int       `json:"active_kanban_tasks"`
	TasksCreatedThisCycle int     `json:"tasks_created_this_cycle"`
}

// KnowledgeBaseConfig contains configuration for the knowledge base
type KnowledgeBaseConfig struct {
	// Local file storage (legacy)
	LocalPath string
	
	// S3 storage configuration (preferred)
	S3Bucket        string
	S3Prefix        string
	S3Region        string
	S3EncryptionKey string // Base64-encoded 32-byte AES-256 key
	S3Compress      bool
}

// NewKnowledgeBase creates a new knowledge base with industry defaults
func NewKnowledgeBase(storagePath string) *KnowledgeBase {
	return NewKnowledgeBaseWithConfig(KnowledgeBaseConfig{
		LocalPath: storagePath,
	})
}

// NewKnowledgeBaseWithConfig creates a knowledge base with full configuration
func NewKnowledgeBaseWithConfig(cfg KnowledgeBaseConfig) *KnowledgeBase {
	kb := &KnowledgeBase{
		storagePath:         cfg.LocalPath,
		EcosystemState:      &EcosystemKnowledge{},
		LearnedPatterns:     make([]LearnedPattern, 0),
		HistoricalInsights:  make([]HistoricalInsight, 0),
		PerformanceBenchmarks: make(map[string]*Benchmark),
		ISPKnowledge:        make(map[string]*ISPProfile),
		AnalysisHistory:     make([]AnalysisResult, 0),
		BestPractices:       initializeIndustryBestPractices(),
		ComplianceRules:     initializeComplianceKnowledge(),
	}
	
	// Initialize S3 storage if configured
	if cfg.S3Bucket != "" {
		s3Cfg := S3StorageConfig{
			Bucket:        cfg.S3Bucket,
			Prefix:        cfg.S3Prefix,
			Region:        cfg.S3Region,
			EncryptionKey: cfg.S3EncryptionKey,
			Compress:      cfg.S3Compress,
		}
		
		s3Storage, err := NewS3Storage(s3Cfg)
		if err != nil {
			log.Printf("KnowledgeBase: Failed to initialize S3 storage: %v (falling back to local)", err)
		} else {
			kb.s3Storage = s3Storage
			log.Printf("KnowledgeBase: Using S3 storage - bucket=%s, prefix=%s", cfg.S3Bucket, cfg.S3Prefix)
		}
	}
	
	// Initialize ISP profiles with industry knowledge
	kb.initializeISPProfiles()
	
	// Try to load existing knowledge
	if err := kb.Load(); err != nil {
		log.Printf("KnowledgeBase: Starting fresh (no existing data or error: %v)", err)
	} else {
		log.Printf("KnowledgeBase: Loaded existing knowledge - %d patterns, %d insights, %d analysis cycles",
			len(kb.LearnedPatterns), len(kb.HistoricalInsights), kb.TotalLearningCycles)
	}
	
	return kb
}

// SetS3Storage sets the S3 storage backend
func (kb *KnowledgeBase) SetS3Storage(s3 *S3Storage) {
	kb.mu.Lock()
	defer kb.mu.Unlock()
	kb.s3Storage = s3
	log.Printf("KnowledgeBase: S3 storage backend configured - bucket=%s", s3.GetBucket())
}

// initializeIndustryBestPractices sets up default industry best practices
func initializeIndustryBestPractices() *IndustryBestPractices {
	return &IndustryBestPractices{
		// Deliverability targets
		DeliveryRateTarget:     0.95,  // 95%
		BounceRateThreshold:    0.02,  // 2%
		ComplaintRateThreshold: 0.001, // 0.1%
		
		// Engagement targets
		OpenRateHealthy:     0.18,   // 18%
		ClickRateHealthy:    0.025,  // 2.5%
		UnsubscribeRateMax:  0.005,  // 0.5%
		
		// List hygiene
		ListCleaningFrequency:  "Monthly",
		InactiveSubscriberDays: 120,
		
		// Gmail guidelines
		GmailGuidelines: []string{
			"Keep complaint rate below 0.1% (Google postmaster recommendation)",
			"Maintain consistent sending volume - avoid sudden spikes",
			"Use Gmail Postmaster Tools to monitor reputation",
			"Implement one-click unsubscribe header",
			"Authenticate with SPF, DKIM, and DMARC",
			"Maintain user engagement - remove inactive subscribers",
		},
		
		// Yahoo guidelines
		YahooGuidelines: []string{
			"Keep complaint rate below 0.1%",
			"Honor unsubscribe requests within 10 days (CAN-SPAM)",
			"Use consistent From address and sending domain",
			"Implement List-Unsubscribe header",
			"Monitor Yahoo Sender Hub for feedback",
		},
		
		// Outlook/Microsoft guidelines
		OutlookGuidelines: []string{
			"Register with Microsoft SNDS (Smart Network Data Services)",
			"Maintain clean sending reputation on shared IPs",
			"Use consistent sending patterns",
			"Implement DMARC with at least p=quarantine",
			"Avoid spam trigger words in subject lines",
		},
		
		// Apple guidelines
		AppleGuidelines: []string{
			"Apple Mail Privacy Protection affects open tracking",
			"Focus on click metrics over open metrics for Apple users",
			"Implement authentication properly for iCloud delivery",
		},
		
		// Warmup guidelines
		WarmupGuidelines: []string{
			"Start with engaged subscribers for new IPs/domains",
			"Increase volume gradually over 4-6 weeks",
			"Monitor deliverability metrics closely during warmup",
			"Target 15-20% volume increase per day maximum",
			"Send to Gmail first (most forgiving during warmup)",
		},
		
		// Authentication
		AuthenticationRequired: []string{
			"SPF (Sender Policy Framework) - REQUIRED",
			"DKIM (DomainKeys Identified Mail) - REQUIRED",
			"DMARC (Domain-based Message Authentication) - HIGHLY RECOMMENDED",
			"BIMI (Brand Indicators for Message Identification) - OPTIONAL but beneficial",
		},
		
		// Content guidelines
		ContentGuidelines: []string{
			"Maintain healthy text-to-image ratio",
			"Avoid excessive capitalization and punctuation",
			"Include clear unsubscribe link",
			"Use recognizable From name and address",
			"Test subject lines for spam score before sending",
		},
		
		// Send time optimization
		SendTimeOptimization: []string{
			"Tuesday-Thursday typically show highest engagement",
			"10am-2pm local time often performs best",
			"Avoid Monday mornings and Friday afternoons",
			"Test and learn for your specific audience",
			"Consider time zone optimization for global lists",
		},
	}
}

// initializeComplianceKnowledge sets up compliance rules
func initializeComplianceKnowledge() *ComplianceKnowledge {
	return &ComplianceKnowledge{
		CANSPAMRules: []string{
			"Do not use false or misleading header information",
			"Do not use deceptive subject lines",
			"Identify the message as an advertisement",
			"Include valid physical postal address",
			"Provide clear opt-out mechanism",
			"Honor opt-out requests within 10 business days",
			"Monitor third-party email marketing",
		},
		
		GDPRRules: []string{
			"Obtain explicit consent before sending marketing emails",
			"Provide easy way to withdraw consent",
			"Keep records of consent",
			"Allow data subject access requests",
			"Implement right to erasure (right to be forgotten)",
			"Report data breaches within 72 hours",
		},
		
		CCPARules: []string{
			"Provide notice at collection of personal information",
			"Honor do-not-sell requests",
			"Provide opt-out of sale of personal information",
			"Respond to consumer requests within 45 days",
		},
		
		CASLRules: []string{
			"Obtain express or implied consent before sending",
			"Include sender identification",
			"Include functional unsubscribe mechanism",
			"Honor unsubscribe requests within 10 business days",
			"Keep consent records for proof",
		},
		
		RequiredElements: []string{
			"Clear sender identification (From name/address)",
			"Functional unsubscribe link",
			"Physical mailing address",
			"Clear subject line (not deceptive)",
			"Ad disclosure (if applicable)",
		},
		
		ProhibitedPractices: []string{
			"Purchasing email lists",
			"Sending without consent",
			"Hidden or broken unsubscribe links",
			"Deceptive subject lines",
			"Misleading From addresses",
			"Ignoring unsubscribe requests",
		},
	}
}

// initializeISPProfiles sets up ISP-specific knowledge
func (kb *KnowledgeBase) initializeISPProfiles() {
	kb.ISPKnowledge = map[string]*ISPProfile{
		"gmail": {
			Name:                 "Gmail",
			MaxComplaintRate:     0.001, // 0.1%
			MaxBounceRate:        0.02,  // 2%
			BestPractices: []string{
				"Use Google Postmaster Tools for reputation monitoring",
				"Implement one-click unsubscribe (RFC 8058)",
				"Keep complaint rate below 0.1%",
				"Maintain consistent sending volume",
			},
		},
		"yahoo": {
			Name:                 "Yahoo",
			MaxComplaintRate:     0.001,
			MaxBounceRate:        0.02,
			BestPractices: []string{
				"Monitor Yahoo Sender Hub",
				"Use List-Unsubscribe header",
				"Maintain engagement to avoid throttling",
			},
		},
		"outlook": {
			Name:                 "Outlook/Microsoft",
			MaxComplaintRate:     0.001,
			MaxBounceRate:        0.02,
			BestPractices: []string{
				"Register with Microsoft SNDS",
				"Monitor Junk Mail Reporting Program (JMRP)",
				"Implement DMARC properly",
			},
		},
		"apple": {
			Name:                 "Apple Mail",
			MaxComplaintRate:     0.001,
			MaxBounceRate:        0.02,
			BestPractices: []string{
				"Understand Mail Privacy Protection impact on opens",
				"Focus on click metrics over open metrics",
				"Ensure proper authentication for iCloud",
			},
		},
		"aol": {
			Name:                 "AOL",
			MaxComplaintRate:     0.001,
			MaxBounceRate:        0.02,
			BestPractices: []string{
				"Monitor AOL Postmaster feedback",
				"Maintain good IP reputation",
			},
		},
	}
}

// Save persists the knowledge base to storage (S3 preferred, falls back to local)
func (kb *KnowledgeBase) Save() error {
	// Try S3 first if configured
	if kb.s3Storage != nil {
		ctx := context.Background()
		if err := kb.s3Storage.SaveKnowledgeBase(ctx, kb); err != nil {
			log.Printf("KnowledgeBase: S3 save failed: %v (trying local fallback)", err)
		} else {
			return nil // S3 save succeeded
		}
	}
	
	// Fall back to local storage
	return kb.saveLocal()
}

// saveLocal saves the knowledge base to local disk
func (kb *KnowledgeBase) saveLocal() error {
	kb.mu.RLock()
	defer kb.mu.RUnlock()
	
	if kb.storagePath == "" {
		return nil
	}
	
	// Ensure directory exists
	dir := filepath.Dir(kb.storagePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}
	
	data, err := json.MarshalIndent(kb, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal knowledge base: %w", err)
	}
	
	if err := os.WriteFile(kb.storagePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write knowledge base: %w", err)
	}
	
	log.Printf("KnowledgeBase: Saved to local file %s", kb.storagePath)
	return nil
}

// Load loads the knowledge base from storage (S3 preferred, falls back to local)
func (kb *KnowledgeBase) Load() error {
	// Try S3 first if configured
	if kb.s3Storage != nil {
		ctx := context.Background()
		loaded, err := kb.s3Storage.LoadKnowledgeBase(ctx)
		if err == nil {
			kb.mergeLoaded(loaded)
			log.Printf("KnowledgeBase: Loaded from S3")
			return nil
		}
		log.Printf("KnowledgeBase: S3 load failed: %v (trying local fallback)", err)
	}
	
	// Fall back to local storage
	return kb.loadLocal()
}

// loadLocal loads the knowledge base from local disk
func (kb *KnowledgeBase) loadLocal() error {
	kb.mu.Lock()
	defer kb.mu.Unlock()
	
	if kb.storagePath == "" {
		return nil
	}
	
	data, err := os.ReadFile(kb.storagePath)
	if err != nil {
		return fmt.Errorf("failed to read knowledge base: %w", err)
	}
	
	// Load into temporary struct to preserve defaults
	temp := &KnowledgeBase{}
	if err := json.Unmarshal(data, temp); err != nil {
		return fmt.Errorf("failed to unmarshal knowledge base: %w", err)
	}
	
	kb.mergeLoadedUnsafe(temp)
	log.Printf("KnowledgeBase: Loaded from local file %s", kb.storagePath)
	return nil
}

// mergeLoaded merges loaded data into the knowledge base (thread-safe)
func (kb *KnowledgeBase) mergeLoaded(temp *KnowledgeBase) {
	kb.mu.Lock()
	defer kb.mu.Unlock()
	kb.mergeLoadedUnsafe(temp)
}

// mergeLoadedUnsafe merges loaded data (caller must hold lock)
func (kb *KnowledgeBase) mergeLoadedUnsafe(temp *KnowledgeBase) {
	// Merge loaded data
	if temp.EcosystemState != nil {
		kb.EcosystemState = temp.EcosystemState
	}
	kb.LearnedPatterns = temp.LearnedPatterns
	kb.HistoricalInsights = temp.HistoricalInsights
	kb.AnalysisHistory = temp.AnalysisHistory
	kb.LastLearningRun = temp.LastLearningRun
	kb.TotalLearningCycles = temp.TotalLearningCycles
	kb.DataPointsAnalyzed = temp.DataPointsAnalyzed
	
	// Merge benchmarks
	for k, v := range temp.PerformanceBenchmarks {
		kb.PerformanceBenchmarks[k] = v
	}
	
	// Merge ISP knowledge (preserve learned data)
	for k, v := range temp.ISPKnowledge {
		if existing, ok := kb.ISPKnowledge[k]; ok {
			// Preserve learned metrics
			existing.DeliveryRate = v.DeliveryRate
			existing.OpenRate = v.OpenRate
			existing.ClickRate = v.ClickRate
			existing.BounceRate = v.BounceRate
			existing.ComplaintRate = v.ComplaintRate
			existing.BaselineDeliveryRate = v.BaselineDeliveryRate
			existing.BaselineBounceRate = v.BaselineBounceRate
			existing.BaselineComplaintRate = v.BaselineComplaintRate
			existing.Status = v.Status
			existing.StatusReason = v.StatusReason
			existing.KnownIssues = v.KnownIssues
		} else {
			kb.ISPKnowledge[k] = v
		}
	}
}

// AddLearnedPattern adds a new learned pattern
func (kb *KnowledgeBase) AddLearnedPattern(pattern LearnedPattern) {
	kb.mu.Lock()
	defer kb.mu.Unlock()
	
	// Check if similar pattern exists
	for i, existing := range kb.LearnedPatterns {
		if existing.Type == pattern.Type && existing.TriggerCondition == pattern.TriggerCondition {
			// Update existing pattern
			kb.LearnedPatterns[i].Occurrences++
			kb.LearnedPatterns[i].LastObserved = time.Now()
			if pattern.Confidence > existing.Confidence {
				kb.LearnedPatterns[i].Confidence = pattern.Confidence
			}
			return
		}
	}
	
	// Add new pattern
	pattern.ID = fmt.Sprintf("pattern_%d", time.Now().UnixNano())
	pattern.FirstObserved = time.Now()
	pattern.LastObserved = time.Now()
	pattern.Occurrences = 1
	kb.LearnedPatterns = append(kb.LearnedPatterns, pattern)
}

// AddHistoricalInsight adds a new historical insight
func (kb *KnowledgeBase) AddHistoricalInsight(insight HistoricalInsight) {
	kb.mu.Lock()
	defer kb.mu.Unlock()
	
	insight.ID = fmt.Sprintf("insight_%d", time.Now().UnixNano())
	insight.GeneratedAt = time.Now()
	kb.HistoricalInsights = append(kb.HistoricalInsights, insight)
	
	// Keep only last 100 insights
	if len(kb.HistoricalInsights) > 100 {
		kb.HistoricalInsights = kb.HistoricalInsights[len(kb.HistoricalInsights)-100:]
	}
}

// UpdateBenchmark updates a performance benchmark
func (kb *KnowledgeBase) UpdateBenchmark(key string, benchmark *Benchmark) {
	kb.mu.Lock()
	defer kb.mu.Unlock()
	
	benchmark.CurrentDate = time.Now()
	kb.PerformanceBenchmarks[key] = benchmark
}

// RecordAnalysis records an analysis result
func (kb *KnowledgeBase) RecordAnalysis(result AnalysisResult) {
	kb.mu.Lock()
	defer kb.mu.Unlock()
	
	kb.LastAnalysis = &result
	kb.AnalysisHistory = append(kb.AnalysisHistory, result)
	kb.LastLearningRun = time.Now()
	kb.TotalLearningCycles++
	
	// Keep only last 168 analyses (7 days of hourly analyses)
	if len(kb.AnalysisHistory) > 168 {
		kb.AnalysisHistory = kb.AnalysisHistory[len(kb.AnalysisHistory)-168:]
	}
}

// GetKnowledgeSummary returns a summary of the knowledge base for the AI
func (kb *KnowledgeBase) GetKnowledgeSummary() map[string]interface{} {
	kb.mu.RLock()
	defer kb.mu.RUnlock()
	
	return map[string]interface{}{
		"ecosystem_health":      kb.EcosystemState.OverallHealth,
		"health_score":          kb.EcosystemState.HealthScore,
		"learned_patterns":      len(kb.LearnedPatterns),
		"historical_insights":   len(kb.HistoricalInsights),
		"benchmarks_tracked":    len(kb.PerformanceBenchmarks),
		"isps_monitored":        len(kb.ISPKnowledge),
		"total_learning_cycles": kb.TotalLearningCycles,
		"data_points_analyzed":  kb.DataPointsAnalyzed,
		"last_analysis":         kb.LastAnalysis,
		"active_issues":         kb.EcosystemState.ActiveIssues,
	}
}

// GetBestPracticesForISP returns best practices for a specific ISP
func (kb *KnowledgeBase) GetBestPracticesForISP(isp string) []string {
	kb.mu.RLock()
	defer kb.mu.RUnlock()
	
	if profile, ok := kb.ISPKnowledge[isp]; ok {
		return profile.BestPractices
	}
	
	// Return general best practices
	return kb.BestPractices.WarmupGuidelines
}

// GetComplianceChecklist returns compliance requirements
func (kb *KnowledgeBase) GetComplianceChecklist() map[string][]string {
	kb.mu.RLock()
	defer kb.mu.RUnlock()
	
	return map[string][]string{
		"canspam":     kb.ComplianceRules.CANSPAMRules,
		"gdpr":        kb.ComplianceRules.GDPRRules,
		"ccpa":        kb.ComplianceRules.CCPARules,
		"casl":        kb.ComplianceRules.CASLRules,
		"required":    kb.ComplianceRules.RequiredElements,
		"prohibited":  kb.ComplianceRules.ProhibitedPractices,
	}
}

// GetRecentPatterns returns recently observed patterns
func (kb *KnowledgeBase) GetRecentPatterns(limit int) []LearnedPattern {
	kb.mu.RLock()
	defer kb.mu.RUnlock()
	
	if limit <= 0 || limit > len(kb.LearnedPatterns) {
		limit = len(kb.LearnedPatterns)
	}
	
	// Sort by last observed (most recent first)
	patterns := make([]LearnedPattern, len(kb.LearnedPatterns))
	copy(patterns, kb.LearnedPatterns)
	
	// Return most recent
	if len(patterns) > limit {
		patterns = patterns[len(patterns)-limit:]
	}
	
	return patterns
}

// RunLearningCycle performs a learning cycle with the provided data
func (kb *KnowledgeBase) RunLearningCycle(ctx context.Context, agent *Agent) error {
	kb.mu.Lock()
	defer kb.mu.Unlock()
	
	log.Println("KnowledgeBase: Running learning cycle...")
	startTime := time.Now()
	
	result := AnalysisResult{
		Timestamp:        startTime,
		CriticalIssues:   make([]string, 0),
		WarningIssues:    make([]string, 0),
		ImmediateActions: make([]string, 0),
		ShortTermActions: make([]string, 0),
		LongTermActions:  make([]string, 0),
		TrendChanges:     make([]string, 0),
	}
	
	// Analyze ecosystem health
	if agent.collectors != nil {
		// Get ecosystem data
		eco, allISPs := agent.getEcosystemData()
		
		// Update ecosystem state
		kb.EcosystemState.DailyAverageVolume = eco.TotalVolume
		kb.EcosystemState.BaselineDeliveryRate = eco.DeliveryRate
		kb.EcosystemState.BaselineOpenRate = eco.OpenRate
		kb.EcosystemState.BaselineClickRate = eco.ClickRate
		kb.EcosystemState.BaselineBounceRate = eco.BounceRate
		kb.EcosystemState.BaselineComplaintRate = eco.ComplaintRate
		kb.EcosystemState.LastAssessment = time.Now()
		
		// Calculate health score (0-100)
		healthScore := 100.0
		if eco.ComplaintRate > 0.001 {
			healthScore -= 30
		} else if eco.ComplaintRate > 0.0005 {
			healthScore -= 15
		}
		if eco.BounceRate > 0.05 {
			healthScore -= 25
		} else if eco.BounceRate > 0.03 {
			healthScore -= 10
		}
		if eco.DeliveryRate < 0.90 {
			healthScore -= 30
		} else if eco.DeliveryRate < 0.95 {
			healthScore -= 15
		}
		kb.EcosystemState.HealthScore = healthScore
		
		if healthScore >= 80 {
			kb.EcosystemState.OverallHealth = "healthy"
		} else if healthScore >= 60 {
			kb.EcosystemState.OverallHealth = "warning"
		} else {
			kb.EcosystemState.OverallHealth = "critical"
		}
		
		result.EcosystemHealth = kb.EcosystemState.OverallHealth
		result.HealthScore = healthScore
		result.TotalVolume24h = eco.TotalVolume
		result.AvgDeliveryRate = eco.DeliveryRate
		result.AvgBounceRate = eco.BounceRate
		result.AvgComplaintRate = eco.ComplaintRate
		
		// Update ISP profiles
		for _, isp := range allISPs {
			ispKey := isp.ISP
			if profile, ok := kb.ISPKnowledge[ispKey]; ok {
				profile.DeliveryRate = isp.DeliveryRate
				profile.OpenRate = isp.OpenRate
				profile.ClickRate = isp.ClickRate
				profile.BounceRate = isp.BounceRate
				profile.ComplaintRate = isp.ComplaintRate
				profile.Status = isp.Status
				profile.StatusReason = isp.StatusReason
				
				// Check for issues
				if isp.Status == "critical" {
					result.CriticalIssues = append(result.CriticalIssues, 
						fmt.Sprintf("%s: %s (Provider: %s)", isp.ISP, isp.StatusReason, isp.Provider))
				} else if isp.Status == "warning" {
					result.WarningIssues = append(result.WarningIssues,
						fmt.Sprintf("%s: %s (Provider: %s)", isp.ISP, isp.StatusReason, isp.Provider))
				}
			}
		}
		
		// Check for Everflow revenue data
		if agent.collectors.Everflow != nil {
			totalRevenue := agent.collectors.Everflow.GetTotalRevenue()
			result.TotalRevenue24h = totalRevenue
			kb.EcosystemState.DailyAverageRevenue = totalRevenue
		}
		
		kb.DataPointsAnalyzed += int64(len(allISPs))
	}
	
	// Generate recommendations based on findings
	if len(result.CriticalIssues) > 0 {
		result.ImmediateActions = append(result.ImmediateActions,
			"Address critical ISP issues immediately to prevent further deliverability impact")
	}
	
	if result.AvgComplaintRate > 0.001 {
		result.ImmediateActions = append(result.ImmediateActions,
			"Review recent campaigns for complaint triggers - rate exceeds industry threshold")
		result.ShortTermActions = append(result.ShortTermActions,
			"Audit list hygiene and remove disengaged subscribers")
	}
	
	if result.AvgBounceRate > 0.03 {
		result.ShortTermActions = append(result.ShortTermActions,
			"Clean email list - bounce rate above recommended threshold")
	}
	
	result.AnalysisDuration = time.Since(startTime).String()
	
	// Store results (don't hold lock during save)
	kb.LastAnalysis = &result
	kb.AnalysisHistory = append(kb.AnalysisHistory, result)
	kb.LastLearningRun = time.Now()
	kb.TotalLearningCycles++
	
	// Keep only last 168 analyses
	if len(kb.AnalysisHistory) > 168 {
		kb.AnalysisHistory = kb.AnalysisHistory[len(kb.AnalysisHistory)-168:]
	}
	
	log.Printf("KnowledgeBase: Learning cycle complete - Health: %s (%.0f), %d critical, %d warning issues",
		result.EcosystemHealth, result.HealthScore, len(result.CriticalIssues), len(result.WarningIssues))
	
	return nil
}
