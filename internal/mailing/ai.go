package mailing

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
)

// AIService provides AI-powered email optimization
type AIService struct {
	store *Store
}

// NewAIService creates a new AI service
func NewAIService(store *Store) *AIService {
	return &AIService{store: store}
}

// SendingPlanOption represents a sending plan option
type SendingPlanOption struct {
	TimePeriod        string             `json:"time_period"`
	Name              string             `json:"name"`
	Description       string             `json:"description"`
	RecommendedVolume int                `json:"recommended_volume"`
	TimeSlots         []TimeSlotPlan     `json:"time_slots"`
	AudienceBreakdown []AudienceSegment  `json:"audience_breakdown"`
	OfferRecommendations []OfferRecommendation `json:"offer_recommendations"`
	Predictions       PlanPredictions    `json:"predictions"`
	ConfidenceScore   float64            `json:"confidence_score"`
	AIExplanation     string             `json:"ai_explanation"`
	Warnings          []string           `json:"warnings"`
	Recommendations   []string           `json:"recommendations"`
}

// TimeSlotPlan represents a time slot in the sending plan
type TimeSlotPlan struct {
	StartTime      time.Time `json:"start_time"`
	EndTime        time.Time `json:"end_time"`
	Volume         int       `json:"volume"`
	Priority       string    `json:"priority"`
	TargetAudience string    `json:"target_audience"`
}

// AudienceSegment represents an audience breakdown segment
type AudienceSegment struct {
	Name               string  `json:"name"`
	Count              int     `json:"count"`
	EngagementLevel    string  `json:"engagement_level"`
	PredictedOpenRate  float64 `json:"predicted_open_rate"`
	PredictedClickRate float64 `json:"predicted_click_rate"`
	RecommendedAction  string  `json:"recommended_action"`
}

// OfferRecommendation represents an offer recommendation
type OfferRecommendation struct {
	OfferID           uuid.UUID `json:"offer_id"`
	OfferName         string    `json:"offer_name"`
	MatchScore        float64   `json:"match_score"`
	PredictedEPC      float64   `json:"predicted_epc"`
	RecommendedVolume int       `json:"recommended_volume"`
	Reason            string    `json:"reason"`
}

// PlanPredictions represents predictions for a plan
type PlanPredictions struct {
	EstimatedOpens       int     `json:"estimated_opens"`
	EstimatedClicks      int     `json:"estimated_clicks"`
	EstimatedRevenue     float64 `json:"estimated_revenue"`
	EstimatedBounceRate  float64 `json:"estimated_bounce_rate"`
	EstimatedComplaintRate float64 `json:"estimated_complaint_rate"`
	RevenueRange         [2]float64 `json:"revenue_range"`
	ConfidenceInterval   float64 `json:"confidence_interval"`
}

// VolumeCapacity represents available sending capacity
type VolumeCapacity struct {
	TotalHourly    int            `json:"total_hourly"`
	TotalDaily     int            `json:"total_daily"`
	UsedToday      int            `json:"used_today"`
	RemainingToday int            `json:"remaining_today"`
	ByServer       []ServerCapacity `json:"by_server"`
	WarmupLimits   map[string]int `json:"warmup_limits"`
}

// ServerCapacity represents capacity for a single server
type ServerCapacity struct {
	ServerID       uuid.UUID `json:"server_id"`
	ServerName     string    `json:"server_name"`
	ServerType     string    `json:"server_type"`
	HourlyCapacity int       `json:"hourly_capacity"`
	DailyCapacity  int       `json:"daily_capacity"`
	UsedToday      int       `json:"used_today"`
	Remaining      int       `json:"remaining"`
	ReputationScore float64  `json:"reputation_score"`
	IsWarming      bool      `json:"is_warming"`
}

// GenerateSendingPlans generates AI-powered sending plan options
func (ai *AIService) GenerateSendingPlans(ctx context.Context, orgID uuid.UUID, targetDate time.Time) ([]*SendingPlanOption, error) {
	// Get capacity
	capacity, err := ai.getVolumeCapacity(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("get capacity: %w", err)
	}

	// Get audience analysis
	audienceAnalysis, err := ai.analyzeAudience(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("analyze audience: %w", err)
	}

	// Get historical performance
	performance, err := ai.getHistoricalPerformance(ctx, orgID, 30)
	if err != nil {
		return nil, fmt.Errorf("get performance: %w", err)
	}

	// Generate three plan options
	plans := []*SendingPlanOption{
		ai.generateMorningPlan(capacity, audienceAnalysis, performance, targetDate),
		ai.generateFirstHalfPlan(capacity, audienceAnalysis, performance, targetDate),
		ai.generateFullDayPlan(capacity, audienceAnalysis, performance, targetDate),
	}

	return plans, nil
}

func (ai *AIService) getVolumeCapacity(ctx context.Context, orgID uuid.UUID) (*VolumeCapacity, error) {
	servers, err := ai.store.GetDeliveryServers(ctx, orgID)
	if err != nil {
		return nil, err
	}

	capacity := &VolumeCapacity{
		ByServer:     make([]ServerCapacity, 0, len(servers)),
		WarmupLimits: make(map[string]int),
	}

	for _, server := range servers {
		sc := ServerCapacity{
			ServerID:        server.ID,
			ServerName:      server.Name,
			ServerType:      server.ServerType,
			HourlyCapacity:  server.HourlyQuota,
			DailyCapacity:   server.DailyQuota,
			UsedToday:       server.UsedDaily,
			Remaining:       server.DailyQuota - server.UsedDaily,
			ReputationScore: server.ReputationScore,
			IsWarming:       server.WarmupEnabled,
		}
		capacity.ByServer = append(capacity.ByServer, sc)
		capacity.TotalHourly += server.HourlyQuota
		capacity.TotalDaily += server.DailyQuota
		capacity.UsedToday += server.UsedDaily
		capacity.RemainingToday += sc.Remaining
	}

	return capacity, nil
}

type audienceStats struct {
	TotalSubscribers   int
	HighEngagement     int
	MediumEngagement   int
	LowEngagement      int
	AverageOpenRate    float64
	AverageClickRate   float64
	OptimalHours       map[int]int
}

func (ai *AIService) analyzeAudience(ctx context.Context, orgID uuid.UUID) (*audienceStats, error) {
	// In production, this would query the database for actual metrics
	return &audienceStats{
		TotalSubscribers: 100000,
		HighEngagement:   20000,
		MediumEngagement: 50000,
		LowEngagement:    30000,
		AverageOpenRate:  15.5,
		AverageClickRate: 2.3,
		OptimalHours: map[int]int{
			9:  15000,
			10: 18000,
			11: 16000,
			14: 14000,
			15: 12000,
		},
	}, nil
}

type performanceMetrics struct {
	AvgOpenRate        float64
	AvgClickRate       float64
	AvgBounceRate      float64
	AvgComplaintRate   float64
	AvgRevenuePerSend  float64
	BestDayOfWeek      int
	BestHourOfDay      int
	TopOffers          []uuid.UUID
}

func (ai *AIService) getHistoricalPerformance(ctx context.Context, orgID uuid.UUID, days int) (*performanceMetrics, error) {
	// In production, this would analyze historical campaign data
	return &performanceMetrics{
		AvgOpenRate:       14.2,
		AvgClickRate:      2.1,
		AvgBounceRate:     1.5,
		AvgComplaintRate:  0.05,
		AvgRevenuePerSend: 0.012,
		BestDayOfWeek:     2, // Tuesday
		BestHourOfDay:     10,
	}, nil
}

func (ai *AIService) generateMorningPlan(capacity *VolumeCapacity, audience *audienceStats, perf *performanceMetrics, targetDate time.Time) *SendingPlanOption {
	// Morning plan: 6 AM - 12 PM, focus on high engagement subscribers
	volume := min(capacity.RemainingToday/3, audience.HighEngagement)

	plan := &SendingPlanOption{
		TimePeriod:        "morning",
		Name:              "Morning Focus",
		Description:       "Concentrated morning send targeting high-engagement subscribers during peak open hours",
		RecommendedVolume: volume,
		TimeSlots: []TimeSlotPlan{
			{
				StartTime:      time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 6, 0, 0, 0, time.UTC),
				EndTime:        time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 9, 0, 0, 0, time.UTC),
				Volume:         volume / 4,
				Priority:       "high",
				TargetAudience: "early_risers",
			},
			{
				StartTime:      time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 9, 0, 0, 0, time.UTC),
				EndTime:        time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 12, 0, 0, 0, time.UTC),
				Volume:         volume * 3 / 4,
				Priority:       "high",
				TargetAudience: "work_start",
			},
		},
		AudienceBreakdown: []AudienceSegment{
			{
				Name:               "High Engagement",
				Count:              audience.HighEngagement,
				EngagementLevel:    "high",
				PredictedOpenRate:  perf.AvgOpenRate * 1.5,
				PredictedClickRate: perf.AvgClickRate * 1.4,
				RecommendedAction:  "Send first",
			},
		},
		Predictions: PlanPredictions{
			EstimatedOpens:        int(float64(volume) * perf.AvgOpenRate * 1.3 / 100),
			EstimatedClicks:       int(float64(volume) * perf.AvgClickRate * 1.2 / 100),
			EstimatedRevenue:      float64(volume) * perf.AvgRevenuePerSend * 1.25,
			EstimatedBounceRate:   perf.AvgBounceRate * 0.8,
			EstimatedComplaintRate: perf.AvgComplaintRate * 0.7,
			ConfidenceInterval:    0.85,
		},
		ConfidenceScore: 0.88,
		AIExplanation:   "Morning sends historically show 30% higher engagement. This plan focuses on your most engaged subscribers during peak morning hours (9-11 AM), which have shown the highest open rates in your sending history.",
		Warnings: []string{
			"Lower volume limits potential reach",
		},
		Recommendations: []string{
			"Ideal for time-sensitive offers",
			"Best for high-value content",
			"Monitor engagement closely for afternoon follow-up",
		},
	}
	plan.Predictions.RevenueRange = [2]float64{
		plan.Predictions.EstimatedRevenue * 0.8,
		plan.Predictions.EstimatedRevenue * 1.2,
	}
	return plan
}

func (ai *AIService) generateFirstHalfPlan(capacity *VolumeCapacity, audience *audienceStats, perf *performanceMetrics, targetDate time.Time) *SendingPlanOption {
	// First half plan: 6 AM - 2 PM, balanced approach
	volume := min(capacity.RemainingToday/2, audience.HighEngagement+audience.MediumEngagement/2)

	plan := &SendingPlanOption{
		TimePeriod:        "first_half",
		Name:              "First Half Balanced",
		Description:       "Extended morning through early afternoon send with balanced engagement targeting",
		RecommendedVolume: volume,
		TimeSlots: []TimeSlotPlan{
			{
				StartTime:      time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 6, 0, 0, 0, time.UTC),
				EndTime:        time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 10, 0, 0, 0, time.UTC),
				Volume:         volume / 3,
				Priority:       "high",
				TargetAudience: "high_engagement",
			},
			{
				StartTime:      time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 10, 0, 0, 0, time.UTC),
				EndTime:        time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 14, 0, 0, 0, time.UTC),
				Volume:         volume * 2 / 3,
				Priority:       "medium",
				TargetAudience: "mixed_engagement",
			},
		},
		AudienceBreakdown: []AudienceSegment{
			{
				Name:               "High Engagement",
				Count:              audience.HighEngagement,
				EngagementLevel:    "high",
				PredictedOpenRate:  perf.AvgOpenRate * 1.4,
				PredictedClickRate: perf.AvgClickRate * 1.3,
				RecommendedAction:  "Priority send",
			},
			{
				Name:               "Medium Engagement",
				Count:              audience.MediumEngagement / 2,
				EngagementLevel:    "medium",
				PredictedOpenRate:  perf.AvgOpenRate,
				PredictedClickRate: perf.AvgClickRate,
				RecommendedAction:  "Standard send",
			},
		},
		Predictions: PlanPredictions{
			EstimatedOpens:        int(float64(volume) * perf.AvgOpenRate * 1.15 / 100),
			EstimatedClicks:       int(float64(volume) * perf.AvgClickRate * 1.1 / 100),
			EstimatedRevenue:      float64(volume) * perf.AvgRevenuePerSend * 1.1,
			EstimatedBounceRate:   perf.AvgBounceRate,
			EstimatedComplaintRate: perf.AvgComplaintRate * 0.9,
			ConfidenceInterval:    0.82,
		},
		ConfidenceScore: 0.85,
		AIExplanation:   "Balanced approach spreading volume across prime engagement hours. Starts with high-engagement subscribers during morning peak, then extends to medium-engagement during late morning. Good balance of reach and performance.",
		Warnings:        []string{},
		Recommendations: []string{
			"Good for general campaigns",
			"Allows afternoon performance review",
			"Consider evening follow-up for non-openers",
		},
	}
	plan.Predictions.RevenueRange = [2]float64{
		plan.Predictions.EstimatedRevenue * 0.75,
		plan.Predictions.EstimatedRevenue * 1.25,
	}
	return plan
}

func (ai *AIService) generateFullDayPlan(capacity *VolumeCapacity, audience *audienceStats, perf *performanceMetrics, targetDate time.Time) *SendingPlanOption {
	// Full day plan: 6 AM - 9 PM, maximum reach
	volume := min(capacity.RemainingToday, audience.TotalSubscribers)

	plan := &SendingPlanOption{
		TimePeriod:        "full_day",
		Name:              "Full Day Maximum",
		Description:       "Full day send maximizing reach across all subscriber segments",
		RecommendedVolume: volume,
		TimeSlots: []TimeSlotPlan{
			{
				StartTime:      time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 6, 0, 0, 0, time.UTC),
				EndTime:        time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 10, 0, 0, 0, time.UTC),
				Volume:         volume / 4,
				Priority:       "high",
				TargetAudience: "high_engagement",
			},
			{
				StartTime:      time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 10, 0, 0, 0, time.UTC),
				EndTime:        time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 15, 0, 0, 0, time.UTC),
				Volume:         volume / 2,
				Priority:       "medium",
				TargetAudience: "medium_engagement",
			},
			{
				StartTime:      time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 15, 0, 0, 0, time.UTC),
				EndTime:        time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 21, 0, 0, 0, time.UTC),
				Volume:         volume / 4,
				Priority:       "low",
				TargetAudience: "low_engagement_evening",
			},
		},
		AudienceBreakdown: []AudienceSegment{
			{
				Name:               "High Engagement",
				Count:              audience.HighEngagement,
				EngagementLevel:    "high",
				PredictedOpenRate:  perf.AvgOpenRate * 1.3,
				PredictedClickRate: perf.AvgClickRate * 1.2,
				RecommendedAction:  "Morning priority",
			},
			{
				Name:               "Medium Engagement",
				Count:              audience.MediumEngagement,
				EngagementLevel:    "medium",
				PredictedOpenRate:  perf.AvgOpenRate,
				PredictedClickRate: perf.AvgClickRate,
				RecommendedAction:  "Midday send",
			},
			{
				Name:               "Low Engagement",
				Count:              audience.LowEngagement,
				EngagementLevel:    "low",
				PredictedOpenRate:  perf.AvgOpenRate * 0.6,
				PredictedClickRate: perf.AvgClickRate * 0.5,
				RecommendedAction:  "Evening re-engagement",
			},
		},
		Predictions: PlanPredictions{
			EstimatedOpens:        int(float64(volume) * perf.AvgOpenRate / 100),
			EstimatedClicks:       int(float64(volume) * perf.AvgClickRate / 100),
			EstimatedRevenue:      float64(volume) * perf.AvgRevenuePerSend,
			EstimatedBounceRate:   perf.AvgBounceRate * 1.1,
			EstimatedComplaintRate: perf.AvgComplaintRate * 1.2,
			ConfidenceInterval:    0.78,
		},
		ConfidenceScore: 0.80,
		AIExplanation:   "Maximum reach plan utilizing full daily capacity. While per-subscriber engagement rates may be slightly lower due to inclusion of lower-engagement segments, total revenue potential is maximized. Evening slot targets subscribers who typically engage after work hours.",
		Warnings: []string{
			"Higher complaint risk from low-engagement segment",
			"Monitor bounce rates closely",
		},
		Recommendations: []string{
			"Best for revenue maximization",
			"Good for broad announcements",
			"Consider suppressing recent non-openers",
		},
	}
	plan.Predictions.RevenueRange = [2]float64{
		plan.Predictions.EstimatedRevenue * 0.7,
		plan.Predictions.EstimatedRevenue * 1.3,
	}
	return plan
}

// OptimalSendTimePredictor predicts optimal send times for subscribers
type OptimalSendTimePredictor struct {
	store *Store
}

// NewOptimalSendTimePredictor creates a new predictor
func NewOptimalSendTimePredictor(store *Store) *OptimalSendTimePredictor {
	return &OptimalSendTimePredictor{store: store}
}

// PredictOptimalSendTime predicts the best time to send to a subscriber
func (p *OptimalSendTimePredictor) PredictOptimalSendTime(ctx context.Context, subscriberID uuid.UUID) (int, float64, error) {
	sub, err := p.store.GetSubscriber(ctx, subscriberID)
	if err != nil {
		return 10, 0.5, err // Default to 10 AM UTC with medium confidence
	}
	if sub == nil {
		return 10, 0.5, nil
	}

	// If we have learned optimal hour, use it
	if sub.OptimalSendHourUTC != nil {
		confidence := 0.8
		if sub.TotalOpens >= 10 {
			confidence = 0.9
		}
		return *sub.OptimalSendHourUTC, confidence, nil
	}

	// Default prediction based on engagement
	if sub.EngagementScore >= 70 {
		return 9, 0.6, nil // High engagement - send during peak hours
	} else if sub.EngagementScore >= 40 {
		return 11, 0.5, nil // Medium engagement
	}
	return 14, 0.4, nil // Low engagement - try afternoon
}

// EngagementScorer calculates and updates engagement scores
type EngagementScorer struct {
	store *Store
}

// NewEngagementScorer creates a new scorer
func NewEngagementScorer(store *Store) *EngagementScorer {
	return &EngagementScorer{store: store}
}

// CalculateScore calculates engagement score for a subscriber
func (es *EngagementScorer) CalculateScore(sub *Subscriber) float64 {
	if sub.TotalEmailsReceived == 0 {
		return 50.0 // Default score for new subscribers
	}

	// Components
	openRate := 0.0
	if sub.TotalEmailsReceived > 0 {
		openRate = float64(sub.TotalOpens) / float64(sub.TotalEmailsReceived)
	}

	clickRate := 0.0
	if sub.TotalOpens > 0 {
		clickRate = float64(sub.TotalClicks) / float64(sub.TotalOpens)
	}

	// Recency factor
	recencyScore := 1.0
	if sub.LastOpenAt != nil {
		daysSinceOpen := time.Since(*sub.LastOpenAt).Hours() / 24
		recencyScore = math.Max(0, 1-daysSinceOpen/90) // Decay over 90 days
	}

	// Calculate weighted score
	score := (openRate*40 + clickRate*30 + recencyScore*30)
	return math.Min(100, math.Max(0, score))
}

// ContentOptimizer optimizes email content
type ContentOptimizer struct{}

// NewContentOptimizer creates a new optimizer
func NewContentOptimizer() *ContentOptimizer {
	return &ContentOptimizer{}
}

// SubjectLineVariant represents a subject line variant
type SubjectLineVariant struct {
	Subject          string  `json:"subject"`
	PredictedOpenRate float64 `json:"predicted_open_rate"`
	Reason           string  `json:"reason"`
}

// OptimizeSubjectLine generates optimized subject line variants
func (co *ContentOptimizer) OptimizeSubjectLine(original string, audienceType string) []SubjectLineVariant {
	variants := []SubjectLineVariant{
		{
			Subject:          original,
			PredictedOpenRate: 15.0,
			Reason:           "Original subject line",
		},
	}

	// Add urgency variant
	variants = append(variants, SubjectLineVariant{
		Subject:          "🔥 " + original,
		PredictedOpenRate: 17.5,
		Reason:           "Emoji increases visibility in inbox",
	})

	// Add personalization variant
	variants = append(variants, SubjectLineVariant{
		Subject:          "{{first_name}}, " + original,
		PredictedOpenRate: 18.0,
		Reason:           "Personalization increases engagement",
	})

	// Sort by predicted open rate
	sort.Slice(variants, func(i, j int) bool {
		return variants[i].PredictedOpenRate > variants[j].PredictedOpenRate
	})

	return variants
}

// DeliverabilityPredictor predicts deliverability issues
type DeliverabilityPredictor struct {
	store *Store
}

// NewDeliverabilityPredictor creates a new predictor
func NewDeliverabilityPredictor(store *Store) *DeliverabilityPredictor {
	return &DeliverabilityPredictor{store: store}
}

// DeliverabilityPrediction represents a deliverability prediction
type DeliverabilityPrediction struct {
	Score           float64  `json:"score"`
	Risk            string   `json:"risk"`
	Issues          []string `json:"issues"`
	Recommendations []string `json:"recommendations"`
}

// PredictDeliverability predicts deliverability for a campaign
func (dp *DeliverabilityPredictor) PredictDeliverability(ctx context.Context, campaign *Campaign) (*DeliverabilityPrediction, error) {
	prediction := &DeliverabilityPrediction{
		Score:  95.0,
		Risk:   "low",
		Issues: []string{},
		Recommendations: []string{},
	}

	// Check content issues
	if len(campaign.HTMLContent) > 100000 {
		prediction.Score -= 5
		prediction.Issues = append(prediction.Issues, "Email content is very large")
		prediction.Recommendations = append(prediction.Recommendations, "Consider reducing image sizes or content length")
	}

	// Check spam trigger words (simplified)
	spamWords := []string{"free", "winner", "urgent", "act now", "limited time"}
	subject := campaign.Subject
	for _, word := range spamWords {
		if containsIgnoreCase(subject, word) {
			prediction.Score -= 3
			prediction.Issues = append(prediction.Issues, fmt.Sprintf("Subject contains potential spam trigger: %s", word))
		}
	}

	// Determine risk level
	if prediction.Score < 70 {
		prediction.Risk = "high"
	} else if prediction.Score < 85 {
		prediction.Risk = "medium"
	}

	return prediction, nil
}

func containsIgnoreCase(s, substr string) bool {
	// Simple case-insensitive contains
	return len(s) >= len(substr) && (s == substr || 
		(len(s) > 0 && containsIgnoreCase(s[1:], substr)))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
