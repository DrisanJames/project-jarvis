package intelligence

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/everflow"
	"github.com/ignite/sparkpost-monitor/internal/ongage"
	"github.com/ignite/sparkpost-monitor/internal/storage"
)

// Service is the AI-powered intelligence and learning engine
type Service struct {
	mu sync.RWMutex

	// Data sources
	ongageCollector   *ongage.Collector
	everflowCollector *everflow.Collector
	storage           *storage.Storage

	// Persistent memory (synced to S3)
	Memory *IntelligenceMemory

	// Learning state
	lastLearningCycle time.Time
	learningInterval  time.Duration
	isLearning        bool
	stopChan          chan struct{}

	// S3 configuration
	s3Bucket string
	s3Prefix string
}

// IntelligenceMemory is the persistent brain of the system
type IntelligenceMemory struct {
	// Core metadata
	Version          string    `json:"version"`
	CreatedAt        time.Time `json:"created_at"`
	LastUpdated      time.Time `json:"last_updated"`
	TotalCycles      int       `json:"total_cycles"`
	DataPointsProcessed int64  `json:"data_points_processed"`

	// Property-Offer Intelligence
	PropertyOfferInsights []PropertyOfferInsight `json:"property_offer_insights"`
	BestPropertyOfferPairs []PropertyOfferPair   `json:"best_property_offer_pairs"`

	// Timing Intelligence
	TimingPatterns        []TimingPattern        `json:"timing_patterns"`
	OptimalSendTimes      map[string][]SendTime  `json:"optimal_send_times"` // property -> times

	// Audience Intelligence
	AudienceInsights      []AudienceInsight      `json:"audience_insights"`
	SegmentProfitability  []SegmentProfit        `json:"segment_profitability"`
	SegmentCostAnalysis   []SegmentCost          `json:"segment_cost_analysis"`

	// ESP-ISP Intelligence
	ESPISPMatrix          []ESPISPPerformance    `json:"esp_isp_matrix"`
	ESPRecommendations    map[string]string      `json:"esp_recommendations"` // ISP -> recommended ESP

	// Marketing Strategy Intelligence
	StrategyInsights      []StrategyInsight      `json:"strategy_insights"`
	CampaignPatterns      []CampaignPattern      `json:"campaign_patterns"`
	
	// Actionable Recommendations
	Recommendations       []Recommendation       `json:"recommendations"`
	RecommendationHistory []Recommendation       `json:"recommendation_history"`

	// Learning Confidence Scores
	ConfidenceScores      map[string]float64     `json:"confidence_scores"`
}

// PropertyOfferInsight represents learned knowledge about property-offer performance
type PropertyOfferInsight struct {
	ID              string    `json:"id"`
	PropertyCode    string    `json:"property_code"`
	PropertyName    string    `json:"property_name"`
	OfferID         string    `json:"offer_id"`
	OfferName       string    `json:"offer_name"`
	
	// Performance metrics
	TotalRevenue    float64   `json:"total_revenue"`
	TotalSent       int64     `json:"total_sent"`
	Conversions     int64     `json:"conversions"`
	ECPM            float64   `json:"ecpm"`
	ConversionRate  float64   `json:"conversion_rate"`
	
	// Trend analysis
	Trend           string    `json:"trend"` // improving, stable, declining
	TrendPercentage float64   `json:"trend_percentage"`
	
	// Learning metadata
	SampleSize      int       `json:"sample_size"`
	Confidence      float64   `json:"confidence"`
	FirstObserved   time.Time `json:"first_observed"`
	LastObserved    time.Time `json:"last_observed"`
	
	// Recommendation
	Recommendation  string    `json:"recommendation"`
}

// PropertyOfferPair represents a high-performing property-offer combination
type PropertyOfferPair struct {
	PropertyCode    string    `json:"property_code"`
	PropertyName    string    `json:"property_name"`
	OfferID         string    `json:"offer_id"`
	OfferName       string    `json:"offer_name"`
	Score           float64   `json:"score"` // Composite performance score
	Revenue         float64   `json:"revenue"`
	ECPM            float64   `json:"ecpm"`
	Rank            int       `json:"rank"`
}

// TimingPattern represents learned send time patterns
type TimingPattern struct {
	ID              string    `json:"id"`
	PropertyCode    string    `json:"property_code"`
	DayOfWeek       int       `json:"day_of_week"` // 0=Sunday
	HourOfDay       int       `json:"hour_of_day"` // 0-23
	
	// Performance at this time
	AvgOpenRate     float64   `json:"avg_open_rate"`
	AvgClickRate    float64   `json:"avg_click_rate"`
	AvgECPM         float64   `json:"avg_ecpm"`
	AvgConvRate     float64   `json:"avg_conv_rate"`
	
	// Sample data
	CampaignCount   int       `json:"campaign_count"`
	TotalSent       int64     `json:"total_sent"`
	Confidence      float64   `json:"confidence"`
	
	// Assessment
	PerformanceTier string    `json:"performance_tier"` // excellent, good, average, poor
	Recommendation  string    `json:"recommendation"`
}

// SendTime represents an optimal send time
type SendTime struct {
	DayOfWeek       string    `json:"day_of_week"`
	HourRange       string    `json:"hour_range"` // e.g., "10:00-14:00"
	ExpectedECPM    float64   `json:"expected_ecpm"`
	Confidence      float64   `json:"confidence"`
}

// AudienceInsight represents learned knowledge about audience segments
type AudienceInsight struct {
	ID              string    `json:"id"`
	SegmentName     string    `json:"segment_name"`
	SegmentType     string    `json:"segment_type"` // openers, clickers, buyers, inactive, etc.
	
	// Performance metrics
	AvgECPM         float64   `json:"avg_ecpm"`
	AvgOpenRate     float64   `json:"avg_open_rate"`
	AvgClickRate    float64   `json:"avg_click_rate"`
	ConversionRate  float64   `json:"conversion_rate"`
	
	// Profitability
	RevenueGenerated float64  `json:"revenue_generated"`
	CostToReach      float64  `json:"cost_to_reach"`
	NetProfit        float64  `json:"net_profit"`
	ROI              float64  `json:"roi"`
	
	// Assessment
	ProfitabilityTier string  `json:"profitability_tier"` // highly_profitable, profitable, break_even, unprofitable
	Confidence       float64  `json:"confidence"`
	Recommendation   string   `json:"recommendation"`
}

// SegmentProfit represents segment profitability analysis
type SegmentProfit struct {
	SegmentName     string    `json:"segment_name"`
	TotalRevenue    float64   `json:"total_revenue"`
	TotalCost       float64   `json:"total_cost"`
	NetProfit       float64   `json:"net_profit"`
	ROI             float64   `json:"roi"`
	CampaignCount   int       `json:"campaign_count"`
	Rank            int       `json:"rank"`
}

// SegmentCost represents segment cost analysis
type SegmentCost struct {
	SegmentName     string    `json:"segment_name"`
	AvgCostPerSend  float64   `json:"avg_cost_per_send"`
	AvgCostPerClick float64   `json:"avg_cost_per_click"`
	AvgCostPerConv  float64   `json:"avg_cost_per_conversion"`
	TotalSpend      float64   `json:"total_spend"`
	Efficiency      string    `json:"efficiency"` // high, medium, low
}

// ESPISPPerformance represents ESP performance for a specific ISP
type ESPISPPerformance struct {
	ESP             string    `json:"esp"`
	ISP             string    `json:"isp"`
	
	// Deliverability
	DeliveryRate    float64   `json:"delivery_rate"`
	BounceRate      float64   `json:"bounce_rate"`
	ComplaintRate   float64   `json:"complaint_rate"`
	
	// Engagement
	OpenRate        float64   `json:"open_rate"`
	ClickRate       float64   `json:"click_rate"`
	
	// Revenue
	AvgECPM         float64   `json:"avg_ecpm"`
	TotalRevenue    float64   `json:"total_revenue"`
	
	// Assessment
	Score           float64   `json:"score"` // Composite score
	IsRecommended   bool      `json:"is_recommended"`
	Confidence      float64   `json:"confidence"`
	SampleSize      int64     `json:"sample_size"`
}

// StrategyInsight represents marketing strategy intelligence
type StrategyInsight struct {
	ID              string    `json:"id"`
	Category        string    `json:"category"` // volume, timing, targeting, creative, offers
	Title           string    `json:"title"`
	Description     string    `json:"description"`
	
	// Supporting data
	Evidence        []string  `json:"evidence"`
	Impact          string    `json:"impact"` // high, medium, low
	ImpactValue     float64   `json:"impact_value"` // Estimated $ impact
	
	// Actionability
	ActionRequired  bool      `json:"action_required"`
	SuggestedAction string    `json:"suggested_action"`
	Priority        int       `json:"priority"` // 1-5, 1 being highest
	
	// Metadata
	GeneratedAt     time.Time `json:"generated_at"`
	Confidence      float64   `json:"confidence"`
	DataSources     []string  `json:"data_sources"`
}

// CampaignPattern represents learned campaign patterns
type CampaignPattern struct {
	ID              string    `json:"id"`
	PatternType     string    `json:"pattern_type"` // success, failure, trending
	Description     string    `json:"description"`
	
	// Pattern characteristics
	Attributes      map[string]interface{} `json:"attributes"`
	SuccessRate     float64   `json:"success_rate"`
	Occurrences     int       `json:"occurrences"`
	
	// Recommendation
	Recommendation  string    `json:"recommendation"`
	Confidence      float64   `json:"confidence"`
}

// Recommendation is an actionable recommendation
type Recommendation struct {
	ID              string    `json:"id"`
	Category        string    `json:"category"` // property_offer, timing, audience, esp, strategy
	Title           string    `json:"title"`
	Description     string    `json:"description"`
	
	// Impact assessment
	ImpactType      string    `json:"impact_type"` // revenue, cost, efficiency, deliverability
	EstimatedImpact float64   `json:"estimated_impact"` // $ amount or percentage
	Confidence      float64   `json:"confidence"`
	
	// Actionability
	Priority        int       `json:"priority"` // 1-5
	Difficulty      string    `json:"difficulty"` // easy, medium, hard
	TimeToImplement string    `json:"time_to_implement"`
	
	// Status
	Status          string    `json:"status"` // new, acknowledged, implemented, dismissed
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// NewService creates a new Intelligence service
func NewService(ongageCollector *ongage.Collector, everflowCollector *everflow.Collector, storage *storage.Storage, s3Bucket, s3Prefix string) *Service {
	svc := &Service{
		ongageCollector:   ongageCollector,
		everflowCollector: everflowCollector,
		storage:           storage,
		learningInterval:  15 * time.Minute, // Learn every 15 minutes
		stopChan:          make(chan struct{}),
		s3Bucket:          s3Bucket,
		s3Prefix:          s3Prefix,
		Memory: &IntelligenceMemory{
			Version:              "1.0",
			CreatedAt:            time.Now(),
			PropertyOfferInsights: make([]PropertyOfferInsight, 0),
			BestPropertyOfferPairs: make([]PropertyOfferPair, 0),
			TimingPatterns:        make([]TimingPattern, 0),
			OptimalSendTimes:      make(map[string][]SendTime),
			AudienceInsights:      make([]AudienceInsight, 0),
			SegmentProfitability:  make([]SegmentProfit, 0),
			SegmentCostAnalysis:   make([]SegmentCost, 0),
			ESPISPMatrix:          make([]ESPISPPerformance, 0),
			ESPRecommendations:    make(map[string]string),
			StrategyInsights:      make([]StrategyInsight, 0),
			CampaignPatterns:      make([]CampaignPattern, 0),
			Recommendations:       make([]Recommendation, 0),
			RecommendationHistory: make([]Recommendation, 0),
			ConfidenceScores:      make(map[string]float64),
		},
	}

	// Try to load existing memory from S3
	if err := svc.LoadMemory(); err != nil {
		log.Printf("Intelligence: Starting with fresh memory (could not load: %v)", err)
	} else {
		log.Printf("Intelligence: Loaded memory - %d cycles, %d property-offer insights, %d recommendations",
			svc.Memory.TotalCycles, len(svc.Memory.PropertyOfferInsights), len(svc.Memory.Recommendations))
	}

	return svc
}

// Start begins the continuous learning loop
func (s *Service) Start() {
	log.Println("Intelligence: Starting continuous learning engine...")
	go s.learningLoop()
}

// Stop halts the learning engine
func (s *Service) Stop() {
	close(s.stopChan)
}

// learningLoop runs the continuous learning process
func (s *Service) learningLoop() {
	// Initial learning cycle after startup delay
	time.Sleep(30 * time.Second)
	s.RunLearningCycle()

	ticker := time.NewTicker(s.learningInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.RunLearningCycle()
		case <-s.stopChan:
			log.Println("Intelligence: Learning engine stopped")
			return
		}
	}
}

// RunLearningCycle executes a complete learning cycle
func (s *Service) RunLearningCycle() {
	s.mu.Lock()
	if s.isLearning {
		s.mu.Unlock()
		return
	}
	s.isLearning = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.isLearning = false
		s.mu.Unlock()
	}()

	startTime := time.Now()
	log.Println("Intelligence: Starting learning cycle...")

	ctx := context.Background()
	dataPoints := int64(0)

	// 1. Learn Property-Offer relationships
	points := s.learnPropertyOfferPatterns()
	dataPoints += points

	// 2. Learn Timing patterns
	points = s.learnTimingPatterns()
	dataPoints += points

	// 3. Learn Audience/Segment patterns
	points = s.learnAudiencePatterns()
	dataPoints += points

	// 4. Learn ESP-ISP optimization
	points = s.learnESPISPPatterns()
	dataPoints += points

	// 5. Generate Strategy Insights
	s.generateStrategyInsights()

	// 6. Generate Recommendations
	s.generateRecommendations()

	// Update metadata
	s.mu.Lock()
	s.Memory.LastUpdated = time.Now()
	s.Memory.TotalCycles++
	s.Memory.DataPointsProcessed += dataPoints
	s.lastLearningCycle = time.Now()
	s.mu.Unlock()

	// Save to S3
	if err := s.SaveMemory(ctx); err != nil {
		log.Printf("Intelligence: Failed to save memory to S3: %v", err)
	}

	duration := time.Since(startTime)
	log.Printf("Intelligence: Learning cycle complete in %v - processed %d data points, generated %d recommendations",
		duration, dataPoints, len(s.Memory.Recommendations))
}

// learnPropertyOfferPatterns analyzes property-offer performance
func (s *Service) learnPropertyOfferPatterns() int64 {
	if s.everflowCollector == nil {
		return 0
	}

	campaigns := s.everflowCollector.GetCampaignRevenue()
	if len(campaigns) == 0 {
		return 0
	}

	// Aggregate by property-offer pairs
	pairMap := make(map[string]*PropertyOfferInsight)
	
	for _, campaign := range campaigns {
		if campaign.PropertyCode == "" || campaign.OfferID == "" {
			continue
		}

		key := fmt.Sprintf("%s_%s", campaign.PropertyCode, campaign.OfferID)
		if _, exists := pairMap[key]; !exists {
			pairMap[key] = &PropertyOfferInsight{
				ID:           key,
				PropertyCode: campaign.PropertyCode,
				PropertyName: campaign.PropertyName,
				OfferID:      campaign.OfferID,
				OfferName:    campaign.OfferName,
				FirstObserved: time.Now(),
			}
		}

		insight := pairMap[key]
		insight.TotalRevenue += campaign.Revenue
		insight.TotalSent += campaign.Sent
		insight.Conversions += campaign.Conversions
		insight.SampleSize++
		insight.LastObserved = time.Now()
	}

	// Calculate derived metrics and generate insights
	insights := make([]PropertyOfferInsight, 0, len(pairMap))
	for _, insight := range pairMap {
		// Calculate ECPM
		if insight.TotalSent > 0 {
			insight.ECPM = (insight.TotalRevenue / float64(insight.TotalSent)) * 1000
		}
		
		// Calculate conversion rate
		if insight.TotalSent > 0 {
			insight.ConversionRate = float64(insight.Conversions) / float64(insight.TotalSent) * 100
		}

		// Calculate confidence based on sample size
		insight.Confidence = calculateConfidence(insight.SampleSize)

		// Generate recommendation
		insight.Recommendation = s.generatePropertyOfferRecommendation(insight)

		insights = append(insights, *insight)
	}

	// Sort by revenue
	sort.Slice(insights, func(i, j int) bool {
		return insights[i].TotalRevenue > insights[j].TotalRevenue
	})

	// Generate best pairs
	bestPairs := make([]PropertyOfferPair, 0)
	for i, insight := range insights {
		if i >= 20 || insight.TotalRevenue < 100 {
			break
		}
		bestPairs = append(bestPairs, PropertyOfferPair{
			PropertyCode: insight.PropertyCode,
			PropertyName: insight.PropertyName,
			OfferID:      insight.OfferID,
			OfferName:    insight.OfferName,
			Score:        insight.ECPM * insight.Confidence,
			Revenue:      insight.TotalRevenue,
			ECPM:         insight.ECPM,
			Rank:         i + 1,
		})
	}

	s.mu.Lock()
	s.Memory.PropertyOfferInsights = insights
	s.Memory.BestPropertyOfferPairs = bestPairs
	s.Memory.ConfidenceScores["property_offer"] = calculateOverallConfidence(insights)
	s.mu.Unlock()

	return int64(len(campaigns))
}

// learnTimingPatterns analyzes send time performance
func (s *Service) learnTimingPatterns() int64 {
	if s.ongageCollector == nil {
		return 0
	}

	campaigns := s.ongageCollector.GetCampaigns()
	if len(campaigns) == 0 {
		return 0
	}

	// Aggregate by day-hour combinations per property
	type timingKey struct {
		property  string
		dayOfWeek int
		hour      int
	}
	timingMap := make(map[timingKey]*TimingPattern)

	for _, campaign := range campaigns {
		if campaign.ScheduleTime.IsZero() {
			continue
		}

		schedTime := campaign.ScheduleTime

		// Extract property from campaign name (format: DATE_PROPERTY_OFFERID_...)
		property := extractPropertyFromCampaign(campaign.Name)
		
		key := timingKey{
			property:  property,
			dayOfWeek: int(schedTime.Weekday()),
			hour:      schedTime.Hour(),
		}

		if _, exists := timingMap[key]; !exists {
			timingMap[key] = &TimingPattern{
				ID:           fmt.Sprintf("%s_%d_%d", property, key.dayOfWeek, key.hour),
				PropertyCode: property,
				DayOfWeek:    key.dayOfWeek,
				HourOfDay:    key.hour,
			}
		}

		pattern := timingMap[key]
		pattern.CampaignCount++
		pattern.TotalSent += campaign.Sent

		// Weighted average for rates
		if campaign.Sent > 0 {
			weight := float64(campaign.Sent)
			pattern.AvgOpenRate = weightedAverage(pattern.AvgOpenRate, campaign.OpenRate, weight, float64(pattern.TotalSent-campaign.Sent))
			pattern.AvgClickRate = weightedAverage(pattern.AvgClickRate, campaign.ClickRate, weight, float64(pattern.TotalSent-campaign.Sent))
		}
	}

	// Process patterns and determine performance tiers
	patterns := make([]TimingPattern, 0, len(timingMap))
	for _, pattern := range timingMap {
		pattern.Confidence = calculateConfidence(pattern.CampaignCount)
		
		// Determine performance tier based on open rate
		if pattern.AvgOpenRate >= 0.25 {
			pattern.PerformanceTier = "excellent"
		} else if pattern.AvgOpenRate >= 0.18 {
			pattern.PerformanceTier = "good"
		} else if pattern.AvgOpenRate >= 0.12 {
			pattern.PerformanceTier = "average"
		} else {
			pattern.PerformanceTier = "poor"
		}

		pattern.Recommendation = s.generateTimingRecommendation(pattern)
		patterns = append(patterns, *pattern)
	}

	// Build optimal send times per property
	optimalTimes := make(map[string][]SendTime)
	propertyPatterns := make(map[string][]TimingPattern)
	
	for _, pattern := range patterns {
		propertyPatterns[pattern.PropertyCode] = append(propertyPatterns[pattern.PropertyCode], pattern)
	}

	for property, propPatterns := range propertyPatterns {
		// Sort by performance
		sort.Slice(propPatterns, func(i, j int) bool {
			return propPatterns[i].AvgOpenRate > propPatterns[j].AvgOpenRate
		})

		// Take top 3 time slots
		times := make([]SendTime, 0, 3)
		for i, pattern := range propPatterns {
			if i >= 3 || pattern.Confidence < 0.3 {
				break
			}
			times = append(times, SendTime{
				DayOfWeek:    dayName(pattern.DayOfWeek),
				HourRange:    fmt.Sprintf("%02d:00-%02d:00", pattern.HourOfDay, (pattern.HourOfDay+2)%24),
				ExpectedECPM: pattern.AvgECPM,
				Confidence:   pattern.Confidence,
			})
		}
		optimalTimes[property] = times
	}

	s.mu.Lock()
	s.Memory.TimingPatterns = patterns
	s.Memory.OptimalSendTimes = optimalTimes
	s.Memory.ConfidenceScores["timing"] = calculateTimingConfidence(patterns)
	s.mu.Unlock()

	return int64(len(campaigns))
}

// learnAudiencePatterns analyzes audience segment performance
func (s *Service) learnAudiencePatterns() int64 {
	if s.ongageCollector == nil {
		return 0
	}

	campaigns := s.ongageCollector.GetCampaigns()
	if len(campaigns) == 0 {
		return 0
	}

	// Aggregate by segment type
	segmentMap := make(map[string]*AudienceInsight)

	for _, campaign := range campaigns {
		segmentType := extractSegmentType(campaign.Name)
		if segmentType == "" {
			segmentType = "unknown"
		}

		if _, exists := segmentMap[segmentType]; !exists {
			segmentMap[segmentType] = &AudienceInsight{
				ID:          segmentType,
				SegmentName: segmentType,
				SegmentType: segmentType,
			}
		}

		insight := segmentMap[segmentType]
		
		// Weighted average for rates
		if campaign.Sent > 0 {
			weight := float64(campaign.Sent)
			totalWeight := insight.RevenueGenerated + weight // Using revenue as proxy for total weight
			
			insight.AvgOpenRate = weightedAverage(insight.AvgOpenRate, campaign.OpenRate, weight, totalWeight-weight)
			insight.AvgClickRate = weightedAverage(insight.AvgClickRate, campaign.ClickRate, weight, totalWeight-weight)
		}

		// Accumulate revenue (would need Everflow integration for actual revenue)
		// For now, estimate based on click rate
		estimatedRevenue := float64(campaign.Clicks) * 0.05 // $0.05 per click estimate
		insight.RevenueGenerated += estimatedRevenue
	}

	// Process insights
	insights := make([]AudienceInsight, 0, len(segmentMap))
	profitability := make([]SegmentProfit, 0, len(segmentMap))

	for _, insight := range segmentMap {
		insight.Confidence = 0.7 // Base confidence for segment analysis
		
		// Determine profitability tier
		if insight.ROI >= 200 {
			insight.ProfitabilityTier = "highly_profitable"
		} else if insight.ROI >= 100 {
			insight.ProfitabilityTier = "profitable"
		} else if insight.ROI >= 0 {
			insight.ProfitabilityTier = "break_even"
		} else {
			insight.ProfitabilityTier = "unprofitable"
		}

		insight.Recommendation = s.generateAudienceRecommendation(insight)
		insights = append(insights, *insight)

		profitability = append(profitability, SegmentProfit{
			SegmentName:   insight.SegmentName,
			TotalRevenue:  insight.RevenueGenerated,
			TotalCost:     insight.CostToReach,
			NetProfit:     insight.NetProfit,
			ROI:           insight.ROI,
			CampaignCount: 0,
		})
	}

	// Sort by ROI
	sort.Slice(profitability, func(i, j int) bool {
		return profitability[i].ROI > profitability[j].ROI
	})
	for i := range profitability {
		profitability[i].Rank = i + 1
	}

	s.mu.Lock()
	s.Memory.AudienceInsights = insights
	s.Memory.SegmentProfitability = profitability
	s.Memory.ConfidenceScores["audience"] = 0.65 // Moderate confidence for segment analysis
	s.mu.Unlock()

	return int64(len(campaigns))
}

// learnESPISPPatterns analyzes ESP-ISP performance relationships
func (s *Service) learnESPISPPatterns() int64 {
	if s.ongageCollector == nil {
		return 0
	}

	espStats := s.ongageCollector.GetESPPerformance()
	if len(espStats) == 0 {
		return 0
	}

	// Build ESP-ISP matrix
	matrix := make([]ESPISPPerformance, 0)
	espISPScores := make(map[string]map[string]float64) // ISP -> ESP -> score

	for _, esp := range espStats {
		perf := ESPISPPerformance{
			ESP:          esp.ESPName,
			ISP:          "all", // Aggregate for now
			DeliveryRate: esp.DeliveryRate,
			BounceRate:   esp.BounceRate,
			OpenRate:     esp.OpenRate,
			ClickRate:    esp.ClickRate,
			SampleSize:   esp.TotalSent,
		}

		// Calculate composite score
		perf.Score = calculateESPScore(perf)
		perf.Confidence = calculateConfidence(int(esp.CampaignCount))
		
		matrix = append(matrix, perf)

		// Track for recommendations
		if _, exists := espISPScores["all"]; !exists {
			espISPScores["all"] = make(map[string]float64)
		}
		espISPScores["all"][esp.ESPName] = perf.Score
	}

	// Generate ESP recommendations per ISP
	recommendations := make(map[string]string)
	for isp, scores := range espISPScores {
		var bestESP string
		var bestScore float64
		for esp, score := range scores {
			if score > bestScore {
				bestScore = score
				bestESP = esp
			}
		}
		if bestESP != "" {
			recommendations[isp] = bestESP
		}
	}

	// Mark recommended ESPs
	for i := range matrix {
		if rec, exists := recommendations[matrix[i].ISP]; exists {
			matrix[i].IsRecommended = (matrix[i].ESP == rec)
		}
	}

	s.mu.Lock()
	s.Memory.ESPISPMatrix = matrix
	s.Memory.ESPRecommendations = recommendations
	s.Memory.ConfidenceScores["esp_isp"] = 0.75
	s.mu.Unlock()

	return int64(len(espStats))
}

// generateStrategyInsights creates high-level strategic insights
func (s *Service) generateStrategyInsights() {
	insights := make([]StrategyInsight, 0)

	s.mu.RLock()
	propertyInsights := s.Memory.PropertyOfferInsights
	timingPatterns := s.Memory.TimingPatterns
	espMatrix := s.Memory.ESPISPMatrix
	s.mu.RUnlock()

	// Volume strategy insights
	if len(propertyInsights) > 0 {
		topPerformers := 0
		for _, insight := range propertyInsights {
			if insight.ECPM > 2.0 {
				topPerformers++
			}
		}
		
		if topPerformers > 0 {
			insights = append(insights, StrategyInsight{
				ID:          fmt.Sprintf("strategy_%d", time.Now().UnixNano()),
				Category:    "offers",
				Title:       "High-Performing Offer Opportunities",
				Description: fmt.Sprintf("Found %d property-offer combinations with eCPM > $2.00. Consider increasing volume on these pairs.", topPerformers),
				Impact:      "high",
				ImpactValue: float64(topPerformers) * 500, // Estimated additional revenue
				ActionRequired: true,
				SuggestedAction: "Review top-performing property-offer pairs and increase send volume where audience permits",
				Priority:    2,
				GeneratedAt: time.Now(),
				Confidence:  0.8,
				DataSources: []string{"everflow", "ongage"},
			})
		}
	}

	// Timing strategy insights
	excellentTimes := 0
	poorTimes := 0
	for _, pattern := range timingPatterns {
		if pattern.PerformanceTier == "excellent" {
			excellentTimes++
		} else if pattern.PerformanceTier == "poor" {
			poorTimes++
		}
	}

	if poorTimes > excellentTimes {
		insights = append(insights, StrategyInsight{
			ID:          fmt.Sprintf("strategy_%d", time.Now().UnixNano()),
			Category:    "timing",
			Title:       "Send Time Optimization Needed",
			Description: fmt.Sprintf("More campaigns sent at poor-performing times (%d) than excellent times (%d). Shifting volume could improve results.", poorTimes, excellentTimes),
			Impact:      "medium",
			ActionRequired: true,
			SuggestedAction: "Review optimal send times per property and adjust scheduling",
			Priority:    3,
			GeneratedAt: time.Now(),
			Confidence:  0.7,
			DataSources: []string{"ongage"},
		})
	}

	// ESP strategy insights
	for _, perf := range espMatrix {
		if perf.BounceRate > 0.03 {
			insights = append(insights, StrategyInsight{
				ID:          fmt.Sprintf("strategy_%d", time.Now().UnixNano()),
				Category:    "deliverability",
				Title:       fmt.Sprintf("High Bounce Rate on %s", perf.ESP),
				Description: fmt.Sprintf("%s has a bounce rate of %.2f%%, above the recommended 3%% threshold.", perf.ESP, perf.BounceRate*100),
				Impact:      "high",
				ActionRequired: true,
				SuggestedAction: "Review list hygiene and consider removing hard bounces",
				Priority:    1,
				GeneratedAt: time.Now(),
				Confidence:  0.9,
				DataSources: []string{"ongage"},
			})
		}
	}

	s.mu.Lock()
	s.Memory.StrategyInsights = insights
	s.mu.Unlock()
}

// generateRecommendations creates actionable recommendations
func (s *Service) generateRecommendations() {
	recommendations := make([]Recommendation, 0)

	s.mu.RLock()
	bestPairs := s.Memory.BestPropertyOfferPairs
	optimalTimes := s.Memory.OptimalSendTimes
	audienceInsights := s.Memory.AudienceInsights
	strategyInsights := s.Memory.StrategyInsights
	s.mu.RUnlock()

	// Property-Offer recommendations
	for i, pair := range bestPairs {
		if i >= 5 {
			break
		}
		recommendations = append(recommendations, Recommendation{
			ID:              fmt.Sprintf("rec_po_%d", time.Now().UnixNano()+int64(i)),
			Category:        "property_offer",
			Title:           fmt.Sprintf("Scale %s + %s", pair.PropertyName, pair.OfferName),
			Description:     fmt.Sprintf("This combination generates $%.2f eCPM with %.0f%% confidence. Consider increasing volume.", pair.ECPM, pair.Score/pair.ECPM*100),
			ImpactType:      "revenue",
			EstimatedImpact: pair.Revenue * 0.2, // 20% potential increase
			Confidence:      pair.Score / (pair.ECPM + 1),
			Priority:        i + 1,
			Difficulty:      "easy",
			TimeToImplement: "immediate",
			Status:          "new",
			CreatedAt:       time.Now(),
			UpdatedAt:       time.Now(),
		})
	}

	// Timing recommendations
	for property, times := range optimalTimes {
		if len(times) == 0 {
			continue
		}
		bestTime := times[0]
		recommendations = append(recommendations, Recommendation{
			ID:              fmt.Sprintf("rec_timing_%s", property),
			Category:        "timing",
			Title:           fmt.Sprintf("Optimize Send Time for %s", property),
			Description:     fmt.Sprintf("Best performance on %s at %s. Shift more volume to this window.", bestTime.DayOfWeek, bestTime.HourRange),
			ImpactType:      "efficiency",
			EstimatedImpact: 15, // 15% improvement estimate
			Confidence:      bestTime.Confidence,
			Priority:        3,
			Difficulty:      "easy",
			TimeToImplement: "1-2 days",
			Status:          "new",
			CreatedAt:       time.Now(),
			UpdatedAt:       time.Now(),
		})
	}

	// Audience recommendations
	for _, insight := range audienceInsights {
		if insight.ProfitabilityTier == "highly_profitable" {
			recommendations = append(recommendations, Recommendation{
				ID:              fmt.Sprintf("rec_aud_%s", insight.SegmentName),
				Category:        "audience",
				Title:           fmt.Sprintf("Expand %s Segment", insight.SegmentName),
				Description:     fmt.Sprintf("This segment shows high profitability. Look for similar audiences to expand reach."),
				ImpactType:      "revenue",
				EstimatedImpact: insight.RevenueGenerated * 0.3,
				Confidence:      insight.Confidence,
				Priority:        2,
				Difficulty:      "medium",
				TimeToImplement: "1 week",
				Status:          "new",
				CreatedAt:       time.Now(),
				UpdatedAt:       time.Now(),
			})
		}
	}

	// Convert strategy insights to recommendations
	for _, insight := range strategyInsights {
		if insight.ActionRequired {
			recommendations = append(recommendations, Recommendation{
				ID:              insight.ID,
				Category:        insight.Category,
				Title:           insight.Title,
				Description:     insight.Description + " " + insight.SuggestedAction,
				ImpactType:      "efficiency",
				EstimatedImpact: insight.ImpactValue,
				Confidence:      insight.Confidence,
				Priority:        insight.Priority,
				Difficulty:      "medium",
				TimeToImplement: "varies",
				Status:          "new",
				CreatedAt:       time.Now(),
				UpdatedAt:       time.Now(),
			})
		}
	}

	// Sort by priority
	sort.Slice(recommendations, func(i, j int) bool {
		return recommendations[i].Priority < recommendations[j].Priority
	})

	s.mu.Lock()
	// Archive old recommendations
	for _, rec := range s.Memory.Recommendations {
		rec.Status = "archived"
		s.Memory.RecommendationHistory = append(s.Memory.RecommendationHistory, rec)
	}
	// Keep only last 100 in history
	if len(s.Memory.RecommendationHistory) > 100 {
		s.Memory.RecommendationHistory = s.Memory.RecommendationHistory[len(s.Memory.RecommendationHistory)-100:]
	}
	s.Memory.Recommendations = recommendations
	s.mu.Unlock()
}

// SaveMemory persists the intelligence memory to S3
func (s *Service) SaveMemory(ctx context.Context) error {
	if s.storage == nil || s.s3Bucket == "" {
		// Fall back to local file storage
		return s.saveMemoryLocal()
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	key := fmt.Sprintf("%s/memory/intelligence_memory.json", s.s3Prefix)
	return s.storage.SaveToS3(ctx, s.s3Bucket, key, s.Memory)
}

// LoadMemory loads the intelligence memory from S3
func (s *Service) LoadMemory() error {
	if s.storage == nil || s.s3Bucket == "" {
		return s.loadMemoryLocal()
	}

	ctx := context.Background()
	key := fmt.Sprintf("%s/memory/intelligence_memory.json", s.s3Prefix)
	
	var memory IntelligenceMemory
	if err := s.storage.GetFromS3(ctx, s.s3Bucket, key, &memory); err != nil {
		return err
	}

	s.mu.Lock()
	s.Memory = &memory
	s.mu.Unlock()

	return nil
}

// saveMemoryLocal saves memory to local file
func (s *Service) saveMemoryLocal() error {
	data, err := json.MarshalIndent(s.Memory, "", "  ")
	if err != nil {
		return err
	}

	return writeFile("data/intelligence/memory.json", data)
}

// loadMemoryLocal loads memory from local file
func (s *Service) loadMemoryLocal() error {
	data, err := readFile("data/intelligence/memory.json")
	if err != nil {
		return err
	}

	var memory IntelligenceMemory
	if err := json.Unmarshal(data, &memory); err != nil {
		return err
	}

	s.mu.Lock()
	s.Memory = &memory
	s.mu.Unlock()

	return nil
}

// GetMemory returns the current intelligence memory
func (s *Service) GetMemory() *IntelligenceMemory {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Memory
}

// GetRecommendations returns current recommendations
func (s *Service) GetRecommendations() []Recommendation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Memory.Recommendations
}

// GetPropertyOfferInsights returns property-offer analysis
func (s *Service) GetPropertyOfferInsights() []PropertyOfferInsight {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Memory.PropertyOfferInsights
}

// GetTimingPatterns returns timing analysis
func (s *Service) GetTimingPatterns() []TimingPattern {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Memory.TimingPatterns
}

// GetESPISPMatrix returns ESP-ISP analysis
func (s *Service) GetESPISPMatrix() []ESPISPPerformance {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Memory.ESPISPMatrix
}

// GetStrategyInsights returns strategic insights
func (s *Service) GetStrategyInsights() []StrategyInsight {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Memory.StrategyInsights
}

// UpdateRecommendationStatus updates a recommendation's status
func (s *Service) UpdateRecommendationStatus(id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, rec := range s.Memory.Recommendations {
		if rec.ID == id {
			s.Memory.Recommendations[i].Status = status
			s.Memory.Recommendations[i].UpdatedAt = time.Now()
			return nil
		}
	}

	return fmt.Errorf("recommendation not found: %s", id)
}

// Helper functions

func calculateConfidence(sampleSize int) float64 {
	if sampleSize >= 100 {
		return 0.95
	} else if sampleSize >= 50 {
		return 0.85
	} else if sampleSize >= 20 {
		return 0.7
	} else if sampleSize >= 10 {
		return 0.5
	} else if sampleSize >= 5 {
		return 0.3
	}
	return 0.1
}

func calculateOverallConfidence(insights []PropertyOfferInsight) float64 {
	if len(insights) == 0 {
		return 0
	}
	total := 0.0
	for _, i := range insights {
		total += i.Confidence
	}
	return total / float64(len(insights))
}

func calculateTimingConfidence(patterns []TimingPattern) float64 {
	if len(patterns) == 0 {
		return 0
	}
	total := 0.0
	for _, p := range patterns {
		total += p.Confidence
	}
	return total / float64(len(patterns))
}

func calculateESPScore(perf ESPISPPerformance) float64 {
	// Weighted score: delivery rate (40%), open rate (30%), click rate (20%), low bounce (10%)
	score := perf.DeliveryRate * 40 +
		perf.OpenRate * 30 +
		perf.ClickRate * 20 +
		(1 - perf.BounceRate) * 10
	return score
}

func weightedAverage(oldAvg, newValue, newWeight, oldWeight float64) float64 {
	if oldWeight+newWeight == 0 {
		return newValue
	}
	return (oldAvg*oldWeight + newValue*newWeight) / (oldWeight + newWeight)
}

func dayName(dayOfWeek int) string {
	days := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	if dayOfWeek >= 0 && dayOfWeek < 7 {
		return days[dayOfWeek]
	}
	return "Unknown"
}

func extractPropertyFromCampaign(name string) string {
	// Campaign name format: DATE_PROPERTY_OFFERID_OFFERNAME_SEGMENT
	// e.g., 01272026_FYF_419_MutualofOmaha_YAH_OPENERS
	parts := splitCampaignName(name)
	if len(parts) >= 2 {
		return parts[1]
	}
	return "unknown"
}

func extractSegmentType(name string) string {
	// Extract segment from campaign name (last part)
	parts := splitCampaignName(name)
	if len(parts) >= 1 {
		lastPart := parts[len(parts)-1]
		// Common segment types
		switch {
		case contains(lastPart, "OPENER"):
			return "openers"
		case contains(lastPart, "CLICKER"):
			return "clickers"
		case contains(lastPart, "BUYER"):
			return "buyers"
		case contains(lastPart, "ABS"):
			return "all_subscribers"
		case contains(lastPart, "YAH"):
			return "yahoo"
		default:
			return lastPart
		}
	}
	return "unknown"
}

func splitCampaignName(name string) []string {
	var parts []string
	var current string
	for _, ch := range name {
		if ch == '_' {
			if current != "" {
				parts = append(parts, current)
				current = ""
			}
		} else {
			current += string(ch)
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || containsMiddle(s, substr)))
}

func containsMiddle(s, substr string) bool {
	for i := 1; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func (s *Service) generatePropertyOfferRecommendation(insight *PropertyOfferInsight) string {
	if insight.ECPM >= 3.0 {
		return "High performer - consider increasing volume"
	} else if insight.ECPM >= 2.0 {
		return "Good performer - maintain current strategy"
	} else if insight.ECPM >= 1.0 {
		return "Average performer - test optimizations"
	}
	return "Low performer - consider reducing volume or testing new creative"
}

func (s *Service) generateTimingRecommendation(pattern *TimingPattern) string {
	switch pattern.PerformanceTier {
	case "excellent":
		return "Peak performance window - prioritize sends at this time"
	case "good":
		return "Strong performance - good backup window"
	case "average":
		return "Moderate performance - use if primary windows unavailable"
	default:
		return "Low performance - avoid sending at this time if possible"
	}
}

func (s *Service) generateAudienceRecommendation(insight *AudienceInsight) string {
	switch insight.ProfitabilityTier {
	case "highly_profitable":
		return "Top segment - expand reach and increase frequency"
	case "profitable":
		return "Good segment - maintain engagement"
	case "break_even":
		return "Neutral segment - optimize targeting or creative"
	default:
		return "Unprofitable segment - review or reduce sends"
	}
}

// File helpers
func writeFile(path string, data []byte) error {
	// Create directory if needed
	dir := path[:len(path)-len("/memory.json")]
	if err := createDir(dir); err != nil {
		return err
	}
	
	f, err := createFile(path)
	if err != nil {
		return err
	}
	defer f.Close()
	
	_, err = f.Write(data)
	return err
}

func readFile(path string) ([]byte, error) {
	return readFileContent(path)
}

func createDir(path string) error {
	return mkdirAll(path)
}

func createFile(path string) (*File, error) {
	return openFile(path)
}

func readFileContent(path string) ([]byte, error) {
	return osReadFile(path)
}

func mkdirAll(path string) error {
	return osMkdirAll(path)
}

// File type and OS function stubs (to be implemented with actual os package)
type File struct {
	path string
}

func (f *File) Close() error { return nil }
func (f *File) Write(data []byte) (int, error) { return len(data), nil }

func openFile(path string) (*File, error) {
	return &File{path: path}, nil
}

func osReadFile(path string) ([]byte, error) {
	return nil, fmt.Errorf("not implemented - use S3")
}

func osMkdirAll(path string) error {
	return nil
}
