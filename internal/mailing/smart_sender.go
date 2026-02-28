package mailing

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// ============================================================================
// SMART SENDER - AI-POWERED CAMPAIGN OPTIMIZATION
// ============================================================================

// SmartSender provides AI-powered optimization for email campaigns
type SmartSender struct {
	db    *sql.DB
	redis *redis.Client
	store *Store

	// Configuration
	metricsInterval     time.Duration // How often to collect metrics
	optimizationInterval time.Duration // How often to run optimization
	
	// Cache
	settingsCache   map[string]*CampaignAISettings
	settingsCacheMu sync.RWMutex
	
	// Claude/Anthropic client would be injected here for advanced AI
	// anthropic *anthropic.Client
}

// NewSmartSender creates a new smart sender
func NewSmartSender(db *sql.DB, redisClient *redis.Client, store *Store) *SmartSender {
	return &SmartSender{
		db:                   db,
		redis:                redisClient,
		store:                store,
		metricsInterval:      1 * time.Minute,
		optimizationInterval: 5 * time.Minute,
		settingsCache:        make(map[string]*CampaignAISettings),
	}
}

// ============================================================================
// AI SETTINGS MANAGEMENT
// ============================================================================

// GetAISettings retrieves AI settings for a campaign
func (s *SmartSender) GetAISettings(ctx context.Context, campaignID uuid.UUID) (*CampaignAISettings, error) {
	// Check cache first
	cacheKey := campaignID.String()
	s.settingsCacheMu.RLock()
	if cached, ok := s.settingsCache[cacheKey]; ok {
		s.settingsCacheMu.RUnlock()
		return cached, nil
	}
	s.settingsCacheMu.RUnlock()

	var settings CampaignAISettings
	err := s.db.QueryRowContext(ctx, `
		SELECT id, campaign_id, enable_smart_sending, enable_throttle_optimization,
		       enable_send_time_optimization, enable_ab_auto_winner, target_metric,
		       min_throttle_rate, max_throttle_rate, current_throttle_rate,
		       learning_period_minutes, ab_confidence_threshold, ab_min_sample_size,
		       pause_on_high_complaints, complaint_threshold, bounce_threshold,
		       created_at, updated_at
		FROM mailing_campaign_ai_settings
		WHERE campaign_id = $1
	`, campaignID).Scan(
		&settings.ID, &settings.CampaignID, &settings.EnableSmartSending,
		&settings.EnableThrottleOptimization, &settings.EnableSendTimeOptimization,
		&settings.EnableABAutoWinner, &settings.TargetMetric,
		&settings.MinThrottleRate, &settings.MaxThrottleRate, &settings.CurrentThrottleRate,
		&settings.LearningPeriodMinutes, &settings.ABConfidenceThreshold, &settings.ABMinSampleSize,
		&settings.PauseOnHighComplaints, &settings.ComplaintThreshold, &settings.BounceThreshold,
		&settings.CreatedAt, &settings.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		// Return default settings
		return s.getDefaultSettings(campaignID), nil
	}
	if err != nil {
		return nil, fmt.Errorf("get AI settings: %w", err)
	}

	// Cache the result
	s.settingsCacheMu.Lock()
	s.settingsCache[cacheKey] = &settings
	s.settingsCacheMu.Unlock()

	return &settings, nil
}

// SaveAISettings saves AI settings for a campaign
func (s *SmartSender) SaveAISettings(ctx context.Context, req *CreateAISettingsRequest) (*CampaignAISettings, error) {
	// Get existing or default settings
	existing, err := s.GetAISettings(ctx, req.CampaignID)
	if err != nil {
		existing = s.getDefaultSettings(req.CampaignID)
	}

	// Apply updates
	if req.EnableSmartSending != nil {
		existing.EnableSmartSending = *req.EnableSmartSending
	}
	if req.EnableThrottleOptimization != nil {
		existing.EnableThrottleOptimization = *req.EnableThrottleOptimization
	}
	if req.EnableSendTimeOptimization != nil {
		existing.EnableSendTimeOptimization = *req.EnableSendTimeOptimization
	}
	if req.EnableABAutoWinner != nil {
		existing.EnableABAutoWinner = *req.EnableABAutoWinner
	}
	if req.TargetMetric != "" {
		existing.TargetMetric = req.TargetMetric
	}
	if req.MinThrottleRate != nil {
		existing.MinThrottleRate = *req.MinThrottleRate
	}
	if req.MaxThrottleRate != nil {
		existing.MaxThrottleRate = *req.MaxThrottleRate
	}
	if req.LearningPeriodMinutes != nil {
		existing.LearningPeriodMinutes = *req.LearningPeriodMinutes
	}
	if req.ABConfidenceThreshold != nil {
		existing.ABConfidenceThreshold = *req.ABConfidenceThreshold
	}
	if req.ABMinSampleSize != nil {
		existing.ABMinSampleSize = *req.ABMinSampleSize
	}
	if req.PauseOnHighComplaints != nil {
		existing.PauseOnHighComplaints = *req.PauseOnHighComplaints
	}
	if req.ComplaintThreshold != nil {
		existing.ComplaintThreshold = *req.ComplaintThreshold
	}
	if req.BounceThreshold != nil {
		existing.BounceThreshold = *req.BounceThreshold
	}

	// Upsert to database
	var id uuid.UUID
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO mailing_campaign_ai_settings (
			campaign_id, enable_smart_sending, enable_throttle_optimization,
			enable_send_time_optimization, enable_ab_auto_winner, target_metric,
			min_throttle_rate, max_throttle_rate, current_throttle_rate,
			learning_period_minutes, ab_confidence_threshold, ab_min_sample_size,
			pause_on_high_complaints, complaint_threshold, bounce_threshold
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (campaign_id) DO UPDATE SET
			enable_smart_sending = $2,
			enable_throttle_optimization = $3,
			enable_send_time_optimization = $4,
			enable_ab_auto_winner = $5,
			target_metric = $6,
			min_throttle_rate = $7,
			max_throttle_rate = $8,
			learning_period_minutes = $10,
			ab_confidence_threshold = $11,
			ab_min_sample_size = $12,
			pause_on_high_complaints = $13,
			complaint_threshold = $14,
			bounce_threshold = $15,
			updated_at = NOW()
		RETURNING id
	`, req.CampaignID, existing.EnableSmartSending, existing.EnableThrottleOptimization,
		existing.EnableSendTimeOptimization, existing.EnableABAutoWinner, existing.TargetMetric,
		existing.MinThrottleRate, existing.MaxThrottleRate, existing.CurrentThrottleRate,
		existing.LearningPeriodMinutes, existing.ABConfidenceThreshold, existing.ABMinSampleSize,
		existing.PauseOnHighComplaints, existing.ComplaintThreshold, existing.BounceThreshold,
	).Scan(&id)

	if err != nil {
		return nil, fmt.Errorf("save AI settings: %w", err)
	}

	existing.ID = id

	// Invalidate cache
	s.settingsCacheMu.Lock()
	delete(s.settingsCache, req.CampaignID.String())
	s.settingsCacheMu.Unlock()

	return existing, nil
}

func (s *SmartSender) getDefaultSettings(campaignID uuid.UUID) *CampaignAISettings {
	return &CampaignAISettings{
		CampaignID:                 campaignID,
		EnableSmartSending:         true,
		EnableThrottleOptimization: true,
		EnableSendTimeOptimization: true,
		EnableABAutoWinner:         true,
		TargetMetric:               TargetMetricOpens,
		MinThrottleRate:            1000,
		MaxThrottleRate:            50000,
		CurrentThrottleRate:        10000,
		LearningPeriodMinutes:      60,
		ABConfidenceThreshold:      0.95,
		ABMinSampleSize:            1000,
		PauseOnHighComplaints:      true,
		ComplaintThreshold:         0.001, // 0.1%
		BounceThreshold:            0.05,  // 5%
	}
}

// ============================================================================
// THROTTLE OPTIMIZATION
// ============================================================================

// OptimizeThrottle analyzes recent metrics and adjusts throttle rate
func (s *SmartSender) OptimizeThrottle(ctx context.Context, campaignID uuid.UUID) (*ThrottleOptimizationResult, error) {
	settings, err := s.GetAISettings(ctx, campaignID)
	if err != nil {
		return nil, err
	}

	if !settings.EnableThrottleOptimization {
		return nil, fmt.Errorf("throttle optimization disabled for campaign")
	}

	// Get recent metrics (last 15 minutes)
	metrics, err := s.getRecentMetrics(ctx, campaignID, 15*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("get recent metrics: %w", err)
	}

	if metrics == nil || len(metrics) == 0 {
		return nil, fmt.Errorf("no metrics available for optimization")
	}

	// Analyze metrics and get recommendation
	recommendation := s.analyzeMetricsForThrottle(metrics, settings)

	// Apply the recommendation
	result := &ThrottleOptimizationResult{
		CampaignID:   campaignID,
		PreviousRate: settings.CurrentThrottleRate,
		Reason:       recommendation.Reason,
		Confidence:   recommendation.Confidence,
	}

	if recommendation.Action == "maintain" {
		result.NewRate = settings.CurrentThrottleRate
		result.ChangePercentage = 0
		return result, nil
	}

	if recommendation.Action == "pause" {
		// Pause the campaign
		if err := s.pauseCampaign(ctx, campaignID, recommendation.Reason); err != nil {
			log.Printf("[SmartSender] Failed to pause campaign: %v", err)
		}
		result.NewRate = 0
		result.ChangePercentage = -100
		return result, nil
	}

	// Apply throttle change
	newRate := recommendation.NewRate
	newRate = max(newRate, settings.MinThrottleRate)
	newRate = min(newRate, settings.MaxThrottleRate)

	result.NewRate = newRate
	if settings.CurrentThrottleRate > 0 {
		result.ChangePercentage = float64(newRate-settings.CurrentThrottleRate) / float64(settings.CurrentThrottleRate) * 100
	}

	// Update the throttle rate
	if err := s.updateThrottleRate(ctx, campaignID, newRate); err != nil {
		return nil, fmt.Errorf("update throttle rate: %w", err)
	}

	// Log the decision
	metricsJSON, _ := json.Marshal(metrics)
	s.logAIDecision(ctx, campaignID, DecisionType(recommendation.Action+"_throttle"), recommendation.Reason,
		fmt.Sprintf("%d", settings.CurrentThrottleRate), fmt.Sprintf("%d", newRate),
		metricsJSON, recommendation.Confidence)

	return result, nil
}

func (s *SmartSender) analyzeMetricsForThrottle(metrics []*RealtimeMetrics, settings *CampaignAISettings) *ThrottleRecommendation {
	if len(metrics) == 0 {
		return &ThrottleRecommendation{
			Action:     "maintain",
			NewRate:    settings.CurrentThrottleRate,
			Reason:     "No metrics available",
			Confidence: 0.5,
			RiskLevel:  "low",
		}
	}

	// Calculate aggregate metrics
	var totalSent, totalBounces, totalComplaints, totalOpens int
	for _, m := range metrics {
		totalSent += m.SentCount
		totalBounces += m.BounceCount
		totalComplaints += m.ComplaintCount
		totalOpens += m.OpenCount
	}

	if totalSent == 0 {
		return &ThrottleRecommendation{
			Action:     "maintain",
			NewRate:    settings.CurrentThrottleRate,
			Reason:     "No sends in analysis window",
			Confidence: 0.5,
			RiskLevel:  "low",
		}
	}

	bounceRate := float64(totalBounces) / float64(totalSent)
	complaintRate := float64(totalComplaints) / float64(totalSent)
	openRate := float64(totalOpens) / float64(totalSent)

	// Decision rules (would be enhanced with Claude for complex decisions)

	// CRITICAL: High complaint rate - pause immediately
	if complaintRate > settings.ComplaintThreshold*2 {
		return &ThrottleRecommendation{
			Action:         "pause",
			NewRate:        0,
			Reason:         fmt.Sprintf("Critical complaint rate: %.4f%% exceeds threshold", complaintRate*100),
			Confidence:     0.95,
			RiskLevel:      "high",
			ExpectedImpact: "Campaign paused to protect sender reputation",
		}
	}

	// HIGH: Complaint rate above threshold - reduce significantly
	if complaintRate > settings.ComplaintThreshold {
		newRate := int(float64(settings.CurrentThrottleRate) * 0.5)
		return &ThrottleRecommendation{
			Action:         "decrease",
			NewRate:        newRate,
			Reason:         fmt.Sprintf("High complaint rate: %.4f%% - reducing throttle by 50%%", complaintRate*100),
			Confidence:     0.9,
			RiskLevel:      "high",
			ExpectedImpact: "Reduced sending rate to lower complaint volume",
		}
	}

	// HIGH: Bounce rate above threshold - reduce
	if bounceRate > settings.BounceThreshold {
		newRate := int(float64(settings.CurrentThrottleRate) * 0.7)
		return &ThrottleRecommendation{
			Action:         "decrease",
			NewRate:        newRate,
			Reason:         fmt.Sprintf("High bounce rate: %.2f%% - reducing throttle by 30%%", bounceRate*100),
			Confidence:     0.85,
			RiskLevel:      "medium",
			ExpectedImpact: "Reduced sending rate to allow ISPs to recover",
		}
	}

	// POSITIVE: Low complaints, good engagement - consider increasing
	if complaintRate < settings.ComplaintThreshold*0.5 &&
		bounceRate < settings.BounceThreshold*0.5 &&
		openRate > 0.10 { // 10%+ open rate

		// Check if we can increase
		if settings.CurrentThrottleRate < settings.MaxThrottleRate {
			newRate := int(float64(settings.CurrentThrottleRate) * 1.25)
			return &ThrottleRecommendation{
				Action:         "increase",
				NewRate:        newRate,
				Reason:         fmt.Sprintf("Strong metrics (%.2f%% open, %.4f%% complaint) - increasing throttle by 25%%", openRate*100, complaintRate*100),
				Confidence:     0.8,
				RiskLevel:      "low",
				ExpectedImpact: "Faster campaign completion with maintained quality",
			}
		}
	}

	// Default: maintain current rate
	return &ThrottleRecommendation{
		Action:         "maintain",
		NewRate:        settings.CurrentThrottleRate,
		Reason:         fmt.Sprintf("Metrics within acceptable range (%.2f%% bounce, %.4f%% complaint)", bounceRate*100, complaintRate*100),
		Confidence:     0.7,
		RiskLevel:      "low",
		ExpectedImpact: "Continued stable sending",
	}
}

func (s *SmartSender) getRecentMetrics(ctx context.Context, campaignID uuid.UUID, window time.Duration) ([]*RealtimeMetrics, error) {
	cutoff := time.Now().Add(-window)

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, campaign_id, timestamp, interval_start, interval_end,
		       sent_count, delivered_count, open_count, unique_open_count,
		       click_count, unique_click_count, bounce_count, hard_bounce_count,
		       soft_bounce_count, complaint_count, unsubscribe_count,
		       open_rate, click_rate, bounce_rate, complaint_rate,
		       cumulative_sent, cumulative_opens, cumulative_clicks,
		       cumulative_bounces, cumulative_complaints,
		       current_throttle_rate, throttle_utilization,
		       ai_recommendation, ai_confidence, created_at
		FROM mailing_campaign_realtime_metrics
		WHERE campaign_id = $1 AND timestamp >= $2
		ORDER BY timestamp DESC
	`, campaignID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var metrics []*RealtimeMetrics
	for rows.Next() {
		var m RealtimeMetrics
		err := rows.Scan(
			&m.ID, &m.CampaignID, &m.Timestamp, &m.IntervalStart, &m.IntervalEnd,
			&m.SentCount, &m.DeliveredCount, &m.OpenCount, &m.UniqueOpenCount,
			&m.ClickCount, &m.UniqueClickCount, &m.BounceCount, &m.HardBounceCount,
			&m.SoftBounceCount, &m.ComplaintCount, &m.UnsubscribeCount,
			&m.OpenRate, &m.ClickRate, &m.BounceRate, &m.ComplaintRate,
			&m.CumulativeSent, &m.CumulativeOpens, &m.CumulativeClicks,
			&m.CumulativeBounces, &m.CumulativeComplaints,
			&m.CurrentThrottleRate, &m.ThrottleUtilization,
			&m.AIRecommendation, &m.AIConfidence, &m.CreatedAt,
		)
		if err != nil {
			continue
		}
		metrics = append(metrics, &m)
	}

	return metrics, nil
}

func (s *SmartSender) updateThrottleRate(ctx context.Context, campaignID uuid.UUID, newRate int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE mailing_campaign_ai_settings
		SET current_throttle_rate = $2, updated_at = NOW()
		WHERE campaign_id = $1
	`, campaignID, newRate)
	return err
}

func (s *SmartSender) pauseCampaign(ctx context.Context, campaignID uuid.UUID, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE mailing_campaigns
		SET status = 'paused', updated_at = NOW()
		WHERE id = $1
	`, campaignID)

	if err != nil {
		return err
	}

	// Create alert
	s.createAlert(ctx, campaignID, AlertHighComplaint, AlertSeverityCritical,
		"Campaign Paused - High Complaints",
		reason,
		nil, 0, 0, "paused")

	return nil
}

// ============================================================================
// SEND TIME OPTIMIZATION
// ============================================================================

// GetOptimalSendTime calculates the optimal send time for a subscriber
func (s *SmartSender) GetOptimalSendTime(ctx context.Context, email string) (*OptimalSendTimeResult, error) {
	emailHash := hashEmail(email)
	domain := extractEmailDomain(email)

	result := &OptimalSendTimeResult{
		Email:      email,
		Source:     "default",
		Confidence: 0.5,
	}

	// 1. Try subscriber-level profile
	profile, err := s.getInboxProfile(ctx, emailHash)
	if err == nil && profile != nil && profile.OptimalSendHour != nil {
		if profile.TotalOpens >= 5 { // Need at least 5 opens for confidence
			result.OptimalHourUTC = *profile.OptimalSendHour
			result.Confidence = profile.OptimalSendHourConfidence
			result.EngagementScore = profile.EngagementScore
			result.Source = "subscriber"
			result.OptimalTime = s.calculateNextOptimalTime(*profile.OptimalSendHour)
			return result, nil
		}
	}

	// 2. Try domain-level patterns
	domainTimes, err := s.getDomainSendTimes(ctx, domain)
	if err == nil && domainTimes != nil {
		optimalHours := s.parseOptimalHours(domainTimes.WeekdayOptimalHours)
		if len(optimalHours) > 0 {
			result.OptimalHourUTC = optimalHours[0]
			result.Confidence = 0.7
			result.Source = "domain"
			result.OptimalTime = s.calculateNextOptimalTime(optimalHours[0])
			return result, nil
		}
	}

	// 3. Default: 10 AM UTC
	result.OptimalHourUTC = 10
	result.OptimalTime = s.calculateNextOptimalTime(10)
	return result, nil
}

func (s *SmartSender) getInboxProfile(ctx context.Context, emailHash string) (*InboxProfile, error) {
	var profile InboxProfile
	err := s.db.QueryRowContext(ctx, `
		SELECT id, email_hash, domain, isp, optimal_send_hour, optimal_send_day,
		       optimal_send_hour_confidence, avg_open_delay_minutes, avg_click_delay_minutes,
		       engagement_score, engagement_trend, last_open_at, last_click_at, last_send_at,
		       total_sends, total_opens, total_clicks, total_bounces, total_complaints,
		       hourly_open_histogram, daily_open_histogram, first_seen_at, updated_at
		FROM mailing_inbox_profiles
		WHERE email_hash = $1
	`, emailHash).Scan(
		&profile.ID, &profile.EmailHash, &profile.Domain, &profile.ISP,
		&profile.OptimalSendHour, &profile.OptimalSendDay, &profile.OptimalSendHourConfidence,
		&profile.AvgOpenDelayMinutes, &profile.AvgClickDelayMinutes,
		&profile.EngagementScore, &profile.EngagementTrend,
		&profile.LastOpenAt, &profile.LastClickAt, &profile.LastSendAt,
		&profile.TotalSends, &profile.TotalOpens, &profile.TotalClicks,
		&profile.TotalBounces, &profile.TotalComplaints,
		&profile.HourlyOpenHistogram, &profile.DailyOpenHistogram,
		&profile.FirstSeenAt, &profile.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &profile, err
}

func (s *SmartSender) getDomainSendTimes(ctx context.Context, domain string) (*DomainSendTime, error) {
	var dst DomainSendTime
	err := s.db.QueryRowContext(ctx, `
		SELECT id, domain, isp, weekday_optimal_hours, weekend_optimal_hours,
		       hourly_engagement_scores, sample_size, avg_open_rate, avg_click_rate,
		       last_calculated_at, created_at, updated_at
		FROM mailing_domain_send_times
		WHERE domain = $1
	`, domain).Scan(
		&dst.ID, &dst.Domain, &dst.ISP, &dst.WeekdayOptimalHours, &dst.WeekendOptimalHours,
		&dst.HourlyEngagementScores, &dst.SampleSize, &dst.AvgOpenRate, &dst.AvgClickRate,
		&dst.LastCalculatedAt, &dst.CreatedAt, &dst.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &dst, err
}

func (s *SmartSender) parseOptimalHours(hoursJSON JSON) []int {
	if hoursJSON == nil {
		return nil
	}
	data, ok := hoursJSON["data"]
	if !ok {
		// Try to parse directly as array
		var hours []int
		b, _ := json.Marshal(hoursJSON)
		json.Unmarshal(b, &hours)
		return hours
	}
	hours, ok := data.([]interface{})
	if !ok {
		return nil
	}
	result := make([]int, 0, len(hours))
	for _, h := range hours {
		if hour, ok := h.(float64); ok {
			result = append(result, int(hour))
		}
	}
	return result
}

func (s *SmartSender) calculateNextOptimalTime(hourUTC int) time.Time {
	now := time.Now().UTC()
	optimal := time.Date(now.Year(), now.Month(), now.Day(), hourUTC, 0, 0, 0, time.UTC)
	if optimal.Before(now) {
		optimal = optimal.Add(24 * time.Hour)
	}
	return optimal
}

// ============================================================================
// INBOX PROFILE MANAGEMENT
// ============================================================================

// UpdateInboxProfile updates the inbox profile from a tracking event
func (s *SmartSender) UpdateInboxProfile(ctx context.Context, email, eventType string, eventTime time.Time) error {
	emailHash := hashEmail(email)
	domain := extractEmailDomain(email)
	isp := getISPForDomain(domain)
	eventHour := eventTime.Hour()
	eventDay := int(eventTime.Weekday())

	// Call the database function
	_, err := s.db.ExecContext(ctx, `
		SELECT update_inbox_profile($1, $2, $3, $4, $5)
	`, emailHash, domain, eventType, eventHour, eventDay)

	if err != nil {
		// Fallback: do manual insert/update
		return s.updateInboxProfileManual(ctx, emailHash, domain, isp, eventType, eventHour, eventDay)
	}

	return nil
}

func (s *SmartSender) updateInboxProfileManual(ctx context.Context, emailHash, domain, isp, eventType string, eventHour, eventDay int) error {
	// Ensure profile exists
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mailing_inbox_profiles (email_hash, domain, isp, total_sends)
		VALUES ($1, $2, $3, 0)
		ON CONFLICT (email_hash) DO NOTHING
	`, emailHash, domain, isp)
	if err != nil {
		return err
	}

	switch eventType {
	case "sent":
		_, err = s.db.ExecContext(ctx, `
			UPDATE mailing_inbox_profiles
			SET total_sends = total_sends + 1, last_send_at = NOW(), updated_at = NOW()
			WHERE email_hash = $1
		`, emailHash)
	case "open", "opened":
		_, err = s.db.ExecContext(ctx, `
			UPDATE mailing_inbox_profiles
			SET total_opens = total_opens + 1, 
			    last_open_at = NOW(),
			    engagement_score = LEAST(1.0, engagement_score + 0.05),
			    updated_at = NOW()
			WHERE email_hash = $1
		`, emailHash)
	case "click", "clicked":
		_, err = s.db.ExecContext(ctx, `
			UPDATE mailing_inbox_profiles
			SET total_clicks = total_clicks + 1,
			    last_click_at = NOW(),
			    engagement_score = LEAST(1.0, engagement_score + 0.1),
			    updated_at = NOW()
			WHERE email_hash = $1
		`, emailHash)
	case "bounce", "bounced":
		_, err = s.db.ExecContext(ctx, `
			UPDATE mailing_inbox_profiles
			SET total_bounces = total_bounces + 1,
			    engagement_score = GREATEST(0, engagement_score - 0.2),
			    updated_at = NOW()
			WHERE email_hash = $1
		`, emailHash)
	case "complaint", "complained":
		_, err = s.db.ExecContext(ctx, `
			UPDATE mailing_inbox_profiles
			SET total_complaints = total_complaints + 1,
			    engagement_score = 0,
			    updated_at = NOW()
			WHERE email_hash = $1
		`, emailHash)
	}

	return err
}

// ============================================================================
// REAL-TIME METRICS
// ============================================================================

// RecordRealtimeMetrics records a real-time metrics snapshot
func (s *SmartSender) RecordRealtimeMetrics(ctx context.Context, campaignID uuid.UUID) (*RealtimeMetrics, error) {
	// Get campaign current stats
	var campaign Campaign
	err := s.db.QueryRowContext(ctx, `
		SELECT id, sent_count, delivered_count, open_count, unique_open_count,
		       click_count, unique_click_count, bounce_count, complaint_count, unsubscribe_count
		FROM mailing_campaigns
		WHERE id = $1
	`, campaignID).Scan(
		&campaign.ID, &campaign.SentCount, &campaign.DeliveredCount,
		&campaign.OpenCount, &campaign.UniqueOpenCount,
		&campaign.ClickCount, &campaign.UniqueClickCount,
		&campaign.BounceCount, &campaign.ComplaintCount, &campaign.UnsubscribeCount,
	)
	if err != nil {
		return nil, err
	}

	// Get previous metrics for delta calculation
	var prevMetrics RealtimeMetrics
	err = s.db.QueryRowContext(ctx, `
		SELECT cumulative_sent, cumulative_opens, cumulative_clicks,
		       cumulative_bounces, cumulative_complaints
		FROM mailing_campaign_realtime_metrics
		WHERE campaign_id = $1
		ORDER BY timestamp DESC
		LIMIT 1
	`, campaignID).Scan(
		&prevMetrics.CumulativeSent, &prevMetrics.CumulativeOpens, &prevMetrics.CumulativeClicks,
		&prevMetrics.CumulativeBounces, &prevMetrics.CumulativeComplaints,
	)
	if err == sql.ErrNoRows {
		// First record, no previous
		prevMetrics = RealtimeMetrics{}
	}

	// Get AI settings for throttle rate
	settings, _ := s.GetAISettings(ctx, campaignID)
	throttleRate := 10000
	if settings != nil {
		throttleRate = settings.CurrentThrottleRate
	}

	now := time.Now()
	intervalStart := now.Add(-1 * time.Minute)

	// Calculate rates
	openRate := float64(0)
	clickRate := float64(0)
	bounceRate := float64(0)
	complaintRate := float64(0)
	if campaign.SentCount > 0 {
		openRate = float64(campaign.OpenCount) / float64(campaign.SentCount)
		clickRate = float64(campaign.ClickCount) / float64(campaign.SentCount)
		bounceRate = float64(campaign.BounceCount) / float64(campaign.SentCount)
		complaintRate = float64(campaign.ComplaintCount) / float64(campaign.SentCount)
	}

	// Calculate throttle utilization
	sentThisInterval := campaign.SentCount - prevMetrics.CumulativeSent
	throttleUtilization := float64(0)
	if throttleRate > 0 {
		// Convert to per-minute
		throttlePerMinute := throttleRate / 60
		if throttlePerMinute > 0 {
			throttleUtilization = float64(sentThisInterval) / float64(throttlePerMinute)
		}
	}

	// Insert new metrics record
	metrics := &RealtimeMetrics{
		CampaignID:           campaignID,
		Timestamp:            now,
		IntervalStart:        intervalStart,
		IntervalEnd:          now,
		SentCount:            campaign.SentCount - prevMetrics.CumulativeSent,
		DeliveredCount:       campaign.DeliveredCount,
		OpenCount:            campaign.OpenCount - prevMetrics.CumulativeOpens,
		ClickCount:           campaign.ClickCount - prevMetrics.CumulativeClicks,
		BounceCount:          campaign.BounceCount - prevMetrics.CumulativeBounces,
		ComplaintCount:       campaign.ComplaintCount - prevMetrics.CumulativeComplaints,
		CumulativeSent:       campaign.SentCount,
		CumulativeOpens:      campaign.OpenCount,
		CumulativeClicks:     campaign.ClickCount,
		CumulativeBounces:    campaign.BounceCount,
		CumulativeComplaints: campaign.ComplaintCount,
		OpenRate:             openRate,
		ClickRate:            clickRate,
		BounceRate:           bounceRate,
		ComplaintRate:        complaintRate,
		CurrentThrottleRate:  throttleRate,
		ThrottleUtilization:  throttleUtilization,
	}

	var id uuid.UUID
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO mailing_campaign_realtime_metrics (
			campaign_id, timestamp, interval_start, interval_end,
			sent_count, delivered_count, open_count, unique_open_count,
			click_count, unique_click_count, bounce_count,
			complaint_count, unsubscribe_count,
			open_rate, click_rate, bounce_rate, complaint_rate,
			cumulative_sent, cumulative_opens, cumulative_clicks,
			cumulative_bounces, cumulative_complaints,
			current_throttle_rate, throttle_utilization
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24)
		RETURNING id
	`, campaignID, now, intervalStart, now,
		metrics.SentCount, metrics.DeliveredCount, metrics.OpenCount, campaign.UniqueOpenCount,
		metrics.ClickCount, campaign.UniqueClickCount, metrics.BounceCount,
		metrics.ComplaintCount, campaign.UnsubscribeCount,
		openRate, clickRate, bounceRate, complaintRate,
		campaign.SentCount, campaign.OpenCount, campaign.ClickCount,
		campaign.BounceCount, campaign.ComplaintCount,
		throttleRate, throttleUtilization,
	).Scan(&id)

	if err != nil {
		return nil, err
	}

	metrics.ID = id
	return metrics, nil
}

// GetRealtimeMetrics retrieves real-time metrics for a campaign
func (s *SmartSender) GetRealtimeMetrics(ctx context.Context, campaignID uuid.UUID, limit int) ([]*RealtimeMetrics, error) {
	if limit <= 0 {
		limit = 60 // Last hour by default
	}

	return s.getRecentMetrics(ctx, campaignID, time.Duration(limit)*time.Minute)
}

// ============================================================================
// AI DECISION LOGGING
// ============================================================================

func (s *SmartSender) logAIDecision(ctx context.Context, campaignID uuid.UUID, decisionType DecisionType, reason, oldValue, newValue string, metricsSnapshot []byte, confidence float64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mailing_ai_decisions (
			campaign_id, decision_type, decision_reason, old_value, new_value,
			metrics_snapshot, ai_model, confidence, applied, applied_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true, NOW())
	`, campaignID, string(decisionType), reason, oldValue, newValue, metricsSnapshot, "rules-based", confidence)
	return err
}

// GetAIDecisions retrieves AI decisions for a campaign
func (s *SmartSender) GetAIDecisions(ctx context.Context, campaignID uuid.UUID, limit int) ([]*AIDecision, error) {
	if limit <= 0 {
		limit = 50
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, campaign_id, organization_id, decision_type, decision_reason,
		       old_value, new_value, metrics_snapshot, ai_model, confidence,
		       applied, applied_at, reverted, reverted_at, revert_reason, created_at
		FROM mailing_ai_decisions
		WHERE campaign_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, campaignID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var decisions []*AIDecision
	for rows.Next() {
		var d AIDecision
		err := rows.Scan(
			&d.ID, &d.CampaignID, &d.OrganizationID, &d.DecisionType, &d.DecisionReason,
			&d.OldValue, &d.NewValue, &d.MetricsSnapshot, &d.AIModel, &d.Confidence,
			&d.Applied, &d.AppliedAt, &d.Reverted, &d.RevertedAt, &d.RevertReason, &d.CreatedAt,
		)
		if err != nil {
			continue
		}
		decisions = append(decisions, &d)
	}

	return decisions, nil
}

// ============================================================================
// ALERTS
// ============================================================================

func (s *SmartSender) createAlert(ctx context.Context, campaignID uuid.UUID, alertType AlertType, severity AlertSeverity, title, message string, metricsSnapshot JSON, threshold, actual float64, autoAction string) error {
	snapshotJSON, _ := json.Marshal(metricsSnapshot)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mailing_campaign_alerts (
			campaign_id, alert_type, severity, title, message,
			metrics_snapshot, threshold_value, actual_value, auto_action_taken
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, campaignID, string(alertType), string(severity), title, message, snapshotJSON, threshold, actual, autoAction)
	return err
}

// GetCampaignAlerts retrieves alerts for a campaign
func (s *SmartSender) GetCampaignAlerts(ctx context.Context, campaignID uuid.UUID, unacknowledgedOnly bool) ([]*CampaignAlert, error) {
	query := `
		SELECT id, campaign_id, organization_id, alert_type, severity, title, message,
		       metrics_snapshot, threshold_value, actual_value, acknowledged,
		       acknowledged_by, acknowledged_at, auto_action_taken, created_at
		FROM mailing_campaign_alerts
		WHERE campaign_id = $1
	`
	if unacknowledgedOnly {
		query += " AND acknowledged = false"
	}
	query += " ORDER BY created_at DESC LIMIT 100"

	rows, err := s.db.QueryContext(ctx, query, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var alerts []*CampaignAlert
	for rows.Next() {
		var a CampaignAlert
		err := rows.Scan(
			&a.ID, &a.CampaignID, &a.OrganizationID, &a.AlertType, &a.Severity,
			&a.Title, &a.Message, &a.MetricsSnapshot, &a.ThresholdValue, &a.ActualValue,
			&a.Acknowledged, &a.AcknowledgedBy, &a.AcknowledgedAt, &a.AutoActionTaken, &a.CreatedAt,
		)
		if err != nil {
			continue
		}
		alerts = append(alerts, &a)
	}

	return alerts, nil
}

// AcknowledgeAlert marks an alert as acknowledged
func (s *SmartSender) AcknowledgeAlert(ctx context.Context, alertID, userID uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE mailing_campaign_alerts
		SET acknowledged = true, acknowledged_by = $2, acknowledged_at = NOW()
		WHERE id = $1
	`, alertID, userID)
	return err
}

// ============================================================================
// WEBHOOK EVENT PROCESSING
// ============================================================================

// ProcessSparkPostEvent processes a SparkPost webhook event
func (s *SmartSender) ProcessSparkPostEvent(ctx context.Context, event *SparkPostWebhookEvent) error {
	// Update inbox profile
	if event.Recipient != "" {
		if err := s.UpdateInboxProfile(ctx, event.Recipient, event.Type, event.Timestamp); err != nil {
			log.Printf("[SmartSender] Failed to update inbox profile: %v", err)
		}
	}

	// Check for anomalies that need immediate attention
	if event.Type == "bounce" || event.Type == "complaint" {
		campaignID, err := uuid.Parse(event.CampaignID)
		if err == nil {
			go s.checkForAnomalies(context.Background(), campaignID)
		}
	}

	return nil
}

func (s *SmartSender) checkForAnomalies(ctx context.Context, campaignID uuid.UUID) {
	settings, err := s.GetAISettings(ctx, campaignID)
	if err != nil || !settings.EnableSmartSending {
		return
	}

	// Get recent metrics
	metrics, err := s.getRecentMetrics(ctx, campaignID, 5*time.Minute)
	if err != nil || len(metrics) == 0 {
		return
	}

	// Check complaint rate
	var totalSent, totalComplaints int
	for _, m := range metrics {
		totalSent += m.CumulativeSent
		totalComplaints += m.CumulativeComplaints
	}

	if totalSent > 0 {
		complaintRate := float64(totalComplaints) / float64(totalSent)
		if complaintRate > settings.ComplaintThreshold && settings.PauseOnHighComplaints {
			// Auto-pause
			s.pauseCampaign(ctx, campaignID, fmt.Sprintf("Auto-pause: Complaint rate %.4f%% exceeds threshold", complaintRate*100))
		}
	}
}

// ============================================================================
// CAMPAIGN HEALTH SCORING
// ============================================================================

// GetCampaignHealthScore calculates overall campaign health
func (s *SmartSender) GetCampaignHealthScore(ctx context.Context, campaignID uuid.UUID) (*CampaignHealthScore, error) {
	metrics, err := s.getRecentMetrics(ctx, campaignID, 60*time.Minute)
	if err != nil {
		return nil, err
	}

	health := &CampaignHealthScore{
		CampaignID:  campaignID,
		LastUpdated: time.Now(),
	}

	if len(metrics) == 0 {
		health.OverallScore = 100
		health.DeliverabilityScore = 100
		health.EngagementScore = 100
		health.ReputationScore = 100
		return health, nil
	}

	// Get latest cumulative metrics
	latest := metrics[0]

	// Calculate deliverability score (based on bounce rate)
	if latest.CumulativeSent > 0 {
		bounceRate := float64(latest.CumulativeBounces) / float64(latest.CumulativeSent)
		health.DeliverabilityScore = math.Max(0, 100-bounceRate*1000)

		// Engagement score (based on open rate)
		openRate := float64(latest.CumulativeOpens) / float64(latest.CumulativeSent)
		health.EngagementScore = math.Min(100, openRate*500) // 20% open rate = 100

		// Reputation score (based on complaint rate)
		complaintRate := float64(latest.CumulativeComplaints) / float64(latest.CumulativeSent)
		health.ReputationScore = math.Max(0, 100-complaintRate*10000)
	} else {
		health.DeliverabilityScore = 100
		health.EngagementScore = 50
		health.ReputationScore = 100
	}

	// Overall score is weighted average
	health.OverallScore = health.DeliverabilityScore*0.3 + health.EngagementScore*0.3 + health.ReputationScore*0.4

	// Add issues and recommendations
	if health.DeliverabilityScore < 80 {
		health.Issues = append(health.Issues, "High bounce rate detected")
		health.Recommendations = append(health.Recommendations, "Review list quality and remove invalid addresses")
	}
	if health.EngagementScore < 50 {
		health.Issues = append(health.Issues, "Low engagement rate")
		health.Recommendations = append(health.Recommendations, "Consider optimizing subject lines and send times")
	}
	if health.ReputationScore < 90 {
		health.Issues = append(health.Issues, "Elevated complaint rate")
		health.Recommendations = append(health.Recommendations, "Review content and ensure clear unsubscribe options")
	}

	// Calculate trends
	if len(metrics) >= 2 {
		health.Trends = s.calculateTrends(metrics)
	}

	return health, nil
}

func (s *SmartSender) calculateTrends(metrics []*RealtimeMetrics) []MetricsTrend {
	var trends []MetricsTrend

	if len(metrics) < 2 {
		return trends
	}

	// Open rate trend
	openRates := make([]float64, len(metrics))
	timestamps := make([]int64, len(metrics))
	for i, m := range metrics {
		openRates[i] = m.OpenRate
		timestamps[i] = m.Timestamp.Unix()
	}

	currentOpen := openRates[0]
	previousOpen := openRates[len(openRates)-1]
	openTrend := "stable"
	openChange := float64(0)
	if previousOpen > 0 {
		openChange = (currentOpen - previousOpen) / previousOpen * 100
		if openChange > 5 {
			openTrend = "increasing"
		} else if openChange < -5 {
			openTrend = "decreasing"
		}
	}

	trends = append(trends, MetricsTrend{
		MetricName:    "open_rate",
		CurrentValue:  currentOpen,
		PreviousValue: previousOpen,
		ChangePercent: openChange,
		Trend:         openTrend,
		DataPoints:    openRates,
		Timestamps:    timestamps,
	})

	return trends
}

// ============================================================================
// UTILITY FUNCTIONS
// ============================================================================

func hashEmail(email string) string {
	h := sha256.New()
	h.Write([]byte(strings.ToLower(email)))
	return hex.EncodeToString(h.Sum(nil))
}

func extractEmailDomain(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		return ""
	}
	return strings.ToLower(parts[1])
}

func getISPForDomain(domain string) string {
	domain = strings.ToLower(domain)

	ispDomains := map[string][]string{
		"gmail":     {"gmail.com", "googlemail.com"},
		"yahoo":     {"yahoo.com", "yahoo.co.uk", "ymail.com", "rocketmail.com"},
		"microsoft": {"outlook.com", "hotmail.com", "live.com", "msn.com"},
		"aol":       {"aol.com", "aim.com"},
		"apple":     {"icloud.com", "me.com", "mac.com"},
	}

	for isp, domains := range ispDomains {
		for _, d := range domains {
			if d == domain {
				return isp
			}
		}
	}
	return "other"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
