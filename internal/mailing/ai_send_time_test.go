package mailing

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewAISendTimeService(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	assert.NotNil(t, service)
	assert.NotNil(t, service.db)
}

func TestGetOptimalSendTime_InvalidSubscriberID(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	_, err = service.GetOptimalSendTime(context.Background(), "invalid-uuid")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid subscriber ID")
}

func TestGetOptimalSendTime_SubscriberSpecificTime(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	subscriberID := uuid.New().String()

	// Mock subscriber optimal time query - high confidence
	mock.ExpectQuery("SELECT optimal_hour, optimal_day, timezone, confidence, sample_size, last_calculated").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"optimal_hour", "optimal_day", "timezone", "confidence", "sample_size", "last_calculated"}).
			AddRow(14, 2, "America/New_York", 0.85, 10, time.Now()))

	recommendation, err := service.GetOptimalSendTime(context.Background(), subscriberID)
	require.NoError(t, err)
	assert.NotNil(t, recommendation)
	assert.Equal(t, "subscriber", recommendation.Source)
	assert.Equal(t, 0.85, recommendation.Confidence)
	assert.Equal(t, "America/New_York", recommendation.Timezone)
}

func TestGetOptimalSendTime_FallbackToAudience(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	subscriberID := uuid.New().String()
	listID := uuid.New()

	// Mock subscriber optimal time query - low confidence (will fallback)
	mock.ExpectQuery("SELECT optimal_hour, optimal_day, timezone, confidence, sample_size, last_calculated").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"optimal_hour", "optimal_day", "timezone", "confidence", "sample_size", "last_calculated"}).
			AddRow(10, 1, "UTC", 0.3, 2, time.Now()))

	// Mock list ID query
	mock.ExpectQuery("SELECT list_id FROM mailing_subscribers").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"list_id"}).AddRow(listID))

	// Mock audience optimal times cache query
	mock.ExpectQuery("SELECT best_hours, best_days, overall_best_hour, overall_best_day").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"best_hours", "best_days", "overall_best_hour", "overall_best_day", "total_sample_size", "confidence_score", "last_calculated"}).
			AddRow(`[{"hour":10,"open_rate":18.5}]`, `[{"day":2,"open_rate":17.5}]`, 10, 2, 100, 0.8, time.Now()))

	recommendation, err := service.GetOptimalSendTime(context.Background(), subscriberID)
	require.NoError(t, err)
	assert.NotNil(t, recommendation)
	assert.Equal(t, "audience", recommendation.Source)
}

func TestGetOptimalSendTime_FallbackToIndustryDefault(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	subscriberID := uuid.New().String()

	// Mock subscriber optimal time query - not found
	mock.ExpectQuery("SELECT optimal_hour, optimal_day, timezone, confidence, sample_size, last_calculated").
		WithArgs(sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)

	// Mock list ID query - not found
	mock.ExpectQuery("SELECT list_id FROM mailing_subscribers").
		WithArgs(sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)

	// Mock industry default query
	mock.ExpectQuery("SELECT best_hour, best_day").
		WillReturnRows(sqlmock.NewRows([]string{"best_hour", "best_day"}).
			AddRow(10, 2))

	recommendation, err := service.GetOptimalSendTime(context.Background(), subscriberID)
	require.NoError(t, err)
	assert.NotNil(t, recommendation)
	assert.Equal(t, "industry_default", recommendation.Source)
}

func TestGetBulkOptimalTimes_EmptyList(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	results, err := service.GetBulkOptimalTimes(context.Background(), []string{})
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestGetBulkOptimalTimes_ValidSubscribers(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	subscriber1 := uuid.New()
	subscriber2 := uuid.New()

	// Mock bulk query - use regex matcher to handle pq.Array
	mock.ExpectQuery("SELECT sot.subscriber_id, sot.optimal_hour, sot.optimal_day").
		WillReturnRows(sqlmock.NewRows([]string{"subscriber_id", "optimal_hour", "optimal_day", "timezone", "confidence", "sample_size"}).
			AddRow(subscriber1, 9, 1, "America/New_York", 0.8, 15).
			AddRow(subscriber2, 14, 3, "America/Los_Angeles", 0.75, 10))

	// Mock industry default for fallback
	mock.ExpectQuery("SELECT best_hour, best_day").
		WillReturnRows(sqlmock.NewRows([]string{"best_hour", "best_day"}).AddRow(10, 2))

	results, err := service.GetBulkOptimalTimes(context.Background(), []string{subscriber1.String(), subscriber2.String()})
	require.NoError(t, err)
	assert.Len(t, results, 2)
}

func TestCalculateAudienceOptimalTimes_CacheHit(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	listID := uuid.New().String()

	// Mock cache hit
	mock.ExpectQuery("SELECT best_hours, best_days, overall_best_hour, overall_best_day").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"best_hours", "best_days", "overall_best_hour", "overall_best_day", "total_sample_size", "confidence_score", "last_calculated"}).
			AddRow(`[{"hour":10,"open_rate":18.5,"click_rate":2.5,"sample_size":100}]`, 
			       `[{"day":2,"open_rate":17.5,"click_rate":2.3,"sample_size":100}]`, 
			       10, 2, 500, 0.85, time.Now()))

	result, err := service.CalculateAudienceOptimalTimes(context.Background(), listID)
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, listID, result.ListID)
	assert.Equal(t, 10, result.OverallBestHour)
	assert.Equal(t, 2, result.OverallBestDay)
	assert.Equal(t, 500, result.SampleSize)
	assert.InDelta(t, 0.85, result.Confidence, 0.01)
}

func TestCalculateAudienceOptimalTimes_FreshCalculation(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	listID := uuid.New().String()

	// Mock cache miss
	mock.ExpectQuery("SELECT best_hours, best_days, overall_best_hour, overall_best_day").
		WithArgs(sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)

	// Mock hourly distribution query
	mock.ExpectQuery("WITH weighted_events AS").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"sent_hour", "open_rate", "click_rate", "sample_size"}).
			AddRow(9, 15.2, 2.1, 50).
			AddRow(10, 18.5, 2.8, 75).
			AddRow(11, 16.8, 2.4, 60))

	// Mock daily distribution query
	mock.ExpectQuery("WITH weighted_events AS").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"sent_day", "open_rate", "click_rate", "sample_size"}).
			AddRow(1, 16.5, 2.2, 40).
			AddRow(2, 18.2, 2.6, 55).
			AddRow(3, 15.8, 2.0, 35))

	// Mock org ID query
	mock.ExpectQuery("SELECT organization_id FROM mailing_lists").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id"}).AddRow(uuid.New()))

	// Mock cache insert
	mock.ExpectExec("INSERT INTO mailing_audience_optimal_times").
		WillReturnResult(sqlmock.NewResult(1, 1))

	result, err := service.CalculateAudienceOptimalTimes(context.Background(), listID)
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, 10, result.OverallBestHour) // Hour with highest open rate
	assert.Equal(t, 2, result.OverallBestDay)   // Day with highest open rate
}

func TestRecalculateSubscriberTime_NoData(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	subscriberID := uuid.New().String()

	// Mock empty history
	mock.ExpectQuery("SELECT sent_hour, sent_day, opened, clicked, POWER").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"sent_hour", "sent_day", "opened", "clicked", "weight", "sent_at"}))

	_, err = service.RecalculateSubscriberTime(context.Background(), subscriberID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no engagement data")
}

func TestRecalculateSubscriberTime_WithData(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	subscriberID := uuid.New().String()

	// Mock history with engagement data
	mock.ExpectQuery("SELECT sent_hour, sent_day, opened, clicked, POWER").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"sent_hour", "sent_day", "opened", "clicked", "weight", "sent_at"}).
			AddRow(10, 2, true, false, 0.9, time.Now().Add(-7*24*time.Hour)).
			AddRow(10, 3, true, true, 0.85, time.Now().Add(-14*24*time.Hour)).
			AddRow(14, 2, false, false, 0.8, time.Now().Add(-21*24*time.Hour)).
			AddRow(10, 1, true, false, 0.75, time.Now().Add(-28*24*time.Hour)))

	// Mock timezone query
	mock.ExpectQuery("SELECT timezone FROM mailing_subscribers").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"timezone"}).AddRow("America/New_York"))

	// Mock upsert
	mock.ExpectExec("INSERT INTO mailing_subscriber_optimal_times").
		WillReturnResult(sqlmock.NewResult(1, 1))

	// Mock subscriber update
	mock.ExpectExec("UPDATE mailing_subscribers").
		WillReturnResult(sqlmock.NewResult(0, 1))

	result, err := service.RecalculateSubscriberTime(context.Background(), subscriberID)
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, subscriberID, result.SubscriberID)
	assert.Equal(t, 10, result.OptimalHour) // Most opens at hour 10
	// Confidence with 3 opens: (0.3 + 3*0.06) * 0.7 (penalty for < 5 opens) = 0.336
	assert.Greater(t, result.Confidence, 0.3)
}

func TestGetTimezoneDistribution(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	listID := uuid.New().String()

	// Mock timezone distribution query
	mock.ExpectQuery("SELECT COALESCE\\(timezone, 'Unknown'\\) as tz, COUNT").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"tz", "count"}).
			AddRow("America/New_York", 500).
			AddRow("America/Los_Angeles", 300).
			AddRow("America/Chicago", 200).
			AddRow("Unknown", 150))

	distribution, err := service.GetTimezoneDistribution(context.Background(), listID)
	require.NoError(t, err)
	assert.Len(t, distribution, 4)
	assert.Equal(t, 500, distribution["America/New_York"])
	assert.Equal(t, 300, distribution["America/Los_Angeles"])
}

func TestScheduleCampaignOptimally_NotEnabled(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	campaignID := uuid.New().String()

	// Mock campaign query - AI optimization disabled
	mock.ExpectQuery("SELECT list_id, segment_id, send_at, ai_send_time_optimization").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"list_id", "segment_id", "send_at", "ai_send_time_optimization"}).
			AddRow(uuid.New(), nil, time.Now().Add(24*time.Hour), false))

	_, err = service.ScheduleCampaignOptimally(context.Background(), campaignID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "AI send time optimization is not enabled")
}

func TestUpdateSubscriberTimezone_Valid(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	subscriberID := uuid.New().String()

	// Mock subscriber update
	mock.ExpectExec("UPDATE mailing_subscribers SET timezone").
		WithArgs("America/New_York", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Mock optimal times update
	mock.ExpectExec("UPDATE mailing_subscriber_optimal_times SET timezone").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = service.UpdateSubscriberTimezone(context.Background(), subscriberID, "America/New_York")
	require.NoError(t, err)
}

func TestUpdateSubscriberTimezone_InvalidTimezone(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	subscriberID := uuid.New().String()

	err = service.UpdateSubscriberTimezone(context.Background(), subscriberID, "Invalid/Timezone")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid timezone")
}

func TestRecordSendTimeEvent(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	subscriberID := uuid.New().String()
	campaignID := uuid.New().String()
	sentAt := time.Now()

	// Mock insert
	mock.ExpectExec("INSERT INTO mailing_send_time_history").
		WillReturnResult(sqlmock.NewResult(1, 1))

	err = service.RecordSendTimeEvent(context.Background(), subscriberID, campaignID, sentAt, "America/New_York")
	require.NoError(t, err)
}

func TestUpdateEngagementOutcome(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	subscriberID := uuid.New().String()
	campaignID := uuid.New().String()
	openDelay := 300 // 5 minutes

	// Mock update
	mock.ExpectExec("UPDATE mailing_send_time_history").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = service.UpdateEngagementOutcome(context.Background(), subscriberID, campaignID, true, false, &openDelay, nil)
	require.NoError(t, err)
}

func TestCalculateNextOccurrence(t *testing.T) {
	service := &AISendTimeService{}

	testCases := []struct {
		name     string
		hour     int
		day      int
		validate func(t *testing.T, result time.Time)
	}{
		{
			name: "next Tuesday at 10am",
			hour: 10,
			day:  2,
			validate: func(t *testing.T, result time.Time) {
				assert.Equal(t, 10, result.Hour())
				assert.Equal(t, time.Tuesday, result.Weekday())
				assert.True(t, result.After(time.Now()))
			},
		},
		{
			name: "any day at 14:00",
			hour: 14,
			day:  -1, // No day preference
			validate: func(t *testing.T, result time.Time) {
				assert.Equal(t, 14, result.Hour())
				assert.True(t, result.After(time.Now()))
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := service.calculateNextOccurrence(tc.hour, tc.day)
			tc.validate(t, result)
		})
	}
}

func TestBuildRecommendation(t *testing.T) {
	service := &AISendTimeService{}

	testCases := []struct {
		name       string
		hour       int
		day        sql.NullInt64
		timezone   string
		confidence float64
		source     string
	}{
		{
			name:       "subscriber source",
			hour:       10,
			day:        sql.NullInt64{Int64: 2, Valid: true},
			timezone:   "America/New_York",
			confidence: 0.85,
			source:     "subscriber",
		},
		{
			name:       "audience source",
			hour:       14,
			day:        sql.NullInt64{Int64: 3, Valid: true},
			timezone:   "UTC",
			confidence: 0.7,
			source:     "audience",
		},
		{
			name:       "industry default",
			hour:       10,
			day:        sql.NullInt64{Valid: false},
			timezone:   "",
			confidence: 0.5,
			source:     "industry_default",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := service.buildRecommendation(tc.hour, tc.day, tc.timezone, tc.confidence, tc.source)
			assert.NotNil(t, result)
			assert.Equal(t, tc.confidence, result.Confidence)
			assert.Equal(t, tc.source, result.Source)
			assert.NotEmpty(t, result.Reasoning)
			assert.True(t, result.RecommendedTime.After(time.Now()))
		})
	}
}

// Integration test helper - tests full flow with real-ish mock data
func TestIntegration_FullOptimizationFlow(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	
	// Simulate: Record events -> Calculate -> Get recommendation
	subscriberID := uuid.New()
	campaignID := uuid.New()
	
	// 1. Record multiple send events
	for i := 0; i < 5; i++ {
		mock.ExpectExec("INSERT INTO mailing_send_time_history").
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
	
	for i := 0; i < 5; i++ {
		err := service.RecordSendTimeEvent(context.Background(), subscriberID.String(), 
			campaignID.String(), time.Now().Add(-time.Duration(i*7*24)*time.Hour), "America/New_York")
		require.NoError(t, err)
	}
	
	// 2. Simulate engagement updates
	for i := 0; i < 3; i++ {
		mock.ExpectExec("UPDATE mailing_send_time_history").
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	
	for i := 0; i < 3; i++ {
		openDelay := 180
		err := service.UpdateEngagementOutcome(context.Background(), subscriberID.String(), 
			campaignID.String(), true, i%2 == 0, &openDelay, nil)
		require.NoError(t, err)
	}
}

// Benchmark tests
func BenchmarkGetOptimalSendTime(b *testing.B) {
	db, mock, err := sqlmock.New()
	require.NoError(b, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	subscriberID := uuid.New().String()

	// Setup mock for each iteration
	for i := 0; i < b.N; i++ {
		mock.ExpectQuery("SELECT optimal_hour, optimal_day, timezone, confidence, sample_size, last_calculated").
			WillReturnRows(sqlmock.NewRows([]string{"optimal_hour", "optimal_day", "timezone", "confidence", "sample_size", "last_calculated"}).
				AddRow(10, 2, "UTC", 0.8, 15, time.Now()))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = service.GetOptimalSendTime(context.Background(), subscriberID)
	}
}

func BenchmarkCalculateAudienceOptimalTimes(b *testing.B) {
	db, mock, err := sqlmock.New()
	require.NoError(b, err)
	defer db.Close()

	service := NewAISendTimeService(db)
	listID := uuid.New().String()

	// Setup mock for each iteration (cache hit scenario)
	for i := 0; i < b.N; i++ {
		mock.ExpectQuery("SELECT best_hours, best_days, overall_best_hour, overall_best_day").
			WillReturnRows(sqlmock.NewRows([]string{"best_hours", "best_days", "overall_best_hour", "overall_best_day", "total_sample_size", "confidence_score", "last_calculated"}).
				AddRow(`[]`, `[]`, 10, 2, 500, 0.85, time.Now()))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = service.CalculateAudienceOptimalTimes(context.Background(), listID)
	}
}
