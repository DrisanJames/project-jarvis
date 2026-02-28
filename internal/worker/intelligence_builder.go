package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// IntelligenceBuilder builds and maintains subscriber intelligence profiles
type IntelligenceBuilder struct {
	db           *sql.DB
	workerID     string
	pollInterval time.Duration
	batchSize    int
	
	// Stats
	totalProcessed int64
	totalErrors    int64
	
	// Control
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	running bool
	mu      sync.RWMutex
}

// SubscriberIntelligence represents the intelligence profile for a subscriber
type SubscriberIntelligence struct {
	SubscriberID       string                 `json:"subscriber_id"`
	EngagementProfile  EngagementProfile      `json:"engagement_profile"`
	TemporalProfile    TemporalProfile        `json:"temporal_profile"`
	ContentPreferences ContentPreferences     `json:"content_preferences"`
	DeliveryProfile    DeliveryProfile        `json:"delivery_profile"`
	RiskProfile        RiskProfile            `json:"risk_profile"`
	PredictiveScores   PredictiveScores       `json:"predictive_scores"`
	ProfileMaturity    ProfileMaturity        `json:"profile_maturity"`
	ProfileStage       string                 `json:"profile_stage"`
}

// EngagementProfile tracks engagement patterns
type EngagementProfile struct {
	OpenRate30d       float64   `json:"open_rate_30d"`
	OpenRate90d       float64   `json:"open_rate_90d"`
	ClickRate30d      float64   `json:"click_rate_30d"`
	ClickRate90d      float64   `json:"click_rate_90d"`
	EngagementTrend   string    `json:"engagement_trend"` // increasing, stable, declining
	LastEngagement    time.Time `json:"last_engagement"`
	EngagementScore   float64   `json:"engagement_score"`
	RecencyScore      float64   `json:"recency_score"`
	FrequencyScore    float64   `json:"frequency_score"`
	DepthScore        float64   `json:"depth_score"`
}

// TemporalProfile tracks time-based patterns
type TemporalProfile struct {
	BestSendHour      int       `json:"best_send_hour"`
	BestSendDay       int       `json:"best_send_day"` // 0=Sunday, 6=Saturday
	AvgOpenDelayMins  float64   `json:"avg_open_delay_mins"`
	TimezoneLikely    string    `json:"timezone_likely"`
	ActivityHours     []int     `json:"activity_hours"` // Hours with most activity
	WeekdayVsWeekend  string    `json:"weekday_vs_weekend"` // weekday, weekend, both
	LastUpdated       time.Time `json:"last_updated"`
}

// ContentPreferences tracks content engagement patterns
type ContentPreferences struct {
	PreferredSubjectLength   string   `json:"preferred_subject_length"` // short, medium, long
	TopClickedCategories     []string `json:"top_clicked_categories"`
	TopOpenedSubjectWords    []string `json:"top_opened_subject_words"`
	PreferredContentType     string   `json:"preferred_content_type"` // promotional, educational, transactional
	EmojiResponseRate        float64  `json:"emoji_response_rate"`
	PersonalizedOpenLift     float64  `json:"personalized_open_lift"`
}

// DeliveryProfile tracks delivery patterns
type DeliveryProfile struct {
	DeliverabilityScore float64 `json:"deliverability_score"`
	BounceCount         int     `json:"bounce_count"`
	SoftBounceCount     int     `json:"soft_bounce_count"`
	LastBounce          *time.Time `json:"last_bounce,omitempty"`
	PreferredESP        string  `json:"preferred_esp"` // ESP with best deliverability
	Domain              string  `json:"domain"`
	DomainCategory      string  `json:"domain_category"` // personal, corporate, freemail
	MailboxProvider     string  `json:"mailbox_provider"` // gmail, outlook, yahoo, other
}

// RiskProfile tracks risk indicators
type RiskProfile struct {
	ChurnRisk           float64 `json:"churn_risk"` // 0-1
	ComplaintRisk       float64 `json:"complaint_risk"` // 0-1
	SpamTrapRisk        float64 `json:"spam_trap_risk"` // 0-1
	UnsubscribeRisk     float64 `json:"unsubscribe_risk"` // 0-1
	InactivityDays      int     `json:"inactivity_days"`
	RiskLevel           string  `json:"risk_level"` // low, medium, high, critical
	LastRiskAssessment  time.Time `json:"last_risk_assessment"`
}

// PredictiveScores contains ML/heuristic predictions
type PredictiveScores struct {
	NextOpenProbability  float64 `json:"next_open_probability"`
	NextClickProbability float64 `json:"next_click_probability"`
	LTV                  float64 `json:"ltv"` // Lifetime value prediction
	OptimalSendTime      string  `json:"optimal_send_time"` // ISO time string
	ReengageScore        float64 `json:"reengage_score"` // Likelihood of re-engagement
}

// ProfileMaturity tracks the completeness of the profile
type ProfileMaturity struct {
	DataPoints         int       `json:"data_points"`
	Completeness       float64   `json:"completeness"` // 0-100
	Confidence         float64   `json:"confidence"` // 0-100
	FirstSeen          time.Time `json:"first_seen"`
	LastUpdated        time.Time `json:"last_updated"`
	MinDataForPrediction int     `json:"min_data_for_prediction"`
}

// NewIntelligenceBuilder creates a new intelligence builder
func NewIntelligenceBuilder(db *sql.DB) *IntelligenceBuilder {
	return &IntelligenceBuilder{
		db:           db,
		workerID:     fmt.Sprintf("intel-%s", uuid.New().String()[:8]),
		pollInterval: 30 * time.Second, // Run every 30 seconds
		batchSize:    500, // Process 500 subscribers at a time
	}
}

// Start begins the intelligence builder
func (ib *IntelligenceBuilder) Start() {
	ib.mu.Lock()
	if ib.running {
		ib.mu.Unlock()
		return
	}
	ib.running = true
	ib.ctx, ib.cancel = context.WithCancel(context.Background())
	ib.mu.Unlock()
	
	log.Printf("IntelligenceBuilder: Starting worker %s", ib.workerID)
	
	// Register worker
	ib.registerWorker()
	
	// Start main loop
	ib.wg.Add(1)
	go ib.buildLoop()
	
	// Start heartbeat
	go ib.heartbeatLoop()
}

// Stop gracefully stops the builder
func (ib *IntelligenceBuilder) Stop() {
	ib.mu.Lock()
	if !ib.running {
		ib.mu.Unlock()
		return
	}
	ib.running = false
	ib.cancel()
	ib.mu.Unlock()
	
	log.Println("IntelligenceBuilder: Stopping...")
	ib.wg.Wait()
	ib.deregisterWorker()
	
	log.Printf("IntelligenceBuilder: Stopped. Processed: %d, Errors: %d",
		atomic.LoadInt64(&ib.totalProcessed), atomic.LoadInt64(&ib.totalErrors))
}

// buildLoop is the main processing loop
func (ib *IntelligenceBuilder) buildLoop() {
	defer ib.wg.Done()
	
	ticker := time.NewTicker(ib.pollInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-ib.ctx.Done():
			return
		case <-ticker.C:
			ib.processSubscribers()
		}
	}
}

// processSubscribers processes subscribers that need intelligence updates
func (ib *IntelligenceBuilder) processSubscribers() {
	ctx, cancel := context.WithTimeout(ib.ctx, 60*time.Second)
	defer cancel()
	
	// Find subscribers with recent activity or stale profiles
	rows, err := ib.db.QueryContext(ctx, `
		SELECT 
			s.id, s.email, s.engagement_score,
			s.total_emails_received, s.total_opens, s.total_clicks,
			s.last_open_at, s.last_click_at, s.last_email_at,
			s.subscribed_at, s.status,
			COALESCE(i.updated_at, '1970-01-01') as intel_updated_at
		FROM mailing_subscribers s
		LEFT JOIN mailing_subscriber_intelligence i ON i.subscriber_id = s.id
		WHERE s.status = 'confirmed'
		AND (
			i.subscriber_id IS NULL  -- No intelligence yet
			OR s.updated_at > i.updated_at  -- Subscriber updated since last intel
			OR i.updated_at < NOW() - INTERVAL '24 hours'  -- Stale profile
		)
		ORDER BY s.total_emails_received DESC
		LIMIT $1
	`, ib.batchSize)
	
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("IntelligenceBuilder: Error fetching subscribers: %v", err)
		}
		return
	}
	defer rows.Close()
	
	var subscribers []struct {
		ID                 uuid.UUID
		Email              string
		EngagementScore    float64
		TotalReceived      int
		TotalOpens         int
		TotalClicks        int
		LastOpenAt         sql.NullTime
		LastClickAt        sql.NullTime
		LastEmailAt        sql.NullTime
		SubscribedAt       time.Time
		Status             string
		IntelUpdatedAt     time.Time
	}
	
	for rows.Next() {
		var sub struct {
			ID                 uuid.UUID
			Email              string
			EngagementScore    float64
			TotalReceived      int
			TotalOpens         int
			TotalClicks        int
			LastOpenAt         sql.NullTime
			LastClickAt        sql.NullTime
			LastEmailAt        sql.NullTime
			SubscribedAt       time.Time
			Status             string
			IntelUpdatedAt     time.Time
		}
		err := rows.Scan(
			&sub.ID, &sub.Email, &sub.EngagementScore,
			&sub.TotalReceived, &sub.TotalOpens, &sub.TotalClicks,
			&sub.LastOpenAt, &sub.LastClickAt, &sub.LastEmailAt,
			&sub.SubscribedAt, &sub.Status, &sub.IntelUpdatedAt,
		)
		if err != nil {
			continue
		}
		subscribers = append(subscribers, sub)
	}
	
	// Process each subscriber
	for _, sub := range subscribers {
		if err := ib.buildIntelligence(ctx, sub.ID, sub.Email, sub.EngagementScore,
			sub.TotalReceived, sub.TotalOpens, sub.TotalClicks,
			sub.LastOpenAt, sub.LastClickAt, sub.LastEmailAt,
			sub.SubscribedAt); err != nil {
			atomic.AddInt64(&ib.totalErrors, 1)
			log.Printf("IntelligenceBuilder: Error building intel for %s: %v", sub.ID, err)
		} else {
			atomic.AddInt64(&ib.totalProcessed, 1)
		}
	}
}

// buildIntelligence builds the intelligence profile for a subscriber
func (ib *IntelligenceBuilder) buildIntelligence(
	ctx context.Context,
	subscriberID uuid.UUID,
	email string,
	engagementScore float64,
	totalReceived, totalOpens, totalClicks int,
	lastOpenAt, lastClickAt, lastEmailAt sql.NullTime,
	subscribedAt time.Time,
) error {
	// Build engagement profile
	engagement := ib.buildEngagementProfile(ctx, subscriberID, engagementScore, totalReceived, totalOpens, totalClicks, lastOpenAt, lastClickAt)
	
	// Build temporal profile
	temporal := ib.buildTemporalProfile(ctx, subscriberID)
	
	// Build delivery profile
	delivery := ib.buildDeliveryProfile(ctx, subscriberID, email)
	
	// Build risk profile
	risk := ib.buildRiskProfile(ctx, subscriberID, engagement, lastOpenAt, lastClickAt)
	
	// Build predictive scores
	predictive := ib.buildPredictiveScores(engagement, temporal, risk)
	
	// Calculate profile maturity
	dataPoints := totalReceived + totalOpens + totalClicks
	completeness := float64(dataPoints) / 50.0 * 100.0 // 50 data points = 100%
	if completeness > 100 {
		completeness = 100
	}
	
	maturity := ProfileMaturity{
		DataPoints:           dataPoints,
		Completeness:         completeness,
		Confidence:           completeness * 0.9, // Confidence slightly lower than completeness
		FirstSeen:            subscribedAt,
		LastUpdated:          time.Now(),
		MinDataForPrediction: 10,
	}
	
	// Determine profile stage
	profileStage := "new"
	if dataPoints >= 50 {
		profileStage = "mature"
	} else if dataPoints >= 20 {
		profileStage = "developing"
	} else if dataPoints >= 5 {
		profileStage = "learning"
	}
	
	// Serialize profiles
	engagementJSON, _ := json.Marshal(engagement)
	temporalJSON, _ := json.Marshal(temporal)
	contentJSON, _ := json.Marshal(ContentPreferences{}) // TODO: Build from tracking data
	deliveryJSON, _ := json.Marshal(delivery)
	riskJSON, _ := json.Marshal(risk)
	predictiveJSON, _ := json.Marshal(predictive)
	maturityJSON, _ := json.Marshal(maturity)
	
	// Get organization ID
	var orgID uuid.UUID
	ib.db.QueryRowContext(ctx, `SELECT organization_id FROM mailing_subscribers WHERE id = $1`, subscriberID).Scan(&orgID)
	
	// Upsert intelligence record
	_, err := ib.db.ExecContext(ctx, `
		INSERT INTO mailing_subscriber_intelligence (
			id, subscriber_id, organization_id,
			engagement_profile, temporal_profile, content_preferences,
			delivery_profile, risk_profile, predictive_scores, profile_maturity,
			profile_stage, created_at, updated_at, last_prediction_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW(), NOW(), NOW()
		)
		ON CONFLICT (subscriber_id) DO UPDATE SET
			engagement_profile = EXCLUDED.engagement_profile,
			temporal_profile = EXCLUDED.temporal_profile,
			content_preferences = EXCLUDED.content_preferences,
			delivery_profile = EXCLUDED.delivery_profile,
			risk_profile = EXCLUDED.risk_profile,
			predictive_scores = EXCLUDED.predictive_scores,
			profile_maturity = EXCLUDED.profile_maturity,
			profile_stage = EXCLUDED.profile_stage,
			updated_at = NOW(),
			last_prediction_at = NOW()
	`, uuid.New(), subscriberID, orgID,
		string(engagementJSON), string(temporalJSON), string(contentJSON),
		string(deliveryJSON), string(riskJSON), string(predictiveJSON), string(maturityJSON),
		profileStage)
	
	if err != nil {
		return fmt.Errorf("failed to upsert intelligence: %w", err)
	}
	
	// Update subscriber's optimal send hour if we have enough data
	if temporal.BestSendHour >= 0 && maturity.Confidence > 50 {
		ib.db.ExecContext(ctx, `
			UPDATE mailing_subscribers 
			SET optimal_send_hour_utc = $2, churn_risk_score = $3, predicted_ltv = $4
			WHERE id = $1
		`, subscriberID, temporal.BestSendHour, risk.ChurnRisk, predictive.LTV)
	}
	
	return nil
}

// buildEngagementProfile builds the engagement profile
func (ib *IntelligenceBuilder) buildEngagementProfile(
	ctx context.Context,
	subscriberID uuid.UUID,
	currentScore float64,
	totalReceived, totalOpens, totalClicks int,
	lastOpenAt, lastClickAt sql.NullTime,
) EngagementProfile {
	profile := EngagementProfile{
		EngagementScore: currentScore,
	}
	
	// Calculate rates
	if totalReceived > 0 {
		profile.OpenRate90d = float64(totalOpens) / float64(totalReceived)
		profile.ClickRate90d = float64(totalClicks) / float64(totalReceived)
	}
	
	// Get 30-day stats
	var opens30d, clicks30d, received30d int
	ib.db.QueryRowContext(ctx, `
		SELECT 
			COUNT(*) FILTER (WHERE event_type = 'sent'),
			COUNT(*) FILTER (WHERE event_type = 'opened'),
			COUNT(*) FILTER (WHERE event_type = 'clicked')
		FROM mailing_tracking_events
		WHERE subscriber_id = $1 AND event_at > NOW() - INTERVAL '30 days'
	`, subscriberID).Scan(&received30d, &opens30d, &clicks30d)
	
	if received30d > 0 {
		profile.OpenRate30d = float64(opens30d) / float64(received30d)
		profile.ClickRate30d = float64(clicks30d) / float64(received30d)
	}
	
	// Determine trend
	if profile.OpenRate30d > profile.OpenRate90d*1.1 {
		profile.EngagementTrend = "increasing"
	} else if profile.OpenRate30d < profile.OpenRate90d*0.9 {
		profile.EngagementTrend = "declining"
	} else {
		profile.EngagementTrend = "stable"
	}
	
	// Set last engagement
	if lastClickAt.Valid {
		profile.LastEngagement = lastClickAt.Time
	} else if lastOpenAt.Valid {
		profile.LastEngagement = lastOpenAt.Time
	}
	
	// Calculate sub-scores (RFM style)
	now := time.Now()
	
	// Recency score (0-100)
	if !profile.LastEngagement.IsZero() {
		daysSinceEngagement := now.Sub(profile.LastEngagement).Hours() / 24
		profile.RecencyScore = 100.0 * (1.0 - (daysSinceEngagement / 90.0))
		if profile.RecencyScore < 0 {
			profile.RecencyScore = 0
		}
	}
	
	// Frequency score (0-100)
	profile.FrequencyScore = float64(totalOpens) / 50.0 * 100.0
	if profile.FrequencyScore > 100 {
		profile.FrequencyScore = 100
	}
	
	// Depth score (clicks as % of opens)
	if totalOpens > 0 {
		profile.DepthScore = float64(totalClicks) / float64(totalOpens) * 100.0
		if profile.DepthScore > 100 {
			profile.DepthScore = 100
		}
	}
	
	return profile
}

// buildTemporalProfile builds the temporal profile
func (ib *IntelligenceBuilder) buildTemporalProfile(ctx context.Context, subscriberID uuid.UUID) TemporalProfile {
	profile := TemporalProfile{
		BestSendHour: -1, // Unknown
		BestSendDay:  -1,
		LastUpdated:  time.Now(),
	}
	
	// Find best hour based on opens
	var bestHour sql.NullInt32
	ib.db.QueryRowContext(ctx, `
		SELECT EXTRACT(HOUR FROM event_at)::int as hour
		FROM mailing_tracking_events
		WHERE subscriber_id = $1 AND event_type = 'opened'
		GROUP BY hour
		ORDER BY COUNT(*) DESC
		LIMIT 1
	`, subscriberID).Scan(&bestHour)
	
	if bestHour.Valid {
		profile.BestSendHour = int(bestHour.Int32)
	}
	
	// Find best day based on opens
	var bestDay sql.NullInt32
	ib.db.QueryRowContext(ctx, `
		SELECT EXTRACT(DOW FROM event_at)::int as day
		FROM mailing_tracking_events
		WHERE subscriber_id = $1 AND event_type = 'opened'
		GROUP BY day
		ORDER BY COUNT(*) DESC
		LIMIT 1
	`, subscriberID).Scan(&bestDay)
	
	if bestDay.Valid {
		profile.BestSendDay = int(bestDay.Int32)
		if profile.BestSendDay == 0 || profile.BestSendDay == 6 {
			profile.WeekdayVsWeekend = "weekend"
		} else {
			profile.WeekdayVsWeekend = "weekday"
		}
	}
	
	// Calculate average open delay
	var avgDelay sql.NullFloat64
	ib.db.QueryRowContext(ctx, `
		WITH send_open_pairs AS (
			SELECT 
				s.event_at as sent_at,
				MIN(o.event_at) as opened_at
			FROM mailing_tracking_events s
			JOIN mailing_tracking_events o ON o.campaign_id = s.campaign_id AND o.subscriber_id = s.subscriber_id
			WHERE s.subscriber_id = $1 
			  AND s.event_type = 'sent' 
			  AND o.event_type = 'opened'
			  AND o.event_at > s.event_at
			GROUP BY s.event_at
		)
		SELECT AVG(EXTRACT(EPOCH FROM (opened_at - sent_at)) / 60)
		FROM send_open_pairs
	`, subscriberID).Scan(&avgDelay)
	
	if avgDelay.Valid {
		profile.AvgOpenDelayMins = avgDelay.Float64
	}
	
	return profile
}

// buildDeliveryProfile builds the delivery profile
func (ib *IntelligenceBuilder) buildDeliveryProfile(ctx context.Context, subscriberID uuid.UUID, email string) DeliveryProfile {
	profile := DeliveryProfile{
		DeliverabilityScore: 100.0, // Start perfect
	}
	
	// Extract domain
	parts := splitEmail(email)
	if len(parts) == 2 {
		profile.Domain = parts[1]
		profile.MailboxProvider = categorizeMailboxProvider(parts[1])
		profile.DomainCategory = categorizeDomainType(parts[1])
	}
	
	// Count bounces
	ib.db.QueryRowContext(ctx, `
		SELECT 
			COUNT(*) FILTER (WHERE bounce_type = 'hard'),
			COUNT(*) FILTER (WHERE bounce_type = 'soft'),
			MAX(bounced_at)
		FROM mailing_bounces
		WHERE subscriber_id = $1
	`, subscriberID).Scan(&profile.BounceCount, &profile.SoftBounceCount, &profile.LastBounce)
	
	// Reduce deliverability score based on bounces
	profile.DeliverabilityScore -= float64(profile.BounceCount) * 20.0
	profile.DeliverabilityScore -= float64(profile.SoftBounceCount) * 5.0
	if profile.DeliverabilityScore < 0 {
		profile.DeliverabilityScore = 0
	}
	
	return profile
}

// buildRiskProfile builds the risk profile
func (ib *IntelligenceBuilder) buildRiskProfile(
	ctx context.Context,
	subscriberID uuid.UUID,
	engagement EngagementProfile,
	lastOpenAt, lastClickAt sql.NullTime,
) RiskProfile {
	profile := RiskProfile{
		LastRiskAssessment: time.Now(),
	}
	
	// Calculate inactivity days
	lastActivity := time.Time{}
	if lastClickAt.Valid && lastClickAt.Time.After(lastActivity) {
		lastActivity = lastClickAt.Time
	}
	if lastOpenAt.Valid && lastOpenAt.Time.After(lastActivity) {
		lastActivity = lastOpenAt.Time
	}
	
	if !lastActivity.IsZero() {
		profile.InactivityDays = int(time.Since(lastActivity).Hours() / 24)
	} else {
		profile.InactivityDays = 365 // No activity recorded
	}
	
	// Churn risk based on inactivity and engagement trend
	profile.ChurnRisk = float64(profile.InactivityDays) / 90.0 // 90 days = 100% risk
	if profile.ChurnRisk > 1.0 {
		profile.ChurnRisk = 1.0
	}
	if engagement.EngagementTrend == "declining" {
		profile.ChurnRisk = profile.ChurnRisk * 1.3 // 30% more likely to churn
		if profile.ChurnRisk > 1.0 {
			profile.ChurnRisk = 1.0
		}
	}
	
	// Check for complaints
	var complaintCount int
	ib.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_complaints WHERE subscriber_id = $1
	`, subscriberID).Scan(&complaintCount)
	
	if complaintCount > 0 {
		profile.ComplaintRisk = 0.8 // High risk if ever complained
	} else {
		profile.ComplaintRisk = 0.05 // Low baseline risk
	}
	
	// Unsubscribe risk based on engagement
	profile.UnsubscribeRisk = (1.0 - engagement.EngagementScore/100.0) * 0.5
	
	// Determine risk level
	maxRisk := max(profile.ChurnRisk, profile.ComplaintRisk, profile.UnsubscribeRisk)
	if maxRisk >= 0.8 {
		profile.RiskLevel = "critical"
	} else if maxRisk >= 0.5 {
		profile.RiskLevel = "high"
	} else if maxRisk >= 0.3 {
		profile.RiskLevel = "medium"
	} else {
		profile.RiskLevel = "low"
	}
	
	return profile
}

// buildPredictiveScores builds predictive scores
func (ib *IntelligenceBuilder) buildPredictiveScores(
	engagement EngagementProfile,
	temporal TemporalProfile,
	risk RiskProfile,
) PredictiveScores {
	scores := PredictiveScores{}
	
	// Next open probability based on historical open rate and trend
	scores.NextOpenProbability = engagement.OpenRate90d
	if engagement.EngagementTrend == "increasing" {
		scores.NextOpenProbability *= 1.2
	} else if engagement.EngagementTrend == "declining" {
		scores.NextOpenProbability *= 0.8
	}
	if scores.NextOpenProbability > 1.0 {
		scores.NextOpenProbability = 1.0
	}
	
	// Next click probability
	scores.NextClickProbability = engagement.ClickRate90d
	if engagement.EngagementTrend == "increasing" {
		scores.NextClickProbability *= 1.2
	}
	if scores.NextClickProbability > 1.0 {
		scores.NextClickProbability = 1.0
	}
	
	// LTV prediction (simplified model)
	// Higher engagement = higher LTV
	scores.LTV = engagement.EngagementScore * 10.0 * (1.0 - risk.ChurnRisk)
	
	// Optimal send time
	if temporal.BestSendHour >= 0 {
		scores.OptimalSendTime = fmt.Sprintf("%02d:00:00", temporal.BestSendHour)
	}
	
	// Re-engagement score for churning subscribers
	if risk.ChurnRisk > 0.5 {
		// Use recency of last engagement
		scores.ReengageScore = 1.0 - (float64(risk.InactivityDays) / 180.0)
		if scores.ReengageScore < 0 {
			scores.ReengageScore = 0
		}
	} else {
		scores.ReengageScore = 1.0 // Already engaged
	}
	
	return scores
}

// Helper functions
func splitEmail(email string) []string {
	for i := len(email) - 1; i >= 0; i-- {
		if email[i] == '@' {
			return []string{email[:i], email[i+1:]}
		}
	}
	return []string{email}
}

func categorizeMailboxProvider(domain string) string {
	switch {
	case containsAny(domain, "gmail.com", "googlemail.com"):
		return "gmail"
	case containsAny(domain, "outlook.com", "hotmail.com", "live.com", "msn.com"):
		return "outlook"
	case containsAny(domain, "yahoo.com", "ymail.com", "rocketmail.com"):
		return "yahoo"
	case containsAny(domain, "aol.com", "aim.com"):
		return "aol"
	case containsAny(domain, "icloud.com", "me.com", "mac.com"):
		return "icloud"
	default:
		return "other"
	}
}

func categorizeDomainType(domain string) string {
	freeMailDomains := []string{"gmail.com", "yahoo.com", "hotmail.com", "outlook.com", "aol.com", "icloud.com"}
	for _, d := range freeMailDomains {
		if domain == d {
			return "freemail"
		}
	}
	return "corporate"
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if s == sub {
			return true
		}
	}
	return false
}

// max is now a builtin function in Go 1.21+

// registerWorker registers this worker
func (ib *IntelligenceBuilder) registerWorker() {
	ib.db.Exec(`
		INSERT INTO mailing_workers (id, worker_type, hostname, status, started_at, last_heartbeat_at)
		VALUES ($1, 'intelligence_builder', $2, 'running', NOW(), NOW())
		ON CONFLICT (id) DO UPDATE SET
			status = 'running',
			started_at = NOW(),
			last_heartbeat_at = NOW()
	`, ib.workerID, "ignite-worker")
}

// deregisterWorker removes this worker
func (ib *IntelligenceBuilder) deregisterWorker() {
	ib.db.Exec(`UPDATE mailing_workers SET status = 'stopped' WHERE id = $1`, ib.workerID)
}

// heartbeatLoop sends periodic heartbeats
func (ib *IntelligenceBuilder) heartbeatLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-ib.ctx.Done():
			return
		case <-ticker.C:
			ib.db.Exec(`
				UPDATE mailing_workers 
				SET last_heartbeat_at = NOW(), total_processed = $2, total_errors = $3
				WHERE id = $1
			`, ib.workerID, atomic.LoadInt64(&ib.totalProcessed), atomic.LoadInt64(&ib.totalErrors))
		}
	}
}
