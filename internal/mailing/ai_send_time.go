package mailing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// AISendTimeService provides AI-powered send time optimization
type AISendTimeService struct {
	db *sql.DB
}

// NewAISendTimeService creates a new AISendTimeService
func NewAISendTimeService(db *sql.DB) *AISendTimeService {
	return &AISendTimeService{db: db}
}

// OptimalSendTime represents a subscriber's calculated optimal send time
type OptimalSendTime struct {
	SubscriberID   string    `json:"subscriber_id"`
	OptimalHour    int       `json:"optimal_hour"`    // 0-23 UTC
	OptimalDay     *int      `json:"optimal_day"`     // 0-6 (Sun-Sat), nil if no day preference
	Timezone       string    `json:"timezone"`
	Confidence     float64   `json:"confidence"`
	SampleSize     int       `json:"sample_size"`
	LastCalculated time.Time `json:"last_calculated"`
	Factors        JSON      `json:"factors"`
}

// SendTimeRecommendation represents a specific send time recommendation
type SendTimeRecommendation struct {
	RecommendedTime time.Time `json:"recommended_time"`
	LocalTime       time.Time `json:"local_time"`
	Timezone        string    `json:"timezone"`
	Confidence      float64   `json:"confidence"`
	Reasoning       string    `json:"reasoning"`
	Source          string    `json:"source"` // "subscriber", "audience", "industry_default"
}

// AudienceOptimalTimes represents optimal times for an entire audience/list
type AudienceOptimalTimes struct {
	ListID          string             `json:"list_id,omitempty"`
	SegmentID       string             `json:"segment_id,omitempty"`
	BestHours       []HourDistribution `json:"best_hours"`
	BestDays        []DayDistribution  `json:"best_days"`
	OverallBestTime time.Time          `json:"overall_best_time"`
	OverallBestHour int                `json:"overall_best_hour"`
	OverallBestDay  int                `json:"overall_best_day"`
	SampleSize      int                `json:"sample_size"`
	Confidence      float64            `json:"confidence_score"`
	LastCalculated  time.Time          `json:"last_calculated"`
}

// HourDistribution represents engagement distribution for a specific hour
type HourDistribution struct {
	Hour       int     `json:"hour"`
	OpenRate   float64 `json:"open_rate"`
	ClickRate  float64 `json:"click_rate"`
	SampleSize int     `json:"sample_size"`
}

// DayDistribution represents engagement distribution for a specific day
type DayDistribution struct {
	Day        int     `json:"day"` // 0-6 (Sunday-Saturday)
	DayName    string  `json:"day_name"`
	OpenRate   float64 `json:"open_rate"`
	ClickRate  float64 `json:"click_rate"`
	SampleSize int     `json:"sample_size"`
}

// CampaignScheduleResult holds the per-subscriber scheduled times for a campaign
type CampaignScheduleResult struct {
	CampaignID     string                         `json:"campaign_id"`
	TotalScheduled int                            `json:"total_scheduled"`
	ScheduledTimes map[string]time.Time           `json:"scheduled_times"` // subscriber_id -> time
	Recommendations map[string]*SendTimeRecommendation `json:"recommendations,omitempty"`
}

// dayNames maps day numbers to names
var dayNames = map[int]string{
	0: "Sunday",
	1: "Monday",
	2: "Tuesday",
	3: "Wednesday",
	4: "Thursday",
	5: "Friday",
	6: "Saturday",
}

// GetOptimalSendTime returns the optimal send time recommendation for a subscriber
func (s *AISendTimeService) GetOptimalSendTime(ctx context.Context, subscriberID string) (*SendTimeRecommendation, error) {
	subUUID, err := uuid.Parse(subscriberID)
	if err != nil {
		return nil, fmt.Errorf("invalid subscriber ID: %w", err)
	}

	// First, try to get subscriber-specific optimal time
	var optimalHour int
	var optimalDay sql.NullInt64
	var timezone sql.NullString
	var confidence float64
	var sampleSize int
	var lastCalculated time.Time

	err = s.db.QueryRowContext(ctx, `
		SELECT optimal_hour, optimal_day, timezone, confidence, sample_size, last_calculated
		FROM mailing_subscriber_optimal_times
		WHERE subscriber_id = $1
	`, subUUID).Scan(&optimalHour, &optimalDay, &timezone, &confidence, &sampleSize, &lastCalculated)

	if err == nil && sampleSize >= 5 && confidence >= 0.6 {
		// We have enough subscriber-specific data
		return s.buildRecommendation(optimalHour, optimalDay, timezone.String, confidence, "subscriber"), nil
	}

	// Fall back to subscriber's list-level data
	var listID uuid.UUID
	err = s.db.QueryRowContext(ctx, `
		SELECT list_id FROM mailing_subscribers WHERE id = $1
	`, subUUID).Scan(&listID)

	if err == nil {
		listOptimal, err := s.CalculateAudienceOptimalTimes(ctx, listID.String())
		if err == nil && listOptimal.SampleSize >= 10 {
			tz := timezone.String
			if tz == "" {
				tz = "UTC"
			}
			return s.buildRecommendation(listOptimal.OverallBestHour, 
				sql.NullInt64{Int64: int64(listOptimal.OverallBestDay), Valid: listOptimal.OverallBestDay >= 0}, 
				tz, listOptimal.Confidence * 0.8, "audience"), nil
		}
	}

	// Fall back to industry defaults
	return s.getIndustryDefault(ctx)
}

// GetBulkOptimalTimes returns optimal send times for multiple subscribers
func (s *AISendTimeService) GetBulkOptimalTimes(ctx context.Context, subscriberIDs []string) (map[string]*SendTimeRecommendation, error) {
	results := make(map[string]*SendTimeRecommendation)

	if len(subscriberIDs) == 0 {
		return results, nil
	}

	// Convert to UUIDs
	uuids := make([]uuid.UUID, 0, len(subscriberIDs))
	for _, id := range subscriberIDs {
		u, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		uuids = append(uuids, u)
	}

	// Batch query for subscriber optimal times
	rows, err := s.db.QueryContext(ctx, `
		SELECT sot.subscriber_id, sot.optimal_hour, sot.optimal_day, 
		       COALESCE(sot.timezone, s.timezone, 'UTC') as timezone,
		       sot.confidence, sot.sample_size
		FROM mailing_subscriber_optimal_times sot
		JOIN mailing_subscribers s ON s.id = sot.subscriber_id
		WHERE sot.subscriber_id = ANY($1)
	`, pq.Array(uuids))
	if err != nil {
		return nil, fmt.Errorf("failed to query optimal times: %w", err)
	}
	defer rows.Close()

	subscriberData := make(map[string]struct {
		Hour       int
		Day        sql.NullInt64
		Timezone   string
		Confidence float64
		SampleSize int
	})

	for rows.Next() {
		var subID uuid.UUID
		var data struct {
			Hour       int
			Day        sql.NullInt64
			Timezone   string
			Confidence float64
			SampleSize int
		}
		if err := rows.Scan(&subID, &data.Hour, &data.Day, &data.Timezone, &data.Confidence, &data.SampleSize); err != nil {
			continue
		}
		subscriberData[subID.String()] = data
	}

	// Get list-level defaults for subscribers without personal data
	var listDefaults = make(map[uuid.UUID]*AudienceOptimalTimes)

	// Build recommendations
	industryDefault, _ := s.getIndustryDefault(ctx)

	for _, id := range subscriberIDs {
		if data, ok := subscriberData[id]; ok && data.SampleSize >= 5 && data.Confidence >= 0.6 {
			results[id] = s.buildRecommendation(data.Hour, data.Day, data.Timezone, data.Confidence, "subscriber")
		} else if industryDefault != nil {
			// Use industry default with reduced confidence
			rec := *industryDefault
			rec.Confidence *= 0.5
			rec.Reasoning = "Using industry default due to insufficient subscriber data"
			rec.Source = "industry_default"
			results[id] = &rec
		}
	}

	// Fill in missing with list-level data where available
	for _, id := range subscriberIDs {
		if _, ok := results[id]; ok {
			continue
		}

		subUUID, err := uuid.Parse(id)
		if err != nil {
			continue
		}

		var listID uuid.UUID
		s.db.QueryRowContext(ctx, `SELECT list_id FROM mailing_subscribers WHERE id = $1`, subUUID).Scan(&listID)

		if listID != uuid.Nil {
			if listOpt, ok := listDefaults[listID]; ok {
				results[id] = s.buildRecommendation(listOpt.OverallBestHour, 
					sql.NullInt64{Int64: int64(listOpt.OverallBestDay), Valid: true}, 
					"UTC", listOpt.Confidence * 0.7, "audience")
			}
		}
	}

	return results, nil
}

// CalculateAudienceOptimalTimes calculates optimal times for an entire list
func (s *AISendTimeService) CalculateAudienceOptimalTimes(ctx context.Context, listID string) (*AudienceOptimalTimes, error) {
	listUUID, err := uuid.Parse(listID)
	if err != nil {
		return nil, fmt.Errorf("invalid list ID: %w", err)
	}

	// Check cache first
	var cached AudienceOptimalTimes
	var bestHoursJSON, bestDaysJSON []byte
	err = s.db.QueryRowContext(ctx, `
		SELECT best_hours, best_days, overall_best_hour, overall_best_day,
		       total_sample_size, confidence_score, last_calculated
		FROM mailing_audience_optimal_times
		WHERE list_id = $1 AND last_calculated > NOW() - INTERVAL '24 hours'
	`, listUUID).Scan(&bestHoursJSON, &bestDaysJSON, &cached.OverallBestHour, &cached.OverallBestDay,
		&cached.SampleSize, &cached.Confidence, &cached.LastCalculated)

	if err == nil && cached.SampleSize > 0 {
		json.Unmarshal(bestHoursJSON, &cached.BestHours)
		json.Unmarshal(bestDaysJSON, &cached.BestDays)
		cached.ListID = listID
		// Calculate overall best time for next occurrence
		cached.OverallBestTime = s.calculateNextOccurrence(cached.OverallBestHour, cached.OverallBestDay)
		return &cached, nil
	}

	// Calculate from historical data
	result := &AudienceOptimalTimes{
		ListID:     listID,
		BestHours:  make([]HourDistribution, 0, 24),
		BestDays:   make([]DayDistribution, 0, 7),
	}

	// Query hourly distribution with exponential decay weighting
	hourRows, err := s.db.QueryContext(ctx, `
		WITH weighted_events AS (
			SELECT 
				sth.sent_hour,
				sth.opened,
				sth.clicked,
				POWER(0.95, EXTRACT(DAY FROM NOW() - sth.sent_at)) as weight
			FROM mailing_send_time_history sth
			JOIN mailing_subscribers s ON s.id = sth.subscriber_id
			WHERE s.list_id = $1
			AND sth.sent_at > NOW() - INTERVAL '90 days'
		)
		SELECT 
			sent_hour,
			COALESCE(SUM(CASE WHEN opened THEN weight ELSE 0 END) / NULLIF(SUM(weight), 0), 0) * 100 as open_rate,
			COALESCE(SUM(CASE WHEN clicked THEN weight ELSE 0 END) / NULLIF(SUM(weight), 0), 0) * 100 as click_rate,
			COUNT(*) as sample_size
		FROM weighted_events
		GROUP BY sent_hour
		ORDER BY sent_hour
	`, listUUID)
	
	if err == nil {
		defer hourRows.Close()
		for hourRows.Next() {
			var dist HourDistribution
			if err := hourRows.Scan(&dist.Hour, &dist.OpenRate, &dist.ClickRate, &dist.SampleSize); err == nil {
				result.BestHours = append(result.BestHours, dist)
				result.SampleSize += dist.SampleSize
			}
		}
	}

	// Query daily distribution
	dayRows, err := s.db.QueryContext(ctx, `
		WITH weighted_events AS (
			SELECT 
				sth.sent_day,
				sth.opened,
				sth.clicked,
				POWER(0.95, EXTRACT(DAY FROM NOW() - sth.sent_at)) as weight
			FROM mailing_send_time_history sth
			JOIN mailing_subscribers s ON s.id = sth.subscriber_id
			WHERE s.list_id = $1
			AND sth.sent_at > NOW() - INTERVAL '90 days'
		)
		SELECT 
			sent_day,
			COALESCE(SUM(CASE WHEN opened THEN weight ELSE 0 END) / NULLIF(SUM(weight), 0), 0) * 100 as open_rate,
			COALESCE(SUM(CASE WHEN clicked THEN weight ELSE 0 END) / NULLIF(SUM(weight), 0), 0) * 100 as click_rate,
			COUNT(*) as sample_size
		FROM weighted_events
		GROUP BY sent_day
		ORDER BY sent_day
	`, listUUID)

	if err == nil {
		defer dayRows.Close()
		for dayRows.Next() {
			var dist DayDistribution
			if err := dayRows.Scan(&dist.Day, &dist.OpenRate, &dist.ClickRate, &dist.SampleSize); err == nil {
				dist.DayName = dayNames[dist.Day]
				result.BestDays = append(result.BestDays, dist)
			}
		}
	}

	// Sort and find best
	if len(result.BestHours) > 0 {
		sort.Slice(result.BestHours, func(i, j int) bool {
			return result.BestHours[i].OpenRate > result.BestHours[j].OpenRate
		})
		result.OverallBestHour = result.BestHours[0].Hour
	} else {
		result.OverallBestHour = 10 // Default to 10 AM UTC
	}

	if len(result.BestDays) > 0 {
		sort.Slice(result.BestDays, func(i, j int) bool {
			return result.BestDays[i].OpenRate > result.BestDays[j].OpenRate
		})
		result.OverallBestDay = result.BestDays[0].Day
	} else {
		result.OverallBestDay = 2 // Default to Tuesday
	}

	// Calculate confidence based on sample size
	result.Confidence = math.Min(0.9, 0.3 + float64(result.SampleSize) * 0.001)
	result.LastCalculated = time.Now()
	result.OverallBestTime = s.calculateNextOccurrence(result.OverallBestHour, result.OverallBestDay)

	// Cache the result
	bestHoursJSON, _ = json.Marshal(result.BestHours)
	bestDaysJSON, _ = json.Marshal(result.BestDays)

	var orgID uuid.UUID
	s.db.QueryRowContext(ctx, `SELECT organization_id FROM mailing_lists WHERE id = $1`, listUUID).Scan(&orgID)

	s.db.ExecContext(ctx, `
		INSERT INTO mailing_audience_optimal_times (
			list_id, organization_id, best_hours, best_days, overall_best_hour, overall_best_day,
			total_sample_size, confidence_score, last_calculated
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		ON CONFLICT (list_id, segment_id) DO UPDATE SET
			best_hours = EXCLUDED.best_hours,
			best_days = EXCLUDED.best_days,
			overall_best_hour = EXCLUDED.overall_best_hour,
			overall_best_day = EXCLUDED.overall_best_day,
			total_sample_size = EXCLUDED.total_sample_size,
			confidence_score = EXCLUDED.confidence_score,
			last_calculated = NOW(),
			updated_at = NOW()
	`, listUUID, orgID, bestHoursJSON, bestDaysJSON, result.OverallBestHour, result.OverallBestDay,
		result.SampleSize, result.Confidence)

	return result, nil
}

// RecalculateSubscriberTime recalculates optimal time for a specific subscriber
func (s *AISendTimeService) RecalculateSubscriberTime(ctx context.Context, subscriberID string) (*OptimalSendTime, error) {
	subUUID, err := uuid.Parse(subscriberID)
	if err != nil {
		return nil, fmt.Errorf("invalid subscriber ID: %w", err)
	}

	// Query historical engagement with exponential decay
	rows, err := s.db.QueryContext(ctx, `
		SELECT 
			sent_hour,
			sent_day,
			opened,
			clicked,
			POWER(0.9, EXTRACT(DAY FROM NOW() - sent_at)) as weight,
			sent_at
		FROM mailing_send_time_history
		WHERE subscriber_id = $1
		AND sent_at > NOW() - INTERVAL '180 days'
		ORDER BY sent_at DESC
		LIMIT 100
	`, subUUID)
	if err != nil {
		return nil, fmt.Errorf("failed to query history: %w", err)
	}
	defer rows.Close()

	hourWeights := make(map[int]float64)
	hourOpens := make(map[int]float64)
	dayWeights := make(map[int]float64)
	dayOpens := make(map[int]float64)
	totalWeight := 0.0
	openedCount := 0

	for rows.Next() {
		var hour, day int
		var opened, clicked bool
		var weight float64
		var sentAt time.Time

		if err := rows.Scan(&hour, &day, &opened, &clicked, &weight, &sentAt); err != nil {
			continue
		}

		hourWeights[hour] += weight
		dayWeights[day] += weight
		totalWeight += weight

		if opened {
			hourOpens[hour] += weight
			dayOpens[day] += weight
			openedCount++
		}
	}

	if totalWeight == 0 {
		return nil, fmt.Errorf("no engagement data found for subscriber")
	}

	// Find best hour (highest weighted open rate)
	var bestHour int
	var bestHourScore float64
	for hour, weight := range hourWeights {
		if weight > 0 {
			score := hourOpens[hour] / weight
			if score > bestHourScore {
				bestHourScore = score
				bestHour = hour
			}
		}
	}

	// Find best day
	var bestDay int
	var bestDayScore float64
	for day, weight := range dayWeights {
		if weight > 0 {
			score := dayOpens[day] / weight
			if score > bestDayScore {
				bestDayScore = score
				bestDay = day
			}
		}
	}

	// Get subscriber timezone
	var timezone sql.NullString
	s.db.QueryRowContext(ctx, `SELECT timezone FROM mailing_subscribers WHERE id = $1`, subUUID).Scan(&timezone)

	// Calculate confidence
	confidence := math.Min(0.95, 0.3 + float64(openedCount) * 0.06)
	if openedCount < 5 {
		confidence *= 0.7
	}

	// Build factors JSON
	factors := JSON{
		"total_emails":     int(totalWeight),
		"opened_count":     openedCount,
		"best_hour_score":  bestHourScore,
		"best_day_score":   bestDayScore,
		"calculation_date": time.Now().Format(time.RFC3339),
	}

	// Upsert into database
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mailing_subscriber_optimal_times (
			subscriber_id, optimal_hour, optimal_day, timezone, 
			confidence, sample_size, factors, last_calculated
		) VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (subscriber_id) DO UPDATE SET
			optimal_hour = EXCLUDED.optimal_hour,
			optimal_day = EXCLUDED.optimal_day,
			timezone = COALESCE(EXCLUDED.timezone, mailing_subscriber_optimal_times.timezone),
			confidence = EXCLUDED.confidence,
			sample_size = EXCLUDED.sample_size,
			factors = EXCLUDED.factors,
			last_calculated = NOW(),
			updated_at = NOW()
	`, subUUID, bestHour, bestDay, timezone, confidence, openedCount, factors)
	if err != nil {
		return nil, fmt.Errorf("failed to save optimal time: %w", err)
	}

	result := &OptimalSendTime{
		SubscriberID:   subscriberID,
		OptimalHour:    bestHour,
		OptimalDay:     &bestDay,
		Timezone:       timezone.String,
		Confidence:     confidence,
		SampleSize:     openedCount,
		LastCalculated: time.Now(),
		Factors:        factors,
	}

	// Also update the main subscriber record
	s.db.ExecContext(ctx, `
		UPDATE mailing_subscribers 
		SET optimal_send_hour_utc = $1, updated_at = NOW()
		WHERE id = $2
	`, bestHour, subUUID)

	return result, nil
}

// ScheduleCampaignOptimally schedules a campaign with per-subscriber optimal times
func (s *AISendTimeService) ScheduleCampaignOptimally(ctx context.Context, campaignID string) (map[string]time.Time, error) {
	campUUID, err := uuid.Parse(campaignID)
	if err != nil {
		return nil, fmt.Errorf("invalid campaign ID: %w", err)
	}

	// Get campaign details
	var listID, segmentID sql.NullString
	var baseScheduleTime sql.NullTime
	var aiOptimization bool

	err = s.db.QueryRowContext(ctx, `
		SELECT list_id, segment_id, send_at, ai_send_time_optimization
		FROM mailing_campaigns WHERE id = $1
	`, campUUID).Scan(&listID, &segmentID, &baseScheduleTime, &aiOptimization)
	if err != nil {
		return nil, fmt.Errorf("campaign not found: %w", err)
	}

	if !aiOptimization {
		return nil, fmt.Errorf("AI send time optimization is not enabled for this campaign")
	}

	// Get subscribers for the campaign
	var subscriberQuery string
	var queryArgs []interface{}

	if segmentID.Valid {
		subscriberQuery = `
			SELECT s.id, sot.optimal_hour, COALESCE(sot.timezone, s.timezone, 'UTC'),
			       COALESCE(sot.confidence, 0.5)
			FROM mailing_subscribers s
			LEFT JOIN mailing_subscriber_optimal_times sot ON sot.subscriber_id = s.id
			WHERE s.list_id IN (SELECT list_id FROM mailing_segments WHERE id = $1)
			AND s.status = 'confirmed'
		`
		queryArgs = []interface{}{segmentID.String}
	} else if listID.Valid {
		subscriberQuery = `
			SELECT s.id, sot.optimal_hour, COALESCE(sot.timezone, s.timezone, 'UTC'),
			       COALESCE(sot.confidence, 0.5)
			FROM mailing_subscribers s
			LEFT JOIN mailing_subscriber_optimal_times sot ON sot.subscriber_id = s.id
			WHERE s.list_id = $1
			AND s.status = 'confirmed'
		`
		queryArgs = []interface{}{listID.String}
	} else {
		return nil, fmt.Errorf("campaign has no list or segment")
	}

	rows, err := s.db.QueryContext(ctx, subscriberQuery, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to get subscribers: %w", err)
	}
	defer rows.Close()

	// Base date for scheduling (either specified send_at or tomorrow)
	var baseDate time.Time
	if baseScheduleTime.Valid {
		baseDate = baseScheduleTime.Time
	} else {
		baseDate = time.Now().Add(24 * time.Hour).Truncate(24 * time.Hour)
	}

	scheduledTimes := make(map[string]time.Time)
	
	// Batch insert preparation
	type scheduleEntry struct {
		subscriberID uuid.UUID
		optimalHour  sql.NullInt64
		timezone     string
		confidence   float64
	}
	var entries []scheduleEntry

	for rows.Next() {
		var e scheduleEntry
		if err := rows.Scan(&e.subscriberID, &e.optimalHour, &e.timezone, &e.confidence); err != nil {
			continue
		}
		entries = append(entries, e)
	}

	// Get list-level default for subscribers without personal data
	var listDefault *AudienceOptimalTimes
	if listID.Valid {
		listDefault, _ = s.CalculateAudienceOptimalTimes(ctx, listID.String)
	}

	// Get industry default
	industryDefault, _ := s.getIndustryDefault(ctx)

	// Calculate scheduled times
	for _, e := range entries {
		var scheduledTime time.Time
		var confidence float64

		if e.optimalHour.Valid && e.confidence >= 0.5 {
			// Use subscriber-specific time
			scheduledTime = time.Date(
				baseDate.Year(), baseDate.Month(), baseDate.Day(),
				int(e.optimalHour.Int64), 0, 0, 0, time.UTC,
			)
			confidence = e.confidence
		} else if listDefault != nil && listDefault.SampleSize >= 10 {
			// Use list-level time
			scheduledTime = time.Date(
				baseDate.Year(), baseDate.Month(), baseDate.Day(),
				listDefault.OverallBestHour, 0, 0, 0, time.UTC,
			)
			confidence = listDefault.Confidence * 0.7
		} else if industryDefault != nil {
			// Use industry default
			scheduledTime = time.Date(
				baseDate.Year(), baseDate.Month(), baseDate.Day(),
				industryDefault.RecommendedTime.Hour(), 0, 0, 0, time.UTC,
			)
			confidence = industryDefault.Confidence * 0.5
		} else {
			// Fallback to 10 AM UTC
			scheduledTime = time.Date(
				baseDate.Year(), baseDate.Month(), baseDate.Day(),
				10, 0, 0, 0, time.UTC,
			)
			confidence = 0.3
		}

		// Ensure scheduled time is in the future
		if scheduledTime.Before(time.Now()) {
			scheduledTime = scheduledTime.Add(24 * time.Hour)
		}

		scheduledTimes[e.subscriberID.String()] = scheduledTime

		// Insert into scheduled times table
		s.db.ExecContext(ctx, `
			INSERT INTO mailing_campaign_scheduled_times (
				campaign_id, subscriber_id, scheduled_time, timezone, confidence
			) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (campaign_id, subscriber_id) DO UPDATE SET
				scheduled_time = EXCLUDED.scheduled_time,
				timezone = EXCLUDED.timezone,
				confidence = EXCLUDED.confidence
		`, campUUID, e.subscriberID, scheduledTime, e.timezone, confidence)
	}

	return scheduledTimes, nil
}

// GetTimezoneDistribution returns the distribution of subscriber timezones for a list
func (s *AISendTimeService) GetTimezoneDistribution(ctx context.Context, listID string) (map[string]int, error) {
	listUUID, err := uuid.Parse(listID)
	if err != nil {
		return nil, fmt.Errorf("invalid list ID: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(timezone, 'Unknown') as tz, COUNT(*) as count
		FROM mailing_subscribers
		WHERE list_id = $1 AND status = 'confirmed'
		GROUP BY timezone
		ORDER BY count DESC
	`, listUUID)
	if err != nil {
		return nil, fmt.Errorf("failed to query timezone distribution: %w", err)
	}
	defer rows.Close()

	distribution := make(map[string]int)
	for rows.Next() {
		var tz string
		var count int
		if err := rows.Scan(&tz, &count); err == nil {
			distribution[tz] = count
		}
	}

	return distribution, nil
}

// RecordSendTimeEvent records a send event for future analysis
func (s *AISendTimeService) RecordSendTimeEvent(ctx context.Context, subscriberID, campaignID string, sentAt time.Time, timezone string) error {
	subUUID, err := uuid.Parse(subscriberID)
	if err != nil {
		return fmt.Errorf("invalid subscriber ID: %w", err)
	}

	var campUUID *uuid.UUID
	if campaignID != "" {
		u, err := uuid.Parse(campaignID)
		if err == nil {
			campUUID = &u
		}
	}

	sentHour := sentAt.Hour()
	sentDay := int(sentAt.Weekday())

	// Calculate local hour if timezone provided
	var localHour *int
	if timezone != "" {
		loc, err := time.LoadLocation(timezone)
		if err == nil {
			localTime := sentAt.In(loc)
			h := localTime.Hour()
			localHour = &h
		}
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mailing_send_time_history (
			subscriber_id, campaign_id, sent_at, sent_hour, sent_day, sent_local_hour, timezone
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, subUUID, campUUID, sentAt, sentHour, sentDay, localHour, timezone)

	return err
}

// UpdateEngagementOutcome updates the engagement outcome for a previously recorded send
func (s *AISendTimeService) UpdateEngagementOutcome(ctx context.Context, subscriberID, campaignID string, opened, clicked bool, openDelay, clickDelay *int) error {
	subUUID, err := uuid.Parse(subscriberID)
	if err != nil {
		return fmt.Errorf("invalid subscriber ID: %w", err)
	}

	campUUID, err := uuid.Parse(campaignID)
	if err != nil {
		return fmt.Errorf("invalid campaign ID: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE mailing_send_time_history
		SET opened = $3, clicked = $4, open_delay_seconds = $5, click_delay_seconds = $6
		WHERE subscriber_id = $1 AND campaign_id = $2
	`, subUUID, campUUID, opened, clicked, openDelay, clickDelay)

	return err
}

// buildRecommendation creates a SendTimeRecommendation from raw data
func (s *AISendTimeService) buildRecommendation(hour int, day sql.NullInt64, timezone string, confidence float64, source string) *SendTimeRecommendation {
	now := time.Now()
	
	// Calculate next occurrence of this hour
	recommendedTime := s.calculateNextOccurrence(hour, int(day.Int64))
	
	// Calculate local time
	var localTime time.Time
	if timezone != "" && timezone != "UTC" {
		loc, err := time.LoadLocation(timezone)
		if err == nil {
			localTime = recommendedTime.In(loc)
		} else {
			localTime = recommendedTime
		}
	} else {
		localTime = recommendedTime
		timezone = "UTC"
	}

	// Build reasoning
	var reasoning string
	switch source {
	case "subscriber":
		reasoning = fmt.Sprintf("Based on %s's personal engagement history, optimal send time is %02d:00 %s",
			"subscriber", hour, timezone)
	case "audience":
		reasoning = fmt.Sprintf("Based on audience-level engagement patterns, optimal send time is %02d:00 %s",
			hour, timezone)
	case "industry_default":
		reasoning = fmt.Sprintf("Using industry best practices - recommended send time is %02d:00 UTC", hour)
	}

	_ = now // silence unused variable

	return &SendTimeRecommendation{
		RecommendedTime: recommendedTime,
		LocalTime:       localTime,
		Timezone:        timezone,
		Confidence:      confidence,
		Reasoning:       reasoning,
		Source:          source,
	}
}

// calculateNextOccurrence returns the next occurrence of a specific hour/day
func (s *AISendTimeService) calculateNextOccurrence(hour int, day int) time.Time {
	now := time.Now().UTC()
	
	// Start with today at the specified hour
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, time.UTC)
	
	// If we have a specific day preference, find the next occurrence
	if day >= 0 && day <= 6 {
		currentDay := int(now.Weekday())
		daysUntil := (day - currentDay + 7) % 7
		if daysUntil == 0 && next.Before(now) {
			daysUntil = 7
		}
		next = next.Add(time.Duration(daysUntil) * 24 * time.Hour)
	} else {
		// No day preference, just find next occurrence of the hour
		if next.Before(now) {
			next = next.Add(24 * time.Hour)
		}
	}
	
	return next
}

// getIndustryDefault returns the industry default send time
func (s *AISendTimeService) getIndustryDefault(ctx context.Context) (*SendTimeRecommendation, error) {
	var bestHour int
	var bestDay sql.NullInt64

	err := s.db.QueryRowContext(ctx, `
		SELECT best_hour, best_day
		FROM mailing_industry_default_times
		WHERE industry = 'general' AND category IS NULL
		LIMIT 1
	`).Scan(&bestHour, &bestDay)

	if err != nil {
		// Hardcoded fallback
		bestHour = 10
		bestDay = sql.NullInt64{Int64: 2, Valid: true} // Tuesday
	}

	return s.buildRecommendation(bestHour, bestDay, "UTC", 0.5, "industry_default"), nil
}

// DeriveTimezoneFromIP attempts to derive timezone from an IP address
func (s *AISendTimeService) DeriveTimezoneFromIP(ctx context.Context, ip string) (string, error) {
	// Check cache first
	var timezone sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT timezone FROM mailing_ip_geolocation_cache
		WHERE ip_address = $1 AND expires_at > NOW()
	`, ip).Scan(&timezone)

	if err == nil && timezone.Valid {
		return timezone.String, nil
	}

	// In a real implementation, you would call a geolocation API here
	// For now, return empty to indicate unknown
	return "", nil
}

// UpdateSubscriberTimezone updates a subscriber's timezone
func (s *AISendTimeService) UpdateSubscriberTimezone(ctx context.Context, subscriberID, timezone string) error {
	subUUID, err := uuid.Parse(subscriberID)
	if err != nil {
		return fmt.Errorf("invalid subscriber ID: %w", err)
	}

	// Validate timezone
	if timezone != "" {
		if _, err := time.LoadLocation(timezone); err != nil {
			return fmt.Errorf("invalid timezone: %w", err)
		}
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE mailing_subscribers SET timezone = $1, updated_at = NOW() WHERE id = $2
	`, timezone, subUUID)
	if err != nil {
		return fmt.Errorf("failed to update timezone: %w", err)
	}

	// Also update optimal times table
	s.db.ExecContext(ctx, `
		UPDATE mailing_subscriber_optimal_times SET timezone = $1, updated_at = NOW() WHERE subscriber_id = $2
	`, timezone, subUUID)

	return nil
}
